// A bounded multi-page backlog complements the clean navigation control.
// Connection acquisition, offered load and transport counters stay separate.
package perfvar

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
)

// Test-only JSON storage bound; unrelated to mobile, packet or owner budgets.
const nativeBrowsingPressureReportByteLimit = 128 * 1024

// Page capacity is a workload setting, not additional production queue space.
type nativeBrowsingRunSettings struct {
	maxLivePages                int
	pressure                    *nativeBrowsingPressureObservation
	nextNavigationOverlap       bool
	originListenerForTest       func(net.Listener)
	originServerTlsForTest      *tls.Config
	originClientTlsForTest      *tls.Config
	originDestinationForTest    func(int, net.Listener) string
	afterOriginsClosedForTest   func()
	wrapOriginHandlerForTest    func(http.Handler) http.Handler
	applicationDeadlineForTest  time.Duration
	beforePageNavigationForTest func(context.Context, int, func()) error
	requestTraceForTest         func(string) *httptrace.ClientTrace
	wrapResponseReaderForTest   func(string, io.Reader) io.Reader
	afterFetchForTest           func(string, nativeBrowsingAssetResult)
}

// Four origins with four HTTP connections each leave a navigation connection
// available beside at most three older assets per origin before the next tap.
func (self nativeBrowsingRunSettings) validate() error {
	if (self.originServerTlsForTest == nil) != (self.originClientTlsForTest == nil) {
		return errors.New("browsing TLS requires both verified client and server configurations")
	}
	if self.maxLivePages < 2 || nativeBrowsingOriginCount < self.maxLivePages {
		return errors.New("native browsing live-page window must be between two and four")
	}
	if self.nextNavigationOverlap && (self.pressure == nil || self.maxLivePages != nativeBrowsingOriginCount) {
		return errors.New("next-navigation body overlap requires the four-page pressure workload")
	}
	return nil
}

// Only the oldest page is disposed after the new page's assets are launched.
func nativeBrowsingRetirePage(page, livePages int) int { return page + 1 - livePages }

// The final window is drained explicitly, including canceled older assets.
func nativeBrowsingFinalPageStart(livePages int) int {
	return nativeBrowsingPageCount + 1 - livePages
}

// Cancellation cleanup releases every held handler, including never-visited
// pages. The per-page owner makes repeated release idempotent.
func nativeBrowsingReleasePages(pages []*nativeBrowsingPageState) {
	for _, page := range pages {
		page.releaseAssets()
		page.nextNavigation.release()
	}
}

// Both modes announce the same written prefix. Only the clean ordering control
// waits for retirement. Revised pressure streams one image up to its final
// fragment, then waits for the next navigation's handoff, independently of age.
func nativeBrowsingWriteAssetSuffix(writer http.ResponseWriter, requestCtx, workCtx context.Context, state *nativeBrowsingPageState, body []byte, asset int, streaming bool, progress *nativeBrowsingRequestProgress) bool {
	count, err := writer.Write(body[:nativeBrowsingHeldPrefixBytes])
	progress.recordServerWrite(nativeBrowsingHeldPrefixBytes, count, err)
	if err != nil || count != nativeBrowsingHeldPrefixBytes {
		return requestCtx.Err() != nil
	}
	flusher, ok := writer.(http.Flusher)
	if !ok {
		return false
	}
	flusher.Flush()
	state.heldOnce[asset].Do(func() { state.held <- asset })
	if !streaming {
		select {
		case <-requestCtx.Done():
			return true
		case <-workCtx.Done():
			return true
		case <-state.release:
		}
	}
	suffixStart := nativeBrowsingHeldPrefixBytes
	if streaming && asset == 2 && state.nextNavigation != nil {
		suffixStart = len(body) - nativeBrowsingHeldPrefixBytes
		count, err = writer.Write(body[nativeBrowsingHeldPrefixBytes:suffixStart])
		progress.recordServerWrite(suffixStart-nativeBrowsingHeldPrefixBytes, count, err)
		if err != nil || count != suffixStart-nativeBrowsingHeldPrefixBytes {
			return requestCtx.Err() != nil
		}
		flusher.Flush()
		select {
		case <-requestCtx.Done():
			return true
		case <-workCtx.Done():
			return true
		case <-state.nextNavigation.released:
		}
	}
	count, err = writer.Write(body[suffixStart:])
	progress.recordServerWrite(len(body)-suffixStart, count, err)
	return requestCtx.Err() != nil
}

// Per-request progress retains no payload and is fixed to the 100-page plan.
// Expected bytes are set before starting its one worker and never changed.
type nativeBrowsingRequestProgress struct {
	expectedBytes          int64
	receivedBytes          atomic.Int64
	started                atomic.Bool
	finished               atomic.Bool
	cancelRequested        atomic.Bool
	serverStarted          atomic.Bool
	serverFinished         atomic.Bool
	serverWritten          atomic.Int64
	serverWriteError       atomic.Bool
	serverShortWrite       atomic.Bool
	bodyOpened             atomic.Bool
	bodyClosed             atomic.Bool
	bodyCloseError         atomic.Bool
	serverReceipt          *clientconnect.MonitorValue[nativeBrowsingServerReceipt]
	connectionAcquisitions atomic.Int64
	requestWrites          atomic.Int64
	requestWriteErrors     atomic.Int64
	responseStatus         atomic.Int64
	nextNavigation         *nativeBrowsingNextNavigationBody
}

