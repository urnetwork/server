package localclient

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// A failed transaction's preparation is not authority for its retry. Land an
// upgrade after the first real rollback and require the returned durable child
// to carry the later read, while both cache tiers retain their free value until
// the upgrade. The separate refusal control pins deferred cache publication.
func TestAuthorityMintRetryRefreshesEntitlementBeforeSnapshot(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		owner, _ := authorityMintTestOwner(t, ctx)
		generator, closeGenerator := authorityMintGenerator(t, ctx, owner)
		defer closeGenerator()
		if model.IsProNetwork(ctx, owner.networkId) {
			t.Fatal("fresh fixture unexpectedly has Pro")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE SEQUENCE synthetic_entitlement_mint_attempt`))
			server.RaisePgResult(tx.Exec(ctx, `
				CREATE FUNCTION synthetic_entitlement_mint_retry() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN
					IF NEW.source_client_id IS NOT NULL AND nextval('synthetic_entitlement_mint_attempt') = 1 THEN
						RAISE EXCEPTION 'synthetic first mint serialization failure' USING ERRCODE='40001';
					END IF;
					RETURN NEW;
				END $$
			`))
			server.RaisePgResult(tx.Exec(ctx, `CREATE TRIGGER synthetic_entitlement_mint_retry BEFORE INSERT ON network_client FOR EACH ROW EXECUTE FUNCTION synthetic_entitlement_mint_retry()`))
		})
		var retries atomic.Int32
		mintCtx := server.Testing_WithTxRerunHook(ctx, func() {
			if retries.Add(1) != 1 {
				t.Fatal("unexpected additional transaction retry")
			}
			local, localOK, cached, cachedOK := model.Testing_ProNetworkCacheEntries(ctx, owner.networkId)
			if !localOK || !cachedOK || local || cached {
				t.Fatal("failed first attempt changed the original free cache")
			}
			now := server.NowUtc()
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(model.AddProTransferBalanceInTx(tx, ctx, owner.networkId, model.ByteCount(1024*1024), now, now.Add(time.Hour)))
			})
		})
		args, err := generator.NewClientArgsContext(mintCtx)
		if err != nil || args == nil || args.ClientAuth == nil {
			t.Fatal("retried mint did not return a child", err)
		}
		claims, err := session.ParseByJwtForAudience(ctx, args.ClientAuth.ByJwt, session.ByJwtAudienceApi)
		if err != nil || !claims.Pro || retries.Load() != 1 {
			t.Fatal("retry reused the first attempt's entitlement")
		}
		if authorityMintChildCount(ctx, owner) != 1 {
			t.Fatal("rolled-back mint leaked or duplicated a child")
		}
		local, localOK, cached, cachedOK := model.Testing_ProNetworkCacheEntries(ctx, owner.networkId)
		if !localOK || !cachedOK || !local || !cached {
			t.Fatal("committed retry did not publish its current entitlement")
		}
	})
}
