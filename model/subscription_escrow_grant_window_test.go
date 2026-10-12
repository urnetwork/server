// Pins the escrow grant read's work and time boundaries using real PostgreSQL
// rows, without changing reservations or relying on elapsed-time thresholds.
package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// One transaction owns its row counter and optional timestamp-boundary setup.
// Every non-grant operation delegates unchanged to the real transaction.
type escrowGrantQueryTestTx struct {
	server.PgTx
	grantQueries int
	grantRows    int
	grantSql     string
	grantArgs    []any
	beforeGrant  func([]any)
}

// Records only the grant query; no process-global database hooks are used.
func (self *escrowGrantQueryTestTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	// Cache validation has a separate balance primary-key lookup; it neither
	// discovers grants nor changes the candidate/locking row budget.
	grant := sql != netEscrowAdmissionCacheSQL && strings.Contains(strings.Join(strings.Fields(sql), " "), " FROM transfer_balance ")
	if grant {
		self.grantQueries++
		self.grantSql, self.grantArgs = sql, append([]any(nil), args...)
		if self.beforeGrant != nil {
			self.beforeGrant(args)
		}
	}
	rows, err := self.PgTx.Query(ctx, sql, args...)
	if !grant || err != nil {
		return rows, err
	}
	return &escrowGrantQueryTestRows{Rows: rows, count: &self.grantRows}, nil
}

// Counts actual rows crossing the PostgreSQL/application boundary, rather
// than inferring query cost from its text or from wall-clock timing.
type escrowGrantQueryTestRows struct {
	pgx.Rows
	count *int
}

// Delegates row iteration and increments only after PostgreSQL returned a row.
func (self *escrowGrantQueryTestRows) Next() bool {
	if !self.Rows.Next() {
		return false
	}
	*self.count += 1
	return true
}

type escrowGrantStatementKind int

const (
	escrowGrantOther escrowGrantStatementKind = iota
	// Identity-only discovery of the complete current set before ownership.
	escrowGrantDiscovery
	// A candidate or locking read that returns grant rows.
	escrowGrantRead
	// An exact reservation snapshot read of the grants it names.
	escrowGrantReservation
)

// Positive allocation discovers the ids of every current grant and admits
// their ownership before its candidate and locking reads (403b9c55). Those
// discovery reads select only balance_id; every other read returns grants.
// The exact census and cache publication, like the cache read's primary-key
// lookup, neither discover grants nor change the candidate/locking budget.
func escrowGrantStatement(sql string) escrowGrantStatementKind {
	if sql == netEscrowAdmissionCacheSQL {
		return escrowGrantReservation
	}
	normalized := strings.Join(strings.Fields(sql), " ")
	switch {
	case !strings.Contains(normalized, " FROM transfer_balance "),
		strings.Contains(normalized, "transfer_balance_net_escrow_"):
		return escrowGrantOther
	case strings.HasPrefix(normalized, "SELECT balance_id FROM transfer_balance "),
		strings.HasPrefix(normalized, "SELECT selected.balance_id FROM ("):
		return escrowGrantDiscovery
	default:
		return escrowGrantRead
	}
}

// Counts one owned allocation's grant work at the driver, from the rows each
// statement actually returned. Positive allocation admits transaction
// ownership only on the server's own transaction (403b9c55), so a wrapper
// transaction cannot carry these counters; the hook binds to that
// transaction's connection through the context the allocation uses instead.
type escrowGrantQueryHook struct {
	grantQueries     int
	grantRows        int
	discoveryQueries int
	discoveryRows    int
	reservationReads int
	reservationRows  int
	grantSql         string
	grantArgs        []any
	// Runs before each candidate or locking read is sent.
	beforeGrant func(sql string, args []any)
}

// Bind with withLegacyStatementHooks inside a scope from startLegacyStatementHooks.
func (self *escrowGrantQueryHook) statementHook() *legacyStatementHook {
	return &legacyStatementHook{
		beforeQuery: func(_ context.Context, sql string, args []any) {
			switch escrowGrantStatement(sql) {
			case escrowGrantDiscovery:
				self.discoveryQueries++
			case escrowGrantRead:
				self.grantQueries++
				self.grantSql, self.grantArgs = sql, append([]any(nil), args...)
				if self.beforeGrant != nil {
					self.beforeGrant(sql, args)
				}
			case escrowGrantReservation:
				self.reservationReads++
				self.reservationRows += len(args[0].([]server.Id))
			}
		},
		completed: func(sql string, tag pgconn.CommandTag) {
			switch escrowGrantStatement(sql) {
			case escrowGrantDiscovery:
				self.discoveryRows += int(tag.RowsAffected())
			case escrowGrantRead:
				self.grantRows += int(tag.RowsAffected())
			}
		},
	}
}

