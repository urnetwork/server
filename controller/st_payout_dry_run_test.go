// The payout dry run reports what the close publishes and writes nothing.
// Keys, networks, clients, amounts and chain responses are synthetic.
package controller

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"math/big"
	"path/filepath"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The account-wallet fixture client with a fixed epoch state and pool
// funding, which the fixture RPC does not serve.
type stPayoutDryRunClient struct {
	*stPayoutAccountWalletClient
	state         *StEpochState
	epochFundings map[uint64]*StPoolFunding
}

func (self *stPayoutDryRunClient) Epoch(context.Context) (*StEpochState, error) {
	return self.state, nil
}

func (self *stPayoutDryRunClient) PoolFunding(_ context.Context, epoch uint64, _ uint64) (*StPoolFunding, error) {
	if funding := self.epochFundings[epoch]; funding != nil {
		return funding, nil
	}
	return nil, errors.New("synthetic pool funding is absent")
}

// Rows and objects any payout publication or roster preparation would add.
func stPayoutDryRunWrites(t testing.TB, blobRoot string) map[string]int {
	t.Helper()
	ctx := t.Context()
	writes := map[string]int{}
	server.Db(ctx, func(conn server.PgConn) {
		for _, table := range []string{"st_payout_artifact", "st_payout_leaf", "st_payout_wallet_resolution", "st_publish", "st_epoch", "st_provider_wallet_history", "pending_task"} {
			var count int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&count))
			writes[table] = count
		}
	})
	objects := 0
	// the store creates its root with its first object
	if err := filepath.WalkDir(blobRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if path == blobRoot && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !entry.IsDir() {
			objects += 1
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	writes["blob objects"] = objects
	return writes
}

// The dry run as the command runs it: in a process whose every session and
// transaction PostgreSQL holds read only, so any write on its path fails.
func stDryRunEpochPayoutReadOnly(tb testing.TB, epoch uint64) (*StPayoutDryRun, error) {
	tb.Helper()
	server.RequirePgReadOnly()
	tb.Cleanup(server.Testing_ReleasePgReadOnly)
	defer server.Testing_ReleasePgReadOnly()
	return StDryRunEpochPayout(tb.Context(), epoch)
}

// The closed epoch's dry run returns the leaves, wallet sources and pool
// funding without writing a row or object, and the close that follows
// publishes exactly the root the dry run reported.
func TestStDryRunEpochPayoutMatchesTheCloseWithoutWriting(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		f := newStPayoutAccountWalletFixture(tb)
		coldkey := stPayoutAccountWalletRow(tb, f.clientId, f.networkId, 43, stPayoutPolicyTime(210))
		client := &stPayoutDryRunClient{
			stPayoutAccountWalletClient: f.client,
			state:                       &StEpochState{Epoch: 2, PendingEpoch: 2, EpochStartBlock: 250, TEpochBlocks: 50, HeadBlock: 275},
			epochFundings: map[uint64]*StPoolFunding{
				1: {HeadBlock: 275, Status: 1, FundedRao: big.NewInt(7_000), TotalRao: big.NewInt(0), ClaimedRao: big.NewInt(0), CarryRao: big.NewInt(500), AccruingRao: big.NewInt(1_200)},
				0: {HeadBlock: 275, Status: 3, FundedRao: big.NewInt(500), TotalRao: big.NewInt(500), ClaimedRao: big.NewInt(0), CarryRao: big.NewInt(500), AccruingRao: big.NewInt(1_200)},
			},
		}
		SetStClient(client)
		before := stPayoutDryRunWrites(tb, f.rpc.base.blobRoot)
		result, err := stDryRunEpochPayoutReadOnly(tb, 1)
		if err != nil {
			tb.Fatal(err)
		}
		if after := stPayoutDryRunWrites(tb, f.rpc.base.blobRoot); len(after) != len(before) {
			tb.Fatal("the dry run changed what it counts", before, after)
		} else {
			for name, count := range before {
				if after[name] != count {
					tb.Fatal("the dry run wrote", name, count, after[name])
				}
			}
		}
		if result.Preview || result.StartBlock != 200 || result.EndBlock != 250 || !maps.Equal(result.Wallets.Sources, map[server.Id]string{f.clientId: StPayoutWalletSourceAccountHistory}) || len(result.Wallets.Refused) != 0 {
			tb.Fatal("the dry run misreported its window or wallet sources", result.Preview, result.StartBlock, result.EndBlock, result.Wallets)
		}
		if len(result.Artifact.Leaves) != 1 || result.Artifact.Leaves[0].Coldkey != coldkey || result.Artifact.SharesTotalBPS != 10_000 || result.PublishedRoot != nil {
			tb.Fatal("the dry run misreported its leaves", result.Artifact.Leaves, result.PublishedRoot)
		}
		if result.FundingErr != nil || result.Funding.FundedRao.Int64() != 7_000 || result.Funding.CarryRao.Int64() != 500 || result.PreviousFundingErr != nil || result.PreviousFunding.StatusName() != "root_missed" {
			tb.Fatal("the dry run misreported the pool funding", result.Funding, result.FundingErr, result.PreviousFunding, result.PreviousFundingErr)
		}
		root, _, err := stComputeReleasePayout(ctx, f.cfg, client, 1, stPayoutPolicyTime(200), stPayoutPolicyTime(250), 200, 250, nil)
		if err != nil || root != result.Artifact.PayoutRoot {
			tb.Fatal("the close published another root than its dry run", root, result.Artifact.PayoutRoot, err)
		}
		if record := model.GetStPayoutArtifact(ctx, f.cfg.DeploymentKey(), 1, f.cfg.NoId); record == nil || record.ContentHash != result.Artifact.ContentHash {
			tb.Fatal("the close published another artifact than its dry run", record)
		}
	})
}

