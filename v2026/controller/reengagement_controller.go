// SPDX-License-Identifier: MPL-2.0

package controller

// The re-engagement email program (mmm/reengagement/PLAN.md): a daily scan
// opens episodes for the four streams (lapsed, never activated, fix
// follow-up, win-back), one scheduled task per network and step re-evaluates
// the facts and sends through Brevo's transactional API, and the webhook,
// landing clicks and one-tap answers feed back into the row. The decisions are
// pure (server/reengagement); this file loads facts, talks to Brevo and the
// scheduler, and records what happened.
//
// SAFETY: every outbound side effect is behind reengagement.yml `enabled`
// (default false, and the file is optional). While disabled the scan opens no
// episode and only counts what it would have opened, so turning it on never
// mails a backlog: the first scan after enabling opens the networks reaching
// day 30 that day. Holdout networks get virtual sends on the same clock.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/onboarding"
	"github.com/urnetwork/server/v2026/reengagement"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

var reengagementEpisodesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "reengagement",
	Name:      "episodes_total",
	Help:      "Scan outcomes per stream: opened, dry_run (program disabled), policy (the mail policy refused), skipped (not eligible now).",
}, []string{"stream", "result"})

var reengagementEmailsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "reengagement",
	Name:      "emails_total",
	Help:      "Step outcomes: sent, virtual (holdout), skipped, deferred (one email a week), dry_run (disabled), failed (Brevo error).",
}, []string{"step", "template", "variant", "result"})

var reengagementExitsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "reengagement",
	Name:      "exits_total",
	Help:      "Episodes ending, by stream and reason.",
}, []string{"stream", "reason"})

var reengagementWebhookEventsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "reengagement",
	Name:      "webhook_events_total",
	Help:      "Brevo transactional webhook events attributed to a re-engagement send, by event and template.",
}, []string{"event", "template"})

var reengagementWebhookUnattributedEventsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "reengagement",
	Name:      "webhook_unattributed_events_total",
	Help:      "Brevo transactional webhook events of re-engagement sends that name no recorded send, so no network records them, by event, template (from the send's tags) and reason: anonymous (Brevo's anonymous tracking posts clicks and spam reports with an an# message id and no email; opens are never recorded) or unknown_send (no send recorded the message id, as for a test send).",
}, []string{"event", "template", "reason"})

var reengagementContextSourcesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "reengagement",
	Name:      "context_sources_total",
	Help:      "Scan candidates by stream and where each part of their mail context came from: part is country, locale, platform or time_zone; source is onboarding, app_store, play, stripe, client_event, device, client (what the network's apps and browsers last reported), country (the country's zone or language), activity (the time of day of the last connection or sign-up) or unknown.",
}, []string{"stream", "part", "source"})

func init() {
	prometheus.MustRegister(
		reengagementEpisodesTotal,
		reengagementEmailsTotal,
		reengagementExitsTotal,
		reengagementWebhookEventsTotal,
		reengagementWebhookUnattributedEventsTotal,
		reengagementContextSourcesTotal,
	)
}

// ReengagementEnabled is reengagement.yml `enabled`: the master switch.
func ReengagementEnabled() bool {
	return model.Reengagement().Enabled
}

func dayStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

const day = 24 * time.Hour

// ----- the daily scan -----

// The scan runs once a day, after the 02:00 onboarding rollup.
const reengagementScanHour, reengagementScanMinute = 2, 30

// NextReengagementScanAt is the next 02:30 UTC after `now`.
func NextReengagementScanAt(now time.Time) time.Time {
	at := time.Date(now.Year(), now.Month(), now.Day(), reengagementScanHour, reengagementScanMinute, 0, 0, time.UTC)
	if !at.After(now) {
		at = at.Add(day)
	}
	return at
}

type ReengagementScanArgs struct {
	// count only, open nothing (the CLI's --dry-run; also forced while disabled)
	DryRun bool `json:"dry_run,omitempty"`
}

type ReengagementScanResult struct {
	Candidates map[string]int `json:"candidates"`
	Opened     map[string]int `json:"opened"`
	Refused    map[string]int `json:"refused"`
	Lifted     int            `json:"lifted"`
	DryRun     bool           `json:"dry_run"`
}

func ScheduleReengagementScan(clientSession *session.ClientSession, tx server.PgTx, at time.Time) {
	task.ScheduleTaskInTx(
		tx,
		ReengagementScan,
		&ReengagementScanArgs{},
		clientSession,
		task.RunOnce("reengagement_scan"),
		task.RunAt(at),
	)
}

