// The group executor is exercised with explicit synthetic custody/provider
// hooks. No host process, network, real claim or provider qualification is used.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

// A deterministic clock orders events without sleeps or wall-clock assumptions.
type winnerMultipassQualificationActor struct {
	Hooks            winnerMultipassQualificationHooks
	Captures         map[int]winnerMultipassQualificationCapture
	Pins             map[int]currentQgQualificationPin
	Claims           map[int][]byte
	Invoked          []int
	CurrentClaim     winnerMultipassQualificationClaim
	Clock            time.Time
	Ticks            int
	Ambiguous        int
	FailAt           int
	ReservationError error
}

// These hooks retain exact bytes in test-owned memory. Their Mode is permanently
// offline; real release authentication is tested separately by its actual reader.
func winnerMultipassQualificationActorFixture(t *testing.T, plan *winnerMultipassQualificationPlan) *winnerMultipassQualificationActor {
	t.Helper()
	actor := &winnerMultipassQualificationActor{Captures: map[int]winnerMultipassQualificationCapture{}, Pins: map[int]currentQgQualificationPin{}, Claims: map[int][]byte{}, Invoked: []int{}, Clock: plan.Owner.StartedAt}
	judgments := map[int]winnerModelJudgment{}
	for _, item := range plan.Cases[1:] {
		content := currentQgExecutionMockResponse(t, item.Bundle.Request, item.Expectation, nil)
		var result winnerReviewBrokerResult
		var snow winnerSnowEvidence
		if json.Unmarshal(content, &result) != nil || json.Unmarshal([]byte(result.PrivateEvidence), &snow) != nil {
			t.Fatal("synthetic judgment unavailable")
		}
		judgments[item.Position] = snow.Members[0].Judgment
	}
	actor.Hooks = winnerMultipassQualificationHooks{Mode: "offline", Now: func() time.Time {
		actor.Ticks++
		actor.Clock = plan.Owner.StartedAt.Add(time.Duration(actor.Ticks) * time.Second)
		return actor.Clock
	},
		ValidateRelease: func(actual winnerMultipassQualificationRelease) error {
			if !reflect.DeepEqual(actual, plan.Release) {
				return errors.New("synthetic release changed")
			}
			return nil
		},
		Reserve: func(*winnerMultipassQualificationCapacity) error { return actor.ReservationError },
		Load: func(invocation winnerMultipassQualificationInvocation) (*winnerMultipassQualificationCapture, currentQgQualificationPin, error) {
			if actor.Ambiguous == invocation.Position {
				return nil, currentQgQualificationPin{}, errors.New("synthetic existing started intent has no authenticated completion")
			}
			if value, found := actor.Captures[invocation.Position]; found {
				return &value, actor.Pins[invocation.Position], nil
			}
			return nil, currentQgQualificationPin{}, nil
		},
		Claim: func(invocation winnerMultipassQualificationInvocation, content []byte) (currentQgQualificationPin, error) {
			if _, found := actor.Claims[invocation.Position]; found {
				return currentQgQualificationPin{}, errors.New("synthetic original claim already exists")
			}
			actor.Claims[invocation.Position] = append([]byte{}, content...)
			if err := json.Unmarshal(content, &actor.CurrentClaim); err != nil {
				return currentQgQualificationPin{}, err
			}
			return currentQgQualificationPin{Path: fmt.Sprintf("/synthetic/claim-%04d.json", invocation.Position), Sha256: winnerMultipassSha256(content)}, nil
		},
		Invoke: func(_ context.Context, input []byte) ([]byte, []byte, error) {
			var envelope winnerMultipassBrokerEnvelope
			if err := json.Unmarshal(input, &envelope); err != nil {
				return nil, nil, err
			}
			invocation := actor.CurrentClaim.Invocation
			actor.Invoked = append(actor.Invoked, invocation.Position)
			if envelope.Operation == "qualification_cancel_before_provider" {
				return []byte{}, []byte("synthetic pre-input cancellation"), errors.New("synthetic expected nonzero cancellation")
			}
			item := plan.Cases[invocation.CasePosition-1]
			result := winnerMultipassBrokerResult{Schema: 1, Kind: "winner-review-multipass-call-result/v1", Operation: envelope.Operation, CallId: envelope.Call.Id, RequestSha256: winnerMultipassSha256(envelope.Payload), StartedAt: actor.Clock, CompletedAt: actor.Clock.Add(time.Millisecond), TransportStatus: 200, TransportContentType: "application/json"}
			if envelope.Operation == "tokenize" {
				admission := winnerMultipassAdmissionFixture(item.Plan, envelope.Call, envelope.Payload, 100)
				result.Admission = &admission
				result.TokenizerRequestArtifact = "tokenize.request.json"
				result.TokenizerResponseArtifact = "tokenize.response.json"
			} else {
				observation := winnerMultipassObservation{Summary: "Synthetic group-control observation; not model evidence.", Issues: []winnerMultipassIssue{}, RetainedIssueIds: []string{}, DeferredArtifactIds: []string{}}
				if envelope.Call.Stage == "final" {
					judgment := judgments[item.Position]
					observation.FinalJudgment = &judgment
				}
				output, _ := json.Marshal(observation)
				terminal, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("synthetic-group-response-%04d", invocation.Position), "status": "completed", "model": envelope.Call.Member.Model, "usage": map[string]int{"input_tokens": 100, "output_tokens": 100}, "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": string(output)}}}}})
				result.Admission = envelope.Admission
				result.VisibleResponse = terminal
				result.VisibleResponseSha256 = winnerMultipassSha256(terminal)
				result.TransportResponseSha256 = result.VisibleResponseSha256
				result.TransportResponseArtifact = "transport.response"
				result.ProviderOutput = string(output)
				result.Observation = observation
			}
			if invocation.Position == actor.FailAt {
				result.Error = "synthetic terminal provider failure"
			}
			content, _ := json.Marshal(result)
			if result.Error != "" {
				return content, []byte("synthetic failure diagnostics"), errors.New(result.Error)
			}
			return content, []byte("synthetic containment diagnostics"), nil
		},
		Authenticate: func(input, output, diagnostics []byte, canceled bool) (*winnerMultipassContainmentReceipt, error) {
			receipt := &winnerMultipassContainmentReceipt{AttemptDirectory: "/synthetic/attempt", ManifestSha256: winnerMultipassSha256(append(append([]byte{}, input...), diagnostics...)), WrapperReceiptSha256: winnerMultipassSha256(append(append([]byte{}, input...), output...)), WrapperReceipt: []byte("synthetic wrapper proof"), Artifacts: []currentQgQualificationBinding{}, Canceled: canceled}
			if !canceled {
				receipt.Result = &winnerMultipassBrokerResult{}
				if err := json.Unmarshal(output, receipt.Result); err != nil {
					return nil, err
				}
			}
			return receipt, nil
		},
		Retain: func(_ context.Context, value winnerMultipassQualificationCapture) (currentQgQualificationPin, error) {
			content, _ := json.Marshal(value)
			pin := currentQgQualificationPin{Path: fmt.Sprintf("/synthetic/capture-%04d.json", value.Invocation.Position), Sha256: winnerMultipassSha256(content)}
			actor.Captures[value.Invocation.Position] = value
			actor.Pins[value.Invocation.Position] = pin
			return pin, nil
		},
		RetainCase: func(position int, value winnerMultipassEvidence) (currentQgQualificationPin, error) {
			content, _ := json.Marshal(value)
			return currentQgQualificationPin{Path: fmt.Sprintf("/synthetic/case-%02d.json", position), Sha256: winnerMultipassSha256(content)}, nil
		},
	}
	return actor
}

