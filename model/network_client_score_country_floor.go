// The country floor of the score export (owner decision 2026-11-13: "keep the
// 80% URL-probe bar, but never let a country go empty. Always admit each
// country's best few providers even below the bar"; FP2FIX.md, "The country
// floor").
//
// The URL bar stays every provider's decision: the 4/5 success ratio of
// Quality and Speed, the Online URL ratio and the rule of one success
// (egress_index.go, egress_online_success.go). Once the export has decided and
// ranked every provider, the floor looks at each country target. Where fewer
// than country_floor_providers of its public providers are online, it admits
// to Online the country's best providers that the bar alone keeps out of it
// (providerEgressBarredFromOnline), in the Online fallback's order, until
// Online holds that many or the candidates run out. The admission holds for
// every location and group copy of the provider, so a provider is decided the
// same way in every target it sits in; a city or region holding none of them
// answers from its country through the location fallback.
//
// Online is the one bucket every answer ends in: a Quality or Speed request
// draws its own natives, borrows the other bucket's, and then the Online
// fallback lends what is left, at twice backfill_tier_offset. So the floor
// keeps every mode of a country served while Quality and Speed keep the bar's
// members alone. A provider admitted as a native instead would be drawn with
// the request's natives, ahead of the other bucket's natives that passed the
// bar: it could take their places in a short answer and would put a worse
// tier ahead of a better one. Lent from Online, an admitted provider fills
// only the places every provider that passed a bar left, behind them all,
// even with the client's demerits.
//
// A candidate passes everything but the bar. It is in the export's cohort (an
// active, top-level, connected and valid provider with a Public provide key),
// no hard exclusion took it out of the maps (reliability, ARIN risk, a TLS
// finding the judge ruled its own, zero carry), and it passes the reliability
// minimums. Today's facets, which every request without a selector reads,
// keep the hard gates of UDP and IP family (FP2-PROTOCOL.md §3): a provider
// the probe found unable to carry UDP, or IPv6 it advertises
// (CapabilityDowngrades), is never admitted there. Its classified copy can
// be, in its capability facet, which only selector-mode requests that take
// its class read, and where its labels keep UDP flows off a tcp exit (§6);
// there the classified decision decides, and the floor counts what selector
// mode draws. Every per-request filter still applies to an admitted provider:
// caller network exclusions, excluded ids, liveness leases, and the
// post-quantum and modern requirements of strict requests.
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
	"slices"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// The decisions the floor fills, as the metric labels them: the strict
// decision of today's facets, and the classified decision of the capability
// facets (FP2-PROTOCOL.md §4.4).
const (
	clientScoreCountryFloorStrict     = "strict"
	clientScoreCountryFloorClassified = "classified"
)

var clientScoreCountryFloorDecisions = []string{clientScoreCountryFloorStrict, clientScoreCountryFloorClassified}

// Whether the URL bar alone keeps the provider out of Online: the decision
// would admit it were its URL evidence at the bar, and does not. Meeting the
// bar sets the ratio verdict passing and lifts the Online URL ratio and the
// rule of one success, and nothing else, so only no_success and low_success
// providers are barred; a hard exclusion is in no bucket either way.
func providerEgressBarredFromOnline(facts *providerEgressFacts, decision providerEgressDecision, egressTestEnabled bool) bool {
	if decision.hardExcluded || decision.online {
		return false
	}
	atBar := *facts
	atBar.noUrlSuccess = false
	atBar.lowUrlSuccess = false
	if atBar.egressQuality != nil {
		passes := true
		atBar.egressQuality = &passes
	}
	return decideProviderEgress(&atBar, egressTestEnabled).online
}

