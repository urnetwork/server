// Scalar recorder controls force hedged identity, aggregate uncertainty and
// bounded overflow independently of a hosted network or wall-clock threshold.
package perfvar

import (
	"encoding/binary"
	"net"
	"testing"
	"testing/synctest"
	"time"

	clientconnect "github.com/urnetwork/connect"
)

// Only the address accessors are used by the actual GotConn observer.
type sdkBrowsingPage1TraceTestConn struct {
	net.Conn
	local, remote net.Addr
}

func (self *sdkBrowsingPage1TraceTestConn) LocalAddr() net.Addr  { return self.local }
func (self *sdkBrowsingPage1TraceTestConn) RemoteAddr() net.Addr { return self.remote }

// Reuse canonical parseable TCP framing with visibly synthetic endpoints.
func sdkBrowsingPage1TraceTestSyn(port uint16, sequence uint32) []byte {
	packet := bridgeAdmissionTestPacket(true, port)
	copy(packet[16:20], []byte{203, 0, 113, 11})
	binary.BigEndian.PutUint16(packet[22:24], 443)
	binary.BigEndian.PutUint32(packet[24:28], sequence)
	packet[33] = 0x02
	return packet
}

// A second staggered TCP tuple must not inherit the first tuple's SYN clock.
// A subsequent unknown callback must also revoke the previous attribution.
func TestSdkBrowsingPage1TimelineSelectsActualHedgedTuple(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		trace := &sdkBrowsingPage1Timeline{}
		trace.arm(time.Now())
		first, second := sdkBrowsingPage1TraceTestSyn(41001, 101), sdkBrowsingPage1TraceTestSyn(41002, 201)
		for _, packet := range [][]byte{first, second} {
			tokens := trace.nativeRead([][]byte{packet})
			trace.nativeOffer(tokens)
			trace.nativeReturned(tokens, 1)
		}
		connection := &sdkBrowsingPage1TraceTestConn{local: &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 41002}, remote: &net.TCPAddr{IP: net.IPv4(203, 0, 113, 11), Port: 443}}
		trace.gotConn(connection)
		reply := sdkBrowsingPage1TraceTestSyn(443, 7000)
		copy(reply[12:16], []byte{203, 0, 113, 11})
		copy(reply[16:20], []byte{192, 0, 2, 1})
		binary.BigEndian.PutUint16(reply[22:24], 41002)
		binary.BigEndian.PutUint32(reply[28:32], 203)
		reply[33] = 0x12
		trace.received([][]byte{reply})
		if trace.flows[1].SynAckKnown {
			t.Fatal("unmatched SYN-ACK generation was attributed to the hedged winner")
		}
		binary.BigEndian.PutUint32(reply[28:32], 202)
		trace.received([][]byte{reply})
		if trace.selected != 2 || !trace.gotConnKnown || !trace.flows[1].SynAckKnown || trace.flows[0].SynAckKnown || trace.flows[1].SynAckNumber != 202 {
			t.Fatal("PAGE1_TRACE_HEDGED_WINNER_MISATTRIBUTED: real GotConn/SYN-ACK did not select the second exact tuple")
		}
		connection.local = &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 49999}
		trace.gotConn(connection)
		origin := trace.originStart(4, "203.0.113.11:443")
		owned := &sdkBrowsingOriginConn{ordinal: 4}
		trace.originEnd(origin, owned, nil)
		trace.handler(owned)
		if trace.page1Origin != origin {
			t.Fatal("actual owned origin socket did not join its dial")
		}
		trace.handler(&sdkBrowsingOriginConn{ordinal: 4})
		if trace.selected != 0 || trace.unknownGotConn != 1 || trace.page1Origin != 0 || trace.unknownOrigin != 1 {
			t.Fatal("PAGE1_TRACE_STALE_ATTRIBUTION: an unknown callback inherited an earlier identity")
		}
		connection.local = &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 41002}
		trace.gotConn(connection)
		trace.gotConn(nil)
		trace.handler(owned)
		trace.handler(nil)
		if trace.selected != 0 || trace.unknownGotConn != 2 || trace.page1Origin != 0 || trace.unknownOrigin != 2 {
			t.Fatal("PAGE1_TRACE_STALE_ATTRIBUTION: a missing callback inherited an earlier identity")
		}
	})
}

