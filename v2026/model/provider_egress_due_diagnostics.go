package model

// Identity-free diagnostics accompany the existing due selection; they neither
// claim work nor change its ordering, retry policy, or response identifiers.

import "time"

// Finite selection lanes. An overlapping candidate belongs to its earliest
// deadline; equal deadlines retain the existing first-head tie break.
type ProviderEgressDueLane int

const (
	ProviderEgressDueNoLocation ProviderEgressDueLane = iota
	ProviderEgressDueStaleLocation
	ProviderEgressDueStaleHealth
	ProviderEgressDueMissingHealth
	ProviderEgressDueLaneCount
)

// Counts actual selected rows, not candidates offered by the bounded heads.
type ProviderEgressDueCount struct {
	Current int
	Expired int
}

// Has no provider identifiers or caller-controlled label values.
type ProviderEgressDueDiagnostics struct {
	Selected [ProviderEgressDueLaneCount]ProviderEgressDueCount
}

// Unlocated rows have no evidence deadline. Equality is not expired, matching
// the direct coverage probe's strict deadline predicate.
func (self *ProviderEgressDueDiagnostics) record(lane ProviderEgressDueLane, deadline time.Time, now time.Time) {
	if lane != ProviderEgressDueNoLocation && deadline.Before(now) {
		self.Selected[lane].Expired++
	} else {
		self.Selected[lane].Current++
	}
}
