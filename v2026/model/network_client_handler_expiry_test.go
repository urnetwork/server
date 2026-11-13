// Handler expiry against the shapes that decide it (network_client_handler_
// retirement.go): a database-path outage that stops every heartbeat while the
// processes still stamp the writer set, an ended process, a lost host, a
// restarted fleet, and a writer set that cannot vouch. Each test reads the
// outcome where it matters: the handler row, its connection row, and the
// process's next heartbeat.
package model

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A handler with one connected row, as a connect process registers them.
type handlerExpiryTestHandler struct {
	handlerId    server.Id
	connectionId server.Id
}

// Creates count handlers, each with one connection of its own client.
func handlerExpiryTestHandlers(ctx context.Context, t testing.TB, count int, port int) []handlerExpiryTestHandler {
	t.Helper()
	handlers := make([]handlerExpiryTestHandler, count)
	for i := range handlers {
		handlerId := CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := ConnectNetworkClient(
			ctx,
			server.NewId(),
			fmt.Sprintf("192.0.2.%d:%d", 10+i, port),
			handlerId,
		)
		if err != nil {
			t.Fatal(err)
		}
		handlers[i] = handlerExpiryTestHandler{
			handlerId:    handlerId,
			connectionId: connectionId,
		}
	}
	return handlers
}

// Sets the handlers' last heartbeat, as a heartbeat path that stopped at
// heartbeatTime leaves them.
func handlerExpiryTestHeartbeatAt(ctx context.Context, handlers []handlerExpiryTestHandler, heartbeatTime time.Time) {
	for _, handler := range handlers {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_client_handler SET heartbeat_time = $2 WHERE handler_id = $1`,
				handler.handlerId,
				heartbeatTime.UTC(),
			))
		})
	}
}

// Stamps the handlers in the writer set, as their live processes do.
func handlerExpiryTestStamp(ctx context.Context, t testing.TB, handlers []handlerExpiryTestHandler) {
	t.Helper()
	for _, handler := range handlers {
		if err := PublishProviderLivenessWriter(ctx, handler.handlerId); err != nil {
			t.Fatal(err)
		}
	}
}

// Whether the handler row exists, read on the primary.
func handlerExpiryTestRowExists(ctx context.Context, handlerId server.Id) (exists bool) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT EXISTS(SELECT 1 FROM network_client_handler WHERE handler_id = $1)`,
			handlerId,
		).Scan(&exists))
	})
	return
}

// Fails unless every handler kept its row and its connected row, and takes its
// next heartbeat, which is what keeps its process serving its providers.
func handlerExpiryTestRequireKept(ctx context.Context, t testing.TB, label string, handlers []handlerExpiryTestHandler) {
	t.Helper()
	for i, handler := range handlers {
		if !handlerExpiryTestRowExists(ctx, handler.handlerId) {
			t.Fatalf("%s handler %d: expiry deleted the row of a live process", label, i)
		}
		if !GetNetworkClientConnectionStatus(ctx, handler.connectionId).Connected {
			t.Fatalf("%s handler %d: expiry retired the connection of a live process", label, i)
		}
		if err := HeartbeatNetworkClientHandler(ctx, handler.handlerId); err != nil {
			t.Fatalf("%s handler %d: the next heartbeat failed (%s), which shuts its process down", label, i, err)
		}
	}
}

// Fails unless every handler's row is gone and its connection retired.
func handlerExpiryTestRequireExpired(ctx context.Context, t testing.TB, label string, handlers []handlerExpiryTestHandler) {
	t.Helper()
	for i, handler := range handlers {
		if handlerExpiryTestRowExists(ctx, handler.handlerId) {
			t.Fatalf("%s handler %d: expiry kept the row of an ended process", label, i)
		}
		if GetNetworkClientConnectionStatus(ctx, handler.connectionId).Connected {
			t.Fatalf("%s handler %d: expiry left the connection of an ended process connected", label, i)
		}
	}
}

