// Synthetic exceptions exercise immutable control reuse and actual execution
// identity independently. No runtime, host, credential or production id is used.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Complete historical metadata makes the ordinary baseline validator part of
// every scope test, not a mocked successful exception verdict.
func securityToolchainFixture(t *testing.T) *competitionNoopScreenFixture {
	t.Helper()
	f := newCompetitionNoopScreenFixture(t)
	f.settings.EvaluationPolicy.ScorerSha256 = f.settings.EvaluationPolicy.SimulatorSha256
	f.settings.automationRoundId, f.settings.distributedRoundEpoch = f.job.RoundId, 2
	f.job.Round.PolicyJson, _ = policySnapshot(f.settings)
	b := f.job.RoundBaseline
	b.BaseSha, b.ProvidersSha256, b.EvaluatorImageDigest = f.settings.BaseSha, f.job.Round.ProvidersSha256, f.settings.EvaluatorImageDigest
	b.ScorerVersion, b.HardwareId, b.HostQualificationSha256 = f.settings.scorerVersion(), f.settings.EvaluationPolicy.HardwareId, f.settings.EvaluationPolicy.HostQualificationSha256
	b.SourceJobId, b.SourceAttempt, b.SourceArtifactManifestSha256 = server.NewId(), 1, strings.Repeat("6", 64)
	b.CreatedAt = time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC)
	value := &competitionSecurityToolchainException{Schema: 1, Kind: "epoch2-security-toolchain-exception/v1", RoundId: f.job.RoundId, Epoch: 2,
		BaselineSha256: b.Sha256, SourceLockSha256: f.settings.AutomationPolicy.SourceLockSha256,
		PreviousEvaluatorImageDigest: f.settings.EvaluatorImageDigest, EvaluatorImageDigest: "sha256:" + strings.Repeat("7", 64),
		PreviousInstrumentSha256: f.settings.EvaluationPolicy.SimulatorSha256, InstrumentSha256: strings.Repeat("8", 64),
		PreviousToolchain: "go1.27.3", Toolchain: "go1.27.4", EvidenceSha256: strings.Repeat("9", 64)}
	for _, host := range f.settings.DistributedEvaluationPolicy.Hosts {
		value.Hosts = append(value.Hosts, competitionSecurityToolchainHost{HostId: host.HostId, PreviousQualificationSha256: host.QualificationSha256, QualificationSha256: strings.Repeat("0", 64)})
		f.settings.distributedCoordinator.Config.Hosts = append(f.settings.distributedCoordinator.Config.Hosts, competitionExecutorConnection{
			HostId: host.HostId, EvaluatorCommandSha256: strings.Repeat("a", 64), AgentConfigSnapshotPath: "/protected/synthetic-role.json"})
	}
	f.settings.securityToolchainException = value
	if err := validateCompetitionSecurityToolchainScope(f.settings, &f.job.Round, b); err != nil {
		t.Fatal("invalid synthetic exception fixture", err)
	}
	return f
}

