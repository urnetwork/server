// SPDX-License-Identifier: MPL-2.0

package onboarding

// The proposals of mmm/ONBOARDING-RETENTION.md section 8 that the owner
// approved on 2026-11-10 and that live in the campaign engine: P7 (a late
// in-app offer's last-chance email waits for its last day) and P18 (a desktop
// habit email where E2's widget email has nothing to point at).

import (
	"testing"
	"time"
)

// P7: E3a went only when the in-app offer ended within 36 hours, so an offer
// the reader saw after sign-up, with days left on day 4, got no last-chance
// email at all (10 of 403 E3a sends in 60 days on Main had days left, and
// stopped). E3a now waits for the offer's last day.
func TestE3aWaitsForALateOffersLastDay(t *testing.T) {
	createdAt := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	// the offer screen seen on day 2, the offer valid 5 days
	expiresAt := createdAt.Add(7 * 24 * time.Hour)
	day4 := SendAt(createdAt, DefaultSchedule.E3aDay, time.UTC, DefaultSendWindow)
	if OfferEndsSoon(expiresAt, day4) {
		t.Fatalf("the fixture's offer ends within %s of day 4", LastChanceWindow)
	}
	lastChance := LastChanceAt(expiresAt, time.UTC, DefaultSendWindow)
	if want := time.Date(2026, 11, 16, 15, 0, 0, 0, time.UTC); !lastChance.Equal(want) {
		t.Fatalf("last chance at %s, want %s", lastChance, want)
	}

	f := healthy(PathA)
	f.OfferState = OfferStateActive
	f.LastChanceAt = &lastChance
	d := Decide(StepE3, f)
	if d.Action != ActionDefer || d.NextStep != StepE3 || !d.At.Equal(lastChance) {
		t.Fatalf("e3a on day 4 with the offer's last day ahead: %+v, want a deferral to %s", d, lastChance)
	}
	if d.Template != TemplateE3LastChance || d.Opening != OpeningJoined {
		t.Errorf("the deferral names the email it waits for: %+v", d)
	}

	// at the last-chance moment the offer ends within the window, and E3a goes
	f.OfferEndsSoon = OfferEndsSoon(expiresAt, lastChance)
	f.LastChanceAt = nil
	d = Decide(StepE3, f)
	if d.Action != ActionSend || d.Template != TemplateE3LastChance || d.Opening != OpeningJoined || d.NextStep != StepE4 {
		t.Fatalf("e3a on the offer's last day: %+v", d)
	}

	// no deferral where E3a could not go anyway
	f = healthy(PathA)
	f.LastChanceAt = &lastChance
	for _, state := range []string{OfferStateNone, OfferStateRedeemed, OfferStateExpired} {
		f.OfferState = state
		if d := Decide(StepE3, f); d.Action != ActionSkip || d.NextStep != StepE4 {
			t.Errorf("e3a with offer %q: %+v", state, d)
		}
	}
	f.OfferState = OfferStateActive
	f.NoOfferEmail = true
	if d := Decide(StepE3, f); d.Action != ActionSkip {
		t.Errorf("e3a where the offer may not be emailed: %+v", d)
	}
	// path B's E3 is the feedback email, whatever the offer
	f = healthy(PathB)
	f.OfferState = OfferStateActive
	f.LastChanceAt = &lastChance
	if d := Decide(StepE3, f); d.Action != ActionSend || d.Template != TemplateE4Feedback {
		t.Errorf("e3b: %+v", d)
	}
}

