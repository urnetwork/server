// The provider intent exempt index: an exact count of a network's exempt intent
// clients without walking its registries.
//
// count_clients (provider_intent_model.go) counts every live member of the
// connected and provider registries, except a skipped member, as a normal or
// an exempt client. An exempt client has a pending or qualified record, a live
// record expiry and a live intent presence. The walk costs three commands per
// member per count; on 2026-11-01 a network of about 950 members in a
// reconnect wave kept it running hundreds of times a second and saturated the
// network's redis node.
//
// The index materializes that predicate per registry entry. It holds `c`+member
// for an exempt member of the connected registry and `p`+member for an exempt
// member of the provider registry, scored by the first of the registry entry's
// expiry, the member's presence expiry and its record expiry. A registry entry
// counts as exempt at `live_min` exactly when its index entry exists and scores
// at least `live_min`, so a count is four ZCOUNTs and a few ZSCOREs. The index
// holds no time logic of its own: it changes only with the state it mirrors,
// so it is exact for any `live_min`, whatever the caller's clock. A member in
// both registries has an entry for each, as the walk counts it once per
// registry, and an entry never outlives its registry entry, so a member that
// left a registry subtracts nothing.
//
// Every script that writes a member's registry entry, presence, record expiry
// or record re-derives the member's entries from the current state in the
// same script (sync_exempt_index). Every script that removes one of them
// removes the entries. All keys share the network's peer hash tag, so each
// script stays in one slot.
//
// A network's index is built lazily. A count that finds it incomplete first
// advances the build, which sweeps the connected registry and then the
// provider registry in score order, deriving each member's entry. A sweep runs
// from its start to the registry's highest score when the sweep starts: a
// member above it, or one whose score moves, was written since and its writer
// derived it. Past the start the cursor is a score, inclusive, so a run of
// equal scores split by a step is resumed whole, and a run longer than a step
// (-inf scores included) is taken in one. An unreadable build state restarts
// the build. Score
// order is the same on every replica, so a build survives a failover, which a
// scan cursor would not. The count uses the index when the build completes and
// the walk otherwise, so a count is exact while the index builds. A new build
// starts by dropping the index, so it never trusts an entry it did not derive.
// The build state is a sentinel member scored -inf (never counted), so a lost
// index takes its state with it and the next count rebuilds it. A complete
// index holds `#ready` scored by its build time negated (never counted either),
// and a count rebuilds an index older than providerIntentExemptRebuildAge.
//
// The cost per call, each command O(log N) in the network's registry size N:
//   - a count: TIME, the sentinel ZSCORE and a TTL read (and an EXPIRE once per
//     half ttl), four ZCOUNTs, an EXISTS while the index holds a live exempt
//     entry, and up to four ZSCOREs for the skipped member, so at most thirteen
//     commands, where the walk took two or three per member;
//   - a write: the entry's re-derivation, at most four reads, then a ZADD and a
//     TTL read (and an EXPIRE once per half ttl) or a ZREM; a removal, one
//     ZREM;
//   - a count while the index builds: the walk, plus at most two sweep steps
//     deriving max(1024, N/4) members and two more, and any run of equal
//     scores, at most seven commands each, for at most a handful of counts per
//     build.
//
// The index is exact once every process that writes these keys runs this code:
// a writer without it changes the mirrored state without the index. Such an
// entry heals at the member's next write by this code (its registry heartbeat,
// presence or check) and an entry left without a registry entry stops counting
// at that entry's former expiry, so the counts converge within a registry ttl
// after the last such process exits. Until then a count is off in either
// direction, and a normal count can even go negative. The same holds when a
// registry or intent key is lost (an eviction, a DEL) while the index survives.
// A count therefore checks the index against what it can afford to: more live
// exempt entries than live registry entries, or a live exempt entry while one
// of the intent keys every exempt entry needs is missing, means the index lost
// track of its sources. The count drops the index, walks, and the next count
// rebuilds it. The checks do not see an index that misses an entry or holds a
// stale status, or one that lost a registry while the other registry has more
// live members than the index has entries; the rebuild age bounds those.
package model

