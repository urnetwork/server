// The country floor of the score export (owner decision 2026-11-13: "keep the
// 80% URL-probe bar, but never let a country go empty. Always admit each
// country's best few providers even below the bar"; FP2FIX.md, "The country
// floor").
//
// The URL bar stays every provider's decision: the 4/5 success ratio of
// Quality and Speed, the Online URL ratio and the rule of one success
// (egress_index.go, egress_online_success.go). Once the export has decided and
// ranked every provider, the floor looks at each country target. Where fewer
// than country_floor_providers of its public providers are in a bucket, it
// admits the country's best providers that the bar alone keeps out of that
// bucket (providerEgressBarredBuckets), in the bucket's own order, until the
// bucket holds that many or the candidates run out. The buckets fill Quality,
// Speed, then Online: each native the floor admits joins Online too, as every
// native does. An admission holds for every location and group copy of the
// provider, so a provider is decided the same way in every target it sits in;
// a city or region holding none of them answers from its country through the
// location fallback.
//
// A candidate passes everything but the bar. It is in the export's cohort (an
// active, top-level, connected and valid provider with a Public provide key),
// no hard exclusion took it out of the maps (reliability, ARIN risk, a TLS
// finding the judge ruled its own, zero carry), it passes the reliability
// minimums, and every other rule of the bucket admits it: ARIN non-quality
// stays out of Quality, the native URL-run minimums and the modern provider
// gate hold, and an unmeasured provider is no candidate for a native bucket.
// Today's facets, which every request without a selector reads, keep the hard
// gates of UDP and IP family (FP2-PROTOCOL.md §3): a provider the probe found
// unable to carry UDP, or IPv6 it advertises (CapabilityDowngrades), is never
// admitted there. Its classified copy can be, in its capability facet, which
// only selector-mode requests that take its class read, and where its labels
// keep UDP flows off a tcp exit (§6); there the classified decision decides
// and the floor counts what selector mode draws. Every per-request filter
// still applies to an admitted provider: caller network exclusions, excluded
// ids, liveness leases, live subscriber validation, and the post-quantum and
// modern requirements of strict requests.
//
// The floor only adds. A provider the bar admits keeps its bucket, tier and
// weight. An admitted native draws with the weight its mode would give it
// (ClientScore.rankWeights) and carries its tier moved into the Online
// fallback's band, twice backfill_tier_offset past it, so it sits behind every
// native and borrowed native of a request even with the client's demerits and
// carries flows only where nothing that passed a bar can.
//
// The native census and capture the flag's presence read the decisions before
// the floor: neither counts a provider the bar keeps out. The admitted supply
// (network_client_score_supply.go) counts the floor's admissions, and the
// location counts read the strict ones the export publishes
// ({provider_country_floor}), so a floored country stays listed.
package model

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// The buckets of the floor, as bits of ClientScore.countryFloor, of the
// barred buckets and of the admissions.
const (
	clientScoreCountryFloorQuality uint8 = 1 << iota
	clientScoreCountryFloorSpeed
	clientScoreCountryFloorOnline
)

// One bucket of the floor.
type clientScoreCountryFloorBucket struct {
	bit uint8
	// the metric label
	name string
	// the native mode; "" for Online
	rankMode RankMode
}

// The buckets in the order the floor fills them: the native modes first,
// since each native the floor admits joins Online too.
var clientScoreCountryFloorBuckets = []clientScoreCountryFloorBucket{
	{bit: clientScoreCountryFloorQuality, name: RankModeQuality, rankMode: RankModeQuality},
	{bit: clientScoreCountryFloorSpeed, name: RankModeSpeed, rankMode: RankModeSpeed},
	{bit: clientScoreCountryFloorOnline, name: RankModeOnline},
}

// The decisions the floor fills, as the metric labels them: the strict
// decision of today's facets, and the classified decision of the capability
// facets (FP2-PROTOCOL.md §4.4).
const (
	clientScoreCountryFloorStrict     = "strict"
	clientScoreCountryFloorClassified = "classified"
)

var clientScoreCountryFloorDecisions = []string{clientScoreCountryFloorStrict, clientScoreCountryFloorClassified}

