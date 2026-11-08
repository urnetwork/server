// Qualification execution uses explicit native custody/claim hooks. Offline
// hooks can exercise the complete reader but cannot mark a provider qualified.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"
)

// The durable intent is hashed before dispatch. Its digest can enter the
// generation envelope without a self-referential request/claim hash cycle.
type winnerMultipassQualificationClaim struct {
	Schema          int                                    `json:"schema"`
	GroupSha256     string                                 `json:"group_sha256"`
	Invocation      winnerMultipassQualificationInvocation `json:"invocation"`
	Owner           winnerMultipassOwner                   `json:"owner"`
	PayloadSha256   string                                 `json:"payload_sha256"`
	AdmissionSha256 string                                 `json:"admission_sha256"`
	StartedAt       time.Time                              `json:"started_at"`
	CallDeadline    time.Time                              `json:"call_deadline"`
}

// Original host I/O remains exact bytes until the native custody writer retains
// it, along with every authenticated wrapper manifest leaf. No source is public.
type winnerMultipassQualificationCapture struct {
	Invocation          winnerMultipassQualificationInvocation `json:"invocation"`
	Claim               currentQgQualificationPin              `json:"claim"`
	ClaimContent        []byte                                 `json:"claim_content"`
	Input               []byte                                 `json:"input"`
	Output              []byte                                 `json:"output"`
	Diagnostics         []byte                                 `json:"diagnostics"`
	Containment         *winnerMultipassContainmentReceipt     `json:"containment"`
	InvokeError         string                                 `json:"invoke_error"`
	AuthenticationError string                                 `json:"authentication_error"`
}

// The final report binds originals by exact retained pins; it does not embed
// every source/prompt/response again or grant source/reward/API authority.
type winnerMultipassQualificationReport struct {
	Schema       int                                   `json:"schema"`
	Kind         string                                `json:"kind"`
	Protocol     string                                `json:"protocol"`
	Mode         string                                `json:"mode"`
	Plan         currentQgQualificationPin             `json:"plan"`
	PlanSha256   string                                `json:"plan_sha256"`
	Release      winnerMultipassQualificationRelease   `json:"release"`
	Owner        winnerMultipassOwner                  `json:"owner"`
	Capacity     *winnerMultipassQualificationCapacity `json:"capacity"`
	Captures     []currentQgQualificationPin           `json:"captures"`
	CaseEvidence map[int]currentQgQualificationPin     `json:"case_evidence"`
	Complete     bool                                  `json:"complete"`
	Qualified    bool                                  `json:"qualified"`
	Error        string                                `json:"error"`
}

// Load may return only a wholly authenticated completed capture or nil for a
// genuinely unstarted invocation. A started/ambiguous intent is an error. Claim
// must exclusively persist/fsync the supplied exact bytes before Invoke. Retain
// must protect originals/readback before any successor invocation can dispatch.
type winnerMultipassQualificationHooks struct {
	Mode            string
	Now             func() time.Time
	ValidateRelease func(winnerMultipassQualificationRelease) error
	Reserve         func(*winnerMultipassQualificationCapacity) error
	Load            func(winnerMultipassQualificationInvocation) (*winnerMultipassQualificationCapture, currentQgQualificationPin, error)
	Claim           func(winnerMultipassQualificationInvocation, []byte) (currentQgQualificationPin, error)
	Invoke          func(context.Context, []byte) ([]byte, []byte, error)
	Authenticate    func([]byte, []byte, []byte, bool) (*winnerMultipassContainmentReceipt, error)
	Retain          func(context.Context, winnerMultipassQualificationCapture) (currentQgQualificationPin, error)
	RetainCase      func(int, winnerMultipassEvidence) (currentQgQualificationPin, error)
}

// Exact record construction and replay share this state, never another
// candidate's or model's conclusions. All planned leaf admissions precede draws.
type winnerMultipassQualificationState struct {
	Evidence       map[int]winnerMultipassEvidence
	Completed      map[int]map[string]winnerMultipassCallEvidence
	LeafAdmissions map[int][]winnerMultipassAdmission
	Admissions     map[string]winnerMultipassAdmission
	ResponseIds    map[string]bool
}

// Initial evidence already binds the final immutable clock, plan and bundle.
func newWinnerMultipassQualificationState(plan *winnerMultipassQualificationPlan) *winnerMultipassQualificationState {
	state := &winnerMultipassQualificationState{Evidence: map[int]winnerMultipassEvidence{}, Completed: map[int]map[string]winnerMultipassCallEvidence{}, LeafAdmissions: map[int][]winnerMultipassAdmission{}, Admissions: map[string]winnerMultipassAdmission{}, ResponseIds: map[string]bool{}}
	for _, item := range plan.Cases {
		content, _ := json.Marshal(item.Plan)
		state.Evidence[item.Position] = winnerMultipassEvidence{Version: winnerMultipassVersion, PlanSha256: winnerMultipassSha256(content), BundleSha256: item.Plan.BundleSha256, Calls: []winnerMultipassCallEvidence{}}
		state.Completed[item.Position] = map[string]winnerMultipassCallEvidence{}
	}
	return state
}

