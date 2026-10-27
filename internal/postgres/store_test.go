package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/justin-napolitano/logistics-runtime/internal/httpapi"
	"github.com/justin-napolitano/logistics-runtime/internal/inventory"
	"github.com/justin-napolitano/logistics-runtime/internal/postgres"
)

func ptr(n int64) *int64 { return &n }

func TestPostgresInventory(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to a dedicated logistics_test PostgreSQL database")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Path != "/logistics_test" {
		t.Fatal("integration database must be named logistics_test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	s, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatalf("migration replay: %v", err)
	}
	if err = s.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	id := "test." + strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "")
	rows := []inventory.Proposal{{ID: id, Name: "towels", Category: "bathroom", Unit: "each", Quantity: 4, Source: "synthetic"}}
	added, err := s.ImportProposals(ctx, rows)
	if err != nil || added != 1 {
		t.Fatalf("import: %d %v", added, err)
	}
	item, err := s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if item.Desired != nil || item.OnHand != nil || item.Incoming != nil || item.Unit != "" || item.ProposedDesired == nil || *item.ProposedDesired != 4 {
		t.Fatal("proposal became confirmed inventory")
	}
	in := inventory.Input{Name: "towels", Category: "bathroom", Unit: "each", Desired: ptr(4), OnHand: ptr(0), Incoming: ptr(1)}
	item, err = s.Replace(ctx, id, item.Revision, in)
	if err != nil {
		t.Fatal(err)
	}
	if item.CountedAt == nil || inventory.Compare(item).Gap == nil || *inventory.Compare(item).Gap != 3 {
		t.Fatal("known-zero comparison failed")
	}
	added, err = s.ImportProposals(ctx, rows)
	if err != nil || added != 0 {
		t.Fatalf("reimport: %d %v", added, err)
	}
	current, err := s.Get(ctx, id)
	if err != nil || current.OnHand == nil || *current.OnHand != 0 || current.Revision != item.Revision {
		t.Fatal("reimport overwrote owner state")
	}
	if _, err = s.Replace(ctx, id, item.Revision-1, in); !errors.Is(err, inventory.ErrConflict) {
		t.Fatalf("stale revision: %v", err)
	}
	changedUnit := in
	changedUnit.Unit = "pack"
	if _, err = s.Replace(ctx, id, item.Revision, changedUnit); !errors.Is(err, inventory.ErrInvalid) {
		t.Fatalf("silently relabeled stock: %v", err)
	}
	if _, err = s.Create(ctx, id, in); !errors.Is(err, inventory.ErrConflict) {
		t.Fatalf("duplicate create: %v", err)
	}
	var wg sync.WaitGroup
	out := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.Replace(ctx, id, item.Revision, in); out <- err }()
	}
	wg.Wait()
	close(out)
	ok, conflict := 0, 0
	for err := range out {
		if err == nil {
			ok++
		} else if errors.Is(err, inventory.ErrConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("concurrent updates: successes=%d conflicts=%d", ok, conflict)
	}
	listed, err := s.List(ctx, inventory.Filter{Category: "bathroom", View: "shortages", Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range listed {
		if v.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatal("missing shortage")
	}
	// Exercise the HTTP path with real persistence rather than a mock store.
	token := strings.Repeat("t", 32)
	handler, err := httpapi.New(s, token, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	request, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/v1/items/"+id, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var view inventory.View
	if err = json.NewDecoder(response.Body).Decode(&view); err != nil || response.StatusCode != 200 || view.Gap == nil || *view.Gap != 3 {
		t.Fatalf("HTTP round trip: %d %+v %v", response.StatusCode, view, err)
	}
	// The database itself enforces nonnegative stock, even outside the API.
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err = conn.Exec(ctx, `UPDATE logistics.items SET on_hand=-1 WHERE id=$1`, id); err == nil {
		t.Fatal("database accepted negative stock")
	}
	if _, err = s.Get(ctx, "missing."+id); !errors.Is(err, inventory.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	t.Run("migration drift refused", func(t *testing.T) {
		var name, checksum string
		if err := conn.QueryRow(ctx, `SELECT name,checksum FROM logistics.schema_migrations LIMIT 1`).Scan(&name, &checksum); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := conn.Exec(ctx, `UPDATE logistics.schema_migrations SET checksum=$2 WHERE name=$1`, name, checksum); err != nil {
				t.Error(err)
			}
		}()
		if _, err := conn.Exec(ctx, `UPDATE logistics.schema_migrations SET checksum='tampered' WHERE name=$1`, name); err != nil {
			t.Fatal(err)
		}
		if err := s.Migrate(ctx); err == nil {
			t.Fatal("accepted changed migration checksum")
		}
	})
	t.Run("runtime grants", func(t *testing.T) {
		role := pgx.Identifier{"runtime_" + strings.TrimPrefix(id, "test.")}.Sanitize()
		if _, err := conn.Exec(ctx, `CREATE ROLE `+role+` NOLOGIN`); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_, _ = conn.Exec(ctx, `REVOKE ALL ON logistics.items FROM `+role)
			_, _ = conn.Exec(ctx, `REVOKE ALL ON SCHEMA logistics FROM `+role)
			if _, err := conn.Exec(ctx, `DROP ROLE `+role); err != nil {
				t.Error(err)
			}
		}()
		if _, err := conn.Exec(ctx, `GRANT USAGE ON SCHEMA logistics TO `+role+`; GRANT SELECT,INSERT,UPDATE ON logistics.items TO `+role); err != nil {
			t.Fatal(err)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+role); err != nil {
			t.Fatal(err)
		}
		var canDelete, canDDL bool
		if err := tx.QueryRow(ctx, `SELECT has_table_privilege(current_user,'logistics.items','DELETE'),has_schema_privilege(current_user,'logistics','CREATE')`).Scan(&canDelete, &canDDL); err != nil {
			t.Fatal(err)
		}
		if canDelete || canDDL {
			t.Fatal("runtime received excess grants")
		}
		if _, err := tx.Exec(ctx, `UPDATE logistics.items SET notes='synthetic runtime update' WHERE id=$1`, id); err != nil {
			t.Fatalf("runtime cannot update inventory: %v", err)
		}
	})
}
