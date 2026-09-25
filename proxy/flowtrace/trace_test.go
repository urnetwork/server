package flowtrace

import (
	"encoding/binary"
	"encoding/json"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func returnPacket(localPort uint16, payload string) []byte {
	packet := make([]byte, 40+len(payload))
	packet[0], packet[9] = 0x45, 6
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	copy(packet[12:16], net.ParseIP("142.251.218.195").To4())
	copy(packet[16:20], net.ParseIP("169.254.1.2").To4())
	binary.BigEndian.PutUint16(packet[20:22], 443)
	binary.BigEndian.PutUint16(packet[22:24], localPort)
	binary.BigEndian.PutUint32(packet[24:28], 100)
	packet[32], packet[33] = 0x50, 0x18
	copy(packet[40:], payload)
	return packet
}

func TestRecorderAttributesActualProviderToExactTLSFlow(t *testing.T) {
	now := time.Now()
	recorder, err := New(443, now)
	if err != nil {
		t.Fatal(err)
	}
	providerA, providerB := connect.Id{1}, connect.Id{2}
	flow := Flow{netip.MustParseAddrPort("169.254.1.2:25258"), netip.MustParseAddrPort("142.251.218.195:443")}
	recorder.add(Event{At: now, Kind: "dial", Protocol: "http", Flow: flow})
	packet := returnPacket(25258, "must-not-retain-tls-payload")
	recorder.Return(connect.SourceId(providerA), [][]byte{packet, returnPacket(25259, "other protocol")}, now)
	clear(packet)
	snapshot := recorder.Snapshot(0, 0, now)
	matched, returned, err := Attribute(snapshot, "http", netip.AddrPort{}, 0)
	if err != nil || matched != flow || len(returned) != 1 || returned[0].Provider != ProviderAlias(snapshot.Session, providerA) {
		t.Fatalf("exact return flow was lost: flow=%v returns=%+v err=%v", matched, returned, err)
	}
	// A later return for the same tuple from another source must remain visible;
	// never overwrite the provider with the currently preferred/window slot.
	recorder.Return(connect.SourceId(providerB), [][]byte{returnPacket(25258, "other provider")}, now)
	_, returned, err = Attribute(recorder.Snapshot(0, 0, now), "wireguard", flow.Origin, flow.Local.Port())
	if err != nil || len(returned) != 2 || returned[0].Provider == returned[1].Provider {
		t.Fatalf("provider change hidden: returns=%+v err=%v", returned, err)
	}
	encoded, _ := json.Marshal(recorder.Snapshot(0, 0, now))
	for _, forbidden := range []string{"must-not-retain-tls-payload", "other protocol", "other provider", providerA.String(), providerB.String()} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("trace retained private content %q", forbidden)
		}
	}
}

func TestRecorderDoesNotAttributeLostExpiredOrAmbiguousIntervals(t *testing.T) {
	now := time.Now()
	recorder, _ := New(443, now)
	flow := Flow{netip.MustParseAddrPort("169.254.1.2:25258"), netip.MustParseAddrPort("142.251.218.195:443")}
	for i := 0; i < Capacity+1; i++ {
		recorder.add(Event{At: now, Kind: "dial", Protocol: "http", Flow: flow})
	}
	snapshot := recorder.Snapshot(0, 0, now)
	if !snapshot.Lost || len(snapshot.Events) != Capacity {
		t.Fatalf("overflow not explicit: lost=%t events=%d", snapshot.Lost, len(snapshot.Events))
	}
	if _, _, err := Attribute(snapshot, "http", netip.AddrPort{}, 0); err == nil {
		t.Fatal("lost interval produced attribution")
	}
	before := recorder.Cursor(now).Cursor
	recorder.Return(connect.SourceId(connect.Id{1}), [][]byte{returnPacket(25258, "payload")}, now.Add(time.Hour))
	if recorder.Cursor(now).Cursor != before {
		t.Fatal("expired trace kept recording")
	}
	snapshot = recorder.Cursor(now.Add(time.Hour))
	if _, _, err := Attribute(snapshot, "http", netip.AddrPort{}, 0); err == nil {
		t.Fatal("expired interval produced attribution")
	}
	snapshot = Snapshot{Now: now, Until: now.Add(time.Second), Events: []Event{
		{Kind: "dial", Protocol: "http", Flow: flow},
		{Kind: "dial", Protocol: "http", Flow: Flow{netip.MustParseAddrPort("169.254.1.2:25259"), flow.Origin}},
	}}
	if _, _, err := Attribute(snapshot, "http", netip.AddrPort{}, 0); err == nil {
		t.Fatal("ambiguous protocol dial produced attribution")
	}
	if _, _, err := Attribute(snapshot, "wireguard", flow.Origin, 0); err == nil {
		t.Fatal("unknown WireGuard source port produced attribution")
	}
	snapshot.Events[1].Flow = flow
	if _, _, err := Attribute(snapshot, "http", netip.AddrPort{}, 0); err == nil {
		t.Fatal("repeated dial with reused tuple produced attribution")
	}
}

func TestRecorderNeverBlocksPacketInjectionAndReportsContention(t *testing.T) {
	now := time.Now()
	recorder, _ := New(443, now)
	recorder.mu.Lock()
	recorder.Return(connect.SourceId(connect.Id{1}), [][]byte{returnPacket(25258, "payload")}, now)
	recorder.mu.Unlock()
	snapshot := recorder.Snapshot(0, 0, now)
	if !snapshot.Lost || snapshot.Dropped != 1 || len(snapshot.Events) != 0 {
		t.Fatalf("contended sample was not explicitly lost: %+v", snapshot)
	}
	if _, _, err := Attribute(snapshot, "wireguard", netip.MustParseAddrPort("142.251.218.195:443"), 25258); err == nil {
		t.Fatal("contended interval produced attribution")
	}
	// Past loss must not poison a later complete cursor-delimited request.
	before := recorder.Cursor(now)
	recorder.Return(connect.SourceId(connect.Id{1}), [][]byte{returnPacket(25258, "later payload")}, now)
	if after := recorder.Snapshot(before.Cursor, before.Dropped, now); after.Lost || len(after.Events) != 1 {
		t.Fatalf("past contention poisoned later interval: %+v", after)
	}
	if invalid := recorder.Snapshot(^uint64(0), 0, now); !invalid.Lost || len(invalid.Events) != 0 {
		t.Fatal("invalid future cursor did not fail closed")
	}
}
