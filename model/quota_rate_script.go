package model

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// The {sq_<network_id>} script behind Quota.CountRate (DESIGN.md 20.7's Counters, closure, allow and
// wait rows; §11.12's rekey row; plan-QUOTA-small-readings reading 19): one all-or-none call per
// CountRate, every key under the network's one hash tag, so nothing crosses a slot. Go resolves each
// rate key through sidecar.ParseQuotaRateKey to its window, its limit and the refusal it would give,
// and the script counts:
//
//	debit    {sq_<n>}rate_debit:<debit_id>   the call's first answer, so a replay answers it again,
//	                                         a refusal included, however the windows stand by then
//	counter  {sq_<n>}rate:<rate key>         "<window index>:<used>", reset when its window turns
//	day      {sq_<n>}closure_bytes_day       "<utc day index>:<bytes>", the network's closure bytes
//	                                         counted today, for §21.7's closure-growth case; no limit
//	pace     {sq_<n>}closure_pace            hash: tail, the latest slot handed out, and second and
//	                                         taken, the uploads the network took in that second
//	turns    {sq_<n>}closure_turns           zset: each asking eraser's next turn, in µs
//
// Every key lives KeyTtl, §20.7's 48 h, past its last write. The pacing is sidecar.QuotaClosurePacing
// line for line, which is what FakeQuota runs, so quotasuite holds the two to one behavior: an eraser
// asking holds one slot on a tail spaced 1/R apart, a refused ask keeps its slot, a taken turn takes a
// new slot at the tail, a slot lapses the horizon past its turn, and a bucket of R a second bounds the
// network when erasers whose turns have passed come back together.
//
// Times reach the script as whole microseconds since the epoch, the caller's clock (so a fake clock
// moves every window), and stay below 2^53, where Lua's doubles are exact. The script writes every
// number it stores with %d, never as Lua would print it.

const quotaRateScriptSource = `
local first = redis.call('GET', KEYS[1])
if first then
  return first
end
local now = tonumber(ARGV[1])
local ttl = ARGV[2]
local count = tonumber(ARGV[3])
local paced = tonumber(ARGV[4])
local perS = tonumber(ARGV[5])
local horizon = tonumber(ARGV[6])
local eraser = ARGV[7]

local function decimal(value)
  return string.format('%d', value)
end

-- a new slot at the tail: 1/R after the latest, and never before now
local function nextSlot()
  local slot = now
  local tail = redis.call('HGET', KEYS[2], 'tail')
  if tail then
    local spaced = tonumber(tail) + math.floor(1000000 / perS)
    if now < spaced then
      slot = spaced
    end
  end
  redis.call('HSET', KEYS[2], 'tail', decimal(slot))
  return slot
end

-- the uploads the network took in the second numbered second
local function taken(second)
  local state = redis.call('HMGET', KEYS[2], 'second', 'taken')
  if state[1] and tonumber(state[1]) == second then
    return tonumber(state[2]) or 0
  end
  return 0
end

local refusal = false
local useds = {}
for i = 1, count do
  local at = 9 + (i - 1) * 5
  local amount = tonumber(ARGV[at + 1])
  local limit = tonumber(ARGV[at + 2])
  local used = 0
  local held = redis.call('GET', KEYS[3 + i])
  if held then
    local window, value = string.match(held, '^(%-?%d+):(%d+)$')
    if window == ARGV[at] then
      used = tonumber(value)
    end
  end
  useds[i] = used
  if not refusal and limit ~= -1 and limit < used + amount then
    refusal = ARGV[at + 3] .. '\t' .. decimal(used) .. '\t' .. ARGV[at + 4]
  end
  if i == paced then
    -- asked even past a refusal, so a refused eraser holds its slot and counts in k until its turn
    redis.call('ZREMRANGEBYSCORE', KEYS[3], '-inf', '(' .. decimal(now - horizon))
    local turn = redis.call('ZSCORE', KEYS[3], eraser)
    if turn then
      turn = tonumber(turn)
    else
      turn = nextSlot()
      redis.call('ZADD', KEYS[3], decimal(turn), eraser)
    end
    redis.call('PEXPIRE', KEYS[2], ttl)
    redis.call('PEXPIRE', KEYS[3], ttl)
    local second = math.floor(now / 1000000)
    local secondTaken = taken(second)
    local lift = false
    if now < turn then
      lift = turn
    elseif perS <= secondTaken then
      lift = (second + 1) * 1000000
    end
    if lift and not refusal then
      local retryMs = math.min(4294967295, math.max(1, math.ceil((lift - now) / 1000)))
      refusal = ARGV[8] .. '\t' .. decimal(secondTaken) .. '\t' .. decimal(retryMs)
    end
  end
end
if refusal then
  redis.call('SET', KEYS[1], refusal, 'PX', ttl)
  return refusal
end
for i = 1, count do
  local at = 9 + (i - 1) * 5
  redis.call('SET', KEYS[3 + i], ARGV[at] .. ':' .. decimal(useds[i] + tonumber(ARGV[at + 1])), 'PX', ttl)
end
if 0 < paced then
  local second = math.floor(now / 1000000)
  redis.call('HSET', KEYS[2], 'second', decimal(second), 'taken', decimal(taken(second) + 1))
  redis.call('ZADD', KEYS[3], decimal(nextSlot()), eraser)
  redis.call('PEXPIRE', KEYS[2], ttl)
  redis.call('PEXPIRE', KEYS[3], ttl)
end
redis.call('SET', KEYS[1], '0', 'PX', ttl)
return '0'
`

