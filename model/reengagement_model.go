package model

// The re-engagement program's state (mmm/reengagement/PLAN.md "Server"): one
// row per network (network_reengagement), the sends Brevo accepted
// (network_reengagement_email), the candidate queries the daily scan opens
// episodes from, and the campaign-wide consent evidence and marketing
// suppression on account_preferences. Every query is bounded by an index or a
// limit; the scan pages through days, never the whole network table.

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// NetworkReengagement is one network's re-engagement state.
type NetworkReengagement struct {
	NetworkId     server.Id
	Stream        string
	Episode       int
	StartedAt     time.Time
	LastConnectAt *time.Time
	Platform      string
	Locale        string
	TimeZone      string
	Country       string
	ExperimentId  string
	Variant       string
	LastStep      string
	NextStep      string
	NextSendAt    *time.Time
	LastSentAt    *time.Time
	// the R2 answer in this episode
	Reason   string
	ReasonAt *time.Time
	// the fix list: a reported problem waiting for its fix
	FixReason   string
	FixReasonAt *time.Time
	FixPlatform string
	FixSentAt   *time.Time
	QuietUntil  *time.Time
	KeepUntil   *time.Time
	ExitReason  string
	ExitedAt    *time.Time

	SendFailures  int
	LastSendError string
}

// Exited is whether the episode has ended.
func (r *NetworkReengagement) Exited() bool {
	return r.ExitedAt != nil
}

// NetworkReengagementEmail is one re-engagement email as Brevo accepted it.
type NetworkReengagementEmail struct {
	MessageId         string
	NetworkId         server.Id
	Step              string
	Template          string
	Variant           string
	Experiment        string
	ExperimentVariant string
	TemplateId        int
	Locale            string
	SentAt            time.Time
}

const networkReengagementColumns = `
	network_id, stream, episode, started_at, last_connect_at, platform, locale, time_zone, country,
	experiment_id, variant, last_step, next_step, next_send_at, last_sent_at,
	reason, reason_at, fix_reason, fix_reason_at, fix_platform, fix_sent_at,
	quiet_until, keep_until, exit_reason, exited_at, send_failures, last_send_error
`

func scanNetworkReengagement(result pgx.Rows) *NetworkReengagement {
	r := &NetworkReengagement{}
	server.Raise(result.Scan(
		&r.NetworkId, &r.Stream, &r.Episode, &r.StartedAt, &r.LastConnectAt, &r.Platform, &r.Locale, &r.TimeZone, &r.Country,
		&r.ExperimentId, &r.Variant, &r.LastStep, &r.NextStep, &r.NextSendAt, &r.LastSentAt,
		&r.Reason, &r.ReasonAt, &r.FixReason, &r.FixReasonAt, &r.FixPlatform, &r.FixSentAt,
		&r.QuietUntil, &r.KeepUntil, &r.ExitReason, &r.ExitedAt, &r.SendFailures, &r.LastSendError,
	))
	return r
}

// GetNetworkReengagement is the network's row, or nil.
func GetNetworkReengagement(ctx context.Context, networkId server.Id) (row *NetworkReengagement) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+networkReengagementColumns+` FROM network_reengagement WHERE network_id = $1`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				row = scanNetworkReengagement(result)
			}
		})
	})
	return
}

