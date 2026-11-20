// Composed operator shutdown must interrupt Connect before the delayed
// steady-state drain watcher exists, including its startup dependency read.
package connect

import (
	"context"
	"net/http"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/router"
)

// The startup barrier makes the cancellation happen inside the real runner's
// dependency read. No database, exchange or socket is constructed.
func TestConnectRunStartupReadinessCanceledByCaller(t *testing.T) {
	t.Setenv("WARP_HOST_IPV4", "127.0.0.1")
	t.Setenv("WARP_HOST_IPV6", "")
	t.Setenv("WARP_PORTS", "8081:8081")
	t.Setenv("WARP_ENV", "local")
	t.Setenv("WARP_VERSION", "0.0.0-test")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{})
	readCanceled := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- runWithDependencies(ctx, RunOptions{Port: 8081, WarpStatus: &router.WarpStatusState{}, PrivateHeapProfileTarget: "disabled"}, func(readCtx context.Context) error {
			close(entered)
			<-readCtx.Done()
			close(readCanceled)
			return readCtx.Err()
		}, func(context.Context) func() {
			t.Error("canceled startup published a metrics cohort")
			return func() {}
		}, func(listenCtx context.Context, _ string, _ http.Handler, _ bool, _ server.HttpServerOptions) error {
			if listenCtx.Err() == nil {
				t.Error("startup cancellation did not reach listener context")
			}
			return listenCtx.Err()
		})
	}()
	<-entered
	cancel()
	<-readCanceled
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}
