package work

import (
	"context"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/model"
)

// A short empty pass can finish before its initial census read. Returning from
// the pass must join that bounded read, not turn it into a canceled refresh.
// The barriers force the production wrapper's normal-return ordering; no
// database, network, scheduler luck or wall-clock sleep supplies the failure.
func TestProviderUrlProbeNormalPassCompletesCensusRead(t *testing.T) {
	const shard = 245
	t.Cleanup(func() { urlProbeShardObservedAt.DeleteLabelValues(strconv.Itoa(shard)) })
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		args := &ProviderEgressProbeArgs{ShardIndex: shard, ShardCount: 256, UrlProbe: &ProviderEgressProbeBatchArgs{}}
		metrics := newProviderUrlProbeFleetRefreshCollectors(prometheus.NewPedanticRegistry())
		collector := newProviderUrlProbeFleetCollector()
		collector.refreshMetrics = metrics
		previous := &providerUrlProbeFleetSnapshot{fleet: model.ProviderUrlProbeFleet{Eligible: 1}, observedAt: time.Unix(1, 0)}
		collector.snapshot.Store(previous)
		observedAt := time.Now()
		readStarted, releaseRead := make(chan struct{}), make(chan struct{})
		completed := make(chan struct{})
		var refreshErr error
		var readCtx context.Context
		reads := 0
		go func() {
			defer close(completed)
			_, err := runWithProviderUrlProbeFleetHeartbeat(ctx, args, func(refreshCtx context.Context) {
				providerUrlProbeFleetHeartbeat(args)(refreshCtx)
				refreshErr = collector.refresh(refreshCtx, observedAt, func(snapshotCtx context.Context, comparisonAt time.Time) model.ProviderUrlProbeFleet {
					reads++
					readCtx = snapshotCtx
					close(readStarted)
					select {
					case <-releaseRead:
						return model.ProviderUrlProbeFleet{Eligible: 100, QuotaComplete: 90, RunsNeeded: 100}
					case <-snapshotCtx.Done():
						panic(snapshotCtx.Err())
					}
				})
			}, func() (*ProviderEgressProbeResult, error) {
				<-readStarted
				return &ProviderEgressProbeResult{}, nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
		<-readStarted
		synctest.Wait()
		returnedBeforeRead := false
		select {
		case <-completed:
			returnedBeforeRead = true
		default:
		}
		pendingSnapshot := collector.snapshot.Load()
		pendingHeartbeat := urlOwnerTestGauge(t, shard)
		close(releaseRead)
		<-completed
		synctest.Wait()
		if returnedBeforeRead || pendingSnapshot != previous || pendingHeartbeat <= 0 {
			t.Error("normal pass canceled its in-flight census instead of retaining the bounded owner")
		}
		current := collector.snapshot.Load()
		if refreshErr != nil || current == previous || current.observedAt != observedAt || current.fleet.Eligible != 100 || current.fleet.QuotaComplete != 90 || current.fleet.RunsNeeded != 100 {
			t.Errorf("normal pass did not publish the complete bounded census: error=%v snapshot=%+v", refreshErr, current)
		}
		if readCtx.Err() == nil || urlOwnerTestGauge(t, shard) != 0 {
			t.Error("completed census retained its context or task heartbeat")
		}
		if testutil.ToFloat64(metrics.results.WithLabelValues("success")) != 1 || testutil.ToFloat64(metrics.results.WithLabelValues("context_canceled")) != 0 {
			t.Error("normal pass was classified as a canceled census refresh")
		}
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if reads != 1 || urlOwnerTestGauge(t, shard) != 0 {
			t.Error("completed URL owner launched another census or heartbeat")
		}
	})
}

// Actual task/process cancellation is still authoritative while the completed
// pass joins its census; it must not wait for a detached ten-second query.
func TestProviderUrlProbeCensusReadPreservesParentCancellation(t *testing.T) {
	const shard = 246
	t.Cleanup(func() { urlProbeShardObservedAt.DeleteLabelValues(strconv.Itoa(shard)) })
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		args := &ProviderEgressProbeArgs{ShardIndex: shard, ShardCount: 256, UrlProbe: &ProviderEgressProbeBatchArgs{}}
		metrics := newProviderUrlProbeFleetRefreshCollectors(prometheus.NewPedanticRegistry())
		collector := newProviderUrlProbeFleetCollector()
		collector.refreshMetrics = metrics
		previous := &providerUrlProbeFleetSnapshot{fleet: model.ProviderUrlProbeFleet{Eligible: 1}, observedAt: time.Unix(1, 0)}
		collector.snapshot.Store(previous)
		startedAt := time.Now()
		readStarted, completed := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(completed)
			runWithProviderUrlProbeFleetHeartbeat(ctx, args, func(refreshCtx context.Context) {
				providerUrlProbeFleetHeartbeat(args)(refreshCtx)
				collector.refresh(refreshCtx, startedAt, func(snapshotCtx context.Context, _ time.Time) model.ProviderUrlProbeFleet {
					close(readStarted)
					<-snapshotCtx.Done()
					return model.ProviderUrlProbeFleet{Eligible: 99}
				})
			}, func() (*ProviderEgressProbeResult, error) {
				<-readStarted
				return &ProviderEgressProbeResult{}, nil
			})
		}()
		<-readStarted
		synctest.Wait()
		cancel()
		<-completed
		if time.Since(startedAt) != 0 || collector.snapshot.Load() != previous || urlOwnerTestGauge(t, shard) != 0 {
			t.Error("parent cancellation delayed shutdown, published a partial census, or retained ownership")
		}
		if testutil.ToFloat64(metrics.results.WithLabelValues("context_canceled")) != 1 || testutil.ToFloat64(metrics.results.WithLabelValues("success")) != 0 {
			t.Error("actual parent cancellation was hidden as a successful census")
		}
	})
}

