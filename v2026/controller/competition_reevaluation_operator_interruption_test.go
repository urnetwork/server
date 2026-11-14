// The historical operator stop is a new typed exception, not a broader
// heartbeat predicate or permission to resample arbitrary terminal failures.
package controller

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/model"
)

// Json construction lets the same positive regression run against the parent
// release, where the missing class must refuse despite an otherwise bound request.
func newCompetitionOperatorInterruptionRequest(t testing.TB) (*Settings, *Principal, model.CompetitionJobReevaluationRequest) {
	t.Helper()
	settings, principal, request := newCompetitionLegacyArchiveRequestFixture(t)
	a := &request.Authorization
	a.IncidentClass = "operator_authorized_interrupted_budget_v1"
	a.IncidentEventId = 3
	a.FailureManifestSha256, a.RejectedArtifactPath = "", ""
	setCompetitionOperatorInterruptionProof(t, &request, map[string]any{
		"schema": 1, "action": "stop_worker", "stopped_at": a.AuthorizedAt.Add(-time.Hour),
		"claim_event_id": int64(1), "claim_event_sha256": strings.Repeat("a", 64), "evidence_sha256": strings.Repeat("b", 64),
	})
	return settings, principal, request
}

// Only the optional proof changes; exact snapshot numbers and outer scope
// stay in the typed request and its canonical authorization digest.
func setCompetitionOperatorInterruptionProof(t testing.TB, request *model.CompetitionJobReevaluationRequest, proof map[string]any) {
	t.Helper()
	content, err := json.Marshal(request.Authorization)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(content, &fields); err != nil {
		t.Fatal(err)
	}
	if proof == nil {
		delete(fields, "operator_interruption_evidence")
	} else {
		fields["operator_interruption_evidence"], err = json.Marshal(proof)
		if err != nil {
			t.Fatal(err)
		}
	}
	content, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var authorization model.CompetitionJobReevaluationAuthorization
	if err := json.Unmarshal(content, &authorization); err != nil {
		t.Fatal(err)
	}
	request.Authorization = authorization
	request.AuthorizationSha256, err = model.CompetitionAutomationDigest(request.Authorization)
	if err != nil {
		t.Fatal(err)
	}
}

// The parent rejects this exact privileged request as an unknown class. The
// fix admits only its structure; ledger and runtime evidence remain separate.
func TestCompetitionReevaluationOperatorInterruptionRequestAcceptsBoundStopEvidence(t *testing.T) {
	settings, principal, request := newCompetitionOperatorInterruptionRequest(t)
	if err := validateCompetitionReevaluationRequest(settings, principal, request); err != nil {
		t.Fatalf("bound operator-stop approval refused: %v", err)
	}
}

// Every semantic mutation receives a fresh outer digest. A matching hash alone
// cannot turn missing, borrowed or temporally ambiguous proof into authority.
func TestCompetitionReevaluationOperatorInterruptionRequestRejectsUnboundProof(t *testing.T) {
	settings, principal, original := newCompetitionOperatorInterruptionRequest(t)
	for _, c := range []struct {
		name   string
		mutate func(*model.CompetitionJobReevaluationRequest, map[string]any)
	}{
		{name: "missing", mutate: func(_ *model.CompetitionJobReevaluationRequest, proof map[string]any) { clear(proof) }},
		{name: "schema", mutate: func(_ *model.CompetitionJobReevaluationRequest, proof map[string]any) { proof["schema"] = 2 }},
		{name: "action", mutate: func(_ *model.CompetitionJobReevaluationRequest, proof map[string]any) {
			proof["action"] = "synthetic_product_failure"
		}},
		{name: "missing-stop", mutate: func(_ *model.CompetitionJobReevaluationRequest, proof map[string]any) {
			proof["stopped_at"] = time.Time{}
		}},
		{name: "future-stop", mutate: func(r *model.CompetitionJobReevaluationRequest, proof map[string]any) {
			proof["stopped_at"] = r.Authorization.AuthorizedAt
		}},
		{name: "submicrosecond-stop", mutate: func(r *model.CompetitionJobReevaluationRequest, proof map[string]any) {
			proof["stopped_at"] = r.Authorization.AuthorizedAt.Add(-time.Hour + time.Nanosecond)
		}},
		{name: "missing-claim", mutate: func(_ *model.CompetitionJobReevaluationRequest, proof map[string]any) { proof["claim_event_id"] = 0 }},
		{name: "terminal-as-claim", mutate: func(r *model.CompetitionJobReevaluationRequest, proof map[string]any) {
			proof["claim_event_id"] = r.Authorization.IncidentEventId
		}},
		{name: "missing-claim-digest", mutate: func(_ *model.CompetitionJobReevaluationRequest, proof map[string]any) {
			proof["claim_event_sha256"] = ""
		}},
		{name: "missing-stop-evidence", mutate: func(_ *model.CompetitionJobReevaluationRequest, proof map[string]any) { proof["evidence_sha256"] = "" }},
		{name: "terminal-as-interruption", mutate: func(r *model.CompetitionJobReevaluationRequest, _ map[string]any) {
			r.Authorization.IncidentAttempt = r.Authorization.ExpectedAttemptCount
		}},
		{name: "hidden-extra-attempt", mutate: func(r *model.CompetitionJobReevaluationRequest, _ map[string]any) {
			r.Authorization.ExpectedAttemptCount++
		}},
		{name: "invented-archive", mutate: func(r *model.CompetitionJobReevaluationRequest, _ map[string]any) {
			r.Authorization.FailureManifestSha256 = strings.Repeat("c", 64)
		}},
		{name: "borrowed-heartbeat-class", mutate: func(r *model.CompetitionJobReevaluationRequest, _ map[string]any) {
			r.Authorization.IncidentClass = model.CompetitionJobReevaluationHeartbeatBudget
		}},
	} {
		_, _, request := newCompetitionOperatorInterruptionRequest(t)
		proof := map[string]any{"schema": 1, "action": "stop_worker", "stopped_at": original.Authorization.AuthorizedAt.Add(-time.Hour),
			"claim_event_id": int64(1), "claim_event_sha256": strings.Repeat("a", 64), "evidence_sha256": strings.Repeat("b", 64)}
		c.mutate(&request, proof)
		setCompetitionOperatorInterruptionProof(t, &request, proof)
		if err := validateCompetitionReevaluationRequest(settings, principal, request); err == nil {
			t.Fatalf("%s acquired operator-stop authority", c.name)
		}
	}
}

