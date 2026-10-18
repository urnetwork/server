// The committed reliability window: chunks survive a drain, and the folded
// scores equal the single-statement scores of the same window exactly.
package model

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

type paymentPlanReliabilityScoreRow struct {
	networkId                    server.Id
	countryLocationId            server.Id
	independentReliabilityScore  float64
	independentReliabilityWeight float64
	reliabilityScore             float64
	reliabilityWeight            float64
	minBlockNumber               int64
	maxBlockNumber               int64
}

func readPaymentPlanReliabilityScores(t testing.TB, ctx context.Context, query server.PgCanQuery) (rows []paymentPlanReliabilityScoreRow) {
	t.Helper()
	result, err := query.Query(ctx, `SELECT network_id, country_location_id, independent_reliability_score, independent_reliability_weight,
		reliability_score, reliability_weight, min_block_number, max_block_number
		FROM network_connection_reliability_score ORDER BY network_id, country_location_id`)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var row paymentPlanReliabilityScoreRow
			server.Raise(result.Scan(&row.networkId, &row.countryLocationId, &row.independentReliabilityScore,
				&row.independentReliabilityWeight, &row.reliabilityScore, &row.reliabilityWeight, &row.minBlockNumber, &row.maxBlockNumber))
			rows = append(rows, row)
		}
	})
	return
}

