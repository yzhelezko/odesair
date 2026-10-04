package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fakeJWT(claims string) string {
	return "h." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".s"
}

func TestAccountIDClaimPaths(t *testing.T) {
	cases := map[string]string{
		`{"chatgpt_account_id":"top"}`:                                "top",
		`{"https://api.openai.com/auth":{"chatgpt_account_id":"ns"}}`: "ns",
		`{"organizations":[{"id":"org-1"},{"id":"org-2"}]}`:           "org-1",
		`{"sub":"user"}`: "",
	}
	for claims, want := range cases {
		if got := accountID(fakeJWT(claims)); got != want {
			t.Errorf("%s: account id = %q, want %q", claims, got, want)
		}
	}
	if got := accountID("garbage", fakeJWT(`{}`), fakeJWT(`{"chatgpt_account_id":"second"}`)); got != "second" {
		t.Fatalf("account id = %q: later tokens must be tried", got)
	}
}

func TestAuthorizeURL(t *testing.T) {
	u, err := url.Parse(authorizeURL("https://issuer.test", "chal", "st"))
	if err != nil || u.Host != "issuer.test" || u.Path != "/oauth/authorize" {
		t.Fatalf("url = %v, err = %v", u, err)
	}
	want := map[string]string{
		"response_type": "code", "client_id": openaiClientID, "redirect_uri": openaiRedirectURI,
		"scope": openaiScope, "code_challenge": "chal", "code_challenge_method": "S256", "state": "st",
	}
	for key, value := range want {
		if got := u.Query().Get(key); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
}

func TestCallbackHandler(t *testing.T) {
	cases := []struct {
		query    string
		wantCode string
		wantErr  bool
	}{
		{"code=abc&state=st", "abc", false},
		{"code=abc&state=other", "", true},
		{"state=st", "", true},
		{"error=access_denied&error_description=nope&state=st", "", true},
	}
	for _, tc := range cases {
		done := make(chan callbackResult, 1)
		rec := httptest.NewRecorder()
		callbackHandler("st", done).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/callback?"+tc.query, nil))
		res := <-done
		if res.code != tc.wantCode || (res.err != nil) != tc.wantErr || (rec.Code != http.StatusOK) != tc.wantErr {
			t.Errorf("%s: code = %q, err = %v, status = %d", tc.query, res.code, res.err, rec.Code)
		}
	}
}

type fakeIssuer struct {
	*httptest.Server
	calls  atomic.Int32
	status int
	form   url.Values
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	f := &fakeIssuer{status: http.StatusOK}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := f.calls.Add(1)
		r.ParseForm()
		f.form = r.PostForm
		if r.URL.Path != "/oauth/token" || f.status != http.StatusOK {
			http.Error(w, `{"error":"invalid_grant"}`, max(f.status, http.StatusBadRequest))
			return
		}
		json.NewEncoder(w).Encode(tokenResponse{
			AccessToken:  fmt.Sprint("access-", n),
			RefreshToken: fmt.Sprint("refresh-", n),
			IDToken:      fakeJWT(`{"chatgpt_account_id":"acct"}`),
			ExpiresIn:    7200,
		})
	}))
	t.Cleanup(f.Close)
	return f
}

func testTokenSource(t *testing.T, issuer *fakeIssuer, tok oauthToken) *tokenSource {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := saveToken(path, tok); err != nil {
		t.Fatal(err)
	}
	return &tokenSource{path: path, issuer: issuer.URL, http: issuer.Client(), now: time.Now}
}

func TestTokenSourceRefreshRotatesAndPersists(t *testing.T) {
	issuer := newFakeIssuer(t)
	src := testTokenSource(t, issuer, oauthToken{Access: "stale", Refresh: "refresh-0", Expires: time.Now().Add(time.Minute)})

	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "access-1" || tok.Refresh != "refresh-1" || tok.AccountID != "acct" || time.Until(tok.Expires) < time.Hour {
		t.Fatalf("token = %+v", tok)
	}
	if issuer.form.Get("grant_type") != "refresh_token" || issuer.form.Get("refresh_token") != "refresh-0" ||
		issuer.form.Get("client_id") != openaiClientID {
		t.Fatalf("refresh form = %v", issuer.form)
	}

	raw, _ := os.ReadFile(src.path)
	var saved oauthToken
	if json.Unmarshal(raw, &saved); saved.Refresh != "refresh-1" || saved.Access != "access-1" {
		t.Fatalf("rotated token must be written back: %s", raw)
	}
	if st, _ := os.Stat(src.path); st.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v", st.Mode().Perm())
	}

	if _, err := src.Token(context.Background()); err != nil || issuer.calls.Load() != 1 {
		t.Fatalf("fresh token must be reused: err = %v, calls = %d", err, issuer.calls.Load())
	}
	src.Expire()
	if tok, _ := src.Token(context.Background()); tok.Access != "access-2" || issuer.form.Get("refresh_token") != "refresh-1" {
		t.Fatalf("Expire must force a refresh with the rotated token: %+v", tok)
	}
}

