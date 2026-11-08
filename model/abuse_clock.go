package model

// §21.8's review clocks as pure functions on an injected time, so FS-49 runs with no
// store. In v0 people review in business hours, and an action holds until they do
// (owner decisions 95, 134), so a clock says when a review is due and a missed one
// only alarms: nothing here lifts anything.
//
// Two kinds of time meet here (reading 1). An elapsed deadline counts every hour, and
// one landing outside hours moves to the next open, which is how a kill at Friday's
// close comes to wait about 63 h (§21.17). A deadline in open hours counts only those.
// Both read the wall clock in the calendar's zone, never a fixed offset, so a zone
// change moves an elapsed deadline on the wall and leaves its length alone.

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// abuse.yml's review.mode values.
const (
	// people review in the calendar's hours (v0)
	AbuseReviewModeBusiness = "business"
	// every instant is open, which M6 turns on
	AbuseReviewModeAlways = "always"
)

// abuse.yml's review block. ABUSE-M5-03's server.AbuseConfig reads it from the file;
// this is the shape a calendar is built from.
type AbuseReviewHours struct {
	// AbuseReviewModeBusiness or AbuseReviewModeAlways
	Mode string
	// an IANA zone name
	Zone string
	// open weekdays, by full or three-letter name in any case
	Days []string
	// local HH:MM, open before close: hours do not run past midnight, and 24/7 is
	// AbuseReviewModeAlways
	Open  string
	Close string
}

// Monday to Friday 09:00 to 18:00, which gives §21.17's 63 h (reading 2). No holidays.
func DefaultAbuseReviewHours() AbuseReviewHours {
	return AbuseReviewHours{
		Mode:  AbuseReviewModeBusiness,
		Zone:  "UTC",
		Days:  []string{"mon", "tue", "wed", "thu", "fri"},
		Open:  "09:00",
		Close: "18:00",
	}
}

// When people are on, and how long each review may take. Immutable once built, so
// safe for concurrent use.
type AbuseCalendar struct {
	settings *AbusePolicySettings
	always   bool
	location *time.Location
	// by time.Weekday
	openWeekdays [7]bool
	// minutes past local midnight
	openMinute  int
	closeMinute int
}

