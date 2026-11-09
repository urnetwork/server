package model

// Row-counted capacity (DESIGN.md 20.7's Counters): LockNetworkQuotaInTx and
// CheckCapacityInTx, and NetworkQuotaLimits, GET /sidecar/limits's body. The database
// tests need the test database and redis, through .stack/server-test.sh.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A table of rows a capacity counts, for the race: each test has its own database.
func testingCapacityTable(t testing.TB, ctx context.Context) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			CREATE TABLE IF NOT EXISTS quota_capacity_test_row (
				network_id uuid NOT NULL,
				row_id uuid NOT NULL,
				PRIMARY KEY (network_id, row_id)
			)
		`))
	})
}

// One creation as a caller does it: the lock, the count, the check, then the row, with
// hold run between the row and the commit. Returns the check's refusal.
func testingCreateCountedRow(ctx context.Context, networkId server.Id, lock bool, hold func()) (refusal error) {
	server.Tx(ctx, func(tx server.PgTx) {
		refusal = nil
		if lock {
			LockNetworkQuotaInTx(ctx, tx, networkId)
		}
		var usedCount int64
		server.Raise(tx.QueryRow(ctx, `SELECT count(*) FROM quota_capacity_test_row WHERE network_id = $1`, networkId).Scan(&usedCount))
		refusal = CheckCapacityInTx(ctx, tx, networkId, "dns_entries", usedCount)
		if refusal != nil {
			return
		}
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO quota_capacity_test_row (network_id, row_id) VALUES ($1, $2)`, networkId, server.NewId()))
		if hold != nil {
			hold()
		}
	}, server.TxReadCommitted, server.OptNoRetry())
	return
}

func testingCountedRows(ctx context.Context, networkId server.Id) (count int64) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM quota_capacity_test_row WHERE network_id = $1`, networkId).Scan(&count))
	})
	return
}

// Two transactions race for the last unit. The first holds its row uncommitted at a
// barrier while the second starts: under the network's lock the second counts only after
// the first commits, so exactly one passes. The twin without the lock lets both through.
func TestCheckCapacityRaceForTheLastUnit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer Testing_SetEnforceSidecarLimits(true)()
		defer Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{"dns_entries": 1})()
		defer Testing_SetSidecarLimits(NetworkTierPro, map[string]int64{"dns_entries": 200})()
		testingCapacityTable(t, ctx)

		race := func(lock bool) (first error, second error, networkId server.Id) {
			networkId = server.NewId()
			NetworkTierOf(ctx, networkId)
			inserted := make(chan struct{})
			release := make(chan struct{})
			firstDone := make(chan error, 1)
			go func() {
				firstDone <- testingCreateCountedRow(ctx, networkId, lock, func() {
					close(inserted)
					<-release
				})
			}()
			select {
			case <-inserted:
			case <-time.After(30 * time.Second):
				t.Fatal("the first creation did not insert")
			}
			secondDone := make(chan error, 1)
			go func() {
				secondDone <- testingCreateCountedRow(ctx, networkId, lock, nil)
			}()
			if !lock {
				// without the lock nothing holds the second back: it counts the
				// first's row as uncommitted and passes before the first commits
				select {
				case second = <-secondDone:
				case <-time.After(30 * time.Second):
					t.Fatal("the unlocked second creation did not finish")
				}
			}
			close(release)
			select {
			case first = <-firstDone:
			case <-time.After(30 * time.Second):
				t.Fatal("the first creation did not commit")
			}
			if lock {
				select {
				case second = <-secondDone:
				case <-time.After(30 * time.Second):
					t.Fatal("the locked second creation did not finish")
				}
			}
			return
		}

		first, second, networkId := race(true)
		connect.AssertEqual(t, first, nil)
		refusal := &QuotaRefusalError{}
		if !errors.As(second, &refusal) {
			t.Fatalf("second = %v, want a refusal", second)
		}
		connect.AssertEqual(t, refusal.Refusal.LimitKey, "dns_entries")
		connect.AssertEqual(t, refusal.Refusal.Limit, int64(1))
		connect.AssertEqual(t, refusal.Refusal.Used, int64(1))
		connect.AssertEqual(t, refusal.Refusal.Upgrade, true)
		connect.AssertEqual(t, testingCountedRows(ctx, networkId), int64(1))

		first, second, networkId = race(false)
		connect.AssertEqual(t, first, nil)
		connect.AssertEqual(t, second, nil)
		connect.AssertEqual(t, testingCountedRows(ctx, networkId), int64(2))
	})
}

// Dark, the check is nil at any count and does no I/O, the caller's transaction
// included. Enforced, zero refuses creation and none always passes.
func TestCheckCapacityDarkZeroAndNone(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		defer Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{"dns_entries": 0, "tls_endpoints": -1})()

		defer Testing_SetEnforceSidecarLimits(false)()
		attempts, stop := server.DenyPostgresForTest(t)
		for _, usedCount := range []int64{0, 1, 1_000_000} {
			connect.AssertEqual(t, CheckCapacityInTx(ctx, nil, networkId, "dns_entries", usedCount), nil)
			connect.AssertEqual(t, CheckCapacityInTx(ctx, nil, networkId, "no_such_limit", usedCount), nil)
		}
		stop()
		connect.AssertEqual(t, attempts(), uint64(0))

		Testing_SetEnforceSidecarLimits(true)
		server.Tx(ctx, func(tx server.PgTx) {
			err := CheckCapacityInTx(ctx, tx, networkId, "dns_entries", 0)
			refusal := &QuotaRefusalError{}
			if !errors.As(err, &refusal) || refusal.Refusal.Limit != 0 || refusal.Refusal.Used != 0 {
				t.Fatalf("zero: %v", err)
			}
			for _, usedCount := range []int64{0, 1, 1_000_000} {
				connect.AssertEqual(t, CheckCapacityInTx(ctx, tx, networkId, "tls_endpoints", usedCount), nil)
			}
			// a key this build does not know reads zero, so it refuses
			if err := CheckCapacityInTx(ctx, tx, networkId, "no_such_limit", 0); !errors.As(err, &refusal) {
				t.Fatalf("an unknown key passed: %v", err)
			}
		})
	})
}

// GET /sidecar/limits's body: the tier first, then every key's limit, the sidecar_abuse:
// keys, directory_* among them, included.
func TestNetworkQuotaLimitsBody(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{
			"sidecar_clients":            20,
			"directory_requests_per_day": 50,
			"directory_card_bytes":       16 * 1024,
		})()
		defer Testing_SetSidecarLimits(NetworkTierPro, map[string]int64{"sidecar_clients": 200})()

		free := server.NewId()
		limits := NetworkQuotaLimits(ctx, free)
		connect.AssertEqual(t, limits.Tier, "free")
		connect.AssertEqual(t, limits.Limits["sidecar_clients"], int64(20))
		connect.AssertEqual(t, limits.Limits["directory_requests_per_day"], int64(50))
		connect.AssertEqual(t, limits.Limits["directory_card_bytes"], int64(16*1024))
		connect.AssertEqual(t, len(limits.Limits), len(proSidecarLimitRows))

		pro := server.NewId()
		now := server.NowUtc()
		testingAddTierBalance(t, ctx, pro, true, now.Add(-time.Hour), now.Add(time.Hour))
		UpdateProNetwork(ctx, pro)
		limits = NetworkQuotaLimits(ctx, pro)
		connect.AssertEqual(t, limits.Tier, "pro")
		connect.AssertEqual(t, limits.Limits["sidecar_clients"], int64(200))
		connect.AssertEqual(t, limits.Limits["directory_requests_per_day"], int64(50))
	})
}
