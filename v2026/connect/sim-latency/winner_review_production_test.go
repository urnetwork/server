// Deterministic native tests enforce the review contract; none claims real-model
// detection, production A/B evidence, or permission to activate the new broker.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/model"
)

// Both provider paths must refuse substituted addenda before transport, not
// rely on a model noticing that its instructions differ from the public policy.
func TestWinnerProductionBenefitReviewRequiresBoundContextBeforeProviders(t *testing.T) {
	for _, snow := range []bool{false, true} {
		for _, mutate := range []func(*winnerReviewBrokerRequest){
			func(r *winnerReviewBrokerRequest) { r.ProductionBenefitReview = nil },
			func(r *winnerReviewBrokerRequest) { r.ProductionBenefitReview.Version = "production-benefit/0" },
			func(r *winnerReviewBrokerRequest) { r.ProductionBenefitReview.AddendumSha256 = strings.Repeat("0", 64) },
			func(r *winnerReviewBrokerRequest) { r.ProductionBenefitReview.Addendum += " synthetic substitution" },
			func(r *winnerReviewBrokerRequest) { r.Template.ProductionBenefitAddendum = nil },
			func(r *winnerReviewBrokerRequest) {
				r.Template.ProductionBenefitAddendum.Sha256 = strings.Repeat("0", 64)
			},
		} {
			request, _ := winnerModelFixture()
			if snow {
				request, _ = winnerSnowJudgmentFixture()
			}
			mutate(&request)
			calls := 0
			client := &http.Client{Transport: winnerReviewTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("synthetic forbidden provider call")
			})}
			var err error
			if snow {
				_, err = executeWinnerSnow(context.Background(), client, request, defaultWinnerSnowModelSettings(), defaultWinnerSnowProviderSettings(), strings.Repeat("a", 64), t.TempDir())
			} else {
				_, err = executeWinnerModel(context.Background(), client, "synthetic-key", request, defaultWinnerModelSettings())
			}
			if err == nil || calls != 0 || !strings.Contains(err.Error(), "production-benefit") {
				t.Fatalf("snow=%t reached provider with invalid addendum: %d %v", snow, calls, err)
			}
		}
	}
}

// Generic passing criteria cannot substitute for concrete evidence. References
// are checked against supplied authenticated files, not LLM-invented filenames.
func TestWinnerProductionBenefitReviewRequiresAffirmativeCausalEvidence(t *testing.T) {
	for _, mutate := range []func(*winnerModelJudgment){
		func(j *winnerModelJudgment) { j.ProductionBenefit = nil },
		func(j *winnerModelJudgment) { j.ProductionBenefit.Version = "production-benefit/0" },
		func(j *winnerModelJudgment) { j.ProductionBenefit.AddendumSha256 = strings.Repeat("0", 64) },
		func(j *winnerModelJudgment) { j.ProductionBenefit.Disposition = "uncertain" },
		func(j *winnerModelJudgment) { j.ProductionBenefit.ProductionPath = " " },
		func(j *winnerModelJudgment) { j.ProductionBenefit.Mechanism = "" },
		func(j *winnerModelJudgment) { j.ProductionBenefit.ClientBenefit = "" },
		func(j *winnerModelJudgment) { j.ProductionBenefit.PreservedSemantics = "" },
		func(j *winnerModelJudgment) { j.ProductionBenefit.Explanation = strings.Repeat("x", 4097) },
		func(j *winnerModelJudgment) { j.ProductionBenefit.SourcePaths = nil },
		func(j *winnerModelJudgment) { j.ProductionBenefit.SourcePaths = []string{"connect/unavailable.go"} },
		func(j *winnerModelJudgment) {
			j.ProductionBenefit.SourcePaths = []string{"connect/example.go", "connect/example.go"}
		},
	} {
		request, judgment := winnerSnowJudgmentFixture()
		mutate(&judgment)
		if validateWinnerSnowMember(judgment, request) == nil {
			t.Fatal("unsupported production-benefit approval accepted")
		}
	}
}

