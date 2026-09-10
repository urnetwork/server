// Synthetic Mimir admission fixtures exercise strict reduction, state, and
// privacy boundaries without copying live identities or configuration values.
package monitor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	mimirAdmissionSyntheticPort        = 19191
	mimirAdmissionSyntheticLocalLimit  = 333333
	mimirAdmissionSyntheticGlobalLimit = 777777
)

// mimirAdmissionFixtureOptions defines one visibly synthetic child frame.
type mimirAdmissionFixtureOptions struct {
	port                 int
	observable           bool
	processStart         string
	memorySeries         int64
	activeSeries         int64
	createdTotal         int64
	removedTotal         int64
	localLimit           int64
	globalLimit          int64
	discardDescriptor    bool
	discardFamilyAbsent  bool
	discardAbsenceSource bool
	discardPresent       bool
	discardTotal         int64
}

// mimirAdmissionSyntheticResponse is one synthetic services-host result.
type mimirAdmissionSyntheticResponse struct {
	output string
	err    error
}

// mimirAdmissionInstanceFixture renders one strict synthetic child frame.
func mimirAdmissionInstanceFixture(options mimirAdmissionFixtureOptions) string {
	if !options.observable {
		return fmt.Sprintf("instance_begin %d\nobservable 0\ninstance_end\n", options.port)
	}
	return fmt.Sprintf(
		"instance_begin %d\n"+
			"observable 1\n"+
			"process_start %s\n"+
			"memory_series %d\n"+
			"active_series %d\n"+
			"created_total %d\n"+
			"removed_total %d\n"+
			"local_limit %d\n"+
			"global_limit %d\n"+
			"discard_descriptor %d\n"+
			"discard_family_absent %d\n"+
			"discard_absence_source %d\n"+
			"discard_present %d\n"+
			"discard_total %d\n"+
			"instance_end\n",
		options.port,
		options.processStart,
		options.memorySeries,
		options.activeSeries,
		options.createdTotal,
		options.removedTotal,
		options.localLimit,
		options.globalLimit,
		mimirAdmissionBoolInt(options.discardDescriptor),
		mimirAdmissionBoolInt(options.discardFamilyAbsent),
		mimirAdmissionBoolInt(options.discardAbsenceSource),
		mimirAdmissionBoolInt(options.discardPresent),
		options.discardTotal,
	)
}

// mimirAdmissionCompleteInstanceFixture renders an exact comparable child.
func mimirAdmissionCompleteInstanceFixture(
	processStart int64,
	discardTotal int64,
	createdTotal int64,
	removedTotal int64,
) string {
	return mimirAdmissionCompleteInstanceFixtureAt(
		mimirAdmissionSyntheticPort,
		strconv.FormatInt(processStart, 10),
		discardTotal,
		createdTotal,
		removedTotal,
	)
}

func mimirAdmissionCompleteInstanceFixtureAt(
	port int,
	processStart string,
	discardTotal int64,
	createdTotal int64,
	removedTotal int64,
) string {
	return mimirAdmissionInstanceFixture(mimirAdmissionFixtureOptions{
		port:                 port,
		observable:           true,
		processStart:         processStart,
		memorySeries:         111111,
		activeSeries:         55555,
		createdTotal:         createdTotal,
		removedTotal:         removedTotal,
		localLimit:           mimirAdmissionSyntheticLocalLimit,
		globalLimit:          mimirAdmissionSyntheticGlobalLimit,
		discardDescriptor:    true,
		discardAbsenceSource: true,
		discardPresent:       true,
		discardTotal:         discardTotal,
	})
}

// An exact reason vector has no exported child until Mimir first increments a
// user/group label tuple. The family descriptor makes that absence observable.
func mimirAdmissionLazyZeroInstanceFixture(
	processStart int64,
	createdTotal int64,
	removedTotal int64,
) string {
	return mimirAdmissionInstanceFixture(mimirAdmissionFixtureOptions{
		port:                 mimirAdmissionSyntheticPort,
		observable:           true,
		processStart:         strconv.FormatInt(processStart, 10),
		memorySeries:         111111,
		activeSeries:         55555,
		createdTotal:         createdTotal,
		removedTotal:         removedTotal,
		localLimit:           mimirAdmissionSyntheticLocalLimit,
		globalLimit:          mimirAdmissionSyntheticGlobalLimit,
		discardDescriptor:    true,
		discardAbsenceSource: true,
		discardPresent:       false,
		discardTotal:         0,
	})
}

// A freshly started, source-recognized Mimir emits no family metadata until
// any discarded-sample label child has been instantiated.
func mimirAdmissionSourceBackedWholeFamilyZeroInstanceFixture(
	processStart int64,
	createdTotal int64,
	removedTotal int64,
) string {
	return mimirAdmissionInstanceFixture(mimirAdmissionFixtureOptions{
		port:                 mimirAdmissionSyntheticPort,
		observable:           true,
		processStart:         strconv.FormatInt(processStart, 10),
		memorySeries:         111111,
		activeSeries:         55555,
		createdTotal:         createdTotal,
		removedTotal:         removedTotal,
		localLimit:           mimirAdmissionSyntheticLocalLimit,
		globalLimit:          mimirAdmissionSyntheticGlobalLimit,
		discardDescriptor:    false,
		discardFamilyAbsent:  true,
		discardAbsenceSource: true,
		discardPresent:       false,
		discardTotal:         0,
	})
}

// mimirAdmissionHostFixture appends the strict fleet and context trailer.
func mimirAdmissionHostFixture(
	instances []string,
	journalComplete bool,
	publisherStarts int64,
	readinessRejects int64,
	admissionRejects int64,
) string {
	return strings.Join(instances, "") + fmt.Sprintf(
		"mimir_count %d\n"+
			"journal_complete %d\n"+
			"publisher_starts %d\n"+
			"readiness_rejects %d\n"+
			"admission_rejects %d\n",
		len(instances),
		mimirAdmissionBoolInt(journalComplete),
		publisherStarts,
		readinessRejects,
		admissionRejects,
	)
}

// mimirAdmissionBoolInt maps a synthetic Boolean to the reducer wire value.
func mimirAdmissionBoolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// runMimirAdmissionSynthetic executes one tick against synthetic host frames.
func runMimirAdmissionSynthetic(
	t *testing.T,
	signal Signal,
	now time.Time,
	responses map[string]mimirAdmissionSyntheticResponse,
) []Alert {
	t.Helper()
	return runMimirAdmissionSyntheticWithStateDir(t, signal, now, "", responses)
}

// The state-directory variant exercises watcher replacement through the same
// atomic, versioned persistence used by configured monitor runs.
func runMimirAdmissionSyntheticWithStateDir(
	t *testing.T,
	signal Signal,
	now time.Time,
	stateDir string,
	responses map[string]mimirAdmissionSyntheticResponse,
) []Alert {
	t.Helper()
	var commands atomic.Int64
	var malformedCommand atomic.Bool
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		commands.Add(1)
		for _, required := range []string{
			mimirAdmissionMarker,
			"/api/v1/status/buildinfo",
			"/config",
			"/metrics",
			"per_user_series_limit",
		} {
			if !strings.Contains(command, required) {
				malformedCommand.Store(true)
			}
		}
		response, ok := responses[host.Name]
		if !ok {
			return "", fmt.Errorf("unexpected synthetic host %s", host.Name)
		}
		return response.output, response.err
	}}
	settings := syntheticSettings(source)
	settings.Environment = "synthetic-environment"
	settings.StateDir = stateDir
	settings.LogServices = []string{"api", "connect", "taskworker"}
	settings.LogServiceBlocks = map[string][]string{
		"api":        {"generated-api-a"},
		"connect":    {"generated-connect-a"},
		"taskworker": {"generated-worker-a"},
	}
	settings.Now = func() time.Time { return now }
	hostNames := make([]string, 0, len(responses))
	for hostName := range responses {
		hostNames = append(hostNames, hostName)
	}
	sort.Strings(hostNames)
	settings.Hosts = make([]HostSettings, 0, len(hostNames)+1)
	for _, hostName := range hostNames {
		settings.Hosts = append(settings.Hosts, HostSettings{
			Name:  hostName,
			Roles: []string{"services"},
		})
	}
	settings.Hosts = append(settings.Hosts, HostSettings{
		Name:  "database-only.example",
		Roles: []string{"pg-primary"},
	})

	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if malformedCommand.Load() {
		t.Error("Mimir admission command omitted a required bounded source")
	}
	if commands.Load() != int64(len(responses)) {
		t.Fatalf("Mimir admission commands = %d, want %d", commands.Load(), len(responses))
	}
	return alerts
}

