package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type scoreReadFixture struct {
	now                       time.Time
	rows                      []map[string]any
	census                    map[string][]map[string]any
	warnings                  []string
	raw                       string
	queryErr, censusErr       error
	beforeQuery, beforeCensus func()
	queries, hosts            atomic.Int32
}

func newScoreReadFixture() *scoreReadFixture {
	f := &scoreReadFixture{now: syntheticSettings(nil).Now(), census: map[string][]map[string]any{}}
	for i, host := range []string{"api-a", "api-b"} {
		f.addProcess(host, fmt.Sprintf("%08x", i+1), float64(f.now.Add(-time.Hour).Unix())-float64(i), 0, 0, true)
	}
	return f
}

func (f *scoreReadFixture) addProcess(host, instance string, start, counts, samples float64, successful bool) {
	f.census[host] = append(f.census[host], map[string]any{"service": "api", "block": "blue", "start": start})
	for _, field := range []string{"start", "counts", "samples", "success"} {
		metric := jwtRejectionsStart
		labels := map[string]string{"__name__": metric, "env": "synthetic", "job": "api", "service": "api", "host": host, "block": "blue", "instance": instance}
		if field == "success" {
			labels["__name__"] = providerScoreReadSuccess
		} else if field != "start" {
			labels["__name__"], labels["phase"] = providerScoreReadCounter, field
		}
		values := []any{}
		for i := range 60 {
			value := start
			if field == "counts" || field == "samples" {
				value = 10
				if i == 59 {
					if field == "counts" {
						value += counts
					} else {
						value += samples
					}
				}
			}
			if field == "success" {
				value = 100
				if successful {
					value += float64(i)
				}
			}
			values = append(values, []any{f.now.Add(time.Duration(i-59) * 15 * time.Second).Unix(), fmt.Sprint(value)})
		}
		f.rows = append(f.rows, map[string]any{"metric": labels, "values": values})
	}
}

func (f *scoreReadFixture) row(host, field string) map[string]any {
	for _, row := range f.rows {
		labels := row["metric"].(map[string]string)
		if labels["host"] != host {
			continue
		}
		if field == "start" && labels["__name__"] == jwtRejectionsStart || field == "success" && labels["__name__"] == providerScoreReadSuccess || labels["phase"] == field {
			return row
		}
	}
	return nil
}

func (f *scoreReadFixture) remove(host, field string) {
	target := f.row(host, field)
	if target == nil {
		return
	}
	labels := target["metric"].(map[string]string)
	kept := f.rows[:0]
	for _, row := range f.rows {
		other := row["metric"].(map[string]string)
		if other["host"] != labels["host"] || other["instance"] != labels["instance"] || other["__name__"] != labels["__name__"] || other["phase"] != labels["phase"] {
			kept = append(kept, row)
		}
	}
	f.rows = kept
}

func scoreReadEncode(t testing.TB, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal("cannot encode score-reader control")
	}
	return string(raw)
}

