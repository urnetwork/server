// `bringyourctl st payout-dry-run`: the read-only release payout computation
// for owner review. Standard output carries aggregates only and is safe to
// share; the optional --out file holds provider identifiers and wallet
// addresses for the owner. Run it inside the data network, as the close runs:
// its census streams every contract closed in the window, which a remote
// tunnel cannot carry in reasonable time.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/big"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/ss58"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

func stPayoutDryRun(opts docopt.Opts) {
	// PostgreSQL refuses every write this process attempts
	server.RequirePgReadOnly()
	ctx := context.Background()
	epochStr, _ := opts.String("--epoch")
	epoch, err := strconv.ParseUint(epochStr, 10, 64)
	if err != nil {
		panic(fmt.Errorf("bad --epoch %q: %s", epochStr, err))
	}
	out, _ := opts.String("--out")
	if out != "" {
		// fail before the census, not after it
		if _, err := os.Lstat(out); !errors.Is(err, os.ErrNotExist) {
			panic(fmt.Errorf("--out %q already exists or cannot be checked: %v", out, err))
		}
	}
	result, err := controller.StDryRunEpochPayout(ctx, epoch)
	if err != nil {
		panic(err)
	}
	writeStPayoutDryRunSummary(os.Stdout, result)
	if out != "" {
		if err := writeStPayoutDryRunDetail(out, result); err != nil {
			panic(err)
		}
		fmt.Printf("provider detail with identifiers and wallet addresses: %s\n", out)
	}
}

// The class of a refusal, without its detail, which can name evidence.
func stPayoutRefusalClass(err error) string {
	switch {
	case errors.Is(err, protocol.ErrWalletMappingIntegrity):
		return "integrity"
	case errors.Is(err, protocol.ErrWalletMappingCapacity):
		return "capacity"
	case errors.Is(err, protocol.ErrWalletMappingUnavailable):
		return "unavailable"
	default:
		return "other"
	}
}

