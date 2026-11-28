package session

import (
	"errors"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// S2 N5: a client whose ancestor chain no longer reaches its root resolved no
// root when it was deactivated, so no tombstone was written, and the
// projection made while the chain was whole kept accepting it for up to an
// hour. The chain top -> middle -> leaf is projected, the middle row is
// deleted (as the stale-child reap deletes a parent in an earlier batch than
// its child), and the leaf is deactivated: every shard now refuses it.
func TestRevocationRecordRefusesAClientWhoseChainBroke(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		ctx := WithStateQuerySource(t.Context(), StateQueryConnectH1)
		network := newTestingStateRefusalNetwork(ctx)
		top := network.taggedClient(ctx, nil)
		middle := network.taggedClient(ctx, top)
		leaf := network.taggedClient(ctx, middle)
		if err := ValidateByJwtState(ctx, leaf, true); err != nil {
			t.Fatal(err)
		}
		if value, ok := testingRevocationField(ctx, t, revocationRecordKey(network.networkId, *top.ClientId), revocationClientField(*leaf.ClientId)); !ok || value == revocationTombstone {
			t.Fatalf("the leaf was not projected: %q %v", value, ok)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client WHERE client_id=$1`, *middle.ClientId))
		})
		network.deactivate(ctx, leaf)
		if err := ValidateByJwtState(ctx, leaf, true); !errors.Is(err, ErrByJwtInactive) {
			t.Fatalf("the deactivated leaf of a broken chain answered %v", err)
		}
		for shard := range revocationRecordShardCount {
			if value, ok := testingRevocationField(ctx, t, revocationRecordShardKey(network.networkId, shard), revocationClientField(*leaf.ClientId)); !ok || value != revocationTombstone {
				t.Fatalf("shard %d does not refuse the leaf: %q %v", shard, value, ok)
			}
		}
	})
}
