package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	"github.com/justin-napolitano/logistics-runtime/internal/ordermail"
	"github.com/justin-napolitano/logistics-runtime/internal/postgres"
	"github.com/justin-napolitano/logistics-runtime/internal/shopping"
)

func TestShoppingMailPersistence(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to dedicated logistics_test")
	}
	u, e := url.Parse(dsn)
	if e != nil || u.Path != "/logistics_test" {
		t.Fatal("dedicated logistics_test required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, e := postgres.Open(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	if e = s.ShoppingReady(ctx); e != nil {
		t.Fatal(e)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	account := "synthetic." + suffix
	item, e := s.Create(ctx, account, inventory.Input{Name: "Synthetic packages", Unit: "package", Desired: ptr(4), Incoming: ptr(0)})
	if e != nil {
		t.Fatal(e)
	}
	tmpl, e := shopping.Clean(shopping.Template{Name: "Synthetic review " + suffix, Lines: []shopping.Line{{ID: "synthetic", Name: "Synthetic product", Merchant: "amazon_fresh", ProductURL: "https://www.amazon.com/dp/B000000000", Package: "one package", Basis: "inventory_gap", ItemID: item.ID, InventoryUnit: "package"}}})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.SaveTemplate(ctx, tmpl); e != nil {
		t.Fatal(e)
	}
	if e = s.SaveTemplate(ctx, tmpl); e != nil {
		t.Fatal(e)
	}
	token := strings.Repeat("t", 32)
	handler, e := httpapi.New(s, token, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if e != nil {
		t.Fatal(e)
	}
	request := func(method, path, body, ctype string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", ctype)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	prepare := func(cycle string) shopping.Basket {
		t.Helper()
		w := request("POST", "/v1/shopping/baskets", fmt.Sprintf(`{"template_id":%q,"cycle":%q}`, tmpl.ID, cycle), "application/json")
		var b shopping.Basket
		if w.Code != 200 {
			t.Fatalf("prepare %d %s", w.Code, w.Body.String())
		}
		if e := json.Unmarshal(w.Body.Bytes(), &b); e != nil {
			t.Fatal(e)
		}
		return b
	}
	b := prepare("first")
	if b.Lines[0].Packages != nil {
		t.Fatal("unknown stock became a zero-stock order")
	}
	in := item.Input
	in.OnHand = ptr(0)
	updated, e := s.Replace(ctx, item.ID, item.Revision, in)
	if e != nil {
		t.Fatal(e)
	}
	replay := prepare("first")
	if replay.ID != b.ID || !replay.CreatedAt.Equal(b.CreatedAt) || replay.Lines[0].Packages != nil {
		t.Fatal("saved basket mutated on retry")
	}
	next := prepare("next")
	if *next.Lines[0].Packages != 4 {
		t.Fatal("new cycle did not capture known gap")
	}
	// Two concurrent saves of one cycle must return the same durable snapshot.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.SaveBasket(ctx, next)
			if err == nil && got.ID != next.ID {
				err = errors.New("different basket")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	w := request("GET", "/v1/shopping/baskets/"+b.ID+"/review", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Unknown") || w.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("review export failed")
	}
	noauth := httptest.NewRecorder()
	handler.ServeHTTP(noauth, httptest.NewRequest("GET", "/v1/shopping/baskets/"+b.ID+"/review", nil))
	if noauth.Code != 401 {
		t.Fatal("public basket leak")
	}
	raw := func(id, subject, date, body string) string {
		return fmt.Sprintf("From: auto-confirm@amazon.com\r\nMessage-ID: <%s@example.invalid>\r\nDate: %s\r\nSubject: %s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nOrder 111-1234567-1234567\n%s", id, date, subject, body)
	}
	confirmation := raw("confirmation", "Your Amazon Fresh order", "Mon, 26 Oct 2026 10:00:00 +0000", "Order total: $25.42")
	m, e := ordermail.Parse(strings.NewReader(confirmation))
	if e != nil {
		t.Fatal(e)
	}
	results := make(chan ordermail.ImportResult, 8)
	errorsCh := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); r, e := s.ImportMail(ctx, account, m); results <- r; errorsCh <- e }()
	}
	wg.Wait()
	close(results)
	close(errorsCh)
	for e := range errorsCh {
		if e != nil {
			t.Fatal(e)
		}
	}
	duplicates := 0
	orderID := ""
	for r := range results {
		if r.Duplicate {
			duplicates++
		}
		orderID = r.OrderID
	}
	if duplicates != 7 || orderID == "" {
		t.Fatalf("duplicate effects %d", duplicates)
	}
	// Identity reuse with changed content must not overwrite existing evidence.
	altered, _ := ordermail.Parse(strings.NewReader(strings.Replace(confirmation, "$25.42", "$99.99", 1)))
	if _, e = s.ImportMail(ctx, account, altered); !errors.Is(e, inventory.ErrConflict) {
		t.Fatal("conflicting message identity accepted")
	}
	other, e := s.ImportMail(ctx, "other."+account, m)
	if e != nil || other.OrderID == orderID {
		t.Fatal("cross-account orders merged")
	}
	for _, mail := range []string{raw("delivery", "Delivered", "Wed, 28 Oct 2026 10:00:00 +0000", "Tracking number: TBA123456789012"), raw("shipment", "Shipped", "Tue, 27 Oct 2026 10:00:00 +0000", "Tracking number: TBA123456789012"), raw("refund", "Refund issued", "Thu, 29 Oct 2026 10:00:00 +0000", "")} {
		w = request("POST", "/v1/mail/import?account="+account, mail, "message/rfc822")
		if w.Code != 200 {
			t.Fatalf("mail API %d %s", w.Code, w.Body.String())
		}
	}
	order, e := s.Order(ctx, orderID)
	if e != nil || len(order.Reports) != 4 {
		t.Fatalf("timeline %+v %v", order, e)
	}
	if order.Reports[1].Kind != "shipped_reported" || order.Reports[2].Kind != "delivered_reported" || order.Reports[3].Kind != "refund_reported" {
		t.Fatal("arrival order overwrote event chronology")
	}
	unknown := raw("unknown", "New unsupported template", "Thu, 29 Oct 2026 10:00:00 +0000", "")
	w = request("POST", "/v1/mail/import?account="+account, unknown, "message/rfc822")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"exception"`) {
		t.Fatal("unsupported message not quarantined")
	}
	exceptions, e := s.MailExceptions(ctx, 200, 0)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, v := range exceptions {
		if v.Account == account {
			found = true
		}
	}
	if !found {
		t.Fatal("missing exception")
	}
	after, e := s.Get(ctx, item.ID)
	if e != nil || after.Revision != updated.Revision || *after.OnHand != 0 || *after.Incoming != 0 {
		t.Fatal("mail changed physical inventory")
	}
	w = request("GET", "/v1/orders/"+orderID, "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "unverified_email_report") {
		t.Fatal("order report API failed")
	}
	// No raw headers/body are persisted in the evidence table.
	conn, e := pgx.Connect(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close(ctx)
	var evidence string
	e = conn.QueryRow(ctx, `SELECT row_to_json(m)::text FROM logistics.order_mail m WHERE id=$1`, shopping.Digest([]byte(account+"\x00"+m.MessageKey))).Scan(&evidence)
	if e != nil || strings.Contains(evidence, "Order total") || strings.Contains(evidence, "auto-confirm") {
		t.Fatal("raw mail persisted")
	}
}

func TestShoppingRuntimePrivileges(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("dedicated test DB required")
	}
	u, e := url.Parse(dsn)
	if e != nil || u.Path != "/logistics_test" {
		t.Fatal("dedicated test DB required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, e := postgres.Open(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	conn, e := pgx.Connect(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close(ctx)
	name := fmt.Sprintf("shopping_runtime_%d", time.Now().UnixNano())
	role := pgx.Identifier{name}.Sanitize()
	if _, e = conn.Exec(ctx, `CREATE ROLE `+role+` NOLOGIN`); e != nil {
		t.Fatal(e)
	}
	defer func() {
		_, _ = conn.Exec(ctx, `DROP OWNED BY `+role)
		if _, err := conn.Exec(ctx, `DROP ROLE `+role); err != nil {
			t.Error(err)
		}
	}()
	_, e = conn.Exec(ctx, `GRANT USAGE ON SCHEMA logistics TO `+role+`; GRANT SELECT,INSERT ON logistics.shopping_templates,logistics.baskets,logistics.orders,logistics.order_reports TO `+role+`; GRANT SELECT,INSERT,UPDATE ON logistics.order_mail TO `+role)
	if e != nil {
		t.Fatal(e)
	}
	tx, e := conn.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `SET LOCAL ROLE `+role); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, `SELECT id FROM logistics.order_mail LIMIT 1 FOR UPDATE`); e != nil {
		t.Fatal("runtime cannot lock mail evidence", e)
	}
	var canDDL, canDelete, canRewriteTemplate bool
	if e = tx.QueryRow(ctx, `SELECT has_schema_privilege(current_user,'logistics','CREATE'),has_table_privilege(current_user,'logistics.order_mail','DELETE'),has_table_privilege(current_user,'logistics.shopping_templates','UPDATE')`).Scan(&canDDL, &canDelete, &canRewriteTemplate); e != nil {
		t.Fatal(e)
	}
	if canDDL || canDelete || canRewriteTemplate {
		t.Fatal("excess runtime privilege")
	}
}
