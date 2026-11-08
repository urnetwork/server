package model

// The sockets store's scripts (§9.8, SOCK-M5-04). Each runs on one slot: a group's two keys share
// {sg_<network_id>_<h16>}, and the set and the listing are one key each, so no script names two tags
// (§22.4). Every key comes from KEYS and none is built inside.
//
// What a script decides is what sidecar's socketsservice decides in Go: the pick is PickSocketListeners,
// the estimate SocketEstimate, the outlier rules SocketEjectionState and SocketReportersEject, with the
// same integer arithmetic, so a script and the fake agree pick for pick on the same clock and draws
// (sidecartest.RunSocketStoreSuite holds both to it). Time is ARGV's, the store's injected clock, and a
// field ttl is only cleanup, never what a lease means.
//
// Records, all strings a script splits by hand, so that no number passes through cjson:
//
//	m:<L>  listen_id;protocol;version;weight;max_streams;flags;alpns;app_mode;name;token;sealed;claim
//	       ids, protocol, ALPN ids (comma separated), app mode, name and token in hex, sealed and claim
//	       in base64; flags 1 exclusive, 2 affinity, 4 serving, 8 draining
//	r:<L>  inflight;report_ms
//	i:<L>  dialer;exclusion_count;exclusion_start_ms;issue_ms,issue_ms,... per dialer, joined by |
//	e:<L>  ejection_count;ejection_start_ms;network:report_ms,network:report_ms,...
//	v      the group version, raised by each change of who listens
//
// Ids are lowercase hex, compared byte by byte and never through tonumber or the locale's collation,
// so a script orders listeners as connect.Id.Cmp does.

import (
	"github.com/redis/go-redis/v9"
)

// Helpers every script shares.
const socketStoreLuaPrelude = `
local function split(s, sep)
  local parts = {}
  local start = 1
  while true do
    local index = string.find(s, sep, start, true)
    if not index then
      parts[#parts + 1] = string.sub(s, start)
      return parts
    end
    parts[#parts + 1] = string.sub(s, start, index - 1)
    start = index + 1
  end
end

local function less(a, b)
  local n = math.min(#a, #b)
  for i = 1, n do
    local x = string.byte(a, i)
    local y = string.byte(b, i)
    if x ~= y then
      return x < y
    end
  end
  return #a < #b
end

local function int(n)
  return string.format("%d", n)
end

local function parse_m(s)
  local f = split(s, ";")
  local alpns = {}
  if f[7] ~= "" then
    alpns = split(f[7], ",")
  end
  local flags = tonumber(f[6])
  return {
    listen = f[1],
    protocol = f[2],
    version = tonumber(f[3]),
    weight = tonumber(f[4]),
    max = tonumber(f[5]),
    exclusive = bit.band(flags, 1) ~= 0,
    serving = bit.band(flags, 4) ~= 0,
    draining = bit.band(flags, 8) ~= 0,
    alpns = alpns
  }
end

local function parse_shares(s)
  local shares = {}
  if not s or s == "" then
    return shares
  end
  for _, entry in ipairs(split(s, "|")) do
    local f = split(entry, ";")
    local issues = {}
    if f[4] ~= "" then
      for _, t in ipairs(split(f[4], ",")) do
        issues[#issues + 1] = tonumber(t)
      end
    end
    shares[f[1]] = {count = tonumber(f[2]), start = tonumber(f[3]), issues = issues}
  end
  return shares
end

local function parse_ejection(s)
  local ejection = {count = 0, start = 0, reports = {}}
  if not s or s == "" then
    return ejection
  end
  local f = split(s, ";")
  ejection.count = tonumber(f[1])
  ejection.start = tonumber(f[2])
  if f[3] ~= "" then
    for _, report in ipairs(split(f[3], ",")) do
      local parts = split(report, ":")
      ejection.reports[#ejection.reports + 1] = {network = parts[1], ms = tonumber(parts[2])}
    end
  end
  return ejection
end

local function format_ejection(ejection)
  local reports = {}
  for _, report in ipairs(ejection.reports) do
    reports[#reports + 1] = report.network .. ":" .. int(report.ms)
  end
  return int(ejection.count) .. ";" .. int(ejection.start) .. ";" .. table.concat(reports, ",")
end

-- Drops what an expired lease left, and the lease.
local function trim(group, ix, now)
  local expired = redis.call("ZRANGEBYSCORE", ix, "-inf", now)
  local fields = {}
  for _, member in ipairs(expired) do
    fields[#fields + 1] = "m:" .. member
    fields[#fields + 1] = "r:" .. member
    fields[#fields + 1] = "i:" .. member
    fields[#fields + 1] = "e:" .. member
    if 400 <= #fields then
      redis.call("HDEL", group, unpack(fields))
      fields = {}
    end
  end
  if 0 < #fields then
    redis.call("HDEL", group, unpack(fields))
  end
  if 0 < #expired then
    redis.call("ZREMRANGEBYSCORE", ix, "-inf", now)
  end
end

local function max_lease(ix)
  local top = redis.call("ZRANGE", ix, -1, -1, "WITHSCORES")
  if #top == 2 then
    return tonumber(top[2])
  end
  return 0
end

local function remove_listener(group, ix, listener)
  redis.call("HDEL", group, "m:" .. listener, "r:" .. listener, "i:" .. listener, "e:" .. listener)
  redis.call("ZREM", ix, listener)
  redis.call("HINCRBY", group, "v", 1)
end

local function version_less(a, b)
  if #a ~= #b then
    return #a < #b
  end
  return less(a, b)
end
`

