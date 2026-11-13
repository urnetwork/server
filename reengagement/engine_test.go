// SPDX-License-Identifier: MPL-2.0

package reengagement

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server/onboarding"
)

func healthy(stream string) Facts {
	return Facts{Stream: stream, HasEmail: true, NetworkExists: true, ProductUpdates: true}
}

func TestGlobalExits(t *testing.T) {
	cases := []struct {
		mutate func(f *Facts)
		want   string
	}{
		{func(f *Facts) { f.HasEmail = false }, ExitNoEmail},
		{func(f *Facts) { f.NetworkExists = false }, ExitNetworkDeleted},
		{func(f *Facts) { f.ProductUpdates = false }, ExitOptOut},
		{func(f *Facts) { f.Suppressed = true }, ExitSuppressed},
		{func(f *Facts) { f.PolicyExit = onboarding.ExitCountryNoMarketing }, onboarding.ExitCountryNoMarketing},
		{func(f *Facts) { f.Complained = true }, ExitComplained},
		{func(f *Facts) { f.Bounced = true }, ExitBounced},
		{func(f *Facts) { f.Pro = true }, ExitPro},
		{func(f *Facts) { f.OnboardingActive = true }, ExitOnboardingActive},
	}
	for _, c := range cases {
		for _, stream := range []string{StreamLapsed, StreamNeverActivated, StreamFix, StreamWinback} {
			f := healthy(stream)
			c.mutate(&f)
			for _, step := range []string{StepR1, StepR2, StepR3, StepN1, StepF1, StepW1, StepEnd} {
				if d := Decide(step, f, DefaultSchedule); d.Action != ActionExit || d.ExitReason != c.want {
					t.Errorf("%s/%s: got %+v, want exit %s", stream, step, d, c.want)
				}
			}
		}
	}
	f := healthy(StreamLapsed)
	f.SentLastYear = 6
	if d := Decide(StepR2, f, DefaultSchedule); d.ExitReason != ExitCapped {
		t.Errorf("yearly cap: %+v", d)
	}
	if d := Decide(StepEnd, f, DefaultSchedule); d.ExitReason == ExitCapped {
		t.Errorf("the sunset check sends nothing and is never capped: %+v", d)
	}
}

func TestLapsedStream(t *testing.T) {
	s := DefaultSchedule
	f := healthy(StreamLapsed)
	// nothing new for the platform: R1 gives its day to R2
	if d := Decide(StepR1, f, s); d.Action != ActionSkip || d.NextStep != StepR2 || d.NextDay != 0 {
		t.Errorf("r1 without news: %+v", d)
	}
	f.Highlights = []string{HighlightOutages}
	if d := Decide(StepR1, f, s); d.Action != ActionSend || d.Template != TemplateR1News || d.Variant != VariantLapsed || d.NextStep != StepR2 || d.NextDay != 7 {
		t.Errorf("r1 with news: %+v", d)
	}
	if d := Decide(StepR2, f, s); d.Action != ActionSend || d.Template != TemplateR2Reason || d.NextStep != StepR3 || d.NextDay != 30 {
		t.Errorf("r2: %+v", d)
	}
	if d := Decide(StepR3, f, s); d.Action != ActionSend || d.Template != TemplateR3Keep || d.NextStep != StepEnd || d.NextDay != 60 {
		t.Errorf("r3: %+v", d)
	}
	// silence ends in a sunset suppression
	if d := Decide(StepEnd, f, s); d.ExitReason != ExitSunset || d.Suppress != SuppressSunset {
		t.Errorf("end silent: %+v", d)
	}
	kept := f
	kept.Kept = true
	if d := Decide(StepEnd, kept, s); d.ExitReason != ExitKept || d.Suppress != "" {
		t.Errorf("end kept: %+v", d)
	}
	// a click or an answer means a live reader: no keep-or-go, no sunset
	clicked := f
	clicked.Clicked = true
	if d := Decide(StepR3, clicked, s); d.ExitReason != ExitEngaged {
		t.Errorf("r3 clicked: %+v", d)
	}
	answered := f
	answered.Reason = ReasonOther
	if d := Decide(StepR2, answered, s); d.ExitReason != ExitAnswered {
		t.Errorf("r2 answered: %+v", d)
	}
	// a connection ends it at any step, without suppressing
	back := f
	back.Reactivated = true
	for _, step := range []string{StepR1, StepR2, StepR3, StepEnd} {
		if d := Decide(step, back, s); d.ExitReason != ExitReactivated || d.Suppress != "" {
			t.Errorf("%s reactivated: %+v", step, d)
		}
	}
	// a holdout runs the same clock with virtual sends
	hold := f
	hold.Holdout = true
	if d := Decide(StepR2, hold, s); d.Action != ActionVirtual || d.Template != TemplateR2Reason || d.NextStep != StepR3 {
		t.Errorf("holdout: %+v", d)
	}
}

