package model

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Main's earning boundary: sn.yml cutoff_utc.
var operatorBlockPointsMainCutoff = time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)

// Block 15 is [2026-10-04, 2026-10-11): its traffic past Main's cutoff is
// [10-06, 10-11). Blocks 16 and 17 are whole. The weeks a block's traffic
// earns in follow the operator block calendar, not the time points post.
func TestOperatorBlockPointsWindowFollowsTheOperatorBlocks(t *testing.T) {
	for _, test := range []struct {
		block      int64
		start, end time.Time
		ok         bool
	}{
		{14, time.Time{}, time.Time{}, false},
		{15, operatorBlockPointsMainCutoff, time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC), true},
		{16, time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC), true},
		{17, time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC), true},
	} {
		start, end, ok := OperatorBlockPointsWindow(test.block, operatorBlockPointsMainCutoff)
		if ok != test.ok || (ok && (!start.Equal(test.start) || !end.Equal(test.end))) {
			t.Fatalf("block %d window = [%s, %s) %t, want [%s, %s) %t", test.block, start, end, ok, test.start, test.end, test.ok)
		}
		if ok && (SubnetBlockNumber(start) != int(test.block) || SubnetBlockNumber(end.Add(-time.Microsecond)) != int(test.block)) {
			t.Fatalf("block %d window leaves the block", test.block)
		}
	}
	if _, _, ok := OperatorBlockPointsWindow(0, operatorBlockPointsMainCutoff); ok {
		t.Fatal("block 0 has a window")
	}
}

// A block is due once its last settlements committed after it closed.
func TestOperatorBlockPointsDueBlocksWaitForTheSettleTime(t *testing.T) {
	for _, test := range []struct {
		now  time.Time
		want []int64
	}{
		{time.Date(2026, 10, 11, 1, 59, 0, 0, time.UTC), []int64{}},
		{time.Date(2026, 10, 11, 2, 0, 0, 0, time.UTC), []int64{15}},
		{time.Date(2026, 10, 28, 23, 0, 0, 0, time.UTC), []int64{15, 16, 17}},
		{time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC), []int64{15, 16, 17}},
		{time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), []int64{15, 16, 17, 18}},
	} {
		if due := OperatorBlockPointsDueBlocks(operatorBlockPointsMainCutoff, test.now); !slices.Equal(due, test.want) {
			t.Fatalf("due blocks at %s = %v, want %v", test.now, due, test.want)
		}
	}
	// a cutoff on a block's open time makes that block whole
	if due := OperatorBlockPointsDueBlocks(time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)); !slices.Equal(due, []int64{15}) {
		t.Fatalf("due blocks from a block boundary = %v", due)
	}
}

// The daily task never awards a block that completed before it was first
// scheduled: those wait for the backfill and its explicit go. After any award
// it catches up every later block.
func TestOperatorBlockPointsAutomaticBlocks(t *testing.T) {
	due := []int64{15, 16, 17, 18, 19}
	for _, test := range []struct {
		name      string
		awarded   []int64
		fromBlock int64
		want      []int64
	}{
		{"first run after a mid-week deploy", nil, 20, []int64{}},
		{"the first block that completed after the deploy", nil, 19, []int64{19}},
		{"several blocks completed after the deploy", nil, 18, []int64{18, 19}},
		{"a run a payout queued without a schedule", nil, 0, []int64{19}},
		{"catch up after the backfill", []int64{15, 16, 17}, 20, []int64{18, 19}},
		{"catch up after a missed week", []int64{18}, 18, []int64{19}},
		{"a gap before the last award is the backfill's", []int64{15, 18}, 0, []int64{19}},
		{"nothing left", []int64{15, 16, 17, 18, 19}, 0, []int64{}},
	} {
		awarded := map[int64]bool{}
		for _, block := range test.awarded {
			awarded[block] = true
		}
		if got := operatorBlockPointsAutomaticBlocks(due, awarded, test.fromBlock); !slices.Equal(got, test.want) {
			t.Fatalf("%s: %v, want %v", test.name, got, test.want)
		}
	}
	if got := operatorBlockPointsAutomaticBlocks([]int64{}, map[int64]bool{}, 0); len(got) != 0 {
		t.Fatal("no due block awarded", got)
	}
}

func TestOperatorBlockPointsPlanIdIsTheBlocks(t *testing.T) {
	if OperatorBlockPointsPlanId(16) != OperatorBlockPointsPlanId(16) {
		t.Fatal("plan id is not stable")
	}
	if OperatorBlockPointsPlanId(16) == OperatorBlockPointsPlanId(17) {
		t.Fatal("blocks share a plan id")
	}
}

