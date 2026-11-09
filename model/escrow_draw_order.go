package model

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// DESIGN.md 20.9's draw order (D-QUOTA-9) and its gate. One comparator, escrowDrawLess, orders
// every draw of a payer's transfer balances and, reversed, every return: lexicographic on
// (isCredit, end time, start time, balance id), false first, so a team_credit line pays only
// after every other in-window balance of the payer has nothing available (E-a), and a
// settlement that keeps bytes in this order returns the rest in reverse (E-b). isCredit is
// grant_kind IS NOT DISTINCT FROM 'team_credit' in every loader. grant_kind is NULL on older
// balances, and the equality spelling would read those as NULL, which an ascending ORDER BY
// sorts after the line, so that spelling is banned in this package
// (TestDrawOrderSitesShareOneOrder).
//
// The sites switch to the comparator on sidecar.yml's escrow_draw_order gate (§19.11, D-OPS-10),
// which EscrowDrawOrderEnabled reads. Off, every query is today's byte for byte, no loader
// selects the flag, isCredit stays false on every balance, and the comparator orders exactly
// as today's sorts by end time, start time and balance id did, so main is unchanged. The gate
// is false in a binary that does not link MG6, whose index the keyset pages need
// (QUOTA-M4-02). Once on, it never turns off while a team_credit balance is in window, or E-a
// fails: a file that says off then reads as on, and the refusal alarms through
// urnetwork_quota_escrow_draw_order_off_refused_total and the log.
//
// Settlement keeps contractSettlementSweepPayouts as it is: it keeps bytes in its input's
// order, so a read in draw order returns the rest in reverse by construction (E-b). The
// spread over large free grants (subscription_redis_selection.go) never takes the line:
// preferred requires zero revenue, and the line always carries revenue (AddTeamGrantInTx).

// The grant kind of a Team's transfer overage credit line (DESIGN.md 20.9), drawn after every
// other balance. It carries net_revenue_nano_cents at the overage rate, pro = false and no tier.
const GrantKindTeamCredit GrantKind = "team_credit"

// The flag every loader selects, spelled once, as MG6's index spells it (QUOTA-M4-02): the
// planner matches the index only when the query's expression equals the index's.
const escrowDrawCreditSql = `(grant_kind IS NOT DISTINCT FROM 'team_credit')`

// A balance's place in draw order.
type escrowDrawKey struct {
	isCredit  bool
	endTime   time.Time
	startTime time.Time
	balanceId server.Id
}

func (self *escrowTransferBalance) drawKey() escrowDrawKey {
	return escrowDrawKey{
		isCredit:  self.isCredit,
		endTime:   self.endTime,
		startTime: self.startTime,
		balanceId: self.balanceId,
	}
}

func (self *DrawOrderBalance) drawKey() escrowDrawKey {
	return escrowDrawKey{
		isCredit:  self.Credit,
		endTime:   self.EndTime,
		startTime: self.StartTime,
		balanceId: self.BalanceId,
	}
}

// The one order of a payer's balances as a comparison: false before true on isCredit, then
// earlier end time, earlier start time and lower balance id, byte by byte, as PostgreSQL
// orders uuids.
func compareEscrowDrawKeys(a escrowDrawKey, b escrowDrawKey) int {
	if a.isCredit != b.isCredit {
		if !a.isCredit {
			return -1
		}
		return 1
	}
	if c := a.endTime.Compare(b.endTime); c != 0 {
		return c
	}
	if c := a.startTime.Compare(b.startTime); c != 0 {
		return c
	}
	return a.balanceId.Cmp(b.balanceId)
}

// Whether a draws before b: the only order of a payer's transfer balances, for escrow, Redis
// reservations, leases, relayed reservations and settlement. Returns walk it in reverse.
func escrowDrawLess(a *escrowTransferBalance, b *escrowTransferBalance) bool {
	return compareEscrowDrawKeys(a.drawKey(), b.drawKey()) < 0
}

// escrowDrawLess as a comparison, for slices.SortFunc.
func escrowDrawCompare(a *escrowTransferBalance, b *escrowTransferBalance) int {
	return compareEscrowDrawKeys(a.drawKey(), b.drawKey())
}

// The escrow loaders' read of a payer's in-window active balances with the flag, the gate-on
// shape of escrowTransferBalanceSql. Its scans read the flag last.
const escrowTransferBalanceDrawOrderSql = `
	SELECT balance_id, paid, balance_byte_count, start_time, end_time, ` + escrowDrawCreditSql + `
	FROM transfer_balance
	WHERE network_id = $1 AND active AND start_time <= $2 AND $2 < end_time
`