// The buckets the URL bar alone keeps the provider out of: those the decision
// would admit it to were its URL evidence at the bar, and does not. Meeting
// the bar sets the ratio verdict passing and lifts the Online URL ratio and
// the rule of one success, and nothing else: the common gates, zero carry,
// the native URL-run minimums, ARIN non-quality, the modern provider gate and
// the want of any evidence still decide, so they keep the provider where they
// put it. A hard exclusion is in no bucket either way.
func providerEgressBarredBuckets(facts *providerEgressFacts, decision providerEgressDecision, egressTestEnabled bool) uint8 {
	if decision.hardExcluded {
		return 0
	}
	atBar := *facts
	atBar.noUrlSuccess = false
	atBar.lowUrlSuccess = false
	if atBar.egressQuality != nil {
		passes := true
		atBar.egressQuality = &passes
	}
	met := decideProviderEgress(&atBar, egressTestEnabled)
	var barredBuckets uint8
	if met.quality && !decision.quality {
		barredBuckets |= clientScoreCountryFloorQuality
	}
	if met.speed && !decision.speed {
		barredBuckets |= clientScoreCountryFloorSpeed
	}
	if met.online && !decision.online {
		barredBuckets |= clientScoreCountryFloorOnline
	}
	return barredBuckets
}

// Whether the score is in the bucket by its own decision.
func (self clientScoreCountryFloorBucket) admits(clientScore *ClientScore) bool {
	if self.rankMode == "" {
		return clientScore.Online
	}
	return clientScore.PassesMinimums[self.rankMode]
}

// The bucket's own order, best first. Quality and Speed by the mode's tier and
// score, lowest first, then the URL success weight, highest first; Online by
// the Online fallback's order (compareOnlineFallback), then the URL success
// weight. Ties go to the lower client id, so consecutive exports choose alike.
func (self clientScoreCountryFloorBucket) compare(a *ClientScore, b *ClientScore) int {
	if self.rankMode != "" {
		if d := cmp.Compare(a.Tiers[self.rankMode], b.Tiers[self.rankMode]); d != 0 {
			return d
		}
		if d := cmp.Compare(a.Scores[self.rankMode], b.Scores[self.rankMode]); d != 0 {
			return d
		}
		if d := cmp.Compare(b.UrlProbeSuccessWeight, a.UrlProbeSuccessWeight); d != 0 {
			return d
		}
	}
	if d := compareOnlineFallback(a, b); d != 0 {
		return d
	}
	if d := cmp.Compare(b.UrlProbeSuccessWeight, a.UrlProbeSuccessWeight); d != 0 {
		return d
	}
	return a.ClientId.Cmp(b.ClientId)
}

// One country's admissions of one decision and bucket.
type clientScoreCountryFloorKey struct {
	country  string
	decision string
	bucket   string
}

// All countries' admissions of one decision and bucket.
type clientScoreCountryFloorFleetKey struct {
	decision string
	bucket   string
}

// What the floor of one export did.
type clientScoreCountryFloor struct {
	// the floor, in providers per bucket; off when not positive
	providers int
	// the providers admitted, by country, decision and bucket; a country
	// without an admission is absent
	countryCounts map[clientScoreCountryFloorKey]int
	// the strict decision's admitted providers, every one of them in Online
	// with the admission, which the location counts read
	strictClientIds map[server.Id]bool
	// the countries with a public provider whose bucket the floor left
	// empty, by decision and bucket: none of their providers qualifies
	emptyCountries map[clientScoreCountryFloorFleetKey][]string
}

