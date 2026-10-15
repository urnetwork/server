// Additive session association, durable operations, and index repair state.
package server

const sessionSchemaSql = `
ALTER TABLE network_client ADD COLUMN session_id uuid NULL;
ALTER TABLE network_client ADD COLUMN session_create_time timestamp NULL;
ALTER TABLE auth_code ADD COLUMN origin_session_id uuid NULL;
CREATE TABLE network_session_operation (
 network_id uuid NOT NULL, operation_id uuid NOT NULL, action varchar(16) NOT NULL,
 fingerprint varchar(64) NOT NULL, actor jsonb NOT NULL, credential_epoch timestamp NOT NULL,
 target_session_id uuid NULL, kept_session_id uuid NULL,
 status varchar(16) NOT NULL, result jsonb NULL, target_session_ids jsonb NOT NULL DEFAULT '[]',
 quota_reserved boolean NOT NULL DEFAULT true, create_time timestamp NOT NULL,
 update_time timestamp NOT NULL, retain_until timestamp NOT NULL,
 PRIMARY KEY(network_id,operation_id)
);
CREATE INDEX network_session_operation_recovery ON network_session_operation(update_time,network_id,operation_id) WHERE status IN ('prepared','enforced');
CREATE INDEX network_session_operation_quota ON network_session_operation(network_id,action,create_time) WHERE quota_reserved;
CREATE TABLE auth_code_redemption (
 code_sha256 varchar(64) NOT NULL, request_id uuid NOT NULL, network_id uuid NOT NULL,
 fingerprint varchar(64) NOT NULL, credential jsonb NOT NULL, status varchar(16) NOT NULL,
 result jsonb NULL, retain_until timestamp NOT NULL, PRIMARY KEY(code_sha256,request_id), UNIQUE(request_id)
);
CREATE INDEX auth_code_redemption_retention ON auth_code_redemption(retain_until);
CREATE TABLE network_session_index_outbox (
 network_id uuid NOT NULL PRIMARY KEY, revision uuid NOT NULL, next_review_time timestamp NOT NULL,
 update_time timestamp NOT NULL
);
`
const sessionClientIndexSql = `CREATE INDEX network_client_session_active ON network_client(network_id,session_id,client_id) WHERE active AND session_id IS NOT NULL`
const sessionAuthCodeIndexSql = `CREATE INDEX auth_code_origin_session ON auth_code(network_id,origin_session_id) WHERE active AND origin_session_id IS NOT NULL`
