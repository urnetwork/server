package controller

// The epoch earnings email follows measurability as GET /sn/head does (owner
// decision 2026-11-17). Boards and networks are synthetic.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The epoch email's Top 200 line came from the live estimate, so every
// network it ranked inside the cutoff read "Top 200 · you qualify" with a
// Claim your head spot button, measurable or not. Both networks here provide
// on an egress of their own, which that estimate ranks inside the cutoff;
// the board measures only one of them, and only that one qualifies.
func TestStEpochEarningsTop200FollowsMeasurabilityDb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newSnTopDbFixture(t, ctx)
		// the live estimate is cached per process: compute it from this database
		resetRanking := func() {
			snHeadRankingCache.lock.Lock()
			snHeadRankingCache.ranking = nil
			snHeadRankingCache.lock.Unlock()
		}
		resetRanking()
		t.Cleanup(resetRanking)

		full := fixture.end.Add(-time.Minute)
		measurableId, _ := fixture.network(t, "m")
		cohortId, _ := fixture.network(t, "c")
		fixture.provider(t, measurableId, "198.51.100.41", full)
		fixture.provider(t, cohortId, "198.51.100.9", full)
		snHeadDbBoard(t, ctx, []*model.SnTopNetworkProjection{
			{NetworkId: measurableId, Position: 1, ProjectedScore: 2, Label: model.SnTopLabelEstimate, Measurable: true, VerifyEligible: true, ClaimPrompt: true},
			{NetworkId: cohortId, Position: 2, ProjectedScore: 1, Label: model.SnTopLabelNotMeasurable, VerifyEligible: true, NotMeasurableReason: model.SnTopReasonNotInCohort},
		})

		// both networks are recipients of the epoch's email
		key := fixture.cfg.DeploymentKey()
		model.SetStPayoutLeaves(ctx, key, snTopDbEpoch, fixture.cfg.NoId, []*model.StPayoutLeaf{
			{Epoch: snTopDbEpoch, NoId: fixture.cfg.NoId, NetworkId: measurableId, Coldkey: ledgerV2Coldkey(1), ShareBps: 5000, LeafIndex: 0},
			{Epoch: snTopDbEpoch, NoId: fixture.cfg.NoId, NetworkId: cohortId, Coldkey: ledgerV2Coldkey(2), ShareBps: 5000, LeafIndex: 1},
		})
		templates := stEpochEarningsTemplates(ctx, key, snTopDbEpoch, &model.StEpoch{Epoch: snTopDbEpoch, Status: model.StEpochStatusFinalized})

		measurable, cohort := templates[measurableId], templates[cohortId]
		if measurable == nil || cohort == nil {
			t.Fatalf("templates %+v", templates)
		}
		if cohort.Top200Eligible || cohort.Top200Text() != "" {
			t.Errorf("a network outside the cohort qualifies: eligible %v, %q", cohort.Top200Eligible, cohort.Top200Text())
		}
		if !measurable.Top200Eligible || measurable.Top200Rank != 1 || measurable.Top200Text() != "Top 200 · you qualify" {
			t.Errorf("the measurable network: eligible %v, rank %d, %q", measurable.Top200Eligible, measurable.Top200Rank, measurable.Top200Text())
		}
	})
}
