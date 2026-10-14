// Optional destination evidence crosses a bounded Redis handoff. Immutable
// records remain until a durable task cursor acknowledges their SQL projection.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server"
)

const ProviderEgressTallyShardCount = 16

// Bounds belong to each independent place shard; no process-wide admission
// semaphore or per-result goroutine is introduced. A record fits in one page.
const (
	providerEgressTallyRecordBytes = 32 * 1024
	providerEgressTallyQueueBytes  = 16 * 1024 * 1024
	providerEgressTallyQueueLength = 16384
	providerEgressTallyPageBytes   = 256 * 1024
	providerEgressTallyPageLength  = 256
	providerEgressTallyIoTimeout   = 250 * time.Millisecond
	providerEgressTallyLossLength  = 1024
	providerEgressTallyLossCarry   = 64
)

// One claim of a shard's owner drains up to a full shard. The owner's claim
// waits on the shared task queue, tens of seconds on Main, so one page per
// claim capped a shard near five runs a second. Each Redis call still returns
// at most one page and deletes at most one page of the acknowledged prefix,
// keeping every script at the single-page bound; the calls of one drain
// coalesce into one finishing transaction of at most
// providerEgressTallyDrainRows shared rows, or one page's rows when the first
// page alone exceeds it.
const (
	providerEgressTallyDrainCalls   = providerEgressTallyQueueLength / providerEgressTallyPageLength
	providerEgressTallyDrainLength  = providerEgressTallyDrainCalls * providerEgressTallyPageLength
	providerEgressTallyDrainRows    = 4096
	providerEgressTallyDrainTimeout = 5 * time.Second
)

var providerEgressTallyHandoffs = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_provider_egress_tally_handoffs_total",
	Help: "Auxiliary tally handoffs by finite accepted, unavailable, invalid or full outcome; never measured quota credits.",
}, []string{"outcome"})

var providerEgressTallyBacklogs = newProviderEgressTallyBacklogCollector(time.Now)

func init() {
	prometheus.MustRegister(providerEgressTallyHandoffs, providerEgressTallyBacklogs)
}

// Carries only aggregate inputs, never a provider identity or credential.
type ProviderEgressTallyRecord struct {
	MeasuredAt time.Time                `json:"measured_at"`
	Run        ProviderEgressRunTally   `json:"run"`
	Loads      []ProviderEgressSiteLoad `json:"loads"`
}

// One immutable prefix and its queue generation. NextCursor advances only
// with the additive SQL updates in the owning task's finishing transaction.
type ProviderEgressTallyPage struct {
	Generation string                      `json:"generation"`
	Cursor     string                      `json:"cursor"`
	NextCursor string                      `json:"next_cursor"`
	Records    []ProviderEgressTallyRecord `json:"records"`
}

// A lost whole run makes its day/place sample incomplete. It does not veto
// decisions supported by other days or places. No provider identity is retained.
type ProviderEgressTallyExclusion struct {
	Day         string `json:"day"`
	CountryCode string `json:"country_code"`
	Region      string `json:"region"`
}

// Safe for concurrent turns of one pass. At most 64 precise losses are carried
// to the next reachable write on their own shard. Overflow is observable, not
// permission to stop unrelated refreshes. Process death can lose this carry.
type ProviderEgressTallyWriter struct {
	stateLock      sync.Mutex
	lostScopeKVs   map[ProviderEgressTallyExclusion]bool
	overflowShards [ProviderEgressTallyShardCount]bool
}

func NewProviderEgressTallyWriter() *ProviderEgressTallyWriter {
	return &ProviderEgressTallyWriter{lostScopeKVs: map[ProviderEgressTallyExclusion]bool{}}
}

func (self *ProviderEgressTallyWriter) rememberLoss(scope ProviderEgressTallyExclusion) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.lostScopeKVs[scope] || len(self.lostScopeKVs) < providerEgressTallyLossCarry {
			self.lostScopeKVs[scope] = true
		} else {
			shard := ProviderEgressTallyShard(ProviderEgressPlace{CountryCode: scope.CountryCode, Region: scope.Region})
			self.overflowShards[shard] = true
		}
	}()
}

// Transfers only this shard's carry out of the lock; no network call holds it.
func (self *ProviderEgressTallyWriter) takeLoss(shard int) ([]ProviderEgressTallyExclusion, bool) {
	var scopes []ProviderEgressTallyExclusion
	var overflow bool
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for scope := range self.lostScopeKVs {
			if ProviderEgressTallyShard(ProviderEgressPlace{CountryCode: scope.CountryCode, Region: scope.Region}) == shard {
				scopes = append(scopes, scope)
				delete(self.lostScopeKVs, scope)
			}
		}
		overflow = self.overflowShards[shard]
		self.overflowShards[shard] = false
	}()
	return scopes, overflow
}

