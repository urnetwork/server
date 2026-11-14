package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The dry run's report shows each block's bytes and every network's bytes and
// share of them beside its points, and no revenue, payout in dollars or
// subsidy: since the owner's rule of 2026-11-13 the payout points follow the
// bytes alone, and an award reads no revenue to show.
func TestOperatorBlockPointsBackfillTextShowsBytesNotRevenue(t *testing.T) {
	gib := model.ByteCount(1024 * 1024 * 1024)
	start := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	providerA, providerB := server.NewId(), server.NewId()
	award := &model.OperatorBlockPoints{
		Schema:           model.OperatorBlockPointsSchema,
		Block:            20,
		PaymentPlanId:    model.OperatorBlockPointsPlanId(20),
		Cutoff:           start,
		WindowStart:      start,
		WindowEnd:        start.Add(model.SubnetBlockDuration),
		TimeScale:        float64(7) / float64(30),
		ByteCount:        4 * gib,
		SweepCount:       3,
		PayoutNanoPoints: 233_333_333_333,
		PointsPerPayout:  1_000_000,
		Payments: []*model.OperatorBlockPayment{
			{NetworkId: providerA, PayoutByteCount: 3 * gib, MinSweepTime: start.Add(time.Hour)},
			{NetworkId: providerB, PayoutByteCount: gib, MinSweepTime: start.Add(2 * time.Hour)},
		},
		Points: []*model.OperatorBlockPoint{
			{NetworkId: providerA, Event: string(model.AccountPointEventPayout), PointValue: 175_000_000_000, PaymentNetworkId: providerA},
			{NetworkId: providerB, Event: string(model.AccountPointEventPayout), PointValue: 58_333_333_333, PaymentNetworkId: providerB},
		},
	}
	legacyEpochEnd := start.Add(-30 * 24 * time.Hour)
	backfill := &OperatorBlockPointsBackfill{
		Schema:         OperatorBlockPointsBackfillSchema,
		Cutoff:         start,
		LegacyEpochEnd: &legacyEpochEnd,
		DueBlocks:      []int64{20},
		AwardedBlocks:  []int64{},
		Awards:         []*model.OperatorBlockPoints{award},
	}
	backfill.Digest = model.OperatorBlockPointsDigest(backfill.Awards)
	text := backfill.Text()
	for _, want := range []string{
		model.OperatorBlockPointsSchema,
		"3 sweeps, 2 providers, 4gib served, paid or unpaid",
		"payout points 233333.333333 (1000000 points per payout, time scale 0.233333), shared by bytes",
		"network_id\tpayout_bytes\tbyte_share\tpayout\t",
		providerA.String() + "\t3221225472\t0.750000000\t175000.000000\t",
		providerB.String() + "\t1073741824\t0.250000000\t58333.333333\t",
		backfill.Digest,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the report lacks %q:\n%s", want, text)
		}
	}
	// the block's section, from its header to the totals
	blockText := text[strings.Index(text, "\nblock 20 "):strings.Index(text, "\ntotal: ")]
	for _, unwanted := range []string{"revenue", "subsidy", "usd", "$"} {
		if strings.Contains(strings.ToLower(blockText), unwanted) {
			t.Fatalf("the block's report shows %q:\n%s", unwanted, blockText)
		}
	}
}
