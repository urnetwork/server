// Keeps the public report's dated operational claims separate from immutable
// performance evidence, including completed-but-rejected calibration.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Contains only report metadata, never a private job, seed or score payload.
type baselineReportSnapshot struct {
	ReadinessCheckedAt   string `json:"readiness_checked_at"`
	QueueObservedAt      string `json:"queue_counts_observed_at"`
	IntakeReady          bool   `json:"intake_ready"`
	GradingReady         bool   `json:"grading_ready"`
	PublicationHeld      bool   `json:"publication_held"`
	ReadyzHttp           int    `json:"readyz_http"`
	Completed            int    `json:"production_completed"`
	Queued               int    `json:"production_queued"`
	HiddenComplete       bool   `json:"hidden_screen_complete"`
	HiddenAccepted       bool   `json:"hidden_screen_accepted"`
	MeasurementsComplete int    `json:"hidden_unique_measurements_complete_at_operator_snapshot"`
	MeasurementsRequired int    `json:"hidden_required_unique_measurements"`
	Custody              string `json:"scoped_compliance_custody"`
}

// Reads the current report's own companion rather than assuming a dated filename.
type baselineReportFixture struct {
	Document         string
	CompanionPath    string
	CompanionContent []byte
	Derivation       struct {
		Integration struct {
			PreservedPreview          string `json:"preserved_0124_preview"`
			PreservedPreviewSha256    string `json:"preserved_0124_preview_sha256"`
			PreservedDerivation       string `json:"preserved_0124_derivation"`
			PreservedDerivationSha256 string `json:"preserved_0124_derivation_sha256"`
		} `json:"integration"`
		PublicSources []struct {
			Path   string `json:"path"`
			Sha256 string `json:"sha256"`
		} `json:"public_sources"`
		Metrics                  map[string]float64         `json:"metrics"`
		Formulas                 map[string]string          `json:"formulas"`
		FrozenProfile            map[string]json.RawMessage `json:"frozen_profile"`
		Rounds                   []map[string]float64       `json:"rounds"`
		Snapshot                 baselineReportSnapshot     `json:"status_snapshot"`
		PrivateEpochBaselineUsed bool                       `json:"private_official_epoch_baseline_used"`
		PrivateOwnerScoresUsed   bool                       `json:"private_owner_scores_used"`
		HiddenMetricsUsed        bool                       `json:"hidden_metrics_used"`
	}
}

// Resolves only regular, local public artifacts inside the baseline directory.
func readBaselineReportLeaf(path string) ([]byte, error) {
	if !filepath.IsLocal(path) {
		return nil, fmt.Errorf("non-local public report source %q", path)
	}
	root, err := filepath.Abs("baseline")
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, path))
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return nil, fmt.Errorf("public report source escapes baseline: %q", path)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("non-regular public report source %q", path)
	}
	return os.ReadFile(resolved)
}

