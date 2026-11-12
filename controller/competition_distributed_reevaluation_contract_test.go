// The actual companion OpenAPI contract must accept the explicit distributed
// projection without admitting private proof or relabeling singleton classes.
package controller

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/urnetwork/server/competitionagent"
	"github.com/urnetwork/server/model"
	"gopkg.in/yaml.v3"
)

// The test override points at the separately frozen companion worktree during
// isolated QA. Normal sibling checkouts use their real checked-in specification.
func competitionDistributedRecoveryContract(t testing.TB) func(any) error {
	t.Helper()
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
	encoded, err := json.Marshal(map[string]any{"$ref": "#/$defs/CompetitionJobReevaluationCycle", "$defs": document.Components.Schemas})
	if err != nil {
		t.Fatal(err)
	}
	encoded = bytes.ReplaceAll(encoded, []byte("#/components/schemas/"), []byte("#/$defs/"))
	var schema jsonschema.Schema
	if err := json.Unmarshal(encoded, &schema); err != nil {
		t.Fatal(err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	return resolved.Validate
}

// Synthetic public history binds its normal three-hour old clock, distinct
// source/baseline pins, and digest-only native closure provenance.
func competitionDistributedRecoveryCycleForTest(t testing.TB) model.CompetitionJobReevaluationCycle {
	t.Helper()
	cycle := newCompetitionReevaluationCycleTestFixture(t)
	cycle.Schema, cycle.IncidentClass = 2, model.CompetitionJobReevaluationDistributedInfrastructure
	cycle.PreviousFailure.Code = "evaluation_time_budget_exhausted"
	started := cycle.AuthorizedAt.Add(-4 * time.Hour)
	cycle.Distributed = &model.CompetitionDistributedJobReevaluationCycle{Schema: 1, Reason: model.CompetitionDistributedReevaluationRetentionBudget,
		OriginalHostId: "synthetic-executor", OriginalSlotId: 1, OriginalFencingToken: 4, OriginalRunId: competitionagent.RunId(cycle.JobId.String(), 0, 1),
		OriginalStartedAt: started, OriginalDeadlineAt: started.Add(3 * time.Hour), SourceLockSha256: strings.Repeat("c", 64), EvaluatorImageDigest: "sha256:" + strings.Repeat("d", 64),
		PolicyApprovalSha256: strings.Repeat("e", 64), ClosureEvidenceSha256: strings.Repeat("f", 64), NativeProofSha256: strings.Repeat("1", 64)}
	cycle.ReceiptSha256 = competitionRecoveryTestDigest(t, func() model.CompetitionJobReevaluationCycle {
		copy := cycle
		copy.ReceiptSha256 = ""
		copy.StartedAt = nil
		return copy
	}())
	if err := model.ValidateCompetitionJobReevaluationCycle(&cycle); err != nil {
		t.Fatal(err)
	}
	return cycle
}

// Both versions are validated against actual YAML, not duplicated enum lists.
func TestCompetitionDistributedRecoveryPublicContractKeepsSingletonAndNewClass(t *testing.T) {
	validate := competitionDistributedRecoveryContract(t)
	for _, cycle := range []model.CompetitionJobReevaluationCycle{newCompetitionReevaluationCycleTestFixture(t), competitionDistributedRecoveryCycleForTest(t)} {
		encoded, err := json.Marshal(cycle)
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]any
		if err := json.Unmarshal(encoded, &object); err != nil {
			t.Fatal(err)
		}
		if err := validate(object); err != nil {
			t.Fatalf("declared cycle schema %d rejected: %v", cycle.Schema, err)
		}
	}
}

// A public reader cannot accidentally accept private native inventories or a
// mixed version/class marker just because the outer JSON is well-formed.
func TestCompetitionDistributedRecoveryPublicContractRejectsMixedAuthorityAndLeaks(t *testing.T) {
	validate := competitionDistributedRecoveryContract(t)
	cycle := competitionDistributedRecoveryCycleForTest(t)
	for _, change := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "private-proof", mutate: func(o map[string]any) {
			o["distributed_proof"] = map[string]any{"files": []string{"synthetic-private-input"}}
		}},
		{name: "old-schema", mutate: func(o map[string]any) { o["schema"] = float64(1) }},
		{name: "missing-distributed", mutate: func(o map[string]any) { delete(o, "distributed") }},
		{name: "singleton-label", mutate: func(o map[string]any) { o["incident_class"] = model.CompetitionJobReevaluationHeartbeatBudget }},
		{name: "unknown-label", mutate: func(o map[string]any) { o["incident_class"] = "synthetic_unknown_class" }},
		{name: "unknown-diagnosis", mutate: func(o map[string]any) { o["distributed"].(map[string]any)["reason"] = "synthetic_unknown_reason" }},
		{name: "public-native-inventory", mutate: func(o map[string]any) {
			o["distributed"].(map[string]any)["files"] = []string{"synthetic-private-input"}
		}},
	} {
		encoded, err := json.Marshal(cycle)
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]any
		if err := json.Unmarshal(encoded, &object); err != nil {
			t.Fatal(err)
		}
		change.mutate(object)
		if err := validate(object); err == nil {
			t.Fatalf("%s accepted by actual public contract", change.name)
		}
	}
}
