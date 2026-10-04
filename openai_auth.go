package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

const (
	openaiIssuer      = "https://auth.openai.com"
	openaiClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	openaiCallback    = "127.0.0.1:1455"
	openaiRedirectURI = "http://localhost:1455/auth/callback"
	openaiScope       = "openid profile email offline_access"
	clientName        = "odesair"

	loginTimeout    = 5 * time.Minute
	authTimeout     = 30 * time.Second
	defaultTokenTTL = time.Hour

	// Refreshing starts refreshEarly before expiry, so a refresh that keeps
	// failing is noticed while the old token still works for days.
	refreshEarly    = 72 * time.Hour
	refreshSkew     = 5 * time.Minute
	refreshCooldown = time.Hour
	urgentCooldown  = time.Minute
	warnWindow      = 48 * time.Hour
	warnPrefix      = "⚠️"
)

type oauthToken struct {
	Access    string    `json:"access"`
	Refresh   string    `json:"refresh"`
	Expires   time.Time `json:"expires"`
	AccountID string    `json:"account_id,omitempty"`
}

type tokenResponse struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// token keeps the previous refresh token and account id when the response omits them.
func (r tokenResponse) token(prev oauthToken, now time.Time) oauthToken {
	ttl := defaultTokenTTL
	if r.ExpiresIn > 0 {
		ttl = time.Duration(r.ExpiresIn) * time.Second
	}
	return oauthToken{
		Access:    r.AccessToken,
		Refresh:   cmp.Or(r.RefreshToken, prev.Refresh),
		Expires:   now.Add(ttl),
		AccountID: cmp.Or(accountID(r.IDToken, r.AccessToken), prev.AccountID),
	}
}

// accountID reads the ChatGPT account id from the first JWT that carries one.
func accountID(jwts ...string) string {
	for _, jwt := range jwts {
		parts := strings.Split(jwt, ".")
		if len(parts) != 3 {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
		if err != nil {
			continue
		}
		var claims struct {
			AccountID string `json:"chatgpt_account_id"`
			Auth      struct {
				AccountID string `json:"chatgpt_account_id"`
			} `json:"https://api.openai.com/auth"`
			Organizations []struct {
				ID string `json:"id"`
			} `json:"organizations"`
		}
		if json.Unmarshal(raw, &claims) != nil {
			continue
		}
		if id := cmp.Or(claims.AccountID, claims.Auth.AccountID); id != "" {
			return id
		}
		if len(claims.Organizations) > 0 {
			return claims.Organizations[0].ID
		}
	}
	return ""
}

func requestToken(ctx context.Context, client *http.Client, issuer string, form url.Values) (tokenResponse, error) {
	var out tokenResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, issuer+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", clientName)
	resp, err := client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return out, err
	}
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("status %d: %s", resp.StatusCode, snippet(raw))
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	if out.AccessToken == "" {
		return out, errors.New("no access token in response")
	}
	return out, nil
}

func saveToken(path string, tok oauthToken) error {
	raw, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func randomString() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func authorizeURL(issuer, challenge, state string) string {
	return issuer + "/oauth/authorize?" + url.Values{
		"response_type":              {"code"},
		"client_id":                  {openaiClientID},
		"redirect_uri":               {openaiRedirectURI},
		"scope":                      {openaiScope},
		"code_challenge":             {challenge},
		"code_challenge_method":      {"S256"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
		"state":                      {state},
		"originator":                 {clientName},
	}.Encode()
}

type callbackResult struct {
	code string
	err  error
}

func callbackHandler(state string, done chan<- callbackResult) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var res callbackResult
		switch {
		case q.Get("error") != "":
			res.err = fmt.Errorf("authorization failed: %s", cmp.Or(q.Get("error_description"), q.Get("error")))
		case q.Get("state") != state:
			res.err = errors.New("state mismatch")
		case q.Get("code") == "":
			res.err = errors.New("missing authorization code")
		default:
			res.code = q.Get("code")
		}
		if res.err != nil {
			http.Error(w, res.err.Error(), http.StatusBadRequest)
		} else {
			io.WriteString(w, "Signed in. You can close this tab.")
		}
		select {
		case done <- res:
		default:
		}
	})
	return mux
}

// login runs the browser sign-in with PKCE and stores the token at path.
func login(ctx context.Context, path string, out io.Writer) error {
	verifier, state := randomString(), randomString()
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	ln, err := net.Listen("tcp", openaiCallback)
	if err != nil {
		return fmt.Errorf("callback listener: %w", err)
	}
	done := make(chan callbackResult, 1)
	srv := &http.Server{Handler: callbackHandler(state, done)}
	go srv.Serve(ln)
	defer srv.Close()

	fmt.Fprintf(out, "Open this URL in a browser and sign in:\n\n%s\n\nWaiting for the callback on %s ...\n",
		authorizeURL(openaiIssuer, challenge, state), openaiRedirectURI)

	var res callbackResult
	select {
	case res = <-done:
	case <-time.After(loginTimeout):
		return errors.New("timed out waiting for the sign-in")
	case <-ctx.Done():
		return ctx.Err()
	}
	if res.err != nil {
		return res.err
	}

	resp, err := requestToken(ctx, &http.Client{Timeout: authTimeout}, openaiIssuer, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {res.code},
		"redirect_uri":  {openaiRedirectURI},
		"client_id":     {openaiClientID},
		"code_verifier": {verifier},
	})
	if err != nil {
		return fmt.Errorf("token exchange: %w", err)
	}
	tok := resp.token(oauthToken{}, time.Now())
	if err := saveToken(path, tok); err != nil {
		return err
	}
	fmt.Fprintf(out, "Saved %s (access token valid until %s).\n", path, tok.Expires.Format(time.RFC3339))
	return nil
}