// Admits each country's best providers below the bar to the buckets that hold
// fewer than providers of its public providers, in every location and group
// copy, and returns what it admitted. locationClientScores and
// locationGroupClientScores are the export's ranked target maps,
// countryCodes the country targets' codes by location id,
// clientIdBarredBuckets the strict decision's barred buckets by provider and
// clientIdCapabilities the classifications, whose barredBuckets are the
// classified decision's. nativeTierOffset moves an admitted native's tier
// into the Online fallback's band. Every copy's maps are its own but its
// tiers, which it shares with its lowest lookback, so an admission replaces
// them.
func applyClientScoreCountryFloor(
	providers int,
	nativeTierOffset int,
	locationClientScores map[server.Id]map[server.Id]*ClientScore,
	locationGroupClientScores map[server.Id]map[server.Id]*ClientScore,
	countryCodes map[server.Id]string,
	clientIdBarredBuckets map[server.Id]uint8,
	clientIdCapabilities map[server.Id]*clientScoreCapability,
) *clientScoreCountryFloor {
	countryFloor := &clientScoreCountryFloor{
		providers:       providers,
		countryCounts:   map[clientScoreCountryFloorKey]int{},
		strictClientIds: map[server.Id]bool{},
		emptyCountries:  map[clientScoreCountryFloorFleetKey][]string{},
	}
	if providers <= 0 {
		return countryFloor
	}

	// the buckets each decision admits each provider to
	clientIdStrictBuckets := map[server.Id]uint8{}
	clientIdClassifiedBuckets := map[server.Id]uint8{}
	onlineBucket := clientScoreCountryFloorBuckets[len(clientScoreCountryFloorBuckets)-1]

	// a provider the floor may admit: its country copy, which membership
	// reads, and the record the bucket orders it by, the copy itself or its
	// classified copy
	type countryFloorCandidate struct {
		clientScore *ClientScore
		ranked      *ClientScore
	}

	// Fills each bucket of one decision of one country. The members are
	// counted with the admissions so far, so Online counts the natives just
	// admitted. candidate gives the record a provider outside the bucket is
	// ordered by, nil when the floor cannot admit it there.
	fill := func(
		country string,
		decision string,
		clientScores []*ClientScore,
		member func(clientScore *ClientScore, bucket clientScoreCountryFloorBucket) bool,
		candidate func(clientScore *ClientScore, bucket clientScoreCountryFloorBucket) *ClientScore,
		clientIdBuckets map[server.Id]uint8,
	) {
		for _, bucket := range clientScoreCountryFloorBuckets {
			memberCount := 0
			for _, clientScore := range clientScores {
				if member(clientScore, bucket) {
					memberCount += 1
				}
			}
			// a country that holds the floor, nearly every one by its
			// providers, gathers no candidates
			if providers <= memberCount {
				continue
			}
			candidates := []countryFloorCandidate{}
			for _, clientScore := range clientScores {
				if member(clientScore, bucket) {
					continue
				}
				if ranked := candidate(clientScore, bucket); ranked != nil {
					candidates = append(candidates, countryFloorCandidate{clientScore: clientScore, ranked: ranked})
				}
			}
			slices.SortFunc(candidates, func(a countryFloorCandidate, b countryFloorCandidate) int {
				return bucket.compare(a.ranked, b.ranked)
			})
			admitted := candidates[:min(providers-memberCount, len(candidates))]
			for _, admittedCandidate := range admitted {
				buckets := bucket.bit
				// a native joins Online with its admission
				if !member(admittedCandidate.clientScore, onlineBucket) {
					buckets |= clientScoreCountryFloorOnline
				}
				clientIdBuckets[admittedCandidate.clientScore.ClientId] |= buckets
			}
			if memberCount+len(admitted) == 0 && 0 < len(clientScores) {
				fleetKey := clientScoreCountryFloorFleetKey{decision: decision, bucket: bucket.name}
				countryFloor.emptyCountries[fleetKey] = append(countryFloor.emptyCountries[fleetKey], country)
			}
		}
	}

	for countryLocationId, clientScores := range locationClientScores {
		country, ok := countryCodes[countryLocationId]
		if !ok {
			continue
		}
		publicClientScores := []*ClientScore{}
		for _, clientScore := range clientScores {
			if !clientScore.NetworkOnly {
				publicClientScores = append(publicClientScores, clientScore)
			}
		}

		// Today's facets: the strict decision, without a provider the probe
		// took a capability from, whatever the request carries.
		strictMember := func(clientScore *ClientScore, bucket clientScoreCountryFloorBucket) bool {
			return bucket.admits(clientScore) || clientIdStrictBuckets[clientScore.ClientId]&bucket.bit != 0
		}
		fill(
			country,
			clientScoreCountryFloorStrict,
			publicClientScores,
			strictMember,
			func(clientScore *ClientScore, bucket clientScoreCountryFloorBucket) *ClientScore {
				if clientScore.CapabilityDowngrades != 0 || !clientScore.passesReliability || clientIdBarredBuckets[clientScore.ClientId]&bucket.bit == 0 {
					return nil
				}
				return clientScore
			},
			clientIdStrictBuckets,
		)

		// What selector mode draws (FP2-PROTOCOL.md §6.3): a provider no
		// capability was taken from by its strict record, the floor's
		// included, and a downgraded one by its classified copy alone.
		classifiedMember := func(clientScore *ClientScore, bucket clientScoreCountryFloorBucket) bool {
			if clientScore.capabilityCopy == nil {
				return clientScore.CapabilityDowngrades == 0 && strictMember(clientScore, bucket)
			}
			return bucket.admits(clientScore.capabilityCopy) || clientIdClassifiedBuckets[clientScore.ClientId]&bucket.bit != 0
		}
		fill(
			country,
			clientScoreCountryFloorClassified,
			publicClientScores,
			classifiedMember,
			func(clientScore *ClientScore, bucket clientScoreCountryFloorBucket) *ClientScore {
				classified := clientScore.capabilityCopy
				if classified == nil || !classified.passesReliability {
					return nil
				}
				capability := clientIdCapabilities[clientScore.ClientId]
				if capability == nil || capability.barredBuckets&bucket.bit == 0 {
					return nil
				}
				return classified
			},
			clientIdClassifiedBuckets,
		)

		// a provider sits in one country target, so its admissions are this
		// country's
		count := func(decision string, clientIdBuckets map[server.Id]uint8) {
			for _, clientScore := range publicClientScores {
				buckets := clientIdBuckets[clientScore.ClientId]
				for _, bucket := range clientScoreCountryFloorBuckets {
					if buckets&bucket.bit != 0 {
						countryFloor.countryCounts[clientScoreCountryFloorKey{country: country, decision: decision, bucket: bucket.name}] += 1
					}
				}
			}
		}
		count(clientScoreCountryFloorStrict, clientIdStrictBuckets)
		count(clientScoreCountryFloorClassified, clientIdClassifiedBuckets)
	}

	for clientId := range clientIdStrictBuckets {
		countryFloor.strictClientIds[clientId] = true
	}
	admit := func(targetClientScores map[server.Id]map[server.Id]*ClientScore) {
		for _, clientScores := range targetClientScores {
			for clientId, clientScore := range clientScores {
				if buckets := clientIdStrictBuckets[clientId]; buckets != 0 {
					admitClientScoreToCountryFloor(clientScore, buckets, nativeTierOffset)
				}
				if buckets := clientIdClassifiedBuckets[clientId]; buckets != 0 && clientScore.capabilityCopy != nil {
					admitClientScoreToCountryFloor(clientScore.capabilityCopy, buckets, nativeTierOffset)
				}
			}
		}
	}
	admit(locationClientScores)
	admit(locationGroupClientScores)
	for _, countries := range countryFloor.emptyCountries {
		slices.Sort(countries)
	}
	return countryFloor
}

