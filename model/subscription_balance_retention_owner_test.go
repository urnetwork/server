package model

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urnetwork/server"
)

type retentionOwnerContextKey struct{}
type retentionOwnerQueryKey struct{}

type retentionOwnerTrace struct {
	mu                                                                sync.Mutex
	cancel                                                            context.CancelFunc
	held                                                              map[*pgx.Conn]bool
	transactions                                                      map[*pgx.Conn]string
	readOnly                                                          map[*pgx.Conn]bool
	discoveryConn                                                     *pgx.Conn
	firstCapacity                                                     int32
	acquires, releases                                                int
	discoveryStarts, discoveryEnds, deletes, commits                  int
	deleteCommits, censusCommits, unknownCommits, deletedRows         int
	censusStarts, censusEnds                                          int
	deleteCandidates, censusCandidates                                []server.Id
	invalidTransaction, censusAfterCommit, censusUncancelled          bool
	overlapped, discoveryOpen, discoveryReleased, deleteBeforeRelease bool
	cancelAfterDiscovery, cancelAfterCommit                           bool
}

func (o *retentionOwnerTrace) TraceAcquireStart(ctx context.Context, pool *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	if ctx.Value(retentionOwnerContextKey{}) != o {
		return ctx
	}
	o.mu.Lock()
	if o.firstCapacity == 0 {
		o.firstCapacity = pool.Stat().MaxConns()
	}
	var cancel context.CancelFunc
	if len(o.held) != 0 {
		o.overlapped = true
		cancel = o.cancel
	}
	o.mu.Unlock()
	// Refuse at the actual acquisition boundary, before a one-slot pool can
	// deadlock. The causal failure does not depend on waiting out a timeout.
	if cancel != nil {
		cancel()
	}
	return ctx
}

func (o *retentionOwnerTrace) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	if ctx.Value(retentionOwnerContextKey{}) != o || data.Err != nil || data.Conn == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.acquires++
	o.held[data.Conn] = true
}

func (o *retentionOwnerTrace) TraceRelease(_ *pgxpool.Pool, data pgxpool.TraceReleaseData) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.held[data.Conn] {
		o.releases++
		delete(o.held, data.Conn)
		if data.Conn == o.discoveryConn && !o.discoveryOpen {
			o.discoveryReleased = true
		}
	}
}

func (o *retentionOwnerTrace) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if ctx.Value(retentionOwnerContextKey{}) != o {
		return ctx
	}
	phase := "other"
	statement := strings.ToLower(strings.Join(strings.Fields(data.SQL), " "))
	o.mu.Lock()
	switch {
	case strings.HasPrefix(statement, "begin "):
		if _, exists := o.transactions[conn]; exists {
			o.invalidTransaction = true
		}
		o.transactions[conn] = "unclassified"
		o.readOnly[conn] = strings.Contains(statement, " read only")
		if !strings.Contains(statement, "isolation level read committed") {
			o.invalidTransaction = true
		}
	case data.SQL == completedTransferBalanceCandidatesSql:
		phase = "discovery"
		o.discoveryStarts++
		o.discoveryOpen = true
		o.discoveryConn = conn
	case data.SQL == completedTransferBalanceDeleteSql:
		phase = "delete"
		o.deletes++
		if o.transactions[conn] != "unclassified" || o.readOnly[conn] {
			o.invalidTransaction = true
		}
		o.transactions[conn] = "delete"
		ids, ok := data.Args[0].([]server.Id)
		if !ok {
			o.invalidTransaction = true
		}
		o.deleteCandidates = slices.Clone(ids)
		if !o.discoveryReleased {
			o.deleteBeforeRelease = true
		}
	case data.SQL == netEscrowReservationPageSQL:
		phase = "census"
		o.censusStarts++
		if o.transactions[conn] != "unclassified" || !o.readOnly[conn] {
			o.invalidTransaction = true
		}
		o.transactions[conn] = "census"
		ids, ok := data.Args[0].([]server.Id)
		if !ok {
			o.invalidTransaction = true
		}
		o.censusCandidates = slices.Clone(ids)
		o.censusAfterCommit = o.deleteCommits > 0
		o.censusUncancelled = ctx.Err() == nil
	case statement == "commit":
		phase = "commit_" + o.transactions[conn]
	case statement == "rollback":
		phase = "rollback"
	}
	o.mu.Unlock()
	return context.WithValue(ctx, retentionOwnerQueryKey{}, phase)
}

