package model

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The onboarding row keeps the distribution the platform's app reported
// (mmm/ONBOARDING-RETENTION.md P6). Filling follows the platform's rule, only
// fill, and a distribution only ever lands with its own platform: a row
// without a platform takes both; a row of the same platform whose
// distribution is unknown takes it; nothing known is replaced.
func TestNetworkOnboardingDistributionOnlyFills(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		create := func(platform string, distribution string) server.Id {
			networkId := server.NewId()
			if !CreateNetworkOnboarding(ctx, &NetworkOnboarding{NetworkId: networkId, CreatedAt: now, Email: true, Platform: platform, Distribution: distribution}) {
				t.Fatalf("could not create the %q/%q fixture", platform, distribution)
			}
			return networkId
		}
		read := func(networkId server.Id) (string, string) {
			row := GetNetworkOnboarding(ctx, networkId)
			if row == nil {
				t.Fatal("the fixture row disappeared")
			}
			return row.Platform, row.Distribution
		}

		created := create("macos", session.ClientDistributionMacDirect)
		platform, distribution := read(created)
		connect.AssertEqual(t, "macos", platform)
		connect.AssertEqual(t, session.ClientDistributionMacDirect, distribution)

		for _, c := range []struct {
			name             string
			rowPlatform      string
			rowDistribution  string
			platform         string
			distribution     string
			wantUpdated      bool
			wantPlatform     string
			wantDistribution string
		}{
			{"no platform yet", "", "", "macos", session.ClientDistributionMacDirect, true, "macos", session.ClientDistributionMacDirect},
			{"no platform yet, no distribution reported", "", "", "android", "", true, "android", ""},
			{"the same platform's unknown distribution", "macos", "", "macos", session.ClientDistributionAppStore, true, "macos", session.ClientDistributionAppStore},
			{"a known distribution stays", "macos", session.ClientDistributionAppStore, "macos", session.ClientDistributionMacDirect, false, "macos", session.ClientDistributionAppStore},
			{"another platform's row", "web", "", "macos", session.ClientDistributionMacDirect, false, "web", ""},
			{"nothing to fill", "macos", "", "macos", "", false, "macos", ""},
		} {
			networkId := create(c.rowPlatform, c.rowDistribution)
			updated := SetNetworkOnboardingPlatform(ctx, networkId, c.platform, c.distribution)
			platform, distribution := read(networkId)
			if updated != c.wantUpdated || platform != c.wantPlatform || distribution != c.wantDistribution {
				t.Errorf("%s: updated %t to %q/%q, want %t to %q/%q", c.name, updated, platform, distribution, c.wantUpdated, c.wantPlatform, c.wantDistribution)
			}
		}
	})
}