// A partial compact result is not a per-packet mask. Call pairing remains
// known even at a legitimate zero-nanosecond event inside the synctest clock.
func TestSdkBrowsingPage1TimelineKeepsPartialAcceptanceUnknown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		trace := &sdkBrowsingPage1Timeline{}
		trace.arm(time.Now())
		packets := [][]byte{sdkBrowsingPage1TraceTestSyn(41003, 301), bridgeAdmissionTestPacket(false, 41004)}
		for _, accepted := range []int32{1, 0, 2} {
			tokens := trace.nativeRead(packets)
			trace.nativeOffer(tokens)
			trace.nativeReturned(tokens, accepted)
		}
		flow := &trace.flows[0]
		if flow.OfferCount != 3 || flow.Offers[0].AcceptanceKnown || !flow.Offers[1].AcceptanceKnown || !flow.Offers[2].AcceptanceKnown ||
			!flow.Offers[0].OfferKnown || !flow.Offers[0].ReturnKnown || flow.Offers[0].BatchPackets != 2 || flow.Offers[0].MatchingPackets != 1 {
			t.Fatal("PAGE1_TRACE_INVENTED_ACCEPTANCE: aggregate partial result became per-packet admission or lost its exact completed call")
		}
	})
}

// Overflow and the first-byte seal refuse new observations without changing
// existing data. A disabled observer remains nil-safe at every fixture seam.
func TestSdkBrowsingPage1TimelineBoundsAndStops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		trace := &sdkBrowsingPage1Timeline{}
		trace.arm(time.Now())
		for index := 0; index < 5; index++ {
			trace.nativeRead([][]byte{sdkBrowsingPage1TraceTestSyn(uint16(42000+index), uint32(1000+index))})
		}
		for range 17 {
			trace.nativeRead([][]byte{sdkBrowsingPage1TraceTestSyn(42000, 1000)})
		}
		trace.nativeRead([][]byte{sdkBrowsingPage1TraceTestSyn(42000, 1001)})
		for range 17 {
			trace.discoveryStart("quality", clientconnect.NewId())
		}
		for range 9 {
			trace.originStart(4, "203.0.113.11:443")
		}
		trace.firstByte()
		if trace.flowCount != 4 || trace.flowOverflow != 1 || trace.flows[0].OfferCount != 16 || trace.offerOverflow != 2 || trace.generationConflicts != 1 ||
			trace.discoveryCount != 16 || trace.discoveryOverflow != 1 || trace.originCount != 8 || trace.originOverflow != 1 || !trace.firstByteKnown {
			t.Fatal("PAGE1_TRACE_UNBOUNDED: configured cap or unknown counter changed")
		}
		if trace.nativeRead([][]byte{sdkBrowsingPage1TraceTestSyn(43000, 2000)}) != ([4]int{}) || trace.discoveryStart("quality", clientconnect.NewId()) != 0 || trace.originStart(4, "203.0.113.11:443") != 0 {
			t.Fatal("sealed first-byte timeline admitted new observations")
		}
		var disabled *sdkBrowsingPage1Timeline
		disabled.arm(time.Now())
		disabled.late(clientconnect.NewId())
		disabled.nativeOffer([4]int{})
		disabled.nativeReturned([4]int{}, 0)
		disabled.received(nil)
		disabled.gotConn(nil)
		disabled.firstByte()
		disabled.discoveryEnd(0, nil, nil)
		disabled.originEnd(0, nil, nil)
		disabled.handler(nil)
		disabled.log(t)
		if disabled.nativeRead(nil) != ([4]int{}) || disabled.discoveryStart("quality", clientconnect.NewId()) != 0 || disabled.originStart(4, "203.0.113.11:443") != 0 {
			t.Fatal("disabled diagnostic changed fixture seam behavior")
		}
	})
}
