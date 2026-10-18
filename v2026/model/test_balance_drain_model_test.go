package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

// These run without Postgres or Redis.

func TestTestBalanceDrainAllowlistParse(t *testing.T) {
	a := server.NewId()
	b := server.NewId()

	allowlist, err := parseTestBalanceDrainAllowlist([]byte(""))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(allowlist), 0)

	allowlist, err = parseTestBalanceDrainAllowlist([]byte("network_ids: []\n"))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(allowlist), 0)

	allowlist, err = parseTestBalanceDrainAllowlist([]byte("network_ids:\n  - " + a.String() + "\n  - " + b.String() + "\n"))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, allowlist, map[server.Id]bool{a: true, b: true})

	// one malformed entry disables the whole list rather than widening it
	_, err = parseTestBalanceDrainAllowlist([]byte("network_ids:\n  - " + a.String() + "\n  - '*'\n"))
	if err == nil {
		t.Fatal("malformed allowlist entry was accepted")
	}
	_, err = parseTestBalanceDrainAllowlist([]byte("network_ids: everyone\n"))
	if err == nil {
		t.Fatal("non-list allowlist was accepted")
	}
}

func TestTestBalanceDrainAllowlistGate(t *testing.T) {
	allowed := server.NewId()
	other := server.NewId()
	Testing_SetTestBalanceDrainAllowlist([]server.Id{allowed})
	defer Testing_SetTestBalanceDrainAllowlist(nil)

	connect.AssertEqual(t, TestBalanceDrainAllowed(context.Background(), allowed), true)
	connect.AssertEqual(t, TestBalanceDrainAllowed(context.Background(), other), false)

	// refused before any database access
	drain, err := DrainTestBalance(context.Background(), other, time.Minute)
	connect.AssertEqual(t, drain == nil, true)
	connect.AssertEqual(t, err, ErrTestBalanceDrainNotAllowed)

	Testing_SetTestBalanceDrainAllowlist([]server.Id{})
	connect.AssertEqual(t, TestBalanceDrainAllowed(context.Background(), allowed), false)
}

type failingQuery struct {
	t *testing.T
}

func (self failingQuery) Query(ctx context.Context, sql string, args ...any) (server.PgResult, error) {
	self.t.Fatal("drain lookup queried for a network outside the allowlist")
	return nil, nil
}

func TestTestBalanceDrainHotPathSkipsOtherNetworks(t *testing.T) {
	Testing_SetTestBalanceDrainAllowlist([]server.Id{server.NewId()})
	defer Testing_SetTestBalanceDrainAllowlist(nil)

	connect.AssertEqual(t, testBalanceDrainActive(context.Background(), failingQuery{t: t}, server.NewId(), time.Now()), false)
	connect.AssertEqual(t, IsTestBalanceDrainActive(context.Background(), server.NewId()), false)
}

func TestTestBalanceDrainEndTimeIsBounded(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	connect.AssertEqual(t, testBalanceDrainEndTime(now, 0), now.Add(DefaultTestBalanceDrainDuration))
	connect.AssertEqual(t, testBalanceDrainEndTime(now, -time.Hour), now.Add(DefaultTestBalanceDrainDuration))
	connect.AssertEqual(t, testBalanceDrainEndTime(now, time.Second), now.Add(MinTestBalanceDrainDuration))
	connect.AssertEqual(t, testBalanceDrainEndTime(now, 5*time.Minute), now.Add(5*time.Minute))
	connect.AssertEqual(t, testBalanceDrainEndTime(now, 365*24*time.Hour), now.Add(MaxTestBalanceDrainDuration))
}

func TestTestBalanceDrainOverlay(t *testing.T) {
	balances := func() []*TransferBalance {
		return []*TransferBalance{
			{BalanceId: server.NewId(), StartBalanceByteCount: 100, BalanceByteCount: 40},
			{BalanceId: server.NewId(), StartBalanceByteCount: 10, BalanceByteCount: 10},
		}
	}

	undrained := balances()
	applyTestBalanceDrain(undrained, false)
	connect.AssertEqual(t, undrained[0].BalanceByteCount, ByteCount(40))
	connect.AssertEqual(t, undrained[1].BalanceByteCount, ByteCount(10))

	drained := balances()
	applyTestBalanceDrain(drained, true)
	for _, balance := range drained {
		connect.AssertEqual(t, balance.BalanceByteCount, ByteCount(0))
	}
	// the purchased amount stays visible; only the available bytes read zero
	connect.AssertEqual(t, drained[0].StartBalanceByteCount, ByteCount(100))

	err := testBalanceDrainEscrowError(true, 1)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "insufficient balance") {
		t.Fatalf("drained escrow must fail as insufficient balance, got %v", err)
	}
	connect.AssertEqual(t, testBalanceDrainEscrowError(true, 0), nil)
	connect.AssertEqual(t, testBalanceDrainEscrowError(false, 1024), nil)
}