// A holdout network's R3 is a virtual send: nobody asked it "should I keep
// writing?", so its silence at the sunset check is no answer. It exits done,
// unsuppressed; before the 2026-11-13 review it was suppressed at day 60 like
// a mailed network that never answered, and so dropped from all later mail.
func TestHoldoutSunsetExitsDoneWithoutSuppression(t *testing.T) {
	s := DefaultSchedule
	for _, stream := range []string{StreamLapsed, StreamNeverActivated} {
		f := healthy(stream)
		f.Holdout = true
		r3 := Decide(StepR3, f, s)
		if r3.Action != ActionVirtual || r3.Template != TemplateR3Keep || r3.NextStep != StepEnd {
			t.Errorf("%s holdout r3: %+v, want a virtual send", stream, r3)
		}
		if d := Decide(StepEnd, f, s); d.Action != ActionExit || d.ExitReason != ExitDone || d.Suppress != "" {
			t.Errorf("%s holdout end: %+v, want exit done without a suppression", stream, d)
		}
		// the mailed arm's silence still ends in the sunset suppression
		f.Holdout = false
		if d := Decide(StepEnd, f, s); d.ExitReason != ExitSunset || d.Suppress != SuppressSunset {
			t.Errorf("%s mailed end: %+v, want the sunset suppression", stream, d)
		}
	}
}

func TestNeverActivatedStream(t *testing.T) {
	s := DefaultSchedule
	f := healthy(StreamNeverActivated)
	for reason, want := range map[string]string{"not_needed": VariantNotNeeded, "1": VariantNotNeeded, "trust": VariantTrust, "3": VariantTrust} {
		f.OnboardingReason = reason
		if d := Decide(StepN1, f, s); d.Action != ActionSend || d.Template != TemplateN1Nudge || d.Variant != want || d.NextStep != StepR3 || d.NextDay != 30 {
			t.Errorf("n1 %q: %+v", reason, d)
		}
	}
	f.OnboardingReason = onboarding.ReasonNotWorking
	if d := Decide(StepN1, f, s); d.Action != ActionExit || d.JoinFix != onboarding.ReasonNotWorking {
		t.Errorf("n1 not working joins the fix list: %+v", d)
	}
	for _, reason := range []string{"", "other", "4"} {
		f.OnboardingReason = reason
		if d := Decide(StepN1, f, s); d.Action != ActionSend || d.Template != TemplateR3Keep || d.NextStep != StepEnd || d.NextDay != 30 {
			t.Errorf("n1 %q goes straight to keep-or-go: %+v", reason, d)
		}
	}
	f.OnboardingReason = "not_needed"
	if d := Decide(StepR3, f, s); d.Template != TemplateR3Keep || d.NextStep != StepEnd || d.NextDay != 60 {
		t.Errorf("n r3: %+v", d)
	}
	f.Reactivated = true
	if d := Decide(StepN1, f, s); d.ExitReason != ExitReactivated {
		t.Errorf("a first connection ends it: %+v", d)
	}
}

func TestFixAndWinbackStreams(t *testing.T) {
	s := DefaultSchedule
	f := healthy(StreamFix)
	if d := Decide(StepF1, f, s); d.ExitReason != ExitDone {
		t.Errorf("f1 without a fix: %+v", d)
	}
	f.Highlights = []string{HighlightConnectionFixes}
	if d := Decide(StepF1, f, s); d.Action != ActionSend || d.Template != TemplateR1News || d.Variant != VariantFixFollowup || d.NextStep != "" {
		t.Errorf("f1: %+v", d)
	}
	w := healthy(StreamWinback)
	if d := Decide(StepW1, w, s); d.Action != ActionSend || d.Template != TemplateW1Winback || d.Variant != VariantDefault {
		t.Errorf("w1: %+v", d)
	}
	w.OfferAllowed = true
	if d := Decide(StepW1, w, s); d.Variant != VariantOffer {
		t.Errorf("w1 offer: %+v", d)
	}
	w.Pro = true
	if d := Decide(StepW1, w, s); d.ExitReason != ExitPro {
		t.Errorf("w1 back on Pro: %+v", d)
	}
}