// Execution gets the new image/fingerprint while the original baseline,
// image, policy, evidence and already fixed submission clock remain unchanged.
func TestCompetitionSecurityToolchainKeepsHistoricalControlAndChangesExecutionOnly(t *testing.T) {
	f := securityToolchainFixture(t)
	before, _ := json.Marshal(f.job.RoundBaseline)
	policyBefore := append([]byte(nil), f.job.Round.PolicyJson...)
	value := f.settings.securityToolchainException
	manifest, err := competitionAgentManifestForJob(f.settings, f.job, f.host)
	if err != nil || manifest.EvaluatorImageDigest != value.EvaluatorImageDigest || manifest.QualificationSha256 != value.Hosts[1].QualificationSha256 ||
		manifest.RoundBaselineSha256 != value.BaselineSha256 || !manifest.Identity.StartedAt.Equal(*f.job.StartedAt) || !manifest.Identity.DeadlineAt.Equal(*f.job.ExecutionDeadline) {
		t.Fatal("hotfix did not preserve control and clocks while selecting the actual image", err)
	}
	if f.settings.EvaluatorImageDigest != value.PreviousEvaluatorImageDigest || f.job.EvaluatorImageDigest != value.PreviousEvaluatorImageDigest {
		t.Fatal("historical frozen image was relabeled")
	}
	check := passingHostCheck(f.settings)
	check.HostId = f.host.HostId
	check.ImageDigest, check.QualificationSha256 = value.EvaluatorImageDigest, value.Hosts[1].QualificationSha256
	if !check.Eligible(f.settings) {
		t.Fatal("actual replacement containment identity was rejected")
	}
	check.ImageDigest, check.QualificationSha256 = value.PreviousEvaluatorImageDigest, f.host.QualificationSha256
	if check.Eligible(f.settings) {
		t.Fatal("old containment identity was relabeled as the new execution release")
	}
	producer := *f.settings
	producer.executorHostId, producer.executorSlotId = "", 0
	if competitionExecutionImageDigest(&producer) != value.PreviousEvaluatorImageDigest || competitionExecutorQualificationSha256(&producer) != producer.EvaluationPolicy.HostQualificationSha256 {
		t.Fatal("independently pinned baseline producer was changed")
	}
	after, _ := json.Marshal(f.job.RoundBaseline)
	if !bytes.Equal(before, after) || !bytes.Equal(policyBefore, f.job.Round.PolicyJson) || validateRoundBaselineRecord(f.settings, &f.job.Round, f.job.RoundBaseline) != nil {
		t.Fatal("security hotfix changed historical control bytes or validator")
	}
}

// Rejected scope is checked against the real baseline validation boundary.
func TestCompetitionSecurityToolchainRejectsOtherRoundsBaselinesAndHostSets(t *testing.T) {
	for _, change := range []func(*competitionSecurityToolchainException){
		func(v *competitionSecurityToolchainException) { v.Schema++ },
		func(v *competitionSecurityToolchainException) { v.Kind = "arbitrary-upgrade" },
		func(v *competitionSecurityToolchainException) { v.RoundId = server.NewId() },
		func(v *competitionSecurityToolchainException) { v.Epoch = 3 },
		func(v *competitionSecurityToolchainException) { v.Toolchain = "go1.27.5" },
		func(v *competitionSecurityToolchainException) { v.PreviousToolchain = "go1.26.6" },
		func(v *competitionSecurityToolchainException) { v.BaselineSha256 = strings.Repeat("a", 64) },
		func(v *competitionSecurityToolchainException) { v.SourceLockSha256 = strings.Repeat("f", 64) },
		func(v *competitionSecurityToolchainException) {
			v.PreviousEvaluatorImageDigest = v.EvaluatorImageDigest
		},
		func(v *competitionSecurityToolchainException) {
			v.EvaluatorImageDigest = v.PreviousEvaluatorImageDigest
		},
		func(v *competitionSecurityToolchainException) { v.PreviousInstrumentSha256 = v.InstrumentSha256 },
		func(v *competitionSecurityToolchainException) { v.InstrumentSha256 = v.PreviousInstrumentSha256 },
		func(v *competitionSecurityToolchainException) { v.Hosts = v.Hosts[:1] },
		func(v *competitionSecurityToolchainException) { v.Hosts[0].HostId = "other.example" },
		func(v *competitionSecurityToolchainException) { v.Hosts[1].HostId = v.Hosts[0].HostId },
		func(v *competitionSecurityToolchainException) {
			v.Hosts[0].PreviousQualificationSha256 = strings.Repeat("e", 64)
		},
		func(v *competitionSecurityToolchainException) { v.Hosts[0], v.Hosts[1] = v.Hosts[1], v.Hosts[0] },
		func(v *competitionSecurityToolchainException) { v.EvidenceSha256 = "" },
	} {
		f := securityToolchainFixture(t)
		change(f.settings.securityToolchainException)
		if validateCompetitionSecurityToolchainScope(f.settings, &f.job.Round, f.job.RoundBaseline) == nil {
			t.Fatal("changed scope or compiler transition was accepted")
		}
	}
}

