package grafana

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type testDashboard struct {
	Uid        string   `json:"uid"`
	Title      string   `json:"title"`
	Tags       []string `json:"tags"`
	Templating struct {
		List []any `json:"list"`
	} `json:"templating"`
	Panels []testPanel `json:"panels"`
}

type testPanel struct {
	Id      int    `json:"id"`
	Type    string `json:"type"`
	Title   string `json:"title"`
	GridPos struct {
		H int `json:"h"`
		W int `json:"w"`
		X int `json:"x"`
		Y int `json:"y"`
	} `json:"gridPos"`
	Options struct {
		ReduceOptions struct {
			Calcs []string `json:"calcs"`
		} `json:"reduceOptions"`
	} `json:"options"`
	Targets []testTarget `json:"targets"`
}

type testTarget struct {
	Expr    string `json:"expr"`
	Instant bool   `json:"instant"`
	Range   *bool  `json:"range"`
}

func readTestDashboard(t *testing.T, name string) testDashboard {
	t.Helper()
	body, err := dashboardsFs.ReadFile("dashboards/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var dashboard testDashboard
	if err := json.Unmarshal(body, &dashboard); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return dashboard
}

func dashboardExpressions(dashboard testDashboard) []string {
	var expressions []string
	for _, panel := range dashboard.Panels {
		for _, target := range panel.Targets {
			if target.Expr != "" {
				expressions = append(expressions, target.Expr)
			}
		}
	}
	return expressions
}

func TestDefaultDashboardDocumentsAreValid(t *testing.T) {
	entries, err := dashboardsFs.ReadDir("dashboards")
	if err != nil {
		t.Fatal(err)
	}
	seenUids := map[string]string{}
	for _, entry := range entries {
		dashboard := readTestDashboard(t, entry.Name())
		if dashboard.Uid == "" || dashboard.Title == "" {
			t.Errorf("%s must have a stable uid and title", entry.Name())
		}
		if previous := seenUids[dashboard.Uid]; previous != "" {
			t.Errorf("dashboard uid %q is shared by %s and %s", dashboard.Uid, previous, entry.Name())
		}
		seenUids[dashboard.Uid] = entry.Name()

		seenPanelIds := map[int]string{}
		for panelIndex, panel := range dashboard.Panels {
			if panel.Type == "" || panel.Title == "" {
				t.Errorf("%s panel %d must have a type and title", entry.Name(), panelIndex)
			}
			if panel.GridPos.X < 0 || panel.GridPos.Y < 0 || panel.GridPos.W <= 0 || panel.GridPos.H <= 0 || 24 < panel.GridPos.X+panel.GridPos.W {
				t.Errorf("%s panel %q has invalid grid position %+v", entry.Name(), panel.Title, panel.GridPos)
			}
			for previousIndex := range panelIndex {
				previous := dashboard.Panels[previousIndex]
				xOverlap := panel.GridPos.X < previous.GridPos.X+previous.GridPos.W && previous.GridPos.X < panel.GridPos.X+panel.GridPos.W
				yOverlap := panel.GridPos.Y < previous.GridPos.Y+previous.GridPos.H && previous.GridPos.Y < panel.GridPos.Y+panel.GridPos.H
				if xOverlap && yOverlap {
					t.Errorf("%s panels %q and %q overlap", entry.Name(), previous.Title, panel.Title)
				}
			}
			// Grafana assigns ids to the older dashboards that omit them. When
			// an authored id is present, keep it unique so public panel API urls
			// and dashboard edits remain stable.
			if panel.Id != 0 {
				if previous := seenPanelIds[panel.Id]; previous != "" {
					t.Errorf("%s panel id %d is shared by %q and %q", entry.Name(), panel.Id, previous, panel.Title)
				}
				seenPanelIds[panel.Id] = panel.Title
			}
		}

		if slices.Contains(dashboard.Tags, PublicTag) && len(dashboard.Templating.List) != 0 {
			t.Errorf("public dashboard %s uses template variables, which Grafana public dashboards do not support", entry.Name())
		}
	}
}

var networkMeasurementMetrics = []string{
	"urnetwork_stats_total_networks",
	"urnetwork_stats_block_users",
	"urnetwork_stats_countries",
	"urnetwork_stats_staked_alpha",
	"urnetwork_stats_block_demand_deposits_alpha",
	"urnetwork_stats_block_miner_emissions_alpha",
	"urnetwork_stats_alpha_usd",
	"urnetwork_stats_prev_block_users",
	"urnetwork_stats_prev_block_demand_deposits_alpha",
	"urnetwork_stats_prev_block_miner_emissions_alpha",
}

func TestPublicNetworkStatsCoversEveryMeasurement(t *testing.T) {
	dashboard := readTestDashboard(t, "public-traffic.json")
	if !slices.Contains(dashboard.Tags, PublicTag) {
		t.Fatal("public network stats dashboard lost its public tag")
	}
	expressions := dashboardExpressions(dashboard)
	for _, metric := range networkMeasurementMetrics {
		want := fmt.Sprintf("max(%s)", metric)
		if !slices.Contains(expressions, want) {
			t.Errorf("public network stats is missing replica-safe query %q", want)
		}
	}
	for _, metric := range []string{"urnetwork_connect_transfer_bytes", "urnetwork_connect_resident_clients"} {
		if !strings.Contains(strings.Join(expressions, "\n"), metric) {
			t.Errorf("public network stats is missing %s", metric)
		}
	}
	for _, expression := range expressions {
		if strings.Contains(expression, "$env") {
			t.Errorf("public query uses unsupported template variable: %s", expression)
		}
	}

	var traffic *testPanel
	for i := range dashboard.Panels {
		if dashboard.Panels[i].Id == 1 {
			traffic = &dashboard.Panels[i]
			break
		}
	}
	if traffic == nil || len(traffic.Targets) != 1 {
		t.Fatal("public traffic total panel is missing")
	}
	target := traffic.Targets[0]
	if target.Expr != `sum(increase(urnetwork_connect_transfer_bytes{instance!=""}[$__range]))` {
		t.Errorf("traffic total query = %q", target.Expr)
	}
	if !target.Instant || target.Range == nil || *target.Range {
		t.Error("traffic total must be an instant query over $__range")
	}
	if !slices.Contains(traffic.Options.ReduceOptions.Calcs, "lastNotNull") {
		t.Error("traffic total must reduce its one instant value with lastNotNull")
	}
}

// registeredApplicationMetrics inventories prometheus option literals in the
// production Go sources. The stats collector creates its gauges through a
// small wrapper, so its string-literal call sites are handled explicitly.
// This makes adding a metric without placing it on an internal dashboard a
// test failure instead of a silent observability gap.
func registeredApplicationMetrics(t *testing.T) []string {
	t.Helper()
	metrics := map[string]bool{}
	stringLiteral := func(expression ast.Expr) (string, bool) {
		literal, ok := expression.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(literal.Value)
		return value, err == nil
	}

	err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if function, ok := call.Fun.(*ast.Ident); ok && function.Name == "newStatsGauge" && 0 < len(call.Args) {
					if name, ok := stringLiteral(call.Args[0]); ok {
						metrics["urnetwork_stats_"+name] = true
					}
				}
			}

			literal, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			selector, ok := literal.Type.(*ast.SelectorExpr)
			if !ok || !slices.Contains([]string{"CounterOpts", "GaugeOpts", "HistogramOpts", "SummaryOpts"}, selector.Sel.Name) {
				return true
			}
			parts := map[string]string{}
			for _, element := range literal.Elts {
				field, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := field.Key.(*ast.Ident)
				if !ok || !slices.Contains([]string{"Namespace", "Subsystem", "Name"}, key.Name) {
					continue
				}
				if value, ok := stringLiteral(field.Value); ok {
					parts[key.Name] = value
				}
			}
			if parts["Name"] == "" {
				return true
			}
			nameParts := []string{}
			for _, key := range []string{"Namespace", "Subsystem", "Name"} {
				if parts[key] != "" {
					nameParts = append(nameParts, parts[key])
				}
			}
			name := strings.Join(nameParts, "_")
			if strings.HasPrefix(name, "urnetwork_") {
				metrics[name] = true
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(metrics))
	for metric := range metrics {
		names = append(names, metric)
	}
	slices.Sort(names)
	return names
}

