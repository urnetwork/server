// Catalog failures must not silently switch providers to compiled-in targets.
package fleetprobe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

func TestUrlPoolFailureDoesNotFallback(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer endpoint.Close()
	pool, err := LoadPool(context.Background(), endpoint.Client(), endpoint.URL, "synthetic-secret")
	if !errors.Is(err, egresshealth.ErrPoolUnavailable) || pool != nil {
		t.Fatalf("catalog failure returned a runnable fallback: pool-present=%t error=%v", pool != nil, err)
	}
}