func TestRouteAnswer(t *testing.T) {
	if a := RouteAnswer(ReasonStopped); a.JoinFix != ReasonStopped || a.ExitReason != ExitAnswered {
		t.Errorf("stopped: %+v", a)
	}
	if a := RouteAnswer(ReasonSlow); a.JoinFix != ReasonSlow {
		t.Errorf("slow: %+v", a)
	}
	if a := RouteAnswer(ReasonNotNeeded); !a.Quiet {
		t.Errorf("not needed: %+v", a)
	}
	if a := RouteAnswer(ReasonSwitched); a.Suppress != SuppressSwitched {
		t.Errorf("switched: %+v", a)
	}
	if a := RouteAnswer(ReasonOther); a.ExitReason != ExitEngaged || a.Suppress != "" {
		t.Errorf("other: %+v", a)
	}
	if a := RouteAnswer("nonsense"); a.ExitReason != "" {
		t.Errorf("unknown: %+v", a)
	}
	if AppReason(ReasonSlow) != onboarding.ReasonNotWorking || AppReason(ReasonSwitched) != onboarding.ReasonOther {
		t.Error("app reasons")
	}
	if !LiftableSuppression(SuppressSunset) || LiftableSuppression("complained") {
		t.Error("liftable")
	}
}

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestHighlights(t *testing.T) {
	now := day("2026-12-01")
	from, to := day("2026-10-12"), day("2026-10-27")
	hs := []*Highlight{
		{Id: HighlightConnectionFixes, Released: map[string]time.Time{"ios": day("2026-11-20"), "android": day("2026-10-16")}, Fixes: []string{"stopped", "slow", "not_working"}},
		{Id: HighlightConnectionRegion, Countries: []string{"CN"}, Released: map[string]time.Time{"*": day("2026-11-10")}, Fixes: []string{"stopped", "not_working"}},
		{Id: HighlightOutages, Released: map[string]time.Time{"*": day("2026-10-27")}, LastConnectFrom: &from, LastConnectTo: &to},
		{Id: HighlightPostQuantum, Released: map[string]time.Time{"*": day("2027-01-01")}},
		{Id: "unknown", Released: map[string]time.Time{"*": day("2026-01-01")}},
	}
	// an iPhone user who last connected on 10-20: the iOS fixes and the
	// outages page; post-quantum isn't out yet; unknown ids never render
	got := NewsHighlights(hs, "ios", "US", day("2026-10-20"), now)
	if !slices.Equal(got, []string{HighlightConnectionFixes, HighlightOutages}) {
		t.Errorf("ios news: %v", got)
	}
	// an Android user who last connected after the Play release: no fix news
	got = NewsHighlights(hs, "android", "US", day("2026-10-20"), now)
	if !slices.Equal(got, []string{HighlightOutages}) {
		t.Errorf("android news: %v", got)
	}
	// a September lapse wasn't there for the outages
	got = NewsHighlights(hs, "ios", "US", day("2026-09-20"), now)
	if !slices.Equal(got, []string{HighlightConnectionFixes}) {
		t.Errorf("september news: %v", got)
	}
	// the China setting only for China
	got = NewsHighlights(hs, "windows", "CN", day("2026-11-01"), now)
	if !slices.Equal(got, []string{HighlightConnectionRegion}) {
		t.Errorf("china news: %v", got)
	}
	// fix follow-ups: only fixes for the reason, released after the answer
	got = FixHighlights(hs, "ios", "US", "slow", day("2026-11-02"), now)
	if !slices.Equal(got, []string{HighlightConnectionFixes}) {
		t.Errorf("fix for slow on ios: %v", got)
	}
	if got := FixHighlights(hs, "android", "US", "slow", day("2026-11-02"), now); len(got) != 0 {
		t.Errorf("android fix shipped before the answer: %v", got)
	}
	if got := FixHighlights(hs, "ios", "US", "switched", day("2026-11-02"), now); len(got) != 0 {
		t.Errorf("not a fixable reason: %v", got)
	}
}

func TestSendAt(t *testing.T) {
	berlin, _ := time.LoadLocation("Europe/Berlin")
	started := time.Date(2026, 11, 2, 2, 30, 0, 0, time.UTC)
	now := started
	at := SendAt(started, 7, berlin, DefaultSchedule, now)
	if want := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC); !at.Equal(want) {
		t.Errorf("10:00 Berlin a week later: %s, want %s", at, want)
	}
	// a step already due goes out at the next window edge, not in the past
	late := started.Add(40 * 24 * time.Hour)
	if at := SendAt(started, 7, berlin, DefaultSchedule, late); at.Before(late) {
		t.Errorf("past send time: %s", at)
	}
}

