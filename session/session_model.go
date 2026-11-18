// Network session inventory is Redis denylist authority; SQL owns recoverable work.
package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

// The most retained revocation markers of a network: revoked sessions whose
// horizon has not ended (held minus live). The live cap is pro.yml's
// max_sessions (session_cap.go).
const NetworkSessionRetainedLimit = 10000

// The most rows one GET /network/sessions page returns, and its default.
const NetworkSessionsPageLimit = 1000

// Closed machine codes are safe to expose; storage details never cross the API.
type SessionError struct {
	Code   string
	Status int
}

// The message a refusal answers with: the cap refusals say what to do (owner
// decision 7), every other code the generic message.
var sessionErrorMessages = map[string]string{
	"session_limit_reached":           "Too many active sessions. Sign out of sessions you no longer use in Account → Sessions, then try again.",
	"session_retention_limit_reached": "Too many recently signed-out sessions. Try again later.",
}

func (self *SessionError) Error() string { return fmt.Sprintf("%d %s", self.Status, self.Code) }
func (self *SessionError) HttpErrorResultBody() any {
	message, ok := sessionErrorMessages[self.Code]
	if !ok {
		message = "Session request could not be completed."
	}
	return map[string]any{"error": message, "code": self.Code}
}
func (self *SessionError) RetryAfterSeconds() int {
	if self.Status == 429 {
		return 86400
	}
	if self.Status == 503 {
		return 1
	}
	return 0
}

func sessionError(code string) error {
	status := 409
	switch code {
	case "session_not_found", "operation_not_found":
		status = 404
	case "session_revoked":
		status = 401
	case "session_quota_exceeded":
		status = 429
	case "network_credential_required":
		status = 403
	case "invalid_request":
		status = 400
	}
	return &SessionError{Code: code, Status: status}
}

type NetworkSession struct {
	SessionId server.Id `json:"session_id"`
	Current   bool      `json:"current"`
	// how the session was created (the mint kind: password, sso, apple,
	// google, wallet, verify, seedphrase, signup, lag, auth_code,
	// api_key_client, hosted_proxy, legacy_proxy, device_adopt, legacy,
	// restored, unknown); the apps label the sign-in method with it
	Kind string `json:"kind"`
	// signin, code or client (SessionKind*)
	SessionKind string `json:"session_kind"`
	// for a client session what owns it: hosted_proxy, api_key, device or
	// legacy (ClientKind*); null for a sign-in or code session
	ClientKind *string `json:"client_kind"`
	// a client session whose client has provide enabled: the session cap
	// does not count it
	Provider        bool             `json:"provider"`
	CreateTime      time.Time        `json:"create_time"`
	LastMintTime    time.Time        `json:"last_mint_time"`
	TokenExpireTime time.Time        `json:"token_expire_time"`
	AcceptUntil     time.Time        `json:"accept_until"`
	OriginSessionId *server.Id       `json:"origin_session_id"`
	LastUsed        *SessionLastUsed `json:"last_used"`
}

// The network's live sessions by kind, whatever page was read.
type NetworkSessionCounts struct {
	// every live session
	Live int `json:"live"`
	// the live sessions the session cap counts: all but the provider ones
	Capped int `json:"capped"`
	// client sessions whose client has provide enabled
	Provider    int `json:"provider"`
	Signin      int `json:"signin"`
	Code        int `json:"code"`
	HostedProxy int `json:"hosted_proxy"`
	ApiKey      int `json:"api_key"`
	Device      int `json:"device"`
	Legacy      int `json:"legacy"`
	// the session cap (pro.yml max_sessions), 0 for none, and whether a mint
	// over it is refused
	Limit         int  `json:"limit"`
	LimitEnforced bool `json:"limit_enforced"`
}

type NetworkSessionsResult struct {
	Sessions         []*NetworkSession     `json:"sessions"`
	Generation       string                `json:"generation"`
	EventId          int64                 `json:"event_id"`
	CurrentSessionId *server.Id            `json:"current_session_id"`
	LegacyCoverage   string                `json:"legacy_coverage"`
	Counts           *NetworkSessionCounts `json:"counts"`
	// the next page's cursor, null after the last page
	NextCursor *string `json:"next_cursor"`
}

// GET /network/sessions query parameters. Every field is optional.
type NetworkSessionsArgs struct {
	// next_cursor of the previous page, with the same filters
	Cursor string
	// rows per page, at most NetworkSessionsPageLimit
	Limit int
	// signin, code or client: only sessions of that kind
	SessionKind string
	// hosted_proxy, api_key, device or legacy: only client sessions of that
	// kind
	ClientKind string
}
type SessionOperationResult struct {
	OperationId      server.Id   `json:"operation_id"`
	Status           string      `json:"status"`
	State            string      `json:"state"`
	SessionId        *server.Id  `json:"session_id,omitempty"`
	KeptSessionId    *server.Id  `json:"kept_session_id,omitempty"`
	RevokedCount     int         `json:"revoked_count"`
	CleanupPending   bool        `json:"cleanup_pending"`
	Generation       string      `json:"generation"`
	EventId          int64       `json:"event_id"`
	TargetSessionIds []server.Id `json:"-"`
	RetainUntil      time.Time   `json:"-"`
}

