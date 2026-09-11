// SPDX-License-Identifier: MPL-2.0

package controller

// The onboarding email campaign engine (mmm/onboarding/PLAN.md "THE EMAIL
// SEQUENCE" and "ARCHITECTURE"): server-driven sends through Brevo's
// transactional API, one scheduled task per network and step, each re-evaluated
// at send time. The decisions are pure (server/onboarding); this file loads the
// facts, talks to Brevo and the task scheduler, and records what happened.
//
// SAFETY: every outbound side effect is behind onboarding.yml `enabled`
// (default false). While disabled the engine keeps its rows and schedule so a
// later enable never replays old steps, and logs each send it would have made.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/onboarding"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// ----- metrics -----

var onboardingStartsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "onboarding",
	Name:      "starts_total",
	Help:      "Networks entering the onboarding campaign, by whether they can receive email and their sequence variant.",
}, []string{"email", "variant"})

var onboardingEmailsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "onboarding",
	Name:      "emails_total",
	Help:      "Campaign step outcomes: sent, skipped (condition not met), dry_run (engine disabled), failed (Brevo error).",
}, []string{"step", "template", "variant", "result"})

var onboardingExitsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "onboarding",
	Name:      "exits_total",
	Help:      "Networks leaving the campaign, by reason.",
}, []string{"reason"})

var onboardingWebhookEventsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "onboarding",
	Name:      "webhook_events_total",
	Help:      "Brevo transactional webhook events attributed to a campaign send, by campaign event and step template.",
}, []string{"event", "template"})

var onboardingAppleOfferCodesAvailable = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "onboarding",
	Name:      "apple_offer_codes_available",
	Help:      "Unexpired, unassigned App Store one-time offer codes in the pool (set by the daily top-up task).",
})

var onboardingAppleOfferCodeBatchesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "onboarding",
	Name:      "apple_offer_code_batches_total",
	Help:      "App Store Connect one-time code batch attempts, by result (created, skipped, failed).",
}, []string{"result"})

func init() {
	prometheus.MustRegister(
		onboardingStartsTotal,
		onboardingEmailsTotal,
		onboardingExitsTotal,
		onboardingWebhookEventsTotal,
		onboardingAppleOfferCodesAvailable,
		onboardingAppleOfferCodeBatchesTotal,
	)
}

// ----- config -----

// OnboardingCampaignEnabled is onboarding.yml `enabled`: the master switch of
// every outbound side effect.
func OnboardingCampaignEnabled() bool {
	return model.Onboarding().Enabled
}

func onboardingSchedule() onboarding.Schedule {
	cfg := model.Onboarding().Schedule
	s := onboarding.Schedule{
		E1Day: cfg.E1Day, E2Day: cfg.E2Day, E3aDay: cfg.E3aDay, E4aDay: cfg.E4aDay,
		E3bDay: cfg.E3bDay, E4bDay: cfg.E4bDay, E5bDay: cfg.E5bDay,
		SendWindow: cfg.SendWindow,
	}
	if s.E1Day <= 0 && s.E2Day <= 0 {
		// no schedule block: the plan's table
		s = onboarding.DefaultSchedule
	}
	return s
}

func onboardingSendWindow() onboarding.SendWindow {
	w, err := onboarding.ParseSendWindow(model.Onboarding().Schedule.SendWindow)
	if err != nil {
		glog.Errorf("[onboarding]schedule.send_window: %s; using %02d:%02d-%02d:%02d\n", err,
			onboarding.DefaultSendWindow.StartHour, onboarding.DefaultSendWindow.StartMinute,
			onboarding.DefaultSendWindow.EndHour, onboarding.DefaultSendWindow.EndMinute)
		return onboarding.DefaultSendWindow
	}
	return w
}

// ----- entering the campaign -----