// The daily scan runs at 02:30 UTC, after that day's 10:00 in the Americas and
// East Asia, so those first steps are already due. Inside the window (09:00 to
// 20:00) they go out a minute on; a network already past the window's end
// waits for the next morning instead of getting mail at night.
func TestSendAtLateScanKeepsTheWindow(t *testing.T) {
	zone := func(name string) *time.Location {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Fatalf("%s: %s", name, err)
		}
		return loc
	}
	scan := time.Date(2026, 11, 4, 2, 30, 20, 0, time.UTC)
	for _, c := range []struct {
		zone string
		want time.Time
	}{
		// 21:30, 20:30 and 23:30 local: past the window, so 09:00 the next morning
		{"America/New_York", time.Date(2026, 11, 4, 9, 0, 0, 0, zone("America/New_York"))},
		{"America/Chicago", time.Date(2026, 11, 4, 9, 0, 0, 0, zone("America/Chicago"))},
		{"America/Sao_Paulo", time.Date(2026, 11, 4, 9, 0, 0, 0, zone("America/Sao_Paulo"))},
		// 18:30 and 11:30 local: inside the window, a minute on
		{"America/Los_Angeles", scan.Add(time.Minute)},
		{"Asia/Tokyo", scan.Add(time.Minute)},
		// 06:00 local: before the send hour, so 10:00 that day
		{"Asia/Tehran", time.Date(2026, 11, 4, 10, 0, 0, 0, zone("Asia/Tehran"))},
		{"UTC", time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)},
	} {
		if at := SendAt(scan, 0, zone(c.zone), DefaultSchedule, scan); !at.Equal(c.want) {
			t.Errorf("%s: %s, want %s", c.zone, at.In(zone(c.zone)), c.want)
		}
	}
}

// Every template's params are exactly its manifest's keys.
func TestParamsMatchManifests(t *testing.T) {
	mmm := filepath.Join("..", "..", "mmm")
	if v := os.Getenv("URNETWORK_MMM_ROOT"); v != "" {
		mmm = v
	}
	root := filepath.Join(mmm, "reengagement", "templates")
	if _, err := os.Stat(root); err != nil {
		t.Skip("mmm/reengagement/templates is not checked out next to server (set URNETWORK_MMM_ROOT)")
	}
	now := day("2026-12-01")
	last, answered, ended, expires := day("2026-10-20"), day("2026-11-02"), day("2026-10-01"), day("2026-12-15")
	base := ParamsInput{
		Locale: "en", Location: time.UTC, Now: now, SiteUrl: "https://ur.io", Token: "T", CompanyLine: "co", DailyDataGb: 30,
		LastConnectAt: &last, ReasonAt: &answered, ProEndedAt: &ended, OfferExpiresAt: &expires,
		Highlights: []string{HighlightOutages},
	}
	for _, tv := range [][2]string{
		{TemplateR1News, VariantLapsed}, {TemplateR1News, VariantFixFollowup}, {TemplateR2Reason, VariantLapsed},
		{TemplateR3Keep, VariantDefault}, {TemplateN1Nudge, VariantNotNeeded}, {TemplateN1Nudge, VariantTrust},
		{TemplateW1Winback, VariantDefault}, {TemplateW1Winback, VariantOffer},
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
		in.Template, in.Variant = tv[0], tv[1]
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
	}
	p := BuildParams(ParamsInput{Template: TemplateR3Keep, Variant: VariantDefault, SiteUrl: "https://ur.io/", Token: "T", DailyDataGb: 30, Now: now})
	if p["cta_url"] != "https://ur.io/o/keep?t=T" || p["daily_data_gb"] != "30 GiB" {
		t.Errorf("keep params: %v", p)
	}
	p = BuildParams(ParamsInput{Template: TemplateR2Reason, Variant: VariantLapsed, SiteUrl: "https://ur.io", Token: "T", LastConnectAt: &last, Locale: "en", Now: now})
	if p["reason_url_2"] != "https://ur.io/f/T?why=slow" || p["last_connect_date"] != "Oct 20" {
		t.Errorf("reason params: %v", p)
	}
	p = BuildParams(ParamsInput{Template: TemplateR1News, Variant: VariantLapsed, SiteUrl: "https://ur.io", Token: "T", Highlights: []string{HighlightConnectionFixes}, Now: now})
	if p["hl_connection_fixes"] != "1" || p["hl_outages"] != "" || p["hl_any"] != "1" {
		t.Errorf("highlight flags: %v", p)
	}
	if CtaSlug(TemplateN1Nudge, VariantTrust) != "learn" || CtaSlug(TemplateR1News, VariantFixFollowup) != "update" || CtaSlug(TemplateW1Winback, VariantDefault) != "winback" {
		t.Error("cta slugs")
	}
}

// W1 goes 30 days after Pro stopped. Stripe, crypto, x402 and manual windows
// already carry the grace in their end; the stores' may still be retrying a
// failed renewal, so W1 waits for their retry period there.
func TestWinbackAfterDays(t *testing.T) {
	s := Schedule{}.WithDefaults()
	if got := s.WinbackAfterDays(true); got != 30 {
		t.Fatalf("grace markets: W1 after %d days, want 30 (the grace is already in end_time)", got)
	}
	if got := s.WinbackAfterDays(false); got != 60 {
		t.Fatalf("store markets: W1 after %d days, want 60 (billing retry)", got)
	}
	s.WinbackDays = 90
	if got := s.WinbackAfterDays(false); got != 90 {
		t.Fatalf("a longer win-back delay wins over the retry period: got %d", got)
	}
}
