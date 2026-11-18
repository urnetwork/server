// The session cap, the kinds of tracked sessions and what a client's removal
// frees (session1118 FIXPLAN, owner decisions 2026-11-18).
package session

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// The R-kinds of a tracked session, as GET /network/sessions reports them in
// session_kind and the cap metrics in their kind label.
const (
	// a sign-in: password, SSO, wallet, verify, seed phrase, signup, a lag.dev
	// guest, and every legacy lineage a network credential carries
	SessionKindSignin = "signin"
	// an auth code redemption
	SessionKindCode = "code"
	// owned by one client: a hosted proxy, a client an API key or an MCP
	// identity created, an adopted device, and a per-client legacy lineage
	SessionKindClient = "client"
)

// What owns a client session, as GET /network/sessions reports it in
// client_kind.
const (
	ClientKindHostedProxy = "hosted_proxy"
	ClientKindApiKey      = "api_key"
	ClientKindDevice      = "device"
	ClientKindLegacy      = "legacy"
)

// The kinds a mint registers a new session with (the kind field of
// GET /network/sessions, which the apps label the sign-in method with).
const (
	MintKindAuthCode     = "auth_code"
	MintKindLegacyProxy  = "legacy_proxy"
	MintKindHostedProxy  = "hosted_proxy"
	MintKindApiKeyClient = "api_key_client"
	MintKindDeviceAdopt  = "device_adopt"
	MintKindLegacy       = "legacy"
)

// The class a live session is indexed by, from the kind it was registered with
// and whether a network credential (one without a client) was ever minted
// with it. A legacy lineage that only client credentials carry is a
// per-client lineage; the first network credential minted with it makes it a
// sign-in. sessionLuaCommon's classify is the same function.
func sessionClass(kind string, network bool) string {
	switch kind {
	case MintKindAuthCode:
		return SessionKindCode
	case MintKindLegacyProxy, MintKindHostedProxy:
		return ClientKindHostedProxy
	case MintKindApiKeyClient:
		return ClientKindApiKey
	case MintKindDeviceAdopt:
		return ClientKindDevice
	case MintKindLegacy:
		if !network {
			return ClientKindLegacy
		}
	}
	return SessionKindSignin
}

// The R-kind and client kind of a class.
func SessionKinds(class string) (sessionKind string, clientKind *string) {
	switch class {
	case SessionKindSignin, SessionKindCode:
		return class, nil
	case ClientKindHostedProxy, ClientKindApiKey, ClientKindDevice, ClientKindLegacy:
		kind := class
		return SessionKindClient, &kind
	}
	return SessionKindSignin, nil
}

// The kind label of the cap metrics: the R-kind of a class.
func sessionKindLabel(class string) string {
	kind, _ := SessionKinds(class)
	return kind
}

// The session cap: pro.yml max_sessions and enforce_session_cap (owner
// decision 6), and the retained marker cap.
type SessionCapPolicy struct {
	// The most live sessions of a network the cap counts: every live session
	// except the client sessions whose client provides publicly. Zero or less
	// is no cap.
	Live int
	// The most retained revocation markers of a network: revoked sessions
	// whose horizon has not ended. Zero or less is no cap.
	Retained int
	// While false no mint is refused at either cap; a mint that would have
	// been is admitted and counted (urnetwork_session_cap_would_refuse_total).
	Enforce bool
}

// Without a policy source (model registers pro.yml's) there is no live cap,
// and nothing is enforced.
func defaultSessionCapPolicy() SessionCapPolicy {
	return SessionCapPolicy{Retained: NetworkSessionRetainedLimit}
}

var sessionCapPolicySource atomic.Pointer[func() SessionCapPolicy]
var sessionCapPolicyOverride atomic.Pointer[SessionCapPolicy]

// SetSessionCapPolicySource sets where every mint reads the cap from: model
// registers pro.yml's at init. session cannot import model.
func SetSessionCapPolicySource(source func() SessionCapPolicy) {
	sessionCapPolicySource.Store(&source)
}

func currentSessionCapPolicy() SessionCapPolicy {
	if policy := sessionCapPolicyOverride.Load(); policy != nil {
		return *policy
	}
	if source := sessionCapPolicySource.Load(); source != nil {
		return (*source)()
	}
	return defaultSessionCapPolicy()
}

// Testing_SetSessionCapPolicy replaces the cap policy until the returned
// function restores the previous one.
func Testing_SetSessionCapPolicy(policy SessionCapPolicy) func() {
	old := sessionCapPolicyOverride.Swap(&policy)
	return func() { sessionCapPolicyOverride.Store(old) }
}

