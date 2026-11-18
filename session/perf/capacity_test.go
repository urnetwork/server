package perf

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// REVOKE-OPERATIONS' maximum-state profile, 2026-10-09: 1,000 live sessions
// and 10,000 retained ids, 9,000 of them real markers, used 2,717,332 bytes of
// Redis keys; a complete list took 26.884 ms and the atomic bulk revoke 20.790
// ms. A sequential 1,000-client SQL/Redis validation pass had p50 1.017 ms and
// p99 2.064 ms.
const (
	referenceMaxStateBytes = 2717332
	referenceListMs        = 26.884
	referenceBulkRevokeMs  = 20.790
	referenceCheckP50Ms    = 1.017
	referenceCheckP99Ms    = 2.064
	// every authentication storage call has two seconds (server.RedisAuth),
	// and a list or bulk revoke two seconds for all its attempts
	storageBudgetMs = 2000
)

// The live-session counts the profile measures at: the live cap on main and
// the cap the 2026-11-18 owner decision sets.
const (
	liveCapProfile     = 1000
	retainedCapProfile = 10000
)

func (self *harness) runCapacityPhase() {
	self.capacityLiveCap()
	self.capacityLarge()
}

type capacityNetwork struct {
	network *perfNetwork
	// the sign-in that lists and keeps its own session in a revoke-others
	actor   *credential
	signIns []*credential
}

// Mints sign-ins until the network holds target live sessions or a mint is
// refused, and returns the refusal class.
func (self *harness) fillSignIns(network *capacityNetwork, target int, op string) string {
	var refusal atomic.Value
	var lock sync.Mutex
	need := target - int(self.liveSessionCount(network.network.id))
	runParallel(max(0, need), 16, func(index int) {
		start := time.Now()
		credential, err := self.signIn(network.network)
		self.ops.observe(op, start, err)
		if err != nil {
			refusal.CompareAndSwap(nil, classify(err))
			return
		}
		lock.Lock()
		network.signIns = append(network.signIns, credential)
		lock.Unlock()
	})
	if value := refusal.Load(); value != nil {
		return value.(string)
	}
	return ""
}

