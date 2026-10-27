// Package postgres implements inventory persistence in the application-owned logistics schema.
package postgres

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/justin-napolitano/logistics-runtime/internal/inventory"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct{ pool *pgxpool.Pool }

func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, errors.New("invalid database configuration")
	}
	cfg.MaxConns = 5
	cfg.MinConns = 0
	cfg.ConnConfig.RuntimeParams["application_name"] = "logistics"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, errors.New("database connection failed")
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, errors.New("database unavailable")
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

// Migrate explicitly applies forward-only, checksummed migrations under a transaction lock.
// Run with a schema-owning identity; serving never runs DDL.
func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(681334218)`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS logistics;
	 CREATE TABLE IF NOT EXISTS logistics.schema_migrations(name text PRIMARY KEY, checksum text NOT NULL)`); err != nil {
		return err
	}
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	for _, file := range files {
		body, err := migrations.ReadFile(file)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(body)
		checksum := hex.EncodeToString(hash[:])
		var previous string
		err = tx.QueryRow(ctx, `SELECT checksum FROM logistics.schema_migrations WHERE name=$1`, file).Scan(&previous)
		if err == nil {
			if previous != checksum {
				return fmt.Errorf("migration checksum changed: %s", file)
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, err = tx.Exec(ctx, string(body)); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO logistics.schema_migrations VALUES($1,$2)`, file, checksum); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) Ready(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `SELECT id FROM logistics.items LIMIT 0`)
	return err
}

const columns = `id,name,category,unit,desired,on_hand,incoming,notes,proposed_desired,proposed_unit,source,revision,updated_at,counted_at`

func scan(row pgx.Row) (inventory.Item, error) {
	var item inventory.Item
	err := row.Scan(&item.ID, &item.Name, &item.Category, &item.Unit, &item.Desired, &item.OnHand, &item.Incoming, &item.Notes, &item.ProposedDesired, &item.ProposedUnit, &item.Source, &item.Revision, &item.UpdatedAt, &item.CountedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return item, inventory.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return item, inventory.ErrConflict
		case "23514", "22001", "22003":
			return item, inventory.ErrInvalid
		}
	}
	return item, err
}

func (s *Store) Get(ctx context.Context, id string) (inventory.Item, error) {
	return scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM logistics.items WHERE id=$1`, id))
}

func (s *Store) List(ctx context.Context, f inventory.Filter) ([]inventory.Item, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+columns+` FROM logistics.items
	 WHERE ($1='' OR category=$1)
	 AND ($2='all' OR ($2='unknown' AND (desired IS NULL OR on_hand IS NULL OR incoming IS NULL))
	 OR ($2='shortages' AND desired-on_hand-incoming>0)) ORDER BY id LIMIT $3 OFFSET $4`, f.Category, f.View, f.Limit, f.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]inventory.Item, 0)
	for rows.Next() {
		item, err := scan(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) Create(ctx context.Context, id string, in inventory.Input) (inventory.Item, error) {
	if !inventory.ValidID(id) {
		return inventory.Item{}, inventory.ErrInvalid
	}
	if err := in.Validate(); err != nil {
		return inventory.Item{}, err
	}
	return scan(s.pool.QueryRow(ctx, `INSERT INTO logistics.items(id,name,category,unit,desired,on_hand,incoming,notes,counted_at)
	 VALUES($1,$2,$3,$4,$5,$6,$7,$8,CASE WHEN $6::bigint IS NULL THEN NULL ELSE now() END) RETURNING `+columns,
		id, in.Name, in.Category, in.Unit, in.Desired, in.OnHand, in.Incoming, in.Notes))
}

// Replace locks the item and checks its revision, preventing concurrent silent overwrites.
// A unit change requires existing quantities to be unset; conversions are never inferred.
func (s *Store) Replace(ctx context.Context, id string, revision int64, in inventory.Input) (inventory.Item, error) {
	if err := in.Validate(); err != nil {
		return inventory.Item{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return inventory.Item{}, err
	}
	defer tx.Rollback(ctx)
	old, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM logistics.items WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return inventory.Item{}, err
	}
	if old.Revision != revision {
		return inventory.Item{}, inventory.ErrConflict
	}
	if old.Unit != in.Unit && (old.Desired != nil || old.OnHand != nil || old.Incoming != nil) {
		return inventory.Item{}, inventory.ErrInvalid
	}
	item, err := scan(tx.QueryRow(ctx, `UPDATE logistics.items SET name=$2,category=$3,unit=$4,desired=$5,on_hand=$6,incoming=$7,notes=$8,
	 counted_at=CASE WHEN $6::bigint IS NULL THEN NULL WHEN on_hand IS DISTINCT FROM $6::bigint THEN now() ELSE counted_at END,
	 revision=revision+1,updated_at=now() WHERE id=$1 RETURNING `+columns, id, in.Name, in.Category, in.Unit, in.Desired, in.OnHand, in.Incoming, in.Notes))
	if err != nil {
		return inventory.Item{}, err
	}
	return item, tx.Commit(ctx)
}
