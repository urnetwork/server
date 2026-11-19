// All qualification controls share one serialized protected spool, never
// eighteen independent local reservations or a reduced evidence census.
package main

import "encoding/json"

func estimateWinnerMultipassQualificationSpoolCapacity(plan *winnerMultipassQualificationPlan, aggregateSettings winnerMultipassCapacitySettings, spoolSettings winnerMultipassSpoolSettings) (*winnerMultipassSpoolCapacity, *winnerMultipassQualificationCapacity, error) {
	aggregate, err := estimateWinnerMultipassQualificationCapacity(plan, aggregateSettings)
	if err != nil {
		return nil, aggregate, err
	}
	encoded, err := json.Marshal(aggregate)
	if err != nil {
		return nil, aggregate, err
	}
	planBytes, err := json.Marshal(plan)
	if err != nil {
		return nil, aggregate, err
	}
	result := &winnerMultipassSpoolCapacity{Settings: spoolSettings, AggregateCapacitySha256: winnerMultipassSha256(encoded), PlanSha256: aggregate.PlanSha256,
		GenerationCalls: plan.GenerationCalls, TokenizerInvocations: plan.TokenizerInvocations, CancellationInvocations: 1, Invocations: len(plan.Invocations), ProtectedCopies: aggregateSettings.ProtectedCopies,
		FullLocalAggregateBytes: aggregate.RequiredDurableBytes, PermanentDocumentsBytes: aggregate.RequiredDurableBytes + 2*int64(len(planBytes)), RequiredMemoryBytes: aggregate.RequiredMemoryBytes}
	for _, item := range plan.Cases[1:] {
		capacity := aggregate.Cases[item.Position]
		result.PermanentDocumentsBytes -= capacity.InvocationAllowanceBytes
		result.ProtectedBytes += capacity.ProtectedCopyBytes
		for _, call := range item.Plan.Calls {
			payloadBytes := item.Plan.Settings.MaximumRequestBytes
			if call.Stage == "scan" {
				payload, err := buildWinnerMultipassCall(item.Plan, item.Bundle, call.Id, map[string]winnerMultipassCallEvidence{})
				if err != nil {
					return result, aggregate, err
				}
				payloadBytes = len(payload)
			}
			for _, operation := range []string{"tokenize", "generate"} {
				peak, err := winnerMultipassSpoolInvocationCapacity(item.Plan, call, operation, payloadBytes, aggregateSettings)
				if err != nil {
					return result, aggregate, err
				}
				result.ActiveInvocationBytes = max(result.ActiveInvocationBytes, peak)
			}
		}
	}
	limits := defaultWinnerMultipassProviderLimits()
	cancelOriginal := int64(limits.MaximumEnvelopeBytes+limits.MaximumResultBytes) + 32*int64(limits.MaximumMetadataBytes)
	result.ProtectedBytes += int64(aggregateSettings.ProtectedCopies) * (cancelOriginal + int64(len(planBytes)))
	result.ActiveInvocationBytes = max(result.ActiveInvocationBytes, int64(2+aggregateSettings.TemporaryCopies)*cancelOriginal+winnerMultipassSpoolCaptureBytes+winnerMultipassSpoolManifestBytes)
	return result, aggregate, finishWinnerMultipassSpoolCapacity(result)
}