// One group's put or del, a page's step (§9.5).
//
// KEYS: group, ix. ARGV: op ("put" or "del"), now_ms, listener, lease_ms, m record, inflight,
// protocol, exclusive ("1" or "0").
//
// put returns {taken, first_listener, remaining, max_lease_ms}; del returns {found, last_listener,
// remaining, max_lease_ms}. A heartbeat of a live listener whose protocol and exclusive flag stand
// skips the taken check, since each later joiner was checked against it, so a heartbeat costs no scan.
const socketStoreGroupLua = socketStoreLuaPrelude + `
local group = KEYS[1]
local ix = KEYS[2]
local op = ARGV[1]
local now = tonumber(ARGV[2])
local listener = ARGV[3]
trim(group, ix, now)
local self_score = redis.call("ZSCORE", ix, listener)

if op == "del" then
  if not self_score then
    return {0, 0, redis.call("ZCARD", ix), int(max_lease(ix))}
  end
  remove_listener(group, ix, listener)
  local remaining = redis.call("ZCARD", ix)
  if remaining == 0 then
    redis.call("DEL", group, ix)
    return {1, 1, 0, "0"}
  end
  return {1, 0, remaining, int(max_lease(ix))}
end

local lease = tonumber(ARGV[4])
local record = ARGV[5]
local inflight = ARGV[6]
local protocol = ARGV[7]
local exclusive = ARGV[8] == "1"

local need_check = true
if self_score then
  local self_m = redis.call("HGET", group, "m:" .. listener)
  if self_m then
    local m = parse_m(self_m)
    if m.protocol == protocol and m.exclusive == exclusive then
      need_check = false
    end
  end
end
if need_check then
  for _, member in ipairs(redis.call("ZRANGE", ix, 0, -1)) do
    if member ~= listener then
      local other = redis.call("HGET", group, "m:" .. member)
      if other then
        local m = parse_m(other)
        if m.exclusive or exclusive or m.protocol ~= protocol then
          if self_score then
            remove_listener(group, ix, listener)
          end
          return {1, 0, redis.call("ZCARD", ix), int(max_lease(ix))}
        end
      end
    end
  end
end

local first = 0
if redis.call("ZCARD", ix) == 0 then
  first = 1
end
redis.call("HSET", group, "m:" .. listener, record, "r:" .. listener, inflight .. ";" .. int(now))
redis.call("HPEXPIRE", group, lease, "FIELDS", 2, "m:" .. listener, "r:" .. listener)
redis.call("ZADD", ix, now + lease, listener)
redis.call("PEXPIRE", ix, lease)
if not self_score then
  redis.call("HINCRBY", group, "v", 1)
end
redis.call("HPEXPIRE", group, lease, "FIELDS", 1, "v")
return {0, first, redis.call("ZCARD", ix), int(now + lease)}
`

