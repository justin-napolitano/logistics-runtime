package mailsync

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type AccessToken func(context.Context, bool) (string, error)
type API struct {
	Client *http.Client
	Token  AccessToken
	Sleep  func(context.Context, time.Duration) error
}

func NewAPI(client *http.Client, token AccessToken) *API {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &API{Client: &copy, Token: token, Sleep: sleep}
}
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (a *API) get(ctx context.Context, endpoint string, limit int, headers map[string]string) ([]byte, int, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || (u.Host != "gmail.googleapis.com" && u.Host != "graph.microsoft.com") {
		return nil, 0, ErrInvalid
	}
	refreshed := false
	didRefresh := false
	for attempt := 0; attempt < 4; attempt++ {
		token, e := a.Token(ctx, refreshed)
		refreshed = false
		if e != nil {
			return nil, 0, e
		}
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if e != nil {
			return nil, 0, ErrInvalid
		}
		req.Header.Set("Authorization", "Bearer "+token)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		response, e := a.Client.Do(req)
		if e != nil {
			if ctx.Err() != nil {
				return nil, 0, ctx.Err()
			}
			if attempt < 3 {
				wait := a.Sleep
				if wait == nil {
					wait = sleep
				}
				if e = wait(ctx, time.Duration(1<<attempt)*time.Second); e != nil {
					return nil, 0, e
				}
				continue
			}
			return nil, 0, ErrProvider
		}
		data, e := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
		response.Body.Close()
		if e != nil {
			return nil, response.StatusCode, ErrProvider
		}
		if len(data) > limit {
			return nil, response.StatusCode, ErrTooLarge
		}
		status := response.StatusCode
		if status >= 200 && status < 300 {
			return data, status, nil
		}
		if status == 401 && !didRefresh && attempt < 3 {
			didRefresh = true
			refreshed = true
			continue
		}
		if status == 401 || status == 403 {
			return nil, status, ErrReconnect
		}
		if status == 404 {
			return nil, status, ErrGone
		}
		if status == 410 {
			return data, status, ErrCursorExpired
		}
		if status == 429 || status >= 500 {
			if attempt == 3 {
				if status == 429 {
					return nil, status, ErrRateLimited
				}
				return nil, status, ErrProvider
			}
			delay := time.Duration(1<<attempt) * time.Second
			if value := response.Header.Get("Retry-After"); value != "" {
				if n, e := strconv.Atoi(value); e == nil && n >= 0 {
					delay = time.Duration(n) * time.Second
				} else if at, e := http.ParseTime(value); e == nil {
					delay = time.Until(at)
					if delay < 0 {
						delay = 0
					}
				}
			}
			if delay > 30*time.Second {
				if status == 429 {
					return nil, status, ErrRateLimited
				}
				return nil, status, ErrProvider
			}
			wait := a.Sleep
			if wait == nil {
				wait = sleep
			}
			if e = wait(ctx, delay); e != nil {
				return nil, status, e
			}
			continue
		}
		// Preserve only a provider error code needed to recover delta cursors, never its text.
		if status == 400 && u.Host == "graph.microsoft.com" {
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			_ = json.Unmarshal(data, &body)
			code := strings.ToLower(body.Error.Code)
			if code == "syncstatenotfound" || code == "invaliddeltatoken" || code == "resyncrequired" {
				return nil, status, ErrCursorExpired
			}
		}
		return nil, status, ErrProvider
	}
	return nil, 0, ErrProvider
}
func (a *API) json(ctx context.Context, u string, out any, headers map[string]string) error {
	b, _, err := a.get(ctx, u, 1<<20, headers)
	if err != nil {
		return err
	}
	if json.Unmarshal(b, out) != nil {
		return ErrProvider
	}
	return nil
}
