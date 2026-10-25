// The account message outbox. A state change that owes its account a message
// (an email or SMS) adds it here in its own transaction with
// `AddAccountMessageInTx`, so the message exists exactly when the change
// commits: a transaction that rolls back leaves no message, and a commit can no
// longer be followed by a crash that loses one. The controller's delivery task
// claims due messages one at a time after the commit, sends them, and records
// the outcome, retrying failures with backoff.
//
// Each message has a key derived from the state change that owes it; adding a
// key that exists adds nothing, so a writer that runs again (a rerun
// transaction, a retried task or webhook) owes one message. A claim leases the
// message to one delivery attempt: a second deliverer skips it until the lease
// ends, and a delivered message is never claimed again. The one window left is
// a crash between a send and its record, after which the lease ends and the
// message is sent again (at least once).
//
// A held message (no deliver time) waits for its writer to release it, so a run
// that commits in several transactions can owe one message (the missing-wallet
// notice of a payout run). The rows keep the recipient, so delivered and
// abandoned messages are removed after a retention period.
package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
	"unicode/utf8"

	"github.com/urnetwork/server/v2026"
)

// The account message templates the model writes itself. The controller's
// templates of the same names render them.
const (
	AuthPasswordSetTemplateName = "auth_password_set"
	NetworkWelcomeTemplateName  = "network_welcome"
)

// The longest last error kept on a message (varchar(512)).
const accountMessageLastErrorMaxLength = 512

// One message in the outbox.
type AccountMessage struct {
	MessageId    server.Id
	NetworkId    *server.Id
	UserAuth     string
	TemplateName string
	// the template's fields as json
	TemplateJson string
	CreateTime   time.Time
	// nil while the message is held
	DeliverTime  *time.Time
	AttemptCount int
	// the claim of the attempt in flight
	ClaimId     *server.Id
	ClaimUntil  *time.Time
	LastError   string
	SentTime    *time.Time
	AbandonTime *time.Time
}

// A message for a state change to add.
type AccountMessageArgs struct {
	// identifies the state change that owes the message, unique per template.
	// Adding a key that exists adds nothing.
	Key          string
	NetworkId    *server.Id
	UserAuth     string
	TemplateName string
	TemplateJson string
	// a held message waits for `ReleaseAccountMessageInTx`
	Held bool
}

// The stored key: the SHA-256 of the template name and the writer's key, so
// keys of any length and content fit and keep no account data.
func AccountMessageKey(templateName string, key string) string {
	digest := sha256.New()
	digest.Write([]byte(templateName))
	digest.Write([]byte{0})
	digest.Write([]byte(key))
	return hex.EncodeToString(digest.Sum(nil))
}

// Inserts one message unless its key exists.
const addAccountMessageSql = `
	INSERT INTO account_message_outbox (
		message_id,
		message_key,
		network_id,
		user_auth,
		template_name,
		template_json,
		create_time,
		deliver_time
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	ON CONFLICT (message_key) DO NOTHING
`

// Adds the message in the caller's transaction, due now unless held. Returns
// false when a message with the key exists.
func AddAccountMessageInTx(ctx context.Context, tx server.PgTx, args *AccountMessageArgs) (added bool) {
	createTime := server.NowUtc()
	var deliverTime *time.Time
	if !args.Held {
		deliverTime = &createTime
	}
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		addAccountMessageSql,
		server.NewId(),
		AccountMessageKey(args.TemplateName, args.Key),
		args.NetworkId,
		args.UserAuth,
		args.TemplateName,
		args.TemplateJson,
		createTime,
		deliverTime,
	))
	return tag.RowsAffected() == 1
}

// Adds the messages in the caller's transaction in one round trip, as
// `AddAccountMessageInTx` adds each.
func AddAccountMessagesInTx(ctx context.Context, tx server.PgTx, argsList []*AccountMessageArgs) {
	if len(argsList) == 0 {
		return
	}
	createTime := server.NowUtc()
	server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
		for _, args := range argsList {
			var deliverTime *time.Time
			if !args.Held {
				deliverTime = &createTime
			}
			batch.Queue(
				addAccountMessageSql,
				server.NewId(),
				AccountMessageKey(args.TemplateName, args.Key),
				args.NetworkId,
				args.UserAuth,
				args.TemplateName,
				args.TemplateJson,
				createTime,
				deliverTime,
			)
		}
	})
}

