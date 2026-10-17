// Source-choice fixtures bind eligibility to pre-deadline isolated-build
// evidence, without executing candidate code or inferring approval from rewards.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Formatting the same parsed score must not replace the immutable score file
// hash used by genuine review, threshold evidence and exact-source activation.
func TestReviewedSourceSignificanceRetainsArchivedScoreHash(t *testing.T) {
	value := validSourceSignificance()
	approved := &controller.CompetitionReviewedSource{ScoreSha256: strings.Repeat("d", 64), Score: controller.ScoreResult{Significance: &controller.ScoreSignificance{
		Method: value.Method, Alpha: value.Alpha, ReplicateCount: value.ReplicateCount, BaselineMeanRawScore: value.BaselineMeanRawScore, CandidateMeanRawScore: value.CandidateMeanRawScore,
		BaselineSampleVariance: &value.BaselineSampleVariance, CandidateSampleVariance: &value.CandidateSampleVariance, ObservedImprovementPercent: value.ObservedImprovementPercent,
		TakeoverMarginPercent: value.TakeoverMarginPercent, MinimumSignificantImprovementPercent: &value.MinimumSignificantImprovementPercent, RequiredImprovementPercent: &value.RequiredImprovementPercent,
		OneSidedPValue: &value.OneSidedPValue, NextEpochMinimumImprovementPercent: &value.NextEpochMinimumImprovementPercent, RecommendedNextEpochTakeoverMarginPercent: &value.RecommendedNextEpochTakeoverMarginPercent,
	}}}
	projection, err := reviewedSourceSignificance(approved)
	if err != nil || projection.ScoreSha256 != approved.ScoreSha256 {
		t.Fatalf("archive identity was replaced: %+v %v", projection, err)
	}
	approved.ScoreSha256 = ""
	if _, err := reviewedSourceSignificance(approved); err == nil {
		t.Fatal("missing archived score identity was invented")
	}
}

// Returns synthetic reviewed-source evidence ready before the source choice.
func reviewedProposalFixture(t *testing.T) (*model.CompetitionSourceActivation, reviewedSourceProposal, time.Time) {
	t.Helper()
	deadline := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	current := &model.CompetitionSourceActivation{Commits: sourceTestCommits(strings.Repeat("a", 40)), SourceGeneration: 0, Threshold: 0.161}
	current.SourceLockSha256, _ = model.CompetitionSourceLockDigest(current.Commits)
	commits := sourceTestCommits(strings.Repeat("a", 40))
	commits["server"] = strings.Repeat("b", 40)
	lockDigest, err := model.CompetitionSourceLockDigest(commits)
	if err != nil {
		t.Fatal(err)
	}
	reviewId := server.NewId()
	patchDigest := strings.Repeat("c", 64)
	receipt := reviewedSourceBuildReceipt{Schema: 1, ReviewId: reviewId, PatchSha256: patchDigest, SourceLockSha256: lockDigest,
		EvaluatorImageDigest: "sha256:" + strings.Repeat("d", 64), HostQualificationSha256: strings.Repeat("e", 64), ContainmentSha256: strings.Repeat("f", 64), TestEvidenceSha256: strings.Repeat("1", 64),
		SandboxId: "synthetic-isolated-build", Offline: true, NoMainCredentials: true, ReadOnlySource: true, Passed: true, TestModel: "gpt-6-sol", TestReasoning: "medium"}
	proposal := reviewedSourceProposal{Schema: 2, ReviewId: reviewId, JobId: server.NewId(), SubmittedAt: deadline.Add(-24 * time.Hour), ReviewDecisionDigest: strings.Repeat("2", 64), ReviewedAt: deadline.Add(-2 * time.Hour), ReadyAt: deadline.Add(-time.Hour),
		BaseSourceLockSha256: current.SourceLockSha256, SourceLockSha256: lockDigest, Commits: commits, PatchSha256: patchDigest, RawScore: 80, Significance: validSourceSignificance(), BuildReceipt: receipt}
	return current, proposal, deadline
}

// Welch means quantify significance; they never replace the exact raw median
// used to choose among eligible implementations.
func TestReviewedSourceChoiceUsesRawMedianNotWelchMean(t *testing.T) {
	current, first, deadline := reviewedProposalFixture(t)
	second := first
	second.ReviewId = server.NewId()
	second.BuildReceipt.ReviewId = second.ReviewId
	firstSignificance, secondSignificance := *first.Significance, *second.Significance
	first.Significance, second.Significance = &firstSignificance, &secondSignificance
	first.RawScore, second.RawScore = 80, 81
	first.Significance.CandidateMeanRawScore, second.Significance.CandidateMeanRawScore = 85, 79
	selected := selectReviewedSourceProposal([]reviewedSourceProposal{second, first}, current, deadline)
	if selected == nil || selected.ReviewId != first.ReviewId {
		t.Fatal("Welch mean incorrectly decided source ranking")
	}
}

