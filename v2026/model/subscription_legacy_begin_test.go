// Count actual pgx protocol cycles and exercise the financial owner's local
// settings. Artificial transport latency is a causal control, not a fleet rate.
package model

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urnetwork/server/v2026"
)

const legacyBeginSettingsSql = `SELECT
 current_setting('statement_timeout')='2s' AND current_setting('lock_timeout')='250ms'
 AND current_setting('transaction_isolation')='read committed'
 AND current_setting('transaction_read_only')='off' AND current_setting('transaction_deferrable')='off'`

// Retains only message headers and finite counts, never credentials, SQL or
// parameter payloads. A socket read/write need not align with a protocol frame.
type legacyBeginWireFrames struct {
	header        [5]byte
	headerBytes   int
	payloadBytes  int
	messageCounts [256]uint64
	invalid       bool
}

// Skip each payload while preserving boundaries across fragmented I/O.
func (self *legacyBeginWireFrames) add(p []byte) {
	for len(p) > 0 && !self.invalid {
		if self.headerBytes < len(self.header) {
			n := copy(self.header[self.headerBytes:], p)
			self.headerBytes += n
			p = p[n:]
			if self.headerBytes < len(self.header) {
				return
			}
			length := binary.BigEndian.Uint32(self.header[1:])
			if length < 4 {
				self.invalid = true
				return
			}
			self.payloadBytes = int(length - 4)
		}
		n := min(self.payloadBytes, len(p))
		self.payloadBytes -= n
		p = p[n:]
		if self.payloadBytes == 0 {
			self.messageCounts[self.header[0]]++
			self.headerBytes = 0
		}
	}
}

// One test owns these counters and all sockets. Observation starts only after
// startup and ends after every worker has returned its pooled connection.
type legacyBeginWireControl struct {
	enabled   atomic.Bool
	ctx       context.Context
	delay     time.Duration
	stateLock sync.Mutex
	queries   uint64
	ready     uint64
	invalid   bool
	acquiring chan struct{}
}

// Shares the normal query context; acquisition notifications use no DB work.
func (self *legacyBeginWireControl) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

// Query results are observed on the wire instead of relying on tracer labels.
func (self *legacyBeginWireControl) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// Every initial worker reaches the real pool acquisition boundary while the
// fixture still owns every slot. No sleep or queue-length polling establishes it.
func (self *legacyBeginWireControl) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	if self.enabled.Load() {
		select {
		case self.acquiring <- struct{}{}:
		default:
		}
	}
	return ctx
}

// The native pool's own counters retain actual wait and checkout outcomes.
func (self *legacyBeginWireControl) TraceAcquireEnd(context.Context, *pgxpool.Pool, pgxpool.TraceAcquireEndData) {
}

// Counts frontend Query/Sync and backend ReadyForQuery frames. Delays only
// request writes after startup; the PostgreSQL server and pgx pool stay real.
type legacyBeginWireConn struct {
	net.Conn
	control   *legacyBeginWireControl
	writeLock sync.Mutex
	readLock  sync.Mutex
	frontend  legacyBeginWireFrames
	backend   legacyBeginWireFrames
}

// Count only bytes accepted by the real socket, including partial writes.
func (self *legacyBeginWireConn) Write(p []byte) (int, error) {
	self.writeLock.Lock()
	defer self.writeLock.Unlock()
	observed := self.control.enabled.Load()
	if observed && self.control.delay > 0 {
		select {
		case <-time.After(self.control.delay):
		case <-self.control.ctx.Done():
			return 0, self.control.ctx.Err()
		}
	}
	n, err := self.Conn.Write(p)
	if observed {
		before := self.frontend.messageCounts['Q'] + self.frontend.messageCounts['S']
		self.frontend.add(p[:n])
		self.control.stateLock.Lock()
		self.control.queries += self.frontend.messageCounts['Q'] + self.frontend.messageCounts['S'] - before
		self.control.invalid = self.control.invalid || self.frontend.invalid
		self.control.stateLock.Unlock()
	}
	return n, err
}

// Observe completed response cycles, regardless of socket packet boundaries.
func (self *legacyBeginWireConn) Read(p []byte) (int, error) {
	self.readLock.Lock()
	defer self.readLock.Unlock()
	n, err := self.Conn.Read(p)
	if self.control.enabled.Load() {
		before := self.backend.messageCounts['Z']
		self.backend.add(p[:n])
		self.control.stateLock.Lock()
		self.control.ready += self.backend.messageCounts['Z'] - before
		self.control.invalid = self.control.invalid || self.backend.invalid
		self.control.stateLock.Unlock()
	}
	return n, err
}