func TestTokenSourceRefreshFailureCoolsDownUntilNewLogin(t *testing.T) {
	issuer := newFakeIssuer(t)
	issuer.status = http.StatusUnauthorized
	src := testTokenSource(t, issuer, oauthToken{Access: "stale", Refresh: "dead", Expires: time.Now().Add(-time.Hour)})

	for range 3 {
		if _, err := src.Token(context.Background()); err == nil {
			t.Fatal("refresh failure must surface")
		}
	}
	if issuer.calls.Load() != 1 {
		t.Fatalf("token endpoint calls = %d: failures must not be retried inside the cooldown", issuer.calls.Load())
	}

	fresh := oauthToken{Access: "new-login", Refresh: "r", Expires: time.Now().Add(time.Hour)}
	if err := saveToken(src.path, fresh); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(src.path, time.Now(), time.Now().Add(time.Minute))
	if tok, err := src.Token(context.Background()); err != nil || tok.Access != "new-login" {
		t.Fatalf("a replaced token file must be picked up: %+v, %v", tok, err)
	}
}

func TestTokenSourceRefreshesEarlyAndWarnsWhileItFails(t *testing.T) {
	issuer := newFakeIssuer(t)
	clock := time.Now()
	src := testTokenSource(t, issuer, oauthToken{Access: "old", Refresh: "r0", Expires: clock.Add(refreshEarly + time.Hour)})
	src.now = func() time.Time { return clock }
	// step moves the clock and returns the token in use, the refresh attempts so far and the warning.
	step := func(d time.Duration) (string, int32, string) {
		clock = clock.Add(d)
		tok, err := src.Token(context.Background())
		if err != nil {
			t.Fatalf("a token that is still valid must be served: %v", err)
		}
		return tok.Access, issuer.calls.Load(), src.Warning()
	}

	if access, calls, warn := step(0); access != "old" || calls != 0 || warn != "" {
		t.Fatalf("outside the early window: access = %s, calls = %d, warn = %q", access, calls, warn)
	}

	issuer.status = http.StatusInternalServerError
	if access, calls, warn := step(2 * time.Hour); access != "old" || calls != 1 || warn != "" {
		t.Fatalf("71h left: access = %s, calls = %d, warn = %q", access, calls, warn)
	}
	if _, calls, _ := step(time.Minute); calls != 1 {
		t.Fatalf("calls = %d: a failed early refresh must wait for the cooldown", calls)
	}
	if access, calls, warn := step(31 * time.Hour); access != "old" || calls != 2 || !strings.Contains(warn, "2 дня") {
		t.Fatalf("under 48h left: access = %s, calls = %d, warn = %q", access, calls, warn)
	}
	if _, calls, warn := step(20 * time.Hour); calls != 3 || !strings.Contains(warn, "1 день") {
		t.Fatalf("under 24h left: calls = %d, warn = %q", calls, warn)
	}

	issuer.status = http.StatusOK
	if access, _, warn := step(2 * time.Hour); access != "access-4" || warn != "" {
		t.Fatalf("after a successful refresh: access = %s, warn = %q", access, warn)
	}
}

func TestTokenSourceRejectsUnusableFile(t *testing.T) {
	issuer := newFakeIssuer(t)
	src := &tokenSource{path: filepath.Join(t.TempDir(), "missing.json"), issuer: issuer.URL, http: issuer.Client(), now: time.Now}
	if _, err := src.Token(context.Background()); err == nil {
		t.Fatal("missing file must fail")
	}
	os.WriteFile(src.path, []byte(`{"access":"a"}`), 0o600)
	if _, err := src.Token(context.Background()); err == nil {
		t.Fatal("file without a refresh token must fail")
	}
}
