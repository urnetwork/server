// Operational identity replacement cannot rewrite the frozen competition.
package controller

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// Synthetic identities bind the same source, fixture and ordered executor set.
func executorControlAmendmentFixture(t *testing.T) (*Settings, *roundRecord, CompetitionExecutorControlAmendment) {
	t.Helper()
	settings := validSettings()
	settings.DistributedEvaluationPolicy = competitionDistributedSlotTestPolicy(t)
	settings.AutomationPolicy.Environment = "synthetic-environment"
	settings.AutomationPolicy.SeasonId = "synthetic-season"
	settings.AutomationPolicy.SourceLockSha256 = strings.Repeat("a", 64)
	round := &roundRecord{RoundResult: RoundResult{RoundId: server.NewId(), Epoch: 2}}
	settings.automationRoundId = round.RoundId
	value := CompetitionExecutorControlAmendment{Schema: 1, Kind: "executor-control-correctness/v1", RoundId: round.RoundId,
		Environment: settings.AutomationPolicy.Environment, CompetitionId: settings.CompetitionId, SeasonId: settings.AutomationPolicy.SeasonId,
		PolicyApprovalSha256: settings.DistributedEvaluationPolicy.ApprovalSha256, SourceLockSha256: settings.AutomationPolicy.SourceLockSha256,
		EvaluatorImageDigest: settings.EvaluatorImageDigest, ConfigLocalSha256: settings.EvaluationPolicy.ConfigLocalSha256, VaultLocalSha256: settings.EvaluationPolicy.VaultLocalSha256,
		EvidenceSha256: strings.Repeat("b", 64)}
	for _, host := range settings.DistributedEvaluationPolicy.Hosts {
		value.Hosts = append(value.Hosts, CompetitionExecutorControlHostAmendment{HostId: host.HostId, PreviousQualificationSha256: host.QualificationSha256,
			QualificationSha256: strings.Repeat("c", 64), PreviousAgentSha256: strings.Repeat("d", 64), AgentSha256: strings.Repeat("e", 64),
			PreviousBuilderSha256: strings.Repeat("f", 64), BuilderSha256: strings.Repeat("0", 64)})
	}
	return settings, round, value
}

// Both old and actual new identity are authenticated without editing host keys,
// slot/transport selection, equivalence margins or original public policy bytes.
func TestExecutorControlAmendmentResolvesOnlyRuntimeIdentity(t *testing.T) {
	settings, round, value := executorControlAmendmentFixture(t)
	if err := validateCompetitionExecutorControlAmendment(settings, round, value); err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(settings.DistributedEvaluationPolicy)
	if err != nil {
		t.Fatal(err)
	}
	_, digest, err := competitionExecutorControlAmendmentBytes(value)
	if err != nil {
		t.Fatal(err)
	}
	settings.executorControlAmendment = &competitionExecutorControlAmendmentRecord{Amendment: value, Sha256: digest}
	for index, original := range settings.DistributedEvaluationPolicy.Hosts {
		resolved := competitionExecutorControlHost(settings, original)
		if resolved.QualificationSha256 != value.Hosts[index].QualificationSha256 {
			t.Fatal("corrected runtime identity was not selected")
		}
		resolved.QualificationSha256 = original.QualificationSha256
		if resolved != original {
			t.Fatal("control amendment altered frozen host authority")
		}
		settings.executorHostId, settings.executorSlotId = original.HostId, original.SlotId
		if competitionExecutorQualificationSha256(settings) != value.Hosts[index].QualificationSha256 {
			t.Fatal("composed worker request still expects the defective identity")
		}
	}
	after, err := json.Marshal(settings.DistributedEvaluationPolicy)
	if err != nil || !bytes.Equal(before, after) || validateCompetitionDistributedPolicy(settings.DistributedEvaluationPolicy) != nil || competitionExecutorControlAmendmentSha256(settings) != digest {
		t.Fatal("runtime resolution mutated the immutable approved policy", err)
	}
}

// A replacement agent cannot inherit another round's operational authority.
func TestExecutorControlAmendmentDoesNotLeakToAnotherRound(t *testing.T) {
	settings, _, value := executorControlAmendmentFixture(t)
	settings.executorControlAmendment = &competitionExecutorControlAmendmentRecord{Amendment: value, Sha256: strings.Repeat("a", 64)}
	settings.automationRoundId = server.NewId()
	original := settings.DistributedEvaluationPolicy.Hosts[0]
	if competitionExecutorControlHost(settings, original) != original || competitionExecutorControlAmendmentSha256(settings) != "" {
		t.Fatal("another round inherited operational replacement authority")
	}
}

