package model

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/onboarding"
)

// TestOnboardingEmailTrackerAttribution is the synthetic root-cause test for
// the E3/E5 collision: both can use e3_last_chance, so only an explicit
// flow_step may attribute their engagement. Webhook retries count one network,
// duplicate accepted sends count one exposure, and product activity belongs to
// the most recent send window.
func TestOnboardingEmailTrackerAttribution(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		networkA := server.NewId()
		networkB := server.NewId()
		networkC := server.NewId()
		networkD := server.NewId()

		emails := []*NetworkOnboardingEmail{
			{MessageId: "synthetic-a-e3", NetworkId: networkA, Step: onboarding.StepE3, Template: onboarding.TemplateE3LastChance, Variant: onboarding.VariantEngaged, SentAt: day.Add(8 * time.Hour)},
			{MessageId: "synthetic-a-e5", NetworkId: networkA, Step: onboarding.StepE5, Template: onboarding.TemplateE3LastChance, Variant: onboarding.VariantEngaged, SentAt: day.Add(4*24*time.Hour + 8*time.Hour)},
			{MessageId: "synthetic-b-e1", NetworkId: networkB, Step: onboarding.StepE1, Template: onboarding.TemplateE1Connect, Variant: onboarding.VariantDefault, SentAt: day.Add(24*time.Hour + 8*time.Hour)},
			// A provider retry can yield another accepted message id. The
			// tracker still counts this network/flow-step once.
			{MessageId: "synthetic-b-e1-retry", NetworkId: networkB, Step: onboarding.StepE1, Template: onboarding.TemplateE1Connect, Variant: onboarding.VariantDefault, SentAt: day.Add(24*time.Hour + 9*time.Hour)},
			{MessageId: "synthetic-c-e3", NetworkId: networkC, Step: onboarding.StepE3, Template: onboarding.TemplateE3LastChance, Variant: onboarding.VariantNotActivated, SentAt: day.Add(2*24*time.Hour + 8*time.Hour)},
			{MessageId: "synthetic-c-e5", NetworkId: networkC, Step: onboarding.StepE5, Template: onboarding.TemplateE3LastChance, Variant: onboarding.VariantNotActivated, SentAt: day.Add(6*24*time.Hour + 8*time.Hour)},
			{MessageId: "synthetic-d-e2", NetworkId: networkD, Step: onboarding.StepE2, Template: onboarding.TemplateE2Widget, Variant: onboarding.VariantDefault, SentAt: day.Add(24*time.Hour + 8*time.Hour)},
		}
		server.Tx(ctx, func(tx server.PgTx) {
			for _, email := range emails {
				AddNetworkOnboardingEmailInTx(tx, ctx, email)
			}
		})

		event := func(networkId server.Id, name string, at time.Time, props map[string]any) *OnboardingEvent {
			return &OnboardingEvent{NetworkId: networkId, Name: name, At: at, ReceivedAt: at, Props: props}
		}
		err := AddOnboardingEvents(ctx, []*OnboardingEvent{
			// Exact flow attribution remains correct even after the next send.
			event(networkA, EventEmailOpened, day.Add(5*24*time.Hour), map[string]any{"step": onboarding.TemplateE3LastChance, "flow_step": onboarding.StepE3}),
			event(networkA, EventEmailClicked, day.Add(5*24*time.Hour), map[string]any{"step": onboarding.TemplateE3LastChance, "flow_step": onboarding.StepE5}),
			event(networkA, EventConnectDay, day.Add(2*24*time.Hour), nil),
			event(networkA, EventWidgetAdded, day.Add(5*24*time.Hour), map[string]any{"kind": "synthetic_widget"}),

			// A unique legacy template remains attributable, but webhook
			// duplicates still count one network.
			event(networkB, EventEmailOpened, day.Add(2*24*time.Hour), map[string]any{"step": onboarding.TemplateE1Connect}),
			event(networkB, EventEmailOpened, day.Add(2*24*time.Hour+time.Minute), map[string]any{"step": onboarding.TemplateE1Connect}),
			event(networkB, EventLandingClicked, day.Add(2*24*time.Hour), map[string]any{"step": onboarding.TemplateE1Connect, "flow_step": onboarding.StepE1}),
			event(networkB, EventPurchaseCompleted, day.Add(2*24*time.Hour), map[string]any{"store": OnboardingStorePlay}),
			// Provider delivery corrections remain attributable for 30 days,
			// while product engagement after the 14-day ceiling does not.
			event(networkB, EventEmailDelivered, day.Add(20*24*time.Hour), map[string]any{"step": onboarding.TemplateE1Connect, "flow_step": onboarding.StepE1}),
			event(networkB, EventConnectDay, day.Add(20*24*time.Hour), nil),

			// This legacy event could mean E3 or E5. It must be surfaced as
			// ambiguous and must not inflate either opened count.
			event(networkC, EventEmailOpened, day.Add(4*24*time.Hour), map[string]any{"step": onboarding.TemplateE3LastChance}),

			// App-open attribution must preserve the landing token's exact
			// flow step rather than falling back to the shared template.
			event(networkD, EventLandingClicked, day.Add(2*24*time.Hour), map[string]any{"step": onboarding.TemplateE2Widget, "flow_step": onboarding.StepE2}),
		})
		connect.AssertEqual(t, nil, err)
		connect.AssertEqual(t, true, AttributeAppOpen(ctx, networkD, day.Add(2*24*time.Hour+time.Minute)))
		connect.AssertEqual(t, false, AttributeAppOpen(ctx, networkD, day.Add(2*24*time.Hour+2*time.Minute)))

		computedAt := day.Add(30 * 24 * time.Hour)
		rows, err := RebuildOnboardingEmailTracker(ctx, day, day.Add(9*24*time.Hour), computedAt)
		connect.AssertEqual(t, nil, err)
		connect.AssertEqual(t, int64(6), rows)

		totals := SumOnboardingEmailTracker(ctx, day, day.Add(9*24*time.Hour))
		e1 := totals[onboarding.StepE1]
		connect.AssertEqual(t, int64(1), e1.Sent)
		connect.AssertEqual(t, int64(1), e1.Delivered)
		connect.AssertEqual(t, int64(1), e1.Opened)
		connect.AssertEqual(t, int64(1), e1.LandingClicked)
		connect.AssertEqual(t, int64(1), e1.ProStarted)
		connect.AssertEqual(t, int64(1), e1.Engaged)
		connect.AssertEqual(t, int64(0), e1.Connected)
		e2 := totals[onboarding.StepE2]
		connect.AssertEqual(t, int64(1), e2.Sent)
		connect.AssertEqual(t, int64(1), e2.AppOpened)
		connect.AssertEqual(t, int64(1), e2.Engaged)

		e3 := totals[onboarding.StepE3]
		connect.AssertEqual(t, int64(2), e3.Sent)
		connect.AssertEqual(t, int64(1), e3.Opened)
		connect.AssertEqual(t, int64(1), e3.Connected)
		connect.AssertEqual(t, int64(1), e3.Engaged)
		connect.AssertEqual(t, int64(1), e3.AttributionAmbiguous)

		e5 := totals[onboarding.StepE5]
		connect.AssertEqual(t, int64(2), e5.Sent)
		connect.AssertEqual(t, int64(1), e5.Clicked)
		connect.AssertEqual(t, int64(1), e5.WidgetAdded)
		connect.AssertEqual(t, int64(1), e5.Engaged)
		connect.AssertEqual(t, int64(0), e5.AttributionAmbiguous)

		// Rebuilding is replacement, not accumulation.
		_, err = RebuildOnboardingEmailTracker(ctx, day, day.Add(9*24*time.Hour), computedAt.Add(time.Minute))
		connect.AssertEqual(t, nil, err)
		connect.AssertEqual(t, int64(1), SumOnboardingEmailTracker(ctx, day, day.Add(9*24*time.Hour))[onboarding.StepE1].Sent)

		page, next := ListOnboardingEmailTracker(ctx, OnboardingEmailTrackerFilter{From: day, To: day.Add(9 * 24 * time.Hour), Limit: 1})
		connect.AssertEqual(t, 1, len(page))
		connect.AssertEqual(t, true, next != nil)
		page2, _ := ListOnboardingEmailTracker(ctx, OnboardingEmailTrackerFilter{From: day, To: day.Add(9 * 24 * time.Hour), After: next, Limit: 10})
		connect.AssertEqual(t, 5, len(page2))
	})
}

