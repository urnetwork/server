package model

// Direct model writes must opt in even when a pooled session defaults to read-only.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/server/v2026"
)

// Pins one disposable test connection, proves its read-only failure, then checks
// the real model path used that same session and explicitly made it writable.
func withReadOnlyModelDbSession(t testing.TB, ctx context.Context, do func()) {
	t.Helper()
	popConfig := server.Config.PushSimpleResource(
		server.DefaultPgConfigResourceName,
		[]byte("min_connections: 0\nmax_connections: 1\n"),
	)
	server.PgReset()
	defer func() {
		popConfig()
		server.PgReset()
	}()

	var backendPid int32
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(ctx, "SET default_transaction_read_only = on"))
		server.Raise(conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&backendPid))
		_, err := conn.Exec(ctx, "UPDATE network_client SET contract_time = NULL WHERE false")
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
			t.Fatal("read-only negative control did not reject a direct UPDATE with SQLSTATE 25006")
		}
	}, server.OptNoRetry())

	do()

	server.Db(ctx, func(conn server.PgConn) {
		var observedPid int32
		var readOnly string
		server.Raise(conn.QueryRow(ctx, "SELECT pg_backend_pid(), current_setting('default_transaction_read_only')").Scan(&observedPid, &readOnly))
		if observedPid != backendPid {
			t.Fatal("model write used a replaced test session; read-only regression is not proven")
		}
		if readOnly != "off" {
			t.Fatal("model write did not explicitly make the read-only test session writable")
		}
	}, server.OptNoRetry())
}

// Both a direct payer and its child must stamp the top-level row after the
// read-only negative control; the child is never counted as another block user.
func TestStampTopLevelClientContractTimeReadOnlySession(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		parentId := server.NewId()
		childId := server.NewId()
		networkId := server.NewId()
		staleTime := server.NowUtc().Add(-2 * time.Hour).Truncate(time.Second)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO network_client (
					client_id, network_id, active, create_time, auth_time, source_client_id
				) VALUES
					($1, $3, true, $4, $4, NULL),
					($2, $3, true, $4, $4, $1)
			`, parentId, childId, networkId, staleTime))
		})
		Testing_ResetContractTimeStampGate()
		defer Testing_ResetContractTimeStampGate()

		for _, payerId := range []server.Id{parentId, childId} {
			testSetNetworkClientContractTime(ctx, parentId, staleTime)
			Testing_ResetContractTimeStampGate()
			withReadOnlyModelDbSession(t, ctx, func() {
				StampTopLevelClientContractTime(ctx, payerId)
				stampedTime := testGetNetworkClientContractTime(ctx, parentId)
				if stampedTime == nil || !staleTime.Add(time.Hour).Before(*stampedTime) {
					t.Fatal("read-only session prevented the top-level contract-usage stamp")
				}
				if testGetNetworkClientContractTime(ctx, childId) != nil {
					t.Fatal("child contract usage stamped a second identity")
				}
				if CountTopLevelClientsWithContractSince(ctx, staleTime.Add(time.Hour)) != 1 {
					t.Fatal("contract usage did not count exactly one top-level identity")
				}

				// Reset only the process gate to exercise the unchanged SQL throttle.
				Testing_ResetContractTimeStampGate()
				StampTopLevelClientContractTime(ctx, payerId)
				unmovedTime := testGetNetworkClientContractTime(ctx, parentId)
				if unmovedTime == nil || !unmovedTime.Equal(*stampedTime) {
					t.Fatal("write intent changed the contract-usage throttle")
				}
			})
		}
	})
}

// The best-effort post-commit stamp must still contain a canceled write rather
// than turn an already-created contract into a caller-visible failure.
func TestStampTopLevelClientContractTimeCanceledIsNonfatal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		Testing_ResetContractTimeStampGate()
		defer Testing_ResetContractTimeStampGate()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		StampTopLevelClientContractTime(ctx, server.NewId())
	})
}
