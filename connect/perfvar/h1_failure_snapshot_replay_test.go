//go:build acklineagetrace

package perfvar

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
)

// This diagnostic intercepts an existing essential, owner-terminal log. It
// installs no packet observer, history buffer, timer, or verbosity change.
// The format and typed arguments are checked against the real producer by a
// focused test; arbitrary logging arguments are never formatted or retained.
const h1FailureExitFormat = "[s]event=sequence_exit reason=%s client=%s destination=%s sequence=%s stream=%s ctx=%v parent_ctx=%v cause=%v pending=%d message=%s number=%d sends=%d lifetime_start_ns=%d write_started_ns=%d lifetime_ns=%d deadline_ns=%d observed_ns=%d selective=%t route_written=%t reliable=%t unreliable=%t carrier_changed=%t retained=%t head_set=%t head_number=%d head_received_ns=%d pending_head=%d pending_sacks=%d pending_contracts=%d\n"

const h1FailureStackCapacity = 2 * 1024 * 1024

type h1FailureExit struct {
	Client, Destination, Sequence, Stream, Message                              clientconnect.Id
	Pending, Sends, PendingSacks, PendingContracts                              int
	Number, HeadNumber, PendingHead                                             uint64
	LifetimeStartNS, WriteStartedNS, DeadlineNS, ObservedNS, HeadReceivedNS     int64
	Lifetime                                                                    time.Duration
	Selective, Written, Reliable, Unreliable, CarrierChanged, Retained, HeadSet bool
}

func h1FailureValue[T any](args []any, index int, destination *T) bool {
	value, ok := args[index].(T)
	if ok {
		*destination = value
	}
	return ok
}

// Return matched=false for unrelated exits; malformed=true only when the
// existing ACK-lifetime event cannot be decoded. No Stringer is invoked.
func decodeH1FailureExit(format string, args []any) (event h1FailureExit, matched, malformed bool) {
	if format != h1FailureExitFormat || len(args) == 0 || args[0] != "ack_lifetime" {
		return event, false, false
	}
	if len(args) != 29 {
		return event, false, true
	}
	var clientTag string
	if !h1FailureValue(args, 1, &clientTag) ||
		!h1FailureValue(args, 2, &event.Destination) || !h1FailureValue(args, 3, &event.Sequence) ||
		!h1FailureValue(args, 4, &event.Stream) || !h1FailureValue(args, 8, &event.Pending) ||
		!h1FailureValue(args, 9, &event.Message) || !h1FailureValue(args, 10, &event.Number) ||
		!h1FailureValue(args, 11, &event.Sends) || !h1FailureValue(args, 12, &event.LifetimeStartNS) ||
		!h1FailureValue(args, 13, &event.WriteStartedNS) || !h1FailureValue(args, 14, &event.Lifetime) ||
		!h1FailureValue(args, 15, &event.DeadlineNS) || !h1FailureValue(args, 16, &event.ObservedNS) ||
		!h1FailureValue(args, 17, &event.Selective) || !h1FailureValue(args, 18, &event.Written) ||
		!h1FailureValue(args, 19, &event.Reliable) || !h1FailureValue(args, 20, &event.Unreliable) ||
		!h1FailureValue(args, 21, &event.CarrierChanged) || !h1FailureValue(args, 22, &event.Retained) ||
		!h1FailureValue(args, 23, &event.HeadSet) || !h1FailureValue(args, 24, &event.HeadNumber) ||
		!h1FailureValue(args, 25, &event.HeadReceivedNS) || !h1FailureValue(args, 26, &event.PendingHead) ||
		!h1FailureValue(args, 27, &event.PendingSacks) || !h1FailureValue(args, 28, &event.PendingContracts) {
		return event, false, true
	}
	// Server control clients use c(id), not an ordinary client UUID. They and
	// canceled setup clients are never active workload evidence.
	var err error
	if event.Client, err = clientconnect.ParseId(clientTag); err != nil {
		return event, false, false
	}
	cause, ok := args[7].(error)
	if args[5] != nil || args[6] != nil || !ok || !errors.Is(cause, context.DeadlineExceeded) ||
		event.Lifetime != 30*time.Second || event.Pending <= 0 || event.Sends <= 0 ||
		!event.Written || !event.Reliable || event.Unreliable || event.Selective ||
		event.Message == (clientconnect.Id{}) || event.Sequence == (clientconnect.Id{}) ||
		event.LifetimeStartNS <= 0 || event.DeadlineNS-event.LifetimeStartNS != int64(event.Lifetime) ||
		event.ObservedNS < event.DeadlineNS {
		return event, false, false
	}
	return event, true, false
}

