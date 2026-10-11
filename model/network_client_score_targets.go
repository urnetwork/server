// The pure middle of UpdateClientScores: staged source rows become per-target
// provider maps, each target gets its cache key family, and a target's
// providers encode into its export payload. The source queries, common gates
// and Redis writes stay in UpdateClientScores.
package model

import (
	mathrand "math/rand"

	"github.com/urnetwork/server"
)

// What the rules of connect/GEOMAP.md §10.3 read about a provider from its
// rollup row, the same in both pool queries.
type clientScoreEgress struct {
	egressIndex   *int
	egressQuality *bool
	// the country of the rollup's location, which the country gate
	// compares with a fresh probe's
	publishedCountryCode *string
}

// One provider/lookback row of either score source query, staged until the
// common gates of the whole cohort are loaded. A location row carries its
// city, region and country ids; a group row carries the distinct groups of
// those three levels.
type clientScoreSourceRow struct {
	score                       *ClientScore
	cityId, regionId, countryId *server.Id
	reputationFailedNames       string
	egress                      *clientScoreEgress
	locationGroup               bool
	locationGroupIds            []server.Id
}

// The provider maps of one score export, keyed by target and then client.
type clientScoreTargetMaps struct {
	locationClientScores      map[server.Id]map[server.Id]*ClientScore
	locationGroupClientScores map[server.Id]map[server.Id]*ClientScore
	// the rollup of every admitted provider, decided once per provider
	clientIdEgresses map[server.Id]*clientScoreEgress
	// providers a current hard egress failure keeps out of every target
	hardExcludedClientIds map[server.Id]bool
}

// Moves each staged row into its targets' provider maps, merging a provider's
// lookback rows into one top-level score per target. A provider with a
// current hard egress failure joins no target, but the targets its rows name
// are still created, and every known location group is a target even when no
// row names it. The export then writes an encoded-empty payload for a target
// without admitted providers, so a reader can tell "no providers" from "never
// written" (cache_empty, not cache_missing), and a target that lost its last
// admitted provider stops serving its previous payload instead of keeping it
// until the cache ttl. A location no row names at all is still not visited.
// Rows are cleared as they are consumed so their staging references are
// released early. observe, when set, sees every row before the hard-exclusion
// check.
func newClientScoreTargetMaps(
	rows []clientScoreSourceRow,
	knownLocationGroupIds []server.Id,
	hardExcluded func(clientId server.Id) bool,
	observe func(row clientScoreSourceRow),
) *clientScoreTargetMaps {
	self := &clientScoreTargetMaps{
		locationClientScores:      map[server.Id]map[server.Id]*ClientScore{},
		locationGroupClientScores: map[server.Id]map[server.Id]*ClientScore{},
		clientIdEgresses:          map[server.Id]*clientScoreEgress{},
		hardExcludedClientIds:     map[server.Id]bool{},
	}
	for _, locationGroupId := range knownLocationGroupIds {
		self.locationGroupClientScores[locationGroupId] = map[server.Id]*ClientScore{}
	}

	addClientScore := func(lookbackClientScore *ClientScore, reputationFailedNames string, m map[server.Id]*ClientScore) *ClientScore {
		clientScore, ok := m[lookbackClientScore.ClientId]
		if !ok {
			clientScore = &ClientScore{
				ClientId:              lookbackClientScore.ClientId,
				NetworkId:             lookbackClientScore.NetworkId,
				NetworkOnly:           lookbackClientScore.NetworkOnly,
				ReputationFailedNames: reputationFailedNames,
				IpFamilies:            lookbackClientScore.IpFamilies,
				LookbackClientScores:  map[int]*ClientScore{},
			}
			m[lookbackClientScore.ClientId] = clientScore
		}
		clientScore.LookbackClientScores[lookbackClientScore.LookbackIndex] = lookbackClientScore
		return clientScore
	}

	for i, row := range rows {
		// Release staging references as each row is moved into its target maps.
		rows[i] = clientScoreSourceRow{}
		lookbackClientScore, reputationFailedNames, egress := row.score, row.reputationFailedNames, row.egress
		if observe != nil {
			observe(row)
		}
		excluded := hardExcluded(lookbackClientScore.ClientId)
		if excluded {
			self.hardExcludedClientIds[lookbackClientScore.ClientId] = true
		} else {
			self.clientIdEgresses[lookbackClientScore.ClientId] = egress
		}
		if !row.locationGroup {
			cityLocationId, regionLocationId, countryLocationId := row.cityId, row.regionId, row.countryId

			// top-level only; the lookback copies stay nil (see `ClientScore`)
			setLocationIds := func(clientScore *ClientScore) {
				clientScore.CityLocationId = cityLocationId
				clientScore.RegionLocationId = regionLocationId
				clientScore.CountryLocationId = countryLocationId
			}

			// once per distinct location id: a country-only client stores
			// its country id in all three columns (see
			// SetConnectionLocation), and a client belongs in a location's
			// pool once. The per-location map is keyed by client id so a
			// repeat is already absorbed, but going through the set makes
			// the intent explicit and keeps this loop in step with the
			// counting loop in UpdateClientLocations.
			for _, locationId := range distinctIds(
				cityLocationId,
				regionLocationId,
				countryLocationId,
			) {
				clientScores, ok := self.locationClientScores[locationId]
				if !ok {
					clientScores = map[server.Id]*ClientScore{}
					self.locationClientScores[locationId] = clientScores
				}
				if !excluded {
					setLocationIds(addClientScore(lookbackClientScore, reputationFailedNames, clientScores))
				}
			}
		} else {
			// One provider/lookback row carries every distinct group at
			// its three location levels, including country-only clients.
			for _, locationGroupId := range row.locationGroupIds {
				clientScores, ok := self.locationGroupClientScores[locationGroupId]
				if !ok {
					clientScores = map[server.Id]*ClientScore{}
					self.locationGroupClientScores[locationGroupId] = clientScores
				}
				if !excluded {
					addClientScore(lookbackClientScore, reputationFailedNames, clientScores)
				}
			}
		}
	}
	return self
}

