// SPDX-License-Identifier: MPL-2.0

package onboarding

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func healthy(path string) Facts {
	return Facts{
		Path:           path,
		HasEmail:       true,
		ProductUpdates: true,
		NetworkExists:  true,
	}
}

func TestGlobalExitsInOrder(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(f *Facts)
		want   string
	}{
		{"no email", func(f *Facts) { f.HasEmail = false }, ExitNoEmail},
		{"deleted", func(f *Facts) { f.NetworkExists = false }, ExitNetworkDeleted},
		{"holdout", func(f *Facts) { f.Holdout = true }, ExitHoldout},
		{"opt out", func(f *Facts) { f.ProductUpdates = false }, ExitOptOut},
		{"suppressed", func(f *Facts) { f.Suppressed = true }, ExitSuppressed},
		{"country policy", func(f *Facts) { f.PolicyExit = ExitCountryNoMarketing }, ExitCountryNoMarketing},
		{"consent evidence", func(f *Facts) { f.PolicyExit = ExitConsentEvidence }, ExitConsentEvidence},
		{"pro", func(f *Facts) { f.Pro = true }, ExitPro},
		{"complained", func(f *Facts) { f.Complained = true }, ExitComplained},
		{"bounced", func(f *Facts) { f.Bounced = true }, ExitBounced},
	}
	for _, c := range cases {
		f := healthy(PathA)
		c.mutate(&f)
		for _, step := range []string{StepE1, StepE2, StepE3, StepE4, StepE5} {
			d := Decide(step, f)
			if d.Action != ActionExit || d.ExitReason != c.want {
				t.Errorf("%s at %s: got %+v, want exit %s", c.name, step, d, c.want)
			}
		}
	}
	if GlobalExit(healthy(PathB)) != "" {
		t.Error("a healthy network must not exit")
	}
}

func TestE1(t *testing.T) {
	d := Decide(StepE1, healthy(PathB))
	if d.Action != ActionSend || d.Template != TemplateE1Connect || d.Variant != VariantDefault || d.NextStep != StepE2 {
		t.Errorf("e1 fresh: %+v", d)
	}
	f := healthy(PathB)
	f.HasConnected = true
	d = Decide(StepE1, f)
	if d.Action != ActionSkip || d.NextStep != StepE2 {
		t.Errorf("e1 connected: %+v", d)
	}
}

func TestE2(t *testing.T) {
	f := healthy(PathA)
	d := Decide(StepE2, f)
	if d.Action != ActionSend || d.Template != TemplateE2Widget || d.Variant != VariantNotActivated || d.NextStep != StepE3 {
		t.Errorf("e2 not activated: %+v", d)
	}
	f.HasConnected = true
	d = Decide(StepE2, f)
	if d.Action != ActionSend || d.Variant != VariantActivated {
		t.Errorf("e2 activated: %+v", d)
	}
	f.WidgetAdded = true
	d = Decide(StepE2, f)
	if d.Action != ActionSkip {
		t.Errorf("e2 widget exists: %+v", d)
	}
}

func TestPathA(t *testing.T) {
	// E3a: the in-app offer's last day
	f := healthy(PathA)
	f.OfferState = OfferStateActive
	d := Decide(StepE3, f)
	if d.Action != ActionSend || d.Template != TemplateE3LastChance || d.Opening != OpeningJoined || d.NextStep != StepE4 {
		t.Errorf("e3a active: %+v", d)
	}
	for _, state := range []string{OfferStateRedeemed, OfferStateExpired, OfferStateNone} {
		f.OfferState = state
		if d := Decide(StepE3, f); d.Action != ActionSkip {
			t.Errorf("e3a %q: %+v", state, d)
		}
	}
	// E4a: feedback, engaged only by a connection. Opens never count: Apple
	// Mail Privacy Protection opens every message, and a network that never
	// connected was told "You've connected on 0 of your first 6 days".
	f = healthy(PathA)
	d = Decide(StepE4, f)
	if d.Action != ActionSend || d.Template != TemplateE4Feedback || d.Variant != VariantNotActivated || d.NextStep != "" {
		t.Errorf("e4a not activated: %+v", d)
	}
	f.EmailOpens = 5
	if d := Decide(StepE4, f); d.Variant != VariantNotActivated {
		t.Errorf("e4a opens without a connection must stay not activated: %+v", d)
	}
	f.HasConnected = true
	if d := Decide(StepE4, f); d.Variant != VariantEngaged {
		t.Errorf("e4a connected: %+v", d)
	}
	f.FeedbackSubmitted = true
	if d := Decide(StepE4, f); d.Action != ActionSkip {
		t.Errorf("e4a feedback exists: %+v", d)
	}
	// no E5 on path A
	if d := Decide(StepE5, healthy(PathA)); d.Action != ActionExit || d.ExitReason != ExitDone {
		t.Errorf("e5 on A: %+v", d)
	}
}

