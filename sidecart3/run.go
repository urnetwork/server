package sidecart3

// Running T2's rows on T3: the same fault.Builder, never a second copy (plan-TEST-small-readings
// reading 1), on a T3 Net, under the rules of §22.8 its row takes, with T3's nightly seeds. Nothing here
// reruns a failed attempt: a run that failed once is a failure, whatever a second would do (R-TEST-9),
// and sidecar-t3.sh sets WARP_TEST_ENV_FAIL_FAST=1 so server's TestEnv does not rerun one either.
//
// Diverge is the check Q-TEST-1 asks for: one builder on T2's fakes and on T3's stores, and a row that
// passes on one and fails on the other names which, since one of the two is not the system.

import (
	"fmt"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/fault"
)

// Runs builder's schedule once on a T3 Net of its own, and fails t with its trace if it fails. settings
// nil takes the defaults.
func Run(t testing.TB, builder fault.Builder, settings *Settings) *fault.Result {
	t.Helper()
	result := runOn(t, builder, NetSettings(t, settings))
	if result.Err != nil {
		t.Errorf("on T3: %s", result)
	}
	return result
}

// Builds and runs one schedule on a Net from netSettings, closed before this returns.
func runOn(t testing.TB, builder fault.Builder, netSettings *sidecartest.NetSettings) *fault.Result {
	net := sidecartest.NewNet(t.Context(), netSettings)
	defer net.Close()
	return fault.Run(t.Context(), builder(net), net)
}

// The rules T3 runs a row under: §22.8's nightly seeds on T3 Nets, traces written as T2's are.
func RuleSettings(t testing.TB, settings *Settings) *fault.RuleSettings {
	if settings == nil {
		settings = DefaultSettings()
	}
	ruleSettings := fault.DefaultRuleSettings()
	ruleSettings.Tier = fault.TierT3
	ruleSettings.Net = NetSettings(t, settings)
	ruleSettings.Seeds = fault.NightlySeeds()
	return ruleSettings
}

// A registered case's T3 run under every rule its row takes (fault.CheckRow on T3's Nets), failing t
// with each failure and logging what ran. A case with no T3 builder fails as missing. ruleSettings nil
// takes RuleSettings with the defaults.
func RunRow(t testing.TB, rowId string, caseName string, ruleSettings *fault.RuleSettings) *fault.RowReport {
	t.Helper()
	registration := fault.Lookup(rowId, caseName, fault.TierT3)
	if registration == nil {
		t.Fatalf("%s %q has no T3 builder registered: missing", rowId, caseName)
	}
	if ruleSettings == nil {
		ruleSettings = RuleSettings(t, nil)
	}
	report := fault.CheckRow(t.Context(), registration, ruleSettings)
	t.Log(report.String())
	for _, failure := range report.Failures {
		t.Errorf("%s", failure)
	}
	for _, err := range report.Errors {
		t.Errorf("%v", err)
	}
	return report
}

// What one builder did on each tier.
type Divergence struct {
	T2 *fault.Result
	T3 *fault.Result
}

// Whether exactly one tier failed: a fake that does not do what the store does, or the reverse.
func (self *Divergence) Diverged() bool {
	return (self.T2.Err == nil) != (self.T3.Err == nil)
}

// nil when both tiers passed; otherwise what failed, a divergence named as one.
func (self *Divergence) Err() error {
	switch {
	case self.T2.Err == nil && self.T3.Err == nil:
		return nil
	case self.T2.Err == nil:
		return fmt.Errorf("%s diverged: it passed on T2's fakes and failed on T3's stores (Q-TEST-1): %s", self.T3.Id, self.T3)
	case self.T3.Err == nil:
		return fmt.Errorf("%s diverged: it passed on T3's stores and failed on T2's fakes (Q-TEST-1): %s", self.T2.Id, self.T2)
	}
	return fmt.Errorf("%s failed on both tiers:\nT2: %s\nT3: %s", self.T2.Id, self.T2, self.T3)
}

// Q-TEST-1's check: builder's fixed schedule on T2's fakes and on T3, each on a Net of its own, failing
// t when either fails and naming a divergence when only one did. A row whose T2 run uses the platform
// fakes fails on T3 until server's exchange carries its clients. settings nil takes the defaults, its Net
// the T2 run's too.
func Diverge(t testing.TB, builder fault.Builder, settings *Settings) *Divergence {
	t.Helper()
	if settings == nil {
		settings = DefaultSettings()
	}
	divergence := &Divergence{
		T2: runOn(t, builder, settings.Net),
		T3: runOn(t, builder, NetSettings(t, settings)),
	}
	if err := divergence.Err(); err != nil {
		t.Error(err)
	}
	return divergence
}
