// Guards the private, per-submission plot contract and sparse-frame mapping;
// these configuration tests do not claim a browser or live Grafana render.
package grafana

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Reads only the fields needed to distinguish candidate points from references.
type competitionSubmissionPlot struct {
	Id          int    `json:"id"`
	Type        string `json:"type"`
	Description string `json:"description"`
	GridPos     struct {
		W int `json:"w"`
		X int `json:"x"`
		Y int `json:"y"`
	} `json:"gridPos"`
	FieldConfig struct {
		Defaults  map[string]json.RawMessage `json:"defaults"`
		Overrides []struct {
			Matcher struct {
				Id      string `json:"id"`
				Options string `json:"options"`
			} `json:"matcher"`
			Properties []struct {
				Id    string          `json:"id"`
				Value json.RawMessage `json:"value"`
			} `json:"properties"`
		} `json:"overrides"`
	} `json:"fieldConfig"`
	Options struct {
		SeriesMapping string `json:"seriesMapping"`
		Series        []struct {
			Frame *int   `json:"frame"`
			X     string `json:"x"`
			Y     string `json:"y"`
		} `json:"series"`
	} `json:"options"`
	Transformations []struct {
		Id     string `json:"id"`
		Filter struct {
			Id      string `json:"id"`
			Options string `json:"options"`
		} `json:"filter"`
		Options struct {
			Conversions []struct {
				TargetField     string `json:"targetField"`
				DestinationType string `json:"destinationType"`
			} `json:"conversions"`
			Sort []struct {
				Field string `json:"field"`
				Desc  bool   `json:"desc"`
			} `json:"sort"`
			RenameByName map[string]string `json:"renameByName"`
		} `json:"options"`
	} `json:"transformations"`
	Targets []struct {
		RefId   string `json:"refId"`
		Expr    string `json:"expr"`
		Format  string `json:"format"`
		Instant bool   `json:"instant"`
		Range   bool   `json:"range"`
	} `json:"targets"`
}

// A missing plot fails at its identity boundary instead of validating zero data.
func readCompetitionSubmissionPlot(t *testing.T, id int) competitionSubmissionPlot {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("dashboards", "competition.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Panels []competitionSubmissionPlot `json:"panels"`
	}
	if err := json.Unmarshal(content, &document); err != nil {
		t.Fatal(err)
	}
	for _, panel := range document.Panels {
		if panel.Id == id {
			return panel
		}
	}
	t.Fatalf("missing submission panel %d", id)
	return competitionSubmissionPlot{}
}

// Per-job scores must never inherit public sharing or mix independent rounds.
func TestCompetitionSubmissionDashboardIsPrivateAndRoundScoped(t *testing.T) {
	dashboard := readTestDashboard(t, "competition.json")
	if slices.Contains(dashboard.Tags, PublicTag) {
		t.Fatal("per-submission scores must remain authenticated")
	}
	found := false
	for _, raw := range dashboard.Templating.List {
		variable, ok := raw.(map[string]any)
		if ok && variable["name"] == "score_round" {
			found = true
			if variable["multi"] != false || variable["includeAll"] != false {
				t.Fatal("score plots must select exactly one round")
			}
			query, _ := variable["query"].(string)
			if !strings.Contains(query, "submission_score_snapshot_ready") ||
				!strings.Contains(query, "env=\"$env\"") {
				t.Fatal("round choices lost the authenticated score snapshot scope")
			}
		}
	}
	if !found {
		t.Fatal("missing explicit score round selector")
	}
}

// Original submission time, not a scrape/finish timestamp, aligns all panels.
func TestCompetitionSubmissionChartsShareOriginalSubmissionAxis(t *testing.T) {
	previousY := -1
	for _, id := range []int{31, 32, 33, 34} {
		panel := readCompetitionSubmissionPlot(t, id)
		if panel.Type != "xychart" || panel.Options.SeriesMapping != "manual" ||
			panel.GridPos.X != 0 || panel.GridPos.W != 24 || panel.GridPos.Y <= previousY {
			t.Fatalf("panel %d lost aligned, stacked XY layout", id)
		}
		previousY = panel.GridPos.Y
		if len(panel.Options.Series) != len(panel.Targets) || len(panel.Targets) < 5 {
			t.Fatalf("panel %d lost candidate categories or reference series", id)
		}
		for i, series := range panel.Options.Series {
			if series.X != "submitted_at_ms_"+panel.Targets[i].RefId || series.Y == "" || series.Frame != nil {
				t.Fatalf("panel %d depends on a positional frame or wrong x axis: %+v", id, series)
			}
		}
		converted, ordered := false, false
		for _, transformation := range panel.Transformations {
			for _, conversion := range transformation.Options.Conversions {
				converted = converted || transformation.Id == "convertFieldType" &&
					conversion.TargetField == "submitted_at_ms" && conversion.DestinationType == "number"
			}
			for _, sort := range transformation.Options.Sort {
				ordered = ordered || transformation.Id == "sortBy" &&
					sort.Field == "submitted_at_ms" && !sort.Desc
			}
		}
		if !converted || !ordered {
			t.Fatalf("panel %d does not convert and sort the original numeric x coordinate", id)
		}
	}
}

