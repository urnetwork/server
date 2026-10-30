package controller

// Recovery replays retained intervals in order; a later empty interval must
// not erase contributions that arrived while the snapshot task was offline.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Real snapshot writes carry each missed tempo's evidence into the next fold.
func TestComputeSnTopSnapshotReplaysMissedTemposDb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newSnTopDbFixture(t, ctx)
		networkId, clientSession := fixture.network(t, "replay")
		defer clientSession.Cancel()
		anonymous := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer anonymous.Cancel()
		clientId := fixture.provider(t, networkId, "198.51.100.9", fixture.end.Add(-time.Minute))
		anchor := snTopTempoAnchor(ctx)
		priorTempo := anchor.TempoOfBlock(snTopDbEpochStart + 360)
		prior := &model.SnTopSnapshot{
			SnapshotId: server.NewId(), TempoIndex: priorTempo.Index, TempoSource: priorTempo.Source,
			Block: priorTempo.EndBlock, TempoStartTime: priorTempo.StartTime, TempoEndTime: priorTempo.EndTime,
			ComputedAt: priorTempo.EndTime, SettlementEpoch: priorTempo.SettlementEpoch,
			HeadSeats: model.SnTopHeadSeats,
		}
		model.WriteSnTopSnapshot(ctx, prior, &model.SnTopProjection{
			NetworkStates: map[server.Id]model.SnTopUnitState{
				networkId: {Ema: 1, SoloEma: 1, LastTempoIndex: priorTempo.Index},
			},
		})
		for index, count := range []int{2, 0, 4} {
			tempo := anchor.TempoOfBlock(priorTempo.EndBlock + uint64(index)*model.SnTopTempoBlocks)
			hops := []*model.SnTrailHop{}
			for hop := 0; hop < count; hop++ {
				hops = append(hops, &model.SnTrailHop{
					TrailId: server.NewId(), HopIndex: 1, ClientId: clientId, NetworkId: &networkId,
					PrefixHash: [32]byte{byte(index + 1), byte(hop + 1)}, Complete: true,
					ConfirmTime: tempo.StartTime.Add(time.Minute),
				})
			}
			model.Testing_InsertSnTrailHops(ctx, hops)
		}

		for index, want := range []float64{1.25, 0.9375, 1.703125} {
			result, err := ComputeSnTopSnapshot(&ComputeSnTopSnapshotArgs{}, anonymous)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Skipped, "")
			connect.AssertEqual(t, result.TempoIndex, priorTempo.Index+int64(index)+1)
			states, _ := model.GetSnTopEmaState(ctx)
			snTopDbNear(t, states[networkId].Ema, want)
			connect.AssertEqual(t, states[networkId].LastTempoIndex, result.TempoIndex)
		}
		again, err := ComputeSnTopSnapshot(&ComputeSnTopSnapshotArgs{}, anonymous)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, again.Skipped, "folded")
		// If retention removes the boundary, surviving EMA values cannot jump
		// from an unknown predecessor directly to today's completed interval.
		before, _ := model.GetSnTopEmaState(ctx)
		server.Tx(ctx, func(tx server.PgTx) { server.RaisePgResult(tx.Exec(ctx, `DELETE FROM sn_top_snapshot`)) })
		missing, err := ComputeSnTopSnapshot(&ComputeSnTopSnapshotArgs{}, anonymous)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, missing.Skipped, "checkpoint_missing")
		after, _ := model.GetSnTopEmaState(ctx)
		connect.AssertEqual(t, after[networkId].Ema, before[networkId].Ema)
	})
}