// No source, scope, fixture, old/new fingerprint or membership mismatch may be
// interpreted as an operational correction to an otherwise unchanged round.
func TestExecutorControlAmendmentRejectsScopeAndIdentityChanges(t *testing.T) {
	for _, mutate := range []func(*CompetitionExecutorControlAmendment){
		func(value *CompetitionExecutorControlAmendment) { value.Schema++ },
		func(value *CompetitionExecutorControlAmendment) { value.Kind = "arbitrary-policy-change" },
		func(value *CompetitionExecutorControlAmendment) { value.RoundId = server.NewId() },
		func(value *CompetitionExecutorControlAmendment) { value.Environment += "-other" },
		func(value *CompetitionExecutorControlAmendment) { value.CompetitionId += "-other" },
		func(value *CompetitionExecutorControlAmendment) { value.SeasonId += "-other" },
		func(value *CompetitionExecutorControlAmendment) { value.PolicyApprovalSha256 = strings.Repeat("f", 64) },
		func(value *CompetitionExecutorControlAmendment) { value.SourceLockSha256 = strings.Repeat("f", 64) },
		func(value *CompetitionExecutorControlAmendment) {
			value.EvaluatorImageDigest = "sha256:" + strings.Repeat("f", 64)
		},
		func(value *CompetitionExecutorControlAmendment) { value.ConfigLocalSha256 = strings.Repeat("f", 64) },
		func(value *CompetitionExecutorControlAmendment) { value.VaultLocalSha256 = strings.Repeat("f", 64) },
		func(value *CompetitionExecutorControlAmendment) { value.EvidenceSha256 = "" },
		func(value *CompetitionExecutorControlAmendment) { value.Hosts = value.Hosts[:1] },
		func(value *CompetitionExecutorControlAmendment) { value.Hosts[0].HostId = "different-executor" },
		func(value *CompetitionExecutorControlAmendment) {
			value.Hosts[0], value.Hosts[1] = value.Hosts[1], value.Hosts[0]
		},
		func(value *CompetitionExecutorControlAmendment) {
			value.Hosts[0].PreviousQualificationSha256 = strings.Repeat("f", 64)
		},
		func(value *CompetitionExecutorControlAmendment) {
			value.Hosts[0].QualificationSha256 = value.Hosts[0].PreviousQualificationSha256
		},
		func(value *CompetitionExecutorControlAmendment) {
			value.Hosts[0].AgentSha256 = value.Hosts[0].PreviousAgentSha256
		},
		func(value *CompetitionExecutorControlAmendment) { value.Hosts[0].BuilderSha256 = "" },
	} {
		settings, round, value := executorControlAmendmentFixture(t)
		mutate(&value)
		if validateCompetitionExecutorControlAmendment(settings, round, value) == nil {
			t.Fatal("unrelated or unbound replacement accepted", value)
		}
	}
}

// Legacy round-one and staging authority remain wholly outside this mechanism.
func TestExecutorControlAmendmentRejectsLegacyAndStaging(t *testing.T) {
	settings, round, value := executorControlAmendmentFixture(t)
	round.Epoch = 1
	if validateCompetitionExecutorControlAmendment(settings, round, value) == nil {
		t.Fatal("epoch one acquired future executor authority")
	}
	round.Epoch, round.Staging = 2, true
	if validateCompetitionExecutorControlAmendment(settings, round, value) == nil {
		t.Fatal("staging acquired production amendment authority")
	}
}

// Previously retained no-op evidence cannot be reused under amended controls,
// even though its original epoch policy approval remains the same.
func TestExecutorControlAmendmentRejectsUnamendedNoopReceipt(t *testing.T) {
	fixture := newCompetitionNoopScreenFixture(t)
	receipt, err := screenCompetitionExecutorNoop(fixture.settings, fixture.job, fixture.host, fixture.directory, fixture.response, fixture.outcome)
	if err != nil {
		t.Fatal(err)
	}
	sealCompetitionNoopScreenReceipt(t, fixture.settings, receipt)
	if err := validateCompetitionExecutorReceipt(fixture.settings, &fixture.job.Round, fixture.job.RoundBaseline, fixture.host, receipt); err != nil {
		t.Fatal("legacy receipt counterfactual invalid", err)
	}
	fixture.settings.automationRoundId = fixture.job.RoundId
	fixture.settings.executorControlAmendment = &competitionExecutorControlAmendmentRecord{Amendment: CompetitionExecutorControlAmendment{RoundId: fixture.job.RoundId}, Sha256: strings.Repeat("a", 64)}
	if validateCompetitionExecutorReceipt(fixture.settings, &fixture.job.Round, fixture.job.RoundBaseline, fixture.host, receipt) == nil {
		t.Fatal("old no-op admission was relabeled as amended runtime evidence")
	}
}
