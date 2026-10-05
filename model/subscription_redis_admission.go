package model

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

// This deliberately exchanges serializable reservation admission for bounded
// Redis coordination. Durable contract usage/debits retain their existing
// authority. Redis loss and a stale concurrent debit may over-admit; an
// old unmarked reservation may under-admit until its 24-hour lease expires.
// New request markers support bounded SQL-fenced recovery before lease expiry.
// Neither case puts creators behind a shared PostgreSQL financial row lock.
const redisContractReservationLease = 24 * time.Hour
const redisContractAdmissionTimeout = time.Second

var errRedisReservationInsufficient = errors.New("Insufficient balance")

var redisContractReservationResults = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_redis_contract_reservation_total",
	Help: "Redis reservation operations by finite outcome; includes token replays and is not committed contract throughput.",
}, []string{"operation", "result"})

var redisGrantSelectionLimitsMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "urnetwork_redis_contract_selection_limit",
	Help: "Declared per-request grant selection limit; immutable process config, not remaining financial balance.",
}, []string{"resource"})

var redisGrantSelectionFraction = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "urnetwork_redis_contract_selection_fraction",
	Help:    "Fraction of the request-owned grant selection capacity consumed, including held requests.",
	Buckets: []float64{0.1, 0.25, 0.5, 0.75, 0.8, 0.9, 1},
}, []string{"resource"})

func init() {
	prometheus.MustRegister(redisContractReservationResults, redisGrantSelectionLimitsMetric, redisGrantSelectionFraction)
}

type redisAdmissionContextKey struct{}

// The immutable request id follows transaction retries. State snapshots are
// safe if an internal caller retries that same context concurrently. PG fences
// publication, and cleanup runs after its own transaction owner has joined.
type redisContractAdmission struct {
	contractId           server.Id
	stateLock            sync.Mutex
	attemptedBalanceIds  []server.Id
	attemptedBalanceKVs  map[server.Id]bool
	compensationAttempts map[server.Id]uint64
	publicationStarted   bool
}

func (self *redisContractAdmission) noteBalance(balanceId server.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.compensationAttempts == nil {
		self.compensationAttempts = make(map[server.Id]uint64, len(self.attemptedBalanceIds))
		// Preexisting discovery state has unknown reservation ownership.
		for _, id := range self.attemptedBalanceIds {
			self.compensationAttempts[id] = 1
		}
	}
	// Register before Redis: a panic, timeout, or lost reply may have allocated
	// a token. Only this attempt's conclusive zero reply can discharge it.
	self.compensationAttempts[balanceId]++
	if self.attemptedBalanceKVs == nil {
		self.attemptedBalanceKVs = make(map[server.Id]bool, len(self.attemptedBalanceIds))
		for _, id := range self.attemptedBalanceIds {
			self.attemptedBalanceKVs[id] = true
		}
	}
	if !self.attemptedBalanceKVs[balanceId] {
		self.attemptedBalanceKVs[balanceId] = true
		self.attemptedBalanceIds = append(self.attemptedBalanceIds, balanceId)
	}
}

func (self *redisContractAdmission) noteZeroReservation(balanceId server.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.compensationAttempts[balanceId] > 0 {
		self.compensationAttempts[balanceId]--
	}
	// Keep discovery state: a full grant can contain another abandoned token
	// that retained recovery must discover before retrying insufficient credit.
}

func (self *redisContractAdmission) notePublication() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.publicationStarted = true
}

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
	return []string{base + "approx_reserved", base + "approx_contracts", base + "approx_expiry", netEscrowKey(balanceId), base + "approx_recovery_v2"}
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
for _,entry in ipairs({{2,'hash'},{3,'zset'},{5,'zset'}}) do
 local kind=redis.call('TYPE',KEYS[entry[1]]).ok
 if kind~='none' and kind~=entry[2] then return redis.error_reply('invalid reservation key type') end
end
if not valid(total) or not valid(legacy) or not valid(ARGV[3]) or not valid(ARGV[4]) then
 return redis.error_reply('invalid reservation counter')
end
local lease=tonumber(ARGV[5])
if not lease or lease<1 or lease>86400000 then return redis.error_reply('invalid reservation lease') end
-- Partial key eviction starts a new approximate generation too. Old releases
-- then find no token and cannot subtract a newly admitted neighbor's bytes.
if not rawTotal or (total~='0' and (redis.call('EXISTS',KEYS[2])==0 or redis.call('EXISTS',KEYS[3])==0)) then
 redis.call('DEL',KEYS[2],KEYS[3],KEYS[5]);total='0'