// LastChanceAt sends on the reader's local day before the offer's local
// expiry day, inside the send window, and within LastChanceWindow of the
// expiry, so the email's "end tomorrow" is true where the reader is and
// OfferEndsSoon holds when the deferred step runs. Every quarter hour of two
// days, in zones on both sides of UTC, across the 2026 DST changes.
func TestLastChanceAtNamesTheReadersTomorrow(t *testing.T) {
	zones := []string{"UTC", "America/Los_Angeles", "America/New_York", "Europe/Berlin", "Asia/Tokyo", "Asia/Kolkata", "Australia/Sydney", "Pacific/Auckland", "America/Sao_Paulo"}
	starts := []time.Time{
		time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC),
		// the US DST end (11-01), Europe's (10-25), Australia's and New
		// Zealand's DST start (10-04, 09-27)
		time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC),
	}
	w := DefaultSendWindow
	for _, zone := range zones {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatal(err)
		}
		for _, start := range starts {
			for minutes := 0; minutes < 2*24*60; minutes += 15 {
				expiresAt := start.Add(time.Duration(minutes) * time.Minute)
				at := LastChanceAt(expiresAt, loc, w)
				if !OfferEndsSoon(expiresAt, at) {
					t.Fatalf("%s: an offer expiring %s gets its last chance at %s, %s before it", zone, expiresAt.In(loc), at.In(loc), expiresAt.Sub(at))
				}
				ey, em, ed := expiresAt.In(loc).Date()
				ay, am, ad := at.In(loc).Date()
				if tomorrow := time.Date(ay, am, ad+1, 0, 0, 0, 0, time.UTC); tomorrow != time.Date(ey, em, ed, 0, 0, 0, 0, time.UTC) {
					t.Fatalf("%s: an offer expiring %s gets its last chance at %s, not the day before", zone, expiresAt.In(loc), at.In(loc))
				}
				local := at.In(loc)
				if m := local.Hour()*60 + local.Minute(); m < w.startMinutes() || w.endMinutes() < m {
					t.Fatalf("%s: last chance at %s is outside the send window", zone, local)
				}
			}
		}
	}
	// the unknown zone is UTC
	expiresAt := time.Date(2026, 11, 17, 23, 30, 0, 0, time.UTC)
	if got, want := LastChanceAt(expiresAt, nil, w), time.Date(2026, 11, 16, 21, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("an expiry after the window's end: %s, want %s", got, want)
	}
	expiresAt = time.Date(2026, 11, 17, 5, 0, 0, 0, time.UTC)
	if got, want := LastChanceAt(expiresAt, nil, w), time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("an expiry before the window's start: %s, want %s", got, want)
	}
}

// P7: the step after a send never goes within MinSendGap of it. The
// schedule's days are a day apart, but an E3a that waited for a late offer's
// last day can run on or after E4a's day 6, and E4a, already due, went a
// minute later.
func TestNextStepWaitsAfterASend(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	w := DefaultSendWindow
	createdAt := time.Date(2026, 11, 10, 15, 0, 0, 0, ny)
	// E3a waited for an offer expiring on day 7 and went on day 6
	sentAt := LastChanceAt(createdAt.Add(7*24*time.Hour), ny, w)
	e4a := NextSendAt(createdAt, DefaultSchedule.E4aDay, &sentAt, sentAt, ny, w)
	if e4a.Before(sentAt.Add(MinSendGap)) {
		t.Fatalf("e4a at %s, %s after the e3a send", e4a.In(ny), e4a.Sub(sentAt))
	}
	if want := DueSendAt(sentAt.Add(MinSendGap), ny, w); !e4a.Equal(want) {
		t.Errorf("e4a at %s, want %s", e4a.In(ny), want.In(ny))
	}
	// without a send (a skip, a dry run) the due step goes at the next
	// window edge, as before
	if got, want := NextSendAt(createdAt, DefaultSchedule.E4aDay, nil, sentAt, ny, w), DueSendAt(sentAt, ny, w); !got.Equal(want) {
		t.Errorf("a due step after a skip: %s, want %s", got.In(ny), want.In(ny))
	}
	// a gap that ends at night waits for the morning
	late := time.Date(2026, 11, 16, 3, 0, 0, 0, ny)
	if got, want := NextSendAt(createdAt, 0, &late, late, ny, w), time.Date(2026, 11, 16, 23, 0, 0, 0, ny).Add(9*time.Hour); !got.Equal(want) {
		t.Errorf("after a send at 03:00: %s, want %s", got.In(ny), want.In(ny))
	}

	// the schedule itself is unchanged: on either path, each step's send
	// places the next one on its own day
	for _, path := range []string{PathA, PathB} {
		for step := StepE1; step != ""; step = NextStep(step, path) {
			next := NextStep(step, path)
			if next == "" {
				break
			}
			sent := SendAt(createdAt, DefaultSchedule.Day(step, path), ny, w)
			if got, want := NextSendAt(createdAt, DefaultSchedule.Day(next, path), &sent, sent, ny, w), SendAt(createdAt, DefaultSchedule.Day(next, path), ny, w); !got.Equal(want) {
				t.Errorf("path %s, %s after %s: %s, want %s", path, next, step, got.In(ny), want.In(ny))
			}
		}
	}
}

