package proxy

import (
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server"
)

// A failing id is constructed at most once per cooldown, however often it is
// opened, as wgTunFactory already bounds one WireGuard peer. Every open of a
// failing new id used to re-run its Redis, PostgreSQL and MintHostedSession
// setup.
func TestProxyDeviceCreationFailureIsRetriedOncePerCooldown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := wgAdmissionTestManager(t)
		failingId, otherId := server.NewId(), server.NewId()
		setupErr := errors.New("synthetic construction failure")
		var calls atomic.Int32
		manager.proxyDeviceBuilder = func(server.Id) (*ProxyDevice, error) {
			calls.Add(1)
			return nil, setupErr
		}

		for range 64 {
			if _, err := manager.OpenProxyDevice(failingId); !errors.Is(err, setupErr) {
				t.Fatalf("open inside the cooldown = %v, want the construction failure", err)
			}
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("64 opens of a failing id constructed %d devices, want 1", got)
		}
		// the cooldown is the failing id's alone
		if _, err := manager.OpenProxyDevice(otherId); !errors.Is(err, setupErr) || calls.Load() != 2 {
			t.Fatal("one id's cooldown held back another id's construction")
		}

		// opens inside the cooldown do not extend it
		time.Sleep(deviceCreationRetryTimeout - time.Nanosecond)
		if _, err := manager.OpenProxyDevice(failingId); !errors.Is(err, setupErr) || calls.Load() != 2 {
			t.Fatal("construction retried before the cooldown ended")
		}
		time.Sleep(time.Nanosecond)
		if _, err := manager.OpenProxyDevice(failingId); !errors.Is(err, setupErr) || calls.Load() != 3 {
			t.Fatalf("construction was not retried when the cooldown ended (%d calls)", calls.Load())
		}
		if manager.DeviceCount() != 0 {
			t.Fatal("a cooling-down id retained a device entry")
		}
	})
}

// A coalesced opener that waited out a failed construction does not start
// another; it returns the failure, and so do openers that come after it.
func TestProxyDeviceCreationCooldownCoversCoalescedOpeners(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := wgAdmissionTestManager(t)
		proxyId := server.NewId()
		entered, release := make(chan struct{}), make(chan struct{})
		setupErr := errors.New("synthetic construction failure")
		var calls atomic.Int32
		manager.proxyDeviceBuilder = func(server.Id) (*ProxyDevice, error) {
			if calls.Add(1) == 1 {
				close(entered)
				<-release
			}
			return nil, setupErr
		}
		results := make(chan error, 8)
		go func() {
			_, err := manager.OpenProxyDevice(proxyId)
			results <- err
		}()
		<-entered
		for range 7 {
			go func() {
				_, err := manager.OpenProxyDevice(proxyId)
				results <- err
			}()
		}
		synctest.Wait()
		close(release)
		for range 8 {
			if err := <-results; !errors.Is(err, setupErr) {
				t.Fatalf("opener result = %v, want the construction failure", err)
			}
		}
		if _, err := manager.OpenProxyDevice(proxyId); !errors.Is(err, setupErr) || calls.Load() != 1 {
			t.Fatalf("%d constructions for one failure cohort, want 1", calls.Load())
		}
	})
}
