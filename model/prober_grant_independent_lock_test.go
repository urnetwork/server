package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// NOWAIT makes accidental contention an explicit PostgreSQL error rather than
// an elapsed-time assertion. It changes only the test's locking grant read.
// Only zero-byte anchors can run through it: a positive allocation admits
// grant ownership, which requires the server's own transaction (403b9c55).
type noWaitGrantTestTx struct{ server.PgTx }

func (tx noWaitGrantTestTx) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	normalized := strings.Join(strings.Fields(query), " ")
	if strings.Contains(normalized, " FROM transfer_balance ") && strings.HasSuffix(normalized, " FOR UPDATE") {
		query += " NOWAIT"
	}
	return tx.PgTx.Query(ctx, query, args...)
}

func independentGrantTestCreate(ctx context.Context, tx server.PgTx, clients escrowSelectionTestClients) (escrow *TransferEscrow, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if recoveredErr, ok := recovered.(error); ok {
				err = recoveredErr
			} else {
				err = fmt.Errorf("unexpected escrow panic type %T", recovered)
			}
		}
	}()
	escrow, _, err = createTransferEscrowInTx(ctx, tx, clients.payerNetworkId, clients.payerId,
		clients.providerNetworkId, clients.providerId, clients.payerNetworkId, 1024, nil)
	return
}

// One owned allocation's result and the transaction-scoped ownership events
// its admission reported, in order.
type dynamicProberAllocation struct {
	escrow *TransferEscrow
	err    error
	events []server.PgOwnershipEventKind
}

// Allocates in the server's own read-committed no-retry transaction, the only
// transaction that can admit grant ownership (403b9c55). A refused or failed
// allocation rolls back. The caller's goroutine runs it to completion, so a
// return while a competitor still holds its locks proves it did not wait.
func allocateDynamicProberGrantForTest(ctx context.Context, clients escrowSelectionTestClients) dynamicProberAllocation {
	allocation := dynamicProberAllocation{}
	observed := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
		if event.TransactionScoped {
			allocation.events = append(allocation.events, event.Kind)
		}
	})
	transactionErr := rareGrantModelError(func() {
		server.Tx(observed, func(tx server.PgTx) {
			allocation.escrow, allocation.err = independentGrantTestCreate(observed, tx, clients)
			server.Raise(allocation.err)
		}, server.TxReadCommitted, server.OptNoRetry())
	})
	if allocation.err == nil {
		allocation.err = transactionErr
	}
	return allocation
}

// The committed contracts the payer funds and every escrow byte on each grant.
// A refused or failed allocation must leave both exactly as they were.
func requireDynamicProberCommittedEscrow(t testing.TB, ctx context.Context, payerNetworkId server.Id, wantContracts int64, wantEscrow map[server.Id]ByteCount) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var contracts int64
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE payer_network_id=$1`, payerNetworkId).Scan(&contracts))
		if contracts != wantContracts {
			t.Fatalf("payer has %d committed contracts, want %d", contracts, wantContracts)
		}
		for balanceId, want := range wantEscrow {
			var escrow ByteCount
			server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(sum(balance_byte_count),0) FROM transfer_escrow WHERE balance_id=$1`, balanceId).Scan(&escrow))
			if escrow != want {
				t.Fatalf("grant escrow=%d, want %d", escrow, want)
			}
		}
	})
}

