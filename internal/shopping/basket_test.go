package shopping

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/justin-napolitano/logistics-runtime/internal/inventory"
)

type fakeInventory struct{ item inventory.Item }

func (f fakeInventory) Get(context.Context, string) (inventory.Item, error) { return f.item, nil }
func pointer(v int64) *int64                                                { return &v }
func sample() Template {
	return Template{Name: "Restock", Lines: []Line{{ID: "coffee", Name: "Cold brew", Merchant: "amazon_fresh", ProductURL: "https://www.amazon.com/dp/B01EN6KSCQ", Package: "48 fl oz bottle", Basis: "planned_packages", PlannedPackages: 2}}}
}
func TestProposalIdentityAndStockBoundaries(t *testing.T) {
	in := sample()
	in.Lines = append(in.Lines, Line{ID: "towels", Name: "Towels", Merchant: "walmart", ProductURL: "https://www.walmart.com/ip/123", Package: "each", Basis: "inventory_gap", ItemID: "towels", InventoryUnit: "each"})
	tmpl, err := Clean(in)
	if err != nil {
		t.Fatal(err)
	}
	reverse := Template{Name: in.Name, Lines: []Line{in.Lines[1], in.Lines[0]}}
	again, _ := Clean(reverse)
	if tmpl.ID != again.ID {
		t.Fatal("order-dependent template identity")
	}
	item := inventory.Item{Revision: 2, Input: inventory.Input{Name: "Towels", Unit: "each", Desired: pointer(4), OnHand: nil, Incoming: pointer(1)}}
	b, err := Prepare(context.Background(), tmpl, "2026-11-first", fakeInventory{item}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if *b.Lines[0].Packages != 2 || b.Lines[0].Status != "planned_quantity_stock_not_deducted" || b.Lines[1].Packages != nil || b.Lines[1].Status != "actual_unknown" {
		t.Fatalf("unknown stock changed: %+v", b)
	}
	item.OnHand = pointer(0)
	known, err := Prepare(context.Background(), tmpl, "2026-11-first", fakeInventory{item}, time.Now())
	if err != nil || *known.Lines[1].Packages != 3 {
		t.Fatalf("gap %v %v", known, err)
	}
	if b.ID != known.ID {
		t.Fatal("cycle identity changed with observation")
	}
	item.Unit = "pack"
	if _, err = Prepare(context.Background(), tmpl, "cycle", fakeInventory{item}, time.Now()); !errors.Is(err, ErrInvalid) {
		t.Fatal("silently relabeled inventory unit")
	}
	// Canonicalization must not mutate a caller's shared template slice.
	if in.Lines[0].ID != "coffee" {
		t.Fatal("caller input mutated")
	}
}
func TestLinkAndHTMLBoundaries(t *testing.T) {
	for _, u := range []string{"javascript:alert(1)", "https://evil.example/p", "https://amazon.com.evil.example/p", "https://user:secret@www.amazon.com/p", "http://www.amazon.com/p", "https://www.amazon.com:443/p"} {
		in := sample()
		in.Lines[0].ProductURL = u
		if _, err := Clean(in); err == nil {
			t.Errorf("accepted %s", u)
		}
	}
	in := sample()
	in.Name = "<script>alert(1)</script>"
	in.Lines[0].Name = "<img src=x onerror=alert(1)>"
	tmpl, _ := Clean(in)
	b, err := Prepare(context.Background(), tmpl, "review", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = Render(&out, b); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "<script>") || strings.Contains(out.String(), "<img src=x") {
		t.Fatal("unsafe review HTML")
	}
	if !strings.Contains(out.String(), "not an Amazon/Walmart cart") {
		t.Fatal("proposal label missing")
	}
}

func TestUnmatchedProductsRemainVisibleAndDuplicatesAreRejected(t *testing.T) {
	in := sample()
	in.Lines[0].ProductURL = ""
	in.Lines[0].ReviewNote = "Find the exact product before buying"
	tmpl, err := Clean(in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Prepare(context.Background(), tmpl, "review", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = Render(&out, b); err != nil || !strings.Contains(out.String(), "product link needed") {
		t.Fatal("unmatched line disappeared")
	}
	in = sample()
	line := in.Lines[0]
	line.ID = "duplicate"
	in.Lines = append(in.Lines, line)
	if _, err = Clean(in); err == nil {
		t.Fatal("same product duplicated under different line IDs")
	}
}
