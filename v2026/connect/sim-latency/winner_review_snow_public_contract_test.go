// The public contract preserves exact current and historical Snow finding sets.
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/model"
	"gopkg.in/yaml.v3"
)

// Compile the published request schema with the same pinned JSON Schema engine
// as provider-contract tests; no model or hand-written schema validator runs.
func TestWinnerSnowPublicContractKeepsExactVersionedCensuses(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "..", "..", "sn", "api", "competition.yml"))
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
	root := map[string]any{"$ref": "#/$defs/CompetitionIntegrityReviewRequest", "$defs": document.Components.Schemas}
	schemaBytes, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	schemaBytes = bytes.ReplaceAll(schemaBytes, []byte("#/components/schemas/"), []byte("#/$defs/"))
	var schema map[string]any
	if err := json.Unmarshal(schemaBytes, &schema); err != nil {
		t.Fatal(err)
	}
	resolved := winnerSnowResolvedSchema(t, schema)
	request, members := winnerSnowMemberFixture(t)
	combined := aggregateWinnerSnow(members, defaultWinnerSnowProviderSettings(), request)
	current := winnerSnowBrokerResult(request, combined, []byte("Synthetic public contract evidence.")).Review
	current.ExpectedDecisionSha256 = strings.Repeat("a", 64)
	historical := current
	historical.ReviewerModel = model.CompetitionSnowWinnerReviewModelV1
	historical.Findings = append([]model.CompetitionReviewFinding{}, current.Findings...)
	for _, finding := range current.Findings[:6] {
		finding.Model = "muse-glimmer-30b"
		historical.Findings = append(historical.Findings, finding)
	}
	for _, review := range []model.CompetitionIntegrityReviewRequest{current, historical} {
		encode := func(value model.CompetitionIntegrityReviewRequest) any {
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var instance any
			if err := json.Unmarshal(encoded, &instance); err != nil {
				t.Fatal(err)
			}
			return instance
		}
		if err := resolved.Validate(encode(review)); err != nil {
			t.Fatalf("complete %s contract refused: %v", review.ReviewerModel, err)
		}
		for _, mutation := range []string{"missing", "duplicate", "other version"} {
			candidate := review
			candidate.Findings = append([]model.CompetitionReviewFinding{}, review.Findings...)
			switch mutation {
			case "missing":
				candidate.Findings = candidate.Findings[1:]
			case "duplicate":
				candidate.Findings[1] = candidate.Findings[0]
			case "other version":
				candidate.ReviewerModel = model.CompetitionSnowWinnerReviewModel
				if review.ReviewerModel == model.CompetitionSnowWinnerReviewModel {
					candidate.ReviewerModel = model.CompetitionSnowWinnerReviewModelV1
				}
			}
			if resolved.Validate(encode(candidate)) == nil {
				t.Errorf("%s accepted %s finding set", review.ReviewerModel, mutation)
			}
		}
	}
}
