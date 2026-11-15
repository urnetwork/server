package model

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func TestNativeCensusRatioExactConfiguredBoundaries(t *testing.T) {
	now := time.Date(2026, 9, 30, 23, 0, 0, 0, time.UTC)
	maxInt := int(^uint(0) >> 1)
	firstPassing := maxInt/5*3 + (maxInt%5*3+4)/5
	for _, tc := range []struct {
		name                   string
		ok, total, num, den    int
		passed, failed, absent int
	}{
		{"one success is enough", 1, 1, 3, 5, 1, 0, 0},
		{"one failure", 0, 1, 3, 5, 0, 1, 0},
		{"exact equality", 3, 5, 3, 5, 1, 0, 0},
		{"below equality", 2, 5, 3, 5, 0, 1, 0},
		{"configured bar", 3, 5, 4, 5, 0, 1, 0},
		{"four fifths equality", 4, 5, 4, 5, 1, 0, 0},
		{"below four fifths", 79, 100, 4, 5, 0, 1, 0},
		{"zero bar observed", 0, 1, 0, 5, 1, 0, 0},
		{"zero bar no evidence", 0, 0, 0, 5, 0, 0, 1},
		{"large exact pass", firstPassing, maxInt, 3, 5, 1, 0, 0},
		{"large exact fail", firstPassing - 1, maxInt, 3, 5, 0, 1, 0},
		{"both products overflow", maxInt - 1, maxInt, maxInt - 1, maxInt, 1, 0, 0},
		{"both products overflow below", maxInt - 2, maxInt, maxInt - 1, maxInt, 0, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := server.NewId()
			settings := DefaultEgressIndexSettings()
			settings.QualityOkNumerator, settings.QualityOkDenominator = tc.num, tc.den
			ratio := newClientScoreNativeEgressRatio(now, now.Add(time.Minute), now, settings,
				map[server.Id]ProviderEgressHealthCounts{id: {OKCount: tc.ok, Total: tc.total}}, map[server.Id]bool{id: true})
			want := ClientScoreNativeEgressRatioCount{Providers: 1, Observed: tc.passed + tc.failed, Passed: tc.passed, Failed: tc.failed, NoEvidence: tc.absent}
			if ratio.UnavailableReason != "" || ratio.PublicOnline == nil || ratio.SourceMap == nil || *ratio.PublicOnline != want || *ratio.SourceMap != want || ratio.OKNumerator != tc.num || ratio.OKDenominator != tc.den {
				t.Fatalf("configured exact ratio result=%+v, want=%+v", ratio, want)
			}
		})
	}
}

func TestNativeCensusRatioSeparatesPublicOnlineAndSourceMap(t *testing.T) {
	pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte("egress_index:\n  quality_ok_numerator: 3\n  quality_ok_denominator: 5\n"))
	defer pop()
	started := time.Date(2026, 9, 30, 23, 0, 0, 0, time.UTC)
	windowEnd := started.Add(3 * time.Second)
	health := map[server.Id]ProviderEgressHealthCounts{}
	scores := map[server.Id]*ClientScore{}
	add := func(online, private bool, counts *ProviderEgressHealthCounts) {
		score := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
		score.Online, score.NetworkOnly = online, private
		// A ratio pass does not imply the other native admission gates passed.
		score.PassesMinimums = map[RankMode]bool{}
		scores[score.ClientId] = score
		if counts != nil {
			health[score.ClientId] = *counts
		}
	}
	add(true, false, &ProviderEgressHealthCounts{OKCount: 3, Total: 5})
	add(true, false, &ProviderEgressHealthCounts{OKCount: 2, Total: 5})
	add(true, false, nil)
	add(true, false, &ProviderEgressHealthCounts{})
	add(true, true, &ProviderEgressHealthCounts{OKCount: 1, Total: 1})
	add(false, false, &ProviderEgressHealthCounts{OKCount: 1, Total: 1})
	health[server.NewId()] = ProviderEgressHealthCounts{Total: 1}
	census := newClientScoreNativeCensus(started, started.Add(time.Minute), windowEnd, health,
		map[server.Id]map[server.Id]*ClientScore{server.NewId(): scores, server.NewId(): scores},
		map[server.Id]map[server.Id]*ClientScore{server.NewId(): scores})
	wantOnline := ClientScoreNativeEgressRatioCount{Providers: 4, Observed: 2, Passed: 1, Failed: 1, NoEvidence: 2}
	wantSource := ClientScoreNativeEgressRatioCount{Providers: 6, Observed: 5, Passed: 3, Failed: 2, NoEvidence: 1}
	ratio := census.EgressRatio
	if ratio == nil || ratio.PublicOnline == nil || ratio.SourceMap == nil || *ratio.PublicOnline != wantOnline || *ratio.SourceMap != wantSource {
		t.Fatalf("target overlap, privacy or source-map scope lost: %+v", ratio)
	}
	if census.Buckets[RankModeQuality].Providers != 0 || census.Buckets[RankModeSpeed].Providers != 0 || census.Buckets["online"].Providers != 4 {
		t.Fatalf("ratio changed native membership: %+v", census.Buckets)
	}
	if !ratio.WindowEndInclusive.Equal(windowEnd) || !ratio.WindowStartExclusive.Equal(windowEnd.Add(-8*time.Hour)) || !census.PublishedAt.IsZero() || validateClientScoreNativeEgressRatio(census) != nil {
		t.Fatalf("query window replaced with export time: %+v", ratio)
	}
}

