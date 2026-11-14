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
// UTC) now gets the points of its traffic past the cutoff, once. Since schema
// v2 (owner decision 2026-11-13: "please make points proportional to traffic
// served and not revenue/paid traffic. After this change revenue should not
// have an affect on any payout."):
//   - payout points: account_points_per_payout scaled to the window, shared
//     exactly among the provider networks in proportion to the bytes each
//     served in the window (operatorBlockPayoutPointShares). A network's bytes
//     are the payout bytes of its sweeps, whatever balance funded them: a
//     purchase, a Pro grant or a free grant, paid or unpaid, and whatever the
//     payer. No revenue, net revenue, subscription revenue or revenue-sized
//     subsidy enters an award, and no payer weighs more than another.
//   - reliability points by reliability weight, the seeker multiplier, and the
//     referral parent and child bonuses (planPayoutPoints), whose inputs are
//     reliability evidence, seeker holdings and referrals, not revenue.
// The blocks awarded before v2 keep what the payout plan's rule gave them,
// which weighed the payout points by a computed payout of net revenue and a
// revenue-sized subsidy: each block is awarded once. No USDC is planned, sent
// or recorded, and the sweeps stay unassigned.
//
// A sweep is a provider network's share of one funding balance's debited bytes
// of a settled contract (allocateContractParticipantPayouts). A participant on
// the contract's paying network takes no share, and a contract the degradation
// valve let settle without escrow has no sweep, so neither earns points here;
// the SN pool's usage (GetStEpochProviderUsage) counts both.
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
//
// An award must run before its window's reliability evidence passes
// client_reliability retention, ClientReliabilityRetention after the window
// opens, about three days after the block is due: past that some evidence
// may be gone, and the award fails closed
// (ErrOperatorBlockPointsReliabilityExpired). The daily task awards its blocks
// oldest first and stops at the first error, since it owns only the blocks
// after its last award, and a block it passed over on a transient error would
// never be tried again. A block past its deadline is no transient error, as
// retention only moves forward: the task records it expired, once
// (operator_block_points_expired), and goes on to the later blocks. An
// expired block never gets its points, by the task or by a backfill
// (OperatorBlockPointsExpiry). Until 2026-11-14 the task stopped at such a
// block on every run, so no later block was ever awarded again.

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/server/v2026"
)

// The schema of an award, which its dry run's file and digest carry. v2
// (2026-11-13) shares the payout points by bytes served; v1 weighed them by a
// computed revenue-based payout. A commit refuses an award of another schema,
// so a v1 dry run cannot commit a block under the old rule.
const OperatorBlockPointsSchema = "urnetwork-operator-block-points-v2"

// The namespace of the blocks' payment plan ids: v1's, for every schema. A
// block keeps one plan id, so an award of any schema finds an earlier award of
// the block, and the leaderboard and the epoch history find either.
const operatorBlockPointsPlanIdNamespace = "urnetwork-operator-block-points-v1"

// The journal event that marks a payment row as an operator block points row
// (audit_account_payment).
const OperatorBlockPointsEventType = "operator_block_points"

// Awards serialize on this lock: the daily task and the backfill.
const operatorBlockPointsLockKey = "operator-block-points/v1"

// A sweep's time is the start of its settlement transaction, so a settlement
// in flight when a block closes commits a sweep inside the block after it
// closed. A block's traffic is read once this has passed after its close.
const OperatorBlockPointsSettleTime = 2 * time.Hour

// An award run's claim bound (controller.AwardOperatorBlockPoints). A block's
// reads take about an hour on Main, and a run that catches up several blocks
// takes longer, so a run can still be reading this long after it started.
const OperatorBlockPointsMaxTime = 12 * time.Hour

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
	// The daily task recorded the block expired (OperatorBlockPointsExpiry):
	// it is never awarded.
	ErrOperatorBlockPointsExpired = errors.New("operator block expired: its reliability evidence passed client_reliability retention before any award, so it is never awarded")
)

