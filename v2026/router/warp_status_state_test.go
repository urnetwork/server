// Service-owned latches keep composed runners from masking each other's
// startup failures while retaining the standalone global status contract.
package router

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

// A newly constructed service is pending regardless of its ready siblings.
func TestWarpStatusStateIsolatesComposedServices(t *testing.T) {
	first := &WarpStatusState{}
	second := &WarpStatusState{}
	if !strings.HasPrefix(first.Status(), "error not ready:") {
		t.Fatal("new service admitted before startup")
	}
	first.SetReady()
	if first.Status() != "ok" || second.Status() == "ok" {
		t.Fatal("one service's admission changed its sibling")
	}
	second.SetNotReady(errors.New("synthetic missing migration"))
	first.SetDrainingIfReady()
	if first.Status() != "draining" || !strings.Contains(second.Status(), "synthetic missing migration") {
		t.Fatal("one service's drain changed its sibling")
	}
	second.SetDrainingIfReady()
	if !strings.HasPrefix(second.Status(), "error not ready:") {
		t.Fatal("drain hid service startup failure")
	}
}

// Pending is also a failure during drain, and global readiness cannot admit
// a service that has its own latch.
func TestWarpStatusStatePendingAndGlobalAreIndependent(t *testing.T) {
	previous := warpStatusOverride.Load()
	t.Cleanup(func() { warpStatusOverride.Store(previous) })
	SetWarpStatusReady()
	state := &WarpStatusState{}
	state.SetDrainingIfReady()
	if !strings.HasPrefix(state.Status(), "error not ready:") {
		t.Fatal("pending service lost its failed readiness during drain")
	}
	state.SetNotReady(errors.New("synthetic startup failure"))
	if (*WarpStatusState)(nil).Status() != "ok" {
		t.Fatal("scoped service changed standalone global latch")
	}
	SetWarpStatusNotReady(errors.New("synthetic global failure"))
	state.SetReady()
	if !strings.Contains((*WarpStatusState)(nil).Status(), "synthetic global failure") {
		t.Fatal("scoped readiness masked global failure")
	}
}

// The actual HTTP handler reads its selected state, rather than the global
// latch that a different service might already have admitted.
func TestWarpStatusStateHandlerUsesOwnedLatch(t *testing.T) {
	t.Setenv("WARP_HOST", "operator.example")
	t.Setenv("WARP_SERVICE", "synthetic-all")
	t.Setenv("WARP_BLOCK", "synthetic-block")
	t.Setenv("WARP_VERSION", "0.0.0-test")
	state := &WarpStatusState{}
	state.SetNotReady(errors.New("synthetic worker pending"))
	response := httptest.NewRecorder()
	state.Handler(response, httptest.NewRequest("GET", "http://operator.example/status", nil))
	var result WarpStatusResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || result.Status != state.Status() || result.Host != "operator.example" {
		t.Fatalf("owned status mismatch: code=%d result=%+v", response.Code, result)
	}
}
