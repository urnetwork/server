// Opt-in first-navigation timing retains bounded scalar observations only.
// It never wakes discovery, delays packets, changes policy or grants admission.
package perfvar

import (
	"encoding/json"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
)

const sdkBrowsingPage1TimelineFlag = "CONNECT_PERFVAR_PAGE1_PHASE_DIAGNOSTIC"

// Four new origin connections and sixteen calls per candidate are the fixed
// diagnostic ceiling. The actual HTTP GotConn tuple selects its winner later.
type sdkBrowsingPage1Timeline struct {
	stateLock                                                                                              sync.Mutex
	tap                                                                                                    time.Time
	stopped                                                                                                bool
	providerId                                                                                             string
	selected                                                                                               int
	gotConnNs, firstByteNs                                                                                 int64
	gotConnKnown, firstByteKnown                                                                           bool
	unknownGotConn, unknownOrigin, generationConflicts                                                     int
	flowCount, flowOverflow, offerOverflow, discoveryCount, discoveryOverflow, originCount, originOverflow int
	flows                                                                                                  [4]sdkBrowsingPage1Flow
	discovery                                                                                              [16]sdkBrowsingPage1Discovery
	origins                                                                                                [8]sdkBrowsingPage1Origin
	page1Origin                                                                                            int
	handlerNs                                                                                              int64
}

// No packet, request body or credential survives an observation call.
type sdkBrowsingPage1Flow struct {
	Path                clientconnect.Ip4Path `json:"path"`
	Sequence            uint32                `json:"initial_syn_sequence"`
	Source, Destination string
	SynAckNs            int64                     `json:"syn_ack_ns"`
	SynAckNumber        uint32                    `json:"syn_ack_number"`
	SynAckKnown         bool                      `json:"syn_ack_known"`
	OfferCount          int                       `json:"offer_count"`
	Offers              [16]sdkBrowsingPage1Offer `json:"offers"`
}

type sdkBrowsingPage1Offer struct {
	ReadNs, OfferNs, ReturnNs                        int64
	BatchPackets, MatchingPackets, AcceptedAggregate int
	SynPackets                                       int
	AcceptanceKnown, OfferKnown, ReturnKnown         bool
}

type sdkBrowsingPage1Discovery struct {
	StartNs, EndNs                                   int64
	Rank                                             string
	LateReadyAtStart, ReturnedLate, Failed, EndKnown bool
	lateId                                           clientconnect.Id
}

type sdkBrowsingPage1Origin struct {
	StartNs, EndNs   int64
	Ordinal          int
	Failed, EndKnown bool
	Socket           *sdkBrowsingOriginConn `json:"-"`
}

// Observe the existing controller tap without changing any deadline.
func (self *sdkBrowsingPage1Timeline) arm(tap time.Time) {
	if self == nil {
		return
	}
	self.stateLock.Lock()
	self.tap = tap
	self.stateLock.Unlock()
}

// Bind the completed fixture constructor to its exact remote identity.
func (self *sdkBrowsingPage1Timeline) late(id clientconnect.Id) {
	if self == nil {
		return
	}
	self.stateLock.Lock()
	self.providerId = id.String()
	self.stateLock.Unlock()
}

// Read owns its pooled packets. Only scalar headers are copied; one token
// per matching candidate associates the later aggregate SDK return safely.
func (self *sdkBrowsingPage1Timeline) nativeRead(packets [][]byte) (tokens [4]int) {
	if self == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.tap.IsZero() || self.stopped {
		return
	}
	now := int64(time.Since(self.tap))
	for _, packet := range packets {
		path, err := clientconnect.ParseIpPath(packet)
		if err != nil || path.Version != 4 || path.Protocol != clientconnect.IpProtocolTcp ||
			!path.DestinationIp.Equal(net.IPv4(203, 0, 113, 11)) || path.DestinationPort != 443 {
			continue
		}
		key := path.ToIp4Path()
		found := -1
		for index := 0; index < self.flowCount; index++ {
			if self.flows[index].Path == key {
				found = index
				break
			}
		}
		if found == -1 {
			if !path.Syn || path.Ack {
				continue
			}
			if len(self.flows) <= self.flowCount {
				self.flowOverflow++
				continue
			}
			found = self.flowCount
			self.flowCount++
			self.flows[found] = sdkBrowsingPage1Flow{Path: key, Sequence: path.SequenceNumber,
				Source:      net.JoinHostPort(path.SourceIp.String(), strconv.Itoa(path.SourcePort)),
				Destination: net.JoinHostPort(path.DestinationIp.String(), strconv.Itoa(path.DestinationPort))}
		}
		flow := &self.flows[found]
		if path.Syn && !path.Ack && path.SequenceNumber != flow.Sequence {
			self.generationConflicts++
			continue
		}
		if tokens[found] == 0 {
			if len(flow.Offers) <= flow.OfferCount {
				self.offerOverflow++
				continue
			}
			flow.OfferCount++
			tokens[found] = flow.OfferCount
			flow.Offers[flow.OfferCount-1] = sdkBrowsingPage1Offer{ReadNs: now, BatchPackets: len(packets), AcceptedAggregate: -1}
		}
		offer := &flow.Offers[tokens[found]-1]
		offer.MatchingPackets++
		if path.Syn && !path.Ack {
			offer.SynPackets++
		}
	}
	return
}