// Reading, not request completion, advances the consumed-body counters.
type nativeBrowsingProgressReader struct {
	reader   io.Reader
	pressure *nativeBrowsingPressureObservation
	progress *nativeBrowsingRequestProgress
}

func (self *nativeBrowsingProgressReader) Read(buffer []byte) (int, error) {
	count, err := self.reader.Read(buffer)
	self.pressure.receivedBytes.Add(int64(count))
	if self.progress != nil {
		self.progress.receivedBytes.Add(int64(count))
		if count != 0 {
			self.progress.nextNavigation.signalBodyProgress()
		}
	}
	return count, err
}

// Streaming may deliver more than the clean 1-KiB gate before cancellation.
// The response reader independently checked every prefix and the HTTP status.
func nativeBrowsingStreamingCancellationValid(result nativeBrowsingAssetResult, expected int) bool {
	return errors.Is(result.err, context.Canceled) &&
		!errors.Is(result.err, errNativeBrowsingResponseMismatch) &&
		0 <= result.bytes && result.bytes <= expected
}

// The eight fixed phase slots store first events only. Callbacks may be on
// transport goroutines; no callback takes a packet-owner lock or reads TCP stats.
type nativeBrowsingHttpTrace struct {
	started  time.Time
	now      func() time.Time
	phases   [8]atomic.Int64
	attempts atomic.Int32
	reused   atomic.Bool
}

// Plus one preserves a legitimate zero elapsed event while zero means unseen.
func (self *nativeBrowsingHttpTrace) mark(index int) {
	self.phases[index].CompareAndSwap(0, int64(self.now().Sub(self.started))+1)
}

// HTTP tracing does not change dialing, concurrency, response reads or deadlines.
func (self *nativeBrowsingHttpTrace) clientTrace(firstByte *atomic.Int64) *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn: func(string) { self.attempts.Add(1); self.mark(0) },
		GotConn: func(info httptrace.GotConnInfo) {
			self.reused.Store(info.Reused)
			self.mark(1)
		},
		ConnectStart:      func(string, string) { self.mark(2) },
		ConnectDone:       func(string, string, error) { self.mark(3) },
		TLSHandshakeStart: func() { self.mark(4) },
		TLSHandshakeDone:  func(tls.ConnectionState, error) { self.mark(5) },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			self.mark(6)
		},
		GotFirstResponseByte: func() {
			firstByte.CompareAndSwap(0, int64(self.now().Sub(self.started)))
			self.mark(7)
		},
	}
}

// Unknown phases are -1. A retried acquisition is marked rather than treating
// first-event differences as a complete single-attempt causal timeline.
type nativeBrowsingNavigationTrace struct {
	Phases                  [8]int64 `json:"phase_ns"`
	Attempts                int32    `json:"attempts"`
	Reused                  bool     `json:"reused"`
	ElapsedNs               int64    `json:"elapsed_ns"`
	ConnectionAcquireNs     int64    `json:"acquire_ns"`
	WrittenToFirstByteNs    int64    `json:"reply_ns"`
	UnfinishedPriorPages    int      `json:"prior_pages"`
	UnfinishedPriorAssets   int      `json:"prior_assets"`
	PriorRemainingBodyBytes int64    `json:"prior_bytes"`
	ActiveRequestsAtStart   int64    `json:"active"`
	OfferedBodyBytesAtStart int64    `json:"offered_bytes"`
	ErrorClass              string   `json:"error,omitempty"`
}

// Record fixed categories, never response bodies, URLs or arbitrary errors.
func nativeBrowsingErrorClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errNativeBrowsingResponseMismatch):
		return "response-validation"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "other"
	}
}

// Only exact observed phase pairs receive a duration; absence is not zero wait.
func (self *nativeBrowsingHttpTrace) snapshot(result nativeBrowsingAssetResult) nativeBrowsingNavigationTrace {
	row := nativeBrowsingNavigationTrace{Attempts: self.attempts.Load(), Reused: self.reused.Load(),
		ElapsedNs: int64(result.complete), ConnectionAcquireNs: -1, WrittenToFirstByteNs: -1,
		ErrorClass: nativeBrowsingErrorClass(result.err)}
	for index := range row.Phases {
		row.Phases[index] = self.phases[index].Load() - 1
	}
	if row.Attempts == 1 {
		if 0 <= row.Phases[0] && row.Phases[0] <= row.Phases[1] {
			row.ConnectionAcquireNs = row.Phases[1] - row.Phases[0]
		}
		if 0 <= row.Phases[6] && row.Phases[6] <= row.Phases[7] {
			row.WrittenToFirstByteNs = row.Phases[7] - row.Phases[6]
		}
	}
	return row
}