// StartNetworkReengagement opens an episode: a new row, or the network's row
// reused when its last episode has ended, began before `cooldownBefore` (a
// fix follow-up ignores the cooldown: the reader asked for it) and no quiet
// window is running. Keeps the fix list and the keep window. Returns false
// when the network is not eligible now (a replayed scan, an open episode).
func StartNetworkReengagement(ctx context.Context, row *NetworkReengagement, cooldownBefore time.Time, now time.Time) (started bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_reengagement (
					network_id, stream, episode, started_at, last_connect_at, platform, locale, time_zone, country,
					experiment_id, variant, next_step, next_send_at
				)
				VALUES ($1, $2, 1, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
				ON CONFLICT (network_id) DO UPDATE SET
					stream = EXCLUDED.stream,
					episode = network_reengagement.episode + 1,
					started_at = EXCLUDED.started_at,
					last_connect_at = EXCLUDED.last_connect_at,
					platform = CASE WHEN EXCLUDED.platform = '' THEN network_reengagement.platform ELSE EXCLUDED.platform END,
					locale = CASE WHEN EXCLUDED.locale = '' THEN network_reengagement.locale ELSE EXCLUDED.locale END,
					time_zone = CASE WHEN EXCLUDED.time_zone = '' THEN network_reengagement.time_zone ELSE EXCLUDED.time_zone END,
					country = CASE WHEN EXCLUDED.country = '' THEN network_reengagement.country ELSE EXCLUDED.country END,
					experiment_id = EXCLUDED.experiment_id,
					variant = EXCLUDED.variant,
					last_step = '',
					next_step = EXCLUDED.next_step,
					next_send_at = EXCLUDED.next_send_at,
					last_sent_at = NULL,
					reason = '',
					reason_at = NULL,
					exit_reason = '',
					exited_at = NULL,
					send_failures = 0,
					last_send_error = ''
				WHERE
					network_reengagement.exited_at IS NOT NULL AND
					(EXCLUDED.stream = 'fix' OR network_reengagement.started_at < $13) AND
					(network_reengagement.quiet_until IS NULL OR network_reengagement.quiet_until <= $14)
			`,
			row.NetworkId,
			row.Stream,
			row.StartedAt,
			row.LastConnectAt,
			row.Platform,
			row.Locale,
			row.TimeZone,
			row.Country,
			row.ExperimentId,
			row.Variant,
			row.NextStep,
			row.NextSendAt,
			cooldownBefore,
			now,
		))
		started = 0 < tag.RowsAffected()
	})
	return
}

// AdvanceNetworkReengagementInTx records a step's outcome and the next step.
// Only advances from `step` (a replayed task changes nothing).
func AdvanceNetworkReengagementInTx(tx server.PgTx, ctx context.Context, networkId server.Id, step string, sentAt *time.Time, nextStep string, nextSendAt *time.Time, now time.Time) {
	var exitReason string
	var exitedAt *time.Time
	if nextStep == "" {
		exitReason = "done"
		exitedAt = &now
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE network_reengagement SET
				last_step = $2,
				last_sent_at = COALESCE($3, last_sent_at),
				next_step = $4,
				next_send_at = $5,
				exit_reason = CASE WHEN $6 = '' THEN exit_reason ELSE $6 END,
				exited_at = COALESCE($7, exited_at),
				send_failures = 0,
				last_send_error = ''
			WHERE network_id = $1 AND next_step = $2 AND exited_at IS NULL
		`,
		networkId,
		step,
		sentAt,
		nextStep,
		nextSendAt,
		exitReason,
		exitedAt,
	))
}

// ExitNetworkReengagement ends the open episode. False when none was open.
func ExitNetworkReengagement(ctx context.Context, networkId server.Id, reason string, now time.Time) (exited bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE network_reengagement
				SET exit_reason = $2, exited_at = $3, next_step = '', next_send_at = NULL
				WHERE network_id = $1 AND exited_at IS NULL
			`,
			networkId,
			reason,
			now,
		))
		exited = 0 < tag.RowsAffected()
	})
	return
}

// RecordNetworkReengagementSendFailure counts a failed send for diagnosis.
func RecordNetworkReengagementSendFailure(ctx context.Context, networkId server.Id, message string) {
	if 256 < len(message) {
		message = message[:256]
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE network_reengagement SET send_failures = send_failures + 1, last_send_error = $2 WHERE network_id = $1`,
			networkId,
			message,
		))
	})
}

// SetNetworkReengagementAnswer records the R2 answer of the open episode.
func SetNetworkReengagementAnswer(ctx context.Context, networkId server.Id, reason string, now time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE network_reengagement SET reason = $2, reason_at = $3 WHERE network_id = $1 AND exited_at IS NULL`,
			networkId,
			reason,
			now,
		))
	})
}

// JoinNetworkReengagementFix puts the network on the fix list: one follow-up
// when a fix for `reason` reaches `platform`. Creates an ended row when the
// network has none (an onboarding answer).
func JoinNetworkReengagementFix(ctx context.Context, networkId server.Id, reason string, platform string, now time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_reengagement (
					network_id, stream, started_at, platform, fix_reason, fix_reason_at, fix_platform, exit_reason, exited_at
				)
				VALUES ($1, 'fix', $4, $3, $2, $4, $3, 'answered', $4)
				ON CONFLICT (network_id) DO UPDATE SET
					fix_reason = $2,
					fix_reason_at = $4,
					fix_platform = CASE WHEN $3 = '' THEN network_reengagement.platform ELSE $3 END,
					fix_sent_at = NULL
			`,
			networkId,
			reason,
			platform,
			now,
		))
	})
}

