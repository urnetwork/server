// Production payout wallet selection shares the independently retained roster
// and authenticated epoch used by the complete work census.
package controller

import (
	"context"
	"crypto/sha256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The source of an observed provider's release payout wallet, or why it has
// none. These are the label values of stPayoutWalletProviders.
const (
	StPayoutWalletSourceRoster          = "roster"
	StPayoutWalletSourceProviderConsent = "provider_consent"
	StPayoutWalletSourceNetworkConsent  = "network_consent"
	StPayoutWalletSourceHotkeyConsent   = "hotkey_consent"
	// no signed evidence: the newest account-history wallet as of the epoch end
	StPayoutWalletSourceAccountHistory = "account_history"
	// no signed evidence, and that account-history wallet names another
	// network than the provider's usage
	StPayoutWalletSourceAccountHistoryOtherNetwork = "account_history_other_network"
	// no signed evidence and no account-history wallet
	StPayoutWalletSourceUnmapped = "unmapped"
	// signed evidence exists and was refused
	StPayoutWalletSourceRefused = "refused"
)

// Every source, in precedence order.
var StPayoutWalletSources = []string{
	StPayoutWalletSourceRoster,
	StPayoutWalletSourceProviderConsent,
	StPayoutWalletSourceNetworkConsent,
	StPayoutWalletSourceHotkeyConsent,
	StPayoutWalletSourceAccountHistory,
	StPayoutWalletSourceAccountHistoryOtherNetwork,
	StPayoutWalletSourceUnmapped,
	StPayoutWalletSourceRefused,
}

// Each selection counts each observed provider once, so a retried close
// counts its providers again.
var stPayoutWalletProviders = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "st",
	Name:      "payout_wallet_providers_total",
	Help:      "Release payout providers by the source that selected their wallet, or why none was selected",
}, []string{"source"})

func init() {
	for _, source := range StPayoutWalletSources {
		stPayoutWalletProviders.WithLabelValues(source)
	}
	prometheus.MustRegister(stPayoutWalletProviders)
}

// The wallet source of every observed provider of one release payout.
type StPayoutWalletSelection struct {
	// a configured roster is the only wallet authority
	RosterConfigured bool
	// each observed provider's wallet source, or why it has none
	Sources map[server.Id]string
	// the protocol's refusal of each refused provider's signed evidence; no
	// other source pays these providers
	Refused map[server.Id]error
}

// Observed providers per source. Every source is present.
func (self *StPayoutWalletSelection) Counts() map[string]int {
	counts := make(map[string]int, len(StPayoutWalletSources))
	for _, source := range StPayoutWalletSources {
		counts[source] = 0
	}
	for _, source := range self.Sources {
		counts[source] += 1
	}
	return counts
}

