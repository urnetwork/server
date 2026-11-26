package monitor

// §32's compute families together (CSRV-M12-06; DESIGN-COMPUTE §14.7): off without monitor.yml's compute block or
// with a family it does not enable, blind with a block that does not read or with no current series, every metric a
// family reads documented in SIGNALS.md §32, and no query keeping a label that could name a network. Pure: the query
// is the test's.

import (
	"context"
	"errors"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// The settings with families enabled.
func computeTestSettings(families ...string) *ComputeMonitorSettings {
	settings := DefaultComputeMonitorSettings()
	settings.Families = families
	return settings
}

// A probe env over settings.
func computeTestEnv(settings *ComputeMonitorSettings) *probeEnv {
	return &probeEnv{cfg: &monitorConfig{env: "synthetic", compute: settings}, now: func() time.Time {
		return time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	}}
}

// The liveness sample every enabled family needs.
func computeTestLive() *sidecarStorageSample {
	return &sidecarStorageSample{check: computeLivenessTag, value: 1}
}

// A rule's sample.
func computeTestSample(class string, tag string, value float64, labels map[string]string) *sidecarStorageSample {
	return &sidecarStorageSample{check: computeRuleTag(class, tag), value: value, labels: labels}
}

// The findings of class, by target.
func computeTestFindings(findings []finding, class string) map[string]finding {
	targetFindings := map[string]finding{}
	for _, f := range findings {
		if f.class == class {
			targetFindings[f.target] = f
		}
	}
	return targetFindings
}

// Every family's.
func computeTestFamilies() []*computeFamily {
	return []*computeFamily{computeLedgerFamily(), computeProvidersFamily(), computePolicyFamily(), computeSearchFamily(), computeEnclavesFamily()}
}

// Without the block each family reports itself configured off, and with a block that does not enable it, too; a block
// that does not read is blind; none of these asks Mimir.
func TestComputeSignalsAreOffWithoutTheirBlock(t *testing.T) {
	queried := 0
	query := func(ctx context.Context, env *probeEnv, text string) ([]*sidecarStorageSample, error) {
		queried += 1
		return nil, nil
	}
	broken := DefaultComputeMonitorSettings()
	broken.ConfigError = "compute names \"x\", which is no family of §32"
	for _, family := range computeTestFamilies() {
		for label, settings := range map[string]*ComputeMonitorSettings{
			"no block":              nil,
			"another family alone":  computeTestSettings(slices.DeleteFunc(slices.Clone(computeFamilies), func(other string) bool { return other == family.family })...),
			"a block that is empty": computeTestSettings(),
		} {
			findings := computeFamilyFindings(context.Background(), computeTestEnv(settings), family, settings, query)
			if len(findings) != 1 || !findings[0].configuredOff || findings[0].class != family.unobservableClass {
				t.Errorf("%s with %s: %+v", family.key, label, findings)
			}
		}
		findings := computeFamilyFindings(context.Background(), computeTestEnv(broken), family, broken, query)
		if len(findings) != 1 || !findings[0].unobservable || findings[0].configuredOff {
			t.Errorf("%s with a broken block: %+v", family.key, findings)
		}
	}
	if queried != 0 {
		t.Fatalf("Mimir was asked %d times", queried)
	}
	// monitor.yml's block, as config.go loads it
	if loadComputeMonitorSettings(nil) != nil {
		t.Error("no block loads settings")
	}
	if settings := loadComputeMonitorSettings(&computeMonitorYaml{Families: []string{"ledger", "search"}}); settings.ConfigError != "" || !slices.Equal(settings.Families, []string{"ledger", "search"}) {
		t.Errorf("ledger and search load as %+v", settings)
	}
	for _, families := range [][]string{{"ledger", "ledger"}, {"treasury"}} {
		if settings := loadComputeMonitorSettings(&computeMonitorYaml{Families: families}); settings.ConfigError == "" {
			t.Errorf("%v loads", families)
		}
	}
}

// An enabled family with no current liveness series reads its visibility class, which holds its pages open; one whose
// query fails reads it too; and with the liveness current, a rule whose own series is absent reads unobservable.
func TestComputeSignalReadsUnobservableWithoutSeries(t *testing.T) {
	for _, family := range computeTestFamilies() {
		settings := computeTestSettings(family.family)
		empty := func(ctx context.Context, env *probeEnv, text string) ([]*sidecarStorageSample, error) {
			return nil, nil
		}
		findings := computeFamilyFindings(context.Background(), computeTestEnv(settings), family, settings, empty)
		if len(findings) != 1 || findings[0].class != family.unobservableClass || !findings[0].isUnobservable() {
			t.Errorf("%s with no series: %+v", family.key, findings)
		}
		failing := func(ctx context.Context, env *probeEnv, text string) ([]*sidecarStorageSample, error) {
			return nil, errors.New("mimir did not answer")
		}
		findings = computeFamilyFindings(context.Background(), computeTestEnv(settings), family, settings, failing)
		if len(findings) != 1 || findings[0].class != family.unobservableClass || !findings[0].isUnobservable() {
			t.Errorf("%s with a failed query: %+v", family.key, findings)
		}
	}
	// the settlement lag's gauge absent while the family is live
	findings := computeFamilyJudge(computeLedgerFamily(), computeTestSettings("ledger"), []*sidecarStorageSample{computeTestLive()})
	if lag := computeTestFindings(findings, "compute-settlement-lag")["compute"]; !lag.unobservable || lag.healthy {
		t.Errorf("an absent lag reads %+v", lag)
	}
	// an alarm counter's absence is no alarm
	if rollup := computeTestFindings(findings, "compute-rollup-mismatch")["compute"]; !rollup.healthy {
		t.Errorf("no alarm series reads %+v", rollup)
	}
}

// Every metric a family's query reads, and every one §32's list holds, is documented in SIGNALS.md §32.
func TestComputeMetricNamesAreDocumented(t *testing.T) {
	catalog, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(catalog), "## 32. ")
	if start < 0 {
		t.Fatal("SIGNALS.md has no §32")
	}
	section := string(catalog[start:])
	if next := strings.Index(section[1:], "\n## "); 0 <= next {
		section = section[:next+1]
	}
	metricPattern := regexp.MustCompile(`urnetwork_compute_[a-z_]+`)
	read := map[string]bool{}
	for _, family := range computeTestFamilies() {
		for _, name := range metricPattern.FindAllString(computeFamilyQuery("synthetic", DefaultComputeMonitorSettings(), family), -1) {
			read[name] = true
		}
	}
	if len(read) < 20 {
		t.Fatalf("the families read %d metrics", len(read))
	}
	for name := range read {
		if !slices.Contains(computeMetricNames, name) {
			t.Errorf("%s is read and not in computeMetricNames", name)
		}
	}
	for _, name := range computeMetricNames {
		if !strings.Contains(section, "`"+name) {
			t.Errorf("%s is not documented in SIGNALS.md §32", name)
		}
	}
}