func operatorBlockPointsTestConfig() *SubsidyConfig {
	return &SubsidyConfig{
		Days:                           30,
		MinDaysFraction:                0.1,
		UsdPerActiveUser:               0.5,
		SubscriptionNetRevenueFraction: 0.5,
		MinPayoutUsd:                   10,
		ActiveUserByteCountThresholdHumanReadable: "1GiB",
		ReferralParentPayoutFraction:              0.25,
		ReferralChildPayoutFraction:               0.125,
		AccountPointsPerPayout:                    1_000_000,
		ReliabilityPointsPerPayout:                500_000,
		ReliabilitySubsidyPerPayoutUsd:            250,
		MinWalletPayoutUsd:                        5,
		WalletPayoutTimeoutHumanReadable:          "720h",
		SeekerHolderMultiplier:                    2,
	}
}

// A block's points are a plan's points for the block's traffic: the subsidy
// split per paying network over its providers by bytes, plus net revenue and
// the reliability subsidy, weighs the payout points; reliability points
// follow reliability weight; the seeker multiplier and the referral bonuses
// apply to them. Every expected value below is computed independently, by the
// formulas of planSubsidyPayments, calculateReliabilityPayoutInTx and
// applyPayoutPoints.
func TestPlanOperatorBlockPointsFollowsThePlanRules(t *testing.T) {
	config := operatorBlockPointsTestConfig()
	gib := ByteCount(1024 * 1024 * 1024)
	start := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	end := start.Add(SubnetBlockDuration)

	payerU, payerV := server.NewId(), server.NewId()
	providerA, providerB := server.NewId(), server.NewId()
	// a network with reliability and no traffic in the block
	providerC := server.NewId()
	parentD, childE, grandchildG, childH := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	balanceU, balanceV := server.NewId(), server.NewId()

	traffic := newOperatorBlockTraffic()
	sweepTime := start.Add(time.Hour)
	paidSweepTime := start.Add(2 * time.Hour)
	balanceStart := start.Add(-15 * 24 * time.Hour)
	balanceEnd := balanceStart.Add(30 * 24 * time.Hour)
	revenue := UsdToNanoCents(12)
	for _, row := range []*operatorBlockSweepRow{
		{payeeNetworkId: providerA, balanceId: balanceU, payerNetworkId: &payerU, paid: true, balanceStartTime: &balanceStart, balanceEndTime: &balanceEnd, subsidyNetRevenue: revenue, payoutByteCount: 3 * gib, netRevenue: UsdToNanoCents(0.1), minSweepTime: sweepTime, minPaidSweepTime: &paidSweepTime, sweepCount: 3},
		{payeeNetworkId: providerB, balanceId: balanceU, payerNetworkId: &payerU, paid: true, balanceStartTime: &balanceStart, balanceEndTime: &balanceEnd, subsidyNetRevenue: revenue, payoutByteCount: gib, minSweepTime: sweepTime, minPaidSweepTime: &paidSweepTime, sweepCount: 1},
		{payeeNetworkId: providerB, balanceId: balanceV, payerNetworkId: &payerV, payoutByteCount: gib / 2, minSweepTime: sweepTime, minPaidSweepTime: &paidSweepTime, sweepCount: 1},
	} {
		traffic.add(row)
	}
	reliability := &operatorBlockReliability{
		effectiveBlockCount: 10080,
		// dyadic weights sum exactly in any order
		networkWeights: map[server.Id]float64{
			providerA: 0.25,
			providerB: 0.5,
			providerC: 0.25,
		},
	}
	award, err := planOperatorBlockPoints(
		&operatorBlockPointsInputs{
			subsidyConfig: config,
			block:         16,
			cutoff:        operatorBlockPointsMainCutoff,
			policySha256:  "policy",
			windowStart:   start,
			windowEnd:     end,
			seekerHolders: map[server.Id]bool{providerA: true},
			networkReferrals: map[server.Id][]server.Id{
				parentD:   {providerA},
				providerA: {childE},
				childE:    {grandchildG},
				providerB: {childH},
			},
		},
		traffic,
		reliability,
	)
	if err != nil {
		t.Fatal(err)
	}

	// planSubsidyPayments over the block's window
	scale := float64(7) / float64(30)
	if award.SubsidyScale != scale {
		t.Fatalf("subsidy scale %v, want %v", award.SubsidyScale, scale)
	}
	if award.TrafficUserCount != 2 || award.ActiveUserCount != 1 || award.PaidUserCount != 1 {
		t.Fatalf("users traffic=%d active=%d paid=%d", award.TrafficUserCount, award.ActiveUserCount, award.PaidUserCount)
	}
	// the balance's revenue pro-rated to the window, per subsidy period
	windowRevenue := float64(revenue) * float64(7*24*time.Hour) / float64(30*24*time.Hour)
	netRevenue := NanoCents(windowRevenue * float64(30*24*time.Hour) / float64(7*24*time.Hour))
	if award.NetRevenue != netRevenue {
		t.Fatalf("net revenue %d, want %d", award.NetRevenue, netRevenue)
	}
	subsidyNetPayoutUsd := scale * max(config.MinPayoutUsd, config.UsdPerActiveUser*1, config.SubscriptionNetRevenueFraction*NanoCentsToUsd(netRevenue))
	if award.SubsidyNetPayoutUsd != subsidyNetPayoutUsd {
		t.Fatalf("subsidy %v, want %v", award.SubsidyNetPayoutUsd, subsidyNetPayoutUsd)
	}
	subsidyA := UsdToNanoCents(float64(3*gib) / float64(4*gib) * subsidyNetPayoutUsd / 2)
	subsidyB := UsdToNanoCents(float64(gib)/float64(4*gib)*subsidyNetPayoutUsd/2) + UsdToNanoCents(1*subsidyNetPayoutUsd/2)

	// calculateReliabilityPayoutInTx: shares of the total weight, including
	// the weight of a network that earns nothing in the block
	reliabilityPointsPerPayout := PointsToNanoPoints(float64(config.ReliabilityPointsPerPayout) * scale)
	reliabilitySubsidyPerPayout := NanoCents(float64(UsdToNanoCents(float64(config.ReliabilitySubsidyPerPayoutUsd))) * scale)
	reliabilityPointsA := NanoPoints(float64(reliabilityPointsPerPayout) * (0.25 / 1.0))
	reliabilityPointsB := NanoPoints(float64(reliabilityPointsPerPayout) * (0.5 / 1.0))
	reliabilityUsdcA := NanoCents(float64(reliabilitySubsidyPerPayout) * (0.25 / 1.0))
	reliabilityUsdcB := NanoCents(float64(reliabilitySubsidyPerPayout) * (0.5 / 1.0))

	payoutA := UsdToNanoCents(0.1) + subsidyA + reliabilityUsdcA
	payoutB := subsidyB + reliabilityUsdcB
	if award.TotalComputedPayout != payoutA+payoutB {
		t.Fatalf("computed payout %d, want %d", award.TotalComputedPayout, payoutA+payoutB)
	}
	payments := map[server.Id]*OperatorBlockPayment{}
	for _, payment := range award.Payments {
		payments[payment.NetworkId] = payment
	}
	if len(payments) != 2 || payments[providerA].ComputedPayout != payoutA || payments[providerB].ComputedPayout != payoutB ||
		payments[providerA].PayoutByteCount != 3*gib || payments[providerB].PayoutByteCount != gib+gib/2 ||
		payments[providerA].SubsidyPayout != subsidyA || payments[providerB].ReliabilitySubsidy != reliabilityUsdcB ||
		!payments[providerA].MinSweepTime.Equal(sweepTime) || !payments[providerA].MinPaidSweepTime.Equal(paidSweepTime) {
		t.Fatalf("payments %+v %+v", payments[providerA], payments[providerB])
	}

	// applyPayoutPoints
	totalPoints := float64(config.AccountPointsPerPayout) * scale
	totalPayout := payoutA + payoutB
	pointsA := PointsToNanoPoints(totalPoints * (float64(payoutA) / float64(totalPayout)))
	pointsB := PointsToNanoPoints(totalPoints * (float64(payoutB) / float64(totalPayout)))
	seekerA := NanoPoints(float64(pointsA)*2) - pointsA + reliabilityPointsA
	multipliedA := NanoPoints(float64(pointsA) * 2)
	type row struct {
		network, payment server.Id
		event            AccountPointEvent
		linked           server.Id
	}
	want := map[row]NanoPoints{
		{providerA, providerA, AccountPointEventReliability, server.Id{}}:      reliabilityPointsA,
		{providerB, providerB, AccountPointEventReliability, server.Id{}}:      reliabilityPointsB,
		{providerA, providerA, AccountPointEventPayout, server.Id{}}:           pointsA,
		{providerA, providerA, AccountPointEventPayoutMultiplier, server.Id{}}: seekerA,
		{parentD, providerA, AccountPointEventPayoutLinkedAccount, providerA}:  NanoPoints(float64(multipliedA) * 0.25),
		{childE, providerA, AccountPointEventPayoutLinkedAccount, providerA}:   NanoPoints(float64(multipliedA) * 0.125),
		{grandchildG, providerA, AccountPointEventPayoutLinkedAccount, childE}: NanoPoints(float64(multipliedA) * 0.125 * 0.5),
		{providerB, providerB, AccountPointEventPayout, server.Id{}}:           pointsB,
		{childH, providerB, AccountPointEventPayoutLinkedAccount, providerB}:   NanoPoints(float64(pointsB) * 0.125),
	}
	got := map[row]NanoPoints{}
	for _, point := range award.Points {
		key := row{point.NetworkId, point.PaymentNetworkId, AccountPointEvent(point.Event), server.Id{}}
		if point.LinkedNetworkId != nil {
			key.linked = *point.LinkedNetworkId
		}
		if _, ok := got[key]; ok {
			t.Fatalf("duplicate point %+v", key)
		}
		got[key] = point.PointValue
	}
	if len(got) != len(want) {
		t.Fatalf("%d points, want %d: %v", len(got), len(want), got)
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("point %+v = %d, want %d", key, got[key], value)
		}
	}
	if award.PayoutNanoPoints != PointsToNanoPoints(totalPoints) || pointsA+pointsB < PointsToNanoPoints(totalPoints)-1 {
		t.Fatalf("payout points %d, rows %d", award.PayoutNanoPoints, pointsA+pointsB)
	}
	if award.PaymentPlanId != OperatorBlockPointsPlanId(16) {
		t.Fatal("award plan id is not the block's")
	}
}