// Two payer clients whose bounded preferences start at different grants. One
// allocation owns every current grant of its payer (403b9c55), so the second,
// though it wants the other grant, is refused busy at admission instead of
// joining a lock queue or skipping to its grant. The refusal writes nothing;
// once the owner commits, a retry keeps its own preferred grant.
func TestDynamicProberDisjointGrantRefusedBusyThenKeepsPreference(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		first := newEscrowSelectionTestClients(t, ctx)
		second := first
		for suffix := range 256 {
			candidate := server.Id{0: 0xf2, 15: byte(suffix)}
			if candidate.Hash()%2 != first.payerId.Hash()%2 {
				second.payerId = candidate
				break
			}
		}
		if second.payerId == first.payerId {
			t.Fatal("fixture did not find distinct grant preferences")
		}
		insertContractLifecycleTestClients(t, ctx, map[server.Id]server.Id{second.payerId: first.payerNetworkId})
		setDynamicProberIdentityForTest(t, ctx, first)
		now := server.NowUtc()
		grants := []*TransferBalance{
			addDynamicProberGrantForTest(ctx, first.payerNetworkId, now.Add(-time.Minute), now.Add(time.Hour), 4096),
			addDynamicProberGrantForTest(ctx, first.payerNetworkId, now.Add(-2*time.Minute), now.Add(time.Hour), 4096),
		}
		firstGrant := grants[first.payerId.Hash()%2]
		secondGrant := grants[second.payerId.Hash()%2]
		if firstGrant.BalanceId == secondGrant.BalanceId {
			t.Fatal("regression did not exercise disjoint grants")
		}
		// Keep the first reservation and its payer ownership uncommitted while
		// the other payer client allocates.
		firstTx, finish := beginRareGrantTestTx(t, ctx)
		defer finish(false)
		firstEscrow, err := independentGrantTestCreate(ctx, firstTx, first)
		if err != nil || firstEscrow == nil || len(firstEscrow.Balances) != 1 || firstEscrow.Balances[0].BalanceId != firstGrant.BalanceId {
			t.Fatalf("first request did not reserve its selected grant: %v", err)
		}
		refused := allocateDynamicProberGrantForTest(ctx, second)
		if refused.escrow != nil || !errors.Is(refused.err, errTransferBalanceOwnershipBusy) {
			t.Fatalf("disjoint grant allocation was not refused busy while its payer was owned: %v", refused.err)
		}
		if len(refused.events) != 1 || refused.events[0] != server.PgOwnershipRefused {
			t.Fatalf("disjoint grant ownership events=%v, want exactly one refusal", refused.events)
		}
		server.Raise(finish(true))
		requireDynamicProberCommittedEscrow(t, ctx, first.payerNetworkId, 1, map[server.Id]ByteCount{
			firstGrant.BalanceId: 1024, secondGrant.BalanceId: 0,
		})
		retried := allocateDynamicProberGrantForTest(ctx, second)
		if retried.err != nil || retried.escrow == nil || len(retried.escrow.Balances) != 1 || retried.escrow.Balances[0].BalanceId != secondGrant.BalanceId {
			t.Fatalf("retry after the owner committed did not keep its own preferred grant: %v", retried.err)
		}
		requireDynamicProberCommittedEscrow(t, ctx, first.payerNetworkId, 2, map[server.Id]ByteCount{
			firstGrant.BalanceId: 1024, secondGrant.BalanceId: 1024,
		})
	})
}

// A writer outside grant ownership, such as an older binary during a rolling
// deploy, can still hold the preferred grant's row lock. The owned allocation
// then skips that busy grant and funds from another available one. It runs
// while the lock is held, so waiting for that lock would last until the
// deadline rather than return.
func TestDynamicProberBusyPreferredGrantUsesAnotherAvailableGrant(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		clients := newEscrowSelectionTestClients(t, ctx)
		setDynamicProberIdentityForTest(t, ctx, clients)
		now := server.NowUtc()
		grants := []*TransferBalance{
			addDynamicProberGrantForTest(ctx, clients.payerNetworkId, now.Add(-time.Minute), now.Add(time.Hour), 4096),
			addDynamicProberGrantForTest(ctx, clients.payerNetworkId, now.Add(-2*time.Minute), now.Add(time.Hour), 4096),
		}
		preferred := grants[clients.payerId.Hash()%2]
		other := grants[(clients.payerId.Hash()%2+1)%2]
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, preferred.BalanceId))
		allocation := allocateDynamicProberGrantForTest(ctx, clients)
		if allocation.err != nil || allocation.escrow == nil || len(allocation.escrow.Balances) != 1 || allocation.escrow.Balances[0].BalanceId != other.BalanceId {
			t.Fatalf("busy preferred grant did not fund from the other available grant: %v", allocation.err)
		}
		if len(allocation.events) == 0 || allocation.events[0] != server.PgOwnershipAdmitted {
			t.Fatalf("allocation ownership events=%v, want admission first", allocation.events)
		}
		server.Raise(held.Rollback(ctx))
		requireDynamicProberCommittedEscrow(t, ctx, clients.payerNetworkId, 1, map[server.Id]ByteCount{
			preferred.BalanceId: 0, other.BalanceId: 1024,
		})
	})
}

