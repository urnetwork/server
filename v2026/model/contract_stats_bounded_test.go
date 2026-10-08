// Bounded open-set counts are tested against real SQL populations and owned
// cancellation barriers, never a wall-clock performance threshold.
package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

// Pin the collector's default to the small cap that has a direct runtime
// control; a larger exactness range needs its own production budget evidence.
func TestOpenContractStatsCollectorUsesVerifiedReadCap(t *testing.T) {
	query := openContractStatsQueryTest(func(_ context.Context, _ string, args ...any) (pgx.Rows, error) {
		if len(args) != 1 || args[0] != int64(1001) {
			t.Fatalf("collector query bound=%v, want the verified 1001-row sentinel", args)
		}
		return &openContractStatsRowsTest{remaining: 1, cancel: func() {}}, nil
	})
	if _, err := readOpenContractStats(t.Context(), query, openContractStatsLimit); err != nil {
		t.Fatal(err)
	}
}

// More than the cap must remain an explicit lower bound, including a zero
// extender count. A rare extender predicate cannot move the cap after its join.
func TestOpenContractStatsCapsLargePopulation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		for i := range 12 {
			id := testContractStatsContract(ctx, now.Add(time.Duration(i)*time.Second), false, nil)
			if i < 8 {
				testContractStatsExtenderParty(ctx, id, ContractPartySource, now)
			}
			testContractStatsContract(ctx, now, true, nil)
		}
		server.Db(ctx, func(conn server.PgConn) {
			snapshot, err := readOpenContractStats(ctx, conn, 3)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.OpenContracts != 4 || snapshot.OpenDisputes != 4 || snapshot.OpenContractsExact || snapshot.OpenDisputesExact {
				t.Fatalf("large population published open=%d/%t dispute=%d/%t; want sentinel4 and lower bounds", snapshot.OpenContracts, snapshot.OpenContractsExact, snapshot.OpenDisputes, snapshot.OpenDisputesExact)
			}
			if snapshot.OpenContractsWithExtender != 0 || snapshot.ObservedAt.IsZero() {
				t.Fatal("capped empty extender sample lost its lower-bound snapshot")
			}
		})
	})
}

// The cap begins at current arrivals rather than an old index prefix whose
// invisible versions can outlive a long snapshot. Selection order is the
// invariant; a local fixture must not infer performance from elapsed time.
func TestOpenContractStatsSamplesNewestRowsBeforeExtenderMembership(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		for i := range 7 {
			id := testContractStatsContract(ctx, now.Add(time.Duration(i)*time.Second), false, nil)
			if i < 4 {
				testContractStatsExtenderParty(ctx, id, ContractPartySource, now)
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			snapshot, err := readOpenContractStats(ctx, conn, 2)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.OpenContracts != 3 || snapshot.OpenContractsExact || snapshot.OpenContractsWithExtender != 0 {
				t.Fatalf("bounded sample open=%d exact=%t extender=%d; want three newest rows, capped and no extender", snapshot.OpenContracts, snapshot.OpenContractsExact, snapshot.OpenContractsWithExtender)
			}
			if snapshot.OpenDisputes != 0 || !snapshot.OpenDisputesExact || snapshot.ObservedAt.IsZero() {
				t.Fatal("newest-row sampling changed independent exactness or snapshot time")
			}
		})
	})
}

// A decoder can finish after cancellation; that row is not fresh authority.
type openContractStatsRowsTest struct {
	pgx.Rows
	remaining int
	closed    bool
	cancel    context.CancelFunc
	scanErr   error
	rowErr    error
}

func (self *openContractStatsRowsTest) Next() bool { self.remaining--; return self.remaining >= 0 }
func (self *openContractStatsRowsTest) Err() error { return self.rowErr }
func (self *openContractStatsRowsTest) Close()     { self.closed = true }
func (self *openContractStatsRowsTest) Scan(dest ...any) error {
	if self.scanErr != nil {
		return self.scanErr
	}
	*dest[0].(*int64), *dest[1].(*int64), *dest[2].(*int64) = 3, 2, 1
	*dest[3].(*time.Time) = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	self.cancel()
	return nil
}

