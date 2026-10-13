// Original native fee authority, settlement and contradiction holds are additive.
package server

// Reserved migration 780. Integration appends this fragment only after the
// actual verify-original text correction at 779; it never fills a placeholder.
const stNativeFeeSettlementSchemaSql = `
CREATE TABLE st_operator_native_fee_owner (
 scope_key varchar(160) PRIMARY KEY REFERENCES st_operator_gas_budget(scope_key),
 approver_public_key varchar(64) NOT NULL,
 create_time timestamp NOT NULL
);
CREATE TABLE st_operator_native_fee_policy (
 policy_sha256 varchar(64) PRIMARY KEY,
 scope_key varchar(160) NOT NULL REFERENCES st_operator_gas_budget(scope_key),
 policy_json bytea NOT NULL,
 authority_json bytea NOT NULL,
 create_time timestamp NOT NULL
);
CREATE TABLE st_operator_native_fee_settlement (
 intent_id uuid PRIMARY KEY REFERENCES st_transaction_intent(intent_id),
 attempt int NOT NULL,
 scope_key varchar(160) NOT NULL REFERENCES st_operator_gas_budget(scope_key),
 logical_key varchar(255) NOT NULL REFERENCES st_operator_gas_intent(logical_key),
 policy_sha256 varchar(64) NOT NULL REFERENCES st_operator_native_fee_policy(policy_sha256),
 transaction_hash varchar(66) NOT NULL,
 statement_sha256 varchar(64) NOT NULL,
 statement_json bytea NOT NULL,
 debit_rao numeric(20,0) NOT NULL CHECK (debit_rao >= 0),
 debit_wei numeric(78,0) NOT NULL CHECK (debit_wei >= 0),
 original_ceiling_wei numeric(78,0) NOT NULL CHECK (original_ceiling_wei > 0),
 create_time timestamp NOT NULL,
 FOREIGN KEY(intent_id,attempt) REFERENCES st_operator_gas_reservation(intent_id,attempt),
 UNIQUE(scope_key,transaction_hash)
);
CREATE INDEX st_operator_native_fee_settlement_scope ON st_operator_native_fee_settlement(scope_key);
CREATE INDEX st_operator_native_fee_settlement_logical ON st_operator_native_fee_settlement(logical_key);
CREATE TABLE st_operator_native_fee_hold (
 intent_id uuid PRIMARY KEY REFERENCES st_transaction_intent(intent_id),
 scope_key varchar(160) NOT NULL REFERENCES st_operator_gas_budget(scope_key),
 logical_key varchar(255) NOT NULL REFERENCES st_operator_gas_intent(logical_key),
 first_policy_sha256 varchar(64) NOT NULL REFERENCES st_operator_native_fee_policy(policy_sha256),
 first_statement_sha256 varchar(64) NOT NULL,
 first_statement_json bytea NOT NULL,
 first_reason varchar(255) NOT NULL,
 maximum_policy_sha256 varchar(64) NOT NULL REFERENCES st_operator_native_fee_policy(policy_sha256),
 maximum_statement_sha256 varchar(64) NOT NULL,
 maximum_statement_json bytea NOT NULL,
 maximum_debit_wei numeric(78,0) CHECK (maximum_debit_wei >= 0),
 create_time timestamp NOT NULL,
 update_time timestamp NOT NULL
);
CREATE INDEX st_operator_native_fee_hold_scope ON st_operator_native_fee_hold(scope_key);
CREATE TABLE st_operator_native_fee_original_object (
 original_sha256 varchar(71) PRIMARY KEY,
 byte_count bigint NOT NULL CHECK (byte_count > 0),
 chunk_count int NOT NULL CHECK (chunk_count > 0),
 create_time timestamp NOT NULL
);
CREATE TABLE st_operator_native_fee_original_chunk (
 original_sha256 varchar(71) NOT NULL REFERENCES st_operator_native_fee_original_object(original_sha256),
 chunk_index int NOT NULL CHECK (chunk_index >= 0),
 original_bytes bytea NOT NULL CHECK (octet_length(original_bytes) > 0 AND octet_length(original_bytes) <= 1048576),
 PRIMARY KEY(original_sha256,chunk_index)
);
CREATE TABLE st_operator_native_fee_original_reference (
 statement_sha256 varchar(64) NOT NULL,
 kind varchar(32) NOT NULL,
 original_path text NOT NULL,
 original_sha256 varchar(71) NOT NULL REFERENCES st_operator_native_fee_original_object(original_sha256),
 PRIMARY KEY(statement_sha256,kind)
);
`