// Admits one copy of a provider to buckets: each native mode with the weight
// the mode would draw it by and its tier moved nativeTierOffset into the
// Online fallback's band, and Online. A bucket the copy is already in keeps
// it as it is.
func admitClientScoreToCountryFloor(clientScore *ClientScore, buckets uint8, nativeTierOffset int) {
	for _, bucket := range clientScoreCountryFloorBuckets {
		if buckets&bucket.bit == 0 || bucket.rankMode == "" || clientScore.PassesMinimums[bucket.rankMode] {
			continue
		}
		// the tiers are shared with the lowest lookback, and so with every
		// other copy of the provider
		tiers := maps.Clone(clientScore.Tiers)
		if tiers == nil {
			tiers = map[string]int{}
		}
		tiers[bucket.rankMode] = nativeTierOffset + clientScore.Tiers[bucket.rankMode]
		clientScore.Tiers = tiers
		if clientScore.ScaledWeights == nil {
			clientScore.ScaledWeights = map[string]float32{}
		}
		clientScore.ScaledWeights[bucket.rankMode] = clientScore.rankWeights[bucket.rankMode]
		if clientScore.PassesMinimums == nil {
			clientScore.PassesMinimums = map[string]bool{}
		}
		clientScore.PassesMinimums[bucket.rankMode] = true
	}
	clientScore.Online = true
	clientScore.countryFloor |= buckets
}

