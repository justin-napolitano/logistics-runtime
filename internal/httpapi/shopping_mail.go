package httpapi

import (
	"errors"
	"mime"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/justin-napolitano/logistics-runtime/internal/inventory"
	"github.com/justin-napolitano/logistics-runtime/internal/mailsync"
	"github.com/justin-napolitano/logistics-runtime/internal/ordermail"
	"github.com/justin-napolitano/logistics-runtime/internal/shopping"
)

type BasketRequest struct {
	TemplateID string `json:"template_id"`
	Cycle      string `json:"cycle"`
}
type OrderList struct {
	Orders []ordermail.Order `json:"orders"`
	Limit  int               `json:"limit"`
	Offset int               `json:"offset"`
}
type ExceptionList struct {
	Exceptions []ordermail.Evidence `json:"exceptions"`
	Limit      int                  `json:"limit"`
	Offset     int                  `json:"offset"`
}
type moduleOperation struct {
	Method, Path, Summary string
	Request, Response     any
	Handle                func(*API, http.ResponseWriter, *http.Request)
}

var shoppingOperations = []moduleOperation{
	{"GET", "/v1/mail/sync-status", "Read mailbox sync freshness and redacted status, without tokens/cursors", nil, []mailsync.Status{}, (*API).syncStatus},
	{"POST", "/v1/shopping/templates", "Save an immutable content-addressed shopping template", shopping.Template{}, shopping.SavedTemplate{}, (*API).saveTemplate},
	{"GET", "/v1/shopping/templates/{id}", "Read a shopping template", nil, shopping.SavedTemplate{}, (*API).getTemplate},
	{"POST", "/v1/shopping/baskets", "Prepare or return the saved proposal for a template/cycle", BasketRequest{}, shopping.Basket{}, (*API).prepareBasket},
	{"GET", "/v1/shopping/baskets/{id}", "Read a saved proposal, not a merchant cart", nil, shopping.Basket{}, (*API).getBasket},
	{"GET", "/v1/shopping/baskets/{id}/review", "Export authenticated standalone HTML with product links", nil, "", (*API).reviewBasket},
	{"POST", "/v1/mail/import", "Import one bounded .eml as unverified order evidence; account query required", nil, ordermail.ImportResult{}, (*API).importMail},
	{"GET", "/v1/mail/exceptions", "List unsupported mail evidence without raw bodies", nil, ExceptionList{}, (*API).mailExceptions},
	{"GET", "/v1/orders", "List orders inferred from imported messages, with partial coverage", nil, OrderList{}, (*API).orders},
	{"GET", "/v1/orders/{id}", "Read ordered email event reports; no physical stock effect", nil, ordermail.Order{}, (*API).order},
}

