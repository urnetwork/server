package providertunnel

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/v2026"
)

// Delay only callback delivery. All state transitions and coalescing are the
// production monitor's own implementation; publication itself never waits.
type heldLossMonitor struct {
	connect.MultiClientMonitor
	entered, release         chan struct{}
	enteredOnce, releaseOnce sync.Once
}

func (self *heldLossMonitor) unblock() { self.releaseOnce.Do(func() { close(self.release) }) }
func (self *heldLossMonitor) AddMonitorEventCallback(callback connect.MonitorEventFunction) func() {
	return self.MultiClientMonitor.AddMonitorEventCallback(func(window *connect.WindowExpandEvent, events map[connect.Id]*connect.ProviderEvent, reset bool) {
		self.enteredOnce.Do(func() { close(self.entered) })
		<-self.release
		callback(window, events, reset)
	})
}

func newHeldLossMonitor(t *testing.T, merged bool) (*connect.RemoteUserNatMultiClientMonitor, *heldLossMonitor, context.Context, func()) {
	t.Helper()
	real := connect.NewRemoteUserNatMultiClientMonitorWithDefaults()
	var monitor connect.MultiClientMonitor = real
	if merged {
		monitor = connect.NewMergedMultiClientMonitor([]connect.MultiClientMonitor{real})
	}
	held := &heldLossMonitor{MultiClientMonitor: monitor, entered: make(chan struct{}), release: make(chan struct{})}
	lost, lose := context.WithCancelCause(t.Context())
	unwatch := watchProviderPath(held, lose)
	cleanup := func() { held.unblock(); unwatch() }
	return real, held, lost, cleanup
}

// A source Removed diff is positive evidence of prior admission even when the
// listener never observed Added. Current code drops that proof and leaves the
// in-flight attempt eligible to become a false provider failure after timeout.
func TestWatchProviderPathCoalescedLoss(t *testing.T) {
	for _, merged := range []bool{false, true} {
		name := "direct"
		if merged {
			name = "merged"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				real, held, lost, cleanup := newHeldLossMonitor(t, merged)
				defer cleanup()
				id, provider := connect.NewId(), connect.NewId()
				real.AddProviderEvent(id, connect.ProviderStateInEvaluation, provider, nil, connect.IpFamilyV4Only)
				<-held.entered
				real.AddProviderEvent(id, connect.ProviderStateAdded, provider, nil, connect.IpFamilyV4Only)
				real.AddProviderEvent(id, connect.ProviderStateRemoved, provider, nil, connect.IpFamilyV4Only)
				if len(real.ProviderEvents()) != 0 {
					t.Fatal("fixture retained an active route")
				}
				held.unblock()
				synctest.Wait()
				if !errors.Is(context.Cause(lost), ErrTunnelLost) {
					t.Fatalf("coalesced admitted removal did not cancel lost path: %v", context.Cause(lost))
				}
			})
		})
	}
}

// No-route setup is still measured according to the original policy. A
// replacement keeps the path live, and another tunnel owns independent loss.
func TestWatchProviderPathCoalescedLossControls(t *testing.T) {
	t.Run("never_added", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			for _, terminal := range []connect.ProviderState{connect.ProviderStateEvaluationFailed, connect.ProviderStateNotAdded} {
				real, held, lost, cleanup := newHeldLossMonitor(t, true)
				func() {
					defer cleanup()
					id := connect.NewId()
					real.AddProviderEvent(id, connect.ProviderStateInEvaluation, id, nil, connect.IpFamilyV4Only)
					<-held.entered
					real.AddProviderEvent(id, terminal, id, nil, connect.IpFamilyV4Only)
					held.unblock()
					synctest.Wait()
					if lost.Err() != nil {
						t.Errorf("unadmitted %s manufactured loss: %v", terminal, context.Cause(lost))
					}
				}()
			}
		})
	})
	t.Run("replacement_survives", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			real, held, lost, cleanup := newHeldLossMonitor(t, true)
			defer cleanup()
			first, second, provider := connect.NewId(), connect.NewId(), connect.NewId()
			real.AddProviderEvent(first, connect.ProviderStateInEvaluation, provider, nil, connect.IpFamilyV4Only)
			<-held.entered
			real.AddProviderEvent(first, connect.ProviderStateAdded, provider, nil, connect.IpFamilyV4Only)
			real.AddProviderEvent(second, connect.ProviderStateAdded, provider, nil, connect.IpFamilyV4Only)
			real.AddProviderEvent(first, connect.ProviderStateRemoved, provider, nil, connect.IpFamilyV4Only)
			held.unblock()
			synctest.Wait()
			if lost.Err() != nil {
				t.Fatal("coalesced removal erased live replacement")
			}
			real.AddProviderEvent(second, connect.ProviderStateRemoved, provider, nil, connect.IpFamilyV4Only)
			synctest.Wait()
			if !errors.Is(context.Cause(lost), ErrTunnelLost) {
				t.Fatal("last replacement removal did not lose path")
			}
		})
	})
	t.Run("independent_tunnels", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			first, firstHeld, firstLost, firstCleanup := newHeldLossMonitor(t, true)
			defer firstCleanup()
			second, secondHeld, secondLost, secondCleanup := newHeldLossMonitor(t, true)
			defer secondCleanup()
			firstHeld.unblock()
			secondHeld.unblock()
			firstId, secondId := connect.NewId(), connect.NewId()
			first.AddProviderEvent(firstId, connect.ProviderStateAdded, firstId, nil, connect.IpFamilyV4Only)
			second.AddProviderEvent(secondId, connect.ProviderStateInEvaluation, secondId, nil, connect.IpFamilyV4Only)
			synctest.Wait()
			first.AddProviderEvent(firstId, connect.ProviderStateRemoved, firstId, nil, connect.IpFamilyV4Only)
			synctest.Wait()
			if !errors.Is(context.Cause(firstLost), ErrTunnelLost) || secondLost.Err() != nil {
				t.Fatal("one tunnel borrowed another's loss")
			}
		})
	})
}
