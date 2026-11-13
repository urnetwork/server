// FindProviders2 in selector mode (FP2-PROTOCOL.md §5, §6): a request that
// names the capabilities its window wants (FindProviders2Args.Select) is
// filled from the classified buckets in the decided order (D7.2), as D11.1
// and D11.2 refine it:
//
//  1. the window's IPv6 need, from dualstack providers: A, then C;
//  2. the requested protocol's classes as one pool: A and B for dualproto, C
//     and D for tcp;
//  3. only for a dualproto request that is not enough, the tcp classes C and
//     D as overflow, each at the overflow band's tier.
//
// Every answered provider carries its labels. A request without select never
// comes here: it is answered by today's code, the hard gate exactly (D7.1).
//
// Each phase fills like a legacy request, within its pool: the requested
// mode's natives, then the other mode's natives at the backfill offset,
// then the Online union at twice the offset; an Online request draws the
// pool's Online members uniformly, and force_minimum the pool's forced
// records. A phase never redraws a provider an earlier phase or a name
// answered. The location fallback climbs only while every pool the level
// read was published without an admitted provider.
//
// This is the legacy body of FindProviders2 per pool. Legacy mode is left
// exactly as it was, at the cost of the duplication; a fix to one is checked
// against the other.
package model

import (
	"context"
	"errors"
	"fmt"
	"maps"
	mathrand "math/rand"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/stats"
)

// The band of an overflow provider (§6.6): its tier carries this many
// backfill offsets, after the native (0), borrowed (1) and Online (2) bands,
// so with any demerits a client adds every main-pool provider still ranks
// ahead of every overflow one.
const findProviders2OverflowBand = 3

// The selectors of one selector-mode request, every default filled (§5.1).
type findProviders2Selection struct {
	ip                   string
	protocol             string
	targetCount          int
	heldCount            int
	dualstackTargetCount int
	heldDualstackCount   int
}

// Validates a request's selectors before any spec is read, and fills their
// defaults (§5.3). Each refusal is a 400 in the "<code> message" convention.
func newFindProviders2Selection(args *FindProviders2Args) (*findProviders2Selection, error) {
	selection := &findProviders2Selection{
		ip:       ProviderCapabilityIpDualstack,
		protocol: ProviderCapabilityProtocolDualproto,
	}
	if ip := args.Select.Ip; ip != nil {
		switch *ip {
		case ProviderCapabilityIpDualstack, ProviderCapabilityIpIpv4:
			selection.ip = *ip
		default:
			return nil, fmt.Errorf("400 unknown select.ip %q", *ip)
		}
	}
	if protocol := args.Select.Protocol; protocol != nil {
		switch *protocol {
		case ProviderCapabilityProtocolDualproto, ProviderCapabilityProtocolTcp:
			selection.protocol = *protocol
		default:
			return nil, fmt.Errorf("400 unknown select.protocol %q", *protocol)
		}
	}
	selection.targetCount = max(args.Count, 1)
	if targetCount := args.Select.TargetCount; targetCount != nil {
		if *targetCount < 1 {
			return nil, fmt.Errorf("400 select.target_count must be at least 1")
		}
		selection.targetCount = *targetCount
	}
	if heldCount := args.Select.HeldCount; heldCount != nil {
		if *heldCount < 0 {
			return nil, fmt.Errorf("400 select.held_count must not be negative")
		}
		selection.heldCount = *heldCount
	}
	selection.dualstackTargetCount = 1
	if dualstackTargetCount := args.Select.DualstackTargetCount; dualstackTargetCount != nil {
		if *dualstackTargetCount < 0 {
			return nil, fmt.Errorf("400 select.dualstack_target_count must not be negative")
		}
		selection.dualstackTargetCount = *dualstackTargetCount
	}
	if heldDualstackCount := args.Select.HeldDualstackCount; heldDualstackCount != nil {
		if *heldDualstackCount < 0 {
			return nil, fmt.Errorf("400 select.held_dualstack_count must not be negative")
		}
		selection.heldDualstackCount = *heldDualstackCount
	}
	// an ipv4 request has no IPv6 minimum, whatever it sends
	if selection.ip == ProviderCapabilityIpIpv4 {
		selection.dualstackTargetCount = 0
	}
	return selection, nil
}

// The echo of the selectors applied (§5.2).
func (self *findProviders2Selection) echo() *FindProviders2Select {
	ip, protocol := self.ip, self.protocol
	targetCount, heldCount := self.targetCount, self.heldCount
	dualstackTargetCount, heldDualstackCount := self.dualstackTargetCount, self.heldDualstackCount
	return &FindProviders2Select{
		Ip:                   &ip,
		Protocol:             &protocol,
		TargetCount:          &targetCount,
		HeldCount:            &heldCount,
		DualstackTargetCount: &dualstackTargetCount,
		HeldDualstackCount:   &heldDualstackCount,
	}
}

// How many dualstack providers the window still wants (D11.1).
func (self *findProviders2Selection) ipv6Need() int {
	if self.ip != ProviderCapabilityIpDualstack {
		return 0
	}
	return max(0, self.dualstackTargetCount-self.heldDualstackCount)
}

// The classes the IPv6 need draws, in order (§6.3): A, then C for a dualproto
// request, where a C provider is overflow; C for a tcp request.
func (self *findProviders2Selection) needClasses() []providerCapabilityClass {
	if self.protocol == ProviderCapabilityProtocolTcp {
		return []providerCapabilityClass{providerCapabilityClassC}
	}
	return []providerCapabilityClass{providerCapabilityClassA, providerCapabilityClassC}
}

// The requested protocol's classes, one pool (§6.3).
func (self *findProviders2Selection) mainClasses() providerCapabilityClassSet {
	if self.protocol == ProviderCapabilityProtocolTcp {
		return newProviderCapabilityClassSet(providerCapabilityClassC, providerCapabilityClassD)
	}
	return newProviderCapabilityClassSet(providerCapabilityClassA, providerCapabilityClassB)
}

// The overflow pool (§6.3): the tcp classes for a dualproto request, none for
// a tcp request, which never receives a dualproto provider.
func (self *findProviders2Selection) overflowClasses() providerCapabilityClassSet {
	if self.protocol == ProviderCapabilityProtocolTcp {
		return 0
	}
	return newProviderCapabilityClassSet(providerCapabilityClassC, providerCapabilityClassD)
}

// Whether a provider of the class is overflow for the request: a tcp
// provider given to a dualproto request.
func (self *findProviders2Selection) overflow(class providerCapabilityClass) bool {
	return self.protocol == ProviderCapabilityProtocolDualproto && class.protocol() == ProviderCapabilityProtocolTcp
}

// The classes an ip_family filter keeps (§6.3), as a filter on IP labels.
// v6-only keeps none: the two-axis model assumes IPv4 (§3.6). An unknown
// filter is ipFamilyFacetsForFilter's 400.
func providerCapabilityClassesForIpFamily(ipFamily string) (providerCapabilityClassSet, error) {
	if _, err := ipFamilyFacetsForFilter(ipFamily); err != nil {
		return 0, err
	}
	switch connect.IpFamilyFilter(ipFamily) {
	case connect.IpFamilyFilterV6Capable, connect.IpFamilyFilterDualstack:
		return newProviderCapabilityClassSet(providerCapabilityClassA, providerCapabilityClassC), nil
	case connect.IpFamilyFilterV4Only:
		return newProviderCapabilityClassSet(providerCapabilityClassB, providerCapabilityClassD), nil
	case connect.IpFamilyFilterV6Only:
		return 0, nil
	default:
		return newProviderCapabilityClassSet(providerCapabilityClasses...), nil
	}
}

