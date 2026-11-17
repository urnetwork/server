package model

// Dead-hop probation (verify_dead_hop.go): who goes on probation and leaves
// it, the probe that lets a provider come back, a refresh that fails, and
// the Wilson inputs that probation reads but never writes.

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urfoundation/sn/protocol"

	"github.com/urnetwork/server"
)

func testVerifyDeadHopMembers(ctx context.Context) map[string]bool {
	return testVerifyCohortMembers(ctx, verifyDeadHopKey)
}

// Lets the next draw refresh, as the marker's lapse does.
func testVerifyDeadHopRefreshDue(ctx context.Context) {
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Del(ctx, verifyDeadHopRefreshKey).Err())
	})
}

// The rule at one refresh: a provider enters with no confirmation of at
// least 50 assignments in the trailing 24 h, and a member stays while the
// window holds its probes and no confirmation.
func TestVerifyDeadHopProbationRule(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()
		now := time.Date(2026, 11, 4, 12, 7, 0, 0, time.UTC)
		type history struct {
			assignments, confirmations int64
			ago                        time.Duration
		}
		cases := []struct {
			name    string
			member  bool
			history []history
			want    bool
		}{
			{name: "no confirmation of 50", history: []history{{50, 0, 0}}, want: true},
			{name: "no confirmation of 49", history: []history{{49, 0, 0}}},
			{name: "one confirmation of 100", history: []history{{100, 1, 0}}},
			{name: "confirmed 21 hours ago, nothing since", history: []history{{40, 36, 21 * time.Hour}, {100, 0, 0}}},
			{name: "the misses are older than the window", history: []history{{60, 0, 25 * time.Hour}}},
			{name: "misses on both sides of the window edge", history: []history{{60, 0, 25 * time.Hour}, {49, 0, 0}}},
			{name: "a member stays on its probes", member: true, history: []history{{3, 0, 0}}, want: true},
			{name: "a member's confirmation ends it", member: true, history: []history{{3, 1, 0}}},
			{name: "a member with no assignment in the window leaves", member: true, history: []history{{60, 0, 25 * time.Hour}}},
			{name: "a member with no rows leaves", member: true},
		}
		ids := make([]server.Id, len(cases))
		members := []any{}
		for i, c := range cases {
			ids[i] = server.NewId()
			for _, h := range c.history {
				testVerifyDeadHopHistory(ctx, settings, ids[i], h.assignments, h.confirmations, now.Add(-h.ago))
			}
			if c.member {
				members = append(members, ids[i].String())
			}
		}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.SAdd(ctx, verifyDeadHopKey, members...).Err())
		})

		benched, entered, left := refreshVerifyDeadHop(ctx, now, settings)
		got := testVerifyDeadHopMembers(ctx)
		for i, c := range cases {
			if got[ids[i].String()] != c.want {
				t.Errorf("%s: on probation %t, want %t", c.name, got[ids[i].String()], c.want)
			}
		}
		if benched != 2 || entered != 1 || left != 3 || len(got) != 2 {
			t.Fatalf("refresh = %d benched, %d in, %d out; set %v", benched, entered, left, got)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			ttl, err := r.TTL(ctx, verifyDeadHopKey).Result()
			server.Raise(err)
			if ttl <= 0 || verifyDeadHopTtl(settings) < ttl {
				t.Fatalf("probation set ttl %s, want at most %s", ttl, verifyDeadHopTtl(settings))
			}
		})

		// a refresh that finds nobody leaves no set behind
		later := now.Add(3 * 24 * time.Hour)
		if benched, _, left := refreshVerifyDeadHop(ctx, later, settings); benched != 0 || left != 2 || 0 < len(testVerifyDeadHopMembers(ctx)) {
			t.Fatalf("a refresh with nobody on probation kept %d (left %d)", benched, left)
		}
	})
}