// Exits() may take the multi-client/window locks. Resolve flow identity only
// at construction and workload phase boundaries, never on the failing owner.
// A newest-generated client is explicitly insufficient to qualify evidence.
type h1FailureActivePin struct {
	client           *clientconnect.Client
	ctx              context.Context
	Client, Provider clientconnect.Id
	FlowCount        int
	Source, Phase    string
	PinnedNS         int64
}

type h1FailureEndpointSnapshot struct {
	Receive         clientconnect.ClientReceiveStatsSnapshot
	Recovery        clientconnect.ClientSendRecoveryStatsSnapshot
	PlatformReceive clientconnect.PlatformTransportReceiveStatsSnapshot
	H1              clientconnect.H1ConnectionStatsSnapshot
	Connected       bool
}

type h1FailureSnapshot struct {
	Event                    h1FailureExit
	Client, Provider         clientconnect.Id
	Phase, IdentitySource    string
	FlowCount                int
	PinnedNS                 int64
	StackBytes               int
	StackTruncated           bool
	TransportSearchTruncated bool
	Device, ProviderEndpoint h1FailureEndpointSnapshot
	TCP                      h1FailureTCPSnapshot
	stack                    []byte
}

type h1FailureSnapshotRecorder struct {
	path                                                                                                 atomic.Pointer[fullTunPath]
	pin                                                                                                  atomic.Pointer[h1FailureActivePin]
	ready                                                                                                atomic.Bool
	claimed                                                                                              atomic.Bool
	snapshot                                                                                             atomic.Pointer[h1FailureSnapshot]
	malformed, unattributed, rejected, duplicates                                                        atomic.Uint64
	deviceSettings, providerSettings, devicePlatforms, providerPlatforms, observerViolations, loadedPins atomic.Uint64
	deviceH1, providerH1                                                                                 clientconnect.H1ConnectionStats
	tcp                                                                                                  h1FailureTCPRecorder
	stack                                                                                                func([]byte, bool) int
}

func newH1FailureSnapshotRecorder() *h1FailureSnapshotRecorder {
	return &h1FailureSnapshotRecorder{stack: runtime.Stack}
}

func (self *h1FailureSnapshotRecorder) refreshPin(path *fullTunPath, phase string) {
	client, id, flows, source := activeH3FullTunDeviceClient(path)
	pin := &h1FailureActivePin{client: client, Client: id, Provider: path.providerClientId, FlowCount: flows, Source: source, Phase: phase, PinnedNS: time.Now().UnixNano()}
	if client != nil {
		pin.ctx = client.Ctx()
	}
	self.pin.Store(pin)
	if phase == "loaded-start" && source == "flow" && flows > 0 {
		self.loadedPins.Add(1)
	}
}

func (self *h1FailureSnapshotRecorder) phaseObserver(path *fullTunPath) *fullTunLatencyProbeTestObserver {
	self.refreshPin(path, "workload-start")
	// observe and finish remain nil: no per-probe/per-packet interception.
	return &fullTunLatencyProbeTestObserver{phase: func(phase string) { self.refreshPin(path, phase) }}
}

func h1FailureObserversNil(settings *clientconnect.ClientSettings) bool {
	return settings.SendBufferSettings.ProgressObserver == nil && settings.ReceiveBufferSettings.ProgressObserver == nil &&
		settings.StreamManagerSettings.StreamBufferSettings.P2pTransportSettings.ProgressObserver == nil
}

