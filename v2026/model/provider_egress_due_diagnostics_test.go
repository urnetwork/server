package model

// Pure diagnostics tests pin the strict expiry clock and absent-deadline lane.

import (
	"testing"
	"time"
)

func TestProviderEgressDueDiagnosticsExpiryBoundary(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	diagnostics := ProviderEgressDueDiagnostics{}
	for lane := ProviderEgressDueNoLocation; lane < ProviderEgressDueLaneCount; lane++ {
		diagnostics.record(lane, now.Add(-time.Nanosecond), now)
		diagnostics.record(lane, now, now)
		diagnostics.record(lane, now.Add(time.Nanosecond), now)
		want := ProviderEgressDueCount{Current: 2, Expired: 1}
		if lane == ProviderEgressDueNoLocation {
			want = ProviderEgressDueCount{Current: 3}
		}
		if diagnostics.Selected[lane] != want {
			t.Fatalf("lane %d: got %+v, want %+v", lane, diagnostics.Selected[lane], want)
		}
	}
}
