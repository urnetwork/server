// The heartbeat over the real handler row (handler_heartbeat.go): rounds driven
// one by one detect expiry's deletion with a real heartbeat, hold the process
// while the database does not answer, and shut it down once answered reads
// confirm; and the production constructor's own loop ends the process the
// way the connect router's cancel does.
package connect

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The 2026-10-19 shape over the database, driven round by round: expiry
// deletes the live handler's row while the database does not answer, and the
// process holds until three answered reads find the row gone.
func TestHandlerHeartbeatOverTheDatabaseHoldsThenShutsDownARetiredProcess(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		handlerId := model.CreateNetworkClientHandler(ctx)
		answering := true
		shutdownCount := 0
		heartbeat := newHandlerHeartbeatWithStore(
			ctx,
			handlerId,
			&HandlerHeartbeatSettings{
				HeartbeatInterval:       time.Second,
				RetirementConfirmations: 3,
				ConfirmationTimeout:     5 * time.Second,
				ShutdownJitter:          0,
			},
			model.HeartbeatNetworkClientHandler,
			func(ctx context.Context, handlerId server.Id) (bool, error) {
				if !answering {
					// a read whose deadline passed before the database answered
					unansweredCtx, unansweredCancel := context.WithCancel(ctx)
					unansweredCancel()
					return model.NetworkClientHandlerExists(unansweredCtx, handlerId)
				}
				return model.NetworkClientHandlerExists(ctx, handlerId)
			},
			func(maxDelay time.Duration) time.Duration {
				return 0
			},
			func() {
				shutdownCount += 1
			},
		)
		now := time.Unix(1_800_000_000, 0)
		roundCount := 0
		nextRound := func() {
			heartbeat.round(now.Add(time.Duration(roundCount) * 5 * time.Second))
			roundCount += 1
		}

		for range 3 {
			nextRound()
		}
		if shutdownCount != 0 || heartbeat.retired {
			t.Fatal("a live handler's heartbeat retired it")
		}

		// expiry deletes the handler while the database path fails
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client_handler WHERE handler_id = $1`, handlerId))
		})
		answering = false
		nextRound()
		if shutdownCount != 0 {
			t.Fatal("the process shut down on the first heartbeat that found its handler row gone")
		}
		if !heartbeat.retired {
			t.Fatal("the heartbeat did not find the handler row gone")
		}
		for i := range 20 {
			nextRound()
			if shutdownCount != 0 {
				t.Fatalf("the process shut down after %d unanswered confirmation reads; it must hold while the database does not answer", i+1)
			}
		}

		answering = true
		nextRound()
		nextRound()
		if shutdownCount != 0 {
			t.Fatal("the process shut down before three answered reads")
		}
		nextRound()
		if shutdownCount != 1 {
			t.Fatalf("three answered reads found the row gone and the process shut down %d times, want once", shutdownCount)
		}
	})
}

// The production constructor starts its own loop over the real store: a
// deleted row is detected, confirmed by answered reads, and the confirming
// round cancels the process as the connect router's cancel does.
func TestNewHandlerHeartbeatShutsDownAConfirmedRetirement(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		handlerId := model.CreateNetworkClientHandler(ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client_handler WHERE handler_id = $1`, handlerId))
		})

		processCtx, processCancel := context.WithCancel(ctx)
		defer processCancel()
		shutdownBefore := handlerRetirementCount(handlerRetirementShutdown)
		newHandlerHeartbeat(
			processCtx,
			handlerId,
			&HandlerHeartbeatSettings{
				HeartbeatInterval:       5 * time.Millisecond,
				RetirementConfirmations: 3,
				ConfirmationTimeout:     5 * time.Second,
				ShutdownJitter:          20 * time.Millisecond,
			},
			processCancel,
		)
		select {
		case <-processCtx.Done():
		case <-time.After(30 * time.Second):
			t.Fatal("a confirmed retirement did not shut the process down")
		}
		if got := handlerRetirementCount(handlerRetirementShutdown) - shutdownBefore; got != 1 {
			t.Fatalf("the process ended with %v confirmed shutdowns, want 1", got)
		}
	})
}