// Loads the exact public document and the uniquely named derivation it links.
func newBaselineReportFixture(t *testing.T) *baselineReportFixture {
	t.Helper()
	content, err := readBaselineReportLeaf("final-baseline.html")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &baselineReportFixture{Document: string(content)}
	links := regexp.MustCompile(`href="([^"<>]+-derivation\.json)"`).FindAllStringSubmatch(fixture.Document, -1)
	if len(links) == 0 {
		t.Fatal("report has no derivation link")
	}
	fixture.CompanionPath = links[0][1]
	for _, link := range links {
		if link[1] != fixture.CompanionPath {
			t.Fatal("report links conflicting derivations")
		}
	}
	fixture.CompanionContent, err = readBaselineReportLeaf(fixture.CompanionPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(fixture.CompanionContent, &fixture.Derivation); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// The report uses four nonnested sections with stable semantic ids.
func baselineReportSection(document, id string) (string, error) {
	start := strings.Index(document, `<section id="`+id+`"`)
	if start < 0 {
		return "", fmt.Errorf("missing report section %q", id)
	}
	end := strings.Index(document[start:], "</section>")
	if end < 0 {
		return "", fmt.Errorf("unclosed report section %q", id)
	}
	return document[start : start+end+len("</section>")], nil
}

// Plot metadata distinguishes real median/mean boundaries from toy references.
// The applicable color and a nonzero dash pattern must share a plotted element.
func validateBaselineReportPlotFringes(plot string) error {
	opening := regexp.MustCompile(`<svg\b[^>]*>`).FindString(plot)
	labels := regexp.MustCompile(`\saria-label="([^"<>]*)"`).FindAllStringSubmatch(opening, -1)
	title := regexp.MustCompile(`(?s)<title>(.*?)</title>`).FindStringSubmatch(plot)
	if len(labels) != 1 || labels[0][1] == "" || len(title) != 2 {
		return fmt.Errorf("plot has no unambiguous semantic label and title")
	}
	identity := strings.ToLower(labels[0][1] + " " + title[1])
	median := strings.Contains(identity, "practical median threshold") || strings.Contains(identity, "raw-score median cutoff")
	mean := strings.Contains(identity, "welch mean fringe") || strings.Contains(identity, "mean-based statistical fringe")
	toy := strings.Contains(identity, "illustrative toy mapped fringe lines") ||
		strings.Contains(identity, "aggregate delivered rate") && strings.Contains(identity, "illustrative fringe") ||
		strings.Contains(identity, "illustrative") && strings.Contains(identity, "scenario, not a forecast")
	practicalRequired, statisticalRequired := false, false
	switch {
	case median && !mean && !toy:
		practicalRequired = true
	case mean && !median && !toy:
		statisticalRequired = true
	case toy && !median && !mean:
		practicalRequired, statisticalRequired = true, true
	default:
		return fmt.Errorf("plot has an unknown or conflated fringe statistic")
	}
	hasDash := func(element string) bool {
		matches := regexp.MustCompile(`\sstroke-dasharray="([^"<>]*)"`).FindAllStringSubmatch(element, -1)
		if len(matches) != 1 {
			return false
		}
		positive := false
		for _, part := range strings.FieldsFunc(matches[0][1], func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t' || r == '\r' || r == '\n'
		}) {
			length, err := strconv.ParseFloat(part, 64)
			if err != nil || math.IsNaN(length) || math.IsInf(length, 0) || length < 0 {
				return false
			}
			positive = positive || 0 < length
		}
		return positive
	}
	fringes := map[string]bool{"#00836d": false, "#8b5bb5": false}
	for _, element := range regexp.MustCompile(`<(line|polyline|path)\b[^>]*>`).FindAllString(plot, -1) {
		stroke := regexp.MustCompile(`\sstroke="([^"<>]*)"`).FindAllStringSubmatch(element, -1)
		if len(stroke) != 1 || !hasDash(element) {
			continue
		}
		color := strings.ToLower(stroke[0][1])
		if _, exists := fringes[color]; exists {
			fringes[color] = true
		}
	}
	if fringes["#00836d"] != practicalRequired || fringes["#8b5bb5"] != statisticalRequired {
		return fmt.Errorf("plot fringes differ from its statistic: practical=%t statistical=%t", practicalRequired, statisticalRequired)
	}
	return nil
}

// Synthetic metadata and line styles exercise the same public plot validator.
func syntheticBaselineReportPlot(label, elements string) string {
	return `<svg role="img" aria-label="` + label + `"><title>Synthetic report plot</title>` + elements + `</svg>`
}

// Rejects stale progress, ambiguous attributes and outcome/custody conflation.
func validateBaselineReportStatus(document string, snapshot baselineReportSnapshot) error {
	section, err := baselineReportSection(document, "status")
	if err != nil {
		return err
	}
	if snapshot.MeasurementsRequired <= 0 || snapshot.MeasurementsComplete < 0 ||
		snapshot.MeasurementsComplete > snapshot.MeasurementsRequired ||
		snapshot.HiddenComplete && snapshot.MeasurementsComplete != snapshot.MeasurementsRequired {
		return fmt.Errorf("inconsistent completed-measurement snapshot")
	}
	attributes := map[string]string{
		"data-observed-at":           snapshot.ReadinessCheckedAt,
		"data-snapshot-at":           snapshot.QueueObservedAt,
		"data-calibration-complete":  strconv.FormatBool(snapshot.HiddenComplete),
		"data-calibration-accepted":  strconv.FormatBool(snapshot.HiddenAccepted),
		"data-measurements-complete": strconv.Itoa(snapshot.MeasurementsComplete),
		"data-measurements-required": strconv.Itoa(snapshot.MeasurementsRequired),
		"data-custody":               snapshot.Custody,
		"data-completed":             strconv.Itoa(snapshot.Completed),
		"data-queued":                strconv.Itoa(snapshot.Queued),
		"data-intake-ready":          strconv.FormatBool(snapshot.IntakeReady),
		"data-grading-ready":         strconv.FormatBool(snapshot.GradingReady),
		"data-publication-held":      strconv.FormatBool(snapshot.PublicationHeld),
		"data-readyz-http":           strconv.Itoa(snapshot.ReadyzHttp),
	}
	for name, expected := range attributes {
		matches := regexp.MustCompile(regexp.QuoteMeta(name)+`="([^"<>]*)"`).FindAllStringSubmatch(section, -1)
		if len(matches) != 1 || matches[0][1] != expected {
			return fmt.Errorf("report attribute %s does not match dated derivation", name)
		}
	}
	counts := regexp.MustCompile(`<div class="large-number">([0-9]+)</div>`).FindAllStringSubmatch(section, -1)
	if len(counts) != 2 || counts[0][1] != strconv.Itoa(snapshot.Completed) || counts[1][1] != strconv.Itoa(snapshot.Queued) {
		return fmt.Errorf("visible queue counts differ from dated derivation")
	}
	if snapshot.HiddenComplete && !snapshot.HiddenAccepted &&
		(!strings.Contains(section, "NOT ACCEPTED") || strings.Contains(strings.ToUpper(section), "RUNNING")) {
		return fmt.Errorf("complete rejected screen is presented as running or accepted")
	}
	return nil
}

// All four required sections keep a real plotted or CSS-encoded visual.
func TestBaselineReportKeepsFourSectionsAndVisuals(t *testing.T) {
	fixture := newBaselineReportFixture(t)
	ids := regexp.MustCompile(`<section id="([^"<>]+)"`).FindAllStringSubmatch(fixture.Document, -1)
	expected := []string{"environment", "public-data", "status", "projection"}
	if len(ids) != len(expected) {
		t.Fatalf("report has %d sections, want %d", len(ids), len(expected))
	}
	for index, id := range expected {
		if ids[index][1] != id {
			t.Fatalf("section %d is %q, want %q", index, ids[index][1], id)
		}
		section, err := baselineReportSection(fixture.Document, id)
		if err != nil {
			t.Fatal(err)
		}
		if id == "status" {
			if !strings.Contains(section, `class="status-path"`) || !strings.Contains(section, `class="queue-grid"`) {
				t.Fatal("status section lost its lifecycle or queue visual")
			}
		} else if !strings.Contains(section, "<svg ") {
			t.Fatalf("section %q has no plot", id)
		}
	}
}

// Completion, acceptance, custody and live readiness are distinct claims.
func TestBaselineReportStatusMatchesDatedDerivation(t *testing.T) {
	fixture := newBaselineReportFixture(t)
	if err := validateBaselineReportStatus(fixture.Document, fixture.Derivation.Snapshot); err != nil {
		t.Fatal(err)
	}
	if fixture.Derivation.PrivateEpochBaselineUsed || fixture.Derivation.PrivateOwnerScoresUsed || fixture.Derivation.HiddenMetricsUsed {
		t.Fatal("public report includes disallowed private performance inputs")
	}
}

// A completed but rejected synthetic screen must never inherit stale success.
func TestBaselineReportStatusRejectsSyntheticStaleClaims(t *testing.T) {
	snapshot := baselineReportSnapshot{
		ReadinessCheckedAt: "2031-02-03T04:05:06Z", QueueObservedAt: "2031-02-03T04:00:00Z",
		IntakeReady: true, GradingReady: false, PublicationHeld: true, ReadyzHttp: 200,
		Completed: 2, Queued: 7, HiddenComplete: true, HiddenAccepted: false,
		MeasurementsComplete: 12, MeasurementsRequired: 12, Custody: "retained-readback-complete",
	}
	document := `<section id="status" data-observed-at="2031-02-03T04:05:06Z" data-snapshot-at="2031-02-03T04:00:00Z" data-calibration-complete="true" data-calibration-accepted="false" data-measurements-complete="12" data-measurements-required="12" data-custody="retained-readback-complete" data-completed="2" data-queued="7" data-intake-ready="true" data-grading-ready="false" data-publication-held="true" data-readyz-http="200"><div class="large-number">2</div><div class="large-number">7</div>NOT ACCEPTED</section>`
	if err := validateBaselineReportStatus(document, snapshot); err != nil {
		t.Fatal(err)
	}
	for _, change := range [][2]string{
		{`data-measurements-complete="12"`, `data-measurements-complete="3"`},
		{`data-calibration-accepted="false"`, `data-calibration-accepted="true"`},
		{`data-custody="retained-readback-complete"`, `data-custody="pending"`},
		{`<div class="large-number">7</div>`, `<div class="large-number">8</div>`},
		{`data-grading-ready="false"`, `data-grading-ready="true"`},
		{`data-publication-held="true"`, `data-publication-held="false"`},
		{`NOT ACCEPTED`, `RUNNING`},
		{`data-custody="retained-readback-complete"`, `data-custody="retained-readback-complete" data-custody="retained-readback-complete"`},
	} {
		if err := validateBaselineReportStatus(strings.Replace(document, change[0], change[1], 1), snapshot); err == nil {
			t.Errorf("stale or ambiguous synthetic claim accepted: %q", change[1])
		}
	}
}

// The exact old running-state report deterministically reproduces the defect.
func TestBaselineReportHistoricalStatusCannotStandInForCurrent(t *testing.T) {
	fixture := newBaselineReportFixture(t)
	historical, err := readBaselineReportLeaf(fixture.Derivation.Integration.PreservedPreview)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(historical)) != fixture.Derivation.Integration.PreservedPreviewSha256 {
		t.Fatal("preserved running-state preview changed")
	}
	if err := validateBaselineReportStatus(string(historical), fixture.Derivation.Snapshot); err == nil {
		t.Fatal("old running/pending report passed current complete/rejected/custodied status")
	}
	priorContent, err := readBaselineReportLeaf(fixture.Derivation.Integration.PreservedDerivation)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(priorContent)) != fixture.Derivation.Integration.PreservedDerivationSha256 {
		t.Fatal("preserved prior derivation changed")
	}
	prior := &baselineReportFixture{}
	if err := json.Unmarshal(priorContent, &prior.Derivation); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prior.Derivation.Metrics, fixture.Derivation.Metrics) ||
		!reflect.DeepEqual(prior.Derivation.Rounds, fixture.Derivation.Rounds) ||
		!reflect.DeepEqual(prior.Derivation.Formulas, fixture.Derivation.Formulas) ||
		!reflect.DeepEqual(prior.Derivation.FrozenProfile, fixture.Derivation.FrozenProfile) {
		t.Fatal("status refresh changed performance math or the frozen profile")
	}
}

