package model

import (
	"encoding/json"
	"github.com/urnetwork/server"
	"strings"
	"testing"
	"time"
)

func TestCompetitionReviewHandoffBoundsAndLegacyEncoding(t *testing.T) {
	args := CompetitionReviewHandoffArgs{RoundId: server.NewId(), JobId: server.NewId(), RewardRevision: 1, Protocol: CompetitionReviewHandoffProtocol, TimeoutSeconds: 86400}
	for _, value := range []*string{&args.DecisionSha256, &args.SealSha256, &args.PatchSha256, &args.BaseSourceLockSha256, &args.ScoreSha256, &args.ArtifactManifestSha256, &args.BaselineSha256, &args.AuthorizationSha256, &args.ProviderSha256} {
		*value = strings.Repeat("a", 64)
	}
	if err := ValidateCompetitionReviewHandoffArgs(args); err != nil {
		t.Fatal(err)
	}
	for _, timeout := range []int{-1, 0, 59, 86401} {
		changed := args
		changed.TimeoutSeconds = timeout
		if ValidateCompetitionReviewHandoffArgs(changed) == nil {
			t.Fatal("unbounded review clock", timeout)
		}
	}
	plan := CompetitionReviewHandoffPlan{PlanSha256: strings.Repeat("b", 64), BundleSha256: strings.Repeat("c", 64), CallIds: []string{"qwen-scan-0001", "gemma-scan-0001"}}
	if err := ValidateCompetitionReviewHandoffPlan(plan); err != nil {
		t.Fatal(err)
	}
	plan.CallIds[1] = plan.CallIds[0]
	if ValidateCompetitionReviewHandoffPlan(plan) == nil {
		t.Fatal("duplicate call admitted")
	}
	for _, value := range []any{CompetitionIntegrityReviewRequest{}, CompetitionIntegrityReviewResult{}, CompetitionWinnerReviewCheckpoint{}} {
		content, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(content), "handoff_id") || strings.Contains(string(content), "model_completed_at") {
			t.Fatal("legacy wire identity changed", string(content))
		}
	}
}

func TestCompetitionReviewHandoffCompletionClockIsNotOperatorClock(t *testing.T) {
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	checkpoint := CompetitionWinnerReviewCheckpoint{StartedAt: now, Deadline: now.Add(time.Hour)}
	review := CompetitionIntegrityReviewResult{CreatedAt: now.Add(time.Minute)}
	if !CompetitionReviewWithinCheckpoint(&review, &checkpoint) {
		t.Fatal("legacy review refused")
	}
	review.CreatedAt = checkpoint.Deadline
	if CompetitionReviewWithinCheckpoint(&review, &checkpoint) {
		t.Fatal("deadline is exclusive")
	}
	checkpoint.HandoffId = server.NewId().String()
	review.Request.HandoffId = checkpoint.HandoffId
	completed := now.Add(time.Minute)
	review.ModelCompletedAt = &completed
	review.CreatedAt = now.Add(48 * time.Hour)
	if !CompetitionReviewWithinCheckpoint(&review, &checkpoint) {
		t.Fatal("timely model evidence lost at later explicit operator import")
	}
	completed = checkpoint.Deadline
	if CompetitionReviewWithinCheckpoint(&review, &checkpoint) {
		t.Fatal("late completion acquired authority")
	}
	completed = now.Add(-time.Nanosecond)
	if CompetitionReviewWithinCheckpoint(&review, &checkpoint) {
		t.Fatal("backdated completion acquired authority")
	}
	completed = now
	review.Request.HandoffId = server.NewId().String()
	if CompetitionReviewWithinCheckpoint(&review, &checkpoint) {
		t.Fatal("cross-owner clock acquired authority")
	}
	checkpoint.HandoffId = ""
	review.Request.HandoffId = ""
	if CompetitionReviewWithinCheckpoint(&review, &checkpoint) {
		t.Fatal("legacy review smuggled a model clock")
	}
}
