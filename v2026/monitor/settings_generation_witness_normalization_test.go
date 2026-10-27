// Raw disabled-host scope must bind the same normalized startup value that
// the public monitor passes to its completed freshness callback.
package monitor

import (
	"bytes"
	"testing"
	"time"
)

// Model LoadSignalSettings' nonnil empty SSH-key result after its early
// defaults pass, then preserve the CLI's validation-by-value boundary.
func settingsWitnessRawDisabledControl(t *testing.T, normalized bool, excluded bool) (SignalSettings, *settingsGenerationWitness, *bytes.Buffer, *int) {
	t.Helper()
	startup, _ := settingsFreshnessFixture(t)
	startup.disabledHosts = []HostSettings{{Name: "disabled-fixture", Roles: []string{}, SSHKeyPaths: []string{}, RedisNodePorts: []int{}, scopeEndpoints: []string{}}}
	if normalized {
		startup = startup.withDefaults()
	}
	if excluded {
		var err error
		startup, err = ExcludeHosts(startup, startup.Hosts[0].Name)
		if err != nil {
			t.Fatal("synthetic CLI exclusion failed")
		}
	}
	loads := 0
	current := startup
	startup.SettingsGenerationCheck = NewSettingsGenerationCheck(func() (SignalSettings, error) {
		loads++
		return current, nil
	})
	if startup.Validate() != nil {
		t.Fatal("synthetic disabled scope failed validation")
	}
	if (startup.disabledHosts[0].SSHKeyPaths == nil) != normalized {
		t.Fatal("validation changed the raw-versus-normalized fixture boundary")
	}
	output := &bytes.Buffer{}
	witness, err := newSettingsGenerationWitness(startup, output, settingsWitnessTestIdentity(), DefaultSettingsGenerationWitnessSettings())
	if err != nil {
		t.Fatal("synthetic witness constructor failed")
	}
	startup.settingsGenerationWitness = witness
	return startup, witness, output, &loads
}

// A normalized observation of the same raw input must remain associated;
// this bypasses scheduler, source loading and sink errors as causal controls.
func TestSettingsWitnessRawDisabledScopeAcceptsNormalizedObservation(t *testing.T) {
	startup, witness, output, loads := settingsWitnessRawDisabledControl(t, false, false)
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	err := witness.observe(t.Context(), startup.withDefaults(), started, started.Add(time.Second), true, nil)
	records := settingsWitnessTestRecords(t, output)
	t.Logf("callback_loads=%d witness_records=%d association_error=%t", *loads, len(records), err != nil)
	if err != nil || *loads != 0 || len(records) != 1 {
		t.Fatal("same normalized disabled scope lost its witness association")
	}
	if records[0].Result != "current" || !records[0].CompleteSettingsEqual || !records[0].StartupAssociation || !records[0].ProcessArtifactBinding {
		t.Fatal("normalized observation did not retain complete witness authority")
	}
}

// Exercise the public constructor used immediately after CLI validation,
// overrides and witness attachment; keep the explicit host hold intact.
func TestSettingsWitnessRawDisabledScopeSurvivesCLIExclusionAndMonitor(t *testing.T) {
	startup, _, output, loads := settingsWitnessRawDisabledControl(t, false, true)
	monitor := NewWithSignals(startup, NewSettingsFreshnessSignal())
	alerts, err := monitor.Run(t.Context())
	witnessErrors := 0
	scopeFindings := 0
	for _, alert := range alerts {
		if alert.Target == "monitor-settings/witness" {
			witnessErrors++
		}
		if alert.Class == "monitor-host-scope-partial" {
			scopeFindings++
		}
	}
	records := settingsWitnessTestRecords(t, output)
	t.Logf("callback_loads=%d witness_records=%d witness_error_findings=%d scope_findings=%d", *loads, len(records), witnessErrors, scopeFindings)
	if err != nil || *loads != 1 || len(records) != 1 || witnessErrors != 0 || scopeFindings != 1 {
		t.Fatal("public monitor lost the normalized witness or immutable host hold")
	}
	if records[0].Result != "current" || !records[0].CompleteSettingsEqual || records[0].StartupExcludedHosts != 1 || records[0].StartupDisabledHosts != 1 {
		t.Fatal("public witness changed currentness or desired scope counts")
	}
}

// The already normalized constructor path is an independent positive control.
func TestSettingsWitnessNormalizedDisabledScopeRemainsCompatible(t *testing.T) {
	startup, _, output, loads := settingsWitnessRawDisabledControl(t, true, false)
	monitor := NewWithSignals(startup, NewSettingsFreshnessSignal())
	alerts, err := monitor.Run(t.Context())
	records := settingsWitnessTestRecords(t, output)
	t.Logf("callback_loads=%d witness_records=%d alert_count=%d", *loads, len(records), len(alerts))
	if err != nil || *loads != 1 || len(records) != 1 || len(alerts) != 0 {
		t.Fatal("already normalized disabled scope lost its positive control")
	}
	if records[0].Result != "current" || !records[0].CompleteSettingsEqual || records[0].StartupDisabledHosts != 1 {
		t.Fatal("normalized positive control lost its complete current witness")
	}
}

// Normalizing private disabled scope must still freeze both its exported
// nested slices and its private endpoint slice against caller mutation.
func TestSettingsWitnessNormalizedDisabledSnapshotRetainsAliasGuard(t *testing.T) {
	startup, _ := settingsFreshnessFixture(t)
	startup.disabledHosts = []HostSettings{{Name: "disabled-fixture", Roles: []string{"services"}, SSHKeyPaths: []string{"synthetic-key"}, scopeEndpoints: []string{"192.0.2.17"}}}
	output := &bytes.Buffer{}
	witness, err := newSettingsGenerationWitness(startup, output, settingsWitnessTestIdentity(), DefaultSettingsGenerationWitnessSettings())
	if err != nil {
		t.Fatal("synthetic disabled alias constructor failed")
	}
	startup.disabledHosts[0].Roles[0] = "changed-role"
	startup.disabledHosts[0].SSHKeyPaths[0] = "changed-key"
	startup.disabledHosts[0].scopeEndpoints[0] = "192.0.2.18"
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	err = witness.observe(t.Context(), startup.withDefaults(), started, started.Add(time.Second), true, nil)
	t.Logf("witness_records=%d association_error=%t", len(settingsWitnessTestRecords(t, output)), err != nil)
	if err == nil || output.Len() != 0 {
		t.Fatal("mutated disabled aliases forged a witness association")
	}
	frozen := witness.startup.disabledHosts[0]
	if frozen.Roles[0] != "services" || frozen.SSHKeyPaths[0] != "synthetic-key" || frozen.scopeEndpoints[0] != "192.0.2.17" {
		t.Fatal("caller mutation reached the frozen disabled snapshot")
	}
}