// MarkNetworkReengagementFixSent takes the network off the fix list.
func MarkNetworkReengagementFixSent(ctx context.Context, networkId server.Id, now time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE network_reengagement SET fix_sent_at = $2 WHERE network_id = $1`,
			networkId,
			now,
		))
	})
}

// SetNetworkReengagementQuiet holds new episodes until `until`.
func SetNetworkReengagementQuiet(ctx context.Context, networkId server.Id, until time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE network_reengagement SET quiet_until = $2 WHERE network_id = $1`, networkId, until))
	})
}

// SetNetworkReengagementKeep records a keep click: the reader asked to keep
// getting occasional email until `until`.
func SetNetworkReengagementKeep(ctx context.Context, networkId server.Id, until time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE network_reengagement SET keep_until = $2 WHERE network_id = $1`, networkId, until))
	})
}

// ----- emails -----

func AddNetworkReengagementEmailInTx(tx server.PgTx, ctx context.Context, email *NetworkReengagementEmail) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO network_reengagement_email (
				message_id, network_id, step, template, variant, experiment, experiment_variant, template_id, locale, sent_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (message_id) DO NOTHING
		`,
		email.MessageId,
		email.NetworkId,
		email.Step,
		email.Template,
		email.Variant,
		email.Experiment,
		email.ExperimentVariant,
		email.TemplateId,
		email.Locale,
		email.SentAt,
	))
}

const networkReengagementEmailColumns = `message_id, network_id, step, template, variant, experiment, experiment_variant, template_id, locale, sent_at`

func scanNetworkReengagementEmail(result pgx.Rows) *NetworkReengagementEmail {
	e := &NetworkReengagementEmail{}
	server.Raise(result.Scan(&e.MessageId, &e.NetworkId, &e.Step, &e.Template, &e.Variant, &e.Experiment, &e.ExperimentVariant, &e.TemplateId, &e.Locale, &e.SentAt))
	return e
}

// GetNetworkReengagementEmail is the send behind a Brevo message id, or nil.
func GetNetworkReengagementEmail(ctx context.Context, messageId string) (email *NetworkReengagementEmail) {
	if strings.TrimSpace(messageId) == "" {
		return nil
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT `+networkReengagementEmailColumns+` FROM network_reengagement_email WHERE message_id = $1`, messageId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				email = scanNetworkReengagementEmail(result)
			}
		})
	})
	return
}

// ListNetworkReengagementEmails is a network's sends, newest first.
func ListNetworkReengagementEmails(ctx context.Context, networkId server.Id) (emails []*NetworkReengagementEmail) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+networkReengagementEmailColumns+` FROM network_reengagement_email WHERE network_id = $1 ORDER BY sent_at DESC LIMIT 100`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				emails = append(emails, scanNetworkReengagementEmail(result))
			}
		})
	})
	return
}

// CountNetworkReengagementSendsSince counts the network's re-engagement emails since `since`.
func CountNetworkReengagementSendsSince(ctx context.Context, networkId server.Id, since time.Time) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT COUNT(*) FROM network_reengagement_email WHERE network_id = $1 AND sent_at >= $2`,
			networkId,
			since,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return
}

// LastCampaignSendAt is the network's latest campaign email of either
// program, for the one-a-week rule.
func LastCampaignSendAt(ctx context.Context, networkId server.Id) (at *time.Time) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT GREATEST(
					(SELECT MAX(sent_at) FROM network_onboarding_email WHERE network_id = $1),
					(SELECT MAX(sent_at) FROM network_reengagement_email WHERE network_id = $1)
				)
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&at))
			}
		})
	})
	return
}

// ----- candidates for the daily scan -----