// Claims the due message that has waited longest, for one delivery attempt that
// must finish before `now + lease`. A message whose earlier claim ended is due
// again. Returns nil when no message is due.
func ClaimAccountMessage(ctx context.Context, now time.Time, lease time.Duration) (message *AccountMessage) {
	claimId := server.NewId()
	claimUntil := now.Add(lease)
	server.Tx(ctx, func(tx server.PgTx) {
		message = nil
		result, err := tx.Query(
			ctx,
			`
				UPDATE account_message_outbox
				SET
					claim_id = $1,
					claim_until = $3,
					attempt_count = account_message_outbox.attempt_count + 1
				WHERE message_id = (
					SELECT message_id
					FROM account_message_outbox
					WHERE
						deliver_time IS NOT NULL AND
						deliver_time <= $2 AND
						sent_time IS NULL AND
						abandon_time IS NULL AND
						(claim_until IS NULL OR claim_until <= $2)
					ORDER BY deliver_time, message_id
					LIMIT 1
					FOR UPDATE SKIP LOCKED
				)
				RETURNING
					message_id,
					network_id,
					user_auth,
					template_name,
					template_json::text,
					create_time,
					deliver_time,
					attempt_count,
					COALESCE(last_error, '')
			`,
			claimId,
			now,
			claimUntil,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				message = &AccountMessage{
					ClaimId:    &claimId,
					ClaimUntil: &claimUntil,
				}
				server.Raise(result.Scan(
					&message.MessageId,
					&message.NetworkId,
					&message.UserAuth,
					&message.TemplateName,
					&message.TemplateJson,
					&message.CreateTime,
					&message.DeliverTime,
					&message.AttemptCount,
					&message.LastError,
				))
			}
		})
	}, server.TxReadCommitted)
	return
}

// Records that the claimed attempt sent the message. Returns false when the
// claim was lost: its lease ended and another attempt claimed the message.
func CompleteAccountMessage(ctx context.Context, messageId server.Id, claimId server.Id, sentTime time.Time) (completed bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE account_message_outbox
				SET
					sent_time = $3,
					claim_id = NULL,
					claim_until = NULL,
					last_error = NULL
				WHERE
					message_id = $1 AND
					claim_id = $2 AND
					sent_time IS NULL AND
					abandon_time IS NULL
			`,
			messageId,
			claimId,
			sentTime,
		))
		completed = tag.RowsAffected() == 1
	}, server.TxReadCommitted)
	return
}

// Records that the claimed attempt failed and makes the message due again at
// `deliverTime`. Returns false when the claim was lost.
func RetryAccountMessage(ctx context.Context, messageId server.Id, claimId server.Id, deliverTime time.Time, lastError string) (retried bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE account_message_outbox
				SET
					deliver_time = $3,
					claim_id = NULL,
					claim_until = NULL,
					last_error = $4
				WHERE
					message_id = $1 AND
					claim_id = $2 AND
					sent_time IS NULL AND
					abandon_time IS NULL
			`,
			messageId,
			claimId,
			deliverTime,
			boundedAccountMessageError(lastError),
		))
		retried = tag.RowsAffected() == 1
	}, server.TxReadCommitted)
	return
}

// Records that the claimed attempt failed and gives the message up. Returns
// false when the claim was lost.
func AbandonAccountMessage(ctx context.Context, messageId server.Id, claimId server.Id, abandonTime time.Time, lastError string) (abandoned bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE account_message_outbox
				SET
					abandon_time = $3,
					claim_id = NULL,
					claim_until = NULL,
					last_error = $4
				WHERE
					message_id = $1 AND
					claim_id = $2 AND
					sent_time IS NULL AND
					abandon_time IS NULL
			`,
			messageId,
			claimId,
			abandonTime,
			boundedAccountMessageError(lastError),
		))
		abandoned = tag.RowsAffected() == 1
	}, server.TxReadCommitted)
	return
}

// The error as stored: at most the column's length, on a rune boundary.
func boundedAccountMessageError(lastError string) string {
	if len(lastError) <= accountMessageLastErrorMaxLength {
		return lastError
	}
	end := accountMessageLastErrorMaxLength
	for 0 < end && !utf8.RuneStart(lastError[end]) {
		end -= 1
	}
	return lastError[:end]
}

// The held messages of the template added at or before `createdBefore`, oldest
// first, locked for the caller's transaction to release or remove.
func GetHeldAccountMessagesForUpdateInTx(
	ctx context.Context,
	tx server.PgTx,
	templateName string,
	createdBefore time.Time,
) []*AccountMessage {
	messages := []*AccountMessage{}
	result, err := tx.Query(
		ctx,
		`
			SELECT
				message_id,
				network_id,
				user_auth,
				template_json::text,
				create_time
			FROM account_message_outbox
			WHERE
				template_name = $1 AND
				deliver_time IS NULL AND
				create_time <= $2
			ORDER BY create_time, message_id
			FOR UPDATE
		`,
		templateName,
		createdBefore,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			message := &AccountMessage{
				TemplateName: templateName,
			}
			server.Raise(result.Scan(
				&message.MessageId,
				&message.NetworkId,
				&message.UserAuth,
				&message.TemplateJson,
				&message.CreateTime,
			))
			messages = append(messages, message)
		}
	})
	return messages
}

// Releases a held message with its final fields, due at `deliverTime`.
func ReleaseAccountMessageInTx(
	ctx context.Context,
	tx server.PgTx,
	messageId server.Id,
	templateJson string,
	deliverTime time.Time,
) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE account_message_outbox
			SET
				template_json = $2,
				deliver_time = $3
			WHERE
				message_id = $1 AND
				deliver_time IS NULL
		`,
		messageId,
		templateJson,
		deliverTime,
	))
}