// EnrollNetworkOnboarding enters a network into the campaign: one row, the
// cohort time being the enrollment time. Two entry points, one gate
// (onboarding.Enroll, decision 2026-09-10 from the research readout):
//
//   - the account path (viaDevice false): NetworkCreate when no verification is
//     required, and AuthVerify when it completes, with the login the account
//     was created with. A network enters when that login is an email address;
//     the sequence can mail it. One without an email login is left to the
//     device path.
//   - the device path (viaDevice true): the network's first device client
//     (AuthNetworkClient). A network without an email login enters here, as a
//     row that exits at once as no_email, so the results job counts it among
//     the real population; a network older than onboarding.EnrollmentHorizon
//     does not enter at all.
//
// A network with neither an email login nor a device never enters: on Main
// about 85% of new networks are created through the API and never register a
// device, and they can neither see an offer screen nor be mailed. A second
// call for the same network is a no-op. Never fails the caller.
func EnrollNetworkOnboarding(clientSession *session.ClientSession, networkId server.Id, userAuth string, viaDevice bool) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[onboarding]campaign enrollment failed for network %s: %v\n", networkId, r)
		}
	}()
	now := server.NowUtc()
	cfg := model.Onboarding()

	createdAt := now
	if viaDevice {
		// the device path pays one row lookup, and one facts lookup only for a
		// network that has no row yet
		if model.GetNetworkOnboarding(clientSession.Ctx, networkId) != nil {
			return
		}
		auth, createTime, ok := model.NetworkEnrollmentFacts(clientSession.Ctx, networkId)
		if !ok {
			return
		}
		userAuth = auth
		createdAt = createTime
	}
	_, authType := model.NormalUserAuth(userAuth)
	hasEmail := authType == model.UserAuthTypeEmail
	if !onboarding.Enroll(hasEmail, viaDevice, createdAt, now) {
		return
	}

	row := &model.NetworkOnboarding{
		NetworkId: networkId,
		CreatedAt: now,
		Email:     hasEmail,
		Platform:  platformFromUserAgent(clientSession),
		Locale:    localeFromSession(clientSession),
	}
	if ip, ok := clientIp(clientSession); ok {
		if ipInfo, err := server.GetIpInfoFromString(ip); err == nil && ipInfo != nil {
			row.TimeZone = ipInfo.Timezone
			row.Country = model.NormalizeCountryCode(ipInfo.CountryCode)
		}
	}
	if a := cfg.AssignmentForSurface(networkId, model.ExperimentSurfaceEmailSequence, now); a != nil {
		row.ExperimentId = a.ExperimentId
		row.EmailVariant = a.Variant
	}

	switch {
	case !hasEmail:
		row.ExitReason = onboarding.ExitNoEmail
		row.ExitedAt = &now
	case row.EmailVariant == model.ExperimentVariantHoldout:
		row.ExitReason = onboarding.ExitHoldout
		row.ExitedAt = &now
	default:
		day := onboardingSchedule().Day(onboarding.StepE1, onboarding.PathB)
		at := onboarding.SendAt(now, day, onboarding.LoadLocation(row.TimeZone), onboardingSendWindow())
		row.NextStep = onboarding.StepE1
		row.NextSendAt = &at
	}

	if !model.CreateNetworkOnboarding(clientSession.Ctx, row) {
		return
	}
	onboardingStartsTotal.WithLabelValues(fmt.Sprintf("%t", hasEmail), row.EmailVariant).Inc()
	if row.ExitedAt != nil {
		onboardingExitsTotal.WithLabelValues(row.ExitReason).Inc()
		return
	}

	if OnboardingCampaignEnabled() {
		if err := brevoUpsertOnboardingContact(clientSession.Ctx, userAuth, row); err != nil {
			// the contact is a nicety (timezone/platform attributes); the sends
			// do not depend on it
			glog.Warningf("[onboarding]brevo contact upsert failed for network %s: %s\n", networkId, err)
		}
	}
	scheduleOnboardingCampaignStep(clientSession, networkId, onboarding.StepE1, *row.NextSendAt)
}

// RecordOnboardingClientContext stores what a device reports on auth-client:
// its time zone, locale and platform. Cheap updates, only while the sequence
// runs, never fail the caller.
func RecordOnboardingClientContext(clientSession *session.ClientSession, networkId server.Id, timeZone string, locale string, deviceSpec string) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[onboarding]client context failed for network %s: %v\n", networkId, r)
		}
	}()
	if tz := strings.TrimSpace(timeZone); tz != "" && len(tz) <= 64 {
		if _, err := time.LoadLocation(tz); err == nil {
			model.SetNetworkOnboardingTimeZone(clientSession.Ctx, networkId, tz)
		}
	}
	if l := normalizeLocale(locale); l != "" {
		model.SetNetworkOnboardingLocale(clientSession.Ctx, networkId, l)
	}
	if p := platformFromDeviceSpec(deviceSpec); p != "" {
		model.SetNetworkOnboardingPlatform(clientSession.Ctx, networkId, p)
	}
}

func clientIp(clientSession *session.ClientSession) (string, bool) {
	if clientSession == nil {
		return "", false
	}
	ip, _, err := clientSession.ClientIpPort()
	if err != nil || ip == "" {
		return "", false
	}
	return ip, true
}

// Platform values (model.EventPlatform*): ios, macos, android, web, windows, linux.
func platformFromUserAgent(clientSession *session.ClientSession) string {
	if clientSession == nil {
		return ""
	}
	ua := strings.ToLower(strings.Join(clientSession.Header["User-Agent"], " "))
	return platformFromDeviceSpec(ua)
}

func platformFromDeviceSpec(spec string) string {
	s := strings.ToLower(spec)
	switch {
	case s == "":
		return ""
	case strings.Contains(s, "iphone"), strings.Contains(s, "ipad"), strings.Contains(s, "ios"):
		return "ios"
	case strings.Contains(s, "android"):
		return "android"
	case strings.Contains(s, "macos"), strings.Contains(s, "mac os"), strings.Contains(s, "macintosh"), strings.Contains(s, "darwin"):
		return "macos"
	case strings.Contains(s, "windows"):
		return "windows"
	case strings.Contains(s, "linux"), strings.Contains(s, "ubuntu"):
		return "linux"
	case strings.Contains(s, "mozilla"), strings.Contains(s, "chrome"), strings.Contains(s, "safari"), strings.Contains(s, "firefox"):
		return "web"
	}
	return ""
}

