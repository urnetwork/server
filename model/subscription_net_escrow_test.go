package model

// The redis net-escrow counter mirrors a postgres-durable reservation. The
// reservation is committed in the escrow tx; the mirror is updated afterwards
// in a post. If the mirror update is bound to the caller's request context, a
// client that disconnects in that window desyncs the counter permanently:
// downward on a lost create (over-reporting available balance, seen as a
// negative residue), upward on a lost settle (hiding balance, the
// "insufficient balance" lockup).

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

type recordingNetEscrowReservationPageConfigurer struct {
	statements []string
	arguments  [][]any
}

func (self *recordingNetEscrowReservationPageConfigurer) Exec(
	_ context.Context,
	sql string,
	arguments ...any,
) (server.PgTag, error) {
	self.statements = append(self.statements, sql)
	self.arguments = append(self.arguments, append([]any(nil), arguments...))
	return server.PgTag{}, nil
}

// A large scalar-array predicate is not a durable access-path boundary: at
// production cardinality PostgreSQL planned every 10,000-balance page as a
// parallel scan of the complete transfer_escrow table. Pin the relational
// shape that makes the existing balance index lookup structural.
func TestNetEscrowReservationPageForcesPerBalanceIndexBoundary(t *testing.T) {
	for _, want := range []string{
		"FROM unnest($1::uuid[])",
		"CROSS JOIN LATERAL",
		"transfer_escrow.balance_id = requested_balance.balance_id",
		"transfer_escrow.settled = false",
		"OFFSET 0",
		"transfer_contract.outcome IS NULL",
	} {
		if !strings.Contains(netEscrowReservationPageSQL, want) {
			t.Fatalf("net-escrow reservation page lost planner boundary %q:\n%s", want, netEscrowReservationPageSQL)
		}
	}
	if strings.Contains(netEscrowReservationPageSQL, "balance_id = ANY") {
		t.Fatalf("net-escrow reservation page restored the full-scan-prone ANY shape:\n%s", netEscrowReservationPageSQL)
	}
	escrowStart := strings.Index(netEscrowReservationPageSQL, "FROM transfer_escrow")
	if escrowStart < 0 {
		t.Fatal("net-escrow reservation page lost its escrow subquery")
	}
	escrowSQL := netEscrowReservationPageSQL[escrowStart:]
	escrowEnd := strings.Index(escrowSQL, "AS selected_escrow")
	if escrowEnd < 0 {
		t.Fatal("net-escrow reservation page lost its escrow boundary")
	}
	escrowSQL = escrowSQL[:escrowEnd]
	if offset := strings.Index(escrowSQL, "OFFSET 0"); offset < 0 ||
		strings.Index(escrowSQL, "transfer_escrow.settled = false") > offset {
		t.Fatalf("unsettled prefilter escaped the lateral optimization boundary:\n%s", netEscrowReservationPageSQL)
	}
}

// A taskworker can disappear without delivering a cancellation to PostgreSQL.
// Keep each normally sub-second page independently fenced well inside the
// enclosing 30-minute task deadline, using transaction-local state only.
func TestNetEscrowReservationPageConfiguresServerSideTimeout(t *testing.T) {
	recorder := &recordingNetEscrowReservationPageConfigurer{}
	configureNetEscrowReservationPageTimeout(
		context.Background(),
		recorder,
		netEscrowReservationPageStatementTimeout,
	)

	if netEscrowReservationPageStatementTimeout != 2*time.Minute {
		t.Fatalf("reservation page timeout = %v, want 2m", netEscrowReservationPageStatementTimeout)
	}
	if len(recorder.statements) != 1 {
		t.Fatalf("reservation page timeout statements = %d, want 1", len(recorder.statements))
	}
	if got, want := recorder.statements[0], `SELECT set_config('statement_timeout', $1, true), set_config('jit', 'off', true)`; got != want {
		t.Fatalf("reservation page timeout statement = %q, want %q", got, want)
	}
	if len(recorder.arguments) != 1 || len(recorder.arguments[0]) != 1 {
		t.Fatalf("reservation page timeout argument shape = %v, want one argument", recorder.arguments)
	}
	if got, want := recorder.arguments[0][0], "120000ms"; got != want {
		t.Fatalf("reservation page timeout = %v, want %s", got, want)
	}
}