// No provider identifier or wallet address is written.
func writeStPayoutDryRunSummary(w io.Writer, result *controller.StPayoutDryRun) {
	line := func(label string, format string, args ...any) {
		fmt.Fprintf(w, "%-36s %s\n", label, fmt.Sprintf(format, args...))
	}
	artifact, selection := result.Artifact, result.Wallets
	fmt.Fprintln(w, "st payout dry run: read-only; nothing was published, stored or sent")
	line("epoch", "%d", result.Epoch)
	if result.Preview {
		line("window", "preview of the open epoch through finalized block %d", result.EndBlock)
	} else {
		line("window", "closed epoch, authenticated boundaries")
	}
	line("blocks", "%d..%d", result.StartBlock, result.EndBlock)
	line("time", "%s .. %s", result.StartTime.UTC().Format(time.RFC3339), result.EndTime.UTC().Format(time.RFC3339))
	line("earning window", "%s .. %s", result.EarningStart.UTC().Format(time.RFC3339), result.EarningEnd.UTC().Format(time.RFC3339))
	if selection.RosterConfigured {
		line("wallet authority", "provider work roster")
	} else {
		line("wallet authority", "no roster: signed consents, then account history for providers without signed evidence")
	}

	// each provider's usage and eligibility by its wallet source
	type sourceTotals struct {
		providers, eligible            int
		usageBytes, eligibleUsageBytes uint64
	}
	totals := map[string]*sourceTotals{}
	for _, source := range controller.StPayoutWalletSources {
		totals[source] = &sourceTotals{}
	}
	exclusionReasonCounts := map[string]int{}
	eligible := 0
	for _, provider := range artifact.Providers {
		source := selection.Sources[server.Id(provider.ClientID)]
		if totals[source] == nil {
			// a provider the selection did not see is reported, not hidden
			source = "unknown"
			totals[source] = &sourceTotals{}
		}
		total := totals[source]
		total.providers += 1
		total.usageBytes += provider.UsageBytes
		if provider.Eligible {
			eligible += 1
			total.eligible += 1
			total.eligibleUsageBytes += provider.UsageBytes
		} else {
			exclusionReasonCounts[provider.ExclusionReason] += 1
		}
	}
	line("providers with usage", "%d", len(artifact.Providers))
	line("by wallet source", "providers, eligible, usage bytes, eligible usage bytes")
	for _, source := range append(slices.Clone(controller.StPayoutWalletSources), "unknown") {
		total := totals[source]
		if total == nil || source == "unknown" && total.providers == 0 {
			continue
		}
		line("  "+source, "%d, %d, %d, %d", total.providers, total.eligible, total.usageBytes, total.eligibleUsageBytes)
	}
	refusalClassCounts := map[string]int{}
	for _, reason := range selection.Refused {
		refusalClassCounts[stPayoutRefusalClass(reason)] += 1
	}
	refusals := []string{}
	for _, class := range slices.Sorted(maps.Keys(refusalClassCounts)) {
		refusals = append(refusals, fmt.Sprintf("%s %d", class, refusalClassCounts[class]))
	}
	if len(refusals) == 0 {
		refusals = append(refusals, "none")
	}
	line("refusals by class", "%s", strings.Join(refusals, ", "))
	line("eligible providers", "%d", eligible)
	exclusions := []string{}
	for _, reason := range slices.Sorted(maps.Keys(exclusionReasonCounts)) {
		exclusions = append(exclusions, fmt.Sprintf("%s %d", reason, exclusionReasonCounts[reason]))
	}
	if len(exclusions) == 0 {
		exclusions = append(exclusions, "none")
	}
	line("excluded providers", "%s", strings.Join(exclusions, ", "))
	line("usage bytes", "total %d, eligible %d, excluded %d", artifact.TotalUsageBytes, artifact.EligibleUsageBytes, artifact.ExcludedUsageBytes)
	line("users", "%d", artifact.TotalUsers)
	largestShareBps := uint64(0)
	for _, leaf := range artifact.Leaves {
		largestShareBps = max(largestShareBps, leaf.ShareBPS)
	}
	line("leaves", "%d", len(artifact.Leaves))
	line("shares total bps", "%d", artifact.SharesTotalBPS)
	line("largest leaf share bps", "%d", largestShareBps)
	line("payout root", "0x%x", artifact.PayoutRoot)
	switch {
	case result.PublishedRoot == nil:
		line("published artifact", "none")
	case *result.PublishedRoot == artifact.PayoutRoot:
		line("published artifact", "root matches")
	default:
		line("published artifact", "root differs: 0x%x", *result.PublishedRoot)
	}
	funding := func(label string, epoch uint64, value *controller.StPoolFunding, err error) {
		if err != nil {
			line(label, "epoch %d unavailable: %s", epoch, err)
			return
		}
		line(label, "epoch %d %s: funded %s, total %s, claimed %s rao", epoch, value.StatusName(), value.FundedRao, value.TotalRao, value.ClaimedRao)
	}
	funding("pool", result.Epoch, result.Funding, result.FundingErr)
	if 0 < result.Epoch {
		funding("previous pool", result.Epoch-1, result.PreviousFunding, result.PreviousFundingErr)
	}
	if result.FundingErr == nil {
		line("carry", "%s rao (added to the next finalized root)", result.Funding.CarryRao)
		line("accruing on pool hotkey", "%s rao (captured at the next timely close)", result.Funding.AccruingRao)
		if projected, note := stPayoutProjectedTotal(result); projected != nil {
			line("projected epoch total", "%s rao (%s)", projected, note)
		}
	}
	line("census read", "%s", result.CensusDuration.Round(time.Millisecond))
	line("elapsed", "%s", result.Duration.Round(time.Millisecond))
}