// ReengagementCandidate is a network a scan may open an episode for.
type ReengagementCandidate struct {
	NetworkId server.Id
	// the last connection day (lapsed), the sign-up (never activated), the
	// paid window's end (win-back)
	At     time.Time
	Market string
}

// The mailable-network filter every candidate query applies: an email login,
// product updates on, no marketing suppression, no open episode.
const reengagementMailableFilter = `
	EXISTS (
		SELECT 1
		FROM network
		INNER JOIN network_user ON network_user.user_id = network.admin_user_id
		WHERE network.network_id = candidate.network_id AND network_user.user_auth LIKE '%@%'
	) AND
	NOT EXISTS (
		SELECT 1 FROM account_preferences
		WHERE
			account_preferences.network_id = candidate.network_id AND
			(account_preferences.product_updates = false OR account_preferences.marketing_suppressed_at IS NOT NULL)
	) AND
	NOT EXISTS (
		SELECT 1 FROM network_reengagement
		WHERE network_reengagement.network_id = candidate.network_id AND network_reengagement.exited_at IS NULL
	)
`

// ListLapsedReengagementCandidates are networks whose last connection day was
// in [dayFrom, dayTo) and who have not connected since: a connect.day in the
// range and none at or after dayTo.
func ListLapsedReengagementCandidates(ctx context.Context, dayFrom time.Time, dayTo time.Time, limit int) (candidates []*ReengagementCandidate) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT candidate.network_id, candidate.at
				FROM (
					SELECT network_id, MAX(at) AS at
					FROM network_onboarding_event
					WHERE name = $3 AND $1 <= at AND at < $2
					GROUP BY network_id
				) candidate
				WHERE
					NOT EXISTS (
						SELECT 1 FROM network_onboarding_event later
						WHERE
							later.network_id = candidate.network_id AND
							(later.network_id, later.at) >= (candidate.network_id, $2::timestamp) AND
							later.name = $3
					) AND
					`+reengagementMailableFilter+`
				ORDER BY candidate.at, candidate.network_id
				LIMIT $4
			`,
			dayFrom,
			dayTo,
			EventConnectDay,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				c := &ReengagementCandidate{}
				server.Raise(result.Scan(&c.NetworkId, &c.At))
				candidates = append(candidates, c)
			}
		})
	})
	return
}

// ListNeverActivatedReengagementCandidates are email sign-ups enrolled in
// onboarding in [createdFrom, createdTo) whose sequence is over. Whether they
// ever connected is decided per network (NetworkConnectDays reads every
// record of a connection).
func ListNeverActivatedReengagementCandidates(ctx context.Context, createdFrom time.Time, createdTo time.Time, limit int) (candidates []*ReengagementCandidate) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT candidate.network_id, candidate.created_at
				FROM network_onboarding candidate
				WHERE
					candidate.email AND
					$1 <= candidate.created_at AND candidate.created_at < $2 AND
					(candidate.exited_at IS NOT NULL OR candidate.next_step = '') AND
					candidate.exit_reason NOT IN ('bounced', 'complained', 'opt_out', 'network_deleted', 'no_email') AND
					`+reengagementMailableFilter+`
				ORDER BY candidate.created_at, candidate.network_id
				LIMIT $3
			`,
			createdFrom,
			createdTo,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				c := &ReengagementCandidate{}
				server.Raise(result.Scan(&c.NetworkId, &c.At))
				candidates = append(candidates, c)
			}
		})
	})
	return
}

// ListPendingReengagementFixes are networks on the fix list answered after
// `answeredAfter` whose follow-up has not gone out, oldest first.
func ListPendingReengagementFixes(ctx context.Context, answeredAfter time.Time, limit int) (rows []*NetworkReengagement) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+networkReengagementColumns+`
				FROM network_reengagement
				WHERE fix_reason_at IS NOT NULL AND fix_sent_at IS NULL AND $1 <= fix_reason_at AND exited_at IS NOT NULL
				ORDER BY fix_reason_at, network_id
				LIMIT $2
			`,
			answeredAfter,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				rows = append(rows, scanNetworkReengagement(result))
			}
		})
	})
	return
}

