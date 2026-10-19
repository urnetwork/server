// Handler expiry's verdicts (selectExpiredNetworkClientHandlers) and its judging
// of the writer stamps (judgeNetworkClientHandlerStamps), its delete's recheck
// of a handler that heartbeat after the read, its metric, and the confirmation
// read a retired connect process uses (NetworkClientHandlerExists).
package model

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
)

// Every verdict: stamps decide while the set vouches, and the fleet decides
// while it does not.
func TestSelectExpiredNetworkClientHandlers(t *testing.T) {
	firstHandlerId, secondHandlerId, thirdHandlerId := server.NewId(), server.NewId(), server.NewId()
	type selectCase struct {
		name                 string
		staleHandlerIds      []server.Id
		freshCount           int
		stampedHandlerIds    map[server.Id]bool
		stampsKnown          bool
		expiredHandlerIds    []server.Id
		deferredStampedCount int
		deferredFleetCount   int
	}
	cases := []selectCase{
		{
			name:                 "the set vouches for every stale handler",
			staleHandlerIds:      []server.Id{firstHandlerId, secondHandlerId, thirdHandlerId},
			freshCount:           0,
			stampedHandlerIds:    map[server.Id]bool{firstHandlerId: true, secondHandlerId: true, thirdHandlerId: true},
			stampsKnown:          true,
			deferredStampedCount: 3,
		},
		{
			name:                 "the set vouches for some",
			staleHandlerIds:      []server.Id{firstHandlerId, secondHandlerId, thirdHandlerId},
			freshCount:           0,
			stampedHandlerIds:    map[server.Id]bool{firstHandlerId: true, thirdHandlerId: true},
			stampsKnown:          true,
			expiredHandlerIds:    []server.Id{secondHandlerId},
			deferredStampedCount: 2,
		},
		{
			name:              "the set vouches for none, whatever the fleet",
			staleHandlerIds:   []server.Id{firstHandlerId, secondHandlerId, thirdHandlerId},
			freshCount:        1,
			stampedHandlerIds: map[server.Id]bool{},
			stampsKnown:       true,
			expiredHandlerIds: []server.Id{firstHandlerId, secondHandlerId, thirdHandlerId},
		},
		{
			name:              "no evidence, a fleet at least as large heartbeats",
			staleHandlerIds:   []server.Id{firstHandlerId, secondHandlerId},
			freshCount:        2,
			expiredHandlerIds: []server.Id{firstHandlerId, secondHandlerId},
		},
		{
			name:               "no evidence, fewer heartbeat",
			staleHandlerIds:    []server.Id{firstHandlerId, secondHandlerId, thirdHandlerId},
			freshCount:         2,
			deferredFleetCount: 3,
		},
		{
			name:               "no evidence, none heartbeat",
			staleHandlerIds:    []server.Id{firstHandlerId},
			freshCount:         0,
			deferredFleetCount: 1,
		},
		{
			name:               "stamps the set cannot vouch for are no evidence",
			staleHandlerIds:    []server.Id{firstHandlerId, secondHandlerId},
			freshCount:         0,
			stampedHandlerIds:  map[server.Id]bool{firstHandlerId: true, secondHandlerId: true},
			stampsKnown:        false,
			deferredFleetCount: 2,
		},
	}
	for _, selected := range cases {
		expiredHandlerIds, deferredStampedCount, deferredFleetCount := selectExpiredNetworkClientHandlers(
			selected.staleHandlerIds,
			selected.freshCount,
			selected.stampedHandlerIds,
			selected.stampsKnown,
		)
		if !slices.Equal(expiredHandlerIds, selected.expiredHandlerIds) || deferredStampedCount != selected.deferredStampedCount || deferredFleetCount != selected.deferredFleetCount {
			t.Errorf(
				"%s: expired %v, %d deferred stamped, %d deferred fleet; want %v, %d, %d",
				selected.name,
				expiredHandlerIds,
				deferredStampedCount,
				deferredFleetCount,
				selected.expiredHandlerIds,
				selected.deferredStampedCount,
				selected.deferredFleetCount,
			)
		}
	}
}