// The script, by the name a host's ServiceHost.Redis runs it under.
var quotaRateScript = &sidecar.RedisScript{
	Name:   "quota_rate",
	Source: quotaRateScriptSource,
}

// The record a passing call leaves under its debit id, and the script's answer for it.
const quotaRatePassedRecord = "0"

// ARGV before the first counter's, and per counter.
const (
	quotaRateHeadArgCount    = 8
	quotaRateCounterArgCount = 5
	// KEYS before the first counter's
	quotaRateHeadKeyCount = 3
)

// {sq_<network_id>}rate_debit:<debit_id>
func quotaRateDebitKey(networkId server.Id, debitId server.Id) string {
	return fmt.Sprintf("{sq_%s}rate_debit:%s", networkId, debitId)
}

// {sq_<network_id>}closure_pace
func quotaRateClosurePaceKey(networkId server.Id) string {
	return fmt.Sprintf("{sq_%s}closure_pace", networkId)
}

// {sq_<network_id>}closure_turns
func quotaRateClosureTurnsKey(networkId server.Id) string {
	return fmt.Sprintf("{sq_%s}closure_turns", networkId)
}

// {sq_<network_id>}rate:<rate key>
func quotaRateCounterKey(networkId server.Id, rateKey string) string {
	return fmt.Sprintf("{sq_%s}rate:%s", networkId, rateKey)
}

// {sq_<network_id>}closure_bytes_day
func quotaRateClosureBytesDayKey(networkId server.Id) string {
	return fmt.Sprintf("{sq_%s}closure_bytes_day", networkId)
}

// The family of each eraser's closure bytes: its keys also count on the network's day total.
var quotaRateClosureBytesFamily = func() string {
	rule, err := sidecar.ParseQuotaRateKey(sidecar.QuotaRateKeyClosureBytes(connect.Id(server.NewId())))
	if err != nil {
		panic(err)
	}
	return rule.Family
}()

// One counter of a call.
type quotaRateCount struct {
	key    string
	window int64
	amount int64
	// -1 for none
	limit int64
	// "<limit_key>\t<limit>\t<upgrade>", what a refusal by this counter says before its used and
	// retry_after_ms
	refusalHead  string
	retryAfterMs uint32
}

// One CountRate as the script takes it.
type quotaRateCall struct {
	keys []string
	args []string
}

// "<limit_key>\t<limit>\t<upgrade>" for refusal, which a record completes with used and retry_after_ms.
func quotaRateRefusalHead(refusal *sidecar.QuotaRefusal) string {
	upgrade := "0"
	if refusal.Upgrade {
		upgrade = "1"
	}
	return refusal.LimitKey + "\t" + strconv.FormatInt(refusal.Limit, 10) + "\t" + upgrade
}

// Whether a rule's refusal ships dark behind enforce_sidecar_limits (R-QUOTA-5, §20.5): a limit read
// from pro.yml with no floor, which an absent pro.yml reads as zero, so that while the switch is off it
// neither counts nor refuses and an absent pro.yml locks nobody out. The closure rates, allowance and
// pacing are not dark: they read a fixed number or a floor, and they bound what passes at the storage
// limit and while spent, and an empty balance refuses whatever the switch says (§20.5, §20.7's
// closure row). The erasure push class refuses nothing, so it is not dark either.
func quotaRateRuleDark(rule *sidecar.QuotaRateRule) bool {
	return rule.LimitKey != "" && rule.FloorLimitKey == "" && rule.MinLimit <= 0
}

