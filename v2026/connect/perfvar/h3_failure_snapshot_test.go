package perfvar

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
)

type h3FailureEndpointSnapshot struct {
	ClientId       clientconnect.Id
	IdentitySource string
	FlowsAtPin     int
	Recovery       clientconnect.ClientSendRecoveryStatsSnapshot
	Receive        clientconnect.ClientReceiveStatsSnapshot
}

// A failure record is taken synchronously before fixture cleanup. It reads
// existing atomic counters at one failure boundary; no success-path packet
// observer, extra worker, polling or diagnostic history is installed.
type h3CorrectnessFailureSnapshot struct {
	CapturedAt                 time.Time
	FixtureContextError        string
	Failure                    string
	Profile                    string
	ExpectedBytesPerDirection  int64
	PayloadBytesSincePairStart int64
	OriginalDevice             h3FailureEndpointSnapshot
	CurrentDevice              h3FailureEndpointSnapshot
	DeviceReplaced             bool
	Provider                   h3FailureEndpointSnapshot
	Pair                       perfvarCorrectnessTCPPair
}

func h3FailureDeviceEndpoint(snapshot h3FullTunClientSnapshot) h3FailureEndpointSnapshot {
	return h3FailureEndpointSnapshot{
		ClientId: snapshot.deviceClientId, IdentitySource: snapshot.deviceSource,
		FlowsAtPin: snapshot.deviceClientFlow,
		Recovery:   snapshot.deviceRecovery, Receive: snapshot.deviceReceive,
	}
}

func snapshotH3CorrectnessFailure(
	path *fullTunPath,
	start h3FullTunClientSnapshot,
	progressStart int64,
	profile string,
	expectedBytes int64,
	pair perfvarCorrectnessTCPPair,
	measureErr error,
) *h3CorrectnessFailureSnapshot {
	if measureErr == nil || path == nil || path.route != fullTunRouteExchangeH3 {
		return nil
	}
	current := snapshotH3FullTunClients(path)
	original := snapshotH3FullTunOriginalDevice(start, current)
	snapshot := &h3CorrectnessFailureSnapshot{
		CapturedAt: time.Now(), Failure: measureErr.Error(), Profile: profile,
		ExpectedBytesPerDirection:  expectedBytes,
		PayloadBytesSincePairStart: path.workloadProgressBytes.Load() - progressStart,
		OriginalDevice:             h3FailureDeviceEndpoint(original), CurrentDevice: h3FailureDeviceEndpoint(current),
		DeviceReplaced: original.deviceClient != current.deviceClient,
		Provider: h3FailureEndpointSnapshot{
			ClientId: path.providerClientId, IdentitySource: "fixture-provider",
			Recovery: current.providerRecovery, Receive: current.providerReceive,
		},
		Pair: pair,
	}
	if err := path.ctx.Err(); err != nil {
		snapshot.FixtureContextError = err.Error()
	}
	return snapshot
}

func logH3CorrectnessFailure(
	t testing.TB,
	path *fullTunPath,
	start h3FullTunClientSnapshot,
	progressStart int64,
	profile string,
	expectedBytes int64,
	pair perfvarCorrectnessTCPPair,
	measureErr error,
) {
	t.Helper()
	snapshot := snapshotH3CorrectnessFailure(path, start, progressStart, profile, expectedBytes, pair, measureErr)
	if snapshot == nil {
		return
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Logf("[h3-correctness-failure-before-close] encode error=%v", err)
		return
	}
	t.Logf("[h3-correctness-failure-before-close] %s", encoded)
}