// mimirAdmissionSingleHostResponses returns one complete synthetic host tick.
func mimirAdmissionSingleHostResponses(
	processStart int64,
	discardTotal int64,
	createdTotal int64,
	removedTotal int64,
	journalValues [3]int64,
) map[string]mimirAdmissionSyntheticResponse {
	return map[string]mimirAdmissionSyntheticResponse{
		"metrics-a.example": {
			output: mimirAdmissionHostFixture(
				[]string{mimirAdmissionCompleteInstanceFixture(
					processStart,
					discardTotal,
					createdTotal,
					removedTotal,
				)},
				true,
				journalValues[0],
				journalValues[1],
				journalValues[2],
			),
		},
	}
}

// A cumulative positive counter pages even before a same-generation baseline.
func TestMimirAdmissionSignalPagesOnInitialPositiveExactCounter(t *testing.T) {
	signal := NewMimirAdmissionSignal()
	if signal.Number() != "11.20a" || signal.Key() != "mimir-admission" ||
		signal.ID() != "observability/mimir-admission" || signal.Cadence() != time.Minute {
		t.Fatalf("wrong Mimir admission signal metadata: number=%s key=%s id=%s cadence=%s",
			signal.Number(), signal.Key(), signal.ID(), signal.Cadence())
	}
	now := time.Date(2032, 1, 2, 3, 4, 0, 0, time.UTC)
	alerts := runMimirAdmissionSynthetic(
		t,
		signal,
		now,
		mimirAdmissionSingleHostResponses(now.Add(-time.Hour).Unix(), 7, 123456, 3456, [3]int64{11, 13, 17}),
	)
	alert := requireAlertClass(t, alerts, "mimir-series-limit")
	if alert.Severity != SeverityPage || alert.Target != "mimir-fleet" ||
		alert.Frame != "per-user-series-limit" || alert.Sustain != 1 {
		t.Fatalf("wrong Mimir admission alert identity: %+v", alert)
	}
	for _, required := range []string{
		"initial_positive_instances=1",
		"discard_counter_increase=7",
		"local_limit=333333..333333",
		"global_limit=777777..777777",
		"publisher_starts=11",
		"readiness_rejects=13",
		"admission_rejects=17",
		"same-window context only",
		"SIGNALS.md §11.20a",
	} {
		if !strings.Contains(alert.Markdown(), required) {
			t.Errorf("Mimir admission alert lacks %q:\n%s", required, alert.Markdown())
		}
	}
}

// A valid family descriptor makes an uninstantiated exact reason an observed
// zero. A later disappearance after a positive remains a reset, not recovery.
func TestMimirAdmissionSignalTreatsDescriptorBackedAbsentRowAsLazyZero(t *testing.T) {
	stateDir := t.TempDir()
	signal := NewMimirAdmissionSignal()
	start := time.Date(2032, 1, 2, 4, 5, 0, 0, time.UTC)
	processStart := start.Add(-time.Hour).Unix()
	lazyZero := map[string]mimirAdmissionSyntheticResponse{
		"metrics-a.example": {output: mimirAdmissionHostFixture(
			[]string{mimirAdmissionLazyZeroInstanceFixture(processStart, 123456, 3456)},
			true,
			0,
			0,
			0,
		)},
	}
	if alerts := runMimirAdmissionSyntheticWithStateDir(t, signal, start, stateDir, lazyZero); len(alerts) != 0 {
		t.Fatalf("descriptor-backed lazy zero alerted: %+v", alerts)
	}
	persisted := mimirAdmissionPersistedState{}
	loaded, err := loadProviderState(stateDir, "mimir-admission", mimirAdmissionStateVersion, &persisted)
	if err != nil || !loaded || len(persisted.Histories) != 1 || persisted.Histories[0].DiscardTotal != 0 {
		t.Fatalf("lazy-zero baseline was not persisted: loaded=%t state=%+v err=%v", loaded, persisted, err)
	}

	positive := mimirAdmissionSingleHostResponses(processStart, 4, 123460, 3456, [3]int64{})
	page := requireAlertClass(
		t,
		runMimirAdmissionSyntheticWithStateDir(t, signal, start.Add(time.Minute), stateDir, positive),
		"mimir-series-limit",
	)
	if !strings.Contains(page.Markdown(), "discard_counter_increase=4") {
		t.Fatalf("positive row after lazy zero lost its exact delta: %s", page.Markdown())
	}

	resetAt := start.Add(2 * time.Minute)
	lazyZeroAfterPositive := map[string]mimirAdmissionSyntheticResponse{
		"metrics-a.example": {output: mimirAdmissionHostFixture(
			[]string{mimirAdmissionLazyZeroInstanceFixture(processStart, 123460, 3456)},
			true,
			0,
			0,
			0,
		)},
	}
	resetAlerts := runMimirAdmissionSyntheticWithStateDir(t, signal, resetAt, stateDir, lazyZeroAfterPositive)
	resetVisibility := requireAlertClass(t, resetAlerts, "cannot-observe")
	requireAlertClass(t, resetAlerts, "mimir-series-limit")
	if !strings.Contains(resetVisibility.Markdown(), "monotonic counter decreased within one process generation") {
		t.Fatalf("disappearing positive row was not treated as a reset: %s", resetVisibility.Markdown())
	}
	persisted = mimirAdmissionPersistedState{}
	loaded, err = loadProviderState(stateDir, "mimir-admission", mimirAdmissionStateVersion, &persisted)
	if err != nil || !loaded || !persisted.Incident || persisted.QuietSinceUnix != 0 ||
		len(persisted.Histories) != 1 || persisted.Histories[0].DiscardTotal != 0 {
		t.Fatalf("lazy-zero reset did not preserve the durable incident: loaded=%t state=%+v err=%v", loaded, persisted, err)
	}

	quietAt := resetAt.Add(time.Minute)
	requireAlertClass(
		t,
		runMimirAdmissionSyntheticWithStateDir(t, signal, quietAt, stateDir, lazyZeroAfterPositive),
		"mimir-series-limit",
	)
	if alerts := runMimirAdmissionSyntheticWithStateDir(
		t,
		signal,
		quietAt.Add(mimirAdmissionQuietWindow),
		stateDir,
		lazyZeroAfterPositive,
	); len(alerts) != 0 {
		t.Fatalf("complete lazy-zero quiet window did not clear: %+v", alerts)
	}
}

