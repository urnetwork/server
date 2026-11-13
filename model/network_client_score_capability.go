// The capability side of the score export and its readers (FP2-PROTOCOL.md
// §4.3, §4.4, §12.4): the classified copies of downgraded providers in their
// own facets and native snapshot, the named-provider sets that label a
// client-id spec, and the post-quantum signal strict requests read once the
// post-quantum marker is set.
//
// Today's facets d, 4 and 6 keep the strict decision. A downgraded
// provider's record there carries CapabilityDowngrades, and its classified
// copy is written only to the capability facet of its class: i (B, a
// dual-stack provider whose IPv6 was dropped), t (C) or x (D). Every
// capability facet is written in the non-forced pool of each rank mode, an
// empty one included, so a reader can tell an exporter that predates them
// from an empty class. The forced pools gain nothing: they already hold
// every provider past the common gates, and a selector-mode forced read
// classes each record by its CapabilityDowngrades.
package model

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// The capability facets: one character each, like the family facets, under
// the same key families (clientScoreLocationFacetCountsKey).
const (
	// class B from a dual-stack provider whose IPv6 was dropped
	clientScoreCapabilityFacetIpv4Dualproto ipFamilyFacet = "i"
	// class C: a dual-stack provider whose UDP was dropped
	clientScoreCapabilityFacetDualstackTcp ipFamilyFacet = "t"
	// class D: a v4-only provider whose UDP was dropped, or a dual-stack one
	// that lost both
	clientScoreCapabilityFacetIpv4Tcp ipFamilyFacet = "x"
)

// Every capability facet the export writes, in a fixed order; exactly the
// facets of the native capability snapshot's manifest.
var clientScoreCapabilityFacets = []ipFamilyFacet{
	clientScoreCapabilityFacetIpv4Dualproto,
	clientScoreCapabilityFacetDualstackTcp,
	clientScoreCapabilityFacetIpv4Tcp,
}

// The capability facet of a downgraded class; A has none.
func clientScoreCapabilityFacetOf(class providerCapabilityClass) (ipFamilyFacet, bool) {
	switch class {
	case providerCapabilityClassB:
		return clientScoreCapabilityFacetIpv4Dualproto, true
	case providerCapabilityClassC:
		return clientScoreCapabilityFacetDualstackTcp, true
	case providerCapabilityClassD:
		return clientScoreCapabilityFacetIpv4Tcp, true
	default:
		return "", false
	}
}

// The native capability snapshot of a target (§4.4): its own pointer, slots
// and manifest beside today's :native_v1, whose readers reject any manifest
// whose facet count is not three.
func clientScoreNativeCapabilityKey(countsKey string) string {
	return countsKey + ":native_cap_v1"
}

// What the export decided for one provider beyond the strict decision.
type clientScoreCapability struct {
	classification providerCapabilityClassification
	// the classified decision and the counts it read, for a downgraded
	// provider only (§3.4)
	decision  providerEgressDecision
	counts    ProviderEgressHealthCounts
	hasCounts bool
	// the newest post-quantum outcome in the window is positive (§12.4)
	postQuantumPositive   bool
	postQuantumValidUntil time.Time
	// post-quantum positive with the latest modern evidence advertising
	// resizable windows: ClientScore.Modern once the post-quantum marker is
	// set (§12.4)
	postQuantumModern           bool
	postQuantumModernValidUntil time.Time
}

// Classifies one decided provider of the export. A v6-only provider is never
// downgraded: the two-axis model assumes IPv4, so selector mode never draws
// one and it has no capability copy (§3.6).
func newClientScoreCapability(
	clientId server.Id,
	ipFamilies uint8,
	egress *clientScoreEgress,
	countFilter providerCountFilter,
	egressSettings *EgressIndexSettings,
	egressTestEnabled bool,
) *clientScoreCapability {
	counts, hasCounts := countFilter.healthCounts[clientId]
	capability := &clientScoreCapability{
		classification: classifyProviderCapabilityOfFamilies(counts, ipFamilies, countFilter.now, egressSettings),
	}
	if counts.postQuantumPositive() {
		capability.postQuantumValidUntil = counts.PostQuantumPositiveAt.Add(ProviderEgressHealthMaxAge)
		if snapshotEnd := countFilter.healthWindowEnd.Add(egressSettings.PostQuantumMaxStaleness); snapshotEnd.Before(capability.postQuantumValidUntil) {
			capability.postQuantumValidUntil = snapshotEnd
		}
		capability.postQuantumPositive = countFilter.now.Before(capability.postQuantumValidUntil)
	}
	capability.postQuantumModern = providerPostQuantumModern(capability.postQuantumPositive, countFilter.modernEvidence[clientId], countFilter.now)
	if capability.postQuantumModern {
		capability.postQuantumModernValidUntil = capability.postQuantumValidUntil
		if windowEnd := countFilter.modernEvidence[clientId].MeasuredAt.Add(ProviderModernEvidenceMaxAge); windowEnd.Before(capability.postQuantumModernValidUntil) {
			capability.postQuantumModernValidUntil = windowEnd
		}
	}
	if capability.classification.downgraded() {
		facts := countFilter.classifiedEgressFacts(clientId, egress.publishedCountryCode, egress.egressIndex, egress.egressQuality, egressSettings, capability.classification)
		capability.decision = decideProviderEgress(facts, egressTestEnabled)
		capability.counts = capability.classification.classifiedCounts(counts)
		capability.hasCounts = hasCounts
	}
	return capability
}