// Manual XY mapping first matches x, then requires y in that same frame.
// Unique x and y display aliases avoid both the shared-x missing-y exception
// and positional shifts when a candidate category has no query frame.
func TestCompetitionSubmissionChartsPreserveSparseQuerySeries(t *testing.T) {
	for _, id := range []int{31, 32, 33, 34} {
		panel := readCompetitionSubmissionPlot(t, id)
		aliases := map[string]string{}
		xAliases := map[string]string{}
		for _, transformation := range panel.Transformations {
			if transformation.Id != "organize" {
				continue
			}
			if transformation.Filter.Id != "byRefId" || transformation.Filter.Options == "" {
				t.Fatalf("panel %d renames multiple unrelated frames at once", id)
			}
			ref := transformation.Filter.Options
			alias := transformation.Options.RenameByName["Value"]
			if alias == "" || transformation.Options.RenameByName["Value #"+ref] != alias {
				t.Fatalf("panel %d lost per-query value field aliases for %s", id, ref)
			}
			aliases[ref] = alias
			xAliases[ref] = transformation.Options.RenameByName["submitted_at_ms"]
			if xAliases[ref] != "submitted_at_ms_"+ref {
				t.Fatalf("panel %d lacks an unambiguous x alias for %s", id, ref)
			}
		}
		seen := map[string]bool{}
		seenXs := map[string]bool{}
		for _, series := range panel.Options.Series {
			if seen[series.Y] || seenXs[series.X] {
				t.Fatalf("panel %d reuses an ambiguous x/y selector: %+v", id, series)
			}
			seen[series.Y] = true
			seenXs[series.X] = true
		}
		// No eligible, significant-only or gate-failed point is present.
		// The ordinary candidate and every reference must remain selectable.
		availableFields := []map[string]bool{}
		for _, target := range panel.Targets {
			if target.RefId == "A" || target.RefId == "B" || target.RefId == "D" {
				continue
			}
			if alias := aliases[target.RefId]; !seen[alias] {
				t.Fatalf("panel %d drops surviving query %s when earlier frames are absent", id, target.RefId)
			}
			// Organize preserves raw field names while adding display aliases.
			availableFields = append(availableFields, map[string]bool{
				"submitted_at_ms":      true,
				"Value":                true,
				xAliases[target.RefId]: true,
				aliases[target.RefId]:  true,
			})
		}
		matchedSeries := 0
		for _, series := range panel.Options.Series {
			for _, fields := range availableFields {
				if fields[series.X] {
					if !fields[series.Y] {
						t.Fatalf("panel %d matches x in another query frame without its y", id)
					}
					matchedSeries++
				}
			}
		}
		if matchedSeries != len(availableFields) {
			t.Fatalf("panel %d lost surviving sparse frames: matched=%d frames=%d", id, matchedSeries, len(availableFields))
		}
	}
}

// Shared field defaults also constrain timestamp x values. A score-only
// min=0 collapses the epoch range toward 1970; p-value max=1 hides every point.
func TestCompetitionSubmissionChartsKeepTimestampAxisUnbounded(t *testing.T) {
	for _, id := range []int{31, 32, 33, 34} {
		panel := readCompetitionSubmissionPlot(t, id)
		for _, bound := range []string{"min", "max"} {
			if _, present := panel.FieldConfig.Defaults[bound]; present {
				t.Fatalf("panel %d applies a score bound to the epoch timestamp axis", id)
			}
		}
		for _, override := range panel.FieldConfig.Overrides {
			if override.Matcher.Id != "byRegexp" || override.Matcher.Options != "^submitted_at_ms_[A-Z]+$" {
				continue
			}
			for _, property := range override.Properties {
				if property.Id == "min" || property.Id == "max" || property.Id == "displayName" {
					t.Fatalf("panel %d bounds timestamp values or collapses unique x aliases", id)
				}
			}
		}
	}
}