// Review arrival order cannot break an exact score tie differently from the
// original sealed admission-time/job-id ordering.
func TestReviewedSourceChoiceRetainsOriginalAdmissionTieBreak(t *testing.T) {
	current, first, deadline := reviewedProposalFixture(t)
	second := first
	second.JobId, second.ReviewId = server.NewId(), server.NewId()
	second.BuildReceipt.ReviewId = second.ReviewId
	second.SubmittedAt = first.SubmittedAt.Add(-time.Second)
	if chosen := selectReviewedSourceProposal([]reviewedSourceProposal{first, second}, current, deadline); chosen == nil || chosen.JobId != second.JobId {
		t.Fatal("review identity replaced admission-time tie break")
	}
	second.SubmittedAt = first.SubmittedAt
	want := first.JobId
	if second.JobId.String() < want.String() {
		want = second.JobId
	}
	if chosen := selectReviewedSourceProposal([]reviewedSourceProposal{second, first}, current, deadline); chosen == nil || chosen.JobId != want {
		t.Fatal("review identity replaced exact job-id tie break")
	}
}

// Ready proposals are selected; late review/build or obsolete-base approvals
// cannot displace the frozen default reviewed-source carry-forward.
func TestReviewedSourceChoiceIncludesOnlyReadyApplicablePromotion(t *testing.T) {
	current, proposal, deadline := reviewedProposalFixture(t)
	if selected := selectReviewedSourceProposal([]reviewedSourceProposal{proposal}, current, deadline); selected == nil || selected.ReviewId != proposal.ReviewId {
		t.Fatal("ready reviewed source was not selected")
	}
	late := proposal
	late.ReadyAt = deadline.Add(time.Nanosecond)
	if selectReviewedSourceProposal([]reviewedSourceProposal{late}, current, deadline) != nil {
		t.Fatal("late build changed frozen source choice")
	}
	late = proposal
	late.ReviewedAt = deadline.Add(time.Nanosecond)
	if selectReviewedSourceProposal([]reviewedSourceProposal{late}, current, deadline) != nil {
		t.Fatal("late approval changed frozen source choice")
	}
	obsolete := proposal
	obsolete.BaseSourceLockSha256 = strings.Repeat("0", 64)
	if selectReviewedSourceProposal([]reviewedSourceProposal{obsolete}, current, deadline) != nil {
		t.Fatal("obsolete evaluated base selected")
	}
}

// Passing tests alone cannot authorize a host or credential-bearing build.
func TestReviewedSourceBuildRequiresIndependentContainedEvidence(t *testing.T) {
	_, proposal, _ := reviewedProposalFixture(t)
	mutations := []func(*reviewedSourceBuildReceipt){
		func(receipt *reviewedSourceBuildReceipt) { receipt.Offline = false },
		func(receipt *reviewedSourceBuildReceipt) { receipt.NoMainCredentials = false },
		func(receipt *reviewedSourceBuildReceipt) { receipt.ReadOnlySource = false },
		func(receipt *reviewedSourceBuildReceipt) { receipt.ContainmentSha256 = "" },
		func(receipt *reviewedSourceBuildReceipt) { receipt.TestEvidenceSha256 = "" },
		func(receipt *reviewedSourceBuildReceipt) { receipt.TestModel = "gpt-6-astra" },
		func(receipt *reviewedSourceBuildReceipt) { receipt.SourceLockSha256 = strings.Repeat("0", 64) },
	}
	for index, mutate := range mutations {
		receipt := proposal.BuildReceipt
		mutate(&receipt)
		if err := validateReviewedSourceBuild(receipt, proposal.ReviewId, proposal.PatchSha256, proposal.SourceLockSha256); err == nil {
			t.Fatalf("unsafe build receipt %d accepted", index)
		}
	}
}

// Even the trusted isolation broker receives no inherited main credentials.
func TestReviewedSourceBuildBrokerEnvironmentIsExplicit(t *testing.T) {
	t.Setenv("SYNTHETIC_MAIN_CREDENTIAL", "must-not-reach-build-broker")
	root := t.TempDir()
	commandPath := filepath.Join(root, "isolated-broker")
	program := []byte("#!/bin/sh\nif [ -n \"${SYNTHETIC_MAIN_CREDENTIAL:-}\" ]; then exit 91; fi\nprintf '%s' 'synthetic-broker-ready'\n")
	if err := os.WriteFile(commandPath, program, 0o500); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(program)
	output, err := runSourceSupervisorProgram(context.Background(), commandPath, hex.EncodeToString(digest[:]), struct{}{}, []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"})
	if err != nil || string(output) != "synthetic-broker-ready" {
		t.Fatalf("broker inherited main environment or failed: %s %v", output, err)
	}
}

// The repository-owned worker is selected through a fixed argument, not an
// operator shell template that might omit or rewrite the fenced round request.
func TestSourceWorkerLauncherUsesConcreteSupervisorProtocol(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "synthetic-worker")
	program := []byte("#!/bin/sh\n[ \"$#\" = 1 ] && [ \"$1\" = --supervisor-request ] || exit 91\nprintf '%s' 'bound-worker-launch'\n")
	if err := os.WriteFile(path, program, 0o500); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(program)
	output, err := runSourceSupervisorProgramArguments(context.Background(), path, hex.EncodeToString(digest[:]), struct{}{}, nil, []string{"--supervisor-request"})
	if err != nil || string(output) != "bound-worker-launch" {
		t.Fatalf("concrete worker protocol not selected: %s %v", output, err)
	}
}
