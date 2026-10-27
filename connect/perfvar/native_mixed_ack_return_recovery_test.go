// Controlled native mixed-data and ACK recovery tests use genuine gVisor
// request data and acknowledgements through the real H1/TUN path.
package perfvar

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// All counters are source-owned test attribution, not a phone memory result.
// Capacity counts include pooled backing slack; roots transferred into native
// admission remain subject to the unchanged production memory budgets.
type nativeMixedAckBrokerObservation struct {
	ControlledCoalescing                                                                           bool
	TemplateCopies, TemplateReturns                                                                int
	TemplatePacketBytes, TemplatePayloadBytes, ApplicationMtu                                      int
	CoverageAck                                                                                    uint32
	CoveredBeforeOriginRelease                                                                     bool
	ReadCount, MaximumReadPackets                                                                  int
	ForwardedFlushes, AckRunFlushesBeforeOrdinary                                                  int
	ReadyReadPacketHistogram                                                                       [65]uint64
	AckPackets, AckAccepted, AckRefused, AckOfferedBeforeReplay                                    int
	SameControlAckOffersBeforeRto, SameControlAckAcceptedBeforeRto, SameControlAckRefusedBeforeRto int
	DataAcceptedBeforeReplay, ReturnPureAcksBeforeReplay                                           int
	ReturnPureAcksAfterArm, HealthyNonAdvancingAcksPassed                                          int
	ControlledProviderAcks, ProviderTemplatePackets                                                int
	ProviderAckPackets, ProviderAckFrontierAdvances                                                int
	ProviderAckFrontier, ProviderBodyAckedBytes                                                    uint32
	NativeGroups, DataAccepted, DataRefused                                                        int
	GroupPacketHistogram                                                                           [5]uint64
	AckGroupHistogram                                                                              [4]uint64
	FirstFourObserved, FirstFourBeforeReplay                                                       bool
	FirstFourAccepted                                                                              [4]bool
	MaximumOwnedRoots, MaximumOwnedPacketBytes, MaximumOwnedCapacityBytes                          int
	OwnedRootsAfterClose                                                                           int
	CompletionReceipts                                                                             int
	CompletionValid                                                                                bool
	NatReplay, TunReplay                                                                           nativeMixedAckReplayObservation
	FirstReplayObservation                                                                         string
	Invalid                                                                                        bool
}

// The NAT public return callback follows the provider's internal handoff. It
// identifies a genuine emitted replay, but is not a pre-emission timestamp.
type nativeMixedAckReplayObservation struct {
	Boundary                                         string
	AfterOriginRelease                               time.Duration
	AckOffers, AckAccepted, AckRefused, ProviderSame int
}

// One existing bridge goroutine owns the request copy and transient ACK roots.
// It never waits for another read to fill a group. Receive callbacks publish
// only scalars and the request-coverage barrier; they never retain packets.
type nativeMixedAckBroker struct {
	state                               *nativeDuplicateAckRecoveryState
	path                                *fullTunPath
	read                                func([][]byte) (int, error)
	send                                func([][]byte, []bool) int
	applicationMtu                      int
	returnResendTimeout                 time.Duration
	loss                                bool
	armed                               atomic.Bool
	replayObserved                      atomic.Bool
	stateLock                           sync.Mutex
	coveredReady                        chan struct{}
	template                            []byte
	templateHeader                      nativeDuplicateAckTcpHeader
	templateSet, templateCovered        bool
	providerFrontierSet                 bool
	healthyFrontier                     uint32
	healthyFrontierSet                  bool
	templateHash                        [32]byte
	ackHashes                           [3][32]byte
	ackHashCount                        int
	ackHashSeen                         [3]bool
	natBasePackets                      int
	unsubscribe                         func()
	natUnsubscribe, natBatchUnsubscribe func()
	observation                         nativeMixedAckBrokerObservation
}

