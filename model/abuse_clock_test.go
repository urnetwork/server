package model

import (
	"testing"
	"time"
)

// FS-49's clock half and §21.8's review column. Every test runs on explicit instants,
// so no test reads the wall clock or sleeps (reading 15). 2026-11-06 is a Friday.

func abuseTestCalendar(t *testing.T, hours AbuseReviewHours) *AbuseCalendar {
	t.Helper()
	calendar, err := NewAbuseCalendar(hours, DefaultAbusePolicySettings())
	if err != nil {
		t.Fatal(err)
	}
	return calendar
}

func abuseTestLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	location, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return location
}

func abuseTestUtc(month time.Month, day int, hour int, minute int) time.Time {
	return time.Date(2026, month, day, hour, minute, 0, 0, time.UTC)
}

func abuseTestHoursIn(zone string) AbuseReviewHours {
	hours := DefaultAbuseReviewHours()
	hours.Zone = zone
	return hours
}

func abuseTestDue(t *testing.T, got time.Time, want time.Time) {
	t.Helper()
	if !got.Equal(want) {
		t.Errorf("due %s, want %s", got, want)
	}
}

func TestAbuseCalendarDefaultHours(t *testing.T) {
	calendar := abuseTestCalendar(t, DefaultAbuseReviewHours())
	for _, c := range []struct {
		at   time.Time
		open bool
	}{
		{at: abuseTestUtc(time.November, 9, 9, 0), open: true},
		{at: abuseTestUtc(time.November, 9, 9, 0).Add(-time.Second), open: false},
		{at: abuseTestUtc(time.November, 9, 18, 0).Add(-time.Second), open: true},
		{at: abuseTestUtc(time.November, 9, 18, 0), open: false},
		{at: abuseTestUtc(time.November, 6, 12, 0), open: true},
		{at: abuseTestUtc(time.November, 7, 12, 0), open: false},
		{at: abuseTestUtc(time.November, 8, 12, 0), open: false},
	} {
		if open := calendar.IsOpen(c.at); open != c.open {
			t.Errorf("%s (%s): open %t", c.at, c.at.Weekday(), open)
		}
	}
}

// §21.17's 63 h: a kill at Friday's close waits for Monday's open, and no less.
func TestAbuseReviewDueKillAtFridayCloseWaitsForMondayOpen(t *testing.T) {
	calendar := abuseTestCalendar(t, DefaultAbuseReviewHours())
	killTime := abuseTestUtc(time.November, 6, 18, 0)
	due := AbuseReviewDue(AbuseSignalWebRiskHit, AbuseTierFree, AbuseStandingStandard, killTime, calendar)
	abuseTestDue(t, due, abuseTestUtc(time.November, 9, 9, 0))
	if elapsed := due.Sub(killTime); elapsed != 63*time.Hour {
		t.Fatalf("elapsed %s", elapsed)
	}
}

