// Tests for the destination pool's storage (connect/GEOMAP.md §11.4): the
// rows, the seed and the candidate sync, the site and place tallies, the
// refresh settings, and what ingest may count.
package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server"
)

// A place mark covers its country, and its region when it names one, as the
// prober compares them.
func TestProviderEgressDestinationPlaceCoversItsCountryAndRegion(t *testing.T) {
	for _, test := range []struct {
		mark   ProviderEgressDestinationPlace
		place  ProviderEgressPlace
		covers bool
	}{
		{mark: ProviderEgressDestinationPlace{Country: "cn"}, place: ProviderEgressPlace{CountryCode: "cn", Region: "Beijing"}, covers: true},
		{mark: ProviderEgressDestinationPlace{Country: "CN "}, place: ProviderEgressPlace{CountryCode: "cn"}, covers: true},
		{mark: ProviderEgressDestinationPlace{Country: "us", Region: "California"}, place: ProviderEgressPlace{CountryCode: "us", Region: "california"}, covers: true},
		{mark: ProviderEgressDestinationPlace{Country: "us", Region: "California"}, place: ProviderEgressPlace{CountryCode: "us", Region: "Texas"}, covers: false},
		{mark: ProviderEgressDestinationPlace{Country: "us", Region: "California"}, place: ProviderEgressPlace{CountryCode: "us"}, covers: false},
		{mark: ProviderEgressDestinationPlace{Country: "cn"}, place: ProviderEgressPlace{CountryCode: "de"}, covers: false},
		{mark: ProviderEgressDestinationPlace{Country: ""}, place: ProviderEgressPlace{}, covers: false},
	} {
		if covers := test.mark.Covers(test.place); covers != test.covers {
			t.Errorf("%+v covers %+v = %t, want %t", test.mark, test.place, covers, test.covers)
		}
	}
}

// A candidate is inactive, out of its cooldown and not dropped for good.
func TestProviderEgressDestinationIsCandidateHonoursCooldownAndRetirements(t *testing.T) {
	now := time.Date(2026, time.September, 20, 0, 0, 0, 0, time.UTC)
	cooldown := 30 * 24 * time.Hour
	retiredAt := func(age time.Duration) *time.Time {
		retired := now.Add(-age)
		return &retired
	}
	for _, test := range []struct {
		name      string
		d         ProviderEgressDestination
		candidate bool
	}{
		{name: "never promoted", d: ProviderEgressDestination{}, candidate: true},
		{name: "active", d: ProviderEgressDestination{Active: true}, candidate: false},
		{name: "cooling down", d: ProviderEgressDestination{RetiredTime: retiredAt(cooldown - time.Second), RetireCount: 1}, candidate: false},
		{name: "cooled down", d: ProviderEgressDestination{RetiredTime: retiredAt(cooldown), RetireCount: 1}, candidate: true},
		{name: "dropped for good", d: ProviderEgressDestination{RetiredTime: retiredAt(2 * cooldown), RetireCount: 3}, candidate: false},
	} {
		if candidate := test.d.IsCandidate(now, cooldown, 3); candidate != test.candidate {
			t.Errorf("%s: candidate = %t, want %t", test.name, candidate, test.candidate)
		}
	}
}