func (o *retentionOwnerTrace) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(retentionOwnerContextKey{}) != o {
		return
	}
	phase, _ := ctx.Value(retentionOwnerQueryKey{}).(string)
	var cancel context.CancelFunc
	o.mu.Lock()
	if phase == "discovery" {
		o.discoveryOpen = false
		o.discoveryEnds++
		if data.Err == nil && o.cancelAfterDiscovery {
			cancel = o.cancel
		}
	}
	if phase == "delete" && data.Err == nil {
		o.deletedRows += int(data.CommandTag.RowsAffected())
	}
	if phase == "census" && data.Err == nil {
		o.censusEnds++
	}
	if strings.HasPrefix(phase, "commit_") {
		if data.Err == nil {
			o.commits++
			switch phase {
			case "commit_delete":
				o.deleteCommits++
				if o.deleteCommits == 1 && o.cancelAfterCommit {
					cancel = o.cancel
				}
			case "commit_census":
				o.censusCommits++
			default:
				o.unknownCommits++
			}
		}
	}
	if phase == "rollback" || strings.HasPrefix(phase, "commit_") {
		delete(o.transactions, conn)
		delete(o.readOnly, conn)
	}
	o.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func retentionOwnerFixture(t *testing.T, retained, deletable int, observer *retentionOwnerTrace, run func(testing.TB, context.Context, context.Context, server.Id, string)) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fixtureCtx := t.Context()
		network := server.NewId()
		server.Tx(fixtureCtx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(fixtureCtx, `INSERT INTO transfer_balance
				(balance_id,network_id,end_time,start_balance_byte_count,balance_byte_count,net_revenue_nano_cents,pro)
				SELECT md5($1::uuid::text || ':kept:' || g)::uuid,$1,now()-interval '10 days',1000,1000,0,false
				FROM generate_series(1,$2) AS g`, network, retained))
			server.RaisePgResult(tx.Exec(fixtureCtx, `INSERT INTO transfer_escrow
				(contract_id,balance_id,balance_byte_count)
				SELECT md5($1::uuid::text || ':contract:' || g)::uuid,
				md5($1::uuid::text || ':kept:' || g)::uuid,23 FROM generate_series(1,$2) AS g`, network, retained))
			server.RaisePgResult(tx.Exec(fixtureCtx, `INSERT INTO transfer_balance
				(balance_id,network_id,end_time,start_balance_byte_count,balance_byte_count,net_revenue_nano_cents,pro)
				SELECT md5($1::uuid::text || ':deletable:' || g)::uuid,$1,now()-interval '10 days',1000,1000,0,false
				FROM generate_series(1,$2) AS g`, network, deletable))
		})
		pop := server.Config.PushSimpleResource(server.MaintenancePgConfigResourceName, []byte("min_connections: 0\nmax_connections: 1\n"))
		defer pop()
		server.PgReset()
		defer server.PgReset()
		observer.held = map[*pgx.Conn]bool{}
		observer.transactions = map[*pgx.Conn]string{}
		observer.readOnly = map[*pgx.Conn]bool{}
		scope, err := server.NewTestPgQueryScope(fixtureCtx, observer)
		server.Raise(err)
		defer func() {
			if err := scope.Close(); err != nil {
				t.Errorf("retention query scope did not restore its pools: %v", err)
			}
		}()
		spoolDir := t.TempDir()
		t.Setenv("TMPDIR", spoolDir)
		ctx, cancel := context.WithTimeout(fixtureCtx, 15*time.Second)
		defer cancel()
		observer.cancel = cancel
		ctx = context.WithValue(ctx, retentionOwnerContextKey{}, observer)
		run(t, fixtureCtx, ctx, network, spoolDir)
	})
}