func TestAbuseReviewDueTakesEveryRowOfTheSignalTable(t *testing.T) {
	calendar := abuseTestCalendar(t, DefaultAbuseReviewHours())
	// a Tuesday, in hours, so no deadline here moves
	now := abuseTestUtc(time.November, 3, 10, 0)
	inAnHour := abuseTestUtc(time.November, 3, 11, 0)
	inFourOpenHours := abuseTestUtc(time.November, 3, 14, 0)
	inADay := abuseTestUtc(time.November, 4, 10, 0)
	inThreeDays := abuseTestUtc(time.November, 6, 10, 0)
	inAWeek := abuseTestUtc(time.November, 10, 10, 0)
	for _, c := range []struct {
		signal   AbuseSignal
		tier     AbuseTier
		standing AbuseStanding
		due      time.Time
	}{
		// machine actions, the tier and standing playing no part
		{signal: AbuseSignalMailJoinHigh, tier: AbuseTierFree, standing: AbuseStandingStandard, due: inADay},
		{signal: AbuseSignalMailJoinMedium, tier: AbuseTierPro, standing: AbuseStandingProbation, due: inADay},
		{signal: AbuseSignalWebRiskHit, tier: AbuseTierTeam, standing: AbuseStandingTrusted, due: inADay},
		{signal: AbuseSignalFeedHit, tier: AbuseTierFree, standing: AbuseStandingProbation, due: inADay},
		{signal: AbuseSignalTrustedFlaggerUrl, tier: AbuseTierPro, standing: AbuseStandingStandard, due: inADay},
		// paged notices
		{signal: AbuseSignalRegistrarNotice, tier: AbuseTierFree, standing: AbuseStandingStandard, due: inAnHour},
		{signal: AbuseSignalRegistryNotice, tier: AbuseTierPro, standing: AbuseStandingStandard, due: inAnHour},
		{signal: AbuseSignalRegistrarUrl, tier: AbuseTierTeam, standing: AbuseStandingTrusted, due: inAnHour},
		{signal: AbuseSignalRegistryUrl, tier: AbuseTierFree, standing: AbuseStandingProbation, due: inAnHour},
		{signal: AbuseSignalLawEnforcementEmergency, tier: AbuseTierFree, standing: AbuseStandingStandard, due: inAnHour},
		{signal: AbuseSignalEuRemovalOrder, tier: AbuseTierFree, standing: AbuseStandingStandard, due: inAnHour},
		{signal: AbuseSignalProductionOrder, tier: AbuseTierFree, standing: AbuseStandingStandard, due: inAnHour},
		// a threat report: 4 open hours on every tier
		{signal: AbuseSignalThreatReport, tier: AbuseTierFree, standing: AbuseStandingStandard, due: inFourOpenHours},
		{signal: AbuseSignalThreatReport, tier: AbuseTierPro, standing: AbuseStandingStandard, due: inFourOpenHours},
		{signal: AbuseSignalThreatReport, tier: AbuseTierTeam, standing: AbuseStandingTrusted, due: inFourOpenHours},
		{signal: AbuseSignalThreatReport, tier: AbuseTierFree, standing: AbuseStandingProbation, due: inFourOpenHours},
		// other: 4 open hours on probation, 72 h paid, 7 d Community
		{signal: AbuseSignalReport, tier: AbuseTierFree, standing: AbuseStandingProbation, due: inFourOpenHours},
		{signal: AbuseSignalReport, tier: AbuseTierPro, standing: AbuseStandingProbation, due: inFourOpenHours},
		{signal: AbuseSignalReport, tier: AbuseTierPro, standing: AbuseStandingStandard, due: inThreeDays},
		{signal: AbuseSignalReport, tier: AbuseTierTeam, standing: AbuseStandingTrusted, due: inThreeDays},
		{signal: AbuseSignalReport, tier: AbuseTierFree, standing: AbuseStandingStandard, due: inAWeek},
		// restricted takes probation's limits and not its clock (§21.7)
		{signal: AbuseSignalReport, tier: AbuseTierFree, standing: AbuseStandingRestricted, due: inAWeek},
		{signal: AbuseSignalReport, tier: AbuseTierPro, standing: AbuseStandingSuspended, due: inThreeDays},
		// a low join is reviewed and never halts, so it is an other row
		{signal: AbuseSignalMailJoinLow, tier: AbuseTierPro, standing: AbuseStandingStandard, due: inThreeDays},
		// a signal this release does not know is §21.8's last row
		{signal: AbuseSignal("unknown"), tier: AbuseTierFree, standing: AbuseStandingStandard, due: inAWeek},
		// a doubt takes the short clock
		{signal: AbuseSignalReport, tier: AbuseTier("unknown"), standing: AbuseStandingStandard, due: inFourOpenHours},
		{signal: AbuseSignalReport, tier: AbuseTier(""), standing: AbuseStandingStandard, due: inFourOpenHours},
		{signal: AbuseSignalReport, tier: AbuseTierPro, standing: AbuseStanding("unknown"), due: inFourOpenHours},
		{signal: AbuseSignalReport, tier: AbuseTierFree, standing: AbuseStanding(""), due: inFourOpenHours},
	} {
		due := AbuseReviewDue(c.signal, c.tier, c.standing, now, calendar)
		if !due.Equal(c.due) {
			t.Errorf("%q %q %q: due %s, want %s", c.signal, c.tier, c.standing, due, c.due)
		}
	}
}