func (self *h1FailureSnapshotRecorder) configureClient(settings *clientconnect.ClientSettings, device bool) {
	if !h1FailureObserversNil(settings) {
		self.observerViolations.Add(1)
	}
	if device {
		self.deviceSettings.Add(1)
	} else {
		self.providerSettings.Add(1)
	}
	logger := settings.Log
	if logger == nil {
		logger = clientconnect.DefaultLogger()
	}
	settings.Log = &h1FailureSnapshotLogger{Logger: logger, recorder: self}
}

func (self *h1FailureSnapshotRecorder) hooks() *fullTunConstructionTestHooks {
	return &fullTunConstructionTestHooks{
		configureDeviceClientSettings:   func(settings *clientconnect.ClientSettings) { self.configureClient(settings, true) },
		configureProviderClientSettings: func(settings *clientconnect.ClientSettings) { self.configureClient(settings, false) },
		configureDevicePlatformSettings: func(settings *clientconnect.PlatformTransportSettings) {
			self.devicePlatforms.Add(1)
			if settings.ProgressObserver != nil {
				self.observerViolations.Add(1)
			}
			settings.H1ConnectionStats = &self.deviceH1
		},
		configureProviderPlatformSettings: func(settings *clientconnect.PlatformTransportSettings) {
			self.providerPlatforms.Add(1)
			if settings.ProgressObserver != nil {
				self.observerViolations.Add(1)
			}
			settings.H1ConnectionStats = &self.providerH1
		},
		afterStage: func(stage fullTunConstructionStage, path *fullTunPath) error {
			if stage == fullTunConstructionStageProviderCarrierTun {
				path.environment.clientDialObserverForTest.Store(&routeClientDialObserver{observe: func(node string, _ *clientconnect.Tun, connection net.Conn) {
					self.tcp.observeDial(node, connection, path.environment.h1Port, path.environment.providerH1Port)
				}})
			}
			if stage == fullTunConstructionStageRouteReady {
				self.path.Store(path)
				self.refreshPin(path, "route-ready")
				self.ready.Store(true)
			}
			return nil
		},
	}
}

type h1FailureSnapshotLogger struct {
	clientconnect.Logger
	recorder *h1FailureSnapshotRecorder
}

func (self *h1FailureSnapshotLogger) Infof(format string, args ...any) {
	self.recorder.observe(format, args)
	self.Logger.Infof(format, args...)
}

func (self *h1FailureSnapshotRecorder) observe(format string, args []any) {
	event, matched, malformed := decodeH1FailureExit(format, args)
	if malformed {
		self.malformed.Add(1)
	}
	if !matched {
		return
	}
	pin := self.pin.Load()
	if !self.ready.Load() || pin == nil || pin.ctx == nil || pin.FlowCount <= 0 || pin.Source != "flow" {
		self.unattributed.Add(1)
		return
	}
	if pin.Client != event.Client || pin.Provider != event.Destination || pin.ctx.Err() != nil {
		self.rejected.Add(1)
		return
	}
	if !self.claimed.CompareAndSwap(false, true) {
		self.duplicates.Add(1)
		return
	}
	// Allocate only after the exact owner-terminal event has been attributed.
	// Stack first, while that owner is still live and before callback teardown.
	snapshot := &h1FailureSnapshot{Event: event, Client: pin.Client, Provider: pin.Provider, Phase: pin.Phase, IdentitySource: pin.Source, FlowCount: pin.FlowCount, PinnedNS: pin.PinnedNS}
	stack := make([]byte, h1FailureStackCapacity)
	n := self.stack(stack, true)
	snapshot.StackTruncated = n >= len(stack) || n < 0
	n = min(max(n, 0), len(stack))
	snapshot.StackBytes, snapshot.stack = n, stack[:n]
	if pin.client != nil {
		snapshot.Device.Receive = pin.client.ReceiveStats()
		snapshot.Device.Recovery = pin.client.SendRecoveryStats()
	}
	if path := self.path.Load(); path != nil {
		if path.providerClient != nil {
			snapshot.ProviderEndpoint.Receive = path.providerClient.ReceiveStats()
			snapshot.ProviderEndpoint.Recovery = path.providerClient.SendRecoveryStats()
		}
		if path.devicePlatformReceiveStats != nil {
			snapshot.Device.PlatformReceive = path.devicePlatformReceiveStats.Snapshot()
		}
		if path.providerPlatformReceiveStats != nil {
			snapshot.ProviderEndpoint.PlatformReceive = path.providerPlatformReceiveStats.Snapshot()
		}
		if path.providerTransport != nil {
			snapshot.ProviderEndpoint.Connected = path.providerTransport.IsConnected()
		}
		// The immutable append-only owner permits a bounded, lock-free lookup.
		if path.deviceTransports != nil {
			node := path.deviceTransports.head.Load()
			for count := 0; node != nil && count < 64; count++ {
				if node.client == pin.client {
					if node.transport != nil {
						snapshot.Device.Connected = node.transport.IsConnected()
					}
					node = nil
					break
				}
				node = node.next
			}
			snapshot.TransportSearchTruncated = node != nil
		}
	}
	// These lifecycle-only collectors use a short metadata mutex; no packet
	// work, route/flow locks, or joins are performed while taking it.
	snapshot.Device.H1, snapshot.ProviderEndpoint.H1 = self.deviceH1.Snapshot(), self.providerH1.Snapshot()
	snapshot.TCP = self.tcp.snapshot(self.path.Load())
	self.snapshot.Store(snapshot)
}