// The scripts' extend keeps a key alive until at least deadline. Only a key
// whose expiry falls short of deadline is refreshed, to target when given: a
// target past deadline gives a moving deadline headroom, so the next refresh
// waits until the deadline passes the target instead of following every call.
//
// The network's shared keys (sessionSharedKeys) are KEYS[1..11]:
//
//	1 z          live sessions, scored by acceptance horizon
//	2 held       live and revoked-retained sessions, scored by retention
//	3 generation 4 eid    the revision
//	5 xz         live client sessions whose client has provide enabled (the
//	             cap does not count them), scored as in z
//	6 zi         live sign-in and code sessions; 7 zco live code sessions
//	8 zh, 9 za, 10 zd, 11 zl  live client sessions of a hosted proxy, an API
//	             key (or MCP) client, an adopted device and a per-client
//	             legacy lineage
//
// Every class index is a subset of z with z's scores, pruned with it. classify
// is session_cap.go's sessionClass.
const sessionLuaCommon = `
local now=tonumber(ARGV[1])
local incarnation=ARGV[2]
local redisclock=redis.call('TIME')
local redisnow=tonumber(redisclock[1])*1000+math.floor(tonumber(redisclock[2])/1000)
local SHARED=11
local function extend(key,deadline,target)
 local ttl=redis.call('PTTL',key)
 if ttl==-1 or (ttl>=0 and redisnow+ttl<deadline) then redis.call('PEXPIREAT',key,target or deadline) end
end
local function revision()
 if redis.call('EXISTS',KEYS[3])==0 or redis.call('EXISTS',KEYS[4])==0 then
  redis.call('SET',KEYS[3],incarnation);redis.call('SET',KEYS[4],1)
 end
 return redis.call('GET',KEYS[3]),tonumber(redis.call('GET',KEYS[4]))
end
local function changed() revision();redis.call('INCR',KEYS[4]) end
local function prune()
 local count=redis.call('ZREMRANGEBYSCORE',KEYS[1],'-inf',now)
 redis.call('ZREMRANGEBYSCORE',KEYS[2],'-inf',now)
 for i=5,SHARED do redis.call('ZREMRANGEBYSCORE',KEYS[i],'-inf',now) end
 if count>0 then changed() end
end
local function shared(deadline,target)
 for i=1,SHARED do extend(KEYS[i],deadline,target) end
end
-- reads keep the revision and the live set alive; every write already left
-- each class index alive past its members' horizons
local function sharedcore(deadline,target)
 for i=1,4 do extend(KEYS[i],deadline,target) end
end
local function fail(code) return cjson.encode({error=code}) end
local clientclass={hosted_proxy=8,api_key=9,device=10,legacy=11}
local function classify(kind,network)
 if kind=='auth_code' then return 'code' end
 if kind=='legacy_proxy' or kind=='hosted_proxy' then return 'hosted_proxy' end
 if kind=='api_key_client' then return 'api_key' end
 if kind=='device_adopt' then return 'device' end
 if kind=='legacy' and not network then return 'legacy' end
 return 'signin'
end
local function unindex(sid) for i=5,SHARED do redis.call('ZREM',KEYS[i],sid) end end
local function index(sid,class,accept)
 if class=='signin' or class=='code' then redis.call('ZADD',KEYS[6],accept,sid) end
 if class=='code' then redis.call('ZADD',KEYS[7],accept,sid) end
 local key=clientclass[class];if key then redis.call('ZADD',KEYS[key],accept,sid) end
end
`

// KEYS: the shared keys, then s:, u: and r: of the session and the origin's
// marker. ARGV[11] is the live cap and ARGV[12] the retained cap (zero or less
// is none), ARGV[14] '1' for a network credential, ARGV[15] the provider
// exemption ('1', '0' or 'keep') and ARGV[16] '1' to refuse at a cap. While
// ARGV[16] is not '1' a mint over a cap is admitted and reports the cap in
// would (owner decision 6: the cap ships unenforced); nothing is ever evicted.
const sessionMintLua = sessionLuaCommon + `
local sid=ARGV[3];local accept=tonumber(ARGV[4]);local retain=tonumber(ARGV[5]);local nominal=tonumber(ARGV[6]);local kind=ARGV[7];local created=ARGV[8];local origin=ARGV[9]
local S,U,R,O=KEYS[SHARED+1],KEYS[SHARED+2],KEYS[SHARED+3],KEYS[SHARED+4]
if redis.call('EXISTS',R)==1 or (ARGV[10]=='1' and redis.call('EXISTS',O)==1) then return fail('session_revoked') end
if accept<=now then return fail('session_expired') end
prune()
local previous=redis.call('ZSCORE',KEYS[1],sid)
local reservation=redis.call('ZSCORE',KEYS[2],sid)
local network=ARGV[14]=='1' or redis.call('HGET',S,'network')=='1'
local class=classify(redis.call('HGET',S,'kind') or kind,network)
local exempt=ARGV[15]
if not clientclass[class] then exempt='0' elseif exempt=='keep' then if redis.call('ZSCORE',KEYS[5],sid) then exempt='1' else exempt='0' end end
local livecap=tonumber(ARGV[11]);local retainedcap=tonumber(ARGV[12]);local would=''
if not previous and exempt~='1' and livecap>0 and redis.call('ZCARD',KEYS[1])-redis.call('ZCARD',KEYS[5])>=livecap then would='live' end
if would=='' and not reservation and retainedcap>0 and redis.call('ZCARD',KEYS[2])-redis.call('ZCARD',KEYS[1])>=retainedcap then would='retained' end
if would~='' and ARGV[16]=='1' then
 if would=='live' then return fail('session_limit_reached') end
 return fail('session_retention_limit_reached')
end
accept=math.max(accept,tonumber(previous) or 0);retain=math.max(retain,tonumber(reservation) or 0)
redis.call('ZADD',KEYS[1],accept,sid);redis.call('ZADD',KEYS[2],retain,sid)
redis.call('HSETNX',S,'create_epoch',ARGV[13]);redis.call('HSETNX',S,'kind',kind);redis.call('HSETNX',S,'create_time',created);redis.call('HSETNX',S,'origin_session_id',origin)
local oldclass=redis.call('HGET',S,'class')
if oldclass and oldclass~=class then unindex(sid) end
index(sid,class,accept)
if exempt=='1' then redis.call('ZADD',KEYS[5],accept,sid) else redis.call('ZREM',KEYS[5],sid) end
redis.call('HSET',S,'class',class)
if network then redis.call('HSET',S,'network','1') end
local oldmint=tonumber(redis.call('HGET',S,'last_mint_time')) or 0
local oldnominal=tonumber(redis.call('HGET',S,'token_expire_time')) or 0
redis.call('HSET',S,'last_mint_time',math.max(now,oldmint),'token_expire_time',math.max(nominal,oldnominal))
extend(S,accept);extend(U,accept)
if not previous then changed() end
local generation,eid=revision();shared(retain)
return cjson.encode({accept_until=accept,retain_until=retain,generation=generation,event_id=eid,created=(not previous),would=would,class=class})
`