// Ten blocks with shared client addresses (so valid client counts of one, two
// and three), invalid observations, a client without a valid location, two
// countries, and a stale score row from an earlier window.
func paymentPlanReliabilityFixture(t testing.TB, ctx context.Context) (start time.Time, end time.Time) {
	t.Helper()
	firstBlock := reliabilityBlockNumber(server.NowUtc().Add(-7 * 24 * time.Hour))
	start = time.UnixMilli(firstBlock * int64(ReliabilityBlockDuration/time.Millisecond)).UTC()
	end = start.Add(9 * ReliabilityBlockDuration)
	networkA, networkB := server.NewId(), server.NewId()
	countryX, countryY := server.NewId(), server.NewId()
	a1, a2, a3, b1, b2 := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		for _, location := range []struct {
			clientId      server.Id
			networkId     server.Id
			countryId     server.Id
			locationCount int
		}{
			{clientId: a1, networkId: networkA, countryId: countryX, locationCount: 1},
			{clientId: a2, networkId: networkA, countryId: countryY, locationCount: 1},
			{clientId: a3, networkId: networkA, countryId: countryX, locationCount: 1},
			{clientId: b1, networkId: networkB, countryId: countryX, locationCount: 1},
			// not a valid location: its observations never score
			{clientId: b2, networkId: networkB, countryId: countryY, locationCount: 2},
		} {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_location_reliability
				(client_id, network_id, country_location_id, update_block_number, client_address_hash_count, location_count)
				VALUES ($1, $2, $3, $4, 1, $5)`, location.clientId, location.networkId, location.countryId, firstBlock, location.locationCount))
		}
		for blockOffset := int64(0); blockOffset < 10; blockOffset += 1 {
			blockNumber := firstBlock + blockOffset
			type observation struct {
				addressHash []byte
				networkId   server.Id
				clientId    server.Id
				valid       bool
			}
			observations := []observation{
				{addressHash: []byte{0x01}, networkId: networkA, clientId: a1, valid: true},
				{addressHash: []byte{0x02}, networkId: networkB, clientId: b1, valid: true},
				{addressHash: []byte{0x02}, networkId: networkB, clientId: b2, valid: blockOffset%3 != 0},
			}
			if blockOffset%2 == 0 {
				observations = append(observations, observation{addressHash: []byte{0x01}, networkId: networkA, clientId: a2, valid: true})
			}
			if blockOffset%4 == 1 {
				observations = append(observations, observation{addressHash: []byte{0x02}, networkId: networkA, clientId: a3, valid: true})
			}
			if blockOffset == 5 {
				observations = append(observations, observation{addressHash: []byte{0x03}, networkId: networkA, clientId: a3, valid: false})
			}
			for _, o := range observations {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO client_reliability
					(block_number, client_address_hash, network_id, client_id,
					connection_established_count, provide_enabled_count, receive_message_count, valid)
					VALUES ($1, $2, $3, $4, 1, 1, 1, $5)`, blockNumber, o.addressHash, o.networkId, o.clientId, o.valid))
			}
		}
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_connection_reliability_score
			(network_id, country_location_id, independent_reliability_score, independent_reliability_weight,
			reliability_score, reliability_weight, min_block_number, max_block_number)
			VALUES ($1, $2, 7, 7, 7, 7, 0, 1)`, networkB, countryY))
	})
	return
}

func TestPaymentPlanReliabilityChunksMatchSingleStatement(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		start, end := paymentPlanReliabilityFixture(t, ctx)
		minBlockNumber, maxBlockNumber := paymentPlanReliabilityBlockWindow(start, end)
		if maxBlockNumber-minBlockNumber != 10 {
			t.Fatal("fixture window is not ten blocks", minBlockNumber, maxBlockNumber)
		}

		// the scores of one statement, rolled back
		rollback := errors.New("single statement reference rollback")
		var reference []paymentPlanReliabilityScoreRow
		value := server.HandleError(func() {
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				UpdateNetworkReliabilityScoresInTx(tx, ctx, start, end, false)
				reference = readPaymentPlanReliabilityScores(t, ctx, tx)
				panic(rollback)
			}, server.TxReadCommitted, server.OptNoRetry())
		})
		if value != rollback || len(reference) != 3 {
			t.Fatal("reference scores did not cover the fixture", value, reference)
		}

		// four chunks of three blocks; drain right after the second commits
		previousChunkBlockCount := paymentPlanReliabilityChunkBlockCount
		paymentPlanReliabilityChunkBlockCount = 3
		defer func() {
			paymentPlanReliabilityChunkBlockCount = previousChunkBlockCount
			paymentPlanReliabilityChunkCommitted = nil
		}()
		drainCtx, drain := context.WithCancel(ctx)
		defer drain()
		committedChunks := []int64{}
		paymentPlanReliabilityChunkCommitted = func(nextBlockNumber int64) {
			committedChunks = append(committedChunks, nextBlockNumber)
			if len(committedChunks) == 2 {
				drain()
			}
		}
		progress, err := preparePaymentPlanReliabilityWindow(drainCtx, start, end)
		if !progress || !errors.Is(err, context.Canceled) || !slices.Equal(committedChunks, []int64{minBlockNumber + 3, minBlockNumber + 6}) {
			t.Fatal("drain did not stop after two committed chunks", progress, err, committedChunks)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var nextBlockNumber int64
			var complete bool
			var partialCount int
			server.Raise(conn.QueryRow(ctx, `SELECT next_block_number, complete_time IS NOT NULL,
				(SELECT COUNT(*) FROM payment_plan_reliability_partial WHERE min_block_number=$1 AND max_block_number=$2)
				FROM payment_plan_reliability_window WHERE min_block_number=$1 AND max_block_number=$2`,
				minBlockNumber, maxBlockNumber).Scan(&nextBlockNumber, &complete, &partialCount))
			if nextBlockNumber != minBlockNumber+6 || complete || partialCount == 0 {
				t.Fatal("drain lost the committed chunks", nextBlockNumber, complete, partialCount)
			}
			// the partial window has not replaced the previous scores
			if scores := readPaymentPlanReliabilityScores(t, ctx, conn); len(scores) != 1 || scores[0].maxBlockNumber != 1 {
				t.Fatal("unfolded chunks changed the scores", scores)
			}
		})

		// the next call resumes at the cursor and folds once
		paymentPlanReliabilityChunkCommitted = func(nextBlockNumber int64) {
			committedChunks = append(committedChunks, nextBlockNumber)
		}
		progress, err = preparePaymentPlanReliabilityWindow(ctx, start, end)
		if !progress || err != nil || !slices.Equal(committedChunks, []int64{minBlockNumber + 3, minBlockNumber + 6, minBlockNumber + 9, minBlockNumber + 10}) {
			t.Fatal("resumed window did not continue from its cursor", progress, err, committedChunks)
		}
		var chunked []paymentPlanReliabilityScoreRow
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			chunked = readPaymentPlanReliabilityScores(t, ctx, tx)
			shift, ok := paymentPlanReliabilityWindowInTx(ctx, tx, start, end)
			if !ok || shift != 0 {
				t.Fatal("folded window is not current", shift, ok)
			}
			var partialCount int
			server.Raise(tx.QueryRow(ctx, `SELECT COUNT(*) FROM payment_plan_reliability_partial`).Scan(&partialCount))
			if partialCount != 0 {
				t.Fatal("fold retained its partials", partialCount)
			}
		}, server.TxReadCommitted)
		if !slices.Equal(chunked, reference) {
			t.Fatalf("chunked scores differ from the single statement:\nchunked=%+v\nreference=%+v", chunked, reference)
		}

		// a completed window is not aggregated again
		progress, err = preparePaymentPlanReliabilityWindow(ctx, start, end)
		if progress || err != nil || len(committedChunks) != 4 {
			t.Fatal("completed window was prepared again", progress, err, committedChunks)
		}
	})
}

// Another window's fold replaces the scores; the plan transaction then refuses
// the stale window until it is folded again.
func TestPaymentPlanReliabilityWindowRefusesReplacedScores(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		start, end := paymentPlanReliabilityFixture(t, ctx)
		if _, err := preparePaymentPlanReliabilityWindow(ctx, start, end); err != nil {
			t.Fatal(err)
		}
		laterStart, laterEnd := start.Add(3*ReliabilityBlockDuration), end
		if _, err := preparePaymentPlanReliabilityWindow(ctx, laterStart, laterEnd); err != nil {
			t.Fatal(err)
		}
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			if _, ok := paymentPlanReliabilityWindowInTx(ctx, tx, start, end); ok {
				t.Fatal("a replaced window was accepted")
			}
			if _, ok := paymentPlanReliabilityWindowInTx(ctx, tx, laterStart, laterEnd); !ok {
				t.Fatal("the current window was refused")
			}
		}, server.TxReadCommitted)
		if progress, err := preparePaymentPlanReliabilityWindow(ctx, start, end); !progress || err != nil {
			t.Fatal("a replaced window was not folded again", progress, err)
		}
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			if _, ok := paymentPlanReliabilityWindowInTx(ctx, tx, start, end); !ok {
				t.Fatal("the refolded window was refused")
			}
		}, server.TxReadCommitted)
	})
}