// Construction retains no packets, creates no worker, and changes no setting.
func newNativeMixedAckBroker(state *nativeDuplicateAckRecoveryState, loss bool) *nativeMixedAckBroker {
	return &nativeMixedAckBroker{
		state:        state,
		loss:         loss,
		coveredReady: make(chan struct{}),
		observation:  nativeMixedAckBrokerObservation{ControlledCoalescing: true, CompletionValid: true},
	}
}

// The override is installed before the normal fixture starts its bridge.
func (self *nativeMixedAckBroker) bind(path *fullTunPath) {
	self.path = path
	self.read = path.appTun.ReadBatch
	path.nativeBridgeReadBatchForTest = self.readBatch
	path.downloadCompletionPhaseForTest = self.observeCompletion
	self.natUnsubscribe = path.providerLocalNat.AddReceivePacketCallback(func(_ clientconnect.TransferPath, _ protocol.ProvideMode, _ *clientconnect.IpPath, packet []byte) {
		self.observeNatReturn(packet)
	})
	self.natBatchUnsubscribe = path.providerLocalNat.AddReceivePacketsCallback(func(_ clientconnect.TransferPath, _ protocol.ProvideMode, _ *clientconnect.IpPath, packets [][]byte) {
		for _, packet := range packets {
			self.observeNatReturn(packet)
		}
	})
}

// Only the workload's existing pre-origin barrier may await genuine coverage.
// This adds no timer and does not hold a packet receive callback or reader.
func (self *nativeMixedAckBroker) waitCovered(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-self.coveredReady:
		return nil
	}
}

// The source clock already precedes origin release. Publication through armed
// makes the immutable native sender visible to the bridge reader.
func (self *nativeMixedAckBroker) arm() error {
	self.state.stateLock.Lock()
	preface, base := self.state.bridgePreface, self.state.base
	self.state.stateLock.Unlock()
	self.stateLock.Lock()
	valid := self.templateSet && self.templateCovered &&
		self.templateHeader.sequence+uint32(self.templateHeader.payloadByteCount) == preface.Sequence &&
		self.templateHeader.ack == base && self.templateHeader.window == preface.Window
	self.observation.CoveredBeforeOriginRelease = valid
	self.observation.ApplicationMtu = self.applicationMtu
	self.stateLock.Unlock()
	if !valid {
		return fmt.Errorf("mixed native premise not exercised: request data lacks actual cumulative coverage")
	}
	self.send = nativeBrowsingBatchSender(self.path.multiClient, clientconnect.SourceId(self.path.deviceClientId))
	self.armed.Store(true)
	return nil
}

// Captures at most one genuine complete logical request segment. The original
// packet continues through the ordinary bridge; no bytes are rewritten.
func (self *nativeMixedAckBroker) captureRequest(packet []byte, header nativeDuplicateAckTcpHeader) {
	if self.template != nil || self.armed.Load() || header.payloadByteCount != logicalTCPFlowPrefaceLength ||
		header.flags&0x17 != 0x10 || self.applicationMtu <= 0 || len(packet) > self.applicationMtu {
		return
	}
	self.state.stateLock.Lock()
	flow, flowSet := self.state.flow, self.state.flowSet
	beforeStart := self.state.started.IsZero()
	self.state.stateLock.Unlock()
	if !flowSet || !beforeStart || !header.sameFlow(flow, true) || header.sequence != flow.ack {
		return
	}
	self.template = clientconnect.MessagePoolCopy(packet)
	self.stateLock.Lock()
	self.templateHeader, self.templateSet = header, true
	self.templateHash = sha256.Sum256(packet)
	self.observation.TemplateCopies++
	self.observation.TemplatePacketBytes = len(packet)
	self.observation.TemplatePayloadBytes = header.payloadByteCount
	self.observation.OwnedRootsAfterClose = 1
	self.observation.MaximumOwnedRoots = 1
	self.observation.MaximumOwnedPacketBytes = len(packet)
	self.observation.MaximumOwnedCapacityBytes = cap(packet)
	self.stateLock.Unlock()
}