// A provider on probation is drawn at most once per probe interval, and a
// confirmation from that probe ends its probation, in the cohort draw and in
// the uniform draw.
func TestVerifyDeadHopProbeAndRecovery(t *testing.T) {
	for _, cohort := range []bool{true, false} {
		server.DefaultTestEnv().Run(t, func(t testing.TB) {
			ctx := context.Background()
			settings := DefaultVerifySettings()
			if cohort {
				settings = testVerifyCohortSettings(8, 16)
				testVerifyCohortSetEpoch(ctx, 7)
			}
			providers, _ := testVerifyCohortProviders(ctx, settings, 4)
			dead, live := providers[0], providers[1:]
			now := server.NowUtc()
			testVerifyDeadHopHistory(ctx, settings, dead, 60, 0, now)
			for _, provider := range live {
				testVerifyDeadHopHistory(ctx, settings, provider, 60, 56, now)
			}

			// one probe, then nothing while its slot is held
			counts := testVerifyDeadHopDraws(t, ctx, settings, 120)
			if counts[dead] != 1 {
				t.Fatalf("cohort=%t: the provider on probation was drawn %d times in one probe interval, want its one probe: %v", cohort, counts[dead], counts)
			}
			testVerifyDeadHopLiveDrawn(t, live, counts)
			server.Redis(ctx, func(r server.RedisClient) {
				ttl, err := r.TTL(ctx, verifyDeadHopProbeKey(dead.String())).Result()
				server.Raise(err)
				if ttl <= 0 || settings.DeadHopProbeInterval < ttl {
					t.Fatalf("cohort=%t: probe slot ttl %s, want at most %s", cohort, ttl, settings.DeadHopProbeInterval)
				}
			})

			// the interval passes: one more probe
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.Del(ctx, verifyDeadHopProbeKey(dead.String())).Err())
			})
			if counts := testVerifyDeadHopDraws(t, ctx, settings, 120); counts[dead] != 1 {
				t.Fatalf("cohort=%t: after its probe interval the provider was drawn %d times, want one probe: %v", cohort, counts[dead], counts)
			}

			// that probe confirmed: once the confirmation reaches the rollup
			// (the current period's row gains the probe and its confirmation)
			// and the probation refreshes, the provider is drawn like the rest
			periodStart := VerifyStatsPeriodStart(now, settings)
			UpsertVerifyProviderStats(ctx, []*VerifyProviderStatsRow{{
				PeriodStart: periodStart, PeriodEnd: periodStart.Add(settings.StatsPeriod),
				ClientId: dead, Assignments: 60/4 + 1, Confirmations: 1,
			}})
			testVerifyDeadHopRefreshDue(ctx)
			counts = testVerifyDeadHopDraws(t, ctx, settings, 120)
			if counts[dead] < 5 || testVerifyDeadHopMembers(ctx)[dead.String()] {
				t.Fatalf("cohort=%t: a confirmed provider stayed on probation: drawn %d of 120: %v", cohort, counts[dead], counts)
			}
		})
	}
}

// Draws refresh once per refresh interval, and probation that is turned off
// neither refreshes nor filters.
func TestVerifyDeadHopRefreshCadenceAndOff(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()
		providers, _ := testVerifyCohortProviders(ctx, settings, 3)
		now := server.NowUtc()

		// the first draw refreshes and holds the marker for the interval
		testVerifyDeadHopDraws(t, ctx, settings, 1)
		server.Redis(ctx, func(r server.RedisClient) {
			ttl, err := r.TTL(ctx, verifyDeadHopRefreshKey).Result()
			server.Raise(err)
			if ttl <= 0 || settings.DeadHopRefreshInterval < ttl {
				t.Fatalf("refresh marker ttl %s, want at most %s", ttl, settings.DeadHopRefreshInterval)
			}
		})
		// a provider that goes dead inside the interval waits for the next refresh
		testVerifyDeadHopHistory(ctx, settings, providers[0], 60, 0, now)
		testVerifyDeadHopDraws(t, ctx, settings, 10)
		if testVerifyDeadHopMembers(ctx)[providers[0].String()] {
			t.Fatal("a draw refreshed while the marker was held")
		}
		testVerifyDeadHopRefreshDue(ctx)
		testVerifyDeadHopDraws(t, ctx, settings, 1)
		if !testVerifyDeadHopMembers(ctx)[providers[0].String()] {
			t.Fatal("the draw after the marker lapsed did not refresh")
		}

		// off: no refresh and no filter, whatever the set holds
		off := DefaultVerifySettings()
		off.DeadHopAssignments = 0
		testVerifyDeadHopRefreshDue(ctx)
		counts := testVerifyDeadHopDraws(t, ctx, off, 120)
		if counts[providers[0]] < 5 {
			t.Fatalf("probation turned off still held a provider back: drawn %d of 120: %v", counts[providers[0]], counts)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			exists, err := r.Exists(ctx, verifyDeadHopRefreshKey).Result()
			server.Raise(err)
			if exists != 0 {
				t.Fatal("probation turned off took the refresh marker")
			}
		})
	})
}