// Normalizes the existing daily SQL key before routing or serializing it.
func providerEgressTallyPlace(place ProviderEgressPlace) (ProviderEgressPlace, error) {
	place.CountryCode = strings.ToLower(strings.TrimSpace(place.CountryCode))
	place.Region = strings.TrimSpace(place.Region)
	if len(place.Region) > 128 {
		place.Region = place.Region[:128]
	}
	if len(place.CountryCode) > 2 || !utf8.ValidString(place.CountryCode) || !utf8.ValidString(place.Region) {
		return ProviderEgressPlace{}, fmt.Errorf("invalid tally place")
	}
	return place, nil
}

// A place has one rollup owner across days, avoiding competing daily writers.
func ProviderEgressTallyShard(place ProviderEgressPlace) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(place.CountryCode + "\x00" + place.Region))
	return int(h.Sum32() % ProviderEgressTallyShardCount)
}

// Queue and loss keys share an explicit cluster slot. The namespace is a wire version.
func providerEgressTallyKeys(shard int) []string {
	return []string{fmt.Sprintf("provider_egress_tally:{v1:%d}:meta", shard), fmt.Sprintf("provider_egress_tally:{v1:%d}:records", shard), fmt.Sprintf("provider_egress_tally:{v1:%d}:loss", shard)}
}

// Admission and known-loss marking are atomic. No trimming can discard an
// unacknowledged record to make room. An ambiguous reply is never replayed.
const providerEgressTallyAppendScript = `
local now = tonumber(ARGV[1])
local function mark(scope)
 if redis.call('ZSCORE',KEYS[3],scope) or redis.call('ZCARD',KEYS[3]) < tonumber(ARGV[8]) then
  redis.call('ZADD',KEYS[3],now,scope)
 else
  redis.call('HSET',KEYS[1],'unscoped_loss_at',now)
 end
end
for _,scope in ipairs(cjson.decode(ARGV[6])) do mark(scope) end
if ARGV[9] == '1' then redis.call('HSET',KEYS[1],'unscoped_loss_at',now) end
local generation = redis.call('HGET', KEYS[1], 'generation')
if not generation or redis.call('HGET', KEYS[1], 'ready') ~= '1' then mark(ARGV[7]);return 'unavailable' end
local count = tonumber(redis.call('HGET', KEYS[1], 'count') or '-1')
local tail = redis.call('HGET', KEYS[1], 'tail')
if count ~= redis.call('XLEN', KEYS[2]) or not tail or (tail ~= '0-0' and redis.call('EXISTS', KEYS[2]) == 0) then
 redis.call('HSET', KEYS[1], 'ready', 0)
 mark(ARGV[7]);return 'unavailable'
end
if #ARGV[2] > tonumber(ARGV[5]) then mark(ARGV[7]);return 'invalid' end
local bytes = tonumber(redis.call('HGET', KEYS[1], 'bytes') or '-1')
if bytes < 0 then mark(ARGV[7]);return 'unavailable' end
if count >= tonumber(ARGV[3]) or bytes + #ARGV[2] > tonumber(ARGV[4]) then
 mark(ARGV[7]);return 'full'
end
local tail = redis.call('XADD', KEYS[2], '*', 'record', ARGV[2])
redis.call('HINCRBY', KEYS[1], 'bytes', #ARGV[2])
redis.call('HINCRBY', KEYS[1], 'count', 1)
redis.call('HSET', KEYS[1], 'tail', tail)
return 'accepted'
`

