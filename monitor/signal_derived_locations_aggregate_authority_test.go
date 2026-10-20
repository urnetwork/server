package monitor

import (
	"context"
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Nonfinite values are valid float syntax, but cannot authorize a complete
// observation. Exercise the public signal and keep errors independent of rows.
func TestDerivedLocationsSignalRejectsNonfiniteMedians(t *testing.T) {
	for _, column := range []int{4, 5} {
		for _, value := range []string{"NaN", "+Inf", "-Inf", "Infinity", "-Infinity"} {
			fixture := healthyDerivedLocationsFixture()
			source := fixture.source(t)
			healthy := source.postgresFn
			source.postgresFn = func(query string) ([]Row, error) {
				rows, err := healthy(query)
				if err == nil && strings.Contains(query, "monitor-signal-2.19c-derived-locations-table") {
					rows[0][column] = value
				}
				return rows, err
			}
			alerts, err := NewDerivedLocationsSignal().Run(context.Background(), syntheticSettings(source))
			field := "residual"
			if column == 5 {
				field = "reputation"
			}
			want := "derived-location table median " + field + ": nonfinite aggregate"
			if err == nil || err.Error() != want || len(alerts) != 0 {
				t.Errorf("column %d value %s: want fixed aggregate error and no alerts", column, value)
			}
			// Fail at the owning table boundary before any dependent observation.
			if len(fixture.queries) != 2 || len(fixture.redisArgs) != 0 {
				t.Errorf("column %d value %s: continued after an invalid table aggregate", column, value)
			}
		}
	}
}

// The registered execution path projects a failed table observation into
// visibility without exposing the scalar or inventing a healthy alert.
func TestDerivedLocationsSignalNonfiniteMedianVisibility(t *testing.T) {
	fixture := healthyDerivedLocationsFixture()
	fixture.table.medianReputation = math.NaN()
	settings := syntheticSettings(fixture.source(t))
	alerts, err := NewWithSignals(settings, NewDerivedLocationsSignal()).Run(context.Background())
	if err == nil || len(alerts) != 1 {
		t.Fatal("nonfinite aggregate did not retain exactly one visibility failure")
	}
	alert := requireAlertClass(t, alerts, "cannot-observe")
	if alert.SignalNumber != "2.19c" || alert.SignalKey != "derived-locations" || alert.SignalID != "monitor/visibility" || alert.Severity != SeverityWarn {
		t.Fatal("aggregate failure lost its public visibility identity")
	}
	requireAlertOmits(t, alert, "NaN", "nonfinite aggregate", "median reputation:")
}

// Finite medians retain the existing thresholds, equality, zero and -1
// empty-table sentinel; residual thresholding belongs to run history.
func TestDerivedLocationsSignalFiniteMedianControls(t *testing.T) {
	for _, test := range []struct {
		residual   float64
		reputation float64
		wantLow    bool
	}{
		{residual: 6.5, reputation: 0.9},
		{residual: 0, reputation: 0.5},
		{residual: 1e300, reputation: 0.5},
		{residual: 6.5, reputation: 0.49, wantLow: true},
		{residual: 0, reputation: 0, wantLow: true},
	} {
		fixture := healthyDerivedLocationsFixture()
		fixture.table.medianResidualKm = test.residual
		fixture.table.medianReputation = test.reputation
		alerts := fixture.run(t, nil)
		if test.wantLow {
			requireDerivedLocationsAlerts(t, alerts, "derive-exclusions/median-reputation")
		} else {
			requireDerivedLocationsAlerts(t, alerts)
		}
	}
	fixture := healthyDerivedLocationsFixture()
	fixture.runs = nil
	fixture.table = derivedLocationsTable{medianResidualKm: -1, medianReputation: -1, lastUpdateAgeSeconds: -1}
	requireDerivedLocationsAlerts(t, fixture.run(t, nil))
}

// An unavailable or partial Redis history cannot erase an independently
// proved finite table violation or manufacture class health.
func TestDerivedLocationsSignalFiniteMedianWithUnknownHistory(t *testing.T) {
	malformed := "{\"run_time\":\"2026-08-29T11:00:00Z\"}\nsynthetic-private-record.example\n"
	for _, partial := range []bool{false, true} {
		fixture := healthyDerivedLocationsFixture()
		if partial {
			fixture.redisOut = &malformed
		} else {
			fixture.redisErr = errors.New("synthetic-private-transport.example")
		}
		fixture.table.medianReputation = 0
		alerts := fixture.run(t, nil)
		requireDerivedLocationsAlerts(t, alerts,
			"derive-run-history-unobservable/",
			"derive-exclusions/median-reputation",
		)
		for _, alert := range alerts {
			requireAlertOmits(t, alert, "synthetic-private-record.example", "synthetic-private-transport.example")
		}
		for _, finding := range fixture.findings(t) {
			if finding.class == "derive-exclusions" && finding.healthy {
				t.Fatal("unknown history authorized exclusions health")
			}
		}
	}
}

// The actual model writer and table aggregate admit PostgreSQL real's
// nonfinite domain. This uses only the attested local disposable fixture.
func TestDerivedLocationsSignalNonfiniteMediansOnADatabase(t *testing.T) {
	if os.Getenv("WARP_ENV") != "local" {
		t.Fatal("derived-location query fixtures require the attested local test environment")
	}
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		row := &model.DerivedLocation{
			NodeKind:          model.DerivedLocationNodeKindProvider,
			NodeId:            server.NewId(),
			GenesisAccuracyKm: 25,
			PingCount:         12,
			PeerCount:         4,
			LocationId:        server.NewId(),
			CityLocationId:    server.NewId(),
			RegionLocationId:  server.NewId(),
			CountryLocationId: server.NewId(),
			UpdateTime:        now,
		}
		for _, field := range []string{"residual", "reputation"} {
			for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
				row.ResidualKm, row.Reputation = 5, 0.9
				if field == "residual" {
					row.ResidualKm = float32(value)
				} else {
					row.Reputation = float32(value)
				}
				written, _ := model.ReplaceDerivedLocations(ctx, []*model.DerivedLocation{row})
				if written != 1 {
					tb.Fatal("synthetic model row was not written")
				}
				settings := syntheticSettings(&derivedLocationsDatabaseSource{})
				settings.Now = func() time.Time { return now }
				alerts, err := NewDerivedLocationsSignal().Run(ctx, settings)
				want := "derived-location table median " + field + ": nonfinite aggregate"
				if err == nil || err.Error() != want || len(alerts) != 0 {
					tb.Errorf("%s: want fixed aggregate error from the native table query", field)
				}
			}
		}
	})
}
