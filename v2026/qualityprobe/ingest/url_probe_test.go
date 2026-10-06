// Stable URL report identities make retries safe for durable cycle counts.
package ingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// An ambiguous first acknowledgement and an explicit retry carry one run id.
func TestUrlProbeReportRetryKeepsRunAndCycleIdentity(t *testing.T) {
	bodies := []submitEgressHealthBody{}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body submitEgressHealthBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		bodies = append(bodies, body)
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer endpoint.Close()
	client := &Client{ServerUrl: endpoint.URL, Http: endpoint.Client(), OperatorSecret: "synthetic-secret"}
	cycleStartedAt := time.Unix(1_800_000_000, 0).UTC()
	result := &egresshealth.Result{CycleStartedAt: cycleStartedAt, OkCount: 1, Total: 1,
		ByClass: map[egresshealth.Class]egresshealth.ClassSummary{egresshealth.ClassSite: {Ok: 1, Total: 1}}}
	providerClientId := connect.NewId().String()
	if err := client.SubmitEgressHealth(context.Background(), providerClientId, result); err == nil {
		t.Fatal("synthetic first acknowledgement should fail")
	}
	if err := client.SubmitEgressHealth(context.Background(), providerClientId, result); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || bodies[0].RunId == "" || bodies[0].RunId != bodies[1].RunId ||
		!bodies[0].CycleStartedAt.Equal(cycleStartedAt) || !bodies[1].CycleStartedAt.Equal(cycleStartedAt) {
		t.Fatalf("retry changed report identities: %+v", bodies)
	}
}

// A quota-full security recheck still carries its durable receipt identity and
// the original configured URL even when that URL has left the current catalog.
func TestUrlProbeDueCarriesRollingIdentityAndSecuritySnapshots(t *testing.T) {
	want := []DueProvider{{ClientId: connect.NewId().String(), RunsNeeded: 0,
		CycleStartedAt: time.Unix(1_800_000_000, 0).UTC(), OutcomeCount: 21, CountryCode: "zz",
		SecurityDestinations: []egresshealth.Destination{{Name: "retired-synthetic", Class: egresshealth.ClassSite,
			Url: "https://synthetic-security.example/original", Expect: egresshealth.ExpectBody, MaxBytes: 12345}}}}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewEncoder(w).Encode(map[string]any{"providers": want}); err != nil {
			t.Error(err)
		}
	}))
	defer endpoint.Close()
	client := &Client{ServerUrl: endpoint.URL, Http: endpoint.Client(), OperatorSecret: "synthetic-secret"}
	got, err := client.Due(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("due response dropped rolling or security recheck identity: got=%+v want=%+v", got, want)
	}
}
