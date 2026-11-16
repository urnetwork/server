package work

import (
	"context"
	"time"
)

// The seam provider_egress_tls_judge_export_test.go drives the judge through,
// so that test reads the same against a judge that carried nothing between
// runs: one run of a new chain.
func testingRunTlsJudge(ctx context.Context, now time.Time, sources *providerEgressTlsJudgeSources) (*JudgeProviderEgressTlsFindingsResult, error) {
	return judgeProviderEgressTlsFindings(ctx, now, &JudgeProviderEgressTlsFindingsArgs{}, sources)
}
