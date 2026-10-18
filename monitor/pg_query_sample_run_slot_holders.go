// SIGNALS.md §2.1a identifies occupied global permits without inferring why
// their owning signals have not returned. These are snapshots, not a timeline.
package monitor

import "context"

// Both samples belong to one actual pool request. Direct probes omit this
// block; a request that never receives a grant has no new durable PG receipt.
type pgSampleRunSlotHolders struct {
	Schema      int                      `json:"schema"`
	Enqueued    *pgSampleRunSlotSnapshot `json:"enqueued"`
	BeforeGrant *pgSampleRunSlotSnapshot `json:"before_grant"`
}

// A complete snapshot has at most four holders, including unknown public keys.
// Invalid clocks retain occupancy but withhold every age and capture offset.
type pgSampleRunSlotSnapshot struct {
	Status     string                  `json:"status"`
	CapturedNs *int64                  `json:"captured_ns"`
	Active     int                     `json:"active"`
	Holders    []pgSampleRunSlotHolder `json:"holders"`
}

// Grant age measures local elapsed ownership, not command time or CPU use.
type pgSampleRunSlotHolder struct {
	SignalKey  string `json:"signal_key"`
	GrantAgeNs *int64 `json:"grant_age_ns"`
}

// Called under the existing pool lock, before enqueue or the selected grant.
// Only a PG request has a recorder. No projection field affects dispatch.
func (self *runSlotPool) observePgHoldersWithLock(ctx context.Context, beforeGrant bool) {
	observation := pgSampleTransportFromContext(ctx)
	if observation == nil {
		return
	}
	now := observation.localNow()
	snapshot := &pgSampleRunSlotSnapshot{
		Status: "ok", CapturedNs: observation.localOffset(now), Active: self.active,
		Holders: []pgSampleRunSlotHolder{},
	}
	for _, holder := range self.holders {
		if holder == nil {
			continue
		}
		age := now.Sub(holder.grantedAt).Nanoseconds()
		if age < 0 || holder.grantedAt.IsZero() {
			snapshot.Status = "clock_invalid"
		}
		snapshot.Holders = append(snapshot.Holders, pgSampleRunSlotHolder{SignalKey: holder.signalKey, GrantAgeNs: &age})
	}
	if snapshot.CapturedNs == nil {
		snapshot.Status = "clock_invalid"
	}
	if snapshot.Status == "clock_invalid" {
		snapshot.CapturedNs = nil
		for i := range snapshot.Holders {
			snapshot.Holders[i].GrantAgeNs = nil
		}
	}
	if self.active < 0 || self.active > len(self.holders) || self.active != len(snapshot.Holders) {
		// A different-sized/custom pool or unavailable bookkeeping cannot be
		// silently represented as a complete four-holder projection.
		snapshot.Status = "unavailable"
		snapshot.Holders = nil
	}
	if beforeGrant {
		if observation.receipt.RunSlotHolders != nil {
			observation.receipt.RunSlotHolders.BeforeGrant = snapshot
		}
	} else {
		observation.receipt.RunSlotHolders = &pgSampleRunSlotHolders{Schema: 1, Enqueued: snapshot}
	}
}
