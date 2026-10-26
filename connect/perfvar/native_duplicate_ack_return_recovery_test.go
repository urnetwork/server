// Controlled full-TUN tests exercise real duplicate acknowledgements and recovery.
// A lost first response segment makes real gVisor produce unchanged ACKs while
// the original full-TUN download and its concurrent UDP probes stay intact.
package perfvar

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
)

// Only scalar headers survive the borrowed callback. These tests use the
// existing IPv4 bulk fixture; unrelated UDP probes never enter this parser.
type nativeDuplicateAckTcpHeader struct {
	sourceIp, destinationIp     [4]byte
	sourcePort, destinationPort uint16
	sequence, ack               uint32
	window                      uint16
	flags                       byte
	payloadByteCount            int
}

// Reads fixed IPv4/TCP fields without retaining or allocating packet storage.
func nativeDuplicateAckHeader(packet []byte) (header nativeDuplicateAckTcpHeader, ok bool) {
	if len(packet) < 40 || packet[0]>>4 != 4 || packet[9] != 6 {
		return header, false
	}
	ipHeaderByteCount := int(packet[0]&15) * 4
	packetByteCount := int(binary.BigEndian.Uint16(packet[2:4]))
	if ipHeaderByteCount < 20 || packetByteCount > len(packet) || packetByteCount < ipHeaderByteCount+20 ||
		binary.BigEndian.Uint16(packet[6:8])&0x3fff != 0 {
		return header, false
	}
	tcp := packet[ipHeaderByteCount:packetByteCount]
	tcpHeaderByteCount := int(tcp[12]>>4) * 4
	if tcpHeaderByteCount < 20 || len(tcp) < tcpHeaderByteCount {
		return header, false
	}
	copy(header.sourceIp[:], packet[12:16])
	copy(header.destinationIp[:], packet[16:20])
	header.sourcePort, header.destinationPort = binary.BigEndian.Uint16(tcp[:2]), binary.BigEndian.Uint16(tcp[2:4])
	header.sequence, header.ack = binary.BigEndian.Uint32(tcp[4:8]), binary.BigEndian.Uint32(tcp[8:12])
	header.window, header.flags = binary.BigEndian.Uint16(tcp[14:16]), tcp[13]
	header.payloadByteCount = len(tcp) - tcpHeaderByteCount
	return header, true
}

// The tuple is taken from the measured flow's real SYN-ACK. Reversal matches
// the actual application output and provider ingress without a port-only join.
func (self nativeDuplicateAckTcpHeader) sameFlow(other nativeDuplicateAckTcpHeader, reverse bool) bool {
	if reverse {
		return self.sourceIp == other.destinationIp && self.destinationIp == other.sourceIp &&
			self.sourcePort == other.destinationPort && self.destinationPort == other.sourcePort
	}
	return self.sourceIp == other.sourceIp && self.destinationIp == other.destinationIp &&
		self.sourcePort == other.sourcePort && self.destinationPort == other.destinationPort
}

type nativeDuplicateAckScalars struct {
	Sequence, Ack uint32
	Window        uint16
}

// The fixed census adds no sampler, queue, packet root, or recovery timer.
// stateLock guards only scalar updates and is never held across packet I/O.
type nativeDuplicateAckRecoveryState struct {
	stateLock                            sync.Mutex
	loss                                 bool
	targetPort                           uint16
	flow                                 nativeDuplicateAckTcpHeader
	flowSet                              bool
	base                                 uint32
	bridgePreface, providerPreface       nativeDuplicateAckScalars
	bridgePrefaceSet, providerPrefaceSet bool
	started, firstDrop, firstReplay      time.Time
	firstDataPackets, laterPackets       int
	droppedPackets                       int
	dataBeforeStart                      int
	generatedSame, providerSame          int
	generatedChanged, providerChanged    int
	generatedAtReplay, providerAtReplay  int
	generatedTimes, providerTimes        [3]time.Duration
	legacyFrames, invalidFrames          int
	invalid                              bool
}