// ReengagementScan opens the day's episodes for every enabled stream and lifts
// the suppressions a connection renewed.
func ReengagementScan(args *ReengagementScanArgs, clientSession *session.ClientSession) (*ReengagementScanResult, error) {
	ctx := clientSession.Ctx
	now := server.NowUtc()
	cfg := model.Reengagement()
	result := &ReengagementScanResult{
		Candidates: map[string]int{},
		Opened:     map[string]int{},
		Refused:    map[string]int{},
		DryRun:     args.DryRun || !cfg.Enabled,
	}
	s := cfg.ScheduleValue()
	limit := cfg.BatchLimit()
	catchUp := time.Duration(cfg.CatchUpDays()-1) * day
	today := dayStart(now)

	open := func(stream string, candidate *model.ReengagementCandidate, firstStep string, lastConnect *time.Time) {
		result.Candidates[stream]++
		mailContext, reader := reengagementContextReader(ctx, candidate.NetworkId, lastConnect)
		countReengagementContextSources(stream, mailContext)
		if refusal := reengagementPolicyExit(ctx, candidate.NetworkId, stream, mailContext.Country, now); refusal != "" {
			result.Refused[stream]++
			reengagementEpisodesTotal.WithLabelValues(stream, "policy").Inc()
			return
		}
		if result.DryRun {
			reengagementEpisodesTotal.WithLabelValues(stream, "dry_run").Inc()
			return
		}
		if openReengagementEpisode(clientSession, candidate.NetworkId, stream, firstStep, lastConnect, mailContext, reader.SendZone, now) {
			result.Opened[stream]++
			reengagementEpisodesTotal.WithLabelValues(stream, "opened").Inc()
		} else {
			reengagementEpisodesTotal.WithLabelValues(stream, "skipped").Inc()
		}
	}

	if cfg.StreamEnabled(reengagement.StreamLapsed) {
		// the last connection day exactly DormantDays ago (and the days a
		// missed scan left behind); networks idle longer are never opened
		lastDay := today.Add(-time.Duration(s.DormantDays) * day)
		for _, c := range model.ListLapsedReengagementCandidates(ctx, lastDay.Add(-catchUp), lastDay.Add(day), limit) {
			last := c.At
			open(reengagement.StreamLapsed, c, reengagement.StepR1, &last)
		}
	}
	if cfg.StreamEnabled(reengagement.StreamNeverActivated) {
		signupDay := today.Add(-time.Duration(s.NeverActiveDay) * day)
		for _, c := range model.ListNeverActivatedReengagementCandidates(ctx, signupDay.Add(-catchUp), signupDay.Add(day), limit) {
			if _, connected := model.NetworkConnectDays(ctx, c.NetworkId, time.Time{}); connected {
				continue
			}
			open(reengagement.StreamNeverActivated, c, reengagement.StepN1, nil)
		}
	}
	if cfg.StreamEnabled(reengagement.StreamFix) {
		answeredAfter := now.Add(-time.Duration(s.FixWindowDays) * day)
		for _, row := range model.ListPendingReengagementFixes(ctx, answeredAfter, limit) {
			platform := row.FixPlatform
			if platform == "" {
				platform = row.Platform
			}
			if len(reengagement.FixHighlights(cfg.ParsedHighlights(), platform, row.Country, row.FixReason, *row.FixReasonAt, now)) == 0 {
				continue
			}
			open(reengagement.StreamFix, &model.ReengagementCandidate{NetworkId: row.NetworkId, At: *row.FixReasonAt}, reengagement.StepF1, row.LastConnectAt)
		}
	}
	if cfg.StreamEnabled(reengagement.StreamWinback) {
		// W1 goes after Pro actually stopped and no store can still be
		// retrying the renewal (Schedule.WinbackAfterDays)
		for _, grace := range []bool{true, false} {
			endDay := today.Add(-time.Duration(s.WinbackAfterDays(grace)) * day)
			for _, c := range model.ListWinbackReengagementCandidates(ctx, endDay.Add(-catchUp), endDay.Add(day), grace, limit) {
				// W is for networks that paid (PLAN.md "AUDIENCES AND
				// STREAMS"): the candidate query counts any supporter window
				// with revenue, which takes in App Store trial markers and Play
				// trial windows; the offer's strict check decides
				if !model.NetworkHasPaidSupporterPeriod(ctx, c.NetworkId) {
					result.Candidates[reengagement.StreamWinback]++
					reengagementEpisodesTotal.WithLabelValues(reengagement.StreamWinback, "skipped").Inc()
					continue
				}
				open(reengagement.StreamWinback, c, reengagement.StepW1, nil)
			}
		}
	}

	if !result.DryRun {
		// a connection renews the relationship a sunset or a "switched" ended
		liftable := reengagement.LiftableSuppressions()
		for _, networkId := range model.ListLiftableSuppressedNetworksConnectedOn(ctx, today.Add(-day), today.Add(day), liftable, limit) {
			if LiftNetworkMarketingSuppression(ctx, networkId, liftable) {
				result.Lifted++
			}
		}
	}
	glog.Infof("[reengagement]scan: candidates %v, opened %v, refused %v, lifted %d, dry run %t\n",
		result.Candidates, result.Opened, result.Refused, result.Lifted, result.DryRun)
	return result, nil
}

func ReengagementScanPost(args *ReengagementScanArgs, result *ReengagementScanResult, clientSession *session.ClientSession, tx server.PgTx) error {
	if args.DryRun {
		// a CLI dry run is a one-off, not the daily chain
		return nil
	}
	ScheduleReengagementScan(clientSession, tx, NextReengagementScanAt(server.NowUtc()))
	return nil
}

func reengagementPurpose(stream string) string {
	if stream == reengagement.StreamWinback {
		return onboarding.PurposeWinback
	}
	return onboarding.PurposeReengagement
}