// Removes held messages that will not be delivered (merged into another, or
// below a threshold).
func RemoveHeldAccountMessagesInTx(ctx context.Context, tx server.PgTx, messageIds []server.Id) {
	if len(messageIds) == 0 {
		return
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			DELETE FROM account_message_outbox
			WHERE
				message_id = ANY($1) AND
				deliver_time IS NULL
		`,
		messageIds,
	))
}

// Removes up to `limit` delivered or abandoned messages added before
// `minCreateTime`, and with them their recipients.
func RemoveFinishedAccountMessages(ctx context.Context, minCreateTime time.Time, limit int) (removedCount int64) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM account_message_outbox
				WHERE message_id IN (
					SELECT message_id
					FROM account_message_outbox
					WHERE
						(sent_time IS NOT NULL OR abandon_time IS NOT NULL) AND
						create_time < $1
					ORDER BY create_time, message_id
					LIMIT $2
				)
			`,
			minCreateTime,
			limit,
		))
		removedCount = tag.RowsAffected()
	}, server.TxReadCommitted)
	return
}

// The message with the template and writer key, or nil.
func GetAccountMessageByKey(ctx context.Context, templateName string, key string) (message *AccountMessage) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					message_id,
					network_id,
					user_auth,
					template_name,
					template_json::text,
					create_time,
					deliver_time,
					attempt_count,
					claim_id,
					claim_until,
					COALESCE(last_error, ''),
					sent_time,
					abandon_time
				FROM account_message_outbox
				WHERE message_key = $1
			`,
			AccountMessageKey(templateName, key),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				message = &AccountMessage{}
				server.Raise(result.Scan(
					&message.MessageId,
					&message.NetworkId,
					&message.UserAuth,
					&message.TemplateName,
					&message.TemplateJson,
					&message.CreateTime,
					&message.DeliverTime,
					&message.AttemptCount,
					&message.ClaimId,
					&message.ClaimUntil,
					&message.LastError,
					&message.SentTime,
					&message.AbandonTime,
				))
			}
		})
	})
	return
}

// Owes the notice that the account's password changed, to the admin's email or
// phone, in the transaction that changes it: one per reset code. An admin
// without email or phone (an account created with a wallet or SSO), or a
// network that is gone, gets none.
func addPasswordSetNoticeInTx(ctx context.Context, tx server.PgTx, networkId server.Id, userAuthResetId server.Id) {
	userAuth, err := getUserAuth(ctx, tx, networkId)
	if err != nil || userAuth == "" {
		return
	}
	normalUserAuth, _ := NormalUserAuthV1(&userAuth)
	if normalUserAuth == nil {
		return
	}
	AddAccountMessageInTx(ctx, tx, &AccountMessageArgs{
		Key:          userAuthResetId.String(),
		NetworkId:    &networkId,
		UserAuth:     *normalUserAuth,
		TemplateName: AuthPasswordSetTemplateName,
		TemplateJson: "{}",
	})
}

// Owes the welcome to the email or phone that completes a sign-up, in the
// transaction that marks it verified: one per user.
func addNetworkWelcomeInTx(ctx context.Context, tx server.PgTx, networkId server.Id, userId server.Id, userAuth string) {
	AddAccountMessageInTx(ctx, tx, &AccountMessageArgs{
		Key:          userId.String(),
		NetworkId:    &networkId,
		UserAuth:     userAuth,
		TemplateName: NetworkWelcomeTemplateName,
		TemplateJson: "{}",
	})
}
