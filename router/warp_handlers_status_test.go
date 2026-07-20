package router

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"
)

// warpctlIsErrorRegex pins the warpctl deploy-poll failure predicate
// (`warp/warpctl/docker.go` WarpStatusResponse.IsError): a status matching
// this fails the health poll, so the deploy times out and reverts. The
// latched statuses must agree with it by construction: "error not ready ..."
// fails the poll; "ok" and "draining" do not.
var warpctlIsErrorRegex = regexp.MustCompile("^(?i)error\\s")

func setWarpStatusTestEnv(t *testing.T) {
	// WarpStatus reports env/service/block; test.sh exports these, and the
	// setenv makes the test self-sufficient standalone
	t.Setenv("WARP_ENV", "local")
	t.Setenv("WARP_SERVICE", "test")
	t.Setenv("WARP_BLOCK", "test")
	t.Setenv("WARP_VERSION", "0.0.0")
}

func getWarpStatus(t *testing.T) *WarpStatusResult {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "http://test/status", nil)
	WarpStatus(w, r)
	if w.Code != 200 {
		t.Fatalf("status http code = %d", w.Code)
	}
	var result WarpStatusResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("status json: %s", err)
	}
	return &result
}

func TestWarpStatusLatch(t *testing.T) {
	setWarpStatusTestEnv(t)
	// leave the latch in the ready state for any later test in this process
	t.Cleanup(SetWarpStatusReady)

	// services that never call the setters keep the historical constant "ok"
	result := getWarpStatus(t)
	if result.Status != "ok" {
		t.Fatalf("default status = %q", result.Status)
	}
	if warpctlIsErrorRegex.MatchString(result.Status) {
		t.Fatalf("default status reads as a poll failure: %q", result.Status)
	}

	// a failed readiness check must fail the warpctl poll so the deploy
	// reverts instead of flipping traffic to a container that cannot serve
	SetWarpStatusNotReady(errors.New("pg: connection refused"))
	result = getWarpStatus(t)
	if !warpctlIsErrorRegex.MatchString(result.Status) {
		t.Fatalf("not-ready status does not fail the warpctl poll: %q", result.Status)
	}

	// ready latches back to ok
	SetWarpStatusReady()
	result = getWarpStatus(t)
	if result.Status != "ok" {
		t.Fatalf("ready status = %q", result.Status)
	}

	// draining is informational: visible to pollers and dashboards, but
	// deliberately NOT an error status (the deploy poll never targets the
	// draining container, and fleet sampling must not count an operator
	// drain as a service error)
	SetWarpStatusDraining()
	result = getWarpStatus(t)
	if result.Status != "draining" {
		t.Fatalf("draining status = %q", result.Status)
	}
	if warpctlIsErrorRegex.MatchString(result.Status) {
		t.Fatalf("draining status reads as a poll failure: %q", result.Status)
	}
}

func TestRouterStatsFlush(t *testing.T) {
	setWarpStatusTestEnv(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stats := NewRouterStats(ctx, time.Minute)
	stats.Success("GET /a", 5*time.Millisecond)
	stats.Success("GET /a", 7*time.Millisecond)
	stats.Error("GET /b")

	// flush reports immediately (drain end) and must not destroy the live
	// bucket
	stats.Flush()

	routeStats := stats.currentRouteStats()
	if stat := routeStats["GET /a"]; stat == nil || stat.successCount != 2 {
		t.Fatalf("flush lost the live bucket success counts: %+v", stat)
	}
	if stat := routeStats["GET /b"]; stat == nil || stat.errorCount != 1 {
		t.Fatalf("flush lost the live bucket error counts: %+v", stat)
	}

	// a second flush is safe
	stats.Flush()

	// the router-level flush used at drain end
	router := NewRouter(ctx, []*Route{})
	router.FlushStats()
}

// SetWarpStatusDrainingIfReady is the signal-handler flip: it must convert a
// ready container to draining, but NEVER hide a latched not-ready error
// behind the benign "draining" — a failed container keeps reporting its
// error through SIGTERM so fleet status sampling counts it as failed.
func TestWarpStatusDrainingIfReadyPrecedence(t *testing.T) {
	setWarpStatusTestEnv(t)
	t.Cleanup(SetWarpStatusReady)

	// ready → draining flips
	SetWarpStatusReady()
	SetWarpStatusDrainingIfReady()
	result := getWarpStatus(t)
	if result.Status != "draining" {
		t.Fatalf("ready->draining flip: status = %q", result.Status)
	}

	// not-ready → draining does NOT flip: the error stays visible and still
	// fails the warpctl poll predicate
	SetWarpStatusNotReady(errors.New("redis: connection refused"))
	SetWarpStatusDrainingIfReady()
	result = getWarpStatus(t)
	if result.Status == "draining" {
		t.Fatalf("not-ready was hidden behind draining")
	}
	if !warpctlIsErrorRegex.MatchString(result.Status) {
		t.Fatalf("not-ready status lost the poll-failure form: %q", result.Status)
	}

	// the unconditional setter still overrides (operator-known-ready path)
	SetWarpStatusDraining()
	result = getWarpStatus(t)
	if result.Status != "draining" {
		t.Fatalf("unconditional draining: status = %q", result.Status)
	}
}
