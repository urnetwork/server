package controller

// extender_eligibility.go — the facts of the shared extender eligibility rules
// (model/extender_eligibility.go, sn WHITEPAPER.md §8.6.2 E1 and E3–E5) as
// every census that judges extenders reads them: the unit key, the operator's
// own networks, the provider rows' wallets and the fleet bindings at the
// epoch's boundaries. The extender pool's census (st_ledger_v2_extenders.go)
// and capture the flag's (X1, X4–X6 of sn CAPTURETHEFLAG.md §4.2) read the
// same facts, so neither pool judges an extender by a list the other does not
// read.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The operator's own networks (E5), one list for every census. An id in any
// list that does not parse is an error, never a dropped id.
type stExtenderOwnNetworks struct {
	// the gated, durable fleet (extender.yml directory.durable_network_ids)
	durableNetworkIds map[server.Id]bool
	// the networks the operator declares it controls, in st.yml
	// extender_pool.declared_network_ids and in ctf.yml census.own_network_ids
	// alike
	declaredNetworkIds map[server.Id]bool
	// the networks that host one of its canary extenders
	canaryNetworkIds map[server.Id]bool
}

// Every network of the three lists.
func (self *stExtenderOwnNetworks) allNetworkIds() map[server.Id]bool {
	allNetworkIds := map[server.Id]bool{}
	for _, networkIds := range []map[server.Id]bool{self.durableNetworkIds, self.declaredNetworkIds, self.canaryNetworkIds} {
		maps.Copy(allNetworkIds, networkIds)
	}
	return allNetworkIds
}

// Reads the networks of the operator's canary extenders: a seam the pure tests
// replace.
var stExtenderCanaryNetworkIdsRead = model.GetExtenderCanaryNetworkIds

// Reads the operator's own networks. An absent extender.yml has no durable
// fleet; an unreadable one is an error, as is an id in any list that does not
// parse: the operator's own networks must not compete because a list could not
// be read.
func readStExtenderOwnNetworks(ctx context.Context, cfg *StConfig) (*stExtenderOwnNetworks, error) {
	ownNetworks := &stExtenderOwnNetworks{
		durableNetworkIds:  map[server.Id]bool{},
		declaredNetworkIds: map[server.Id]bool{},
		canaryNetworkIds:   map[server.Id]bool{},
	}
	parse := func(list string, networkIdStrs []string, networkIds map[server.Id]bool) error {
		for _, networkIdStr := range networkIdStrs {
			networkId, err := server.ParseId(strings.TrimSpace(networkIdStr))
			if err != nil {
				return fmt.Errorf("%s holds %q, which is not a network id", list, networkIdStr)
			}
			networkIds[networkId] = true
		}
		return nil
	}
	extenderConfig, err := EnvExtenderConfig()
	if err != nil {
		var configErr *extenderConfigError
		// an absent configuration is a deployment without an extender network
		if !errors.As(err, &configErr) || configErr.message != "the extender network is not configured" {
			return nil, fmt.Errorf("extender.yml cannot be read for its durable_network_ids: %s", err)
		}
	} else if err := parse("extender.yml directory.durable_network_ids", extenderConfig.Directory.DurableNetworkIds, ownNetworks.durableNetworkIds); err != nil {
		return nil, err
	}
	pool, err := stExtenderPoolSettings(cfg)
	if err != nil {
		return nil, err
	}
	maps.Copy(ownNetworks.declaredNetworkIds, pool.declaredNetworkIds)
	// read whether or not capture the flag's census is on, so the operator's
	// own networks are one list in both pools
	if err := parse("ctf.yml census.own_network_ids", model.CtfDataSettings().Census.OwnNetworkIds, ownNetworks.declaredNetworkIds); err != nil {
		return nil, err
	}
	for _, networkId := range stExtenderCanaryNetworkIdsRead(ctx) {
		ownNetworks.canaryNetworkIds[networkId] = true
	}
	return ownNetworks, nil
}

// Gathers the facts the shared eligibility rules judge an epoch's records by:
//   - units keyed as the head's egress prefix (§8.4);
//   - the operator's own networks (readStExtenderOwnNetworks);
//   - each provider client's coldkey, by the provider rows' wallet selection
//     for its record's network;
//   - and head bindings at either of the epoch's boundary blocks.
//
// The wallet resolutions are not retained here, so a census can never
// disagree with the retention the provider rows make of the same clients'
// wallets.
func stExtenderEligibilityRules(ctx context.Context, cfg *StConfig, client StClient, authority *StPayoutEpochAuthority, records []*model.StExtenderCensusRecord) (*model.ExtenderEligibilityRules, error) {
	ownNetworks, err := readStExtenderOwnNetworks(ctx, cfg)
	if err != nil {
		return nil, err
	}
	settings := VerifySettings()
	rules := &model.ExtenderEligibilityRules{
		Unit:               func(ip netip.Addr) [32]byte { return model.VerifyEgressIpHashWithSettings(ip, settings) },
		DurableNetworkIds:  ownNetworks.durableNetworkIds,
		DeclaredNetworkIds: ownNetworks.declaredNetworkIds,
		CanaryNetworkIds:   ownNetworks.canaryNetworkIds,
		ClientIdColdkeys:   map[server.Id][32]byte{},
		HeadBoundClientIds: map[server.Id]bool{},
	}
	if len(records) == 0 {
		return rules, nil
	}
	// the provider clients, once each, in client id order
	clientIdNetworkIds := map[server.Id]server.Id{}
	for _, record := range records {
		if _, ok := clientIdNetworkIds[record.ClientId]; !ok {
			clientIdNetworkIds[record.ClientId] = record.NetworkId
		}
	}
	clientIds := slices.SortedFunc(maps.Keys(clientIdNetworkIds), func(a, b server.Id) int { return a.Cmp(b) })
	usages := make([]*model.StProviderUsage, len(clientIds))
	clientId16s := make([][16]byte, len(clientIds))
	for i, clientId := range clientIds {
		usages[i] = &model.StProviderUsage{ClientId: clientId, NetworkId: clientIdNetworkIds[clientId]}
		clientId16s[i] = stId16(clientId)
	}
	work, workConfigured, err := stLoadProviderWorkAuthority(ctx, cfg, authority)
	if err != nil {
		return nil, err
	}
	clientIdResolutions, err := stLedgerV2Wallets(ctx, cfg, work, workConfigured, authority, usages)
	if err != nil {
		return nil, err
	}
	for clientId, resolution := range clientIdResolutions {
		if resolution != nil && resolution.Coldkey != ([32]byte{}) {
			rules.ClientIdColdkeys[clientId] = resolution.Coldkey
		}
	}
	headBoundClientId16s, err := stHeadExcludedClients(ctx, cfg, client, authority.Epoch, authority.Start.Block, authority.End.Block, clientId16s)
	if err != nil {
		return nil, err
	}
	for clientId16 := range headBoundClientId16s {
		rules.HeadBoundClientIds[server.Id(clientId16)] = true
	}
	return rules, nil
}