func (self *h1FailureSnapshotRecorder) dump(t testing.TB, runIndex int) {
	t.Helper()
	snapshot := self.snapshot.Load()
	complete := !self.claimed.Load() || snapshot != nil && !snapshot.StackTruncated && !snapshot.TransportSearchTruncated && !snapshot.TCP.Incomplete
	t.Logf("[h1-failure-snapshot-summary] run_index=%d captured=%t complete=%t malformed=%d unattributed=%d rejected=%d duplicates=%d device_settings=%d provider_settings=%d device_platforms=%d provider_platforms=%d observer_violations=%d loaded_flow_pins=%d per_packet_observers=false verbose_v1=%t baseline_eligible=false", runIndex, snapshot != nil, complete, self.malformed.Load(), self.unattributed.Load(), self.rejected.Load(), self.duplicates.Load(), self.deviceSettings.Load(), self.providerSettings.Load(), self.devicePlatforms.Load(), self.providerPlatforms.Load(), self.observerViolations.Load(), self.loadedPins.Load(), clientconnect.DefaultLogger().V(1).Enabled())
	if snapshot != nil {
		metadata, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("[h1-failure-snapshot] run_index=%d %s", runIndex, metadata)
		t.Logf("[h1-failure-stack] run_index=%d bytes=%d capacity=%d truncated=%t\n%s\n[h1-failure-stack-end] run_index=%d", runIndex, snapshot.StackBytes, h1FailureStackCapacity, snapshot.StackTruncated, snapshot.stack, runIndex)
	}
	if !complete || self.malformed.Load() != 0 || self.unattributed.Load() != 0 || self.observerViolations.Load() != 0 ||
		self.deviceSettings.Load() == 0 || self.providerSettings.Load() != 1 || self.devicePlatforms.Load() == 0 || self.providerPlatforms.Load() != 1 || self.loadedPins.Load() == 0 {
		t.Error("H1 failure-only diagnostic is incomplete or did not arm at the loaded flow boundary")
	}
}

