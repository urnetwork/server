// Network session inventory is Redis denylist authority; SQL owns recoverable work.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/urnetwork/server/v2026"
)

const NetworkSessionLiveLimit = 1000
const NetworkSessionRetainedLimit = 10000

// Closed machine codes are safe to expose; storage details never cross the API.
type SessionError struct {
	Code   string
	Status int
}

func (self *SessionError) Error() string { return fmt.Sprintf("%d %s", self.Status, self.Code) }
func (self *SessionError) HttpErrorResultBody() any {
	return map[string]any{"error": "Session request could not be completed.", "code": self.Code}
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
	SessionId       server.Id        `json:"session_id"`
	Current         bool             `json:"current"`
	Kind            string           `json:"kind"`
	CreateTime      time.Time        `json:"create_time"`
	LastMintTime    time.Time        `json:"last_mint_time"`
	TokenExpireTime time.Time        `json:"token_expire_time"`
	AcceptUntil     time.Time        `json:"accept_until"`
	OriginSessionId *server.Id       `json:"origin_session_id"`
	LastUsed        *SessionLastUsed `json:"last_used"`
}
type NetworkSessionsResult struct {
	Sessions         []*NetworkSession `json:"sessions"`
	Generation       string            `json:"generation"`
	EventId          int64             `json:"event_id"`
	CurrentSessionId *server.Id        `json:"current_session_id"`
	LegacyCoverage   string            `json:"legacy_coverage"`
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

const sessionLuaCommon = `
local now=tonumber(ARGV[1])
local incarnation=ARGV[2]
local redisclock=redis.call('TIME')
local redisnow=tonumber(redisclock[1])*1000+math.floor(tonumber(redisclock[2])/1000)
local function extend(key,deadline)
 local ttl=redis.call('PTTL',key)
 if ttl==-1 or (ttl>=0 and redisnow+ttl<deadline) then redis.call('PEXPIREAT',key,deadline) end
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
 if count>0 then changed() end
end
local function shared(deadline)
 for i=1,4 do extend(KEYS[i],deadline) end
end
local function fail(code) return cjson.encode({error=code}) end
`

const sessionMintLua = sessionLuaCommon + `
local sid=ARGV[3];local accept=tonumber(ARGV[4]);local retain=tonumber(ARGV[5]);local nominal=tonumber(ARGV[6]);local kind=ARGV[7];local created=ARGV[8];local origin=ARGV[9]
if redis.call('EXISTS',KEYS[7])==1 or (ARGV[10]=='1' and redis.call('EXISTS',KEYS[8])==1) then return fail('session_revoked') end
if accept<=now then return fail('session_expired') end
prune()
local previous=redis.call('ZSCORE',KEYS[1],sid)
local reservation=redis.call('ZSCORE',KEYS[2],sid)
if not previous and redis.call('ZCARD',KEYS[1])>=tonumber(ARGV[11]) then return fail('session_limit_reached') end
if not reservation and redis.call('ZCARD',KEYS[2])>=tonumber(ARGV[12]) then return fail('session_retention_limit_reached') end
accept=math.max(accept,tonumber(previous) or 0);retain=math.max(retain,tonumber(reservation) or 0)
redis.call('ZADD',KEYS[1],accept,sid);redis.call('ZADD',KEYS[2],retain,sid)
redis.call('HSETNX',KEYS[5],'create_epoch',ARGV[13]);redis.call('HSETNX',KEYS[5],'kind',kind);redis.call('HSETNX',KEYS[5],'create_time',created);redis.call('HSETNX',KEYS[5],'origin_session_id',origin)
local oldmint=tonumber(redis.call('HGET',KEYS[5],'last_mint_time')) or 0
local oldnominal=tonumber(redis.call('HGET',KEYS[5],'token_expire_time')) or 0
redis.call('HSET',KEYS[5],'last_mint_time',math.max(now,oldmint),'token_expire_time',math.max(nominal,oldnominal))
extend(KEYS[5],accept);extend(KEYS[6],accept)
if not previous then changed() end
local generation,eid=revision();shared(retain)
return cjson.encode({accept_until=accept,retain_until=retain,generation=generation,event_id=eid})
`

type registeredSession struct {
	AcceptUntil int64  `json:"accept_until"`
	RetainUntil int64  `json:"retain_until"`
	Generation  string `json:"generation"`
	EventId     int64  `json:"event_id"`
}

func sessionEval(ctx context.Context, script string, keys []string, args []any, out any) (returnErr error) {
	operation := "other"
	switch script {
	case sessionMintLua:
		operation = "mint"
	case sessionRevokeLua:
		operation = "revoke"
	case sessionListLua:
		operation = "list"
	case sessionInventoryLua:
		operation = "inventory"
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
func sessionSharedKeys(networkId server.Id) []string {
	return []string{SessionKey(networkId, "z"), SessionKey(networkId, "held"), SessionKey(networkId, "generation"), SessionKey(networkId, "eid")}
}
func sessionMemberKeys(networkId, sid server.Id) []string {
	return []string{SessionKey(networkId, "s:"+sid.String()), SessionKey(networkId, "u:"+sid.String()), SessionMarkerKey(networkId, sid)}
}

// Registering before signing is required even while new session creation is dark.
func RegisterNetworkSession(ctx context.Context, credential *ByJwt, kind string, origin *server.Id, fenceOrigin bool, now time.Time) (*registeredSession, error) {
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
	var result registeredSession
	err := sessionEval(ctx, sessionMintLua, keys, []any{now.UnixMilli(), server.NewId().String(), credential.SessionId.String(), DeadlineMillis(credential.AcceptUntil()), DeadlineMillis(credential.AcceptUntil().Add(SessionRetentionMargin)), DeadlineMillis(credential.ExpiresAt.Time), kind, credential.CreateTime.UTC().Format(time.RFC3339Nano), originValue, fence, NetworkSessionLiveLimit, NetworkSessionRetainedLimit, credential.CreateTime.UTC().Format("2006-01-02T15:04:05.000000000Z")}, &result)
	return &result, err
}

const sessionInventoryLua = sessionLuaCommon + `
prune();local generation,eid=revision()
local members=redis.call('ZRANGEBYSCORE',KEYS[1],'('..now,'+inf','WITHSCORES')
local maxretain=redis.call('ZREVRANGE',KEYS[2],0,0,'WITHSCORES')
shared(math.max(now+86400000,tonumber(maxretain[2]) or 0))
local encoded=cjson.encode(members);if #members==0 then encoded='[]' end
return '{"members":'..encoded..',"generation":'..cjson.encode(generation)..',"event_id":'..eid..'}'
`

type sessionInventory struct {
	Members    []string `json:"members"`
	Generation string   `json:"generation"`
	EventId    int64    `json:"event_id"`
}

func readSessionInventory(ctx context.Context, networkId server.Id, now time.Time) (sessionInventory, error) {
	var inv sessionInventory
	err := sessionEval(ctx, sessionInventoryLua, sessionSharedKeys(networkId), []any{now.UnixMilli(), server.NewId().String()}, &inv)
	return inv, err
}

const sessionListLua = sessionLuaCommon + `
prune();local generation,eid=revision()
if generation~=ARGV[3] or eid~=tonumber(ARGV[4]) then return fail('session_snapshot_changed') end
local members=redis.call('ZRANGEBYSCORE',KEYS[1],'('..now,'+inf','WITHSCORES')
if #members~=tonumber(ARGV[5])*2 then return fail('session_snapshot_changed') end
local result={}
for i=1,#members/2 do
 if members[2*i-1]~=ARGV[5+i] then return fail('session_snapshot_changed') end
 local base=4+(i-1)*3
 if redis.call('EXISTS',KEYS[base+3])==1 then
  redis.call('ZREM',KEYS[1],members[2*i-1]);redis.call('DEL',KEYS[base+1],KEYS[base+2]);changed();return fail('session_snapshot_changed')
 end
 local m=redis.call('HMGET',KEYS[base+1],'kind','create_time','last_mint_time','token_expire_time','origin_session_id')
 local used=redis.call('GET',KEYS[base+2])
 table.insert(result,{sid=members[2*i-1],accept=tonumber(members[2*i]),kind=m[1] or 'unknown',created=m[2] or '',mint=tonumber(m[3]) or 0,nominal=tonumber(m[4]) or 0,origin=m[5] or '',used=used or ''})
end
local encoded=cjson.encode(result);if #result==0 then encoded='[]' end
return '{"rows":'..encoded..',"generation":'..cjson.encode(generation)..',"event_id":'..eid..'}'
`

func GetNetworkSessions(clientSession *ClientSession) (*NetworkSessionsResult, error) {
	if clientSession.ByJwt.ClientId != nil {
		return nil, sessionError("network_credential_required")
	}
	ctx, cancel := context.WithTimeout(clientSession.Ctx, 2*time.Second)
	defer cancel()
	for attempts := 0; attempts < 4; attempts++ {
		now := server.NowUtc()
		inv, err := readSessionInventory(ctx, clientSession.ByJwt.NetworkId, now)
		if err != nil {
			return nil, err
		}
		keys := sessionSharedKeys(clientSession.ByJwt.NetworkId)
		args := []any{now.UnixMilli(), server.NewId().String(), inv.Generation, inv.EventId, len(inv.Members) / 2}
		for i := 0; i < len(inv.Members); i += 2 {
			sid, err := server.ParseId(inv.Members[i])
			if err != nil {
				return nil, ErrSessionStoreUnavailable
			}
			keys = append(keys, sessionMemberKeys(clientSession.ByJwt.NetworkId, sid)...)
			args = append(args, sid.String())
		}
		var snapshot struct {
			Rows []struct {
				Sid     server.Id `json:"sid"`
				Accept  int64     `json:"accept"`
				Kind    string    `json:"kind"`
				Created string    `json:"created"`
				Mint    int64     `json:"mint"`
				Nominal int64     `json:"nominal"`
				Origin  string    `json:"origin"`
				Used    string    `json:"used"`
			} `json:"rows"`
			Generation string `json:"generation"`
			EventId    int64  `json:"event_id"`
		}
		err = sessionEval(ctx, sessionListLua, keys, args, &snapshot)
		if err != nil {
			var se *SessionError
			if errors.As(err, &se) && se.Code == "session_snapshot_changed" {
				continue
			}
			return nil, err
		}
		out := &NetworkSessionsResult{Sessions: []*NetworkSession{}, Generation: snapshot.Generation, EventId: snapshot.EventId, CurrentSessionId: clientSession.ByJwt.SessionId, LegacyCoverage: "partial"}
		for _, row := range snapshot.Rows {
			item := &NetworkSession{SessionId: row.Sid, Kind: row.Kind, LastMintTime: time.UnixMilli(row.Mint).UTC(), TokenExpireTime: time.UnixMilli(row.Nominal).UTC(), AcceptUntil: time.UnixMilli(row.Accept).UTC()}
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
		sort.Slice(out.Sessions, func(i, j int) bool {
			a, b := out.Sessions[i], out.Sessions[j]
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

const sessionRevokeLua = sessionLuaCommon + `
local receipt=redis.call('GET',KEYS[5]);if receipt then return receipt end
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
 if action=='reset' then
  local created=redis.call('HGET',KEYS[base+1],'create_epoch')
  -- All pre-v1 metadata predates the reset. Fresh mints stamp fixed UTC nanos.
  if created and created>=ARGV[9+count] then return false end
 end
 return true
end
for i=1,count do
 local sid=ARGV[8+i];local base=5+(i-1)*3
 if eligible(sid,base) then
  local accept=tonumber(redis.call('ZSCORE',KEYS[1],sid)) or 0
  local retain=tonumber(redis.call('ZSCORE',KEYS[2],sid)) or 0
  local marked=redis.call('EXISTS',KEYS[base+3])==1
  if not marked and accept<=now then return fail('session_not_found') end
  table.insert(targets,sid);untiltime=math.max(untiltime,retain,accept+300000)
 end
end
for i=1,count do
 local sid=ARGV[8+i];local base=5+(i-1)*3
 if eligible(sid,base) then
  local retain=math.max(tonumber(redis.call('ZSCORE',KEYS[2],sid)) or 0,(tonumber(redis.call('ZSCORE',KEYS[1],sid)) or 0)+300000,now+300000)
  if redis.call('EXISTS',KEYS[base+3])==0 then newly=newly+1;redis.call('SET',KEYS[base+3],1) end
  extend(KEYS[base+3],retain);redis.call('ZADD',KEYS[2],retain,sid)
  redis.call('ZREM',KEYS[1],sid);redis.call('DEL',KEYS[base+1],KEYS[base+2])
 end
end
if newly>0 then changed() end
generation,eid=revision();local status='revoked';if newly==0 then status='already_revoked' end
local result=cjson.encode({operation_id=operation,status=status,state='enforced',revoked_count=newly,cleanup_pending=#targets>0,generation=generation,event_id=eid,targets=targets,retain_until=untiltime})
if #targets==0 then result=string.gsub(result,'"targets":{}','"targets":[]') end
redis.call('SET',KEYS[5],result);extend(KEYS[5],untiltime);shared(untiltime);return result
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
// are initialized together, never interpreted as ordered zero.
func NetworkSessionRevision(ctx context.Context, networkId server.Id) (string, int64, error) {
	inventory, err := readSessionInventory(ctx, networkId, server.NowUtc())
	return inventory.Generation, inventory.EventId, err
}
