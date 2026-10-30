package model

// Claimed-fleet priority in the verify cohort (LEADERTOP.md design question
// 6): the priority set keeps at most a per-fleet share of each live claimed
// fleet, eligible and carried members first; its members draw ahead of every
// other provider and skip the growth gate, but never pass either cap.
// Identities are synthetic.

import (
	"context"
	"slices"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The per-fleet share fits a full head of 200 fleets into one cohort.
func TestVerifyCohortPriorityPerFleetIsAnEvenShare(t *testing.T) {
	for size, want := range map[int]int{1500: 7, 200: 1, 199: 1, 4000: 20} {
		if got := VerifyCohortPriorityPerFleet(testVerifyCohortSettings(size, size)); got != want {
			t.Fatalf("cohort %d: %d per fleet, want %d", size, got, want)
		}
	}
}

// Each fleet contributes at most perFleet members: eligible ones first, then
// ones already in the lifetime set, then by client id. A refresh replaces the
// set, an empty refresh removes it, and the set expires if refreshes stop.
func TestVerifyCohortPrioritySetCapsEachFleet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyCohortSettings(10, 20)
		testVerifyCohortSetEpoch(ctx, 3)
		keys := newVerifyCohortKeys(settings.CohortDeploymentKey, 3)
		eligible, _ := testVerifyCohortProviders(ctx, settings, 3)
		offline := []server.Id{server.NewId(), server.NewId(), server.NewId()}
		slices.SortFunc(offline, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		// offline[2] is carried from an earlier epoch
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.SAdd(ctx, keys.lifetime, offline[2].String()).Err())
		})
		fleetA := append(append([]server.Id{}, offline...), eligible[0])
		fleetB := []server.Id{eligible[1], eligible[2], offline[0]}
		count := SetVerifyCohortPriority(ctx, settings.CohortDeploymentKey, [][]server.Id{fleetA, fleetB}, 2)
		priority := testVerifyCohortMembers(ctx, keys.priority)
		// fleet A: its eligible member, then its carried one; fleet B: both eligible
		want := []string{eligible[0].String(), offline[2].String(), eligible[1].String(), eligible[2].String()}
		if count != 4 || len(priority) != 4 {
			t.Fatalf("priority set %v (%d), want %v", priority, count, want)
		}
		for _, member := range want {
			if !priority[member] {
				t.Fatalf("priority set %v lacks %s", priority, member)
			}
		}
		server.Redis(ctx, func(r server.RedisClient) {
			if ttl, err := r.TTL(ctx, keys.priority).Result(); err != nil || ttl <= 0 || verifyCohortPriorityTtl < ttl {
				t.Fatalf("priority ttl %v %v", ttl, err)
			}
		})
		// a refresh replaces the set
		if count := SetVerifyCohortPriority(ctx, settings.CohortDeploymentKey, [][]server.Id{fleetB}, 1); count != 1 {
			t.Fatal("refresh", count)
		}
		if priority := testVerifyCohortMembers(ctx, keys.priority); len(priority) != 1 || !priority[eligible[1].String()] {
			t.Fatal("refreshed priority set", priority)
		}
		if count := SetVerifyCohortPriority(ctx, settings.CohortDeploymentKey, nil, 7); count != 0 || len(testVerifyCohortMembers(ctx, keys.priority)) != 0 {
			t.Fatal("an empty refresh kept members", count)
		}
		if status, ok := GetVerifyCohortStatus(ctx, settings); !ok || status.Epoch != 3 || status.Capacity != 10 || status.LifetimeLimit != 20 || status.Lifetime != 1 || status.Members != 0 || status.Priority != 0 {
			t.Fatal("status", status, ok)
		}
	})
}

// With claimed fleets, every priority member that is eligible is drawn into
// the cohort before any other new provider, past a closed growth gate; the
// cohort and lifetime caps still hold, and a priority member the lifetime
// set has no room for is refused.
func TestVerifyCohortSamplingDrawsClaimedMembersFirst(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyCohortSettings(4, 5)
		settings.ReliabilityAMin = 1000
		testVerifyCohortSetEpoch(ctx, 7)
		keys := newVerifyCohortKeys(settings.CohortDeploymentKey, 7)
		providers, _ := testVerifyCohortProviders(ctx, settings, 12)
		// one unclaimed provider is admitted first; with a_min 1000 the growth
		// gate then stays closed to every other new provider
		first := testVerifyCohortSample(t, ctx, settings, 1)
		var unclaimed string
		for hop := range first {
			unclaimed = hop
		}
		claimed := []server.Id{}
		for _, provider := range providers {
			if provider.String() != unclaimed && len(claimed) < 3 {
				claimed = append(claimed, provider)
			}
		}
		if count := SetVerifyCohortPriority(ctx, settings.CohortDeploymentKey, [][]server.Id{claimed[:2], claimed[2:]}, 2); count != 3 {
			t.Fatal("priority set", count)
		}
		testVerifyCohortSample(t, ctx, settings, 12)
		members := testVerifyCohortMembers(ctx, keys.epoch)
		if len(members) != 4 || !members[unclaimed] {
			t.Fatalf("epoch cohort %v", members)
		}
		for _, member := range claimed {
			if !members[member.String()] {
				t.Fatalf("claimed member %s was not admitted past the closed growth gate: %v", member, members)
			}
		}
		// the cohort is full: a new claimed fleet's member waits for room
		late := providers[len(providers)-1]
		if members[late.String()] {
			t.Fatal("the late provider was already admitted")
		}
		SetVerifyCohortPriority(ctx, settings.CohortDeploymentKey, [][]server.Id{{late}}, 2)
		testVerifyCohortSample(t, ctx, settings, 8)
		if members := testVerifyCohortMembers(ctx, keys.epoch); len(members) != 4 || members[late.String()] {
			t.Fatalf("a claimed member passed the cohort size: %v", members)
		}
		// the next epoch has room, but the lifetime set has one slot left
		testVerifyCohortSetEpoch(ctx, 8)
		next := newVerifyCohortKeys(settings.CohortDeploymentKey, 8)
		lateTwo := providers[len(providers)-2]
		SetVerifyCohortPriority(ctx, settings.CohortDeploymentKey, [][]server.Id{{late}, {lateTwo}}, 1)
		testVerifyCohortSample(t, ctx, settings, 12)
		lifetime := testVerifyCohortMembers(ctx, keys.lifetime)
		admitted := testVerifyCohortMembers(ctx, next.epoch)
		if len(lifetime) != 5 || len(admitted) != 4 || admitted[late.String()] == admitted[lateTwo.String()] {
			t.Fatalf("lifetime %v, epoch 8 cohort %v: exactly one of the two late claimed members fits the lifetime limit", lifetime, admitted)
		}
		if status, ok := GetVerifyCohortStatus(ctx, settings); !ok || status.Epoch != 8 || status.Members != 4 || status.Lifetime != 5 || status.Priority != 2 {
			t.Fatal("status", status, ok)
		}
		queried := append([]server.Id{late, lateTwo}, claimed...)
		epochMembers, lifetimeMembers := GetVerifyCohortMembership(ctx, settings, queried)
		for _, clientId := range queried {
			if epochMembers[clientId] != admitted[clientId.String()] || lifetimeMembers[clientId] != lifetime[clientId.String()] {
				t.Fatal("membership", clientId, epochMembers, lifetimeMembers)
			}
		}
		if len(lifetimeMembers) != 4 {
			t.Fatal("lifetime membership", lifetimeMembers)
		}
	})
}