func TestPathB(t *testing.T) {
	// E3b: feedback
	f := healthy(PathB)
	f.HasConnected = true
	d := Decide(StepE3, f)
	if d.Action != ActionSend || d.Template != TemplateE4Feedback || d.Variant != VariantEngaged || d.NextStep != StepE4 {
		t.Errorf("e3b engaged: %+v", d)
	}
	// E4b: issue by email when no offer exists
	f = healthy(PathB)
	d = Decide(StepE4, f)
	if d.Action != ActionSend || d.Template != TemplateE4bOffer || !d.IssueOffer || d.NextStep != StepE5 {
		t.Errorf("e4b: %+v", d)
	}
	for _, state := range []string{OfferStateActive, OfferStateRedeemed, OfferStateExpired} {
		f.OfferState = state
		if d := Decide(StepE4, f); d.Action != ActionSkip || d.IssueOffer {
			t.Errorf("e4b with offer %q: %+v", state, d)
		}
	}
	// E5b: the emailed offer's last day
	f = healthy(PathB)
	f.OfferState = OfferStateActive
	d = Decide(StepE5, f)
	if d.Action != ActionSend || d.Template != TemplateE3LastChance || d.Opening != OpeningLastNote || d.NextStep != "" {
		t.Errorf("e5b: %+v", d)
	}
	f.OfferState = OfferStateRedeemed
	if d := Decide(StepE5, f); d.Action != ActionSkip {
		t.Errorf("e5b redeemed: %+v", d)
	}
	// a complaint on E4b is a global exit before E5b runs at all
	f.OfferState = OfferStateActive
	f.Complained = true
	f.ComplainedStep = StepE4
	if d := Decide(StepE5, f); d.Action != ActionExit || d.ExitReason != ExitComplained {
		t.Errorf("e5b after complaint: %+v", d)
	}
	// an empty path is path B
	f = healthy("")
	if d := Decide(StepE4, f); !d.IssueOffer {
		t.Errorf("empty path must behave as B: %+v", d)
	}
}

// The offer steps stand down when the offer may not be emailed (a no-offer
// country) or can't be redeemed from the email (the App Store build without the
// offer route): no E3a, no E4b (so no offer is issued by email), no E5b. The
// rest of the sequence is untouched.
func TestOfferStepsStandDownWithoutAnEmailOffer(t *testing.T) {
	f := healthy(PathA)
	f.OfferState = OfferStateActive
	f.NoOfferEmail = true
	if d := Decide(StepE3, f); d.Action != ActionSkip || d.NextStep != StepE4 {
		t.Errorf("e3a without an email offer: %+v", d)
	}
	f = healthy(PathB)
	f.NoOfferEmail = true
	if d := Decide(StepE4, f); d.Action != ActionSkip || d.IssueOffer || d.NextStep != StepE5 {
		t.Errorf("e4b without an email offer: %+v", d)
	}
	f.OfferState = OfferStateActive
	if d := Decide(StepE5, f); d.Action != ActionSkip {
		t.Errorf("e5b without an email offer: %+v", d)
	}
	if d := Decide(StepE1, f); d.Action != ActionSend {
		t.Errorf("e1 must still go: %+v", d)
	}
	f.HasConnected = true
	if d := Decide(StepE3, f); d.Action != ActionSend || d.Template != TemplateE4Feedback {
		t.Errorf("e3b feedback must still go: %+v", d)
	}
}

