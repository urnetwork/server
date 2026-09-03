package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

type subtensorConvergenceFixture struct {
	lag           float64
	netRate       float64
	targetRate    float64
	importRate    float64
	importSeconds float64
	queuedBlocks  float64
	sampleCount   float64
	sampleAge     float64
}

func TestSubtensorConvergenceSignalSyntheticDetectsSlowSerialImport(t *testing.T) {
	now := time.Date(2026, 9, 3, 6, 35, 0, 0, time.UTC)
	alerts, err := runSubtensorConvergenceFixture(t, now, subtensorConvergenceFixture{
		lag: 1_398_810, netRate: 0.461772, targetRate: 0.081669,
		importRate: 0.543441, importSeconds: 1.833538,
		queuedBlocks: 2112, sampleCount: 240, sampleAge: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "subtensor-slow-convergence")
	if alert.SignalNumber != "17.5" || alert.SignalKey != "subtensor-convergence" ||
		alert.Frame != "lightnode" || alert.Sustain != 3 {
		t.Fatalf("slow-convergence identity = %+v", alert)
	}
	for _, want := range []string{
		"estimated 35.1 days",
		"window=1h",
		"lag=1398810",
		"net_blocks_per_second=0.461772",
		"imported_blocks_per_second=0.543441",
		"seconds_per_imported_block=1.833538",
		"queued_blocks=2112",
		"import_worker_busy_pct=99.6",
		"eta_days=35.060",
		"serial historical block import rather than peer supply",
		"official v452 finney checkpoint is not present in the v452 testfinney chain spec",
		"do not add peers or restart the same generation",
		"faster single-core/storage hardware",
		"SIGNALS.md §17.5",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("slow convergence alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestSubtensorConvergenceSignalSyntheticDetectsAdvancingButDivergingNode(t *testing.T) {
	now := time.Date(2026, 9, 3, 6, 35, 0, 0, time.UTC)
	alerts, err := runSubtensorConvergenceFixture(t, now, subtensorConvergenceFixture{
		lag: 1_400_000, netRate: -0.05, targetRate: 0.08,
		importRate: 0.03, importSeconds: 2,
		queuedBlocks: 0, sampleCount: 240, sampleAge: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "subtensor-nonconverging")
	if !strings.Contains(alert.Symptom, "did not reduce") ||
		!strings.Contains(alert.Observed, "eta_days=non-converging") {
		t.Fatalf("non-converging alert lost the slope boundary: %+v", alert)
	}
	for _, candidate := range alerts {
		if candidate.Class == "subtensor-slow-convergence" {
			t.Fatalf("diverging node emitted the weaker slow class: %+v", candidate)
		}
	}
}

func TestSubtensorConvergenceSignalSyntheticAcceptsBoundedETA(t *testing.T) {
	now := time.Date(2026, 9, 3, 6, 35, 0, 0, time.UTC)
	alerts, err := runSubtensorConvergenceFixture(t, now, subtensorConvergenceFixture{
		lag: 100_000, netRate: 2, targetRate: 0.08,
		importRate: 2.08, importSeconds: 0.2,
		queuedBlocks: 50, sampleCount: 240, sampleAge: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("bounded convergence ETA produced alerts: %+v", alerts)
	}
}

func TestSubtensorConvergenceSignalSyntheticTreatsShortHistoryAsUnknown(t *testing.T) {
	now := time.Date(2026, 9, 3, 6, 35, 0, 0, time.UTC)
	_, err := runSubtensorConvergenceFixture(t, now, subtensorConvergenceFixture{
		lag: 1_400_000, netRate: 0.5, targetRate: 0.08,
		importRate: 0.58, importSeconds: 1.7,
		queuedBlocks: 2112, sampleCount: 30, sampleAge: 5,
	})
	if err == nil || !strings.Contains(err.Error(), "30 one-hour samples, want at least 200") {
		t.Fatalf("short-history error = %v", err)
	}
}

func TestSubtensorConvergenceSignalSyntheticPrioritizesStaleSourceOverBrokenSlope(t *testing.T) {
	now := time.Date(2026, 9, 3, 8, 1, 0, 0, time.UTC)
	_, err := runSubtensorConvergenceFixture(t, now, subtensorConvergenceFixture{
		lag: 1_401_819, netRate: 18.450862, targetRate: -17.904045,
		importRate: 0.353584, importSeconds: 1.827060,
		queuedBlocks: 2112, sampleCount: 143, sampleAge: 260,
	})
	if err == nil || !strings.Contains(err.Error(), "source sample is 260s old") {
		t.Fatalf("stale broken-slope error = %v", err)
	}
	if strings.Contains(err.Error(), "inconsistent one-hour measures") || strings.Contains(err.Error(), "143 one-hour samples") {
		t.Fatalf("stale source was obscured by a derived value: %v", err)
	}
}

func TestSubtensorConvergenceValidationOrderAndMissingNamesAreDeterministic(t *testing.T) {
	targets := map[string]subtensorConvergenceTarget{
		"snow\x00subtensor-lightnode": {host: "snow", job: "subtensor-lightnode"},
		"snow\x00subtensor":           {host: "snow", job: "subtensor"},
	}
	wantKeys := []string{"snow\x00subtensor", "snow\x00subtensor-lightnode"}
	gotKeys := sortedSubtensorConvergenceTargetKeys(targets)
	if strings.Join(gotKeys, "|") != strings.Join(wantKeys, "|") {
		t.Fatalf("target validation order = %q, want %q", gotKeys, wantKeys)
	}
	missing := missingSubtensorConvergenceMeasures(
		subtensorConvergenceNetRate |
			subtensorConvergenceTargetRate |
			subtensorConvergenceImportRate |
			subtensorConvergenceImportSeconds |
			subtensorConvergenceSamples,
	)
	if got, want := strings.Join(missing, ","), "lag,queued_blocks,sample_age"; got != want {
		t.Fatalf("missing measure names = %q, want %q", got, want)
	}
}

func TestSubtensorConvergenceQueryUsesExactFreshOneHourSourceSeries(t *testing.T) {
	query := subtensorConvergenceQuery(
		"main",
		[]string{"snow"},
		[]string{"subtensor", "subtensor-lightnode"},
	)
	for _, want := range []string{
		`env="main"`,
		`host=~"^(?:snow)$"`,
		`job=~"^(?:subtensor|subtensor-lightnode)$"`,
		`status="best"`,
		`status="sync_target"`,
		`deriv(`,
		`[1h]`,
		`substrate_block_verification_and_import_time_count`,
		`substrate_block_verification_and_import_time_sum`,
		`substrate_sync_queued_blocks`,
		`count_over_time(`,
		`timestamp(`,
		`"monitor_measure","lag"`,
		`"monitor_measure","sample_age"`,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("convergence query missing %q:\n%s", want, query)
		}
	}
}

func runSubtensorConvergenceFixture(t testing.TB, now time.Time, fixture subtensorConvergenceFixture) (Alerts, error) {
	t.Helper()
	payload := subtensorConvergenceFixtureJSON(t, now, "snow", "subtensor-lightnode", fixture)
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics-1" {
			return "", fmt.Errorf("unexpected metrics host %s", host.Name)
		}
		if !strings.Contains(command, "/prometheus/api/v1/query?query=") {
			return "", fmt.Errorf("unexpected command %q", command)
		}
		parsed, err := url.Parse(strings.Trim(strings.TrimPrefix(command, "curl -fsS --max-time 15 "), "'"))
		if err != nil {
			return "", err
		}
		query := parsed.Query().Get("query")
		if !strings.Contains(query, `job=~"^(?:subtensor-lightnode)$"`) {
			return "", fmt.Errorf("query lost exact lightnode identity: %s", query)
		}
		return payload, nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.Hosts = append(settings.Hosts,
		HostSettings{Name: "metrics-1", Roles: []string{"services"}},
		HostSettings{
			Name: "snow", Roles: []string{"subtensor"},
			Subtensor: &SubtensorHostSettings{
				WarpMaxLag: 4096,
				Nodes: []SubtensorNodeSettings{{
					Name: "lightnode", SyncMode: "warp", ContainerName: "subtensor-lightnode",
				}},
			},
		},
	)
	return NewSubtensorConvergenceSignal().Run(context.Background(), settings)
}

func subtensorConvergenceFixtureJSON(t testing.TB, now time.Time, host, job string, fixture subtensorConvergenceFixture) string {
	t.Helper()
	values := []struct {
		name  string
		value float64
	}{
		{name: "lag", value: fixture.lag},
		{name: "net_rate", value: fixture.netRate},
		{name: "target_rate", value: fixture.targetRate},
		{name: "import_rate", value: fixture.importRate},
		{name: "import_seconds", value: fixture.importSeconds},
		{name: "queued_blocks", value: fixture.queuedBlocks},
		{name: "sample_count", value: fixture.sampleCount},
		{name: "sample_age", value: fixture.sampleAge},
	}
	result := make([]map[string]any, 0, len(values))
	for _, value := range values {
		result = append(result, map[string]any{
			"metric": map[string]string{
				"host": host, "job": job, "monitor_measure": value.name,
			},
			"value": []any{
				float64(now.Unix()),
				strconv.FormatFloat(value.value, 'f', -1, 64),
			},
		})
	}
	document := map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": "vector",
			"result":     result,
		},
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