// The Online fallback's order, best first (compareOnlineFallback): reliability
// weight times URL success weight, then the speed tier and score; then the
// URL success weight alone, and ties to the lower client id, so consecutive
// exports choose alike.
func compareClientScoreCountryFloor(a *ClientScore, b *ClientScore) int {
	if d := compareOnlineFallback(a, b); d != 0 {
		return d
	}
	if d := cmp.Compare(b.UrlProbeSuccessWeight, a.UrlProbeSuccessWeight); d != 0 {
		return d
	}
	return a.ClientId.Cmp(b.ClientId)
}

// One country's admissions of one decision.
type clientScoreCountryFloorKey struct {
	country  string
	decision string
}

// What the floor of one export did.
type clientScoreCountryFloor struct {
	// the floor, in providers; off when not positive
	providers int
	// the providers admitted, by country and decision; a country without an
	// admission is absent
	countryCounts map[clientScoreCountryFloorKey]int
	// the strict decision's admitted providers, which the location counts
	// read
	strictClientIds map[server.Id]bool
	// the countries with a public provider whose Online the floor left
	// empty, by decision: none of their providers qualifies
	emptyCountries map[string][]string
}

// Admits each country's best providers below the bar to Online where it holds
// fewer than providers of the country's public providers, in every location
// and group copy, and returns what it admitted. locationClientScores and
// locationGroupClientScores are the export's ranked target maps, countryCodes
// the country targets' codes by location id, clientIdBarredFromOnline the
// providers the bar alone keeps out of the strict decision's Online, and
// clientIdCapabilities the classifications, whose barredFromOnline is the
// classified decision's.
func applyClientScoreCountryFloor(
	providers int,
	locationClientScores map[server.Id]map[server.Id]*ClientScore,
	locationGroupClientScores map[server.Id]map[server.Id]*ClientScore,
	countryCodes map[server.Id]string,
	clientIdBarredFromOnline map[server.Id]bool,
	clientIdCapabilities map[server.Id]*clientScoreCapability,
) *clientScoreCountryFloor {
	countryFloor := &clientScoreCountryFloor{
		providers:       providers,
		countryCounts:   map[clientScoreCountryFloorKey]int{},
		strictClientIds: map[server.Id]bool{},
		emptyCountries:  map[string][]string{},
	}
	if providers <= 0 {
		return countryFloor
	}

	// the providers each decision admits
	strictClientIds := map[server.Id]bool{}
	classifiedClientIds := map[server.Id]bool{}

	// a provider the floor may admit: its country copy, which membership
	// reads, and the record Online orders it by, the copy itself or its
	// classified copy
	type countryFloorCandidate struct {
		clientScore *ClientScore
		ranked      *ClientScore
	}

	// Fills Online for one decision of one country. candidate gives the
	// record a provider outside Online is ordered by, nil when the floor
	// cannot admit it.
	fill := func(
		country string,
		decision string,
		clientScores []*ClientScore,
		member func(clientScore *ClientScore) bool,
		candidate func(clientScore *ClientScore) *ClientScore,
		admittedClientIds map[server.Id]bool,
	) {
		memberCount := 0
		for _, clientScore := range clientScores {
			if member(clientScore) {
				memberCount += 1
			}
		}
		// a country that holds the floor, nearly every one by its providers,
		// gathers no candidates
		if providers <= memberCount {
			return
		}
		candidates := []countryFloorCandidate{}
		for _, clientScore := range clientScores {
			if member(clientScore) {
				continue
			}
			if ranked := candidate(clientScore); ranked != nil {
				candidates = append(candidates, countryFloorCandidate{clientScore: clientScore, ranked: ranked})
			}
		}
		slices.SortFunc(candidates, func(a countryFloorCandidate, b countryFloorCandidate) int {
			return compareClientScoreCountryFloor(a.ranked, b.ranked)
		})
		admitted := candidates[:min(providers-memberCount, len(candidates))]
		for _, admittedCandidate := range admitted {
			admittedClientIds[admittedCandidate.clientScore.ClientId] = true
		}
		if 0 < len(admitted) {
			countryFloor.countryCounts[clientScoreCountryFloorKey{country: country, decision: decision}] += len(admitted)
		}
		if memberCount+len(admitted) == 0 && 0 < len(clientScores) {
			countryFloor.emptyCountries[decision] = append(countryFloor.emptyCountries[decision], country)
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
		strictMember := func(clientScore *ClientScore) bool {
			return clientScore.Online || strictClientIds[clientScore.ClientId]
		}
		fill(
			country,
			clientScoreCountryFloorStrict,
			publicClientScores,
			strictMember,
			func(clientScore *ClientScore) *ClientScore {
				if clientScore.CapabilityDowngrades != 0 || !clientScore.passesReliability || !clientIdBarredFromOnline[clientScore.ClientId] {
					return nil
				}
				return clientScore
			},
			strictClientIds,
		)

		// What selector mode draws (FP2-PROTOCOL.md §6.3): a provider no
		// capability was taken from by its strict record, the floor's
		// included, and a downgraded one by its classified copy alone.
		fill(
			country,
			clientScoreCountryFloorClassified,
			publicClientScores,
			func(clientScore *ClientScore) bool {
				if clientScore.capabilityCopy == nil {
					return clientScore.CapabilityDowngrades == 0 && strictMember(clientScore)
				}
				return clientScore.capabilityCopy.Online || classifiedClientIds[clientScore.ClientId]
			},
			func(clientScore *ClientScore) *ClientScore {
				classified := clientScore.capabilityCopy
				if classified == nil || !classified.passesReliability {
					return nil
				}
				capability := clientIdCapabilities[clientScore.ClientId]
				if capability == nil || !capability.barredFromOnline {
					return nil
				}
				return classified
			},
			classifiedClientIds,
		)
	}

	for clientId := range strictClientIds {
		countryFloor.strictClientIds[clientId] = true
	}
	admit := func(targetClientScores map[server.Id]map[server.Id]*ClientScore) {
		for _, clientScores := range targetClientScores {
			for clientId, clientScore := range clientScores {
				if strictClientIds[clientId] {
					admitClientScoreToCountryFloor(clientScore)
				}
				if classifiedClientIds[clientId] && clientScore.capabilityCopy != nil {
					admitClientScoreToCountryFloor(clientScore.capabilityCopy)
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

// Admits one copy of a provider to Online, its native modes as they were.
func admitClientScoreToCountryFloor(clientScore *ClientScore) {
	clientScore.Online = true
	clientScore.countryFloor = true
}

// The admissions of all countries, by decision, zero included.
func (self *clientScoreCountryFloor) fleetCounts() map[string]int {
	fleetCounts := map[string]int{}
	for _, decision := range clientScoreCountryFloorDecisions {
		fleetCounts[decision] += 0
	}
	for key, count := range self.countryCounts {
		fleetCounts[key.decision] += count
	}
	return fleetCounts
}

// One line for the export's log: the floor, each decision's admissions with
// the countries they went to, and the countries it left without an online
// provider. Every list is of countries, so the line stays bounded.
func (self *clientScoreCountryFloor) logSummary() string {
	if self.providers <= 0 {
		return "off (egress_index.country_floor_providers is not positive)"
	}
	texts := []string{}
	for _, decision := range clientScoreCountryFloorDecisions {
		countryTexts := []string{}
		total := 0
		for key, count := range self.countryCounts {
			if key.decision == decision {
				countryTexts = append(countryTexts, fmt.Sprintf("%s %d", key.country, count))
				total += count
			}
		}
		slices.Sort(countryTexts)
		text := fmt.Sprintf("%s +%d online in %d countries [%s]", decision, total, len(countryTexts), strings.Join(countryTexts, " "))
		if empty := self.emptyCountries[decision]; 0 < len(empty) {
			text += fmt.Sprintf(", %d left without one [%s]", len(empty), strings.Join(empty, " "))
		}
		texts = append(texts, text)
	}
	return fmt.Sprintf("%d providers per country; %s", self.providers, strings.Join(texts, "; "))
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
