// Exercises pool liveness on the real pgx wire protocol without a database.
package server

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Owns every synthetic connection and joins its protocol reader on cleanup.
// Optional query barriers control the exact failure boundary, not wall time.
type pgPoolWireFixture struct {
	stateLock   sync.Mutex
	connections []net.Conn
	queries     []string
	dialCount   int
	pingCount   int
	closed      bool
	workers     sync.WaitGroup
	query       func(int, string) bool
	queryError  func(int, string) *pgproto3.ErrorResponse
	// the number of coming commits to answer with a rollback, as postgres does
	// for a transaction aborted by an error the client never received
	commitRollbacks atomic.Int32
}

// Whether this commit is one of the rollbacks a test asked for.
func (self *pgPoolWireFixture) takeCommitRollback() bool {
	for {
		count := self.commitRollbacks.Load()
		if count <= 0 {
			return false
		}
		if self.commitRollbacks.CompareAndSwap(count, count-1) {
			return true
		}
	}
}

// Supplies a synthetic PostgreSQL startup and simple-query transport.
func (self *pgPoolWireFixture) dial(ctx context.Context, network string, address string) (net.Conn, error) {
	client, peer := net.Pipe()
	self.stateLock.Lock()
	if self.closed {
		self.stateLock.Unlock()
		_ = client.Close()
		_ = peer.Close()
		return nil, net.ErrClosed
	}
	self.connections = append(self.connections, client, peer)
	self.workers.Add(1)
	self.stateLock.Unlock()
	go func() {
		defer self.workers.Done()
		defer peer.Close()
		backend := pgproto3.NewBackend(peer, peer)
		message, err := backend.ReceiveStartupMessage()
		if err != nil {
			return
		}
		// pgx can open a separate CancelRequest transport while disposing a
		// failed session. It is not another authenticated pool connection.
		if _, ok := message.(*pgproto3.StartupMessage); !ok {
			return
		}
		self.stateLock.Lock()
		self.dialCount += 1
		connectionIndex := self.dialCount
		self.stateLock.Unlock()
		backend.Send(&pgproto3.AuthenticationOk{})
		backend.Send(&pgproto3.ParameterStatus{Name: "server_version", Value: "18.0"})
		backend.Send(&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"})
		backend.Send(&pgproto3.ParameterStatus{Name: "standard_conforming_strings", Value: "on"})
		backend.Send(&pgproto3.BackendKeyData{ProcessID: uint32(connectionIndex), SecretKey: []byte{0, 0, 0, 1}})
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		if err := backend.Flush(); err != nil {
			return
		}
		txStatus := byte('I')
		for {
			message, err := backend.Receive()
			if err != nil {
				return
			}
			switch message := message.(type) {
			case *pgproto3.Query:
				self.stateLock.Lock()
				self.queries = append(self.queries, message.String)
				if message.String == "-- ping" {
					self.pingCount += 1
				}
				self.stateLock.Unlock()
				if self.query != nil && !self.query(connectionIndex, message.String) {
					return
				}
				// like postgres, an aborted transaction refuses every statement
				// but its end
				if txStatus == 'E' && message.String != "commit" && message.String != "rollback" {
					backend.Send(&pgproto3.ErrorResponse{Severity: "ERROR", Code: "25P02", Message: "current transaction is aborted, commands ignored until end of transaction block"})
					backend.Send(&pgproto3.ReadyForQuery{TxStatus: txStatus})
					if err := backend.Flush(); err != nil {
						return
					}
					continue
				}
				if self.queryError != nil {
					if err := self.queryError(connectionIndex, message.String); err != nil {
						if message.String == "commit" {
							txStatus = 'I'
						} else if txStatus == 'T' {
							txStatus = 'E'
						}
						backend.Send(err)
						backend.Send(&pgproto3.ReadyForQuery{TxStatus: txStatus})
						if err := backend.Flush(); err != nil {
							return
						}
						continue
					}
				}
				commandTag := "SELECT 1"
				switch {
				case message.String == "-- ping":
					backend.Send(&pgproto3.EmptyQueryResponse{})
				case strings.HasPrefix(message.String, "begin"):
					txStatus, commandTag = 'T', "BEGIN"
					backend.Send(&pgproto3.CommandComplete{CommandTag: []byte(commandTag)})
				case message.String == "commit" && (txStatus == 'E' || self.takeCommitRollback()):
					// postgres ends an aborted transaction's commit in a rollback
					txStatus = 'I'
					backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("ROLLBACK")})
				case message.String == "commit" || message.String == "rollback":
					txStatus = 'I'
					backend.Send(&pgproto3.CommandComplete{CommandTag: []byte(strings.ToUpper(message.String))})
				default:
					backend.Send(&pgproto3.CommandComplete{CommandTag: []byte(commandTag)})
				}
				backend.Send(&pgproto3.ReadyForQuery{TxStatus: txStatus})
				if err := backend.Flush(); err != nil {
					return
				}
			case *pgproto3.Terminate:
				return
			default:
				return
			}
		}
	}()
	return client, nil
}