// Snapshots are fixture aggregates at navigation return, not per-flow blame or
// an observation at the exact five-second crossing. No new packet observer runs.
type nativeBrowsingPressureBoundary struct {
	AssetOwnership            nativeBrowsingAssetOwnership            `json:"asset_ownership"`
	Page                      int                                     `json:"page"`
	AtNs                      int64                                   `json:"at_ns"`
	BridgeStarted             uint64                                  `json:"bridge_started"`
	BridgePending             int                                     `json:"bridge_pending"`
	BridgeRefused             uint64                                  `json:"bridge_refused"`
	BridgeSendNs              int64                                   `json:"bridge_send_ns"`
	Multi                     clientconnect.MultiClientMemorySnapshot `json:"multi"`
	ProviderInitialWrites     uint64                                  `json:"provider_initial_writes"`
	ProviderTimeoutResends    uint64                                  `json:"provider_timeout_resends"`
	ProviderMissingContracts  uint64                                  `json:"provider_missing_contracts"`
	ProviderAdmissionWaitNs   int64                                   `json:"provider_admission_wait_ns"`
	ProviderUnacked           uint64                                  `json:"provider_unacked"`
	ProviderPackDrops         uint64                                  `json:"provider_pack_drops"`
	ProviderPackWaits         uint64                                  `json:"provider_pack_waits"`
	ProviderPackWaitSuccess   uint64                                  `json:"provider_pack_wait_success"`
	ProviderPackSaturations   uint64                                  `json:"provider_pack_saturations"`
	ProviderAckBlocked        uint64                                  `json:"provider_ack_blocked"`
	ProviderNatRemotePackets  int64                                   `json:"provider_nat_remote_packets"`
	ProviderNatBlockedPackets int64                                   `json:"provider_nat_blocked_packets"`
	Tcp                       [3][4]uint64                            `json:"tcp"`
	LinkCount                 int                                     `json:"link_count"`
	LinkWireBytes             uint64                                  `json:"link_wire_bytes"`
	LinkLossDrops             uint64                                  `json:"link_loss_drops"`
	LinkQueueDrops            uint64                                  `json:"link_queue_drops"`
	LinkQueuedBytes           int                                     `json:"link_queued_bytes"`
	PoolRoots                 uint64                                  `json:"pool_roots"`
	PoolBytes                 clientconnect.ByteCount                 `json:"pool_bytes"`
}

// Only canonical aggregate counters are sampled at navigation return. No
// socket, endpoint state or transport callback is retained by this report.
func nativeBrowsingCaptureBoundary(path *fullTunPath, page int, pressure *nativeBrowsingPressureObservation) *nativeBrowsingPressureBoundary {
	if pressure.captureBoundaryForTest != nil {
		return pressure.captureBoundaryForTest(path, page, pressure)
	}
	result := &nativeBrowsingPressureBoundary{Page: page, AtNs: time.Now().UnixNano(),
		BridgeStarted: path.bridgeSends.startedCount.Load(), BridgeRefused: path.bridgeSends.failureCount.Load(),
		BridgeSendNs: int64(path.bridgeSends.sendDurationNanoseconds.Load()), Multi: path.multiClient.MemorySnapshot(),
		PoolRoots: clientconnect.MessagePoolOutstandingCount(), PoolBytes: clientconnect.MessagePoolOutstandingByteCount()}
	result.AssetOwnership = pressure.assetOwnershipSnapshot()
	func() {
		path.bridgeSends.stateLock.Lock()
		defer path.bridgeSends.stateLock.Unlock()
		result.BridgePending = len(path.bridgeSends.liveEntries)
	}()
	recovery, receive := path.providerClient.SendRecoveryStats(), path.providerClient.ReceiveStats()
	result.ProviderInitialWrites, result.ProviderTimeoutResends = recovery.InitialWriteCount, recovery.TimeoutResendWriteCount
	result.ProviderMissingContracts = recovery.MissingContractRequestCount
	result.ProviderAdmissionWaitNs = int64(recovery.ReliableAdmissionWaitDuration)
	result.ProviderUnacked, result.ProviderPackDrops = receive.ResendQueueUnackedItemCount, receive.PackHandoffDropCount
	result.ProviderPackWaits, result.ProviderPackWaitSuccess, result.ProviderPackSaturations = receive.PackHandoffWaitCount, receive.PackHandoffWaitSuccess, receive.PackHandoffSaturationCount
	result.ProviderAckBlocked = receive.AckRouteWriteBlockedCount
	nat := path.providerRemoteNat.PacketStats()
	result.ProviderNatRemotePackets = nat.RemoteEgressPacketCount + nat.RemoteIngressPacketCount
	result.ProviderNatBlockedPackets = nat.BlockEgressPacketCount + nat.BlockIngressPacketCount
	for index, tun := range []*clientconnect.Tun{path.appTun, path.deviceCarrierTun, path.providerCarrierTun} {
		stats := tun.Stats().TCP
		result.Tcp[index] = [4]uint64{stats.SegmentsSent.Value(), stats.ValidSegmentsReceived.Value(), stats.Retransmits.Value(), stats.Timeouts.Value()}
	}
	for _, link := range path.environment.network.snapshotLinks() {
		result.LinkCount++
		result.LinkWireBytes += link.WireByteCount
		result.LinkLossDrops += link.LossDropPacketCount
		result.LinkQueueDrops += link.QueueDropPacketCount
		result.LinkQueuedBytes += link.QueuedByteCount
	}
	return result
}