// The facets a pool reads (§6.4) and the filter each applies. Non-forced:
// A reads d and B reads 4, skipping a downgraded record there, whose
// classified copy is in its capability facet; B also reads i, C t and D x.
// Forced: d can hold every class after downgrades, and 4 can hold B or D.
// Read those original family facets, then keep each record by its class.
type findProviders2PoolFacets struct {
	familyFacets     []ipFamilyFacet
	capabilityFacets []ipFamilyFacet
	filters          map[ipFamilyFacet]func(*ClientScore) bool
}

// The facets and filters of a pool of classes, forced or not.
func newFindProviders2PoolFacets(classes providerCapabilityClassSet, forceMinimum bool) findProviders2PoolFacets {
	poolFacets := findProviders2PoolFacets{
		filters: map[ipFamilyFacet]func(*ClientScore) bool{},
	}
	inPool := func(score *ClientScore) bool {
		return classes.has(score.capabilityClass()) && score.IpFamily() != connect.IpFamilyV6Only
	}
	if forceMinimum {
		if classes != 0 {
			poolFacets.familyFacets = append(poolFacets.familyFacets, ipFamilyFacetDualstack)
		}
		if classes.has(providerCapabilityClassB) || classes.has(providerCapabilityClassD) {
			poolFacets.familyFacets = append(poolFacets.familyFacets, ipFamilyFacetV4Only)
		}
		for _, facet := range poolFacets.familyFacets {
			poolFacets.filters[facet] = inPool
		}
		return poolFacets
	}
	notDowngraded := func(score *ClientScore) bool {
		return score.CapabilityDowngrades == 0 && inPool(score)
	}
	if classes.has(providerCapabilityClassA) {
		poolFacets.familyFacets = append(poolFacets.familyFacets, ipFamilyFacetDualstack)
		poolFacets.filters[ipFamilyFacetDualstack] = notDowngraded
	}
	if classes.has(providerCapabilityClassB) {
		poolFacets.familyFacets = append(poolFacets.familyFacets, ipFamilyFacetV4Only)
		poolFacets.filters[ipFamilyFacetV4Only] = notDowngraded
	}
	for _, class := range classes.classes() {
		if facet, ok := clientScoreCapabilityFacetOf(class); ok {
			poolFacets.capabilityFacets = append(poolFacets.capabilityFacets, facet)
			// a capability facet holds the classified copies of its class
			poolFacets.filters[facet] = func(score *ClientScore) bool {
				return score.CapabilityDowngrades != 0 && score.capabilityClass() == class && inPool(score)
			}
		}
	}
	return poolFacets
}

// Every facet the pool's union pages are read from.
func (self findProviders2PoolFacets) unionFacets() []ipFamilyFacet {
	return append(slices.Clone(self.familyFacets), self.capabilityFacets...)
}

// The union read options of the pool: its filters, and no un-faceted
// fallback, whose pages hold every class.
func (self findProviders2PoolFacets) readOptions() clientScoreReadOptions {
	return clientScoreReadOptions{
		facetFilters:        self.filters,
		noUnfacetedFallback: true,
	}
}

// Loads a pool's union pages of one mode, every facet in one shuffled order
// (§6.3), from the non-forced or the forced pool.
func loadFindProviders2PoolUnion(
	forceMinimum bool,
	mode RankMode,
	read *findProviders2PoolRead,
	poolFacets findProviders2PoolFacets,
) (map[server.Id]*ClientScore, *clientScoreCursor, error) {
	return loadClientScoresWithOptions(forceMinimum, mode, read.ctx, read.locationIds, read.locationGroupIds, read.clientLocationId, read.n, poolFacets.unionFacets(), false, read.partition, poolFacets.readOptions(), read.observation)
}

// The targets, caller, allowance and partition one pool read shares.
type findProviders2PoolRead struct {
	ctx              context.Context
	locationIds      map[server.Id]bool
	locationGroupIds map[server.Id]bool
	clientLocationId server.Id
	n                int
	partition        clientScorePartitionMode
	observation      *findProviders2LoadObservation
}

// Loads a pool's native pages of one mode from both native snapshots, every
// facet in one shuffled order: today's family facets with their filters
// and the capability facets. available is false when neither snapshot has
// metadata for any target: a writer that predates the schema, whose union
// pages establish extent instead. A capability snapshot no export has
// published yet leaves its classes without natives (§5.4, the rolling
// deploy).
func loadFindProviders2PoolNative(
	mode RankMode,
	read *findProviders2PoolRead,
	poolFacets findProviders2PoolFacets,
) (scores map[server.Id]*ClientScore, cursor *clientScoreCursor, available bool, returnErr error) {
	ctx := read.ctx
	server.Redis(ctx, func(r server.RedisClient) {
		modernPages := map[string]clientScorePage{}
		otherPages := map[string]clientScorePage{}
		for _, source := range []struct {
			snapshot clientScoreNativeSnapshot
			facets   []ipFamilyFacet
		}{
			{snapshot: clientScoreNativeFamilySnapshot, facets: poolFacets.familyFacets},
			{snapshot: clientScoreNativeCapabilitySnapshot, facets: poolFacets.capabilityFacets},
		} {
			if len(source.facets) == 0 {
				continue
			}
			pages, err := clientScoreNativeTargetPages(ctx, r, source.snapshot, mode, read.locationIds, read.locationGroupIds, read.clientLocationId, source.facets)
			if pages.canceled {
				available = true
				returnErr = err
				return
			}
			if !pages.available {
				continue
			}
			available = true
			returnErr = errors.Join(returnErr, err)
			if read.partition != clientScorePartitionOff {
				for state, targetCount := range pages.targetStates {
					findProviders2ModernPartitionTargets.WithLabelValues("native", state).Add(float64(targetCount))
				}
			}
			for i, facet := range source.facets {
				filter := poolFacets.filters[facet]
				for key, page := range pages.modernByFacet[i] {
					page.filter = filter
					modernPages[key] = page
				}
				for key, page := range pages.otherByFacet[i] {
					page.filter = filter
					otherPages[key] = page
				}
			}
		}
		if !available {
			returnErr = nil
			return
		}
		var modernGroups, otherGroups []map[string]clientScorePage
		switch read.partition {
		case clientScorePartitionModernFirst:
			modernGroups, otherGroups = []map[string]clientScorePage{modernPages}, []map[string]clientScorePage{otherPages}
		case clientScorePartitionModernOnly:
			modernGroups = []map[string]clientScorePage{modernPages}
			if read.observation != nil {
				for _, page := range otherPages {
					read.observation.unreadNonModernRows += page.expectedCount
				}
			}
		default:
			pages := maps.Clone(modernPages)
			maps.Copy(pages, otherPages)
			otherGroups = []map[string]clientScorePage{pages}
		}
		cursor = newClientScorePhasedCursor(modernGroups, otherGroups, read.observation)
		cursor.nativeOnly = true
		cursor.sourceIncomplete = returnErr != nil
		var readErr error
		scores, readErr = cursor.readWithClient(ctx, r, read.n)
		returnErr = errors.Join(returnErr, readErr)
	})
	return
}

