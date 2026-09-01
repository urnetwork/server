// This file drives verified TCP, QUIC, UDP, web-like, and latency-under-load
// traffic through production gVisor TUNs connected by the PERFVAR simulator.
package perfvar

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptrace"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	clientconnect "github.com/urnetwork/connect/v2026"
)

// Latency summaries use nearest-rank samples from one sorted duration vector.
type latencyDistribution struct {
	P50 time.Duration `json:"p50_nanoseconds"`
	P95 time.Duration `json:"p95_nanoseconds"`
	P99 time.Duration `json:"p99_nanoseconds"`
	Max time.Duration `json:"max_nanoseconds"`
}

// Workload observations are route-neutral and feed scenario result records.
type workloadResult struct {
	UsefulByteCount            int64                     `json:"useful_byte_count"`
	WarmupByteCount            int64                     `json:"warmup_byte_count,omitempty"`
	WarmupDuration             time.Duration             `json:"warmup_duration_nanoseconds,omitempty"`
	OfferedPacketCount         int64                     `json:"offered_packet_count,omitempty"`
	DeliveredPacketCount       int64                     `json:"delivered_packet_count,omitempty"`
	DuplicatePacketCount       int64                     `json:"duplicate_packet_count,omitempty"`
	ReorderedPacketCount       int64                     `json:"reordered_packet_count,omitempty"`
	CorruptPacketCount         int64                     `json:"corrupt_packet_count,omitempty"`
	Duration                   time.Duration             `json:"duration_nanoseconds"`
	SetupDuration              time.Duration             `json:"setup_duration_nanoseconds,omitempty"`
	TimeToFirstByte            time.Duration             `json:"time_to_first_byte_nanoseconds,omitempty"`
	GoodputMegabytes           float64                   `json:"goodput_megabytes_per_second"`
	GoodputGigabits            float64                   `json:"goodput_gigabits_per_second"`
	Latency                    latencyDistribution       `json:"latency"`
	IdleLatency                latencyDistribution       `json:"idle_latency"`
	LoadedLatency              latencyDistribution       `json:"loaded_latency"`
	PostLoadLatency            latencyDistribution       `json:"post_load_latency"`
	IdleProbeAttemptCount      int                       `json:"idle_probe_attempt_count,omitempty"`
	IdleProbeSuccessCount      int                       `json:"idle_probe_success_count,omitempty"`
	IdleProbeFailureCount      int                       `json:"idle_probe_failure_count,omitempty"`
	LoadedProbeAttemptCount    int                       `json:"loaded_probe_attempt_count,omitempty"`
	LoadedProbeSuccessCount    int                       `json:"loaded_probe_success_count,omitempty"`
	LoadedProbeFailureCount    int                       `json:"loaded_probe_failure_count,omitempty"`
	PostLoadProbeAttemptCount  int                       `json:"post_load_probe_attempt_count,omitempty"`
	PostLoadProbeSuccessCount  int                       `json:"post_load_probe_success_count,omitempty"`
	PostLoadProbeFailureCount  int                       `json:"post_load_probe_failure_count,omitempty"`
	TerminalMarkerAttemptCount int                       `json:"terminal_marker_attempt_count,omitempty"`
	AllocatedByteCount         uint64                    `json:"allocated_byte_count"`
	AllocationCount            uint64                    `json:"allocation_count"`
	GarbageCollectionCount     uint32                    `json:"garbage_collection_count"`
	GarbageCollectionPause     time.Duration             `json:"garbage_collection_pause_nanoseconds"`
	ContentHash                string                    `json:"content_hash,omitempty"`
	ProfileEvents              []profileEventObservation `json:"profile_events,omitempty"`
	ForwardLink                directionalLinkSnapshot   `json:"forward_link"`
	ReverseLink                directionalLinkSnapshot   `json:"reverse_link"`
}

// A deadline setter covers TCP, UDP, and QUIC stream I/O uniformly.
type workloadDeadlineSetter interface {
	SetDeadline(time.Time) error
}

// The context deadline is the hard run boundary even when a workload-specific
// allowance is intentionally much larger for an impaired path.
func boundedWorkloadDeadline(ctx context.Context, timeout time.Duration) time.Time {
	deadline := time.Now().Add(timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		return contextDeadline
	}
	return deadline
}

// Established socket and QUIC I/O does not inherit the context used to dial it.
// Moving its deadline to now makes both pending and future operations observe a
// cancellation while the returned stop function prevents a retained callback.
func interruptDeadlineOnContext(ctx context.Context, target workloadDeadlineSetter) func() {
	stop := context.AfterFunc(ctx, func() {
		_ = target.SetDeadline(time.Now())
	})
	return func() {
		stop()
	}
}

// Socket and context timers can become ready in either order at the same
// absolute deadline. Preserve the context cause when its boundary has arrived.
func contextBoundWorkloadError(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if contextDeadline, ok := ctx.Deadline(); ok && !time.Now().Before(contextDeadline) {
		return context.DeadlineExceeded
	}
	return err
}

