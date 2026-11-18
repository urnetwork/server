//go:build sidecar_pending

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/pager"
)

// §21.8's paged notices and clock pass (abuse_notice_controller.go) on MG5's
// tables, pager.FakeSink and a fake clock: FS-49's paging half and FS-50's
// registrar notice. Names are .example.

// A notice harness: the settings with the test root, a fake clock at now, and
// a fake pager on that clock installed for the rest of the test.
type testAbuseNoticeWorld struct {
	settings *model.AbuseSettings
	now      time.Time
	sink     *pager.FakeSink
}

func newTestAbuseNotices(t testing.TB, now time.Time) *testAbuseNoticeWorld {
	self := &testAbuseNoticeWorld{
		settings: model.DefaultAbuseSettings(),
		now:      now,
	}
	self.settings.RootNames = []string{testAbuseIntakeRoot}
	self.settings.PlatformNames = []string{"platform.example"}
	self.sink = pager.NewFakeSink(pager.FakeSinkSettings{Now: func() time.Time {
		return self.now
	}})
	previousNow := abuseNoticeDeps.now
	previousSettings := abuseNoticeDeps.settings
	previousEnvironment := abuseNoticeDeps.environment
	abuseNoticeDeps.now = func() time.Time {
		return self.now
	}
	abuseNoticeDeps.settings = func() *model.AbuseSettings {
		return self.settings
	}
	abuseNoticeDeps.environment = func() string {
		return "test"
	}
	restorePager := SetAbuseNoticePager(self.sink)
	t.Cleanup(func() {
		restorePager()
		abuseNoticeDeps.now = previousNow
		abuseNoticeDeps.settings = previousSettings
		abuseNoticeDeps.environment = previousEnvironment
	})
	return self
}

// Every class that pages.
var testAbuseNoticeClasses = []model.AbuseNoticeClass{
	model.AbuseNoticeClassRegistrar,
	model.AbuseNoticeClassRegistry,
	model.AbuseNoticeClassLawEnforcementEmergency,
	model.AbuseNoticeClassEuRemovalOrder,
	model.AbuseNoticeClassProductionOrder,
}