// One Find (§9.6's step 4, §9.8): trim, apply the failed-ticket reports, filter or sample, pick, and add
// the primary to this dialer's share. One script on the group's two keys and no other.
//
// KEYS: group, ix. ARGV: now_ms, dialer, dialer_network, protocol, min_version, alpns, first_draw,
// second_draw, sample_count, ticket_lifetime_ms, clamp_floor, clamp_percent, ejection_step_ms,
// ejection_max_ms, ejection_group_percent, reporter_window_ms, reporter_network_count, share_ttl_ms,
// reports (listener:listen pairs, comma separated).
//
// Returns {outcome, exclusions, ejections} and then, for the primary and the backup that were picked,
// listener, lease_ms, m, r, i and e each, as they stood before the primary's share was added to.
// Outcome 1 is found, 2 no listener, 3 busy, as socketsservice.SocketPickOutcome numbers them.
const socketStorePickLua = socketStoreLuaPrelude + `
local group = KEYS[1]
local ix = KEYS[2]
local now = tonumber(ARGV[1])
local dialer = ARGV[2]
local dialer_network = ARGV[3]
local want_protocol = ARGV[4]
local min_version = tonumber(ARGV[5])
local want_alpns = {}
if ARGV[6] ~= "" then
  want_alpns = split(ARGV[6], ",")
end
local first_draw = tonumber(ARGV[7])
local second_draw = tonumber(ARGV[8])
local sample_count = tonumber(ARGV[9])
local ticket_lifetime = tonumber(ARGV[10])
local clamp_floor = tonumber(ARGV[11])
local clamp_percent = tonumber(ARGV[12])
local ejection_step = tonumber(ARGV[13])
local ejection_max = tonumber(ARGV[14])
local group_percent = tonumber(ARGV[15])
local reporter_window = tonumber(ARGV[16])
local reporter_networks = tonumber(ARGV[17])
local share_ttl = tonumber(ARGV[18])
local reports = {}
if ARGV[19] ~= "" then
  reports = split(ARGV[19], ",")
end

local function active(count, start)
  return now < start + math.min(count * ejection_step, ejection_max)
end

-- the state after one more ejection, or nil when none takes (SocketEjectionState.Eject)
local function eject(count, start, ejected_count, group_count)
  if active(count, start) then
    return nil
  end
  if group_count * group_percent < (ejected_count + 1) * 100 then
    return nil
  end
  local consecutive = 1
  if 0 < count and now < start + ejection_max then
    consecutive = count + 1
  end
  return {count = consecutive, start = now}
end

local function clamp_count(max)
  return math.max(clamp_floor, math.floor(max * clamp_percent / 100))
end

-- a share stops mattering once no issue of it can count and its exclusion can neither hold nor
-- continue a run
local function format_shares(shares, report)
  local dialers = {}
  for share_dialer in pairs(shares) do
    dialers[#dialers + 1] = share_dialer
  end
  table.sort(dialers, less)
  local entries = {}
  for _, share_dialer in ipairs(dialers) do
    local share = shares[share_dialer]
    local issues = {}
    for _, t in ipairs(share.issues) do
      if report <= t and now < t + ticket_lifetime then
        issues[#issues + 1] = int(t)
      end
    end
    if 0 < #issues or now < share.start + ejection_max then
      entries[#entries + 1] = share_dialer .. ";" .. int(share.count) .. ";" .. int(share.start) .. ";" .. table.concat(issues, ",")
    end
  end
  return table.concat(entries, "|")
end

trim(group, ix, now)
local group_count = redis.call("ZCARD", ix)
if group_count == 0 then
  return {2, 0, 0}
end

local function report_ms_of(r)
  if not r then
    return 0
  end
  return tonumber(split(r, ";")[2])
end

local exclusions = 0
local ejections = 0
for _, report in ipairs(reports) do
  local parts = split(report, ":")
  local listener = parts[1]
  local fields = redis.call("HMGET", group, "m:" .. listener, "r:" .. listener, "i:" .. listener, "e:" .. listener)
  if fields[1] and redis.call("ZSCORE", ix, listener) and parse_m(fields[1]).listen == parts[2] then
    local members = redis.call("ZRANGE", ix, 0, -1)
    local shares = parse_shares(fields[3])
    local share = shares[dialer] or {count = 0, start = 0, issues = {}}
    local excluded = 0
    for _, member in ipairs(members) do
      local other_share = parse_shares(redis.call("HGET", group, "i:" .. member))[dialer]
      if other_share and active(other_share.count, other_share.start) then
        excluded = excluded + 1
      end
    end
    local excluded_state = eject(share.count, share.start, excluded, group_count)
    if excluded_state then
      share.count = excluded_state.count
      share.start = excluded_state.start
      shares[dialer] = share
      exclusions = exclusions + 1
      redis.call("HSET", group, "i:" .. listener, format_shares(shares, report_ms_of(fields[2])))
      redis.call("HPEXPIRE", group, share_ttl, "FIELDS", 1, "i:" .. listener)
    end

    local ejection = parse_ejection(fields[4])
    local kept = {}
    for _, kept_report in ipairs(ejection.reports) do
      if kept_report.ms <= now and now - kept_report.ms <= reporter_window then
        kept[#kept + 1] = kept_report
      end
    end
    kept[#kept + 1] = {network = dialer_network, ms = now}
    ejection.reports = kept
    local networks = {}
    local network_count = 0
    for _, kept_report in ipairs(kept) do
      if kept_report.ms <= now and now - kept_report.ms <= reporter_window and not networks[kept_report.network] then
        networks[kept_report.network] = true
        network_count = network_count + 1
      end
    end
    if reporter_networks <= network_count then
      local ejected = 0
      for _, member in ipairs(members) do
        local other = parse_ejection(redis.call("HGET", group, "e:" .. member))
        if active(other.count, other.start) then
          ejected = ejected + 1
        end
      end
      local ejected_state = eject(ejection.count, ejection.start, ejected, group_count)
      if ejected_state then
        ejection.count = ejected_state.count
        ejection.start = ejected_state.start
        ejections = ejections + 1
      end
    end
    redis.call("HSET", group, "e:" .. listener, format_ejection(ejection))
    redis.call("HPEXPIRE", group, share_ttl, "FIELDS", 1, "e:" .. listener)
  end
end

local members
if sample_count < group_count then
  members = redis.call("ZRANDMEMBER", ix, sample_count, "WITHSCORES")
else
  members = redis.call("ZRANGE", ix, 0, -1, "WITHSCORES")
end

local candidates = {}
for index = 1, #members, 2 do
  local listener = members[index]
  local fields = redis.call("HMGET", group, "m:" .. listener, "r:" .. listener, "i:" .. listener, "e:" .. listener)
  if fields[1] then
    local candidate = parse_m(fields[1])
    candidate.id = listener
    candidate.lease = tonumber(members[index + 1])
    candidate.inflight = 0
    candidate.report = 0
    if fields[2] then
      local r = split(fields[2], ";")
      candidate.inflight = tonumber(r[1])
      candidate.report = tonumber(r[2])
    end
    candidate.shares = parse_shares(fields[3])
    local ejection = parse_ejection(fields[4])
    candidate.ejection_count = ejection.count
    candidate.ejection_start = ejection.start
    candidate.raw = {listener, members[index + 1], fields[1], fields[2] or "", fields[3] or "", fields[4] or ""}
    candidates[#candidates + 1] = candidate
  end
end

local function estimate(candidate)
  local clamp = clamp_count(candidate.max)
  local est = math.max(candidate.inflight, 0)
  for _, share in pairs(candidate.shares) do
    local issued = 0
    for _, t in ipairs(share.issues) do
      if candidate.report <= t and now < t + ticket_lifetime then
        issued = issued + 1
      end
    end
    est = est + math.min(issued, clamp)
  end
  return est
end

local function matches(candidate)
  if candidate.lease <= now or not candidate.serving or candidate.draining or candidate.weight <= 0 then
    return false
  end
  if want_protocol ~= "" and want_protocol ~= candidate.protocol then
    return false
  end
  if candidate.version < min_version then
    return false
  end
  if #want_alpns == 0 then
    return true
  end
  for _, want in ipairs(want_alpns) do
    for _, served in ipairs(candidate.alpns) do
      if want == served then
        return true
      end
    end
  end
  return false
end

local function shunned(candidate)
  if active(candidate.ejection_count, candidate.ejection_start) then
    return true
  end
  local share = candidate.shares[dialer]
  return share ~= nil and active(share.count, share.start)
end

local matched = false
local roomy = {}
local pickable = {}
for _, candidate in ipairs(candidates) do
  if matches(candidate) then
    matched = true
    if estimate(candidate) < candidate.max then
      roomy[#roomy + 1] = candidate
      if not shunned(candidate) then
        pickable[#pickable + 1] = candidate
      end
    end
  end
end
if not matched then
  return {2, exclusions, ejections}
end
if #pickable == 0 then
  pickable = roomy
end
if #pickable == 0 then
  return {3, exclusions, ejections}
end
table.sort(pickable, function(a, b) return less(a.id, b.id) end)

local function draw_index(draw, skip)
  local total = 0
  for index, candidate in ipairs(pickable) do
    if index ~= skip then
      total = total + candidate.weight
    end
  end
  local target = draw % total
  local chosen = -1
  for index, candidate in ipairs(pickable) do
    if index ~= skip then
      chosen = index
      if target < candidate.weight then
        break
      end
      target = target - candidate.weight
    end
  end
  return chosen
end

local first_index = draw_index(first_draw, 0)
local primary = pickable[first_index]
local backup = nil
if 1 < #pickable then
  backup = pickable[draw_index(second_draw, first_index)]
  local first_load = estimate(primary) + 1
  local second_load = estimate(backup) + 1
  if second_load * primary.weight < first_load * backup.weight then
    primary, backup = backup, primary
  end
end

local reply = {1, exclusions, ejections}
for _, value in ipairs(primary.raw) do
  reply[#reply + 1] = value
end
if backup then
  for _, value in ipairs(backup.raw) do
    reply[#reply + 1] = value
  end
end

local share = primary.shares[dialer] or {count = 0, start = 0, issues = {}}
local issues = {}
for _, t in ipairs(share.issues) do
  if primary.report <= t and now < t + ticket_lifetime then
    issues[#issues + 1] = t
  end
end
issues[#issues + 1] = now
local clamp = clamp_count(primary.max)
while clamp < #issues do
  table.remove(issues, 1)
end
share.issues = issues
primary.shares[dialer] = share
redis.call("HSET", group, "i:" .. primary.id, format_shares(primary.shares, primary.report))
redis.call("HPEXPIRE", group, share_ttl, "FIELDS", 1, "i:" .. primary.id)
return reply
`

