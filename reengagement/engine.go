// SPDX-License-Identifier: MPL-2.0

// Package reengagement holds the re-engagement email program's decisions
// (mmm/reengagement/PLAN.md "THE SEQUENCES"): which stream a network is in,
// what each step does at send time, which "what changed" items apply, and the
// template params. Pure, like onboarding/campaign.go: the controller loads the
// facts, talks to Brevo and the scheduler, and records what happened.
package reengagement

import (
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server/onboarding"
)

// Streams. A network is in at most one episode at a time.
const (
	// connected before, then 30 days without a connection
	StreamLapsed = "lapsed"
	// signed up with email, never connected, onboarding over (day 30)
	StreamNeverActivated = "never_activated"
	// answered "it didn't work" / "stopped working" / "too slow"; one mail
	// when a fix for its platform ships
	StreamFix = "fix"
	// paid for Pro, which ended and was not renewed
	StreamWinback = "winback"
)

// Steps: the flow steps the tokens and events carry (onboarding/token.go), and
// StepEnd, the sunset check that sends nothing.
const (
	StepR1  = onboarding.StepR1
	StepR2  = onboarding.StepR2
	StepR3  = onboarding.StepR3
	StepN1  = onboarding.StepN1
	StepF1  = onboarding.StepF1
	StepW1  = onboarding.StepW1
	StepEnd = "end"
)

// Templates (mmm/reengagement/templates/<template>/<variant>), uploaded to
// Brevo as reengagement/<template>/<variant>/<locale>.
const (
	TemplateR1News    = "r1_news"
	TemplateR2Reason  = "r2_reason"
	TemplateR3Keep    = "r3_keep"
	TemplateN1Nudge   = "n1_nudge"
	TemplateW1Winback = "w1_winback"
)

const (
	VariantLapsed      = "lapsed"
	VariantFixFollowup = "fix_followup"
	VariantDefault     = "default"
	VariantNotNeeded   = "not_needed"
	VariantTrust       = "trust"
	VariantOffer       = "offer"
)

// The R2 one-tap reasons.
const (
	ReasonStopped   = "stopped"
	ReasonSlow      = "slow"
	ReasonNotNeeded = "not_needed"
	ReasonSwitched  = "switched"
	ReasonOther     = "other"
)

// R2Reasons is the order of R2's buttons (reason_url_1..5).
var R2Reasons = []string{ReasonStopped, ReasonSlow, ReasonNotNeeded, ReasonSwitched, ReasonOther}

// IsR2Reason reports whether reason is one of R2's buttons.
func IsR2Reason(reason string) bool {
	for _, r := range R2Reasons {
		if r == reason {
			return true
		}
	}
	return false
}

// AppReason is the reason the apps' feedback screen understands for a
// re-engagement answer (it knows the onboarding four), so the screen opens
// pre-filled; the precise answer is recorded server-side either way.
func AppReason(reason string) string {
	switch reason {
	case ReasonStopped, ReasonSlow:
		return onboarding.ReasonNotWorking
	case ReasonNotNeeded:
		return onboarding.ReasonNotNeeded
	default:
		return onboarding.ReasonOther
	}
}

// Actions.
const (
	ActionSend = "send"
	// a holdout network: the step is recorded as a virtual send at the same
	// moment, so both arms run on the same clock
	ActionVirtual = "virtual"
	ActionSkip    = "skip"
	ActionExit    = "exit"
)

// Exit reasons.
const (
	ExitNoEmail          = "no_email"
	ExitNetworkDeleted   = "network_deleted"
	ExitOptOut           = "opt_out"
	ExitSuppressed       = "suppressed"
	ExitBounced          = "bounced"
	ExitComplained       = "complained"
	ExitPro              = "pro"
	ExitOnboardingActive = "onboarding_active"
	ExitReactivated      = "reactivated"
	// the reader answered and the answer routed it elsewhere (fix list,
	// quiet, suppressed)
	ExitAnswered = "answered"
	// clicked or answered without reconnecting: the address is live, no
	// keep-or-go needed
	ExitEngaged = "engaged"
	ExitKept    = "kept"
	ExitSunset  = "sunset"
	ExitCapped  = "capped"
	ExitDone    = "done"
)

