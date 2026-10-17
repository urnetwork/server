// Frozen patch policy is authoritative even when a replica's current settings have changed.
package controller

import (
	"context"
	"net/http"
	"testing"

	"github.com/urnetwork/server"
)

type competitionAutomationPreflightFixture struct {
	*fakeStore
	screened int
}

// Records that validation reached the durable pre-fee boundary.
func (self *competitionAutomationPreflightFixture) ScreenAutomationSubmission(context.Context, *Settings, server.Id, *CanonicalPatch) (bool, string, error) {
	self.screened++
	return true, "", nil
}

// Both validation paths accept only the policy committed before the round opened.
func TestCompetitionV2AdmissionAndPreflightUseFrozenPatchPolicy(t *testing.T) {
	settings := automationApiSettings()
	frozen, err := policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	roundId, jobId := server.NewId(), server.NewId()
	round := &roundRecord{RoundResult: RoundResult{RoundId: roundId, Staging: true, Status: "open"}, PolicyJson: frozen}
	store := &competitionAutomationPreflightFixture{fakeStore: &fakeStore{stagingRound: round,
		enqueueJob: &queuedJob{ScoreJobResult: ScoreJobResult{JobId: jobId, RoundId: roundId, State: "queued"}, Round: *round}}}
	// A later deployment's smaller limit must not change an already committed round.
	settings.PatchPolicy.MaxPatchBytes = 1
	service := newServiceWithImageDigest(settings, store, testApiImageDigest(), nil)
	principal := &Principal{Id: "test-submitter", Role: "submitter"}
	args := ScoreArgs{RoundId: roundId, Patch: testPatch("frozen-policy")}
	if _, status, evalError := service.PreflightV2(context.Background(), args, principal); status != http.StatusOK || evalError != nil || store.screened != 1 {
		t.Fatalf("preflight used current policy: status=%d error=%+v screened=%d", status, evalError, store.screened)
	}
	if _, status, evalError := service.SubmitV2(context.Background(), args, principal); status != http.StatusAccepted || evalError != nil || store.enqueueCalls != 1 {
		t.Fatalf("admission used current policy: status=%d error=%+v calls=%d", status, evalError, store.enqueueCalls)
	}
	// The inverse also matters: widening current configuration cannot widen the frozen limit.
	round.PolicyJson, err = policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	settings.PatchPolicy.MaxPatchBytes = 262144
	if _, status, _ := service.PreflightV2(context.Background(), args, principal); status != http.StatusRequestEntityTooLarge || store.screened != 1 {
		t.Fatalf("preflight bypassed frozen limit: status=%d screened=%d", status, store.screened)
	}
	if _, status, _ := service.SubmitV2(context.Background(), args, principal); status != http.StatusRequestEntityTooLarge || store.enqueueCalls != 1 {
		t.Fatalf("admission bypassed frozen limit: status=%d calls=%d", status, store.enqueueCalls)
	}
}
