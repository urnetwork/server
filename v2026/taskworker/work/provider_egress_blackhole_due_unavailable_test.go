package work

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
	"github.com/urnetwork/server/v2026/router"
)

// This transport drives the real router synchronously, retaining HTTP status,
// response body and ingest decoding while fake time owns the bounded retry.
type blackholeDueRouterTransport struct{ handler http.Handler }

func (self blackholeDueRouterTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	writer := httptest.NewRecorder()
	self.handler.ServeHTTP(writer, request)
	return writer.Result(), nil
}

// A child DB deadline previously became empty 200 -> JSON EOF -> permanent
// decode failure, stranding cheap capacity behind the independent Full lane.
func TestBlackholeDueInternalDeadlineRetriesExactWireEndpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requests := 0
		handler := router.NewRouter(t.Context(), []*router.Route{router.NewRoute(http.MethodGet, "/network/provider-blackhole-due", func(w http.ResponseWriter, r *http.Request) {
			requests++
			if requests == 1 {
				child, cancel := context.WithDeadline(r.Context(), time.Unix(0, 0))
				defer cancel()
				if child.Err() == nil || r.Context().Err() != nil {
					t.Error("child deadline fixture not reached")
				}
				panic(server.DbContextDoneError)
			}
			_, _ = io.WriteString(w, `{"providers":[{"client_id":"synthetic-provider"}]}`)
		})})
		client := &ingest.Client{ServerUrl: "https://synthetic.example", OperatorSecret: "synthetic-secret", Http: &http.Client{Transport: blackholeDueRouterTransport{handler}}}
		pass := &providerEgressProbePass{blackholeDue: client.BlackholeDue}
		started := time.Now()
		due, err := pass.blackholeDueWithRetry(t.Context(), 250, nil)
		if err != nil || requests != 2 || len(due) != 1 || due[0].ClientId != "synthetic-provider" {
			t.Fatalf("transient internal deadline stranded cheap due admission: requests=%d providers=%d error=%v", requests, len(due), err)
		}
		if elapsed := time.Since(started); elapsed < 250*time.Millisecond || elapsed >= 500*time.Millisecond {
			t.Fatalf("recovery lost bounded jitter: %s", elapsed)
		}
	})
}

// Persistent overload cannot extend admission or turn read retries into a
// hot loop. Cancellation and explicit endpoint faults still win over 503.
func TestBlackholeDueUnavailableKeepsRetryAndOwnerBounds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		unavailable := errors.Join(ingest.ErrRejected, ingest.ErrBlackholeDueUnavailable)
		reads := 0
		before := testutil.ToFloat64(egressProbeBlackholeDueReads.WithLabelValues("unavailable"))
		pass := &providerEgressProbePass{blackholeDue: func(context.Context, int) ([]ingest.DueProvider, error) { reads++; return nil, unavailable }}
		started := time.Now()
		_, err := pass.blackholeDueWithRetry(t.Context(), 250, nil)
		if reads != 3 || err != unavailable || time.Since(started) < 500*time.Millisecond || time.Since(started) >= time.Second {
			t.Fatalf("unavailable retry exceeded existing bound: reads=%d elapsed=%s error=%v", reads, time.Since(started), err)
		}
		if got := testutil.ToFloat64(egressProbeBlackholeDueReads.WithLabelValues("unavailable")) - before; got != 3 {
			t.Fatalf("unavailable attempts observed=%g, want3", got)
		}
		for _, permanent := range []error{ingest.ErrUnauthorized, ingest.ErrBlackholeUnsupported, context.Canceled} {
			reads = 0
			joined := errors.Join(unavailable, permanent)
			pass.blackholeDue = func(context.Context, int) ([]ingest.DueProvider, error) { reads++; return nil, joined }
			_, err = pass.blackholeDueWithRetry(t.Context(), 250, nil)
			if reads != 1 || err != joined {
				t.Fatalf("permanent endpoint/caller failure retried: reads=%d error=%v", reads, err)
			}
		}
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		reads = 0
		pass.blackholeDue = func(context.Context, int) ([]ingest.DueProvider, error) { reads++; return nil, unavailable }
		_, err = pass.blackholeDueWithRetry(ctx, 250, nil)
		if reads != 1 || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("owner deadline permitted another unavailable read: reads=%d error=%v", reads, err)
		}
	})
}