// The pager runs at any hour, so a notice's hour is an hour even on a Saturday night.
func TestAbuseReviewDuePagedNoticeIgnoresTheCalendar(t *testing.T) {
	calendar := abuseTestCalendar(t, DefaultAbuseReviewHours())
	now := abuseTestUtc(time.November, 7, 3, 0)
	for _, signal := range []AbuseSignal{
		AbuseSignalRegistrarNotice,
		AbuseSignalRegistryNotice,
		AbuseSignalRegistrarUrl,
		AbuseSignalRegistryUrl,
		AbuseSignalLawEnforcementEmergency,
		AbuseSignalEuRemovalOrder,
		AbuseSignalProductionOrder,
	} {
		due := AbuseReviewDue(signal, AbuseTierFree, AbuseStandingStandard, now, calendar)
		if !due.Equal(abuseTestUtc(time.November, 7, 4, 0)) {
			t.Errorf("%q: due %s", signal, due)
		}
	}
}

// Open hours count only open hours, and a deadline spent exactly at close stays there.
func TestAbuseReviewDueOpenHoursSpanAWeekend(t *testing.T) {
	calendar := abuseTestCalendar(t, DefaultAbuseReviewHours())
	for _, c := range []struct {
		name string
		now  time.Time
		due  time.Time
	}{
		{name: "two left on friday", now: abuseTestUtc(time.November, 6, 16, 0), due: abuseTestUtc(time.November, 9, 11, 0)},
		{name: "at friday's close", now: abuseTestUtc(time.November, 6, 18, 0), due: abuseTestUtc(time.November, 9, 13, 0)},
		{name: "on saturday", now: abuseTestUtc(time.November, 7, 12, 0), due: abuseTestUtc(time.November, 9, 13, 0)},
		{name: "on sunday night", now: abuseTestUtc(time.November, 8, 23, 0), due: abuseTestUtc(time.November, 9, 13, 0)},
		{name: "before monday's open", now: abuseTestUtc(time.November, 9, 8, 0), due: abuseTestUtc(time.November, 9, 13, 0)},
		{name: "exactly four left", now: abuseTestUtc(time.November, 6, 14, 0), due: abuseTestUtc(time.November, 6, 18, 0)},
		{name: "a minute under four left", now: abuseTestUtc(time.November, 6, 14, 1), due: abuseTestUtc(time.November, 9, 9, 1)},
		{name: "overnight midweek", now: abuseTestUtc(time.November, 3, 17, 0), due: abuseTestUtc(time.November, 4, 12, 0)},
	} {
		due := AbuseReviewDue(AbuseSignalThreatReport, AbuseTierFree, AbuseStandingStandard, c.now, calendar)
		if !due.Equal(c.due) {
			t.Errorf("%s: due %s, want %s", c.name, due, c.due)
		}
	}
}

func TestAbuseReviewDueLandingOnASundayMovesToMonday(t *testing.T) {
	calendar := abuseTestCalendar(t, DefaultAbuseReviewHours())
	monday := abuseTestUtc(time.November, 9, 9, 0)

	// 72 h from a Thursday, paid
	now := abuseTestUtc(time.November, 5, 10, 0)
	due := AbuseReviewDue(AbuseSignalReport, AbuseTierPro, AbuseStandingStandard, now, calendar)
	abuseTestDue(t, due, monday)

	// 7 d from a Sunday, Community
	now = abuseTestUtc(time.November, 1, 10, 0)
	due = AbuseReviewDue(AbuseSignalReport, AbuseTierFree, AbuseStandingStandard, now, calendar)
	abuseTestDue(t, due, monday)
}

// An elapsed deadline moves only when it lands outside hours, and counts from when it
// started, in hours or not.
func TestAbuseReviewDueElapsedMovesOnlyOutsideHours(t *testing.T) {
	calendar := abuseTestCalendar(t, DefaultAbuseReviewHours())
	for _, c := range []struct {
		name string
		now  time.Time
		due  time.Time
	}{
		{name: "before open", now: abuseTestUtc(time.November, 2, 7, 0), due: abuseTestUtc(time.November, 3, 9, 0)},
		{name: "after close", now: abuseTestUtc(time.November, 3, 19, 0), due: abuseTestUtc(time.November, 5, 9, 0)},
		{name: "from a sunday, in hours on monday", now: abuseTestUtc(time.November, 8, 10, 0), due: abuseTestUtc(time.November, 9, 10, 0)},
		{name: "exactly at close", now: abuseTestUtc(time.November, 5, 18, 0), due: abuseTestUtc(time.November, 6, 18, 0)},
		{name: "a minute past close", now: abuseTestUtc(time.November, 5, 18, 1), due: abuseTestUtc(time.November, 9, 9, 0)},
		{name: "exactly at open", now: abuseTestUtc(time.November, 2, 9, 0), due: abuseTestUtc(time.November, 3, 9, 0)},
	} {
		due := AbuseReviewDue(AbuseSignalFeedHit, AbuseTierFree, AbuseStandingStandard, c.now, calendar)
		if !due.Equal(c.due) {
			t.Errorf("%s: due %s, want %s", c.name, due, c.due)
		}
	}
}