// reengagementPolicyExit is the mail policy's verdict for the network now.
func reengagementPolicyExit(ctx context.Context, networkId server.Id, stream string, country string, now time.Time) string {
	mail := model.GetNetworkMailState(ctx, networkId)
	_, createTime, _ := model.NetworkEnrollmentFacts(ctx, networkId)
	return model.Onboarding().MailPolicy().Exit(reengagementPurpose(stream), onboarding.MailFacts{
		Country:       country,
		ConsentSource: mail.Source,
		CreatedAt:     createTime,
		LastPaidEnd:   model.NetworkLastPaidEnd(ctx, networkId),
		Now:           now,
	})
}

// openReengagementEpisode starts the episode in the network's resolved mail
// context and schedules its first step in the context's send zone.
func openReengagementEpisode(clientSession *session.ClientSession, networkId server.Id, stream string, firstStep string, lastConnect *time.Time, mailContext ReengagementContext, sendZone onboarding.SendZone, now time.Time) bool {
	ctx := clientSession.Ctx
	cfg := model.Reengagement()
	s := cfg.ScheduleValue()
	row := &model.NetworkReengagement{
		NetworkId:     networkId,
		Stream:        stream,
		StartedAt:     now,
		LastConnectAt: lastConnect,
		Platform:      mailContext.Platform,
		Locale:        mailContext.Locale,
		TimeZone:      mailContext.TimeZone,
		Country:       mailContext.Country,
		NextStep:      firstStep,
	}
	if a := cfg.AssignmentForSurface(networkId, model.ExperimentSurfaceForStream(stream), now); a != nil {
		row.ExperimentId = a.ExperimentId
		row.Variant = a.Variant
	}
	at := reengagement.SendAtZone(now, 0, sendZone, s, now)
	row.NextSendAt = &at
	cooldownBefore := now.Add(-time.Duration(s.CooldownDays) * day)
	if !model.StartNetworkReengagement(ctx, row, cooldownBefore, now) {
		return false
	}
	started := model.GetNetworkReengagement(ctx, networkId)
	if started == nil {
		return false
	}
	props := map[string]any{"stream": stream}
	if row.ExperimentId != "" {
		props["experiment"] = row.ExperimentId
		props["arm"] = row.Variant
	}
	if mailContext.Country != "" {
		// the country signal and its source, kept with the episode
		// (PLAN.md "Compliance" 7)
		props["country"] = mailContext.Country
		props["country_source"] = mailContext.CountrySource
	}
	WriteServerEventWithContext(ctx, networkId, model.EventReengageEpisodeStarted, props, mailContext.Platform)
	server.Tx(ctx, func(tx server.PgTx) {
		scheduleReengagementStepInTx(tx, clientSession, networkId, started.Episode, firstStep, at)
	})
	return true
}

// ----- the step task -----

type ReengagementStepArgs struct {
	NetworkId server.Id `json:"network_id"`
	Episode   int       `json:"episode"`
	Step      string    `json:"step"`
}

type ReengagementStepResult struct {
	Action     string     `json:"action"`
	Template   string     `json:"template,omitempty"`
	Variant    string     `json:"variant,omitempty"`
	ExitReason string     `json:"exit_reason,omitempty"`
	NextStep   string     `json:"next_step,omitempty"`
	NextSendAt *time.Time `json:"next_send_at,omitempty"`
}

func scheduleReengagementStepInTx(tx server.PgTx, clientSession *session.ClientSession, networkId server.Id, episode int, step string, at time.Time) {
	task.ScheduleTaskInTx(
		tx,
		ReengagementStep,
		&ReengagementStepArgs{NetworkId: networkId, Episode: episode, Step: step},
		clientSession,
		task.RunOnce("reengagement_step", networkId, episode, step),
		task.RunAt(at),
	)
}