// The markets whose supporter windows record their end with the 30-day
// manual-payment grace already in it (controller manualPaymentGracePeriod:
// crypto_pay_credit.go and payment_reconcile_controller.go add it to
// end_time), so Pro stops at end_time; the App Store and Play end theirs at the
// store's expiry and may retry a failed renewal after it.
var reengagementGraceMarkets = []string{"stripe", "solana", "monero", "zcash", "bittensor", "bitcoin", "ethereum", "x402", "manual"}

// ReengagementGraceMarkets are the markets whose end_time includes the grace.
func ReengagementGraceMarkets() []string {
	return reengagementGraceMarkets
}

// ListWinbackReengagementCandidates are networks that paid for Pro (a window
// with revenue) whose latest supporter window ended in [endFrom, endTo), with
// no later window on any market, no refund on record, and a market in
// `graceMarkets` (grace = true) or not in it (grace = false).
func ListWinbackReengagementCandidates(ctx context.Context, endFrom time.Time, endTo time.Time, grace bool, limit int) (candidates []*ReengagementCandidate) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT candidate.network_id, candidate.end_time, candidate.market
				FROM (
					SELECT DISTINCT ON (network_id) network_id, end_time, COALESCE(market, '') AS market
					FROM subscription_renewal
					WHERE subscription_type = 'supporter' AND $1 <= end_time AND end_time < $2
					ORDER BY network_id, end_time DESC
				) candidate
				WHERE
					(candidate.market = ANY($3::text[])) = $4 AND
					NOT EXISTS (
						SELECT 1 FROM subscription_renewal later
						WHERE
							later.network_id = candidate.network_id AND
							later.subscription_type = 'supporter' AND
							candidate.end_time < later.end_time
					) AND
					EXISTS (
						SELECT 1 FROM subscription_renewal paid
						WHERE
							paid.network_id = candidate.network_id AND
							paid.subscription_type = 'supporter' AND
							0 < paid.net_revenue_nano_cents
					) AND
					NOT EXISTS (SELECT 1 FROM stripe_refund WHERE stripe_refund.network_id = candidate.network_id) AND
					`+reengagementMailableFilter+`
				ORDER BY candidate.end_time, candidate.network_id
				LIMIT $5
			`,
			endFrom,
			endTo,
			reengagementGraceMarkets,
			grace,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				c := &ReengagementCandidate{}
				server.Raise(result.Scan(&c.NetworkId, &c.At, &c.Market))
				candidates = append(candidates, c)
			}
		})
	})
	return
}

// ----- facts -----

// NetworkLastConnectDay is the network's latest connect.day, or nil.
func NetworkLastConnectDay(ctx context.Context, networkId server.Id) (at *time.Time) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT at FROM network_onboarding_event
				WHERE network_id = $1 AND name = $2
				ORDER BY at DESC
				LIMIT 1
			`,
			networkId,
			EventConnectDay,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var t time.Time
				server.Raise(result.Scan(&t))
				at = &t
			}
		})
	})
	return
}

// NetworkHasEventSince is whether the network has an event `name` at or after `since`.
func NetworkHasEventSince(ctx context.Context, networkId server.Id, name string, since time.Time) (exists bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT 1 FROM network_onboarding_event
				WHERE network_id = $1 AND (network_id, at) >= ($1::uuid, $3::timestamp) AND name = $2
				LIMIT 1
			`,
			networkId,
			name,
			since,
		)
		server.WithPgResult(result, err, func() {
			exists = result.Next()
		})
	})
	return
}

// NetworkOnboardingAnswer is the latest one-tap or in-app answer to the
// onboarding feedback email ("what got in the way?"): the reason of the
// newest feedback.tapped or feedback.submitted event that has one.
func NetworkOnboardingAnswer(ctx context.Context, networkId server.Id) (reason string, at *time.Time) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT props->>'reason', at FROM network_onboarding_event
				WHERE
					network_id = $1 AND name IN ($2, $3) AND
					COALESCE(props->>'reason', '') <> ''
				ORDER BY at DESC
				LIMIT 1
			`,
			networkId,
			EventFeedbackTapped,
			EventFeedbackSubmitted,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var t time.Time
				server.Raise(result.Scan(&reason, &t))
				at = &t
			}
		})
	})
	return
}

