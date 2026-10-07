package acceptance

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/server/v2026/proxy/flowtrace"
)

const wireGuardDNSCap = 8
const wireGuardDNSTextCap = 2048

type wireGuardDNSContextKey struct{}

// Tuples stay in memory. A match means only an active socket in this request,
// not authenticated packet generation, resolver acceptance or provider identity.
type wireGuardDNSFlow struct {
	local, remote netip.AddrPort
	active, tcp   bool
	queryId       uint16
	queryKnown    bool
	queryRepeated bool
	historyIndex  int
}

type wireGuardDNSPacket struct {
	protocol, icmpType, icmpCode uint8
	bytes                        uint16
	quote                        uint8 // none, UDP header, short, malformed, fragmented, other
	match                        uint8 // unmatched, unique active tuple, ambiguous active tuple
	localEnvelope                bool
	closestActive                uint16 // bitset of equally-close field-mismatch masks
	closedCurrent                bool
	prior                        uint8 // exact prior-request tuple: active=1, closed=2
}

type wireGuardDNSRequest struct {
	stack            *wireGuardStack
	mu               sync.Mutex
	flows            [wireGuardDNSCap]wireGuardDNSFlow
	flowN            int
	packets          [wireGuardDNSCap]wireGuardDNSPacket
	packetN          int
	lookup           [6]uint16 // started, success, not-found, deadline, canceled, error
	io               [7]uint16 // reads, delivered reads, read bytes, writes, write bytes, read errors, write errors
	limited          bool
	frozen           bool
	freezeOnce       sync.Once
	text             string
	providerMetadata bool
	window           *wireGuardDNSReadiness
	windowRequest    uint64
	started          time.Time
}

type wireGuardDNSExchange struct {
	request *wireGuardDNSRequest
	index   int
}

func (s *wireGuardStack) startDNSRequest() *wireGuardDNSRequest {
	return s.startDNSRequestInWindow(nil)
}

func (s *wireGuardStack) startDNSRequestInWindow(window *wireGuardDNSReadiness) *wireGuardDNSRequest {
	r := &wireGuardDNSRequest{stack: s, window: window}
	if window != nil {
		r.windowRequest, r.started = window.begin()
	}
	s.statsLock.Lock()
	defer s.statsLock.Unlock()
	for i, active := range s.dnsRequests {
		if active == nil {
			s.dnsRequests[i] = r
			return r
		}
	}
	r.limited = true
	return r
}

func dnsAddrPort(address net.Addr) netip.AddrPort {
	var ip net.IP
	var port int
	switch address := address.(type) {
	case *net.UDPAddr:
		if address != nil {
			ip, port = address.IP, address.Port
		}
	case *net.TCPAddr:
		if address != nil {
			ip, port = address.IP, address.Port
		}
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok || port <= 0 || port > 65535 {
		return netip.AddrPort{}
	}
	return netip.AddrPortFrom(addr.Unmap(), uint16(port))
}

func (r *wireGuardDNSRequest) exchange(connection net.Conn, tcp bool) *wireGuardDNSExchange {
	if r == nil {
		return nil
	}
	flow := wireGuardDNSFlow{local: dnsAddrPort(connection.LocalAddr()), remote: dnsAddrPort(connection.RemoteAddr()), active: true, tcp: tcp, historyIndex: -1}
	r.stack.statsLock.Lock()
	defer r.stack.statsLock.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return nil
	}
	if r.flowN == len(r.flows) || !flow.local.IsValid() || !flow.remote.IsValid() {
		r.limited = true
		return nil
	}
	x := &wireGuardDNSExchange{r, r.flowN}
	if r.window != nil && !tcp {
		flow.historyIndex = r.window.register(r.windowRequest, flow)
	}
	r.flows[r.flowN] = flow
	r.flowN++
	return x
}

func (x *wireGuardDNSExchange) close() {
	if x == nil {
		return
	}
	r := x.request
	r.stack.statsLock.Lock()
	defer r.stack.statsLock.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.frozen {
		r.flows[x.index].active = false
	}
	if r.window != nil {
		// A request snapshot can freeze before its resolver Dial closes. Do
		// not call that socket retired until its actual close is observed.
		r.window.closeFlow(r.flows[x.index].historyIndex)
	}
}

func (r *wireGuardDNSRequest) add(counter *uint16, n int) {
	value := int(*counter) + min(max(n, 0), 65535)
	if value > 65535 || n > 65535 {
		r.limited = true
	}
	*counter = uint16(min(value, 65535))
}

func (x *wireGuardDNSExchange) ioEvent(n int, err error, write bool) {
	if x == nil {
		return
	}
	r := x.request
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return
	}
	if write {
		r.add(&r.io[3], 1)
		r.add(&r.io[4], n)
		if err != nil {
			r.add(&r.io[6], 1)
		}
	} else {
		r.add(&r.io[0], 1)
		r.add(&r.io[2], n)
		if n > 0 {
			r.add(&r.io[1], 1)
		}
		if err != nil {
			r.add(&r.io[5], 1)
		}
	}
}

