package monitor

// SIGNALS.md §31.2 (OPS-M6-01) on fixture rows as the monitor's read-only session returns them: an interface
// not current for a version past map.stale_after_s pages, and so does a custom host's version pending past it;
// a young version does not; a state that does not read is a visibility alert; no finding names a custom host; and
// FS-50's map-stale case at 02:00 on the fake clock, which is FS-122's paged clause, reaches the rota's sink. Nothing
// here sleeps: time moves by hand.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server/pager"
)

// The synthetic interfaces, as lbmap names them, and a custom host a version added.
const (
	lbMapTestCurrent = "alpha-edge-0-eno1"
	lbMapTestStale   = "alpha-edge-1-eno1"
	lbMapTestHost    = "shop.customer.example"
)

// A source whose read-only session answers the map's query with rows, or fails it.
type lbMapTestSource struct {
	rows []Row
	err  error
}

func (self *lbMapTestSource) PostgreSQL(ctx context.Context, query string) ([]Row, error) {
	if !strings.Contains(query, "dns_custom_host_map_applied") {
		return nil, fmt.Errorf("an unexpected query")
	}
	return self.rows, self.err
}

func (self *lbMapTestSource) Redis(ctx context.Context, host HostSettings, port int, args ...string) (string, error) {
	return "", errors.New("no redis here")
}

func (self *lbMapTestSource) Host(ctx context.Context, host HostSettings, command string) (string, error) {
	return "", errors.New("no host here")
}

// dns.yml's map windows: §13.4's 2 min poll silence and probe freshness and 20 min staleness.
func lbMapTestSettings() (*LbMapStaleSettings, error) {
	settings := DefaultLbMapStaleSettings()
	settings.Root = "ur.example."
	settings.Map = dnsservice.DefaultDnsSettings().Map
	return settings, nil
}

// Rows for two map versions, version 2 made versionAge before now, two interfaces, the current one current for
// version 2 and the stale one still on version 1, and active custom hosts on version 2.
func lbMapTestRows(now time.Time, versionAge time.Duration) []Row {
	milliseconds := func(when time.Time) string { return fmt.Sprint(when.UnixMilli()) }
	return []Row{
		{"version", "1", milliseconds(now.Add(-2 * time.Hour)), "", "", "", ""},
		{"version", "2", milliseconds(now.Add(-versionAge)), "", "", "", ""},
		{"interface", lbMapTestCurrent, "2", milliseconds(now.Add(-30 * time.Second)), "2", milliseconds(now.Add(-30 * time.Second)),
			`{"added": ["` + lbMapTestHost + `"], "results": {"` + lbMapTestHost + `": true}}`},
		{"interface", lbMapTestStale, "1", milliseconds(now.Add(-30 * time.Second)), "1", milliseconds(now.Add(-30 * time.Second)), ""},
		{"host", "2", "", "", "", "", ""},
	}
}