// Takes a bounded copy into Redis after health acceptance. Errors are explicit
// auxiliary evidence loss, with no synchronous PostgreSQL fallback or retry.
func (self *ProviderEgressTallyWriter) Record(ctx context.Context, measuredAt time.Time, run ProviderEgressRunTally, loads []ProviderEgressSiteLoad) error {
	place, err := providerEgressTallyPlace(run.Place)
	if err != nil || measuredAt.IsZero() {
		providerEgressTallyHandoffs.WithLabelValues("invalid").Inc()
		return fmt.Errorf("invalid tally record")
	}
	run.Place = place
	scope := ProviderEgressTallyExclusion{Day: measuredAt.UTC().Format(time.DateOnly), CountryCode: place.CountryCode, Region: place.Region}
	// The existing writer ignores invalid names and counts repeated names.
	validLoads := make([]ProviderEgressSiteLoad, 0, min(len(loads), 512))
	for _, load := range loads {
		load.Name = strings.TrimSpace(load.Name)
		if load.Name != "" && len(load.Name) <= 128 && utf8.ValidString(load.Name) {
			validLoads = append(validLoads, load)
		}
		if len(validLoads) > 512 {
			self.rememberLoss(scope)
			providerEgressTallyHandoffs.WithLabelValues("invalid").Inc()
			return fmt.Errorf("tally record exceeds bounded site count")
		}
	}
	raw, err := json.Marshal(ProviderEgressTallyRecord{MeasuredAt: measuredAt.UTC(), Run: run, Loads: validLoads})
	if err != nil {
		self.rememberLoss(scope)
		providerEgressTallyHandoffs.WithLabelValues("invalid").Inc()
		return err
	}
	shard := ProviderEgressTallyShard(place)
	lostScopes, overflow := self.takeLoss(shard)
	// Marshal each member once: stable bytes make repeated markers idempotent.
	lostMembers := make([]string, 0, len(lostScopes))
	for _, lost := range lostScopes {
		member, _ := json.Marshal(lost)
		lostMembers = append(lostMembers, string(member))
	}
	lostJson, _ := json.Marshal(lostMembers)
	scopeJson, _ := json.Marshal(scope)
	ctx, cancel := context.WithTimeout(ctx, providerEgressTallyIoTimeout)
	defer cancel()
	var outcome string
	err = server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		var redisErr error
		outcome, redisErr = r.Eval(ctx, providerEgressTallyAppendScript, providerEgressTallyKeys(shard),
			time.Now().Unix(), string(raw), providerEgressTallyQueueLength, providerEgressTallyQueueBytes,
			providerEgressTallyRecordBytes, string(lostJson), string(scopeJson), providerEgressTallyLossLength, map[bool]string{false: "0", true: "1"}[overflow]).Text()
		return redisErr
	})
	if err != nil || outcome != "accepted" {
		for _, lost := range lostScopes {
			self.rememberLoss(lost)
		}
		self.rememberLoss(scope)
		if overflow {
			func() { self.stateLock.Lock(); defer self.stateLock.Unlock(); self.overflowShards[shard] = true }()
		}
		if err != nil || (outcome != "full" && outcome != "invalid") {
			outcome = "unavailable"
		}
		providerEgressTallyHandoffs.WithLabelValues(outcome).Inc()
		if err != nil {
			return err
		}
		return fmt.Errorf("tally handoff %s", outcome)
	}
	providerEgressTallyHandoffs.WithLabelValues("accepted").Inc()
	return nil
}

// Initializes only an empty first generation. Missing metadata after a durable
// cursor has advanced is loss, not permission to start again at zero. Cleanup
// deletes at most ARGV[9] records at or before the durable cursor ARGV[2], then
// returns at most one immutable page after the read cursor ARGV[8], which a
// drain advances past its durable cursor without acknowledging anything. The
// reply heads with the admission occupancy left after cleanup, the oldest
// unacknowledged record's millisecond, whether a record follows the page, and
// whether acknowledged records remain to delete.
const providerEgressTallyReadScript = `
local generation = redis.call('HGET', KEYS[1], 'generation')
-- Only a committed repair successor supplies a different prior generation.
-- Unlink is bounded by the existing queue cap and frees its payload off-thread.
if ARGV[7] ~= '' and generation ~= ARGV[1] then
 if ARGV[6] ~= '1' or ARGV[2] ~= '0-0' or (generation and generation ~= ARGV[7]) then return redis.error_reply('tally generation mismatch') end
 redis.call('UNLINK',KEYS[2])
 redis.call('HSET',KEYS[1],'generation',ARGV[1],'bytes',0,'ready',0,'tail','0-0','count',0,'observed_at',0,'pending_since',0,'unscoped_loss_at',ARGV[5])
 generation = ARGV[1]
end
if not generation then
 if ARGV[6] ~= '1' or ARGV[2] ~= '0-0' or redis.call('EXISTS', KEYS[2]) ~= 0 then return redis.error_reply('tally continuity missing') end
 redis.call('HSET', KEYS[1], 'generation', ARGV[1], 'bytes', 0, 'ready', 0, 'tail', '0-0', 'count', 0)
elseif generation ~= ARGV[1] then return redis.error_reply('tally generation mismatch') end
local count = tonumber(redis.call('HGET', KEYS[1], 'count') or '-1')
if count ~= redis.call('XLEN', KEYS[2]) then
 redis.call('HSET', KEYS[1], 'ready', 0)
 return redis.error_reply('tally record count mismatch')
end
local tail = redis.call('HGET', KEYS[1], 'tail')
if not tail then return redis.error_reply('tally tail missing') end
local function greater(a,b)
 local am,as = string.match(a,'^(%d+)%-(%d+)$')
 local bm,bs = string.match(b,'^(%d+)%-(%d+)$')
 if #am ~= #bm then return #am > #bm end
 if am ~= bm then return am > bm end
 if #as ~= #bs then return #as > #bs end
 return as > bs
end
if greater(ARGV[2],tail) then return redis.error_reply('tally cursor exceeds retained tail') end
if greater(ARGV[2],ARGV[8]) or greater(ARGV[8],tail) then return redis.error_reply('tally read cursor outside retained range') end
if tail ~= '0-0' then
 if redis.call('EXISTS', KEYS[2]) == 0 then redis.call('HSET',KEYS[1],'ready',0); return redis.error_reply('tally records missing') end
 local info = redis.call('XINFO', 'STREAM', KEYS[2])
 local actual = nil
 for i=1,#info,2 do if info[i] == 'last-generated-id' then actual=info[i+1] end end
 if actual ~= tail then redis.call('HSET',KEYS[1],'ready',0); return redis.error_reply('tally stream continuity mismatch') end
end
if ARGV[6] == '0' then redis.call('HSET', KEYS[1], 'ready', 1) end
local old = redis.call('XRANGE', KEYS[2], '-', ARGV[2], 'COUNT', ARGV[9])
for _, entry in ipairs(old) do
 redis.call('HINCRBY', KEYS[1], 'bytes', -#entry[2][2])
 redis.call('HINCRBY', KEYS[1], 'count', -1)
 redis.call('XDEL', KEYS[2], entry[1])
end
local result = {'', '', '', '', ''}
local bytes = 0
local cursor = ARGV[8]
for i=1,tonumber(ARGV[3]) do
 local page = redis.call('XRANGE', KEYS[2], '(' .. cursor, '+', 'COUNT', 1)
 if #page == 0 then break end
 local entry = page[1]
 bytes = bytes + #entry[2][2]
 if bytes > tonumber(ARGV[4]) then break end
 table.insert(result, entry[1]); table.insert(result, entry[2][2])
 cursor = entry[1]
end
redis.call('HSET', KEYS[1], 'observed_at', ARGV[5])
local first = redis.call('XRANGE', KEYS[2], '(' .. ARGV[2], '+', 'COUNT', 1)
local pending = #first == 0 and '0' or string.match(first[1][1], '^(%d+)')
redis.call('HSET', KEYS[1], 'pending_since', pending)
result[1] = redis.call('HGET', KEYS[1], 'count') or '-1'
result[2] = redis.call('HGET', KEYS[1], 'bytes') or '-1'
result[3] = pending
result[4] = tostring(#redis.call('XRANGE', KEYS[2], '(' .. cursor, '+', 'COUNT', 1))
result[5] = tostring(#redis.call('XRANGE', KEYS[2], '-', ARGV[2], 'COUNT', 1))
return result
`

