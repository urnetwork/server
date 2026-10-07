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

var providerEgressTallyHandoffs = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_provider_egress_tally_handoffs_total",
	Help: "Auxiliary tally handoffs by finite accepted, unavailable, invalid or full outcome; never measured quota credits.",
}, []string{"outcome"})

func init() {
	prometheus.MustRegister(providerEgressTallyHandoffs)
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
// deletes at most one committed page before returning the next immutable page.
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
if tail ~= '0-0' then
 if redis.call('EXISTS', KEYS[2]) == 0 then redis.call('HSET',KEYS[1],'ready',0); return redis.error_reply('tally records missing') end
 local info = redis.call('XINFO', 'STREAM', KEYS[2])
 local actual = nil
 for i=1,#info,2 do if info[i] == 'last-generated-id' then actual=info[i+1] end end
 if actual ~= tail then redis.call('HSET',KEYS[1],'ready',0); return redis.error_reply('tally stream continuity mismatch') end
end
if ARGV[6] == '0' then redis.call('HSET', KEYS[1], 'ready', 1) end
local old = redis.call('XRANGE', KEYS[2], '-', ARGV[2], 'COUNT', ARGV[3])
for _, entry in ipairs(old) do
 redis.call('HINCRBY', KEYS[1], 'bytes', -#entry[2][2])
 redis.call('HINCRBY', KEYS[1], 'count', -1)
 redis.call('XDEL', KEYS[2], entry[1])
end
local result = {}
local bytes = 0
local cursor = ARGV[2]
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
redis.call('HSET', KEYS[1], 'pending_since', #first == 0 and '0' or string.match(first[1][1], '^(%d+)'))
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

// Reads one finite prefix; it never acknowledges a speculative cursor. The
// caller obtains cursor/generation from its durable task arguments only.
func ReadProviderEgressTallyPage(ctx context.Context, shard int, generation, cursor string, initialize bool, repairFrom ...string) (*ProviderEgressTallyPage, error) {
	if shard < 0 || shard >= ProviderEgressTallyShardCount || generation == "" {
		return nil, fmt.Errorf("invalid tally owner")
	}
	if _, err := CompareProviderEgressTallyCursor(cursor, "0-0"); err != nil {
		return nil, err
	}
	prior := ""
	if len(repairFrom) > 1 {
		return nil, fmt.Errorf("invalid tally repair")
	}
	if len(repairFrom) == 1 {
		prior = repairFrom[0]
	}
	if prior != "" && (!initialize || cursor != "0-0" || prior == generation) {
		return nil, fmt.Errorf("invalid tally repair")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var values []string
	err := server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		var redisErr error
		values, redisErr = r.Eval(ctx, providerEgressTallyReadScript, providerEgressTallyKeys(shard), generation, cursor,
			providerEgressTallyPageLength, providerEgressTallyPageBytes, time.Now().Unix(), map[bool]string{false: "0", true: "1"}[initialize], prior).StringSlice()
		return redisErr
	})
	if err != nil {
		return nil, err
	}
	if len(values)%2 != 0 || len(values) > providerEgressTallyPageLength*2 {
		return nil, fmt.Errorf("invalid tally page")
	}
	page := &ProviderEgressTallyPage{Generation: generation, Cursor: cursor, NextCursor: cursor}
	for i := 0; i < len(values); i += 2 {
		order, err := CompareProviderEgressTallyCursor(values[i], page.NextCursor)
		if err != nil || order <= 0 {
			return nil, fmt.Errorf("nonmonotone tally page")
		}
		var record ProviderEgressTallyRecord
		if len(values[i+1]) > providerEgressTallyRecordBytes || json.Unmarshal([]byte(values[i+1]), &record) != nil ||
			record.MeasuredAt.IsZero() || len(record.Loads) > 512 || ProviderEgressTallyShard(record.Run.Place) != shard {
			return nil, fmt.Errorf("invalid retained tally record")
		}
		page.Records = append(page.Records, record)
		page.NextCursor = values[i]
	}
	return page, nil
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