// Identifies the allocator's grant locks by their SQL: a bounded speculative
// candidate lock skips busy rows, while the complete fallback blocks.
func dynamicProberGrantLock(sql string) (locking bool, speculative bool) {
	normalized := strings.Join(strings.Fields(sql), " ")
	return strings.Contains(normalized, " FOR UPDATE"), strings.Contains(normalized, " SKIP LOCKED")
}

// Counts exact reservation snapshot attempts; a matching durable revision
// avoids a historical census within an attempt. Zero-byte anchors take no
// ownership, so this wrapper can still observe them.
type grantHintTestTx struct {
	server.PgTx
	reservationReads int
	reservationRows  int
}

func (tx *grantHintTestTx) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	if query == netEscrowAdmissionCacheSQL {
		tx.reservationReads++
		tx.reservationRows += len(args[0].([]server.Id))
	}
	return tx.PgTx.Query(ctx, query, args...)
}

func TestDynamicProberOptimisticCreditRechecksAndReleasesBeforeAlternative(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := newEscrowSelectionTestClients(t, ctx)
		setDynamicProberIdentityForTest(t, ctx, clients)
		now := server.NowUtc()
		grants := []*TransferBalance{
			addDynamicProberGrantForTest(ctx, clients.payerNetworkId, now.Add(-time.Minute), now.Add(time.Hour), 1024),
			addDynamicProberGrantForTest(ctx, clients.payerNetworkId, now.Add(-2*time.Minute), now.Add(time.Hour), 1024),
		}
		preferred := grants[clients.payerId.Hash()%2]
		other := grants[(clients.payerId.Hash()%2+1)%2]
		// Observe the owned allocation at the driver; positive allocation
		// admits grant ownership only on the server's own transaction (403b9c55).
		stopHooks := startLegacyStatementHooks(t, ctx)
		defer stopHooks()
		observer := acquireContractLifecycleTestConnection(t, ctx)
		defer observer.Release()
		locks := 0
		fallback := false
		var lockErrs []error
		server.Tx(ctx, func(tx server.PgTx) {
			controlled := &escrowGrantQueryHook{beforeGrant: func(sql string, args []any) {
				locking, speculative := dynamicProberGrantLock(sql)
				if !locking {
					return
				}
				if !speculative {
					fallback = true
					return
				}
				locks++
				switch locks {
				case 1:
					if args[2].([]server.Id)[0] != preferred.BalanceId {
						lockErrs = append(lockErrs, errors.New("optimistic selection changed payer order"))
					}
					// Reserve AFTER discovery and BEFORE its lock. Durable credit
					// must never stand in for fresh locked reservation authority.
					clients.reserve(ctx, preferred.BalanceId, 1024)
				case 2:
					if ids := args[2].([]server.Id); len(ids) != 1 || ids[0] != other.BalanceId {
						lockErrs = append(lockErrs, errors.New("rejected grant was retried or alternative was skipped"))
					}
					if _, err := observer.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE NOWAIT`, preferred.BalanceId); err != nil {
						lockErrs = append(lockErrs, fmt.Errorf("rejected candidate retained a lock before alternative: %w", err))
					}
				default:
					lockErrs = append(lockErrs, errors.New("candidate attempts were not bounded by available grants"))
				}
			}}
			hooked, unhook := withLegacyStatementHooks(ctx, tx, controlled.statementHook())
			defer unhook()
			escrow, err := independentGrantTestCreate(hooked, tx, clients)
			if err != nil || escrow == nil || len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != other.BalanceId {
				t.Fatalf("optimistic hint authorized reserved credit or lost alternative: %v", err)
			}
			if controlled.reservationReads != 2 || controlled.reservationRows != 2 {
				t.Fatalf("selected-grant census was amplified: reads=%d rows=%d", controlled.reservationReads, controlled.reservationRows)
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		if err := errors.Join(lockErrs...); err != nil {
			t.Fatal(err)
		}
		if fallback {
			t.Fatal("available adjacent grant unnecessarily joined full locking fallback")
		}
		if locks != 2 {
			t.Fatal("optimistic credit control did not exercise both grant locks")
		}
	})
}

// The speculative attempt finds the grant fully reserved; the reservation is
// released before the complete fallback, which must re-read funding rather
// than trust the stale hint. While the allocation owns the payer's grants an
// owned settlement is refused busy (403b9c55), so the release comes from a
// writer outside grant ownership, such as an older binary during a rolling
// deploy, ending the prior contract.
func TestDynamicProberRejectedCandidateStillUsesCreditReleasedBeforeFallback(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := newEscrowSelectionTestClients(t, ctx)
		setDynamicProberIdentityForTest(t, ctx, clients)
		now := server.NowUtc()
		grant := addDynamicProberGrantForTest(ctx, clients.payerNetworkId, now.Add(-time.Minute), now.Add(time.Hour), 1024)
		prior := allocateDynamicProberGrantForTest(ctx, clients)
		if prior.err != nil || prior.escrow == nil {
			t.Fatalf("initial reservation failed: %v", prior.err)
		}
		stopHooks := startLegacyStatementHooks(t, ctx)
		defer stopHooks()
		fallback := false
		var settleErr error
		server.Tx(ctx, func(tx server.PgTx) {
			controlled := &escrowGrantQueryHook{beforeGrant: func(sql string, _ []any) {
				locking, speculative := dynamicProberGrantLock(sql)
				if !locking || speculative || fallback {
					return
				}
				fallback = true
				// The owned zero-use settlement needs the grant this allocation
				// owns, and is refused without waiting or writing.
				settleErr = rareGrantModelError(func() {
					server.RunPosts(ctx, settleNetEscrowOrderingTestContract(ctx, prior.escrow.ContractId)...)
				})
				server.Db(ctx, func(conn server.PgConn) {
					server.RaisePgResult(conn.Exec(ctx, `UPDATE transfer_contract SET outcome='canceled',close_time=now() WHERE contract_id=$1`, prior.escrow.ContractId))
				})
			}}
			hooked, unhook := withLegacyStatementHooks(ctx, tx, controlled.statementHook())
			defer unhook()
			escrow, err := independentGrantTestCreate(hooked, tx, clients)
			if err != nil || escrow == nil || len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != grant.BalanceId {
				t.Fatalf("stale reserved hint falsely rejected released credit: %v", err)
			}
			if controlled.reservationReads != 2 || controlled.reservationRows != 2 {
				t.Fatalf("pessimistic-hint fallback changed bounded work: reads=%d rows=%d", controlled.reservationReads, controlled.reservationRows)
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		if !fallback {
			t.Fatal("pessimistic-hint control never reached complete fallback")
		}
		if !errors.Is(settleErr, errTransferBalanceOwnershipBusy) {
			t.Fatalf("owned settlement inside the payer's allocation was not refused busy: %v", settleErr)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var reports int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM contract_close WHERE contract_id=$1`, prior.escrow.ContractId).Scan(&reports))
			if reports != 0 {
				t.Fatal("refused settlement committed its close reports")
			}
		})
		if got := openEscrowReservedForBalances(ctx, []server.Id{grant.BalanceId})[grant.BalanceId].reserved; got != 1024 {
			t.Fatalf("released credit did not leave exactly one funded reservation: %d", got)
		}
	})
}

