// These benchmarks compare the old singular and new batch WireGuard handoff
// at ProxyDevice's exact borrowed-to-owned boundary. Both variants perform the
// production message-pool copy before the asynchronous send completes.
package proxy

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// Measures one 128-packet userwireguard burst with identical packet ownership
// in both modes. The batch cases include their owned outer slice and activity
// update, so any gain is not borrowed from a weaker test fixture.
func BenchmarkProxyDeviceWireGuardBorrowedUpload(benchmark *testing.B) {
	const packetCount = 128
	const packetByteCount = 1280
	const offset = 16

	packets := make([][]byte, packetCount)
	for packetIndex := range packets {
		packets[packetIndex] = make([]byte, offset+packetByteCount)
	}

	benchmark.Run("singular", func(benchmark *testing.B) {
		proxyDevice := &ProxyDevice{
			ctx: context.Background(),
			sendOwnedPacketForTest: func(packet []byte) bool {
				connect.MessagePoolReturn(packet)
				return true
			},
		}
		benchmark.SetBytes(packetCount * packetByteCount)
		benchmark.ReportAllocs()
		benchmark.ResetTimer()
		for range benchmark.N {
			for _, packet := range packets {
				if !proxyDevice.Send(packet[offset:]) {
					benchmark.Fatal("singular packet was not admitted")
				}
			}
		}
	})

	for _, batchSize := range []int{8, 64, 128} {
		benchmark.Run(fmt.Sprintf("batch-%d", batchSize), func(benchmark *testing.B) {
			proxyDevice := &ProxyDevice{
				ctx: context.Background(),
				sendOwnedPacketsForTest: func(ownedPackets [][]byte) int {
					for _, ownedPacket := range ownedPackets {
						connect.MessagePoolReturn(ownedPacket)
					}
					return len(ownedPackets)
				},
			}
			benchmark.SetBytes(packetCount * packetByteCount)
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			for range benchmark.N {
				sentPacketCount := 0
				for packetStart := 0; packetStart < len(packets); packetStart += batchSize {
					packetEnd := min(packetStart+batchSize, len(packets))
					sentPacketCount += proxyDevice.SendBorrowedBatch(
						packets[packetStart:packetEnd],
						offset,
					)
				}
				if sentPacketCount != packetCount {
					benchmark.Fatalf(
						"batch admitted %d packets, want %d",
						sentPacketCount,
						packetCount,
					)
				}
			}
		})
	}
}