// Context-aware application delay cannot extend a canceled mobile-surrogate run.
func waitForWorkloadDelay(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// One reusable path owns two production TUNs and both directional schedulers.
type tunPath struct {
	ctx         context.Context
	cancel      context.CancelFunc
	left        *clientconnect.Tun
	right       *clientconnect.Tun
	network     *simulatedIPNetwork
	forwardLink *directionalLink
	reverseLink *directionalLink
	closeOnce   sync.Once

	// A nil production seam can inject an exact admission at the first
	// all-links-idle observation to prove a measurement generation retry.
	afterLinksIdleForTest func()
	// The link index exposes the gap between sequential interval resets.
	afterMeasurementLinkSnapshotForTest func(int)
}

// One immutable calibration boundary excludes route and protocol setup from
// the physical counters attributed to the timed application interval.
type tunPathMeasurementBoundary struct {
	capturedAt  time.Time
	forwardLink directionalLinkSnapshot
	reverseLink directionalLinkSnapshot
}

// A resource profile changes explicit TUN capacity without claiming device fidelity.
type tunResourceProfile struct {
	ChannelSize        int
	TcpBufferDefault   int
	TcpBufferMax       int
	UdpBuffer          int
	BatchSize          int
	AppDelay           time.Duration
	SingularBridgeSend bool
	// ApplicationMtu overrides only the full-route device-facing TUN. Carrier
	// TUNs retain their access profile MTU so an MTU experiment cannot silently
	// change the simulated underlay at the same time.
	ApplicationMtu int
	// LogicalDataLaneCount configures both endpoint Client generators in the
	// full-TUN fixture. It does not affect direct underlay calibration.
	LogicalDataLaneCount int
}

// Optional seams expose exact TCP admission and latency-worker lifecycle
// boundaries to deterministic workload regressions.
type workloadTCPFlowTestSettings struct {
	flowServerSettings          *logicalTCPFlowServerSettings
	beforeClientDialHook        func(context.Context, *clientconnect.Tun, string) error
	beforeProbeServerDoneHook   func()
	beforeProbeServerWaitHook   func()
	beforeBulkSenderDoneHook    func()
	beforeBulkSenderWaitHook    func()
	beforeBulkReceiverDoneHook  func()
	afterLoadedProbeAttemptHook func(int)
}

// Default resources retain production settings with enough ring space for calibration.
func defaultTunResourceProfile() tunResourceProfile {
	return tunResourceProfile{
		ChannelSize: 4096,
		BatchSize:   64,
	}
}

// The application side of a full tunnel models the product VPN interface,
// whose advertised MTU cannot exceed Connect's packet boundary. Physical link
// profiles and direct calibration retain their own inner MTU; an explicit
// application override remains available for boundary diagnostics.
func resolvedFullTunApplicationMtu(
	profile networkProfile,
	resources tunResourceProfile,
) int {
	if 0 < resources.ApplicationMtu {
		return resources.ApplicationMtu
	}
	return min(clientconnect.DefaultMtu, profile.InnerMtu)
}

// The mobile surrogate deliberately shrinks queues and inserts app-boundary delay.
func mobileTunResourceProfile() tunResourceProfile {
	return tunResourceProfile{
		ChannelSize:      256,
		TcpBufferDefault: 256 * 1024,
		TcpBufferMax:     2 * 1024 * 1024,
		UdpBuffer:        128 * 1024,
		BatchSize:        8,
		AppDelay:         100 * time.Microsecond,
	}
}

// Resource limits override production defaults while preserving a distinct
// initial TCP window and auto-tuning ceiling.
func applyTunResourceProfile(settings *clientconnect.TunSettings, resources tunResourceProfile) {
	if 0 < resources.TcpBufferDefault {
		tcpBufferMax := resources.TcpBufferDefault
		if 0 < resources.TcpBufferMax {
			tcpBufferMax = resources.TcpBufferMax
		}
		settings.TcpReceiveBuffer.Default = resources.TcpBufferDefault
		settings.TcpReceiveBuffer.Max = tcpBufferMax
		settings.TcpSendBuffer.Default = resources.TcpBufferDefault
		settings.TcpSendBuffer.Max = tcpBufferMax
	}
	if 0 < resources.UdpBuffer {
		settings.UdpReceiveBufferByteCount = resources.UdpBuffer
		settings.UdpSendBufferByteCount = resources.UdpBuffer
	}
}

// Construction applies profile MTU and optional resource limits symmetrically.
func newTunPath(
	ctx context.Context,
	profile networkProfile,
	resources tunResourceProfile,
) (*tunPath, error) {
	pathCtx, cancel := context.WithCancel(ctx)
	newTun := func() (*clientconnect.Tun, error) {
		settings := clientconnect.DefaultTunSettingsWithBufferSize(resources.ChannelSize)
		settings.Mtu = profile.InnerMtu
		applyTunResourceProfile(settings, resources)
		return clientconnect.CreateTun(pathCtx, settings)
	}
	left, err := newTun()
	if err != nil {
		cancel()
		return nil, err
	}
	right, err := newTun()
	if err != nil {
		left.Close()
		cancel()
		return nil, err
	}
	network := newSimulatedIPNetwork(pathCtx)
	if err := network.addTun("left", left); err != nil {
		left.Close()
		right.Close()
		cancel()
		return nil, err
	}
	if err := network.addTun("right", right); err != nil {
		network.close()
		right.Close()
		cancel()
		return nil, err
	}
	forwardLink, reverseLink, err := network.addBidirectionalLink("left", "right", profile)
	if err != nil {
		network.close()
		cancel()
		return nil, err
	}
	return &tunPath{
		ctx:         pathCtx,
		cancel:      cancel,
		left:        left,
		right:       right,
		network:     network,
		forwardLink: forwardLink,
		reverseLink: reverseLink,
	}, nil
}

// Both address directions are available to workload listeners and dialers.
func (self *tunPath) endpointAddress(left bool) net.IP {
	if left {
		return net.IP(self.left.LocalAddresses()[0].AsSlice())
	}
	return net.IP(self.right.LocalAddresses()[0].AsSlice())
}

// Teardown cancels application work before joining the network and TUN workers.
func (self *tunPath) close() {
	self.closeOnce.Do(func() {
		self.cancel()
		self.network.close()
	})
}

// Every calibration boundary repeats its all-link pass if either direction
// admits a packet while the prior generation is being joined.
func (self *tunPath) waitForTerminalIdle(ctx context.Context) bool {
	return waitForDirectionalLinksTerminalIdle(
		ctx,
		self.network.directionalLinks(),
		self.afterLinksIdleForTest,
	)
}

// Interval epochs differ by design, so monotonic state is compared after
// removing only the lock-held per-measurement identity and its maxima.
func sameCalibrationLinkMonotonicState(
	before directionalLinkSnapshot,
	after directionalLinkSnapshot,
) bool {
	before.measurementMaximumEpoch = nil
	before.measurementMaximumPackets = 0
	before.measurementMaximumBytes = 0
	before.measurementMaximumPacketBytes = 0
	after.measurementMaximumEpoch = nil
	after.measurementMaximumPackets = 0
	after.measurementMaximumBytes = 0
	after.measurementMaximumPacketBytes = 0
	return before == after
}

// A stable candidate retains its new interval identity and has no post-reset
// admission. Traffic after this check belongs to the measured interval.
func stableCalibrationLinkMeasurement(
	start directionalLinkSnapshot,
	end directionalLinkSnapshot,
) bool {
	return start.measurementMaximumEpoch != nil &&
		start.measurementMaximumEpoch == end.measurementMaximumEpoch &&
		sameCalibrationLinkMonotonicState(start, end) &&
		end.activeSubmissionCount == 0 && end.QueuedPacketCount == 0 &&
		end.QueuedByteCount == 0
}

// A start boundary first joins setup traffic, then retries both physical
// directions if any monotonic state crossed their sequential epoch resets.
func (self *tunPath) beginMeasurement(
	ctx context.Context,
) (tunPathMeasurementBoundary, error) {
	for {
		if !self.waitForTerminalIdle(ctx) {
			return tunPathMeasurementBoundary{}, fmt.Errorf(
				"join calibration measurement start: context=%v",
				ctx.Err(),
			)
		}
		beforeForward := self.forwardLink.snapshot()
		beforeReverse := self.reverseLink.snapshot()
		forwardLink, ok := self.forwardLink.beginMeasurementSnapshot(ctx)
		if !ok {
			return tunPathMeasurementBoundary{}, fmt.Errorf(
				"begin forward calibration interval: context=%v",
				ctx.Err(),
			)
		}
		if self.afterMeasurementLinkSnapshotForTest != nil {
			self.afterMeasurementLinkSnapshotForTest(0)
		}
		reverseLink, ok := self.reverseLink.beginMeasurementSnapshot(ctx)
		if !ok {
			return tunPathMeasurementBoundary{}, fmt.Errorf(
				"begin reverse calibration interval: context=%v",
				ctx.Err(),
			)
		}
		if self.afterMeasurementLinkSnapshotForTest != nil {
			self.afterMeasurementLinkSnapshotForTest(1)
		}
		afterForward := self.forwardLink.snapshot()
		afterReverse := self.reverseLink.snapshot()
		if sameCalibrationLinkMonotonicState(beforeForward, forwardLink) &&
			sameCalibrationLinkMonotonicState(beforeReverse, reverseLink) &&
			stableCalibrationLinkMeasurement(forwardLink, afterForward) &&
			stableCalibrationLinkMeasurement(reverseLink, afterReverse) {
			return tunPathMeasurementBoundary{
				capturedAt:  time.Now(),
				forwardLink: forwardLink,
				reverseLink: reverseLink,
			}, nil
		}
	}
}

// An end boundary joins every measured packet before returning interval-only
// counters and the queue maxima owned by the matching measurement identity.
func (self *tunPath) finishMeasurement(
	ctx context.Context,
	start tunPathMeasurementBoundary,
) (directionalLinkSnapshot, directionalLinkSnapshot, error) {
	if !self.waitForTerminalIdle(ctx) {
		return directionalLinkSnapshot{}, directionalLinkSnapshot{}, fmt.Errorf(
			"join calibration measurement end: context=%v",
			ctx.Err(),
		)
	}
	duration := time.Since(start.capturedAt)
	return subtractDirectionalLinkSnapshot(
			start.forwardLink,
			self.forwardLink.snapshot(),
			duration,
		), subtractDirectionalLinkSnapshot(
			start.reverseLink,
			self.reverseLink.snapshot(),
			duration,
		), nil
}

// A setup high-water mark and larger lifetime totals cannot leak into a later
// one-packet interval. Explicit ingress barriers make the setup queue larger
// than the measurement without relying on scheduler timing.
func TestTunPathMeasurementBoundaryExcludesPriorLifetimeCounters(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	profile := initialNetworkProfiles(20260811)["clean-lan"]
	path, err := newTunPath(ctx, profile, defaultTunResourceProfile())
	if err != nil {
		t.Fatal(err)
	}
	defer path.close()

	firstIngressEntered := make(chan struct{})
	releaseFirstIngress := make(chan struct{})
	var firstIngress atomic.Bool
	path.forwardLink.beforeIngressForTest = func() {
		if firstIngress.CompareAndSwap(false, true) {
			close(firstIngressEntered)
			select {
			case <-releaseFirstIngress:
			case <-ctx.Done():
			}
		}
	}
	firstSetupDone := make(chan error, 1)
	go func() {
		_, submitErr := path.forwardLink.submitWithDeliver(
			make([]byte, 700),
			func([]byte) bool { return true },
		)
		firstSetupDone <- submitErr
	}()
	select {
	case <-firstIngressEntered:
	case <-ctx.Done():
		t.Fatalf("first setup ingress did not reach barrier: %v", ctx.Err())
	}
	if _, err := path.forwardLink.submitWithDeliver(
		make([]byte, 600),
		func([]byte) bool { return true },
	); err != nil {
		t.Fatalf("second setup admission: %v", err)
	}
	setupWhileHeld := path.forwardLink.snapshot()
	if setupWhileHeld.MaximumQueuedPackets < 2 || setupWhileHeld.MaximumQueuedBytes < 1300 {
		t.Fatalf("setup did not establish the larger lifetime high-water mark: %+v", setupWhileHeld)
	}
	close(releaseFirstIngress)
	if err := <-firstSetupDone; err != nil {
		t.Fatalf("first setup admission: %v", err)
	}
	path.forwardLink.beforeIngressForTest = nil

	measurementStart, err := path.beginMeasurement(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const measuredByteCount = 19
	if _, err := path.forwardLink.submitWithDeliver(
		make([]byte, measuredByteCount),
		func([]byte) bool { return true },
	); err != nil {
		t.Fatalf("measured admission: %v", err)
	}
	forward, reverse, err := path.finishMeasurement(ctx, measurementStart)
	if err != nil {
		t.Fatal(err)
	}
	if forward.AdmittedPacketCount != 1 || forward.AdmittedByteCount != measuredByteCount ||
		forward.DeliveredPacketCount != 1 || forward.DeliveredByteCount != measuredByteCount {
		t.Fatalf("measured interval retained setup counters: %+v", forward)
	}
	if forward.MaximumQueuedPackets != 1 || forward.MaximumQueuedBytes != measuredByteCount {
		t.Fatalf("measured interval queue maxima are not exact: %+v", forward)
	}
	if forward.MaximumSubmittedPacketBytes != measuredByteCount {
		t.Fatalf("measured interval packet-size maximum is not exact: %+v", forward)
	}
	if reverse.AdmittedPacketCount != 0 || reverse.AdmittedByteCount != 0 ||
		reverse.DeliveredPacketCount != 0 || reverse.DeliveredByteCount != 0 {
		t.Fatalf("idle reverse interval retained lifetime counters: %+v", reverse)
	}
	lifetime := path.forwardLink.snapshot()
	if lifetime.AdmittedPacketCount != 3 || lifetime.AdmittedByteCount != 1319 ||
		lifetime.MaximumQueuedPackets < 2 || lifetime.MaximumQueuedBytes < 1300 ||
		lifetime.MaximumSubmittedPacketBytes != 700 {
		t.Fatalf("lifetime diagnostic lost setup traffic: %+v", lifetime)
	}
}

// A terminal reverse-link submission between the forward and reverse epoch
// resets must invalidate the whole candidate. Otherwise the later reset absorbs
// that setup drop while the earlier direction begins at a different instant.
func TestTunPathMeasurementBoundaryRetriesCrossResetSubmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	profile := initialNetworkProfiles(20260816)["clean-lan"]
	path, err := newTunPath(ctx, profile, defaultTunResourceProfile())
	if err != nil {
		t.Fatal(err)
	}
	defer path.close()

	resetPassCount := 0
	var injectionErr error
	path.afterMeasurementLinkSnapshotForTest = func(linkIndex int) {
		if linkIndex != 0 {
			return
		}
		resetPassCount += 1
		if resetPassCount == 1 {
			_, injectionErr = path.reverseLink.submit(
				make([]byte, profile.Reverse.OuterMtu+1),
			)
		}
	}
	measurementStart, err := path.beginMeasurement(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if injectionErr != nil {
		t.Fatalf("inject cross-reset reverse submission: %v", injectionErr)
	}
	if resetPassCount != 2 {
		t.Fatalf("calibration reset passes=%d, want 2", resetPassCount)
	}
	if measurementStart.reverseLink.submittedPacketCount != 1 ||
		measurementStart.reverseLink.MtuDropPacketCount != 1 {
		t.Fatalf("accepted reverse baseline lost setup drop: %+v", measurementStart.reverseLink)
	}
	if !stableCalibrationLinkMeasurement(
		measurementStart.forwardLink,
		path.forwardLink.snapshot(),
	) || !stableCalibrationLinkMeasurement(
		measurementStart.reverseLink,
		path.reverseLink.snapshot(),
	) {
		t.Fatal("accepted calibration generation was not stable")
	}

	forward, reverse, err := path.finishMeasurement(ctx, measurementStart)
	if err != nil {
		t.Fatal(err)
	}
	if forward.MtuDropPacketCount != 0 || reverse.MtuDropPacketCount != 0 ||
		forward.MaximumSubmittedPacketBytes != 0 ||
		reverse.MaximumSubmittedPacketBytes != 0 {
		t.Fatalf("cross-reset setup drop leaked into empty interval: forward=%+v reverse=%+v", forward, reverse)
	}
}

// Percentile selection is deterministic and keeps empty vectors explicit.
func summarizeLatencies(latencies []time.Duration) latencyDistribution {
	if len(latencies) == 0 {
		return latencyDistribution{}
	}
	sorted := slices.Clone(latencies)
	slices.Sort(sorted)
	value := func(percentile int) time.Duration {
		index := (percentile*len(sorted) + 99) / 100
		index = min(max(index, 1), len(sorted))
		return sorted[index-1]
	}
	return latencyDistribution{
		P50: value(50),
		P95: value(95),
		P99: value(99),
		Max: sorted[len(sorted)-1],
	}
}

// Useful-byte rates use decimal transport units as required by PERFVAR.md.
func finishWorkloadResult(result workloadResult) workloadResult {
	if 0 < result.Duration {
		result.GoodputMegabytes = float64(result.UsefulByteCount) / 1_000_000 / result.Duration.Seconds()
		result.GoodputGigabits = float64(result.UsefulByteCount*8) / 1_000_000_000 / result.Duration.Seconds()
	}
	return result
}

// The deterministic chunk makes corruption visible without retaining the transfer.
func deterministicPayload() []byte {
	payload := make([]byte, 64*1024)
	for byteIndex := range payload {
		payload[byteIndex] = byte((byteIndex*31 + 17) % 251)
	}
	return payload
}

// The expected digest is streamed with the same final partial-chunk shape.
func deterministicPayloadHash(byteCount int64) string {
	payload := deterministicPayload()
	hash := sha256.New()
	for remaining := byteCount; 0 < remaining; {
		chunk := payload
		if remaining < int64(len(chunk)) {
			chunk = payload[:remaining]
		}
		hash.Write(chunk)
		remaining -= int64(len(chunk))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// A calibration deadline scales with its configured capacity and round trip.
// The outer scenario context remains the final bound for a stalled workload.
func calibrationWorkloadTimeout(profile networkProfile, byteCount int64) time.Duration {
	roundTrip := profile.Forward.BaseDelay +
		profile.Forward.ProcessingDelay +
		profile.Reverse.BaseDelay +
		profile.Reverse.ProcessingDelay
	rateBitsPerSecond := min(
		profile.Forward.RateBitsPerSecond,
		profile.Reverse.RateBitsPerSecond,
	)
	rateDuration := time.Duration(0)
	if 0 < rateBitsPerSecond {
		rateDuration = time.Duration(float64(time.Second) * float64(byteCount*8) / float64(rateBitsPerSecond))
	}
	minimumTimeout := 30 * time.Second
	if perfvarRaceEnabled {
		// The race runtime reduces the userspace gVisor path to about 1.5 Mb/s
		// on the baseline macOS host, so the exact 32 MiB gate can take just over
		// three minutes without a transport stall or packet loss.
		minimumTimeout = 4 * time.Minute
	}
	return max(minimumTimeout, 60*roundTrip, 60*rateDuration)
}

// One or more inner TCP flows verify exact bytes and content in one direction.
func measureTCPWorkload(
	ctx context.Context,
	profile networkProfile,
	resources tunResourceProfile,
	forward bool,
	flowCount int,
	byteCountPerFlow int64,
) (workloadResult, error) {
	return measureTCPWorkloadWithStartHook(
		ctx,
		profile,
		resources,
		forward,
		flowCount,
		byteCountPerFlow,
		nil,
	)
}

// Warmed TCP uses one established connection for an untimed route-local BDP
// followed by the independently hashed and timed measured payload.
func measureWarmedTCPWorkload(
	ctx context.Context,
	profile networkProfile,
	resources tunResourceProfile,
	forward bool,
	warmupByteCount int64,
	measuredByteCount int64,
) (workloadResult, error) {
	return measureTCPWorkloadWithWarmupAndStartHook(
		ctx,
		profile,
		resources,
		forward,
		1,
		warmupByteCount,
		measuredByteCount,
		nil,
		nil,
	)
}

// The optional hook gives cancellation tests an exact post-handshake boundary.
func measureTCPWorkloadWithStartHook(
	ctx context.Context,
	profile networkProfile,
	resources tunResourceProfile,
	forward bool,
	flowCount int,
	byteCountPerFlow int64,
	startHook func(*tunPath) error,
) (workloadResult, error) {
	return measureTCPWorkloadWithWarmupAndStartHook(
		ctx,
		profile,
		resources,
		forward,
		flowCount,
		0,
		byteCountPerFlow,
		nil,
		startHook,
	)
}

// One implementation keeps cold and warmed TCP byte validation identical.
// The start hook runs after a positively acknowledged warmup boundary.
func measureTCPWorkloadWithWarmupAndStartHook(
	ctx context.Context,
	profile networkProfile,
	resources tunResourceProfile,
	forward bool,
	flowCount int,
	warmupByteCountPerFlow int64,
	byteCountPerFlow int64,
	beforeWarmupAckHook func(*tunPath, bool) error,
	startHook func(*tunPath) error,
) (workloadResult, error) {
	return measureTCPWorkloadWithWarmupAndFlowTestSettings(
		ctx,
		profile,
		resources,
		forward,
		flowCount,
		warmupByteCountPerFlow,
		byteCountPerFlow,
		beforeWarmupAckHook,
		startHook,
		nil,
	)
}

// Test settings expose admission without changing the production-facing
// workload helper or any measured payload boundary.
func measureTCPWorkloadWithWarmupAndFlowTestSettings(
	ctx context.Context,
	profile networkProfile,
	resources tunResourceProfile,
	forward bool,
	flowCount int,
	warmupByteCountPerFlow int64,
	byteCountPerFlow int64,
	beforeWarmupAckHook func(*tunPath, bool) error,
	startHook func(*tunPath) error,
	testSettings *workloadTCPFlowTestSettings,
) (workloadResult, error) {
	path, err := newTunPath(ctx, profile, resources)
	if err != nil {
		return workloadResult{}, err
	}
	defer path.close()
	listenerTun := path.right
	dialTun := path.left
	listenerIP := path.endpointAddress(false)
	if !forward {
		listenerTun = path.left
		dialTun = path.right
		listenerIP = path.endpointAddress(true)
	}
	listener, err := listenerTun.ListenTCP(&net.TCPAddr{IP: listenerIP, Port: 0})
	if err != nil {
		return workloadResult{}, err
	}
	expectedHash := deterministicPayloadHash(byteCountPerFlow)
	expectedWarmupHash := deterministicPayloadHash(warmupByteCountPerFlow)
	receiverStart := make(chan struct{})
	var workloadDeadline time.Time
	var receiverStartOnce sync.Once
	startReceivers := func() {
		receiverStartOnce.Do(func() {
			close(receiverStart)
		})
	}
	var flowServerSettings *logicalTCPFlowServerSettings
	if testSettings != nil {
		flowServerSettings = testSettings.flowServerSettings
	}
	flowServer := newLogicalTCPFlowServer(
		ctx,
		listener,
		flowCount,
		func(_ logicalTCPFlowId, connection net.Conn) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-receiverStart:
			}
			stopInterrupt := interruptDeadlineOnContext(ctx, connection)
			defer stopInterrupt()
			if err := connection.SetDeadline(workloadDeadline); err != nil {
				return err
			}
			if 0 < warmupByteCountPerFlow {
				warmupHash := sha256.New()
				readByteCount, readErr := io.CopyN(
					warmupHash,
					connection,
					warmupByteCountPerFlow,
				)
				if readErr != nil {
					return readErr
				}
				if readByteCount != warmupByteCountPerFlow ||
					hex.EncodeToString(warmupHash.Sum(nil)) != expectedWarmupHash {
					return fmt.Errorf("TCP warmup content mismatch bytes=%d", readByteCount)
				}
				if beforeWarmupAckHook != nil {
					if err := beforeWarmupAckHook(path, forward); err != nil {
						return err
					}
				}
				if err := writeWorkloadAll(connection, []byte{1}); err != nil {
					return err
				}
			}
			hash := sha256.New()
			readByteCount, readErr := io.CopyN(hash, connection, byteCountPerFlow)
			if readErr != nil {
				return readErr
			}
			if readByteCount != byteCountPerFlow ||
				hex.EncodeToString(hash.Sum(nil)) != expectedHash {
				return fmt.Errorf("TCP content mismatch bytes=%d", readByteCount)
			}
			return nil
		},
		flowServerSettings,
	)
	defer flowServer.CloseAndWait()
	defer startReceivers()
	if testSettings != nil && testSettings.beforeClientDialHook != nil {
		if err := testSettings.beforeClientDialHook(
			ctx,
			dialTun,
			listener.Addr().String(),
		); err != nil {
			return workloadResult{}, err
		}
	}

	setupStart := time.Now()
	connections := make([]net.Conn, 0, flowCount)
	for flowIndex := range flowCount {
		connection, dialErr := dialTun.DialContext(ctx, "tcp", listener.Addr().String())
		if dialErr != nil {
			for _, opened := range connections {
				_ = opened.Close()
			}
			return workloadResult{}, dialErr
		}
		connections = append(connections, connection)
		if err := writeLogicalTCPFlowPreface(connection, logicalTCPFlowId(flowIndex)); err != nil {
			for _, opened := range connections {
				_ = opened.Close()
			}
			return workloadResult{}, contextBoundWorkloadError(ctx, err)
		}
	}
	if err := flowServer.WaitReady(ctx); err != nil {
		for _, opened := range connections {
			_ = opened.Close()
		}
		return workloadResult{}, contextBoundWorkloadError(ctx, err)
	}
	setupDuration := time.Since(setupStart)

	workloadTimeout := calibrationWorkloadTimeout(
		profile,
		int64(flowCount)*(warmupByteCountPerFlow+byteCountPerFlow),
	)
	workloadDeadline = boundedWorkloadDeadline(ctx, workloadTimeout)
	startReceivers()
	warmupDuration := time.Duration(0)
	if 0 < warmupByteCountPerFlow {
		warmupStart := time.Now()
		var warmupWaitGroup sync.WaitGroup
		warmupErrors := make(chan error, flowCount)
		for _, connection := range connections {
			warmupWaitGroup.Add(1)
			go func() {
				defer warmupWaitGroup.Done()
				stopInterrupt := interruptDeadlineOnContext(ctx, connection)
				defer stopInterrupt()
				if err := connection.SetDeadline(workloadDeadline); err != nil {
					warmupErrors <- err
					return
				}
				if err := writeDeterministicWorkloadPayload(
					ctx,
					connection,
					resources.AppDelay,
					warmupByteCountPerFlow,
				); err != nil {
					warmupErrors <- err
					return
				}
				ack := make([]byte, 1)
				if _, err := io.ReadFull(connection, ack); err != nil {
					warmupErrors <- err
				}
			}()
		}
		warmupWaitGroup.Wait()
		select {
		case warmupErr := <-warmupErrors:
			for _, opened := range connections {
				_ = opened.Close()
			}
			if receiverErr := flowServer.Wait(); receiverErr != nil {
				return workloadResult{}, fmt.Errorf("TCP warmup receiver: %w", receiverErr)
			}
			return workloadResult{}, fmt.Errorf("TCP warmup sender: %w", warmupErr)
		default:
		}
		if !path.waitForTerminalIdle(ctx) {
			for _, opened := range connections {
				_ = opened.Close()
			}
			_ = flowServer.Wait()
			return workloadResult{}, fmt.Errorf("join TCP warmup link boundary: context=%v", ctx.Err())
		}
		warmupDuration = time.Since(warmupStart)
	}
	measurementStart, err := path.beginMeasurement(ctx)
	if err != nil {
		for _, opened := range connections {
			_ = opened.Close()
		}
		_ = flowServer.Wait()
		return workloadResult{}, err
	}
	if startHook != nil {
		if err := startHook(path); err != nil {
			for _, opened := range connections {
				_ = opened.Close()
			}
			_ = flowServer.Wait()
			return workloadResult{}, err
		}
	}

	var memoryBefore runtime.MemStats
	runtime.ReadMemStats(&memoryBefore)
	startTime := time.Now()
	var senderWaitGroup sync.WaitGroup
	senderErrors := make(chan error, flowCount)
	for _, connection := range connections {
		senderWaitGroup.Add(1)
		go func() {
			defer senderWaitGroup.Done()
			defer connection.Close()
			stopInterrupt := interruptDeadlineOnContext(ctx, connection)
			defer stopInterrupt()
			if err := connection.SetDeadline(workloadDeadline); err != nil {
				senderErrors <- err
				return
			}
			if err := writeDeterministicWorkloadPayload(
				ctx,
				connection,
				resources.AppDelay,
				byteCountPerFlow,
			); err != nil {
				senderErrors <- err
				return
			}
		}()
	}
	senderWaitGroup.Wait()
	receiverErr := flowServer.Wait()
	duration := time.Since(startTime)
	if err := ctx.Err(); err != nil {
		return workloadResult{}, fmt.Errorf(
			"TCP context: %w; forward=%+v reverse=%+v",
			err,
			path.forwardLink.snapshot(),
			path.reverseLink.snapshot(),
		)
	}
	select {
	case senderErr := <-senderErrors:
		return workloadResult{}, fmt.Errorf(
			"TCP sender: %w; forward=%+v reverse=%+v",
			contextBoundWorkloadError(ctx, senderErr),
			path.forwardLink.snapshot(),
			path.reverseLink.snapshot(),
		)
	default:
	}
	if receiverErr != nil {
		return workloadResult{}, fmt.Errorf(
			"TCP receiver: %w; forward=%+v reverse=%+v",
			contextBoundWorkloadError(ctx, receiverErr),
			path.forwardLink.snapshot(),
			path.reverseLink.snapshot(),
		)
	}
	forwardLink, reverseLink, err := path.finishMeasurement(ctx, measurementStart)
	if err != nil {
		return workloadResult{}, fmt.Errorf("finish measured TCP: %w", err)
	}
	var memoryAfter runtime.MemStats
	runtime.ReadMemStats(&memoryAfter)
	result := workloadResult{
		UsefulByteCount:        int64(flowCount) * byteCountPerFlow,
		WarmupByteCount:        int64(flowCount) * warmupByteCountPerFlow,
		WarmupDuration:         warmupDuration,
		Duration:               duration,
		SetupDuration:          setupDuration,
		AllocatedByteCount:     memoryAfter.TotalAlloc - memoryBefore.TotalAlloc,
		AllocationCount:        memoryAfter.Mallocs - memoryBefore.Mallocs,
		GarbageCollectionCount: memoryAfter.NumGC - memoryBefore.NumGC,
		GarbageCollectionPause: time.Duration(memoryAfter.PauseTotalNs - memoryBefore.PauseTotalNs),
		ContentHash:            expectedHash,
		ForwardLink:            forwardLink,
		ReverseLink:            reverseLink,
	}
	return finishWorkloadResult(result), nil
}

// Exact writes reject a zero-progress connection instead of spinning forever.
func writeWorkloadAll(connection net.Conn, payload []byte) error {
	for 0 < len(payload) {
		writtenByteCount, err := connection.Write(payload)
		if 0 < writtenByteCount {
			payload = payload[writtenByteCount:]
		}
		if err != nil {
			return err
		}
		if writtenByteCount == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

// Deterministic streaming reuses one bounded chunk for warmup and measurement.
func writeDeterministicWorkloadPayload(
	ctx context.Context,
	connection net.Conn,
	appDelay time.Duration,
	byteCount int64,
) error {
	payload := deterministicPayload()
	for remaining := byteCount; 0 < remaining; {
		chunk := payload
		if remaining < int64(len(chunk)) {
			chunk = payload[:remaining]
		}
		if err := waitForWorkloadDelay(ctx, appDelay); err != nil {
			return err
		}
		if err := writeWorkloadAll(connection, chunk); err != nil {
			return err
		}
		remaining -= int64(len(chunk))
	}
	return nil
}

// A deterministic admission harness leaves identified client sockets dormant
// at the server preface read and observes their final unclaimed join.
type workloadDormantCandidateHarness struct {
	candidateCount int
	prefaceReads   chan net.Conn
	joined         chan bool
	readErrors     chan error
	claimedFlows   atomic.Int64

	stateLock         sync.Mutex
	serverConnections map[net.Conn]bool
	clientConnections []net.Conn
	clientReaders     sync.WaitGroup
}

// Buffered callback channels keep server lifecycle hooks independent from the
// test goroutine while the production dial is deliberately withheld.
func newWorkloadDormantCandidateHarness(candidateCount int) *workloadDormantCandidateHarness {
	return &workloadDormantCandidateHarness{
		candidateCount:    candidateCount,
		prefaceReads:      make(chan net.Conn, 32+8*candidateCount),
		joined:            make(chan bool, candidateCount),
		readErrors:        make(chan error, candidateCount),
		serverConnections: map[net.Conn]bool{},
	}
}

// Hooks inject every dormant socket before the measured helper starts its
// identified dials, then count only identities the server actually claims.
func (self *workloadDormantCandidateHarness) settings() *workloadTCPFlowTestSettings {
	return &workloadTCPFlowTestSettings{
		flowServerSettings: &logicalTCPFlowServerSettings{
			beforePrefaceReadForTest: func(connection net.Conn) {
				self.prefaceReads <- connection
			},
			afterClaimForTest: func(_ net.Conn, _ logicalTCPFlowId, claimed bool) {
				if claimed {
					self.claimedFlows.Add(1)
				}
			},
			beforeCandidateDoneForTest: func(
				connection net.Conn,
				_ logicalTCPFlowId,
				claimed bool,
			) {
				isInjected := func() bool {
					self.stateLock.Lock()
					defer self.stateLock.Unlock()
					return self.serverConnections[connection]
				}()
				if isInjected {
					self.joined <- claimed
				}
			},
		},
		beforeClientDialHook: self.inject,
	}
}

// Each returned TUN socket is matched to the server connection already
// blocked on its missing preface, without sleeps or scheduler assumptions.
func (self *workloadDormantCandidateHarness) inject(
	ctx context.Context,
	dialTun *clientconnect.Tun,
	address string,
) error {
	for range self.candidateCount {
		connection, err := dialTun.DialContext(ctx, "tcp", address)
		if err != nil {
			return fmt.Errorf("dial dormant workload candidate: %w", err)
		}
		var serverConnection net.Conn
		for serverConnection == nil {
			select {
			case candidate := <-self.prefaceReads:
				if candidate.RemoteAddr().String() == connection.LocalAddr().String() {
					serverConnection = candidate
				}
			case <-ctx.Done():
				_ = connection.Close()
				return fmt.Errorf("wait for dormant workload candidate preface read: %w", ctx.Err())
			}
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.serverConnections[serverConnection] = true
			self.clientConnections = append(self.clientConnections, connection)
		}()
		self.clientReaders.Add(1)
		go func() {
			defer self.clientReaders.Done()
			buffer := make([]byte, 1)
			_, readErr := connection.Read(buffer)
			self.readErrors <- readErr
		}()
	}
	return nil
}

// Client sockets remain owned by the harness until the workload has returned.
func (self *workloadDormantCandidateHarness) close() {
	connections := func() []net.Conn {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return slices.Clone(self.clientConnections)
	}()
	for _, connection := range connections {
		_ = connection.Close()
	}
	self.clientReaders.Wait()
}

// Successful workload completion must include every losing handler join and
// peer-visible close, and none may have reached the authoritative handler.
func (self *workloadDormantCandidateHarness) assertJoined(
	t *testing.T,
	ctx context.Context,
) {
	t.Helper()
	for candidateIndex := range self.candidateCount {
		select {
		case claimed := <-self.joined:
			if claimed {
				t.Fatalf("dormant candidate %d claimed a logical flow", candidateIndex)
			}
		case <-ctx.Done():
			t.Fatalf("dormant candidate %d did not join: %v", candidateIndex, ctx.Err())
		}
	}
	for candidateIndex := range self.candidateCount {
		select {
		case readErr := <-self.readErrors:
			if readErr == nil {
				t.Fatalf("dormant candidate %d read unexpectedly succeeded", candidateIndex)
			}
		case <-ctx.Done():
			t.Fatalf("dormant candidate %d was not closed: %v", candidateIndex, ctx.Err())
		}
	}
}

// The logical quota counts three identified winners even when three dormant
// candidates reached preface reads first; receiver hooks run for winners only.
func TestTCPWorkloadClaimsAllFlowsAfterDormantAcceptedCandidates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const flowCount = 3
	const warmupByteCount = 16*1024 + 1
	const measuredByteCount = 32*1024 + 3
	harness := newWorkloadDormantCandidateHarness(flowCount)
	defer harness.close()
	var warmupHookCount atomic.Int64
	var startHookCount atomic.Int64
	result, err := measureTCPWorkloadWithWarmupAndFlowTestSettings(
		ctx,
		initialNetworkProfiles(20260811)["clean-lan"],
		defaultTunResourceProfile(),
		true,
		flowCount,
		warmupByteCount,
		measuredByteCount,
		func(*tunPath, bool) error {
			if hookCount := warmupHookCount.Add(1); flowCount < hookCount {
				return fmt.Errorf("warmup hook count=%d exceeds flow quota=%d", hookCount, flowCount)
			}
			if claimedFlowCount := harness.claimedFlows.Load(); claimedFlowCount != flowCount {
				return fmt.Errorf("claimed flows=%d at warmup hook, want=%d", claimedFlowCount, flowCount)
			}
			return nil
		},
		func(*tunPath) error {
			startHookCount.Add(1)
			if warmupCount := warmupHookCount.Load(); warmupCount != flowCount {
				return fmt.Errorf("warmup hooks=%d at measured start, want=%d", warmupCount, flowCount)
			}
			return nil
		},
		harness.settings(),
	)
	if err != nil {
		t.Fatal(err)
	}
	harness.assertJoined(t, ctx)
	if claimedFlowCount := harness.claimedFlows.Load(); claimedFlowCount != flowCount {
		t.Fatalf("claimed flows=%d, want=%d", claimedFlowCount, flowCount)
	}
	if warmupCount := warmupHookCount.Load(); warmupCount != flowCount {
		t.Fatalf("warmup hook count=%d, want=%d", warmupCount, flowCount)
	}
	if startCount := startHookCount.Load(); startCount != 1 {
		t.Fatalf("start hook count=%d, want=1", startCount)
	}
	if result.UsefulByteCount != flowCount*measuredByteCount ||
		result.WarmupByteCount != flowCount*warmupByteCount ||
		result.ContentHash != deterministicPayloadHash(measuredByteCount) {
		t.Fatalf("multi-flow result=%+v", result)
	}
}

// A non-chunk-aligned warmup proves the receiver resets its hash at an exact
// same-connection barrier before measuring either simulated direction.
func TestWarmedTCPWorkloadSeparatesWarmupAndMeasuredPayload(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	profile := initialNetworkProfiles(20260810)["clean-lan"]
	const warmupByteCount = 512*1024 + 1
	const measuredByteCount = 64*1024 + 3
	for _, forward := range []bool{true, false} {
		startBoundaryCount := 0
		result, err := measureTCPWorkloadWithWarmupAndStartHook(
			ctx,
			profile,
			defaultTunResourceProfile(),
			forward,
			1,
			warmupByteCount,
			measuredByteCount,
			nil,
			func(*tunPath) error {
				startBoundaryCount += 1
				return nil
			},
		)
		if err != nil {
			t.Fatalf("forward=%t warmed TCP: %v", forward, err)
		}
		if startBoundaryCount != 1 {
			t.Errorf("forward=%t measured start count=%d", forward, startBoundaryCount)
		}
		if result.WarmupByteCount != warmupByteCount || result.WarmupDuration <= 0 {
			t.Errorf("forward=%t warmup result=%+v", forward, result)
		}
		if result.UsefulByteCount != measuredByteCount ||
			result.ContentHash != deterministicPayloadHash(measuredByteCount) {
			t.Errorf("forward=%t measured result=%+v", forward, result)
		}
		dataLink := result.ForwardLink
		if !forward {
			dataLink = result.ReverseLink
		}
		if uint64(warmupByteCount) <= dataLink.AdmittedByteCount {
			t.Errorf(
				"forward=%t measured link retained warmup bytes: warmup=%d link=%+v",
				forward,
				warmupByteCount,
				dataLink,
			)
		}
	}
}

// A packet admitted after the first all-links-idle observation forces an exact
// generation retry, so the untunneled measured phase cannot start early.
func TestWarmedTCPWorkloadRetriesWarmupLinkGeneration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	profile := initialNetworkProfiles(20260811)["clean-lan"]
	for _, forward := range []bool{true, false} {
		retryReached := make(chan struct{})
		releaseRetry := make(chan struct{})
		measuredStarted := make(chan struct{})
		injectionDone := make(chan error, 1)
		idlePassCount := 0
		var releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(releaseRetry) })
		workloadDone := make(chan struct {
			result workloadResult
			err    error
		}, 1)
		go func() {
			result, err := measureTCPWorkloadWithWarmupAndStartHook(
				ctx,
				profile,
				defaultTunResourceProfile(),
				forward,
				1,
				64*1024+1,
				128*1024+3,
				func(path *tunPath, forward bool) error {
					dataLink := path.forwardLink
					if !forward {
						dataLink = path.reverseLink
					}
					path.afterLinksIdleForTest = func() {
						idlePassCount += 1
						if idlePassCount == 1 {
							_, err := dataLink.submit(make([]byte, 100))
							injectionDone <- err
							return
						}
						if idlePassCount == 2 {
							close(retryReached)
							<-releaseRetry
						}
					}
					return nil
				},
				func(*tunPath) error {
					close(measuredStarted)
					return nil
				},
			)
			workloadDone <- struct {
				result workloadResult
				err    error
			}{result: result, err: err}
		}()
		select {
		case <-retryReached:
		case <-ctx.Done():
			t.Fatalf("forward=%t warmup link generation was not retried: %v", forward, ctx.Err())
		}
		select {
		case <-measuredStarted:
			t.Fatalf("forward=%t measured body started before the warmup link generation joined", forward)
		default:
		}
		select {
		case completion := <-workloadDone:
			t.Fatalf("forward=%t workload returned before the warmup link generation joined: %+v", forward, completion)
		default:
		}
		releaseOnce.Do(func() { close(releaseRetry) })
		select {
		case completion := <-workloadDone:
			if completion.err != nil {
				t.Fatalf("forward=%t warmed workload: %v", forward, completion.err)
			}
			if completion.result.WarmupByteCount != 64*1024+1 ||
				completion.result.UsefulByteCount != 128*1024+3 {
				t.Fatalf("forward=%t warmed result=%+v", forward, completion.result)
			}
		case <-ctx.Done():
			t.Fatalf("forward=%t workload did not complete after link release: %v", forward, ctx.Err())
		}
		select {
		case injectionErr := <-injectionDone:
			if injectionErr != nil {
				t.Fatalf("forward=%t injected generation: %v", forward, injectionErr)
			}
		case <-ctx.Done():
			t.Fatalf("forward=%t injected generation did not terminate: %v", forward, ctx.Err())
		}
	}
}