// Strict requests need a still-current positive, including on forced and
// Online fallback draws. Ordinary URL freshness does not renew this proof.
func (self *ClientScore) postQuantumCurrent(now time.Time) bool {
	return self.PostQuantumPositive && now.Before(self.PostQuantumValidUntil)
}

// Stamps the same bounded proof on every strict/classified exported score.
func (self *clientScoreCapability) setPostQuantum(score *ClientScore) {
	score.PostQuantumPositive = self.postQuantumPositive
	score.PostQuantumValidUntil = self.postQuantumValidUntil
	score.PostQuantumModernValidUntil = self.postQuantumModernValidUntil
}

// A versioned hash beside each existing set keeps old readers and writers
// compatible. Missing deadlines fail closed after the PQ ready marker.
func providerPostQuantumProofKey(setKey string) string {
	return setKey + ":proof_v1"
}

// Sets a classified copy's scores and tiers from the ranking index of its
// classified counts (FP2-PROTOCOL.md §3.4): the index base of Quality, zero
// for Speed, as clientScoreRankModeBases gives a score with an index, over
// the performance of its lowest lookback. The copy keeps the strict tiers
// when its counts give no current evidence. The minimum scores, which the
// selection weight reads, have a zero base either way and do not change.
func rankClientScoreCapabilityTiers(classified *ClientScore, counts ProviderEgressHealthCounts, now time.Time, egressSettings *EgressIndexSettings) {
	index := ComputeEgressIndex(&EgressHealthRun{MeasuredAt: counts.MeasuredAt, OkCount: counts.OKCount, Total: counts.Total}, now, egressSettings)
	if !index.Evidence || len(classified.LookbackClientScores) == 0 {
		return
	}
	minLookbackIndex := -1
	for lookbackIndex := range classified.LookbackClientScores {
		if minLookbackIndex < 0 || lookbackIndex < minLookbackIndex {
			minLookbackIndex = lookbackIndex
		}
	}
	lookback := classified.LookbackClientScores[minLookbackIndex]
	ranked := &ClientScore{
		Scores: map[string]int{},
		Tiers:  map[string]int{},
	}
	rankModeBases, rankModeMinimumBases := clientScoreRankModeBases(0, 0, &index.Index)
	setClientScoreRanks(
		ranked,
		rankModeBases,
		rankModeMinimumBases,
		lookback.MinRelativeLatencyMillis,
		lookback.MaxBytesPerSecond,
		lookback.HasLatencyTest,
		lookback.HasSpeedTest,
	)
	classified.Scores = ranked.Scores
	classified.Tiers = ranked.Tiers
}

// Modern as strict requests read it once the post-quantum marker is set
// (§12.4): post-quantum positive, with the latest modern evidence fresh and
// advertising resizable windows.
func providerPostQuantumModern(postQuantumPositive bool, record *ProviderModernEvidenceRecord, now time.Time) bool {
	return postQuantumPositive &&
		record != nil &&
		record.WindowResizable &&
		record.MeasuredAt.After(now.Add(-ProviderModernEvidenceMaxAge))
}

// The named-provider sets (§4.4, §12.4), replaced every export with its ttl,
// each with a ready member, like {provider_hard_exclusions}. FindProviders2
// labels a client-id spec from the capability sets and, once the
// post-quantum marker is set, answers a strict post-quantum request's named
// providers from the post-quantum sets.
const (
	providerCapabilityDualstackKey   = "{provider_capability_dualstack}"
	providerCapabilityTcpKey         = "{provider_capability_tcp}"
	providerPostQuantumPositiveKey   = "{provider_post_quantum_positive}"
	providerPostQuantumModernKey     = "{provider_modern}"
	providerCapabilitySetReadyMember = "ready:v1"
)