// Suppression reasons this program applies (account_preferences.marketing_suppressed_reason).
const (
	SuppressSunset   = "sunset"
	SuppressSwitched = "switched"
)

// LiftableSuppression reports whether a connection lifts the suppression: the
// program's own (silence, "switched"); never a bounce, complaint or
// unsubscribe.
func LiftableSuppression(reason string) bool {
	return reason == SuppressSunset || reason == SuppressSwitched
}

// Schedule holds the day offsets and limits (reengagement.yml `schedule`).
type Schedule struct {
	DormantDays      int
	R2AfterR1Days    int
	R3Day            int
	EndAfterR3Days   int
	NeverActiveDay   int
	NKeepAfterN1Days int
	CooldownDays     int
	QuietDays        int
	FixWindowDays    int
	WinbackDays      int
	// how long Apple and Google may still retry a failed renewal after the
	// recorded window ends (billing retry, grace and account hold): W1 waits
	// at least this long on those stores (PLAN.md "Billing failures are not
	// churn")
	StoreRetryDays int
	KeepDays       int
	MinGapDays     int
	MaxPerYear     int
	SendHour       int
	SendWindow     string
}

// DefaultSchedule is PLAN.md's: lapse at 30 days, R2 a week after R1, R3 at
// day 30, the sunset check 30 days later; never-activated at day 30 after
// sign-up; 180 days between episodes; W1 30 days after Pro ended (60 on the
// stores, which retry failed renewals that long); a week between marketing
// emails; six re-engagement emails a year at most.
var DefaultSchedule = Schedule{
	DormantDays:      30,
	R2AfterR1Days:    7,
	R3Day:            30,
	EndAfterR3Days:   30,
	NeverActiveDay:   30,
	NKeepAfterN1Days: 30,
	CooldownDays:     180,
	QuietDays:        180,
	FixWindowDays:    180,
	WinbackDays:      30,
	StoreRetryDays:   60,
	KeepDays:         365,
	MinGapDays:       7,
	MaxPerYear:       6,
	SendHour:         10,
	SendWindow:       "09:00-20:00",
}

// WithDefaults fills unset (zero) fields from DefaultSchedule.
func (s Schedule) WithDefaults() Schedule {
	d := DefaultSchedule
	pick := func(v int, def int) int {
		if v <= 0 {
			return def
		}
		return v
	}
	s.DormantDays = pick(s.DormantDays, d.DormantDays)
	s.R2AfterR1Days = pick(s.R2AfterR1Days, d.R2AfterR1Days)
	s.R3Day = pick(s.R3Day, d.R3Day)
	s.EndAfterR3Days = pick(s.EndAfterR3Days, d.EndAfterR3Days)
	s.NeverActiveDay = pick(s.NeverActiveDay, d.NeverActiveDay)
	s.NKeepAfterN1Days = pick(s.NKeepAfterN1Days, d.NKeepAfterN1Days)
	s.CooldownDays = pick(s.CooldownDays, d.CooldownDays)
	s.QuietDays = pick(s.QuietDays, d.QuietDays)
	s.FixWindowDays = pick(s.FixWindowDays, d.FixWindowDays)
	s.WinbackDays = pick(s.WinbackDays, d.WinbackDays)
	s.StoreRetryDays = pick(s.StoreRetryDays, d.StoreRetryDays)
	s.KeepDays = pick(s.KeepDays, d.KeepDays)
	s.MinGapDays = pick(s.MinGapDays, d.MinGapDays)
	s.MaxPerYear = pick(s.MaxPerYear, d.MaxPerYear)
	if s.SendHour <= 0 || 23 < s.SendHour {
		s.SendHour = d.SendHour
	}
	if strings.TrimSpace(s.SendWindow) == "" {
		s.SendWindow = d.SendWindow
	}
	return s
}

