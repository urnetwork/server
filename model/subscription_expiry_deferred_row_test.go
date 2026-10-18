// A completed page defers operationally failed rows and commits a started prefix.
package model

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Only the page's own receipt, issued under a live parent, may carry a context
// stop. The production row text was "timeout: context deadline exceeded" from a
// bounded connection check; the same cause outside a receipt stays rejected.
func TestExpiryRowReceiptAcceptsOnlyRowLocalStops(t *testing.T) {
	id := expiryFairTestId(17)
	productionShape := errors.Join(fmt.Errorf("timeout: %w", context.DeadlineExceeded), &forceCloseNonfinalError{})
	receipt := &forceCloseRowError{contractId: id, index: 17, cause: productionShape}
	if receipt.Error() != fmt.Sprintf("force close contract %s at index 17: %s", id, productionShape.Error()) ||
		!errors.Is(receipt, context.DeadlineExceeded) {
		t.Fatal("the row receipt changed the stored diagnostic or hid its typed cause")
	}
	accepted := []error{
		receipt,
		&forceCloseRowError{contractId: id, cause: errors.Join(server.DbContextDoneError, context.DeadlineExceeded)},
		&forceCloseRowError{contractId: id, cause: errors.Join(context.Canceled, &forceCloseNonfinalError{})},
		&forceCloseRowError{contractId: id, cause: errors.Join(errTransferBalanceOwnershipBusy, &forceCloseNonfinalError{})},
	}
	for _, member := range accepted {
		failure := &ForceCloseVisitError{cause: server.NewErrorCauseBatch([]error{member}), complete: true}
		if !failure.CanCheckpoint() || !forceClosePageCanAdvance(failure) {
			t.Fatal("a completed row's own operational failure pinned its page", member)
		}
	}
	rejected := []error{
		fmt.Errorf("force close contract %s at index 17: %w", id, productionShape),
		fmt.Errorf("synthetic outer: %w", receipt),
		&forceCloseRowError{contractId: id, cause: &expiryVisitRuntimePanic{}},
		&forceCloseRowError{contractId: id, cause: server.NewErrorCauseBatch([]error{context.DeadlineExceeded})},
		&forceCloseRowError{contractId: id},
		(*forceCloseRowError)(nil),
	}
	for _, member := range rejected {
		failure := &ForceCloseVisitError{cause: server.NewErrorCauseBatch([]error{member}), complete: true}
		if failure.CanCheckpoint() || forceClosePageCanAdvance(failure) {
			t.Fatal("an unattested stop, interruption or malformed receipt acquired a checkpoint", member)
		}
	}
	deferred := []server.Id{expiryFairTestId(1), expiryFairTestId(2)}
	failure := &ForceCloseVisitError{cause: receipt, complete: true, progressed: true, deferredContractIds: deferred}
	returned := failure.DeferredContractIds()
	returned[0] = expiryFairTestId(3)
	if !failure.Progressed() || !slices.Equal(failure.DeferredContractIds(), deferred) {
		t.Fatal("the deferred receipt lost its progress or exposed mutable identities")
	}
}

// Creates aged free contracts in order, with a NULL deadline past its fallback.
func newForceCloseDeferredRows(t testing.TB, ctx context.Context, count int) []server.Id {
	t.Helper()
	network, source, destination := server.NewId(), server.NewId(), server.NewId()
	addContractPayoutTestClients(ctx, map[server.Id]server.Id{source: network, destination: network})
	created := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	ids := make([]server.Id, count)
	for index := range ids {
		id, err := CreateContractNoEscrow(ctx, network, source, network, destination, 100)
		if err != nil {
			t.Fatal(err)
		}
		ids[index] = id
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,expiration_time=NULL WHERE contract_id=$1`,
				id, created.Add(time.Duration(index)*time.Second)))
		})
	}
	return ids
}

// Reads the terminal flag and retained expiry proof of each row.
func readForceCloseDeferredRows(t testing.TB, ctx context.Context, ids []server.Id) (terminal []bool, proof []bool) {
	t.Helper()
	terminal, proof = make([]bool, len(ids)), make([]bool, len(ids))
	server.Db(ctx, func(conn server.PgConn) {
		for index, id := range ids {
			server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NOT NULL, usage_unverified FROM transfer_contract WHERE contract_id=$1`, id).
				Scan(&terminal[index], &proof[index]))
		}
	})
	return
}

// A row-local deadline in one continuation leaves only that row open. The
// completed page attests the visit, records the row and advances its cursor.
func TestForceClosePageDefersRowLocalDeadline(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		ids := newForceCloseDeferredRows(t, ctx, 3)
		victim := ids[1]
		callCtx := context.WithValue(ctx, forceCloseContinuationContextKey{}, func(parent context.Context, id server.Id) context.Context {
			if id != victim {
				return parent
			}
			// Expired before the first statement: deterministic, and the
			// parent page context stays live throughout.
			child, stop := context.WithDeadline(parent, time.Unix(1, 0))
			stop()
			return child
		})
		count, cursor, _, err := forceCloseOpenContractIdsPage(callCtx, server.NowUtc(), len(ids), 1, 0, 0, nil)
		visited, ok := err.(*ForceCloseVisitError)
		if !ok || !visited.CanCheckpoint() || !forceClosePageCanAdvance(err) || count != 3 || visited.AttemptedCloseCount() != count {
			t.Fatalf("a row-local deadline pinned the completed page: count=%d error=%v", count, err)
		}
		if cursor == nil || cursor.Open == nil || cursor.Open.ContractId != ids[2] || !errors.Is(err, context.DeadlineExceeded) ||
			!strings.Contains(err.Error(), fmt.Sprintf("force close contract %s at index 1: ", victim)) {
			t.Fatal("the deferred row lost its cursor position, typed cause or stored diagnostic", cursor, err)
		}
		if !visited.Progressed() || !slices.Equal(visited.DeferredContractIds(), []server.Id{victim}) {
			t.Fatal("the page did not record exactly the deferred row among progressing siblings", visited.DeferredContractIds())
		}
		terminal, proof := readForceCloseDeferredRows(t, ctx, ids)
		if !terminal[0] || terminal[1] || !terminal[2] || !proof[1] {
			t.Fatal("siblings did not close, or the deferred row lost custody of its retained proof", terminal, proof)
		}
	})
}