// One run of the signal at now against source.
func runLbMapStale(t *testing.T, source SignalSource, settings func() (*LbMapStaleSettings, error), now time.Time) Alerts {
	t.Helper()
	signalSettings := syntheticSettings(source)
	signalSettings.Now = func() time.Time { return now }
	alerts, err := newLbMapStaleSignal(settings).Run(context.Background(), signalSettings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

// The alerts of class for target.
func lbMapAlertsOf(alerts Alerts, class string, target string) Alerts {
	found := Alerts{}
	for _, alert := range alerts {
		if alert.Class == class && alert.Target == target {
			found = append(found, alert)
		}
	}
	return found
}

// An interface not current for the newest version past 20 minutes pages, and so does the custom hosts' version
// pending on it; the current interface does not; and no alert names the custom host.
func TestLbMapStalePagesAStaleInterfaceAndAPendingHostVersion(t *testing.T) {
	now := time.Date(2026, time.November, 7, 2, 0, 0, 0, time.UTC)
	alerts := runLbMapStale(t, &lbMapTestSource{rows: lbMapTestRows(now, 25*time.Minute)}, lbMapTestSettings, now)
	if len(lbMapAlertsOf(alerts, lbMapClassStale, lbMapTestStale)) != 1 {
		t.Fatalf("the stale interface did not page: %+v", alerts)
	}
	if len(lbMapAlertsOf(alerts, lbMapClassStale, lbMapTestCurrent)) != 0 {
		t.Fatalf("the current interface paged: %+v", alerts)
	}
	pending := lbMapAlertsOf(alerts, lbMapClassHostPending, lbMapVersionTarget(2))
	if len(pending) != 1 || !strings.Contains(pending[0].Markdown(), lbMapTestStale) {
		t.Fatalf("the pending version did not page naming its interface: %+v", pending)
	}
	for _, alert := range alerts {
		if strings.Contains(alert.Markdown(), lbMapTestHost) || alert.Severity != SeverityPage {
			t.Fatalf("an alert names a custom host or is no page: %+v", alert)
		}
	}
}

// A version younger than map.stale_after_s is not yet stale on any interface, and its hosts are not yet past
// their window: nothing pages.
func TestLbMapStaleWaitsOutAYoungVersion(t *testing.T) {
	now := time.Date(2026, time.November, 7, 2, 0, 0, 0, time.UTC)
	alerts := runLbMapStale(t, &lbMapTestSource{rows: lbMapTestRows(now, 5*time.Minute)}, lbMapTestSettings, now)
	if len(alerts) != 0 {
		t.Fatalf("a young version alerted: %+v", alerts)
	}
	// every interface current: nothing either
	rows := lbMapTestRows(now, 25*time.Minute)
	rows[3] = Row{"interface", lbMapTestStale, "2", fmt.Sprint(now.Add(-30 * time.Second).UnixMilli()), "2", fmt.Sprint(now.Add(-30 * time.Second).UnixMilli()),
		`{"added": ["` + lbMapTestHost + `"], "results": {"` + lbMapTestHost + `": true}}`}
	if alerts := runLbMapStale(t, &lbMapTestSource{rows: rows}, lbMapTestSettings, now); len(alerts) != 0 {
		t.Fatalf("a current map alerted: %+v", alerts)
	}
}

// A state that does not read, from a failed query or a row of no kind, is a visibility alert, never a clear; with
// no dns.yml the signal is configured off.
func TestLbMapStaleBlindRunsAreVisibilityAlerts(t *testing.T) {
	now := time.Date(2026, time.November, 7, 2, 0, 0, 0, time.UTC)
	for name, source := range map[string]*lbMapTestSource{
		"a failed query":   {err: errors.New("relation dns_custom_host_map_applied does not exist")},
		"a row of no kind": {rows: []Row{{"surprise", "1", "", "", "", "", ""}}},
		"a short row":      {rows: []Row{{"version", "1"}}},
	} {
		blind := lbMapAlertsOf(runLbMapStale(t, source, lbMapTestSettings, now), lbMapClassUnobservable, lbMapStaleTarget)
		if len(blind) != 1 {
			t.Errorf("%s: no visibility alert: %+v", name, blind)
		}
	}
	off := runLbMapStale(t, &lbMapTestSource{}, func() (*LbMapStaleSettings, error) { return nil, errLbMapStaleNoDnsYml }, now)
	for _, alert := range off {
		if alert.Class != lbMapClassUnobservable {
			t.Fatalf("configured off alerted %+v", alert)
		}
	}
}

// FS-50's map-stale case at 02:00 on the fake clock, FS-122's paged clause (D-OPS-9, R-OPS-11): the stale
// interface's page reaches the first person at once and the second at 30 minutes unacknowledged.
func TestFs50MapStaleAt0200ReachesTheRota(t *testing.T) {
	harness := newPagerHarness()
	notifier := NewPagerAlertNotifier(harness.settings())
	defer closePager(t, notifier)
	at0200 := time.Date(2026, time.November, 7, 2, 0, 0, 0, time.UTC)
	if !harness.clock.Now().Equal(at0200) {
		t.Fatalf("the clock reads %s", harness.clock.Now())
	}
	stale := scriptedExecution{alerts: runLbMapStale(t, &lbMapTestSource{rows: lbMapTestRows(at0200, 25*time.Minute)}, lbMapTestSettings, at0200)}
	runScriptedExecutions(t, []scriptedExecution{stale}, notifier.Handle)
	flushPager(t, notifier)
	requirePagedPeople(t, harness.sink, pager.FakeFirstPerson, pager.FakeFirstPerson)
	classes := map[string]bool{}
	for _, notification := range harness.sink.Notifications() {
		classes[notification.Page.Class] = true
		if !notification.Time.Equal(at0200) || notification.Page.BothAtOnce {
			t.Fatalf("paged %+v", notification)
		}
		if strings.Contains(notification.Page.Summary, lbMapTestHost) || strings.Contains(notification.Page.Summary, lbMapTestStale) {
			t.Fatalf("the page names its target: %q", notification.Page.Summary)
		}
	}
	if !classes[lbMapClassStale] || !classes[lbMapClassHostPending] {
		t.Fatalf("paged classes %v", classes)
	}
	harness.clock.Advance(30 * time.Minute)
	harness.sink.Tick()
	requirePagedPeople(t, harness.sink, pager.FakeFirstPerson, pager.FakeFirstPerson, pager.FakeSecondPerson, pager.FakeSecondPerson)
}
