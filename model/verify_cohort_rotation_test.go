package model

// Stale rotation of the verify cohort (LEADERTOP.md design question 6, part
// two; COHORT-ROTATION.md): with the lifetime set full, rotation frees room
// only for claimed fleets' members waiting for it; claimed members grow the
// census up to the census limit while unclaimed providers never grow it past
// the lifetime limit; a rotated member returns at no census cost; and a Redis
// that loses the census refuses new providers until it is restored from
// Postgres. Identities are synthetic.

import (
	"context"
	"net/netip"
	"testing"

	"github.com/urnetwork/server"
)

func testVerifyCohortRotationSettings() *VerifySettings {
	settings := testVerifyCohortSettings(10, 4)
	settings.CohortCensusLimit = 6
	settings.ReliabilityAMin = 0
	return settings
}

// Registers eligible providers, each on an address no other provider of the
// test holds: two clients on one address are ambiguous and neither is
// eligible.
type testVerifyCohortAddresses struct {
	next int
}

func (self *testVerifyCohortAddresses) providers(ctx context.Context, settings *VerifySettings, count int) []server.Id {
	providers := []server.Id{}
	for range count {
		clientId := server.NewId()
		ip := netip.AddrFrom4([4]byte{198, 19, byte(self.next / 250), byte(self.next%250 + 1)})
		self.next++
		testVerifySetProvideModes(ctx, clientId)
		FeedVerifyEgress(ctx, clientId, ip, settings)
		providers = append(providers, clientId)
	}
	return providers
}

func testVerifyCohortSetEligible(ctx context.Context, eligible bool, clientIds ...server.Id) {
	server.Redis(ctx, func(r server.RedisClient) {
		for _, clientId := range clientIds {
			if eligible {
				server.Raise(r.SAdd(ctx, verifyEligibleKey, clientId.String()).Err())
			} else {
				server.Raise(r.SRem(ctx, verifyEligibleKey, clientId.String()).Err())
			}
		}
	})
}