// What a load may count: not on probation, not from a place the site is
// marked for, and anything the pool does not hold.
func TestProviderEgressHealthScoringScoresOnlyScoredSitesFromCompatiblePlaces(t *testing.T) {
	scoring := NewProviderEgressHealthScoring([]*ProviderEgressDestination{
		{Name: "scored-site", Class: "site", Active: true},
		{Name: "probation-site", Class: "site", Active: true, Probation: true},
		{Name: "blocked-site", Class: "cdn", Active: true, Incompatible: []ProviderEgressDestinationPlace{{Country: "cn"}}},
	})
	cn := ProviderEgressPlace{CountryCode: "cn"}
	de := ProviderEgressPlace{CountryCode: "de"}
	for _, test := range []struct {
		name   string
		place  ProviderEgressPlace
		scores bool
	}{
		{name: "scored-site", place: cn, scores: true},
		{name: "probation-site", place: de, scores: false},
		{name: "blocked-site", place: cn, scores: false},
		{name: "blocked-site", place: de, scores: true},
		{name: "builtin-site", place: cn, scores: true},
	} {
		if scores := scoring.Scores(test.name, test.place); scores != test.scores {
			t.Errorf("%s from %s scores = %t, want %t", test.name, test.place.CountryCode, scores, test.scores)
		}
	}
	if scoring.Class("blocked-site") != "cdn" || scoring.Class("builtin-site") != "" {
		t.Error("the scoring does not give back the pool's classes")
	}
	var none *ProviderEgressHealthScoring
	if !none.Scores("probation-site", de) || none.Class("scored-site") != "" {
		t.Error("no scoring must count every load")
	}
}

// Ingest takes the failures the pool says must not count out of a run's
// counts and names them apart; everything else stays as submitted.
func TestScoreProviderEgressHealthLeavesUnscoredFailuresOut(t *testing.T) {
	scoring := NewProviderEgressHealthScoring([]*ProviderEgressDestination{
		{Name: "probation-site", Class: "site", Active: true, Probation: true},
		{Name: "blocked-site", Class: "cdn", Active: true, Incompatible: []ProviderEgressDestinationPlace{{Country: "cn"}}},
	})
	health := &ProviderEgressHealth{
		OKCount: 7,
		Total:   10,
		ClassResults: map[string]ProviderEgressHealthClassResult{
			"site": {OK: 4, Total: 6},
			"cdn":  {OK: 3, Total: 4},
		},
		FailedNames: "probation-site,blocked-site,scored-site",
	}
	scored := ScoreProviderEgressHealth(health, ProviderEgressPlace{CountryCode: "cn"}, scoring)
	if scored.OKCount != 7 || scored.Total != 8 {
		t.Fatalf("scored counts = %d/%d, want 7/8", scored.OKCount, scored.Total)
	}
	if scored.ClassResults["site"].Total != 5 || scored.ClassResults["cdn"].Total != 3 {
		t.Errorf("scored classes = %+v", scored.ClassResults)
	}
	if scored.FailedNames != "scored-site" || scored.UnscoredFailedNames != "probation-site,blocked-site" {
		t.Errorf("scored names = failed %q unscored %q", scored.FailedNames, scored.UnscoredFailedNames)
	}
	if health.Total != 10 || health.ClassResults["site"].Total != 6 {
		t.Fatal("scoring changed the submitted run")
	}

	// from another country the blocked site counts
	scored = ScoreProviderEgressHealth(health, ProviderEgressPlace{CountryCode: "de"}, scoring)
	if scored.Total != 9 || scored.FailedNames != "blocked-site,scored-site" {
		t.Errorf("scored from de = total %d failed %q", scored.Total, scored.FailedNames)
	}
}

// A settings block overrides the defaults key by key, a class map entry by
// entry, and is refused whole when one value is wrong.
func TestProviderEgressSiteSettingsFromYamlOverlaysTheDefaults(t *testing.T) {
	unmarshalYaml := func(text string) func(any) error {
		return func(out any) error {
			return yaml.Unmarshal([]byte(text), out)
		}
	}
	settings, err := ProviderEgressSiteSettingsFromYaml(unmarshalYaml(`
settings:
  site_retire_share: 0.6
  site_pool_size: {site: 120}
`))
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	defaults := DefaultProviderEgressSiteSettings()
	if settings.SiteRetireShare != 0.6 || settings.SitePoolSize["site"] != 120 {
		t.Fatalf("overrides were not applied: %+v", settings)
	}
	if settings.SitePoolSize["dns"] != defaults.SitePoolSize["dns"] || settings.SiteMinSamples != defaults.SiteMinSamples {
		t.Fatalf("an unnamed value lost its default: %+v", settings)
	}
	for _, text := range []string{
		"settings:\n  site_retire_share: 1.5\n",
		"settings:\n  site_min_samples: 0\n",
		"settings:\n  site_pool_size: {cdn: 0}\n",
		"settings:\n  site_tally_retention_seconds: 60\n",
		"settings:\n  site_refresh_max_time_seconds: 0\n",
	} {
		if _, err := ProviderEgressSiteSettingsFromYaml(unmarshalYaml(text)); err == nil {
			t.Errorf("settings %q were accepted", strings.TrimSpace(text))
		}
	}
}

