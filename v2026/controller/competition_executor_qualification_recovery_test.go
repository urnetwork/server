// Entirely synthetic authority and scoring fixtures; no production captures.
package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A single recovery stays bound to the genuine synthetic full-R9 fixture.
func executorQualificationRecoveryTestValue(fixture *competitionNoopScreenFixture) CompetitionExecutorQualificationRecovery {
	s := fixture.settings
	return CompetitionExecutorQualificationRecovery{Schema: 1, Kind: "executor-qualification-single-recovery/v1", RoundId: fixture.job.RoundId, HostId: fixture.host.HostId,
		OriginalJobId: fixture.job.JobId, ReplacementJobId: server.NewId(), BaselineSha256: fixture.job.RoundBaseline.Sha256, QualificationSha256: fixture.host.QualificationSha256,
		ControlAmendmentSha256: competitionExecutorControlAmendmentSha256(s), PolicyApprovalSha256: s.DistributedEvaluationPolicy.ApprovalSha256,
		SourceLockSha256: s.AutomationPolicy.SourceLockSha256, EvaluatorImageDigest: s.EvaluatorImageDigest, ProvidersSha256: fixture.job.Round.ProvidersSha256,
		ConfigLocalSha256: s.EvaluationPolicy.ConfigLocalSha256, VaultLocalSha256: s.EvaluationPolicy.VaultLocalSha256,
		OriginalScoreSha256: strings.Repeat("d", 64), EvidenceSha256: strings.Repeat("e", 64)}
}

func TestExecutorQualificationRecoveryCanonicalAuthorityRejectsRehashedScopeChanges(t *testing.T) {
	fixture := newCompetitionNoopScreenFixture(t)
	valid := executorQualificationRecoveryTestValue(fixture)
	for _, change := range []struct {
		name   string
		mutate func(*CompetitionExecutorQualificationRecovery)
	}{
		{name: "valid", mutate: func(*CompetitionExecutorQualificationRecovery) {}},
		{name: "different round", mutate: func(v *CompetitionExecutorQualificationRecovery) { v.RoundId = server.NewId() }},
		{name: "different host", mutate: func(v *CompetitionExecutorQualificationRecovery) { v.HostId = "synthetic-foreign-executor" }},
		{name: "different original", mutate: func(v *CompetitionExecutorQualificationRecovery) { v.OriginalJobId = server.NewId() }},
		{name: "old job relabeled", mutate: func(v *CompetitionExecutorQualificationRecovery) { v.ReplacementJobId = v.OriginalJobId }},
		{name: "missing replacement", mutate: func(v *CompetitionExecutorQualificationRecovery) { v.ReplacementJobId = server.Id{} }},
		{name: "different policy", mutate: func(v *CompetitionExecutorQualificationRecovery) { v.PolicyApprovalSha256 = strings.Repeat("0", 64) }},
		{name: "different source", mutate: func(v *CompetitionExecutorQualificationRecovery) { v.SourceLockSha256 = strings.Repeat("0", 64) }},
		{name: "different image", mutate: func(v *CompetitionExecutorQualificationRecovery) {
			v.EvaluatorImageDigest = "sha256:" + strings.Repeat("0", 64)
		}},
		{name: "different workload", mutate: func(v *CompetitionExecutorQualificationRecovery) { v.ProvidersSha256 = strings.Repeat("0", 64) }},
		{name: "different host proof", mutate: func(v *CompetitionExecutorQualificationRecovery) { v.QualificationSha256 = strings.Repeat("0", 64) }},
		{name: "missing failure", mutate: func(v *CompetitionExecutorQualificationRecovery) { v.OriginalScoreSha256 = "" }},
		{name: "missing approval evidence", mutate: func(v *CompetitionExecutorQualificationRecovery) { v.EvidenceSha256 = "" }},
	} {
		value := valid
		change.mutate(&value)
		canonical, digest, err := competitionExecutorQualificationRecoveryBytes(value)
		if err != nil {
			t.Fatal(err)
		}
		_, err = decodeCompetitionExecutorQualificationRecovery(fixture.settings, &fixture.job.Round, canonical, digest)
		if (change.name == "valid") != (err == nil) {
			t.Fatalf("authority mutation %s: %v", change.name, err)
		}
	}
}