// Sequence metadata gives UDP exact loss, duplication, reorder, and latency accounting.
func measureUDPWorkload(
	ctx context.Context,
	profile networkProfile,
	resources tunResourceProfile,
	duration time.Duration,
	offeredBitsPerSecond int64,
	payloadByteCount int,
) (workloadResult, error) {
	return measureUDPWorkloadWithStartHook(
		ctx,
		profile,
		resources,
		duration,
		offeredBitsPerSecond,
		payloadByteCount,
		nil,
	)
}

// The optional hook gives drain regressions an exact post-setup carrier edge.
func measureUDPWorkloadWithStartHook(
	ctx context.Context,
	profile networkProfile,
	resources tunResourceProfile,
	duration time.Duration,
	offeredBitsPerSecond int64,
	payloadByteCount int,
	startHook func(*tunPath) error,
) (workloadResult, error) {
	path, err := newTunPath(ctx, profile, resources)
	if err != nil {
		return workloadResult{}, err
	}
	defer path.close()
	listener, err := path.right.ListenUDP(&net.UDPAddr{IP: path.endpointAddress(false), Port: 0})
	if err != nil {
		return workloadResult{}, err
	}
	defer listener.Close()
	connection, err := path.left.DialContext(ctx, "udp", listener.LocalAddr().String())
	if err != nil {
		return workloadResult{}, err
	}
	defer connection.Close()
	if payloadByteCount < 24 {
		return workloadResult{}, fmt.Errorf("UDP payload %d is smaller than its 24-byte header", payloadByteCount)
	}

	packetInterval := time.Duration(float64(time.Second) * float64(payloadByteCount*8) / float64(offeredBitsPerSecond))
	targetPacketCount := int64(duration / packetInterval)
	receivedSequences := make(map[uint64]bool)
	latencies := []time.Duration{}
	var stateLock sync.Mutex
	var deliveredPacketCount int64
	var duplicatePacketCount int64
	var reorderedPacketCount int64
	var corruptPacketCount int64
	var highestSequence uint64
	receiverDone := make(chan struct{})
	receiverProgress := make(chan struct{}, 1)
	go func() {
		defer close(receiverDone)
		buffer := make([]byte, 64*1024)
		for {
			readByteCount, _, readErr := listener.ReadFrom(buffer)
			if readErr != nil {
				return
			}
			if readByteCount != payloadByteCount {
				stateLock.Lock()
				corruptPacketCount += 1
				stateLock.Unlock()
				select {
				case receiverProgress <- struct{}{}:
				default:
				}
				continue
			}
			sequence := binary.BigEndian.Uint64(buffer[0:8])
			sendUnixNano := int64(binary.BigEndian.Uint64(buffer[8:16]))
			checksum := binary.BigEndian.Uint64(buffer[16:24])
			if checksum != sequence^uint64(sendUnixNano) {
				stateLock.Lock()
				corruptPacketCount += 1
				stateLock.Unlock()
				select {
				case receiverProgress <- struct{}{}:
				default:
				}
				continue
			}
			stateLock.Lock()
			if receivedSequences[sequence] {
				duplicatePacketCount += 1
			} else {
				receivedSequences[sequence] = true
				deliveredPacketCount += 1
				if sequence < highestSequence {
					reorderedPacketCount += 1
				} else {
					highestSequence = sequence
				}
				latencies = append(latencies, time.Since(time.Unix(0, sendUnixNano)))
			}
			stateLock.Unlock()
			select {
			case receiverProgress <- struct{}{}:
			default:
			}
		}
	}()
	receiverStopped := false
	stopReceiver := func() {
		if receiverStopped {
			return
		}
		listener.Close()
		<-receiverDone
		receiverStopped = true
	}
	defer stopReceiver()

	payload := make([]byte, payloadByteCount)
	measurementStart, err := path.beginMeasurement(ctx)
	if err != nil {
		return workloadResult{}, err
	}
	if startHook != nil {
		if err := startHook(path); err != nil {
			return workloadResult{}, err
		}
	}
	linkBefore := measurementStart.forwardLink
	submittedPacketCountBefore := path.forwardLink.submittedPackets.Load()
	startTime := time.Now()
	for packetIndex := int64(0); packetIndex < targetPacketCount; packetIndex += 1 {
		targetTime := startTime.Add(time.Duration(packetIndex) * packetInterval)
		if wait := time.Until(targetTime); 0 < wait {
			time.Sleep(wait)
		}
		sequence := uint64(packetIndex + 1)
		sendUnixNano := time.Now().UnixNano()
		binary.BigEndian.PutUint64(payload[0:8], sequence)
		binary.BigEndian.PutUint64(payload[8:16], uint64(sendUnixNano))
		binary.BigEndian.PutUint64(payload[16:24], sequence^uint64(sendUnixNano))
		if _, writeErr := connection.Write(payload); writeErr != nil {
			return workloadResult{}, writeErr
		}
	}
	sendDuration := time.Since(startTime)
	targetSubmittedPacketCount := submittedPacketCountBefore + uint64(targetPacketCount)
	if !path.forwardLink.waitForSubmissionCount(ctx, targetSubmittedPacketCount) {
		return workloadResult{}, fmt.Errorf(
			"UDP network submitted %d/%d packets before context ended: %w",
			path.forwardLink.submittedPackets.Load()-submittedPacketCountBefore,
			targetPacketCount,
			ctx.Err(),
		)
	}
	if !path.forwardLink.waitIdle(ctx) {
		return workloadResult{}, fmt.Errorf(
			"UDP network did not reach a terminal delivery or drop for every admitted packet: %w",
			ctx.Err(),
		)
	}
	linkAfter := path.forwardLink.snapshot()
	targetReceivedPacketCount := linkAfter.DeliveredPacketCount - linkBefore.DeliveredPacketCount
	waitForReceiver := func() error {
		for {
			stateLock.Lock()
			receivedPacketCount := uint64(deliveredPacketCount + duplicatePacketCount + corruptPacketCount)
			stateLock.Unlock()
			if targetReceivedPacketCount <= receivedPacketCount {
				return nil
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf(
					"UDP receiver processed %d/%d carrier deliveries: %w",
					receivedPacketCount,
					targetReceivedPacketCount,
					ctx.Err(),
				)
			case <-receiverDone:
				return fmt.Errorf(
					"UDP receiver ended after %d/%d carrier deliveries",
					receivedPacketCount,
					targetReceivedPacketCount,
				)
			case <-receiverProgress:
			}
		}
	}
	if err := waitForReceiver(); err != nil {
		return workloadResult{}, err
	}
	stopReceiver()
	forwardLink, reverseLink, err := path.finishMeasurement(ctx, measurementStart)
	if err != nil {
		return workloadResult{}, fmt.Errorf("finish measured UDP: %w", err)
	}
	stateLock.Lock()
	result := workloadResult{
		UsefulByteCount:      deliveredPacketCount * int64(payloadByteCount),
		OfferedPacketCount:   targetPacketCount,
		DeliveredPacketCount: deliveredPacketCount,
		DuplicatePacketCount: duplicatePacketCount,
		ReorderedPacketCount: reorderedPacketCount,
		CorruptPacketCount:   corruptPacketCount,
		Duration:             sendDuration,
		Latency:              summarizeLatencies(latencies),
		ForwardLink:          forwardLink,
		ReverseLink:          reverseLink,
	}
	stateLock.Unlock()
	return finishWorkloadResult(result), nil
}

