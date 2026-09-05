package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Uses the exact production queries and authored dashboard expressions. The
// engine is optional for ordinary package builds but required as a release
// gate for this signal: point MONITOR_PROMQL_ENGINE_SOURCE at pinned Mimir
// source containing its vendored Prometheus engine, then run this test.
func TestSubtensorConvergencePromQLEngine(t *testing.T) {
	engineSource := os.Getenv("MONITOR_PROMQL_ENGINE_SOURCE")
	if engineSource == "" {
		t.Skip("set MONITOR_PROMQL_ENGINE_SOURCE to run the required pinned-engine regression")
	}
	engineSource, err := filepath.Abs(engineSource)
	if err != nil {
		t.Fatal(err)
	}
	engineTest, err := filepath.Abs("testdata/subtensor_promql_engine_test.go")
	if err != nil {
		t.Fatal(err)
	}
	script := subtensorConvergencePromQLScript(t)
	scriptPath := filepath.Join(t.TempDir(), "subtensor.test")
	if err := os.WriteFile(scriptPath, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "test", "-mod=vendor", engineTest, "-run", "^TestSubtensorPromQL$", "-count=1")
	command.Dir = engineSource
	command.Env = append(os.Environ(), "GOWORK=off", "MONITOR_PROMQL_TEST_SCRIPT="+scriptPath)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("pinned PromQL engine regression: %v\n%s", err, output)
	}
	t.Logf("pinned PromQL engine: %s", strings.TrimSpace(string(output)))
}

// Ensures the optional engine gate cannot silently test a stale copy of the
// dashboard. It shares expression construction only; the engine executes the
// actual JSON expressions below on independent raw sample fixtures.
func TestSubtensorConvergenceDashboardBindsCanonicalTargetExpressions(t *testing.T) {
	subtensorConvergenceDashboardExpressions(t)
}

