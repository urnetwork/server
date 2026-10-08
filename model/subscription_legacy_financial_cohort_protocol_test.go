// Real PostgreSQL framing distinguishes SQL statements, sends and synchronization.
package model

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// The fixture alone supplies the connection configuration. Authentication is
// completed before counters start. Only counts are retained, never frame data,
// bind values, credentials or server messages. Read boundaries remain real.
type legacyCohortProtocolConn struct {
	net.Conn
	ctx           context.Context
	enabled       atomic.Bool
	delay         time.Duration
	outbound      []byte
	inbound       []byte
	syncs         atomic.Int64
	simpleQueries atomic.Int64
	ready         atomic.Int64
	sentBytes     atomic.Int64
	receivedBytes atomic.Int64
}

func (self *legacyCohortProtocolConn) Write(data []byte) (int, error) {
	count, err := self.Conn.Write(data)
	if !self.enabled.Load() {
		return count, err
	}
	self.sentBytes.Add(int64(count))
	self.outbound = append(self.outbound, data[:count]...)
	for len(self.outbound) >= 5 {
		length := int(binary.BigEndian.Uint32(self.outbound[1:5])) + 1
		if length < 5 || length > 16*1024*1024 {
			return count, fmt.Errorf("fixture frontend frame exceeds bound")
		}
		if len(self.outbound) < length {
			break
		}
		switch self.outbound[0] {
		case 'S':
			self.syncs.Add(1)
		case 'Q':
			self.simpleQueries.Add(1)
		}
		self.outbound = self.outbound[length:]
	}
	return count, err
}

func (self *legacyCohortProtocolConn) Read(destination []byte) (int, error) {
	if len(self.inbound) == 0 {
		var header [5]byte
		if _, err := io.ReadFull(self.Conn, header[:]); err != nil {
			return 0, err
		}
		length := int(binary.BigEndian.Uint32(header[1:5])) + 1
		if length < 5 || length > 16*1024*1024 {
			return 0, fmt.Errorf("fixture backend frame exceeds bound")
		}
		frame := make([]byte, length)
		copy(frame, header[:])
		if _, err := io.ReadFull(self.Conn, frame[5:]); err != nil {
			return 0, err
		}
		if self.enabled.Load() {
			self.receivedBytes.Add(int64(length))
			if header[0] == 'Z' {
				self.ready.Add(1)
				if self.delay > 0 {
					// Latency is an explicit model applied at a real protocol
					// synchronization response. No wall threshold is an oracle.
					select {
					case <-time.After(self.delay):
					case <-self.ctx.Done():
						return 0, self.ctx.Err()
					}
				}
			}
		}
		self.inbound = frame
	}
	count := copy(destination, self.inbound)
	self.inbound = self.inbound[count:]
	return count, nil
}

func legacyCohortProtocolConnection(t testing.TB, ctx context.Context, delay time.Duration) (*pgx.Conn, *legacyCohortProtocolConn) {
	t.Helper()
	var config *pgx.ConnConfig
	server.Db(ctx, func(conn server.PgConn) { config = conn.Conn().Config().Copy() })
	// DefaultTestEnv has already validated the private local fixture. Cleartext
	// there permits framing observation without changing SQL or authentication.
	config.TLSConfig = nil
	config.Fallbacks = nil
	var observed *legacyCohortProtocolConn
	config.DialFunc = func(dialCtx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(dialCtx, network, address)
		if err != nil {
			return nil, err
		}
		observed = &legacyCohortProtocolConn{Conn: conn, ctx: ctx, delay: delay}
		return observed, nil
	}
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal("isolated protocol connection failed", err)
	}
	if observed == nil {
		t.Fatal("protocol fixture did not own its connection")
	}
	return conn, observed
}

// A completed lock query gives a lower bound on grant residence; transaction
// start through commit gives an upper bound. Neither is called server lock time.
type legacyCohortProtocolTx struct {
	server.PgTx
	grantReady time.Time
	statements int
	dispatches int
}

type legacyCohortGrantRows struct {
	pgx.Rows
	owner *legacyCohortProtocolTx
	found bool
}

func (self *legacyCohortGrantRows) Next() bool {
	found := self.Rows.Next()
	self.found = self.found || found
	return found
}
func (self *legacyCohortGrantRows) Close() {
	self.Rows.Close()
	if self.found && self.owner.grantReady.IsZero() {
		self.owner.grantReady = time.Now()
	}
}

func (self *legacyCohortProtocolTx) Exec(ctx context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	self.dispatches++
	self.statements++
	return self.PgTx.Exec(ctx, query, args...)
}

func (self *legacyCohortProtocolTx) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	self.dispatches++
	self.statements++
	rows, err := self.PgTx.Query(ctx, query, args...)
	if err == nil && strings.Contains(query, "FROM transfer_balance") && strings.Contains(query, "FOR UPDATE") {
		return &legacyCohortGrantRows{Rows: rows, owner: self}, nil
	}
	return rows, err
}
func (self *legacyCohortProtocolTx) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	self.dispatches++
	self.statements++
	return self.PgTx.QueryRow(ctx, query, args...)
}
func (self *legacyCohortProtocolTx) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	self.dispatches++
	self.statements += len(batch.QueuedQueries)
	return self.PgTx.SendBatch(ctx, batch)
}

