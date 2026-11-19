// SIGNALS.md §2.1a. The address-local channel still decides every admission.
// The grant returned to a caller owns its release regardless of channel FIFO.
package monitor

import "sync"

// This bookkeeping shares the exact lifetime and address key of the semaphore.
// Successful channel sends can precede publication under mu. Such a gap makes
// the snapshot incomplete. No blocking acquisition or callback runs under mu.
type hostCommandSlots struct {
	permits chan struct{}

	mu       sync.Mutex
	ordinal  uint64
	released uint64
	holders  [maxConcurrentRemoteCommandsPerHost]pgHostPermitGrant
	recent   [maxConcurrentRemoteCommandsPerHost]pgHostPermitGrant
}

// A grant ordinal is local to this address and limiter instance. Only the
// static catalog key, or "unknown", enters a private receipt.
type pgHostPermitGrant struct {
	GrantOrdinal uint64 `json:"grant_ordinal"`
	SignalKey    string `json:"signal_key"`
}

// Active counts occupied channel slots, excluding this sampler's own grant.
// An incomplete census withholds the list rather than misidentifying an owner.
type pgHostPermitSnapshot struct {
	Status  string              `json:"status"`
	Active  int                 `json:"active"`
	Holders []pgHostPermitGrant `json:"holders"`
}

// The two snapshots bracket this caller's channel select, including local
// descheduling. Releases contains at most the two most recent exact releases.
type pgSampleHostPermitHolders struct {
	Schema       int                  `json:"schema"`
	AtRequest    pgHostPermitSnapshot `json:"at_request"`
	AtReturn     pgHostPermitSnapshot `json:"at_return"`
	ReleaseCount *uint64              `json:"release_count"`
	Releases     []pgHostPermitGrant  `json:"releases"`
	Attribution  string               `json:"attribution"`
}

// Register only a successful send. The index stays with that one release
// closure. Neither a custom larger semaphore nor exhausted ordinals affect admission.
func (self *hostCommandSlots) rememberGrantWithLock(key string) int {
	if cap(self.permits) <= len(self.holders) && self.ordinal < ^uint64(0) {
		for i, holder := range self.holders {
			if holder.GrantOrdinal == 0 {
				self.ordinal++
				self.holders[i] = pgHostPermitGrant{GrantOrdinal: self.ordinal, SignalKey: key}
				return i
			}
		}
	}
	return -1
}

// The caller owns one successful send, so the original receive cannot block.
// Seal only that closure's grant, even when a different command entered first.
func (self *hostCommandSlots) release(holder int) {
	self.mu.Lock()
	defer self.mu.Unlock()
	<-self.permits
	if holder >= 0 {
		copy(self.recent[:], self.recent[1:])
		self.recent[len(self.recent)-1] = self.holders[holder]
		self.holders[holder] = pgHostPermitGrant{}
	}
	if self.released < ^uint64(0) {
		self.released++
	}
}

// Releases and publication are fenced by mu. An unregistered channel send can
// increase occupancy concurrently, so sample its count once as the census point.
func (self *hostCommandSlots) snapshotWithLock(own int) pgHostPermitSnapshot {
	active := len(self.permits) - own
	snapshot := pgHostPermitSnapshot{Status: "unavailable", Active: active}
	if cap(self.permits) > len(self.holders) || self.ordinal == ^uint64(0) {
		return snapshot
	}
	holders := make([]pgHostPermitGrant, 0, len(self.holders))
	for _, holder := range self.holders {
		if holder.GrantOrdinal != 0 {
			holders = append(holders, holder)
		}
	}
	if len(holders) == active {
		snapshot.Status = "ok"
		snapshot.Holders = holders
	}
	return snapshot
}

// Close the original request window before publishing this caller's own grant.
// A sole release can explain admission only from a complete, full initial census.
func (self *hostCommandSlots) finishPgObservationWithLock(observation *pgSampleTransportObservation, releaseStart uint64, granted bool) {
	if observation == nil {
		return
	}
	detail := observation.receipt.HostPermitHolders
	own := 0
	if granted {
		own = 1
	}
	detail.AtReturn = self.snapshotWithLock(own)
	detail.Attribution = "unavailable"
	if cap(self.permits) > len(self.holders) || self.ordinal == ^uint64(0) || self.released == ^uint64(0) {
		return
	}
	count := self.released - releaseStart
	detail.ReleaseCount = &count
	retained := min(count, uint64(len(self.recent)))
	detail.Releases = append([]pgHostPermitGrant{}, self.recent[len(self.recent)-int(retained):]...)
	if !granted {
		detail.Attribution = "not_granted"
		return
	}
	if detail.AtRequest.Status != "ok" || detail.AtReturn.Status != "ok" {
		return
	}
	switch {
	case count == 0 && detail.AtRequest.Active < cap(self.permits):
		detail.Attribution = "no_release_required"
	case count > 1:
		detail.Attribution = "multiple_releases"
	case detail.AtRequest.Active < cap(self.permits):
		detail.Attribution = "not_full_at_request"
	case count == 1:
		for _, holder := range detail.AtRequest.Holders {
			if holder == detail.Releases[0] {
				detail.Attribution = "unique_release"
				return
			}
		}
	}
}