// platformDeviceName is the `platform` template param ("iPhone").
func platformDeviceName(platform string) string {
	switch platform {
	case "ios":
		return "iPhone"
	case "android":
		return "Android"
	case "macos":
		return "Mac"
	case "windows":
		return "Windows PC"
	case "linux":
		return "Linux"
	case "web":
		return "browser"
	}
	return "device"
}

func localeFromSession(clientSession *session.ClientSession) string {
	if clientSession == nil {
		return ""
	}
	for _, header := range clientSession.Header["Accept-Language"] {
		first := strings.TrimSpace(strings.Split(strings.Split(header, ",")[0], ";")[0])
		if l := normalizeLocale(first); l != "" {
			return l
		}
	}
	return ""
}

// normalizeLocale keeps a BCP 47 tag's language and region ("de-DE", "zh-HK"),
// lower-case language and upper-case region, or "" for junk.
func normalizeLocale(locale string) string {
	locale = strings.TrimSpace(strings.ReplaceAll(locale, "_", "-"))
	if locale == "" || locale == "*" || 32 < len(locale) {
		return ""
	}
	parts := strings.Split(locale, "-")
	lang := strings.ToLower(parts[0])
	if len(lang) < 2 || 3 < len(lang) {
		return ""
	}
	for _, r := range lang {
		if r < 'a' || 'z' < r {
			return ""
		}
	}
	if len(parts) == 1 {
		return lang
	}
	region := strings.ToUpper(parts[len(parts)-1])
	if len(region) == 2 || len(region) == 3 {
		return lang + "-" + region
	}
	return lang
}

// ----- the step task -----

type OnboardingCampaignStepArgs struct {
	NetworkId server.Id `json:"network_id"`
	Step      string    `json:"step"`
}

type OnboardingCampaignStepResult struct {
	Action     string     `json:"action"`
	Template   string     `json:"template,omitempty"`
	Variant    string     `json:"variant,omitempty"`
	ExitReason string     `json:"exit_reason,omitempty"`
	NextStep   string     `json:"next_step,omitempty"`
	NextSendAt *time.Time `json:"next_send_at,omitempty"`
}

func scheduleOnboardingCampaignStep(clientSession *session.ClientSession, networkId server.Id, step string, at time.Time) {
	task.ScheduleTask(
		OnboardingCampaignStep,
		&OnboardingCampaignStepArgs{NetworkId: networkId, Step: step},
		clientSession,
		task.RunOnce("onboarding_campaign_step", networkId, step),
		task.RunAt(at),
	)
}

func scheduleOnboardingCampaignStepInTx(tx server.PgTx, clientSession *session.ClientSession, networkId server.Id, step string, at time.Time) {
	task.ScheduleTaskInTx(
		tx,
		OnboardingCampaignStep,
		&OnboardingCampaignStepArgs{NetworkId: networkId, Step: step},
		clientSession,
		task.RunOnce("onboarding_campaign_step", networkId, step),
		task.RunAt(at),
	)
}

// OnboardingCampaignStep runs one step for one network: re-evaluates the
// conditions and exits now, sends (or logs, when disabled), records, and hands
// the next step to the post function to schedule. A Brevo failure returns an
// error so the scheduler retries the step.
func OnboardingCampaignStep(
	args *OnboardingCampaignStepArgs,
	clientSession *session.ClientSession,
) (*OnboardingCampaignStepResult, error) {
	ctx := clientSession.Ctx
	now := server.NowUtc()
	row := model.GetNetworkOnboarding(ctx, args.NetworkId)
	if row == nil {
		return &OnboardingCampaignStepResult{Action: onboarding.ActionSkip}, nil
	}
	if row.Exited() {
		return &OnboardingCampaignStepResult{Action: onboarding.ActionExit, ExitReason: row.ExitReason}, nil
	}
	if row.NextStep != args.Step {
		// a replayed or stale task: the row moved on
		return &OnboardingCampaignStepResult{Action: onboarding.ActionSkip}, nil
	}

	facts, offer := loadOnboardingFacts(ctx, row, now)
	decision := onboarding.Decide(args.Step, facts)
	result := &OnboardingCampaignStepResult{
		Action:   decision.Action,
		Template: decision.Template,
		Variant:  decision.Variant,
		NextStep: decision.NextStep,
	}

	if decision.Action == onboarding.ActionExit {
		if model.ExitNetworkOnboarding(ctx, row.NetworkId, decision.ExitReason, now) {
			onboardingExitsTotal.WithLabelValues(decision.ExitReason).Inc()
		}
		result.ExitReason = decision.ExitReason
		return result, nil
	}

	var sentAt *time.Time
	if decision.Action == onboarding.ActionSend {
		if decision.IssueOffer {
			tier := model.Pro().PriceTierForCountry(row.Country)
			issued, _, err := IssueOnboardingOfferByEmail(clientSession, row.NetworkId, tier.Name)
			if err != nil {
				model.RecordNetworkOnboardingSendFailure(ctx, row.NetworkId, "offer: "+err.Error())
				return nil, fmt.Errorf("issue offer by email for network %s: %w", row.NetworkId, err)
			}
			offer = issued
		}
		outcome, err := sendOnboardingCampaignEmail(ctx, row, args.Step, decision, offer, now)
		if err != nil {
			onboardingEmailsTotal.WithLabelValues(args.Step, decision.Template, decision.Variant, "failed").Inc()
			model.RecordNetworkOnboardingSendFailure(ctx, row.NetworkId, err.Error())
			return nil, err
		}
		onboardingEmailsTotal.WithLabelValues(args.Step, decision.Template, decision.Variant, outcome).Inc()
		if outcome == "sent" {
			sentAt = &now
		}
	} else {
		onboardingEmailsTotal.WithLabelValues(args.Step, decision.Template, decision.Variant, "skipped").Inc()
	}

	if decision.NextStep != "" {
		day := onboardingSchedule().Day(decision.NextStep, facts.Path)
		at := onboarding.SendAt(row.CreatedAt, day, onboarding.LoadLocation(row.TimeZone), onboardingSendWindow())
		if at.Before(now) {
			// a step that is already due (a long retry, a late enable) goes out
			// at the next window edge rather than immediately
			at = onboarding.SendAt(now, 0, onboarding.LoadLocation(row.TimeZone), onboardingSendWindow())
			if at.Before(now) {
				at = now.Add(time.Minute)
			}
		}
		result.NextSendAt = &at
	} else {
		onboardingExitsTotal.WithLabelValues(onboarding.ExitDone).Inc()
	}
	server.Tx(ctx, func(tx server.PgTx) {
		model.AdvanceNetworkOnboardingInTx(tx, ctx, row.NetworkId, args.Step, sentAt, facts.Path, decision.NextStep, result.NextSendAt, now)
	})
	return result, nil
}

