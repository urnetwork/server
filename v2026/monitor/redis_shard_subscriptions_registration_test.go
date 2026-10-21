// Baseline-compatible registration RED: this file uses only existing APIs.
package monitor

import (
	"testing"
	"time"
)

// Overlay this file alone to reproduce the absent probe at its registry boundary;
// compilation failure in other new-source tests is not the expected RED.
func TestRedisShardSubscriptionsRegisteredCensus(t *testing.T) {
	matches := 0
	for _, signal := range NewSignals() {
		if signal.Key() != "redis-shard-subscriptions" {
			continue
		}
		matches++
		if signal.Number() != "3.6a" || signal.ID() != "redis/shard-subscriptions" || signal.Cadence() != 15*time.Minute {
			t.Fatal("registered sharded-subscription census does not match its owned catalog contract")
		}
	}
	if matches != 1 {
		t.Fatal("missing unique registered SIGNALS.md section 3.6a sharded-subscription census")
	}
}