// Replaces the four sets from the export's decisions. Each set is its own
// cluster slot, so each is replaced in its own transaction; a reader that
// finds a set without its ready member reads every provider as absent.
func publishProviderCapabilitySets(ctx context.Context, clientIdCapabilities map[server.Id]*clientScoreCapability, clientIdIpFamilies map[server.Id]uint8, ttl time.Duration) error {
	keyMembers := map[string][]any{
		providerCapabilityDualstackKey: {providerCapabilitySetReadyMember},
		providerCapabilityTcpKey:       {providerCapabilitySetReadyMember},
		providerPostQuantumPositiveKey: {providerCapabilitySetReadyMember},
		providerPostQuantumModernKey:   {providerCapabilitySetReadyMember},
	}
	keyProofs := map[string]map[string]any{
		providerPostQuantumPositiveKey: {providerCapabilitySetReadyMember: "1"},
		providerPostQuantumModernKey:   {providerCapabilitySetReadyMember: "1"},
	}
	for clientId, capability := range clientIdCapabilities {
		member := clientId.String()
		ipFamily := (&ClientScore{IpFamilies: clientIdIpFamilies[clientId]}).IpFamily()
		if ipFamily == connect.IpFamilyDualstack && capability.classification.downgrades&ClientScoreCapabilityDowngradeIpv6 == 0 {
			keyMembers[providerCapabilityDualstackKey] = append(keyMembers[providerCapabilityDualstackKey], member)
		}
		if capability.classification.downgrades&ClientScoreCapabilityDowngradeUdp != 0 {
			keyMembers[providerCapabilityTcpKey] = append(keyMembers[providerCapabilityTcpKey], member)
		}
		if capability.postQuantumPositive {
			keyMembers[providerPostQuantumPositiveKey] = append(keyMembers[providerPostQuantumPositiveKey], member)
			keyProofs[providerPostQuantumPositiveKey][member] = capability.postQuantumValidUntil.UnixNano()
		}
		if capability.postQuantumModern {
			keyMembers[providerPostQuantumModernKey] = append(keyMembers[providerPostQuantumModernKey], member)
			keyProofs[providerPostQuantumModernKey][member] = capability.postQuantumModernValidUntil.UnixNano()
		}
	}
	var returnErr error
	server.Redis(ctx, func(r server.RedisClient) {
		for key, members := range keyMembers {
			_, err := r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Del(ctx, key)
				pipe.SAdd(ctx, key, members...)
				pipe.Expire(ctx, key, ttl)
				if proofs, ok := keyProofs[key]; ok {
					proofKey := providerPostQuantumProofKey(key)
					pipe.Del(ctx, proofKey)
					pipe.HSet(ctx, proofKey, proofs)
					pipe.Expire(ctx, proofKey, ttl)
				}
				return nil
			})
			if err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("publish %s: %w", key, err))
			}
		}
	})
	return returnErr
}

// Batches set membership and proof deadlines in one same-slot transaction.
// Old exports keep their set format; absence, malformed or expired proof
// refuses strict membership without a per-provider database read.
func readProviderPostQuantumSet(ctx context.Context, key string, clientIds []server.Id, now time.Time) (map[server.Id]bool, error) {
	members := map[server.Id]bool{}
	if len(clientIds) == 0 {
		return members, nil
	}
	fields := make([]string, 1, len(clientIds)+1)
	args := make([]any, 1, len(clientIds)+1)
	fields[0], args[0] = providerCapabilitySetReadyMember, providerCapabilitySetReadyMember
	for _, clientId := range clientIds {
		member := clientId.String()
		fields = append(fields, member)
		args = append(args, member)
	}
	var returnErr error
	server.Redis(ctx, func(r server.RedisClient) {
		var membership *redis.BoolSliceCmd
		var proof *redis.SliceCmd
		_, returnErr = r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			membership = pipe.SMIsMember(ctx, key, args...)
			proof = pipe.HMGet(ctx, providerPostQuantumProofKey(key), fields...)
			return nil
		})
		if returnErr != nil {
			return
		}
		present, deadlines := membership.Val(), proof.Val()
		if len(present) != len(fields) || len(deadlines) != len(fields) {
			returnErr = fmt.Errorf("incomplete %s proof response", key)
			return
		}
		if !present[0] || deadlines[0] != "1" {
			return
		}
		for i, clientId := range clientIds {
			value, ok := deadlines[i+1].(string)
			if !present[i+1] || !ok {
				continue
			}
			deadline, err := strconv.ParseInt(value, 10, 64)
			if err == nil && now.Before(time.Unix(0, deadline)) {
				members[clientId] = true
			}
		}
	})
	return members, returnErr
}