func (f *scoreReadFixture) settings(t *testing.T) SignalSettings {
	t.Helper()
	settings := syntheticSettings(nil)
	settings.Now = func() time.Time { return f.now }
	settings.LogServices = []string{"api"}
	settings.LogServiceHosts = map[string][]string{"api": {"api-a", "api-b"}}
	settings.LogServiceBlocks = map[string][]string{"api": {"blue"}}
	settings.Hosts = []HostSettings{{Name: "api-a", Roles: []string{"api"}}, {Name: "api-b", Roles: []string{"api"}}}
	settings.Source = &providerHttpsTestSource{
		syntheticSource: &syntheticSource{hostTimeoutFn: func(host HostSettings, command string, timeout time.Duration) (string, error) {
			f.hosts.Add(1)
			if f.beforeCensus != nil {
				f.beforeCensus()
			}
			if timeout != jwtRejectionsCensusTimeout || command != "timeout 14s python3 -c "+shellSingleQuote(jwtRejectionsCensusProgram("synthetic", []string{"api"})) {
				t.Error("score reader changed the accepted bounded API-only census")
			}
			if f.censusErr != nil {
				return "", f.censusErr
			}
			return scoreReadEncode(t, map[string]any{"schema": 1, "host": host.Name, "at": f.now.Unix(), "processes": f.census[host.Name]}), nil
		}},
		do: func(request *http.Request) (*http.Response, error) {
			f.queries.Add(1)
			if f.beforeQuery != nil {
				f.beforeQuery()
			}
			user, password, ok := request.BasicAuth()
			if !ok || user != "query-synthetic" || password != "private-query-password" || request.Method != http.MethodPost || request.URL.String() != "https://synthetic-grafana.metrics.example/prometheus/api/v1/query" || request.ParseForm() != nil || request.PostForm.Get("time") != fmt.Sprint(f.now.Unix()) {
				t.Error("score reader lost authenticated fixed-time source authority")
			}
			query := request.PostForm.Get("query")
			want := `{__name__=~"process_start_time_seconds|urnetwork_client_score_read_errors_total|urnetwork_findproviders2_load_seconds_count",env="synthetic",job="api",service="api",host=~"api-a|api-b",block=~"blue",phase=~"counts|samples|"}[15m]`
			if query != want {
				t.Error("score reader changed its exact unaggregated source query")
			}
			raw := f.raw
			if raw == "" {
				raw = scoreReadEncode(t, map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": f.rows}, "warnings": f.warnings})
			}
			return providerHttpsTestResponse(raw), f.queryErr
		},
	}
	providerHttpsTestCredentials(t, &settings)
	return settings
}

func (f *scoreReadFixture) run(t *testing.T) Alerts {
	t.Helper()
	alerts, err := NewProviderScoreReadErrorsSignal().Run(context.Background(), f.settings(t))
	if err != nil {
		t.Fatal("score reader failed before its observation assertion")
	}
	return alerts
}

func TestProviderScoreReadErrorsPositiveAndHealthy(t *testing.T) {
	for _, phase := range []string{"counts", "samples", "both", "none", "all-failed"} {
		f := newScoreReadFixture()
		for _, field := range []string{"counts", "samples"} {
			if phase == field || phase == "both" || phase == "all-failed" {
				values := f.row("api-a", field)["values"].([]any)
				values[59].([]any)[1] = "11"
			}
		}
		if phase == "all-failed" {
			for _, host := range []string{"api-a", "api-b"} {
				for _, point := range f.row(host, "success")["values"].([]any) {
					point.([]any)[1] = "100"
				}
			}
		}
		alerts := f.run(t)
		if phase == "none" {
			if len(alerts) != 0 {
				t.Fatal("complete exercised zero-error window was not healthy")
			}
			continue
		}
		alert := requireAlertClass(t, alerts, "provider-score-read-errors")
		if len(alerts) != 1 || alert.Severity != SeverityWarn || alert.Sustain != 2 || isVisibilityAlert(alert) || NewProviderScoreReadErrorsSignal().Cadence() != 15*time.Minute || f.queries.Load() != 1 || f.hosts.Load() != 2 {
			t.Fatal("positive delta lost WARN2/15m or bounded source ownership")
		}
		requireAlertOmits(t, alert, "api-a", "api-b", "00000001", "private-query-password")
		if !strings.Contains(alert.Context, "not failed keys, unique requests, provider scarcity") || !strings.Contains(alert.Context, "PING and decoding") {
			t.Fatal("phase error became a scarcity or outage diagnosis")
		}
	}
}

func TestProviderScoreReadErrorsResetPartialAndIdentity(t *testing.T) {
	for _, mode := range []string{"missing-phase", "missing-start", "missing-success", "missing-process", "missing-native", "native-extra", "retired-process", "replaced-start", "changed-start", "duplicate-start", "duplicate-phase", "reset", "gap", "stale", "phase-clock", "fractional", "overflow", "future", "source-label", "wrong-env", "wrong-service", "wrong-instance", "warnings", "empty", "duplicate-json", "quiet"} {
		f := newScoreReadFixture()
		switch mode {
		case "missing-phase":
			f.remove("api-b", "samples")
		case "missing-start":
			f.remove("api-b", "start")
		case "missing-success":
			f.remove("api-b", "success")
		case "missing-process":
			for _, field := range []string{"start", "counts", "samples", "success"} {
				f.remove("api-b", field)
			}
		case "missing-native":
			f.census["api-b"] = nil
		case "native-extra":
			f.census["api-b"] = append(f.census["api-b"], map[string]any{"service": "api", "block": "blue", "start": f.now.Add(-10 * time.Minute).Unix()})
		case "retired-process":
			f.census["api-b"][0]["start"] = f.now.Add(-10 * time.Minute).Unix()
		case "replaced-start":
			for _, point := range f.row("api-b", "start")["values"].([]any) {
				point.([]any)[1] = fmt.Sprint(f.now.Add(-10 * time.Minute).Unix())
			}
		case "changed-start":
			f.row("api-b", "start")["values"].([]any)[59].([]any)[1] = fmt.Sprint(f.now.Add(-2 * time.Hour).Unix())
		case "duplicate-start":
			f.addProcess("api-b", "11223344", float64(f.now.Add(-time.Hour).Unix())-1, 0, 0, true)
		case "duplicate-phase":
			f.rows = append(f.rows, f.row("api-b", "counts"))
		case "reset":
			f.row("api-b", "counts")["values"].([]any)[30].([]any)[1] = "0"
		case "gap":
			for _, field := range []string{"start", "counts", "samples", "success"} {
				row := f.row("api-b", field)
				values := row["values"].([]any)
				row["values"] = append(append([]any{}, values[:20]...), values[24:]...)
			}
		case "stale":
			for _, field := range []string{"start", "counts", "samples", "success"} {
				row := f.row("api-b", field)
				row["values"] = row["values"].([]any)[:57]
			}
		case "phase-clock":
			f.row("api-b", "samples")["values"].([]any)[20].([]any)[0] = f.now.Add(-584 * time.Second).Unix()
		case "fractional":
			f.row("api-b", "counts")["values"].([]any)[59].([]any)[1] = "10.5"
		case "overflow":
			f.row("api-b", "counts")["values"].([]any)[59].([]any)[1] = "9007199254740992"
		case "future":
			f.row("api-b", "start")["values"].([]any)[59].([]any)[0] = f.now.Add(time.Second).Unix()
		case "source-label":
			f.row("api-b", "counts")["metric"].(map[string]string)["private"] = "PRIVATE-CANARY"
		case "wrong-env":
			f.row("api-b", "counts")["metric"].(map[string]string)["env"] = "PRIVATE-CANARY"
		case "wrong-service":
			f.row("api-b", "counts")["metric"].(map[string]string)["service"] = "connect"
		case "wrong-instance":
			f.row("api-b", "counts")["metric"].(map[string]string)["instance"] = "PRIVATE-CANARY"
		case "warnings":
			f.warnings = []string{"PRIVATE-CANARY"}
		case "empty":
			f.rows = nil
		case "duplicate-json":
			f.raw = `{"status":"success","status":"success","data":{"resultType":"matrix","result":[]}}`
		case "quiet":
			for _, host := range []string{"api-a", "api-b"} {
				for _, point := range f.row(host, "success")["values"].([]any) {
					point.([]any)[1] = "100"
				}
			}
		}
		alerts := f.run(t)
		alert := requireAlertClass(t, alerts, "provider-score-read-errors-unobservable")
		if len(alerts) != 1 || !isVisibilityAlert(alert) || alert.Sustain != 2 {
			t.Fatalf("%s manufactured health or an error count", mode)
		}
		requireAlertOmits(t, alert, "PRIVATE-CANARY", "api-a", "api-b", "00000001")
	}
}

func TestProviderScoreReadErrorsIndependentPositiveAndNativeCoverage(t *testing.T) {
	for _, mode := range []string{"sibling-missing", "same-process-phase-missing", "same-process-reset", "success-missing", "native-sibling-missing", "old-and-new", "stale-old", "ambiguous-duplicate"} {
		f := newScoreReadFixture()
		f.row("api-a", "counts")["values"].([]any)[59].([]any)[1] = "11"
		switch mode {
		case "sibling-missing":
			f.remove("api-b", "samples")
		case "same-process-phase-missing":
			f.remove("api-a", "samples")
		case "same-process-reset":
			f.row("api-a", "samples")["values"].([]any)[30].([]any)[1] = "0"
		case "success-missing":
			f.remove("api-a", "success")
		case "native-sibling-missing":
			f.census["api-b"] = nil
		case "old-and-new":
			f.addProcess("api-a", "11223344", float64(f.now.Add(-2*time.Hour).Unix()), 2, 0, true)
		case "stale-old":
			f.addProcess("api-a", "11223344", float64(f.now.Add(-2*time.Hour).Unix()), 2, 0, true)
			for _, row := range f.rows {
				if row["metric"].(map[string]string)["instance"] == "11223344" {
					row["values"] = row["values"].([]any)[:50]
				}
			}
		case "ambiguous-duplicate":
			f.addProcess("api-a", "11223344", float64(f.now.Add(-time.Hour).Unix()), 1, 0, true)
		}
		alerts := f.run(t)
		wantPositive := mode != "ambiguous-duplicate"
		wantUnknown := mode != "old-and-new"
		if (requireAlertClassCount(alerts, "provider-score-read-errors") == 1) != wantPositive || (requireAlertClassCount(alerts, "provider-score-read-errors-unobservable") == 1) != wantUnknown {
			t.Fatalf("%s lost independent authority or accepted duplicate generation", mode)
		}
		if mode == "old-and-new" && !strings.Contains(alerts[0].Observed, "counts_failed_pipelines=3") {
			t.Fatal("live overlapping generations were dropped or double counted")
		}
	}
}

func TestProviderScoreReadErrorsClockAndFieldDomains(t *testing.T) {
	now := syntheticSettings(nil).Now()
	for _, bad := range []string{`[["1","0"],[2,"0"]]`, `[[1,0],[2,0]]`, `[[1,"NaN"],[2,"0"]]`, `[[1,"-1"],[2,"0"]]`, `[[1e300,"0"],[2,"0"]]`} {
		var rows [][]json.RawMessage
		if json.Unmarshal([]byte(bad), &rows) != nil {
			t.Fatal("invalid domain fixture")
		}
		if _, ok := providerScoreReadPoints(rows, now, true); ok {
			t.Fatal("invalid source clock/value encoding admitted")
		}
	}
	for _, duration := range []time.Duration{-time.Nanosecond, time.Minute + time.Nanosecond} {
		f := newScoreReadFixture()
		settings := f.settings(t)
		env, err := newProbeEnv(settings.withDefaults())
		if err != nil {
			t.Fatal(err)
		}
		scope := providerScoreReadScope(env)
		census := jwtRejectionsCensus{complete: true, starts: map[jwtRejectionsSlot][]float64{}}
		for host, rows := range f.census {
			for _, row := range rows {
				census.starts[jwtRejectionsSlot{service: "api", host: host, block: "blue"}] = append(census.starts[jwtRejectionsSlot{service: "api", host: host, block: "blue"}], row["start"].(float64))
			}
		}
		raw := scoreReadEncode(t, map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": f.rows}})
		if e := parseProviderScoreReads(raw, "synthetic", now, now.Add(duration), scope, census); e.complete || e.counts != 0 {
			t.Fatal("owner clock rollback/overrun admitted source")
		}
	}
	if math.Trunc(providerScoreReadExactInteger) != providerScoreReadExactInteger {
		t.Fatal("counter exactness ceiling changed")
	}
}

func TestProviderScoreReadErrorsWindowBoundaries(t *testing.T) {
	for _, control := range []struct {
		firstCut, lastCut, gapSkip int
		completion                 time.Duration
		processAge                 time.Duration
		known                      bool
	}{
		{firstCut: 1, lastCut: 2, gapSkip: 2, known: true},
		{firstCut: 2}, {lastCut: 3}, {gapSkip: 3},
		{completion: time.Minute, known: true}, {completion: time.Minute + time.Nanosecond},
		{processAge: 15 * time.Minute, known: true}, {processAge: 15*time.Minute - time.Second},
	} {
		f := newScoreReadFixture()
		for _, row := range f.rows {
			values := row["values"].([]any)
			if control.gapSkip > 0 {
				values = append(append([]any{}, values[:20]...), values[20+control.gapSkip:]...)
			}
			values = values[control.firstCut : len(values)-control.lastCut]
			row["values"] = values
			if control.processAge > 0 && row["metric"].(map[string]string)["__name__"] == jwtRejectionsStart {
				for _, point := range values {
					point.([]any)[1] = fmt.Sprint(f.now.Add(-control.processAge).Unix())
				}
			}
		}
		if control.processAge > 0 {
			for _, rows := range f.census {
				rows[0]["start"] = float64(f.now.Add(-control.processAge).Unix())
			}
		}
		settings := f.settings(t)
		clocks := 0
		settings.Now = func() time.Time {
			clocks++
			if clocks == 1 {
				return f.now
			}
			return f.now.Add(control.completion)
		}
		alerts, err := NewProviderScoreReadErrorsSignal().Run(context.Background(), settings)
		if err != nil || (len(alerts) == 0) != control.known {
			t.Fatal("raw endpoint/gap/process-age/owner boundary changed authority")
		}
	}
}

func TestProviderScoreReadErrorsGenerationCancellationAndCause(t *testing.T) {
	for _, mode := range []string{"pre-generation", "post-native-generation", "post-query-generation", "query-error", "query-cancel", "native-cancel", "parent-ended", "final-cancel"} {
		f := newScoreReadFixture()
		ctx, cancel := context.WithCancel(context.Background())
		private := errors.New("PRIVATE-SOURCE-CAUSE")
		if mode == "parent-ended" {
			cancel()
		}
		if mode == "query-error" || mode == "query-cancel" {
			f.queryErr = private
		}
		if mode == "query-cancel" {
			f.beforeQuery = cancel
		}
		if mode == "native-cancel" {
			f.beforeCensus = cancel
			f.censusErr = private
		}
		settings := f.settings(t)
		checks, clocks := 0, 0
		settings.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) {
			checks++
			if mode == "pre-generation" || mode == "post-native-generation" && checks == 2 || mode == "post-query-generation" && checks >= 4 {
				return false, nil
			}
			return true, nil
		}
		settings.Now = func() time.Time {
			clocks++
			if mode == "final-cancel" && clocks == 2 {
				cancel()
			}
			return f.now
		}
		alerts, err := NewProviderScoreReadErrorsSignal().Run(ctx, settings)
		ended := strings.Contains(mode, "cancel") || mode == "parent-ended"
		if ended {
			if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
				t.Fatalf("%s returned findings beyond the owner", mode)
			}
			if (mode == "query-cancel" || mode == "native-cancel") && !errors.Is(err, private) {
				t.Fatal("original source cause was discarded")
			}
			if strings.Contains(err.Error(), private.Error()) {
				t.Fatal("private cause rendered")
			}
		} else {
			if err != nil {
				t.Fatal("live source became top-level failure")
			}
			requireAlertClass(t, alerts, "provider-score-read-errors-unobservable")
		}
		if (mode == "pre-generation" || mode == "parent-ended") && f.hosts.Load() != 0 {
			t.Fatal("native read followed refused owner/generation")
		}
		if (mode == "pre-generation" || mode == "post-native-generation" || mode == "parent-ended" || mode == "native-cancel") && f.queries.Load() != 0 {
			t.Fatal("query followed refused dependency")
		}
		cancel()
	}
}

