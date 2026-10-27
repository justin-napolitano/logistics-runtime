// Package shopping prepares reviewable baskets. It never contacts a merchant.
package shopping

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/justin-napolitano/logistics-runtime/internal/inventory"
)

var ErrInvalid = errors.New("invalid shopping input")

// Template quantities are either a fixed planning scenario or a known inventory gap.
// InventoryGap requires a one-to-one match between the inventory unit and package unit.
type Template struct {
	Name  string `json:"name"`
	Lines []Line `json:"lines"`
}
type Line struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Merchant        string `json:"merchant"`
	ProductURL      string `json:"product_url"`
	Package         string `json:"package"`
	ReviewNote      string `json:"review_note"`
	Basis           string `json:"basis"` // planned_packages or inventory_gap
	PlannedPackages int64  `json:"planned_packages"`
	ItemID          string `json:"item_id"`
	InventoryUnit   string `json:"inventory_unit"`
}
type SavedTemplate struct {
	ID string `json:"id"`
	Template
}
type BasketLine struct {
	Line
	Packages          *int64 `json:"packages"`
	Status            string `json:"status"`
	InventoryRevision *int64 `json:"inventory_revision"`
}
type Basket struct {
	ID         string       `json:"id"`
	TemplateID string       `json:"template_id"`
	Cycle      string       `json:"cycle"`
	Name       string       `json:"name"`
	Lines      []BasketLine `json:"lines"`
	CreatedAt  time.Time    `json:"created_at"`
	Kind       string       `json:"kind"`
}
type Inventory interface {
	Get(context.Context, string) (inventory.Item, error)
}
type Store interface {
	SaveTemplate(context.Context, SavedTemplate) error
	Template(context.Context, string) (SavedTemplate, error)
	SaveBasket(context.Context, Basket) (Basket, error)
	Basket(context.Context, string) (Basket, error)
}

func Digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func Clean(t Template) (SavedTemplate, error) {
	t.Name = strings.TrimSpace(t.Name)
	if len(t.Name) == 0 || len(t.Name) > 200 || len(t.Lines) == 0 || len(t.Lines) > 100 {
		return SavedTemplate{}, ErrInvalid
	}
	t.Lines = append([]Line(nil), t.Lines...)
	seen := map[string]bool{}
	products := map[string]bool{}
	inventoryItems := map[string]bool{}
	for i := range t.Lines {
		l := &t.Lines[i]
		l.Name = strings.TrimSpace(l.Name)
		if !inventory.ValidID(l.ID) || seen[l.ID] || len(l.Name) == 0 || len(l.Name) > 300 || len(l.Package) == 0 || len(l.Package) > 160 || len(l.ProductURL) > 2048 {
			return SavedTemplate{}, ErrInvalid
		}
		seen[l.ID] = true
		hosts := map[string]string{"amazon_fresh": "amazon.com", "walmart": "walmart.com"}
		host, ok := hosts[l.Merchant]
		if !ok || len(l.ReviewNote) > 500 {
			return SavedTemplate{}, ErrInvalid
		}
		for _, value := range []string{l.Name, l.Package, l.ProductURL, l.ReviewNote, l.InventoryUnit, t.Name} {
			if strings.ContainsRune(value, 0) {
				return SavedTemplate{}, ErrInvalid
			}
		}
		if l.ProductURL != "" {
			key := l.Merchant + "\x00" + l.ProductURL
			if products[key] {
				return SavedTemplate{}, ErrInvalid
			}
			products[key] = true
			u, err := url.Parse(l.ProductURL)
			if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Port() != "" || (u.Hostname() != host && u.Hostname() != "www."+host) {
				return SavedTemplate{}, ErrInvalid
			}
		}
		switch l.Basis {
		case "planned_packages":
			if l.PlannedPackages < 1 || l.PlannedPackages > 1000 || l.ItemID != "" || l.InventoryUnit != "" {
				return SavedTemplate{}, ErrInvalid
			}
		case "inventory_gap":
			if inventoryItems[l.ItemID] {
				return SavedTemplate{}, ErrInvalid
			}
			inventoryItems[l.ItemID] = true
			if !inventory.ValidID(l.ItemID) || len(l.InventoryUnit) == 0 || len(l.InventoryUnit) > 60 || l.PlannedPackages != 0 {
				return SavedTemplate{}, ErrInvalid
			}
		default:
			return SavedTemplate{}, ErrInvalid
		}
	}
	sort.Slice(t.Lines, func(i, j int) bool { return t.Lines[i].ID < t.Lines[j].ID })
	b, _ := json.Marshal(t)
	return SavedTemplate{ID: Digest(b), Template: t}, nil
}
func Prepare(ctx context.Context, t SavedTemplate, cycle string, inv Inventory, now time.Time) (Basket, error) {
	canonical, err := Clean(t.Template)
	if err != nil || canonical.ID != t.ID || !inventory.ValidID(cycle) {
		return Basket{}, ErrInvalid
	}
	b := Basket{ID: Digest([]byte(t.ID + "\x00" + cycle)), TemplateID: t.ID, Cycle: cycle, Name: t.Name, CreatedAt: now.UTC(), Kind: "proposal_not_merchant_cart", Lines: make([]BasketLine, 0, len(t.Lines))}
	for _, l := range canonical.Lines {
		out := BasketLine{Line: l, Status: "planned_quantity_stock_not_deducted"}
		if l.Basis == "planned_packages" {
			q := l.PlannedPackages
			out.Packages = &q
		} else {
			if inv == nil {
				return Basket{}, ErrInvalid
			}
			item, e := inv.Get(ctx, l.ItemID)
			if e != nil {
				return Basket{}, e
			}
			out.InventoryRevision = &item.Revision
			if item.Unit != l.InventoryUnit {
				return Basket{}, ErrInvalid
			}
			view := inventory.Compare(item)
			out.Packages = view.Gap
			out.Status = view.Status
		}
		b.Lines = append(b.Lines, out)
	}
	return b, nil
}

// Render exports a standalone review file; links navigate to products, never submit orders.
func Render(w io.Writer, b Basket) error { return review.Execute(w, b) }

var review = template.Must(template.New("basket").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="referrer" content="no-referrer"><title>{{.Name}}</title><style>body{font:17px system-ui;max-width:900px;margin:40px auto;padding:20px;color:#172019;background:#fffdf8}table{border-collapse:collapse;width:100%}td,th{text-align:left;padding:12px;border-bottom:1px solid #ddd}a{color:#a61930}</style><h1>{{.Name}}</h1><p>Cycle: {{.Cycle}} · Saved {{.CreatedAt.Format "2006-01-02"}}</p><p><strong>Shopping proposal, not an Amazon/Walmart cart or an order.</strong> Prices, availability, delivery charges and substitutions require merchant review. Planned quantities do not assume zero stock. Inventory-gap lines require matching counting units; open requests and pending purchasing attempts are not yet integrated.</p><table><thead><tr><th>Product</th><th>Package</th><th>Packages</th><th>Basis / status</th></tr></thead><tbody>{{range .Lines}}<tr><td>{{if .ProductURL}}<a href="{{.ProductURL}}" rel="noreferrer noopener">{{.Name}}</a>{{else}}{{.Name}} — product link needed{{end}}{{if .ReviewNote}}<br><small>{{.ReviewNote}}</small>{{end}}</td><td>{{.Package}}</td><td>{{if .Packages}}{{.Packages}}{{else}}Unknown{{end}}</td><td>{{.Status}}</td></tr>{{end}}</tbody></table><p>Basket {{.ID}}. This file contains private shopping preferences; share deliberately.</p></html>`))
