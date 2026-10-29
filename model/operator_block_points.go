package model

// Operator block points (owner decision 2026-10-28: "points should have never
// paused. The operator should be doing points in parallel with the SN pool
// each week/block.").
//
// A payout plan applies points only inside its subsidy epoch
// (PaymentPlanner.applyPayoutPoints). Since the provider earning boundary
// (sn.yml cutoff_utc, b19f1ebad) every plan selects only contracts that closed
// before the cutoff, and clamps its epoch to the cutoff: traffic after it earns
// from the SN pool, never USDC. Once the last epoch reached the cutoff (the
// plans of 2026-10-11), no plan could form an epoch, so no traffic after the
// cutoff received points.
//
// Each completed operator block (SubnetBlock: seven days from Sunday 00:00
// UTC) now gets the points of its traffic past the cutoff, once, by the rules
// a plan applied before the cutoff:
//   - the plan's computation for the block's traffic: each paying network's
//     subsidy share split over its providers by bytes, the subscription
//     revenue, the net revenue and the reliability subsidy by reliability
//     weight, over the block's window. It only weighs the points: no USDC is
//     planned, sent or recorded, and the sweeps stay unassigned.
//   - the plan's points rule (planPayoutPoints): payout points, which are
//     account_points_per_payout scaled to the window, in proportion to that
//     computed payout; reliability points; the seeker multiplier; and the
//     referral parent and child bonuses.
//
// An award is its own ledger: a payment plan id derived from the block (the
// award happens once whoever writes it), the plan's payment_report, and one
// payment row per provider network that carries the points and stores its
// operator block (paymentPlanBlocksSql), so the leaderboard's Blocks and
// Streak count it. The payment rows carry no USDC and own no sweep. They are
// inserted canceled, with a journal marker, so no USDC path (the sender,
// re-planning, transfer stats, the payout list) treats them as obligations.
// The SN pool weighs contract usage (GetStEpochProviderUsage), which this
// never writes.
//
// The daily task (controller.AwardOperatorBlockPoints) awards each block once
// it completes; the backfill (bringyourctl account-points backfill-blocks)
// awards the blocks that completed before the task ran, with an explicit go.

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
)

const OperatorBlockPointsSchema = "urnetwork-operator-block-points-v1"

// The journal event that marks a payment row as an operator block points row
// (audit_account_payment).
const OperatorBlockPointsEventType = "operator_block_points"

// Awards serialize on this lock: the daily task and the backfill.
const operatorBlockPointsLockKey = "operator-block-points/v1"

// A sweep's time is the start of its settlement transaction, so a settlement
// in flight when a block closes commits a sweep inside the block after it
// closed. A block's traffic is read once this has passed after its close.
const OperatorBlockPointsSettleTime = 2 * time.Hour

// The traffic of a block is read in statements of this much sweep time: about
// 300k sweeps and 6 s on Main (2026-10-20).
var operatorBlockPointsSweepReadDuration = 30 * time.Minute

// Client locations are read in batches of this many clients.
const operatorBlockPointsLocationBatchSize = 10000

var (
	ErrOperatorBlockPointsNoBoundary = errors.New("operator block points need the provider earning boundary (sn.yml); without it payout plans award points")
	ErrOperatorBlockPointsIncomplete = errors.New("operator block is not complete")
	// The reliability window starts before client_reliability retention: some
	// of its evidence may be gone, which would skew the reliability shares.
	ErrOperatorBlockPointsReliabilityExpired = errors.New("operator block reliability evidence is past client_reliability retention")
	// The client_reliability drain has not reached the end of the block.
	ErrOperatorBlockPointsReliabilityPending = errors.New("operator block reliability evidence is not drained yet")
)

// OperatorBlockPointsPlanId is the payment plan id of an operator block's
// points. It derives from the block, so every writer finds an earlier award by
// its payment_report primary key.
func OperatorBlockPointsPlanId(block int64) server.Id {
	sum := md5.Sum([]byte(OperatorBlockPointsSchema + "/" + strconv.FormatInt(block, 10)))
	return server.RequireIdFromBytes(sum[:])
}

// The open time of the 1-based operator block.
func operatorBlockStart(block int64) time.Time {
	return SubnetBlockGenesis.Add(time.Duration(block-1) * SubnetBlockDuration)
}

// OperatorBlockPointsWindow is the block's traffic past the cutoff, [start,
// end). ok is false for a block that ends at or before the cutoff.
func OperatorBlockPointsWindow(block int64, cutoff time.Time) (start time.Time, end time.Time, ok bool) {
	if block < 1 {
		return time.Time{}, time.Time{}, false
	}
	blockStart := operatorBlockStart(block)
	end = blockStart.Add(SubnetBlockDuration)
	start = server.MaxTime(blockStart, cutoff)
	return start, end, start.Before(end)
}

// OperatorBlockPointsDueBlocks lists, ascending, the blocks with traffic past
// the cutoff that completed, with their last settlements, by now.
func OperatorBlockPointsDueBlocks(cutoff time.Time, now time.Time) []int64 {
	blocks := []int64{}
	for block := max(1, pointsBlockOf(cutoff)); ; block += 1 {
		_, end, ok := OperatorBlockPointsWindow(block, cutoff)
		if !ok {
			continue
		}
		if now.Before(end.Add(OperatorBlockPointsSettleTime)) {
			return blocks
		}
		blocks = append(blocks, block)
	}
}