// Full synthetic execution and authenticated replay exercise original-data
// reconstruction without promoting an offline completion into qualification.
func TestWinnerMultipassQualificationOfflineExecutionAndResumeNeverQualify(t *testing.T) {
	plan := winnerMultipassQualificationFixture(t)
	actor := winnerMultipassQualificationActorFixture(t, plan)
	content, _ := json.Marshal(plan)
	pin := currentQgQualificationPin{Path: "/synthetic/plan.json", Sha256: winnerMultipassSha256(content)}
	capacity := defaultWinnerMultipassCapacitySettings()
	capacity.MaximumDurableBytes = 64 * 1024 * 1024 * 1024
	report, err := executeWinnerMultipassQualification(context.Background(), plan, pin, capacity, actor.Hooks)
	if err != nil || report == nil || !report.Complete || report.Qualified || len(report.CaseEvidence) != 18 || len(actor.Invoked) != len(plan.Invocations) {
		t.Fatalf("offline group execution did not reconstruct exact complete outcomes: %v", err)
	}
	originalCalls := len(actor.Invoked)
	actor.Hooks.Now = func() time.Time { return plan.Owner.Deadline.Add(time.Hour) }
	resumed, err := executeWinnerMultipassQualification(context.Background(), plan, pin, capacity, actor.Hooks)
	if err != nil || !resumed.Complete || resumed.Qualified || len(actor.Invoked) != originalCalls || !reflect.DeepEqual(resumed, report) {
		t.Fatal("completed replay renewed clocks, redrew a model or relabeled offline evidence", err)
	}
}