// A clock reads the zone, so the weekend clocks change on takes an hour more or less
// than 63 h, and Monday's open stays 09:00 on the wall.
func TestAbuseReviewDueAcrossAWeekendZoneChange(t *testing.T) {
	newYork := abuseTestLocation(t, "America/New_York")
	calendar := abuseTestCalendar(t, abuseTestHoursIn("America/New_York"))
	for _, c := range []struct {
		name    string
		now     time.Time
		due     time.Time
		elapsed time.Duration
	}{
		{
			name:    "clocks go back",
			now:     time.Date(2026, time.October, 30, 18, 0, 0, 0, newYork),
			due:     time.Date(2026, time.November, 2, 9, 0, 0, 0, newYork),
			elapsed: 64 * time.Hour,
		},
		{
			name:    "clocks go forward",
			now:     time.Date(2026, time.March, 6, 18, 0, 0, 0, newYork),
			due:     time.Date(2026, time.March, 9, 9, 0, 0, 0, newYork),
			elapsed: 62 * time.Hour,
		},
	} {
		due := AbuseReviewDue(AbuseSignalWebRiskHit, AbuseTierFree, AbuseStandingStandard, c.now, calendar)
		if !due.Equal(c.due) {
			t.Errorf("%s: due %s, want %s", c.name, due, c.due)
		}
		if elapsed := due.Sub(c.now); elapsed != c.elapsed {
			t.Errorf("%s: elapsed %s, want %s", c.name, elapsed, c.elapsed)
		}
	}
}

// 72 h are 72 h: across a zone change the deadline moves on the wall, where a fixed
// offset would have kept it.
func TestAbuseReviewDueElapsedKeepsItsLengthAcrossAZoneChange(t *testing.T) {
	newYork := abuseTestLocation(t, "America/New_York")
	calendar := abuseTestCalendar(t, abuseTestHoursIn("America/New_York"))

	now := time.Date(2026, time.October, 30, 12, 0, 0, 0, newYork)
	due := AbuseReviewDue(AbuseSignalReport, AbuseTierPro, AbuseStandingStandard, now, calendar)
	abuseTestDue(t, due, time.Date(2026, time.November, 2, 11, 0, 0, 0, newYork))

	now = time.Date(2026, time.March, 6, 12, 0, 0, 0, newYork)
	due = AbuseReviewDue(AbuseSignalReport, AbuseTierPro, AbuseStandingStandard, now, calendar)
	abuseTestDue(t, due, time.Date(2026, time.March, 9, 13, 0, 0, 0, newYork))

	// a day landing an hour before open only because the clocks went back
	everyDay := abuseTestHoursIn("America/New_York")
	everyDay.Days = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}
	calendar = abuseTestCalendar(t, everyDay)
	now = time.Date(2026, time.October, 31, 9, 30, 0, 0, newYork)
	due = AbuseReviewDue(AbuseSignalFeedHit, AbuseTierFree, AbuseStandingStandard, now, calendar)
	abuseTestDue(t, due, time.Date(2026, time.November, 1, 9, 0, 0, 0, newYork))
}

