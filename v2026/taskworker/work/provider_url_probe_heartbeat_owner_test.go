package work

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	dto "github.com/prometheus/client_model/go"
)

func urlOwnerTestGauge(t *testing.T, shard int) float64 {
	t.Helper()
	var sample dto.Metric
	if err := urlProbeShardObservedAt.WithLabelValues(strconv.Itoa(shard)).Write(&sample); err != nil {
		t.Fatal(err)
	}
	return sample.GetGauge().GetValue()
}

// Exercise the actual publisher and wrapper. No provider, socket, database,
// real-time sleep, or observed goroutine stack supplies the ordering.
func TestProviderUrlProbeHeartbeatOwnerRetiresOnEveryExit(t *testing.T) {
	for index, mode := range []string{"success", "error", "cancel", "panic"} {
		t.Run(mode, func(t *testing.T) {
			shard := 240 + index
			t.Cleanup(func() { urlProbeShardObservedAt.DeleteLabelValues(strconv.Itoa(shard)) })
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				args := &ProviderEgressProbeArgs{ShardIndex: shard, ShardCount: 256, UrlProbe: &ProviderEgressProbeBatchArgs{}}
				refresh := providerUrlProbeFleetHeartbeat(args)
				started, release := make(chan struct{}), make(chan struct{})
				type result struct {
					err        error
					panicValue any
				}
				completed := make(chan result, 1)
				go func() {
					got := result{}
					defer func() { got.panicValue = recover(); completed <- got }()
					_, got.err = runWithProviderUrlProbeFleetHeartbeat(ctx, args, refresh, func() (*ProviderEgressProbeResult, error) {
						close(started)
						<-release
						switch mode {
						case "error":
							return nil, errors.New("synthetic pass failure")
						case "cancel":
							return nil, ctx.Err()
						case "panic":
							panic("synthetic pass panic")
						}
						// Match the actual pass's final successful heartbeat.
						refresh(ctx)
						return &ProviderEgressProbeResult{}, nil
					})
				}()
				<-started
				synctest.Wait()
				initial := urlOwnerTestGauge(t, shard)
				if initial <= 0 {
					t.Fatal("active URL owner was not published")
				}
				time.Sleep(4 * time.Minute)
				synctest.Wait()
				if got := urlOwnerTestGauge(t, shard); got <= initial || float64(time.Now().Unix())-got > 60 {
					t.Fatal("long active URL owner lost its heartbeat")
				}
				if mode == "cancel" {
					cancel()
				}
				close(release)
				got := <-completed
				// Also release the old baseline panic path's unjoined refresh worker.
				cancel()
				synctest.Wait()
				if (got.panicValue != nil) != (mode == "panic") {
					t.Fatalf("panic propagation changed: %v", got.panicValue)
				}
				if (got.err != nil) != (mode == "error" || mode == "cancel") {
					t.Fatalf("error propagation changed: %v", got.err)
				}
				if value := urlOwnerTestGauge(t, shard); value != 0 {
					t.Errorf("completed URL pass remains a fresh shard owner: heartbeat=%v", value)
				}
				time.Sleep(2 * time.Minute)
				synctest.Wait()
				if value := urlOwnerTestGauge(t, shard); value != 0 {
					t.Error("retired URL owner was republished")
				}
			})
		})
	}
}

// Even an unexpected same-process overlap must retain the active invocation.
// Ordinary cross-process handoff clears only the predecessor's own series.
func TestProviderUrlProbeHeartbeatHandoffPreservesActiveOwner(t *testing.T) {
	const shard = 244
	t.Cleanup(func() { urlProbeShardObservedAt.DeleteLabelValues(strconv.Itoa(shard)) })
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		args := &ProviderEgressProbeArgs{ShardIndex: shard, ShardCount: 256, UrlProbe: &ProviderEgressProbeBatchArgs{}}
		launch := func() (chan struct{}, chan struct{}) {
			release, done := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(done)
				runWithProviderUrlProbeFleetHeartbeat(ctx, args, providerUrlProbeFleetHeartbeat(args), func() (*ProviderEgressProbeResult, error) {
					<-release
					return &ProviderEgressProbeResult{}, nil
				})
			}()
			synctest.Wait()
			return release, done
		}
		firstRelease, firstDone := launch()
		secondRelease, secondDone := launch()
		close(firstRelease)
		<-firstDone
		synctest.Wait()
		if urlOwnerTestGauge(t, shard) <= 0 {
			t.Error("retiring predecessor erased an active URL owner")
		}
		before := urlOwnerTestGauge(t, shard)
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if urlOwnerTestGauge(t, shard) <= before {
			t.Error("remaining active URL owner stopped refreshing")
		}
		close(secondRelease)
		<-secondDone
		synctest.Wait()
		if urlOwnerTestGauge(t, shard) != 0 {
			t.Error("last completed URL owner retained its heartbeat")
		}
	})
}