// Exactly 100 compact navigation records and two boundary snapshots are kept.
// Atomic request counters have one fixture lifetime and no history or payload.
type nativeBrowsingPressureObservation struct {
	WorkloadVersion                string                                                 `json:"workload_version"`
	Profile                        networkProfile                                         `json:"device_access"`
	ProviderProfile                string                                                 `json:"provider_access"`
	MaximumLivePages               int                                                    `json:"maximum_live_pages"`
	HttpConnectionsPerOrigin       int                                                    `json:"http_connections_per_origin"`
	PhaseNames                     [8]string                                              `json:"phase_names"`
	Navigations                    [nativeBrowsingPageCount]nativeBrowsingNavigationTrace `json:"navigations"`
	SlowNavigationCount            int                                                    `json:"slow_navigation_count"`
	LastHealthy                    *nativeBrowsingPressureBoundary                        `json:"last_healthy,omitempty"`
	FirstSlowOrError               *nativeBrowsingPressureBoundary                        `json:"first_slow_or_error,omitempty"`
	OfferedBodyBytes               int64                                                  `json:"offered_body_bytes"`
	ReceivedBodyBytes              int64                                                  `json:"received_body_bytes"`
	PayloadSerializationNs         int64                                                  `json:"payload_only_serialization_floor_ns"`
	ElapsedNs                      int64                                                  `json:"elapsed_ns"`
	MaximumActiveRequests          int64                                                  `json:"maximum_active_requests"`
	CancellationAttempts           int                                                    `json:"cancellation_attempts"`
	CompletedDespiteCancellation   int                                                    `json:"completed_despite_cancellation"`
	ServerObservedCancellation     int                                                    `json:"server_observed_cancellation"`
	AssetDrain                     nativeBrowsingAssetOwnership                           `json:"asset_drain"`
	MaximumPriorRemainingBodyBytes int64                                                  `json:"maximum_prior_remaining_body_bytes"`
	Scope                          string                                                 `json:"scope"`
	captureBoundaryForTest         func(*fullTunPath, int, *nativeBrowsingPressureObservation) *nativeBrowsingPressureBoundary
	slowNavigation                 time.Duration
	activeRequests                 atomic.Int64
	maximumActiveRequests          atomic.Int64
	offeredBytes                   atomic.Int64
	receivedBytes                  atomic.Int64
	assets                         [nativeBrowsingPageCount][nativeBrowsingAssetCount]nativeBrowsingRequestProgress
}

// Count application offers separately from received prefixes and link bytes.
func (self *nativeBrowsingPressureObservation) beginFetch(expectedBytes int) {
	self.offeredBytes.Add(int64(expectedBytes))
	active := self.activeRequests.Add(1)
	for old := self.maximumActiveRequests.Load(); old < active && !self.maximumActiveRequests.CompareAndSwap(old, active); old = self.maximumActiveRequests.Load() {
	}
}

// Navigation is serial; asset request counters may change concurrently.
func (self *nativeBrowsingPressureObservation) beginNavigation(page, livePages int) *nativeBrowsingHttpTrace {
	row := &self.Navigations[page]
	for prior := max(0, page+1-livePages); prior < page; prior++ {
		unfinished := false
		for asset := range nativeBrowsingAssetCount {
			progress := &self.assets[prior][asset]
			if progress.expectedBytes == 0 || progress.finished.Load() {
				continue
			}
			unfinished = true
			row.UnfinishedPriorAssets++
			row.PriorRemainingBodyBytes += max(0, progress.expectedBytes-progress.receivedBytes.Load())
		}
		if unfinished {
			row.UnfinishedPriorPages++
		}
	}
	self.MaximumPriorRemainingBodyBytes = max(self.MaximumPriorRemainingBodyBytes, row.PriorRemainingBodyBytes)
	row.ActiveRequestsAtStart = self.activeRequests.Load()
	row.OfferedBodyBytesAtStart = self.offeredBytes.Load()
	return &nativeBrowsingHttpTrace{now: time.Now}
}

// A slow return is evidence, not proof that expected HTTP or bandwidth queueing
// was a VPN regression. Errors retain the workload's original failure behavior.
func (self *nativeBrowsingPressureObservation) finishNavigation(path *fullTunPath, page int, trace *nativeBrowsingHttpTrace, result nativeBrowsingAssetResult) {
	prior, row := self.Navigations[page], trace.snapshot(result)
	row.UnfinishedPriorPages, row.ActiveRequestsAtStart = prior.UnfinishedPriorPages, prior.ActiveRequestsAtStart
	row.UnfinishedPriorAssets, row.PriorRemainingBodyBytes = prior.UnfinishedPriorAssets, prior.PriorRemainingBodyBytes
	row.OfferedBodyBytesAtStart = prior.OfferedBodyBytesAtStart
	self.Navigations[page] = row
	slow := self.slowNavigation <= result.firstByte || result.err != nil && self.slowNavigation <= result.complete
	if slow {
		self.SlowNavigationCount++
	}
	if self.FirstSlowOrError == nil {
		if slow || result.err != nil {
			self.FirstSlowOrError = nativeBrowsingCaptureBoundary(path, page, self)
		} else {
			self.LastHealthy = nativeBrowsingCaptureBoundary(path, page, self)
		}
	}
}

