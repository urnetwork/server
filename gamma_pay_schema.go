package server

// Pay with Gamma (mmm/BTCPAY.md §18, controller/gamma_*.go): seven new empty
// tables and their indexes. Nothing existing is locked or
// rewritten, and no earlier release reads or writes them. Apply before
// deploying the api, alt and taskworker that use them.
//
//   - gamma_deposit_hotkey: the deposit hotkeys we assigned, each to one
//     account, by global derivation index, with the biller's last look at each
//     hotkey's deposit on chain
//   - gamma_account: an account that pays with gamma: its automatic renewal
//     and top-up switches (on by default), its top-up item and monthly limit
//   - gamma_payment_intent: an invoice payable from the account's deposit,
//     bought by the customer or made by the biller for a renewal, a data
//     top-up or a seat upgrade; an automatic one is unique per (account,
//     purpose, period), and every intent has one public 32-byte reference
//   - gamma_charge: the intent's spend entry, recorded before it is ever
//     submitted, with the intent's reference; at most one per intent, re-armed
//     after a skip only once the deposit changes
//   - gamma_spend_batch: one signed Proxy.proxy(spend_gamma) extrinsic, kept
//     with its exact bytes, nonce and era; at most one is live at a time
//   - gamma_chain_state: the watcher's cursor and what the terms endpoint shows
//   - gamma_alarm: what the watcher saw that it must never see, and plain TAO
//     sent to a deposit hotkey by mistake
const gammaPaySchemaSql = `
		CREATE TABLE gamma_deposit_hotkey (
			hotkey varchar(64) NOT NULL PRIMARY KEY,
			network_id uuid NOT NULL,
			derivation_index bigint NOT NULL,
			create_time timestamp NOT NULL,
			observed_block bigint NULL,
			observed_at timestamp NULL,
			deposit_coldkey varchar(64) NULL,
			deposit_spender varchar(64) NULL,
			deposit_balance bigint NULL,
			deposit_spendable bigint NULL,
			deposit_daily_cap bigint NULL,
			hotkey_owner varchar(64) NULL,
			stray_tao_rao bigint NOT NULL DEFAULT 0
		);
		CREATE UNIQUE INDEX gamma_deposit_hotkey_index_unique
		ON gamma_deposit_hotkey (derivation_index);
		CREATE INDEX gamma_deposit_hotkey_network
		ON gamma_deposit_hotkey (network_id, derivation_index);

		CREATE TABLE gamma_account (
			network_id uuid NOT NULL PRIMARY KEY,
			auto_renew bool NOT NULL DEFAULT true,
			auto_topup bool NOT NULL DEFAULT true,
			topup_item varchar(32) NOT NULL DEFAULT 'data_1tib',
			monthly_limit bigint NULL,
			renew_plan varchar(32) NULL,
			price_tier varchar(32) NULL,
			hotkey varchar(64) NULL,
			last_balance_bytes bigint NULL,
			last_balance_at timestamp NULL,
			seat_demand_at timestamp NULL,
			create_time timestamp NOT NULL,
			update_time timestamp NOT NULL
		);

		CREATE TABLE gamma_payment_intent (
			intent_id uuid NOT NULL PRIMARY KEY,
			status_token_hash bytea NOT NULL,
			extra_status_token_hashes bytea[] NOT NULL DEFAULT '{}',
			network_id uuid NOT NULL,
			hotkey varchar(64) NOT NULL,
			purpose varchar(16) NOT NULL,
			period_key varchar(64) NULL,
			reference bytea NOT NULL,
			delivery varchar(16) NOT NULL,
			item varchar(32) NOT NULL,
			tier varchar(32) NULL,
			offer_applied bool NOT NULL DEFAULT false,
			price_usd_nano_cents bigint NOT NULL,
			regular_price_usd_nano_cents bigint NOT NULL,
			email varchar(256) NULL,
			term_start_time timestamp NULL,
			create_time timestamp NOT NULL,
			expires_at timestamp NOT NULL,
			credited_at timestamp NULL,
			balance_id uuid NULL,
			balance_code_id uuid NULL,
			credit_failure_count int NOT NULL DEFAULT 0,
			voided_at timestamp NULL
		);
		CREATE UNIQUE INDEX gamma_payment_intent_reference
		ON gamma_payment_intent (reference);
		CREATE UNIQUE INDEX gamma_payment_intent_period
		ON gamma_payment_intent (network_id, purpose, period_key)
		WHERE period_key IS NOT NULL;
		CREATE INDEX gamma_payment_intent_open
		ON gamma_payment_intent (create_time)
		WHERE credited_at IS NULL AND voided_at IS NULL;
		CREATE INDEX gamma_payment_intent_network
		ON gamma_payment_intent (network_id, create_time);

		CREATE TABLE gamma_charge (
			reference bytea NOT NULL PRIMARY KEY,
			intent_id uuid NOT NULL,
			hotkey varchar(64) NOT NULL,
			amount bigint NOT NULL,
			state varchar(16) NOT NULL,
			batch_id uuid NULL,
			attempts int NOT NULL DEFAULT 0,
			create_time timestamp NOT NULL,
			resolved_at timestamp NULL,
			block_number bigint NULL,
			block_hash varchar(66) NULL,
			event_index int NULL,
			skip_reason varchar(64) NULL,
			skip_deposit varchar(256) NULL,
			notice_sent_at timestamp NULL
		);
		CREATE UNIQUE INDEX gamma_charge_intent
		ON gamma_charge (intent_id);
		CREATE INDEX gamma_charge_batch
		ON gamma_charge (batch_id)
		WHERE batch_id IS NOT NULL;
		CREATE INDEX gamma_charge_open
		ON gamma_charge (state, create_time)
		WHERE state IN ('pending', 'spent', 'skipped');

		CREATE TABLE gamma_spend_batch (
			batch_id uuid NOT NULL PRIMARY KEY,
			nonce bigint NOT NULL,
			era_birth bigint NOT NULL,
			era_period int NOT NULL,
			extrinsic bytea NOT NULL,
			extrinsic_hash varchar(66) NOT NULL,
			state varchar(16) NOT NULL,
			create_time timestamp NOT NULL,
			submitted_at timestamp NULL,
			resolved_at timestamp NULL,
			block_number bigint NULL,
			block_hash varchar(66) NULL,
			extrinsic_index int NULL,
			fee_rao bigint NULL,
			note text NULL
		);
		CREATE UNIQUE INDEX gamma_spend_batch_extrinsic_hash
		ON gamma_spend_batch (extrinsic_hash);
		CREATE UNIQUE INDEX gamma_spend_batch_live
		ON gamma_spend_batch ((true))
		WHERE state IN ('signed', 'submitted');

		CREATE TABLE gamma_chain_state (
			singleton bool NOT NULL PRIMARY KEY DEFAULT true CHECK (singleton),
			scanned_block bigint NULL,
			scanned_hash varchar(66) NULL,
			finalized_block bigint NULL,
			runtime_spec int NULL,
			readiness varchar(48) NOT NULL,
			mint_readiness varchar(48) NOT NULL,
			terms jsonb NULL,
			update_time timestamp NOT NULL
		);

		CREATE TABLE gamma_alarm (
			alarm_key varchar(160) NOT NULL PRIMARY KEY,
			kind varchar(48) NOT NULL,
			block_number bigint NULL,
			detail text NOT NULL,
			create_time timestamp NOT NULL
		);
		CREATE INDEX gamma_alarm_create_time
		ON gamma_alarm (create_time);
	`

// The catalog monitor follows this immutable DDL wherever merges place it.
func GammaPaySchemaMigrationVersion() int {
	return sqlMigrationVersion(gammaPaySchemaSql)
}
