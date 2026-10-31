package model

import (
	"context"
	"io/fs"
	"maps"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/stats"
	"github.com/urnetwork/server/stats/sample"
)

// recordFindProviders2Sample writes a faithful, anonymized sample: the pool in
// scaled-weight order (capped, with pool_count preserved) and the chosen ids.
// Runs without a database — the pool is constructed directly.
func TestFindProviders2SampleExport(t *testing.T) {
	siteHome := t.TempDir()
	t.Setenv("WARP_SITE_HOME", siteHome)
	t.Setenv("WARP_ENV", "local") // raw ids, so chosen round-trips to the input
	t.Setenv("WARP_SERVICE", "testfp")
	t.Setenv("WARP_HOST", "testhost")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := stats.Enable(ctx, nil)
	connect.AssertEqual(t, s.Enabled(), true)

	rankMode := RankModeQuality
	clientScores := map[server.Id]*ClientScore{}
	ids := []server.Id{}
	for i := 0; i < 4; i += 1 {
		clientId := server.NewId()
		ids = append(ids, clientId)
		clientScores[clientId] = &ClientScore{
			ClientId:                     clientId,
			NetworkId:                    server.NewId(),
			ScaledWeights:                map[string]float32{rankMode: float32(i + 1)},
			Tiers:                        map[string]int{rankMode: i},
			Scores:                       map[string]int{rankMode: 10 * i},
			ReliabilityWeight:            float64(i),
			IndependentReliabilityWeight: 0.9,
			MaxBytesPerSecond:            ByteCount(1000 * (i + 1)),
			HasSpeedTest:                 true,
		}
	}
	// chosen = the two highest-weight ids
	chosen := []server.Id{ids[3], ids[2]}

	recordFindProviders2Sample(
		s,
		&FindProviders2Args{
			Count:    2,
			RankMode: rankMode,
			Specs: []*ProviderSpec{
				{LocationId: &ids[0]},
			},
		},
		rankMode,
		2,
		"zz",
		12.5,
		clientScores,
		chosen,
	)

	waitForFindProviders2SampleJobs()
	s.Close()

	// find the single segment
	var segmentPath string
	filepath.WalkDir(siteHome, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".pb.zst") {
			segmentPath = path
		}
		return nil
	})
	if segmentPath == "" {
		t.Fatalf("no segment written under %s", siteHome)
	}

	samples := []*sample.FindProviders2Sample{}
	err := stats.ReadSegment(segmentPath, func(frame []byte) error {
		var fp sample.FindProviders2Sample
		if err := proto.Unmarshal(frame, &fp); err != nil {
			return err
		}
		samples = append(samples, &fp)
		return nil
	})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(samples), 1)

	fp := samples[0]
	connect.AssertEqual(t, fp.CallerCountryCode, "zz")
	connect.AssertEqual(t, fp.RankMode, rankMode)
	connect.AssertEqual(t, int(fp.PoolCount), 4)
	connect.AssertEqual(t, int(fp.EffectiveCount), 2)
	connect.AssertEqual(t, int(fp.SpecLocationCount), 1)
	connect.AssertEqual(t, len(fp.Candidates), 4)
	// pool is weight-descending: first candidate is the highest weight (i=3)
	connect.AssertEqual(t, fp.Candidates[0].ScaledWeight, float32(4))
	connect.AssertEqual(t, fp.Candidates[3].ScaledWeight, float32(1))
	// raw (local) anonymization: chosen ids round-trip to the input ids
	connect.AssertEqual(t, len(fp.ChosenClientIds), 2)
	connect.AssertEqual(t, fp.ChosenClientIds[0], ids[3].Bytes())
	connect.AssertEqual(t, fp.ChosenClientIds[1], ids[2].Bytes())
}

func TestFindProviders2OutcomeLabelsAreBoundedAndClassifyProductResult(t *testing.T) {
	locationID := server.NewId()
	groupID := server.NewId()
	for _, tc := range []struct {
		name         string
		args         *FindProviders2Args
		country      string
		wantFamily   string
		wantLocation string
		wantBand     string
	}{
		{
			name: "ordinary IPv6 group is a small list",
			args: &FindProviders2Args{
				Specs:    []*ProviderSpec{{LocationGroupId: &groupID}},
				IpFamily: "v6-capable",
			},
			country: "US", wantFamily: "v6", wantLocation: "group", wantBand: "1-2",
		},
		{
			name: "mixed request with malformed country remains bounded",
			args: &FindProviders2Args{
				Specs: []*ProviderSpec{{LocationId: &locationID}, {BestAvailable: true}},
			},
			country: "not-a-country", wantFamily: "any", wantLocation: "mixed", wantBand: "0",
		},
		{
			name:    "fixed client request is direct",
			args:    &FindProviders2Args{IpFamily: "v4-only"},
			country: "ca", wantFamily: "v4", wantLocation: "direct", wantBand: "10+",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := findProviders2OutcomeIpFamily(tc.args.IpFamily); got != tc.wantFamily {
				t.Fatalf("family = %q, want %q", got, tc.wantFamily)
			}
			if got := findProviders2OutcomeLocationKind(tc.args); got != tc.wantLocation {
				t.Fatalf("location kind = %q, want %q", got, tc.wantLocation)
			}
			resultCount := 0
			switch tc.wantBand {
			case "1-2":
				resultCount = 2
			case "10+":
				resultCount = 10
			}
			if got := findProviders2OutcomeCountBand(resultCount); got != tc.wantBand {
				t.Fatalf("result band = %q, want %q", got, tc.wantBand)
			}
		})
	}
}