func (self *harness) capacityLiveCap() {
	profile := &capacityNetwork{network: self.newNetwork("cap1k")}
	actor, err := self.signIn(profile.network)
	self.must(err, "the profile network's sign-in")
	profile.actor = actor

	self.measure("mint.signin.fill1k", liveCapProfile-1, func() {
		if refusal := self.fillSignIns(profile, liveCapProfile, "capacity.mint.signin"); refusal != "" {
			self.t.Fatalf("filling to the live cap was refused: %s", refusal)
		}
	})
	bytes, keys := self.networkSessionBytes(profile.network.id)
	self.report.addMemory("live session (sign-in, registry keys only)", liveCapProfile, bytes, map[string]int64{"keys": int64(keys)})

	// every session used once through the api: its last-use key and the
	// record's projection of the session
	credentials := append([]*credential{profile.actor}, profile.signIns...)
	recordBefore := self.networkRecordBytes(profile.network.id)
	self.measure("api.auth.first_use", len(credentials), func() {
		runParallel(len(credentials), 16, func(index int) {
			start := time.Now()
			err := self.apiAuth(credentials[index].jwt)
			self.ops.observe("capacity.api.auth.first_use", start, err)
		})
	})
	usedBytes, _ := self.networkSessionBytes(profile.network.id)
	recordAfter := self.networkRecordBytes(profile.network.id)
	self.report.addMemory("live session (sign-in) after first API use: registry + last-use key", liveCapProfile, usedBytes, self.networkKeyBreakdown(profile.network.id))
	self.report.addMemory("revocation record projection per used session", liveCapProfile, recordAfter-recordBefore, nil)

	self.measureListAndHint(profile, "1000")

	// the next new session at the live cap
	start := time.Now()
	_, err = self.signIn(profile.network)
	self.ops.observe("capacity.mint.over_live_cap", start, err)
	self.report.addOutcome(fmt.Sprintf("a new sign-in at %d live sessions", liveCapProfile), outcomeName(err), 0, 0)
	if err == nil {
		// R-dark admits it; keep the network at the profile's count
		self.revokeSingle(profile, 1)
	}

	// Repeated create and revoke cycles until the retained set is full, as
	// REVOKE-FINAL's retained capacity describes: 1,000 live and 9,000 real
	// markers.
	for cycle := 0; ; cycle++ {
		held := self.heldCount(profile.network.id)
		live := self.liveSessionCount(profile.network.id)
		markers := held - live
		if retainedCapProfile-liveCapProfile <= markers {
			break
		}
		if cycle > 20 {
			self.t.Fatalf("the retained profile did not fill: %d held, %d live", held, live)
		}
		need := int64(retainedCapProfile-liveCapProfile) - markers
		if need >= liveCapProfile-1 {
			self.revokeOthers(profile, "capacity.revoke.others@1000", fmt.Sprintf("revoke.others@1000.cycle%d", cycle))
		} else {
			self.revokeSingle(profile, int(need))
		}
		if refusal := self.fillSignIns(profile, liveCapProfile, "capacity.mint.signin"); refusal != "" {
			self.t.Fatalf("refilling the profile network was refused: %s", refusal)
		}
	}
	held, live := self.heldCount(profile.network.id), self.liveSessionCount(profile.network.id)
	breakdown := self.networkKeyBreakdown(profile.network.id)
	referenceKeys := breakdown["z"] + breakdown["held"] + breakdown["generation"] + breakdown["eid"] + breakdown["s:"] + breakdown["r:"]
	breakdown["held#members"] = held
	breakdown["live"] = live
	self.report.addMemory("max-state profile: 1,000 live + 9,000 markers (keys the reference measured)", 1, referenceKeys, breakdown)
	self.report.MaxStateBytes = referenceKeys
	if markerCount := breakdown["r:#count"]; 0 < markerCount {
		heldShare := float64(breakdown["held"]) * float64(markerCount) / float64(max(1, held))
		self.report.addMemory("retained marker (r: key + its held entry)", int(markerCount), breakdown["r:"]+int64(heldShare), nil)
	}
	self.report.addMemory("revoke receipts (op: keys, kept 24 h)", int(breakdown["op:#count"]), breakdown["op:"], nil)

	self.measureListAndHint(profile, "max")

	start = time.Now()
	_, err = self.signIn(profile.network)
	self.ops.observe("capacity.mint.at_retained_cap", start, err)
	self.report.addOutcome(fmt.Sprintf("a new sign-in at %d live and %d held", live, held), outcomeName(err), 0, 0)

	// the bulk revoke at the maximum state
	self.revokeOthers(profile, "capacity.revoke.others@max", "revoke.others@max")
}

func outcomeName(err error) string {
	if err == nil {
		return "accepted"
	}
	return classify(err)
}

func (self *harness) measureListAndHint(network *capacityNetwork, label string) {
	lists := 10
	self.measure("list@"+label, lists, func() {
		for range lists {
			actor := self.credentialSession(network.actor)
			start := time.Now()
			result, err := session.GetNetworkSessions(actor)
			actor.Cancel()
			self.ops.observe("capacity.list@"+label, start, err)
			if err == nil && len(result.Sessions) == 0 {
				self.t.Fatalf("the list of %s is empty", label)
			}
		}
	})
	hints := 50
	self.measure("hint@"+label, hints, func() {
		for range hints {
			start := time.Now()
			_, _, err := session.NetworkSessionRevision(self.ctx, network.network.id)
			self.ops.observe("capacity.hint@"+label, start, err)
		}
	})
}

// Revokes every live session of the network but the actor's, through the
// API's model call.
func (self *harness) revokeOthers(network *capacityNetwork, op string, unitName string) *session.SessionOperationResult {
	var result *session.SessionOperationResult
	live := int(self.liveSessionCount(network.network.id))
	self.must(self.admin.slowlogReset(self.ctx), "slowlog reset")
	self.measure(unitName, live, func() {
		actor := self.credentialSession(network.actor)
		defer actor.Cancel()
		start := time.Now()
		var err error
		result, err = session.RevokeOtherNetworkSessions(&session.RevokeOtherSessionsArgs{OperationId: server.NewId()}, actor)
		self.ops.observe(op, start, err)
		self.must(err, "revoke others")
	})
	self.recordSlowScripts(unitName)
	network.signIns = nil
	return result
}

// Revokes count sign-ins of the network one at a time.
func (self *harness) revokeSingle(network *capacityNetwork, count int) {
	targets := network.signIns
	if len(targets) < count {
		self.t.Fatalf("only %d sign-ins to revoke, %d asked", len(targets), count)
	}
	for _, target := range targets[:count] {
		actor := self.credentialSession(network.actor)
		start := time.Now()
		_, err := session.RevokeNetworkSession(&session.RevokeSessionArgs{SessionId: *target.claims.SessionId, OperationId: server.NewId()}, actor)
		actor.Cancel()
		self.ops.observe("capacity.revoke.single", start, err)
		self.must(err, "revoke single")
	}
	network.signIns = targets[count:]
}