// Validates a Redis stream cursor without float conversion or lexical ordering.
func CompareProviderEgressTallyCursor(a, b string) (int, error) {
	parse := func(value string) ([2]uint64, error) {
		var result [2]uint64
		parts := strings.Split(value, "-")
		if len(parts) != 2 {
			return result, fmt.Errorf("invalid tally cursor")
		}
		for i, part := range parts {
			v, err := strconv.ParseUint(part, 10, 64)
			if err != nil || strconv.FormatUint(v, 10) != part {
				return result, fmt.Errorf("invalid tally cursor")
			}
			result[i] = v
		}
		return result, nil
	}
	x, err := parse(a)
	if err != nil {
		return 0, err
	}
	y, err := parse(b)
	if err != nil {
		return 0, err
	}
	for i := range x {
		if x[i] < y[i] {
			return -1, nil
		}
		if x[i] > y[i] {
			return 1, nil
		}
	}
	return 0, nil
}

// Validates an owner's durable arguments and returns its repair predecessor.
func providerEgressTallyOwnerPrior(shard int, generation, cursor string, initialize bool, repairFrom []string) (string, error) {
	if shard < 0 || shard >= ProviderEgressTallyShardCount || generation == "" {
		return "", fmt.Errorf("invalid tally owner")
	}
	if _, err := CompareProviderEgressTallyCursor(cursor, "0-0"); err != nil {
		return "", err
	}
	prior := ""
	if len(repairFrom) > 1 {
		return "", fmt.Errorf("invalid tally repair")
	}
	if len(repairFrom) == 1 {
		prior = repairFrom[0]
	}
	if prior != "" && (!initialize || cursor != "0-0" || prior == generation) {
		return "", fmt.Errorf("invalid tally repair")
	}
	return prior, nil
}

// Reads one finite prefix; it never acknowledges a speculative cursor. The
// caller obtains cursor/generation from its durable task arguments only. The
// owner drains through DrainProviderEgressTallies; this single call remains
// for fixtures that inspect one page.
func readProviderEgressTallyPage(ctx context.Context, shard int, generation, cursor string, initialize bool, repairFrom ...string) (*ProviderEgressTallyPage, error) {
	prior, err := providerEgressTallyOwnerPrior(shard, generation, cursor, initialize, repairFrom)
	if err != nil {
		return nil, err
	}
	call, err := readProviderEgressTallyCall(ctx, shard, generation, cursor, cursor, providerEgressTallyPageLength, initialize, prior)
	if err != nil {
		return nil, err
	}
	return &ProviderEgressTallyPage{Generation: generation, Cursor: cursor, NextCursor: call.nextCursor, Records: call.records}, nil
}

