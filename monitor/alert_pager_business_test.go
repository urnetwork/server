package monitor

// §19.8's business row through the pager (QD-754), on the harness's fake
// clock, which stands at 02:00 on a Saturday: the default review hours,
// Monday to Friday 09:00 to 18:00 UTC, next open on Monday at 09:00.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/pager"
)

// A second signal, whose executions own no incident of the first.
var pagerOtherTestSignal = selectionSignal{number: "1.4", key: "other-signal", id: "pg/other-signal"}

// The harness's Saturday 02:00 and the calendar's next open.
var (
	pagerTestNight = time.Date(2026, time.November, 7, 2, 0, 0, 0, time.UTC)
	pagerTestOpen  = time.Date(2026, time.November, 9, 9, 0, 0, 0, time.UTC)
)

// A calendar a test opens and closes by hand.
type pagerTestCalendar struct {
	open bool
}

func (self *pagerTestCalendar) IsOpen(time.Time) bool {
	return self.open
}

// The business rows of the table.
func pagerBusinessTestRows() []pageClass {
	rows := []pageClass{}
	for _, row := range pageClasses {
		if row.hours == pageHoursBusiness {
			rows = append(rows, row)
		}
	}
	return rows
}

func requireNoPagerAttempts(t *testing.T, sink *pager.FakeSink) {
	t.Helper()
	if got := sink.Attempts(); len(got) != 0 {
		t.Fatalf("attempts %+v", got)
	}
}

// Every business row raised at 02:00 on a Saturday: nobody is paged through
// the weekend, the first person is paged at Monday's open by whichever signal
// runs next, the second 30 minutes later unacknowledged, and the clear
// resolves the page as any page's does.
func TestBusinessPageRaisedAtNightPagesAtTheCalendarsOpen(t *testing.T) {
	rows := pagerBusinessTestRows()
	if len(rows) == 0 {
		t.Fatal("the table has no business row")
	}
	for _, row := range rows {
		harness := newPagerHarness()
		notifier := NewPagerAlertNotifier(harness.settings())
		present := SignalExecution{Alerts: Alerts{pagerTestAlert(row.class, SeverityPage)}}
		notifier.Handle(slackTestSignal, present)
		flushPager(t, notifier)
		requireNoPagerAttempts(t, harness.sink)
		if !strings.Contains(harness.logged(), "a page of class "+row.class+" is held until business hours open") {
			t.Fatalf("%s: the hold was not logged:\n%s", row.class, harness.logged())
		}

		// Saturday, Sunday and Monday up to a second before the open
		for _, at := range []time.Time{
			pagerTestNight.Add(12 * time.Hour),
			pagerTestNight.Add(34 * time.Hour),
			pagerTestOpen.Add(-time.Second),
		} {
			harness.clock.Advance(at.Sub(harness.clock.Now()))
			notifier.Handle(slackTestSignal, present)
			notifier.Handle(pagerOtherTestSignal, SignalExecution{})
			flushPager(t, notifier)
			requireNoPagerAttempts(t, harness.sink)
		}

		// the open, taken from another signal's execution
		harness.clock.Advance(time.Second)
		notifier.Handle(pagerOtherTestSignal, SignalExecution{})
		flushPager(t, notifier)
		requirePagedPeople(t, harness.sink, pager.FakeFirstPerson)
		notification := harness.sink.Notifications()[0]
		if notification.Page.Class != row.class || notification.Page.BothAtOnce || !notification.Time.Equal(pagerTestOpen) || !notification.Page.Time.Equal(pagerTestNight) {
			t.Fatalf("%s: paged %+v", row.class, notification)
		}
		if want := pagerDedupKey("synthetic", present.Alerts[0]); notification.Page.DedupKey != want {
			t.Fatalf("%s: key %s, want %s", row.class, notification.Page.DedupKey, want)
		}

		// later runs page nobody again, and the rota's 30 minutes run from the open
		notifier.Handle(slackTestSignal, present)
		flushPager(t, notifier)
		harness.clock.Advance(30*time.Minute - time.Second)
		harness.sink.Tick()
		requirePagedPeople(t, harness.sink, pager.FakeFirstPerson)
		harness.clock.Advance(time.Second)
		harness.sink.Tick()
		requirePagedPeople(t, harness.sink, pager.FakeFirstPerson, pager.FakeSecondPerson)

		notifier.Handle(slackTestSignal, SignalExecution{})
		notifier.Handle(slackTestSignal, SignalExecution{})
		flushPager(t, notifier)
		if got := harness.sink.OpenKeys(); len(got) != 0 {
			t.Fatalf("%s: open keys %v", row.class, got)
		}
		closePager(t, notifier)
	}
}

