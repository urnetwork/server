package monitor

// Signal-specific unknown states reach the Slack notifier as visibility, so
// an execution that could not observe part of its source never clears what it
// did not see.

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Runs a real signal through the run loop and cadence gate, one tick per run,
// calling beforeRun ahead of each run and handing every gated execution to
// the notifier.
func postSignalRunsThroughTheGate(t *testing.T, signal Signal, settings SignalSettings, runs int, beforeRun func(int), notifier *SlackAlertNotifier) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	handled := make(chan struct{})
	runErr := make(chan error, 1)
	beforeRun(0)
	go func() {
		runErr <- NewWithSignals(settings, signal).runExecutionLoop(
			ctx,
			RunLoopOptions{},
			func(_ context.Context, signal Signal, execution SignalExecution) error {
				notifier.Handle(signal, execution)
				handled <- struct{}{}
				return nil
			},
			func(time.Duration) runLoopTicker { return &manualRunLoopTicker{c: ticks} },
		)
	}()
	for run := 0; run < runs; run++ {
		if 0 < run {
			beforeRun(run)
			select {
			case ticks <- time.Now():
			case <-time.After(30 * time.Second):
				t.Fatalf("run %d: the loop never waited for its cadence", run+1)
			}
		}
		select {
		case <-handled:
		case <-time.After(30 * time.Second):
			t.Fatalf("run %d was never handled", run+1)
		}
	}
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("run loop returned %v after cancellation", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run loop did not stop after cancellation")
	}
}

// Counts posts with a prefix that name an identity.
func countSlackPosts(texts []string, prefix string, identity string) int {
	count := 0
	for _, text := range texts {
		if strings.HasPrefix(text, prefix) && strings.Contains(text, identity) {
			count++
		}
	}
	return count
}

// A redis node whose INFO times out is reported as redis/node-mem
// cannot-observe, an unobservable finding: its node-mem PAGE is kept while the
// node is unreachable instead of clearing and being raised again.
func TestSlackAlertNotifierKeepsARedisPageWhileItsNodeIsUnreachable(t *testing.T) {
	var run atomic.Int32
	rows := []string{
		"6380 93 100 80 1 10\n6381 10 100 9 1 10\n6382 10 100 9 1 10",
		"6380 unreachable\n6381 10 100 9 1 10\n6382 10 100 9 1 10",
		"6380 unreachable\n6381 10 100 9 1 10\n6382 10 100 9 1 10",
		"6380 93 100 80 1 10\n6381 10 100 9 1 10\n6382 10 100 9 1 10",
	}
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if strings.Contains(command, "for p in") {
			return rows[run.Load()], nil
		}
		return "", nil
	}}
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
	postSignalRunsThroughTheGate(t, NewRedisMemorySignal(), syntheticSettings(source), len(rows), func(i int) { run.Store(int32(i)) }, notifier)
	closeSlackNotifier(t, notifier)

	texts := webhook.posted()
	// nothing the unreachable node hides clears, and nothing is raised twice
	for _, identity := range []string{
		"redis/node-mem-critical · node-mem-critical · redis-1:6380",
		"redis/mem-skew · mem-skew · redis-1:6380",
		// the unreachable node is an incident of its own after two runs
		"redis/node-mem · cannot-observe · redis-1",
	} {
		if countSlackPosts(texts, "[", identity) != 1 || countSlackPosts(texts, "[CLEARED]", identity) != 0 {
			t.Fatalf("%s was not raised exactly once and kept:\n%s", identity, strings.Join(texts, "\n---\n"))
		}
	}
	if countSlackPosts(texts, "[PAGE] raised", "redis/node-mem-critical · node-mem-critical · redis-1:6380") != 1 || len(texts) != 3 {
		t.Fatalf("posts:\n%s", strings.Join(texts, "\n---\n"))
	}
}