// A no-benefit finding rejects eligibility without inventing misconduct. A
// missing causal contract instead remains uncertain and cannot enter that path.
func TestWinnerProductionBenefitReviewSeparatesIneligibilityAndUncertainty(t *testing.T) {
	request, judgment := winnerSnowJudgmentFixture()
	judgment.Decision, judgment.ReasonCodes = "disqualify", []string{"NO_PRODUCTION_BENEFIT"}
	judgment.ProductionBenefit.Disposition = "fail"
	judgment.ProductionBenefit.Explanation = "Synthetic no-op control has no production effect; this is not evidence of malicious intent."
	judgment.Findings[1].Disposition, judgment.Findings[1].ReasonCode = "fail", "NO_PRODUCTION_BENEFIT"
	if err := validateWinnerSnowMember(judgment, request); err != nil {
		t.Fatal("non-misconduct eligibility rejection refused", err)
	}
	judgment.ProductionBenefit.Disposition = "uncertain"
	if validateWinnerSnowMember(judgment, request) == nil {
		t.Fatal("uncertainty converted into proven ineligibility")
	}
	judgment.Decision, judgment.ReasonCodes = "uncertain", []string{"SOURCE_NOT_READY"}
	judgment.Findings[1].Disposition, judgment.Findings[1].ReasonCode = "uncertain", "SOURCE_NOT_READY"
	if err := validateWinnerSnowMember(judgment, request); err != nil {
		t.Fatal("missing-context hold is not representable", err)
	}
}

// Each union member supplies its own same-version original; one model cannot
// lend another an eligibility check or silently cover an unavailable assessment.
func TestWinnerProductionBenefitReviewUnionRequiresEveryMember(t *testing.T) {
	for _, mutate := range []func(*winnerModelJudgment){
		func(j *winnerModelJudgment) { j.ProductionBenefit = nil },
		func(j *winnerModelJudgment) { j.ProductionBenefit.AddendumSha256 = strings.Repeat("0", 64) },
		func(j *winnerModelJudgment) { j.ProductionBenefit.Version = "production-benefit/0" },
		func(j *winnerModelJudgment) { j.Decision, j.ProductionBenefit.Disposition = "uncertain", "uncertain" },
	} {
		request, members := winnerSnowMemberFixture(t)
		check := *members[1].Judgment.ProductionBenefit
		members[1].Judgment.ProductionBenefit = &check
		mutate(&members[1].Judgment)
		content, err := json.Marshal(members[1].Judgment)
		if err != nil {
			t.Fatal(err)
		}
		members[1].ProviderOutput = string(content)
		if aggregateWinnerSnow(members, defaultWinnerSnowProviderSettings(), request).Decision != "uncertain" {
			t.Fatal("partial eligibility assessment authorized the union")
		}
	}
}

// Cache identity includes the addendum, with no reroll when a completed old
// judgment lacks the new check or its interpretation changes.
func TestWinnerProductionBenefitReviewCacheBindsAddendum(t *testing.T) {
	request, _ := winnerModelFixture()
	path := filepath.Join(t.TempDir(), "completed.json")
	calls := 0
	run := func() ([]byte, error) { calls++; return []byte("synthetic completed judgment"), nil }
	if _, _, err := winnerReviewCompletedResponse(path, request, run); err != nil {
		t.Fatal(err)
	}
	request.ProductionBenefitReview.AddendumSha256 = strings.Repeat("0", 64)
	if _, _, err := winnerReviewCompletedResponse(path, request, run); err == nil || calls != 1 {
		t.Fatal("changed addendum reused or rerolled a completed judgment", err, calls)
	}
}

// The provider schema also requires the subcheck; native validation remains
// essential because a provider may ignore its schema or fill it incorrectly.
func TestWinnerProductionBenefitReviewSchemaRequiresSubcheck(t *testing.T) {
	request, judgment := winnerSnowJudgmentFixture()
	for i := range judgment.Findings {
		judgment.Findings[i].EvidenceSha256 = ""
	}
	content, err := json.Marshal(judgment)
	if err != nil {
		t.Fatal(err)
	}
	var instance map[string]any
	if err := json.Unmarshal(content, &instance); err != nil {
		t.Fatal(err)
	}
	for _, schema := range []map[string]any{winnerModelSchema(), winnerSnowModelSchema(request.Request.Policy)} {
		resolved := winnerSnowResolvedSchema(t, schema)
		if err := resolved.Validate(instance); err != nil {
			t.Fatal("complete synthetic check refused", err)
		}
		delete(instance, "production_benefit")
		if resolved.Validate(instance) == nil {
			t.Fatal("provider schema allowed generic approval without a production check")
		}
		if err := json.Unmarshal(content, &instance); err != nil {
			t.Fatal(err)
		}
	}
	// The public projection keeps six criteria and only the bound addendum.
	result := winnerModelBrokerResult(request, judgment, []byte("synthetic evidence"))
	if len(result.Review.Findings) != 6 || result.Review.ProductionBenefitAddendum == nil || *result.Review.ProductionBenefitAddendum != *model.CompetitionProductionBenefitReviewAddendum() {
		t.Fatal("public eligibility projection changed its census or lost policy identity")
	}
}