func requireRetentionOwnerTraceClosed(t testing.TB, observer *retentionOwnerTrace, spoolDir string) {
	t.Helper()
	observer.mu.Lock()
	capacity, held := observer.firstCapacity, len(observer.held)
	acquires, releases := observer.acquires, observer.releases
	open, early := observer.discoveryOpen, observer.deleteBeforeRelease
	transactions, invalid := len(observer.transactions), observer.invalidTransaction
	unknownCommits := observer.unknownCommits
	observer.mu.Unlock()
	if capacity != 1 || held != 0 || acquires != releases || open || early || transactions != 0 || invalid || unknownCommits != 0 {
		t.Fatalf("retention resource lifecycle differs: capacity=%d held=%d acquire=%d release=%d discovery_open=%t early_delete=%t transactions=%d invalid_transaction=%t unknown_commits=%d", capacity, held, acquires, releases, open, early, transactions, invalid, unknownCommits)
	}
	entries, err := os.ReadDir(spoolDir)
	server.Raise(err)
	if len(entries) != 0 {
		t.Fatalf("retention left %d named spool files", len(entries))
	}
	// The native fixture is Linux. An unlinked but unclosed file still
	// consumes its descriptor and storage, so directory emptiness is not enough.
	fdEntries, err := os.ReadDir("/proc/self/fd")
	server.Raise(err)
	openSpools := 0
	for _, entry := range fdEntries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		server.Raise(err)
		if strings.HasPrefix(target, filepath.Join(spoolDir, "urnetwork-balance-retention-")) {
			openSpools++
		}
	}
	if openSpools != 0 {
		t.Fatalf("retention kept %d anonymous spool descriptors open", openSpools)
	}
}