// WinbackAfterDays is how many days after a supporter window's recorded end
// W1 goes. Stripe, crypto, x402 and manual windows record their end with our
// 30-day grace already in it (Pro stops at end_time), so W1 goes WinbackDays
// after it. App Store and Play windows end at the store's expiry, and the store
// may still be retrying a failed renewal: W1 waits for the later of WinbackDays
// and StoreRetryDays.
func (s Schedule) WinbackAfterDays(graceInEnd bool) int {
	if graceInEnd || s.StoreRetryDays <= s.WinbackDays {
		return s.WinbackDays
	}
	return s.StoreRetryDays
}

// Facts is what a step decision reads, loaded at send time.
type Facts struct {
	Stream string

	// ----- global -----
	HasEmail       bool
	NetworkExists  bool
	ProductUpdates bool
	Suppressed     bool
	Bounced        bool
	Complained     bool
	Pro            bool
	Holdout        bool
	// the mail policy's exit (onboarding/policy.go), or ""
	PolicyExit       string
	OnboardingActive bool
	SentLastYear     int

	// ----- progress in the episode -----
	// a connection since the episode began (never-activated: ever)
	Reactivated bool
	// a re-engagement landing click since the episode began
	Clicked bool
	// the R2 answer in this episode, or ""
	Reason string
	// the onboarding feedback email's answer (onboarding.Reason*), for N1
	OnboardingReason string
	// a keep click inside the keep window
	Kept bool
	// the "what changed" items that apply to the network now
	Highlights []string
	// W1: the comeback offer may be issued for this network (store, country,
	// once per 12 months, never for anyone who never paid)
	OfferAllowed bool
}

// Decision is what a step does.
type Decision struct {
	Action     string
	Template   string
	Variant    string
	ExitReason string
	NextStep   string
	// days after the episode start
	NextDay int
	// apply this marketing suppression with the exit
	Suppress string
	// the network joins the fix list with this reason
	JoinFix string
	// quiet for the schedule's QuietDays
	Quiet bool
}

// GlobalExit is the exit that applies before any step, or "".
func GlobalExit(f Facts) string {
	switch {
	case !f.HasEmail:
		return ExitNoEmail
	case !f.NetworkExists:
		return ExitNetworkDeleted
	case !f.ProductUpdates:
		return ExitOptOut
	case f.Suppressed:
		return ExitSuppressed
	case f.PolicyExit != "":
		return f.PolicyExit
	case f.Complained:
		return ExitComplained
	case f.Bounced:
		return ExitBounced
	case f.Pro:
		return ExitPro
	case f.OnboardingActive:
		return ExitOnboardingActive
	}
	return ""
}