// A name keys a row and a failed-name list, so it may carry neither commas
// nor spaces.
func TestValidateProviderEgressDestinationNameRefusesWhatAListCannotCarry(t *testing.T) {
	for _, name := range []string{"news-site", "a1"} {
		if err := ValidateProviderEgressDestinationName(name); err != nil {
			t.Errorf("%q refused: %v", name, err)
		}
	}
	for _, name := range []string{"", " ", "news,site", "news site", " news", strings.Repeat("a", 129)} {
		if err := ValidateProviderEgressDestinationName(name); err == nil {
			t.Errorf("%q accepted", name)
		}
	}
}

// A test row: an active, scored site at a .example host.
func testDestinationRow(name string) *ProviderEgressDestination {
	return &ProviderEgressDestination{
		Name:     name,
		Class:    "site",
		Url:      "https://" + name + ".example/robots.txt",
		Expect:   "body",
		MaxBytes: 1024,
		Category: "news",
		Region:   "global",
		Source:   ProviderEgressDestinationSourceBuiltin,
		Active:   true,
	}
}

// Every column survives a write and a read.
func TestProviderEgressDestinationStoresEveryColumn(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Microsecond)
		markedAt := now.Add(-time.Hour)
		share := 0.25
		row := testDestinationRow("stored-site")
		row.Headers = map[string]string{"Accept": "text/plain"}
		row.Verify = ProviderEgressDestinationVerify{Kind: "contains", Text: "User-agent"}
		row.Incompatible = []ProviderEgressDestinationPlace{{Country: "cn"}, {Country: "us", Region: "California", MarkedAt: &markedAt}}
		row.Probation = true
		row.Revision = 2
		row.AddedTime = now.Add(-24 * time.Hour)
		row.PromotedTime = &now
		row.RetireCount = 1
		row.RetireReason = "failed 60.0% of 250 healthy exits"
		row.FailureShare = &share
		row.SampleCount = 250
		row.JudgedTime = &now
		SetProviderEgressDestination(ctx, row)

		rows := GetProviderEgressDestinations(ctx)
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		got := rows[0]
		if got.Name != row.Name || got.Url != row.Url || got.Expect != "body" || got.MaxBytes != 1024 ||
			got.Headers["Accept"] != "text/plain" || got.Verify != row.Verify ||
			got.Category != "news" || got.Region != "global" || got.Revision != 2 ||
			!got.Active || !got.Probation || got.RetireCount != 1 || got.RetireReason != row.RetireReason ||
			got.SampleCount != 250 || got.FailureShare == nil || *got.FailureShare != 0.25 {
			t.Fatalf("stored row = %+v", got)
		}
		if !got.AddedTime.Equal(row.AddedTime) || got.PromotedTime == nil || !got.PromotedTime.Equal(now) || got.JudgedTime == nil || got.RetiredTime != nil {
			t.Fatalf("stored times = added %s promoted %v judged %v retired %v", got.AddedTime, got.PromotedTime, got.JudgedTime, got.RetiredTime)
		}
		if len(got.Incompatible) != 2 || got.Incompatible[0].MarkedAt != nil ||
			got.Incompatible[1].MarkedAt == nil || !got.Incompatible[1].MarkedAt.Equal(markedAt) {
			t.Fatalf("stored places = %+v", got.Incompatible)
		}
	})
}

