// Grant selection retains the payer's financial order and spreads the internal
// prober and multi-grant payers across their large free grants. Only Redis
// reserves bytes; discovery never locks shared SQL grants.
// Selection precedes the contract transaction: each discovery read is one
// autocommit statement on a pooled connection, released before the Redis
// reservations it informs.
package model

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
	"gopkg.in/yaml.v3"
)

const redisGrantPageRows = 64

// Redis admission's first discovery window. An internal prober, or a payer
// holding at least two large free grants, spreads its allocations across those
// grants by the payer client's hash. Each grant is its own Redis escrow slot
// and its own debit shard, so one very large network cannot saturate a single
// Redis node with every admission and release. A payer with fewer than two
// such grants reads no candidates and keeps the earliest-expiry order; the
// LIMIT 2 count bounds the eligibility read. The legacy allocator keeps
// proberGrantSelectionSql unchanged.
const redisGrantSpreadSelectionSql = `
	WITH allocation AS MATERIALIZED (
		SELECT EXISTS (
			SELECT 1 FROM prober_identity WHERE singleton AND network_id = $1
			UNION ALL SELECT 1 FROM prober_shard_run WHERE network_id = $1
		) AS internal_prober,
		(SELECT count(*) FROM (
			SELECT 1 FROM transfer_balance
			WHERE network_id = $1 AND active AND start_time <= $2 AND $2 < end_time
				AND NOT paid AND NOT pro
				AND net_revenue_nano_cents = 0
				AND subsidy_net_revenue_nano_cents = 0
				AND start_balance_byte_count >= $4
			LIMIT 2
		) AS large_free) = 2 AS multi_grant
	)
	SELECT selected.balance_id, selected.paid, selected.balance_byte_count,
		selected.start_time, selected.end_time, allocation.internal_prober,
		NOT selected.paid AND NOT selected.pro
			AND selected.net_revenue_nano_cents = 0
			AND selected.subsidy_net_revenue_nano_cents = 0
			AND selected.start_balance_byte_count >= $4 AS preferred
	FROM allocation
	CROSS JOIN LATERAL (
		SELECT balance_id, paid, balance_byte_count, start_time, end_time,
			pro, net_revenue_nano_cents, subsidy_net_revenue_nano_cents,
			start_balance_byte_count
		FROM transfer_balance
		WHERE network_id = $1 AND active AND start_time <= $2 AND $2 < end_time
		ORDER BY start_time DESC, end_time DESC
		LIMIT CASE WHEN allocation.internal_prober OR allocation.multi_grant THEN $3::bigint ELSE 0 END
	) AS selected
`

var errRedisGrantSelectionCapacity = errors.New("Redis grant selection capacity reached; remaining funding is unknown")

// Limits are immutable values; each request owns its counters and cursor.
// A larger reviewed profile permits a subsequent fresh request to look farther.
// No grant, liability or reservation is deleted when an operation hits a cap.
type redisGrantSelectionLimits struct {
	MaxRows     int `yaml:"max_rows"`
	MaxSelected int `yaml:"max_selected"`
}

type redisGrantSelectionContextKey struct{}

// A test seam may observe discovery reads; production reads pooled connections.
type redisGrantReadContextKey struct{}

// Runs one discovery read on a pooled connection, outside any transaction.
func readRedisGrants(ctx context.Context, read func(query server.PgCanQuery)) {
	observe, _ := ctx.Value(redisGrantReadContextKey{}).(func(server.PgCanQuery) server.PgCanQuery)
	server.Db(ctx, func(conn server.PgConn) {
		if observe != nil {
			read(observe(conn))
			return
		}
		read(conn)
	})
}

// Settings are loaded once per process, not on every contract's hot path.
// Deploy an immutable config revision and restart to change the profile. This
// shares no mutable admission budget between independent requests.
var configuredRedisGrantSelectionLimits = sync.OnceValues(func() (redisGrantSelectionLimits, error) {
	resource, err := server.Config.SimpleResource("redis_contract_admission.yml")
	if errors.Is(err, server.ErrResourceNotFound) {
		return redisGrantSelectionLimits{MaxRows: 4096, MaxSelected: 256}, nil
	}
	if err != nil {
		return redisGrantSelectionLimits{}, err
	}
	data, err := resource.BytesE()
	if err != nil {
		return redisGrantSelectionLimits{}, err
	}
	return parseRedisGrantSelectionLimits(data)
})