// Which of clientIds a published set holds; ready false when the set has no
// ready member (no export of this schema, or expired), and then every id is
// absent.
func readProviderCapabilitySet(ctx context.Context, key string, clientIds []server.Id) (members map[server.Id]bool, ready bool, returnErr error) {
	members = map[server.Id]bool{}
	if len(clientIds) == 0 {
		return
	}
	args := make([]any, 0, len(clientIds)+1)
	args = append(args, providerCapabilitySetReadyMember)
	for _, clientId := range clientIds {
		args = append(args, clientId.String())
	}
	server.Redis(ctx, func(r server.RedisClient) {
		memberships, err := r.SMIsMember(ctx, key, args...).Result()
		if err != nil {
			returnErr = err
			return
		}
		if len(memberships) != len(args) {
			returnErr = fmt.Errorf("incomplete %s membership response", key)
			return
		}
		ready = memberships[0]
		if !ready {
			return
		}
		for i, clientId := range clientIds {
			if memberships[i+1] {
				members[clientId] = true
			}
		}
	})
	return
}

// The labels of named providers (§6.4): membership of the dualstack set gives
// dualstack, of the tcp set tcp; absence, or a set no export has published,
// gives ipv4 and dualproto.
func readNamedProviderCapabilities(ctx context.Context, clientIds []server.Id) (map[server.Id]*FindProvidersCapabilities, error) {
	dualstackClientIds, _, err := readProviderCapabilitySet(ctx, providerCapabilityDualstackKey, clientIds)
	if err != nil {
		return nil, err
	}
	tcpClientIds, _, err := readProviderCapabilitySet(ctx, providerCapabilityTcpKey, clientIds)
	if err != nil {
		return nil, err
	}
	clientIdCapabilities := map[server.Id]*FindProvidersCapabilities{}
	for _, clientId := range clientIds {
		capabilities := &FindProvidersCapabilities{
			Ip:       ProviderCapabilityIpIpv4,
			Protocol: ProviderCapabilityProtocolDualproto,
		}
		if dualstackClientIds[clientId] {
			capabilities.Ip = ProviderCapabilityIpDualstack
		}
		if tcpClientIds[clientId] {
			capabilities.Protocol = ProviderCapabilityProtocolTcp
		}
		clientIdCapabilities[clientId] = capabilities
	}
	return clientIdCapabilities, nil
}

// Whether client_score_post_quantum_v1_ready is set, read through a
// per-process cache no older than the request settings' max age, so a
// strict post-quantum request costs no read of its own. Before the marker,
// strict post-quantum requests read the probe's modern evidence of the last
// seven days, never a provider's declaration (§12.4).
type clientScorePostQuantumReadyCache struct {
	stateLock sync.Mutex
	ready     bool
	readTime  time.Time
	hasRead   bool
}

var clientScorePostQuantumReadyState = &clientScorePostQuantumReadyCache{}

// A reset environment reads the marker again.
func init() {
	server.OnReset(func() {
		clientScorePostQuantumReadyState.reset()
	})
}

// Forgets the cached marker.
func (self *clientScorePostQuantumReadyCache) reset() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.ready = false
	self.readTime = time.Time{}
	self.hasRead = false
}

// The marker as of at most maxAge ago. A failed read keeps the previous
// value; with none, it is an error the request returns.
func (self *clientScorePostQuantumReadyCache) get(ctx context.Context, maxAge time.Duration) (bool, error) {
	now := time.Now()
	cached, ready := func() (bool, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.hasRead && now.Sub(self.readTime) < maxAge, self.ready
	}()
	if cached {
		return ready, nil
	}
	ready, err := GetClientScorePostQuantumReady(ctx)
	if err != nil {
		stale, staleReady := func() (bool, bool) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			return self.hasRead, self.ready
		}()
		if stale {
			return staleReady, nil
		}
		return false, err
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.ready = ready
		self.readTime = now
		self.hasRead = true
	}()
	return ready, nil
}

// Whether the post-quantum marker is set: the URL probe has measured
// post-quantum outcomes for a whole evidence window (FP2-PROTOCOL.md §10).
func GetClientScorePostQuantumReady(ctx context.Context) (ready bool, returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.Get(ctx, ClientScorePostQuantumReadyKey).Result()
		if errors.Is(err, redis.Nil) {
			return
		}
		if err != nil {
			returnErr = err
			return
		}
		ready = value != ""
	})
	return
}
