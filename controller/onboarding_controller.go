package controller

import (
	"fmt"
	"math"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/onboarding"
	"github.com/urnetwork/server/session"
)

// The onboarding program's api surface (mmm/onboarding/PLAN.md): the regional
// price tier on the plan response, the welcome offer, the closed client event
// endpoint, the experiment assignments, and the landing/feedback token endpoints.
// The Stripe half (payment sheet, prices, coupon, webhooks) is in
// onboarding_stripe_controller.go.

type OnboardingError struct {
	Message string `json:"message"`
}

// ----- price tier -----

// How the plan response resolved the country behind the price tier. storefront
// and billing are what the store or the card will charge; ip and default are
// DISPLAY ESTIMATES only -- the charged tier on Stripe is finalized from the
// card's billing country, and the stores price by their own storefront.
const (
	PriceTierSourceStorefront = "storefront"
	PriceTierSourceBilling    = "billing"
	PriceTierSourceIp         = "ip"
	PriceTierSourceDefault    = "default"
)

type PriceTierResult struct {
	Name       string  `json:"name"`
	YearlyUsd  float64 `json:"yearly_usd"`
	MonthlyUsd float64 `json:"monthly_usd"`
	Currency   string  `json:"currency"`
	// Source is one of storefront | billing | ip | default
	Source string `json:"source"`
	// Estimate is true when the tier is a display estimate (ip or default)
	Estimate bool `json:"estimate"`
}

type ResolvedPriceTier struct {
	Tier        *model.ProPriceTier
	Source      string
	CountryCode string
}

func (self *ResolvedPriceTier) Result() *PriceTierResult {
	return &PriceTierResult{
		Name:       self.Tier.Name,
		YearlyUsd:  self.Tier.YearlyUsd,
		MonthlyUsd: self.Tier.MonthlyUsd,
		Currency:   model.PriceTierCurrency,
		Source:     self.Source,
		Estimate:   self.Source == PriceTierSourceIp || self.Source == PriceTierSourceDefault,
	}
}

// ResolvePriceTier resolves the caller's price tier in the documented order: the
// store's storefront country when the app sent one, the Stripe customer's
// billing country when a customer exists, else the client ip as an estimate,
// else the default tier.
func ResolvePriceTier(clientSession *session.ClientSession, storefrontCountry string) *ResolvedPriceTier {
	pro := model.Pro()
	if code := model.CountryCodeFromStorefront(storefrontCountry); code != "" {
		return &ResolvedPriceTier{Tier: pro.PriceTierForCountry(code), Source: PriceTierSourceStorefront, CountryCode: code}
	}
	if clientSession.ByJwt != nil {
		if code := model.NormalizeCountryCode(model.GetStripeCustomerBillingCountry(clientSession.Ctx, clientSession.ByJwt.NetworkId)); code != "" {
			return &ResolvedPriceTier{Tier: pro.PriceTierForCountry(code), Source: PriceTierSourceBilling, CountryCode: code}
		}
	}
	if code := clientCountryCode(clientSession); code != "" {
		return &ResolvedPriceTier{Tier: pro.PriceTierForCountry(code), Source: PriceTierSourceIp, CountryCode: code}
	}
	return &ResolvedPriceTier{Tier: pro.DefaultPriceTier(), Source: PriceTierSourceDefault}
}

// clientCountryCode is the ip database's country for the session's address, ""
// when the address or the database is unavailable. Never fails the request: a
// missing ip database (local) just means an unknown country.
func clientCountryCode(clientSession *session.ClientSession) (code string) {
	defer func() {
		if r := recover(); r != nil {
			code = ""
		}
	}()
	ip, _, err := clientSession.ClientIpPort()
	if err != nil {
		return ""
	}
	ipInfo, err := server.GetIpInfoFromString(ip)
	if err != nil || ipInfo == nil {
		return ""
	}
	return model.NormalizeCountryCode(ipInfo.CountryCode)
}

// ----- the welcome offer -----

