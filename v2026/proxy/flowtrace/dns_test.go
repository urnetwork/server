package flowtrace

import (
	"encoding/binary"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

func dnsTestPacket(port, id uint16, response bool) []byte {
	packet := make([]byte, 40+len("never-retain-question-or-answer"))
	packet[0], packet[8], packet[9] = 0x45, 64, 17
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	local, resolver := [4]byte{192, 0, 2, 2}, [4]byte{198, 51, 100, 53}
	copy(packet[12:16], local[:])
	copy(packet[16:20], resolver[:])
	binary.BigEndian.PutUint16(packet[20:22], port)
	binary.BigEndian.PutUint16(packet[22:24], 53)
	if response {
		copy(packet[12:16], resolver[:])
		copy(packet[16:20], local[:])
		binary.BigEndian.PutUint16(packet[20:22], 53)
		binary.BigEndian.PutUint16(packet[22:24], port)
		packet[30] = 0x80
	}
	binary.BigEndian.PutUint16(packet[24:26], uint16(len(packet)-20))
	binary.BigEndian.PutUint16(packet[28:30], id)
	copy(packet[40:], "never-retain-question-or-answer")
	return packet
}

func dnsTestQuote(packet []byte) []byte {
	quote := make([]byte, 56)
	quote[0], quote[8], quote[9], quote[20], quote[21] = 0x45, 64, 1, 3, 3
	binary.BigEndian.PutUint16(quote[2:4], uint16(len(quote)))
	copy(quote[12:16], packet[16:20])
	copy(quote[16:20], packet[12:16])
	copy(quote[28:], packet[:28])
	return quote
}

func dnsTestQuery(port, id uint16) DnsQuery {
	return DnsQuery{LocalPort: port, Resolver: netip.MustParseAddrPort("198.51.100.53:53"), Id: id}
}

func TestRecorderDnsExactCorrelationRejectsStaleAndForeignReturns(t *testing.T) {
	now := time.Now()
	r, _ := NewWithDns(443, true, now)
	query := dnsTestPacket(45001, 7, false)
	offer := r.CaptureDnsEgress(query, now)
	clear(query) // no borrowed packet survives queue admission
	r.CompleteDnsEgress(offer, true)
	provider := connect.Id{42}
	r.Return(connect.SourceId(provider), [][]byte{
		dnsTestPacket(45002, 7, true),                // another socket
		dnsTestPacket(45001, 8, true),                // same tuple, stale/different DNS ID
		dnsTestQuote(dnsTestPacket(45001, 7, false)), // tuple-only, no generation
		dnsTestQuote(dnsTestPacket(45002, 7, false)), // unrelated quote
		dnsTestPacket(45001, 7, true),
	}, now)
	snapshot := r.Snapshot(0, 0, now)
	evidence, err := AttributeDns(snapshot, []DnsQuery{dnsTestQuery(45001, 7)})
	if err != nil || len(evidence) != 1 {
		t.Fatalf("DNS evidence = %+v, %v", evidence, err)
	}
	item := evidence[0]
	if item.Unavailable != "" || item.Admission != "accepted" || len(item.Replies) != 1 || item.OtherIdReplies != 1 || item.TupleOnlyIcmp != 1 {
		t.Fatalf("stale/foreign evidence mixed with exact query: %+v", item)
	}
	if item.Query.Active || item.Replies[0].Provider != ProviderAlias(snapshot.Session, provider) {
		t.Fatal("inactive query was resurrected or actual return provider was lost")
	}
	encoded, _ := json.Marshal(snapshot)
	if strings.Contains(string(encoded), "never-retain") || strings.Contains(string(encoded), provider.String()) {
		t.Fatal("DNS trace retained payload or raw identity")
	}
}

func TestRecorderDnsFailsClosedForMissingIdentityAndAmbiguousOffers(t *testing.T) {
	for _, kind := range []string{"missing-provider", "ambiguous-offer", "missing-offer"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Now()
			r, _ := NewWithDns(443, true, now)
			if kind != "missing-offer" {
				r.CompleteDnsEgress(r.CaptureDnsEgress(dnsTestPacket(45001, 0, false), now), true)
			}
			if kind == "ambiguous-offer" {
				r.CompleteDnsEgress(r.CaptureDnsEgress(dnsTestPacket(45001, 0, false), now), true)
			}
			provider := connect.Id{42}
			if kind == "missing-provider" {
				provider = connect.Id{}
			}
			r.Return(connect.SourceId(provider), [][]byte{dnsTestPacket(45001, 0, true)}, now)
			evidence, err := AttributeDns(r.Snapshot(0, 0, now), []DnsQuery{dnsTestQuery(45001, 0)})
			if err != nil || len(evidence) != 1 || evidence[0].Unavailable == "" || len(evidence[0].Replies) != 0 {
				t.Fatalf("uncertain metadata claimed ownership: %+v, %v", evidence, err)
			}
		})
	}
}