// redis-connections shares the memory probe but accepts only clients-spike
// findings. A failed host's cannot-observe passes its filter, so a spike on
// that host is kept while the host does not answer.
func TestSlackAlertNotifierKeepsARedisSpikeWhileItsHostIsUnreachable(t *testing.T) {
	var run atomic.Int32
	nodeRows := func(lo, hi int, spikePort int) string {
		lines := []string{}
		for port := lo; port <= hi; port++ {
			connected := 10
			if port == spikePort {
				connected = 1000
			}
			lines = append(lines, fmt.Sprintf("%d 10 100 9 1 %d", port, connected))
		}
		return strings.Join(lines, "\n")
	}
	// the spike reaches its Sustain of 2, edge-5 stops answering for two runs,
	// then the spike is back
	edge5Down := []bool{false, false, true, true, false, false}
	source := &syntheticSource{hostFn: func(configured HostSettings, command string) (string, error) {
		if !strings.Contains(command, "for p in") {
			return "", nil
		}
		if configured.Name == redisTestEdge5 {
			if edge5Down[run.Load()] {
				return "", errors.New("synthetic ssh timeout")
			}
			return nodeRows(6412, 6427, 6412), nil
		}
		return nodeRows(6380, 6411, 0), nil
	}}
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
	postSignalRunsThroughTheGate(t, NewRedisConnectionsSignal(), redisTwoHostSettings(source), len(edge5Down), func(i int) { run.Store(int32(i)) }, notifier)
	closeSlackNotifier(t, notifier)

	texts := webhook.posted()
	spike := "redis/clients-spike · clients-spike · " + redisTestEdge5 + ":6412"
	if countSlackPosts(texts, "[CLEARED]", spike) != 0 || countSlackPosts(texts, "[WARN] raised", spike) != 1 {
		t.Fatalf("clients spike was not kept while its host was unreachable:\n%s", strings.Join(texts, "\n---\n"))
	}
	if countSlackPosts(texts, "[WARN] raised", "redis/node-mem · cannot-observe · "+redisTestEdge5+"/node-mem") != 1 {
		t.Fatalf("the unreachable host was not raised through the redis-connections filter:\n%s", strings.Join(texts, "\n---\n"))
	}
}

// pg-cpu reports a database host it cannot read as pg-cpu-unavailable, an
// unknown state named neither cannot-observe nor *-unobservable: its
// pg-cpu-high PAGE is kept while the host does not answer instead of clearing
// and being raised again.
func TestSlackAlertNotifierKeepsAPgCpuPageWhileItsHostIsUnavailable(t *testing.T) {
	var run atomic.Int32
	page := pgCpuTestOutput(syntheticSettings(nil).Now(), 34000000000)
	// the PAGE reaches its Sustain of 2, the host stops answering for two runs,
	// the Sustain of pg-cpu-unavailable, then the PAGE is back
	unavailable := []bool{false, false, true, true, false, false}
	source := &syntheticSource{hostTimeoutFn: func(HostSettings, string, time.Duration) (string, error) {
		if unavailable[run.Load()] {
			return "", errors.New("synthetic ssh timeout")
		}
		return page, nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "database-a.example", Roles: []string{"pg-primary"}}}
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
	postSignalRunsThroughTheGate(t, NewPgCpuSignal(), settings, len(unavailable), func(i int) { run.Store(int32(i)) }, notifier)
	closeSlackNotifier(t, notifier)

	texts := webhook.posted()
	high := "pg/cpu · pg-cpu-high · database-a.example"
	if countSlackPosts(texts, "[CLEARED]", high) != 0 || countSlackPosts(texts, "[PAGE] raised", high) != 1 {
		t.Fatalf("pg-cpu-high was not raised once and kept while its host was unavailable:\n%s", strings.Join(texts, "\n---\n"))
	}
	if countSlackPosts(texts, "[WARN] raised", "pg/cpu · pg-cpu-unavailable · database-a.example") != 1 {
		t.Fatalf("the unavailable host was not raised:\n%s", strings.Join(texts, "\n---\n"))
	}
}

// While hosts are blocked, global findings are withheld from the run and the
// host-scope partial alert stands in for them: the run is incomplete, so a
// withheld global PAGE is kept. A configured exclusion without blocked hosts,
// which settings freshness reports every run, does not make a run incomplete.
func TestSlackAlertNotifierKeepsAGlobalPageWhileHostsAreBlocked(t *testing.T) {
	settings := syntheticSettings(nil)
	settings.ExcludedHosts = []string{"pg-1"}
	partial := func(blockedCount int) Alert {
		return alertFromFinding(settings, slackTestSignal.number, slackTestSignal.key, "PostgreSQL active backends", hostScopeCoverageFinding(settings, blockedCount))
	}
	if isVisibilityAlert(partial(0)) || !isVisibilityAlert(partial(1)) {
		t.Fatalf("host-scope partial unobservable: configured exclusion %t, blocked host %t", isVisibilityAlert(partial(0)), isVisibilityAlert(partial(1)))
	}

	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
	fleet := slackTestAlert("fleet", "", SeverityPage)
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{fleet}})
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{partial(1)}})
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{partial(1)}})
	closeSlackNotifier(t, notifier)

	texts := webhook.posted()
	if countSlackPosts(texts, "[CLEARED]", "pg/active-pileup · active-pileup · fleet") != 0 {
		t.Fatalf("a withheld global PAGE cleared under partial host coverage:\n%s", strings.Join(texts, "\n---\n"))
	}
}

