package flowtrace

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"time"
)

const DnsQueryCapacity = 8

// Header metadata only: no question, answer, payload, or raw provider identity.
// DNS IDs are correlation hints, not authenticated generations. An ICMP quote
// contains no DNS ID, so it can never establish query-generation ownership.
type DnsMetadata struct {
	Id          uint16 `json:"id,omitempty"`
	IdKnown     bool   `json:"id_known,omitempty"`
	Response    bool   `json:"response,omitempty"`
	IcmpType    uint8  `json:"icmp_type,omitempty"`
	IcmpCode    uint8  `json:"icmp_code,omitempty"`
	PacketBytes int    `json:"packet_bytes"`
	Admission   string `json:"admission,omitempty"`
}

type DnsQuery struct {
	LocalPort uint16
	Resolver  netip.AddrPort
	Id        uint16
	Active    bool
}

type DnsEvidence struct {
	Query          DnsQuery
	Flow           Flow
	Admission      string
	Replies        []Event
	TupleOnlyIcmp  int
	OtherIdReplies int
	Unavailable    string
}

// Inspect before queue admission: after admission the packet's pooled owner
// may already have returned/reused its bytes. Completion retains only values.
func (r *Recorder) CaptureDnsEgress(packet []byte, now time.Time) Event {
	if r == nil || !r.dnsEnabled || !now.Before(r.until) {
		return Event{}
	}
	event, _ := dnsEvent(packet, false, now)
	return event
}

func (r *Recorder) CompleteDnsEgress(event Event, accepted bool) {
	admission := "rejected"
	if accepted {
		admission = "accepted"
	}
	r.completeDnsEgress(event, admission)
}

func (r *Recorder) completeDnsEgress(event Event, admission string) {
	if r == nil || event.Dns == nil {
		return
	}
	metadata := *event.Dns
	metadata.Admission = admission
	event.Dns = &metadata
	r.add(event)
}

// The temporary batch is capped by the recorder, not the caller's size.
// No parsing/allocation occurs while the recorder is absent or DNS disabled.
func (r *Recorder) CaptureDnsBatch(packets [][]byte, now time.Time) map[int]Event {
	if r == nil || !r.dnsEnabled || !now.Before(r.until) {
		return nil
	}
	var events map[int]Event
	for i, packet := range packets {
		if event := r.CaptureDnsEgress(packet, now); event.Dns != nil {
			if len(events) == DnsQueryCapacity {
				r.dropped.Add(1)
				continue
			}
			if events == nil {
				events = make(map[int]Event)
			}
			events[i] = event
		}
	}
	return events
}

func (r *Recorder) CompleteDnsBatch(events map[int]Event, acceptedCount, packetCount int) {
	// DeviceLocal reports an aggregate count, not an accepted prefix or
	// per-packet results. A partially accepted mixed-flow batch cannot prove
	// which DNS packet reached the route, even if just one DNS was captured.
	admission := "unproven-partial-batch"
	if packetCount > 0 && acceptedCount == packetCount {
		admission = "accepted"
	} else if packetCount > 0 && acceptedCount == 0 {
		admission = "rejected"
	}
	for _, event := range events {
		r.completeDnsEgress(event, admission)
	}
}

func dnsIpv4(packet []byte, quoted bool) (header, size int, ok bool) {
	if len(packet) < 20 || packet[0]>>4 != 4 {
		return
	}
	header, size = int(packet[0]&15)*4, int(binary.BigEndian.Uint16(packet[2:4]))
	ok = header >= 20 && header <= len(packet) && header <= size &&
		(quoted || size <= len(packet)) && binary.BigEndian.Uint16(packet[6:8])&0x3fff == 0
	return
}