// The seed fills an empty pool once, every row active and scored, and a pool
// with any row is never seeded again.
func TestSeedProviderEgressDestinationsFillsOnlyAnEmptyPool(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		first := testDestinationRow("first-site")
		first.Active = false
		first.Probation = true
		if !SeedProviderEgressDestinations(ctx, []*ProviderEgressDestination{first, testDestinationRow("second-site")}) {
			t.Fatal("an empty pool was not seeded")
		}
		if SeedProviderEgressDestinations(ctx, []*ProviderEgressDestination{testDestinationRow("third-site")}) {
			t.Fatal("a seeded pool was seeded again")
		}
		rows := GetProviderEgressDestinations(ctx)
		if len(rows) != 2 || rows[0].Name != "first-site" || rows[1].Name != "second-site" {
			t.Fatalf("pool = %d rows, want the first seed alone", len(rows))
		}
		for _, row := range rows {
			if !row.Scored() || row.Source != ProviderEgressDestinationSourceBuiltin {
				t.Errorf("a seeded row is not active, scored and builtin: %+v", row)
			}
		}
	})
}

// A new candidate arrives inactive; a higher revision refreshes a row's
// contract and clears its retirement history but keeps its pool state and its
// learned places; the same revision changes nothing.
func TestSyncProviderEgressDestinationCandidatesFollowsTheRevision(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		markedAt := server.NowUtc().Add(-time.Hour)
		retiredAt := server.NowUtc().Add(-24 * time.Hour)
		builtin := testDestinationRow("builtin-site")
		builtin.Incompatible = []ProviderEgressDestinationPlace{{Country: "ir"}, {Country: "cn", MarkedAt: &markedAt}}
		SetProviderEgressDestination(ctx, builtin)
		dropped := testDestinationRow("dropped-site")
		dropped.Active = false
		dropped.RetiredTime = &retiredAt
		dropped.RetireCount = 3
		dropped.RetireReason = "failed"
		SetProviderEgressDestination(ctx, dropped)

		fresh := testDestinationRow("fresh-site")
		fresh.Revision = 1
		correction := testDestinationRow("builtin-site")
		correction.Url = "https://corrected.example/robots.txt"
		correction.Incompatible = []ProviderEgressDestinationPlace{{Country: "kp"}}
		correction.Revision = 1
		readded := testDestinationRow("dropped-site")
		readded.Revision = 1
		changed := SyncProviderEgressDestinationCandidates(ctx, []*ProviderEgressDestination{fresh, correction, readded})
		if strings.Join(changed, ",") != "builtin-site,dropped-site,fresh-site" {
			t.Fatalf("changed = %v", changed)
		}

		byName := map[string]*ProviderEgressDestination{}
		for _, row := range GetProviderEgressDestinations(ctx) {
			byName[row.Name] = row
		}
		if row := byName["fresh-site"]; row.Active || row.Source != ProviderEgressDestinationSourceCandidates {
			t.Errorf("a new candidate = %+v, want an inactive candidate", row)
		}
		row := byName["builtin-site"]
		if !row.Active || row.Url != "https://corrected.example/robots.txt" || row.Revision != 1 {
			t.Errorf("the corrected row = %+v, want its new contract and its pool state", row)
		}
		if len(row.Incompatible) != 2 || row.Incompatible[0].Country != "kp" || row.Incompatible[1].Country != "cn" {
			t.Errorf("the corrected row's places = %+v, want the declared kp and the learned cn", row.Incompatible)
		}
		if row := byName["dropped-site"]; row.RetireCount != 0 || row.RetiredTime != nil || row.RetireReason != "" || row.Active {
			t.Errorf("the re-added row = %+v, want a fresh candidate", row)
		}

		if changed := SyncProviderEgressDestinationCandidates(ctx, []*ProviderEgressDestination{fresh, correction, readded}); len(changed) != 0 {
			t.Fatalf("the same revisions changed %v", changed)
		}
	})
}