// Decide applies the plan's table to one step of the network's stream.
func Decide(step string, f Facts, s Schedule) Decision {
	s = s.WithDefaults()
	if reason := GlobalExit(f); reason != "" {
		return Decision{Action: ActionExit, ExitReason: reason}
	}
	if step != StepEnd && 0 < s.MaxPerYear && s.MaxPerYear <= f.SentLastYear {
		return Decision{Action: ActionExit, ExitReason: ExitCapped}
	}
	mail := func(template string, variant string, next string, nextDay int) Decision {
		action := ActionSend
		if f.Holdout {
			action = ActionVirtual
		}
		return Decision{Action: action, Template: template, Variant: variant, NextStep: next, NextDay: nextDay}
	}
	exit := func(reason string) Decision {
		return Decision{Action: ActionExit, ExitReason: reason}
	}

	switch f.Stream {
	case StreamLapsed:
		if f.Reactivated {
			return exit(ExitReactivated)
		}
		switch step {
		case StepR1:
			if 0 < len(f.Highlights) {
				return mail(TemplateR1News, VariantLapsed, StepR2, s.R2AfterR1Days)
			}
			// nothing changed for this network: the reason ask takes R1's day
			return Decision{Action: ActionSkip, NextStep: StepR2, NextDay: 0}
		case StepR2:
			if f.Reason != "" {
				return exit(ExitAnswered)
			}
			return mail(TemplateR2Reason, VariantLapsed, StepR3, s.R3Day)
		case StepR3:
			if f.Reason != "" || f.Clicked {
				return exit(ExitEngaged)
			}
			return mail(TemplateR3Keep, VariantDefault, StepEnd, s.R3Day+s.EndAfterR3Days)
		case StepEnd:
			return sunset(f)
		}
	case StreamNeverActivated:
		if f.Reactivated {
			return exit(ExitReactivated)
		}
		switch step {
		case StepN1:
			switch onboarding.FeedbackReasonCode(f.OnboardingReason) {
			case onboarding.ReasonNotNeeded:
				return mail(TemplateN1Nudge, VariantNotNeeded, StepR3, s.NKeepAfterN1Days)
			case onboarding.ReasonTrust:
				return mail(TemplateN1Nudge, VariantTrust, StepR3, s.NKeepAfterN1Days)
			case onboarding.ReasonNotWorking:
				return Decision{Action: ActionExit, ExitReason: ExitAnswered, JoinFix: onboarding.ReasonNotWorking}
			default:
				return mail(TemplateR3Keep, VariantDefault, StepEnd, s.EndAfterR3Days)
			}
		case StepR3:
			if f.Clicked {
				return exit(ExitEngaged)
			}
			return mail(TemplateR3Keep, VariantDefault, StepEnd, s.NKeepAfterN1Days+s.EndAfterR3Days)
		case StepEnd:
			return sunset(f)
		}
	case StreamFix:
		if step == StepF1 {
			if f.Reactivated {
				return exit(ExitReactivated)
			}
			if len(f.Highlights) == 0 {
				return exit(ExitDone)
			}
			return mail(TemplateR1News, VariantFixFollowup, "", 0)
		}
	case StreamWinback:
		if step == StepW1 {
			variant := VariantDefault
			if f.OfferAllowed {
				variant = VariantOffer
			}
			return mail(TemplateW1Winback, variant, "", 0)
		}
	}
	return exit(ExitDone)
}

// sunset is the end of an episode that asked "should I keep writing?". A
// holdout network was never asked: its R3 was a virtual send, so its silence
// says nothing about the address, and it exits done without the sunset
// suppression (until the 2026-11-13 review every holdout network was
// suppressed here at day 60, and so left out of all later mail).
func sunset(f Facts) Decision {
	switch {
	case f.Holdout:
		return Decision{Action: ActionExit, ExitReason: ExitDone}
	case f.Kept:
		return Decision{Action: ActionExit, ExitReason: ExitKept}
	case f.Clicked || f.Reason != "":
		return Decision{Action: ActionExit, ExitReason: ExitEngaged}
	}
	return Decision{Action: ActionExit, ExitReason: ExitSunset, Suppress: SuppressSunset}
}

// Answer is what an R2 reason does to the network (recorded when it is
// tapped): end the episode and route the network.
type Answer struct {
	// the episode exits with this reason ("" = it continues)
	ExitReason string
	JoinFix    string
	Quiet      bool
	Suppress   string
}

// RouteAnswer maps an R2 reason to its effect (PLAN.md "What the R2 answers
// do").
func RouteAnswer(reason string) Answer {
	switch reason {
	case ReasonStopped, ReasonSlow:
		return Answer{ExitReason: ExitAnswered, JoinFix: reason}
	case ReasonNotNeeded:
		return Answer{ExitReason: ExitAnswered, Quiet: true}
	case ReasonSwitched:
		return Answer{ExitReason: ExitAnswered, Suppress: SuppressSwitched}
	case ReasonOther:
		// the feedback form opens with the reason; the episode ends quietly
		return Answer{ExitReason: ExitEngaged}
	}
	return Answer{}
}

// ----- the "what changed" items -----