func TestH1FailureSnapshotRun1ThenRun2Replay(t *testing.T) {
	const selector = "CONNECT_PERFVAR_H1_FAILURE_SNAPSHOT_REPLAY"
	if os.Getenv(selector) == "" {
		t.Skip("explicit failure-only H1 selector required")
	}
	if os.Getenv(selector) != "h1-run1-run2" || os.Getenv("URNETWORK_ACK_LINEAGE_REPLAY_SLOT") != "granted" {
		t.Fatal("H1 snapshot requires the pinned pair and parent-owned host/source slot")
	}
	if runtime.GOMAXPROCS(0) != 8 || !ackLineageReplayRuntimeSupported(runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.GOMAXPROCS(0)) {
		t.Fatal("H1 snapshot runtime differs from canonical eight-core host")
	}
	if perfvarProgressTraceEnabled() || clientconnect.DefaultLogger().V(1).Enabled() || newPerfvarProgressTraceForTest != nil || newFullTunConstructionHooksForTest != nil || newFullTunLatencyProbeObserverForTest != nil {
		t.Fatal("H1 snapshot requires V0, nil per-packet observers, and unowned diagnostic hooks")
	}
	scenario := h1AckLineageRun1ThenRun2Scenario(t)
	defer func() { newFullTunConstructionHooksForTest = nil; newFullTunLatencyProbeObserverForTest = nil }()
	environment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	environment.Run(t, func(t testing.TB) {
		for _, runIndex := range []int{1, 2} {
			recorder := newH1FailureSnapshotRecorder()
			newFullTunConstructionHooksForTest, newFullTunLatencyProbeObserverForTest = recorder.hooks, recorder.phaseObserver
			ctx, cancel := context.WithTimeout(context.Background(), perfvarRunTimeout(scenario))
			record, err := measurePerfvarRun(ctx, t, scenario, runIndex)
			cancel()
			recorder.dump(t, runIndex)
			if err != nil {
				t.Fatal(err)
			}
			record.Host.MeasurementKind = "diagnostic-h1-failure-snapshot"
			emitPerfvarRecord(t, record)
			if !record.Correct {
				t.Errorf("H1 snapshot reproduced failure: run=%d %s", runIndex, record.FailureReason)
			}
			if record.InvalidReason != "" {
				t.Errorf("H1 snapshot retained invalid flag: %s", record.InvalidReason)
			}
		}
	})
}

func h1FailureTestArgs(client, provider clientconnect.Id) []any {
	start := int64(1_000_000_000)
	return []any{"ack_lifetime", client.String(), provider, clientconnect.NewId(), clientconnect.NewId(), nil, nil, context.DeadlineExceeded, 11, clientconnect.NewId(), uint64(24), 4, start, start, 30 * time.Second, start + int64(30*time.Second), start + int64(30*time.Second) + 1, false, true, true, false, false, false, true, uint64(23), start - 1, uint64(0), 0, 0}
}

func h1FailureTestRecorder() (*h1FailureSnapshotRecorder, []any) {
	client, provider := clientconnect.NewId(), clientconnect.NewId()
	recorder := newH1FailureSnapshotRecorder()
	recorder.ready.Store(true)
	recorder.pin.Store(&h1FailureActivePin{Client: client, Provider: provider, FlowCount: 2, Source: "flow", Phase: "loaded-start", ctx: context.Background()})
	recorder.stack = func(buffer []byte, all bool) int { return copy(buffer, "bounded stack\n") }
	return recorder, h1FailureTestArgs(client, provider)
}

func TestH1FailureSnapshotIdentityAndNegativeControls(t *testing.T) {
	for _, kind := range []string{"match", "wrong-client", "wrong-provider", "canceled-client", "canceled-sequence", "canceled-parent", "setup-60s", "server-control", "not-written", "not-reliable", "unreliable", "selective", "future-deadline", "no-flow", "newest-fallback", "not-ready", "unrelated", "malformed", "short"} {
		t.Run(kind, func(t *testing.T) {
			recorder, args := h1FailureTestRecorder()
			format := h1FailureExitFormat
			switch kind {
			case "wrong-client":
				args[1] = clientconnect.NewId().String()
			case "wrong-provider":
				args[2] = clientconnect.NewId()
			case "canceled-client":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				recorder.pin.Load().ctx = ctx
			case "canceled-sequence":
				args[5] = context.Canceled
			case "canceled-parent":
				args[6] = context.Canceled
			case "setup-60s":
				args[14] = 60 * time.Second
			case "server-control":
				args[1] = "c(" + args[1].(string) + ")"
			case "not-written":
				args[18] = false
			case "not-reliable":
				args[19] = false
			case "unreliable":
				args[20] = true
			case "selective":
				args[17] = true
			case "future-deadline":
				args[16] = int64(1)
			case "no-flow":
				recorder.pin.Load().FlowCount = 0
			case "newest-fallback":
				recorder.pin.Load().Source = "newest-fallback"
			case "not-ready":
				recorder.ready.Store(false)
			case "unrelated":
				format = "ordinary log %v"
			case "malformed":
				args[10] = 24
			case "short":
				args = args[:28]
			}
			recorder.observe(format, args)
			if (recorder.snapshot.Load() != nil) != (kind == "match") {
				t.Fatalf("captured=%t for %s", recorder.snapshot.Load() != nil, kind)
			}
		})
	}
}

