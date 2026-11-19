package connect

// Loading the lb_alt front from the environment, and connect's start with and
// without it.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

// Sets the environment runWithDependencies reads, without a route file.
func setTestFrontRunEnv(t *testing.T) {
	t.Setenv("WARP_HOST_IPV4", "127.0.0.1")
	t.Setenv("WARP_HOST_IPV6", "")
	t.Setenv("WARP_PORTS", "8081:8081")
	t.Setenv("WARP_ENV", "test")
	t.Setenv("WARP_VERSION", "0.0.0-test")
	t.Setenv("WARP_SERVICE", "connect")
	t.Setenv("WARP_BLOCK", "g1")
	t.Setenv("WARP_HOST", "edge-a.example")
	t.Setenv(frontRouteFileEnvName, "")
	t.Setenv(frontGenerationEnvName, "")
}

// Without WARP_LBALT_ROUTE_FILE nothing changes: no front is built, and the
// private /status carries none of the front's headers.
func TestLbAltWithoutTheRouteFileChangesNothing(t *testing.T) {
	setTestFrontRunEnv(t)
	publicFront, err := newFrontServerFromEnv()
	if err != nil || publicFront != nil {
		t.Fatalf("front %v, err %v without a route file", publicFront, err)
	}

	var statusHeader http.Header
	err = runWithDependencies(
		t.Context(),
		RunOptions{Port: 8081, WarpStatus: &router.WarpStatusState{}, PrivateHeapProfileTarget: "disabled"},
		func(context.Context) error {
			// no database: the start serves /status only
			return errors.New("the test's readiness failure")
		},
		func(context.Context) func() {
			t.Error("a start that is not ready published a metrics cohort")
			return func() {}
		},
		func(_ context.Context, _ string, handler http.Handler, _ bool, _ server.HttpServerOptions) error {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/status", nil))
			statusHeader = recorder.Header()
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if statusHeader == nil {
		t.Fatal("the private listener never served")
	}
	for name := range statusHeader {
		if strings.HasPrefix(name, "X-Ur-Front-") {
			t.Fatalf("/status without a route file carries %s", name)
		}
	}
}

// A route file that can't be read fails connect's start before readiness or
// any listener: connect never comes up half-configured.
func TestLbAltRouteFileFailureFailsTheStart(t *testing.T) {
	setTestFrontRunEnv(t)
	t.Setenv(frontRouteFileEnvName, filepath.Join(t.TempDir(), "absent", "route.json"))
	err := runWithDependencies(
		t.Context(),
		RunOptions{Port: 8081, WarpStatus: &router.WarpStatusState{}, PrivateHeapProfileTarget: "disabled"},
		func(context.Context) error {
			t.Error("readiness ran with an unreadable route file")
			return nil
		},
		func(context.Context) func() {
			t.Error("a failed start published a metrics cohort")
			return func() {}
		},
		func(context.Context, string, http.Handler, bool, server.HttpServerOptions) error {
			t.Error("the private listener served with an unreadable route file")
			return nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "lb_alt") {
		t.Fatalf("start with an unreadable route file = %v", err)
	}
}

// The route file must be this generation's and this env's: another one
// means warpctl mounted another block's file, and the load fails before it
// reads a certificate.
func TestLbAltRouteFileOfAnotherBlockFailsTheLoad(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "nginx", "testdata", "route_v1_example.json"))
	if err != nil {
		t.Fatal(err)
	}
	routeFilePath := filepath.Join(t.TempDir(), "route.json")
	if err := os.WriteFile(routeFilePath, raw, 0o444); err != nil {
		t.Fatal(err)
	}
	vaultDir := t.TempDir()
	if _, err := loadFront(routeFilePath, "test-connect-g2-fedcba9876543210", "test", vaultDir); err == nil || !strings.Contains(err.Error(), "generation") {
		t.Fatalf("a route file of another generation loaded: %v", err)
	}
	if _, err := loadFront(routeFilePath, "", "other", vaultDir); err == nil || !strings.Contains(err.Error(), "env") {
		t.Fatalf("a route file of another env loaded: %v", err)
	}
	if _, err := loadFront(filepath.Join(t.TempDir(), "absent.json"), "", "test", vaultDir); err == nil {
		t.Fatal("an absent route file loaded")
	}
}
