package main

import (
	"encoding/json"
	"testing"
	"time"
)

// Total operation budgets agree across provider admission and original claims.
// Subrequests cannot each acquire a fresh five-minute model-loading allowance.
func TestWinnerMultipassOperationDeadlineAdmissionAndClaimParity(t *testing.T) {
	bundle, plan, _ := winnerMultipassProviderFixture(t)
	call := plan.Calls[0]
	payload, err := buildWinnerMultipassCall(plan, bundle, call.Id, map[string]winnerMultipassCallEvidence{})
	if err != nil {
		t.Fatal(err)
	}
	now := plan.Owner.StartedAt.Add(time.Minute)
	limits := defaultWinnerMultipassProviderLimits()
	group := &winnerMultipassQualificationPlan{Owner: plan.Owner}
	groupBytes, _ := json.Marshal(group)
	for _, item := range []struct {
		operation string
		bound     time.Duration
	}{{operation: "tokenize", bound: 5 * time.Minute}, {operation: "generate", bound: 30 * time.Minute}, {operation: "qualification_cancel_before_provider", bound: 5 * time.Minute}} {
		{
			bound, err := winnerMultipassOperationTimeout(item.operation, limits)
			if err != nil || bound != item.bound {
				t.Fatal("operation budget changed", err)
			}
			envelope := winnerMultipassBrokerEnvelope{Schema: 1, Kind: "winner-review-multipass-call/v1", Operation: item.operation, Owner: plan.Owner, CallDeadline: now.Add(bound), PlanSha256: winnerMultipassSha256(groupBytes), BundleSha256: plan.BundleSha256, Call: call, Payload: payload, Settings: plan.Settings}
			if item.operation == "generate" {
				admission := winnerMultipassAdmissionFixture(plan, call, payload, 100)
				envelope.Admission = &admission
			}
			invocation := winnerMultipassQualificationInvocation{Position: 1, CasePosition: 2, CallId: call.Id, Operation: item.operation, Phase: "synthetic"}
			admissionBytes, _ := json.Marshal(envelope.Admission)
			claim := winnerMultipassQualificationClaim{Schema: 1, GroupSha256: winnerMultipassSha256(groupBytes), Invocation: invocation, Owner: plan.Owner, PayloadSha256: winnerMultipassSha256(payload), AdmissionSha256: winnerMultipassSha256(admissionBytes), StartedAt: now, CallDeadline: envelope.CallDeadline}
			capture := winnerMultipassQualificationCapture{Invocation: invocation}
			bind := func() {
				capture.ClaimContent, _ = json.Marshal(claim)
				capture.Claim = currentQgQualificationPin{Path: "/synthetic/operation-claim.json", Sha256: winnerMultipassSha256(capture.ClaimContent)}
				if item.operation == "generate" {
					envelope.AuthorizedClaimSha256 = capture.Claim.Sha256
				}
				capture.Input, _ = json.Marshal(envelope)
			}
			bind()
			if err := validateWinnerMultipassQualificationClaim(capture.ClaimContent, group, capture); err != nil {
				t.Fatal("exact operation bound rejected", err)
			}
			if item.operation != "qualification_cancel_before_provider" {
				if err := validateWinnerMultipassBrokerEnvelope(envelope, plan.Provider, now, limits); err != nil {
					t.Fatal("provider disagrees with original claim deadline", err)
				}
			}
			envelope.CallDeadline = envelope.CallDeadline.Add(time.Nanosecond)
			claim.CallDeadline = envelope.CallDeadline
			bind()
			if err := validateWinnerMultipassQualificationClaim(capture.ClaimContent, group, capture); err == nil {
				t.Fatal("overlong original operation claim accepted")
			}
			if err := validateWinnerMultipassBrokerEnvelope(envelope, plan.Provider, now, limits); err == nil {
				t.Fatal("overlong operation envelope accepted")
			}
		}
	}
	if _, err := winnerMultipassOperationTimeout("synthetic-unknown", limits); err == nil {
		t.Fatal("unknown operation obtained a deadline")
	}
}
