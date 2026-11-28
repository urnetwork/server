// Model output contracts must not offer operator-only lifecycle decisions.
package main

import (
	"encoding/json"
	"testing"
)

// Global incident and appeal codes belong to operator decisions, not a model's
// static review, even when the provider frontend enforces only the schema.
func TestWinnerSnowSchemaRejectsOperatorOnlySummaryReasons(t *testing.T) {
	request, judgment := winnerSnowJudgmentFixture()
	judgment.Decision = "uncertain"
	for index := range judgment.Findings {
		judgment.Findings[index].EvidenceSha256 = ""
	}
	schema := winnerSnowResolvedSchema(t, winnerSnowModelSchema(request.Request.Policy))
	for _, reason := range []string{"EVALUATION_INTEGRITY_INCIDENT", "APPEAL_UPHELD"} {
		judgment.ReasonCodes = []string{reason}
		content, err := json.Marshal(judgment)
		if err != nil {
			t.Fatal(err)
		}
		var instance any
		if err := json.Unmarshal(content, &instance); err != nil {
			t.Fatal(err)
		}
		if schema.Validate(instance) == nil {
			t.Errorf("model schema offered operator-only summary reason %s", reason)
		}
	}
}

// Restrict individual findings too; narrowing only the summary would leave
// the same invalid authority suggestion inside the retained member findings.
func TestWinnerSnowSchemaRejectsOperatorOnlyFindingReasons(t *testing.T) {
	request, judgment := winnerSnowJudgmentFixture()
	judgment.Decision, judgment.ReasonCodes = "uncertain", []string{"SOURCE_NOT_READY"}
	for index := range judgment.Findings {
		judgment.Findings[index].EvidenceSha256 = ""
	}
	schema := winnerSnowResolvedSchema(t, winnerSnowModelSchema(request.Request.Policy))
	for _, reason := range []string{"EVALUATION_INTEGRITY_INCIDENT", "APPEAL_UPHELD"} {
		judgment.Findings[0].ReasonCode = reason
		content, err := json.Marshal(judgment)
		if err != nil {
			t.Fatal(err)
		}
		var instance any
		if err := json.Unmarshal(content, &instance); err != nil {
			t.Fatal(err)
		}
		if schema.Validate(instance) == nil {
			t.Errorf("model schema offered operator-only finding reason %s", reason)
		}
	}
}

// A model cannot propose a reward-identity exclusion the season never adopted.
// The prospective exclusion policy still has its original supported reasons.
func TestWinnerSnowSchemaScopesRepeatReasonsToFrozenPolicy(t *testing.T) {
	request, judgment := winnerSnowJudgmentFixture()
	for index := range judgment.Findings {
		judgment.Findings[index].EvidenceSha256 = ""
	}
	for _, policy := range []string{"allow", "exclude_exact_identity"} {
		request.Request.Policy.RepeatRewardPolicy = policy
		schema := winnerSnowResolvedSchema(t, winnerSnowModelSchema(request.Request.Policy))
		for _, reason := range []string{"REWARD_DUPLICATE", "REWARD_RULE_EVASION"} {
			judgment.Decision, judgment.ReasonCodes = "disqualify", []string{reason}
			judgment.Findings[0].Disposition, judgment.Findings[0].ReasonCode = "fail", reason
			content, err := json.Marshal(judgment)
			if err != nil {
				t.Fatal(err)
			}
			var instance any
			if err := json.Unmarshal(content, &instance); err != nil {
				t.Fatal(err)
			}
			expected := policy == "exclude_exact_identity"
			if (schema.Validate(instance) == nil) != expected || (validateWinnerSnowMember(judgment, request) == nil) != expected {
				t.Fatalf("schema/native exclusion authority differs: policy=%s reason=%s", policy, reason)
			}
		}
	}
}

// Source-only uncertainty remains expressible but cannot be converted into
// reward misconduct by the narrower schema or the unchanged native validator.
func TestWinnerSnowSchemaPreservesSourceOnlyUncertainty(t *testing.T) {
	request, judgment := winnerSnowJudgmentFixture()
	judgment.Decision, judgment.ReasonCodes = "uncertain", []string{"PRODUCT_INTEGRATION_RISK"}
	for index := range judgment.Findings {
		judgment.Findings[index].EvidenceSha256 = ""
	}
	judgment.Findings[0].Disposition, judgment.Findings[0].ReasonCode = "uncertain", "PRODUCT_INTEGRATION_RISK"
	content, err := json.Marshal(judgment)
	if err != nil {
		t.Fatal(err)
	}
	var instance any
	if err := json.Unmarshal(content, &instance); err != nil {
		t.Fatal(err)
	}
	if err := winnerSnowResolvedSchema(t, winnerSnowModelSchema(request.Request.Policy)).Validate(instance); err != nil {
		t.Fatal("source-only uncertainty was erased from the model contract", err)
	}
	if err := validateWinnerSnowMember(judgment, request); err != nil {
		t.Fatal("native source-only uncertainty was erased", err)
	}
	judgment.Decision, judgment.Findings[0].Disposition = "disqualify", "fail"
	if validateWinnerSnowMember(judgment, request) == nil {
		t.Fatal("source-only uncertainty became a reward disqualification")
	}
}
