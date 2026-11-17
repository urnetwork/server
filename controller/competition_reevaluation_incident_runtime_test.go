// A genuine older generic recovery qualification is not evidence that its API
// and worker understand the newly typed backing-service or heartbeat receipts.
package controller

import (
	"strings"
	"testing"

	"github.com/urnetwork/server/model"
)

// New classes need explicit separately qualified support. Old archive grants
// retain their existing bytes and remain usable without the optional extension.
func TestCompetitionReevaluationIncidentRuntimeRequiresExplicitCapability(t *testing.T) {
	settings, _, request := newCompetitionLegacyArchiveRequestFixture(t)
	a := request.Authorization
	proof := model.CompetitionJobReevaluationRuntime{Schema: 1, Kind: model.CompetitionJobReevaluationRuntimeKind, Accepted: true,
		Capability: model.CompetitionJobReevaluationCapability, SourceCommit: a.FixedSourceCommit, ApiImageDigest: a.ApiImageDigest,
		WorkerImageDigest: a.FixedWorkerImageDigest, WorkerSha256: a.FixedWorkerSha256, EvidenceSha256: strings.Repeat("a", 64), ApiCompatible: true, WorkerCompatible: true}
	digest, err := model.CompetitionAutomationDigest(proof)
	if err != nil {
		t.Fatal(err)
	}
	if err := BindCompetitionJobReevaluationRuntime(settings, proof, digest); err != nil {
		t.Fatal(err)
	}
	a.RuntimeQualificationSha256 = digest
	if err := validateCompetitionReevaluationRuntime(settings, a); err != nil {
		t.Fatalf("older archive qualification stranded: %v", err)
	}
	for _, class := range []string{model.CompetitionJobReevaluationHeartbeatBudget, model.CompetitionJobReevaluationBackingService, model.CompetitionJobReevaluationOperatorInterruption} {
		a.IncidentClass = class
		if validateCompetitionReevaluationRuntime(settings, a) == nil {
			t.Fatalf("generic old qualification granted %s", class)
		}
		copyProof := proof
		copyProof.IncidentClasses = []string{class}
		newDigest, err := model.CompetitionAutomationDigest(copyProof)
		if err != nil {
			t.Fatal(err)
		}
		if err := BindCompetitionJobReevaluationRuntime(settings, copyProof, newDigest); err != nil {
			t.Fatal(err)
		}
		a.RuntimeQualificationSha256 = newDigest
		copyProof.IncidentClasses[0] = "synthetic-caller-mutation"
		if err := validateCompetitionReevaluationRuntime(settings, a); err != nil {
			t.Fatalf("bound capability followed caller storage: %v", err)
		}
		if err := BindCompetitionJobReevaluationRuntime(settings, proof, digest); err != nil {
			t.Fatal(err)
		}
		a.RuntimeQualificationSha256 = digest
	}
}

// An unknown or repeated capability cannot acquire authority merely by
// carrying a self-consistent outer digest and accepted=true.
func TestCompetitionReevaluationIncidentRuntimeRejectsUnknownOrDuplicateCapabilities(t *testing.T) {
	settings, _, request := newCompetitionLegacyArchiveRequestFixture(t)
	a := request.Authorization
	for _, classes := range [][]string{{"synthetic_unknown"}, {""},
		{model.CompetitionJobReevaluationBackingService, model.CompetitionJobReevaluationBackingService}} {
		proof := model.CompetitionJobReevaluationRuntime{Schema: 1, Kind: model.CompetitionJobReevaluationRuntimeKind, Accepted: true,
			Capability: model.CompetitionJobReevaluationCapability, SourceCommit: a.FixedSourceCommit, ApiImageDigest: a.ApiImageDigest,
			WorkerImageDigest: a.FixedWorkerImageDigest, WorkerSha256: a.FixedWorkerSha256, EvidenceSha256: strings.Repeat("a", 64),
			ApiCompatible: true, WorkerCompatible: true, IncidentClasses: classes}
		digest, err := model.CompetitionAutomationDigest(proof)
		if err != nil {
			t.Fatal(err)
		}
		if BindCompetitionJobReevaluationRuntime(settings, proof, digest) == nil || settings.reevaluationRuntime != nil {
			t.Fatalf("unsupported capability set granted authority: %+v", classes)
		}
	}
}
