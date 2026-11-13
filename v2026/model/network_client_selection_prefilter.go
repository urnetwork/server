package model

import (
	"slices"
	"time"

	"github.com/urnetwork/connect/v2026"

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
	// a request that requires modern providers refuses every other
	// (FP2-MODERN.md)
	requireModern bool
	// Selector mode (FP2-PROTOCOL.md §6.3): the classes of the pool the
	// request draws, which replace the family facets as the family filter;
	// zero for a legacy request.
	capabilityClasses providerCapabilityClassSet
	// once the post-quantum marker is set, a strict post-quantum request
	// refuses every provider without a positive signal (§12.4), in the
	// same capability cell
	requirePostQuantumPositive bool
	// One instant shared by every draw and named-provider check of the request.
	now time.Time
}

// The nonzero values match the existing request-filter observation cells;
// hard exclusions keep cell zero and are evaluated separately. Cell 4 is a
// capability refusal, which the declared post-quantum refusals also count in.
func (self clientScoreRequestFilter) rejection(clientId server.Id, score *ClientScore) int {
	if score.NetworkOnly && score.NetworkId != self.callerNetworkId {
		return 1
	}
	if self.capabilityClasses != 0 {
		if !self.capabilityClasses.has(score.capabilityClass()) || score.IpFamily() == connect.IpFamilyV6Only {
			return 2
		}
	} else if !slices.Contains(self.facets, score.ipFamilyFacet()) {
		return 2
	}
	if self.excludedClientIds[clientId] != 0 {
		return 3
	}
	if self.requireModern && !score.Modern {
		return 4
	}
	if self.requirePostQuantumPositive {
		if !score.postQuantumCurrent(self.now) ||
			(self.requireModern && !self.now.Before(score.PostQuantumModernValidUntil)) {
			return 4
		}
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