// Open hours are real hours, so a zone change inside them makes that day's hours
// longer or shorter than its wall clock says.
func TestAbuseReviewDueOpenHoursCountRealTimeAcrossAZoneChange(t *testing.T) {
	newYork := abuseTestLocation(t, "America/New_York")
	hours := abuseTestHoursIn("America/New_York")
	hours.Days = []string{"sun"}
	hours.Open = "00:00"
	hours.Close = "06:00"
	calendar := abuseTestCalendar(t, hours)

	// two to the change, then two more, reading an hour less on the wall when the
	// clocks go back and an hour more when they go forward
	now := time.Date(2026, time.November, 1, 0, 0, 0, 0, newYork)
	due := AbuseReviewDue(AbuseSignalThreatReport, AbuseTierFree, AbuseStandingStandard, now, calendar)
	abuseTestDue(t, due, time.Date(2026, time.November, 1, 3, 0, 0, 0, newYork))

	now = time.Date(2026, time.March, 8, 0, 0, 0, 0, newYork)
	due = AbuseReviewDue(AbuseSignalThreatReport, AbuseTierFree, AbuseStandingStandard, now, calendar)
	abuseTestDue(t, due, time.Date(2026, time.March, 8, 5, 0, 0, 0, newYork))

	// that morning holds five real hours, so a sixth spills into the next sunday
	settings := DefaultAbusePolicySettings()
	settings.ReportOpenReview = 6 * time.Hour
	long, err := NewAbuseCalendar(hours, settings)
	if err != nil {
		t.Fatal(err)
	}
	due = AbuseReviewDue(AbuseSignalThreatReport, AbuseTierFree, AbuseStandingStandard, now, long)
	abuseTestDue(t, due, time.Date(2026, time.March, 15, 1, 0, 0, 0, newYork))
}

// An open time the clocks skip opens at the change, whichever side time.Date answers.
func TestAbuseCalendarOpensAtTheChangeWhenTheClocksSkipItsOpen(t *testing.T) {
	hours := abuseTestHoursIn("America/New_York")
	hours.Days = []string{"sun"}
	hours.Open = "02:30"
	hours.Close = "06:00"
	calendar := abuseTestCalendar(t, hours)
	// 02:00 EST on 2026-03-08 becomes 03:00 EDT at 07:00 UTC
	change := abuseTestUtc(time.March, 8, 7, 0)
	if calendar.IsOpen(change.Add(-time.Second)) {
		t.Errorf("open just before the change")
	}
	if !calendar.IsOpen(change) {
		t.Errorf("closed at the change")
	}

	// time.Date lands on the far side of a skip east of UTC: 02:00 CET on 2026-03-29
	// becomes 03:00 CEST at 01:00 UTC
	hours = abuseTestHoursIn("Europe/Berlin")
	hours.Days = []string{"sun"}
	hours.Open = "02:30"
	hours.Close = "06:00"
	calendar = abuseTestCalendar(t, hours)
	change = abuseTestUtc(time.March, 29, 1, 0)
	if calendar.IsOpen(change.Add(-time.Second)) {
		t.Errorf("berlin: open just before the change")
	}
	if !calendar.IsOpen(change) {
		t.Errorf("berlin: closed at the change")
	}

	// a zone that skips midnight itself: Cuba's 00:00 becomes 01:00 at 05:00 UTC
	hours = abuseTestHoursIn("America/Havana")
	hours.Days = []string{"sun"}
	hours.Open = "00:00"
	hours.Close = "06:00"
	calendar = abuseTestCalendar(t, hours)
	change = abuseTestUtc(time.March, 8, 5, 0)
	if calendar.IsOpen(change.Add(-time.Second)) {
		t.Errorf("havana: open just before the change")
	}
	if !calendar.IsOpen(change) {
		t.Errorf("havana: closed at the change")
	}
}

// Hours wholly inside a skipped stretch do not exist that day, and the walk carries on
// to the next open date rather than spin on an empty window.
func TestAbuseCalendarHoursTheClocksSkipOpenNextTime(t *testing.T) {
	newYork := abuseTestLocation(t, "America/New_York")
	hours := abuseTestHoursIn("America/New_York")
	hours.Days = []string{"sun"}
	hours.Open = "02:15"
	hours.Close = "02:45"
	calendar := abuseTestCalendar(t, hours)
	now := time.Date(2026, time.March, 8, 0, 0, 0, 0, newYork)
	due := AbuseReviewDue(AbuseSignalThreatReport, AbuseTierFree, AbuseStandingStandard, now, calendar)
	// 30 open minutes a sunday, so 4 open hours is 8 sundays from March 15, the last
	// spent at its close
	abuseTestDue(t, due, time.Date(2026, time.May, 3, 2, 45, 0, 0, newYork))

	// an elapsed deadline landing before that empty morning waits for the next hours,
	// not for the change
	now = time.Date(2026, time.March, 7, 1, 0, 0, 0, newYork)
	due = AbuseReviewDue(AbuseSignalFeedHit, AbuseTierFree, AbuseStandingStandard, now, calendar)
	abuseTestDue(t, due, time.Date(2026, time.March, 15, 2, 15, 0, 0, newYork))
}

