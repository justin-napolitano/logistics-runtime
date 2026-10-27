package httpapi

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestShoppingMailInputAndAuthorization(t *testing.T) {
	token := strings.Repeat("t", 32)
	handler, err := New(nil, token, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path, body, content string
		auth                        bool
		status                      int
	}{
		{"GET", "/v1/orders", "", "", false, 401},
		{"GET", "/v1/orders?limit=201", "", "", true, 400},
		{"GET", "/v1/orders?limit=1&limit=2", "", "", true, 400},
		{"GET", "/v1/mail/exceptions?foo=bar", "", "", true, 400},
		{"POST", "/v1/shopping/templates", `{"name":"x","lines":[]}`, "application/json", true, 422},
		{"POST", "/v1/shopping/baskets", `{"template_id":"x","cycle":"bad cycle"}`, "application/json", true, 422},
		{"POST", "/v1/mail/import?account=personal", "x", "text/plain", true, 415},
		{"POST", "/v1/mail/import?account=a&account=b", "x", "message/rfc822", true, 400},
		{"POST", "/v1/mail/import?account=a", strings.Repeat("x", (2<<20)+1), "message/rfc822", true, 400},
		{"GET", "/v1/orders", "", "", true, 503},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		if tc.auth {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		r.Header.Set("Content-Type", tc.content)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Errorf("%s %s: %d want %d", tc.method, tc.path, w.Code, tc.status)
		}
	}
}
