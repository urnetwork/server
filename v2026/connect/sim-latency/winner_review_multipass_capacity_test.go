// Capacity tests are pure synthetic accounting, never disk reservations or a
// claim that the host has enough space for a real candidate's complete review.
package main

import "testing"

// Every future invocation and original retained copy contributes to the bound.
func TestWinnerMultipassCapacityCountsExactPlanAndAdditionalTokenizerCalls(t *testing.T) {
	bundle, plan, _ := winnerMultipassFixture(t)
	settings := defaultWinnerMultipassCapacitySettings()
	capacity, err := estimateWinnerMultipassCapacity(plan, bundle, len(plan.Calls), settings)
	if err != nil {
		t.Fatal(err)
	}
	if capacity.GenerationCalls != len(plan.Calls) || capacity.TokenizerInvocations != len(plan.Calls) || capacity.RequiredDurableBytes <= capacity.WrapperOriginalBytes || capacity.RequiredMemoryBytes <= capacity.CoreEvidenceBytes {
		t.Fatal("capacity receipt omitted originals or staging/memory copies")
	}
	extra, err := estimateWinnerMultipassCapacity(plan, bundle, len(plan.Calls)+1, settings)
	if err != nil || extra.RequiredDurableBytes <= capacity.RequiredDurableBytes {
		t.Fatal("extra prospective tokenizer attempt was not reserved")
	}
	settings.ProtectedCopies++
	copied, err := estimateWinnerMultipassCapacity(plan, bundle, len(plan.Calls), settings)
	if err != nil || copied.RequiredDurableBytes <= capacity.RequiredDurableBytes {
		t.Fatal("protected originals were treated as free storage")
	}
}

// Refusal returns the measured bound so an operator can inspect the real gap.
func TestWinnerMultipassCapacityRejectsInsufficientDiskBeforeClaim(t *testing.T) {
	bundle, plan, _ := winnerMultipassFixture(t)
	settings := defaultWinnerMultipassCapacitySettings()
	settings.MaximumDurableBytes = 1
	capacity, err := estimateWinnerMultipassCapacity(plan, bundle, len(plan.Calls), settings)
	if err == nil || capacity == nil || capacity.RequiredDurableBytes <= settings.MaximumDurableBytes {
		t.Fatal("unreserved original transcripts could begin")
	}
}

// A generous disk budget cannot conceal native accumulated-evidence memory.
func TestWinnerMultipassCapacityRejectsInsufficientMemoryBeforeClaim(t *testing.T) {
	bundle, plan, _ := winnerMultipassFixture(t)
	settings := defaultWinnerMultipassCapacitySettings()
	settings.MaximumMemoryBytes = 1
	capacity, err := estimateWinnerMultipassCapacity(plan, bundle, len(plan.Calls), settings)
	if err == nil || capacity == nil || capacity.RequiredMemoryBytes <= settings.MaximumMemoryBytes {
		t.Fatal("native memory bound was ignored")
	}
	settings = defaultWinnerMultipassCapacitySettings()
	settings.MaximumMemoryBytes++
	if _, err := estimateWinnerMultipassCapacity(plan, bundle, len(plan.Calls), settings); err == nil {
		t.Fatal("native eight-GiB ceiling was raised")
	}
}

// Missing tokenizer or original-copy budgets cannot serve as a cheap preflight.
func TestWinnerMultipassCapacityRejectsIncompleteInvocationOrCopyCensus(t *testing.T) {
	bundle, plan, _ := winnerMultipassFixture(t)
	settings := defaultWinnerMultipassCapacitySettings()
	if _, err := estimateWinnerMultipassCapacity(plan, bundle, len(plan.Calls)-1, settings); err == nil {
		t.Fatal("unbudgeted generation token admission accepted")
	}
	settings.ProtectedCopies = 0
	if _, err := estimateWinnerMultipassCapacity(plan, bundle, len(plan.Calls), settings); err == nil {
		t.Fatal("protected original custody omitted")
	}
}

// The same fixed names and bounds feed parent custody and containment validation.
func TestWinnerMultipassCapacityJournalCensusMatchesOperations(t *testing.T) {
	settings := defaultWinnerMultipassSettings()
	token, err := winnerMultipassJournalLeafLimits("tokenize", settings)
	if err != nil || len(token) != 4 || token["tokenize.response.json"] != 8*1024*1024 || token["transport.response"] != 0 {
		t.Fatal("tokenizer original census changed")
	}
	generation, err := winnerMultipassJournalLeafLimits("generate", settings)
	if err != nil || len(generation) != 3 || generation["transport.response"] != 16*1024*1024 || generation["call.complete.json"] != 4*1024*1024 {
		t.Fatal("generation original census changed")
	}
	if _, err := winnerMultipassJournalLeafLimits("retry", settings); err == nil {
		t.Fatal("unplanned operation gained custody authority")
	}
}

// Releasing planned operation allowances from a real fallocate reservation does
// not double-count the reservation or consume the protected-document residual.
func TestWinnerMultipassInvocationAllowancesSumToAggregateReservation(t *testing.T) {
	bundle, plan, _ := winnerMultipassFixture(t)
	settings := defaultWinnerMultipassCapacitySettings()
	capacity, err := estimateWinnerMultipassCapacity(plan, bundle, len(plan.Calls), settings)
	if err != nil {
		t.Fatal(err)
	}
	total := int64(0)
	for _, call := range plan.Calls {
		requestBytes := plan.Settings.MaximumRequestBytes
		if call.Stage == "scan" {
			payload, err := buildWinnerMultipassCall(plan, bundle, call.Id, map[string]winnerMultipassCallEvidence{})
			if err != nil {
				t.Fatal(err)
			}
			requestBytes = len(payload)
		}
		for _, operation := range []string{"tokenize", "generate"} {
			allowance, err := winnerMultipassInvocationCapacity(plan, call, operation, requestBytes, settings)
			if err != nil {
				t.Fatal(err)
			}
			total += allowance
		}
	}
	if total != capacity.InvocationAllowanceBytes || total+capacity.DocumentAndTemporaryBytes != capacity.RequiredDurableBytes || capacity.DocumentAndTemporaryBytes <= 0 {
		t.Fatal("operation/residual capacity formula diverged")
	}
	call := plan.Calls[0]
	call.Id = "synthetic-unplanned"
	if _, err := winnerMultipassInvocationCapacity(plan, call, "generate", 1, settings); err == nil {
		t.Fatal("unplanned operation consumed reservation authority")
	}
}
