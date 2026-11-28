// Prospective batching tests intercept every tokenizer count. They are neither
// live runtime measurements nor model qualification evidence.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func winnerMultipassQualificationPackingRequests(t *testing.T, largeInstrument bool) ([]winnerReviewBrokerRequest, *winnerMultipassQualificationPlan) {
	t.Helper()
	plan := winnerMultipassQualificationFixture(t)
	requests := []winnerReviewBrokerRequest{}
	for _, item := range plan.Cases {
		content, _ := json.Marshal(item.Bundle.Request)
		var request winnerReviewBrokerRequest
		if err := json.Unmarshal(content, &request); err != nil {
			t.Fatal(err)
		}
		if largeInstrument {
			request.WorkloadReview.InstrumentFiles[0].Content = strings.Repeat("// synthetic frozen instrument coverage line\n", 4500)
			request.WorkloadReview.InstrumentFiles[0].Sha256 = winnerMultipassSha256([]byte(request.WorkloadReview.InstrumentFiles[0].Content))
		}
		requests = append(requests, request)
	}
	requests[0].Operation = "qualification_cancel_before_provider"
	return requests, plan
}

// Different per-member partitions still cover identical complete artifacts;
// packing and exact-final passes each make only one model transition, not one
// per candidate. Every probe has its own exact rendered request identity.
func TestWinnerMultipassQualificationPackingBatchesMembersAcrossCompleteCorpus(t *testing.T) {
	requests, initial := winnerMultipassQualificationPackingRequests(t, true)
	settings := initial.Settings
	settings.SpanBytes = 32768
	members := []string{}
	positions := []int{}
	payloads := map[string]bool{}
	packing, err := packWinnerMultipassQualification(context.Background(), requests, initial.Owner, settings, initial.Release, func(_ context.Context, position int, plan *winnerMultipassPlan, call winnerMultipassCall, payload []byte) (winnerMultipassAdmission, error) {
		if position < 2 || position > 19 || call.Stage != "scan" || currentQgQualificationContainsExpectedLabels(payload) {
			t.Fatal("packing probed cancellation, synthesis, another case or private labels")
		}
		members = append(members, call.Member.Name)
		positions = append(positions, position)
		payloads[winnerMultipassSha256(payload)] = true
		return winnerMultipassAdmissionFixture(plan, call, payload, 20000*len(call.UnitIds)), nil
	})
	if err != nil || packing == nil || validateWinnerMultipassQualificationPacking(packing) != nil {
		t.Fatal("complete synthetic prospective group did not pack", err)
	}
	if len(members) != len(packing.Probes)+len(packing.FinalAdmissions) || len(payloads) == 0 || packing.Plan.GenerationCalls > settings.MaximumCalls {
		t.Fatal("packing lost probes or enlarged the model-call budget")
	}
	transitions := 0
	last := ""
	for index, member := range members {
		if member != last {
			transitions++
			if positions[index] != 2 {
				t.Fatal("member batch did not start at first generated control")
			}
			last = member
		} else if positions[index] < positions[index-1] {
			t.Fatal("prospective member batch reordered controls")
		}
	}
	if transitions != 4 || members[0] != "qwen" || members[len(packing.Probes)] != "qwen" {
		t.Fatal("group packing repeatedly swapped models across controls")
	}
	for _, item := range packing.Plan.Cases[1:] {
		if err := validateWinnerMultipassPlan(item.Plan, item.Bundle); err != nil {
			t.Fatal(err)
		}
		groups := map[string][][]string{}
		for _, group := range item.Plan.ScanGroups {
			groups[group.Member] = append(groups[group.Member], group.UnitIds)
		}
		if reflect.DeepEqual(groups["qwen"], groups["gemma"]) {
			t.Fatal("distinct native token capacities did not change prospective partitions")
		}
	}
	for _, mutate := range []func(*winnerMultipassQualificationPacking){
		func(value *winnerMultipassQualificationPacking) {
			value.FinalAdmissions = value.FinalAdmissions[:len(value.FinalAdmissions)-1]
		},
		func(value *winnerMultipassQualificationPacking) { value.FinalAdmissions[0].CasePosition = 3 },
		func(value *winnerMultipassQualificationPacking) {
			value.FinalAdmissions[0].Admission.RequestSha256 = strings.Repeat("0", 64)
		},
		func(value *winnerMultipassQualificationPacking) {
			value.Plan.Owner.Deadline = value.Plan.Owner.Deadline.Add(1)
		},
	} {
		content, _ := json.Marshal(packing)
		var changed winnerMultipassQualificationPacking
		if err := json.Unmarshal(content, &changed); err != nil {
			t.Fatal(err)
		}
		mutate(&changed)
		if validateWinnerMultipassQualificationPacking(&changed) == nil {
			t.Fatal("partial, cross-case, altered or re-owned final admissions accepted")
		}
	}
}

// A transport failure stops at its first occurrence and preserves that probe;
// no resizing, second member, final preflight or model draw is attempted.
func TestWinnerMultipassQualificationPackingRetainsFirstFailedProbeWithoutRetry(t *testing.T) {
	requests, initial := winnerMultipassQualificationPackingRequests(t, false)
	calls := 0
	packing, err := packWinnerMultipassQualification(context.Background(), requests, initial.Owner, initial.Settings, initial.Release, func(_ context.Context, _ int, plan *winnerMultipassPlan, call winnerMultipassCall, payload []byte) (winnerMultipassAdmission, error) {
		calls++
		return winnerMultipassAdmissionFixture(plan, call, payload, 100), errors.New("synthetic terminal tokenizer transport failure")
	})
	if err == nil || calls != 1 || packing == nil || len(packing.Probes) != 1 || len(packing.FinalAdmissions) != 0 || validateWinnerMultipassQualificationPacking(packing) == nil {
		t.Fatal("failed packing probe was retried, discarded or accepted as complete")
	}
}

// The exact final payload is recounted after hashing; a changed BPE count can
// still refuse the unstarted plan, never silently consume its reserved slack.
func TestWinnerMultipassQualificationPackingRefusesChangedFinalRenderedCount(t *testing.T) {
	requests, initial := winnerMultipassQualificationPackingRequests(t, false)
	calls := 0
	packing, err := packWinnerMultipassQualification(context.Background(), requests, initial.Owner, initial.Settings, initial.Release, func(_ context.Context, _ int, plan *winnerMultipassPlan, call winnerMultipassCall, payload []byte) (winnerMultipassAdmission, error) {
		calls++
		count := 100
		if calls == 37 {
			count = plan.Settings.TokenizationProfiles[call.Member.Name].MaximumContextTokens
		}
		return winnerMultipassAdmissionFixture(plan, call, payload, count), nil
	})
	if err == nil || packing == nil || calls != 37 || len(packing.Probes) != 36 || len(packing.FinalAdmissions) != 1 {
		t.Fatal("changed exact final token count was accepted, resized or retried")
	}
}