// The real provider ACK proves that the exact request sequence interval is
// covered before any response data is released. No packet bytes are retained.
func (self *nativeMixedAckBroker) observeReturn(packet []byte) {
	header, valid := nativeDuplicateAckHeader(packet)
	if !valid {
		return
	}
	self.state.stateLock.Lock()
	base, replayed, flow, flowSet := self.state.base, !self.state.firstReplay.IsZero(), self.state.flow, self.state.flowSet
	self.state.stateLock.Unlock()
	if self.armed.Load() && flowSet && header.sameFlow(flow, false) && header.payloadByteCount > 0 && header.sequence == base && replayed {
		self.observeReplay("client-tun-before-injection")
	}
	if header.payloadByteCount != 0 || header.flags&0x17 != 0x10 {
		return
	}
	self.stateLock.Lock()
	if self.armed.Load() && header.sameFlow(self.templateHeader, true) {
		self.observation.ReturnPureAcksAfterArm++
	}
	if self.armed.Load() && self.loss && !self.replayObserved.Load() && header.sameFlow(self.templateHeader, true) {
		self.observation.ReturnPureAcksBeforeReplay++
	}
	closeReady := self.templateSet && !self.templateCovered && header.sameFlow(self.templateHeader, true) &&
		header.ack == self.templateHeader.sequence+uint32(self.templateHeader.payloadByteCount)
	if closeReady {
		self.templateCovered = true
		self.observation.CoverageAck = header.ack
	}
	self.stateLock.Unlock()
	if closeReady {
		close(self.coveredReady)
	}
}

// Public NAT callbacks borrow exact original/replay packets after internal
// return handoff. They only publish scalar sequence observations here.
func (self *nativeMixedAckBroker) observeNatReturn(packet []byte) {
	header, valid := nativeDuplicateAckHeader(packet)
	if !valid || header.payloadByteCount == 0 || !self.armed.Load() {
		return
	}
	self.state.stateLock.Lock()
	base := self.state.base
	self.state.stateLock.Unlock()
	self.stateLock.Lock()
	matched := header.sameFlow(self.templateHeader, true) && header.sequence == base
	if matched {
		self.natBasePackets++
	}
	replayed := matched && self.natBasePackets == 2
	self.stateLock.Unlock()
	if replayed {
		self.observeReplay("provider-nat-public-callback-after-internal-handoff")
	}
}

// Each label records its actual observer boundary. There is no claim that
// either callback precedes the provider's earlier internal enqueue operation.
func (self *nativeMixedAckBroker) observeReplay(boundary string) {
	self.state.stateLock.Lock()
	started, providerSame := self.state.started, self.state.providerSame
	self.state.stateLock.Unlock()
	self.stateLock.Lock()
	observation := nativeMixedAckReplayObservation{
		Boundary:           boundary,
		AfterOriginRelease: time.Since(started),
		AckOffers:          self.observation.AckPackets,
		AckAccepted:        self.observation.AckAccepted,
		AckRefused:         self.observation.AckRefused,
		ProviderSame:       providerSame,
	}
	if boundary == "client-tun-before-injection" && self.observation.TunReplay.Boundary == "" {
		self.observation.TunReplay = observation
	}
	if boundary != "client-tun-before-injection" && self.observation.NatReplay.Boundary == "" {
		self.observation.NatReplay = observation
	}
	if self.observation.FirstReplayObservation == "" {
		self.observation.FirstReplayObservation = boundary
	}
	self.stateLock.Unlock()
	self.replayObserved.Store(true)
}

