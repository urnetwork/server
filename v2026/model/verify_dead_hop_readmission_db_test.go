package model

// The 11-03 validator dead-hop root cause: a provider whose every assignment
// goes unconfirmed is drawn again and again. Admission checks a candidate's
// identity, live egress and provide mode, never its outcomes, so the cohort
// re-admits such a carried member every epoch, and the uniform draw keeps
// drawing it, and each of its hops ends the validator's trail. These tests
// use only the draw and the rollup rows, so they run unchanged against the
// code before dead-hop probation (verify_dead_hop.go), where they fail.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Writes a provider's verify_provider_stats rows for the four 15-minute
// periods that end with the one containing end: assignments and
// confirmations, split evenly. These are the rows the payout's Wilson
// reliability reads.
func testVerifyDeadHopHistory(ctx context.Context, settings *VerifySettings, clientId server.Id, assignments int64, confirmations int64, end time.Time) {
	rows := []*VerifyProviderStatsRow{}
	last := VerifyStatsPeriodStart(end, settings)
	for i := int64(0); i < 4; i += 1 {
		periodStart := last.Add(-time.Duration(3-i) * settings.StatsPeriod)
		row := &VerifyProviderStatsRow{
			PeriodStart:   periodStart,
			PeriodEnd:     periodStart.Add(settings.StatsPeriod),
			ClientId:      clientId,
			Assignments:   assignments / 4,
			Confirmations: confirmations / 4,
		}
		if i == 3 {
			row.Assignments += assignments % 4
			row.Confirmations += confirmations % 4
		}
		rows = append(rows, row)
	}
	UpsertVerifyProviderStats(ctx, rows)
}

// Counts next hops per provider over a run of draws without exclusions.
func testVerifyDeadHopDraws(t testing.TB, ctx context.Context, settings *VerifySettings, draws int) map[server.Id]int {
	t.Helper()
	counts := map[server.Id]int{}
	for i := 0; i < draws; i += 1 {
		nextHop, n := SampleVerifyNextHop(ctx, nil, settings)
		if nextHop == nil || n < 1 {
			t.Fatalf("draw %d assigned no hop (n=%d): %v", i, n, counts)
		}
		counts[*nextHop] += 1
	}
	return counts
}

// Every live provider is still drawn.
func testVerifyDeadHopLiveDrawn(t testing.TB, live []server.Id, counts map[server.Id]int) {
	t.Helper()
	for _, provider := range live {
		if counts[provider] == 0 {
			t.Fatalf("a provider that confirms was never drawn: %v", counts)
		}
	}
}

func TestVerifyCohortDoesNotReadmitDeadCarriedMember(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyCohortSettings(8, 16)
		providers, _ := testVerifyCohortProviders(ctx, settings, 4)
		dead, live := providers[0], providers[1:]

		// all four were members last epoch, so all four are carried
		previous := newVerifyCohortKeys(settings.CohortDeploymentKey, 6)
		server.Redis(ctx, func(r server.RedisClient) {
			for _, provider := range providers {
				if admitted, _ := admitVerifyCohort(ctx, r, previous, provider.String(), settings, true, nil); !admitted {
					t.Fatalf("epoch 6 refused %s", provider)
				}
			}
		})
		now := server.NowUtc()
		testVerifyDeadHopHistory(ctx, settings, dead, 60, 0, now)
		for _, provider := range live {
			testVerifyDeadHopHistory(ctx, settings, provider, 60, 56, now)
		}

		testVerifyCohortSetEpoch(ctx, 7)
		counts := testVerifyDeadHopDraws(t, ctx, settings, 120)
		if 1 < counts[dead] {
			t.Fatalf("a carried member that confirmed none of its 60 assignments in the last hour was re-admitted and drawn %d of 120 times: %v", counts[dead], counts)
		}
		testVerifyDeadHopLiveDrawn(t, live, counts)
	})
}

func TestVerifyUniformDrawSkipsDeadProvider(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()
		providers, _ := testVerifyCohortProviders(ctx, settings, 4)
		dead, live := providers[0], providers[1:]
		now := server.NowUtc()
		testVerifyDeadHopHistory(ctx, settings, dead, 60, 0, now)
		for _, provider := range live {
			testVerifyDeadHopHistory(ctx, settings, provider, 60, 56, now)
		}

		counts := testVerifyDeadHopDraws(t, ctx, settings, 120)
		if 1 < counts[dead] {
			t.Fatalf("a provider that confirmed none of its 60 assignments in the last hour was drawn %d of 120 times: %v", counts[dead], counts)
		}
		testVerifyDeadHopLiveDrawn(t, live, counts)
	})
}