func TestH1FailureSnapshotOneShotPublicationAndBounds(t *testing.T) {
	recorder, args := h1FailureTestRecorder()
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Uint64
	recorder.stack = func(buffer []byte, all bool) int {
		calls.Add(1)
		if !all || len(buffer) != h1FailureStackCapacity {
			t.Error("stack capture is not bounded/all-goroutine")
		}
		close(entered)
		<-release
		return copy(buffer, "first immutable stack")
	}
	var workers sync.WaitGroup
	workers.Go(func() { recorder.observe(h1FailureExitFormat, args) })
	<-entered
	if !recorder.claimed.Load() || recorder.snapshot.Load() != nil {
		t.Fatal("partial snapshot was published")
	}
	for range 8 {
		workers.Go(func() { recorder.observe(h1FailureExitFormat, args) })
	}
	close(release)
	workers.Wait()
	snapshot := recorder.snapshot.Load()
	if calls.Load() != 1 || recorder.duplicates.Load() != 8 || string(snapshot.stack) != "first immutable stack" || snapshot.StackTruncated {
		t.Fatal("one-shot ownership failed")
	}
	args[10] = uint64(100)
	recorder.observe(h1FailureExitFormat, args)
	if snapshot.Event.Number != 24 {
		t.Fatal("published snapshot mutated")
	}
	for _, size := range []int{-1, h1FailureStackCapacity, h1FailureStackCapacity + 1} {
		r, a := h1FailureTestRecorder()
		r.stack = func([]byte, bool) int { return size }
		r.observe(h1FailureExitFormat, a)
		if s := r.snapshot.Load(); !s.StackTruncated || len(s.stack) > h1FailureStackCapacity {
			t.Fatalf("truncation hidden: size=%d", size)
		}
	}
}

func TestH1FailureSnapshotConfigurationHasNoPacketObservers(t *testing.T) {
	recorder := newH1FailureSnapshotRecorder()
	before := clientconnect.DefaultLogger()
	settings := clientconnect.DefaultClientSettings()
	hooks := recorder.hooks()
	hooks.configureDeviceClientSettings(settings)
	if !h1FailureObserversNil(settings) || settings.Log == nil || clientconnect.DefaultLogger() != before || recorder.snapshot.Load() != nil {
		t.Fatal("diagnostic changed packet observers/global logger or eagerly captured")
	}
	platform := clientconnect.DefaultPlatformTransportSettings()
	hooks.configureDevicePlatformSettings(platform)
	if platform.ProgressObserver != nil || platform.H1ConnectionStats != &recorder.deviceH1 {
		t.Fatal("platform observer attached")
	}
	settings.SendBufferSettings.ProgressObserver = func(clientconnect.TransferProgressEvent) {}
	hooks.configureProviderClientSettings(settings)
	if recorder.observerViolations.Load() != 1 || settings.SendBufferSettings.ProgressObserver == nil {
		t.Fatal("existing observer was silently removed")
	}
}

func TestH1FailureSnapshotNonCandidateAllocations(t *testing.T) {
	recorder := newH1FailureSnapshotRecorder()
	logger := &h1FailureSnapshotLogger{Logger: clientconnect.NewNoopLogger(), recorder: recorder}
	args := []any{"ordinary"}
	if allocations := testing.AllocsPerRun(100, func() { logger.Infof("ordinary %s", args...) }); allocations != 0 {
		t.Fatalf("noncandidate logger allocates: %g", allocations)
	}
	if recorder.claimed.Load() || recorder.snapshot.Load() != nil {
		t.Fatal("noncandidate allocated history")
	}
}