// A group's live listeners, for ListListeners. KEYS: group, ix. ARGV: now_ms. Returns listener,
// lease_ms, m, r, i and e for each, in lease order.
const socketStoreReadLua = socketStoreLuaPrelude + `
local group = KEYS[1]
local ix = KEYS[2]
local members = redis.call("ZRANGEBYSCORE", ix, "(" .. ARGV[1], "+inf", "WITHSCORES")
local reply = {}
for index = 1, #members, 2 do
  local listener = members[index]
  local fields = redis.call("HMGET", group, "m:" .. listener, "r:" .. listener, "i:" .. listener, "e:" .. listener)
  if fields[1] then
    reply[#reply + 1] = listener
    reply[#reply + 1] = members[index + 1]
    reply[#reply + 1] = fields[1]
    reply[#reply + 1] = fields[2] or ""
    reply[#reply + 1] = fields[3] or ""
    reply[#reply + 1] = fields[4] or ""
  end
end
return reply
`

// A page's first step on the client's set: refuses an older set_version, and reads what the set holds
// in the page's range. Writes nothing.
//
// KEYS: {sc_<client_id>}set. ARGV: set_version, key_start, key_end ("" unbounded). Returns {"stale",
// stored} or {"ok", stored, name keys in range...}, stored "" when the set has none.
const socketStoreSetBeginLua = socketStoreLuaPrelude + `
local key = KEYS[1]
local meta = redis.call("ZRANGEBYLEX", key, "-", "(\1", "LIMIT", 0, 1)
local stored = ""
if #meta == 1 then
  stored = string.sub(meta[1], 18)
end
if stored ~= "" and version_less(ARGV[1], stored) then
  return {"stale", stored}
end
local max = "+"
if ARGV[3] ~= "" then
  max = "(\1" .. ARGV[3]
end
local reply = {"ok", stored}
for _, member in ipairs(redis.call("ZRANGEBYLEX", key, "[\1" .. ARGV[2], max)) do
  reply[#reply + 1] = string.sub(member, 2)
end
return reply
`