// An incident that clears before the calendar opens pages nobody, then or at
// the open. Its resolve reaches the sink, which has no page of the key.
func TestBusinessPageResolvedBeforeTheCalendarOpensIsDropped(t *testing.T) {
	for _, row := range pagerBusinessTestRows() {
		harness := newPagerHarness()
		notifier := NewPagerAlertNotifier(harness.settings())
		alert := pagerTestAlert(row.class, SeverityPage)
		notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{alert}})
		harness.clock.Advance(time.Hour)
		notifier.Handle(slackTestSignal, SignalExecution{})
		notifier.Handle(slackTestSignal, SignalExecution{})
		flushPager(t, notifier)
		if !strings.Contains(harness.logged(), "a page of class "+row.class+" held for business hours was dropped") {
			t.Fatalf("%s: the drop was not logged:\n%s", row.class, harness.logged())
		}

		harness.clock.Advance(pagerTestOpen.Sub(harness.clock.Now()))
		notifier.Handle(slackTestSignal, SignalExecution{})
		notifier.Handle(pagerOtherTestSignal, SignalExecution{})
		flushPager(t, notifier)
		harness.clock.Advance(time.Hour)
		harness.sink.Tick()
		requirePagedPeople(t, harness.sink)
		want := []pager.FakeAttempt{{Resolve: true, DedupKey: pagerDedupKey("synthetic", alert)}}
		if got := harness.sink.Attempts(); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: attempts %+v, want %+v", row.class, got, want)
		}
		closePager(t, notifier)
		if strings.Contains(harness.logged(), "still held for business hours") {
			t.Fatalf("%s: a dropped page was counted as held at close:\n%s", row.class, harness.logged())
		}
	}
}

// The calendar changes nothing for an any-hours row: closed all night, every
// one of them still pages at once, and its clear resolves it.
func TestAnyHoursPageIsUnchangedByTheCalendar(t *testing.T) {
	anyCount := 0
	for _, row := range pageClasses {
		if row.hours != pageHoursAny {
			continue
		}
		anyCount++
		harness := newPagerHarness()
		settings := harness.settings()
		settings.BusinessCalendar = &pagerTestCalendar{}
		notifier := NewPagerAlertNotifier(settings)
		notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{pagerTestAlert(row.class, SeverityPage)}})
		flushPager(t, notifier)
		if notifications := harness.sink.Notifications(); len(notifications) == 0 || !notifications[0].Time.Equal(pagerTestNight) || notifications[0].Page.Class != row.class {
			t.Fatalf("%s: paged %+v", row.class, notifications)
		}
		notifier.Handle(slackTestSignal, SignalExecution{})
		notifier.Handle(slackTestSignal, SignalExecution{})
		flushPager(t, notifier)
		if got := harness.sink.OpenKeys(); len(got) != 0 {
			t.Fatalf("%s: open keys %v", row.class, got)
		}
		closePager(t, notifier)
		if strings.Contains(harness.logged(), "held") {
			t.Fatalf("%s: an any-hours page was held:\n%s", row.class, harness.logged())
		}
	}
	if anyCount < 10 {
		t.Fatalf("%d any-hours rows: the table lost §19.8's rows", anyCount)
	}
}