// A pool's Quality or Speed source, as loadPreferredClientScoresWithPartition
// reads today's: the native pages while the native reader is on, else, or
// when they are unavailable, the union pages, keeping validated native rows
// and the source marked incomplete.
func loadFindProviders2PoolPreferred(
	nativeReaderEnabled bool,
	forceMinimum bool,
	mode RankMode,
	read *findProviders2PoolRead,
	poolFacets findProviders2PoolFacets,
) (scores map[server.Id]*ClientScore, cursor *clientScoreCursor, returnErr error) {
	if nativeReaderEnabled && !forceMinimum {
		var available bool
		scores, cursor, available, returnErr = loadFindProviders2PoolNative(mode, read, poolFacets)
		if read.ctx.Err() != nil {
			returnErr = read.ctx.Err()
			return
		}
		if returnErr == nil && available {
			return
		}
		if returnErr != nil {
			nativeScores, nativeErr := scores, returnErr
			scores, cursor, returnErr = loadFindProviders2PoolUnion(false, mode, read, poolFacets)
			if scores == nil {
				scores = map[server.Id]*ClientScore{}
			}
			for clientId, score := range nativeScores {
				if scores[clientId] == nil {
					scores[clientId] = score
				}
			}
			if cursor != nil {
				cursor.sourceIncomplete = true
			}
			returnErr = errors.Join(nativeErr, returnErr)
			return
		}
	}
	return loadFindProviders2PoolUnion(forceMinimum, mode, read, poolFacets)
}

// Selector-mode requests by rank mode and selectors, and the providers they
// answered by overflow step and labels (§8.1).
var findProviders2SelectRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_findproviders2_select_requests_total",
	Help: "FindProviders2 requests in selector mode, per rank mode and requested ip and protocol (FP2-PROTOCOL.md §5)",
}, []string{"rank_mode", "ip", "protocol"})

var findProviders2SelectAnswered = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_findproviders2_select_answered_total",
	Help: "Providers FindProviders2 answered in selector mode, named and discovered, per rank mode, overflow step (0 or 1) and the provider's ip and protocol labels",
}, []string{"rank_mode", "overflow_step", "ip", "protocol"})

var findProviders2SelectOverflow = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_findproviders2_select_overflow_total",
	Help: "Selector-mode FindProviders2 requests given tcp overflow providers, per rank mode",
}, []string{"rank_mode"})

var findProviders2SelectIpv6Need = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_findproviders2_select_ipv6_need_total",
	Help: "Selector-mode FindProviders2 discovery requests with an IPv6 need, per rank mode and whether the answer met it (met, short)",
}, []string{"rank_mode", "outcome"})

// Requests that ask for post-quantum providers, per field (§12, §8.1),
// legacy and selector mode alike.
var findProviders2PostQuantumRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_findproviders2_post_quantum_requests_total",
	Help: "FindProviders2 requests that set post_quantum_encryption or require_modern_provider, per field",
}, []string{"field"})

// Every label set is exported from the first scrape.
func init() {
	for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed, RankModeOnline} {
		for _, ip := range []string{ProviderCapabilityIpDualstack, ProviderCapabilityIpIpv4} {
			for _, protocol := range []string{ProviderCapabilityProtocolDualproto, ProviderCapabilityProtocolTcp} {
				findProviders2SelectRequests.WithLabelValues(rankMode, ip, protocol)
				for _, overflowStep := range []string{"0", "1"} {
					findProviders2SelectAnswered.WithLabelValues(rankMode, overflowStep, ip, protocol)
				}
			}
		}
		findProviders2SelectOverflow.WithLabelValues(rankMode)
		for _, outcome := range []string{"met", "short"} {
			findProviders2SelectIpv6Need.WithLabelValues(rankMode, outcome)
		}
	}
	for _, field := range []string{"post_quantum_encryption", "require_modern_provider"} {
		findProviders2PostQuantumRequests.WithLabelValues(field)
	}
	prometheus.MustRegister(findProviders2SelectRequests, findProviders2SelectAnswered, findProviders2SelectOverflow, findProviders2SelectIpv6Need, findProviders2PostQuantumRequests)
}

// Counts a request's post-quantum fields.
func recordFindProviders2PostQuantumRequest(findProviders2 *FindProviders2Args) {
	if findProviders2.PostQuantumEncryption {
		findProviders2PostQuantumRequests.WithLabelValues("post_quantum_encryption").Inc()
	}
	if findProviders2.RequireModernProvider {
		findProviders2PostQuantumRequests.WithLabelValues("require_modern_provider").Inc()
	}
}

// Whether a strict post-quantum request reads the measured signal: the
// marker is set (§12.4). A request that asks for neither field reads
// nothing.
func findProviders2PostQuantumReady(findProviders2 *FindProviders2Args, session *session.ClientSession) (bool, error) {
	if !findProviders2.PostQuantumEncryption && !findProviders2.RequireModernProvider {
		return false, nil
	}
	return clientScorePostQuantumReadyState.get(session.Ctx, requestEgressIndexSettings().RequestSettingsMaxAge)
}

// The candidates a post_quantum_encryption request may answer (§12.4): once
// the marker is set, a candidate whose record is post-quantum positive, and
// a named provider in {provider_post_quantum_positive}; before it, or after
// a probe gap clears it, the providers whose modern evidence of the last
// seven days is post-quantum (readProviderPostQuantumEvidenceClientIds),
// named and discovered alike. Either way only a probe's positive counts,
// never a provider's own declaration (owner, 2026-11-13).
func readFindProviders2PostQuantumEncryption(session *session.ClientSession, postQuantumReady bool, scores map[server.Id]*ClientScore, clientIds []server.Id, now time.Time) (map[server.Id]bool, error) {
	if !postQuantumReady {
		return readProviderPostQuantumEvidenceClientIds(session.Ctx, clientIds, now)
	}
	capableClientIds := map[server.Id]bool{}
	namedClientIds := []server.Id{}
	for _, clientId := range clientIds {
		if score := scores[clientId]; score != nil {
			if score.postQuantumCurrent(now) {
				capableClientIds[clientId] = true
			}
		} else {
			namedClientIds = append(namedClientIds, clientId)
		}
	}
	members, err := readProviderPostQuantumSet(session.Ctx, providerPostQuantumPositiveKey, namedClientIds, now)
	if err != nil {
		return nil, err
	}
	maps.Copy(capableClientIds, members)
	return capableClientIds, nil
}

// The named providers a require_modern_provider request may answer: once
// the marker is set, members of {provider_modern} (§12.4); before it, those
// modern by their evidence of the last seven days
// (readProviderModernClientIds), network-only ones by the same evidence.
func readFindProviders2NamedModern(session *session.ClientSession, postQuantumReady bool, clientIds []server.Id, now time.Time) (map[server.Id]bool, error) {
	if !postQuantumReady {
		return readProviderModernClientIds(session.Ctx, clientIds, now)
	}
	return readProviderPostQuantumSet(session.Ctx, providerPostQuantumModernKey, clientIds, now)
}

// The answer's band of a discovered provider, for the answered-modern counts.
type findProviders2SelectAnswer struct {
	band     string
	modern   bool
	overflow bool
	need     bool
}