// NetworkLastPaidEnd is the end of the network's latest paid supporter window, or nil.
func NetworkLastPaidEnd(ctx context.Context, networkId server.Id) (at *time.Time) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT MAX(end_time) FROM subscription_renewal
				WHERE network_id = $1 AND subscription_type = 'supporter' AND 0 < net_revenue_nano_cents
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&at))
			}
		})
	})
	return
}

// NetworkLastPaidWindow is the end and market of the network's latest paid
// supporter window.
func NetworkLastPaidWindow(ctx context.Context, networkId server.Id) (end time.Time, market string, ok bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT end_time, COALESCE(market, '') FROM subscription_renewal
				WHERE network_id = $1 AND subscription_type = 'supporter' AND 0 < net_revenue_nano_cents
				ORDER BY end_time DESC
				LIMIT 1
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&end, &market))
				ok = true
			}
		})
	})
	return
}

// NetworkStripeBillingCountry is the billing country of the network's Stripe
// customer, or "".
func NetworkStripeBillingCountry(ctx context.Context, networkId server.Id) (country string) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT COALESCE(billing_country, '') FROM stripe_customer
				WHERE network_id = $1 AND COALESCE(billing_country, '') <> ''
				ORDER BY create_time DESC
				LIMIT 1
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&country))
			}
		})
	})
	return strings.ToUpper(strings.TrimSpace(country))
}

// NetworkClientContext is what a network's own clients last reported about
// themselves, read when its onboarding row is missing or lacks a part: the
// platform and the locale of its newest client events (POST /client/events,
// sent by the apps since 2026-09-09), and the device spec its newest client's
// app last sent on auth-client, each with when it was reported. Nothing here
// comes from an IP.
type NetworkClientContext struct {
	EventPlatform   string
	EventPlatformAt *time.Time
	EventLocale     string
	EventLocaleAt   *time.Time
	// the device of the network's newest client, and that client's last
	// authentication or connection
	DeviceSpec   string
	DeviceSpecAt *time.Time
}

// GetNetworkClientContext reads the newest client-reported platform and locale
// (client events only: the server's own events carry the platform it was
// told) and the newest client's device spec. Each part is one backward read
// of a per-network index.
func GetNetworkClientContext(ctx context.Context, networkId server.Id) *NetworkClientContext {
	c := &NetworkClientContext{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					event_platform.platform,
					event_platform.at,
					event_locale.locale,
					event_locale.at,
					newest_device.device_spec,
					newest_device.auth_time
				FROM (SELECT 1) one
				LEFT JOIN LATERAL (
					SELECT platform, at FROM network_onboarding_event
					WHERE network_id = $1 AND name = ANY($2::text[]) AND platform <> ''
					ORDER BY at DESC
					LIMIT 1
				) event_platform ON true
				LEFT JOIN LATERAL (
					SELECT locale, at FROM network_onboarding_event
					WHERE network_id = $1 AND name = ANY($2::text[]) AND locale <> ''
					ORDER BY at DESC
					LIMIT 1
				) event_locale ON true
				LEFT JOIN LATERAL (
					SELECT device.device_spec, newest.auth_time
					FROM (
						SELECT device_id, auth_time FROM network_client
						WHERE network_id = $1 AND device_id IS NOT NULL
						ORDER BY create_time DESC
						LIMIT 1
					) newest
					INNER JOIN device ON device.device_id = newest.device_id
				) newest_device ON true
			`,
			networkId,
			ClientEventNames(),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var eventPlatform, eventLocale, deviceSpec *string
				server.Raise(result.Scan(
					&eventPlatform,
					&c.EventPlatformAt,
					&eventLocale,
					&c.EventLocaleAt,
					&deviceSpec,
					&c.DeviceSpecAt,
				))
				if eventPlatform != nil {
					c.EventPlatform = *eventPlatform
				}
				if eventLocale != nil {
					c.EventLocale = *eventLocale
				}
				if deviceSpec != nil {
					c.DeviceSpec = *deviceSpec
				}
			}
		})
	})
	return c
}

// ----- consent evidence and suppression (account_preferences) -----

// NetworkMailState is the network's product-updates preference, how it was
// set, and its marketing suppression.
type NetworkMailState struct {
	ProductUpdates   bool
	Source           string
	SourceTime       *time.Time
	SuppressedAt     *time.Time
	SuppressedReason string
}

// GetNetworkMailState reads the preference row; a network without one is
// opted in with an unknown source (the sign-up default).
func GetNetworkMailState(ctx context.Context, networkId server.Id) *NetworkMailState {
	state := &NetworkMailState{ProductUpdates: true}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT product_updates, product_updates_source, product_updates_source_time,
					marketing_suppressed_at, marketing_suppressed_reason
				FROM account_preferences
				WHERE network_id = $1
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&state.ProductUpdates, &state.Source, &state.SourceTime, &state.SuppressedAt, &state.SuppressedReason))
			}
		})
	})
	return state
}

// SetNetworkProductUpdatesSourceInTx records how the preference was just set.
func SetNetworkProductUpdatesSourceInTx(tx server.PgTx, ctx context.Context, networkId server.Id, source string, now time.Time) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE account_preferences
			SET product_updates_source = $2, product_updates_source_time = $3
			WHERE network_id = $1
		`,
		networkId,
		source,
		now,
	))
}

