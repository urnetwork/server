package perfvar

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
)

// Real parseable packets keep the test on the native batch metadata path.
func bridgeAdmissionTestPacket(tcp bool, sourcePort uint16) []byte {
	packet := make([]byte, 32)
	packet[0], packet[8], packet[9] = 0x45, 64, 17
	if tcp {
		packet = make([]byte, 40)
		packet[0], packet[8], packet[9], packet[32], packet[33] = 0x45, 64, 6, 0x50, 0x10
	} else {
		binary.BigEndian.PutUint16(packet[24:26], uint16(len(packet)-20))
	}
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	copy(packet[12:16], []byte{192, 0, 2, 1})
	copy(packet[16:20], []byte{198, 51, 100, 1})
	binary.BigEndian.PutUint16(packet[20:22], sourcePort)
	binary.BigEndian.PutUint16(packet[22:24], 42000)
	return packet
}

// The sender knows which flow it admitted. An aggregate partial count cannot
// turn the accepted UDP packet into a rejection just because TCP was refused.
func TestFullTunBridgeBatchMixedFlowAdmission(t *testing.T) {
	for _, reversed := range []bool{false, true} {
		name := "tcp_then_udp"
		if reversed {
			name = "udp_then_tcp"
		}
		t.Run(name, func(t *testing.T) {
			tracker := newFullTunBridgeSendTracker()
			udp := bridgeAdmissionTestPacket(false, 41000)
			udpBytes := clientconnect.ByteCount(len(udp))
			ipPath, err := clientconnect.ParseIpPath(udp)
			if err != nil {
				t.Fatal(err)
			}
			window, ok := tracker.beginFlowWindow(fullTunBridgeFlowKeyFromIpPath(ipPath))
			if !ok {
				t.Fatal("begin UDP window")
			}
			packets := [][]byte{bridgeAdmissionTestPacket(true, 41001), udp}
			if reversed {
				packets[0], packets[1] = packets[1], packets[0]
			}
			delayCalls, sendCalls, admittedUDP := 0, 0, 0
			accepted := sendFullTunBridgeBatch(tracker, packets, time.Microsecond,
				func(time.Duration) { delayCalls++ },
				func(batch [][]byte, accepted []bool) int {
					sendCalls++
					for i, packet := range batch {
						path, err := clientconnect.ParseIpPath(packet)
						if err != nil {
							t.Fatal(err)
						}
						if path.Protocol == clientconnect.IpProtocolUdp {
							admittedUDP++
							accepted[i] = true
						}
						// A consuming sender may immediately recycle packet bytes.
						clear(packet)
					}
					return admittedUDP
				})
			if accepted != 1 || admittedUDP != 1 || delayCalls != 1 || sendCalls != 1 {
				t.Fatalf("admission/delay/send=%d/%d/%d/%d", accepted, admittedUDP, delayCalls, sendCalls)
			}
			boundary, ok := tracker.flowBoundary(t.Context(), window, 1, udpBytes)
			if !ok || !tracker.waitThrough(t.Context(), boundary) {
				t.Fatalf("accepted UDP contaminated by incidental TCP rejection; failures=%d", tracker.failureCount.Load())
			}
			if tracker.failureCount.Load() != 1 || !tracker.finishFlowWindow(window) {
				t.Fatal("exact rejected packet count or successful UDP finish was lost")
			}
			diagnostic := tracker.flowDiagnostic(window)
			if len(diagnostic.RecentFailures) != 1 || diagnostic.RecentFailures[0].ReturnedCount != 1 {
				t.Fatalf("mixed batch evidence missing: %+v", diagnostic)
			}
			for _, packet := range diagnostic.RecentFailures[0].Packets {
				if !packet.Flow.valid || packet.Accepted != (packet.Flow.protocol == clientconnect.IpProtocolUdp) {
					t.Fatalf("metadata lost or inferred after sender recycled bytes: %+v", packet)
				}
			}
		})
	}
}

