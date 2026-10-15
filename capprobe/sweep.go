// The sweep over the connected providers: its place between passes, one
// pass's selection, and where the sweep stands after the pass.
package capprobe

import (
	"context"
	"slices"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The probe's place in its sweep over the connected providers, carried from
// each pass to the next in the task's arguments.
//
// A sweep reads the population once, in client id order, a bounded page at
// a time, and measures each provider that is due and eligible as it passes
// it. A provider that gave no evidence is due again, but the sweep has moved
// on: the next sweep retries it, and a new sweep starts no sooner than
// min_sweep_interval_minutes after the last one started. So a provider is
// measured at most once a sweep, and a provider that never answers costs one
// tunnel a sweep. The only per-provider state is the downgrades held back
// from fresh modern evidence until a later sweep confirms them.
type Sweep struct {
	// the last provider the sweep has passed; zero before a sweep starts
	Cursor server.Id `json:"cursor"`
	// when the current sweep started; zero before the first
	StartedAt time.Time `json:"started_at,omitempty"`
	// the earliest a new sweep may start; zero for at once
	NextSweepAt time.Time `json:"next_sweep_at,omitempty"`
	// negative readings held back from fresh modern evidence, oldest first,
	// at most maxHeldDowngrades; carried from sweep to sweep
	HeldDowngrades []HeldDowngrade `json:"held_downgrades,omitempty"`
}

// A negative reading held back from a provider's modern evidence while that
// evidence was fresh (model.RecordProviderModernEvidenceHoldingDowngrade).
// The provider stays due, so the next sweep measures it again: a negative
// reading then confirms the downgrade and is recorded, and any reading that
// is recorded, a modern one included, retires it.
type HeldDowngrade struct {
	ClientId server.Id `json:"client_id"`
	// the start of the sweep that held it
	SweepStartedAt time.Time `json:"sweep_started_at"`
}

// The most held downgrades a sweep carries in the task's arguments, a few KiB
// each pass. A negative reading held past it is held again, untracked, and
// the sweep after can carry it; it is never recorded unconfirmed.
const maxHeldDowngrades = 256

// One pass's view of the sweep's held downgrades: whether a negative reading
// confirms one an earlier sweep held, and which the next pass carries. Not
// safe for concurrent use; the pass's lock guards it.
type heldDowngradeSet struct {
	sweepStartedAt time.Time
	heldDowngrades []HeldDowngrade
}

// The held downgrades of sweep still worth carrying at now. One held as long
// ago as the staleness window is retired: the modern evidence it held up was
// due for a refresh when it was held, so it has gone stale since, and the
// gate no longer reads it as modern.
func newHeldDowngradeSet(sweep Sweep, now time.Time) *heldDowngradeSet {
	heldDowngrades := []HeldDowngrade{}
	for _, heldDowngrade := range sweep.HeldDowngrades {
		if now.Sub(heldDowngrade.SweepStartedAt) < model.ProviderModernEvidenceMaxAge {
			heldDowngrades = append(heldDowngrades, heldDowngrade)
		}
	}
	return &heldDowngradeSet{
		sweepStartedAt: sweep.StartedAt,
		heldDowngrades: heldDowngrades,
	}
}

// Whether a negative reading of clientId confirms a downgrade an earlier
// sweep held.
func (self *heldDowngradeSet) confirms(clientId server.Id) bool {
	for _, heldDowngrade := range self.heldDowngrades {
		if heldDowngrade.ClientId == clientId {
			return heldDowngrade.SweepStartedAt.Before(self.sweepStartedAt)
		}
	}
	return false
}

// Carries a downgrade of clientId held now, unless one is carried already or
// the set is full.
func (self *heldDowngradeSet) hold(clientId server.Id) {
	for _, heldDowngrade := range self.heldDowngrades {
		if heldDowngrade.ClientId == clientId {
			return
		}
	}
	if len(self.heldDowngrades) < maxHeldDowngrades {
		self.heldDowngrades = append(self.heldDowngrades, HeldDowngrade{ClientId: clientId, SweepStartedAt: self.sweepStartedAt})
	}
}

// Retires the downgrade held for clientId, whose evidence was recorded.
func (self *heldDowngradeSet) retire(clientId server.Id) {
	self.heldDowngrades = slices.DeleteFunc(self.heldDowngrades, func(heldDowngrade HeldDowngrade) bool {
		return heldDowngrade.ClientId == clientId
	})
}

// The held downgrades the next pass carries, nil for none.
func (self *heldDowngradeSet) carried() []HeldDowngrade {
	if len(self.heldDowngrades) == 0 {
		return nil
	}
	return slices.Clone(self.heldDowngrades)
}

// Whether a sweep is under way.
func (self Sweep) inProgress() bool {
	return self.Cursor != (server.Id{}) || !self.StartedAt.IsZero()
}

// How often a probe between sweeps runs a waiting pass, which reads nothing:
// it keeps urnetwork_capability_probe_last_pass_timestamp_seconds current, so
// a probe that stopped is told apart from one that waits.
const waitingPassInterval = 10 * time.Minute

// When the pass after one that left sweep at now should run: within a sweep
// after the pass interval, between sweeps at the next sweep's start or the
// waiting pass interval, whichever comes first.
func (self Settings) NextPassAt(sweep Sweep, now time.Time) time.Time {
	runAt := now.Add(self.PassInterval())
	if !sweep.inProgress() && runAt.Before(sweep.NextSweepAt) {
		if waitingAt := now.Add(waitingPassInterval); waitingAt.Before(sweep.NextSweepAt) {
			return waitingAt
		}
		return sweep.NextSweepAt
	}
	return runAt
}

// The providers one pass selected, in client id order, and where the sweep
// stands after them. The counts cover only the providers the pass passed, up
// to its cursor, so a provider the next pass reads again counts once.
type selection struct {
	dueClientIds []server.Id
	// the last provider passed: the last selected one when the limit cut a
	// page short, else the end of the last page read
	cursor server.Id
	// the last page read was short: the population ends with this selection
	endReached bool
	scanned    int
	due        int
	eligible   int
}

// The fewest due providers one eligibility read judges.
const minEligibilityChunk = 32

// Reads pages after the sweep's cursor until the pass has its limit of due,
// eligible providers, the population ends, or max_pages_per_pass pages are
// read. Evidence is due at now: a modern row after modern_refresh_hours,
// any other after other_refresh_hours, no row at once.
//
// A page's due providers are judged for eligibility in chunks of twice the
// pass's remaining room, so a page with more eligible providers than the
// pass can take is judged little past what it takes. When the pass fills
// inside a page it resumes after the last provider it took; the page's later
// providers are read, and judged, again by the next pass.
func (self *passRunner) selectDue(ctx context.Context, cursor server.Id, now time.Time) (selection, error) {
	selected := selection{dueClientIds: []server.Id{}, cursor: cursor}
	modernRefreshBefore := now.Add(-self.settings.modernRefresh())
	otherRefreshBefore := now.Add(-self.settings.otherRefresh())
	// how many of ids (in client id order) are at or before last
	countThrough := func(ids []server.Id, last server.Id) int {
		count := 0
		for _, id := range ids {
			if 0 < id.Cmp(last) {
				break
			}
			count += 1
		}
		return count
	}
	for pages := 0; pages < self.settings.MaxPagesPerPass && len(selected.dueClientIds) < self.settings.Limit; pages += 1 {
		if err := ctx.Err(); err != nil {
			return selected, err
		}
		page, err := self.readPage(ctx, selected.cursor, self.settings.PageSize, modernRefreshBefore, otherRefreshBefore)
		if err != nil {
			return selected, err
		}
		pageClientIds := make([]server.Id, 0, len(page))
		dueClientIds := []server.Id{}
		for _, candidate := range page {
			pageClientIds = append(pageClientIds, candidate.ClientId)
			if candidate.Due {
				dueClientIds = append(dueClientIds, candidate.ClientId)
			}
		}
		for judged := 0; judged < len(dueClientIds); {
			room := self.settings.Limit - len(selected.dueClientIds)
			chunkEnd := min(len(dueClientIds), judged+max(2*room, minEligibilityChunk))
			eligibleClientIds, err := self.filterEligible(ctx, dueClientIds[judged:chunkEnd])
			if err != nil {
				return selected, err
			}
			if room < len(eligibleClientIds) || (room == len(eligibleClientIds) && chunkEnd < len(dueClientIds)) {
				// the pass fills inside this page
				taken := eligibleClientIds[:room]
				last := taken[len(taken)-1]
				selected.dueClientIds = append(selected.dueClientIds, taken...)
				selected.cursor = last
				selected.scanned += countThrough(pageClientIds, last)
				selected.due += countThrough(dueClientIds, last)
				selected.eligible += len(taken)
				return selected, nil
			}
			selected.dueClientIds = append(selected.dueClientIds, eligibleClientIds...)
			selected.eligible += len(eligibleClientIds)
			judged = chunkEnd
		}
		selected.scanned += len(page)
		selected.due += len(dueClientIds)
		if 0 < len(page) {
			selected.cursor = page[len(page)-1].ClientId
		}
		if len(page) < self.settings.PageSize {
			selected.endReached = true
			return selected, nil
		}
	}
	return selected, nil
}

// Where the sweep stands after a pass that started the first startedCount
// providers it selected, in order. A pass that could not start every one
// resumes after the last it started, so the rest are selected again; a pass
// that started all of them and reached the population's end completes the
// sweep, and the next may start min_sweep_interval_minutes after this one
// started.
func nextSweep(sweep Sweep, selected selection, startedCount int, minSweepInterval time.Duration) (next Sweep, completed bool) {
	if startedCount < len(selected.dueClientIds) {
		cursor := sweep.Cursor
		if 0 < startedCount {
			cursor = selected.dueClientIds[startedCount-1]
		}
		return Sweep{Cursor: cursor, StartedAt: sweep.StartedAt}, false
	}
	if selected.endReached {
		return Sweep{NextSweepAt: sweep.StartedAt.Add(minSweepInterval)}, true
	}
	return Sweep{Cursor: selected.cursor, StartedAt: sweep.StartedAt}, false
}

// The production page read: model.GetProviderCapabilityProbePage, with a
// datastore failure returned rather than raised.
func readProviderCapabilityProbePage(
	ctx context.Context,
	afterClientId server.Id,
	pageSize int,
	modernRefreshBefore time.Time,
	otherRefreshBefore time.Time,
) (page []model.ProviderCapabilityProbeCandidate, returnErr error) {
	server.HandleError(func() {
		page = model.GetProviderCapabilityProbePage(ctx, afterClientId, pageSize, modernRefreshBefore, otherRefreshBefore)
	}, func(err error) {
		page, returnErr = nil, err
	})
	return
}

// The production eligibility read, model.FilterProviderCapabilityProbeEligible,
// with a datastore failure returned rather than raised.
func filterProviderCapabilityProbeEligible(ctx context.Context, clientIds []server.Id) (eligibleClientIds []server.Id, returnErr error) {
	server.HandleError(func() {
		eligibleClientIds = model.FilterProviderCapabilityProbeEligible(ctx, clientIds)
	}, func(err error) {
		eligibleClientIds, returnErr = nil, err
	})
	return
}
