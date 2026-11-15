// A newly applied report's locked snapshot owns the decision to do no
// settlement work. Retried reports retain the ordinary fresh-read recovery.
package model

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

// One marked public call, including acquisition before any query is possible.
type closeReportPoolCall struct {
	acquisitions   atomic.Int64
	maxConnections atomic.Int64
	entered        chan struct{}
	reentered      chan struct{}
	atCapacity     atomic.Bool
}

type closeReportPoolCallKey struct{}

// Observes actual pool entry without changing its context, capacity or reply.
type closeReportPoolObserver struct{}

func (closeReportPoolObserver) TraceAcquireStart(ctx context.Context, pool *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	call, _ := ctx.Value(closeReportPoolCallKey{}).(*closeReportPoolCall)
	if call == nil {
		return ctx
	}
	count := call.acquisitions.Add(1)
	call.maxConnections.Store(int64(pool.Stat().MaxConns()))
	if count == 1 && call.entered != nil {
		close(call.entered)
	}
	if count == 2 && call.reentered != nil {
		stat := pool.Stat()
		call.atCapacity.Store(stat.MaxConns() == 1 && stat.AcquiredConns() == 1)
		close(call.reentered)
	}
	return ctx
}

func (closeReportPoolObserver) TraceAcquireEnd(context.Context, *pgxpool.Pool, pgxpool.TraceAcquireEndData) {
}

func (closeReportPoolObserver) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (closeReportPoolObserver) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// One adapter keeps exact report identities stable across a retry.
type closeReportPoolAdapter struct {
	name string
	call func(context.Context, server.Id, server.Id, ByteCount, bool, server.Id) (bool, error)
}

func closeReportPoolAdapters() []closeReportPoolAdapter {
	return []closeReportPoolAdapter{
		{name: "legacy", call: func(ctx context.Context, contractId, clientId server.Id, amount ByteCount, checkpoint bool, _ server.Id) (bool, error) {
			added, err := CloseContractUsage(ctx, contractId, clientId, amount, checkpoint)
			return added > 0, err
		}},
		{name: "client_legacy", call: func(ctx context.Context, contractId, clientId server.Id, amount ByteCount, checkpoint bool, _ server.Id) (bool, error) {
			added, err := CloseClientContractUsage(ctx, contractId, clientId, amount, checkpoint)
			return added > 0, err
		}},
		{name: "identified", call: CloseContractReport},
		{name: "original", call: func(ctx context.Context, contractId, clientId server.Id, amount ByteCount, checkpoint bool, reportId server.Id) (bool, error) {
			return CloseContractWithReport(ctx, ContractCloseReport{ContractId: contractId, ClientId: clientId, ReportId: reportId, AckedByteCount: amount, Checkpoint: checkpoint})
		}},
	}
}

// Owns a real main-pool checkout acquired only by the final report's committed
// Redis post. That checkout observes the close row and remains protocol-idle.
type closeReportCommittedPoolHold struct {
	contractId  server.Id
	party       ContractParty
	ctx         context.Context
	armed       atomic.Bool
	started     atomic.Bool
	valid       atomic.Bool
	held        chan struct{}
	done        chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
	err         error
	post        func()
}

func (self *closeReportCommittedPoolHold) DialHook(next redis.DialHook) redis.DialHook { return next }

func (self *closeReportCommittedPoolHold) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// The lifecycle post is dispatched after confirmed COMMIT and pool return.
// The holder's real read proves those boundaries before the report continues.
func (self *closeReportCommittedPoolHold) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		args := command.Args()
		if command.Name() == "eval" && len(args) > 9 && args[1] == contractHoleEventScript && args[8] == "remove" && args[9] == self.contractId.String() && self.armed.CompareAndSwap(true, false) {
			self.started.Store(true)
			ready := make(chan error, 1)
			go func() {
				defer close(self.done)
				signaled := false
				self.err = server.HandleError1(func() error {
					server.Db(self.ctx, func(conn server.PgConn) {
						var count int
						server.Raise(conn.QueryRow(self.ctx, `SELECT count(*) FROM contract_close
 WHERE contract_id=$1 AND party=$2 AND used_transfer_byte_count=11 AND NOT checkpoint`, self.contractId, self.party).Scan(&count))
						if count != 1 || conn.Conn().PgConn().TxStatus() != 'I' {
							panic(fmt.Errorf("holder did not observe one committed final outside a transaction: count=%d status=%c", count, conn.Conn().PgConn().TxStatus()))
						}
						self.valid.Store(true)
						close(self.held)
						ready <- nil
						signaled = true
						select {
						case <-self.release:
						case <-self.ctx.Done():
						}
					})
					return nil
				}, func(err error) error { return err })
				if !signaled {
					close(self.held)
					ready <- self.err
				}
			}()
			select {
			case err := <-ready:
				if err != nil {
					return err
				}
			case <-ctx.Done():
				return ctx.Err()
			}
			if self.post != nil {
				self.post()
			}
		}
		return next(ctx, command)
	}
}

