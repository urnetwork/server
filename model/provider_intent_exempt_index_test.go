// The provider intent exempt index against the walk it replaces. Randomized
// histories of registry and intent writes go through the production writers,
// and every write is followed by both counts, in one script, at the expiry
// boundaries of the state and with and without a skipped member.
package model

import (
	"context"
	"fmt"
	mathrand "math/rand/v2"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// count_clients as of 9c80fdb79, before the exempt index, verbatim but for its
// name: the oracle every index count must equal.
const testingProviderIntentWalkOracleLua = `
local function oracle_count_clients(pi_key, present_key, expire_key, connected_key, proxy_key, provider_key, live_min, skip_member)
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
						local status = string.match(record, '^(%S+)')
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
`

// Advances the index build one step (ARGV[1] build limit, ARGV[2] key ttl),
// then returns for each (live_min, skip_member) pair of ARGV[3:] the oracle's
// normal and exempt counts and, when the index is complete, the index's (else
// -1 and -1, and -2 and -2 when the index disagrees with its sources), and
// last 1 when the index is complete.
var testingProviderIntentExemptCheckLua = fmt.Sprintf(`%s
%s
local ready = build_exempt_index(KEYS[7], KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[6], ARGV[1], ARGV[2])
local results = {}
for i = 3, #ARGV, 2 do
	local oracle_normal, oracle_exempt = oracle_count_clients(KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5], KEYS[6], ARGV[i], ARGV[i + 1])
	local index_normal, index_exempt = -1, -1
	if ready then
		index_normal, index_exempt = count_clients_indexed(KEYS[7], KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5], KEYS[6], ARGV[i], ARGV[i + 1])
		if index_normal == false then
			index_normal, index_exempt = -2, -2
		end
	end
	table.insert(results, oracle_normal)
	table.insert(results, oracle_exempt)
	table.insert(results, index_normal)
	table.insert(results, index_exempt)
end
if ready then
	table.insert(results, 1)
else
	table.insert(results, 0)
end
return results
`, providerIntentCountLua, testingProviderIntentWalkOracleLua)

// One count compared: the oracle's and the index's.
type testingExemptCount struct {
	liveMin      string
	skipMember   string
	oracleNormal int64
	oracleExempt int64
	indexNormal  int64
	indexExempt  int64
}

// A randomized history of one network's registry and provider intent writes,
// each checked against the oracle. Not safe for concurrent use.
type testingExemptHistory struct {
	t         testing.TB
	ctx       context.Context
	networkId server.Id
	random    *mathrand.Rand
	// members the history writes through the production writers
	clientIds []server.Id
	// the resident of each member's latest connected registration
	residentIds map[server.Id]server.Id
	// the build limit of the checks; production counts use theirs
	buildLimit int
	// live_min samples per check
	liveMinSampleCount int
	// the member the latest write touched, whose own boundaries every check
	// compares
	focusClientId *server.Id

	checkCount        int
	indexedCheckCount int
	comparedCount     int
}

// A history of `memberCount` members on a new network, seeded for replay.
func newTestingExemptHistory(t testing.TB, ctx context.Context, seed uint64, memberCount int, buildLimit int) *testingExemptHistory {
	history := &testingExemptHistory{
		t:                  t,
		ctx:                ctx,
		networkId:          server.NewId(),
		random:             mathrand.New(mathrand.NewPCG(seed, seed^0x5bd1e995)),
		residentIds:        map[server.Id]server.Id{},
		buildLimit:         buildLimit,
		liveMinSampleCount: 10,
	}
	for range memberCount {
		history.clientIds = append(history.clientIds, server.NewId())
	}
	return history
}

// One of the history's members, which becomes the focus of the next check.
func (self *testingExemptHistory) randomClientId() server.Id {
	clientId := self.clientIds[self.random.IntN(len(self.clientIds))]
	self.focusClientId = &clientId
	return clientId
}

// A duration in [minDuration, maxDuration], in whole milliseconds.
func (self *testingExemptHistory) randomDuration(minDuration time.Duration, maxDuration time.Duration) time.Duration {
	span := (maxDuration - minDuration).Milliseconds()
	return minDuration + time.Duration(self.random.Int64N(span+1))*time.Millisecond
}

// Expiries from a few milliseconds, which lapse within the history, to minutes.
func (self *testingExemptHistory) randomTtl() time.Duration {
	if self.random.IntN(3) == 0 {
		return self.randomDuration(time.Millisecond, 20*time.Millisecond)
	}
	return self.randomDuration(time.Second, 10*time.Minute)
}

// A clock for the intent writes: mostly the present, so presences and records
// are live, else hours in the past (records already expired) to an hour ahead
// (entries pruned early).
func (self *testingExemptHistory) randomIntentNow() time.Time {
	if self.random.IntN(4) != 0 {
		return server.NowUtc().Add(self.randomDuration(-time.Minute, time.Minute))
	}
	return server.NowUtc().Add(self.randomDuration(-10*time.Hour, time.Hour))
}

// Any status, exempt or not, with an attempt up to nine hours old, so inside
// or past its allowance.
func (self *testingExemptHistory) randomIntentState(now time.Time) *ProviderIntentState {
	statuses := []ProviderIntentStatus{
		ProviderIntentStatusPending,
		ProviderIntentStatusQualified,
		ProviderIntentStatusNormal,
		ProviderIntentStatusOverLimit,
	}
	attemptTime := now.Add(-self.randomDuration(0, 9*time.Hour))
	return &ProviderIntentState{
		Status:       statuses[self.random.IntN(len(statuses))],
		AttemptTime:  attemptTime,
		GraceEndTime: attemptTime.Add(providerIntentGraceTimeout),
		CheckTime:    now.Add(time.Minute),
	}
}

// Registers (or re-registers) a member as a connected peer.
func (self *testingExemptHistory) addConnected() {
	clientId := self.randomClientId()
	residentId := server.NewId()
	self.residentIds[clientId] = residentId
	addNetworkPeerWithProvideModesLoader(self.ctx, self.networkId, &NetworkPeer{ClientId: clientId}, residentId, self.randomTtl(), func() (map[ProvideMode]bool, error) {
		return map[ProvideMode]bool{}, nil
	})
}

// Extends a member's registration, as the resident heartbeat does.
func (self *testingExemptHistory) refreshConnected() {
	clientId := self.randomClientId()
	if residentId, ok := self.residentIds[clientId]; ok {
		RefreshNetworkPeer(self.ctx, self.networkId, clientId, residentId, self.randomTtl())
	}
}

// Removes a member's registration, as the resident teardown does.
func (self *testingExemptHistory) removeConnected() {
	clientId := self.randomClientId()
	if residentId, ok := self.residentIds[clientId]; ok {
		RemoveNetworkPeer(self.ctx, self.networkId, clientId, residentId)
	}
}

// Moves a member from the connected to the isolated (proxy) registry.
func (self *testingExemptHistory) isolate() {
	isolateNetworkPeer(self.ctx, self.networkId, self.randomClientId())
}

// Negative ttls register an expired install, which the same call prunes.
func (self *testingExemptHistory) addProvider() {
	ttl := self.randomTtl()
	if self.random.IntN(5) == 0 {
		ttl = -ttl
	}
	AddNetworkProviderPeer(self.ctx, self.networkId, self.randomClientId(), ttl)
}

// Removes a member's provider install.
func (self *testingExemptHistory) removeProvider() {
	RemoveNetworkProviderPeer(self.ctx, self.networkId, self.randomClientId())
}

// Writes a random state as the check or a connection would, with or without a
// presence refresh. The write prunes expired records and presences at its
// clock first.
func (self *testingExemptHistory) applyIntent() {
	clientId := self.randomClientId()
	now := self.randomIntentNow()
	presenceExpireTime := time.Time{}
	if self.random.IntN(3) != 0 {
		presenceExpireTime = now.Add(self.randomTtl())
	}
	record, _ := getProviderIntentRecord(self.ctx, self.networkId, clientId, now)
	result := applyProviderIntentState(self.ctx, self.networkId, clientId, record, self.randomIntentState(now), false, 0, presenceExpireTime, now)
	if !result.applied {
		self.t.Fatalf("intent state not applied")
	}
}

// Refreshes a member's intent presence, as an intent connection does.
func (self *testingExemptHistory) observeIntent() {
	now := self.randomIntentNow()
	observeProviderIntentAt(self.ctx, self.networkId, self.randomClientId(), self.randomTtl(), now)
}

// A failed client's slot decision, made by the apply script with its count.
// The decision must match the oracle's count of the state it decided on.
func (self *testingExemptHistory) decideSlot() {
	clientId := self.randomClientId()
	now := server.NowUtc().Add(self.randomDuration(-time.Minute, time.Minute))
	// prune at the decision's clock first, as the decision's own write does,
	// so the oracle sees the state the decision counts. Each write prunes a
	// bounded batch, and a history has fewer expired entries than four.
	for range 4 {
		pruneId := server.NewId()
		pruneRecord, _ := getProviderIntentRecord(self.ctx, self.networkId, pruneId, now)
		applyProviderIntentState(self.ctx, self.networkId, pruneId, pruneRecord, &ProviderIntentState{
			Status:      ProviderIntentStatusNormal,
			AttemptTime: now,
		}, false, 0, time.Time{}, now)
	}

	liveMin := strconv.FormatInt(now.UnixMilli()+1, 10)
	counts, _ := self.counts([]string{liveMin}, []string{string(clientId.Bytes())})
	limit := 1 + self.random.IntN(len(self.clientIds))
	expectOverLimit := int64(limit) <= counts[0].oracleNormal

	normal := &ProviderIntentState{
		Status:      ProviderIntentStatusNormal,
		AttemptTime: now,
		CheckTime:   now.Add(ProviderIntentAttemptAllowance),
	}
	presenceExpireTime := time.Time{}
	if self.random.IntN(2) == 0 {
		presenceExpireTime = now.Add(self.randomTtl())
	}
	record, _ := getProviderIntentRecord(self.ctx, self.networkId, clientId, now)
	result := applyProviderIntentState(self.ctx, self.networkId, clientId, record, normal, true, limit, presenceExpireTime, now)
	if !result.applied {
		self.t.Fatalf("slot decision not applied")
	}
	if result.overLimit != expectOverLimit {
		self.t.Fatalf(
			"slot decision over limit %t with limit %d, oracle normal count %d",
			result.overLimit,
			limit,
			counts[0].oracleNormal,
		)
	}
}

// The production count must match the oracle at its clock.
func (self *testingExemptHistory) countClients() {
	now := server.NowUtc().Add(self.randomDuration(-time.Minute, time.Minute))
	normalCount, providerIntentCount := getNetworkConnectedClientCountsAt(self.ctx, self.networkId, now)
	counts, _ := self.counts([]string{strconv.FormatInt(now.UnixMilli()+1, 10)}, []string{""})
	connect.AssertEqual(self.t, int64(normalCount), counts[0].oracleNormal)
	connect.AssertEqual(self.t, int64(providerIntentCount), counts[0].oracleExempt)
}

// A lost index (eviction, failover): the next count rebuilds it.
func (self *testingExemptHistory) dropIndex() {
	server.Redis(self.ctx, func(r server.RedisClient) {
		err := r.Del(self.ctx, providerIntentExemptKey(self.networkId)).Err()
		server.Raise(err)
	})
}

// State written without the index, as before it existed, including states the
// production writers never produce. The index left behind holds a stale live
// entry and no build state, so the next build must start over.
func (self *testingExemptHistory) writeLegacyState() {
	clientId := self.randomClientId()
	member := string(clientId.Bytes())
	nowMs := server.NowUtc().UnixMilli()
	randomScore := func() float64 {
		return float64(nowMs + self.randomDuration(-time.Hour, time.Hour).Milliseconds())
	}
	records := []string{
		"pending {}",
		"qualified {}",
		"normal {}",
		"over_limit {}",
		"pendingx {}",
		"",
	}
	server.Redis(self.ctx, func(r server.RedisClient) {
		pipe := r.TxPipeline()
		switch self.random.IntN(6) {
		case 0:
			pipe.ZAdd(self.ctx, networkPeerConnectedKey(self.networkId), redis.Z{Score: randomScore(), Member: member})
		case 1:
			pipe.ZAdd(self.ctx, networkPeerConnectedProviderKey(self.networkId), redis.Z{Score: randomScore(), Member: member})
		case 2:
			pipe.ZAdd(self.ctx, providerIntentPresentKey(self.networkId), redis.Z{Score: randomScore(), Member: member})
		case 3:
			pipe.ZAdd(self.ctx, providerIntentExpireKey(self.networkId), redis.Z{Score: randomScore(), Member: member})
		case 4:
			pipe.HSet(self.ctx, providerIntentKey(self.networkId), member, records[self.random.IntN(len(records))])
		default:
			pipe.ZRem(self.ctx, providerIntentPresentKey(self.networkId), member)
		}
		pipe.Del(self.ctx, providerIntentExemptKey(self.networkId))
		pipe.ZAdd(self.ctx, providerIntentExemptKey(self.networkId), redis.Z{
			Score:  float64(nowMs + time.Hour.Milliseconds()),
			Member: "c" + string(server.NewId().Bytes()),
		})
		_, err := pipe.Exec(self.ctx)
		server.Raise(err)
	})
}

// Runs the check script for every (live_min, skip member) pair.
func (self *testingExemptHistory) counts(liveMins []string, skipMembers []string) (counts []testingExemptCount, ready bool) {
	args := []any{self.buildLimit, int64(networkPeerKeyTtl / time.Second)}
	for _, liveMin := range liveMins {
		for _, skipMember := range skipMembers {
			args = append(args, liveMin, skipMember)
		}
	}
	var values []int64
	server.Redis(self.ctx, func(r server.RedisClient) {
		var err error
		values, err = r.Eval(self.ctx, testingProviderIntentExemptCheckLua, providerIntentKeys(self.networkId), args...).Int64Slice()
		server.Raise(err)
	})
	ready = values[len(values)-1] == 1
	i := 0
	for _, liveMin := range liveMins {
		for _, skipMember := range skipMembers {
			counts = append(counts, testingExemptCount{
				liveMin:      liveMin,
				skipMember:   skipMember,
				oracleNormal: values[i],
				oracleExempt: values[i+1],
				indexNormal:  values[i+2],
				indexExempt:  values[i+3],
			})
			i += 4
		}
	}
	return
}

// The scores of every registry, presence and record expiry entry.
func (self *testingExemptHistory) stateScores() (scores []int64, bothMembers []string) {
	server.Redis(self.ctx, func(r server.RedisClient) {
		pipe := r.Pipeline()
		connectedCmd := pipe.ZRangeWithScores(self.ctx, networkPeerConnectedKey(self.networkId), 0, -1)
		providerCmd := pipe.ZRangeWithScores(self.ctx, networkPeerConnectedProviderKey(self.networkId), 0, -1)
		presentCmd := pipe.ZRangeWithScores(self.ctx, providerIntentPresentKey(self.networkId), 0, -1)
		expireCmd := pipe.ZRangeWithScores(self.ctx, providerIntentExpireKey(self.networkId), 0, -1)
		_, err := pipe.Exec(self.ctx)
		if err != nil && err != server.RedisNil {
			panic(err)
		}
		connectedMembers := map[string]bool{}
		for _, z := range connectedCmd.Val() {
			connectedMembers[z.Member.(string)] = true
		}
		for _, z := range providerCmd.Val() {
			if connectedMembers[z.Member.(string)] {
				bothMembers = append(bothMembers, z.Member.(string))
			}
		}
		for _, cmd := range []*redis.ZSliceCmd{connectedCmd, providerCmd, presentCmd, expireCmd} {
			for _, z := range cmd.Val() {
				scores = append(scores, int64(z.Score))
			}
		}
	})
	return
}

// Compares both counts at sampled expiry boundaries of the current state: one
// millisecond either side of and at entry scores, the present, and before and
// after everything, each with no skipped member, a random member and a member
// of both registries.
func (self *testingExemptHistory) check() (ready bool) {
	nowMs := server.NowUtc().UnixMilli()
	focusClientId := self.focusClientId
	scores, bothMembers := self.stateScores()
	candidates := []int64{0, nowMs + 1, nowMs + 365*24*time.Hour.Milliseconds()}
	for _, score := range scores {
		candidates = append(candidates, score-1, score, score+1)
	}
	liveMins := []string{strconv.FormatInt(nowMs+1, 10)}
	for range self.liveMinSampleCount {
		liveMins = append(liveMins, strconv.FormatInt(candidates[self.random.IntN(len(candidates))], 10))
	}
	skipMembers := []string{"", string(self.clientIds[self.random.IntN(len(self.clientIds))].Bytes())}
	if 0 < len(bothMembers) {
		skipMembers = append(skipMembers, bothMembers[self.random.IntN(len(bothMembers))])
	}
	if focusClientId != nil {
		// every boundary of the written member's entries, index entries included
		for _, score := range self.memberScores(*focusClientId) {
			liveMins = append(liveMins, strconv.FormatInt(score-1, 10), strconv.FormatInt(score, 10), strconv.FormatInt(score+1, 10))
		}
		skipMembers = append(skipMembers, string(focusClientId.Bytes()))
	}

	counts, ready := self.counts(liveMins, skipMembers)
	self.checkCount += 1
	if !ready {
		return
	}
	self.indexedCheckCount += 1
	for _, count := range counts {
		self.comparedCount += 1
		if count.indexNormal == -2 {
			// production writers alone never leave the index behind its sources
			self.t.Fatalf("live_min %s skip %x: the index disagreed with its sources", count.liveMin, count.skipMember)
		}
		if count.indexNormal != count.oracleNormal || count.indexExempt != count.oracleExempt {
			self.t.Fatalf(
				"live_min %s skip %x: index normal %d exempt %d, walk normal %d exempt %d",
				count.liveMin,
				count.skipMember,
				count.indexNormal,
				count.indexExempt,
				count.oracleNormal,
				count.oracleExempt,
			)
		}
	}
	return
}

// The scores of one member's registry, presence, record expiry and index
// entries.
func (self *testingExemptHistory) memberScores(clientId server.Id) (scores []int64) {
	member := string(clientId.Bytes())
	server.Redis(self.ctx, func(r server.RedisClient) {
		pipe := r.Pipeline()
		cmds := []*redis.FloatCmd{
			pipe.ZScore(self.ctx, networkPeerConnectedKey(self.networkId), member),
			pipe.ZScore(self.ctx, networkPeerConnectedProviderKey(self.networkId), member),
			pipe.ZScore(self.ctx, providerIntentPresentKey(self.networkId), member),
			pipe.ZScore(self.ctx, providerIntentExpireKey(self.networkId), member),
			pipe.ZScore(self.ctx, providerIntentExemptKey(self.networkId), "c"+member),
			pipe.ZScore(self.ctx, providerIntentExemptKey(self.networkId), "p"+member),
		}
		_, err := pipe.Exec(self.ctx)
		if err != nil && err != server.RedisNil {
			panic(err)
		}
		for _, cmd := range cmds {
			if score, err := cmd.Result(); err == nil {
				scores = append(scores, int64(score))
			}
		}
	})
	return
}

// Runs `writeCount` random writes, weighted by `weights` in the order of the
// writes below, with a check after each.
func (self *testingExemptHistory) run(writeCount int, weights []int) {
	writes := []func(){
		self.addConnected,
		self.refreshConnected,
		self.removeConnected,
		self.isolate,
		self.addProvider,
		self.removeProvider,
		self.applyIntent,
		self.observeIntent,
		self.decideSlot,
		self.countClients,
		self.dropIndex,
		self.writeLegacyState,
	}
	if len(weights) != len(writes) {
		self.t.Fatalf("%d weights for %d writes", len(weights), len(writes))
	}
	totalWeight := 0
	for _, weight := range weights {
		totalWeight += weight
	}
	for range writeCount {
		choice := self.random.IntN(totalWeight)
		for i, weight := range weights {
			if choice < weight {
				writes[i]()
				break
			}
			choice -= weight
		}
		self.check()
	}
}

// Randomized histories of a small network (fewer members than a build step,
// so a build completes in one count) through every writer: both counts agree
// at every sampled boundary, skipped member and status.
func TestProviderIntentExemptIndexMatchesWalk(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		for seed := uint64(1); seed <= 4; seed += 1 {
			history := newTestingExemptHistory(t, ctx, seed, 24, providerIntentExemptBuildLimit)
			history.run(400, []int{6, 6, 3, 1, 6, 3, 10, 6, 3, 2, 1, 2})
			t.Logf(
				"seed %d: %d checks, %d with the index complete, %d counts compared",
				seed,
				history.checkCount,
				history.indexedCheckCount,
				history.comparedCount,
			)
			if history.indexedCheckCount < history.checkCount*3/4 {
				t.Fatalf("seed %d: the index was complete for only %d of %d checks", seed, history.indexedCheckCount, history.checkCount)
			}
		}
	})
}

