package mailauth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/justin-napolitano/logistics-runtime/internal/mailsync"
	"golang.org/x/oauth2"
)

// Infisical uses the documented v4 shared-secret API. Token is a narrowly scoped
// machine access token supplied by the deployment, never stored in PostgreSQL.
type Infisical struct {
	ProjectID, Environment, Path, Token      string
	Client                                   *http.Client
	UniversalClientID, UniversalClientSecret string
	mu                                       sync.Mutex
	machineToken                             string
	machineExpiry                            time.Time
}

func (s *Infisical) endpoint(ref string) (string, error) {
	if s.ProjectID == "" || s.Environment == "" || (s.Token == "" && (s.UniversalClientID == "" || s.UniversalClientSecret == "")) || len(ref) == 0 || len(ref) > 128 {
		return "", mailsync.ErrCredential
	}
	for _, r := range ref {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return "", mailsync.ErrInvalid
		}
	}
	return "https://us.infisical.com/api/v4/secrets/" + ref, nil
}
func (s *Infisical) request(ctx context.Context, method, endpoint string, body []byte) ([]byte, int, error) {
	req, e := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if e != nil {
		return nil, 0, mailsync.ErrCredential
	}
	token, e := s.access(ctx)
	if e != nil {
		return nil, 0, e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, e := SafeClient(s.Client).Do(req)
	if e != nil {
		return nil, 0, mailsync.ErrCredential
	}
	defer response.Body.Close()
	b, e := io.ReadAll(io.LimitReader(response.Body, 64<<10+1))
	if e != nil || len(b) > 64<<10 {
		return nil, response.StatusCode, mailsync.ErrCredential
	}
	return b, response.StatusCode, nil
}
func (s *Infisical) Load(ctx context.Context, ref string) (*oauth2.Token, error) {
	endpoint, e := s.endpoint(ref)
	if e != nil {
		return nil, e
	}
	path := s.Path
	if path == "" {
		path = "/"
	}
	query := url.Values{"projectId": {s.ProjectID}, "environment": {s.Environment}, "secretPath": {path}, "type": {"shared"}, "expandSecretReferences": {"false"}, "includeImports": {"false"}}
	b, status, e := s.request(ctx, "GET", endpoint+"?"+query.Encode(), nil)
	if e != nil {
		return nil, e
	}
	if status == 404 {
		return nil, mailsync.ErrReconnect
	}
	if status != 200 {
		return nil, mailsync.ErrCredential
	}
	var response struct {
		Secret struct {
			Value string `json:"secretValue"`
		} `json:"secret"`
	}
	if json.Unmarshal(b, &response) != nil {
		return nil, mailsync.ErrCredential
	}
	var t oauth2.Token
	if json.Unmarshal([]byte(response.Secret.Value), &t) != nil || t.RefreshToken == "" || strings.ContainsAny(t.AccessToken, "\r\n") {
		return nil, mailsync.ErrReconnect
	}
	return &t, nil
}
func (s *Infisical) Save(ctx context.Context, ref string, t *oauth2.Token) error {
	endpoint, e := s.endpoint(ref)
	if e != nil {
		return e
	}
	if t == nil || t.RefreshToken == "" {
		return mailsync.ErrReconnect
	}
	value, e := json.Marshal(t)
	if e != nil {
		return mailsync.ErrCredential
	}
	path := s.Path
	if path == "" {
		path = "/"
	}
	body, _ := json.Marshal(map[string]any{"projectId": s.ProjectID, "environment": s.Environment, "secretPath": path, "type": "shared", "secretValue": string(value), "skipMultilineEncoding": true})
	_, status, e := s.request(ctx, "PATCH", endpoint, body)
	if e != nil {
		return e
	}
	if status == 404 {
		_, status, e = s.request(ctx, "POST", endpoint, body)
	}
	if e != nil || status < 200 || status >= 300 {
		return mailsync.ErrCredential
	}
	return nil
}

// access renews the machine identity credential for long-running workers. Direct
// INFISICAL_TOKEN mode is supported for short-lived jobs and expires as configured.
func (s *Infisical) access(ctx context.Context) (string, error) {
	if s.Token != "" {
		return s.Token, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.machineToken != "" && time.Now().Add(time.Minute).Before(s.machineExpiry) {
		return s.machineToken, nil
	}
	body := url.Values{"clientId": {s.UniversalClientID}, "clientSecret": {s.UniversalClientSecret}}.Encode()
	req, e := http.NewRequestWithContext(ctx, "POST", "https://us.infisical.com/api/v1/auth/universal-auth/login", strings.NewReader(body))
	if e != nil {
		return "", mailsync.ErrCredential
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, e := SafeClient(s.Client).Do(req)
	if e != nil {
		return "", mailsync.ErrCredential
	}
	defer response.Body.Close()
	data, e := io.ReadAll(io.LimitReader(response.Body, 64<<10+1))
	if e != nil || len(data) > 64<<10 || response.StatusCode != 200 {
		return "", mailsync.ErrCredential
	}
	var value struct {
		AccessToken string `json:"accessToken"`
		ExpiresIn   int64  `json:"expiresIn"`
	}
	if json.Unmarshal(data, &value) != nil || value.AccessToken == "" || value.ExpiresIn < 60 || value.ExpiresIn > 366*24*3600 {
		return "", mailsync.ErrCredential
	}
	s.machineToken = value.AccessToken
	s.machineExpiry = time.Now().Add(time.Duration(value.ExpiresIn) * time.Second)
	return value.AccessToken, nil
}