// Only the explicit provider trace needs a DNS ID. Keep the ordinary request
// observer payload-free, and never retain the borrowed Write buffer.
func (x *wireGuardDNSExchange) wroteQuery(packet []byte, n int, err error) {
	if x == nil || !x.request.providerMetadata || err != nil || n != len(packet) || n < 12 || packet[2]&0x80 != 0 {
		return
	}
	r := x.request
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen || r.flows[x.index].tcp {
		return
	}
	flow := &r.flows[x.index]
	flow.queryRepeated = flow.queryKnown
	flow.queryId, flow.queryKnown = binary.BigEndian.Uint16(packet[:2]), true
}

// Called after freeze. An incomplete or repeated local query fails closed;
// inactive sockets remain explicitly inactive rather than claiming a late reply.
func (r *wireGuardDNSRequest) providerQueries() []flowtrace.DnsQuery {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.providerMetadata || !r.frozen || r.limited {
		return nil
	}
	var queries []flowtrace.DnsQuery
	for _, flow := range r.flows[:r.flowN] {
		if flow.tcp || !flow.queryKnown || flow.queryRepeated {
			return nil
		}
		queries = append(queries, flowtrace.DnsQuery{LocalPort: flow.local.Port(), Resolver: flow.remote, Id: flow.queryId, Active: flow.active})
	}
	return queries
}

func (r *wireGuardDNSRequest) lookupEvent(start bool, err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return
	}
	index := 5
	switch {
	case start:
		index = 0
	case err == nil:
		index = 1
	case errors.Is(err, context.DeadlineExceeded):
		index = 3
	case errors.Is(err, context.Canceled):
		index = 4
	default:
		if e, ok := err.(*net.DNSError); ok && e != nil {
			switch {
			case e.IsTimeout:
				index = 3
			case e.IsNotFound:
				index = 2
			}
		}
	}
	r.add(&r.lookup[index], 1)
}

func dnsIPv4(packet []byte, quote bool) (int, int, bool) {
	if len(packet) < 20 || packet[0]>>4 != 4 {
		return 0, 0, false
	}
	h, n := int(packet[0]&15)*4, int(binary.BigEndian.Uint16(packet[2:4]))
	return h, n, h >= 20 && len(packet) >= h && n >= h && (quote || len(packet) >= n)
}

// Structural inspection only, never parsing DNS payload or validating packet
// checksums. The netstack/resolver, not this observer, decides acceptance.
func dnsPacket(packet []byte) (event wireGuardDNSPacket, flow wireGuardDNSFlow) {
	event.bytes = uint16(min(len(packet), 65535))
	h, n, ok := dnsIPv4(packet, false)
	if !ok {
		event.quote = 3
		return
	}
	event.protocol = packet[9]
	if binary.BigEndian.Uint16(packet[6:8])&0x3fff != 0 {
		event.quote = 4
		return
	}
	payload := packet[h:n]
	if event.protocol == 1 {
		if len(payload) < 8 {
			event.quote = 2
			return
		}
		event.icmpType, event.icmpCode = payload[0], payload[1]
		if event.icmpType != 3 && event.icmpType != 11 && event.icmpType != 12 {
			return
		}
		packet = payload[8:]
		h, n, ok = dnsIPv4(packet, true)
		if !ok {
			event.quote = 3
			return
		}
		if binary.BigEndian.Uint16(packet[6:8])&0x3fff != 0 {
			event.quote = 4
			return
		}
		if packet[9] != 17 {
			event.quote = 5
			return
		}
	} else if event.protocol != 17 {
		return
	}
	if len(packet) < h+8 || n < h+8 {
		event.quote = 2
		return
	}
	length := int(binary.BigEndian.Uint16(packet[h+4 : h+6]))
	if length < 8 || n < h+length {
		event.quote = 3
		return
	}
	source := netip.AddrPortFrom(netip.AddrFrom4([4]byte(packet[12:16])), binary.BigEndian.Uint16(packet[h:h+2]))
	target := netip.AddrPortFrom(netip.AddrFrom4([4]byte(packet[16:20])), binary.BigEndian.Uint16(packet[h+2:h+4]))
	flow.local, flow.remote = target, source
	if event.protocol == 1 {
		event.quote = 1
		flow.local, flow.remote = source, target
	}
	return
}

// Field masks compare tuples, not packet ownership or generation. Several
// equally close sockets can yield different masks; retain those alternatives
// rather than selecting a convenient explanation for an unmatched quotation.
func dnsTupleDifference(a, b wireGuardDNSFlow) uint8 {
	var mask uint8
	if a.local.Addr() != b.local.Addr() {
		mask |= 1
	}
	if a.local.Port() != b.local.Port() {
		mask |= 2
	}
	if a.remote.Addr() != b.remote.Addr() {
		mask |= 4
	}
	if a.remote.Port() != b.remote.Port() {
		mask |= 8
	}
	return mask
}

func dnsTupleFields(mask uint8) string {
	if mask == 0 {
		return "none"
	}
	var fields []string
	for i, field := range []string{"source_address", "source_port", "remote_address", "remote_port"} {
		if mask&(1<<i) != 0 {
			fields = append(fields, field)
		}
	}
	return strings.Join(fields, "+")
}