end
local now=redis.call('TIME'); local milliseconds=tonumber(now[1])*1000+math.floor(tonumber(now[2])/1000)
local expired=redis.call('ZRANGEBYSCORE',KEYS[3],'-inf',milliseconds,'LIMIT',0,32)
for _,token in ipairs(expired) do
 local amount=redis.call('HGET',KEYS[2],token)
 if amount then
  if not valid(amount) then return redis.error_reply('invalid reservation token') end
  total=sub(total,amount); redis.call('HDEL',KEYS[2],token)
 end
 redis.call('ZREM',KEYS[3],token); redis.call('ZREM',KEYS[5],token)
end
local existing=redis.call('HGET',KEYS[2],ARGV[2])
if existing and not valid(existing) then return redis.error_reply('invalid reservation token') end
local amount='0'
if ARGV[1]=='release-owned' and existing and not redis.call('ZSCORE',KEYS[5],ARGV[2]) then
 return redis.error_reply('missing owned reservation marker')
end
if (ARGV[1]=='release-owned' or ARGV[1]=='published') and existing and ARGV[4]~='0' and existing~=ARGV[4] then
 return redis.error_reply('reservation recovery amount changed')
end
if ARGV[1]=='release' or ARGV[1]=='release-owned' then
 if existing then
  if ARGV[1]=='release-owned' then amount=existing end
  total=sub(total,existing); redis.call('HDEL',KEYS[2],ARGV[2]); redis.call('ZREM',KEYS[3],ARGV[2])
 end
 redis.call('ZREM',KEYS[5],ARGV[2])
elseif ARGV[1]=='settle' then
 -- Never resurrect a token after the durable batch already released it.
 -- A missing/expired Redis generation retains its documented approximation.
 if existing then
  if less(existing,ARGV[4]) then return redis.error_reply('settled consumption exceeds reservation') end
  total=sub(total,sub(existing,ARGV[4])); amount=ARGV[4]
  redis.call('HSET',KEYS[2],ARGV[2],amount)
 end
elseif ARGV[1]=='reserve' or ARGV[1]=='reserve-owned' or ARGV[1]=='reserve-owned-full' or ARGV[1]=='restore' then
 if existing then
  if (ARGV[1]=='reserve-owned' or ARGV[1]=='reserve-owned-full') and not redis.call('ZSCORE',KEYS[5],ARGV[2]) then
   return redis.error_reply('missing owned reservation marker')
  end
  if less(ARGV[4],existing) then return redis.error_reply('reservation retry changed amount') end
  amount=existing
 else
  local available=sub(sub(ARGV[3],legacy),total)
  if ARGV[1]=='restore' then available=sub('9223372036854775807',total) end
  amount=ARGV[4]; if less(available,amount) then
   if ARGV[1]=='restore' then return redis.error_reply('reservation recovery overflow') end
   amount=available
   if ARGV[1]=='reserve-owned-full' then amount='0' end
  end
  if amount~='0' then
   -- The total remains <= the supplied signed-int64 credit, so INCRBY is exact.
   redis.call('SET',KEYS[1],total)
   redis.call('INCRBY',KEYS[1],amount); total=redis.call('GET',KEYS[1])
   redis.call('HSET',KEYS[2],ARGV[2],amount)
   redis.call('ZADD',KEYS[3],milliseconds+lease,ARGV[2])
   if ARGV[1]=='reserve-owned' or ARGV[1]=='reserve-owned-full' then redis.call('ZADD',KEYS[5],milliseconds,ARGV[2]) end
  end
 end
 if ARGV[1]=='restore' then redis.call('ZREM',KEYS[5],ARGV[2]) end
elseif ARGV[1]=='published' then
 redis.call('ZREM',KEYS[5],ARGV[2])