func TestRecorderDnsBoundedLossExpiryAndCapability(t *testing.T) {
	now := time.Now()
	for _, kind := range []string{"overflow", "contention", "expired", "unsupported", "batch-cap"} {
		t.Run(kind, func(t *testing.T) {
			r, _ := NewWithDns(443, kind != "unsupported", now)
			query := dnsTestPacket(45001, 7, false)
			r.CompleteDnsEgress(r.CaptureDnsEgress(query, now), true)
			readAt := now
			switch kind {
			case "overflow":
				for range Capacity {
					r.CompleteDnsEgress(r.CaptureDnsEgress(query, now), true)
				}
			case "contention":
				r.mu.Lock()
				r.Return(connect.SourceId(connect.Id{1}), [][]byte{dnsTestPacket(45001, 7, true)}, now)
				r.mu.Unlock()
			case "expired":
				readAt = now.Add(time.Hour)
			case "batch-cap":
				packets := make([][]byte, DnsQueryCapacity+1)
				for i := range packets {
					packets[i] = query
				}
				captured := r.CaptureDnsBatch(packets, now)
				if len(captured) != DnsQueryCapacity {
					t.Fatal("temporary DNS batch was not bounded")
				}
				r.CompleteDnsBatch(captured, len(packets), len(packets))
			}
			if _, err := AttributeDns(r.Snapshot(0, 0, readAt), []DnsQuery{dnsTestQuery(45001, 7)}); err == nil {
				t.Fatal("incomplete interval produced DNS attribution")
			}
		})
	}
}

func TestRecorderDnsDisabledHasNoAllocationOrMutation(t *testing.T) {
	now := time.Now()
	r, _ := New(443, now)
	packets := [][]byte{dnsTestPacket(45001, 7, false)}
	for _, recorder := range []*Recorder{nil, r} {
		if allocations := testing.AllocsPerRun(100, func() {
			event := recorder.CaptureDnsEgress(packets[0], now)
			recorder.CompleteDnsEgress(event, true)
			recorder.CompleteDnsBatch(recorder.CaptureDnsBatch(packets, now), 1, 1)
		}); allocations != 0 {
			t.Fatalf("disabled DNS trace allocations = %v", allocations)
		}
	}
	if len(r.Snapshot(0, 0, now).Events) != 0 {
		t.Fatal("DNS recorded without explicit opt-in")
	}
}

func TestRecorderDnsBatchPartialAdmissionIsNotAPrefix(t *testing.T) {
	now := time.Now()
	r, _ := NewWithDns(443, true, now)
	packets := [][]byte{dnsTestPacket(45001, 7, false), dnsTestPacket(45002, 8, false)}
	r.CompleteDnsBatch(r.CaptureDnsBatch(packets, now), 1, len(packets))
	for _, event := range r.Snapshot(0, 0, now).Events {
		if event.Dns.Admission != "unproven-partial-batch" {
			t.Fatalf("aggregate batch count claimed exact packet admission: %+v", event.Dns)
		}
	}
}

func TestRecorderDnsBatchAdmissionAllNoneAndUnknown(t *testing.T) {
	for _, test := range []struct {
		accepted, total int
		want            string
	}{
		{2, 2, "accepted"}, {0, 2, "rejected"}, {1, 2, "unproven-partial-batch"},
		{-1, 2, "unproven-partial-batch"}, {3, 2, "unproven-partial-batch"},
	} {
		now := time.Now()
		r, _ := NewWithDns(443, true, now)
		// One captured DNS packet does not make the aggregate count exact:
		// the other, non-DNS packet could be the sole accepted packet.
		r.CompleteDnsBatch(r.CaptureDnsBatch([][]byte{dnsTestPacket(45001, 7, false)}, now), test.accepted, test.total)
		events := r.Snapshot(0, 0, now).Events
		if len(events) != 1 || events[0].Dns.Admission != test.want {
			t.Fatalf("aggregate %d/%d claimed admission %+v, want %s", test.accepted, test.total, events, test.want)
		}
	}
}

func TestRecorderDnsMalformedAndFragmentedPacketsDoNotClaimQueries(t *testing.T) {
	now := time.Now()
	for _, kind := range []string{"short-ip", "fragment", "short-dns", "short-quote", "wrong-port"} {
		packet := dnsTestPacket(45001, 7, false)
		switch kind {
		case "short-ip":
			packet = packet[:10]
		case "fragment":
			packet[6] = 0x20
		case "short-dns":
			binary.BigEndian.PutUint16(packet[24:26], 10)
		case "short-quote":
			packet = dnsTestQuote(packet)[:40]
		case "wrong-port":
			binary.BigEndian.PutUint16(packet[22:24], 54)
		}
		if _, ok := dnsEvent(packet, kind == "short-quote", now); ok {
			t.Errorf("%s claimed DNS metadata", kind)
		}
	}
}
