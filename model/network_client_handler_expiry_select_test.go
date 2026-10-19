// Handler expiry's verdicts (selectExpiredNetworkClientHandlers), its delete's
// recheck of a handler that heartbeat after the read, its metric, and the
// confirmation read a retired connect process uses (NetworkClientHandlerExists).
package model

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
)

func TestSelectExpiredNetworkClientHandlers(t *testing.T) {
	a, b, c := server.NewId(), server.NewId(), server.NewId()
	type selectCase struct {
		name            string
		staleHandlerIds []server.Id
		freshCount      int
		stampsCurrent   []bool
		stampsOk        bool
		expired         []server.Id
		deferredStamped int
		deferredFleet   int
	}
	cases := []selectCase{
		{
			name:            "the set vouches for every stale handler",
			staleHandlerIds: []server.Id{a, b, c},
			freshCount:      0,
			stampsCurrent:   []bool{true, true, true},
			stampsOk:        true,
			deferredStamped: 3,
		},
		{
			name:            "the set vouches for some",
			staleHandlerIds: []server.Id{a, b, c},
			freshCount:      0,
			stampsCurrent:   []bool{true, false, true},
			stampsOk:        true,
			expired:         []server.Id{b},
			deferredStamped: 2,
		},
		{
			name:            "the set vouches for none, whatever the fleet",
			staleHandlerIds: []server.Id{a, b, c},
			freshCount:      1,
			stampsCurrent:   []bool{false, false, false},
			stampsOk:        true,
			expired:         []server.Id{a, b, c},
		},
		{
			name:            "no evidence, a fleet at least as large heartbeats",
			staleHandlerIds: []server.Id{a, b},
			freshCount:      2,
			expired:         []server.Id{a, b},
		},
		{
			name:            "no evidence, fewer heartbeat",
			staleHandlerIds: []server.Id{a, b, c},
			freshCount:      2,
			deferredFleet:   3,
		},
		{
			name:            "no evidence, none heartbeat",
			staleHandlerIds: []server.Id{a},
			freshCount:      0,
			deferredFleet:   1,
		},
		{
			name:            "stamps that do not match the handlers are no evidence",
			staleHandlerIds: []server.Id{a, b, c},
			freshCount:      0,
			stampsCurrent:   []bool{false},
			stampsOk:        true,
			deferredFleet:   3,
		},
	}
	for _, c := range cases {
		expired, deferredStamped, deferredFleet := selectExpiredNetworkClientHandlers(
			c.staleHandlerIds,
			c.freshCount,
			c.stampsCurrent,
			c.stampsOk,
		)
		if !slices.Equal(expired, c.expired) || deferredStamped != c.deferredStamped || deferredFleet != c.deferredFleet {
			t.Errorf(
				"%s: expired %v, %d deferred stamped, %d deferred fleet; want %v, %d, %d",
				c.name,
				expired,
				deferredStamped,
				deferredFleet,
				c.expired,
				c.deferredStamped,
				c.deferredFleet,
			)
		}
	}
}

// A handler judged ended between the read and the delete whose heartbeat
// lands in that gap is no longer stale, and the delete's recheck keeps it.
func TestHandlerExpiryKeepsAHandlerThatHeartbeatAfterTheRead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		handlers := handlerExpiryTestHandlers(ctx, t, 2, 20101)
		now := server.NowUtc()
		handlerExpiryTestHeartbeatAt(ctx, handlers, now.Add(-10*time.Minute))
		minTime := now.Add(-2 * NetworkClientHandlerHeartbeatTimeout)

		var readHandlerIds []server.Id
		expireNetworkClientHandlers(ctx, minTime, func(ctx context.Context, handlerIds []server.Id) ([]bool, bool) {
			readHandlerIds = slices.Clone(handlerIds)
			// the first process heartbeats while expiry judges it
			if err := HeartbeatNetworkClientHandler(ctx, handlers[0].handlerId); err != nil {
				t.Fatal(err)
			}
			return make([]bool, len(handlerIds)), true
		})

		if len(readHandlerIds) != 2 {
			t.Fatalf("expiry judged %d stale handlers, want 2", len(readHandlerIds))
		}
		if !handlerExpiryTestRowExists(ctx, handlers[0].handlerId) {
			t.Fatal("expiry deleted a handler that heartbeat after its read")
		}
		if handlerExpiryTestRowExists(ctx, handlers[1].handlerId) {
			t.Fatal("expiry kept a handler it judged ended")
		}
	})
}

