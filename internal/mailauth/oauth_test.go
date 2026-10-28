package mailauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/justin-napolitano/logistics-runtime/internal/mailsync"
	"golang.org/x/oauth2"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func reply(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

type memoryTokens struct {
	token *oauth2.Token
	saves int
	fail  bool
}

func (s *memoryTokens) Load(context.Context, string) (*oauth2.Token, error) { return s.token, nil }
func (s *memoryTokens) Save(_ context.Context, _ string, t *oauth2.Token) error {
	if s.fail {
		return errors.New("synthetic store unavailable")
	}
	copy := *t
	s.token = &copy
	s.saves++
	return nil
}
func TestRefreshRotatesAndPersistsBeforeUse(t *testing.T) {
	config, _ := Config("outlook", "synthetic-client", "", "common")
	store := &memoryTokens{token: &oauth2.Token{AccessToken: "expired", RefreshToken: "synthetic-old-refresh", Expiry: time.Unix(1, 0)}}
	calls := 0
	client := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "login.microsoftonline.com" {
			t.Fatal("wrong token endpoint")
		}
		if e := r.ParseForm(); e != nil {
			t.Fatal(e)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "synthetic-old-refresh" {
			t.Fatal("wrong refresh grant")
		}
		return reply(200, `{"access_token":"synthetic-new-access","refresh_token":"synthetic-new-refresh","expires_in":3600,"token_type":"Bearer"}`), nil
	})}
	manager := &Manager{Config: config, Store: store, Ref: "SYNTHETIC_TOKEN", Client: client}
	access, e := manager.Access(context.Background(), false)
	if e != nil || access != "synthetic-new-access" || store.saves != 1 || store.token.RefreshToken != "synthetic-new-refresh" {
		t.Fatal("rotation not durable", e)
	}
	if _, e = manager.Access(context.Background(), false); e != nil || calls != 1 {
		t.Fatal("valid token refreshed unnecessarily")
	}
	store.fail = true
	manager.token.Expiry = time.Unix(1, 0)
	client.Transport = transport(func(*http.Request) (*http.Response, error) {
		return reply(200, `{"access_token":"synthetic-another","refresh_token":"synthetic-rotated","expires_in":3600,"token_type":"Bearer"}`), nil
	})
	if _, e = manager.Access(context.Background(), false); !errors.Is(e, mailsync.ErrCredential) {
		t.Fatal("unsaved rotated token used", e)
	}
}
func TestPKCECallbackStateAndNoTokenLeak(t *testing.T) {
	config, _ := Config("gmail", "synthetic-client", "synthetic-secret", "")
	var flow *Flow
	calls := 0
	client := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		calls++
		if e := r.ParseForm(); e != nil {
			t.Fatal(e)
		}
		if r.Form.Get("code_verifier") != flow.Verifier || r.Form.Get("code") != "synthetic-code" {
			t.Fatal("PKCE exchange missing")
		}
		return reply(200, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600,"token_type":"Bearer"}`), nil
	})}
	flow = NewFlow(config, "http://127.0.0.1:12345/callback", client)
	u, _ := url.Parse(flow.URL())
	if u.Query().Get("code_challenge_method") != "S256" || u.Query().Get("state") != flow.State || u.Query().Get("code_challenge") == flow.Verifier {
		t.Fatal("unsafe authorization URL")
	}
	bad := httptest.NewRecorder()
	flow.ServeHTTP(bad, httptest.NewRequest("GET", "/callback?state=wrong&code=attacker", nil))
	if bad.Code != 400 || calls != 0 {
		t.Fatal("CSRF state accepted")
	}
	good := httptest.NewRecorder()
	flow.ServeHTTP(good, httptest.NewRequest("GET", "/callback?state="+flow.State+"&code=synthetic-code", nil))
	result := <-flow.Result
	if good.Code != 200 || result.Err != nil || result.Token.RefreshToken != "synthetic-refresh" || strings.Contains(good.Body.String(), "synthetic-refresh") {
		t.Fatal("callback failed or exposed token")
	}
	replay := httptest.NewRecorder()
	flow.ServeHTTP(replay, httptest.NewRequest("GET", "/callback?state="+flow.State+"&code=synthetic-code", nil))
	if replay.Code != 409 || calls != 1 {
		t.Fatal("authorization code replayed")
	}
}
func TestInfisicalReadAndWrite(t *testing.T) {
	token := &oauth2.Token{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", Expiry: time.Now().Add(time.Hour)}
	encoded, _ := json.Marshal(token)
	methods := []string{}
	store := &Infisical{ProjectID: "synthetic-project", Environment: "dev", Path: "/mail", Token: "synthetic-machine-token", Client: &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		methods = append(methods, r.Method)
		if r.URL.Host != "us.infisical.com" || r.Header.Get("Authorization") != "Bearer synthetic-machine-token" {
			t.Fatal("wrong secret host or scope")
		}
		switch r.Method {
		case "GET":
			if r.URL.Query().Get("projectId") != "synthetic-project" || r.URL.Query().Get("secretPath") != "/mail" {
				t.Fatal("secret scope omitted")
			}
			body, _ := json.Marshal(map[string]any{"secret": map[string]any{"secretValue": string(encoded)}})
			return reply(200, string(body)), nil
		case "PATCH":
			return reply(404, `{}`), nil
		case "POST":
			var b map[string]any
			if json.NewDecoder(r.Body).Decode(&b) != nil || b["projectId"] != "synthetic-project" || !strings.Contains(b["secretValue"].(string), "synthetic-refresh") {
				t.Fatal("token not saved in secret store")
			}
			return reply(201, `{}`), nil
		}
		return nil, nil
	})}}
	got, e := store.Load(context.Background(), "SYNTHETIC_TOKEN")
	if e != nil || got.RefreshToken != token.RefreshToken {
		t.Fatal(e)
	}
	if e = store.Save(context.Background(), "SYNTHETIC_TOKEN", token); e != nil {
		t.Fatal(e)
	}
	if strings.Join(methods, ",") != "GET,PATCH,POST" {
		t.Fatal(methods)
	}
}
func TestRevokedRefreshIsSanitized(t *testing.T) {
	config, _ := Config("gmail", "synthetic", "", "")
	store := &memoryTokens{token: &oauth2.Token{AccessToken: "old", RefreshToken: "synthetic-refresh", Expiry: time.Unix(1, 0)}}
	manager := Manager{Config: config, Store: store, Ref: "SYNTHETIC", Client: &http.Client{Transport: transport(func(*http.Request) (*http.Response, error) {
		return reply(400, `{"error":"invalid_grant","error_description":"private mailbox detail must not leak"}`), nil
	})}}
	if _, e := manager.Access(context.Background(), false); !errors.Is(e, mailsync.ErrReconnect) || strings.Contains(e.Error(), "private") {
		t.Fatal(e)
	}
}

func TestInfisicalMachineTokenRenewal(t *testing.T) {
	logins := 0
	store := &Infisical{ProjectID: "synthetic-project", Environment: "dev", UniversalClientID: "synthetic-client", UniversalClientSecret: "synthetic-secret", Client: &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "universal-auth/login") {
			logins++
			if r.ParseForm() != nil || r.Form.Get("clientId") != "synthetic-client" {
				t.Fatal("wrong machine login")
			}
			return reply(200, `{"accessToken":"synthetic-machine-token","expiresIn":7200}`), nil
		}
		if r.Header.Get("Authorization") != "Bearer synthetic-machine-token" {
			t.Fatal("missing machine token")
		}
		return reply(404, `{}`), nil
	})}}
	_, _ = store.Load(context.Background(), "SYNTHETIC")
	_, _ = store.Load(context.Background(), "SYNTHETIC")
	if logins != 1 {
		t.Fatal("machine login not cached")
	}
	store.machineExpiry = time.Unix(1, 0)
	_, _ = store.Load(context.Background(), "SYNTHETIC")
	if logins != 2 {
		t.Fatal("machine token was not renewed")
	}
}