// Admission occupancy of one shard at an owner read, after its cleanup.
// Acknowledged records not yet deleted still count against admission, so
// they are included. PendingSince is zero when nothing is unacknowledged.
type ProviderEgressTallyBacklog struct {
	Records      int64
	Bytes        int64
	PendingSince time.Time
	ObservedAt   time.Time
}

// Fraction of whichever bound refuses the next handoff first; refusal as
// full begins at one.
func (self ProviderEgressTallyBacklog) FillRatio() float64 {
	return max(float64(self.Records)/float64(providerEgressTallyQueueLength), float64(self.Bytes)/float64(providerEgressTallyQueueBytes))
}

// One script call of an owner read. More reports a retained record after
// nextCursor; dirty reports acknowledged records left to delete.
type providerEgressTallyCall struct {
	records    []ProviderEgressTallyRecord
	nextCursor string
	more       bool
	dirty      bool
	backlog    ProviderEgressTallyBacklog
}

// One bounded script call. Cleanup is limited to the durable cursor ackCursor;
// the page starts after fromCursor, at or after it. Nothing is acknowledged.
func readProviderEgressTallyCall(ctx context.Context, shard int, generation, ackCursor, fromCursor string, pageLength int, initialize bool, prior string) (*providerEgressTallyCall, error) {
	if order, err := CompareProviderEgressTallyCursor(fromCursor, ackCursor); err != nil || order < 0 {
		return nil, fmt.Errorf("invalid tally read cursor")
	}
	if pageLength < 0 || pageLength > providerEgressTallyPageLength {
		return nil, fmt.Errorf("invalid tally page length")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var values []string
	err := server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		var redisErr error
		values, redisErr = r.Eval(ctx, providerEgressTallyReadScript, providerEgressTallyKeys(shard), generation, ackCursor,
			pageLength, providerEgressTallyPageBytes, time.Now().Unix(), map[bool]string{false: "0", true: "1"}[initialize], prior,
			fromCursor, providerEgressTallyPageLength).StringSlice()
		return redisErr
	})
	if err != nil {
		return nil, err
	}
	const headLength = 5
	if len(values) < headLength || (len(values)-headLength)%2 != 0 || len(values)-headLength > pageLength*2 {
		return nil, fmt.Errorf("invalid tally page")
	}
	records, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid tally page")
	}
	bytes, err := strconv.ParseInt(values[1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid tally page")
	}
	pendingMillis, err := strconv.ParseInt(values[2], 10, 64)
	if err != nil || pendingMillis < 0 || (values[3] != "0" && values[3] != "1") || (values[4] != "0" && values[4] != "1") {
		return nil, fmt.Errorf("invalid tally page")
	}
	call := &providerEgressTallyCall{
		nextCursor: fromCursor,
		more:       values[3] == "1",
		dirty:      values[4] == "1",
		backlog:    ProviderEgressTallyBacklog{Records: records, Bytes: bytes, ObservedAt: time.Now()},
	}
	if 0 < pendingMillis {
		call.backlog.PendingSince = time.UnixMilli(pendingMillis)
	}
	for i := headLength; i < len(values); i += 2 {
		order, err := CompareProviderEgressTallyCursor(values[i], call.nextCursor)
		if err != nil || order <= 0 {
			return nil, fmt.Errorf("nonmonotone tally page")
		}
		var record ProviderEgressTallyRecord
		if len(values[i+1]) > providerEgressTallyRecordBytes || json.Unmarshal([]byte(values[i+1]), &record) != nil ||
			record.MeasuredAt.IsZero() || len(record.Loads) > 512 || ProviderEgressTallyShard(record.Run.Place) != shard {
			return nil, fmt.Errorf("invalid retained tally record")
		}
		call.records = append(call.records, record)
		call.nextCursor = values[i]
	}
	return call, nil
}

// One owner execution's bounded read: whole pages coalesced into one batch
// that the finishing transaction commits with NextCursor. Stop names why
// reading ended: caught_up, initialize, or a budget (calls, rows, time) or
// error that leaves a known backlog for the next claim. Err is a later call's
// failure, while reading or while cleaning after it; the pages before it are
// complete and immutable, so they still commit and the next claim rereads
// from there. Backlog is the first call's occupancy, after at most one page
// of cleanup: within one page of what producers met when the claim began.
// Later calls report the trough the drain's own cleanup leaves.
type ProviderEgressTallyDrain struct {
	NextCursor string
	Batch      *ProviderEgressTallyBatch
	Records    int
	Pages      int
	Calls      int
	Stop       string
	Err        error
	Backlog    ProviderEgressTallyBacklog
}

