// SIGNALS.md §22.10 implementation acceptance, executable for every
// registered §22.1 through §22.9 family. Each family registers its own
// acceptance fixtures from its own test file; these tests then require, for
// every registered family, a healthy control, a proved counterexample with the
// §22.10 evidence frame, retention of that counterexample after a healthy
// read, an unknown source that never turns green, an explicit not-deployed
// state, cancellation that withholds findings, and a cadence that spans the
// §7 five-minute recovery window.
package monitor

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"
)

// One family's acceptance fixtures. Each builder returns a fresh signal and
// settings; repair turns the violated fixture healthy in place.
type subnetFamilyAcceptanceCase struct {
	healthy       func(t *testing.T) (Signal, SignalSettings)
	violated      func(t *testing.T) (Signal, SignalSettings, func())
	violatedClass string
	unknown       func(t *testing.T) (Signal, SignalSettings)
	canceled      func(t *testing.T) (Signal, SignalSettings, context.Context)
	// The family's permanent capability gaps that a healthy run still reports.
	fixedGaps []subnetFamilyAcceptanceGap
}

// One standing gap: always an unobservable warn of this class and frame.
type subnetFamilyAcceptanceGap struct {
	class string
	frame string
}

// Populated by each family's own test file.
var subnetFamilyAcceptanceCases = map[string]subnetFamilyAcceptanceCase{}

// Hex identities never appear in a rendered alert.
var subnetFamilyAcceptanceIdentity = regexp.MustCompile(`0x[0-9a-fA-F]{40}`)

func subnetFamilyAcceptanceNumbers(t *testing.T) []string {
	t.Helper()
	numbers := []string{}
	registered := map[string]bool{}
	for _, signal := range NewSignals() {
		registered[signal.Number()] = true
	}
	for _, family := range subnetCorrectnessFamilies {
		if registered[family.number] {
			numbers = append(numbers, family.number)
		}
	}
	return numbers
}

// Every registered family has fixtures, and every fixture names a family.
func TestSubnetFamilyAcceptanceEveryRegisteredFamilyHasFixtures(t *testing.T) {
	numbers := subnetFamilyAcceptanceNumbers(t)
	if len(numbers) == 0 {
		t.Fatal("no §22 family is registered")
	}
	registered := map[string]bool{}
	for _, number := range numbers {
		registered[number] = true
		acceptance, ok := subnetFamilyAcceptanceCases[number]
		if !ok {
			t.Errorf("registered §%s family has no acceptance fixtures: register a subnetFamilyAcceptanceCase from its own test file (SIGNALS.md §22.10)", number)
			continue
		}
		if acceptance.healthy == nil || acceptance.violated == nil || acceptance.unknown == nil || acceptance.canceled == nil || acceptance.violatedClass == "" {
			t.Errorf("§%s acceptance fixtures are incomplete", number)
		}
	}
	for number := range subnetFamilyAcceptanceCases {
		if !registered[number] {
			t.Errorf("acceptance fixtures name unregistered family §%s", number)
		}
	}
}

// A healthy fixture raises no incident; only its declared capability gaps.
func TestSubnetFamilyAcceptanceHealthyControl(t *testing.T) {
	for _, number := range subnetFamilyAcceptanceNumbers(t) {
		acceptance, ok := subnetFamilyAcceptanceCases[number]
		if !ok {
			continue
		}
		signal, settings := acceptance.healthy(t)
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatalf("§%s healthy run failed: %v", number, err)
		}
		for _, alert := range alerts {
			allowed := false
			for _, gap := range acceptance.fixedGaps {
				allowed = allowed || alert.Class == gap.class && alert.Frame == gap.frame && alert.Severity == SeverityWarn && alert.unobservable
			}
			if !allowed {
				t.Errorf("§%s healthy control alerted: %s %s %s", number, alert.Class, alert.Target, alert.Frame)
			}
		}
	}
}

// A proved counterexample pages with the complete §22.10 evidence frame.
func TestSubnetFamilyAcceptanceViolationCarriesEvidence(t *testing.T) {
	for _, number := range subnetFamilyAcceptanceNumbers(t) {
		acceptance, ok := subnetFamilyAcceptanceCases[number]
		if !ok {
			continue
		}
		signal, settings, _ := acceptance.violated(t)
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatalf("§%s violated run failed: %v", number, err)
		}
		pages := 0
		for _, alert := range alerts {
			markdown := alert.Markdown()
			if subnetFamilyAcceptanceIdentity.MatchString(markdown) {
				t.Errorf("§%s alert %s renders a hex identity", number, alert.Class)
			}
			if alert.SignalNumber != number || alert.Environment == "" || alert.Target == "" {
				t.Errorf("§%s alert %s lacks its stable identity", number, alert.Class)
			}
			if strings.Contains(alert.Target, "epoch") || strings.Contains(alert.Target, "block") {
				t.Errorf("§%s alert %s puts changing detail in its target %q", number, alert.Class, alert.Target)
			}
			if alert.Class != acceptance.violatedClass || alert.Severity != SeverityPage {
				continue
			}
			pages += 1
			for _, required := range []string{"environment=", "scope=", "checkpoint=finalized-", "completeness=", "comparison_source="} {
				if !strings.Contains(alert.Context, required) {
					t.Errorf("§%s counterexample context lacks %q: %q", number, required, alert.Context)
				}
			}
			for _, required := range []string{"state=violated", "invariant=", "expected=", "observed="} {
				if !strings.Contains(alert.Observed, required) {
					t.Errorf("§%s counterexample observed lacks %q: %q", number, required, alert.Observed)
				}
			}
			if alert.Action == "" || alert.Verify == "" || alert.Sustain != 1 {
				t.Errorf("§%s counterexample lacks its action, recovery proof or sustain", number)
			}
		}
		if pages == 0 {
			t.Errorf("§%s violated fixture raised no %s page", number, acceptance.violatedClass)
		}
	}
}

