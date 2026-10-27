package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/justin-napolitano/logistics-runtime/internal/mailsync"
	"github.com/justin-napolitano/logistics-runtime/internal/postgres"
)

func TestMailboxCLIControlsWithoutProviderAccess(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("dedicated logistics_test database required")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Path != "/logistics_test" {
		t.Fatal("dedicated test database required")
	}
	t.Setenv("DATABASE_URL", dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	c := mailsync.Connection{ID: "cli." + suffix, Provider: "gmail", Account: "synthetic", CredentialRef: "CLI_SYNTHETIC_" + suffix, Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	b, _ := json.Marshal(c)
	path := filepath.Join(t.TempDir(), "synthetic.connection.json")
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err = runMailbox(logger, "mail-configure", []string{path}); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"mail-pause", "mail-sync", "mail-resume", "mail-reset"} {
		if err = runMailbox(logger, command, []string{c.ID}); err != nil {
			t.Fatal(command, err)
		}
		lease, err := s.AcquireMail(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		p := lease.Progress()
		lease.Close()
		want := "pending"
		if command == "mail-pause" || command == "mail-sync" {
			want = "paused"
		}
		if p.Status != want || p.LastAttempt != nil || p.Cursor != (mailsync.Cursor{}) {
			t.Fatal("control unexpectedly contacted provider or changed cursor", command, p)
		}
	}
	if err = runMailbox(logger, "mail-status", nil); err != nil {
		t.Fatal(err)
	}
}
