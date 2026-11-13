// A healthy traffic source can have no eligible payout providers while its
// independent proof workers replay startup history. Test the real provider join
// and signed artifact rather than inventing a failed root submission.
package controller

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/startifact"
)

// Usage survives a proof blackout, but head exclusion and the pool provider's
// missing exposure correctly leave an auditable zero-root payout census. Fresh
// assignments alone remain insufficient; confirmations restore the real leaf.
func TestStBuildReleaseProviderInputsProofStarvationProducesEmptyCensus(t *testing.T) {
	usages := []*model.StProviderUsage{stCanonicalUsageRow(1, 11, 900), stCanonicalUsageRow(2, 12, 100)}
	wallets := map[server.Id]*model.StProviderWallet{
		usages[0].ClientId: {ColdkeyPubkey: [32]byte{1}},
		usages[1].ClientId: {ColdkeyPubkey: [32]byte{2}},
	}
	bindings := []*StFleetBindingState{{Active: true, Generation: 3}, {}}
	signer, err := crypto.ToECDSA(bytes.Repeat([]byte{0x35}, 32))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name          string
		assignments   int64
		confirmations int64
		leaves        int
	}{
		{name: "replaying proof workers", leaves: 0},
		{name: "assignments without proof", assignments: 8, leaves: 0},
		{name: "fresh confirmed exposure", assignments: 8, confirmations: 8, leaves: 1},
	} {
		reliabilities := []*model.StClientReliability{
			{ClientId: usages[0].ClientId, Assignments: 8, Confirmations: 8},
			{ClientId: usages[1].ClientId, Assignments: c.assignments, Confirmations: c.confirmations},
		}
		providers, err := stBuildReleaseProviderInputs(usages, reliabilities, wallets, bindings, 8)
		if err != nil {
			t.Fatal(err)
		}
		artifact, err := startifact.Build(startifact.BuildInput{
			DeploymentID: "synthetic-proof-starvation", GenesisHash: "0x" + strings.Repeat("ab", 32),
			PolicyHash: "0x" + strings.Repeat("cd", 32), ChainID: 945, Netuid: 7,
			Coordinator: common.HexToAddress("0x100"), SettlementVault: common.HexToAddress("0x200"), Epoch: 8, NoID: 2,
			Start:                startifact.Boundary{Number: 100, Hash: "0x" + strings.Repeat("01", 32)},
			End:                  startifact.Boundary{Number: 200, Hash: "0x" + strings.Repeat("02", 32)},
			OperatorSnapshotHash: "sha256:" + strings.Repeat("10", 32), FleetSnapshotHash: "sha256:" + strings.Repeat("20", 32),
			Providers: providers, ReliabilityAMin: 8, CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := startifact.Sign(artifact, signer); err != nil {
			t.Fatal(err)
		}
		if err := startifact.Verify(artifact); err != nil {
			t.Fatal(err)
		}
		if len(artifact.Leaves) != c.leaves || artifact.TotalUsageBytes != 1000 || !providers[0].HeadExcluded || providers[0].Eligible {
			t.Fatalf("%s altered usage or payout census: %+v", c.name, artifact)
		}
		if c.leaves == 0 {
			if artifact.PayoutRoot != ([32]byte{}) || artifact.SharesTotalBPS != 0 || artifact.ExcludedUsageBytes != 1000 || providers[1].ExclusionReason != "reliability_exposure_floor" {
				t.Fatalf("%s fabricated a payable root: %+v", c.name, artifact)
			}
		} else if artifact.PayoutRoot == ([32]byte{}) || artifact.Leaves[0].ClientID != stId16(usages[1].ClientId) || artifact.Leaves[0].ShareBPS != 10000 || artifact.ExcludedUsageBytes != 900 {
			t.Fatalf("confirmed pool exposure did not restore only its real leaf: %+v", artifact)
		}
	}
}
