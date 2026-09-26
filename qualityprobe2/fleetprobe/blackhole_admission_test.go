// The optional per-call admission signal never becomes a global limit or
// a cancellation shortcut for already-started provider measurements.
package fleetprobe

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/operator-proxy/ingest"
	"github.com/urnetwork/operator-proxy/prober"
)

func testBlackholeAdmissionContract(t *testing.T, ctx context.Context, stop <-chan struct{}, minimum int, count int) (BlackholeSummary, int, error) {
	t.Helper()
	providers := make([]prober.Provider, count)
	for index := range providers {
		providers[index] = prober.Provider{ClientId: fmt.Sprintf("synthetic-%03d", index)}
	}
	var started atomic.Int32
	summary, err := RunBlackhole(ctx, providers, BlackholeOptions{
		Timeout: time.Second, Concurrency: 2, AdmissionDone: stop, MinimumAdmission: minimum,
		CheckOne: func(ctx context.Context, provider prober.Provider) BlackholeResult {
			started.Add(1)
			if ctx.Err() != nil {
				t.Error("a canceled check was admitted")
			}
			return BlackholeResult{Check: ingest.BlackholeCheck{ClientId: provider.ClientId, Ok: true}}
		},
	})
	return summary, int(started.Load()), err
}

func TestRunBlackholeAdmissionClosedKeepsMinimum(t *testing.T) {
	stop := make(chan struct{})
	close(stop)
	summary, started, err := testBlackholeAdmissionContract(t, context.Background(), stop, 3, 7)
	if err != nil || started != 3 || len(summary.Checks) != 3 {
		t.Fatalf("closed admission minimum: started=%d checks=%d err=%v", started, len(summary.Checks), err)
	}
	for index, check := range summary.Checks {
		if check.ClientId != fmt.Sprintf("synthetic-%03d", index) || !check.Ok {
			t.Fatal("initial cohort lost due order or passing evidence")
		}
	}
}

func TestRunBlackholeAdmissionClosedWithoutMinimumStartsNothing(t *testing.T) {
	stop := make(chan struct{})
	close(stop)
	summary, started, err := testBlackholeAdmissionContract(t, context.Background(), stop, 0, 7)
	if err != nil || started != 0 || len(summary.Checks) != 0 {
		t.Fatalf("closed zero-minimum admission: started=%d checks=%d err=%v", started, len(summary.Checks), err)
	}
}

func TestRunBlackholeAdmissionMinimumIsBoundedBySelectedProviders(t *testing.T) {
	stop := make(chan struct{})
	close(stop)
	summary, started, err := testBlackholeAdmissionContract(t, context.Background(), stop, 99, 2)
	if err != nil || started != 2 || len(summary.Checks) != 2 {
		t.Fatalf("minimum invented work: started=%d checks=%d err=%v", started, len(summary.Checks), err)
	}
}

func TestRunBlackholeAdmissionNilSignalIsNotACap(t *testing.T) {
	summary, started, err := testBlackholeAdmissionContract(t, context.Background(), nil, 1, 7)
	if err != nil || started != 7 || len(summary.Checks) != 7 {
		t.Fatalf("minimum became a cap without a stop signal: started=%d checks=%d err=%v", started, len(summary.Checks), err)
	}
}

func TestRunBlackholeAdmissionCancellationOverridesMinimum(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	summary, started, err := testBlackholeAdmissionContract(t, ctx, nil, 3, 7)
	if err != nil || started != 0 || len(summary.Checks) != 0 {
		t.Fatalf("minimum bypassed parent cancellation: started=%d checks=%d err=%v", started, len(summary.Checks), err)
	}
}

func TestRunBlackholeAdmissionRejectsNegativeMinimumBeforeWork(t *testing.T) {
	summary, started, err := testBlackholeAdmissionContract(t, context.Background(), nil, -1, 7)
	if err == nil || started != 0 || len(summary.Checks) != 0 {
		t.Fatalf("invalid minimum admitted work: started=%d checks=%d err=%v", started, len(summary.Checks), err)
	}
}