func (a *API) shoppingAvailable(w http.ResponseWriter) bool {
	if a.shopping == nil {
		write(w, 503, map[string]string{"error": "shopping storage unavailable"})
		return false
	}
	return true
}
func (a *API) mailAvailable(w http.ResponseWriter) bool {
	if a.mail == nil {
		write(w, 503, map[string]string{"error": "order mail storage unavailable"})
		return false
	}
	return true
}
func (a *API) moduleFail(w http.ResponseWriter, e error) {
	switch {
	case errors.Is(e, shopping.ErrInvalid), errors.Is(e, ordermail.ErrInvalid), errors.Is(e, inventory.ErrInvalid):
		write(w, 422, map[string]string{"error": "invalid shopping or mail input"})
	case errors.Is(e, inventory.ErrNotFound):
		write(w, 404, map[string]string{"error": "resource not found"})
	case errors.Is(e, inventory.ErrConflict):
		write(w, 409, map[string]string{"error": "resource identity conflict; reconcile before retrying"})
	default:
		a.fail(w, e)
	}
}
func (a *API) saveTemplate(w http.ResponseWriter, r *http.Request) {
	var in shopping.Template
	if !decodeLimit(w, r, &in, 128<<10) {
		return
	}
	t, err := shopping.Clean(in)
	if err != nil {
		a.moduleFail(w, err)
		return
	}
	if !a.shoppingAvailable(w) {
		return
	}
	if err = a.shopping.SaveTemplate(r.Context(), t); err != nil {
		a.moduleFail(w, err)
		return
	}
	w.Header().Set("Location", "/v1/shopping/templates/"+t.ID)
	write(w, 200, t)
}
func (a *API) getTemplate(w http.ResponseWriter, r *http.Request) {
	if !validID(w, r) || !a.shoppingAvailable(w) {
		return
	}
	t, err := a.shopping.Template(r.Context(), r.PathValue("id"))
	if err != nil {
		a.moduleFail(w, err)
		return
	}
	write(w, 200, t)
}
func (a *API) prepareBasket(w http.ResponseWriter, r *http.Request) {
	var in BasketRequest
	if !decode(w, r, &in) {
		return
	}
	if !inventory.ValidID(in.TemplateID) || !inventory.ValidID(in.Cycle) {
		a.moduleFail(w, shopping.ErrInvalid)
		return
	}
	if !a.shoppingAvailable(w) {
		return
	}
	id := shopping.Digest([]byte(in.TemplateID + "\x00" + in.Cycle))
	if b, err := a.shopping.Basket(r.Context(), id); err == nil {
		write(w, 200, b)
		return
	} else if !errors.Is(err, inventory.ErrNotFound) {
		a.moduleFail(w, err)
		return
	}
	t, err := a.shopping.Template(r.Context(), in.TemplateID)
	if err != nil {
		a.moduleFail(w, err)
		return
	}
	b, err := shopping.Prepare(r.Context(), t, in.Cycle, a.store, time.Now())
	if err != nil {
		a.moduleFail(w, err)
		return
	}
	b, err = a.shopping.SaveBasket(r.Context(), b)
	if err != nil {
		a.moduleFail(w, err)
		return
	}
	w.Header().Set("Location", "/v1/shopping/baskets/"+b.ID)
	write(w, 200, b)
}
func (a *API) getBasket(w http.ResponseWriter, r *http.Request) {
	if !validID(w, r) || !a.shoppingAvailable(w) {
		return
	}
	b, err := a.shopping.Basket(r.Context(), r.PathValue("id"))
	if err != nil {
		a.moduleFail(w, err)
		return
	}
	write(w, 200, b)
}
func (a *API) reviewBasket(w http.ResponseWriter, r *http.Request) {
	if !validID(w, r) || !a.shoppingAvailable(w) {
		return
	}
	b, err := a.shopping.Basket(r.Context(), r.PathValue("id"))
	if err != nil {
		a.moduleFail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	_ = shopping.Render(w, b)
}
func (a *API) importMail(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if len(q) != 1 || len(q["account"]) != 1 || !inventory.ValidID(q.Get("account")) {
		write(w, 400, map[string]string{"error": "one valid account query parameter required"})
		return
	}
	typ, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || typ != "message/rfc822" {
		write(w, 415, map[string]string{"error": "message/rfc822 required"})
		return
	}
	m, err := ordermail.Parse(http.MaxBytesReader(w, r.Body, ordermail.MaxMessageBytes))
	if err != nil {
		write(w, 400, map[string]string{"error": "invalid or oversized message"})
		return
	}
	if !a.mailAvailable(w) {
		return
	}
	out, err := a.mail.ImportMail(r.Context(), q.Get("account"), m)
	if err != nil {
		a.moduleFail(w, err)
		return
	}
	write(w, 200, out)
}
func page(w http.ResponseWriter, r *http.Request) (int, int, bool) {
	q := r.URL.Query()
	limit, offset := 100, 0
	for k, v := range q {
		if (k != "limit" && k != "offset") || len(v) != 1 {
			write(w, 400, map[string]string{"error": "invalid pagination"})
			return 0, 0, false
		}
	}
	for k, p := range map[string]*int{"limit": &limit, "offset": &offset} {
		if q.Has(k) {
			v, e := strconv.Atoi(q.Get(k))
			if e != nil {
				write(w, 400, map[string]string{"error": "invalid pagination"})
				return 0, 0, false
			}
			*p = v
		}
	}
	if limit < 1 || limit > 200 || offset < 0 || offset > 1000000 {
		write(w, 400, map[string]string{"error": "invalid pagination"})
		return 0, 0, false
	}
	return limit, offset, true
}
func (a *API) orders(w http.ResponseWriter, r *http.Request) {
	limit, offset, ok := page(w, r)
	if !ok || !a.mailAvailable(w) {
		return
	}
	orders, err := a.mail.Orders(r.Context(), limit, offset)
	if err != nil {
		a.moduleFail(w, err)
		return
	}
	write(w, 200, OrderList{orders, limit, offset})
}
func (a *API) order(w http.ResponseWriter, r *http.Request) {
	if !validID(w, r) || !a.mailAvailable(w) {
		return
	}
	order, err := a.mail.Order(r.Context(), r.PathValue("id"))
	if err != nil {
		a.moduleFail(w, err)
		return
	}
	write(w, 200, order)
}
func (a *API) mailExceptions(w http.ResponseWriter, r *http.Request) {
	limit, offset, ok := page(w, r)
	if !ok || !a.mailAvailable(w) {
		return
	}
	out, err := a.mail.MailExceptions(r.Context(), limit, offset)
	if err != nil {
		a.moduleFail(w, err)
		return
	}
	write(w, 200, ExceptionList{out, limit, offset})
}
func shoppingSpec(paths map[string]any) {
	for _, op := range shoppingOperations {
		path, ok := paths[op.Path].(map[string]any)
		if !ok {
			path = map[string]any{}
			paths[op.Path] = path
		}
		media := "application/json"
		if op.Path == "/v1/shopping/baskets/{id}/review" {
			media = "text/html"
		}
		entry := map[string]any{"summary": op.Summary, "security": []any{map[string]any{"bearerAuth": []string{}}}, "responses": map[string]any{"200": map[string]any{"description": "Success (including idempotent replay)", "content": map[string]any{media: map[string]any{"schema": schema(reflect.TypeOf(op.Response))}}}, "default": map[string]any{"description": "400 malformed/oversized input, 401 unauthorized, 404 missing, 409 source identity conflict, 415 media type, 422 invalid fields, 500/503 storage unavailable"}}}
		if op.Request != nil {
			entry["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": schema(reflect.TypeOf(op.Request))}}}
		}
		if op.Path == "/v1/mail/import" {
			entry["parameters"] = []any{map[string]any{"name": "account", "in": "query", "required": true, "schema": map[string]any{"type": "string", "maxLength": 120}}}
			entry["requestBody"] = map[string]any{"required": true, "content": map[string]any{"message/rfc822": map[string]any{"schema": map[string]any{"type": "string", "description": "RFC 5322/MIME message, maximum 2 MiB"}}}}
		}
		if op.Path == "/v1/orders" || op.Path == "/v1/mail/exceptions" {
			entry["parameters"] = []any{map[string]any{"name": "limit", "in": "query", "schema": map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "default": 100}}, map[string]any{"name": "offset", "in": "query", "schema": map[string]any{"type": "integer", "minimum": 0, "maximum": 1000000, "default": 0}}}
		}
		if strings.Contains(op.Path, "{id}") {
			entry["parameters"] = []any{map[string]any{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}}}
		}
		path[strings.ToLower(op.Method)] = entry
	}
}

func (a *API) syncStatus(w http.ResponseWriter, r *http.Request) {
	if len(r.URL.Query()) != 0 {
		write(w, 400, map[string]string{"error": "no query parameters supported"})
		return
	}
	store, ok := a.store.(mailsync.Store)
	if !ok {
		write(w, 503, map[string]string{"error": "mail sync storage unavailable"})
		return
	}
	out, e := store.MailStatuses(r.Context())
	if e != nil {
		a.moduleFail(w, e)
		return
	}
	write(w, 200, out)
}