// A short-lived certificate keeps inner QUIC hermetic and production-like.
func newWorkloadTlsConfigs() (*tls.Config, *tls.Config, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "perfvar.invalid"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"perfvar.invalid"},
	}
	certificateBytes, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, nil, err
	}
	certificate := tls.Certificate{
		Certificate: [][]byte{certificateBytes},
		PrivateKey:  privateKey,
	}
	serverConfig := &tls.Config{
		Certificates: []tls.Certificate{certificate},
		NextProtos:   []string{"perfvar-quic"},
		MinVersion:   tls.VersionTLS13,
	}
	clientConfig := &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         "perfvar.invalid",
		NextProtos:         []string{"perfvar-quic"},
		MinVersion:         tls.VersionTLS13,
	}
	return serverConfig, clientConfig, nil
}

// Inner QUIC runs above simulated UDP and verifies an exact stream hash.
func measureQUICWorkload(
	ctx context.Context,
	profile networkProfile,
	resources tunResourceProfile,
	byteCount int64,
) (workloadResult, error) {
	return measureQUICWorkloadWithStartHook(ctx, profile, resources, byteCount, nil)
}

// The optional hook runs after the first stream write has entered QUIC.
func measureQUICWorkloadWithStartHook(
	ctx context.Context,
	profile networkProfile,
	resources tunResourceProfile,
	byteCount int64,
	startHook func(*tunPath) error,
) (workloadResult, error) {
	path, err := newTunPath(ctx, profile, resources)
	if err != nil {
		return workloadResult{}, err
	}
	defer path.close()
	serverPacketConn, err := path.right.ListenUDP(&net.UDPAddr{IP: path.endpointAddress(false), Port: 0})
	if err != nil {
		return workloadResult{}, err
	}
	clientPacketConn, err := path.left.ListenUDP(&net.UDPAddr{IP: path.endpointAddress(true), Port: 0})
	if err != nil {
		serverPacketConn.Close()
		return workloadResult{}, err
	}
	serverTlsConfig, clientTlsConfig, err := newWorkloadTlsConfigs()
	if err != nil {
		serverPacketConn.Close()
		clientPacketConn.Close()
		return workloadResult{}, err
	}
	quicConfig := &quic.Config{
		HandshakeIdleTimeout: 15 * time.Second,
		MaxIdleTimeout:       30 * time.Second,
		InitialPacketSize:    uint16(min(profile.InnerMtu, 1400)),
	}
	serverTransport := &quic.Transport{Conn: serverPacketConn}
	listener, err := serverTransport.ListenEarly(serverTlsConfig, quicConfig)
	if err != nil {
		serverTransport.Close()
		clientPacketConn.Close()
		return workloadResult{}, err
	}
	defer listener.Close()
	defer serverTransport.Close()
	expectedHash := deterministicPayloadHash(byteCount)
	workloadDeadline := boundedWorkloadDeadline(
		ctx,
		calibrationWorkloadTimeout(profile, byteCount),
	)
	serverResult := make(chan error, 1)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		connection, acceptErr := listener.Accept(ctx)
		if acceptErr != nil {
			serverResult <- acceptErr
			return
		}
		stopConnectionInterrupt := context.AfterFunc(ctx, func() {
			_ = connection.CloseWithError(0, "workload context canceled")
		})
		defer stopConnectionInterrupt()
		stream, streamErr := connection.AcceptStream(ctx)
		if streamErr != nil {
			serverResult <- streamErr
			return
		}
		stopStreamInterrupt := interruptDeadlineOnContext(ctx, stream)
		defer stopStreamInterrupt()
		if deadlineErr := stream.SetDeadline(workloadDeadline); deadlineErr != nil {
			serverResult <- deadlineErr
			return
		}
		hash := sha256.New()
		readByteCount, readErr := io.CopyN(hash, stream, byteCount)
		if readErr != nil {
			serverResult <- readErr
			return
		}
		if readByteCount != byteCount || hex.EncodeToString(hash.Sum(nil)) != expectedHash {
			serverResult <- fmt.Errorf("QUIC content mismatch bytes=%d", readByteCount)
			return
		}
		serverResult <- nil
	}()

	clientTransport := &quic.Transport{Conn: clientPacketConn}
	defer clientTransport.Close()
	setupStart := time.Now()
	connection, err := clientTransport.DialEarly(ctx, serverPacketConn.LocalAddr(), clientTlsConfig, quicConfig)
	if err != nil {
		return workloadResult{}, err
	}
	defer connection.CloseWithError(0, "")
	stopConnectionInterrupt := context.AfterFunc(ctx, func() {
		_ = connection.CloseWithError(0, "workload context canceled")
	})
	defer stopConnectionInterrupt()
	stream, err := connection.OpenStreamSync(ctx)
	if err != nil {
		return workloadResult{}, err
	}
	stopStreamInterrupt := interruptDeadlineOnContext(ctx, stream)
	defer stopStreamInterrupt()
	if err := stream.SetDeadline(workloadDeadline); err != nil {
		return workloadResult{}, err
	}
	setupDuration := time.Since(setupStart)
	measurementStart, err := path.beginMeasurement(ctx)
	if err != nil {
		return workloadResult{}, err
	}
	payload := deterministicPayload()
	startTime := time.Now()
	hookCalled := false
	for remaining := byteCount; 0 < remaining; {
		chunk := payload
		if remaining < int64(len(chunk)) {
			chunk = payload[:remaining]
		}
		writtenByteCount, writeErr := stream.Write(chunk)
		remaining -= int64(writtenByteCount)
		if writeErr != nil {
			return workloadResult{}, contextBoundWorkloadError(ctx, writeErr)
		}
		if !hookCalled && startHook != nil {
			hookCalled = true
			if err := startHook(path); err != nil {
				return workloadResult{}, err
			}
		}
	}
	if err := stream.Close(); err != nil {
		return workloadResult{}, contextBoundWorkloadError(ctx, err)
	}
	select {
	case err := <-serverResult:
		if err != nil {
			return workloadResult{}, contextBoundWorkloadError(ctx, err)
		}
	case <-ctx.Done():
		return workloadResult{}, ctx.Err()
	}
	duration := time.Since(startTime)
	select {
	case <-serverDone:
	case <-ctx.Done():
		return workloadResult{}, ctx.Err()
	}
	_ = connection.CloseWithError(0, "")
	_ = listener.Close()
	_ = clientTransport.Close()
	_ = serverTransport.Close()
	forwardLink, reverseLink, err := path.finishMeasurement(ctx, measurementStart)
	if err != nil {
		return workloadResult{}, fmt.Errorf("finish measured QUIC: %w", err)
	}
	return finishWorkloadResult(workloadResult{
		UsefulByteCount: byteCount,
		Duration:        duration,
		SetupDuration:   setupDuration,
		ContentHash:     expectedHash,
		ForwardLink:     forwardLink,
		ReverseLink:     reverseLink,
	}), nil
}

