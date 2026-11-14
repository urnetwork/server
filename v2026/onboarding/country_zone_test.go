// SPDX-License-Identifier: MPL-2.0

package onboarding

import (
	"testing"
	"time"
)

// The table covers every ISO 3166-1 country and Kosovo (the codes GeoLite2
// and the stores report), and every zone it names loads from the time
// package, as the server's embedded database (server/tzdata.go) carries it.
func TestCountryZoneTableCoversCountriesWithLoadableZones(t *testing.T) {
	for _, country := range []string{
		"US", "CA", "MX", "BR", "AR", "GB", "DE", "FR", "ES", "IT", "NL", "CH", "SE", "PL", "UA", "RU", "TR",
		"IN", "PK", "BD", "CN", "JP", "KR", "TW", "HK", "ID", "PH", "VN", "TH", "MY", "SG", "AU", "NZ",
		"ZA", "NG", "KE", "EG", "MA", "SA", "AE", "IL", "IR", "KZ", "BY", "XK",
	} {
		if CountryZoneName(country) == "" {
			t.Errorf("%s has no zone", country)
		}
	}
	if len(countryCodeZones) < 240 {
		t.Errorf("the table has %d countries", len(countryCodeZones))
	}
	for country, row := range countryCodeZones {
		if _, err := time.LoadLocation(row.zone); err != nil {
			t.Errorf("%s: zone %q does not load: %s", country, row.zone, err)
		}
		if row.openLaterMinutes < 0 || row.closeEarlierMinutes < 0 || 11*60 <= row.openLaterMinutes+row.closeEarlierMinutes {
			t.Errorf("%s: narrowing %d+%d minutes", country, row.openLaterMinutes, row.closeEarlierMinutes)
		}
	}
	if CountryZoneName("zz") != "" || CountryZoneName("") != "" {
		t.Error("an unknown code has a zone")
	}
	if CountryZoneName(" jp ") != "Asia/Tokyo" {
		t.Errorf("JP in any case and spacing: %q", CountryZoneName(" jp "))
	}
}

// A country with one clock, or one clock that serves 90% of its people, is
// that clock's most populous zone; the US, Canada, Mexico, Russia, Australia
// and Indonesia keep their most populous zone and narrow the window.
func TestCountryZoneTableAnchors(t *testing.T) {
	direct := map[string]string{
		"JP": "Asia/Tokyo",
		"KR": "Asia/Seoul",
		"DE": "Europe/Berlin",
		// Copenhagen as Danish cities name it, though zone1970.tab files it
		// under Berlin
		"DK": "Europe/Copenhagen",
		// all seven of Kazakhstan's zones are UTC+5
		"KZ": "Asia/Almaty",
		// UTC-3 serves 94% of Brazil
		"BR": "America/Sao_Paulo",
		"CN": "Asia/Shanghai",
		"IN": "Asia/Kolkata",
		"GB": "Europe/London",
	}
	for country, zone := range direct {
		row := countryCodeZones[country]
		if row.zone != zone || row.openLaterMinutes != 0 || row.closeEarlierMinutes != 0 {
			t.Errorf("%s: %+v, want %s alone", country, row, zone)
		}
	}
	for _, country := range []string{"US", "CA", "MX", "RU", "AU", "ID"} {
		row := countryCodeZones[country]
		if row.openLaterMinutes+row.closeEarlierMinutes == 0 {
			t.Errorf("%s is not narrowed: %+v", country, row)
		}
	}
	if row := countryCodeZones["US"]; row.zone != "America/New_York" || row.openLaterMinutes != 6*60 || row.closeEarlierMinutes != 0 {
		t.Errorf("US: %+v, want New York opening six hours later (Honolulu)", row)
	}
}