// A business row raised while the calendar is open pages at once, as an
// any-hours row does, and one promoted to page severity at night is held from
// its promotion.
func TestBusinessPageInBusinessHoursPagesAtOnce(t *testing.T) {
	harness := newPagerHarness()
	harness.clock.Advance(pagerTestOpen.Add(time.Hour).Sub(harness.clock.Now()))
	notifier := NewPagerAlertNotifier(harness.settings())
	defer closePager(t, notifier)
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{pagerTestAlert("filecluster-drive-dwpd-business", SeverityPage)}})
	flushPager(t, notifier)
	requirePagedPeople(t, harness.sink, pager.FakeFirstPerson)
	if notification := harness.sink.Notifications()[0]; !notification.Time.Equal(pagerTestOpen.Add(time.Hour)) {
		t.Fatalf("paged %+v", notification)
	}

	// Monday 18:00 is closed: a warn there pages nothing, and its promotion is held
	harness.clock.Advance(8 * time.Hour)
	warn := pagerTestAlert("dns-ns-failing", SeverityWarn)
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{pagerTestAlert("filecluster-drive-dwpd-business", SeverityPage), warn}})
	promoted := warn
	promoted.Severity = SeverityPage
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{pagerTestAlert("filecluster-drive-dwpd-business", SeverityPage), promoted}})
	flushPager(t, notifier)
	requirePagedPeople(t, harness.sink, pager.FakeFirstPerson)

	// Tuesday 09:00
	harness.clock.Advance(15 * time.Hour)
	notifier.Handle(pagerOtherTestSignal, SignalExecution{})
	flushPager(t, notifier)
	classes := []string{}
	for _, notification := range harness.sink.Notifications() {
		classes = append(classes, notification.Page.Class)
	}
	if want := []string{"filecluster-drive-dwpd-business", "dns-ns-failing"}; !reflect.DeepEqual(classes, want) {
		t.Fatalf("paged %v, want %v", classes, want)
	}
}

// Held pages go out oldest hold first, each under its own key, and a clear
// between them drops only its own.
func TestHeldBusinessPagesGoOutInTheOrderHeld(t *testing.T) {
	harness := newPagerHarness()
	notifier := NewPagerAlertNotifier(harness.settings())
	defer closePager(t, notifier)
	first := pagerTestAlert("filecluster-hdd-trigger", SeverityPage)
	second := pagerTestAlert("filecluster-cohort-limit", SeverityPage)
	third := pagerTestAlert("dns-ns-failing", SeverityPage)
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{first}})
	harness.clock.Advance(time.Hour)
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{first, second}})
	harness.clock.Advance(time.Hour)
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{first, second, third}})
	// the second clears on Sunday
	harness.clock.Advance(24 * time.Hour)
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{first, third}})
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{first, third}})
	flushPager(t, notifier)
	requirePagedPeople(t, harness.sink)

	harness.clock.Advance(pagerTestOpen.Sub(harness.clock.Now()))
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{first, third}})
	flushPager(t, notifier)
	classes := []string{}
	for _, notification := range harness.sink.Notifications() {
		classes = append(classes, notification.Page.Class)
	}
	if want := []string{"filecluster-hdd-trigger", "dns-ns-failing"}; !reflect.DeepEqual(classes, want) {
		t.Fatalf("paged %v, want %v", classes, want)
	}
	if got := len(harness.sink.OpenKeys()); got != 2 {
		t.Fatalf("%d open keys", got)
	}
}

// A watcher restarted out of hours holds again the incident an earlier watcher
// paged in hours: the open page and its clock stay as they were, and the clear
// the new watcher sees at night still closes it.
func TestRestartedWatcherResolvesAnEarlierWatchersBusinessPageAtNight(t *testing.T) {
	harness := newPagerHarness()
	calendar := &pagerTestCalendar{open: true}
	settings := harness.settings()
	settings.BusinessCalendar = calendar
	alert := pagerTestAlert("filecluster-drive-rating-short", SeverityPage)

	earlier := NewPagerAlertNotifier(settings)
	earlier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{alert}})
	closePager(t, earlier)
	requirePagedPeople(t, harness.sink, pager.FakeFirstPerson)

	calendar.open = false
	later := NewPagerAlertNotifier(settings)
	defer closePager(t, later)
	later.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{alert}})
	flushPager(t, later)
	if got := harness.sink.OpenKeys(); len(got) != 1 {
		t.Fatalf("open keys %v", got)
	}
	later.Handle(slackTestSignal, SignalExecution{})
	later.Handle(slackTestSignal, SignalExecution{})
	flushPager(t, later)
	if got := harness.sink.OpenKeys(); len(got) != 0 {
		t.Fatalf("open keys %v", got)
	}
	requirePagedPeople(t, harness.sink, pager.FakeFirstPerson)
}

