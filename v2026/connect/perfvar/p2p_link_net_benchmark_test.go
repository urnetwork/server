// This file measures the userspace P2P outer-link wrapper's packet-rate and
// allocation headroom independently of ICE, DTLS, SCTP, and application work.
package perfvar

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/pion/transport/v4"
)

// A nonblocking UDP sink retains an observable byte count so the compiler
// cannot remove the delayed delegate write from the benchmark.
type benchmarkP2pUDPConn struct {
	transport.UDPConn
	writtenByteCount atomic.Uint64
}

// Every accepted payload is consumed without adding socket or channel cost.
func (self *benchmarkP2pUDPConn) Write(payload []byte) (int, error) {
	self.writtenByteCount.Add(uint64(len(payload)))
	return len(payload), nil
}

// A bounded userspace socket hands every accepted delegate write to one
// concurrent wrapper reader so credit acquire/release cost is measured intact.
type benchmarkP2pCreditUDPConn struct {
	transport.UDPConn
	packetByteCounts chan int
	writtenByteCount atomic.Uint64
}

// Delegate writes enqueue one datagram for the receiver wrapper.
func (self *benchmarkP2pCreditUDPConn) Write(payload []byte) (int, error) {
	self.packetByteCounts <- len(payload)
	self.writtenByteCount.Add(uint64(len(payload)))
	return len(payload), nil
}

// Delegate reads consume one exact datagram; the wrapper releases its credit.
func (self *benchmarkP2pCreditUDPConn) Read(payload []byte) (int, error) {
	packetByteCount := <-self.packetByteCounts
	if len(payload) < packetByteCount {
		return len(payload), nil
	}
	return packetByteCount, nil
}

// The benchmark reports the wrapper's outer packet rate, outer bit rate, and
// allocations while bounded batches prevent an artificial ingress overflow.
func BenchmarkP2pLinkUDPConnWrite(b *testing.B) {
	const payloadByteCount = 1200
	const batchPacketCount = 512
	profile := testP2pLinkProfile(1500, oversizeModeDrop)
	profile.RateBitsPerSecond = 1_000_000_000_000
	profile.BurstByteCount = 64 * 1024 * 1024
	profile.QueueByteCount = 64 * 1024 * 1024
	profile.QueuePacketCount = 2048
	link := newDirectionalLink(context.Background(), profile, 7301, nil)
	b.Cleanup(link.close)
	sink := &benchmarkP2pUDPConn{}
	connection := &p2pLinkUDPConn{UDPConn: sink, link: link}
	payload := make([]byte, payloadByteCount)

	b.ReportAllocs()
	b.SetBytes(payloadByteCount)
	b.ResetTimer()
	batchCount := 0
	for packetIndex := 0; packetIndex < b.N; packetIndex += 1 {
		writtenByteCount, err := connection.Write(payload)
		if err != nil || writtenByteCount != payloadByteCount {
			b.Fatalf("write bytes=%d err=%v", writtenByteCount, err)
		}
		batchCount += 1
		if batchCount == batchPacketCount {
			if !link.waitIdle(context.Background()) {
				b.Fatal("link did not become idle")
			}
			batchCount = 0
		}
	}
	if 0 < batchCount && !link.waitIdle(context.Background()) {
		b.Fatal("final link batch did not become idle")
	}
	b.StopTimer()

	duration := b.Elapsed()
	if 0 < duration {
		packetRate := float64(b.N) / duration.Seconds()
		outerByteCount := float64(b.N * (payloadByteCount + p2pIPv4UDPHeaderByteCount))
		b.ReportMetric(packetRate, "outer-packets/s")
		b.ReportMetric(outerByteCount*8/duration.Seconds()/1_000_000_000, "outer-Gbit/s")
	}
	wantWrittenByteCount := uint64(b.N * payloadByteCount)
	if sink.writtenByteCount.Load() != wantWrittenByteCount {
		b.Fatalf("delegate bytes=%d want=%d", sink.writtenByteCount.Load(), wantWrittenByteCount)
	}
	if snapshot := link.snapshot(); snapshot.QueueDropPacketCount != 0 ||
		snapshot.ReceiverDropPacketCount != 0 || snapshot.CanceledDropPacketCount != 0 {
		b.Fatalf("benchmark link disposition=%+v", snapshot)
	}
}

