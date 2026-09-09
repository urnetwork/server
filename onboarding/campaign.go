// SPDX-License-Identifier: MPL-2.0

package onboarding

// The onboarding email campaign state machine (mmm/onboarding/PLAN.md "THE
// EMAIL SEQUENCE"). This file is pure: it decides what a step does from facts
// the controller loads, and where the next send lands in the network's local
// time. Nothing here touches the database or Brevo, so every branch has a unit
// test.

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Steps of the sequence. Paths share the step names; the day offset and the
// template differ by path (A saw the in-app offer, B did not).
const (
	StepE1 = "e1"
	StepE2 = "e2"
	StepE3 = "e3"
	StepE4 = "e4"
	StepE5 = "e5"
)

// Paths. PathA = the welcome offer was issued in-app (the network saw the
// offer screen); PathB = it was not, so the campaign issues it by email (E4b).
const (
	PathA = "A"
	PathB = "B"
)

// Templates are the mmm/onboarding/templates/<step> names, uploaded to Brevo
// as onboarding/<template>/<variant>/<locale>.
const (
	TemplateE1Connect    = "e1_connect"
	TemplateE2Widget     = "e2_widget"
	TemplateE3LastChance = "e3_last_chance"
	TemplateE4Feedback   = "e4_feedback"
	TemplateE4bOffer     = "e4b_offer"
)

// Template variants.
const (
	VariantDefault      = "default"
	VariantActivated    = "activated"
	VariantNotActivated = "not_activated"
	VariantEngaged      = "engaged"
)

// Openings of the shared last-chance template (params.opening).
const (
	OpeningJoined   = "joined"
	OpeningLastNote = "last_note"
)

// Exit reasons recorded on the network_onboarding row.
const (
	ExitNoEmail        = "no_email"
	ExitHoldout        = "holdout"
	ExitOptOut         = "opt_out"
	ExitPro            = "pro"
	ExitNetworkDeleted = "network_deleted"
	ExitBounced        = "bounced"
	ExitComplained     = "complained"
	ExitDone           = "done"
)

// Actions of a decision.
const (
	ActionSend = "send"
	ActionSkip = "skip"
	ActionExit = "exit"
)

// Schedule is the day offset of every step from the account email, per path,
// and the local send window (onboarding.yml schedule.*).
type Schedule struct {
	E1Day  int
	E2Day  int
	E3aDay int
	E4aDay int
	E3bDay int
	E4bDay int
	E5bDay int
	// "HH:MM-HH:MM" local; sends outside it are clamped to its edges
	SendWindow string
}

// DefaultSchedule is the plan's table.
var DefaultSchedule = Schedule{
	E1Day:      1,
	E2Day:      3,
	E3aDay:     4,
	E4aDay:     6,
	E3bDay:     5,
	E4bDay:     7,
	E5bDay:     11,
	SendWindow: "08:00-21:00",
}

// Day is the offset in days of `step` on `path`, or -1 when the path has no
// such step (E5 exists only on path B).
func (s Schedule) Day(step string, path string) int {
	switch step {
	case StepE1:
		return s.E1Day
	case StepE2:
		return s.E2Day
	case StepE3:
		if path == PathA {
			return s.E3aDay
		}
		return s.E3bDay
	case StepE4:
		if path == PathA {
			return s.E4aDay
		}
		return s.E4bDay
	case StepE5:
		if path == PathB {
			return s.E5bDay
		}
	}
	return -1
}

// NextStep is the step after `step` on `path`, or "" when the sequence is over.
func NextStep(step string, path string) string {
	switch step {
	case "":
		return StepE1
	case StepE1:
		return StepE2
	case StepE2:
		return StepE3
	case StepE3:
		return StepE4
	case StepE4:
		if path == PathB {
			return StepE5
		}
	}
	return ""
}

// Facts is everything a step decision looks at, loaded by the controller at
// send time (never earlier: conditions are re-evaluated when the task runs).
type Facts struct {
	// Path is the effective path now: A when an in-app offer exists, else the
	// row's path, else B. It is re-evaluated at every step.
	Path string

	// ----- global exits -----
	HasEmail       bool
	Holdout        bool // email.sequence variant is `holdout`
	ProductUpdates bool // the sign-up preference; false after an unsubscribe too
	Pro            bool
	NetworkExists  bool
	Bounced        bool
	Complained     bool
	ComplainedStep string

	// ----- step conditions -----
	HasConnected      bool
	WidgetAdded       bool
	OfferState        string // "" (none) | active | redeemed | expired
	FeedbackSubmitted bool
	EmailOpens        int
}

