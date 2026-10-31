// Exercises incident and pause contracts without production identities or time.
package model

import (
	"strings"
	"testing"
)

// This reason represents only a paused evaluator, never an admission, source or
// reward waiver. Intake authority must separately prove its precise approval.
func TestCompetitionAutomationCalibrationMaintenanceRequiresActiveEvaluation(t *testing.T) {
	args := CompetitionAutomationHoldArgs{Kind: "evaluation", Active: true, ExpectedRevision: 3,
		IdempotencyKey: "synthetic-calibration", ReasonCode: "CALIBRATION_MAINTENANCE",
		PublicExplanation: "Synthetic serial calibration pause.", EvidenceSha256: strings.Repeat("f", 64)}
	if err := ValidateCompetitionAutomationHold(args); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"admission", "source_activation", "reward_delivery"} {
		args.Kind = kind
		if ValidateCompetitionAutomationHold(args) == nil {
			t.Fatalf("maintenance reason accepted for %s", kind)
		}
	}
	args.Kind, args.Active = "evaluation", false
	if ValidateCompetitionAutomationHold(args) == nil {
		t.Fatal("maintenance reason released the evaluator")
	}
	args.ReasonCode = "RECOVERY_APPROVED"
	if err := ValidateCompetitionAutomationHold(args); err != nil {
		t.Fatal("normal explicit recovery changed", err)
	}
}

// Releasing a pause requires an affirmative recovery decision, not a boolean.
func TestCompetitionAutomationHoldRequiresRecoveryApproval(t *testing.T) {
	args := CompetitionAutomationHoldArgs{
		Kind: "source_activation", Active: true, IdempotencyKey: "synthetic-hold",
		ReasonCode: "OPERATOR_HOLD", PublicExplanation: "Synthetic qualification investigation.",
		EvidenceSha256: strings.Repeat("a", 64),
	}
	if err := ValidateCompetitionAutomationHold(args); err != nil {
		t.Fatal(err)
	}
	args.Active = false
	if ValidateCompetitionAutomationHold(args) == nil {
		t.Fatal("hold released without recovery approval")
	}
	args.ReasonCode = "RECOVERY_APPROVED"
	if err := ValidateCompetitionAutomationHold(args); err != nil {
		t.Fatal(err)
	}
}

// A pause in one capability must never silently mean a broad unscoped switch.
func TestCompetitionAutomationHoldHasExactCapabilities(t *testing.T) {
	args := CompetitionAutomationHoldArgs{
		Active: true, IdempotencyKey: "synthetic-hold", ReasonCode: "OPERATOR_HOLD",
		PublicExplanation: "Synthetic investigation.", EvidenceSha256: strings.Repeat("b", 64),
	}
	for _, kind := range []string{"admission", "evaluation", "source_activation", "reward_delivery"} {
		args.Kind = kind
		if err := ValidateCompetitionAutomationHold(args); err != nil {
			t.Errorf("capability %q: %v", kind, err)
		}
	}
	args.Kind = "all"
	if ValidateCompetitionAutomationHold(args) == nil {
		t.Fatal("unscoped capability accepted")
	}
}

// Invalidating evidence is distinct from disqualifying an otherwise measured miner.
func TestCompetitionAutomationIncidentRequiresConcreteScope(t *testing.T) {
	args := CompetitionAutomationIncidentArgs{
		IdempotencyKey: "synthetic-incident", InvalidatesEvaluation: true,
		ReasonCode: "EVALUATION_INTEGRITY_INCIDENT", PublicExplanation: "Synthetic evidence integrity failure.",
		EvidenceSha256: strings.Repeat("c", 64),
	}
	if err := ValidateCompetitionAutomationIncident(args); err != nil {
		t.Fatal(err)
	}
	args.ReasonCode = "NO_IMPROVEMENT"
	if ValidateCompetitionAutomationIncident(args) == nil {
		t.Fatal("ordinary no-improvement accepted as invalidation authority")
	}
	args.InvalidatesEvaluation, args.SourceCompromised, args.ReasonCode = false, true, "SOURCE_COMPROMISE"
	if ValidateCompetitionAutomationIncident(args) == nil {
		t.Fatal("compromised source accepted without an exact lock")
	}
	args.SourceLockSha256 = strings.Repeat("d", 64)
	if err := ValidateCompetitionAutomationIncident(args); err != nil {
		t.Fatal(err)
	}
}
