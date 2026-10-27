// Provider reasons describe all six findings. Native reward dispositions use
// only compatible summary reasons without discarding or rewriting originals.
package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/server/model"
)

// The old direct projection rejected a substantiated fraud finding merely
// because the same model also summarized its passing criteria as APPROVED.
func TestWinnerSnowMemberProjectsMixedReasonsWithoutChangingEvidence(t *testing.T) {
	request, judgment := winnerModelFixture()
	judgment.Decision = "disqualify"
	judgment.ReasonCodes = []string{"MEASUREMENT_FABRICATION", "APPROVED"}
	judgment.Findings[0].Disposition, judgment.Findings[0].ReasonCode = "fail", "MEASUREMENT_FABRICATION"
	original, err := json.Marshal(judgment)
	if err != nil {
		t.Fatal(err)
	}
	probe := request.Template
	probe.RewardDisposition, probe.SourceDisposition = "disqualified", "rejected"
	probe.ReasonCodes, probe.PublicExplanation = judgment.ReasonCodes, judgment.PublicExplanation
	probe.Findings = append([]model.CompetitionReviewFinding{}, judgment.Findings...)
	probe.PrivateEvidenceSha256 = strings.Repeat("a", 64)
	probe.WithheldEvidenceExplanation = "Synthetic private evidence."
	for index := range probe.Findings {
		probe.Findings[index].EvidenceSha256 = probe.PrivateEvidenceSha256
	}
	policy := request.Request.Policy
	policy.WinnerReviewPolicy, policy.WinnerReviewTimeoutSeconds = "", 0
	if err := model.ValidateCompetitionIntegrityReview(probe, policy); err == nil || !strings.Contains(err.Error(), "cannot disqualify a reward") {
		t.Fatalf("unfiltered old projection did not reproduce the failure: %v", err)
	}
	if err := validateWinnerSnowMember(judgment, request); err != nil {
		t.Fatal("passing-criterion reason invalidated a substantiated rejection", err)
	}
	after, err := json.Marshal(judgment)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("projection rewrote the retained provider judgment", err)
	}
}

// Projection cannot invent an eligible failure from source-only concerns,
// unsupported global reasons, or an otherwise all-passing judgment.
func TestWinnerSnowMemberProjectionCannotInventRewardDisqualification(t *testing.T) {
	for _, fixture := range []struct {
		name        string
		criterion   int
		disposition string
		finding     string
		reasons     []string
	}{
		{name: "source only", criterion: 5, disposition: "fail", finding: "SOURCE_NOT_READY", reasons: []string{"SOURCE_NOT_READY", "APPROVED"}},
		{name: "source only with unsupported reward summary", criterion: 5, disposition: "fail", finding: "SOURCE_NOT_READY", reasons: []string{"MEASUREMENT_FABRICATION", "SOURCE_NOT_READY", "APPROVED"}},
		{name: "unsupported failure", criterion: 0, disposition: "fail", finding: "MEASUREMENT_FABRICATION", reasons: []string{"SCORE_TAMPERING", "APPROVED"}},
		{name: "no failed finding", criterion: 0, disposition: "pass", finding: "APPROVED", reasons: []string{"MEASUREMENT_FABRICATION", "APPROVED"}},
	} {
		request, judgment := winnerModelFixture()
		judgment.Decision, judgment.ReasonCodes = "disqualify", fixture.reasons
		judgment.Findings[fixture.criterion].Disposition = fixture.disposition
		judgment.Findings[fixture.criterion].ReasonCode = fixture.finding
		if err := validateWinnerSnowMember(judgment, request); err == nil {
			t.Fatalf("%s acquired unsupported reward authority", fixture.name)
		}
	}
}

// The composed union retains eighteen original attributed findings and the
// mixed member reasons while publishing only the compatible reward summary.
func TestWinnerSnowUnionRetainsMixedMemberReasonsAndAllFindings(t *testing.T) {
	request, members := winnerSnowMemberFixture(t)
	judgment := members[2].Judgment
	judgment.Findings = append([]model.CompetitionReviewFinding{}, judgment.Findings...)
	judgment.Decision, judgment.ReasonCodes = "disqualify", []string{"MEASUREMENT_FABRICATION", "APPROVED"}
	judgment.Findings[0].Disposition, judgment.Findings[0].ReasonCode = "fail", "MEASUREMENT_FABRICATION"
	content, err := json.Marshal(judgment)
	if err != nil {
		t.Fatal(err)
	}
	members[2].Judgment, members[2].ProviderOutput = judgment, string(content)
	original, err := json.Marshal(members)
	if err != nil {
		t.Fatal(err)
	}
	combined := aggregateWinnerSnow(members, defaultWinnerSnowProviderSettings(), request)
	if combined.Decision != "disqualify" || len(combined.Findings) != 18 || len(combined.ReasonCodes) != 1 || combined.ReasonCodes[0] != "MEASUREMENT_FABRICATION" {
		t.Fatal("mixed summary lost rejection or the complete findings union", combined.Decision, combined.ReasonCodes, len(combined.Findings))
	}
	after, err := json.Marshal(members)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("aggregation rewrote original member evidence", err)
	}
}