// The 2026-10-19 shape: a pooler livelock stopped every heartbeat for longer
// than two heartbeat timeouts while every process kept stamping the writer
// set. Expiry must keep them all, or each process shuts itself down on its
// next heartbeat that gets through.
func TestHandlerExpiryKeepsStampedHandlersWhenEveryHeartbeatStops(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		handlers := handlerExpiryTestHandlers(ctx, t, 6, 20001)
		now := server.NowUtc()
		handlerExpiryTestHeartbeatAt(ctx, handlers, now.Add(-10*time.Minute))
		handlerExpiryTestStamp(ctx, t, handlers)

		CloseExpiredNetworkClientHandlers(ctx, now.Add(-2*NetworkClientHandlerHeartbeatTimeout))

		handlerExpiryTestRequireKept(ctx, t, "outage", handlers)
	})
}

// A partial outage: one pool still passes some heartbeats, so a few handlers
// stay fresh while most go stale. Their processes still stamp, so expiry keeps
// every one.
func TestHandlerExpiryKeepsStampedHandlersWhenMostHeartbeatsStop(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		staleHandlers := handlerExpiryTestHandlers(ctx, t, 5, 20002)
		freshHandlers := handlerExpiryTestHandlers(ctx, t, 1, 20003)
		now := server.NowUtc()
		handlerExpiryTestHeartbeatAt(ctx, staleHandlers, now.Add(-10*time.Minute))
		handlerExpiryTestStamp(ctx, t, staleHandlers)
		handlerExpiryTestStamp(ctx, t, freshHandlers)

		CloseExpiredNetworkClientHandlers(ctx, now.Add(-2*NetworkClientHandlerHeartbeatTimeout))

		handlerExpiryTestRequireKept(ctx, t, "stale", staleHandlers)
		handlerExpiryTestRequireKept(ctx, t, "fresh", freshHandlers)
	})
}

// No process stamped within the window, so the writer set cannot vouch, and no
// handler heartbeat: a total outage of the heartbeat path, or of every
// process. Expiry keeps every handler until a process shows it is alive.
func TestHandlerExpiryKeepsEveryHandlerWhenNoneHeartbeatAndTheSetCannotVouch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		handlers := handlerExpiryTestHandlers(ctx, t, 4, 20004)
		now := server.NowUtc()
		handlerExpiryTestHeartbeatAt(ctx, handlers, now.Add(-10*time.Minute))

		CloseExpiredNetworkClientHandlers(ctx, now.Add(-2*NetworkClientHandlerHeartbeatTimeout))

		handlerExpiryTestRequireKept(ctx, t, "outage", handlers)
	})
}

// One process ended among live ones: its heartbeat and its stamps stopped,
// while the others heartbeat and stamp. Expiry deletes it and retires its row.
func TestHandlerExpiryDeletesTheHandlerOfAnEndedProcess(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		liveHandlers := handlerExpiryTestHandlers(ctx, t, 5, 20005)
		endedHandlers := handlerExpiryTestHandlers(ctx, t, 1, 20006)
		now := server.NowUtc()
		handlerExpiryTestHeartbeatAt(ctx, endedHandlers, now.Add(-10*time.Minute))
		handlerExpiryTestStamp(ctx, t, liveHandlers)

		CloseExpiredNetworkClientHandlers(ctx, now.Add(-2*NetworkClientHandlerHeartbeatTimeout))

		handlerExpiryTestRequireExpired(ctx, t, "ended", endedHandlers)
		handlerExpiryTestRequireKept(ctx, t, "live", liveHandlers)
	})
}

// A lost host: several processes ended together, a minority of the fleet.
func TestHandlerExpiryDeletesTheHandlersOfALostHost(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		liveHandlers := handlerExpiryTestHandlers(ctx, t, 7, 20007)
		lostHandlers := handlerExpiryTestHandlers(ctx, t, 3, 20008)
		now := server.NowUtc()
		handlerExpiryTestHeartbeatAt(ctx, lostHandlers, now.Add(-10*time.Minute))
		handlerExpiryTestStamp(ctx, t, liveHandlers)

		CloseExpiredNetworkClientHandlers(ctx, now.Add(-2*NetworkClientHandlerHeartbeatTimeout))

		handlerExpiryTestRequireExpired(ctx, t, "lost", lostHandlers)
		handlerExpiryTestRequireKept(ctx, t, "live", liveHandlers)
	})
}