// The rebuild as it was before feedback and subscription starts moved to semi
// joins, kept verbatim as the oracle for that rewrite. It probes
// account_feedback and subscription_renewal once per send, and again inside
// engaged because classified is inlined.
const onboardingEmailTrackerReferenceRebuildSql = `
	WITH candidate_sends AS MATERIALIZED (
		SELECT
			email.message_id, email.network_id, email.step, email.template,
			email.variant, email.experiment, email.experiment_variant,
			email.sent_at
		FROM network_onboarding_email AS email
		WHERE email.sent_at >= $1 AND email.sent_at < $2
			AND email.step IN ('e1', 'e2', 'e3', 'e4', 'e5')
			AND NOT EXISTS (
				SELECT 1
				FROM network_onboarding_email AS earlier
				WHERE earlier.network_id = email.network_id
					AND earlier.step = email.step
					AND (earlier.sent_at, earlier.message_id) < (email.sent_at, email.message_id)
			)
	),
	sequenced AS MATERIALIZED (
		SELECT
			email.*,
			(
				SELECT later.sent_at
				FROM network_onboarding_email AS later
				WHERE later.network_id = email.network_id
					AND later.step <> email.step
					AND (later.sent_at, later.step) > (email.sent_at, email.step)
				ORDER BY later.sent_at, later.step
				LIMIT 1
			) AS next_sent_at,
			EXISTS (
				SELECT 1
				FROM network_onboarding_email AS shared
				WHERE shared.network_id = email.network_id
					AND shared.template = email.template
					AND shared.step <> email.step
			) AS template_ambiguous
		FROM candidate_sends AS email
	),
	sends AS MATERIALIZED (
		SELECT
			email.*,
			COALESCE(campaign.platform, '') AS platform,
			COALESCE(campaign.path, '') AS path,
			LEAST(
				COALESCE(email.next_sent_at, email.sent_at + INTERVAL '14 days'),
				email.sent_at + INTERVAL '14 days'
			) AS engagement_ends_at,
			LEAST(
				COALESCE(email.next_sent_at, email.sent_at + INTERVAL '30 days'),
				email.sent_at + INTERVAL '30 days'
			) AS legacy_attribution_ends_at
		FROM sequenced AS email
		LEFT JOIN network_onboarding AS campaign USING (network_id)
	),
	classified AS (
		SELECT
			sends.*,
			events.delivered,
			events.opened,
			events.clicked,
			events.landing_clicked,
			events.app_opened,
			events.connected,
			events.widget_added,
			(events.feedback_submitted OR EXISTS (
				SELECT 1 FROM account_feedback AS feedback
				WHERE feedback.network_id = sends.network_id
					AND feedback.feedback_time >= sends.sent_at
					AND feedback.feedback_time < sends.engagement_ends_at
			)) AS feedback_submitted,
			(events.pro_started OR EXISTS (
				SELECT 1 FROM subscription_renewal AS renewal
				WHERE renewal.network_id = sends.network_id
					AND renewal.start_time >= sends.sent_at
					AND renewal.start_time < sends.engagement_ends_at
			)) AS pro_started,
			events.bounced,
			events.unsubscribed,
			events.complained,
			events.attribution_ambiguous
		FROM sends
		CROSS JOIN LATERAL (
			SELECT
				COALESCE(BOOL_OR(event.name = 'email.delivered' AND event.step_attributed), false) AS delivered,
				COALESCE(BOOL_OR(event.name = 'email.opened' AND event.step_attributed), false) AS opened,
				COALESCE(BOOL_OR(event.name = 'email.clicked' AND event.step_attributed), false) AS clicked,
				COALESCE(BOOL_OR(event.name = 'landing.clicked' AND event.step_attributed), false) AS landing_clicked,
				COALESCE(BOOL_OR(event.name = 'app.opened' AND event.step_attributed), false) AS app_opened,
				COALESCE(BOOL_OR(event.name IN ('connect.first', 'connect.day') AND event.at < sends.engagement_ends_at), false) AS connected,
				COALESCE(BOOL_OR(event.name = 'widget.added' AND event.at < sends.engagement_ends_at), false) AS widget_added,
				COALESCE(BOOL_OR(event.name = 'feedback.submitted' AND event.at < sends.engagement_ends_at), false) AS feedback_submitted,
				COALESCE(BOOL_OR(event.name = 'purchase.completed' AND event.at < sends.engagement_ends_at), false) AS pro_started,
				COALESCE(BOOL_OR(event.name = 'email.bounced' AND event.step_attributed), false) AS bounced,
				COALESCE(BOOL_OR(event.name = 'email.unsubscribed' AND event.step_attributed), false) AS unsubscribed,
				COALESCE(BOOL_OR(event.name = 'email.complained' AND event.step_attributed), false) AS complained,
				COALESCE(BOOL_OR(event.legacy_ambiguous), false) AS attribution_ambiguous
			FROM (
				SELECT
					raw.name,
					raw.at,
					(
						raw.props->>'flow_step' = sends.step OR (
							COALESCE(raw.props->>'flow_step', '') = '' AND
							NOT sends.template_ambiguous AND
							raw.props->>'step' = sends.template
						)
					) AS step_attributed,
					(
						raw.name IN (
							'email.delivered', 'email.opened', 'email.clicked',
							'email.bounced', 'email.unsubscribed', 'email.complained',
							'landing.clicked', 'app.opened'
						) AND
						COALESCE(raw.props->>'flow_step', '') = '' AND
						sends.template_ambiguous AND
						raw.props->>'step' = sends.template AND
						raw.at < sends.legacy_attribution_ends_at
					) AS legacy_ambiguous
				FROM network_onboarding_event AS raw
				WHERE raw.network_id = sends.network_id
					AND raw.at >= sends.sent_at
					AND raw.at < sends.sent_at + INTERVAL '30 days'
					AND raw.name IN (
						'email.delivered', 'email.opened', 'email.clicked',
						'email.bounced', 'email.unsubscribed', 'email.complained',
						'landing.clicked', 'app.opened', 'connect.first',
						'connect.day', 'widget.added', 'feedback.submitted',
						'purchase.completed'
					)
			) AS event
		) AS events
	),
	aggregated AS (
		SELECT
			date_trunc('day', sent_at) AS send_day,
			step, template, variant, experiment, experiment_variant, platform, path,
			COUNT(*) AS sent,
			COUNT(*) FILTER (WHERE delivered) AS delivered,
			COUNT(*) FILTER (WHERE opened) AS opened,
			COUNT(*) FILTER (WHERE clicked) AS clicked,
			COUNT(*) FILTER (WHERE landing_clicked) AS landing_clicked,
			COUNT(*) FILTER (WHERE app_opened) AS app_opened,
			COUNT(*) FILTER (WHERE connected) AS connected,
			COUNT(*) FILTER (WHERE widget_added) AS widget_added,
			COUNT(*) FILTER (WHERE feedback_submitted) AS feedback_submitted,
			COUNT(*) FILTER (WHERE pro_started) AS pro_started,
			COUNT(*) FILTER (WHERE
				clicked OR landing_clicked OR app_opened OR connected OR
				widget_added OR feedback_submitted OR pro_started
			) AS engaged,
			COUNT(*) FILTER (WHERE bounced) AS bounced,
			COUNT(*) FILTER (WHERE unsubscribed) AS unsubscribed,
			COUNT(*) FILTER (WHERE complained) AS complained,
			COUNT(*) FILTER (WHERE attribution_ambiguous) AS attribution_ambiguous
		FROM classified
		GROUP BY send_day, step, template, variant, experiment,
			experiment_variant, platform, path
	)
	INSERT INTO onboarding_email_tracker_daily (
		send_day, step, template, variant, experiment, experiment_variant,
		platform, path, sent, delivered, opened, clicked, landing_clicked,
		app_opened, connected, widget_added, feedback_submitted, pro_started,
		engaged, bounced, unsubscribed, complained, attribution_ambiguous,
		computed_at
	)
	SELECT
		send_day, step, template, variant, experiment, experiment_variant,
		platform, path, sent, delivered, opened, clicked, landing_clicked,
		app_opened, connected, widget_added, feedback_submitted, pro_started,
		engaged, bounced, unsubscribed, complained, attribution_ambiguous, $3
	FROM aggregated
`