// The admissions of all countries, by decision and bucket, zero included.
func (self *clientScoreCountryFloor) fleetCounts() map[clientScoreCountryFloorFleetKey]int {
	fleetCounts := map[clientScoreCountryFloorFleetKey]int{}
	for _, decision := range clientScoreCountryFloorDecisions {
		for _, bucket := range clientScoreCountryFloorBuckets {
			fleetCounts[clientScoreCountryFloorFleetKey{decision: decision, bucket: bucket.name}] += 0
		}
	}
	for key, count := range self.countryCounts {
		fleetCounts[clientScoreCountryFloorFleetKey{decision: key.decision, bucket: key.bucket}] += count
	}
	return fleetCounts
}

// One line for the export's log: the floor, each decision's admissions per
// bucket with the countries they went to, and the countries it left empty.
// Every list is of countries, so the line stays bounded.
func (self *clientScoreCountryFloor) logSummary() string {
	if self.providers <= 0 {
		return "off (egress_index.country_floor_providers is not positive)"
	}
	list := func(countries []string) string {
		return strings.Join(countries, " ")
	}
	texts := []string{}
	for _, decision := range clientScoreCountryFloorDecisions {
		for _, bucket := range clientScoreCountryFloorBuckets {
			countryTexts := []string{}
			total := 0
			for key, count := range self.countryCounts {
				if key.decision == decision && key.bucket == bucket.name {
					countryTexts = append(countryTexts, fmt.Sprintf("%s %d", key.country, count))
					total += count
				}
			}
			slices.Sort(countryTexts)
			text := fmt.Sprintf("%s %s +%d in %d countries [%s]", decision, bucket.name, total, len(countryTexts), list(countryTexts))
			if empty := self.emptyCountries[clientScoreCountryFloorFleetKey{decision: decision, bucket: bucket.name}]; 0 < len(empty) {
				text += fmt.Sprintf(", %d left empty [%s]", len(empty), list(empty))
			}
			texts = append(texts, text)
		}
	}
	return fmt.Sprintf("%d providers per bucket; %s", self.providers, strings.Join(texts, "; "))
}

// The strict admissions of the latest score export, for the location counts:
// a set of client ids with a ready member, replaced every export with its
// ttl. A count without it counts the decisions alone.
const providerCountryFloorKey = "{provider_country_floor}"

const providerCountryFloorReadyMember = "ready:v1"

// Replaces the published set with the export's strict admissions, waiting at
// most timeout.
func publishProviderCountryFloor(ctx context.Context, clientIds map[server.Id]bool, ttl time.Duration, timeout time.Duration) error {
	members := []any{providerCountryFloorReadyMember}
	for clientId := range clientIds {
		members = append(members, clientId.String())
	}
	redisCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return server.RedisWithDeadline(redisCtx, func(r server.RedisClient) error {
		_, err := r.TxPipelined(redisCtx, func(pipe redis.Pipeliner) error {
			pipe.Del(redisCtx, providerCountryFloorKey)
			pipe.SAdd(redisCtx, providerCountryFloorKey, members...)
			pipe.Expire(redisCtx, providerCountryFloorKey, ttl)
			return nil
		})
		return err
	})
}

// The published strict admissions, waiting at most timeout; an error when
// none is published or Redis is unavailable.
func readProviderCountryFloor(ctx context.Context, timeout time.Duration) (map[server.Id]bool, error) {
	var members []string
	redisCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := server.RedisWithDeadline(redisCtx, func(r server.RedisClient) error {
		var err error
		members, err = r.SMembers(redisCtx, providerCountryFloorKey).Result()
		return err
	})
	if err != nil {
		return nil, err
	}
	if !slices.Contains(members, providerCountryFloorReadyMember) {
		return nil, errors.New("none is published")
	}
	clientIds := map[server.Id]bool{}
	for _, member := range members {
		if member == providerCountryFloorReadyMember {
			continue
		}
		clientId, err := server.ParseId(member)
		if err != nil {
			return nil, fmt.Errorf("a member is not a client id: %w", err)
		}
		clientIds[clientId] = true
	}
	return clientIds, nil
}