func TestExecutorQualificationRecoveryReceiptRequiresExactAuthorityAndAllGates(t *testing.T) {
	fixture := newCompetitionNoopScreenFixture(t)
	value := executorQualificationRecoveryTestValue(fixture)
	canonical, digest, err := competitionExecutorQualificationRecoveryBytes(value)
	if err != nil {
		t.Fatal(err)
	}
	record, err := decodeCompetitionExecutorQualificationRecovery(fixture.settings, &fixture.job.Round, canonical, digest)
	if err != nil {
		t.Fatal(err)
	}
	selected := *fixture.settings
	selected.executorQualificationRecovery = record
	originalJobId := fixture.job.JobId
	fixture.job.JobId = value.ReplacementJobId
	receipt, err := screenCompetitionExecutorNoop(&selected, fixture.job, fixture.host, fixture.directory, fixture.response, fixture.outcome)
	if err != nil || !receipt.Passed || receipt.RecoverySha256 != digest {
		t.Fatal("replacement changed the R9 screen", err)
	}
	sealCompetitionNoopScreenReceipt(t, &selected, receipt)
	if err := validateCompetitionExecutorReceipt(&selected, &fixture.job.Round, fixture.job.RoundBaseline, fixture.host, receipt); err != nil {
		t.Fatal(err)
	}
	if validateCompetitionExecutorReceipt(fixture.settings, &fixture.job.Round, fixture.job.RoundBaseline, fixture.host, receipt) == nil {
		t.Fatal("receipt accepted without its recovery authority")
	}
	record.Sha256 = strings.Repeat("0", 64)
	receipt.RecoverySha256 = record.Sha256
	sealCompetitionNoopScreenReceipt(t, &selected, receipt)
	if validateCompetitionExecutorReceipt(&selected, &fixture.job.Round, fixture.job.RoundBaseline, fixture.host, receipt) == nil {
		t.Fatal("matching forged labels replaced canonical recovery authority")
	}
	record.Sha256, receipt.RecoverySha256 = digest, digest
	receipt.QualificationJobId = originalJobId.String()
	sealCompetitionNoopScreenReceipt(t, &selected, receipt)
	if validateCompetitionExecutorReceipt(&selected, &fixture.job.Round, fixture.job.RoundBaseline, fixture.host, receipt) == nil {
		t.Fatal("old job relabeled as replacement")
	}
	fixture.outcome.Score.Gates["G5_stability"] = Gate{Passed: false}
	if _, err := screenCompetitionExecutorNoop(&selected, fixture.job, fixture.host, fixture.directory, fixture.response, fixture.outcome); err == nil {
		t.Fatal("replacement waived G5")
	}
}

func TestExecutorQualificationRecoveryRejectsUnknownAuthorityLeaves(t *testing.T) {
	fixture := newCompetitionNoopScreenFixture(t)
	value := executorQualificationRecoveryTestValue(fixture)
	content, _, err := competitionExecutorQualificationRecoveryBytes(value)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(content, &wire); err != nil {
		t.Fatal(err)
	}
	wire["replicates"] = 1
	content, err = json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeCompetitionExecutorQualificationRecovery(fixture.settings, &fixture.job.Round, content, strings.Repeat("a", 64)); err == nil {
		t.Fatal("authority added a configurable sampling bypass")
	}
	if err := (PostgresStore{}).RecordExecutorQualificationRecovery(context.Background(), nil, model.CompetitionSupervisorLease{}, value, ""); err == nil {
		t.Fatal("malformed registration reached external I/O")
	}
}
