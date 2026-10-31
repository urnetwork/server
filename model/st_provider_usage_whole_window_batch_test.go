// A whole-window batch hands each row's check to the in-order adder exactly,
// and a panic on either side reaches the reader instead of hanging the read.
package model

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func TestStEpochUsageBatchAddsCheckedRowsInOrder(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	batch := newStEpochUsageBatch()
	for range 5 {
		batch.rows = append(batch.rows, stEpochUsageRow{contractId: server.NewId(), closedAt: &at})
	}
	refused := errors.New("refused")
	batch.Check(func(row *stEpochUsageRow) (*contractUsageSnapshot, error) {
		if row == &batch.rows[3] {
			return nil, refused
		}
		return &contractUsageSnapshot{Version: 1}, nil
	})
	select {
	case <-batch.checked:
	default:
		t.Fatal("a checked batch did not signal its adder")
	}
	added := []server.Id{}
	result := batch.Add(func(row *stEpochUsageRow, snapshot *contractUsageSnapshot, originalReports []byte) error {
		if snapshot == nil || originalReports != nil {
			t.Fatal("a row was added without its own check")
		}
		added = append(added, row.contractId)
		return nil
	})
	if !errors.Is(result.err, refused) || result.panicked != nil || len(added) != 3 || added[0] != batch.rows[0].contractId || added[2] != batch.rows[2].contractId {
		t.Fatalf("rows after a refusal were added, or out of order: %+v %d", result, len(added))
	}
}

func TestStEpochUsageBatchPanicsReachTheReader(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	checkPanic := newStEpochUsageBatch()
	checkPanic.rows = append(checkPanic.rows, stEpochUsageRow{contractId: server.NewId(), closedAt: &at})
	checkPanic.Check(func(*stEpochUsageRow) (*contractUsageSnapshot, error) { panic("check") })
	<-checkPanic.checked
	if result := checkPanic.Add(func(*stEpochUsageRow, *contractUsageSnapshot, []byte) error {
		t.Fatal("a row whose check panicked was added")
		return nil
	}); result.panicked != "check" {
		t.Fatalf("a check panic was lost: %+v", result)
	}

	addPanic := newStEpochUsageBatch()
	addPanic.rows = append(addPanic.rows, stEpochUsageRow{contractId: server.NewId(), closedAt: &at})
	addPanic.Check(func(*stEpochUsageRow) (*contractUsageSnapshot, error) { return &contractUsageSnapshot{Version: 1}, nil })
	if result := addPanic.Add(func(*stEpochUsageRow, *contractUsageSnapshot, []byte) error { panic("add") }); result.panicked != "add" || result.err != nil {
		t.Fatalf("an add panic was lost: %+v", result)
	}
}

// Synthetic rows numbered in read order; the number rides in the contract id.
type stEpochUsageTestRows struct {
	count int
	at    time.Time
	read  int
	// next fails at this row, when set
	failAt  int
	failErr error
}

func (self *stEpochUsageTestRows) next(row *stEpochUsageRow) (bool, error) {
	if self.failErr != nil && self.read == self.failAt {
		return false, self.failErr
	}
	if self.read == self.count {
		return false, nil
	}
	var id server.Id
	id[0], id[1], id[2] = byte(self.read>>16), byte(self.read>>8), byte(self.read)
	*row = stEpochUsageRow{contractId: id, closedAt: &self.at}
	self.read++
	return true, nil
}

func stEpochUsageTestPosition(row *stEpochUsageRow) int {
	return int(row.contractId[0])<<16 | int(row.contractId[1])<<8 | int(row.contractId[2])
}

// Every row is added exactly once, in read order, including a partial last batch.
func TestStEpochUsageRowsAddInReadOrder(t *testing.T) {
	source := &stEpochUsageTestRows{count: 3*stEpochUsageBatchRows + 17, at: time.Unix(1_700_000_000, 0).UTC()}
	added := []int{}
	err := addStEpochUsageRowsInOrder(4, func() { t.Error("a complete read was canceled") }, source.next,
		func(*stEpochUsageRow) (*contractUsageSnapshot, error) { return &contractUsageSnapshot{Version: 1}, nil },
		func(row *stEpochUsageRow, snapshot *contractUsageSnapshot, originalReports []byte) error {
			added = append(added, stEpochUsageTestPosition(row))
			return nil
		})
	if err != nil || len(added) != source.count {
		t.Fatalf("added %d of %d rows: %v", len(added), source.count, err)
	}
	for i, position := range added {
		if position != i {
			t.Fatalf("row %d added at %d", position, i)
		}
	}
}

