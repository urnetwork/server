// An immutable cached v1 payout cannot bypass the recorded v2 epoch cutoff.
package controller

import (
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func TestStCachedPayoutRespectsRecordedV2Cutoff(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f, _ := newLedgerV2ActivationFixture(t)
		observeLedgerV2Activation(t, f)
		f.cfg.SettlementVault = common.Address{4}
		model.AddStPayoutArtifact(f.ctx, f.cfg.DeploymentKey(), &model.StPayoutArtifact{Epoch: 5, NoId: f.cfg.NoId, ContentHash: "synthetic-retained-payout", ContentKey: "synthetic/payout", HistoryKey: "synthetic/history", PayoutRoot: [32]byte{1}, CreateTime: server.NowUtc()})
		if _, _, err := StComputeEpochPayout(f.ctx, 5); !errors.Is(err, ErrStV1EpochSettledByV2) {
			t.Fatalf("cached v1 payout bypassed recorded cutoff: %v", err)
		}
	})
}