// GetOperatorBlockPointsAwarded reports which of the blocks have their points.
func GetOperatorBlockPointsAwarded(ctx context.Context, blocks []int64) map[int64]bool {
	awarded := map[int64]bool{}
	if len(blocks) == 0 {
		return awarded
	}
	planBlocks := map[server.Id]int64{}
	planIds := []server.Id{}
	for _, block := range blocks {
		planId := OperatorBlockPointsPlanId(block)
		planBlocks[planId] = block
		planIds = append(planIds, planId)
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT payment_plan_id FROM payment_report WHERE payment_plan_id = ANY($1::uuid[])`,
			planIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var planId server.Id
				server.Raise(result.Scan(&planId))
				awarded[planBlocks[planId]] = true
			}
		})
	})
	return awarded
}

// operatorBlockPointsAutomaticBlocks is what one run of the daily task
// awards, ascending:
//   - after any award, every later due block without one, so a missed run
//     catches up
//   - before the first award, the due blocks from fromBlock, the block that
//     was open when the task was first scheduled: the blocks that completed
//     before this code ran are the backfill's, which needs an explicit go
//   - without either, only the latest due block
func operatorBlockPointsAutomaticBlocks(due []int64, awarded map[int64]bool, fromBlock int64) []int64 {
	blocks := []int64{}
	lastAwarded := int64(0)
	for block := range awarded {
		if awarded[block] {
			lastAwarded = max(lastAwarded, block)
		}
	}
	for _, block := range due {
		if awarded[block] {
			continue
		}
		switch {
		case 0 < lastAwarded:
			if lastAwarded < block {
				blocks = append(blocks, block)
			}
		case 0 < fromBlock:
			if fromBlock <= block {
				blocks = append(blocks, block)
			}
		default:
			if block == due[len(due)-1] {
				blocks = append(blocks, block)
			}
		}
	}
	return blocks
}

// OperatorBlockPoints is one block's computed award. It is everything a
// commit writes, so a reviewed dry run commits exactly what it showed, and
// its digest binds that content.
type OperatorBlockPoints struct {
	Schema        string    `json:"schema"`
	Block         int64     `json:"block"`
	PaymentPlanId server.Id `json:"payment_plan_id"`
	// the provider earning boundary the award was computed under
	PolicySha256 string    `json:"policy_sha256"`
	Cutoff       time.Time `json:"cutoff"`
	// the block's traffic past the cutoff, [WindowStart, WindowEnd)
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`

	// the plan's computation for the window (planSubsidyPayments)
	SubsidyScale             float64   `json:"subsidy_scale"`
	SubsidyNetPayoutUsd      float64   `json:"subsidy_net_payout_usd"`
	ActiveUserCount          int       `json:"active_user_count"`
	PaidUserCount            int       `json:"paid_user_count"`
	TrafficUserCount         int       `json:"traffic_user_count"`
	NetPayoutByteCountPaid   ByteCount `json:"net_payout_byte_count_paid"`
	NetPayoutByteCountUnpaid ByteCount `json:"net_payout_byte_count_unpaid"`
	NetRevenue               NanoCents `json:"net_revenue_nano_cents"`
	SweepCount               int64     `json:"sweep_count"`

	// the reliability window, in reliability blocks [min, max)
	ReliabilityMinBlockNumber      int64 `json:"reliability_min_block_number"`
	ReliabilityMaxBlockNumber      int64 `json:"reliability_max_block_number"`
	ReliabilityEffectiveBlockCount int64 `json:"reliability_effective_block_count"`
	ReliabilityNetworkCount        int   `json:"reliability_network_count"`

	// account_points_per_payout scaled to the window
	PayoutNanoPoints NanoPoints `json:"payout_nano_points"`
	PointsPerPayout  int        `json:"points_per_payout"`
	// the payout the plan computed, which weighs the points; none is paid
	TotalComputedPayout NanoCents `json:"total_computed_payout_nano_cents"`

	Payments []*OperatorBlockPayment `json:"payments"`
	Points   []*OperatorBlockPoint   `json:"points"`
}

// OperatorBlockPayment is one provider network's computed payment: what a
// plan would have paid it for the window. The row it commits carries no USDC.
type OperatorBlockPayment struct {
	NetworkId          server.Id `json:"network_id"`
	PayoutByteCount    ByteCount `json:"payout_byte_count"`
	MinSweepTime       time.Time `json:"min_sweep_time"`
	NetRevenue         NanoCents `json:"net_revenue_nano_cents"`
	SubsidyPayout      NanoCents `json:"subsidy_payout_nano_cents"`
	ReliabilitySubsidy NanoCents `json:"reliability_subsidy_nano_cents"`
	ComputedPayout     NanoCents `json:"computed_payout_nano_cents"`
	// the earliest sweep with payout bytes: the operator block it earns in
	MinPaidSweepTime *time.Time `json:"min_paid_sweep_time,omitempty"`
}

// OperatorBlockPoint is one account_point row, linked to the payment of
// PaymentNetworkId.
type OperatorBlockPoint struct {
	NetworkId        server.Id  `json:"network_id"`
	Event            string     `json:"event"`
	PointValue       NanoPoints `json:"point_value"`
	PaymentNetworkId server.Id  `json:"payment_network_id"`
	LinkedNetworkId  *server.Id `json:"linked_network_id,omitempty"`
}

// The sum of the award's points.
func (self *OperatorBlockPoints) TotalNanoPoints() NanoPoints {
	total := NanoPoints(0)
	for _, point := range self.Points {
		total += point.PointValue
	}
	return total
}

// The sum of the award's points by event.
func (self *OperatorBlockPoints) EventNanoPoints() map[string]NanoPoints {
	totals := map[string]NanoPoints{}
	for _, point := range self.Points {
		totals[point.Event] += point.PointValue
	}
	return totals
}

// Digest is the sha256 of the award's canonical JSON.
func (self *OperatorBlockPoints) Digest() string {
	data, err := json.Marshal(self)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// OperatorBlockPointsDigest binds a set of awards, in block order.
func OperatorBlockPointsDigest(awards []*OperatorBlockPoints) string {
	sorted := slices.Clone(awards)
	slices.SortFunc(sorted, func(a *OperatorBlockPoints, b *OperatorBlockPoints) int {
		return int(a.Block - b.Block)
	})
	data, err := json.Marshal(sorted)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(append([]byte(OperatorBlockPointsSchema+"\n"), data...))
	return hex.EncodeToString(sum[:])
}

// The block's traffic past the cutoff, as the plan aggregates its selection:
// per provider (payee) and per paying network and provider.
type operatorBlockTraffic struct {
	payees map[server.Id]*operatorBlockPayee
	// payer -> payee
	pairs map[server.Id]map[server.Id]*operatorBlockPair
	// the balances that carry subscription revenue
	revenueBalances map[server.Id]*subsidyRevenueBalance
	sweepCount      int64
}

type operatorBlockPayee struct {
	payoutByteCount  ByteCount
	netRevenue       NanoCents
	minSweepTime     time.Time
	minPaidSweepTime *time.Time
}

type operatorBlockPair struct {
	paidByteCount   ByteCount
	unpaidByteCount ByteCount
}

func newOperatorBlockTraffic() *operatorBlockTraffic {
	return &operatorBlockTraffic{
		payees:          map[server.Id]*operatorBlockPayee{},
		pairs:           map[server.Id]map[server.Id]*operatorBlockPair{},
		revenueBalances: map[server.Id]*subsidyRevenueBalance{},
	}
}

// One read row: a provider's sweeps funded by one balance.
type operatorBlockSweepRow struct {
	payeeNetworkId    server.Id
	balanceId         server.Id
	payerNetworkId    *server.Id
	paid              bool
	balanceStartTime  *time.Time
	balanceEndTime    *time.Time
	subsidyNetRevenue NanoCents
	payoutByteCount   ByteCount
	netRevenue        NanoCents
	minSweepTime      time.Time
	minPaidSweepTime  *time.Time
	sweepCount        int64
}

// Adds a row as the plan aggregates it: every sweep counts toward its
// provider's bytes and net revenue, and a sweep with a balance row counts
// toward the subsidy (planSubsidyPayments joins the balance).
func (self *operatorBlockTraffic) add(row *operatorBlockSweepRow) {
	self.sweepCount += row.sweepCount
	payee, ok := self.payees[row.payeeNetworkId]
	if !ok {
		payee = &operatorBlockPayee{minSweepTime: row.minSweepTime}
		self.payees[row.payeeNetworkId] = payee
	}
	payee.payoutByteCount += row.payoutByteCount
	payee.netRevenue += row.netRevenue
	payee.minSweepTime = server.MinTime(payee.minSweepTime, row.minSweepTime)
	if row.minPaidSweepTime != nil && (payee.minPaidSweepTime == nil || row.minPaidSweepTime.Before(*payee.minPaidSweepTime)) {
		minPaidSweepTime := *row.minPaidSweepTime
		payee.minPaidSweepTime = &minPaidSweepTime
	}
	if row.payerNetworkId == nil {
		return
	}
	payees, ok := self.pairs[*row.payerNetworkId]
	if !ok {
		payees = map[server.Id]*operatorBlockPair{}
		self.pairs[*row.payerNetworkId] = payees
	}
	pair, ok := payees[row.payeeNetworkId]
	if !ok {
		pair = &operatorBlockPair{}
		payees[row.payeeNetworkId] = pair
	}
	if row.paid {
		pair.paidByteCount += row.payoutByteCount
	} else {
		pair.unpaidByteCount += row.payoutByteCount
	}
	if 0 < row.subsidyNetRevenue && row.balanceStartTime != nil && row.balanceEndTime != nil {
		self.revenueBalances[row.balanceId] = &subsidyRevenueBalance{
			startTime:         *row.balanceStartTime,
			endTime:           *row.balanceEndTime,
			subsidyNetRevenue: row.subsidyNetRevenue,
		}
	}
}

// The unpaid sweeps of contracts that settled at or after the cutoff, with a
// sweep time in [$1, $2), per provider and funding balance: the plan's
// selection (planPayments) past the boundary. A sweep's paid traffic is
// pointsPaidSweepSql, the definition of the operator blocks.
var operatorBlockPointsSweepSql = `
	SELECT
		sweep.network_id,
		sweep.balance_id,
		balance.network_id,
		COALESCE(balance.paid, false),
		balance.start_time,
		balance.end_time,
		COALESCE(balance.subsidy_net_revenue_nano_cents, 0)::bigint,
		SUM(sweep.payout_byte_count)::bigint,
		SUM(sweep.payout_net_revenue_nano_cents)::bigint,
		MIN(sweep.sweep_time),
		MIN(sweep.sweep_time) FILTER (WHERE ` + pointsPaidSweepSql("sweep") + `),
		COUNT(*)
	FROM transfer_escrow_sweep AS sweep
	INNER JOIN transfer_contract AS contract ON
		contract.contract_id = sweep.contract_id
	LEFT JOIN transfer_balance AS balance ON
		balance.balance_id = sweep.balance_id
	WHERE
		$1 <= sweep.sweep_time AND
		sweep.sweep_time < $2 AND
		sweep.payment_id IS NULL AND
		$3 <= contract.close_time AND
		contract.outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination')
	GROUP BY
		sweep.network_id,
		sweep.balance_id,
		balance.network_id,
		balance.paid,
		balance.start_time,
		balance.end_time,
		balance.subsidy_net_revenue_nano_cents
`

// Reads the window's traffic in bounded read-only statements. Like every read
// of an award, each is a statement of its own, outside any transaction, so a
// slow client is never idle in a transaction while it drains a result (see
// readPaymentPlanReliabilityPartials).
func readOperatorBlockTraffic(ctx context.Context, start time.Time, end time.Time, cutoff time.Time) (*operatorBlockTraffic, error) {
	traffic := newOperatorBlockTraffic()
	for chunkStart := start; chunkStart.Before(end); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chunkEnd := server.MinTime(chunkStart.Add(operatorBlockPointsSweepReadDuration), end)
		var rows []*operatorBlockSweepRow
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			rows = nil
			result, err := conn.Query(ctx, operatorBlockPointsSweepSql, chunkStart, chunkEnd, cutoff)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					row := &operatorBlockSweepRow{}
					server.Raise(result.Scan(
						&row.payeeNetworkId,
						&row.balanceId,
						&row.payerNetworkId,
						&row.paid,
						&row.balanceStartTime,
						&row.balanceEndTime,
						&row.subsidyNetRevenue,
						&row.payoutByteCount,
						&row.netRevenue,
						&row.minSweepTime,
						&row.minPaidSweepTime,
						&row.sweepCount,
					))
					rows = append(rows, row)
				}
			})
			boundedReadDone("traffic")
		})
		for _, row := range rows {
			traffic.add(row)
		}
		if chunkEnd.Sub(start)%(24*time.Hour) == 0 || !chunkEnd.Before(end) {
			// progress: a block's reads take hours on Main
			glog.Infof("[obp]traffic [%s, %s) read to %s: %d sweeps\n", start.Format(time.RFC3339), end.Format(time.RFC3339), chunkEnd.Format(time.RFC3339), traffic.sweepCount)
		}
		chunkStart = chunkEnd
	}
	return traffic, nil
}