type registeredSession struct {
	AcceptUntil int64  `json:"accept_until"`
	RetainUntil int64  `json:"retain_until"`
	Generation  string `json:"generation"`
	EventId     int64  `json:"event_id"`
	// the mint added the session to the live set
	Created bool `json:"created"`
	// the cap the mint was admitted over while the cap is not enforced:
	// live, retained or empty
	Would string `json:"would"`
	// the session's class (sessionClass)
	Class string `json:"class"`
}

func sessionEval(ctx context.Context, script string, keys []string, args []any, out any) (returnErr error) {
	operation := "other"
	switch script {
	case sessionMintLua:
		operation = "mint"
	case sessionRevokeLua:
		operation = "revoke"
	case sessionListLua, sessionPageLua:
		operation = "list"
	case sessionInventoryLua, sessionRevisionLua:
		operation = "inventory"
	case sessionFreeLua:
		operation = "free"
	}
	start := time.Now()
	defer func() { observeSessionStorage(operation, start, returnErr) }()
	var raw string
	err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		var err error
		raw, err = r.Eval(ctx, script, keys, args...).Text()
		return err
	})
	if err != nil {
		return errors.Join(ErrAuthUnavailable, ErrSessionStoreUnavailable)
	}
	var refusal struct {
		Error string `json:"error"`
	}
	if err = json.Unmarshal([]byte(raw), &refusal); err != nil {
		return errors.Join(ErrAuthUnavailable, ErrSessionStoreUnavailable)
	}
	if refusal.Error != "" {
		return sessionError(refusal.Error)
	}
	if out != nil {
		return json.Unmarshal([]byte(raw), out)
	}
	return nil
}

// The shared keys every script declares first (sessionLuaCommon).
func sessionSharedKeys(networkId server.Id) []string {
	return []string{
		SessionKey(networkId, "z"),
		SessionKey(networkId, "held"),
		SessionKey(networkId, "generation"),
		SessionKey(networkId, "eid"),
		SessionKey(networkId, "xz"),
		SessionKey(networkId, "zi"),
		SessionKey(networkId, "zco"),
		SessionKey(networkId, "zh"),
		SessionKey(networkId, "za"),
		SessionKey(networkId, "zd"),
		SessionKey(networkId, "zl"),
	}
}
func sessionMemberKeys(networkId, sid server.Id) []string {
	return []string{SessionKey(networkId, "s:"+sid.String()), SessionKey(networkId, "u:"+sid.String()), SessionMarkerKey(networkId, sid)}
}

// Registering before signing is required even while new session creation is dark.
func RegisterNetworkSession(ctx context.Context, credential *ByJwt, kind string, origin *server.Id, fenceOrigin bool, now time.Time) (*registeredSession, error) {
	return registerNetworkSession(ctx, credential, kind, origin, fenceOrigin, now, sessionExemptKeep)
}

// exempt is the provider exemption the mint sets (sessionMintExemption). The
// session cap's policy is read once per mint.
func registerNetworkSession(ctx context.Context, credential *ByJwt, kind string, origin *server.Id, fenceOrigin bool, now time.Time, exempt string) (*registeredSession, error) {
	if credential.SessionId == nil || credential.ExpiresAt == nil {
		return nil, sessionError("invalid_request")
	}
	keys := append(sessionSharedKeys(credential.NetworkId), sessionMemberKeys(credential.NetworkId, *credential.SessionId)...)
	originValue := ""
	originKey := SessionKey(credential.NetworkId, "no_origin")
	if origin != nil {
		originValue = origin.String()
		originKey = SessionMarkerKey(credential.NetworkId, *origin)
	}
	keys = append(keys, originKey)
	fence := "0"
	if fenceOrigin {
		fence = "1"
	}
	network := "0"
	if credential.ClientId == nil {
		network = "1"
	}
	policy := currentSessionCapPolicy()
	enforce := "0"
	if policy.Enforce {
		enforce = "1"
	}
	var result registeredSession
	err := sessionEval(ctx, sessionMintLua, keys, []any{now.UnixMilli(), server.NewId().String(), credential.SessionId.String(), DeadlineMillis(credential.AcceptUntil()), DeadlineMillis(credential.AcceptUntil().Add(SessionRetentionMargin)), DeadlineMillis(credential.ExpiresAt.Time), kind, credential.CreateTime.UTC().Format(time.RFC3339Nano), originValue, fence, max(0, policy.Live), max(0, policy.Retained), credential.CreateTime.UTC().Format("2006-01-02T15:04:05.000000000Z"), network, exempt, enforce}, &result)
	if err != nil {
		// a refused mint is always of a new session, of the class its kind gives
		observeSessionCap(true, sessionClass(kind, credential.ClientId == nil), sessionCapOfRefusal(err))
		return &result, err
	}
	observeSessionCap(false, result.Class, result.Would)
	return &result, nil
}