// Expired and future grants are active while their durable bytes are positive,
// but neither may consume the per-contract grant read's wire/decoding work.
func TestCreateTransferEscrowReadsOnlyCurrentGrants(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := newEscrowSelectionTestClients(t, ctx)
		now := server.NowUtc()
		firstId, secondId := server.NewId(), server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
				add := func(id, networkId server.Id, start, end time.Time, bytes ByteCount, paid bool) {
					revenue := 0
					if paid {
						revenue = 1
					}
					batch.Queue(`INSERT INTO transfer_balance
						(balance_id, network_id, start_time, end_time, start_balance_byte_count, balance_byte_count, net_revenue_nano_cents)
						VALUES ($1, $2, $3, $4, $5, $5, $6)`, id, networkId, start, end, bytes, revenue)
				}
				add(firstId, clients.payerNetworkId, now.Add(-time.Hour), now.Add(time.Hour), 1024, true)
				add(secondId, clients.payerNetworkId, now.Add(-time.Hour), now.Add(2*time.Hour), 1024, false)
				for range 128 {
					add(server.NewId(), clients.payerNetworkId, now.Add(-2*time.Hour), now.Add(-time.Hour), 1024, false)
					add(server.NewId(), clients.payerNetworkId, now.Add(time.Hour), now.Add(2*time.Hour), 1024, false)
				}
				add(server.NewId(), clients.payerNetworkId, now.Add(-time.Hour), now.Add(time.Hour), 0, true)
				add(server.NewId(), server.NewId(), now.Add(-time.Hour), now.Add(time.Hour), 1024, true)
			})
		})
		stopHooks := startLegacyStatementHooks(t, ctx)
		defer stopHooks()
		for _, byteCount := range []ByteCount{0, 1536} {
			query := &escrowGrantQueryHook{}
			var escrow *TransferEscrow
			var posts []func() any
			// Positive allocation admits its grants' transaction ownership
			// (403b9c55), which only the server's own read-committed no-retry
			// transaction can hold, so the counters observe that transaction.
			server.Tx(ctx, func(tx server.PgTx) {
				hooked, unhook := withLegacyStatementHooks(ctx, tx, query.statementHook())
				defer unhook()
				var err error
				escrow, posts, err = createTransferEscrowInTx(hooked, tx, clients.payerNetworkId, clients.payerId,
					clients.providerNetworkId, clients.providerId, clients.payerNetworkId, byteCount, nil)
				server.Raise(err)
			}, server.TxReadCommitted, server.OptNoRetry())
			server.RunPosts(ctx, posts...)
			wantQueries, wantRows := 1, 1
			wantDiscoveries, wantDiscoveryRows := 0, 0
			if byteCount > 0 {
				wantQueries = 2 // Identity-only discovery, then locked current grants.
				wantRows = 2
				// The allocator and its complete locked read each discover
				// the ids of the two current grants before admitting them.
				wantDiscoveries, wantDiscoveryRows = 2, 4
			}
			if query.grantQueries != wantQueries || query.grantRows != wantRows {
				t.Errorf("bytes=%d grant queries=%d rows=%d, want %d queries and %d current rows", byteCount, query.grantQueries, query.grantRows, wantQueries, wantRows)
			}
			if query.discoveryQueries != wantDiscoveries || query.discoveryRows != wantDiscoveryRows {
				t.Errorf("bytes=%d ownership discovery queries=%d rows=%d, want %d queries and %d current rows", byteCount, query.discoveryQueries, query.discoveryRows, wantDiscoveries, wantDiscoveryRows)
			}
			want := map[server.Id]ByteCount{firstId: 0}
			wantPriority := Priority(PaidPriority)
			if byteCount != 0 {
				want[firstId], want[secondId] = 1024, 512
				wantPriority = (PaidPriority + UnpaidPriority) / 2
			}
			if len(escrow.Balances) != len(want) || escrow.Priority != wantPriority || escrow.TransferByteCount != byteCount {
				t.Fatalf("bytes=%d changed allocation count, priority or reserved bytes", byteCount)
			}
			for _, balance := range escrow.Balances {
				if expected, ok := want[balance.BalanceId]; !ok || balance.BalanceByteCount != expected {
					t.Fatalf("bytes=%d changed earliest-current grant allocation", byteCount)
				}
			}
			if byteCount != 0 {
				if Testing_NetEscrowByteCount(ctx, firstId) != 1024 || Testing_NetEscrowByteCount(ctx, secondId) != 512 {
					t.Fatal("current grants lost their committed reservation mirrors")
				}
			}
		}
	})
}

