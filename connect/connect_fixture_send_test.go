// Own fixture frames through admission and retain the original test failure
// when teardown cancels a sender that is still waiting for encryption.
package connect

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
)

// Takes the frame: successful admission transfers it to the client; refusal
// returns it here. Only the owning client's shutdown permits an early return.
func TestingSendConnectFrame(
	client *connect.Client,
	frame *protocol.Frame,
	destinationId connect.Id,
	ackCallback connect.AckFunction,
	opts ...any,
) bool {
	success, err := client.SendWithTimeoutDetailed(frame, destinationId, ackCallback, -1, opts...)
	if !success {
		connect.MessagePoolReturn(frame.MessageBytes)
	}
	if err != nil {
		if client.Ctx().Err() != nil && server.IsDoneError(err) {
			return false
		}
		panic(fmt.Errorf("could not send: %w", err))
	}
	if !success {
		panic("infinite-timeout fixture send was refused")
	}
	return true
}

// Fake time parks a real Required send before cancellation. Both acknowledged
// and no-acknowledgement callers must return their unadmitted frame and leave
// the original test failure observable instead of panicking during teardown.
func TestConnectFixtureSendShutdownWhileWaitingForEncryption(t *testing.T) {
	// The pool owns a process-wide stats worker, outside any fake-time bubble.
	connect.MessagePoolReturn(connect.MessagePoolGet(2 * 1024))
	for _, noAck := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			settings := connect.DefaultClientSettings()
			settings.Log = connect.NewNoopLogger()
			settings.ControlPingTimeout = 0
			settings.EncryptionSettings.Mode = connect.EncryptionModeRequired
			client := connect.NewClient(context.Background(), connect.NewId(), connect.NewNoContractClientOob(), settings)

			frame := &protocol.Frame{
				MessageType:  protocol.MessageType_TestSimpleMessage,
				MessageBytes: connect.MessagePoolGet(2 * 1024),
			}
			witness := connect.MessagePoolShareReadOnly(frame.MessageBytes)
			defer func() {
				if err := client.CloseAndWait(context.Background()); err != nil {
					t.Errorf("join canceled fixture client: %v", err)
				}
				if !connect.MessagePoolReturn(witness) {
					connect.MessagePoolReturn(frame.MessageBytes)
					t.Errorf("noAck=%t: refused fixture frame retained its pool owner", noAck)
				}
			}()
			var opts []any
			if noAck {
				opts = append(opts, connect.NoAck())
			}
			type result struct {
				sent       bool
				panicValue any
			}
			done := make(chan result, 1)
			go func() {
				var sendResult result
				defer func() {
					sendResult.panicValue = recover()
					done <- sendResult
				}()
				sendResult.sent = TestingSendConnectFrame(client, frame, connect.NewId(), nil, opts...)
			}()
			synctest.Wait()
			select {
			case result := <-done:
				t.Fatalf("noAck=%t: send did not wait for encryption: %+v", noAck, result)
			default:
			}

			client.Close()
			got := <-done
			if got.sent || got.panicValue != nil {
				t.Errorf("noAck=%t: canceled send=%t panic=%v", noAck, got.sent, got.panicValue)
			}
		})
	}
}