// ARGV[3] is the retention and ARGV[4] the headroom, in milliseconds
// (sessionInventorySettings).
const sessionInventoryLua = sessionLuaCommon + `
prune();local generation,eid=revision()
local members=redis.call('ZRANGEBYSCORE',KEYS[1],'('..now,'+inf','WITHSCORES')
local maxretain=redis.call('ZREVRANGE',KEYS[2],0,0,'WITHSCORES')
local horizon=math.max(now+tonumber(ARGV[3]),tonumber(maxretain[2]) or 0)
sharedcore(horizon,horizon+tonumber(ARGV[4]))
local encoded=cjson.encode(members);if #members==0 then encoded='[]' end
return '{"members":'..encoded..',"generation":'..cjson.encode(generation)..',"event_id":'..eid..'}'
`

type sessionInventory struct {
	Members    []string `json:"members"`
	Generation string   `json:"generation"`
	EventId    int64    `json:"event_id"`
}

// How an inventory read keeps the network's shared session keys alive.
// Every read leaves them alive until at least its horizon: the later of the
// read plus the retention, and the network's latest retained session.
type sessionInventorySettings struct {
	// How long the shared keys outlive each read, at least, so the revision a
	// hint carries (generation, event id) survives its readers. 24h.
	retention time.Duration
	// How far past the horizon a read sets the shared keys' expiry when it
	// finds that expiry short of the horizon. A read that finds it at or past
	// the horizon leaves it, so the keys of a network that is read steadily
	// are refreshed once per headroom, not on every read. Each refresh is a
	// PEXPIREAT whose keyspace expire event reaches every connect process: on
	// 2026-11-15 the connect hint reads, about 24k a second, each moved the
	// horizon and refreshed, and their expire events overflowed connect's
	// key-event merge (CPUOPT6). 1h: about one refresh an hour per network
	// read, and an idle network's keys outlive its last read by at most an
	// hour more than the retention. Non-positive refreshes on every read that
	// moves the horizon.
	retentionHeadroom time.Duration
}

func defaultSessionInventorySettings() *sessionInventorySettings {
	return &sessionInventorySettings{
		retention:         24 * time.Hour,
		retentionHeadroom: time.Hour,
	}
}

func readSessionInventory(ctx context.Context, networkId server.Id, now time.Time) (sessionInventory, error) {
	settings := defaultSessionInventorySettings()
	var inv sessionInventory
	err := sessionEval(ctx, sessionInventoryLua, sessionSharedKeys(networkId), []any{now.UnixMilli(), server.NewId().String(), settings.retention.Milliseconds(), max(0, settings.retentionHeadroom.Milliseconds())}, &inv)
	return inv, err
}

// The revision alone: the inventory read without its members, for the hints
// connect reads per network (NetworkSessionRevision). A network's live set
// is unbounded once provider sessions are not capped, and a hint never needed
// it. ARGV as sessionInventoryLua.
const sessionRevisionLua = sessionLuaCommon + `
prune();local generation,eid=revision()
local maxretain=redis.call('ZREVRANGE',KEYS[2],0,0,'WITHSCORES')
local horizon=math.max(now+tonumber(ARGV[3]),tonumber(maxretain[2]) or 0)
sharedcore(horizon,horizon+tonumber(ARGV[4]))
return '{"generation":'..cjson.encode(generation)..',"event_id":'..eid..'}'
`

func readSessionRevision(ctx context.Context, networkId server.Id, now time.Time) (sessionInventory, error) {
	settings := defaultSessionInventorySettings()
	var inv sessionInventory
	err := sessionEval(ctx, sessionRevisionLua, sessionSharedKeys(networkId), []any{now.UnixMilli(), server.NewId().String(), settings.retention.Milliseconds(), max(0, settings.retentionHeadroom.Milliseconds())}, &inv)
	return inv, err
}

// The groups a page walks, in order, by the class index each reads
// (sessionLuaCommon): 1 sign-ins and codes (zi), 2 codes (zco), 3 hosted
// proxies (zh), 4 API key clients (za), 5 adopted devices (zd), 6 per-client
// legacy lineages (zl).
const (
	sessionGroupInteractive = 1
	sessionGroupCode        = 2
	sessionGroupHostedProxy = 3
	sessionGroupApiKey      = 4
	sessionGroupDevice      = 5
	sessionGroupLegacy      = 6
)