// The script's call for one CountRate, or nil when nothing is left to count: every key dark while
// enforced is false. An unknown or misspelled key, a negative amount, a paced key counted other than
// once, or two paced keys, is the caller's bug and an error, as FakeQuota answers it.
func newQuotaRateCall(
	networkId server.Id,
	limits *sidecar.QuotaLimits,
	keyAmounts map[string]int64,
	debitId server.Id,
	now time.Time,
	settings *QuotaRateSettings,
	enforced bool,
) (*quotaRateCall, error) {
	if networkId == (server.Id{}) {
		return nil, errors.New("quota rate: a zero network id")
	}
	if debitId == (server.Id{}) {
		return nil, errors.New("quota rate: a zero debit id")
	}
	// the windows and the pacing read whole microseconds, as the script does
	now = now.UTC().Truncate(time.Microsecond)

	keyRules := map[string]*sidecar.QuotaRateRule{}
	pacedCount := 0
	for key, amount := range keyAmounts {
		rule, err := sidecar.ParseQuotaRateKey(key)
		if err != nil {
			return nil, err
		}
		if amount < 0 {
			return nil, fmt.Errorf("quota rate: a negative count %d of %s", amount, key)
		}
		if rule.Paced {
			if amount != 1 {
				return nil, fmt.Errorf("quota rate: %s counts one upload, not %d", key, amount)
			}
			pacedCount += 1
		}
		keyRules[key] = rule
	}
	if 1 < pacedCount {
		return nil, fmt.Errorf("quota rate: %d paced keys in one call", pacedCount)
	}

	keys := []string{}
	for key, rule := range keyRules {
		if !enforced && quotaRateRuleDark(rule) {
			continue
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil, nil
	}
	// FakeQuota's order: the first refusal in key order is the one a call answers
	slices.Sort(keys)

	counts := make([]*quotaRateCount, 0, len(keys)+1)
	pacedIndex := 0
	var pacedRule *sidecar.QuotaRateRule
	closureByteCount := int64(0)
	hasClosureBytes := false
	for _, key := range keys {
		rule := keyRules[key]
		amount := keyAmounts[key]
		window := rule.Window.Index(now)
		// the refusal this counter gives, but for its used, which the script fills in
		template := rule.Refusal(limits, 0, now)
		counts = append(counts, &quotaRateCount{
			key:          quotaRateCounterKey(networkId, key),
			window:       window,
			amount:       amount,
			limit:        rule.Limit(limits),
			refusalHead:  quotaRateRefusalHead(template),
			retryAfterMs: template.RetryAfterMs,
		})
		if rule.Paced {
			pacedIndex = len(counts)
			pacedRule = rule
		}
		if rule.Family == quotaRateClosureBytesFamily {
			hasClosureBytes = true
			closureByteCount += amount
		}
	}
	if hasClosureBytes {
		// the network's closure bytes today, which no limit bounds (§20.10 rejects a network-wide cap)
		counts = append(counts, &quotaRateCount{
			key:         quotaRateClosureBytesDayKey(networkId),
			window:      sidecar.QuotaRateWindowUtcDay.Index(now),
			amount:      closureByteCount,
			limit:       -1,
			refusalHead: "",
		})
	}

	perS := sidecar.QuotaClosurePacingPerS(limits)
	pacedEraser := ""
	pacingHead := ""
	if pacedRule != nil {
		pacedEraser = pacedRule.SubjectId.String()
		pacingHead = quotaRateRefusalHead(sidecar.QuotaClosurePacingRefusal(limits, 0, now, now))
	}

	call := &quotaRateCall{
		keys: make([]string, 0, quotaRateHeadKeyCount+len(counts)),
		args: make([]string, 0, quotaRateHeadArgCount+quotaRateCounterArgCount*len(counts)),
	}
	call.keys = append(
		call.keys,
		quotaRateDebitKey(networkId, debitId),
		quotaRateClosurePaceKey(networkId),
		quotaRateClosureTurnsKey(networkId),
	)
	call.args = append(
		call.args,
		strconv.FormatInt(now.UnixMicro(), 10),
		strconv.FormatInt(max(settings.KeyTtl.Milliseconds(), 1), 10),
		strconv.Itoa(len(counts)),
		strconv.Itoa(pacedIndex),
		strconv.FormatInt(perS, 10),
		strconv.FormatInt(settings.ClosureSlotHorizon.Microseconds(), 10),
		pacedEraser,
		pacingHead,
	)
	for _, count := range counts {
		call.keys = append(call.keys, count.key)
		call.args = append(
			call.args,
			strconv.FormatInt(count.window, 10),
			strconv.FormatInt(count.amount, 10),
			strconv.FormatInt(count.limit, 10),
			count.refusalHead,
			strconv.FormatUint(uint64(count.retryAfterMs), 10),
		)
	}
	return call, nil
}

// The script's answer: nil for a call that counted, else the refusal it recorded.
func parseQuotaRateReply(reply *sidecar.RedisReply) (*sidecar.QuotaRefusal, error) {
	if reply == nil || reply.Kind != sidecar.RedisReplyBulk {
		return nil, errors.New("quota rate: the script answered no record")
	}
	record := string(reply.Bulk)
	if record == quotaRatePassedRecord {
		return nil, nil
	}
	fields := strings.Split(record, "\t")
	if len(fields) != 5 || fields[0] == "" {
		return nil, fmt.Errorf("quota rate: a malformed record %q", record)
	}
	limit, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("quota rate: a malformed limit in %q", record)
	}
	used, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("quota rate: a malformed used in %q", record)
	}
	retryAfterMs, err := strconv.ParseUint(fields[4], 10, 32)
	if err != nil {
		return nil, fmt.Errorf("quota rate: a malformed retry_after_ms in %q", record)
	}
	return &sidecar.QuotaRefusal{
		LimitKey:     fields[0],
		Limit:        limit,
		Used:         used,
		RetryAfterMs: uint32(retryAfterMs),
		Upgrade:      fields[2] == "1",
	}, nil
}