// ReengagementStep runs one step for one network: re-evaluates the facts,
// sends (or records a virtual send for the holdout, or logs while disabled),
// and hands the next step to the post function. A Brevo failure returns an
// error so the scheduler retries the step.
func ReengagementStep(args *ReengagementStepArgs, clientSession *session.ClientSession) (*ReengagementStepResult, error) {
	ctx := clientSession.Ctx
	now := server.NowUtc()
	cfg := model.Reengagement()
	s := cfg.ScheduleValue()
	row := model.GetNetworkReengagement(ctx, args.NetworkId)
	if row == nil {
		return &ReengagementStepResult{Action: reengagement.ActionSkip}, nil
	}
	if row.Exited() || row.Episode != args.Episode {
		return &ReengagementStepResult{Action: reengagement.ActionExit, ExitReason: row.ExitReason}, nil
	}
	if row.NextStep != args.Step {
		// a replay after the advance committed: follow the row, send nothing
		return &ReengagementStepResult{Action: reengagement.ActionSkip, NextStep: row.NextStep, NextSendAt: row.NextSendAt}, nil
	}

	facts := loadReengagementFacts(ctx, row, now)
	decision := reengagement.Decide(args.Step, facts, s)
	result := &ReengagementStepResult{Action: decision.Action, Template: decision.Template, Variant: decision.Variant, NextStep: decision.NextStep}

	if decision.Action == reengagement.ActionExit {
		applyReengagementExit(ctx, row, decision.ExitReason, decision.Suppress, decision.JoinFix, decision.Quiet, now)
		result.ExitReason = decision.ExitReason
		return result, nil
	}

	// the zone and the languages as of now: a device may have reported them
	// since the episode opened
	reader := reengagementMailReader(ctx, row)
	if decision.Action == reengagement.ActionSend || decision.Action == reengagement.ActionVirtual {
		// one campaign email a week across both programs, for both arms
		if last := model.LastCampaignSendAt(ctx, row.NetworkId); last != nil {
			if earliest := last.Add(time.Duration(s.MinGapDays) * day); now.Before(earliest) {
				at := reengagement.SendAtZone(earliest, 0, reader.SendZone, s, now)
				reengagementEmailsTotal.WithLabelValues(args.Step, decision.Template, decision.Variant, "deferred").Inc()
				server.Tx(ctx, func(tx server.PgTx) {
					model.AdvanceNetworkReengagementInTx(tx, ctx, row.NetworkId, args.Step, nil, args.Step, &at, now)
				})
				return &ReengagementStepResult{Action: reengagement.ActionSkip, NextStep: args.Step, NextSendAt: &at}, nil
			}
		}
	}

	var sentAt *time.Time
	switch decision.Action {
	case reengagement.ActionSend:
		outcome, sent, err := sendReengagementEmail(ctx, row, reader, args.Step, decision, facts, now)
		if err != nil {
			reengagementEmailsTotal.WithLabelValues(args.Step, decision.Template, decision.Variant, "failed").Inc()
			model.RecordNetworkReengagementSendFailure(ctx, row.NetworkId, err.Error())
			return nil, err
		}
		// W1 goes out without its offer when none could be issued
		decision = sent
		result.Variant = decision.Variant
		reengagementEmailsTotal.WithLabelValues(args.Step, decision.Template, decision.Variant, outcome).Inc()
		if outcome == "sent" {
			sentAt = &now
		}
	case reengagement.ActionVirtual:
		writeReengagementEmailEvent(ctx, row, model.EventReengageVirtualSend, decision.Template, decision.Variant, args.Step, now)
		reengagementEmailsTotal.WithLabelValues(args.Step, decision.Template, decision.Variant, "virtual").Inc()
	default:
		reengagementEmailsTotal.WithLabelValues(args.Step, decision.Template, decision.Variant, "skipped").Inc()
	}
	if row.Stream == reengagement.StreamFix && decision.Action != reengagement.ActionSkip {
		model.MarkNetworkReengagementFixSent(ctx, row.NetworkId, now)
	}

	if decision.NextStep != "" {
		at := reengagement.SendAtZone(row.StartedAt, decision.NextDay, reader.SendZone, s, now)
		result.NextSendAt = &at
	} else {
		reengagementExitsTotal.WithLabelValues(row.Stream, reengagement.ExitDone).Inc()
		WriteServerEventWithContext(ctx, row.NetworkId, model.EventReengageExit, map[string]any{"stream": row.Stream, "reason": reengagement.ExitDone}, row.Platform)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		model.AdvanceNetworkReengagementInTx(tx, ctx, row.NetworkId, args.Step, sentAt, decision.NextStep, result.NextSendAt, now)
	})
	return result, nil
}

// ReengagementStepPost schedules the next step in the task's completion
// transaction.
func ReengagementStepPost(args *ReengagementStepArgs, result *ReengagementStepResult, clientSession *session.ClientSession, tx server.PgTx) error {
	if result != nil && result.NextStep != "" && result.NextSendAt != nil {
		scheduleReengagementStepInTx(tx, clientSession, args.NetworkId, args.Episode, result.NextStep, *result.NextSendAt)
	}
	return nil
}

// applyReengagementExit ends the episode and applies what the exit carries.
func applyReengagementExit(ctx context.Context, row *model.NetworkReengagement, reason string, suppress string, joinFix string, quiet bool, now time.Time) {
	s := model.Reengagement().ScheduleValue()
	if suppress != "" {
		SuppressNetworkMarketing(ctx, row.NetworkId, suppress, now)
	}
	if joinFix != "" {
		model.JoinNetworkReengagementFix(ctx, row.NetworkId, joinFix, row.Platform, now)
	}
	if quiet {
		model.SetNetworkReengagementQuiet(ctx, row.NetworkId, now.Add(time.Duration(s.QuietDays)*day))
	}
	if model.ExitNetworkReengagement(ctx, row.NetworkId, reason, now) {
		reengagementExitsTotal.WithLabelValues(row.Stream, reason).Inc()
		WriteServerEventWithContext(ctx, row.NetworkId, model.EventReengageExit, map[string]any{"stream": row.Stream, "reason": reason}, row.Platform)
	}
}

