package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// FS-49 (§22.5's §21 row; §21.8, §21.17, D-ABUSE-3, D-ABUSE-5, owner decisions 95, 134, 143; ABUSE-M5-21): abuse
// clocks on a fake clock across a weekend, and every registrar or registry notice paging. The clock's single rows
// are abuse_clock_test.go's and the pages through pager.FakeSink are controller/abuse_notice_controller_test.go's;
// this runs one weekend whole, pure and then on MG5's abuse_case, on abuse.yml's default calendar (Monday to
// Friday, 09:00 to 18:00 UTC):
//
//	opened     signal                              due         why
//	Fri 18:00  a Web Risk hit's kill               Mon 09:00   24 h elapsed lands Sat 18:00, so the next open: 63 h
//	Sat 03:00  a high mail join's halt             Mon 09:00   24 h elapsed lands Sun 03:00
//	Sat 03:00  each paging notice class            Sat 04:00   the pager's hour, whatever the calendar
//	Sat 03:00  a threat report                     Mon 13:00   4 open hours
//	Sun 12:00  a probation network's report        Mon 13:00   4 open hours
//	Fri 18:00  a paid network's report             Mon 18:00   72 h lands on Monday's close, the last instant of its hours
//	Fri 18:00  a Community network's report        Fri+7 18:00 7 d lands on the next Friday's close
//
// A clock past due raises the overdue count, a business-hours warning, and lifts nothing: every case stays open.
// Every class pages both people, registrar and registry notices above all (owner decisions 134, 143); removal
// orders and registrar and registry notices stay with the rota, and production orders and law-enforcement
// emergencies, which are data requests, go to counsel.

// The weekend's instants: 2026-11-06 is a Friday.
var (
	testFs49Friday1800     = time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)
	testFs49Saturday0300   = time.Date(2026, 11, 7, 3, 0, 0, 0, time.UTC)
	testFs49Sunday1200     = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	testFs49Monday0900     = time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	testFs49Monday1300     = time.Date(2026, 11, 9, 13, 0, 0, 0, time.UTC)
	testFs49Monday1800     = time.Date(2026, 11, 9, 18, 0, 0, 0, time.UTC)
	testFs49NextFriday1800 = time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC)
)

// One row of the schedule. notice is the class a notice's row is filed under, none for every other row.
type testFs49Row struct {
	name     string
	signal   AbuseSignal
	tier     AbuseTier
	standing AbuseStanding
	opened   time.Time
	due      time.Time
	notice   AbuseNoticeClass
}

// The schedule in the file header, the notice classes each a row.
func testFs49Rows() []*testFs49Row {
	rows := []*testFs49Row{
		{"a Web Risk hit's kill", AbuseSignalWebRiskHit, AbuseTierFree, AbuseStandingStandard, testFs49Friday1800, testFs49Monday0900, AbuseNoticeClassNone},
		{"a high mail join's halt", AbuseSignalMailJoinHigh, AbuseTierPro, AbuseStandingStandard, testFs49Saturday0300, testFs49Monday0900, AbuseNoticeClassNone},
		{"a threat report", AbuseSignalThreatReport, AbuseTierPro, AbuseStandingStandard, testFs49Saturday0300, testFs49Monday1300, AbuseNoticeClassNone},
		{"a probation network's report", AbuseSignalReport, AbuseTierFree, AbuseStandingProbation, testFs49Sunday1200, testFs49Monday1300, AbuseNoticeClassNone},
		{"a paid network's report", AbuseSignalReport, AbuseTierPro, AbuseStandingStandard, testFs49Friday1800, testFs49Monday1800, AbuseNoticeClassNone},
		{"a Community network's report", AbuseSignalReport, AbuseTierFree, AbuseStandingStandard, testFs49Friday1800, testFs49NextFriday1800, AbuseNoticeClassNone},
	}
	for _, class := range testFs49NoticeClasses {
		rows = append(rows, &testFs49Row{"a " + string(class) + " notice", class.Signal(), "", "", testFs49Saturday0300, testFs49Saturday0300.Add(time.Hour), class})
	}
	return rows
}

// Every class a notice can be filed under, each of which pages.
var testFs49NoticeClasses = []AbuseNoticeClass{
	AbuseNoticeClassRegistrar,
	AbuseNoticeClassRegistry,
	AbuseNoticeClassLawEnforcementEmergency,
	AbuseNoticeClassEuRemovalOrder,
	AbuseNoticeClassProductionOrder,
}

