// Payout canonicalization regressions cover unordered and ambiguous database
// rows before they can affect positional bindings or signed snapshots.
package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/startifact"
)

// Builds one valid database-shaped payout row with independently selectable
// provider and network identities.
func stCanonicalUsageRow(client, network byte, usage int64) *model.StProviderUsage {
	return &model.StProviderUsage{
		ClientId:        server.Id{client},
		NetworkId:       server.Id{network},
		PayoutByteCount: usage,
	}
}

// Exercises the production client join and Merkle builder with two providers
// sharing one account network. Either provider can be the promoted head;
// only that client is excluded and the other's own wallet receives the leaf.
func TestStBuildReleaseProviderInputsIsolatesSharedNetworkHead(t *testing.T) {
	usages := []*model.StProviderUsage{stCanonicalUsageRow(1, 11, 61), stCanonicalUsageRow(2, 11, 60)}
	reliabilities := []*model.StClientReliability{
		{ClientId: usages[0].ClientId, NetworkId: usages[0].NetworkId, Assignments: 10, Confirmations: 5},
		{ClientId: usages[1].ClientId, NetworkId: usages[1].NetworkId, Assignments: 20, Confirmations: 20},
	}
	wallets := map[server.Id]*model.StProviderWallet{
		usages[0].ClientId: {ColdkeyPubkey: [32]byte{1}},
		usages[1].ClientId: {ColdkeyPubkey: [32]byte{2}},
	}
	for headIndex := range usages {
		bindings := []*StFleetBindingState{{}, {}}
		bindings[headIndex] = &StFleetBindingState{Active: true, Generation: 7}
		providers, err := stBuildReleaseProviderInputs(usages, reliabilities, wallets, bindings, 8)
		if err != nil {
			t.Fatal(err)
		}
		for index, provider := range providers {
			if provider.ClientID != stId16(usages[index].ClientId) || provider.UsageBytes != uint64(usages[index].PayoutByteCount) ||
				provider.Coldkey != wallets[usages[index].ClientId].ColdkeyPubkey || provider.Assignments != uint64(reliabilities[index].Assignments) ||
				provider.HeadExcluded != (index == headIndex) || provider.Eligible != (index != headIndex) {
				t.Fatalf("head %d provider %d lost individual attribution: %+v", headIndex, index, provider)
			}
		}
		artifact, err := startifact.Build(startifact.BuildInput{
			DeploymentID: "provider-attribution", GenesisHash: "0x" + strings.Repeat("ab", 32),
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
		poolIndex := 1 - headIndex
		if len(artifact.Leaves) != 1 || artifact.Leaves[0].ClientID != stId16(usages[poolIndex].ClientId) ||
			artifact.Leaves[0].Coldkey != wallets[usages[poolIndex].ClientId].ColdkeyPubkey || artifact.Leaves[0].ShareBPS != 10000 ||
			artifact.TotalUsageBytes != 121 || artifact.ExcludedUsageBytes != uint64(usages[headIndex].PayoutByteCount) {
			t.Fatalf("head %d produced incorrect pool payout: %+v", headIndex, artifact)
		}
	}
}

// A missing wallet or inadequate reliability applies to that exact client,
// never to every client that happens to share its network account.
func TestStBuildReleaseProviderInputsIsolatesSharedNetworkEligibility(t *testing.T) {
	usages := []*model.StProviderUsage{stCanonicalUsageRow(1, 11, 61), stCanonicalUsageRow(2, 11, 60)}
	reliabilities := []*model.StClientReliability{
		{ClientId: usages[0].ClientId, Assignments: 7, Confirmations: 7},
		{ClientId: usages[1].ClientId, Assignments: 10, Confirmations: 10},
	}
	wallets := map[server.Id]*model.StProviderWallet{usages[1].ClientId: {ColdkeyPubkey: [32]byte{2}}}
	providers, err := stBuildReleaseProviderInputs(usages, reliabilities, wallets, []*StFleetBindingState{{}, {}}, 8)
	if err != nil {
		t.Fatal(err)
	}
	if providers[0].Eligible || providers[0].ExclusionReason != "missing_payout_wallet" || !providers[1].Eligible || providers[1].UsageBytes != 60 {
		t.Fatalf("shared account leaked eligibility between clients: %+v", providers)
	}
	for _, bindings := range [][]*StFleetBindingState{{{}}, {{}, nil}} {
		if _, err := stBuildReleaseProviderInputs(usages, reliabilities, wallets, bindings, 8); err == nil {
			t.Fatal("incomplete positional binding rows were accepted")
		}
	}
}

// Proves arbitrary database result order produces one stable client order
// without mutating the caller's slice.
func TestStCanonicalProviderUsagesOrdersIndependentDatabaseResults(t *testing.T) {
	first := stCanonicalUsageRow(1, 11, 100)
	second := stCanonicalUsageRow(2, 12, 200)
	third := stCanonicalUsageRow(3, 13, 300)
	permutations := [][]*model.StProviderUsage{
		{third, first, second},
		{second, third, first},
		{first, second, third},
	}
	for permutationIndex, rows := range permutations {
		original := append([]*model.StProviderUsage(nil), rows...)
		ordered, err := stCanonicalProviderUsages(rows)
		if err != nil {
			t.Fatalf("permutation %d: %v", permutationIndex, err)
		}
		if len(ordered) != 3 || ordered[0] != first || ordered[1] != second || ordered[2] != third {
			t.Fatalf("permutation %d produced noncanonical order: %+v", permutationIndex, ordered)
		}
		for index := range rows {
			if rows[index] != original[index] {
				t.Fatalf("permutation %d mutated caller row %d", permutationIndex, index)
			}
		}
	}
}

// Rejects ambiguous or malformed source rows before positional binding lookup
// can authenticate the wrong client or snapshot bytes.
func TestStCanonicalProviderUsagesRejectsDuplicateAndMalformedRows(t *testing.T) {
	valid := stCanonicalUsageRow(1, 11, 100)
	tests := []struct {
		name string
		rows []*model.StProviderUsage
	}{
		{name: "duplicate-client-across-networks", rows: []*model.StProviderUsage{valid, stCanonicalUsageRow(1, 12, 200)}},
		{name: "nil-row", rows: []*model.StProviderUsage{nil}},
		{name: "zero-client", rows: []*model.StProviderUsage{{ClientId: server.Id{}, NetworkId: server.Id{1}, PayoutByteCount: 1}}},
		{name: "zero-network", rows: []*model.StProviderUsage{{ClientId: server.Id{1}, NetworkId: server.Id{}, PayoutByteCount: 1}}},
		{name: "negative-usage", rows: []*model.StProviderUsage{{ClientId: server.Id{1}, NetworkId: server.Id{1}, PayoutByteCount: -1}}},
	}
	for _, test := range tests {
		if ordered, err := stCanonicalProviderUsages(test.rows); err == nil {
			t.Fatalf("%s unexpectedly produced %+v", test.name, ordered)
		}
	}
}