func TestInternalDashboardsCoverEveryApplicationMetric(t *testing.T) {
	entries, err := dashboardsFs.ReadDir("dashboards")
	if err != nil {
		t.Fatal(err)
	}
	var internal strings.Builder
	for _, entry := range entries {
		dashboard := readTestDashboard(t, entry.Name())
		if slices.Contains(dashboard.Tags, PublicTag) {
			continue
		}
		for _, expression := range dashboardExpressions(dashboard) {
			internal.WriteString(expression)
			internal.WriteByte('\n')
		}
	}
	queries := internal.String()
	metrics := registeredApplicationMetrics(t)
	if len(metrics) == 0 {
		t.Fatal("did not find any registered application metrics")
	}
	for _, metric := range metrics {
		if !strings.Contains(queries, metric) {
			t.Errorf("custom application metric %s is absent from the internal dashboards", metric)
		}
	}
}

func TestInternalNetworkMeasurementsAreScopedAndReplicaSafe(t *testing.T) {
	expressions := dashboardExpressions(readTestDashboard(t, "signals.json"))
	for _, metric := range networkMeasurementMetrics {
		want := fmt.Sprintf(`max(%s{env="$env"})`, metric)
		if !slices.Contains(expressions, want) {
			t.Errorf("internal network measurements is missing query %q", want)
		}
	}
}
