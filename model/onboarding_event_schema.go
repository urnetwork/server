package model

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode/utf8"
)

// The CLOSED client event schema (mmm/onboarding/PLAN.md "Optimization loop" §1).
//
// Every event the apps may send is listed here with the exact set of prop keys it
// may carry. The server REJECTS unknown names and unknown prop keys, and never
// stores free text except feedback.submitted.text, which the controller passes
// through the analytics redaction before it is stored. Server-written events
// (landing.clicked, app.opened, email.*, trial.*, refund, retention.*) are listed
// too, marked ServerOnly, so a client cannot forge attribution or outcome rows.

// Client event names.
const (
	EventOnboardingStepShown     = "onboarding.step.shown"
	EventOnboardingStepCompleted = "onboarding.step.completed"
	EventOnboardingStepSkipped   = "onboarding.step.skipped"
	EventOfferScreenShown        = "offer.screen.shown"
	EventOfferCardTapped         = "offer.card.tapped"
	EventOfferCtaTapped          = "offer.cta.tapped"
	EventOfferDeclined           = "offer.declined"
	EventPurchaseStarted         = "purchase.started"
	EventPurchaseCompleted       = "purchase.completed"
	EventPurchaseCancelled       = "purchase.cancelled"
	EventPurchaseFailed          = "purchase.failed"
	EventConnectFirst            = "connect.first"
	EventWidgetAdded             = "widget.added"
	EventFeedbackSubmitted       = "feedback.submitted"
	EventSignupOptoutChanged     = "signup.optout_changed"
)

// Server-written event names (never accepted from a client).
const (
	EventLandingClicked = "landing.clicked"
	EventAppOpened      = "app.opened"
	// S2 campaign engine
	EventEmailSent         = "email.sent"
	EventEmailDelivered    = "email.delivered"
	EventEmailOpened       = "email.opened"
	EventEmailClicked      = "email.clicked"
	EventEmailBounced      = "email.bounced"
	EventEmailComplained   = "email.complained"
	EventEmailUnsubscribed = "email.unsubscribed"
	// S3 results
	EventTrialConverted = "trial.converted"
	EventTrialCancelled = "trial.cancelled"
	EventRefund         = "refund"
	EventRetentionD7    = "retention.d7"
	EventRetentionD30   = "retention.d30"
	// EventConnectDay is written by the client connection path once per
	// network per UTC day with at least one connection: the durable
	// connection history the rollup's connect_7d, retention_d7 and
	// retention_d30 read (network_client_connection is pruned 8 h after
	// disconnect and cannot back them).
	EventConnectDay = "connect.day"
)

// Event platforms (the `platform` field of every client event).
var EventPlatforms = []string{"ios", "macos", "android", "web", "windows", "linux"}

// Event owners, for the seams: which stream writes the server-side event.
const (
	EventOwnerClient = "client"
	EventOwnerS1     = "s1"
	EventOwnerS2     = "s2"
	EventOwnerS3     = "s3"
)

type EventPropKind int

const (
	// a short machine token: [A-Za-z0-9_.:+-], at most MaxLen runes
	EventPropToken EventPropKind = iota
	// one of Values
	EventPropEnum
	// an integer in [Min, Max]
	EventPropInt
	// a finite number in [Min, Max]
	EventPropNumber
	EventPropBool
	// free text, at most MaxLen runes; the ONLY kind stored as prose, and only
	// after redaction
	EventPropText
)

type EventPropSpec struct {
	Kind   EventPropKind
	Values []string
	MaxLen int
	Min    float64
	Max    float64
	// Required props must be present
	Required bool
}

type EventSpec struct {
	Name  string
	Props map[string]*EventPropSpec
	// ServerOnly events are written by the server and refused from clients
	ServerOnly bool
	Owner      string
}

const (
	eventTokenMaxLen = 64
	eventTextMaxLen  = 2000
)

func propToken(required bool) *EventPropSpec {
	return &EventPropSpec{Kind: EventPropToken, MaxLen: eventTokenMaxLen, Required: required}
}

func propEnum(required bool, values ...string) *EventPropSpec {
	return &EventPropSpec{Kind: EventPropEnum, Values: values, Required: required}
}

func propInt(required bool, min float64, max float64) *EventPropSpec {
	return &EventPropSpec{Kind: EventPropInt, Min: min, Max: max, Required: required}
}

