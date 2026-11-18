// Schema bypass must not grant a model the operator's lifecycle authority.
package main

import "testing"

// The old native vocabulary accepted operator incident/appeal codes for
// approval and returned early for uncertainty. Both paths must refuse them.
func TestWinnerSnowNativeRejectsOperatorOnlySummaryReasons(t *testing.T) {
	request, judgment := winnerSnowJudgmentFixture()
	for _, decision := range []string{"approve", "uncertain"} {
		for _, reason := range []string{"EVALUATION_INTEGRITY_INCIDENT", "APPEAL_UPHELD"} {
			judgment.Decision, judgment.ReasonCodes = decision, []string{reason}
			if validateWinnerSnowMember(judgment, request) == nil {
				t.Errorf("native %s accepted operator-only summary reason %s", decision, reason)
			}
		}
	}
}

// Findings are checked before the uncertainty shortcut as well as before an
// approval; a valid summary cannot smuggle an operator action into a finding.
func TestWinnerSnowNativeRejectsOperatorOnlyFindingReasons(t *testing.T) {
	request, judgment := winnerSnowJudgmentFixture()
	for _, decision := range []string{"approve", "uncertain"} {
		judgment.Decision = decision
		for _, reason := range []string{"EVALUATION_INTEGRITY_INCIDENT", "APPEAL_UPHELD"} {
			judgment.Findings[0].ReasonCode = reason
			if validateWinnerSnowMember(judgment, request) == nil {
				t.Errorf("native %s accepted operator-only finding reason %s", decision, reason)
			}
		}
	}
}
