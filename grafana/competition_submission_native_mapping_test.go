// Replays the narrow Grafana 13.2 mapping contract with synthetic numeric
// frames. Native before/after browser evidence is separate from these Go tests.
package grafana

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Raw dimensions admit the old document for a deterministic pre-fix failure.
type competitionNativePlot struct {
	Id            int    `json:"id"`
	PluginVersion string `json:"pluginVersion"`
	Options       struct {
		Mapping       string `json:"mapping"`
		SeriesMapping string `json:"seriesMapping"`
		Series        []struct {
			Frame json.RawMessage `json:"frame"`
			X     json.RawMessage `json:"x"`
			Y     json.RawMessage `json:"y"`
		} `json:"series"`
	} `json:"options"`
	Targets []struct {
		RefId string `json:"refId"`
	} `json:"targets"`
	Transformations []struct {
		Id     string `json:"id"`
		Filter struct {
			Id      string `json:"id"`
			Options string `json:"options"`
		} `json:"filter"`
		Options struct {
			RenameByName map[string]string `json:"renameByName"`
			Conversions  []struct {
				TargetField     string `json:"targetField"`
				DestinationType string `json:"destinationType"`
			} `json:"conversions"`
		} `json:"options"`
	} `json:"transformations"`
}

// Only numeric raw/display names and row cardinality affect this regression.
type competitionNativeFrame struct {
	RefId       string
	NumberNames map[string]bool
	Rows        int
}

// Reads the real panel configuration, never a copied production score fixture.
func readCompetitionNativePlot(t *testing.T, id int) competitionNativePlot {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("dashboards", "competition.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Panels []competitionNativePlot `json:"panels"`
	}
	if err := json.Unmarshal(content, &document); err != nil {
		t.Fatal(err)
	}
	for _, panel := range document.Panels {
		if panel.Id == id {
			return panel
		}
	}
	t.Fatalf("missing native submission plot %d", id)
	return competitionNativePlot{}
}

// Grafana 13.2 transformDataFrame.ts94-108 reinserts unmatched frames at its
// zero-sentinel index. Scoped organize operations therefore reorder frames.
// This is a focused source-contract projection, not a JavaScript renderer.
func competitionNativeTransformedFrames(panel competitionNativePlot, available map[string]bool, rows int) []competitionNativeFrame {
	frames := []competitionNativeFrame{}
	if rows == 0 {
		return frames
	}
	for _, target := range panel.Targets {
		if available != nil && !available[target.RefId] {
			continue
		}
		frames = append(frames, competitionNativeFrame{RefId: target.RefId, NumberNames: map[string]bool{}, Rows: rows})
	}
	for i := range frames {
		value := "Value"
		if len(frames) > 1 {
			value += " #" + frames[i].RefId
		}
		frames[i].NumberNames[value] = true
	}
	for _, transformation := range panel.Transformations {
		if transformation.Id == "convertFieldType" {
			for _, conversion := range transformation.Options.Conversions {
				if conversion.TargetField == "submitted_at_ms" && conversion.DestinationType == "number" {
					for i := range frames {
						frames[i].NumberNames["submitted_at_ms"] = true
					}
				}
			}
		}
		if transformation.Id != "organize" || transformation.Filter.Id != "byRefId" {
			continue
		}
		after, appendFrames := []competitionNativeFrame{}, []competitionNativeFrame{}
		insert := 0
		for i, frame := range frames {
			if frame.RefId == transformation.Filter.Options {
				for raw, alias := range transformation.Options.RenameByName {
					if frame.NumberNames[raw] {
						frame.NumberNames[alias] = true
					}
				}
				after = append(after, frame)
			} else {
				if insert == 0 {
					insert = i
				}
				appendFrames = append(appendFrames, frame)
			}
		}
		// JavaScript splice clamps an insertion beyond the selected array end.
		insert = min(insert, len(after))
		combined := append([]competitionNativeFrame{}, after[:insert]...)
		combined = append(combined, appendFrames...)
		frames = append(combined, after[insert:]...)
	}
	return frames
}

// The pinned native migration handles missing versions and versions below11.1.
// Modern options without a version are misinterpreted as old string dimensions.
func competitionNativeNeedsMigration(version string) bool {
	if version == "" {
		return true
	}
	parts := strings.SplitN(version, ".", 3)
	number := parts[0]
	if len(parts) > 1 {
		number += "." + parts[1]
	}
	parsed, err := strconv.ParseFloat(number, 64)
	return err == nil && parsed < 11.1
}

// Native byName accepts an exact raw or display name, never an options object.
func competitionNativeDimensionName(raw json.RawMessage, legacy bool) string {
	var name string
	if legacy {
		_ = json.Unmarshal(raw, &name)
		return name
	}
	var dimension struct {
		Matcher struct {
			Id      string `json:"id"`
			Options string `json:"options"`
		} `json:"matcher"`
	}
	if json.Unmarshal(raw, &dimension) != nil || dimension.Matcher.Id != "byName" {
		return ""
	}
	return dimension.Matcher.Options
}