// One page of members, walking the groups in ARGV[4] (comma separated) from
// the cursor: group ARGV[5] (0: the walk has not started), at scores up to
// ARGV[6] and below member ARGV[7] at that score. Within a group the order is
// the index's: latest acceptance horizon first, so the most recently minted
// first. ARGV[8] '1' skips code sessions in the sign-in group. ARGV[9] is the
// caller's current session, or empty: the first page (ARGV[10] '1') leads
// with it when it is live in a walked group, and every walk passes over it,
// so it is read once, first. The next cursor is the last member the walk
// returned, or the walk's start when the page held only the current session.
// Returns at most ARGV[3] members, the next cursor and the network's counts.
const sessionPageLua = sessionLuaCommon + `
prune();local generation,eid=revision()
local groupkey={6,7,8,9,10,11}
local limit=tonumber(ARGV[3])
local cg=tonumber(ARGV[5]);local cs=tonumber(ARGV[6]);local cm=ARGV[7];local current=ARGV[9]
local out={}
if ARGV[10]=='1' and current~='' then
 for g in string.gmatch(ARGV[4],'%d+') do
  g=tonumber(g)
  local score=redis.call('ZSCORE',KEYS[groupkey[g]],current)
  if score and tonumber(score)>now and not (g==1 and ARGV[8]=='1' and redis.call('ZSCORE',KEYS[7],current)) then
   table.insert(out,{current,g,tonumber(score)});break
  end
 end
end
local budget=limit-#out
local walked={};local started=(cg==0)
for g in string.gmatch(ARGV[4],'%d+') do
 g=tonumber(g)
 if not started and g==cg then started=true end
 if started and #walked<=budget then
  local key=KEYS[groupkey[g]];local max='+inf';local skipping=false
  if g==cg then max=ARGV[6];skipping=true end
  local offset=0
  while #walked<=budget do
   local batch=redis.call('ZREVRANGEBYSCORE',key,max,'('..now,'WITHSCORES','LIMIT',offset,128)
   if #batch==0 then break end
   for i=1,#batch,2 do
    local m=batch[i];local s=tonumber(batch[i+1]);local take=true
    if skipping then if s==cs and m>=cm then take=false else skipping=false end end
    if take and m==current then take=false end
    if take and g==1 and ARGV[8]=='1' and redis.call('ZSCORE',KEYS[7],m) then take=false end
    if take then table.insert(walked,{m,g,s}) end
    if #walked>budget then break end
   end
   offset=offset+128
  end
 end
end
local nextcursor='null'
if #walked>budget then
 table.remove(walked)
 if #walked>0 then local last=walked[#walked];nextcursor=cjson.encode({last[2],last[3],last[1]})
 else nextcursor='[0,0,""]' end
end
for _,w in ipairs(walked) do table.insert(out,w) end
local members=cjson.encode(out);if #out==0 then members='[]' end
local counts={}
for _,i in ipairs({1,5,6,7,8,9,10,11}) do table.insert(counts,redis.call('ZCOUNT',KEYS[i],'('..now,'+inf')) end
return '{"members":'..members..',"next":'..nextcursor..',"generation":'..cjson.encode(generation)..',"event_id":'..eid..',"counts":'..cjson.encode(counts)..'}'
`

// The page's rows, read coherently with the revision the page was read at:
// any change of the live set changes the revision (a refresh extends a
// member but changes nothing else), so a matching revision proves every
// member is still live. A live member that carries a marker is repaired and
// the page read again. KEYS: the shared keys, then s:, u: and r: of each
// member; ARGV[3..4] the revision, ARGV[5] the member count, then the members.
const sessionListLua = sessionLuaCommon + `
prune();local generation,eid=revision()
if generation~=ARGV[3] or eid~=tonumber(ARGV[4]) then return fail('session_snapshot_changed') end
local result={}
for i=1,tonumber(ARGV[5]) do
 local sid=ARGV[5+i];local base=SHARED+(i-1)*3
 local accept=redis.call('ZSCORE',KEYS[1],sid)
 if not accept or tonumber(accept)<=now then return fail('session_snapshot_changed') end
 if redis.call('EXISTS',KEYS[base+3])==1 then
  redis.call('ZREM',KEYS[1],sid);unindex(sid);redis.call('DEL',KEYS[base+1],KEYS[base+2]);changed();return fail('session_snapshot_changed')
 end
 local m=redis.call('HMGET',KEYS[base+1],'kind','create_time','last_mint_time','token_expire_time','origin_session_id','class','network')
 local used=redis.call('GET',KEYS[base+2])
 local provider=redis.call('ZSCORE',KEYS[5],sid)~=false
 table.insert(result,{sid=sid,accept=tonumber(accept),kind=m[1] or 'unknown',created=m[2] or '',mint=tonumber(m[3]) or 0,nominal=tonumber(m[4]) or 0,origin=m[5] or '',class=m[6] or '',network=(m[7]=='1'),provider=provider,used=used or ''})
end
local encoded=cjson.encode(result);if #result==0 then encoded='[]' end
return '{"rows":'..encoded..',"generation":'..cjson.encode(generation)..',"event_id":'..eid..'}'
`

// The groups a request reads, in order, and whether the sign-in group skips
// codes. The default reads every group: the sign-ins first, as before
// client sessions existed, so a client that reads one page sees them.
func sessionPageGroups(args *NetworkSessionsArgs) (groups []int, signinOnly bool, err error) {
	sessionKind, clientKind := "", ""
	if args != nil {
		sessionKind, clientKind = args.SessionKind, args.ClientKind
	}
	clientGroups := map[string]int{
		ClientKindHostedProxy: sessionGroupHostedProxy,
		ClientKindApiKey:      sessionGroupApiKey,
		ClientKindDevice:      sessionGroupDevice,
		ClientKindLegacy:      sessionGroupLegacy,
	}
	if clientKind != "" {
		group, ok := clientGroups[clientKind]
		if !ok || (sessionKind != "" && sessionKind != SessionKindClient) {
			return nil, false, sessionError("invalid_request")
		}
		return []int{group}, false, nil
	}
	switch sessionKind {
	case "":
		return []int{sessionGroupInteractive, sessionGroupHostedProxy, sessionGroupApiKey, sessionGroupDevice, sessionGroupLegacy}, false, nil
	case SessionKindSignin:
		return []int{sessionGroupInteractive}, true, nil
	case SessionKindCode:
		return []int{sessionGroupCode}, false, nil
	case SessionKindClient:
		return []int{sessionGroupHostedProxy, sessionGroupApiKey, sessionGroupDevice, sessionGroupLegacy}, false, nil
	}
	return nil, false, sessionError("invalid_request")
}

