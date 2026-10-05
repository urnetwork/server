// A proposed exclusion list cannot hide a previously published SDK contract.
// Actual candidate discovery uses retained windows and replays their originals.
package controller

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/urfoundation/sn/payoutartifact"
	snprotocol "github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The public producer must find a prior original even when the new authority
// omits it and the current SQL window contains no record for the old contract.
func TestProviderWorkActualPayoutFindsOmittedPriorSdkCandidate(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkWindowFixture(t)
		keys := providerWorkRosterFixture(t, f)
		record, verified := providerWorkPublishCanceledPrior(t, f, keys)
		priorEpoch := f.epoch.Epoch
		f.epoch.Epoch++
		f.authority.Epoch = f.epoch.Epoch
		f.authority.PriorContracts = []payoutartifact.WholeWorkPriorContract{}
		start := f.epoch.EndTime
		for side, number := range []uint64{40, 50} {
			clock := start.Add(time.Duration(side) * time.Hour)
			header := &types.Header{Number: new(big.Int).SetUint64(number), Time: uint64(clock.Unix())}
			raw, err := rlp.EncodeToBytes(header)
			if err != nil {
				t.Fatal(err)
			}
			boundary := payoutartifact.Boundary{Number: number, Hash: header.Hash().Hex()}
			if side == 0 {
				f.epoch.StartTime, f.epoch.StartHeader, f.epoch.Start = clock, raw, snprotocol.ClientKeyEffectiveBoundary{Block: number, Hash: [32]byte(header.Hash())}
				f.authority.Start = boundary
			} else {
				f.epoch.EndTime, f.epoch.EndHeader, f.epoch.End = clock, raw, snprotocol.ClientKeyEffectiveBoundary{Block: number, Hash: [32]byte(header.Hash())}
				f.authority.End = boundary
			}
		}
		f.window.Start, f.window.End = f.epoch.StartTime.Format(time.RFC3339Nano), f.epoch.EndTime.Format(time.RFC3339Nano)
		f.window.Records = []payoutartifact.ClosedWorkWindowRecord{}
		f.retainAuthority(t)
		providerWorkRetainRosterCuts(t, f, keys, &record, true)
		inventory, expected, err := stPrepareWholeWorkInventory(t.Context(), f.cfg, f.epoch, f.window)
		if err != nil || inventory == nil || len(expected.PriorContracts) != 1 || expected.PriorContracts[0] != verified.ReconciledContracts[0] {
			t.Fatal("omitted old SDK candidate escaped original prior lookup", expected.PriorContracts, err)
		}
		ids := []server.Id{server.Id(record.ContractId)}
		epochs, err := model.ListProviderWorkPriorEpochs(t.Context(), f.domain, f.cfg.DeploymentKey(), f.cfg.NoId, f.epoch.Epoch, ids, 64)
		if err != nil || len(epochs) != 1 || epochs[0] != priorEpoch {
			t.Fatal("actual published original index lost candidate", epochs, err)
		}
		for _, domain := range [][32]byte{f.domain, [32]byte{181}} {
			before := priorEpoch
			if domain != f.domain {
				before = f.epoch.Epoch
			}
			epochs, err := model.ListProviderWorkPriorEpochs(t.Context(), domain, f.cfg.DeploymentKey(), f.cfg.NoId, before, ids, 64)
			if err != nil || len(epochs) != 0 {
				t.Fatal("prior locator crossed domain or epoch", epochs, err)
			}
		}
		client := &providerWorkRosterClient{StClient: newStubStClient(&StEpochState{})}
		if _, _, err := stComputeReleasePayout(t.Context(), f.cfg, client, f.epoch.Epoch, f.epoch.StartTime, f.epoch.EndTime, f.epoch.Start.Block, f.epoch.End.Block, f.epoch); !errors.Is(err, payoutartifact.ErrClosedWorkIntegrity) {
			t.Fatal("actual payout did not refuse reassigning retained prior original", err)
		}
		if artifact := model.GetStPayoutArtifact(t.Context(), f.cfg.DeploymentKey(), f.epoch.Epoch, f.cfg.NoId); artifact != nil {
			t.Fatal("omitted prior proposal published another artifact")
		}
	})
}