// Status maintenance cannot move a public point, fringe or toy-projection line.
func TestBaselineReportKeepsPerformanceSectionsAndPublicSources(t *testing.T) {
	fixture := newBaselineReportFixture(t)
	historical, err := readBaselineReportLeaf(fixture.Derivation.Integration.PreservedPreview)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"environment", "public-data", "projection"} {
		before, err := baselineReportSection(string(historical), id)
		if err != nil {
			t.Fatal(err)
		}
		after, err := baselineReportSection(fixture.Document, id)
		if err != nil {
			t.Fatal(err)
		}
		if before != after {
			t.Fatalf("status refresh changed immutable %q section", id)
		}
		if id != "environment" {
			plots := regexp.MustCompile(`(?s)<svg\b.*?</svg>`).FindAllString(after, -1)
			if len(plots) == 0 {
				t.Fatalf("no performance plots in %q", id)
			}
			for index, plot := range plots {
				if err := validateBaselineReportPlotFringes(plot); err != nil {
					t.Errorf("%s plot %d: %s", id, index, err)
				}
			}
		}
	}
	if len(fixture.Derivation.PublicSources) == 0 {
		t.Fatal("report has no public-source bindings")
	}
	for _, source := range fixture.Derivation.PublicSources {
		content, err := readBaselineReportLeaf(source.Path)
		if err != nil {
			t.Fatal(err)
		}
		expected, err := hex.DecodeString(source.Sha256)
		actual := sha256.Sum256(content)
		if err != nil || len(expected) != len(actual) || source.Sha256 != fmt.Sprintf("%x", actual) {
			t.Fatalf("public source %q does not match its digest", source.Path)
		}
	}
}

