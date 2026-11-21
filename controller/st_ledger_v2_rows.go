package controller

// st_ledger_v2_rows.go — a covered epoch's provider rows (VERSION2-FINAL.md
// §6.2, §8.2) from v1's inputs, at a million providers: the usage census over
// the epoch's earning window; wallet selection by the signed roster or
// retained consents, in bulk over consent chains verified once per head; head
// exclusion at both of the epoch's boundary blocks, reading bindingAt only for
// FleetBound candidates; and, from SN25 epoch 7, the exit rule
// (stLedgerV2ExitDiffers). Reliability is not an input.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// ErrStLedgerV2WindowClipped refuses an epoch whose earning window the
// cutoff_utc would clip; every v2 epoch starts after the cutoff (§6.2).
var ErrStLedgerV2WindowClipped = errors.New("st ledger v2: the cutoff_utc clips the epoch's earning window")

// stLedgerV2ExitFirstEpoch is the first SN25 epoch whose provider rows judge a
// provider by its probed exit (owner decision 2026-11-20, "forward-only rule
// from epoch 7"; sn WHITEPAPER.md §8.2). Every earlier epoch, epoch 6
// included, is built exactly as before.
const stLedgerV2ExitFirstEpoch uint64 = 7

// Whether the exit rule judges an epoch's providers.
func stLedgerV2ExitRuleInForce(epoch uint64) bool {
	return stLedgerV2ExitFirstEpoch <= epoch
}

// stLedgerV2ExitDiffers is an epoch's exit rule: the providers that earn no
// usage in it because a probe judged their exit different from every network
// they connected from, at a judgement observed in the epoch's earning window
// [timestamp(start), timestamp(end)) (model/provider_egress_exit_model.go,
// model.GetProviderEgressExitDiffersClientIds). Their users' traffic leaves
// from somewhere other than where they connect, as a proxy slot's does when it
// connects through a residential proxy and sends its users' traffic out of a
// datacenter. Only the durable record's positive evidence counts: a provider
// it holds nothing for in the window earns as before, and so does every one
// active only before the record existed. An epoch before
// stLedgerV2ExitFirstEpoch has none.
func stLedgerV2ExitDiffers(ctx context.Context, epoch uint64, authority *StPayoutEpochAuthority) map[server.Id]bool {
	if !stLedgerV2ExitRuleInForce(epoch) {
		return map[server.Id]bool{}
	}
	return model.GetProviderEgressExitDiffersClientIds(ctx, authority.StartTime, authority.EndTime)
}

// stLedgerV2NewEpochRows builds a covered epoch's provider rows for the
// configured pool from v1's inputs, read as v1 reads them: one row per
// provider with settled bytes in the earning window [timestamp(start),
// timestamp(end)) of the epoch's boundary blocks, in client id order, with
// the coldkey its wallet selection picks (zero without one, and, from epoch 7,
// for a provider the exit rule holds) and the head flag of an active fleet
// binding at either boundary. The selected wallets' resolutions are retained
// as v1 retains them, before the rows are recorded.
func stLedgerV2NewEpochRows(ctx context.Context, epoch uint64) ([]protocol.LedgerUsageRow, error) {
	rows, _, _, err := stLedgerV2NewAmendedEpochRows(ctx, epoch, model.StExtenderAmendment{}, nil)
	return rows, err
}

