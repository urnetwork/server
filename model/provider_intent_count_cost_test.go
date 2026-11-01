// The redis cost of a provider intent count, measured as the commands the
// server executes for one slot decision or one count, scripts' commands
// included. On 2026-11-01 the walk over every registry member (three commands
// per member) saturated a redis node for one network of about 950 members.
package model

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The commands the redis server has executed, those run inside scripts
// included, apart from INFO and the script calls themselves.
func testingRedisCommandsExecuted(t testing.TB, ctx context.Context) (count int64) {
	server.Redis(ctx, func(r server.RedisClient) {
		info, err := r.Info(ctx, "commandstats").Result()
		if err != nil {
			t.Fatalf("commandstats: %s", err)
		}
		for _, line := range strings.Split(info, "\n") {
			name, stats, ok := strings.Cut(strings.TrimSpace(line), ":")
			if !ok || !strings.HasPrefix(name, "cmdstat_") {
				continue
			}
			switch strings.TrimPrefix(name, "cmdstat_") {
			case "info", "eval", "evalsha", "eval_ro", "evalsha_ro":
				continue
			}
			for _, field := range strings.Split(stats, ",") {
				if value, ok := strings.CutPrefix(field, "calls="); ok {
					calls, err := strconv.ParseInt(value, 10, 64)
					if err != nil {
						t.Fatalf("commandstats %s: %s", line, err)
					}
					count += calls
				}
			}
		}
	})
	return
}

// The fewest commands `measure` took over `runs` runs. Other activity on the
// server only adds commands, so the minimum is the operation's own cost.
func testingFewestRedisCommands(t testing.TB, ctx context.Context, runs int, measure func()) int64 {
	fewest := int64(-1)
	for range runs {
		before := testingRedisCommandsExecuted(t, ctx)
		measure()
		commands := testingRedisCommandsExecuted(t, ctx) - before
		if fewest < 0 || commands < fewest {
			fewest = commands
		}
	}
	return fewest
}

// The commands of one normal slot decision of a failed client (the apply that
// runs count_clients atomically with its write) and of one network count, in
// a network of `connectedCount` connected members and half as many provider
// installs. Half of each are exempt intent clients with a live presence.
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
	decisionCommands = testingFewestRedisCommands(t, ctx, 8, func() {
		result := applyProviderIntentState(ctx, networkId, failedId, record, normal, true, 1<<20, now.Add(time.Hour), now)
		if !result.applied || result.overLimit {
			t.Fatalf("decision: applied=%t over_limit=%t", result.applied, result.overLimit)
		}
	})

	countCommands = testingFewestRedisCommands(t, ctx, 8, func() {
		normalCount, providerIntentCount := getNetworkConnectedClientCountsAt(ctx, networkId, now)
		connect.AssertEqual(t, normalCount, connectedCount+providerCount+1-exemptCount)
		connect.AssertEqual(t, providerIntentCount, exemptCount)
	})
	return
}

// A slot decision and a count cost the same handful of redis commands in a
// network of 49 registry members and of 1537, where the walk over every member
// cost two or three commands per member.
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
		if 48 < smallDecision || 16 < smallCount {
			t.Fatalf("redis commands per decision %d (want at most 48), per count %d (want at most 16)", smallDecision, smallCount)
		}
	})
}