func TestScheduleDaysAndNextStep(t *testing.T) {
	s := DefaultSchedule
	want := map[string][2]int{StepE1: {1, 1}, StepE2: {3, 3}, StepE3: {4, 5}, StepE4: {6, 7}, StepE5: {-1, 11}}
	for step, days := range want {
		if got := s.Day(step, PathA); got != days[0] {
			t.Errorf("%s A day = %d, want %d", step, got, days[0])
		}
		if got := s.Day(step, PathB); got != days[1] {
			t.Errorf("%s B day = %d, want %d", step, got, days[1])
		}
	}
	if NextStep("", PathA) != StepE1 || NextStep(StepE4, PathA) != "" || NextStep(StepE4, PathB) != StepE5 || NextStep(StepE5, PathB) != "" {
		t.Error("next step chain")
	}
}

func TestSendAt(t *testing.T) {
	la, _ := time.LoadLocation("America/Los_Angeles")
	w := DefaultSendWindow
	// sign-up 14:30 local -> the same wall clock a day later
	created := time.Date(2026, 3, 6, 14, 30, 0, 0, la)
	at := SendAt(created, 1, la, w)
	if got := at.In(la); got.Hour() != 14 || got.Minute() != 30 || got.Day() != 7 {
		t.Errorf("same wall clock: %s", got)
	}
	// DST starts 2026-03-08 in LA: day 3 keeps the wall-clock hour
	at = SendAt(created, 3, la, w)
	if got := at.In(la); got.Hour() != 14 || got.Minute() != 30 || got.Day() != 9 {
		t.Errorf("across DST: %s", got)
	}
	if at.Sub(SendAt(created, 1, la, w)) != 2*24*time.Hour-time.Hour {
		t.Errorf("DST day is 23h in UTC: %s", at.Sub(SendAt(created, 1, la, w)))
	}
	// 02:15 local -> 08:00
	created = time.Date(2026, 9, 9, 2, 15, 0, 0, la)
	if got := SendAt(created, 1, la, w).In(la); got.Hour() != 8 || got.Minute() != 0 || got.Day() != 10 {
		t.Errorf("early clamp: %s", got)
	}
	// 23:40 local -> 21:00
	created = time.Date(2026, 9, 9, 23, 40, 0, 0, la)
	if got := SendAt(created, 1, la, w).In(la); got.Hour() != 21 || got.Minute() != 0 || got.Day() != 10 {
		t.Errorf("late clamp: %s", got)
	}
	// exactly 21:00 stays
	created = time.Date(2026, 9, 9, 21, 0, 0, 0, la)
	if got := SendAt(created, 1, la, w).In(la); got.Hour() != 21 {
		t.Errorf("edge: %s", got)
	}
	// nil location = UTC
	created = time.Date(2026, 9, 9, 5, 0, 0, 0, time.UTC)
	if got := SendAt(created, 2, nil, w); got.Hour() != 8 || got.Day() != 11 {
		t.Errorf("utc fallback: %s", got)
	}
	if LoadLocation("Not/AZone") != time.UTC || LoadLocation("") != time.UTC || LoadLocation("Europe/Berlin").String() != "Europe/Berlin" {
		t.Error("LoadLocation fallbacks")
	}
}

func TestSendWindowNextOpen(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	w := DefaultSendWindow
	for _, c := range []struct {
		name     string
		at, want time.Time
	}{
		{"before the start", time.Date(2026, 11, 4, 6, 15, 0, 0, ny), time.Date(2026, 11, 4, 8, 0, 0, 0, ny)},
		{"the start", time.Date(2026, 11, 4, 8, 0, 0, 0, ny), time.Date(2026, 11, 4, 8, 0, 0, 0, ny)},
		{"inside", time.Date(2026, 11, 4, 14, 5, 9, 0, ny), time.Date(2026, 11, 4, 14, 5, 9, 0, ny)},
		{"the end", time.Date(2026, 11, 4, 21, 0, 59, 0, ny), time.Date(2026, 11, 4, 21, 0, 59, 0, ny)},
		{"past the end", time.Date(2026, 11, 4, 21, 1, 0, 0, ny), time.Date(2026, 11, 5, 8, 0, 0, 0, ny)},
		{"before midnight", time.Date(2026, 11, 4, 23, 59, 0, 0, ny), time.Date(2026, 11, 5, 8, 0, 0, 0, ny)},
		{"after midnight", time.Date(2026, 11, 5, 0, 30, 0, 0, ny), time.Date(2026, 11, 5, 8, 0, 0, 0, ny)},
		// DST ends 2026-11-01 02:00 in New York: still 08:00 wall clock
		{"across DST", time.Date(2026, 10, 31, 22, 0, 0, 0, ny), time.Date(2026, 11, 1, 8, 0, 0, 0, ny)},
	} {
		if got := w.NextOpen(c.at, ny); !got.Equal(c.want) {
			t.Errorf("%s: %s, want %s", c.name, got.In(ny), c.want)
		}
	}
	custom := SendWindow{StartHour: 9, StartMinute: 30, EndHour: 20}
	if got, want := custom.NextOpen(time.Date(2026, 11, 4, 20, 5, 0, 0, ny), ny), time.Date(2026, 11, 5, 9, 30, 0, 0, ny); !got.Equal(want) {
		t.Errorf("custom window: %s, want %s", got.In(ny), want)
	}
	if got, want := w.NextOpen(time.Date(2026, 11, 4, 22, 0, 0, 0, time.UTC), nil), time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("nil location = UTC: %s, want %s", got, want)
	}
}