// Missing, duplicate and failed rows must all erase partial evidence.
func TestOpenContractStatsRejectsIncompleteRows(t *testing.T) {
	for _, rows := range []*openContractStatsRowsTest{
		{remaining: 0},
		{remaining: 2},
		{remaining: 1, scanErr: errors.New("synthetic decode failure")},
		{remaining: 1, rowErr: errors.New("synthetic read failure")},
	} {
		rows.cancel = func() {}
		query := openContractStatsQueryTest(func(context.Context, string, ...any) (pgx.Rows, error) { return rows, nil })
		snapshot, err := readOpenContractStats(t.Context(), query, 3)
		if err == nil || !snapshot.ObservedAt.IsZero() || !rows.closed {
			t.Fatalf("partial row published: snapshot=%+v err=%v closed=%t", snapshot, err, rows.closed)
		}
	}
}

// The explicit decode hook forces late cancellation without timer races.
func TestOpenContractStatsRejectsCanceledDecodedSnapshot(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rows := &openContractStatsRowsTest{remaining: 1, cancel: cancel}
	query := openContractStatsQueryTest(func(context.Context, string, ...any) (pgx.Rows, error) { return rows, nil })
	snapshot, err := readOpenContractStats(ctx, query, 3)
	if !errors.Is(err, context.Canceled) || !snapshot.ObservedAt.IsZero() || !rows.closed {
		t.Fatalf("late cancellation snapshot=%+v err=%v closed=%t", snapshot, err, rows.closed)
	}
}

// Already canceled callers do not borrow or query a database connection.
func TestOpenContractStatsRejectsCanceledParentBeforeQuery(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	query := openContractStatsQueryTest(func(context.Context, string, ...any) (pgx.Rows, error) {
		t.Fatal("canceled request issued SQL")
		return nil, nil
	})
	_, err := readOpenContractStats(ctx, query, 3)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
}

// Full small populations retain exact counts, duplicate parties count once,
// and an independently capped dispute set does not invalidate exact open data.
func TestOpenContractStatsKeepsExactSmallPopulation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		server.Db(ctx, func(conn server.PgConn) {
			snapshot, err := readOpenContractStats(ctx, conn, 3)
			if err != nil || snapshot.OpenContracts != 0 || snapshot.OpenDisputes != 0 || !snapshot.OpenContractsExact || !snapshot.OpenDisputesExact {
				t.Fatalf("empty population is not exact: %+v err=%v", snapshot, err)
			}
		})
		first := testContractStatsContract(ctx, now, false, nil)
		testContractStatsContract(ctx, now.Add(time.Second), false, nil)
		testContractStatsContract(ctx, now.Add(2*time.Second), false, nil)
		testContractStatsExtenderParty(ctx, first, ContractPartySource, now)
		testContractStatsExtenderParty(ctx, first, ContractPartyDestination, now)
		settled := ContractOutcomeSettled
		closed := testContractStatsContract(ctx, now, false, &settled)
		testContractStatsExtenderParty(ctx, closed, ContractPartySource, now)
		testContractStatsContract(ctx, now, true, &settled)
		for range 4 {
			testContractStatsContract(ctx, now, true, nil)
		}
		server.Db(ctx, func(conn server.PgConn) {
			snapshot, err := readOpenContractStats(ctx, conn, 3)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.OpenContracts != 3 || snapshot.OpenContractsWithExtender != 1 || !snapshot.OpenContractsExact || snapshot.OpenDisputes != 4 || snapshot.OpenDisputesExact {
				t.Fatalf("small open/capped dispute semantics changed: %+v", snapshot)
			}
		})
	})
}

// One caller owns its fake query; entry into Query is the cancellation barrier.
type openContractStatsQueryTest func(context.Context, string, ...any) (pgx.Rows, error)

func (self openContractStatsQueryTest) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return self(ctx, sql, args...)
}

// Deadline presence is asserted synchronously; cancellation happens only after
// Query entry, so neither a short sleep nor scheduler luck proves the bound.
func TestOpenContractStatsBoundsSlowQuery(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{})
	finished := make(chan error, 1)
	query := openContractStatsQueryTest(func(ctx context.Context, _ string, _ ...any) (pgx.Rows, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > openContractStatsTimeout {
			return nil, errors.New("query lacks bounded deadline")
		}
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	go func() { _, err := readOpenContractStats(ctx, query, 3); finished <- err }()
	select {
	case <-entered:
		cancel()
		if err := <-finished; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel result=%v", err)
		}
	case err := <-finished:
		t.Fatalf("slow-query safety failed before barrier: %v", err)
	}
}
