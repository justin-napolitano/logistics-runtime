package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/justin-napolitano/logistics-runtime/internal/inventory"
	"github.com/justin-napolitano/logistics-runtime/internal/mailsync"
)

func (s *Store) ConfigureMail(ctx context.Context, c mailsync.Connection) error {
	if e := c.Validate(); e != nil {
		return e
	}
	c.Since = c.Since.UTC()
	b, _ := json.Marshal(c)
	var existing []byte
	e := s.pool.QueryRow(ctx, `INSERT INTO logistics.mail_connections(id,config) VALUES($1,$2) ON CONFLICT(id) DO UPDATE SET id=excluded.id RETURNING config`, c.ID, b).Scan(&existing)
	if e != nil {
		return e
	}
	var old mailsync.Connection
	if json.Unmarshal(existing, &old) != nil {
		return mailsync.ErrInvalid
	}
	if old != c {
		return inventory.ErrConflict
	}
	return nil
}
func (s *Store) MailConnections(ctx context.Context) ([]mailsync.Connection, error) {
	rows, e := s.pool.Query(ctx, `SELECT config FROM logistics.mail_connections ORDER BY id LIMIT 101`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []mailsync.Connection{}
	for rows.Next() {
		var b []byte
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		var c mailsync.Connection
		if json.Unmarshal(b, &c) != nil {
			return nil, mailsync.ErrInvalid
		}
		out = append(out, c)
	}
	if len(out) > 100 {
		return nil, mailsync.ErrInvalid
	}
	return out, rows.Err()
}
func (s *Store) MailStatuses(ctx context.Context) ([]mailsync.Status, error) {
	rows, e := s.pool.Query(ctx, `SELECT id,config->>'provider',progress FROM logistics.mail_connections ORDER BY id LIMIT 101`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []mailsync.Status{}
	for rows.Next() {
		var id, provider string
		var b []byte
		if e = rows.Scan(&id, &provider, &b); e != nil {
			return nil, e
		}
		var p mailsync.Progress
		if json.Unmarshal(b, &p) != nil {
			return nil, mailsync.ErrInvalid
		}
		out = append(out, mailsync.Status{ID: id, Provider: provider, State: p.Status, ErrorCode: p.ErrorCode, LastSuccess: p.LastSuccess, LastAttempt: p.LastAttempt, Imported: p.Imported, Skipped: p.Skipped, RecoveryCount: p.RecoveryCount})
	}
	return out, rows.Err()
}

type mailLease struct {
	conn *pgxpool.Conn
	c    mailsync.Connection
	p    mailsync.Progress
	key  string
}

func (l *mailLease) Connection() mailsync.Connection { return l.c }
func (l *mailLease) Progress() mailsync.Progress     { return l.p }
func (l *mailLease) Save(ctx context.Context, p mailsync.Progress) error {
	b, e := json.Marshal(p)
	if e != nil {
		return e
	}
	_, e = l.conn.Exec(ctx, `UPDATE logistics.mail_connections SET progress=$2,updated_at=now() WHERE id=$1`, l.c.ID, b)
	if e == nil {
		l.p = p
	}
	return e
}
func (l *mailLease) Close() {
	if l.conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var unlocked bool
	e := l.conn.QueryRow(ctx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, l.key).Scan(&unlocked)
	if e != nil || !unlocked {
		raw := l.conn.Hijack()
		_ = raw.Close(ctx)
	} else {
		l.conn.Release()
	}
	l.conn = nil
}
func (s *Store) AcquireMail(ctx context.Context, id string) (mailsync.Lease, error) {
	if !inventory.ValidID(id) {
		return nil, mailsync.ErrInvalid
	}
	conn, e := s.pool.Acquire(ctx)
	if e != nil {
		return nil, e
	}
	key := "logistics.mail.sync:" + id
	var locked bool
	e = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, key).Scan(&locked)
	if e != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		raw := conn.Hijack()
		_ = raw.Close(closeCtx)
		return nil, e
	}
	if !locked {
		conn.Release()
		return nil, mailsync.ErrBusy
	}
	l := &mailLease{conn: conn, key: key}
	var config, progress []byte
	e = conn.QueryRow(ctx, `SELECT config,progress FROM logistics.mail_connections WHERE id=$1`, id).Scan(&config, &progress)
	if e != nil {
		l.Close()
		if errors.Is(e, pgx.ErrNoRows) {
			return nil, inventory.ErrNotFound
		}
		return nil, e
	}
	if json.Unmarshal(config, &l.c) != nil || json.Unmarshal(progress, &l.p) != nil || l.c.Validate() != nil {
		l.Close()
		return nil, mailsync.ErrInvalid
	}
	return l, nil
}

var _ mailsync.Store = (*Store)(nil)