// Real median and mean panels remain separate; toy plots need both references.
func TestBaselineReportFringesAcceptApplicableStatistics(t *testing.T) {
	practical := `<line stroke="#00836d" stroke-dasharray="4 2"/>`
	statistical := `<line stroke="#8b5bb5" stroke-dasharray="2, 3"/>`
	for _, test := range []struct {
		label    string
		elements string
	}{
		{label: "Synthetic practical median threshold", elements: practical},
		{label: "Synthetic Welch mean fringe", elements: statistical},
		{label: "Synthetic illustrative toy mapped fringe lines", elements: practical + statistical},
		{label: "Synthetic aggregate delivered rate and illustrative fringe references", elements: practical + statistical},
		{label: "Synthetic illustrative scenario, not a forecast", elements: practical + statistical},
	} {
		if err := validateBaselineReportPlotFringes(syntheticBaselineReportPlot(test.label, test.elements)); err != nil {
			t.Errorf("legitimate statistic-specific plot %q refused: %s", test.label, err)
		}
	}
}

// An opposite-color line cannot substitute for a missing applicable fringe.
func TestBaselineReportFringesRejectMissingApplicableLines(t *testing.T) {
	practical := `<line stroke="#00836d" stroke-dasharray="4 2"/>`
	statistical := `<line stroke="#8b5bb5" stroke-dasharray="2 3"/>`
	for _, test := range []struct {
		label    string
		elements string
	}{
		{label: "Synthetic practical median threshold"},
		{label: "Synthetic practical median threshold", elements: statistical},
		{label: "Synthetic Welch mean fringe"},
		{label: "Synthetic Welch mean fringe", elements: practical},
		{label: "Synthetic illustrative toy mapped fringe lines", elements: practical},
		{label: "Synthetic illustrative toy mapped fringe lines", elements: statistical},
		{label: "Synthetic aggregate delivered rate and illustrative fringe references", elements: practical},
		{label: "Synthetic illustrative scenario, not a forecast", elements: statistical},
	} {
		if err := validateBaselineReportPlotFringes(syntheticBaselineReportPlot(test.label, test.elements)); err == nil {
			t.Errorf("missing applicable fringe accepted for %q", test.label)
		}
	}
}

