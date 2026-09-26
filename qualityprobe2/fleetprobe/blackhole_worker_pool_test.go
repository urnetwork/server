// Pool ownership and direct admission boundaries use synthetic checks only.
package fleetprobe

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/qualityprobe/ingest"
	"github.com/urnetwork/server/qualityprobe/prober"
)

func testBlackholePoolResult(provider prober.Provider) BlackholeResult {
	return BlackholeResult{Check: ingest.BlackholeCheck{ClientId: provider.ClientId, Ok: true}}
}

func testBlackholePoolStop(t *testing.T, additional bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		pool, err := NewBlackholeWorkerPool(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer pool.CloseAndWait()
		release, stop := make(chan struct{}), make(chan struct{})
		firstDone, secondDone := make(chan struct{}), make(chan struct{})
		var called atomic.Int32
		options := BlackholeOptions{Timeout: time.Second, Concurrency: 1, WorkerPool: pool, CheckOne: func(_ context.Context, p prober.Provider) BlackholeResult {
			called.Add(1)
			<-release
			return testBlackholePoolResult(p)
		}}
		go func() {
			_, _ = RunBlackhole(ctx, []prober.Provider{{ClientId: "synthetic-first"}}, options)
			close(firstDone)
		}()
		synctest.Wait()
		second := options
		if additional {
			second.AdditionalAdmissionDone = stop
		} else {
			second.AdmissionDone = stop
		}
		var summary BlackholeSummary
		var secondErr error
		go func() {
			summary, secondErr = RunBlackhole(ctx, []prober.Provider{{ClientId: "synthetic-second"}}, second)
			close(secondDone)
		}()
		synctest.Wait()
		close(stop)
		synctest.Wait()
		select {
		case <-secondDone:
		default:
			t.Error("blocked admission ignored its direct stop edge")
		}
		close(release)
		<-firstDone
		<-secondDone
		if called.Load() != 1 || len(summary.Checks) != 0 || secondErr != nil {
			t.Errorf("refused check ran or became a result: called=%d checks=%d err=%v", called.Load(), len(summary.Checks), secondErr)
		}
	})
}

func TestBlackholeWorkerPoolDirectFullStop(t *testing.T)   { testBlackholePoolStop(t, false) }
func TestBlackholeWorkerPoolDirectCutoffStop(t *testing.T) { testBlackholePoolStop(t, true) }

func TestBlackholeWorkerPoolInitialMinimumAndOwnerIsolation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		firstCtx, cancelFirst := context.WithCancel(context.Background())
		defer cancelFirst()
		first, err := NewBlackholeWorkerPool(firstCtx, 1)
		if err != nil {
			t.Fatal(err)
		}
		second, err := NewBlackholeWorkerPool(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		defer second.CloseAndWait()
		firstDone := make(chan struct{})
		go func() {
			_, _ = RunBlackhole(firstCtx, []prober.Provider{{ClientId: "synthetic-held"}}, BlackholeOptions{Timeout: time.Second, Concurrency: 1, WorkerPool: first, CheckOne: func(ctx context.Context, p prober.Provider) BlackholeResult {
				<-ctx.Done()
				return testBlackholePoolResult(p)
			}})
			close(firstDone)
		}()
		synctest.Wait()
		stop := make(chan struct{})
		close(stop)
		options := BlackholeOptions{Timeout: time.Second, Concurrency: 1, WorkerPool: second, MinimumAdmission: 2, AdmissionDone: stop, AdditionalAdmissionDone: stop, CheckOne: func(_ context.Context, p prober.Provider) BlackholeResult { return testBlackholePoolResult(p) }}
		providers := []prober.Provider{{ClientId: "synthetic-two"}, {ClientId: "synthetic-three"}, {ClientId: "synthetic-four"}}
		got, err := RunBlackhole(context.Background(), providers, options)
		if err != nil || len(got.Checks) != 2 {
			t.Errorf("independent owner/minimum blocked: checks=%d err=%v", len(got.Checks), err)
		}
		cancelFirst()
		<-firstDone
		first.CloseAndWait()
		got, err = RunBlackhole(context.Background(), providers, options)
		if err != nil || len(got.Checks) != 2 {
			t.Error("closing another owner poisoned surviving pool")
		}
	})
}

func TestBlackholeWorkerPoolClosedIsNotHealthyEmpty(t *testing.T) {
	pool, err := NewBlackholeWorkerPool(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	pool.CloseAndWait()
	got, err := RunBlackhole(context.Background(), []prober.Provider{{ClientId: "synthetic-unavailable"}}, BlackholeOptions{Timeout: time.Second, Concurrency: 1, WorkerPool: pool, CheckOne: func(_ context.Context, p prober.Provider) BlackholeResult {
		t.Error("closed owner executed a check")
		return testBlackholePoolResult(p)
	}})
	if !errors.Is(err, ErrBlackholeWorkerPoolClosed) || len(got.Checks) != 0 {
		t.Errorf("closed pool became healthy: checks=%d err=%v", len(got.Checks), err)
	}
}
