// The proxy test dashboard's shared copy: the internal proxy test dashboard
// over every country, with fixed queries, so that a grafana public dashboard
// runs every panel. It is tagged public and unlisted: published for an outside
// viewer given its link (owner decision, 2026-11-09) and left out of the public
// directory. The copy is derived from the internal dashboard, so these tests
// hold the two together: an edit to proxy-test.json fails here until the copy
// follows it.
package grafana

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

const proxyTestSharedDashboardName = "proxy-test-shared.json"

// The country variable's matcher as every per-country query of the internal
// dashboard writes it. With every country selected it is country=~".*", which
// matches every series, so dropping it keeps the query's meaning.
const proxyTestCountryMatcher = `,country=~"$country"`

func readProxyTestDocumentJson(t *testing.T, name string) map[string]any {
	t.Helper()
	body, err := dashboardsFs.ReadFile("dashboards/" + name)
	if err != nil {
		t.Fatal(err)
	}
	document := map[string]any{}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return document
}

// Every panel of a dashboard document, row panels' nested panels included.
func proxyTestDocumentPanels(document map[string]any) []map[string]any {
	panels := []map[string]any{}
	var walk func(node any)
	walk = func(node any) {
		list, _ := node.([]any)
		for _, item := range list {
			if panel, ok := item.(map[string]any); ok {
				panels = append(panels, panel)
				walk(panel["panels"])
			}
		}
	}
	walk(document["panels"])
	return panels
}

// The shared copy as derived from the internal dashboard document: its
// identity and tags, no variables and no links, every country matcher
// dropped, and titles that said "selected countries" saying "all countries".
// The tags leave out the internal dashboard's host tag, since public viewers
// read the tags. Descriptions are the copy's own, and are checked on their own.
func proxyTestSharedFromInternal(t *testing.T, internal map[string]any) map[string]any {
	t.Helper()
	body, err := json.Marshal(internal)
	if err != nil {
		t.Fatal(err)
	}
	shared := map[string]any{}
	if err := json.Unmarshal(body, &shared); err != nil {
		t.Fatal(err)
	}
	shared["uid"] = "urnetwork-proxy-test-shared"
	shared["title"] = "urnetwork / proxy test (shared)"
	shared["tags"] = []any{"urnetwork", "proxy", PublicTag, UnlistedTag}
	templating, ok := shared["templating"].(map[string]any)
	if !ok {
		t.Fatal("the internal dashboard has no templating")
	}
	templating["list"] = []any{}
	shared["links"] = []any{}
	for _, panel := range proxyTestDocumentPanels(shared) {
		if title, ok := panel["title"].(string); ok {
			panel["title"] = strings.ReplaceAll(title, "selected countries", "all countries")
		}
		delete(panel, "links")
		targets, _ := panel["targets"].([]any)
		for _, item := range targets {
			if target, ok := item.(map[string]any); ok {
				if expr, ok := target["expr"].(string); ok {
					target["expr"] = strings.ReplaceAll(expr, proxyTestCountryMatcher, "")
				}
			}
		}
	}
	return shared
}

// Removes the dashboard's and the panels' descriptions, returning them.
func proxyTestRemoveDescriptions(document map[string]any) []string {
	descriptions := []string{}
	if description, ok := document["description"].(string); ok {
		descriptions = append(descriptions, description)
	}
	delete(document, "description")
	for _, panel := range proxyTestDocumentPanels(document) {
		if description, ok := panel["description"].(string); ok {
			descriptions = append(descriptions, description)
		}
		delete(panel, "description")
	}
	return descriptions
}

// The first path at which two decoded json documents differ, or "".
func proxyTestJsonDifference(path string, want any, got any) string {
	switch wantValue := want.(type) {
	case map[string]any:
		gotValue, ok := got.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s: want an object, got %v", path, got)
		}
		keys := []string{}
		for key := range wantValue {
			keys = append(keys, key)
		}
		for key := range gotValue {
			if _, ok := wantValue[key]; !ok {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			if difference := proxyTestJsonDifference(path+"."+key, wantValue[key], gotValue[key]); difference != "" {
				return difference
			}
		}
		return ""
	case []any:
		gotValue, ok := got.([]any)
		if !ok || len(gotValue) != len(wantValue) {
			return fmt.Sprintf("%s: want %d items, got %v", path, len(wantValue), got)
		}
		for index := range wantValue {
			if difference := proxyTestJsonDifference(fmt.Sprintf("%s[%d]", path, index), wantValue[index], gotValue[index]); difference != "" {
				return difference
			}
		}
		return ""
	default:
		if !reflect.DeepEqual(want, got) {
			return fmt.Sprintf("%s: want %v, got %v", path, want, got)
		}
		return ""
	}
}

