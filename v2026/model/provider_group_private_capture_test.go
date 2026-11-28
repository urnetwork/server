package model

import (
	"bytes"
	"context"
	"encoding/gob"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/internal/privateprovidercapture"
)

func privateGroupObservation(t testing.TB, args *FindProviders2Args) (*findProviders2SelectionObservation, *privateprovidercapture.Recorder, context.Context) {
	t.Helper()
	r := privateprovidercapture.NewRecorder(context.Background())
	t.Cleanup(r.Close)
	if r.Arm(strings.Repeat("c", 32)) != "armed" {
		t.Fatal("synthetic private arm failed")
	}
	var ctx context.Context
	r.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) { ctx = request.Context() })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/network/find-providers2", nil))
	observation := newFindProviders2SelectionObservation(args)
	observation.beginPrivateCapture(ctx)
	observation.discovery = true
	observation.requestedCount = 20
	observation.privateGroupCount = len(args.Specs)
	return observation, r, ctx
}

func TestProviderGroupPrivateCaptureCompletedMissingBoundary(t *testing.T) {
	group, caller := server.NewId(), server.NewId()
	for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
		observation, r, ctx := privateGroupObservation(t, &FindProviders2Args{Specs: []*ProviderSpec{{LocationGroupId: &group}}, RankMode: mode})
		observation.load.privateSource = "alternate_legacy"
		otherMode, _ := backfillRankMode(mode)
		observation.load.missingTarget(nil, caller, mode)
		observation.load.missingTarget(&group, caller, otherMode)
		if r.Inspect(strings.Repeat("c", 32), false).Retained != 0 {
			t.Fatal("reader captured before successful completion")
		}
		observation.complete(0)
		observation.finish(ctx)
		out := r.Inspect(strings.Repeat("c", 32), true)
		if out.State != "complete" || len(out.Records) != 1 {
			t.Fatal("completed missing group was not privately captured")
		}
		record := out.Records[0]
		if record.GroupID != group.String() || record.CallerLocationID != caller.String() || record.LoadRank != otherMode || record.RequestRank != mode || record.Source != "alternate_legacy" || record.MissingTargets != 2 {
			t.Fatal("actual missing group/caller/source was replaced")
		}
	}
}

func TestProviderGroupPrivateCaptureHealthyFailedAndUnknownControls(t *testing.T) {
	group, caller, other := server.NewId(), server.NewId(), server.NewId()
	for _, kind := range []string{"nonempty", "error", "canceled", "page_gap", "cache_empty", "location", "mixed", "forced_minimum", "count_zero", "unsupported_rank", "v6", "no_actual_group"} {
		args := &FindProviders2Args{Specs: []*ProviderSpec{{LocationGroupId: &group}}}
		switch kind {
		case "location":
			args.Specs = []*ProviderSpec{{LocationId: &other}}
		case "mixed":
			args.Specs = append(args.Specs, &ProviderSpec{LocationId: &other})
		case "forced_minimum":
			args.ForceMinimum = true
		case "count_zero":
			args.ForceCount = true
		case "unsupported_rank":
			args.RankMode = "synthetic-unknown"
		case "v6":
			args.IpFamily = "v6"
		}
		observation, r, ctx := privateGroupObservation(t, args)
		observation.load.privateSource = "primary_legacy"
		if kind == "no_actual_group" {
			observation.load.missingTarget(nil, caller, RankModeQuality)
		} else if kind != "cache_empty" {
			observation.load.missingTarget(&group, caller, RankModeQuality)
		}
		if kind == "page_gap" {
			observation.load.missingPages = 1
		}
		if kind == "canceled" {
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			ctx = canceled
		}
		if kind != "error" && kind != "canceled" {
			count := 0
			if kind == "nonempty" {
				count = 20
			}
			observation.complete(count)
		}
		observation.finish(ctx)
		if out := r.Inspect(strings.Repeat("c", 32), true); out.State != "no_sample_yet" || out.Records != nil {
			t.Fatalf("%s manufactured affected tuple", kind)
		}
	}
}

// The actual Redis reader, including the native-to-legacy fallback, supplies
// the tuple. The unaffected group has valid empty metadata, so the sample must
// select the absent group regardless of map order, never just the first spec.
func TestProviderGroupPrivateCaptureReaderBoundary(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		caller, missing, present := server.NewId(), server.NewId(), server.NewId()
		for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
			var empty bytes.Buffer
			if err := gob.NewEncoder(&empty).Encode([]int{}); err != nil {
				t.Fatal(err)
			}
			server.Redis(t.Context(), func(r server.RedisClient) {
				if err := r.Set(t.Context(), clientScoreLocationGroupCountsKey(false, mode, present, caller), empty.Bytes(), time.Minute).Err(); err != nil {
					t.Fatal(err)
				}
			})
			for _, native := range []bool{false, true} {
				args := &FindProviders2Args{Specs: []*ProviderSpec{{LocationGroupId: &present}, {LocationGroupId: &missing}}, RankMode: mode}
				observation, r, ctx := privateGroupObservation(t, args)
				observation.load.privateSource = "primary_legacy"
				scores, _, err := loadPreferredClientScoresWithCursor(native, false, mode, ctx, nil, map[server.Id]bool{present: true, missing: true}, caller, 100, []ipFamilyFacet{ipFamilyFacetDualstack, ipFamilyFacetV4Only}, &observation.load)
				if err != nil || len(scores) != 0 || observation.load.missingTargets != 1 {
					t.Fatalf("reader boundary changed: native=%t mode=%s", native, mode)
				}
				observation.complete(len(scores))
				observation.finish(ctx)
				out := r.Inspect(strings.Repeat("c", 32), true)
				if out.State != "complete" || len(out.Records) != 1 || out.Records[0].GroupID != missing.String() || out.Records[0].CallerLocationID != caller.String() || out.Records[0].RequestedGroups != 2 {
					t.Fatal("reader lost exact absent target/caller provenance")
				}
			}
		}
	})
}
