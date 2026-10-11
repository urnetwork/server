package model

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

// An absent score key preserves the existing cache-miss/fallback policy.
// Every other read error invalidates the requested batch, even when Exec
// returns only an earlier redis.Nil. Do not retry or return a partial pool.
func execClientScoreReadPipeline(ctx context.Context, pipeline redis.Pipeliner) error {
	commands, err := pipeline.Exec(ctx)
	for _, command := range commands {
		if commandErr := command.Err(); commandErr != nil && !errors.Is(commandErr, redis.Nil) {
			return commandErr
		}
	}
	if err != nil && !errors.Is(err, redis.Nil) {
		return err
	}
	return nil
}

// One count document's caller-keyed read and, for a nonzero caller, the
// zero-caller baseline read that the caller's alias may select instead.
type clientScoreCountsRead struct {
	caller   *redis.StringCmd
	baseline *redis.StringCmd
}

// One requested target's caller alias and count document reads.
type clientScoreTargetRead struct {
	alias     *redis.StringCmd
	unfaceted clientScoreCountsRead
	facets    map[ipFamilyFacet]clientScoreCountsRead
}

// Queues the alias and count reads of every requested target through get,
// which the reader binds to one pipeline. The reads hold results only after
// that pipeline executes.
func newClientScoreTargetReads(
	forceMinimum bool,
	rankMode RankMode,
	locationIds map[server.Id]bool,
	locationGroupIds map[server.Id]bool,
	clientLocationId server.Id,
	facets []ipFamilyFacet,
	get func(key string) *redis.StringCmd,
) (locationReads map[server.Id]clientScoreTargetRead, locationGroupReads map[server.Id]clientScoreTargetRead) {
	locationReads = map[server.Id]clientScoreTargetRead{}
	locationGroupReads = map[server.Id]clientScoreTargetRead{}
	for locationId, _ := range locationIds {
		read := clientScoreTargetRead{
			unfaceted: clientScoreCountsRead{
				caller: get(clientScoreLocationCountsKey(forceMinimum, rankMode, locationId, clientLocationId)),
			},
			facets: map[ipFamilyFacet]clientScoreCountsRead{},
		}
		if clientLocationId != (server.Id{}) {
			read.alias = get(clientScoreLocationAliasKey(forceMinimum, rankMode, locationId, clientLocationId))
			read.unfaceted.baseline = get(clientScoreLocationCountsKey(forceMinimum, rankMode, locationId, server.Id{}))
		}
		for _, facet := range facets {
			facetRead := clientScoreCountsRead{
				caller: get(clientScoreLocationFacetCountsKey(forceMinimum, rankMode, locationId, clientLocationId, facet)),
			}
			if clientLocationId != (server.Id{}) {
				facetRead.baseline = get(clientScoreLocationFacetCountsKey(forceMinimum, rankMode, locationId, server.Id{}, facet))
			}
			read.facets[facet] = facetRead
		}
		locationReads[locationId] = read
	}
	for locationGroupId, _ := range locationGroupIds {
		read := clientScoreTargetRead{
			unfaceted: clientScoreCountsRead{
				caller: get(clientScoreLocationGroupCountsKey(forceMinimum, rankMode, locationGroupId, clientLocationId)),
			},
			facets: map[ipFamilyFacet]clientScoreCountsRead{},
		}
		if clientLocationId != (server.Id{}) {
			read.alias = get(clientScoreLocationGroupAliasKey(forceMinimum, rankMode, locationGroupId, clientLocationId))
			read.unfaceted.baseline = get(clientScoreLocationGroupCountsKey(forceMinimum, rankMode, locationGroupId, server.Id{}))
		}
		for _, facet := range facets {
			facetRead := clientScoreCountsRead{
				caller: get(clientScoreLocationGroupFacetCountsKey(forceMinimum, rankMode, locationGroupId, clientLocationId, facet)),
			}
			if clientLocationId != (server.Id{}) {
				facetRead.baseline = get(clientScoreLocationGroupFacetCountsKey(forceMinimum, rankMode, locationGroupId, server.Id{}, facet))
			}
			read.facets[facet] = facetRead
		}
		locationGroupReads[locationGroupId] = read
	}
	return
}