// Untouched scoring context cannot stand in for a changed production path.
// Supplemental instrument references are permitted alongside the real change.
func TestWinnerProductionBenefitReviewRequiresChangedProductPath(t *testing.T) {
	request, judgment := winnerSnowJudgmentFixture()
	judgment.ProductionBenefit.SourcePaths = []string{"server/connect/sim-latency/score.go"}
	if err := validateWinnerProductionBenefit(judgment, request); err == nil || !strings.Contains(err.Error(), "actual changed patch path") {
		t.Fatal("scorer-only causal claim authorized a product change", err)
	}
	judgment.ProductionBenefit.SourcePaths = append(judgment.ProductionBenefit.SourcePaths, "connect/example.go")
	if err := validateWinnerSnowMember(judgment, request); err != nil {
		t.Fatal("actual changed product path with supplemental context refused", err)
	}
}

// A no-op may be ineligible while remaining free of work suppression. Its new
// non-misconduct reason must not satisfy the independent traffic-abuse check.
func TestWinnerProductionBenefitReviewDoesNotInventTrafficMisconduct(t *testing.T) {
	request, judgment := winnerSnowJudgmentFixture()
	judgment.Decision, judgment.ReasonCodes = "disqualify", []string{"NO_PRODUCTION_BENEFIT"}
	judgment.ProductionBenefit.Disposition = "fail"
	judgment.Findings[1].Disposition, judgment.Findings[1].ReasonCode = "fail", "NO_PRODUCTION_BENEFIT"
	if err := validateWinnerSnowMember(judgment, request); err != nil {
		t.Fatal(err)
	}
	judgment.WorkloadPreservation.Disposition = "fail"
	if validateWinnerSnowMember(judgment, request) == nil {
		t.Fatal("lack of production benefit was treated as evidence of traffic misconduct")
	}
}

// The real single-provider projection retains one completed invalid response as
// uncertainty; it cannot fall back to generic passing findings or accuse fraud.
func TestWinnerProductionBenefitReviewSingleProviderCannotSkipCheck(t *testing.T) {
	for _, mutate := range []func(*winnerModelJudgment){
		func(j *winnerModelJudgment) { j.ProductionBenefit = nil },
		func(j *winnerModelJudgment) { j.ProductionBenefit.Disposition = "uncertain" },
		func(j *winnerModelJudgment) { j.ProductionBenefit.Version = "production-benefit/0" },
		func(j *winnerModelJudgment) {
			j.ProductionBenefit.SourcePaths = []string{"server/connect/sim-latency/score.go"}
		},
	} {
		request, judgment := winnerModelFixture()
		mutate(&judgment)
		output, err := json.Marshal(judgment)
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		client := &http.Client{Transport: winnerReviewTransport(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(winnerModelFixtureStream(t, string(output))))}, nil
		})}
		result, err := executeWinnerModel(t.Context(), client, "synthetic-key", request, defaultWinnerModelSettings())
		if err != nil || result == nil || calls != 1 || result.Review.RewardDisposition != "pending" || result.Review.SourceDisposition != "deferred" {
			t.Fatal("completed incomplete eligibility check did not hold without reroll", err, calls)
		}
		if validateWinnerReviewBrokerResult(result, request.Template, request.Request.Policy, request) == nil {
			t.Fatal("incomplete single-provider check gained review authority")
		}
	}
}

// A public projection cannot substitute a different addendum from the retained
// complete provider input, even if its other candidate/evidence fields agree.
func TestWinnerProductionBenefitReviewRejectsReceiptPolicySubstitution(t *testing.T) {
	result, request := winnerBrokerAuthenticatedFixture(t)
	if err := validateWinnerReviewBrokerResult(&result, request.Template, request.Request.Policy, request); err != nil {
		t.Fatal(err)
	}
	addendum := *result.Review.ProductionBenefitAddendum
	result.Review.ProductionBenefitAddendum = &addendum
	addendum.Sha256 = strings.Repeat("0", 64)
	if validateWinnerReviewBrokerResult(&result, request.Template, request.Request.Policy, request) == nil {
		t.Fatal("public receipt substituted the qualified eligibility policy")
	}
}