// A healthy read after a proved counterexample cannot erase it.
func TestSubnetFamilyAcceptanceRetainsCounterexamples(t *testing.T) {
	for _, number := range subnetFamilyAcceptanceNumbers(t) {
		acceptance, ok := subnetFamilyAcceptanceCases[number]
		if !ok {
			continue
		}
		signal, settings, repair := acceptance.violated(t)
		if _, err := signal.Run(context.Background(), settings); err != nil {
			t.Fatalf("§%s violated run failed: %v", number, err)
		}
		repair()
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatalf("§%s repaired run failed: %v", number, err)
		}
		retained := false
		for _, alert := range alerts {
			retained = retained || alert.Class == acceptance.violatedClass && alert.Severity == SeverityPage && strings.Contains(alert.Observed, "retained=true")
		}
		if !retained {
			t.Errorf("§%s healthy read erased its counterexample", number)
		}
	}
}

// An unavailable source is an unknown with a specific fixed reason.
func TestSubnetFamilyAcceptanceUnknownSourceNeverGreen(t *testing.T) {
	for _, number := range subnetFamilyAcceptanceNumbers(t) {
		acceptance, ok := subnetFamilyAcceptanceCases[number]
		if !ok {
			continue
		}
		signal, settings := acceptance.unknown(t)
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatalf("§%s unknown run failed: %v", number, err)
		}
		unknown := 0
		for _, alert := range alerts {
			if alert.Severity == SeverityPage {
				t.Errorf("§%s unknown source paged: %s", number, alert.Class)
			}
			if alert.unobservable && strings.Contains(alert.Observed, "state=unknown reason=") && !strings.Contains(alert.Observed, "reason= ") {
				unknown += 1
			}
		}
		if unknown == 0 {
			t.Errorf("§%s unknown source produced no unknown alert", number)
		}
	}
}

// Without a reviewed pin, an explicit disable is not deployed.
func TestSubnetFamilyAcceptanceDisabledIsNotDeployed(t *testing.T) {
	for _, number := range subnetFamilyAcceptanceNumbers(t) {
		acceptance, ok := subnetFamilyAcceptanceCases[number]
		if !ok {
			continue
		}
		signal, settings := acceptance.healthy(t)
		settings.SubnetDeployment = nil
		settings.STConfigStatus = STConfigurationExplicitDisabled
		settings.VerificationEnabled = false
		settings.STDeploymentKey = ""
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil || len(alerts) != 0 {
			t.Errorf("§%s disabled subnet alerted: err=%v alerts=%d", number, err, len(alerts))
		}
	}
}

// Parent cancellation withholds every finding and returns an error.
func TestSubnetFamilyAcceptanceCancellation(t *testing.T) {
	for _, number := range subnetFamilyAcceptanceNumbers(t) {
		acceptance, ok := subnetFamilyAcceptanceCases[number]
		if !ok {
			continue
		}
		signal, settings, ctx := acceptance.canceled(t)
		alerts, err := signal.Run(ctx, settings)
		if err == nil || len(alerts) != 0 {
			t.Errorf("§%s canceled run returned alerts=%d err=%v", number, len(alerts), err)
		}
	}
}

// Two complete healthy runs of every family span the §7 five-minute recovery
// window, and every default read budget is bounded.
func TestSubnetFamilyAcceptanceRecoveryWindowAndBudgets(t *testing.T) {
	settings := DefaultSubnetReplaySettings()
	if reason := settings.budgetReason(); reason != "" || settings.Cadence <= settings.RunBudget {
		t.Fatalf("default family budgets are not bounded inside one cadence: %s", reason)
	}
	for _, signal := range NewSignals() {
		for _, family := range subnetCorrectnessFamilies {
			if signal.Number() == family.number && signal.Cadence() < 5*time.Minute {
				t.Errorf("§%s cadence %s cannot span the five-minute recovery window in two runs", family.number, signal.Cadence())
			}
		}
	}
}

// The §22.10 text and the crosswalk keep the acceptance contract visible.
func TestSubnetFamilyAcceptanceCatalogKeepsQualificationOpen(t *testing.T) {
	section := strings.Join(strings.Fields(subnetCorrectnessDocumentation(t)), " ")
	for _, required := range []string{
		"subnet_family_acceptance_test.go",
		"production paging additionally requires",
	} {
		if !strings.Contains(section, required) {
			t.Errorf("§22.10 is missing %q", required)
		}
	}
}
