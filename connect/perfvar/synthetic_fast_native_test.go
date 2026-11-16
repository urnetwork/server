// Three fresh native Fast iterations each retain five ten-second windows.
// Public SDK32/64 consumers and Desktop128 providers must fully join each time.
package perfvar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
)

// The first cold measurement remains in the same five-window arithmetic mean.
func TestFullTunH1SyntheticFastSdk32(t *testing.T) {
	runSyntheticFastNative(t, 32*1024*1024)
}

// The default Android64 window retains all ten quality and two speed clients.
func TestFullTunH1SyntheticFastSdk64(t *testing.T) {
	runSyntheticFastNative(t, 64*1024*1024)
}

// These are public observations of actual SDK/native owners. Other cumulative
// sequence fields are unavailable, rather than evidence of zero network loss.
type syntheticFastNativeBoundary struct {
	NativeOffered               uint64                         `json:"native_offered"`
	NativeAccepted              uint64                         `json:"native_accepted"`
	NativeRefused               uint64                         `json:"native_refused"`
	NativeCompleted             uint64                         `json:"native_completed"`
	ReceiveOfferedPackets       uint64                         `json:"receive_tun_offered_packets"`
	ReceiveOfferedBytes         uint64                         `json:"receive_tun_offered_bytes"`
	ReceiveAcceptedBytes        uint64                         `json:"receive_tun_accepted_bytes"`
	PendingLogicalObserved      bool                           `json:"pending_logical_observed"`
	PendingLogicalEgressPackets int64                          `json:"pending_logical_egress_packets"`
	PendingLogicalEgressBytes   int64                          `json:"pending_logical_egress_bytes"`
	Consumer                    *sdk.DeviceLocalMemoryUsage    `json:"consumer"`
	ConsumerPackets             *sdk.PacketStats               `json:"consumer_packet_stats"`
	Providers                   []*sdk.DeviceLocalMemoryUsage  `json:"providers"`
	DefaultWindowReady          bool                           `json:"default_window_ready"`
	Windows                     syntheticFastWindowObservation `json:"windows"`
}

// Unknown egress is unresolved logical attribution, not a physical carrier or
// live queue depth. Its reassignable counts are reported separately.
type syntheticFastPendingAttribution struct {
	Observed      bool  `json:"observed"`
	BeforePackets int64 `json:"before_packets"`
	AfterPackets  int64 `json:"after_packets"`
	DeltaPackets  int64 `json:"delta_packets"`
	BeforeBytes   int64 `json:"before_bytes"`
	AfterBytes    int64 `json:"after_bytes"`
	DeltaBytes    int64 `json:"delta_bytes"`
}

