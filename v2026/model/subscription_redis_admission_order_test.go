// Driver observations preserve main's ordered admission ownership boundaries.
package model

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

// Count actual driver sends; discovery and financial insertion remain distinct.
type redisAdmissionReadTrace struct {
	*redisPublicationObserver
	stateLock     sync.Mutex
	readBatches   int
	separateReads int
}

func (self *redisAdmissionReadTrace) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	ctx = self.redisPublicationObserver.TraceQueryStart(ctx, conn, data)
	if ctx.Value(self.redisPublicationObserver) == true {
		sql := strings.Join(strings.Fields(data.SQL), " ")
		if strings.Contains(sql, "pg_advisory_xact_lock(hashtextextended") || strings.HasPrefix(sql, "SELECT EXISTS(SELECT 1 FROM transfer_contract") || strings.Contains(sql, "FOR SHARE") || strings.Contains(sql, "contract_lifecycle_write_boundary") || strings.HasPrefix(sql, "SELECT count(*)=$2 FROM transfer_balance") {
			self.stateLock.Lock()
			self.separateReads++
			self.stateLock.Unlock()
		}
	}
	return ctx
}

func (self *redisAdmissionReadTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	if ctx.Value(self.redisPublicationObserver) == true && len(data.Batch.QueuedQueries) >= 2 && strings.Contains(data.Batch.QueuedQueries[0].SQL, "pg_advisory_xact_lock(hashtextextended") && strings.Contains(data.Batch.QueuedQueries[1].SQL, "SELECT EXISTS(SELECT 1 FROM transfer_contract") {
		self.stateLock.Lock()
		self.readBatches++
		self.stateLock.Unlock()
	}
	return ctx
}
func (self *redisAdmissionReadTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
}
func (self *redisAdmissionReadTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {
}

// Main's independent checks accept each result before later work is dispatched.
// Held-owner refusal controls separately prove why these boundaries matter.
func TestRedisAdmissionPreservesMainReadBoundaries(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, fixture, 17)
		trace := &redisAdmissionReadTrace{redisPublicationObserver: &redisPublicationObserver{}}
		scope, err := server.NewTestPgQueryScope(ctx, trace)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()
		observed := context.WithValue(ctx, trace.redisPublicationObserver, true)
		escrow, err := CreateTransferEscrow(observed, fixture.sourceNetworkId, fixture.sourceId, fixture.destinationNetworkId, fixture.destinationId, 23)
		if err != nil || escrow == nil {
			t.Fatal("public creation failed", err)
		}
		requireRedisPublicationCustody(t, ctx, fixture, neighbor.ContractId, escrow.ContractId, 40, 2)
		trace.stateLock.Lock()
		batches, separate := trace.readBatches, trace.separateReads
		trace.stateLock.Unlock()
		if batches != 0 || separate != 5 {
			t.Fatalf("admission driver sends: read_batches=%d separate_reads=%d; want 0/5", batches, separate)
		}
		trace.redisPublicationObserver.require(t, 1+redisPublicationDiscoveryAcquisitions, 0)
	})
}