// A later batch's refusal is checked first, while the earlier refusal's check
// is still running. The read returns the earlier one, adds no row after it,
// and cancels the source.
func TestStEpochUsageRowsFirstRefusalInReadOrder(t *testing.T) {
	early, late := 10, 2*stEpochUsageBatchRows+10
	earlyErr, lateErr := errors.New("early refusal"), errors.New("late refusal")
	lateChecked := make(chan struct{})
	source := &stEpochUsageTestRows{count: 5 * stEpochUsageBatchRows, at: time.Unix(1_700_000_000, 0).UTC()}
	var canceled atomic.Bool
	added := 0
	err := addStEpochUsageRowsInOrder(4, func() { canceled.Store(true) }, source.next,
		func(row *stEpochUsageRow) (*contractUsageSnapshot, error) {
			switch stEpochUsageTestPosition(row) {
			case early:
				select {
				case <-lateChecked:
				case <-time.After(10 * time.Second):
					t.Error("the later refusal was not checked while the earlier one was")
				}
				return nil, earlyErr
			case late:
				close(lateChecked)
				return nil, lateErr
			}
			return &contractUsageSnapshot{Version: 1}, nil
		},
		func(*stEpochUsageRow, *contractUsageSnapshot, []byte) error {
			added++
			return nil
		})
	if err != earlyErr || added != early || !canceled.Load() {
		t.Fatalf("refusal %v after %d rows (canceled %t), want %v after %d", err, added, canceled.Load(), earlyErr, early)
	}
}

// A refusal read before the source fails is the result; rows the source never
// returned cannot refuse the read.
func TestStEpochUsageRowsRefusalAndReadErrorOrder(t *testing.T) {
	readErr, refusal := errors.New("read failed"), errors.New("refused")
	for _, test := range []struct {
		refuseAt int
		want     error
	}{{refuseAt: 100, want: refusal}, {refuseAt: 3000, want: readErr}} {
		source := &stEpochUsageTestRows{count: 4 * stEpochUsageBatchRows, at: time.Unix(1_700_000_000, 0).UTC(), failAt: 2000, failErr: readErr}
		var canceled atomic.Bool
		err := addStEpochUsageRowsInOrder(4, func() { canceled.Store(true) }, source.next,
			func(row *stEpochUsageRow) (*contractUsageSnapshot, error) {
				if stEpochUsageTestPosition(row) == test.refuseAt {
					return nil, refusal
				}
				return &contractUsageSnapshot{Version: 1}, nil
			},
			func(*stEpochUsageRow, *contractUsageSnapshot, []byte) error { return nil })
		if err != test.want || !canceled.Load() || 2000 < source.read {
			t.Fatalf("refusal at %d: %v (canceled %t, %d read), want %v", test.refuseAt, err, canceled.Load(), source.read, test.want)
		}
	}
}

// A panicking check is raised on the reader's goroutine after every worker
// has stopped.
func TestStEpochUsageRowsCheckPanicReachesReader(t *testing.T) {
	source := &stEpochUsageTestRows{count: 3 * stEpochUsageBatchRows, at: time.Unix(1_700_000_000, 0).UTC()}
	defer func() {
		if r := recover(); r != "check" {
			t.Fatalf("the check panic was not raised to the reader: %v", r)
		}
	}()
	addStEpochUsageRowsInOrder(4, func() {}, source.next,
		func(row *stEpochUsageRow) (*contractUsageSnapshot, error) {
			if stEpochUsageTestPosition(row) == 1500 {
				panic("check")
			}
			return &contractUsageSnapshot{Version: 1}, nil
		},
		func(*stEpochUsageRow, *contractUsageSnapshot, []byte) error { return nil })
	t.Fatal("a panicking check returned")
}
