// An owner turn of deadline closes reads each grant's pending journal sum once,
// not once per member, and its charges stay exactly those of a read per member.
package model

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// The deadline escrow read, in both its shapes; a member's first argument.
const deadlineEscrowReadFragment = "CROSS JOIN LATERAL (SELECT contract_id,balance_id,balance_byte_count,settled,payout_byte_count,redis_reserved"

// Counts every statement that sums a grant's journal debits, and runs inject
// once, before the deadline escrow read of one contract.
type deadlinePendingTracer struct {
	sums      atomic.Int64
	before    server.Id
	inject    func()
	injecting atomic.Bool
	injected  atomic.Bool
}

func (self *deadlinePendingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if self.injecting.Load() {
		return ctx
	}
	if strings.Contains(data.SQL, "sum(debit_byte_count)") && strings.Contains(data.SQL, "FROM transfer_debit_journal") {
		self.sums.Add(1)
	}
	if self.inject != nil && strings.Contains(data.SQL, deadlineEscrowReadFragment) && 0 < len(data.Args) &&
		data.Args[0] == any(self.before) && self.injected.CompareAndSwap(false, true) {
		self.injecting.Store(true)
		defer self.injecting.Store(false)
		self.inject()
	}
	return ctx
}

func (self *deadlinePendingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func deadlinePendingTraced(t testing.TB, ctx context.Context, tracer *deadlinePendingTracer, f func()) {
	t.Helper()
	scope, err := server.NewTestPgQueryScope(ctx, tracer)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := scope.Close(); err != nil {
			t.Error(err)
		}
	}()
	f()
}

// A due Redis contract on the fixture's grant with one source report.
func deadlinePendingTestContract(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, escrow, reported ByteCount) server.Id {
	t.Helper()
	id := createRedisAdmissionTest(ctx, f, escrow).ContractId
	server.Raise(CloseContract(ctx, id, f.sourceId, reported, true))
	return id
}

func deadlinePendingTestDebits(t testing.TB, ctx context.Context, balanceId server.Id, contractIds []server.Id) (debits []ByteCount, pending, balance ByteCount) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		for _, contractId := range contractIds {
			var debit ByteCount
			server.Raise(conn.QueryRow(ctx, `SELECT COALESCE((SELECT debit_byte_count FROM transfer_debit_journal
				WHERE balance_id=$1 AND contract_id=$2),-1)`, balanceId, contractId).Scan(&debit))
			debits = append(debits, debit)
		}
		server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(sum(debit_byte_count),0) FROM transfer_debit_journal
			WHERE balance_id=$1 AND NOT applied`, balanceId).Scan(&pending))
		server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1`, balanceId).Scan(&balance))
	})
	return
}

