package grafana

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The owner's session decisions of 2026-11-18 (session/REVOKE-FINAL.md §15)
// chart in their own signals row:
//   - credentials accepted unchecked because Redis could not be reached
//     (decision 3), beside the Redis pool timeouts and the Redis pool dial
//     failures the services log, and as the share of record checks the
//     alerts in warp grafana/alerting/session-auth.yml evaluate;
//   - mints that fell back to untagged credentials, and mints of an existing
//     session signed on it unextended (decision 9);
//   - new sessions the limit would refuse while all/pro.yml keeps it
//     unenforced, and those it refuses once enforced, by kind and cap;
//   - client sessions freed by removal, by path;
//   - the sampled live sessions per network.
//
// Each panel names the decision it watches, and promtool evaluates the charted
// expressions over synthetic series.
func TestSessionRolloutPanelsChartFailOpenLimitAndFrees(t *testing.T) {
	dashboard := readTestDashboard(t, "signals.json")
	panel := func(title string) *testPanel {
		t.Helper()
		for index := range dashboard.Panels {
			if dashboard.Panels[index].Title == title {
				return &dashboard.Panels[index]
			}
		}
		t.Fatalf("signals dashboard lacks the %q panel", title)
		return nil
	}
	row := panel("sessions: unchecked acceptances, untagged mints, the session limit and frees (session/REVOKE-FINAL.md §15)")
	if row.Type != "row" {
		t.Errorf("the session rollout row is a %s", row.Type)
	}

	type chart struct {
		title   string
		unit    string
		targets [][2]string
		words   []string
	}
	const unchecked = `urnetwork_session_check_unavailable_total{env="$env"}`
	const liveBuckets = `urnetwork_session_live_per_network_bucket{env="$env"}[1h]`
	const liveCount = `urnetwork_session_live_per_network_count{env="$env"}[1h]`
	const passes = `urnetwork_session_live_sample_passes_total{env="$env"}[1h]`
	charts := []chart{
		{"credentials accepted unchecked / s by service, path and credential", "ops", [][2]string{
			{`sum by (service, path, credential) (rate(` + unchecked + `[$__rate_interval]))`, "unchecked {{service}} {{path}} {{credential}}"},
			{`sum by (service) (rate(urnetwork_redis_pool_timeouts_total{env="$env"}[$__rate_interval]))`, "redis pool timeouts {{service}}"},
		}, []string{"decision 3", "REVOKE-FINAL.md §15", "never answers 503", "revoked during the failure", "cancelled", "Redis pool timeouts", "dial failures"}},
		{"unchecked share of tagged credential checks by service", "percentunit", [][2]string{
			{`sum by (service) (rate(` + unchecked + `[5m])) / (sum by (service) (rate(urnetwork_jwt_state_record_checks_total{env="$env"}[5m])) or sum by (service) (rate(` + unchecked + `[5m])))`, "{{service}}"},
		}, []string{"session-auth.yml", "SessionChecksUnchecked", "5% held for 10 minutes", "at least 1", "SessionChecksUncheckedSevere", "25% held for 5 minutes", "at least 10", "record_unavailable", "reads 100%"}},
		{"Redis pool dial failures / min by service", "short", [][2]string{
			{`sum by (service) (count_over_time({env="$env"} |= "connection pool: failed to dial" [1m]))`, "{{service}}"},
		}, []string{"failed to dial after N attempts", "No counter", "i/o timeout", "connection refused", "redis cluster"}},
		{"mints issued untagged or unextended / s by service and kind (Redis unreachable at mint)", "ops", [][2]string{
			{`sum by (service, kind) (rate(urnetwork_session_mint_untagged_total{env="$env"}[$__rate_interval]))`, "untagged {{service}} {{kind}}"},
			{`sum by (service, kind) (rate(urnetwork_session_mint_unextended_total{env="$env"}[$__rate_interval]))`, "unextended {{service}} {{kind}}"},
		}, []string{"decisions 3 and 9", "untagged credential", "next refresh", "SessionMintsUntagged", "1%", "operation mint", "without extending it", "no later than the credential it was minted from", "hosted proxy's child"}},
		{"session limit: would refuse (unenforced) and refused / s by kind and cap", "ops", [][2]string{
			{`sum by (kind, cap) (rate(urnetwork_session_cap_would_refuse_total{env="$env"}[$__rate_interval]))`, "would refuse {{kind}} {{cap}}"},
			{`sum by (kind, cap) (rate(urnetwork_session_cap_refused_total{env="$env"}[$__rate_interval]))`, "refused {{kind}} {{cap}}"},
		}, []string{"signin, code, client", "5,000", "all/pro.yml", "enforce_session_cap", "409 session_limit_reached", "Account → Sessions", "nothing is evicted", "concurrent client limit exempts", "declared provide intent", "legacy token's first renewal", "decisions 6 and 7"}},
		{"would-refuse new sessions in the last 24 hours by kind and cap", "short", [][2]string{
			{`sum by (kind, cap) (increase(urnetwork_session_cap_would_refuse_total{env="$env"}[1d]))`, "{{kind}} {{cap}}"},
		}, []string{"24-hour", "enforce_session_cap", "plan step 4"}},
		{"client sessions freed on removal / s by path", "ops", [][2]string{
			{`sum by (service, path) (rate(urnetwork_session_freed_on_removal_total{env="$env"}[$__rate_interval]))`, "{{service}} {{path}}"},
		}, []string{"decision 6", "30-day idle reap", "no retained marker", "lifecycle lock", "never frees a sign-in or auth-code session"}},
		{"networks above each live-session level (per sample pass)", "short", [][2]string{
			{`sum(increase(` + liveCount + `)) / sum(increase(` + passes + `))`, "networks with a live session"},
			{`(scalar(sum(increase(` + liveCount + `))) - sum by (le) (increase(urnetwork_session_live_per_network_bucket{env="$env",le=~"10|100|500|1000|2000|5000|10000"}[1h]))) / scalar(sum(increase(` + passes + `)))`, "above {{le}}"},
			{`sum by (outcome) (increase(` + passes + `))`, "sample passes in the last hour: {{outcome}}"},
		}, []string{"observes every network that has a live session set once", "no network labels", "divided by its passes", "does not depend on the period", "partial", "enforce_session_cap", "Logarithmic"}},
		{"live sessions per network: p50, p99, p99.9", "short", [][2]string{
			{`histogram_quantile(0.5, sum by (le) (rate(` + liveBuckets + `)))`, "p50"},
			{`histogram_quantile(0.99, sum by (le) (rate(` + liveBuckets + `)))`, "p99"},
			{`histogram_quantile(0.999, sum by (le) (rate(` + liveBuckets + `)))`, "p99.9"},
		}, []string{"estimated from the histogram's buckets", "5,000 live-session limit"}},
	}
	panels := map[string]*testPanel{}
	for _, c := range charts {
		p := panel(c.title)
		panels[c.title] = p
		if p.Type != "timeseries" || p.FieldConfig.Defaults.Unit != c.unit {
			t.Errorf("%s is a %s in %q, want a timeseries in %q", c.title, p.Type, p.FieldConfig.Defaults.Unit, c.unit)
		}
		if len(p.Targets) != len(c.targets) {
			t.Errorf("%s has %d targets, want %d", c.title, len(p.Targets), len(c.targets))
			continue
		}
		for index, want := range c.targets {
			if p.Targets[index].Expr != want[0] || p.Targets[index].LegendFormat != want[1] {
				t.Errorf("%s target %d = %q %q, want %q %q", c.title, index, p.Targets[index].Expr, p.Targets[index].LegendFormat, want[0], want[1])
			}
		}
		for _, word := range c.words {
			if !strings.Contains(p.Description, word) {
				t.Errorf("%s does not explain %q", c.title, word)
			}
		}
		// the rollout row sits below its own row header, above the next row
		if p.GridPos.Y <= row.GridPos.Y || row.GridPos.Y+25 < p.GridPos.Y+p.GridPos.H {
			t.Errorf("%s at %+v is outside the session rollout row at %d", c.title, p.GridPos, row.GridPos.Y)
		}
	}

	levels := func(p *testPanel) []incidentLevel {
		found := []incidentLevel{}
		for _, step := range p.FieldConfig.Defaults.Thresholds.Steps {
			if step.Value != nil {
				found = append(found, incidentLevel{color: step.Color, value: *step.Value})
			}
		}
		return found
	}
	// the share panel draws the alert levels; the quantiles draw the limit
	share := panels["unchecked share of tagged credential checks by service"]
	if got := levels(share); !slices.Equal(got, []incidentLevel{warnAt(0.05), pageAt(0.25)}) || share.FieldConfig.Defaults.Custom.ThresholdsStyle.Mode != "line" {
		t.Errorf("the unchecked share draws %v as %q, want lines at the alert levels", got, share.FieldConfig.Defaults.Custom.ThresholdsStyle.Mode)
	}
	quantiles := panels["live sessions per network: p50, p99, p99.9"]
	if got := levels(quantiles); !slices.Equal(got, []incidentLevel{pageAt(5000)}) || quantiles.FieldConfig.Defaults.Custom.ThresholdsStyle.Mode != "line" {
		t.Errorf("the quantiles draw %v as %q, want a line at the 5,000 limit", got, quantiles.FieldConfig.Defaults.Custom.ThresholdsStyle.Mode)
	}

	// what the shared panel type leaves out: the dial failures are a Loki
	// query, and the networks per level use a logarithmic axis
	body, err := dashboardsFs.ReadFile("dashboards/signals.json")
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Panels []struct {
			Title      string `json:"title"`
			Datasource struct {
				Uid string `json:"uid"`
			} `json:"datasource"`
			FieldConfig struct {
				Defaults struct {
					Min    *float64 `json:"min"`
					Custom struct {
						ScaleDistribution struct {
							Type string `json:"type"`
							Log  int    `json:"log"`
						} `json:"scaleDistribution"`
					} `json:"custom"`
				} `json:"defaults"`
			} `json:"fieldConfig"`
			Targets []struct {
				Datasource *struct {
					Uid string `json:"uid"`
				} `json:"datasource"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	for _, p := range raw.Panels {
		if _, ok := panels[p.Title]; !ok {
			continue
		}
		if p.Title == "Redis pool dial failures / min by service" {
			if p.Datasource.Uid != "warp-loki" || len(p.Targets) != 1 || p.Targets[0].Datasource == nil || p.Targets[0].Datasource.Uid != "warp-loki" {
				t.Errorf("the dial failures panel does not query warp-loki")
			}
			continue
		}
		if p.Datasource.Uid != "warp-mimir" {
			t.Errorf("%s queries %q, want warp-mimir", p.Title, p.Datasource.Uid)
		}
		scale := p.FieldConfig.Defaults.Custom.ScaleDistribution
		if p.Title == "networks above each live-session level (per sample pass)" {
			if scale.Type != "log" || scale.Log != 10 || p.FieldConfig.Defaults.Min != nil {
				t.Errorf("the networks per level do not use a base-10 logarithmic axis without a zero minimum: %+v", scale)
			}
		} else if scale.Type != "" {
			t.Errorf("%s changes its axis scale to %q", p.Title, scale.Type)
		}
	}

	evaluateSessionRolloutExpressions(t, panels)
}

func evaluateSessionRolloutExpressions(t *testing.T, panels map[string]*testPanel) {
	t.Helper()
	promtool := os.Getenv("URNETWORK_PROMTOOL")
	if promtool == "" {
		var err error
		promtool, err = exec.LookPath("promtool")
		if err != nil {
			t.Fatal("official promtool 3.5.0 is required; run bash ./grafana/promtool.sh from the server root and export its path as URNETWORK_PROMTOOL")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	version, err := exec.CommandContext(ctx, promtool, "--version").CombinedOutput()
	if err != nil || !strings.HasPrefix(string(version), "promtool, version 3.5.0 ") {
		t.Fatal("official promtool 3.5.0 is required")
	}

	series := func(metric, env, service, instance, labels, values string) map[string]string {
		return map[string]string{"series": metric + `{env="` + env + `",service="` + service + `",host="synthetic-host",block="a",instance="` + instance + `"` + labels + `}`, "values": values}
	}
	expression := func(title string, target int) string {
		return strings.NewReplacer("$env", "synthetic", "$__rate_interval", "5m").Replace(panels[title].Targets[target].Expr)
	}
	sample := func(labels string, value float64) map[string]any {
		return map[string]any{"labels": labels, "value": value}
	}

	// per second at 1-minute samples: 60 x the rate per minute
	minuteInputs := []map[string]string{
		// connect: 3% of 10,000 checks a second unchecked, from two paths
		series("urnetwork_jwt_state_record_checks_total", "synthetic", "connect", "c1", `,caller="connect_h1",operation="validate",credential="client",outcome="record_valid"`, "0+582000x20"),
		series("urnetwork_jwt_state_record_checks_total", "synthetic", "connect", "c1", `,caller="unknown",operation="validate",credential="client",outcome="record_unavailable"`, "0+18000x20"),
		series("urnetwork_session_check_unavailable_total", "synthetic", "connect", "c1", `,path="lease",credential="client"`, "0+12000x20"),
		series("urnetwork_session_check_unavailable_total", "synthetic", "connect", "c2", `,path="lease",credential="client"`, "0+5400x20"),
		series("urnetwork_session_check_unavailable_total", "synthetic", "connect", "c2", `,path="connect_h1",credential="session"`, "0+600x20"),
		// proxy: unchecked acceptances and no record checks read 100%
		series("urnetwork_session_check_unavailable_total", "synthetic", "proxy", "p1", `,path="hosted",credential="client"`, "0+120x20"),
		series("urnetwork_redis_pool_timeouts_total", "synthetic", "connect", "c1", "", "0+30x20"),
		series("urnetwork_session_mint_untagged_total", "synthetic", "api", "a1", `,kind="api_key_client"`, "0+60x20"),
		series("urnetwork_session_mint_untagged_total", "synthetic", "api", "a2", `,kind="api_key_client"`, "0+60x20"),
		series("urnetwork_session_mint_untagged_total", "synthetic", "proxy", "p1", `,kind="legacy_proxy"`, "0+6x20"),
		// refreshes and a hosted proxy's children signed on their session unextended
		series("urnetwork_session_mint_unextended_total", "synthetic", "api", "a1", `,kind="signin"`, "0+30x20"),
		series("urnetwork_session_mint_unextended_total", "synthetic", "proxy", "p1", `,kind="client"`, "0+90x20"),
		series("urnetwork_session_mint_unextended_total", "other", "api", "o1", `,kind="signin"`, "0+60000x20"),
		series("urnetwork_session_cap_would_refuse_total", "synthetic", "api", "a1", `,kind="client",cap="live"`, "0+120x20"),
		series("urnetwork_session_cap_would_refuse_total", "synthetic", "proxy", "p1", `,kind="client",cap="live"`, "0+60x20"),
		series("urnetwork_session_cap_would_refuse_total", "synthetic", "api", "a1", `,kind="signin",cap="retained"`, "0+6x20"),
		series("urnetwork_session_cap_refused_total", "synthetic", "api", "a1", `,kind="code",cap="live"`, "0+12x20"),
		series("urnetwork_session_freed_on_removal_total", "synthetic", "taskworker", "t1", `,path="idle_reap"`, "0+600x20"),
		series("urnetwork_session_freed_on_removal_total", "synthetic", "api", "a1", `,path="remove_client"`, "0+90x20"),
		// another environment never shows
		series("urnetwork_session_check_unavailable_total", "other", "connect", "o1", `,path="lease",credential="client"`, "0+60000x20"),
		series("urnetwork_session_cap_would_refuse_total", "other", "api", "o1", `,kind="client",cap="live"`, "0+60000x20"),
		series("urnetwork_session_freed_on_removal_total", "other", "api", "o1", `,path="remove_client"`, "0+60000x20"),
	}
	const (
		uncheckedTitle = "credentials accepted unchecked / s by service, path and credential"
		shareTitle     = "unchecked share of tagged credential checks by service"
		untaggedTitle  = "mints issued untagged or unextended / s by service and kind (Redis unreachable at mint)"
		capTitle       = "session limit: would refuse (unenforced) and refused / s by kind and cap"
		dayTitle       = "would-refuse new sessions in the last 24 hours by kind and cap"
		freedTitle     = "client sessions freed on removal / s by path"
		networksTitle  = "networks above each live-session level (per sample pass)"
		quantileTitle  = "live sessions per network: p50, p99, p99.9"
	)
	minuteTests := []map[string]any{
		{"expr": expression(uncheckedTitle, 0), "eval_time": "10m", "exp_samples": []map[string]any{
			sample(`{credential="client",path="lease",service="connect"}`, 290),
			sample(`{credential="session",path="connect_h1",service="connect"}`, 10),
			sample(`{credential="client",path="hosted",service="proxy"}`, 2),
		}},
		{"expr": expression(uncheckedTitle, 1), "eval_time": "10m", "exp_samples": []map[string]any{
			sample(`{service="connect"}`, 0.5),
		}},
		{"expr": expression(shareTitle, 0), "eval_time": "10m", "exp_samples": []map[string]any{
			sample(`{service="connect"}`, 0.03),
			sample(`{service="proxy"}`, 1),
		}},
		{"expr": expression(untaggedTitle, 0), "eval_time": "10m", "exp_samples": []map[string]any{
			sample(`{kind="api_key_client",service="api"}`, 2),
			sample(`{kind="legacy_proxy",service="proxy"}`, 0.1),
		}},
		{"expr": expression(untaggedTitle, 1), "eval_time": "10m", "exp_samples": []map[string]any{
			sample(`{kind="signin",service="api"}`, 0.5),
			sample(`{kind="client",service="proxy"}`, 1.5),
		}},
		{"expr": expression(capTitle, 0), "eval_time": "10m", "exp_samples": []map[string]any{
			sample(`{cap="live",kind="client"}`, 3),
			sample(`{cap="retained",kind="signin"}`, 0.1),
		}},
		{"expr": expression(capTitle, 1), "eval_time": "10m", "exp_samples": []map[string]any{
			sample(`{cap="live",kind="code"}`, 0.2),
		}},
		{"expr": expression(freedTitle, 0), "eval_time": "10m", "exp_samples": []map[string]any{
			sample(`{path="idle_reap",service="taskworker"}`, 10),
			sample(`{path="remove_client",service="api"}`, 1.5),
		}},
	}

	// Every pass observes the same 3 networks, of 5, 1,500 and 6,000 live
	// sessions: 2 above 10, 100, 500 and 1,000, 1 above 2,000 and 5,000, none
	// above 10,000. Samples are 30 minutes apart, each after passes of its
	// own: the panel divides the observations by the passes counted beside
	// them, whatever the period and whichever taskworker ran them.
	perPass := map[string]int{"10": 1, "100": 1, "500": 1, "1000": 1, "2000": 2, "5000": 2, "10000": 3, "+Inf": 3}
	taskworker := func(env, instance string, passesPerSample int, partial bool) []map[string]string {
		values := func(perSample int) string { return "0+" + strconv.Itoa(perSample) + "x6" }
		inputs := []map[string]string{}
		for le, count := range perPass {
			inputs = append(inputs, series("urnetwork_session_live_per_network_bucket", env, "taskworker", instance, `,le="`+le+`"`, values(count*passesPerSample)))
		}
		inputs = append(inputs,
			series("urnetwork_session_live_per_network_count", env, "taskworker", instance, "", values(perPass["+Inf"]*passesPerSample)),
			series("urnetwork_session_live_sample_passes_total", env, "taskworker", instance, `,outcome="complete"`, values(passesPerSample)))
		if partial {
			inputs = append(inputs, series("urnetwork_session_live_sample_passes_total", env, "taskworker", instance, `,outcome="partial"`, values(0)))
		}
		return inputs
	}
	perPassSamples := func(complete, partial float64) []map[string]any {
		return []map[string]any{
			{"expr": expression(networksTitle, 0), "eval_time": "2h", "exp_samples": []map[string]any{sample(`{}`, 3)}},
			{"expr": expression(networksTitle, 1), "eval_time": "2h", "exp_samples": []map[string]any{
				sample(`{le="10"}`, 2), sample(`{le="100"}`, 2), sample(`{le="500"}`, 2), sample(`{le="1000"}`, 2),
				sample(`{le="2000"}`, 1), sample(`{le="5000"}`, 1), sample(`{le="10000"}`, 0),
			}},
			{"expr": expression(networksTitle, 2), "eval_time": "2h", "exp_samples": func() []map[string]any {
				samples := []map[string]any{sample(`{outcome="complete"}`, complete)}
				if 0 <= partial {
					samples = append(samples, sample(`{outcome="partial"}`, partial))
				}
				return samples
			}()},
		}
	}
	// two taskworkers each run a pass every 30 minutes: 4 passes an hour
	fourInputs := slices.Concat(
		taskworker("synthetic", "t1", 1, true),
		taskworker("synthetic", "t2", 1, false),
		taskworker("other", "o1", 300, false),
	)
	fourTests := append(perPassSamples(4, 0),
		// linear within a bucket: rank 1.5 of 3 is halfway from 1,000 to 2,000
		map[string]any{"expr": expression(quantileTitle, 0), "eval_time": "2h", "exp_samples": []map[string]any{sample(`{}`, 1500)}},
		map[string]any{"expr": expression(quantileTitle, 1), "eval_time": "2h", "exp_samples": []map[string]any{sample(`{}`, 9850)}},
		map[string]any{"expr": expression(quantileTitle, 2), "eval_time": "2h", "exp_samples": []map[string]any{sample(`{}`, 9985)}},
	)
	// one taskworker runs a pass every 30 minutes: 2 passes an hour, the same
	// networks per pass
	twoInputs := taskworker("synthetic", "t3", 1, false)
	twoTests := perPassSamples(2, -1)

	// hourly samples over a day and a half: 24 would-be refusals an hour
	dayInputs := []map[string]string{
		series("urnetwork_session_cap_would_refuse_total", "synthetic", "api", "a1", `,kind="client",cap="live"`, "0+24x36"),
		series("urnetwork_session_cap_would_refuse_total", "other", "api", "o1", `,kind="client",cap="live"`, "0+2400x36"),
	}
	dayTests := []map[string]any{
		{"expr": expression(dayTitle, 0), "eval_time": "30h", "exp_samples": []map[string]any{sample(`{cap="live",kind="client"}`, 576)}},
	}

	fixture, err := json.Marshal(map[string]any{"evaluation_interval": "1m", "tests": []map[string]any{
		{"name": "session rollout rates and shares", "interval": "1m", "input_series": minuteInputs, "promql_expr_test": minuteTests},
		{"name": "live sessions per network, 4 passes an hour on two taskworkers", "interval": "30m", "input_series": fourInputs, "promql_expr_test": fourTests},
		{"name": "live sessions per network, 2 passes an hour", "interval": "30m", "input_series": twoInputs, "promql_expr_test": twoTests},
		{"name": "would-be refusals per day", "interval": "1h", "input_series": dayInputs, "promql_expr_test": dayTests},
	}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "session_rollout.yml")
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(ctx, promtool, "test", "rules", path).CombinedOutput(); err != nil {
		t.Fatalf("session rollout expression evaluation: %v\n%s", err, output)
	}
}
