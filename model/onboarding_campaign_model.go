// SPDX-License-Identifier: MPL-2.0

package model

// The onboarding email campaign rows (mmm/onboarding/PLAN.md "THE EMAIL
// SEQUENCE"): one network_onboarding row per network and one
// network_onboarding_email row per Brevo send. The decisions live in
// server/onboarding (pure); this file is the storage and the fact loaders the
// controller feeds it with.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// NetworkOnboarding is one network's campaign state.
type NetworkOnboarding struct {
	NetworkId server.Id
	CreatedAt time.Time
	// A saw the in-app offer, B did not; "" until E1 decides
	Path string
	// Email is whether the account has an email auth at all (phone and seed
	// accounts get a row that exits as no_email so the results job can count them)
	Email    bool
	TimeZone string
	Platform string
	Locale   string
	// Country is the sign-up IP's country (upper-case alpha-2), the price tier
	// the emailed offer is priced at
	Country      string
	ExperimentId string
	EmailVariant string
	E1SentAt     *time.Time
	E2SentAt     *time.Time
	E3SentAt     *time.Time
	E4SentAt     *time.Time
	E5SentAt     *time.Time
	LastStep     string
	NextStep     string
	NextSendAt   *time.Time
	ExitReason   string
	ExitedAt     *time.Time
	Bounced      bool
	Complained   bool
	// ComplainedStep is the step whose email drew the complaint
	ComplainedStep string
	SendFailures   int
	LastSendError  string
}

// Exited is whether the sequence has stopped for this network.
func (o *NetworkOnboarding) Exited() bool {
	return o.ExitedAt != nil
}

// NetworkOnboardingEmail is one campaign email as Brevo accepted it.
type NetworkOnboardingEmail struct {
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

// CreateNetworkOnboarding inserts the row once. A second call for the same
// network (verify after create, a replayed task) changes nothing and returns
// false.
func CreateNetworkOnboarding(ctx context.Context, row *NetworkOnboarding) (created bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_onboarding (
					network_id, created_at, path, email, time_zone, platform, locale, country,
					experiment_id, email_variant, next_step, next_send_at, exit_reason, exited_at
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
				ON CONFLICT (network_id) DO NOTHING
			`,
			row.NetworkId,
			row.CreatedAt,
			row.Path,
			row.Email,
			row.TimeZone,
			row.Platform,
			row.Locale,
			row.Country,
			row.ExperimentId,
			row.EmailVariant,
			row.NextStep,
			row.NextSendAt,
			row.ExitReason,
			row.ExitedAt,
		))
		created = 0 < tag.RowsAffected()
	})
	return
}

const networkOnboardingSelect = `
	SELECT
		network_id, created_at, path, email, time_zone, platform, locale, country,
		experiment_id, email_variant,
		e1_sent_at, e2_sent_at, e3_sent_at, e4_sent_at, e5_sent_at,
		last_step, next_step, next_send_at, exit_reason, exited_at,
		bounced, complained, complained_step, send_failures, last_send_error
	FROM network_onboarding
	WHERE network_id = $1
`

func scanNetworkOnboarding(result pgx.Rows) *NetworkOnboarding {
	o := &NetworkOnboarding{}
	server.Raise(result.Scan(
		&o.NetworkId, &o.CreatedAt, &o.Path, &o.Email, &o.TimeZone, &o.Platform, &o.Locale, &o.Country,
		&o.ExperimentId, &o.EmailVariant,
		&o.E1SentAt, &o.E2SentAt, &o.E3SentAt, &o.E4SentAt, &o.E5SentAt,
		&o.LastStep, &o.NextStep, &o.NextSendAt, &o.ExitReason, &o.ExitedAt,
		&o.Bounced, &o.Complained, &o.ComplainedStep, &o.SendFailures, &o.LastSendError,
	))
	return o
}

// GetNetworkOnboarding is the row, or nil when the network never entered the
// campaign (accounts created before it shipped).
func GetNetworkOnboarding(ctx context.Context, networkId server.Id) (row *NetworkOnboarding) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, networkOnboardingSelect, networkId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				row = scanNetworkOnboarding(result)
			}
		})
	})
	return
}

// SetNetworkOnboardingPath records the path E1 decided (re-evaluated later).
func SetNetworkOnboardingPath(ctx context.Context, networkId server.Id, path string) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE network_onboarding SET path = $2 WHERE network_id = $1`,
			networkId,
			path,
		))
	})
}

// SetNetworkOnboardingTimeZone stores the zone a client reported. Only a
// non-empty zone overwrites, and only while the sequence is running.
func SetNetworkOnboardingTimeZone(ctx context.Context, networkId server.Id, timeZone string) (updated bool) {
	if timeZone == "" {
		return false
	}
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE network_onboarding
				SET time_zone = $2
				WHERE network_id = $1 AND time_zone != $2 AND exited_at IS NULL
			`,
			networkId,
			timeZone,
		))
		updated = 0 < tag.RowsAffected()
	})
	return
}

// SetNetworkOnboardingPlatform stores the platform once it is known (the
// auth-client device spec); only fills an empty value.
func SetNetworkOnboardingPlatform(ctx context.Context, networkId server.Id, platform string) (updated bool) {
	if platform == "" {
		return false
	}
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE network_onboarding SET platform = $2 WHERE network_id = $1 AND platform = ''`,
			networkId,
			platform,
		))
		updated = 0 < tag.RowsAffected()
	})
	return
}

