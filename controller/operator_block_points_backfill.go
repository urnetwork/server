package controller

// The operator block points backfill (bringyourctl account-points
// backfill-blocks): the points of the completed blocks past the earning
// boundary that the daily task did not award, because they completed before
// it ran. Owner decision 2026-10-28.
//
// The dry run computes every selected block's award with read-only statements
// (model.ComputeOperatorBlockPoints) and writes nothing to the database. It
// prints the awards and a digest, and saves them to a file. A commit takes the
// file and the digest the dry run printed, refuses any other content, and
// commits exactly those awards. A block that already has its points, from
// the task or an earlier commit, is skipped, so a commit can run again.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

const OperatorBlockPointsBackfillSchema = "urnetwork-operator-block-points-backfill-v1"

var ErrOperatorBlockPointsBackfillDigest = errors.New("the operator block points file does not have the expected digest; run the dry run again")

type OperatorBlockPointsBackfill struct {
	Schema       string    `json:"schema"`
	CreateTime   time.Time `json:"create_time"`
	Cutoff       time.Time `json:"cutoff"`
	PolicySha256 string    `json:"policy_sha256"`
	// the end of the last subsidy epoch of the payout plans: traffic before it
	// has its points from the plans
	LegacyEpochEnd *time.Time `json:"legacy_epoch_end,omitempty"`
	// the completed blocks past the boundary, and those that have their points
	DueBlocks     []int64                      `json:"due_blocks"`
	AwardedBlocks []int64                      `json:"awarded_blocks"`
	Awards        []*model.OperatorBlockPoints `json:"awards"`
	// model.OperatorBlockPointsDigest of the awards
	Digest string `json:"digest"`
}

// ParseOperatorBlocks parses a comma-separated block list.
func ParseOperatorBlocks(value string) ([]int64, error) {
	blocks := []int64{}
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var block int64
		if _, err := fmt.Sscan(part, &block); err != nil || block < 1 {
			return nil, fmt.Errorf("invalid operator block %q", part)
		}
		blocks = append(blocks, block)
	}
	slices.Sort(blocks)
	return slices.Compact(blocks), nil
}