// Requires Postgres and Redis (server.DefaultTestEnv).
func TestTestBalanceDrainRoundTrip(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationId := server.NewId()
		otherNetworkId := server.NewId()
		otherId := server.NewId()
		testingCreatePaymentClient(ctx, sourceNetworkId, sourceId)
		testingCreatePaymentClient(ctx, destinationNetworkId, destinationId)
		testingCreatePaymentClient(ctx, otherNetworkId, otherId)

		balanceByteCount := ByteCount(1024 * 1024 * 1024)
		for _, networkId := range []server.Id{sourceNetworkId, otherNetworkId} {
			AddBasicTransferBalance(ctx, networkId, balanceByteCount, server.NowUtc(), server.NowUtc().Add(24*time.Hour))
		}

		Testing_SetTestBalanceDrainAllowlist([]server.Id{sourceNetworkId})
		defer Testing_SetTestBalanceDrainAllowlist(nil)

		before := GetActiveTransferBalanceByteCount(ctx, sourceNetworkId)
		connect.AssertEqual(t, before, balanceByteCount)

		_, err := DrainTestBalance(ctx, otherNetworkId, time.Minute)
		connect.AssertEqual(t, err, ErrTestBalanceDrainNotAllowed)

		drain, err := DrainTestBalance(ctx, sourceNetworkId, 5*time.Minute)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, drain.DrainedBalanceByteCount, before)
		again, err := DrainTestBalance(ctx, sourceNetworkId, 5*time.Minute)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, again.DrainId, drain.DrainId)

		connect.AssertEqual(t, GetActiveTransferBalanceByteCount(ctx, sourceNetworkId), ByteCount(0))
		_, err = CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1024)
		if err == nil || !strings.Contains(err.Error(), "Insufficient balance") {
			t.Fatalf("drained payer created an escrow: %v", err)
		}

		// another network is untouched
		connect.AssertEqual(t, GetActiveTransferBalanceByteCount(ctx, otherNetworkId), balanceByteCount)
		_, err = CreateTransferEscrow(ctx, otherNetworkId, otherId, destinationNetworkId, destinationId, 1024)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, RestoreTestBalance(ctx, otherNetworkId), int64(0))

		connect.AssertEqual(t, RestoreTestBalance(ctx, sourceNetworkId), int64(1))
		connect.AssertEqual(t, RestoreTestBalance(ctx, sourceNetworkId), int64(0))
		connect.AssertEqual(t, GetActiveTransferBalanceByteCount(ctx, sourceNetworkId), before)
		_, err = CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1024)
		connect.AssertEqual(t, err, nil)

		// the audit row is kept
		count := 0
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT COUNT(*) FROM test_balance_drain WHERE network_id = $1 AND restore_time IS NOT NULL`, sourceNetworkId)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&count))
				}
			})
		})
		connect.AssertEqual(t, count, 1)
	})
}

func TestTestBalanceDrainVaultAllowlistDefaultsClosed(t *testing.T) {
	Testing_SetTestBalanceDrainAllowlist(nil)
	testBalanceDrainGates.snapshot.Store(nil)
	defer testBalanceDrainGates.snapshot.Store(nil)

	networkId := server.NewId()
	connect.AssertEqual(t, TestBalanceDrainAllowed(context.Background(), networkId), false)

	pop := server.Vault.PushSimpleResource(testBalanceDrainVaultResource, []byte("network_ids:\n  - "+networkId.String()+"\n"))
	testBalanceDrainGates.snapshot.Store(nil)
	connect.AssertEqual(t, TestBalanceDrainAllowed(context.Background(), networkId), true)
	connect.AssertEqual(t, TestBalanceDrainAllowed(context.Background(), server.NewId()), false)
	pop()

	// a malformed vault file disables the gate
	pop = server.Vault.PushSimpleResource(testBalanceDrainVaultResource, []byte("network_ids:\n  - "+networkId.String()+"\n  - nope\n"))
	testBalanceDrainGates.snapshot.Store(nil)
	connect.AssertEqual(t, TestBalanceDrainAllowed(context.Background(), networkId), false)
	pop()
}