// A signal that cannot see keeps a held page held: a failed run and a
// visibility alert clear nothing, so the page still goes at the open.
func TestBlindRunsDoNotDropAHeldBusinessPage(t *testing.T) {
	harness := newPagerHarness()
	notifier := NewPagerAlertNotifier(harness.settings())
	defer closePager(t, notifier)
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{pagerTestAlert("filecluster-cohort-exhaustion", SeverityPage)}})
	for range 3 {
		notifier.Handle(slackTestSignal, SignalExecution{})
		notifier.Handle(slackTestSignal, SignalExecution{Failed: true})
	}
	flushPager(t, notifier)
	requireNoPagerAttempts(t, harness.sink)

	harness.clock.Advance(pagerTestOpen.Sub(harness.clock.Now()))
	notifier.Handle(slackTestSignal, SignalExecution{Failed: true})
	flushPager(t, notifier)
	requirePagedPeople(t, harness.sink, pager.FakeFirstPerson)
}

// Close says how many pages it leaves held, and sends none of them.
func TestCloseCountsThePagesStillHeld(t *testing.T) {
	harness := newPagerHarness()
	notifier := NewPagerAlertNotifier(harness.settings())
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{
		pagerTestAlert("filecluster-drive-dwpd-business", SeverityPage),
		pagerTestAlert("filecluster-hdd-trigger", SeverityPage),
	}})
	closePager(t, notifier)
	closePager(t, notifier)
	if got := strings.Count(harness.logged(), "2 page(s) were still held for business hours at close"); got != 1 {
		t.Fatalf("logged %d times:\n%s", got, harness.logged())
	}
	requireNoPagerAttempts(t, harness.sink)
}

// A sink outage at the open delays a held page like any other: the sender
// retries until the sink takes it.
func TestHeldBusinessPageIsRetriedThroughASinkOutage(t *testing.T) {
	harness := newPagerHarness()
	settings := harness.settings()
	waiting := make(chan struct{})
	proceed := make(chan struct{})
	settings.Delivery.RetryWait = func(ctx context.Context, _ time.Duration) bool {
		select {
		case waiting <- struct{}{}:
		case <-ctx.Done():
			return false
		}
		select {
		case <-proceed:
			return true
		case <-ctx.Done():
			return false
		}
	}
	notifier := NewPagerAlertNotifier(settings)
	defer closePager(t, notifier)
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{pagerTestAlert("dns-ns-failing", SeverityPage)}})
	harness.sink.Fail(errors.New("synthetic outage"))
	harness.clock.Advance(pagerTestOpen.Sub(harness.clock.Now()))
	notifier.Handle(pagerOtherTestSignal, SignalExecution{})
	// the first hand-off failed, and the sender waits to retry
	select {
	case <-waiting:
	case <-time.After(pagerTestWait):
		t.Fatal("the sender did not retry")
	}
	requirePagedPeople(t, harness.sink)
	harness.sink.Fail(nil)
	close(proceed)
	flushPager(t, notifier)
	requirePagedPeople(t, harness.sink, pager.FakeFirstPerson)
}

