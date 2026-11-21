package controller

// Tier B of the verify cohort's rolling census (sn VERIFY-COHORT-V2.md §2.6;
// model/verify_cohort_ranked.go): at each board fold, one representative of
// each unclaimed network ranked in the top 400 in 3 of the last 4 folds, one
// per prefix, drawn after claimed fleets' members. The fold already holds every
// provider's network, eligibility and prefix pin, so the representatives are
// chosen from exactly what the board ranked. Outside the rolling census the set
// is cleared: new unclaimed providers can't enter the legacy census anyway.

import (
	"context"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Recomputes the ranked set after the board fold wrote its snapshot. A failure
// is logged and leaves the previous set to lapse at its ttl; the board is
// unaffected.
func snTopRefreshVerifyCohortRanked(ctx context.Context, settings *model.VerifySettings, projection *model.SnTopProjection, input *model.SnTopProjectionInput) {
	if settings == nil || settings.CohortSize <= 0 || settings.CohortDeploymentKey == "" || projection == nil || input == nil {
		return
	}
	server.HandleError(func() {
		if !model.VerifyCohortRollingMode(ctx, settings) {
			model.SetVerifyCohortRanked(ctx, settings.CohortDeploymentKey, nil)
			return
		}
		networks := make([]model.VerifyCohortRankedNetwork, 0, len(projection.Networks))
		for _, row := range projection.Networks {
			networks = append(networks, model.VerifyCohortRankedNetwork{NetworkId: row.NetworkId, Position: row.Position, Claimed: row.Claimed})
		}
		// a provider whose unit rule (a) withheld has no prefix here either
		providers := snTopVerifyCohortRankedProviders(input.Providers)
		clientIds := []server.Id{}
		for _, provider := range input.Providers {
			if provider.Eligible {
				clientIds = append(clientIds, provider.ClientId)
			}
		}
		recentTop := model.GetSnTopRecentTopNetworks(ctx, 4, model.VerifyCohortRankedPositions)
		counted, probation := model.GetVerifyCohortRankedLookups(ctx, settings, clientIds)
		representatives := model.SelectVerifyCohortRanked(networks, recentTop, providers, counted, probation)
		count := model.SetVerifyCohortRanked(ctx, settings.CohortDeploymentKey, representatives)
		glog.Infof("[sntop]verify cohort ranked set: %d representatives of the top %d\n", count, model.VerifyCohortRankedPositions)
	}, func(err error) {
		glog.Infof("[sntop]verify cohort ranked set refresh failed: %s\n", err)
	})
}
