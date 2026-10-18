// Frozen patch policy is authoritative even when a replica's current settings have changed.
package controller

import (
	"context"
	"net/http"
	"testing"

	"github.com/urnetwork/server"
)

// Both validation paths accept only the policy committed before the round opened.
func TestCompetitionV2AdmissionAndPreflightUseFrozenPatchPolicy(t *testing.T) {
	settings, stagingStore, _ := newAutomationStagingApiFixture(t)
	round := stagingStore.stagingRound
	store := &competitionAutomationStagingPreflightFixture{automationStagingApiStore: stagingStore}
	service := newServiceWithImageDigest(settings, store, testApiImageDigest(), nil)
	principal := &Principal{Id: "test-submitter", Role: "submitter"}
	args := ScoreArgs{RoundId: round.RoundId, Patch: testPatch("frozen-policy")}
	if _, status, evalError := service.SubmitV2(context.Background(), args, principal); status != http.StatusAccepted || evalError != nil || store.enqueueCalls != 1 {
		t.Fatalf("qualified initial admission failed: status=%d error=%+v", status, evalError)
	}
	// A later deployment's smaller limit must not change an already committed round.
	settings.PatchPolicy.MaxPatchBytes = 1
	if _, status, evalError := service.PreflightV2(context.Background(), args, principal); status != http.StatusOK || evalError != nil || store.screened != 1 {
		t.Fatalf("preflight used current policy: status=%d error=%+v screened=%d", status, evalError, store.screened)
	}
	// Screening can describe the frozen limit while runtime policy drift still
	// blocks actual admission; staging is not an escape from source readiness.
	if _, status, evalError := service.SubmitV2(context.Background(), args, principal); status != http.StatusServiceUnavailable || evalError == nil || evalError.Code != "round_source_not_ready" || store.enqueueCalls != 1 {
		t.Fatalf("admission ignored runtime drift: status=%d error=%+v calls=%d", status, evalError, store.enqueueCalls)
	}
	// The inverse also matters: widening current configuration cannot widen the frozen limit.
	frozen, err := sourceSettingsFromIntent(settings, store.intent)
	if err != nil {
		t.Fatal(err)
	}
	round.PolicyJson, err = policySnapshot(frozen)
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

type competitionAutomationStagingPreflightFixture struct {
	*automationStagingApiStore
	screened int
}

// Screening remains distinct from activation and does not enqueue or charge.
func (self *competitionAutomationStagingPreflightFixture) ScreenAutomationSubmission(context.Context, *Settings, server.Id, *CanonicalPatch) (bool, string, error) {
	self.screened++
	return true, "", nil
}
