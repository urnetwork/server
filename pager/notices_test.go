package pager

// §21.8's notices on the rota's pager: both people at once, at any hour, a
// page holding only the notice's kind, the time and its case id, and the
// sink's refusal returned to the caller rather than swallowed. Time moves only
// by hand.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Every kind that pages.
var testNoticeKinds = []NoticeKind{
	NoticeKindRegistrar,
	NoticeKindRegistry,
	NoticeKindLawEnforcementEmergency,
	NoticeKindEuRemovalOrder,
	NoticeKindProductionOrder,
}

func testNotice(kind NoticeKind, caseId server.Id, filedTime time.Time) Notice {
	return Notice{Kind: kind, CaseId: caseId, Time: filedTime, Environment: "synthetic"}
}

// FS-50's notice and FS-49's paging half on the rota's policy: each kind, filed
// at 02:00 on a weekday and at 03:00 on a Saturday, pages both people at the
// same instant, and nobody further at 30 minutes, since nobody is left.
func TestPageNoticePagesBothPeopleAtOnceAtAnyHour(t *testing.T) {
	for _, filedTime := range []time.Time{
		time.Date(2026, time.November, 5, 2, 0, 0, 0, time.UTC),
		time.Date(2026, time.November, 7, 3, 0, 0, 0, time.UTC),
	} {
		for _, kind := range testNoticeKinds {
			clock := newTestClock()
			clock.now = filedTime
			sink := NewFakeSink(FakeSinkSettings{Now: clock.Now})
			caseId := server.NewId()
			if err := PageNotice(context.Background(), sink, testNotice(kind, caseId, filedTime)); err != nil {
				t.Fatalf("%s: %v", kind, err)
			}
			key := NoticeDedupKey(caseId)
			requirePaged(t, sink, "first:"+key, "second:"+key)
			notifications := sink.Notifications()
			for _, notification := range notifications {
				if !notification.Time.Equal(filedTime) || notification.Page.Class != NoticePageClass || !notification.Page.BothAtOnce {
					t.Fatalf("%s at %s: paged %+v", kind, filedTime, notification)
				}
			}
			clock.Advance(30 * time.Minute)
			sink.Tick()
			requirePaged(t, sink, "first:"+key, "second:"+key)
		}
	}
}

// A page leaves the site, so its key is the case id alone and its summary is
// the row's fixed words with the kind and the case id: nothing a notice could
// carry beyond those has a field to arrive in.
func TestNoticePageHoldsOnlyItsKindTimeAndCaseId(t *testing.T) {
	caseId := server.RequireParseId("0193a5d2-3c4b-7a10-8f00-000000000001")
	filedTime := time.Date(2026, time.November, 7, 2, 0, 0, 0, time.FixedZone("synthetic", 3600))
	page, err := noticePage(testNotice(NoticeKindRegistrar, caseId, filedTime))
	if err != nil {
		t.Fatal(err)
	}
	want := Page{
		DedupKey: "notice-0193a5d2-3c4b-7a10-8f00-000000000001",
		Class:    "abuse-notice",
		Summary: "[synthetic] abuse-notice (registrar): registrar, registry, law-enforcement notices. Then: " +
			"§21.8: removal orders and registrar or registry notices handled within 1 h by the rota; " +
			"production orders go to counsel's out-of-hours contact. This one: the rota handles it within 1 h. " +
			"Case 0193a5d2-3c4b-7a10-8f00-000000000001.",
		Time:       filedTime.UTC(),
		BothAtOnce: true,
	}
	if !reflect.DeepEqual(page, want) {
		t.Fatalf("page %+v\nwant %+v", page, want)
	}
	if err := page.Validate(); err != nil {
		t.Fatalf("the notice's page does not validate: %v", err)
	}
	if err := ValidateDedupKey(NoticeDedupKey(caseId)); err != nil {
		t.Fatalf("a case's key is not an opaque id: %v", err)
	}
}

// §21.8 sends production orders and other data requests to counsel and keeps
// removal orders and registrar or registry notices with the rota: each page
// says which.
func TestNoticePageNamesWhereEachKindGoes(t *testing.T) {
	for kind, wantCounsel := range map[NoticeKind]bool{
		NoticeKindRegistrar:               false,
		NoticeKindRegistry:                false,
		NoticeKindEuRemovalOrder:          false,
		NoticeKindLawEnforcementEmergency: true,
		NoticeKindProductionOrder:         true,
	} {
		page, err := noticePage(testNotice(kind, server.NewId(), time.Unix(1, 0)))
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		_, this, _ := strings.Cut(page.Summary, "This one: ")
		if got := strings.Contains(this, "counsel"); got != wantCounsel {
			t.Errorf("%s: summary %q", kind, page.Summary)
		}
		if !strings.Contains(page.Summary, "("+string(kind)+")") {
			t.Errorf("%s: the summary does not name its kind: %q", kind, page.Summary)
		}
	}
}