// A page's last step on the client's set: its range becomes the page's held keys, and its version and
// network the page's. Its members are 0x01 || name key, and one 0x00 || network_id || set_version,
// which sorts first and never inside a page's range.
//
// KEYS: {sc_<client_id>}set. ARGV: network_id (16 bytes), set_version, key_start, key_end, ttl_ms, then
// the held name keys. Returns {"stale", stored} or {"ok", set_version}.
const socketStoreSetCommitLua = socketStoreLuaPrelude + `
local key = KEYS[1]
local meta = redis.call("ZRANGEBYLEX", key, "-", "(\1", "LIMIT", 0, 1)
if #meta == 1 then
  local stored = string.sub(meta[1], 18)
  if version_less(ARGV[2], stored) then
    return {"stale", stored}
  end
end
local max = "+"
if ARGV[4] ~= "" then
  max = "(\1" .. ARGV[4]
end
redis.call("ZREMRANGEBYLEX", key, "[\1" .. ARGV[3], max)
redis.call("ZREMRANGEBYLEX", key, "-", "(\1")
local members = {}
for index = 6, #ARGV do
  members[#members + 1] = 0
  members[#members + 1] = "\1" .. ARGV[index]
  if 400 <= #members then
    redis.call("ZADD", key, unpack(members))
    members = {}
  end
end
if 0 < #members then
  redis.call("ZADD", key, unpack(members))
end
redis.call("ZADD", key, 0, "\0" .. ARGV[1] .. ARGV[2])
redis.call("PEXPIRE", key, ARGV[5])
return {"ok", ARGV[2]}
`

