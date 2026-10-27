package mailsync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justin-napolitano/logistics-runtime/internal/ordermail"
)

type memoryStore struct {
	mu       sync.Mutex
	busy     bool
	c        Connection
	p        Progress
	imported map[string]ordermail.Message
	calls    int
}

func (s *memoryStore) ConfigureMail(context.Context, Connection) error { return nil }
func (s *memoryStore) MailConnections(context.Context) ([]Connection, error) {
	return []Connection{s.c}, nil
}
func (s *memoryStore) MailStatuses(context.Context) ([]Status, error) { return nil, nil }
func (s *memoryStore) AcquireMail(context.Context, string) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy {
		return nil, ErrBusy
	}
	s.busy = true
	return &memoryLease{s}, nil
}
func (s *memoryStore) ImportMail(_ context.Context, _ string, m ordermail.Message) (ordermail.ImportResult, error) {
	s.calls++
	if s.imported == nil {
		s.imported = map[string]ordermail.Message{}
	}
	_, duplicate := s.imported[m.MessageKey]
	s.imported[m.MessageKey] = m
	return ordermail.ImportResult{Duplicate: duplicate}, nil
}

type memoryLease struct{ s *memoryStore }

func (l *memoryLease) Connection() Connection { return l.s.c }
func (l *memoryLease) Progress() Progress     { return l.s.p }
func (l *memoryLease) Save(ctx context.Context, p Progress) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	l.s.p = p
	return nil
}
func (l *memoryLease) Close() { l.s.mu.Lock(); defer l.s.mu.Unlock(); l.s.busy = false }

type fakeProvider struct {
	identity string
	page     func(Cursor) (Page, error)
	message  func(string) ([]byte, error)
}

func (f fakeProvider) Identity(context.Context) (string, error)                     { return f.identity, nil }
func (f fakeProvider) Page(_ context.Context, _ Connection, c Cursor) (Page, error) { return f.page(c) }
func (f fakeProvider) Message(_ context.Context, _ Connection, id string) ([]byte, error) {
	return f.message(id)
}
func sampleConnection() Connection {
	return Connection{ID: "synthetic.gmail", Provider: "gmail", Account: "synthetic.orders", CredentialRef: "SYNTHETIC_MAIL_TOKEN", Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
}
func rawMail(id string) []byte {
	return []byte(fmt.Sprintf("From: auto-confirm@amazon.com\r\nMessage-ID: <%s@example.invalid>\r\nDate: Tue, 27 Oct 2026 10:00:00 +0000\r\nSubject: Your Amazon Fresh order\r\n\r\nOrder 111-1234567-1234567\r\n", id))
}
func TestPageCheckpointAndRetry(t *testing.T) {
	store := &memoryStore{c: sampleConnection()}
	fail := true
	provider := fakeProvider{identity: "synthetic-id", page: func(c Cursor) (Page, error) {
		return Page{IDs: []string{"one", "two"}, Next: Cursor{Phase: "done"}, Complete: true}, nil
	}, message: func(id string) ([]byte, error) {
		if id == "two" && fail {
			return nil, ErrProvider
		}
		return rawMail(id), nil
	}}
	build := func(Connection) (Provider, error) { return provider, nil }
	r := Runner{Store: store}
	if _, e := r.Run(context.Background(), store.c.ID, build); !errors.Is(e, ErrProvider) {
		t.Fatal(e)
	}
	if store.p.Cursor.Phase != "" || len(store.imported) != 1 || store.p.Status != "error" {
		t.Fatal("advanced past unhandled message")
	}
	fail = false
	status, e := r.Run(context.Background(), store.c.ID, build)
	if e != nil || status.State != "ready" || len(store.imported) != 2 || store.calls != 3 || store.p.Cursor.Phase != "done" {
		t.Fatalf("resume %+v %v", status, e)
	}
	if store.p.LastSuccess == nil {
		t.Fatal("no success marker")
	}
}
func TestExpiryPauseReconnectAndIdentity(t *testing.T) {
	store := &memoryStore{c: sampleConnection(), p: Progress{Cursor: Cursor{Phase: "expired"}}}
	pages := 0
	provider := fakeProvider{identity: "first", page: func(c Cursor) (Page, error) {
		pages++
		if c.Phase == "expired" {
			return Page{}, ErrCursorExpired
		}
		return Page{Next: Cursor{Phase: "fresh"}, Complete: true}, nil
	}, message: func(string) ([]byte, error) { return nil, nil }}
	build := func(Connection) (Provider, error) { return provider, nil }
	r := Runner{Store: store}
	if _, e := r.Run(context.Background(), store.c.ID, build); e != nil || pages != 2 || store.p.RecoveryCount != 1 {
		t.Fatal("cursor recovery", e)
	}
	store.p.Status = "paused"
	called := false
	if _, e := r.Run(context.Background(), store.c.ID, func(Connection) (Provider, error) { called = true; return provider, nil }); e != nil || called {
		t.Fatal("paused source contacted provider")
	}
	store.p.Status = "pending"
	provider.identity = "different"
	if _, e := r.Run(context.Background(), store.c.ID, build); !errors.Is(e, ErrReconnect) || store.p.Status != "reconnect_required" {
		t.Fatal("mailbox identity changed silently", e)
	}
	called = false
	if _, e := r.Run(context.Background(), store.c.ID, func(Connection) (Provider, error) { called = true; return provider, nil }); !errors.Is(e, ErrReconnect) || called {
		t.Fatal("background reconnect loop")
	}
}
func TestBoundedWorkAndPermanentOversize(t *testing.T) {
	store := &memoryStore{c: sampleConnection()}
	provider := fakeProvider{identity: "first", page: func(c Cursor) (Page, error) {
		return Page{IDs: []string{"large", "deleted", "unrelated"}, Next: Cursor{Phase: c.Phase + "x"}}, nil
	}, message: func(id string) ([]byte, error) {
		switch id {
		case "large":
			return nil, ErrTooLarge
		case "deleted":
			return nil, ErrGone
		}
		return nil, nil
	}}
	status, e := (Runner{Store: store, MaxPages: 2}).Run(context.Background(), store.c.ID, func(Connection) (Provider, error) { return provider, nil })
	if e != nil || status.State != "pending" || store.p.Cursor.Phase != "xx" || len(store.imported) != 1 || status.Skipped != 4 {
		t.Fatalf("bounded processing %+v %v", status, e)
	}
	for _, m := range store.imported {
		if !strings.HasPrefix(m.MessageKey, "provider-limit:") || m.Reason != "mailbox_message_too_large" {
			t.Fatal("missing permanent size exception")
		}
	}
	lease, e := store.AcquireMail(context.Background(), store.c.ID)
	if e != nil {
		t.Fatal(e)
	}
	defer lease.Close()
	if _, e = (Runner{Store: store}).Run(context.Background(), store.c.ID, nil); !errors.Is(e, ErrBusy) {
		t.Fatal("overlapping run accepted")
	}
}
func TestCancellationPersistsFailureWithoutAdvancing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &memoryStore{c: sampleConnection()}
	p := fakeProvider{identity: "first", page: func(Cursor) (Page, error) { return Page{IDs: []string{"one"}, Next: Cursor{Phase: "bad"}}, nil }, message: func(string) ([]byte, error) { cancel(); return nil, ctx.Err() }}
	if _, e := (Runner{Store: store}).Run(ctx, store.c.ID, func(Connection) (Provider, error) { return p, nil }); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if store.p.Cursor.Phase != "" || store.p.Status != "error" {
		t.Fatal("cancelled page acknowledged")
	}
}
