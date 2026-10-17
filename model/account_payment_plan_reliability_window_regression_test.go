// A score window is identified by both bounds, including when a replacement
// excludes a provider's entire history without moving the window end.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// One provider contributes only in the prefix that the next window excludes.
func insertPaymentPlanReliabilityPrefixProvider(t testing.TB, ctx context.Context, observedTime time.Time) server.Id {
	t.Helper()
	networkId, clientId, countryId := server.NewId(), server.NewId(), server.NewId()
	blockNumber := reliabilityBlockNumber(observedTime)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_location_reliability
			(client_id, network_id, country_location_id, update_block_number, client_address_hash_count, location_count)
			VALUES ($1, $2, $3, $4, 1, 1)`, clientId, networkId, countryId, blockNumber))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO client_reliability
			(block_number, client_address_hash, network_id, client_id,
			connection_established_count, provide_enabled_count, receive_message_count, valid)
			VALUES ($1, $2, $3, $4, 1, 1, 1, true)`, blockNumber, []byte{0xfe}, networkId, clientId))
	})
	return networkId
}

// Observe stored output of the real scoring writer, including its deletion.
func requirePaymentPlanReliabilityPrefixScore(t testing.TB, ctx context.Context, tx server.PgTx, networkId server.Id, want bool) {
	t.Helper()
	var present bool
	server.Raise(tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM network_connection_reliability_score WHERE network_id = $1
	)`, networkId).Scan(&present))
	if present != want {
		t.Fatalf("prefix-only provider score present=%t, want %t", present, want)
	}
}

// Replacing a wider window with the same end must remove its prefix-only
// provider, keep retries idle, and let the real planner allocate that window.
func TestPaymentPlanReliabilitySameEndRemovesPrefixOnlyProvider(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		cohort, start, end := paymentReliabilityTransactionFixture(t, ctx)
		earlierStart := start.Add(-3 * ReliabilityBlockDuration)
		prefixNetworkId := insertPaymentPlanReliabilityPrefixProvider(t, ctx, earlierStart)
		if progress, err := preparePaymentPlanReliabilityWindow(ctx, earlierStart, end); !progress || err != nil {
			t.Fatal("the first window did not commit", progress, err)
		}
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			requirePaymentPlanReliabilityPrefixScore(t, ctx, tx, prefixNetworkId, true)
			if _, ok := paymentPlanReliabilityWindowInTx(ctx, tx, earlierStart, end); !ok {
				t.Fatal("the first window is not current")
			}
		}, server.TxReadCommitted)

		previousChunkCommitted := paymentPlanReliabilityChunkCommitted
		defer func() { paymentPlanReliabilityChunkCommitted = previousChunkCommitted }()
		committedChunkCount := 0
		paymentPlanReliabilityChunkCommitted = func(int64) { committedChunkCount += 1 }
		if progress, err := preparePaymentPlanReliabilityWindow(ctx, start, end); !progress || err != nil {
			t.Fatal("the narrower window did not commit", progress, err)
		}
		if committedChunkCount == 0 {
			t.Fatal("the narrower window did not aggregate its input")
		}
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			requirePaymentPlanReliabilityPrefixScore(t, ctx, tx, prefixNetworkId, false)
			if _, ok := paymentPlanReliabilityWindowInTx(ctx, tx, earlierStart, end); ok {
				t.Fatal("the replaced window was accepted")
			}
			if _, ok := paymentPlanReliabilityWindowInTx(ctx, tx, start, end); !ok {
				t.Fatal("the narrower window was refused")
			}
		}, server.TxReadCommitted)
		completeTime := requirePaymentReliabilityWindowCommitted(t, ctx, cohort.network, start, end)
		preparedChunkCount := committedChunkCount
		if progress, err := preparePaymentPlanReliabilityWindow(ctx, start, end); progress || err != nil || committedChunkCount != preparedChunkCount {
			t.Fatal("retry aggregated the completed window again", progress, err, committedChunkCount, preparedChunkCount)
		}

		plan, err := createPaymentPlan(ctx, payoutTransitionRevenueConfig(), false, 0, false, nil)
		if err != nil || plan == nil || plan.SubsidyPayment == nil || len(plan.NetworkPayments) != 1 ||
			!plan.SubsidyPayment.StartTime.Equal(start) || !plan.SubsidyPayment.EndTime.Equal(end) {
			t.Fatal("the real plan could not allocate the narrower window", plan, err)
		}
		if plan.NetworkPayments[cohort.network] == nil || committedChunkCount != preparedChunkCount {
			t.Fatal("the plan lost its provider or recomputed its committed scores", plan, committedChunkCount, preparedChunkCount)
		}
		if retainedTime := requirePaymentReliabilityWindowCommitted(t, ctx, cohort.network, start, end); !retainedTime.Equal(completeTime) {
			t.Fatal("the plan replaced the completed window", completeTime, retainedTime)
		}
	})
}

// The inline writer used by dry runs must remove the same stale prefix row.
func TestNetworkReliabilityScoresSameEndRemovesPrefixOnlyProvider(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		start, end := paymentPlanReliabilityFixture(t, ctx)
		earlierStart := start.Add(-3 * ReliabilityBlockDuration)
		prefixNetworkId := insertPaymentPlanReliabilityPrefixProvider(t, ctx, earlierStart)
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			UpdateNetworkReliabilityScoresInTx(tx, ctx, earlierStart, end, false)
			requirePaymentPlanReliabilityPrefixScore(t, ctx, tx, prefixNetworkId, true)
		}, server.TxReadCommitted)
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			UpdateNetworkReliabilityScoresInTx(tx, ctx, start, end, false)
			requirePaymentPlanReliabilityPrefixScore(t, ctx, tx, prefixNetworkId, false)
			rows := readPaymentPlanReliabilityScores(t, ctx, tx)
			if len(rows) != 3 {
				t.Fatal("the inline writer lost a provider contributing to the narrower window", rows)
			}
			minBlockNumber, maxBlockNumber := paymentPlanReliabilityBlockWindow(start, end)
			for _, row := range rows {
				if row.minBlockNumber != minBlockNumber || row.maxBlockNumber != maxBlockNumber {
					t.Fatal("the inline writer retained scores from another window", row)
				}
			}
		}, server.TxReadCommitted)
	})
}
