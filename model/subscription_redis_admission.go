package model

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

// This deliberately exchanges serializable reservation admission for bounded
// Redis coordination. Durable contract usage/debits retain their existing
// authority. Redis loss and a stale concurrent debit may over-admit; an
// abandoned reservation may under-admit until its 24-hour lease expires.
// Neither case puts creators behind a shared PostgreSQL financial row lock.
const redisContractReservationLease = 24 * time.Hour
const redisContractAdmissionTimeout = time.Second

var redisContractReservationResults = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_redis_contract_reservation_total",
	Help: "Redis reservation operations by finite outcome; includes token replays and is not committed contract throughput.",
}, []string{"operation", "result"})

func init() { prometheus.MustRegister(redisContractReservationResults) }

type redisAdmissionContextKey struct{}
type redisContractAdmission struct{ contractId server.Id }

func redisAdmissionFromContext(ctx context.Context) *redisContractAdmission {
	admission, _ := ctx.Value(redisAdmissionContextKey{}).(*redisContractAdmission)
	return admission
}

func withRedisContractAdmission(ctx context.Context) context.Context {
	if redisAdmissionFromContext(ctx) != nil {
		return ctx
	}
	// A database serialization retry keeps the same Redis request identity.
	// Admission is unconditional after the compatibility rollout. No shared
	// policy read or process-local financial permit precedes this path.
	return context.WithValue(ctx, redisAdmissionContextKey{}, &redisContractAdmission{contractId: server.NewId()})
}

func redisContractReservationKeys(balanceId server.Id) []string {
	base := fmt.Sprintf("{escrow_%s}", balanceId)
	return []string{base + "approx_reserved", base + "approx_contracts", base + "approx_expiry", netEscrowKey(balanceId)}
}

// All keys occupy one Redis cluster slot. Monetary arithmetic uses decimal
// strings and Redis's signed integer operations, never Lua's floating point.
// Each operation expires at most 32 abandoned reservations; no history scan or
// database census occurs in admission. Successful release removes its token,
// so replay cannot release another contract after eviction/reconciliation.
const redisContractReservationScript = `
local function valid(s)
 return s and string.match(s, '^%d+$') and (s == '0' or string.sub(s,1,1) ~= '0') and
  (#s < 19 or (#s == 19 and s <= '9223372036854775807'))
end
local function less(a,b) return #a < #b or (#a == #b and a < b) end
local function sub(a,b)
 if less(a,b) then return '0' end
 local out = {}; local carry=0
 for i=0,#a-1 do
  local x=tonumber(string.sub(a,#a-i,#a-i))-carry
  local j=#b-i; local y=0; if j>0 then y=tonumber(string.sub(b,j,j)) end
  if x<y then x=x+10; carry=1 else carry=0 end
  table.insert(out,1,tostring(x-y))
 end
 local s=string.gsub(table.concat(out),'^0+',''); if s=='' then return '0' end; return s
end
local rawTotal=redis.call('GET',KEYS[1])
local total=rawTotal or '0'
local legacy=redis.call('GET',KEYS[4]) or '0'
if not valid(total) or not valid(legacy) or not valid(ARGV[3]) or not valid(ARGV[4]) then
 return redis.error_reply('invalid reservation counter')
end
local lease=tonumber(ARGV[5])
if not lease or lease<1 or lease>86400000 then return redis.error_reply('invalid reservation lease') end
-- Partial key eviction starts a new approximate generation too. Old releases
-- then find no token and cannot subtract a newly admitted neighbor's bytes.
if not rawTotal or (total~='0' and (redis.call('EXISTS',KEYS[2])==0 or redis.call('EXISTS',KEYS[3])==0)) then
 redis.call('DEL',KEYS[2],KEYS[3]);total='0'
end
local now=redis.call('TIME'); local milliseconds=tonumber(now[1])*1000+math.floor(tonumber(now[2])/1000)
local expired=redis.call('ZRANGEBYSCORE',KEYS[3],'-inf',milliseconds,'LIMIT',0,32)
for _,token in ipairs(expired) do
 local amount=redis.call('HGET',KEYS[2],token)
 if amount then
  if not valid(amount) then return redis.error_reply('invalid reservation token') end
  total=sub(total,amount); redis.call('HDEL',KEYS[2],token)
 end
 redis.call('ZREM',KEYS[3],token)
end
local existing=redis.call('HGET',KEYS[2],ARGV[2])
if existing and not valid(existing) then return redis.error_reply('invalid reservation token') end
local amount='0'
if ARGV[1]=='release' then
 if existing then
  total=sub(total,existing); redis.call('HDEL',KEYS[2],ARGV[2]); redis.call('ZREM',KEYS[3],ARGV[2])
 end
elseif ARGV[1]=='settle' then
 -- Never resurrect a token after the durable batch already released it.
 -- A missing/expired Redis generation retains its documented approximation.
 if existing then
  if less(existing,ARGV[4]) then return redis.error_reply('settled consumption exceeds reservation') end
  total=sub(total,sub(existing,ARGV[4])); amount=ARGV[4]
  redis.call('HSET',KEYS[2],ARGV[2],amount)
 end
elseif ARGV[1]=='reserve' or ARGV[1]=='restore' then
 if existing then
  if less(ARGV[4],existing) then return redis.error_reply('reservation retry changed amount') end
  amount=existing
 else
  local available=sub(sub(ARGV[3],legacy),total)
  if ARGV[1]=='restore' then available=sub('9223372036854775807',total) end
  amount=ARGV[4]; if less(available,amount) then
   if ARGV[1]=='restore' then return redis.error_reply('reservation recovery overflow') end
   amount=available
  end
  if amount~='0' then
   -- The total remains <= the supplied signed-int64 credit, so INCRBY is exact.
   redis.call('SET',KEYS[1],total)
   redis.call('INCRBY',KEYS[1],amount); total=redis.call('GET',KEYS[1])
   redis.call('HSET',KEYS[2],ARGV[2],amount)
   redis.call('ZADD',KEYS[3],milliseconds+lease,ARGV[2])
  end
 end
else return redis.error_reply('invalid reservation operation') end
-- Recovery may restore an older token with only a short lease remaining.
-- Never let that operation shorten the shared keys beneath younger tokens.
redis.call('SET',KEYS[1],total,'PX',90000000)
redis.call('PEXPIRE',KEYS[2],90000000)
redis.call('PEXPIRE',KEYS[3],90000000)
return amount
`

