// The sweep's selection, its place between passes, and its cadence.
package capprobe

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The sweep's selection and its place: which providers a pass selects, where
// the next pass resumes, when a sweep ends and the next may start.

// One pass's selection over store at now from cursor.
func selectFake(t *testing.T, settings Settings, store *fakeStore, cursor server.Id) selection {
	t.Helper()
	runner := newFakeRunner(t, settings, store, newFakeFleet())
	selected, err := runner.selectDue(context.Background(), cursor, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return selected
}

// The limit cuts a page short: the pass takes the first eligible due
// providers, resumes after the last one it took, and counts only the
// providers up to it, which the next pass does not read again.
func TestSelectionStopsAtTheLimit(t *testing.T) {
	store := newFakeStore(6)
	ids := store.clientIds
	store.ineligible[ids[1]] = true
	selected := selectFake(t, fakeSettings(1, 3, 6, 5), store, server.Id{})
	connect.AssertEqual(t, selected.dueClientIds, []server.Id{ids[0], ids[2], ids[3]})
	connect.AssertEqual(t, selected.cursor, ids[3])
	connect.AssertEqual(t, selected.endReached, false)
	connect.AssertEqual(t, []int{selected.scanned, selected.due, selected.eligible}, []int{4, 4, 3})
	resumed := selectFake(t, fakeSettings(1, 3, 6, 5), store, selected.cursor)
	connect.AssertEqual(t, resumed.dueClientIds, ids[4:])
	connect.AssertEqual(t, resumed.endReached, true)
}

// A page with far more due providers than the pass has room for is judged
// for eligibility in bounded chunks: twice the room, at least the chunk
// floor, and the next chunk only when the first left room.
func TestSelectionJudgesEligibilityInBoundedChunks(t *testing.T) {
	store := newFakeStore(200)
	ids := store.clientIds
	selected := selectFake(t, fakeSettings(2, 2, 200, 1), store, server.Id{})
	connect.AssertEqual(t, selected.dueClientIds, ids[:2])
	connect.AssertEqual(t, len(store.filterCalls), 1)
	connect.AssertEqual(t, len(store.filterCalls[0]), minEligibilityChunk)

	// a first chunk without eligible providers leads to the next
	for _, clientId := range ids[2 : 2+minEligibilityChunk] {
		store.ineligible[clientId] = true
	}
	store.filterCalls = nil
	selected = selectFake(t, fakeSettings(2, 2, 200, 1), store, ids[1])
	connect.AssertEqual(t, selected.dueClientIds, ids[2+minEligibilityChunk:4+minEligibilityChunk])
	connect.AssertEqual(t, len(store.filterCalls), 2)
	connect.AssertEqual(t, selected.cursor, ids[3+minEligibilityChunk])
}

// Pages continue until the population ends, which a short page shows. Only
// the due providers are asked about eligibility, and only eligible ones are
// selected; the cursor is the end of the last page.
func TestSelectionReadsPagesToThePopulationEnd(t *testing.T) {
	store := newFakeStore(6)
	ids := store.clientIds
	store.notPublic[ids[0]] = true
	store.notPublic[ids[3]] = true
	store.ineligible[ids[1]] = true
	store.ineligible[ids[5]] = true
	selected := selectFake(t, fakeSettings(1, 10, 3, 5), store, server.Id{})
	connect.AssertEqual(t, selected.dueClientIds, []server.Id{ids[2], ids[4]})
	connect.AssertEqual(t, selected.cursor, ids[5])
	connect.AssertEqual(t, selected.endReached, true)
	connect.AssertEqual(t, []int{selected.scanned, selected.due, selected.eligible}, []int{6, 4, 2})
	connect.AssertEqual(t, store.filterCalls, [][]server.Id{{ids[1], ids[2]}, {ids[4], ids[5]}})
	connect.AssertEqual(t, store.pageReads, 3)
}

// One pass reads at most max_pages_per_pass pages, and resumes after them.
func TestSelectionReadsAtMostItsPages(t *testing.T) {
	store := newFakeStore(6)
	ids := store.clientIds
	for _, clientId := range ids {
		store.ineligible[clientId] = true
	}
	selected := selectFake(t, fakeSettings(1, 10, 2, 2), store, server.Id{})
	connect.AssertEqual(t, len(selected.dueClientIds), 0)
	connect.AssertEqual(t, selected.cursor, ids[3])
	connect.AssertEqual(t, selected.endReached, false)
	connect.AssertEqual(t, store.pageReads, 2)
	// the next pass reads the last full page and the empty one after it
	resumed := selectFake(t, fakeSettings(1, 10, 2, 2), store, selected.cursor)
	connect.AssertEqual(t, resumed.cursor, ids[5])
	connect.AssertEqual(t, resumed.endReached, true)
	connect.AssertEqual(t, store.pageReads, 4)
}

// An empty population ends the sweep at once, and a failed read fails the
// selection.
func TestSelectionOfAnEmptyPopulationAndAFailedRead(t *testing.T) {
	store := newFakeStore(0)
	selected := selectFake(t, fakeSettings(1, 10, 2, 2), store, server.Id{})
	connect.AssertEqual(t, selected.endReached, true)
	connect.AssertEqual(t, selected.cursor, server.Id{})
	store.readErr = errors.New("synthetic read failure")
	runner := newFakeRunner(t, fakeSettings(1, 10, 2, 2), store, newFakeFleet())
	if _, err := runner.selectDue(context.Background(), server.Id{}, time.Now()); err == nil {
		t.Fatal("a failed page read selected")
	}
}

// Due follows the evidence's age: none at once, a modern reading after the
// modern refresh, any other after the other refresh.
func TestSelectionRefreshesEvidenceByItsAge(t *testing.T) {
	now := time.Now()
	store := newFakeStore(4)
	ids := store.clientIds
	settings := fakeSettings(1, 10, 10, 1)
	settings.ModernRefreshHours = 72
	settings.OtherRefreshHours = 24
	modern := model.ProviderModernEvidence{E2eNegotiated: true, PostQuantum: true, WindowAdvertised: true}
	legacy := model.ProviderModernEvidence{}
	modern.ObservedAt = now.Add(-71 * time.Hour)
	store.evidence[ids[0]] = modern
	modern.ObservedAt = now.Add(-73 * time.Hour)
	store.evidence[ids[1]] = modern
	legacy.ObservedAt = now.Add(-25 * time.Hour)
	store.evidence[ids[2]] = legacy
	runner := newFakeRunner(t, settings, store, newFakeFleet())
	selected, err := runner.selectDue(context.Background(), server.Id{}, now)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, selected.dueClientIds, []server.Id{ids[1], ids[2], ids[3]})
}

