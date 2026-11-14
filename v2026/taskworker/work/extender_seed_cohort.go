package work

import (
	"crypto/ed25519"
	"encoding/hex"
	"slices"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The cohort of the public seed bundle (connect/EXTENDER-UPDATE2.md §1.2, §2.2).
//
// The bundle is public and identical on every mirror, so whatever it holds is
// disclosed to anyone who fetches it. The cohort is therefore small, stable and
// drawn only from records the operator already discloses publicly:
//
//   - Only the open tier with no canary channel (R1, R4). A gated or
//     channel-restricted extender is never a candidate, whatever else holds
//     it; the signed record is checked again after signing
//     (extenderSeedRecordPublishable).
//   - The dns default sets' partition of each family first. The geo dns sets
//     already hand that partition to any resolver the default location
//     answers (R2), so a cohort drawn from it discloses no record the dns
//     channel does not, and fetching a mirror reveals that partition, not
//     another random slice of the fleet. A partition short of a cohort is
//     filled from the next partitions around the ring, in ring order, as the
//     dns default set is filled when its own partition is short. The pool is
//     the dns publisher's own (GetActiveNetworkExtenderDnsAddresses), so the
//     two cannot disagree about which extenders are open.
//   - Stable between planned updates. Each cohort epoch
//     (ExtenderSeedCohortEpochTimeout) ranks the candidates by a keyed order;
//     within an epoch a member stays while it is eligible, and only a vacancy
//     is filled, from the epoch's ranking. A planned update is the epoch's
//     turn, which draws the cohort again from the ranking of that epoch.
//   - Both families where possible: up to half the cohort is filled first
//     from candidates with an IPv4 address and up to half from candidates with
//     an IPv6 address, then the rest by rank, so an IPv4-only and an
//     IPv6-only client each find candidates.
//
// The partition and the ranking are keyed by the operator's directory secret,
// so every replica draws the same cohort and nobody without the root key can
// predict it.

// How long one cohort is kept before the planned update draws it again.
const ExtenderSeedCohortEpochTimeout = 7 * 24 * time.Hour

// The largest cohort, the envelope's record limit (§2.2).
const ExtenderSeedMaxRecordCount = 8

// The smallest cohort the publisher aims for, where the open pool allows: the
// client's low-water mark (connect ExtenderNetworkClientSettings.LowWaterCount,
// 4). A client that holds fewer usable records than that after applying a seed
// runs its dns bootstrap first, which can stall for its 30 second budget where
// DoH is black-holed, so a seed below it does not restore reach promptly.
func extenderSeedMinRecordCount() int {
	return min(ExtenderSeedMaxRecordCount, max(1, connect.DefaultExtenderNetworkClientSettings().LowWaterCount))
}

// The channel and vantage the cohort's ranking is keyed under. "seed" is a key
// space of its own, apart from the dns, feed and gated channels.
const (
	extenderSeedRankChannel = "seed"
	extenderSeedRankVantage = "seed"
)

// The cohort size of a configured record count: the limit when unset or above
// it, and never below the client's low-water mark.
func extenderSeedRecordCount(configured int) int {
	if configured <= 0 || ExtenderSeedMaxRecordCount < configured {
		return ExtenderSeedMaxRecordCount
	}
	return max(configured, extenderSeedMinRecordCount())
}

// One open extender the cohort may hold: its identity key, as the dns pools
// key it, and the families it has an active address of.
type extenderSeedCandidate struct {
	extenderId server.Id
	keyHex     string
	ipv4       bool
	ipv6       bool
}

// The open pool of the dns sets, by identity key. Only the open tier with no
// canary channel and a real identity key is a candidate.
func extenderSeedCandidates(addresses []*model.NetworkExtenderDnsAddress) (map[string]*extenderSeedCandidate, map[int][]string) {
	candidates := map[string]*extenderSeedCandidate{}
	pools := map[int][]string{}
	for _, address := range addresses {
		if address.DirectoryTier != connect.ExtenderDirectoryTierOpen || address.CanaryChannel != "" {
			continue
		}
		if len(address.PublicKey) != ed25519.PublicKeySize {
			// a record names its key; a keyless row has nothing to publish
			continue
		}
		if address.IpVersion != 4 && address.IpVersion != 6 {
			continue
		}
		keyHex := hex.EncodeToString(address.PublicKey)
		candidate, ok := candidates[keyHex]
		if !ok {
			candidate = &extenderSeedCandidate{
				extenderId: address.ExtenderId,
				keyHex:     keyHex,
			}
			candidates[keyHex] = candidate
		}
		switch address.IpVersion {
		case 4:
			if !candidate.ipv4 {
				pools[4] = append(pools[4], keyHex)
			}
			candidate.ipv4 = true
		case 6:
			if !candidate.ipv6 {
				pools[6] = append(pools[6], keyHex)
			}
			candidate.ipv6 = true
		}
	}
	return candidates, pools
}

// One draw of the cohort, taken a candidate at a time so that a candidate the
// publisher cannot use -- gone between the pool read and the signing, an
// address another member already lists -- is passed over without disturbing
// the order of the rest.
//
// previousKeyHexes are the identity keys of the stored bundle's records, in its
// order, when that bundle is of the same cohort epoch; nil otherwise. Each
// that is still an open candidate is offered first, in its place. Then the
// epoch's ranking of the union of the dns default partitions is offered, each
// family first up to half the cohort.
type extenderSeedCohortDraw struct {
	candidates map[string]*extenderSeedCandidate
	previous   []string
	ranked     []string
	count      int
	quota      int
	offered    map[string]bool
	chosen     []*extenderSeedCandidate
	ipv4Count  int
	ipv6Count  int
}

func newExtenderSeedCohortDraw(
	addresses []*model.NetworkExtenderDnsAddress,
	secret []byte,
	epoch uint64,
	previousKeyHexes []string,
	count int,
) *extenderSeedCohortDraw {
	count = extenderSeedRecordCount(count)
	candidates, pools := extenderSeedCandidates(addresses)
	return &extenderSeedCohortDraw{
		candidates: candidates,
		previous:   slices.Clone(previousKeyHexes),
		ranked:     extenderSeedRingOrder(pools, secret, epoch),
		count:      count,
		quota:      (count + 1) / 2,
		offered:    map[string]bool{},
	}
}

// Every candidate in the order a cohort is filled from: by how far its dns
// partition is around the ring from the dns default sets' partition of its
// family (R2) -- the default partition first, which is what a resolver at the
// default location is dealt -- and within one distance by the epoch's keyed
// seed ranking. A cohort therefore takes the default partition whole before it
// takes anything else, and a partition short of a cohort is filled from the
// next ones in ring order, as the dns default set is when its partition is
// short: deterministically, never a random slice. A key in both families' pools
// takes the nearer of its two distances.
func extenderSeedRingOrder(pools map[int][]string, secret []byte, epoch uint64) []string {
	distances := map[string]int{}
	for _, ipVersion := range extenderDnsIpVersions {
		pool := slices.Clone(pools[ipVersion])
		slices.Sort(pool)
		pool = slices.Compact(pool)
		if len(pool) == 0 {
			continue
		}
		partitionCount := connect.ExtenderPartitionCount(len(pool))
		vantagePartition := connect.ExtenderVantagePartition(
			secret,
			connect.ExtenderChannelDns,
			[]byte(extenderDnsDefaultVantage),
			partitionCount,
		)
		for _, keyHex := range pool {
			partition := connect.ExtenderRecordPartition(secret, connect.ExtenderChannelDns, keyHex, partitionCount)
			distance := (partition - vantagePartition + partitionCount) % partitionCount
			if previous, ok := distances[keyHex]; !ok || distance < previous {
				distances[keyHex] = distance
			}
		}
	}
	byDistance := map[int][]string{}
	for keyHex, distance := range distances {
		byDistance[distance] = append(byDistance[distance], keyHex)
	}
	sortedDistances := []int{}
	for distance := range byDistance {
		sortedDistances = append(sortedDistances, distance)
	}
	slices.Sort(sortedDistances)
	ranked := []string{}
	for _, distance := range sortedDistances {
		ranked = append(ranked, connect.ExtenderPartitionOrder(
			secret,
			extenderSeedRankChannel,
			[]byte(extenderSeedRankVantage),
			epoch,
			byDistance[distance],
		)...)
	}
	return ranked
}

// The most preferred candidate not offered yet, or nil when the cohort is full
// or no candidate is left.
func (self *extenderSeedCohortDraw) next() *extenderSeedCandidate {
	if self.count <= len(self.chosen) {
		return nil
	}
	offer := func(keyHex string) *extenderSeedCandidate {
		candidate, ok := self.candidates[keyHex]
		if !ok || self.offered[keyHex] {
			return nil
		}
		self.offered[keyHex] = true
		return candidate
	}
	for _, keyHex := range self.previous {
		if candidate := offer(keyHex); candidate != nil {
			return candidate
		}
	}
	pick := func(wants func(candidate *extenderSeedCandidate) bool) *extenderSeedCandidate {
		for _, keyHex := range self.ranked {
			if self.offered[keyHex] || !wants(self.candidates[keyHex]) {
				continue
			}
			return offer(keyHex)
		}
		return nil
	}
	if self.ipv6Count < self.quota {
		if candidate := pick(func(candidate *extenderSeedCandidate) bool { return candidate.ipv6 }); candidate != nil {
			return candidate
		}
	}
	if self.ipv4Count < self.quota {
		if candidate := pick(func(candidate *extenderSeedCandidate) bool { return candidate.ipv4 }); candidate != nil {
			return candidate
		}
	}
	return pick(func(candidate *extenderSeedCandidate) bool { return true })
}

// Takes an offered candidate into the cohort.
func (self *extenderSeedCohortDraw) accept(candidate *extenderSeedCandidate) {
	self.chosen = append(self.chosen, candidate)
	if candidate.ipv4 {
		self.ipv4Count += 1
	}
	if candidate.ipv6 {
		self.ipv6Count += 1
	}
}

// Whether the key is an open candidate of this draw's pool.
func (self *extenderSeedCohortDraw) open(keyHex string) bool {
	_, ok := self.candidates[keyHex]
	return ok
}

// How many open candidates this draw's pool holds.
func (self *extenderSeedCohortDraw) candidateCount() int {
	return len(self.candidates)
}

// selectExtenderSeedCohort is the draw with every offered candidate accepted:
// the cohort's extender ids in bundle order.
func selectExtenderSeedCohort(
	addresses []*model.NetworkExtenderDnsAddress,
	secret []byte,
	epoch uint64,
	previousKeyHexes []string,
	count int,
) []server.Id {
	draw := newExtenderSeedCohortDraw(addresses, secret, epoch, previousKeyHexes, count)
	for candidate := draw.next(); candidate != nil; candidate = draw.next() {
		draw.accept(candidate)
	}
	extenderIds := make([]server.Id, 0, len(draw.chosen))
	for _, candidate := range draw.chosen {
		extenderIds = append(extenderIds, candidate.extenderId)
	}
	return extenderIds
}
