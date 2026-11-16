// The capture the flag row of the taskworker dashboard: the presence samples,
// the census and the driver stay scoped to the taskworker, the sample and
// census gauges read the taskworker whose sample or census is newest, and every
// capture the flag metric is charted.
package grafana

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The capture the flag row's panel ids on the taskworker dashboard.
const (
	ctfRowPanel            = 106
	ctfSamplesPanel        = 107
	ctfPresentPanel        = 108
	ctfSampleAgesPanel     = 109
	ctfCensusRunsPanel     = 110
	ctfCensusCountsPanel   = 111
	ctfCensusExcludedPanel = 112
	ctfCensusSamplesPanel  = 113
	ctfCensusLagPanel      = 114
	ctfEpochsPanel         = 115
	ctfStepsPanel          = 116
	ctfAlertsPanel         = 117
	ctfDrandPanel          = 118
	ctfBuildPanel          = 119
	ctfWinnersPanel        = 120
)

// The label matchers every capture the flag selector starts with.
const ctfTaskworkerFilter = `env="$env",service="taskworker",block=~"$block",host=~"$host",instance!=""`

// Each panel charts its metrics as pinned here, under the taskworker filters,
// and its description explains every label value the metrics carry. The
// counters the code labels in advance read a plain increase; the driver's
// counters, whose series start at their first count, read the first-count
// form (counter_first_count_dashboard_test.go). The row sits after the SN25
// extender pool row, and every capture the flag metric the server registers
// is on it.
func TestCtfDashboardPanels(t *testing.T) {
	dashboard := readTestDashboard(t, "taskworker.json")
	if slices.Contains(dashboard.Tags, PublicTag) {
		t.Fatal("capture the flag diagnostics must remain authenticated")
	}
	row := dashboardPanelById(dashboard, ctfRowPanel)
	if row == nil || row.Type != "row" || row.Title != "SN25 capture the flag: presence samples, census and driver (controller/ctf_metrics.go, controller/st_ctf_metrics.go)" {
		t.Fatalf("taskworker dashboard lacks the capture the flag row: %+v", row)
	}
	extenderPoolRow, nextRow := dashboardPanelById(dashboard, 98), dashboardPanelById(dashboard, 103)
	if extenderPoolRow == nil || nextRow == nil || row.GridPos.Y <= extenderPoolRow.GridPos.Y || nextRow.GridPos.Y <= row.GridPos.Y {
		t.Fatal("the capture the flag row must sit between the SN25 extender pool row and the operator block points row")
	}

	selector := func(metric string) string {
		return metric + "{" + ctfTaskworkerFilter + "}"
	}
	const on = " and on (env, service, block, host, instance) "
	newestSample := "topk(1, " + selector("urnetwork_sn_ctf_last_sample_timestamp_seconds") + " > time() - 1800)"
	newestCensus := "topk(1, " + selector("urnetwork_sn_ctf_last_census_epoch") + " > 0)"
	firstCount := func(metric string, labels string) string {
		return "sum by (" + labels + ") ((increase(" + selector(metric) + "[1h]) and " + selector(metric) + " offset 1h) or " + selector(metric) + ")"
	}
	roles := []string{"provider", "extender"}
	cases := []struct {
		id        int
		title     string
		unit      string
		targets   []testTarget
		explained []string
	}{
		{id: ctfSamplesPanel, title: "CTF presence samples per hour by result", unit: "short", targets: []testTarget{
			{Expr: "sum by (result) (increase(" + selector("urnetwork_sn_ctf_presence_samples_total") + "[1h]))", LegendFormat: "{{result}}"},
		}, explained: []string{"taken", "no_providers", "late", "outage"}},
		{id: ctfPresentPanel, title: "CTF miners present in the newest sample by role", unit: "short", targets: []testTarget{
			{Expr: "max by (role) (" + selector("urnetwork_sn_ctf_sample_present") + on + newestSample + ")", LegendFormat: "{{role}}"},
		}, explained: append([]string{"newest", "30 minutes"}, roles...)},
		{id: ctfSampleAgesPanel, title: "CTF presence sample ages", unit: "s", targets: []testTarget{
			{Expr: "time() - max(" + selector("urnetwork_sn_ctf_last_sample_timestamp_seconds") + " > 0)", LegendFormat: "since the newest sample"},
			{Expr: "max((" + selector("urnetwork_sn_ctf_sample_source_age_seconds") + " > 0)" + on + newestSample + ")", LegendFormat: "online set age at the newest sample"},
		}, explained: []string{"20 minutes", "does not set the online set age", "reads nothing if it has read none", "30 minutes"}},
		{id: ctfCensusRunsPanel, title: "CTF census runs in the trailing 7 days by result", unit: "short", targets: []testTarget{
			{Expr: "sum by (result) (increase(" + selector("urnetwork_sn_ctf_census_runs_total") + "[7d]))", LegendFormat: "{{result}}"},
		}, explained: []string{"built", "published", "retry", "given_up", "50 minutes"}},
		{id: ctfCensusCountsPanel, title: "CTF newest census: members, candidates and units by role", unit: "short", targets: []testTarget{
			{Expr: "max by (role) (" + selector("urnetwork_sn_ctf_census_members") + on + newestCensus + ")", LegendFormat: "members {{role}}"},
			{Expr: "max by (role) (" + selector("urnetwork_sn_ctf_census_candidates") + on + newestCensus + ")", LegendFormat: "candidates {{role}}"},
			{Expr: "max by (role) (" + selector("urnetwork_sn_ctf_census_units") + on + newestCensus + ")", LegendFormat: "units {{role}}"},
		}, explained: []string{"members", "candidates", "units", "newest", "restart"}},
		{id: ctfCensusExcludedPanel, title: "CTF newest census: members left out by role and first failed rule", unit: "short", targets: []testTarget{
			{Expr: "max by (role, reason) (" + selector("urnetwork_sn_ctf_census_excluded") + on + newestCensus + ")", LegendFormat: "{{role}} {{reason}}"},
		}, explained: []string{"inactive", "offline_at_end", "not_public", "no_location", "own_network", "head_bound", "no_wallet", "duplicate_address", "country_changed", "presence"}},
		{id: ctfCensusSamplesPanel, title: "CTF newest census: samples taken and excused", unit: "short", targets: []testTarget{
			{Expr: "max(" + selector("urnetwork_sn_ctf_census_samples_taken") + on + newestCensus + ")", LegendFormat: "taken, of 672"},
			{Expr: "max(" + selector("urnetwork_sn_ctf_census_samples_excused") + on + newestCensus + ")", LegendFormat: "excused"},
		}, explained: []string{"672", "outage", "ctf_census_samples"}},
		{id: ctfCensusLagPanel, title: "CTF newest census: capture lag", unit: "s", targets: []testTarget{
			{Expr: "max(" + selector("urnetwork_sn_ctf_census_capture_lag_seconds") + on + newestCensus + ")", LegendFormat: "capture lag"},
		}, explained: []string{"50 minutes", "given_up"}},
		{id: ctfEpochsPanel, title: "CTF epochs: census built and driver phases", unit: "none", targets: []testTarget{
			{Expr: "max(" + selector("urnetwork_sn_ctf_last_census_epoch") + " > 0)", LegendFormat: "census built"},
			{Expr: "max by (phase) (" + selector("urnetwork_st_ctf_last_epoch") + ")", LegendFormat: "driver {{phase}}"},
		}, explained: []string{"census", "commit", "recycle", "shadow"}},
		{id: ctfStepsPanel, title: "CTF driver steps per hour by step and status", unit: "short", targets: []testTarget{
			{Expr: firstCount("urnetwork_st_ctf_steps_total", "step, status"), LegendFormat: "{{step}} {{status}}"},
		}, explained: []string{"ctf_commit_census", "ctf_epoch", "ctf_finalize", "ctf_answer", "ctf_record", "ctf_recycle", "ctf_shadow", "confirmed", "skipped", "failed", "missed", "error", "first count"}},
		{id: ctfAlertsPanel, title: "CTF driver alerts per hour by alert", unit: "short", targets: []testTarget{
			{Expr: firstCount("urnetwork_st_ctf_alerts_total", "alert"), LegendFormat: "{{alert}}"},
		}, explained: []string{"once per subject", "ctf_census_build", "ctf_census_samples", "ctf_presence_gap", "first raise"}},
		{id: ctfDrandPanel, title: "CTF drand relay answers per hour by result", unit: "short", targets: []testTarget{
			{Expr: firstCount("urnetwork_st_ctf_drand_fetches_total", "result"), LegendFormat: "{{result}}"},
		}, explained: []string{"verified", "refused", "unavailable", "ctf_drand", "first count"}},
		{id: ctfBuildPanel, title: "CTF last build by taskworker: α by part", unit: "short", targets: []testTarget{
			{Expr: "max by (host, block, instance, part) (" + selector("urnetwork_st_ctf_last_build_rao") + ") / 1e9", LegendFormat: "{{part}} {{host}} {{block}} {{instance}}"},
		}, explained: []string{"rao / 10^9", "fresh", "paid", "pending", "carried", "before the commit"}},
		{id: ctfWinnersPanel, title: "CTF last build by taskworker: winners", unit: "short", targets: []testTarget{
			{Expr: "max by (host, block, instance) (" + selector("urnetwork_st_ctf_last_build_winners") + on + "urnetwork_st_ctf_last_build_rao{" + ctfTaskworkerFilter + `,part="fresh"})`, LegendFormat: "winners {{host}} {{block}} {{instance}}"},
		}, explained: []string{"initial 0"}},
	}
	var queries strings.Builder
	for _, c := range cases {
		panel := dashboardPanelById(dashboard, c.id)
		if panel == nil {
			t.Errorf("capture the flag panel %d is missing", c.id)
			continue
		}
		if panel.Type != "timeseries" || panel.Title != c.title || panel.FieldConfig.Defaults.Unit != c.unit {
			t.Errorf("capture the flag panel %d is the %s %q in %q, want the %s time series %q", c.id, panel.Type, panel.Title, panel.FieldConfig.Defaults.Unit, c.unit, c.title)
		}
		if panel.GridPos.Y <= row.GridPos.Y || nextRow.GridPos.Y <= panel.GridPos.Y {
			t.Errorf("panel %q sits outside its row", panel.Title)
		}
		if len(panel.Targets) != len(c.targets) {
			t.Errorf("panel %q has %d queries, want %d", panel.Title, len(panel.Targets), len(c.targets))
			continue
		}
		for index, target := range panel.Targets {
			want := c.targets[index]
			if target.Expr != want.Expr || target.LegendFormat != want.LegendFormat || target.Instant {
				t.Errorf("panel %q query %d = %s as %q, want the range query %s as %q", panel.Title, index, target.Expr, target.LegendFormat, want.Expr, want.LegendFormat)
			}
			queries.WriteString(target.Expr)
			queries.WriteByte('\n')
		}
		for _, explained := range c.explained {
			if !strings.Contains(panel.Description, explained) {
				t.Errorf("panel %q does not explain %q", panel.Title, explained)
			}
		}
	}

	ctfMetrics := []string{}
	for _, metric := range registeredApplicationMetrics(t) {
		if strings.HasPrefix(metric, "urnetwork_sn_ctf_") || strings.HasPrefix(metric, "urnetwork_st_ctf_") {
			ctfMetrics = append(ctfMetrics, metric)
		}
	}
	if len(ctfMetrics) == 0 {
		t.Fatal("the metric inventory has no capture the flag metric")
	}
	for _, metric := range ctfMetrics {
		if !strings.Contains(queries.String(), metric) {
			t.Errorf("the capture the flag row does not chart %s", metric)
		}
	}
}