// FindProviders2 for a request with select (§6.2).
func findProviders2WithSelect(
	findProviders2 *FindProviders2Args,
	session *session.ClientSession,
) (*FindProviders2Result, error) {
	requestTime := server.NowUtc()
	observation := newFindProviders2SelectionObservation(findProviders2)
	observation.beginPrivateCapture(session.Ctx)
	defer observation.finish(session.Ctx)
	// the selectors and the family filter are refused before any spec is
	// read: a selector this server cannot interpret must not widen
	selection, err := newFindProviders2Selection(findProviders2)
	if err != nil {
		return nil, err
	}
	keepClasses, err := providerCapabilityClassesForIpFamily(findProviders2.IpFamily)
	if err != nil {
		return nil, err
	}
	providers := []*FindProvidersProvider{}
	rankMode := RankModeQuality
	if findProviders2.RankMode != "" {
		rankMode = findProviders2.RankMode
	}
	metricRankMode := findProviders2MetricRankMode(rankMode)
	findProviders2SelectRequests.WithLabelValues(metricRankMode, selection.ip, selection.protocol).Inc()
	callerCountryCode := ""
	postQuantumReady, err := findProviders2PostQuantumReady(findProviders2, session)
	if err != nil {
		return nil, err
	}

	locationIds := map[server.Id]bool{}
	locationGroupIds := map[server.Id]bool{}

	excludeFinalDestinations := sync.OnceValue(func() map[server.Id]uint8 {
		excludeFinalDestinations := map[server.Id]uint8{}
		for _, clientId := range findProviders2.ExcludeClientIds {
			excludeFinalDestinations[clientId] |= 1
		}
		for _, destination := range findProviders2.ExcludeDestinations {
			excludeFinalDestinations[destination[len(destination)-1]] |= 2
		}
		return excludeFinalDestinations
	})

	specClientIds := []server.Id{}
	for _, spec := range findProviders2.Specs {
		if spec.LocationId != nil {
			locationIds[*spec.LocationId] = true
		}
		if spec.LocationGroupId != nil {
			locationGroupIds[*spec.LocationGroupId] = true
		}
		if spec.ClientId != nil {
			clientId := *(spec.ClientId)
			if excludeFinalDestinations()[clientId] == 0 {
				specClientIds = append(specClientIds, clientId)
			}
		}
		if spec.BestAvailable {
			homeLocationId, ok := countryCodeLocationIds()["us"]
			if ok {
				locationIds[homeLocationId] = true
			}
		}
	}

	// Named providers precede discovery results, pass the common gates and
	// carry the labels of the named-provider sets (§6.4).
	namedClientIds := map[server.Id]bool{}
	appendSpecProviders := func(hardExcludedClientIds map[server.Id]bool) error {
		answeredClientIds := []server.Id{}
		for _, clientId := range specClientIds {
			if hardExcludedClientIds[clientId] || namedClientIds[clientId] {
				continue
			}
			namedClientIds[clientId] = true
			answeredClientIds = append(answeredClientIds, clientId)
		}
		clientIdCapabilities, err := readNamedProviderCapabilities(session.Ctx, answeredClientIds)
		if err != nil {
			return err
		}
		for _, clientId := range answeredClientIds {
			providers = append(providers, &FindProvidersProvider{
				ClientId:     clientId,
				Capabilities: clientIdCapabilities[clientId],
			})
			observation.explicitReturned++
		}
		return nil
	}

	if 0 < len(locationIds) || 0 < len(locationGroupIds) {
		observation.discovery = true
		observation.privateGroupCount = len(locationGroupIds)
		requestSettings := requestEgressIndexSettings()
		partition := findProviders2PartitionMode(requestSettings, findProviders2.RequireModernProvider)
		modernFirst := partition != clientScorePartitionOff
		modernPriorityIncomplete := false
		var count int
		if findProviders2.ForceCount {
			count = findProviders2.Count
		} else {
			count = max(findProviders2.Count, 20)
		}
		observation.requestedCount = count
		loadCount := findProviders2LoadCount(count, len(excludeFinalDestinations()))
		backfillTierOffset := requestSettings.BackfillTierOffset
		// the pool draw: every candidate of a pool in one group (§6.3)
		poolFacets := []ipFamilyFacet{clientScorePoolFacet}

		observation.enter("caller_location")
		clientIp, _, err := session.ParseClientIpPort()
		if err != nil {
			return nil, err
		}
		ipInfo, err := server.GetIpInfo(clientIp)
		if err != nil {
			return nil, err
		}
		callerCountryCode = ipInfo.CountryCode

		countryLocations := countryCodeLocationIds()
		clientLocationId := countryLocations[ipInfo.CountryCode]
		var targetDirectory map[server.Id]*locationDirectoryEntry
		if snapshot := currentLocationDirectory.snapshot.Load(); snapshot != nil {
			targetDirectory = snapshot.entries
		}
		observation.targetKind = findProviders2TargetKind(findProviders2, countryLocations, targetDirectory)

		var callerNetworkId server.Id
		if session.ByJwt != nil {
			callerNetworkId = session.ByJwt.NetworkId
		}

		var fallback *findProviders2LocationFallback
		match := LocationType("")
		for {
			// what the level knows of each candidate, shared by its phases
			hardExcludedClientIds := map[server.Id]bool{}
			qualityExcludedClientIds := map[server.Id]bool{}
			offlineClientIds := map[server.Id]bool{}
			incapableClientIds := map[server.Id]bool{}
			exclusionReadClientIds := map[server.Id]bool{}
			qualityReadClientIds := map[server.Id]bool{}
			filterTime := requestTime
			evidence := findProviders2Evidence(requestSettings, findProviders2.ForceMinimum, filterTime)
			validation := &providerQualityValidation{parent: session.Ctx}
			defer validation.close()
			// the discovered providers of the level and how each was answered
			clientIdAnswers := map[server.Id]*findProviders2SelectAnswer{}
			loadDuration := time.Duration(0)
			var mainClientScores map[server.Id]*ClientScore
			directory := locationDirectory()
			observation.targetKind = findProviders2TargetKind(findProviders2, countryLocations, directory)
			observation.targetCountry = findProviders2TargetCountry(findProviders2, countryLocations, directory)

			// the current phase's pools, which pruneSelected revokes from
			var phaseClientScores, phaseNativeClientScores map[server.Id]*ClientScore
			pruneSelected := func() {
				offlineSelections := map[server.Id]bool{}
				rejected := func(clientId server.Id) bool {
					if offlineClientIds[clientId] {
						return !namedClientIds[clientId]
					}
					return hardExcludedClientIds[clientId] || incapableClientIds[clientId]
				}
				for clientId := range phaseClientScores {
					if rejected(clientId) {
						delete(phaseClientScores, clientId)
						delete(phaseNativeClientScores, clientId)
					}
				}
				providers = slices.DeleteFunc(providers, func(provider *FindProvidersProvider) bool {
					if offlineClientIds[provider.ClientId] && !namedClientIds[provider.ClientId] {
						offlineSelections[provider.ClientId] = true
					}
					reject := hardExcludedClientIds[provider.ClientId] || offlineSelections[provider.ClientId] || incapableClientIds[provider.ClientId]
					if reject {
						delete(clientIdAnswers, provider.ClientId)
						if namedClientIds[provider.ClientId] {
							observation.explicitReturned--
						}
					}
					return reject
				})
			}
			readQuality := func(ids []server.Id, strict bool) error {
				if len(ids) == 0 {
					return nil
				}
				observation.enter("hard_exclusions")
				excluded, risky, offline, err := validation.read(ids)
				if err != nil {
					if session.Ctx.Err() != nil || strict {
						return err
					}
					observation.backfillUnavailable = true
					return nil
				}
				maps.Copy(qualityExcludedClientIds, excluded)
				maps.Copy(qualityExcludedClientIds, offline)
				maps.Copy(offlineClientIds, offline)
				maps.Copy(hardExcludedClientIds, risky)
				for _, id := range ids {
					qualityReadClientIds[id] = true
				}
				pruneSelected()
				return nil
			}

			// Common refusals apply to every bucket of every phase; subscriber
			// refusals only to native Quality, as in legacy mode.
			readExclusions := func(requestFilter clientScoreRequestFilter, scores map[server.Id]*ClientScore, mode RankMode, native bool, namedIds []server.Id) error {
				observation.enter("hard_exclusions")
				candidateClientIds := requestFilter.unchecked(scores, exclusionReadClientIds)
				for _, clientId := range namedIds {
					if !exclusionReadClientIds[clientId] {
						candidateClientIds = append(candidateClientIds, clientId)
					}
				}
				if findProviders2.PostQuantumEncryption {
					capableClientIds, err := readFindProviders2PostQuantumEncryption(session, postQuantumReady, scores, candidateClientIds, requestTime)
					if err != nil {
						return err
					}
					candidateClientIds = slices.DeleteFunc(candidateClientIds, func(clientId server.Id) bool {
						if capableClientIds[clientId] {
							return false
						}
						incapableClientIds[clientId] = true
						exclusionReadClientIds[clientId] = true
						return true
					})
				}
				excluded, err := getProviderHardExclusions(session.Ctx, candidateClientIds)
				if err != nil {
					return err
				}
				maps.Copy(hardExcludedClientIds, excluded)
				namedClientIdSet := map[server.Id]bool{}
				for _, clientId := range namedIds {
					namedClientIdSet[clientId] = true
				}
				leaseClientIds := make([]server.Id, 0, len(candidateClientIds))
				for _, clientId := range candidateClientIds {
					if !hardExcludedClientIds[clientId] && !namedClientIdSet[clientId] {
						leaseClientIds = append(leaseClientIds, clientId)
					}
				}
				offline, err := readFindProviders2Liveness(session.Ctx, leaseClientIds, server.NowUtc(), rankMode)
				if err != nil {
					return err
				}
				maps.Copy(offlineClientIds, offline)
				for _, clientId := range candidateClientIds {
					exclusionReadClientIds[clientId] = true
				}
				qualityClientIds := []server.Id{}
				if rankMode == RankModeQuality {
					for _, id := range namedIds {
						if !hardExcludedClientIds[id] && !incapableClientIds[id] && !qualityReadClientIds[id] {
							qualityClientIds = append(qualityClientIds, id)
						}
					}
				}
				if err := readQuality(qualityClientIds, true); err != nil {
					return err
				}
				// a later Quality page may expose common risk for an earlier
				// selection of any phase
				if mode == RankModeQuality && native {
					prior := []server.Id{}
					for _, provider := range providers {
						id := provider.ClientId
						if score := scores[id]; score != nil && !qualityReadClientIds[id] &&
							!hardExcludedClientIds[id] && score.PassesMinimums[mode] &&
							score.urlEvidenceCurrent(filterTime) {
							prior = append(prior, id)
						}
					}
					if err := readQuality(prior, findProviders2.ForceMinimum); err != nil {
						return err
					}
				}
				pruneSelected()
				return nil
			}

			filterPool := func(requestFilter clientScoreRequestFilter, clientScores map[server.Id]*ClientScore, mode RankMode, hardExcludedClientIds map[server.Id]bool) {
				observation.enter("filter")
				now := filterTime
				for _, clientScore := range clientScores {
					if !clientScore.urlEvidenceCurrent(now) {
						if clientScore.PassesMinimums[RankModeQuality] || clientScore.PassesMinimums[RankModeSpeed] {
							clientScore.Online = true
						}
						clientScore.PassesMinimums = nil
						clientScore.UrlProbeSuccessWeight = 1
					}
				}
				if mode == RankModeOnline {
					for clientId, clientScore := range clientScores {
						if clientScore.PassesMinimums[RankModeQuality] || clientScore.PassesMinimums[RankModeSpeed] {
							clientScore.Online = true
						}
						if !clientScore.Online {
							delete(clientScores, clientId)
							continue
						}
						scaledWeights := maps.Clone(clientScore.ScaledWeights)
						if scaledWeights == nil {
							scaledWeights = map[string]float32{}
						}
						scaledWeights[mode] = 1.0
						clientScore.ScaledWeights = scaledWeights
					}
				}
				before := len(clientScores)
				for clientId := range hardExcludedClientIds {
					delete(clientScores, clientId)
				}
				for clientId := range offlineClientIds {
					delete(clientScores, clientId)
				}
				capabilityDropped := 0
				for clientId := range incapableClientIds {
					if _, ok := clientScores[clientId]; ok {
						delete(clientScores, clientId)
						capabilityDropped += 1
					}
				}
				for clientId := range qualityExcludedClientIds {
					if clientScore := clientScores[clientId]; clientScore != nil {
						if findProviders2.ForceMinimum && rankMode == RankModeQuality {
							delete(clientScores, clientId)
						} else if clientScore.PassesMinimums[RankModeQuality] {
							clientScore.Online = true
							clientScore.PassesMinimums = maps.Clone(clientScore.PassesMinimums)
							delete(clientScore.PassesMinimums, RankModeQuality)
						}
					}
				}
				// never redraws a provider an earlier phase or a name answered
				for _, provider := range providers {
					delete(clientScores, provider.ClientId)
				}
				observation.dropped[0] += before - len(clientScores) - capabilityDropped
				observation.dropped[4] += capabilityDropped
				requestFilter.apply(clientScores, observation)
				observation.eligible += len(clientScores)
				if findProviders2.ForceMinimum {
					for _, clientScore := range clientScores {
						clientScore.ScaledWeights = maps.Clone(clientScore.ScaledWeights)
						clientScore.ScaledWeights[mode] = 1.0
					}
				}
				intermediaryScale := float32(0.5)
				for _, destination := range findProviders2.ExcludeDestinations {
					for _, clientId := range destination[:len(destination)-1] {
						if clientScore, ok := clientScores[clientId]; ok {
							clientScore.ScaledWeights = maps.Clone(clientScore.ScaledWeights)
							clientScore.ScaledWeights[mode] *= intermediaryScale
						}
					}
				}
			}

			selectProviders := func(candidateClientScores map[server.Id]*ClientScore, mode RankMode, n int) []server.Id {
				return selectClientScoresModernFirst(candidateClientScores, n, modernFirst, func(candidateClientScores map[server.Id]*ClientScore, n int) []server.Id {
					return selectClientScoresByWeight(candidateClientScores, mode, poolFacets, n, evidence)
				})
			}
			selectOnline := func(candidateClientScores map[server.Id]*ClientScore, n int) []server.Id {
				return selectClientScoresModernFirst(candidateClientScores, n, modernFirst, func(candidateClientScores map[server.Id]*ClientScore, n int) []server.Id {
					return selectOnlineClientScores(candidateClientScores, poolFacets, n, evidence, mathrand.Float64)
				})
			}
			selectOnlineUniformly := func(candidateClientScores map[server.Id]*ClientScore, n int) []server.Id {
				return selectClientScoresModernFirst(candidateClientScores, n, modernFirst, func(candidateClientScores map[server.Id]*ClientScore, n int) []server.Id {
					clientIds := slices.Collect(maps.Keys(candidateClientScores))
					mathrand.Shuffle(len(clientIds), func(i int, j int) {
						clientIds[i], clientIds[j] = clientIds[j], clientIds[i]
					})
					n = max(0, min(n, len(clientIds)))
					connect.WeightedSelectFunc(clientIds, n, func(clientId server.Id) float32 {
						return candidateClientScores[clientId].ScaledWeights[RankModeOnline]
					})
					return clientIds[:n]
				})
			}

			validateQualityPool := func(quota int, scores, priorScores map[server.Id]*ClientScore, mode RankMode, native bool) error {
				if mode != RankModeQuality || (!native && !findProviders2.ForceMinimum) {
					return nil
				}
				modernPasses := []bool{false}
				if modernFirst {
					modernPasses = []bool{true, false}
				}
				for _, modernPass := range modernPasses {
					for {
						pending := map[server.Id]*ClientScore{}
						validated := 0
						for id, score := range scores {
							if hardExcludedClientIds[id] || qualityExcludedClientIds[id] {
								if hardExcludedClientIds[id] || offlineClientIds[id] || findProviders2.ForceMinimum {
									delete(scores, id)
								} else {
									score.Online = true
									score.PassesMinimums = maps.Clone(score.PassesMinimums)
									delete(score.PassesMinimums, mode)
								}
								continue
							}
							if priorScores[id] != nil || (!findProviders2.ForceMinimum && !score.PassesMinimums[mode]) {
								continue
							}
							if modernPass && !score.Modern {
								continue
							}
							if qualityReadClientIds[id] {
								validated++
							} else {
								pending[id] = score
							}
						}
						needed := quota - len(priorScores) - validated
						if needed <= 0 || len(pending) == 0 || validation.unavailable != nil {
							break
						}
						ids := selectProviders(pending, mode, providerQualityValidationDraw(needed, findProviders2.ForceMinimum))
						if err := readQuality(ids, findProviders2.ForceMinimum); err != nil {
							return err
						}
					}
				}
				return nil
			}

			refillPool := func(requestFilter clientScoreRequestFilter, quota int, scores, priorScores map[server.Id]*ClientScore, mode RankMode, cursor *clientScoreCursor, knownHardExclusions map[server.Id]bool, backfill, native bool) error {
				if quota <= 0 {
					return nil
				}
				if err := validateQualityPool(quota, scores, priorScores, mode, native); err != nil {
					return err
				}
				uniqueCount := func(modernOnly bool) int {
					uniqueCount := 0
					for clientId, score := range scores {
						if mode == RankModeQuality && (native || findProviders2.ForceMinimum) && !qualityReadClientIds[clientId] {
							continue
						}
						if !findProviders2.ForceMinimum && ((native && !score.PassesMinimums[mode]) || (!native && !score.Online)) {
							continue
						}
						if modernOnly && !score.Modern {
							continue
						}
						if priorScores[clientId] == nil {
							uniqueCount++
						}
					}
					return uniqueCount
				}
				unknown := cursor == nil || cursor.sourceIncomplete || 0 < cursor.missingPages
				for cursor.hasMore() {
					if mode == RankModeQuality && native && validation.unavailable != nil {
						unknown = true
						break
					}
					modernPhase := modernFirst && cursor.inModernPhase()
					if quota <= len(priorScores)+uniqueCount(modernPhase) {
						break
					}
					var additionalRows int
					if native && cursor.nativeOnly {
						additionalRows = loadCount
					} else {
						phaseReadCount := cursor.readCount
						if modernFirst {
							if modernPhase {
								phaseReadCount = cursor.modernReadCount
							} else {
								phaseReadCount = cursor.readCount - cursor.modernReadCount
							}
						}
						discardedRows := max(0, phaseReadCount-uniqueCount(modernPhase))
						limit := findProviders2RefillLoadCount(quota, len(excludeFinalDestinations()), discardedRows)
						additionalRows = limit - phaseReadCount
					}
					if additionalRows <= 0 {
						if modernPhase && cursor.hasOtherPhase() {
							cursor.skipModernPhase()
							modernPriorityIncomplete = true
							continue
						}
						if native {
							unknown = true
						}
						break
					}
					if backfill {
						observation.enter("load_backfill")
					} else {
						observation.enter("load_primary")
					}
					extraScores, err := cursor.read(session.Ctx, additionalRows)
					if err != nil {
						if session.Ctx.Err() != nil || (!native && !backfill) {
							return err
						}
						unknown = true
					}
					readFailed := err != nil
					unknown = unknown || cursor.sourceIncomplete || 0 < cursor.missingPages
					observation.loaded += len(extraScores)
					if err := readExclusions(requestFilter, extraScores, mode, native, nil); err != nil {
						return err
					}
					filterPool(requestFilter, extraScores, mode, knownHardExclusions)
					if native && cursor.nativeOnly {
						retainNativeClientScores(extraScores, mode)
					}
					for clientId, clientScore := range extraScores {
						if scores[clientId] == nil {
							scores[clientId] = clientScore
						}
					}
					if err := validateQualityPool(quota, scores, priorScores, mode, native); err != nil {
						return err
					}
					if readFailed {
						break
					}
				}
				unknown = unknown || (mode == RankModeQuality && native && validation.unavailable != nil)
				if unknown {
					observation.backfillUnavailable = true
				}
				if native {
					outcome := "exhausted"
					if unknown {
						outcome = "unavailable"
					} else if quota <= len(priorScores)+uniqueCount(false) {
						outcome = "quota"
					}
					observation.nativeSource(mode, backfill, outcome)
				}
				return nil
			}

			// The named providers are read with the level's first pool, as
			// legacy mode reads them with its primary pool.
			namedPending := 0 < len(specClientIds)
			readNamed := func(requestFilter clientScoreRequestFilter, clientScores map[server.Id]*ClientScore) error {
				if !namedPending {
					return readExclusions(requestFilter, clientScores, rankMode, true, nil)
				}
				namedPending = false
				if findProviders2.RequireModernProvider {
					observation.enter("hard_exclusions")
					modernClientIds, err := readFindProviders2NamedModern(session, postQuantumReady, specClientIds, filterTime)
					if err != nil {
						return err
					}
					for _, clientId := range specClientIds {
						if !modernClientIds[clientId] {
							incapableClientIds[clientId] = true
							exclusionReadClientIds[clientId] = true
						}
					}
				}
				if err := readExclusions(requestFilter, clientScores, rankMode, true, specClientIds); err != nil {
					return err
				}
				namedExcludedClientIds := maps.Clone(hardExcludedClientIds)
				if rankMode == RankModeQuality {
					maps.Copy(namedExcludedClientIds, qualityExcludedClientIds)
				}
				maps.Copy(namedExcludedClientIds, incapableClientIds)
				return appendSpecProviders(namedExcludedClientIds)
			}

			// Fills up to n providers from the pool of classes (§6.5), each
			// at its band's tier, plus the overflow band's for an overflow
			// pool. Returns how many it answered.
			fill := func(classes providerCapabilityClassSet, n int, overflow bool, need bool) (int, error) {
				classes = classes.intersect(keepClasses)
				if classes == 0 || n <= 0 {
					return 0, nil
				}
				requestFilter := clientScoreRequestFilter{
					callerNetworkId:            callerNetworkId,
					excludedClientIds:          excludeFinalDestinations(),
					requireModern:              findProviders2.RequireModernProvider,
					capabilityClasses:          classes,
					requirePostQuantumPositive: postQuantumReady,
					now:                        requestTime,
				}
				read := &findProviders2PoolRead{
					ctx:              session.Ctx,
					locationIds:      locationIds,
					locationGroupIds: locationGroupIds,
					clientLocationId: clientLocationId,
					n:                loadCount,
					partition:        partition,
					observation:      &observation.load,
				}
				readFacets := newFindProviders2PoolFacets(classes, findProviders2.ForceMinimum)
				answered := 0
				// the tier of a provider in its band, plus the overflow band's
				answer := func(clientScore *ClientScore, mode RankMode, tier int, band string) {
					provider := findProvidersProviderFromClientScore(clientScore, mode, directory, match)
					provider.Tier = tier
					ip, protocol := clientScore.CapabilityLabels()
					provider.Capabilities = &FindProvidersCapabilities{Ip: ip, Protocol: protocol}
					if overflow {
						provider.Tier += findProviders2OverflowBand * backfillTierOffset
						provider.OverflowStep = 1
					}
					providers = append(providers, provider)
					clientIdAnswers[clientScore.ClientId] = &findProviders2SelectAnswer{
						band:     band,
						modern:   clientScore.Modern,
						overflow: overflow,
						need:     need,
					}
					answered++
				}

				observation.enter("load_primary")
				observation.load.privateSource = "primary_legacy"
				loadStartTime := time.Now()
				var clientScores map[server.Id]*ClientScore
				var primaryCursor *clientScoreCursor
				var err error
				if rankMode == RankModeOnline {
					clientScores, primaryCursor, err = loadFindProviders2PoolUnion(false, onlineClientScoreSourceRankMode, read, newFindProviders2PoolFacets(classes, false))
				} else {
					clientScores, primaryCursor, err = loadFindProviders2PoolPreferred(requestSettings.NativeReaderEnabled, findProviders2.ForceMinimum, rankMode, read, readFacets)
				}
				if err != nil {
					if session.Ctx.Err() != nil || findProviders2.ForceMinimum || rankMode == RankModeOnline {
						return answered, err
					}
					observation.backfillUnavailable = true
					if primaryCursor != nil {
						primaryCursor.sourceIncomplete = true
					}
				}
				if clientScores == nil {
					clientScores = map[server.Id]*ClientScore{}
				}
				observation.loaded += len(clientScores)
				loadDuration += time.Since(loadStartTime)
				findProviders2LoadSeconds.Observe(time.Since(loadStartTime).Seconds())
				phaseClientScores, phaseNativeClientScores = clientScores, nil

				if err := readNamed(requestFilter, clientScores); err != nil {
					return answered, err
				}
				filterPool(requestFilter, clientScores, rankMode, hardExcludedClientIds)
				if primaryCursor != nil && primaryCursor.nativeOnly {
					retainNativeClientScores(clientScores, rankMode)
				}
				if err := refillPool(requestFilter, n, clientScores, nil, rankMode, primaryCursor, hardExcludedClientIds, false, !findProviders2.ForceMinimum && rankMode != RankModeOnline); err != nil {
					return answered, err
				}
				observation.enter("select")

				nativeClientScores := map[server.Id]*ClientScore{}
				for clientId, clientScore := range clientScores {
					if (findProviders2.ForceMinimum || clientScore.PassesMinimums[rankMode] || (rankMode == RankModeOnline && clientScore.Online)) &&
						(rankMode != RankModeQuality || qualityReadClientIds[clientId]) {
						nativeClientScores[clientId] = clientScore
					}
				}
				phaseNativeClientScores = nativeClientScores
				var clientIds []server.Id
				if rankMode == RankModeOnline {
					clientIds = selectOnlineUniformly(nativeClientScores, n)
				} else {
					clientIds = selectProviders(nativeClientScores, rankMode, n)
				}
				for _, clientId := range clientIds {
					clientScore := clientScores[clientId]
					answer(clientScore, rankMode, clientScore.Tiers[rankMode], "native")
				}
				if !need && !overflow {
					mainClientScores = clientScores
				}

				otherRankMode, ok := backfillRankMode(rankMode)
				if !ok || findProviders2.ForceMinimum || n <= answered {
					return answered, nil
				}
				remainingCount := func() int {
					return n - answered
				}
				observation.enter("load_backfill")
				observation.load.privateSource = "alternate_legacy"
				otherClientScores, otherCursor, err := loadFindProviders2PoolPreferred(requestSettings.NativeReaderEnabled, false, otherRankMode, read, readFacets)
				if err != nil {
					if session.Ctx.Err() != nil {
						return answered, err
					}
					observation.backfillUnavailable = true
					if otherCursor != nil {
						otherCursor.sourceIncomplete = true
					}
				}
				if otherClientScores == nil {
					otherClientScores = map[server.Id]*ClientScore{}
				}
				observation.loaded += len(otherClientScores)
				if err := readExclusions(requestFilter, otherClientScores, otherRankMode, true, nil); err != nil {
					return answered, err
				}
				filterPool(requestFilter, otherClientScores, otherRankMode, hardExcludedClientIds)
				if otherCursor != nil && otherCursor.nativeOnly {
					retainNativeClientScores(otherClientScores, otherRankMode)
				}
				if err := refillPool(requestFilter, n, otherClientScores, nativeClientScores, otherRankMode, otherCursor, hardExcludedClientIds, true, true); err != nil {
					return answered, err
				}
				observation.backfillClientScores = otherClientScores
				observation.enter("select")

				// the other bucket's natives, Quality borrowing only Speed
				// natives within its cutoffs
				borrowableClientScores := map[server.Id]*ClientScore{}
				pastCutoffClientScores := map[server.Id]*ClientScore{}
				for clientId, clientScore := range otherClientScores {
					if !clientScore.PassesMinimums[otherRankMode] ||
						(otherRankMode == RankModeQuality && !qualityReadClientIds[clientId]) {
						continue
					}
					if _, native := nativeClientScores[clientId]; native {
						continue
					}
					if clientIdAnswers[clientId] != nil {
						continue
					}
					if rankMode == RankModeQuality && ClientScoreCutoffTier <= clientScore.Tiers[RankModeQuality] {
						pastCutoffClientScores[clientId] = clientScore
						continue
					}
					borrowableClientScores[clientId] = clientScore
				}
				for _, clientId := range selectProviders(borrowableClientScores, otherRankMode, remainingCount()) {
					clientScore := otherClientScores[clientId]
					answer(clientScore, otherRankMode, clientScore.Tiers[otherRankMode]+backfillTierOffset, "borrowed")
				}
				if remainingCount() <= 0 {
					return answered, nil
				}

				// then the pool's Online union
				answeredClientScores := map[server.Id]*ClientScore{}
				for _, provider := range providers {
					answeredClientScores[provider.ClientId] = &ClientScore{ClientId: provider.ClientId}
				}
				onlineClientScores := map[server.Id]*ClientScore{}
				for _, source := range []struct {
					mode   RankMode
					scores map[server.Id]*ClientScore
					cursor *clientScoreCursor
				}{
					{mode: rankMode, scores: clientScores, cursor: primaryCursor},
					{mode: otherRankMode, scores: otherClientScores, cursor: otherCursor},
				} {
					pool, cursor := source.scores, source.cursor
					if cursor == nil || cursor.nativeOnly {
						observation.enter("load_backfill")
						observation.load.privateSource = "online_legacy"
						var err error
						pool, cursor, err = loadFindProviders2PoolUnion(false, source.mode, read, newFindProviders2PoolFacets(classes, false))
						if err != nil {
							if session.Ctx.Err() != nil {
								return answered, err
							}
							observation.backfillUnavailable = true
							if cursor != nil {
								cursor.sourceIncomplete = true
							}
						}
						if pool == nil {
							pool = map[server.Id]*ClientScore{}
						}
						observation.loaded += len(pool)
						if err := readExclusions(requestFilter, pool, source.mode, false, nil); err != nil {
							return answered, err
						}
						filterPool(requestFilter, pool, source.mode, hardExcludedClientIds)
					}
					priorScores := maps.Clone(answeredClientScores)
					maps.Copy(priorScores, onlineClientScores)
					if err := refillPool(requestFilter, n-answered+len(priorScores), pool, priorScores, source.mode, cursor, hardExcludedClientIds, true, false); err != nil {
						return answered, err
					}
					for clientId, clientScore := range pool {
						if clientScore.Online && answeredClientScores[clientId] == nil && onlineClientScores[clientId] == nil && !offlineClientIds[clientId] {
							onlineClientScores[clientId] = clientScore
						}
					}
				}
				for clientId, clientScore := range pastCutoffClientScores {
					if answeredClientScores[clientId] == nil && onlineClientScores[clientId] == nil && !offlineClientIds[clientId] {
						onlineClientScores[clientId] = clientScore
					}
				}
				for _, clientId := range selectOnline(onlineClientScores, remainingCount()) {
					clientScore := onlineClientScores[clientId]
					findProviders2OnlineFallbackEvidence.WithLabelValues(rankMode, clientScoreEvidence(clientScore, filterTime).String()).Inc()
					answer(clientScore, RankModeSpeed, 2*backfillTierOffset, "online")
				}
				return answered, nil
			}

			// the discovered providers the level holds now, as pruning may
			// revoke any of them
			discoveredCount := func(keep func(*findProviders2SelectAnswer, *FindProvidersProvider) bool) int {
				discovered := 0
				for _, provider := range providers {
					if answer := clientIdAnswers[provider.ClientId]; answer != nil && keep(answer, provider) {
						discovered++
					}
				}
				return discovered
			}
			allDiscovered := func(*findProviders2SelectAnswer, *FindProvidersProvider) bool { return true }

			// 1. the IPv6 need: A, then C (§6.3)
			ipv6Need := selection.ipv6Need()
			if 0 < count && 0 < ipv6Need {
				for _, class := range selection.needClasses() {
					needCount := ipv6Need - discoveredCount(func(answer *findProviders2SelectAnswer, provider *FindProvidersProvider) bool {
						return answer.need && provider.Capabilities != nil && provider.Capabilities.Ip == ProviderCapabilityIpDualstack
					})
					if needCount <= 0 {
						break
					}
					if _, err := fill(newProviderCapabilityClassSet(class), min(needCount, count-discoveredCount(allDiscovered)), selection.overflow(class), true); err != nil {
						return nil, err
					}
				}
			}
			// 2. the requested protocol's classes, as one pool
			if 0 < count {
				if _, err := fill(selection.mainClasses(), count-discoveredCount(allDiscovered), false, false); err != nil {
					return nil, err
				}
			}
			// 3. overflow, for a dualproto request the requested protocol
			// cannot fill: held clients count toward the target, and so do
			// the request's usable dualproto candidates (D7.3, D11.2)
			if overflowClasses := selection.overflowClasses(); 0 < count && overflowClasses != 0 {
				eligibleCount := discoveredCount(func(answer *findProviders2SelectAnswer, provider *FindProvidersProvider) bool {
					return !answer.overflow
				})
				if selection.heldCount+eligibleCount < selection.targetCount {
					if _, err := fill(overflowClasses, count-discoveredCount(allDiscovered), true, false); err != nil {
						return nil, err
					}
				}
			}
			// names with no pool to read them with: a family filter that
			// keeps no class, or a count of zero
			if namedPending {
				if err := readNamed(clientScoreRequestFilter{callerNetworkId: callerNetworkId, excludedClientIds: excludeFinalDestinations(), capabilityClasses: keepClasses}, map[server.Id]*ClientScore{}); err != nil {
					return nil, err
				}
			}
			observation.primaryClientScores = mainClientScores

			// climbs a level only while every pool the level read was
			// published without an admitted provider
			if !findProviders2.Strict && requestSettings.LocationFallback && 0 < count && observation.targetsWithoutSupply() {
				if fallback == nil {
					fallback = newFindProviders2LocationFallback(locationIds, directory)
				}
				if level, ok := fallback.nextLevel(); ok {
					locationIds, locationGroupIds, match = level.locationIds, map[server.Id]bool{}, level.match
					observation.fallbackLevel = match
					specClientIds = nil
					continue
				}
			}
			discovered := discoveredCount(allDiscovered)
			if fallback != nil {
				fallback.record(observation.targetKind, match, 0 < discovered)
			}

			// the answer's metrics, once for the attempt that answered
			borrowedCount := 0
			overflowCount := 0
			needMetCount := 0
			chosenClientIds := []server.Id{}
			for _, provider := range providers {
				answer := clientIdAnswers[provider.ClientId]
				if answer == nil {
					continue
				}
				chosenClientIds = append(chosenClientIds, provider.ClientId)
				if answer.band != "native" {
					borrowedCount++
				}
				if answer.overflow {
					overflowCount++
				}
				if answer.need && provider.Capabilities != nil && provider.Capabilities.Ip == ProviderCapabilityIpDualstack {
					needMetCount++
				}
				recordFindProviders2AnsweredModern(rankMode, answer.band, answer.modern)
			}
			if _, ok := backfillRankMode(rankMode); (ok || rankMode == RankModeOnline) && !findProviders2.ForceMinimum {
				findProviders2BackfillProviders.WithLabelValues(rankMode).Observe(float64(borrowedCount))
				findProviders2AnsweredProviders.WithLabelValues(rankMode).Add(float64(discovered))
			}
			if 0 < overflowCount {
				findProviders2SelectOverflow.WithLabelValues(metricRankMode).Inc()
			}
			if 0 < count && 0 < ipv6Need && keepClasses.intersect(newProviderCapabilityClassSet(providerCapabilityClassA, providerCapabilityClassC)) != 0 {
				outcome := "short"
				if ipv6Need <= needMetCount {
					outcome = "met"
				}
				findProviders2SelectIpv6Need.WithLabelValues(metricRankMode, outcome).Inc()
			}
			observation.discoveryReturned = discovered
			if modernPriorityIncomplete {
				findProviders2ModernPriorityIncomplete.WithLabelValues(metricRankMode).Inc()
			}
			if s := stats.Default(); s.Enabled() && mainClientScores != nil {
				recordFindProviders2Sample(
					s,
					findProviders2,
					rankMode,
					count,
					ipInfo.CountryCode,
					float64(loadDuration.Nanoseconds())/1e6,
					mainClientScores,
					chosenClientIds,
				)
			}
			if 50*time.Millisecond <= loadDuration && glog.V(1) {
				glog.Infof("[nclm]findproviders2 select load %.2fms (%d)\n", float64(loadDuration)/float64(time.Millisecond), observation.loaded)
			}
			break
		}
	} else {
		observation.enter("hard_exclusions")
		hardExcludedClientIds, err := getProviderRequestExclusions(session.Ctx, specClientIds, rankMode)
		if err != nil {
			return nil, err
		}
		if findProviders2.RequireModernProvider {
			modernClientIds, err := readFindProviders2NamedModern(session, postQuantumReady, specClientIds, requestTime)
			if err != nil {
				return nil, err
			}
			for _, clientId := range specClientIds {
				if !modernClientIds[clientId] {
					hardExcludedClientIds[clientId] = true
				}
			}
		}
		if findProviders2.PostQuantumEncryption {
			capableClientIds, err := readFindProviders2PostQuantumEncryption(session, postQuantumReady, nil, specClientIds, requestTime)
			if err != nil {
				return nil, err
			}
			for _, clientId := range specClientIds {
				if !capableClientIds[clientId] {
					hardExcludedClientIds[clientId] = true
				}
			}
		}
		if err := appendSpecProviders(hardExcludedClientIds); err != nil {
			return nil, err
		}
	}

	if 0 < len(providers) {
		observation.enter("record_matches")
		providerClientIds := make([]server.Id, 0, len(providers))
		for _, provider := range providers {
			providerClientIds = append(providerClientIds, provider.ClientId)
		}
		now := server.NowUtc()
		RecordProviderSearchMatches(session.Ctx, providerClientIds, now)
		recordProviderAppearances(session.Ctx, providerClientIds, now)
	}
	for _, provider := range providers {
		overflowStep := strconv.Itoa(provider.OverflowStep)
		if provider.Capabilities != nil {
			findProviders2SelectAnswered.WithLabelValues(metricRankMode, overflowStep, provider.Capabilities.Ip, provider.Capabilities.Protocol).Inc()
		}
	}

	result := &FindProviders2Result{
		Providers: providers,
		Select:    selection.echo(),
	}
	recordFindProviders2Outcome(findProviders2, callerCountryCode, len(result.Providers))
	observation.complete(len(result.Providers))
	if len(result.Providers) == 0 {
		recordFindProviders2EmptyOutcome(findProviders2, callerCountryCode, observation.answeredTargetKind(), observation.targetCountry, observation.reason)
	}
	return result, nil
}
