// Package mailsync coordinates read-only mailbox synchronization into order evidence.
package mailsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/justin-napolitano/logistics-runtime/internal/inventory"
	"github.com/justin-napolitano/logistics-runtime/internal/ordermail"
)

var (
	ErrInvalid        = errors.New("invalid mailbox configuration")
	ErrBusy           = errors.New("mailbox synchronization already running")
	ErrCursorExpired  = errors.New("mailbox cursor expired")
	ErrReconnect      = errors.New("mailbox authorization requires reconnect")
	ErrProvider       = errors.New("mailbox provider unavailable")
	ErrGone           = errors.New("message no longer available")
	ErrRateLimited    = errors.New("mailbox provider rate limited")
	ErrCredential     = errors.New("credential storage unavailable")
	ErrTooLarge       = errors.New("mailbox response exceeds limits")
	ErrWindowTooBroad = errors.New("mailbox backfill window exceeds provider limit")
)

type Connection struct {
	ID            string    `json:"id"`
	Provider      string    `json:"provider"`
	Account       string    `json:"account"`
	CredentialRef string    `json:"credential_ref"`
	Since         time.Time `json:"since"`
	Folder        string    `json:"folder"`
}

func (c Connection) Validate() error {
	if !inventory.ValidID(c.ID) || !inventory.ValidID(c.Account) || !secretName(c.CredentialRef) || c.Since.IsZero() || len(c.Folder) > 512 || strings.ContainsAny(c.Folder, "\x00\r\n") {
		return ErrInvalid
	}
	if c.Provider != "gmail" && c.Provider != "outlook" {
		return ErrInvalid
	}
	return nil
}
func secretName(s string) bool {
	if len(s) < 1 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}

type Cursor struct {
	Phase     string `json:"phase"`
	HistoryID string `json:"history_id,omitempty"`
	PageToken string `json:"page_token,omitempty"`
	DeltaURL  string `json:"delta_url,omitempty"`
	Seen      int    `json:"seen,omitempty"`
}
type Progress struct {
	Cursor        Cursor     `json:"cursor"`
	IdentityHash  string     `json:"identity_hash"`
	Status        string     `json:"status"`
	ErrorCode     string     `json:"error_code"`
	LastSuccess   *time.Time `json:"last_success"`
	LastAttempt   *time.Time `json:"last_attempt"`
	Imported      int        `json:"imported"`
	Skipped       int        `json:"skipped"`
	RecoveryCount int        `json:"recovery_count"`
}
type Status struct {
	ID            string     `json:"id"`
	Provider      string     `json:"provider"`
	State         string     `json:"state"`
	ErrorCode     string     `json:"error_code"`
	LastSuccess   *time.Time `json:"last_success"`
	LastAttempt   *time.Time `json:"last_attempt"`
	Imported      int        `json:"imported"`
	Skipped       int        `json:"skipped"`
	RecoveryCount int        `json:"recovery_count"`
}
type Lease interface {
	Connection() Connection
	Progress() Progress
	Save(context.Context, Progress) error
	Close()
}
type Store interface {
	ConfigureMail(context.Context, Connection) error
	MailConnections(context.Context) ([]Connection, error)
	MailStatuses(context.Context) ([]Status, error)
	AcquireMail(context.Context, string) (Lease, error)
	ImportMail(context.Context, string, ordermail.Message) (ordermail.ImportResult, error)
}
type Page struct {
	IDs      []string
	Next     Cursor
	Complete bool
}
type Provider interface {
	Identity(context.Context) (string, error)
	Page(context.Context, Connection, Cursor) (Page, error)
	Message(context.Context, Connection, string) ([]byte, error)
}

func IdentityHash(provider, identity string) string {
	h := sha256.Sum256([]byte(provider + "\x00" + identity))
	return hex.EncodeToString(h[:])
}