// PlanOperatorBlockPointsBackfill is the dry run: the awards of the selected
// blocks that do not have their points yet, by default every completed block
// past the boundary. It only reads.
func PlanOperatorBlockPointsBackfill(ctx context.Context, blocks []int64, now time.Time) (*OperatorBlockPointsBackfill, error) {
	transition, err := server.LoadProviderPayoutEarningPolicy(ctx)
	if err != nil {
		return nil, err
	}
	if transition == nil {
		return nil, model.ErrOperatorBlockPointsNoBoundary
	}
	due := model.OperatorBlockPointsDueBlocks(transition.Cutoff, now)
	if len(blocks) == 0 {
		blocks = due
	}
	for _, block := range blocks {
		if !slices.Contains(due, block) {
			return nil, fmt.Errorf("operator block %d is not a completed block past the earning boundary (due: %v)", block, due)
		}
	}
	backfill := &OperatorBlockPointsBackfill{
		Schema:        OperatorBlockPointsBackfillSchema,
		CreateTime:    now,
		Cutoff:        transition.Cutoff,
		PolicySha256:  transition.ConfigSha256,
		DueBlocks:     due,
		AwardedBlocks: []int64{},
		Awards:        []*model.OperatorBlockPoints{},
	}
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT MAX(end_time) FROM subsidy_payment`).Scan(&backfill.LegacyEpochEnd))
	})
	awarded := model.GetOperatorBlockPointsAwarded(ctx, due)
	backfill.AwardedBlocks = slices.Sorted(maps.Keys(awarded))
	for _, block := range blocks {
		if awarded[block] {
			continue
		}
		award, err := model.ComputeOperatorBlockPoints(ctx, model.EnvSubsidyConfig(), block, now)
		if err != nil {
			return nil, fmt.Errorf("operator block %d: %w", block, err)
		}
		backfill.Awards = append(backfill.Awards, award)
	}
	backfill.Digest = model.OperatorBlockPointsDigest(backfill.Awards)
	return backfill, nil
}

// CommitOperatorBlockPointsBackfill commits the awards of a dry run whose
// digest is expect, and returns the blocks it awarded and the blocks it found
// awarded already.
func CommitOperatorBlockPointsBackfill(
	clientSession *session.ClientSession,
	backfill *OperatorBlockPointsBackfill,
	expect string,
) (committed []int64, skipped []int64, returnErr error) {
	ctx := clientSession.Ctx
	if backfill.Schema != OperatorBlockPointsBackfillSchema || expect == "" ||
		backfill.Digest != expect || model.OperatorBlockPointsDigest(backfill.Awards) != expect {
		return nil, nil, ErrOperatorBlockPointsBackfillDigest
	}
	committed = []int64{}
	skipped = []int64{}
	awards := slices.Clone(backfill.Awards)
	slices.SortFunc(awards, func(a *model.OperatorBlockPoints, b *model.OperatorBlockPoints) int {
		return int(a.Block - b.Block)
	})
	defer func() {
		if 0 < len(committed) {
			server.Tx(ctx, func(tx server.PgTx) {
				TriggerRebuildPointsLeaderboardInTx(clientSession, tx)
			})
		}
	}()
	for _, award := range awards {
		awarded, err := model.CommitOperatorBlockPoints(ctx, award)
		if err != nil {
			return committed, skipped, fmt.Errorf("operator block %d: %w", award.Block, err)
		}
		if awarded {
			committed = append(committed, award.Block)
		} else {
			skipped = append(skipped, award.Block)
		}
	}
	return committed, skipped, nil
}

// The points of one network in an award, by event.
type operatorBlockNetworkPoints struct {
	networkId      server.Id
	payment        *model.OperatorBlockPayment
	payout         model.NanoPoints
	reliability    model.NanoPoints
	multiplier     model.NanoPoints
	linkedAccounts model.NanoPoints
	total          model.NanoPoints
}

func operatorBlockNetworksPoints(award *model.OperatorBlockPoints) []*operatorBlockNetworkPoints {
	networks := map[server.Id]*operatorBlockNetworkPoints{}
	network := func(networkId server.Id) *operatorBlockNetworkPoints {
		points, ok := networks[networkId]
		if !ok {
			points = &operatorBlockNetworkPoints{networkId: networkId}
			networks[networkId] = points
		}
		return points
	}
	for _, payment := range award.Payments {
		network(payment.NetworkId).payment = payment
	}
	for _, point := range award.Points {
		points := network(point.NetworkId)
		switch model.AccountPointEvent(point.Event) {
		case model.AccountPointEventPayout:
			points.payout += point.PointValue
		case model.AccountPointEventReliability:
			points.reliability += point.PointValue
		case model.AccountPointEventPayoutMultiplier:
			points.multiplier += point.PointValue
		case model.AccountPointEventPayoutLinkedAccount:
			points.linkedAccounts += point.PointValue
		}
		points.total += point.PointValue
	}
	sorted := slices.Collect(maps.Values(networks))
	slices.SortFunc(sorted, func(a *operatorBlockNetworkPoints, b *operatorBlockNetworkPoints) int {
		if a.total != b.total {
			if b.total < a.total {
				return -1
			}
			return 1
		}
		return a.networkId.Cmp(b.networkId)
	})
	return sorted
}

// Points with their six decimal places (1 point is 1,000,000 nano points).
func operatorBlockPointsText(nanoPoints model.NanoPoints) string {
	return fmt.Sprintf("%.6f", float64(nanoPoints)/1_000_000)
}

// Text is the dry run's report: per block its window and computation, its
// points by event, and every network's points.
func (self *OperatorBlockPointsBackfill) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "operator block points backfill (%s)\n", self.Schema)
	fmt.Fprintf(&b, "earning boundary: cutoff %s, sn.yml sha256 %s\n", self.Cutoff.Format(time.RFC3339Nano), self.PolicySha256)
	if self.LegacyEpochEnd != nil {
		fmt.Fprintf(&b, "payout plans' last subsidy epoch ends %s: traffic before it has its points from the plans\n", self.LegacyEpochEnd.Format(time.RFC3339Nano))
	}
	fmt.Fprintf(&b, "completed blocks past the boundary: %v; with points already: %v\n", self.DueBlocks, self.AwardedBlocks)
	totalNanoPoints := model.NanoPoints(0)
	networks := map[server.Id]bool{}
	for _, award := range self.Awards {
		fmt.Fprintf(&b, "\nblock %d [%s, %s) plan %s\n",
			award.Block,
			award.WindowStart.Format(time.RFC3339),
			award.WindowEnd.Format(time.RFC3339),
			award.PaymentPlanId,
		)
		fmt.Fprintf(&b, "  traffic: %d sweeps, %d providers, %d paying networks (%d active, %d paid), %s paid + %s unpaid\n",
			award.SweepCount,
			len(award.Payments),
			award.TrafficUserCount,
			award.ActiveUserCount,
			award.PaidUserCount,
			model.ByteCountHumanReadable(award.NetPayoutByteCountPaid),
			model.ByteCountHumanReadable(award.NetPayoutByteCountUnpaid),
		)
		fmt.Fprintf(&b, "  computed payout (weighs the points; no USDC): $%.6f, subsidy scale %.6f, subsidy $%.6f, subscription revenue $%.6f\n",
			model.NanoCentsToUsd(award.TotalComputedPayout),
			award.SubsidyScale,
			award.SubsidyNetPayoutUsd,
			model.NanoCentsToUsd(award.NetRevenue),
		)
		fmt.Fprintf(&b, "  reliability: blocks [%d, %d), %d effective, %d networks\n",
			award.ReliabilityMinBlockNumber,
			award.ReliabilityMaxBlockNumber,
			award.ReliabilityEffectiveBlockCount,
			award.ReliabilityNetworkCount,
		)
		events := award.EventNanoPoints()
		fmt.Fprintf(&b, "  points: payout %s, reliability %s, seeker %s, referral %s, total %s in %d rows\n",
			operatorBlockPointsText(events[string(model.AccountPointEventPayout)]),
			operatorBlockPointsText(events[string(model.AccountPointEventReliability)]),
			operatorBlockPointsText(events[string(model.AccountPointEventPayoutMultiplier)]),
			operatorBlockPointsText(events[string(model.AccountPointEventPayoutLinkedAccount)]),
			operatorBlockPointsText(award.TotalNanoPoints()),
			len(award.Points),
		)
		fmt.Fprintf(&b, "  network_id\tpayout_bytes\tcomputed_payout_usd\tpayout\treliability\tseeker\treferral\ttotal\n")
		for _, points := range operatorBlockNetworksPoints(award) {
			bytes := model.ByteCount(0)
			computedPayout := model.NanoCents(0)
			if points.payment != nil {
				bytes = points.payment.PayoutByteCount
				computedPayout = points.payment.ComputedPayout
			}
			fmt.Fprintf(&b, "  %s\t%d\t%.9f\t%s\t%s\t%s\t%s\t%s\n",
				points.networkId,
				bytes,
				model.NanoCentsToUsd(computedPayout),
				operatorBlockPointsText(points.payout),
				operatorBlockPointsText(points.reliability),
				operatorBlockPointsText(points.multiplier),
				operatorBlockPointsText(points.linkedAccounts),
				operatorBlockPointsText(points.total),
			)
			if 0 < points.total {
				networks[points.networkId] = true
			}
		}
		totalNanoPoints += award.TotalNanoPoints()
	}
	fmt.Fprintf(&b, "\ntotal: %d blocks to award, %s points for %d networks\n", len(self.Awards), operatorBlockPointsText(totalNanoPoints), len(networks))
	fmt.Fprintf(&b, "digest: %s\n", self.Digest)
	return b.String()
}