// The reliability of the window: each network's multiplied reliability
// weight, as a plan reads it from network_connection_reliability_score
// (GetAllMultipliedNetworkReliabilityScoresInTx) after folding the window
// (paymentPlanReliabilityFoldSql). It is computed here from read-only reads,
// so neither a dry run nor an award writes the scores a payout plan owns.
type operatorBlockReliability struct {
	minBlockNumber      int64
	maxBlockNumber      int64
	effectiveBlockCount int64
	networkWeights      map[server.Id]float64
}

// The reliability blocks of a half-open window.
func operatorBlockReliabilityBlockWindow(start time.Time, end time.Time) (minBlockNumber int64, maxBlockNumber int64) {
	return paymentPlanReliabilityBlockWindow(start, end.Add(-time.Nanosecond))
}

// A network's clients in one country; hasCountry is false for a valid client
// location without a country.
type operatorBlockReliabilityKey struct {
	networkId         server.Id
	countryLocationId server.Id
	hasCountry        bool
}

// Reads the window's reliability with the plan's fold, in bounded read-only
// statements, none in a transaction: the plan's chunk reads
// (readPaymentPlanReliabilityPartials), the valid client locations, the exact
// numeric sums per network and country, the division in SQL as the fold does
// it, and the country multipliers.
func readOperatorBlockReliability(ctx context.Context, start time.Time, end time.Time, now time.Time) (*operatorBlockReliability, error) {
	minBlockNumber, maxBlockNumber := operatorBlockReliabilityBlockWindow(start, end)
	reliability := &operatorBlockReliability{
		minBlockNumber: minBlockNumber,
		maxBlockNumber: maxBlockNumber,
		networkWeights: map[server.Id]float64{},
	}
	checkRetention := func() error {
		if minBlockNumber < clientReliabilityRetentionCutoffBlock(ctx, now) {
			return fmt.Errorf("%w: window starts at reliability block %d", ErrOperatorBlockPointsReliabilityExpired, minBlockNumber)
		}
		return nil
	}
	if err := checkRetention(); err != nil {
		return nil, err
	}

	var shift int64
	var coveredBlockCount int64
	var degradedBlockNumbers []int64
	// three statements, each its own snapshot as in a read committed
	// transaction, with no transaction open between them
	server.Db(ctx, func(conn server.PgConn) {
		shift = reliabilityRollupBlockShift(ctx, conn, maxBlockNumber)
		coveredBlockCount = reliabilityCoveredBlockCount(ctx, conn, minBlockNumber, maxBlockNumber)
		degradedBlockNumbers = reliabilityDegradedBlocks(ctx, conn, minBlockNumber, maxBlockNumber)
		boundedReadDone("reliability_blocks")
	})
	if 0 < shift {
		return nil, fmt.Errorf("%w: %d blocks behind the window end", ErrOperatorBlockPointsReliabilityPending, shift)
	}
	reliability.effectiveBlockCount = coveredBlockCount - int64(len(degradedBlockNumbers))
	if reliability.effectiveBlockCount <= 0 {
		// no evidence to judge: no reliability points, as a plan without
		// scores pays none
		glog.Infof("[obp]no usable blocks in reliability window [%d, %d)\n", minBlockNumber, maxBlockNumber)
		return reliability, nil
	}

	sums := map[operatorBlockReliabilityKey]*reliabilityExactSum{}
	// client -> its valid location's country; absent for an invalid location
	clientCountries := map[server.Id]*server.Id{}
	clientRead := map[server.Id]bool{}
	for chunkMin := minBlockNumber; chunkMin < maxBlockNumber; chunkMin += paymentPlanReliabilityChunkBlockCount {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chunkMax := min(chunkMin+paymentPlanReliabilityChunkBlockCount, maxBlockNumber)
		partials := readPaymentPlanReliabilityPartials(ctx, chunkMin, chunkMax, degradedBlockNumbers)

		unread := []server.Id{}
		for key := range partials {
			if !clientRead[key.clientId] {
				clientRead[key.clientId] = true
				unread = append(unread, key.clientId)
			}
		}
		slices.SortFunc(unread, server.Id.Cmp)
		for batch := range slices.Chunk(unread, operatorBlockPointsLocationBatchSize) {
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`
					SELECT client_id, country_location_id
					FROM network_client_location_reliability
					WHERE client_id = ANY($1::uuid[]) AND valid = true
					`,
					batch,
				)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						var clientId server.Id
						var countryLocationId *server.Id
						server.Raise(result.Scan(&clientId, &countryLocationId))
						clientCountries[clientId] = countryLocationId
					}
				})
				boundedReadDone("client_locations")
			})
		}

		for partialKey, partial := range partials {
			countryLocationId, valid := clientCountries[partialKey.clientId]
			if !valid {
				continue
			}
			key := operatorBlockReliabilityKey{networkId: partialKey.networkId}
			if countryLocationId != nil {
				key.countryLocationId = *countryLocationId
				key.hasCountry = true
			}
			sum, ok := sums[key]
			if !ok {
				sum = &reliabilityExactSum{}
				sums[key] = sum
			}
			sum.add(partial.reliabilitySum.numeric())
		}
		// progress: a block's reads take hours on Main
		glog.Infof("[obp]reliability blocks [%d, %d) read to %d: %d clients\n", minBlockNumber, maxBlockNumber, chunkMax, len(clientRead))
	}
	if err := checkRetention(); err != nil {
		return nil, err
	}
	if len(sums) == 0 {
		return reliability, nil
	}

	keys := slices.SortedFunc(maps.Keys(sums), func(a operatorBlockReliabilityKey, b operatorBlockReliabilityKey) int {
		if c := a.networkId.Cmp(b.networkId); c != 0 {
			return c
		}
		if a.hasCountry != b.hasCountry {
			if a.hasCountry {
				return 1
			}
			return -1
		}
		return a.countryLocationId.Cmp(b.countryLocationId)
	})
	numerics := make([]pgtype.Numeric, len(keys))
	for i, key := range keys {
		numerics[i] = sums[key].numeric()
	}
	weights := make([]float64, len(keys))
	multipliers := map[server.Id]float64{}
	server.Db(ctx, func(conn server.PgConn) {
		// the fold's division and its float8 store, exactly
		result, err := conn.Query(
			ctx,
			`
			SELECT t.i, (t.s / $1::bigint)::float8
			FROM unnest($2::numeric[]) WITH ORDINALITY AS t(s, i)
			ORDER BY t.i
			`,
			reliability.effectiveBlockCount,
			numerics,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var i int64
				var weight float64
				server.Raise(result.Scan(&i, &weight))
				weights[i-1] = weight
			}
		})
		boundedReadDone("reliability_weights")
		result, err = conn.Query(
			ctx,
			`SELECT country_location_id, reliability_multiplier FROM network_client_location_reliability_multiplier`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var countryLocationId server.Id
				var multiplier float64
				server.Raise(result.Scan(&countryLocationId, &multiplier))
				multipliers[countryLocationId] = multiplier
			}
		})
		boundedReadDone("reliability_multipliers")
	})
	for i, key := range keys {
		multiplier := 1.0
		if key.hasCountry {
			if m, ok := multipliers[key.countryLocationId]; ok {
				multiplier = m
			}
		}
		// the explicit conversion rounds the product, as SQL does, before the sum
		reliability.networkWeights[key.networkId] += float64(weights[i] * multiplier)
	}
	return reliability, nil
}