// One reference coordinate has no line segment. A marker must survive so the
// first completed submission still shows its baseline and each real fringe.
func TestCompetitionSubmissionReferenceLinesKeepSingletonMarkers(t *testing.T) {
	for _, id := range []int{31, 32, 33, 34} {
		panel := readCompetitionSubmissionPlot(t, id)
		for _, series := range panel.Options.Series[4:] {
			markerAndLine := false
			for _, override := range panel.FieldConfig.Overrides {
				if override.Matcher.Id != "byName" || override.Matcher.Options != series.Y {
					continue
				}
				for _, property := range override.Properties {
					if property.Id == "custom.show" {
						var show string
						if err := json.Unmarshal(property.Value, &show); err != nil {
							t.Fatal(err)
						}
						markerAndLine = show == "points+lines"
					}
				}
			}
			if !markerAndLine {
				t.Fatalf("panel %d hides reference %q with only one completed submission", id, series.Y)
			}
		}
	}
}

// A ready producer cannot bless another producer's stale or incomplete points.
func TestCompetitionSubmissionChartsGateEachProducerBeforeDeduplication(t *testing.T) {
	for _, id := range []int{31, 32, 33, 34, 35} {
		panel := readCompetitionSubmissionPlot(t, id)
		for _, target := range panel.Targets {
			if !target.Instant || target.Range || target.Format != "table" {
				t.Fatalf("panel %d turns one submission into repeated scrape points", id)
			}
			if !strings.HasPrefix(target.Expr, "max by (job_id, round_id, submitted_at_ms, classification) (") ||
				!strings.Contains(target.Expr, "and ignoring(job_id, submitted_at_ms, statistic, classification)") {
				t.Fatalf("panel %d loses per-producer gating before host deduplication", id)
			}
			for _, required := range []string{
				"env=\"$env\"", "host=~\"$host\"", "round_id=\"$score_round\"",
				"submission_score_snapshot_ready", "submission_score_snapshot_complete",
				"submission_score_snapshot_timestamp_seconds", "submission_score_snapshot_max_age_seconds",
				">= 0",
			} {
				if !strings.Contains(target.Expr, required) {
					t.Errorf("panel %d query %s lacks %s", id, target.RefId, required)
				}
			}
		}
	}
}

// Median takeover and mean significance must never masquerade as one cutoff.
func TestCompetitionSubmissionChartsKeepDistinctMedianAndMeanFringes(t *testing.T) {
	cases := []struct {
		id         int
		statistics []string
		forbidden  []string
	}{
		{id: 31, statistics: []string{"median_raw_ms", "baseline_median_raw_ms", "practical_median_max_ms"}, forbidden: []string{"significant_mean_max_ms", "baseline_mean_raw_ms"}},
		{id: 32, statistics: []string{"mean_raw_ms", "baseline_mean_raw_ms", "significant_mean_max_ms"}, forbidden: []string{"practical_median_max_ms"}},
		{id: 33, statistics: []string{"mean_improvement_percent", "zero", "minimum_significant_improvement_percent", "takeover_margin_percent"}},
		{id: 34, statistics: []string{"one_sided_p_value", "alpha"}, forbidden: []string{"baseline_median_raw_ms"}},
	}
	for _, c := range cases {
		panel := readCompetitionSubmissionPlot(t, c.id)
		queries := ""
		for _, target := range panel.Targets {
			queries += target.Expr + "\n"
		}
		for _, statistic := range c.statistics {
			if !strings.Contains(queries, "statistic=\""+statistic+"\"") {
				t.Errorf("panel %d lacks stored statistic %s", c.id, statistic)
			}
		}
		for _, statistic := range c.forbidden {
			if strings.Contains(queries, "statistic=\""+statistic+"\"") {
				t.Errorf("panel %d mixes incompatible cutoff %s", c.id, statistic)
			}
		}
	}
}

// Missing and early-failure measurements remain unknown, never fast zeroes.
func TestCompetitionSubmissionDashboardCallsOutIncompleteInventories(t *testing.T) {
	for _, id := range []int{27, 28} {
		panel := readCompetitionSubmissionPlot(t, id)
		if !strings.Contains(panel.Description, "subset") &&
			!strings.Contains(panel.Description, "partial inventory") {
			t.Errorf("panel %d hides bounded snapshot completeness", id)
		}
	}
	panel := readCompetitionSubmissionPlot(t, 28)
	if !strings.Contains(panel.Description, "no fabricated zero") ||
		!strings.Contains(panel.Description, "including failed gates") {
		t.Fatal("failed scored rows and unscored failures lost their distinct meaning")
	}
}
