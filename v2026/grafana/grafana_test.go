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
	Uid         string              `json:"uid"`
	Title       string              `json:"title"`
	Description string              `json:"description"`
	Tags        []string            `json:"tags"`
	Links       []testDashboardLink `json:"links"`
	Templating  struct {
		List []any `json:"list"`
	} `json:"templating"`
	Panels []testPanel `json:"panels"`
}

type testDashboardLink struct {
	Title       string `json:"title"`
	Url         string `json:"url"`
	KeepTime    bool   `json:"keepTime"`
	IncludeVars bool   `json:"includeVars"`
}

type testPanel struct {
	Id          int    `json:"id"`
	Type        string `json:"type"`
	Title       string `json:"title"`
	Description string `json:"description"`
	FieldConfig struct {
		Defaults struct {
			Thresholds struct {
				Steps []struct {
					Color string   `json:"color"`
					Value *float64 `json:"value"`
				} `json:"steps"`
			} `json:"thresholds"`
		} `json:"defaults"`
		Overrides []struct {
			Matcher struct {
				Options string `json:"options"`
			} `json:"matcher"`
			Properties []struct {
				Id    string          `json:"id"`
				Value json.RawMessage `json:"value"`
			} `json:"properties"`
		} `json:"overrides"`
	} `json:"fieldConfig"`
	GridPos struct {
		H int `json:"h"`
		W int `json:"w"`
		X int `json:"x"`
		Y int `json:"y"`
	} `json:"gridPos"`
	Options struct {
		Content       string `json:"content"`
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

func TestEgressProbeDashboardUsesPopulationAwareOutcomeClasses(t *testing.T) {
	dashboard := readTestDashboard(t, "egress-probes.json")
	failures := dashboardPanelById(dashboard, 8)
	if failures == nil || len(failures.Targets) != 1 {
		t.Fatal("egress current-failure panel is missing")
	}
	if failures.Title != "current proved failures" {
		t.Fatalf("egress failure panel title = %q", failures.Title)
	}
	expression := failures.Targets[0].Expr
	for _, class := range []string{
		"tunnel_failed", "contract_failed", "no_consensus", "locate_failed",
		"not_confident", "submit_failed", "unknown_failure",
	} {
		if !strings.Contains(expression, class) {
			t.Errorf("egress failure expression omits %q: %s", class, expression)
		}
	}
	for _, neutral := range []string{"unobserved", "inconsistent", `result!="ok"`} {
		if strings.Contains(expression, neutral) {
			t.Errorf("egress failure expression counts neutral state %q: %s", neutral, expression)
		}
	}
	if strings.Contains(expression, "vector(0)") {
		t.Errorf("egress failure stat hides an absent exporter as zero: %s", expression)
	}
	if !strings.Contains(expression, "fleet_snapshot_timestamp_seconds") ||
		!strings.Contains(expression, "topk(1,") {
		t.Errorf("egress failure stat does not select one fresh fleet snapshot: %s", expression)
	}

	dominant := dashboardPanelById(dashboard, 10)
	share := dashboardPanelById(dashboard, 11)
	fleet := dashboardPanelById(dashboard, 14)
	if dominant == nil || !strings.Contains(dominant.Description, "complete") ||
		!strings.Contains(dominant.Description, "eligible") ||
		share == nil || !strings.Contains(share.Description, "eligible population") ||
		fleet == nil || fleet.Title != "eligible fleet by probe outcome" ||
		!strings.Contains(fleet.Description, "unobserved") {
		t.Fatal("egress dashboard does not explain the reconstructed eligible-population semantics")
	}
	if len(fleet.Targets) != 1 || !strings.Contains(fleet.Targets[0].Expr, "sum by (result)") {
		t.Fatal("egress fleet panel must preserve each outcome in the selected snapshot")
	}
	success := dashboardPanelById(dashboard, 9)
	if success == nil || len(success.Targets) != 1 ||
		!strings.HasPrefix(success.Targets[0].Expr, "sum(") ||
		strings.Contains(success.Targets[0].Expr, "vector(0)") {
		t.Fatal("egress success panel must preserve exporter absence in its selected fleet snapshot")
	}
	for id := 5; id <= 14; id++ {
		panel := dashboardPanelById(dashboard, id)
		if panel == nil || len(panel.Targets) != 1 {
			t.Fatalf("fleet snapshot panel %d is missing", id)
		}
		expression := panel.Targets[0].Expr
		for _, contract := range []string{
			"fleet_snapshot_timestamp_seconds",
			"topk(1,",
			"and on(env,service,block,host,instance)",
			`service="taskworker"`,
			"time() - 900",
			"time() + 30",
		} {
			if !strings.Contains(expression, contract) {
				t.Errorf("fleet snapshot panel %d omits %q: %s", id, contract, expression)
			}
		}
		if strings.Contains(expression, "vector(0)") {
			t.Errorf("fleet snapshot panel %d hides stale or absent telemetry as zero: %s", id, expression)
		}
	}
	if !strings.Contains(dashboard.Description, "go no-data after 15 minutes") ||
		!strings.Contains(dashboard.Description, "§2.23") {
		t.Fatal("egress dashboard does not disclose snapshot freshness and direct-database authority")
	}
}

func TestCompetitionDashboardOperationalSignals(t *testing.T) {
	dashboard := readTestDashboard(t, "competition.json")
	joined := strings.Join(dashboardExpressions(dashboard), "\n")
	for _, metric := range []string{
		"urnetwork_competition_runner_heartbeat_timestamp_seconds",
		"urnetwork_competition_submission_queue_size",
		"urnetwork_competition_current_evaluation_info",
		"urnetwork_competition_current_evaluation_elapsed_seconds",
		"urnetwork_competition_significant_submission_found",
		"urnetwork_competition_evaluation_duration_estimate_seconds",
		"urnetwork_competition_submission_backlog_estimated_seconds",
		"urnetwork_competition_live_evaluation_metric_value",
		"urnetwork_competition_current_round_staging",
	} {
		if !strings.Contains(joined, metric) {
			t.Errorf("competition dashboard is missing %s", metric)
		}
	}

	heartbeat := dashboardPanelById(dashboard, 15)
	if heartbeat == nil || heartbeat.Title != "runner heartbeat age" || len(heartbeat.Targets) != 1 {
		t.Fatal("competition runner heartbeat panel is missing")
	}
	if !strings.Contains(heartbeat.Targets[0].Expr, "time() - max(") {
		t.Errorf("runner heartbeat panel does not calculate heartbeat age: %s", heartbeat.Targets[0].Expr)
	}
	foundWarning := false
	for _, step := range heartbeat.FieldConfig.Defaults.Thresholds.Steps {
		if step.Color == "orange" && step.Value != nil && *step.Value == 30 {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Fatal("runner heartbeat panel must warn at 30 seconds")
	}
	era := dashboardPanelById(dashboard, 26)
	if era == nil || era.Title != "current era" || len(era.Targets) != 1 ||
		!strings.Contains(era.Targets[0].Expr, "urnetwork_competition_current_round_staging") {
		t.Fatal("competition staging-era panel is missing")
	}
	for _, panelId := range []int{22, 23, 24, 25} {
		panel := dashboardPanelById(dashboard, panelId)
		if panel == nil || panel.Type != "bargauge" || len(panel.Targets) != 1 ||
			!strings.Contains(panel.Targets[0].Expr, "urnetwork_competition_live_evaluation_metric_value") {
			t.Errorf("live evaluation plot %d is missing or invalid", panelId)
		}
		colors := map[string]string{}
		if panel != nil {
			for _, override := range panel.FieldConfig.Overrides {
				for _, property := range override.Properties {
					if property.Id == "color" {
						var color struct {
							FixedColor string `json:"fixedColor"`
						}
						if err := json.Unmarshal(property.Value, &color); err != nil {
							t.Fatalf("parse live plot color override: %v", err)
						}
						colors[override.Matcher.Options] = color.FixedColor
					}
				}
			}
		}
		if colors[`.*\[improved\].*`] != "#3987e5" || colors[`.*\[regressed\].*`] != "#e02f44" {
			t.Errorf("live evaluation plot %d does not map improvement blue and regression red: %#v", panelId, colors)
		}
	}
}

func TestBackupArchiveDashboardFailsClosedAfterFiveDays(t *testing.T) {
	dashboard := readTestDashboard(t, "backup-archives.json")
	joined := strings.Join(dashboardExpressions(dashboard), "\n")
	for _, required := range []string{
		"urnetwork_backup_archive_latest_timestamp_seconds",
		"urnetwork_backup_archive_in_progress",
		"urnetwork_backup_archive_storage_bytes",
		"urnetwork_backup_archive_volume_free_bytes",
		`archive="pg"`,
		`archive="redis"`,
		`archive="github-urnetwork"`,
		`archive="github-urfoundation"`,
	} {
		if !strings.Contains(joined, required) {
			t.Errorf("backup archive dashboard is missing %q", required)
		}
	}

	activity := dashboardPanelById(dashboard, 3)
	if activity == nil || activity.Title != "current backup activity" || len(activity.Targets) != 1 {
		t.Fatal("backup archive activity panel is missing")
	}
	if !strings.Contains(activity.Targets[0].Expr, "max by (archive)") ||
		!strings.Contains(activity.Targets[0].Expr, "urnetwork_backup_archive_in_progress") {
		t.Errorf("backup activity does not identify the running archive: %s", activity.Targets[0].Expr)
	}

	archives := map[int]string{
		5: `archive="pg"`,
		6: `archive="redis"`,
		7: `archive="github-urnetwork"`,
		8: `archive="github-urfoundation"`,
	}
	for panelID, archiveSelector := range archives {
		panel := dashboardPanelById(dashboard, panelID)
		if panel == nil || len(panel.Targets) != 1 {
			t.Errorf("backup freshness panel %d is missing", panelID)
			continue
		}
		expression := panel.Targets[0].Expr
		for _, required := range []string{archiveSelector, "time()", "432000", "vector(1)"} {
			if !strings.Contains(expression, required) {
				t.Errorf("backup freshness panel %d is missing %q: %s", panelID, required, expression)
			}
		}
		foundErrorThreshold := false
		for _, step := range panel.FieldConfig.Defaults.Thresholds.Steps {
			if step.Color == "red" && step.Value != nil && *step.Value == 1 {
				foundErrorThreshold = true
			}
		}
		if !foundErrorThreshold {
			t.Errorf("backup freshness panel %d must render ERROR in red", panelID)
		}
	}

	for _, panelID := range []int{9, 10, 11, 12} {
		panel := dashboardPanelById(dashboard, panelID)
		if panel == nil || len(panel.Targets) != 1 ||
			!strings.Contains(panel.Targets[0].Expr, "topk(1") ||
			!strings.Contains(panel.Targets[0].Expr, "max_over_time(") ||
			!strings.Contains(panel.Targets[0].Expr, "[30d]") ||
			!strings.Contains(panel.Targets[0].Expr, "* 1000") {
			t.Errorf("historical archive panel %d is missing or does not select the bounded last-known generation", panelID)
			continue
		}
		if !strings.Contains(strings.ToLower(panel.Title+" "+panel.Description), "historical") ||
			!strings.Contains(strings.ToLower(panel.Description), "stale") ||
			!strings.Contains(strings.ToLower(panel.Description), "fail-closed") {
			t.Errorf("historical archive panel %d does not disclose stale/non-health semantics", panelID)
		}
	}

	telemetryLastSeen := dashboardPanelById(dashboard, 16)
	if telemetryLastSeen == nil || len(telemetryLastSeen.Targets) != 1 {
		t.Fatal("backup host telemetry last-seen panel is missing")
	}
	for _, required := range []string{"node_uname_info", "timestamp(", "max_over_time(", "[30d:]", "* 1000"} {
		if !strings.Contains(telemetryLastSeen.Targets[0].Expr, required) {
			t.Errorf("backup host telemetry last-seen query omits %q: %s", required, telemetryLastSeen.Targets[0].Expr)
		}
	}
	if !strings.Contains(strings.ToLower(telemetryLastSeen.Description), "diagnostic only") ||
		!strings.Contains(strings.ToLower(telemetryLastSeen.Description), "fail-closed") {
		t.Error("backup host telemetry last-seen panel could be mistaken for current health")
	}

	storage := dashboardPanelById(dashboard, 15)
	if storage == nil || storage.Type != "bargauge" || storage.Title != "archive volume storage breakdown" || len(storage.Targets) != 2 {
		t.Fatal("backup archive storage breakdown panel is missing")
	}
	for _, required := range []string{
		"urnetwork_backup_archive_storage_bytes",
		`archive=~"pg|redis|code"`,
		"urnetwork_backup_archive_volume_free_bytes",
	} {
		if !strings.Contains(storage.Targets[0].Expr+"\n"+storage.Targets[1].Expr, required) {
			t.Errorf("backup storage breakdown is missing %q", required)
		}
	}
}

func TestRedisClusterCounterRatesCoverStaggeredScrapes(t *testing.T) {
	dashboard := readTestDashboard(t, "redis-cluster.json")
	wantMetrics := map[int][]string{
		8:  {"redis_commands_duration_seconds_total", "redis_commands_processed_total"},
		9:  {"redis_commands_processed_total"},
		11: {"redis_evicted_keys_total", "redis_expired_keys_total"},
	}

	for panelID, metrics := range wantMetrics {
		panel := dashboardPanelById(dashboard, panelID)
		if panel == nil {
			t.Errorf("Redis counter-rate panel %d is missing", panelID)
			continue
		}
		expressions := make([]string, 0, len(panel.Targets))
		for _, target := range panel.Targets {
			expressions = append(expressions, target.Expr)
		}
		joined := strings.Join(expressions, "\n")
		for _, metric := range metrics {
			if !strings.Contains(joined, "rate("+metric) {
				t.Errorf("Redis panel %d is missing rate for %s: %s", panelID, metric, joined)
			}
		}
		if strings.Contains(joined, "$__rate_interval") {
			t.Errorf("Redis panel %d uses $__rate_interval, which can be shorter than the 61–92 second staggered scrape interval: %s", panelID, joined)
		}
		if strings.Count(joined, "[5m]") != len(metrics) {
			t.Errorf("Redis panel %d must use one five-minute range per counter, got: %s", panelID, joined)
		}
	}
}

type testTarget struct {
	Expr         string `json:"expr"`
	Instant      bool   `json:"instant"`
	Range        *bool  `json:"range"`
	Format       string `json:"format"`
	LegendFormat string `json:"legendFormat"`
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

func TestOnboardingDashboardUsesFreshPrivacySafeEmailTracker(t *testing.T) {
	dashboard := readTestDashboard(t, "onboarding.json")
	if dashboard.Title != "urnetwork / onboarding" {
		t.Fatalf("onboarding dashboard title = %q", dashboard.Title)
	}
	raw, err := dashboardsFs.ReadFile("dashboards/onboarding.json")
	if err != nil {
		t.Fatal(err)
	}
	document := string(raw)
	for _, required := range []string{
		"urnetwork_onboarding_email_tracker_networks",
		"urnetwork_onboarding_email_tracker_snapshot_timestamp_seconds",
		"/admin/onboarding/email-tracker",
		"attribution_ambiguous",
		"landing_clicked", "app_opened", "connected", "widget_added",
		"feedback_submitted", "pro_started", "bounced", "unsubscribed", "complained",
	} {
		if !strings.Contains(document, required) {
			t.Errorf("onboarding dashboard lacks %q", required)
		}
	}
	for _, expression := range dashboardExpressions(dashboard) {
		if !strings.Contains(expression, "urnetwork_onboarding_email_tracker_networks") {
			continue
		}
		if !strings.Contains(expression, "topk(1,") ||
			!strings.Contains(expression, "email_tracker_snapshot_timestamp_seconds") ||
			!strings.Contains(expression, "time() - 1200") {
			t.Errorf("email tracker query does not select one fresh taskworker snapshot: %s", expression)
		}
		for _, forbidden := range []string{"network_id", "message_id", "user_auth", "vector(0)"} {
			if strings.Contains(expression, forbidden) {
				t.Errorf("email tracker query contains forbidden %q: %s", forbidden, expression)
			}
		}
	}
}

func TestSubscriptionsDashboardUsesFreshPrivacySafeLedgerSnapshot(t *testing.T) {
	dashboard := readTestDashboard(t, "subscriptions.json")
	if dashboard.Uid != "urnetwork-subscriptions" || dashboard.Title != "urnetwork / subscriptions" {
		t.Fatalf("subscriptions dashboard identity = %q / %q", dashboard.Uid, dashboard.Title)
	}
	if slices.Contains(dashboard.Tags, PublicTag) {
		t.Fatal("subscriptions dashboard must remain authenticated")
	}

	documentBytes, err := dashboardsFs.ReadFile("dashboards/subscriptions.json")
	if err != nil {
		t.Fatal(err)
	}
	document := string(documentBytes)
	for _, required := range []string{
		`"query": "24h,7d,30d"`,
		"upgrades / new paid accounts",
		"current paid accounts by store",
		"paid subscriber engagement by store",
		"observed churn: expiry or cancellation",
		"provider-confirmed terminal events",
		"reconciliation repairs",
		"data packs fulfilled",
		"data-pack bytes fulfilled",
		"Raw cancellation intent is not durably available",
		"never renders an invented cancellation-intent zero",
	} {
		if !strings.Contains(document, required) {
			t.Errorf("subscriptions dashboard lacks %q", required)
		}
	}
	for _, metric := range []string{
		"urnetwork_subscription_active_accounts",
		"urnetwork_subscription_new_paid_accounts",
		"urnetwork_subscription_engaged_accounts",
		"urnetwork_subscription_churned_accounts",
		"urnetwork_subscription_reconciliation_events",
		"urnetwork_subscription_reconciliation_source_timestamp_seconds",
		"urnetwork_subscription_data_pack_fulfillments",
		"urnetwork_subscription_data_pack_bytes",
		"urnetwork_subscription_snapshot_timestamp_seconds",
	} {
		if !strings.Contains(document, metric) {
			t.Errorf("subscriptions dashboard does not cover %s", metric)
		}
	}
	if !strings.Contains(document, "Google Play / Android") {
		t.Error("subscriptions dashboard does not name the google series as Google Play / Android")
	}

	expressions := dashboardExpressions(dashboard)
	if len(expressions) == 0 {
		t.Fatal("subscriptions dashboard has no Prometheus queries")
	}
	for _, expression := range expressions {
		for _, contract := range []string{
			"urnetwork_subscription_snapshot_timestamp_seconds",
			"topk(1,",
			`service="taskworker"`,
			"time() - 1200",
			"time() + 30",
		} {
			if !strings.Contains(expression, contract) {
				t.Errorf("subscription query omits fresh single-snapshot contract %q: %s", contract, expression)
			}
		}
		for _, forbidden := range []string{
			"vector(0)", "network_id", "user_id", "client_id", "purchase_event_id",
			"transaction_id", "invoice", "purchase_token", "email", "users_24h",
		} {
			if strings.Contains(expression, forbidden) {
				t.Errorf("subscription query contains forbidden %q: %s", forbidden, expression)
			}
		}
	}

	// Every business series is joined to the exact identity labels selected by
	// the fleet-wide snapshot. Snapshot age itself starts from that selector.
	for _, expression := range expressions {
		withoutSnapshot := strings.ReplaceAll(expression, "urnetwork_subscription_snapshot_timestamp_seconds", "")
		if strings.Contains(withoutSnapshot, "urnetwork_subscription_") &&
			!strings.Contains(expression, "and on(env,service,block,host,instance)") {
			t.Errorf("subscription query is not tied to one publisher identity: %s", expression)
		}
	}

	active := dashboardPanelById(dashboard, 8)
	engagement := dashboardPanelById(dashboard, 9)
	upgrades := dashboardPanelById(dashboard, 21)
	churn := dashboardPanelById(dashboard, 11)
	if active == nil || len(active.Targets) != 1 ||
		!strings.Contains(active.Targets[0].Expr, `store=~"apple|google|stripe|solana"`) {
		t.Fatal("subscriptions dashboard lacks the bounded four-store active view")
	}
	if upgrades == nil || len(upgrades.Targets) != 1 ||
		!strings.Contains(upgrades.Targets[0].Expr, `store=~"apple|google|stripe|solana"`) ||
		!strings.Contains(upgrades.Targets[0].Expr, `window="$window"`) {
		t.Fatal("subscriptions dashboard lacks the bounded four-store upgrade view")
	}
	upgradeTotal := dashboardPanelById(dashboard, 6)
	if upgradeTotal == nil || len(upgradeTotal.Targets) != 1 ||
		!strings.Contains(upgradeTotal.Targets[0].Expr, `store="deduplicated"`) {
		t.Fatal("subscriptions dashboard lacks the first-ever deduplicated upgrade total")
	}
	for _, panel := range []*testPanel{engagement, churn} {
		if panel == nil || len(panel.Targets) != 1 ||
			!strings.Contains(panel.Targets[0].Expr, `store=~"apple|google|stripe|solana|deduplicated"`) ||
			!strings.Contains(panel.Targets[0].Expr, `window="$window"`) {
			t.Fatal("subscriptions dashboard lacks a bounded store/window account view")
		}
	}

	terminal := dashboardPanelById(dashboard, 13)
	repairs := dashboardPanelById(dashboard, 14)
	for _, panel := range []*testPanel{terminal, repairs} {
		if panel == nil || len(panel.Targets) != 1 {
			t.Fatal("subscriptions reconciliation panel is missing")
		}
		expression := panel.Targets[0].Expr
		for _, required := range []string{
			`store="all"`,
			`store=~"apple|google|stripe|solana"`,
			"time() - 9000",
			"time() - 10800",
			"and on(env,service,block,host,instance,store)",
		} {
			if !strings.Contains(expression, required) {
				t.Errorf("reconciliation panel %q omits source gate %q: %s", panel.Title, required, expression)
			}
		}
	}
	if !strings.Contains(terminal.Targets[0].Expr, `action=~"ended|refunded|disputed|revoked"`) {
		t.Errorf("terminal event panel has an unbounded or incomplete action set: %s", terminal.Targets[0].Expr)
	}
	if !strings.Contains(repairs.Targets[0].Expr, `action=~"credited|ended|entitlement_repaired"`) {
		t.Errorf("repair panel has an unbounded or incomplete action set: %s", repairs.Targets[0].Expr)
	}

	for _, panelID := range []int{17, 18} {
		panel := dashboardPanelById(dashboard, panelID)
		if panel == nil || len(panel.Targets) != 1 ||
			!strings.Contains(panel.Targets[0].Expr, `source=~"balance_code|direct_balance|deduplicated"`) ||
			!strings.Contains(panel.Targets[0].Expr, `window="$window"`) {
			t.Errorf("data-pack panel %d lacks the bounded deduplicated source/window contract", panelID)
		}
	}

	cancellation := dashboardPanelById(dashboard, 12)
	if cancellation == nil || cancellation.Type != "text" ||
		!strings.Contains(cancellation.Options.Content, "not durably available") ||
		!strings.Contains(cancellation.Options.Content, "never renders") {
		t.Fatal("subscriptions dashboard must explicitly disclose unavailable cancellation-intent attribution")
	}
}

func TestProxyDashboardCoversBoundedServiceTrafficAndCapacity(t *testing.T) {
	dashboard := readTestDashboard(t, "proxy.json")
	if dashboard.Uid != "urnetwork-proxy" || dashboard.Title != "urnetwork / proxy" {
		t.Fatalf("proxy dashboard identity = %q / %q", dashboard.Uid, dashboard.Title)
	}
	if slices.Contains(dashboard.Tags, PublicTag) {
		t.Fatal("proxy dashboard must remain authenticated")
	}

	documentBytes, err := dashboardsFs.ReadFile("dashboards/proxy.json")
	if err != nil {
		t.Fatal(err)
	}
	document := string(documentBytes)
	for _, required := range []string{
		"client-boundary relay bytes / s",
		"admissions by protocol and outcome",
		"bounded HTTP / SOCKS library events / s",
		"WireGuard packets / s by direction and outcome",
		"WireGuard queue drops and receiver failures / s",
		"aggregate devices, peers, prewarm, and lifecycle",
		"platform transport byte capacity",
		"WireGuard return backpressure",
		"caller-lock cache occupancy",
		"control and device-RPC HTTP outcomes",
		"process CPU, memory, goroutines, and file descriptors",
		"Missing or stale telemetry remains no-data",
	} {
		if !strings.Contains(document, required) {
			t.Errorf("proxy dashboard lacks %q", required)
		}
	}
	for _, metric := range []string{
		"urnetwork_proxy_ready",
		"urnetwork_proxy_drain_active_remaining",
		"urnetwork_proxy_ingress_admissions_total",
		"urnetwork_proxy_sessions_total",
		"urnetwork_proxy_session_duration_seconds",
		"urnetwork_proxy_sessions_active",
		"urnetwork_proxy_session_bytes_total",
		"urnetwork_proxy_ingress_events_total",
		"urnetwork_proxy_ingress_active",
		"urnetwork_proxy_session_interval_max_seconds",
		"urnetwork_proxy_session_interval_max_timestamp_seconds",
		"urnetwork_proxy_wireguard_packets_total",
		"urnetwork_proxy_wireguard_bytes_total",
		"urnetwork_proxy_devices_live",
		"urnetwork_proxy_prewarmed_devices",
		"urnetwork_proxy_wg_peers",
		"urnetwork_proxy_device_memory_tracked_used_bytes",
		"urnetwork_proxy_platform_transports_pending_h1",
		"urnetwork_proxy_wireguard_return_backpressure_total",
		"urnetwork_proxy_lock_cache_entries",
		"urnetwork_http_requests_total",
		"urnetwork_pg_pool_connections",
		"process_resident_memory_bytes",
	} {
		if !strings.Contains(document, metric) {
			t.Errorf("proxy dashboard does not cover %s", metric)
		}
	}

	expressions := dashboardExpressions(dashboard)
	if len(expressions) == 0 {
		t.Fatal("proxy dashboard has no Prometheus queries")
	}
	for _, expression := range expressions {
		for _, required := range []string{
			`env="$env"`, `service="proxy"`, `block=~"$block"`,
			`host=~"$host"`, `instance!=""`,
		} {
			if !strings.Contains(expression, required) {
				t.Errorf("proxy query omits scope %q: %s", required, expression)
			}
		}
		for _, forbidden := range []string{
			"vector(0)", "proxy_id", "client_id", "device_id", "user_id",
			"remote_addr", "destination=", "credential=", "error_text",
		} {
			if strings.Contains(expression, forbidden) {
				t.Errorf("proxy query contains forbidden %q: %s", forbidden, expression)
			}
		}
	}

	maximum := dashboardPanelById(dashboard, 9)
	if maximum == nil || len(maximum.Targets) != 1 {
		t.Fatal("proxy fresh maximum panel is missing")
	}
	for _, required := range []string{
		"urnetwork_proxy_session_interval_max_timestamp_seconds",
		"and on (env, service, block, host, instance, protocol)",
		"time() - 120", "time() + 30",
	} {
		if !strings.Contains(maximum.Targets[0].Expr, required) {
			t.Errorf("proxy maximum omits freshness contract %q: %s", required, maximum.Targets[0].Expr)
		}
	}
	directional := dashboardPanelById(dashboard, 12)
	if directional == nil || len(directional.Targets) != 1 ||
		!strings.Contains(directional.Targets[0].Expr, "sum by (direction, outcome)") {
		t.Fatal("proxy dashboard does not preserve WireGuard direction and outcome")
	}
	rpc := dashboardPanelById(dashboard, 22)
	if rpc == nil || len(rpc.Targets) != 1 ||
		!strings.Contains(rpc.Targets[0].Expr, "sum by (route, status, outcome)") {
		t.Fatal("proxy dashboard does not preserve bounded control/RPC route outcomes")
	}
}

func TestMcpDashboardCoversBoundedCallsFetchAndCapacity(t *testing.T) {
	dashboard := readTestDashboard(t, "mcp.json")
	if dashboard.Uid != "urnetwork-mcp" || dashboard.Title != "urnetwork / mcp" {
		t.Fatalf("MCP dashboard identity = %q / %q", dashboard.Uid, dashboard.Title)
	}
	if slices.Contains(dashboard.Tags, PublicTag) {
		t.Fatal("MCP dashboard must remain authenticated")
	}

	documentBytes, err := dashboardsFs.ReadFile("dashboards/mcp.json")
	if err != nil {
		t.Fatal(err)
	}
	document := string(documentBytes)
	for _, required := range []string{
		"protocol calls by method, tool, and outcome",
		"HTTP authentication and transport outcomes",
		"tool input and output bytes / s",
		"providerLocations output / s",
		"fetch resource outputs / s",
		"active authenticated callers (process-local)",
		"fetch concurrency and capacity",
		"fetch gate waiters and admission outcomes",
		"fetch result, HTTP status, and continuation classes",
		"PostgreSQL acquire and lifecycle pressure",
		"HTTP drain outcomes",
		"Missing or stale telemetry remains no-data",
	} {
		if !strings.Contains(document, required) {
			t.Errorf("MCP dashboard lacks %q", required)
		}
	}
	for _, metric := range []string{
		"urnetwork_mcp_ready",
		"urnetwork_mcp_calls_total",
		"urnetwork_mcp_call_duration_seconds",
		"urnetwork_mcp_calls_inflight",
		"urnetwork_mcp_call_interval_max_seconds",
		"urnetwork_mcp_call_interval_max_timestamp_seconds",
		"urnetwork_mcp_tool_bytes_total",
		"urnetwork_mcp_tool_items_total",
		"urnetwork_mcp_active_callers",
		"urnetwork_mcp_fetch_concurrency",
		"urnetwork_mcp_fetch_waiters",
		"urnetwork_mcp_fetch_wait_duration_seconds",
		"urnetwork_mcp_fetch_admissions_total",
		"urnetwork_mcp_fetch_results_total",
		"urnetwork_http_requests_total",
		"urnetwork_http_request_bytes_total",
		"urnetwork_http_response_bytes_total",
		"urnetwork_http_request_duration_seconds",
		"urnetwork_http_server_draining",
		"urnetwork_pg_pool_connections",
		"process_resident_memory_bytes",
	} {
		if !strings.Contains(document, metric) {
			t.Errorf("MCP dashboard does not cover %s", metric)
		}
	}

	expressions := dashboardExpressions(dashboard)
	if len(expressions) == 0 {
		t.Fatal("MCP dashboard has no Prometheus queries")
	}
	for _, expression := range expressions {
		for _, required := range []string{
			`env="$env"`, `service="mcp"`, `block=~"$block"`,
			`host=~"$host"`, `instance!=""`,
		} {
			if !strings.Contains(expression, required) {
				t.Errorf("MCP query omits scope %q: %s", required, expression)
			}
		}
		for _, forbidden := range []string{
			"vector(0)", "user_id", "client_id", "network_id", "proxy_id",
			"remote_addr", "url=", "identity=", "error_text", "error_message",
		} {
			if strings.Contains(expression, forbidden) {
				t.Errorf("MCP query contains forbidden %q: %s", forbidden, expression)
			}
		}
	}

	maximum := dashboardPanelById(dashboard, 11)
	if maximum == nil || len(maximum.Targets) != 1 {
		t.Fatal("MCP fresh maximum panel is missing")
	}
	for _, required := range []string{
		"urnetwork_mcp_call_interval_max_timestamp_seconds",
		"and on (env, service, block, host, instance, method, tool)",
		"time() - 120", "time() + 30",
	} {
		if !strings.Contains(maximum.Targets[0].Expr, required) {
			t.Errorf("MCP maximum omits freshness contract %q: %s", required, maximum.Targets[0].Expr)
		}
	}
	callers := dashboardPanelById(dashboard, 15)
	if callers == nil || len(callers.Targets) != 1 ||
		!strings.Contains(callers.Targets[0].Expr, "max by (window)") ||
		strings.Contains(callers.Targets[0].Expr, "sum by (window)") {
		t.Fatal("MCP dashboard invents a fleet-distinct caller count")
	}
	concurrency := dashboardPanelById(dashboard, 16)
	if concurrency == nil || len(concurrency.Targets) != 1 ||
		!strings.Contains(concurrency.Targets[0].Expr, `scope=~"global_active|global_capacity|per_identity_active|per_identity_capacity|active_identities"`) {
		t.Fatal("MCP dashboard lacks the bounded fetch concurrency scope set")
	}
	fetch := dashboardPanelById(dashboard, 19)
	if fetch == nil || len(fetch.Targets) != 1 {
		t.Fatal("MCP fetch-result panel is missing")
	}
	for _, dimension := range []string{"outcome", "status_class", "truncated", "continuation", "payment"} {
		if !strings.Contains(fetch.Targets[0].Expr, dimension) {
			t.Errorf("MCP fetch-result panel omits %s: %s", dimension, fetch.Targets[0].Expr)
		}
	}
}

func TestWebAnalyticsDashboardPrivacyContract(t *testing.T) {
	dashboard := readTestDashboard(t, "web-analytics.json")
	if slices.Contains(dashboard.Tags, PublicTag) {
		t.Fatal("web analytics contains search terms and must remain an authenticated dashboard")
	}
	joined := strings.Join(dashboardExpressions(dashboard), "\n")
	for _, metric := range []string{
		"urnetwork_web_search_clicks_total",
		"urnetwork_web_search_impressions_total",
		"urnetwork_web_search_ingest_rows_total",
		"urnetwork_web_search_ingest_last_success_timestamp_seconds",
	} {
		if !strings.Contains(joined, metric) {
			t.Errorf("web analytics is missing %s", metric)
		}
	}
	for _, panelID := range []int{2, 3, 4, 6, 7, 8, 9} {
		panel := dashboardPanelById(dashboard, panelID)
		if panel == nil || len(panel.Targets) != 1 {
			t.Fatalf("web analytics page-view panel %d is missing", panelID)
		}
		expression := panel.Targets[0].Expr
		for _, required := range []string{
			`service="web"`,
			`event="web_page_view"`,
			`privacy_safe="true"`,
			"count_over_time",
		} {
			if !strings.Contains(expression, required) {
				t.Errorf("page-view panel %d lacks %s: %s", panelID, required, expression)
			}
		}
	}
	privateLabel := regexp.MustCompile(`(?i)\b(ip|client_ip|remote_addr|user_id|cookie|full_referrer)\b`)
	if privateLabel.MatchString(joined) {
		t.Errorf("web analytics query references a forbidden user-level field: %s", privateLabel.FindString(joined))
	}
	searchTerms := dashboardPanelById(dashboard, 15)
	if searchTerms == nil || searchTerms.Type != "logs" || len(searchTerms.Targets) != 1 {
		t.Fatal("web analytics privacy-filtered search terms panel is missing")
	}
	termQuery := searchTerms.Targets[0].Expr
	for _, required := range []string{
		`service="taskworker"`,
		`event="web_search_query"`,
		`privacy_safe="true"`,
		`{{.query}}`,
	} {
		if !strings.Contains(termQuery, required) {
			t.Errorf("search terms query lacks %s: %s", required, termQuery)
		}
	}
	// Query text is intentionally a parsed Loki field, never a persistent
	// Prometheus label on a high-cardinality metric.
	for _, expression := range dashboardExpressions(dashboard) {
		if strings.Contains(expression, "urnetwork_web_") && strings.Contains(expression, `query=`) {
			t.Errorf("raw search query used as a metric label: %s", expression)
		}
	}
}

func TestServiceLogsLinksToLogsDrilldown(t *testing.T) {
	dashboard := readTestDashboard(t, "service-logs.json")
	for _, link := range dashboard.Links {
		if link.Title != "Logs Drilldown" {
			continue
		}
		if link.Url != "/a/grafana-lokiexplore-app/explore?var-ds=warp-loki" {
			t.Fatalf("Logs Drilldown URL = %q", link.Url)
		}
		if !link.KeepTime {
			t.Fatal("Logs Drilldown link must preserve the dashboard time range")
		}
		if link.IncludeVars {
			t.Fatal("service dashboard variables are not Logs Drilldown variables")
		}
		return
	}
	t.Fatal("service logs dashboard is missing its Logs Drilldown link")
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
		`max by (host,chain,job)`,
		`{{job}}`,
	} {
		if !strings.Contains(content, required) {
			t.Errorf("Subtensor dashboard does not separate both node jobs: missing %s", required)
		}
	}
}

func TestSubtensorDashboardDistinguishesRealLagFromStaleExport(t *testing.T) {
	dashboard := readTestDashboard(t, "subtensor.json")
	queries := strings.Join(dashboardExpressions(dashboard), "\n")
	for _, required := range []string{
		`status="sync_target"`,
		`status="best"`,
		`deriv(substrate_block_height`,
		`[1h]`,
		`time() - max by (host,chain,job) (timestamp(`,
		`max by (host,chain)`,
		`[1h:15s]`,
		` >= time() - 90`,
		` >= 200`,
	} {
		if !strings.Contains(queries, required) {
			t.Errorf("Subtensor dashboard cannot disambiguate lag/export freshness: missing %s", required)
		}
	}
	for _, id := range []int{13, 14} {
		panel := dashboardPanelById(dashboard, id)
		if panel == nil || len(panel.Targets) != 1 {
			t.Fatalf("missing target panel %d", id)
		}
		query := panel.Targets[0].Expr
		for _, required := range []string{`job=~"^(?:subtensor|subtensor-lightnode)$"`, `job=~"$node"`, `substrate_sub_libp2p_is_major_syncing`, `on (host,chain)`} {
			if !strings.Contains(query, required) {
				t.Errorf("target panel %d missing %s", id, required)
			}
		}
		if strings.Contains(query, "max by (job)") || strings.Contains(query, "max by (host)") {
			t.Errorf("target panel %d merges host or chain identity", id)
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

func TestAdmissionCacheAndSourcePanelsUseActionableQueries(t *testing.T) {
	signalExpressions := dashboardExpressions(readTestDashboard(t, "signals.json"))
	for _, expression := range []string{
		`sum(rate(urnetwork_circle_transfer_admissions_total{env="$env"}[$__rate_interval]))`,
		`sum(rate(urnetwork_circle_transfer_deferrals_total{env="$env"}[$__rate_interval]))`,
		`sum(rate(urnetwork_circle_transfer_admission_errors_total{env="$env"}[$__rate_interval]))`,
		`histogram_quantile(0.50, sum by (le) (rate(urnetwork_circle_transfer_admission_wait_seconds_bucket{env="$env"}[$__rate_interval])))`,
		`histogram_quantile(0.95, sum by (le) (rate(urnetwork_circle_transfer_admission_wait_seconds_bucket{env="$env"}[$__rate_interval])))`,
		`sum(rate(urnetwork_circle_transfer_admission_wait_seconds_sum{env="$env"}[$__rate_interval])) / sum(rate(urnetwork_circle_transfer_admission_wait_seconds_count{env="$env"}[$__rate_interval]))`,
		`max by (host, block, instance) (urnetwork_proxy_lock_cache_entries{env="$env"})`,
		`max by (host, block, instance) (urnetwork_proxy_lock_cache_capacity{env="$env"})`,
		`sum(rate(urnetwork_proxy_lock_cache_hits_total{env="$env"}[$__rate_interval]))`,
		`sum(rate(urnetwork_proxy_lock_cache_misses_total{env="$env"}[$__rate_interval]))`,
		`sum(rate(urnetwork_proxy_lock_cache_expirations_total{env="$env"}[$__rate_interval]))`,
		`sum(rate(urnetwork_proxy_lock_cache_evictions_total{env="$env"}[$__rate_interval]))`,
		`max by (host, block) (urnetwork_proxy_lifecycle_join_enabled{env="$env"})`,
	} {
		if !slices.Contains(signalExpressions, expression) {
			t.Errorf("signals dashboard is missing actionable query %q", expression)
		}
	}

	sourcePanel := dashboardPanelById(readTestDashboard(t, "services-overview.json"), 6)
	if sourcePanel == nil || sourcePanel.Type != "table" || len(sourcePanel.Targets) != 1 {
		t.Fatal("running source revisions table is missing")
	}
	wantSourceQuery := `urnetwork_source_info{env="$env",service=~"$service",block=~"$block",host=~"$host"}`
	sourceTarget := sourcePanel.Targets[0]
	if sourceTarget.Expr != wantSourceQuery || sourceTarget.Format != "table" || !sourceTarget.Instant {
		t.Errorf("running source revisions query = %+v, want instant table %q", sourceTarget, wantSourceQuery)
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
			if strings.HasPrefix(
				filepath.ToSlash(path),
				"../connect/sim-latency/eval-",
			) {
				return filepath.SkipDir
			}
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "vendor" || strings.HasPrefix(
				filepath.ToSlash(path),
				"../connect/sim-latency/eval-",
			) {
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

// The lossless failure total identifies the alerting cause, while this bounded
// breakdown distinguishes request shapes without exposing client identifiers.
func TestMissingOriginDetailsHaveActionableDashboardQuery(t *testing.T) {
	dashboard := readTestDashboard(t, "signals.json")
	wantTitle := "\u00a74 contract failures + origin/destination details / min (lossless)"
	var detailsTarget *testTarget
	for panelIndex := range dashboard.Panels {
		panel := &dashboard.Panels[panelIndex]
		if panel.Title != wantTitle {
			continue
		}
		for targetIndex := range panel.Targets {
			target := &panel.Targets[targetIndex]
			if strings.Contains(target.Expr, "urnetwork_connect_missing_origin_details_total") {
				detailsTarget = target
				break
			}
		}
		break
	}
	if detailsTarget == nil {
		t.Fatalf("signals dashboard panel %q lacks the missing-origin detail query", wantTitle)
	}

	wantQuery := `sum by (request_companion, resolution, relationship, source_lifecycle, destination_lifecycle) (rate(urnetwork_connect_missing_origin_details_total{env="$env",instance!=""}[$__rate_interval])) * 60`
	if detailsTarget.Expr != wantQuery {
		t.Errorf("missing-origin detail query = %q, want %q", detailsTarget.Expr, wantQuery)
	}
	wantLegend := "missing origin request_companion={{request_companion}} resolution={{resolution}} relationship={{relationship}} source={{source_lifecycle}} destination={{destination_lifecycle}}"
	if detailsTarget.LegendFormat != wantLegend {
		t.Errorf("missing-origin detail legend = %q, want %q", detailsTarget.LegendFormat, wantLegend)
	}
}

// Keep the lossless aggregate beside its bounded diagnostic breakdown. Rate
// each process counter before summing, retain every finite causal dimension,
// and never turn missing detail into zero or expose raw endpoint identifiers.
func TestInactiveDestinationDetailsHaveActionableDashboardQuery(t *testing.T) {
	dashboard := readTestDashboard(t, "signals.json")
	wantTitle := "\u00a74 contract failures + origin/destination details / min (lossless)"
	var targets []testTarget
	for _, panel := range dashboard.Panels {
		if panel.Title == wantTitle {
			targets = panel.Targets
			break
		}
	}
	if targets == nil {
		t.Fatalf("signals dashboard lacks panel %q", wantTitle)
	}
	tests := []struct {
		metric string
		query  string
		legend string
	}{
		{
			metric: "urnetwork_connect_contract_failures_total",
			query:  `sum by (cause, companion) (rate(urnetwork_connect_contract_failures_total{env="$env",instance!=""}[$__rate_interval])) * 60`,
			legend: "{{cause}} companion={{companion}}",
		},
		{
			metric: "urnetwork_connect_inactive_destination_details_total",
			query:  `sum by (request_companion, sender_role, resolution, relationship, source_lifecycle, destination_lifecycle) (rate(urnetwork_connect_inactive_destination_details_total{env="$env",instance!=""}[$__rate_interval])) * 60`,
			legend: "inactive destination request_companion={{request_companion}} sender_role={{sender_role}} resolution={{resolution}} relationship={{relationship}} source={{source_lifecycle}} destination={{destination_lifecycle}}",
		},
	}
	for _, test := range tests {
		matches := 0
		for _, target := range targets {
			if !strings.Contains(target.Expr, test.metric) {
				continue
			}
			matches++
			if target.Expr != test.query {
				t.Errorf("%s query = %q, want %q", test.metric, target.Expr, test.query)
			}
			if target.LegendFormat != test.legend {
				t.Errorf("%s legend = %q, want %q", test.metric, target.LegendFormat, test.legend)
			}
		}
		if matches != 1 {
			t.Errorf("%s has %d panel queries, want exactly one", test.metric, matches)
		}
	}
}

func TestProxyWireGuardRuntimeFailuresHaveActionableDashboardQueries(t *testing.T) {
	expressions := dashboardExpressions(readTestDashboard(t, "signals.json"))
	tests := []struct {
		metric   string
		required []string
	}{
		{
			metric: "urnetwork_proxy_wg_inbound_peer_queue_drop_packets",
			required: []string{
				"rate(",
				`{env="$env"}`,
				"[$__rate_interval]",
			},
		},
		{
			metric: "urnetwork_proxy_wg_inbound_decryption_queue_drop_packets",
			required: []string{
				"rate(",
				`{env="$env"}`,
				"[$__rate_interval]",
			},
		},
		{
			metric: "urnetwork_proxy_wg_receive_routine_failures",
			required: []string{
				"max_over_time(",
				`{env="$env"}`,
				"[$__rate_interval]",
			},
		},
	}
	for _, test := range tests {
		var query string
		for _, expression := range expressions {
			if strings.Contains(expression, test.metric) {
				query = expression
				break
			}
		}
		if query == "" {
			t.Errorf("signals dashboard is missing %s", test.metric)
			continue
		}
		for _, required := range test.required {
			if !strings.Contains(query, required) {
				t.Errorf("%s query is missing %q: %s", test.metric, required, query)
			}
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
