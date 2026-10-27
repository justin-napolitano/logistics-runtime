package mailsync

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}
func TestGmailBackfillHistoryAndRaw(t *testing.T) {
	ctx := context.Background()
	c := sampleConnection()
	c.Folder = "Orders"
	listCalls := 0
	historyCalls := 0
	rawCalls := 0
	api := NewAPI(&http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer synthetic-token" {
			t.Fatal("missing authorization")
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/profile"):
			return response(200, `{"emailAddress":"synthetic@example.invalid","historyId":"100"}`), nil
		case strings.HasSuffix(r.URL.Path, "/messages"):
			listCalls++
			if r.URL.Query().Get("labelIds") != "Orders" || !strings.Contains(r.URL.Query().Get("q"), "after:") {
				t.Fatal("unbounded backfill")
			}
			if listCalls == 1 {
				return response(200, `{"messages":[{"id":"first"}],"nextPageToken":"page-two"}`), nil
			}
			if r.URL.Query().Get("pageToken") != "page-two" {
				t.Fatal("page cursor lost")
			}
			return response(200, `{"messages":[]}`), nil
		case strings.HasSuffix(r.URL.Path, "/history"):
			historyCalls++
			if r.URL.Query().Get("startHistoryId") != "100" {
				t.Fatal("backfill lost starting watermark")
			}
			if historyCalls == 1 {
				return response(200, `{"history":[{"messagesAdded":[{"message":{"id":"new"}},{"message":{"id":"new"}}]}],"nextPageToken":"history-two","historyId":"150"}`), nil
			}
			return response(200, `{"history":[],"historyId":"200"}`), nil
		case strings.Contains(r.URL.Path, "/messages/"):
			if r.URL.Query().Get("format") == "metadata" {
				return response(200, `{"internalDate":"1793095200000","payload":{"headers":[{"name":"From","value":"auto-confirm@amazon.com"}]}}`), nil
			}
			rawCalls++
			return response(200, `{"raw":"`+base64.RawURLEncoding.EncodeToString(rawMail("synthetic"))+`"}`), nil
		}
		t.Fatal("unexpected provider request", r.URL.Path)
		return nil, nil
	})}, func(context.Context, bool) (string, error) { return "synthetic-token", nil })
	g := Gmail{api}
	if _, e := g.Identity(ctx); e != nil {
		t.Fatal(e)
	}
	p, e := g.Page(ctx, c, Cursor{})
	if e != nil || p.Next.HistoryID != "100" || p.Complete {
		t.Fatal(p, e)
	}
	p, e = g.Page(ctx, c, p.Next)
	if e != nil || p.Next.Phase != "gmail_history" {
		t.Fatal(p, e)
	}
	p, e = g.Page(ctx, c, p.Next)
	if e != nil || len(p.IDs) != 1 || p.Next.HistoryID != "100" {
		t.Fatal(p, e)
	}
	p, e = g.Page(ctx, c, p.Next)
	if e != nil || !p.Complete || p.Next.HistoryID != "200" {
		t.Fatal(p, e)
	}
	b, e := g.Message(ctx, c, "new")
	if e != nil || len(b) == 0 || rawCalls != 1 {
		t.Fatal(e)
	}
}
func TestOutlookDeltaAndOriginBoundary(t *testing.T) {
	next := "https://graph.microsoft.com/v1.0/me/mailFolders/inbox/messages/delta?$skiptoken=synthetic"
	delta := "https://graph.microsoft.com/v1.0/me/mailfolders('synthetic-folder')/messages/delta?$deltatoken=synthetic"
	calls := 0
	api := NewAPI(&http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if strings.Contains(r.URL.Path, "/$value") {
			if !strings.Contains(r.Header.Get("Prefer"), "ImmutableId") {
				t.Fatal("unstable message IDs")
			}
			return response(200, string(rawMail("graph"))), nil
		}
		if calls == 1 {
			if !strings.Contains(r.URL.Query().Get("$filter"), "receivedDateTime ge") {
				t.Fatal("no backfill bound")
			}
			return response(200, fmt.Sprintf(`{"@odata.nextLink":%q,"value":[{"id":"eligible","from":{"emailAddress":{"address":"ship-confirm@amazon.com"}},"receivedDateTime":"2026-10-27T10:00:00Z"},{"id":"skip","from":{"emailAddress":{"address":"other@example.invalid"}},"receivedDateTime":"2026-10-27T10:00:00Z"},{"id":"gone","@removed":{"reason":"deleted"}}]}`, next)), nil
		}
		return response(200, fmt.Sprintf(`{"@odata.deltaLink":%q,"value":[]}`, delta)), nil
	})}, func(context.Context, bool) (string, error) { return "synthetic", nil })
	o := Outlook{api}
	c := sampleConnection()
	c.Provider = "outlook"
	p, e := o.Page(context.Background(), c, Cursor{})
	if e != nil || len(p.IDs) != 1 || p.IDs[0] != "eligible" || p.Complete {
		t.Fatal(p, e)
	}
	p, e = o.Page(context.Background(), c, p.Next)
	if e != nil || !p.Complete || p.Next.DeltaURL != delta {
		t.Fatal(p, e)
	}
	if _, e = o.Message(context.Background(), c, "eligible"); e != nil {
		t.Fatal(e)
	}
	before := calls
	for _, bad := range []string{"https://evil.invalid/v1.0/me/mailFolders/inbox/messages/delta", "http://graph.microsoft.com/v1.0/me/mailFolders/inbox/messages/delta", "https://graph.microsoft.com/v1.0/me/messages", "https://user:secret@graph.microsoft.com/v1.0/me/mailFolders/x/messages/delta"} {
		if _, e = o.Page(context.Background(), c, Cursor{DeltaURL: bad}); e == nil {
			t.Fatal("unsafe cursor URL accepted")
		}
	}
	if calls != before {
		t.Fatal("unsafe destination contacted")
	}
}
func TestRetriesRefreshAndReconnect(t *testing.T) {
	calls, forced := 0, 0
	api := NewAPI(&http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		switch calls {
		case 1:
			return response(401, `{}`), nil
		case 2:
			r := response(429, `{}`)
			r.Header.Set("Retry-After", "0")
			return r, nil
		}
		return response(200, `{}`), nil
	})}, func(_ context.Context, force bool) (string, error) {
		if force {
			forced++
		}
		return "synthetic", nil
	})
	api.Sleep = func(context.Context, time.Duration) error { return nil }
	if _, _, e := api.get(context.Background(), gmailBase+"profile", 100, nil); e != nil || calls != 3 || forced != 1 {
		t.Fatal(calls, forced, e)
	}
	api.Client.Transport = roundTrip(func(*http.Request) (*http.Response, error) { return response(404, `{}`), nil })
	if _, e := (Gmail{api}).Page(context.Background(), sampleConnection(), Cursor{Phase: "gmail_history", HistoryID: "1"}); e != ErrCursorExpired {
		t.Fatal("history expiry not identified", e)
	}
	api.Client.Transport = roundTrip(func(*http.Request) (*http.Response, error) { return response(410, `{}`), nil })
	if _, e := (Outlook{api}).Page(context.Background(), sampleConnection(), Cursor{}); e != ErrCursorExpired {
		t.Fatal("delta expiry not identified", e)
	}
}