// A bad block is an error rather than a guess: a wrong calendar fails silently, since
// nothing alarms on a deadline that was set too late. An always calendar reads no
// other field of the block.
func NewAbuseCalendar(hours AbuseReviewHours, settings *AbusePolicySettings) (*AbuseCalendar, error) {
	switch hours.Mode {
	case AbuseReviewModeAlways:
		return &AbuseCalendar{settings: settings, always: true}, nil
	case AbuseReviewModeBusiness:
	default:
		return nil, fmt.Errorf("abuse review mode %q unknown", hours.Mode)
	}

	// LoadLocation reads "" as UTC and "Local" as the host's zone, neither of them
	// a zone the file named
	if hours.Zone == "" || hours.Zone == "Local" {
		return nil, fmt.Errorf("abuse review zone %q unknown", hours.Zone)
	}
	location, err := time.LoadLocation(hours.Zone)
	if err != nil {
		return nil, fmt.Errorf("abuse review zone %q unknown: %w", hours.Zone, err)
	}

	minute := func(value string) (int, error) {
		clock, err := time.Parse("15:04", value)
		if err != nil {
			return 0, fmt.Errorf("abuse review time %q unknown: %w", value, err)
		}
		return clock.Hour()*60 + clock.Minute(), nil
	}
	openMinute, err := minute(hours.Open)
	if err != nil {
		return nil, err
	}
	closeMinute, err := minute(hours.Close)
	if err != nil {
		return nil, err
	}
	if closeMinute <= openMinute {
		return nil, fmt.Errorf("abuse review opens at %q and closes at %q", hours.Open, hours.Close)
	}

	var openWeekdays [7]bool
	for _, day := range hours.Days {
		found := false
		for weekday := time.Sunday; weekday <= time.Saturday; weekday += 1 {
			name := strings.ToLower(weekday.String())
			if strings.ToLower(day) == name || strings.ToLower(day) == name[:3] {
				openWeekdays[weekday] = true
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("abuse review day %q unknown", day)
		}
	}
	if len(hours.Days) == 0 {
		return nil, errors.New("abuse review names no open day")
	}

	return &AbuseCalendar{
		settings:     settings,
		location:     location,
		openWeekdays: openWeekdays,
		openMinute:   openMinute,
		closeMinute:  closeMinute,
	}, nil
}

// Hours run from open up to but not including close.
func (self *AbuseCalendar) IsOpen(at time.Time) bool {
	if self.always {
		return true
	}
	openTime, _ := self.nextHours(at)
	return !at.Before(openTime)
}

// The first open hours ending after at. Validation left an open weekday and a zone
// change empties one date's hours at most, so the walk ends within two weeks.
func (self *AbuseCalendar) nextHours(at time.Time) (openTime time.Time, closeTime time.Time) {
	year, month, day := at.In(self.location).Date()
	for i := 0; ; i += 1 {
		date := time.Date(year, month, day+i, 0, 0, 0, 0, time.UTC)
		if !self.openWeekdays[date.Weekday()] {
			continue
		}
		openTime = self.wallInstant(date, self.openMinute)
		closeTime = self.wallInstant(date, self.closeMinute)
		if openTime.Before(closeTime) && at.Before(closeTime) {
			return openTime, closeTime
		}
	}
}

// The first instant of a local date whose wall clock reaches a minute of the day.
// time.Date may answer either side of a zone change, so a wall clock the change skips
// takes the instant of the change, and one it repeats takes its first pass.
func (self *AbuseCalendar) wallInstant(date time.Time, minute int) time.Time {
	year, month, day := date.Date()
	instant := time.Date(year, month, day, minute/60, minute%60, 0, 0, self.location)
	startTime, endTime := instant.ZoneBounds()

	wallTime := time.Date(
		instant.Year(), instant.Month(), instant.Day(),
		instant.Hour(), instant.Minute(), 0, 0, time.UTC,
	)
	askedTime := date.Add(time.Duration(minute) * time.Minute)
	switch {
	case wallTime.After(askedTime):
		return startTime
	case wallTime.Before(askedTime):
		return endTime
	}

	if !startTime.IsZero() {
		_, offset := instant.Zone()
		_, offsetBefore := startTime.Add(-time.Second).Zone()
		firstPass := instant.Add(time.Duration(offset-offsetBefore) * time.Second)
		if firstPass.Before(startTime) {
			return firstPass
		}
	}
	return instant
}

// An elapsed deadline, moved to the next open if it lands outside hours. A deadline
// at close stays: people are on until then.
func (self *AbuseCalendar) afterElapsed(at time.Time, elapsed time.Duration) time.Time {
	due := at.Add(elapsed)
	if self.always {
		return due
	}
	openTime, _ := self.nextHours(due.Add(-time.Nanosecond))
	if due.Before(openTime) {
		return openTime
	}
	return due
}

// A deadline counting only open hours. Each pass spends one day's hours, measured as
// real time, so a zone change inside them counts for what it is.
func (self *AbuseCalendar) afterOpenHours(at time.Time, remaining time.Duration) time.Time {
	if self.always {
		return at.Add(remaining)
	}
	for {
		openTime, closeTime := self.nextHours(at)
		if at.Before(openTime) {
			at = openTime
		}
		span := closeTime.Sub(at)
		if remaining <= span {
			return at.Add(remaining)
		}
		remaining -= span
		at = closeTime
	}
}

// When a review is due, as a UTC instant, for a case opened at now (§21.8). A paged
// notice ignores the calendar, because the pager runs at any hour.
//
// Off a named row, every doubt takes the short clock: a standing or tier this does
// not know reviews early, which costs a reviewer minutes, where a clock too long
// could cost a customer or the root.
func AbuseReviewDue(
	signal AbuseSignal,
	tier AbuseTier,
	standing AbuseStanding,
	now time.Time,
	calendar *AbuseCalendar,
) time.Time {
	settings := calendar.settings
	switch signal {
	case AbuseSignalRegistrarNotice, AbuseSignalRegistryNotice,
		AbuseSignalRegistrarUrl, AbuseSignalRegistryUrl,
		AbuseSignalLawEnforcementEmergency, AbuseSignalEuRemovalOrder, AbuseSignalProductionOrder:
		return now.Add(settings.NoticeReview).UTC()

	case AbuseSignalMailJoinHigh, AbuseSignalMailJoinMedium,
		AbuseSignalWebRiskHit, AbuseSignalFeedHit, AbuseSignalTrustedFlaggerUrl:
		return calendar.afterElapsed(now, settings.MachineReview).UTC()

	case AbuseSignalThreatReport:
		return calendar.afterOpenHours(now, settings.ReportOpenReview).UTC()
	}

	// §21.8's last row. Restricted takes probation's limits and not its clock (§21.7).
	offProbation := false
	switch standing {
	case AbuseStandingStandard, AbuseStandingTrusted, AbuseStandingRestricted, AbuseStandingSuspended:
		offProbation = true
	}
	switch {
	case offProbation && (tier == AbuseTierPro || tier == AbuseTierTeam):
		return calendar.afterElapsed(now, settings.PaidReview).UTC()
	case offProbation && tier == AbuseTierFree:
		return calendar.afterElapsed(now, settings.CommunityReview).UTC()
	}
	return calendar.afterOpenHours(now, settings.ReportOpenReview).UTC()
}
