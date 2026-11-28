package model

// A Redis that loses every verify cohort key while a durable census exists, as
// a flush of the cohort's cluster slot does. The next rotation restores the
// census from Postgres, but Redis held the only copy of the active (lifetime)
// set. Identities are synthetic.

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/v2026"
)

// The admission refusals counted for one reason, in this process.
func testVerifyCohortRefusals(refusal verifyCohortRefusal) float64 {
	label, _ := refusal.label()
	return testutil.ToFloat64(verifyCohortAdmissionRefusals.WithLabelValues(label))
}

func testVerifyCohortCensusFullRefusals() float64 {
	return testVerifyCohortRefusals(verifyCohortRefusedCensusFull)
}

// Root cause: after a flush the rotation restores the census, which claimed
// members had grown past the lifetime limit, but not the active set, which
// then holds only the members re-admitted since. With room left in the active
// set the sampler draws only new providers, the admission script refuses each
// one as census_full, and once its attempts are used up a SEED gets no next
// hop although this epoch's cohort has members to assign.
func TestVerifyCohortSeedGetsANextHopAfterRedisLosesTheCohort(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyCohortRotationSettings()
		addresses := &testVerifyCohortAddresses{}

		// epoch 1: four providers fill the active set to the lifetime limit,
		// and rotation makes the census ready and durable
		testVerifyCohortSetEpoch(ctx, 1)
		unclaimed := addresses.providers(ctx, settings, 4)
		testVerifyCohortSample(t, ctx, settings, 8)
		RotateVerifyCohort(ctx, settings)
		// epoch 3: three of them are offline, and two claimed members take
		// rotated room, growing the census past the lifetime limit
		testVerifyCohortSetEpoch(ctx, 3)
		testVerifyCohortSetEligible(ctx, false, unclaimed[1:]...)
		claimed := addresses.providers(ctx, settings, 2)
		fleets := [][]server.Id{claimed}
		SetVerifyCohortPriority(ctx, settings.CohortDeploymentKey, fleets, 2)
		RotateVerifyCohort(ctx, settings)
		testVerifyCohortSample(t, ctx, settings, 6)
		keys := newVerifyCohortKeys(settings.CohortDeploymentKey, 3)
		online := []server.Id{unclaimed[0], claimed[0], claimed[1]}
		measured := testVerifyCohortMembers(ctx, keys.epoch)
		census := testVerifyCohortMembers(ctx, keys.census)
		if len(measured) != len(online) || len(census) != 6 || len(testVerifyCohortDurableCensus(ctx)) != 6 {
			t.Fatal("setup: epoch cohort", measured, "census", census)
		}
		for _, member := range online {
			if !measured[member.String()] {
				t.Fatal("setup: an online member is not in the epoch cohort", member)
			}
		}
		// eligible providers the census has no room for
		fresh := addresses.providers(ctx, settings, 12)
		seed, validator := server.NewId(), server.NewId()
		if hop, _ := SampleVerifyNextHop(ctx, []server.Id{seed, validator}, settings); hop == nil || !measured[hop.String()] {
			t.Fatal("before the loss a SEED was not served from the epoch cohort", hop)
		}

		// Redis loses every key of the cohort, whatever its epoch; the binding
		// sync sets the priority set again and the rotation restores the
		// census from Postgres
		server.Redis(ctx, func(r server.RedisClient) {
			prefix := strings.TrimSuffix(keys.lifetime, "lifetime")
			lost := []string{}
			iter := r.Scan(ctx, 0, prefix+"*", 1000).Iterator()
			for iter.Next(ctx) {
				lost = append(lost, iter.Val())
			}
			server.Raise(iter.Err())
			if len(lost) == 0 {
				t.Fatal("no cohort keys to lose")
			}
			server.Raise(r.Del(ctx, lost...).Err())
		})
		SetVerifyCohortPriority(ctx, settings.CohortDeploymentKey, fleets, 2)
		if rotation := RotateVerifyCohort(ctx, settings); rotation == nil || !rotation.Restored || rotation.Census != 6 {
			t.Fatal("the census was not restored", rotation)
		}
		// the online members return to this epoch's cohort, as their draws
		// did after the loss
		for _, member := range online {
			if admitted, _ := testVerifyCohortAdmit(ctx, settings, 3, member, member != unclaimed[0]); !admitted {
				t.Fatal("an online census member did not return after the restore", member)
			}
		}

		refusedBefore := testVerifyCohortCensusFullRefusals()
		hop, n := SampleVerifyNextHop(ctx, []server.Id{seed, validator}, settings)
		refused := testVerifyCohortCensusFullRefusals() - refusedBefore
		active := testVerifyCohortMembers(ctx, keys.lifetime)
		census = testVerifyCohortMembers(ctx, keys.census)
		if hop == nil {
			t.Fatalf("after the restore a SEED got no next hop: %d of %d attempts refused as census_full; active set %d, census %d, lifetime limit %d, epoch cohort %d",
				int(refused), settings.SampleMaxAttempts, len(active), len(census), settings.CohortLifetimeLimit, len(testVerifyCohortMembers(ctx, keys.epoch)))
		}
		if !measured[hop.String()] || n < 1 {
			t.Fatal("the SEED's next hop is not an epoch cohort member", hop, n)
		}
		if refused != 0 {
			t.Fatalf("the sampler drew %d new providers the census had no room for", int(refused))
		}

		// the restore rebuilt the active set from the census, to the lifetime
		// limit, online members first
		if len(active) != settings.CohortLifetimeLimit {
			t.Fatal("the active set was not rebuilt to the lifetime limit", active)
		}
		testVerifyCohortSubset(t, "rebuilt active set", active, census)
		for _, member := range online {
			if !active[member.String()] {
				t.Fatal("the rebuilt active set lacks an online member", member)
			}
		}
		// every SEED is served, and nothing grows the census
		for range 20 {
			if hop, _ := SampleVerifyNextHop(ctx, []server.Id{seed, validator}, settings); hop == nil || !measured[hop.String()] {
				t.Fatal("a SEED was not served from the epoch cohort", hop)
			}
		}
		if refused := testVerifyCohortCensusFullRefusals() - refusedBefore; refused != 0 {
			t.Fatalf("%d census_full refusals while the census had no room", int(refused))
		}
		census = testVerifyCohortMembers(ctx, keys.census)
		if len(census) != 6 || len(testVerifyCohortDurableCensus(ctx)) != 6 {
			t.Fatal("the census grew", census)
		}
		for _, provider := range fresh {
			if census[provider.String()] {
				t.Fatal("a new unclaimed provider entered a census past the lifetime limit", provider)
			}
		}
	})
}