// A large network whose registries were written before the index existed,
// with a run of equal expiries longer than a build step: the build takes
// several counts while the production writers interleave with it, then both
// counts agree. The index is dropped from time to time so builds restart under
// writes.
func TestProviderIntentExemptIndexMatchesWalkWhileBuilding(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		history := newTestingExemptHistory(t, ctx, 7, 24, 1)
		history.liveMinSampleCount = 4

		// legacy members the history never registers itself, enough that each
		// registry sweep takes several steps
		nowMs := server.NowUtc().UnixMilli()
		random := mathrand.New(mathrand.NewPCG(11, 13))
		legacyIds := []server.Id{}
		server.Redis(ctx, func(r server.RedisClient) {
			pipe := r.TxPipeline()
			for i := range 600 {
				clientId := server.NewId()
				legacyIds = append(legacyIds, clientId)
				member := string(clientId.Bytes())
				registryKey := networkPeerConnectedKey(history.networkId)
				if i%2 == 1 {
					registryKey = networkPeerConnectedProviderKey(history.networkId)
				}
				// some lapse within the test, some lapsed already
				expiryMs := nowMs + random.Int64N(20*time.Minute.Milliseconds()) - 2*time.Minute.Milliseconds()
				pipe.ZAdd(ctx, registryKey, redis.Z{Score: float64(expiryMs), Member: member})
				if random.IntN(3) != 0 {
					pipe.ZAdd(ctx, providerIntentPresentKey(history.networkId), redis.Z{
						Score:  float64(nowMs + random.Int64N(20*time.Minute.Milliseconds()) - 2*time.Minute.Milliseconds()),
						Member: member,
					})
					pipe.ZAdd(ctx, providerIntentExpireKey(history.networkId), redis.Z{
						Score:  float64(nowMs + random.Int64N(8*time.Hour.Milliseconds()) - time.Minute.Milliseconds()),
						Member: member,
					})
					records := []string{"pending {}", "qualified {}", "normal {}", "over_limit {}"}
					pipe.HSet(ctx, providerIntentKey(history.networkId), member, records[random.IntN(len(records))])
				}
			}
			_, err := pipe.Exec(ctx)
			server.Raise(err)
		})
		// a run of legacy registrations sharing one expiry, longer than a build
		// step, so a step must take the run whole
		runExpiryMs := nowMs + 15*time.Minute.Milliseconds()
		server.Redis(ctx, func(r server.RedisClient) {
			pipe := r.TxPipeline()
			for range 300 {
				clientId := server.NewId()
				legacyIds = append(legacyIds, clientId)
				member := string(clientId.Bytes())
				pipe.ZAdd(ctx, networkPeerConnectedKey(history.networkId), redis.Z{Score: float64(runExpiryMs), Member: member})
				pipe.ZAdd(ctx, providerIntentPresentKey(history.networkId), redis.Z{Score: float64(nowMs + time.Hour.Milliseconds()), Member: member})
				pipe.ZAdd(ctx, providerIntentExpireKey(history.networkId), redis.Z{Score: float64(nowMs + time.Hour.Milliseconds()), Member: member})
				pipe.HSet(ctx, providerIntentKey(history.networkId), member, "pending {}")
			}
			_, err := pipe.Exec(ctx)
			server.Raise(err)
		})
		// the history's intent writes and installs also reach legacy members,
		// the run's included
		history.clientIds = append(history.clientIds, legacyIds[:48]...)
		history.clientIds = append(history.clientIds, legacyIds[len(legacyIds)-24:]...)

		history.run(240, []int{6, 6, 3, 1, 6, 3, 10, 6, 1, 1, 2, 0})
		t.Logf(
			"%d checks, %d with the index complete, %d counts compared",
			history.checkCount,
			history.indexedCheckCount,
			history.comparedCount,
		)
		if history.indexedCheckCount == 0 || history.indexedCheckCount == history.checkCount {
			t.Fatalf("builds never interleaved with writes: %d of %d checks complete", history.indexedCheckCount, history.checkCount)
		}
	})
}