// A restarted fleet, even one that came back smaller: the old processes
// stopped stamping and heartbeating, and the new ones do both. Expiry deletes
// every old handler however few new ones there are.
func TestHandlerExpiryDeletesTheOldHandlersOfASmallerRestartedFleet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		oldHandlers := handlerExpiryTestHandlers(ctx, t, 6, 20009)
		newHandlers := handlerExpiryTestHandlers(ctx, t, 2, 20010)
		now := server.NowUtc()
		handlerExpiryTestHeartbeatAt(ctx, oldHandlers, now.Add(-10*time.Minute))
		handlerExpiryTestStamp(ctx, t, newHandlers)

		CloseExpiredNetworkClientHandlers(ctx, now.Add(-2*NetworkClientHandlerHeartbeatTimeout))

		handlerExpiryTestRequireExpired(ctx, t, "old", oldHandlers)
		handlerExpiryTestRequireKept(ctx, t, "new", newHandlers)
	})
}

// A total outage ends: the stale handlers were kept while nothing could vouch,
// and the first replacement process to stamp lets the next round delete the
// ones whose processes are gone.
func TestHandlerExpiryDeletesKeptHandlersOnceAReplacementStamps(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		goneHandlers := handlerExpiryTestHandlers(ctx, t, 4, 20011)
		now := server.NowUtc()
		handlerExpiryTestHeartbeatAt(ctx, goneHandlers, now.Add(-10*time.Minute))

		CloseExpiredNetworkClientHandlers(ctx, now.Add(-2*NetworkClientHandlerHeartbeatTimeout))
		handlerExpiryTestRequireKept(ctx, t, "kept", goneHandlers)
		// the requirement heartbeat them; the processes are gone, so their
		// heartbeat stops again
		handlerExpiryTestHeartbeatAt(ctx, goneHandlers, now.Add(-10*time.Minute))

		replacementHandlers := handlerExpiryTestHandlers(ctx, t, 1, 20012)
		handlerExpiryTestStamp(ctx, t, replacementHandlers)

		CloseExpiredNetworkClientHandlers(ctx, server.NowUtc().Add(-2*NetworkClientHandlerHeartbeatTimeout))

		handlerExpiryTestRequireExpired(ctx, t, "gone", goneHandlers)
		handlerExpiryTestRequireKept(ctx, t, "replacement", replacementHandlers)
	})
}

// The writer set cannot vouch (Redis lost its stamps, or no process stamps),
// but most of the fleet still heartbeats: the stale minority ended, and
// expiry deletes it.
func TestHandlerExpiryFallsBackToTheFleetWhenTheSetCannotVouch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		liveHandlers := handlerExpiryTestHandlers(ctx, t, 3, 20013)
		endedHandlers := handlerExpiryTestHandlers(ctx, t, 1, 20014)
		now := server.NowUtc()
		handlerExpiryTestHeartbeatAt(ctx, endedHandlers, now.Add(-10*time.Minute))

		CloseExpiredNetworkClientHandlers(ctx, now.Add(-2*NetworkClientHandlerHeartbeatTimeout))

		handlerExpiryTestRequireExpired(ctx, t, "ended", endedHandlers)
		handlerExpiryTestRequireKept(ctx, t, "live", liveHandlers)
	})
}

// A stamp older than the writer window vouches for nothing: a process that
// stopped stamping two minutes ago ended, whatever its last stamp.
func TestHandlerExpiryDeletesAHandlerWhoseStampLapsed(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		liveHandlers := handlerExpiryTestHandlers(ctx, t, 3, 20015)
		endedHandlers := handlerExpiryTestHandlers(ctx, t, 1, 20016)
		now := server.NowUtc()
		handlerExpiryTestHeartbeatAt(ctx, endedHandlers, now.Add(-10*time.Minute))
		handlerExpiryTestStamp(ctx, t, liveHandlers)
		nodeTime := livenessTestNodeTime(ctx, t)
		livenessTestScore(ctx, t, providerLivenessWritersKey, map[string]int64{
			endedHandlers[0].handlerId.String(): nodeTime.Add(-2 * time.Minute).UnixMilli(),
		})

		CloseExpiredNetworkClientHandlers(ctx, now.Add(-2*NetworkClientHandlerHeartbeatTimeout))

		handlerExpiryTestRequireExpired(ctx, t, "lapsed", endedHandlers)
		handlerExpiryTestRequireKept(ctx, t, "live", liveHandlers)
	})
}