// A page cursor: the filter's groups and the last member a page returned.
// Opaque to callers; a cursor read with other filters is refused.
type sessionPageCursor struct {
	groups string
	group  int
	score  int64
	member server.Id
}

func encodeSessionPageCursor(cursor sessionPageCursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("v1|%s|%d|%d|%s", cursor.groups, cursor.group, cursor.score, cursor.member)))
}

func decodeSessionPageCursor(encoded string, groups string) (*sessionPageCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, sessionError("invalid_request")
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 5 || parts[0] != "v1" || parts[1] != groups {
		return nil, sessionError("invalid_request")
	}
	cursor := &sessionPageCursor{groups: groups}
	// group 0 is the walk's start, after a first page that held the current
	// session only
	if cursor.group, err = strconv.Atoi(parts[2]); err != nil || cursor.group < 0 || sessionGroupLegacy < cursor.group {
		return nil, sessionError("invalid_request")
	}
	if cursor.score, err = strconv.ParseInt(parts[3], 10, 64); err != nil {
		return nil, sessionError("invalid_request")
	}
	if cursor.member, err = server.ParseId(parts[4]); err != nil {
		return nil, sessionError("invalid_request")
	}
	return cursor, nil
}

// GET /network/sessions with no parameters: the first page of every kind.
func GetNetworkSessions(clientSession *ClientSession) (*NetworkSessionsResult, error) {
	return GetNetworkSessionsPage(clientSession, nil)
}

// One page of the network's live sessions: sign-ins and codes first, then
// the client sessions by what owns them (hosted proxies, API key clients,
// adopted devices, per-client legacy lineages), each newest first. A page is
// one coherent read; a page whose revision differs from the previous page's
// may overlap it or miss a session, and the caller reads again from the
// first page when it needs a coherent whole. The counts cover the whole
// network whatever the filters.
func GetNetworkSessionsPage(clientSession *ClientSession, args *NetworkSessionsArgs) (*NetworkSessionsResult, error) {
	if clientSession.ByJwt.ClientId != nil {
		return nil, sessionError("network_credential_required")
	}
	groups, signinOnly, err := sessionPageGroups(args)
	if err != nil {
		return nil, err
	}
	groupNames := make([]string, len(groups))
	for i, group := range groups {
		groupNames[i] = strconv.Itoa(group)
	}
	groupList := strings.Join(groupNames, ",")
	limit := NetworkSessionsPageLimit
	var cursor *sessionPageCursor
	if args != nil {
		if args.Limit < 0 || NetworkSessionsPageLimit < args.Limit {
			return nil, sessionError("invalid_request")
		}
		if 0 < args.Limit {
			limit = args.Limit
		}
		if args.Cursor != "" {
			if cursor, err = decodeSessionPageCursor(args.Cursor, groupList); err != nil {
				return nil, err
			}
		}
	}
	signinOnlyValue := "0"
	if signinOnly {
		signinOnlyValue = "1"
	}
	networkId := clientSession.ByJwt.NetworkId
	ctx, cancel := context.WithTimeout(clientSession.Ctx, 2*time.Second)
	defer cancel()
	for attempts := 0; attempts < 4; attempts++ {
		now := server.NowUtc()
		current := ""
		if clientSession.ByJwt.SessionId != nil {
			current = clientSession.ByJwt.SessionId.String()
		}
		pageArgs := []any{now.UnixMilli(), server.NewId().String(), limit, groupList, 0, 0, "", signinOnlyValue, current, "1"}
		if cursor != nil {
			member := ""
			if cursor.group != 0 {
				member = cursor.member.String()
			}
			pageArgs = []any{now.UnixMilli(), server.NewId().String(), limit, groupList, cursor.group, cursor.score, member, signinOnlyValue, current, "0"}
		}
		var page struct {
			Members    [][3]json.RawMessage `json:"members"`
			Next       []json.RawMessage    `json:"next"`
			Generation string               `json:"generation"`
			EventId    int64                `json:"event_id"`
			Counts     []int                `json:"counts"`
		}
		if err := sessionEval(ctx, sessionPageLua, sessionSharedKeys(networkId), pageArgs, &page); err != nil {
			return nil, err
		}
		type pageMember struct {
			sid   server.Id
			group int
		}
		members := []pageMember{}
		for _, raw := range page.Members {
			var sid string
			var group int
			if json.Unmarshal(raw[0], &sid) != nil || json.Unmarshal(raw[1], &group) != nil {
				return nil, ErrSessionStoreUnavailable
			}
			id, err := server.ParseId(sid)
			if err != nil {
				return nil, ErrSessionStoreUnavailable
			}
			members = append(members, pageMember{sid: id, group: group})
		}
		keys := sessionSharedKeys(networkId)
		listArgs := []any{now.UnixMilli(), server.NewId().String(), page.Generation, page.EventId, len(members)}
		for _, member := range members {
			keys = append(keys, sessionMemberKeys(networkId, member.sid)...)
			listArgs = append(listArgs, member.sid.String())
		}
		var snapshot struct {
			Rows []struct {
				Sid      server.Id `json:"sid"`
				Accept   int64     `json:"accept"`
				Kind     string    `json:"kind"`
				Created  string    `json:"created"`
				Mint     int64     `json:"mint"`
				Nominal  int64     `json:"nominal"`
				Origin   string    `json:"origin"`
				Class    string    `json:"class"`
				Network  bool      `json:"network"`
				Provider bool      `json:"provider"`
				Used     string    `json:"used"`
			} `json:"rows"`
			Generation string `json:"generation"`
			EventId    int64  `json:"event_id"`
		}
		err = sessionEval(ctx, sessionListLua, keys, listArgs, &snapshot)
		if err != nil {
			var se *SessionError
			if errors.As(err, &se) && se.Code == "session_snapshot_changed" {
				continue
			}
			return nil, err
		}
		policy := currentSessionCapPolicy()
		out := &NetworkSessionsResult{
			Sessions:         []*NetworkSession{},
			Generation:       snapshot.Generation,
			EventId:          snapshot.EventId,
			CurrentSessionId: clientSession.ByJwt.SessionId,
			LegacyCoverage:   "partial",
			Counts:           sessionCounts(page.Counts, policy),
		}
		groupOf := map[server.Id]int{}
		for _, member := range members {
			groupOf[member.sid] = member.group
		}
		for _, row := range snapshot.Rows {
			item := &NetworkSession{SessionId: row.Sid, Kind: row.Kind, Provider: row.Provider, LastMintTime: time.UnixMilli(row.Mint).UTC(), TokenExpireTime: time.UnixMilli(row.Nominal).UTC(), AcceptUntil: time.UnixMilli(row.Accept).UTC()}
			class := row.Class
			if class == "" {
				// registered before sessions carried their class
				class = sessionClass(row.Kind, row.Network)
			}
			item.SessionKind, item.ClientKind = SessionKinds(class)
			item.CreateTime, _ = time.Parse(time.RFC3339Nano, row.Created)
			item.Current = out.CurrentSessionId != nil && *out.CurrentSessionId == item.SessionId
			if origin, err := server.ParseId(row.Origin); err == nil {
				item.OriginSessionId = &origin
			}
			if row.Used != "" {
				var use SessionLastUsed
				if json.Unmarshal([]byte(row.Used), &use) == nil {
					item.LastUsed = &use
				}
			}
			out.Sessions = append(out.Sessions, item)
		}
		if len(page.Next) == 3 {
			var next sessionPageCursor
			var member string
			if json.Unmarshal(page.Next[0], &next.group) != nil || json.Unmarshal(page.Next[1], &next.score) != nil || json.Unmarshal(page.Next[2], &member) != nil {
				return nil, ErrSessionStoreUnavailable
			}
			// group 0: the walk has not started (the page held the current
			// session only)
			if next.group != 0 {
				if next.member, err = server.ParseId(member); err != nil {
					return nil, ErrSessionStoreUnavailable
				}
			}
			next.groups = groupList
			encoded := encodeSessionPageCursor(next)
			out.NextCursor = &encoded
		}
		// within the page: the groups in order, the current session first,
		// then by last use (or creation), newest first
		sort.SliceStable(out.Sessions, func(i, j int) bool {
			a, b := out.Sessions[i], out.Sessions[j]
			if ga, gb := groupOf[a.SessionId], groupOf[b.SessionId]; ga != gb {
				return ga < gb
			}
			if a.Current != b.Current {
				return a.Current
			}
			at, bt := a.CreateTime.Unix(), b.CreateTime.Unix()
			if a.LastUsed != nil {
				at = a.LastUsed.UnixTime
			}
			if b.LastUsed != nil {
				bt = b.LastUsed.UnixTime
			}
			if at != bt {
				return at > bt
			}
			return a.SessionId.Less(b.SessionId)
		})
		return out, nil
	}
	return nil, ErrSessionStoreUnavailable
}