func redisContractReservation(ctx context.Context, operation string, balanceId, contractId server.Id,
	credit, requested ByteCount, lease time.Duration) (ByteCount, error) {
	bounded, cancel := context.WithTimeout(ctx, redisContractAdmissionTimeout)
	defer cancel()
	var amount ByteCount
	err := server.RedisWithDeadline(bounded, func(r server.RedisClient) error {
		value, err := r.Eval(bounded, redisContractReservationScript, redisContractReservationKeys(balanceId),
			operation, contractId.String(), strconv.FormatInt(credit, 10), strconv.FormatInt(requested, 10), lease.Milliseconds()).Text()
		if err != nil {
			return err
		}
		amount, err = strconv.ParseInt(value, 10, 64)
		if err != nil || amount < 0 || amount > requested {
			return fmt.Errorf("invalid reservation response")
		}
		return nil
	})
	result := "accepted"
	if err != nil {
		result = "error"
	} else if operation == "reserve" && amount == 0 {
		result = "refused"
	}
	redisContractReservationResults.WithLabelValues(operation, result).Inc()
	return amount, err
}

func releaseRedisContractReservations(ctx context.Context, contractId server.Id, ids []server.Id) {
	for _, id := range ids {
		_, err := redisContractReservation(ctx, "release", id, contractId, 0, 0, redisContractReservationLease)
		server.Raise(err)
	}
}

