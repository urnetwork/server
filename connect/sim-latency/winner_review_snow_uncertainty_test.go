// The retained Muse rejection had one substantiated failure and one uncertain
// criterion. Synthetic tuples exercise that shape without any provider request.
package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/server/model"
)

// These are the observed statuses/reasons only; no provider explanation, prompt
// or candidate bytes are copied into a regression fixture.
func winnerSnowMixedUncertaintyFixture() (winnerReviewBrokerRequest, winnerModelJudgment) {
	request, judgment := winnerSnowJudgmentFixture()
	judgment.Decision, judgment.ReasonCodes = "disqualify", []string{"MEASUREMENT_FABRICATION"}
	judgment.Findings[0].Disposition, judgment.Findings[0].ReasonCode = "fail", "MEASUREMENT_FABRICATION"
	judgment.Findings[1].Disposition, judgment.Findings[1].ReasonCode = "uncertain", "SOURCE_NOT_READY"
	return request, judgment
}

// Execute the old API projection as a deterministic negative control, then
// the real fixed member validator. No criterion is converted to a passing one.
func TestWinnerSnowMemberPreservesSupportedRejectionWithUncertainty(t *testing.T) {
	request, judgment := winnerSnowMixedUncertaintyFixture()
	before, err := json.Marshal(judgment)
	if err != nil {
		t.Fatal(err)
	}
	probe := request.Template
	probe.RewardDisposition, probe.SourceDisposition = "disqualified", "rejected"
	probe.ReasonCodes, probe.PublicExplanation, probe.Findings = judgment.ReasonCodes, judgment.PublicExplanation, append([]model.CompetitionReviewFinding{}, judgment.Findings...)
	probe.PrivateEvidenceSha256, probe.WithheldEvidenceExplanation = strings.Repeat("a", 64), "Synthetic original member evidence."
	for index := range probe.Findings {
		probe.Findings[index].EvidenceSha256 = probe.PrivateEvidenceSha256
	}
	legacyPolicy := request.Request.Policy
	legacyPolicy.WinnerReviewPolicy, legacyPolicy.WinnerReviewTimeoutSeconds = "", 0
	if err := model.ValidateCompetitionIntegrityReview(probe, legacyPolicy); err == nil || !strings.Contains(err.Error(), "invalid finding disposition") {
		t.Fatal("old policy clearing did not reproduce the actual Muse refusal", err)
	}
	if err := validateWinnerSnowMember(judgment, request); err != nil {
		t.Fatal("supported Snow rejection lost a genuine uncertain criterion", err)
	}
	after, err := json.Marshal(judgment)
	if err != nil || !bytes.Equal(before, after) || judgment.Findings[1].Disposition != "uncertain" {
		t.Fatal("member validation rewrote original evidence", err)
	}
}

// Neither an uncertainty nor a source-only concern can stand in for an
// eligible substantiated failure, and uncertainty can never authorize approval.
func TestWinnerSnowMixedUncertaintyCannotInventApprovalOrRejection(t *testing.T) {
	for _, sample := range []struct {
		name   string
		mutate func(*winnerModelJudgment)
	}{
		{name: "approve with uncertainty", mutate: func(j *winnerModelJudgment) {
			j.Decision, j.ReasonCodes = "approve", []string{"APPROVED"}
			j.Findings[0].Disposition, j.Findings[0].ReasonCode = "pass", "APPROVED"
		}},
		{name: "uncertainty only", mutate: func(j *winnerModelJudgment) { j.Findings[0].Disposition, j.Findings[0].ReasonCode = "pass", "APPROVED" }},
		{name: "source-only failed finding", mutate: func(j *winnerModelJudgment) {
			j.Findings[0].Disposition, j.Findings[0].ReasonCode = "pass", "APPROVED"
			j.Findings[5].Disposition, j.Findings[5].ReasonCode = "fail", "SOURCE_NOT_READY"
			j.ReasonCodes = []string{"MEASUREMENT_FABRICATION", "SOURCE_NOT_READY", "APPROVED"}
		}},
		{name: "unsupported global reason", mutate: func(j *winnerModelJudgment) { j.ReasonCodes = []string{"SCORE_TAMPERING", "APPROVED"} }},
	} {
		request, judgment := winnerSnowMixedUncertaintyFixture()
		sample.mutate(&judgment)
		if err := validateWinnerSnowMember(judgment, request); err == nil {
			t.Errorf("%s acquired unsupported Snow authority", sample.name)
		}
	}
}

// The real union keeps the uncertain original alongside every other member
// finding; the unchanged public native validator still receives all twelve.
func TestWinnerSnowUnionPreservesUncertainFindingAndNativeQuorum(t *testing.T) {
	request, members := winnerSnowMemberFixture(t)
	_, mixed := winnerSnowMixedUncertaintyFixture()
	content, err := json.Marshal(mixed)
	if err != nil {
		t.Fatal(err)
	}
	members[1].Judgment, members[1].ProviderOutput = mixed, string(content)
	before, err := json.Marshal(members)
	if err != nil {
		t.Fatal(err)
	}
	combined := aggregateWinnerSnow(members, defaultWinnerSnowProviderSettings(), request)
	if combined.Decision != "disqualify" || len(combined.Findings) != 12 || combined.Findings[7].Model != "gemma-4-26b-a4b" || combined.Findings[7].Disposition != "uncertain" || combined.Findings[7].ReasonCode != "SOURCE_NOT_READY" {
		t.Fatal("mixed rejection lost its genuine member census", combined.Decision, len(combined.Findings))
	}
	result := winnerSnowBrokerResult(request, combined, []byte("Synthetic complete two-member retained evidence."))
	if err := model.ValidateCompetitionIntegrityReview(result.Review, request.Request.Policy); err != nil {
		t.Fatal("complete Snow rejection failed unchanged native API validation", err)
	}
	after, err := json.Marshal(members)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("aggregation rewrote original member judgments or output", err)
	}
}