// Missing/corrupt controls cannot trigger a new baseline through the exception.
func TestCompetitionSecurityToolchainRejectsMissingOrChangedHistoricalControl(t *testing.T) {
	f := securityToolchainFixture(t)
	if validateCompetitionSecurityToolchainScope(f.settings, &f.job.Round, nil) == nil {
		t.Fatal("missing control was accepted")
	}
	f.job.RoundBaseline.Json = append(f.job.RoundBaseline.Json, '\n')
	if validateCompetitionSecurityToolchainScope(f.settings, &f.job.Round, f.job.RoundBaseline) == nil {
		t.Fatal("changed historical bytes were accepted")
	}
}

// Extra JSON policy leaves must not create a back door for changing gates.
func TestCompetitionSecurityToolchainRejectsAdditionalPolicyAuthority(t *testing.T) {
	f := securityToolchainFixture(t)
	content, _ := json.Marshal(f.settings.securityToolchainException)
	var object map[string]any
	if err := json.Unmarshal(content, &object); err != nil {
		t.Fatal(err)
	}
	object["takeover_margin"] = 0
	content, _ = json.Marshal(object)
	var value competitionSecurityToolchainException
	if decodeFailureEvidenceJson(content, &value) == nil {
		t.Fatal("security exception gained undeclared gate authority")
	}
}

// Both execution and historical identities are retained together, and neither
// an omitted decision nor a substituted new image may enter an archive.
func TestCompetitionSecurityToolchainArchiveBindsBothImagesAndExactDecision(t *testing.T) {
	f := securityToolchainFixture(t)
	value := f.settings.securityToolchainException
	manifest := artifactManifest{EvaluatorImageDigest: value.PreviousEvaluatorImageDigest, ExecutionEvaluatorImageDigest: value.EvaluatorImageDigest, SecurityToolchainException: value}
	if err := validateCompetitionSecurityToolchainArchive(f.settings, f.job, manifest); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*artifactManifest){
		func(m *artifactManifest) { m.SecurityToolchainException = nil },
		func(m *artifactManifest) { m.ExecutionEvaluatorImageDigest = "" },
		func(m *artifactManifest) { m.ExecutionEvaluatorImageDigest = value.PreviousEvaluatorImageDigest },
		func(m *artifactManifest) {
			copy := *value
			copy.EvidenceSha256 = strings.Repeat("e", 64)
			m.SecurityToolchainException = &copy
		},
	} {
		changed := manifest
		change(&changed)
		if validateCompetitionSecurityToolchainArchive(f.settings, f.job, changed) == nil {
			t.Fatal("archive accepted missing or substituted exception identity")
		}
	}
	f.settings.securityToolchainException = nil
	if validateCompetitionSecurityToolchainArchive(f.settings, f.job, manifest) == nil {
		t.Fatal("ordinary execution accepted unapproved exception metadata")
	}
	if err := validateCompetitionSecurityToolchainArchive(f.settings, f.job, artifactManifest{}); err != nil {
		t.Fatal("historical archive shape no longer accepted", err)
	}
}

// A scope mismatch is rejected before transport, seed access or any execution
// directory. The nil transport would fail loudly if that boundary regressed.
func TestCompetitionSecurityToolchainEvaluateRejectsScopeBeforeDispatch(t *testing.T) {
	f := securityToolchainFixture(t)
	f.job.ExecutorHostId, f.job.SlotFencingToken = f.host.HostId, 1
	f.settings.securityToolchainException.BaselineSha256 = strings.Repeat("e", 64)
	evaluator := &competitionAgentEvaluator{host: f.host}
	outcome := evaluator.Evaluate(context.Background(), f.settings, f.job)
	if outcome.Error == nil || outcome.Error.Code != "round_policy_mismatch" {
		t.Fatal("invalid hotfix scope reached dispatch", outcome.Error)
	}
}