// Captures the production query's single clock value, then creates exact
// boundaries before that query runs. No sleeps or guessed timing are needed.
func TestCreateTransferEscrowGrantWindowKeepsExactBoundaries(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := newEscrowSelectionTestClients(t, ctx)
		var query *escrowGrantQueryTestTx
		var earliestId server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			query = &escrowGrantQueryTestTx{PgTx: tx, beforeGrant: func(args []any) {
				if len(args) != 2 {
					t.Fatal("grant query did not carry its one allocation timestamp")
				}
				now, ok := args[1].(time.Time)
				if !ok || now.Location() != time.UTC || now.Nanosecond()%1000 != 0 {
					t.Fatal("grant query timestamp lost PostgreSQL-precision UTC semantics")
				}
				for _, boundary := range []struct {
					start, end time.Time
					paid       bool
				}{
					{start: now.Add(-time.Second), end: now.Add(time.Microsecond), paid: true},
					{start: now, end: now.Add(time.Second)},
					{start: now.Add(-time.Second), end: now},
					{start: now.Add(time.Microsecond), end: now.Add(time.Second)},
				} {
					balance := &TransferBalance{NetworkId: clients.payerNetworkId,
						StartTime: boundary.start, EndTime: boundary.end, StartBalanceByteCount: 1024, BalanceByteCount: 1024}
					if boundary.paid {
						balance.NetRevenue = 1
					}
					AddTransferBalanceInTx(ctx, tx, balance)
					if boundary.paid {
						earliestId = balance.BalanceId
					}
				}
			}}
			escrow, _, err := createTransferEscrowInTx(ctx, query, clients.payerNetworkId, clients.payerId,
				clients.providerNetworkId, clients.providerId, clients.payerNetworkId, 0, nil)
			server.Raise(err)
			if query.grantQueries != 1 || query.grantRows != 1 || len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != earliestId || escrow.Priority != PaidPriority {
				t.Fatal("allocation changed the inclusive-start/exclusive-end boundary or earliest zero-byte anchor")
			}

			// Index availability and query semantics are separate controls.
			// A four-row fixture cannot establish Main's cost-based plan, so
			// never require the optimizer to select one particular index.
			var indexDefinition string
			server.Raise(tx.QueryRow(ctx, `
				SELECT pg_get_indexdef(indexrelid)
				FROM pg_index INNER JOIN pg_class ON pg_class.oid = indexrelid
				WHERE relname = 'transfer_balance_active_network_id_start_end_time'
					AND indisvalid AND indisready
			`).Scan(&indexDefinition))
			if !strings.Contains(indexDefinition, "(active, network_id, start_time, end_time)") {
				t.Fatal("existing grant-window index is missing its expected key columns")
			}
			var raw []byte
			server.Raise(tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON, COSTS OFF) "+query.grantSql, query.grantArgs...).Scan(&raw))
			var plans []map[string]any
			server.Raise(json.Unmarshal(raw, &plans))
			startBound, endBound := false, false
			var inspect func(map[string]any)
			inspect = func(node map[string]any) {
				for _, field := range []string{"Index Cond", "Recheck Cond", "Filter"} {
					condition, _ := node[field].(string)
					startBound = startBound || strings.Contains(condition, "start_time")
					endBound = endBound || strings.Contains(condition, "end_time")
				}
				if children, ok := node["Plans"].([]any); ok {
					for _, child := range children {
						inspect(child.(map[string]any))
					}
				}
			}
			for _, plan := range plans {
				inspect(plan["Plan"].(map[string]any))
			}
			if !startBound || !endBound {
				t.Fatal("PostgreSQL plan did not apply both grant-window bounds before returning rows")
			}
		})
	})
}