// The build state of a network's index: "" when none, else its sentinel.
func testingProviderIntentExemptBuildState(ctx context.Context, networkId server.Id) (state string) {
	server.Redis(ctx, func(r server.RedisClient) {
		states, err := r.ZRangeByScore(ctx, providerIntentExemptKey(networkId), &redis.ZRangeBy{
			Min:   "-inf",
			Max:   "-inf",
			Count: 1,
		}).Result()
		server.Raise(err)
		if 0 < len(states) {
			state = states[0]
		}
	})
	return
}

// A network with no index (its state predates it) is counted exactly from the
// first count. Each count derives a bounded share of the registry members, a
// quarter of them or the build limit, so the build takes a handful of counts
// that walk; the counts after it read the index. Each count is counted by its
// path.
func TestProviderIntentExemptIndexBuildsLazilyInBoundedSteps(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		now := server.NowUtc()
		nowMs := now.UnixMilli()

		connectedCount := 3 * providerIntentExemptBuildLimit
		providerCount := providerIntentExemptBuildLimit
		exemptCount := 0
		server.Redis(ctx, func(r server.RedisClient) {
			pipe := r.TxPipeline()
			for i := range connectedCount + providerCount {
				member := string(server.NewId().Bytes())
				registryKey := networkPeerConnectedKey(networkId)
				if connectedCount <= i {
					registryKey = networkPeerConnectedProviderKey(networkId)
				}
				// distinct expiries, as registrations written over time have
				pipe.ZAdd(ctx, registryKey, redis.Z{Score: float64(nowMs + time.Hour.Milliseconds() + int64(i)), Member: member})
				if i%3 == 0 {
					pipe.ZAdd(ctx, providerIntentPresentKey(networkId), redis.Z{Score: float64(nowMs + time.Hour.Milliseconds()), Member: member})
					pipe.ZAdd(ctx, providerIntentExpireKey(networkId), redis.Z{Score: float64(nowMs + time.Hour.Milliseconds()), Member: member})
					pipe.HSet(ctx, providerIntentKey(networkId), member, "qualified {}")
					exemptCount += 1
				}
			}
			_, err := pipe.Exec(ctx)
			server.Raise(err)
		})
		memberCount := connectedCount + providerCount

		walkCounter := providerIntentCountsCounter.WithLabelValues(providerIntentCountPathWalk)
		indexCounter := providerIntentCountsCounter.WithLabelValues(providerIntentCountPathIndex)
		walksBefore := testutil.ToFloat64(walkCounter)
		indexedBefore := testutil.ToFloat64(indexCounter)

		counts := 0
		for {
			before := testingRedisCommandsExecuted(t, ctx)
			normalCount, providerIntentCount := getNetworkConnectedClientCountsAt(ctx, networkId, now)
			commands := testingRedisCommandsExecuted(t, ctx) - before
			counts += 1
			connect.AssertEqual(t, normalCount, memberCount-exemptCount)
			connect.AssertEqual(t, providerIntentCount, exemptCount)
			state := testingProviderIntentExemptBuildState(ctx, networkId)
			t.Logf("count %d: %d commands, build state %q", counts, commands, state)
			// a count derives at most the build limit (with distinct expiries no
			// run of equal scores adds to it), at most six commands each, beside
			// the walk of at most three per member
			if int64(3*memberCount+6*providerIntentExemptBuildLimit+64) < commands {
				t.Fatalf("count %d took %d commands", counts, commands)
			}
			if state == "#ready" {
				break
			}
			if counts == 1 && state == "" {
				t.Fatalf("the first count left no build state")
			}
			if 8 < counts {
				t.Fatalf("the build did not complete in %d counts", counts)
			}
		}
		// the first count did not build everything at once
		if counts < 2 {
			t.Fatalf("the build of %d members completed in %d count", memberCount, counts)
		}
		// every count before the last walked; the last completed the index and
		// read it
		connect.AssertEqual(t, testutil.ToFloat64(walkCounter)-walksBefore, float64(counts-1))
		connect.AssertEqual(t, testutil.ToFloat64(indexCounter)-indexedBefore, float64(1))

		indexedCommands := testingFewestRedisCommands(t, ctx, 4, func() {
			normalCount, providerIntentCount := getNetworkConnectedClientCountsAt(ctx, networkId, now)
			connect.AssertEqual(t, normalCount, memberCount-exemptCount)
			connect.AssertEqual(t, providerIntentCount, exemptCount)
		})
		if 16 < indexedCommands {
			t.Fatalf("a count with the index complete took %d commands", indexedCommands)
		}
		connect.AssertEqual(t, testutil.ToFloat64(walkCounter)-walksBefore, float64(counts-1))
		connect.AssertEqual(t, testutil.ToFloat64(indexCounter)-indexedBefore, float64(1+4))
	})
}