// The open epoch previews from its start through the finalized head.
func TestStDryRunEpochPayoutPreviewsTheOpenEpoch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		f := newStPayoutAccountWalletFixture(tb)
		stPayoutAccountWalletRow(tb, f.clientId, f.networkId, 45, stPayoutPolicyTime(210))
		stPayoutAccountWalletUsage(tb, f.networkId, f.clientId, stPayoutPolicyTime(260), 1024*1024)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO verify_provider_stats (period_start, period_end, client_id, assignments, confirmations) VALUES ($1, $2, $3, 8, 8)`,
				stPayoutPolicyTime(255), stPayoutPolicyTime(265), f.clientId))
		})
		client := &stPayoutDryRunClient{stPayoutAccountWalletClient: f.client, state: &StEpochState{Epoch: 2, PendingEpoch: 2, EpochStartBlock: 250, TEpochBlocks: 50, HeadBlock: 275}}
		SetStClient(client)
		result, err := stDryRunEpochPayoutReadOnly(tb, 2)
		if err != nil {
			tb.Fatal(err)
		}
		if !result.Preview || result.StartBlock != 250 || result.EndBlock != 275 || !result.StartTime.Equal(stPayoutPolicyTime(250)) || !result.EndTime.Equal(stPayoutPolicyTime(275)) {
			tb.Fatal("the preview window is not the open epoch through the finalized head", result.Preview, result.StartBlock, result.EndBlock, result.StartTime, result.EndTime)
		}
		if result.Artifact.TotalUsageBytes != 1024*1024 || len(result.Artifact.Leaves) != 1 || result.Wallets.Sources[f.clientId] != StPayoutWalletSourceAccountHistory {
			tb.Fatal("the preview did not read the open epoch's usage", result.Artifact.TotalUsageBytes, result.Artifact.Leaves, result.Wallets)
		}
		if result.FundingErr == nil {
			tb.Fatal("absent pool funding was reported")
		}
		if _, err := stDryRunEpochPayoutReadOnly(tb, 3); err == nil {
			tb.Fatal("an epoch that has not started was previewed")
		}
	})
}

// With a configured roster the preparation retains observations, so the dry
// run refuses before reading usage.
func TestStDryRunEpochPayoutRefusesAConfiguredRoster(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		f := newProviderWorkWindowFixture(tb)
		SetStConfig(f.cfg)
		SetStClient(newStubStClient(&StEpochState{Epoch: 8, PendingEpoch: 8}))
		tb.Cleanup(func() { SetStClient(nil); SetStConfig(nil) })
		if result, err := StDryRunEpochPayout(tb.Context(), f.epoch.Epoch); result != nil || !errors.Is(err, ErrStPayoutDryRunRosterConfigured) {
			tb.Fatal("the dry run ran with a configured roster", result, err)
		}
	})
}