func TestNativeCensusRatioUsesPublicationConfiguration(t *testing.T) {
	pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte("egress_index:\n  quality_ok_numerator: 3\n  quality_ok_denominator: 5\n"))
	defer pop()
	now := time.Date(2026, 9, 30, 23, 0, 0, 0, time.UTC)
	score := nativeTestScore(RankModeSpeed, ipFamilyFacetV4Only)
	score.Online = true
	census := newClientScoreNativeCensus(now, now.Add(time.Minute), now,
		map[server.Id]ProviderEgressHealthCounts{score.ClientId: {OKCount: 3, Total: 5}},
		map[server.Id]map[server.Id]*ClientScore{server.NewId(): {score.ClientId: score}})
	if ratio := census.EgressRatio; ratio == nil || ratio.OKNumerator != 3 || ratio.OKDenominator != 5 || ratio.PublicOnline == nil || ratio.PublicOnline.Passed != 1 || ratio.PublicOnline.Failed != 0 {
		t.Fatalf("publication ignored configured threshold: %+v", ratio)
	}
}

func TestNativeCensusRatioInvalidSourceRemainsUnavailable(t *testing.T) {
	now := time.Date(2026, 9, 30, 23, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, reason string
		change       func(*EgressIndexSettings, map[server.Id]ProviderEgressHealthCounts, *time.Time)
	}{
		{"negative successes", "health_counts_invalid", func(_ *EgressIndexSettings, h map[server.Id]ProviderEgressHealthCounts, _ *time.Time) {
			h[server.NewId()] = ProviderEgressHealthCounts{OKCount: -1, Total: 1}
		}},
		{"negative total", "health_counts_invalid", func(_ *EgressIndexSettings, h map[server.Id]ProviderEgressHealthCounts, _ *time.Time) {
			h[server.NewId()] = ProviderEgressHealthCounts{Total: -1}
		}},
		{"success above total", "health_counts_invalid", func(_ *EgressIndexSettings, h map[server.Id]ProviderEgressHealthCounts, _ *time.Time) {
			h[server.NewId()] = ProviderEgressHealthCounts{OKCount: 2, Total: 1}
		}},
		{"zero denominator", "ratio_configuration_invalid", func(s *EgressIndexSettings, _ map[server.Id]ProviderEgressHealthCounts, _ *time.Time) {
			s.QualityOkDenominator = 0
		}},
		{"negative numerator", "ratio_configuration_invalid", func(s *EgressIndexSettings, _ map[server.Id]ProviderEgressHealthCounts, _ *time.Time) {
			s.QualityOkNumerator = -1
		}},
		{"ratio above one", "ratio_configuration_invalid", func(s *EgressIndexSettings, _ map[server.Id]ProviderEgressHealthCounts, _ *time.Time) {
			s.QualityOkNumerator = 6
		}},
		{"missing source clock", "source_window_invalid", func(_ *EgressIndexSettings, _ map[server.Id]ProviderEgressHealthCounts, end *time.Time) {
			*end = time.Time{}
		}},
		{"source before generation", "source_window_invalid", func(_ *EgressIndexSettings, _ map[server.Id]ProviderEgressHealthCounts, end *time.Time) {
			*end = now.Add(-time.Nanosecond)
		}},
		{"source after completion", "source_window_invalid", func(_ *EgressIndexSettings, _ map[server.Id]ProviderEgressHealthCounts, end *time.Time) {
			*end = now.Add(time.Minute + time.Nanosecond)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings, windowEnd := DefaultEgressIndexSettings(), now
			id := server.NewId()
			health := map[server.Id]ProviderEgressHealthCounts{id: {OKCount: 1, Total: 1}}
			tc.change(settings, health, &windowEnd)
			ratio := newClientScoreNativeEgressRatio(now, now.Add(time.Minute), windowEnd, settings, health, map[server.Id]bool{id: true})
			if ratio.UnavailableReason != tc.reason || ratio.PublicOnline != nil || ratio.SourceMap != nil {
				t.Fatalf("invalid source became a partial ratio claim: %+v", ratio)
			}
			if err := validateClientScoreNativeEgressRatio(&ClientScoreNativeCensus{EgressRatio: ratio}); err != nil {
				t.Fatalf("explicit unavailable diagnostic is unreadable: %v", err)
			}
		})
	}
}

