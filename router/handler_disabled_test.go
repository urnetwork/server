package router

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

func TestTemporarilyUnavailable(t *testing.T) {
	wrappedCalled := false
	wrapped := func(w http.ResponseWriter, r *http.Request) {
		wrappedCalled = true
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/stats/providers", nil)
	TemporarilyUnavailable(wrapped)(w, r)

	connect.AssertEqual(t, wrappedCalled, false)
	connect.AssertEqual(t, w.Code, http.StatusServiceUnavailable)
	connect.AssertEqual(t, w.Header().Get("Content-Type"), "application/json")
	connect.AssertEqual(t, w.Header().Get("Retry-After"), "3600")
	body, err := io.ReadAll(w.Result().Body)
	connect.AssertEqual(t, err, nil)
	if !strings.Contains(string(body), `"error"`) {
		t.Fatalf("expected json error body, got %s", string(body))
	}
}
