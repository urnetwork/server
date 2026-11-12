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

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The message of an extender configuration that is absent: a deployment
// without an extender network, which has no gated fleet.
const extenderConfigAbsentMessage = "the extender network is not configured"

// stExtenderDurableNetworkIds reads the gated, durable fleet of extender.yml
// (directory.durable_network_ids). An absent extender.yml has none. An
// unreadable one, or an id in it that does not parse, is an error: the
// operator's own fleet must not compete because its list could not be read.
func stExtenderDurableNetworkIds() ([]server.Id, error) {
	extenderConfig, err := EnvExtenderConfig()
	if err != nil {
		var configErr *extenderConfigError
		if errors.As(err, &configErr) && configErr.message == extenderConfigAbsentMessage {
			return nil, nil
		}
		return nil, fmt.Errorf("extender.yml cannot be read for its durable_network_ids: %s", err)
	}
	networkIds := []server.Id{}
	for _, networkIdStr := range extenderConfig.Directory.DurableNetworkIds {
		networkId, err := server.ParseId(strings.TrimSpace(networkIdStr))
		if err != nil {
			return nil, fmt.Errorf("extender.yml directory.durable_network_ids holds %q, which is not a network id", networkIdStr)
		}
		networkIds = append(networkIds, networkId)
	}
	return networkIds, nil
}

// stExtenderOwnNetworks are the operator's own networks (E5), one list for
// every census:
//   - Durable: its gated, durable fleet (extender.yml
//     directory.durable_network_ids);
//   - Declared: the networks it declares it controls, in st.yml
//     extender_pool.declared_network_ids and in ctf.yml
//     census.own_network_ids alike;
//   - Canary: the networks that host one of its canary extenders.
//
// An id in any list that does not parse is an error, never a dropped id.
type stExtenderOwnNetworks struct {
	Durable  map[server.Id]bool
	Declared map[server.Id]bool
	Canary   map[server.Id]bool
}

// All is every network of the three lists.
func (self *stExtenderOwnNetworks) All() map[server.Id]bool {
	all := map[server.Id]bool{}
	for _, networks := range []map[server.Id]bool{self.Durable, self.Declared, self.Canary} {
		for networkId := range networks {
			all[networkId] = true
		}
	}
	return all
}

// stExtenderCanaryNetworkIdsRead reads the networks of the operator's canary
// extenders: a seam the pure tests replace.
var stExtenderCanaryNetworkIdsRead = model.GetExtenderCanaryNetworkIds

// readStExtenderOwnNetworks reads the operator's own networks.
func readStExtenderOwnNetworks(ctx context.Context, cfg *StConfig) (*stExtenderOwnNetworks, error) {
	own := &stExtenderOwnNetworks{Durable: map[server.Id]bool{}, Declared: map[server.Id]bool{}, Canary: map[server.Id]bool{}}
	durable, err := stExtenderDurableNetworkIds()
	if err != nil {
		return nil, err
	}
	for _, networkId := range durable {
		own.Durable[networkId] = true
	}
	pool, err := stExtenderPoolSettings(cfg)
	if err != nil {
		return nil, err
	}
	for networkId := range pool.declared {
		own.Declared[networkId] = true
	}
	// read whether or not capture the flag's census is on, so the operator's
	// own networks are one list in both pools
	for _, networkIdStr := range model.CtfDataSettings().Census.OwnNetworkIds {
		networkId, err := server.ParseId(strings.TrimSpace(networkIdStr))
		if err != nil {
			return nil, fmt.Errorf("ctf.yml census.own_network_ids holds %q, which is not a network id", networkIdStr)
		}
		own.Declared[networkId] = true
	}
	for _, networkId := range stExtenderCanaryNetworkIdsRead(ctx) {
		own.Canary[networkId] = true
	}
	return own, nil
}

// stExtenderEligibilityRules gathers the facts the shared eligibility rules
// judge an epoch's records by:
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
	own, err := readStExtenderOwnNetworks(ctx, cfg)
	if err != nil {
		return nil, err
	}
	settings := VerifySettings()
	rules := &model.ExtenderEligibilityRules{
		Unit:           func(ip netip.Addr) [32]byte { return model.VerifyEgressIpHashWithSettings(ip, settings) },
		Durable:        own.Durable,
		Declared:       own.Declared,
		CanaryNetworks: own.Canary,
		Coldkeys:       map[server.Id][32]byte{},
		HeadBound:      map[server.Id]bool{},
	}
	if len(records) == 0 {
		return rules, nil
	}
	// the provider clients, once each, in client id order
	networks := map[server.Id]server.Id{}
	for _, record := range records {
		if _, ok := networks[record.ClientId]; !ok {
			networks[record.ClientId] = record.NetworkId
		}
	}
	clientIds := slices.SortedFunc(maps.Keys(networks), func(a, b server.Id) int { return a.Cmp(b) })
	usages := make([]*model.StProviderUsage, len(clientIds))
	ids16 := make([][16]byte, len(clientIds))
	for i, clientId := range clientIds {
		usages[i] = &model.StProviderUsage{ClientId: clientId, NetworkId: networks[clientId]}
		ids16[i] = stId16(clientId)
	}
	work, workConfigured, err := stLoadProviderWorkAuthority(ctx, cfg, authority)
	if err != nil {
		return nil, err
	}
	resolutions, err := stLedgerV2Wallets(ctx, cfg, work, workConfigured, authority, usages)
	if err != nil {
		return nil, err
	}
	for clientId, resolution := range resolutions {
		if resolution != nil && resolution.Coldkey != ([32]byte{}) {
			rules.Coldkeys[clientId] = resolution.Coldkey
		}
	}
	bound, err := stHeadExcludedClients(ctx, cfg, client, authority.Epoch, authority.Start.Block, authority.End.Block, ids16)
	if err != nil {
		return nil, err
	}
	for clientId := range bound {
		rules.HeadBound[server.Id(clientId)] = true
	}
	return rules, nil
}