// A step that is already due goes out inside the window: right away while it
// is open, else at the next window start, never tonight past the window's end
// (SendAt's same-day clamp is already in the past there).
func TestDueSendAt(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	w := DefaultSendWindow
	for _, c := range []struct {
		name      string
		now, want time.Time
	}{
		{"past the end", time.Date(2026, 11, 4, 21, 30, 20, 0, ny), time.Date(2026, 11, 5, 8, 0, 0, 0, ny)},
		{"a minute on passes the end", time.Date(2026, 11, 4, 21, 0, 30, 0, ny), time.Date(2026, 11, 5, 8, 0, 0, 0, ny)},
		{"open", time.Date(2026, 11, 4, 14, 5, 20, 0, ny), time.Date(2026, 11, 4, 14, 6, 20, 0, ny)},
		{"open, on the minute", time.Date(2026, 11, 4, 14, 5, 0, 0, ny), time.Date(2026, 11, 4, 14, 5, 0, 0, ny)},
		{"before the start", time.Date(2026, 11, 4, 6, 15, 0, 0, ny), time.Date(2026, 11, 4, 8, 0, 0, 0, ny)},
		// DST ends 2026-11-01 in New York: the next morning is 08:00 EST
		{"past the end before DST ends", time.Date(2026, 10, 31, 22, 0, 0, 0, ny), time.Date(2026, 11, 1, 8, 0, 0, 0, ny)},
	} {
		got := DueSendAt(c.now, ny, w)
		if !got.Equal(c.want) {
			t.Errorf("%s: %s, want %s", c.name, got.In(ny), c.want)
		}
		if got.Before(c.now) {
			t.Errorf("%s: %s is in the past", c.name, got.In(ny))
		}
	}
	if got, want := DueSendAt(time.Date(2026, 11, 4, 22, 0, 0, 0, time.UTC), nil, w), time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("nil location = UTC: %s, want %s", got, want)
	}
}

func TestParseSendWindow(t *testing.T) {
	w, err := ParseSendWindow("")
	if err != nil || w != DefaultSendWindow {
		t.Errorf("default: %+v %v", w, err)
	}
	w, err = ParseSendWindow("09:30-20:00")
	if err != nil || w.StartHour != 9 || w.StartMinute != 30 || w.EndHour != 20 {
		t.Errorf("parse: %+v %v", w, err)
	}
	for _, bad := range []string{"9-20", "21:00-08:00", "08:00", "08:60-21:00"} {
		if _, err := ParseSendWindow(bad); err == nil {
			t.Errorf("%q must fail", bad)
		}
	}
}

func TestWebhookMapping(t *testing.T) {
	cases := map[string]string{
		"delivered": "email.delivered", "opened": "email.opened", "unique_opened": "email.opened",
		"click": "email.clicked", "hard_bounce": "email.bounced", "blocked": "email.bounced",
		"spam": "email.complained", "unsubscribed": "email.unsubscribed", "request": "", "soft_bounce": "", "deferred": "",
	}
	for event, want := range cases {
		if got := EventForWebhook(event); got != want {
			t.Errorf("%s -> %q, want %q", event, got, want)
		}
	}
	if WebhookExit("email.bounced") != ExitBounced || WebhookExit("email.complained") != ExitComplained || WebhookExit("email.unsubscribed") != ExitOptOut || WebhookExit("email.opened") != "" {
		t.Error("webhook exits")
	}
}

