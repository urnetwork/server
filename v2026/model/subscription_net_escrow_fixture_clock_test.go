// Fixture grant discovery and publication must agree on an already-current
// window even when the host clock leads the database clock.
package model

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

// Exact constructor input shape; the test shifts the supplied times without
// replacing the constructor's start/end formula or any SQL expression.
const netEscrowFixtureBalanceInsertSql = `
 INSERT INTO transfer_balance (
  balance_id, network_id, start_time, end_time, start_balance_byte_count,
  net_revenue_nano_cents, balance_byte_count, pro
 ) VALUES ($1, $2, $3, $4, $5, $6, $5, false)
`

// Exact mandatory publication check, observed without rewriting its SQL or args.
const netEscrowFixturePublicationSql = `SELECT count(*)=$2 FROM transfer_balance
 WHERE balance_id=ANY($1) AND active AND start_time<=clock_timestamp() AT TIME ZONE 'UTC'
 AND clock_timestamp() AT TIME ZONE 'UTC'<end_time`

// Marks only this fixture's host-clock inputs, never unrelated pool users.
type netEscrowFixtureClockContextKey struct{}

// The four host-time input shapes are scoped to one fixture and one public
// admission. The real PostgreSQL clock and publication reply remain untouched.
type netEscrowFixtureClock struct {
	offset            time.Duration
	fixtureWrites     atomic.Int64
	activeReads       atomic.Int64
	spreadReads       atomic.Int64
	fundingReads      atomic.Int64
	publicationChecks atomic.Int64
	invalidShape      atomic.Bool
}

// Shift only exact tagged constructor/discovery time arguments. pgx invokes
// this hook before it encodes those same argument values for the real server.
func (self *netEscrowFixtureClock) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if clock, _ := ctx.Value(netEscrowFixtureClockContextKey{}).(*netEscrowFixtureClock); clock != self {
		return ctx
	}
	normalized := strings.Join(strings.Fields(data.SQL), " ")
	shift := func(index int) bool {
		if index >= len(data.Args) {
			return false
		}
		at, ok := data.Args[index].(time.Time)
		if !ok {
			return false
		}
		data.Args[index] = at.Add(self.offset)
		return true
	}
	hostRead := func(width int) bool {
		if len(data.Args) != width {
			return false
		}
		if _, ok := data.Args[0].(server.Id); !ok {
			return false
		}
		return shift(1)
	}
	switch {
	case normalized == strings.Join(strings.Fields(netEscrowFixtureBalanceInsertSql), " "):
		if len(data.Args) != 6 {
			self.invalidShape.Store(true)
			return ctx
		}
		_, balanceOk := data.Args[0].(server.Id)
		_, networkOk := data.Args[1].(server.Id)
		_, startOk := data.Args[2].(time.Time)
		_, endOk := data.Args[3].(time.Time)
		amount, amountOk := data.Args[4].(ByteCount)
		revenue, revenueOk := data.Args[5].(NanoCents)
		if !balanceOk || !networkOk || !startOk || !endOk || !amountOk || amount != 1000 || !revenueOk || revenue != 0 {
			self.invalidShape.Store(true)
			return ctx
		}
		shift(2)
		shift(3)
		self.fixtureWrites.Add(1)
	case data.SQL == activeTransferBalanceSql:
		if !hostRead(2) {
			self.invalidShape.Store(true)
			return ctx
		}
		self.activeReads.Add(1)
	case data.SQL == redisGrantSpreadSelectionSql:
		if len(data.Args) != 4 {
			self.invalidShape.Store(true)
			return ctx
		}
		count, countOk := data.Args[2].(int)
		threshold, thresholdOk := data.Args[3].(ByteCount)
		if !countOk || count != proberGrantFirstCount || !thresholdOk || threshold != ProberTransferBalanceTopUp || !hostRead(4) {
			self.invalidShape.Store(true)
			return ctx
		}
		self.spreadReads.Add(1)
	case data.SQL == escrowTransferBalanceSql+` ORDER BY end_time,start_time,balance_id LIMIT $3`:
		if len(data.Args) != 3 {
			self.invalidShape.Store(true)
			return ctx
		}
		limit, limitOk := data.Args[2].(int)
		if !limitOk || limit != redisGrantPageRows || !hostRead(3) {
			self.invalidShape.Store(true)
			return ctx
		}
		self.fundingReads.Add(1)
	case normalized == strings.Join(strings.Fields(netEscrowFixturePublicationSql), " "):
		if len(data.Args) != 2 {
			self.invalidShape.Store(true)
			return ctx
		}
		ids, idsOk := data.Args[0].([]server.Id)
		count, countOk := data.Args[1].(int)
		if !idsOk || !countOk || len(ids) != 1 || count != 1 {
			self.invalidShape.Store(true)
			return ctx
		}
		self.publicationChecks.Add(1)
	}
	return ctx
}

