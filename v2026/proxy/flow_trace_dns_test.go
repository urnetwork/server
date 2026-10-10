package proxy

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/proxy/flowtrace"
)

// An explicitly armed recorder must span the owned WireGuard submission and
// authenticated provider return, including DNS before an HTTPS origin exists.
func TestFlowTraceDNSOptInCapturesPacketBoundaries(t *testing.T) {
	device := natTestDevice(t, true, netip.MustParseAddr("192.0.2.2"))
	device.ctx = context.Background()
	handler := flowTraceHandler{
		auth: func(string) (server.Id, error) { return server.Id{1}, nil },
		open: func(server.Id) (*ProxyDevice, error) { return device, nil },
	}
	request := httptest.NewRequest(http.MethodPost, flowTracePath, strings.NewReader(`{"port":443,"include_dns":true}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("arm DNS trace: %d", response.Code)
	}
	var captured []byte
	device.sendOwnedPacketForTest = func(packet []byte) bool {
		captured = append([]byte(nil), packet...)
		connect.MessagePoolReturn(packet)
		return true
	}
	if !device.Send(dnsTraceTestPacket("192.0.2.2", "198.51.100.53", 45001, 53, false)) {
		t.Fatal("DNS submission failed")
	}
	answer := wgNatUDPReply(captured, []byte{0, 7, 0x80, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	device.flowTrace.Load().Return(connect.SourceId(connect.Id{42}), [][]byte{answer}, time.Now())
	snapshot := device.flowTrace.Load().Snapshot(0, 0, time.Now())
	if len(snapshot.Events) != 2 || snapshot.Events[0].Kind != "dns_egress" || snapshot.Events[1].Kind != "dns_return" {
		t.Fatalf("DNS submission/return metadata = %+v, want both boundaries", snapshot.Events)
	}
	var capability map[string]any
	if json.Unmarshal(response.Body.Bytes(), &capability) != nil || capability["dns_enabled"] != true {
		t.Fatal("server did not positively advertise DNS trace capability")
	}
}

func TestFlowTraceDNSBatchCapturesBeforeOwnershipHandoff(t *testing.T) {
	device := natTestDevice(t, true, netip.MustParseAddr("192.0.2.2"))
	device.ctx = context.Background()
	r, _ := flowtrace.NewWithDns(443, true, time.Now())
	device.flowTrace.Store(r)
	device.sendOwnedPacketsForTest = func(packets [][]byte) int {
		clear(packets[0])
		connect.MessagePoolReturn(packets[0])
		return 1
	}
	packets := [][]byte{
		dnsTraceTestPacket("192.0.2.2", "198.51.100.53", 45001, 53, false),
		dnsTraceTestPacket("192.0.2.2", "198.51.100.53", 45002, 53, false),
	}
	if device.SendBorrowedBatch(packets, 0) != 1 {
		t.Fatal("partial admission changed")
	}
	snapshot := r.Snapshot(0, 0, time.Now())
	if len(snapshot.Events) != 2 {
		t.Fatalf("DNS events=%d", len(snapshot.Events))
	}
	for _, event := range snapshot.Events {
		if event.Dns == nil || !event.Dns.IdKnown || event.Dns.Id != 7 || event.Dns.Admission != "unproven-partial-batch" || event.Flow.Local.Addr() != device.natAddr {
			t.Fatalf("borrowed bytes or aggregate admission corrupted metadata: %+v", event)
		}
	}
}

func dnsTraceTestPacket(source, destination string, sourcePort, destinationPort uint16, response bool) []byte {
	packet := make([]byte, 40)
	packet[0], packet[8], packet[9] = 0x45, 64, 17
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	src, dst := netip.MustParseAddr(source).As4(), netip.MustParseAddr(destination).As4()
	copy(packet[12:16], src[:])
	copy(packet[16:20], dst[:])
	binary.BigEndian.PutUint16(packet[20:22], sourcePort)
	binary.BigEndian.PutUint16(packet[22:24], destinationPort)
	binary.BigEndian.PutUint16(packet[24:26], 20)
	binary.BigEndian.PutUint16(packet[28:30], 7)
	if response {
		packet[30] = 0x80
	}
	binary.BigEndian.PutUint16(packet[10:12], natTestChecksum(packet[:20]))
	return packet
}
