package model

import (
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// The daily free grant reaches every ordinary network but no prober shard's
// private network, live or drained: the shard is funded only by its own
// credit, and a second balance there would hold the shard's cleanup forever.
func TestProberShardNetworksGetNoFreeGrant(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		live := shardTestOwner(t, ctx, shardTestKey(0))
		drained := shardTestOwner(t, ctx, shardTestKey(1))
		server.Raise(DrainProberShard(ctx, drained.Key))
		ordinaryId := server.NewId()
		Testing_CreateNetwork(ctx, ordinaryId, "shard-grant-"+ordinaryId.String()[:8], server.NewId())

		startTime, endTime := FreeGrantWindow(server.NowUtc())
		added := AddFreeTransferBalanceToAllNetworks(ctx, startTime, endTime, ByteCount(1024))
		connect.AssertEqual(t, added[ordinaryId], ByteCount(1024))
		for _, owner := range []*ProberShardOwner{live, drained} {
			_, granted := added[owner.NetworkId]
			connect.AssertEqual(t, granted, false)
			if _, _, balances := shardTestRows(t, ctx, owner); balances != 1 {
				t.Fatalf("shard network %s holds %d balances, want only its own credit", owner.NetworkId, balances)
			}
		}

		// with no second balance the drained shard's cleanup proceeds
		if deleted, err := ReapProberShard(ctx, drained.Key); err != nil || !deleted {
			t.Fatalf("drained shard was held after the free grant: deleted=%t err=%v", deleted, err)
		}
	})
}