// Web-like traffic measures fresh connection setup, first byte, and exact bodies.
func measureWebWorkload(
	ctx context.Context,
	profile networkProfile,
	resources tunResourceProfile,
) (workloadResult, error) {
	path, err := newTunPath(ctx, profile, resources)
	if err != nil {
		return workloadResult{}, err
	}
	defer path.close()
	listener, err := path.right.ListenTCP(&net.TCPAddr{IP: path.endpointAddress(false), Port: 0})
	if err != nil {
		return workloadResult{}, err
	}
	defer listener.Close()
	smallBody := bytes.Repeat([]byte("s"), 16*1024)
	mediumBody := bytes.Repeat([]byte("m"), 512*1024)
	handler := http.NewServeMux()
	handler.HandleFunc("/small", func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write(smallBody)
	})
	handler.HandleFunc("/medium", func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write(mediumBody)
	})
	httpServer := &http.Server{Handler: handler}
	defer httpServer.Close()
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		_ = httpServer.Serve(listener)
	}()
	transport := &http.Transport{
		DialContext:       path.left.DialContext,
		DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}

	requestBodies := [][]byte{smallBody, mediumBody, smallBody}
	requestPaths := []string{"/small", "/medium", "/small"}
	latencies := []time.Duration{}
	var firstByteTotal time.Duration
	var usefulByteCount int64
	measurementStart, err := path.beginMeasurement(ctx)
	if err != nil {
		return workloadResult{}, err
	}
	startTime := time.Now()
	for requestIndex, requestPath := range requestPaths {
		requestStart := time.Now()
		firstByteTime := time.Time{}
		request, err := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			fmt.Sprintf("http://%s%s", listener.Addr(), requestPath),
			nil,
		)
		if err != nil {
			return workloadResult{}, err
		}
		request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{
			GotFirstResponseByte: func() {
				firstByteTime = time.Now()
			},
		}))
		response, err := client.Do(request)
		if err != nil {
			return workloadResult{}, err
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil {
			return workloadResult{}, readErr
		}
		if response.StatusCode != http.StatusOK || !bytes.Equal(body, requestBodies[requestIndex]) {
			return workloadResult{}, fmt.Errorf("web response %d failed integrity", requestIndex)
		}
		completion := time.Since(requestStart)
		latencies = append(latencies, completion)
		if !firstByteTime.IsZero() {
			firstByteTotal += firstByteTime.Sub(requestStart)
		}
		usefulByteCount += int64(len(body))
	}
	duration := time.Since(startTime)
	transport.CloseIdleConnections()
	if err := httpServer.Close(); err != nil {
		return workloadResult{}, err
	}
	select {
	case <-serverDone:
	case <-ctx.Done():
		return workloadResult{}, ctx.Err()
	}
	forwardLink, reverseLink, err := path.finishMeasurement(ctx, measurementStart)
	if err != nil {
		return workloadResult{}, fmt.Errorf("finish measured web workload: %w", err)
	}
	return finishWorkloadResult(workloadResult{
		UsefulByteCount: usefulByteCount,
		Duration:        duration,
		TimeToFirstByte: firstByteTotal / time.Duration(len(requestPaths)),
		Latency:         summarizeLatencies(latencies),
		ForwardLink:     forwardLink,
		ReverseLink:     reverseLink,
	}), nil
}

const minimumLatencyProbeSuccessCount = 3

const (
	// A fixed offered rate makes two carrier variants carry the same interactive
	// demand. The former closed loop issued fewer probes when a path timed out,
	// which made that path look cheaper during the bulk comparison.
	loadedLatencyProbeMinimumInterval = time.Millisecond
	loadedLatencyProbeMaximumInterval = 500 * time.Millisecond
	loadedLatencyProbeReadInterval    = 100 * time.Millisecond
	loadedLatencyProbeTargetCount     = 12
)

var errInsufficientLatencyProbeSamples = errors.New("insufficient successful latency probes")

var errLoadedLatencyProbeIncomplete = errors.New("loaded latency probe incomplete when bulk ended")

// Targets a stable sample count on fast paths while capping impaired paths at
// two offered probes per second so the measurement is not its own bottleneck.
func loadedLatencyProbeIntervalForRate(
	bulkByteCount int64,
	rateBitsPerSecond int64,
) time.Duration {
	if bulkByteCount <= 0 || rateBitsPerSecond <= 0 {
		return loadedLatencyProbeMaximumInterval
	}
	expectedDuration := time.Duration(
		float64(time.Second) * float64(bulkByteCount*8) /
			float64(rateBitsPerSecond),
	)
	return min(
		loadedLatencyProbeMaximumInterval,
		max(
			loadedLatencyProbeMinimumInterval,
			expectedDuration/loadedLatencyProbeTargetCount,
		),
	)
}

// Probe accounting keeps timeouts visible instead of silently shrinking a
// percentile sample until it no longer represents the selected phase.
type latencyProbeSamples struct {
	latencies    []time.Duration
	attemptCount int
	failureCount int
	firstFailure error
}

// Every attempted probe contributes either one latency or one explicit failure.
func (self *latencyProbeSamples) add(latency time.Duration, err error) {
	self.attemptCount += 1
	if err != nil {
		self.failureCount += 1
		if self.firstFailure == nil {
			self.firstFailure = err
		}
		return
	}
	self.latencies = append(self.latencies, latency)
}

// Three successful observations are the minimum useful percentile sample.
func (self latencyProbeSamples) validate(phase string) error {
	if minimumLatencyProbeSuccessCount <= len(self.latencies) {
		return nil
	}
	reason := self.firstFailure
	if reason == nil {
		reason = errInsufficientLatencyProbeSamples
	}
	return fmt.Errorf(
		"%s latency probes succeeded %d/%d, need at least %d: %w",
		phase,
		len(self.latencies),
		self.attemptCount,
		minimumLatencyProbeSuccessCount,
		reason,
	)
}

// Probe counts and summaries are copied even when validation rejects a phase.
func applyLatencyProbeSamples(
	result *workloadResult,
	idle latencyProbeSamples,
	loaded latencyProbeSamples,
	postLoad latencyProbeSamples,
) {
	result.IdleLatency = summarizeLatencies(idle.latencies)
	result.LoadedLatency = summarizeLatencies(loaded.latencies)
	result.PostLoadLatency = summarizeLatencies(postLoad.latencies)
	result.IdleProbeAttemptCount = idle.attemptCount
	result.IdleProbeSuccessCount = len(idle.latencies)
	result.IdleProbeFailureCount = idle.failureCount
	result.LoadedProbeAttemptCount = loaded.attemptCount
	result.LoadedProbeSuccessCount = len(loaded.latencies)
	result.LoadedProbeFailureCount = loaded.failureCount
	result.PostLoadProbeAttemptCount = postLoad.attemptCount
	result.PostLoadProbeSuccessCount = len(postLoad.latencies)
	result.PostLoadProbeFailureCount = postLoad.failureCount
}

// Every latency phase must retain enough successful probes for percentiles.
func validateLatencyProbeSamples(
	idle latencyProbeSamples,
	loaded latencyProbeSamples,
	postLoad latencyProbeSamples,
) error {
	for _, phase := range []struct {
		name    string
		samples latencyProbeSamples
	}{
		{name: "idle", samples: idle},
		{name: "loaded", samples: loaded},
		{name: "post-load", samples: postLoad},
	} {
		if err := phase.samples.validate(phase.name); err != nil {
			return err
		}
	}
	return nil
}

// Keep the three phases in disjoint portions of the wire sequence space. A
// low-rate or race-instrumented bulk transfer can offer thousands of loaded
// probes; small decimal offsets (formerly 1, 1,000, and 2,000) eventually
// overlapped and made a late loaded reply look like a corrupt post-load probe.
const (
	latencyProbeIdleStartSequence     uint64 = 1
	latencyProbeLoadedStartSequence   uint64 = 1 << 32
	latencyProbePostLoadStartSequence uint64 = 1 << 63
)

// One datagram probe returns its same-process round-trip latency or an error.
func runLatencyProbe(
	ctx context.Context,
	connection net.Conn,
	sequence uint64,
	timeout time.Duration,
) (time.Duration, error) {
	var packet [32]byte
	binary.BigEndian.PutUint64(packet[:], sequence)
	startTime := time.Now()
	stopInterrupt := interruptDeadlineOnContext(ctx, connection)
	defer stopInterrupt()
	if err := connection.SetDeadline(boundedWorkloadDeadline(ctx, timeout)); err != nil {
		return 0, contextBoundWorkloadError(ctx, err)
	}
	if _, err := connection.Write(packet[:]); err != nil {
		return 0, contextBoundWorkloadError(ctx, err)
	}
	var response [len(packet)]byte
	for {
		if _, err := io.ReadFull(connection, response[:]); err != nil {
			return 0, contextBoundWorkloadError(ctx, err)
		}
		if bytes.Equal(response[:], packet[:]) {
			return time.Since(startTime), nil
		}

		// A timed-out UDP echo can arrive after its caller has moved to the
		// next sequence. Ignore a well-formed older reply within this probe's
		// original deadline; treating it as corruption poisons every later
		// phase while the socket drains the delayed backlog one item at a time.
		responseSequence := binary.BigEndian.Uint64(response[:])
		var stale [len(packet)]byte
		binary.BigEndian.PutUint64(stale[:], responseSequence)
		if responseSequence < sequence && bytes.Equal(response[:], stale[:]) {
			continue
		}
		return 0, fmt.Errorf("latency probe sequence %d was corrupted", sequence)
	}
}