func TestFullTunBridgeBatchRejectsMeasuredUdp(t *testing.T) {
	tracker := newFullTunBridgeSendTracker()
	udp := bridgeAdmissionTestPacket(false, 41000)
	path, _ := clientconnect.ParseIpPath(udp)
	window, _ := tracker.beginFlowWindow(fullTunBridgeFlowKeyFromIpPath(path))
	packets := [][]byte{udp, bridgeAdmissionTestPacket(true, 41001)}
	sendFullTunBridgeBatch(tracker, packets, 0, func(time.Duration) {}, func(_ [][]byte, accepted []bool) int {
		accepted[1] = true
		return 1
	})
	boundary, ok := tracker.flowBoundary(t.Context(), window, 1, clientconnect.ByteCount(len(udp)))
	if !ok || tracker.waitThrough(t.Context(), boundary) || tracker.finishFlowWindow(window) {
		t.Fatal("actual measured UDP refusal was accepted")
	}
	diagnostic := tracker.flowDiagnostic(window)
	if diagnostic.ObservedPackets != 1 || diagnostic.RejectedPackets != 1 || diagnostic.AcceptedPackets != 0 || diagnostic.PendingPackets != 0 {
		t.Fatalf("flow admission evidence=%+v", diagnostic)
	}
	err := tracker.flowFailure(t.Context(), window, 1, 32, true, 0, 0, 0, 7)
	for _, want := range []string{"terminal admission rejected", "target_packets=1", "RejectedPackets:1", "receiver_delivered=0", "tcp_collapse_drops_global=7"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("failure lacks %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "%!") {
		t.Fatalf("nil context was formatted as a wrapped cause: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if !errors.Is(tracker.flowFailure(ctx, window, 1, 32, true, 0, 0, 0, 0), context.Canceled) {
		t.Fatal("real cancellation cause was not preserved")
	}
}

func TestFullTunBridgeBatchRejectsInconsistentResults(t *testing.T) {
	for _, returned := range []int{-1, 0, 2} {
		tracker := newFullTunBridgeSendTracker()
		udp := bridgeAdmissionTestPacket(false, 41000)
		path, _ := clientconnect.ParseIpPath(udp)
		window, _ := tracker.beginFlowWindow(fullTunBridgeFlowKeyFromIpPath(path))
		sendFullTunBridgeBatch(tracker, [][]byte{udp}, 0, func(time.Duration) {}, func(_ [][]byte, accepted []bool) int {
			accepted[0] = true
			return returned
		})
		if _, ok := tracker.flowBoundary(t.Context(), window, 1, 32); ok {
			t.Fatalf("inconsistent return=%d accepted a window", returned)
		}
		diagnostic := tracker.flowDiagnostic(window)
		if !diagnostic.Invalid || diagnostic.RejectedPackets != 1 || len(diagnostic.RecentFailures) != 1 || !diagnostic.RecentFailures[0].ResultMismatch {
			t.Fatalf("inconsistent native result lacks evidence: %+v", diagnostic)
		}
	}
}

func TestFullTunBridgeBatchRejectsPartialMeasuredFlow(t *testing.T) {
	tracker := newFullTunBridgeSendTracker()
	udp := bridgeAdmissionTestPacket(false, 41000)
	path, _ := clientconnect.ParseIpPath(udp)
	window, _ := tracker.beginFlowWindow(fullTunBridgeFlowKeyFromIpPath(path))
	packets := [][]byte{udp, bridgeAdmissionTestPacket(false, 41000), bridgeAdmissionTestPacket(true, 41001)}
	sendFullTunBridgeBatch(tracker, packets, 0, func(time.Duration) {}, func(_ [][]byte, accepted []bool) int {
		accepted[0], accepted[2] = true, true
		return 2
	})
	boundary, ok := tracker.flowBoundary(t.Context(), window, 2, 64)
	if !ok || tracker.waitThrough(t.Context(), boundary) || tracker.finishFlowWindow(window) {
		t.Fatal("partially refused measured flow was accepted")
	}
	diagnostic := tracker.flowDiagnostic(window)
	if diagnostic.AcceptedPackets != 1 || diagnostic.RejectedPackets != 1 || diagnostic.ObservedPackets != 2 {
		t.Fatalf("same-flow partial outcome was lost: %+v", diagnostic)
	}
}

func TestFullTunBridgeBatchFailureDiagnosticsBounded(t *testing.T) {
	tracker := newFullTunBridgeSendTracker()
	udp := bridgeAdmissionTestPacket(false, 41000)
	path, _ := clientconnect.ParseIpPath(udp)
	window, _ := tracker.beginFlowWindow(fullTunBridgeFlowKeyFromIpPath(path))
	for batch := 0; batch < fullTunBridgeFailureBatchLimit+3; batch++ {
		packets := make([][]byte, 64)
		for i := range packets {
			packets[i] = bridgeAdmissionTestPacket(false, uint16(41000+i))
		}
		sendFullTunBridgeBatch(tracker, packets, 0, func(time.Duration) {}, func(batch [][]byte, _ []bool) int {
			for _, packet := range batch {
				clear(packet)
			}
			return 0
		})
	}
	diagnostic := tracker.flowDiagnostic(window)
	if len(diagnostic.RecentFailures) != fullTunBridgeFailureBatchLimit || diagnostic.OmittedFailureBatches != 3 {
		t.Fatalf("unbounded or unlabelled truncated history: %+v", diagnostic)
	}
	for _, batch := range diagnostic.RecentFailures {
		if len(batch.Packets) != 64 || batch.Id < 4 {
			t.Fatalf("bad bounded batch id/count=%d/%d", batch.Id, len(batch.Packets))
		}
		for i, packet := range batch.Packets {
			if packet.Index != i || !packet.Flow.valid || packet.Flow.sourcePort != uint16(41000+i) || packet.Bytes != 32 || packet.Accepted {
				t.Fatalf("metadata changed after buffer recycle: %+v", packet)
			}
		}
	}
}
