package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/justin-napolitano/logistics-runtime/internal/httpapi"
	"github.com/justin-napolitano/logistics-runtime/internal/inventory"
	"github.com/justin-napolitano/logistics-runtime/internal/mailsync"
	"github.com/justin-napolitano/logistics-runtime/internal/ordermail"
	"github.com/justin-napolitano/logistics-runtime/internal/postgres"
)

type persistedProvider struct{ fail bool }

func (p persistedProvider) Identity(context.Context) (string, error) {
	return "synthetic-mailbox-id", nil
}
func (p persistedProvider) Page(_ context.Context, _ mailsync.Connection, c mailsync.Cursor) (mailsync.Page, error) {
	return mailsync.Page{IDs: []string{"one", "two"}, Next: mailsync.Cursor{Phase: "complete"}, Complete: true}, nil
}
func (p persistedProvider) Message(_ context.Context, _ mailsync.Connection, id string) ([]byte, error) {
	if id == "two" && p.fail {
		return nil, mailsync.ErrProvider
	}
	return []byte(fmt.Sprintf("From: auto-confirm@amazon.com\r\nMessage-ID: <sync-%s@example.invalid>\r\nDate: Tue, 27 Oct 2026 10:00:00 +0000\r\nSubject: Your Amazon Fresh order\r\n\r\nOrder 111-1234567-1234567\r\n", id)), nil
}
func TestDurableMailboxLeaseCheckpointAndAPI(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("dedicated logistics_test database required")
	}
	u, e := url.Parse(dsn)
	if e != nil || u.Path != "/logistics_test" {
		t.Fatal("dedicated test DB required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, e := postgres.Open(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	config := mailsync.Connection{ID: "mail." + suffix, Provider: "gmail", Account: "mail-test." + suffix, CredentialRef: "SYNTHETIC_TOKEN_" + suffix, Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	if e = s.ConfigureMail(ctx, config); e != nil {
		t.Fatal(e)
	}
	if e = s.ConfigureMail(ctx, config); e != nil {
		t.Fatal("configure replay", e)
	}
	changed := config
	changed.Provider = "outlook"
	if e = s.ConfigureMail(ctx, changed); !errors.Is(e, inventory.ErrConflict) {
		t.Fatal("silent provider change", e)
	}
	lease, e := s.AcquireMail(ctx, config.ID)
	if e != nil {
		t.Fatal(e)
	}
	second, e := postgres.Open(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer second.Close()
	if _, e = second.AcquireMail(ctx, config.ID); !errors.Is(e, mailsync.ErrBusy) {
		t.Fatal("cross-process lease overlap", e)
	}
	lease.Close()
	r := mailsync.Runner{Store: s}
	_, e = r.Run(ctx, config.ID, func(mailsync.Connection) (mailsync.Provider, error) { return persistedProvider{fail: true}, nil })
	if !errors.Is(e, mailsync.ErrProvider) {
		t.Fatal(e)
	}
	lease, e = s.AcquireMail(ctx, config.ID)
	if e != nil {
		t.Fatal(e)
	}
	if lease.Progress().Cursor.Phase != "" || lease.Progress().Status != "error" {
		t.Fatal("partial page checkpoint advanced")
	}
	lease.Close()
	// A new store instance resumes from durable progress and replays without duplication.
	status, e := (mailsync.Runner{Store: second}).Run(ctx, config.ID, func(mailsync.Connection) (mailsync.Provider, error) { return persistedProvider{}, nil })
	if e != nil || status.State != "ready" {
		t.Fatal(status, e)
	}
	order, e := s.Order(ctx, ordermail.OrderID(config.Account, "111-1234567-1234567"))
	if e != nil || len(order.Reports) != 2 {
		t.Fatal("duplicate or missing report", order, e)
	}
	lease, e = s.AcquireMail(ctx, config.ID)
	if e != nil {
		t.Fatal(e)
	}
	p := lease.Progress()
	if p.Cursor.Phase != "complete" || p.LastSuccess == nil || p.IdentityHash == "" {
		t.Fatal("durable checkpoint absent")
	}
	lease.Close()
	handler, e := httpapi.New(s, strings.Repeat("t", 32), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if e != nil {
		t.Fatal(e)
	}
	req := httptest.NewRequest("GET", "/v1/mail/sync-status", nil)
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("t", 32))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatal("sync status unavailable", w.Code)
	}
	for _, secret := range []string{"credential_ref", "cursor", "identity_hash", config.CredentialRef} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("private synchronization details exposed")
		}
	}
	var result []mailsync.Status
	if json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal("invalid status response")
	}
}
