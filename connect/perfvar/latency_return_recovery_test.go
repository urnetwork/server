// This file forces inner TCP tail loss after successful Transfer delivery
// while the same production route continues carrying its UDP probe workload.
package perfvar

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
)

// A bounded receiver-side loss burst must recover inside the unchanged bulk
// deadline. A timer per missing segment previously stranded this exact tail.
func TestFullTunLatencyUnderLoadDownloadRecoversTailBurst(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		profile := initialNetworkProfiles(4017)["clean-lan"]
		fixture, err := newPerfvarCorrectnessFixture(
			t, fullTunRouteExchangeH1, profile, profile, profile,
			defaultTunResourceProfile(), 5*time.Minute,
		)
		if err != nil {
			t.Fatal(err)
		}
		defer fixture.close()
		path := fixture.path
		const bulkByteCount = int64(2 * 1024 * 1024)
		const tailByteCount = uint32(128 * 1024)
		var stateLock sync.Mutex
		dataStartPortSequences := map[uint16]uint32{}
		droppedSequenceBools := map[uint32]bool{}
		// The fixture has already joined readiness. The next TCP handshake
		// belongs to the measured download; UDP probes always pass unchanged.
		admit := func(packet []byte) bool {
			if len(packet) < 40 || packet[0]>>4 != 4 || packet[9] != 6 {
				return true
			}
			tcp := packet[int(packet[0]&15)*4:]
			if len(tcp) < 20 {
				return true
			}
			stateLock.Lock()
			defer stateLock.Unlock()
			port := binary.BigEndian.Uint16(tcp[2:4])
			sequence := binary.BigEndian.Uint32(tcp[4:8])
			if tcp[13]&2 != 0 {
				dataStartPortSequences[port] = sequence + 1
				return true
			}
			base, exists := dataStartPortSequences[port]
			if !exists || len(tcp) == int(tcp[12]>>4)*4 ||
				int32(sequence-base) < int32(uint32(bulkByteCount)-tailByteCount) {
				return true
			}
			// Only the original copy of each tail segment is lost. This is a
			// single finite NIC loss event; every subsequent replay is kept.
			if droppedSequenceBools[sequence] {
				return true
			}
			droppedSequenceBools[sequence] = true
			return false
		}
		path.multiClient.SetReceivePacketCallback(func(_ clientconnect.TransferPath, _ protocol.ProvideMode, _ *clientconnect.IpPath, packet []byte) {
			if admit(packet) {
				_, _ = path.appTun.Write(packet)
			}
		})
		path.multiClient.SetReceivePacketsCallback(func(_ clientconnect.TransferPath, _ protocol.ProvideMode, _ *clientconnect.IpPath, packets [][]byte) {
			admittedPackets := make([][]byte, 0, len(packets))
			for _, packet := range packets {
				if admit(packet) {
					admittedPackets = append(admittedPackets, packet)
				}
			}
			_, _ = path.appTun.WriteBatch(admittedPackets)
		})
		observation, err := fixture.measure(
			perfvarWorkloadLatencyUnderLoad,
			perfvarDirectionDownload,
			func(ctx context.Context, path *fullTunPath) (workloadResult, error) {
				return measureFullTunLatencyUnderLoadDirection(ctx, path, bulkByteCount, false)
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		stateLock.Lock()
		droppedPacketCount := len(droppedSequenceBools)
		stateLock.Unlock()
		if droppedPacketCount < 32 {
			t.Fatalf("tail loss dropped %d packets, want at least 32", droppedPacketCount)
		}
		result := observation.Result
		if result.UsefulByteCount != bulkByteCount ||
			result.ContentHash != deterministicPayloadHash(bulkByteCount) ||
			result.IdleLatency.P50 <= 0 || result.PostLoadLatency.P50 <= 0 ||
			result.LoadedProbeSuccessCount < minimumLatencyProbeSuccessCount {
			t.Fatalf("tail recovery latency-under-load result=%+v", result)
		}
		t.Logf("recovered %d lost tail segments in %s with %d successful loaded probes", droppedPacketCount, result.Duration, result.LoadedProbeSuccessCount)
	})
}