// Mimir 3.1.1 leaves all discarded-sample vectors empty after a clean start,
// so Prometheus emits neither the family nor its HELP/TYPE lines. Only the
// exact source-backed artifact contract can turn that whole-family absence
// into zero; an unknown build remains unknown.
func TestMimirAdmissionSignalTreatsSourceBackedWholeFamilyAbsenceAsLazyZero(t *testing.T) {
	stateDir := t.TempDir()
	signal := NewMimirAdmissionSignal()
	start := time.Date(2032, 1, 3, 4, 5, 0, 0, time.UTC)
	processStart := start.Add(-time.Hour).Unix()
	sourceBackedZero := map[string]mimirAdmissionSyntheticResponse{
		"metrics-a.example": {output: mimirAdmissionHostFixture(
			[]string{mimirAdmissionSourceBackedWholeFamilyZeroInstanceFixture(processStart, 123456, 3456)},
			true,
			0,
			0,
			0,
		)},
	}
	if alerts := runMimirAdmissionSyntheticWithStateDir(t, signal, start, stateDir, sourceBackedZero); len(alerts) != 0 {
		t.Fatalf("source-backed whole-family lazy zero alerted: %+v", alerts)
	}
	persisted := mimirAdmissionPersistedState{}
	loaded, err := loadProviderState(stateDir, "mimir-admission", mimirAdmissionStateVersion, &persisted)
	if err != nil || !loaded || len(persisted.Histories) != 1 || persisted.Histories[0].DiscardTotal != 0 {
		t.Fatalf("whole-family lazy-zero baseline was not persisted: loaded=%t state=%+v err=%v", loaded, persisted, err)
	}

	positive := mimirAdmissionSingleHostResponses(processStart, 4, 123460, 3456, [3]int64{})
	page := requireAlertClass(
		t,
		runMimirAdmissionSyntheticWithStateDir(t, signal, start.Add(time.Minute), stateDir, positive),
		"mimir-series-limit",
	)
	if !strings.Contains(page.Markdown(), "discard_counter_increase=4") {
		t.Fatalf("positive row after whole-family zero lost its exact delta: %s", page.Markdown())
	}

	sourceBackedZeroAfterPositive := map[string]mimirAdmissionSyntheticResponse{
		"metrics-a.example": {output: mimirAdmissionHostFixture(
			[]string{mimirAdmissionSourceBackedWholeFamilyZeroInstanceFixture(processStart, 123460, 3456)},
			true,
			0,
			0,
			0,
		)},
	}
	resetAlerts := runMimirAdmissionSyntheticWithStateDir(
		t,
		signal,
		start.Add(2*time.Minute),
		stateDir,
		sourceBackedZeroAfterPositive,
	)
	resetVisibility := requireAlertClass(t, resetAlerts, "cannot-observe")
	if !strings.Contains(resetVisibility.Markdown(), "monotonic counter decreased within one process generation") {
		t.Fatalf("whole-family disappearance after a positive was not a reset: %s", resetVisibility.Markdown())
	}
	resetPage := requireAlertClass(t, resetAlerts, "mimir-series-limit")
	if !strings.Contains(resetPage.Markdown(), "descriptor_instances=0 source_zero_instances=1") {
		t.Fatalf("source-backed zero path was not rendered independently: %s", resetPage.Markdown())
	}

	unknownInstance := strings.Replace(
		mimirAdmissionSourceBackedWholeFamilyZeroInstanceFixture(processStart, 123456, 3456),
		"discard_absence_source 1",
		"discard_absence_source 0",
		1,
	)
	unknownAlerts := runMimirAdmissionSynthetic(
		t,
		NewMimirAdmissionSignal(),
		start,
		map[string]mimirAdmissionSyntheticResponse{
			"metrics-a.example": {output: mimirAdmissionHostFixture([]string{unknownInstance}, true, 0, 0, 0)},
		},
	)
	unknownVisibility := requireAlertClass(t, unknownAlerts, "cannot-observe")
	if !strings.Contains(unknownVisibility.Markdown(), "outside the recognized source contract") {
		t.Fatalf("unknown artifact family absence did not fail closed: %s", unknownVisibility.Markdown())
	}
}

// Port plus canonical sub-second process start keeps concurrent children and
// rapid replacements distinct even when their whole-second starts coincide.
func TestMimirAdmissionSignalKeepsSameSecondChildrenIndependent(t *testing.T) {
	stateDir := t.TempDir()
	signal := NewMimirAdmissionSignal()
	start := time.Date(2032, 1, 3, 4, 5, 0, 0, time.UTC)
	processStart := "1956614700.1250"
	baseline := map[string]mimirAdmissionSyntheticResponse{
		"metrics-a.example": {output: mimirAdmissionHostFixture([]string{
			mimirAdmissionCompleteInstanceFixtureAt(19191, processStart, 0, 1000, 100),
			mimirAdmissionCompleteInstanceFixtureAt(19192, processStart, 0, 2000, 200),
		}, true, 0, 0, 0)},
	}
	if alerts := runMimirAdmissionSyntheticWithStateDir(t, signal, start, stateDir, baseline); len(alerts) != 0 {
		t.Fatalf("same-second child baseline alerted: %+v", alerts)
	}
	increased := map[string]mimirAdmissionSyntheticResponse{
		"metrics-a.example": {output: mimirAdmissionHostFixture([]string{
			mimirAdmissionCompleteInstanceFixtureAt(19191, processStart, 3, 1010, 100),
			mimirAdmissionCompleteInstanceFixtureAt(19192, processStart, 0, 2020, 201),
		}, true, 0, 0, 0)},
	}
	page := requireAlertClass(
		t,
		runMimirAdmissionSyntheticWithStateDir(t, signal, start.Add(time.Minute), stateDir, increased),
		"mimir-series-limit",
	)
	for _, required := range []string{"affected_instances=1", "discard_counter_increase=3", "created_increase=30"} {
		if !strings.Contains(page.Markdown(), required) {
			t.Errorf("same-second child delta lacks %q: %s", required, page.Markdown())
		}
	}
	persisted := mimirAdmissionPersistedState{}
	loaded, err := loadProviderState(stateDir, "mimir-admission", mimirAdmissionStateVersion, &persisted)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded || len(persisted.Histories) != 2 || persisted.Histories[0].Port == persisted.Histories[1].Port ||
		persisted.Histories[0].ProcessStart != persisted.Histories[1].ProcessStart {
		t.Fatalf("same-second child identities collapsed: %+v", persisted)
	}
	expectedStart, err := canonicalMimirAdmissionProcessStart(processStart)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Histories[0].ProcessStart != expectedStart {
		t.Fatalf("process start = %q, want canonical %q", persisted.Histories[0].ProcessStart, expectedStart)
	}
}

// Only an uninterrupted complete and comparable interval resolves a page.
func TestMimirAdmissionSignalRequiresCompleteTwoHourQuietWindow(t *testing.T) {
	signal := NewMimirAdmissionSignal()
	start := time.Date(2032, 2, 3, 4, 5, 0, 0, time.UTC)
	processStart := start.Add(-time.Hour).Unix()
	baseline := mimirAdmissionSingleHostResponses(processStart, 0, 120000, 2000, [3]int64{})
	if alerts := runMimirAdmissionSynthetic(t, signal, start, baseline); len(alerts) != 0 {
		t.Fatalf("initial zero baseline alerted: %+v", alerts)
	}

	increaseAt := start.Add(time.Minute)
	increased := mimirAdmissionSingleHostResponses(processStart, 3, 120100, 2001, [3]int64{})
	requireAlertClass(t, runMimirAdmissionSynthetic(t, signal, increaseAt, increased), "mimir-series-limit")

	quietAt := increaseAt.Add(time.Minute)
	flat := mimirAdmissionSingleHostResponses(processStart, 3, 120100, 2001, [3]int64{})
	requireAlertClass(t, runMimirAdmissionSynthetic(t, signal, quietAt, flat), "mimir-series-limit")
	requireAlertClass(
		t,
		runMimirAdmissionSynthetic(t, signal, quietAt.Add(mimirAdmissionQuietWindow-time.Second), flat),
		"mimir-series-limit",
	)
	if alerts := runMimirAdmissionSynthetic(t, signal, quietAt.Add(mimirAdmissionQuietWindow), flat); len(alerts) != 0 {
		t.Fatalf("complete two-hour quiet window did not clear: %+v", alerts)
	}
}