// Whether a client provides publicly (model's one provider predicate,
// ClientProvidesPubliclyInTx). A top-level client's own session is excluded
// from the session cap while it does.
type ClientProvideEnabledInTx func(ctx context.Context, tx server.PgTx, clientId server.Id) (bool, error)

var clientProvideEnabledInTx atomic.Pointer[ClientProvideEnabledInTx]

// SetClientProvideEnabledInTx sets the provider predicate mints read. model
// registers it at init; without one no session is provider-exempt.
func SetClientProvideEnabledInTx(predicate ClientProvideEnabledInTx) {
	clientProvideEnabledInTx.Store(&predicate)
}

// The exemption a mint asks for (the mint script's exempt argument): the
// top-level client minting its own session decides it; any other mint keeps
// what the session has.
const (
	sessionExemptKeep = "keep"
	sessionExemptYes  = "1"
	sessionExemptNo   = "0"
)

// The exemption a client credential's mint sets, read in its transaction. Only
// the top-level client decides: a child carries its root's session.
func sessionMintExemption(ctx context.Context, tx server.PgTx, credential *ByJwt) (string, error) {
	if credential.ClientId == nil || credential.RootClientId == nil || *credential.ClientId != *credential.RootClientId {
		return sessionExemptKeep, nil
	}
	predicate := clientProvideEnabledInTx.Load()
	if predicate == nil {
		return sessionExemptNo, nil
	}
	enabled, err := (*predicate)(ctx, tx, *credential.ClientId)
	if err != nil {
		return "", err
	}
	if enabled {
		return sessionExemptYes, nil
	}
	return sessionExemptNo, nil
}

// Counts a mint the cap refused, or would have.
func observeSessionCap(refused bool, class string, capacity string) {
	if capacity == "" {
		return
	}
	if refused {
		sessionCapRefused.WithLabelValues(sessionKindLabel(class), capacity).Inc()
	} else {
		sessionCapWouldRefuse.WithLabelValues(sessionKindLabel(class), capacity).Inc()
	}
}

// The cap a refusal code names.
func sessionCapOfRefusal(err error) string {
	var refusal *SessionError
	if errors.As(err, &refusal) {
		switch refusal.Code {
		case "session_limit_reached":
			return "live"
		case "session_retention_limit_reached":
			return "retained"
		}
	}
	return ""
}

// Frees client sessions from the live set without a marker (R-free). A
// session is freed only when it is live, unrevoked and of a client class, and
// no network credential was ever minted with it: every credential it has is
// then a client credential, and the caller has deactivated every client bound
// to it, so the client-active check refuses all of them. A sign-in or code
// session is never freed here; nor is a session whose metadata is gone, whose
// credentials cannot be told apart. KEYS: the shared keys, then s:, u: and r:
// of each session; ARGV[3] the count, then the sessions.
const sessionFreeLua = sessionLuaCommon + `
prune();local freed=0
for i=1,tonumber(ARGV[3]) do
 local sid=ARGV[3+i];local base=SHARED+(i-1)*3
 local class=redis.call('HGET',KEYS[base+1],'class')
 if redis.call('EXISTS',KEYS[base+3])==0 and clientclass[class] and redis.call('HGET',KEYS[base+1],'network')~='1' then
  if redis.call('ZREM',KEYS[1],sid)==1 then freed=freed+1 end
  redis.call('ZREM',KEYS[2],sid);unindex(sid);redis.call('DEL',KEYS[base+1],KEYS[base+2])
 end
end
if freed>0 then changed() end
return cjson.encode({freed=freed})
`

// The most sessions one free script handles; more go in further scripts.
const sessionFreeBatch = 256

// FreeClientSessions frees the network's client sessions the caller's
// committed removal left without an active client (sessionFreeLua), and counts
// them by removal path. It takes no lifecycle lock and writes no journal: a
// free that is lost leaves the session listed until its horizon, never an
// unlisted credential. Returns how many it freed.
func FreeClientSessions(ctx context.Context, networkId server.Id, sessionIds []server.Id, path string) (int, error) {
	freed := 0
	for start := 0; start < len(sessionIds); start += sessionFreeBatch {
		batch := sessionIds[start:min(len(sessionIds), start+sessionFreeBatch)]
		keys := sessionSharedKeys(networkId)
		args := []any{server.NowUtc().UnixMilli(), server.NewId().String(), len(batch)}
		for _, sid := range batch {
			keys = append(keys, sessionMemberKeys(networkId, sid)...)
			args = append(args, sid.String())
		}
		var result struct {
			Freed int `json:"freed"`
		}
		if err := sessionEval(ctx, sessionFreeLua, keys, args, &result); err != nil {
			return freed, err
		}
		freed += result.Freed
	}
	if 0 < freed {
		sessionFreedOnRemoval.WithLabelValues(path).Add(float64(freed))
	}
	return freed, nil
}