// A genuine-shaped failed draw is retained and terminates the whole attempt.
func TestWinnerMultipassQualificationFailedDrawIsRetainedAndNeverRerolled(t *testing.T) {
	plan := winnerMultipassQualificationFixture(t)
	actor := winnerMultipassQualificationActorFixture(t, plan)
	for _, invocation := range plan.Invocations {
		if invocation.Operation == "generate" {
			actor.FailAt = invocation.Position
			break
		}
	}
	content, _ := json.Marshal(plan)
	pin := currentQgQualificationPin{Path: "/synthetic/plan.json", Sha256: winnerMultipassSha256(content)}
	capacity := defaultWinnerMultipassCapacitySettings()
	capacity.MaximumDurableBytes = 64 * 1024 * 1024 * 1024
	report, err := executeWinnerMultipassQualification(context.Background(), plan, pin, capacity, actor.Hooks)
	if err == nil || report == nil || report.Qualified || report.Complete || len(actor.Invoked) != actor.FailAt || len(report.Captures) != actor.FailAt || actor.Captures[actor.FailAt].InvokeError == "" {
		t.Fatal("failed original draw was discarded or allowed a successor")
	}
	before := len(actor.Invoked)
	if _, err := executeWinnerMultipassQualification(context.Background(), plan, pin, capacity, actor.Hooks); err == nil || len(actor.Invoked) != before {
		t.Fatal("failed original draw was rerolled")
	}
}

// Missing completion is not an invitation to dispatch the same durable intent.
func TestWinnerMultipassQualificationAmbiguousIntentBlocksBeforeDispatch(t *testing.T) {
	plan := winnerMultipassQualificationFixture(t)
	actor := winnerMultipassQualificationActorFixture(t, plan)
	actor.Ambiguous = 1
	content, _ := json.Marshal(plan)
	pin := currentQgQualificationPin{Path: "/synthetic/plan.json", Sha256: winnerMultipassSha256(content)}
	capacity := defaultWinnerMultipassCapacitySettings()
	capacity.MaximumDurableBytes = 64 * 1024 * 1024 * 1024
	if _, err := executeWinnerMultipassQualification(context.Background(), plan, pin, capacity, actor.Hooks); err == nil || len(actor.Invoked) != 0 || len(actor.Claims) != 0 {
		t.Fatal("ambiguous started operation was resampled")
	}
}

// Capacity refusal happens before cancellation, tokenizer or generation input.
func TestWinnerMultipassQualificationReservationFailureMakesNoProviderCall(t *testing.T) {
	plan := winnerMultipassQualificationFixture(t)
	actor := winnerMultipassQualificationActorFixture(t, plan)
	actor.ReservationError = errors.New("synthetic insufficient allocated disk")
	content, _ := json.Marshal(plan)
	pin := currentQgQualificationPin{Path: "/synthetic/plan.json", Sha256: winnerMultipassSha256(content)}
	capacity := defaultWinnerMultipassCapacitySettings()
	capacity.MaximumDurableBytes = 64 * 1024 * 1024 * 1024
	if _, err := executeWinnerMultipassQualification(context.Background(), plan, pin, capacity, actor.Hooks); err == nil || len(actor.Invoked) != 0 || len(actor.Claims) != 0 {
		t.Fatal("unreserved qualification began network activity")
	}
}
