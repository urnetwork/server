package connect

import (
	"net"
	"testing"
)

// TestParsePpHeaderPacketV1 is the platform-independent proxy-protocol
// regression. The nginx end-to-end fixture uses Unix process groups and is
// build-tagged accordingly; header decoding must remain compiled and tested on
// Windows too.
func TestParsePpHeaderPacketV1(t *testing.T) {
	const header = "PROXY TCP4 192.0.2.10 198.51.100.20 12345 443\r\n"
	payload := []byte(header + "payload")

	headerByteCount, parsed, err := parsePpHeaderPacket(payload)
	if err != nil {
		t.Fatal(err)
	}
	if headerByteCount != len(header) {
		t.Fatalf("header byte count = %d, want %d", headerByteCount, len(header))
	}
	if parsed == nil {
		t.Fatal("proxy header was not detected")
	}
	source, ok := parsed.Source.(*net.TCPAddr)
	if !ok {
		t.Fatalf("source type = %T, want *net.TCPAddr", parsed.Source)
	}
	if source.IP.String() != "192.0.2.10" || source.Port != 12345 {
		t.Fatalf("source = %s, want 192.0.2.10:12345", source)
	}
}
