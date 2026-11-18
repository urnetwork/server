package server

// Append as migration 777 after the participant/session migration. No existing
// transaction, signature or outcome is rewritten by this additive migration.
const stOperatorGasSchemaSql = `
CREATE TABLE st_operator_gas_budget (
 scope_key varchar(160) PRIMARY KEY,
 chain_id bigint NOT NULL CHECK (chain_id > 0),
 genesis_hash varchar(66) NOT NULL,
 no_id bigint NOT NULL CHECK (no_id > 0),
 approver_public_key varchar(64) NOT NULL,
 current_revision bigint NOT NULL CHECK (current_revision >= 0),
 current_policy_sha256 varchar(64) NOT NULL,
 maximum_lifetime_wei numeric(78,0) NOT NULL CHECK (maximum_lifetime_wei > 0),
 maximum_lifetime_attempts bigint NOT NULL CHECK (maximum_lifetime_attempts > 0),
 create_time timestamp NOT NULL,
 update_time timestamp NOT NULL,
 UNIQUE(chain_id, genesis_hash, no_id)
);
CREATE TABLE st_operator_gas_policy (
 policy_sha256 varchar(64) PRIMARY KEY,
 scope_key varchar(160) NOT NULL REFERENCES st_operator_gas_budget(scope_key),
 revision bigint NOT NULL CHECK (revision >= 0),
 policy_json bytea NOT NULL,
 authority_json bytea NOT NULL,
 create_time timestamp NOT NULL,
 UNIQUE(scope_key, revision)
);
CREATE TABLE st_operator_gas_account (
 chain_id bigint NOT NULL,
 genesis_hash varchar(66) NOT NULL,
 from_address varchar(42) NOT NULL,
 scope_key varchar(160) NOT NULL REFERENCES st_operator_gas_budget(scope_key),
 initial_history_sha256 varchar(64) NOT NULL,
 create_time timestamp NOT NULL,
 PRIMARY KEY(chain_id, genesis_hash, from_address)
);
CREATE INDEX st_operator_gas_account_scope ON st_operator_gas_account(scope_key);
CREATE TABLE st_operator_gas_intent (
 logical_key varchar(255) PRIMARY KEY,
 scope_key varchar(160) NOT NULL REFERENCES st_operator_gas_budget(scope_key),
 original_policy_sha256 varchar(64) NOT NULL REFERENCES st_operator_gas_policy(policy_sha256),
 maximum_liability_wei numeric(78,0) NOT NULL CHECK (maximum_liability_wei > 0),
 maximum_attempts bigint NOT NULL CHECK (maximum_attempts > 0),
 create_time timestamp NOT NULL
);
CREATE TABLE st_operator_gas_reservation (
 intent_id uuid NOT NULL REFERENCES st_transaction_intent(intent_id),
 attempt int NOT NULL CHECK (attempt > 0),
 scope_key varchar(160) NOT NULL REFERENCES st_operator_gas_budget(scope_key),
 logical_key varchar(255) NOT NULL REFERENCES st_operator_gas_intent(logical_key),
 policy_sha256 varchar(64) NOT NULL REFERENCES st_operator_gas_policy(policy_sha256),
 kind varchar(16) NOT NULL CHECK (kind IN ('execution','cancellation')),
 unsigned_transaction bytea NOT NULL,
 signing_hash varchar(66) NOT NULL,
 maximum_liability_wei numeric(78,0) NOT NULL CHECK (maximum_liability_wei > 0),
 signed_tx_hash varchar(66) NULL,
 historical boolean NOT NULL,
 create_time timestamp NOT NULL,
 PRIMARY KEY(intent_id, attempt)
);
CREATE INDEX st_operator_gas_reservation_scope ON st_operator_gas_reservation(scope_key);
CREATE INDEX st_operator_gas_reservation_logical ON st_operator_gas_reservation(logical_key);
`