// The window opens later and closes earlier by the zone's narrowing; one the
// narrowing would empty stays whole.
func TestSendZoneWindow(t *testing.T) {
	w := SendWindow{StartHour: 9, EndHour: 20}
	zone := SendZone{OpenLater: 6 * time.Hour, CloseEarlier: 90 * time.Minute}
	if got := zone.Window(w); got != (SendWindow{StartHour: 15, EndHour: 18, EndMinute: 30}) {
		t.Errorf("narrowed window %+v", got)
	}
	if got := (SendZone{}).Window(w); got != w {
		t.Errorf("the reader's own zone narrowed the window to %+v", got)
	}
	if got := (SendZone{OpenLater: 7 * time.Hour, CloseEarlier: 5 * time.Hour}).Window(w); got != w {
		t.Errorf("an emptied window became %+v", got)
	}
	if hour, minute := (SendWindow{StartHour: 8, EndHour: 21}).Middle(); hour != 14 || minute != 30 {
		t.Errorf("middle %02d:%02d", hour, minute)
	}
	if (SendZone{}).Loc() != time.UTC || (SendZone{}).DisplayLocation() != time.UTC {
		t.Error("the zero zone is not UTC")
	}
}

// A US reader known only by country is mailed inside 09:00-20:00 in every
// populated US zone, whatever the time of day the step comes due and whatever
// the season: the zone is New York's with the window narrowed to the hours
// when Honolulu has opened and New York has not closed. The 10:00 UTC fallback
// it replaces lands at 00:00-05:00 across the country.
func TestCountrySendZoneKeepsTheUnitedStatesInDaytime(t *testing.T) {
	zone, ok := CountrySendZone("US")
	if !ok {
		t.Fatal("no US zone")
	}
	window := zone.Window(SendWindow{StartHour: 9, EndHour: 20})
	zones := []string{"America/New_York", "America/Chicago", "America/Denver", "America/Phoenix", "America/Los_Angeles", "America/Anchorage", "Pacific/Honolulu"}
	locations := []*time.Location{}
	for _, name := range zones {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Fatal(err)
		}
		locations = append(locations, loc)
	}
	inDaytime := func(at time.Time) bool {
		for _, loc := range locations {
			local := at.In(loc)
			minutes := local.Hour()*60 + local.Minute()
			if minutes < 9*60 || 20*60 < minutes {
				return false
			}
		}
		return true
	}
	for at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC); at.Before(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)); at = at.Add(47 * time.Minute) {
		for _, placed := range []time.Time{
			SendAt(at, 3, zone.Loc(), window),
			DueSendAt(at, zone.Loc(), window),
		} {
			if !inDaytime(placed) {
				t.Fatalf("a step due %s goes at %s: %s in Honolulu, %s in New York", at, placed, placed.In(locations[6]), placed.In(locations[0]))
			}
		}
	}

	// the UTC placement the country zone replaces
	utc := SendAt(time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC), 0, time.UTC, SendWindow{StartHour: 9, EndHour: 20})
	if inDaytime(utc) {
		t.Fatal("10:00 UTC is daytime across the US")
	}
}

// An activity frame reads the moment as the chosen local time, so a send
// placed at that local time goes at the moment's time of day.
func TestActivitySendZone(t *testing.T) {
	signUp := time.Date(2026, 11, 3, 1, 0, 0, 0, time.UTC)
	zone := ActivitySendZone(signUp, 14, 30)
	if local := signUp.In(zone.Loc()); local.Hour() != 14 || local.Minute() != 30 {
		t.Fatalf("the sign-up reads %s", local)
	}
	if !zone.Frame || zone.DisplayLocation() != time.UTC {
		t.Error("a frame's dates are not written in UTC")
	}
	// an onboarding step three days on keeps the sign-up's time of day, with
	// no clamp (the 08:00-21:00 window in UTC moved it to 08:00, 03:00 in New
	// York)
	at := SendAt(signUp, 3, zone.Loc(), SendWindow{StartHour: 8, EndHour: 21})
	if want := signUp.AddDate(0, 0, 3); !at.Equal(want) {
		t.Errorf("E1 at %s, want %s", at, want)
	}
	for _, c := range []struct {
		at     time.Time
		hour   int
		offset time.Duration
	}{
		{time.Date(2026, 1, 1, 23, 0, 0, 0, time.UTC), 10, -13*time.Hour + 24*time.Hour},
		{time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC), 20, 18*time.Hour - 24*time.Hour},
	} {
		_, offset := c.at.In(ActivitySendZone(c.at, c.hour, 0).Loc()).Zone()
		if time.Duration(offset)*time.Second != c.offset {
			t.Errorf("%s read as %02d:00: offset %s, want %s", c.at, c.hour, time.Duration(offset)*time.Second, c.offset)
		}
	}
}
