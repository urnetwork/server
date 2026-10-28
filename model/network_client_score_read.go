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

// One requested target's caller alias and count document reads, and the
// read of its facets' modern partition (network_client_score_modern.go).
type clientScoreTargetRead struct {
	alias     *redis.StringCmd
	unfaceted clientScoreCountsRead
	facets    map[ipFamilyFacet]clientScoreCountsRead
	modern    clientScoreCountsRead
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
		read.modern.caller = get(clientScoreLocationModernKey(forceMinimum, rankMode, locationId, clientLocationId))
		if clientLocationId != (server.Id{}) {
			read.alias = get(clientScoreLocationAliasKey(forceMinimum, rankMode, locationId, clientLocationId))
			read.unfaceted.baseline = get(clientScoreLocationCountsKey(forceMinimum, rankMode, locationId, server.Id{}))
			read.modern.baseline = get(clientScoreLocationModernKey(forceMinimum, rankMode, locationId, server.Id{}))
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
		read.modern.caller = get(clientScoreLocationGroupModernKey(forceMinimum, rankMode, locationGroupId, clientLocationId))
		if clientLocationId != (server.Id{}) {
			read.alias = get(clientScoreLocationGroupAliasKey(forceMinimum, rankMode, locationGroupId, clientLocationId))
			read.unfaceted.baseline = get(clientScoreLocationGroupCountsKey(forceMinimum, rankMode, locationGroupId, server.Id{}))
			read.modern.baseline = get(clientScoreLocationGroupModernKey(forceMinimum, rankMode, locationGroupId, server.Id{}))
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
// order: one group per requested facet, then the un-faceted fallback. It is
// clientScoreTargetPages with each facet's partition classes merged, the
// traversal before the partition.
func clientScoreTargetPageCounts(
	forceMinimum bool,
	rankMode RankMode,
	clientLocationId server.Id,
	facets []ipFamilyFacet,
	locationReads map[server.Id]clientScoreTargetRead,
	locationGroupReads map[server.Id]clientScoreTargetRead,
	observation *findProviders2LoadObservation,
) (groupSampleKeyCounts []map[string]int, sourceIncomplete bool, returnErr error) {
	facetPageSets, unfacetedPages, _, sourceIncomplete, returnErr := clientScoreTargetPages(
		forceMinimum,
		rankMode,
		clientLocationId,
		facets,
		locationReads,
		locationGroupReads,
		observation,
	)
	if returnErr != nil {
		return nil, sourceIncomplete, returnErr
	}
	groupSampleKeyCounts = make([]map[string]int, 0, len(facetPageSets)+1)
	for _, facetPageSet := range facetPageSets {
		sampleKeyCounts := map[string]int{}
		for _, pages := range []map[string]clientScorePage{facetPageSet.modern, facetPageSet.other} {
			for key, page := range pages {
				sampleKeyCounts[key] = page.expectedCount
			}
		}
		groupSampleKeyCounts = append(groupSampleKeyCounts, sampleKeyCounts)
	}
	sampleKeyCounts := map[string]int{}
	for key, page := range unfacetedPages {
		sampleKeyCounts[key] = page.expectedCount
	}
	groupSampleKeyCounts = append(groupSampleKeyCounts, sampleKeyCounts)
	return groupSampleKeyCounts, sourceIncomplete, nil
}

// One requested facet's union pages by partition class
// (network_client_score_modern.go).
type clientScoreFacetPageSet struct {
	// the leading modern pages of partitioned targets, each with its
	// checksum, and every page of stale targets
	modern map[string]clientScorePage
	// the other pages of partitioned targets, and every page of absent ones
	other map[string]clientScorePage
}

// Decodes the executed count and partition reads into sample pages: per
// requested facet by partition class, then the un-faceted fallback, which is
// never partitioned. A target with any requested facet present, an
// encoded-empty one included, was written by a faceted export, and an absent
// facet only marks the source incomplete. A target with neither facets nor
// un-faceted counts is missing metadata, which the observation records:
// unknown cache visibility, not a target without providers. targetStates
// counts the faceted targets by partition state: absent without a partition,
// stale when any facet's partition does not describe the counts read, else
// partitioned.
func clientScoreTargetPages(
	forceMinimum bool,
	rankMode RankMode,
	clientLocationId server.Id,
	facets []ipFamilyFacet,
	locationReads map[server.Id]clientScoreTargetRead,
	locationGroupReads map[server.Id]clientScoreTargetRead,
	observation *findProviders2LoadObservation,
) (facetPageSets []clientScoreFacetPageSet, unfacetedPages map[string]clientScorePage, targetStates map[string]int, sourceIncomplete bool, returnErr error) {
	facetPageSets = make([]clientScoreFacetPageSet, len(facets))
	for i := range facetPageSets {
		facetPageSets[i] = clientScoreFacetPageSet{
			modern: map[string]clientScorePage{},
			other:  map[string]clientScorePage{},
		}
	}
	unfacetedPages = map[string]clientScorePage{}
	targetStates = map[string]int{}

	decodeCounts := func(alias *redis.StringCmd, read clientScoreCountsRead) (effectiveClientLocationId server.Id, countsBytes []byte, counts []int, ok bool) {
		effectiveClientLocationId, countsBytes = selectClientScorePayload(
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
		targetState := ""
		for facetIndex, facet := range facets {
			effectiveClientLocationId, countsBytes, counts, ok := decodeCounts(read.alias, read.facets[facet])
			if returnErr != nil {
				return
			}
			if !ok {
				continue
			}
			faceted = true
			facetCount++
			// the partition of the payload the counts came from: the
			// baseline's when the alias selected it
			partitionBytes := clientScoreCommandBytes(read.modern.caller)
			if effectiveClientLocationId != clientLocationId {
				partitionBytes = clientScoreCommandBytes(read.modern.baseline)
			}
			state, modernPageChecksums := clientScoreUnionFacetPartition(partitionBytes, facet, countsBytes, counts)
			switch {
			case targetState == "", state == clientScoreModernStale:
				targetState = state
			case targetState != state && targetState != clientScoreModernStale:
				targetState = clientScoreModernStale
			}
			for i, count := range counts {
				key := facetSampleKey(effectiveClientLocationId, facet, i)
				page := clientScorePage{key: key, expectedCount: count}
				switch {
				case state == clientScoreModernStale:
					facetPageSets[facetIndex].modern[key] = page
				case i < len(modernPageChecksums):
					page.modernChecksum = modernPageChecksums[i]
					page.hasModernChecksum = true
					facetPageSets[facetIndex].modern[key] = page
				default:
					page.modernRecovery = state == clientScoreModernPartitioned
					facetPageSets[facetIndex].other[key] = page
				}
			}
		}
		if faceted {
			targetStates[targetState] += 1
			if facetCount != len(facets) {
				sourceIncomplete = true
			}
			return
		}
		// written before the facets existed: fall back to the un-faceted buckets
		effectiveClientLocationId, _, counts, ok := decodeCounts(read.alias, read.unfaceted)
		if returnErr != nil || !ok {
			sourceIncomplete = true
			if returnErr == nil && observation != nil {
				observation.missingTarget(groupId, clientLocationId, rankMode)
			}
			return
		}
		targetStates[clientScoreModernAbsent] += 1
		for i, count := range counts {
			key := sampleKey(effectiveClientLocationId, i)
			unfacetedPages[key] = clientScorePage{key: key, expectedCount: count}
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