// Operator blocks the daily task recorded expired, counted once each, when it
// wrote the record. Every process registers it at zero; the taskworker that
// ran the award counts.
var operatorBlockPointsExpiredBlocksCounter = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: "urnetwork", Subsystem: "operator_block_points", Name: "expired_blocks_total",
	Help: "Operator blocks the daily award recorded expired, once each: their reliability evidence passed client_reliability retention before any award, so they never get their points (operator_block_points_expired)",
})

func init() {
	prometheus.MustRegister(operatorBlockPointsExpiredBlocksCounter)
}

// OperatorBlockPointsPlanId is the payment plan id of an operator block's
// points. It derives from the block, so every writer finds an earlier award by
// its payment_report primary key.
func OperatorBlockPointsPlanId(block int64) server.Id {
	sum := md5.Sum([]byte(operatorBlockPointsPlanIdNamespace + "/" + strconv.FormatInt(block, 10)))
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
// awards, ascending, of the due blocks that have neither an award nor an
// expiry record:
//   - after any award, every later due block without one, so a missed run
//     catches up
//   - before the first award, the due blocks from fromBlock, the block that
//     was open when the task was first scheduled: the blocks that completed
//     before this code ran are the backfill's, which needs an explicit go
//   - without either, only the latest due block
//
// An expiry record leaves the last award where it was: it only drops its
// block from the list.
func operatorBlockPointsAutomaticBlocks(due []int64, awarded map[int64]bool, expired map[int64]bool, fromBlock int64) []int64 {
	blocks := []int64{}
	lastAwarded := int64(0)
	for block := range awarded {
		if awarded[block] {
			lastAwarded = max(lastAwarded, block)
		}
	}
	for _, block := range due {
		if awarded[block] || expired[block] {
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

	// the window's share of a subsidy period (subsidy.yml days), at most 1,
	// which scales the points per period to the window
	TimeScale float64 `json:"time_scale"`
	// the bytes the provider networks served in the window, which share the
	// payout points, and the sweeps that carry them
	ByteCount  ByteCount `json:"byte_count"`
	SweepCount int64     `json:"sweep_count"`

	// the reliability window, in reliability blocks [min, max)
	ReliabilityMinBlockNumber      int64 `json:"reliability_min_block_number"`
	ReliabilityMaxBlockNumber      int64 `json:"reliability_max_block_number"`
	ReliabilityEffectiveBlockCount int64 `json:"reliability_effective_block_count"`
	ReliabilityNetworkCount        int   `json:"reliability_network_count"`

	// account_points_per_payout scaled to the window: the payout points the
	// networks share by bytes
	PayoutNanoPoints NanoPoints `json:"payout_nano_points"`
	PointsPerPayout  int        `json:"points_per_payout"`

	Payments []*OperatorBlockPayment `json:"payments"`
	Points   []*OperatorBlockPoint   `json:"points"`
}

// OperatorBlockPayment is one provider network's traffic in the window, which
// its payment row records. The row carries no USDC.
type OperatorBlockPayment struct {
	NetworkId server.Id `json:"network_id"`
	// the bytes it served, always positive
	PayoutByteCount ByteCount `json:"payout_byte_count"`
	// its earliest sweep in the window: the operator block it earns in
	MinSweepTime time.Time `json:"min_sweep_time"`
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

// The block's traffic past the cutoff: each provider network's bytes.
type operatorBlockTraffic struct {
	payees     map[server.Id]*operatorBlockPayee
	sweepCount int64
}

// One provider network's bytes in the window, and its earliest sweep.
type operatorBlockPayee struct {
	payoutByteCount ByteCount
	minSweepTime    time.Time
}

func newOperatorBlockTraffic() *operatorBlockTraffic {
	return &operatorBlockTraffic{
		payees: map[server.Id]*operatorBlockPayee{},
	}
}

// One read row: a provider network's sweeps in one read's sweep time.
type operatorBlockSweepRow struct {
	payeeNetworkId  server.Id
	payoutByteCount ByteCount
	minSweepTime    time.Time
	sweepCount      int64
}

// Adds a row to its provider network's bytes.
func (self *operatorBlockTraffic) add(row *operatorBlockSweepRow) {
	self.sweepCount += row.sweepCount
	payee, ok := self.payees[row.payeeNetworkId]
	if !ok {
		payee = &operatorBlockPayee{minSweepTime: row.minSweepTime}
		self.payees[row.payeeNetworkId] = payee
	}
	payee.payoutByteCount += row.payoutByteCount
	payee.minSweepTime = server.MinTime(payee.minSweepTime, row.minSweepTime)
}

// The sweeps no USDC payment owns (payment_id), with payout bytes
// (pointsPaidSweepSql, the definition of the operator blocks), of contracts
// that settled at or after the cutoff, with a sweep time in [$1, $2), per
// provider network. Neither the funding balance, whether it was paid, nor the
// sweep's revenue is read: the bytes alone weigh the points.
var operatorBlockPointsSweepSql = `
	SELECT
		sweep.network_id,
		SUM(sweep.payout_byte_count)::bigint,
		MIN(sweep.sweep_time),
		COUNT(*)
	FROM transfer_escrow_sweep AS sweep
	INNER JOIN transfer_contract AS contract ON
		contract.contract_id = sweep.contract_id
	WHERE
		$1 <= sweep.sweep_time AND
		sweep.sweep_time < $2 AND
		sweep.payment_id IS NULL AND
		` + pointsPaidSweepSql("sweep") + ` AND
		$3 <= contract.close_time AND
		contract.outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination')
	GROUP BY sweep.network_id
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
						&row.payoutByteCount,
						&row.minSweepTime,
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
// weight, as the payout plan read it after folding the window
// (readReliabilityWindowWeights). It is computed from read-only reads, so
// neither a dry run nor an award writes anything for it.
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

// The reliability window that starts at minBlockNumber is still inside
// client_reliability retention at now, so an award of it can still be
// computed. One predicate for the award (readOperatorBlockReliability), which
// fails closed past it, for a block's expiry (OperatorBlockPointsExpiryAt),
// and for the blocks whose sweeps the contract horizon reaper keeps
// (OperatorBlockPointsPendingWindows).
func operatorBlockReliabilityRetained(minBlockNumber int64, now time.Time) bool {
	return clientReliabilityRetentionCutoffBlock(now) <= minBlockNumber
}

// A completed block past its deadline: its window's first reliability block
// is older than the first one client_reliability retention keeps at the
// award clock, so no award of it can read all its evidence, and one fails
// closed (ErrOperatorBlockPointsReliabilityExpired). The deadline is the
// window's start plus ClientReliabilityRetention, at reliability block
// granularity: about three days after a whole block is due. Retention only
// moves forward, so an expiry is permanent. The daily task records it
// (operator_block_points_expired) and never awards the block; a commit of any
// award of a recorded block is refused, and no backfill can compute one.
type OperatorBlockPointsExpiry struct {
	Block       int64
	WindowStart time.Time
	WindowEnd   time.Time
	// the window's first reliability block, where an award reads from
	ReliabilityMinBlockNumber int64
	// the first reliability block retention keeps at ExpireTime, after the
	// window's first
	RetentionMinBlockNumber int64
	// the award clock that found the block past its deadline
	ExpireTime time.Time
}

// The block's expiry at the award clock now, or false while an award of it can
// still read its whole window, and for a block with no traffic past the
// cutoff. Pure.
func OperatorBlockPointsExpiryAt(block int64, cutoff time.Time, now time.Time) (*OperatorBlockPointsExpiry, bool) {
	start, end, ok := OperatorBlockPointsWindow(block, cutoff)
	if !ok {
		return nil, false
	}
	minBlockNumber, _ := operatorBlockReliabilityBlockWindow(start, end)
	if operatorBlockReliabilityRetained(minBlockNumber, now) {
		return nil, false
	}
	return &OperatorBlockPointsExpiry{
		Block:                     block,
		WindowStart:               start,
		WindowEnd:                 end,
		ReliabilityMinBlockNumber: minBlockNumber,
		RetentionMinBlockNumber:   clientReliabilityRetentionCutoffBlock(now),
		ExpireTime:                now,
	}, true
}

// Which of the blocks the daily task recorded expired.
func GetOperatorBlockPointsExpired(ctx context.Context, blocks []int64) map[int64]bool {
	expired := map[int64]bool{}
	if len(blocks) == 0 {
		return expired
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT block_number FROM operator_block_points_expired WHERE block_number = ANY($1::bigint[])`,
			blocks,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var block int64
				server.Raise(result.Scan(&block))
				expired[block] = true
			}
		})
	})
	return expired
}

