package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const (
	k8sAPI        = "https://kubernetes.default.svc"
	k8sAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"
)

// secretStore writes the token into the Kubernetes Secret that the token file
// is mounted from: the mount itself is read-only.
type secretStore struct {
	api  string
	dir  string
	name string
	key  string
	http *http.Client
}

func newSecretStore(name, key string) (*secretStore, error) {
	ca, err := os.ReadFile(k8sAccountDir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("no certificate in the service account CA file")
	}
	client := &http.Client{
		Timeout:   authTimeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
	}
	return &secretStore{api: k8sAPI, dir: k8sAccountDir, name: name, key: key, http: client}, nil
}

func (s *secretStore) save(ctx context.Context, tok oauthToken) error {
	// The account token rotates, so it is read on every call.
	bearer, err := os.ReadFile(s.dir + "/token")
	if err != nil {
		return err
	}
	namespace, err := os.ReadFile(s.dir + "/namespace")
	if err != nil {
		return err
	}
	raw, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	// encoding/json writes []byte as base64, the encoding Secret data uses.
	patch, err := json.Marshal(map[string]map[string][]byte{"data": {s.key: raw}})
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/api/v1/namespaces/%s/secrets/%s", s.api, strings.TrimSpace(string(namespace)), s.name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(patch))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/merge-patch+json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(bearer)))
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("patch secret %s: status %d: %s", s.name, resp.StatusCode, snippet(body))
	}
	return nil
}
