// The heartbeat over the real handler row (handler_heartbeat.go): a real
// heartbeat detects expiry's deletion, confirmation reads the database does
// not answer hold the process, and answered ones shut it down.
package connect

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func TestHandlerHeartbeatOverTheDatabaseHoldsThenShutsDownARetiredProcess(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		handlerId := model.CreateNetworkClientHandler(ctx)

		heartbeatErrs := make(chan error, 4096)
		readErrs := make(chan error, 4096)
		var answering atomic.Bool
		shutdown := make(chan struct{})
		heartbeat := newHandlerHeartbeatWithStore(
			ctx,
			handlerId,
			&HandlerHeartbeatSettings{
				HeartbeatInterval:       5 * time.Millisecond,
				RetirementConfirmations: 3,
				ConfirmationTimeout:     5 * time.Second,
				ShutdownJitter:          0,
			},
			func(ctx context.Context, handlerId server.Id) error {
				err := model.HeartbeatNetworkClientHandler(ctx, handlerId)
				heartbeatErrs <- err
				return err
			},
			func(ctx context.Context, handlerId server.Id) (bool, error) {
				readCtx := ctx
				if !answering.Load() {
					// a database that does not answer: the read's deadline passed
					canceledCtx, cancelRead := context.WithCancel(ctx)
					cancelRead()
					readCtx = canceledCtx
				}
				exists, err := model.NetworkClientHandlerExists(readCtx, handlerId)
				readErrs <- err
				return exists, err
			},
			func(maxDelay time.Duration) time.Duration {
				return 0
			},
			func() {
				close(shutdown)
			},
		)
		go heartbeat.run()

		for range 3 {
			if err := <-heartbeatErrs; err != nil {
				t.Fatalf("a live handler's heartbeat failed: %s", err)
			}
		}

		// expiry deletes the handler while the database path fails
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client_handler WHERE handler_id = $1`, handlerId))
		})
		for err := range heartbeatErrs {
			if err != nil {
				break
			}
		}

		for i := range 20 {
			select {
			case err := <-readErrs:
				if err == nil {
					t.Fatalf("confirmation read %d was answered, want unanswered", i)
				}
			case <-shutdown:
				t.Fatalf("the process shut down after %d unanswered confirmation reads; it must hold while the database does not answer", i)
			}
		}
		select {
		case <-shutdown:
			t.Fatal("the process shut down while the database did not answer its confirmation reads")
		default:
		}

		answering.Store(true)
		select {
		case <-shutdown:
		case <-time.After(30 * time.Second):
			t.Fatal("answered reads found the row gone and the process did not shut down")
		}
	})
}

// The production constructor over the real store starts its own loop: a
// deleted row is detected, confirmed by answered reads, and shuts the process
// down after its jitter.
func TestNewHandlerHeartbeatShutsDownAConfirmedRetirement(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		handlerId := model.CreateNetworkClientHandler(ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client_handler WHERE handler_id = $1`, handlerId))
		})

		shutdown := make(chan struct{})
		newHandlerHeartbeat(
			ctx,
			handlerId,
			&HandlerHeartbeatSettings{
				HeartbeatInterval:       5 * time.Millisecond,
				RetirementConfirmations: 3,
				ConfirmationTimeout:     5 * time.Second,
				ShutdownJitter:          20 * time.Millisecond,
			},
			func() {
				close(shutdown)
			},
		)
		select {
		case <-shutdown:
		case <-time.After(30 * time.Second):
			t.Fatal("a confirmed retirement did not shut the process down")
		}
	})
}
