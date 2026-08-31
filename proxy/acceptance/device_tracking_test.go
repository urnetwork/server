package acceptance

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sdk"
)

func TestHostedDeviceStateUsesProviderAliases(t *testing.T) {
	providerA := sdk.RequireIdFromBytes(bytes.Repeat([]byte{0x11}, 16))
	providerB := sdk.RequireIdFromBytes(bytes.Repeat([]byte{0x22}, 16))
	exits := sdk.NewExitList()
	exits.Add(&sdk.Exit{
		ClientId:                        providerA,
		FlowCount:                       1,
		Tier:                            0,
		EffectiveTier:                   1,
		Warning:                         true,
		WarningCause:                    "starved",
		Proven:                          true,
		ProviderBlockIngressPacketCount: 7,
		ProviderBlockEgressPacketCount:  3,
		ProviderDiagnosticsSequence:     9,
		ProviderBuildVersion:            "2026.8.29 test",
	})
	exits.Add(&sdk.Exit{ClientId: providerB})
	destinations := sdk.NewDestinationExitList()
	destinations.Add(&sdk.DestinationExit{
		DestinationIp: "65.49.70.82",
		ClientId:      providerA,
		FlowCount:     1,
	})

	summary := formatHostedDeviceState(
		&sdk.WindowStatus{TargetSize: 4, ProviderStateAdded: 4, MinSatisfied: true},
		&sdk.ReliabilityMetrics{FlowsOpened: 3, DialFailuresIntercepted: 1, FlowsReraced: 1},
		"out=2/200B in=3/300B last_in=05:00:00.000Z(+1/100B)",
		exits,
		destinations,
		map[string]string{},
	)
	for _, rawID := range []string{providerA.String(), providerB.String()} {
		if strings.Contains(summary, rawID) {
			t.Fatalf("provider id leaked into diagnostics: %s", summary)
		}
	}
	for _, expected := range []string{
		"65.49.70.82->p1(1)",
		"p1(flow=1 dial=0 tier=0/1 warn=true",
		"blocks=7/3 seq=9 build=2026.8.29_test",
		"flows=3",
		"dial=1 reraced=1",
		"last_in=05:00:00.000Z(+1/100B)",
	} {
		if !strings.Contains(summary, expected) {
			t.Fatalf("diagnostics %q do not contain %q", summary, expected)
		}
	}
}

func TestHostedDevicePacketStateRetainsLastRemoteIngress(t *testing.T) {
	state := &hostedDevicePacketState{}
	start := time.Date(2026, 8, 29, 6, 9, 38, 0, time.UTC)
	initial := state.summary(start, &sdk.PacketStats{
		RemoteEgressPacketCount:  10,
		RemoteEgressByteCount:    1000,
		RemoteIngressPacketCount: 12,
		RemoteIngressByteCount:   1200,
	})
	if !strings.Contains(initial, "last_in=none") {
		t.Fatalf("initial packet summary = %q", initial)
	}
	responseAt := start.Add(3 * time.Second)
	response := state.summary(responseAt, &sdk.PacketStats{
		RemoteEgressPacketCount:  13,
		RemoteEgressByteCount:    1300,
		RemoteIngressPacketCount: 14,
		RemoteIngressByteCount:   2650,
	})
	if !strings.Contains(response, "last_in=06:09:41.000Z(+2/1450B)") {
		t.Fatalf("response packet summary = %q", response)
	}
	quiet := state.summary(responseAt.Add(20*time.Second), &sdk.PacketStats{
		RemoteEgressPacketCount:  15,
		RemoteEgressByteCount:    1500,
		RemoteIngressPacketCount: 14,
		RemoteIngressByteCount:   2650,
	})
	if !strings.Contains(quiet, "last_in=06:09:41.000Z(+2/1450B)") {
		t.Fatalf("quiet packet summary erased the last ingress: %q", quiet)
	}
}