// A fresh watcher preserves the incident and begins a new complete quiet hold.
func TestMimirAdmissionSignalRestartCannotClearPersistedIncident(t *testing.T) {
	stateDir := t.TempDir()
	start := time.Date(2032, 2, 4, 5, 6, 0, 0, time.UTC)
	processStart := start.Add(-time.Hour).Unix()
	signal := NewMimirAdmissionSignal()
	baseline := mimirAdmissionSingleHostResponses(processStart, 0, 125000, 2500, [3]int64{})
	if alerts := runMimirAdmissionSyntheticWithStateDir(t, signal, start, stateDir, baseline); len(alerts) != 0 {
		t.Fatalf("persisted zero baseline alerted: %+v", alerts)
	}
	positiveAt := start.Add(time.Minute)
	positive := mimirAdmissionSingleHostResponses(processStart, 4, 125100, 2501, [3]int64{})
	requireAlertClass(
		t,
		runMimirAdmissionSyntheticWithStateDir(t, signal, positiveAt, stateDir, positive),
		"mimir-series-limit",
	)
	resetAt := positiveAt.Add(time.Minute)
	reset := mimirAdmissionSingleHostResponses(processStart, 0, 125100, 2501, [3]int64{})
	resetAlerts := runMimirAdmissionSyntheticWithStateDir(t, signal, resetAt, stateDir, reset)
	requireAlertClass(t, resetAlerts, "cannot-observe")
	requireAlertClass(t, resetAlerts, "mimir-series-limit")
	quietAt := resetAt.Add(time.Minute)
	requireAlertClass(
		t,
		runMimirAdmissionSyntheticWithStateDir(t, signal, quietAt, stateDir, reset),
		"mimir-series-limit",
	)

	persisted := mimirAdmissionPersistedState{}
	loaded, err := loadProviderState(stateDir, "mimir-admission", mimirAdmissionStateVersion, &persisted)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded || !persisted.Incident || persisted.QuietSinceUnix != quietAt.Unix() || len(persisted.Histories) != 1 {
		t.Fatalf("durable incident state = %+v, loaded=%t", persisted, loaded)
	}

	restartAt := quietAt.Add(mimirAdmissionQuietWindow)
	restarted := NewMimirAdmissionSignal()
	restartedAlert := requireAlertClass(
		t,
		runMimirAdmissionSyntheticWithStateDir(t, restarted, restartAt, stateDir, reset),
		"mimir-series-limit",
	)
	if !strings.Contains(restartedAlert.Markdown(), "quiet_complete=0s") {
		t.Fatalf("watcher restart reused unobserved quiet time: %s", restartedAlert.Markdown())
	}
	if alerts := runMimirAdmissionSyntheticWithStateDir(
		t,
		restarted,
		restartAt.Add(mimirAdmissionQuietWindow),
		stateDir,
		reset,
	); len(alerts) != 0 {
		t.Fatalf("fresh post-restart quiet window did not clear: %+v", alerts)
	}
}

// Alternating watcher generations must reload under the shared lock instead
// of overwriting a newer incident with state cached before that incident.
func TestMimirAdmissionSignalAlternatingWatchersReloadLatestState(t *testing.T) {
	stateDir := t.TempDir()
	start := time.Date(2032, 2, 5, 6, 7, 0, 0, time.UTC)
	processStart := start.Add(-time.Hour).Unix()
	first := NewMimirAdmissionSignal()
	second := NewMimirAdmissionSignal()
	baseline := mimirAdmissionSingleHostResponses(processStart, 0, 126000, 2600, [3]int64{})
	if alerts := runMimirAdmissionSyntheticWithStateDir(t, first, start, stateDir, baseline); len(alerts) != 0 {
		t.Fatalf("first watcher baseline alerted: %+v", alerts)
	}
	if alerts := runMimirAdmissionSyntheticWithStateDir(t, second, start.Add(time.Minute), stateDir, baseline); len(alerts) != 0 {
		t.Fatalf("second watcher baseline alerted: %+v", alerts)
	}
	positive := mimirAdmissionSingleHostResponses(processStart, 6, 126010, 2601, [3]int64{})
	requireAlertClass(
		t,
		runMimirAdmissionSyntheticWithStateDir(t, first, start.Add(2*time.Minute), stateDir, positive),
		"mimir-series-limit",
	)
	flatAt := start.Add(3 * time.Minute)
	requireAlertClass(
		t,
		runMimirAdmissionSyntheticWithStateDir(t, second, flatAt, stateDir, positive),
		"mimir-series-limit",
	)
	requireAlertClass(
		t,
		runMimirAdmissionSyntheticWithStateDir(t, first, flatAt.Add(time.Minute), stateDir, positive),
		"mimir-series-limit",
	)
	persisted := mimirAdmissionPersistedState{}
	loaded, err := loadProviderState(stateDir, "mimir-admission", mimirAdmissionStateVersion, &persisted)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded || !persisted.Incident || persisted.QuietSinceUnix != flatAt.Unix() ||
		len(persisted.Histories) != 1 || persisted.Histories[0].DiscardTotal != 6 {
		t.Fatalf("alternating watcher overwrote latest state: %+v, loaded=%t", persisted, loaded)
	}
}