// SetNetworkOnboardingLocale stores the locale a client reported (same rules
// as the time zone).
func SetNetworkOnboardingLocale(ctx context.Context, networkId server.Id, locale string) (updated bool) {
	if locale == "" {
		return false
	}
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE network_onboarding
				SET locale = $2
				WHERE network_id = $1 AND locale != $2 AND exited_at IS NULL
			`,
			networkId,
			locale,
		))
		updated = 0 < tag.RowsAffected()
	})
	return
}

// AdvanceNetworkOnboardingInTx records the outcome of one step: the send time
// when it sent (nil when skipped), the step just handled, and what comes next
// (next step "" and nil time = the sequence is complete, which is an exit as
// `done`). It also clears any prior send failure.
func AdvanceNetworkOnboardingInTx(
	tx server.PgTx,
	ctx context.Context,
	networkId server.Id,
	step string,
	sentAt *time.Time,
	path string,
	nextStep string,
	nextSendAt *time.Time,
	now time.Time,
) {
	sentColumn := map[string]string{
		"e1": "e1_sent_at", "e2": "e2_sent_at", "e3": "e3_sent_at", "e4": "e4_sent_at", "e5": "e5_sent_at",
	}[step]
	if sentColumn != "" && sentAt != nil {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE network_onboarding SET `+sentColumn+` = $2 WHERE network_id = $1`,
			networkId,
			*sentAt,
		))
	}
	var exitReason string
	var exitedAt *time.Time
	if nextStep == "" {
		exitReason = "done"
		exitedAt = &now
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE network_onboarding
			SET
				path = $2,
				last_step = $3,
				next_step = $4,
				next_send_at = $5,
				exit_reason = CASE WHEN $6 = '' THEN exit_reason ELSE $6 END,
				exited_at = COALESCE(exited_at, $7),
				last_send_error = ''
			WHERE network_id = $1
		`,
		networkId,
		path,
		step,
		nextStep,
		nextSendAt,
		exitReason,
		exitedAt,
	))
}

// ExitNetworkOnboarding stops the sequence with a reason. The first exit wins;
// a later one changes nothing (returns false).
func ExitNetworkOnboarding(ctx context.Context, networkId server.Id, reason string, now time.Time) (exited bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		exited = ExitNetworkOnboardingInTx(tx, ctx, networkId, reason, now)
	})
	return
}

func ExitNetworkOnboardingInTx(tx server.PgTx, ctx context.Context, networkId server.Id, reason string, now time.Time) bool {
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE network_onboarding
			SET exit_reason = $2, exited_at = $3, next_step = '', next_send_at = NULL
			WHERE network_id = $1 AND exited_at IS NULL
		`,
		networkId,
		reason,
		now,
	))
	return 0 < tag.RowsAffected()
}

// RecordNetworkOnboardingSendFailure counts a failed Brevo call (the step is
// retried by the task scheduler; the row shows why the last try failed).
func RecordNetworkOnboardingSendFailure(ctx context.Context, networkId server.Id, message string) {
	if 256 < len(message) {
		message = message[:256]
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE network_onboarding
				SET send_failures = send_failures + 1, last_send_error = $2
				WHERE network_id = $1
			`,
			networkId,
			message,
		))
	})
}

// MarkNetworkOnboardingBounced / Complained flag the row from the webhook. The
// exit itself is recorded by ExitNetworkOnboarding.
func MarkNetworkOnboardingBounced(ctx context.Context, networkId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE network_onboarding SET bounced = true WHERE network_id = $1`,
			networkId,
		))
	})
}

func MarkNetworkOnboardingComplained(ctx context.Context, networkId server.Id, step string) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE network_onboarding SET complained = true, complained_step = $2 WHERE network_id = $1`,
			networkId,
			step,
		))
	})
}

// ----- sent emails -----

// AddNetworkOnboardingEmailInTx records a send Brevo accepted, keyed by its
// message id so webhook events attribute back to the network and step.
func AddNetworkOnboardingEmailInTx(tx server.PgTx, ctx context.Context, email *NetworkOnboardingEmail) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO network_onboarding_email (
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

const networkOnboardingEmailSelect = `
	SELECT message_id, network_id, step, template, variant, experiment, experiment_variant, template_id, locale, sent_at
	FROM network_onboarding_email