func testAbuseNoticeActionCount(t testing.TB, ctx context.Context) (count int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM abuse_action`).Scan(&count))
	})
	return
}

// FS-49's paging half: a notice of every class filed at Saturday 03:00 reaches
// the pager for both people at once, at that instant, under a key holding only
// its case id, opens a case due within the hour whatever the calendar, and
// kills nothing.
func TestFileAbuseNoticePagesEveryClassOnASaturdayNight(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		saturday0300 := time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC)
		if saturday0300.Weekday() != time.Saturday {
			t.Fatalf("%s is no Saturday", saturday0300)
		}
		notices := newTestAbuseNotices(t, saturday0300)

		for i, class := range testAbuseNoticeClasses {
			result, err := FileAbuseNotice(ctx, &AbuseNoticeArgs{Class: class, Target: testAbuseIntakeRoot})
			if err != nil {
				t.Fatalf("%s: %v", class, err)
			}
			if !result.Opened || !result.Paged {
				t.Fatalf("%s: opened %t, paged %t", class, result.Opened, result.Paged)
			}
			if !result.ReviewDueTime.Equal(saturday0300.Add(time.Hour)) {
				t.Errorf("%s: due %s, want an hour after it was filed, at any hour", class, result.ReviewDueTime)
			}
			notifications := notices.sink.Notifications()
			if len(notifications) != 2*(i+1) {
				t.Fatalf("%s: %d people paged so far, want %d", class, len(notifications), 2*(i+1))
			}
			first, second := notifications[2*i], notifications[2*i+1]
			if first.Person != pager.FakeFirstPerson || second.Person != pager.FakeSecondPerson {
				t.Errorf("%s: paged %s then %s, want both people", class, first.Person, second.Person)
			}
			for _, notification := range []pager.FakeNotification{first, second} {
				if !notification.Time.Equal(saturday0300) {
					t.Errorf("%s: paged at %s, want the instant it was filed", class, notification.Time)
				}
				page := notification.Page
				if page.Class != pager.NoticePageClass || !page.BothAtOnce {
					t.Errorf("%s: page class %q, both at once %t", class, page.Class, page.BothAtOnce)
				}
				if page.DedupKey != pager.NoticeDedupKey(result.CaseId) || page.DedupKey != "notice-"+result.CaseId.String() {
					t.Errorf("%s: the key is %q, want only the case id", class, page.DedupKey)
				}
				// opaque ids only (I-13): the root the notice is about is not in the page
				if strings.Contains(page.Summary, testAbuseIntakeRoot) || strings.Contains(page.DedupKey, testAbuseIntakeRoot) {
					t.Errorf("%s: the page names its target", class)
				}
				if !strings.Contains(page.Summary, string(class)) || !strings.Contains(page.Summary, result.CaseId.String()) {
					t.Errorf("%s: the page lacks its class or its case id", class)
				}
			}

			abuseCase := model.GetAbuseCase(ctx, result.CaseId)
			if abuseCase == nil || abuseCase.State != model.AbuseCaseStateOpen || model.AbuseNoticeClassOfCase(abuseCase) != class {
				t.Fatalf("%s: the case is %+v", class, abuseCase)
			}
			if abuseCase.NetworkId != nil || abuseCase.ReporterClass != class.ReporterClass() {
				t.Errorf("%s: the case names network %v from %s", class, abuseCase.NetworkId, abuseCase.ReporterClass)
			}
			if abuseCase.ReviewDueTime == nil || !abuseCase.ReviewDueTime.Equal(saturday0300.Add(time.Hour)) {
				t.Errorf("%s: the case is due %v", class, abuseCase.ReviewDueTime)
			}
		}
		if count := testAbuseNoticeActionCount(t, ctx); count != 0 {
			t.Errorf("filing notices recorded %d actions, want none: a notice kills nothing", count)
		}
	})
}

// FS-50's registrar notice at 02:00: both people at once, with no wait for the
// first to acknowledge, and no second page of the second person when the
// rota's 30 minutes pass.
func TestFileAbuseNoticeRegistrarAt0200PagesBothWithNoEscalationWait(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		at0200 := time.Date(2026, 11, 18, 2, 0, 0, 0, time.UTC)
		notices := newTestAbuseNotices(t, at0200)

		result, err := FileAbuseNotice(ctx, &AbuseNoticeArgs{Class: model.AbuseNoticeClassRegistrar, Target: testAbuseIntakeRoot})
		if err != nil {
			t.Fatal(err)
		}
		if result.Route != model.AbuseNoticeRouteRota || result.CounselContact != "" {
			t.Errorf("a registrar notice routes to %s (%q), want the rota", result.Route, result.CounselContact)
		}
		notifications := notices.sink.Notifications()
		if len(notifications) != 2 || !notifications[0].Time.Equal(at0200) || !notifications[1].Time.Equal(at0200) {
			t.Fatalf("paged %+v, want both people at 02:00", notifications)
		}
		notices.now = at0200.Add(31 * time.Minute)
		notices.sink.Tick()
		if count := len(notices.sink.Notifications()); count != 2 {
			t.Errorf("%d notifications after the rota's 30 minutes, want the same 2", count)
		}
	})
}

// A production order routes to counsel's out-of-hours contact, from abuse.yml,
// and the contact stays out of the page. A removal order stays with the rota.
func TestFileAbuseNoticeProductionOrderRoutesToCounsel(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer server.Testing_PushAbuseConfig("counsel_contact: counsel-on-call.example\n")()
		notices := newTestAbuseNotices(t, time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC))

		production, err := FileAbuseNotice(ctx, &AbuseNoticeArgs{Class: model.AbuseNoticeClassProductionOrder, Target: testAbuseIntakeRoot})
		if err != nil {
			t.Fatal(err)
		}
		if production.Route != model.AbuseNoticeRouteCounsel || production.CounselContact != "counsel-on-call.example" {
			t.Errorf("a production order routes to %s (%q), want counsel's contact", production.Route, production.CounselContact)
		}
		removal, err := FileAbuseNotice(ctx, &AbuseNoticeArgs{Class: model.AbuseNoticeClassEuRemovalOrder, Target: testAbuseIntakeRoot})
		if err != nil {
			t.Fatal(err)
		}
		if removal.Route != model.AbuseNoticeRouteRota || removal.CounselContact != "" {
			t.Errorf("a removal order routes to %s (%q), want the rota", removal.Route, removal.CounselContact)
		}
		for _, notification := range notices.sink.Notifications() {
			if strings.Contains(notification.Page.Summary, "counsel-on-call.example") {
				t.Error("the page carries counsel's contact")
			}
		}
		if count := len(notices.sink.Notifications()); count != 4 {
			t.Errorf("%d notifications, want both people for both notices", count)
		}
	})
}

// Only the notice classes page: a Search Console flag, a class a caller made
// up and no class at all are refused, open no case and reach no pager.
func TestFileAbuseNoticeRefusesWhatDoesNotPage(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		notices := newTestAbuseNotices(t, time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC))

		for _, class := range []model.AbuseNoticeClass{
			model.AbuseNoticeClassNone,
			model.AbuseNoticeClass(model.AbuseReporterClassSearchConsole),
			model.AbuseNoticeClass(model.AbuseReporterClassPublic),
			"law_enforcement",
			"anything",
		} {
			result, err := FileAbuseNotice(ctx, &AbuseNoticeArgs{Class: class, Target: testAbuseIntakeRoot})
			if !errors.Is(err, model.ErrAbuseCaseInvalid) || result != nil {
				t.Errorf("class %q: %v, %v", class, result, err)
			}
		}
		// a target that is no host name is refused before anything is written
		for _, target := range []string{"", "root", "https://root.example/path", "198.51.100.7", "a b.example"} {
			result, err := FileAbuseNotice(ctx, &AbuseNoticeArgs{Class: model.AbuseNoticeClassRegistrar, Target: target})
			if !errors.Is(err, model.ErrAbuseCaseInvalid) || result != nil {
				t.Errorf("target %q: %v, %v", target, result, err)
			}
		}
		if count := testAbuseIntakeCaseCount(t, ctx); count != 0 {
			t.Errorf("%d cases, want none", count)
		}
		if attempts := notices.sink.Attempts(); len(attempts) != 0 {
			t.Errorf("the pager saw %d attempts, want none", len(attempts))
		}

		// the success twin
		if _, err := FileAbuseNotice(ctx, &AbuseNoticeArgs{Class: model.AbuseNoticeClassRegistrar, Target: testAbuseIntakeRoot}); err != nil {
			t.Fatal(err)
		}
		if count := len(notices.sink.Notifications()); count != 2 {
			t.Errorf("%d notifications for a registrar notice, want 2", count)
		}
	})
}

// A Search Console flag taken as the report it is waits for business hours:
// it opens a case on the report clock and pages no one (§21.17).
func TestSearchConsoleFlagDoesNotPage(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		saturday0300 := time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC)
		notices := newTestAbuseNotices(t, saturday0300)
		settings := newTestAbuseIntake(t)
		previousNow := abuseIntakeDeps.now
		abuseIntakeDeps.now = func() time.Time {
			return saturday0300
		}
		defer func() {
			abuseIntakeDeps.now = previousNow
		}()
		newTestAbuseDeps(t)
		_, zone := newTestAbuseIntakeNetwork(t, ctx)

		result := testAcceptAbuseReport(t, ctx, &model.AbuseReport{
			Category:      model.AbuseCategoryPhishing,
			ReporterClass: model.AbuseReporterClassSearchConsole,
			Url:           "https://shop." + zone + "/",
		})
		if !result.Joined || len(result.Actions) != 0 {
			t.Fatalf("joined %t with %d actions, want a case and no machine action", result.Joined, len(result.Actions))
		}
		abuseCase := model.GetAbuseCase(ctx, result.CaseId)
		monday := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
		if abuseCase == nil || abuseCase.ReviewDueTime == nil || abuseCase.ReviewDueTime.Before(monday) {
			t.Errorf("the flag's case is due %v, want no earlier than Monday's open", abuseCase)
		}
		if attempts := notices.sink.Attempts(); len(attempts) != 0 {
			t.Errorf("the pager saw %d attempts for a Search Console flag, want none", len(attempts))
		}
		// and the clock pass pages only notices
		pass := RunAbuseClockPass(ctx, saturday0300.Add(time.Minute), settings)
		if pass.NoticePagedCount != 0 || len(notices.sink.Attempts()) != 0 {
			t.Errorf("the clock pass paged %d cases that are no notice", pass.NoticePagedCount)
		}
	})
}

// A pager that refuses loses no page: the error is returned beside the case,
// a retry naming the case pages under the same key and opens no second case,
// and the key repeats on every attempt.
func TestFileAbuseNoticeRetryRepeatsItsKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		notices := newTestAbuseNotices(t, time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC))
		outage := errors.New("the pager service is down")
		notices.sink.Fail(outage)

		result, err := FileAbuseNotice(ctx, &AbuseNoticeArgs{Class: model.AbuseNoticeClassRegistry, Target: testAbuseIntakeRoot})
		if !errors.Is(err, outage) {
			t.Fatalf("a refused page answered %v, want the pager's own error", err)
		}
		if result == nil || !result.Opened || result.Paged {
			t.Fatalf("a refused page answered %+v, want the case it opened, unpaged", result)
		}
		if count := len(notices.sink.Notifications()); count != 0 {
			t.Fatalf("%d notifications during the outage", count)
		}

		notices.sink.Fail(nil)
		notices.now = notices.now.Add(2 * time.Minute)
		// a retry of another class's case, or of no notice case, pages nothing
		caseId := result.CaseId
		if _, err := FileAbuseNotice(ctx, &AbuseNoticeArgs{Class: model.AbuseNoticeClassRegistrar, CaseId: &caseId}); !errors.Is(err, model.ErrAbuseCaseNotFound) {
			t.Errorf("a retry under another class: %v", err)
		}
		unknownCaseId := server.NewId()
		if _, err := FileAbuseNotice(ctx, &AbuseNoticeArgs{Class: model.AbuseNoticeClassRegistry, CaseId: &unknownCaseId}); !errors.Is(err, model.ErrAbuseCaseNotFound) {
			t.Errorf("a retry of no case: %v", err)
		}
		retry, err := FileAbuseNotice(ctx, &AbuseNoticeArgs{Class: model.AbuseNoticeClassRegistry, CaseId: &caseId})
		if err != nil {
			t.Fatal(err)
		}
		if retry.Opened || !retry.Paged || retry.CaseId != caseId {
			t.Errorf("the retry answered %+v", retry)
		}
		if count := testAbuseIntakeCaseCount(t, ctx); count != 1 {
			t.Errorf("%d cases after a retry, want 1", count)
		}
		attempts := notices.sink.Attempts()
		if len(attempts) != 2 || attempts[0].DedupKey != attempts[1].DedupKey || !attempts[0].Refused || attempts[1].Refused {
			t.Errorf("the pager saw %+v, want the same key refused then taken", attempts)
		}
		// the page's time is the notice's, not the retry's
		notifications := notices.sink.Notifications()
		if len(notifications) != 2 || !notifications[0].Page.Time.Equal(time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC)) {
			t.Errorf("paged %+v", notifications)
		}
		// a third filing of the same case while its page is open pages no one again
		if _, err := FileAbuseNotice(ctx, &AbuseNoticeArgs{Class: model.AbuseNoticeClassRegistry, CaseId: &caseId}); err != nil {
			t.Fatal(err)
		}
		if count := len(notices.sink.Notifications()); count != 2 {
			t.Errorf("%d notifications after a repeat of an open page, want 2", count)
		}
	})
}

// A process with no pager refuses the page and keeps the case, and the clock
// pass of a process that has one pages it: a crash between a notice's commit
// and the pager costs a minute, not the page. Once the case is reviewed the
// pass stops paging it, and past the repage window it pages no more.
func TestAbuseClockPassPagesANoticeWhosePageWasLost(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		filed := time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC)
		notices := newTestAbuseNotices(t, filed)
		restoreUnavailable := SetAbuseNoticePager(&unavailableAbuseNoticePager{})

		result, err := FileAbuseNotice(ctx, &AbuseNoticeArgs{Class: model.AbuseNoticeClassRegistrar, Target: testAbuseIntakeRoot})
		if !errors.Is(err, ErrAbuseNoticePagerUnavailable) || result == nil || result.Paged {
			t.Fatalf("with no pager: %+v, %v", result, err)
		}
		pass := RunAbuseClockPass(ctx, filed.Add(time.Minute), notices.settings)
		if pass.NoticePagedCount != 0 || pass.NoticeOwedCount != 1 {
			t.Errorf("a pass with no pager: %+v", pass)
		}
		restoreUnavailable()

		notices.now = filed.Add(2 * time.Minute)
		pass = RunAbuseClockPass(ctx, notices.now, notices.settings)
		if pass.NoticePagedCount != 1 || pass.NoticeOwedCount != 0 {
			t.Fatalf("a pass with a pager: %+v", pass)
		}
		notifications := notices.sink.Notifications()
		if len(notifications) != 2 || notifications[0].Page.DedupKey != pager.NoticeDedupKey(result.CaseId) || !notifications[0].Page.BothAtOnce {
			t.Fatalf("the pass paged %+v", notifications)
		}
		// the next pass repeats the key and wakes no one again
		RunAbuseClockPass(ctx, filed.Add(3*time.Minute), notices.settings)
		if count := len(notices.sink.Notifications()); count != 2 {
			t.Errorf("%d notifications after a second pass, want 2", count)
		}

		// past the window the pass leaves the notice to the overdue count
		late := filed.Add(notices.settings.NoticeRepageWindow + time.Minute)
		attemptCount := len(notices.sink.Attempts())
		pass = RunAbuseClockPass(ctx, late, notices.settings)
		if pass.NoticePagedCount != 0 || len(notices.sink.Attempts()) != attemptCount || pass.OverdueCount != 1 {
			t.Errorf("a pass past the window: %+v", pass)
		}

		// a reviewed notice is closed, and a closed case is paged no more
		reviewerId := server.NewId()
		if _, err := model.RecordAbuseReview(ctx, &model.AbuseReviewArgs{
			CaseId:     result.CaseId,
			ReviewerId: reviewerId,
			Reason:     "handled",
		}, filed.Add(4*time.Minute), notices.settings); err != nil {
			t.Fatal(err)
		}
		pass = RunAbuseClockPass(ctx, filed.Add(5*time.Minute), notices.settings)
		if pass.NoticePagedCount != 0 || pass.NoticeOwedCount != 0 || len(notices.sink.Attempts()) != attemptCount {
			t.Errorf("a pass after the review: %+v", pass)
		}
	})
}

// A clock past due warns and lifts nothing: the pass counts the case into
// urnetwork_abuse_review_overdue and leaves its action standing and pending,
// with no page.
func TestAbuseClockPassCountsOverdueReviewsAndLiftsNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		friday := time.Date(2026, 11, 13, 17, 0, 0, 0, time.UTC)
		notices := newTestAbuseNotices(t, friday)
		newTestAbuseDeps(t)

		networkId := server.NewId()
		host := "shop.quietnet.root.example"
		var caseId server.Id
		var err error
		server.Tx(ctx, func(tx server.PgTx) {
			caseId, _, err = model.OpenAbuseCaseInTx(ctx, tx, &model.AbuseCaseArgs{
				NetworkId:     &networkId,
				Category:      model.AbuseCategoryPhishing,
				Target:        model.AbuseTarget{Kind: model.AbuseTargetKindHost, Name: host},
				ReporterClass: model.AbuseReporterClassWebRisk,
				JoinKind:      model.AbuseJoinKindHost,
				JoinKey:       host,
			}, friday, notices.settings)
		})
		if err != nil {
			t.Fatal(err)
		}
		due := friday.Add(24 * time.Hour)
		applied, err := ApplyAbuseAction(ctx, &model.AbuseActionArgs{
			ActionId:      server.NewId(),
			CaseId:        caseId,
			Kind:          model.AbuseActionKindHostKill,
			Target:        host,
			ReviewDueTime: due,
		})
		if err != nil {
			t.Fatal(err)
		}

		pass := RunAbuseClockPass(ctx, due.Add(-time.Minute), notices.settings)
		if pass.OverdueCount != 0 || testutil.ToFloat64(abuseReviewOverdueGauge) != 0 {
			t.Errorf("before its clock: %+v, gauge %v", pass, testutil.ToFloat64(abuseReviewOverdueGauge))
		}
		pass = RunAbuseClockPass(ctx, due.Add(time.Minute), notices.settings)
		if pass.OverdueCount != 1 || testutil.ToFloat64(abuseReviewOverdueGauge) != 1 {
			t.Errorf("past its clock: %+v, gauge %v", pass, testutil.ToFloat64(abuseReviewOverdueGauge))
		}
		// weeks late, the action still stands and waits for a person
		RunAbuseClockPass(ctx, due.Add(30*24*time.Hour), notices.settings)
		action := model.GetAbuseAction(ctx, applied.Action.ActionId)
		if action == nil || action.ReviewState != model.AbuseReviewStatePending {
			t.Errorf("the overdue action is %+v, want it standing and pending", action)
		}
		if count := testAbuseNoticeActionCount(t, ctx); count != 1 {
			t.Errorf("%d action rows, want the one kill and no lift", count)
		}
		if abuseCase := model.GetAbuseCase(ctx, caseId); abuseCase == nil || abuseCase.State != model.AbuseCaseStateOpen {
			t.Errorf("the overdue case is %+v, want it open", abuseCase)
		}
		if attempts := notices.sink.Attempts(); len(attempts) != 0 {
			t.Errorf("an overdue review paged: %+v", attempts)
		}
	})
}

// A role mail pages only when its DKIM verified under a listed sender: an
// unlisted sender, a listed domain whose signature failed and mail to another
// address open no case and reach no pager, and no body is an argument.
func TestFileAbuseNoticeOfMailPagesOnlyAListedVerifiedSender(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer server.Testing_PushAbuseConfig("notice_senders:\n  registrar.example: registrar\n  police.example: law_enforcement\n")()
		notices := newTestAbuseNotices(t, time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC))

		quiet := []*model.AbuseNoticeMail{
			{RecipientLocalPart: "abuse", Signatures: []model.AbuseNoticeSignature{{Domain: "stranger.example", Verified: true}}},
			{RecipientLocalPart: "abuse", Signatures: []model.AbuseNoticeSignature{{Domain: "registrar.example", Verified: false}}},
			{RecipientLocalPart: "abuse"},
			{RecipientLocalPart: "sales", Signatures: []model.AbuseNoticeSignature{{Domain: "registrar.example", Verified: true}}},
			{RecipientLocalPart: "abuse", Signatures: []model.AbuseNoticeSignature{{Domain: "mail.registrar.example", Verified: true}}},
		}
		for i, mail := range quiet {
			result, err := FileAbuseNoticeOfMail(ctx, mail, testAbuseIntakeRoot)
			if result != nil || err != nil {
				t.Errorf("mail %d: %+v, %v, want no notice", i, result, err)
			}
		}
		if count := testAbuseIntakeCaseCount(t, ctx); count != 0 {
			t.Errorf("%d cases from mail that is no notice", count)
		}
		if attempts := notices.sink.Attempts(); len(attempts) != 0 {
			t.Errorf("the pager saw %d attempts from mail that is no notice", len(attempts))
		}

		result, err := FileAbuseNoticeOfMail(ctx, &model.AbuseNoticeMail{
			RecipientLocalPart: "postmaster",
			Signatures: []model.AbuseNoticeSignature{
				{Domain: "stranger.example", Verified: true},
				{Domain: "Registrar.Example.", Verified: true},
			},
		}, testAbuseIntakeRoot)
		if err != nil || result == nil || !result.Paged {
			t.Fatalf("a listed registrar's mail: %+v, %v", result, err)
		}
		abuseCase := model.GetAbuseCase(ctx, result.CaseId)
		if model.AbuseNoticeClassOfCase(abuseCase) != model.AbuseNoticeClassRegistrar || abuseCase.Target != testAbuseIntakeRoot {
			t.Errorf("the mail's case is %+v", abuseCase)
		}
		if count := len(notices.sink.Notifications()); count != 2 {
			t.Errorf("%d notifications, want both people", count)
		}
		if count := testAbuseNoticeActionCount(t, ctx); count != 0 {
			t.Errorf("a mailed notice recorded %d actions, want none", count)
		}
	})
}

// A notice naming a host under the root joins its network, so the rota's kill
// of that host takes §21's one action path under the notice's case; filing it
// still takes no action of its own.
func TestFileAbuseNoticeNamingAHostJoinsItsNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		notices := newTestAbuseNotices(t, time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC))
		newTestAbuseIntake(t)
		newTestAbuseDeps(t)
		networkId, zone := newTestAbuseIntakeNetwork(t, ctx)
		host := "shop." + zone

		result, err := FileAbuseNotice(ctx, &AbuseNoticeArgs{
			Class:    model.AbuseNoticeClassRegistrar,
			Target:   host,
			Category: model.AbuseCategoryPhishing,
		})
		if err != nil {
			t.Fatal(err)
		}
		abuseCase := model.GetAbuseCase(ctx, result.CaseId)
		if abuseCase == nil || abuseCase.NetworkId == nil || *abuseCase.NetworkId != networkId || abuseCase.Target != host {
			t.Fatalf("the case is %+v, want the host's network", abuseCase)
		}
		if count := testAbuseNoticeActionCount(t, ctx); count != 0 {
			t.Fatalf("filing recorded %d actions", count)
		}
		for _, notification := range notices.sink.Notifications() {
			if strings.Contains(notification.Page.Summary, host) || strings.Contains(notification.Page.Summary, networkId.String()) {
				t.Error("the page names the host or its network")
			}
		}

		// the rota's kill, as a person's action under the case
		reviewerId := server.NewId()
		applied, err := ApplyAbuseAction(ctx, &model.AbuseActionArgs{
			ActionId:     server.NewId(),
			CaseId:       result.CaseId,
			Kind:         model.AbuseActionKindHostKill,
			Target:       host,
			ReviewerId:   &reviewerId,
			ReviewReason: "the registrar's notice names this host",
		})
		if err != nil {
			t.Fatal(err)
		}
		if !applied.Applied || applied.Action.ByMachine || applied.Action.NetworkId == nil || *applied.Action.NetworkId != networkId {
			t.Errorf("the rota's kill is %+v", applied.Action)
		}
	})
}

// The staff route's file_notice: a reviewer or legal files, an unknown class
// is a bad request, and a page the pager did not take is answered with the
// case id and paged false, so the filer can file again.
func TestAbuseStaffFileNotice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		notices := newTestAbuseNotices(t, time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC))
		reviewer := &AbuseStaff{ReviewerId: server.NewId(), Roles: []AbuseStaffRole{AbuseStaffRoleReviewer}}
		engineer := &AbuseStaff{ReviewerId: server.NewId(), Roles: []AbuseStaffRole{AbuseStaffRoleEngineering}}

		body := func(fields map[string]any) []byte {
			fields["kind"] = "file_notice"
			encoded, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			return encoded
		}
		if _, err := AbuseStaffAction(ctx, engineer, body(map[string]any{"class": "registrar", "target": testAbuseIntakeRoot})); !errors.Is(err, ErrAbuseStaffRoleRefused) {
			t.Errorf("an engineer filing: %v", err)
		}
		if _, err := AbuseStaffAction(ctx, reviewer, body(map[string]any{"class": "search_console", "target": testAbuseIntakeRoot})); !errors.Is(err, ErrAbuseStaffRequestInvalid) {
			t.Errorf("a class that does not page: %v", err)
		}
		if _, err := AbuseStaffAction(ctx, reviewer, body(map[string]any{"class": "registrar", "target": testAbuseIntakeRoot, "url": "https://root.example/x"})); !errors.Is(err, ErrAbuseStaffRequestInvalid) {
			t.Errorf("a field the kind does not take: %v", err)
		}
		if attempts := notices.sink.Attempts(); len(attempts) != 0 {
			t.Fatalf("refused filings reached the pager: %+v", attempts)
		}

		notices.sink.Fail(errors.New("the pager service is down"))
		answer, err := AbuseStaffAction(ctx, reviewer, body(map[string]any{"class": "registrar", "target": testAbuseIntakeRoot}))
		if err != nil {
			t.Fatal(err)
		}
		owed, ok := answer.Result.(*AbuseStaffNoticeResult)
		if !ok || owed.Paged || !owed.Opened || owed.CaseId == (server.Id{}) || owed.Route != string(model.AbuseNoticeRouteRota) {
			t.Fatalf("during an outage the route answered %+v", answer.Result)
		}
		notices.sink.Fail(nil)
		answer, err = AbuseStaffAction(ctx, reviewer, body(map[string]any{"class": "registrar", "target": testAbuseIntakeRoot, "case_id": owed.CaseId.String()}))
		if err != nil {
			t.Fatal(err)
		}
		paged := answer.Result.(*AbuseStaffNoticeResult)
		if !paged.Paged || paged.Opened || paged.CaseId != owed.CaseId {
			t.Errorf("the second filing answered %+v", paged)
		}
		if count := testAbuseIntakeCaseCount(t, ctx); count != 1 {
			t.Errorf("%d cases, want 1", count)
		}
	})
}

// QD-1070: api and taskworker page notices through the webhook the monitor pages through, read from the monitor's
// vault file under a retrying sink. A file with no endpoint, one naming an endpoint off loopback while no pager
// service is chosen, and one that does not parse install nothing, the pager in force staying, and no error holds
// the endpoint; the monitor's loopback webhook takes a filed notice's page under its case's key before the filing
// answers paged.
func TestInstallAbuseNoticePagerPagesThroughTheMonitorsWebhook(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		events := make(chan map[string]any, 8)
		webhook := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			event := map[string]any{}
			if err := json.NewDecoder(request.Body).Decode(&event); err == nil {
				events <- event
			}
			writer.WriteHeader(http.StatusAccepted)
		}))
		defer webhook.Close()
		notices := newTestAbuseNotices(t, time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC))

		for _, monitorYml := range []string{
			"alerts: {}\n",
			"alerts:\n  pager_webhook_url: https://pager.example/events/synthetic-endpoint-token\n",
			"alerts: [synthetic-endpoint-token\n",
		} {
			if _, err := newAbuseNoticePagerSink([]byte(monitorYml)); !errors.Is(err, ErrAbuseNoticePagerRefused) || strings.Contains(err.Error(), "synthetic-endpoint-token") {
				t.Errorf("%q: %v", monitorYml, err)
			}
			pop := server.Vault.PushSimpleResource(AbuseNoticePagerFileName, []byte(monitorYml))
			restore := InstallAbuseNoticePager()
			if currentAbuseNoticePager() != notices.sink {
				t.Errorf("%q installed a pager", monitorYml)
			}
			restore()
			pop()
		}

		defer server.Vault.PushSimpleResource(AbuseNoticePagerFileName, []byte("alerts:\n  pager_webhook_url: "+webhook.URL+"/events\n"))()
		restore := InstallAbuseNoticePager()
		defer restore()
		if _, ok := currentAbuseNoticePager().(*pager.RetrySink); !ok {
			t.Fatalf("the monitor's webhook installed %T", currentAbuseNoticePager())
		}
		result, err := FileAbuseNotice(ctx, &AbuseNoticeArgs{Class: model.AbuseNoticeClassRegistrar, Target: testAbuseIntakeRoot})
		if err != nil || !result.Opened || !result.Paged {
			t.Fatalf("a registrar's notice through the webhook: %+v, %v", result, err)
		}
		// the filing answered paged once the webhook took the page, so the event is here
		select {
		case event := <-events:
			if event["action"] != "trigger" || event["dedup_key"] != pager.NoticeDedupKey(result.CaseId) || event["both_at_once"] != true {
				t.Fatalf("the webhook took %v", event)
			}
		default:
			t.Fatal("the filing answered paged with no page at the webhook")
		}
	})
}