// One read of a payer's escrow balances in the gate's shape, fixed for the whole operation
// that reads it, so a gate that moves mid-operation never mixes the two.
type escrowBalanceRead struct {
	drawOrder bool
}

// The read the gate gives now.
func newEscrowBalanceRead() escrowBalanceRead {
	return escrowBalanceRead{
		drawOrder: EscrowDrawOrderEnabled(),
	}
}

// escrowTransferBalanceSql, or its gate-on shape with the flag.
func (self escrowBalanceRead) sql() string {
	if self.drawOrder {
		return escrowTransferBalanceDrawOrderSql
	}
	return escrowTransferBalanceSql
}

// Scans one row of sql() into balance.
func (self escrowBalanceRead) scan(rows pgx.Rows, balance *escrowTransferBalance) error {
	if self.drawOrder {
		return rows.Scan(&balance.balanceId, &balance.paid, &balance.balanceByteCount,
			&balance.startTime, &balance.endTime, &balance.isCredit)
	}
	return rows.Scan(&balance.balanceId, &balance.paid, &balance.balanceByteCount,
		&balance.startTime, &balance.endTime)
}

// The order of a payer's escrow balances in a keyset page with the gate on, flag first, as MG6's
// transfer_balance_active_network_credit_end_start_id orders them (QUOTA-M4-02), so each page is
// one index scan with no sort.
const escrowDrawOrderBySql = ` ORDER BY ` + escrowDrawCreditSql + `,end_time,start_time,balance_id`

// One keyset page of a payer's in-window active balances, as the Redis admission census and its
// exhaustion proof read them: the proof says it reads "as the census's keyset pages read them",
// so both build their pages here. Off, today's text, by end time, start time and balance id; on,
// flag first, with the cursor comparing all four keys. A nil cursor reads the first page.
func (self escrowBalanceRead) censusPage(payerNetworkId server.Id, now time.Time, cursor *escrowTransferBalance, count int) (string, []any) {
	sql := self.sql()
	args := []any{payerNetworkId, now}
	if self.drawOrder {
		if cursor == nil {
			sql += escrowDrawOrderBySql + ` LIMIT $3`
			args = append(args, count)
		} else {
			sql += ` AND (` + escrowDrawCreditSql + `,end_time,start_time,balance_id)>($3,$4,$5,$6)` + escrowDrawOrderBySql + ` LIMIT $7`
			args = append(args, cursor.isCredit, cursor.endTime, cursor.startTime, cursor.balanceId, count)
		}
		return sql, args
	}
	if cursor == nil {
		sql += ` ORDER BY end_time,start_time,balance_id LIMIT $3`
		args = append(args, count)
	} else {
		sql += ` AND (end_time,start_time,balance_id)>($3,$4,$5) ORDER BY end_time,start_time,balance_id LIMIT $6`
		args = append(args, cursor.endTime, cursor.startTime, cursor.balanceId, count)
	}
	return sql, args
}

// settlementEscrowReadSql in draw order: the flag, end time, start time and balance id, so
// contractSettlementSweepPayouts keeps bytes on the line last and returns the rest to it
// first (E-b).
const settlementEscrowDrawOrderReadSql = `
    SELECT
        selected_escrow.balance_id,
        selected_escrow.balance_byte_count,
        selected_balance.start_balance_byte_count,
        selected_balance.net_revenue_nano_cents
    FROM unnest(ARRAY[$1::uuid]) AS requested_contract(contract_id)
    CROSS JOIN LATERAL (
        SELECT balance_id, balance_byte_count
        FROM transfer_escrow
        WHERE contract_id = requested_contract.contract_id
        OFFSET 0
    ) AS selected_escrow
    CROSS JOIN LATERAL (
        SELECT start_balance_byte_count, net_revenue_nano_cents, end_time, start_time,
            ` + escrowDrawCreditSql + ` AS is_credit
        FROM transfer_balance
        WHERE balance_id = selected_escrow.balance_id
        OFFSET 0
    ) AS selected_balance
    ORDER BY selected_balance.is_credit, selected_balance.end_time, selected_balance.start_time, selected_escrow.balance_id
`

// The settlement read the gate gives now.
func settlementEscrowReadSqlFor(drawOrder bool) string {
	if drawOrder {
		return settlementEscrowDrawOrderReadSql
	}
	return settlementEscrowReadSql
}