// Use only the disposable database already attested by TestEnv. Credentials
// remain in the copied config and are never rendered into a URL or transcript.
func legacyBeginTestPool(t testing.TB, ctx context.Context, control *legacyBeginWireControl) *pgxpool.Pool {
	t.Helper()
	var connConfig *pgx.ConnConfig
	server.Db(ctx, func(conn server.PgConn) { connConfig = conn.Conn().Config() })
	if connConfig.TLSConfig != nil {
		t.Fatal("wire control requires the attested local plaintext PostgreSQL fixture")
	}
	config, err := pgxpool.ParseConfig("host=synthetic-pg.example user=synthetic dbname=synthetic sslmode=disable")
	server.Raise(err)
	config.ConnConfig = connConfig
	config.MinConns, config.MaxConns = 0, 2
	// Warm-connection control: liveness/startup behavior has separate gates.
	config.ShouldPing = func(context.Context, pgxpool.ShouldPingParams) bool { return false }
	config.ConnConfig.Tracer = control
	dial := config.ConnConfig.DialFunc
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &legacyBeginWireConn{Conn: conn, control: control}, nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	server.Raise(err)
	return pool
}

// Sixteen contenders share two warmed slots. The old two-request setup and
// production BeginQuery run the same read/commit under zero and 5 ms transport
// delay. Exact request counts prove the saving; wall time is reported, not gated.
func TestLegacySettlementBeginLoadedPoolRequestControl(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		const workers = 16
		const rounds = 4
		for _, delay := range []time.Duration{0, 5 * time.Millisecond} {
			for _, combined := range []bool{false, true} {
				func() {
					control := &legacyBeginWireControl{ctx: ctx, delay: delay, acquiring: make(chan struct{}, workers*rounds)}
					pool := legacyBeginTestPool(t, ctx, control)
					defer pool.Close()
					held := make([]*pgxpool.Conn, 2)
					defer func() {
						for _, conn := range held {
							if conn != nil {
								conn.Release()
							}
						}
					}()
					for i := range held {
						held[i] = server.RaisePgResult(pool.Acquire(ctx))
						// Preserve production's prepared-statement mode while
						// excluding each connection's first description request.
						var warm bool
						server.Raise(held[i].QueryRow(ctx, legacyBeginSettingsSql).Scan(&warm))
					}
					release := sync.OnceFunc(func() {
						for _, conn := range held {
							conn.Release()
						}
					})
					defer release()
					before := pool.Stat()
					control.enabled.Store(true)
					done := make(chan error, workers)
					started := time.Now()
					for range workers {
						go func() {
							value := server.HandleError(func() {
								for range rounds {
									options := pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite, DeferrableMode: pgx.NotDeferrable}
									if combined {
										options.BeginQuery = legacySettlementBeginSql
									}
									tx := server.RaisePgResult(pool.BeginTx(ctx, options))
									func() {
										defer tx.Rollback(context.WithoutCancel(ctx))
										if !combined {
											configureContractExpiryRepairTx(ctx, tx)
										}
										var valid bool
										server.Raise(tx.QueryRow(ctx, legacyBeginSettingsSql).Scan(&valid))
										if !valid {
											panic(fmt.Errorf("transaction setup changed its effective settings"))
										}
										server.Raise(tx.Commit(ctx))
									}()
								}
							})
							var workerErr error
							if value != nil {
								var ok bool
								workerErr, ok = value.(error)
								if !ok {
									workerErr = fmt.Errorf("transaction worker panicked with %T", value)
								}
							}
							done <- workerErr
						}()
					}
					for range workers {
						select {
						case <-control.acquiring:
						case <-ctx.Done():
							release()
							t.Fatal("pool contenders did not reach acquisition")
						}
					}
					if got := pool.Stat().AcquiredConns(); got != int32(len(held)) {
						cancel()
						t.Fatal("loaded-pool barrier lost its explicit slot owners")
					}
					release()
					var firstErr error
					for range workers {
						if err := <-done; firstErr == nil {
							firstErr = err
						}
					}
					server.Raise(firstErr)
					elapsed := time.Since(started)
					control.enabled.Store(false)
					after := pool.Stat()
					wantQueries := uint64(workers * rounds * 4)
					if combined {
						wantQueries -= workers * rounds
					}
					control.stateLock.Lock()
					queries, ready, invalid := control.queries, control.ready, control.invalid
					control.stateLock.Unlock()
					if invalid || queries != wantQueries || ready != wantQueries || after.AcquiredConns() != 0 || after.AcquireCount()-before.AcquireCount() != workers*rounds {
						t.Fatalf("combined=%t invalid=%t requests=%d replies=%d expected=%d held=%d acquires=%d", combined, invalid, queries, ready, wantQueries, after.AcquiredConns(), after.AcquireCount()-before.AcquireCount())
					}
					t.Logf("setup_control combined=%t delay=%s contenders=%d pool_slots=2 transactions=%d requests=%d replies=%d elapsed=%s pool_acquire_duration=%s empty_acquires=%d; no financial-body or fleet-rate claim", combined, delay, workers, workers*rounds, queries, ready, elapsed, after.AcquireDuration()-before.AcquireDuration(), after.EmptyAcquireCount()-before.EmptyAcquireCount())
				}()
			}
		}
	})
}