// No family's query keeps a label but a provider's catalog id, or matches on one that could name a network, member,
// sidecar or request: its matchers are the environment and closed sets (I-13).
func TestComputeQueriesHoldNoNetworkLabel(t *testing.T) {
	for _, family := range computeTestFamilies() {
		query := computeFamilyQuery("synthetic", DefaultComputeMonitorSettings(), family)
		for _, group := range regexp.MustCompile(`by \(([^)]*)\)`).FindAllStringSubmatch(query, -1) {
			if group[1] != "provider" {
				t.Errorf("%s keeps %q", family.key, group[1])
			}
		}
		for _, matcher := range regexp.MustCompile(`([a-z_]+)(=~|=)"`).FindAllStringSubmatch(query, -1) {
			if !slices.Contains([]string{"env", "state", "outcome", "result", "class", "kind", "monitor_check"}, matcher[1]) {
				t.Errorf("%s matches on %s", family.key, matcher[1])
			}
		}
	}
}

// Every page class a family reports has its row and is a page class, and every compute row a family's rule.
func TestComputePageClassesHaveTheirRules(t *testing.T) {
	ruleClasses := map[string]bool{}
	for _, family := range computeTestFamilies() {
		if alertClassKinds[family.unobservableClass] != visibilityAlertClass {
			t.Errorf("%s is not a visibility class", family.unobservableClass)
		}
		for _, rule := range family.rules {
			ruleClasses[rule.class] = true
			if _, ok := pageClassOf(rule.class); !ok || alertClassKinds[rule.class] != pageAlertClass {
				t.Errorf("%s has no page row", rule.class)
			}
		}
	}
	if len(ruleClasses) != 26 {
		t.Errorf("the families report %d page classes, §14.7 twenty-six", len(ruleClasses))
	}
	for _, row := range pageClasses {
		if strings.HasPrefix(row.class, "compute-") && !ruleClasses[row.class] {
			t.Errorf("%s has a row and no rule", row.class)
		}
	}
}