// Every key of the exempt index shares the network's peer hash tag, so each
// script that maintains it stays in one cluster slot.
func TestProviderIntentExemptKeysShareNetworkSlot(t *testing.T) {
	networkId := server.NewId()
	tag := fmt.Sprintf("{np_%s}", networkId)
	keys := slices.Concat(providerIntentKeys(networkId), []string{
		networkPeerMetaKey(networkId),
		networkPeerDisconnectedKey(networkId),
		networkPeerEventIdKey(networkId),
	})
	for _, key := range keys {
		if len(key) < len(tag) || key[:len(tag)] != tag {
			t.Fatalf("key %s outside the network's hash tag", key)
		}
	}
	connect.AssertEqual(t, slices.Contains(providerIntentKeys(networkId), providerIntentExemptKey(networkId)), true)
}

// One exempt member, connected and installed, pending with a live presence, is
// written by one writer at a time. Both counts agree at every boundary of the
// member's entries before and after the write, with and without the member
// skipped. Each case moves one of the member's entries or the first of its
// expiries, so a writer that left the index behind would fail its case.
func TestProviderIntentExemptIndexFollowsEachWriter(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		noProvideModes := func() (map[ProvideMode]bool, error) {
			return map[ProvideMode]bool{}, nil
		}
		pending := func(attemptTime time.Time) *ProviderIntentState {
			return &ProviderIntentState{
				Status:       ProviderIntentStatusPending,
				AttemptTime:  attemptTime,
				GraceEndTime: attemptTime.Add(providerIntentGraceTimeout),
				CheckTime:    attemptTime.Add(providerIntentGraceTimeout),
			}
		}
		// writes a state at `now`, pruning expired records and presences first
		apply := func(history *testingExemptHistory, clientId server.Id, state *ProviderIntentState, presenceExpireTime time.Time, now time.Time) {
			record, _ := getProviderIntentRecord(ctx, history.networkId, clientId, now)
			result := applyProviderIntentState(ctx, history.networkId, clientId, record, state, false, 0, presenceExpireTime, now)
			if !result.applied {
				t.Fatalf("intent state not applied")
			}
		}
		type writerCase struct {
			name string
			// the member's expiries before the write
			connectedTtl time.Duration
			providerTtl  time.Duration
			presenceTtl  time.Duration
			write        func(history *testingExemptHistory, clientId server.Id, residentId server.Id, otherId server.Id)
		}
		cases := []writerCase{
			{"refresh", 30 * time.Second, 40 * time.Second, 50 * time.Second, func(history *testingExemptHistory, clientId server.Id, residentId server.Id, otherId server.Id) {
				connect.AssertEqual(t, RefreshNetworkPeer(ctx, history.networkId, clientId, residentId, 2*time.Minute), true)
			}},
			{"re-add", 30 * time.Second, 40 * time.Second, 50 * time.Second, func(history *testingExemptHistory, clientId server.Id, residentId server.Id, otherId server.Id) {
				addNetworkPeerWithProvideModesLoader(ctx, history.networkId, &NetworkPeer{ClientId: clientId}, server.NewId(), 2*time.Minute, noProvideModes)
			}},
			{"remove", 30 * time.Second, 40 * time.Second, 50 * time.Second, func(history *testingExemptHistory, clientId server.Id, residentId server.Id, otherId server.Id) {
				RemoveNetworkPeer(ctx, history.networkId, clientId, residentId)
			}},
			{"isolate", 30 * time.Second, 40 * time.Second, 50 * time.Second, func(history *testingExemptHistory, clientId server.Id, residentId server.Id, otherId server.Id) {
				isolateNetworkPeer(ctx, history.networkId, clientId)
			}},
			{"connected prune", 30 * time.Second, 40 * time.Second, 50 * time.Second, func(history *testingExemptHistory, clientId server.Id, residentId server.Id, otherId server.Id) {
				// the registration lapses, and the next registration prunes it
				connect.AssertEqual(t, RefreshNetworkPeer(ctx, history.networkId, clientId, residentId, time.Millisecond), true)
				select {
				case <-time.After(10 * time.Millisecond):
				}
				addNetworkPeerWithProvideModesLoader(ctx, history.networkId, &NetworkPeer{ClientId: otherId}, server.NewId(), time.Minute, noProvideModes)
			}},
			{"provider refresh", 30 * time.Second, 40 * time.Second, 50 * time.Second, func(history *testingExemptHistory, clientId server.Id, residentId server.Id, otherId server.Id) {
				AddNetworkProviderPeer(ctx, history.networkId, clientId, 2*time.Minute)
			}},
			{"provider prune", 30 * time.Second, 40 * time.Second, 50 * time.Second, func(history *testingExemptHistory, clientId server.Id, residentId server.Id, otherId server.Id) {
				// an expired install is registered and pruned by the same call
				AddNetworkProviderPeer(ctx, history.networkId, clientId, -time.Second)
			}},
			{"provider remove", 30 * time.Second, 40 * time.Second, 50 * time.Second, func(history *testingExemptHistory, clientId server.Id, residentId server.Id, otherId server.Id) {
				RemoveNetworkProviderPeer(ctx, history.networkId, clientId)
			}},
			{"failed status", 30 * time.Second, 40 * time.Second, 50 * time.Second, func(history *testingExemptHistory, clientId server.Id, residentId server.Id, otherId server.Id) {
				now := server.NowUtc()
				apply(history, clientId, &ProviderIntentState{
					Status:      ProviderIntentStatusNormal,
					AttemptTime: now,
					CheckTime:   now.Add(ProviderIntentAttemptAllowance),
				}, time.Time{}, now)
			}},
			{"presence by apply", 30 * time.Second, 40 * time.Second, 20 * time.Second, func(history *testingExemptHistory, clientId server.Id, residentId server.Id, otherId server.Id) {
				now := server.NowUtc()
				apply(history, clientId, pending(now), now.Add(10*time.Minute), now)
			}},
			{"observe", 30 * time.Second, 40 * time.Second, 20 * time.Second, func(history *testingExemptHistory, clientId server.Id, residentId server.Id, otherId server.Id) {
				observeProviderIntentAt(ctx, history.networkId, clientId, 10*time.Minute, server.NowUtc())
			}},
			{"record prune", 30 * time.Second, 40 * time.Second, 50 * time.Second, func(history *testingExemptHistory, clientId server.Id, residentId server.Id, otherId server.Id) {
				// a record written nine hours ago has expired; the next write prunes it
				now := server.NowUtc()
				then := now.Add(-9 * time.Hour)
				apply(history, clientId, pending(then), now.Add(50*time.Second), then)
				apply(history, otherId, pending(now), time.Time{}, now)
			}},
			{"presence prune", 30 * time.Second, 40 * time.Second, 50 * time.Second, func(history *testingExemptHistory, clientId server.Id, residentId server.Id, otherId server.Id) {
				// a presence that has lapsed; the next write prunes it
				now := server.NowUtc()
				apply(history, clientId, pending(now), now.Add(-time.Second), now)
				apply(history, otherId, pending(now), time.Time{}, now)
			}},
		}
		for _, writer := range cases {
			history := newTestingExemptHistory(t, ctx, 1, 2, providerIntentExemptBuildLimit)
			clientId := history.clientIds[0]
			otherId := history.clientIds[1]
			residentId := server.NewId()
			addNetworkPeerWithProvideModesLoader(ctx, history.networkId, &NetworkPeer{ClientId: clientId}, residentId, writer.connectedTtl, noProvideModes)
			AddNetworkProviderPeer(ctx, history.networkId, clientId, writer.providerTtl)
			now := server.NowUtc()
			apply(history, clientId, pending(now), now.Add(writer.presenceTtl), now)

			history.focusClientId = &clientId
			if !history.check() {
				t.Fatalf("%s: the index was not complete before the write", writer.name)
			}
			writer.write(history, clientId, residentId, otherId)
			history.focusClientId = &clientId
			if !history.check() {
				t.Fatalf("%s: the index was not complete after the write", writer.name)
			}
		}
	})
}

