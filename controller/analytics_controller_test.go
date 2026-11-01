package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func testAnalyticsControllerConfig() *model.AnalyticsConfig {
	config := model.NewDefaultAnalyticsConfig()
	config.Search.RedactEmailAddresses = true
	config.Search.RedactPhoneNumbers = true
	config.Search.RedactIPAddresses = true
	config.WebViews.NormalizeTrailingSlash = true
	return config
}

func TestAnalyticsPrivacySafeQueryRequiresVolumeAndRedactsIdentifiers(t *testing.T) {
	config := testAnalyticsControllerConfig()
	if query, ok := privacySafeQuery(config, "quiet term", 9); ok || query != "" {
		t.Fatalf("below-floor query survived: %q", query)
	}
	query, ok := privacySafeQuery(
		config,
		"contact me@example.com at +1 (312) 555-0123 from 203.0.113.7",
		10,
	)
	if !ok {
		t.Fatal("volume-qualified query was rejected")
	}
	for _, private := range []string{"me@example.com", "312", "203.0.113.7"} {
		if strings.Contains(query, private) {
			t.Fatalf("query retained %q: %q", private, query)
		}
	}
	for _, marker := range []string{"[redacted-email]", "[redacted-phone]", "[redacted-ip]"} {
		if !strings.Contains(query, marker) {
			t.Fatalf("query lacks %q: %q", marker, query)
		}
	}
}

func TestAnalyticsNormalizePathDropsQueryAndRejectsForeignOrigin(t *testing.T) {
	config := testAnalyticsControllerConfig()
	site := model.AnalyticsSiteConfig{Name: "ur.io", Origin: "https://ur.io"}
	path, ok := normalizePath(config, site, "https://ur.io/blog/post/?secret=discard#fragment")
	if !ok || path != "/blog/post" {
		t.Fatalf("normalized path = %q, %v", path, ok)
	}
	if _, ok := normalizePath(config, site, "https://example.com/blog/post"); ok {
		t.Fatal("foreign landing origin was accepted")
	}
}

func TestAnalyticsParseBingDateUsesProviderDay(t *testing.T) {
	day, ok := parseBingDate("/Date(1316156400000-0700)/")
	if !ok || day.Format("2006-01-02") != "2011-09-16" {
		t.Fatalf("bing date = %v, %v", day, ok)
	}
	if _, ok := parseBingDate("2011-09-16"); ok {
		t.Fatal("accepted undocumented Bing date encoding")
	}
}

func TestAnalyticsManualCSVAppliesPrivacyAndAliases(t *testing.T) {
	config := testAnalyticsControllerConfig()
	sites := map[string]model.AnalyticsSiteConfig{
		"ur.io": {Name: "ur.io", Origin: "https://ur.io"},
	}
	csvBody := strings.Join([]string{
		"property,day,keyword,landing_page,clicks,shows,avg_position",
		"ur.io,2026-08-01,private tail,https://ur.io/blog/?discard=yes,1,9,2",
		"ur.io,2026-08-01,visible term,https://ur.io/blog/?discard=yes,2,20,3",
	}, "\n")
	rows, rejected, err := parseManualCSV(config, sites, []byte(csvBody))
	if err != nil {
		t.Fatal(err)
	}
	if rejected != 1 || len(rows) != 1 {
		t.Fatalf("rows=%d rejected=%d", len(rows), rejected)
	}
	if rows[0].Query != "visible term" || rows[0].Path != "/blog" || rows[0].Impressions != 20 {
		t.Fatalf("row = %+v", rows[0])
	}
}

func TestAnalyticsGoogleWindowUsesExclusiveFinalizedEndAndOverlap(t *testing.T) {
	config := testAnalyticsControllerConfig()
	now := time.Date(2026, time.August, 28, 12, 0, 0, 0, time.UTC)
	start, end, err := googleWindow(config, model.WebSearchAnalyticsIngestState{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if end.Format("2006-01-02") != "2026-08-25" || end.Sub(start) != 90*24*time.Hour {
		t.Fatalf("initial window = %s..%s", start, end)
	}
	state := model.WebSearchAnalyticsIngestState{CursorTime: end}
	overlapStart, overlapEnd, err := googleWindow(config, state, now)
	if err != nil {
		t.Fatal(err)
	}
	if overlapEnd != end || overlapStart != end.Add(-72*time.Hour) {
		t.Fatalf("overlap window = %s..%s", overlapStart, overlapEnd)
	}
}

// A Google stream whose first attempt failed has a state row without a
// cursor. It is due again one interval after that attempt and reads the same
// window as a new stream. After a success, its cursor sets the overlap window.
func TestAnalyticsGoogleStreamAfterFailedFirstAttemptStartsLikeNewStream(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		config := testAnalyticsControllerConfig()
		interval := config.Providers.Google.IntervalDuration()
		attemptTime := time.Date(2026, time.October, 30, 4, 52, 0, 0, time.UTC)
		model.MarkWebSearchAnalyticsIngestAttempt(ctx, "google", "ur.io", "web", attemptTime)
		model.MarkWebSearchAnalyticsIngestError(ctx, "google", "ur.io", "web", attemptTime, "request_failed")

		state := model.GetWebSearchAnalyticsIngestState(ctx, "google", "ur.io", "web")
		nextRun := attemptTime.Add(interval)
		if isDue(state, nextRun.Add(-time.Second), interval) || !isDue(state, nextRun, interval) {
			t.Fatalf("failed first attempt at %s is not due exactly one interval later: %+v", attemptTime, state)
		}
		start, end, err := googleWindow(config, state, nextRun)
		if err != nil {
			t.Fatal(err)
		}
		newState := model.GetWebSearchAnalyticsIngestState(ctx, "google", "ur.io", "image")
		newStart, newEnd, err := googleWindow(config, newState, nextRun)
		if err != nil {
			t.Fatal(err)
		}
		if !start.Equal(newStart) || !end.Equal(newEnd) || end.Sub(start) != config.InitialLookback() {
			t.Fatalf("window after a failed first attempt = %s..%s, new stream = %s..%s", start, end, newStart, newEnd)
		}

		model.MarkWebSearchAnalyticsIngestSuccess(ctx, "google", "ur.io", "web", nextRun, end)
		state = model.GetWebSearchAnalyticsIngestState(ctx, "google", "ur.io", "web")
		overlapStart, overlapEnd, err := googleWindow(config, state, nextRun.Add(interval))
		if err != nil {
			t.Fatal(err)
		}
		if !overlapStart.Equal(end.Add(-config.Overlap())) || !overlapEnd.Equal(end) {
			t.Fatalf("window after a success = %s..%s, want the overlap before cursor %s", overlapStart, overlapEnd, end)
		}
	})
}
