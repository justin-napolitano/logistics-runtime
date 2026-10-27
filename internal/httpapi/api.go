// Package httpapi exposes the private inventory API. It serves no frontend.
package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/justin-napolitano/logistics-runtime/internal/inventory"
	"github.com/justin-napolitano/logistics-runtime/internal/ordermail"
	"github.com/justin-napolitano/logistics-runtime/internal/shopping"
)

// CreateRequest supplies a stable caller-generated ID; duplicate creation is a conflict.
type CreateRequest struct {
	ID string `json:"id"`
	inventory.Input
}

// ReplaceRequest is a full replacement of editable state, conditional on Revision.
type ReplaceRequest struct {
	Revision int64 `json:"revision"`
	inventory.Input
}

type API struct {
	store    inventory.Store
	shopping shopping.Store
	mail     ordermail.Store
	logger   *slog.Logger
}

func New(store inventory.Store, token string, logger *slog.Logger) (http.Handler, error) {
	if len(token) < 32 || strings.TrimSpace(token) != token {
		return nil, errors.New("API_TOKEN must contain at least 32 characters without surrounding whitespace")
	}
	if logger == nil {
		logger = slog.Default()
	}
	a := &API{store: store, logger: logger}
	a.shopping, _ = store.(shopping.Store)
	a.mail, _ = store.(ordermail.Store)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		write(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := store.Ready(ctx); err != nil {
			write(w, 503, map[string]string{"status": "unavailable"})
			return
		}
		write(w, 200, map[string]string{"status": "ready"})
	})
	private := http.NewServeMux()
	for _, op := range operations {
		op := op
		private.HandleFunc(op.Method+" "+op.Path, func(w http.ResponseWriter, r *http.Request) { op.Handle(a, w, r) })
	}
	for _, op := range shoppingOperations {
		op := op
		private.HandleFunc(op.Method+" "+op.Path, func(w http.ResponseWriter, r *http.Request) { op.Handle(a, w, r) })
	}
	private.HandleFunc("GET /v1/openapi.json", func(w http.ResponseWriter, r *http.Request) { write(w, 200, Spec()) })
	expected := sha256.Sum256([]byte(token))
	mux.Handle("/v1/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		given := sha256.Sum256([]byte(strings.TrimPrefix(header, "Bearer ")))
		if !strings.HasPrefix(header, "Bearer ") || subtle.ConstantTimeCompare(given[:], expected[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="logistics"`)
			write(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		private.ServeHTTP(w, r)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := rand.Text()
		w.Header().Set("X-Request-ID", requestID)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		start := time.Now()
		// Deliberately omit URLs, query strings, headers, bodies and database errors.
		defer func() {
			logger.Info("request", "request_id", requestID, "method", r.Method, "duration_ms", time.Since(start).Milliseconds())
		}()
		mux.ServeHTTP(w, r.WithContext(ctx))
	}), nil
}

func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	return decodeLimit(w, r, out, 16<<10)
}
func decodeLimit(w http.ResponseWriter, r *http.Request, out any, limit int64) bool {
	if ct := strings.Split(r.Header.Get("Content-Type"), ";")[0]; strings.TrimSpace(ct) != "application/json" {
		write(w, 415, map[string]string{"error": "application/json required"})
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		write(w, 400, map[string]string{"error": "invalid JSON body"})
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		write(w, 400, map[string]string{"error": "expected one JSON object"})
		return false
	}
	return true
}

func write(w http.ResponseWriter, status int, out any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(out)
}

func (a *API) fail(w http.ResponseWriter, err error) {
	status, message := 500, "inventory operation failed"
	switch {
	case errors.Is(err, inventory.ErrInvalid):
		status, message = 422, "invalid fields or unit change; clear existing quantities before changing unit"
	case errors.Is(err, inventory.ErrNotFound):
		status, message = 404, "item not found"
	case errors.Is(err, inventory.ErrConflict):
		status, message = 409, "item exists or revision changed; read current state before retrying"
	}
	if status == 500 {
		a.logger.Error("inventory operation failed")
	}
	write(w, status, map[string]string{"error": message})
}

func validID(w http.ResponseWriter, r *http.Request) bool {
	if !inventory.ValidID(r.PathValue("id")) {
		write(w, 400, map[string]string{"error": "invalid item ID"})
		return false
	}
	return true
}

func (a *API) get(w http.ResponseWriter, r *http.Request) {
	if !validID(w, r) {
		return
	}
	item, err := a.store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	write(w, 200, inventory.Compare(item))
}

func (a *API) create(w http.ResponseWriter, r *http.Request) {
	var body CreateRequest
	if !decode(w, r, &body) {
		return
	}
	if !inventory.ValidID(body.ID) {
		a.fail(w, inventory.ErrInvalid)
		return
	}
	if err := body.Input.Validate(); err != nil {
		a.fail(w, err)
		return
	}
	item, err := a.store.Create(r.Context(), body.ID, body.Input)
	if err != nil {
		a.fail(w, err)
		return
	}
	w.Header().Set("Location", "/v1/items/"+item.ID)
	write(w, 201, inventory.Compare(item))
}

func (a *API) replace(w http.ResponseWriter, r *http.Request) {
	if !validID(w, r) {
		return
	}
	var body ReplaceRequest
	if !decode(w, r, &body) {
		return
	}
	if body.Revision < 1 {
		a.fail(w, inventory.ErrInvalid)
		return
	}
	if err := body.Input.Validate(); err != nil {
		a.fail(w, err)
		return
	}
	item, err := a.store.Replace(r.Context(), r.PathValue("id"), body.Revision, body.Input)
	if err != nil {
		a.fail(w, err)
		return
	}
	write(w, 200, inventory.Compare(item))
}

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for k := range q {
		if k != "limit" && k != "offset" && k != "view" && k != "category" {
			write(w, 400, map[string]string{"error": "unknown query parameter"})
			return
		}
	}
	f := inventory.Filter{Category: q.Get("category"), View: q.Get("view"), Limit: 100}
	if f.View == "" {
		f.View = "all"
	}
	if f.View != "all" && f.View != "unknown" && f.View != "shortages" {
		write(w, 400, map[string]string{"error": "invalid view"})
		return
	}
	for key, dest := range map[string]*int{"limit": &f.Limit, "offset": &f.Offset} {
		if q.Has(key) {
			n, err := strconv.Atoi(q.Get(key))
			if err != nil {
				write(w, 400, map[string]string{"error": "invalid pagination"})
				return
			}
			*dest = n
		}
	}
	if f.Limit < 1 || f.Limit > 200 || f.Offset < 0 || f.Offset > 1000000 || len(f.Category) > 200 {
		write(w, 400, map[string]string{"error": "invalid filters"})
		return
	}
	items, err := a.store.List(r.Context(), f)
	if err != nil {
		a.fail(w, err)
		return
	}
	views := make([]inventory.View, 0, len(items))
	for _, item := range items {
		views = append(views, inventory.Compare(item))
	}
	write(w, 200, struct {
		Items  []inventory.View `json:"items"`
		Limit  int              `json:"limit"`
		Offset int              `json:"offset"`
	}{views, f.Limit, f.Offset})
}
