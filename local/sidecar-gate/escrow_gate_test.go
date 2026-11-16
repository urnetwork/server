package main

// §19.11's escrow_draw_order rule (OPS-M6-01): on a fixture config checkout, the switch on in a release env before
// MG6's marker fails the gate, on after it passes, off passes, and local and canary, whose builds link the pending
// groups, are not read.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A config checkout whose envs' sidecar.yml switch escrow_draw_order as given.
func escrowGateConfig(t *testing.T, envs map[string]bool) string {
	t.Helper()
	root := t.TempDir()
	for env, on := range envs {
		directory := filepath.Join(root, env)
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		text := "sidecar:\n  escrow_draw_order: {enabled: false}\n"
		if on {
			text = "sidecar:\n  escrow_draw_order: {enabled: true}\n"
		}
		if err := os.WriteFile(filepath.Join(directory, "sidecar.yml"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestEscrowDrawOrderOnBeforeMg6sMarkerFailsTheGate(t *testing.T) {
	unregistered := func() bool { return false }
	registered := func() bool { return true }

	on := escrowGateConfig(t, map[string]bool{"main": true, "local": true})
	result := escrowDrawOrderCheck(on, unregistered)
	if result.outcome != gateFail || !strings.Contains(strings.Join(result.details, "\n"), "main: on before M6's Gn-out registered MG6's index") {
		t.Fatalf("on before the marker: %+v", result)
	}
	if strings.Contains(strings.Join(result.details, "\n"), "local") {
		t.Fatalf("a pending env was read: %+v", result)
	}
	if result := escrowDrawOrderCheck(on, registered); result.outcome != gatePass {
		t.Fatalf("on after the marker: %+v", result)
	}
	off := escrowGateConfig(t, map[string]bool{"main": false, "canary": true})
	if result := escrowDrawOrderCheck(off, unregistered); result.outcome != gatePass {
		t.Fatalf("off: %+v", result)
	}
	if result := escrowDrawOrderCheck(filepath.Join(t.TempDir(), "absent"), unregistered); result.outcome != gateOpen {
		t.Fatalf("no checkout: %+v", result)
	}
}

// The checkout's own config holds escrow_draw_order off while MG6 is pending, so today's gates pass the rule.
func TestEscrowDrawOrderRuleHoldsOnTheConfigCheckout(t *testing.T) {
	root := filepath.Join("..", "..", "..", "config")
	if _, err := os.Stat(filepath.Join(root, "main", "sidecar.yml")); err != nil {
		t.Skipf("config is not beside this checkout: %v", err)
	}
	if result := escrowDrawOrderCheck(root, func() bool { return false }); result.outcome != gatePass {
		t.Fatalf("the config checkout: %+v", result)
	}
}