// Each real connection begins cold and is then reused; modeled 0/1ms cases
// use the same real statements and fixed funds.
// This isolates the financial owner on one real connection: server pool acquire,
// provider signing and transaction-owned Redis hole posts are not measured here.
// Full public-page tests separately cover those owners and total throughput.
func TestLegacyFinancialCohortRealProtocolWork(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		type sample struct {
			Profile          string `json:"profile"`
			DelayNs          int64  `json:"delay_ns_per_ready_for_query"`
			Contracts        int    `json:"contracts"`
			Transactions     int    `json:"transactions"`
			Syncs            int64  `json:"frontend_syncs"`
			SimpleQueries    int64  `json:"simple_queries"`
			Ready            int64  `json:"backend_ready_for_query"`
			SentBytes        int64  `json:"frontend_bytes"`
			ReceivedBytes    int64  `json:"backend_bytes"`
			WallNs           int64  `json:"financial_wall_ns"`
			MaxTransactionNs int64  `json:"max_transaction_wall_ns"`
			MaxGrantLowerNs  int64  `json:"max_grant_residence_lower_ns"`
			SqlStatements    int    `json:"observed_statements"`
			Dispatches       int    `json:"observed_dispatches"`
		}
		for _, delay := range []time.Duration{0, time.Millisecond} {
			var prior sample
			for _, profile := range []string{"individual", "cohort"} {
				f := legacyFinancialCohortSeed(t, ctx, 32)
				conn, wire := legacyCohortProtocolConnection(t, ctx, delay)
				defer func() {
					cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
					defer stop()
					_ = conn.Close(cleanup)
				}()
				measured, finish := withLegacySettlementPostBatch(ctx)
				wire.enabled.Store(true)
				started := time.Now()
				result := sample{Profile: profile, DelayNs: int64(delay), Contracts: len(f.ids)}
				stride := 1
				if profile == "cohort" {
					stride = 8
				}
				for offset := 0; offset < len(f.ids); offset += stride {
					txStarted := time.Now()
					tx, err := conn.BeginTx(measured, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
					if err != nil {
						t.Fatal(err)
					}
					defer func() {
						cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
						defer stop()
						_ = tx.Rollback(cleanup)
					}()
					owner := &legacyCohortProtocolTx{PgTx: tx}
					var posts []func() any
					if profile == "individual" {
						var completed, busy bool
						posts, completed, busy, _, err = flushLegacySettlementInTx(measured, owner, f.ids[offset])
						if err != nil || !completed || busy {
							tx.Rollback(ctx)
							t.Fatal("individual financial control failed", err)
						}
					} else {
						var attempts []legacyFinancialCohortAttempt
						attempts, posts, err = flushLegacySettlementCohortInTx(measured, owner, f.ids[offset:offset+stride])
						if err != nil || len(attempts) != stride {
							tx.Rollback(ctx)
							t.Fatal("cohort financial control failed", err)
						}
						for _, attempt := range attempts {
							if !attempt.completed || attempt.busy || attempt.fallback {
								tx.Rollback(ctx)
								t.Fatal("healthy protocol cohort fell back", attempt)
							}
						}
					}
					if !owner.grantReady.IsZero() {
						// Stop before commit dispatch: its reply includes time after
						// PostgreSQL released the grant and cannot be a lower bound.
						result.MaxGrantLowerNs = max(result.MaxGrantLowerNs, time.Since(owner.grantReady).Nanoseconds())
					}
					server.Raise(tx.Commit(measured))
					result.Transactions++
					result.MaxTransactionNs = max(result.MaxTransactionNs, time.Since(txStarted).Nanoseconds())
					result.SqlStatements += owner.statements
					result.Dispatches += owner.dispatches
					server.RunPosts(measured, posts...)
				}
				result.WallNs = time.Since(started).Nanoseconds()
				wire.enabled.Store(false)
				result.Syncs = wire.syncs.Load()
				result.SimpleQueries = wire.simpleQueries.Load()
				result.Ready = wire.ready.Load()
				result.SentBytes = wire.sentBytes.Load()
				result.ReceivedBytes = wire.receivedBytes.Load()
				server.Raise(conn.Close(ctx))
				finish()
				legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
				raw, _ := json.Marshal(result)
				t.Logf("legacy_financial_protocol=%s", raw)
				if result.Ready != result.Syncs+result.SimpleQueries {
					t.Fatal("protocol synchronization inventory differs", result)
				}
				if profile == "individual" {
					prior = result
				} else if result.Ready*4 >= prior.Ready {
					t.Fatalf("cohort failed to amortize actual PostgreSQL synchronization: individual=%d cohort=%d", prior.Ready, result.Ready)
				}
			}
		}
	})
}