// A failed atomic save cannot commit an in-memory clear or baseline advance.
func TestMimirAdmissionSignalSaveFailurePreservesDurableIncident(t *testing.T) {
	stateDir := t.TempDir()
	start := time.Date(2032, 2, 6, 7, 8, 0, 0, time.UTC)
	processStart := start.Add(-time.Hour).Unix()
	signal := NewMimirAdmissionSignal()
	baseline := mimirAdmissionSingleHostResponses(processStart, 0, 127000, 2700, [3]int64{})
	if alerts := runMimirAdmissionSyntheticWithStateDir(t, signal, start, stateDir, baseline); len(alerts) != 0 {
		t.Fatalf("save-failure baseline alerted: %+v", alerts)
	}
	positive := mimirAdmissionSingleHostResponses(processStart, 5, 127010, 2701, [3]int64{})
	requireAlertClass(
		t,
		runMimirAdmissionSyntheticWithStateDir(t, signal, start.Add(time.Minute), stateDir, positive),
		"mimir-series-limit",
	)
	quietAt := start.Add(2 * time.Minute)
	requireAlertClass(
		t,
		runMimirAdmissionSyntheticWithStateDir(t, signal, quietAt, stateDir, positive),
		"mimir-series-limit",
	)
	before := mimirAdmissionPersistedState{}
	loaded, err := loadProviderState(stateDir, "mimir-admission", mimirAdmissionStateVersion, &before)
	if err != nil || !loaded {
		t.Fatalf("load pre-failure state: loaded=%t err=%v", loaded, err)
	}
	probe := signal.(*signalAdapter).probe.(*mimirAdmissionProbe)
	probe.saveProviderState = func(string, string, int, any) error {
		return fmt.Errorf("generated save failure")
	}
	failureAlerts := runMimirAdmissionSyntheticWithStateDir(
		t,
		signal,
		quietAt.Add(mimirAdmissionQuietWindow),
		stateDir,
		positive,
	)
	requireAlertClass(t, failureAlerts, "cannot-observe")
	requireAlertClass(t, failureAlerts, "mimir-series-limit")
	after := mimirAdmissionPersistedState{}
	loaded, err = loadProviderState(stateDir, "mimir-admission", mimirAdmissionStateVersion, &after)
	if err != nil || !loaded {
		t.Fatalf("load post-failure state: loaded=%t err=%v", loaded, err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("failed save advanced durable state:\nbefore=%+v\nafter=%+v", before, after)
	}
	probe.saveProviderState = nil
	restarted := NewMimirAdmissionSignal()
	restartPage := requireAlertClass(
		t,
		runMimirAdmissionSyntheticWithStateDir(
			t,
			restarted,
			quietAt.Add(mimirAdmissionQuietWindow+time.Minute),
			stateDir,
			positive,
		),
		"mimir-series-limit",
	)
	if !strings.Contains(restartPage.Markdown(), "quiet_complete=0s") {
		t.Fatalf("failed save became a clearing event: %s", restartPage.Markdown())
	}
}

// State lock and validation failures retain an incident already known by this
// process without changing its baselines or quiet boundary.
func TestMimirAdmissionSignalEarlyStateFailuresRetainMaturePage(t *testing.T) {
	stateDir := t.TempDir()
	start := time.Date(2032, 2, 7, 8, 9, 0, 0, time.UTC)
	processStart := start.Add(-time.Hour).Unix()
	signal := NewMimirAdmissionSignal()
	positive := mimirAdmissionSingleHostResponses(processStart, 8, 128000, 2800, [3]int64{})
	requireAlertClass(
		t,
		runMimirAdmissionSyntheticWithStateDir(t, signal, start, stateDir, positive),
		"mimir-series-limit",
	)
	requireAlertClass(
		t,
		runMimirAdmissionSyntheticWithStateDir(t, signal, start.Add(time.Minute), stateDir, positive),
		"mimir-series-limit",
	)
	probe := signal.(*signalAdapter).probe.(*mimirAdmissionProbe)
	wantState := probe.snapshotState()
	if !wantState.incident || wantState.quietSince.IsZero() || len(wantState.histories) != 1 {
		t.Fatalf("mature synthetic incident was not seeded: %+v", wantState)
	}

	var commands atomic.Int64
	settings := syntheticSettings(&syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		commands.Add(1)
		return "", fmt.Errorf("generated command must not run")
	}})
	settings.Environment = "synthetic-environment"
	settings.StateDir = stateDir
	settings.Now = func() time.Time { return start.Add(2 * time.Minute) }
	settings.Hosts = []HostSettings{{Name: "metrics-a.example", Roles: []string{"services"}}}

	probe.lockProviderState = func(context.Context, string, string) (*providerStateLock, error) {
		return nil, fmt.Errorf("generated lock failure")
	}
	lockAlerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, lockAlerts, "cannot-observe")
	requireAlertClass(t, lockAlerts, "mimir-series-limit")
	if gotState := probe.snapshotState(); !reflect.DeepEqual(gotState, wantState) {
		t.Fatalf("lock failure changed retained state:\nwant=%+v\ngot=%+v", wantState, gotState)
	}
	probe.lockProviderState = nil

	processIdentity, err := canonicalMimirAdmissionProcessStart("1959638400.5")
	if err != nil {
		t.Fatal(err)
	}
	invalid := mimirAdmissionPersistedState{
		Incident:       true,
		QuietSinceUnix: wantState.quietSince.Unix(),
		Histories:      make([]mimirAdmissionPersistedHistory, mimirAdmissionHistoryLimit+1),
	}
	for offset := range invalid.Histories {
		invalid.Histories[offset] = mimirAdmissionPersistedHistory{
			Host:         "generated-metrics.example",
			Port:         20000 + offset,
			ProcessStart: processIdentity,
			DiscardTotal: 8,
			CreatedTotal: 128000 + int64(offset),
			RemovedTotal: 2800 + int64(offset),
		}
	}
	if err := saveProviderState(stateDir, "mimir-admission", mimirAdmissionStateVersion, invalid); err != nil {
		t.Fatal(err)
	}
	settings.Now = func() time.Time { return start.Add(3 * time.Minute) }
	loadAlerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	visibility := requireAlertClass(t, loadAlerts, "cannot-observe")
	page := requireAlertClass(t, loadAlerts, "mimir-series-limit")
	if !strings.Contains(visibility.Markdown(), "durable state is unreadable") ||
		!strings.Contains(page.Markdown(), "direct_complete=false") {
		t.Fatalf("early load failure lost fixed retained boundary:\nvisibility=%s\npage=%s", visibility.Markdown(), page.Markdown())
	}
	if gotState := probe.snapshotState(); !reflect.DeepEqual(gotState, wantState) {
		t.Fatalf("invalid durable reload changed retained state:\nwant=%+v\ngot=%+v", wantState, gotState)
	}
	if commands.Load() != 0 {
		t.Fatalf("early state failures ran %d host commands", commands.Load())
	}
}

// A partial descriptor and malformed bounded frames remain unknown even for a
// source-recognized artifact; the lazy-family contract covers exact absence.
func TestMimirAdmissionSignalKeepsDescriptorLossAndMalformedFramesUnknown(t *testing.T) {
	signal := NewMimirAdmissionSignal()
	start := time.Date(2032, 3, 4, 5, 6, 0, 0, time.UTC)
	processStart := start.Add(-time.Hour).Unix()
	positive := mimirAdmissionSingleHostResponses(processStart, 5, 130000, 3000, [3]int64{})
	requireAlertClass(t, runMimirAdmissionSynthetic(t, signal, start, positive), "mimir-series-limit")

	descriptorMissing := mimirAdmissionInstanceFixture(mimirAdmissionFixtureOptions{
		port:                 mimirAdmissionSyntheticPort,
		observable:           true,
		processStart:         strconv.FormatInt(processStart, 10),
		memorySeries:         111111,
		activeSeries:         55555,
		createdTotal:         130000,
		removedTotal:         3000,
		localLimit:           mimirAdmissionSyntheticLocalLimit,
		globalLimit:          mimirAdmissionSyntheticGlobalLimit,
		discardDescriptor:    false,
		discardFamilyAbsent:  false,
		discardAbsenceSource: true,
		discardPresent:       false,
		discardTotal:         0,
	})
	descriptorAlerts := runMimirAdmissionSynthetic(
		t,
		signal,
		start.Add(3*time.Hour),
		map[string]mimirAdmissionSyntheticResponse{
			"metrics-a.example": {output: mimirAdmissionHostFixture([]string{descriptorMissing}, true, 0, 0, 0)},
		},
	)
	descriptorVisibility := requireAlertClass(t, descriptorAlerts, "cannot-observe")
	requireAlertClass(t, descriptorAlerts, "mimir-series-limit")
	if !strings.Contains(descriptorVisibility.Markdown(), "discard counter descriptor is unavailable") {
		t.Fatalf("descriptor absence lost its boundary: %s", descriptorVisibility.Markdown())
	}

	malformed := strings.Replace(
		mimirAdmissionSingleHostResponses(processStart, 5, 130000, 3000, [3]int64{})["metrics-a.example"].output,
		"memory_series 111111\n",
		"",
		1,
	)
	malformedAlerts := runMimirAdmissionSynthetic(
		t,
		signal,
		start.Add(4*time.Hour),
		map[string]mimirAdmissionSyntheticResponse{
			"metrics-a.example": {output: malformed},
		},
	)
	requireAlertClass(t, malformedAlerts, "cannot-observe")
	requireAlertClass(t, malformedAlerts, "mimir-series-limit")

	flatAt := start.Add(4*time.Hour + time.Minute)
	flat := mimirAdmissionSingleHostResponses(processStart, 5, 130000, 3000, [3]int64{})
	requireAlertClass(t, runMimirAdmissionSynthetic(t, signal, flatAt, flat), "mimir-series-limit")
	if alerts := runMimirAdmissionSynthetic(t, signal, flatAt.Add(mimirAdmissionQuietWindow), flat); len(alerts) != 0 {
		t.Fatalf("fresh complete quiet window after unknown state did not clear: %+v", alerts)
	}
}