// What cannot page is refused before the sink, and says why without repeating
// a value. An unknown kind never pages: the abuse worker decides which notices
// page, and a kind this file does not know is a mistake there.
func TestPageNoticeRefusesWhatCannotPage(t *testing.T) {
	filedTime := time.Date(2026, time.November, 7, 2, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name   string
		notice Notice
	}{
		{name: "unknown kind", notice: testNotice("search_console", server.NewId(), filedTime)},
		{name: "empty kind", notice: testNotice("", server.NewId(), filedTime)},
		{name: "no case", notice: testNotice(NoticeKindRegistrar, server.Id{}, filedTime)},
		{name: "no time", notice: testNotice(NoticeKindRegistrar, server.NewId(), time.Time{})},
	} {
		sink, _ := newClockedFakeSink()
		err := PageNotice(context.Background(), sink, c.notice)
		if err == nil {
			t.Errorf("%s: paged", c.name)
			continue
		}
		if strings.Contains(err.Error(), "search_console") {
			t.Errorf("%s: the refusal repeats the value: %v", c.name, err)
		}
		if 0 < len(sink.Attempts()) {
			t.Errorf("%s: the sink was called", c.name)
		}
	}
	if err := PageNotice(context.Background(), nil, testNotice(NoticeKindRegistrar, server.NewId(), filedTime)); err == nil {
		t.Error("a notice paged through no sink")
	}
}

// The sink's refusal comes back to the caller unchanged, so the abuse worker
// can retry: nothing here swallows a lost page.
func TestPageNoticeReturnsTheSinksRefusal(t *testing.T) {
	sink, _ := newClockedFakeSink()
	outage := errors.New("synthetic outage")
	sink.Fail(outage)
	notice := testNotice(NoticeKindRegistry, server.NewId(), time.Date(2026, time.November, 7, 2, 0, 0, 0, time.UTC))
	if err := PageNotice(context.Background(), sink, notice); !errors.Is(err, outage) {
		t.Fatalf("err = %v", err)
	}
	requirePaged(t, sink)
	sink.Fail(nil)
	if err := PageNotice(context.Background(), sink, notice); err != nil {
		t.Fatal(err)
	}
	key := NoticeDedupKey(notice.CaseId)
	requirePaged(t, sink, "first:"+key, "second:"+key)
}

// A retry, or a second notice filed under the same open case, opens no second
// page and restarts no clock.
func TestPageNoticeRepeatedForOneCaseOpensOnePage(t *testing.T) {
	sink, clock := newClockedFakeSink()
	caseId := server.NewId()
	filedTime := clock.Now()
	for range 3 {
		if err := PageNotice(context.Background(), sink, testNotice(NoticeKindRegistrar, caseId, filedTime)); err != nil {
			t.Fatal(err)
		}
		clock.Advance(time.Minute)
	}
	key := NoticeDedupKey(caseId)
	requirePaged(t, sink, "first:"+key, "second:"+key)
	if got := sink.OpenKeys(); !reflect.DeepEqual(got, []string{key}) {
		t.Fatalf("open keys %v", got)
	}
}

// Through the sender a caller that can retry uses: an outage holds the notice
// and retries it, and once the sink takes it both people are paged at once.
func TestPageNoticeThroughRetrySinkReachesBothAfterAnOutage(t *testing.T) {
	sink, _ := newClockedFakeSink()
	sink.Fail(errors.New("synthetic outage"))
	steps := newRetrySteps()
	retrySink := NewRetrySink(sink, steppedSettings(steps, &lockedLines{}))
	defer closeRetrySink(t, retrySink)

	caseId := server.NewId()
	notice := testNotice(NoticeKindLawEnforcementEmergency, caseId, time.Date(2026, time.November, 7, 2, 0, 0, 0, time.UTC))
	if err := PageNotice(context.Background(), retrySink, notice); err != nil {
		t.Fatal(err)
	}
	steps.requireWait(t)
	requirePaged(t, sink)
	sink.Fail(nil)
	steps.proceed(t)
	requireFlushed(t, retrySink)
	key := NoticeDedupKey(caseId)
	requirePaged(t, sink, "first:"+key, "second:"+key)
}