// The reliability subsidy of a plan (calculateReliabilityPayoutInTx) from the
// networks' multiplied weights: reliability points and the reliability USDC
// subsidy per subsidy period, scaled to the window, in proportion to weight.
func operatorBlockReliabilitySubsidies(
	networkWeights map[server.Id]float64,
	subsidyScale float64,
	subsidyConfig *SubsidyConfig,
) map[server.Id]NetworkReliabilitySubsidy {
	networkReliabilitySubsidies := map[server.Id]NetworkReliabilitySubsidy{}

	networkIds := slices.SortedFunc(maps.Keys(networkWeights), server.Id.Cmp)
	totalReliabilityWeight := 0.0
	for _, networkId := range networkIds {
		totalReliabilityWeight += networkWeights[networkId]
	}
	if totalReliabilityWeight == 0 {
		return networkReliabilitySubsidies
	}

	reliabilityPointsPerPayout := PointsToNanoPoints(float64(subsidyConfig.ReliabilityPointsPerPayout) * subsidyScale)
	reliabilitySubsidyPerPayout := NanoCents(float64(UsdToNanoCents(float64(subsidyConfig.ReliabilitySubsidyPerPayoutUsd))) * subsidyScale)

	for _, networkId := range networkIds {
		subsidy := NetworkReliabilitySubsidy{}
		weight := networkWeights[networkId]

		if reliabilityPoints := float64(reliabilityPointsPerPayout) * float64(weight/totalReliabilityWeight); reliabilityPoints > 0 {
			subsidy.Points = NanoPoints(reliabilityPoints)
		}
		if reliabilitySubsidyPerPayout > 0 {
			reliabilitySubsidy := NanoCents(float64(reliabilitySubsidyPerPayout) * (weight / totalReliabilityWeight))
			if reliabilitySubsidy >= 0 {
				subsidy.Usdc = reliabilitySubsidy
			}
		}
		networkReliabilitySubsidies[networkId] = subsidy
	}
	return networkReliabilitySubsidies
}

