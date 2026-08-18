package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	statspkg "github.com/urnetwork/server/stats"
	"github.com/urnetwork/server/stats/sample"
)

type scoreFixtureOptions struct {
	evaluationId    string
	rowCount        int
	failures        int
	totalMs         float64
	bytesPerRow     int64
	loadMillis      float32
	poolCount       int32
	emptyPool       bool
	sampleOffsetMs  int64
	accountingBytes int64
	stderrExtra     string
	resource        ResourceReport
}

type scoreFixture struct {
	dir          string
	run          string
	stderr       string
	accounting   string
	samples      string
	resource     string
	marker       string
	baseline     string
	runStats     *RunStats
	baselineData *ScoreBaselineManifest
	inputs       ScoreInputs
}

func defaultScoreFixtureOptions() scoreFixtureOptions {
	return scoreFixtureOptions{
		rowCount:        100,
		totalMs:         100,
		bytesPerRow:     100,
		loadMillis:      10,
		poolCount:       100,
		sampleOffsetMs:  10,
		accountingBytes: 10_000,
		resource: ResourceReport{
			Schema:             1,
			Kind:               resourceReportKind,
			CgroupId:           "test-cgroup-1",
			MeasurementStartMs: 1,
			MeasurementEndMs:   123456790,
			Complete:           true,
		},
	}
}