// Observe the actual compact SDK call before entering it.
func (self *sdkBrowsingPage1Timeline) nativeOffer(tokens [4]int) {
	if self == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for index, token := range tokens {
		if token != 0 {
			self.flows[index].Offers[token-1].OfferNs = int64(time.Since(self.tap))
			self.flows[index].Offers[token-1].OfferKnown = true
		}
	}
}

// Partial aggregate acceptance cannot attest an individual member. Completed
// calls keep their registered token even after the first-byte seal.
func (self *sdkBrowsingPage1Timeline) nativeReturned(tokens [4]int, accepted int32) {
	if self == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for index, token := range tokens {
		if token == 0 {
			continue
		}
		offer := &self.flows[index].Offers[token-1]
		offer.ReturnNs, offer.AcceptedAggregate, offer.ReturnKnown = int64(time.Since(self.tap)), int(accepted), true
		offer.AcceptanceKnown = accepted == 0 || int(accepted) == offer.BatchPackets
	}
}

// Borrow returned packets only long enough to match exact SYN-ACK proof.
func (self *sdkBrowsingPage1Timeline) received(packets [][]byte) {
	if self == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.tap.IsZero() || self.stopped {
		return
	}
	for _, packet := range packets {
		path, err := clientconnect.ParseIpPath(packet)
		if err != nil || path.Version != 4 || path.Protocol != clientconnect.IpProtocolTcp || !path.Syn || !path.Ack {
			continue
		}
		key := path.Reverse().ToIp4Path()
		for index := 0; index < self.flowCount; index++ {
			flow := &self.flows[index]
			if flow.Path == key && !flow.SynAckKnown && path.AckSequenceNumber == flow.Sequence+1 {
				flow.SynAckNs, flow.SynAckNumber, flow.SynAckKnown = int64(time.Since(self.tap)), path.AckSequenceNumber, true
			}
		}
	}
}