func dnsTupleAlternatives(masks uint16) string {
	if masks == 0 {
		return "not_observed"
	}
	var alternatives []string
	for mask := range 16 {
		if masks&(1<<mask) != 0 {
			alternatives = append(alternatives, dnsTupleFields(uint8(mask)))
		}
	}
	return strings.Join(alternatives, "|")
}

// statsLock -> request.mu is the only nested lock order. Every inbound packet
// is window evidence; only the tuple comparison is specific to a DNS exchange.
func (s *wireGuardStack) observeDNSPacketLocked(packet []byte) {
	event, flow := dnsPacket(packet)
	local := len(packet) >= 20 && packet[0]>>4 == 4 && netip.AddrFrom4([4]byte(packet[16:20])) == s.clientIPv4
	var matches [wireGuardDNSCap]bool
	count := 0
	for i, r := range s.dnsRequests {
		if r == nil {
			continue
		}
		r.mu.Lock()
		for _, active := range r.flows[:r.flowN] {
			if local && active.active && !active.tcp && active.local == flow.local && active.remote == flow.remote {
				matches[i] = true
				count++
			}
		}
		r.mu.Unlock()
	}
	for i, r := range s.dnsRequests {
		if r == nil {
			continue
		}
		r.mu.Lock()
		if !r.frozen {
			copyEvent := event
			copyEvent.localEnvelope = local
			if flow.local.IsValid() && flow.remote.IsValid() {
				closestDistance := 5
				for _, candidate := range r.flows[:r.flowN] {
					if candidate.tcp {
						continue
					}
					mask := dnsTupleDifference(candidate, flow)
					if !candidate.active {
						copyEvent.closedCurrent = copyEvent.closedCurrent || mask == 0
						continue
					}
					distance := bits.OnesCount8(mask)
					if distance < closestDistance {
						closestDistance, copyEvent.closestActive = distance, 0
					}
					if distance == closestDistance {
						copyEvent.closestActive |= 1 << mask
					}
				}
				if r.window != nil {
					copyEvent.prior = r.window.priorTuple(r.windowRequest, flow)
				}
			}
			if matches[i] {
				copyEvent.match = 1
				if count > 1 {
					copyEvent.match = 2
				}
			}
			if r.packetN < len(r.packets) {
				r.packets[r.packetN] = copyEvent
				r.packetN++
			} else {
				r.limited = true
			}
		}
		r.mu.Unlock()
	}
}

func (r *wireGuardDNSRequest) freeze() string {
	r.freezeOnce.Do(func() {
		r.stack.statsLock.Lock()
		for i, active := range r.stack.dnsRequests {
			if active == r {
				r.stack.dnsRequests[i] = nil
			}
		}
		r.mu.Lock()
		r.frozen = true
		r.stack.statsLock.Unlock()
		defer r.mu.Unlock()
		var packets []string
		for _, p := range r.packets[:r.packetN] {
			shape := [...]string{"none", "udp_header", "short", "malformed", "fragmented", "other_protocol"}[p.quote]
			match := [...]string{"unmatched", "matched", "ambiguous"}[p.match]
			prior := [...]string{"not_observed", "active", "closed", "active_and_closed"}[p.prior]
			packets = append(packets, fmt.Sprintf("{proto=%d bytes=%d icmp=%d/%d quote_shape=%s active_dns_tuple=%s local_envelope=%t closest_active_fields=%s closed_current_tuple=%t prior_request_tuple=%s}", p.protocol, p.bytes, p.icmpType, p.icmpCode, shape, match, p.localEnvelope, dnsTupleAlternatives(p.closestActive), p.closedCurrent, prior))
		}
		for {
			r.text = fmt.Sprintf("WireGuard DNS metadata frozen=true packet_scope=request_window_not_ownership checksums=unchecked generation=unproven provider=unavailable exchanges=%d packet_observed_before_injection=[%s] socket_delivered_candidate{reads=%d with_data=%d bytes=%d read_errors=%d writes=%d written_bytes=%d write_errors=%d} lookup_authoritative{started=%d success=%d not_found=%d deadline=%d canceled=%d error=%d} limited=%t text_limit=%t omitted_packets=%d", r.flowN, strings.Join(packets, ","), r.io[0], r.io[1], r.io[2], r.io[5], r.io[3], r.io[4], r.io[6], r.lookup[0], r.lookup[1], r.lookup[2], r.lookup[3], r.lookup[4], r.lookup[5], r.limited, len(packets) != r.packetN, r.packetN-len(packets))
			if len(r.text) <= wireGuardDNSTextCap || len(packets) == 0 {
				break
			}
			i := len(packets) / 2
			packets = append(packets[:i], packets[i+1:]...)
		}
		if r.window != nil {
			r.window.record(r)
		}
	})
	return r.text
}

func wireGuardDNSSuffix(summary string) string {
	if summary == "" {
		return ""
	}
	return "; " + summary
}
