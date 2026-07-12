package router

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-playground/assert/v2"
)

func TestTemporarilyUnavailable(t *testing.T) {
	wrappedCalled := false
	wrapped := func(w http.ResponseWriter, r *http.Request) {
		wrappedCalled = true
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/stats/providers", nil)
	TemporarilyUnavailable(wrapped)(w, r)

	assert.Equal(t, wrappedCalled, false)
	assert.Equal(t, w.Code, http.StatusServiceUnavailable)
	assert.Equal(t, w.Header().Get("Content-Type"), "application/json")
	assert.Equal(t, w.Header().Get("Retry-After"), "3600")
	body, err := io.ReadAll(w.Result().Body)
	assert.Equal(t, err, nil)
	if !strings.Contains(string(body), `"error"`) {
		t.Fatalf("expected json error body, got %s", string(body))
	}
}
