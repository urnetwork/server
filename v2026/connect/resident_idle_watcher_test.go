package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
)

// The real SDK callback fixture and production idle watcher run in a virtual
// time bubble. This tests the full production allowance without a five-minute
// wall-clock wait or a replacement timer/expiry implementation.
func TestResidentIdleWatcherUsesExactProductionAllowance(t *testing.T) {
	// The SDK owns one process-wide pool statistics worker. Start that global
	// owner outside the bubble so it is not mistaken for a resident worker.
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := DefaultExchangeSettings()
		if settings.ExchangeResidentTtl != 300*time.Second {
			t.Fatal("test requires the unchanged production idle allowance")
		}
		resident := newResidentCallbackLifecycleFixture(t, parent, settings)
		defer func() {
			if err := resident.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		resident.UpdateActivity()
		done := make(chan struct{})
		go func() { defer close(done); resident.runIdleWatcher() }()
		synctest.Wait()
		time.Sleep(10 * time.Second)
		if !resident.UpdateActivity() {
			t.Fatal("healthy resident rejected activity")
		}
		time.Sleep(290 * time.Second)
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("old timer shortened the latest activity allowance")
		default:
		}
		time.Sleep(10 * time.Second)
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("idle retirement rounded310s up to600s")
		}
		if parent.Err() != nil {
			t.Fatal("resident expiry canceled its parent")
		}
	})
}

func TestResidentIdleWatcherParentCancellationJoins(t *testing.T) {
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		resident := newResidentCallbackLifecycleFixture(t, parent, DefaultExchangeSettings())
		defer func() {
			if err := resident.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		resident.UpdateActivity()
		done := make(chan struct{})
		go func() { defer close(done); resident.runIdleWatcher() }()
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("parent cancellation waited for the idle timer")
		}
	})
}