// Offer states, as model.OnboardingOfferState reports them.
const (
	OfferStateNone     = ""
	OfferStateActive   = "active"
	OfferStateRedeemed = "redeemed"
	OfferStateExpired  = "expired"
)

// Decision is what a step does.
type Decision struct {
	Action   string
	Template string
	Variant  string
	// Opening is the last-chance template's opening (params.opening)
	Opening string
	// IssueOffer: issue the welcome offer by email before sending (E4b)
	IssueOffer bool
	// ExitReason when Action == ActionExit
	ExitReason string
	// NextStep after this one on the effective path ("" = done)
	NextStep string
}

// engaged is the plan's feedback-variant rule: two or more email opens or one
// connection.
func engaged(f Facts) bool {
	return 2 <= f.EmailOpens || f.HasConnected
}

// GlobalExit is the exit that applies before any step, or "" when none does.
// Checked at send time for every step, in the plan's order.
func GlobalExit(f Facts) string {
	switch {
	case !f.HasEmail:
		return ExitNoEmail
	case !f.NetworkExists:
		return ExitNetworkDeleted
	case f.Holdout:
		return ExitHoldout
	case !f.ProductUpdates:
		return ExitOptOut
	case f.Pro:
		return ExitPro
	case f.Complained:
		return ExitComplained
	case f.Bounced:
		return ExitBounced
	}
	return ""
}

// Decide applies the plan's table to one step. An unknown step exits as done.
func Decide(step string, f Facts) Decision {
	path := f.Path
	if path != PathA {
		path = PathB
	}
	if reason := GlobalExit(f); reason != "" {
		return Decision{Action: ActionExit, ExitReason: reason}
	}
	next := NextStep(step, path)
	skip := Decision{Action: ActionSkip, NextStep: next}
	send := func(template string, variant string) Decision {
		return Decision{Action: ActionSend, Template: template, Variant: variant, NextStep: next}
	}
	feedback := func() Decision {
		if f.FeedbackSubmitted {
			return skip
		}
		if engaged(f) {
			return send(TemplateE4Feedback, VariantEngaged)
		}
		return send(TemplateE4Feedback, VariantNotActivated)
	}

	switch step {
	case StepE1:
		// day 1: the first connection is the goal; a network that already
		// connected does not need the nudge
		if f.HasConnected {
			return skip
		}
		return send(TemplateE1Connect, VariantDefault)
	case StepE2:
		// day 3: activated -> the widget habit unless a widget exists;
		// not activated -> another connect nudge
		if f.HasConnected {
			if f.WidgetAdded {
				return skip
			}
			return send(TemplateE2Widget, VariantActivated)
		}
		return send(TemplateE2Widget, VariantNotActivated)
	case StepE3:
		if path == PathA {
			// E3a day 4: the in-app offer's last day, still free
			if f.OfferState == OfferStateActive {
				d := send(TemplateE3LastChance, VariantDefault)
				d.Opening = OpeningJoined
				return d
			}
			return skip
		}
		// E3b day 5: feedback
		return feedback()
	case StepE4:
		if path == PathA {
			// E4a day 6: feedback
			return feedback()
		}
		// E4b day 7: issue the offer by email when none exists yet
		if f.OfferState == OfferStateNone {
			d := send(TemplateE4bOffer, VariantDefault)
			d.IssueOffer = true
			return d
		}
		return skip
	case StepE5:
		if path == PathB {
			// E5b day 11: the emailed offer's last day, no complaint on E4b
			if f.OfferState == OfferStateActive && !(f.Complained && f.ComplainedStep == StepE4) {
				d := send(TemplateE3LastChance, VariantDefault)
				d.Opening = OpeningLastNote
				return d
			}
			return skip
		}
	}
	return Decision{Action: ActionExit, ExitReason: ExitDone}
}

// ----- send time -----

// SendWindow is the local window sends are clamped into.
type SendWindow struct {
	StartHour, StartMinute int
	EndHour, EndMinute     int
}