// loadReengagementFacts reads everything a decision looks at, as of now.
func loadReengagementFacts(ctx context.Context, row *model.NetworkReengagement, now time.Time) reengagement.Facts {
	cfg := model.Reengagement()
	_, userAuth, ok := model.GetNetworkAdminUserAuth(ctx, row.NetworkId)
	mail := model.GetNetworkMailState(ctx, row.NetworkId)
	onb := model.GetNetworkOnboarding(ctx, row.NetworkId)
	f := reengagement.Facts{
		Stream:         row.Stream,
		HasEmail:       ok && strings.Contains(userAuth, "@"),
		NetworkExists:  model.NetworkExists(ctx, row.NetworkId),
		ProductUpdates: mail.ProductUpdates,
		Suppressed:     mail.SuppressedAt != nil,
		Pro:            model.IsProNetwork(ctx, row.NetworkId),
		Holdout:        row.Variant == model.ExperimentVariantHoldout,
		PolicyExit:     reengagementPolicyExit(ctx, row.NetworkId, row.Stream, row.Country, now),
		SentLastYear:   model.CountNetworkReengagementSendsSince(ctx, row.NetworkId, now.Add(-365*day)),
		Reason:         row.Reason,
		Kept:           row.KeepUntil != nil && now.Before(*row.KeepUntil),
	}
	if onb != nil {
		f.Bounced = onb.Bounced
		f.Complained = onb.Complained
		f.OnboardingActive = !onb.Exited() && onb.NextStep != ""
	}
	switch row.Stream {
	case reengagement.StreamNeverActivated:
		_, f.Reactivated = model.NetworkConnectDays(ctx, row.NetworkId, time.Time{})
		f.OnboardingReason, _ = model.NetworkOnboardingAnswer(ctx, row.NetworkId)
	case reengagement.StreamLapsed:
		_, f.Reactivated = model.NetworkConnectDays(ctx, row.NetworkId, dayStart(row.StartedAt))
		if row.LastConnectAt != nil {
			f.Highlights = reengagement.NewsHighlights(cfg.ParsedHighlights(), row.Platform, row.Country, *row.LastConnectAt, now)
		}
	case reengagement.StreamFix:
		if row.FixReasonAt != nil {
			_, f.Reactivated = model.NetworkConnectDays(ctx, row.NetworkId, dayStart(*row.FixReasonAt).Add(day))
			platform := row.FixPlatform
			if platform == "" {
				platform = row.Platform
			}
			f.Highlights = reengagement.FixHighlights(cfg.ParsedHighlights(), platform, row.Country, row.FixReason, *row.FixReasonAt, now)
		}
	case reengagement.StreamWinback:
		// the comeback offer (Google Play and Stripe, the offer arm); the App
		// Store's is decided at the send, where Apple is asked
		f.OfferAllowed = winbackComebackOfferAllowed(winbackOfferFacts(ctx, row, now))
	}
	f.Clicked = model.NetworkHasEventSince(ctx, row.NetworkId, model.EventReengageLandingClicked, row.StartedAt)
	return f
}

// ----- sending -----

// ReengagementSendPlan is a fully resolved send.
type ReengagementSendPlan struct {
	Step       string            `json:"step"`
	Template   string            `json:"template"`
	Variant    string            `json:"variant"`
	Locale     string            `json:"locale"`
	TemplateId int               `json:"template_id"`
	Params     map[string]string `json:"params"`
	Tags       []string          `json:"tags"`
}

// reengagementProEndedAt is when Pro stopped for a win-back network: the end
// of its last supporter window, which already carries our 30-day grace on the
// markets that give one and the store's day of slack on the others. A gift
// year redeemed after the network's last paid window held Pro until its own
// end, so that is when Pro stopped.
func reengagementProEndedAt(ctx context.Context, networkId server.Id) *time.Time {
	end, _, ok := model.NetworkLastPaidWindow(ctx, networkId)
	if giftEnd, giftOk := model.NetworkLastGiftWindowEnd(ctx, networkId); giftOk && (!ok || end.Before(giftEnd)) {
		end, ok = giftEnd, true
	}
	if !ok {
		return nil
	}
	return &end
}