func TestHostedDeviceTrackerRetainsRecentTransitions(t *testing.T) {
	tracker := &sdkHostedDeviceTracker{}
	start := time.Date(2026, 8, 29, 5, 0, 0, 0, time.UTC)
	for i := 0; i < 70; i++ {
		tracker.record(start.Add(time.Duration(i)*time.Second), fmt.Sprintf("state=%02d", i))
	}
	tracker.record(start.Add(71*time.Second), "state=69")

	tracker.stateLock.Lock()
	if len(tracker.history) != hostedDeviceHistorySize {
		tracker.stateLock.Unlock()
		t.Fatalf("history length = %d, want %d", len(tracker.history), hostedDeviceHistorySize)
	}
	first := tracker.history[0]
	tracker.stateLock.Unlock()
	if !strings.Contains(first, "state=06") {
		t.Fatalf("history did not evict its oldest transitions: %s", first)
	}
	diagnostic := tracker.Diagnostic()
	if diagnostic != "05:01:09.000Z state=69" {
		t.Fatalf("diagnostic = %q, want latest transition", diagnostic)
	}
	tracker.stateLock.Lock()
	lastHistorySize := len(tracker.history)
	tracker.stateLock.Unlock()
	if lastHistorySize != hostedDeviceHistorySize {
		t.Fatalf("an unchanged poll was recorded twice: history length = %d", lastHistorySize)
	}
}

func TestHostedDeviceTrackerPrefersLastLiveFlow(t *testing.T) {
	tracker := &sdkHostedDeviceTracker{}
	start := time.Date(2026, 8, 29, 5, 0, 0, 0, time.UTC)
	tracker.record(start, "remote=connected destinations=[65.49.70.82->p1(1)] active=[p1(flow=1)]")
	tracker.record(start.Add(time.Second), "remote=connected destinations=[] active=[]")

	diagnostic := tracker.Diagnostic()
	if !strings.Contains(diagnostic, "65.49.70.82->p1(1)") || strings.Contains(diagnostic, "destinations=[]") {
		t.Fatalf("diagnostic did not preserve the last live flow: %s", diagnostic)
	}
}

func TestHostedDeviceDiagnosticRetainsAggregateStateBeforeTruncation(t *testing.T) {
	tracker := &sdkHostedDeviceTracker{}
	start := time.Date(2026, 8, 29, 5, 0, 0, 0, time.UTC)
	longProviderDetail := strings.Repeat("p1(flow=1 warn=false),", 40)
	tracker.record(start, fmt.Sprintf(
		"remote=connected window={9/11 min=true} reliability={flows=275 exit_loss=21 lost=16 recovery=3/1 pending=1} packets={out=1/40B in=2/80B last_in=05:00:00.000Z(+1/40B)} exits=11 destinations=[65.49.70.84->p1(1)] active=[%s]",
		longProviderDetail,
	))

	diagnostic := tracker.Diagnostic()
	if len(diagnostic) != hostedDeviceDiagnosticMaxSize+3 {
		t.Fatalf("truncated diagnostic length = %d, want %d", len(diagnostic), hostedDeviceDiagnosticMaxSize+3)
	}
	for _, expected := range []string{"window={9/11 min=true}", "flows=275", "exit_loss=21", "lost=16", "pending=1", "last_in=05:00:00.000Z"} {
		if !strings.Contains(diagnostic, expected) {
			t.Fatalf("truncated diagnostic %q does not contain aggregate %q", diagnostic, expected)
		}
	}
}

// The failed target can be one of several API LB addresses while a hosted
// device also carries control traffic. Its route and busiest exit must survive
// truncation, or the next live failure cannot be joined to provider state.
func TestHostedDeviceDiagnosticRetainsTargetRouteAndBusyExitBeforeTruncation(t *testing.T) {
	providerBusy := sdk.RequireIdFromBytes(bytes.Repeat([]byte{0x44}, 16))
	providerIdle := sdk.RequireIdFromBytes(bytes.Repeat([]byte{0x55}, 16))
	exits := sdk.NewExitList()
	exits.Add(&sdk.Exit{
		ClientId:             providerIdle,
		FlowCount:            1,
		ProviderBuildVersion: strings.Repeat("idle-build-", 16),
	})
	exits.Add(&sdk.Exit{
		ClientId:                        providerBusy,
		FlowCount:                       17,
		Warning:                         true,
		Quarantined:                     true,
		WarningCause:                    "no receive ack",
		Proven:                          true,
		ProviderBlockIngressPacketCount: 11,
		ProviderBlockEgressPacketCount:  13,
		ProviderDiagnosticsSequence:     19,
		ProviderBuildVersion:            "2026.8.30-main",
	})
	destinations := sdk.NewDestinationExitList()
	destinations.Add(&sdk.DestinationExit{DestinationIp: "208.67.222.222", ClientId: providerIdle, FlowCount: 1})
	destinations.Add(&sdk.DestinationExit{DestinationIp: "65.49.70.82", ClientId: providerBusy, FlowCount: 17})

	tracker := &sdkHostedDeviceTracker{}
	tracker.record(time.Date(2026, 8, 30, 23, 46, 34, 0, time.UTC), formatHostedDeviceState(
		&sdk.WindowStatus{TargetSize: 8, ProviderStateAdded: 7, MinSatisfied: true},
		&sdk.ReliabilityMetrics{FlowsOpened: 164, ExitLossEvents: 12, FlowsLostToExit: 16, RecoveryPending: 1},
		"out=3214/461805B in=4175/1053563B last_in=23:46:34.000Z(+7/280B)",
		exits,
		destinations,
		map[string]string{},
	))

	diagnostic := tracker.Diagnostic()
	for _, evidence := range []string{
		"65.49.70.82->p1(17)",
		"p1(flow=17",
		"warn=true quarantine=true",
		"cause=no_receive_ack",
		"blocks=11/13 seq=19 build=2026.8.30-main",
	} {
		if !strings.Contains(diagnostic, evidence) {
			t.Errorf("truncated diagnostic %q does not contain %q", diagnostic, evidence)
		}
	}
}

