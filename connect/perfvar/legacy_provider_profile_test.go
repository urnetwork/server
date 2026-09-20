package perfvar

import (
	"strings"
	"testing"
	"time"
)

// Use the exact resolver called before sweep fixture construction. The
// initial-only catalog omitted cell-edge, yielding an all-zero profile and
// zero edge-TUN MTU before either provider capability arm could connect.
// This is a pure configuration test: no database, simulator or socket is used.
func TestPerfvarLegacyProviderCompatibilityProfiles(t *testing.T) {
	const seed int64 = 20260919
	for _, row := range []struct {
		name             string
		forward, reverse int64
		mtu              int
		delay            time.Duration
	}{
		{cellEdge5mDown1mUpName, 1_000_000, 5_000_000, 1400, 60 * time.Millisecond},
		{"single-region-1000ms-rtt", 100_000_000, 100_000_000, 1500, 500 * time.Millisecond},
		{"clean-lan", 1_000_000_000, 1_000_000_000, 1500, time.Millisecond},
	} {
		t.Run(row.name, func(t *testing.T) {
			profile, err := perfvarLegacyProviderProfile(seed, row.name)
			if err != nil {
				t.Fatalf("sweep cannot resolve its declared profile before setup: %v", err)
			}
			if profile.Name != row.name || profile.Seed != seed ||
				profile.Forward.RateBitsPerSecond != row.forward ||
				profile.Reverse.RateBitsPerSecond != row.reverse ||
				carrierTunMtu(profile) != row.mtu ||
				profile.Forward.BaseDelay != row.delay || profile.Reverse.BaseDelay != row.delay {
				t.Fatalf("sweep profile differs from its declared network: %+v", profile)
			}
		})
	}
}

// A future typo must fail at lookup rather than become a 90-second platform
// timeout for every current/legacy row. A zero-valued profile is never success.
func TestPerfvarLegacyProviderCompatibilityRejectsUnknownProfile(t *testing.T) {
	for _, name := range []string{"", "cell-edge-5m-down-1m-up-typo", "clean-lan-typo"} {
		profile, err := perfvarLegacyProviderProfile(20260919, name)
		if err == nil || !strings.Contains(err.Error(), "unknown") || profile.Name != "" || carrierTunMtu(profile) != 0 {
			t.Fatalf("profile %q was not rejected before fixture setup: %+v, %v", name, profile, err)
		}
	}
}