func propNumber(required bool, min float64, max float64) *EventPropSpec {
	return &EventPropSpec{Kind: EventPropNumber, Min: min, Max: max, Required: required}
}

func propBool(required bool) *EventPropSpec {
	return &EventPropSpec{Kind: EventPropBool, Required: required}
}

func propText(maxLen int) *EventPropSpec {
	return &EventPropSpec{Kind: EventPropText, MaxLen: maxLen}
}

// Shared prop vocabularies.
var (
	EventPlans          = []string{PlanYearly, PlanMonthly}
	EventStores         = []string{OnboardingStoreApple, OnboardingStorePlay, OnboardingStoreStripe, OnboardingStoreSolana}
	EventOfferSurfaces  = []string{OnboardingOfferSurfaceIntroStep, OnboardingOfferSurfaceFinalScreen, OnboardingOfferSurfaceEmailLink, OnboardingOfferSurfaceAccount}
	EventDeclineControl = []string{"free_plan_link", "back", "system_dismiss"}
	EventTiers          = []string{"standard", "regional"}
)

const maxElapsedMs = 7 * 24 * 60 * 60 * 1000

func stepProps() map[string]*EventPropSpec {
	return map[string]*EventPropSpec{
		"step":       propToken(true),
		"index":      propInt(false, 0, 1000),
		"elapsed_ms": propInt(false, 0, maxElapsedMs),
	}
}

func purchaseProps() map[string]*EventPropSpec {
	return map[string]*EventPropSpec{
		"store":       propEnum(true, EventStores...),
		"product":     propToken(false),
		"plan":        propEnum(false, EventPlans...),
		"trial":       propBool(false),
		"price":       propNumber(false, 0, 100000),
		"currency":    &EventPropSpec{Kind: EventPropToken, MaxLen: 3},
		"error_class": propToken(false),
	}
}

func emailProps() map[string]*EventPropSpec {
	return map[string]*EventPropSpec{
		"step":       propToken(true),
		"experiment": propToken(false),
		"variant":    propToken(false),
	}
}