// The relay-only route policy independently excludes alternate paths. Known
// carrier observations must grow in both H1 directions with no other movement;
// unresolved logical egress is not evidence of a physical non-H1 send.
func syntheticFastH1WindowError(before, after *sdk.PacketStats) error {
	type carrierBytes struct{ ingress, egress int64 }
	read := func(stats *sdk.PacketStats) (map[sdk.TransportType]carrierBytes, error) {
		if stats == nil || stats.TransportStats == nil {
			return nil, errors.New("synthetic Fast carrier attribution is unavailable")
		}
		carriers := make(map[sdk.TransportType]carrierBytes)
		var ingress, egress int64
		for index := 0; index < stats.TransportStats.Len(); index++ {
			row := stats.TransportStats.Get(index)
			if row == nil || row.Stats == nil || row.Stats.RemoteIngressByteCount < 0 || row.Stats.RemoteEgressByteCount < 0 {
				return nil, errors.New("synthetic Fast carrier attribution is invalid")
			}
			if _, exists := carriers[row.TransportType]; exists {
				return nil, errors.New("synthetic Fast carrier attribution is duplicated")
			}
			carriers[row.TransportType] = carrierBytes{ingress: row.Stats.RemoteIngressByteCount, egress: row.Stats.RemoteEgressByteCount}
			ingress += row.Stats.RemoteIngressByteCount
			egress += row.Stats.RemoteEgressByteCount
		}
		_, h1Exists := carriers[sdk.TransportTypeH1]
		_, pendingExists := carriers[sdk.TransportTypeUnknown]
		if !h1Exists || !pendingExists || ingress != stats.RemoteIngressByteCount || egress != stats.RemoteEgressByteCount {
			return nil, errors.New("synthetic Fast carrier byte partition is incomplete")
		}
		return carriers, nil
	}
	beforeCarriers, beforeErr := read(before)
	afterCarriers, afterErr := read(after)
	if err := errors.Join(beforeErr, afterErr); err != nil {
		return err
	}
	if after.RemoteIngressByteCount < before.RemoteIngressByteCount || after.RemoteEgressByteCount < before.RemoteEgressByteCount {
		return errors.New("synthetic Fast aggregate traffic counters reset")
	}
	beforeH1, afterH1 := beforeCarriers[sdk.TransportTypeH1], afterCarriers[sdk.TransportTypeH1]
	if afterH1.ingress <= beforeH1.ingress || afterH1.egress <= beforeH1.egress {
		return fmt.Errorf("synthetic Fast lacks H1 traffic in both directions: ingress_delta=%d egress_delta=%d", afterH1.ingress-beforeH1.ingress, afterH1.egress-beforeH1.egress)
	}
	for transport, value := range beforeCarriers {
		current, exists := afterCarriers[transport]
		if !exists {
			return fmt.Errorf("synthetic Fast lost carrier attribution for %s", transport)
		}
		if transport == sdk.TransportTypeUnknown {
			if current.ingress != value.ingress {
				return errors.New("synthetic Fast unknown physical ingress changed")
			}
			continue
		}
		if transport != sdk.TransportTypeH1 && current != value {
			return fmt.Errorf("synthetic Fast non-H1 attribution changed: carrier=%s ingress_delta=%d egress_delta=%d", transport, current.ingress-value.ingress, current.egress-value.egress)
		}
	}
	for transport, value := range afterCarriers {
		if _, exists := beforeCarriers[transport]; !exists && (value.ingress != 0 || value.egress != 0) {
			return fmt.Errorf("synthetic Fast acquired non-H1 carrier attribution for %s", transport)
		}
	}
	return nil
}

// Per-iteration evidence retains the original five-window payload and cleanup
// receipts. No full report survives the callback into the next fixture.
type syntheticFastNativeIterationReport struct {
	Schema                                        string `json:"schema"`
	Scope                                         string `json:"scope"`
	ConsumerTarget                                int64  `json:"consumer_target_bytes"`
	ProviderTarget                                int64  `json:"provider_target_bytes"`
	ProviderCount                                 int    `json:"provider_count"`
	Access, Provider                              networkProfile
	Sequence                                      syntheticFastSequenceReport       `json:"sequence"`
	Mean                                          syntheticFastMeanReport           `json:"mean"`
	PendingLogicalEgress                          []syntheticFastPendingAttribution `json:"pending_logical_egress"`
	WorkerFinalJoined                             [5]bool                           `json:"worker_final_joined"`
	ClientAfterJoin                               *sdk.DeviceLocalMemoryUsage       `json:"client_after_join"`
	ProviderAfterJoin                             []*sdk.DeviceLocalMemoryUsage     `json:"provider_after_join"`
	OriginHandlersStarted, OriginHandlersFinished int64
	NativeFinalOffered                            uint64                      `json:"native_final_offered_packets"`
	NativeFinalCompleted                          uint64                      `json:"native_final_completed_packets"`
	NativeFinalAccepted                           uint64                      `json:"native_final_accepted_packets"`
	NativeFinalRefused                            uint64                      `json:"native_final_refused_packets"`
	NativeFinalInvalid                            bool                        `json:"native_final_invalid"`
	ReceiveFinalOfferedPackets                    uint64                      `json:"receive_final_offered_packets"`
	ReceiveFinalOfferedBytes                      uint64                      `json:"receive_final_offered_bytes"`
	ReceiveFinalAcceptedBytes                     uint64                      `json:"receive_final_accepted_bytes"`
	ReceiveFinalInvalid                           bool                        `json:"receive_final_invalid"`
	Valid                                         bool                        `json:"valid"`
	Error                                         string                      `json:"error,omitempty"`
	OwnedDnsBootstrap                             syntheticFastOwnedDohReport `json:"owned_dns_bootstrap"`
	Iteration                                     int                         `json:"iteration"`
	OwnersJoined                                  bool                        `json:"owners_joined"`
	EnvironmentClosed                             bool                        `json:"environment_closed"`
	EvidenceValid                                 bool                        `json:"evidence_valid"`
	MeetsMinimum                                  bool                        `json:"meets_minimum"`
}

