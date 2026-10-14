package controller

// st_v1_deposit_gas_test.go — the gas limits the durable engine signs the
// deposit key's stake moves with: the staging transferStake to the staking
// precompile and the v1 coordinator's deposit, each never below its
// stake-move floor as the operator gas policy caps it.

import (
	"context"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// SV-4 (the v1 deposit, §8.1 gas floor): the staging transferStake and the
// coordinator's deposit are signed with their estimate plus 20%, here 60,000,
// and never with less than their floors, which the operator gas policy caps
// at its maximum_gas and at the gas its per-intent liability covers at the
// signed fee. Without a policy the whole floors apply. Under a maximum_gas of
// 1,000,000 both are signed with 1,000,000, and under 3,500,000 with their
// floors. Under a policy whose per-intent liability covers only the
// estimate's limit at the fixture's price of 10 wei, both are signed with
// that limit, as before the floors, and neither is refused.
func TestStV1DepositIsSignedAtTheGasFloor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if stV1DepositGasFloor != 3_400_000 || stV1DepositStagingGasFloor != 2_800_000 {
			t.Fatalf("deposit floors %d and %d", stV1DepositGasFloor, stV1DepositStagingGasFloor)
		}
		for i, test := range []struct {
			name    string
			policy  func(*server.StOperatorGasPolicy)
			staging uint64
			deposit uint64
		}{
			{"without a policy", nil, stV1DepositStagingGasFloor, stV1DepositGasFloor},
			{"under maximum_gas 1,000,000", func(policy *server.StOperatorGasPolicy) {
				policy.MaximumGas, policy.MaximumIntentLiabilityWei, policy.MaximumLifetimeLiabilityWei = 1_000_000, "35000000", "350000000"
			}, 1_000_000, 1_000_000},
			{"under maximum_gas 3,500,000", func(policy *server.StOperatorGasPolicy) {
				policy.MaximumGas, policy.MaximumIntentLiabilityWei, policy.MaximumLifetimeLiabilityWei = 3_500_000, "35000000", "350000000"
			}, stV1DepositStagingGasFloor, stV1DepositGasFloor},
			// stGasFixturePolicy's own limits: maximum_gas 100,000 and a
			// per-intent liability of 600,000 wei, 60,000 gas at 10 wei.
			{"under a liability that covers only the estimate's limit", func(*server.StOperatorGasPolicy) {}, 60_000, 60_000},
		} {
			noId := uint64(i + 1)
			client, _, fixture := newStDepositCustodyFixture(t, noId)
			if test.policy != nil {
				stGasFixturePolicy(t, client, test.policy)
			}
			ctx := context.Background()
			if _, err := client.stageDepositPrincipal(ctx, 7, big.NewInt(100)); err != nil {
				t.Fatalf("%s: staging: %v", test.name, err)
			}
			if _, err := client.DepositCredit(ctx, 7, noId, big.NewInt(100)); err != nil {
				t.Fatalf("%s: deposit: %v", test.name, err)
			}
			fixture.stateLock.Lock()
			sent := append([]*types.Transaction(nil), fixture.sent...)
			deposited := fixture.deposited.Int64()
			fixture.stateLock.Unlock()
			if len(sent) != 2 || sent[0].Gas() != test.staging || sent[1].Gas() != test.deposit || deposited != 100 {
				t.Fatalf("%s: %d sent, %d deposited", test.name, len(sent), deposited)
			}
		}
	})
}

// SV-4 (the v1 deposit, §8.1 gas floor): each floor is capped at what the
// operator gas policy still accepts, the per-operation and the lifetime
// liability its other nonces leave. Under a per-intent and lifetime liability
// of 30,000,000 wei the staging transferStake is signed with its 2,800,000
// floor at 10 wei, which leaves 2,000,000 wei of the lifetime. The deposit's
// floor is capped at the 200,000 gas that covers, above the estimate's limit
// of 60,000, and the deposit goes out.
func TestStV1DepositFloorStaysWithinTheRemainingLiability(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(t, 1)
		policy, _, _ := stGasFixturePolicy(t, client, func(policy *server.StOperatorGasPolicy) {
			policy.MaximumGas, policy.MaximumIntentLiabilityWei, policy.MaximumLifetimeLiabilityWei = 3_500_000, "30000000", "30000000"
		})
		ctx := context.Background()
		if _, err := client.stageDepositPrincipal(ctx, 7, big.NewInt(100)); err != nil {
			t.Fatalf("staging: %v", err)
		}
		if _, err := client.DepositCredit(ctx, 7, 1, big.NewInt(100)); err != nil {
			t.Fatalf("deposit: %v", err)
		}
		fixture.stateLock.Lock()
		sent := append([]*types.Transaction(nil), fixture.sent...)
		deposited := fixture.deposited.Int64()
		fixture.stateLock.Unlock()
		if len(sent) != 2 || sent[0].Gas() != stV1DepositStagingGasFloor || sent[1].Gas() != 200_000 || deposited != 100 {
			t.Fatalf("%d sent, %d deposited", len(sent), deposited)
		}
		stGasFixtureSnapshot(t, policy, "30000000", 2)
	})
}