// An epoch's rows as stLedgerV2NewEpochRows builds them, under the SN25
// extender amendment (sn WHITEPAPER.md §8.6.2). For an epoch the amendment
// covers, the rows hold the contracts' hop shares less the ones it pools (a
// platform endpoint's, and those on a contract's origin network), the pool
// returned is the epoch's extender pool: Z and its parts, the contracts of
// declared measurement and test networks left out, and the relay returned is
// the bytes its extenders relayed (model.GetStEpochExtenderRelay), read in the
// default slices. For any other epoch the rows are exactly
// stLedgerV2NewEpochRows's and the pool and relay are nil. pool is the
// extender pool's configuration, needed only when the amendment covers the
// epoch.
func stLedgerV2NewAmendedEpochRows(ctx context.Context, epoch uint64, amendment model.StExtenderAmendment, pool *stExtenderPoolConfig) ([]protocol.LedgerUsageRow, *model.StEpochExtenderPool, *model.StEpochExtenderRelay, error) {
	return stLedgerV2AmendedEpochRows(ctx, epoch, amendment, pool, true, model.StEpochUsageSlices{}, model.DefaultStEpochRelaySettings())
}

// As stLedgerV2NewAmendedEpochRows, retaining the selected wallets'
// resolutions only when retain is set: a shadow report reads an epoch without
// recording anything (st_ledger_v2_extender_shadow.go). slicing cuts the read
// of an epoch the amendment covers into short snapshots
// (model.GetStEpochAmendedUsageInSlices), to the same totals; its zero value
// reads the epoch in one snapshot, as a root's build does. relay walks the
// epoch's relay bytes with its settings; nil reads none, for a caller that
// compares rows only.
func stLedgerV2AmendedEpochRows(ctx context.Context, epoch uint64, amendment model.StExtenderAmendment, pool *stExtenderPoolConfig, retain bool, slicing model.StEpochUsageSlices, relay *model.StEpochRelaySettings) ([]protocol.LedgerUsageRow, *model.StEpochExtenderPool, *model.StEpochExtenderRelay, error) {
	cfg, client, err := stRequire()
	if err != nil {
		return nil, nil, nil, err
	}
	authority, err := client.PayoutEpochAuthority(ctx, epoch)
	if err != nil {
		return nil, nil, nil, err
	}
	if authority == nil || authority.Epoch != epoch || authority.PolicyHash != cfg.PolicyHash {
		return nil, nil, nil, fmt.Errorf("st ledger v2: the payout authority of epoch %d is absent or differs", epoch)
	}
	transition, err := server.LoadProviderPayoutEarningPolicy(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	if start, _ := transition.SnWindow(authority.StartTime, authority.EndTime); !start.Equal(authority.StartTime) {
		return nil, nil, nil, fmt.Errorf("%w: epoch %d starts at %s, before %s", ErrStLedgerV2WindowClipped, epoch, authority.StartTime.Format(time.RFC3339), transition.Cutoff.Format(time.RFC3339))
	}
	var measurementNetworkIds map[server.Id]bool
	if amendment.InForce(epoch) {
		if pool == nil {
			return nil, nil, nil, fmt.Errorf("st ledger v2: epoch %d is covered by the extender amendment without the extender pool's configuration", epoch)
		}
		measurementNetworkIds = stExtenderPoolMeasurementNetworks(ctx, pool)
	}
	// the census is canonical: one usage per client, in client id order
	usages, extenders, err := model.GetStEpochAmendedUsageInSlices(ctx, epoch, authority.StartTime, authority.EndTime, amendment, measurementNetworkIds, slicing)
	if err != nil {
		return nil, nil, nil, err
	}
	// the relay bytes, over the same window, payers and amendment, before
	// anything is retained
	var relayed *model.StEpochExtenderRelay
	if relay != nil && extenders != nil {
		relayed, err = model.GetStEpochExtenderRelay(ctx, epoch, authority.StartTime, authority.EndTime, amendment, measurementNetworkIds, relay)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	// the rows: the roster's provider universe when one is signed, the
	// wallets, and the head flags. With retain, the selected wallets'
	// resolutions are retained, as v1 retains them, before the rows are
	// recorded.
	// a signed roster, when one is configured, fixes the provider universe
	work, workConfigured, err := stLoadProviderWorkAuthority(ctx, cfg, authority)
	if err == nil {
		usages, err = stProviderWorkUsages(ctx, work, usages)
	}
	if err != nil {
		return nil, nil, nil, err
	}
	resolutions, err := stLedgerV2Wallets(ctx, cfg, work, workConfigured, authority, usages)
	if err != nil {
		return nil, nil, nil, err
	}
	// The exit rule, from epoch 7. A provider it holds is ineligible as one
	// without a selected wallet is: its row keeps its settled bytes, which the
	// epoch's usage statement counts as it counts every row, with a zero
	// coldkey, which the allocation never pays, and no resolution is retained
	// for it. The population and its usage stay the census's, and the head
	// flag stays the chain's, which validators check (§6.2, §9.2), so nothing
	// a validator checks changes.
	differs := stLedgerV2ExitDiffers(ctx, epoch, authority)
	for clientId := range differs {
		delete(resolutions, clientId)
	}
	rows := make([]protocol.LedgerUsageRow, 0, len(usages))
	clientIds := make([][16]byte, 0, len(usages))
	var differsRows int
	var differsBytes, rowBytes uint64
	for _, usage := range usages {
		// a row has settled bytes; a roster's zero rows have none
		if usage.PayoutByteCount <= 0 {
			continue
		}
		row := protocol.LedgerUsageRow{ClientID: stId16(usage.ClientId), UsageBytes: uint64(usage.PayoutByteCount)}
		if resolution := resolutions[usage.ClientId]; resolution != nil {
			row.Coldkey = resolution.Coldkey
		}
		rowBytes += row.UsageBytes
		if differs[usage.ClientId] {
			differsRows++
			differsBytes += row.UsageBytes
		}
		rows = append(rows, row)
		clientIds = append(clientIds, row.ClientID)
	}
	if stLedgerV2ExitRuleInForce(epoch) {
		glog.Infof("[st]v2 epoch %d exit rule: %d of %d provider rows, with %d of %d bytes, earn no usage by a differing probed exit; the record holds %d differing clients in the window\n",
			epoch, differsRows, len(rows), differsBytes, rowBytes, len(differs))
	}
	excluded, err := stHeadExcludedClients(ctx, cfg, client, epoch, authority.Start.Block, authority.End.Block, clientIds)
	if err != nil {
		return nil, nil, nil, err
	}
	for i := range rows {
		rows[i].HeadExcluded = excluded[rows[i].ClientID] != nil
	}
	if !retain {
		return rows, extenders, relayed, nil
	}
	retained := make([]*model.StPayoutWalletResolution, 0, len(resolutions))
	for _, resolution := range resolutions {
		retained = append(retained, resolution)
	}
	if err := model.AddStPayoutWalletResolutionsBatched(ctx, cfg.DeploymentKey(), epoch, cfg.NoId, retained); err != nil {
		return nil, nil, nil, err
	}
	return rows, extenders, relayed, nil
}

// stLedgerV2Wallets selects each provider's earning wallet as v1 does, in
// bulk: from the signed roster's pinned heads when provider_work.yml is
// configured, else from the observed providers' newest retained consents.
// The provider's own consent wins, then its network's, then its network's
// hotkey delegation.
func stLedgerV2Wallets(ctx context.Context, cfg *StConfig, work *stProviderWorkAuthority, configured bool, authority *StPayoutEpochAuthority, usages []*model.StProviderUsage) (map[server.Id]*model.StPayoutWalletResolution, error) {
	scope := stProviderWalletEpochScope(cfg, authority)
	if configured {
		if err := stProviderWalletRosterAdmission(ctx, cfg, work, authority); err != nil {
			return nil, err
		}
		return model.GetStProviderWalletResolutionsForEpoch(ctx, work.Raw, work.Expectation, scope)
	}
	providers, err := stRetainedProviderWalletIdentities(ctx, cfg, authority, usages)
	if err != nil {
		return nil, err
	}
	return model.GetStRetainedProviderWalletResolutionsForEpoch(ctx, crypto.PubkeyToAddress(cfg.RootKey.PublicKey), scope, providers)
}