// No traffic, no points; a network without traffic in the block gets no
// reliability points, as a plan gives none to a network without a payment.
func TestPlanOperatorBlockPointsWithoutTraffic(t *testing.T) {
	start := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	award, err := planOperatorBlockPoints(
		&operatorBlockPointsInputs{
			subsidyConfig: operatorBlockPointsTestConfig(),
			block:         16,
			cutoff:        operatorBlockPointsMainCutoff,
			windowStart:   start,
			windowEnd:     start.Add(SubnetBlockDuration),
		},
		newOperatorBlockTraffic(),
		&operatorBlockReliability{networkWeights: map[server.Id]float64{server.NewId(): 1}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(award.Payments) != 0 || len(award.Points) != 0 || award.TotalComputedPayout != 0 {
		t.Fatalf("award without traffic %+v", award)
	}
}

// The digest binds every value a commit writes, survives the dry run's file,
// and changes with any point.
func TestOperatorBlockPointsDigestBindsTheAwards(t *testing.T) {
	start := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	payer, provider := server.NewId(), server.NewId()
	traffic := newOperatorBlockTraffic()
	paidSweepTime := start.Add(time.Hour)
	traffic.add(&operatorBlockSweepRow{payeeNetworkId: provider, balanceId: server.NewId(), payerNetworkId: &payer, payoutByteCount: 1024, minSweepTime: paidSweepTime, minPaidSweepTime: &paidSweepTime, sweepCount: 1})
	award, err := planOperatorBlockPoints(
		&operatorBlockPointsInputs{
			subsidyConfig: operatorBlockPointsTestConfig(),
			block:         16,
			cutoff:        operatorBlockPointsMainCutoff,
			policySha256:  "policy",
			windowStart:   start,
			windowEnd:     start.Add(SubnetBlockDuration),
		},
		traffic,
		&operatorBlockReliability{networkWeights: map[server.Id]float64{}},
	)
	if err != nil || len(award.Points) == 0 {
		t.Fatal(err, award)
	}
	digest := OperatorBlockPointsDigest([]*OperatorBlockPoints{award})

	data, err := json.Marshal([]*OperatorBlockPoints{award})
	if err != nil {
		t.Fatal(err)
	}
	var parsed []*OperatorBlockPoints
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	if OperatorBlockPointsDigest(parsed) != digest || parsed[0].Digest() != award.Digest() {
		t.Fatal("the dry run's file changed the digest")
	}
	parsed[0].Points[0].PointValue += 1
	if OperatorBlockPointsDigest(parsed) == digest {
		t.Fatal("digest does not bind the points")
	}
}