// A solid colored line and an unrelated dashed element are not a fringe.
func TestBaselineReportFringesRejectIncorrectLineStyles(t *testing.T) {
	for _, test := range []struct {
		label    string
		elements string
	}{
		{label: "Synthetic practical median threshold", elements: `<line stroke="#00836d"/><line stroke="#123456" stroke-dasharray="4 2"/>`},
		{label: "Synthetic Welch mean fringe", elements: `<line stroke="#8b5bb5"/><line stroke="#123456" stroke-dasharray="4 2"/>`},
		{label: "Synthetic practical median threshold", elements: `<line stroke="#00836d" stroke-dasharray="0 0"/>`},
		{label: "Synthetic Welch mean fringe", elements: `<line stroke="#8b5bb5" stroke-dasharray="none"/>`},
		{label: "Synthetic practical median threshold", elements: `<text stroke="#00836d" stroke-dasharray="4 2">Synthetic legend</text>`},
		{label: "Synthetic Welch mean fringe", elements: `<line data-stroke="#8b5bb5" stroke-dasharray="4 2"/>`},
	} {
		if err := validateBaselineReportPlotFringes(syntheticBaselineReportPlot(test.label, test.elements)); err == nil {
			t.Errorf("incorrect applicable fringe style accepted for %q: %s", test.label, test.elements)
		}
	}
}