// Only a declared child may be tokenized or dispatched; source/context comes
// from the exact complete plan and mechanically replayed predecessor originals.
func winnerMultipassQualificationEnvelope(plan *winnerMultipassQualificationPlan, invocation winnerMultipassQualificationInvocation, state *winnerMultipassQualificationState, deadline time.Time) (winnerMultipassBrokerEnvelope, error) {
	item := plan.Cases[invocation.CasePosition-1]
	var call winnerMultipassCall
	for _, candidate := range item.Plan.Calls {
		if candidate.Id == invocation.CallId {
			call = candidate
			break
		}
	}
	if call.Id == "" {
		return winnerMultipassBrokerEnvelope{}, errors.New("qualification invocation has no declared coverage call")
	}
	payload, err := buildWinnerMultipassCall(item.Plan, item.Bundle, call.Id, state.Completed[item.Position])
	if err != nil {
		return winnerMultipassBrokerEnvelope{}, err
	}
	if currentQgQualificationContainsExpectedLabels(payload) {
		return winnerMultipassBrokerEnvelope{}, errors.New("private control labels entered provider input")
	}
	content, _ := json.Marshal(item.Plan)
	envelope := winnerMultipassBrokerEnvelope{Schema: 1, Kind: "winner-review-multipass-call/v1", Operation: invocation.Operation, Owner: item.Plan.Owner, CallDeadline: deadline, PlanSha256: winnerMultipassSha256(content), BundleSha256: item.Plan.BundleSha256, Call: call, Payload: payload, Settings: item.Plan.Settings, AllowModelLoad: invocation.AllowModelLoad}
	if invocation.Operation == "generate" {
		for _, control := range plan.Cases[1:] {
			if err := validateWinnerMultipassLeafAdmissions(control.Plan, control.Bundle, state.LeafAdmissions[control.Position]); err != nil {
				return envelope, err
			}
		}
		key := fmt.Sprintf("%02d/%s", item.Position, call.Id)
		admission, found := state.Admissions[key]
		if !found {
			return envelope, errors.New("generation lacks its immediately planned exact token admission")
		}
		envelope.Admission = &admission
		if err := validateWinnerMultipassAdmission(item.Plan, call, payload, admission); err != nil {
			return envelope, err
		}
	}
	return envelope, nil
}