// The referring network of each network, from the referral map. A network
// referred by more than one network takes the lowest id; one that referred
// itself has none (planPayments).
func operatorBlockReferralParents(networkReferrals map[server.Id][]server.Id) map[server.Id]server.Id {
	parents := map[server.Id]server.Id{}
	for _, parentNetworkId := range slices.SortedFunc(maps.Keys(networkReferrals), server.Id.Cmp) {
		for _, childNetworkId := range networkReferrals[parentNetworkId] {
			if childNetworkId == parentNetworkId {
				continue
			}
			if _, ok := parents[childNetworkId]; !ok {
				parents[childNetworkId] = parentNetworkId
			}
		}
	}
	return parents
}

// The inputs of an award other than the traffic and reliability reads.
type operatorBlockPointsInputs struct {
	subsidyConfig    *SubsidyConfig
	block            int64
	cutoff           time.Time
	policySha256     string
	windowStart      time.Time
	windowEnd        time.Time
	seekerHolders    map[server.Id]bool
	networkReferrals map[server.Id][]server.Id
}

// planOperatorBlockPoints computes the award from its reads, writing
// nothing. The payout is planSubsidyPayments' computation over the window, and
// the points are planPayoutPoints. A plan's withholding of small payments, its
// minimum epoch duration and its carried USDC bonuses are USDC rules: with no
// USDC, every provider with traffic in the block earns in the block.
func planOperatorBlockPoints(
	inputs *operatorBlockPointsInputs,
	traffic *operatorBlockTraffic,
	reliability *operatorBlockReliability,
) (*OperatorBlockPoints, error) {
	subsidyConfig := inputs.subsidyConfig
	planId := OperatorBlockPointsPlanId(inputs.block)
	award := &OperatorBlockPoints{
		Schema:                         OperatorBlockPointsSchema,
		Block:                          inputs.block,
		PaymentPlanId:                  planId,
		PolicySha256:                   inputs.policySha256,
		Cutoff:                         inputs.cutoff,
		WindowStart:                    inputs.windowStart,
		WindowEnd:                      inputs.windowEnd,
		SweepCount:                     traffic.sweepCount,
		ReliabilityMinBlockNumber:      reliability.minBlockNumber,
		ReliabilityMaxBlockNumber:      reliability.maxBlockNumber,
		ReliabilityEffectiveBlockCount: reliability.effectiveBlockCount,
		ReliabilityNetworkCount:        len(reliability.networkWeights),
		PointsPerPayout:                subsidyConfig.AccountPointsPerPayout,
		Payments:                       []*OperatorBlockPayment{},
		Points:                         []*OperatorBlockPoint{},
	}

	// the computed payments, keyed by network; each placeholder payment id is
	// its network until a commit assigns the row's id
	payments := map[server.Id]*AccountPayment{}
	for networkId, payee := range traffic.payees {
		payments[networkId] = &AccountPayment{
			PaymentId:       networkId,
			PaymentPlanId:   planId,
			NetworkId:       networkId,
			PayoutByteCount: payee.payoutByteCount,
			Payout:          payee.netRevenue,
			MinSweepTime:    payee.minSweepTime,
		}
	}

	/**
	 * The subsidy, as planSubsidyPayments computes it
	 */
	payerNetworkIds := slices.SortedFunc(maps.Keys(traffic.pairs), server.Id.Cmp)
	for _, payerNetworkId := range payerNetworkIds {
		payerPaid := ByteCount(0)
		payerUnpaid := ByteCount(0)
		for _, pair := range traffic.pairs[payerNetworkId] {
			payerPaid += pair.paidByteCount
			payerUnpaid += pair.unpaidByteCount
		}
		award.NetPayoutByteCountPaid += payerPaid
		award.NetPayoutByteCountUnpaid += payerUnpaid
		if subsidyConfig.ActiveUserByteCountThreshold() <= payerPaid+payerUnpaid {
			award.ActiveUserCount += 1
		}
		if 0 < payerPaid {
			award.PaidUserCount += 1
		}
		if 0 < payerPaid+payerUnpaid {
			award.TrafficUserCount += 1
		}
	}

	revenueBalances := []*subsidyRevenueBalance{}
	for _, balanceId := range slices.SortedFunc(maps.Keys(traffic.revenueBalances), server.Id.Cmp) {
		revenueBalances = append(revenueBalances, traffic.revenueBalances[balanceId])
	}
	award.NetRevenue = subsidyNetRevenuePerPeriod(
		revenueBalances,
		inputs.windowStart,
		inputs.windowEnd,
		subsidyConfig.Duration(),
	)
	subsidyPayoutUsd := max(
		subsidyConfig.MinPayoutUsd,
		subsidyConfig.UsdPerActiveUser*float64(award.ActiveUserCount),
		subsidyConfig.SubscriptionNetRevenueFraction*NanoCentsToUsd(award.NetRevenue),
	)
	award.SubsidyScale = min(float64(inputs.windowEnd.Sub(inputs.windowStart))/float64(subsidyConfig.Duration()), 1.0)
	award.SubsidyNetPayoutUsd = award.SubsidyScale * subsidyPayoutUsd

	subsidyPayouts := map[server.Id]NanoCents{}
	netPayout := NanoCents(0)
	if 0 < award.TrafficUserCount {
		for _, payerNetworkId := range payerNetworkIds {
			payees := traffic.pairs[payerNetworkId]
			netPayerPayoutByteCount := ByteCount(0)
			for _, pair := range payees {
				netPayerPayoutByteCount += pair.paidByteCount + pair.unpaidByteCount
			}
			if netPayerPayoutByteCount <= 0 {
				continue
			}
			for _, payeeNetworkId := range slices.SortedFunc(maps.Keys(payees), server.Id.Cmp) {
				pair := payees[payeeNetworkId]
				// each user is weighted equally, relative to their own traffic
				weight := float64(pair.paidByteCount+pair.unpaidByteCount) / float64(netPayerPayoutByteCount)
				payout := UsdToNanoCents(weight * award.SubsidyNetPayoutUsd / float64(award.TrafficUserCount))
				netPayout += payout
				if 0 < payout {
					subsidyPayouts[payeeNetworkId] += payout
				}
			}
		}
	}
	// +1 for rounding errors
	if UsdToNanoCents(award.SubsidyNetPayoutUsd+1) < netPayout {
		return nil, fmt.Errorf("operator block %d subsidy exceeds the allowed amount: %d <> %d", inputs.block, UsdToNanoCents(award.SubsidyNetPayoutUsd), netPayout)
	}
	for networkId, payout := range subsidyPayouts {
		payment := payments[networkId]
		payment.Payout += payout
		payment.SubsidyPayout += payout
	}

	/**
	 * The reliability subsidy, as calculateReliabilityPayoutInTx computes it
	 */
	reliabilitySubsidies := operatorBlockReliabilitySubsidies(reliability.networkWeights, award.SubsidyScale, subsidyConfig)
	for networkId, reliabilitySubsidy := range reliabilitySubsidies {
		if payment, ok := payments[networkId]; ok {
			payment.Payout += reliabilitySubsidy.Usdc
			payment.ReliabilitySubsidy = reliabilitySubsidy.Usdc
		}
	}

	for _, networkId := range slices.SortedFunc(maps.Keys(payments), server.Id.Cmp) {
		payment := payments[networkId]
		payee := traffic.payees[networkId]
		award.TotalComputedPayout += payment.Payout - payment.BonusPayout
		award.Payments = append(award.Payments, &OperatorBlockPayment{
			NetworkId:          networkId,
			PayoutByteCount:    payment.PayoutByteCount,
			MinSweepTime:       payment.MinSweepTime,
			NetRevenue:         payee.netRevenue,
			SubsidyPayout:      payment.SubsidyPayout,
			ReliabilitySubsidy: payment.ReliabilitySubsidy,
			ComputedPayout:     payment.Payout,
			MinPaidSweepTime:   payee.minPaidSweepTime,
		})
	}

	/**
	 * The points, by the plan's rule
	 */
	if 0 < award.SubsidyScale && 0 < award.TotalComputedPayout {
		totalPoints := float64(subsidyConfig.AccountPointsPerPayout) * award.SubsidyScale
		award.PayoutNanoPoints = PointsToNanoPoints(totalPoints)
		rows := planPayoutPoints(&payoutPointsPlan{
			ctx:                  context.Background(),
			subsidyConfig:        subsidyConfig,
			paymentPlanId:        planId,
			totalPoints:          totalPoints,
			totalPayout:          award.TotalComputedPayout,
			payments:             payments,
			reliabilitySubsidies: reliabilitySubsidies,
			seekerHolders:        inputs.seekerHolders,
			referralParents:      operatorBlockReferralParents(inputs.networkReferrals),
			networkReferrals:     inputs.networkReferrals,
		})
		for _, row := range rows {
			point := &OperatorBlockPoint{
				NetworkId:  row.NetworkId,
				Event:      string(row.Event),
				PointValue: row.PointValue,
				// the placeholder payment id is the payment's network
				PaymentNetworkId: *row.AccountPaymentId,
			}
			if row.LinkedNetworkId != nil {
				linkedNetworkId := *row.LinkedNetworkId
				point.LinkedNetworkId = &linkedNetworkId
			}
			award.Points = append(award.Points, point)
		}
	}
	slices.SortStableFunc(award.Points, compareOperatorBlockPoints)
	return award, nil
}