func TestProviderScoreReadErrorsInventoryAndExclusions(t *testing.T) {
	for _, mode := range []string{"absent-service", "absent-placement", "excluded-host", "disabled-host", "unenrolled-host", "over-bound"} {
		f := newScoreReadFixture()
		settings := f.settings(t)
		switch mode {
		case "absent-service":
			settings.LogServices = nil
			settings.LogServiceHosts = nil
			settings.LogServiceBlocks = nil
		case "absent-placement":
			settings.LogServiceHosts = nil
		case "excluded-host":
			var err error
			settings, err = ExcludeHosts(settings, "api-b")
			if err != nil {
				t.Fatal("synthetic host exclusion was refused")
			}
		case "disabled-host":
			// Mirror the loader: disabled hosts have a separate inventory;
			// HostSettings has no Disabled field. Desired placement stays intact.
			settings.disabledHosts = []HostSettings{settings.Hosts[1]}
			settings.Hosts = settings.Hosts[:1]
		case "unenrolled-host":
			settings.Hosts = settings.Hosts[:1]
		case "over-bound":
			settings.LogServiceHosts["api"] = nil
			settings.Hosts = nil
			for i := range 17 {
				name := fmt.Sprintf("api-%02d", i)
				settings.LogServiceHosts["api"] = append(settings.LogServiceHosts["api"], name)
				settings.Hosts = append(settings.Hosts, HostSettings{Name: name, Roles: []string{"api"}})
			}
		}
		observedSibling := mode == "excluded-host" || mode == "disabled-host" || mode == "unenrolled-host"
		if observedSibling {
			// All three cases return the same complete api-a histories. Unknown
			// must come from required api-b coverage, not foreign response rows.
			f.rows = f.rows[:4]
			if placement := settings.LogServiceHosts["api"]; len(placement) != 2 || placement[0] != "api-a" || placement[1] != "api-b" {
				t.Fatal("observation policy erased desired API placement")
			}
		}
		source := settings.Source.(*providerHttpsTestSource)
		readCensus := source.syntheticSource.hostTimeoutFn
		source.syntheticSource.hostTimeoutFn = func(host HostSettings, command string, timeout time.Duration) (string, error) {
			if host.Name != "api-a" {
				t.Error("native read reached a disabled, excluded or unenrolled host")
			}
			return readCensus(host, command, timeout)
		}
		// Only the admitted sibling belongs in the raw query selector.
		source.do = func(request *http.Request) (*http.Response, error) {
			f.queries.Add(1)
			if request.ParseForm() != nil || !strings.Contains(request.PostForm.Get("query"), `host=~"api-a"`) {
				t.Error("scope read did not exclude the unavailable host")
			}
			return providerHttpsTestResponse(scoreReadEncode(t, map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": f.rows}})), nil
		}
		alerts, err := NewProviderScoreReadErrorsSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal("inventory control failed validation")
		}
		if mode == "disabled-host" {
			if len(alerts) != 0 {
				t.Fatal("explicit disabled inventory remained required")
			}
		} else {
			warning := requireAlertClass(t, alerts, "provider-score-read-errors-unobservable")
			if !isVisibilityAlert(warning) || requireAlertClassCount(alerts, "provider-score-read-errors") != 0 {
				t.Fatal("missing inventory became complete health or a product finding")
			}
		}
		if observedSibling {
			if f.hosts.Load() != 1 || f.queries.Load() != 1 {
				t.Fatal("inventory observation did not read exactly its permitted sibling")
			}
		} else if f.hosts.Load() != 0 || f.queries.Load() != 0 {
			t.Fatal("invalid census scope started source work")
		}
	}
}

// Unknown reader evidence restarts recovery without dropping the retained incident.
func TestProviderScoreReadErrorsNotifierRetainsUnknown(t *testing.T) {
	base := syntheticSettings(nil).Now()
	f := newScoreReadFixture()
	settings := f.settings(t)
	signal := NewProviderScoreReadErrorsSignal()
	webhook, url := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(url))
	t.Cleanup(func() { closeSlackNotifier(t, notifier) })
	if notifier.settings.ClearAfterCompleteRuns != 2 {
		t.Fatal("recovery default changed")
	}
	assertIncident := func(present bool, credit int) {
		t.Helper()
		notifier.stateLock.Lock()
		defer notifier.stateLock.Unlock()
		incident, found := notifier.keyIncidents[signal.ID()+"|provider-score-read-errors|api-fleet"]
		if found != present || found && incident.signalKeyAbsentRuns[signal.Key()] != credit {
			t.Fatal("incomplete reader source cleared or advanced incident")
		}
	}
	postSignalRunsThroughTheGate(t, signal, settings, 8, func(i int) {
		// Before each run, unknown observations reset this owner's credit.
		// Healthy runs 6 and 7 supply the uninterrupted recovery after the gap.
		switch i {
		case 0, 1:
			assertIncident(false, 0)
		case 2, 3, 5, 6:
			assertIncident(true, 0)
		case 4, 7:
			assertIncident(true, 1)
		}
		f.now = base.Add(time.Duration(i) * 15 * time.Minute)
		f.rows = nil
		f.census = map[string][]map[string]any{}
		for j, host := range []string{"api-a", "api-b"} {
			count := float64(0)
			if i < 2 {
				count = 1
			}
			f.addProcess(host, fmt.Sprintf("%08x", j+1), float64(base.Add(-time.Hour).Unix())-float64(j), count, 0, true)
		}
		if i == 2 || i == 4 || i == 5 {
			f.remove("api-b", "samples")
		}
		if i > 0 {
			harness.advance(15 * time.Minute)
		}
	}, notifier)
	closeSlackNotifier(t, notifier)
	assertIncident(false, 0)
	identity := "\n" + signal.ID() + " · provider-score-read-errors · api-fleet\n"
	if countSlackPosts(webhook.posted(), "[WARN] raised", identity) != 1 || countSlackPosts(webhook.posted(), "[CLEARED] WARN", identity) != 1 {
		t.Fatal("reader incident duplicated or lost two-complete recovery")
	}
}