// A generation transition or decreasing monotonic counter interrupts closure.
func TestMimirAdmissionSignalGenerationAndCounterResetNeverClearIncident(t *testing.T) {
	signal := NewMimirAdmissionSignal()
	start := time.Date(2032, 4, 5, 6, 7, 0, 0, time.UTC)
	firstProcess := start.Add(-time.Hour).Unix()
	baseline := mimirAdmissionSingleHostResponses(firstProcess, 0, 140000, 4000, [3]int64{})
	if alerts := runMimirAdmissionSynthetic(t, signal, start, baseline); len(alerts) != 0 {
		t.Fatalf("initial zero baseline alerted: %+v", alerts)
	}
	positive := mimirAdmissionSingleHostResponses(firstProcess, 4, 140100, 4001, [3]int64{})
	requireAlertClass(t, runMimirAdmissionSynthetic(t, signal, start.Add(time.Minute), positive), "mimir-series-limit")

	secondProcess := start.Add(2 * time.Hour).Unix()
	replacement := mimirAdmissionSingleHostResponses(secondProcess, 0, 500, 20, [3]int64{})
	replaced := requireAlertClass(
		t,
		runMimirAdmissionSynthetic(t, signal, start.Add(4*time.Hour), replacement),
		"mimir-series-limit",
	)
	for _, required := range []string{"generation_changes=1", "comparable=false", "quiet_complete=0s"} {
		if !strings.Contains(replaced.Markdown(), required) {
			t.Errorf("generation replacement lost %q: %s", required, replaced.Markdown())
		}
	}

	quietStart := start.Add(4*time.Hour + time.Minute)
	requireAlertClass(t, runMimirAdmissionSynthetic(t, signal, quietStart, replacement), "mimir-series-limit")
	decreased := mimirAdmissionSingleHostResponses(secondProcess, 0, 499, 19, [3]int64{})
	resetAlerts := runMimirAdmissionSynthetic(t, signal, quietStart.Add(time.Hour), decreased)
	requireAlertClass(t, resetAlerts, "mimir-series-limit")
	resetVisibility := requireAlertClass(t, resetAlerts, "cannot-observe")
	if !strings.Contains(resetVisibility.Markdown(), "monotonic counter decreased within one process generation") {
		t.Fatalf("counter reset lost its boundary: %s", resetVisibility.Markdown())
	}
	requireAlertClass(
		t,
		runMimirAdmissionSynthetic(t, signal, quietStart.Add(4*time.Hour), decreased),
		"mimir-series-limit",
	)
}

// An unknown host cannot suppress a confirmed positive sibling counter.
func TestMimirAdmissionSignalConfirmedSiblingStillPagesThroughUnknownHost(t *testing.T) {
	signal := NewMimirAdmissionSignal()
	now := time.Date(2032, 5, 6, 7, 8, 0, 0, time.UTC)
	privateSyntheticMarker := "generated-private-fragment-must-not-leave"
	responses := mimirAdmissionSingleHostResponses(now.Add(-time.Hour).Unix(), 9, 150000, 5000, [3]int64{})
	responses["metrics-b.example"] = mimirAdmissionSyntheticResponse{
		err: fmt.Errorf("synthetic transport failure %s", privateSyntheticMarker),
	}
	alerts := runMimirAdmissionSynthetic(t, signal, now, responses)
	page := requireAlertClass(t, alerts, "mimir-series-limit")
	visibility := requireAlertClass(t, alerts, "cannot-observe")
	if visibility.Target != "metrics-b.example/mimir-admission" {
		t.Fatalf("wrong unknown sibling target: %+v", visibility)
	}
	for _, required := range []string{
		"configured_hosts=2",
		"observable_hosts=1",
		"affected_instances=1",
		"direct_complete=false",
	} {
		if !strings.Contains(page.Markdown(), required) {
			t.Errorf("partial-fleet page lacks %q: %s", required, page.Markdown())
		}
	}
	requireAlertOmits(t, page, privateSyntheticMarker)
	requireAlertOmits(t, visibility, privateSyntheticMarker)
}

// Journal aggregates provide context without selecting the classification.
func TestMimirAdmissionSignalJournalContextDoesNotChangeRootCause(t *testing.T) {
	start := time.Date(2032, 6, 7, 8, 9, 0, 0, time.UTC)
	processStart := start.Add(-time.Hour).Unix()
	contexts := [][3]int64{{0, 0, 0}, {21, 22, 23}}
	alerts := make([]Alert, 0, len(contexts))
	for _, contextValues := range contexts {
		signal := NewMimirAdmissionSignal()
		baseline := mimirAdmissionSingleHostResponses(processStart, 0, 160000, 6000, contextValues)
		if initial := runMimirAdmissionSynthetic(t, signal, start, baseline); len(initial) != 0 {
			t.Fatalf("counterfactual baseline alerted: %+v", initial)
		}
		positive := mimirAdmissionSingleHostResponses(processStart, 2, 160010, 6000, contextValues)
		alerts = append(alerts, requireAlertClass(
			t,
			runMimirAdmissionSynthetic(t, signal, start.Add(time.Minute), positive),
			"mimir-series-limit",
		))
	}
	if alerts[0].Class != alerts[1].Class || alerts[0].Frame != alerts[1].Frame ||
		alerts[0].Mechanism != alerts[1].Mechanism || alerts[0].Action != alerts[1].Action {
		t.Fatalf("journal context changed direct root cause:\nzero=%+v\nnonzero=%+v", alerts[0], alerts[1])
	}
	if !strings.Contains(alerts[0].Observed, "publisher_starts=0 readiness_rejects=0 admission_rejects=0") ||
		!strings.Contains(alerts[1].Observed, "publisher_starts=21 readiness_rejects=22 admission_rejects=23") {
		t.Fatalf("counterfactual context was not retained:\nzero=%s\nnonzero=%s", alerts[0].Observed, alerts[1].Observed)
	}
}

// Duplicate, incomplete, invalid, and trailing fields all fail closed.
func TestParseMimirAdmissionHostSampleRejectsAdversarialFrames(t *testing.T) {
	processStart := time.Date(2032, 7, 8, 9, 10, 0, 0, time.UTC).Unix()
	valid := mimirAdmissionHostFixture(
		[]string{mimirAdmissionCompleteInstanceFixture(processStart, 0, 170000, 7000)},
		true,
		0,
		0,
		0,
	)
	if _, err := parseMimirAdmissionHostSample(valid); err != nil {
		t.Fatalf("valid strict frame failed: %v", err)
	}
	cases := []struct {
		name   string
		input  string
		needle string
	}{
		{
			name:   "duplicate fixed field",
			input:  strings.Replace(valid, "memory_series 111111", "memory_series 111111\nmemory_series 111111", 1),
			needle: "duplicate memory_series",
		},
		{
			name:   "missing exact reason field",
			input:  strings.Replace(valid, "discard_present 1\n", "", 1),
			needle: "instance omitted discard_present",
		},
		{
			name:   "invalid numeric field",
			input:  strings.Replace(valid, fmt.Sprintf("process_start %d", processStart), "process_start not-a-number", 1),
			needle: "invalid process_start",
		},
		{
			name:   "count mismatch",
			input:  strings.Replace(valid, "mimir_count 1", "mimir_count 2", 1),
			needle: "invalid instance count",
		},
		{
			name: "unobservable frame with metric data",
			input: mimirAdmissionHostFixture(
				[]string{fmt.Sprintf(
					"instance_begin %d\nobservable 0\nprocess_start %d\ninstance_end\n",
					mimirAdmissionSyntheticPort,
					processStart,
				)},
				true,
				0,
				0,
				0,
			),
			needle: "unobservable instance contains metric fields",
		},
		{
			name:   "absent exact row with positive total",
			input:  strings.Replace(strings.Replace(valid, "discard_present 1", "discard_present 0", 1), "discard_total 0", "discard_total 8", 1),
			needle: "absent discard row has a nonzero total",
		},
		{
			name:   "descriptor contradicts absent family",
			input:  strings.Replace(valid, "discard_family_absent 0", "discard_family_absent 1", 1),
			needle: "present descriptor contradicts absent family",
		},
		{
			name: "absent family contains exact row",
			input: strings.Replace(
				strings.Replace(valid, "discard_descriptor 1", "discard_descriptor 0", 1),
				"discard_family_absent 0",
				"discard_family_absent 1",
				1,
			),
			needle: "absent family contains an exact counter row",
		},
		{
			name:   "incomplete journal with retained count",
			input:  strings.Replace(strings.Replace(valid, "journal_complete 1", "journal_complete 0", 1), "publisher_starts 0", "publisher_starts 1", 1),
			needle: "incomplete journal context contains counts",
		},
		{
			name:   "trailing fragment",
			input:  valid + "generated_trailing_fragment 1\n",
			needle: "unknown field",
		},
	}
	for _, candidate := range cases {
		_, err := parseMimirAdmissionHostSample(candidate.input)
		if err == nil || !strings.Contains(err.Error(), candidate.needle) {
			t.Errorf("%s parse error = %v, want %q", candidate.name, err, candidate.needle)
		}
	}
}