// A seeded synthetic history around a ten-day send window: sends that repeat,
// tie, skip steps, share the E3/E5 template and straddle the window, legacy and
// exact events of every tracked name, and feedback and subscription starts,
// half of them placed on a send, engagement or correction boundary.
type onboardingEmailTrackerFixture struct {
	from          time.Time
	to            time.Time
	computedAt    time.Time
	emailCount    int
	feedbackCount int
	renewalCount  int
}

func addOnboardingEmailTrackerFixture(t testing.TB, ctx context.Context, seed uint64, day time.Time) *onboardingEmailTrackerFixture {
	t.Helper()
	const networkCount = 48
	const noiseFeedbackCount = 64
	r := rand.New(rand.NewPCG(seed, 0x6f6e626f61726469))
	pick := func(values ...string) string {
		return values[r.IntN(len(values))]
	}
	stepTemplates := map[string]string{
		onboarding.StepE1: onboarding.TemplateE1Connect,
		onboarding.StepE2: onboarding.TemplateE2Widget,
		onboarding.StepE3: onboarding.TemplateE3LastChance,
		onboarding.StepE4: onboarding.TemplateE4Feedback,
		onboarding.StepE5: onboarding.TemplateE3LastChance,
	}
	stepGaps := []time.Duration{0, time.Hour, 24 * time.Hour, 2 * 24 * time.Hour, 4 * 24 * time.Hour, 9 * 24 * time.Hour, 16 * 24 * time.Hour}
	eventNames := []string{
		EventEmailDelivered, EventEmailOpened, EventEmailClicked, EventEmailBounced,
		EventEmailUnsubscribed, EventEmailComplained, EventLandingClicked, EventAppOpened,
		EventConnectFirst, EventConnectDay, EventWidgetAdded, EventFeedbackSubmitted,
		EventPurchaseCompleted, EventRetentionD7,
	}

	fixture := &onboardingEmailTrackerFixture{
		from:       day,
		to:         day.Add(9 * 24 * time.Hour),
		computedAt: day.Add(60 * 24 * time.Hour),
	}
	emails := []*NetworkOnboardingEmail{}
	events := []*OnboardingEvent{}
	// one account_feedback or subscription_renewal row
	type sourceRow struct {
		networkId server.Id
		at        time.Time
	}
	feedbackRows := []sourceRow{}
	renewalRows := []sourceRow{}
	for i := range networkCount {
		networkId := server.NewId()
		if r.IntN(5) != 0 {
			CreateNetworkOnboarding(ctx, &NetworkOnboarding{
				NetworkId: networkId,
				CreatedAt: day.Add(-30 * 24 * time.Hour),
				Path:      pick("", onboarding.PathA, onboarding.PathB),
				Email:     true,
				Platform:  pick("", "android", "ios"),
			})
		}

		// the first step lands from 8 days before the window to 2 days past it
		sentAt := day.Add(time.Duration(r.IntN(20*24)-8*24) * time.Hour)
		networkEmails := []*NetworkOnboardingEmail{}
		for _, step := range onboarding.FlowSteps() {
			if r.IntN(7) == 0 {
				continue
			}
			template := stepTemplates[step]
			if step == onboarding.StepE4 && r.IntN(2) == 0 {
				template = onboarding.TemplateE4bOffer
			}
			if step == onboarding.StepE5 && r.IntN(3) == 0 {
				template = "e5_synthetic"
			}
			email := &NetworkOnboardingEmail{
				MessageId:         fmt.Sprintf("synthetic-%d-%02d-%s", seed, i, step),
				NetworkId:         networkId,
				Step:              step,
				Template:          template,
				Variant:           pick(onboarding.VariantDefault, onboarding.VariantActivated, onboarding.VariantNotActivated, onboarding.VariantEngaged),
				Experiment:        pick("", "synthetic_sequence"),
				ExperimentVariant: pick("", "control", "treatment"),
				SentAt:            sentAt,
			}
			networkEmails = append(networkEmails, email)
			if r.IntN(6) == 0 {
				// a provider retry at the same instant (the message id breaks
				// the tie) or an hour later
				retry := *email
				retry.MessageId = email.MessageId + "-retry"
				if r.IntN(2) == 0 {
					retry.SentAt = sentAt.Add(time.Hour)
				}
				networkEmails = append(networkEmails, &retry)
			}
			sentAt = sentAt.Add(stepGaps[r.IntN(len(stepGaps))])
		}
		if len(networkEmails) == 0 {
			continue
		}
		emails = append(emails, networkEmails...)

		instants := []time.Time{}
		for _, email := range networkEmails {
			for _, offset := range []time.Duration{0, 14 * 24 * time.Hour, 30 * 24 * time.Hour} {
				instants = append(instants, email.SentAt.Add(offset), email.SentAt.Add(offset-time.Microsecond))
			}
		}
		at := func() time.Time {
			if r.IntN(2) == 0 {
				return instants[r.IntN(len(instants))]
			}
			return networkEmails[0].SentAt.Add(time.Duration(r.IntN(40*24*60)-2*24*60) * time.Minute)
		}
		for range r.IntN(10) {
			email := networkEmails[r.IntN(len(networkEmails))]
			var props map[string]any
			switch r.IntN(5) {
			case 1:
				props = map[string]any{"step": email.Template}
			case 2:
				props = map[string]any{"step": email.Template, "flow_step": ""}
			case 3:
				props = map[string]any{"step": email.Template, "flow_step": email.Step}
			case 4:
				props = map[string]any{"flow_step": pick(onboarding.FlowSteps()...)}
			}
			eventAt := at()
			events = append(events, &OnboardingEvent{
				NetworkId:  networkId,
				Name:       eventNames[r.IntN(len(eventNames))],
				At:         eventAt,
				ReceivedAt: eventAt,
				Props:      props,
			})
		}
		for range r.IntN(4) {
			feedbackRows = append(feedbackRows, sourceRow{networkId: networkId, at: at()})
		}
		for range r.IntN(3) {
			renewalRows = append(renewalRows, sourceRow{networkId: networkId, at: at()})
		}
	}
	for range noiseFeedbackCount {
		feedbackRows = append(feedbackRows, sourceRow{
			networkId: server.NewId(),
			at:        day.Add(time.Duration(r.IntN(10*24*60)) * time.Minute),
		})
	}

	server.Tx(ctx, func(tx server.PgTx) {
		for _, email := range emails {
			AddNetworkOnboardingEmailInTx(tx, ctx, email)
		}
		for _, feedback := range feedbackRows {
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO account_feedback (feedback_id, network_id, user_id, feedback_time)
				VALUES ($1, $2, $3, $4)
			`, server.NewId(), feedback.networkId, server.NewId(), feedback.at))
		}
		for _, renewal := range renewalRows {
			tag := server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO subscription_renewal (
					network_id, subscription_type, start_time, end_time, net_revenue_nano_cents
				)
				VALUES ($1, 'supporter', $2, $3, 0)
				ON CONFLICT DO NOTHING
			`, renewal.networkId, renewal.at, renewal.at.Add(30*24*time.Hour)))
			fixture.renewalCount += int(tag.RowsAffected())
		}
	})
	connect.AssertEqual(t, nil, AddOnboardingEvents(ctx, events))
	fixture.emailCount = len(emails)
	fixture.feedbackCount = len(feedbackRows)
	return fixture
}