// Limits of one drain; see providerEgressTallyDrainCalls.
type providerEgressTallyDrainBudget struct {
	calls   int
	rows    int
	timeout time.Duration
}

// The production limits. Pure tests narrow a copy, for example to one call to
// reproduce the one-page owner.
func defaultProviderEgressTallyDrainBudget() providerEgressTallyDrainBudget {
	return providerEgressTallyDrainBudget{calls: providerEgressTallyDrainCalls, rows: providerEgressTallyDrainRows, timeout: providerEgressTallyDrainTimeout}
}

// Reads every retained page after the durable cursor that the budget allows.
// Every call deletes only records at or before the durable cursor, which the
// previous owner's finishing transaction committed, and its calls also clean
// that owner's whole drained prefix, so admission space returns at the rate
// pages are read. The first call keeps the single-page meaning, including
// initialization, repair and its errors. The drain's Backlog is published for
// its shard; an owner whose first call fails publishes nothing new, so its
// shard's observed time ages where it can be alerted on.
func DrainProviderEgressTallies(ctx context.Context, shard int, generation, cursor string, initialize bool, repairFrom ...string) (*ProviderEgressTallyDrain, error) {
	prior, err := providerEgressTallyOwnerPrior(shard, generation, cursor, initialize, repairFrom)
	if err != nil {
		return nil, err
	}
	providerEgressTallyBacklogs.arm()
	read := func(ctx context.Context, fromCursor string, pageLength int, first bool) (*providerEgressTallyCall, error) {
		if first {
			return readProviderEgressTallyCall(ctx, shard, generation, cursor, fromCursor, pageLength, initialize, prior)
		}
		return readProviderEgressTallyCall(ctx, shard, generation, cursor, fromCursor, pageLength, false, "")
	}
	drain, err := drainProviderEgressTallies(ctx, cursor, initialize, defaultProviderEgressTallyDrainBudget(), read, time.Now)
	if err != nil {
		return nil, err
	}
	providerEgressTallyBacklogs.observe(shard, drain.Backlog)
	return drain, nil
}

// The drain policy over any page reader, so it can be exercised without Redis.
// read must clean only at or before the durable cursor and read after
// fromCursor. A page is added whole or left for the next claim.
func drainProviderEgressTallies(
	ctx context.Context,
	cursor string,
	initialize bool,
	budget providerEgressTallyDrainBudget,
	read func(ctx context.Context, fromCursor string, pageLength int, first bool) (*providerEgressTallyCall, error),
	now func() time.Time,
) (*ProviderEgressTallyDrain, error) {
	startTime := now()
	first, err := read(ctx, cursor, providerEgressTallyPageLength, true)
	if err != nil {
		return nil, err
	}
	coalescer := newProviderEgressTallyCoalescer()
	if err := coalescer.add(first.records); err != nil {
		return nil, err
	}
	drain := &ProviderEgressTallyDrain{NextCursor: first.nextCursor, Calls: 1, Backlog: first.backlog}
	if 0 < len(first.records) {
		drain.Records = len(first.records)
		drain.Pages = 1
	}
	last := first
	reading := !initialize
	if initialize {
		drain.Stop = "initialize"
	}
	for !initialize {
		if reading && !last.more {
			reading = false
			drain.Stop = "caught_up"
		}
		// Cleanup-only calls continue after reading stops, within the budget.
		if !reading && !last.dirty {
			break
		}
		if budget.calls <= drain.Calls {
			if reading {
				drain.Stop = "calls"
			}
			break
		}
		if budget.timeout <= now().Sub(startTime) {
			if reading {
				drain.Stop = "time"
			}
			break
		}
		pageLength := 0
		if reading {
			pageLength = providerEgressTallyPageLength
		}
		call, err := read(ctx, drain.NextCursor, pageLength, false)
		drain.Calls += 1
		if err != nil {
			drain.Err = err
			if reading {
				drain.Stop = "error"
			}
			break
		}
		last = call
		if !reading || len(call.records) == 0 {
			continue
		}
		rows, err := coalescer.rowsWith(call.records)
		if err != nil {
			drain.Err = err
			drain.Stop = "error"
			reading = false
			continue
		}
		if budget.rows < rows {
			// The whole page stays retained for the next claim.
			drain.Stop = "rows"
			reading = false
			continue
		}
		server.Raise(coalescer.add(call.records))
		drain.NextCursor = call.nextCursor
		drain.Records += len(call.records)
		drain.Pages += 1
	}
	drain.Batch = coalescer.batch()
	return drain, nil
}

