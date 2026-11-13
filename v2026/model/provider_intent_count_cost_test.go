// The redis cost of a provider intent count, measured as the commands one slot
// decision or one count runs. On 2026-11-01 the walk over every registry
// member (two or three commands per member) saturated a redis node for one
// network of about 950 members.
package model

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// Runs a production script's source with its KEYS and ARGV, every redis.call
// counted by a shim around it. Returns the commands the script ran and its
// result. The count is the script's own: the server's commandstats would also
// count every other client of the server, and false-fail.
func testingCountedScript(t testing.TB, ctx context.Context, script string, keys []string, args ...any) (commands int64, result []any) {
	counted := fmt.Sprintf(`
local counted_calls = 0
local counted_redis = redis
local redis = setmetatable({
	call = function(...)
		counted_calls = counted_calls + 1
		return counted_redis.call(...)
	end,
}, {__index = counted_redis})
local function counted_script()
%s
end
local counted_result = counted_script()
return {counted_calls, counted_result}
`, script)
	server.Redis(ctx, func(r server.RedisClient) {
		values, err := r.Eval(ctx, counted, keys, args...).Slice()
		server.Raise(err)
		commands = values[0].(int64)
		result, _ = values[1].([]any)
	})
	return
}

// The commands of one normal slot decision of a failed client (the apply that
// runs count_clients atomically with its write) and of one network count, in
// a network of `connectedCount` connected members and half as many provider
// installs. Half of each are exempt intent clients with a live presence. The
// measured calls are the production scripts with the production arguments,
// once the network's index is complete.
func testingProviderIntentCountCosts(t testing.TB, ctx context.Context, connectedCount int) (decisionCommands int64, countCommands int64) {
	now := server.NowUtc()
	networkId := server.NewId()
	noProvideModes := func() (map[ProvideMode]bool, error) {
		return map[ProvideMode]bool{}, nil
	}
	exempt := &ProviderIntentState{
		Status:       ProviderIntentStatusPending,
		AttemptTime:  now,
		GraceEndTime: now.Add(providerIntentGraceTimeout),
		CheckTime:    now.Add(providerIntentGraceTimeout),
	}
	providerCount := connectedCount / 2
	exemptCount := 0
	for i := range connectedCount {
		clientId := server.NewId()
		addNetworkPeerWithProvideModesLoader(ctx, networkId, &NetworkPeer{ClientId: clientId}, server.NewId(), time.Hour, noProvideModes)
		if i%2 == 0 {
			testingSetProviderIntentState(t, ctx, networkId, clientId, exempt, time.Hour, now)
			exemptCount += 1
		}
	}
	for i := range providerCount {
		clientId := server.NewId()
		AddNetworkProviderPeer(ctx, networkId, clientId, time.Hour)
		if i%2 == 0 {
			testingSetProviderIntentState(t, ctx, networkId, clientId, exempt, time.Hour, now)
			exemptCount += 1
		}
	}
	failedId := server.NewId()
	addNetworkPeerWithProvideModesLoader(ctx, networkId, &NetworkPeer{ClientId: failedId}, server.NewId(), time.Hour, noProvideModes)
	normal := &ProviderIntentState{
		Status:      ProviderIntentStatusNormal,
		AttemptTime: now,
		CheckTime:   now.Add(ProviderIntentAttemptAllowance),
	}
	testingSetProviderIntentState(t, ctx, networkId, failedId, normal, time.Hour, now)

	// every decision finds a free slot and rewrites the same record
	record, _ := getProviderIntentRecord(ctx, networkId, failedId, now)
	overLimit := *normal
	overLimit.Status = ProviderIntentStatusOverLimit
	decisionArgs := providerIntentApplyArgs(failedId, record, normal.record(), overLimit.record(), 1<<20, normal, now.Add(time.Hour), now)
	// the decisions build the index first, a quarter of the registries or the
	// build limit per decision; the decision that completes the build reads
	// the index too, so the decision after it is the one measured
	for builds := 0; testingProviderIntentExemptBuildState(ctx, networkId) != "#ready"; builds += 1 {
		_, result := testingCountedScript(t, ctx, providerIntentApplyLua, providerIntentKeys(networkId), decisionArgs...)
		connect.AssertEqual(t, result[0], int64(1))
		if 4 < builds {
			t.Fatalf("decisions did not complete the index in %d decisions", builds)
		}
	}
	decisionCommands, result := testingCountedScript(t, ctx, providerIntentApplyLua, providerIntentKeys(networkId), decisionArgs...)
	connect.AssertEqual(t, result[0], int64(1))
	connect.AssertEqual(t, result[2], int64(1))

	countCommands, result = testingCountedScript(t, ctx, providerIntentCountClientsLua, providerIntentKeys(networkId), providerIntentCountArgs(now)...)
	connect.AssertEqual(t, result[0], int64(connectedCount+providerCount+1-exemptCount))
	connect.AssertEqual(t, result[1], int64(exemptCount))
	connect.AssertEqual(t, result[2], int64(1))
	return
}

// A slot decision and a count run the same handful of redis commands in a
// network of 49 registry members and of 1537, where the walk over every member
// ran two or three commands per member.
func TestProviderIntentCountCostIndependentOfNetworkSize(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()

		smallDecision, smallCount := testingProviderIntentCountCosts(t, ctx, 32)
		largeDecision, largeCount := testingProviderIntentCountCosts(t, ctx, 1024)
		t.Logf(
			"redis commands per decision: %d (49 members), %d (1537 members); per count: %d, %d",
			smallDecision,
			largeDecision,
			smallCount,
			largeCount,
		)

		connect.AssertEqual(t, largeDecision, smallDecision)
		connect.AssertEqual(t, largeCount, smallCount)
		if 48 < smallDecision || 13 < smallCount {
			t.Fatalf("redis commands per decision %d (want at most 48), per count %d (want at most 13)", smallDecision, smallCount)
		}
	})
}