// OnboardingCampaignStepPost schedules the next step in the task's completion
// transaction, so a crash between the send and the schedule cannot lose the
// sequence (the row's next_step/next_send_at also let a sweep re-create it).
func OnboardingCampaignStepPost(
	args *OnboardingCampaignStepArgs,
	result *OnboardingCampaignStepResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if result != nil && result.NextStep != "" && result.NextSendAt != nil {
		scheduleOnboardingCampaignStepInTx(tx, clientSession, args.NetworkId, result.NextStep, *result.NextSendAt)
	}
	return nil
}

// loadOnboardingFacts reads everything a decision looks at, as of now.
func loadOnboardingFacts(ctx context.Context, row *model.NetworkOnboarding, now time.Time) (onboarding.Facts, *model.OnboardingOffer) {
	offer := model.GetOnboardingOffer(ctx, row.NetworkId)
	path := row.Path
	if p := model.OnboardingPathForNetwork(offer); p != "" {
		path = p
	}
	if path == "" {
		path = onboarding.PathB
	}
	offerState := onboarding.OfferStateNone
	if offer != nil {
		switch model.OnboardingOfferState(offer, now) {
		case model.OnboardingOfferStateActive:
			offerState = onboarding.OfferStateActive
		case model.OnboardingOfferStateRedeemed:
			offerState = onboarding.OfferStateRedeemed
		case model.OnboardingOfferStateExpired:
			offerState = onboarding.OfferStateExpired
		}
	}
	connectDays, connected := model.NetworkConnectDays(ctx, row.NetworkId, row.CreatedAt)
	facts := onboarding.Facts{
		Path:              path,
		HasEmail:          row.Email,
		Holdout:           row.EmailVariant == model.ExperimentVariantHoldout,
		Paused:            row.ExperimentId != "" && model.PausedVariantsForExperiment(row.ExperimentId, now)[row.EmailVariant],
		ProductUpdates:    model.NetworkProductUpdates(ctx, row.NetworkId),
		Pro:               model.IsProNetwork(ctx, row.NetworkId),
		NetworkExists:     model.NetworkExists(ctx, row.NetworkId),
		Bounced:           row.Bounced,
		Complained:        row.Complained,
		ComplainedStep:    row.ComplainedStep,
		HasConnected:      connected || model.HasOnboardingEvent(ctx, row.NetworkId, model.EventConnectFirst),
		WidgetAdded:       model.HasOnboardingEvent(ctx, row.NetworkId, model.EventWidgetAdded),
		OfferState:        offerState,
		FeedbackSubmitted: model.NetworkHasFeedback(ctx, row.NetworkId, row.CreatedAt) || model.HasOnboardingEvent(ctx, row.NetworkId, model.EventFeedbackSubmitted),
		EmailOpens:        model.CountOnboardingEvents(ctx, row.NetworkId, model.EventEmailOpened),
	}
	_ = connectDays
	return facts, offer
}

// ----- rendering -----

// OnboardingSendPlan is a fully resolved send: the template id and the params,
// as `bringyourctl onboarding preview` shows it and the sender posts it.
type OnboardingSendPlan struct {
	Step       string            `json:"step"`
	Template   string            `json:"template"`
	Variant    string            `json:"variant"`
	Locale     string            `json:"locale"`
	TemplateId int               `json:"template_id"`
	Params     map[string]string `json:"params"`
	Tags       []string          `json:"tags"`
}