// An open time the clocks repeat opens on the first pass.
func TestAbuseCalendarRepeatedOpenTimeTakesItsFirstPass(t *testing.T) {
	hours := abuseTestHoursIn("America/New_York")
	hours.Days = []string{"sun"}
	hours.Open = "01:30"
	hours.Close = "06:00"
	calendar := abuseTestCalendar(t, hours)
	// 01:30 EDT on 2026-11-01 is 05:30 UTC, and 01:30 EST an hour later
	firstPass := abuseTestUtc(time.November, 1, 5, 30)
	if calendar.IsOpen(firstPass.Add(-time.Second)) {
		t.Errorf("open just before the first pass")
	}
	if !calendar.IsOpen(firstPass) {
		t.Errorf("closed on the first pass")
	}
	if !calendar.IsOpen(firstPass.Add(time.Hour)) {
		t.Errorf("closed on the second pass")
	}

	// time.Date answers the second pass east of UTC: 02:30 CEST on 2026-10-25 is 00:30
	// UTC, and 02:30 CET an hour later
	hours = abuseTestHoursIn("Europe/Berlin")
	hours.Days = []string{"sun"}
	hours.Open = "02:30"
	hours.Close = "06:00"
	calendar = abuseTestCalendar(t, hours)
	firstPass = abuseTestUtc(time.October, 25, 0, 30)
	if calendar.IsOpen(firstPass.Add(-time.Second)) {
		t.Errorf("berlin: open just before the first pass")
	}
	if !calendar.IsOpen(firstPass) {
		t.Errorf("berlin: closed on the first pass")
	}
}

// A calendar reads the date in its own zone, so hours late on a local Monday are found
// while UTC is already on Tuesday, and early ones while it is still on Sunday.
func TestAbuseCalendarReadsTheDateInItsZone(t *testing.T) {
	hours := abuseTestHoursIn("America/New_York")
	hours.Days = []string{"mon"}
	hours.Open = "21:00"
	hours.Close = "22:00"
	calendar := abuseTestCalendar(t, hours)
	// Monday 21:30 EST is Tuesday 02:30 UTC
	if !calendar.IsOpen(abuseTestUtc(time.November, 10, 2, 30)) {
		t.Errorf("new york: closed on a local monday evening")
	}

	hours = abuseTestHoursIn("Asia/Tokyo")
	hours.Days = []string{"mon"}
	hours.Open = "01:00"
	hours.Close = "02:00"
	calendar = abuseTestCalendar(t, hours)
	// Monday 01:30 JST is Sunday 16:30 UTC
	if !calendar.IsOpen(abuseTestUtc(time.November, 8, 16, 30)) {
		t.Errorf("tokyo: closed on a local monday morning")
	}
}