// Uses a held table lock as an explicit barrier: the guarded statement cannot
// complete, so PostgreSQL itself must cancel it. Rolling back the page
// transaction must also restore the pooled session's prior timeout.
func TestNetEscrowReservationPageTimeoutCancelsAndStaysLocal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		server.Db(ctx, func(lockConn server.PgConn) {
			lockTx, err := lockConn.Begin(ctx)
			server.Raise(err)
			lockReleased := false
			defer func() {
				if !lockReleased {
					_ = lockTx.Rollback(ctx)
				}
			}()
			server.RaisePgResult(lockTx.Exec(ctx, `LOCK TABLE transfer_contract IN ACCESS EXCLUSIVE MODE`))

			server.Db(ctx, func(blockedConn server.PgConn) {
				var baseline string
				server.Raise(blockedConn.QueryRow(ctx, `SHOW statement_timeout`).Scan(&baseline))

				pageTx, err := blockedConn.Begin(ctx)
				server.Raise(err)
				configureNetEscrowReservationPageTimeout(ctx, pageTx, 25*time.Millisecond)

				var configured string
				server.Raise(pageTx.QueryRow(ctx, `SHOW statement_timeout`).Scan(&configured))
				if configured != "25ms" {
					t.Fatalf("transaction statement timeout = %q, want 25ms", configured)
				}

				_, queryErr := pageTx.Exec(ctx, `SELECT count(*) FROM transfer_contract`)
				var pgErr *pgconn.PgError
				if !errors.As(queryErr, &pgErr) || pgErr.Code != pgerrcode.QueryCanceled {
					t.Fatalf("blocked page error = %v, want PostgreSQL query cancellation", queryErr)
				}
				server.Raise(pageTx.Rollback(ctx))

				var restored string
				server.Raise(blockedConn.QueryRow(ctx, `SHOW statement_timeout`).Scan(&restored))
				if restored != baseline {
					t.Fatalf("pooled statement timeout = %q after rollback, want baseline %q", restored, baseline)
				}
			})

			server.Raise(lockTx.Rollback(ctx))
			lockReleased = true
		})
	})
}

// The expiry-boundary repair must stay proportional to authoritative open
// escrow. Scanning all historical balances would turn one five-minute repair
// pass into a production-cardinality table walk.
func TestNetEscrowNoncurrentOpenBalancePageStaysIndexBounded(t *testing.T) {
	for _, want := range []string{
		"transfer_escrow.settled = false",
		"transfer_contract.outcome IS NULL",
		"NOT (",
		"transfer_balance.start_time <= $1 AND $1 < transfer_balance.end_time",
		"transfer_escrow.balance_id > $2",
		"GROUP BY transfer_escrow.balance_id",
		"ORDER BY transfer_escrow.balance_id",
		"LIMIT $3",
	} {
		if !strings.Contains(netEscrowNoncurrentOpenBalancePageSQL, want) {
			t.Fatalf("non-current open-escrow page lost bounded predicate %q:\n%s", want, netEscrowNoncurrentOpenBalancePageSQL)
		}
	}
	if strings.Contains(netEscrowNoncurrentOpenBalancePageSQL, "end_time +") {
		t.Fatalf("non-current open-escrow page restored a fixed grace window:\n%s", netEscrowNoncurrentOpenBalancePageSQL)
	}
}