// buildReengagementSendPlan resolves a send, in the reader's language
// (the first of their languages a template serves, else their country's,
// else English) with dates in their zone. offer is the win-back offer W1
// issued for this send; without one, an offer variant (a dry run, a preview)
// shows the expiry an offer issued now would have.
func buildReengagementSendPlan(ctx context.Context, row *model.NetworkReengagement, reader mailReader, step string, decision reengagement.Decision, facts reengagement.Facts, offer *model.WinbackOffer, now time.Time) (*ReengagementSendPlan, error) {
	cfg := model.Reengagement()
	templateId, locale, _, ok := cfg.Brevo.TemplateIdForReader(decision.Template, decision.Variant, reader.Languages, model.Onboarding().EffectiveMailLanguageSettings())
	if !ok || templateId <= 0 {
		return nil, fmt.Errorf("no Brevo template for %s/%s/%s", decision.Template, decision.Variant, row.Locale)
	}
	token, err := onboarding.NewToken(&onboarding.TokenClaims{
		NetworkId: row.NetworkId,
		Step:      reengagement.CtaSlug(decision.Template, decision.Variant),
		FlowStep:  step,
		ExpiresAt: now.Add(30 * day).Unix(),
	})
	if err != nil {
		return nil, fmt.Errorf("mint landing token: %w", err)
	}
	in := reengagement.ParamsInput{
		Template:      decision.Template,
		Variant:       decision.Variant,
		Locale:        locale,
		Location:      reader.SendZone.DisplayLocation(),
		Now:           now,
		SiteUrl:       cfg.EffectiveSiteUrl(),
		Token:         token,
		CompanyLine:   cfg.EffectiveCompanyLine(),
		DailyDataGb:   float64(model.Pro().Free.Data) / float64(model.Gib),
		LastConnectAt: row.LastConnectAt,
		ReasonAt:      row.FixReasonAt,
		Highlights:    facts.Highlights,
		PqReady:       cfg.PqReady,
	}
	if decision.Template == reengagement.TemplateW1Winback {
		in.ProEndedAt = reengagementProEndedAt(ctx, row.NetworkId)
		switch {
		case offer != nil && offer.Store == reengagement.WinbackStoreApple:
			// Apple's offer: the App Store line, never a price or a date of ours
			in.AppleOfferHint = true
		case offer != nil:
			expiresAt := offer.ExpiresAt
			in.OfferExpiresAt = &expiresAt
		case decision.Variant == reengagement.VariantOffer:
			expiresAt := now.Add(cfg.WinbackOfferValidity())
			in.OfferExpiresAt = &expiresAt
		}
	}
	return &ReengagementSendPlan{
		Step:       step,
		Template:   decision.Template,
		Variant:    decision.Variant,
		Locale:     locale,
		TemplateId: templateId,
		Params:     reengagement.BuildParams(in),
		Tags:       onboarding.CampaignTags("reengagement", decision.Template, decision.Variant, row.ExperimentId, row.Variant),
	}, nil
}

// brevoSendReengagementTemplate posts one email; refuses while disabled so no
// path can bypass the switch.
func brevoSendReengagementTemplate(ctx context.Context, to string, plan *ReengagementSendPlan) (string, error) {
	if !ReengagementEnabled() {
		return "", fmt.Errorf("re-engagement is disabled (reengagement.yml enabled: false)")
	}
	cfg := model.Reengagement()
	return brevoPostSmtpEmail(ctx, &brevoSmtpEmailArgs{
		To:         []brevoEmailAddress{{Email: to}},
		TemplateId: plan.TemplateId,
		Params:     plan.Params,
		Tags:       plan.Tags,
		ReplyTo:    &brevoEmailAddress{Email: cfg.EffectiveReplyTo()},
		Headers:    map[string]string{"X-Mailin-custom": "reengagement:" + plan.Step + ":" + plan.Template + ":" + plan.Variant},
	})
}

// sendReengagementEmail renders and sends one step. Returns "sent" or
// "dry_run" and the decision the email went out with (W1 drops to its default
// variant when no offer could be issued); an error means retry. W1 issues its
// win-back offer here, after the one-a-week deferral and right before the
// send, so the offer's deadline starts with the email; a dry run issues
// nothing.
func sendReengagementEmail(ctx context.Context, row *model.NetworkReengagement, reader mailReader, step string, decision reengagement.Decision, facts reengagement.Facts, now time.Time) (string, reengagement.Decision, error) {
	_, userAuth, ok := model.GetNetworkAdminUserAuth(ctx, row.NetworkId)
	if !ok || !strings.Contains(userAuth, "@") {
		return "", decision, fmt.Errorf("network %s has no admin email", row.NetworkId)
	}
	if !ReengagementEnabled() {
		plan, err := buildReengagementSendPlan(ctx, row, reader, step, decision, facts, nil, now)
		if err != nil {
			return "", decision, err
		}
		glog.V(1).Infof("[reengagement]dry run: would send %s/%s/%s (template %d) to %s for network %s\n",
			plan.Template, plan.Variant, plan.Locale, plan.TemplateId, maskEmail(userAuth), row.NetworkId)
		return "dry_run", decision, nil
	}
	var offer *model.WinbackOffer
	if decision.Template == reengagement.TemplateW1Winback {
		var err error
		offer, decision, err = issueWinbackOfferForSend(ctx, row, decision, now)
		if err != nil {
			return "", decision, err
		}
	}
	plan, err := buildReengagementSendPlan(ctx, row, reader, step, decision, facts, offer, now)
	if err != nil {
		return "", decision, err
	}
	messageId, err := brevoSendReengagementTemplate(ctx, userAuth, plan)
	if err != nil {
		return "", decision, err
	}
	server.Tx(ctx, func(tx server.PgTx) {
		model.AddNetworkReengagementEmailInTx(tx, ctx, &model.NetworkReengagementEmail{
			MessageId:         messageId,
			NetworkId:         row.NetworkId,
			Step:              step,
			Template:          plan.Template,
			Variant:           plan.Variant,
			Experiment:        row.ExperimentId,
			ExperimentVariant: row.Variant,
			TemplateId:        plan.TemplateId,
			Locale:            plan.Locale,
			SentAt:            now,
		})
	})
	writeReengagementEmailEvent(ctx, row, model.EventReengageEmailSent, plan.Template, plan.Variant, step, now)
	return "sent", decision, nil
}