func TestProxyTestSharedDashboardIdentity(t *testing.T) {
	dashboard := readTestDashboard(t, proxyTestSharedDashboardName)
	if dashboard.Uid != "urnetwork-proxy-test-shared" || dashboard.Title != "urnetwork / proxy test (shared)" {
		t.Errorf("uid %q, title %q", dashboard.Uid, dashboard.Title)
	}
	if !slices.Equal(dashboard.Tags, []string{"urnetwork", "proxy", PublicTag, UnlistedTag}) {
		t.Errorf("tags = %q", dashboard.Tags)
	}
	if len(dashboard.Templating.List) != 0 || len(dashboard.Links) != 0 {
		t.Errorf("the copy has %d variables and %d links, want none", len(dashboard.Templating.List), len(dashboard.Links))
	}

	// the runner's host, as the internal dashboard's description gives its
	// host label, is named nowhere in the copy, tags included: public viewers
	// read the whole document
	internalDescription, _ := readProxyTestDocumentJson(t, proxyTestDashboardName)["description"].(string)
	hostLabel := regexp.MustCompile(`host="([^"]+)"`).FindStringSubmatch(internalDescription)
	if hostLabel == nil {
		t.Fatal("the internal dashboard's description no longer gives the runner's host label; update this check")
	}
	runnerHost := hostLabel[1]

	// no link anywhere: an outside viewer can open no private dashboard and no
	// explore view
	var walk func(path string, node any)
	walk = func(path string, node any) {
		switch value := node.(type) {
		case map[string]any:
			for key, child := range value {
				if links, ok := child.([]any); key == "links" && (!ok || len(links) != 0) {
					t.Errorf("%s.links = %v, want none", path, child)
				}
				walk(path+"."+key, child)
			}
		case []any:
			for index, child := range value {
				walk(fmt.Sprintf("%s[%d]", path, index), child)
			}
		case string:
			for _, internalPath := range []string{"/d/", "/explore", "/dashboards/", "/goto/"} {
				if strings.Contains(value, internalPath) {
					t.Errorf("%s links into grafana (%s): %s", path, internalPath, value)
				}
			}
			if strings.Contains(strings.ToLower(value), strings.ToLower(runnerHost)) {
				t.Errorf("%s names the runner's host: %s", path, value)
			}
		}
	}
	walk("", readProxyTestDocumentJson(t, proxyTestSharedDashboardName))
}

// The copy is the internal dashboard with only the changes that make it run
// as a public dashboard: the layout, the field configs, the units and
// thresholds, the time range and every query are the internal dashboard's,
// each country matcher dropped.
func TestProxyTestSharedDashboardIsTheInternalDashboardOverEveryCountry(t *testing.T) {
	internal := readProxyTestDocumentJson(t, proxyTestDashboardName)
	shared := readProxyTestDocumentJson(t, proxyTestSharedDashboardName)

	// the internal dashboard selects countries only through this matcher, so
	// dropping it leaves no country variable anywhere
	matchers := 0
	for _, expression := range dashboardDocumentExpressions(t, proxyTestDashboardName) {
		matchers += strings.Count(expression, proxyTestCountryMatcher)
	}
	if matchers == 0 {
		t.Fatal("the internal dashboard has no country matcher to drop")
	}

	want := proxyTestSharedFromInternal(t, internal)
	proxyTestRemoveDescriptions(want)
	proxyTestRemoveDescriptions(shared)
	if difference := proxyTestJsonDifference("", want, shared); difference != "" {
		t.Fatalf("the shared copy is not the internal dashboard over every country; regenerate it from proxy-test.json: %s", difference)
	}
}