// `mode: always` gives M6's clocks: every instant open, so FS-49's weekend elapses at once.
func TestAbuseReviewDueModeAlwaysGivesTheClocksOfM6(t *testing.T) {
	calendar := abuseTestCalendar(t, AbuseReviewHours{Mode: AbuseReviewModeAlways})
	for _, at := range []time.Time{
		abuseTestUtc(time.November, 7, 3, 0),
		abuseTestUtc(time.November, 8, 12, 0),
		abuseTestUtc(time.November, 9, 8, 59),
	} {
		if !calendar.IsOpen(at) {
			t.Errorf("%s closed", at)
		}
	}
	for _, c := range []struct {
		name     string
		signal   AbuseSignal
		tier     AbuseTier
		standing AbuseStanding
		now      time.Time
		due      time.Time
	}{
		{
			name:     "a kill at friday's close",
			signal:   AbuseSignalWebRiskHit,
			tier:     AbuseTierFree,
			standing: AbuseStandingStandard,
			now:      abuseTestUtc(time.November, 6, 18, 0),
			due:      abuseTestUtc(time.November, 7, 18, 0),
		},
		{
			name:     "four hours on a saturday night",
			signal:   AbuseSignalThreatReport,
			tier:     AbuseTierFree,
			standing: AbuseStandingStandard,
			now:      abuseTestUtc(time.November, 7, 22, 0),
			due:      abuseTestUtc(time.November, 8, 2, 0),
		},
		{
			name:     "72 h landing on a sunday",
			signal:   AbuseSignalReport,
			tier:     AbuseTierPro,
			standing: AbuseStandingStandard,
			now:      abuseTestUtc(time.November, 5, 10, 0),
			due:      abuseTestUtc(time.November, 8, 10, 0),
		},
		{
			name:     "7 d landing on a sunday",
			signal:   AbuseSignalReport,
			tier:     AbuseTierFree,
			standing: AbuseStandingStandard,
			now:      abuseTestUtc(time.November, 1, 10, 0),
			due:      abuseTestUtc(time.November, 8, 10, 0),
		},
		{
			name:     "a notice",
			signal:   AbuseSignalRegistrarNotice,
			tier:     AbuseTierFree,
			standing: AbuseStandingStandard,
			now:      abuseTestUtc(time.November, 7, 3, 0),
			due:      abuseTestUtc(time.November, 7, 4, 0),
		},
	} {
		due := AbuseReviewDue(c.signal, c.tier, c.standing, c.now, calendar)
		if !due.Equal(c.due) {
			t.Errorf("%s: due %s, want %s", c.name, due, c.due)
		}
	}
}

// The same instant gets the same deadline, however the caller's time is zoned, and the
// answer is UTC, as every stored time is.
func TestAbuseReviewDueAnswersUtcForAnyInputZone(t *testing.T) {
	calendar := abuseTestCalendar(t, abuseTestHoursIn("Asia/Tokyo"))
	now := time.Date(2026, time.November, 6, 18, 0, 0, 0, abuseTestLocation(t, "Asia/Tokyo"))
	for _, at := range []time.Time{
		now,
		now.UTC(),
		now.In(abuseTestLocation(t, "America/New_York")),
		now.In(time.FixedZone("", 5*60*60+30*60)),
	} {
		for _, c := range []struct {
			signal AbuseSignal
			tier   AbuseTier
		}{
			{signal: AbuseSignalWebRiskHit, tier: AbuseTierFree},
			{signal: AbuseSignalRegistrarNotice, tier: AbuseTierFree},
			{signal: AbuseSignalThreatReport, tier: AbuseTierFree},
			{signal: AbuseSignalReport, tier: AbuseTierFree},
			{signal: AbuseSignalReport, tier: AbuseTierPro},
			{signal: AbuseSignalReport, tier: AbuseTier("unknown")},
		} {
			due := AbuseReviewDue(c.signal, c.tier, AbuseStandingStandard, at, calendar)
			want := AbuseReviewDue(c.signal, c.tier, AbuseStandingStandard, now, calendar)
			if !due.Equal(want) {
				t.Errorf("%q %q from %s: due %s, want %s", c.signal, c.tier, at, due, want)
			}
			if due.Location() != time.UTC {
				t.Errorf("%q %q from %s: in %s", c.signal, c.tier, at, due.Location())
			}
		}
	}
}

// The clocks read the calendar's settings instance, not §21.8's numbers.
func TestAbuseReviewDueReadsItsSettings(t *testing.T) {
	settings := DefaultAbusePolicySettings()
	settings.NoticeReview = 30 * time.Minute
	settings.MachineReview = 2 * time.Hour
	settings.ReportOpenReview = 1 * time.Hour
	settings.PaidReview = 3 * time.Hour
	settings.CommunityReview = 5 * time.Hour
	calendar, err := NewAbuseCalendar(DefaultAbuseReviewHours(), settings)
	if err != nil {
		t.Fatal(err)
	}
	now := abuseTestUtc(time.November, 3, 10, 0)
	for _, c := range []struct {
		signal AbuseSignal
		tier   AbuseTier
		due    time.Time
	}{
		{signal: AbuseSignalRegistrarNotice, tier: AbuseTierFree, due: abuseTestUtc(time.November, 3, 10, 30)},
		{signal: AbuseSignalWebRiskHit, tier: AbuseTierFree, due: abuseTestUtc(time.November, 3, 12, 0)},
		{signal: AbuseSignalThreatReport, tier: AbuseTierFree, due: abuseTestUtc(time.November, 3, 11, 0)},
		{signal: AbuseSignalReport, tier: AbuseTierPro, due: abuseTestUtc(time.November, 3, 13, 0)},
		{signal: AbuseSignalReport, tier: AbuseTierFree, due: abuseTestUtc(time.November, 3, 15, 0)},
	} {
		due := AbuseReviewDue(c.signal, c.tier, AbuseStandingStandard, now, calendar)
		if !due.Equal(c.due) {
			t.Errorf("%q %q: due %s, want %s", c.signal, c.tier, due, c.due)
		}
	}
}