// Both real financial entrypoints must override session defaults before taking
// intent ownership, retain exact finance/replay behavior, and restore SET LOCAL
// after commit. A trigger observes settings inside the actual financial owner.
func TestLegacySettlementBeginFinancialSettingsAndSessionRelease(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		first, firstId := legacySettlementTestIntent(t, ctx)
		second, secondId := legacySettlementTestIntent(t, ctx)
		refused, refusedId := legacySettlementTestIntent(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET used_transfer_byte_count=400 WHERE contract_id=$1 AND party='destination'`, refusedId))
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION legacy_begin_check() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN
 IF current_setting('statement_timeout') <> '2s' OR current_setting('lock_timeout') <> '250ms'
 OR current_setting('transaction_isolation') <> 'read committed'
 OR current_setting('transaction_read_only') <> 'off' OR current_setting('transaction_deferrable') <> 'off'
 THEN RAISE EXCEPTION 'synthetic settlement transaction settings changed'; END IF;
 RETURN OLD;
 END $$;
 CREATE TRIGGER legacy_begin_check BEFORE DELETE ON legacy_settlement_intent
 FOR EACH ROW EXECUTE FUNCTION legacy_begin_check()`))
		})
		popConfig := server.Config.PushSimpleResource(server.DefaultPgConfigResourceName, []byte("min_connections: 0\nmax_connections: 1\n"))
		server.PgReset()
		defer func() { popConfig(); server.PgReset() }()
		var originalPid int32
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `SET statement_timeout='17s'; SET lock_timeout='11s';
 SET default_transaction_isolation='serializable'; SET default_transaction_read_only=on;
 SET default_transaction_deferrable=on`))
			server.Raise(conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&originalPid))
		})
		complete, busy, _, err := flushLegacySettlement(ctx, firstId)
		if err != nil || !complete || busy {
			t.Fatal("automatic financial owner failed with combined begin", err)
		}
		result, err := DrainLegacySettlements(ctx, LegacySettlementDrainRequest{ExpectedPayerNetworkId: second.sourceNetworkId, ContractIds: []server.Id{secondId}, Apply: true})
		if err != nil || len(result.Contracts) != 1 || !result.Contracts[0].FinancialCommitAcknowledged {
			t.Fatal("explicit financial owner failed with combined begin", err)
		}
		complete, busy, _, err = flushLegacySettlement(ctx, refusedId)
		if !errors.Is(err, errContractInsufficientEscrow) || complete || busy {
			t.Fatal("combined begin changed the independent accounting rollback", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var observedPid int32
			var restored bool
			server.Raise(conn.QueryRow(ctx, `SELECT pg_backend_pid(),
 current_setting('statement_timeout')='17s' AND current_setting('lock_timeout')='11s'
 AND current_setting('default_transaction_isolation')='serializable'
 AND current_setting('default_transaction_deferrable')='on'`).Scan(&observedPid, &restored))
			if observedPid != originalPid || !restored {
				t.Fatal("transaction-local setup leaked or the tested session was replaced")
			}
			server.RaisePgResult(conn.Exec(ctx, `RESET ALL`))
		})
		requireLegacySettlementTestState(t, ctx, first, firstId, false, true, 989, 0)
		requireLegacySettlementTestState(t, ctx, second, secondId, false, true, 989, 0)
		requireLegacySettlementTestState(t, ctx, refused, refusedId, true, false, 1000, 100)
		requireLegacyProviderDurability(t, ctx, first, firstId, 11)
		requireLegacyProviderDurability(t, ctx, second, secondId, 11)
		requireLegacyProviderDurability(t, ctx, refused, refusedId, 0)
		requireRedisExpiryClock(t, ctx, "22")
		complete, busy, _, err = flushLegacySettlement(ctx, firstId)
		if err != nil || complete || !busy {
			t.Fatal("combined begin changed the no-op financial replay", err)
		}
		requireRedisExpiryClock(t, ctx, "22")
	})
}
