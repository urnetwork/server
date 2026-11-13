package providertunnel

import (
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// Capture only bubble goroutines before creating any fixture owner. This
// identifies the existing testing/runtime harness without guessing its frames.
func routeTestBubbleWorkers(t *testing.T) map[string]string {
	t.Helper()
	stack := make([]byte, 1<<20)
	n := runtime.Stack(stack, true)
	if n == len(stack) {
		t.Fatal("goroutine evidence exceeds finite cap")
	}
	workers := map[string]string{}
	for _, goroutine := range strings.Split(string(stack[:n]), "\n\n") {
		header, _, _ := strings.Cut(goroutine, "\n")
		if !strings.Contains(header, "synctest bubble ") {
			continue
		}
		fields := strings.Fields(header)
		if len(fields) < 3 || fields[0] != "goroutine" {
			t.Fatal("unknown bubble goroutine header")
		}
		workers[fields[1]] = goroutine
	}
	return workers
}

// This runs after the measured clock is captured and cache/server owners join.
// The existing canceled-response release policy can leave only net/http's
// legacy Client.Timeout helper alive until its original timeout. It is not a
// dial, request body, TLS worker or route owner; refuse any such hidden worker.
func drainRouteTestHttpTimers(t *testing.T, bound time.Duration, harness map[string]string) {
	t.Helper()
	synctest.Wait()
	if len(harness) == 0 || len(harness) > 8 || bound <= 0 || bound > time.Minute {
		t.Fatal("test harness or HTTP timer bound unavailable")
	}
	timers := 0
	for id, goroutine := range routeTestBubbleWorkers(t) {
		if _, present := harness[id]; present {
			continue
		}
		if !strings.Contains(goroutine, "net/http.setRequestCancel.func4") {
			t.Errorf("unexpected post-close bubble worker: %s", goroutine)
		} else {
			timers++
		}
	}
	if timers > 32 {
		t.Error("legacy HTTP timer count exceeds finite fixture cap")
	}
	if timers != 0 {
		// Teardown-only virtual time: never included in DNS or URL duration.
		time.Sleep(bound)
		synctest.Wait()
	}
	for id, goroutine := range routeTestBubbleWorkers(t) {
		if _, present := harness[id]; !present {
			t.Errorf("worker survived original HTTP timer bound: %s", goroutine)
		}
	}
	t.Logf("post_close_legacy_http_timers=%d teardown_timeout_bound=%s", timers, bound)
}
