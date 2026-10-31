package monitor

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// A sentinel over an explicit registry view; production uses NewSignals.
func subnetCoverageSignalWith(registered ...string) Signal {
	view := map[string]bool{}
	for _, number := range registered {
		view[number] = true
	}
	return &signalAdapter{
		number: "22.0",
		key:    "subnet-coverage",
		name:   "Subnet correctness observation coverage",
		probe:  subnetCoverageProbe{registered: func() map[string]bool { return view }},
	}
}

func TestSubnetCoverageReportsEnabledReaderGapWithoutLeakingNamespace(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.VerificationEnabled = true
	settings.STConfigStatus = STConfigurationEnabled
	settings.STDeploymentKey = "synthetic-private-deployment-namespace"

	alerts, err := subnetCoverageSignalWith().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "subnet-monitor-coverage")
	for _, required := range []string{
		"desired_state=enabled",
		"implemented_correctness_families=0",
		"required_correctness_families=9",
		"missing_families=22.1,22.2,22.3,22.4,22.5,22.6,22.7,22.8,22.9",
		"§22.1 finalized chain, deployment, policy, and governance readers",
		"independently qualified",
	} {
		if !strings.Contains(alert.Markdown(), required) {
			t.Errorf("coverage alert is missing %q:\n%s", required, alert.Markdown())
		}
	}
	requireAlertOmits(t, alert, settings.STDeploymentKey)
}

// A family leaves the warning once its reader is registered; the others stay.
func TestSubnetCoverageRetiresRegisteredFamilies(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.VerificationEnabled = true
	settings.STConfigStatus = STConfigurationEnabled
	settings.STDeploymentKey = "synthetic-private-deployment-namespace"

	alerts, err := subnetCoverageSignalWith("22.6", "22.7", "22.8", "22.9", "22.8a").Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "subnet-monitor-coverage")
	if !strings.Contains(alert.Observed, "implemented_correctness_families=4") || !strings.Contains(alert.Observed, "missing_families=22.1,22.2,22.3,22.4,22.5") {
		t.Fatalf("partial registration was not retired family by family: %q", alert.Observed)
	}
	for _, retired := range []string{"§22.6 ", "§22.7 ", "§22.8 ", "§22.9 "} {
		if strings.Contains(alert.Evidence, retired) {
			t.Fatalf("registered family %s still reported missing: %q", retired, alert.Evidence)
		}
	}

	all := []string{}
	for _, family := range subnetCorrectnessFamilies {
		all = append(all, family.number)
	}
	alerts, err = subnetCoverageSignalWith(all...).Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("every family registered still alerted: %v %+v", err, alerts)
	}
}

// A lettered partial reader is never counted as its parent family.
func TestSubnetCoveragePartialReaderIsNotAFamily(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.VerificationEnabled = true
	settings.STConfigStatus = STConfigurationEnabled
	settings.STDeploymentKey = "synthetic-private-deployment-namespace"
	alerts, err := subnetCoverageSignalWith("22.8a").Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "subnet-monitor-coverage")
	if !strings.Contains(alert.Observed, "implemented_correctness_families=0") {
		t.Fatalf("partial §22.8a reader counted as a family: %q", alert.Observed)
	}
}

// The production sentinel reads the real registry: every registered family
// number is retired and every unregistered one stays reported.
func TestSubnetCoverageUsesProductionRegistry(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.VerificationEnabled = true
	settings.STConfigStatus = STConfigurationEnabled
	settings.STDeploymentKey = "synthetic-private-deployment-namespace"
	registered := map[string]bool{}
	for _, signal := range NewSignals() {
		registered[signal.Number()] = true
	}
	implemented := 0
	for _, family := range subnetCorrectnessFamilies {
		if registered[family.number] {
			implemented += 1
		}
	}
	alerts, err := NewSubnetCoverageSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if implemented == len(subnetCorrectnessFamilies) {
		if len(alerts) != 0 {
			t.Fatalf("all families registered but the sentinel alerted: %+v", alerts)
		}
		return
	}
	alert := requireAlertClass(t, alerts, "subnet-monitor-coverage")
	if !strings.Contains(alert.Observed, "implemented_correctness_families="+strconv.Itoa(implemented)) {
		t.Fatalf("sentinel count differs from the registry: %q", alert.Observed)
	}
	for _, family := range subnetCorrectnessFamilies {
		if registered[family.number] == strings.Contains(alert.Evidence, "§"+family.number+" ") {
			t.Fatalf("family %s registered=%t but evidence=%q", family.number, registered[family.number], alert.Evidence)
		}
	}
}

func TestSubnetCoverageTreatsExplicitDisableAsNotDeployed(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.STConfigStatus = STConfigurationExplicitDisabled

	alerts, err := subnetCoverageSignalWith().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("explicitly disabled subnet returned alerts: %+v", alerts)
	}
}

func TestSubnetCoveragePreservesUnknownConfiguration(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.STConfigStatus = STConfigurationUnavailable

	alerts, err := subnetCoverageSignalWith().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "subnet-monitor-coverage")
	if !strings.Contains(alert.Observed, "desired_state=unavailable") ||
		!strings.Contains(alert.Mechanism, "cannot decide whether economic work is deployed") {
		t.Fatalf("unavailable desired state was not retained: %+v", alert)
	}
}

func TestSubnetCoveragePreservesEnabledInvalidConfiguration(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.VerificationEnabled = true
	settings.STConfigStatus = STConfigurationEnabledInvalid

	alerts, err := subnetCoverageSignalWith().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "subnet-monitor-coverage")
	if !strings.Contains(alert.Observed, "desired_state=enabled-invalid") ||
		!strings.Contains(alert.Mechanism, "cannot decide whether economic work is deployed") {
		t.Fatalf("invalid enabled desired state was not retained: %+v", alert)
	}
}

func TestSubnetCoveragePreservesLegacyUnknownConfiguration(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		settings := syntheticSettings(&syntheticSource{})
		settings.VerificationEnabled = enabled
		settings.STConfigStatus = STConfigurationUnknown
		settings.STDeploymentKey = "synthetic-private-deployment-namespace"

		alerts, err := subnetCoverageSignalWith().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, "subnet-monitor-coverage")
		want := "desired_state=unknown"
		if enabled {
			want = "desired_state=enabled-with-unknown-configuration-authority"
		}
		if !strings.Contains(alert.Observed, want) {
			t.Fatalf("legacy unknown desired state = %q, want %q", alert.Observed, want)
		}
		requireAlertOmits(t, alert, settings.STDeploymentKey)
	}
}

func TestSubnetCoverageCanceledContextCannotBlockLocalEvaluation(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.VerificationEnabled = true
	settings.STConfigStatus = STConfigurationEnabled
	settings.STDeploymentKey = "synthetic-canceled-namespace"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	alerts, err := subnetCoverageSignalWith().Run(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "subnet-monitor-coverage")
}

func TestSubnetCoverageRegistrationMetadata(t *testing.T) {
	signal := NewSubnetCoverageSignal()
	if signal.Number() != "22.0" || signal.Key() != "subnet-coverage" ||
		signal.ID() != "subnet/coverage" || signal.Cadence() <= 0 {
		t.Fatalf("unexpected subnet coverage metadata: number=%q key=%q id=%q cadence=%s",
			signal.Number(), signal.Key(), signal.ID(), signal.Cadence())
	}
}