func TestLatencyProbePhaseSequencesDoNotOverlapUnderLongLoad(t *testing.T) {
	if latencyProbeIdleStartSequence >= latencyProbeLoadedStartSequence ||
		latencyProbeLoadedStartSequence+1_000_000 >= latencyProbePostLoadStartSequence {
		t.Fatal("latency probe phase sequence ranges overlap")
	}

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	serverErr := make(chan error, 1)
	go func() {
		var request [32]byte
		if _, err := io.ReadFull(server, request[:]); err != nil {
			serverErr <- err
			return
		}
		var lateLoadedReply [32]byte
		binary.BigEndian.PutUint64(
			lateLoadedReply[:],
			latencyProbeLoadedStartSequence+5000,
		)
		if _, err := server.Write(lateLoadedReply[:]); err != nil {
			serverErr <- err
			return
		}
		_, err := server.Write(request[:])
		serverErr <- err
	}()

	latency, err := runLatencyProbe(
		t.Context(),
		client,
		latencyProbePostLoadStartSequence+7,
		time.Second,
	)
	if err != nil {
		t.Fatalf("post-load probe rejected a late loaded reply: %v", err)
	}
	if latency <= 0 {
		t.Fatalf("post-load probe latency = %v", latency)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("serve latency probe: %v", err)
	}
}

// Tracks a fixed offered probe train independently from response timing. It is
// owned by the workload goroutine; the reader only publishes complete replies.
type loadedLatencyProbeState struct {
	samples latencyProbeSamples
	pending map[uint64]time.Time
	timeout time.Duration
}

func newLoadedLatencyProbeState(timeout time.Duration) *loadedLatencyProbeState {
	return &loadedLatencyProbeState{
		pending: map[uint64]time.Time{},
		timeout: timeout,
	}
}

// Records one offered slot whether or not the local write was accepted.
func (self *loadedLatencyProbeState) attempt(
	sequence uint64,
	sendTime time.Time,
	err error,
) {
	self.samples.attemptCount += 1
	if err != nil {
		self.samples.failureCount += 1
		if self.samples.firstFailure == nil {
			self.samples.firstFailure = err
		}
		return
	}
	self.pending[sequence] = sendTime
}

// Matches replies out of order; expired or duplicate sequences are harmless.
func (self *loadedLatencyProbeState) receive(
	packet [32]byte,
	receiveTime time.Time,
) {
	sequence := binary.BigEndian.Uint64(packet[:])
	var expected [len(packet)]byte
	binary.BigEndian.PutUint64(expected[:], sequence)
	if packet != expected {
		if self.samples.firstFailure == nil {
			self.samples.firstFailure = fmt.Errorf("loaded latency probe response was corrupted")
		}
		return
	}
	sendTime, ok := self.pending[sequence]
	if !ok {
		return
	}
	delete(self.pending, sequence)
	if receiveTime.Before(sendTime) {
		if self.samples.firstFailure == nil {
			self.samples.firstFailure = fmt.Errorf("loaded latency probe response preceded its send")
		}
		self.samples.failureCount += 1
		return
	}
	latency := receiveTime.Sub(sendTime)
	if self.timeout < latency {
		self.samples.failureCount += 1
		if self.samples.firstFailure == nil {
			self.samples.firstFailure = context.DeadlineExceeded
		}
		return
	}
	self.samples.latencies = append(
		self.samples.latencies,
		latency,
	)
}

// Converts every elapsed pending probe into one explicit timeout.
func (self *loadedLatencyProbeState) expire(currentTime time.Time) {
	for sequence, sendTime := range self.pending {
		if currentTime.Sub(sendTime) < self.timeout {
			continue
		}
		delete(self.pending, sequence)
		self.samples.failureCount += 1
		if self.samples.firstFailure == nil {
			self.samples.firstFailure = context.DeadlineExceeded
		}
	}
}

// Closes the measurement boundary without letting post-load replies improve
// the loaded phase retroactively.
func (self *loadedLatencyProbeState) finish() {
	for sequence := range self.pending {
		delete(self.pending, sequence)
		self.samples.failureCount += 1
		if self.samples.firstFailure == nil {
			self.samples.firstFailure = errLoadedLatencyProbeIncomplete
		}
	}
}

// One complete loaded-probe read is timestamped before the bounded handoff.
type loadedLatencyProbeResponse struct {
	packet      [32]byte
	receiveTime time.Time
	err         error
}

// Optional barriers expose the response handoff without changing measurements.
type loadedLatencyProbeTestSettings struct {
	afterAttemptHook          func(int)
	afterResponseReadHook     func()
	unbufferedResponseHandoff bool
}

// Offers probes at a fixed rate until the bulk goroutine exits. Multiple UDP
// requests may be outstanding, so a timeout never suppresses later demand.
func runLoadedLatencyProbes(
	ctx context.Context,
	connection net.Conn,
	startSequence uint64,
	timeout time.Duration,
	interval time.Duration,
	workloadDone <-chan struct{},
	testSettings *loadedLatencyProbeTestSettings,
) latencyProbeSamples {
	select {
	case <-ctx.Done():
		return latencyProbeSamples{}
	case <-workloadDone:
		return latencyProbeSamples{}
	default:
	}

	probeCtx, probeCancel := context.WithCancel(ctx)
	responseBufferCount := 64
	if testSettings != nil && testSettings.unbufferedResponseHandoff {
		responseBufferCount = 0
	}
	responses := make(chan loadedLatencyProbeResponse, responseBufferCount)
	responseInput := (<-chan loadedLatencyProbeResponse)(responses)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer close(responses)
		publishError := func(err error) {
			if probeCtx.Err() != nil {
				return
			}
			responses <- loadedLatencyProbeResponse{
				receiveTime: time.Now(),
				err:         err,
			}
		}
		for {
			if err := connection.SetReadDeadline(
				time.Now().Add(loadedLatencyProbeReadInterval),
			); err != nil {
				publishError(err)
				return
			}
			var packet [32]byte
			_, err := io.ReadFull(connection, packet[:])
			if err != nil {
				if probeCtx.Err() != nil {
					return
				}
				var netErr net.Error
				if errors.As(err, &netErr) && netErr.Timeout() {
					continue
				}
				publishError(err)
				return
			}
			response := loadedLatencyProbeResponse{
				packet:      packet,
				receiveTime: time.Now(),
			}
			if testSettings != nil && testSettings.afterResponseReadHook != nil {
				testSettings.afterResponseReadHook()
			}
			// A complete read belongs to the measurement until the owner applies
			// its receive-time boundary. Cancellation cannot discard the handoff.
			responses <- response
		}
	}()

	state := newLoadedLatencyProbeState(timeout)
	nextSequence := startSequence
	writeProbe := func() {
		sequence := nextSequence
		nextSequence += 1
		var packet [32]byte
		binary.BigEndian.PutUint64(packet[:], sequence)
		sendTime := time.Now()
		err := connection.SetWriteDeadline(boundedWorkloadDeadline(ctx, timeout))
		if err == nil {
			var writeByteCount int
			writeByteCount, err = connection.Write(packet[:])
			if err == nil && writeByteCount != len(packet) {
				err = io.ErrShortWrite
			}
		}
		state.attempt(sequence, sendTime, err)
		if testSettings != nil && testSettings.afterAttemptHook != nil {
			testSettings.afterAttemptHook(state.samples.attemptCount)
		}
	}
	writeProbe()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	responsesOpen := true
	for {
		select {
		case <-ctx.Done():
			responsesOpen = false
		case <-workloadDone:
			responsesOpen = false
		case response, ok := <-responseInput:
			if !ok {
				responseInput = nil
				continue
			}
			if response.err != nil {
				if state.samples.firstFailure == nil {
					state.samples.firstFailure = response.err
				}
				continue
			}
			state.receive(response.packet, response.receiveTime)
		case currentTime := <-ticker.C:
			state.expire(currentTime)
			writeProbe()
		}
		if !responsesOpen {
			break
		}
	}

	receiveBoundary := time.Now()
	probeCancel()
	_ = connection.SetReadDeadline(time.Now())
	for response := range responses {
		if receiveBoundary.Before(response.receiveTime) {
			continue
		}
		if response.err != nil {
			if state.samples.firstFailure == nil {
				state.samples.firstFailure = response.err
			}
			continue
		}
		state.receive(response.packet, response.receiveTime)
	}
	<-readerDone
	state.expire(time.Now())
	state.finish()
	_ = connection.SetDeadline(time.Time{})
	return state.samples
}

// A bulk TCP flow and a separate UDP probe share both simulated directions.
// The compatibility entry point retains the original upload direction.
func measureLatencyUnderLoad(
	ctx context.Context,
	profile networkProfile,
	resources tunResourceProfile,
	bulkByteCount int64,
) (workloadResult, error) {
	return measureLatencyUnderLoadDirection(
		ctx,
		profile,
		resources,
		bulkByteCount,
		true,
	)
}

// Direction is device-oriented: forward=true loads the device upload while
// forward=false loads the device download. The UDP echo continues to traverse
// both directions so its RTT remains directly comparable across the pair.
func measureLatencyUnderLoadDirection(
	ctx context.Context,
	profile networkProfile,
	resources tunResourceProfile,
	bulkByteCount int64,
	forward bool,
) (workloadResult, error) {
	return measureLatencyUnderLoadWithDirectionAndStartHook(
		ctx,
		profile,
		resources,
		bulkByteCount,
		forward,
		nil,
	)
}

// The optional hook gives cancellation tests a post-handshake bulk boundary.
func measureLatencyUnderLoadWithStartHook(
	ctx context.Context,
	profile networkProfile,
	resources tunResourceProfile,
	bulkByteCount int64,
	startHook func(*tunPath) error,
) (workloadResult, error) {
	return measureLatencyUnderLoadWithDirectionAndStartHook(
		ctx,
		profile,
		resources,
		bulkByteCount,
		true,
		startHook,
	)
}

func measureLatencyUnderLoadWithDirectionAndStartHook(
	ctx context.Context,
	profile networkProfile,
	resources tunResourceProfile,
	bulkByteCount int64,
	forward bool,
	startHook func(*tunPath) error,
) (workloadResult, error) {
	return measureLatencyUnderLoadWithFlowTestSettingsDirection(
		ctx,
		profile,
		resources,
		bulkByteCount,
		forward,
		startHook,
		nil,
	)
}

// Test settings expose bulk-flow admission while the normal helper retains
// its existing hooks and result contract.
func measureLatencyUnderLoadWithFlowTestSettings(
	ctx context.Context,
	profile networkProfile,
	resources tunResourceProfile,
	bulkByteCount int64,
	startHook func(*tunPath) error,
	testSettings *workloadTCPFlowTestSettings,
) (workloadResult, error) {
	return measureLatencyUnderLoadWithFlowTestSettingsDirection(
		ctx,
		profile,
		resources,
		bulkByteCount,
		true,
		startHook,
		testSettings,
	)
}