// The offered full bodies include later cancellations. Only received body
// prefixes form the stated payload-only serialization lower bound.
func (self *nativeBrowsingPressureObservation) finish(start time.Time) {
	self.AssetDrain = self.assetOwnershipSnapshot()
	self.OfferedBodyBytes, self.ReceivedBodyBytes = self.offeredBytes.Load(), self.receivedBytes.Load()
	self.MaximumActiveRequests = self.maximumActiveRequests.Load()
	self.ElapsedNs = int64(time.Since(start))
	self.PayloadSerializationNs = self.ReceivedBodyBytes * 8 * int64(time.Second) / self.Profile.Reverse.RateBitsPerSecond
}

// All rebuilt clients at an endpoint retain one strict retry owner. The
// configured profiles, lane policy and deadline are the frozen pressure plan.
type nativeBrowsingPressurePlan struct {
	profile             networkProfile
	provider            networkProfile
	resources           tunResourceProfile
	deadline            time.Duration
	maxLivePages        int
	deviceRetryBudget   *clientconnect.TransferMemoryBudget
	providerRetryBudget *clientconnect.TransferMemoryBudget
}

// Construct fresh endpoint budgets without changing pacing or window sizing.
func newNativeBrowsingPressurePlan() *nativeBrowsingPressurePlan {
	resources := defaultTunResourceProfile()
	resources.LogicalDataLaneCount = 0
	return &nativeBrowsingPressurePlan{
		profile:             cellEdgeNetworkProfiles(4013)[cellEdge5mDown1mUpName],
		provider:            initialNetworkProfiles(4013)["clean-lan"],
		resources:           resources,
		deadline:            300 * time.Second,
		maxLivePages:        4,
		deviceRetryBudget:   clientconnect.NewTransferMemoryBudget(2 * 1024 * 1024),
		providerRetryBudget: clientconnect.NewTransferMemoryBudget(2 * 1024 * 1024),
	}
}

// Use existing fixture construction seams; no transport observer or private
// production overlay is needed to offer native packets or apply retry limits.
func (self *nativeBrowsingPressurePlan) hooks() *fullTunConstructionTestHooks {
	return &fullTunConstructionTestHooks{
		afterStage: func(stage fullTunConstructionStage, path *fullTunPath) error {
			if stage == fullTunConstructionStageMultiClient {
				path.nativeIngressForTest = true
			}
			return nil
		},
		configureProviderClientSettings: func(settings *clientconnect.ClientSettings) {
			settings.SendBufferSettings.ResendQueueBudget = self.providerRetryBudget
			settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
		},
		configureDeviceClientSettings: func(settings *clientconnect.ClientSettings) {
			settings.SendBufferSettings.ResendQueueBudget = self.deviceRetryBudget
			settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
		},
	}
}

// The ordinary pressure root explicitly opts into the revised image-tail gate.
// Other users of streaming bodies retain the original completion policy.
func (self *nativeBrowsingPressurePlan) runSettings(pressure *nativeBrowsingPressureObservation) nativeBrowsingRunSettings {
	return nativeBrowsingRunSettings{maxLivePages: self.maxLivePages, pressure: pressure, nextNavigationOverlap: true}
}

// Four pages accumulate across the existing cell-edge access shape. The
// provider stays colocated; bodies stream while new HTML requests are offered.
func TestFullTunH1NativeRapidBrowsingPressure100Pages(t *testing.T) {
	if testing.Short() {
		t.Skip("full H1/TUN browsing pressure topology")
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		plan := newNativeBrowsingPressurePlan()
		fixture, err := newPerfvarCorrectnessFixtureWithHooks(t, fullTunRouteExchangeH1,
			plan.profile, plan.profile, plan.provider, plan.resources, plan.deadline, plan.hooks())
		if err != nil {
			t.Fatal(err)
		}
		pressure := &nativeBrowsingPressureObservation{Profile: plan.profile, ProviderProfile: plan.provider.Name,
			MaximumLivePages: plan.maxLivePages, HttpConnectionsPerOrigin: nativeBrowsingConnectionsPerOrigin,
			slowNavigation: sdk.DefaultDeviceLocalSettings().SendTimeout,
			PhaseNames:     [8]string{"get_conn", "got_conn", "connect_start", "connect_done", "tls_start", "tls_done", "wrote_request", "first_byte"},
			Scope:          "host full-TUN/H1; one shared 2-MiB retained retry budget per endpoint, lane 0; four origins with four connections each and at most four live pages; revised next-navigation-image-v2 holds only the final 1 KiB of image 2 on pages 0..98 until the next HTML handoff; other body bytes stream before retirement; 100 deterministic 50..300-KiB pages; inner plain HTTP/1.1 and synthetic local listeners; no DNS; 300-second application deadline. Connection events may include losing speculative dials. Aggregate counters are navigation-return snapshots. Server writes are local HTTP acceptance; Body.Close is not a socket-close receipt. Process memory is test attribution, not mobile qualification or a throughput baseline."}
		for index := range pressure.Navigations {
			pressure.Navigations[index].ConnectionAcquireNs = -1
			pressure.Navigations[index].WrittenToFirstByteNs = -1
			for phase := range pressure.Navigations[index].Phases {
				pressure.Navigations[index].Phases[phase] = -1
			}
		}
		observation := &nativeBrowsingObservation{Baseline: nativeBrowsingMemoryNow(), Pressure: pressure}
		observation.Peak = observation.Baseline
		_, observation.NativeCapability = any(fixture.path.multiClient).(nativeBrowsingResultsClient)
		before, start := fixture.path.bridgeSends.batchSnapshot(), time.Now()
		defer func() {
			pressure.finish(start)
			observation.Bridge = observeFullTunBridgeBatches(before, fixture.path.bridgeSends.batchSnapshot())
			fixture.close()
			for index, budget := range []*clientconnect.TransferMemoryBudget{plan.deviceRetryBudget, plan.providerRetryBudget} {
				reserved, released := budget.Counts()
				if budget.UsedByteCount() != 0 || reserved != released {
					t.Errorf("browsing endpoint %d retained retry ownership: used=%d reserved=%d released=%d",
						index, budget.UsedByteCount(), reserved, released)
				}
			}
			observation.Drain = nativeBrowsingMemoryNow()
			encoded, encodeErr := json.Marshal(observation)
			if encodeErr != nil || nativeBrowsingPressureReportByteLimit < len(encoded) {
				t.Errorf("bounded browsing pressure observation: bytes=%d err=%v", len(encoded), encodeErr)
				return
			}
			t.Logf("[perfvar] native-browsing-pressure100 %s", encoded)
		}()
		if err := measureNativeBrowsing100WithSettings(fixture.ctx, fixture.path, observation,
			plan.runSettings(pressure)); err != nil {
			t.Fatal(err)
		}
		if err := nativeBrowsingPressureLatencyError(observation); err != nil {
			t.Fatal(err)
		}
		if observation.OverlapProofs != nativeBrowsingPageCount-1 || pressure.MaximumPriorRemainingBodyBytes == 0 {
			t.Fatalf("pressure browsing body overlap: proofs=%d want=%d", observation.OverlapProofs, nativeBrowsingPageCount-1)
		}
		if err := fixture.path.verifyRoute(); err != nil {
			t.Fatal(err)
		}
	})
}