func compareOperatorBlockPoints(a *OperatorBlockPoint, b *OperatorBlockPoint) int {
	if c := a.PaymentNetworkId.Cmp(b.PaymentNetworkId); c != 0 {
		return c
	}
	if c := a.NetworkId.Cmp(b.NetworkId); c != 0 {
		return c
	}
	if a.Event != b.Event {
		if a.Event < b.Event {
			return -1
		}
		return 1
	}
	if (a.LinkedNetworkId == nil) != (b.LinkedNetworkId == nil) {
		if a.LinkedNetworkId == nil {
			return -1
		}
		return 1
	}
	if a.LinkedNetworkId != nil {
		if c := a.LinkedNetworkId.Cmp(*b.LinkedNetworkId); c != 0 {
			return c
		}
	}
	switch {
	case a.PointValue < b.PointValue:
		return -1
	case b.PointValue < a.PointValue:
		return 1
	default:
		return 0
	}
}

// ComputeOperatorBlockPoints computes a completed block's award with read-only
// statements; it writes nothing, so a read-only process can run it.
func ComputeOperatorBlockPoints(
	ctx context.Context,
	subsidyConfig *SubsidyConfig,
	block int64,
	now time.Time,
) (award *OperatorBlockPoints, returnErr error) {
	defer recoverOperatorBlockPointsError(&returnErr)
	transition, err := server.LoadProviderPayoutEarningPolicy(ctx)
	if err != nil {
		return nil, err
	}
	if transition == nil {
		return nil, ErrOperatorBlockPointsNoBoundary
	}
	start, end, ok := OperatorBlockPointsWindow(block, transition.Cutoff)
	if !ok {
		return nil, fmt.Errorf("operator block %d has no traffic past the cutoff %s", block, transition.Cutoff.Format(time.RFC3339))
	}
	if now.Before(end.Add(OperatorBlockPointsSettleTime)) {
		return nil, fmt.Errorf("%w: block %d ends %s", ErrOperatorBlockPointsIncomplete, block, end.Format(time.RFC3339))
	}

	readStart := time.Now()
	traffic, err := readOperatorBlockTraffic(ctx, start, end, transition.Cutoff)
	if err != nil {
		return nil, err
	}
	reliability, err := readOperatorBlockReliability(ctx, start, end, now)
	if err != nil {
		return nil, err
	}
	award, err = planOperatorBlockPoints(
		&operatorBlockPointsInputs{
			subsidyConfig:    subsidyConfig,
			block:            block,
			cutoff:           transition.Cutoff,
			policySha256:     transition.ConfigSha256,
			windowStart:      start,
			windowEnd:        end,
			seekerHolders:    GetAllSeekerHolders(ctx),
			networkReferrals: GetNetworkReferralsMap(ctx),
		},
		traffic,
		reliability,
	)
	if err != nil {
		return nil, err
	}
	glog.Infof(
		"[obp]block %d [%s, %s): %d sweeps, %d providers, %d paying networks, %d reliability networks, %d point rows, %d nano points, read in %s\n",
		block,
		start.Format(time.RFC3339),
		end.Format(time.RFC3339),
		award.SweepCount,
		len(award.Payments),
		award.TrafficUserCount,
		award.ReliabilityNetworkCount,
		len(award.Points),
		award.TotalNanoPoints(),
		time.Since(readStart).Round(time.Second),
	)
	return award, nil
}