// TestNetEscrowMirrorSurvivesCallerCancel requires the mirror to match the
// committed reservation once the create call returns, even though the caller's
// context is cancelled the moment it does.
//
// Today the mirror update runs synchronously inside the create (RunPosts before
// return), so this holds trivially. It is a guard, not a regression test: making
// the mirror update asynchronous or binding it to the caller's context would
// reopen the window where postgres holds a reservation the counter never
// recorded, and the eventual settle then decrements past zero (a negative
// residue, which over-reports available balance).
func TestNetEscrowMirrorSurvivesCallerCancel(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		netTransferByteCount := ByteCount(1024 * 1024 * 1024)
		contractByteCount := ByteCount(4 * 1024 * 1024)

		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationId := server.NewId()
		testingCreatePaymentClient(ctx, sourceNetworkId, sourceId)
		testingCreatePaymentClient(ctx, destinationNetworkId, destinationId)

		balanceCode, err := CreateBalanceCode(
			ctx,
			netTransferByteCount,
			365*24*time.Hour,
			UsdToNanoCents(10.00),
			"net-escrow-cancel",
			"",
			"",
		)
		connect.AssertEqual(t, err, nil)
		testingRedeemPaymentBalanceCode(t, ctx, sourceNetworkId, balanceCode.Secret)

		// the caller goes away as soon as the request returns: the contract is
		// committed, the mirror update is still outstanding
		cancelCtx, cancel := context.WithCancel(ctx)
		transferEscrow, err := CreateTransferEscrow(
			cancelCtx,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			contractByteCount,
		)
		cancel()
		connect.AssertEqual(t, err, nil)

		// the reservation is durable in postgres, so the mirror must reach the
		// same total. poll: the mirror update is asynchronous by design.
		var netEscrow ByteCount
		deadline := time.Now().Add(10 * time.Second)
		for {
			netEscrow = ByteCount(0)
			for _, balance := range transferEscrow.Balances {
				netEscrow += Testing_NetEscrowByteCount(ctx, balance.BalanceId)
			}
			if netEscrow == contractByteCount {
				break
			}
			if !time.Now().Before(deadline) {
				t.Fatalf(
					"net escrow mirror = %d, want %d: the mirror update was lost with the caller's context, so the settle will decrement a reservation the counter never recorded (negative residue)",
					netEscrow,
					contractByteCount,
				)
			}
			select {
			case <-time.After(100 * time.Millisecond):
			}
		}
	})
}

// TestNetEscrowReconcileRepairsExpiredBalanceWithOpenEscrow reproduces the
// 2026-09-01 UTC activation boundary. A balance stopped being current while
// contracts created in its final interval remained open for the close worker's
// grace period. The old current-window-only reconcile skipped the balance, so
// a lost reservation mirror could not be repaired before settlement released
// it and drove the counter negative.
func TestNetEscrowReconcileRepairsExpiredBalanceWithOpenEscrow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		sourceNetworkId := server.NewId()
		sourceClientId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationClientId := server.NewId()
		testingCreatePaymentClient(ctx, sourceNetworkId, sourceClientId)
		testingCreatePaymentClient(ctx, destinationNetworkId, destinationClientId)

		const balanceByteCount = ByteCount(1024 * 1024 * 1024)
		const contractByteCount = ByteCount(32 * 1024 * 1024)
		err := AddBasicTransferBalance(
			ctx,
			sourceNetworkId,
			balanceByteCount,
			now.Add(-2*time.Hour),
			now.Add(time.Hour),
		)
		connect.AssertEqual(t, err, nil)

		transferEscrow, err := createTransferEscrow(
			ctx,
			sourceNetworkId,
			sourceClientId,
			destinationNetworkId,
			destinationClientId,
			contractByteCount,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(transferEscrow.Balances), 1)
		balanceId := transferEscrow.Balances[0].BalanceId

		// Cross the balance boundary without closing the contract, then reproduce
		// the lost create mirror exposed by the production close cohort.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				UPDATE transfer_balance
				SET end_time = $2
				WHERE balance_id = $1
			`, balanceId, now.Add(-time.Minute)))
		}, server.TxReadCommitted)
		Testing_DeleteNetEscrow(ctx, balanceId)
		connect.AssertEqual(t, len(GetActiveTransferBalances(ctx, sourceNetworkId)), 0)

		driftByNetworkId, reconciledBalanceCount := ReconcileNetEscrow(ctx, true)
		connect.AssertEqual(t, reconciledBalanceCount, 1)
		connect.AssertEqual(t, driftByNetworkId[sourceNetworkId], -contractByteCount)
		connect.AssertEqual(t, Testing_NetEscrowByteCount(ctx, balanceId), contractByteCount)

		// The operator-targeted form must cover the same lifecycle set; otherwise
		// a directed repair would misleadingly report zero balances at the exact
		// boundary where the fleet pass now succeeds.
		Testing_DeleteNetEscrow(ctx, balanceId)
		targetedDrift, targetedBalanceCount := ReconcileNetEscrowForNetwork(ctx, sourceNetworkId, true)
		connect.AssertEqual(t, targetedBalanceCount, 1)
		connect.AssertEqual(t, targetedDrift, -contractByteCount)
		connect.AssertEqual(t, Testing_NetEscrowByteCount(ctx, balanceId), contractByteCount)
	})
}