func testVerifyCohortDurableCensus(ctx context.Context) map[server.Id]bool {
	census := map[server.Id]bool{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT client_id FROM verify_cohort_census WHERE deployment_key = $1`, string(testVerifyCohortDeployment))
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var clientId server.Id
				server.Raise(rows.Scan(&clientId))
				census[clientId] = true
			}
		})
	})
	return census
}

func testVerifyCohortAdmit(ctx context.Context, settings *VerifySettings, epoch uint64, clientId server.Id, priority bool) (admitted bool, entered bool) {
	keys := newVerifyCohortKeys(settings.CohortDeploymentKey, epoch)
	server.Redis(ctx, func(r server.RedisClient) {
		admitted, _, entered = admitVerifyCohortAs(ctx, r, keys, clientId.String(), settings,
			verifyCohortAdmission{priority: priority, censusRequired: verifyCohortCensusRequired(ctx, settings.CohortDeploymentKey)}, nil)
	})
	return
}

// A full lifetime set refuses a claimed member until rotation frees room for
// it; then claimed members grow the census to its limit, an unclaimed new
// provider is still refused, and a rotated member returns at no census cost.
// Every census member is durable.
func TestVerifyCohortRotationAdmitsClaimedMembersPastAFullLifetime(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyCohortRotationSettings()
		addresses := &testVerifyCohortAddresses{}
		testVerifyCohortSetEpoch(ctx, 1)
		unclaimed := addresses.providers(ctx, settings, 4)
		testVerifyCohortSample(t, ctx, settings, 8)
		keys := newVerifyCohortKeys(settings.CohortDeploymentKey, 3)
		if lifetime := testVerifyCohortMembers(ctx, keys.lifetime); len(lifetime) != 4 {
			t.Fatal("lifetime set", lifetime)
		}
		// two epochs later the four are offline: stale
		testVerifyCohortSetEpoch(ctx, 3)
		testVerifyCohortSetEligible(ctx, false, unclaimed...)
		claimed := addresses.providers(ctx, settings, 3)
		SetVerifyCohortPriority(ctx, settings.CohortDeploymentKey, [][]server.Id{claimed}, 3)
		if nextHop, _ := SampleVerifyNextHop(ctx, nil, settings); nextHop != nil {
			t.Fatal("a claimed member passed the full lifetime set before rotation", nextHop)
		}

		rotation := RotateVerifyCohort(ctx, settings)
		if rotation == nil || rotation.Demand != 3 || rotation.Rotated != 3 || rotation.Census != 4 || rotation.Active != 1 || rotation.Restored {
			t.Fatal("rotation", rotation)
		}
		census := testVerifyCohortMembers(ctx, keys.census)
		if len(census) != 4 || len(testVerifyCohortDurableCensus(ctx)) != 4 {
			t.Fatal("the rotated members left the census", census)
		}
		// claimed members fill the census to its limit of six
		hops := map[string]bool{}
		for range 6 {
			if nextHop, _ := SampleVerifyNextHop(ctx, nil, settings); nextHop != nil {
				hops[nextHop.String()] = true
			}
		}
		admitted := 0
		for _, member := range claimed {
			if hops[member.String()] {
				admitted++
			}
		}
		if admitted != 2 || len(testVerifyCohortMembers(ctx, keys.census)) != 6 {
			t.Fatalf("%d claimed members admitted, census %v: want the census limit to admit two", admitted, testVerifyCohortMembers(ctx, keys.census))
		}
		// an unclaimed new provider never grows the census past the lifetime limit
		fresh := addresses.providers(ctx, settings, 1)
		if ok, _ := testVerifyCohortAdmit(ctx, settings, 3, fresh[0], false); ok {
			t.Fatal("an unclaimed provider grew the census past the lifetime limit")
		}
		// a rotated member returns without growing the census
		returning := unclaimed[0]
		if lifetime := testVerifyCohortMembers(ctx, keys.lifetime); lifetime[returning.String()] {
			returning = unclaimed[3]
		}
		testVerifyCohortSetEligible(ctx, true, returning)
		if ok, entered := testVerifyCohortAdmit(ctx, settings, 3, returning, false); !ok || entered || len(testVerifyCohortMembers(ctx, keys.census)) != 6 {
			t.Fatal("a rotated member did not return at no census cost", ok, entered)
		}
		durable := testVerifyCohortDurableCensus(ctx)
		if len(durable) != 6 {
			t.Fatal("durable census", durable)
		}
		for member := range testVerifyCohortMembers(ctx, keys.census) {
			clientId, _ := server.ParseId(member)
			if !durable[clientId] {
				t.Fatal("a census member is not durable", member)
			}
		}
	})
}

// Rotation removes stale members only while claimed members wait for room,
// at most that many, and never a member drawn this or last epoch, an eligible
// member or a claimed one.
func TestVerifyCohortRotationOnlyMakesRoomForWaitingMembers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyCohortRotationSettings()
		testVerifyCohortSetEpoch(ctx, 6)
		current, previous := newVerifyCohortKeys(settings.CohortDeploymentKey, 6), newVerifyCohortKeys(settings.CohortDeploymentKey, 5)
		drawnNow, drawnBefore, eligible, stale := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		claimedActive := server.NewId()
		server.Redis(ctx, func(r server.RedisClient) {
			for _, member := range []server.Id{drawnNow, drawnBefore, eligible, stale} {
				server.Raise(r.SAdd(ctx, current.lifetime, member.String()).Err())
			}
			server.Raise(r.SAdd(ctx, current.epoch, drawnNow.String()).Err())
			server.Raise(r.SAdd(ctx, previous.epoch, drawnBefore.String()).Err())
		})
		testVerifyCohortSetEligible(ctx, true, eligible)
		// nobody waits: nothing rotates
		if rotation := RotateVerifyCohort(ctx, settings); rotation == nil || rotation.Rotated != 0 || rotation.Demand != 0 {
			t.Fatal("rotation without demand", rotation)
		}
		// two claimed members wait and one claimed member is already active
		waiting := (&testVerifyCohortAddresses{}).providers(ctx, settings, 2)
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.SRem(ctx, current.lifetime, eligible.String()).Err())
			server.Raise(r.SAdd(ctx, current.lifetime, claimedActive.String()).Err())
		})
		SetVerifyCohortPriority(ctx, settings.CohortDeploymentKey, [][]server.Id{append(waiting, claimedActive)}, 3)
		rotation := RotateVerifyCohort(ctx, settings)
		if rotation == nil || rotation.Demand != 2 || rotation.Rotated != 1 {
			t.Fatal("rotation with demand", rotation)
		}
		lifetime := testVerifyCohortMembers(ctx, current.lifetime)
		if lifetime[stale.String()] || !lifetime[drawnNow.String()] || !lifetime[drawnBefore.String()] || !lifetime[claimedActive.String()] {
			t.Fatal("rotation removed a protected member or kept the stale one", lifetime)
		}
		if census := testVerifyCohortMembers(ctx, current.census); !census[stale.String()] {
			t.Fatal("the rotated member left the census", census)
		}
	})
}

// A Redis that loses the census refuses every new provider while a durable
// census exists; the next rotation restores it from Postgres, and the restored
// census still bounds admissions.
func TestVerifyCohortCensusFailsClosedAfterRedisLoss(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyCohortRotationSettings()
		testVerifyCohortSetEpoch(ctx, 2)
		keys := newVerifyCohortKeys(settings.CohortDeploymentKey, 2)
		addresses := &testVerifyCohortAddresses{}
		first := addresses.providers(ctx, settings, 3)
		testVerifyCohortSample(t, ctx, settings, 6)
		if rotation := RotateVerifyCohort(ctx, settings); rotation == nil || rotation.Census != 3 || len(testVerifyCohortDurableCensus(ctx)) != 3 {
			t.Fatal("seeding", rotation)
		}
		// Redis loses every cohort key
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Del(ctx, keys.lifetime, keys.census, keys.censusReady, keys.epoch, keys.draws).Err())
		})
		testVerifyCohortSetEligible(ctx, false, first...)
		late := addresses.providers(ctx, settings, 2)
		if nextHop, _ := SampleVerifyNextHop(ctx, nil, settings); nextHop != nil {
			t.Fatal("a new provider was admitted while the census was lost", nextHop)
		}
		rotation := RotateVerifyCohort(ctx, settings)
		if rotation == nil || !rotation.Restored || rotation.Census != 3 {
			t.Fatal("restore", rotation)
		}
		// the restored census admits one more unclaimed provider, to the
		// lifetime limit of four, and no further
		admitted := 0
		for _, provider := range late {
			if ok, _ := testVerifyCohortAdmit(ctx, settings, 2, provider, false); ok {
				admitted++
			}
		}
		if admitted != 1 || len(testVerifyCohortMembers(ctx, keys.census)) != 4 {
			t.Fatal("admissions after restore", admitted, testVerifyCohortMembers(ctx, keys.census))
		}
	})
}

// An api deployed ahead of the census migration has no verify_cohort_census
// table. No durable census can exist then, so draws admit as they did before
// rotation instead of failing every next hop on the missing relation.
func TestVerifyCohortDrawsWithoutTheCensusTable(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyCohortRotationSettings()
		testVerifyCohortSetEpoch(ctx, 2)
		keys := newVerifyCohortKeys(settings.CohortDeploymentKey, 2)
		addresses := &testVerifyCohortAddresses{}
		addresses.providers(ctx, settings, 3)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TABLE verify_cohort_census`))
		})
		hops := testVerifyCohortSample(t, ctx, settings, 6)
		if len(hops) != 3 || len(testVerifyCohortMembers(ctx, keys.lifetime)) != 3 {
			t.Fatal("draws without the census table", hops, testVerifyCohortMembers(ctx, keys.lifetime))
		}
		if verifyCohortCensusRequired(ctx, settings.CohortDeploymentKey) {
			t.Fatal("a missing census table required a durable census")
		}
	})
}

