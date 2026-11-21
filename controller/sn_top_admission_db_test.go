// Rule (a) of the SN Top board end to end (sn_top_admission.go, owner decision
// 2026-11-20): the fold reads the export's admission set from Redis and ranks a
// network only on the prefixes of its admitted providers. A network whose only
// provider was not admitted keeps its provider verify eligible but shows no
// unit; without a fresh set the fold skips the tempo. Before the rule the
// fold counted every pinned provider. Fixtures use RFC 5737 documentation
// addresses.
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

func TestComputeSnTopSnapshotWithholdsUnadmittedProvidersDb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newSnTopDbFixture(t, ctx)
		anonymous := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer anonymous.Cancel()

		full := fixture.end.Add(-time.Minute)
		admittedNetwork, _ := fixture.network(t, "a")
		withheldNetwork, _ := fixture.network(t, "w")
		admitted := fixture.provider(t, admittedNetwork, "198.51.100.9", full)
		withheld := fixture.provider(t, withheldNetwork, "198.51.100.41", full)
		model.Testing_AddVerifyCohortMembers(ctx, fixture.cfg.DeploymentKey(), snTopDbEpoch, []server.Id{admitted, withheld})
		// the export decided the second network's provider neither Speed
		// nor Quality, and it is in no udp-off country
		fixture.admit(withheld, false)

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
	})
}

func TestComputeSnTopSnapshotSkipsWithoutFreshAdmissionDb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newSnTopDbFixture(t, ctx)
		anonymous := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer anonymous.Cancel()

		networkId, _ := fixture.network(t, "a")
		fixture.provider(t, networkId, "198.51.100.9", fixture.end.Add(-time.Minute))
		// the export stalled: its newest set is older than the board reads
		model.Testing_SetSnTopAdmission(ctx, &model.SnTopAdmission{
			MeasuredAt: server.NowUtc().Add(-model.SnTopAdmissionMaxAge - time.Minute),
			Providers:  fixture.admission,
		})

		result, err := ComputeSnTopSnapshot(&ComputeSnTopSnapshotArgs{}, anonymous)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Skipped, snTopSkippedAdmissionStale)
		connect.AssertEqual(t, model.GetLatestSnTopSnapshot(ctx) == nil, true)
	})
}