func TestFormatting(t *testing.T) {
	if FormatUsd(29.99) != "$29.99" || FormatUsd(4) != "$4" || FormatUsd(39.999) != "$40" || FormatUsd(0.5) != "$0.50" {
		t.Errorf("usd: %s %s %s %s", FormatUsd(29.99), FormatUsd(4), FormatUsd(39.999), FormatUsd(0.5))
	}
	if FormatCount(4812) != "4,812" || FormatCount(999) != "999" || FormatCount(1234567) != "1,234,567" || FormatCount(0) != "0" {
		t.Error("count grouping")
	}
	if FormatGb(30) != "30 GB" || FormatGb(2.5) != "2.5 GB" {
		t.Error("gb")
	}
	la, _ := time.LoadLocation("America/Los_Angeles")
	at := time.Date(2026, 9, 14, 22, 5, 0, 0, time.UTC)
	if got := FormatLocalDateTime(at, la); got != "Sep 14, 2026 at 3:05 PM PDT" {
		t.Errorf("local date time: %s", got)
	}
}

// Every template's params are exactly the manifest's keys.
func TestParamsMatchManifests(t *testing.T) {
	// URNETWORK_MMM_ROOT points at another mmm checkout (a worktree)
	mmm := filepath.Join("..", "..", "mmm")
	if v := os.Getenv("URNETWORK_MMM_ROOT"); v != "" {
		mmm = v
	}
	root := filepath.Join(mmm, "onboarding", "templates")
	if _, err := os.Stat(root); err != nil {
		t.Skip("mmm/onboarding/templates is not checked out next to server")
	}
	expires := time.Date(2026, 9, 14, 22, 5, 0, 0, time.UTC)
	base := ParamsInput{
		// an iPhone: the App Store bills it (the direct branch is TestOfferParamsStoreBilled's)
		Platform: "iPhone", StoreBilled: true, DailyDataGb: 30, DaysActive: 3, ConnectDays: 2, ProvidersOnline: 4812, CountriesOnline: 91,
		OfferExpiresAt: &expires, OfferPriceUsd: 29.99, RegularYearUsd: 39.99, TrialDays: 14,
		Location: time.UTC, Now: expires.Add(-4 * 24 * time.Hour), Token: "tok", SiteUrl: "https://ur.io", CompanyLine: "co",
	}
	for _, tv := range [][2]string{
		{TemplateE1Connect, VariantDefault}, {TemplateE2Widget, VariantActivated}, {TemplateE2Widget, VariantNotActivated},
		{TemplateE3LastChance, VariantDefault}, {TemplateE4Feedback, VariantEngaged}, {TemplateE4Feedback, VariantNotActivated},
		{TemplateE4bOffer, VariantDefault},
	} {
		data, err := os.ReadFile(filepath.Join(root, tv[0], tv[1], "manifest.json"))
		if err != nil {
			t.Fatalf("%s/%s: %s", tv[0], tv[1], err)
		}
		var manifest struct {
			Params map[string]string `json:"params"`
		}
		if err := json.Unmarshal(data, &manifest); err != nil {
			t.Fatal(err)
		}
		in := base
		in.Template, in.Variant, in.Opening = tv[0], tv[1], OpeningJoined
		got := BuildParams(in)
		var gotKeys, wantKeys []string
		for k := range got {
			gotKeys = append(gotKeys, k)
		}
		for k := range manifest.Params {
			wantKeys = append(wantKeys, k)
		}
		slices.Sort(gotKeys)
		slices.Sort(wantKeys)
		if !slices.Equal(gotKeys, wantKeys) {
			t.Errorf("%s/%s params %v, manifest %v", tv[0], tv[1], gotKeys, wantKeys)
		}
		for k, v := range got {
			if v == "" && k != "offer_expires_at" {
				t.Errorf("%s/%s %s is empty", tv[0], tv[1], k)
			}
		}
	}
	// the urls
	p := BuildParams(ParamsInput{Template: TemplateE4Feedback, Variant: VariantEngaged, Token: "T", SiteUrl: "https://ur.io/"})
	if p["rating_url_5"] != "https://ur.io/f/T?r=5" {
		t.Errorf("rating url: %s", p["rating_url_5"])
	}
	p = BuildParams(ParamsInput{Template: TemplateE4Feedback, Variant: VariantNotActivated, Token: "T", SiteUrl: "https://ur.io"})
	if p["reason_url_2"] != "https://ur.io/f/T?why=not_working" || p["days_active"] != "5" {
		t.Errorf("reason url / floor: %v", p)
	}
	p = BuildParams(ParamsInput{Template: TemplateE1Connect, Token: "T", SiteUrl: "https://ur.io", DailyDataGb: 30})
	if p["cta_url"] != "https://ur.io/o/connect?t=T" || p["daily_data_gb"] != "30 GB" || p["offer_expires_at"] != "" {
		t.Errorf("e1 params: %v", p)
	}
	if StepLandingSlug(TemplateE3LastChance) != "offer" || StepLandingSlug(TemplateE2Widget) != "widgets" {
		t.Error("slugs")
	}
}