func TestNativeCensusRatioReaderRejectsMalformedCountsAndProvenance(t *testing.T) {
	now := time.Date(2026, 9, 30, 23, 0, 0, 0, time.UTC)
	makeCensus := func() *ClientScoreNativeCensus {
		id := server.NewId()
		return &ClientScoreNativeCensus{
			SourceStartedAt: now, SourceCompletedAt: now.Add(time.Minute), PolicyVersion: 1,
			Buckets: map[string]ClientScoreNativeBucketCount{"online": {Providers: 1}},
			EgressRatio: newClientScoreNativeEgressRatio(now, now.Add(time.Minute), now, DefaultEgressIndexSettings(),
				map[server.Id]ProviderEgressHealthCounts{id: {OKCount: 1, Total: 1}}, map[server.Id]bool{id: true}),
		}
	}
	if err := validateClientScoreNativeEgressRatio(makeCensus()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*ClientScoreNativeCensus)
	}{
		{"missing online", func(c *ClientScoreNativeCensus) { c.EgressRatio.PublicOnline = nil }},
		{"missing source", func(c *ClientScoreNativeCensus) { c.EgressRatio.SourceMap = nil }},
		{"wrong cohort", func(c *ClientScoreNativeCensus) { c.Buckets["online"] = ClientScoreNativeBucketCount{Providers: 2} }},
		{"incomplete no evidence", func(c *ClientScoreNativeCensus) { c.EgressRatio.PublicOnline.NoEvidence = 1 }},
		{"negative pass", func(c *ClientScoreNativeCensus) { c.EgressRatio.PublicOnline.Passed = -1 }},
		{"overflow count", func(c *ClientScoreNativeCensus) { c.EgressRatio.PublicOnline.Observed = int(^uint(0) >> 1) }},
		{"public pass exceeds map", func(c *ClientScoreNativeCensus) {
			c.EgressRatio.SourceMap.Passed = 0
			c.EgressRatio.SourceMap.Failed = 1
		}},
		{"unknown policy", func(c *ClientScoreNativeCensus) { c.PolicyVersion = 0 }},
		{"source too early", func(c *ClientScoreNativeCensus) { c.SourceStartedAt = now.Add(time.Second) }},
		{"source too late", func(c *ClientScoreNativeCensus) { c.SourceCompletedAt = now.Add(-time.Second) }},
		{"wrong window width", func(c *ClientScoreNativeCensus) { c.EgressRatio.WindowStartExclusive = now.Add(-4 * time.Hour) }},
		{"bad threshold", func(c *ClientScoreNativeCensus) { c.EgressRatio.OKDenominator = 0 }},
		{"unavailable with counts", func(c *ClientScoreNativeCensus) { c.EgressRatio.UnavailableReason = "health_counts_invalid" }},
		{"unknown reason", func(c *ClientScoreNativeCensus) {
			c.EgressRatio = &ClientScoreNativeEgressRatio{UnavailableReason: "unknown"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			census := makeCensus()
			tc.change(census)
			if err := validateClientScoreNativeEgressRatio(census); err == nil {
				t.Fatal("malformed optional ratio was accepted as complete")
			}
		})
	}
}