// The page script's counts: z, xz, zi, zco, zh, za, zd, zl.
func sessionCounts(counts []int, policy SessionCapPolicy) *NetworkSessionCounts {
	out := &NetworkSessionCounts{Limit: max(0, policy.Live), LimitEnforced: policy.Enforce}
	if len(counts) != 8 {
		return out
	}
	out.Live = counts[0]
	out.Provider = counts[1]
	out.Capped = max(0, counts[0]-counts[1])
	out.Signin = max(0, counts[2]-counts[3])
	out.Code = counts[3]
	out.HostedProxy = counts[4]
	out.ApiKey = counts[5]
	out.Device = counts[6]
	out.Legacy = counts[7]
	return out
}

// KEYS: the shared keys, the operation receipt, then s:, u: and r: of each
// target. A password reset (reset), sign-out-all (reset) and sign out other
// sessions (others) never retire a hosted proxy's own session (owner decision
// 4): a proxy ends only when it is removed or its own session is revoked
// (single, or the network's deletion).
const sessionRevokeLua = sessionLuaCommon + `
local OP=KEYS[SHARED+1]
local receipt=redis.call('GET',OP);if receipt then return receipt end
prune();local generation,eid=revision()
local action=ARGV[3];local operation=ARGV[4];local keep=ARGV[5];local count=tonumber(ARGV[8]);local targets={};local newly=0;local untiltime=now+86400000
if action=='others' or action=='reset' or action=='delete' then
 if generation~=ARGV[6] or eid~=tonumber(ARGV[7]) then return fail('session_snapshot_changed') end
 local live=redis.call('ZRANGEBYSCORE',KEYS[1],'('..now,'+inf')
 if #live~=count then return fail('session_snapshot_changed') end
 for i=1,count do if live[i]~=ARGV[8+i] then return fail('session_snapshot_changed') end end
 if action=='others' then local kept=redis.call('ZSCORE',KEYS[1],keep);if not kept or tonumber(kept)<=now then return fail('current_session_required') end end
end
local function eligible(sid,base)
 if sid==keep then return false end
 if action=='reset' or action=='others' then
  local kind=redis.call('HGET',KEYS[base+1],'kind')
  if kind=='legacy_proxy' or kind=='hosted_proxy' then return false end
 end
 if action=='reset' then
  local created=redis.call('HGET',KEYS[base+1],'create_epoch')
  -- All pre-v1 metadata predates the reset. Fresh mints stamp fixed UTC nanos.
  if created and created>=ARGV[9+count] then return false end
 end
 return true
end
for i=1,count do
 local sid=ARGV[8+i];local base=SHARED+1+(i-1)*3
 if eligible(sid,base) then
  local accept=tonumber(redis.call('ZSCORE',KEYS[1],sid)) or 0
  local retain=tonumber(redis.call('ZSCORE',KEYS[2],sid)) or 0
  local marked=redis.call('EXISTS',KEYS[base+3])==1
  if not marked and accept<=now then return fail('session_not_found') end
  table.insert(targets,sid);untiltime=math.max(untiltime,retain,accept+300000)
 end
end
for i=1,count do
 local sid=ARGV[8+i];local base=SHARED+1+(i-1)*3
 if eligible(sid,base) then
  local retain=math.max(tonumber(redis.call('ZSCORE',KEYS[2],sid)) or 0,(tonumber(redis.call('ZSCORE',KEYS[1],sid)) or 0)+300000,now+300000)
  if redis.call('EXISTS',KEYS[base+3])==0 then newly=newly+1;redis.call('SET',KEYS[base+3],1) end
  extend(KEYS[base+3],retain);redis.call('ZADD',KEYS[2],retain,sid)
  redis.call('ZREM',KEYS[1],sid);unindex(sid);redis.call('DEL',KEYS[base+1],KEYS[base+2])
 end
end
if newly>0 then changed() end
generation,eid=revision();local status='revoked';if newly==0 then status='already_revoked' end
local result=cjson.encode({operation_id=operation,status=status,state='enforced',revoked_count=newly,cleanup_pending=#targets>0,generation=generation,event_id=eid,targets=targets,retain_until=untiltime})
if #targets==0 then result=string.gsub(result,'"targets":{}','"targets":[]') end
redis.call('SET',OP,result);extend(OP,untiltime);shared(untiltime);return result
`

