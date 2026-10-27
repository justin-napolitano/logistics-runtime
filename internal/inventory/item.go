// Package inventory defines inventory comparisons independently from transport and storage.
package inventory

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
)

var (
	ErrInvalid  = errors.New("invalid inventory input")
	ErrNotFound = errors.New("item not found")
	ErrConflict = errors.New("item exists or revision changed")
	idPattern   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,119}$`)
)

// Input replaces the editable state of an item. Nil quantities mean unknown/unset.
// Quantities are whole counts in one explicitly confirmed unit, not weights or currency.
type Input struct {
	Name     string `json:"name"`
	Category string `json:"category"`
	Unit     string `json:"unit"`
	Desired  *int64 `json:"desired"`
	OnHand   *int64 `json:"on_hand"`
	Incoming *int64 `json:"incoming"`
	Notes    string `json:"notes"`
}

// Item preserves historical proposals separately from owner-confirmed quantities.
type Item struct {
	ID string `json:"id"`
	Input
	ProposedDesired *int64     `json:"proposed_desired"`
	ProposedUnit    string     `json:"proposed_unit"`
	Source          string     `json:"source"`
	Revision        int64      `json:"revision"`
	UpdatedAt       time.Time  `json:"updated_at"`
	CountedAt       *time.Time `json:"counted_at"`
}

// View contains derived facts only; gaps never cause tasks or purchases.
type View struct {
	Item
	Gap    *int64 `json:"gap"`
	Status string `json:"status"`
}

func ValidID(id string) bool { return idPattern.MatchString(id) }

func (in *Input) Validate() error {
	in.Name = strings.TrimSpace(in.Name)
	in.Category = strings.TrimSpace(in.Category)
	in.Unit = strings.TrimSpace(in.Unit)
	if in.Name == "" || len(in.Name) > 200 || len(in.Category) > 200 || len(in.Unit) > 60 || len(in.Notes) > 4000 {
		return ErrInvalid
	}
	for _, q := range []*int64{in.Desired, in.OnHand, in.Incoming} {
		if q != nil && (*q < 0 || *q > 1000000000 || in.Unit == "") {
			return ErrInvalid
		}
	}
	return nil
}

// Compare requires a target and both stock and incoming evidence before computing a gap.
func Compare(item Item) View {
	v := View{Item: item}
	switch {
	case item.Desired == nil:
		v.Status = "desired_unset"
	case item.OnHand == nil:
		v.Status = "actual_unknown"
	case item.Incoming == nil:
		v.Status = "incoming_unknown"
	default:
		gap := max(int64(0), *item.Desired-*item.OnHand-*item.Incoming)
		v.Gap = &gap
		v.Status = "sufficient"
		if gap > 0 {
			v.Status = "shortage"
		}
	}
	return v
}

// Filter supports bounded lists. View is all, unknown or shortages.
type Filter struct {
	Category, View string
	Limit, Offset  int
}

// Store is shared by the HTTP API and local catalog command.
type Store interface {
	List(ctx context.Context, filter Filter) ([]Item, error)
	Get(ctx context.Context, id string) (Item, error)
	Create(ctx context.Context, id string, input Input) (Item, error)
	Replace(ctx context.Context, id string, revision int64, input Input) (Item, error)
	Ready(ctx context.Context) error
}