// A cap that refuses a claimed member refuses the rest of its fleet: with the
// active set full and more claimed members waiting for rotated room than a
// SEED has attempts, the first refusal ends the claimed draws and the SEED is
// served from this epoch's cohort. Nobody enters the full active set.
func TestVerifyCohortClaimedRefusalFallsBackToTheEpochCohort(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyCohortSettings(20, 4)
		settings.CohortCensusLimit = 40
		settings.ReliabilityAMin = 0
		addresses := &testVerifyCohortAddresses{}
		testVerifyCohortSetEpoch(ctx, 2)
		addresses.providers(ctx, settings, 4)
		testVerifyCohortSample(t, ctx, settings, 8)
		RotateVerifyCohort(ctx, settings)
		keys := newVerifyCohortKeys(settings.CohortDeploymentKey, 2)
		measured := testVerifyCohortMembers(ctx, keys.epoch)
		if len(measured) != 4 || len(testVerifyCohortMembers(ctx, keys.lifetime)) != 4 {
			t.Fatal("setup: epoch cohort", measured)
		}
		waiting := addresses.providers(ctx, settings, settings.SampleMaxAttempts+2)
		SetVerifyCohortPriority(ctx, settings.CohortDeploymentKey, [][]server.Id{waiting}, len(waiting))

		for draw := range 10 {
			refusedBefore := testVerifyCohortCensusFullRefusals()
			hop, _ := SampleVerifyNextHop(ctx, []server.Id{server.NewId()}, settings)
			refused := testVerifyCohortCensusFullRefusals() - refusedBefore
			if hop == nil {
				t.Fatalf("draw %d: a SEED got no next hop: %d of %d attempts refused as census_full", draw, int(refused), settings.SampleMaxAttempts)
			}
			if !measured[hop.String()] {
				t.Fatal("the SEED's next hop is not an epoch cohort member", hop)
			}
			if refused != 1 {
				t.Fatalf("draw %d: %d census_full refusals, want the first to end the claimed draws", draw, int(refused))
			}
		}
		if lifetime, census := testVerifyCohortMembers(ctx, keys.lifetime), testVerifyCohortMembers(ctx, keys.census); len(lifetime) != 4 || len(census) != 4 {
			t.Fatal("a claimed member entered the full active set", lifetime, census)
		}
	})
}

// The sampler does not read the rolling census's interlock: once it refuses a
// new provider, the remaining attempts skip new providers and the SEED is
// served from this epoch's cohort.
func TestVerifyCohortInterlockRefusalFallsBackToTheEpochCohort(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyCohortRotationSettings()
		addresses := &testVerifyCohortAddresses{}
		now := server.NowUtc()
		testVerifyCohortStartRolling(t, ctx, settings, verifyCohortInterlockLimit-1, now)
		member := addresses.providers(ctx, settings, 1)[0]
		if admitted, _ := testVerifyCohortAdmit(ctx, settings, 4, member, false); !admitted {
			t.Fatal("the first admission under the interlock was refused")
		}
		addresses.providers(ctx, settings, 12)

		refusedBefore := testVerifyCohortRefusals(verifyCohortRefusedInterlock)
		hop, _ := SampleVerifyNextHop(ctx, []server.Id{server.NewId()}, settings)
		refused := testVerifyCohortRefusals(verifyCohortRefusedInterlock) - refusedBefore
		if hop == nil {
			t.Fatalf("a SEED got no next hop: %d of %d attempts refused by the interlock", int(refused), settings.SampleMaxAttempts)
		}
		if *hop != member || refused != 1 {
			t.Fatalf("next hop %v after %d interlock refusals, want the epoch cohort's member after one", hop, int(refused))
		}
		keys := newVerifyCohortKeys(settings.CohortDeploymentKey, 4)
		if census := testVerifyCohortMembers(ctx, keys.census); len(census) != 1 || !census[member.String()] {
			t.Fatal("a provider entered the census past the interlock", census)
		}
	})
}