// ----- the gate -----

// Settings of the escrow_draw_order gate.
type EscrowDrawOrderSettings struct {
	// How long one look for an in-window team_credit balance answers, while sidecar.yml says
	// off, before the next. §19.11's 60 s, the file's own reread. Non-positive looks again on
	// every read.
	LineCheckInterval time.Duration
	// The bound on one look, which runs on its own connection, off every caller's path.
	LineCheckTimeout time.Duration
	// How long the gate must have read on in this process before a team_credit line may be
	// written (AddTeamGrantInTx): every other process rereads sidecar.yml within §19.11's 60
	// s, so no site still in today's order ever meets a line. 20.9's "opens only once every
	// site has reread the gate on". Non-positive opens as soon as the gate reads on.
	CreditOpenDelay time.Duration
}

func DefaultEscrowDrawOrderSettings() *EscrowDrawOrderSettings {
	return &EscrowDrawOrderSettings{
		LineCheckInterval: 60 * time.Second,
		LineCheckTimeout:  10 * time.Second,
		// one reread interval and a margin for a config push that lands host by host
		CreditOpenDelay: 90 * time.Second,
	}
}

var escrowDrawOrderOffRefused = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "urnetwork_quota_escrow_draw_order_off_refused_total",
	Help: "Looks that found a team_credit balance in window while sidecar.yml set escrow_draw_order off, so the gate stayed on (DESIGN.md 20.9, E-a). Any increase needs an operator.",
})

func init() {
	prometheus.MustRegister(escrowDrawOrderOffRefused)
}

// The gate's state in one process. Reads take no I/O but the config reader's: the look for a
// line runs in its own goroutine, on its own connection, so a caller holding a transaction
// never acquires a second connection (connect/CODESTYLE.md, Locking). Safe for concurrent use.
type escrowDrawOrderGate struct {
	settings *EscrowDrawOrderSettings
	now      func() time.Time
	// whether the binary links MG6
	linked func() bool
	// sidecar.yml's escrow_draw_order, and whether the file names it at all
	file func() (on bool, named bool)
	// whether a team_credit balance is in window at the time given
	lookForLine func(ctx context.Context, now time.Time) bool
	// starts a look off the caller's path
	startLook func(look func())

	stateLock sync.Mutex
	// what the gate answered last, and since when it has answered on
	on      bool
	onSince time.Time
	// the last look's answer
	lineKnown bool
	line      bool
	lineTime  time.Time
	looking   bool
}

func newEscrowDrawOrderGate(settings *EscrowDrawOrderSettings) *escrowDrawOrderGate {
	return &escrowDrawOrderGate{
		settings: settings,
		now:      server.NowUtc,
		linked:   server.SidecarMg6Linked,
		file: func() (bool, bool) {
			config := server.CurrentSidecarConfig()
			return config.EscrowDrawOrder(), config.EscrowDrawOrderNamed()
		},
		lookForLine: lookForTeamCreditInWindow,
		startLook: func(look func()) {
			go look()
		},
	}
}

// Whether every site follows the comparator now.
func (self *escrowDrawOrderGate) enabled() bool {
	if !self.linked() {
		return false
	}
	fileOn, fileNamed := self.file()
	now := self.now()
	look := false
	enabled := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if fileOn {
			if !self.on {
				self.on = true
				self.onSince = now
			}
			return true
		}
		if !fileNamed && !self.on {
			// The file never named the gate and this process never read it on: today's order,
			// with nothing to look for. A line is written only while the gate reads on, so an
			// environment that has one names the gate, on or explicitly off.
			return false
		}
		if !self.lineKnown || self.settings.LineCheckInterval <= now.Sub(self.lineTime) {
			if !self.looking {
				self.looking = true
				look = true
			}
		}
		if !self.lineKnown || self.line {
			// Until a look shows no line in window, the file's off is refused: before the
			// first look nothing shows that none is there, and with MG6 linked the draw order
			// is right whether or not one is.
			if !self.on {
				self.on = true
				self.onSince = now
			}
			return true
		}
		self.on = false
		return false
	}()
	if look {
		self.startLook(self.look)
	}
	return enabled
}

// Whether a team_credit line may be written now: the file says on, and the gate has read on
// in this process for at least CreditOpenDelay.
func (self *escrowDrawOrderGate) creditOpen() bool {
	if !self.enabled() {
		return false
	}
	if fileOn, _ := self.file(); !fileOn {
		return false
	}
	now := self.now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.on && self.settings.CreditOpenDelay <= now.Sub(self.onSince)
}