// Without its flag the PG query sample reports pg-query-sample-unavailable
// every run. That standing, configured state never raises an incident or
// joins a start digest; it still marks its runs unobservable.
func TestSlackAlertNotifierIgnoresAConfiguredOffQuerySample(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.SSHUser = "monitor"
	for _, digest := range []bool{false, true} {
		webhook, webhookUrl := newFakeSlackWebhook(t)
		harness := newSlackTestHarness()
		notifierSettings := harness.settings(webhookUrl)
		if digest {
			notifierSettings = harness.digestSettings(webhookUrl)
		}
		notifier := NewSlackAlertNotifier(notifierSettings)
		postSignalRunsThroughTheGate(t, NewPgQuerySampleSignal(), settings, 3, func(int) {}, notifier)
		closeSlackNotifier(t, notifier)
		if texts := webhook.posted(); len(texts) != 0 {
			t.Errorf("digest %t: posted %q", digest, texts)
		}
	}
	// the alert itself is what the disabled sampler reports
	sample, err := NewPgQuerySampleSignal().Run(context.Background(), settings)
	if err != nil || len(sample) != 1 || sample[0].Class != "pg-query-sample-unavailable" || !sample[0].configuredOff || !sample[0].unobservable {
		t.Fatalf("disabled sample = %+v, %v", sample, err)
	}
}

// The direct alert constructors of unknown states mark their alerts, as does
// the proxy path that discovered no allocation, and ordinary alerts, including
// a measured proxy handshake failure, stay unmarked.
func TestEveryVisibilityAlertConstructorMarksItsAlert(t *testing.T) {
	settings := syntheticSettings(nil)
	pgSample := selectionSignal{number: "2.30", key: "pg-query-sample", id: "pg/query-sample"}
	alerts := Alerts{}
	for _, c := range []struct {
		signal Signal
		err    error
	}{
		{signal: slackTestSignal, err: errors.New("synthetic command timeout")},
		{signal: slackTestSignal, err: errors.New("host-a.example: kex_exchange_identification: read: Connection reset by peer")},
		{signal: slackTestSignal, err: newProviderVisibilityError(providerAuthenticationClass, "synthetic provider refusal")},
		{signal: pgSample, err: errors.New("synthetic sample failure")},
	} {
		alerts = append(alerts, visibilityAlert(settings, c.signal, c.err))
	}
	alerts = append(alerts,
		appleVisibilityAlert(settings, "apple-crash-data-stale", "symptom", "mechanism", "observed", "action"),
		appleBacklogAlert(settings, 3, 9),
		playVisibilityAlert(settings, "play-crash-data-stale", "symptom", "mechanism", "observed", "action"),
		playIssueOverflowAlert(settings, "com.example.app", 40, 20),
		alertFromFinding(settings, "1.3", "active-pileup", "PostgreSQL active backends", cannotObserveFinding("pg-1", errors.New("synthetic ssh timeout"))),
	)
	proxyPath := func(observed finding) Alert {
		return alertFromFinding(settings, "14.5", "proxy-path", "Public proxy path", observed)
	}
	noAllocation := evaluateProxyHandshakes(context.Background(), &probeEnv{cfg: &monitorConfig{env: "main"}}, &host{name: "proxy-1"}, nil, nil)
	if len(noAllocation) != 1 || noAllocation[0].class != "proxy-public-handshake" {
		t.Fatalf("proxy path without an allocation = %+v", noAllocation)
	}
	alerts = append(alerts, proxyPath(noAllocation[0]))
	freshness := settings
	freshness.SSHUser = "monitor"
	freshness.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) {
		return false, errors.New("synthetic settings reload failure")
	}
	unobservableSettings, err := NewSettingsFreshnessSignal().Run(context.Background(), freshness)
	if err != nil || len(unobservableSettings) != 1 || unobservableSettings[0].Class != "settings-generation-unobservable" {
		t.Fatalf("settings freshness = %+v, %v", unobservableSettings, err)
	}
	alerts = append(alerts, unobservableSettings...)
	for _, alert := range alerts {
		if !isVisibilityAlert(alert) {
			t.Errorf("visibility alert %s/%s is not marked unobservable", alert.SignalID, alert.Class)
		}
	}
	if isVisibilityAlert(slackTestAlert("pg-1", "", SeverityPage)) {
		t.Fatal("an ordinary alert is marked unobservable")
	}
	handshake := finding{probeId: "proxy/public-path", tier: tierWarn, class: "proxy-public-handshake", target: "proxy-1", frame: "main-proxy-g1/tcp4", sustain: 2}
	if isVisibilityAlert(proxyPath(handshake)) {
		t.Fatal("a measured proxy handshake failure is marked unobservable")
	}
}