func buildOnboardingSendPlan(ctx context.Context, row *model.NetworkOnboarding, step string, decision onboarding.Decision, offer *model.OnboardingOffer, now time.Time) (*OnboardingSendPlan, error) {
	cfg := model.Onboarding()
	templateId, ok := cfg.Brevo.TemplateId(decision.Template, decision.Variant, row.Locale)
	if !ok || templateId <= 0 {
		return nil, fmt.Errorf("no Brevo template for %s/%s/%s", decision.Template, decision.Variant, row.Locale)
	}
	token, err := onboarding.NewToken(&onboarding.TokenClaims{
		NetworkId: row.NetworkId,
		Step:      decision.Template,
		FlowStep:  step,
		ExpiresAt: now.Add(30 * 24 * time.Hour).Unix(),
	})
	if err != nil {
		return nil, fmt.Errorf("mint landing token: %w", err)
	}
	loc := onboarding.LoadLocation(row.TimeZone)
	tier := model.Pro().PriceTierForCountry(row.Country)
	regular := tier.YearlyUsd
	offerPrice := cfg.OfferPriceUsd(regular)
	var offerExpires *time.Time
	if offer != nil {
		if t := model.Pro().PriceTierByName(offer.Tier); t != nil {
			regular = t.YearlyUsd
			offerPrice = regular * float64(100-offer.PercentOff) / 100
		}
		expires := offer.ExpiresAt
		offerExpires = &expires
	}
	connectDays, _ := model.NetworkConnectDays(ctx, row.NetworkId, row.CreatedAt)
	providers, countries := providersOnline(ctx)
	in := onboarding.ParamsInput{
		Template:        decision.Template,
		Variant:         decision.Variant,
		Opening:         decision.Opening,
		Platform:        platformDeviceName(row.Platform),
		DailyDataGb:     float64(model.Pro().Free.Data) / float64(model.Gib),
		DaysActive:      int(now.Sub(row.CreatedAt).Hours() / 24),
		ConnectDays:     connectDays,
		ProvidersOnline: providers,
		CountriesOnline: countries,
		OfferExpiresAt:  offerExpires,
		OfferPriceUsd:   offerPrice,
		RegularYearUsd:  regular,
		TrialDays:       14,
		Location:        loc,
		Now:             now,
		Token:           token,
		SiteUrl:         cfg.EffectiveSiteUrl(),
		CompanyLine:     cfg.EffectiveCompanyLine(),
	}
	return &OnboardingSendPlan{
		Step:       step,
		Template:   decision.Template,
		Variant:    decision.Variant,
		Locale:     row.Locale,
		TemplateId: templateId,
		Params:     onboarding.BuildParams(in),
		Tags:       []string{"onboarding", decision.Template, decision.Variant},
	}, nil
}

// providersOnline is the live provider and country count the E2 template
// quotes, cached for an hour: it is one heavy query and every send in the hour
// can share it.
var providersOnlineCache struct {
	sync.Mutex
	providers int
	countries int
	at        time.Time
}

func providersOnline(ctx context.Context) (providers int, countries int) {
	providersOnlineCache.Lock()
	defer providersOnlineCache.Unlock()
	if !providersOnlineCache.at.IsZero() && time.Since(providersOnlineCache.at) < time.Hour {
		return providersOnlineCache.providers, providersOnlineCache.countries
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				glog.Warningf("[onboarding]providers online: %v\n", r)
			}
		}()
		localSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer localSession.Cancel()
		result, err := model.GetProviderLocations(localSession)
		if err != nil || result == nil {
			return
		}
		count := 0
		for _, location := range result.Locations {
			if location.LocationType == model.LocationTypeCountry {
				count += location.ProviderCount
			}
		}
		providersOnlineCache.providers = count
		providersOnlineCache.countries = result.CountryCount
		providersOnlineCache.at = time.Now()
	}()
	return providersOnlineCache.providers, providersOnlineCache.countries
}

// ----- sending -----

type brevoSmtpEmailArgs struct {
	To         []brevoEmailAddress `json:"to"`
	TemplateId int                 `json:"templateId"`
	Params     map[string]string   `json:"params"`
	Tags       []string            `json:"tags,omitempty"`
	ReplyTo    *brevoEmailAddress  `json:"replyTo,omitempty"`
	Headers    map[string]string   `json:"headers,omitempty"`
}

type brevoEmailAddress struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