// The paired benchmark includes the hidden-drop prevention path and reports
// its sustained outer bit rate with a concurrently draining receive wrapper.
func BenchmarkP2pLinkUDPConnWriteWithReceiveCredits(b *testing.B) {
	const payloadByteCount = 1200
	const batchPacketCount = 512
	profile := testP2pLinkProfile(1500, oversizeModeDrop)
	profile.RateBitsPerSecond = 1_000_000_000_000
	profile.BurstByteCount = 64 * 1024 * 1024
	profile.QueueByteCount = 64 * 1024 * 1024
	profile.QueuePacketCount = 2048
	link := newDirectionalLink(context.Background(), profile, 7302, nil)
	credits := newP2pReceiveCredits(p2pVnetReceiveCreditPacketCount)
	b.Cleanup(func() {
		credits.close()
		link.close()
	})
	loopback := &benchmarkP2pCreditUDPConn{
		packetByteCounts: make(chan int, p2pVnetReceiveQueuePacketCount),
	}
	sender := &p2pLinkUDPConn{
		UDPConn:         loopback,
		link:            link,
		outboundCredits: credits,
	}
	receiver := &p2pLinkUDPConn{UDPConn: loopback, inboundCredits: credits}
	payload := make([]byte, payloadByteCount)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		readPayload := make([]byte, payloadByteCount)
		for range b.N {
			_, _ = receiver.Read(readPayload)
		}
	}()

	b.ReportAllocs()
	b.SetBytes(payloadByteCount)
	b.ResetTimer()
	batchCount := 0
	for packetIndex := 0; packetIndex < b.N; packetIndex += 1 {
		writtenByteCount, err := sender.Write(payload)
		if err != nil || writtenByteCount != payloadByteCount {
			b.Fatalf("write bytes=%d err=%v", writtenByteCount, err)
		}
		batchCount += 1
		if batchCount == batchPacketCount {
			if !waitForP2pTerminalIdle(
				context.Background(),
				[]*directionalLink{link},
				[]*p2pReceiveCredits{credits},
				nil,
			) {
				b.Fatal("link and receive credits did not become idle")
			}
			batchCount = 0
		}
	}
	if 0 < batchCount && !waitForP2pTerminalIdle(
		context.Background(),
		[]*directionalLink{link},
		[]*p2pReceiveCredits{credits},
		nil,
	) {
		b.Fatal("final link and receive-credit batch did not become idle")
	}
	<-readDone
	b.StopTimer()

	duration := b.Elapsed()
	if 0 < duration {
		packetRate := float64(b.N) / duration.Seconds()
		outerByteCount := float64(b.N * (payloadByteCount + p2pIPv4UDPHeaderByteCount))
		b.ReportMetric(packetRate, "outer-packets/s")
		b.ReportMetric(outerByteCount*8/duration.Seconds()/1_000_000_000, "outer-Gbit/s")
	}
	wantWrittenByteCount := uint64(b.N * payloadByteCount)
	if loopback.writtenByteCount.Load() != wantWrittenByteCount {
		b.Fatalf(
			"delegate bytes=%d want=%d",
			loopback.writtenByteCount.Load(),
			wantWrittenByteCount,
		)
	}
	linkSnapshot := link.snapshot()
	if linkSnapshot.QueueDropPacketCount != 0 || linkSnapshot.ReceiverDropPacketCount != 0 ||
		linkSnapshot.CanceledDropPacketCount != 0 {
		b.Fatalf("benchmark link disposition=%+v", linkSnapshot)
	}
	creditSnapshot := credits.snapshot()
	if !creditSnapshot.isExactLiveTerminal() ||
		creditSnapshot.ReadPacketCount != uint64(b.N) {
		b.Fatalf("benchmark receive-credit disposition=%+v", creditSnapshot)
	}
	b.ReportMetric(float64(creditSnapshot.MaximumOutstandingPackets), "max-outstanding-packets")
}
