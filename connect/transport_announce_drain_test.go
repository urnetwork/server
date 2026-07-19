package connect

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

type testingReliabilityTotals struct {
	excusedNew     int64
	connectionNew  int64
	provideChanged int64
	established    int64
	anyValid       bool
	anyInvalid     bool
	// every row that contains an excused reconnect is valid: the excuse kept
	// the block from invalidating
	excusedRowsAllValid bool
	found               bool
}

// testingReadClientReliabilityTotals sums the client's `client_reliability`
// rows over the block range
func testingReadClientReliabilityTotals(ctx context.Context, clientId server.Id, minBlock int64, maxBlock int64) (totals testingReliabilityTotals) {
	totals.excusedRowsAllValid = true
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				connection_excused_new_count,
				connection_new_count,
				provide_changed_count,
				connection_established_count,
				valid
			FROM client_reliability
			WHERE client_id = $1 AND block_number BETWEEN $2 AND $3
			`,
			clientId,
			minBlock,
			maxBlock,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var excusedNew, connectionNew, provideChanged, established int64
				var valid bool
				server.Raise(result.Scan(&excusedNew, &connectionNew, &provideChanged, &established, &valid))
				totals.excusedNew += excusedNew
				totals.connectionNew += connectionNew
				totals.provideChanged += provideChanged
				totals.established += established
				if valid {
					totals.anyValid = true
				} else {
					totals.anyInvalid = true
				}
				if 0 < excusedNew && !valid {
					totals.excusedRowsAllValid = false
				}
				totals.found = true
			}
		})
	})
	return
}

// testingReliabilityBlockNumber mirrors the model's private block numbering
func testingReliabilityBlockNumber(t time.Time) int64 {
	return t.UTC().UnixMilli() / int64(model.ReliabilityBlockDuration/time.Millisecond)
}

// The announce new-connection branch consumes a drain excuse marker: the
// reconnect is recorded as `connection_excused_new_count` (block stays valid)
// and the mechanical provide re-announce that follows is not counted as a
// provide change. Without a marker the same reconnect and provide change are
// charged as organic (CONNECTDRAIN2.md §3.1).
//
// Requires the test DB env (WARP_ENV=local + postgres/redis/vault), like the
// rest of this package; skipped under -short.
func TestConnectionAnnounceDrainExcuse(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()
		handlerId := server.NewId()

		announceSettings := DefaultConnectionAnnounceSettings()
		announceSettings.SyncConnectionTimeout = 100 * time.Millisecond
		announceSettings.LocationRetryTimeout = 0
		announceSettings.EnableDrainExcuse = true

		blockNumber := func(t time.Time) int64 {
			return t.UTC().UnixMilli() / int64(model.ReliabilityBlockDuration/time.Millisecond)
		}

		// runAnnounce connects a fresh providing client, optionally sets a
		// drain excuse marker, runs the announce for a few sync periods with
		// traffic, and changes the provide keys between syncs
		runAnnounce := func(excuse bool) (clientId server.Id, minBlock int64, maxBlock int64) {
			clientId = server.NewId()
			model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
			model.SetProvide(ctx, clientId, map[model.ProvideMode][]byte{
				model.ProvideModePublic: make([]byte, 32),
			})
			if excuse {
				model.SetDrainExcuse(ctx, clientId, server.NewId(), 1*time.Minute)
			}

			startTime := server.NowUtc()

			announceCtx, announceCancel := context.WithCancel(ctx)
			defer announceCancel()
			announce := NewConnectionAnnounce(
				announceCtx,
				announceCancel,
				networkId,
				clientId,
				"127.0.0.1:20000",
				handlerId,
				// non-zero so the connection starts non-established
				// (the new-connection branch runs on the first sync)
				2*time.Millisecond,
				&TestConfig{},
				announceSettings,
			)

			// traffic for the first sync (the new-connection branch)
			announce.ReceiveMessage(1024)
			select {
			case <-ctx.Done():
				return
			case <-time.After(150 * time.Millisecond):
			}

			// a provide key change between syncs: excused connections suppress
			// it as the mechanical re-announce; organic connections are charged
			model.SetProvide(ctx, clientId, map[model.ProvideMode][]byte{
				model.ProvideModePublic: make([]byte, 32),
			})

			// traffic for the established syncs
			for range 3 {
				announce.ReceiveMessage(1024)
				select {
				case <-ctx.Done():
					return
				case <-time.After(150 * time.Millisecond):
				}
			}
			announceCancel()

			minBlock = blockNumber(startTime) - 1
			maxBlock = blockNumber(server.NowUtc()) + 1
			return
		}

		excusedClientId, minBlock, maxBlock := runAnnounce(true)
		organicClientId, organicMinBlock, organicMaxBlock := runAnnounce(false)

		// drain the redis counters into pg
		model.RollupClientReliabilityStats(ctx, server.NowUtc().Add(3*model.ReliabilityBlockDuration))

		// the excused client: the reconnect is excused, the provide re-announce
		// is suppressed, and no block is invalidated
		totals := testingReadClientReliabilityTotals(ctx, excusedClientId, minBlock, maxBlock)
		connect.AssertEqual(t, true, totals.found)
		connect.AssertEqual(t, int64(1), totals.excusedNew)
		connect.AssertEqual(t, int64(0), totals.connectionNew)
		connect.AssertEqual(t, int64(0), totals.provideChanged)
		connect.AssertEqual(t, true, 1 <= totals.established)
		connect.AssertEqual(t, false, totals.anyInvalid)
		connect.AssertEqual(t, true, totals.anyValid)

		// the marker was consumed exactly once
		connect.AssertEqual(t, true, model.TakeDrainExcuse(ctx, excusedClientId) == nil)

		// the organic client: the reconnect is charged as new, and the provide
		// change invalidates its block
		totals = testingReadClientReliabilityTotals(ctx, organicClientId, organicMinBlock, organicMaxBlock)
		connect.AssertEqual(t, true, totals.found)
		connect.AssertEqual(t, int64(0), totals.excusedNew)
		connect.AssertEqual(t, int64(1), totals.connectionNew)
		connect.AssertEqual(t, int64(1), totals.provideChanged)
		connect.AssertEqual(t, true, totals.anyInvalid)
	})
}