// A grafana public dashboard returns each query's series with all their
// labels, so a panel of the copy may show only the runner's own labels: every
// metric is aggregated, grouped by runner labels only, and nothing joins or
// rewrites labels back in. The promtool evaluation below shows the same for
// input series that carry host, instance, account and network labels.
func TestProxyTestSharedDashboardShowsOnlyRunnerLabels(t *testing.T) {
	expressions := dashboardDocumentExpressions(t, proxyTestSharedDashboardName)
	if len(expressions) == 0 {
		t.Fatal("the shared copy has no queries")
	}
	groupingPattern := regexp.MustCompile(`\b(by|on|ignoring)\s*\(([^)]*)\)`)
	for _, expression := range expressions {
		if strings.Contains(expression, "$") {
			t.Errorf("query uses a variable, which a public dashboard does not resolve: %s", expression)
		}
		for _, operator := range []string{"without", "ignoring", "group_left", "group_right", "label_replace", "label_join"} {
			if regexp.MustCompile(`\b` + operator + `\b`).MatchString(expression) {
				t.Errorf("query uses %s, which can keep or add labels: %s", operator, expression)
			}
		}
		for _, grouping := range groupingPattern.FindAllStringSubmatch(expression, -1) {
			for _, label := range strings.Split(grouping[2], ",") {
				label = strings.TrimSpace(label)
				if label == "" {
					continue
				}
				if !slices.Contains(proxyTestLabels, label) {
					t.Errorf("query groups %s %q, which is not a runner label: %s", grouping[1], label, expression)
				}
			}
		}
		// each read sits directly in a max, which leaves only its grouping
		for _, read := range proxyTestReads(t, expression) {
			before := read.before
			for _, function := range append(slices.Clone(proxyTestCounterFunctions), proxyTestGaugeFunctions...) {
				before = strings.TrimSuffix(before, function)
			}
			if proxyTestMaxPattern.FindStringSubmatch(before) == nil {
				t.Errorf("%s is not read directly inside max, so its series' labels reach the result: %s", read.metric, expression)
			}
		}
	}
}

// The copy shows the providers by country to an outside viewer: the total,
// the age of the counts, the total over time and the table of every listed
// country, with the internal dashboard's queries over every country, and the
// table's description says the averages fill in from the deploy.
func TestProxyTestSharedDashboardShowsProvidersByCountry(t *testing.T) {
	proxyTestAssertProvidersPanels(t, proxyTestSharedDashboardName, func(query string) string {
		return strings.ReplaceAll(query, proxyTestCountryMatcher, "")
	})
}

// promtool evaluates the copy's panels over the same runner files as the
// internal dashboard's with every country selected, and every panel gives the
// same series. The input series also carry an instance address, an account
// and a network id, and no panel passes one on.
func TestProxyTestSharedDashboardPromqlEvaluatesTheRunnerFile(t *testing.T) {
	proxyTestEvaluateRunnerFile(t, proxyTestSharedDashboardName, `,instance="192.0.2.10:2021",account="synthetic-account",network_id="synthetic-network"`)
}

// The copy's descriptions read for an outside viewer: each panel says how to
// read it, none asks to choose countries, which the copy cannot do, and none
// names the runner's host labels, its files or an account.
func TestProxyTestSharedDashboardDescriptionsReadForAnOutsideViewer(t *testing.T) {
	shared := readProxyTestDocumentJson(t, proxyTestSharedDashboardName)
	for _, panel := range proxyTestDocumentPanels(shared) {
		description, _ := panel["description"].(string)
		if panel["type"] != "row" && description == "" {
			t.Errorf("panel %v (%v) has no description", panel["id"], panel["title"])
		}
	}
	// an account is "the account", "the test account" or "a Pro test
	// account", never a named one
	accountPattern := regexp.MustCompile(`(\S+) (?:Pro test )?account\b`)
	for _, description := range proxyTestRemoveDescriptions(shared) {
		for _, word := range []string{"selected", "Select ", "host=", "job=", "instance", "/var/", "/etc/", "$"} {
			if strings.Contains(description, word) {
				t.Errorf("description says %q: %s", word, description)
			}
		}
		for _, match := range accountPattern.FindAllStringSubmatch(description, -1) {
			if !slices.Contains([]string{"the", "a", "test"}, match[1]) {
				t.Errorf("description names an account (%q): %s", match[0], description)
			}
		}
	}
}