// The highlight ids the templates have a paragraph for.
const (
	HighlightConnectionFixes  = "connection_fixes"
	HighlightConnectionRegion = "connection_region"
	HighlightPostQuantum      = "post_quantum"
	HighlightOutages          = "outages"
)

// TemplateHighlights is the template paragraphs' order.
var TemplateHighlights = []string{HighlightConnectionFixes, HighlightConnectionRegion, HighlightPostQuantum, HighlightOutages}

// Highlight is one configured "what changed" item (reengagement.yml
// `highlights`).
type Highlight struct {
	Id string
	// empty = every platform
	Platforms []string
	// empty = every country
	Countries []string
	// when the change reached each platform's installable build; "*" for
	// every platform at once. A platform without a date hasn't got it.
	Released map[string]time.Time
	// only for networks whose last connection fell in [From, To)
	LastConnectFrom *time.Time
	LastConnectTo   *time.Time
	// the reasons a fix answers (stopped, slow, not_working); empty = it is
	// news only, not a fix follow-up
	Fixes []string
}

func containsFold(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(strings.TrimSpace(x), v) {
			return true
		}
	}
	return false
}

func (h *Highlight) releasedAt(platform string) (time.Time, bool) {
	if t, ok := h.Released[platform]; ok {
		return t, true
	}
	t, ok := h.Released["*"]
	return t, ok
}

// applies reports whether the item is news to a network on `platform` in
// `country` whose news starts at `since` (its last connection, or its
// answer), as of now.
func (h *Highlight) applies(platform string, country string, since time.Time, lastConnect *time.Time, now time.Time) bool {
	if 0 < len(h.Platforms) && !containsFold(h.Platforms, platform) {
		return false
	}
	if 0 < len(h.Countries) && !containsFold(h.Countries, country) {
		return false
	}
	released, ok := h.releasedAt(platform)
	if !ok || released.After(now) || !released.After(since) {
		return false
	}
	if h.LastConnectFrom != nil || h.LastConnectTo != nil {
		if lastConnect == nil {
			return false
		}
		if h.LastConnectFrom != nil && lastConnect.Before(*h.LastConnectFrom) {
			return false
		}
		if h.LastConnectTo != nil && !lastConnect.Before(*h.LastConnectTo) {
			return false
		}
	}
	return true
}

// NewsHighlights are the template items that are news to a lapsed network:
// released for its platform (and country) after its last connection.
func NewsHighlights(highlights []*Highlight, platform string, country string, lastConnect time.Time, now time.Time) []string {
	out := []string{}
	for _, id := range TemplateHighlights {
		for _, h := range highlights {
			if h != nil && h.Id == id && h.applies(platform, country, lastConnect, &lastConnect, now) {
				out = append(out, id)
				break
			}
		}
	}
	return out
}

// FixHighlights are the items that answer a network's reported problem:
// fixes for its reason, released for its platform after the answer.
func FixHighlights(highlights []*Highlight, platform string, country string, reason string, answeredAt time.Time, now time.Time) []string {
	out := []string{}
	for _, id := range TemplateHighlights {
		for _, h := range highlights {
			if h != nil && h.Id == id && containsFold(h.Fixes, reason) && h.applies(platform, country, answeredAt, nil, now) {
				out = append(out, id)
				break
			}
		}
	}
	return out
}

// ----- send time -----

// SendAt places a step: `day` days after the episode start, at the schedule's
// hour in the network's zone, clamped into the send window, and never in the
// past.
func SendAt(startedAt time.Time, day int, loc *time.Location, s Schedule, now time.Time) time.Time {
	s = s.WithDefaults()
	if loc == nil {
		loc = time.UTC
	}
	w, err := onboarding.ParseSendWindow(s.SendWindow)
	if err != nil {
		w = onboarding.DefaultSendWindow
	}
	local := startedAt.In(loc)
	anchor := time.Date(local.Year(), local.Month(), local.Day(), s.SendHour, 0, 0, 0, loc)
	at := onboarding.SendAt(anchor, day, loc, w)
	if at.Before(now) {
		// due already (a late scan, a retry): the next window edge
		at = onboarding.DueSendAt(now, loc, w)
	}
	return at
}