func measureLatencyUnderLoadWithFlowTestSettingsDirection(
	ctx context.Context,
	profile networkProfile,
	resources tunResourceProfile,
	bulkByteCount int64,
	forward bool,
	startHook func(*tunPath) error,
	testSettings *workloadTCPFlowTestSettings,
) (workloadResult, error) {
	path, err := newTunPath(ctx, profile, resources)
	if err != nil {
		return workloadResult{}, err
	}
	defer path.close()
	probeListener, err := path.right.ListenUDP(&net.UDPAddr{IP: path.endpointAddress(false), Port: 0})
	if err != nil {
		return workloadResult{}, err
	}
	stopProbeListenerInterrupt := context.AfterFunc(ctx, func() {
		_ = probeListener.Close()
	})
	defer stopProbeListenerInterrupt()
	probeServerDone := make(chan struct{})
	go func() {
		defer close(probeServerDone)
		defer func() {
			if testSettings != nil && testSettings.beforeProbeServerDoneHook != nil {
				testSettings.beforeProbeServerDoneHook()
			}
		}()
		packetBytes := make([]byte, 2048)
		for {
			readByteCount, sourceAddress, readErr := probeListener.ReadFrom(packetBytes)
			if readErr != nil {
				return
			}
			_, _ = probeListener.WriteTo(packetBytes[:readByteCount], sourceAddress)
		}
	}()
	var probeServerJoinOnce sync.Once
	joinProbeServer := func() {
		probeServerJoinOnce.Do(func() {
			_ = probeListener.Close()
			if testSettings != nil && testSettings.beforeProbeServerWaitHook != nil {
				testSettings.beforeProbeServerWaitHook()
			}
			<-probeServerDone
		})
	}
	defer joinProbeServer()
	probeConnection, err := path.left.DialContext(ctx, "udp", probeListener.LocalAddr().String())
	if err != nil {
		return workloadResult{}, err
	}
	defer probeConnection.Close()
	probeTimeout := max(2*time.Second, 8*(profile.Forward.BaseDelay+profile.Reverse.BaseDelay))
	probeMany := func(startSequence uint64, count int) latencyProbeSamples {
		samples := latencyProbeSamples{latencies: make([]time.Duration, 0, count)}
		for probeIndex := range count {
			if ctx.Err() != nil {
				break
			}
			latency, probeErr := runLatencyProbe(
				ctx,
				probeConnection,
				startSequence+uint64(probeIndex),
				probeTimeout,
			)
			samples.add(latency, probeErr)
			if err := waitForWorkloadDelay(ctx, 5*time.Millisecond); err != nil {
				break
			}
		}
		return samples
	}
	measurementStart, err := path.beginMeasurement(ctx)
	if err != nil {
		return workloadResult{}, err
	}
	idleSamples := probeMany(latencyProbeIdleStartSequence, 12)
	if err := idleSamples.validate("idle"); err != nil {
		forwardLink, reverseLink, boundaryErr := path.finishMeasurement(ctx, measurementStart)
		result := workloadResult{
			ForwardLink: forwardLink,
			ReverseLink: reverseLink,
		}
		applyLatencyProbeSamples(&result, idleSamples, latencyProbeSamples{}, latencyProbeSamples{})
		if boundaryErr != nil {
			return result, errors.Join(err, boundaryErr)
		}
		return result, err
	}

	bulkListenerTun := path.right
	bulkDialTun := path.left
	bulkListenerIP := path.endpointAddress(false)
	bulkRateBitsPerSecond := profile.Forward.RateBitsPerSecond
	if !forward {
		bulkListenerTun = path.left
		bulkDialTun = path.right
		bulkListenerIP = path.endpointAddress(true)
		bulkRateBitsPerSecond = profile.Reverse.RateBitsPerSecond
	}
	bulkListener, err := bulkListenerTun.ListenTCP(&net.TCPAddr{IP: bulkListenerIP, Port: 0})
	if err != nil {
		return workloadResult{}, err
	}
	workloadDeadline := boundedWorkloadDeadline(
		ctx,
		calibrationWorkloadTimeout(profile, bulkByteCount),
	)
	bulkReceiverReady := make(chan struct{})
	bulkReceiverFinished := make(chan struct{})
	var flowServerSettings *logicalTCPFlowServerSettings
	if testSettings != nil {
		flowServerSettings = testSettings.flowServerSettings
	}
	const bulkFlowId = logicalTCPFlowId(0)
	flowServer := newLogicalTCPFlowServer(
		ctx,
		bulkListener,
		1,
		func(flowId logicalTCPFlowId, connection net.Conn) error {
			if flowId != bulkFlowId {
				return fmt.Errorf("latency-under-load flow id=%d, want=%d", flowId, bulkFlowId)
			}
			stopInterrupt := interruptDeadlineOnContext(ctx, connection)
			defer stopInterrupt()
			if err := connection.SetDeadline(workloadDeadline); err != nil {
				return err
			}
			close(bulkReceiverReady)
			_, copyErr := io.CopyN(io.Discard, connection, bulkByteCount)
			if copyErr == nil && testSettings != nil &&
				testSettings.beforeBulkReceiverDoneHook != nil {
				testSettings.beforeBulkReceiverDoneHook()
			}
			close(bulkReceiverFinished)
			return copyErr
		},
		flowServerSettings,
	)
	defer flowServer.CloseAndWait()
	bulkLoadedFinished := make(chan struct{})
	go func() {
		select {
		case <-bulkReceiverFinished:
		case <-flowServer.Done():
		}
		close(bulkLoadedFinished)
	}()
	if testSettings != nil && testSettings.beforeClientDialHook != nil {
		if err := testSettings.beforeClientDialHook(
			ctx,
			bulkDialTun,
			bulkListener.Addr().String(),
		); err != nil {
			return workloadResult{}, err
		}
	}
	bulkConnection, err := bulkDialTun.DialContext(ctx, "tcp", bulkListener.Addr().String())
	if err != nil {
		return workloadResult{}, err
	}
	defer bulkConnection.Close()
	stopBulkInterrupt := interruptDeadlineOnContext(ctx, bulkConnection)
	defer stopBulkInterrupt()
	if err := bulkConnection.SetDeadline(workloadDeadline); err != nil {
		return workloadResult{}, err
	}
	if err := writeLogicalTCPFlowPreface(bulkConnection, bulkFlowId); err != nil {
		return workloadResult{}, contextBoundWorkloadError(ctx, err)
	}
	if err := flowServer.WaitReady(ctx); err != nil {
		return workloadResult{}, contextBoundWorkloadError(ctx, err)
	}
	select {
	case <-ctx.Done():
		return workloadResult{}, ctx.Err()
	case <-bulkReceiverReady:
	case <-flowServer.Done():
		if receiverErr := flowServer.Wait(); receiverErr != nil {
			return workloadResult{}, contextBoundWorkloadError(ctx, receiverErr)
		}
	}
	if startHook != nil {
		if err := startHook(path); err != nil {
			return workloadResult{}, err
		}
	}
	bulkSenderDone := make(chan error, 1)
	bulkSenderFinished := make(chan struct{})
	bulkStart := time.Now()
	go func() {
		defer close(bulkSenderFinished)
		publishResult := func(err error) {
			if testSettings != nil && testSettings.beforeBulkSenderDoneHook != nil {
				testSettings.beforeBulkSenderDoneHook()
			}
			bulkSenderDone <- err
		}
		payload := deterministicPayload()
		for remaining := bulkByteCount; 0 < remaining; {
			chunk := payload
			if remaining < int64(len(chunk)) {
				chunk = payload[:remaining]
			}
			writtenByteCount, writeErr := bulkConnection.Write(chunk)
			remaining -= int64(writtenByteCount)
			if writeErr != nil {
				_ = bulkConnection.Close()
				if ctx.Err() != nil {
					publishResult(ctx.Err())
					return
				}
				publishResult(writeErr)
				return
			}
		}
		publishResult(nil)
	}()
	var bulkSenderJoinOnce sync.Once
	joinBulkSender := func(closeConnection bool) {
		bulkSenderJoinOnce.Do(func() {
			if closeConnection {
				_ = bulkConnection.Close()
			}
			if testSettings != nil && testSettings.beforeBulkSenderWaitHook != nil {
				testSettings.beforeBulkSenderWaitHook()
			}
			<-bulkSenderFinished
		})
	}
	defer joinBulkSender(true)
	var loadedProbeTestSettings *loadedLatencyProbeTestSettings
	if testSettings != nil {
		loadedProbeTestSettings = &loadedLatencyProbeTestSettings{
			afterAttemptHook: testSettings.afterLoadedProbeAttemptHook,
		}
	}
	loadedSamples := runLoadedLatencyProbes(
		ctx,
		probeConnection,
		latencyProbeLoadedStartSequence,
		probeTimeout,
		loadedLatencyProbeIntervalForRate(
			bulkByteCount,
			bulkRateBitsPerSecond,
		),
		bulkLoadedFinished,
		loadedProbeTestSettings,
	)
	var bulkErr error
	select {
	case bulkErr = <-bulkSenderDone:
	case <-ctx.Done():
		// Do not wait for the sender's result publication before entering the
		// deferred join. Closing the connection is what interrupts a blocked
		// write; the join then owns the sender until its terminal publication.
		return workloadResult{}, ctx.Err()
	}
	if bulkErr != nil {
		return workloadResult{}, contextBoundWorkloadError(ctx, bulkErr)
	}
	joinBulkSender(false)
	if err := flowServer.Wait(); err != nil {
		return workloadResult{}, contextBoundWorkloadError(ctx, err)
	}
	bulkDuration := time.Since(bulkStart)
	postLoadSamples := probeMany(latencyProbePostLoadStartSequence, 12)
	joinProbeServer()
	_ = probeConnection.Close()
	_ = bulkConnection.Close()
	forwardLink, reverseLink, err := path.finishMeasurement(ctx, measurementStart)
	if err != nil {
		return workloadResult{}, fmt.Errorf("finish measured latency-under-load: %w", err)
	}
	result := workloadResult{
		UsefulByteCount: bulkByteCount,
		Duration:        bulkDuration,
		ForwardLink:     forwardLink,
		ReverseLink:     reverseLink,
	}
	applyLatencyProbeSamples(&result, idleSamples, loadedSamples, postLoadSamples)
	result = finishWorkloadResult(result)
	if err := validateLatencyProbeSamples(idleSamples, loadedSamples, postLoadSamples); err != nil {
		return result, err
	}
	return result, nil
}

// A dormant first bulk candidate cannot consume the one-flow receiver quota or
// fire the post-handshake hook reserved for the later identified winner.
func TestLatencyUnderLoadUsesWinnerAfterDormantAcceptedCandidate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	const bulkByteCount = 1024 * 1024
	harness := newWorkloadDormantCandidateHarness(1)
	defer harness.close()
	var startHookCount atomic.Int64
	resources := defaultTunResourceProfile()
	resources.TcpBufferDefault = 128 * 1024
	result, err := measureLatencyUnderLoadWithFlowTestSettings(
		ctx,
		initialNetworkProfiles(20260811)["clean-lan"],
		resources,
		bulkByteCount,
		func(*tunPath) error {
			startHookCount.Add(1)
			if claimedFlowCount := harness.claimedFlows.Load(); claimedFlowCount != 1 {
				return fmt.Errorf("claimed flows=%d at bulk start, want=1", claimedFlowCount)
			}
			return nil
		},
		harness.settings(),
	)
	if err != nil {
		t.Fatal(err)
	}
	harness.assertJoined(t, ctx)
	if claimedFlowCount := harness.claimedFlows.Load(); claimedFlowCount != 1 {
		t.Fatalf("claimed flows=%d, want=1", claimedFlowCount)
	}
	if startCount := startHookCount.Load(); startCount != 1 {
		t.Fatalf("start hook count=%d, want=1", startCount)
	}
	if result.UsefulByteCount != bulkByteCount ||
		result.LoadedProbeSuccessCount < minimumLatencyProbeSuccessCount {
		t.Fatalf("latency-under-load result=%+v", result)
	}
}

// An error after bulk readiness closes the UDP listener but cannot return
// while the probe server retains its final goroutine lifecycle credit.
func TestLatencyUnderLoadEarlyErrorJoinsProbeServer(t *testing.T) {
	safetyCtx, safetyCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer safetyCancel()
	expectedErr := errors.New("stop after latency bulk readiness")
	probeServerHeld := make(chan struct{})
	probeServerWaitReached := make(chan struct{})
	releaseProbeServer := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseProbeServer)
		})
	}
	defer release()
	testSettings := &workloadTCPFlowTestSettings{
		beforeProbeServerDoneHook: func() {
			close(probeServerHeld)
			<-releaseProbeServer
		},
		beforeProbeServerWaitHook: func() {
			close(probeServerWaitReached)
		},
	}
	completion := make(chan error, 1)
	go func() {
		_, err := measureLatencyUnderLoadWithFlowTestSettings(
			safetyCtx,
			initialNetworkProfiles(20260811)["clean-lan"],
			defaultTunResourceProfile(),
			1024*1024,
			func(*tunPath) error {
				return expectedErr
			},
			testSettings,
		)
		completion <- err
	}()
	waitBarrier := func(name string, barrier <-chan struct{}) {
		t.Helper()
		select {
		case <-barrier:
		case err := <-completion:
			t.Fatalf("latency helper returned before %s: %v", name, err)
		case <-safetyCtx.Done():
			t.Fatalf("wait for %s: %v", name, safetyCtx.Err())
		}
	}
	waitBarrier("held probe-server completion", probeServerHeld)
	waitBarrier("probe-server join", probeServerWaitReached)
	select {
	case err := <-completion:
		t.Fatalf("latency helper returned while probe server was held: %v", err)
	default:
	}
	release()
	select {
	case err := <-completion:
		if !errors.Is(err, expectedErr) {
			t.Fatalf("latency helper error=%v, want=%v", err, expectedErr)
		}
	case <-safetyCtx.Done():
		t.Fatalf("latency helper did not return after probe-server release: %v", safetyCtx.Err())
	}
}

// Loaded probes remain active after the sender fills its socket until the
// receiver has consumed the complete bulk payload.
func TestLatencyUnderLoadLoadedProbesFollowReceiverCompletion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	bulkSenderDone := make(chan struct{})
	bulkReceiverHeld := make(chan struct{})
	fifthLoadedAttempt := make(chan struct{})
	releaseReceiver := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseReceiver)
		})
	}
	defer release()
	testSettings := &workloadTCPFlowTestSettings{
		beforeBulkSenderDoneHook: func() {
			close(bulkSenderDone)
		},
		beforeBulkReceiverDoneHook: func() {
			close(bulkReceiverHeld)
			<-releaseReceiver
		},
		afterLoadedProbeAttemptHook: func(attemptCount int) {
			if attemptCount == 5 {
				close(fifthLoadedAttempt)
			}
		},
	}
	completion := make(chan struct {
		result workloadResult
		err    error
	}, 1)
	go func() {
		result, err := measureLatencyUnderLoadWithFlowTestSettings(
			ctx,
			initialNetworkProfiles(20260818)["clean-lan"],
			defaultTunResourceProfile(),
			64*1024,
			nil,
			testSettings,
		)
		completion <- struct {
			result workloadResult
			err    error
		}{result: result, err: err}
	}()
	for name, boundary := range map[string]<-chan struct{}{
		"bulk sender completion":   bulkSenderDone,
		"held receiver completion": bulkReceiverHeld,
		"fifth loaded probe":       fifthLoadedAttempt,
	} {
		select {
		case <-boundary:
		case completed := <-completion:
			t.Fatalf("latency helper returned before %s: %v", name, completed.err)
		case <-ctx.Done():
			t.Fatalf("wait for %s: %v", name, ctx.Err())
		}
	}
	release()
	select {
	case completed := <-completion:
		if completed.err != nil {
			t.Fatal(completed.err)
		}
		if completed.result.LoadedProbeAttemptCount < 5 {
			t.Fatalf(
				"loaded attempts=%d want at least 5",
				completed.result.LoadedProbeAttemptCount,
			)
		}
	case <-ctx.Done():
		t.Fatalf("latency helper completion: %v", ctx.Err())
	}
}

// Cancellation after the final bulk write cannot return while the sender is
// held immediately before publishing its result and releasing its lifecycle.
func TestLatencyUnderLoadCancellationJoinsBulkSender(t *testing.T) {
	safetyCtx, safetyCancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer safetyCancel()
	workloadCtx, workloadCancel := context.WithCancel(safetyCtx)
	defer workloadCancel()
	bulkSenderHeld := make(chan struct{})
	bulkSenderWaitReached := make(chan struct{})
	releaseBulkSender := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseBulkSender)
		})
	}
	defer release()
	testSettings := &workloadTCPFlowTestSettings{
		beforeBulkSenderDoneHook: func() {
			close(bulkSenderHeld)
			<-releaseBulkSender
		},
		beforeBulkSenderWaitHook: func() {
			close(bulkSenderWaitReached)
		},
	}
	resources := defaultTunResourceProfile()
	resources.TcpBufferDefault = 128 * 1024
	completion := make(chan error, 1)
	go func() {
		_, err := measureLatencyUnderLoadWithFlowTestSettings(
			workloadCtx,
			initialNetworkProfiles(20260811)["clean-lan"],
			resources,
			1024*1024,
			nil,
			testSettings,
		)
		completion <- err
	}()
	select {
	case <-bulkSenderHeld:
	case err := <-completion:
		t.Fatalf("latency helper returned before bulk sender was held: %v", err)
	case <-safetyCtx.Done():
		t.Fatalf("wait for held bulk sender: %v", safetyCtx.Err())
	}
	workloadCancel()
	select {
	case <-bulkSenderWaitReached:
	case err := <-completion:
		t.Fatalf("latency helper returned before bulk-sender join: %v", err)
	case <-safetyCtx.Done():
		t.Fatalf("wait for bulk-sender join: %v", safetyCtx.Err())
	}
	select {
	case err := <-completion:
		t.Fatalf("latency helper returned while bulk sender was held: %v", err)
	default:
	}
	release()
	select {
	case err := <-completion:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("latency helper error=%v, want context canceled", err)
		}
	case <-safetyCtx.Done():
		t.Fatalf("latency helper did not return after bulk-sender release: %v", safetyCtx.Err())
	}
}