type brevoSmtpEmailResult struct {
	MessageId string `json:"messageId"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

// brevoSendTemplate posts one transactional email. Callers gate on
// OnboardingCampaignEnabled; this function itself refuses to run while
// disabled so no path can bypass the switch.
func brevoSendTemplate(ctx context.Context, to string, plan *OnboardingSendPlan) (messageId string, err error) {
	if !OnboardingCampaignEnabled() {
		return "", fmt.Errorf("onboarding campaign is disabled (onboarding.yml enabled: false)")
	}
	cfg := model.Onboarding()
	args := &brevoSmtpEmailArgs{
		To:         []brevoEmailAddress{{Email: to}},
		TemplateId: plan.TemplateId,
		Params:     plan.Params,
		Tags:       plan.Tags,
		ReplyTo:    &brevoEmailAddress{Email: cfg.EffectiveReplyTo()},
		Headers:    map[string]string{"X-Mailin-custom": "onboarding:" + plan.Step + ":" + plan.Template + ":" + plan.Variant},
	}
	status, r, err := server.HttpPostWithStatus[brevoSmtpEmailResult](
		ctx,
		brevoApiUrl("smtp/email"),
		args,
		brevoHeader,
		brevoResponseJsonObject[brevoSmtpEmailResult],
	)
	if err != nil {
		return "", fmt.Errorf("brevo send: %w", err)
	}
	if status == nil || status.Code < 200 || 300 <= status.Code {
		code := "?"
		if status != nil {
			code = status.Status
		}
		return "", fmt.Errorf("brevo send: %s %s %s", code, r.Code, r.Message)
	}
	if r.MessageId == "" {
		return "", fmt.Errorf("brevo send: no message id")
	}
	return r.MessageId, nil
}

// sendOnboardingCampaignEmail renders and sends one step to the network's
// account email. Returns "sent" or "dry_run"; an error means retry.
func sendOnboardingCampaignEmail(ctx context.Context, row *model.NetworkOnboarding, step string, decision onboarding.Decision, offer *model.OnboardingOffer, now time.Time) (string, error) {
	_, userAuth, ok := model.GetNetworkAdminUserAuth(ctx, row.NetworkId)
	if !ok || userAuth == "" {
		return "", fmt.Errorf("network %s has no admin email", row.NetworkId)
	}
	plan, err := buildOnboardingSendPlan(ctx, row, step, decision, offer, now)
	if err != nil {
		return "", err
	}
	if !OnboardingCampaignEnabled() {
		glog.V(1).Infof("[onboarding]dry run: would send %s/%s/%s (template %d) to %s for network %s\n",
			plan.Template, plan.Variant, plan.Locale, plan.TemplateId, maskEmail(userAuth), row.NetworkId)
		return "dry_run", nil
	}
	messageId, err := brevoSendTemplate(ctx, userAuth, plan)
	if err != nil {
		return "", err
	}
	server.Tx(ctx, func(tx server.PgTx) {
		model.AddNetworkOnboardingEmailInTx(tx, ctx, &model.NetworkOnboardingEmail{
			MessageId:         messageId,
			NetworkId:         row.NetworkId,
			Step:              step,
			Template:          plan.Template,
			Variant:           plan.Variant,
			Experiment:        row.ExperimentId,
			ExperimentVariant: row.EmailVariant,
			TemplateId:        plan.TemplateId,
			Locale:            plan.Locale,
			SentAt:            now,
		})
	})
	writeOnboardingEmailEvent(ctx, row.NetworkId, model.EventEmailSent, plan.Template, step, row.ExperimentId, row.EmailVariant, row.Platform, now)
	return "sent", nil
}

// writeOnboardingEmailEvent records an email.* event with the sequence
// experiment stamped from the row (WriteServerEvent would stamp a per-step
// surface instead). Never fails the caller.
func writeOnboardingEmailEvent(ctx context.Context, networkId server.Id, name string, template string, flowStep string, experiment string, variant string, platform string, at time.Time) {
	props := map[string]any{"step": template}
	if onboarding.IsFlowStep(flowStep) {
		props["flow_step"] = flowStep
	}
	if experiment != "" {
		props["experiment"] = experiment
		props["variant"] = variant
	}
	validated, err := model.ValidateServerEvent(name, props)
	if err != nil {
		glog.Errorf("[onboarding]refusing %s: %s\n", name, err)
		return
	}
	event := &model.OnboardingEvent{
		NetworkId:  networkId,
		Name:       name,
		At:         at,
		ReceivedAt: server.NowUtc(),
		Platform:   platform,
		Path:       model.OnboardingPathForNetwork(model.GetOnboardingOffer(ctx, networkId)),
		Experiment: experiment,
		Variant:    variant,
		Props:      validated,
	}
	if err := model.AddOnboardingEvent(ctx, event); err != nil {
		glog.Errorf("[onboarding]could not write %s for network %s: %s\n", name, networkId, err)
	}
}

// ----- Brevo contacts -----

type brevoContactUpsertArgs struct {
	Email         string            `json:"email"`
	UpdateEnabled bool              `json:"updateEnabled"`
	Attributes    map[string]string `json:"attributes,omitempty"`
}

var brevoContactAttributesEnsured sync.Once

// brevoUpsertOnboardingContact creates or updates the Brevo contact with the
// attributes the campaign's segments use. Creates the attributes once per
// process (idempotent against the account).
func brevoUpsertOnboardingContact(ctx context.Context, email string, row *model.NetworkOnboarding) error {
	brevoContactAttributesEnsured.Do(func() {
		for _, name := range []string{"EXT_ID", "CONTACT_TIMEZONE", "PLATFORM", "LOCALE"} {
			status, _, err := server.HttpPostWithStatus[brevoContactResult](
				ctx,
				brevoApiUrl("contacts/attributes/normal/"+name),
				map[string]string{"type": "text"},
				brevoHeader,
				brevoResponseJsonObject[brevoContactResult],
			)
			if err != nil {
				glog.Warningf("[onboarding]brevo attribute %s: %s\n", name, err)
				continue
			}
			// 400 duplicate_parameter = it exists
			if status != nil && (status.Code == 400 || (200 <= status.Code && status.Code < 300)) {
				continue
			}
			if status != nil {
				glog.Warningf("[onboarding]brevo attribute %s: %s\n", name, status.Status)
			}
		}
	})
	args := &brevoContactUpsertArgs{
		Email:         email,
		UpdateEnabled: true,
		Attributes: map[string]string{
			"EXT_ID":           row.NetworkId.String(),
			"CONTACT_TIMEZONE": row.TimeZone,
			"PLATFORM":         row.Platform,
			"LOCALE":           row.Locale,
		},
	}
	status, r, err := server.HttpPostWithStatus[brevoContactResult](
		ctx,
		brevoApiUrl("contacts"),
		args,
		brevoHeader,
		brevoResponseJsonObject[brevoContactResult],
	)
	if err != nil {
		return err
	}
	if status == nil {
		return fmt.Errorf("missing response status")
	}
	if (200 <= status.Code && status.Code < 300) || r.Code == "duplicate_parameter" {
		return nil
	}
	return fmt.Errorf("%s %s %s", status.Status, r.Code, r.Message)
}

type brevoContactResult struct {
	Id      uint64 `json:"id"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ----- the transactional webhook -----

// webhookTags reads the send's tags from either form Brevo uses.
func webhookTags(args *BrevoWebhookArgs) []string {
	if 0 < len(args.Tags) {
		return args.Tags
	}
	if args.Tag != "" {
		var tags []string
		if err := json.Unmarshal([]byte(args.Tag), &tags); err == nil {
			return tags
		}
		return []string{args.Tag}
	}
	return nil
}

// handleOnboardingBrevoWebhook records a campaign delivery event against the
// network the message was sent to. Non-campaign messages (the message id is
// unknown) are ignored here; the caller keeps the account-wide unsubscribe
// behavior. Never fails the webhook.
func handleOnboardingBrevoWebhook(ctx context.Context, args *BrevoWebhookArgs) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[onboarding]webhook %s failed: %v\n", args.Event, r)
		}
	}()
	name := onboarding.EventForWebhook(args.Event)
	if name == "" {
		return
	}
	email := model.GetNetworkOnboardingEmail(ctx, args.MessageId)
	if email == nil {
		tags := webhookTags(args)
		if len(tags) == 0 || tags[0] != "onboarding" {
			return
		}
		// a campaign tag without a known message id: attribute by the custom
		// header when present, otherwise there is no network to record against
		glog.V(1).Infof("[onboarding]webhook %s for unknown campaign message %q (%v)\n", args.Event, args.MessageId, tags)
		return
	}
	at := server.NowUtc()
	if 0 < args.TsEvent {
		at = time.Unix(args.TsEvent, 0).UTC()
	}
	onboardingWebhookEventsTotal.WithLabelValues(name, email.Template).Inc()
	writeOnboardingEmailEvent(ctx, email.NetworkId, name, email.Template, email.Step, email.Experiment, email.ExperimentVariant, "", at)

	switch name {
	case model.EventEmailBounced:
		model.MarkNetworkOnboardingBounced(ctx, email.NetworkId)
	case model.EventEmailComplained:
		model.MarkNetworkOnboardingComplained(ctx, email.NetworkId, email.Step)
	}
	if reason := onboarding.WebhookExit(name); reason != "" {
		if model.ExitNetworkOnboarding(ctx, email.NetworkId, reason, at) {
			onboardingExitsTotal.WithLabelValues(reason).Inc()
		}
	}
}

