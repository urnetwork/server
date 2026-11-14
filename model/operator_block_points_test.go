package model

import (
	"crypto/md5"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
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
// it catches up every later block. A block recorded expired is left out, and
// moves nothing else: without expiry records the blocks are those before
// expiry was recorded.
func TestOperatorBlockPointsAutomaticBlocks(t *testing.T) {
	due := []int64{15, 16, 17, 18, 19}
	for _, test := range []struct {
		name      string
		awarded   []int64
		expired   []int64
		fromBlock int64
		want      []int64
	}{
		{"first run after a mid-week deploy", nil, nil, 20, []int64{}},
		{"the first block that completed after the deploy", nil, nil, 19, []int64{19}},
		{"several blocks completed after the deploy", nil, nil, 18, []int64{18, 19}},
		{"a run a payout queued without a schedule", nil, nil, 0, []int64{19}},
		{"catch up after the backfill", []int64{15, 16, 17}, nil, 20, []int64{18, 19}},
		{"catch up after a missed week", []int64{18}, nil, 18, []int64{19}},
		{"a gap before the last award is the backfill's", []int64{15, 18}, nil, 0, []int64{19}},
		{"nothing left", []int64{15, 16, 17, 18, 19}, nil, 0, []int64{}},
		{"an expired block after the last award is left out", []int64{17}, []int64{18}, 0, []int64{19}},
		{"expired blocks leave the last award where it was", []int64{15}, []int64{16, 17}, 0, []int64{18, 19}},
		{"an expired block before the first award", nil, []int64{18}, 18, []int64{19}},
		{"the latest due block expired without a schedule", nil, []int64{19}, 0, []int64{}},
		{"every block after the last award expired", []int64{16}, []int64{17, 18, 19}, 0, []int64{}},
	} {
		awarded := map[int64]bool{}
		for _, block := range test.awarded {
			awarded[block] = true
		}
		expired := map[int64]bool{}
		for _, block := range test.expired {
			expired[block] = true
		}
		if got := operatorBlockPointsAutomaticBlocks(due, awarded, expired, test.fromBlock); !slices.Equal(got, test.want) {
			t.Fatalf("%s: %v, want %v", test.name, got, test.want)
		}
	}
	if got := operatorBlockPointsAutomaticBlocks([]int64{}, map[int64]bool{}, map[int64]bool{}, 0); len(got) != 0 {
		t.Fatal("no due block awarded", got)
	}
}

// A block expires when its window's first reliability block leaves
// client_reliability retention: ClientReliabilityRetention after the window
// opens, at reliability block granularity. A whole block is due its
// settle time after it closes and expires about three days later; a block
// whose window starts at a cutoff inside it expires that much later. A block
// with no traffic past the cutoff never expires. The expiry carries the
// window, its first reliability block and the first one retention keeps.
func TestOperatorBlockPointsExpiryIsTheRetentionDeadline(t *testing.T) {
	block := int64(20)
	start := operatorBlockStart(block)
	due := operatorBlockStart(block + 1).Add(OperatorBlockPointsSettleTime)
	deadline := start.Add(ClientReliabilityRetention)
	if !due.Before(deadline.Add(-ReliabilityBlockDuration)) {
		t.Fatalf("block %d is due at %s, not before its deadline %s", block, due, deadline)
	}
	for _, test := range []struct {
		cutoff  time.Time
		now     time.Time
		expired bool
	}{
		{operatorBlockPointsMainCutoff, due, false},
		{operatorBlockPointsMainCutoff, deadline.Add(-ReliabilityBlockDuration), false},
		// the first reliability block retention keeps passes the window's
		// first one a block's length after the deadline
		{operatorBlockPointsMainCutoff, deadline.Add(ReliabilityBlockDuration - time.Nanosecond), false},
		{operatorBlockPointsMainCutoff, deadline.Add(ReliabilityBlockDuration), true},
		{operatorBlockPointsMainCutoff, deadline.Add(30 * 24 * time.Hour), true},
		// a cutoff a day into the block moves its deadline a day on
		{start.Add(24 * time.Hour), deadline.Add(ReliabilityBlockDuration), false},
		{start.Add(24 * time.Hour), deadline.Add(24*time.Hour + ReliabilityBlockDuration), true},
		// a block that ends at the cutoff has no window to expire
		{start.Add(SubnetBlockDuration), deadline.Add(30 * 24 * time.Hour), false},
	} {
		expiry, expired := OperatorBlockPointsExpiryAt(block, test.cutoff, test.now)
		if expired != test.expired || (expiry != nil) != expired {
			t.Fatalf("block %d with cutoff %s at %s: expired %t, want %t", block, test.cutoff, test.now, expired, test.expired)
		}
		if !expired {
			continue
		}
		windowStart, windowEnd, _ := OperatorBlockPointsWindow(block, test.cutoff)
		minBlockNumber, _ := operatorBlockReliabilityBlockWindow(windowStart, windowEnd)
		if expiry.Block != block || !expiry.WindowStart.Equal(windowStart) || !expiry.WindowEnd.Equal(windowEnd) ||
			expiry.ReliabilityMinBlockNumber != minBlockNumber || expiry.RetentionMinBlockNumber != clientReliabilityRetentionCutoffBlock(test.now) ||
			expiry.RetentionMinBlockNumber <= expiry.ReliabilityMinBlockNumber || !expiry.ExpireTime.Equal(test.now) {
			t.Fatalf("block %d with cutoff %s at %s: expiry %+v", block, test.cutoff, test.now, expiry)
		}
	}
}

// A block's plan id never moves with the award's schema: an award, the
// leaderboard and the epoch history find a block's award by it, so a schema
// that moved the ids would award every awarded block again.
func TestOperatorBlockPointsPlanIdIsTheBlocks(t *testing.T) {
	if OperatorBlockPointsPlanId(16) != OperatorBlockPointsPlanId(16) {
		t.Fatal("plan id is not stable")
	}
	if OperatorBlockPointsPlanId(16) == OperatorBlockPointsPlanId(17) {
		t.Fatal("blocks share a plan id")
	}
	// the ids the v1 awards were written under
	for _, block := range []int64{15, 16, 19, 20, 21} {
		sum := md5.Sum([]byte("urnetwork-operator-block-points-v1/" + strconv.FormatInt(block, 10)))
		if OperatorBlockPointsPlanId(block) != server.RequireIdFromBytes(sum[:]) {
			t.Fatalf("block %d plan id is not the one its award was written under", block)
		}
	}
}

func operatorBlockPointsTestConfig() *SubsidyConfig {
	return &SubsidyConfig{
		Days:                         30,
		ReferralParentPayoutFraction: 0.25,
		ReferralChildPayoutFraction:  0.125,
		AccountPointsPerPayout:       1_000_000,
		ReliabilityPointsPerPayout:   500_000,
		SeekerHolderMultiplier:       2,
	}
}

// n network ids in ascending order, so a test knows the id order the award
// visits them in.
func operatorBlockPointsTestIds(n int) []server.Id {
	ids := make([]server.Id, n)
	for i := range ids {
		ids[i] = server.NewId()
	}
	slices.SortFunc(ids, server.Id.Cmp)
	return ids
}

// The payout points are shared exactly: the shares sum to the points, each is
// within one nano point of its exact share, the nano points the floors leave
// go to the largest remainders (a tie to the lower id), and only the bytes'
// ratio counts. Every expected share is worked by hand; the third case's
// products pass 64 bits.
func TestOperatorBlockPayoutPointSharesAreExact(t *testing.T) {
	ids := operatorBlockPointsTestIds(3)
	a, b, c := ids[0], ids[1], ids[2]
	gib := ByteCount(1024 * 1024 * 1024)
	for i, test := range []struct {
		total NanoPoints
		bytes map[server.Id]ByteCount
		want  map[server.Id]NanoPoints
	}{
		// 3 each, and the leftover to the lowest id of the tie
		{10, map[server.Id]ByteCount{a: 1, b: 1, c: 1}, map[server.Id]NanoPoints{a: 4, b: 3, c: 3}},
		// a block's points (1,000,000 x 7/30): b's remainder 2/3 beats a's 1/3
		{233_333_333_333, map[server.Id]ByteCount{a: 2, b: 1}, map[server.Id]NanoPoints{a: 155_555_555_555, b: 77_777_777_778}},
		{233_333_333_333, map[server.Id]ByteCount{a: 2 * gib, b: gib}, map[server.Id]NanoPoints{a: 155_555_555_555, b: 77_777_777_778}},
		// a quarter and three quarters of a petabyte-scale block
		{233_333_333_333, map[server.Id]ByteCount{a: 1_000_000_000_000_000, b: 3_000_000_000_000_000}, map[server.Id]NanoPoints{a: 58_333_333_333, b: 175_000_000_000}},
		{7, map[server.Id]ByteCount{c: 5}, map[server.Id]NanoPoints{c: 7}},
		{0, map[server.Id]ByteCount{a: 5}, map[server.Id]NanoPoints{}},
		{7, map[server.Id]ByteCount{}, map[server.Id]NanoPoints{}},
	} {
		got := operatorBlockPayoutPointShares(test.total, test.bytes)
		if !maps.Equal(got, test.want) {
			t.Fatalf("case %d: shares %v, want %v", i, got, test.want)
		}
	}
}

// The award's points rule: account_points_per_payout scaled to the window is
// shared by bytes alone; reliability points follow reliability weight, for
// the networks with bytes; the seeker multiplier and the referral bonuses
// apply to the payout points. The traffic it reads carries no revenue and no
// payer. The payout shares are worked by hand: the window's points are
// 233,333,333,333 nano points (1,000,000 x 7/30), of which A's 2 GiB take
// 155,555,555,555 and B's 1 GiB the rest; the bonuses follow the plan's
// formulas.
func TestPlanOperatorBlockPointsSharesPayoutPointsByBytes(t *testing.T) {
	config := operatorBlockPointsTestConfig()
	gib := ByteCount(1024 * 1024 * 1024)
	start := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	end := start.Add(SubnetBlockDuration)

	providerA, providerB := server.NewId(), server.NewId()
	// a network with reliability and no traffic in the block
	providerC := server.NewId()
	// a network whose sweeps carry no bytes
	providerZ := server.NewId()
	parentD, childE, grandchildG, childH := server.NewId(), server.NewId(), server.NewId(), server.NewId()

	traffic := newOperatorBlockTraffic()
	firstSweepTime := start.Add(time.Hour)
	for _, row := range []*operatorBlockSweepRow{
		{payeeNetworkId: providerA, payoutByteCount: gib, minSweepTime: start.Add(2 * time.Hour), sweepCount: 2},
		{payeeNetworkId: providerA, payoutByteCount: gib, minSweepTime: firstSweepTime, sweepCount: 1},
		{payeeNetworkId: providerB, payoutByteCount: gib, minSweepTime: start.Add(3 * time.Hour), sweepCount: 1},
		{payeeNetworkId: providerZ, payoutByteCount: 0, minSweepTime: start.Add(3 * time.Hour), sweepCount: 1},
	} {
		traffic.add(row)
	}
	reliability := &operatorBlockReliability{
		effectiveBlockCount: 10080,
		// dyadic weights sum exactly in any order
		networkWeights: map[server.Id]float64{
			providerA: 0.25,
			providerB: 0.5,
			providerC: 0.125,
			providerZ: 0.125,
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

	scale := float64(7) / float64(30)
	payoutNanoPoints := PointsToNanoPoints(float64(config.AccountPointsPerPayout) * scale)
	if award.TimeScale != scale || award.PayoutNanoPoints != payoutNanoPoints || payoutNanoPoints != 233_333_333_333 {
		t.Fatalf("time scale %v and payout points %d, want %v and %d", award.TimeScale, award.PayoutNanoPoints, scale, payoutNanoPoints)
	}
	if award.ByteCount != 3*gib || award.SweepCount != 5 || award.Schema != OperatorBlockPointsSchema || award.PaymentPlanId != OperatorBlockPointsPlanId(16) {
		t.Fatalf("award %+v", award)
	}
	wantPayments := []*OperatorBlockPayment{
		{NetworkId: providerA, PayoutByteCount: 2 * gib, MinSweepTime: firstSweepTime},
		{NetworkId: providerB, PayoutByteCount: gib, MinSweepTime: start.Add(3 * time.Hour)},
	}
	slices.SortFunc(wantPayments, func(x *OperatorBlockPayment, y *OperatorBlockPayment) int { return x.NetworkId.Cmp(y.NetworkId) })
	if len(award.Payments) != len(wantPayments) {
		t.Fatalf("payments %+v", award.Payments)
	}
	for i, payment := range award.Payments {
		if *payment != *wantPayments[i] {
			t.Fatalf("payment %+v, want %+v", payment, wantPayments[i])
		}
	}

	pointsA := NanoPoints(155_555_555_555)
	pointsB := NanoPoints(77_777_777_778)
	// calculateReliabilityPayoutInTx's points: shares of the total weight,
	// including the weight of networks that earn nothing in the block
	reliabilityPointsPerPayout := PointsToNanoPoints(float64(config.ReliabilityPointsPerPayout) * scale)
	reliabilityPointsA := NanoPoints(float64(reliabilityPointsPerPayout) * (0.25 / 1.0))
	reliabilityPointsB := NanoPoints(float64(reliabilityPointsPerPayout) * (0.5 / 1.0))
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
		{providerA, providerA, AccountPointEventPayoutMultiplier, server.Id{}}: multipliedA - pointsA + reliabilityPointsA,
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
	// the block's payout points, on the configured scale, exactly
	if events := award.EventNanoPoints(); events[string(AccountPointEventPayout)] != award.PayoutNanoPoints {
		t.Fatalf("payout points sum to %d, want %d", events[string(AccountPointEventPayout)], award.PayoutNanoPoints)
	}
}

// An award of another schema never commits, so a dry run made before the
// bytes rule (v1) cannot award its block by revenue; and an award's payments
// each carry bytes inside the block.
func TestValidateOperatorBlockPointsRefusesOtherSchemasAndEmptyPayments(t *testing.T) {
	start := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	transition := &server.ProviderPayoutTransition{Cutoff: operatorBlockPointsMainCutoff, ConfigSha256: "policy"}
	provider := server.NewId()
	plan := func() *OperatorBlockPoints {
		traffic := newOperatorBlockTraffic()
		traffic.add(&operatorBlockSweepRow{payeeNetworkId: provider, payoutByteCount: 1024, minSweepTime: start.Add(time.Hour), sweepCount: 1})
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
		if err != nil {
			t.Fatal(err)
		}
		return award
	}
	if err := validateOperatorBlockPoints(plan(), transition); err != nil {
		t.Fatal(err)
	}
	v1 := plan()
	v1.Schema = "urnetwork-operator-block-points-v1"
	if err := validateOperatorBlockPoints(v1, transition); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("a v1 award was accepted: %v", err)
	}
	noBytes := plan()
	noBytes.Payments[0].PayoutByteCount = 0
	if err := validateOperatorBlockPoints(noBytes, transition); err == nil {
		t.Fatal("a payment without bytes was accepted")
	}
	outside := plan()
	outside.Payments[0].MinSweepTime = start.Add(SubnetBlockDuration)
	if err := validateOperatorBlockPoints(outside, transition); err == nil {
		t.Fatal("a payment outside the block was accepted")
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
	if len(award.Payments) != 0 || len(award.Points) != 0 || award.ByteCount != 0 || award.PayoutNanoPoints != 0 {
		t.Fatalf("award without traffic %+v", award)
	}
}

// The digest binds every value a commit writes, survives the dry run's file,
// and changes with any point.
func TestOperatorBlockPointsDigestBindsTheAwards(t *testing.T) {
	start := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	provider := server.NewId()
	traffic := newOperatorBlockTraffic()
	traffic.add(&operatorBlockSweepRow{payeeNetworkId: provider, payoutByteCount: 1024, minSweepTime: start.Add(time.Hour), sweepCount: 1})
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
