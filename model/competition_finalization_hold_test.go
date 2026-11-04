// A prospective finalization pause adds no fields to existing hold identities
// and cannot silently become a grading, maintenance or publication embargo.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// The new scheduler capability retains explicit CAS and recovery approval.
func TestCompetitionFinalizationHoldValidationIsIndependent(t *testing.T) {
	args := CompetitionAutomationHoldArgs{Kind: "finalization", Active: true, ExpectedRevision: 2,
		IdempotencyKey: "synthetic-finalization", ReasonCode: "OPERATOR_HOLD",
		PublicExplanation: "Synthetic finalization investigation.", EvidenceSha256: strings.Repeat("a", 64)}
	if err := ValidateCompetitionAutomationHold(args); err != nil {
		t.Fatal(err)
	}
	args.ReasonCode = "CALIBRATION_MAINTENANCE"
	if ValidateCompetitionAutomationHold(args) == nil {
		t.Fatal("finalization gained an evaluation-maintenance waiver")
	}
	args.Active, args.ReasonCode = false, "OPERATOR_HOLD"
	if ValidateCompetitionAutomationHold(args) == nil {
		t.Fatal("finalization released without recovery approval")
	}
	args.ReasonCode = "RECOVERY_APPROVED"
	if err := ValidateCompetitionAutomationHold(args); err != nil {
		t.Fatal(err)
	}
}

// Existing request bytes and their hashes cannot drift when a new physical
// journal is added; no optional field is introduced into the old contract.
func TestCompetitionFinalizationPreservesFourLegacyHoldDigests(t *testing.T) {
	for _, kind := range []string{"admission", "evaluation", "source_activation", "reward_delivery"} {
		args := CompetitionAutomationHoldArgs{Kind: kind, Active: true, ExpectedRevision: 2,
			IdempotencyKey: "synthetic-key", ReasonCode: "OPERATOR_HOLD", PublicExplanation: "Synthetic investigation.", EvidenceSha256: strings.Repeat("a", 64)}
		want := `{"kind":"` + kind + `","active":true,"expected_revision":2,"idempotency_key":"synthetic-key","reason_code":"OPERATOR_HOLD","public_explanation":"Synthetic investigation.","evidence_sha256":"` + strings.Repeat("a", 64) + `"}`
		content, err := json.Marshal(args)
		if err != nil || string(content) != want {
			t.Fatalf("%s canonical request changed: %s %v", kind, content, err)
		}
		digest := sha256.Sum256([]byte(want))
		if got, err := CompetitionAutomationDigest(args); err != nil || got != hex.EncodeToString(digest[:]) {
			t.Fatal("legacy canonical hold identity changed", kind, got, err)
		}
	}
}

// The new operational bit is absent when inactive, keeping historical status
// encodings intact; active finalization is separately visible from publication.
func TestCompetitionFinalizationStatusIsOptionalAndProspective(t *testing.T) {
	status := CompetitionRuntimeStatus{IntakeReady: true, GradingReady: true}
	content, err := json.Marshal(status)
	if err != nil || string(content) != `{"intake_ready":true,"grading_ready":true,"publication_held":false}` {
		t.Fatal("inactive finalization changed existing runtime bytes", string(content), err)
	}
	status.FinalizationHeld = true
	content, err = json.Marshal(status)
	if err != nil || !strings.Contains(string(content), `"finalization_held":true`) || status.PublicationHeld || !status.GradingReady || !status.IntakeReady {
		t.Fatal("prospective status became an intake/grading/publication pause", status, err)
	}
}
