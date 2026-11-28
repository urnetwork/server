package model

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urnetwork/server/v2026"
)

// Observe only the getter's marked caller, including release before any result
// or error assertion. The two successful reads must share its one pool owner.
type transferEscrowReadbackObserver struct {
	stateLock     sync.Mutex
	acquisitions  int
	acquireErrors int
	releases      int
	queryStarts   int
	queryEnds     int
	queryErrors   int
	unownedReads  int
	maxActive     int
	active        map[*pgx.Conn]bool
}

func (self *transferEscrowReadbackObserver) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	return ctx
}

func (self *transferEscrowReadbackObserver) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	if ctx.Value(self) != true {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.acquisitions++
	if data.Err != nil || data.Conn == nil {
		self.acquireErrors++
		return
	}
	if self.active == nil {
		self.active = map[*pgx.Conn]bool{}
	}
	self.active[data.Conn] = true
	self.maxActive = max(self.maxActive, len(self.active))
}

func (self *transferEscrowReadbackObserver) TraceRelease(_ *pgxpool.Pool, data pgxpool.TraceReleaseData) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.active[data.Conn] {
		self.releases++
		delete(self.active, data.Conn)
	}
}

func (self *transferEscrowReadbackObserver) TraceQueryStart(ctx context.Context, conn *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	if ctx.Value(self) != true {
		return ctx
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.queryStarts++
	if !self.active[conn] {
		self.unownedReads++
	}
	return ctx
}

func (self *transferEscrowReadbackObserver) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(self) != true {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.queryEnds++
	if data.Err != nil {
		self.queryErrors++
	}
}

func (self *transferEscrowReadbackObserver) require(t testing.TB, queries, queryErrors int) {
	t.Helper()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.acquisitions != 1 || self.acquireErrors != 0 || self.releases != 1 || self.maxActive != 1 || len(self.active) != 0 || self.unownedReads != 0 {
		t.Fatalf("getter did not join one database owner: acquisitions=%d errors=%d releases=%d max_active=%d active=%d unowned_reads=%d", self.acquisitions, self.acquireErrors, self.releases, self.maxActive, len(self.active), self.unownedReads)
	}
	if self.queryStarts != queries || self.queryEnds != queries || self.queryErrors != queryErrors {
		t.Fatalf("getter read lifecycle starts=%d ends=%d errors=%d; want %d/%d/%d", self.queryStarts, self.queryEnds, self.queryErrors, queries, queries, queryErrors)
	}
}

func testTransferEscrowReadback(t *testing.T, missing bool, requested ByteCount) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		contractId := server.NewId()
		var want *TransferEscrow
		if !missing {
			var err error
			want, err = CreateTransferEscrow(ctx, fixture.sourceNetworkId, fixture.sourceId, fixture.destinationNetworkId, fixture.destinationId, requested)
			if err != nil || want == nil || want.TransferByteCount != requested {
				t.Fatal("readback fixture creation failed", err)
			}
			contractId = want.ContractId
		}
		observer := &transferEscrowReadbackObserver{}
		scope, err := server.NewTestPgQueryScope(ctx, observer)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()
		observed := context.WithValue(ctx, observer, true)
		var got *TransferEscrow
		var readErr error
		server.HandleError(func() {
			got = GetTransferEscrow(observed, contractId)
		}, func(err error) { readErr = err })
		if readErr != nil {
			var pgErr *pgconn.PgError
			if errors.As(readErr, &pgErr) && pgErr.Code == "42P01" && strings.Contains(pgErr.Message, "transfer_byte_count") {
				observer.require(t, 1, 1)
				t.Fatal("transfer escrow readback referenced the amount column as a relation")
			}
			t.Fatal("transfer escrow readback failed", readErr)
		}
		if missing {
			if got != nil {
				t.Fatal("missing contract gained an escrow readback")
			}
			observer.require(t, 1, 0)
			return
		}
		if got == nil || got.TransferByteCount != requested || got.Priority != want.Priority || len(got.Balances) != 1 || got.Balances[0] == nil || got.Balances[0].BalanceId != fixture.balanceId || got.Balances[0].BalanceByteCount != requested {
			t.Fatal("escrow readback lost its exact committed amount, priority or grant anchor")
		}
		observer.require(t, 2, 0)
		if Testing_NetEscrowByteCount(ctx, fixture.balanceId) != requested {
			t.Fatal("escrow readback changed reservation accounting")
		}
	})
}

func TestTransferEscrowReadbackMissingSingleBorrow(t *testing.T) {
	testTransferEscrowReadback(t, true, 0)
}

func TestTransferEscrowReadbackZeroSingleBorrow(t *testing.T) {
	testTransferEscrowReadback(t, false, 0)
}

func TestTransferEscrowReadbackNonzeroSingleBorrow(t *testing.T) {
	testTransferEscrowReadback(t, false, 23)
}