// Uses one actual pgx pool connection; no production config or network is read.
func newPgPoolWireFixture(t testing.TB, query func(int, string) bool, shouldPing func(context.Context, pgxpool.ShouldPingParams) bool, configure ...func(*pgPoolWireFixture, *pgxpool.Config)) (*pgPoolWireFixture, *safePgPool) {
	t.Helper()
	fixture := &pgPoolWireFixture{query: query}
	config, err := pgxpool.ParseConfig("host=synthetic-pg.example user=synthetic dbname=synthetic sslmode=disable connect_timeout=5")
	if err != nil {
		t.Fatal(err)
	}
	config.MinConns, config.MaxConns = 0, 1
	config.ConnConfig.DialFunc = fixture.dial
	config.ConnConfig.LookupFunc = func(ctx context.Context, host string) ([]string, error) {
		return []string{"192.0.2.1"}, nil
	}
	config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	for _, f := range configure {
		f(fixture, config)
	}
	configurePgPoolLiveness(config)
	configurePgPoolWriteTracking(config)
	configurePgPoolStatementErrors(config)
	config.ShouldPing = shouldPing
	pgPool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	pool := &safePgPool{pool: pgPool}
	t.Cleanup(func() {
		fixture.stateLock.Lock()
		fixture.closed = true
		connections := fixture.connections
		fixture.stateLock.Unlock()
		for _, conn := range connections {
			_ = conn.Close()
		}
		pool.close()
		fixture.workers.Wait()
	})
	return fixture, pool
}

// Production must retain pgx's idle policy and bound both initial and idle
// validations. This observes the exact configuration used by the wire fixture.
func TestPgPoolLivenessConfigPreservesBoundedIdleChecks(t *testing.T) {
	config, err := pgxpool.ParseConfig("host=synthetic-pg.example user=synthetic dbname=synthetic sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	configurePgPoolLiveness(config)
	if config.PingTimeout != PgPingTimeout || config.ShouldPing != nil || config.AfterConnect == nil {
		t.Fatal("pool liveness lost initial validation, bounded idle Ping, or pgx idle policy")
	}
}

// Successful first-use validation is retained, but eight immediately reusable
// operations must not manufacture eight additional pooler round trips.
func TestDbHotAcquireHasOnlyInitialWirePing(t *testing.T) {
	fixture, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false })
	for range 8 {
		dbWithPool(context.Background(), pool, func(conn PgConn) {
			RaisePgResult(conn.Exec(context.Background(), "SELECT 1"))
		}, OptNoRetry())
	}
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	if fixture.pingCount != 1 || fixture.dialCount != 1 || len(fixture.queries) != 9 {
		t.Fatalf("wire pings=%d dials=%d queries=%d, want1,1,9", fixture.pingCount, fixture.dialCount, len(fixture.queries))
	}
}

// An explicit barrier models a pooler that stalls only redundant pings. A hot
// checkout must reach its callback without waiting for that unrelated reply.
func TestDbHotAcquireDoesNotWaitForRedundantPing(t *testing.T) {
	redundantPing := make(chan struct{})
	releasePing := make(chan struct{})
	var closeRelease sync.Once
	defer closeRelease.Do(func() { close(releasePing) })
	pingCount := 0
	_, pool := newPgPoolWireFixture(t, func(connectionIndex int, query string) bool {
		if query == "-- ping" {
			pingCount += 1
			if pingCount == 2 {
				close(redundantPing)
				<-releasePing
			}
		}
		return true
	}, func(context.Context, pgxpool.ShouldPingParams) bool { return false })
	warm, err := pool.open().Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	warm.Release()
	entered := make(chan struct{})
	done := make(chan any, 1)
	go func() {
		done <- captureDbErrorPanic(func() {
			dbWithPool(context.Background(), pool, func(conn PgConn) { close(entered) }, OptNoRetry())
		})
	}()
	select {
	case <-redundantPing:
		t.Error("hot callback waited behind a redundant PostgreSQL Ping")
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Error("synthetic checkout failed to reach either explicit barrier")
	}
	closeRelease.Do(func() { close(releasePing) })
	if recovered := <-done; recovered != nil {
		t.Fatalf("checkout panic=%v", recovered)
	}
}

