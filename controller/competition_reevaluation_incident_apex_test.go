// Cached unstable failures need a bounded authenticated discovery path for an
// operator-approved backing-service recovery, not an automatic cache reset.
package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// The real owner poll first observes no grant, then an invalid grant and finally
// the exact typed receipt. Only that last observation can advance the cache.
func TestCompetitionReevaluationApexBackingServiceRefreshRequiresAuthenticatedCycle(t *testing.T) {
	store, name, poll, now := newApexReevaluationFixtureWithFailure(t, true, EvaluationFailure{Kind: "submission", Code: "run_unstable"})
	before, err := store.Get(name)
	if err != nil || before == nil {
		t.Fatalf("read original unstable cache: %v", err)
	}
	invalidCycle := *poll.Reevaluation
	invalidCycle.IncidentClass = ""
	sealApexReevaluationCycle(t, &invalidCycle)
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-backing-owner" || r.URL.Path != "/competition/v2/score/"+poll.JobId.String() {
			t.Error("backing-service discovery lost owner authentication or exact job path")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		response := poll
		switch calls.Add(1) {
		case 1:
			response.State, response.EvaluationStatus, response.EvaluationFailure, response.Reevaluation = "completed", "failed", before.EvaluationFailure, nil
		case 2:
			response.Reevaluation = &invalidCycle
		}
		writeCompetitionJson(w, http.StatusOK, response)
	}))
	defer api.Close()
	options := ApexAdapterOptions{HttpClient: api.Client(), MaxAttempts: 1, Now: func() time.Time { return now },
		V2CompetitionId: "synthetic-competition", PrivateScoreRefreshEvery: time.Minute}
	adapter, err := NewApexAdapter(api.URL, "synthetic-backing-owner", store, &apexConformanceFeeCollector{}, options)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := adapter.PollNext(context.Background())
	if err != nil || unchanged == nil || unchanged.Reevaluation != nil || unchanged.EvaluationFailure == nil ||
		*unchanged.EvaluationFailure != *before.EvaluationFailure || len(unchanged.ReevaluationHistory) != 0 || calls.Load() != 1 {
		t.Fatalf("owner probe invented recovery from an unstable code: %+v %v calls=%d", unchanged, err, calls.Load())
	}
	now = now.Add(2 * time.Minute)
	if record, err := adapter.PollNext(context.Background()); err == nil || record != nil || calls.Load() != 2 {
		t.Fatalf("untyped receipt superseded submission failure: %+v %v calls=%d", record, err, calls.Load())
	}
	saved, err := store.Get(name)
	if err != nil || saved.Reevaluation != nil || saved.EvaluationFailure == nil || *saved.EvaluationFailure != *before.EvaluationFailure {
		t.Fatal("rejected receipt erased cached original failure", err)
	}
	now = now.Add(2 * time.Minute)
	recovered, err := adapter.PollNext(context.Background())
	if err != nil || recovered == nil || recovered.Reevaluation == nil || recovered.State != "queued" || calls.Load() != 3 ||
		recovered.JobId != before.JobId || recovered.FeeReceipt != before.FeeReceipt || len(recovered.ReevaluationHistory) != 1 ||
		recovered.ReevaluationHistory[0].EvaluationFailure != *before.EvaluationFailure || recovered.Published || recovered.Winner {
		t.Fatalf("typed backing-service recovery lost original association/history: %+v %v calls=%d", recovered, err, calls.Load())
	}
	reopened, err := NewApexAdapterFileStore(store.directory)
	if err != nil {
		t.Fatal(err)
	}
	durable, err := reopened.Get(name)
	if err != nil || !reflect.DeepEqual(durable, recovered) {
		t.Fatalf("recovery receipt or original failure lost on cache restart: %v", err)
	}
}

// Discovery cannot consume a used cycle, expose a completed/private score or
// bypass the existing publication, staging and terminal retry bounds.
func TestCompetitionReevaluationApexBackingServiceRefreshRetainsDiscoveryBounds(t *testing.T) {
	store, name, poll, _ := newApexReevaluationFixtureWithFailure(t, true, EvaluationFailure{Kind: "submission", Code: "run_unstable"})
	record, err := store.Get(name)
	if err != nil || record == nil || !apexReevaluationRefreshEligible(*record) {
		t.Fatalf("unscored unstable receipt discovery unavailable: %v", err)
	}
	for _, c := range []struct {
		name   string
		mutate func(*ApexAdapterRecord)
	}{
		{name: "used", mutate: func(r *ApexAdapterRecord) { r.Reevaluation = cloneApexReevaluationCycle(poll.Reevaluation) }},
		{name: "published", mutate: func(r *ApexAdapterRecord) { r.Published = true }},
		{name: "staging", mutate: func(r *ApexAdapterRecord) { r.Staging = true }},
		{name: "retriable", mutate: func(r *ApexAdapterRecord) {
			r.EvaluationFailure = &EvaluationFailure{Kind: "submission", Code: "run_unstable", Retriable: true}
		}},
		{name: "completed", mutate: func(r *ApexAdapterRecord) { r.EvaluationStatus = "completed" }},
		{name: "private-score", mutate: func(r *ApexAdapterRecord) { r.PrivateScore = privateScoreFeedbackFixture() }},
		{name: "resource-failure", mutate: func(r *ApexAdapterRecord) {
			r.EvaluationFailure = &EvaluationFailure{Kind: "submission", Code: "resource_violation"}
		}},
	} {
		changed := *record
		c.mutate(&changed)
		if apexReevaluationRefreshEligible(changed) {
			t.Fatalf("%s widened read-only discovery", c.name)
		}
	}
}
