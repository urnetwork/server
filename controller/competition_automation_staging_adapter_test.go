// The real reference adapter must never touch payment authority for explicit staging admissions.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Any invocation is itself failure; no receipt can mask accidental fee collection.
type stagingFeeTripwire struct {
	calls int
}

// Takes no money and records every attempted crossing of the staging fee boundary.
func (self *stagingFeeTripwire) CollectOnce(context.Context, string, int) (string, error) {
	self.calls++
	return "", errors.New("staging crossed the fee boundary")
}

// Retry and cached replay traverse the production adapter implementation with a forbidden collector.
func TestApexV2StagingAdmissionRetryAndReplayNeverCollectsFee(t *testing.T) {
	roundId, jobId := server.NewId(), server.NewId()
	patch := []byte(testPatch("synthetic-stage-adapter"))
	patchSha256 := digestHex(patch)
	competitionId := "synthetic-v2-stage"
	submitCalls := 0
	api := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		encode := func(value any) {
			if err := json.NewEncoder(response).Encode(value); err != nil {
				t.Error(err)
			}
		}
		switch request.URL.Path {
		case "/competition/info":
			encode(InfoResult{CompetitionId: competitionId, StagingRound: &RoundResult{RoundId: roundId, Epoch: 1, Staging: true, Status: "open", DecisionMode: model.CompetitionDecisionModeV2, RequiredApiCapability: model.CompetitionAutomationCapability}})
		case "/competition/v2/score":
			submitCalls++
			if submitCalls == 1 {
				response.WriteHeader(http.StatusServiceUnavailable)
				encode(infrastructureError("synthetic_unavailable", "synthetic retry boundary"))
				return
			}
			encode(ScoreAcceptedResult{JobId: jobId, RoundId: roundId, Staging: true, PatchSha256: patchSha256, State: "queued", StatusUrl: "/competition/v2/score/" + jobId.String(), DecisionMode: model.CompetitionDecisionModeV2, RequiredApiCapability: model.CompetitionAutomationCapability})
		default:
			t.Errorf("unexpected staging request %s", request.URL.Path)
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	defer api.Close()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := NewApexAdapterFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	fees := &stagingFeeTripwire{}
	adapter, err := NewApexAdapter(api.URL, "synthetic-submitter-token", store, fees, ApexAdapterOptions{HttpClient: api.Client(), V2CompetitionId: competitionId, Wait: func(context.Context, time.Duration) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		record, err := adapter.Submit(context.Background(), "synthetic-stage-submission", patch)
		if err != nil || record == nil || !record.Staging || record.JobId != jobId || record.FeeUsd != 0 || record.FeeReceipt != "" {
			t.Fatalf("fee-free v2 staging admission: %+v, %v", record, err)
		}
	}
	if fees.calls != 0 || submitCalls != 2 {
		t.Fatalf("unexpected fee or retry calls: fees=%d submits=%d", fees.calls, submitCalls)
	}
}

// Identical environment/season strings cannot make production and staging inboxes interchangeable.
func TestApexV2StagingRewardScopeRejectsProductionReplay(t *testing.T) {
	scope, now, event, _ := rewardAdapterFixture(t)
	scope.Staging = true
	if err := validateApexRewardEvent(event, scope, now); err == nil {
		t.Fatal("staging inbox accepted a production decision")
	}
	event.Decision.Staging = true
	event.Decision.DecisionSha256 = ""
	event.Decision.DecisionSha256, _ = model.CompetitionAutomationDigest(event.Decision)
	event.PayloadSha256, _ = model.CompetitionAutomationDigest(event.Decision)
	if err := validateApexRewardEvent(event, scope, now); err != nil {
		t.Fatal(err)
	}
	event.Decision.SettlementMode = "live"
	if err := validateApexRewardEvent(event, scope, now); err == nil {
		t.Fatal("staging inbox accepted live settlement authority")
	}
}