`

func scanNetworkOnboardingEmail(result pgx.Rows) *NetworkOnboardingEmail {
	e := &NetworkOnboardingEmail{}
	server.Raise(result.Scan(
		&e.MessageId, &e.NetworkId, &e.Step, &e.Template, &e.Variant, &e.Experiment, &e.ExperimentVariant, &e.TemplateId, &e.Locale, &e.SentAt,
	))
	return e
}

// GetNetworkOnboardingEmail looks a webhook's message id up, nil when the
// message was not a campaign send.
func GetNetworkOnboardingEmail(ctx context.Context, messageId string) (email *NetworkOnboardingEmail) {
	if messageId == "" {
		return nil
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, networkOnboardingEmailSelect+` WHERE message_id = $1`, messageId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				email = scanNetworkOnboardingEmail(result)
			}
		})
	})
	return
}

// ListNetworkOnboardingEmails is every campaign send for a network, oldest first.
func ListNetworkOnboardingEmails(ctx context.Context, networkId server.Id) (emails []*NetworkOnboardingEmail) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, networkOnboardingEmailSelect+` WHERE network_id = $1 ORDER BY sent_at ASC`, networkId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				emails = append(emails, scanNetworkOnboardingEmail(result))
			}
		})
	})
	return
}

// ----- facts the step decisions look at -----

// NetworkExists is whether the network row still exists (networks are hard
// deleted).
func NetworkExists(ctx context.Context, networkId server.Id) (exists bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT 1 FROM network WHERE network_id = $1`, networkId)
		server.WithPgResult(result, err, func() {
			exists = result.Next()
		})
	})
	return
}

// NetworkProductUpdates is the network's product-updates preference; a network
// without a preference row is opted in (the sign-up default).
func NetworkProductUpdates(ctx context.Context, networkId server.Id) (productUpdates bool) {
	productUpdates = true
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT product_updates FROM account_preferences WHERE network_id = $1`, networkId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&productUpdates))
			}
		})
	})
	return
}

// NetworkConnectDays counts the distinct UTC days on which any of the
// network's clients held a connection since `since`, and whether there was one
// at all.
func NetworkConnectDays(ctx context.Context, networkId server.Id, since time.Time) (days int, connected bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT COUNT(DISTINCT date_trunc('day', network_client_connection.connect_time))
				FROM network_client_connection
				INNER JOIN network_client ON network_client.client_id = network_client_connection.client_id
				WHERE network_client.network_id = $1 AND network_client_connection.connect_time >= $2
			`,
			networkId,
			since,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&days))
			}
		})
	})
	connected = 0 < days
	return
}

// NetworkHasFeedback is whether the network submitted app feedback since `since`.
func NetworkHasFeedback(ctx context.Context, networkId server.Id, since time.Time) (exists bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT 1 FROM account_feedback WHERE network_id = $1 AND feedback_time >= $2 LIMIT 1`,
			networkId,
			since,
		)
		server.WithPgResult(result, err, func() {
			exists = result.Next()
		})
	})
	return
}

// CountOnboardingEvents counts a network's events by name.
func CountOnboardingEvents(ctx context.Context, networkId server.Id, name string) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT COUNT(*) FROM network_onboarding_event WHERE network_id = $1 AND name = $2`,
			networkId,
			name,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return
}

// ListOnboardingEvents is a network's events, oldest first, for the status tool.
func ListOnboardingEvents(ctx context.Context, networkId server.Id, limit int) (events []*OnboardingEvent) {
	if limit <= 0 {
		limit = 200
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT event_id, name, at, received_at, platform, app_version, locale, tier, path, experiment, variant, session, props
				FROM network_onboarding_event
				WHERE network_id = $1
				ORDER BY at ASC
				LIMIT $2
			`,
			networkId,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				event := &OnboardingEvent{NetworkId: networkId}
				var props *string
				server.Raise(result.Scan(
					&event.EventId, &event.Name, &event.At, &event.ReceivedAt, &event.Platform, &event.AppVersion, &event.Locale,
					&event.Tier, &event.Path, &event.Experiment, &event.Variant, &event.Session, &props,
				))
				if props != nil {
					event.Props = decodeEventProps(*props)
				}
				events = append(events, event)
			}
		})
	})
	return
}

// decodeEventProps parses a stored props document; a malformed one is an empty map.
func decodeEventProps(propsJson string) map[string]any {
	props := map[string]any{}
	if err := json.Unmarshal([]byte(propsJson), &props); err != nil {
		return map[string]any{}
	}
	return props
}

// NetworkEnrollmentFacts is what the enrollment gate needs about a network
// that has no campaign row yet: its admin user's login (empty for a
// seed-phrase, wallet or phone account, or a missing user) and its creation
// time. ok is false when the network does not exist.
func NetworkEnrollmentFacts(ctx context.Context, networkId server.Id) (userAuth string, createTime time.Time, ok bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT coalesce(u.user_auth, ''), n.create_time
				FROM network n
				LEFT JOIN network_user u ON u.user_id = n.admin_user_id
				WHERE n.network_id = $1
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&userAuth, &createTime))
				ok = true
			}
		})
	})
	return
}