// DefaultSendWindow is 08:00-21:00.
var DefaultSendWindow = SendWindow{StartHour: 8, EndHour: 21}

// ParseSendWindow parses "HH:MM-HH:MM". Empty = the default; malformed = error.
func ParseSendWindow(value string) (SendWindow, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return DefaultSendWindow, nil
	}
	parts := strings.Split(value, "-")
	if len(parts) != 2 {
		return SendWindow{}, fmt.Errorf("send window %q is not HH:MM-HH:MM", value)
	}
	parse := func(s string) (int, int, error) {
		hm := strings.Split(strings.TrimSpace(s), ":")
		if len(hm) != 2 {
			return 0, 0, fmt.Errorf("send window time %q is not HH:MM", s)
		}
		h, err := strconv.Atoi(hm[0])
		if err != nil || h < 0 || 23 < h {
			return 0, 0, fmt.Errorf("send window hour %q", hm[0])
		}
		m, err := strconv.Atoi(hm[1])
		if err != nil || m < 0 || 59 < m {
			return 0, 0, fmt.Errorf("send window minute %q", hm[1])
		}
		return h, m, nil
	}
	sh, sm, err := parse(parts[0])
	if err != nil {
		return SendWindow{}, err
	}
	eh, em, err := parse(parts[1])
	if err != nil {
		return SendWindow{}, err
	}
	w := SendWindow{StartHour: sh, StartMinute: sm, EndHour: eh, EndMinute: em}
	if w.endMinutes() <= w.startMinutes() {
		return SendWindow{}, fmt.Errorf("send window %q ends before it starts", value)
	}
	return w, nil
}

func (w SendWindow) startMinutes() int { return w.StartHour*60 + w.StartMinute }
func (w SendWindow) endMinutes() int   { return w.EndHour*60 + w.EndMinute }

// SendAt places a step: the sign-up's local hour and minute, `day` days later,
// clamped into the window. Local means `loc` (the network's time zone, or UTC
// when unknown, which is why the clamp exists). Day arithmetic goes through
// time.Date in `loc`, so a DST change between sign-up and the step keeps the
// wall-clock hour rather than shifting it.
func SendAt(createdAt time.Time, day int, loc *time.Location, w SendWindow) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	local := createdAt.In(loc)
	at := time.Date(local.Year(), local.Month(), local.Day()+day, local.Hour(), local.Minute(), 0, 0, loc)
	minutes := at.Hour()*60 + at.Minute()
	if minutes < w.startMinutes() {
		at = time.Date(at.Year(), at.Month(), at.Day(), w.StartHour, w.StartMinute, 0, 0, loc)
	} else if w.endMinutes() < minutes {
		at = time.Date(at.Year(), at.Month(), at.Day(), w.EndHour, w.EndMinute, 0, 0, loc)
	}
	return at.UTC()
}

// LoadLocation resolves an IANA zone name, falling back to UTC for "" or an
// unknown name. The fallback is deliberate: a bad zone must never stop a step.
func LoadLocation(name string) *time.Location {
	name = strings.TrimSpace(name)
	if name == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

// EventForWebhook maps a Brevo transactional webhook event name to the
// campaign event name (model.EventEmail*), or "" when the event is not one the
// campaign records. `opened` and `unique_opened` both count as an open (the
// engaged rule counts opens); `request` is the accepted-for-delivery ack and
// is not recorded.
func EventForWebhook(event string) string {
	switch strings.ToLower(strings.TrimSpace(event)) {
	case "delivered":
		return "email.delivered"
	case "opened", "unique_opened", "proxy_open":
		return "email.opened"
	case "click", "clicked":
		return "email.clicked"
	case "hard_bounce", "blocked", "invalid_email", "error":
		return "email.bounced"
	case "spam", "complaint":
		return "email.complained"
	case "unsubscribed", "unsubscribe":
		return "email.unsubscribed"
	}
	return ""
}

// WebhookExit is the campaign exit a webhook event forces, or "".
func WebhookExit(campaignEvent string) string {
	switch campaignEvent {
	case "email.bounced":
		return ExitBounced
	case "email.complained":
		return ExitComplained
	case "email.unsubscribed":
		return ExitOptOut
	}
	return ""
}