// Mirrors only the native migration/frame/name selection used by these panels:
// migrations.ts10-16,177-217 and utils.ts53-114 at Grafana v13.2.0.
func competitionNativeMatchedRows(panel competitionNativePlot, frames []competitionNativeFrame) map[string]int {
	matched := map[string]int{}
	legacy := competitionNativeNeedsMigration(panel.PluginVersion)
	for i, series := range panel.Options.Series {
		if i >= len(panel.Targets) {
			continue
		}
		x, y := competitionNativeDimensionName(series.X, legacy), competitionNativeDimensionName(series.Y, legacy)
		if x == "" || y == "" || x == y {
			continue
		}
		mapping, frameIndex := panel.Options.Mapping, -1
		if legacy {
			mapping, frameIndex = "manual", 0
			if panel.Options.SeriesMapping == "manual" {
				frameIndex = i
			}
		} else if len(series.Frame) > 0 {
			var frame struct {
				Matcher struct {
					Id      string `json:"id"`
					Options int    `json:"options"`
				} `json:"matcher"`
			}
			if json.Unmarshal(series.Frame, &frame) != nil || frame.Matcher.Id != "byIndex" {
				continue
			}
			frameIndex = frame.Matcher.Options
		}
		if mapping == "manual" && frameIndex < 0 {
			continue
		}
		for index, frame := range frames {
			if frameIndex >= 0 && frameIndex != index {
				continue
			}
			if frame.NumberNames[x] && frame.NumberNames[y] {
				// A cross-query mapping is not an acceptable substitute for a lost point.
				if frame.RefId != panel.Targets[i].RefId {
					return map[string]int{"foreign_query": 1}
				}
				matched[frame.RefId] += frame.Rows
			}
		}
	}
	return matched
}

// The old manual migration binds A..F to ordinals while organize returns F..A.
// This fails on the pre-fix document even when every query has valid numbers.
func TestCompetitionSubmissionNativeMappingsSurviveFilteredFrameReordering(t *testing.T) {
	for _, id := range []int{31, 32, 33, 34} {
		panel := readCompetitionNativePlot(t, id)
		frames := competitionNativeTransformedFrames(panel, nil, 2)
		for i, frame := range frames {
			if frame.RefId != panel.Targets[len(panel.Targets)-1-i].RefId {
				t.Fatalf("panel %d did not exercise native filtered-frame reversal", id)
			}
		}
		matched := competitionNativeMatchedRows(panel, frames)
		if len(matched) != len(panel.Targets) {
			t.Fatalf("panel %d selects %d of %d valid series after native filtered-frame reordering: %v", id, len(matched), len(panel.Targets), matched)
		}
		for _, target := range panel.Targets {
			if matched[target.RefId] != 2 {
				t.Fatalf("panel %d lost or misrouted query %s", id, target.RefId)
			}
		}
	}
}

// Missing early categories must not shift references; one row keeps real
// reference points, and empty input must never create a fast zero point.
func TestCompetitionSubmissionNativeMappingsPreserveSparseSingletonAndEmpty(t *testing.T) {
	for _, id := range []int{31, 32, 33, 34} {
		panel := readCompetitionNativePlot(t, id)
		available := map[string]bool{"C": true}
		for _, target := range panel.Targets[4:] {
			available[target.RefId] = true
		}
		for _, rows := range []int{2, 1, 0} {
			matched := competitionNativeMatchedRows(panel, competitionNativeTransformedFrames(panel, available, rows))
			if rows == 0 {
				if len(matched) != 0 {
					t.Fatalf("panel %d fabricates empty points: %v", id, matched)
				}
				continue
			}
			if len(matched) != len(available) {
				t.Fatalf("panel %d loses sparse references with %d rows: %v", id, rows, matched)
			}
			for ref := range available {
				if matched[ref] != rows {
					t.Fatalf("panel %d loses sparse query %s with %d rows", id, ref, rows)
				}
			}
		}
	}
}

// Pinning the native plugin version prevents the legacy migration from
// wrapping modern matcher objects as invalid byName option values.
func TestCompetitionSubmissionNativeMappingsGuardModernRemigration(t *testing.T) {
	for _, id := range []int{31, 32, 33, 34} {
		panel := readCompetitionNativePlot(t, id)
		if panel.PluginVersion != "13.2.0" || panel.Options.Mapping != "auto" || panel.Options.SeriesMapping != "" {
			t.Fatalf("panel %d can be remigrated into positional bindings", id)
		}
		for _, series := range panel.Options.Series {
			if len(series.Frame) > 0 || competitionNativeDimensionName(series.X, false) == "" || competitionNativeDimensionName(series.Y, false) == "" {
				t.Fatalf("panel %d is not explicitly name-bound", id)
			}
		}
		frames := competitionNativeTransformedFrames(panel, nil, 1)
		for _, version := range []string{"", "11.0.0"} {
			altered := panel
			altered.PluginVersion = version
			if matched := competitionNativeMatchedRows(altered, frames); len(matched) != 0 {
				t.Fatalf("panel %d did not exercise legacy matcher-object refusal: %v", id, matched)
			}
		}
	}
}