import (
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
)

var providerIntentCountsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_provider_intent_counts_total",
	Help: "Provider intent client counts by how they were made: from the network's exempt index, by walking its registries while the index builds, or by walking after dropping an index that disagreed with its sources",
}, []string{"path"})

// the `path` label values of urnetwork_provider_intent_counts_total
const (
	providerIntentCountPathIndex = "index"
	providerIntentCountPathWalk  = "walk"
	providerIntentCountPathLost  = "lost"
)

// Every label value is exported from the start, so a rate is defined before
// the first count.
func init() {
	for _, path := range []string{providerIntentCountPathIndex, providerIntentCountPathWalk, providerIntentCountPathLost} {
		providerIntentCountsCounter.WithLabelValues(path)
	}
	prometheus.MustRegister(providerIntentCountsCounter)
}

// Counts one count by the path its script reports (count_clients): 1 the
// index, 2 the walk, 3 the walk after dropping a lost index, and anything else
// no count.
func countProviderIntentCountPath(countPath int64) {
	switch countPath {
	case 1:
		providerIntentCountsCounter.WithLabelValues(providerIntentCountPathIndex).Inc()
	case 2:
		providerIntentCountsCounter.WithLabelValues(providerIntentCountPathWalk).Inc()
	case 3:
		providerIntentCountsCounter.WithLabelValues(providerIntentCountPathLost).Inc()
	}
}

// zset: `c`+client id bytes and `p`+client id bytes of exempt registry entries,
// scored by their first expiry, plus one build state sentinel: `#ready` scored
// by its build time in unix milli negated, or a build in progress scored -inf
func providerIntentExemptKey(networkId server.Id) string {
	return fmt.Sprintf("{np_%s}pi_exempt", networkId)
}

// How long a count trusts a complete index before rebuilding it from its
// sources. The count's own checks see an index that counts entries without
// sources, not one that misses entries or holds a stale status, as one left by
// a writer without the index (a rollout or a rollback) can. The age bounds such
// a disagreement to an hour and a build. A rebuild costs each counted network
// one sweep an hour, a walk only for a network above the build limit. The age
// is measured on the redis clock (TIME), so the callers' clocks do not move it.
const providerIntentExemptRebuildAge = time.Hour

// The fewest registry members one count derives while the index builds. A
// count builds at least a quarter of the network's registry members, so a build
// takes at most a handful of counts that walk, and a network of up to this many
// members is built by its first count without a walk.
const providerIntentExemptBuildLimit = 1024

// Re-derives one registry entry's exempt index entry. The status match is the
// walk's (count_clients_walk). `key_ttl_seconds` is a backstop for the index
// key: entries carry their own expiry in their score, so it is re-armed only
// once half of it has passed.
const providerIntentExemptSyncLua = `
local function exempt_index_score(registry_key, pi_key, present_key, expire_key, member)
	local present = redis.call('ZSCORE', present_key, member)
	if present == false then
		return false
	end
	local expire = redis.call('ZSCORE', expire_key, member)
	if expire == false then
		return false
	end
	local registered = redis.call('ZSCORE', registry_key, member)
	if registered == false then
		return false
	end
	local record = redis.call('HGET', pi_key, member)
	if record == false then
		return false
	end
	local status = string.match(record, '^(%S+)')
	if status ~= 'pending' and status ~= 'qualified' then
		return false
	end
	-- the original score strings, so the entry keeps their exact values
	local score = registered
	if tonumber(present) < tonumber(score) then
		score = present
	end
	if tonumber(expire) < tonumber(score) then
		score = expire
	end
	return score
end

local function sync_exempt_index(index_key, registry_tag, registry_key, pi_key, present_key, expire_key, member, key_ttl_seconds)
	local score = exempt_index_score(registry_key, pi_key, present_key, expire_key, member)
	if score == false then
		redis.call('ZREM', index_key, registry_tag .. member)
		return
	end
	redis.call('ZADD', index_key, score, registry_tag .. member)
	if redis.call('TTL', index_key) < tonumber(key_ttl_seconds) / 2 then
		redis.call('EXPIRE', index_key, key_ttl_seconds)
	end
end
`