// A freshly authenticated socket is not sufficient: its initial query Ping
// must succeed before the first application callback can execute.
func TestPgPoolFirstUsePingFailurePreventsCallback(t *testing.T) {
	fixture, pool := newPgPoolWireFixture(t, func(connectionIndex int, query string) bool { return query != "-- ping" }, nil)
	callbacks := 0
	recovered := captureDbErrorPanic(func() {
		dbWithPool(context.Background(), pool, func(conn PgConn) { callbacks += 1 }, OptNoRetry())
	})
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	if recovered == nil || callbacks != 0 || fixture.pingCount != 1 {
		t.Fatalf("first-use failure=%v callbacks=%d pings=%d, want error,0,1", recovered, callbacks, fixture.pingCount)
	}
}

// Force pgx's stale-idle branch without sleeps; a failed health query must
// discard the old connection before passing a fresh, validated one onward.
func TestPgPoolIdlePingReplacesStaleConnection(t *testing.T) {
	var forceIdle atomic.Bool
	firstConnectionPings := 0
	fixture, pool := newPgPoolWireFixture(t, func(connectionIndex int, query string) bool {
		if connectionIndex == 1 && query == "-- ping" {
			firstConnectionPings += 1
			return firstConnectionPings != 2
		}
		return true
	}, func(context.Context, pgxpool.ShouldPingParams) bool { return forceIdle.Swap(false) })
	warm, err := pool.open().Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	oldPid := warm.Conn().PgConn().PID()
	warm.Release()
	forceIdle.Store(true)
	var usedPid uint32
	dbWithPool(context.Background(), pool, func(conn PgConn) { usedPid = conn.Conn().PgConn().PID() }, OptNoRetry())
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	if usedPid == oldPid || fixture.dialCount != 2 || fixture.pingCount != 3 {
		t.Fatalf("stale replacement same=%t dials=%d pings=%d, want false,2,3", usedPid == oldPid, fixture.dialCount, fixture.pingCount)
	}
}

// A healthy idle connection gets one health query and stays reusable; stale
// validation is not permission to churn otherwise healthy pool connections.
func TestPgPoolHealthyIdlePingRetainsConnection(t *testing.T) {
	var forceIdle atomic.Bool
	fixture, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return forceIdle.Swap(false) })
	warm, err := pool.open().Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	oldPid := warm.Conn().PgConn().PID()
	warm.Release()
	forceIdle.Store(true)
	var usedPid uint32
	dbWithPool(context.Background(), pool, func(conn PgConn) { usedPid = conn.Conn().PgConn().PID() }, OptNoRetry())
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	if usedPid != oldPid || fixture.dialCount != 1 || fixture.pingCount != 2 {
		t.Fatalf("healthy idle same=%t dials=%d pings=%d, want true,1,2", usedPid == oldPid, fixture.dialCount, fixture.pingCount)
	}
}

// A caller cancellation must interrupt a blocked stale-idle validation, not
// wait for the peer to reply or allow an application callback onto the socket.
func TestPgPoolIdlePingCancelsBlockedWire(t *testing.T) {
	var forceIdle atomic.Bool
	idlePing := make(chan struct{})
	releasePing := make(chan struct{})
	var closeRelease sync.Once
	defer closeRelease.Do(func() { close(releasePing) })
	pingCount := 0
	_, pool := newPgPoolWireFixture(t, func(connectionIndex int, query string) bool {
		if query == "-- ping" {
			pingCount += 1
			if pingCount == 2 {
				close(idlePing)
				<-releasePing
			}
		}
		return true
	}, func(context.Context, pgxpool.ShouldPingParams) bool { return forceIdle.Swap(false) })
	warm, err := pool.open().Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	warm.Release()
	forceIdle.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		conn, err := pool.open().Acquire(ctx)
		if err == nil {
			conn.Release()
		}
		done <- err
	}()
	select {
	case <-idlePing:
		cancel()
	case <-time.After(5 * time.Second):
		t.Error("synthetic idle validation did not reach the query barrier")
		cancel()
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("blocked idle Ping error=%v, want canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("caller cancellation did not interrupt blocked idle Ping")
	}
	closeRelease.Do(func() { close(releasePing) })
}

// Removing a redundant health query must not bypass caller cancellation.
func TestDbHotAcquirePreservesCanceledCaller(t *testing.T) {
	_, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false })
	warm, err := pool.open().Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	warm.Release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	callbacks := 0
	recovered := captureDbErrorPanic(func() {
		dbWithPool(ctx, pool, func(conn PgConn) { callbacks += 1 }, OptNoRetry())
	})
	recoveredErr, ok := recovered.(error)
	if !ok || !errors.Is(recoveredErr, context.Canceled) || callbacks != 0 {
		t.Fatalf("canceled acquire=%v callbacks=%d, want canceled,0", recovered, callbacks)
	}
}
