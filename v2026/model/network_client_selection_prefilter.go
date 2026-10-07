package model

import (
	"slices"

	"github.com/urnetwork/server/v2026"
)

// Request-only refusals require no current database facts. Apply the same
// predicates when choosing candidates to validate and when filtering the pool,
// so rejected retry history or another network's private providers cannot
// consume subscriber/security query capacity. Every remaining candidate still
// passes the common hard gates and the selected bucket's subscriber policy.
type clientScoreRequestFilter struct {
	callerNetworkId   server.Id
	facets            []ipFamilyFacet
	excludedClientIds map[server.Id]uint8
}

// The nonzero values match the existing request-filter observation cells;
// hard exclusions keep cell zero and are evaluated separately.
func (self clientScoreRequestFilter) rejection(clientId server.Id, score *ClientScore) int {
	if score.NetworkOnly && score.NetworkId != self.callerNetworkId {
		return 1
	}
	if !slices.Contains(self.facets, score.ipFamilyFacet()) {
		return 2
	}
	if self.excludedClientIds[clientId] != 0 {
		return 3
	}
	return 0
}

func (self clientScoreRequestFilter) unchecked(scores map[server.Id]*ClientScore, checked map[server.Id]bool) []server.Id {
	ids := make([]server.Id, 0, len(scores))
	for clientId, score := range scores {
		if !checked[clientId] && self.rejection(clientId, score) == 0 {
			ids = append(ids, clientId)
		}
	}
	return ids
}

func (self clientScoreRequestFilter) apply(scores map[server.Id]*ClientScore, observation *findProviders2SelectionObservation) {
	for clientId, score := range scores {
		if reason := self.rejection(clientId, score); reason != 0 {
			observation.dropped[reason]++
			if reason == 3 {
				observation.explicitSources |= self.excludedClientIds[clientId]
			}
			delete(scores, clientId)
		}
	}
}
