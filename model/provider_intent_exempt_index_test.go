// The provider intent exempt index against the walk it replaces. Randomized
// histories of registry and intent writes go through the production writers,
// and every write is followed by both counts, in one script, at the expiry
// boundaries of the state and with and without a skipped member.
package model

import (
	"context"
	"fmt"
	"math"
	mathrand "math/rand/v2"
	"slices"
	"strconv"
	"strings"
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

// Counts the first (live_min, skip_member) pair of ARGV[3:] as production
// does (count_clients, with ARGV[1] build limit and ARGV[2] key ttl): one build
// step, then the index, or the walk while the index builds or after dropping
// it. The other pairs read the index when it is complete and the production
// walk otherwise. Returns for each pair the oracle's normal and exempt counts,
// the counts and the path (1 index, 2 walk, 3 walk after a drop), and last 1
// when the index is complete.
var testingProviderIntentExemptCheckLua = fmt.Sprintf(`%s
%s
local first_normal, first_exempt, first_path = count_clients(KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5], KEYS[6], KEYS[7], ARGV[3], ARGV[4], ARGV[1], ARGV[2])
local ready = redis.call('ZSCORE', KEYS[7], '#ready') ~= false
local results = {}
for i = 3, #ARGV, 2 do
	local oracle_normal, oracle_exempt = oracle_count_clients(KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5], KEYS[6], ARGV[i], ARGV[i + 1])
	local normal, exempt, path = first_normal, first_exempt, first_path
	if 3 < i then
		if ready then
			normal, exempt = count_clients_indexed(KEYS[7], KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5], KEYS[6], ARGV[i], ARGV[i + 1])
			path = 1
			if normal == false then
				normal, exempt, path = -1, -1, 3
			end
		else
			normal, exempt = count_clients_walk(KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5], KEYS[6], ARGV[i], ARGV[i + 1])
			path = 2
		end
	end
	table.insert(results, oracle_normal)
	table.insert(results, oracle_exempt)
	table.insert(results, normal)
	table.insert(results, exempt)
	table.insert(results, path)
end
if ready then
	table.insert(results, 1)
else
	table.insert(results, 0)
end
return results
`, providerIntentCountLua, testingProviderIntentWalkOracleLua)

// One count compared: the oracle's and production's, with production's path.
type testingExemptCount struct {
	liveMin      string
	skipMember   string
	oracleNormal int64
	oracleExempt int64
	normal       int64
	exempt       int64
	path         int64
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
	// checks whose production count walked while the index built
	walkedCheckCount int
	comparedCount    int
	// writes without the index under a complete index, and those after which
	// the production count dropped the index or still equaled the walk
	underIndexWriteCount int
	underIndexLostCount  int
	underIndexExactCount int
	sourceLossCount      int
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

// Writes one registry or intent entry of a member directly, as a process
// without the index does, including states the production writers never
// produce.
func (self *testingExemptHistory) writeWithoutTheIndex(pipe redis.Pipeliner) {
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
	switch self.random.IntN(7) {
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
	case 5:
		pipe.ZRem(self.ctx, providerIntentPresentKey(self.networkId), member)
	default:
		pipe.ZRem(self.ctx, networkPeerConnectedKey(self.networkId), member)
	}
}

// State written without the index, as before it existed. The index left
// behind holds a stale live entry and no build state, so the next build must
// start over.
func (self *testingExemptHistory) writeLegacyState() {
	nowMs := server.NowUtc().UnixMilli()
	server.Redis(self.ctx, func(r server.RedisClient) {
		pipe := r.TxPipeline()
		self.writeWithoutTheIndex(pipe)
		pipe.Del(self.ctx, providerIntentExemptKey(self.networkId))
		pipe.ZAdd(self.ctx, providerIntentExemptKey(self.networkId), redis.Z{
			Score:  float64(nowMs + time.Hour.Milliseconds()),
			Member: "c" + string(server.NewId().Bytes()),
		})
		_, err := pipe.Exec(self.ctx)
		server.Raise(err)
	})
}

// A write by a process without the index while the index stands (a rollout or
// a rollback). The production count right after it either drops the index for
// disagreeing with its sources and walks, or reads it, which can lag the
// write; it never counts a negative normal. The history then ages a complete
// index past providerIntentExemptRebuildAge, so the next count rebuilds it,
// and starts a build in progress over.
func (self *testingExemptHistory) writeUnderTheIndex() {
	ready := testingProviderIntentExemptBuildState(self.ctx, self.networkId) == "#ready"
	server.Redis(self.ctx, func(r server.RedisClient) {
		pipe := r.TxPipeline()
		self.writeWithoutTheIndex(pipe)
		_, err := pipe.Exec(self.ctx)
		server.Raise(err)
	})
	now := server.NowUtc()
	oracleNormal, oracleExempt := testingProviderIntentOracleCounts(self.ctx, self.networkId, now)
	lostCounter := providerIntentCountsCounter.WithLabelValues(providerIntentCountPathLost)
	lostBefore := testutil.ToFloat64(lostCounter)
	normalCount, exemptCount := getNetworkConnectedClientCountsAt(self.ctx, self.networkId, now)
	if normalCount < 0 || exemptCount < 0 {
		self.t.Fatalf("a count under a write without the index: normal %d exempt %d", normalCount, exemptCount)
	}
	if !ready {
		self.dropIndex()
		return
	}
	self.underIndexWriteCount += 1
	if lostBefore < testutil.ToFloat64(lostCounter) {
		self.underIndexLostCount += 1
	}
	if int64(normalCount) == oracleNormal && int64(exemptCount) == oracleExempt {
		self.underIndexExactCount += 1
	}
	if testingProviderIntentExemptBuildState(self.ctx, self.networkId) == "#ready" {
		testingAgeProviderIntentExemptIndex(self.t, self.ctx, self.networkId, providerIntentExemptRebuildAge+time.Minute)
	}
}

// A registry or intent key lost while the index stands (an eviction, a DEL).
// The production count right after it never counts a negative normal, and
// equals the walk when an intent key was lost: every live exempt entry needs
// all three. The history then drops the index, which no longer mirrors its
// sources at every expiry.
func (self *testingExemptHistory) loseSourceKey() {
	keys := []string{
		providerIntentKey(self.networkId),
		providerIntentPresentKey(self.networkId),
		providerIntentExpireKey(self.networkId),
		networkPeerConnectedKey(self.networkId),
		networkPeerConnectedProviderKey(self.networkId),
	}
	lost := self.random.IntN(len(keys))
	server.Redis(self.ctx, func(r server.RedisClient) {
		err := r.Del(self.ctx, keys[lost]).Err()
		server.Raise(err)
	})
	now := server.NowUtc()
	oracleNormal, oracleExempt := testingProviderIntentOracleCounts(self.ctx, self.networkId, now)
	normalCount, exemptCount := getNetworkConnectedClientCountsAt(self.ctx, self.networkId, now)
	if normalCount < 0 || exemptCount < 0 {
		self.t.Fatalf("a count after losing %s: normal %d exempt %d", keys[lost], normalCount, exemptCount)
	}
	if lost < 3 && (int64(normalCount) != oracleNormal || int64(exemptCount) != oracleExempt) {
		self.t.Fatalf("a count after losing %s: normal %d exempt %d, walk normal %d exempt %d", keys[lost], normalCount, exemptCount, oracleNormal, oracleExempt)
	}
	self.sourceLossCount += 1
	self.dropIndex()
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
				normal:       values[i+2],
				exempt:       values[i+3],
				path:         values[i+4],
			})
			i += 5
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
	if ready {
		self.indexedCheckCount += 1
	}
	if counts[0].path == 2 {
		self.walkedCheckCount += 1
	}
	for _, count := range counts {
		self.comparedCount += 1
		if count.path == 3 {
			// production writers alone never leave the index behind its sources
			self.t.Fatalf("live_min %s skip %x: the index disagreed with its sources", count.liveMin, count.skipMember)
		}
		if count.normal != count.oracleNormal || count.exempt != count.oracleExempt {
			self.t.Fatalf(
				"live_min %s skip %x: normal %d exempt %d (path %d), walk normal %d exempt %d",
				count.liveMin,
				count.skipMember,
				count.normal,
				count.exempt,
				count.path,
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
		self.writeUnderTheIndex,
		self.loseSourceKey,
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
// so a build completes in one count) through every writer, with writes by a
// process without the index under a complete index and lost registry and
// intent keys: both counts agree at every sampled boundary, skipped member and
// status.
func TestProviderIntentExemptIndexMatchesWalk(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		for seed := uint64(1); seed <= 4; seed += 1 {
			history := newTestingExemptHistory(t, ctx, seed, 24, providerIntentExemptBuildLimit)
			history.run(400, []int{6, 6, 3, 1, 6, 3, 10, 6, 3, 2, 1, 2, 3, 1})
			t.Logf(
				"seed %d: %d checks, %d with the index complete, %d counts compared; %d writes without the index under it (%d dropped it, %d exact), %d source keys lost",
				seed,
				history.checkCount,
				history.indexedCheckCount,
				history.comparedCount,
				history.underIndexWriteCount,
				history.underIndexLostCount,
				history.underIndexExactCount,
				history.sourceLossCount,
			)
			if history.indexedCheckCount < history.checkCount*3/4 {
				t.Fatalf("seed %d: the index was complete for only %d of %d checks", seed, history.indexedCheckCount, history.checkCount)
			}
			if history.underIndexWriteCount == 0 || history.sourceLossCount == 0 {
				t.Fatalf("seed %d: no write without the index under it or no lost key", seed)
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

		history.run(240, []int{6, 6, 3, 1, 6, 3, 10, 6, 1, 1, 2, 0, 1, 1})
		t.Logf(
			"%d checks, %d with the index complete, %d whose production count walked while it built, %d counts compared",
			history.checkCount,
			history.indexedCheckCount,
			history.walkedCheckCount,
			history.comparedCount,
		)
		if history.indexedCheckCount == 0 || history.indexedCheckCount == history.checkCount {
			t.Fatalf("builds never interleaved with writes: %d of %d checks complete", history.indexedCheckCount, history.checkCount)
		}
		// the checks' build limit is below the member count, so the production
		// count walks while the index builds
		if history.walkedCheckCount == 0 {
			t.Fatalf("the production walk never ran")
		}
	})
}

// The build state of a network's index: "" when none, "#ready" when complete,
// else its build sentinel.
func testingProviderIntentExemptBuildState(ctx context.Context, networkId server.Id) (state string) {
	server.Redis(ctx, func(r server.RedisClient) {
		_, err := r.ZScore(ctx, providerIntentExemptKey(networkId), "#ready").Result()
		if err == nil {
			state = "#ready"
			return
		}
		if err != server.RedisNil {
			panic(err)
		}
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

// Writes a network's registries and intent state as before the index existed:
// three build limits of connected members and one of installs, at distinct
// expiries, a third of them qualified with a live presence. Returns the members
// and the exempt members.
func testingProviderIntentPreIndexNetwork(ctx context.Context, now time.Time) (networkId server.Id, memberCount int, exemptCount int) {
	networkId = server.NewId()
	nowMs := now.UnixMilli()
	connectedCount := 3 * providerIntentExemptBuildLimit
	memberCount = connectedCount + providerIntentExemptBuildLimit
	server.Redis(ctx, func(r server.RedisClient) {
		pipe := r.TxPipeline()
		for i := range memberCount {
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
	return
}

// A network with no index (its state predates it) is counted exactly from the
// first count. Each count derives a bounded share of the registry members, a
// quarter of them or the build limit, so the build takes a handful of counts
// that walk; the counts after it read the index. The commands are the count
// script's own (testingCountedScript). Each production count is counted by its
// path.
func TestProviderIntentExemptIndexBuildsLazilyInBoundedSteps(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()

		networkId, memberCount, exemptCount := testingProviderIntentPreIndexNetwork(ctx, now)
		counts := 0
		for {
			commands, result := testingCountedScript(t, ctx, providerIntentCountClientsLua, providerIntentKeys(networkId), providerIntentCountArgs(now)...)
			counts += 1
			connect.AssertEqual(t, result[0], int64(memberCount-exemptCount))
			connect.AssertEqual(t, result[1], int64(exemptCount))
			state := testingProviderIntentExemptBuildState(ctx, networkId)
			t.Logf("count %d: %d commands, path %d, build state %q", counts, commands, result[2], state)
			// a count derives at most the build limit and two more (with distinct
			// expiries no run of equal scores adds to it), at most seven commands
			// each, beside the walk of at most three per member
			if int64(3*memberCount+7*(providerIntentExemptBuildLimit+2)+64) < commands {
				t.Fatalf("count %d took %d commands", counts, commands)
			}
			if state == "#ready" {
				connect.AssertEqual(t, result[2], int64(1))
				break
			}
			connect.AssertEqual(t, result[2], int64(2))
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
		indexedCommands, result := testingCountedScript(t, ctx, providerIntentCountClientsLua, providerIntentKeys(networkId), providerIntentCountArgs(now)...)
		connect.AssertEqual(t, result[2], int64(1))
		if 13 < indexedCommands {
			t.Fatalf("a count with the index complete took %d commands", indexedCommands)
		}

		// the production count reports its path
		walkCounter := providerIntentCountsCounter.WithLabelValues(providerIntentCountPathWalk)
		indexCounter := providerIntentCountsCounter.WithLabelValues(providerIntentCountPathIndex)
		networkId, memberCount, exemptCount = testingProviderIntentPreIndexNetwork(ctx, now)
		walksBefore := testutil.ToFloat64(walkCounter)
		indexedBefore := testutil.ToFloat64(indexCounter)
		productionCounts := 0
		for testingProviderIntentExemptBuildState(ctx, networkId) != "#ready" {
			normalCount, providerIntentCount := getNetworkConnectedClientCountsAt(ctx, networkId, now)
			productionCounts += 1
			connect.AssertEqual(t, normalCount, memberCount-exemptCount)
			connect.AssertEqual(t, providerIntentCount, exemptCount)
			if 8 < productionCounts {
				t.Fatalf("the build did not complete in %d counts", productionCounts)
			}
		}
		// every count before the last walked; the last completed the index and
		// read it
		connect.AssertEqual(t, productionCounts, counts)
		connect.AssertEqual(t, testutil.ToFloat64(walkCounter)-walksBefore, float64(counts-1))
		connect.AssertEqual(t, testutil.ToFloat64(indexCounter)-indexedBefore, float64(1))
		for range 4 {
			getNetworkConnectedClientCountsAt(ctx, networkId, now)
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

// Sets a complete index's build time, as if it had been built `age` ago on the
// redis clock.
func testingAgeProviderIntentExemptIndex(t testing.TB, ctx context.Context, networkId server.Id, age time.Duration) {
	server.Redis(ctx, func(r server.RedisClient) {
		now, err := r.Time(ctx).Result()
		server.Raise(err)
		added, err := r.ZAddXX(ctx, providerIntentExemptKey(networkId), redis.Z{
			Score:  float64(-now.Add(-age).UnixMilli()),
			Member: "#ready",
		}).Result()
		server.Raise(err)
		connect.AssertEqual(t, added, int64(0))
	})
}

// A writer without the index fails a client (a rollout or a rollback): the
// index still counts the client exempt, and the count cannot see it, since the
// client's registry entry and intent keys are all there. A complete index is
// trusted for providerIntentExemptRebuildAge on the redis clock; past it, the
// next count rebuilds the index and agrees with the walk again. An index built
// before its build time was recorded (`#ready` scored -inf) is rebuilt at once.
func TestProviderIntentExemptIndexRebuiltPastItsAge(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, built := range []string{"aged", "unrecorded"} {
			now := server.NowUtc()
			networkId, connectedIds := testingProviderIntentExemptNetwork(t, ctx, now)
			failedId := connectedIds[0]
			server.Redis(ctx, func(r server.RedisClient) {
				err := r.HSet(ctx, providerIntentKey(networkId), string(failedId.Bytes()), (&ProviderIntentState{
					Status:      ProviderIntentStatusNormal,
					AttemptTime: now,
					CheckTime:   now.Add(ProviderIntentAttemptAllowance),
				}).record()).Err()
				server.Raise(err)
			})
			oracleNormal, oracleExempt := testingProviderIntentOracleCounts(ctx, networkId, now)
			connect.AssertEqual(t, oracleNormal, int64(1))
			connect.AssertEqual(t, oracleExempt, int64(4))

			if built == "aged" {
				// within the age the index is trusted, and lags the write
				testingAgeProviderIntentExemptIndex(t, ctx, networkId, providerIntentExemptRebuildAge-10*time.Minute)
				normalCount, exemptCount := getNetworkConnectedClientCountsAt(ctx, networkId, now)
				connect.AssertEqual(t, normalCount, 0)
				connect.AssertEqual(t, exemptCount, 5)
				testingAgeProviderIntentExemptIndex(t, ctx, networkId, providerIntentExemptRebuildAge+10*time.Minute)
			} else {
				server.Redis(ctx, func(r server.RedisClient) {
					err := r.ZAdd(ctx, providerIntentExemptKey(networkId), redis.Z{Score: math.Inf(-1), Member: "#ready"}).Err()
					server.Raise(err)
				})
			}
			normalCount, exemptCount := getNetworkConnectedClientCountsAt(ctx, networkId, now)
			connect.AssertEqual(t, int64(normalCount), oracleNormal)
			connect.AssertEqual(t, int64(exemptCount), oracleExempt)
			connect.AssertEqual(t, testingProviderIntentExemptBuildState(ctx, networkId), "#ready")
			// the rebuilt index records its build time
			server.Redis(ctx, func(r server.RedisClient) {
				builtScore, err := r.ZScore(ctx, providerIntentExemptKey(networkId), "#ready").Result()
				server.Raise(err)
				redisNow, err := r.Time(ctx).Result()
				server.Raise(err)
				if age := redisNow.Sub(time.UnixMilli(int64(-builtScore))); age < 0 || time.Minute < age {
					t.Fatalf("%s: the rebuilt index records a build %s ago", built, age)
				}
			})
		}
	})
}

// An index without exempt entries holds only `#ready`, which no sync writes, so
// the counts that read it keep its ttl armed: it does not expire a day after
// its build and become the first key a volatile-ttl eviction takes.
func TestProviderIntentExemptIndexTtlRearmedByCounts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		now := server.NowUtc()
		for range 4 {
			clientId := server.NewId()
			addNetworkPeerWithProvideModesLoader(ctx, networkId, &NetworkPeer{ClientId: clientId}, server.NewId(), 5*time.Minute, func() (map[ProvideMode]bool, error) {
				return map[ProvideMode]bool{}, nil
			})
			testingSetProviderIntentState(t, ctx, networkId, clientId, &ProviderIntentState{
				Status:      ProviderIntentStatusNormal,
				AttemptTime: now,
				CheckTime:   now.Add(ProviderIntentAttemptAllowance),
			}, 10*time.Minute, now)
		}
		normalCount, exemptCount := getNetworkConnectedClientCountsAt(ctx, networkId, now)
		connect.AssertEqual(t, normalCount, 4)
		connect.AssertEqual(t, exemptCount, 0)
		server.Redis(ctx, func(r server.RedisClient) {
			// a day of ttl has nearly passed
			err := r.Expire(ctx, providerIntentExemptKey(networkId), 100*time.Second).Err()
			server.Raise(err)
		})
		getNetworkConnectedClientCountsAt(ctx, networkId, now)
		server.Redis(ctx, func(r server.RedisClient) {
			ttl, err := r.TTL(ctx, providerIntentExemptKey(networkId)).Result()
			server.Raise(err)
			if ttl < networkPeerKeyTtl/2 {
				t.Fatalf("index ttl %s after a count", ttl)
			}
		})
	})
}

// Writes `scoreCount` registry members per score, without the index, as
// registrations from before it.
func testingProviderIntentLegacyRegistrations(ctx context.Context, registryKey string, scores []float64, scoreCount int) {
	server.Redis(ctx, func(r server.RedisClient) {
		pipe := r.TxPipeline()
		for _, score := range scores {
			for range scoreCount {
				pipe.ZAdd(ctx, registryKey, redis.Z{Score: score, Member: string(server.NewId().Bytes())})
			}
		}
		_, err := pipe.Exec(ctx)
		server.Raise(err)
	})
}

// A network of exactly the build limit is built by its first count, which
// reads the index instead of walking: a step reads one member past its budget
// to see that the sweep ends within it, and the provider sweep runs on what the
// connected sweep left.
func TestProviderIntentExemptIndexBuildLimitNetworkBuiltByFirstCount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		walkCounter := providerIntentCountsCounter.WithLabelValues(providerIntentCountPathWalk)
		for _, providerCount := range []int{0, 24} {
			networkId := server.NewId()
			now := server.NowUtc()
			nowMs := float64(now.UnixMilli())
			connectedScores := []float64{}
			for i := range providerIntentExemptBuildLimit - providerCount {
				connectedScores = append(connectedScores, nowMs+float64(time.Hour.Milliseconds()+int64(i)))
			}
			providerScores := []float64{}
			for i := range providerCount {
				providerScores = append(providerScores, nowMs+float64(time.Hour.Milliseconds()+int64(i)))
			}
			testingProviderIntentLegacyRegistrations(ctx, networkPeerConnectedKey(networkId), connectedScores, 1)
			testingProviderIntentLegacyRegistrations(ctx, networkPeerConnectedProviderKey(networkId), providerScores, 1)

			walksBefore := testutil.ToFloat64(walkCounter)
			normalCount, exemptCount := getNetworkConnectedClientCountsAt(ctx, networkId, now)
			connect.AssertEqual(t, normalCount, providerIntentExemptBuildLimit)
			connect.AssertEqual(t, exemptCount, 0)
			connect.AssertEqual(t, testingProviderIntentExemptBuildState(ctx, networkId), "#ready")
			connect.AssertEqual(t, testutil.ToFloat64(walkCounter)-walksBefore, float64(0))
		}
	})
}

// A run of equal scores longer than a build step at the very start of the
// sweep, -inf scores here, which no production writer gives: the cursor's
// start is not a score, so the run is taken whole on the next step and the
// build completes in a handful of counts, each equal to the walk.
func TestProviderIntentExemptIndexBuildSweepsALongRunAtTheStart(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		now := server.NowUtc()
		nowMs := float64(now.UnixMilli())
		testingProviderIntentLegacyRegistrations(ctx, networkPeerConnectedKey(networkId), []float64{math.Inf(-1)}, providerIntentExemptBuildLimit+64)
		later := []float64{}
		for i := range 16 {
			later = append(later, nowMs+float64(time.Hour.Milliseconds()+int64(i)))
		}
		testingProviderIntentLegacyRegistrations(ctx, networkPeerConnectedKey(networkId), later, 1)
		for counts := 1; ; counts += 1 {
			oracleNormal, oracleExempt := testingProviderIntentOracleCounts(ctx, networkId, now)
			normalCount, exemptCount := getNetworkConnectedClientCountsAt(ctx, networkId, now)
			connect.AssertEqual(t, int64(normalCount), oracleNormal)
			connect.AssertEqual(t, int64(exemptCount), oracleExempt)
			state := testingProviderIntentExemptBuildState(ctx, networkId)
			t.Logf("count %d: build state %q", counts, state)
			if state == "#ready" {
				break
			}
			if 4 < counts {
				t.Fatalf("the build did not complete in %d counts", counts)
			}
		}
	})
}

// A build state that does not read (a corrupted or foreign sentinel) restarts
// the build instead of failing every count.
func TestProviderIntentExemptIndexRestartsAnUnreadableBuild(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, state := range []string{"#c abc 123", "#c 1 nan", "#c nan 1", "#q 1 2", "#c start", "#p 5 xyz", "#c start inf none"} {
			now := server.NowUtc()
			networkId, _ := testingProviderIntentExemptNetwork(t, ctx, now)
			server.Redis(ctx, func(r server.RedisClient) {
				pipe := r.TxPipeline()
				pipe.ZRem(ctx, providerIntentExemptKey(networkId), "#ready")
				pipe.ZAdd(ctx, providerIntentExemptKey(networkId), redis.Z{Score: math.Inf(-1), Member: state})
				_, err := pipe.Exec(ctx)
				server.Raise(err)
			})
			normalCount, exemptCount := getNetworkConnectedClientCountsAt(ctx, networkId, now)
			connect.AssertEqual(t, normalCount, 0)
			connect.AssertEqual(t, exemptCount, 5)
			connect.AssertEqual(t, testingProviderIntentExemptBuildState(ctx, networkId), "#ready")
		}
	})
}

// The calls of one redis command the server has executed.
func testingRedisCommandCalls(t testing.TB, ctx context.Context, name string) (calls int64) {
	server.Redis(ctx, func(r server.RedisClient) {
		info, err := r.Info(ctx, "commandstats").Result()
		server.Raise(err)
		for _, line := range strings.Split(info, "\n") {
			stats, ok := strings.CutPrefix(strings.TrimSpace(line), "cmdstat_"+name+":")
			if !ok {
				continue
			}
			for _, field := range strings.Split(stats, ",") {
				if value, ok := strings.CutPrefix(field, "calls="); ok {
					calls, err = strconv.ParseInt(value, 10, 64)
					server.Raise(err)
				}
			}
		}
	})
	return
}

// The scripts that carry the exempt index library run by sha: each call is an
// EVALSHA, and only a script the server has not cached is sent whole. Other
// clients of the server only add EVALSHA calls, so the bound holds whatever
// else runs.
func TestProviderIntentExemptScriptsRunBySha(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		networkId, connectedIds := testingProviderIntentExemptNetwork(t, ctx, now)
		noProvideModes := func() (map[ProvideMode]bool, error) {
			return map[ProvideMode]bool{}, nil
		}
		residentId := server.NewId()
		peerId := server.NewId()
		addNetworkPeerWithProvideModesLoader(ctx, networkId, &NetworkPeer{ClientId: peerId}, residentId, time.Minute, noProvideModes)
		normal := &ProviderIntentState{
			Status:      ProviderIntentStatusNormal,
			AttemptTime: now,
			CheckTime:   now.Add(ProviderIntentAttemptAllowance),
		}
		calls := []struct {
			name string
			call func()
		}{
			{"slot decision", func() {
				record, _ := getProviderIntentRecord(ctx, networkId, connectedIds[0], now)
				applyProviderIntentState(ctx, networkId, connectedIds[0], record, normal, true, 1<<20, now.Add(time.Minute), now)
			}},
			{"count", func() {
				getNetworkConnectedClientCountsAt(ctx, networkId, now)
			}},
			{"observe", func() {
				observeProviderIntentAt(ctx, networkId, connectedIds[1], time.Minute, now)
			}},
			{"peer add", func() {
				addNetworkPeerWithProvideModesLoader(ctx, networkId, &NetworkPeer{ClientId: server.NewId()}, server.NewId(), time.Minute, noProvideModes)
			}},
			{"peer refresh", func() {
				connect.AssertEqual(t, RefreshNetworkPeer(ctx, networkId, peerId, residentId, time.Minute), true)
			}},
			{"install", func() {
				AddNetworkProviderPeer(ctx, networkId, server.NewId(), time.Minute)
			}},
		}
		for _, c := range calls {
			before := testingRedisCommandCalls(t, ctx, "evalsha")
			for range 8 {
				c.call()
			}
			if evalShaCount := testingRedisCommandCalls(t, ctx, "evalsha") - before; evalShaCount < 8 {
				t.Fatalf("%s: %d EVALSHA calls for 8 calls", c.name, evalShaCount)
			}
		}
	})
}