// Queries execute normally; only their host-domain input times were shifted.
func (self *netEscrowFixtureClock) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// A bounded call observes a host lead far longer than its whole lifetime.
// The already-current fixture must reserve, while a genuinely future SQL
// grant still reaches the unchanged publication check and must be refused.
func TestNetEscrowOrderingFixtureReservesWhenHostTimeLeadsDatabase(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		for _, c := range []struct {
			name        string
			hostLead    time.Duration
			wantCurrent bool
		}{
			{name: "already_current", hostLead: 30 * time.Minute, wantCurrent: true},
			{name: "genuinely_future", hostLead: 2 * time.Hour, wantCurrent: false},
		} {
			func() {
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				var databaseNow time.Time
				server.Db(ctx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(ctx, `SELECT clock_timestamp() AT TIME ZONE 'UTC'`).Scan(&databaseNow))
				})
				clock := &netEscrowFixtureClock{offset: databaseNow.Add(c.hostLead).Sub(server.NowUtc())}
				if clock.offset <= 30*time.Second {
					t.Fatalf("%s: test host-clock shift does not exceed the whole operation bound: %s", c.name, clock.offset)
				}
				scope, err := server.NewTestPgQueryScope(ctx, clock)
				server.Raise(err)
				defer func() { server.Raise(scope.Close()) }()
				shifted := context.WithValue(ctx, netEscrowFixtureClockContextKey{}, clock)
				fixture := newNetEscrowOrderingTestFixture(t, shifted)
				escrow, admissionErr := server.HandleError2(func() (*TransferEscrow, error) {
					return CreateTransferEscrow(shifted, fixture.sourceNetworkId, fixture.sourceId,
						fixture.destinationNetworkId, fixture.destinationId, 100)
				}, func(err error) (*TransferEscrow, error) { return nil, err })
				var current bool
				var published int
				server.Db(ctx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(ctx, `SELECT active AND start_time<=clock_timestamp() AT TIME ZONE 'UTC'
 AND clock_timestamp() AT TIME ZONE 'UTC'<end_time FROM transfer_balance WHERE balance_id=$1`, fixture.balanceId).Scan(&current))
					server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE source_network_id=$1`, fixture.sourceNetworkId).Scan(&published))
				})
				if clock.invalidShape.Load() || clock.fixtureWrites.Load() != 1 || clock.activeReads.Load() != 1 ||
					clock.spreadReads.Load() != 1 || clock.fundingReads.Load() != 1 || clock.publicationChecks.Load() != 1 {
					t.Fatalf("%s: clock proof did not traverse the exact fixture/discovery/publication shapes: invalid=%t writes=%d active=%d spread=%d funding=%d publication=%d",
						c.name, clock.invalidShape.Load(), clock.fixtureWrites.Load(), clock.activeReads.Load(), clock.spreadReads.Load(), clock.fundingReads.Load(), clock.publicationChecks.Load())
				}
				if c.wantCurrent {
					if !current || admissionErr != nil || escrow == nil || len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != fixture.balanceId || published != 1 {
						t.Fatalf("%s: host-current fixture did not publish against the real database clock: database_current=%t published=%d error=%v", c.name, current, published, admissionErr)
					}
				} else if current || escrow != nil || admissionErr == nil || admissionErr.Error() != "selected Redis grant expired or changed before contract publication" || published != 0 {
					t.Fatalf("%s: real future grant acquired publication authority: database_current=%t published=%d escrow=%v error=%v", c.name, current, published, escrow, admissionErr)
				}
			}()
		}
	})
}
