package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
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
	targetSamples float64
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
		"chain=synthetic-chain",
		"trusted_target_sample_count=240",
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

func TestSubtensorSlowConvergenceDoesNotClaimCurrentProgressAcrossStaticHead(t *testing.T) {
	now := time.Date(2026, 9, 8, 22, 36, 0, 0, time.UTC)
	alerts, err := runSubtensorConvergenceFixture(t, now, subtensorConvergenceFixture{
		lag: 491_951, netRate: 0.292253, targetRate: 0.083160,
		importRate: 0.473083, importSeconds: 0.714288,
		queuedBlocks: 0, sampleCount: 240, sampleAge: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	convergence := requireAlertClass(t, alerts, "subtensor-slow-convergence")

	target := &host{name: "subtensor.example.test", subtensor: &SubtensorHostSettings{WarpMaxLag: 4096}}
	configured := SubtensorNodeSettings{Name: "lightnode", SyncMode: "warp"}
	node := healthySubtensorNode("lightnode", "warp", 9947, 9946, 7_473_275, 7_473_275)
	current := findingByClass(t, evaluateSubtensorNode(target, configured, node, 7_965_223, nil), "subtensor-progress")
	if current.healthy {
		t.Fatal("equal current heads did not produce the current-progress control")
	}

	markdown := convergence.Markdown()
	for _, want := range []string{
		"trailing-one-hour best-head slope remains positive",
		"does not prove the current head is advancing",
		"co-resident subtensor-progress finding is the stronger current-state signal",
		"follow that current static-head boundary first",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("slow-convergence alert missing current-progress precedence %q:\n%s", want, markdown)
		}
	}
	for _, forbidden := range []string{"The node head is advancing", "Preserve the progressing generation"} {
		if strings.Contains(markdown, forbidden) {
			t.Fatalf("slow-convergence alert claimed current progress with a static control %q:\n%s", forbidden, markdown)
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
	alerts, err := runSubtensorConvergenceFixture(t, now, subtensorConvergenceFixture{
		lag: 1_400_000, netRate: 0.5, targetRate: 0.08,
		importRate: 0.58, importSeconds: 1.7,
		queuedBlocks: 2112, sampleCount: 30, sampleAge: 5,
	})
	if err != nil || len(alerts) != 1 || alerts[0].Class != "cannot-observe" || !strings.Contains(alerts[0].Observed, "generation_state=metrics-insufficient-history") {
		t.Fatalf("short history must be explicit unknown: alerts=%+v err=%v", alerts, err)
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
		"subtensor.example.test\x00subtensor-lightnode": {host: "subtensor.example.test", job: "subtensor-lightnode"},
		"subtensor.example.test\x00subtensor":           {host: "subtensor.example.test", job: "subtensor"},
	}
	wantKeys := []string{"subtensor.example.test\x00subtensor", "subtensor.example.test\x00subtensor-lightnode"}
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
	if got, want := strings.Join(missing, ","), "lag,queued_blocks,sample_age,target_sample_count"; got != want {
		t.Fatalf("missing measure names = %q, want %q", got, want)
	}
}

func TestSubtensorConvergenceQueryUsesExactFreshOneHourSourceSeries(t *testing.T) {
	query := subtensorConvergenceQuery(
		"main",
		map[string]subtensorConvergenceTarget{
			"subtensor.example.test\x00subtensor":           {host: "subtensor.example.test", job: "subtensor"},
			"subtensor.example.test\x00subtensor-lightnode": {host: "subtensor.example.test", job: "subtensor-lightnode"},
		},
	)
	for _, want := range []string{
		`env="main"`,
		`host="subtensor.example.test"`,
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
		`"monitor_measure","target_sample_count"`,
		`max by (host,chain)`,
		`substrate_sub_libp2p_is_major_syncing`,
		`[1h:15s]`,
		` >= time() - 90`,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("convergence query missing %q:\n%s", want, query)
		}
	}
}

func runSubtensorConvergenceFixture(t testing.TB, now time.Time, fixture subtensorConvergenceFixture) (Alerts, error) {
	t.Helper()
	payload := subtensorConvergenceFixtureJSON(t, now, "subtensor.example.test", "subtensor-lightnode", fixture)
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(payload), &response); err != nil {
		t.Fatal(err)
	}
	for _, series := range response.Data.Result {
		series.Metric["chain"] = "synthetic-chain"
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	payload = string(encoded)
	generation := subtensorConvergenceGenerationTestIdentity(t, now.Add(-2*time.Hour))
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Subtensor != nil && strings.Contains(command, "/usr/local/sbin/subtensor-monitor "+shellSingleQuote("subtensor-lightnode")) {
			return generation, nil
		}
		if host.Name != "metrics.example.test" {
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
		HostSettings{Name: "metrics.example.test", Roles: []string{"services"}},
		HostSettings{
			Name: "subtensor.example.test", Roles: []string{"subtensor"},
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

func TestSubtensorConvergenceRejectsShortCanonicalTargetHistory(t *testing.T) {
	now := time.Date(2026, 9, 5, 6, 0, 0, 0, time.UTC)
	alerts, err := runSubtensorConvergenceFixture(t, now, subtensorConvergenceFixture{
		lag: 100_000, netRate: 3, targetRate: 0.08, importRate: 3.08, importSeconds: 0.2,
		sampleCount: 240, sampleAge: 0, targetSamples: 199,
	})
	if err != nil || len(alerts) != 1 || alerts[0].Class != "cannot-observe" || !strings.Contains(alerts[0].Observed, "generation_state=metrics-insufficient-history") {
		t.Fatalf("short canonical history must be explicit unknown: alerts=%+v err=%v", alerts, err)
	}
}

func TestSubtensorConvergenceAcceptsCaughtUpNodeWithNoImports(t *testing.T) {
	now := time.Date(2026, 9, 5, 6, 0, 0, 0, time.UTC)
	alerts, err := runSubtensorConvergenceFixture(t, now, subtensorConvergenceFixture{
		lag: 0, netRate: 0, targetRate: 0, importRate: 0, importSeconds: 0,
		sampleCount: 240, sampleAge: 0, targetSamples: 240,
	})
	if err != nil || len(alerts) != 0 {
		t.Fatalf("caught-up zero-work observation: alerts=%d err=%v", len(alerts), err)
	}
}

func TestSubtensorConvergenceParserRejectsMixedAndMissingChainsAndDuplicates(t *testing.T) {
	now := time.Date(2026, 9, 5, 6, 0, 0, 0, time.UTC)
	fixture := subtensorConvergenceFixture{lag: 10, netRate: 1, targetRate: 0.08, importRate: 1.08, importSeconds: 0.2, sampleCount: 240}
	raw := subtensorConvergenceFixtureJSON(t, now, "subtensor.example.test", "subtensor", fixture)
	targets := map[string]subtensorConvergenceTarget{"subtensor.example.test\x00subtensor": {host: "subtensor.example.test", job: "subtensor"}}
	for _, test := range []struct{ name, raw, want string }{
		{name: "mixed", raw: strings.Replace(raw, `"chain":"synthetic-chain"`, `"chain":"synthetic-other-chain"`, 1), want: "mixed chain"},
		{name: "missing", raw: strings.ReplaceAll(raw, `"chain":"synthetic-chain",`, ""), want: "missing or mixed chain"},
	} {
		_, err := parseSubtensorConvergence(test.raw, targets, now)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: err=%v", test.name, err)
		}
	}
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		t.Fatal(err)
	}
	response.Data.Result = append(response.Data.Result, response.Data.Result[0])
	duplicated, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseSubtensorConvergence(string(duplicated), targets, now); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate observation error = %v", err)
	}
}

func TestSubtensorConvergenceQueryPreservesConfiguredHostJobPairs(t *testing.T) {
	query := subtensorConvergenceQuery("main", map[string]subtensorConvergenceTarget{
		"subtensor.example.test\x00archive": {host: "subtensor.example.test", job: "archive"},
		"other.example.test\x00light":       {host: "other.example.test", job: "light"},
	})
	for _, want := range []string{`host="subtensor.example.test",job=~"^(?:archive)$"`, `host="other.example.test",job=~"^(?:light)$"`} {
		if !strings.Contains(query, want) {
			t.Errorf("missing inventory pair %s", want)
		}
	}
	if strings.Contains(query, `host=~`) || strings.Contains(query, `archive|light`) || strings.Contains(query, `light|archive`) {
		t.Fatal("convergence query expanded inventory into a host/job Cartesian product")
	}
}

func TestSubtensorConvergenceParserRejectsDifferentChainsAcrossConfiguredJobs(t *testing.T) {
	now := time.Date(2026, 9, 5, 6, 0, 0, 0, time.UTC)
	fixture := subtensorConvergenceFixture{lag: 10, netRate: 1, targetRate: 0.08, importRate: 1.08, importSeconds: 0.2, sampleCount: 240}
	var combined mimirInstantResponse
	for _, source := range []struct{ job, chain string }{
		{job: "subtensor", chain: "synthetic-chain"},
		{job: "subtensor-lightnode", chain: "synthetic-other-chain"},
	} {
		raw := subtensorConvergenceFixtureJSON(t, now, "subtensor.example.test", source.job, fixture)
		raw = strings.ReplaceAll(raw, `"chain":"synthetic-chain"`, `"chain":`+strconv.Quote(source.chain))
		var response mimirInstantResponse
		if err := json.Unmarshal([]byte(raw), &response); err != nil {
			t.Fatal(err)
		}
		combined.Status, combined.Data.ResultType = response.Status, response.Data.ResultType
		combined.Data.Result = append(combined.Data.Result, response.Data.Result...)
	}
	raw, err := json.Marshal(combined)
	if err != nil {
		t.Fatal(err)
	}
	targets := map[string]subtensorConvergenceTarget{
		"subtensor.example.test\x00subtensor":           {host: "subtensor.example.test", job: "subtensor"},
		"subtensor.example.test\x00subtensor-lightnode": {host: "subtensor.example.test", job: "subtensor-lightnode"},
	}
	if _, err := parseSubtensorConvergence(string(raw), targets, now); err == nil || !strings.Contains(err.Error(), "configured jobs on subtensor.example.test expose different chains") {
		t.Fatalf("cross-job chain ambiguity error = %v", err)
	}
}

func subtensorConvergenceFixtureJSON(t testing.TB, now time.Time, host, job string, fixture subtensorConvergenceFixture) string {
	t.Helper()
	if fixture.targetSamples == 0 {
		fixture.targetSamples = fixture.sampleCount
	}
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
		{name: "target_sample_count", value: fixture.targetSamples},
	}
	result := make([]map[string]any, 0, len(values))
	for _, value := range values {
		result = append(result, map[string]any{
			"metric": map[string]string{
				"host": host, "job": job, "chain": "synthetic-chain", "monitor_measure": value.name,
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

// Context-aware synthetic transport exercises the ordinary Signal path without
// native endpoints, credentials, or new product fields in the pre-fix RED.
type subtensorConvergenceGenerationTestSource struct {
	*syntheticSource
	hostCall func(context.Context, HostSettings, string) (string, error)
}

func (self *subtensorConvergenceGenerationTestSource) Host(ctx context.Context, configured HostSettings, command string) (string, error) {
	return self.hostCall(ctx, configured, command)
}

// State capture is synchronized because helper calls may run concurrently.
type subtensorConvergenceGenerationTestCapture struct {
	stateLock      sync.Mutex
	helperCallKVs  map[string]int
	helperCalls    int
	mimirCalls     int
	sourceCalls    int
	pinnedTime     string
	beforeComplete bool
	boundedHelpers bool
}

func subtensorConvergenceGenerationTestSettings(source SignalSource, now time.Time) SignalSettings {
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.Hosts = []HostSettings{
		{Name: "metrics.example.test", Roles: []string{"services"}, LANAddress: "192.0.2.11", OverlayAddress: "198.51.100.11"},
		{
			Name: "subtensor.example.test", Roles: []string{"subtensor"}, LANAddress: "192.0.2.12", OverlayAddress: "198.51.100.12",
			Subtensor: &SubtensorHostSettings{
				WarpMaxLag: 4096,
				Nodes: []SubtensorNodeSettings{
					{Name: "archive", SyncMode: "full", ContainerName: "subtensor"},
					{Name: "lightnode", SyncMode: "warp", ContainerName: "subtensor-lightnode"},
				},
			},
		},
	}
	return settings
}

func subtensorConvergenceGenerationTestPayload(t testing.TB, now time.Time, settings SignalSettings, fixture subtensorConvergenceFixture) string {
	t.Helper()
	var combined mimirInstantResponse
	for _, configured := range settings.Hosts {
		if configured.Subtensor == nil {
			continue
		}
		for _, node := range configured.Subtensor.Nodes {
			job := node.ContainerName
			if job == "" {
				job = node.Name
			}
			var response mimirInstantResponse
			if err := json.Unmarshal([]byte(subtensorConvergenceFixtureJSON(t, now, configured.Name, job, fixture)), &response); err != nil {
				t.Fatal(err)
			}
			for _, series := range response.Data.Result {
				series.Metric["chain"] = "synthetic-chain"
				series.Value[0] = json.RawMessage(strconv.FormatFloat(float64(now.Unix())+float64(now.Nanosecond())/float64(time.Second), 'f', -1, 64))
			}
			combined.Status, combined.Data.ResultType = response.Status, response.Data.ResultType
			combined.Data.Result = append(combined.Data.Result, response.Data.Result...)
		}
	}
	encoded, err := json.Marshal(combined)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func subtensorConvergenceGenerationTestIdentity(t testing.TB, start time.Time) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]string{
		"container_started": start.UTC().Format(time.RFC3339Nano),
		"container_image":   "synthetic-private-image", "data_path": "/synthetic/private/data",
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func subtensorConvergenceGenerationTestTransport(now time.Time, payload string, identity func(HostSettings, string, int) string) (*subtensorConvergenceGenerationTestSource, *subtensorConvergenceGenerationTestCapture) {
	capture := &subtensorConvergenceGenerationTestCapture{helperCallKVs: map[string]int{}, boundedHelpers: true}
	source := &subtensorConvergenceGenerationTestSource{syntheticSource: &syntheticSource{}}
	source.hostCall = func(ctx context.Context, configured HostSettings, command string) (string, error) {
		capture.stateLock.Lock()
		capture.sourceCalls++
		capture.stateLock.Unlock()
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if strings.Contains(command, "/prometheus/api/v1/query?query=") {
			parsed, err := url.Parse(strings.Trim(strings.TrimPrefix(command, "curl -fsS --max-time 15 "), "'"))
			if err != nil {
				return "", err
			}
			capture.stateLock.Lock()
			capture.mimirCalls++
			capture.pinnedTime = parsed.Query().Get("time")
			capture.beforeComplete = capture.helperCalls == 2
			capture.stateLock.Unlock()
			return payload, nil
		}
		if configured.Subtensor != nil {
			for _, node := range configured.Subtensor.Nodes {
				if node.ContainerName == "" || !strings.Contains(command, "/usr/local/sbin/subtensor-monitor "+shellSingleQuote(node.ContainerName)) {
					continue
				}
				key := configured.Name + "\x00" + node.ContainerName
				capture.stateLock.Lock()
				call := capture.helperCallKVs[key]
				capture.helperCallKVs[key] = call + 1
				capture.helperCalls++
				deadline, bounded := ctx.Deadline()
				capture.boundedHelpers = capture.boundedHelpers && bounded && time.Until(deadline) <= time.Minute
				capture.stateLock.Unlock()
				return identity(configured, node.ContainerName, call), nil
			}
		}
		return "", fmt.Errorf("unsupported synthetic observation command")
	}
	return source, capture
}

func subtensorConvergenceGenerationTestHealthyFixture() subtensorConvergenceFixture {
	return subtensorConvergenceFixture{
		lag: 105_000, netRate: 2.5, targetRate: 0.09, importRate: 2.59, importSeconds: 0.15,
		queuedBlocks: 37, sampleCount: 220, targetSamples: 221, sampleAge: 5,
	}
}

func TestSubtensorConvergenceGenerationRejectsYoungCompleteHour(t *testing.T) {
	now := time.Date(2099, 2, 3, 4, 5, 6, 0, time.UTC)
	settings := subtensorConvergenceGenerationTestSettings(nil, now)
	payload := subtensorConvergenceGenerationTestPayload(t, now, settings, subtensorConvergenceGenerationTestHealthyFixture())
	identity := subtensorConvergenceGenerationTestIdentity(t, now.Add(-20*time.Minute))
	source, _ := subtensorConvergenceGenerationTestTransport(now, payload, func(HostSettings, string, int) string { return identity })
	settings.Source = source
	alerts, err := NewSubtensorConvergenceSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 {
		t.Fatalf("cross-generation hour accepted: visibility=%d want=2", len(alerts))
	}
	for _, alert := range alerts {
		if alert.Class != "cannot-observe" || !strings.Contains(alert.Observed, "generation_state=young") || alert.Sustain != 2 || alert.Playbook != "SIGNALS.md §17.5" {
			t.Fatalf("young generation did not retain owning visibility contract: class=%s", alert.Class)
		}
		requireAlertOmits(t, alert, identity, "synthetic-private-image", "/synthetic/private/data", now.Add(-20*time.Minute).Format(time.RFC3339Nano))
	}
}

func TestSubtensorConvergenceGenerationRejectsChangeAcrossQuery(t *testing.T) {
	now := time.Date(2099, 2, 3, 4, 5, 6, 0, time.UTC)
	settings := subtensorConvergenceGenerationTestSettings(nil, now)
	payload := subtensorConvergenceGenerationTestPayload(t, now, settings, subtensorConvergenceGenerationTestHealthyFixture())
	oldIdentity := subtensorConvergenceGenerationTestIdentity(t, now.Add(-2*time.Hour))
	newIdentity := subtensorConvergenceGenerationTestIdentity(t, now.Add(-30*time.Second))
	source, _ := subtensorConvergenceGenerationTestTransport(now, payload, func(_ HostSettings, _ string, call int) string {
		if call == 0 {
			return oldIdentity
		}
		return newIdentity
	})
	settings.Source = source
	alerts, err := NewSubtensorConvergenceSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 {
		t.Fatalf("in-flight generation change accepted: visibility=%d want=2", len(alerts))
	}
	for _, alert := range alerts {
		if alert.Class != "cannot-observe" || !strings.Contains(alert.Observed, "generation_state=changed") {
			t.Fatalf("changed generation classified as %s", alert.Class)
		}
	}
}

func TestSubtensorConvergenceGenerationUnknownIdentityIsNotHealthy(t *testing.T) {
	now := time.Date(2099, 2, 3, 4, 5, 6, 0, time.UTC)
	settings := subtensorConvergenceGenerationTestSettings(nil, now)
	payload := subtensorConvergenceGenerationTestPayload(t, now, settings, subtensorConvergenceGenerationTestHealthyFixture())
	valid := subtensorConvergenceGenerationTestIdentity(t, now.Add(-2*time.Hour))
	for _, test := range []struct{ name, identity string }{
		{name: "missing", identity: `{}`},
		{name: "malformed-start", identity: strings.Replace(valid, now.Add(-2*time.Hour).Format(time.RFC3339Nano), "not-a-time", 1)},
		{name: "future-start", identity: strings.Replace(valid, now.Add(-2*time.Hour).Format(time.RFC3339Nano), now.Add(time.Minute).Format(time.RFC3339Nano), 1)},
		{name: "missing-image", identity: strings.Replace(valid, "synthetic-private-image", "", 1)},
		{name: "missing-data", identity: strings.Replace(valid, "/synthetic/private/data", "", 1)},
		{name: "helper-error", identity: `{"container_error":"synthetic-private-secret"}`},
		{name: "duplicate-start", identity: strings.TrimSuffix(valid, "}") + `,"container_started":"not-a-time"}`},
		{name: "trailing-document", identity: valid + `{}`},
		{name: "null-start", identity: strings.Replace(valid, strconv.Quote(now.Add(-2*time.Hour).Format(time.RFC3339Nano)), "null", 1)},
		{name: "zero-start", identity: strings.Replace(valid, now.Add(-2*time.Hour).Format(time.RFC3339Nano), "0001-01-01T00:00:00Z", 1)},
		{name: "relative-data", identity: strings.Replace(valid, "/synthetic/private/data", "synthetic/private/data", 1)},
		{name: "oversized", identity: strings.TrimSuffix(valid, "}") + `,"padding":"` + strings.Repeat("x", 65*1024) + `"}`},
	} {
		source, _ := subtensorConvergenceGenerationTestTransport(now, payload, func(HostSettings, string, int) string { return test.identity })
		settings.Source = source
		alerts, err := NewSubtensorConvergenceSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatalf("%s: unknown identity returned execution error", test.name)
		}
		if len(alerts) != 2 {
			t.Errorf("unknown generation accepted: case=%s visibility=%d want=2", test.name, len(alerts))
			continue
		}
		for _, alert := range alerts {
			if alert.Class != "cannot-observe" {
				t.Errorf("%s: unknown generation became %s", test.name, alert.Class)
			}
			requireAlertOmits(t, alert, "synthetic-private-secret", "synthetic-private-image", "/synthetic/private/data", "not-a-time")
		}
	}
}

func TestSubtensorConvergenceGenerationAcceptsStableCompleteHour(t *testing.T) {
	now := time.Date(2099, 2, 3, 4, 5, 6, 0, time.UTC)
	settings := subtensorConvergenceGenerationTestSettings(nil, now)
	payload := subtensorConvergenceGenerationTestPayload(t, now, settings, subtensorConvergenceGenerationTestHealthyFixture())
	identity := subtensorConvergenceGenerationTestIdentity(t, now.Add(-2*time.Hour))
	source, _ := subtensorConvergenceGenerationTestTransport(now, payload, func(HostSettings, string, int) string { return identity })
	settings.Source = source
	alerts, err := NewSubtensorConvergenceSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("same-generation healthy control failed: visibility=%d err=%v", len(alerts), err)
	}
}

// An initially qualified generation plus a short metrics history is the exact
// boundary seen after collector warm-up. It must remain a typed unknown rather
// than causing the complete monitor snapshot to exit through generic error
// handling.
func TestSubtensorConvergenceGenerationTreatsAllQualifiedShortHistoryAsUnknown(t *testing.T) {
	now := time.Date(2099, 2, 3, 4, 5, 6, 0, time.UTC)
	settings := subtensorConvergenceGenerationTestSettings(nil, now)
	fixture := subtensorConvergenceGenerationTestHealthyFixture()
	fixture.sampleCount, fixture.targetSamples = 69, 69
	payload := subtensorConvergenceGenerationTestPayload(t, now, settings, fixture)
	identity := subtensorConvergenceGenerationTestIdentity(t, now.Add(-2*time.Hour))
	source, capture := subtensorConvergenceGenerationTestTransport(now, payload, func(HostSettings, string, int) string { return identity })
	settings.Source = source
	alerts, err := NewSubtensorConvergenceSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 2 || capture.helperCalls != 2 || capture.mimirCalls != 1 {
		t.Fatalf("all-qualified short history must remain visible: alerts=%d helpers=%d mimir=%d err=%v", len(alerts), capture.helperCalls, capture.mimirCalls, err)
	}
	for _, alert := range alerts {
		if alert.Class != "cannot-observe" || !strings.Contains(alert.Observed, "generation_state=metrics-insufficient-history") {
			t.Fatalf("short history became %s: %s", alert.Class, alert.Observed)
		}
	}
}

func TestSubtensorConvergenceGenerationPinsAndBoundsSingleBracketedQuery(t *testing.T) {
	now := time.Date(2099, 2, 3, 4, 5, 6, 0, time.UTC)
	settings := subtensorConvergenceGenerationTestSettings(nil, now)
	payload := subtensorConvergenceGenerationTestPayload(t, now, settings, subtensorConvergenceGenerationTestHealthyFixture())
	identity := subtensorConvergenceGenerationTestIdentity(t, now.Add(-2*time.Hour))
	source, capture := subtensorConvergenceGenerationTestTransport(now, payload, func(HostSettings, string, int) string { return identity })
	settings.Source = source
	if _, err := NewSubtensorConvergenceSignal().Run(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	if capture.pinnedTime != now.Format(time.RFC3339Nano) || capture.mimirCalls != 1 || capture.helperCalls != 4 || !capture.beforeComplete || !capture.boundedHelpers {
		t.Fatalf("generation query not pinned/bracketed/bounded: helpers=%d mimir=%d pinned=%t before=%t bounded=%t", capture.helperCalls, capture.mimirCalls, capture.pinnedTime == now.Format(time.RFC3339Nano), capture.beforeComplete, capture.boundedHelpers)
	}
}

func TestSubtensorConvergenceGenerationPreservesQualifiedOtherHost(t *testing.T) {
	now := time.Date(2099, 2, 3, 4, 5, 6, 0, time.UTC)
	settings := subtensorConvergenceGenerationTestSettings(nil, now)
	sibling := settings.Hosts[1]
	sibling.Name, sibling.LANAddress, sibling.OverlayAddress = "sibling.example.test", "192.0.2.13", "198.51.100.13"
	settings.Hosts = append(settings.Hosts, sibling)
	fixture := subtensorConvergenceGenerationTestHealthyFixture()
	fixture.netRate = -0.04
	payload := subtensorConvergenceGenerationTestPayload(t, now, settings, fixture)
	oldIdentity := subtensorConvergenceGenerationTestIdentity(t, now.Add(-2*time.Hour))
	youngIdentity := subtensorConvergenceGenerationTestIdentity(t, now.Add(-20*time.Minute))
	source, _ := subtensorConvergenceGenerationTestTransport(now, payload, func(configured HostSettings, job string, _ int) string {
		if configured.Name == "subtensor.example.test" && job == "subtensor" {
			return youngIdentity
		}
		return oldIdentity
	})
	settings.Source = source
	alerts, err := NewSubtensorConvergenceSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	unknown, retained := 0, 0
	for _, alert := range alerts {
		if alert.Target == "subtensor.example.test" {
			if alert.Class != "cannot-observe" {
				t.Fatalf("unqualified contributor supported same-host convergence: class=%s", alert.Class)
			}
			unknown++
		}
		if alert.Target == "sibling.example.test" && alert.Class == "subtensor-nonconverging" && alert.Sustain == 3 && alert.Severity == SeverityWarn {
			retained++
		}
	}
	if unknown != 2 || retained != 2 || len(settings.Hosts) != 3 {
		t.Fatalf("generation qualification lost full inventory or sibling findings: unknown=%d retained=%d", unknown, retained)
	}
}

func TestSubtensorConvergenceGenerationExcludedHostHasNoHelperContact(t *testing.T) {
	now := time.Date(2099, 2, 3, 4, 5, 6, 0, time.UTC)
	settings := subtensorConvergenceGenerationTestSettings(nil, now)
	payload := subtensorConvergenceGenerationTestPayload(t, now, settings, subtensorConvergenceGenerationTestHealthyFixture())
	identity := subtensorConvergenceGenerationTestIdentity(t, now.Add(-2*time.Hour))
	source, capture := subtensorConvergenceGenerationTestTransport(now, payload, func(HostSettings, string, int) string { return identity })
	settings.Source = source
	settings.ExcludedHosts = []string{"subtensor.example.test"}
	alerts, err := NewSubtensorConvergenceSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "monitor-host-scope-partial")
	if capture.helperCalls != 0 || !strings.Contains(alert.Observed, "blocked_hosts=1") || alert.SignalKey != "subtensor-convergence" || len(settings.Hosts) != 2 {
		t.Fatalf("excluded generation host escaped admission/full-topology visibility: helper_calls=%d", capture.helperCalls)
	}
}

func TestSubtensorConvergenceGenerationRequiresExplicitContainer(t *testing.T) {
	now := time.Date(2099, 2, 3, 4, 5, 6, 0, time.UTC)
	settings := subtensorConvergenceGenerationTestSettings(nil, now)
	settings.Hosts[1].Subtensor.Nodes[0].ContainerName = ""
	payload := subtensorConvergenceGenerationTestPayload(t, now, settings, subtensorConvergenceGenerationTestHealthyFixture())
	identity := subtensorConvergenceGenerationTestIdentity(t, now.Add(-2*time.Hour))
	source, _ := subtensorConvergenceGenerationTestTransport(now, payload, func(HostSettings, string, int) string { return identity })
	settings.Source = source
	alerts, err := NewSubtensorConvergenceSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 2 {
		t.Fatalf("implicit container identity accepted: visibility=%d err=%v", len(alerts), err)
	}
	for _, alert := range alerts {
		if alert.Class != "cannot-observe" || !strings.Contains(alert.Observed, "generation_state=missing-container") {
			t.Fatalf("missing explicit container did not remain visibility: class=%s", alert.Class)
		}
	}
}

func TestSubtensorConvergenceGenerationPreCanceledDoesNotCallSource(t *testing.T) {
	now := time.Date(2099, 2, 3, 4, 5, 6, 0, time.UTC)
	settings := subtensorConvergenceGenerationTestSettings(nil, now)
	payload := subtensorConvergenceGenerationTestPayload(t, now, settings, subtensorConvergenceGenerationTestHealthyFixture())
	identity := subtensorConvergenceGenerationTestIdentity(t, now.Add(-2*time.Hour))
	source, capture := subtensorConvergenceGenerationTestTransport(now, payload, func(HostSettings, string, int) string { return identity })
	settings.Source = source
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	alerts, err := NewSubtensorConvergenceSignal().Run(ctx, settings)
	if err != ctx.Err() || len(alerts) != 0 || capture.sourceCalls != 0 {
		t.Fatalf("pre-canceled generation work invoked source or fabricated state: calls=%d exact_lifecycle=%t", capture.sourceCalls, err == ctx.Err())
	}
}

func TestSubtensorConvergenceGenerationInFlightCancellationIsLifecycle(t *testing.T) {
	now := time.Date(2099, 2, 3, 4, 5, 6, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	var enteredOnce sync.Once
	source := &subtensorConvergenceGenerationTestSource{syntheticSource: &syntheticSource{}}
	source.hostCall = func(commandCtx context.Context, _ HostSettings, _ string) (string, error) {
		enteredOnce.Do(func() { close(entered) })
		<-commandCtx.Done()
		return "", commandCtx.Err()
	}
	settings := subtensorConvergenceGenerationTestSettings(source, now)
	type runResult struct {
		alerts Alerts
		err    error
	}
	done := make(chan runResult, 1)
	go func() {
		alerts, err := NewSubtensorConvergenceSignal().Run(ctx, settings)
		done <- runResult{alerts: alerts, err: err}
	}()
	<-entered
	cancel()
	result := <-done
	if result.err != ctx.Err() || !errors.Is(result.err, context.Canceled) || len(result.alerts) != 0 {
		t.Fatalf("in-flight generation cancellation became fabricated findings or wrapped lifecycle: exact=%t alerts=%d", result.err == ctx.Err(), len(result.alerts))
	}
}

func TestSubtensorConvergenceGenerationRejectsImageAndDataChanges(t *testing.T) {
	now := time.Date(2099, 2, 3, 4, 5, 6, 0, time.UTC)
	settings := subtensorConvergenceGenerationTestSettings(nil, now)
	payload := subtensorConvergenceGenerationTestPayload(t, now, settings, subtensorConvergenceGenerationTestHealthyFixture())
	oldIdentity := subtensorConvergenceGenerationTestIdentity(t, now.Add(-2*time.Hour))
	for _, changedIdentity := range []string{
		strings.Replace(oldIdentity, "synthetic-private-image", "synthetic-replacement-image", 1),
		strings.Replace(oldIdentity, "/synthetic/private/data", "/synthetic/replacement/data", 1),
	} {
		source, capture := subtensorConvergenceGenerationTestTransport(now, payload, func(_ HostSettings, _ string, call int) string {
			if call == 0 {
				return oldIdentity
			}
			return changedIdentity
		})
		settings.Source = source
		alerts, err := NewSubtensorConvergenceSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 2 || capture.mimirCalls != 1 || capture.helperCalls != 4 {
			t.Fatalf("same-start image/data change escaped the bracket: visibility=%d err=%v", len(alerts), err)
		}
		for _, alert := range alerts {
			if alert.Class != "cannot-observe" || !strings.Contains(alert.Observed, "generation_state=changed") {
				t.Fatalf("same-start identity change became %s", alert.Class)
			}
			requireAlertOmits(t, alert, "synthetic-private-image", "synthetic-replacement-image", "/synthetic/private/data", "/synthetic/replacement/data")
		}
	}
}

func TestSubtensorConvergenceGenerationAcceptsExactWindowBoundary(t *testing.T) {
	now := time.Date(2099, 2, 3, 4, 5, 6, 123_000_000, time.UTC)
	settings := subtensorConvergenceGenerationTestSettings(nil, now)
	payload := subtensorConvergenceGenerationTestPayload(t, now, settings, subtensorConvergenceGenerationTestHealthyFixture())
	identity := subtensorConvergenceGenerationTestIdentity(t, now.Add(-time.Hour))
	source, capture := subtensorConvergenceGenerationTestTransport(now, payload, func(HostSettings, string, int) string { return identity })
	settings.Source = source
	alerts, err := NewSubtensorConvergenceSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 || capture.pinnedTime != now.Format(time.RFC3339Nano) || capture.helperCalls != 4 {
		t.Fatalf("exact-hour stable boundary lost millisecond-pinned health: visibility=%d err=%v", len(alerts), err)
	}
}

func TestSubtensorConvergenceGenerationRejectsMismatchedPinnedEvaluation(t *testing.T) {
	now := time.Date(2099, 2, 3, 4, 5, 6, 0, time.UTC)
	settings := subtensorConvergenceGenerationTestSettings(nil, now)
	payload := subtensorConvergenceGenerationTestPayload(t, now.Add(-time.Second), settings, subtensorConvergenceGenerationTestHealthyFixture())
	identity := subtensorConvergenceGenerationTestIdentity(t, now.Add(-2*time.Hour))
	source, capture := subtensorConvergenceGenerationTestTransport(now, payload, func(HostSettings, string, int) string { return identity })
	settings.Source = source
	alerts, err := NewSubtensorConvergenceSignal().Run(context.Background(), settings)
	if err == nil || !strings.Contains(err.Error(), "evaluation does not match pinned time") || len(alerts) != 0 || capture.helperCalls != 2 {
		t.Fatalf("unbound Mimir evaluation accepted: visibility=%d helper_calls=%d", len(alerts), capture.helperCalls)
	}
}

func TestSubtensorConvergenceGenerationRechecksFreshnessAfterBracket(t *testing.T) {
	now := time.Date(2099, 2, 3, 4, 5, 6, 0, time.UTC)
	settings := subtensorConvergenceGenerationTestSettings(nil, now)
	fixture := subtensorConvergenceGenerationTestHealthyFixture()
	fixture.sampleAge = 70
	payload := subtensorConvergenceGenerationTestPayload(t, now, settings, fixture)
	identity := subtensorConvergenceGenerationTestIdentity(t, now.Add(-2*time.Hour))
	var clockLock sync.Mutex
	clock := now
	settings.Now = func() time.Time {
		clockLock.Lock()
		defer clockLock.Unlock()
		return clock
	}
	source, _ := subtensorConvergenceGenerationTestTransport(now, payload, func(_ HostSettings, _ string, call int) string {
		if call == 1 {
			clockLock.Lock()
			clock = now.Add(21 * time.Second)
			clockLock.Unlock()
		}
		return identity
	})
	settings.Source = source
	alerts, err := NewSubtensorConvergenceSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 2 {
		t.Fatalf("post-bracket source freshness lost visibility: count=%d err=%v", len(alerts), err)
	}
	for _, alert := range alerts {
		if alert.Class != "cannot-observe" || !strings.Contains(alert.Observed, "generation_state=source-stale") {
			t.Fatalf("aged source supported %s", alert.Class)
		}
	}
}

func TestSubtensorConvergenceGenerationChildFailurePreservesOtherHost(t *testing.T) {
	now := time.Date(2099, 2, 3, 4, 5, 6, 0, time.UTC)
	settings := subtensorConvergenceGenerationTestSettings(nil, now)
	sibling := settings.Hosts[1]
	sibling.Name, sibling.LANAddress, sibling.OverlayAddress = "sibling.example.test", "192.0.2.13", "198.51.100.13"
	settings.Hosts = append(settings.Hosts, sibling)
	fixture := subtensorConvergenceGenerationTestHealthyFixture()
	fixture.netRate = -0.04
	payload := subtensorConvergenceGenerationTestPayload(t, now, settings, fixture)
	identity := subtensorConvergenceGenerationTestIdentity(t, now.Add(-2*time.Hour))
	source, capture := subtensorConvergenceGenerationTestTransport(now, payload, func(HostSettings, string, int) string { return identity })
	originalHostCall := source.hostCall
	source.hostCall = func(ctx context.Context, configured HostSettings, command string) (string, error) {
		output, err := originalHostCall(ctx, configured, command)
		if configured.Name == "subtensor.example.test" && strings.Contains(command, "/usr/local/sbin/subtensor-monitor ") {
			capture.stateLock.Lock()
			after := capture.helperCallKVs[configured.Name+"\x00subtensor"] > 1 || capture.helperCallKVs[configured.Name+"\x00subtensor-lightnode"] > 1
			capture.stateLock.Unlock()
			if after {
				return "", context.DeadlineExceeded
			}
		}
		return output, err
	}
	settings.Source = source
	alerts, err := NewSubtensorConvergenceSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	unknown, retained := 0, 0
	for _, alert := range alerts {
		if alert.Target == "subtensor.example.test" && alert.Class == "cannot-observe" && strings.Contains(alert.Observed, "generation_state=deadline") {
			unknown++
		}
		if alert.Target == "sibling.example.test" && alert.Class == "subtensor-nonconverging" && alert.Sustain == 3 {
			retained++
		}
	}
	if unknown != 2 || retained != 2 || capture.mimirCalls != 1 || len(settings.Hosts) != 3 {
		t.Fatalf("one child failure erased independent qualification: unknown=%d retained=%d", unknown, retained)
	}
}

func TestSubtensorConvergenceGenerationRejectsUnsupportedContainerWithoutContact(t *testing.T) {
	now := time.Date(2099, 2, 3, 4, 5, 6, 0, time.UTC)
	settings := subtensorConvergenceGenerationTestSettings(nil, now)
	settings.Hosts[1].Subtensor.Nodes[0].ContainerName = "subtensor;synthetic-command"
	payload := subtensorConvergenceGenerationTestPayload(t, now, settings, subtensorConvergenceGenerationTestHealthyFixture())
	identity := subtensorConvergenceGenerationTestIdentity(t, now.Add(-2*time.Hour))
	source, capture := subtensorConvergenceGenerationTestTransport(now, payload, func(HostSettings, string, int) string { return identity })
	settings.Source = source
	alerts, err := NewSubtensorConvergenceSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 2 || capture.helperCalls != 1 || capture.mimirCalls != 0 {
		t.Fatalf("unsupported helper target invoked transport or supported health: visibility=%d helper_calls=%d", len(alerts), capture.helperCalls)
	}
	for _, alert := range alerts {
		if alert.Class != "cannot-observe" || !strings.Contains(alert.Observed, "generation_state=unsupported-container") {
			t.Fatalf("unsupported explicit container became %s", alert.Class)
		}
	}
}

func TestSubtensorConvergenceGenerationPreservesPartialVisibilityOnMetricsFailure(t *testing.T) {
	now := time.Date(2099, 2, 3, 4, 5, 6, 0, time.UTC)
	for _, test := range []struct {
		name    string
		partial bool
		output  string
		err     error
	}{
		{name: "transport", partial: true, err: errors.New("synthetic-private-transport")},
		{name: "parse", partial: true, output: `{"synthetic-private-payload":`},
		{name: "no-partial-transport", partial: false, err: errors.New("synthetic-private-transport")},
	} {
		settings := subtensorConvergenceGenerationTestSettings(nil, now)
		sibling := settings.Hosts[1]
		sibling.Name, sibling.LANAddress, sibling.OverlayAddress = "sibling.example.test", "192.0.2.13", "198.51.100.13"
		settings.Hosts = append(settings.Hosts, sibling)
		payload := subtensorConvergenceGenerationTestPayload(t, now, settings, subtensorConvergenceGenerationTestHealthyFixture())
		identity := subtensorConvergenceGenerationTestIdentity(t, now.Add(-2*time.Hour))
		young := subtensorConvergenceGenerationTestIdentity(t, now.Add(-20*time.Minute))
		source, capture := subtensorConvergenceGenerationTestTransport(now, payload, func(configured HostSettings, _ string, _ int) string {
			if test.partial && configured.Name == "subtensor.example.test" {
				return young
			}
			return identity
		})
		originalHostCall := source.hostCall
		source.hostCall = func(ctx context.Context, configured HostSettings, command string) (string, error) {
			output, err := originalHostCall(ctx, configured, command)
			if strings.Contains(command, "/prometheus/api/v1/query?query=") {
				return test.output, test.err
			}
			return output, err
		}
		settings.Source = source
		alerts, err := NewSubtensorConvergenceSignal().Run(context.Background(), settings)
		if !test.partial {
			if !errors.Is(err, test.err) || len(alerts) != 0 {
				t.Fatalf("non-partial transport error changed: alerts=%d err=%v", len(alerts), err)
			}
			continue
		}
		if err != nil || len(alerts) != 4 || capture.helperCalls != 4 || capture.mimirCalls != 1 {
			t.Fatalf("%s: metrics failure erased established visibility: alerts=%d helpers=%d", test.name, len(alerts), capture.helperCalls)
		}
		youngCount, unavailableCount := 0, 0
		for _, alert := range alerts {
			if alert.Class != "cannot-observe" || alert.Sustain != 2 || alert.Playbook != "SIGNALS.md §17.5" {
				t.Fatalf("%s: partial metrics source inferred %s", test.name, alert.Class)
			}
			if alert.Target == "subtensor.example.test" && strings.Contains(alert.Observed, "generation_state=young") {
				youngCount++
			}
			if alert.Target == "sibling.example.test" && strings.Contains(alert.Observed, "generation_state=metrics-unobservable") {
				unavailableCount++
			}
			requireAlertOmits(t, alert, "synthetic-private-transport", "synthetic-private-payload", "synthetic-private-image", "/synthetic/private/data")
		}
		wantYoung := 0
		// This counter deliberately names only the sibling. In the
		// all-qualified failure case the primary host also has two
		// metrics-unobservable alerts, accounted for by the total above.
		wantUnavailable := 2
		if test.partial {
			wantYoung = 2
		}
		if youngCount != wantYoung || unavailableCount != wantUnavailable || len(settings.Hosts) != 3 {
			t.Fatalf("%s: partial visibility attribution changed: young=%d metrics=%d", test.name, youngCount, unavailableCount)
		}
	}
}