// A blocked census still stops at the collector's real ten-second deadline.
// Normal pass completion does not extend the query budget or publish old data
// with a new comparison clock. Synctest advances only while the owner waits.
func TestProviderUrlProbeCensusReadKeepsTenSecondDeadline(t *testing.T) {
	const shard = 247
	t.Cleanup(func() { urlProbeShardObservedAt.DeleteLabelValues(strconv.Itoa(shard)) })
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		args := &ProviderEgressProbeArgs{ShardIndex: shard, ShardCount: 256, UrlProbe: &ProviderEgressProbeBatchArgs{}}
		metrics := newProviderUrlProbeFleetRefreshCollectors(prometheus.NewPedanticRegistry())
		collector := newProviderUrlProbeFleetCollector()
		collector.refreshMetrics = metrics
		previous := &providerUrlProbeFleetSnapshot{fleet: model.ProviderUrlProbeFleet{Eligible: 1}, observedAt: time.Unix(1, 0)}
		collector.snapshot.Store(previous)
		startedAt := time.Now()
		readStarted := make(chan struct{})
		_, err := runWithProviderUrlProbeFleetHeartbeat(ctx, args, func(refreshCtx context.Context) {
			providerUrlProbeFleetHeartbeat(args)(refreshCtx)
			collector.refresh(refreshCtx, startedAt, func(snapshotCtx context.Context, _ time.Time) model.ProviderUrlProbeFleet {
				close(readStarted)
				<-snapshotCtx.Done()
				return model.ProviderUrlProbeFleet{Eligible: 99}
			})
		}, func() (*ProviderEgressProbeResult, error) {
			<-readStarted
			return &ProviderEgressProbeResult{}, nil
		})
		if err != nil || time.Since(startedAt) != 10*time.Second || collector.snapshot.Load() != previous || urlOwnerTestGauge(t, shard) != 0 {
			t.Errorf("bounded census changed task result, budget, retained snapshot or owner: elapsed=%s error=%v", time.Since(startedAt), err)
		}
		if testutil.ToFloat64(metrics.results.WithLabelValues("context_deadline")) != 1 || testutil.ToFloat64(metrics.results.WithLabelValues("context_canceled")) != 0 || testutil.ToFloat64(metrics.results.WithLabelValues("success")) != 0 {
			t.Error("slow census lost its real deadline classification")
		}
	})
}

// Non-URL legacy callers keep the immediate pass-end cancellation contract;
// they do not inherit the URL collector's explicit ten-second read budget.
func TestProviderUrlProbeLegacyRefreshStillCancelsAtPassEnd(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		startedAt := time.Now()
		readStarted := make(chan struct{})
		var refreshCtx context.Context
		_, err := runWithProviderUrlProbeFleetHeartbeat(context.Background(), &ProviderEgressProbeArgs{}, func(ctx context.Context) {
			refreshCtx = ctx
			close(readStarted)
			<-ctx.Done()
		}, func() (*ProviderEgressProbeResult, error) {
			<-readStarted
			return &ProviderEgressProbeResult{}, nil
		})
		if err != nil || refreshCtx.Err() != context.Canceled || time.Since(startedAt) != 0 {
			t.Errorf("legacy refresh lifetime changed: elapsed=%s error=%v", time.Since(startedAt), err)
		}
	})
}