func TestDynamicProberReservationCensusCountIndependentOfWindow(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := newEscrowSelectionTestClients(t, ctx)
		setDynamicProberIdentityForTest(t, ctx, clients)
		now := server.NowUtc()
		for index := range proberGrantFirstCount + proberGrantExtendedCount {
			grant := addDynamicProberGrantForTest(ctx, clients.payerNetworkId,
				now.Add(-time.Duration(index+1)*time.Minute), now.Add(time.Hour), 1024)
			clients.reserve(ctx, grant.BalanceId, 1024)
		}
		stopHooks := startLegacyStatementHooks(t, ctx)
		defer stopHooks()
		server.Tx(ctx, func(tx server.PgTx) {
			controlled := &escrowGrantQueryHook{}
			hooked, unhook := withLegacyStatementHooks(ctx, tx, controlled.statementHook())
			defer unhook()
			escrow, err := independentGrantTestCreate(hooked, tx, clients)
			if escrow != nil || err == nil || !strings.Contains(err.Error(), "Insufficient balance") {
				t.Fatalf("fully reserved windows did not retain authoritative shortfall: %v", err)
			}
			// At most four selected-grant attempts per window, then one
			// complete fallback. Never census every speculative candidate.
			if controlled.reservationReads != 2*proberGrantAttemptsPerWindow+1 ||
				controlled.reservationRows != 2*proberGrantAttemptsPerWindow+proberGrantFirstCount+proberGrantExtendedCount {
				t.Fatalf("reservation census became per-grant work: reads=%d rows=%d", controlled.reservationReads, controlled.reservationRows)
			}
		}, server.TxReadCommitted, server.OptNoRetry())
	})
}

