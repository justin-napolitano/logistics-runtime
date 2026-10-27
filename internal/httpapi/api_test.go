package httpapi

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthAndInputBoundaries(t *testing.T) {
	token := strings.Repeat("a", 32)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	if _, err := New(nil, "", logger); err == nil {
		t.Fatal("accepted empty token")
	}
	h, err := New(nil, token, logger)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path, body, auth, content string
		status                            int
	}{
		{"GET", "/v1/items", "", "", "", 401},
		{"GET", "/v1/items", "", "Basic " + token, "", 401},
		{"GET", "/v1/items?limit=10000", "", "Bearer " + token, "", 400},
		{"GET", "/v1/items?view=bogus", "", "Bearer " + token, "", 400},
		{"POST", "/v1/items", "{}", "Bearer " + token, "text/plain", 415},
		{"POST", "/v1/items", `{"id":"x","name":"x","unexpected":1}`, "Bearer " + token, "application/json", 400},
		{"POST", "/v1/items", `{"id":"x","name":"x"} {}`, "Bearer " + token, "application/json", 400},
		{"POST", "/v1/items", `{"id":"x","name":"x","on_hand":2}`, "Bearer " + token, "application/json", 422},
		{"POST", "/v1/items", `{"id":"x","name":"x","notes":"` + strings.Repeat("a", 17000) + `"}`, "Bearer " + token, "application/json", 400},
		{"PUT", "/v1/items/x", `{"revision":0,"name":"x"}`, "Bearer " + token, "application/json", 422},
		{"GET", "/healthz", "", "", "", 200},
		{"GET", "/v1/openapi.json", "", "Bearer " + token, "", 200},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		r.Header.Set("Authorization", tc.auth)
		r.Header.Set("Content-Type", tc.content)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Errorf("%s %s: got %d, want %d", tc.method, tc.path, w.Code, tc.status)
		}
		if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Request-ID") == "" {
			t.Fatal("missing privacy/correlation headers")
		}
	}
}