// Refuse ambiguous profiles before any reservation or SQL publication.
func parseRedisGrantSelectionLimits(data []byte) (redisGrantSelectionLimits, error) {
	var result redisGrantSelectionLimits
	if len(data) == 0 || len(data) > 4096 {
		return result, errors.New("Redis grant selection profile size invalid")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&result); err != nil {
		return result, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return result, errors.New("Redis grant selection profile must have one document")
	}
	return result, result.validate()
}

// Absolute ceilings keep even an explicitly enlarged request owner finite.
func (self redisGrantSelectionLimits) validate() error {
	if self.MaxRows < redisGrantPageRows || self.MaxRows > 65536 || self.MaxSelected < 1 || self.MaxSelected > 4096 || self.MaxSelected > self.MaxRows {
		return errors.New("Redis grant selection limits require rows64..65536 and selected1..min(rows,4096)")
	}
	return nil
}

// This error carries an incomplete observation, never an invented zero balance.
// The last examined tuple lets diagnostics distinguish progress from a restart.
type redisGrantSelectionCapacityError struct {
	Rows        int
	Selected    int
	Limits      redisGrantSelectionLimits
	LastBalance server.Id
}

func (self *redisGrantSelectionCapacityError) Error() string {
	return fmt.Sprintf("%v: rows=%d/%d selected=%d/%d last_balance=%s; increase the reviewed redis_contract_admission.yml profile or reduce request size", errRedisGrantSelectionCapacity, self.Rows, self.Limits.MaxRows, self.Selected, self.Limits.MaxSelected, self.LastBalance)
}

func (self *redisGrantSelectionCapacityError) Unwrap() error { return errRedisGrantSelectionCapacity }

// Only these exact pre-mutation Lua refusals allow another independently valid
// grant to fund the request. An uncertain transport result is never skipped.
func redisGrantCounterHeld(err error) bool {
	var reply redis.Error
	if !errors.As(err, &reply) {
		return false
	}
	return reply.Error() == "invalid reservation counter" || reply.Error() == "invalid reservation key type"
}

