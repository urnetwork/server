// SPDX-License-Identifier: MPL-2.0

package onboarding

// Where a reader's campaign emails are placed in the day, from the best
// source the server has (the campaign review of 2026-11-13: 18% of
// re-engagement emails went at 10:00 UTC, 02:00-07:00 in the Americas,
// because only a still-running onboarding row kept a zone). The controller
// resolves the source; this file turns each kind into a SendZone:
//
//   - the reader's own zone (ZoneSendZone): what a device reported, or the
//     sign-up IP's;
//   - the reader's country (CountrySendZone), from a table generated from
//     IANA's zone1970.tab and GeoNames' city populations (gencountry): a
//     country with one clock, or whose zone serves the dominant share of its
//     people, is that zone; any other (the United States, Canada, Mexico,
//     Russia, Australia, Indonesia, ...) keeps its most populous zone and
//     narrows the send window, so that a send inside it is inside the window
//     in every populated zone of the country;
//   - the time of day the reader was last seen (ActivitySendZone): a frame in
//     which that moment reads as a chosen local time, when nothing names a
//     zone.

//go:generate go run ./gencountry -inputs $URNETWORK_COUNTRY_INPUTS

import (
	"fmt"
	"strings"
	"time"
)

// countryZone is one row of the generated country table.
type countryZone struct {
	// an IANA zone, as the country's own cities name it
	zone string
	// the most the zone is ever ahead of the country's westernmost populated
	// zone, and the most its easternmost populated zone is ever ahead of it;
	// 0 for a country with one clock or a dominant zone
	openLaterMinutes    int
	closeEarlierMinutes int
}

// SendZone is where one reader's sends are placed: the location the send
// hour and the window are read in, and how much of the window the reader's
// country leaves usable. The zero value places in UTC with the whole window.
type SendZone struct {
	// nil is UTC
	Location *time.Location
	// the window opens this much later and closes this much earlier (a
	// country's zone, CountrySendZone); 0 for the reader's own zone
	OpenLater    time.Duration
	CloseEarlier time.Duration
	// Location is a frame placed by the reader's activity
	// (ActivitySendZone), not a zone the reader keeps: dates in the emails
	// are written in UTC, as for an unknown zone
	Frame bool
}

// ZoneSendZone places sends in a reader's own zone.
func ZoneSendZone(loc *time.Location) SendZone {
	return SendZone{Location: loc}
}

// CountryZoneName is the zone the country's readers' sends are placed in
// when their own is unknown, or "" for a code the table does not know.
// country is alpha-2, in any case.
func CountryZoneName(country string) string {
	return countryCodeZones[strings.ToUpper(strings.TrimSpace(country))].zone
}

// CountrySendZone places sends for a reader known only by country: in the
// country's zone, with the window narrowed so that a send inside it is
// inside the window in every populated zone of the country. ok is false for
// a code the table does not know, or a zone the time package cannot load.
func CountrySendZone(country string) (zone SendZone, ok bool) {
	row, ok := countryCodeZones[strings.ToUpper(strings.TrimSpace(country))]
	if !ok {
		return SendZone{}, false
	}
	loc, err := time.LoadLocation(row.zone)
	if err != nil {
		return SendZone{}, false
	}
	return SendZone{
		Location:     loc,
		OpenLater:    time.Duration(row.openLaterMinutes) * time.Minute,
		CloseEarlier: time.Duration(row.closeEarlierMinutes) * time.Minute,
	}, true
}

// ActivitySendZone is the frame of a reader whose zone and country are both
// unknown: a fixed offset in which `at`, a moment the reader was active (the
// sign-up, the last connection), reads as localHour:localMinute. A send placed
// at that local time goes at the time of day the reader was last seen awake,
// and the window is read around it rather than in UTC.
func ActivitySendZone(at time.Time, localHour int, localMinute int) SendZone {
	at = at.UTC()
	offset := time.Duration(localHour)*time.Hour + time.Duration(localMinute)*time.Minute -
		(time.Duration(at.Hour())*time.Hour + time.Duration(at.Minute())*time.Minute)
	// the nearest offset a zone can have, -12 h through +14 h
	for offset < -12*time.Hour {
		offset += 24 * time.Hour
	}
	for 14*time.Hour < offset {
		offset -= 24 * time.Hour
	}
	sign, magnitude := "+", offset
	if offset < 0 {
		sign, magnitude = "-", -offset
	}
	// the frame's name only shows in tooling: the emails write a frame's
	// dates in UTC (DisplayLocation)
	name := fmt.Sprintf("UTC%s%02d:%02d", sign, int(magnitude/time.Hour), int(magnitude%time.Hour/time.Minute))
	return SendZone{
		Location: time.FixedZone(name, int(offset/time.Second)),
		Frame:    true,
	}
}

// Loc is the location sends are placed in; UTC for the zero value.
func (self SendZone) Loc() *time.Location {
	if self.Location == nil {
		return time.UTC
	}
	return self.Location
}

// DisplayLocation is the location the emails write dates in: the zone, or
// UTC for a frame.
func (self SendZone) DisplayLocation() *time.Location {
	if self.Frame {
		return time.UTC
	}
	return self.Loc()
}

// Middle is the local time halfway through the window.
func (w SendWindow) Middle() (hour int, minute int) {
	middle := (w.startMinutes() + w.endMinutes()) / 2
	return middle / 60, middle % 60
}

// Window is w narrowed by the zone's OpenLater and CloseEarlier. When the
// country's zones span more than the window, no moment is inside it in all
// of them, and the window stays w: the country's own zone keeps its day.
func (self SendZone) Window(w SendWindow) SendWindow {
	if self.OpenLater <= 0 && self.CloseEarlier <= 0 {
		return w
	}
	start := w.startMinutes() + int(self.OpenLater/time.Minute)
	end := w.endMinutes() - int(self.CloseEarlier/time.Minute)
	if end <= start || 24*60 <= start || end < 0 {
		return w
	}
	return SendWindow{StartHour: start / 60, StartMinute: start % 60, EndHour: end / 60, EndMinute: end % 60}
}