// Each taskworker keeps the gauges of the last sample and the last census it
// made, and its unlabeled gauges start at 0. So the sample panels read the
// taskworker whose sample is newest, while that sample is under 30 minutes old,
// and the census panels the taskworker whose census epoch is newest, even when
// an older one reports higher values; a newest sample whose taskworker has read
// no fresh online set reads no online set age, never an older one's; a
// taskworker that has sampled or built nothing never reads; the driver epochs
// are the latest any taskworker reached; the build reads per taskworker, and
// winners only where a build set its amounts; and other services are ignored.
func TestCtfDashboardPromqlReadsTheNewestSampleAndCensus(t *testing.T) {
	promtool := os.Getenv("URNETWORK_PROMTOOL")
	if promtool == "" {
		var err error
		promtool, err = exec.LookPath("promtool")
		if err != nil {
			t.Fatal("official promtool 3.5.0 is required; run bash ./grafana/promtool.sh from the server root and export its path as URNETWORK_PROMTOOL")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	version, err := exec.CommandContext(ctx, promtool, "--version").CombinedOutput()
	if err != nil || !strings.HasPrefix(string(version), "promtool, version 3.5.0 ") {
		t.Fatal("official promtool 3.5.0 is required")
	}

	dashboard := readTestDashboard(t, "taskworker.json")
	variables := strings.NewReplacer(`"$env"`, `"synthetic"`, `"$block"`, `".*"`, `"$host"`, `".*"`)
	expression := func(id int, refId string) string {
		panel := dashboardPanelById(dashboard, id)
		if panel == nil {
			t.Fatalf("capture the flag panel %d is missing", id)
		}
		for _, target := range panel.Targets {
			if target.RefId == refId {
				return variables.Replace(target.Expr)
			}
		}
		t.Fatalf("capture the flag panel %d has no target %s", id, refId)
		return ""
	}

	// One process's capture the flag gauges as its last sample, census and
	// build left them. A process that never sampled or built keeps the
	// gauges' initial 0; its driver epochs and build amounts have no series.
	type worker struct {
		service  string
		instance string
		// the last sample's clock and what it found
		sample, providers, extenders, sourceAge float64
		// the last census's epoch and what it counted
		census, members, candidates, units, presenceExcluded, taken, excused, lag float64
		// the driver's epochs by phase, and the last build's fresh rao
		phaseEpochs map[string]float64
		fresh       float64
		winners     float64
	}
	inputs := func(workers ...worker) []map[string]string {
		series := []map[string]string{}
		for _, w := range workers {
			gauge := func(name string, labels string, value float64) {
				series = append(series, map[string]string{
					"series": fmt.Sprintf(`%s{env="synthetic",service=%q,block="b1",host="synthetic-host",instance=%q%s}`, name, w.service, w.instance, labels),
					"values": strconv.FormatFloat(value, 'f', -1, 64) + "x60",
				})
			}
			gauge("urnetwork_sn_ctf_last_sample_timestamp_seconds", "", w.sample)
			gauge("urnetwork_sn_ctf_sample_present", `,role="provider"`, w.providers)
			gauge("urnetwork_sn_ctf_sample_present", `,role="extender"`, w.extenders)
			gauge("urnetwork_sn_ctf_sample_source_age_seconds", "", w.sourceAge)
			gauge("urnetwork_sn_ctf_last_census_epoch", "", w.census)
			gauge("urnetwork_sn_ctf_census_members", `,role="provider"`, w.members)
			gauge("urnetwork_sn_ctf_census_candidates", `,role="provider"`, w.candidates)
			gauge("urnetwork_sn_ctf_census_units", `,role="provider"`, w.units)
			gauge("urnetwork_sn_ctf_census_excluded", `,role="provider",reason="presence"`, w.presenceExcluded)
			gauge("urnetwork_sn_ctf_census_samples_taken", "", w.taken)
			gauge("urnetwork_sn_ctf_census_samples_excused", "", w.excused)
			gauge("urnetwork_sn_ctf_census_capture_lag_seconds", "", w.lag)
			gauge("urnetwork_st_ctf_last_build_winners", "", w.winners)
			for _, phase := range []string{"census", "commit"} {
				if epoch, ok := w.phaseEpochs[phase]; ok {
					gauge("urnetwork_st_ctf_last_epoch", fmt.Sprintf(`,phase=%q`, phase), epoch)
				}
			}
			if 0 < w.fresh {
				gauge("urnetwork_st_ctf_last_build_rao", `,part="fresh"`, w.fresh)
			}
		}
		return series
	}
	// evaluated at 3000 s: a sample is current when it was taken after 1200 s
	sampler := worker{service: "taskworker", instance: "sampler",
		sample: 2400, providers: 7, extenders: 3, sourceAge: 60,
		census: 40, members: 100, candidates: 80, units: 50, presenceExcluded: 9, taken: 600, excused: 4, lag: 120,
		phaseEpochs: map[string]float64{"commit": 41}}
	censor := worker{service: "taskworker", instance: "censor",
		sample: 1500, providers: 10, extenders: 5, sourceAge: 900,
		census: 41, members: 90, candidates: 70, units: 40, presenceExcluded: 12, taken: 672, excused: 0, lag: 300,
		phaseEpochs: map[string]float64{"census": 41, "commit": 40}, fresh: 2500000000, winners: 12}
	idle := worker{service: "taskworker", instance: "idle"}
	api := worker{service: "api", instance: "api",
		sample: 2900, providers: 999, extenders: 999, sourceAge: 999,
		census: 99, members: 999, candidates: 999, units: 999, presenceExcluded: 999, taken: 999, excused: 999, lag: 999,
		phaseEpochs: map[string]float64{"census": 99, "commit": 99}, fresh: 9000000000, winners: 999}
	staleSampler := sampler
	staleSampler.sample = 1100
	staleCensor := censor
	staleCensor.sample = 1000
	// a no_providers sample on a taskworker that has not read a fresh online
	// set since it started: no providers, and its online set age is still 0
	unfreshSampler := sampler
	unfreshSampler.sample = 2700
	unfreshSampler.providers = 0
	unfreshSampler.sourceAge = 0

	value := func(v float64) []map[string]any {
		return []map[string]any{{"labels": "{}", "value": v}}
	}
	labeled := func(labels string, v float64) map[string]any {
		return map[string]any{"labels": labels, "value": v}
	}
	none := []map[string]any{}
	type evaluation struct {
		id      int
		refId   string
		samples []map[string]any
	}
	cases := []struct {
		name        string
		workers     []worker
		evaluations []evaluation
	}{
		{name: "the newest sample and the newest census win over higher older values and other services", workers: []worker{sampler, censor, idle, api}, evaluations: []evaluation{
			{ctfPresentPanel, "A", []map[string]any{labeled(`{role="provider"}`, 7), labeled(`{role="extender"}`, 3)}},
			{ctfSampleAgesPanel, "A", value(600)},
			{ctfSampleAgesPanel, "B", value(60)},
			{ctfCensusCountsPanel, "A", []map[string]any{labeled(`{role="provider"}`, 90)}},
			{ctfCensusCountsPanel, "B", []map[string]any{labeled(`{role="provider"}`, 70)}},
			{ctfCensusCountsPanel, "C", []map[string]any{labeled(`{role="provider"}`, 40)}},
			{ctfCensusExcludedPanel, "A", []map[string]any{labeled(`{role="provider",reason="presence"}`, 12)}},
			{ctfCensusSamplesPanel, "A", value(672)},
			{ctfCensusSamplesPanel, "B", value(0)},
			{ctfCensusLagPanel, "A", value(300)},
			{ctfEpochsPanel, "A", value(41)},
			{ctfEpochsPanel, "B", []map[string]any{labeled(`{phase="census"}`, 41), labeled(`{phase="commit"}`, 41)}},
			{ctfBuildPanel, "A", []map[string]any{labeled(`{block="b1",host="synthetic-host",instance="censor",part="fresh"}`, 2.5)}},
			{ctfWinnersPanel, "A", []map[string]any{labeled(`{block="b1",host="synthetic-host",instance="censor"}`, 12)}},
		}},
		{name: "a newest sample without a fresh online set reads no age, not an older taskworker's", workers: []worker{unfreshSampler, censor}, evaluations: []evaluation{
			{ctfPresentPanel, "A", []map[string]any{labeled(`{role="provider"}`, 0), labeled(`{role="extender"}`, 3)}},
			{ctfSampleAgesPanel, "A", value(300)},
			{ctfSampleAgesPanel, "B", none},
		}},
		{name: "a sample 30 minutes old reads nothing but its age", workers: []worker{staleSampler, staleCensor}, evaluations: []evaluation{
			{ctfPresentPanel, "A", none},
			{ctfSampleAgesPanel, "A", value(1900)},
			{ctfSampleAgesPanel, "B", none},
			{ctfCensusLagPanel, "A", value(300)},
		}},
		{name: "a taskworker that has sampled and built nothing reads nothing", workers: []worker{idle, api}, evaluations: []evaluation{
			{ctfPresentPanel, "A", none},
			{ctfSampleAgesPanel, "A", none},
			{ctfSampleAgesPanel, "B", none},
			{ctfCensusCountsPanel, "A", none},
			{ctfCensusExcludedPanel, "A", none},
			{ctfCensusSamplesPanel, "A", none},
			{ctfCensusLagPanel, "A", none},
			{ctfEpochsPanel, "A", none},
			{ctfEpochsPanel, "B", none},
			{ctfBuildPanel, "A", none},
			{ctfWinnersPanel, "A", none},
		}},
	}
	tests := []map[string]any{}
	for _, c := range cases {
		expressionTests := []map[string]any{}
		for _, e := range c.evaluations {
			expressionTests = append(expressionTests, map[string]any{"expr": expression(e.id, e.refId), "eval_time": "50m", "exp_samples": e.samples})
		}
		tests = append(tests, map[string]any{"name": c.name, "interval": "1m", "input_series": inputs(c.workers...), "promql_expr_test": expressionTests})
	}
	// Json is also valid yaml; its quoting preserves the exact embedded query.
	fixture, err := json.Marshal(map[string]any{"evaluation_interval": "1m", "tests": tests})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ctf.yml")
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(ctx, promtool, "test", "rules", path).CombinedOutput(); err != nil {
		t.Fatalf("capture the flag expression evaluation: %v\n%s", err, output)
	}
}