else return redis.error_reply('invalid reservation operation') end
-- Recovery may restore an older token with only a short lease remaining.
-- Never let that operation shorten the shared keys beneath younger tokens.
redis.call('SET',KEYS[1],total,'PX',90000000)
redis.call('PEXPIRE',KEYS[2],90000000)
redis.call('PEXPIRE',KEYS[3],90000000)
redis.call('PEXPIRE',KEYS[5],90000000)
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
	} else if (operation == "reserve" || operation == "reserve-owned" || operation == "reserve-owned-full") && amount == 0 {
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
	// The v2 marker is recoverable only while all writers of this exact request
	// hold this transaction-scoped fence. No shared payer/balance lock is added.
	server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, redisContractAdmissionLock(admission.contractId)))
	var published bool
	server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=$1)`, admission.contractId).Scan(&published))
	if published {
		admission.notePublication()
		return nil, nil, errors.New("Redis reservation request already has SQL custody; reconcile its original contract")
	}
	deadline, err := validateProberShardPayerInTx(ctx, tx, sourceNetworkId, destinationNetworkId, payerNetworkId)
	if err != nil {
		return nil, nil, err
	}
	leave := server.EnterContractCreationStage(ctx, server.ContractStageGrantSelection)
	payerClientId := sourceId
	if sourceNetworkId != payerNetworkId {
		payerClientId = destinationId
	}
	selected, priority, err := selectRedisTransferBalances(ctx, tx, admission, payerNetworkId, payerClientId, requested)
	leave()
	if err != nil {
		return nil, nil, err
	}
	if err := lockActiveContractClientsInTx(ctx, tx, sourceNetworkId, sourceId, destinationNetworkId, destinationId); err != nil {
		return nil, nil, err
	}
	if err := validateProberShardAdmissionDeadlineInTx(ctx, tx, deadline); err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	selectedIds := make([]server.Id, 0, len(selected))
	var granted ByteCount
	for _, balance := range selected {
		if balance.BalanceByteCount <= 0 || balance.BalanceByteCount > requested-granted {
			return nil, nil, errors.New("selected Redis reservation exceeds requested authority")
		}
		granted += balance.BalanceByteCount
		selectedIds = append(selectedIds, balance.BalanceId)
	}
	if amount, ok := grantTransferEscrowByteCount(requested, granted); !ok || amount != granted {
		return nil, nil, errors.New("selected Redis reservation does not cover the shrink floor")
	}
	if err := redisGrantSelectedCurrent(ctx, tx, selectedIds); err != nil {
		return nil, nil, err
	}
	// Per-contract rows only. The compatibility triggers omit marked rows from
	// legacy revision maintenance, and no shared snapshot is read or published.
	slices.SortFunc(selected, func(a, b *TransferEscrowBalance) int { return a.BalanceId.Cmp(b.BalanceId) })
	// From this point the transaction owner decides commit/rollback. A failed
	// round trip is not proof of absence, so compensation must not release a
	// possibly committed contract. Existing reconciliation/lease recovery owns it.
	admission.notePublication()
	server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
		for _, b := range selected {
			batch.Queue(`INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count,redis_reserved) VALUES($1,$2,$3,true)`, admission.contractId, b.BalanceId, b.BalanceByteCount)
		}
		batch.Queue(`INSERT INTO transfer_contract(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,companion_contract_id,payer_network_id,usage_origin_is_source,create_time,priority)
            VALUES($1,$2,$3,$4,$5,$6,$7,$8,($7::uuid IS NULL),clock_timestamp() AT TIME ZONE 'UTC',$9)`,
			admission.contractId, sourceNetworkId, sourceId, destinationNetworkId, destinationId, granted, companionId, payerNetworkId, priority)
		batch.Queue(contractExtenderInsertSql, admission.contractId, sourceId, destinationId, ContractPartySource, ContractPartyDestination)
	})
	providerWorkRetainReservationInTx(ctx, tx, admission.contractId)
	return &TransferEscrow{ContractId: admission.contractId, CompanionContractId: companionId, TransferByteCount: granted, Priority: priority, Balances: selected}, nil, nil
}

// reserveRedisTransferEscrowBalances reserves up to `requested` bytes from
// `balances` in order, through `reserve`, which atomically reserves at most
// `remaining` bytes of one balance's unreserved credit and returns the amount
// reserved (the Redis reservation script clamps to what concurrent contracts
// left available, so two creators never reserve the same bytes).
//
// The granted contract size is the reserved total when it covers the request,
// or shrinks to fit the reserved total when that is at least the shrink floor
// (see grantTransferEscrowByteCount). Below the floor every reservation this
// request made is released through `release` and the request is refused with
// "Insufficient balance". The selected escrow balances sum to the granted size.
func reserveRedisTransferEscrowBalances(
	balances []*escrowTransferBalance,
	requested ByteCount,
	reserve func(balance *escrowTransferBalance, remaining ByteCount) (ByteCount, error),
	release func(balanceId server.Id),
) (selected []*TransferEscrowBalance, granted ByteCount, priority Priority, err error) {
	selected = []*TransferEscrowBalance{}
	remaining := requested
	for _, balance := range balances {
		if balance.balanceByteCount <= 0 {
			continue
		}
		amount, err := reserve(balance, remaining)
		if err != nil {
			return nil, 0, 0, err
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
	reserved := requested - remaining
	granted, ok := grantTransferEscrowByteCount(requested, reserved)
	if !ok {
		// This is a known pre-write refusal, unlike an ambiguous commit. Release
		// only this request's tokens.
		for _, b := range selected {
			release(b.BalanceId)
		}
		return nil, 0, 0, fmt.Errorf("Insufficient balance (%d).", reserved)
	}
	priority /= Priority(len(selected))
	return selected, granted, priority, nil
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
