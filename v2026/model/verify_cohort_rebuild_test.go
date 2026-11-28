package model

// The lifetime (active) set's rebuild from the census (RotateVerifyCohort),
// and the census bounds after a Redis loss at the default limits. Identities
// are synthetic.

import (
	"context"
	"slices"
	"testing"

	"github.com/urnetwork/server/v2026"
)

func testVerifyCohortCard(ctx context.Context, key string) int {
	var count int64
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		count, err = r.SCard(ctx, key).Result()
		server.Raise(err)
	})
	return int(count)
}

// Writes a durable census, as the census write-through does.
func testVerifyCohortPutDurableCensus(ctx context.Context, clientIds []server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO verify_cohort_census (deployment_key, client_id, admitted_at)
				SELECT $1, client_id, $3 FROM unnest($2::uuid[]) AS client_id
			`,
			string(testVerifyCohortDeployment), clientIds, server.NowUtc(),
		))
	})
}

// A lost lifetime set gets the census members rotation keeps, to the lifetime
// limit: claimed members, members drawn this or the previous epoch, eligible
// members, then stale ones in descending client id. Members already in the set
// stay. The rebuild runs once per loss of its marker or the set, and never
// changes the census.
func TestVerifyCohortRotationRebuildsTheLifetimeSetFromTheCensus(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyCohortRotationSettings()
		testVerifyCohortSetEpoch(ctx, 6)
		keys, previous := newVerifyCohortKeys(settings.CohortDeploymentKey, 6), newVerifyCohortKeys(settings.CohortDeploymentKey, 5)
		claimed, drawnNow, drawnBefore, eligible := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		stale := []server.Id{server.NewId(), server.NewId()}
		slices.SortFunc(stale, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		all := append([]server.Id{claimed, drawnNow, drawnBefore, eligible}, stale...)
		// a ready census past the lifetime limit, with no lifetime set
		testVerifyCohortPutDurableCensus(ctx, all)
		server.Redis(ctx, func(r server.RedisClient) {
			for _, member := range all {
				server.Raise(r.SAdd(ctx, keys.census, member.String()).Err())
			}
			server.Raise(r.Set(ctx, keys.censusReady, "1", 0).Err())
			server.Raise(r.SAdd(ctx, keys.epoch, drawnNow.String()).Err())
			server.Raise(r.SAdd(ctx, previous.epoch, drawnBefore.String()).Err())
		})
		testVerifyCohortSetEligible(ctx, true, eligible)
		SetVerifyCohortPriority(ctx, settings.CohortDeploymentKey, [][]server.Id{{claimed}}, 1)
		want := func(members ...server.Id) map[string]bool {
			set := map[string]bool{}
			for _, member := range members {
				set[member.String()] = true
			}
			return set
		}
		same := func(a map[string]bool, b map[string]bool) bool {
			if len(a) != len(b) {
				return false
			}
			for member := range a {
				if !b[member] {
					return false
				}
			}
			return true
		}

		rotation := RotateVerifyCohort(ctx, settings)
		if rotation == nil || rotation.Rebuilt != 4 || rotation.Active != 4 || rotation.Census != 6 || rotation.Rotated != 0 || rotation.Restored {
			t.Fatal("rebuild", rotation)
		}
		if lifetime := testVerifyCohortMembers(ctx, keys.lifetime); !same(lifetime, want(claimed, drawnNow, drawnBefore, eligible)) {
			t.Fatal("the rebuilt lifetime set did not keep what rotation keeps", lifetime)
		}
		if len(testVerifyCohortMembers(ctx, keys.census)) != 6 || len(testVerifyCohortDurableCensus(ctx)) != 6 {
			t.Fatal("the rebuild changed the census")
		}

		// once rebuilt, a smaller set is the cohort's own doing: no rebuild
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.SRem(ctx, keys.lifetime, eligible.String()).Err())
		})
		if rotation := RotateVerifyCohort(ctx, settings); rotation == nil || rotation.Rebuilt != 0 || rotation.Active != 3 {
			t.Fatal("a second rotation rebuilt the lifetime set", rotation)
		}
		// an empty set is rebuilt again
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Del(ctx, keys.lifetime).Err())
		})
		if rotation := RotateVerifyCohort(ctx, settings); rotation == nil || rotation.Rebuilt != 4 || rotation.Active != 4 {
			t.Fatal("an empty lifetime set was not rebuilt", rotation)
		}
		// a lost marker rebuilds around the members still in the set, which
		// count against the limit
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Del(ctx, keys.lifetime, keys.lifetimeReady).Err())
			server.Raise(r.SAdd(ctx, keys.lifetime, stale[0].String()).Err())
		})
		if rotation := RotateVerifyCohort(ctx, settings); rotation == nil || rotation.Rebuilt != 3 {
			t.Fatal("a lost marker did not rebuild the lifetime set", rotation)
		}
		if lifetime := testVerifyCohortMembers(ctx, keys.lifetime); !same(lifetime, want(stale[0], claimed, drawnNow, drawnBefore)) {
			t.Fatal("the rebuild did not keep the set's members within the limit", lifetime)
		}
		// with room for one stale member, it is the one rotation takes last
		wider := testVerifyCohortRotationSettings()
		wider.CohortLifetimeLimit = 5
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Del(ctx, keys.lifetime).Err())
		})
		if rotation := RotateVerifyCohort(ctx, wider); rotation == nil || rotation.Rebuilt != 5 {
			t.Fatal("rebuild to a wider limit", rotation)
		}
		if lifetime := testVerifyCohortMembers(ctx, keys.lifetime); !same(lifetime, want(claimed, drawnNow, drawnBefore, eligible, stale[1])) {
			t.Fatal("the rebuild kept the stale member rotation takes first", lifetime)
		}

		// the script adds only census members, and never past the limit
		outside := server.NewId()
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Del(ctx, keys.lifetime).Err())
			added, err := r.Eval(ctx, verifyCohortRebuildScript, []string{keys.lifetime, keys.census}, 2, outside.String(), stale[0].String(), stale[1].String(), claimed.String()).Int()
			server.Raise(err)
			if added != 2 {
				t.Fatal("the rebuild script added", added)
			}
		})
		if lifetime := testVerifyCohortMembers(ctx, keys.lifetime); !same(lifetime, want(stale[0], stale[1])) {
			t.Fatal("the rebuild script added a member outside the census or past the limit", lifetime)
		}
	})
}

// At the default limits (controller/verify_controller.go, pinned by
// TestVerifyCohortCensusFitsTheValidatorCensus): unclaimed providers never
// grow the census past the 2,000 lifetime limit, claimed members never past
// the 3,072 census limit, three quarters of the validator's hard 4,096. A Redis
// loss with the census one short of its limit restores the census and
// rebuilds the lifetime set without passing either bound; one claimed member
// takes the last census place, every other new provider is refused, and every
// SEED is served.
func TestVerifyCohortRestoreKeepsTheCensusBoundsAtTheDefaultLimits(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const lifetimeLimit, censusLimit, validatorMaxProviders = 2000, 3072, 4096
		settings := testVerifyCohortSettings(1500, lifetimeLimit)
		settings.CohortCensusLimit = censusLimit
		if !(settings.CohortSize <= lifetimeLimit && lifetimeLimit <= censusLimit && censusLimit < validatorMaxProviders && verifyCohortInterlockLimit < validatorMaxProviders) {
			t.Fatal("the default limits do not nest inside the validator's census")
		}
		addresses := &testVerifyCohortAddresses{}
		testVerifyCohortSetEpoch(ctx, 7)
		keys := newVerifyCohortKeys(settings.CohortDeploymentKey, 7)
		online := addresses.providers(ctx, settings, 3)
		durable := append([]server.Id{}, online...)
		for len(durable) < censusLimit-1 {
			durable = append(durable, server.NewId())
		}
		testVerifyCohortPutDurableCensus(ctx, durable)
		claimed := addresses.providers(ctx, settings, 2)
		fresh := addresses.providers(ctx, settings, 12)
		SetVerifyCohortPriority(ctx, settings.CohortDeploymentKey, [][]server.Id{claimed}, len(claimed))

		// Redis holds no cohort key: the census is restored, the lifetime set
		// rebuilt to its limit, and rotation frees room for the two claimed
		// members waiting
		rotation := RotateVerifyCohort(ctx, settings)
		if rotation == nil || !rotation.Restored || rotation.Census != censusLimit-1 || rotation.Rebuilt != lifetimeLimit || rotation.Rotated != 2 || rotation.Active != lifetimeLimit-2 {
			t.Fatal("restore", rotation)
		}
		lifetime, census := testVerifyCohortMembers(ctx, keys.lifetime), testVerifyCohortMembers(ctx, keys.census)
		testVerifyCohortSubset(t, "rebuilt lifetime set", lifetime, census)
		for _, member := range online {
			if !lifetime[member.String()] {
				t.Fatal("the rebuilt lifetime set lacks an eligible census member", member)
			}
		}

		seed := server.NewId()
		for draw := range 40 {
			if hop, _ := SampleVerifyNextHop(ctx, []server.Id{seed}, settings); hop == nil {
				t.Fatalf("draw %d: a SEED got no next hop", draw)
			}
			if active, census := testVerifyCohortCard(ctx, keys.lifetime), testVerifyCohortCard(ctx, keys.census); lifetimeLimit < active || censusLimit < census {
				t.Fatalf("draw %d: active set %d, census %d past its bound", draw, active, census)
			}
		}
		census = testVerifyCohortMembers(ctx, keys.census)
		entered := 0
		for _, member := range claimed {
			if census[member.String()] {
				entered++
			}
		}
		if len(census) != censusLimit || entered != 1 || len(testVerifyCohortDurableCensus(ctx)) != censusLimit {
			t.Fatalf("census %d with %d claimed members, want the limit %d with one", len(census), entered, censusLimit)
		}
		for _, provider := range fresh {
			if census[provider.String()] {
				t.Fatal("an unclaimed provider entered a census past the lifetime limit", provider)
			}
		}
	})
}