// Records the block expired, once: under the awards' lock, and only while it
// has no award, so no block is both awarded and expired
// (CommitOperatorBlockPoints takes the same lock). recorded is true when this
// call wrote the record, and false when the block was recorded already or has
// an award.
func recordOperatorBlockPointsExpiry(ctx context.Context, expiry *OperatorBlockPointsExpiry) (recorded bool, returnErr error) {
	defer recoverOperatorBlockPointsError(&returnErr)
	planId := OperatorBlockPointsPlanId(expiry.Block)
	server.Tx(ctx, func(tx server.PgTx) {
		recorded = false
		server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, operatorBlockPointsLockKey))
		tag, err := tx.Exec(
			ctx,
			`
			INSERT INTO operator_block_points_expired (
				block_number,
				window_start,
				window_end,
				reliability_min_block_number,
				retention_min_block_number,
				expire_time,
				create_time
			)
			SELECT $1, $2, $3, $4, $5, $6, $7
			WHERE
				NOT EXISTS (SELECT 1 FROM payment_report WHERE payment_plan_id = $8) AND
				NOT EXISTS (SELECT 1 FROM account_payment WHERE payment_plan_id = $8)
			ON CONFLICT (block_number) DO NOTHING
			`,
			expiry.Block,
			expiry.WindowStart,
			expiry.WindowEnd,
			expiry.ReliabilityMinBlockNumber,
			expiry.RetentionMinBlockNumber,
			expiry.ExpireTime,
			server.NowUtc(),
			planId,
		)
		server.Raise(err)
		recorded = tag.RowsAffected() == 1
	}, server.TxReadCommitted)
	return recorded, nil
}