// Provider ingress is post-Multi/Transfer evidence, not an ACK-application hook.
func (self *nativeMixedAckBroker) observeProvider(packet []byte) {
	header, valid := nativeDuplicateAckHeader(packet)
	if !valid || !self.armed.Load() {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !header.sameFlow(self.templateHeader, false) {
		return
	}
	if header.payloadByteCount != 0 {
		if sha256.Sum256(packet) == self.templateHash {
			self.observation.ProviderTemplatePackets++
		}
		return
	}
	if header.flags&0x17 != 0x10 {
		return
	}
	self.observation.ProviderAckPackets++
	hash := sha256.Sum256(packet)
	for index := 0; index < self.ackHashCount; index++ {
		if !self.ackHashSeen[index] && hash == self.ackHashes[index] {
			self.ackHashSeen[index] = true
			self.observation.ControlledProviderAcks++
			break
		}
	}
	if !self.providerFrontierSet || int32(header.ack-self.observation.ProviderAckFrontier) > 0 {
		if self.providerFrontierSet {
			self.observation.ProviderAckFrontierAdvances++
		}
		self.providerFrontierSet = true
		self.observation.ProviderAckFrontier = header.ack
	}
}

// The real origin must receive exactly the original completion marker. A
// duplicate request forwarded as fresh socket data would violate this fence.
func (self *nativeMixedAckBroker) observeCompletion(event fullTunDownloadCompletionEvent) {
	if event.Phase != "server-read" {
		return
	}
	self.stateLock.Lock()
	self.observation.CompletionReceipts++
	self.observation.CompletionValid = self.observation.CompletionValid && event.Count == 1 && event.Value == 1 && event.Err == nil
	self.stateLock.Unlock()
}

// Takes every ACK through the same consuming native entry used by the ordinary
// fixture. ACK bridge results map from exact original member indexes. The extra
// already-covered data share has separate accounting and is never called an ACK.
func (self *nativeMixedAckBroker) sendAcks(acks [][]byte) {
	var group [4][]byte
	group[0] = clientconnect.MessagePoolShareReadOnly(self.template)
	copy(group[1:], acks)
	self.state.stateLock.Lock()
	started, preface := self.state.started, self.state.bridgePreface
	self.state.stateLock.Unlock()
	beforeReplay := !self.replayObserved.Load()
	packetBytes, capacityBytes := len(self.template), cap(self.template)
	for _, packet := range acks {
		packetBytes += len(packet)
		capacityBytes += cap(packet)
	}
	var fullAccepted [4]bool
	send := func(_ [][]byte, accepted []bool) int {
		var sameControl [3]bool
		entryElapsed := time.Since(started)
		for index, packet := range acks {
			header, valid := nativeDuplicateAckHeader(packet)
			sameControl[index] = valid && (nativeDuplicateAckScalars{Sequence: header.sequence, Ack: header.ack, Window: header.window}) == preface
			self.stateLock.Lock()
			if self.ackHashCount < len(self.ackHashes) {
				self.ackHashes[self.ackHashCount] = sha256.Sum256(packet)
				self.ackHashCount++
			}
			if sameControl[index] && entryElapsed < self.returnResendTimeout {
				self.observation.SameControlAckOffersBeforeRto++
			}
			self.stateLock.Unlock()
		}
		count := self.send(group[:len(acks)+1], fullAccepted[:len(acks)+1])
		exitElapsed := time.Since(started)
		fullCount, ackCount := 0, 0
		for index, admitted := range fullAccepted[:len(acks)+1] {
			if admitted {
				fullCount++
				if index > 0 {
					ackCount++
				}
			}
		}
		copy(accepted, fullAccepted[1:len(acks)+1])
		self.stateLock.Lock()
		self.observation.Invalid = self.observation.Invalid || count != fullCount
		if exitElapsed < self.returnResendTimeout {
			for index := range acks {
				if sameControl[index] {
					if fullAccepted[index+1] {
						self.observation.SameControlAckAcceptedBeforeRto++
					} else {
						self.observation.SameControlAckRefusedBeforeRto++
					}
				}
			}
		}
		if beforeReplay && fullAccepted[0] {
			self.observation.DataAcceptedBeforeReplay++
		}
		self.stateLock.Unlock()
		return ackCount
	}
	ackCount := sendFullTunBridgeBatch(self.path.bridgeSends, acks, 0, time.Sleep, send)
	self.stateLock.Lock()
	self.observation.NativeGroups++
	self.observation.GroupPacketHistogram[len(acks)+1]++
	self.observation.AckGroupHistogram[len(acks)]++
	self.observation.AckAccepted += ackCount
	self.observation.AckRefused += len(acks) - ackCount
	if fullAccepted[0] {
		self.observation.DataAccepted++
	} else {
		self.observation.DataRefused++
	}
	if beforeReplay {
		self.observation.AckOfferedBeforeReplay += len(acks)
	}
	if len(acks) == 3 && !self.observation.FirstFourObserved {
		self.observation.FirstFourObserved = true
		self.observation.FirstFourBeforeReplay = beforeReplay
		self.observation.FirstFourAccepted = fullAccepted
	}
	self.observation.MaximumOwnedRoots = max(self.observation.MaximumOwnedRoots, 1+len(acks))
	self.observation.MaximumOwnedPacketBytes = max(self.observation.MaximumOwnedPacketBytes, packetBytes)
	self.observation.MaximumOwnedCapacityBytes = max(self.observation.MaximumOwnedCapacityBytes, capacityBytes)
	self.stateLock.Unlock()
}

// Earlier nonselected members keep their dispatch order. This is still a
// controlled coalescing experiment; the production sender keeps its own flow
// grouping and carrier scheduling behavior.
func (self *nativeMixedAckBroker) flushForwarded(packets [][]byte) {
	if self.path.beforeBridgeSendBatchForTest != nil {
		self.path.beforeBridgeSendBatchForTest(packets)
	}
	sendFullTunBridgeBatch(self.path.bridgeSends, packets, 0, time.Sleep, self.send)
	clear(packets)
	self.stateLock.Lock()
	self.observation.ForwardedFlushes++
	self.stateLock.Unlock()
}

// One normal blocking read is followed only by processing packets it already
// returned. Groups flush at three ACKs or at the end of that ready batch; no
// cross-read accumulation, extra read, artificial pause, or ACK timer exists.
func (self *nativeMixedAckBroker) readBatch(packets [][]byte) (int, error) {
	count, err := self.read(packets)
	if err != nil {
		self.closeOwned()
		return count, err
	}
	self.stateLock.Lock()
	self.observation.ReadCount++
	self.observation.MaximumReadPackets = max(self.observation.MaximumReadPackets, count)
	if count < len(self.observation.ReadyReadPacketHistogram) {
		self.observation.ReadyReadPacketHistogram[count]++
	} else {
		self.observation.Invalid = true
	}
	self.stateLock.Unlock()
	var acks [3][]byte
	ackCount, forwarded := 0, 0
	for index := 0; index < count; index++ {
		packet := packets[index]
		packets[index] = nil
		header, valid := nativeDuplicateAckHeader(packet)
		if valid {
			self.captureRequest(packet, header)
		}
		eligible := valid && self.armed.Load() && header.sameFlow(self.templateHeader, false) &&
			header.payloadByteCount == 0 && header.flags&0x17 == 0x10
		if eligible && self.loss {
			self.state.stateLock.Lock()
			replayed := !self.state.firstReplay.IsZero()
			self.state.stateLock.Unlock()
			eligible = !replayed && !self.replayObserved.Load()
		}
		if eligible && !self.loss {
			self.stateLock.Lock()
			frontier := self.templateHeader.ack
			if self.healthyFrontierSet {
				frontier = self.healthyFrontier
			}
			eligible = self.observation.AckPackets < 3 && int32(header.ack-frontier) > 0
			if eligible {
				self.healthyFrontier, self.healthyFrontierSet = header.ack, true
			} else if int32(header.ack-frontier) <= 0 {
				self.observation.HealthyNonAdvancingAcksPassed++
			}
			self.stateLock.Unlock()
		}
		if !eligible {
			if ackCount != 0 {
				self.sendAcks(acks[:ackCount])
				clear(acks[:])
				ackCount = 0
				self.stateLock.Lock()
				self.observation.AckRunFlushesBeforeOrdinary++
				self.stateLock.Unlock()
			}
			packets[forwarded], forwarded = packet, forwarded+1
			continue
		}
		if forwarded != 0 {
			self.flushForwarded(packets[:forwarded])
			forwarded = 0
		}
		self.state.observeOutgoing(packet, false)
		self.stateLock.Lock()
		self.observation.AckPackets++
		self.observation.Invalid = self.observation.Invalid || len(packet) > self.applicationMtu
		self.stateLock.Unlock()
		acks[ackCount], ackCount = packet, ackCount+1
		if !self.loss || ackCount == len(acks) {
			self.sendAcks(acks[:ackCount])
			clear(acks[:])
			ackCount = 0
		}
	}
	if ackCount != 0 {
		self.sendAcks(acks[:ackCount])
	}
	return forwarded, nil
}

// The read error follows TUN closure; the existing fixture joins that bridge
// before this idempotent cleanup is also called from its final pool fence.
func (self *nativeMixedAckBroker) closeOwned() {
	if self.template != nil {
		clientconnect.MessagePoolReturn(self.template)
		self.template = nil
		self.stateLock.Lock()
		self.observation.TemplateReturns++
		self.observation.OwnedRootsAfterClose = 0
		self.stateLock.Unlock()
	}
}

// Snapshots retain scalar receipts only. The common source snapshot supplies
// the exact matching duplicate-control census and before-RTO replay time.
func (self *nativeMixedAckBroker) snapshot() nativeMixedAckBrokerObservation {
	self.state.stateLock.Lock()
	base := self.state.base
	self.state.stateLock.Unlock()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	result := self.observation
	if self.providerFrontierSet {
		result.ProviderBodyAckedBytes = result.ProviderAckFrontier - base
	}
	return result
}

// Final validation follows the original complete fixture join. Healthy source
// arms must finish equal useful body work, admit every broker ACK, and advance
// the real provider ACK frontier through all response bytes.
func (self *nativeMixedAckBroker) reportAfterClose(t testing.TB, loss bool) {
	self.closeOwned()
	if self.unsubscribe != nil {
		self.unsubscribe()
	}
	if self.natUnsubscribe != nil {
		self.natUnsubscribe()
	}
	if self.natBatchUnsubscribe != nil {
		self.natBatchUnsubscribe()
	}
	observation := self.snapshot()
	encoded, _ := json.Marshal(observation)
	t.Logf("[perfvar] controlled-mixed-native-custody %s baseline_eligible=false", encoded)
	if observation.Invalid || observation.TemplateCopies != 1 || observation.TemplateReturns != 1 ||
		observation.OwnedRootsAfterClose != 0 || !observation.CoveredBeforeOriginRelease ||
		observation.MaximumOwnedRoots > 4 || observation.MaximumOwnedPacketBytes > 4*observation.ApplicationMtu ||
		observation.AckPackets != observation.AckAccepted+observation.AckRefused ||
		observation.CompletionReceipts != 1 || !observation.CompletionValid {
		t.Errorf("controlled mixed native ownership or actual-work receipt invalid: %+v", observation)
	}
	if !loss && (observation.AckPackets != 3 || observation.NativeGroups != 3 || observation.AckAccepted != 3 ||
		observation.DataAccepted != 3 || observation.DataRefused != 0 || observation.ProviderTemplatePackets != 3 || observation.ControlledProviderAcks != 3 ||
		observation.AckRefused != 0 || observation.ProviderAckFrontierAdvances == 0 ||
		observation.ProviderBodyAckedBytes < 2*1024*1024 || observation.ProviderBodyAckedBytes > 2*1024*1024+1) {
		t.Errorf("controlled mixed native healthy work was not equal: %+v", observation)
	}
}

// The no-loss control uses the same controlled coalescing and real body work.
func TestFullTunNativeMixedCoveredDataHealthyDownload(t *testing.T) {
	testFullTunNativeDownloadDuplicateAckRecoveryMode(t, false, true)
}

// Three genuine unchanged ACKs may occur across several ready mixed offers;
// an exact four-member offer is reported when naturally available.
func TestFullTunNativeMixedCoveredDataDownloadRepairsBeforeRto(t *testing.T) {
	testFullTunNativeDownloadDuplicateAckRecoveryMode(t, true, true)
}