// Matching feedback and subscription starts with semi joins must store exactly
// the rows the per-send probes stored, across seeded histories that put
// feedback, renewals and events on every attribution boundary.
func TestOnboardingEmailTrackerRebuildMatchesReference(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		// every stored row of the window in key order, one comparable line each
		trackerLines := func(fixture *onboardingEmailTrackerFixture) []string {
			rows, next := ListOnboardingEmailTracker(ctx, OnboardingEmailTrackerFilter{
				From:  fixture.from,
				To:    fixture.to,
				Limit: 2000,
			})
			connect.AssertEqual(t, true, next == nil)
			lines := []string{}
			for _, row := range rows {
				lines = append(lines, fmt.Sprintf(
					"%s %s %s %s %q %q %q %q sent=%d delivered=%d opened=%d clicked=%d landing=%d app=%d connected=%d widget=%d feedback=%d pro=%d engaged=%d bounced=%d unsubscribed=%d complained=%d ambiguous=%d computed=%s",
					row.SendDayStr, row.Step, row.Template, row.Variant, row.Experiment,
					row.ExperimentVariant, row.Platform, row.Path, row.Sent, row.Delivered,
					row.Opened, row.Clicked, row.LandingClicked, row.AppOpened, row.Connected,
					row.WidgetAdded, row.FeedbackSubmitted, row.ProStarted, row.Engaged,
					row.Bounced, row.Unsubscribed, row.Complained, row.AttributionAmbiguous,
					row.ComputedAt.UTC().Format(time.RFC3339Nano),
				))
			}
			return lines
		}
		outcomeCounts := map[string]int64{}
		for _, seed := range []uint64{1, 2, 3} {
			// windows 200 days apart share no network, send or event
			day := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC).Add(time.Duration(seed) * 200 * 24 * time.Hour)
			fixture := addOnboardingEmailTrackerFixture(t, ctx, seed, day)

			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx,
					`DELETE FROM onboarding_email_tracker_daily WHERE send_day >= $1 AND send_day <= $2`,
					fixture.from, fixture.to,
				))
				server.RaisePgResult(tx.Exec(ctx,
					onboardingEmailTrackerReferenceRebuildSql,
					fixture.from, fixture.to.Add(24*time.Hour), fixture.computedAt,
				))
			}, server.TxReadCommitted)
			referenceLines := trackerLines(fixture)

			rowCount, err := RebuildOnboardingEmailTracker(ctx, fixture.from, fixture.to, fixture.computedAt)
			connect.AssertEqual(t, nil, err)
			lines := trackerLines(fixture)
			connect.AssertEqual(t, int64(len(referenceLines)), rowCount)
			t.Logf("seed %d: %d sends, %d feedback rows, %d renewals, %d tracker rows", seed, fixture.emailCount, fixture.feedbackCount, fixture.renewalCount, rowCount)
			if strings.Join(referenceLines, "\n") != strings.Join(lines, "\n") {
				t.Fatalf("seed %d rebuild differs from the reference:\nreference:\n%s\nrebuild:\n%s",
					seed, strings.Join(referenceLines, "\n"), strings.Join(lines, "\n"))
			}

			for _, stepTotals := range SumOnboardingEmailTracker(ctx, fixture.from, fixture.to) {
				for outcome, count := range stepTotals.Outcomes() {
					outcomeCounts[outcome] += count
				}
			}
		}
		// the histories reach every outcome, so agreement is not agreement on
		// zeroes
		t.Logf("outcomes across seeds: %v", outcomeCounts)
		for _, outcome := range onboarding.EmailTrackerOutcomes() {
			if outcomeCounts[outcome] == 0 {
				t.Errorf("fixture produced no %s outcome", outcome)
			}
		}
	})
}