// The count library of the provider intent scripts. count_clients_walk is the
// walk over every live registry member, used while a network's index builds.
// build_exempt_index advances the build and returns whether the index is
// complete; its state is `#c <cursor> <sweep end>` or `#p ...` while it
// sweeps a registry, and `#ready` scored by the negated build time. The cursor
// is `start` or a score, the sweep end `none` (an empty registry) or a score. sweep_exempt_index takes one step of a
// sweep and returns the members derived and the next cursor, or false at the
// sweep's end. count_clients_indexed counts from a complete index, or returns
// false when the index disagrees with its sources. The counts return
// {normal_count, exempt_count} for `live_min` (the decimal unix milli string of
// the first live expiry) without `skip_member`.
var providerIntentExemptCountLua = fmt.Sprintf(`%s
local function count_clients_walk(pi_key, present_key, expire_key, connected_key, proxy_key, provider_key, live_min, skip_member)
	local live_min_ms = tonumber(live_min)
	local normal_count = redis.call('ZCOUNT', proxy_key, live_min, '+inf')
	local exempt_count = 0
	for _, registry_key in ipairs({connected_key, provider_key}) do
		local members = redis.call('ZRANGEBYSCORE', registry_key, live_min, '+inf')
		for _, member in ipairs(members) do
			if member ~= skip_member then
				local exempt = false
				local present = redis.call('ZSCORE', present_key, member)
				local expire = redis.call('ZSCORE', expire_key, member)
				if present and expire and live_min_ms <= tonumber(present) and live_min_ms <= tonumber(expire) then
					local record = redis.call('HGET', pi_key, member)
					if record then
						local status = string.match(record, '^(%%S+)')
						exempt = status == 'pending' or status == 'qualified'
					end
				end
				if exempt then
					exempt_count = exempt_count + 1
				else
					normal_count = normal_count + 1
				end
			end
		end
	end
	return normal_count, exempt_count
end

local function exempt_sweep_end(registry_key)
	local last = redis.call('ZRANGE', registry_key, -1, -1, 'WITHSCORES')
	if #last == 0 then
		return 'none'
	end
	return last[2]
end

local function exempt_sweep_token(token, word)
	if token == word then
		return true
	end
	local score = tonumber(token)
	return score ~= nil and score == score
end

local function sweep_exempt_index(index_key, registry_tag, registry_key, pi_key, present_key, expire_key, cursor, sweep_end, budget, key_ttl_seconds)
	if sweep_end == 'none' then
		return 0, false
	end
	local min = cursor
	if cursor == 'start' then
		min = '-inf'
	end
	-- one past the budget tells whether the sweep ends within it
	local entries = redis.call('ZRANGEBYSCORE', registry_key, min, sweep_end, 'WITHSCORES', 'LIMIT', 0, budget + 1)
	for i = 1, #entries, 2 do
		sync_exempt_index(index_key, registry_tag, registry_key, pi_key, present_key, expire_key, entries[i], key_ttl_seconds)
	end
	local derived = #entries / 2
	if derived <= budget then
		return derived, false
	end
	local last = entries[#entries]
	if cursor == 'start' or tonumber(cursor) < tonumber(last) then
		-- the next step resumes at the last score, so a run split here is whole
		return derived, last
	end
	-- a run of equal scores longer than a step: derive it whole and move past it
	local run = redis.call('ZRANGEBYSCORE', registry_key, cursor, cursor)
	for _, member in ipairs(run) do
		sync_exempt_index(index_key, registry_tag, registry_key, pi_key, present_key, expire_key, member, key_ttl_seconds)
	end
	local next_entries = redis.call('ZRANGEBYSCORE', registry_key, '(' .. cursor, sweep_end, 'WITHSCORES', 'LIMIT', 0, 1)
	if #next_entries == 0 then
		return derived + #run, false
	end
	return derived + #run, next_entries[2]
end

local function build_exempt_index(index_key, pi_key, present_key, expire_key, connected_key, provider_key, build_limit, key_ttl_seconds)
	local time = redis.call('TIME')
	local now_ms = tonumber(time[1]) * 1000 + math.floor(tonumber(time[2]) / 1000)
	local built = redis.call('ZSCORE', index_key, '#ready')
	if built ~= false then
		if math.abs(now_ms + tonumber(built)) <= %d then
			-- an index without exempt entries is written by no sync
			if redis.call('TTL', index_key) < tonumber(key_ttl_seconds) / 2 then
				redis.call('EXPIRE', index_key, key_ttl_seconds)
			end
			return true
		end
		-- past its age, the index is rebuilt from its sources
	end
	local registry_tag, cursor, sweep_end = nil, nil, nil
	local states = redis.call('ZRANGEBYSCORE', index_key, '-inf', '-inf', 'LIMIT', 0, 1)
	if built == false and #states == 1 then
		registry_tag, cursor, sweep_end = string.match(states[1], '^#([cp]) ([^ ]+) ([^ ]+)$')
		if registry_tag ~= nil and not (exempt_sweep_token(cursor, 'start') and exempt_sweep_token(sweep_end, 'none')) then
			-- an unreadable state restarts the build
			registry_tag = nil
		end
	end
	if registry_tag == nil then
		-- a new build derives every entry itself
		redis.call('UNLINK', index_key)
		registry_tag = 'c'
		cursor = 'start'
		sweep_end = exempt_sweep_end(connected_key)
	else
		redis.call('ZREM', index_key, states[1])
	end
	local budget = math.max(
		tonumber(build_limit),
		math.ceil((redis.call('ZCARD', connected_key) + redis.call('ZCARD', provider_key)) / 4)
	)
	-- at most one step per registry; the provider step runs on what the
	-- connected step left, so an empty or short provider registry completes
	-- the build in the same count
	while true do
		local registry_key = connected_key
		if registry_tag == 'p' then
			registry_key = provider_key
		end
		local derived, next_cursor = sweep_exempt_index(index_key, registry_tag, registry_key, pi_key, present_key, expire_key, cursor, sweep_end, math.max(budget, 0), key_ttl_seconds)
		budget = budget - derived
		if next_cursor then
			cursor = next_cursor
			break
		end
		if registry_tag == 'p' then
			redis.call('ZADD', index_key, -now_ms, '#ready')
			redis.call('EXPIRE', index_key, key_ttl_seconds)
			return true
		end
		registry_tag = 'p'
		cursor = 'start'
		sweep_end = exempt_sweep_end(provider_key)
	end
	redis.call('ZADD', index_key, '-inf', '#' .. registry_tag .. ' ' .. cursor .. ' ' .. sweep_end)
	redis.call('EXPIRE', index_key, key_ttl_seconds)
	return false
end

local function count_clients_indexed(index_key, pi_key, present_key, expire_key, connected_key, proxy_key, provider_key, live_min, skip_member)
	local live_min_ms = tonumber(live_min)
	local live_count = redis.call('ZCOUNT', connected_key, live_min, '+inf') + redis.call('ZCOUNT', provider_key, live_min, '+inf')
	local exempt_count = redis.call('ZCOUNT', index_key, live_min, '+inf')
	if live_count < exempt_count then
		return false
	end
	if 0 < exempt_count and redis.call('EXISTS', pi_key, present_key, expire_key) < 3 then
		return false
	end
	for _, registry in ipairs({{'c', connected_key}, {'p', provider_key}}) do
		local registered = redis.call('ZSCORE', registry[2], skip_member)
		if registered and live_min_ms <= tonumber(registered) then
			live_count = live_count - 1
			local exempt = redis.call('ZSCORE', index_key, registry[1] .. skip_member)
			if exempt and live_min_ms <= tonumber(exempt) then
				exempt_count = exempt_count - 1
			end
		end
	end
	return redis.call('ZCOUNT', proxy_key, live_min, '+inf') + live_count - exempt_count, exempt_count
end
`, providerIntentExemptSyncLua, providerIntentExemptRebuildAge.Milliseconds())
