package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/ss58"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// A synthetic dry run: one provider paid to its account-history wallet and
// one refused provider.
func newStPayoutDryRunSummaryFixture(t *testing.T) (*controller.StPayoutDryRun, []string) {
	t.Helper()
	clientId, networkId, refusedId, refusedNetworkId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	var coldkey [32]byte
	for index := range coldkey {
		coldkey[index] = byte(200 + index)
	}
	coldkeySs58, err := ss58.Encode(coldkey, ss58.BittensorPrefix)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.October, 9, 18, 10, 0, 0, time.UTC)
	result := &controller.StPayoutDryRun{
		Epoch: 1, StartBlock: 9_247_228, EndBlock: 9_297_628, StartTime: start, EndTime: start.Add(7 * 24 * time.Hour), EarningStart: start, EarningEnd: start.Add(7 * 24 * time.Hour),
		Wallets: &controller.StPayoutWalletSelection{
			Sources: map[server.Id]string{clientId: controller.StPayoutWalletSourceAccountHistory, refusedId: controller.StPayoutWalletSourceRefused},
			Refused: map[server.Id]error{refusedId: errors.Join(protocol.ErrWalletMappingIntegrity, fmt.Errorf("synthetic evidence %s", refusedId))},
		},
		Artifact: &payoutartifact.Artifact{
			Providers: []payoutartifact.ProviderInput{
				{ClientID: [16]byte(clientId), NetworkID: [16]byte(networkId), Coldkey: coldkey, UsageBytes: 4096, Assignments: 8, Confirmations: 8, ReliabilityPPM: 1_000_000, Eligible: true},
				{ClientID: [16]byte(refusedId), NetworkID: [16]byte(refusedNetworkId), UsageBytes: 1024, ExclusionReason: "missing_payout_wallet"},
			},
			Leaves:          []payoutartifact.Leaf{{Index: 0, ClientID: [16]byte(clientId), Coldkey: coldkey, ShareBPS: 10_000}},
			TotalUsageBytes: 5120, EligibleUsageBytes: 4096, ExcludedUsageBytes: 1024, TotalUsers: 3, SharesTotalBPS: 10_000, PayoutRoot: [32]byte{7},
		},
		ProviderWallets:  map[server.Id]*model.StProviderWallet{clientId: {ClientId: clientId, NetworkId: networkId, ColdkeySs58: coldkeySs58, ColdkeyPubkey: coldkey}},
		NetworkForClient: map[[16]byte]server.Id{[16]byte(clientId): networkId},
		Funding:          &controller.StPoolFunding{Status: 0, FundedRao: big.NewInt(0), TotalRao: big.NewInt(0), ClaimedRao: big.NewInt(0), CarryRao: big.NewInt(500), AccruingRao: big.NewInt(1_200)},
		PreviousFunding:  &controller.StPoolFunding{Status: 1, FundedRao: big.NewInt(500), TotalRao: big.NewInt(0), ClaimedRao: big.NewInt(0), CarryRao: big.NewInt(500), AccruingRao: big.NewInt(1_200)},
		CensusDuration:   90 * time.Second,
		Duration:         2 * time.Minute,
	}
	identities := []string{coldkeySs58, fmt.Sprintf("%x", coldkey)}
	for _, id := range []server.Id{clientId, networkId, refusedId, refusedNetworkId} {
		identities = append(identities, id.String(), fmt.Sprintf("%x", [16]byte(id)))
	}
	return result, identities
}

// Standard output is shared with reviewers, so it carries aggregates only.
func TestStPayoutDryRunSummaryWritesOnlyAggregates(t *testing.T) {
	result, identities := newStPayoutDryRunSummaryFixture(t)
	var out bytes.Buffer
	writeStPayoutDryRunSummary(&out, result)
	summary := out.String()
	for _, identity := range identities {
		if strings.Contains(summary, identity) {
			t.Fatalf("the shared summary leaked %q:\n%s", identity, summary)
		}
	}
	for _, line := range [][]string{
		{"providers", "with", "usage", "2"},
		{"account_history", "1,", "1,", "4096,", "4096"},
		{"refused", "1,", "0,", "1024,", "0"},
		{"refusals", "by", "class", "integrity", "1"},
		{"excluded", "providers", "missing_payout_wallet", "1"},
		{"shares", "total", "bps", "10000"},
		{"carry", "500", "rao"},
		{"pool", "epoch", "1", "unset:"},
		{"previous", "pool", "epoch", "0", "funded:", "funded", "500,"},
		{"projected", "epoch", "total", "1700", "rao"},
	} {
		found := false
		for _, summaryLine := range strings.Split(summary, "\n") {
			fields := strings.Fields(summaryLine)
			if len(line) <= len(fields) && strings.Join(fields[:len(line)], " ") == strings.Join(line, " ") {
				found = true
			}
		}
		if !found {
			t.Fatalf("the summary lacks %q:\n%s", strings.Join(line, " "), summary)
		}
	}
}

// The owner's detail file names each provider's wallet source and leaf, is
// readable by the owner only and never replaces an existing file.
func TestStPayoutDryRunDetailIsOwnerOnly(t *testing.T) {
	result, _ := newStPayoutDryRunSummaryFixture(t)
	path := filepath.Join(t.TempDir(), "epoch-1.json")
	if err := writeStPayoutDryRunDetail(path, result); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("the detail file is readable by others", info, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var detail stPayoutDryRunDetail
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatal(err)
	}
	paid := result.ProviderWallets[server.Id(result.Artifact.Leaves[0].ClientID)]
	if len(detail.Leaves) != 1 || detail.Leaves[0].ColdkeySs58 != paid.ColdkeySs58 || len(detail.Providers) != 2 || detail.Providers[0].WalletSource != controller.StPayoutWalletSourceAccountHistory || detail.Providers[0].ColdkeySs58 != paid.ColdkeySs58 || detail.Providers[1].WalletSource != controller.StPayoutWalletSourceRefused || !strings.Contains(detail.Providers[1].Refusal, "synthetic evidence") {
		t.Fatal("the detail file misreports wallets", detail)
	}
	if err := writeStPayoutDryRunDetail(path, result); err == nil {
		t.Fatal("the detail file replaced an existing file")
	}
}
