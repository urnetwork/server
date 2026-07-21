package main

import (
	"fmt"
	"strings"
	"testing"
)

// batteries are expensive escalation probes: they must run once when a finding
// trips (healthy -> broken), not on every tick while the condition holds, and
// a healthy tick re-arms them.
func TestBatteryOncePerTrip(t *testing.T) {
	latch := newBatteryLatch()
	runs := 0
	battery := func() string {
		runs += 1
		return fmt.Sprintf("battery output %d", runs)
	}

	// N consecutive broken ticks: one battery run, evidence preserved on each
	for i := 0; i < 5; i += 1 {
		evidence := latch.broken("active-pileup", battery)
		if !strings.Contains(evidence, "battery output 1") {
			t.Fatalf("tick %d evidence lost the trip output: %q", i, evidence)
		}
		if i > 0 && !strings.Contains(evidence, "battery collected once at trip") {
			t.Fatalf("tick %d evidence not annotated as cached: %q", i, evidence)
		}
	}
	if runs != 1 {
		t.Fatalf("battery ran %d times across 5 consecutive broken ticks; want 1", runs)
	}

	// keys are independent
	latch.broken("idle-in-tx", battery)
	if runs != 2 {
		t.Fatalf("independent key did not run its own battery: runs=%d", runs)
	}

	// a healthy tick re-arms: the next trip runs the battery again
	latch.healthy("active-pileup")
	evidence := latch.broken("active-pileup", battery)
	if runs != 3 || !strings.Contains(evidence, "battery output 3") {
		t.Fatalf("re-armed trip did not run a fresh battery: runs=%d evidence=%q", runs, evidence)
	}

	// healthy on an armed key is a no-op
	latch.healthy("never-tripped")
	if runs != 3 {
		t.Fatalf("healthy on an armed key ran a battery: runs=%d", runs)
	}
}
