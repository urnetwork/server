//go:build acklineagetrace

package perfvar

import (
	"fmt"
	"os"
	"testing"

	clientconnect "github.com/urnetwork/connect"
)

// This one-cell wrapper changes only opt-in diagnostic observation. It cannot
// select a candidate arm, change the production-shaped fixture, or qualify a
// timing/memory baseline. The stopped cohort is not resumed by this replay.
func dohWarmLineageSelection(getenv func(string) string) error {
	for key, want := range map[string]string{
		"CONNECT_PERFVAR_DOH_WARM_LINEAGE":           "1",
		"CONNECT_PERFVAR_DOH_WARM_FAILURE_SNAPSHOT":  "1",
		"CONNECT_PERFVAR_DOH_TRANSFER_ARM":           "ack-8-8",
		"CONNECT_PERFVAR_DOH_TRANSFER_PROFILE":       "established-rtt3s",
		"CONNECT_PERFVAR_DOH_TRANSFER_RUN":           "2",
		"URNETWORK_ACK_LINEAGE_REPLAY_SLOT":          "granted",
		"CONNECT_PERFVAR_PROGRESS_TRACE":             "0",
		"CONNECT_PERFVAR_ACK_LINEAGE_REPLAY":         "",
		"CONNECT_PERFVAR_H1_FAILURE_SNAPSHOT_REPLAY": "",
		"CONNECT_PERFVAR_TCP_RECOVERY_ARM":           "",
	} {
		if getenv(key) != want {
			return fmt.Errorf("DoH warmup lineage selector mismatch: %s", key)
		}
	}
	return nil
}

func dohWarmLineageIdentity() error {
	scenario, err := dohTransferScenarioNamed("established-rtt3s")
	if err != nil {
		return err
	}
	trace, err := dohTransferTrace(scenario, 2)
	want := perfvarTrace{Version: 1, RunIndex: 2,
		IdentityHash:            "53bd1340aad695294aa5919cf43fb0c4629d305cc68b4994577183780005fa8a",
		ApplicationOrDirectSeed: 9184385345430742930, ProviderSeed: 4941575313349692791, InternalSeed: 7337279986857032589}
	if err != nil || trace != want || tcpRecoveryHash(scenario) != "6f78cd9ad3aba000dbd7a57ff94054840436fd9f9ee43e4e4a4954a969b21876" ||
		tcpRecoveryHash([]networkProfile{scenario.DeviceAccess, scenario.ProviderAccess}) != "026c4846e64bd349dd97cf5819edcd953c85fc1d5f253a6aef62bf0c688dbb30" {
		return fmt.Errorf("exact failed DoH warmup scenario/profile/trace changed: %v", err)
	}
	return nil
}

func installDohWarmLineage() (func(), error) {
	if newPerfvarProgressTraceForTest != nil {
		return nil, fmt.Errorf("progress observer factory already owned")
	}
	newPerfvarProgressTraceForTest = func() *perfvarProgressTrace { return newH1AckReplayTrace(2) }
	return func() { newPerfvarProgressTraceForTest = nil }, nil
}

func TestDohWarmLineageReplay(t *testing.T) {
	if os.Getenv("CONNECT_PERFVAR_DOH_WARM_LINEAGE") == "" {
		t.Skip("explicit exact warmup lineage selector required")
	}
	if err := dohWarmLineageSelection(os.Getenv); err != nil {
		t.Fatal(err)
	}
	if err := dohWarmLineageIdentity(); err != nil {
		t.Fatal(err)
	}
	restore, err := installDohWarmLineage()
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	t.Logf("[doh-warm-lineage] exact_control=established-rtt3s/ack-8-8/run2 capacity_per_role=%d first_events=true per_packet_metadata=true payloads=false baseline_eligible=false", ackReplayCapacity)
	// Existing fixture still enforces CPU8/non-race/V0 and emits exactly one
	// full correctness record. Its endpoint joins dump the bounded recorders.
	TestDohTransferRecoveryExperimentReplay(t)
}

func TestDohWarmLineageSelectionAndIdentity(t *testing.T) {
	values := map[string]string{
		"CONNECT_PERFVAR_DOH_WARM_LINEAGE": "1", "CONNECT_PERFVAR_DOH_WARM_FAILURE_SNAPSHOT": "1",
		"CONNECT_PERFVAR_DOH_TRANSFER_ARM": "ack-8-8", "CONNECT_PERFVAR_DOH_TRANSFER_PROFILE": "established-rtt3s",
		"CONNECT_PERFVAR_DOH_TRANSFER_RUN": "2", "URNETWORK_ACK_LINEAGE_REPLAY_SLOT": "granted",
		"CONNECT_PERFVAR_PROGRESS_TRACE": "0", "CONNECT_PERFVAR_ACK_LINEAGE_REPLAY": "",
		"CONNECT_PERFVAR_H1_FAILURE_SNAPSHOT_REPLAY": "", "CONNECT_PERFVAR_TCP_RECOVERY_ARM": "",
	}
	if err := dohWarmLineageSelection(func(key string) string { return values[key] }); err != nil {
		t.Fatal(err)
	}
	if err := dohWarmLineageIdentity(); err != nil {
		t.Fatal(err)
	}
	for key, original := range values {
		values[key] = "wrong"
		if dohWarmLineageSelection(func(key string) string { return values[key] }) == nil {
			t.Fatalf("unowned/changed selector accepted: %s", key)
		}
		values[key] = original
	}
}

func TestDohWarmLineageFactoryOwnershipAndCleanup(t *testing.T) {
	if newPerfvarProgressTraceForTest != nil {
		t.Fatal("test starts with an owned factory")
	}
	restore, err := installDohWarmLineage()
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	if rejected, err := installDohWarmLineage(); err == nil || rejected != nil {
		t.Fatal("another owner overwrote the factory")
	}
	trace := newPerfvarProgressTrace()
	settings := clientconnect.DefaultClientSettings()
	platform := clientconnect.DefaultPlatformTransportSettings()
	trace.configure(settings)
	trace.configurePlatform(platform)
	if settings.SendBufferSettings.ProgressObserver == nil || settings.ReceiveBufferSettings.ProgressObserver == nil || platform.ProgressObserver == nil {
		t.Fatal("lineage does not cover logical and physical boundaries")
	}
	restore()
	if newPerfvarProgressTraceForTest != nil {
		t.Fatal("lineage factory survived teardown")
	}
}
