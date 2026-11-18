package sidecart3

// M2's exit sweep on T3 (TEST-M2-19): the rules its rules item says a T3 row runs under are the ones RunRow takes. The
// sweep's T3 rows themselves run where their owners wrote them, in evshost and in this package under
// sidecar_localstack (sidecar's gate_m2_out.go); R2's epoch resync, which this file once was to hold, went with MLS
// (owner decision 610), since v0 keeps no epochs or acks.

import (
	"slices"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"
)

// A T3 row runs under §22.8's nightly form: T3's tier, the nightly 100 seeds where a T2 row runs 10, the Net's engines
// hosted on server's stores, the platform fakes refused, and the barrier wait CheckRow allows. Building the settings
// opens no store, so this needs none.
func TestGateT3RunsTheNightlySeeds(t *testing.T) {
	settings := RuleSettings(t, nil)
	if settings.Tier != fault.TierT3 {
		t.Errorf("T3's rules run on tier %q", settings.Tier)
	}
	if !slices.Equal(settings.Seeds, fault.NightlySeeds()) || len(settings.Seeds) != 100 {
		t.Errorf("T3's rules run %d seeds, the nightly form's 100", len(settings.Seeds))
	}
	if slices.Equal(settings.Seeds, fault.MergeSeeds()) {
		t.Error("T3's rules run a merge's seeds")
	}
	if settings.Net == nil || settings.Net.Engines == nil || settings.Net.Platform == nil {
		t.Fatal("T3's rules build no T3 Net: its engines or platform are T2's")
	}
	if settings.Net.RefusePlatformFakes == "" {
		t.Error("T3's Net takes the fake contract issuer and ControlId sink")
	}
	if limit := fault.DefaultRuleSettings().Net.Barriers.AwaitTimeout; limit < settings.Net.Barriers.AwaitTimeout {
		t.Errorf("T3's barrier wait %s is longer than §22.8's %s", settings.Net.Barriers.AwaitTimeout, limit)
	}
}
