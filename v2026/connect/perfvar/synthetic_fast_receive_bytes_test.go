package perfvar

import (
	"bytes"
	"encoding/binary"
	"testing"

	clientconnect "github.com/urnetwork/connect/v2026"
)

// The real public TUN returns injected bytes for both its single-packet and
// multi-packet paths. No SDK, TestEnv, socket or provider is needed here.
func TestSyntheticFastReceiveTunCountsBytes(t *testing.T) {
	for _, packetCount := range []int{1, 3} {
		settings := clientconnect.DefaultTunSettings()
		settings.Log = clientconnect.NewNoopLogger()
		tun, err := clientconnect.CreateTun(t.Context(), settings)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := tun.Close(); err != nil {
				t.Error(err)
			}
		})
		fixture := &syntheticFastNativeFixture{appTun: tun}
		var destination [4]byte
		foundIpv4 := false
		for _, address := range tun.LocalAddresses() {
			if address.Is4() {
				destination, foundIpv4 = address.As4(), true
				break
			}
		}
		if !foundIpv4 {
			t.Fatal("public TUN has no owned IPv4 destination")
		}
		packets := make([][]byte, packetCount)
		originals := make([][]byte, packetCount)
		wantBytes := uint64(0)
		for index := range packets {
			// IPv4 UDP permits a zero UDP checksum. A complete IPv4 checksum
			// and the actual TUN destination keep these ordinary valid inputs.
			packet := make([]byte, 32+index*8)
			packet[0], packet[8], packet[9] = 0x45, 64, 17
			binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
			copy(packet[12:16], []byte{192, 0, 2, 1})
			copy(packet[16:20], destination[:])
			binary.BigEndian.PutUint16(packet[20:22], uint16(41000+index))
			binary.BigEndian.PutUint16(packet[22:24], 42000)
			binary.BigEndian.PutUint16(packet[24:26], uint16(len(packet)-20))
			checksum := uint32(0)
			for offset := 0; offset < 20; offset += 2 {
				checksum += uint32(binary.BigEndian.Uint16(packet[offset : offset+2]))
			}
			for checksum>>16 != 0 {
				checksum = (checksum & 0xffff) + (checksum >> 16)
			}
			binary.BigEndian.PutUint16(packet[10:12], ^uint16(checksum))
			if _, err := clientconnect.ParseIpPath(packet); err != nil {
				t.Fatal("invalid receive fixture packet", err)
			}
			packets[index], originals[index] = packet, bytes.Clone(packet)
			wantBytes += uint64(len(packet))
		}
		fixture.receiveApplicationPackets(packets)
		if fixture.receiveInvalid.Load() || fixture.receiveOfferedPackets.Load() != uint64(packetCount) || fixture.receiveOfferedBytes.Load() != wantBytes ||
			fixture.receiveAcceptedBytes.Load() != wantBytes || fixture.receiveError.Load() != nil {
			t.Errorf("FAST_TUN_RECEIVE_BYTE_UNIT_MISMATCH: packets=%d offered_packets=%d offered_bytes=%d accepted_bytes=%d want_bytes=%d invalid=%t receive_error=%t",
				packetCount, fixture.receiveOfferedPackets.Load(), fixture.receiveOfferedBytes.Load(), fixture.receiveAcceptedBytes.Load(), wantBytes,
				fixture.receiveInvalid.Load(), fixture.receiveError.Load() != nil)
		}
		for index := range packets {
			if !bytes.Equal(packets[index], originals[index]) {
				t.Error("TUN receive callback changed a borrowed packet", index)
			}
		}
		if fixture.bridgeInvalid.Load() || fixture.bridgeStarted.Load() != 0 || fixture.bridgeCompleted.Load() != 0 ||
			fixture.bridgeAccepted.Load() != 0 || fixture.bridgeRefused.Load() != 0 {
			t.Error("TUN byte accounting changed compact-input packet accounting")
		}
		if err := tun.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