func subtensorConvergenceDashboardExpressions(t testing.TB) map[int]string {
	t.Helper()
	raw, err := os.ReadFile("../grafana/dashboards/subtensor.json")
	if err != nil {
		t.Fatal(err)
	}
	var dashboard struct {
		Panels []struct {
			Id      int `json:"id"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(raw, &dashboard); err != nil {
		t.Fatal(err)
	}
	actual := map[int]string{}
	for _, panel := range dashboard.Panels {
		if panel.Id == 13 || panel.Id == 14 || panel.Id == 15 {
			if len(panel.Targets) != 1 {
				t.Fatalf("panel %d has %d expressions", panel.Id, len(panel.Targets))
			}
			actual[panel.Id] = panel.Targets[0].Expr
		}
	}
	expressions := subtensorConvergenceExpressions(
		`env="$env",host=~"$host",chain=~"$chain",job=~"^(?:subtensor|subtensor-lightnode)$"`,
		`env="$env",host=~"$host",chain=~"$chain",job=~"$node"`,
	)
	for id, want := range map[int]string{
		13: expressions["lag"],
		14: "(" + expressions["net_rate"] + ") and (" + expressions["sample_count"] + " >= 200) and (" + expressions["target_sample_count"] + " >= 200) and (" + expressions["sample_age"] + " <= 90)",
		15: expressions["sample_age"],
	} {
		if actual[id] != want {
			t.Fatalf("dashboard panel %d diverged from its tested canonical target expression", id)
		}
	}
	return actual
}

// Each mode modifies only target visibility or source identity. Both best
// heads advance linearly except the explicit caught-up, zero-import control.
func subtensorConvergencePromQLData(mode string) string {
	return "clear\nload 15s\n" + subtensorConvergencePromQLSeries(mode) + "\n"
}

func subtensorConvergencePromQLSeries(mode string) string {
	var data strings.Builder
	for nodeIndex, job := range []string{"subtensor", "subtensor-lightnode"} {
		values := map[string][]string{}
		for step := 0; step <= 240; step++ {
			best := 1000*(nodeIndex+1) + 15*step
			target := 100000 + step
			major, count, duration, queued := "1", strconv.Itoa(15*step), strconv.Itoa(3*step), "50"
			fallback := false
			switch mode {
			case "one fallback":
				fallback = nodeIndex == 1
			case "alternating fallback":
				fallback = step%2 == nodeIndex
			case "target rises from fallback":
				fallback = nodeIndex == 1 && step < 120
			case "target drops to fallback":
				fallback = nodeIndex == 1 && step >= 120
			case "diverging target drops to fallback":
				best, target = 1000*(nodeIndex+1)+step, 100000+2*step
				count, duration = strconv.Itoa(step), strconv.FormatFloat(0.2*float64(step), 'f', 1, 64)
				fallback = nodeIndex == 1 && step >= 120
			case "short simultaneous gap":
				fallback = 90 <= step && step < 110
			case "all fallback":
				fallback = true
			case "current simultaneous fallback":
				fallback = step > 230
			case "short target history":
				fallback = step < 42
			case "target behind node":
				if nodeIndex == 1 {
					best, fallback = 200000+15*step, true
				}
			case "caught up", "stale major":
				best, target, major, count, duration, queued = 100000, 100000, "0", "0", "0", "0"
			}
			if fallback {
				target = best
			}
			bestText, targetText := strconv.Itoa(best), strconv.Itoa(target)
			// At evaluation these samples are 150s old: inside the engine's
			// five-minute lookback, but outside our explicit 90s boundary.
			if mode == "stale target" && step > 230 {
				targetText = "_"
			}
			if mode == "stale major" && step > 230 {
				major = "_"
			}
			if mode == "short best history" && step < 42 {
				bestText = "_"
			}
			if mode == "stale best" && step > 230 {
				bestText = "_"
			}
			for name, value := range map[string]string{
				"best": bestText, "sync_target": targetText, "major": major,
				"count": count, "sum": duration, "queue": queued,
			} {
				values[name] = append(values[name], value)
			}
		}
		labels := `env="main",host="snow",chain="bittensor",job=` + strconv.Quote(job)
		for _, measure := range []struct{ key, metric, extra string }{
			{key: "best", metric: "substrate_block_height", extra: `,status="best"`},
			{key: "sync_target", metric: "substrate_block_height", extra: `,status="sync_target"`},
			{key: "major", metric: "substrate_sub_libp2p_is_major_syncing"},
			{key: "count", metric: "substrate_block_verification_and_import_time_count"},
			{key: "sum", metric: "substrate_block_verification_and_import_time_sum"},
			{key: "queue", metric: "substrate_sync_queued_blocks"},
		} {
			fmt.Fprintf(&data, "  %s{%s%s} %s\n", measure.metric, labels, measure.extra, strings.Join(values[measure.key], " "))
		}
	}
	return data.String()
}

// The legacy comparisons intentionally reproduce both false zero lag and
// slope errors from target fallback transitions. New assertions exercise the
// entire generated measure union before selecting the measure under test.
func subtensorConvergencePromQLScript(t testing.TB) string {
	t.Helper()
	targets := map[string]subtensorConvergenceTarget{
		"snow\x00subtensor":           {host: "snow", job: "subtensor"},
		"snow\x00subtensor-lightnode": {host: "snow", job: "subtensor-lightnode"},
	}
	query := subtensorConvergenceQuery("main", targets)
	measure := func(name string) string {
		return "(" + query + ") and on (monitor_measure) label_replace(vector(1),\"monitor_measure\"," + strconv.Quote(name) + ",\"\",\"\")"
	}
	dashboard := subtensorConvergenceDashboardExpressions(t)
	for id, expression := range dashboard {
		dashboard[id] = strings.NewReplacer("$env", "main", "$host", "snow", "$chain", "bittensor", "$node", "subtensor-lightnode").Replace(expression)
	}
	var script strings.Builder
	eval := func(expression, expected string) {
		fmt.Fprintf(&script, "eval instant at 60m %s\n  {} %s\n\n", expression, expected)
	}
	count := func(expression string, expected int) {
		eval("count("+expression+") or vector(0)", strconv.Itoa(expected))
	}
	near := func(expression, expected string) {
		eval("abs(sum("+expression+") - ("+expected+")) < bool 0.000001", "1")
	}
	legacyLabels := `env="main",host="snow",chain="bittensor",job="subtensor-lightnode"`
	legacyBest := `substrate_block_height{` + legacyLabels + `,status="best"}`
	legacyTarget := `substrate_block_height{` + legacyLabels + `,status="sync_target"}`
	legacyNet := `deriv(` + legacyBest + `[1h]) - ignoring (status) deriv(` + legacyTarget + `[1h])`
	for _, mode := range []string{"one fallback", "alternating fallback", "target rises from fallback", "target drops to fallback", "short simultaneous gap"} {
		script.WriteString("# " + mode + "\n")
		script.WriteString(subtensorConvergencePromQLData(mode))
		count(query, 18)
		near(measure("net_rate"), "28/15")
		near(measure("target_rate"), "2/15")
		near(measure("lag"), "190280")
		near(dashboard[13], "94640")
		near(dashboard[14], "14/15")
		switch mode {
		case "one fallback":
			near(legacyTarget+` - ignoring (status) `+legacyBest, "0")
		case "target rises from fallback":
			eval("sum("+legacyNet+") < bool 0", "1")
		case "target drops to fallback":
			eval("sum("+legacyNet+") > bool 10", "1")
		}
	}
	script.WriteString("# old per-job target reports positive catch-up while true lag is growing\n")
	script.WriteString(subtensorConvergencePromQLData("diverging target drops to fallback"))
	count(query, 18)
	near(measure("net_rate"), "-2/15")
	near(measure("lag"), "197480")
	near(dashboard[13], "98240")
	near(dashboard[14], "-1/15")
	eval("sum("+legacyNet+") > bool 10", "1")
	for _, mode := range []string{"all fallback", "current simultaneous fallback", "stale target", "stale major", "stale best"} {
		script.WriteString("# " + mode + "\n")
		script.WriteString(subtensorConvergencePromQLData(mode))
		count(measure("lag"), 0)
		count(dashboard[13], 0)
		count(dashboard[14], 0)
	}
	for _, mode := range []string{"short target history", "short best history"} {
		script.WriteString("# " + mode + "\n")
		script.WriteString(subtensorConvergencePromQLData(mode))
		count(dashboard[14], 0)
		count("("+measure("target_sample_count")+") < 200", 2)
	}
	script.WriteString("# caught-up equality is valid with fresh major_syncing=0 and zero actual imports\n")
	script.WriteString(subtensorConvergencePromQLData("caught up"))
	count(query, 18)
	for _, name := range []string{"lag", "net_rate", "target_rate", "import_rate", "import_seconds"} {
		near(measure(name), "0")
	}
	near(dashboard[13], "0")
	near(dashboard[14], "0")
	script.WriteString("# a reference behind the selected node is unknown, not clamped zero lag\n")
	script.WriteString(subtensorConvergencePromQLData("target behind node"))
	count(query, 14)
	count(dashboard[13], 0)
	count(dashboard[14], 0)

	// The configured pairs are snow/archive and other/lightnode. Poison the
	// other two pairs with a much faster target; a Cartesian selector admits
	// both bogus references even though each label is individually allowed.
	script.WriteString("# exact inventory pairs exclude cross-host job pollution\nclear\nload 15s\n")
	pairedSeries := subtensorConvergencePromQLSeries("") + strings.ReplaceAll(subtensorConvergencePromQLSeries(""), `host="snow"`, `host="other"`)
	for _, line := range strings.Split(strings.TrimSuffix(pairedSeries, "\n"), "\n") {
		unconfigured := (strings.Contains(line, `host="snow"`) && strings.Contains(line, `job="subtensor-lightnode"`)) ||
			(strings.Contains(line, `host="other"`) && strings.Contains(line, `job="subtensor"`))
		if unconfigured && strings.Contains(line, `status="sync_target"`) {
			line = line[:strings.Index(line, "}")+1] + " 900000+1000x240"
		}
		script.WriteString(line + "\n")
	}
	script.WriteByte('\n')
	pairedQuery := subtensorConvergenceQuery("main", map[string]subtensorConvergenceTarget{
		"snow\x00subtensor":            {host: "snow", job: "subtensor"},
		"other\x00subtensor-lightnode": {host: "other", job: "subtensor-lightnode"},
	})
	count(pairedQuery, 18)
	near("("+pairedQuery+") and on (monitor_measure) label_replace(vector(1),\"monitor_measure\",\"lag\",\"\",\"\")", "190280")
	legacyCrossedTarget := `max by (host) (substrate_block_height{env="main",host=~"snow|other",job=~"subtensor|subtensor-lightnode",status="sync_target"})`
	near(legacyCrossedTarget, "2280000")

	// A second chain remains separate in PromQL, then is rejected as an
	// ambiguous node identity by the parser tests. Other hosts, environments,
	// and unconfigured jobs cannot supply a target for snow/bittensor.
	script.WriteString("# preserve chain grouping and exclude unrelated hosts, environments, and jobs\nclear\nload 15s\n")
	script.WriteString(subtensorConvergencePromQLSeries("one fallback"))
	noise := strings.ReplaceAll(subtensorConvergencePromQLSeries("caught up"), "100000", "900000")
	script.WriteString(strings.ReplaceAll(noise, `chain="bittensor"`, `chain="other-chain"`))
	script.WriteString(strings.ReplaceAll(noise, `host="snow"`, `host="other"`))
	script.WriteString(strings.ReplaceAll(noise, `env="main"`, `env="other"`))
	script.WriteString(strings.NewReplacer(`job="subtensor"`, `job="unconfigured"`, `job="subtensor-lightnode"`, `job="unconfigured-lightnode"`).Replace(noise))
	script.WriteByte('\n')
	count(query, 36)
	near("("+measure("lag")+") and on (chain) label_replace(vector(1),\"chain\",\"bittensor\",\"\",\"\")", "190280")
	near("("+measure("lag")+") and on (chain) label_replace(vector(1),\"chain\",\"other-chain\",\"\",\"\")", "0")
	near(dashboard[13], "94640")
	near(dashboard[14], "14/15")
	for _, scope := range []struct{ host, chain string }{
		{host: "snow", chain: "bittensor|other-chain"},
		{host: "snow|other", chain: "bittensor"},
	} {
		multiScope := strings.NewReplacer("$env", "main", "$host", scope.host, "$chain", scope.chain, "$node", "subtensor-lightnode").Replace(subtensorConvergenceDashboardExpressions(t)[13])
		count(multiScope, 2)
		near(multiScope, "94640")
	}
	return script.String()
}