// tokenSource serves the access token from a file and keeps it fresh. The
// refresh token rotates, so every refresh is written back. Used from the agent
// goroutine only.
type tokenSource struct {
	path   string
	issuer string
	http   *http.Client
	now    func() time.Time
	save   func(ctx context.Context, tok oauthToken) error

	mod     time.Time
	tok     oauthToken
	triedAt time.Time
	failure error
	// warning is read from the siren goroutine too.
	warning atomic.Pointer[string]
}

// newTokenSource writes refreshed tokens to the file, or to the named Secret when the file is mounted from one.
func newTokenSource(path, secret string) *tokenSource {
	s := &tokenSource{path: path, issuer: openaiIssuer, http: &http.Client{Timeout: authTimeout}, now: time.Now}
	s.save = func(_ context.Context, tok oauthToken) error { return saveToken(path, tok) }
	if secret != "" {
		store, err := newSecretStore(secret, filepath.Base(path))
		if err != nil {
			s.save = func(context.Context, oauthToken) error { return fmt.Errorf("secret %s: %w", secret, err) }
		} else {
			s.save = store.save
		}
	}
	return s
}

// Check proves at startup that a refreshed token can be stored, by writing the current one back.
func (s *tokenSource) Check(ctx context.Context) error {
	if err := s.reload(); err != nil {
		return err
	}
	return s.store(ctx)
}

func (s *tokenSource) store(ctx context.Context) error {
	if err := s.save(ctx, s.tok); err != nil {
		return err
	}
	if st, err := os.Stat(s.path); err == nil {
		s.mod = st.ModTime()
	}
	return nil
}

func (s *tokenSource) Token(ctx context.Context) (oauthToken, error) {
	if err := s.reload(); err != nil {
		return oauthToken{}, fmt.Errorf("openai auth: %w", err)
	}
	now := s.now()
	defer func() {
		warning := s.warn(s.tok.Expires.Sub(now))
		s.warning.Store(&warning)
	}()
	remaining := s.tok.Expires.Sub(now)
	if remaining > refreshEarly {
		return s.tok, nil
	}
	usable := remaining > refreshSkew
	wait := urgentCooldown
	if usable {
		wait = refreshCooldown
	}
	if now.Sub(s.triedAt) >= wait {
		s.triedAt = now
		s.refresh(ctx, now)
	}
	if usable || s.failure == nil {
		return s.tok, nil
	}
	return oauthToken{}, s.failure
}

func (s *tokenSource) refresh(ctx context.Context, now time.Time) {
	resp, err := requestToken(ctx, s.http, s.issuer, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {s.tok.Refresh},
		"client_id":     {openaiClientID},
	})
	if err != nil {
		s.failure = fmt.Errorf("openai auth: refresh failed, sign in again with the login command: %w", err)
		slog.Error("openai token refresh failed", "expires", s.tok.Expires.Format(time.RFC3339), "err", err)
		return
	}
	s.tok, s.failure = resp.token(s.tok, now), nil
	if err := s.store(ctx); err != nil {
		slog.Error("refreshed openai token not saved, a restart will need a new login", "err", err)
	}
	slog.Info("openai token refreshed", "expires", s.tok.Expires.Format(time.RFC3339))
}

// Warning is set while refreshing fails and the token is about to run out. It
// is as fresh as the last Token call.
func (s *tokenSource) Warning() string {
	if w := s.warning.Load(); w != nil {
		return *w
	}
	return ""
}

func (s *tokenSource) warn(remaining time.Duration) string {
	switch {
	case s.failure == nil || remaining > warnWindow:
		return ""
	case remaining > warnWindow/2:
		return warnPrefix + " Токен OpenAI истекает через 2 дня. Нужен повторный вход."
	}
	return warnPrefix + " Токен OpenAI истекает через 1 день. Нужен повторный вход."
}

// reload picks up a token file replaced by a new login.
func (s *tokenSource) reload() error {
	st, err := os.Stat(s.path)
	if err != nil {
		return err
	}
	if st.ModTime().Equal(s.mod) {
		return nil
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var tok oauthToken
	if err := json.Unmarshal(raw, &tok); err != nil {
		return fmt.Errorf("%s: %w", s.path, err)
	}
	if tok.Refresh == "" {
		return fmt.Errorf("%s has no refresh token", s.path)
	}
	s.tok, s.mod, s.failure, s.triedAt = tok, st.ModTime(), nil, time.Time{}
	return nil
}

// Expire forces a refresh on the next call, after the API rejected the token.
func (s *tokenSource) Expire() {
	s.tok.Expires, s.triedAt = time.Time{}, time.Time{}
}