// Release and join precede restoring the traced fixture pools.
func (self *closeReportCommittedPoolHold) close(t testing.TB) {
	t.Helper()
	self.armed.Store(false)
	if !self.started.Load() {
		return
	}
	self.releaseOnce.Do(func() { close(self.release) })
	select {
	case <-self.done:
		if self.err != nil {
			t.Error("committed pool holder failed", self.err)
		}
	case <-time.After(10 * time.Second):
		t.Error("committed pool holder did not join after release")
	}
}

// A first final commits, publishes revocation, and needs no further SQL. An
// opposing final started under the held checkout waits, then settles exactly
// once after release; only that settlement removes the stream membership.
func TestContractCloseFirstFinalDoesNotReacquirePool(t *testing.T) {
	for _, adapter := range closeReportPoolAdapters() {
		for _, party := range []ContractParty{ContractPartySource, ContractPartyDestination} {
			for _, cancelAfterCommit := range []bool{false, true} {
				func() {
					label := fmt.Sprintf("%s/%s/cancel_after_commit=%t", adapter.name, party, cancelAfterCommit)
					env := server.DefaultTestEnv()
					env.RerunCount = 0
					env.Run(t, func(t testing.TB) {
						t.Logf("case=%s", label)
						ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
						defer cancel()
						fixture := newNetEscrowOrderingTestFixture(t, ctx)
						contract := createRedisAdmissionTest(ctx, fixture, 100)
						AddToStream(ctx, contract.ContractId, fixture.sourceId, fixture.destinationId, nil)
						beforeStreamEvent := GetStreamEventId(ctx, fixture.sourceId)
						firstClientId, otherClientId := fixture.sourceId, fixture.destinationId
						if party == ContractPartyDestination {
							firstClientId, otherClientId = otherClientId, firstClientId
						}
						pop := server.Config.PushSimpleResource("db.yml", []byte("min_connections: 0\nmax_connections: 1\n"))
						server.PgReset()
						defer func() { pop(); server.PgReset() }()
						scope, err := server.NewTestPgQueryScope(ctx, closeReportPoolObserver{})
						server.Raise(err)
						defer func() { server.Raise(scope.Close()) }()
						holder := &closeReportCommittedPoolHold{contractId: contract.ContractId, party: party, ctx: ctx, held: make(chan struct{}), done: make(chan struct{}), release: make(chan struct{})}
						holder.armed.Store(true)
						defer holder.close(t)
						install, stopInstall := context.WithTimeout(ctx, time.Second)
						server.Raise(server.RedisWithDeadline(install, func(client server.RedisClient) error { client.AddHook(holder); return nil }))
						stopInstall()
						firstCtx, stopFirst := context.WithCancel(ctx)
						defer stopFirst()
						if cancelAfterCommit {
							holder.post = stopFirst
						}
						firstCall := &closeReportPoolCall{reentered: make(chan struct{})}
						firstCtx = context.WithValue(firstCtx, closeReportPoolCallKey{}, firstCall)
						firstDone := make(chan error, 1)
						go func() {
							firstDone <- server.HandleError1(func() error {
								_, err := adapter.call(firstCtx, contract.ContractId, firstClientId, 11, false, server.NewId())
								return err
							}, func(err error) error { return err })
						}()
						firstJoined := false
						defer func() {
							stopFirst()
							if !firstJoined {
								<-firstDone
							}
						}()
						select {
						case <-holder.held:
							if !holder.valid.Load() {
								t.Fatal(label, "pool holder did not observe the committed first final")
							}
						case <-ctx.Done():
							t.Fatal(label, "first report did not reach its committed Redis post", ctx.Err())
						}
						otherCall := &closeReportPoolCall{entered: make(chan struct{})}
						otherCtx := context.WithValue(ctx, closeReportPoolCallKey{}, otherCall)
						otherReportId := server.NewId()
						otherDone := make(chan error, 1)
						go func() {
							otherDone <- server.HandleError1(func() error {
								_, err := adapter.call(otherCtx, contract.ContractId, otherClientId, 11, false, otherReportId)
								return err
							}, func(err error) error { return err })
						}()
						otherJoined := false
						defer func() {
							holder.releaseOnce.Do(func() { close(holder.release) })
							if !otherJoined {
								<-otherDone
							}
						}()
						select {
						case <-otherCall.entered:
						case <-ctx.Done():
							t.Fatal(label, "opposing final did not request the held pool", ctx.Err())
						}
						select {
						case err := <-firstDone:
							firstJoined = true
							var wantErr error
							if cancelAfterCommit {
								wantErr = context.Canceled
							}
							if !errors.Is(err, wantErr) || firstCall.acquisitions.Load() != 1 || firstCall.maxConnections.Load() != 1 {
								t.Fatal(label, "committed first final did not finish with its sole report acquisition", err, firstCall.acquisitions.Load(), firstCall.maxConnections.Load())
							}
						case <-firstCall.reentered:
							stopFirst()
							err := <-firstDone
							firstJoined = true
							t.Fatalf("%s: committed first final reacquired the occupied pool for a no-op settlement: acquisitions=%d sole_slot_held=%t error=%v", label, firstCall.acquisitions.Load(), firstCall.atCapacity.Load(), err)
						case <-ctx.Done():
							t.Fatal(label, "committed first final did not finish", ctx.Err())
						}
						select {
						case err := <-otherDone:
							otherJoined = true
							t.Fatal(label, "opposing final completed while the sole checkout was held", err)
						default:
						}
						if _, _, found := GetStream(ctx, contract.ContractId); !found || GetStreamEventId(ctx, fixture.sourceId) != beforeStreamEvent {
							t.Fatal(label, "first final retired a still-open stream")
						}
						requireContractHoleCount(t, server.WithoutPostgres(ctx), fixture.sourceId, fixture.destinationId, 0)
						holder.close(t)
						select {
						case err := <-otherDone:
							otherJoined = true
							server.Raise(err)
						case <-ctx.Done():
							t.Fatal(label, "opposing final did not finish after pool release", ctx.Err())
						}
						if _, _, found := GetStream(ctx, contract.ContractId); found || GetStreamEventId(ctx, fixture.sourceId) != beforeStreamEvent+1 {
							t.Fatal(label, "opposing settlement did not remove its stream membership once")
						}
						_, retryErr := adapter.call(ctx, contract.ContractId, otherClientId, 11, false, otherReportId)
						if retryErr != nil && !isOnlyContractAlreadySettled(retryErr) {
							t.Fatal(label, "terminal retry changed its existing outcome", retryErr)
						}
						if GetStreamEventId(ctx, fixture.sourceId) != beforeStreamEvent+1 {
							t.Fatal(label, "terminal retry repeated stream retirement")
						}
						var exact bool
						server.Db(ctx, func(conn server.PgConn) {
							server.Raise(conn.QueryRow(ctx, `SELECT
 (SELECT outcome='settled' FROM transfer_contract WHERE contract_id=$1)
 AND (SELECT count(*)=1 AND sum(debit_byte_count)=11 AND bool_and(NOT applied) FROM transfer_debit_journal WHERE contract_id=$1)
 AND (SELECT balance_byte_count=1000 FROM transfer_balance WHERE balance_id=$2)`, contract.ContractId, fixture.balanceId).Scan(&exact))
						})
						if !exact || Testing_NetEscrowByteCount(ctx, fixture.balanceId) != 100 {
							t.Fatal(label, "opposing final or terminal retry changed deferred debit custody")
						}
						flushRedisAdmissionTestDebits(t, ctx, fixture.balanceId, 1, 1)
					})
				}()
			}
		}
	}
}

