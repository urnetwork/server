package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDrainable is a minimal drainable for coordinator tests: an active count
// that only the test decrements, with the same WaitIdle contract as the lib
// proxies (idle = draining AND active == 0).
type fakeDrainable struct {
	mu       sync.Mutex
	draining bool
	active   int
	notify   chan struct{}
}

func newFakeDrainable(active int) *fakeDrainable {
	return &fakeDrainable{
		active: active,
		notify: make(chan struct{}),
	}
}

func (self *fakeDrainable) Drain() {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.draining = true
	self.notifyWithLock()
}

func (self *fakeDrainable) Draining() bool {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.draining
}

func (self *fakeDrainable) ActiveCount() int {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.active
}

func (self *fakeDrainable) exitOne() {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.active -= 1
	self.notifyWithLock()
}

func (self *fakeDrainable) notifyWithLock() {
	close(self.notify)
	self.notify = make(chan struct{})
}

func (self *fakeDrainable) WaitIdle(ctx context.Context) bool {
	for {
		idle, notify := func() (bool, chan struct{}) {
			self.mu.Lock()
			defer self.mu.Unlock()
			return self.draining && self.active == 0, self.notify
		}()
		if idle {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-notify:
		}
	}
}

func drainTestSettings() *ProxySettings {
	settings := DefaultProxySettings()
	settings.DrainGraceTimeout = 5 * time.Second
	settings.DrainBeforeExitTimeout = 1 * time.Second
	return settings
}

func TestDrainCoordinatorCompletesWhenIdle(t *testing.T) {
	readiness := NewReadiness()
	readiness.SetInitialSyncDone()
	settings := drainTestSettings()

	a := newFakeDrainable(1)
	b := newFakeDrainable(2)

	coordinator := NewDrainCoordinator(readiness, settings)
	coordinator.AddTarget(a)
	coordinator.AddTarget(b)
	beforeExitRan := false
	coordinator.AddBeforeExit(func(ctx context.Context) {
		// runs after the grace wait, with the targets already idle
		if a.ActiveCount() != 0 || b.ActiveCount() != 0 {
			t.Errorf("before-exit hook ran before idle")
		}
		beforeExitRan = true
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		coordinator.Drain(ctx)
	}()

	// the coordinator flips readiness and drains every target
	waitFor(t, 2*time.Second, "targets draining", func() bool {
		return readiness.Draining() && a.Draining() && b.Draining()
	})
	if ready, reason := readiness.Status(); ready || reason != "draining" {
		t.Fatalf("status while draining = %t %q", ready, reason)
	}

	// still active: the drain must not complete
	select {
	case <-doneCh:
		t.Fatal("drain completed with active connections")
	case <-time.After(200 * time.Millisecond):
	}

	a.exitOne()
	b.exitOne()
	b.exitOne()

	select {
	case <-doneCh:
	case <-time.After(3 * time.Second):
		t.Fatal("drain did not complete after the targets went idle")
	}
	if !coordinator.Drained() {
		t.Fatal("Drained must report true after a completed drain")
	}
	if !beforeExitRan {
		t.Fatal("before-exit hook did not run")
	}
}

func TestDrainCoordinatorGraceDeadline(t *testing.T) {
	readiness := NewReadiness()
	settings := drainTestSettings()
	settings.DrainGraceTimeout = 300 * time.Millisecond

	// a target that never goes idle: the grace deadline must end the drain
	a := newFakeDrainable(1)

	coordinator := NewDrainCoordinator(readiness, settings)
	coordinator.AddTarget(a)
	beforeExitRan := false
	coordinator.AddBeforeExit(func(ctx context.Context) {
		beforeExitRan = true
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startTime := time.Now()
	coordinator.Drain(ctx)
	elapsed := time.Since(startTime)

	if elapsed < settings.DrainGraceTimeout {
		t.Fatalf("drain returned before the grace deadline (%s)", elapsed)
	}
	if 5*time.Second < elapsed {
		t.Fatalf("drain did not end promptly at the grace deadline (%s)", elapsed)
	}
	if !coordinator.Drained() {
		t.Fatal("Drained must report true after a deadline-bounded drain")
	}
	if !beforeExitRan {
		t.Fatal("before-exit hook did not run after the deadline")
	}
}

func TestDrainCoordinatorDisabled(t *testing.T) {
	readiness := NewReadiness()
	settings := drainTestSettings()
	settings.EnableDrain = false

	a := newFakeDrainable(1)

	coordinator := NewDrainCoordinator(readiness, settings)
	coordinator.AddTarget(a)
	beforeExitRan := false
	coordinator.AddBeforeExit(func(ctx context.Context) {
		beforeExitRan = true
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	coordinator.Drain(ctx)

	if a.Draining() {
		t.Fatal("targets must not be drained when the drain is disabled")
	}
	if !readiness.Draining() {
		t.Fatal("readiness must still flip to draining")
	}
	if !coordinator.Drained() {
		t.Fatal("Drained must report true")
	}
	if !beforeExitRan {
		t.Fatal("before-exit hook must still run")
	}
}

func TestReadinessStatusHandler(t *testing.T) {
	readiness := NewReadiness()

	// not ready before the initial sync
	r := httptest.NewRequest("GET", "/status", nil)
	w := httptest.NewRecorder()
	readiness.StatusHandler(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status before initial sync = %d, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "initial proxy client sync") {
		t.Fatalf("body = %q", w.Body.String())
	}

	// ready after the initial sync
	readiness.SetInitialSyncDone()
	if ready, reason := readiness.Status(); !ready || reason != "" {
		t.Fatalf("status after initial sync = %t %q", ready, reason)
	}

	// draining flips it back to 503
	readiness.SetDraining()
	w = httptest.NewRecorder()
	readiness.StatusHandler(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status while draining = %d, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "draining") {
		t.Fatalf("body = %q", w.Body.String())
	}
}