// Four members of one paid grant whose room runs out partway: 150 of room
// after an accepted 850-byte debit, 60 bytes reported by each member. A read
// per member charges 60, 60, 30 and 0, and so must the turn. On 2026-11-25
// the sum scanned the whole journal of a grant with a large backlog for every
// member, so each turn held the grant's owner for minutes; the turn now reads
// it once and checks it once before commit.
func TestDeadlineOwnerTurnReadsGrantPendingOnce(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		var contractIds []server.Id
		for range 4 {
			contractIds = append(contractIds, deadlinePendingTestContract(t, ctx, f, 100, 60))
		}
		slices.SortFunc(contractIds, server.Id.Cmp)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_debit_journal(contract_id,balance_id,shard,debit_byte_count)
				VALUES($1,$2,$3,850)`, server.NewId(), f.balanceId, transferDebitShard(f.balanceId)))
		})
		deadline := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
		members := []ContractDeadline{}
		for _, contractId := range contractIds {
			members = append(members, ContractDeadline{ContractId: contractId, Deadline: deadline})
		}
		tracer := &deadlinePendingTracer{}
		var results map[server.Id]*ContractDeadlineReconciliation
		var busy, entered bool
		var err error
		deadlinePendingTraced(t, ctx, tracer, func() {
			results, busy, entered, err = reconcileContractOwnerMembersAtDeadline(ctx, []server.Id{f.balanceId}, members)
		})
		if err != nil || busy || !entered || len(results) != len(members) {
			t.Fatal("the owner turn did not close its members", err, busy, entered, len(results))
		}
		charged := []ByteCount{}
		for _, contractId := range contractIds {
			charged = append(charged, results[contractId].Charged)
		}
		if !slices.Equal(charged, []ByteCount{60, 60, 30, 0}) {
			t.Fatal("the turn's charges differ from a read per member", charged)
		}
		if sums := tracer.sums.Load(); sums != 2 {
			t.Fatalf("the owner turn summed the grant's pending journal %d times for %d members, want one read and one check", sums, len(members))
		}
		debits, pending, balance := deadlinePendingTestDebits(t, ctx, f.balanceId, contractIds)
		if !slices.Equal(debits, []ByteCount{60, 60, 30, 0}) || pending != 1000 || balance != 1000 {
			t.Fatal("the turn's journal differs from its charges or exceeds the grant", debits, pending, balance)
		}
		applied, released, flushBusy, flushErr := flushTransferDebitBalance(ctx, f.balanceId)
		_, pending, balance = deadlinePendingTestDebits(t, ctx, f.balanceId, nil)
		if flushErr != nil || flushBusy || applied != 5 || released != 5 || pending != 0 || balance != 0 ||
			Testing_NetEscrowByteCount(ctx, f.balanceId) != 0 {
			t.Fatal("the flush did not apply the turn's debits exactly", applied, released, flushBusy, flushErr, pending, balance)
		}
	})
}

// A settlement outside the owner appends a debit to the grant while a shared
// sibling turn holds it, after the first sibling's charge and before the
// second's. Read per member, the second sibling sees it and is held to the
// room left. The turn's single read cannot see it, so its check fails the
// turn before commit, and each sibling closes alone with its own read: the
// charges are the per-member ones, and the grant is never overdrawn.
func TestDeadlineOwnerTurnKeepsPerMemberChargesUnderConcurrentSettlement(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		primary := deadlinePendingTestContract(t, ctx, f, 100, 50)
		siblings := []server.Id{
			deadlinePendingTestContract(t, ctx, f, 300, 300),
			deadlinePendingTestContract(t, ctx, f, 300, 300),
		}
		slices.SortFunc(siblings, server.Id.Cmp)
		concurrent := createRedisAdmissionTest(ctx, f, 100).ContractId
		// The admissions reserved 800 of 1000 bytes; the grant now holds 650,
		// so the room, not the reservations, bounds the last charge.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET balance_byte_count=650 WHERE balance_id=$1`, f.balanceId))
		})
		deadline := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
		due := func(_ context.Context, _ server.PgCanQuery, contractIds []server.Id) []ContractDeadline {
			deadlines := []ContractDeadline{}
			for _, contractId := range contractIds {
				if slices.Contains(siblings, contractId) {
					deadlines = append(deadlines, ContractDeadline{ContractId: contractId, Deadline: deadline})
				}
			}
			return deadlines
		}
		tracer := &deadlinePendingTracer{before: siblings[1], inject: func() {
			_ = asyncDebitTestSettle(ctx, concurrent, 100)
		}}
		var result *ContractOwnerDeadlineResult
		var err error
		deadlinePendingTraced(t, ctx, tracer, func() {
			result, err = ReconcileContractOwnerAtDeadline(ctx, []server.Id{f.balanceId}, ContractDeadline{ContractId: primary, Deadline: deadline}, due)
		})
		if err != nil || result == nil || result.Busy || result.Reconciliation == nil || result.SiblingsClosed != 2 || !tracer.injected.Load() {
			t.Fatal("the owner did not close its primary and both siblings around the concurrent settlement", result, err, tracer.injected.Load())
		}
		debits, pending, balance := deadlinePendingTestDebits(t, ctx, f.balanceId, []server.Id{primary, siblings[0], siblings[1], concurrent})
		if !slices.Equal(debits, []ByteCount{50, 300, 200, 100}) || pending != 650 || balance != 650 {
			t.Fatal("the siblings' charges differ from a read per member or overdraw the grant", debits, pending, balance)
		}
		applied, released, flushBusy, flushErr := flushTransferDebitBalance(ctx, f.balanceId)
		_, pending, balance = deadlinePendingTestDebits(t, ctx, f.balanceId, nil)
		if flushErr != nil || flushBusy || applied != 4 || released != 4 || pending != 0 || balance != 0 {
			t.Fatal("the flush did not apply the charges exactly", applied, released, flushBusy, flushErr, pending, balance)
		}
	})
}
