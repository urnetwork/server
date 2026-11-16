// Synthetic public receipts exercise the bounded predecessor chain without
// production identities, private snapshots or native inventory.
package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Both cycles use the same paid admission and frozen evaluator scope.
func secondRecoveryCycleForTest(t *testing.T) CompetitionJobReevaluationCycle {
	t.Helper()
	sha := strings.Repeat("a", 64)
	start := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	firstStart := start.Add(time.Minute)
	previous := CompetitionJobReevaluationCycle{
		Schema: 2, Environment: "synthetic", CompetitionId: "synthetic-competition", SeasonId: "synthetic-season",
		JobId: server.NewId(), RoundId: server.NewId(), PatchSha256: sha, RoundPolicySha256: sha, BaselineSha256: sha,
		Cycle: 1, AuthorizationSha256: sha, EvidenceSha256: sha, PreviousTerminalSha256: sha,
		PreviousFailure: CompetitionEvaluationFailure{Kind: "infrastructure", Code: "executor_process_failed"},
		IncidentClass:   CompetitionJobReevaluationOperatorRequestedPatchRetry, RuntimeQualificationSha256: sha,
		FixedSourceCommit: strings.Repeat("b", 40), FixedWorkerSha256: sha, FixedWorkerImageDigest: "sha256:" + sha,
		ApiImageDigest: "sha256:" + sha, AuthorizedAt: start.Add(time.Second), StartedAt: &firstStart, ExecutionTimeoutSeconds: 3 * 60 * 60,
		Distributed: &CompetitionDistributedJobReevaluationCycle{Schema: 1, Reason: CompetitionJobReevaluationOperatorRequestedPatchRetry,
			OriginalHostId: "synthetic-host", OriginalSlotId: 1, OriginalFencingToken: 1, OriginalRunId: sha,
			OriginalStartedAt: start, OriginalDeadlineAt: start.Add(3 * time.Hour), SourceLockSha256: sha,
			EvaluatorImageDigest: "sha256:" + sha, PolicyApprovalSha256: sha, ClosureEvidenceSha256: sha, NativeProofSha256: sha,
			PatchRetry: &CompetitionOperatorRequestedPatchRetry{Schema: 1, PatchSha256: sha, ApplicabilityEvidenceSha256: sha}},
	}
	var err error
	previous.ReceiptSha256, err = CompetitionJobReevaluationCycleDigest(previous)
	if err != nil || ValidateCompetitionJobReevaluationCycle(&previous) != nil {
		t.Fatal("invalid synthetic first recovery", err)
	}
	cycle := previous
	distributed := *previous.Distributed
	distributed.OriginalStartedAt, distributed.OriginalDeadlineAt = firstStart, firstStart.Add(3*time.Hour)
	cycle.Schema, cycle.Cycle, cycle.StartedAt, cycle.Distributed = 3, 2, nil, &distributed
	cycle.AuthorizedAt = firstStart.Add(time.Minute)
	cycle.SecondRecovery = &CompetitionOperatorSecondRecovery{Schema: 1, JobId: previous.JobId,
		PreviousReceiptSha256: previous.ReceiptSha256, ApprovalEvidenceSha256: sha, PreviousRecovery: &previous}
	cycle.ReceiptSha256, err = CompetitionJobReevaluationCycleDigest(cycle)
	if err != nil {
		t.Fatal(err)
	}
	return cycle
}

// The latest public receipt retains the complete authenticated first receipt.
func TestCompetitionSecondRecoveryPublicReceiptKeepsPredecessor(t *testing.T) {
	cycle := secondRecoveryCycleForTest(t)
	if err := ValidateCompetitionJobReevaluationCycle(&cycle); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(cycle)
	if err != nil {
		t.Fatal(err)
	}
	var decoded CompetitionJobReevaluationCycle
	if json.Unmarshal(encoded, &decoded) != nil || ValidateCompetitionJobReevaluationCycle(&decoded) != nil ||
		decoded.SecondRecovery.PreviousRecovery.ReceiptSha256 != cycle.SecondRecovery.PreviousReceiptSha256 {
		t.Fatal("public JSON lost predecessor authority")
	}
	previous, err := json.Marshal(cycle.SecondRecovery.PreviousRecovery)
	if err != nil || strings.Contains(string(previous), "second_recovery") {
		t.Fatal("legacy receipt bytes gained a successor field", err)
	}
}

// Resealing the current receipt cannot manufacture another scope or a third cycle.
func TestCompetitionSecondRecoveryPublicReceiptRejectsChangedChain(t *testing.T) {
	original := secondRecoveryCycleForTest(t)
	for _, mutate := range []func(*CompetitionJobReevaluationCycle){
		func(c *CompetitionJobReevaluationCycle) { c.Cycle = 3 },
		func(c *CompetitionJobReevaluationCycle) { c.SecondRecovery.JobId = server.NewId() },
		func(c *CompetitionJobReevaluationCycle) {
			c.SecondRecovery.PreviousReceiptSha256 = strings.Repeat("f", 64)
		},
		func(c *CompetitionJobReevaluationCycle) { c.SecondRecovery.ApprovalEvidenceSha256 = "" },
		func(c *CompetitionJobReevaluationCycle) { c.SecondRecovery.PreviousRecovery.StartedAt = nil },
		func(c *CompetitionJobReevaluationCycle) { c.SecondRecovery.PreviousRecovery = nil },
		func(c *CompetitionJobReevaluationCycle) { c.Schema = 2 },
	} {
		encoded, _ := json.Marshal(original)
		var changed CompetitionJobReevaluationCycle
		if err := json.Unmarshal(encoded, &changed); err != nil {
			t.Fatal(err)
		}
		mutate(&changed)
		changed.ReceiptSha256, _ = CompetitionJobReevaluationCycleDigest(changed)
		if ValidateCompetitionJobReevaluationCycle(&changed) == nil {
			t.Fatal("changed public successor chain was accepted")
		}
	}
}