// The walk's counts at `now`, without a skipped member, read only.
var testingProviderIntentOracleCountLua = fmt.Sprintf(`%s
local normal_count, exempt_count = oracle_count_clients(KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5], KEYS[6], ARGV[1], '')
return {normal_count, exempt_count}
`, testingProviderIntentWalkOracleLua)

func testingProviderIntentOracleCounts(ctx context.Context, networkId server.Id, now time.Time) (normalCount int64, exemptCount int64) {
	server.Redis(ctx, func(r server.RedisClient) {
		values, err := r.Eval(ctx, testingProviderIntentOracleCountLua, providerIntentKeys(networkId), now.UnixMilli()+1).Int64Slice()
		server.Raise(err)
		normalCount = values[0]
		exemptCount = values[1]
	})
	return
}

// A network of three exempt connected members and two exempt installs, written
// by the production writers, with a complete index.
func testingProviderIntentExemptNetwork(t testing.TB, ctx context.Context, now time.Time) (networkId server.Id, connectedIds []server.Id) {
	networkId = server.NewId()
	pending := &ProviderIntentState{
		Status:       ProviderIntentStatusPending,
		AttemptTime:  now,
		GraceEndTime: now.Add(providerIntentGraceTimeout),
		CheckTime:    now.Add(providerIntentGraceTimeout),
	}
	for i := range 5 {
		clientId := server.NewId()
		if i < 3 {
			addNetworkPeerWithProvideModesLoader(ctx, networkId, &NetworkPeer{ClientId: clientId}, server.NewId(), 5*time.Minute, func() (map[ProvideMode]bool, error) {
				return map[ProvideMode]bool{}, nil
			})
			connectedIds = append(connectedIds, clientId)
		} else {
			AddNetworkProviderPeer(ctx, networkId, clientId, 5*time.Minute)
		}
		testingSetProviderIntentState(t, ctx, networkId, clientId, pending, 10*time.Minute, now)
	}
	normalCount, exemptCount := getNetworkConnectedClientCountsAt(ctx, networkId, now)
	connect.AssertEqual(t, normalCount, 0)
	connect.AssertEqual(t, exemptCount, 5)
	connect.AssertEqual(t, testingProviderIntentExemptBuildState(ctx, networkId), "#ready")
	return
}