// Completion alone must not hide the pauses this workload was built to catch.
// Use the same navigation threshold as the clean browsing control, without
// changing offered load, cancellation, the link shape or the overall deadline.
func nativeBrowsingPressureLatencyError(observation *nativeBrowsingObservation) error {
	if observation.CompletedNavigations != nativeBrowsingPageCount {
		return fmt.Errorf("NATIVE_BROWSING_INCOMPLETE: completed=%d want=%d", observation.CompletedNavigations, nativeBrowsingPageCount)
	}
	limit := sdk.DefaultDeviceLocalSettings().SendTimeout
	for page, row := range observation.Pages {
		if row.NavigationStalled || limit <= time.Duration(row.FirstByteNanoseconds) {
			return fmt.Errorf("NATIVE_BROWSING_NAVIGATION_STALL: page=%d first_byte=%s limit=%s", page, time.Duration(row.FirstByteNanoseconds), limit)
		}
	}
	if observation.Pressure == nil || observation.Pressure.SlowNavigationCount != 0 {
		return errors.New("NATIVE_BROWSING_NAVIGATION_STALL: missing pressure evidence or nonzero slow navigation count")
	}
	return nil
}

func TestNativeBrowsingPressureLatencyGateRejectsCompletedStalls(t *testing.T) {
	limit := sdk.DefaultDeviceLocalSettings().SendTimeout
	for _, test := range []struct {
		name            string
		page            int
		delay           time.Duration
		stalled         bool
		slow            int
		completed       int
		wantError       bool
		missingPressure bool
	}{
		{name: "below-boundary", delay: limit - time.Nanosecond, completed: nativeBrowsingPageCount},
		{name: "first-page-boundary", delay: limit, completed: nativeBrowsingPageCount, wantError: true},
		{name: "middle-page-stall", page: nativeBrowsingPageCount / 2, delay: limit + time.Nanosecond, completed: nativeBrowsingPageCount, wantError: true},
		{name: "last-page-boundary", page: nativeBrowsingPageCount - 1, delay: limit, completed: nativeBrowsingPageCount, wantError: true},
		{name: "row-verdict", stalled: true, completed: nativeBrowsingPageCount, wantError: true},
		{name: "aggregate-verdict", slow: 1, completed: nativeBrowsingPageCount, wantError: true},
		{name: "incomplete", completed: nativeBrowsingPageCount - 1, wantError: true},
		{name: "missing-pressure", completed: nativeBrowsingPageCount, missingPressure: true, wantError: true},
	} {
		observation := &nativeBrowsingObservation{CompletedNavigations: test.completed,
			Pressure: &nativeBrowsingPressureObservation{SlowNavigationCount: test.slow}}
		if test.missingPressure {
			observation.Pressure = nil
		}
		observation.Pages[test.page].FirstByteNanoseconds = int64(test.delay)
		observation.Pages[test.page].NavigationStalled = test.stalled
		if err := nativeBrowsingPressureLatencyError(observation); (err != nil) != test.wantError {
			t.Fatalf("%s: completed browsing latency gate: err=%v want_error=%t", test.name, err, test.wantError)
		}
	}
}