// Database errors raise; the award's callers return them.
func recoverOperatorBlockPointsError(returnErr *error) {
	if value := recover(); value != nil {
		if err, ok := value.(error); ok {
			*returnErr = err
			return
		}
		panic(value)
	}
}

// The event id of the marker of payment `%[1]s`, as the USDC close-out derives
// its marker (usdcPayoutClosedEventIdSql).
func operatorBlockPointsEventId(paymentId server.Id) server.Id {
	sum := md5.Sum([]byte(OperatorBlockPointsEventType + "/" + paymentId.String()))
	return server.RequireIdFromBytes(sum[:])
}

// The award was computed for the boundary in force and is internally whole.
func validateOperatorBlockPoints(award *OperatorBlockPoints, transition *server.ProviderPayoutTransition) error {
	if award.Schema != OperatorBlockPointsSchema {
		return fmt.Errorf("operator block points schema %q is not %q", award.Schema, OperatorBlockPointsSchema)
	}
	if award.PaymentPlanId != OperatorBlockPointsPlanId(award.Block) {
		return fmt.Errorf("operator block %d has plan id %s, not its own", award.Block, award.PaymentPlanId)
	}
	if transition == nil || award.PolicySha256 != transition.ConfigSha256 || !award.Cutoff.Equal(transition.Cutoff) {
		return fmt.Errorf("operator block %d was computed under another earning boundary", award.Block)
	}
	start, end, ok := OperatorBlockPointsWindow(award.Block, transition.Cutoff)
	if !ok || !award.WindowStart.Equal(start) || !award.WindowEnd.Equal(end) {
		return fmt.Errorf("operator block %d window differs from the block's", award.Block)
	}
	networks := map[server.Id]bool{}
	for _, payment := range award.Payments {
		if networks[payment.NetworkId] {
			return fmt.Errorf("operator block %d lists network %s twice", award.Block, payment.NetworkId)
		}
		networks[payment.NetworkId] = true
		if payment.MinPaidSweepTime != nil && (payment.MinPaidSweepTime.Before(start) || !payment.MinPaidSweepTime.Before(end)) {
			return fmt.Errorf("operator block %d payment of %s is outside the block", award.Block, payment.NetworkId)
		}
	}
	for _, point := range award.Points {
		if !networks[point.PaymentNetworkId] {
			return fmt.Errorf("operator block %d point links no payment of the award", award.Block)
		}
	}
	return nil
}