// One target of the export and its admitted providers, possibly none.
type clientScoreTarget struct {
	id            server.Id
	locationGroup bool
	clientScores  map[server.Id]*ClientScore
}

// Every location and location-group target of the maps, in map order.
func (self *clientScoreTargetMaps) targets() []clientScoreTarget {
	targets := make([]clientScoreTarget, 0, len(self.locationClientScores)+len(self.locationGroupClientScores))
	for locationId, clientScores := range self.locationClientScores {
		targets = append(targets, clientScoreTarget{id: locationId, clientScores: clientScores})
	}
	for locationGroupId, clientScores := range self.locationGroupClientScores {
		targets = append(targets, clientScoreTarget{
			id:            locationGroupId,
			locationGroup: true,
			clientScores:  clientScores,
		})
	}
	return targets
}

// The cache key family of one target in one export mode, for any caller
// location. These are the same key builders the readers use.
func newClientScoreTargetKeys(forceMinimum bool, rankMode RankMode, target clientScoreTarget) clientScoreTargetKeys {
	if target.locationGroup {
		return clientScoreTargetKeys{
			counts: func(callerId server.Id) string {
				return clientScoreLocationGroupCountsKey(forceMinimum, rankMode, target.id, callerId)
			},
			filter: func(callerId server.Id) string {
				return clientScoreLocationGroupFilterKey(forceMinimum, rankMode, target.id, callerId)
			},
			sample: func(callerId server.Id, sampleIndex int) string {
				return clientScoreLocationGroupSampleKey(forceMinimum, rankMode, target.id, callerId, sampleIndex)
			},
			alias: func(callerId server.Id) string {
				return clientScoreLocationGroupAliasKey(forceMinimum, rankMode, target.id, callerId)
			},
			facetCounts: func(callerId server.Id, facet ipFamilyFacet) string {
				return clientScoreLocationGroupFacetCountsKey(forceMinimum, rankMode, target.id, callerId, facet)
			},
			facetSample: func(callerId server.Id, facet ipFamilyFacet, sampleIndex int) string {
				return clientScoreLocationGroupFacetSampleKey(forceMinimum, rankMode, target.id, callerId, facet, sampleIndex)
			},
		}
	}
	return clientScoreTargetKeys{
		counts: func(callerId server.Id) string {
			return clientScoreLocationCountsKey(forceMinimum, rankMode, target.id, callerId)
		},
		filter: func(callerId server.Id) string {
			return clientScoreLocationFilterKey(forceMinimum, rankMode, target.id, callerId)
		},
		sample: func(callerId server.Id, sampleIndex int) string {
			return clientScoreLocationSampleKey(forceMinimum, rankMode, target.id, callerId, sampleIndex)
		},
		alias: func(callerId server.Id) string {
			return clientScoreLocationAliasKey(forceMinimum, rankMode, target.id, callerId)
		},
		facetCounts: func(callerId server.Id, facet ipFamilyFacet) string {
			return clientScoreLocationFacetCountsKey(forceMinimum, rankMode, target.id, callerId, facet)
		},
		facetSample: func(callerId server.Id, facet ipFamilyFacet, sampleIndex int) string {
			return clientScoreLocationFacetSampleKey(forceMinimum, rankMode, target.id, callerId, facet, sampleIndex)
		},
	}
}

