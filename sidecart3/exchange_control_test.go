package sidecart3

// The T3 exchange's out-of-band control (exchange.go) on the private test stores: a control the controller raises on,
// as it does once a canceled platform's context reaches the db layer at teardown, still answers its callback, with the
// raise as its error. A client counts a control in flight until its callback runs and its close waits for every one, so
// a raise that skipped the callback left a client whose close never returned (TEST-M2-15's resident rows found it).

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
)

func TestT3OutOfBandControlAnswersAControllerRaise(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true}).Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithCancel(t.Context())
		// a platform at teardown: the controller's first db call raises the cancellation
		cancel()
		control := &controllerOutOfBandControl{
			ctx:                     ctx,
			clientId:                server.NewId(),
			contractManagerSettings: connect.DefaultContractManagerSettings(),
		}
		contractId := make([]byte, 16)
		if _, err := rand.Read(contractId); err != nil {
			t.Fatal(err)
		}
		frame := connect.RequireToFrameWithDefaultProtocolVersion(&protocol.CloseContract{ContractId: contractId})
		answered := make(chan error, 1)
		control.SendControl([]*protocol.Frame{frame}, func(resultFrames []*protocol.Frame, err error) {
			answered <- err
		})
		select {
		case err := <-answered:
			if err == nil {
				t.Error("a control the controller raised on answered no error")
			}
		case <-time.After(30 * time.Second):
			// only a failure's bound: the callback runs as soon as the raise is taken
			t.Fatal("a control the controller raised on never answered its callback")
		}
	})
}