// ----- params -----

// ParamsInput is the data one send renders from.
type ParamsInput struct {
	Template string
	Variant  string
	Locale   string
	Location *time.Location
	Now      time.Time

	SiteUrl     string
	Token       string
	CompanyLine string
	DailyDataGb float64

	LastConnectAt *time.Time
	ReasonAt      *time.Time
	ProEndedAt    *time.Time
	// the comeback offer's real expiry (W1 offer)
	OfferExpiresAt *time.Time

	Highlights []string
	// the post-quantum fix is live (n1 trust)
	PqReady bool
	// Apple lists a win-back offer for the subscription (W1)
	AppleOfferHint bool
}

// CtaSlug is the landing slug of a template's button.
func CtaSlug(template string, variant string) string {
	switch template {
	case TemplateR1News:
		if variant == VariantFixFollowup {
			return onboarding.SlugUpdate
		}
		return onboarding.SlugConnect
	case TemplateR3Keep:
		return onboarding.SlugKeep
	case TemplateN1Nudge:
		if variant == VariantTrust {
			return onboarding.SlugLearn
		}
		return onboarding.SlugConnect
	case TemplateW1Winback:
		return onboarding.SlugWinback
	case TemplateR2Reason:
		return onboarding.SlugFeedback
	}
	return onboarding.SlugConnect
}

func flag(on bool) string {
	if on {
		return "1"
	}
	return ""
}

// BuildParams is exactly the manifest's keys for the template and variant
// (mmm/reengagement/templates/<template>/<variant>/manifest.json).
func BuildParams(in ParamsInput) map[string]string {
	site := strings.TrimRight(in.SiteUrl, "/")
	cta := site + "/o/" + CtaSlug(in.Template, in.Variant) + "?t=" + in.Token
	day := func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return onboarding.FormatDayLocale(*t, in.Location, in.Locale, in.Now)
	}
	data := onboarding.FormatGb(in.DailyDataGb)
	highlights := func(p map[string]string, withAny bool) {
		any := false
		for _, id := range TemplateHighlights {
			on := containsFold(in.Highlights, id)
			p["hl_"+id] = flag(on)
			any = any || on
		}
		if withAny {
			p["hl_any"] = flag(any)
		}
	}
	p := map[string]string{"company_line": in.CompanyLine}
	switch in.Template {
	case TemplateR1News:
		p["cta_url"] = cta
		if in.Variant == VariantFixFollowup {
			p["reason_date"] = day(in.ReasonAt)
			highlights(p, false)
		} else {
			p["last_connect_date"] = day(in.LastConnectAt)
			p["daily_data_gb"] = data
			highlights(p, true)
		}
	case TemplateR2Reason:
		p["last_connect_date"] = day(in.LastConnectAt)
		for i, reason := range R2Reasons {
			p["reason_url_"+strconv.Itoa(i+1)] = onboarding.FeedbackUrl(in.SiteUrl, in.Token, "why="+reason)
		}
	case TemplateR3Keep:
		p["cta_url"] = cta
		p["daily_data_gb"] = data
	case TemplateN1Nudge:
		p["cta_url"] = cta
		if in.Variant == VariantTrust {
			p["pq_ready"] = flag(in.PqReady)
		} else {
			p["daily_data_gb"] = data
		}
	case TemplateW1Winback:
		p["cta_url"] = cta
		p["pro_ended_date"] = day(in.ProEndedAt)
		p["daily_data_gb"] = data
		if in.Variant == VariantOffer {
			expires := ""
			if in.OfferExpiresAt != nil {
				expires = onboarding.FormatDateTimeLocale(*in.OfferExpiresAt, in.Location, in.Locale)
			}
			p["offer_expires_at"] = expires
		} else {
			p["apple_offer_hint"] = flag(in.AppleOfferHint)
		}
	}
	return p
}