// Replay validates the exact original envelope/output even for cached calls.
// The external receipt reader supplies actual containment; no model certifies it.
func applyWinnerMultipassQualificationCapture(plan *winnerMultipassQualificationPlan, invocation winnerMultipassQualificationInvocation, capture winnerMultipassQualificationCapture, state *winnerMultipassQualificationState, authenticate func([]byte, []byte, []byte, bool) (*winnerMultipassContainmentReceipt, error)) error {
	if !reflect.DeepEqual(capture.Invocation, invocation) || !validSha256(capture.Claim.Sha256) || capture.AuthenticationError != "" {
		return errors.New("qualification capture has an altered invocation/claim or failed authentication")
	}
	if err := validateWinnerMultipassQualificationClaim(capture.ClaimContent, plan, capture); err != nil {
		return err
	}
	var original winnerMultipassBrokerEnvelope
	if rejectWinnerReviewCaseAliases(capture.Input) != nil || decodeStrictJSONBytes(capture.Input, &original, "original qualification envelope") != nil {
		return errors.New("qualification original envelope malformed")
	}
	expected, err := winnerMultipassQualificationEnvelope(plan, invocation, state, original.CallDeadline)
	if err != nil {
		return err
	}
	if invocation.Operation == "generate" {
		expected.AuthorizedClaimSha256 = capture.Claim.Sha256
	}
	canonical, _ := json.Marshal(expected)
	if !bytes.Equal(canonical, capture.Input) {
		return errors.New("qualification captured input differs from exact immutable planned context")
	}
	canceled := invocation.Operation == "qualification_cancel_before_provider"
	receipt, err := authenticate(capture.Input, capture.Output, capture.Diagnostics, canceled)
	if err != nil {
		return err
	}
	if capture.Containment == nil || !reflect.DeepEqual(receipt, capture.Containment) || !validSha256(receipt.ManifestSha256) || !validSha256(receipt.WrapperReceiptSha256) || receipt.Canceled != canceled {
		return errors.New("qualification actual containment differs from retained original proof")
	}
	if canceled {
		if receipt.Result != nil || len(capture.Output) != 0 {
			return errors.New("pre-input cancellation returned model output")
		}
		return nil
	}
	if capture.InvokeError != "" || receipt.Result == nil || receipt.Result.Error != "" {
		return errors.New("qualification preserves a failed provider draw; no retry or successor allowed")
	}
	result := receipt.Result
	item := plan.Cases[invocation.CasePosition-1]
	if result.Operation != invocation.Operation || result.CallId != invocation.CallId || result.RequestSha256 != winnerMultipassSha256(original.Payload) {
		return errors.New("qualification provider result belongs to another request")
	}
	if invocation.Operation == "tokenize" {
		if result.Admission == nil {
			return errors.New("qualification tokenizer lost its actual proof")
		}
		if err := validateWinnerMultipassAdmission(item.Plan, original.Call, original.Payload, *result.Admission); err != nil {
			return err
		}
		if invocation.Phase == "frozen_leaf_preflight" {
			state.LeafAdmissions[item.Position] = append(state.LeafAdmissions[item.Position], *result.Admission)
		} else {
			state.Admissions[fmt.Sprintf("%02d/%s", item.Position, invocation.CallId)] = *result.Admission
		}
		return nil
	}
	if result.Admission == nil || !reflect.DeepEqual(result.Admission, original.Admission) {
		return errors.New("qualification generation replaced actual token admission")
	}
	value := winnerMultipassCallEvidence{CallId: invocation.CallId, StartedAt: result.StartedAt, CompletedAt: result.CompletedAt, Request: original.Payload, Admission: *result.Admission,
		VisibleResponse: result.VisibleResponse, VisibleResponseSha256: result.VisibleResponseSha256, TransportResponseSha256: result.TransportResponseSha256, TransportReceiptSha256: receipt.WrapperReceiptSha256, ProviderOutput: result.ProviderOutput, Observation: result.Observation}
	var response winnerModelResponse
	if json.Unmarshal(value.VisibleResponse, &response) != nil || state.ResponseIds[response.Id] {
		return errors.New("qualification provider response replayed across calls or controls")
	}
	evidence := state.Evidence[item.Position]
	evidence.Calls = append(evidence.Calls, value)
	completed, err := validateWinnerMultipassCompletedPrefix(item.Plan, item.Bundle, evidence)
	if err != nil {
		return err
	}
	state.ResponseIds[response.Id] = true
	state.Evidence[item.Position] = evidence
	state.Completed[item.Position] = completed
	if original.Call.Stage == "final" {
		judgment := value.Observation.FinalJudgment
		if judgment == nil || judgment.Decision != item.Expectation.Decision || judgment.WorkloadPreservation == nil || judgment.ProductionBenefit == nil || judgment.WorkloadPreservation.Disposition != item.Expectation.WorkloadDisposition || judgment.ProductionBenefit.Disposition != item.Expectation.ProductionDisposition {
			return errors.New("genuine qualification member failed its precommitted control expectation; stop without reroll")
		}
	}
	return nil
}

