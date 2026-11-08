// Spool arithmetic is synthetic accounting, not remote capacity observation or
// permission to remove an original before protected custody has completed.
package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestWinnerMultipassSpoolCapacityPreservesEntireAggregateAndMemory(t *testing.T) {
	bundle, plan, _ := winnerMultipassFixture(t)
	aggregateSettings := defaultWinnerMultipassCapacitySettings()
	aggregateSettings.MaximumDurableBytes = 128 * 1024 * 1024 * 1024
	spoolSettings := winnerMultipassSpoolSettings{LifecycleVersion: winnerMultipassSpoolLifecycle, MaximumLocalPeakBytes: 8 * 1024 * 1024 * 1024, MaximumProtectedBytes: 128 * 1024 * 1024 * 1024}
	original, err := estimateWinnerMultipassCapacity(plan, bundle, len(plan.Calls), aggregateSettings)
	if err != nil {
		t.Fatal(err)
	}
	originalBytes, _ := json.Marshal(original)
	spool, aggregate, err := estimateWinnerMultipassSpoolCapacity(plan, bundle, len(plan.Calls), aggregateSettings, spoolSettings)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(aggregate, original) || spool.AggregateCapacitySha256 != winnerMultipassSha256(originalBytes) || spool.FullLocalAggregateBytes != original.RequiredDurableBytes || spool.RequiredMemoryBytes != original.RequiredMemoryBytes ||
		spool.Invocations != 2*len(plan.Calls) || spool.MaximumConcurrentInvocations != 1 || spool.ProtectedBytes <= original.ProtectedCopyBytes || spool.PermanentDocumentsBytes != original.DocumentAndTemporaryBytes ||
		spool.RequiredLocalPeakBytes != spool.PermanentDocumentsBytes+spool.CompactManifestBytes+spool.ActiveInvocationBytes+spool.RecoveryWorkspaceBytes || spool.RecoveryWorkspaceBytes <= spool.ActiveInvocationBytes {
		t.Fatal("spool relabeled aggregate custody, reduced memory, or omitted serial recovery/staging")
	}
	spoolSettings.MaximumLocalPeakBytes = spool.RequiredLocalPeakBytes - 1
	if _, _, err := estimateWinnerMultipassSpoolCapacity(plan, bundle, len(plan.Calls), aggregateSettings, spoolSettings); err == nil {
		t.Fatal("unreserved local spool accepted")
	}
	spoolSettings.MaximumLocalPeakBytes = 8 * 1024 * 1024 * 1024
	spoolSettings.MaximumProtectedBytes = spool.ProtectedBytes - 1
	if _, _, err := estimateWinnerMultipassSpoolCapacity(plan, bundle, len(plan.Calls), aggregateSettings, spoolSettings); err == nil {
		t.Fatal("unbudgeted protected originals accepted")
	}
	spoolSettings.MaximumProtectedBytes = 128 * 1024 * 1024 * 1024
	spoolSettings.LifecycleVersion = "synthetic-unqualified-cleanup"
	if _, _, err := estimateWinnerMultipassSpoolCapacity(plan, bundle, len(plan.Calls), aggregateSettings, spoolSettings); err == nil {
		t.Fatal("unknown deletion lifecycle accepted")
	}
}

func TestWinnerMultipassQualificationSpoolCountsAllControlsCancellationAndProtectedRecords(t *testing.T) {
	plan := winnerMultipassQualificationFixture(t)
	aggregateSettings := defaultWinnerMultipassCapacitySettings()
	aggregateSettings.MaximumDurableBytes = 128 * 1024 * 1024 * 1024
	spoolSettings := winnerMultipassSpoolSettings{LifecycleVersion: winnerMultipassSpoolLifecycle, MaximumLocalPeakBytes: 8 * 1024 * 1024 * 1024, MaximumProtectedBytes: 128 * 1024 * 1024 * 1024}
	spool, aggregate, err := estimateWinnerMultipassQualificationSpoolCapacity(plan, aggregateSettings, spoolSettings)
	if err != nil {
		t.Fatal(err)
	}
	canonical, _ := json.Marshal(aggregate)
	if spool.AggregateCapacitySha256 != winnerMultipassSha256(canonical) || len(aggregate.Cases) != 18 || spool.Invocations != len(plan.Invocations) || spool.CancellationInvocations != 1 || spool.GenerationCalls != 72 || spool.TokenizerInvocations != 108 ||
		spool.RequiredMemoryBytes != aggregate.RequiredMemoryBytes || spool.FullLocalAggregateBytes != aggregate.RequiredDurableBytes || spool.RequiredLocalPeakBytes >= spool.FullLocalAggregateBytes || spool.ProtectedBytes < int64(spool.Invocations)*winnerMultipassSpoolCaptureBytes {
		t.Fatal("qualification spool cut controls, transcripts or memory instead of bounding local residency")
	}
	t.Logf("Synthetic only: full-local=%d local-peak=%d protected=%d unchanged-memory=%d", spool.FullLocalAggregateBytes, spool.RequiredLocalPeakBytes, spool.ProtectedBytes, spool.RequiredMemoryBytes)
	aggregateSettings.MaximumMemoryBytes = 1
	if _, _, err := estimateWinnerMultipassQualificationSpoolCapacity(plan, aggregateSettings, spoolSettings); err == nil {
		t.Fatal("spool concealed accumulated native memory")
	}
}
