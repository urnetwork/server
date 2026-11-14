// The public schema and native addendum share exact eligibility semantics while
// retaining the historical optional encoding required for immutable receipts.
package controller

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"gopkg.in/yaml.v3"
)

// Real schema compilation catches reason/addendum/census drift without relying
// on a second hand-written interpretation or mutating a live reviewer API.
func TestCompetitionProductionBenefitPublicContractConformance(t *testing.T) {
	content, err := readCompetitionTestSnContract()
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Components struct {
			Schemas map[string]any `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(content, &document); err != nil {
		t.Fatal(err)
	}
	schemaBytes, err := json.Marshal(map[string]any{"$ref": "#/$defs/CompetitionIntegrityReviewRequest", "$defs": document.Components.Schemas})
	if err != nil {
		t.Fatal(err)
	}
	schemaBytes = bytes.ReplaceAll(schemaBytes, []byte("#/components/schemas/"), []byte("#/$defs/"))
	var schema jsonschema.Schema
	if err := json.Unmarshal(schemaBytes, &schema); err != nil {
		t.Fatal(err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, snow := range []bool{false, true} {
		for _, fixture := range []struct {
			name  string
			valid bool
		}{
			{name: "historical", valid: true}, {name: "new approval", valid: true}, {name: "eligibility rejection", valid: true},
			{name: "missing addendum"}, {name: "wrong hash"}, {name: "wrong version"}, {name: "wrong criterion"},
			{name: "unmatched summary"}, {name: "unmatched finding"}, {name: "not applicable approval"},
		} {
			digest := strings.Repeat("a", 64)
			review := model.CompetitionIntegrityReviewRequest{IdempotencyKey: "synthetic-review", ExpectedRevision: 1, JobId: server.Id{0: 1},
				PatchSha256: digest, BaseSourceLockSha256: digest, ScoreSha256: digest, BaselineSha256: digest, ArtifactManifestSha256: digest, CriteriaVersion: "synthetic-criteria",
				RewardDisposition: "approved", SourceDisposition: "approved", ReasonCodes: []string{"APPROVED"}, PublicExplanation: "Synthetic supported client benefit.", WithheldEvidenceExplanation: "Synthetic retained evidence.", PrivateEvidenceSha256: digest}
			members := []string{""}
			if snow {
				members = model.CompetitionSnowWinnerReviewModelsForPolicy(model.CompetitionSnowWinnerReviewPolicy)
				review.ReviewerModel = model.CompetitionSnowWinnerReviewModel
				review.ReviewerReasoning = model.CompetitionSnowWinnerReviewReasoning
				review.ExpectedDecisionSha256 = digest
			}
			for _, member := range members {
				for _, criterion := range []string{"measurement_integrity", "workload_generalization", "containment", "patch_scope", "reproducibility", "source_safety"} {
					review.Findings = append(review.Findings, model.CompetitionReviewFinding{Model: member, Criterion: criterion, Disposition: "pass", ReasonCode: "APPROVED", Explanation: "Synthetic source evidence.", EvidenceSha256: digest})
				}
			}
			if fixture.name != "historical" {
				review.ProductionBenefitAddendum = model.CompetitionProductionBenefitReviewAddendum()
			}
			if fixture.name != "historical" && fixture.name != "new approval" && fixture.name != "not applicable approval" {
				review.RewardDisposition, review.SourceDisposition, review.ReasonCodes = "disqualified", "rejected", []string{"NO_PRODUCTION_BENEFIT"}
				review.Findings[1].Disposition, review.Findings[1].ReasonCode = "fail", "NO_PRODUCTION_BENEFIT"
			}
			switch fixture.name {
			case "missing addendum":
				review.ProductionBenefitAddendum = nil
			case "wrong hash":
				review.ProductionBenefitAddendum.Sha256 = strings.Repeat("0", 64)
			case "wrong version":
				review.ProductionBenefitAddendum.Version = "production-benefit/0"
			case "wrong criterion":
				review.Findings[1].Criterion = "source_safety"
			case "unmatched summary":
				review.ReasonCodes = []string{"UNSAFE_BEHAVIOR"}
			case "unmatched finding":
				review.Findings[1].Disposition, review.Findings[1].ReasonCode = "pass", "APPROVED"
			case "not applicable approval":
				review.Findings[1].Disposition = "not_applicable"
			}
			encoded, err := json.Marshal(review)
			if err != nil {
				t.Fatal(err)
			}
			var instance any
			if err := json.Unmarshal(encoded, &instance); err != nil {
				t.Fatal(err)
			}
			if err := resolved.Validate(instance); (err == nil) != fixture.valid {
				t.Errorf("snow=%t %s: valid=%t want=%t: %v", snow, fixture.name, err == nil, fixture.valid, err)
			}
		}
	}
}
