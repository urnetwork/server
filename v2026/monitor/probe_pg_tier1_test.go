package main

import (
	"testing"
	"time"
)

func TestPgConnectRateUsesCumulativeCounter(t *testing.T) {
	probe := &pgConnectRateProbe{}
	start := time.Unix(1000, 0)

	if _, ok := probe.observe(10_000, start); ok {
		t.Fatal("first sample must warm up")
	}
	rate, ok := probe.observe(11_200, start.Add(2*time.Minute))
	if !ok || rate != 600 {
		t.Fatalf("two-minute rate = %d, ok=%t; want 600/min", rate, ok)
	}

	// PostgreSQL statistics reset/restart: re-warm rather than emitting a
	// large negative/zero incident.
	if _, ok := probe.observe(25, start.Add(3*time.Minute)); ok {
		t.Fatal("counter reset must warm up")
	}
	rate, ok = probe.observe(125, start.Add(4*time.Minute))
	if !ok || rate != 100 {
		t.Fatalf("post-reset rate = %d, ok=%t; want 100/min", rate, ok)
	}
}
