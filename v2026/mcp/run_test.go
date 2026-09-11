package mcp

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/router"
)

// setMcpRunTestEnvironment installs only synthetic Warp identity values.
func setMcpRunTestEnvironment(t *testing.T) {
	t.Helper()
	for key, value := range map[string]string{
		"WARP_ENV": "test", "WARP_VERSION": "2026.9.11+1",
		"WARP_HOST": "fixture.example", "WARP_SERVICE": "mcp", "WARP_BLOCK": "g1",
		"WARP_HOST_IPV4": "192.0.2.10", "WARP_HOST_IPV6": "2001:db8::10", "WARP_PORTS": "8080:8080",
	} {
		t.Setenv(key, value)
	}
	t.Cleanup(router.SetWarpStatusReady)
}

func TestRunRejectsInvalidInputsBeforeEnvironmentAccess(t *testing.T) {
	if err := Run(nil, RunOptions{Port: 1}); err == nil {
		t.Fatal("nil context was accepted")
	}
	if err := Run(context.Background(), RunOptions{Port: 0}); err == nil {
		t.Fatal("zero port was accepted")
	}
	if err := Run(context.Background(), RunOptions{Port: 65_536}); err == nil {
		t.Fatal("overflow port was accepted")
	}
}

func TestRunRejectedMcpDoesNotPublishMetrics(t *testing.T) {
	setMcpRunTestEnvironment(t)
	wantErr := errors.New("synthetic readiness failure")
	starts := 0
	serves := 0
	err := runWithDependencies(
		context.Background(),
		RunOptions{Port: 8080},
		func(context.Context) error { return wantErr },
		func(context.Context) func() {
			starts++
			return func() { t.Error("rejected MCP candidate flushed metrics") }
		},
		func(context.Context, string, http.Handler, bool, server.HttpServerOptions) error {
			serves++
			return nil
		},
		func() []*router.Route {
			return []*router.Route{router.NewRoute("GET", "/status", router.WarpStatus)}
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if starts != 0 || serves != 1 {
		t.Fatalf("metric starts/serves = %d/%d, want 0/1", starts, serves)
	}
}

func TestRunFlushesMetricsAfterMcpDrain(t *testing.T) {
	setMcpRunTestEnvironment(t)
	ctx, cancel := context.WithCancel(context.Background())
	var stage atomic.Int32
	err := runWithDependencies(
		ctx,
		RunOptions{Port: 8080},
		func(context.Context) error {
			if !stage.CompareAndSwap(0, 1) {
				t.Fatal("MCP startup ran out of order")
			}
			return nil
		},
		func(context.Context) func() {
			if !stage.CompareAndSwap(1, 2) {
				t.Fatal("MCP metrics started before readiness")
			}
			return func() {
				if !stage.CompareAndSwap(3, 4) {
					t.Error("MCP metrics flushed before HTTP drain completed")
				}
			}
		},
		func(serveCtx context.Context, _ string, _ http.Handler, _ bool, options server.HttpServerOptions) error {
			if options.ShutdownTimeout <= 0 || options.KeepaliveDrainTimeout <= 0 {
				t.Errorf("MCP drain options = %+v, want bounded shutdown and keepalive drain", options)
			}
			cancel()
			<-serveCtx.Done()
			if !stage.CompareAndSwap(2, 3) {
				t.Error("MCP listener completed out of order")
			}
			return serveCtx.Err()
		},
		func() []*router.Route {
			return []*router.Route{router.NewRoute("GET", "/status", router.WarpStatus)}
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := stage.Load(); got != 4 {
		t.Fatalf("MCP terminal stage = %d, want flushed stage 4", got)
	}
}