// No stale handler: no read of the writer set at all.
func TestHandlerExpiryReadsNoStampsWithoutStaleHandlers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		handlerExpiryTestHandlers(ctx, t, 2, 20102)

		expireNetworkClientHandlers(ctx, server.NowUtc().Add(-2*NetworkClientHandlerHeartbeatTimeout), func(ctx context.Context, handlerIds []server.Id) ([]bool, bool) {
			t.Fatal("expiry read the writer set without a stale handler")
			return nil, false
		})
	})
}

// Every stale handler is counted once by its verdict.
func TestHandlerExpiryCountsEveryVerdict(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		stampedHandlers := handlerExpiryTestHandlers(ctx, t, 2, 20103)
		endedHandlers := handlerExpiryTestHandlers(ctx, t, 1, 20104)
		now := server.NowUtc()
		handlerExpiryTestHeartbeatAt(ctx, stampedHandlers, now.Add(-10*time.Minute))
		handlerExpiryTestHeartbeatAt(ctx, endedHandlers, now.Add(-10*time.Minute))
		handlerExpiryTestStamp(ctx, t, stampedHandlers)

		counts := func() (expired float64, deferredStamped float64, deferredFleet float64) {
			return testutil.ToFloat64(networkClientHandlerExpiryCounter.WithLabelValues(networkClientHandlerExpiryExpired)),
				testutil.ToFloat64(networkClientHandlerExpiryCounter.WithLabelValues(networkClientHandlerExpiryDeferredStamped)),
				testutil.ToFloat64(networkClientHandlerExpiryCounter.WithLabelValues(networkClientHandlerExpiryDeferredFleet))
		}
		expiredBefore, deferredStampedBefore, deferredFleetBefore := counts()

		CloseExpiredNetworkClientHandlers(ctx, now.Add(-2*NetworkClientHandlerHeartbeatTimeout))

		expired, deferredStamped, deferredFleet := counts()
		if expired-expiredBefore != 1 || deferredStamped-deferredStampedBefore != 2 || deferredFleet-deferredFleetBefore != 0 {
			t.Fatalf(
				"counted %v expired, %v deferred stamped, %v deferred fleet; want 1, 2, 0",
				expired-expiredBefore,
				deferredStamped-deferredStampedBefore,
				deferredFleet-deferredFleetBefore,
			)
		}
	})
}

// The confirmation read: the row's presence, and a database failure returned,
// not raised.
func TestNetworkClientHandlerExists(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		handlerId := CreateNetworkClientHandler(ctx)
		exists, err := NetworkClientHandlerExists(ctx, handlerId)
		if err != nil || !exists {
			t.Fatalf("a present handler read (%t, %v), want (true, nil)", exists, err)
		}

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client_handler WHERE handler_id = $1`, handlerId))
		})
		exists, err = NetworkClientHandlerExists(ctx, handlerId)
		if err != nil || exists {
			t.Fatalf("a deleted handler read (%t, %v), want (false, nil)", exists, err)
		}

		canceledCtx, cancel := context.WithCancel(ctx)
		cancel()
		exists, err = NetworkClientHandlerExists(canceledCtx, handlerId)
		if err == nil || exists {
			t.Fatalf("a read the database did not answer returned (%t, %v), want (false, an error)", exists, err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Logf("unanswered read error: %v", err)
		}
	})
}