// The network's listing after a page or a removal: each touched name's field holds kind;lease_ms;sealed,
// living until the group's newest lease, or goes once the group is empty.
//
// KEYS: {sn_<network_id>}names. ARGV: now_ms, then per name: name key, max_lease_ms ("0" removes),
// kind ("e" edge, "b" blinded, "" keeps the field's own), sealed (with kind "b").
const socketStoreNamesLua = socketStoreLuaPrelude + `
local key = KEYS[1]
local now = tonumber(ARGV[1])
for index = 2, #ARGV, 4 do
  local field = ARGV[index]
  local lease = tonumber(ARGV[index + 1])
  local kind = ARGV[index + 2]
  local sealed = ARGV[index + 3]
  if lease <= now then
    redis.call("HDEL", key, field)
  else
    if kind == "" then
      local old = redis.call("HGET", key, field)
      if old then
        local kind_end = string.find(old, ";", 1, true)
        local lease_end = string.find(old, ";", kind_end + 1, true)
        kind = string.sub(old, 1, kind_end - 1)
        sealed = string.sub(old, lease_end + 1)
      end
    end
    if kind ~= "" then
      redis.call("HSET", key, field, kind .. ";" .. int(lease) .. ";" .. sealed)
      redis.call("HPEXPIRE", key, lease - now, "FIELDS", 1, field)
    end
  end
end
return 1
`

// §6.10.1's pair: one field and its ttl in one step. KEYS: {svc_<listener_client_id>}. ARGV: dialer,
// grant_expire_ms, ttl_ms.
const socketStorePairLua = `
redis.call("HSET", KEYS[1], ARGV[1], ARGV[2])
redis.call("HPEXPIRE", KEYS[1], ARGV[3], "FIELDS", 1, ARGV[1])
return 1
`

// Every script's source, by name, which the slot check reads.
var socketStoreScriptSources = map[string]string{
	"group":      socketStoreGroupLua,
	"pick":       socketStorePickLua,
	"read":       socketStoreReadLua,
	"set_begin":  socketStoreSetBeginLua,
	"set_commit": socketStoreSetCommitLua,
	"names":      socketStoreNamesLua,
	"pair":       socketStorePairLua,
}

// Every script, by name, made once.
var socketStoreScripts = func() map[string]*redis.Script {
	scripts := map[string]*redis.Script{}
	for name, source := range socketStoreScriptSources {
		scripts[name] = redis.NewScript(source)
	}
	return scripts
}()