// Workload helpers compile and execute through a small clean path by default.
func TestWorkloadCleanPathCorrectness(t *testing.T) {
	if testing.Short() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	profile := initialNetworkProfiles(20260810)["clean-lan"]
	resources := defaultTunResourceProfile()
	tcpResult, err := measureTCPWorkload(ctx, profile, resources, true, 1, 512*1024)
	if err != nil {
		t.Fatalf("TCP workload: %v", err)
	}
	if tcpResult.UsefulByteCount != 512*1024 || tcpResult.ContentHash == "" {
		t.Fatalf("TCP result=%+v", tcpResult)
	}
	udpResult, err := measureUDPWorkload(ctx, profile, resources, 50*time.Millisecond, 10_000_000, 1000)
	if err != nil {
		t.Fatalf("UDP workload: %v", err)
	}
	if udpResult.DeliveredPacketCount == 0 || udpResult.CorruptPacketCount != 0 {
		t.Fatalf("UDP result=%+v", udpResult)
	}
}

// Completion joins an exact carrier delivery held beyond send completion; no
// fixed drain duration can truncate the measurement while that packet is live.
func TestUDPWorkloadWaitsForHeldCarrierDelivery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	profile := initialNetworkProfiles(20260810)["clean-lan"]
	releaseCarrier := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releaseCarrier) })
	}
	t.Cleanup(release)
	barrierPublished := make(chan (<-chan struct{}), 1)
	// One asynchronous result keeps assertions on the test goroutine.
	type workloadCompletion struct {
		result workloadResult
		err    error
	}
	completed := make(chan workloadCompletion, 1)
	go func() {
		result, err := measureUDPWorkloadWithStartHook(
			ctx,
			profile,
			defaultTunResourceProfile(),
			time.Millisecond,
			8_000_000,
			1000,
			func(path *tunPath) error {
				barrierPublished <- holdLinkScheduleForTest(
					[]*directionalLink{path.forwardLink},
					func(observation linkScheduleObservation) bool {
						return observation.terminalDropCause == linkTerminalDropNone &&
							1000 <= observation.packetByteCount
					},
					releaseCarrier,
				)
				return nil
			},
		)
		completed <- workloadCompletion{result: result, err: err}
	}()
	var carrierHeld <-chan struct{}
	select {
	case carrierHeld = <-barrierPublished:
	case completion := <-completed:
		t.Fatalf("UDP workload returned before publishing its delivery barrier: %+v", completion)
	case <-ctx.Done():
		t.Fatalf("publish UDP delivery barrier: %v", ctx.Err())
	}
	select {
	case <-carrierHeld:
	case completion := <-completed:
		t.Fatalf("UDP workload returned before its carrier delivery was held: %+v", completion)
	case <-ctx.Done():
		t.Fatalf("wait for held UDP carrier delivery: %v", ctx.Err())
	}
	select {
	case completion := <-completed:
		t.Fatalf("UDP workload returned through its held carrier delivery: %+v", completion)
	default:
	}
	release()
	select {
	case completion := <-completed:
		if completion.err != nil {
			t.Fatal(completion.err)
		}
		if completion.result.OfferedPacketCount != 1 || completion.result.DeliveredPacketCount != 1 ||
			completion.result.CorruptPacketCount != 0 || completion.result.ForwardLink.QueuedPacketCount != 0 {
			t.Fatalf("held-delivery UDP result=%+v", completion.result)
		}
	case <-ctx.Done():
		t.Fatalf("UDP workload did not complete after delivery release: %v", ctx.Err())
	}
}

// Modeled loss is a terminal event, while delivered packets still reach the
// receiver before the workload returns even when the link reorders releases.
func TestUDPWorkloadAccountsTerminalLossAndReordering(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	profile := initialNetworkProfiles(20260810)["clean-lan"]
	profile.Forward.LossModel = lossModelEveryN
	profile.Forward.DropEveryPacketCount = 3
	profile.Forward.ReorderProbability = 1
	result, err := measureUDPWorkload(
		ctx,
		profile,
		defaultTunResourceProfile(),
		20*time.Millisecond,
		5_000_000,
		1000,
	)
	if err != nil {
		t.Fatal(err)
	}
	terminalPacketCount := result.ForwardLink.DeliveredPacketCount +
		result.ForwardLink.LossDropPacketCount +
		result.ForwardLink.MtuDropPacketCount +
		result.ForwardLink.QueueDropPacketCount +
		result.ForwardLink.OutageDropPacketCount +
		result.ForwardLink.ReceiverDropPacketCount +
		result.ForwardLink.CanceledDropPacketCount
	if result.OfferedPacketCount != int64(terminalPacketCount) {
		t.Fatalf(
			"UDP terminal accounting=%d offered=%d result=%+v",
			terminalPacketCount,
			result.OfferedPacketCount,
			result,
		)
	}
	if result.ForwardLink.LossDropPacketCount == 0 || result.ForwardLink.ReorderedPacketCount == 0 {
		t.Fatalf("UDP impairment was not exercised: %+v", result.ForwardLink)
	}
	if result.DeliveredPacketCount != int64(result.ForwardLink.DeliveredPacketCount) ||
		result.CorruptPacketCount != 0 {
		t.Fatalf("UDP receiver accounting diverged from carrier: %+v", result)
	}
}

// The default bulk payload must not turn harness capacity into hidden loss.
func TestWorkloadCleanPath32MiBHasNoHarnessDrops(t *testing.T) {
	testTimeout := 2 * time.Minute
	if perfvarRaceEnabled {
		testTimeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	const byteCount = 32 * 1024 * 1024
	profile := initialNetworkProfiles(20260810)["clean-lan"]
	result, err := measureTCPWorkload(
		ctx,
		profile,
		defaultTunResourceProfile(),
		true,
		1,
		byteCount,
	)
	if err != nil {
		t.Fatalf("clean 32 MiB TCP: %v", err)
	}
	if result.UsefulByteCount != byteCount || result.ContentHash != deterministicPayloadHash(byteCount) {
		t.Fatalf("clean 32 MiB result=%+v", result)
	}
	for name, snapshot := range map[string]directionalLinkSnapshot{
		"forward": result.ForwardLink,
		"reverse": result.ReverseLink,
	} {
		if snapshot.QueueDropPacketCount != 0 || snapshot.ReceiverDropPacketCount != 0 {
			t.Errorf("%s harness drops: %+v", name, snapshot)
		}
	}
}

// Established bulk I/O is held at an exact post-blackhole carrier submission,
// then explicit cancellation must stop it; the timeout is only a liveness cap.
func testCalibrationBulkWorkloadCancelsAfterBlackhole(
	t *testing.T,
	measure func(context.Context, func(*tunPath) error) error,
) {
	livenessCtx, livenessCancel := context.WithTimeout(context.Background(), time.Minute)
	defer livenessCancel()
	workloadCtx, workloadCancel := context.WithCancel(livenessCtx)
	defer workloadCancel()
	barrierPublished := make(chan (<-chan struct{}), 1)
	completion := make(chan error, 1)
	go func() {
		completion <- measure(workloadCtx, func(path *tunPath) error {
			if err := path.network.setBlackhole(workloadCtx, true); err != nil {
				return err
			}
			barrierPublished <- holdLinkScheduleForTest(
				[]*directionalLink{path.forwardLink, path.reverseLink},
				func(observation linkScheduleObservation) bool {
					return observation.terminalDropCause == linkTerminalDropOutage &&
						1000 <= observation.packetByteCount
				},
				workloadCtx.Done(),
			)
			return nil
		})
	}()
	var carrierHeld <-chan struct{}
	select {
	case carrierHeld = <-barrierPublished:
	case err := <-completion:
		t.Fatalf("workload returned before publishing its carrier barrier: %v", err)
	case <-livenessCtx.Done():
		t.Fatalf("publish post-blackhole carrier barrier: %v", livenessCtx.Err())
	}
	select {
	case <-carrierHeld:
	case err := <-completion:
		t.Fatalf("workload returned before a post-blackhole carrier submission: %v", err)
	case <-livenessCtx.Done():
		t.Fatalf("wait for post-blackhole carrier submission: %v", livenessCtx.Err())
	}
	workloadCancel()
	select {
	case err := <-completion:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blackhole error=%v, want explicit context cancellation", err)
		}
	case <-livenessCtx.Done():
		t.Fatalf("workload did not stop after explicit cancellation: %v", livenessCtx.Err())
	}
}

// TCP write/read ownership stops at explicit cancellation while a carrier
// packet is held in the outage generation.
func TestCalibrationTCPWorkloadCancelsAfterBlackhole(t *testing.T) {
	testCalibrationBulkWorkloadCancelsAfterBlackhole(
		t,
		func(ctx context.Context, startHook func(*tunPath) error) error {
			profile := initialNetworkProfiles(20260812)["clean-lan"]
			_, err := measureTCPWorkloadWithStartHook(
				ctx,
				profile,
				defaultTunResourceProfile(),
				true,
				1,
				8*1024*1024,
				startHook,
			)
			return err
		},
	)
}

// QUIC's longer transport deadline cannot outlive explicit cancellation while
// one post-handshake datagram is held by the outage generation.
func TestCalibrationQUICWorkloadCancelsAfterBlackhole(t *testing.T) {
	testCalibrationBulkWorkloadCancelsAfterBlackhole(
		t,
		func(ctx context.Context, startHook func(*tunPath) error) error {
			profile := initialNetworkProfiles(20260812)["clean-lan"]
			_, err := measureQUICWorkloadWithStartHook(
				ctx,
				profile,
				defaultTunResourceProfile(),
				20*1024*1024,
				startHook,
			)
			return err
		},
	)
}

// Bulk TCP and its concurrent UDP probes share one explicit cancellation edge
// while a post-handshake carrier packet is held by the outage generation.
func TestCalibrationLatencyUnderLoadCancelsAfterBlackhole(t *testing.T) {
	testCalibrationBulkWorkloadCancelsAfterBlackhole(
		t,
		func(ctx context.Context, startHook func(*tunPath) error) error {
			profile := initialNetworkProfiles(20260812)["clean-lan"]
			_, err := measureLatencyUnderLoadWithStartHook(
				ctx,
				profile,
				defaultTunResourceProfile(),
				8*1024*1024,
				startHook,
			)
			return err
		},
	)
}

// Failed probes remain machine-visible and too-small percentile samples fail.
func TestLatencyProbeAccountingRequiresMeaningfulSamples(t *testing.T) {
	samples := latencyProbeSamples{}
	samples.add(time.Millisecond, nil)
	samples.add(2*time.Millisecond, nil)
	samples.add(0, context.DeadlineExceeded)
	if err := samples.validate("loaded"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("two-sample validation error=%v", err)
	}
	samples.add(3*time.Millisecond, nil)
	if err := samples.validate("loaded"); err != nil {
		t.Fatalf("three-sample validation: %v", err)
	}
	result := workloadResult{}
	applyLatencyProbeSamples(&result, samples, samples, samples)
	if result.LoadedProbeAttemptCount != 4 ||
		result.LoadedProbeSuccessCount != 3 ||
		result.LoadedProbeFailureCount != 1 {
		t.Fatalf("loaded probe accounting=%+v", result)
	}
}

// Large impaired calibrations must not inherit the old fixed two-minute
// deadline when their configured-rate allowance is intentionally longer.
func TestCalibrationWorkloadTimeoutScalesForLargeImpairedTransfer(t *testing.T) {
	profile := initialNetworkProfiles(5003)["lte"]
	byteCount := int64(32 * 1024 * 1024)
	timeout := calibrationWorkloadTimeout(profile, byteCount)
	if timeout <= 2*time.Minute {
		t.Fatalf("LTE 32 MiB calibration timeout=%s", timeout)
	}
	rateDuration := time.Duration(
		float64(time.Second) * float64(byteCount*8) /
			float64(profile.Reverse.RateBitsPerSecond),
	)
	if timeout != 60*rateDuration {
		t.Fatalf("LTE calibration timeout=%s want=%s", timeout, 60*rateDuration)
	}
}

// The clean 32 MiB gate retains an explicit race-runtime allowance instead of
// inheriting the configured 1 Gb/s profile's unrealistically short duration.
func TestCalibrationWorkloadTimeoutPreservesRaceInstrumentationAllowance(t *testing.T) {
	profile := initialNetworkProfiles(5004)["clean-lan"]
	byteCount := int64(32 * 1024 * 1024)
	expected := 30 * time.Second
	if perfvarRaceEnabled {
		expected = 4 * time.Minute
	}
	if timeout := calibrationWorkloadTimeout(profile, byteCount); timeout != expected {
		t.Fatalf("clean 32 MiB calibration timeout=%s want=%s", timeout, expected)
	}
}

// QUIC, web, and loaded-latency paths each retain their production protocol.
func TestWorkloadProtocolCorrectness(t *testing.T) {
	if testing.Short() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	profile := initialNetworkProfiles(20260811)["clean-lan"]
	resources := defaultTunResourceProfile()
	quicResult, err := measureQUICWorkload(ctx, profile, resources, 256*1024)
	if err != nil {
		t.Fatalf("QUIC workload: %v", err)
	}
	if quicResult.UsefulByteCount != 256*1024 || quicResult.ContentHash == "" {
		t.Fatalf("QUIC result=%+v", quicResult)
	}
	webResult, err := measureWebWorkload(ctx, profile, resources)
	if err != nil {
		t.Fatalf("web workload: %v", err)
	}
	if webResult.UsefulByteCount == 0 || webResult.TimeToFirstByte <= 0 {
		t.Fatalf("web result=%+v", webResult)
	}
	loadedResult, err := measureLatencyUnderLoad(ctx, profile, resources, 4*1024*1024)
	if err != nil {
		t.Fatalf("latency-under-load workload: %v", err)
	}
	if loadedResult.IdleLatency.P50 <= 0 || loadedResult.LoadedLatency.P50 <= 0 ||
		loadedResult.LoadedProbeSuccessCount < minimumLatencyProbeSuccessCount {
		t.Fatalf("latency-under-load result=%+v", loadedResult)
	}
	loadedDownloadResult, err := measureLatencyUnderLoadDirection(
		ctx,
		profile,
		resources,
		4*1024*1024,
		false,
	)
	if err != nil {
		t.Fatalf("download latency-under-load workload: %v", err)
	}
	if loadedDownloadResult.UsefulByteCount != 4*1024*1024 ||
		loadedDownloadResult.IdleLatency.P50 <= 0 ||
		loadedDownloadResult.LoadedLatency.P50 <= 0 ||
		loadedDownloadResult.LoadedProbeSuccessCount < minimumLatencyProbeSuccessCount {
		t.Fatalf("download latency-under-load result=%+v", loadedDownloadResult)
	}
}