// Where the sweep stands after a pass: a pass that started some of its
// providers resumes after the last it started, one that started none where
// it began; one that started all of them resumes after its selection, and at
// the population's end completes the sweep, the next starting the minimum
// interval after this one started.
func TestNextSweep(t *testing.T) {
	ids := newFakeStore(4).clientIds
	startedAt := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	sweep := Sweep{Cursor: ids[0], StartedAt: startedAt}
	selected := selection{dueClientIds: []server.Id{ids[1], ids[2]}, cursor: ids[3], endReached: true}
	for _, c := range []struct {
		name      string
		started   int
		next      Sweep
		completed bool
	}{
		{name: "none started", started: 0, next: Sweep{Cursor: ids[0], StartedAt: startedAt}},
		{name: "one started", started: 1, next: Sweep{Cursor: ids[1], StartedAt: startedAt}},
		{name: "all started at the end", started: 2, next: Sweep{NextSweepAt: startedAt.Add(6 * time.Hour)}, completed: true},
	} {
		next, completed := nextSweep(sweep, selected, c.started, 6*time.Hour)
		if !reflect.DeepEqual(next, c.next) || completed != c.completed {
			t.Errorf("%s: next %+v completed %t, want %+v %t", c.name, next, completed, c.next, c.completed)
		}
	}
	selected.endReached = false
	next, completed := nextSweep(sweep, selected, 2, 6*time.Hour)
	connect.AssertEqual(t, next, Sweep{Cursor: ids[3], StartedAt: startedAt})
	connect.AssertEqual(t, completed, false)
}

// Passes carry the sweep through the population: every due, eligible
// provider is measured once, in order, and recorded when it answers; the
// sweep completes at the end, the next pass waits for the minimum interval,
// and the next sweep measures again only what gave no evidence or has aged.
func TestPassesSweepThePopulationOnceAnInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newFakeStore(7)
		ids := store.clientIds
		fleet := newFakeFleet()
		store.notPublic[ids[1]] = true
		store.ineligible[ids[4]] = true
		for _, clientId := range ids {
			fleet.providers[clientId] = fakeModernProvider()
		}
		// one provider never answers, so it gives no evidence
		fleet.providers[ids[5]] = fakeSilentProvider()
		settings := fakeSettings(2, 2, 3, 1)
		runner := newFakeRunner(t, settings, store, fleet)

		sweep := Sweep{}
		var results []*PassResult
		for range 8 {
			next, result, err := runner.run(t.Context(), sweep)
			if err != nil {
				t.Fatal(err)
			}
			results = append(results, result)
			sweep = next
			if result.SweepCompleted {
				break
			}
		}
		if !results[len(results)-1].SweepCompleted {
			t.Fatalf("the sweep did not complete: %+v", results)
		}
		// a pass's providers open concurrently, so compare in id order
		connect.AssertEqual(t, sortedIds(fleet.openedProviderIds()), []server.Id{ids[0], ids[2], ids[3], ids[5], ids[6]})
		connect.AssertEqual(t, sortedIds(store.recordedIds()), []server.Id{ids[0], ids[2], ids[3], ids[6]})
		connect.AssertEqual(t, len(results), 3)
		connect.AssertEqual(t, sweep.Cursor, server.Id{})
		if sweep.NextSweepAt.IsZero() {
			t.Fatal("a completed sweep set no next start")
		}

		// the next sweep waits its interval, reading nothing
		reads := store.pageReads
		next, result, err := runner.run(t.Context(), sweep)
		if err != nil || !result.Waiting || !reflect.DeepEqual(next, sweep) || store.pageReads != reads {
			t.Fatalf("a pass before the next sweep's start ran: %+v %+v %v", next, result, err)
		}

		// at its start, the next sweep measures only the provider that gave no evidence
		time.Sleep(sweep.NextSweepAt.Sub(time.Now()))
		for range 8 {
			next, result, err := runner.run(t.Context(), sweep)
			if err != nil {
				t.Fatal(err)
			}
			sweep = next
			if result.SweepCompleted {
				break
			}
		}
		connect.AssertEqual(t, sortedIds(fleet.openedProviderIds()), []server.Id{ids[0], ids[2], ids[3], ids[5], ids[5], ids[6]})
		connect.AssertEqual(t, len(store.recordedIds()), 4)
	})
}

// ids in client id order.
func sortedIds(ids []server.Id) []server.Id {
	sorted := slices.Clone(ids)
	slices.SortFunc(sorted, func(a server.Id, b server.Id) int { return a.Cmp(b) })
	return sorted
}