// The activated and engaged texts read "You've connected on {connect_days} of
// your first {days_active} days" (support inbox 8146). connect_days counts UTC
// calendar days from every connection record since sign-up, and days_active
// whole days since sign-up, so a network that signed up late in a UTC day and
// connected daily has more connection days than whole days. The text says it
// connected on all of them, never on more days than it has had.
func TestConnectDaysNeverExceedDaysActive(t *testing.T) {
	for _, c := range []struct {
		template    string
		variant     string
		daysActive  int
		connectDays int
		wantConnect string
		wantActive  string
	}{
		{TemplateE2Widget, VariantActivated, 3, 4, "3", "3"},
		{TemplateE2Widget, VariantActivated, 3, 2, "2", "3"},
		// the activated text's floor of 2 days applies first
		{TemplateE2Widget, VariantActivated, 1, 3, "2", "2"},
		{TemplateE4Feedback, VariantEngaged, 6, 7, "6", "6"},
		{TemplateE4Feedback, VariantEngaged, 5, 3, "3", "5"},
		// the params never exceed the facts, even for a combination the
		// decision no longer produces (engaged with no connection)
		{TemplateE4Feedback, VariantEngaged, 5, 0, "0", "5"},
	} {
		p := BuildParams(ParamsInput{
			Template:    c.template,
			Variant:     c.variant,
			DaysActive:  c.daysActive,
			ConnectDays: c.connectDays,
			Token:       "T",
			SiteUrl:     "https://ur.io",
		})
		if p["connect_days"] != c.wantConnect || p["days_active"] != c.wantActive {
			t.Errorf("%s/%s with %d connection days in %d days: connected on %s of %s days, want %s of %s",
				c.template, c.variant, c.connectDays, c.daysActive, p["connect_days"], p["days_active"], c.wantConnect, c.wantActive)
		}
	}
}

// The App Store and Google Play charge their own local price, so the offer
// emails name none for the networks they bill (REVIEW.md finding 7); the
// direct (Stripe) platforms keep the amount.
func TestOfferParamsStoreBilled(t *testing.T) {
	expires := time.Date(2026, 9, 14, 22, 5, 0, 0, time.UTC)
	for _, platform := range []string{"ios", "macos", "android"} {
		if !StoreBilledPlatform(platform) {
			t.Fatalf("%s is store billed", platform)
		}
	}
	for _, platform := range []string{"web", "windows", "linux", ""} {
		if StoreBilledPlatform(platform) {
			t.Fatalf("%s is billed directly", platform)
		}
	}
	for _, template := range []string{TemplateE3LastChance, TemplateE4bOffer} {
		in := ParamsInput{Template: template, Variant: VariantDefault, OfferExpiresAt: &expires, OfferPriceUsd: 30, RegularYearUsd: 40, TrialDays: 14,
			Location: time.UTC, Now: expires.Add(-24 * time.Hour), Token: "tok", SiteUrl: "https://ur.io"}
		if got := BuildParams(in)["store_billed"]; got != "" {
			t.Fatalf("%s: direct billing has store_billed %q", template, got)
		}
		in.StoreBilled = true
		if got := BuildParams(in)["store_billed"]; got != "1" {
			t.Fatalf("%s: store billing has store_billed %q", template, got)
		}
	}
}
