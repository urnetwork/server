package model

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A fake grant table: payers by the end time of a grant they still hold escrow
// on. The fake drain records the payers in visit order.
type fakeStrandedGrants struct {
	ends     map[server.Id][]time.Time
	reads    int
	readErr  error
	drained  []server.Id
	failing  map[server.Id]bool
	closes   map[server.Id]int
	spending func()
}

func (self *fakeStrandedGrants) ops() strandedGrantCloseOps {
	return strandedGrantCloseOps{
		readPayers: func(ctx context.Context, endedAfter time.Time, endedBefore time.Time, afterNetworkId server.Id, limit int) ([]server.Id, error) {
			self.reads++
			if self.readErr != nil {
				return nil, self.readErr
			}
			payers := []server.Id{}
			for payer, ends := range self.ends {
				if payer.Cmp(afterNetworkId) <= 0 {
					continue
				}
				for _, end := range ends {
					if end.After(endedAfter) && !end.After(endedBefore) {
						payers = append(payers, payer)
						break
					}
				}
			}
			slices.SortFunc(payers, func(a server.Id, b server.Id) int { return a.Cmp(b) })
			if limit < len(payers) {
				payers = payers[:limit]
			}
			return payers, nil
		},
		drain: func(ctx context.Context, payerNetworkId server.Id, now time.Time, more func() bool, settings *PayerDueContractCloseSettings) (*PayerDueContractClose, error) {
			if self.spending != nil {
				self.spending()
			}
			if !more() {
				// the drain checks the budget before its first turn
				return &PayerDueContractClose{Due: 1, More: true}, nil
			}
			self.drained = append(self.drained, payerNetworkId)
			if self.failing[payerNetworkId] {
				return nil, errors.New("synthetic drain failure")
			}
			closed := self.closes[payerNetworkId]
			// a drained payer holds no more escrow on its ended grants
			delete(self.ends, payerNetworkId)
			return &PayerDueContractClose{Due: closed, Closed: closed}, nil
		},
	}
}

// Payer ids in network order.
func strandedGrantTestPayers(count int) []server.Id {
	payers := make([]server.Id, count)
	for i := range payers {
		payers[i] = server.NewId()
	}
	slices.SortFunc(payers, func(a server.Id, b server.Id) int { return a.Cmp(b) })
	return payers
}

func strandedGrantTestSettings() *StrandedGrantCloseSettings {
	settings := DefaultStrandedGrantCloseSettings()
	settings.PageSize = 2
	return settings
}

// The first run arms the backfill at the settled edge and walks it back to the
// lookback, newest end times first, draining each payer that still holds escrow
// on an ended grant once per grant it is found on. Grants that ended within
// Settle, or before the lookback, wait or are left to the other closers.
func TestStrandedGrantPassBackfillsNewestFirst(t *testing.T) {
	now := time.Date(2026, 10, 29, 14, 0, 0, 0, time.UTC)
	settings := strandedGrantTestSettings()
	payers := strandedGrantTestPayers(5)
	fake := &fakeStrandedGrants{
		ends: map[server.Id][]time.Time{
			// the proxy payer of 7830: daily grants that ended on 10-20 and 10-29
			payers[0]: {time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC), time.Date(2026, 10, 29, 1, 0, 0, 0, time.UTC)},
			// grants that ended at the 01:00 boundary two days ago
			payers[1]: {time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC)},
			payers[2]: {time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC)},
			// within Settle: its contracts may not all be due yet
			payers[3]: {now.Add(-30 * time.Minute)},
			// before the lookback
			payers[4]: {now.Add(-15 * 24 * time.Hour)},
		},
		closes: map[server.Id]int{payers[0]: 5334, payers[1]: 3, payers[2]: 1},
	}
	result := closeStrandedGrantContracts(t.Context(), &CloseStrandedGrantContractsArgs{}, now, func() bool { return true }, settings, fake.ops())

	if result.Error != "" || result.Exhausted {
		t.Fatal("the backfill did not finish", result)
	}
	if !slices.Equal(fake.drained, []server.Id{payers[0], payers[1], payers[2]}) {
		t.Fatal("the backfill did not drain the stranded payers newest first", fake.drained, payers)
	}
	if result.Payers != 3 || result.Closed != 5338 {
		t.Fatal("the run miscounted", result.Payers, result.Closed)
	}
	edge := now.Add(-settings.Settle)
	next := result.Next
	if !next.Fresh.Equal(edge) || next.FreshWindow != nil || next.BackfillWindow != nil ||
		!next.BackfillUpper.Equal(now.Add(-settings.Lookback)) || !next.BackfillFloor.Equal(now.Add(-settings.Lookback)) {
		t.Fatal("the backfill did not end at the lookback", next)
	}

	// An hour later the grant that ended within Settle has settled, and the
	// fresh walk finds it.
	later := now.Add(time.Hour)
	result = closeStrandedGrantContracts(t.Context(), &next, later, func() bool { return true }, settings, fake.ops())
	if !slices.Equal(fake.drained, []server.Id{payers[0], payers[1], payers[2], payers[3]}) || result.Error != "" {
		t.Fatal("the fresh walk did not drain the payer whose grant settled", fake.drained)
	}
	if !result.Next.Fresh.Equal(later.Add(-settings.Settle)) || result.Next.FreshWindow != nil {
		t.Fatal("the fresh walk did not reach the new edge", result.Next)
	}

	// Caught up, a run reads nothing.
	reads := fake.reads
	result = closeStrandedGrantContracts(t.Context(), &result.Next, later, func() bool { return true }, settings, fake.ops())
	if fake.reads != reads || result.Payers != 0 || result.Exhausted {
		t.Fatal("a caught-up run read or drained", fake.reads-reads, result)
	}
}