// Counter reads on zero-value Clients need no network workers. Deliberately
// stale baseline/replacement counters prove the helper reads the retained
// original owner again, while its identity remains the measurement's pin.
func TestH3DiagnosticOriginalClientIdentitySurvivesReplacement(t *testing.T) {
	original, replacement := &clientconnect.Client{}, &clientconnect.Client{}
	start := h3FullTunClientSnapshot{
		deviceClient: original, deviceClientId: clientconnect.NewId(), deviceClientFlow: 3, deviceSource: "flow",
		deviceRecovery: clientconnect.ClientSendRecoveryStatsSnapshot{InitialWriteCount: 10},
		deviceReceive:  clientconnect.ClientReceiveStatsSnapshot{AckRouteWriteCount: 20},
	}
	current := h3FullTunClientSnapshot{
		deviceClient: replacement, deviceClientId: clientconnect.NewId(), deviceClientFlow: 0, deviceSource: "newest-fallback",
		deviceRecovery:   clientconnect.ClientSendRecoveryStatsSnapshot{InitialWriteCount: 30},
		deviceReceive:    clientconnect.ClientReceiveStatsSnapshot{AckRouteWriteCount: 40},
		providerRecovery: clientconnect.ClientSendRecoveryStatsSnapshot{InitialWriteCount: 50},
		providerReceive:  clientconnect.ClientReceiveStatsSnapshot{AckRouteWriteCount: 60},
	}
	got := snapshotH3FullTunOriginalDevice(start, current)
	if got.deviceClient != original || got.deviceClientId != start.deviceClientId ||
		got.deviceClientFlow != start.deviceClientFlow || got.deviceSource != start.deviceSource {
		t.Fatal("original counters were attributed to the replacement generation")
	}
	if !reflect.DeepEqual(got.deviceRecovery, original.SendRecoveryStats()) || !reflect.DeepEqual(got.deviceReceive, original.ReceiveStats()) {
		t.Fatal("original endpoint reused the baseline or replacement counters")
	}
	if !reflect.DeepEqual(got.providerRecovery, current.providerRecovery) || !reflect.DeepEqual(got.providerReceive, current.providerReceive) ||
		current.deviceClient != replacement || current.deviceRecovery.InitialWriteCount != 30 {
		t.Fatal("pinning the original corrupted the provider or replacement snapshot")
	}
	if got := snapshotH3FullTunOriginalDevice(h3FullTunClientSnapshot{}, current); !reflect.DeepEqual(got, current) {
		t.Fatal("missing original pin invented a device generation")
	}
}

// A TCP read timeout alone cannot say whether application bytes or only the
// returned hash stalled. Preserve delivered bytes, the completed directional
// result and physical counters before cancellation changes live state.
func TestH3DiagnosticFailureSnapshotPreservesPreTeardownEvidence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	original, replacement := &clientconnect.Client{}, &clientconnect.Client{}
	originalId, providerId := clientconnect.NewId(), clientconnect.NewId()
	pin := h3FullTunClientSnapshot{deviceClient: original, deviceClientId: originalId, deviceClientFlow: 1, deviceSource: "flow"}
	current := &atomic.Pointer[clientconnect.Client]{}
	current.Store(replacement)
	path := &fullTunPath{ctx: ctx, route: fullTunRouteExchangeH3, deviceClient: current, providerClientId: providerId}
	path.workloadProgressBytes.Store(108)
	pair := perfvarCorrectnessTCPPair{Upload: perfvarCorrectnessObservation{
		Result:  workloadResult{UsefulByteCount: 100, ContentHash: "completed-upload"},
		Carrier: perfvarCarrierObservation{WireByteCount: 131, Duration: time.Second},
	}}
	failure := errors.New("upload: synthetic TCP read timeout")
	got := snapshotH3CorrectnessFailure(path, pin, 8, "loss-200bp", 100, pair, failure)
	cancel()
	path.workloadProgressBytes.Store(1000)
	current.Store(original)
	if got == nil || got.CapturedAt.IsZero() || got.FixtureContextError != "" || got.Failure != failure.Error() ||
		got.Profile != "loss-200bp" || got.ExpectedBytesPerDirection != 100 || got.PayloadBytesSincePairStart != 100 ||
		!got.DeviceReplaced || got.OriginalDevice.ClientId != originalId || got.OriginalDevice.IdentitySource != "flow" ||
		got.CurrentDevice.IdentitySource != "newest-fallback" || got.Provider.ClientId != providerId || !reflect.DeepEqual(got.Pair, pair) {
		t.Fatalf("failure lost evidence from before cleanup: %+v", got)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var decoded h3CorrectnessFailureSnapshot
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.PayloadBytesSincePairStart != 100 || decoded.OriginalDevice.ClientId != originalId {
		t.Fatalf("failure evidence did not survive serialization: %v %+v", err, decoded)
	}
	if got := snapshotH3CorrectnessFailure(path, pin, 8, "loss-200bp", 100, pair, nil); got != nil {
		t.Fatal("successful case produced a failure snapshot")
	}
	path.route = fullTunRouteExchangeH1
	if got := snapshotH3CorrectnessFailure(path, pin, 8, "loss-200bp", 100, pair, failure); got != nil {
		t.Fatal("H3-only diagnostic captured another carrier")
	}
}
