package taskworker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

func TestRunRejectsInvalidInputsBeforeEnvironmentAccess(t *testing.T) {
	if err := Run(nil, RunOptions{Port: 1, Count: 1, BatchSize: 1}); err == nil {
		t.Fatal("nil context was accepted")
	}
	if err := Run(context.Background(), RunOptions{Port: 0, Count: 1, BatchSize: 1}); err == nil {
		t.Fatal("zero port was accepted")
	}
	if err := Run(context.Background(), RunOptions{Port: 1, Count: 0, BatchSize: 1}); err == nil {
		t.Fatal("zero worker count was accepted")
	}
	if err := Run(context.Background(), RunOptions{Port: 1, Count: 1, BatchSize: 0}); err == nil {
		t.Fatal("zero batch size was accepted")
	}
}

func TestRunRejectedTaskworkerPreservesStatusWithoutPublishingMetrics(t *testing.T) {
	for key, value := range map[string]string{
		"WARP_ENV": "test", "WARP_VERSION": "2026.9.4+1037600680",
		"WARP_HOST": "synthetic-host", "WARP_SERVICE": "taskworker", "WARP_BLOCK": "g1",
		"WARP_HOST_IPV4": "127.0.0.1", "WARP_HOST_IPV6": "", "WARP_PORTS": "8080:8080",
	} {
		t.Setenv(key, value)
	}
	t.Cleanup(router.SetWarpStatusReady)
	readinessErr := errors.New("database migration head 627 is below binary-required head 630")
	checks, starts, flushes, serves := 0, 0, 0, 0
	err := runWithDependencies(context.Background(), RunOptions{Port: 8080, Count: 1, BatchSize: 1},
		func(context.Context) error {
			checks++
			router.SetWarpStatusNotReady(readinessErr)
			return readinessErr
		},
		func(context.Context) func() {
			starts++
			return func() { flushes++ }
		},
		func(_ context.Context, _ string, handler http.Handler, _ bool, _ server.HttpServerOptions) error {
			serves++
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/status", nil))
			var status router.WarpStatusResult
			if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
				t.Fatalf("decode rejected candidate status: %v", err)
			}
			if response.Code != http.StatusOK || status.Status != "error not ready: "+readinessErr.Error() {
				t.Fatalf("rejected candidate lost its readiness failure: code=%d status=%q", response.Code, status.Status)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if checks != 1 || serves != 1 || starts != 0 || flushes != 0 {
		t.Fatalf("checks/serves/metrics starts/flushes = %d/%d/%d/%d, want 1/1/0/0", checks, serves, starts, flushes)
	}
}