// CommitOperatorBlockPoints writes a computed award in one transaction,
// unless the block already has its points: committed is false then and
// nothing is written.
func CommitOperatorBlockPoints(ctx context.Context, award *OperatorBlockPoints) (committed bool, returnErr error) {
	defer recoverOperatorBlockPointsError(&returnErr)
	transition, err := server.LoadProviderPayoutEarningPolicy(ctx)
	if err != nil {
		return false, err
	}
	if err := validateOperatorBlockPoints(award, transition); err != nil {
		return false, err
	}

	server.Tx(ctx, func(tx server.PgTx) {
		committed = false
		server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, operatorBlockPointsLockKey))
		var awarded bool
		server.Raise(tx.QueryRow(
			ctx,
			`
			SELECT
				EXISTS (SELECT 1 FROM payment_report WHERE payment_plan_id = $1) OR
				EXISTS (SELECT 1 FROM account_payment WHERE payment_plan_id = $1)
			`,
			award.PaymentPlanId,
		).Scan(&awarded))
		if awarded {
			return
		}

		now := server.NowUtc()
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO payment_report (
				payment_plan_id,
				total_payout_nano_cents,
				total_nano_points,
				point_scale_factor,
				time_scale_factor,
				payout_points_per_payout
			)
			VALUES ($1, $2, $3, $4, $5, $6)
			`,
			award.PaymentPlanId,
			award.TotalComputedPayout,
			award.PayoutNanoPoints,
			float64(award.PointsPerPayout)*award.SubsidyScale,
			award.SubsidyScale,
			award.PointsPerPayout,
		))

		paymentIds := map[server.Id]server.Id{}
		for _, payment := range award.Payments {
			paymentIds[payment.NetworkId] = server.NewId()
		}
		eventDetails, err := json.Marshal(map[string]any{
			"block":           award.Block,
			"payment_plan_id": award.PaymentPlanId,
			"policy_sha256":   award.PolicySha256,
		})
		server.Raise(err)
		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for _, payment := range award.Payments {
				paymentId := paymentIds[payment.NetworkId]
				// no USDC: the block's traffic is paid by the SN pool
				batch.Queue(
					`
					INSERT INTO account_payment (
						payment_id,
						payment_plan_id,
						network_id,
						payout_byte_count,
						payout_nano_cents,
						subsidy_payout_nano_cents,
						reliability_subsidy_nano_cents,
						min_sweep_time,
						create_time,
						canceled,
						cancel_time,
						block_rollup_complete
					)
					VALUES ($1, $2, $3, $4, 0, 0, 0, $5, $6, true, $6, true)
					`,
					paymentId,
					award.PaymentPlanId,
					payment.NetworkId,
					payment.PayoutByteCount,
					payment.MinSweepTime,
					now,
				)
				batch.Queue(
					`
					INSERT INTO audit_account_payment (event_id, payment_id, event_type, event_details)
					VALUES ($1, $2, $3, $4)
					`,
					operatorBlockPointsEventId(paymentId),
					paymentId,
					OperatorBlockPointsEventType,
					string(eventDetails),
				)
			}
		})

		// the payments' operator blocks, by the plan's statement: the award's
		// selection is one row per provider, its paid bytes at its earliest
		// paid sweep, and the window is inside one block
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			CREATE TEMPORARY TABLE temp_account_payment (
				network_id uuid NOT NULL,
				payout_byte_count bigint NOT NULL,
				sweep_time timestamp NOT NULL
			)
			ON COMMIT DROP
			`,
		))
		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for _, payment := range award.Payments {
				if payment.MinPaidSweepTime == nil {
					continue
				}
				batch.Queue(
					`INSERT INTO temp_account_payment (network_id, payout_byte_count, sweep_time) VALUES ($1, $2, $3)`,
					payment.NetworkId,
					payment.PayoutByteCount,
					*payment.MinPaidSweepTime,
				)
			}
		})
		server.CreateTempJoinTableInTx(
			ctx,
			tx,
			"temp_payment_network_ids(network_id uuid -> payment_id uuid)",
			paymentIds,
		)
		server.RaisePgResult(tx.Exec(
			ctx,
			paymentPlanBlocksSql,
			SubnetBlockGenesis,
			int64(SubnetBlockDuration/time.Second),
		))
		var outsideCount int64
		server.Raise(tx.QueryRow(
			ctx,
			`
			SELECT COUNT(*) FROM account_payment_block
			INNER JOIN temp_payment_network_ids ON
				temp_payment_network_ids.payment_id = account_payment_block.payment_id
			WHERE account_payment_block.block_number <> $1
			`,
			award.Block,
		).Scan(&outsideCount))
		if outsideCount != 0 {
			panic(fmt.Errorf("operator block %d stored %d blocks outside the block", award.Block, outsideCount))
		}

		pointArgs := []ApplyAccountPointsArgs{}
		for _, point := range award.Points {
			paymentId := paymentIds[point.PaymentNetworkId]
			args := ApplyAccountPointsArgs{
				NetworkId:        point.NetworkId,
				Event:            AccountPointEvent(point.Event),
				PointValue:       point.PointValue,
				AccountPaymentId: &paymentId,
				PaymentPlanId:    &award.PaymentPlanId,
			}
			if point.LinkedNetworkId != nil {
				linkedNetworkId := *point.LinkedNetworkId
				args.LinkedNetworkId = &linkedNetworkId
			}
			pointArgs = append(pointArgs, args)
		}
		ApplyAccountPointsBatchInTx(ctx, tx, pointArgs)
		committed = true
	}, server.TxReadCommitted)

	if committed {
		glog.Infof(
			"[obp]block %d awarded: plan %s, %d payments, %d point rows, %d nano points\n",
			award.Block,
			award.PaymentPlanId,
			len(award.Payments),
			len(award.Points),
			award.TotalNanoPoints(),
		)
	}
	return committed, nil
}

// AwardDueOperatorBlockPoints awards the blocks one run of the daily task owns
// (operatorBlockPointsAutomaticBlocks), oldest first, and returns the awards
// it committed. Without an earning boundary it does nothing: payout plans
// award points.
func AwardDueOperatorBlockPoints(
	ctx context.Context,
	subsidyConfig *SubsidyConfig,
	now time.Time,
	fromBlock int64,
) (awards []*OperatorBlockPoints, returnErr error) {
	defer recoverOperatorBlockPointsError(&returnErr)
	transition, err := server.LoadProviderPayoutEarningPolicy(ctx)
	if err != nil {
		return nil, err
	}
	if transition == nil {
		return nil, nil
	}
	due := OperatorBlockPointsDueBlocks(transition.Cutoff, now)
	awarded := GetOperatorBlockPointsAwarded(ctx, due)
	blocks := operatorBlockPointsAutomaticBlocks(due, awarded, fromBlock)
	glog.Infof("[obp]due blocks %v, awarded %v, from block %d: awarding %v\n", due, slices.Sorted(maps.Keys(awarded)), fromBlock, blocks)
	for _, block := range blocks {
		award, err := ComputeOperatorBlockPoints(ctx, subsidyConfig, block, now)
		if err != nil {
			return awards, err
		}
		committed, err := CommitOperatorBlockPoints(ctx, award)
		if err != nil {
			return awards, err
		}
		if committed {
			awards = append(awards, award)
		}
	}
	return awards, nil
}