// The blocks with traffic past the cutoff that an award can still read,
// ascending, through the block after the open one. A block counts while its
// reliability was still retained when the oldest award that can still be
// reading started, OperatorBlockPointsMaxTime and an hour before now: that
// award checked retention at its start and reads for hours after, until its
// last check on the clock fails it closed. The block after the open one
// covers a sweep that commits in it while a caller still holds this list. Pure.
func operatorBlockPointsAwardableBlocks(cutoff time.Time, now time.Time) []int64 {
	oldestAwardStart := now.Add(-OperatorBlockPointsMaxTime - time.Hour)
	blocks := []int64{}
	for block := max(1, pointsBlockOf(cutoff)); block <= pointsBlockOf(now)+1; block += 1 {
		start, end, ok := OperatorBlockPointsWindow(block, cutoff)
		if !ok {
			continue
		}
		minBlockNumber, _ := operatorBlockReliabilityBlockWindow(start, end)
		if operatorBlockReliabilityRetained(minBlockNumber, oldestAwardStart) {
			blocks = append(blocks, block)
		}
	}
	return blocks
}

// The traffic windows, [starts[i], ends[i]), that an award can still read
// (ComputeOperatorBlockPoints): those of the awardable blocks
// (operatorBlockPointsAwardableBlocks) that have no award yet. An award reads
// the unpaid sweeps whose sweep time is in its window, and a sweep's time is
// its settlement, so a contract minted and closed long before can still have
// one there. Outside these windows a block is awarded, can never be awarded
// because its reliability is past retention (the award fails closed), or has
// no traffic past the cutoff.
func OperatorBlockPointsPendingWindows(ctx context.Context, cutoff time.Time, now time.Time) (starts []time.Time, ends []time.Time, returnErr error) {
	defer recoverOperatorBlockPointsError(&returnErr)
	blocks := operatorBlockPointsAwardableBlocks(cutoff, now)
	awarded := GetOperatorBlockPointsAwarded(ctx, blocks)
	starts = []time.Time{}
	ends = []time.Time{}
	for _, block := range blocks {
		if awarded[block] {
			continue
		}
		start, end, _ := OperatorBlockPointsWindow(block, cutoff)
		starts = append(starts, start)
		ends = append(ends, end)
	}
	return starts, ends, nil
}