func TestRecordFindProviders2OutcomeRecordsReturnedCountBand(t *testing.T) {
	groupID := server.NewId()
	args := &FindProviders2Args{
		Specs:        []*ProviderSpec{{LocationGroupId: &groupID}},
		RankMode:     RankModeSpeed,
		IpFamily:     "v6-capable",
		ForceMinimum: false,
	}
	counter, err := findProviders2OutcomesTotal.GetMetricWithLabelValues(
		"v6", "group", "us", RankModeSpeed, "false", "1-2", "none",
	)
	if err != nil {
		t.Fatal(err)
	}
	before := testutil.ToFloat64(counter)
	recordFindProviders2Outcome(args, "US", 2)
	if got := testutil.ToFloat64(counter); got != before+1 {
		t.Fatalf("outcome counter = %v, want %v", got, before+1)
	}
}

// Every completed empty answer the outcome counter's zero band counts is also
// counted, under the same request-class labels, with the requested target's
// kind and the request's terminal selection reason, so the provider-count
// probe (SIGNALS.md §2.9a) can tell the zeros a request explains from the
// platform's. An empty specification runs without a database; its zero is the
// request's own (no_specs).
func TestFindProviders2EmptyOutcomeExplainsTheOutcomeZeroBand(t *testing.T) {
	cohort := map[string]string{"ip_family": "any", "location_kind": "direct", "caller_country": "unknown", "rank_mode": "quality", "force_minimum": "false", "capability": "none"}
	zeroBand := maps.Clone(cohort)
	zeroBand["result_count"] = "0"
	explained := maps.Clone(cohort)
	explained["target_kind"] = "none"
	explained["reason"] = "no_specs"
	outcomesBefore := selectionMetricCount(t, "urnetwork_findproviders2_outcomes_total", zeroBand)
	explainedBefore := selectionMetricCount(t, "urnetwork_findproviders2_empty_outcomes_total", explained)
	result, err := FindProviders2(&FindProviders2Args{}, &session.ClientSession{Ctx: context.Background()})
	if err != nil || result == nil || len(result.Providers) != 0 {
		t.Fatalf("empty request changed its existing result: error=%v", err)
	}
	if after := selectionMetricCount(t, "urnetwork_findproviders2_outcomes_total", zeroBand); after != outcomesBefore+1 {
		t.Fatalf("outcome zero band = %g, want %g", after, outcomesBefore+1)
	}
	if after := selectionMetricCount(t, "urnetwork_findproviders2_empty_outcomes_total", explained); after != explainedBefore+1 {
		t.Fatalf("empty-outcome explanation = %g, want %g", after, explainedBefore+1)
	}
}

// The explanation's request-class labels are the outcome counter's, value for
// value, including the caller-country normalization, so the two join on them
// alone.
func TestRecordFindProviders2EmptyOutcomeSharesTheOutcomeCohortLabels(t *testing.T) {
	groupID := server.NewId()
	args := &FindProviders2Args{
		Specs:                 []*ProviderSpec{{LocationGroupId: &groupID}},
		RankMode:              RankModeSpeed,
		IpFamily:              "v6-capable",
		RequireModernProvider: true,
	}
	for _, callerCountry := range []string{" US ", "not-a-country"} {
		wantCountry := "us"
		if callerCountry == "not-a-country" {
			wantCountry = "unknown"
		}
		outcome, err := findProviders2OutcomesTotal.GetMetricWithLabelValues("v6", "group", wantCountry, RankModeSpeed, "false", "0", "modern")
		if err != nil {
			t.Fatal(err)
		}
		explained, err := findProviders2EmptyOutcomesTotal.GetMetricWithLabelValues("v6", "group", wantCountry, RankModeSpeed, "false", "modern", "group", "cache_missing")
		if err != nil {
			t.Fatal(err)
		}
		outcomeBefore, explainedBefore := testutil.ToFloat64(outcome), testutil.ToFloat64(explained)
		recordFindProviders2Outcome(args, callerCountry, 0)
		recordFindProviders2EmptyOutcome(args, callerCountry, "group", "cache_missing")
		if got := testutil.ToFloat64(outcome); got != outcomeBefore+1 {
			t.Fatalf("%q: outcome zero band = %v, want %v", callerCountry, got, outcomeBefore+1)
		}
		if got := testutil.ToFloat64(explained); got != explainedBefore+1 {
			t.Fatalf("%q: explanation = %v, want %v", callerCountry, got, explainedBefore+1)
		}
	}
}

func TestTopFindProviders2CandidatesDoesNotSortWholePool(t *testing.T) {
	rankMode := RankModeQuality
	clientScores := map[server.Id]*ClientScore{}
	for i := 0; i < 100; i++ {
		clientId := server.NewId()
		clientScores[clientId] = &ClientScore{
			ScaledWeights: map[string]float32{rankMode: float32(i)},
		}
	}
	top := topFindProviders2Candidates(clientScores, rankMode, 5)
	if len(top) != 5 {
		t.Fatalf("retained %d candidates, want 5", len(top))
	}
	for i, want := range []float32{99, 98, 97, 96, 95} {
		if top[i].weight != want {
			t.Fatalf("top[%d].weight = %v, want %v", i, top[i].weight, want)
		}
	}
}