type OnboardingOfferResult struct {
	IssuedAt   time.Time `json:"issued_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	PercentOff int       `json:"percent_off"`
	MonthsFree int       `json:"months_free"`
	// the first year at the offer, and the regular yearly price, for the caller's
	// current price tier
	FirstYearUsd   float64 `json:"first_year_usd"`
	RegularYearUsd float64 `json:"regular_year_usd"`
	Tier           string  `json:"tier"`
	Currency       string  `json:"currency"`
	// active | redeemed | expired
	State          string     `json:"state"`
	AppleOfferCode string     `json:"apple_offer_code,omitempty"`
	PlayOfferTag   string     `json:"play_offer_tag,omitempty"`
	StripeCouponId string     `json:"stripe_coupon_id,omitempty"`
	RedeemedAt     *time.Time `json:"redeemed_at,omitempty"`
	Store          string     `json:"store,omitempty"`
}

func onboardingOfferResult(offer *model.OnboardingOffer, tier *model.ProPriceTier, now time.Time) *OnboardingOfferResult {
	if offer == nil {
		return nil
	}
	regular := tier.YearlyUsd
	first := math.Round(regular*float64(100-offer.PercentOff)) / 100
	result := &OnboardingOfferResult{
		IssuedAt:       offer.IssuedAt,
		ExpiresAt:      offer.ExpiresAt,
		PercentOff:     offer.PercentOff,
		MonthsFree:     offer.MonthsFree,
		FirstYearUsd:   first,
		RegularYearUsd: regular,
		Tier:           tier.Name,
		Currency:       model.PriceTierCurrency,
		State:          model.OnboardingOfferState(offer, now),
		RedeemedAt:     offer.RedeemedAt,
	}
	if offer.AppleOfferCode != nil {
		result.AppleOfferCode = *offer.AppleOfferCode
	}
	if offer.PlayOfferTag != nil {
		result.PlayOfferTag = *offer.PlayOfferTag
	}
	if offer.StripeCouponId != nil {
		result.StripeCouponId = *offer.StripeCouponId
	}
	if offer.Store != nil {
		result.Store = *offer.Store
	}
	return result
}

type OnboardingOfferIssueArgs struct {
	// where the offer is being shown: intro_step | final_screen | account
	Surface string `json:"surface"`
	// the store's storefront country, when the app knows it
	StorefrontCountry string `json:"storefront_country,omitempty"`
}

type OnboardingOfferIssueResult struct {
	Offer *OnboardingOfferResult `json:"offer,omitempty"`
	// Created is true for the call that issued the offer; false when it already existed
	Created bool             `json:"created"`
	Error   *OnboardingError `json:"error,omitempty"`
}

func onboardingOfferError(message string) *OnboardingOfferIssueResult {
	return &OnboardingOfferIssueResult{Error: &OnboardingError{Message: message}}
}

// OnboardingOfferIssue issues the caller's welcome offer once. Idempotent: a
// second call returns the existing offer in whatever state it is in, and an
// expired offer is never re-issued. Refused for networks the in-app offer
// experiment holds out.
func OnboardingOfferIssue(
	args *OnboardingOfferIssueArgs,
	clientSession *session.ClientSession,
) (*OnboardingOfferIssueResult, error) {
	if err := model.CheckOnboardingRateLimit(clientSession, model.OnboardingOfferRateLimit); err != nil {
		return nil, err
	}
	cfg := model.Onboarding()
	if !cfg.OfferEnabled() {
		return onboardingOfferError("The welcome offer is not available."), nil
	}
	surface := strings.TrimSpace(args.Surface)
	switch surface {
	case model.OnboardingOfferSurfaceIntroStep, model.OnboardingOfferSurfaceFinalScreen, model.OnboardingOfferSurfaceAccount:
	case "":
		surface = model.OnboardingOfferSurfaceIntroStep
	default:
		return onboardingOfferError("Unknown surface."), nil
	}
	networkId := clientSession.ByJwt.NetworkId
	now := server.NowUtc()

	if existing := model.GetOnboardingOffer(clientSession.Ctx, networkId); existing != nil {
		tier := ResolvePriceTier(clientSession, args.StorefrontCountry)
		return &OnboardingOfferIssueResult{Offer: onboardingOfferResult(existing, tier.Tier, now)}, nil
	}

	// the in-app holdout never gets an in-app offer; it reaches the email offer
	// on path B
	if a := cfg.AssignmentForSurface(networkId, model.ExperimentSurfaceOfferIntroStep, now); a != nil && a.Variant == model.ExperimentVariantHoldout {
		return onboardingOfferError("The welcome offer is not available."), nil
	}

	tier := ResolvePriceTier(clientSession, args.StorefrontCountry)
	ensureAppleOfferCodePoolLoaded(clientSession)

	offer, created, err := model.IssueOnboardingOffer(clientSession.Ctx, &model.IssueOnboardingOfferArgs{
		NetworkId:          networkId,
		IssuedBy:           model.OnboardingOfferIssuedByInApp,
		Surface:            surface,
		Tier:               tier.Tier.Name,
		PercentOff:         cfg.Offer.PercentOff,
		MonthsFree:         cfg.Offer.MonthsFree,
		Validity:           cfg.OfferValidity(),
		StripeCouponId:     cfg.Offer.StripeCouponId,
		PlayOfferTag:       cfg.Offer.PlayOfferTag,
		AppleOfferCode:     cfg.Offer.AppleCustomOfferCode,
		AppleOfferCodePool: true,
	})
	if err != nil {
		glog.Errorf("[onboarding]could not issue offer for network %s: %s\n", networkId, err)
		return onboardingOfferError("Could not issue the offer. Please try again."), nil
	}
	return &OnboardingOfferIssueResult{
		Offer:   onboardingOfferResult(offer, tier.Tier, now),
		Created: created,
	}, nil
}

// IssueOnboardingOfferByEmail is the campaign engine's (S2) issue path for step
// E4b: same terms, issued_by email. Returns the offer (existing or new).
func IssueOnboardingOfferByEmail(clientSession *session.ClientSession, networkId server.Id, tierName string) (*model.OnboardingOffer, bool, error) {
	cfg := model.Onboarding()
	if !cfg.OfferEnabled() {
		return nil, false, fmt.Errorf("the welcome offer is not enabled")
	}
	ensureAppleOfferCodePoolLoaded(clientSession)
	return model.IssueOnboardingOffer(clientSession.Ctx, &model.IssueOnboardingOfferArgs{
		NetworkId:          networkId,
		IssuedBy:           model.OnboardingOfferIssuedByEmail,
		Surface:            model.OnboardingOfferSurfaceEmailLink,
		Tier:               tierName,
		PercentOff:         cfg.Offer.PercentOff,
		MonthsFree:         cfg.Offer.MonthsFree,
		Validity:           cfg.OfferValidity(),
		StripeCouponId:     cfg.Offer.StripeCouponId,
		PlayOfferTag:       cfg.Offer.PlayOfferTag,
		AppleOfferCode:     cfg.Offer.AppleCustomOfferCode,
		AppleOfferCodePool: true,
	})
}

// EligibleOnboardingOffer is the network's offer when it can be redeemed now,
// else nil. Every purchase path asks this.
func EligibleOnboardingOffer(clientSession *session.ClientSession, networkId server.Id) *model.OnboardingOffer {
	offer := model.GetOnboardingOffer(clientSession.Ctx, networkId)
	if offer != nil && offer.Eligible(server.NowUtc()) {
		return offer
	}
	return nil
}

var appleOfferCodePoolLoad sync.Once

// ensureAppleOfferCodePoolLoaded loads the configured App Store one-time code
// batch (onboarding.yml offer.apple_offer_codes_path) into the pool table once per
// process. Idempotent against the table, so every server can do it. SEAM: App
// Store Connect batch generation (S2) inserts into the same pool through
// model.LoadAppleOfferCodePool.
func ensureAppleOfferCodePoolLoaded(clientSession *session.ClientSession) {
	appleOfferCodePoolLoad.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				glog.Errorf("[onboarding]apple offer code pool load failed: %v\n", r)
			}
		}()
		path := strings.TrimSpace(model.Onboarding().Offer.AppleOfferCodesPath)
		if path == "" {
			return
		}
		paths, err := server.Config.ResourcePaths(path)
		if err != nil || len(paths) == 0 {
			glog.Errorf("[onboarding]apple offer codes %q not found: %v\n", path, err)
			return
		}
		data, err := os.ReadFile(paths[0])
		if err != nil {
			glog.Errorf("[onboarding]apple offer codes %q could not be read: %s\n", path, err)
			return
		}
		codes, err := model.ParseAppleOfferCodeCsv(string(data))
		if err != nil {
			glog.Errorf("[onboarding]apple offer codes %q could not be parsed: %s\n", path, err)
			return
		}
		added, err := model.LoadAppleOfferCodePool(clientSession.Ctx, codes)
		if err != nil {
			glog.Errorf("[onboarding]apple offer code pool load failed: %s\n", err)
			return
		}
		glog.Infof("[onboarding]apple offer code pool: %d codes in batch, %d new\n", len(codes), added)
	})
}

// ----- the plan response -----

// SubscriptionBalanceForStorefront is SubscriptionBalance plus the onboarding plan
// fields: the price tier, the welcome offer and the experiment assignments.
// storefrontCountry is the store's storefront country when the app knows it.
func SubscriptionBalanceForStorefront(storefrontCountry string, clientSession *session.ClientSession) (*SubscriptionBalanceResult, error) {
	result, err := SubscriptionBalance(clientSession)
	if err != nil {
		return nil, err
	}
	DecoratePlan(result, storefrontCountry, clientSession)
	return result, nil
}

// DecoratePlan fills the onboarding plan fields on a balance result.
func DecoratePlan(result *SubscriptionBalanceResult, storefrontCountry string, clientSession *session.ClientSession) {
	now := server.NowUtc()
	networkId := clientSession.ByJwt.NetworkId
	tier := ResolvePriceTier(clientSession, storefrontCountry)
	result.PriceTier = tier.Result()
	result.OnboardingOffer = onboardingOfferResult(model.GetOnboardingOffer(clientSession.Ctx, networkId), tier.Tier, now)
	result.Experiments = model.Onboarding().AssignExperiments(networkId, now)
}

// ----- client events -----

// MaxClientEventsPerCall bounds one POST /client/events batch.
const MaxClientEventsPerCall = 200

// clientEventPastClamp / clientEventFutureClamp bound a client clock: an event
// older than the past clamp is moved to it, one in the future is moved to now.
const (
	clientEventPastClamp   = 30 * 24 * time.Hour
	clientEventFutureClamp = 5 * time.Minute
)

type ClientEvent struct {
	Name string `json:"name"`
	// At is the client's event time (RFC 3339); missing = now
	At         *time.Time     `json:"at,omitempty"`
	Platform   string         `json:"platform"`
	AppVersion string         `json:"app_version,omitempty"`
	Locale     string         `json:"locale,omitempty"`
	Session    string         `json:"session,omitempty"`
	Props      map[string]any `json:"props,omitempty"`
}

type ClientEventsSendArgs struct {
	Events []*ClientEvent `json:"events"`
}

type ClientEventRejection struct {
	Index   int    `json:"index"`
	Message string `json:"message"`
}

type ClientEventsSendResult struct {
	// Accepted counts the events stored (or deduplicated, for once-per-network
	// events); Rejected lists the schema refusals by batch index. A rejection is
	// final: the client must not resend the event.
	Accepted int                     `json:"accepted"`
	Rejected []*ClientEventRejection `json:"rejected,omitempty"`
}

// ClientEventsSend stores a batch of client events after checking each one
// against the closed schema. The network id comes from the jwt only; tier, path,
// experiment and variant are stamped by the server.
func ClientEventsSend(
	args *ClientEventsSendArgs,
	clientSession *session.ClientSession,
) (*ClientEventsSendResult, error) {
	if MaxClientEventsPerCall < len(args.Events) {
		return nil, fmt.Errorf("400 At most %d events per call.", MaxClientEventsPerCall)
	}
	if err := model.CheckOnboardingRateLimit(clientSession, model.ClientEventsRateLimit); err != nil {
		return nil, err
	}
	result := &ClientEventsSendResult{Rejected: []*ClientEventRejection{}}
	if len(args.Events) == 0 {
		return result, nil
	}

	networkId := clientSession.ByJwt.NetworkId
	now := server.NowUtc()
	cfg := model.Onboarding()
	// stamped once per batch: the tier estimate and the campaign path
	tier := ResolvePriceTier(clientSession, "")
	path := model.OnboardingPathForNetwork(model.GetOnboardingOffer(clientSession.Ctx, networkId))
	assignments := cfg.AssignExperiments(networkId, now)

	connectFirstSeen := false
	events := []*model.OnboardingEvent{}
	for i, clientEvent := range args.Events {
		if clientEvent == nil {
			result.Rejected = append(result.Rejected, &ClientEventRejection{Index: i, Message: "missing event"})
			continue
		}
		event, err := normalizeClientEvent(clientEvent, now)
		if err != nil {
			result.Rejected = append(result.Rejected, &ClientEventRejection{Index: i, Message: err.Error()})
			continue
		}
		event.NetworkId = networkId
		event.ReceivedAt = now
		event.Tier = tier.Tier.Name
		event.Path = path
		if a := clientEventAssignment(assignments, event); a != nil {
			event.Experiment = a.ExperimentId
			event.Variant = a.Variant
		}
		if event.Name == model.EventConnectFirst {
			// once per network: the client may resend it after a reinstall
			if connectFirstSeen || model.HasOnboardingEvent(clientSession.Ctx, networkId, model.EventConnectFirst) {
				result.Accepted += 1
				continue
			}
			connectFirstSeen = true
		}
		events = append(events, event)
		result.Accepted += 1
	}

	if err := model.AddOnboardingEvents(clientSession.Ctx, events); err != nil {
		glog.Errorf("[onboarding]could not store %d events for network %s: %s\n", len(events), networkId, err)
		return nil, fmt.Errorf("Could not store events.")
	}
	return result, nil
}

// normalizeClientEvent validates one client event against the schema and returns
// the row to store (without the server-stamped fields).
func normalizeClientEvent(clientEvent *ClientEvent, now time.Time) (*model.OnboardingEvent, error) {
	props, err := model.ValidateClientEvent(clientEvent.Name, clientEvent.Props)
	if err != nil {
		return nil, err
	}
	platform := strings.ToLower(strings.TrimSpace(clientEvent.Platform))
	platformOk := false
	for _, allowed := range model.EventPlatforms {
		if platform == allowed {
			platformOk = true
			break
		}
	}
	if !platformOk {
		return nil, fmt.Errorf("platform must be one of %s", strings.Join(model.EventPlatforms, "|"))
	}
	appVersion := strings.TrimSpace(clientEvent.AppVersion)
	if appVersion != "" && !model.IsEventToken(appVersion, 64) {
		return nil, fmt.Errorf("app_version must be a token of at most 64 characters")
	}
	locale := strings.TrimSpace(clientEvent.Locale)
	if locale != "" && !model.IsEventToken(locale, 32) {
		return nil, fmt.Errorf("locale must be a token of at most 32 characters")
	}
	sessionId := strings.TrimSpace(clientEvent.Session)
	if sessionId != "" && !model.IsEventToken(sessionId, 64) {
		return nil, fmt.Errorf("session must be a token of at most 64 characters")
	}
	at := now
	if clientEvent.At != nil {
		at = clientEvent.At.UTC()
		if at.After(now.Add(clientEventFutureClamp)) {
			at = now
		}
		if at.Before(now.Add(-clientEventPastClamp)) {
			at = now.Add(-clientEventPastClamp)
		}
	}
	// the only prose in the schema goes through the analytics redaction
	for _, key := range model.EventTextPropKeys(clientEvent.Name) {
		if value, ok := props[key].(string); ok {
			redacted, keep := redactEventText(value)
			if keep {
				props[key] = redacted
			} else {
				delete(props, key)
			}
		}
	}
	return &model.OnboardingEvent{
		Name:       clientEvent.Name,
		At:         at,
		Platform:   platform,
		AppVersion: appVersion,
		Locale:     locale,
		Session:    sessionId,
		Props:      props,
	}, nil
}

// clientEventAssignment picks the experiment an event is an exposure of: an
// offer.screen.shown event names its surface; the other offer.* events belong to
// the in-app offer experiment; everything else carries no experiment.
func clientEventAssignment(assignments map[string]*model.ExperimentAssignment, event *model.OnboardingEvent) *model.ExperimentAssignment {
	if !strings.HasPrefix(event.Name, "offer.") {
		return nil
	}
	surface := model.ExperimentSurfaceOfferInApp
	if event.Name == model.EventOfferScreenShown {
		if s, ok := event.Props["surface"].(string); ok && s != "" {
			surface = "offer." + s
		}
	}
	for _, candidate := range model.ExperimentSurfaceFallbacks(surface) {
		if a, ok := assignments[candidate]; ok {
			return a
		}
	}
	return nil
}

// maxEventTextRunes bounds stored feedback text.
const maxEventTextRunes = 2000

// redactEventText applies the analytics privacy rules (email, ip and phone
// redaction, whitespace normalization, a length cap) to the one free-text prop.
// Unlike the search-console path it always redacts: feedback text is prose a
// person typed, and it is stored per network.
func redactEventText(value string) (string, bool) {
	value = normalizeSpaces(strings.TrimSpace(value))
	if value == "" {
		return "", false
	}
	value = emailPattern.ReplaceAllString(value, "[redacted-email]")
	value = ipTokenPattern.ReplaceAllStringFunc(value, func(tokenValue string) string {
		trimmed := strings.Trim(tokenValue, "[](){}.,;")
		if net.ParseIP(trimmed) != nil {
			return strings.Replace(tokenValue, trimmed, "[redacted-ip]", 1)
		}
		return tokenValue
	})
	value = phonePattern.ReplaceAllString(value, "[redacted-phone]")
	value = truncateRunes(value, maxEventTextRunes)
	return value, value != ""
}

// ----- server-written events -----

// WriteServerEvent stores an event the server itself observed (landing.clicked,
// app.opened, signup.optout_changed at sign-up, ...). The name must be in the
// schema. Never fails the caller: an event that cannot be stored is logged.
func WriteServerEvent(clientSession *session.ClientSession, networkId server.Id, name string, props map[string]any, platform string) bool {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[onboarding]could not write %s for network %s: %v\n", name, networkId, r)
		}
	}()
	validated, err := model.ValidateServerEvent(name, props)
	if err != nil {
		glog.Errorf("[onboarding]refusing server event: %s\n", err)
		return false
	}
	now := server.NowUtc()
	event := &model.OnboardingEvent{
		NetworkId:  networkId,
		Name:       name,
		At:         now,
		ReceivedAt: now,
		Platform:   platform,
		Path:       model.OnboardingPathForNetwork(model.GetOnboardingOffer(clientSession.Ctx, networkId)),
		Props:      validated,
	}
	if clientSession != nil {
		if code := clientCountryCode(clientSession); code != "" {
			event.Tier = model.Pro().PriceTierForCountry(code).Name
		}
	}
	if step, ok := validated["step"].(string); ok && step != "" {
		if a := model.Onboarding().AssignmentForSurface(networkId, "email."+step, now); a != nil {
			event.Experiment = a.ExperimentId
			event.Variant = a.Variant
		}
	}
	if err := model.AddOnboardingEvent(clientSession.Ctx, event); err != nil {
		glog.Errorf("[onboarding]could not write %s for network %s: %s\n", name, networkId, err)
		return false
	}
	return true
}

// AttributeAppOpen writes app.opened for the network when a landing click
// preceded this device authentication within the attribution window. Runs in the
// auth-client path, so it is one cheap query and never fails the caller.
func AttributeAppOpen(clientSession *session.ClientSession, networkId server.Id) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[onboarding]app open attribution failed for network %s: %v\n", networkId, r)
		}
	}()
	model.AttributeAppOpen(clientSession.Ctx, networkId, server.NowUtc())
}

// ----- landing click and feedback tokens -----

type OnboardingClickArgs struct {
	Token string `json:"token"`
}

type OnboardingClickResult struct {
	Ok bool `json:"ok"`
	// Step and Destination are returned for every token whose signature verifies
	// (expired ones too), so the landing page can still route the user
	Step        string `json:"step,omitempty"`
	Destination string `json:"destination,omitempty"`
	// invalid | expired
	Error string `json:"error,omitempty"`
}

// OnboardingClick records a landing-page click (the campaign's attribution event)
// and tells the page where in the app to send the user. No auth: the token is the
// credential, and the response carries nothing about the network.
func OnboardingClick(
	args *OnboardingClickArgs,
	clientSession *session.ClientSession,
) (*OnboardingClickResult, error) {
	if err := model.CheckOnboardingRateLimit(clientSession, model.OnboardingTokenRateLimit); err != nil {
		return nil, err
	}
	now := server.NowUtc()
	claims, err := onboarding.Parse(args.Token, now)
	if err == onboarding.ErrTokenExpired && claims != nil {
		return &OnboardingClickResult{
			Ok:          false,
			Step:        claims.Step,
			Destination: onboarding.Destination(claims.Step),
			Error:       "expired",
		}, nil
	}
	if err != nil || claims == nil {
		return &OnboardingClickResult{Ok: false, Error: "invalid"}, nil
	}
	WriteServerEvent(clientSession, claims.NetworkId, model.EventLandingClicked, map[string]any{
		"step": claims.Step,
	}, "")
	return &OnboardingClickResult{
		Ok:          true,
		Step:        claims.Step,
		Destination: onboarding.Destination(claims.Step),
	}, nil
}

type OnboardingFeedbackTokenResult struct {
	Ok     bool   `json:"ok"`
	Step   string `json:"step,omitempty"`
	Rating int    `json:"rating,omitempty"`
	Reason string `json:"reason,omitempty"`
	// invalid | expired
	Error string `json:"error,omitempty"`
}

// OnboardingFeedbackToken resolves a feedback link token (ur.io/f/<token>?r=n or
// ?why=n) to the rating or reason the one-tap button stood for, so the in-app
// feedback screen opens pre-filled. The token carries the step and may carry the
// rating/reason; the query values override when present and valid.
func OnboardingFeedbackToken(
	token string,
	ratingQuery string,
	reasonQuery string,
	clientSession *session.ClientSession,
) (*OnboardingFeedbackTokenResult, error) {
	if err := model.CheckOnboardingRateLimit(clientSession, model.OnboardingTokenRateLimit); err != nil {
		return nil, err
	}
	claims, err := onboarding.Parse(token, server.NowUtc())
	if err == onboarding.ErrTokenExpired {
		return &OnboardingFeedbackTokenResult{Ok: false, Error: "expired"}, nil
	}
	if err != nil || claims == nil {
		return &OnboardingFeedbackTokenResult{Ok: false, Error: "invalid"}, nil
	}
	result := &OnboardingFeedbackTokenResult{
		Ok:     true,
		Step:   claims.Step,
		Rating: claims.Rating,
		Reason: claims.Reason,
	}
	if ratingQuery != "" {
		var rating int
		if _, err := fmt.Sscanf(strings.TrimSpace(ratingQuery), "%d", &rating); err == nil && 1 <= rating && rating <= 5 {
			result.Rating = rating
		}
	}
	if reason := strings.TrimSpace(reasonQuery); reason != "" && model.IsEventToken(reason, 64) {
		result.Reason = reason
	}
	if result.Rating < 1 || 5 < result.Rating {
		result.Rating = 0
	}
	return result, nil
}

// ----- sign-up opt-out -----

// ProductUpdatesFromCreateArgs is the product-updates preference a sign-up asked
// for: the explicit flag when sent, else on (the sign-up forms show the line with
// the box ticked).
func ProductUpdatesFromCreateArgs(networkCreate *model.NetworkCreateArgs) bool {
	if networkCreate.ProductUpdates == nil {
		return true
	}
	return *networkCreate.ProductUpdates
}
