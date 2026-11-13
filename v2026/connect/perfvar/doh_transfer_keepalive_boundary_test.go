//go:build acklineagetrace

package perfvar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"
)

const dohTransferKeepaliveBoundary = "request-source-pack-carrier-before-cache-retirement-v1"

type dohTransferCleanupCounters struct {
	DevicePackFailures, ProviderPackFailures         uint64
	DeviceWorkloadFailures, ProviderWorkloadFailures uint64
	DeviceTrackerInvalid, ProviderTrackerInvalid     bool
}

type dohTransferCleanupObservation struct {
	CacheCloseReturned, PathCloseReturned bool
	PathCloseDuration                     time.Duration
	Error                                 string `json:",omitempty"`
	Before, After                         dohTransferCleanupCounters
}

func captureDohTransferCleanup(result *dohTransferCleanupObservation, closePath func() error, snapshot func() dohTransferCleanupCounters) {
	started := time.Now()
	err := closePath()
	result.PathCloseReturned, result.PathCloseDuration = true, time.Since(started)
	if err != nil {
		result.Error = err.Error()
		if len(result.Error) > 2048 {
			result.Error = result.Error[:2048] + " [truncated]"
		}
	}
	result.After = snapshot()
}

func TestDohTransferKeepaliveCleanupOutcomeIsSeparate(t *testing.T) {
	for _, failed := range []bool{false, true} {
		result := &dohTransferCleanupObservation{CacheCloseReturned: true, Before: dohTransferCleanupCounters{DevicePackFailures: 2}}
		var calls []string
		captureDohTransferCleanup(result, func() error {
			calls = append(calls, "close")
			if failed {
				return errors.New("bounded teardown join failed")
			}
			return nil
		}, func() dohTransferCleanupCounters {
			calls = append(calls, "snapshot")
			return dohTransferCleanupCounters{DevicePackFailures: 5, ProviderWorkloadFailures: 1}
		})
		if !reflect.DeepEqual(calls, []string{"close", "snapshot"}) || !result.CacheCloseReturned || !result.PathCloseReturned ||
			result.Before.DevicePackFailures != 2 || result.After.DevicePackFailures != 5 || result.After.ProviderWorkloadFailures != 1 || (result.Error != "") != failed {
			t.Fatal("post-request cleanup loss was hidden or folded into the sealed query verdict")
		}
	}
}

// Keep-alive queries use the identical source/Pack/carrier fixed-point join,
// with the same caller-owned deadline. Only explicit cache retirement moves
// outside the measured request verdict. measureDohTransferExperiment already
// defers op.close before warmup, so all exits still close the cache, origin,
// and listener. Historical scenarios keep their post-close join untouched.
func joinDohTransferRequestBoundary(ctx context.Context, policy string, retire func(), join func(context.Context) error) error {
	switch policy {
	case "":
		retireDohTransferOperation(retire)
	case dohTransferKeepaliveBoundary, dohTransferPrefixBoundary:
	default:
		return fmt.Errorf("unknown DoH request boundary policy %q", policy)
	}
	// Preserve the historical ordering: the fixed join budget starts after
	// explicit retirement in post-close scenarios, not before Close returns.
	endCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return join(endCtx)
}

func TestDohTransferKeepaliveBoundaryOrderingFailureAndCleanup(t *testing.T) {
	for _, policy := range []string{"", dohTransferKeepaliveBoundary, dohTransferPrefixBoundary} {
		for _, fail := range []bool{false, true} {
			var calls []string
			wantErr := errors.New("exact source/Pack/carrier owner still pending")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			deadline, _ := ctx.Deadline()
			var gotErr error
			func() {
				// This is the same unconditional operation cleanup registered
				// before warmup in measureDohTransferExperiment.
				defer func() { calls = append(calls, "cleanup") }()
				gotErr = joinDohTransferRequestBoundary(ctx, policy, func() { calls = append(calls, "retire") }, func(got context.Context) error {
					calls = append(calls, "join")
					if end, ok := got.Deadline(); !ok || end != deadline {
						t.Fatal("request boundary replaced/extended the original deadline")
					}
					if fail {
						return wantErr
					}
					return nil
				})
				calls = append(calls, "seal-verdict")
			}()
			want := []string{"join", "seal-verdict", "cleanup"}
			if policy == "" {
				want = append([]string{"retire"}, want...)
			}
			if !reflect.DeepEqual(calls, want) || fail && gotErr != wantErr || !fail && gotErr != nil {
				t.Fatalf("policy=%q fail=%t calls=%v err=%v", policy, fail, calls, gotErr)
			}
		}
	}
	called := false
	if err := joinDohTransferRequestBoundary(context.Background(), "skip-all-ownership", func() { called = true }, func(context.Context) error { called = true; return nil }); err == nil || called {
		t.Fatal("unknown policy bypassed ownership")
	}
}

func TestDohTransferKeepaliveIdentityAndLegacyPreservation(t *testing.T) {
	if err := dohTerminalLineageIdentity(); err != nil {
		t.Fatal("historical exact post-close identity changed:", err)
	}
	for _, suffix := range []string{"", "-burst-1"} {
		legacy, err := dohTransferScenarioNamed("established-rtt3s" + suffix)
		if err != nil {
			t.Fatal(err)
		}
		current, err := dohTransferScenarioNamed("established-rtt3s-keepalive" + suffix)
		if err != nil || legacy.RequestBoundaryPolicy != "" || legacy.Version != 3 || current.Version != 4 || current.RequestBoundaryPolicy != dohTransferKeepaliveBoundary {
			t.Fatal("new request-only scenario relabeled historical close evidence")
		}
		normalized := current
		normalized.Name, normalized.Version, normalized.RequestBoundaryPolicy = legacy.Name, legacy.Version, legacy.RequestBoundaryPolicy
		if !reflect.DeepEqual(normalized, legacy) {
			t.Fatal("keep-alive policy changed route, loss, timers, resources or ACK ownership")
		}
		for run := 13; run <= 22; run++ {
			trace, err := dohTransferTrace(current, run)
			oldTrace, _ := dohTransferTrace(legacy, run)
			if err != nil || trace == oldTrace {
				t.Fatal("new request-only policy reused historical trace identity")
			}
			if os.Getenv("CONNECT_PERFVAR_DOH_TRANSFER_EMIT_IDENTITIES") == "1" {
				encoded, err := json.Marshal(struct {
					Scenario     dohTransferScenario `json:"scenario"`
					ScenarioHash string              `json:"scenario_hash"`
					ProfileHash  string              `json:"profile_hash"`
					Trace        perfvarTrace        `json:"trace"`
					RunIndex     int                 `json:"run_index"`
				}{current, tcpRecoveryHash(current), tcpRecoveryHash([]networkProfile{current.DeviceAccess, current.ProviderAccess}), trace, run})
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("[doh-keepalive-identity] %s", encoded)
			}
		}
	}
	if _, err := dohTransferScenarioNamed("established-rtt3s-keepalive-ignore-loss"); err == nil {
		t.Fatal("unknown keep-alive profile silently became clean")
	}
}
