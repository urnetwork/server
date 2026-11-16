// Explicit legacy recovery approval is distinct from a proved inventory defect.
// Only the existing scoped, snapshot-bound operator request can carry it.
package controller

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// This tests request structure only; actual terminal and immutable incident
// authority are checked through the public store in the PostgreSQL regressions.
func newCompetitionLegacyArchiveRequestFixture(t testing.TB) (*Settings, *Principal, model.CompetitionJobReevaluationRequest) {
	t.Helper()
	settings := automationApiSettings()
	principal := &Principal{Id: "synthetic-legacy-operator", Role: "operator", Environment: settings.AutomationPolicy.Environment,
		CompetitionId: settings.CompetitionId, SeasonId: settings.AutomationPolicy.SeasonId}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a := model.CompetitionJobReevaluationAuthorization{
		Schema: 1, Kind: model.CompetitionJobReevaluationAuthorizationKind, IncidentClass: model.CompetitionJobReevaluationLegacyArchiveBudget,
		Environment: principal.Environment, CompetitionId: principal.CompetitionId, SeasonId: principal.SeasonId,
		JobId: server.NewId(), RoundId: server.NewId(), ExpectedAttemptCount: 2, ExpectedCycle: 0,
		ExpectedTerminalSnapshot: json.RawMessage(`{"state":"failed","score_json":null}`), EvidenceSha256: strings.Repeat("1", 64),
		IncidentEventId: 1, IncidentEventSha256: strings.Repeat("2", 64), IncidentAttempt: 1,
		RoundPolicySha256: strings.Repeat("3", 64), BaselineSha256: strings.Repeat("4", 64), RuntimeQualificationSha256: strings.Repeat("5", 64),
		FixedSourceCommit: strings.Repeat("6", 40), FixedWorkerSha256: strings.Repeat("7", 64),
		FixedWorkerImageDigest: "sha256:" + strings.Repeat("8", 64), ApiImageDigest: "sha256:" + strings.Repeat("9", 64),
		AuthorizedAt: at, ExpiresAt: at.Add(time.Hour), SingleUse: true,
	}
	var err error
	a.ExpectedTerminalSha256, err = CompetitionJobReevaluationSnapshotDigest(a.ExpectedTerminalSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	request := model.CompetitionJobReevaluationRequest{IdempotencyKey: "synthetic-legacy-approval", Authorization: a}
	request.AuthorizationSha256, err = model.CompetitionAutomationDigest(a)
	if err != nil {
		t.Fatal(err)
	}
	return settings, principal, request
}

// The new class cannot invent inventory evidence or treat its terminal attempt
// as the required earlier interruption, even with a self-consistent digest.
func TestCompetitionReevaluationLegacyRequestRequiresDistinctBoundEvidence(t *testing.T) {
	settings, principal, request := newCompetitionLegacyArchiveRequestFixture(t)
	if err := validateCompetitionReevaluationRequest(settings, principal, request); err != nil {
		t.Fatalf("explicit synthetic legacy approval was refused: %v", err)
	}
	for _, change := range []struct {
		name   string
		mutate func(*model.CompetitionJobReevaluationAuthorization)
	}{
		{name: "unknown-class", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.IncidentClass = "synthetic_unknown_class" }},
		{name: "invented-inventory-hash", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
			a.FailureManifestSha256 = strings.Repeat("a", 64)
		}},
		{name: "invented-inventory-path", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
			a.RejectedArtifactPath = "failed-evidence/evaluation-sources.json"
		}},
		{name: "missing-prior-attempt", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.IncidentAttempt = 0 }},
		{name: "terminal-attempt", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.IncidentAttempt = a.ExpectedAttemptCount }},
		{name: "future-attempt", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.IncidentAttempt = a.ExpectedAttemptCount + 1 }},
		{name: "missing-operator-dossier", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.EvidenceSha256 = "" }},
		{name: "missing-event", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.IncidentEventId = 0 }},
		{name: "missing-event-digest", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.IncidentEventSha256 = "" }},
		{name: "not-single-use", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.SingleUse = false }},
	} {
		changed := request
		change.mutate(&changed.Authorization)
		var err error
		changed.AuthorizationSha256, err = model.CompetitionAutomationDigest(changed.Authorization)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateCompetitionReevaluationRequest(settings, principal, changed); err == nil {
			t.Fatalf("self-consistent %s granted legacy request authority", change.name)
		}
	}
}

// A new incident class grants no new principal role and cannot detach the
// operator's evidence digest from the pinned full authorization document.
func TestCompetitionReevaluationLegacyRequestPreservesScopeAndApprovalDigest(t *testing.T) {
	settings, principal, request := newCompetitionLegacyArchiveRequestFixture(t)
	for _, change := range []struct {
		name   string
		mutate func(*Principal)
	}{
		{name: "submitter", mutate: func(p *Principal) { p.Role = "submitter" }},
		{name: "reviewer", mutate: func(p *Principal) { p.Role = competitionReviewerRole }},
		{name: "unscoped", mutate: func(p *Principal) { p.Environment = "" }},
		{name: "other-season", mutate: func(p *Principal) { p.SeasonId = "synthetic-other-season" }},
	} {
		changed := *principal
		change.mutate(&changed)
		if err := validateCompetitionReevaluationRequest(settings, &changed, request); err == nil {
			t.Fatalf("%s acquired legacy operator approval authority", change.name)
		}
	}
	changed := request
	changed.Authorization.EvidenceSha256 = strings.Repeat("e", 64)
	if err := validateCompetitionReevaluationRequest(settings, principal, changed); err == nil {
		t.Fatal("another dossier reused the original authorization digest")
	}
}
