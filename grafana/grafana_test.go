package grafana

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
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
		Layers []struct {
			Type     string `json:"type"`
			Location struct {
				Mode      string `json:"mode"`
				Lookup    string `json:"lookup"`
				Gazetteer string `json:"gazetteer"`
			} `json:"location"`
		} `json:"layers"`
	} `json:"options"`
	Targets []testTarget `json:"targets"`
}

type testTarget struct {
	Expr    string `json:"expr"`
	Instant bool   `json:"instant"`
	Range   *bool  `json:"range"`
	Format  string `json:"format"`
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

func dashboardPanelById(dashboard testDashboard, id int) *testPanel {
	for panelIndex := range dashboard.Panels {
		if dashboard.Panels[panelIndex].Id == id {
			return &dashboard.Panels[panelIndex]
		}
	}
	return nil
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

func TestInfrastructureDashboardsCoverServiceAndHostSignals(t *testing.T) {
	tests := []struct {
		name           string
		diskMountpoint string
		serviceMetrics []string
	}{
		{
			name:           "minio.json",
			diskMountpoint: "/mnt/data",
			serviceMetrics: []string{
				"minio_cluster_health_nodes_online_count",
				"minio_cluster_health_drives_offline_count",
				"minio_cluster_health_capacity_usable_free_bytes",
				"minio_cluster_usage_buckets_total_bytes",
				"minio_api_requests_5xx_errors_total",
			},
		},
		{
			name:           "subtensor.json",
			diskMountpoint: "/",
			serviceMetrics: []string{
				"substrate_block_height",
				"substrate_sub_libp2p_peers_count",
				"substrate_sub_libp2p_is_major_syncing",
				"substrate_ready_transactions_number",
				"substrate_rpc_sessions_opened",
			},
		},
		{
			name:           "postgres.json",
			diskMountpoint: "/",
			serviceMetrics: []string{
				"pg_up",
				"pg_stat_activity_count",
				"pg_stat_activity_max_tx_duration",
				"pg_settings_max_connections",
				"pg_stat_database_xact_commit",
				"pg_stat_database_deadlocks",
				"pg_locks_count",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dashboard := readTestDashboard(t, test.name)
			queries := strings.Join(dashboardExpressions(dashboard), "\n")
			for _, metric := range test.serviceMetrics {
				if !strings.Contains(queries, metric) {
					t.Errorf("dashboard is missing service metric %s", metric)
				}
			}
			if !strings.Contains(queries, "node_cpu_seconds_total") ||
				!strings.Contains(queries, "node_memory_MemAvailable_bytes") {
				t.Error("dashboard must include host CPU and memory context")
			}
			if !strings.Contains(queries, "node_filesystem_avail_bytes") ||
				!strings.Contains(queries, `mountpoint="`+test.diskMountpoint+`"`) {
				t.Errorf("dashboard must include host disk context for %s", test.diskMountpoint)
			}
			if !strings.Contains(queries, `{env="$env"`) ||
				!strings.Contains(queries, `host=~"$host"`) {
				t.Error("dashboard queries must be scoped by env and host")
			}
		})
	}
}

func TestSubtensorDashboardSeparatesArchiveAndLightnodeMetrics(t *testing.T) {
	dashboard := readTestDashboard(t, "subtensor.json")
	queries := dashboardExpressions(dashboard)
	for _, query := range queries {
		if strings.Contains(query, "substrate_") && !strings.Contains(query, `job=~"$node"`) {
			t.Errorf("Subtensor query does not honor the archive/lightnode selector: %s", query)
		}
	}
	raw, err := dashboardsFs.ReadFile("dashboards/subtensor.json")
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	for _, required := range []string{
		`"name": "node"`,
		`subtensor(|-lightnode)`,
		`max by (job)`,
		`{{job}}`,
	} {
		if !strings.Contains(content, required) {
			t.Errorf("Subtensor dashboard does not separate both node jobs: missing %s", required)
		}
	}
}

// the scalar operator network measurements (controller/stats_collector.go).
// every taskworker publishes the same value, so a dashboard must read each
// with max — never sum or avg — to select the measurement without replica
// skew. the public dashboard and the internal signals dashboard must both
// show every one of them
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
	"urnetwork_stats_users_24h",
	"urnetwork_stats_online_providers",
	"urnetwork_stats_provider_regions",
	"urnetwork_stats_provider_cities",
	"urnetwork_stats_block_number",
	"urnetwork_stats_block_start_seconds",
	"urnetwork_stats_block_end_seconds",
	"urnetwork_stats_block_miner_claims_alpha",
	"urnetwork_stats_block_miners_claimed",
	"urnetwork_stats_prev_block_miner_claims_alpha",
	"urnetwork_stats_prev_block_miners_claimed",
}

// the labeled operator network measurements, read with max by (labels)
var networkLabeledMeasurementMetrics = []string{
	"urnetwork_stats_online_providers_by_country",
}

// the only metrics a public (no login) dashboard may query. everything
// else in the registry is internal: infrastructure health, error and
// auth taxonomies, drain and deploy state, allocator internals, and the
// exchange mesh detail (see grafana.go). adding a metric here is a
// publication decision, so it is deliberately an explicit list
var publicSafeMetrics = append(append([]string{
	"urnetwork_connect_transfer_bytes",
	"urnetwork_connect_resident_clients",
	"urnetwork_connect_exchange_io_bytes_total",
	"urnetwork_connect_connection_new",
}, networkMeasurementMetrics...), networkLabeledMeasurementMetrics...)

var metricNamePattern = regexp.MustCompile(`urnetwork_[a-z0-9_]+`)

// metricOccurrences returns the byte offsets at which the whole metric
// name occurs in expression (not as a prefix of a longer name)
func metricOccurrences(expression string, metric string) []int {
	offsets := []int{}
	for _, index := range metricNamePattern.FindAllStringIndex(expression, -1) {
		if expression[index[0]:index[1]] == metric {
			offsets = append(offsets, index[0])
		}
	}
	return offsets
}

var maxByPrefixPattern = regexp.MustCompile(`max by \([a-z_, ]+\) \($`)

// assertReplicaSafeReads fails unless every occurrence of metric in
// expression is read as max(<metric>...) or max by (...) (<metric>...),
// and, when selector is not empty, is immediately followed by it
func assertReplicaSafeReads(t *testing.T, where string, expression string, metric string, selector string) {
	t.Helper()
	for _, offset := range metricOccurrences(expression, metric) {
		before := expression[:offset]
		if !strings.HasSuffix(before, "max(") && !maxByPrefixPattern.MatchString(before) {
			t.Errorf("%s reads replicated measurement %s without max: %s", where, metric, expression)
		}
		after := expression[offset+len(metric):]
		if selector != "" && !strings.HasPrefix(after, selector) {
			t.Errorf("%s reads %s without the %s selector: %s", where, metric, selector, expression)
		}
	}
}

func TestPublicNetworkStatsCoversEveryMeasurement(t *testing.T) {
	dashboard := readTestDashboard(t, "public-traffic.json")
	if !slices.Contains(dashboard.Tags, PublicTag) {
		t.Fatal("public network stats dashboard lost its public tag")
	}
	expressions := dashboardExpressions(dashboard)
	joined := strings.Join(expressions, "\n")
	for _, metric := range append(slices.Clone(networkMeasurementMetrics), networkLabeledMeasurementMetrics...) {
		if len(metricOccurrences(joined, metric)) == 0 {
			t.Errorf("public network stats is missing measurement %s", metric)
		}
		for _, expression := range expressions {
			assertReplicaSafeReads(t, "public network stats", expression, metric, "")
		}
	}
	for _, metric := range []string{"urnetwork_connect_transfer_bytes", "urnetwork_connect_resident_clients", "urnetwork_connect_connection_new"} {
		if len(metricOccurrences(joined, metric)) == 0 {
			t.Errorf("public network stats is missing %s", metric)
		}
	}
	for _, expression := range expressions {
		if strings.Contains(expression, "$env") {
			t.Errorf("public query uses unsupported template variable: %s", expression)
		}
	}

	traffic := dashboardPanelById(dashboard, 1)
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

// a public dashboard is readable without a login, so it may query only the
// allowlisted public metrics, and it may never break a query out by the
// pusher's fleet labels: per-host, per-process, per-deploy-block, or
// per-service series disclose fleet size, per-host capacity, and deploy
// cadence even when the metric itself is public
func TestPublicDashboardsQueryOnlyPublicSafeMetrics(t *testing.T) {
	entries, err := dashboardsFs.ReadDir("dashboards")
	if err != nil {
		t.Fatal(err)
	}
	fleetLabelPattern := regexp.MustCompile(`by \([^)]*\b(host|instance|block|service|env)\b`)
	public := 0
	for _, entry := range entries {
		dashboard := readTestDashboard(t, entry.Name())
		if !slices.Contains(dashboard.Tags, PublicTag) {
			continue
		}
		public += 1
		for _, expression := range dashboardExpressions(dashboard) {
			for _, metric := range metricNamePattern.FindAllString(expression, -1) {
				if !slices.Contains(publicSafeMetrics, metric) {
					t.Errorf("%s queries %s, which is not a public-safe metric: %s", entry.Name(), metric, expression)
				}
			}
			if fleetLabelPattern.MatchString(expression) {
				t.Errorf("%s breaks a public query out by a fleet label: %s", entry.Name(), expression)
			}
		}
	}
	if public == 0 {
		t.Fatal("no public dashboard found")
	}
}

// the provider map is the public dashboard's centerpiece: an instant table
// query of the per-country gauge, placed by looking the ISO country code up
// in grafana's bundled country gazetteer. the collector exports the code
// upper case to match the gazetteer keys
func TestPublicNetworkStatsProviderMap(t *testing.T) {
	dashboard := readTestDashboard(t, "public-traffic.json")
	var maps []testPanel
	for _, panel := range dashboard.Panels {
		if panel.Type == "geomap" {
			maps = append(maps, panel)
		}
	}
	if len(maps) != 1 {
		t.Fatalf("public network stats has %d geomap panels, want 1", len(maps))
	}
	geomap := maps[0]
	if len(geomap.Targets) != 1 {
		t.Fatalf("provider map has %d targets, want 1", len(geomap.Targets))
	}
	target := geomap.Targets[0]
	if target.Expr != "max by (country_code, country) (urnetwork_stats_online_providers_by_country)" {
		t.Errorf("provider map query = %q", target.Expr)
	}
	if !target.Instant || target.Format != "table" {
		t.Error("provider map must be an instant table query so the country code is a lookup field")
	}
	if len(geomap.Options.Layers) != 1 {
		t.Fatalf("provider map has %d layers, want 1", len(geomap.Options.Layers))
	}
	layer := geomap.Options.Layers[0]
	if layer.Type != "markers" || layer.Location.Mode != "lookup" || layer.Location.Lookup != "country_code" || layer.Location.Gazetteer != "public/gazetteer/countries.json" {
		t.Errorf("provider map layer = %+v", layer)
	}
}

func TestExchangeTrafficDashboardsUseLiveIoWithoutDoubleCounting(t *testing.T) {
	public := readTestDashboard(t, "public-traffic.json")
	throughput := dashboardPanelById(public, 3)
	if throughput == nil || len(throughput.Targets) != 1 {
		t.Fatal("public live exchange throughput panel is missing")
	}
	wantPublic := `sum(rate(urnetwork_connect_exchange_io_bytes_total{direction="sent",kind="data",instance!=""}[$__rate_interval])) * 8`
	if throughput.Targets[0].Expr != wantPublic {
		t.Errorf("public live exchange throughput query = %q, want %q", throughput.Targets[0].Expr, wantPublic)
	}

	internal := readTestDashboard(t, "connect.json")
	for _, metric := range []string{
		"urnetwork_connect_exchange_io_bytes_total",
		"urnetwork_connect_exchange_io_frames_total",
		"urnetwork_connect_exchange_active_connections",
	} {
		if !strings.Contains(strings.Join(dashboardExpressions(internal), "\n"), metric) {
			t.Errorf("connect dashboard is missing %s", metric)
		}
	}

	active := dashboardPanelById(internal, 4)
	if active == nil || len(active.Targets) != 2 {
		t.Fatal("current active exchange connection panel is missing")
	}
	for targetIndex, direction := range []string{"outbound", "inbound"} {
		target := active.Targets[targetIndex]
		if !strings.Contains(target.Expr, `direction="`+direction+`"`) {
			t.Errorf("active connection target %d does not select %s: %s", targetIndex, direction, target.Expr)
		}
		if !target.Instant || target.Range == nil || *target.Range {
			t.Errorf("active connection target %d must be an instant query", targetIndex)
		}
	}
}

// registeredApplicationMetrics inventories prometheus option literals in the
// production Go sources. The stats collector creates its gauges through
// small wrappers (newStatsGauge, newStatsGaugeVec), so their string-literal
// call sites are handled explicitly.
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
				if function, ok := call.Fun.(*ast.Ident); ok && (function.Name == "newStatsGauge" || function.Name == "newStatsGaugeVec") && 0 < len(call.Args) {
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
	joined := strings.Join(expressions, "\n")
	for _, metric := range append(slices.Clone(networkMeasurementMetrics), networkLabeledMeasurementMetrics...) {
		if len(metricOccurrences(joined, metric)) == 0 {
			t.Errorf("internal network measurements is missing %s", metric)
		}
		for _, expression := range expressions {
			assertReplicaSafeReads(t, "internal network measurements", expression, metric, `{env="$env"}`)
		}
	}
}
