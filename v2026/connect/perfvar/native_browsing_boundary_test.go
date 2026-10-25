// The ordinary PERFVAR census must exercise the same native batch sender as
// browsing, including unavailable remote admission before a later ready flow.
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

// Empty remote discovery is deliberate admission pressure, not an error or a
// fabricated full global budget. The local override has independent capacity.
type nativeBrowsingEmptyGenerator struct {
	calls atomic.Int64
}

// No peer may be selected until this fixture has finished its assertion.
func (self *nativeBrowsingEmptyGenerator) NextDestinations(int, []clientconnect.MultiHopId, string) (map[clientconnect.MultiHopId]clientconnect.DestinationStats, error) {
	self.calls.Add(1)
	return map[clientconnect.MultiHopId]clientconnect.DestinationStats{}, nil
}

// A generated provider would violate this fixture's unavailable-remote premise.
func (self *nativeBrowsingEmptyGenerator) NewClientArgs() (*clientconnect.MultiClientGeneratorClientArgs, error) {
	return nil, errors.New("synthetic remote unavailable")
}

// No argument owner is created by this fixture.
func (self *nativeBrowsingEmptyGenerator) RemoveClientArgs(*clientconnect.MultiClientGeneratorClientArgs) {
}

// No client owner is created by this fixture.
func (self *nativeBrowsingEmptyGenerator) RemoveClientWithArgs(*clientconnect.Client, *clientconnect.MultiClientGeneratorClientArgs) {
}

// Keep constructor settings real even though enumeration returns no peer.
func (self *nativeBrowsingEmptyGenerator) NewClientSettings() *clientconnect.ClientSettings {
	return clientconnect.DefaultClientSettings()
}

// No route is created behind the missing discovery result.
func (self *nativeBrowsingEmptyGenerator) NewClient(context.Context, *clientconnect.MultiClientGeneratorClientArgs, *clientconnect.ClientSettings) (*clientconnect.Client, error) {
	return nil, errors.New("synthetic remote unavailable")
}

// This is a cold dynamic selection, not a fixed known unhealthy endpoint.
func (self *nativeBrowsingEmptyGenerator) FixedDestinationSize() (int, bool) { return 0, false }

// The previous read contains a remote original; only after it returns may the
// next read offer a separately ready local request. Real NAT socket delivery,
// not a count-only callback, must finish before any remote peer becomes ready.
func TestFullTunNativeBridgeNextReadSurvivesUnavailableRemote(t *testing.T) {
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
		workers.Add(1)
		started := time.Now()
		go func() {
			defer workers.Done()
			firstCount := sendFullTunBridgeBatch(tracker, first, 0, func(time.Duration) {}, send)
			secondCount := sendFullTunBridgeBatch(tracker, second, 0, func(time.Duration) {}, send)
			done <- [2]int{firstCount, secondCount}
		}()
		synctest.Wait()
		select {
		case counts := <-done:
			if counts != [2]int{0, 1} || time.Since(started) != 0 {
				t.Fatalf("native bridge disposition/time=%v/%s", counts, time.Since(started))
			}
		default:
			t.Fatal("NATIVE_PERFVAR_NEXT_READ_WAITED_FOR_REMOTE: first batch owns the serialized next-read boundary")
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