// Structural metadata only; the forwarding stack owns checksum validation.
func dnsEvent(packet []byte, incoming bool, now time.Time) (Event, bool) {
	event := Event{At: now, Kind: "dns_egress"}
	header, size, ok := dnsIpv4(packet, false)
	if !ok {
		return Event{}, false
	}
	metadata := DnsMetadata{PacketBytes: size}
	quoted := false
	if packet[9] == 1 && incoming {
		if size < header+8 || (packet[header] != 3 && packet[header] != 11 && packet[header] != 12) {
			return Event{}, false
		}
		metadata.IcmpType, metadata.IcmpCode = packet[header], packet[header+1]
		packet = packet[header+8 : size]
		header, size, ok = dnsIpv4(packet, true)
		quoted = true
		if !ok {
			return Event{}, false
		}
	}
	if packet[9] != 17 || size < header+8 || len(packet) < header+8 {
		return Event{}, false
	}
	udp := packet[header:]
	udpSize := int(binary.BigEndian.Uint16(udp[4:6]))
	if udpSize < 8 || size < header+udpSize || (!quoted && len(udp) < udpSize) {
		return Event{}, false
	}
	source := netip.AddrPortFrom(netip.AddrFrom4([4]byte(packet[12:16])), binary.BigEndian.Uint16(udp[:2]))
	target := netip.AddrPortFrom(netip.AddrFrom4([4]byte(packet[16:20])), binary.BigEndian.Uint16(udp[2:4]))
	event.Flow = Flow{Local: source, Origin: target}
	if incoming && !quoted {
		event.Kind = "dns_return"
		event.Flow = Flow{Local: target, Origin: source}
	}
	if quoted {
		event.Kind = "dns_icmp"
	} else {
		if udpSize < 20 {
			return Event{}, false
		}
		metadata.Id, metadata.IdKnown = binary.BigEndian.Uint16(udp[8:10]), true
		metadata.Response = udp[10]&0x80 != 0
		if metadata.Response != incoming {
			return Event{}, false
		}
	}
	if event.Flow.Origin.Port() != 53 || event.Flow.Local.Port() == 0 {
		return Event{}, false
	}
	event.Dns = &metadata
	return event, true
}

// Join the client's bounded request-local queries to the exact post-NAT
// submission. Repeated offers or colliding tuples are ambiguous, not proof.
// Even a matching DNS ID is only correlation; quotes remain tuple-only.
func AttributeDns(snapshot Snapshot, queries []DnsQuery) ([]DnsEvidence, error) {
	if !snapshot.DnsEnabled || snapshot.Lost || !snapshot.Now.Before(snapshot.Until) {
		return nil, errors.New("DNS trace interval unsupported or incomplete")
	}
	if len(queries) == 0 || DnsQueryCapacity < len(queries) {
		return nil, errors.New("DNS query metadata unavailable or limited")
	}
	evidence := make([]DnsEvidence, 0, len(queries))
	for _, query := range queries {
		item := DnsEvidence{Query: query}
		offers := 0
		for _, event := range snapshot.Events {
			if event.Kind == "dns_egress" && event.Dns != nil && event.Dns.IdKnown && event.Dns.Id == query.Id &&
				event.Flow.Local.Port() == query.LocalPort && event.Flow.Origin == query.Resolver {
				offers++
				item.Flow, item.Admission = event.Flow, event.Dns.Admission
			}
		}
		if offers != 1 {
			item.Unavailable = "egress-not-observed-or-ambiguous"
		} else {
			for _, event := range snapshot.Events {
				if event.Flow != item.Flow || event.Dns == nil {
					continue
				}
				if event.Kind == "dns_icmp" {
					item.TupleOnlyIcmp++
				} else if event.Kind == "dns_return" && event.Dns.IdKnown {
					if event.Dns.Id != query.Id {
						item.OtherIdReplies++
						continue
					}
					if event.Provider == "" || event.Provider == "unavailable" {
						item.Unavailable = "provider-identity-unavailable"
						continue
					}
					item.Replies = append(item.Replies, event)
				}
			}
		}
		if item.Unavailable != "" {
			item.Replies = nil
		}
		evidence = append(evidence, item)
	}
	return evidence, nil
}