// A drain's occupancy is fresh for this long. Older depths are withheld,
// since other workers' claims have changed them since; the observed time is
// never withheld, so a shard no owner has read lately shows its age.
const providerEgressTallyBacklogFreshness = 5 * time.Minute

// Per-shard occupancy at the first call of this process's latest drain of each
// shard. The fleet view of a depth is the maximum across instances, and of the
// observed time the latest. Once this process drains any shard, every shard's
// observed time is published, zero for one not read here since start, so an
// owner stuck before its first read stays visible. Safe for concurrent use.
type providerEgressTallyBacklogCollector struct {
	now           func() time.Time
	stateLock     sync.Mutex
	armed         bool
	shardBacklogs [ProviderEgressTallyShardCount]ProviderEgressTallyBacklog
	records       *prometheus.Desc
	bytes         *prometheus.Desc
	fill          *prometheus.Desc
	pending       *prometheus.Desc
	observed      *prometheus.Desc
}

// Publishes nothing until this process drains a shard or observes one.
func newProviderEgressTallyBacklogCollector(now func() time.Time) *providerEgressTallyBacklogCollector {
	shard := []string{"shard"}
	return &providerEgressTallyBacklogCollector{
		now: now,
		records: prometheus.NewDesc("urnetwork_provider_egress_tally_queue_records",
			"Tally records a shard retained at the first read of its owner's latest drain in this process, after at most one page of cleanup, so within 256 records of the occupancy producers met; handoffs are refused as full at 16384; withheld when older than five minutes", shard, nil),
		bytes: prometheus.NewDesc("urnetwork_provider_egress_tally_queue_bytes",
			"Tally record bytes a shard retained at the first read of its owner's latest drain in this process, after at most one page of cleanup; handoffs are refused as full past 16 MiB; withheld when older than five minutes", shard, nil),
		fill: prometheus.NewDesc("urnetwork_provider_egress_tally_queue_fill_ratio",
			"Retained tally records or bytes at the first read of the shard owner's latest drain in this process, as a fraction of the shard's admission bound, whichever is fuller; at most one page below the peak, and handoffs are refused as full at 1; withheld when older than five minutes", shard, nil),
		pending: prometheus.NewDesc("urnetwork_provider_egress_tally_queue_oldest_pending_seconds",
			"Age of the shard's oldest unacknowledged tally record at the first read of its owner's latest drain in this process; zero when none is pending; withheld when older than five minutes", shard, nil),
		observed: prometheus.NewDesc("urnetwork_provider_egress_tally_queue_observed_timestamp_seconds",
			"Unix time of the first read of the shard owner's latest drain in this process whose first read succeeded; never withheld, so its age shows a stuck owner; zero when this process has drained other shards but not this one since it started", shard, nil),
	}
}

// Marks this process as a tally owner: every shard's observed time is
// published from now on, read here or not.
func (self *providerEgressTallyBacklogCollector) arm() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.armed = true
	}()
}

// Records a drain's occupancy unless an occupancy observed later is already
// held, and arms the collector.
func (self *providerEgressTallyBacklogCollector) observe(shard int, backlog ProviderEgressTallyBacklog) {
	if shard < 0 || shard >= ProviderEgressTallyShardCount || backlog.Records < 0 || backlog.Bytes < 0 {
		return
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.armed = true
		if backlog.ObservedAt.Before(self.shardBacklogs[shard].ObservedAt) {
			return
		}
		self.shardBacklogs[shard] = backlog
	}()
}

// The five shard-labelled families; see the constructor.
func (self *providerEgressTallyBacklogCollector) Describe(metrics chan<- *prometheus.Desc) {
	metrics <- self.records
	metrics <- self.bytes
	metrics <- self.fill
	metrics <- self.pending
	metrics <- self.observed
}

// Emits one consistent copy of the samples: the observed time for every
// shard once armed, and the depths of shards observed within the window.
func (self *providerEgressTallyBacklogCollector) Collect(metrics chan<- prometheus.Metric) {
	var armed bool
	var shardBacklogs [ProviderEgressTallyShardCount]ProviderEgressTallyBacklog
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		armed = self.armed
		shardBacklogs = self.shardBacklogs
	}()
	if !armed {
		return
	}
	now := self.now()
	for shard, backlog := range shardBacklogs {
		label := strconv.Itoa(shard)
		observedSeconds := 0.0
		if !backlog.ObservedAt.IsZero() {
			observedSeconds = float64(backlog.ObservedAt.UnixNano()) / float64(time.Second)
		}
		metrics <- prometheus.MustNewConstMetric(self.observed, prometheus.GaugeValue, observedSeconds, label)
		if backlog.ObservedAt.IsZero() || providerEgressTallyBacklogFreshness < now.Sub(backlog.ObservedAt) {
			continue
		}
		pendingSeconds := 0.0
		if !backlog.PendingSince.IsZero() {
			pendingSeconds = max(0, backlog.ObservedAt.Sub(backlog.PendingSince).Seconds())
		}
		metrics <- prometheus.MustNewConstMetric(self.records, prometheus.GaugeValue, float64(backlog.Records), label)
		metrics <- prometheus.MustNewConstMetric(self.bytes, prometheus.GaugeValue, float64(backlog.Bytes), label)
		metrics <- prometheus.MustNewConstMetric(self.fill, prometheus.GaugeValue, backlog.FillRatio(), label)
		metrics <- prometheus.MustNewConstMetric(self.pending, prometheus.GaugeValue, pendingSeconds, label)
	}
}