// One run adds each of its loads once to its site's row at its place, a
// canary only to the canary counts, and the run to its place's row; days
// before the retention are dropped.
func TestAddProviderEgressRunTallyCountsEachLoadOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		measuredAt := time.Date(2026, time.September, 20, 13, 0, 0, 0, time.UTC)
		run := ProviderEgressRunTally{Place: ProviderEgressPlace{CountryCode: "DE", Region: "Berlin"}, Healthy: true}
		loads := []ProviderEgressSiteLoad{
			{Name: "passing-site", Ok: true, Healthy: true},
			{Name: "failing-site", Healthy: true},
			{Name: "unhealthy-site"},
			{Name: "canary-site", Ok: true, Canary: true},
			{Name: ""},
		}
		AddProviderEgressRunTally(ctx, measuredAt, run, loads)
		AddProviderEgressRunTally(ctx, measuredAt.Add(time.Hour), ProviderEgressRunTally{Place: run.Place, EchoFailed: true}, loads[:1])

		byName := map[string]ProviderEgressSiteTally{}
		for _, tally := range GetProviderEgressSiteTallies(ctx, measuredAt) {
			if tally.Place.CountryCode != "de" || tally.Place.Region != "Berlin" {
				t.Fatalf("tally place = %+v, want de/Berlin", tally.Place)
			}
			byName[tally.Name] = tally
		}
		if len(byName) != 4 {
			t.Fatalf("tallied sites = %v, want four", byName)
		}
		for name, want := range map[string]ProviderEgressSiteTally{
			"passing-site":   {LoadCount: 2, HealthyLoadCount: 2},
			"failing-site":   {LoadCount: 1, FailureCount: 1, HealthyLoadCount: 1, HealthyFailureCount: 1},
			"unhealthy-site": {LoadCount: 1, FailureCount: 1},
			"canary-site":    {CanaryLoadCount: 1, CanaryPassCount: 1},
		} {
			got := byName[name]
			want.Name = name
			want.Place = got.Place
			if got != want {
				t.Errorf("%s tally = %+v, want %+v", name, got, want)
			}
		}
		places := GetProviderEgressPlaceTallies(ctx, measuredAt)
		if len(places) != 1 || places[0].RunCount != 2 || places[0].HealthyRunCount != 1 || places[0].EchoFailureCount != 1 {
			t.Fatalf("place tallies = %+v, want two runs, one healthy, one echo failure", places)
		}

		RemoveExpiredProviderEgressTallies(ctx, measuredAt.Add(24*time.Hour))
		if 0 < len(GetProviderEgressSiteTallies(ctx, measuredAt)) || 0 < len(GetProviderEgressPlaceTallies(ctx, measuredAt)) {
			t.Fatal("expired tally days were kept")
		}
	})
}

// The fleet-wide class totals are over the scored class tallies of the runs
// measured since the given time.
func TestGetProviderEgressHealthClassTotalsSumsRecentRuns(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		for _, health := range []*ProviderEgressHealth{
			{MeasuredAt: now, ClassResults: map[string]ProviderEgressHealthClassResult{"site": {OK: 20, Total: 26}, "cdn": {OK: 9, Total: 10}}},
			{MeasuredAt: now.Add(-10 * time.Minute), ClassResults: map[string]ProviderEgressHealthClassResult{"site": {OK: 26, Total: 26}}},
			{MeasuredAt: now.Add(-2 * time.Hour), ClassResults: map[string]ProviderEgressHealthClassResult{"site": {OK: 0, Total: 26}}},
		} {
			health.ClientId = server.NewId()
			SetProviderEgressHealth(ctx, health)
		}
		totals := GetProviderEgressHealthClassTotals(ctx, now.Add(-time.Hour))
		if totals["site"] != (ProviderEgressClassTotal{Ok: 46, Total: 52}) ||
			totals["cdn"] != (ProviderEgressClassTotal{Ok: 9, Total: 10}) ||
			totals[""] != (ProviderEgressClassTotal{Ok: 55, Total: 62}) {
			t.Fatalf("class totals = %+v", totals)
		}
	})
}
