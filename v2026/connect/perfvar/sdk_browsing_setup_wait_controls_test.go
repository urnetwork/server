package perfvar

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// Use the canonical six-owner public views and the actual readiness predicate.
// Only exit health changes in these controls; structural ownership stays fixed.
func sdkBrowsingSetupWaitControl(t *testing.T) sdkBrowsingSetupSnapshot {
	t.Helper()
	ready, exits, status, locations := sdkBrowsingSetupControlViews(t)
	exits.Get(0).Warning = true
	return sdkBrowsingSetupSnapshotFrom(exits, status, locations, ready)
}

func TestSdkBrowsingSetupWaitRefreshesExitHealth(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		initial := sdkBrowsingSetupWaitControl(t)
		settings := defaultSdkBrowsingSetupWaitSettings()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		structuralChanged := make(chan struct{}, 1)
		discoveryChanged := make(chan struct{}, 1)
		done := make(chan struct {
			snapshot sdkBrowsingSetupSnapshot
			err      error
		}, 1)
		var stateLock sync.Mutex
		current := initial
		reads := 0
		go func() {
			snapshot, err := waitSdkBrowsingSetupInitialSnapshot(ctx, settings, structuralChanged, discoveryChanged, func() sdkBrowsingSetupSnapshot {
				stateLock.Lock()
				defer stateLock.Unlock()
				reads++
				return current
			}, func() *sdkBrowsingSetupEmptyReceipt { return nil })
			done <- struct {
				snapshot sdkBrowsingSetupSnapshot
				err      error
			}{snapshot: snapshot, err: err}
		}()
		synctest.Wait()
		initialReads, ready := func() (int, sdkBrowsingSetupSnapshot) {
			stateLock.Lock()
			defer stateLock.Unlock()
			initialReads := reads
			current.Warnings = 0
			return initialReads, current
		}()
		if initialReads != 1 || initial.Warnings != 1 || ready.Warnings != 0 {
			t.Fatalf("initial warning did not park the actual SDK32 setup waiter: reads=%d initial=%s ready=%s", initialReads, initial, ready)
		}

		// Warning and quarantine changes have no public structural callback.
		// Require one bounded health-read interval, not the setup deadline.
		time.Sleep(settings.ExitHealthReadInterval)
		synctest.Wait()
		select {
		case result := <-done:
			if result.err != nil || ctx.Err() != nil || result.snapshot != ready {
				t.Fatalf("health refresh did not accept the unchanged ready census: snapshot=%s err=%v ctx=%v", result.snapshot, result.err, ctx.Err())
			}
		default:
			// Prove that the exact same ready census succeeds once an
			// unrelated event wakes the old loop, and join before RED.
			structuralChanged <- struct{}{}
			synctest.Wait()
			result := <-done
			if result.err != nil || result.snapshot != ready || ctx.Err() != nil {
				t.Fatalf("structural-event positive control failed: snapshot=%s err=%v ctx=%v", result.snapshot, result.err, ctx.Err())
			}
			t.Fatal("SDK_BROWSING_SETUP_EXIT_HEALTH_WAKE_LOST: warning cleared without a structural or discovery event; actual SDK32 setup waiter remained blocked until an unrelated event")
		}
	})
}

func TestSdkBrowsingSetupWaitKeepsWarningBlocking(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		row := sdkBrowsingSetupWaitControl(t)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, err := waitSdkBrowsingSetupInitialSnapshot(ctx, defaultSdkBrowsingSetupWaitSettings(), make(chan struct{}), make(chan struct{}), func() sdkBrowsingSetupSnapshot { return row }, nil)
			done <- err
		}()
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("setup accepted a remaining warning: %v", err)
		default:
		}
		cancel()
		synctest.Wait()
		if err := <-done; !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "warnings=1") {
			t.Fatalf("warning wait did not retain cancellation and census: %v", err)
		}
	})
}

func TestSdkBrowsingSetupWaitRejectsForeignOwnership(t *testing.T) {
	row := sdkBrowsingSetupWaitControl(t)
	row.Warnings, row.ForeignProviders = 0, 1
	_, err := waitSdkBrowsingSetupInitialSnapshot(t.Context(), defaultSdkBrowsingSetupWaitSettings(), make(chan struct{}), make(chan struct{}), func() sdkBrowsingSetupSnapshot { return row }, nil)
	if err == nil || !strings.Contains(err.Error(), "SDK_BROWSING_SETUP_OWNERSHIP") {
		t.Fatalf("setup accepted foreign ownership: %v", err)
	}
}

func TestSdkBrowsingSetupWaitCancellationStaysFinal(t *testing.T) {
	row := sdkBrowsingSetupWaitControl(t)
	row.Warnings = 0
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := waitSdkBrowsingSetupInitialSnapshot(ctx, defaultSdkBrowsingSetupWaitSettings(), make(chan struct{}), make(chan struct{}), func() sdkBrowsingSetupSnapshot { return row }, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ready census overrode the owner cancellation: %v", err)
	}
}