// The exact boundary is accepted, while one additional current child fails
// closed before any durable baseline can be replaced.
func TestMimirAdmissionSignalBoundsCurrentChildIdentities(t *testing.T) {
	stateDir := t.TempDir()
	signal := NewMimirAdmissionSignal()
	start := time.Date(2032, 7, 9, 10, 11, 0, 0, time.UTC)
	instances := make([]string, 0, mimirAdmissionHistoryLimit+1)
	for offset := 0; offset < mimirAdmissionHistoryLimit; offset++ {
		instances = append(instances, mimirAdmissionCompleteInstanceFixtureAt(
			20000+offset,
			"1957046400.25",
			0,
			180000+int64(offset),
			8000+int64(offset),
		))
	}
	boundary := map[string]mimirAdmissionSyntheticResponse{
		"metrics-a.example": {output: mimirAdmissionHostFixture(instances, true, 0, 0, 0)},
	}
	if alerts := runMimirAdmissionSyntheticWithStateDir(t, signal, start, stateDir, boundary); len(alerts) != 0 {
		t.Fatalf("identity boundary alerted: %+v", alerts)
	}
	before := mimirAdmissionPersistedState{}
	loaded, err := loadProviderState(stateDir, "mimir-admission", mimirAdmissionStateVersion, &before)
	if err != nil || !loaded || len(before.Histories) != mimirAdmissionHistoryLimit {
		t.Fatalf("identity boundary state: loaded=%t histories=%d err=%v", loaded, len(before.Histories), err)
	}
	overBound := append(append([]string(nil), instances...), mimirAdmissionCompleteInstanceFixtureAt(
		20000+mimirAdmissionHistoryLimit,
		"1957046400.75",
		9,
		190000,
		9000,
	))
	alerts := runMimirAdmissionSyntheticWithStateDir(
		t,
		signal,
		start.Add(time.Minute),
		stateDir,
		map[string]mimirAdmissionSyntheticResponse{
			"metrics-a.example": {output: mimirAdmissionHostFixture(overBound, true, 0, 0, 0)},
		},
	)
	visibility := requireAlertClass(t, alerts, "cannot-observe")
	if !strings.Contains(visibility.Markdown(), "current child identity bound exceeded") {
		t.Fatalf("over-bound observation lost fixed cause: %s", visibility.Markdown())
	}
	after := mimirAdmissionPersistedState{}
	loaded, err = loadProviderState(stateDir, "mimir-admission", mimirAdmissionStateVersion, &after)
	if err != nil || !loaded {
		t.Fatalf("load state after over-bound tick: loaded=%t err=%v", loaded, err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatal("over-bound observation replaced durable state")
	}
}

// The shell reducer emits only exact aggregates from documentation-only hosts.
func TestMimirAdmissionScriptReducesExactMetricsAndJournalContext(t *testing.T) {
	binDir := t.TempDir()
	writeSyntheticExecutable := func(name string, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeSyntheticExecutable("ss", `#!/bin/sh
printf '%s\n' \
  'LISTEN 0 4096 192.0.2.10:19191 198.51.100.20:*' \
  'LISTEN 0 4096 192.0.2.10:29292 [2001:db8::20]:*'
`)
	writeSyntheticExecutable("curl", `#!/bin/sh
case "${*}" in
  *':19191/api/v1/status/buildinfo')
	case "${MIMIR_ADMISSION_TEST_SHAPE:-positive}" in
	  future-whole-family-zero)
		printf '%s\n' '{"application":"Grafana Mimir","version":"9.9.9","revision":"feedface00"}'
		;;
	  duplicate-source-whole-family-zero)
		printf '%s\n' '{"application":"Grafana Mimir","version":"3.1.1","version":"3.1.1","revision":"a3d6c90f25"}'
		;;
	  *)
		printf '%s\n' '{"application":"Grafana Mimir","version":"3.1.1","revision":"a3d6c90f25"}'
		;;
	esac
	;;
  *':29292/api/v1/status/buildinfo')
    printf '%s\n' '{"application":"Synthetic Other"}'
    ;;
  *':19191/config')
    printf '%s\n' \
      'limits:' \
      '  max_global_series_per_user: 777777' \
      'storage:' \
      '  synthetic_marker: generated-config-value-must-not-leave'
    ;;
  *':19191/metrics')
	printf '%s\n' \
	  'process_start_time_seconds 1956528000.125' \
      'cortex_ingester_memory_series 111111' \
      'cortex_ingester_active_series{instance="generated-instance-a"} 22222' \
      'cortex_ingester_active_series{instance="generated-instance-b"} 33333' \
      'cortex_ingester_memory_series_created_total{instance="generated-instance-a"} 123456' \
      'cortex_ingester_memory_series_created_total{instance="generated-instance-b"} 234567' \
      'cortex_ingester_memory_series_removed_total{instance="generated-instance-a"} 3456' \
      'cortex_ingester_memory_series_removed_total{instance="generated-instance-b"} 4567' \
      'cortex_ingester_local_limits{limit="max_global_series_per_user",source="metrics-a.example"} 333333'
	case "${MIMIR_ADMISSION_TEST_SHAPE:-positive}" in
	  positive)
		printf '%s\n' \
		  '# HELP cortex_discarded_samples_total Synthetic discarded samples.' \
		  '# TYPE cortex_discarded_samples_total counter' \
		  'cortex_discarded_samples_total{reason="per_user_series_limit",source="generated-source-a"} 2' \
		  'cortex_discarded_samples_total{reason="per_user_series_limit",source="generated-source-b"} 5'
		;;
	  lazy-zero)
		printf '%s\n' \
		  '# HELP cortex_discarded_samples_total Synthetic discarded samples.' \
		  '# TYPE cortex_discarded_samples_total counter' \
		  'cortex_discarded_samples_total{reason="generated_other_reason",source="generated-source-c"} 11'
		;;
	  malformed-exact)
		printf '%s\n' \
		  '# HELP cortex_discarded_samples_total Synthetic discarded samples.' \
		  '# TYPE cortex_discarded_samples_total counter' \
		  'cortex_discarded_samples_total{reason="per_user_series_limit",source="generated-source-d"} generated-malformed-value'
		;;
	  malformed-descriptor)
		printf '%s\n' \
		  '# HELP cortex_discarded_samples_total Synthetic discarded samples.'
		;;
	  whole-family-zero|future-whole-family-zero|duplicate-source-whole-family-zero)
		;;
	  *) exit 3 ;;
	esac
    ;;
  *) exit 2 ;;
esac
`)
	writeSyntheticExecutable("journalctl", `#!/bin/sh
printf '%s\n' \
  '[stats]publishing generated-publisher-a' \
  '[api]not ready generated-candidate-a' \
  'Stats push rejected (400): per-user series limit; source=generated-source-a'
`)
	writeSyntheticExecutable("timeout", `#!/bin/sh
shift
exec "$@"
`)

	runReducer := func(shape string) (mimirAdmissionHostSample, []byte) {
		t.Helper()
		command := exec.Command("sh", "-c", mimirAdmissionScript("192.0.2.10"))
		command.Env = append(
			os.Environ(),
			"PATH="+binDir+":"+os.Getenv("PATH"),
			"journal_identifiers=warp|synthetic-environment|api|generated-api-a",
			"MIMIR_ADMISSION_TEST_SHAPE="+shape,
		)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("Mimir admission reducer (%s): %v\n%s", shape, err, output)
		}
		sample, err := parseMimirAdmissionHostSample(string(output))
		if err != nil {
			t.Fatalf("parse Mimir admission reducer output (%s): %v\n%s", shape, err, output)
		}
		return sample, output
	}

	sample, output := runReducer("positive")
	if sample.count != 1 || len(sample.instances) != 1 || !sample.journalComplete ||
		sample.publisherStarts != 1 || sample.readinessRejects != 1 || sample.admissionRejects != 1 {
		t.Fatalf("reducer lost host aggregates: %+v\n%s", sample, output)
	}
	instance := sample.instances[0]
	expectedProcessStart, err := canonicalMimirAdmissionProcessStart("1956528000.125")
	if err != nil {
		t.Fatal(err)
	}
	if instance.port != mimirAdmissionSyntheticPort || instance.processStart != expectedProcessStart ||
		instance.memorySeries != 111111 || instance.activeSeries != 55555 ||
		instance.createdTotal != 358023 || instance.removedTotal != 8023 ||
		instance.localLimit != mimirAdmissionSyntheticLocalLimit ||
		instance.globalLimit != mimirAdmissionSyntheticGlobalLimit ||
		!instance.discardDescriptor || instance.discardFamilyAbsent || !instance.discardAbsenceSource ||
		!instance.discardPresent || instance.discardTotal != 7 {
		t.Fatalf("reducer lost exact child metrics: %+v\n%s", instance, output)
	}
	for _, forbidden := range []string{
		mimirAdmissionLazyFamilyVersion,
		mimirAdmissionLazyFamilySourceRevision,
		"generated-config-value-must-not-leave",
		"generated-instance-a",
		"generated-source-a",
		"generated-publisher-a",
		"generated-candidate-a",
	} {
		if strings.Contains(string(output), forbidden) {
			t.Errorf("reducer output leaked synthetic fixture marker %q: %s", forbidden, output)
		}
	}

	lazySample, lazyOutput := runReducer("lazy-zero")
	if lazySample.count != 1 || len(lazySample.instances) != 1 ||
		!lazySample.instances[0].observable || !lazySample.instances[0].discardDescriptor ||
		lazySample.instances[0].discardFamilyAbsent || !lazySample.instances[0].discardAbsenceSource ||
		lazySample.instances[0].discardPresent || lazySample.instances[0].discardTotal != 0 {
		t.Fatalf("reducer did not preserve descriptor-backed lazy zero: %+v\n%s", lazySample, lazyOutput)
	}
	wholeFamilySample, wholeFamilyOutput := runReducer("whole-family-zero")
	if wholeFamilySample.count != 1 || len(wholeFamilySample.instances) != 1 ||
		!wholeFamilySample.instances[0].observable || wholeFamilySample.instances[0].discardDescriptor ||
		!wholeFamilySample.instances[0].discardFamilyAbsent || !wholeFamilySample.instances[0].discardAbsenceSource ||
		wholeFamilySample.instances[0].discardPresent || wholeFamilySample.instances[0].discardTotal != 0 {
		t.Fatalf("reducer lost source-backed whole-family zero: %+v\n%s", wholeFamilySample, wholeFamilyOutput)
	}
	for _, shape := range []string{"future-whole-family-zero", "duplicate-source-whole-family-zero"} {
		unknownSample, unknownOutput := runReducer(shape)
		if unknownSample.count != 1 || len(unknownSample.instances) != 1 ||
			!unknownSample.instances[0].observable || unknownSample.instances[0].discardDescriptor ||
			!unknownSample.instances[0].discardFamilyAbsent || unknownSample.instances[0].discardAbsenceSource {
			t.Fatalf("reducer admitted unknown source contract (%s): %+v\n%s", shape, unknownSample, unknownOutput)
		}
	}
	malformedDescriptorSample, malformedDescriptorOutput := runReducer("malformed-descriptor")
	if malformedDescriptorSample.count != 1 || len(malformedDescriptorSample.instances) != 1 ||
		!malformedDescriptorSample.instances[0].observable || malformedDescriptorSample.instances[0].discardDescriptor ||
		malformedDescriptorSample.instances[0].discardFamilyAbsent || !malformedDescriptorSample.instances[0].discardAbsenceSource {
		t.Fatalf("reducer confused malformed descriptor with whole-family absence: %+v\n%s", malformedDescriptorSample, malformedDescriptorOutput)
	}
	malformedSample, malformedOutput := runReducer("malformed-exact")
	if malformedSample.count != 1 || len(malformedSample.instances) != 1 || malformedSample.instances[0].observable {
		t.Fatalf("reducer accepted malformed exact counter row: %+v\n%s", malformedSample, malformedOutput)
	}
	if strings.Contains(string(malformedOutput), "generated-malformed-value") {
		t.Fatalf("reducer leaked malformed synthetic source text: %s", malformedOutput)
	}
}

// The production command discovers children and restricts journal selectors.
func TestMimirAdmissionCommandUsesBoundedDependencySafeSources(t *testing.T) {
	command := mimirAdmissionCommand("synthetic-environment", map[string][]string{
		"api":        {"generated-api-b", "generated-api-a"},
		"connect":    {"generated-connect-a"},
		"taskworker": {"generated-worker-a"},
		"unrelated":  {"generated-unrelated-a"},
	})
	for _, required := range []string{
		mimirAdmissionMarker,
		"ss -ltnH",
		"/api/v1/status/buildinfo",
		"/config",
		"/metrics",
		"cortex_discarded_samples_total",
		"reason=\"per_user_series_limit\"",
		"lazy_family_version='" + mimirAdmissionLazyFamilyVersion + "'",
		"lazy_family_source_revision='" + mimirAdmissionLazyFamilySourceRevision + "'",
		"discard_family_absent",
		"discard_absence_source",
		"journal_complete",
		"admission_rejects",
		"warp|synthetic-environment|api|generated-api-a",
		"warp|synthetic-environment|api|generated-api-b",
		"warp|synthetic-environment|connect|generated-connect-a",
		"warp|synthetic-environment|taskworker|generated-worker-a",
	} {
		if !strings.Contains(command, required) {
			t.Errorf("Mimir admission command lacks %q", required)
		}
	}
	if strings.Contains(command, "generated-unrelated-a") {
		t.Fatalf("Mimir admission command included unrelated log service: %s", command)
	}
}

// Every public identifier resolves to the same single registry entry.
func TestMimirAdmissionSignalIsUniquelyRegistered(t *testing.T) {
	selected, err := IncludeSignals(
		NewSignals(),
		"11.20a",
		"mimir-admission",
		"observability/mimir-admission",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0].Key() != "mimir-admission" {
		t.Fatalf("Mimir admission registry selection = %+v", selected)
	}
}
