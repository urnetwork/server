package acceptance

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sdk/v2026"
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