func TestNewAbuseCalendarRefusesABadBlock(t *testing.T) {
	block := func(change func(hours *AbuseReviewHours)) AbuseReviewHours {
		hours := DefaultAbuseReviewHours()
		change(&hours)
		return hours
	}
	for _, c := range []struct {
		name  string
		hours AbuseReviewHours
	}{
		{name: "no mode", hours: block(func(hours *AbuseReviewHours) { hours.Mode = "" })},
		{name: "unknown mode", hours: block(func(hours *AbuseReviewHours) { hours.Mode = "sometimes" })},
		{name: "no zone", hours: block(func(hours *AbuseReviewHours) { hours.Zone = "" })},
		{name: "the host's zone", hours: block(func(hours *AbuseReviewHours) { hours.Zone = "Local" })},
		{name: "unknown zone", hours: block(func(hours *AbuseReviewHours) { hours.Zone = "Mars/Olympus_Mons" })},
		{name: "no open", hours: block(func(hours *AbuseReviewHours) { hours.Open = "" })},
		{name: "no close", hours: block(func(hours *AbuseReviewHours) { hours.Close = "" })},
		{name: "a bad open", hours: block(func(hours *AbuseReviewHours) { hours.Open = "9am" })},
		{name: "a close past the day", hours: block(func(hours *AbuseReviewHours) { hours.Close = "24:00" })},
		{name: "empty hours", hours: block(func(hours *AbuseReviewHours) { hours.Close = hours.Open })},
		{name: "overnight hours", hours: block(func(hours *AbuseReviewHours) { hours.Open, hours.Close = "22:00", "06:00" })},
		{name: "no days", hours: block(func(hours *AbuseReviewHours) { hours.Days = nil })},
		{name: "an unknown day", hours: block(func(hours *AbuseReviewHours) { hours.Days = []string{"mon", "someday"} })},
	} {
		calendar, err := NewAbuseCalendar(c.hours, DefaultAbusePolicySettings())
		if err == nil {
			t.Errorf("%s: no error", c.name)
		}
		if calendar != nil {
			t.Errorf("%s: a calendar", c.name)
		}
	}
}

func TestNewAbuseCalendarReadsDayNames(t *testing.T) {
	hours := DefaultAbuseReviewHours()
	hours.Days = []string{"Monday", "SAT", "sunday"}
	calendar := abuseTestCalendar(t, hours)
	for _, c := range []struct {
		at   time.Time
		open bool
	}{
		{at: abuseTestUtc(time.November, 2, 10, 0), open: true},
		{at: abuseTestUtc(time.November, 3, 10, 0), open: false},
		{at: abuseTestUtc(time.November, 7, 10, 0), open: true},
		{at: abuseTestUtc(time.November, 8, 10, 0), open: true},
	} {
		if open := calendar.IsOpen(c.at); open != c.open {
			t.Errorf("%s (%s): open %t", c.at, c.at.Weekday(), open)
		}
	}
}

// An always calendar reads nothing else, so a stale block under it cannot refuse.
func TestNewAbuseCalendarAlwaysReadsNoOtherField(t *testing.T) {
	_, err := NewAbuseCalendar(
		AbuseReviewHours{Mode: AbuseReviewModeAlways, Zone: "Mars/Olympus_Mons", Open: "late"},
		DefaultAbusePolicySettings(),
	)
	if err != nil {
		t.Fatal(err)
	}
}
