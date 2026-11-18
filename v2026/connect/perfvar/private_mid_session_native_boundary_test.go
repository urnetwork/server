// This source targets current Connect and is intended to build unchanged on a
// compatible, pinned pre-native closure. No historical OLD arm is bound yet.
// Real native sender capability decides the path; no private API substitutes for it.
package perfvar

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// The previous read contains a remote original; only after it returns may the
// next read offer a separately ready local request. Real NAT socket delivery,
// not a count-only callback, must finish before any remote peer becomes ready.
func TestPrivateMidSessionNativeNextReadFollowsReadyPacket(t *testing.T) {
	// Process-owned pool maintenance must not be born inside the test bubble.
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(64))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		generator := &nativeBrowsingEmptyGenerator{}
		settings := clientconnect.DefaultMultiClientSettings()
		settings.Log = clientconnect.NewNoopLogger()
		settings.HeartbeatInterval, settings.ProviderProbe, settings.IpAssocSettings = 0, false, nil
		settings.SecurityPolicyGenerator = func(context.Context, *clientconnect.SecurityPolicyStatsCollector) clientconnect.SecurityPolicy {
			return clientconnect.DisableSecurityPolicy()
		}
		clientSocket, serverSocket := net.Pipe()
		var dialed atomic.Int64
		settings.LocalDialContextSettings = &clientconnect.DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) {
			if dialed.Add(1) != 1 {
				return nil, errors.New("duplicate synthetic local socket")
			}
			return clientSocket, nil
		}}
		replies := make(chan []byte, 1)
		multi := clientconnect.NewRemoteUserNatMultiClient(ctx, generator,
			func(_ clientconnect.TransferPath, _ protocol.ProvideMode, forwardPath *clientconnect.IpPath, packet []byte) {
				replyPath, payload, err := clientconnect.ParseIpPathWithPayload(packet)
				if err != nil || replyPath.Protocol != clientconnect.IpProtocolUdp ||
					!replyPath.SourceIp.Equal(net.IPv4(198, 51, 100, 2)) || replyPath.SourcePort != 42000 ||
					!replyPath.DestinationIp.Equal(net.IPv4(192, 0, 2, 1)) || replyPath.DestinationPort != 41002 ||
					forwardPath.Protocol != clientconnect.IpProtocolUdp ||
					!forwardPath.SourceIp.Equal(net.IPv4(192, 0, 2, 1)) || forwardPath.SourcePort != 41002 ||
					!forwardPath.DestinationIp.Equal(net.IPv4(198, 51, 100, 2)) || forwardPath.DestinationPort != 42000 {
					t.Error("independent local response changed flow identity")
					return
				}
				select {
				case replies <- append([]byte(nil), payload...):
				default:
					t.Error("duplicate local response exceeded its one-reply owner")
				}
			}, protocol.ProvideMode_Network, settings)
		multi.SetBlockActionOverrides([]*clientconnect.BlockActionOverride{{
			OverrideId: clientconnect.NewId(), Hosts: []string{"198.51.100.2"},
			RouteOverride: &clientconnect.RouteOverride{Local: true},
		}})
		remote := bridgeAdmissionTestPacket(true, 41001)
		local := bridgeAdmissionTestPacket(false, 41002)
		copy(local[16:20], []byte{198, 51, 100, 2})
		copy(local[28:], []byte("tiny"))
		first, second := [][]byte{clientconnect.MessagePoolCopy(remote)}, [][]byte{clientconnect.MessagePoolCopy(local)}
		witnesses := [][]byte{clientconnect.MessagePoolShareReadOnly(first[0]), clientconnect.MessagePoolShareReadOnly(second[0])}
		var workers sync.WaitGroup
		defer func() {
			cancel()
			_ = clientSocket.Close()
			_ = serverSocket.Close()
			workers.Wait()
			joinCtx, joinCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer joinCancel()
			if err := multi.CloseAndWait(joinCtx); err != nil {
				t.Error(err)
			}
			for _, witness := range witnesses {
				if !clientconnect.MessagePoolReturn(witness) {
					t.Error("native bridge packet retained an owner after joined cleanup")
				}
			}
		}()
		workers.Add(1)
		go func() {
			defer workers.Done()
			var body [4]byte
			if _, err := io.ReadFull(serverSocket, body[:]); err != nil {
				return
			}
			if !bytes.Equal(body[:], []byte("tiny")) {
				t.Error("local server received wrong request")
				return
			}
			_, _ = serverSocket.Write([]byte("done"))
		}()
		tracker := newFullTunBridgeSendTracker()
		send := nativeBrowsingBatchSender(multi, clientconnect.SourceId(clientconnect.NewId()))
		done := make(chan [2]int, 1)
		firstSendEntered := make(chan struct{})
		nextReady := make(chan [][]byte, 1)
		var nextReadyPublished, nextReadReturned, firstCallReturned atomic.Bool
		var sendCalls atomic.Int64
		recordedSend := func(packets [][]byte, accepted []bool) int {
			if sendCalls.Add(1) == 1 {
				close(firstSendEntered)
			}
			return send(packets, accepted)
		}
		workers.Add(1)
		started := time.Now()
		go func() {
			defer workers.Done()
			firstCount := sendFullTunBridgeBatch(tracker, first, 0, func(time.Duration) {}, recordedSend)
			firstCallReturned.Store(true)
			readyBatch := <-nextReady
			if !nextReadyPublished.Load() {
				t.Error("next read preceded its real ready-packet publication")
			}
			nextReadReturned.Store(true)
			secondCount := sendFullTunBridgeBatch(tracker, readyBatch, 0, func(time.Duration) {}, recordedSend)
			done <- [2]int{firstCount, secondCount}
		}()
		<-firstSendEntered
		// Independent work is queued after configured-sender wrapper entry. A valid OLD
		// call may still own the bridge; Wait observes quiescence, not a timeout.
		nextReadyPublished.Store(true)
		nextReady <- second
		synctest.Wait()
		if !nextReadReturned.Load() {
			if firstCallReturned.Load() {
				t.Fatal("ready queue was not read after the source returned")
			}
			t.Fatal("MID_SESSION_NATIVE_NEXT_READ_BLOCKED: first source call has not returned while exact independent packet is queued")
		}
		select {
		case counts := <-done:
			if counts != [2]int{0, 1} || time.Since(started) != 0 {
				t.Fatalf("native bridge disposition/time=%v/%s", counts, time.Since(started))
			}
		default:
			t.Fatal("MID_SESSION_NATIVE_NEXT_READ_BLOCKED: first batch owns the serialized next-read boundary")
		}
		select {
		case reply := <-replies:
			if !bytes.Equal(reply, []byte("done")) {
				t.Fatal("later ready request lost its exact response")
			}
		default:
			t.Fatal("later ready request never completed while remote remained unavailable")
		}
		if dialed.Load() != 1 || multi.PacketStats().LocalEgressPacketCount != 1 ||
			tracker.failureCount.Load() != 1 || generator.calls.Load() == 0 {
			t.Fatal("bridge pressure, local admission, or exact refusal accounting was not exercised")
		}
	})
}