// Sets whether a live client session is provider-exempt from the cap. Only a
// live, unrevoked client session changes. KEYS: the shared keys, then s: and
// r: of the session; ARGV[3] the session, ARGV[4] '1' exempt or '0'.
const sessionExemptLua = sessionLuaCommon + `
local sid=ARGV[3];local S,R=KEYS[SHARED+1],KEYS[SHARED+2]
local accept=redis.call('ZSCORE',KEYS[1],sid)
if not accept or tonumber(accept)<=now or redis.call('EXISTS',R)==1 then return '{"changed":0}' end
if not clientclass[redis.call('HGET',S,'class')] or redis.call('HGET',S,'network')=='1' then return '{"changed":0}' end
local updated=0
if ARGV[4]=='1' then updated=redis.call('ZADD',KEYS[5],accept,sid) else updated=redis.call('ZREM',KEYS[5],sid) end
local maximum=redis.call('ZREVRANGE',KEYS[2],0,0,'WITHSCORES')
extend(KEYS[5],tonumber(maximum[2]) or tonumber(accept))
return '{"changed":'..updated..'}'
`

// UpdateSessionProvideExemption records a top-level client's change of
// provide state on its own client session (the provider exemption a mint sets
// otherwise only at the session's next mint). Best effort: a failure leaves
// the exemption to the session's next mint.
func UpdateSessionProvideExemption(ctx context.Context, networkId, sessionId server.Id, exempt bool) error {
	value := sessionExemptNo
	if exempt {
		value = sessionExemptYes
	}
	keys := append(sessionSharedKeys(networkId), SessionKey(networkId, "s:"+sessionId.String()), SessionMarkerKey(networkId, sessionId))
	return sessionEval(ctx, sessionExemptLua, keys, []any{server.NowUtc().UnixMilli(), server.NewId().String(), sessionId.String(), value}, nil)
}

// How many networks one pipeline of a sample pass reads.
const sessionLevelPipeline = 512

// SampleSessionLevels is one pass of the session level sample: every network
// in the 32 maintenance index shards (session_maintenance.go), which hold
// every network with session state, that has a live set is observed once in
// urnetwork_session_live_per_network with the live sessions the cap counts
// (live minus provider-exempt). Each shard's index is read whole in one
// command, a snapshot no concurrent review can shift, and a network is in one
// shard only, so a pass observes each network at most once. It only reads:
// no script, no prune, no ttl. A pass the context ends is partial: the
// networks it read were observed once and the rest not at all. Returns the
// networks observed.
func SampleSessionLevels(ctx context.Context, now time.Time) (observed int, complete bool, returnErr error) {
	defer func() {
		outcome := "complete"
		if !complete {
			outcome = "partial"
		}
		sessionLiveSamplePasses.WithLabelValues(outcome).Inc()
	}()
	liveFrom := "(" + strconv.FormatInt(now.UnixMilli(), 10)
	for shard := 0; shard < SessionIndexShards; shard++ {
		indexKey := fmt.Sprintf("{nsi_%02d}z", shard)
		var members []string
		if err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			var err error
			members, err = r.ZRange(ctx, indexKey, 0, -1).Result()
			return err
		}); err != nil {
			return observed, false, err
		}
		for start := 0; start < len(members); start += sessionLevelPipeline {
			batch := members[start:min(len(members), start+sessionLevelPipeline)]
			networkIds := make([]server.Id, 0, len(batch))
			for _, member := range batch {
				if networkId, err := server.ParseId(member); err == nil {
					networkIds = append(networkIds, networkId)
				}
			}
			var live, provider []*redis.IntCmd
			if err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
				pipeline := r.Pipeline()
				live, provider = make([]*redis.IntCmd, len(networkIds)), make([]*redis.IntCmd, len(networkIds))
				for i, networkId := range networkIds {
					live[i] = pipeline.ZCount(ctx, SessionKey(networkId, "z"), liveFrom, "+inf")
					provider[i] = pipeline.ZCount(ctx, SessionKey(networkId, "xz"), liveFrom, "+inf")
				}
				_, err := pipeline.Exec(ctx)
				if errors.Is(err, server.RedisNil) {
					err = nil
				}
				return err
			}); err != nil {
				return observed, false, err
			}
			for i := range networkIds {
				count := live[i].Val()
				if count <= 0 {
					continue
				}
				sessionLivePerNetwork.Observe(float64(max(0, count-provider[i].Val())))
				observed++
			}
		}
	}
	return observed, true, nil
}