// The weekend's clocks, pure: each row comes due when the header says, and every registrar or registry notice, like
// every other class the form offers, pages, its review ignoring the calendar; only a production order leaves the
// rota for counsel.
func TestFs49WeekendClocks(t *testing.T) {
	calendar, err := NewAbuseCalendar(DefaultAbuseReviewHours(), DefaultAbusePolicySettings())
	if err != nil {
		t.Fatal(err)
	}
	if testFs49Friday1800.Weekday() != time.Friday || testFs49NextFriday1800.Weekday() != time.Friday {
		t.Fatal("the weekend's days are not the days named")
	}
	for _, row := range testFs49Rows() {
		if due := AbuseReviewDue(row.signal, row.tier, row.standing, row.opened, calendar); !due.Equal(row.due) {
			t.Errorf("%s opened %s: due %s, want %s", row.name, row.opened.Weekday(), due, row.due)
		}
	}
	// §21.17's 63 h for a kill at Friday's close
	if wait := testFs49Monday0900.Sub(testFs49Friday1800); wait != 63*time.Hour {
		t.Errorf("a kill at Friday's close waits %s", wait)
	}
	for _, class := range testFs49NoticeClasses {
		if !class.Pages() {
			t.Errorf("%s does not page", class)
		}
		dataRequest := class == AbuseNoticeClassProductionOrder || class == AbuseNoticeClassLawEnforcementEmergency
		if route := class.Route(); dataRequest != (route == AbuseNoticeRouteCounsel) {
			t.Errorf("%s routes to %s", class, route)
		}
	}
	if AbuseNoticeClassNone.Pages() {
		t.Error("no class pages")
	}
}

// The same weekend on MG5's abuse_case, in a build with sidecar_pending: each row's case opened at its instant with
// its clock, the notices through OpenAbuseNoticeCase, and an hourly walk of the fake clock from Friday's close to the
// next Monday counting the cases past due, each exactly once its due has passed. Nothing the walk does closes a case.
func TestFs49WeekendClocksOnPostgres(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultAbuseSettings()
		calendar := abuseReportCalendar(settings)
		rows := testFs49Rows()
		caseIds := []server.Id{}
		for _, row := range rows {
			var caseId server.Id
			if row.notice != AbuseNoticeClassNone {
				noticeCase, err := OpenAbuseNoticeCase(ctx, &AbuseNoticeCaseArgs{Class: row.notice, Target: "root.example"}, row.opened, settings)
				if err != nil {
					t.Fatalf("%s: %v", row.name, err)
				}
				if !noticeCase.ReviewDueTime.Equal(row.due) {
					t.Fatalf("%s: due %s", row.name, noticeCase.ReviewDueTime)
				}
				caseId = noticeCase.CaseId
			} else {
				networkId := server.NewId()
				var err error
				server.Tx(ctx, func(tx server.PgTx) {
					caseId, _, err = OpenAbuseCaseInTx(ctx, tx, &AbuseCaseArgs{
						NetworkId:     &networkId,
						Category:      AbuseCategoryPhishing,
						Target:        AbuseTarget{Kind: AbuseTargetKindHost, Name: "shop.root.example"},
						ReporterClass: AbuseReporterClassWebRisk,
						JoinKind:      AbuseJoinKindHost,
						JoinKey:       "shop.root.example",
						ReviewDueTime: AbuseReviewDue(row.signal, row.tier, row.standing, row.opened, calendar),
					}, row.opened, settings)
				})
				if err != nil {
					t.Fatalf("%s: %v", row.name, err)
				}
			}
			caseIds = append(caseIds, caseId)
		}

		for now := testFs49Friday1800.Add(-time.Hour); !now.After(testFs49NextFriday1800.Add(time.Hour)); now = now.Add(time.Hour) {
			want := int64(0)
			for _, row := range rows {
				// a case counts from the hour its clock passed, and only once opened
				if !row.opened.After(now) && row.due.Before(now) {
					want += 1
				}
			}
			if count := CountOverdueAbuseReviews(ctx, now); count != want {
				t.Fatalf("%s %s: %d reviews overdue, want %d", now.Weekday(), now.Format("15:04"), count, want)
			}
		}
		for i, caseId := range caseIds {
			if abuseCase := GetAbuseCase(ctx, caseId); abuseCase == nil || abuseCase.State != AbuseCaseStateOpen {
				t.Errorf("%s: the walk left the case %+v", rows[i].name, abuseCase)
			}
		}
	})
}