// Stamps judged against the writer window: a stale or empty set, or a reply
// that does not answer every handler, vouches for none; otherwise the handlers
// stamped within the window are the stamped ones.
func TestJudgeNetworkClientHandlerStamps(t *testing.T) {
	firstHandlerId, secondHandlerId, thirdHandlerId := server.NewId(), server.NewId(), server.NewId()
	nowMillis := int64(1_800_000_000_000)
	windowMillis := providerLivenessWriterWindow.Milliseconds()
	type judgeCase struct {
		name              string
		handlerIds        []server.Id
		newestMillis      int64
		stampMillis       []int64
		stampedHandlerIds map[server.Id]bool
		stampsKnown       bool
	}
	cases := []judgeCase{
		{
			name:              "a current, a lapsed and a missing stamp",
			handlerIds:        []server.Id{firstHandlerId, secondHandlerId, thirdHandlerId},
			newestMillis:      nowMillis - 1000,
			stampMillis:       []int64{nowMillis - 1000, nowMillis - 2*windowMillis, 0},
			stampedHandlerIds: map[server.Id]bool{firstHandlerId: true},
			stampsKnown:       true,
		},
		{
			name:              "a stamp at the edge of the window",
			handlerIds:        []server.Id{firstHandlerId},
			newestMillis:      nowMillis,
			stampMillis:       []int64{nowMillis - windowMillis},
			stampedHandlerIds: map[server.Id]bool{firstHandlerId: true},
			stampsKnown:       true,
		},
		{
			name:         "a set no process stamped within the window",
			handlerIds:   []server.Id{firstHandlerId},
			newestMillis: nowMillis - windowMillis - 1,
			stampMillis:  []int64{nowMillis - windowMillis - 1},
			stampsKnown:  false,
		},
		{
			name:         "an empty set",
			handlerIds:   []server.Id{firstHandlerId},
			newestMillis: 0,
			stampMillis:  []int64{0},
			stampsKnown:  false,
		},
		{
			name:         "a reply that does not answer every handler",
			handlerIds:   []server.Id{firstHandlerId, secondHandlerId},
			newestMillis: nowMillis,
			stampMillis:  []int64{nowMillis},
			stampsKnown:  false,
		},
	}
	for _, judged := range cases {
		stampedHandlerIds, stampsKnown := judgeNetworkClientHandlerStamps(
			judged.handlerIds,
			nowMillis,
			judged.newestMillis,
			judged.stampMillis,
		)
		if stampsKnown != judged.stampsKnown || !maps.Equal(stampedHandlerIds, judged.stampedHandlerIds) {
			t.Errorf(
				"%s: stamped %v known %t, want %v known %t",
				judged.name,
				stampedHandlerIds,
				stampsKnown,
				judged.stampedHandlerIds,
				judged.stampsKnown,
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
		expireNetworkClientHandlers(ctx, minTime, func(ctx context.Context, handlerIds []server.Id) (map[server.Id]bool, bool) {
			readHandlerIds = slices.Clone(handlerIds)
			// the first process heartbeats while expiry judges it
			if err := HeartbeatNetworkClientHandler(ctx, handlers[0].handlerId); err != nil {
				t.Fatal(err)
			}
			// the set vouches, and stamps neither
			return map[server.Id]bool{}, true
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

		expireNetworkClientHandlers(ctx, server.NowUtc().Add(-2*NetworkClientHandlerHeartbeatTimeout), func(ctx context.Context, handlerIds []server.Id) (map[server.Id]bool, bool) {
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

		counts := func() (expiredCount float64, deferredStampedCount float64, deferredFleetCount float64) {
			return testutil.ToFloat64(networkClientHandlerExpiryCounter.WithLabelValues(networkClientHandlerExpiryExpired)),
				testutil.ToFloat64(networkClientHandlerExpiryCounter.WithLabelValues(networkClientHandlerExpiryDeferredStamped)),
				testutil.ToFloat64(networkClientHandlerExpiryCounter.WithLabelValues(networkClientHandlerExpiryDeferredFleet))
		}
		expiredBefore, deferredStampedBefore, deferredFleetBefore := counts()

		CloseExpiredNetworkClientHandlers(ctx, now.Add(-2*NetworkClientHandlerHeartbeatTimeout))

		expiredAfter, deferredStampedAfter, deferredFleetAfter := counts()
		if expiredAfter-expiredBefore != 1 || deferredStampedAfter-deferredStampedBefore != 2 || deferredFleetAfter-deferredFleetBefore != 0 {
			t.Fatalf(
				"counted %v expired, %v deferred stamped, %v deferred fleet; want 1, 2, 0",
				expiredAfter-expiredBefore,
				deferredStampedAfter-deferredStampedBefore,
				deferredFleetAfter-deferredFleetBefore,
			)
		}
	})
}

// The confirmation read: the row's presence, and a read the database does not
// answer returned as its error, not raised.
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

		// a read whose deadline passed before the database answered
		canceledCtx, cancel := context.WithCancel(ctx)
		cancel()
		exists, err = NetworkClientHandlerExists(canceledCtx, handlerId)
		if exists || !errors.Is(err, context.Canceled) {
			t.Fatalf("a read the database did not answer returned (%t, %v), want (false, context canceled)", exists, err)
		}
	})
}