// Discovery releases each bounded SQL result and its connection before
// reserving in Redis. The existing active/network/end/start/id index supports
// the fallback keyset; request-local counters do not serialize unrelated
// payers or balances. Reservations are pending until the request's contract
// transaction has joined.
func selectRedisTransferBalances(ctx context.Context, admission *redisContractAdmission, reservation *redisPendingReservation,
	payerNetworkId, payerClientId server.Id, requested ByteCount) ([]*TransferEscrowBalance, Priority, error) {
	limits, ok := ctx.Value(redisGrantSelectionContextKey{}).(redisGrantSelectionLimits)
	if !ok {
		var err error
		limits, err = configuredRedisGrantSelectionLimits()
		if err != nil {
			return nil, 0, err
		}
	}
	if err := limits.validate(); err != nil {
		return nil, 0, err
	}
	now := server.NowUtc()
	rowsRead := 0
	last := server.Id{}
	selected := []*TransferEscrowBalance{}
	redisGrantSelectionLimitsMetric.WithLabelValues("rows").Set(float64(limits.MaxRows))
	redisGrantSelectionLimitsMetric.WithLabelValues("selected").Set(float64(limits.MaxSelected))
	defer func() {
		redisGrantSelectionFraction.WithLabelValues("rows").Observe(float64(rowsRead) / float64(limits.MaxRows))
		redisGrantSelectionFraction.WithLabelValues("selected").Observe(float64(len(selected)) / float64(limits.MaxSelected))
	}()
	// A payer this process recently refused as exhausted first tries to prove
	// the census's refusal with one read of each grant, reserving nothing
	// (subscription_redis_exhaustion.go). A rerun after this request's own
	// recovery released bytes skips the proof.
	if !admission.recoveryRerun() && exhaustedRedisPayers.recent(payerNetworkId, time.Now()) {
		rows, room, proven := proveRedisGrantExhaustion(ctx, admission, payerNetworkId, now, requested, limits)
		if proven {
			rowsRead = rows
			if rowsRead*5 >= limits.MaxRows*4 {
				redisContractReservationResults.WithLabelValues("selection", "capacity-warning").Inc()
			}
			exhaustedRedisPayers.note(payerNetworkId, time.Now())
			redisContractReservationResults.WithLabelValues("exhaustion-proof", "proven").Inc()
			return nil, 0, fmt.Errorf("%w (%d).", errRedisReservationInsufficient, room)
		}
		// the census decides this payer's requests for the hold
		exhaustedRedisPayers.noteUnproven(payerNetworkId, time.Now())
		redisContractReservationResults.WithLabelValues("exhaustion-proof", "unproven").Inc()
	}
	var priority Priority
	remaining := requested
	var held error
	heldBalanceIds := map[server.Id]bool{}
	warned := false
	warn := func() {
		if !warned && (rowsRead*5 >= limits.MaxRows*4 || len(selected)*5 >= limits.MaxSelected*4) {
			warned = true
			redisContractReservationResults.WithLabelValues("selection", "capacity-warning").Inc()
		}
	}
	capacity := func() error {
		redisContractReservationResults.WithLabelValues("selection", "capacity-held").Inc()
		return errors.Join(&redisGrantSelectionCapacityError{Rows: rowsRead, Selected: len(selected), Limits: limits, LastBalance: last}, held)
	}
	// After this request's first refusal, each batch's remaining candidates are
	// probed read-only, and a reservation the probe proves refused is skipped
	// (subscription_redis_grant_probe.go). Attempt order and the set of
	// considered grants are unchanged.
	probe := redisGrantProbe{contractId: admission.contractId}
	reserve := func(balance *escrowTransferBalance, whole bool) (bool, error) {
		if heldBalanceIds[balance.balanceId] || balance.balanceByteCount <= 0 {
			return false, nil
		}
		operation := "reserve-pending"
		if whole {
			operation = "reserve-pending-full"
		}
		if refused, idle := probe.refuses(balance.balanceId, remaining, whole); refused {
			// The reservation would have reserved nothing. A grant holding a
			// recovery or pending marker stays in this request's recovery
			// discovery, as a refused reservation keeps it; no token exists.
			if !idle {
				admission.noteBalance(balance.balanceId)
				admission.noteZeroReservation(balance.balanceId)
			}
			redisContractReservationResults.WithLabelValues(operation, "skipped").Inc()
			return false, nil
		}
		admission.noteBalance(balance.balanceId)
		reservation.noteReservationRequest()
		amount, err := redisContractReservation(ctx, operation, balance.balanceId, admission.contractId, balance.balanceByteCount, remaining, redisContractReservationLease)
		if err != nil {
			if redisGrantCounterHeld(err) {
				heldBalanceIds[balance.balanceId] = true
				held = err // One bounded diagnostic; original counters stay untouched.
				redisContractReservationResults.WithLabelValues("selection", "counter-held").Inc()
				return false, nil
			}
			return false, err
		}
		if amount == 0 {
			admission.noteZeroReservation(balance.balanceId)
			probe.noteRefusal()
			return false, nil
		}
		if whole && amount != remaining {
			return false, errors.New("whole-grant Redis reservation returned partial authority")
		}
		selected = append(selected, &TransferEscrowBalance{BalanceId: balance.balanceId, BalanceByteCount: amount})
		if balance.paid {
			priority += PaidPriority
		}
		remaining -= amount
		warn()
		return remaining == 0, nil
	}

	// Reuse the independently scoped free-grant policy. Each small window
	// preserves discovery order before the payer client's hash chooses a start.
	// The first window returns rows only for a spreading payer: the internal
	// prober, or a payer with at least two large free grants.
	internal := false
	spread := false
	// An internal prober records one finite selection outcome, as the legacy
	// allocator does; ordinary payers record none. A failure after the first
	// window identified the prober, including a raised query, remains "error".
	proberOutcome := ""
	defer func() {
		if proberOutcome != "" {
			proberGrantSelectionResults.WithLabelValues(proberOutcome).Inc()
		}
	}()
	for window := 0; window < 2; window++ {
		var candidates []*escrowTransferBalance
		sql := redisGrantSpreadSelectionSql
		args := []any{payerNetworkId, now, proberGrantFirstCount, ProberTransferBalanceTopUp}
		count := proberGrantFirstCount
		if window == 1 {
			sql = proberGrantExtendedSql
			args = []any{payerNetworkId, now, proberGrantExtendedCount, proberGrantFirstCount, ProberTransferBalanceTopUp}
			count = proberGrantExtendedCount
		}
		if limits.MaxRows-rowsRead < count {
			return nil, 0, capacity()
		}
		raw := 0
		readRedisGrants(ctx, func(query server.PgCanQuery) {
			rows, err := query.Query(ctx, sql, args...)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					balance := &escrowTransferBalance{}
					var preferred bool
					values := []any{&balance.balanceId, &balance.paid, &balance.balanceByteCount, &balance.startTime, &balance.endTime}
					if window == 0 {
						values = append(values, &internal)
						spread = true
					}
					values = append(values, &preferred)
					server.Raise(rows.Scan(values...))
					rowsRead++
					raw++
					last = balance.balanceId
					if preferred {
						candidates = append(candidates, balance)
					}
				}
			})
		})
		warn()
		if !spread {
			break
		}
		if internal && proberOutcome == "" {
			proberOutcome = "error"
		}
		if len(candidates) != 0 {
			start := int(payerClientId.Hash() % uint64(len(candidates)))
			attempts := make([]*escrowTransferBalance, 0, proberGrantAttemptsPerWindow)
			for offset := range candidates {
				balance := candidates[(start+offset)%len(candidates)]
				if requested <= balance.balanceByteCount {
					attempts = append(attempts, balance)
					if len(attempts) == proberGrantAttemptsPerWindow {
						break
					}
				}
			}
			probe.forget()
			for index, balance := range attempts {
				probe.observe(ctx, attempts[index:])
				done, err := reserve(balance, true)
				if err != nil {
					return nil, 0, err
				}
				if done {
					if internal {
						proberOutcome = "selected_first"
						if window == 1 {
							proberOutcome = "selected_extended"
						}
					}
					exhaustedRedisPayers.forget(payerNetworkId)
					return selected, priority, nil
				}
			}
		}
		if raw < count {
			break
		}
	}
	if internal {
		// The keyset census below is the prober's authoritative fallback.
		proberOutcome = "fallback"
	}

	// Ordinary/fallback funding keeps exact earliest-expiry order. Keyset
	// pages avoid repeatedly scanning an offset prefix and bound decoded rows.
	var cursor *escrowTransferBalance
	for {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if rowsRead == limits.MaxRows || len(selected) == limits.MaxSelected {
			return nil, 0, capacity()
		}
		count := min(redisGrantPageRows, limits.MaxRows-rowsRead)
		sql := escrowTransferBalanceSql
		args := []any{payerNetworkId, now}
		if cursor == nil {
			sql += ` ORDER BY end_time,start_time,balance_id LIMIT $3`
			args = append(args, count)
		} else {
			sql += ` AND (end_time,start_time,balance_id)>($3,$4,$5) ORDER BY end_time,start_time,balance_id LIMIT $6`
			args = append(args, cursor.endTime, cursor.startTime, cursor.balanceId, count)
		}
		page := []*escrowTransferBalance{}
		readRedisGrants(ctx, func(query server.PgCanQuery) {
			rows, err := query.Query(ctx, sql, args...)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					balance := &escrowTransferBalance{}
					server.Raise(rows.Scan(&balance.balanceId, &balance.paid, &balance.balanceByteCount, &balance.startTime, &balance.endTime))
					page = append(page, balance)
					rowsRead++
				}
			})
		})
		warn()
		probe.forget()
		for index, balance := range page {
			cursor, last = balance, balance.balanceId
			if len(selected) == limits.MaxSelected {
				return nil, 0, capacity()
			}
			probe.observe(ctx, page[index:])
			done, err := reserve(balance, false)
			if err != nil {
				return nil, 0, err
			}
			if done {
				exhaustedRedisPayers.forget(payerNetworkId)
				return selected, priority / Priority(len(selected)), nil
			}
		}
		if len(page) < count {
			if held != nil {
				return nil, 0, fmt.Errorf("Redis grant funding incomplete because a counter remains unknown: %w", held)
			}
			// Only a complete bounded census can authorize shrink-to-fit. A
			// capacity refusal or unknown counter is not proof of less funding.
			if _, ok := grantTransferEscrowByteCount(requested, requested-remaining); ok && len(selected) != 0 {
				exhaustedRedisPayers.forget(payerNetworkId)
				return selected, priority / Priority(len(selected)), nil
			}
			// The internal prober keeps its census and selection outcomes.
			if !internal {
				exhaustedRedisPayers.note(payerNetworkId, time.Now())
			}
			return nil, 0, fmt.Errorf("%w (%d).", errRedisReservationInsufficient, requested-remaining)
		}
	}
}

// A blocked endpoint, or the wait for the transaction, can outlive a grant.
// Admission rechecks the selected snapshot's time bounds after the endpoint
// wait; expiry never creates new credit.
func redisGrantSelectedCurrent(ctx context.Context, tx server.PgTx, ids []server.Id) error {
	var current bool
	server.Raise(tx.QueryRow(ctx, `SELECT count(*)=$2 FROM transfer_balance
		WHERE balance_id=ANY($1) AND active AND start_time<=clock_timestamp() AT TIME ZONE 'UTC'
		AND clock_timestamp() AT TIME ZONE 'UTC'<end_time`, ids, len(ids)).Scan(&current))
	if !current {
		return errors.New("selected Redis grant expired or changed before contract publication")
	}
	return nil
}