// A registry or intent key lost while the index is complete (an eviction, a
// DEL), or a registration removed by a writer without the index: the next
// count finds more live exempt entries than live registry entries, or exempt
// entries without the intent keys every exempt entry needs. It drops the
// index, walks, and counts the drop; the count after rebuilds the index from
// what is left and reads it.
func TestProviderIntentExemptIndexDroppedWhenItLosesItsSources(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		lostCounter := providerIntentCountsCounter.WithLabelValues(providerIntentCountPathLost)
		indexCounter := providerIntentCountsCounter.WithLabelValues(providerIntentCountPathIndex)
		losses := []struct {
			name string
			lose func(r server.RedisClient, networkId server.Id, connectedIds []server.Id) error
		}{
			{"connected", func(r server.RedisClient, networkId server.Id, connectedIds []server.Id) error {
				return r.Del(ctx, networkPeerConnectedKey(networkId)).Err()
			}},
			{"provider", func(r server.RedisClient, networkId server.Id, connectedIds []server.Id) error {
				return r.Del(ctx, networkPeerConnectedProviderKey(networkId)).Err()
			}},
			{"pi_present", func(r server.RedisClient, networkId server.Id, connectedIds []server.Id) error {
				return r.Del(ctx, providerIntentPresentKey(networkId)).Err()
			}},
			{"pi_expire", func(r server.RedisClient, networkId server.Id, connectedIds []server.Id) error {
				return r.Del(ctx, providerIntentExpireKey(networkId)).Err()
			}},
			{"pi", func(r server.RedisClient, networkId server.Id, connectedIds []server.Id) error {
				return r.Del(ctx, providerIntentKey(networkId)).Err()
			}},
			// the remove of a writer without the index
			{"remove without the index", func(r server.RedisClient, networkId server.Id, connectedIds []server.Id) error {
				return r.ZRem(ctx, networkPeerConnectedKey(networkId), string(connectedIds[0].Bytes())).Err()
			}},
		}
		for _, loss := range losses {
			now := server.NowUtc()
			networkId, connectedIds := testingProviderIntentExemptNetwork(t, ctx, now)
			server.Redis(ctx, func(r server.RedisClient) {
				err := loss.lose(r, networkId, connectedIds)
				server.Raise(err)
			})
			oracleNormal, oracleExempt := testingProviderIntentOracleCounts(ctx, networkId, now)

			lostBefore := testutil.ToFloat64(lostCounter)
			normalCount, exemptCount := getNetworkConnectedClientCountsAt(ctx, networkId, now)
			t.Logf("lost %s: normal %d exempt %d, walk normal %d exempt %d", loss.name, normalCount, exemptCount, oracleNormal, oracleExempt)
			connect.AssertEqual(t, int64(normalCount), oracleNormal)
			connect.AssertEqual(t, int64(exemptCount), oracleExempt)
			connect.AssertEqual(t, testutil.ToFloat64(lostCounter)-lostBefore, float64(1))
			connect.AssertEqual(t, testingProviderIntentExemptBuildState(ctx, networkId), "")

			indexedBefore := testutil.ToFloat64(indexCounter)
			normalCount, exemptCount = getNetworkConnectedClientCountsAt(ctx, networkId, now)
			connect.AssertEqual(t, int64(normalCount), oracleNormal)
			connect.AssertEqual(t, int64(exemptCount), oracleExempt)
			connect.AssertEqual(t, testutil.ToFloat64(indexCounter)-indexedBefore, float64(1))
			connect.AssertEqual(t, testingProviderIntentExemptBuildState(ctx, networkId), "#ready")
		}
	})
}
