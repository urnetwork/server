package controller

// Dead-hop probation (model/verify_dead_hop.go) skips, in the verify draw, a
// provider with no confirmation of at least 50 assignments in 24 h. A skipped
// draw is never an assignment. Had it been assigned it would have been one
// more miss, and a provider with no confirmation earns nothing at any
// exposure, so the payout is the same either way.

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/startifact"
)

// The release payout (mainnet's, through the settlement vault) with the dead
// provider's skipped draws counted as misses or not: it is ineligible with a
// reliability of 0 in both, and every leaf, share, usage total and the root
// are the same. Only its own assignment count differs.
func TestStReleasePayoutIgnoresSkippedUnconfirmedDraws(t *testing.T) {
	// the dead provider shares its account network with a live one
	usages := []*model.StProviderUsage{stCanonicalUsageRow(1, 11, 500), stCanonicalUsageRow(2, 12, 300), stCanonicalUsageRow(3, 11, 200)}
	wallets := map[server.Id]*model.StProviderWallet{
		usages[0].ClientId: {ColdkeyPubkey: [32]byte{1}},
		usages[1].ClientId: {ColdkeyPubkey: [32]byte{2}},
		usages[2].ClientId: {ColdkeyPubkey: [32]byte{3}},
	}
	build := func(skipped int64) *startifact.Artifact {
		reliabilities := []*model.StClientReliability{
			{ClientId: usages[0].ClientId, NetworkId: usages[0].NetworkId, Assignments: 60 + skipped, Confirmations: 0},
			{ClientId: usages[1].ClientId, NetworkId: usages[1].NetworkId, Assignments: 100, Confirmations: 90},
			{ClientId: usages[2].ClientId, NetworkId: usages[2].NetworkId, Assignments: 40, Confirmations: 40},
		}
		providers, err := stBuildReleaseProviderInputs(usages, reliabilities, wallets, []*StFleetBindingState{{}, {}, {}}, 8)
		if err != nil {
			t.Fatal(err)
		}
		if providers[0].Eligible || providers[0].ExclusionReason != "reliability_exposure_floor" || !providers[1].Eligible || !providers[2].Eligible {
			t.Fatalf("skipped %d: eligibility %+v", skipped, providers)
		}
		artifact, err := startifact.Build(startifact.BuildInput{
			DeploymentID: "dead-hop-probation", GenesisHash: "0x" + strings.Repeat("ab", 32),
			PolicyHash: "0x" + strings.Repeat("cd", 32), ChainID: 945, Netuid: 521,
			Coordinator: common.HexToAddress("0x100"), SettlementVault: common.HexToAddress("0x200"), Epoch: 4, NoID: 1,
			Start:                startifact.Boundary{Number: 100, Hash: "0x" + strings.Repeat("01", 32)},
			End:                  startifact.Boundary{Number: 200, Hash: "0x" + strings.Repeat("02", 32)},
			OperatorSnapshotHash: "sha256:" + strings.Repeat("10", 32), FleetSnapshotHash: "sha256:" + strings.Repeat("20", 32),
			Providers: providers, ReliabilityAMin: 8, CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return artifact
	}

	skippedNone := build(0)
	if len(skippedNone.Leaves) != 2 || skippedNone.ExcludedUsageBytes != 500 {
		t.Fatalf("baseline payout: %+v", skippedNone)
	}
	for _, skipped := range []int64{1, 24, 500, 60_000} {
		counted := build(skipped)
		if !reflect.DeepEqual(counted.Leaves, skippedNone.Leaves) || counted.PayoutRoot != skippedNone.PayoutRoot ||
			counted.SharesTotalBPS != skippedNone.SharesTotalBPS || counted.TotalUsageBytes != skippedNone.TotalUsageBytes ||
			counted.EligibleUsageBytes != skippedNone.EligibleUsageBytes || counted.ExcludedUsageBytes != skippedNone.ExcludedUsageBytes {
			t.Fatalf("%d skipped draws counted as misses changed the payout: %+v vs %+v", skipped, counted.Leaves, skippedNone.Leaves)
		}
		for index := range counted.Providers {
			before, after := skippedNone.Providers[index], counted.Providers[index]
			if after.ClientID == stId16(usages[0].ClientId) {
				if after.ReliabilityPPM != 0 || before.ReliabilityPPM != 0 || after.Assignments != before.Assignments+uint64(skipped) {
					t.Fatalf("dead provider %+v vs %+v", after, before)
				}
				continue
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("%d skipped draws changed another provider: %+v vs %+v", skipped, after, before)
			}
		}
	}
}