// Reads the window's reliability with the plan's fold
// (readReliabilityWindowWeights), and multiplies each network's weight in a
// country by the country's multiplier.
func readOperatorBlockReliability(ctx context.Context, start time.Time, end time.Time, now time.Time) (*operatorBlockReliability, error) {
	minBlockNumber, maxBlockNumber := operatorBlockReliabilityBlockWindow(start, end)
	reliability := &operatorBlockReliability{
		minBlockNumber: minBlockNumber,
		maxBlockNumber: maxBlockNumber,
		networkWeights: map[server.Id]float64{},
	}
	checkRetention := func(at time.Time) error {
		if !operatorBlockReliabilityRetained(minBlockNumber, at) {
			return fmt.Errorf("%w: window starts at reliability block %d", ErrOperatorBlockPointsReliabilityExpired, minBlockNumber)
		}
		return nil
	}
	if err := checkRetention(now); err != nil {
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

	weights, err := readReliabilityWindowWeights(ctx, minBlockNumber, maxBlockNumber, degradedBlockNumbers, reliability.effectiveBlockCount)
	if err != nil {
		return nil, err
	}
	// the reads take hours, so retention is checked again on the clock, as
	// the contract horizon reaper judges the block (operatorBlockPointsAwardableBlocks)
	if err := checkRetention(server.NowUtc()); err != nil {
		return nil, err
	}
	if len(weights) == 0 {
		return reliability, nil
	}

	multipliers := map[server.Id]float64{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
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
	for _, weight := range weights {
		multiplier := 1.0
		if weight.key.hasCountry {
			if m, ok := multipliers[weight.key.countryLocationId]; ok {
				multiplier = m
			}
		}
		// the explicit conversion rounds the product, as SQL does, before the sum
		reliability.networkWeights[weight.key.networkId] += float64(weight.weight * multiplier)
	}
	return reliability, nil
}

// The reliability points of the window from the networks' multiplied weights,
// as a plan computed them (calculateReliabilityPayoutInTx):
// reliability_points_per_payout scaled to the window, in proportion to weight.
// A network of no weight, or whose share truncates to nothing, is absent.
func operatorBlockReliabilityPoints(
	networkWeights map[server.Id]float64,
	timeScale float64,
	subsidyConfig *SubsidyConfig,
) map[server.Id]NanoPoints {
	networkPoints := map[server.Id]NanoPoints{}

	networkIds := slices.SortedFunc(maps.Keys(networkWeights), server.Id.Cmp)
	totalReliabilityWeight := 0.0
	for _, networkId := range networkIds {
		totalReliabilityWeight += networkWeights[networkId]
	}
	if totalReliabilityWeight == 0 {
		return networkPoints
	}

	reliabilityPointsPerPayout := PointsToNanoPoints(float64(subsidyConfig.ReliabilityPointsPerPayout) * timeScale)
	for _, networkId := range networkIds {
		weight := networkWeights[networkId]
		if points := NanoPoints(float64(reliabilityPointsPerPayout) * float64(weight/totalReliabilityWeight)); 0 < points {
			networkPoints[networkId] = points
		}
	}
	return networkPoints
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
// nothing: one payment per provider network with bytes in the window, and the
// points of planPayoutPoints. The traffic it is given carries no revenue and
// no payer, so neither can weigh the points. A plan's withholding of small
// payments, its minimum epoch duration and its carried USDC bonuses were USDC
// rules: with no USDC, every provider with traffic in the block earns in the
// block.
func planOperatorBlockPoints(
	inputs *operatorBlockPointsInputs,
	traffic *operatorBlockTraffic,
	reliability *operatorBlockReliability,
) (*OperatorBlockPoints, error) {
	subsidyConfig := inputs.subsidyConfig
	planId := OperatorBlockPointsPlanId(inputs.block)
	award := &OperatorBlockPoints{
		Schema:        OperatorBlockPointsSchema,
		Block:         inputs.block,
		PaymentPlanId: planId,
		PolicySha256:  inputs.policySha256,
		Cutoff:        inputs.cutoff,
		WindowStart:   inputs.windowStart,
		WindowEnd:     inputs.windowEnd,
		// as a plan scaled its points to its epoch
		TimeScale:                      min(float64(inputs.windowEnd.Sub(inputs.windowStart))/float64(subsidyConfig.Duration()), 1.0),
		SweepCount:                     traffic.sweepCount,
		ReliabilityMinBlockNumber:      reliability.minBlockNumber,
		ReliabilityMaxBlockNumber:      reliability.maxBlockNumber,
		ReliabilityEffectiveBlockCount: reliability.effectiveBlockCount,
		ReliabilityNetworkCount:        len(reliability.networkWeights),
		PointsPerPayout:                subsidyConfig.AccountPointsPerPayout,
		Payments:                       []*OperatorBlockPayment{},
		Points:                         []*OperatorBlockPoint{},
	}

	networkByteCounts := map[server.Id]ByteCount{}
	for _, networkId := range slices.SortedFunc(maps.Keys(traffic.payees), server.Id.Cmp) {
		payee := traffic.payees[networkId]
		if payee.payoutByteCount <= 0 {
			continue
		}
		if math.MaxInt64-award.ByteCount < payee.payoutByteCount {
			return nil, fmt.Errorf("operator block %d bytes overflow", inputs.block)
		}
		award.ByteCount += payee.payoutByteCount
		networkByteCounts[networkId] = payee.payoutByteCount
		award.Payments = append(award.Payments, &OperatorBlockPayment{
			NetworkId:       networkId,
			PayoutByteCount: payee.payoutByteCount,
			MinSweepTime:    payee.minSweepTime,
		})
	}

	if 0 < award.TimeScale && 0 < award.ByteCount {
		award.PayoutNanoPoints = PointsToNanoPoints(float64(subsidyConfig.AccountPointsPerPayout) * award.TimeScale)
		rows := planPayoutPoints(&payoutPointsPlan{
			ctx:               context.Background(),
			subsidyConfig:     subsidyConfig,
			paymentPlanId:     planId,
			payoutNanoPoints:  award.PayoutNanoPoints,
			networkByteCounts: networkByteCounts,
			reliabilityPoints: operatorBlockReliabilityPoints(reliability.networkWeights, award.TimeScale, subsidyConfig),
			seekerHolders:     inputs.seekerHolders,
			referralParents:   operatorBlockReferralParents(inputs.networkReferrals),
			networkReferrals:  inputs.networkReferrals,
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
		"[obp]block %d [%s, %s): %d sweeps, %d providers, %d bytes, %d reliability networks, %d point rows, %d nano points, read in %s\n",
		block,
		start.Format(time.RFC3339),
		end.Format(time.RFC3339),
		award.SweepCount,
		len(award.Payments),
		award.ByteCount,
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
		if payment.PayoutByteCount <= 0 {
			return fmt.Errorf("operator block %d payment of %s carries no bytes", award.Block, payment.NetworkId)
		}
		if payment.MinSweepTime.Before(start) || !payment.MinSweepTime.Before(end) {
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
// nothing is written. A block the daily task recorded expired is refused
// (ErrOperatorBlockPointsExpired), even with an award computed before its
// deadline: an expired block is never awarded.
func CommitOperatorBlockPoints(ctx context.Context, award *OperatorBlockPoints) (committed bool, returnErr error) {
	defer recoverOperatorBlockPointsError(&returnErr)
	transition, err := server.LoadProviderPayoutEarningPolicy(ctx)
	if err != nil {
		return false, err
	}
	if err := validateOperatorBlockPoints(award, transition); err != nil {
		return false, err
	}

	expired := false
	server.Tx(ctx, func(tx server.PgTx) {
		committed = false
		expired = false
		server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, operatorBlockPointsLockKey))
		var awarded bool
		server.Raise(tx.QueryRow(
			ctx,
			`
			SELECT
				EXISTS (SELECT 1 FROM payment_report WHERE payment_plan_id = $1) OR
				EXISTS (SELECT 1 FROM account_payment WHERE payment_plan_id = $1),
				EXISTS (SELECT 1 FROM operator_block_points_expired WHERE block_number = $2)
			`,
			award.PaymentPlanId,
			award.Block,
		).Scan(&awarded, &expired))
		if awarded {
			// a block is never both (recordOperatorBlockPointsExpiry)
			expired = false
			return
		}
		if expired {
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
			// no payout weighs the points since v2: they follow bytes
			0,
			award.PayoutNanoPoints,
			float64(award.PointsPerPayout)*award.TimeScale,
			award.TimeScale,
			award.PointsPerPayout,
		))

		paymentIds := map[server.Id]server.Id{}
		for _, payment := range award.Payments {
			paymentIds[payment.NetworkId] = server.NewId()
		}
		// the schema records which rule awarded the block
		eventDetails, err := json.Marshal(map[string]any{
			"block":           award.Block,
			"payment_plan_id": award.PaymentPlanId,
			"policy_sha256":   award.PolicySha256,
			"schema":          award.Schema,
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
		// selection is one row per provider, its bytes at its earliest sweep,
		// and the window is inside one block
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
				batch.Queue(
					`INSERT INTO temp_account_payment (network_id, payout_byte_count, sweep_time) VALUES ($1, $2, $3)`,
					payment.NetworkId,
					payment.PayoutByteCount,
					payment.MinSweepTime,
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

	if expired {
		return false, fmt.Errorf("%w: block %d", ErrOperatorBlockPointsExpired, award.Block)
	}
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
// it committed. A block past its deadline at now is recorded expired, once,
// and passed over, so the later blocks are awarded as usual. Any other error
// stops the run at its block, and the task's retry starts again there: the
// task owns only the blocks after its last award, so a block passed over on a
// transient error would never be tried again. An award whose reads outlast
// its deadline fails closed on the clock; the retry finds it expired. Without
// an earning boundary it does nothing: payout plans award points.
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
	expired := GetOperatorBlockPointsExpired(ctx, due)
	blocks := operatorBlockPointsAutomaticBlocks(due, awarded, expired, fromBlock)
	glog.Infof("[obp]due blocks %v, awarded %v, expired %v, from block %d: awarding %v\n", due, slices.Sorted(maps.Keys(awarded)), slices.Sorted(maps.Keys(expired)), fromBlock, blocks)
	for _, block := range blocks {
		if expiry, ok := OperatorBlockPointsExpiryAt(block, transition.Cutoff, now); ok {
			recorded, err := recordOperatorBlockPointsExpiry(ctx, expiry)
			if err != nil {
				return awards, err
			}
			if recorded {
				operatorBlockPointsExpiredBlocksCounter.Inc()
				glog.Errorf(
					"[obp]block %d expired without its points: its window [%s, %s) reads from reliability block %d, and client_reliability retention keeps from %d at %s; recorded, never awarded\n",
					block,
					expiry.WindowStart.Format(time.RFC3339),
					expiry.WindowEnd.Format(time.RFC3339),
					expiry.ReliabilityMinBlockNumber,
					expiry.RetentionMinBlockNumber,
					now.Format(time.RFC3339),
				)
			}
			continue
		}
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
