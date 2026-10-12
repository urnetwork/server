// Any network's bulk removal request reads the task queue for its global
// concurrency cap. On production that queue is tens of millions of rows, so
// these tests pin the admission read to an index path and to a bounded
// statement, observing the statements the real request issues.
package model

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Records every pending_task read a traced request issues, with arguments.
type bulkRemovalQueueReadTrace struct {
	stateLock sync.Mutex
	queries   []string
	args      [][]any
}

// Retains reads of the queue; writes and session statements are not reads.
func (self *bulkRemovalQueueReadTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(data.SQL)), "SELECT") && strings.Contains(data.SQL, "pending_task") {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.queries = append(self.queries, data.SQL)
		self.args = append(self.args, slices.Clone(data.Args))
	}
	return ctx
}

// Nothing to record after a statement.
func (self *bulkRemovalQueueReadTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// The queue reads observed so far.
func (self *bulkRemovalQueueReadTrace) snapshot() ([]string, [][]any) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.queries), slices.Clone(self.args)
}

// One more id than the synchronous path takes, so the request reaches the
// concurrency cap's queue read.
func bulkRemovalAsyncClientIdsForTest() []server.Id {
	clientIds := make([]server.Id, RemoveNetworkClientsBatchCount+1)
	for i := range clientIds {
		clientIds[i] = server.NewId()
	}
	return clientIds
}

// With sequential scans disabled, every queue read the request issued must
// still find an index path. The bare function_name count had none and read
// the whole queue in parallel on every admitted bulk removal request.
func TestRemoveNetworkClientsAdmissionReadUsesFunctionPollIndex(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		clientSession := &session.ClientSession{
			Ctx:   ctx,
			ByJwt: &session.ByJwt{NetworkId: networkId},
		}

		trace := &bulkRemovalQueueReadTrace{}
		scope, err := server.NewTestPgQueryScope(ctx, trace)
		server.Raise(err)
		defer func() {
			server.Raise(scope.Close())
		}()
		result, err := RemoveNetworkClients(&RemoveNetworkClientsArgs{ClientIds: bulkRemovalAsyncClientIdsForTest()}, clientSession)
		if err != nil || !result.Scheduled {
			t.Fatalf("bulk removal was not admitted: result=%+v err=%v", result, err)
		}
		server.Raise(scope.Close())

		queries, queryArgs := trace.snapshot()
		if len(queries) == 0 {
			t.Fatal("bulk removal admission issued no queue read")
		}
		for index, query := range queries {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `SET LOCAL enable_seqscan = off`))
				// counts accumulate in this backend until the transaction ends
				queueScanCount := func() (count int64) {
					server.Raise(tx.QueryRow(ctx, `SELECT pg_stat_get_xact_numscans('pending_task'::regclass)`).Scan(&count))
					return
				}
				queueScanCountBefore := queueScanCount()
				rows, err := tx.Query(ctx, query, queryArgs[index]...)
				server.Raise(err)
				for rows.Next() {
				}
				rows.Close()
				server.Raise(rows.Err())
				if queueScanCountAfter := queueScanCount(); queueScanCountAfter != queueScanCountBefore {
					t.Fatalf("bulk removal admission scanned the whole queue (sequential scans %d->%d): %s", queueScanCountBefore, queueScanCountAfter, strings.Join(strings.Fields(query), " "))
				}
			}, server.TxReadCommitted, pgx.ReadOnly)
		}
	})
}

// A queue that cannot be read within the bound refuses the request while the
// caller is still live, and releases the hourly reservation made for it.
// Another session's exclusive lock on the queue holds the read, which waited
// for the caller's whole deadline before the bound.
func TestRemoveNetworkClientsRefusesAndReleasesWhenAdmissionReadIsBounded(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		networkId := server.NewId()
		clientSession := &session.ClientSession{
			Ctx:   ctx,
			ByJwt: &session.ByJwt{NetworkId: networkId},
		}

		holder, err := server.AcquireMaintenanceDbConn(ctx)
		server.Raise(err)
		holderTx, err := holder.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite})
		if err != nil {
			holder.Release()
			t.Fatal(err)
		}
		released := false
		release := func() {
			if !released {
				released = true
				_ = holderTx.Rollback(context.Background())
				holder.Release()
			}
		}
		defer release()
		server.RaisePgResult(holderTx.Exec(ctx, `LOCK TABLE pending_task IN ACCESS EXCLUSIVE MODE`))

		result, err := RemoveNetworkClients(&RemoveNetworkClientsArgs{ClientIds: bulkRemovalAsyncClientIdsForTest()}, clientSession)
		var pgErr *pgconn.PgError
		if ctx.Err() != nil || !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.QueryCanceled || result != nil {
			t.Fatalf("bulk removal admission waited on the queue past its statement bound: caller=%v result=%+v err=%v", ctx.Err(), result, err)
		}
		release()

		// the refused request's reservation is released: the whole current
		// bucket is still available
		_, bucketStart, err := ReserveBulkClientRemovalSlot(ctx, server.NewId(), MaxBulkClientRemovalsPerBucket)
		if err != nil || !bucketStart.Equal(bulkClientRemovalBucketStart(server.NowUtc())) {
			t.Fatalf("refused bulk removal kept its reservation: bucket=%v err=%v", bucketStart, err)
		}
	})
}