func requireRetentionOwnerBalances(t testing.TB, ctx context.Context, network server.Id, wantBalances, wantDebt int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var balances, debt int
		var debtBytes int64
		server.Raise(conn.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM transfer_balance WHERE network_id=$1),
			(SELECT count(*) FROM transfer_escrow WHERE balance_id IN
				(SELECT balance_id FROM transfer_balance WHERE network_id=$1) AND NOT settled),
			(SELECT COALESCE(sum(balance_byte_count),0) FROM transfer_escrow WHERE balance_id IN
				(SELECT balance_id FROM transfer_balance WHERE network_id=$1) AND NOT settled)`, network).Scan(&balances, &debt, &debtBytes))
		if balances != wantBalances || debt != wantDebt || debtBytes != int64(23*wantDebt) {
			t.Fatalf("retention changed candidate/debt custody: balances=%d want=%d debt=%d want=%d debt_bytes=%d", balances, wantBalances, debt, wantDebt, debtBytes)
		}
	})
}

func TestCompletedTransferBalanceRetentionDoesNotReacquirePostgres(t *testing.T) {
	observer := &retentionOwnerTrace{}
	retentionOwnerFixture(t, 300, 20, observer, func(t testing.TB, fixtureCtx, ctx context.Context, network server.Id, spoolDir string) {
		recovered := server.HandleError(func() {
			removeCompletedTransferBalanceBatches(ctx, server.NowUtc().Add(-7*24*time.Hour))
		})
		observer.mu.Lock()
		overlapped, starts, ends, deletes := observer.overlapped, observer.discoveryStarts, observer.discoveryEnds, observer.deletes
		observer.mu.Unlock()
		requireRetentionOwnerTraceClosed(t, observer, spoolDir)
		if starts != 1 || ends != 1 {
			t.Fatalf("retention discovery observation did not complete: starts=%d ends=%d", starts, ends)
		}
		if overlapped {
			requireRetentionOwnerBalances(t, fixtureCtx, network, 320, 300)
			t.Fatal("retention reacquired PostgreSQL while its discovery connection was held")
		}
		if recovered != nil || deletes != 2 {
			t.Fatalf("retention did not finish the one-pass bounded batch control: panic_type=%T discovery=%d/%d deletes=%d", recovered, starts, ends, deletes)
		}
		requireRetentionOwnerBalances(t, fixtureCtx, network, 300, 300)
	})
}

func TestCompletedTransferBalanceRetentionCancelsBeforeDeletion(t *testing.T) {
	observer := &retentionOwnerTrace{cancelAfterDiscovery: true}
	retentionOwnerFixture(t, 300, 20, observer, func(t testing.TB, fixtureCtx, ctx context.Context, network server.Id, spoolDir string) {
		recovered := server.HandleError(func() {
			removeCompletedTransferBalanceBatches(ctx, server.NowUtc().Add(-7*24*time.Hour))
		})
		err, _ := recovered.(error)
		observer.mu.Lock()
		deletes, commits, overlapped := observer.deletes, observer.commits, observer.overlapped
		observer.mu.Unlock()
		if !errors.Is(err, context.Canceled) || deletes != 0 || commits != 0 || overlapped {
			t.Fatalf("cancelled discovery escaped into deletion: panic_type=%T deletes=%d commits=%d overlap=%t", recovered, deletes, commits, overlapped)
		}
		requireRetentionOwnerTraceClosed(t, observer, spoolDir)
		requireRetentionOwnerBalances(t, fixtureCtx, network, 320, 300)
	})
}

func TestCompletedTransferBalanceRetentionCancellationKeepsCommittedBatch(t *testing.T) {
	observer := &retentionOwnerTrace{cancelAfterCommit: true}
	retentionOwnerFixture(t, 0, 520, observer, func(t testing.TB, fixtureCtx, ctx context.Context, network server.Id, spoolDir string) {
		recovered := server.HandleError(func() {
			removeCompletedTransferBalanceBatches(ctx, server.NowUtc().Add(-7*24*time.Hour))
		})
		err, _ := recovered.(error)
		observer.mu.Lock()
		deletes, commits, overlapped := observer.deletes, observer.commits, observer.overlapped
		deleteCommits, censusCommits, deletedRows := observer.deleteCommits, observer.censusCommits, observer.deletedRows
		censusStarts, censusEnds := observer.censusStarts, observer.censusEnds
		censusAfterCommit, censusUncancelled := observer.censusAfterCommit, observer.censusUncancelled
		deleteCandidates, censusCandidates := slices.Clone(observer.deleteCandidates), slices.Clone(observer.censusCandidates)
		observer.mu.Unlock()
		// The deletion commit cancels the request. Its detached mirror census
		// still commits read-only before the next batch observes cancellation.
		if !errors.Is(err, context.Canceled) || deletes != 1 || deleteCommits != 1 || deletedRows != 256 || commits != 2 || censusCommits != 1 || overlapped {
			t.Fatalf("post-commit cancellation changed the batch boundary: panic_type=%T deletes=%d delete_commits=%d deleted_rows=%d commits=%d census_commits=%d overlap=%t", recovered, deletes, deleteCommits, deletedRows, commits, censusCommits, overlapped)
		}
		slices.SortFunc(deleteCandidates, server.Id.Cmp)
		slices.SortFunc(censusCandidates, server.Id.Cmp)
		if censusStarts != 1 || censusEnds != 1 || !censusAfterCommit || !censusUncancelled || len(censusCandidates) != 256 || !slices.Equal(deleteCandidates, censusCandidates) {
			t.Fatalf("committed batch did not finish its exact read-only mirror census: census=%d/%d after_commit=%t uncancelled=%t candidate_count=%d", censusStarts, censusEnds, censusAfterCommit, censusUncancelled, len(censusCandidates))
		}
		requireRetentionOwnerTraceClosed(t, observer, spoolDir)
		requireRetentionOwnerBalances(t, fixtureCtx, network, 264, 0)
		// The next independently owned invocation must finish the unprocessed
		// suffix instead of restarting a retained first page or losing work.
		removeCompletedTransferBalanceBatches(fixtureCtx, server.NowUtc().Add(-7*24*time.Hour))
		requireRetentionOwnerBalances(t, fixtureCtx, network, 0, 0)
		requireRetentionOwnerTraceClosed(t, observer, spoolDir)
	})
}