// Actual authenticated HTTP acquisition selects a recorded tuple. Unknown or
// overflowed attribution remains explicit instead of choosing the first SYN.
func (self *sdkBrowsingPage1Timeline) gotConn(connection net.Conn) {
	if self == nil {
		return
	}
	source, destination := "", ""
	if connection != nil {
		source, destination = connection.LocalAddr().String(), connection.RemoteAddr().String()
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.gotConnNs, self.gotConnKnown, self.selected = int64(time.Since(self.tap)), true, 0
	for index := 0; index < self.flowCount; index++ {
		if self.flows[index].Source == source && self.flows[index].Destination == destination {
			self.selected = index + 1
		}
	}
	if self.selected == 0 {
		self.unknownGotConn++
	}
}

// Stop new observations at the original HTTP first-byte callback.
func (self *sdkBrowsingPage1Timeline) firstByte() {
	if self == nil {
		return
	}
	self.stateLock.Lock()
	if !self.tap.IsZero() && !self.firstByteKnown {
		self.firstByteNs = int64(time.Since(self.tap))
		self.firstByteKnown = true
		self.stopped = true
	}
	self.stateLock.Unlock()
}

// Reserve one fixed observation before the actual discovery API call.
func (self *sdkBrowsingPage1Timeline) discoveryStart(rank string, ready clientconnect.Id) int {
	if self == nil {
		return 0
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.tap.IsZero() || self.stopped {
		return 0
	}
	if len(self.discovery) <= self.discoveryCount {
		self.discoveryOverflow++
		return 0
	}
	self.discoveryCount++
	self.discovery[self.discoveryCount-1] = sdkBrowsingPage1Discovery{StartNs: int64(time.Since(self.tap)), Rank: rank, LateReadyAtStart: ready != (clientconnect.Id{}), lateId: ready}
	return self.discoveryCount
}

// Complete only its registered call with actual returned provider identity.
func (self *sdkBrowsingPage1Timeline) discoveryEnd(token int, result *clientconnect.FindProviders2Result, err error) {
	if self == nil || token == 0 {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	event := &self.discovery[token-1]
	event.EndNs, event.Failed, event.EndKnown = int64(time.Since(self.tap)), err != nil, true
	if result != nil && event.lateId != (clientconnect.Id{}) {
		for _, provider := range result.Providers {
			if provider != nil && provider.ClientId == event.lateId {
				event.ReturnedLate = true
			}
		}
	}
}

// Observe only late4 new origin1 dials, with a fixed eight-call bound.
func (self *sdkBrowsingPage1Timeline) originStart(ordinal int, address string) int {
	if self == nil || ordinal != 4 || address != "203.0.113.11:443" {
		return 0
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.tap.IsZero() || self.stopped {
		return 0
	}
	if len(self.origins) <= self.originCount {
		self.originOverflow++
		return 0
	}
	self.originCount++
	self.origins[self.originCount-1] = sdkBrowsingPage1Origin{StartNs: int64(time.Since(self.tap)), Ordinal: ordinal}
	return self.originCount
}

// Associate the completed dial with its existing owned socket.
func (self *sdkBrowsingPage1Timeline) originEnd(token int, socket *sdkBrowsingOriginConn, err error) {
	if self == nil || token == 0 {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	event := &self.origins[token-1]
	event.EndNs, event.Failed, event.Socket, event.EndKnown = int64(time.Since(self.tap)), err != nil, socket, true
}

// The real page1 handler identifies its exact provider-origin socket.
func (self *sdkBrowsingPage1Timeline) handler(socket *sdkBrowsingOriginConn) {
	if self == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.handlerNs, self.page1Origin = int64(time.Since(self.tap)), 0
	for index := 0; index < self.originCount; index++ {
		if socket != nil && self.origins[index].Socket == socket {
			self.page1Origin = index + 1
		}
	}
	if self.page1Origin == 0 {
		self.unknownOrigin++
	}
}

// Joined fixture cleanup has ended all callbacks before this bounded report.
func (self *sdkBrowsingPage1Timeline) log(t testing.TB) {
	if self == nil {
		return
	}
	self.stateLock.Lock()
	report := struct {
		Scope                                                          string `json:"scope"`
		TapUnixNs                                                      int64  `json:"tap_unix_ns"`
		LateProviderId                                                 string `json:"late_provider_id"`
		SelectedCandidate                                              int    `json:"selected_candidate"`
		GotConnNs, FirstByteNs                                         int64
		GotConnKnown, FirstByteKnown                                   bool
		UnknownGotConn, UnknownOrigin, GenerationConflicts             int
		Candidates                                                     []sdkBrowsingPage1Flow
		Discovery                                                      []sdkBrowsingPage1Discovery
		Origins                                                        []sdkBrowsingPage1Origin
		Page1Origin                                                    int
		HandlerNs                                                      int64
		FlowOverflow, OfferOverflow, DiscoveryOverflow, OriginOverflow int
	}{Scope: "diagnostic only; exact original100 workload/oracles; fixed4 TCPcandidates x16offers,16discovery,8origin dials; aggregate acceptance never invented per packet; APIvisibility not admission; no internal clientAdded/ping/replay hook", TapUnixNs: self.tap.UnixNano(), LateProviderId: self.providerId,
		SelectedCandidate: self.selected, GotConnNs: self.gotConnNs, FirstByteNs: self.firstByteNs, GotConnKnown: self.gotConnKnown, FirstByteKnown: self.firstByteKnown, UnknownGotConn: self.unknownGotConn, UnknownOrigin: self.unknownOrigin, GenerationConflicts: self.generationConflicts,
		Candidates: append([]sdkBrowsingPage1Flow(nil), self.flows[:self.flowCount]...), Discovery: append([]sdkBrowsingPage1Discovery(nil), self.discovery[:self.discoveryCount]...), Origins: append([]sdkBrowsingPage1Origin(nil), self.origins[:self.originCount]...),
		Page1Origin: self.page1Origin, HandlerNs: self.handlerNs, FlowOverflow: self.flowOverflow, OfferOverflow: self.offerOverflow, DiscoveryOverflow: self.discoveryOverflow, OriginOverflow: self.originOverflow}
	self.stateLock.Unlock()
	encoded, err := json.Marshal(report)
	if err != nil || len(encoded) > 48*1024 {
		t.Errorf("bounded page1 timeline report: bytes=%d err=%v", len(encoded), err)
		return
	}
	t.Logf("[perfvar] sdk-browsing-page1-timeline %s", encoded)
}
