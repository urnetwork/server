// Rule (a) of the SN Top board end to end (sn_top_admission.go,
// sn_top_shadow.go, owner decisions 2026-11-20): the fold reads the export's
// admission set from Redis. Before the rule's first settlement epoch the live
// board ranks as before, the shadow board ranks only admitted providers, and
// GET /sn/top?board=shadow and GET /sn/top/me show it with each network's
// reasons; from the first epoch the live board ranks under the rule and a
// fold without a fresh set skips the tempo. Before the rule the fold counted
// every pinned provider. Fixtures use RFC 5737 documentation addresses.
package controller

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Two networks of one provider each; the export decided the second's
// provider neither Speed nor Quality, in no udp-off country.
func snTopAdmissionDbNetworks(t testing.TB, ctx context.Context, fixture *snTopDbFixture) (admittedNetwork server.Id, withheldNetwork server.Id, withheld server.Id, withheldSession *session.ClientSession) {
	full := fixture.end.Add(-time.Minute)
	admittedNetwork, _ = fixture.network(t, "a")
	withheldNetwork, withheldSession = fixture.network(t, "w")
	admitted := fixture.provider(t, admittedNetwork, "198.51.100.9", full)
	withheld = fixture.provider(t, withheldNetwork, "198.51.100.41", full)
	model.Testing_AddVerifyCohortMembers(ctx, fixture.cfg.DeploymentKey(), fixture.epoch, []server.Id{admitted, withheld})
	fixture.admit(withheld, false)
	return
}

func TestComputeSnTopSnapshotShadowBoardBeforeTheRuleDb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newSnTopDbFixtureAtEpoch(t, ctx, model.SnTopUnitRuleFirstEpoch-1)
		anonymous := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer anonymous.Cancel()
		_, withheldNetwork, withheld, withheldSession := snTopAdmissionDbNetworks(t, ctx, fixture)

		result, err := ComputeSnTopSnapshot(&ComputeSnTopSnapshotArgs{}, anonymous)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Skipped, "")
		// the live board ranks both networks, as before the rule
		connect.AssertEqual(t, result.TotalRanked, 2)

		// the shadow board ranks only the admitted one, beside its live place
		shadow, err := GetSnTop(&SnTopArgs{Board: snTopBoardShadow}, anonymous)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, shadow.Board, snTopBoardShadow)
		connect.AssertEqual(t, shadow.RuleFirstEpoch, model.SnTopUnitRuleFirstEpoch)
		connect.AssertEqual(t, shadow.Snapshot.TotalRanked, 1)
		connect.AssertEqual(t, len(shadow.Rows), 1)
		connect.AssertEqual(t, shadow.Rows[0].Position, 1)
		connect.AssertEqual(t, shadow.Rows[0].Prefixes, 1)
		connect.AssertEqual(t, 0 < *shadow.Rows[0].LivePosition, true)

		// the withheld network's owner reads which provider is out and why
		me, err := GetSnTopMe(withheldSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, me.Me.Units.RuleLive, false)
		connect.AssertEqual(t, me.Me.Units.CountedProviders, 0)
		connect.AssertEqual(t, me.Me.Units.WithheldProviders, []model.SnTopProviderUnit{{ClientId: withheld, Reason: model.SnTopUnitReasonUrl}})
		connect.AssertEqual(t, me.Me.Shadow == nil, true)
		connect.AssertEqual(t, 0 < me.Me.Row.Position, true)
		_ = withheldNetwork

		// a stalled export before the rule: the live board still folds, the
		// shadow does not move
		model.Testing_SetSnTopAdmission(ctx, &model.SnTopAdmission{
			MeasuredAt: server.NowUtc().Add(-model.SnTopAdmissionMaxAge - time.Minute),
			Providers:  fixture.admission,
		})
		later := fixture.anchor.Add(model.SnTopTempoDuration)
		next, err := computeSnTopSnapshotAt(&ComputeSnTopSnapshotArgs{}, anonymous, later)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, next.Skipped, "")
		board, err := model.GetSnTopShadowBoard(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, board.TempoIndex, result.TempoIndex)
	})
}

func TestComputeSnTopSnapshotRanksUnderTheRuleDb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newSnTopDbFixtureAtEpoch(t, ctx, model.SnTopUnitRuleFirstEpoch)
		anonymous := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer anonymous.Cancel()
		admittedNetwork, withheldNetwork, withheld, withheldSession := snTopAdmissionDbNetworks(t, ctx, fixture)

		result, err := ComputeSnTopSnapshot(&ComputeSnTopSnapshotArgs{}, anonymous)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Skipped, "")
		connect.AssertEqual(t, result.EligibleProviders, 2)
		connect.AssertEqual(t, result.TotalRanked, 1)

		latest := model.GetLatestSnTopSnapshot(ctx)
		connect.AssertEqual(t, latest == nil, false)
		row := model.GetSnTopNetworkRow(ctx, latest.SnapshotId, admittedNetwork)
		connect.AssertEqual(t, row == nil, false)
		connect.AssertEqual(t, row.Position, 1)
		connect.AssertEqual(t, row.Prefixes, 1)
		// the withheld network has no unit, so no ranked row
		if withheldRow := model.GetSnTopNetworkRow(ctx, latest.SnapshotId, withheldNetwork); withheldRow != nil {
			connect.AssertEqual(t, withheldRow.Position, 0)
			connect.AssertEqual(t, withheldRow.Prefixes, 0)
		}
		me, err := GetSnTopMe(withheldSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, me.Me.Units.RuleLive, true)
		connect.AssertEqual(t, me.Me.Units.WithheldByReason, map[string]int{model.SnTopUnitReasonUrl: 1})
		connect.AssertEqual(t, me.Me.Units.WithheldProviders[0].ClientId, withheld)

		// a stalled export once the rule is live: the fold skips the tempo
		model.Testing_SetSnTopAdmission(ctx, &model.SnTopAdmission{
			MeasuredAt: server.NowUtc().Add(-model.SnTopAdmissionMaxAge - time.Minute),
			Providers:  fixture.admission,
		})
		next, err := computeSnTopSnapshotAt(&ComputeSnTopSnapshotArgs{}, anonymous, fixture.anchor.Add(model.SnTopTempoDuration))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, next.Skipped, snTopSkippedAdmissionStale)
	})
}