// Three sequential original fixtures each close before the next TestEnv starts.
// The test harness invokes these internally sampled performance roots once.
func runSyntheticFastNative(t *testing.T, target int64) {
	if testing.Short() {
		t.Skip("full native synthetic Fast fixture requires TestEnv services")
	}
	var median syntheticFastMedianReport
	defer func() {
		median.ConsumerTarget = target
		if err := logSyntheticFastReport(t, "synthetic-fast-median", median); err != nil {
			t.Error(err)
		}
	}()
	err := collectSyntheticFastMedian(&median, func(index int) (syntheticFastIterationSample, error) {
		return measureSyntheticFastNativeEnvironment(t, target, index)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Only completed immutable values cross from the TestEnv callback. A failed
// teardown may exit the test without returning; it cannot race this publisher.
type syntheticFastNativeIterationResult struct {
	Report syntheticFastNativeIterationReport
	Err    error
}

// Deferred publication retains a completed failed record even when TestEnv
// calls FailNow. An abandoned callback supplies no invented mean or join proof.
func measureSyntheticFastNativeEnvironment(t *testing.T, target int64, index int) (sample syntheticFastIterationSample, resultErr error) {
	results := make(chan syntheticFastNativeIterationResult, 1)
	environmentClosed := false
	resultErr = errors.New("synthetic Fast TestEnv iteration did not return")
	defer func() {
		var report syntheticFastNativeIterationReport
		select {
		case result := <-results:
			report, resultErr = result.Report, result.Err
		default:
		}
		if !environmentClosed {
			resultErr = errors.Join(resultErr, errors.New("synthetic Fast TestEnv cleanup did not return"))
		}
		report.Schema, report.Iteration, report.ConsumerTarget = "perfvar-public-native-fast-five-iteration-v3", index, target
		report.EnvironmentClosed = environmentClosed
		_, numericOnly := resultErr.(*syntheticFastMeanMinimumError)
		report.EvidenceValid = (resultErr == nil || numericOnly) && report.OwnersJoined && report.EnvironmentClosed
		report.MeetsMinimum = report.EvidenceValid && report.Mean.Valid
		report.Valid = report.MeetsMinimum
		if resultErr != nil {
			report.Error = resultErr.Error()
		}
		if err := logSyntheticFastReport(t, "synthetic-fast", report); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
		sample = syntheticFastIterationSample{Mean: report.Mean, OwnersJoined: report.OwnersJoined, EnvironmentClosed: report.EnvironmentClosed}
	}()
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		report, err := measureSyntheticFastNativeIteration(t, target)
		results <- syntheticFastNativeIterationResult{Report: report, Err: err}
	})
	// Run has joined/reset its own namespace before a later callback can start.
	environmentClosed = true
	return
}

// Both iteration records and the compact aggregate retain an independent cap.
func logSyntheticFastReport(t testing.TB, label string, report any) error {
	encoded, err := json.Marshal(report)
	if err != nil {
		return err
	}
	if len(encoded) > 2*1024*1024 {
		return errors.New("synthetic Fast bounded report exceeded2MiB")
	}
	t.Logf("[perfvar] %s %s", label, encoded)
	return nil
}

// Complete SDK footprints are sampled after genuine close/join, before a later
// iteration can construct a consumer or provider. Missing ledgers are invalid.
func syntheticFastNativeIterationJoinError(report syntheticFastNativeIterationReport) error {
	expectedProviders := 0
	switch report.ConsumerTarget {
	case 32 * 1024 * 1024:
		expectedProviders = 6
	case 64 * 1024 * 1024:
		expectedProviders = 12
	default:
		return errors.New("synthetic Fast iteration has an unknown consumer target")
	}
	if report.ProviderTarget != 128*1024*1024 || report.ProviderCount != expectedProviders || len(report.ProviderAfterJoin) != expectedProviders {
		return errors.New("synthetic Fast iteration lost an original provider after join")
	}
	for _, joined := range report.WorkerFinalJoined {
		if !joined {
			return errors.New("synthetic Fast iteration workload owner did not join")
		}
	}
	if report.OriginHandlersStarted == 0 || report.OriginHandlersStarted != report.OriginHandlersFinished {
		return errors.New("synthetic Fast iteration origin handlers did not join")
	}
	usages := append([]*sdk.DeviceLocalMemoryUsage{report.ClientAfterJoin}, report.ProviderAfterJoin...)
	for _, usage := range usages {
		if usage == nil || usage.TotalByteCount != 0 || usage.TransferRootUsedByteCount != 0 ||
			usage.TransferRootReservedByteCount != usage.TransferRootReleasedByteCount || usage.PlatformTransportUsedByteCount != 0 ||
			usage.PlatformTransportUsedCount != 0 || usage.PlatformTransportPendingH1Count != 0 || usage.PlatformTransportPendingHandoffCount != 0 ||
			usage.PlatformTransportActiveHandoffCount != 0 || usage.PlatformTransportReservedBytes != usage.PlatformTransportReleasedBytes {
			return errors.New("synthetic Fast iteration public SDK ownership remained after join")
		}
	}
	return nil
}

// Four owned origins preserve the original retained five-window workload. All
// closes are explicit and deferred, including partial construction and failure.
func measureSyntheticFastNativeIteration(t testing.TB, target int64) (report syntheticFastNativeIterationReport, resultErr error) {
	settings := defaultSyntheticFastSettings()
	settings.LoadedLatency = true
	profile := initialNetworkProfiles(7919)["clean-lan"]
	profile.Name = "synthetic-fast-access"
	profile.Forward = newLinkProfile(settings.OfferedBitsPerSecond, settings.AccessRoundTrip/2, 0, settings.LossProbability, 20*time.Millisecond)
	profile.Forward.QueueByteCount = min(2*1024*1024, profile.Forward.QueueByteCount)
	profile.Reverse = profile.Forward
	providerProfile := initialNetworkProfiles(7927)["clean-lan"]
	providerProfile.Forward = newLinkProfile(settings.OfferedBitsPerSecond, 0, 0, 0, 20*time.Millisecond)
	providerProfile.Forward.QueueByteCount = min(2*1024*1024, providerProfile.Forward.QueueByteCount)
	providerProfile.Reverse = providerProfile.Forward
	fixture, err := newSyntheticFastNativeFixtureWithCleanup(t, target, profile, providerProfile, nil)
	if err != nil {
		return report, err
	}
	defer func() {
		if err := fixture.close(); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	sequenceBound := syntheticFastSequenceBound(settings, 5)
	ctx, cancel := context.WithTimeout(fixture.ctx, sequenceBound)
	defer cancel()
	origin := &syntheticFastOrigin{settings: settings, ctx: ctx, providerForRequestForTest: fixture.providerForRequest}
	inner := nativeBrowsingRunSettings{}
	inner.originServerTlsForTest, inner.originClientTlsForTest, err = nativeBrowsingTlsConfigs()
	if err != nil {
		return report, err
	}
	var servers [4]*http.Server
	var listeners [4]net.Listener
	var serverWorkers sync.WaitGroup
	var transport *http.Transport
	var closeOriginsOnce sync.Once
	var originJoinErr error
	closeOrigins := func() error {
		closeOriginsOnce.Do(func() {
			cancel()
			if transport != nil {
				transport.CloseIdleConnections()
			}
			joinCtx, stopJoin := context.WithTimeout(context.Background(), settings.FixtureDrainTimeout)
			defer stopJoin()
			for _, server := range servers {
				if server != nil {
					originJoinErr = errors.Join(originJoinErr, server.Shutdown(joinCtx), server.Close())
				}
			}
			for _, listener := range listeners {
				if listener != nil {
					_ = listener.Close()
				}
			}
			joined := make(chan struct{})
			go func() { serverWorkers.Wait(); close(joined) }()
			select {
			case <-joined:
			case <-joinCtx.Done():
				originJoinErr = errors.Join(originJoinErr, errors.New("synthetic Fast origin server workers did not join"))
			}
			for index := range origin.runs {
				if owner := origin.runs[index].Load(); owner != nil {
					originJoinErr = errors.Join(originJoinErr, owner.sealAndWait(joinCtx))
				}
			}
			if origin.started.Load() != origin.finished.Load() {
				originJoinErr = errors.Join(originJoinErr, errors.New("synthetic Fast origin handlers did not join"))
			}
			fixture.stateLock.Lock()
			fixture.originPorts = [4]int{}
			fixture.stateLock.Unlock()
		})
		return originJoinErr
	}
	var workerOwners [5]*syntheticFastWorkerOwner
	var workerJoined [5]bool
	var drainOnce sync.Once
	var workerJoinErr error
	drainWorkers := func() error {
		drainOnce.Do(func() {
			joinCtx, stopJoin := context.WithTimeout(context.Background(), settings.FixtureDrainTimeout)
			defer stopJoin()
			for index, owner := range workerOwners {
				if owner != nil {
					err := owner.join(joinCtx)
					workerJoined[index] = err == nil
					workerJoinErr = errors.Join(workerJoinErr, err)
				}
			}
		})
		return workerJoinErr
	}
	defer func() {
		if err := errors.Join(closeOrigins(), fixture.close(), drainWorkers()); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	for index := range listeners {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return report, err
		}
		listeners[index] = listener
		fixture.stateLock.Lock()
		fixture.originPorts[index] = listener.Addr().(*net.TCPAddr).Port
		fixture.stateLock.Unlock()
		httpServer, serveListener := newNativeBrowsingInnerServer(origin, listener, inner)
		syntheticFastOriginOwnConnContext(httpServer)
		servers[index] = httpServer
		serverWorkers.Add(1)
		go func() { defer serverWorkers.Done(); _ = httpServer.Serve(serveListener) }()
	}
	transport = newNativeBrowsingInnerTransport(func(dialCtx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || port != "443" {
			return nil, errors.New("synthetic Fast application requested an unowned origin")
		}
		for index := range listeners {
			if host == fmt.Sprintf("origin%d.test", index) {
				return syntheticFastObserveOwnedDial(dialCtx, func() (net.Conn, error) {
					return fixture.appTun.DialContext(dialCtx, network, net.JoinHostPort(net.IPv4(203, 0, 113, byte(10+index)).String(), "443"))
				})
			}
		}
		return nil, errors.New("synthetic Fast application origin name is unowned")
	}, inner)
	syntheticFastConfigureHttpPool(transport, settings)
	transport.DisableCompression, transport.ResponseHeaderTimeout = true, settings.HeaderTimeout
	client := &http.Client{Transport: transport}
	capture := func() (syntheticFastCumulative, any) {
		windows := fixture.windowObservation()
		boundary := syntheticFastNativeBoundary{NativeOffered: fixture.bridgeStarted.Load(), NativeAccepted: fixture.bridgeAccepted.Load(),
			NativeRefused: fixture.bridgeRefused.Load(), NativeCompleted: fixture.bridgeCompleted.Load(),
			ReceiveOfferedPackets: fixture.receiveOfferedPackets.Load(), ReceiveOfferedBytes: fixture.receiveOfferedBytes.Load(), ReceiveAcceptedBytes: fixture.receiveAcceptedBytes.Load(),
			Consumer: fixture.device.MemoryUsed(), ConsumerPackets: fixture.device.GetPacketStats(), Providers: make([]*sdk.DeviceLocalMemoryUsage, len(fixture.providers)), DefaultWindowReady: windows.Ready, Windows: windows}
		if boundary.ConsumerPackets != nil && boundary.ConsumerPackets.TransportStats != nil {
			for index := 0; index < boundary.ConsumerPackets.TransportStats.Len(); index++ {
				row := boundary.ConsumerPackets.TransportStats.Get(index)
				if row != nil && row.TransportType == sdk.TransportTypeUnknown && row.Stats != nil {
					boundary.PendingLogicalObserved = true
					boundary.PendingLogicalEgressPackets = row.Stats.RemoteEgressPacketCount
					boundary.PendingLogicalEgressBytes = row.Stats.RemoteEgressByteCount
				}
			}
		}
		for index := range fixture.providers {
			boundary.Providers[index] = fixture.providers[index].device.MemoryUsed()
		}
		return syntheticFastCumulative{Offered: boundary.NativeOffered, Accepted: boundary.NativeAccepted, Refused: boundary.NativeRefused}, boundary
	}
	started := time.Now()
	sequence, workloadErr := measureSyntheticFastSequence(ctx, settings, 5, fmt.Sprintf("public-sdk%d-desktop128-retained", target/(1024*1024)),
		func(runCtx context.Context, ordinal int) (syntheticFastReport, error) {
			owner, err := syntheticFastCarrierBeginOriginRun(origin, runCtx, ordinal, false, nil)
			if err != nil {
				return syntheticFastReport{}, err
			}
			if !fixture.windowReady() {
				return syntheticFastReport{}, errors.New("synthetic Fast default window policy was not live before measurement")
			}
			report, err := measureSyntheticFast(runCtx, client, func(stream int) string {
				return fmt.Sprintf("https://origin%d.test/bulk/%d?run=%d", stream%4, stream, ordinal)
			}, settings, nil, owner)
			workerOwners[ordinal] = report.workerOwner
			if !fixture.windowReady() {
				err = errors.Join(err, errors.New("synthetic Fast default window policy was not live after measurement"))
			}
			return report, errors.Join(err, syntheticFastLoadedProbeError(report))
		}, func(joinCtx context.Context, ordinal int) error {
			owner := origin.runs[ordinal].Load()
			if owner == nil {
				return errors.New("synthetic Fast retained request owner is missing")
			}
			return owner.sealAndWait(joinCtx)
		}, capture)
	pendingAttribution := make([]syntheticFastPendingAttribution, len(sequence.Runs))
	for index := range sequence.Runs {
		row := &sequence.Runs[index]
		if row.State == "NOTRUN" {
			continue
		}
		before, beforeOk := row.BeforeDetails.(syntheticFastNativeBoundary)
		after, afterOk := row.AfterDetails.(syntheticFastNativeBoundary)
		var carrierErr error
		if !beforeOk || !afterOk {
			carrierErr = errors.New("synthetic Fast lost native window telemetry")
		} else {
			carrierErr = syntheticFastH1WindowError(before.ConsumerPackets, after.ConsumerPackets)
			pendingAttribution[index] = syntheticFastPendingAttribution{
				Observed:      before.PendingLogicalObserved && after.PendingLogicalObserved,
				BeforePackets: before.PendingLogicalEgressPackets, AfterPackets: after.PendingLogicalEgressPackets,
				DeltaPackets: after.PendingLogicalEgressPackets - before.PendingLogicalEgressPackets,
				BeforeBytes:  before.PendingLogicalEgressBytes, AfterBytes: after.PendingLogicalEgressBytes,
				DeltaBytes: after.PendingLogicalEgressBytes - before.PendingLogicalEgressBytes,
			}
		}
		if carrierErr != nil {
			row.State, sequence.Valid = "invalid", false
			if row.Error == "" {
				row.Error = carrierErr.Error()
			} else {
				row.Error += "; " + carrierErr.Error()
			}
			workloadErr = errors.Join(workloadErr, carrierErr)
		}
		if owner := origin.runs[index].Load(); owner != nil {
			row.ProviderMask, row.UnknownProviderRequests = owner.providerMask.Load(), owner.providerUnknown.Load()
			if row.ProviderMask == 0 || row.UnknownProviderRequests != 0 {
				row.State, row.Error, sequence.Valid = "invalid", "synthetic Fast original request provider attribution is missing", false
				workloadErr = errors.Join(workloadErr, errors.New(row.Error))
			}
			for flowIndex := range row.Workload.Flows {
				row.Workload.Flows[flowIndex].ProviderMask = owner.streamProviderMasks[flowIndex].Load()
			}
		}
	}
	originErr := closeOrigins()
	fixtureErr := fixture.close()
	workerErr := drainWorkers()
	if time.Since(started) > sequenceBound {
		workloadErr = errors.Join(workloadErr, context.DeadlineExceeded)
		sequence.Valid = false
	}
	mean, meanErr := syntheticFastFiveWindowMean(sequence)
	resultErr = errors.Join(workloadErr, originErr, fixtureErr, workerErr)
	if resultErr == nil {
		resultErr = meanErr
	} else if meanErr != nil {
		resultErr = errors.Join(resultErr, meanErr)
	}
	report = syntheticFastNativeIterationReport{Schema: "perfvar-public-native-fast-five-iteration-v3", ConsumerTarget: target, ProviderTarget: 128 * 1024 * 1024, ProviderCount: len(fixture.providers),
		Access: profile, Provider: providerProfile, Sequence: sequence, Mean: mean, PendingLogicalEgress: pendingAttribution, WorkerFinalJoined: workerJoined, ClientAfterJoin: fixture.afterJoin, ProviderAfterJoin: fixture.providerAfterJoin,
		OriginHandlersStarted: origin.started.Load(), OriginHandlersFinished: origin.finished.Load(), Valid: resultErr == nil,
		NativeFinalOffered: fixture.bridgeStarted.Load(), NativeFinalCompleted: fixture.bridgeCompleted.Load(),
		NativeFinalAccepted: fixture.bridgeAccepted.Load(), NativeFinalRefused: fixture.bridgeRefused.Load(), NativeFinalInvalid: fixture.bridgeInvalid.Load(),
		ReceiveFinalOfferedPackets: fixture.receiveOfferedPackets.Load(), ReceiveFinalOfferedBytes: fixture.receiveOfferedBytes.Load(),
		ReceiveFinalAcceptedBytes: fixture.receiveAcceptedBytes.Load(), ReceiveFinalInvalid: fixture.receiveInvalid.Load(),
		OwnedDnsBootstrap: fixture.ownedDoh.snapshot(),
		Scope:             "Five original retained cold/continuing ten-second windows; real public SDK32/64 consumers, public Desktop128 providers, compact native packet ABI, app TCP/TUN, provider TCP/NAT, verified TLS inner H1 and authenticated outer H1 in both directions. Explicit public Auto/AllowDirect=false retains default windows and selects relay-only public contracts; Known H1 ingress and egress must grow and every known non-H1 carrier remains unchanged; unknown egress is unresolved logical attribution, reported separately, and may move to its first known route. Unknown ingress remains a failure. Public egress counters attribute each logical packet to its first known physical route, not every retry; the route-policy/source contract supplies the independent P2P exclusion. Production contracts, capacities and default window/flow policy remain unchanged. Each iteration retains the arithmetic mean of every WholeRunMbps including startup/join; the root gates the median of three independent iteration means at200Mbps. Mandatory complete/cancellation-censored small GETs stay strictly below5s. Native compact offered/accepted/refused counters count packets; application receive counters explicitly count offered packets and offered/accepted bytes. Other legacy cumulative fields are unobserved, not zero-loss evidence. Default DNS endpoint identities are served by an owned HTTPS fixture with scoped test CA trust; public resolver service behavior is not measured. Default endpoint lists, family pairing and warm/racing/fallback settings stay unchanged. Resolver transport family coverage is established only by the corresponding actual socket and TLS/h2 response counters; a family without those receipts remains unexercised. Same-host synthetic fixture, not a physical-phone or deployed-provider result."}
	joinErr := syntheticFastNativeIterationJoinError(report)
	report.OwnersJoined = joinErr == nil && originErr == nil && fixtureErr == nil && workerErr == nil
	if joinErr != nil {
		resultErr = errors.Join(resultErr, joinErr)
	}
	return report, resultErr
}
