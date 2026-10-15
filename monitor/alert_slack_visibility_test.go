package monitor

// Signal-specific unknown states reach the Slack notifier as visibility, so
// an execution that could not observe part of its source never clears what it
// did not see.

import (
	"context"
	"errors"
	"fmt"
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
