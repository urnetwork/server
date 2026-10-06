package monitor

import (
	"context"
	"strings"
	"testing"
)

func TestSubnetCoverageReportsEnabledReaderGapWithoutLeakingNamespace(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.VerificationEnabled = true
	settings.STConfigStatus = STConfigurationEnabled
	settings.STDeploymentKey = "synthetic-private-deployment-namespace"

	alerts, err := NewSubnetCoverageSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "subnet-monitor-coverage")
	for _, required := range []string{
		"desired_state=enabled",
		"implemented_correctness_families=0",
		"required_correctness_families=9",
		"finalized native/EVM checkpoint readers",
		"independently qualified",
	} {
		if !strings.Contains(alert.Markdown(), required) {
			t.Errorf("coverage alert is missing %q:\n%s", required, alert.Markdown())
		}
	}
	requireAlertOmits(t, alert, settings.STDeploymentKey)
}

func TestSubnetCoverageTreatsExplicitDisableAsNotDeployed(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.STConfigStatus = STConfigurationExplicitDisabled

	alerts, err := NewSubnetCoverageSignal().Run(context.Background(), settings)
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

	alerts, err := NewSubnetCoverageSignal().Run(context.Background(), settings)
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

	alerts, err := NewSubnetCoverageSignal().Run(context.Background(), settings)
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
		t.Run(map[bool]string{false: "disabled-boolean", true: "enabled-boolean"}[enabled], func(t *testing.T) {
			settings := syntheticSettings(&syntheticSource{})
			settings.VerificationEnabled = enabled
			settings.STConfigStatus = STConfigurationUnknown
			settings.STDeploymentKey = "synthetic-private-deployment-namespace"

			alerts, err := NewSubnetCoverageSignal().Run(context.Background(), settings)
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
		})
	}
}

func TestSubnetCoverageCanceledContextCannotBlockLocalEvaluation(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.VerificationEnabled = true
	settings.STConfigStatus = STConfigurationEnabled
	settings.STDeploymentKey = "synthetic-canceled-namespace"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	alerts, err := NewSubnetCoverageSignal().Run(ctx, settings)
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