// Shuffles and buckets one list of scores into ClientScoreSampleCount-sized
// samples. Encode on demand so 48 parallel caller-location exporters retain
// at most one sample each, rather than every encoded sample for their current
// provider location. The returned bytes move directly into the 512-item/8MiB
// streaming writer and are cleared after the synchronous Exec.
func splitClientScoreSamples(clientScores []*ClientScore) (countsBytes []byte, counts []int, encodeSample func(int) []byte) {
	mathrand.Shuffle(len(clientScores), func(i int, j int) {
		clientScores[i], clientScores[j] = clientScores[j], clientScores[i]
	})

	n := (len(clientScores) + ClientScoreSampleCount - 1) / ClientScoreSampleCount

	counts = make([]int, n)
	clientsPerSample := 0
	if 0 < n {
		clientsPerSample = (len(clientScores) + n - 1) / n
		for i := range n {
			i0 := i * clientsPerSample
			i1 := min((i+1)*clientsPerSample, len(clientScores))
			counts[i] = i1 - i0
		}
	}
	encodeSample = func(i int) []byte {
		i0 := i * clientsPerSample
		i1 := min((i+1)*clientsPerSample, len(clientScores))
		return encodeClientScoreGobValue(updateClientScoresPhaseMetrics, clientScores[i0:i1])
	}

	countsBytes = encodeClientScoreGobValue(updateClientScoresPhaseMetrics, counts)
	return
}

// Encodes one target's providers for one export mode: the un-faceted union,
// every family facet, the native facets of a non-forced mode, and the public
// ClientFilter.
func exportClientScores(forceMinimum bool, rankMode RankMode, s map[server.Id]*ClientScore) clientScoreExportPayload {
	mapSpan := updateClientScoresPhaseMetrics.start(updateClientScoresPhaseTargetMap)
	defer mapSpan.finish()
	clientScores := []*ClientScore{}
	facetClientScores := map[ipFamilyFacet][]*ClientScore{}
	nativeFacetClientScores := map[ipFamilyFacet][]*ClientScore{}
	publicCount := 0
	publicNetReliabilityWeight := float64(0)
	for _, clientScore := range s {
		if !forceMinimum && clientScore.PassesMinimums[rankMode] {
			facet := clientScore.ipFamilyFacet()
			nativeFacetClientScores[facet] = append(nativeFacetClientScores[facet], clientScore)
		}
		// an online provider sits in both modes' samples, native to
		// neither: FindProviders2 borrows from it last
		if clientScore.PassesMinimums[rankMode] || clientScore.Online || forceMinimum {
			clientScores = append(clientScores, clientScore)
			facet := clientScore.ipFamilyFacet()
			facetClientScores[facet] = append(facetClientScores[facet], clientScore)
			if !clientScore.NetworkOnly {
				publicCount += 1
				publicNetReliabilityWeight += clientScore.ReliabilityWeight
			}
		}
	}

	// the samples above carry Network-only providers too -- FindProviders2
	// filters them per request against the caller's network -- but the
	// ClientFilter does not. It is read only by loadLocationStables, which
	// decides the `Stable` flag GetProviderLocations publishes to every
	// user. That is a public surface, so like the provider count in
	// UpdateClientLocations it counts only providers a stranger can reach:
	// a location whose only supply is network-only is not stable, and with
	// zero public providers it reports no providers at all.
	filter := &ClientFilter{
		Count:                publicCount,
		NetReliabilityWeight: publicNetReliabilityWeight,
	}

	payload := clientScoreExportPayload{
		facets: map[ipFamilyFacet]clientScoreFacetPayload{},
	}
	updateClientScoresPhaseMetrics.addWork(updateClientScoresPhaseTargetMap, len(s), 0)
	mapSpan.finish()
	payload.countsBytes, payload.counts, payload.encodeSample = splitClientScoreSamples(clientScores)
	if !forceMinimum {
		payload.nativeFacets = map[ipFamilyFacet]clientScoreFacetPayload{}
		for _, facet := range ipFamilyFacets {
			nativePayload := clientScoreFacetPayload{}
			nativePayload.countsBytes, nativePayload.counts, nativePayload.encodeSample = splitClientScoreSamples(nativeFacetClientScores[facet])
			payload.nativeFacets[facet] = nativePayload
		}
	}
	// every facet, an empty one included: see the key layout comment
	for _, facet := range ipFamilyFacets {
		facetPayload := clientScoreFacetPayload{}
		facetPayload.countsBytes, facetPayload.counts, facetPayload.encodeSample = splitClientScoreSamples(facetClientScores[facet])
		payload.facets[facet] = facetPayload
	}

	payload.filterBytes = encodeClientScoreGobValue(updateClientScoresPhaseMetrics, filter)

	return payload
}