func writeReengagementEmailEvent(ctx context.Context, row *model.NetworkReengagement, name string, template string, variant string, step string, at time.Time) {
	props := map[string]any{"step": template, "flow_step": step, "variant": variant}
	if row.ExperimentId != "" {
		props["experiment"] = row.ExperimentId
		props["arm"] = row.Variant
	}
	validated, err := model.ValidateServerEvent(name, props)
	if err != nil {
		glog.Errorf("[reengagement]refusing %s: %s\n", name, err)
		return
	}
	event := &model.OnboardingEvent{
		NetworkId:  row.NetworkId,
		Name:       name,
		At:         at,
		ReceivedAt: server.NowUtc(),
		Platform:   row.Platform,
		Experiment: row.ExperimentId,
		Variant:    row.Variant,
		Props:      validated,
	}
	if err := model.AddOnboardingEvent(ctx, event); err != nil {
		glog.Errorf("[reengagement]could not write %s for network %s: %s\n", name, row.NetworkId, err)
	}
}

// ----- the webhook -----

func reengagementEventForWebhook(event string) string {
	switch onboarding.EventForWebhook(event) {
	case model.EventEmailDelivered:
		return model.EventReengageEmailDelivered
	case model.EventEmailClicked:
		return model.EventReengageEmailClicked
	case model.EventEmailBounced:
		return model.EventReengageEmailBounced
	case model.EventEmailComplained:
		return model.EventReengageEmailComplained
	case model.EventEmailUnsubscribed:
		return model.EventReengageEmailUnsubscribed
	}
	// opens are not recorded: they decide nothing (Apple Mail Privacy
	// Protection) and in the EU they need consent
	return ""
}

// handleReengagementBrevoWebhook records a delivery event against the send it
// belongs to. A bounce or complaint suppresses every campaign; an
// unsubscribe ends the episode (the caller turns product updates off). An
// event of a re-engagement send that names no recorded send is only counted
// by template (urnetwork_reengagement_webhook_unattributed_events_total):
// Brevo's anonymous clicks and spam reports, which are never looked up, and
// ids that no send recorded.
func handleReengagementBrevoWebhook(ctx context.Context, args *BrevoWebhookArgs) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[reengagement]webhook %s failed: %v\n", args.Event, r)
		}
	}()
	name := reengagementEventForWebhook(args.Event)
	if name == "" {
		return
	}
	if brevoWebhookAnonymous(args) {
		if template, ok := brevoWebhookCampaignTemplate(args, "reengagement"); ok {
			reengagementWebhookUnattributedEventsTotal.WithLabelValues(name, template, brevoWebhookUnattributedAnonymous).Inc()
			if name == model.EventReengageEmailComplained {
				reportAnonymousBrevoComplaint("reengagement", template)
			}
		}
		return
	}
	email := model.GetNetworkReengagementEmail(ctx, args.MessageId)
	if email == nil {
		if template, ok := brevoWebhookCampaignTemplate(args, "reengagement"); ok {
			reengagementWebhookUnattributedEventsTotal.WithLabelValues(name, template, brevoWebhookUnattributedUnknownSend).Inc()
		}
		return
	}
	at := server.NowUtc()
	if 0 < args.TsEvent {
		at = time.Unix(args.TsEvent, 0).UTC()
	}
	reengagementWebhookEventsTotal.WithLabelValues(name, email.Template).Inc()
	row := model.GetNetworkReengagement(ctx, email.NetworkId)
	if row == nil {
		row = &model.NetworkReengagement{NetworkId: email.NetworkId}
	}
	eventRow := *row
	eventRow.ExperimentId, eventRow.Variant = email.Experiment, email.ExperimentVariant
	writeReengagementEmailEvent(ctx, &eventRow, name, email.Template, email.Variant, email.Step, at)
	switch name {
	case model.EventReengageEmailBounced:
		SuppressNetworkMarketing(ctx, email.NetworkId, reengagement.ExitBounced, at)
		applyReengagementExit(ctx, row, reengagement.ExitBounced, "", "", false, at)
	case model.EventReengageEmailComplained:
		SuppressNetworkMarketing(ctx, email.NetworkId, reengagement.ExitComplained, at)
		applyReengagementExit(ctx, row, reengagement.ExitComplained, "", "", false, at)
	case model.EventReengageEmailUnsubscribed:
		applyReengagementExit(ctx, row, reengagement.ExitOptOut, "", "", false, at)
	}
}

// ----- landing clicks and one-tap answers (onboarding_controller.go routes
// tokens with a re-engagement flow step here) -----

// recordReengagementClick records a landing click; the keep page's click is
// the keep-or-go answer.
func recordReengagementClick(clientSession *session.ClientSession, claims *onboarding.TokenClaims, slug string) {
	ctx := clientSession.Ctx
	now := server.NowUtc()
	WriteServerEvent(clientSession, claims.NetworkId, model.EventReengageLandingClicked, map[string]any{
		"step":      slug,
		"flow_step": claims.FlowStep,
	}, "")
	if slug == onboarding.SlugKeep {
		s := model.Reengagement().ScheduleValue()
		model.SetNetworkReengagementKeep(ctx, claims.NetworkId, now.Add(time.Duration(s.KeepDays)*day))
		WriteServerEvent(clientSession, claims.NetworkId, model.EventReengageKeep, map[string]any{"flow_step": claims.FlowStep}, "")
	}
}

