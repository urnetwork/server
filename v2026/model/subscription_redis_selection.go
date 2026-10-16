// Grant selection retains the payer's financial order and internal-prober
// spread. Only Redis reserves bytes; discovery never locks shared SQL grants.
package model

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
	"gopkg.in/yaml.v3"
)

const redisGrantPageRows = 64

var errRedisGrantSelectionCapacity = errors.New("Redis grant selection capacity reached; remaining funding is unknown")

// Limits are immutable values; each request owns its counters and cursor.
// A larger reviewed profile permits a subsequent fresh request to look farther.
// No grant, liability or reservation is deleted when an operation hits a cap.
type redisGrantSelectionLimits struct {
	MaxRows     int `yaml:"max_rows"`
	MaxSelected int `yaml:"max_selected"`
}

type redisGrantSelectionContextKey struct{}

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

// Discovery closes each bounded SQL result before reserving in Redis. The
// existing active/network/end/start/id index supports the fallback keyset;
// request-local counters do not serialize unrelated payers or balances.
func selectRedisTransferBalances(ctx context.Context, tx server.PgTx, admission *redisContractAdmission,
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
	reserve := func(balance *escrowTransferBalance, whole bool) (bool, error) {
		if heldBalanceIds[balance.balanceId] || balance.balanceByteCount <= 0 {
			return false, nil
		}
		admission.noteBalance(balance.balanceId)
		operation := "reserve-owned"
		if whole {
			operation = "reserve-owned-full"
		}
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
	internal := false
	for window := 0; window < 2; window++ {
		var candidates []*escrowTransferBalance
		sql := proberGrantSelectionSql
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
		rows, err := tx.Query(ctx, sql, args...)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				balance := &escrowTransferBalance{}
				var preferred bool
				values := []any{&balance.balanceId, &balance.paid, &balance.balanceByteCount, &balance.startTime, &balance.endTime}
				if window == 0 {
					values = append(values, &internal)
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
		warn()
		if !internal {
			break
		}
		if len(candidates) != 0 {
			start := int(payerClientId.Hash() % uint64(len(candidates)))
			attempts := 0
			for offset := range candidates {
				balance := candidates[(start+offset)%len(candidates)]
				if balance.balanceByteCount < requested {
					continue
				}
				attempts++
				done, err := reserve(balance, true)
				if err != nil {
					return nil, 0, err
				}
				if done {
					return selected, priority, nil
				}
				if attempts == proberGrantAttemptsPerWindow {
					break
				}
			}
		}
		if raw < count {
			break
		}
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
		rows, err := tx.Query(ctx, sql, args...)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				balance := &escrowTransferBalance{}
				server.Raise(rows.Scan(&balance.balanceId, &balance.paid, &balance.balanceByteCount, &balance.startTime, &balance.endTime))
				page = append(page, balance)
				rowsRead++
			}
		})
		warn()
		for _, balance := range page {
			cursor, last = balance, balance.balanceId
			if len(selected) == limits.MaxSelected {
				return nil, 0, capacity()
			}
			done, err := reserve(balance, false)
			if err != nil {
				return nil, 0, err
			}
			if done {
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
				return selected, priority / Priority(len(selected)), nil
			}
			return nil, 0, fmt.Errorf("%w (%d).", errRedisReservationInsufficient, requested-remaining)
		}
	}
}

// A blocked endpoint can outlive a grant. Admission rechecks the selected
// snapshot's time bounds after that wait; expiry never creates new credit.
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