// SetNetworkProductUpdatesSource records how the preference was just set.
func SetNetworkProductUpdatesSource(ctx context.Context, networkId server.Id, source string, now time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		SetNetworkProductUpdatesSourceInTx(tx, ctx, networkId, source, now)
	})
}

// SuppressNetworkMarketingInTx stops every campaign's mail to the network. The
// first suppression's time and reason stay.
//
// Callers use controller.SuppressNetworkMarketing, which mirrors the
// suppression to Brevo's lists in the same transaction.
func SuppressNetworkMarketingInTx(tx server.PgTx, ctx context.Context, networkId server.Id, reason string, now time.Time) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO account_preferences (network_id, product_updates, marketing_suppressed_at, marketing_suppressed_reason)
			VALUES ($1, true, $3, $2)
			ON CONFLICT (network_id) DO UPDATE SET
				marketing_suppressed_at = COALESCE(account_preferences.marketing_suppressed_at, $3),
				marketing_suppressed_reason = CASE
					WHEN account_preferences.marketing_suppressed_at IS NULL THEN $2
					ELSE account_preferences.marketing_suppressed_reason
				END
		`,
		networkId,
		reason,
		now,
	))
}

// LiftNetworkMarketingSuppressionInTx clears a suppression whose reason is in
// `liftable` (a connection renews the relationship after a sunset; a bounce,
// complaint or unsubscribe never lifts). Returns whether it lifted.
//
// Callers use controller.LiftNetworkMarketingSuppression, which mirrors the
// lift to Brevo's lists in the same transaction.
func LiftNetworkMarketingSuppressionInTx(tx server.PgTx, ctx context.Context, networkId server.Id, liftable []string) (lifted bool) {
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE account_preferences
			SET marketing_suppressed_at = NULL, marketing_suppressed_reason = ''
			WHERE network_id = $1 AND marketing_suppressed_at IS NOT NULL AND marketing_suppressed_reason = ANY($2::text[])
		`,
		networkId,
		liftable,
	))
	return 0 < tag.RowsAffected()
}

// ListLiftableSuppressedNetworksConnectedOn are networks suppressed for a
// liftable reason that connected in [dayFrom, dayTo) after the suppression.
func ListLiftableSuppressedNetworksConnectedOn(ctx context.Context, dayFrom time.Time, dayTo time.Time, liftable []string, limit int) (networkIds []server.Id) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT DISTINCT event.network_id
				FROM network_onboarding_event event
				INNER JOIN account_preferences ON account_preferences.network_id = event.network_id
				WHERE
					event.name = $3 AND $1 <= event.at AND event.at < $2 AND
					account_preferences.marketing_suppressed_at IS NOT NULL AND
					account_preferences.marketing_suppressed_reason = ANY($4::text[]) AND
					date_trunc('day', account_preferences.marketing_suppressed_at) <= event.at
				LIMIT $5
			`,
			dayFrom,
			dayTo,
			EventConnectDay,
			liftable,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var id server.Id
				server.Raise(result.Scan(&id))
				networkIds = append(networkIds, id)
			}
		})
	})
	return
}