func TestTransportRetryAndLongRetryAfter(t *testing.T) {
	calls := 0
	api := NewAPI(&http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("synthetic transport failure")
		}
		return response(200, `{}`), nil
	})}, func(context.Context, bool) (string, error) { return "synthetic", nil })
	api.Sleep = func(context.Context, time.Duration) error { return nil }
	if _, _, err := api.get(context.Background(), gmailBase+"profile", 100, nil); err != nil || calls != 2 {
		t.Fatal("transport retry failed", calls, err)
	}
	api.Client.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
		r := response(429, `{}`)
		r.Header.Set("Retry-After", "3600")
		return r, nil
	})
	api.Sleep = func(context.Context, time.Duration) error { t.Fatal("long backoff blocked worker"); return nil }
	if _, _, err := api.get(context.Background(), gmailBase+"profile", 100, nil); err != ErrRateLimited {
		t.Fatal("long backoff not deferred", err)
	}
}

func TestOutlookBackfillLimit(t *testing.T) {
	link := "https://graph.microsoft.com/v1.0/me/mailFolders/inbox/messages/delta?$deltatoken=synthetic"
	api := NewAPI(&http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		return response(200, fmt.Sprintf(`{"@odata.deltaLink":%q,"value":[{"id":"synthetic"}]}`, link)), nil
	})}, func(context.Context, bool) (string, error) { return "synthetic", nil })
	cursor := Cursor{Phase: "outlook_backfill", Seen: 4999, DeltaURL: link}
	if _, err := (Outlook{api}).Page(context.Background(), sampleConnection(), cursor); err != ErrWindowTooBroad {
		t.Fatal("truncated initial sync accepted", err)
	}
	cursor.Phase = "outlook_delta"
	if _, err := (Outlook{api}).Page(context.Background(), sampleConnection(), cursor); err != nil {
		t.Fatal("incremental sync incorrectly capped", err)
	}
}