// A report may have committed before its owner lost the reply or reached
// settlement. An exact retry remains unknown and must settle two retained finals.
func TestContractCloseCommittedRetryStillReadsAndSettles(t *testing.T) {
	for _, adapter := range closeReportPoolAdapters() {
		func() {
			label := adapter.name
			env := server.DefaultTestEnv()
			env.RerunCount = 0
			env.Run(t, func(t testing.TB) {
				t.Logf("case=%s", label)
				ctx := t.Context()
				fixture := newNetEscrowOrderingTestFixture(t, ctx)
				contract := createRedisAdmissionTest(ctx, fixture, 100)
				sourceReportId, destinationReportId := server.NewId(), server.NewId()
				for _, report := range []ContractCloseReport{
					{ContractId: contract.ContractId, ClientId: fixture.sourceId, ReportId: sourceReportId, AckedByteCount: 11},
					{ContractId: contract.ContractId, ClientId: fixture.destinationId, ReportId: destinationReportId, AckedByteCount: 11},
				} {
					server.Tx(ctx, func(tx server.PgTx) {
						if adapter.name == "original" {
							applied, err := closeContractReportInTx(ctx, tx, report)
							if !applied || err != nil {
								t.Fatal(label, "report preparation did not commit", applied, err)
							}
						} else {
							var reportId *server.Id
							if adapter.name == "identified" {
								reportId = &report.ReportId
							}
							applied, _, err := applyContractCloseReportInTx(ctx, tx, report.ContractId, report.ClientId, report.AckedByteCount, false, reportId)
							if !applied || err != nil {
								t.Fatal(label, "report preparation did not commit", applied, err)
							}
						}
					}, server.TxReadCommitted, server.OptNoRetry())
				}
				scope, err := server.NewTestPgQueryScope(ctx, closeReportPoolObserver{})
				server.Raise(err)
				defer func() { server.Raise(scope.Close()) }()
				call := &closeReportPoolCall{}
				observed := context.WithValue(ctx, closeReportPoolCallKey{}, call)
				applied, err := adapter.call(observed, contract.ContractId, fixture.sourceId, 11, false, sourceReportId)
				if applied || err != nil || call.acquisitions.Load() < 2 {
					t.Fatal(label, "committed retry skipped fresh recovery or repeated its report", applied, err, call.acquisitions.Load())
				}
				var exact bool
				server.Db(ctx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(ctx, `SELECT
 (SELECT outcome='settled' FROM transfer_contract WHERE contract_id=$1)
 AND (SELECT count(*)=1 AND sum(debit_byte_count)=11 FROM transfer_debit_journal WHERE contract_id=$1)`, contract.ContractId).Scan(&exact))
				})
				if !exact {
					t.Fatal(label, "retained two-final reports did not settle exactly once on retry")
				}
				flushRedisAdmissionTestDebits(t, ctx, fixture.balanceId, 1, 1)
			})
		}()
	}
}

