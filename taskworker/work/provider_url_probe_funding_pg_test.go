package work

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func urlFundingSelectedGrants(t testing.TB) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "urnetwork_prober_grant_selection_total" {
			for _, metric := range family.Metric {
				for _, label := range metric.Label {
					if label.GetName() == "result" && label.GetValue() == "selected_first" {
						return metric.GetCounter().GetValue()
					}
				}
			}
		}
	}
	return 0 // the counter has no cell before the first allocation
}

// Persist the actual configured grant, then leave nine forecast-sized portions
// in unsettled escrow. Further origin/ahead/prefetch and companion reservations
// must use the remaining headroom of this same grant. A sibling shard is fully
// funded independently; no shared identity, grant top-up or cleanup shortcut is
// allowed. The large first reservation tests accounting, not controller limits.
func TestUrlProbeShardFundingPersistsPrivateHeadroomWithDeferredReclaim(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		args := providerEgressProbeArgs(defaultProviderEgressProbeSettings("funding.example"), 0)
		credit, err := providerUrlProbeShardCredit(args)
		if err != nil {
			t.Fatal(err)
		}
		key := model.ProberShardKey{TaskId: server.NewId(), Epoch: server.NewId(), ShardIndex: 0, ShardCount: 2}
		owner, err := model.BeginProberShard(ctx, key, credit, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		siblingKey := model.ProberShardKey{TaskId: server.NewId(), Epoch: server.NewId(), ShardIndex: 1, ShardCount: 2}
		sibling, err := model.BeginProberShard(ctx, siblingKey, credit, time.Hour)
		if err != nil || owner.NetworkId == sibling.NetworkId || owner.BalanceId == sibling.BalanceId {
			t.Fatal("sibling shard did not own a distinct account/grant", err)
		}
		identity, err := model.ProberShardIdentity(ctx, owner)
		if err != nil {
			t.Fatal(err)
		}
		credentials, err := newProviderEgressCredentials(identity)
		if err != nil {
			t.Fatal(err)
		}
		source := connect.Id(owner.ClientId)
		minted, err := credentials.AuthNetworkClient(ctx, &connect.AuthNetworkClientArgs{SourceClientId: &source})
		if err != nil {
			t.Fatal(err)
		}
		claims, err := session.ParseByJwtForAudience(ctx, minted.ByClientJwt, session.ByJwtAudienceApi)
		if err != nil || claims == nil || claims.ClientId == nil || claims.NetworkId != owner.NetworkId {
			t.Fatal("derived credential left its private network", err)
		}
		child := *claims.ClientId
		peer, peerNetwork := server.NewId(), server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client(client_id,network_id,active) VALUES($1,$2,true)`, peer, peerNetwork))
		})
		details := model.GetProvideRelationshipDetails(ctx, child, peer)
		if details.SourceOwner != model.NetworkClientSourceOwnerEgressProber || details.SourceLifecycle != model.NetworkClientLifecycleActiveDerived {
			t.Fatal("derived private client lost prober ownership classification")
		}
		before := urlFundingSelectedGrants(t)
		reserved := credit - credit/10
		old, err := model.CreateTransferEscrow(ctx, owner.NetworkId, child, peerNetwork, peer, reserved)
		if err != nil || old == nil || len(old.Balances) != 1 || old.Balances[0].BalanceId != owner.BalanceId {
			t.Fatal("one private grant could not retain unreclaimed headroom", err)
		}
		standard := model.ByteCount(connect.DefaultContractManagerSettings().StandardContractTransferByteCount)
		// Use another peer for ordinary ramp clamping, independently of the large
		// accounting-only reservation above.
		nextPeer := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client(client_id,network_id,active) VALUES($1,$2,true)`, nextPeer, peerNetwork))
		})
		for range 3 {
			forward, err := model.CreateTransferEscrow(ctx, owner.NetworkId, child, peerNetwork, nextPeer, standard)
			if err != nil || forward == nil || len(forward.Balances) != 1 || forward.Balances[0].BalanceId != owner.BalanceId {
				t.Fatal("origin/renewal/prefetch lost its private grant", err)
			}
			back, err := model.CreateCompanionTransferEscrow(ctx, peerNetwork, nextPeer, owner.NetworkId, child, 2*standard, time.Hour)
			if err != nil || back == nil || back.TransferByteCount != standard || len(back.Balances) != 1 || back.Balances[0].BalanceId != owner.BalanceId {
				t.Fatal("companion failed to use the shard's origin ramp", err)
			}
			reserved += 2 * standard
		}
		if got := urlFundingSelectedGrants(t) - before; got != 7 {
			t.Fatalf("derived client did not use indexed internal grant selection: %v", got)
		}
		if cross, err := model.CreateTransferEscrow(ctx, owner.NetworkId, child, sibling.NetworkId, sibling.ClientId, standard); err == nil || cross != nil {
			t.Fatal("two private shards shared a contract")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var grants, shared int
			var amount, unsettled, siblingAmount model.ByteCount
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM transfer_balance WHERE network_id=$1),
				(SELECT count(*) FROM prober_identity),
				(SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2),
				(SELECT COALESCE(sum(balance_byte_count),0) FROM transfer_escrow WHERE balance_id=$2 AND NOT settled),
				(SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$3)`, owner.NetworkId, owner.BalanceId, sibling.BalanceId).Scan(&grants, &shared, &amount, &unsettled, &siblingAmount))
			if grants != 1 || shared != 0 || amount != credit || unsettled != reserved || siblingAmount != credit {
				t.Fatalf("private funding changed: grants=%d shared=%d amount=%d unsettled=%d sibling=%d", grants, shared, amount, unsettled, siblingAmount)
			}
		})
		if err := model.DrainProberShard(ctx, key); err != nil {
			t.Fatal(err)
		}
		if removed, err := model.ReapProberShard(ctx, key); err != nil || removed {
			t.Fatal("cleanup erased unsettled reservations", err)
		}
		if err := model.DrainProberShard(ctx, siblingKey); err != nil {
			t.Fatal(err)
		}
		if removed, err := model.ReapProberShard(ctx, siblingKey); err != nil || !removed {
			t.Fatal("unrelated debt-free shard cleanup blocked", err)
		}
	})
}