// P18: E2's widget email skips windows, linux and web, which have no widget
// (2026-11-10, e38589c28), so a connected desktop reader got nothing on day 3.
// It gets the desktop habit email now: the app started with the computer, or
// on the web the linked extension pinned to the toolbar. Only once the
// release has uploaded its template: until then the step skips as before.
func TestE2DesktopEmail(t *testing.T) {
	for _, c := range []struct {
		platform    string
		widgetAdded bool
		template    bool
		want        string
	}{
		{"windows", false, true, VariantDesktop},
		{"linux", false, true, VariantDesktop},
		{"web", true, true, VariantDesktop},
		// the web reader connects through the extension; without it linked
		// the pin line would name something they lack
		{"web", false, true, ""},
		// not uploaded yet
		{"windows", false, false, ""},
		{"linux", false, false, ""},
		{"web", true, false, ""},
		// the phones keep the widget email, and skip once a widget exists
		{"ios", false, true, VariantActivated},
		{"android", false, true, VariantActivated},
		{"ios", true, true, ""},
		{"android", true, true, ""},
		// a Mac has no home or lock screen: the desktop email (the
		// 2026-11-13 review); an unknown platform gets neither
		{"macos", false, true, VariantDesktop},
		{"macos", true, true, VariantDesktop},
		{"", false, true, ""},
	} {
		f := healthy(PathB)
		f.Platform = c.platform
		// the reader's app has the widget wherever there is one
		f.WidgetBuild = true
		f.HasConnected = true
		f.WidgetAdded = c.widgetAdded
		f.DesktopTemplate = c.template
		d := Decide(StepE2, f)
		if c.want == "" {
			if d.Action != ActionSkip || d.NextStep != StepE3 {
				t.Errorf("e2 on %q (widget %t, desktop template %t): %+v, want a skip", c.platform, c.widgetAdded, c.template, d)
			}
		} else if d.Action != ActionSend || d.Template != TemplateE2Widget || d.Variant != c.want || d.NextStep != StepE3 {
			t.Errorf("e2 on %q (widget %t, desktop template %t): %+v, want %s", c.platform, c.widgetAdded, c.template, d, c.want)
		}
		// a reader who never connected gets the connect nudge, everywhere
		f.HasConnected = false
		if d := Decide(StepE2, f); d.Action != ActionSend || d.Variant != VariantNotActivated {
			t.Errorf("e2 not activated on %q: %+v", c.platform, d)
		}
	}
}

// The desktop email's params: the activated line's counts, and which branch
// (the browser's extension or the app's start with the computer).
func TestE2DesktopParams(t *testing.T) {
	in := ParamsInput{
		Template: TemplateE2Widget, Variant: VariantDesktop, Locale: "en",
		DaysActive: 3, ConnectDays: 2, ProvidersOnline: 4812, CountriesOnline: 91,
		Location: time.UTC,
		Token:    "T", SiteUrl: "https://ur.io/", CompanyLine: "co",
	}
	for _, browser := range []bool{true, false} {
		in.Browser = browser
		p := BuildParams(in)
		want := map[string]string{
			"cta_url": "https://ur.io/o/widgets?t=T", "connect_days": "2", "days_active": "3",
			"providers_online": "4,812", "countries_online": "91", "company_line": "co", "browser": "",
		}
		if browser {
			want["browser"] = "1"
		}
		if len(p) != len(want) {
			t.Errorf("browser %t: params %v, want %v", browser, p, want)
		}
		for k, v := range want {
			if p[k] != v {
				t.Errorf("browser %t: %s = %q, want %q", browser, k, p[k], v)
			}
		}
	}
	// the line quotes both counts: with either unknown it is left out
	in.CountriesOnline = 0
	if p := BuildParams(in); p["providers_online"] != "" || p["countries_online"] != "" {
		t.Errorf("unknown country count: %v", p)
	}
	// a first day still reads "2 of your first 2 days" at most
	in.DaysActive, in.ConnectDays = 0, 5
	if p := BuildParams(in); p["connect_days"] != "2" || p["days_active"] != "2" {
		t.Errorf("first days: %v", p)
	}
}