// The production cost driver: account_feedback has no network_id index, so a
// per-send probe read the whole table once or twice for every send. Rows read
// from it in the rebuild transaction must stay within one pass over the table
// plus one probe per send, whichever plan postgres picks.
func TestOnboardingEmailTrackerRebuildDoesNotScanFeedbackPerSend(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := addOnboardingEmailTrackerFixture(t, ctx, 7, time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC))

		var tableRowCount int64
		var readCount int64
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(tx.QueryRow(ctx, `SELECT COUNT(*) FROM account_feedback`).Scan(&tableRowCount))
			// pg_stat_xact counters are this transaction's own reads, and
			// postgres reports them only between transactions
			feedbackReads := func() int64 {
				var reads int64
				server.Raise(tx.QueryRow(ctx, `
					SELECT COALESCE(seq_tup_read, 0) + COALESCE(idx_tup_fetch, 0)
					FROM pg_stat_xact_user_tables
					WHERE relname = 'account_feedback'
				`).Scan(&reads))
				return reads
			}
			readsBefore := feedbackReads()
			server.RaisePgResult(tx.Exec(ctx,
				onboardingEmailTrackerRebuildSql,
				fixture.from, fixture.to.Add(24*time.Hour), fixture.computedAt,
			))
			readCount = feedbackReads() - readsBefore
		}, server.TxReadCommitted)

		connect.AssertEqual(t, int64(fixture.feedbackCount), tableRowCount)
		limit := tableRowCount + int64(fixture.emailCount)
		t.Logf("rebuild read %d account_feedback rows for %d sends over a %d-row table", readCount, fixture.emailCount, tableRowCount)
		if limit < readCount {
			t.Fatalf(
				"rebuild read %d account_feedback rows for %d sends over a %d-row table; want at most %d",
				readCount, fixture.emailCount, tableRowCount, limit,
			)
		}
	})
}
