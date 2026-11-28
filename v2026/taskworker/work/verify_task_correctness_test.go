// Enabled verification tasks must persist their source effects and recur.
package work

import (
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Exercise all four enabled bodies through the queue: expired/live trail
// selection, stats replacement on replay, proxy egress publication and retention.
func TestEnabledVerifyTasksCommitEffectsAndRecur(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		controller.SetStConfig(&controller.StConfig{Enabled: true})
		settings := model.DefaultVerifySettings()
		controller.SetVerifySettings(settings)
		defer func() { controller.SetVerifySettings(nil); controller.SetStConfig(nil) }()
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		now := server.NowUtc()
		trails := []*model.VerifyTrail{}
		for _, at := range []time.Time{now.Add(-settings.StepTimeout - settings.StepTimeoutGrace - time.Hour), now.Add(24 * time.Hour)} {
			ms := uint64(at.UnixMilli())
			trail := &model.VerifyTrail{TrailId: server.NewId(), ClientId: server.NewId(), Vpk: []byte(server.NewId().String()),
				ServerNonce: []byte("synthetic-nonce"), M: 3, ServerKeyId: 1, Status: model.VerifyTrailStatusActive,
				CreateMs: ms, ActivityMs: ms, Hops: []*model.VerifyTrailHop{{ClientId: server.NewId(), ConfirmedMs: ms, Seed: true}},
				Pending: &model.VerifyTrailHop{ClientId: server.NewId(), AssignedMs: ms, AssignN: 1}}
			model.CreateVerifyTrail(ctx, trail, `{}`, settings)
			trails = append(trails, trail)
		}
		statsId, proxyId := server.NewId(), server.NewId()
		statsAt := now.Truncate(settings.StatsPeriod).Add(-settings.StatsPeriod)
		model.RecordVerifyAssignment(ctx, statsId, statsAt, settings)
		model.RecordVerifyConfirmation(ctx, statsId, 23, statsAt, settings)
		model.SetProvide(ctx, proxyId, map[model.ProvideMode][]byte{model.ProvideModePublic: make([]byte, 32)})
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO proxy_client(proxy_id,client_id,instance_id,proxy_host,block,client_ipv4,proxy_client_json)
				VALUES($1,$1,$2,'synthetic.example','synthetic',3325256705,'{}')`, proxyId, server.NewId()))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO verify_provider_stats(period_start,period_end,client_id,assignments,confirmations)
				VALUES('2000-01-01','2000-01-02',$1,100,50)`, statsId))
		})
		for range 2 {
			for _, sample := range []asyncMaintenanceCase{
				newAsyncMaintenanceCase(SweepVerifyTrails, SweepVerifyTrailsPost, &SweepVerifyTrailsArgs{}),
				newAsyncMaintenanceCase(RollupVerifyProviderStats, RollupVerifyProviderStatsPost, &RollupVerifyProviderStatsArgs{}),
				newAsyncMaintenanceCase(RefreshVerifyProxyEgress, RefreshVerifyProxyEgressPost, &RefreshVerifyProxyEgressArgs{}),
				newAsyncMaintenanceCase(RemoveOldVerifyProviderStats, RemoveOldVerifyProviderStatsPost, &RemoveOldVerifyProviderStatsArgs{}),
			} {
				runProjectionAuditTask(t, owner, sample, true)
			}
			if row := model.GetVerifyTrailRow(ctx, trails[0].TrailId); row == nil || row.Status != model.VerifyTrailRowStatusExpired {
				t.Fatal("expired trail was not persisted", row)
			}
			if trail := model.GetVerifyTrail(ctx, trails[1].TrailId); trail == nil || trail.Status != model.VerifyTrailStatusActive {
				t.Fatal("live trail was expired", trail)
			}
			rows := model.GetVerifyProviderStats(ctx, statsId)
			if len(rows) != 1 || rows[0].Assignments != 1 || rows[0].Confirmations != 1 {
				t.Fatal("stats replay or retention incorrect", rows)
			}
			resolved := model.ResolveVerifyEgress(ctx, netip.MustParseAddr("198.51.100.1"), settings)
			if resolved == nil || *resolved != proxyId {
				t.Fatal("proxy egress was not published", resolved)
			}
		}
	})
}
