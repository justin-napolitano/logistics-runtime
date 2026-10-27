package mailsync

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/justin-napolitano/logistics-runtime/internal/ordermail"
)

type Runner struct {
	Store    Store
	MaxPages int
	Now      func() time.Time
}

// Run checkpoints only after every message on a page is durably handled. Replays
// are safe because import has its own transactional deduplication.
func (r Runner) Run(ctx context.Context, id string, build func(Connection) (Provider, error)) (out Status, err error) {
	lease, err := r.Store.AcquireMail(ctx, id)
	if err != nil {
		return out, err
	}
	defer lease.Close()
	c, p := lease.Connection(), lease.Progress()
	if p.Status == "paused" {
		return Status{ID: c.ID, Provider: c.Provider, State: "paused", LastSuccess: p.LastSuccess, LastAttempt: p.LastAttempt}, nil
	}
	if p.Status == "reconnect_required" {
		return Status{ID: c.ID, Provider: c.Provider, State: p.Status, ErrorCode: p.ErrorCode, LastSuccess: p.LastSuccess, LastAttempt: p.LastAttempt}, ErrReconnect
	}
	now := r.Now
	if now == nil {
		now = time.Now
	}
	attempt := now().UTC()
	p.LastAttempt = &attempt
	p.Imported = 0
	p.Skipped = 0
	summary := func() Status {
		return Status{c.ID, c.Provider, p.Status, p.ErrorCode, p.LastSuccess, p.LastAttempt, p.Imported, p.Skipped, p.RecoveryCount}
	}
	fail := func(e error) (Status, error) {
		p.Status = "error"
		p.ErrorCode = "sync_failed"
		switch {
		case errors.Is(e, ErrReconnect):
			p.Status = "reconnect_required"
			p.ErrorCode = "authorization_required"
		case errors.Is(e, ErrRateLimited):
			p.ErrorCode = "rate_limited"
		case errors.Is(e, ErrCredential):
			p.ErrorCode = "credential_store_unavailable"
		case errors.Is(e, ErrInvalid):
			p.ErrorCode = "invalid_configuration"
		case errors.Is(e, ErrWindowTooBroad):
			p.ErrorCode = "narrower_backfill_window_required"
		case errors.Is(e, ErrTooLarge):
			p.ErrorCode = "message_too_large"
		}
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if saveErr := lease.Save(saveCtx, p); saveErr != nil {
			return summary(), saveErr
		}
		return summary(), e
	}
	p.Status = "running"
	p.ErrorCode = ""
	if err = lease.Save(ctx, p); err != nil {
		return summary(), err
	}
	provider, err := build(c)
	if err != nil {
		return fail(err)
	}
	identity, err := provider.Identity(ctx)
	if err != nil {
		return fail(err)
	}
	if identity == "" {
		return fail(ErrProvider)
	}
	hash := IdentityHash(c.Provider, identity)
	if p.IdentityHash != "" && p.IdentityHash != hash {
		return fail(ErrReconnect)
	}
	p.IdentityHash = hash
	if err = lease.Save(ctx, p); err != nil {
		return summary(), err
	}
	maxPages := r.MaxPages
	if maxPages <= 0 {
		maxPages = 20
	}
	if maxPages > 100 {
		return fail(ErrInvalid)
	}
	recovered := false
	for pageIndex := 0; pageIndex < maxPages; pageIndex++ {
		if err = ctx.Err(); err != nil {
			return fail(err)
		}
		page, e := provider.Page(ctx, c, p.Cursor)
		if errors.Is(e, ErrCursorExpired) && !recovered {
			recovered = true
			p.Cursor = Cursor{}
			p.RecoveryCount++
			if err = lease.Save(ctx, p); err != nil {
				return summary(), err
			}
			continue
		}
		if e != nil {
			return fail(e)
		}
		if len(page.IDs) > 500 {
			return fail(ErrProvider)
		}
		for _, messageID := range page.IDs {
			raw, e := provider.Message(ctx, c, messageID)
			if errors.Is(e, ErrGone) {
				p.Skipped++
				continue
			}
			if errors.Is(e, ErrTooLarge) {
				marker := IdentityHash(c.Provider, messageID)
				evidence := ordermail.Message{MessageKey: "provider-limit:" + marker, Digest: marker, ParserVersion: ordermail.ParserVersion, Status: "exception", Reason: "mailbox_message_too_large"}
				if _, saveErr := r.Store.ImportMail(ctx, c.Account, evidence); saveErr != nil {
					return fail(saveErr)
				}
				p.Imported++
				continue
			}
			if e != nil {
				return fail(e)
			}
			if raw == nil {
				p.Skipped++
				continue
			}
			message, e := ordermail.Parse(bytes.NewReader(raw))
			if e != nil {
				return fail(ErrTooLarge)
			}
			if _, e = r.Store.ImportMail(ctx, c.Account, message); e != nil {
				return fail(e)
			}
			p.Imported++
		}
		p.Cursor = page.Next
		if page.Complete {
			success := now().UTC()
			p.LastSuccess = &success
			p.Status = "ready"
			p.ErrorCode = ""
			if err = lease.Save(ctx, p); err != nil {
				return summary(), err
			}
			return summary(), nil
		}
		if err = lease.Save(ctx, p); err != nil {
			return summary(), err
		}
	}
	p.Status = "pending"
	p.ErrorCode = ""
	if err = lease.Save(ctx, p); err != nil {
		return summary(), err
	}
	return summary(), nil
}