// ----- webhook registration -----

type brevoWebhookListResult struct {
	Webhooks []struct {
		Id   int64  `json:"id"`
		Url  string `json:"url"`
		Type string `json:"type"`
	} `json:"webhooks"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type brevoWebhookCreateArgs struct {
	Url         string   `json:"url"`
	Description string   `json:"description"`
	Events      []string `json:"events"`
	Type        string   `json:"type"`
}

// The transactional events the campaign records (Brevo's webhook event names).
var onboardingBrevoWebhookEvents = []string{
	"delivered", "opened", "uniqueOpened", "click", "hardBounce", "softBounce", "blocked", "spam", "unsubscribed", "invalid",
}

// EnsureOnboardingBrevoWebhook registers the transactional webhook for
// onboarding.brevo.webhook_url once (idempotent by url). Refuses while the
// engine is disabled. Returns what it did.
func EnsureOnboardingBrevoWebhook(ctx context.Context) (string, error) {
	if !OnboardingCampaignEnabled() {
		return "", fmt.Errorf("onboarding campaign is disabled (onboarding.yml enabled: false); not registering")
	}
	url := strings.TrimSpace(model.Onboarding().Brevo.WebhookUrl)
	if url == "" {
		return "skipped: onboarding.brevo.webhook_url is empty", nil
	}
	status, list, err := server.HttpGetWithStatus[brevoWebhookListResult](
		ctx,
		brevoApiUrl("webhooks?type=transactional"),
		brevoHeader,
		brevoResponseJsonObject[brevoWebhookListResult],
	)
	if err != nil {
		return "", fmt.Errorf("list webhooks: %w", err)
	}
	if status != nil && status.Code != 404 && (status.Code < 200 || 300 <= status.Code) {
		return "", fmt.Errorf("list webhooks: %s %s", status.Status, list.Message)
	}
	for _, w := range list.Webhooks {
		if strings.EqualFold(strings.TrimRight(w.Url, "/"), strings.TrimRight(url, "/")) {
			return fmt.Sprintf("exists: webhook %d %s", w.Id, w.Url), nil
		}
	}
	created := &brevoWebhookCreateArgs{
		Url:         url,
		Description: "URnetwork onboarding campaign (server-registered)",
		Events:      onboardingBrevoWebhookEvents,
		Type:        "transactional",
	}
	status, r, err := server.HttpPostWithStatus[brevoContactResult](
		ctx,
		brevoApiUrl("webhooks"),
		created,
		brevoHeader,
		brevoResponseJsonObject[brevoContactResult],
	)
	if err != nil {
		return "", fmt.Errorf("create webhook: %w", err)
	}
	if status == nil || status.Code < 200 || 300 <= status.Code {
		return "", fmt.Errorf("create webhook: %s %s %s", status.Status, r.Code, r.Message)
	}
	return fmt.Sprintf("created: webhook %d %s", r.Id, url), nil
}

// ----- tooling (bringyourctl onboarding) -----

// OnboardingStatus is what `bringyourctl onboarding status` prints.
type OnboardingStatus struct {
	Row    *model.NetworkOnboarding        `json:"row"`
	Offer  *model.OnboardingOffer          `json:"offer"`
	Emails []*model.NetworkOnboardingEmail `json:"emails"`
	Events []*model.OnboardingEvent        `json:"events"`
	Facts  *onboarding.Facts               `json:"facts,omitempty"`
}

func OnboardingCampaignStatus(ctx context.Context, networkId server.Id) *OnboardingStatus {
	status := &OnboardingStatus{
		Row:    model.GetNetworkOnboarding(ctx, networkId),
		Offer:  model.GetOnboardingOffer(ctx, networkId),
		Emails: model.ListNetworkOnboardingEmails(ctx, networkId),
		Events: model.ListOnboardingEvents(ctx, networkId, 500),
	}
	if status.Row != nil {
		facts, _ := loadOnboardingFacts(ctx, status.Row, server.NowUtc())
		status.Facts = &facts
	}
	return status
}

// OnboardingPreview is `bringyourctl onboarding preview`: the decision the step
// would make now and, when it would send, the resolved send.
type OnboardingPreview struct {
	Decision onboarding.Decision `json:"decision"`
	Plan     *OnboardingSendPlan `json:"plan,omitempty"`
	Error    string              `json:"error,omitempty"`
}

func OnboardingCampaignPreview(ctx context.Context, networkId server.Id, step string) (*OnboardingPreview, error) {
	row := model.GetNetworkOnboarding(ctx, networkId)
	if row == nil {
		return nil, fmt.Errorf("network %s is not in the campaign", networkId)
	}
	now := server.NowUtc()
	facts, offer := loadOnboardingFacts(ctx, row, now)
	decision := onboarding.Decide(step, facts)
	preview := &OnboardingPreview{Decision: decision}
	if decision.Action == onboarding.ActionSend {
		plan, err := buildOnboardingSendPlan(ctx, row, step, decision, offer, now)
		if err != nil {
			preview.Error = err.Error()
		} else {
			preview.Plan = plan
		}
	}
	return preview, nil
}

// OnboardingCampaignSendTest sends ONE email of a template/variant/locale to
// an explicit address with sample data, through the real Brevo template. The
// only send path outside the scheduler; still behind `enabled`. Records no
// row and no event.
func OnboardingCampaignSendTest(ctx context.Context, to string, template string, variant string, locale string) (string, error) {
	if !OnboardingCampaignEnabled() {
		return "", fmt.Errorf("onboarding campaign is disabled (onboarding.yml enabled: false)")
	}
	if variant == "" {
		variant = onboarding.VariantDefault
	}
	if locale == "" {
		locale = "en"
	}
	cfg := model.Onboarding()
	templateId, ok := cfg.Brevo.TemplateId(template, variant, locale)
	if !ok || templateId <= 0 {
		return "", fmt.Errorf("no Brevo template for %s/%s/%s", template, variant, locale)
	}
	now := server.NowUtc()
	expires := now.Add(cfg.OfferValidity())
	tier := model.Pro().PriceTiers()[0]
	opening := onboarding.OpeningJoined
	providers, countries := providersOnline(ctx)
	plan := &OnboardingSendPlan{
		Step: "test", Template: template, Variant: variant, Locale: locale, TemplateId: templateId,
		Params: onboarding.BuildParams(onboarding.ParamsInput{
			Template: template, Variant: variant, Opening: opening,
			Platform: "iPhone", DailyDataGb: float64(model.Pro().Free.Data) / float64(model.Gib),
			DaysActive: 3, ConnectDays: 2, ProvidersOnline: providers, CountriesOnline: countries,
			OfferExpiresAt: &expires, OfferPriceUsd: cfg.OfferPriceUsd(tier.YearlyUsd), RegularYearUsd: tier.YearlyUsd, TrialDays: 14,
			Location: time.UTC, Now: now, Token: "test", SiteUrl: cfg.EffectiveSiteUrl(), CompanyLine: cfg.EffectiveCompanyLine(),
		}),
		Tags: []string{"onboarding", "test", template, variant},
	}
	return brevoSendTemplate(ctx, to, plan)
}
