// Live-route assertions distinguish prior outage activity from the current
// measurement without relying on transport shutdown scheduling.
package perfvar

import (
	"testing"

	clientconnect "github.com/urnetwork/connect/v2026"
)

// A completed network-change fallback remains in the same client's lifetime
// counters even after a new direct carrier successfully sends the workload.
func TestLiveP2pDirectIntervalAllowsPriorFallback(t *testing.T) {
	before := clientconnect.P2pDataPlaneStatsSnapshot{
		FastSendMessageCount: 4,
		FastFallbackCount:    1,
	}
	after := before
	after.FastSendMessageCount += 8
	if err := verifyLiveP2pDirectInterval(before, after); err != nil {
		t.Fatalf("restored direct traffic was rejected for the earlier outage: %v", err)
	}
}

// A new fallback must still fail the direct-traffic assertion, whether or not
// the same client encountered a fallback before measurement started.
func TestLiveP2pDirectIntervalRejectsNewFallback(t *testing.T) {
	for _, previousFallbackCount := range []uint64{0, 1} {
		before := clientconnect.P2pDataPlaneStatsSnapshot{
			FastSendMessageCount: 4,
			FastFallbackCount:    previousFallbackCount,
		}
		after := before
		after.FastSendMessageCount += 8
		after.FastFallbackCount += 1
		if err := verifyLiveP2pDirectInterval(before, after); err == nil {
			t.Fatalf("new fallback was accepted after %d prior fallbacks", previousFallbackCount)
		}
	}
}

// Successful direct setup is not evidence that a later measured workload used
// the direct carrier; that interval must produce its own direct sends.
func TestLiveP2pDirectIntervalRequiresNewTraffic(t *testing.T) {
	for _, previousFallbackCount := range []uint64{0, 1} {
		before := clientconnect.P2pDataPlaneStatsSnapshot{
			FastSendMessageCount: 4,
			FastFallbackCount:    previousFallbackCount,
		}
		if err := verifyLiveP2pDirectInterval(before, before); err == nil {
			t.Fatalf("setup-only traffic was accepted after %d prior fallbacks", previousFallbackCount)
		}
	}
}