type sessionReceipt struct {
	SessionOperationResult
	Targets   []server.Id `json:"targets"`
	Retention int64       `json:"retain_until"`
}

func enforceSessionRevoke(ctx context.Context, networkId, operationId server.Id, action string, target, keep *server.Id, cutoff ...time.Time) (*SessionOperationResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for attempts := 0; attempts < 4; attempts++ {
		now := server.NowUtc()
		inv := sessionInventory{}
		var targets []server.Id
		if action == "others" || action == "reset" || action == "delete" {
			var err error
			inv, err = readSessionInventory(ctx, networkId, now)
			if err != nil {
				return nil, err
			}
			for i := 0; i < len(inv.Members); i += 2 {
				sid, err := server.ParseId(inv.Members[i])
				if err != nil {
					return nil, err
				}
				targets = append(targets, sid)
			}
		} else if target != nil {
			targets = []server.Id{*target}
		} else {
			return nil, sessionError("invalid_request")
		}
		keepValue := ""
		if keep != nil {
			keepValue = keep.String()
		}
		keys := append(sessionSharedKeys(networkId), SessionKey(networkId, "op:"+operationId.String()))
		args := []any{now.UnixMilli(), server.NewId().String(), action, operationId.String(), keepValue, inv.Generation, inv.EventId, len(targets)}
		for _, sid := range targets {
			keys = append(keys, sessionMemberKeys(networkId, sid)...)
			args = append(args, sid.String())
		}
		if len(cutoff) > 0 {
			args = append(args, cutoff[0].UTC().Format("2006-01-02T15:04:05.000000000Z"))
		} else {
			args = append(args, now.UTC().Format("2006-01-02T15:04:05.000000000Z"))
		}
		var receipt sessionReceipt
		err := sessionEval(ctx, sessionRevokeLua, keys, args, &receipt)
		if err != nil {
			var se *SessionError
			if errors.As(err, &se) && se.Code == "session_snapshot_changed" {
				continue
			}
			return nil, err
		}
		receipt.SessionId = target
		receipt.KeptSessionId = keep
		receipt.TargetSessionIds = receipt.Targets
		receipt.RetainUntil = time.UnixMilli(receipt.Retention).UTC()
		return &receipt.SessionOperationResult, nil
	}
	return nil, ErrSessionStoreUnavailable
}

func readSessionReceipt(ctx context.Context, networkId, operationId server.Id) (*SessionOperationResult, error) {
	var raw string
	err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		var err error
		raw, err = r.Get(ctx, SessionKey(networkId, "op:"+operationId.String())).Result()
		return err
	})
	if errors.Is(err, server.RedisNil) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.Join(ErrAuthUnavailable, ErrSessionStoreUnavailable)
	}
	var receipt sessionReceipt
	if err = json.Unmarshal([]byte(raw), &receipt); err != nil {
		return nil, fmt.Errorf("invalid session receipt: %w", ErrSessionStoreUnavailable)
	}
	receipt.TargetSessionIds = receipt.Targets
	receipt.RetainUntil = time.UnixMilli(receipt.Retention).UTC()
	return &receipt.SessionOperationResult, nil
}

// Notifications share the authoritative list incarnation. Missing paired keys
// are initialized together, never interpreted as ordered zero. A read that
// finds the shared keys alive past its horizon writes no ttl, so it publishes
// no keyspace event (sessionInventorySettings).
//
// It reads the revision alone (sessionRevisionLua): connect reads it for every
// network with a hint listener, and the members of a large network are no
// part of a hint.
func NetworkSessionRevision(ctx context.Context, networkId server.Id) (string, int64, error) {
	inventory, err := readSessionRevision(ctx, networkId, server.NowUtc())
	return inventory.Generation, inventory.EventId, err
}