// What finalize would pay the epoch's root, from the vault as read now: the
// stake a timely close captures (or the already captured funding), the carry,
// and the previous epoch's funding while it waits to be marked missed. It is
// a projection; later emission, carry and finalize order change it.
func stPayoutProjectedTotal(result *controller.StPayoutDryRun) (*big.Int, string) {
	funding := result.Funding
	if funding == nil || funding.CarryRao == nil || funding.AccruingRao == nil || funding.FundedRao == nil {
		return nil, ""
	}
	// statuses: 0 unset, 1 funded
	projected := new(big.Int)
	notes := []string{}
	switch funding.Status {
	case 0:
		projected.Add(projected, funding.AccruingRao)
		notes = append(notes, "accruing stake if the close is timely")
	case 1:
		projected.Add(projected, funding.FundedRao)
		notes = append(notes, "captured funding")
	default:
		return nil, ""
	}
	projected.Add(projected, funding.CarryRao)
	notes = append(notes, "carry")
	if previous := result.PreviousFunding; result.PreviousFundingErr == nil && previous != nil && previous.Status == 1 && previous.FundedRao != nil {
		projected.Add(projected, previous.FundedRao)
		notes = append(notes, fmt.Sprintf("epoch %d funding once it is marked missed", result.Epoch-1))
	}
	return projected, strings.Join(notes, " + ")
}

type stPayoutDryRunLeaf struct {
	Index       uint64 `json:"index"`
	ClientId    string `json:"client_id"`
	NetworkId   string `json:"network_id"`
	ColdkeySs58 string `json:"coldkey_ss58"`
	ShareBps    uint64 `json:"share_bps"`
}

type stPayoutDryRunProvider struct {
	ClientId        string `json:"client_id"`
	NetworkId       string `json:"network_id"`
	WalletSource    string `json:"wallet_source"`
	ColdkeySs58     string `json:"coldkey_ss58,omitempty"`
	Refusal         string `json:"refusal,omitempty"`
	UsageBytes      uint64 `json:"usage_bytes"`
	Assignments     uint64 `json:"assignments"`
	Confirmations   uint64 `json:"confirmations"`
	ReliabilityPpm  uint32 `json:"reliability_ppm"`
	Eligible        bool   `json:"eligible"`
	ExclusionReason string `json:"exclusion_reason,omitempty"`
}

type stPayoutDryRunDetail struct {
	Epoch      uint64                   `json:"epoch"`
	Preview    bool                     `json:"preview"`
	PayoutRoot string                   `json:"payout_root"`
	Leaves     []stPayoutDryRunLeaf     `json:"leaves"`
	Providers  []stPayoutDryRunProvider `json:"providers"`
}

// Created exclusively and readable by the owner only.
func writeStPayoutDryRunDetail(path string, result *controller.StPayoutDryRun) error {
	detail := stPayoutDryRunDetail{Epoch: result.Epoch, Preview: result.Preview, PayoutRoot: fmt.Sprintf("0x%x", result.Artifact.PayoutRoot), Leaves: []stPayoutDryRunLeaf{}, Providers: []stPayoutDryRunProvider{}}
	for _, leaf := range result.Artifact.Leaves {
		coldkeySs58, err := ss58.Encode(leaf.Coldkey, ss58.BittensorPrefix)
		if err != nil {
			return err
		}
		detail.Leaves = append(detail.Leaves, stPayoutDryRunLeaf{Index: leaf.Index, ClientId: server.Id(leaf.ClientID).String(), NetworkId: result.NetworkForClient[leaf.ClientID].String(), ColdkeySs58: coldkeySs58, ShareBps: leaf.ShareBPS})
	}
	for _, provider := range result.Artifact.Providers {
		clientId := server.Id(provider.ClientID)
		row := stPayoutDryRunProvider{ClientId: clientId.String(), NetworkId: server.Id(provider.NetworkID).String(), WalletSource: result.Wallets.Sources[clientId], UsageBytes: provider.UsageBytes, Assignments: provider.Assignments, Confirmations: provider.Confirmations, ReliabilityPpm: provider.ReliabilityPPM, Eligible: provider.Eligible, ExclusionReason: provider.ExclusionReason}
		if wallet := result.ProviderWallets[clientId]; wallet != nil {
			row.ColdkeySs58 = wallet.ColdkeySs58
		}
		if reason := result.Wallets.Refused[clientId]; reason != nil {
			row.Refusal = reason.Error()
		}
		detail.Providers = append(detail.Providers, row)
	}
	encoded, err := json.MarshalIndent(detail, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
