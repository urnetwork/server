// The process publisher owns a final sample after the listener's last event,
// including cancellation and listener failure after successful admission.
package connect

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

// The real runner starts and stops its exchange. The injected publisher gathers
// a real counter so an event after the periodic owner's cancellation must still
// reach the terminal sample; no sleep or periodic tick can make this pass.
func requireRunFinalStats(t *testing.T, listenerFailure bool) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		for key, value := range map[string]string{
			"WARP_SERVICE": "connect", "WARP_BLOCK": "g1", "WARP_HOST": "synthetic-host",
			"WARP_VERSION":   "2026.1.1+1",
			"WARP_HOST_IPV4": "127.0.0.1", "WARP_HOST_IPV6": "", "WARP_PORTS": "8080:8080,5080:0",
			"ARIN_SHADOW_CAPTURE_CONFIG": "",
		} {
			t.Setenv(key, value)
		}
		t.Cleanup(router.SetWarpStatusReady)
		registry := prometheus.NewRegistry()
		counter := prometheus.NewCounter(prometheus.CounterOpts{
			Name: "urnetwork_test_connect_final_events_total",
			Help: "Synthetic final acknowledged event at the listener boundary.",
		})
		registry.MustRegister(counter)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		starts, flushes := 0, 0
		listenerReturned := false
		var flushedValue float64
		listenerErr := errors.New("synthetic listener failure after admission")
		err := runWithDependencies(ctx, RunOptions{Port: 8080, PrivateHeapProfileTarget: "disabled"},
			func(context.Context) error { return nil },
			func(context.Context) func() {
				starts++
				return func() {
					flushes++
					if !listenerReturned {
						t.Fatal("final statistics were flushed before the listener ended")
					}
					families, err := registry.Gather()
					if err != nil || len(families) != 1 || len(families[0].Metric) != 1 {
						t.Fatal("final counter registry was not gathered", err)
					}
					flushedValue = families[0].Metric[0].GetCounter().GetValue()
				}
			},
			func(serveCtx context.Context, _ string, _ http.Handler, _ bool, _ server.HttpServerOptions) error {
				if !listenerFailure {
					cancel()
					// The actual drain owns this cancellation edge.
					<-serveCtx.Done()
				}
				counter.Inc()
				listenerReturned = true
				if listenerFailure {
					return listenerErr
				}
				return nil
			})
		if listenerFailure && !errors.Is(err, listenerErr) || !listenerFailure && err != nil {
			t.Fatal("final statistics changed the listener result", err)
		}
		if starts != 1 || flushes != 1 || flushedValue != 1 {
			t.Fatalf("final publisher starts=%d flushes=%d value=%v; want 1/1/1", starts, flushes, flushedValue)
		}
	})
}

// A graceful drain must retain the last acknowledged event after cancellation.
func TestRunFlushesFinalStatsAfterDrain(t *testing.T) {
	requireRunFinalStats(t, false)
}

// A started publisher must retain the final sample on an early listener error.
func TestRunFlushesFinalStatsAfterListenerFailure(t *testing.T) {
	requireRunFinalStats(t, true)
}