var eventSpecs = func() map[string]*EventSpec {
	specs := []*EventSpec{
		{Name: EventOnboardingStepShown, Props: stepProps(), Owner: EventOwnerClient},
		{Name: EventOnboardingStepCompleted, Props: stepProps(), Owner: EventOwnerClient},
		{Name: EventOnboardingStepSkipped, Props: stepProps(), Owner: EventOwnerClient},
		{Name: EventOfferScreenShown, Owner: EventOwnerClient, Props: map[string]*EventPropSpec{
			"surface":      propEnum(true, EventOfferSurfaces...),
			"experiment":   propToken(false),
			"variant":      propToken(false),
			"tier":         propEnum(false, EventTiers...),
			"price_shown":  propNumber(false, 0, 100000),
			"currency":     &EventPropSpec{Kind: EventPropToken, MaxLen: 3},
			"expires_in_s": propInt(false, 0, 366*24*60*60),
		}},
		{Name: EventOfferCardTapped, Owner: EventOwnerClient, Props: map[string]*EventPropSpec{
			"plan": propEnum(true, EventPlans...),
		}},
		{Name: EventOfferCtaTapped, Owner: EventOwnerClient, Props: map[string]*EventPropSpec{
			"plan":  propEnum(true, EventPlans...),
			"store": propEnum(true, EventStores...),
		}},
		{Name: EventOfferDeclined, Owner: EventOwnerClient, Props: map[string]*EventPropSpec{
			"control":    propEnum(true, EventDeclineControl...),
			"elapsed_ms": propInt(false, 0, maxElapsedMs),
		}},
		{Name: EventPurchaseStarted, Props: purchaseProps(), Owner: EventOwnerClient},
		{Name: EventPurchaseCompleted, Props: purchaseProps(), Owner: EventOwnerClient},
		{Name: EventPurchaseCancelled, Props: purchaseProps(), Owner: EventOwnerClient},
		{Name: EventPurchaseFailed, Props: purchaseProps(), Owner: EventOwnerClient},
		{Name: EventConnectFirst, Props: map[string]*EventPropSpec{}, Owner: EventOwnerClient},
		{Name: EventWidgetAdded, Owner: EventOwnerClient, Props: map[string]*EventPropSpec{
			"kind": propToken(true),
		}},
		{Name: EventFeedbackSubmitted, Owner: EventOwnerClient, Props: map[string]*EventPropSpec{
			"rating":   propInt(false, 1, 5),
			"reason":   propToken(false),
			"has_text": propBool(false),
			// the ONLY free text in the schema; redacted by the controller
			"text": propText(eventTextMaxLen),
		}},
		{Name: EventSignupOptoutChanged, Owner: EventOwnerClient, Props: map[string]*EventPropSpec{
			"product_updates": propBool(true),
		}},

		// server-written
		{Name: EventLandingClicked, ServerOnly: true, Owner: EventOwnerS1, Props: map[string]*EventPropSpec{
			"step": propToken(true),
		}},
		{Name: EventAppOpened, ServerOnly: true, Owner: EventOwnerS1, Props: map[string]*EventPropSpec{
			"step": propToken(true),
		}},
		{Name: EventEmailSent, ServerOnly: true, Owner: EventOwnerS2, Props: emailProps()},
		{Name: EventEmailDelivered, ServerOnly: true, Owner: EventOwnerS2, Props: emailProps()},
		{Name: EventEmailOpened, ServerOnly: true, Owner: EventOwnerS2, Props: emailProps()},
		{Name: EventEmailClicked, ServerOnly: true, Owner: EventOwnerS2, Props: emailProps()},
		{Name: EventEmailBounced, ServerOnly: true, Owner: EventOwnerS2, Props: emailProps()},
		{Name: EventEmailComplained, ServerOnly: true, Owner: EventOwnerS2, Props: emailProps()},
		{Name: EventEmailUnsubscribed, ServerOnly: true, Owner: EventOwnerS2, Props: emailProps()},
		{Name: EventTrialConverted, ServerOnly: true, Owner: EventOwnerS3, Props: map[string]*EventPropSpec{
			"store": propEnum(false, EventStores...),
			"plan":  propEnum(false, EventPlans...),
		}},
		{Name: EventTrialCancelled, ServerOnly: true, Owner: EventOwnerS3, Props: map[string]*EventPropSpec{
			"store": propEnum(false, EventStores...),
			"plan":  propEnum(false, EventPlans...),
		}},
		{Name: EventRefund, ServerOnly: true, Owner: EventOwnerS3, Props: map[string]*EventPropSpec{
			"store":  propEnum(false, EventStores...),
			"amount": propNumber(false, 0, 100000),
		}},
		{Name: EventRetentionD7, ServerOnly: true, Owner: EventOwnerS3, Props: map[string]*EventPropSpec{
			"connect_days": propInt(false, 0, 7),
		}},
		{Name: EventConnectDay, ServerOnly: true, Owner: EventOwnerS3, Props: map[string]*EventPropSpec{}},
		{Name: EventRetentionD30, ServerOnly: true, Owner: EventOwnerS3, Props: map[string]*EventPropSpec{
			"connect_days": propInt(false, 0, 30),
		}},
	}
	m := map[string]*EventSpec{}
	for _, spec := range specs {
		m[spec.Name] = spec
	}
	return m
}()

// EventSpecFor returns the schema entry for an event name, or nil.
func EventSpecFor(name string) *EventSpec {
	return eventSpecs[name]
}