// A parent stop is not a row's own failure: the interrupted page issues no
// receipt, keeps its prior cursor and never defers the unvisited sibling.
func TestForceClosePageParentStopGrantsNoRowReceipt(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		ids := newForceCloseDeferredRows(t, ctx, 3)
		parent, stopParent := context.WithCancel(ctx)
		defer stopParent()
		callCtx := context.WithValue(parent, forceCloseContinuationContextKey{}, func(parent context.Context, id server.Id) context.Context {
			if id == ids[1] {
				stopParent()
			}
			return parent
		})
		_, cursor, _, err := forceCloseOpenContractIdsPage(callCtx, server.NowUtc(), len(ids), 1, 0, 0, nil)
		if err == nil || forceClosePageCanAdvance(err) || cursor == nil || cursor.Open == nil {
			t.Fatal("parent cancellation acquired a completed-visit checkpoint", cursor, err)
		}
		var visited *ForceCloseVisitError
		var receipt *forceCloseRowError
		if errors.As(err, &visited) || errors.As(err, &receipt) {
			t.Fatal("an interrupted page attested a row receipt", err)
		}
		if terminal, _ := readForceCloseDeferredRows(t, ctx, ids); !terminal[0] || terminal[2] {
			t.Fatal("parent cancellation changed rows outside its started work", terminal)
		}
	})
}

// Once the dispatch limit closes, no later row starts. One full wave always
// starts; the cursor passes unselected rows only up to the first unstarted
// selected row, so the next page resumes exactly there and skips nothing.
func TestForceClosePageCommitsStartedPrefixWhenDispatchCloses(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		// Each case admits this many rows after the first wave. The raw order
		// is e0, e1, active, e2, e3, e4; cursorAt is the last raw row passed.
		for _, c := range []struct {
			allowed  int
			cursorAt int
		}{{allowed: 0, cursorAt: 0}, {allowed: 1, cursorAt: 2}, {allowed: 2, cursorAt: 3}, {allowed: 8, cursorAt: 5}} {
			eligible := newForceCloseDeferredRows(t, ctx, 5)
			// A future deadline and a fresh checkpoint withdraw this active
			// row from selection while it still sits inside the raw page.
			active := server.NewId()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
					(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,
					transfer_byte_count,usage_origin_is_source,create_time,expiration_time)
					SELECT $1::uuid,source_network_id,source_id,destination_network_id,destination_id,source_network_id,
					100,true,create_time+interval '500 milliseconds',$2 FROM transfer_contract WHERE contract_id=$3`,
					active, server.NowUtc().Add(time.Hour), eligible[1]))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
					VALUES ($1,'source',0,$2,true),($1,'destination',0,$2,true)`, active, server.NowUtc()))
			})
			raw := []server.Id{eligible[0], eligible[1], active, eligible[2], eligible[3], eligible[4]}
			checks := 0
			dispatch := func() bool {
				checks++
				return checks <= c.allowed
			}
			cutoff := server.NowUtc().Add(-time.Minute)
			count, cursor, _, err := forceCloseOpenContractIdsPageDispatched(ctx, cutoff, len(raw), 1, 0, 0, nil, dispatch)
			started := min(c.allowed+1, len(eligible))
			if err != nil || count != int64(started) || cursor == nil || cursor.Open == nil || cursor.OpenDone ||
				cursor.Open.ContractId != raw[c.cursorAt] {
				t.Fatalf("dispatch limit lost its started prefix: allowed=%d count=%d cursor=%+v error=%v", c.allowed, count, cursor, err)
			}
			terminal, _ := readForceCloseDeferredRows(t, ctx, eligible)
			for index, closed := range terminal {
				if closed != (index < started) {
					t.Fatal("a row outside the started prefix changed, or a started row did not close", c.allowed, terminal)
				}
			}
			rest, next, _, err := forceCloseOpenContractIdsPageDispatched(ctx, cutoff, len(raw), 1, 0, 0, cursor, nil)
			if err != nil || rest != int64(len(eligible)-started) || next != nil {
				t.Fatalf("the next page skipped or repeated rows: allowed=%d rest=%d next=%+v error=%v", c.allowed, rest, next, err)
			}
			terminal, _ = readForceCloseDeferredRows(t, ctx, append(slices.Clone(eligible), active))
			if slices.Contains(terminal[:len(eligible)], false) || terminal[len(eligible)] {
				t.Fatal("resuming the prefix did not retire every eligible row exactly once", c.allowed, terminal)
			}
			// The next case starts from the head again, so remove this open row.
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM contract_close WHERE contract_id=$1`, active))
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, active))
			})
		}
	})
}