func (self *harness) recordSlowScripts(name string) {
	entries, err := self.admin.slowlog(self.ctx, self.nodeNames)
	self.must(err, "slowlog")
	summary := &slowSummary{Name: name, ByScript: map[string]*slowScript{}}
	for _, entry := range entries {
		key := entry.Command
		if entry.ScriptHint != "" {
			key = "eval:" + entry.ScriptHint
		}
		script := summary.ByScript[key]
		if script == nil {
			script = &slowScript{}
			summary.ByScript[key] = script
		}
		script.Count++
		script.TotalMs += entry.Ms
		if script.MaxMs < entry.Ms {
			script.MaxMs = entry.Ms
			script.MaxKeys = entry.Keys
			script.MaxNode = entry.Node
		}
	}
	self.report.Slow = append(self.report.Slow, summary)
}

// A network with sessions large client-bound sessions: hosted proxy clients,
// each with its own session (legacy_proxy) and a client row bound to it, plus
// the owner's sign-in. The real mint path fills it; when it refuses at the
// live cap, as main does at 1,000, the rest is written with the mint script's
// key layout and bound in SQL, and the report says so.
type largeNetwork struct {
	capacityNetwork
	clients   []*credential
	synthetic int
	refusal   string
}

func (self *harness) largeNetwork(name string, sessions int) *largeNetwork {
	large := &largeNetwork{capacityNetwork: capacityNetwork{network: self.newNetwork(name)}}
	actor, err := self.signIn(large.network)
	self.must(err, "the large network's sign-in")
	large.actor = actor
	type pending struct{ device, client server.Id }
	rows := make([]pending, sessions-1)
	runParallel(len(rows), 16, func(index int) {
		rows[index] = pending{device: server.NewId(), client: server.NewId()}
		model.Testing_CreateDevice(self.ctx, large.network.id, rows[index].device, rows[index].client, "perf hosted", "proxy")
	})
	var refused atomic.Value
	clients := make([]*credential, len(rows))
	self.measure("mint.hosted.new."+name, len(rows), func() {
		runParallel(len(rows), 16, func(index int) {
			if refused.Load() != nil {
				return
			}
			start := time.Now()
			credential, err := self.hostedMint(large.network, rows[index].device, rows[index].client)
			self.ops.observe("capacity.mint.hosted.new", start, err)
			if err != nil {
				refused.CompareAndSwap(nil, classify(err))
				return
			}
			clients[index] = credential
		})
	})
	missing := []int{}
	for index, credential := range clients {
		if credential == nil {
			missing = append(missing, index)
		}
	}
	if value := refused.Load(); value != nil {
		large.refusal = value.(string)
	}
	if 0 < len(missing) {
		if !strings.HasPrefix(large.refusal, "refused:session_limit_reached") {
			self.t.Fatalf("hosted mints failed for %d clients: %s", len(missing), large.refusal)
		}
		self.logf("%s: the mint path refused at %d live sessions (%s); writing %d more with the mint script's key layout", name, self.liveSessionCount(large.network.id), large.refusal, len(missing))
		synthetic := make([]syntheticClient, len(missing))
		for position, index := range missing {
			synthetic[position] = syntheticClient{device: rows[index].device, client: rows[index].client}
		}
		claims := self.registerSynthetic(large.network, synthetic, "legacy_proxy")
		for position, index := range missing {
			clients[index] = &credential{claims: claims[position]}
		}
		large.synthetic = len(missing)
	}
	large.clients = clients
	return large
}

type syntheticClient struct {
	device server.Id
	client server.Id
}