func TestH1FailureSnapshotRejectsUnformattedArguments(t *testing.T) {
	recorder, args := h1FailureTestRecorder()
	args = slices.Clone(args)
	args[1] = h1FailurePanicStringer{}
	recorder.observe(h1FailureExitFormat, args)
	if recorder.malformed.Load() != 1 || recorder.claimed.Load() {
		t.Fatal("malformed client was not rejected")
	}
}

type h1FailurePanicStringer struct{}

func (h1FailurePanicStringer) String() string { panic("diagnostic must not format log arguments") }

// Exercise the actual 30-second terminal owner with a reliable H1 route whose
// peer never ACKs. Virtual time makes this deterministic; it is a trigger
// contract test, not evidence that injected loss caused the historical bug.
func TestH1FailureSnapshotRealAckLifetimeProducer(t *testing.T) {
	// Keep the process-global pool maintenance worker outside the time bubble.
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		recorder := newH1FailureSnapshotRecorder()
		recorder.stack = func(buffer []byte, all bool) int { return copy(buffer, "real owner frontier") }
		settings := clientconnect.DefaultClientSettings()
		settings.Log = &h1FailureSnapshotLogger{Logger: clientconnect.NewNoopLogger(), recorder: recorder}
		settings.ControlPingTimeout = 0
		settings.EncryptionSettings.Mode = clientconnect.EncryptionModeOff
		settings.SendBufferSettings.AckTimeout = 30 * time.Second
		settings.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Now().Add(time.Hour)
		client := clientconnect.NewClient(ctx, clientconnect.NewId(), clientconnect.NewNoContractClientOob(), settings)
		provider := clientconnect.NewId()
		client.ContractManager().AddNoContractPeer(provider)
		client.ContractManager().AddNoContractPeer(clientconnect.ControlId)
		route := make(clientconnect.Route, 128)
		client.RouteManager().UpdateTransport(clientconnect.NewSendGatewayTransportWithType(clientconnect.TransportTypeH1), []clientconnect.Route{route})
		defer func() {
			cancel()
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			for len(route) > 0 {
				clientconnect.MessagePoolReturn(<-route)
			}
		}()
		recorder.ready.Store(true)
		recorder.pin.Store(&h1FailureActivePin{client: client, ctx: client.Ctx(), Client: client.ClientId(), Provider: provider, Source: "flow", Phase: "loaded-start", FlowCount: 1})
		frame, err := clientconnect.ToFrame(&protocol.SimpleMessage{Content: "trigger-contract"}, clientconnect.DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		ack := make(chan error, 1)
		accepted, err := client.SendWithTimeoutDetailed(frame, provider, func(err error) { ack <- err }, time.Second)
		if !accepted || err != nil {
			clientconnect.MessagePoolReturn(frame.MessageBytes)
			t.Fatalf("send admission: accepted=%t err=%v", accepted, err)
		}
		synctest.Wait()
		if len(route) == 0 {
			t.Fatal("reliable route never owned the physical write")
		}
		time.Sleep(31 * time.Second)
		synctest.Wait()
		snapshot := recorder.snapshot.Load()
		if snapshot == nil || snapshot.Event.Client != client.ClientId() || snapshot.Event.Destination != provider || snapshot.Event.Lifetime != 30*time.Second || snapshot.Event.Sends == 0 || client.Ctx().Err() != nil {
			t.Fatalf("real active owner failed to trigger: snapshot=%+v malformed=%d rejected=%d unattributed=%d", snapshot, recorder.malformed.Load(), recorder.rejected.Load(), recorder.unattributed.Load())
		}
		select {
		case err := <-ack:
			if err == nil {
				t.Fatal("missing ACK reported success")
			}
		default:
			t.Fatal("terminal owner did not finish its callback")
		}
	})
}
