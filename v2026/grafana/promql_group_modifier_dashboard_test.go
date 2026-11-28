package grafana

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// Every expression string anywhere in a dashboard document, including nested
// row panels and templating queries.
func dashboardDocumentExpressions(t *testing.T, name string) []string {
	t.Helper()
	body, err := dashboardsFs.ReadFile("dashboards/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var document any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	expressions := []string{}
	var walk func(node any)
	walk = func(node any) {
		switch value := node.(type) {
		case map[string]any:
			for key, child := range value {
				if expression, ok := child.(string); ok && key == "expr" {
					expressions = append(expressions, expression)
				} else {
					walk(child)
				}
			}
		case []any:
			for _, child := range value {
				walk(child)
			}
		}
	}
	walk(document)
	return expressions
}

func promqlIdentifierByte(c byte) bool {
	return c == '_' || c == ':' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
}

func promqlLabelName(label string) bool {
	if strings.HasPrefix(label, `"`) && strings.HasSuffix(label, `"`) && 2 <= len(label) {
		return true
	}
	if label == "" || '0' <= label[0] && label[0] <= '9' {
		return false
	}
	for i := 0; i < len(label); i++ {
		if label[i] == ':' || !promqlIdentifierByte(label[i]) {
			return false
		}
	}
	return true
}

// group_left and group_right take an optional label list, so a parenthesis
// right after the modifier always opens that list. An operand written there,
// as in "group_left ((up > 0))", is a PromQL parse error, not a grouping.
func promqlGroupModifierError(expression string) string {
	for _, modifier := range []string{"group_left", "group_right"} {
		for offset := 0; offset < len(expression); {
			index := strings.Index(expression[offset:], modifier)
			if index < 0 {
				break
			}
			start := offset + index
			offset = start + len(modifier)
			if 0 < start && promqlIdentifierByte(expression[start-1]) ||
				offset < len(expression) && promqlIdentifierByte(expression[offset]) {
				continue
			}
			rest := strings.TrimLeft(expression[offset:], " \t\r\n")
			if !strings.HasPrefix(rest, "(") {
				continue
			}
			end := strings.IndexByte(rest, ')')
			if end < 0 {
				return modifier + " opens an unterminated label list"
			}
			for _, label := range strings.Split(rest[1:end], ",") {
				if label = strings.TrimSpace(label); label != "" && !promqlLabelName(label) {
					return fmt.Sprintf("%s takes a label list, not the operand %.60q", modifier, rest)
				}
			}
		}
	}
	return ""
}

func TestPromqlGroupModifierErrorMatchesParser(t *testing.T) {
	for _, valid := range []string{
		`a - on (host) group_left () (b)`,
		`a - on (host,chain) group_right () max by (host) (b)`,
		`a >= on(instance) group_left timestamp(b)`,
		`a * on(instance) group_left(version, "service.name") b`,
		`a * on(instance) group_left(version,) b`,
		`my_group_left_total + group_rightmost`,
	} {
		if problem := promqlGroupModifierError(valid); problem != "" {
			t.Errorf("%q rejected: %s", valid, problem)
		}
	}
	// Mimir's parser rejects each of these with "unexpected ( in grouping opts".
	for _, invalid := range []string{
		`a >= on(instance) group_left ((((b > 0))))`,
		`a >= on(instance) group_left (b > 0)`,
		`a >= on(instance) group_right(b{c="d"})`,
	} {
		if promqlGroupModifierError(invalid) == "" {
			t.Errorf("%q accepted", invalid)
		}
	}
}

// The URL quota panels once passed an operand where the label list belongs,
// so Mimir refused all six queries and the coverage panels showed only errors.
func TestDashboardGroupModifiersTakeLabelLists(t *testing.T) {
	entries, err := dashboardsFs.ReadDir("dashboards")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		for _, expression := range dashboardDocumentExpressions(t, entry.Name()) {
			if problem := promqlGroupModifierError(expression); problem != "" {
				t.Errorf("%s: %s", entry.Name(), problem)
			}
		}
	}
	dashboard := readTestDashboard(t, "egress-probes.json")
	for _, id := range []int{74, 75, 76, 77, 78, 79, 92} {
		panel := dashboardPanelById(dashboard, id)
		if panel == nil || len(panel.Targets) != 1 || promqlGroupModifierError(panel.Targets[0].Expr) != "" {
			t.Errorf("URL quota panel %d is missing or does not parse", id)
		}
	}
}

// The deficit diagnostic once took the maximum of each cell across every
// process, including replicas still exporting an old census generation.
func TestUrlProbeMatureDeficitDashboardUsesCurrentAtomicCensus(t *testing.T) {
	dashboard := readTestDashboard(t, "egress-probes.json")
	panel := dashboardPanelById(dashboard, 92)
	if panel == nil || len(panel.Targets) != 1 || panel.Targets[0].LegendFormat != "{{state}}" {
		t.Fatal("missing mature URL deficit diagnostic panel")
	}
	query := panel.Targets[0].Expr
	for _, guard := range []string{
		`urnetwork_url_probe_mature_deficit_diagnostic{env="$env",job="taskworker",instance!=""}`,
		"urnetwork_url_probe_fleet_observed_timestamp_seconds",
		"urnetwork_url_probe_shard_observed_timestamp_seconds", `shard="0"`,
		"process_start_time_seconds", "count by (env,job,host,block)",
		"and on() (count(", "time() - 180", "time() + 30", "group_left timestamp(",
		"urnetwork_url_probe_admission_cohort_contract",
		`timestamp(urnetwork_url_probe_mature_deficit_diagnostic_contract{env="$env",job="taskworker",instance!=""}) == on(env,job,host,block,instance) timestamp(urnetwork_url_probe_fleet_observed_timestamp_seconds`,
	} {
		if !strings.Contains(query, guard) {
			t.Errorf("deficit diagnostic loses current atomic census guard %q", guard)
		}
	}
	for _, forbidden := range []string{"topk(", "vector(0)", "clamp_min(", "sum(urnetwork_url_probe", `service="taskworker"`} {
		if strings.Contains(query, forbidden) {
			t.Errorf("deficit diagnostic can mix or manufacture cells: %s", forbidden)
		}
	}
	cohort := dashboardPanelById(dashboard, 78)
	if cohort == nil || !strings.Contains(query, strings.TrimSuffix(strings.SplitN(cohort.Targets[0].Expr, "and on(env,job,host,block,instance) ", 2)[1], ")")) {
		t.Error("deficit diagnostic does not share the cohort panels' owner and freshness guard")
	}
	if !strings.Contains(panel.Description, "never summed") || !strings.Contains(panel.Description, "no data") ||
		!strings.Contains(panel.Description, "stable client-hash order") {
		t.Error("deficit diagnostic omits publisher, freshness or sampling semantics")
	}
	if slices.Contains(dashboard.Tags, PublicTag) {
		t.Fatal("URL deficit diagnostics must remain authenticated")
	}
}
