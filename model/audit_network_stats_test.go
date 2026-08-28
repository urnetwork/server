package model

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
)

func TestNetworkDataFromCurrentWalksAuditChangesBackward(t *testing.T) {
	changes := map[string]networkDayChange{
		"2026-08-24": {created: 3, deleted: 2},
		"2026-08-25": {created: 4, deleted: 1},
	}

	data := networkDataFromCurrent(
		"2026-08-23",
		"2026-08-25",
		100,
		changes,
	)

	want := map[string]int{
		"2026-08-23": 96,
		"2026-08-24": 97,
		"2026-08-25": 100,
	}
	for day, wantCount := range want {
		if got := data[day]; got != wantCount {
			t.Errorf("network count for %s = %d, want %d", day, got, wantCount)
		}
	}
}

func TestNetworkDataFromCurrentCarriesNetworksWithoutAuditEvents(t *testing.T) {
	data := networkDataFromCurrent(
		"2026-08-23",
		"2026-08-25",
		884_161,
		nil,
	)

	for day, got := range data {
		if got != 884_161 {
			t.Errorf("network count for %s = %d, want authoritative current baseline %d", day, got, 884_161)
		}
	}
}

// The production incident was caused by live network rows whose creation
// audit events had aged out. This exercises the DB query with exactly that
// shape: a real network row and no audit_network_event baseline must still be
// present in both the current summary and today's daily value.
func TestComputeStatsNetworkAnchorsToLiveNetworkTable(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		Testing_CreateNetwork(
			ctx,
			server.NewId(),
			"audit-network-stats-anchor",
			server.NewId(),
		)

		stats := &Stats{Lookback: 2}
		server.Db(ctx, func(conn server.PgConn) {
			computeStatsNetwork(ctx, stats, conn)
		})

		want := int(CountNetworks(ctx))
		if stats.NetworksSummary != want {
			t.Fatalf("NetworksSummary = %d, want live network count %d", stats.NetworksSummary, want)
		}
		_, endDay := dayRange(stats.Lookback)
		if got := stats.NetworksData[endDay]; got != want {
			t.Fatalf("NetworksData[%s] = %d, want live network count %d", endDay, got, want)
		}
	})
}

func TestSetCurrentProviderSummaries(t *testing.T) {
	stats := &Stats{
		ProvidersSummary: 999,
		CountriesSummary: 999,
		RegionsSummary:   999,
		CitiesSummary:    999,
	}
	setCurrentProviderSummaries(stats, []ProviderCountryCount{
		{Count: 10, RegionCount: 3, CityCount: 5},
		{Count: 7, RegionCount: 2, CityCount: 4},
	})

	if stats.ProvidersSummary != 17 ||
		stats.CountriesSummary != 2 ||
		stats.RegionsSummary != 5 ||
		stats.CitiesSummary != 9 {
		t.Fatalf(
			"current summaries = providers:%d countries:%d regions:%d cities:%d, want 17/2/5/9",
			stats.ProvidersSummary,
			stats.CountriesSummary,
			stats.RegionsSummary,
			stats.CitiesSummary,
		)
	}
}