// Production packet totals and several active exits can fill the root result
// budget. The failed destination and its busy exit must precede that suffix;
// otherwise an established-flow loss is indistinguishable from a device-wide
// outage even though the tracker observed the exact provider join.
func TestHostedDeviceDiagnosticPrioritizesTargetRouteOverPacketSuffix(t *testing.T) {
	provider := sdk.RequireIdFromBytes(bytes.Repeat([]byte{0x66}, 16))
	exits := sdk.NewExitList()
	exits.Add(&sdk.Exit{
		ClientId:             provider,
		FlowCount:            353,
		Warning:              true,
		Quarantined:          true,
		WarningCause:         "provider return stopped for established flow",
		ProviderBuildVersion: "2026.8.30-1033129380",
	})
	for i := 0; i < 7; i++ {
		exits.Add(&sdk.Exit{
			ClientId:             sdk.RequireIdFromBytes(bytes.Repeat([]byte{byte(0x70 + i)}, 16)),
			FlowCount:            int32(100 - i),
			ProviderBuildVersion: strings.Repeat("secondary-build-", 4),
		})
	}
	destinations := sdk.NewDestinationExitList()
	destinations.Add(&sdk.DestinationExit{
		DestinationIp: "65.49.70.84",
		ClientId:      provider,
		FlowCount:     1,
	})

	tracker := &sdkHostedDeviceTracker{}
	tracker.record(time.Date(2026, 8, 31, 0, 49, 48, 0, time.UTC), formatHostedDeviceState(
		&sdk.WindowStatus{TargetSize: 8, ProviderStateAdded: 8, MinSatisfied: true, StallReason: "evaluating"},
		&sdk.ReliabilityMetrics{FlowsOpened: 353, ExitLossEvents: 37, FlowsLostToExit: 32, RecoveryCount: 2, RemovalsDeferred: 9},
		"out=7365/1089192B in=14605/2843528B last_in=00:49:46.981Z(+12/5974B)",
		exits,
		destinations,
		map[string]string{},
	))

	diagnostic := tracker.Diagnostic()
	for _, evidence := range []string{
		"65.49.70.84->p1(1)",
		"p1(flow=353",
		"cause=provider_return_stopped_for_established_flow",
	} {
		if !strings.Contains(diagnostic, evidence) {
			t.Errorf("bounded diagnostic %q does not contain %q", diagnostic, evidence)
		}
	}
}

type staticHostedDeviceTracker struct {
	diagnostic string
}

func (self *staticHostedDeviceTracker) Diagnostic() string {
	return self.diagnostic
}

func (self *staticHostedDeviceTracker) Close() {
}

func TestHostedDeviceDiagnosticsPreserveProbeError(t *testing.T) {
	probeError := errors.New("probe failed")
	wrapped := withHostedDeviceDiagnostics(probeError, &staticHostedDeviceTracker{diagnostic: "provider=p1"})
	if !errors.Is(wrapped, probeError) {
		t.Fatalf("wrapped error lost the probe cause: %v", wrapped)
	}
	if !strings.Contains(wrapped.Error(), "hosted device timeline: provider=p1") {
		t.Fatalf("wrapped error lost the device timeline: %v", wrapped)
	}
}