// Decodes the executed count reads into sample page counts grouped by draw
// order: one group per requested facet, then the un-faceted fallback. A
// target with any requested facet present, an encoded-empty one included, was
// written by a faceted export, and an absent facet only marks the source
// incomplete. A target with neither facets nor un-faceted counts is missing
// metadata, which the observation records: unknown cache visibility, not a
// target without providers.
func clientScoreTargetPageCounts(
	forceMinimum bool,
	rankMode RankMode,
	clientLocationId server.Id,
	facets []ipFamilyFacet,
	locationReads map[server.Id]clientScoreTargetRead,
	locationGroupReads map[server.Id]clientScoreTargetRead,
	observation *findProviders2LoadObservation,
) (groupSampleKeyCounts []map[string]int, sourceIncomplete bool, returnErr error) {
	// sample keys grouped by draw order: one group per requested facet,
	// then the un-faceted fallback group
	groupCount := len(facets) + 1
	groupSampleKeyCounts = make([]map[string]int, groupCount)
	for i := range groupSampleKeyCounts {
		groupSampleKeyCounts[i] = map[string]int{}
	}

	decodeCounts := func(alias *redis.StringCmd, read clientScoreCountsRead) (effectiveClientLocationId server.Id, counts []int, ok bool) {
		effectiveClientLocationId, countsBytes := selectClientScorePayload(
			clientLocationId,
			clientScoreCommandBytes(alias),
			clientScoreCommandBytes(read.caller),
			clientScoreCommandBytes(read.baseline),
		)
		if len(countsBytes) == 0 {
			return
		}
		b := bytes.NewBuffer(countsBytes)
		e := gob.NewDecoder(b)
		if err := e.Decode(&counts); err != nil {
			returnErr = err
			return
		}
		ok = true
		return
	}

	addTarget := func(
		read clientScoreTargetRead,
		groupId *server.Id,
		facetSampleKey func(effectiveClientLocationId server.Id, facet ipFamilyFacet, index int) string,
		sampleKey func(effectiveClientLocationId server.Id, index int) string,
	) {
		faceted := false
		facetCount := 0
		for facetIndex, facet := range facets {
			effectiveClientLocationId, counts, ok := decodeCounts(read.alias, read.facets[facet])
			if returnErr != nil {
				return
			}
			if !ok {
				continue
			}
			faceted = true
			facetCount++
			for i, count := range counts {
				groupSampleKeyCounts[facetIndex][facetSampleKey(effectiveClientLocationId, facet, i)] = count
			}
		}
		if faceted {
			if facetCount != len(facets) {
				sourceIncomplete = true
			}
			return
		}
		// written before the facets existed: fall back to the un-faceted buckets
		effectiveClientLocationId, counts, ok := decodeCounts(read.alias, read.unfaceted)
		if returnErr != nil || !ok {
			sourceIncomplete = true
			if returnErr == nil && observation != nil {
				observation.missingTarget(groupId, clientLocationId, rankMode)
			}
			return
		}
		for i, count := range counts {
			groupSampleKeyCounts[groupCount-1][sampleKey(effectiveClientLocationId, i)] = count
		}
	}

	for locationId, read := range locationReads {
		addTarget(
			read,
			nil,
			func(effectiveClientLocationId server.Id, facet ipFamilyFacet, index int) string {
				return clientScoreLocationFacetSampleKey(forceMinimum, rankMode, locationId, effectiveClientLocationId, facet, index)
			},
			func(effectiveClientLocationId server.Id, index int) string {
				return clientScoreLocationSampleKey(forceMinimum, rankMode, locationId, effectiveClientLocationId, index)
			},
		)
		if returnErr != nil {
			return
		}
	}
	for locationGroupId, read := range locationGroupReads {
		addTarget(
			read,
			&locationGroupId,
			func(effectiveClientLocationId server.Id, facet ipFamilyFacet, index int) string {
				return clientScoreLocationGroupFacetSampleKey(forceMinimum, rankMode, locationGroupId, effectiveClientLocationId, facet, index)
			},
			func(effectiveClientLocationId server.Id, index int) string {
				return clientScoreLocationGroupSampleKey(forceMinimum, rankMode, locationGroupId, effectiveClientLocationId, index)
			},
		)
		if returnErr != nil {
			return
		}
	}
	return
}