// Execute only the fixed invocation census. Native hooks own exclusive durable
// claims, storage reservations and full original custody; this function performs
// no direct network, DB, API approval, source promotion or reward operation.
func executeWinnerMultipassQualification(ctx context.Context, plan *winnerMultipassQualificationPlan, planPin currentQgQualificationPin, capacitySettings winnerMultipassCapacitySettings, hooks winnerMultipassQualificationHooks) (report *winnerMultipassQualificationReport, resultErr error) {
	if err := validateWinnerMultipassQualificationPlan(plan); err != nil {
		return nil, err
	}
	if hooks.Mode != "offline" && hooks.Mode != "genuine_provider" || hooks.Now == nil || hooks.ValidateRelease == nil || hooks.Reserve == nil || hooks.Load == nil || hooks.Claim == nil || hooks.Invoke == nil || hooks.Authenticate == nil || hooks.Retain == nil || hooks.RetainCase == nil {
		return nil, errors.New("qualification requires complete explicit native release/custody hooks")
	}
	content, _ := json.Marshal(plan)
	digest := winnerMultipassSha256(content)
	if planPin.Sha256 != digest {
		return nil, errors.New("qualification owner did not retain its exact precommitted plan")
	}
	report = &winnerMultipassQualificationReport{Schema: 1, Kind: "winner-review-multipass-qualification-report/v1", Protocol: winnerMultipassVersion, Mode: hooks.Mode, Plan: planPin, PlanSha256: digest, Release: plan.Release, Owner: plan.Owner, Captures: []currentQgQualificationPin{}, CaseEvidence: map[int]currentQgQualificationPin{}}
	defer func() {
		if resultErr != nil {
			report.Error = resultErr.Error()
			report.Qualified = false
		}
	}()
	if hooks.Mode == "genuine_provider" {
		if err := validateCurrentQgQualificationRuntime(); err != nil {
			return report, err
		}
	}
	if err := hooks.ValidateRelease(plan.Release); err != nil {
		return report, err
	}
	capacity, err := estimateWinnerMultipassQualificationCapacity(plan, capacitySettings)
	report.Capacity = capacity
	if err != nil {
		return report, err
	}
	if err := hooks.Reserve(capacity); err != nil {
		return report, err
	}
	state := newWinnerMultipassQualificationState(plan)
	for _, invocation := range plan.Invocations {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		capture, pin, err := hooks.Load(invocation)
		if err != nil {
			return report, err
		}
		if capture == nil {
			now := hooks.Now().UTC()
			if now.Before(plan.Owner.StartedAt) || !now.Before(plan.Owner.Deadline) {
				return report, errors.New("qualification immutable owner clock does not authorize an unstarted invocation")
			}
			deadline := now.Add(defaultWinnerMultipassProviderLimits().GenerationTimeout)
			if deadline.After(plan.Owner.Deadline) {
				deadline = plan.Owner.Deadline
			}
			envelope, err := winnerMultipassQualificationEnvelope(plan, invocation, state, deadline)
			if err != nil {
				return report, err
			}
			admissionBytes, _ := json.Marshal(envelope.Admission)
			claim := winnerMultipassQualificationClaim{Schema: 1, GroupSha256: digest, Invocation: invocation, Owner: envelope.Owner, PayloadSha256: winnerMultipassSha256(envelope.Payload), AdmissionSha256: winnerMultipassSha256(admissionBytes), StartedAt: now, CallDeadline: deadline}
			claimBytes, _ := json.Marshal(claim)
			claimPin, err := hooks.Claim(invocation, claimBytes)
			if err != nil {
				return report, err
			}
			if claimPin.Sha256 != winnerMultipassSha256(claimBytes) {
				return report, errors.New("native qualification claim did not retain exact original intent")
			}
			if invocation.Operation == "generate" {
				envelope.AuthorizedClaimSha256 = claimPin.Sha256
			}
			input, _ := json.Marshal(envelope)
			callCtx, cancel := context.WithDeadline(ctx, deadline)
			output, diagnostics, invokeErr := hooks.Invoke(callCtx, input)
			cancel()
			capture = &winnerMultipassQualificationCapture{Invocation: invocation, Claim: claimPin, ClaimContent: claimBytes, Input: input, Output: output, Diagnostics: diagnostics}
			if invokeErr != nil {
				capture.InvokeError = invokeErr.Error()
			}
			receipt, authErr := hooks.Authenticate(input, output, diagnostics, invocation.Operation == "qualification_cancel_before_provider")
			capture.Containment = receipt
			if authErr != nil {
				capture.AuthenticationError = authErr.Error()
			}
			retentionCtx, retentionCancel := context.WithTimeout(context.WithoutCancel(ctx), defaultCurrentQgQualificationSettings().CleanupGrace)
			pin, err = hooks.Retain(retentionCtx, *capture)
			retentionCancel()
			if err != nil {
				return report, err
			}
		}
		captureBytes, _ := json.Marshal(capture)
		if !validSha256(pin.Sha256) || pin.Sha256 != winnerMultipassSha256(captureBytes) {
			return report, errors.New("qualification capture lacks exact protected original custody")
		}
		report.Captures = append(report.Captures, pin)
		if err := applyWinnerMultipassQualificationCapture(plan, invocation, *capture, state, hooks.Authenticate); err != nil {
			return report, err
		}
		if invocation.Operation == "generate" {
			item := plan.Cases[invocation.CasePosition-1]
			value := state.Evidence[item.Position]
			if len(value.Calls) == len(item.Plan.Calls) {
				pin, err := hooks.RetainCase(item.Position, value)
				if err != nil {
					return report, err
				}
				content, _ := json.Marshal(value)
				if pin.Sha256 != winnerMultipassSha256(content) {
					return report, errors.New("completed qualification case lacks original evidence custody")
				}
				report.CaseEvidence[item.Position] = pin
			}
		}
	}
	delete(state.Evidence, 1)
	if err := validateWinnerMultipassQualificationOutcomes(plan, state.Evidence); err != nil {
		return report, err
	}
	report.Complete = true
	report.Qualified = hooks.Mode == "genuine_provider"
	return report, nil
}
