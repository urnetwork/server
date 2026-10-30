// Continuations reuse their transaction and preserve the explicit sweep exception.
package controller

import (
	"context"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urnetwork/server"
)

func TestStV1ContinuationUsesRecordedCutoffAndSweep(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f, _ := newLedgerV2ActivationFixture(t)
		observeLedgerV2Activation(t, f)
		priorSweep := stLedgerV1SweepEpoch
		t.Cleanup(func() { stLedgerV1SweepEpoch = priorSweep })
		stLedgerV1SweepEpoch = func(context.Context, *StConfig) (uint64, bool) { return 7, true }
		server.Tx(f.ctx, func(tx server.PgTx) {
			for _, entry := range []struct {
				epoch   uint64
				allowed bool
			}{{epoch: 4, allowed: true}, {epoch: 5, allowed: false}, {epoch: 6, allowed: false}, {epoch: 7, allowed: true}, {epoch: 8, allowed: false}} {
				allowed, err := StV1TaskContinuationAllowedInTx(f.ctx, tx, entry.epoch)
				if err != nil || allowed != entry.allowed {
					t.Fatalf("epoch %d: allowed=%t err=%v", entry.epoch, allowed, err)
				}
			}
			missing := *f.cfg
			missing.VaultV2 = common.Address{}
			SetStConfig(&missing)
			if allowed, err := StV1TaskContinuationAllowedInTx(f.ctx, tx, 4); err == nil || allowed {
				t.Fatalf("missing pair admitted continuation: %t %v", allowed, err)
			}
			SetStConfig(f.cfg)
		})
	})
}