// Two clients of the payer compete for its only grant. While the first
// allocation owns the payer's grants, the second is refused busy at admission
// rather than waiting for the grant (403b9c55), and writes nothing. Once the
// first commits, a retry rechecks the committed reservation and finds no
// credit; no stale hint reuses the bytes the first allocation reserved.
func TestDynamicProberSameGrantRefusedBusyThenRechecksCommittedReservation(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		first := newEscrowSelectionTestClients(t, ctx)
		second := first
		second.payerId = server.NewId()
		insertContractLifecycleTestClients(t, ctx, map[server.Id]server.Id{second.payerId: first.payerNetworkId})
		setDynamicProberIdentityForTest(t, ctx, first)
		now := server.NowUtc()
		grant := addDynamicProberGrantForTest(ctx, first.payerNetworkId, now.Add(-time.Minute), now.Add(time.Hour), 1024)
		firstTx, finish := beginRareGrantTestTx(t, ctx)
		defer finish(false)
		firstEscrow, err := independentGrantTestCreate(ctx, firstTx, first)
		if err != nil || firstEscrow == nil {
			t.Fatalf("first reservation failed: %v", err)
		}
		refused := allocateDynamicProberGrantForTest(ctx, second)
		if refused.escrow != nil || !errors.Is(refused.err, errTransferBalanceOwnershipBusy) {
			t.Fatalf("same-grant allocation was not refused busy while the first owned it: %v", refused.err)
		}
		if len(refused.events) != 1 || refused.events[0] != server.PgOwnershipRefused {
			t.Fatalf("same-grant ownership events=%v, want exactly one refusal", refused.events)
		}
		server.Raise(finish(true))
		retried := allocateDynamicProberGrantForTest(ctx, second)
		if retried.escrow != nil || retried.err == nil || errors.Is(retried.err, errTransferBalanceOwnershipBusy) ||
			!strings.Contains(retried.err.Error(), "Insufficient balance") {
			t.Fatalf("retry reused a stale hint after the first reservation committed: %v", retried.err)
		}
		requireDynamicProberCommittedEscrow(t, ctx, first.payerNetworkId, 1, map[server.Id]ByteCount{grant.BalanceId: 1024})
	})
}