// recordReengagementAnswer records an R2 answer and routes the network.
func recordReengagementAnswer(clientSession *session.ClientSession, claims *onboarding.TokenClaims, reason string) {
	ctx := clientSession.Ctx
	now := server.NowUtc()
	WriteServerEvent(clientSession, claims.NetworkId, model.EventReengageReason, map[string]any{
		"reason":    reason,
		"flow_step": claims.FlowStep,
	}, "")
	row := model.GetNetworkReengagement(ctx, claims.NetworkId)
	if row == nil {
		return
	}
	model.SetNetworkReengagementAnswer(ctx, claims.NetworkId, reason, now)
	answer := reengagement.RouteAnswer(reason)
	if answer.ExitReason != "" {
		applyReengagementExit(ctx, row, answer.ExitReason, answer.Suppress, answer.JoinFix, answer.Quiet, now)
	}
}

// ----- tooling (bringyourctl reengagement) -----

type ReengagementStatus struct {
	Row       *model.NetworkReengagement        `json:"row"`
	MailState *model.NetworkMailState           `json:"mail_state"`
	Emails    []*model.NetworkReengagementEmail `json:"emails"`
	Facts     *reengagement.Facts               `json:"facts,omitempty"`
	// the context a scan would open an episode in now, with its sources
	Context ReengagementContext `json:"context"`
	// what the network's apps and browsers last reported
	MailContext *model.NetworkMailContext `json:"mail_context,omitempty"`
}

func ReengagementNetworkStatus(ctx context.Context, networkId server.Id) *ReengagementStatus {
	status := &ReengagementStatus{
		Row:         model.GetNetworkReengagement(ctx, networkId),
		MailState:   model.GetNetworkMailState(ctx, networkId),
		Emails:      model.ListNetworkReengagementEmails(ctx, networkId),
		MailContext: model.GetNetworkMailContext(ctx, networkId),
	}
	var lastConnect *time.Time
	if status.Row != nil {
		lastConnect = status.Row.LastConnectAt
	}
	status.Context = reengagementContext(ctx, networkId, lastConnect)
	if status.Row != nil {
		facts := loadReengagementFacts(ctx, status.Row, server.NowUtc())
		status.Facts = &facts
	}
	return status
}

type ReengagementPreview struct {
	Decision reengagement.Decision `json:"decision"`
	Plan     *ReengagementSendPlan `json:"plan,omitempty"`
	Error    string                `json:"error,omitempty"`
}

// ReengagementNetworkPreview renders the decision a step would make now and,
// when it would send, the plan. Nothing is sent.
func ReengagementNetworkPreview(ctx context.Context, networkId server.Id, step string) (*ReengagementPreview, error) {
	row := model.GetNetworkReengagement(ctx, networkId)
	if row == nil {
		return nil, fmt.Errorf("network %s has no re-engagement row", networkId)
	}
	now := server.NowUtc()
	facts := loadReengagementFacts(ctx, row, now)
	decision := reengagement.Decide(step, facts, model.Reengagement().ScheduleValue())
	preview := &ReengagementPreview{Decision: decision}
	if decision.Action == reengagement.ActionSend || decision.Action == reengagement.ActionVirtual {
		plan, err := buildReengagementSendPlan(ctx, row, reengagementMailReader(ctx, row), step, decision, facts, nil, now)
		if err != nil {
			preview.Error = err.Error()
		} else {
			preview.Plan = plan
		}
	}
	return preview, nil
}

// ReengagementSendTest sends ONE template to an explicit address with sample
// data. Behind `enabled`; records nothing.
func ReengagementSendTest(ctx context.Context, to string, template string, variant string, locale string) (string, error) {
	if !ReengagementEnabled() {
		return "", fmt.Errorf("re-engagement is disabled (reengagement.yml enabled: false)")
	}
	if variant == "" {
		variant = reengagement.VariantDefault
	}
	if locale == "" {
		locale = "en"
	}
	cfg := model.Reengagement()
	templateId, matched, ok := cfg.Brevo.TemplateIdLocale(template, variant, locale)
	if !ok {
		return "", fmt.Errorf("no Brevo template for %s/%s/%s", template, variant, locale)
	}
	now := server.NowUtc()
	last, answered, ended, expires := now.Add(-35*day), now.Add(-20*day), now.Add(-40*day), now.Add(14*day)
	plan := &ReengagementSendPlan{
		Step: "test", Template: template, Variant: variant, Locale: matched, TemplateId: templateId,
		Params: reengagement.BuildParams(reengagement.ParamsInput{
			Template: template, Variant: variant, Locale: matched, Location: time.UTC, Now: now,
			SiteUrl: cfg.EffectiveSiteUrl(), Token: "test", CompanyLine: cfg.EffectiveCompanyLine(),
			DailyDataGb:   float64(model.Pro().Free.Data) / float64(model.Gib),
			LastConnectAt: &last, ReasonAt: &answered, ProEndedAt: &ended, OfferExpiresAt: &expires,
			Highlights: reengagement.TemplateHighlights, PqReady: cfg.PqReady,
		}),
		Tags: []string{"reengagement", "test", template, variant},
	}
	return brevoSendReengagementTemplate(ctx, to, plan)
}