// Before a sender final, either party's new checkpoint cannot settle. The
// accepted public report therefore holds only its existing transaction checkout.
func TestContractCloseCheckpointDoesNotReacquirePool(t *testing.T) {
	for _, adapter := range closeReportPoolAdapters() {
		for _, party := range []ContractParty{ContractPartySource, ContractPartyDestination} {
			func() {
				label := adapter.name + "/" + string(party)
				env := server.DefaultTestEnv()
				env.RerunCount = 0
				env.Run(t, func(t testing.TB) {
					t.Logf("case=%s", label)
					ctx := t.Context()
					fixture := newNetEscrowOrderingTestFixture(t, ctx)
					contract := createRedisAdmissionTest(ctx, fixture, 100)
					scope, err := server.NewTestPgQueryScope(ctx, closeReportPoolObserver{})
					server.Raise(err)
					defer func() { server.Raise(scope.Close()) }()
					clientId := fixture.sourceId
					if party == ContractPartyDestination {
						clientId = fixture.destinationId
					}
					call := &closeReportPoolCall{}
					observed := context.WithValue(ctx, closeReportPoolCallKey{}, call)
					_, err = adapter.call(observed, contract.ContractId, clientId, 11, true, server.NewId())
					if err != nil || call.acquisitions.Load() != 1 {
						t.Fatal(label, "new checkpoint reacquired the pool for settlement it cannot perform", err, call.acquisitions.Load())
					}
				})
			}()
		}
	}
}
