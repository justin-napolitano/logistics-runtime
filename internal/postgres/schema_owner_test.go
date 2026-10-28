package postgres_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/justin-napolitano/logistics-runtime/internal/postgres"
)

func TestMigratePrecreatedSchemaWithoutDatabaseCreate(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("dedicated logistics_test database required")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Path != "/logistics_test" {
		t.Fatal("dedicated test database required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	suffix := fmt.Sprint(time.Now().UnixNano())
	roleName, dbName := "schema_owner_"+suffix, "logistics_migration_test_"+suffix
	role, database := pgx.Identifier{roleName}.Sanitize(), pgx.Identifier{dbName}.Sanitize()
	if _, err = admin.Exec(ctx, `CREATE ROLE `+role+` LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, e := admin.Exec(ctx, `DROP ROLE `+role); e != nil {
			t.Error(e)
		}
	}()
	if _, err = admin.Exec(ctx, `CREATE DATABASE `+database); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, e := admin.Exec(ctx, `DROP DATABASE `+database+` WITH (FORCE)`); e != nil {
			t.Error(e)
		}
	}()
	u.Path = "/" + dbName
	control, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close(ctx)
	if _, err = control.Exec(ctx, `CREATE SCHEMA logistics AUTHORIZATION `+role+`; CREATE SCHEMA ledger; CREATE TABLE ledger.sentinel(id integer PRIMARY KEY); INSERT INTO ledger.sentinel VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	u.User = url.User(roleName) // Synthetic PostgreSQL test server uses trust auth.
	store, err := postgres.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for range 2 {
		if err = store.Migrate(ctx); err != nil {
			t.Fatal("schema-only migration failed", err)
		}
	}
	var canCreate bool
	if err = control.QueryRow(ctx, `SELECT has_database_privilege($1,current_database(),'CREATE')`, roleName).Scan(&canCreate); err != nil || canCreate {
		t.Fatal("migration relied on database CREATE", err)
	}
	var migrations, sentinel int
	if err = control.QueryRow(ctx, `SELECT count(*) FROM logistics.schema_migrations`).Scan(&migrations); err != nil || migrations != 3 {
		t.Fatal("migration history mismatch", migrations, err)
	}
	if err = control.QueryRow(ctx, `SELECT id FROM ledger.sentinel`).Scan(&sentinel); err != nil || sentinel != 1 {
		t.Fatal("unrelated synthetic data changed", err)
	}
	var canReadLedger bool
	if err = control.QueryRow(ctx, `SELECT has_table_privilege($1,'ledger.sentinel','SELECT')`, roleName).Scan(&canReadLedger); err != nil || canReadLedger {
		t.Fatal("migration role received cross-schema access", err)
	}
}