// Inventory of the package source, so a new signal cannot add an unknown state
// the notifier does not see. Findings with the reserved names (class
// cannot-observe or *-unobservable, probe monitor/visibility) are marked by
// alertFromFinding; a finding of another unknown class sets unobservable
// itself. Alerts built directly bypass alertFromFinding, so:
//   - an Alert literal with a reserved Class literal, or with SignalID
//     monitor/visibility, sets unobservable: true;
//   - a function that builds an Alert from a class parameter sets
//     unobservable when any call passes it a reserved class literal.
func TestEveryUnobservableAlertConstructorSetsTheMarker(t *testing.T) {
	classPattern := regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	reserved := func(class string) bool {
		return classPattern.MatchString(class) && (class == "cannot-observe" || strings.HasSuffix(class, "-unobservable"))
	}
	stringValue := func(expr ast.Expr) (string, bool) {
		literal, ok := expr.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(literal.Value)
		return value, err == nil
	}
	keyValues := func(literal *ast.CompositeLit) map[string]ast.Expr {
		values := map[string]ast.Expr{}
		for _, element := range literal.Elts {
			if pair, ok := element.(*ast.KeyValueExpr); ok {
				if key, ok := pair.Key.(*ast.Ident); ok {
					values[key.Name] = pair.Value
				}
			}
		}
		return values
	}
	isTrue := func(expr ast.Expr) bool {
		ident, ok := expr.(*ast.Ident)
		return ok && ident.Name == "true"
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fileSet := token.NewFileSet()
	files := []*ast.File{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}

	// function name -> index of the class parameter its Alert literal uses,
	// and whether that literal sets the marker
	type classHelper struct {
		parameter int
		marks     bool
	}
	nameClassHelpers := map[string]classHelper{}
	reservedClasses := map[string]bool{}
	alertLiterals := 0
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			if literal, ok := node.(*ast.BasicLit); ok {
				if value, ok := stringValue(literal); ok && reserved(value) {
					reservedClasses[value] = true
				}
			}
			return true
		})
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			nameParameters := map[string]int{}
			index := 0
			for _, field := range function.Type.Params.List {
				for _, name := range field.Names {
					nameParameters[name.Name] = index
					index++
				}
				if len(field.Names) == 0 {
					index++
				}
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				literal, ok := node.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if ident, ok := literal.Type.(*ast.Ident); !ok || ident.Name != "Alert" {
					return true
				}
				alertLiterals++
				values := keyValues(literal)
				position := fileSet.Position(literal.Pos())
				class, classIsLiteral := stringValue(values["Class"])
				signalId, _ := stringValue(values["SignalID"])
				if (classIsLiteral && reserved(class)) || signalId == "monitor/visibility" {
					if !isTrue(values["unobservable"]) {
						t.Errorf("%s: Alert %s/%s does not set unobservable: true", position, signalId, class)
					}
				}
				if ident, ok := values["Class"].(*ast.Ident); ok {
					if parameter, ok := nameParameters[ident.Name]; ok {
						_, marks := values["unobservable"]
						nameClassHelpers[function.Name.Name] = classHelper{parameter: parameter, marks: marks}
					}
				}
				return true
			})
		}
	}
	if alertLiterals < 10 || len(nameClassHelpers) == 0 || len(reservedClasses) < 30 {
		t.Fatalf("inventory too small to be the package: %d Alert literals, %d class helpers, %d reserved classes", alertLiterals, len(nameClassHelpers), len(reservedClasses))
	}

	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			helper, ok := nameClassHelpers[ident.Name]
			if !ok || len(call.Args) <= helper.parameter {
				return true
			}
			if class, ok := stringValue(call.Args[helper.parameter]); ok && reserved(class) && !helper.marks {
				t.Errorf("%s: %s builds a %s Alert without setting unobservable", fileSet.Position(call.Pos()), ident.Name, class)
			}
			return true
		})
	}

	// every reserved class reaching an alert through a finding is marked
	classes := make([]string, 0, len(reservedClasses))
	for class := range reservedClasses {
		classes = append(classes, class)
	}
	sort.Strings(classes)
	settings := syntheticSettings(nil)
	for _, class := range classes {
		alert := alertFromFinding(settings, "0", "inventory", "Inventory", finding{probeId: "inventory/probe", tier: tierWarn, class: class, target: "target"})
		if !alert.unobservable {
			t.Errorf("finding class %s is not marked unobservable", class)
		}
	}
	visibility := alertFromFinding(settings, "0", "inventory", "Inventory", finding{probeId: "monitor/visibility", tier: tierWarn, class: "tailer-reconcile", target: "target"})
	ordinary := alertFromFinding(settings, "0", "inventory", "Inventory", finding{probeId: "inventory/probe", tier: tierWarn, class: "active-pileup", target: "target"})
	if !visibility.unobservable || ordinary.unobservable {
		t.Fatalf("monitor/visibility finding marked %t, ordinary finding marked %t", visibility.unobservable, ordinary.unobservable)
	}
}