// A refresh that cannot read the stats is counted and logged, keeps the last
// probation set, and never fails the draw.
func TestVerifyDeadHopFailedRefreshKeepsTheLastSet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()
		providers, _ := testVerifyCohortProviders(ctx, settings, 2)
		dead, live := providers[0], providers[1]
		testVerifyDeadHopHistory(ctx, settings, dead, 60, 0, server.NowUtc())
		testVerifyDeadHopDraws(t, ctx, settings, 1)
		if !testVerifyDeadHopMembers(ctx)[dead.String()] {
			t.Fatal("the first draw did not put the dead provider on probation")
		}

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE verify_provider_stats RENAME TO verify_provider_stats_hidden`))
		})
		testVerifyDeadHopRefreshDue(ctx)
		counts := testVerifyDeadHopDraws(t, ctx, settings, 20)
		if counts[live] < 19 || !testVerifyDeadHopMembers(ctx)[dead.String()] {
			t.Fatalf("a failed refresh changed the draw: %v, probation %v", counts, testVerifyDeadHopMembers(ctx))
		}
		server.Redis(ctx, func(r server.RedisClient) {
			exists, err := r.Exists(ctx, verifyDeadHopRefreshKey).Result()
			server.Raise(err)
			if exists != 1 {
				t.Fatal("a failed refresh released the marker, so every draw would retry it")
			}
		})
	})
}

// Probation reads the Wilson inputs and never writes them. A dead provider's
// pending Redis counters and its rollup rows are the same after a refresh and
// a run of draws that skip it, and its reliability is 0 with or without the
// skipped draws. (A probe is an ordinary assignment, recorded by the
// controller like any other.)
func TestVerifyDeadHopLeavesWilsonInputsUnchanged(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()
		providers, _ := testVerifyCohortProviders(ctx, settings, 4)
		dead, live := providers[0], providers[1:]
		now := server.NowUtc()
		// the dead provider's assignments go through the real counters and
		// the rollup, as the controller's records do
		for i := 0; i < 60; i += 1 {
			RecordVerifyAssignment(ctx, dead, now, settings)
		}
		RollupVerifyProviderStats(ctx, now, settings)
		for _, provider := range live {
			testVerifyDeadHopHistory(ctx, settings, provider, 60, 56, now)
		}
		inputs := func() (rows []*VerifyProviderStatsRow, pending map[string]string, inSet bool) {
			rows = GetVerifyProviderStats(ctx, dead)
			server.Redis(ctx, func(r server.RedisClient) {
				var err error
				pending, err = r.HGetAll(ctx, verifyStatsKey(dead, VerifyStatsPeriodStart(now, settings))).Result()
				if err != nil && !errors.Is(err, redis.Nil) {
					server.Raise(err)
				}
				inSet, err = r.SIsMember(ctx, verifyStatClientsKey, dead.String()).Result()
				server.Raise(err)
			})
			return
		}
		rowsBefore, pendingBefore, inSetBefore := inputs()
		if len(rowsBefore) != 1 || rowsBefore[0].Assignments != 60 || rowsBefore[0].Confirmations != 0 || pendingBefore["assignments"] != "60" {
			t.Fatalf("setup: rows %+v pending %v", rowsBefore, pendingBefore)
		}

		counts := testVerifyDeadHopDraws(t, ctx, settings, 200)
		if 1 < counts[dead] {
			t.Fatalf("the dead provider was drawn %d times, want at most its probe", counts[dead])
		}
		rowsAfter, pendingAfter, inSetAfter := inputs()
		if !reflect.DeepEqual(rowsBefore, rowsAfter) || !reflect.DeepEqual(pendingBefore, pendingAfter) || inSetBefore != inSetAfter {
			t.Fatalf("probation changed the Wilson inputs: rows %+v -> %+v, pending %v -> %v", rowsBefore, rowsAfter, pendingBefore, pendingAfter)
		}
		// no confirmation means no reliability, at any exposure: the skipped
		// draws, had they been assigned and missed, change nothing
		aMin := uint64(settings.ReliabilityAMin)
		for _, assignments := range []uint64{60, 61, 260, 60_000} {
			if ppm := protocol.ReliabilityPPM(0, assignments, aMin); ppm != 0 {
				t.Fatalf("reliability of 0 confirmations of %d = %d ppm", assignments, ppm)
			}
		}
	})
}