func createRedisTransferEscrowInTx(ctx context.Context, tx server.PgTx, admission *redisContractAdmission,
	sourceNetworkId, sourceId, destinationNetworkId, destinationId, payerNetworkId server.Id,
	requested ByteCount, companionId *server.Id) (*TransferEscrow, []func() any, error) {
	deadline, err := validateProberShardPayerInTx(ctx, tx, sourceNetworkId, destinationNetworkId, payerNetworkId)
	if err != nil {
		return nil, nil, err
	}
	var balances []*escrowTransferBalance
	leave := server.EnterContractCreationStage(ctx, server.ContractStageGrantSelection)
	rows, err := tx.Query(ctx, escrowTransferBalanceSql+` ORDER BY end_time,start_time,balance_id`, payerNetworkId, server.NowUtc())
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			b := &escrowTransferBalance{}
			server.Raise(rows.Scan(&b.balanceId, &b.paid, &b.balanceByteCount, &b.startTime, &b.endTime))
			balances = append(balances, b)
		}
	})
	leave()
	selected := []*TransferEscrowBalance{}
	remaining := requested
	var priority Priority
	for _, balance := range balances {
		if balance.balanceByteCount <= 0 {
			continue
		}
		amount, err := redisContractReservation(ctx, "reserve", balance.balanceId, admission.contractId, balance.balanceByteCount, remaining, redisContractReservationLease)
		if err != nil {
			return nil, nil, err
		}
		if amount == 0 {
			continue
		}
		selected = append(selected, &TransferEscrowBalance{BalanceId: balance.balanceId, BalanceByteCount: amount})
		if balance.paid {
			priority += PaidPriority
		}
		remaining -= amount
		if remaining == 0 {
			break
		}
	}
	if remaining != 0 {
		// This is a known pre-write refusal, unlike an ambiguous commit. Release
		// only this request's tokens; a failed compensation expires on its lease.
		for _, b := range selected {
			_, _ = redisContractReservation(ctx, "release", b.BalanceId, admission.contractId, 0, 0, redisContractReservationLease)
		}
		return nil, nil, fmt.Errorf("Insufficient balance (%d).", requested-remaining)
	}
	priority /= Priority(len(selected))
	if err := lockActiveContractClientsInTx(ctx, tx, sourceNetworkId, sourceId, destinationNetworkId, destinationId); err != nil {
		return nil, nil, err
	}
	if err := validateProberShardAdmissionDeadlineInTx(ctx, tx, deadline); err != nil {
		return nil, nil, err
	}
	// Per-contract rows only. The compatibility triggers omit marked rows from
	// legacy revision maintenance, and no shared snapshot is read or published.
	slices.SortFunc(selected, func(a, b *TransferEscrowBalance) int { return a.BalanceId.Cmp(b.BalanceId) })
	server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
		for _, b := range selected {
			batch.Queue(`INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count,redis_reserved) VALUES($1,$2,$3,true)`, admission.contractId, b.BalanceId, b.BalanceByteCount)
		}
		batch.Queue(`INSERT INTO transfer_contract(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,companion_contract_id,payer_network_id,usage_origin_is_source,create_time,priority)
            VALUES($1,$2,$3,$4,$5,$6,$7,$8,($7::uuid IS NULL),clock_timestamp() AT TIME ZONE 'UTC',$9)`,
			admission.contractId, sourceNetworkId, sourceId, destinationNetworkId, destinationId, requested, companionId, payerNetworkId, priority)
		batch.Queue(contractExtenderInsertSql, admission.contractId, sourceId, destinationId, ContractPartySource, ContractPartyDestination)
	})
	return &TransferEscrow{ContractId: admission.contractId, CompanionContractId: companionId, TransferByteCount: requested, Priority: priority, Balances: selected}, nil, nil
}

// ReconcileRedisContractReservation repairs one known contract, without shared
// financial locks or a whole-history scan. A close racing this read can briefly
// resurrect admission debt; its original 24-hour horizon is never extended.
// Replays are idempotent while the reservation token exists. This is exception
// recovery, not a financial authority or part of the healthy create hot path.
func ReconcileRedisContractReservation(ctx context.Context, contractId server.Id) {
	type reservation struct {
		id              server.Id
		amount          ByteCount
		terminal        bool
		ageMilliseconds float64
		pending         *ByteCount
	}
	var reservations []reservation
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT escrow.balance_id,escrow.balance_byte_count,contract.outcome IS NOT NULL,
            GREATEST(0,EXTRACT(epoch FROM (clock_timestamp() AT TIME ZONE 'UTC'-contract.create_time))*1000)::double precision,
            (SELECT debit_byte_count FROM transfer_debit_journal WHERE contract_id=contract.contract_id AND balance_id=escrow.balance_id AND NOT applied)
            FROM transfer_contract AS contract
            CROSS JOIN LATERAL (SELECT balance_id,balance_byte_count,redis_reserved FROM transfer_escrow
                WHERE contract_id=contract.contract_id OFFSET 0) AS escrow
            WHERE contract.contract_id=$1 AND escrow.redis_reserved AND escrow.balance_byte_count>0`, contractId)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var value reservation
				server.Raise(rows.Scan(&value.id, &value.amount, &value.terminal, &value.ageMilliseconds, &value.pending))
				reservations = append(reservations, value)
			}
		})
	})
	for _, value := range reservations {
		remaining := redisContractReservationLease - time.Duration(value.ageMilliseconds)*time.Millisecond
		operation := "restore"
		if value.terminal {
			operation = "release"
			if value.pending != nil {
				operation = "settle"
				value.amount = *value.pending
			}
			remaining = redisContractReservationLease
		} else if remaining < time.Millisecond {
			operation = "release"
			remaining = redisContractReservationLease
		}
		_, err := redisContractReservation(ctx, operation, value.id, contractId, value.amount, value.amount, remaining)
		server.Raise(err)
	}
}

func readRedisContractReservations(ctx context.Context, ids []server.Id) map[server.Id]ByteCount {
	result := make(map[server.Id]ByteCount, len(ids))
	if len(ids) == 0 {
		return result
	}
	server.RedisDoOnce(ctx, func(r server.RedisClient) {
		commands := map[server.Id]*redis.StringCmd{}
		_, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for _, id := range ids {
				commands[id] = pipe.Get(ctx, redisContractReservationKeys(id)[0])
			}
			return nil
		})
		if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}
		for id, command := range commands {
			amount, err := command.Int64()
			if errors.Is(err, redis.Nil) {
				amount = 0
			} else {
				server.Raise(err)
			}
			if amount < 0 {
				server.Raise(fmt.Errorf("negative Redis reservation"))
			}
			result[id] = amount
		}
	})
	return result
}
