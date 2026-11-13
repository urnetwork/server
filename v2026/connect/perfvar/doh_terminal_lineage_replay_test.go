//go:build acklineagetrace

package perfvar

import (
	"fmt"
	"os"
	"testing"
)

func dohTerminalLineageSelection(getenv func(string) string) error {
	for key, want := range map[string]string{
		"CONNECT_PERFVAR_DOH_TERMINAL_LINEAGE": "1", "CONNECT_PERFVAR_DOH_TERMINAL_FAILURE_SNAPSHOT": "1",
		"CONNECT_PERFVAR_DOH_WARM_FAILURE_SNAPSHOT": "1", "CONNECT_PERFVAR_DOH_TRANSFER_ARM": "ack-3-8",
		"CONNECT_PERFVAR_DOH_TRANSFER_PROFILE": "established-rtt3s", "CONNECT_PERFVAR_DOH_TRANSFER_RUN": "12",
		"URNETWORK_ACK_LINEAGE_REPLAY_SLOT": "granted", "CONNECT_PERFVAR_PROGRESS_TRACE": "0",
		"CONNECT_PERFVAR_DOH_WARM_LINEAGE": "", "CONNECT_PERFVAR_ACK_LINEAGE_REPLAY": "",
		"CONNECT_PERFVAR_H1_FAILURE_SNAPSHOT_REPLAY": "", "CONNECT_PERFVAR_TCP_RECOVERY_ARM": "",
	} {
		if getenv(key) != want {
			return fmt.Errorf("exact terminal lineage selector mismatch: %s", key)
		}
	}
	return nil
}

func dohTerminalLineageIdentity() error {
	scenario, err := dohTransferScenarioNamed("established-rtt3s")
	if err != nil {
		return err
	}
	trace, err := dohTransferTrace(scenario, 12)
	want := perfvarTrace{Version: 1, RunIndex: 12,
		IdentityHash:            "b7ada904b8faac6d4434c7d0b3794acd5df2e4d7841ad123aa4ceb1445e5a153",
		ApplicationOrDirectSeed: 696083066762885142, ProviderSeed: 5102027271804518564, InternalSeed: 5808160214881301211}
	if err != nil || trace != want || tcpRecoveryHash(scenario) != "6f78cd9ad3aba000dbd7a57ff94054840436fd9f9ee43e4e4a4954a969b21876" ||
		tcpRecoveryHash([]networkProfile{scenario.DeviceAccess, scenario.ProviderAccess}) != "026c4846e64bd349dd97cf5819edcd953c85fc1d5f253a6aef62bf0c688dbb30" {
		return fmt.Errorf("exact terminal failure identity changed: %v", err)
	}
	return nil
}

func installDohTerminalLineage() (func(), error) {
	if newPerfvarProgressTraceForTest != nil || dohTransferRetireForTest != nil {
		return nil, fmt.Errorf("progress observer factory already owned")
	}
	phase := &dohTerminalRetirementPhase{}
	dohTransferRetireForTest = phase.retire
	newPerfvarProgressTraceForTest = func() *perfvarProgressTrace { return newDohTerminalTCPTrace(phase) }
	return func() { newPerfvarProgressTraceForTest = nil; dohTransferRetireForTest = nil }, nil
}

func TestDohTerminalLineageReplay(t *testing.T) {
	if os.Getenv("CONNECT_PERFVAR_DOH_TERMINAL_LINEAGE") == "" {
		t.Skip("explicit exact terminal lineage selector required")
	}
	if err := dohTerminalLineageSelection(os.Getenv); err != nil {
		t.Fatal(err)
	}
	if err := dohTerminalLineageIdentity(); err != nil {
		t.Fatal(err)
	}
	restore, err := installDohTerminalLineage()
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	t.Logf("[doh-terminal-lineage] exact_component=established-rtt3s/ack-3-8/run12 capacity_per_role=%d per_packet_metadata=true payloads=false baseline_eligible=false", ackReplayCapacity)
	TestDohTransferRecoveryExperimentReplay(t)
}

func TestDohTerminalLineageSelectionAndIdentity(t *testing.T) {
	values := map[string]string{
		"CONNECT_PERFVAR_DOH_TERMINAL_LINEAGE": "1", "CONNECT_PERFVAR_DOH_TERMINAL_FAILURE_SNAPSHOT": "1",
		"CONNECT_PERFVAR_DOH_WARM_FAILURE_SNAPSHOT": "1", "CONNECT_PERFVAR_DOH_TRANSFER_ARM": "ack-3-8",
		"CONNECT_PERFVAR_DOH_TRANSFER_PROFILE": "established-rtt3s", "CONNECT_PERFVAR_DOH_TRANSFER_RUN": "12",
		"URNETWORK_ACK_LINEAGE_REPLAY_SLOT": "granted", "CONNECT_PERFVAR_PROGRESS_TRACE": "0",
		"CONNECT_PERFVAR_DOH_WARM_LINEAGE": "", "CONNECT_PERFVAR_ACK_LINEAGE_REPLAY": "",
		"CONNECT_PERFVAR_H1_FAILURE_SNAPSHOT_REPLAY": "", "CONNECT_PERFVAR_TCP_RECOVERY_ARM": "",
	}
	if err := dohTerminalLineageSelection(func(key string) string { return values[key] }); err != nil {
		t.Fatal(err)
	}
	if err := dohTerminalLineageIdentity(); err != nil {
		t.Fatal(err)
	}
	for key, old := range values {
		values[key] = "wrong"
		if dohTerminalLineageSelection(func(key string) string { return values[key] }) == nil {
			t.Fatalf("changed selector accepted: %s", key)
		}
		values[key] = old
	}
}

func TestDohTerminalLineageFactoryOwnershipAndCleanup(t *testing.T) {
	if newPerfvarProgressTraceForTest != nil {
		t.Fatal("factory already owned before test")
	}
	restore, err := installDohTerminalLineage()
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	if second, err := installDohTerminalLineage(); second != nil || err == nil {
		t.Fatal("factory owner overwritten")
	}
	if newPerfvarProgressTrace().observeForTest == nil {
		t.Fatal("bounded observer missing")
	}
	restore()
	if newPerfvarProgressTraceForTest != nil || dohTransferRetireForTest != nil {
		t.Fatal("factory survived teardown")
	}
}