// Watches the actual ReadBatch result before Multi takes any packet. GRO
// remains enabled, so the census counts emitted ACKs, never input segments.
func (self *nativeDuplicateAckRecoveryState) observeOutgoing(packet []byte, provider bool) {
	header, ok := nativeDuplicateAckHeader(packet)
	if !ok {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.flowSet || !header.sameFlow(self.flow, true) {
		return
	}
	if self.started.IsZero() && header.payloadByteCount > 0 {
		control := nativeDuplicateAckScalars{Sequence: header.sequence + uint32(header.payloadByteCount), Ack: header.ack, Window: header.window}
		if provider {
			self.providerPreface, self.providerPrefaceSet = control, true
		} else {
			self.bridgePreface, self.bridgePrefaceSet = control, true
		}
		return
	}
	if self.firstDrop.IsZero() || header.payloadByteCount != 0 || header.flags&0x17 != 0x10 || header.ack != self.base {
		return
	}
	control := nativeDuplicateAckScalars{Sequence: header.sequence, Ack: header.ack, Window: header.window}
	if control != self.bridgePreface {
		if provider {
			self.providerChanged++
		} else {
			self.generatedChanged++
		}
		return
	}
	if provider {
		if self.providerSame < len(self.providerTimes) {
			self.providerTimes[self.providerSame] = time.Since(self.started)
		}
		self.providerSame++
	} else {
		if self.generatedSame < len(self.generatedTimes) {
			self.generatedTimes[self.generatedSame] = time.Since(self.started)
		}
		self.generatedSame++
	}
}

// The first dropped response has no earlier payload custody on this fresh
// zero-warmup flow. Every later original or replay passes unchanged to gVisor.
func (self *nativeDuplicateAckRecoveryState) admitReturn(packet []byte) bool {
	header, ok := nativeDuplicateAckHeader(packet)
	if !ok {
		return true
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if header.sourcePort == self.targetPort && header.flags&0x12 == 0x12 {
		if self.flowSet && (!header.sameFlow(self.flow, false) || header.sequence+1 != self.base) {
			self.invalid = true
		}
		self.flow, self.flowSet, self.base = header, true, header.sequence+1
	}
	if !self.flowSet || !header.sameFlow(self.flow, false) || header.payloadByteCount == 0 {
		return true
	}
	if self.started.IsZero() {
		self.dataBeforeStart++
		return true
	}
	if header.sequence != self.base {
		self.laterPackets++
		return true
	}
	self.firstDataPackets++
	if self.loss && self.firstDrop.IsZero() {
		self.firstDrop = time.Now()
		self.droppedPackets++
		return false
	}
	if self.loss && self.firstReplay.IsZero() {
		self.firstReplay = time.Now()
		self.generatedAtReplay, self.providerAtReplay = self.generatedSame, self.providerSame
	}
	return true
}

// The lower timer bound precedes origin release; the original helper then
// closes startMeasured before its first origin Write. No timeout is changed.
func (self *nativeDuplicateAckRecoveryState) start() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.invalid || !self.flowSet || !self.bridgePrefaceSet || !self.providerPrefaceSet ||
		self.bridgePreface != self.providerPreface || self.bridgePreface.Ack != self.base || self.dataBeforeStart != 0 {
		return fmt.Errorf("fresh flow did not join identical native/provider preface state before origin release")
	}
	self.started = time.Now()
	return nil
}

// Healthy native delivery retains the original full-TUN/H1 bulk/probe gates.
func TestFullTunNativeDownloadHealthyReturnWithoutReplay(t *testing.T) {
	testFullTunNativeDownloadDuplicateAckRecovery(t, false)
}

// A real gVisor ACK train must reach the provider and repair before its timer.
func TestFullTunNativeDownloadDuplicateAcksRepairBeforeRto(t *testing.T) {
	testFullTunNativeDownloadDuplicateAckRecovery(t, true)
}

// Both top-level cases use the same plain full-TUN fixture and workload. This
// is separate from SDK memory qualification and historical browsing attribution.
func testFullTunNativeDownloadDuplicateAckRecovery(t *testing.T, loss bool) {
	t.Helper()
	if testing.Short() {
		t.Skip("full H1/TUN recovery topology")
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	var deferredCausalFailure string
	testEnvironment.Run(t, func(t testing.TB) {
		state := &nativeDuplicateAckRecoveryState{loss: loss}
		var returnResendTimeout time.Duration
		var tcpGro bool
		hooks := &fullTunConstructionTestHooks{
			configureProviderLocalNatSettings: func(settings *clientconnect.LocalUserNatSettings) {
				returnResendTimeout = settings.TcpBufferSettings.ReturnResendTimeout
			},
			configureApplicationTunSettings: func(settings *clientconnect.TunSettings) { tcpGro = settings.TcpGro },
			afterStage: func(stage fullTunConstructionStage, path *fullTunPath) error {
				if stage == fullTunConstructionStageMultiClient {
					path.nativeIngressForTest = true
					path.beforeBridgeSendBatchForTest = func(packets [][]byte) {
						for _, packet := range packets {
							state.observeOutgoing(packet, false)
						}
					}
				}
				return nil
			},
		}
		poolBefore := routeMessagePoolOutstanding()
		profile := initialNetworkProfiles(4017)["clean-lan"]
		fixture, err := newPerfvarCorrectnessFixtureWithHooks(t, fullTunRouteExchangeH1, profile, profile, profile,
			defaultTunResourceProfile(), 5*time.Minute, hooks)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			fixture.close()
			if after, balanced := routeMessagePoolBalance(poolBefore); !balanced {
				t.Errorf("joined full-TUN pool ownership: %d -> %d classes=%v", poolBefore, after.outstanding, after.classes)
			}
		}()
		path := fixture.path
		_, native := any(path.multiClient).(nativeBrowsingResultsClient)
		if !native || returnResendTimeout <= 0 {
			t.Fatalf("native capability or actual provider timer absent: native=%t timeout=%s", native, returnResendTimeout)
		}
		path.beforeWorkloadClientDialForTest = func(_ context.Context, address string) error {
			_, port, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			value, err := strconv.ParseUint(port, 10, 16)
			state.stateLock.Lock()
			state.targetPort = uint16(value)
			state.stateLock.Unlock()
			return err
		}
		unsub := path.providerClient.AddReceiveCallback(func(_ clientconnect.TransferPath, frames []*protocol.Frame, _ clientconnect.Peer) {
			for _, frame := range frames {
				if frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
					continue
				}
				packet := frame.MessageBytes
				if !frame.Raw {
					message, err := clientconnect.FromFrame(frame)
					state.stateLock.Lock()
					state.legacyFrames++
					if err != nil {
						state.invalidFrames++
					}
					state.stateLock.Unlock()
					if err != nil {
						continue
					}
					packet = message.(*protocol.IpPacketToProvider).IpPacket.PacketBytes
				}
				state.observeOutgoing(packet, true)
			}
		})
		defer unsub()
		path.multiClient.SetReceivePacketCallback(func(_ clientconnect.TransferPath, _ protocol.ProvideMode, _ *clientconnect.IpPath, packet []byte) {
			if state.admitReturn(packet) {
				_, _ = path.appTun.Write(packet)
			}
		})
		path.multiClient.SetReceivePacketsCallback(func(_ clientconnect.TransferPath, _ protocol.ProvideMode, _ *clientconnect.IpPath, packets [][]byte) {
			admittedPackets := make([][]byte, 0, len(packets))
			for _, packet := range packets {
				if state.admitReturn(packet) {
					admittedPackets = append(admittedPackets, packet)
				}
			}
			_, _ = path.appTun.WriteBatch(admittedPackets)
		})
		const bulkByteCount = int64(2 * 1024 * 1024)
		observation, measureErr := fixture.measure(perfvarWorkloadLatencyUnderLoad, perfvarDirectionDownload,
			func(ctx context.Context, path *fullTunPath) (workloadResult, error) {
				previousDatagramAllowance := path.setAllowProviderDatagramPackFailures(true)
				defer path.setAllowProviderDatagramPackFailures(previousDatagramAllowance)
				result, err := measureFullTunLatencyUnderLoadDirectionWithStartHook(ctx, path, bulkByteCount, false, state.start)
				if err == nil {
					err = path.waitForPostWorkloadBoundary(ctx)
				}
				return result, err
			})
		state.stateLock.Lock()
		dropAfterStart, repairAfterStart, blockedUntilRepair := time.Duration(0), time.Duration(0), time.Duration(0)
		if !state.firstDrop.IsZero() {
			dropAfterStart = state.firstDrop.Sub(state.started)
		}
		if !state.firstReplay.IsZero() {
			repairAfterStart = state.firstReplay.Sub(state.started)
			blockedUntilRepair = state.firstReplay.Sub(state.firstDrop)
		}
		census := struct {
			Loss                           bool
			Native                         bool
			TcpGro                         bool
			Control                        nativeDuplicateAckScalars
			ConfiguredReturnResendTimeout  time.Duration
			FirstDropAfterOriginRelease    time.Duration
			FirstReplayAfterOriginRelease  time.Duration
			ReceiverHoleDuration           time.Duration
			GeneratedSame                  int
			ProviderSame                   int
			GeneratedChanged               int
			ProviderChanged                int
			GeneratedAtReplay              int
			ProviderAtReplay               int
			GeneratedAckAfterOriginRelease [3]time.Duration
			ProviderAckAfterOriginRelease  [3]time.Duration
			FirstSegmentCopies             int
			LaterReturnPackets             int
			ForcedDrops                    int
			DataBeforeOriginRelease        int
			LegacyFrames                   int
			InvalidFrames                  int
			Invalid                        bool
		}{
			Loss:                           loss,
			Native:                         native,
			TcpGro:                         tcpGro,
			Control:                        state.bridgePreface,
			ConfiguredReturnResendTimeout:  returnResendTimeout,
			FirstDropAfterOriginRelease:    dropAfterStart,
			FirstReplayAfterOriginRelease:  repairAfterStart,
			ReceiverHoleDuration:           blockedUntilRepair,
			GeneratedSame:                  state.generatedSame,
			ProviderSame:                   state.providerSame,
			GeneratedChanged:               state.generatedChanged,
			ProviderChanged:                state.providerChanged,
			GeneratedAtReplay:              state.generatedAtReplay,
			ProviderAtReplay:               state.providerAtReplay,
			GeneratedAckAfterOriginRelease: state.generatedTimes,
			ProviderAckAfterOriginRelease:  state.providerTimes,
			FirstSegmentCopies:             state.firstDataPackets,
			LaterReturnPackets:             state.laterPackets,
			ForcedDrops:                    state.droppedPackets,
			DataBeforeOriginRelease:        state.dataBeforeStart,
			LegacyFrames:                   state.legacyFrames,
			InvalidFrames:                  state.invalidFrames,
			Invalid:                        state.invalid,
		}
		state.stateLock.Unlock()
		encoded, _ := json.Marshal(census)
		t.Logf("[perfvar] native-duplicate-ack-recovery %s workload_duration=%s useful_bytes=%d content_hash=%s loaded_probe_successes=%d measure_error=%v baseline_eligible=false", encoded,
			observation.Result.Duration, observation.Result.UsefulByteCount, observation.Result.ContentHash, observation.Result.LoadedProbeSuccessCount, measureErr)
		if measureErr != nil {
			t.Fatal(measureErr)
		}
		result := observation.Result
		if result.UsefulByteCount != bulkByteCount || result.ContentHash != deterministicPayloadHash(bulkByteCount) ||
			result.IdleLatency.P50 <= 0 || result.PostLoadLatency.P50 <= 0 || result.LoadedProbeSuccessCount < minimumLatencyProbeSuccessCount {
			t.Fatalf("unchanged latency-under-load contract: %+v", result)
		}
		if census.Invalid || census.InvalidFrames != 0 || census.DataBeforeOriginRelease != 0 {
			t.Fatal("fresh measured flow provenance was invalid")
		}
		if !loss {
			if census.ForcedDrops != 0 || census.FirstSegmentCopies != 1 {
				t.Fatal("healthy control dropped or replayed its first response segment")
			}
			return
		}
		if census.ForcedDrops != 1 || census.FirstSegmentCopies < 2 || census.LaterReturnPackets < 3 ||
			census.GeneratedSame < 3 || census.GeneratedChanged != 0 || census.ProviderChanged != 0 {
			t.Fatal("mechanism NOT_EXERCISED: need one first-segment loss and three real unchanged gVisor ACKs with no changing-control repair")
		}
		if census.ProviderSame < 3 {
			deferredCausalFailure = fmt.Sprintf("real duplicate ACKs disappeared between native bridge and provider ingress: generated=%d provider=%d", census.GeneratedSame, census.ProviderSame)
			return
		}
		if census.GeneratedAtReplay < 3 || repairAfterStart <= 0 || returnResendTimeout <= repairAfterStart {
			t.Fatalf("missing first byte did not repair before the unchanged provider timer: repair=%s timer=%s", repairAfterStart, returnResendTimeout)
		}
	})
	if deferredCausalFailure != "" {
		t.Fatalf("%s", deferredCausalFailure)
	}
}