// The keys the mint script writes for a new session, without its capacity
// checks: the live and retained members, the metadata hash and the revision,
// and the client row's binding. The result claims match what MintHostedSession
// would have signed.
const syntheticRegisterLua = `
local now=tonumber(ARGV[1]);local accept=tonumber(ARGV[2]);local retain=tonumber(ARGV[3])
if redis.call('EXISTS',KEYS[3])==0 or redis.call('EXISTS',KEYS[4])==0 then redis.call('SET',KEYS[3],ARGV[8]);redis.call('SET',KEYS[4],1) end
for i=1,#ARGV-8 do
 local sid=ARGV[8+i];local key=KEYS[4+i]
 redis.call('ZADD',KEYS[1],accept,sid);redis.call('ZADD',KEYS[2],retain,sid)
 redis.call('HSET',key,'create_epoch',ARGV[7],'kind',ARGV[5],'create_time',ARGV[6],'origin_session_id','','last_mint_time',now,'token_expire_time',ARGV[4])
 redis.call('PEXPIREAT',key,accept)
end
redis.call('INCR',KEYS[4])
for i=1,4 do local ttl=redis.call('PTTL',KEYS[i]);if ttl==-1 or (ttl>=0 and now+ttl<retain) then redis.call('PEXPIREAT',KEYS[i],retain) end end
return #ARGV-8
`

func (self *harness) registerSynthetic(network *perfNetwork, clients []syntheticClient, kind string) []*session.ByJwt {
	claims := make([]*session.ByJwt, len(clients))
	for index, client := range clients {
		credential := session.NewByJwt(network.id, network.user, network.name, false, false).Client(client.device, client.client)
		sid := server.NewId()
		credential.SessionId = &sid
		root := client.client
		credential.RootClientId = &root
		claims[index] = credential
	}
	for start := 0; start < len(claims); start += 500 {
		batch := claims[start:min(len(claims), start+500)]
		first := batch[0]
		keys := []string{session.SessionKey(network.id, "z"), session.SessionKey(network.id, "held"), session.SessionKey(network.id, "generation"), session.SessionKey(network.id, "eid")}
		args := []any{
			time.Now().UnixMilli(),
			session.DeadlineMillis(first.AcceptUntil()),
			session.DeadlineMillis(first.AcceptUntil().Add(session.SessionRetentionMargin)),
			session.DeadlineMillis(first.ExpiresAt.Time),
			kind,
			first.CreateTime.UTC().Format(time.RFC3339Nano),
			first.CreateTime.UTC().Format("2006-01-02T15:04:05.000000000Z"),
			server.NewId().String(),
		}
		for _, credential := range batch {
			keys = append(keys, session.SessionKey(network.id, "s:"+credential.SessionId.String()))
			args = append(args, credential.SessionId.String())
		}
		self.must(server.RedisAuth(self.ctx, func(ctx context.Context, r server.RedisClient) error {
			return r.Eval(ctx, syntheticRegisterLua, keys, args...).Err()
		}), "synthetic registration")
		server.Tx(self.ctx, func(tx server.PgTx) {
			for _, credential := range batch {
				server.RaisePgResult(tx.Exec(self.ctx, `UPDATE network_client SET session_id=$3,session_create_time=$4 WHERE client_id=$1 AND network_id=$2`, *credential.ClientId, network.id, *credential.SessionId, credential.CreateTime))
			}
		})
	}
	return claims
}

func (self *harness) capacityLarge() {
	sessions := self.config.profile.largeSessions
	large := self.largeNetwork("large", sessions)
	self.large = large
	live := self.liveSessionCount(large.network.id)
	bytes, keys := self.networkSessionBytes(large.network.id)
	self.report.addMemory(fmt.Sprintf("live session (hosted proxy, registry keys only) at %d live", live), int(live), bytes, map[string]int64{"keys": int64(keys), "synthetic": int64(large.synthetic)})
	if large.refusal != "" {
		self.report.addOutcome(fmt.Sprintf("hosted proxy mints toward %d live sessions", sessions), large.refusal, large.synthetic, sessions-1-large.synthetic)
	} else {
		self.report.addOutcome(fmt.Sprintf("hosted proxy mints toward %d live sessions", sessions), "accepted", 0, sessions-1)
	}

	// every client checked once, as connected clients are: the record holds
	// each client's projection
	recordBefore := self.networkRecordBytes(large.network.id)
	self.measure("check.miss.large", len(large.clients), func() {
		runParallel(len(large.clients), 16, func(index int) {
			start := time.Now()
			err := self.leaseCheck(large.clients[index].claims)
			self.ops.observe("capacity.check.miss.large", start, err)
		})
	})
	recordAfter := self.networkRecordBytes(large.network.id)
	self.report.addMemory("revocation record projection per hosted proxy client", len(large.clients), recordAfter-recordBefore, nil)

	self.measureListAndHint(&large.capacityNetwork, fmt.Sprintf("%d", sessions))
}