// Actual-statistic panels cannot acquire a second, incompatible cutoff.
func TestBaselineReportFringesRejectConflatedStatistics(t *testing.T) {
	both := `<line stroke="#00836d" stroke-dasharray="4 2"/><line stroke="#8b5bb5" stroke-dasharray="2 3"/>`
	for _, label := range []string{
		"Synthetic practical median threshold",
		"Synthetic Welch mean fringe",
		"Synthetic practical median threshold and Welch mean fringe",
		"Synthetic practical median threshold and illustrative toy mapped fringe lines",
		"Synthetic unclassified statistic",
	} {
		if err := validateBaselineReportPlotFringes(syntheticBaselineReportPlot(label, both)); err == nil {
			t.Errorf("conflated or unclassified fringe contract accepted: %q", label)
		}
	}
}

// The illustrative six-round trajectory reuses each prior round's ceiling.
func TestBaselineReportProjectionUsesEachRoundThreshold(t *testing.T) {
	fixture := newBaselineReportFixture(t)
	metrics, rounds := fixture.Derivation.Metrics, fixture.Derivation.Rounds
	q := metrics["q"]
	if !(0 < q && q < 1) || math.Abs(q-(1-metrics["margin"])) > 1e-12 || len(rounds) != 7 {
		t.Fatal("invalid six-round projection margin or row count")
	}
	for index, round := range rounds {
		factor := math.Pow(q, float64(index))
		for name, expected := range map[string]float64{
			"step": float64(index), "factor": factor,
			"rawMedianCeiling":     metrics["rawMedian"] * factor,
			"ttfbP50Ms":            metrics["ttfbP50"] * factor,
			"ttfbP95Ms":            metrics["ttfbP95"] * factor,
			"largeTransferP50Mbps": metrics["transferP50"] / factor,
			"largeTransferP95Mbps": metrics["transferP95"] / factor,
		} {
			actual, exists := round[name]
			if !exists || math.IsNaN(actual) || math.IsInf(actual, 0) || math.Abs(actual-expected) > 1e-10*math.Max(1, math.Abs(expected)) {
				t.Errorf("round %d %s=%g, want %g", index, name, actual, expected)
			}
		}
	}
}

// The public lineage must bind exact current and preserved report bytes.
func TestBaselineReportLineagePinsExactReports(t *testing.T) {
	fixture := newBaselineReportFixture(t)
	readme, err := readBaselineReportLeaf("README.md")
	if err != nil {
		t.Fatal(err)
	}
	link := regexp.MustCompile(`\((REPORT-LINEAGE-[^()]+\.json)\)`).FindStringSubmatch(string(readme))
	if len(link) != 2 {
		t.Fatal("baseline README lost its report-lineage link")
	}
	content, err := readBaselineReportLeaf(link[1])
	if err != nil {
		t.Fatal(err)
	}
	var lineage struct {
		Current struct {
			Path             string `json:"path"`
			Sha256           string `json:"sha256"`
			DerivationPath   string `json:"derivation_path"`
			DerivationSha256 string `json:"derivation_sha256"`
		} `json:"new_public_data_report"`
		Previous struct {
			Path   string `json:"preserved_path"`
			Sha256 string `json:"observed_bytes_sha256"`
		} `json:"previous_public_preview"`
	}
	if err := json.Unmarshal(content, &lineage); err != nil {
		t.Fatal(err)
	}
	if lineage.Current.Path != "final-baseline.html" || lineage.Current.DerivationPath != fixture.CompanionPath ||
		lineage.Current.Sha256 != fmt.Sprintf("%x", sha256.Sum256([]byte(fixture.Document))) ||
		lineage.Current.DerivationSha256 != fmt.Sprintf("%x", sha256.Sum256(fixture.CompanionContent)) ||
		lineage.Previous.Path != fixture.Derivation.Integration.PreservedPreview ||
		lineage.Previous.Sha256 != fixture.Derivation.Integration.PreservedPreviewSha256 {
		t.Fatal("report, derivation and preserved-preview lineage differ")
	}
}