// Every scheduled navigation retains older unfinished work without filling
// all connections of any origin. The clean two-page disposition stays exact.
func TestNativeBrowsingPressureScheduleKeepsFourPages(t *testing.T) {
	for _, limit := range []int{2, 4} {
		settings := nativeBrowsingRunSettings{maxLivePages: limit}
		if err := settings.validate(); err != nil {
			t.Fatal(err)
		}
		live, retired := map[int]bool{}, map[int]bool{}
		maximumPrior, canceled := 0, 0
		for page := range nativeBrowsingPageCount {
			maximumPrior = max(maximumPrior, len(live))
			var heldByOrigin [nativeBrowsingOriginCount]int
			for previous := range live {
				for asset := range nativeBrowsingAssetCount {
					heldByOrigin[(previous+asset)%nativeBrowsingOriginCount]++
				}
			}
			for _, held := range heldByOrigin {
				if held >= 4 {
					t.Fatalf("page=%d limit=%d fixture exhausted all origin connections", page, limit)
				}
			}
			live[page] = true
			if len(live) > limit {
				t.Fatal("page window exceeded its hard bound")
			}
			if oldest := nativeBrowsingRetirePage(page, limit); oldest >= 0 {
				if !live[oldest] || retired[oldest] {
					t.Fatal("retired page is missing or already disposed")
				}
				delete(live, oldest)
				retired[oldest] = true
				if oldest%2 == 0 {
					canceled += 2
				}
			}
		}
		for page := nativeBrowsingFinalPageStart(limit); page < nativeBrowsingPageCount; page++ {
			if !live[page] || retired[page] {
				t.Fatal("final page owner missing")
			}
			delete(live, page)
			retired[page] = true
			if page != nativeBrowsingPageCount-1 && page%2 == 0 {
				canceled += 2
			}
		}
		if maximumPrior != limit-1 || len(live) != 0 || len(retired) != 100 || canceled != 100 {
			t.Fatalf("limit=%d prior=%d live=%d retired=%d canceled=%d", limit, maximumPrior, len(live), len(retired), canceled)
		}
	}
}

// Acquisition includes dialing; raw fixture-dial phases distinguish it from a
// written request's response delay, including an error before a connection.
func TestNativeBrowsingPressureTraceSeparatesConnectionAcquisition(t *testing.T) {
	now := time.Unix(100, 0)
	trace := &nativeBrowsingHttpTrace{started: now, now: func() time.Time { return now }}
	var first atomic.Int64
	hooks := trace.clientTrace(&first)
	hooks.GetConn("synthetic.example:80")
	now = now.Add(20 * time.Millisecond)
	hooks.ConnectStart("tcp", "synthetic.example:80")
	now = now.Add(10 * time.Millisecond)
	hooks.ConnectDone("tcp", "synthetic.example:80", nil)
	now = now.Add(10 * time.Millisecond)
	hooks.GotConn(httptrace.GotConnInfo{})
	now = now.Add(10 * time.Millisecond)
	hooks.WroteRequest(httptrace.WroteRequestInfo{})
	now = now.Add(200 * time.Millisecond)
	hooks.GotFirstResponseByte()
	row := trace.snapshot(nativeBrowsingAssetResult{complete: 250 * time.Millisecond})
	if row.ConnectionAcquireNs != int64(40*time.Millisecond) || row.Phases[3]-row.Phases[2] != int64(10*time.Millisecond) || row.WrittenToFirstByteNs != int64(200*time.Millisecond) || row.Phases[4] != -1 || first.Load() != int64(250*time.Millisecond) {
		t.Fatalf("phase attribution changed: %+v", row)
	}
	hooks.GetConn("synthetic.example:80")
	if repeated := trace.snapshot(nativeBrowsingAssetResult{}); repeated.Attempts != 2 || repeated.ConnectionAcquireNs != -1 {
		t.Fatal("multiple attempts claimed a single exact acquisition interval")
	}
	blocked := &nativeBrowsingHttpTrace{started: now, now: func() time.Time { return now }}
	blocked.clientTrace(&first).GetConn("synthetic.example:80")
	row = blocked.snapshot(nativeBrowsingAssetResult{complete: 5 * time.Second, err: context.DeadlineExceeded})
	if row.ErrorClass != "deadline" || row.Phases[0] != 0 || row.Phases[1] != -1 || row.ConnectionAcquireNs != -1 {
		t.Fatal("failed request lost unknown connection evidence")
	}
	// The fixed report fits its existing cap with all 100 rows populated,
	// long phase durations and both aggregate boundary snapshots retained.
	pressure := &nativeBrowsingPressureObservation{Profile: cellEdgeNetworkProfiles(4013)[cellEdge5mDown1mUpName],
		LastHealthy: &nativeBrowsingPressureBoundary{}, FirstSlowOrError: &nativeBrowsingPressureBoundary{}, Scope: strings.Repeat("x", 1024)}
	for _, boundary := range []*nativeBrowsingPressureBoundary{pressure.LastHealthy, pressure.FirstSlowOrError} {
		boundary.ProviderPackWaits, boundary.ProviderPackWaitSuccess, boundary.ProviderPackSaturations = ^uint64(0), ^uint64(0), ^uint64(0)
	}
	observation := &nativeBrowsingObservation{Pressure: pressure}
	for page := range nativeBrowsingPageCount {
		pressure.Navigations[page] = nativeBrowsingNavigationTrace{Phases: [8]int64{300e9, 300e9, 300e9, 300e9, 300e9, 300e9, 300e9, 300e9},
			Attempts: 2, ElapsedNs: 300e9, ConnectionAcquireNs: 300e9, WrittenToFirstByteNs: 300e9,
			UnfinishedPriorPages: 3, UnfinishedPriorAssets: 12, PriorRemainingBodyBytes: 900 * 1024,
			ActiveRequestsAtStart: 16, OfferedBodyBytesAtStart: 30 * 1024 * 1024, ErrorClass: "response-validation"}
		observation.Pages[page] = nativeBrowsingPageObservation{Page: page, PlannedBytes: 300 * 1024, CompletedBytes: 300 * 1024,
			CanceledPrefixBytes: 150 * 1024, CanceledAssets: 2, FirstByteNanoseconds: 300e9, CompletionNanoseconds: 300e9,
			PreviousAssetsHeld: 2, NavigationStalled: true}
	}
	if encoded, err := json.Marshal(observation); err != nil || nativeBrowsingPressureReportByteLimit < len(encoded) {
		t.Fatalf("fixed pressure report exceeded its bound: bytes=%d err=%v", len(encoded), err)
	}
}

