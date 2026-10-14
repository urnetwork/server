package controller

// st_v1_deposit_gas_test.go — the gas limits the durable engine signs the
// deposit key's stake moves with: the staging transferStake to the staking
// precompile and the v1 coordinator's deposit, each never below its
// stake-move floor as the operator gas policy caps it.

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"

	"github.com/urnetwork/server"
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
