package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The physical row version (xmin) and window end of each lookback's score row
// of a client.
func testingClientScoreVersions(ctx context.Context, clientId server.Id) map[int]string {
	versions := map[int]string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT lookback_index, xmin::text||':'||max_block_number::text
			FROM client_connection_reliability_score WHERE client_id = $1`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var lookbackIndex int
				var version string
				server.Raise(result.Scan(&lookbackIndex, &version))
				versions[lookbackIndex] = version
			}
		})
	})
	return versions
}

// A pass over the same window recomputes the same scores, so it rewrites no
// score row, while it still deletes a row it did not refresh. A pass over the
// next window moves every row's window, which the stale-row delete and the
// freshness signal read, so it rewrites them all.
func TestUpdateClientReliabilityScoresRewritesOnlyChangedRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		location := &Location{
			LocationType: LocationTypeCity,
			City:         "score_write_city",
			Region:       "score_write_region",
			Country:      "Score Write Country",
			CountryCode:  "sw",
		}
		CreateLocation(ctx, location)

		networkId := server.NewId()
		clientId := server.NewId()
		clientAddressHash := testingConnectClientWithLocation(ctx, t, networkId, clientId, "192.0.2.51:20001", location)

		startTime := server.NowUtc().UTC().Truncate(ReliabilityBlockDuration).Add(-24 * time.Hour)
		blockTime := func(i int) time.Time {
			return startTime.Add(time.Duration(i) * ReliabilityBlockDuration)
		}
		const blockCount = 72
		for i := 0; i < blockCount; i++ {
			AddClientReliabilityStats(ctx, networkId, clientId, clientAddressHash, blockTime(i), &ClientReliabilityStats{
				ConnectionEstablishedCount: 1,
				ProvideEnabledCount:        1,
				ReceiveMessageCount:        1,
			})
		}
		UpdateClientLocationReliabilities(ctx, startTime, blockTime(blockCount))
		startBlock := reliabilityBlockNumber(startTime)
		server.Tx(ctx, func(tx server.PgTx) {
			server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
				for i := 0; i < blockCount; i++ {
					batch.Queue(
						`INSERT INTO client_reliability_block (block_number, client_count, valid_client_count) VALUES ($1, 1, 1)`,
						startBlock+int64(i),
					)
				}
			})
		})

		UpdateClientReliabilityScores(ctx, blockTime(60), false)
		published := testingClientScoreVersions(ctx, clientId)
		if len(published) != len(ClientLookbacks) {
			t.Fatalf("published %d lookbacks, want %d: %v", len(published), len(ClientLookbacks), published)
		}

		// a score of a client the next pass does not refresh
		staleClientId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO client_connection_reliability_score (
					client_id, lookback_index, independent_reliability_score, reliability_score, reliability_weight, max_block_number
				) VALUES ($1, 1, 1, 1, 1, $2)`,
				staleClientId,
				startBlock,
			))
		})

		UpdateClientReliabilityScores(ctx, blockTime(60), false)
		for lookbackIndex, version := range testingClientScoreVersions(ctx, clientId) {
			if version != published[lookbackIndex] {
				t.Fatalf("lookback %d: the same window rewrote the score: %s -> %s", lookbackIndex, published[lookbackIndex], version)
			}
		}
		if stale := testingClientScoreVersions(ctx, staleClientId); len(stale) != 0 {
			t.Fatalf("the stale score survived the pass: %v", stale)
		}

		UpdateClientReliabilityScores(ctx, blockTime(61), false)
		moved := testingClientScoreVersions(ctx, clientId)
		if len(moved) != len(ClientLookbacks) {
			t.Fatalf("the next window published %d lookbacks, want %d", len(moved), len(ClientLookbacks))
		}
		for lookbackIndex, version := range moved {
			if version == published[lookbackIndex] {
				t.Fatalf("lookback %d: the next window did not rewrite the score: %s", lookbackIndex, version)
			}
		}
	})
}
