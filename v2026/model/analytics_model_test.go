package model

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func testWebSearchAnalyticsConfig() WebSearchAnalyticsConfig {
	return WebSearchAnalyticsConfig{
		MinimumImpressions:       WebSearchAnalyticsAbsoluteMinimumImpressions,
		MaximumRowsPerSitePeriod: 5000,
	}
}

func TestEnforceWebSearchAnalyticsVolumeCapKeepsHighestImpressionRows(t *testing.T) {
	config := testWebSearchAnalyticsConfig()
	config.MaximumRowsPerSitePeriod = 3
	day := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	rows := []WebSearchAnalyticsRow{}
	for impressions := 10; impressions < 15; impressions++ {
		rows = append(rows, WebSearchAnalyticsRow{
			Provider: "google", Site: "ur.io", PeriodStart: day,
			PeriodEnd: day.Add(24 * time.Hour), SearchType: "web",
			Query: fmt.Sprintf("q-%d", impressions), Impressions: float64(impressions),
		})
	}
	kept, dropped := EnforceWebSearchAnalyticsVolumeCap(config, rows)
	if dropped != 2 || len(kept) != 3 {
		t.Fatalf("kept=%d dropped=%d", len(kept), dropped)
	}
	for index, want := range []float64{14, 13, 12} {
		if kept[index].Impressions != want {
			t.Fatalf("kept[%d] impressions=%v, want %v", index, kept[index].Impressions, want)
		}
	}
}

func TestEnforceWebSearchAnalyticsVolumeCapRejectsRowsBelowCurrentFloor(t *testing.T) {
	config := testWebSearchAnalyticsConfig()
	config.MinimumImpressions = 25
	day := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	rows := []WebSearchAnalyticsRow{
		{
			Provider: "google", Site: "ur.io", PeriodStart: day,
			PeriodEnd: day.Add(24 * time.Hour), SearchType: "web",
			Query: "too quiet", Impressions: 24,
		},
		{
			Provider: "google", Site: "ur.io", PeriodStart: day,
			PeriodEnd: day.Add(24 * time.Hour), SearchType: "web",
			Query: "enough volume", Impressions: 25,
		},
	}
	kept, dropped := EnforceWebSearchAnalyticsVolumeCap(config, rows)
	if dropped != 1 || len(kept) != 1 || kept[0].Query != "enough volume" {
		t.Fatalf("kept=%+v dropped=%d", kept, dropped)
	}
}

func TestValidWebSearchAnalyticsRowHasAbsoluteVolumeFloor(t *testing.T) {
	day := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	row := WebSearchAnalyticsRow{
		Provider: "google", Site: "ur.io", PeriodStart: day,
		PeriodEnd: day.Add(24 * time.Hour), SearchType: "web",
		Query: "quiet", Impressions: WebSearchAnalyticsAbsoluteMinimumImpressions - 1,
	}
	if ValidWebSearchAnalyticsRow(row) {
		t.Fatal("row below the absolute impression floor was valid")
	}
	row.Impressions = WebSearchAnalyticsAbsoluteMinimumImpressions
	if !ValidWebSearchAnalyticsRow(row) {
		t.Fatal("row at the absolute impression floor was invalid")
	}
}

func TestAnalyticsConfigRejectsLowPrivacyFloorAndReferrerTerms(t *testing.T) {
	config := NewDefaultAnalyticsConfig()
	config.Search.MinimumImpressions = WebSearchAnalyticsAbsoluteMinimumImpressions - 1
	if err := config.Validate(); err == nil {
		t.Fatal("minimum impression floor below the absolute minimum was accepted")
	}
	config.Search.MinimumImpressions = WebSearchAnalyticsAbsoluteMinimumImpressions
	config.Search.AcceptTermsFromReferrers = true
	if err := config.Validate(); err == nil {
		t.Fatal("search terms from referrers were accepted")
	}
}

func TestWebSearchAnalyticsCleanup(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := server.NowUtc().UTC().Truncate(24 * time.Hour)
		oldDay := day.Add(-500 * 24 * time.Hour)
		rows := []WebSearchAnalyticsRow{
			{
				Provider: "google", Site: "ur.io", PeriodStart: oldDay,
				PeriodEnd: oldDay.Add(24 * time.Hour), SearchType: "web",
				Query: "expired", Impressions: 100,
			},
		}
		for _, impressions := range []float64{20, 30, 40, 50, 60, 70} {
			rows = append(rows, WebSearchAnalyticsRow{
				Provider: "google", Site: "ur.io", PeriodStart: day,
				PeriodEnd: day.Add(24 * time.Hour), SearchType: "web",
				Query: fmt.Sprintf("q-%.0f", impressions), Impressions: impressions,
			})
		}

		insertConfig := testWebSearchAnalyticsConfig()
		if persisted := PersistWebSearchAnalyticsRows(ctx, insertConfig, rows); len(persisted) != len(rows) {
			t.Fatalf("persisted=%d, want %d", len(persisted), len(rows))
		}
		if removed := RemoveExpiredWebSearchAnalyticsRows(ctx, day.Add(-400*24*time.Hour), 10000); removed != 1 {
			t.Fatalf("expired removed=%d, want 1", removed)
		}

		cleanupConfig := testWebSearchAnalyticsConfig()
		cleanupConfig.MinimumImpressions = 25
		cleanupConfig.MaximumRowsPerSitePeriod = 3
		if removed := RemoveWebSearchAnalyticsRowsBelowVolume(ctx, cleanupConfig, 10000); removed != 1 {
			t.Fatalf("below-volume removed=%d, want 1", removed)
		}
		if removed := TrimExcessWebSearchAnalyticsRows(ctx, cleanupConfig, rows[1:]); removed != 2 {
			t.Fatalf("excess removed=%d, want 2", removed)
		}

		impressions := []float64{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT impressions
				 FROM web_search_analytics
				 WHERE provider = 'google' AND site = 'ur.io'
				 ORDER BY impressions DESC`,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var value float64
					server.Raise(result.Scan(&value))
					impressions = append(impressions, value)
				}
			})
		})
		for index, want := range []float64{70, 60, 50} {
			if len(impressions) <= index || impressions[index] != want {
				t.Fatalf("remaining impressions=%v, want [70 60 50]", impressions)
			}
		}
		if len(impressions) != 3 {
			t.Fatalf("remaining impressions=%v, want exactly 3 rows", impressions)
		}
	})
}
