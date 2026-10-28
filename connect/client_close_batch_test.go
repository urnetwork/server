// Pending close sets must drain exactly once through bounded control carriers,
// including a backlog larger than the former 1,024-request take.
package connect

import (
	"io"
	"math"
	"testing"

	connectlib "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Exercises the Redis consumer and both supported control encodings at the
// default carrier ceiling. A mixed set checks both request kinds together.
func TestClientCloseBacklogFitsTransport(t *testing.T) {
	testClientCloseBacklogFitsTransport(t, DefaultExchangeSettings())
}

// A deployment can request a larger take without producing oversized frames.
func TestClientCloseLargeConfiguredBatchFitsTransport(t *testing.T) {
	settings := DefaultExchangeSettings()
	settings.ClientCloseBatchSize = 2048
	testClientCloseBacklogFitsTransport(t, settings)
}

// The exchange's tighter configured ceiling also bounds every emitted frame.
func TestClientCloseSmallConfiguredCarrierFitsTransport(t *testing.T) {
	settings := DefaultExchangeSettings()
	settings.FramerSettings = connectlib.DefaultFramerSettings(4096)
	testClientCloseBacklogFitsTransport(t, settings)
}

// Uses real SPOP and listener delivery; framing checks run on the calling
// goroutine so a failed assertion cannot strand the listener worker.
func testClientCloseBacklogFitsTransport(t *testing.T, settings *ExchangeSettings) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		contractIds := make([]server.Id, 1024)
		peerIds := make([]server.Id, 33)
		pendingContractKVs := map[server.Id]bool{}
		pendingPeerKVs := map[server.Id]bool{}
		for i := range contractIds {
			contractIds[i] = server.NewId()
			pendingContractKVs[contractIds[i]] = true
		}
		for i := range peerIds {
			peerIds[i] = server.NewId()
			pendingPeerKVs[peerIds[i]] = true
		}
		model.AddClientCloseRequests(ctx, clientId, contractIds, peerIds)
		// Repeated hints retain exactly one pending request of each kind.
		model.AddClientCloseRequests(ctx, clientId, contractIds[:1], peerIds[:1])

		messageLimit := min(settings.FramerSettings.MaxMessageLen, int(connectlib.DefaultClientSettings().MinimumMessageLenLimit()))
		framer := connectlib.NewFramer(connectlib.DefaultFramerSettings(messageLimit))
		frameCount := 0
		listener := newClientCloseListener(ctx, clientId, func(frame *protocol.Frame) {
			defer connectlib.MessagePoolReturn(frame.MessageBytes)
			frameCount++
			message, err := connectlib.FromFrame(frame)
			if err != nil {
				t.Fatal(err)
			}
			requests, ok := message.(*protocol.CloseSequences)
			if !ok || len(requests.ContractIds)+len(requests.PeerIds) == 0 {
				t.Fatal("listener emitted an empty or unrelated control")
			}
			for _, idBytes := range requests.ContractIds {
				id := server.RequireIdFromBytes(idBytes)
				if !pendingContractKVs[id] {
					t.Fatalf("duplicate or unexpected contract %s", id)
				}
				delete(pendingContractKVs, id)
			}
			for _, idBytes := range requests.PeerIds {
				id := server.RequireIdFromBytes(idBytes)
				if !pendingPeerKVs[id] {
					t.Fatalf("duplicate or unexpected peer %s", id)
				}
				delete(pendingPeerKVs, id)
			}
			pack := &protocol.Pack{
				MessageId:      server.NewId().Bytes(),
				SequenceId:     server.NewId().Bytes(),
				SequenceNumber: math.MaxUint64,
				Head:           true,
				Frames:         []*protocol.Frame{frame},
				Tag:            &protocol.Tag{SendTime: math.MaxUint64},
			}
			packFrame := connectlib.RequireToFrameWithDefaultProtocolVersion(pack)
			defer connectlib.MessagePoolReturn(packFrame.MessageBytes)
			path := &protocol.TransferPath{SourceId: connectlib.ControlId.Bytes(), DestinationId: clientId.Bytes()}
			for _, carrier := range []*protocol.TransferFrame{
				{TransferPath: path, Frame: packFrame},
				{TransferPath: path, Pack: pack},
			} {
				carrierBytes, err := connectlib.ProtoMarshal(carrier)
				if err != nil {
					t.Fatal(err)
				}
				err = framer.Write(io.Discard, carrierBytes)
				connectlib.MessagePoolReturn(carrierBytes)
				if err != nil {
					t.Fatalf("close carrier exceeds %d-byte transport limit: %v", messageLimit, err)
				}
			}
		}, settings)
		defer listener.CloseAndWait()
		listener.deliver()
		if frameCount < 2 || len(pendingContractKVs)+len(pendingPeerKVs) != 0 {
			t.Fatalf("frames=%d remaining contracts=%d peers=%d", frameCount, len(pendingContractKVs), len(pendingPeerKVs))
		}
		if requests := model.TakeClientCloseRequests(ctx, clientId, len(contractIds)+len(peerIds)); !requests.Empty() {
			t.Fatal("listener left pending requests behind")
		}
	})
}