// §19.8's business row, copied here so an edit to the table has to say why:
// the six classes QD-754 names, and no other row waits for business hours.
func TestPageClassesHoldSection19_8sBusinessRow(t *testing.T) {
	want := map[string]string{
		"filecluster-drive-dwpd-business": "M3",
		"filecluster-drive-rating-short":  "M3",
		"filecluster-cohort-exhaustion":   "M3",
		"filecluster-cohort-limit":        "M3",
		"filecluster-hdd-trigger":         "M3",
		"dns-ns-failing":                  "M5",
	}
	got := []string{}
	for _, row := range pagerBusinessTestRows() {
		got = append(got, row.class)
		if row.firstMilestone != want[row.class] || row.bothAtOnce || row.hours.String() != "business" {
			t.Errorf("%s: row %+v", row.class, row)
		}
	}
	wantClasses := []string{}
	for class := range want {
		wantClasses = append(wantClasses, class)
	}
	sort.Strings(got)
	sort.Strings(wantClasses)
	if !reflect.DeepEqual(got, wantClasses) {
		t.Fatalf("business rows %v, want %v", got, wantClasses)
	}
	exhaustion, _ := pageClassOf("filecluster-cohort-exhaustion")
	limit, _ := pageClassOf("filecluster-cohort-limit")
	if exhaustion.row != limit.row || exhaustion.action != limit.action {
		t.Fatal("§19.8 gives the projected exhaustion and the cohort limit one clause")
	}
	if row, _ := pageClassOf("postgres-primary-down"); row.hours != pageHoursAny || row.hours.String() != "any" {
		t.Fatalf("a row with no hours is not any-hours: %+v", row)
	}
}

// The signals raise the business classes at page severity, the tier the pager
// reads, and every other class of theirs as it was.
func TestBusinessClassesAreRaisedAtPageTier(t *testing.T) {
	wear := fileClusterWearProbe{}
	storageSettings := DefaultSidecarStorageSettings()
	for class, want := range map[string]string{
		fileClusterWearClassDwpdBusiness: tierPage,
		fileClusterWearClassRatingShort:  tierPage,
		fileClusterWearClassDwpdDay:      tierWarn,
		fileClusterWearClassDwpdWeek:     tierWarn,
		fileClusterWearClassMediaErrors:  tierWarn,
		fileClusterWearClassSpareLow:     tierWarn,
		fileClusterWearClassUnobservable: tierWarn,
	} {
		if got := wear.wearFinding(storageSettings, class, "meta-1", "", "synthetic", "").tier; got != want {
			t.Errorf("%s: tier %s, want %s", class, got, want)
		}
	}
	cohort := fileClusterCohortProbe{}
	for class, want := range map[string]string{
		fileClusterCohortClassLimit:        tierPage,
		fileClusterCohortClassExhaustion:   tierPage,
		fileClusterCohortClassHddTrigger:   tierPage,
		fileClusterCohortClassTrigger:      tierWarn,
		fileClusterCohortClassUnobservable: tierWarn,
	} {
		if got := cohort.cohortFinding(class, "meta-1", "", "synthetic", "").tier; got != want {
			t.Errorf("%s: tier %s, want %s", class, got, want)
		}
	}
	canary := &dnsCanaryCheck{settings: DefaultDnsCanarySettings()}
	failing := canary.finding(dnsCanaryClassNsFailing, "ns1.example.test.", "synthetic", "")
	if failing.tier != tierPage || failing.sustain != 60 || failing.pageSustain != 0 {
		t.Errorf("dns-ns-failing: %+v", failing)
	}
	for _, class := range []string{dnsCanaryClassDivergence, dnsCanaryClassKeyWindow, dnsCanaryClassPropagation, dnsCanaryClassUnobservable} {
		if got := canary.finding(class, "example.test.", "synthetic", "").tier; got != tierWarn {
			t.Errorf("%s: tier %s", class, got)
		}
	}
}

// A calendar on abuse.yml as a test pushes it.
func newAbuseReviewPagerTestCalendar(t *testing.T, yaml string) (*abuseReviewPagerCalendar, *[]string) {
	t.Helper()
	if yaml != "" {
		t.Cleanup(server.Testing_PushAbuseConfig(yaml))
	}
	logs := &[]string{}
	config := server.GetAbuseConfig
	if yaml == "" {
		config = func() *server.AbuseConfig { return nil }
	}
	return newAbuseReviewPagerCalendar(config, func(format string, args ...any) {
		*logs = append(*logs, fmt.Sprintf(format, args...))
	}), logs
}

