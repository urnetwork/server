package connect

import (
	"bytes"
	"context"
	"testing"
	"testing/synctest"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

// Uses real AddForward -> Client.ForwardWithTimeout -> ForwardSequence
// admission. A first accepted frame owns the route wait; the second remains
// outside the zero-capacity sequence queue when its exchange caller closes.
func TestResidentForwardCloseCancelsUnadmittedWork(t *testing.T) {
	// The process-global pool statistics worker belongs outside this bubble.
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := clientconnect.DefaultClientSettingsWithBufferSize(0)
		client := clientconnect.NewClient(ctx, clientconnect.NewId(), clientconnect.NewNoContractClientOob(), settings)
		defer func() {
			cancel()
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		resident := &Resident{ctx: ctx, client: client, exchange: &Exchange{settings: DefaultExchangeSettingsWithBufferSize(0)}}
		forward, closeForward, err := resident.AddForward()
		if err != nil {
			t.Fatal(err)
		}
		destination := clientconnect.NewId()
		message := func(value string) []byte {
			data, err := proto.Marshal(&protocol.TransferFrame{TransferPath: &protocol.TransferPath{SourceId: clientconnect.NewId().Bytes(), DestinationId: destination.Bytes()}, EncryptedTransferFrame: []byte(value)})
			if err != nil {
				t.Fatal(err)
			}
			return clientconnect.MessagePoolCopy(data)
		}
		first := message("accepted sibling remains owned")
		second := message("unadmitted exchange caller cancels")
		firstWitness := clientconnect.MessagePoolShareReadOnly(first)
		secondWitness := clientconnect.MessagePoolShareReadOnly(second)
		forward <- first
		forward <- second
		synctest.Wait()
		closed := make(chan struct{})
		go func() { closeForward(); close(closed) }()
		synctest.Wait()
		returned := false
		select {
		case <-closed:
			returned = true
		default:
		}
		if returned {
			if client.IsDone() {
				t.Error("exchange caller close canceled healthy resident client")
			}
			route := make(chan []byte, 1)
			client.RouteManager().UpdateTransport(clientconnect.NewSendClientTransport(clientconnect.DestinationId(destination)), []clientconnect.Route{route})
			delivered := <-route
			if !bytes.Equal(delivered, firstWitness) {
				t.Error("exchange caller close changed accepted sibling bytes")
			}
			clientconnect.MessagePoolReturn(delivered)
		}
		cancel()
		if !returned {
			<-closed
		}
		if err := client.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		requireResidentPoolOwnerReturned(t, firstWitness, "accepted forward sibling")
		requireResidentPoolOwnerReturned(t, secondWitness, "unadmitted forward caller")
		if !returned {
			t.Fatal("exchange forward close waited for resident-wide admission instead of canceling its caller")
		}
	})
}