// The operator's scope and original failure remain unchanged. No submitter,
// scored outcome or unknown class can acquire this explicit exception.
func TestCompetitionReevaluationOperatorInterruptionRetainsFailureAndPrincipalScope(t *testing.T) {
	settings, principal, request := newCompetitionOperatorInterruptionRequest(t)
	for _, role := range []string{"submitter", competitionReviewerRole, "synthetic_unknown"} {
		changed := *principal
		changed.Role = role
		if validateCompetitionReevaluationRequest(settings, &changed, request) == nil {
			t.Fatalf("%s acquired operator-stop authority", role)
		}
	}
	cycle := newCompetitionReevaluationCycleTestFixture(t)
	cycle.IncidentClass = "operator_authorized_interrupted_budget_v1"
	cycle.PreviousFailure = model.CompetitionEvaluationFailure{Kind: "infrastructure", Code: "evaluation_time_budget_exhausted", Retriable: false}
	for _, c := range []struct {
		failure model.CompetitionEvaluationFailure
		allowed bool
	}{
		{failure: cycle.PreviousFailure, allowed: true},
		{failure: model.CompetitionEvaluationFailure{Kind: "submission", Code: "run_unstable"}},
		{failure: model.CompetitionEvaluationFailure{Kind: "infrastructure", Code: "artifact_archive_failed"}},
		{failure: model.CompetitionEvaluationFailure{Kind: "infrastructure", Code: "evaluation_time_budget_exhausted", Retriable: true}},
	} {
		cycle.PreviousFailure = c.failure
		var err error
		cycle.ReceiptSha256, err = model.CompetitionJobReevaluationCycleDigest(cycle)
		if err != nil {
			t.Fatal(err)
		}
		if actual := model.ValidateCompetitionJobReevaluationCycle(&cycle) == nil; actual != c.allowed {
			t.Fatalf("failure=%+v allowed=%t want=%t", c.failure, actual, c.allowed)
		}
	}
}

// The new optional object keeps the same exact typed-protocol boundary as its
// enclosing authorization; unknown and case-folded fields cannot hide consent.
func TestCompetitionReevaluationOperatorInterruptionProofDecoderIsStrict(t *testing.T) {
	_, _, request := newCompetitionOperatorInterruptionRequest(t)
	content, err := json.Marshal(request.Authorization)
	if err != nil {
		t.Fatal(err)
	}
	for _, malformed := range []string{
		strings.Replace(string(content), `"action":"stop_worker"`, `"Action":"stop_worker"`, 1),
		strings.Replace(string(content), `"action":"stop_worker"`, `"action":"stop_worker","waive":true`, 1),
		strings.Replace(string(content), `"action":"stop_worker"`, `"action":"stop_worker","action":"stop_worker"`, 1),
	} {
		var authorization model.CompetitionJobReevaluationAuthorization
		if err := decodeFailureEvidenceJson([]byte(malformed), &authorization); err == nil {
			t.Fatal("ambiguous operator proof crossed the typed decoder")
		}
	}
}

// A public authenticated marker keeps the same paid cache identity and old
// failure. It grants no extra fee, publication authority or later retry cycle.
func TestCompetitionReevaluationOperatorInterruptionPollRetainsFeeAndHistory(t *testing.T) {
	store, name, poll, now := newApexReevaluationFixtureWithFailure(t, true,
		EvaluationFailure{Kind: "infrastructure", Code: "evaluation_time_budget_exhausted", Retriable: false})
	before, err := store.Get(name)
	if err != nil || before == nil {
		t.Fatalf("read original synthetic budget cache: %v", err)
	}
	poll.Reevaluation.IncidentClass = "operator_authorized_interrupted_budget_v1"
	sealApexReevaluationCycle(t, poll.Reevaluation)
	after, err := store.RecordPoll(name, poll, now)
	if err != nil || after == nil || after.Reevaluation == nil || after.Reevaluation.IncidentClass != poll.Reevaluation.IncidentClass ||
		after.JobId != before.JobId || after.RoundId != before.RoundId || after.FeeReceipt != before.FeeReceipt ||
		len(after.ReevaluationHistory) != 1 || after.ReevaluationHistory[0].EvaluationFailure != *before.EvaluationFailure ||
		after.PrivateScore != nil || after.Score != nil || after.Published || after.Winner {
		t.Fatalf("operator receipt changed paid identity, history or publication: %v", err)
	}
	unmarked := poll
	unmarked.Reevaluation = nil
	if _, err := store.RecordPoll(name, unmarked, now.Add(time.Second)); err == nil {
		t.Fatal("old unmarked observation erased the committed recovery cycle")
	}
}
