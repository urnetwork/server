package model

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// Every bounded read of an award.
var operatorBlockPointsBoundedReads = []string{
	"traffic",
	"reliability_blocks",
	"reliability_partials",
	"client_locations",
	"reliability_weights",
	"reliability_multipliers",
}

// The dry run's failure on Main, reproduced (2026-10-29: "FATAL: terminating
// connection due to idle-in-transaction timeout (SQLSTATE 25P03)", 2.5 h into
// block 15). A read's rows wait in socket buffers after the server has sent
// them, and the dry run's client, on a loaded host through a tunnel, drained
// a reliability read's rows for longer than Main's 60 s
// idle_in_transaction_session_timeout while the read's transaction was open.
// Here the database's timeout is 100 ms and the client stalls 2 s after the
// first read of each kind; the award completes, and it is the award computed
// without stalls: the reads, and so the digest the commit checks, are the
// same.
func TestOperatorBlockPointsAwardOutlastsIdleInTransactionTimeout(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		block := newOperatorBlockPointsReadFixture(t, ctx, now)
		config := operatorBlockPointsTestConfig()
		baseline, err := ComputeOperatorBlockPoints(ctx, config, block, now)
		if err != nil {
			t.Fatal(err)
		}

		// for every session the pools open from now on
		setIdleInTransactionTimeout := func(setting string) {
			server.Db(ctx, func(conn server.PgConn) {
				var database string
				server.Raise(conn.QueryRow(ctx, `SELECT current_database()`).Scan(&database))
				server.RaisePgResult(conn.Exec(ctx, `ALTER DATABASE `+pgx.Identifier{database}.Sanitize()+` `+setting))
			}, server.OptReadWrite())
			server.PgReset()
		}
		setIdleInTransactionTimeout(`SET idle_in_transaction_session_timeout = '100ms'`)
		defer setIdleInTransactionTimeout(`RESET idle_in_transaction_session_timeout`)

		var stateLock sync.Mutex
		reads := map[string]int{}
		testingBoundedReadDone = func(read string) {
			stateLock.Lock()
			reads[read] += 1
			first := reads[read] == 1
			stateLock.Unlock()
			if first {
				time.Sleep(2 * time.Second)
			}
		}
		defer func() {
			testingBoundedReadDone = nil
		}()

		// as the dry run runs it
		server.RequirePgReadOnly()
		award, err := ComputeOperatorBlockPoints(ctx, config, block, now)
		server.Testing_ReleasePgReadOnly()
		if err != nil {
			t.Fatal(err)
		}
		for _, read := range operatorBlockPointsBoundedReads {
			if reads[read] == 0 {
				t.Fatalf("the award ran no %s read: %v", read, reads)
			}
		}
		for read := range reads {
			if !slices.Contains(operatorBlockPointsBoundedReads, read) {
				t.Fatalf("unknown read %s", read)
			}
		}
		if award.Digest() != baseline.Digest() || award.ReliabilityNetworkCount != 1 || award.SweepCount != 1 {
			t.Fatalf("award %+v, without stalls %+v", award, baseline)
		}

		// the setting held: a transaction left idle past it is ended
		var idleErr any
		func() {
			defer func() {
				idleErr = recover()
			}()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `SELECT 1`))
				time.Sleep(time.Second)
			}, server.OptNoRetry())
		}()
		if err, ok := idleErr.(error); !ok || !strings.Contains(err.Error(), "25P03") {
			t.Fatalf("a transaction idle past the timeout was not ended: %v", idleErr)
		}
	})
}