// ClientEventNames lists the names a client may send, sorted.
func ClientEventNames() []string {
	names := []string{}
	for name, spec := range eventSpecs {
		if !spec.ServerOnly {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// ServerEventNames lists the server-written names, sorted.
func ServerEventNames() []string {
	names := []string{}
	for name, spec := range eventSpecs {
		if spec.ServerOnly {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// EventValidationError is a schema refusal for one event. It is a client fault,
// reported per event in the batch response rather than failing the request.
type EventValidationError struct {
	Message string
}

func (self *EventValidationError) Error() string {
	return self.Message
}

func eventInvalid(format string, args ...any) error {
	return &EventValidationError{Message: fmt.Sprintf(format, args...)}
}

// IsEventToken reports whether a value is a short machine token: ASCII letters,
// digits and _ . : + - only, non-empty, at most maxLen runes.
func IsEventToken(value string, maxLen int) bool {
	if value == "" || maxLen < len(value) {
		return false
	}
	for _, c := range value {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case c == '_', c == '.', c == ':', c == '+', c == '-':
		default:
			return false
		}
	}
	return true
}

// ValidateClientEvent checks an event a CLIENT sent against the closed schema and
// returns the normalized props (ints as int64, numbers as float64, bools, tokens
// and enums as strings, text as a trimmed string). Unknown names, server-only
// names, unknown prop keys, missing required props and ill-typed values are all
// refused with an *EventValidationError.
func ValidateClientEvent(name string, props map[string]any) (map[string]any, error) {
	spec := eventSpecs[name]
	if spec == nil {
		return nil, eventInvalid("unknown event name %q", name)
	}
	if spec.ServerOnly {
		return nil, eventInvalid("event %q is written by the server", name)
	}
	return validateEventProps(spec, props)
}

// ValidateServerEvent is the same check for an event the server writes itself; it
// accepts every schema entry.
func ValidateServerEvent(name string, props map[string]any) (map[string]any, error) {
	spec := eventSpecs[name]
	if spec == nil {
		return nil, eventInvalid("unknown event name %q", name)
	}
	return validateEventProps(spec, props)
}

func validateEventProps(spec *EventSpec, props map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for key, value := range props {
		propSpec, ok := spec.Props[key]
		if !ok {
			return nil, eventInvalid("event %q: unknown prop %q", spec.Name, key)
		}
		if value == nil {
			continue
		}
		normalized, err := propSpec.normalize(value)
		if err != nil {
			return nil, eventInvalid("event %q: prop %q %s", spec.Name, key, err.Error())
		}
		out[key] = normalized
	}
	for key, propSpec := range spec.Props {
		if propSpec.Required {
			if _, ok := out[key]; !ok {
				return nil, eventInvalid("event %q: missing prop %q", spec.Name, key)
			}
		}
	}
	return out, nil
}

func (self *EventPropSpec) normalize(value any) (any, error) {
	switch self.Kind {
	case EventPropToken:
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("must be a string")
		}
		s = strings.TrimSpace(s)
		maxLen := self.MaxLen
		if maxLen <= 0 {
			maxLen = eventTokenMaxLen
		}
		if !IsEventToken(s, maxLen) {
			return nil, fmt.Errorf("must be a token of at most %d characters", maxLen)
		}
		return s, nil
	case EventPropEnum:
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("must be a string")
		}
		s = strings.TrimSpace(s)
		for _, allowed := range self.Values {
			if s == allowed {
				return s, nil
			}
		}
		return nil, fmt.Errorf("must be one of %s", strings.Join(self.Values, "|"))
	case EventPropInt:
		f, ok := numberValue(value)
		if !ok || f != math.Trunc(f) {
			return nil, fmt.Errorf("must be an integer")
		}
		if f < self.Min || self.Max < f {
			return nil, fmt.Errorf("must be in [%d, %d]", int64(self.Min), int64(self.Max))
		}
		return int64(f), nil
	case EventPropNumber:
		f, ok := numberValue(value)
		if !ok {
			return nil, fmt.Errorf("must be a number")
		}
		if f < self.Min || self.Max < f {
			return nil, fmt.Errorf("must be in [%g, %g]", self.Min, self.Max)
		}
		return f, nil
	case EventPropBool:
		b, ok := value.(bool)
		if !ok {
			return nil, fmt.Errorf("must be a boolean")
		}
		return b, nil
	case EventPropText:
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("must be a string")
		}
		if !utf8.ValidString(s) {
			return nil, fmt.Errorf("must be valid utf-8")
		}
		s = strings.TrimSpace(s)
		if self.MaxLen < utf8.RuneCountInString(s) {
			return nil, fmt.Errorf("must be at most %d characters", self.MaxLen)
		}
		return s, nil
	default:
		return nil, fmt.Errorf("has no schema kind")
	}
}

func numberValue(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false
		}
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int32:
		return float64(v), true
	case int64:
		return float64(v), true
	default:
		return 0, false
	}
}

// EventTextPropKeys lists the free-text props of an event (the ones the
// controller must redact before storing); today only feedback.submitted.text.
func EventTextPropKeys(name string) []string {
	spec := eventSpecs[name]
	if spec == nil {
		return nil
	}
	keys := []string{}
	for key, propSpec := range spec.Props {
		if propSpec.Kind == EventPropText {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}