// SV-4 (the v1 deposit, §8.1 gas floor): the deposit key must hold each
// send's gas limit at its fee cap, for the staging transferStake its
// 2,800,000 floor at the fixture's 10 wei, 28,000,000 wei. Below that the node
// refuses the staging as underfunded. The signed staging keeps nonce 7, and the
// deposit key's later sends stop at it: each broadcasts its stored bytes
// again, and none takes a nonce or signs the deposit. Its fee replacement, at
// 12 wei, needs 33,600,000 wei and is refused as well. Once the key holds
// 34,000,000 wei, the replacement goes out at nonce 7, and the deposit, its
// 3,400,000 floor at 10 wei, at nonce 8.
func TestStV1DepositStagingWaitsForTheDepositKeyToBeFunded(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(t, 1)
		policy, _, _ := stGasFixturePolicy(t, client, func(policy *server.StOperatorGasPolicy) {
			policy.MaximumGas, policy.MaximumIntentLiabilityWei, policy.MaximumLifetimeLiabilityWei = 3_500_000, "100000000", "1000000000"
		})
		fixture.stateLock.Lock()
		fixture.balance = big.NewInt(27_999_999)
		fixture.stateLock.Unlock()
		ctx := context.Background()
		stagingKey, err := stTransactionLogicalKey(client.cfg, fmt.Sprintf("deposit-fund:7:%d:3", client.cfg.NoId))
		if err != nil {
			t.Fatal(err)
		}
		underfunded := func(step string, attempts int, send func(context.Context) error) {
			t.Helper()
			stopCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err := send(stopCtx)
			cancel()
			staging := model.GetStTransactionIntent(ctx, stagingKey)
			fixture.stateLock.Lock()
			sent := len(fixture.sent)
			fixture.stateLock.Unlock()
			if err == nil || staging == nil || staging.Nonce != 7 || staging.AttemptCount != attempts || model.GetStTransactionIntent(ctx, stGasCurrentLogical(t, client)) != nil || sent != 0 {
				t.Fatalf("%s: %v, staging %+v, %d sent", step, err, staging, sent)
			}
		}
		underfunded("staging", 1, func(ctx context.Context) error {
			_, err := client.stageDepositPrincipal(ctx, 7, big.NewInt(100))
			return err
		})
		deposit := func(ctx context.Context) error {
			_, err := client.DepositCredit(ctx, 7, 1, big.NewInt(100))
			return err
		}
		underfunded("deposit behind the staging", 1, deposit)
		stGasFixtureSnapshot(t, policy, "28000000", 1)

		staging := model.GetStTransactionIntent(ctx, stagingKey)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE st_transaction_attempt SET create_time=$2 WHERE intent_id=$1`, staging.IntentId, server.NowUtc().Add(-2*stTxReplacementDelay)))
		})
		underfunded("the staging's fee replacement", 2, deposit)
		stGasFixtureSnapshot(t, policy, "33600000", 2)

		fixture.stateLock.Lock()
		fixture.balance = big.NewInt(34_000_000)
		fixture.stateLock.Unlock()
		if err := deposit(ctx); err != nil {
			t.Fatalf("deposit once funded: %v", err)
		}
		fixture.stateLock.Lock()
		sent := append([]*types.Transaction(nil), fixture.sent...)
		deposited := fixture.deposited.Int64()
		fixture.stateLock.Unlock()
		replacement := model.GetStTransactionAttempts(ctx, staging.IntentId)[0]
		if len(sent) != 2 || sent[0].Nonce() != 7 || sent[0].Gas() != stV1DepositStagingGasFloor || sent[0].GasPrice().Int64() != 12 || sent[0].Hash().Hex() != common.HexToHash(replacement.TxHash).Hex() ||
			sent[1].Nonce() != 8 || sent[1].Gas() != stV1DepositGasFloor || sent[1].GasPrice().Int64() != 10 || deposited != 100 {
			t.Fatalf("%d sent, %d deposited", len(sent), deposited)
		}
		stGasFixtureSnapshot(t, policy, "67600000", 3)
	})
}
