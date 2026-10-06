package server

// The account message outbox (model/account_message_outbox_model.go). A state
// change that owes its account an email or SMS writes the message here in its
// own transaction, so the message exists exactly when the change commits; the
// controller's delivery task sends due messages after the commit.
//
// message_key is the SHA-256 of the template name and the key the writer derives
// from its state change, so a writer that runs again adds nothing. A row with no
// deliver_time is held for a later release (one missing-wallet notice per payout
// run). claim_id and claim_until lease a row to the one delivery attempt in
// flight; attempt_count counts claims. Delivered (sent_time) and abandoned
// (abandon_time) rows are removed after a retention period, and with them the
// recipient.
const accountMessageOutboxSchemaSql = `
	CREATE TABLE account_message_outbox (
		message_id uuid NOT NULL,
		message_key char(64) NOT NULL,
		network_id uuid NULL,
		user_auth varchar(256) NOT NULL,
		template_name varchar(64) NOT NULL,
		template_json jsonb NOT NULL,
		create_time timestamp NOT NULL,
		deliver_time timestamp NULL,
		attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
		claim_id uuid NULL,
		claim_until timestamp NULL,
		last_error varchar(512) NULL,
		sent_time timestamp NULL,
		abandon_time timestamp NULL,

		PRIMARY KEY (message_id),
		UNIQUE (message_key)
	);

	CREATE INDEX account_message_outbox_due
		ON account_message_outbox (deliver_time, message_id)
		WHERE deliver_time IS NOT NULL AND sent_time IS NULL AND abandon_time IS NULL;

	CREATE INDEX account_message_outbox_held
		ON account_message_outbox (template_name, create_time, message_id)
		WHERE deliver_time IS NULL;

	CREATE INDEX account_message_outbox_finished
		ON account_message_outbox (create_time, message_id)
		WHERE sent_time IS NOT NULL OR abandon_time IS NOT NULL;
`
