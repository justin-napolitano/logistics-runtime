// Package mailauth manages OAuth consent/refresh with external secret storage.
package mailauth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/justin-napolitano/logistics-runtime/internal/mailsync"
	"golang.org/x/oauth2"
)

type TokenStore interface {
	Load(context.Context, string) (*oauth2.Token, error)
	Save(context.Context, string, *oauth2.Token) error
}

func Config(provider, id, secret, tenant string) (oauth2.Config, error) {
	if id == "" || strings.ContainsAny(id, "\r\n") {
		return oauth2.Config{}, mailsync.ErrInvalid
	}
	config := oauth2.Config{ClientID: id, ClientSecret: secret}
	switch provider {
	case "gmail":
		config.Endpoint = oauth2.Endpoint{AuthURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: "https://oauth2.googleapis.com/token", AuthStyle: oauth2.AuthStyleInParams}
		config.Scopes = []string{"https://www.googleapis.com/auth/gmail.readonly"}
	case "outlook":
		if tenant == "" {
			tenant = "common"
		}
		for _, r := range tenant {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return config, mailsync.ErrInvalid
			}
		}
		base := "https://login.microsoftonline.com/" + tenant + "/oauth2/v2.0/"
		config.Endpoint = oauth2.Endpoint{AuthURL: base + "authorize", TokenURL: base + "token", AuthStyle: oauth2.AuthStyleInParams}
		config.Scopes = []string{"offline_access", "https://graph.microsoft.com/Mail.Read", "https://graph.microsoft.com/User.Read"}
	default:
		return config, mailsync.ErrInvalid
	}
	return config, nil
}
func SafeClient(client *http.Client) *http.Client {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copy
}
func tokenError(err error) error {
	var retrieve *oauth2.RetrieveError
	if errors.As(err, &retrieve) {
		if retrieve.ErrorCode == "invalid_grant" || retrieve.ErrorCode == "interaction_required" || (retrieve.Response != nil && retrieve.Response.StatusCode == 401) {
			return mailsync.ErrReconnect
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return mailsync.ErrProvider
}

type Manager struct {
	Config oauth2.Config
	Store  TokenStore
	Ref    string
	Client *http.Client
	mu     sync.Mutex
	token  *oauth2.Token
}

// Access persists rotated credentials before any mailbox read can use them.
func (m *Manager) Access(ctx context.Context, force bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.token == nil {
		t, e := m.Store.Load(ctx, m.Ref)
		if e != nil {
			return "", e
		}
		m.token = t
	}
	current := *m.token
	if force {
		current.Expiry = time.Unix(1, 0)
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, SafeClient(m.Client))
	t, e := m.Config.TokenSource(ctx, &current).Token()
	if e != nil {
		return "", tokenError(e)
	}
	if t.AccessToken == "" || !strings.EqualFold(t.Type(), "Bearer") {
		return "", mailsync.ErrReconnect
	}
	if t.RefreshToken == "" {
		t.RefreshToken = m.token.RefreshToken
	}
	if t.AccessToken != m.token.AccessToken || t.RefreshToken != m.token.RefreshToken || !t.Expiry.Equal(m.token.Expiry) {
		if e = m.Store.Save(ctx, m.Ref, t); e != nil {
			return "", mailsync.ErrCredential
		}
		m.token = t
	}
	return t.AccessToken, nil
}

type Flow struct {
	Config          oauth2.Config
	Client          *http.Client
	State, Verifier string
	Result          chan FlowResult
	mu              sync.Mutex
	claimed         bool
}
type FlowResult struct {
	Token *oauth2.Token
	Err   error
}

func NewFlow(config oauth2.Config, redirect string, client *http.Client) *Flow {
	config.RedirectURL = redirect
	return &Flow{Config: config, Client: SafeClient(client), State: rand.Text(), Verifier: oauth2.GenerateVerifier(), Result: make(chan FlowResult, 1)}
}
func (f *Flow) URL() string {
	return f.Config.AuthCodeURL(f.State, oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(f.Verifier))
}
func (f *Flow) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	if r.Method != "GET" || r.URL.Path != "/callback" {
		http.Error(w, "Not found", 404)
		return
	}
	query := r.URL.Query()
	if len(query["state"]) != 1 || subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(f.State)) != 1 {
		http.Error(w, "Invalid authorization state", 400)
		return
	}
	f.mu.Lock()
	if f.claimed {
		f.mu.Unlock()
		http.Error(w, "Authorization already handled", 409)
		return
	}
	f.claimed = true
	f.mu.Unlock()
	if query.Get("error") != "" || len(query["code"]) != 1 || query.Get("code") == "" {
		f.Result <- FlowResult{Err: mailsync.ErrReconnect}
		http.Error(w, "Authorization was not completed", 400)
		return
	}
	ctx := context.WithValue(r.Context(), oauth2.HTTPClient, f.Client)
	token, e := f.Config.Exchange(ctx, query.Get("code"), oauth2.VerifierOption(f.Verifier))
	if e != nil {
		f.Result <- FlowResult{Err: tokenError(e)}
		http.Error(w, "Authorization exchange failed", 502)
		return
	}
	if token.RefreshToken == "" {
		f.Result <- FlowResult{Err: mailsync.ErrReconnect}
		http.Error(w, "Offline permission missing; reconnect with consent", 400)
		return
	}
	f.Result <- FlowResult{Token: token}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("Authorization received. Return to the terminal to confirm credentials were saved."))
}