// Where a Quota's scripts run: a host's ServiceHost.Redis, whose RunScript has this shape, or this
// process's own Redis (NewQuotaRateServerRedis). An error means Redis did not answer.
type QuotaRateRedis interface {
	RunScript(ctx context.Context, script *sidecar.RedisScript, keys []string, args []string) (*sidecar.RedisReply, error)
}

// This process's Redis as a QuotaRateRedis: one script call, by SHA-1 with a fallback to its source,
// on the non-retrying deadline pool with no preflight. A call whose context has no deadline is bounded
// by callTimeout. Safe for concurrent use.
func NewQuotaRateServerRedis(callTimeout time.Duration) QuotaRateRedis {
	return &quotaRateServerRedis{
		callTimeout: callTimeout,
	}
}

type quotaRateServerRedis struct {
	callTimeout time.Duration
	// each script's go-redis form, by name
	scripts sync.Map
}

func (self *quotaRateServerRedis) RunScript(ctx context.Context, script *sidecar.RedisScript, keys []string, args []string) (*sidecar.RedisReply, error) {
	if script == nil {
		return nil, errors.New("quota rate: no script")
	}
	value, _ := self.scripts.LoadOrStore(script.Name, redis.NewScript(script.Source))
	redisScript := value.(*redis.Script)
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, self.callTimeout)
		defer cancel()
	}
	argv := make([]any, len(args))
	for index, arg := range args {
		argv[index] = arg
	}
	var reply *sidecar.RedisReply
	err := server.RedisWithDeadline(ctx, func(client server.RedisClient) error {
		result, err := redisScript.Run(ctx, client, keys, argv...).Result()
		if errors.Is(err, redis.Nil) {
			reply = &sidecar.RedisReply{Kind: sidecar.RedisReplyNil}
			return nil
		}
		if err != nil {
			return err
		}
		switch result := result.(type) {
		case string:
			reply = &sidecar.RedisReply{Kind: sidecar.RedisReplyBulk, Bulk: []byte(result)}
		case int64:
			reply = &sidecar.RedisReply{Kind: sidecar.RedisReplyInteger, Integer: result}
		default:
			return fmt.Errorf("quota rate: a script reply of %T", result)
		}
		return nil
	}, server.OptRedisNoPreflight())
	if err != nil {
		return nil, err
	}
	return reply, nil
}