// A run whose budget ends keeps its window and payer position; the next run
// resumes after the last payer visited, never skipping one whose drain did not
// start. New end times go before the rest of the backfill.
func TestStrandedGrantPassResumesWhereTheBudgetEnded(t *testing.T) {
	now := time.Date(2026, 10, 29, 14, 0, 0, 0, time.UTC)
	settings := strandedGrantTestSettings()
	payers := strandedGrantTestPayers(5)
	boundary := time.Date(2026, 10, 29, 1, 0, 0, 0, time.UTC)
	fake := &fakeStrandedGrants{ends: map[server.Id][]time.Time{}, closes: map[server.Id]int{}}
	for _, payer := range payers[:4] {
		fake.ends[payer] = []time.Time{boundary}
	}

	// The budget allows two drains.
	budget := 2
	more := func() bool { return 0 < budget }
	fake.spending = func() { budget-- }
	result := closeStrandedGrantContracts(t.Context(), &CloseStrandedGrantContractsArgs{}, now, func() bool {
		return more()
	}, settings, fake.ops())
	if !result.Exhausted || result.Payers != 1 || !slices.Equal(fake.drained, []server.Id{payers[0]}) {
		t.Fatal("the run did not stop when its budget ended", result, fake.drained)
	}
	window := result.Next.BackfillWindow
	if window == nil || window.After != payers[0] || !window.Lower.Before(boundary) || window.Upper.Before(boundary) {
		t.Fatal("the run did not keep its window and position", result.Next)
	}

	// Meanwhile a grant ended and settled: the next run drains its payer first,
	// then resumes the backfill after payers[0] with the same window bounds.
	settledEnd := now.Add(-settings.Settle).Add(10 * time.Minute)
	fake.ends[payers[4]] = []time.Time{settledEnd}
	later := now.Add(15 * time.Minute)
	budget = 100
	result = closeStrandedGrantContracts(t.Context(), &result.Next, later, more, settings, fake.ops())
	if result.Exhausted || result.Error != "" {
		t.Fatal("the second run did not finish", result)
	}
	if !slices.Equal(fake.drained, []server.Id{payers[0], payers[4], payers[1], payers[2], payers[3]}) {
		t.Fatal("the second run did not drain the new grant's payer first and resume after the last visited", fake.drained, payers)
	}
}

// A failed drain passes its payer, and a failed read stops the run with its
// position kept.
func TestStrandedGrantPassPassesFailedDrainsAndKeepsPositionOnReadErrors(t *testing.T) {
	now := time.Date(2026, 10, 29, 14, 0, 0, 0, time.UTC)
	settings := strandedGrantTestSettings()
	payers := strandedGrantTestPayers(2)
	boundary := time.Date(2026, 10, 29, 1, 0, 0, 0, time.UTC)
	fake := &fakeStrandedGrants{
		ends:    map[server.Id][]time.Time{payers[0]: {boundary}, payers[1]: {boundary}},
		failing: map[server.Id]bool{payers[0]: true},
		closes:  map[server.Id]int{payers[1]: 2},
	}
	result := closeStrandedGrantContracts(t.Context(), &CloseStrandedGrantContractsArgs{}, now, func() bool { return true }, settings, fake.ops())
	if result.Failed != 1 || result.Payers != 1 || result.Closed != 2 || result.Error != "" ||
		!slices.Equal(fake.drained, []server.Id{payers[0], payers[1]}) {
		t.Fatal("a failed drain held the pass", result, fake.drained)
	}

	fake.readErr = errors.New("synthetic read timeout")
	args := CloseStrandedGrantContractsArgs{}
	result = closeStrandedGrantContracts(t.Context(), &args, now, func() bool { return true }, settings, fake.ops())
	if result.Error != "synthetic read timeout" || result.Next.BackfillWindow == nil ||
		!result.Next.BackfillWindow.Upper.Equal(now.Add(-settings.Settle)) || result.Next.BackfillWindow.After != (server.Id{}) {
		t.Fatal("a failed read did not stop the run at its position", result)
	}
}
