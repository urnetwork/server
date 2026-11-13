package controller

// st_ledger_v2_rows.go — a covered epoch's provider rows (VERSION2-FINAL.md
// §6.2, §8.2) from v1's inputs, at a million providers: the usage census over
// the epoch's earning window; wallet selection by the signed roster or
// retained consents, in bulk over consent chains verified once per head; and
// head exclusion at both of the epoch's boundary blocks, reading bindingAt
// only for FleetBound candidates. Reliability is not an input.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/urfoundation/sn/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// ErrStLedgerV2WindowClipped refuses an epoch whose earning window the
// cutoff_utc would clip; every v2 epoch starts after the cutoff (§6.2).
var ErrStLedgerV2WindowClipped = errors.New("st ledger v2: the cutoff_utc clips the epoch's earning window")

// stLedgerV2NewEpochRows builds a covered epoch's provider rows for the
// configured pool from v1's inputs, read as v1 reads them: one row per
// provider with settled bytes in the earning window [timestamp(start),
// timestamp(end)) of the epoch's boundary blocks, in client id order, with
// the coldkey its wallet selection picks (zero without one) and the head flag
// of an active fleet binding at either boundary. The selected wallets'
// resolutions are retained as v1 retains them, before the rows are recorded.
func stLedgerV2NewEpochRows(ctx context.Context, epoch uint64) ([]protocol.LedgerUsageRow, error) {
	rows, _, err := stLedgerV2NewAmendedEpochRows(ctx, epoch, model.StExtenderAmendment{}, nil)
	return rows, err
}

// An epoch's rows as stLedgerV2NewEpochRows builds them, under the SN25
// extender amendment (sn WHITEPAPER.md §8.6.2). For an epoch the amendment
// covers, the rows hold the contracts' hop shares less the ones it pools (a
// platform endpoint's, and those on a contract's origin network), and the
// pool returned is the epoch's extender pool: Z and its parts, the contracts
// of declared measurement and test networks left out. For any other epoch the
// rows are exactly stLedgerV2NewEpochRows's and the pool is nil. pool is the
// extender pool's configuration, needed only when the amendment covers the
// epoch.
func stLedgerV2NewAmendedEpochRows(ctx context.Context, epoch uint64, amendment model.StExtenderAmendment, pool *stExtenderPoolConfig) ([]protocol.LedgerUsageRow, *model.StEpochExtenderPool, error) {
	return stLedgerV2AmendedEpochRows(ctx, epoch, amendment, pool, true)
}

// As stLedgerV2NewAmendedEpochRows, retaining the selected wallets'
// resolutions only when retain is set: a shadow report reads an epoch without
// recording anything (st_ledger_v2_extender_shadow.go).
func stLedgerV2AmendedEpochRows(ctx context.Context, epoch uint64, amendment model.StExtenderAmendment, pool *stExtenderPoolConfig, retain bool) ([]protocol.LedgerUsageRow, *model.StEpochExtenderPool, error) {
	cfg, client, err := stRequire()
	if err != nil {
		return nil, nil, err
	}
	authority, err := client.PayoutEpochAuthority(ctx, epoch)
	if err != nil {
		return nil, nil, err
	}
	if authority == nil || authority.Epoch != epoch || authority.PolicyHash != cfg.PolicyHash {
		return nil, nil, fmt.Errorf("st ledger v2: the payout authority of epoch %d is absent or differs", epoch)
	}
	transition, err := server.LoadProviderPayoutEarningPolicy(ctx)
	if err != nil {
		return nil, nil, err
	}
	if start, _ := transition.SnWindow(authority.StartTime, authority.EndTime); !start.Equal(authority.StartTime) {
		return nil, nil, fmt.Errorf("%w: epoch %d starts at %s, before %s", ErrStLedgerV2WindowClipped, epoch, authority.StartTime.Format(time.RFC3339), transition.Cutoff.Format(time.RFC3339))
	}
	var measurementNetworkIds map[server.Id]bool
	if amendment.InForce(epoch) {
		if pool == nil {
			return nil, nil, fmt.Errorf("st ledger v2: epoch %d is covered by the extender amendment without the extender pool's configuration", epoch)
		}
		measurementNetworkIds = stExtenderPoolMeasurementNetworks(ctx, pool)
	}
	// the census is canonical: one usage per client, in client id order
	usages, extenders, err := model.GetStEpochAmendedUsageAtEpoch(ctx, epoch, authority.StartTime, authority.EndTime, amendment, measurementNetworkIds)
	if err != nil {
		return nil, nil, err
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
		return nil, nil, err
	}
	resolutions, err := stLedgerV2Wallets(ctx, cfg, work, workConfigured, authority, usages)
	if err != nil {
		return nil, nil, err
	}
	rows := make([]protocol.LedgerUsageRow, 0, len(usages))
	clientIds := make([][16]byte, 0, len(usages))
	for _, usage := range usages {
		// a row has settled bytes; a roster's zero rows have none
		if usage.PayoutByteCount <= 0 {
			continue
		}
		row := protocol.LedgerUsageRow{ClientID: stId16(usage.ClientId), UsageBytes: uint64(usage.PayoutByteCount)}
		if resolution := resolutions[usage.ClientId]; resolution != nil {
			row.Coldkey = resolution.Coldkey
		}
		rows = append(rows, row)
		clientIds = append(clientIds, row.ClientID)
	}
	excluded, err := stHeadExcludedClients(ctx, cfg, client, epoch, authority.Start.Block, authority.End.Block, clientIds)
	if err != nil {
		return nil, nil, err
	}
	for i := range rows {
		rows[i].HeadExcluded = excluded[rows[i].ClientID] != nil
	}
	if !retain {
		return rows, extenders, nil
	}
	retained := make([]*model.StPayoutWalletResolution, 0, len(resolutions))
	for _, resolution := range resolutions {
		retained = append(retained, resolution)
	}
	if err := model.AddStPayoutWalletResolutionsBatched(ctx, cfg.DeploymentKey(), epoch, cfg.NoId, retained); err != nil {
		return nil, nil, err
	}
	return rows, extenders, nil
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