func TestNativeCensusRatioCacheCompatibilityAndUnknown(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		started := server.NowUtc().Add(-time.Hour)
		census := newClientScoreNativeCensus(started, started.Add(time.Minute), started, nil)
		if err := writeClientScoreNativeCensus(ctx, census, time.Hour); err != nil {
			t.Fatal(err)
		}
		read, err := GetClientScoreNativeCensus(ctx)
		if err != nil || read == nil || !reflect.DeepEqual(read.EgressRatio, census.EgressRatio) || read.EgressRatio.PublicOnline.Providers != 0 {
			t.Fatalf("complete empty ratio did not survive publication: %v", err)
		}
		encoded, err := json.Marshal(census)
		if err != nil {
			t.Fatal(err)
		}
		var older map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &older); err != nil {
			t.Fatal(err)
		}
		delete(older, "egress_ratio")
		encoded, err = json.Marshal(older)
		if err != nil {
			t.Fatal(err)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, clientScoreNativeCensusKey, encoded, time.Hour).Err())
		})
		read, err = GetClientScoreNativeCensus(ctx)
		if err != nil || read == nil || read.EgressRatio != nil || read.PublicationId != census.PublicationId || !read.SourceStartedAt.Equal(census.SourceStartedAt) || !read.PublishedAt.Equal(census.PublishedAt) {
			t.Fatalf("old publication became zero or lost provenance: %v", err)
		}
		older["egress_ratio"] = json.RawMessage(`{}`)
		encoded, err = json.Marshal(older)
		if err != nil {
			t.Fatal(err)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, clientScoreNativeCensusKey, encoded, time.Hour).Err())
		})
		if read, err := GetClientScoreNativeCensus(ctx); err == nil || read != nil {
			t.Fatal("present incomplete ratio became healthy zero")
		}
		for _, incomplete := range []string{
			`{"window_start_exclusive":"2026-09-30T15:00:00Z","window_end_inclusive":"2026-09-30T23:00:00Z","ok_denominator":5,"public_online":{},"source_map":{}}`,
			`{"window_start_exclusive":"2026-09-30T15:00:00Z","window_end_inclusive":"2026-09-30T23:00:00Z","ok_numerator":0,"ok_denominator":5,"public_online":{"providers":0,"observed":0,"passed":0,"failed":0},"source_map":{"providers":0,"observed":0,"passed":0,"failed":0,"no_evidence":0}}`,
		} {
			var value ClientScoreNativeEgressRatio
			if err := json.Unmarshal([]byte(incomplete), &value); err == nil {
				t.Fatal("missing zero-valued ratio fields became complete")
			}
		}
		census.EgressRatio = &ClientScoreNativeEgressRatio{UnavailableReason: "health_counts_invalid"}
		if err := writeClientScoreNativeCensus(ctx, census, time.Hour); err != nil {
			t.Fatal(err)
		}
		if read, err := GetClientScoreNativeCensus(ctx); err != nil || read == nil || read.EgressRatio.UnavailableReason != "health_counts_invalid" || read.EgressRatio.PublicOnline != nil || read.Buckets["online"].Providers != 0 {
			t.Fatalf("unavailable ratio damaged existing native publication: %v", err)
		}
	})
}

func TestProviderEgressHealthSnapshotPreservesQueryWindowAndPolicy(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now, id := server.NowUtc().Truncate(time.Microsecond), server.NewId()
		policy := SelectedProviderUrlProbePolicyVersion()
		server.Tx(ctx, func(tx server.PgTx) {
			for _, row := range []struct {
				at          time.Time
				ok, n, p    int
				urlAccepted bool
			}{
				{now.Add(-time.Hour), 1, 1, policy, true},
				{now.Add(-time.Minute), 0, 1, policy, false}, // Existing predicate deliberately does not add url_probe.
				{now.Add(-8 * time.Hour), 1, 1, policy, true},
				{now.Add(time.Hour), 1, 1, policy, true},
				{now.Add(-time.Minute), 1, 1, policy + 1, true},
				{now.Add(-time.Minute), 2, 2, policy, true},
			} {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
					(run_id, client_id, measured_at, ok_count, total_count, url_probe_policy_version, url_probe, class_results, tls_authentication_failure)
					VALUES ($1,$2,$3,$4,$5,$6,$7,'{}'::jsonb,false)`, server.NewId(), id, row.at, row.ok, row.n, row.p, row.urlAccepted))
			}
		}, server.OptNoRetry())
		before := server.NowUtc()
		counts, end := getAllProviderEgressHealthCountsSnapshot(ctx)
		after := server.NowUtc()
		if end.Before(before) || after.Before(end) || len(counts) != 1 || counts[id].OKCount != 1 || counts[id].Total != 2 || !counts[id].FirstMeasuredAt.Equal(now.Add(-time.Hour)) || !counts[id].MeasuredAt.Equal(now.Add(-time.Minute)) {
			t.Fatalf("snapshot lost query endpoint or existing policy/window predicate: before=%s end=%s after=%s counts=%+v", before, end, after, counts[id])
		}
		if got := GetAllProviderEgressHealthCounts(ctx); !reflect.DeepEqual(got, counts) {
			t.Fatal("exported map API changed source membership")
		}
	})
}