// A configured provider work roster is the only wallet authority, and its
// missing epoch original holds the payout. Until one is configured, retained
// signed consents select wallets with the roster's precedence, verification
// and prospective gate: the provider's own consent, then its network's, then
// the hotkey delegation. Only a provider with no signed evidence at all, or
// none effective at the epoch, falls back to its newest account-history wallet
// as of the epoch end, and only when that wallet names the provider's observed
// network. Refused evidence is never replaced by another source. Each provider
// paid without a roster is logged, and every provider is counted by source.
func stSelectReleasePayoutWallets(ctx context.Context, cfg *StConfig, approved *stProviderWorkAuthority, rosterConfigured bool, epoch *StPayoutEpochAuthority, usages []*model.StProviderUsage) (map[server.Id]*model.StProviderWallet, *StPayoutWalletSelection, error) {
	selection := &StPayoutWalletSelection{RosterConfigured: rosterConfigured, Sources: make(map[server.Id]string, len(usages)), Refused: map[server.Id]error{}}
	if rosterConfigured {
		wallets, err := stProviderWalletsForEpoch(ctx, cfg, approved, epoch)
		if err != nil {
			return nil, nil, err
		}
		for clientId := range wallets {
			selection.Sources[clientId] = StPayoutWalletSourceRoster
		}
		stCountPayoutWalletSources(selection)
		return wallets, selection, nil
	}
	retained, err := stRetainedProviderWalletSelectionForEpoch(ctx, cfg, epoch, usages)
	if err != nil {
		return nil, nil, err
	}
	// read only when some provider has no signed evidence
	var accountHistory map[server.Id]*model.StProviderWallet
	if len(retained.Absent) != 0 {
		accountHistory = model.GetStProviderWalletsAt(ctx, epoch.EndTime)
	}
	wallets := make(map[server.Id]*model.StProviderWallet, len(usages))
	for _, usage := range usages {
		clientId := usage.ClientId
		if wallet := retained.Wallets[clientId]; wallet != nil {
			source, description := stPayoutWalletConsentSource(wallet)
			if source == "" {
				return nil, nil, protocol.ErrWalletMappingIntegrity
			}
			wallets[clientId] = wallet
			selection.Sources[clientId] = source
			glog.Warningf("[st]epoch %d provider %s paid to the wallet of its %s: no provider work roster is configured\n", epoch.Epoch, clientId, description)
			continue
		}
		if reason, refused := retained.Refused[clientId]; refused {
			selection.Sources[clientId] = StPayoutWalletSourceRefused
			selection.Refused[clientId] = reason
			continue
		}
		if _, absent := retained.Absent[clientId]; !absent {
			// the retained selection places every observed provider exactly once
			return nil, nil, protocol.ErrWalletMappingIntegrity
		}
		switch wallet := accountHistory[clientId]; {
		case wallet == nil || wallet.ColdkeyPubkey == ([32]byte{}):
			selection.Sources[clientId] = StPayoutWalletSourceUnmapped
		case wallet.NetworkId != usage.NetworkId:
			selection.Sources[clientId] = StPayoutWalletSourceAccountHistoryOtherNetwork
			glog.Warningf("[st]epoch %d provider %s unpaid: it has no signed wallet evidence and its account-history wallet names another network\n", epoch.Epoch, clientId)
		default:
			wallets[clientId] = wallet
			selection.Sources[clientId] = StPayoutWalletSourceAccountHistory
			glog.Warningf("[st]epoch %d provider %s paid to its account-history wallet: it has no signed wallet evidence and no provider work roster is configured\n", epoch.Epoch, clientId)
		}
	}
	counts := stCountPayoutWalletSources(selection)
	glog.Warningf(
		"[st]epoch %d payout wallets of %d providers without a provider work roster: own consent %d, network consent %d, hotkey delegation %d, account history %d; unpaid: refused %d, account history on another network %d, no wallet %d\n",
		epoch.Epoch, len(usages),
		counts[StPayoutWalletSourceProviderConsent], counts[StPayoutWalletSourceNetworkConsent], counts[StPayoutWalletSourceHotkeyConsent], counts[StPayoutWalletSourceAccountHistory],
		counts[StPayoutWalletSourceRefused], counts[StPayoutWalletSourceAccountHistoryOtherNetwork], counts[StPayoutWalletSourceUnmapped],
	)
	return wallets, selection, nil
}

// The source of a wallet a retained signed consent selected, by the chain
// whose consent names it. Empty for any other wallet.
func stPayoutWalletConsentSource(wallet *model.StProviderWallet) (source string, description string) {
	if wallet == nil || wallet.Resolution == nil {
		return "", ""
	}
	switch wallet.Resolution.Mode {
	case protocol.EarningWalletModeProvider:
		return StPayoutWalletSourceProviderConsent, "own signed consent"
	case protocol.EarningWalletModeNetwork:
		return StPayoutWalletSourceNetworkConsent, "network's signed consent"
	case protocol.EarningWalletModeHotkey:
		return StPayoutWalletSourceHotkeyConsent, "network's hotkey delegation"
	default:
		return "", ""
	}
}

func stCountPayoutWalletSources(selection *StPayoutWalletSelection) map[string]int {
	counts := selection.Counts()
	for source, count := range counts {
		stPayoutWalletProviders.WithLabelValues(source).Add(float64(count))
	}
	return counts
}