// Any other census read failure fails closed without failing the draw: with
// no ready Redis census a new provider waits, and nothing is cached, so the
// first draw after the failure reads the census again.
func TestVerifyCohortCensusReadFailureFailsClosed(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyCohortRotationSettings()
		testVerifyCohortSetEpoch(ctx, 2)
		addresses := &testVerifyCohortAddresses{}
		addresses.providers(ctx, settings, 2)
		renameDeploymentKey := func(from string, to string) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE verify_cohort_census RENAME COLUMN `+from+` TO `+to))
			})
		}
		renameDeploymentKey("deployment_key", "unreadable_deployment_key")
		if nextHop, _ := SampleVerifyNextHop(ctx, nil, settings); nextHop != nil {
			t.Fatal("a new provider was admitted while the census could not be read", nextHop)
		}
		renameDeploymentKey("unreadable_deployment_key", "deployment_key")
		testVerifyCohortSample(t, ctx, settings, 2)
	})
}

// Rotation frees only the room the free active slots don't cover: one free
// slot covers one waiting claimed member, and with the active set full one
// waiting member rotates one stale member out. The rotation reports the
// active set after rotating, and the member's admission grows the census by
// one past the active set it was seeded from. This is the shape of the first
// production rotation (2026-10-31): "1 stale members out for 1 waiting
// (census 2000, active 1999)", then 2,001 durable census members.
func TestVerifyCohortRotationCountsFreeActiveSlots(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyCohortRotationSettings()
		testVerifyCohortSetEpoch(ctx, 6)
		keys := newVerifyCohortKeys(settings.CohortDeploymentKey, 6)
		stale := []server.Id{server.NewId(), server.NewId(), server.NewId()}
		server.Redis(ctx, func(r server.RedisClient) {
			for _, member := range stale {
				server.Raise(r.SAdd(ctx, keys.lifetime, member.String()).Err())
			}
		})
		addresses := &testVerifyCohortAddresses{}
		first := addresses.providers(ctx, settings, 1)[0]
		SetVerifyCohortPriority(ctx, settings.CohortDeploymentKey, [][]server.Id{{first}}, 3)

		// three of four active: the free slot covers the one waiting member
		rotation := RotateVerifyCohort(ctx, settings)
		if rotation == nil || rotation.Demand != 1 || rotation.Rotated != 0 || rotation.Active != 3 || rotation.Census != 3 {
			t.Fatal("one free slot for one waiting member", rotation)
		}
		if hops := testVerifyCohortSample(t, ctx, settings, 1); !hops[first.String()] {
			t.Fatal("the waiting member did not take the free slot", hops)
		}

		// four of four active: one waiting member rotates one stale member out
		second := addresses.providers(ctx, settings, 1)[0]
		SetVerifyCohortPriority(ctx, settings.CohortDeploymentKey, [][]server.Id{{first}, {second}}, 3)
		rotation = RotateVerifyCohort(ctx, settings)
		if rotation == nil || rotation.Demand != 1 || rotation.Rotated != 1 || rotation.Active != 3 || rotation.Census != 4 {
			t.Fatal("a full active set for one waiting member", rotation)
		}
		testVerifyCohortSample(t, ctx, settings, 2)
		active := testVerifyCohortMembers(ctx, keys.lifetime)
		if len(active) != 4 || !active[first.String()] || !active[second.String()] {
			t.Fatal("the waiting member did not take the rotated slot", active)
		}
		if census, durable := testVerifyCohortMembers(ctx, keys.census), testVerifyCohortDurableCensus(ctx); len(census) != 5 || len(durable) != 5 {
			t.Fatal("the admission grows the census by one", census, durable)
		}

		// the census (5) is now past the lifetime limit, but free slots count
		// against the active set: a third waiting member rotates one out
		third := addresses.providers(ctx, settings, 1)[0]
		SetVerifyCohortPriority(ctx, settings.CohortDeploymentKey, [][]server.Id{{first}, {second}, {third}}, 3)
		rotation = RotateVerifyCohort(ctx, settings)
		if rotation == nil || rotation.Demand != 1 || rotation.Rotated != 1 || rotation.Active != 3 || rotation.Census != 5 {
			t.Fatal("free slots count against the active set, not the census", rotation)
		}
	})
}