// The same writer used by real handlers streams its suffix before retirement;
// only the clean control deliberately parks the body at the prefix boundary.
func TestNativeBrowsingPressureBodiesFlowBeforePageRetirement(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			state := newNativeBrowsingPageState()
			defer func() { state.releaseAssets(); synctest.Wait() }()
			writer := httptest.NewRecorder()
			body := nativeBrowsingAsset(0, 2)
			done := make(chan bool, 1)
			go func() {
				done <- nativeBrowsingWriteAssetSuffix(writer, context.Background(), context.Background(), state, body, 2, streaming, nil)
			}()
			synctest.Wait()
			select {
			case asset := <-state.held:
				if asset != 2 {
					t.Fatal("wrong asset prefix")
				}
			default:
				t.Fatal("body prefix was not written")
			}
			select {
			case <-state.release:
				t.Fatal("test released page before checking body flow")
			default:
			}
			if streaming {
				select {
				case canceled := <-done:
					if canceled {
						t.Fatal("streaming body was canceled")
					}
				default:
					t.Fatal("streaming suffix waited for page retirement")
				}
				if writer.Body.Len() != len(body) {
					t.Fatal("streaming suffix did not reach the writer")
				}
			} else {
				select {
				case <-done:
					t.Fatal("clean control lost its ordering gate")
				default:
				}
				if writer.Body.Len() != nativeBrowsingHeldPrefixBytes {
					t.Fatal("clean body crossed its held prefix")
				}
				state.releaseAssets()
				synctest.Wait()
				select {
				case <-done:
				default:
					t.Fatal("released clean body did not finish")
				}
			}
			state.releaseAssets()
		})
	}
	pressure := &nativeBrowsingPressureObservation{}
	progress := &pressure.assets[0][2]
	progress.expectedBytes = 8192
	progress.started.Store(true)
	reader := &nativeBrowsingProgressReader{reader: strings.NewReader(strings.Repeat("x", 4096)), pressure: pressure, progress: progress}
	buffer := make([]byte, 2048)
	if count, err := reader.Read(buffer); count != len(buffer) || err != nil {
		t.Fatal("progress reader did not preserve its read")
	}
	pressure.beginNavigation(1, 4)
	row := pressure.Navigations[1]
	if pressure.receivedBytes.Load() != 2048 || row.PriorRemainingBodyBytes != 6144 || row.UnfinishedPriorAssets != 1 || progress.finished.Load() {
		t.Fatal("body progress was deferred until request completion")
	}
	partial := nativeBrowsingAssetResult{bytes: 4096, err: context.Canceled}
	if !nativeBrowsingStreamingCancellationValid(partial, 8192) || nativeBrowsingCanceledAssetValid(partial) {
		t.Fatal("streaming and gated cancellation bounds were conflated")
	}
	partial.err = errors.Join(context.Canceled, errNativeBrowsingResponseMismatch)
	if nativeBrowsingStreamingCancellationValid(partial, 8192) {
		t.Fatal("cancellation hid response corruption")
	}
}

// Real waiters on each held-page release are joined under cancellation. A
// second cleanup must not double-close or leave any retained page worker.
func TestNativeBrowsingPressureCleanupReleasesEveryPage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		pages := make([]*nativeBrowsingPageState, 4)
		done := make(chan struct{}, len(pages))
		for index := range pages {
			pages[index] = newNativeBrowsingPageState()
			go func(page *nativeBrowsingPageState) {
				select {
				case <-ctx.Done():
				case <-page.release:
				}
				done <- struct{}{}
			}(pages[index])
		}
		synctest.Wait()
		nativeBrowsingReleasePages(pages)
		nativeBrowsingReleasePages(pages)
		synctest.Wait()
		for index := range pages {
			select {
			case <-pages[index].release:
			default:
				t.Fatal("cleanup left a held page")
			}
			select {
			case <-done:
			default:
				t.Fatal("cleanup did not join a held-page waiter")
			}
		}
	})
}