// Only explicit lost-continuity replies permit a durable repair intent. A
// timeout or generation mismatch cannot authorize discarding retained data.
func IsProviderEgressTallyContinuityLoss(err error) bool {
	if err == nil {
		return false
	}
	switch strings.TrimPrefix(err.Error(), "ERR ") {
	case "tally continuity missing", "tally record count mismatch", "tally tail missing", "tally records missing", "tally stream continuity mismatch":
		return true
	default:
		return false
	}
}

// Refresh may use unaffected evidence even when another shard is unavailable.
// UnknownShards is observational only: the old best-effort refresh contract did
// not promise complete samples. Known losses exclude precise day/place rows.
type ProviderEgressTallyProjection struct {
	Excluded      []ProviderEgressTallyExclusion
	UnknownShards []int
}

// Reads at most 1024 scoped losses per shard with one overall deadline. Markers
// age out with the configured SQL retention, not a global refresh veto. Missing
// metadata, stale owners, excess loss cardinality and Redis errors stay visible.
func ReadProviderEgressTallyProjection(ctx context.Context, now, retentionStart time.Time) *ProviderEgressTallyProjection {
	projection := &ProviderEgressTallyProjection{}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for shard := range ProviderEgressTallyShardCount {
		var values []string
		err := server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
			var redisErr error
			values, redisErr = r.Eval(ctx, `
redis.call('ZREMRANGEBYSCORE',KEYS[3],'-inf','('..ARGV[2])
local status = 'ready'
local last = tonumber(redis.call('HGET',KEYS[1],'observed_at') or '0')
local pending = tonumber(redis.call('HGET',KEYS[1],'pending_since') or '0')/1000
local unscoped = tonumber(redis.call('HGET',KEYS[1],'unscoped_loss_at') or '0')
if redis.call('HGET',KEYS[1],'ready') ~= '1' or last < tonumber(ARGV[1])-60 then status = 'unavailable'
elseif pending > 0 and pending < tonumber(ARGV[1])-60 then status = 'lagged'
elseif unscoped >= tonumber(ARGV[2]) then status = 'unscoped_loss' end
local result = {status}
for _,scope in ipairs(redis.call('ZRANGE',KEYS[3],0,tonumber(ARGV[3])-1)) do table.insert(result,scope) end
return result
`, providerEgressTallyKeys(shard), now.Unix(), retentionStart.Unix(), providerEgressTallyLossLength).StringSlice()
			return redisErr
		})
		unknown := err != nil || len(values) == 0 || values[0] != "ready"
		if err == nil && len(values) > 0 && len(values) <= providerEgressTallyLossLength+1 {
			for _, member := range values[1:] {
				var scope ProviderEgressTallyExclusion
				if json.Unmarshal([]byte(member), &scope) != nil {
					unknown = true
					continue
				}
				day, dayErr := time.Parse(time.DateOnly, scope.Day)
				place, placeErr := providerEgressTallyPlace(ProviderEgressPlace{CountryCode: scope.CountryCode, Region: scope.Region})
				if dayErr != nil || placeErr != nil || place.CountryCode != scope.CountryCode || place.Region != scope.Region || ProviderEgressTallyShard(place) != shard {
					unknown = true
					continue
				}
				if !day.Before(retentionStart.UTC().Truncate(24 * time.Hour)) {
					projection.Excluded = append(projection.Excluded, scope)
				}
			}
		}
		if unknown {
			projection.UnknownShards = append(projection.UnknownShards, shard)
		}
	}
	return projection
}

// Supplies a bounded, non-null JSON array to the tally query's anti-join.
func providerEgressTallyExclusionJson(excluded []ProviderEgressTallyExclusion) string {
	if len(excluded) > ProviderEgressTallyShardCount*providerEgressTallyLossLength {
		panic("tally exclusion bound exceeded")
	}
	if len(excluded) == 0 {
		return "[]"
	}
	encoded, err := json.Marshal(excluded)
	server.Raise(err)
	return string(encoded)
}