// With no file the calendar is the default review hours: Monday to Friday,
// 09:00 up to 18:00 UTC.
func TestAbuseReviewPagerCalendarDefaultsToTheReviewHours(t *testing.T) {
	calendar, logs := newAbuseReviewPagerTestCalendar(t, "")
	for at, want := range map[time.Time]bool{
		pagerTestNight:                        false,
		pagerTestOpen.Add(-time.Nanosecond):   false,
		pagerTestOpen:                         true,
		pagerTestOpen.Add(9*time.Hour - 1):    true,
		pagerTestOpen.Add(9 * time.Hour):      false,
		pagerTestOpen.Add(-24 * time.Hour):    false,
		pagerTestOpen.Add(4 * 24 * time.Hour): true,
		pagerTestOpen.Add(5 * 24 * time.Hour): false,
	} {
		if got := calendar.IsOpen(at); got != want {
			t.Errorf("%s: open %t, want %t", at.Format(time.RFC3339Nano), got, want)
		}
	}
	if len(*logs) != 0 {
		t.Fatalf("logged %v", *logs)
	}
}

// abuse.yml's business_hours block moves the calendar, zone included.
func TestAbuseReviewPagerCalendarReadsAbuseYmlsHours(t *testing.T) {
	calendar, _ := newAbuseReviewPagerTestCalendar(t, "business_hours: {zone: Europe/Berlin, days: [sat], open: \"03:30\", close: \"04:00\"}\n")
	// Berlin stands at UTC+1 in November: 03:30 there is 02:30 UTC
	for at, want := range map[time.Time]bool{
		pagerTestNight:                       false,
		pagerTestNight.Add(30 * time.Minute): true,
		pagerTestNight.Add(time.Hour):        false,
		pagerTestOpen:                        false,
	} {
		if got := calendar.IsOpen(at); got != want {
			t.Errorf("%s: open %t, want %t", at.Format(time.RFC3339), got, want)
		}
	}
}

// M6's always mode is the reviewers', not the rota's nights: the pager keeps
// the block's hours under it.
func TestAbuseReviewPagerCalendarKeepsHoursUnderAlwaysMode(t *testing.T) {
	calendar, logs := newAbuseReviewPagerTestCalendar(t, "business_hours: {mode: always}\n")
	if calendar.IsOpen(pagerTestNight) || !calendar.IsOpen(pagerTestOpen) {
		t.Fatal("always mode opened the night, or closed Monday morning")
	}
	if len(*logs) != 0 {
		t.Fatalf("logged %v", *logs)
	}
}

// A block that builds no calendar keeps the default hours and says so once,
// however often the calendar is asked.
func TestAbuseReviewPagerCalendarKeepsTheDefaultHoursOnABadBlock(t *testing.T) {
	calendar, logs := newAbuseReviewPagerTestCalendar(t, "business_hours: {zone: Nowhere/Synthetic}\n")
	for range 3 {
		if calendar.IsOpen(pagerTestNight) || !calendar.IsOpen(pagerTestOpen) {
			t.Fatal("a bad block changed the hours")
		}
	}
	if len(*logs) != 1 || !strings.Contains((*logs)[0], "business-hours pages keep the default hours") {
		t.Fatalf("logged %v", *logs)
	}
}

// A notifier with no calendar set reads abuse.yml's.
func TestPagerNotifierDefaultsToAbuseYmlsCalendar(t *testing.T) {
	t.Cleanup(server.Testing_PushAbuseConfig("business_hours: {days: [sat], open: \"03:00\", close: \"04:00\"}\n"))
	harness := newPagerHarness()
	settings := harness.settings()
	settings.BusinessCalendar = nil
	notifier := NewPagerAlertNotifier(settings)
	defer closePager(t, notifier)
	present := SignalExecution{Alerts: Alerts{pagerTestAlert("filecluster-drive-dwpd-business", SeverityPage)}}
	notifier.Handle(slackTestSignal, present)
	flushPager(t, notifier)
	requireNoPagerAttempts(t, harness.sink)
	harness.clock.Advance(time.Hour)
	notifier.Handle(slackTestSignal, present)
	flushPager(t, notifier)
	requirePagedPeople(t, harness.sink, pager.FakeFirstPerson)
}
