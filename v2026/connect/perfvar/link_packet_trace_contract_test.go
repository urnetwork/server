package perfvar

import (
	"context"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestLinkPacketTraceRealSchedulerBoundaries(t *testing.T) {
	for _, mode := range []string{"delivered", "loss-drop", "queue-drop", "mtu-drop", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			profile := newLinkProfile(1_000_000_000, 0, 0, 0, time.Millisecond)
			switch mode {
			case "loss-drop":
				profile.LossModel, profile.LossProbability = lossModelIndependent, 1
			case "queue-drop":
				profile.QueuePacketCount = 0
			case "mtu-drop":
				profile.OuterMtu = 1
			case "canceled":
				profile.BaseDelay = time.Hour
			}
			link := newDirectionalLink(ctx, profile, 1, func(packet []byte) bool { clear(packet); return true })
			defer link.close()
			var mu sync.Mutex
			var stages []string
			scheduled := make(chan struct{})
			cleanup, ok := link.installPacketTraceForTest(func(stage string, observation linkScheduleObservation, packet []byte) {
				if stage == "delivered" && packet != nil {
					t.Error("post-handoff observer borrowed bytes")
				}
				if stage == "scheduled" && (observation.rateReadyTime.IsZero() || observation.releaseTime.IsZero() || observation.sequence == 0) {
					t.Error("schedule arithmetic omitted")
				}
				mu.Lock()
				stages = append(stages, stage)
				mu.Unlock()
				if stage == "scheduled" {
					close(scheduled)
				}
			})
			if !ok {
				t.Fatal("trace ownership refused")
			}
			defer cleanup()
			if _, err := link.submit([]byte{1, 2, 3}); err != nil {
				t.Fatal(err)
			}
			if mode == "canceled" {
				select {
				case <-scheduled:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				link.close()
			} else if !waitForDirectionalLinksTerminalIdle(ctx, []*directionalLink{link}, nil) {
				t.Fatal("link did not terminate")
			}
			cleanup()
			want := []string{mode}
			if mode == "delivered" {
				want = []string{"scheduled", "delivery-offer", "delivered"}
			}
			if mode == "loss-drop" || mode == "canceled" {
				want = []string{"scheduled", mode}
			}
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(stages, want) {
				t.Fatalf("boundaries=%v want=%v", stages, want)
			}
		})
	}
}

func TestLinkPacketTraceExclusiveOwnershipAndNilCost(t *testing.T) {
	link := &directionalLink{}
	packet := []byte{1, 2, 3}
	if allocations := testing.AllocsPerRun(100, func() { link.tracePacketForTest("test", 1, packet, linkScheduleObservation{}) }); allocations != 0 {
		t.Fatalf("nil packet trace allocations=%g", allocations)
	}
	count := 0
	cleanup, ok := link.installPacketTraceForTest(func(stage string, observation linkScheduleObservation, borrowed []byte) {
		count++
		if stage != "test" || observation.sequence != 7 || observation.scheduleTime.IsZero() || len(borrowed) != 3 {
			t.Error("packet boundary changed")
		}
	})
	if !ok {
		t.Fatal("packet observer unavailable")
	}
	defer cleanup()
	if _, ok := link.installPacketTraceForTest(func(string, linkScheduleObservation, []byte) {}); ok {
		t.Fatal("conflicting packet observer admitted")
	}
	link.tracePacketForTest("test", 7, packet, linkScheduleObservation{})
	cleanup()
	link.tracePacketForTest("test", 7, packet, linkScheduleObservation{})
	if count != 1 {
		t.Fatalf("retired observer count=%d", count)
	}
	next, ok := link.installPacketTraceForTest(func(string, linkScheduleObservation, []byte) {})
	if !ok {
		t.Fatal("packet owner not released")
	}
	defer next()
	cleanup()
	if link.packetTraceForTest.Load() == nil {
		t.Fatal("stale cleanup cleared new owner")
	}
}

func TestLinkPacketTraceCleanupJoinsAndPanicIsContained(t *testing.T) {
	func() {
		link := &directionalLink{}
		entered, release, finished, closed := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
		cleanup, ok := link.installPacketTraceForTest(func(string, linkScheduleObservation, []byte) { close(entered); <-release; panic("diagnostic panic") })
		if !ok {
			t.Fatal("packet owner unavailable")
		}
		owner := link.packetTraceForTest.Load()
		go func() { link.tracePacketForTest("test", 1, []byte{1}, linkScheduleObservation{}); close(finished) }()
		<-entered
		go func() { cleanup(); close(closed) }()
		var releaseOnce sync.Once
		defer func() { releaseOnce.Do(func() { close(release) }); <-finished; <-closed }()
		until := time.Now().Add(2 * time.Second)
		for {
			select {
			case <-closed:
				t.Fatal("cleanup crossed active callback")
			default:
			}
			if !owner.join.TryRLock() {
				break
			}
			owner.join.RUnlock()
			if time.Now().After(until) {
				t.Fatal("cleanup never reached join")
			}
			runtime.Gosched()
		}
		releaseOnce.Do(func() { close(release) })
		<-finished
		<-closed
		if link.packetTraceForTest.Load() != nil {
			t.Fatal("panic leaked packet owner")
		}
	}()
}