// One look for an in-window line, its answer stored for the next reads. A look that fails
// keeps the last answer and is retried by the next read past the interval.
func (self *escrowDrawOrderGate) look() {
	now := self.now()
	found, ok := false, false
	defer func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.looking = false
		if !ok {
			return
		}
		self.lineKnown = true
		self.line = found
		self.lineTime = now
	}()
	timeout := func() time.Duration {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.settings.LineCheckTimeout
	}()
	server.HandleError(func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		found = self.lookForLine(ctx, now)
		ok = true
	}, func(err error) {
		glog.Infof("[quota][draw-order]look for an in-window team_credit balance failed: %s\n", err)
	})
	if fileOn, _ := self.file(); ok && found && !fileOn {
		escrowDrawOrderOffRefused.Inc()
		glog.Errorf("[quota][draw-order]sidecar.yml sets escrow_draw_order off while a team_credit balance is in window; the gate stays on (DESIGN.md 20.9, E-a)\n")
	}
}

// Runs a look now, on the caller's goroutine, for tests that need the gate's answer settled.
func (self *escrowDrawOrderGate) lookNow() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.looking = true
	}()
	self.look()
}

// Forgets every answer, as a new process would start, and takes new settings.
func (self *escrowDrawOrderGate) reset(settings *EscrowDrawOrderSettings) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.settings = settings
	self.on = false
	self.onSince = time.Time{}
	self.lineKnown = false
	self.line = false
	self.lineTime = time.Time{}
}

// Whether any Team network holds a team_credit balance in window at now. Only a Team network
// holds one (AddTeamGrantInTx requires network_team), so this probes each Team's balances by
// network rather than scanning every live balance; network_team is MG6's, read only where MG6
// is linked.
func lookForTeamCreditInWindow(ctx context.Context, now time.Time) (found bool) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT EXISTS (
					SELECT 1
					FROM network_team
					CROSS JOIN LATERAL (
						SELECT 1
						FROM transfer_balance
						WHERE
							transfer_balance.network_id = network_team.network_id AND
							`+escrowDrawCreditSql+` AND
							transfer_balance.start_time <= $1 AND
							$1 < transfer_balance.end_time
						LIMIT 1
					) AS line
				)
			`,
			now.UTC(),
		).Scan(&found))
	})
	return
}

// The process's gate. sidecar.yml is the process's, so the gate has one owner per process.
var escrowDrawOrder = newEscrowDrawOrderGate(DefaultEscrowDrawOrderSettings())

// Whether every draw and return site follows escrowDrawLess now (DESIGN.md 20.9's gate):
// sidecar.yml's escrow_draw_order, reread every 60 s, false unless the binary links MG6, and
// on whatever the file says while a team_credit balance is in window. No I/O on the caller's
// path but the config reader's.
func EscrowDrawOrderEnabled() bool {
	return escrowDrawOrder.enabled()
}

// Whether a team_credit line may be written now: the gate on by the file and read on in this
// process for at least EscrowDrawOrderSettings.CreditOpenDelay. Team checkout opens on it.
func EscrowDrawOrderOpenForCredit() bool {
	return escrowDrawOrder.creditOpen()
}

// Stands in for sidecar.yml's escrow_draw_order until the returned pop, with the gate's state
// forgotten on both sides, and opens the line without delay. The gate's look for an in-window
// line runs before this returns, so a test reads a settled answer.
func Testing_SetEscrowDrawOrder(enabled bool) func() {
	yaml := "sidecar:\n  escrow_draw_order: {enabled: false}\n"
	if enabled {
		yaml = "sidecar:\n  escrow_draw_order: {enabled: true}\n"
	}
	pop := server.Testing_PushSidecarConfig(yaml)
	settings := DefaultEscrowDrawOrderSettings()
	settings.CreditOpenDelay = 0
	escrowDrawOrder.reset(settings)
	if server.SidecarMg6Linked() {
		escrowDrawOrder.lookNow()
	}
	return func() {
		pop()
		escrowDrawOrder.reset(DefaultEscrowDrawOrderSettings())
	}
}

// Runs the gate's look for an in-window line now, as the next read past the interval would.
func Testing_LookForEscrowDrawOrderLine() {
	escrowDrawOrder.lookNow()
}