// Missing independent wallet authority holds this payout only. It cannot
// borrow the current directory, a legacy login or a later projection; startup
// and the existing account-display and legacy-payment APIs remain separate.
func stProviderWalletsForEpoch(ctx context.Context, cfg *StConfig, approved *stProviderWorkAuthority, epoch *StPayoutEpochAuthority) (map[server.Id]*model.StProviderWallet, error) {
	if err := stProviderWalletRosterAdmission(ctx, cfg, approved, epoch); err != nil {
		return nil, err
	}
	return model.GetStProviderWalletsForEpoch(ctx, approved.Raw, approved.Expectation, stProviderWalletEpochScope(cfg, epoch))
}

// The roster selection's preconditions, shared with settlement v2's bulk
// selection.
func stProviderWalletRosterAdmission(ctx context.Context, cfg *StConfig, approved *stProviderWorkAuthority, epoch *StPayoutEpochAuthority) error {
	if ctx == nil || cfg == nil || cfg.RootKey == nil || approved == nil || epoch == nil {
		return protocol.ErrWalletMappingUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if cfg.Netuid == 0 || cfg.Netuid > 65535 || cfg.PolicyHash != epoch.PolicyHash || approved.Expectation.ClientKeyRootSigner != crypto.PubkeyToAddress(cfg.RootKey.PublicKey) {
		return protocol.ErrWalletMappingIntegrity
	}
	return nil
}

// Before provider_work.yml is provisioned there is no roster to pin heads. The
// observed providers' newest retained chains, each signed by its coldkey and
// issued under the configured root key, select wallets under the roster's
// precedence, verification and prospective gate. A provider without accepted
// evidence is reported absent or refused rather than holding the payout.
func stRetainedProviderWalletSelectionForEpoch(ctx context.Context, cfg *StConfig, epoch *StPayoutEpochAuthority, usages []*model.StProviderUsage) (*model.StRetainedProviderWalletSelection, error) {
	providers, err := stRetainedProviderWalletIdentities(ctx, cfg, epoch, usages)
	if err != nil {
		return nil, err
	}
	return model.GetStRetainedProviderWalletSelectionForEpoch(ctx, crypto.PubkeyToAddress(cfg.RootKey.PublicKey), stProviderWalletEpochScope(cfg, epoch), providers)
}

// The retained selection's preconditions and the observed providers' wallet
// identities, shared with settlement v2's bulk selection.
func stRetainedProviderWalletIdentities(ctx context.Context, cfg *StConfig, epoch *StPayoutEpochAuthority, usages []*model.StProviderUsage) ([]model.StProviderWalletIdentity, error) {
	if ctx == nil || cfg == nil || cfg.RootKey == nil || epoch == nil {
		return nil, protocol.ErrWalletMappingUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.Netuid == 0 || cfg.Netuid > 65535 || cfg.PolicyHash != epoch.PolicyHash {
		return nil, protocol.ErrWalletMappingIntegrity
	}
	providers := make([]model.StProviderWalletIdentity, len(usages))
	for index, usage := range usages {
		if usage == nil {
			return nil, protocol.ErrWalletMappingIntegrity
		}
		providers[index] = model.StProviderWalletIdentity{ClientId: usage.ClientId, NetworkId: usage.NetworkId}
	}
	return providers, nil
}

// The configured operator domain under the authenticated epoch's policy and
// boundaries, shared by both head sources.
func stProviderWalletEpochScope(cfg *StConfig, epoch *StPayoutEpochAuthority) model.StProviderWalletEpochScope {
	domain := protocol.ClientKeyHistoryDomain{ChainID: cfg.ChainId, GenesisHash: cfg.GenesisHash, Netuid: uint16(cfg.Netuid), Coordinator: cfg.ContractAddress, SettlementVault: cfg.SettlementVault, DeploymentIDHash: sha256.Sum256([]byte(cfg.DeploymentId)), PolicyHash: epoch.PolicyHash, NoID: cfg.NoId}
	return model.StProviderWalletEpochScope{Domain: domain, Epoch: epoch.Epoch, Start: payoutartifact.Boundary{Number: epoch.Start.Block, Hash: common.Hash(epoch.Start.Hash).Hex()}, End: payoutartifact.Boundary{Number: epoch.End.Block, Hash: common.Hash(epoch.End.Hash).Hex()}, StartTime: epoch.StartTime}
}
