// The §22.1--§22.5 readers retire their families from the §22.0 sentinel
// through the production registry, while every other family stays reported.
package monitor

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

func TestSubnetFamiliesOneThroughFiveRetireFromTheSentinel(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.VerificationEnabled = true
	settings.STConfigStatus = STConfigurationEnabled
	settings.STDeploymentKey = "synthetic-private-deployment-namespace"
	registered := map[string]string{}
	for _, signal := range NewSignals() {
		registered[signal.Number()] = signal.Key()
	}
	for number, key := range map[string]string{"22.1": "subnet-chain", "22.2": "subnet-identity", "22.3": "subnet-deposits", "22.4": "subnet-validation", "22.5": "subnet-head"} {
		if registered[number] != key {
			t.Fatalf("family §%s is registered as %q, want %q", number, registered[number], key)
		}
	}
	alerts, err := NewSubnetCoverageSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) == 0 {
		// Every family is registered; the sentinel has retired completely.
		return
	}
	alert := requireAlertClass(t, alerts, "subnet-monitor-coverage")
	fields := map[string]string{}
	for _, field := range strings.Fields(alert.Observed) {
		if name, value, ok := strings.Cut(field, "="); ok {
			fields[name] = value
		}
	}
	missing, listed := fields["missing_families"]
	if !listed {
		t.Fatalf("the sentinel does not list its missing families: %s", alert.Observed)
	}
	for _, number := range strings.Split(missing, ",") {
		switch number {
		case "22.1", "22.2", "22.3", "22.4", "22.5":
			t.Fatalf("registered family §%s is still reported missing: %s", number, alert.Observed)
		}
	}
	implemented, err := strconv.Atoi(fields["implemented_correctness_families"])
	if err != nil || implemented < 5 {
		t.Fatalf("the sentinel counts %q implemented families, want at least five", fields["implemented_correctness_families"])
	}
	for _, number := range []string{"22.1", "22.2", "22.3", "22.4", "22.5"} {
		if strings.Contains(alert.Evidence, "§"+number+" ") {
			t.Fatalf("registered family §%s still has a missing prerequisite: %s", number, alert.Evidence)
		}
	}
}

// A registered family reader reports its own gap when its enrollment is
// missing, so retiring the sentinel's family never turns into silence.
func TestSubnetRetiredFamiliesReportTheirOwnGaps(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.VerificationEnabled = true
	settings.STConfigStatus = STConfigurationEnabled
	settings.STDeploymentKey = "synthetic-private-deployment-namespace"
	settings.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) { return true, nil }
	for _, signal := range []Signal{NewSubnetChainSignal(), NewSubnetIdentitySignal(), NewSubnetDepositsSignal(), NewSubnetValidationSignal(), NewSubnetHeadSignal()} {
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		if len(alerts) != 1 || alerts[0].Class != "subnet-monitor-coverage" || !alerts[0].unobservable || !strings.Contains(alerts[0].Observed, "reason=expectations-missing") {
			t.Fatalf("%s did not report its own missing enrollment: %+v", signal.Key(), alerts)
		}
	}
}
