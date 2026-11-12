// From the SN25 extender amendment's first epoch, an epoch reads each contract's
// hop shares either as provider usage or as the extender pool's bytes.
package model

import (
	"fmt"
	"math"
	"time"

	"github.com/urnetwork/server"
)

// The SN25 extender amendment (sn WHITEPAPER.md §8.6.2) as the epoch reader
// applies it to a contract's usage snapshot. The zero value is no amendment in
// force, which reads every share as provider usage, as §8.2 counts it today.
//
// The owner's answer to the amendment's Q7 (2026-11-11), "extenders count for
// one of n in payout weighting for contracts", fixes how a contract's bytes
// divide. The snapshot's even split over the contract's n hops stands exactly
// as newContractUsageSnapshot made it: the egress, every stream intermediary
// and the provider client of every extender party, each client once, less the
// origin. An extender party is one of the n (connect/EXTENDER.md J3), and so
// are a platform endpoint and a hop on the origin's network. The amendment
// only decides where each share goes: a platform endpoint's share and a share
// on the origin's network are the contract's extender pool bytes z_c, and
// every other share, an extender party's included, stays provider usage. It
// never re-splits a contract, so no share passes to another hop and no byte is
// credited both to a provider and to the pool.
type stExtenderAmendment struct {
	// E_x, the first epoch the amendment covers, as its signed amendment entry
	// fixes it (sn WHITEPAPER.md §8.6.7); 0 while no entry is in force
	FirstEpoch uint64
	// the NO's registered platform endpoints (st_platform_endpoint.go), which
	// hold a hop share but earn none: a share is a platform endpoint's on a
	// contract created while its registration was in force. Empty until
	// sidecar's edges are registered.
	PlatformEndpoints StPlatformEndpointSet
}

// Whether the amendment covers epoch.
func (self stExtenderAmendment) InForce(epoch uint64) bool {
	return self.FirstEpoch != 0 && self.FirstEpoch <= epoch
}

// One contract's usage snapshot as one epoch reads it. The provider shares and
// the two pool parts sum to the snapshot's bytes.
type stContractEpochShares struct {
	// the shares that are provider usage (§8.2), in the snapshot's client order
	Providers []contractProviderUsage
	// the platform endpoints' shares: z_c's intra-platform bytes
	IntraPlatform ByteCount
	// the shares on the origin's network: z_c's intra-network bytes
	IntraNetwork ByteCount
	// the shares that are z_c, both parts, in the snapshot's client order
	Pooled []contractProviderUsage
}

// The contract's extender pool bytes z_c.
func (self *stContractEpochShares) ExtenderPoolBytes() ByteCount {
	return self.IntraPlatform + self.IntraNetwork
}

// Reads a contract's shares for epoch. Before the amendment's first epoch every
// share is provider usage. From it, the share of a platform endpoint registered
// when the contract was created (createTime) is intra-platform, even on the
// origin's network; any other share on the origin's network is intra-network
// (contractShareIsIntraNetwork); and every other share is provider usage,
// unchanged. Each share moves whole, to one place.
func stContractSharesForEpoch(
	snapshot *contractUsageSnapshot,
	epoch uint64,
	originNetworkId server.Id,
	createTime time.Time,
	amendment stExtenderAmendment,
) (*stContractEpochShares, error) {
	if snapshot == nil {
		return nil, fmt.Errorf("missing contract usage snapshot")
	}
	inForce := amendment.InForce(epoch)
	if inForce && originNetworkId == (server.Id{}) {
		return nil, fmt.Errorf("the extender amendment needs the contract's origin network")
	}
	shares := &stContractEpochShares{
		Providers: make([]contractProviderUsage, 0, len(snapshot.Providers)),
	}
	var total int64
	for _, provider := range snapshot.Providers {
		if provider.ByteCount < 0 || int64(provider.ByteCount) > math.MaxInt64-total {
			return nil, fmt.Errorf("invalid or overflowing contract provider usage")
		}
		total += int64(provider.ByteCount)
		switch {
		case inForce && amendment.PlatformEndpoints.Holds(provider.ClientId, createTime):
			shares.IntraPlatform += provider.ByteCount
			shares.Pooled = append(shares.Pooled, provider)
		case inForce && contractShareIsIntraNetwork(provider.NetworkId, originNetworkId):
			shares.IntraNetwork += provider.ByteCount
			shares.Pooled = append(shares.Pooled, provider)
		default:
			shares.Providers = append(shares.Providers, provider)
		}
	}
	if total != int64(snapshot.ByteCount) {
		return nil, fmt.Errorf("nonconserving contract provider usage")
	}
	return shares, nil
}