func newScoreFixture(t *testing.T, options scoreFixtureOptions) *scoreFixture {
	t.Helper()
	dir := t.TempDir()
	fixture := &scoreFixture{
		dir:        dir,
		run:        filepath.Join(dir, "candidate.csv"),
		stderr:     filepath.Join(dir, "candidate.stderr"),
		accounting: filepath.Join(dir, "accounting.json"),
		samples:    filepath.Join(dir, "samples.pb.xz"),
		resource:   filepath.Join(dir, "resource.json"),
		marker:     filepath.Join(dir, "candidate.complete.json"),
		baseline:   filepath.Join(dir, "baseline.json"),
	}

	evaluationId := options.evaluationId
	if evaluationId == "" {
		evaluationId = "eval-fixture-1"
	}
	const timeoutMs = int64(1000)
	const startMs = int64(1000)
	flags := map[string]string{
		"duration":        "1s",
		"fleet_shards":    "1",
		"hosts":           "4",
		"impair":          "true",
		"prewarm":         "13h0m0s",
		"ramp":            "1s",
		"request_timeout": "1s",
		"reset":           "true",
		"settle":          "1s",
	}
	failures := options.failures
	if options.rowCount < failures {
		failures = options.rowCount
	}
	fixture.runStats = &RunStats{
		Schema:             runStatsSchema,
		Kind:               runStatsKind,
		ScoreSchema:        apexScoreSchema,
		ScorerVersion:      officialScorerVersion,
		EvaluationId:       evaluationId,
		Official:           true,
		RequestTimeoutMs:   timeoutMs,
		StatsRoot:          fixture.samples,
		StatsInstanceId:    "stats-instance-1",
		ResourceReport:     fixture.resource,
		AccountingReport:   fixture.accounting,
		FinalMarker:        fixture.marker,
		CompletionState:    "complete",
		CompletedUnixMs:    123456789,
		ConfigSha256:       strings.Repeat("a", 64),
		BuildRevision:      strings.Repeat("b", 40),
		BuildModified:      false,
		Hostname:           "official-box-1",
		Os:                 "linux",
		Arch:               "amd64",
		NumCpu:             12,
		Flags:              flags,
		MeasureStartMs:     startMs,
		MeasureEndMs:       startMs + 1000,
		ClientsPool:        10,
		ClientsEstablished: 10,
		Rows:               options.rowCount,
		RowsInWindow:       options.rowCount,
		Failures:           failures,
		Metrics:            map[string]MetricSummary{},
	}

	var csv strings.Builder
	csv.WriteString(resultCSVHeader + "\n")
	for i := 0; i < options.rowCount; i++ {
		status := 200
		if options.rowCount-failures <= i {
			status = 0
		}
		fmt.Fprintf(
			&csv,
			"%d,c,/,%d,%d,%d,10,%.3f,1000\n",
			startMs+int64(i),
			0,
			status,
			options.bytesPerRow,
			options.totalMs,
		)
	}
	if err := os.WriteFile(fixture.run, []byte(csv.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	csvSum := sha256.Sum256([]byte(csv.String()))
	fixture.runStats.ResultsCsvSha256 = fmt.Sprintf("%x", csvSum)
	fixture.runStats.ResultsCsvBytes = int64(csv.Len())
	if err := writeRunStats(scoreSidecarForCSV(fixture.run), fixture.runStats); err != nil {
		t.Fatal(err)
	}
	if err := writeFinalMarker(fixture.marker, scoreSidecarForCSV(fixture.run), fixture.runStats); err != nil {
		t.Fatal(err)
	}
	stderr := fmt.Sprintf("[sim-latency eval=%s] measure window complete; draining\n%s", evaluationId, options.stderrExtra)
	if err := os.WriteFile(fixture.stderr, []byte(stderr), 0o644); err != nil {
		t.Fatal(err)
	}

	accounting := AccountingSnapshot{
		Schema:              1,
		Kind:                accountingSnapshotKind,
		EvaluationId:        evaluationId,
		Complete:            true,
		MeasureStartMs:      startMs,
		MeasureEndMs:        startMs + 1000,
		ProviderEgressBytes: options.accountingBytes,
	}
	writeScoreJSON(t, fixture.accounting, accounting)

	writer, err := statspkg.NewFlatWriter(fixture.samples)
	if err != nil {
		t.Fatal(err)
	}
	poolCount := options.poolCount
	candidates := []*sample.Candidate{{ClientId: []byte("candidate")}}
	if options.emptyPool {
		poolCount = 0
		candidates = nil
	}
	sampleBytes, err := proto.Marshal(&sample.FindProviders2Sample{
		TimeUnixMilli: uint64(startMs + options.sampleOffsetMs),
		LoadMillis:    options.loadMillis,
		PoolCount:     poolCount,
		Candidates:    candidates,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteFrame(sampleBytes); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	options.resource.EvaluationId = evaluationId
	writeScoreJSON(t, fixture.resource, options.resource)

	fixture.baselineData = scoreBaselineFromReplicates(
		"round-1",
		fixture.runStats.ConfigSha256,
		timeoutMs,
		flags,
		0.01,
		[]ScoreBaselineReplicate{{
			RawScore:               100,
			SuccessRate:            1,
			RequestCount:           100,
			ReceivedBytes:          10_000,
			FindProvidersLoadP95Ms: 10,
			FindProvidersPoolP05:   100,
		}},
	)
	writeScoreJSON(t, fixture.baseline, fixture.baselineData)
	fixture.inputs = ScoreInputs{
		Runs:             []string{fixture.run},
		Stderr:           []string{fixture.stderr},
		Accounting:       []string{fixture.accounting},
		Samples:          []string{fixture.samples},
		ResourceReports:  []string{fixture.resource},
		Markers:          []string{fixture.marker},
		BaselineManifest: fixture.baseline,
	}
	return fixture
}

func writeScoreJSON(t *testing.T, path string, value any) {
	t.Helper()
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(content, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func requireGate(t *testing.T, result *ScoreResult, name string, passed bool) {
	t.Helper()
	gate, ok := result.Gates[name]
	if !ok {
		t.Fatalf("gate %s missing: %+v", name, result.EvalError)
	}
	if gate.Passed != passed {
		t.Fatalf("gate %s passed=%t, want %t (details=%+v)", name, gate.Passed, passed, gate.Details)
	}
}

func requireTakeoverEligibility(t *testing.T, result *ScoreResult, want bool) {
	t.Helper()
	got := result.Diagnostics.BaselineTakeoverEligible
	if got == nil || *got != want {
		t.Fatalf("takeover eligibility = %v, want %t", got, want)
	}
}

func requireScoreGolden(t *testing.T, name string, result *ScoreResult) {
	t.Helper()
	got, err := marshalScoreResult(result)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "score", name+".golden.json")
	if os.Getenv("UPDATE_SCORE_GOLDENS") == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s score golden mismatch\nwant:\n%s\ngot:\n%s", name, want, got)
	}
}

func baselineInputsFromFixture(fixture *scoreFixture) ScoreBaselineInputs {
	return ScoreBaselineInputs{
		Runs:            []string{fixture.run},
		Stderr:          []string{fixture.stderr},
		Accounting:      []string{fixture.accounting},
		Samples:         []string{fixture.samples},
		ResourceReports: []string{fixture.resource},
		Markers:         []string{fixture.marker},
		RoundId:         "round-baseline-build-1",
		TakeoverMargin:  0.05,
	}
}

func TestBuildScoreBaselineUsesOfficialArtifactValidation(t *testing.T) {
	fixture := newScoreFixture(t, defaultScoreFixtureOptions())
	baseline, err := BuildScoreBaseline(baselineInputsFromFixture(fixture))
	if err != nil {
		t.Fatal(err)
	}
	if baseline.RoundId != "round-baseline-build-1" || baseline.TakeoverMargin != 0.05 || len(baseline.Replicates) != 1 {
		t.Fatalf("unexpected baseline contract: %+v", baseline)
	}
	replicate := baseline.Replicates[0]
	if replicate.RawScore != 100 || replicate.SuccessRate != 1 ||
		replicate.RequestCount != 100 || replicate.ReceivedBytes != 10_000 ||
		replicate.FindProvidersLoadP95Ms != 10 || replicate.FindProvidersPoolP05 != 100 {
		t.Fatalf("unexpected baseline diagnostics: %+v", replicate)
	}

	writeScoreJSON(t, fixture.baseline, baseline)
	result := Score(fixture.inputs)
	if result.EvalError != nil || !result.Placeable || result.NormalizedScore != 100 {
		t.Fatalf("generated baseline did not round-trip through scorer: %+v", result)
	}
}

func TestBuildScoreBaselineRejectsEvenReplicateCount(t *testing.T) {
	fixture := newScoreFixture(t, defaultScoreFixtureOptions())
	inputs := baselineInputsFromFixture(fixture)
	inputs.Runs = append(inputs.Runs, fixture.run)
	inputs.Stderr = append(inputs.Stderr, fixture.stderr)
	inputs.Accounting = append(inputs.Accounting, fixture.accounting)
	inputs.Samples = append(inputs.Samples, fixture.samples)
	inputs.ResourceReports = append(inputs.ResourceReports, fixture.resource)
	inputs.Markers = append(inputs.Markers, fixture.marker)
	if _, err := BuildScoreBaseline(inputs); err == nil || !strings.Contains(err.Error(), "odd and positive") {
		t.Fatalf("expected odd replicate rejection, got %v", err)
	}
}

func TestBuildScoreBaselineRejectsUnhealthyBaseline(t *testing.T) {
	options := defaultScoreFixtureOptions()
	options.failures = 4
	fixture := newScoreFixture(t, options)
	if _, err := BuildScoreBaseline(baselineInputsFromFixture(fixture)); err == nil || !strings.Contains(err.Error(), "97% success floor") {
		t.Fatalf("expected baseline success rejection, got %v", err)
	}
}

func TestBuildScoreBaselineReportsSanitizedStabilityFindings(t *testing.T) {
	options := defaultScoreFixtureOptions()
	options.stderrExtra = "panic: private diagnostic payload\nUnexpected error: private recovery payload\n"
	fixture := newScoreFixture(t, options)

	_, err := BuildScoreBaseline(baselineInputsFromFixture(fixture))
	if err == nil {
		t.Fatal("unstable baseline was accepted")
	}
	want := "baseline replicate 1 is not stable (findings: unexpected_recovery,runtime_panic)"
	if err.Error() != want {
		t.Fatalf("stability error = %q, want %q", err, want)
	}
	if strings.Contains(err.Error(), "private") {
		t.Fatalf("stability error leaked raw stderr: %q", err)
	}
	var coded *scoreCodedError
	if !errors.As(err, &coded) || coded.code != "baseline_gate_failed" {
		t.Fatalf("stability error = %#v, want baseline_gate_failed", err)
	}
}

func TestBuildScoreBaselineRejectsPrewindowMatchmakingSample(t *testing.T) {
	options := defaultScoreFixtureOptions()
	options.sampleOffsetMs = -1
	fixture := newScoreFixture(t, options)
	if _, err := BuildScoreBaseline(baselineInputsFromFixture(fixture)); err == nil ||
		!strings.Contains(err.Error(), "matchmaking integrity") {
		t.Fatalf("expected out-of-window matchmaking rejection, got %v", err)
	}
}

func TestScoreBaselinePlaceable(t *testing.T) {
	fixture := newScoreFixture(t, defaultScoreFixtureOptions())
	result := Score(fixture.inputs)
	if result.EvalError != nil || !result.Placeable {
		t.Fatalf("baseline not placeable: %+v", result)
	}
	if result.RawScore != 100 || result.NormalizedScore != 100 {
		t.Fatalf("score raw=%v normalized=%v", result.RawScore, result.NormalizedScore)
	}
	for name := range result.Gates {
		requireGate(t, result, name, true)
	}
	requireScoreGolden(t, "baseline", result)
}

func TestScoreGenuineImprovement(t *testing.T) {
	options := defaultScoreFixtureOptions()
	options.totalMs = 80
	fixture := newScoreFixture(t, options)
	result := Score(fixture.inputs)
	if result.EvalError != nil || !result.Placeable {
		t.Fatalf("improvement not placeable: %+v", result)
	}
	if result.RawScore != 80 || result.NormalizedScore != 125 {
		t.Fatalf("unexpected improved score: %+v", result)
	}
	requireTakeoverEligibility(t, result, true)
	requireScoreGolden(t, "improvement", result)
}

func TestScoreRegression(t *testing.T) {
	options := defaultScoreFixtureOptions()
	options.totalMs = 125
	fixture := newScoreFixture(t, options)
	result := Score(fixture.inputs)
	if result.EvalError != nil || !result.Placeable {
		t.Fatalf("valid regression should remain placeable: %+v", result)
	}
	if result.RawScore != 125 || result.NormalizedScore != 80 {
		t.Fatalf("unexpected regression score: %+v", result)
	}
	requireTakeoverEligibility(t, result, false)
	requireScoreGolden(t, "regression", result)
}

func TestScoreReplicateMedian(t *testing.T) {
	values := []float64{80, 120, 100}
	fixtures := make([]*scoreFixture, 0, len(values))
	for i, value := range values {
		options := defaultScoreFixtureOptions()
		options.evaluationId = fmt.Sprintf("eval-median-%d", i+1)
		options.totalMs = value
		fixtures = append(fixtures, newScoreFixture(t, options))
	}
	baseline := fixtures[0].baselineData
	baseline.Replicates = []ScoreBaselineReplicate{
		{RawScore: 99, SuccessRate: 1, RequestCount: 100, ReceivedBytes: 10_000, FindProvidersLoadP95Ms: 10, FindProvidersPoolP05: 100},
		{RawScore: 101, SuccessRate: 1, RequestCount: 100, ReceivedBytes: 10_000, FindProvidersLoadP95Ms: 10, FindProvidersPoolP05: 100},
		{RawScore: 100, SuccessRate: 1, RequestCount: 100, ReceivedBytes: 10_000, FindProvidersLoadP95Ms: 10, FindProvidersPoolP05: 100},
	}
	writeScoreJSON(t, fixtures[0].baseline, baseline)
	inputs := ScoreInputs{BaselineManifest: fixtures[0].baseline}
	for _, fixture := range fixtures {
		inputs.Runs = append(inputs.Runs, fixture.run)
		inputs.Stderr = append(inputs.Stderr, fixture.stderr)
		inputs.Accounting = append(inputs.Accounting, fixture.accounting)
		inputs.Samples = append(inputs.Samples, fixture.samples)
		inputs.ResourceReports = append(inputs.ResourceReports, fixture.resource)
		inputs.Markers = append(inputs.Markers, fixture.marker)
	}
	result := Score(inputs)
	if result.EvalError != nil || !result.Placeable {
		t.Fatalf("median replicate score failed: %+v", result)
	}
	if result.RawScore != 100 || result.Diagnostics.Baseline.RawScore != 100 || result.NormalizedScore != 100 {
		t.Fatalf("wrong median aggregate: %+v", result.Diagnostics)
	}
}

func TestScoreGateFailures(t *testing.T) {
	tests := []struct {
		name string
		gate string
		make func() scoreFixtureOptions
	}{
		{"G1", "G1_success", func() scoreFixtureOptions { o := defaultScoreFixtureOptions(); o.failures = 4; return o }},
		{"G2", "G2_volume", func() scoreFixtureOptions {
			o := defaultScoreFixtureOptions()
			o.rowCount = 94
			o.accountingBytes = 9400
			return o
		}},
		{"G3", "G3_path_integrity", func() scoreFixtureOptions { o := defaultScoreFixtureOptions(); o.accountingBytes = 9499; return o }},
		{"G4", "G4_matchmaking", func() scoreFixtureOptions { o := defaultScoreFixtureOptions(); o.loadMillis = 12.6; return o }},
		{"G5", "G5_stability", func() scoreFixtureOptions {
			o := defaultScoreFixtureOptions()
			o.stderrExtra = "Unexpected error: injected\n"
			return o
		}},
		{"G6", "G6_resources", func() scoreFixtureOptions { o := defaultScoreFixtureOptions(); o.resource.OomKilled = true; return o }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newScoreFixture(t, test.make())
			result := Score(fixture.inputs)
			requireGate(t, result, test.gate, false)
			if result.Placeable {
				t.Fatalf("result with failed %s is placeable", test.gate)
			}
			if (test.gate == "G5_stability" || test.gate == "G6_resources") &&
				(result.EvalError == nil || result.EvalError.Kind != "submission") {
				t.Fatalf("%s failure lacks typed submission error: %+v", test.gate, result.EvalError)
			}
			requireScoreGolden(t, "gate-"+strings.ToLower(test.name), result)
		})
	}
}

func TestScoreMultipleGateFailures(t *testing.T) {
	options := defaultScoreFixtureOptions()
	options.rowCount = 90
	options.failures = 10
	options.bytesPerRow = 50
	options.accountingBytes = 1000
	options.emptyPool = true
	fixture := newScoreFixture(t, options)
	result := Score(fixture.inputs)
	for _, name := range []string{"G1_success", "G2_volume", "G3_path_integrity", "G4_matchmaking"} {
		requireGate(t, result, name, false)
	}
	requireScoreGolden(t, "multiple-gate-failures", result)
}

func TestScoreRejectsMatchmakingPoolCollapse(t *testing.T) {
	options := defaultScoreFixtureOptions()
	options.poolCount = 89
	fixture := newScoreFixture(t, options)
	result := Score(fixture.inputs)
	requireGate(t, result, "G4_matchmaking", false)
	if result.Placeable {
		t.Fatal("candidate below 90% of baseline pool p05 is placeable")
	}
}

func TestScoreNonPlaceableImprovementCannotTakeOver(t *testing.T) {
	options := defaultScoreFixtureOptions()
	options.totalMs = 80
	options.stderrExtra = "Unexpected error: injected\n"
	fixture := newScoreFixture(t, options)
	result := Score(fixture.inputs)
	if result.RawScore != 80 || result.Placeable || result.EvalError == nil {
		t.Fatalf("unexpected failed improved result: %+v", result)
	}
	requireGate(t, result, "G5_stability", false)
	requireTakeoverEligibility(t, result, false)
	requireScoreGolden(t, "improved-nonplaceable", result)
}

func TestScoreGateBoundariesInclusive(t *testing.T) {
	baseline := baselineAggregate{ScoreAggregateDiagnostics: ScoreAggregateDiagnostics{
		RawScore: 100, SuccessRate: 0.98, RequestCount: 100, ReceivedBytes: 10_000, FindProvidersLoadP95Ms: 10, FindProvidersPoolP05: 100,
	}}
	candidate := ScoreAggregateDiagnostics{
		RawScore: 99, SuccessRate: 0.97, RequestCount: 95, ReceivedBytes: 9500,
		AccountingCoverage: 0.95, FindProvidersLoadP95Ms: 12.5, FindProvidersPoolP05: 90,
	}
	replicates := []candidateReplicate{{
		diagnostics: ScoreReplicateDiagnostics{FindProvidersSamples: 1, FindProvidersPoolP05: 90},
		stabilityOK: true,
		resourcesOK: true,
	}}
	gates := buildScoreGates(baseline, candidate, replicates)
	for name, gate := range gates {
		if !gate.Passed {
			t.Fatalf("inclusive boundary failed %s: %+v", name, gate)
		}
	}
	takeoverEligible := true
	result := &ScoreResult{
		ScoreSchema:     apexScoreSchema,
		RawScore:        candidate.RawScore,
		NormalizedScore: clampScore(100*baseline.RawScore/candidate.RawScore, 1, 200),
		Placeable:       allScoreGatesPass(gates),
		Gates:           gates,
		Diagnostics: ScoreDiagnostics{
			RoundId:                  "boundary-fixture",
			ReplicateCount:           1,
			RequestTimeoutMs:         1000,
			TakeoverMargin:           0.01,
			BaselineTakeoverRawMax:   99,
			BaselineTakeoverEligible: &takeoverEligible,
			Baseline:                 &baseline.ScoreAggregateDiagnostics,
			Candidate:                &candidate,
			Replicates:               []ScoreReplicateDiagnostics{replicates[0].diagnostics},
		},
	}
	requireScoreGolden(t, "boundaries", result)
}

func TestScoreMalformedInputsFailClosed(t *testing.T) {
	t.Run("incomplete run is submission error", func(t *testing.T) {
		fixture := newScoreFixture(t, defaultScoreFixtureOptions())
		fixture.runStats.CompletionState = "incomplete"
		fixture.runStats.IncompleteCode = "panic"
		if err := writeRunStats(scoreSidecarForCSV(fixture.run), fixture.runStats); err != nil {
			t.Fatal(err)
		}
		result := Score(fixture.inputs)
		if result.EvalError == nil || result.EvalError.Kind != "submission" || result.Placeable {
			t.Fatalf("incomplete run classification: %+v", result)
		}
	})

	t.Run("legacy run schema", func(t *testing.T) {
		fixture := newScoreFixture(t, defaultScoreFixtureOptions())
		fixture.runStats.Schema = 1
		if err := writeRunStats(scoreSidecarForCSV(fixture.run), fixture.runStats); err != nil {
			t.Fatal(err)
		}
		result := Score(fixture.inputs)
		if result.EvalError == nil || result.EvalError.Kind != "infrastructure" || result.Placeable {
			t.Fatalf("legacy schema scored: %+v", result)
		}
	})

	t.Run("OOM before sidecar is submission error", func(t *testing.T) {
		fixture := newScoreFixture(t, defaultScoreFixtureOptions())
		if err := os.Remove(scoreSidecarForCSV(fixture.run)); err != nil {
			t.Fatal(err)
		}
		resource := defaultScoreFixtureOptions().resource
		resource.EvaluationId = fixture.runStats.EvaluationId
		resource.OomKilled = true
		writeScoreJSON(t, fixture.resource, resource)
		result := Score(fixture.inputs)
		if result.EvalError == nil || result.EvalError.Kind != "submission" || result.Placeable {
			t.Fatalf("pre-sidecar OOM classification: %+v", result)
		}
	})

	t.Run("empty csv", func(t *testing.T) {
		fixture := newScoreFixture(t, defaultScoreFixtureOptions())
		if err := os.WriteFile(fixture.run, []byte("t_start_ms,client,path,depth,status,bytes,ttfb_ms,total_ms,bytes_per_s\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		result := Score(fixture.inputs)
		if result.EvalError == nil || result.Placeable {
			t.Fatalf("empty CSV scored: %+v", result)
		}
		requireScoreGolden(t, "empty-csv", result)
	})

	t.Run("malformed samples", func(t *testing.T) {
		fixture := newScoreFixture(t, defaultScoreFixtureOptions())
		if err := os.WriteFile(fixture.samples, []byte("not xz"), 0o644); err != nil {
			t.Fatal(err)
		}
		result := Score(fixture.inputs)
		if result.EvalError == nil || result.Placeable {
			t.Fatalf("malformed samples scored: %+v", result)
		}
		requireScoreGolden(t, "malformed-samples", result)
	})

	t.Run("NaN csv", func(t *testing.T) {
		fixture := newScoreFixture(t, defaultScoreFixtureOptions())
		content, err := os.ReadFile(fixture.run)
		if err != nil {
			t.Fatal(err)
		}
		content = []byte(strings.Replace(string(content), ",100.000,1000\n", ",NaN,1000\n", 1))
		if err := os.WriteFile(fixture.run, content, 0o644); err != nil {
			t.Fatal(err)
		}
		result := Score(fixture.inputs)
		if result.EvalError == nil || result.Placeable {
			t.Fatalf("NaN CSV scored: %+v", result)
		}
		requireScoreGolden(t, "nan-csv", result)
	})

	t.Run("Inf csv", func(t *testing.T) {
		fixture := newScoreFixture(t, defaultScoreFixtureOptions())
		content, err := os.ReadFile(fixture.run)
		if err != nil {
			t.Fatal(err)
		}
		content = []byte(strings.Replace(string(content), ",100.000,1000\n", ",Inf,1000\n", 1))
		if err := os.WriteFile(fixture.run, content, 0o644); err != nil {
			t.Fatal(err)
		}
		result := Score(fixture.inputs)
		if result.EvalError == nil || result.Placeable {
			t.Fatalf("Inf CSV scored: %+v", result)
		}
		requireScoreGolden(t, "inf-csv", result)
	})

	t.Run("mixed results csv", func(t *testing.T) {
		fixture := newScoreFixture(t, defaultScoreFixtureOptions())
		content, err := os.ReadFile(fixture.run)
		if err != nil {
			t.Fatal(err)
		}
		content = []byte(strings.Replace(string(content), ",100.000,1000\n", ",101.000,1000\n", 1))
		if err := os.WriteFile(fixture.run, content, 0o644); err != nil {
			t.Fatal(err)
		}
		result := Score(fixture.inputs)
		if result.EvalError == nil || result.EvalError.Code != "results_identity_mismatch" || result.Placeable {
			t.Fatalf("mixed results CSV scored: %+v", result)
		}
	})

	t.Run("mixed samples", func(t *testing.T) {
		fixture := newScoreFixture(t, defaultScoreFixtureOptions())
		content, err := os.ReadFile(fixture.samples)
		if err != nil {
			t.Fatal(err)
		}
		other := filepath.Join(fixture.dir, "other-samples.pb.xz")
		if err := os.WriteFile(other, content, 0o644); err != nil {
			t.Fatal(err)
		}
		fixture.inputs.Samples[0] = other
		result := Score(fixture.inputs)
		if result.EvalError == nil || result.EvalError.Code != "sample_identity_mismatch" || result.Placeable {
			t.Fatalf("mixed sample corpus scored: %+v", result)
		}
	})
}

func TestScoreLegacyLogNoise(t *testing.T) {
	fixture := newScoreFixture(t, defaultScoreFixtureOptions())
	content, err := os.ReadFile(fixture.run)
	if err != nil {
		t.Fatal(err)
	}
	newline := strings.IndexByte(string(content), '\n')
	withNoise := append([]byte{}, content[:newline+1]...)
	withNoise = append(withNoise, []byte("[legacy sdk] harmless diagnostic with,commas\n")...)
	withNoise = append(withNoise, content[newline+1:]...)
	if err := os.WriteFile(fixture.run, withNoise, 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.runStats.Rows = 100
	result := Score(fixture.inputs)
	if result.EvalError != nil || !result.Placeable {
		t.Fatalf("legacy log noise rejected: %+v", result)
	}
	requireScoreGolden(t, "baseline", result)
}

func TestReadScoreCSVRejectsNonCanonicalInput(t *testing.T) {
	validRow := "1000,client,/,0,200,100,10,100,1000"
	tests := []struct {
		name    string
		content string
	}{
		{"arbitrary prefix", "untrusted prefix\n" + resultCSVHeader + "\n" + validRow + "\n"},
		{"arbitrary bracketed line", resultCSVHeader + "\n[not legacy] ignored?\n" + validRow + "\n"},
		{"reordered header", "client,t_start_ms,path,depth,status,bytes,ttfb_ms,total_ms,bytes_per_s\n" + validRow + "\n"},
		{"duplicate header", "t_start_ms,client,path,depth,status,bytes,ttfb_ms,total_ms,total_ms\n" + validRow + "\n"},
		{"short row", resultCSVHeader + "\n1000,client,/,0,200,100,10,100\n"},
		{"long row", resultCSVHeader + "\n" + validRow + ",extra\n"},
		{"empty client", resultCSVHeader + "\n1000,,/,0,200,100,10,100,1000\n"},
		{"empty path", resultCSVHeader + "\n1000,client,,0,200,100,10,100,1000\n"},
		{"negative depth", resultCSVHeader + "\n1000,client,/,-1,200,100,10,100,1000\n"},
		{"NaN throughput", resultCSVHeader + "\n1000,client,/,0,200,100,10,100,NaN\n"},
		{"Inf throughput", resultCSVHeader + "\n1000,client,/,0,200,100,10,100,Inf\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "results.csv")
			if err := os.WriteFile(path, []byte(test.content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := readScoreCSV(path); err == nil {
				t.Fatal("non-canonical results CSV was accepted")
			} else {
				var coded *scoreCodedError
				if !errors.As(err, &coded) || coded.code != "invalid_results_csv" {
					t.Fatalf("error = %v, want invalid_results_csv", err)
				}
			}
		})
	}
}

func TestReadScoreCSVAllowsOnlyKnownLegacyNoise(t *testing.T) {
	content := strings.Join([]string{
		"I0816 00:00:00.000000 1 legacy.go:1] glog diagnostic",
		"[legacy sdk] diagnostic before header",
		resultCSVHeader,
		"[legacy sdk] diagnostic after header,with,commas",
		"1000,client,/,0,200,100,10,100,1000",
		"",
	}, "\n")
	path := filepath.Join(t.TempDir(), "results.csv")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	rows, _, _, err := readScoreCSV(path)
	if err != nil || len(rows) != 1 {
		t.Fatalf("known legacy noise: rows=%d err=%v", len(rows), err)
	}
}

func TestDecodeStrictJSONRejectsDuplicateKeysAtEveryDepth(t *testing.T) {
	for name, content := range map[string]string{
		"top level":     `{"a":1,"a":2}`,
		"nested object": `{"a":{"b":1,"b":2}}`,
		"array object":  `{"a":[{"b":1,"b":2}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var target any
			if err := decodeStrictJSONBytes([]byte(content), &target, "fixture"); err == nil {
				t.Fatal("duplicate JSON key was accepted")
			}
		})
	}

	var target any
	if err := decodeStrictJSONBytes(
		[]byte(`{"a":[{"b":1},{"b":2}]}`),
		&target,
		"fixture",
	); err != nil {
		t.Fatalf("unique nested JSON rejected: %v", err)
	}
}

func TestScoreDeterministicJSON(t *testing.T) {
	fixture := newScoreFixture(t, defaultScoreFixtureOptions())
	a, err := marshalScoreResult(Score(fixture.inputs))
	if err != nil {
		t.Fatal(err)
	}
	b, err := marshalScoreResult(Score(fixture.inputs))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatal("identical artifact bytes produced different scorer JSON")
	}
}

func TestScoreCLIParity(t *testing.T) {
	if testing.Short() {
		t.Skip("builds scorer binary")
	}
	fixture := newScoreFixture(t, defaultScoreFixtureOptions())
	want, err := marshalScoreResult(Score(fixture.inputs))
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "sim-latency")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build scorer: %v\n%s", err, output)
	}
	command := exec.Command(
		binary,
		"score",
		"--run", fixture.run,
		"--stderr", fixture.stderr,
		"--baseline", fixture.baseline,
		"--accounting", fixture.accounting,
		"--samples", fixture.samples,
		"--resource-report", fixture.resource,
		"--marker", fixture.marker,
	)
	got, err := command.Output()
	if err != nil {
		t.Fatalf("score CLI: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("CLI/core mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}
}
