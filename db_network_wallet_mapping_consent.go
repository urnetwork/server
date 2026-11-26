// Migration 786 retains coldkey-signed network wallet mappings, one chain per
// (domain, network) beside the unchanged per-provider chains of migration 772,
// and the earning wallet each published release epoch settled for every
// provider. All three tables are append-only.
package server

const networkWalletMappingConsentSchemaSql = `
CREATE TABLE network_wallet_mapping_challenge (
 nonce bytea PRIMARY KEY CHECK (octet_length(nonce)=32),
 domain_hash bytea NOT NULL CHECK (octet_length(domain_hash)=32),
 network_id uuid NOT NULL,
 generation bigint NOT NULL CHECK (generation BETWEEN 1 AND 4096),
 expires_at bigint NOT NULL,
 message text NOT NULL CHECK (octet_length(message) BETWEEN 1 AND 8192)
);
CREATE INDEX network_wallet_mapping_challenge_owner ON network_wallet_mapping_challenge(domain_hash,network_id,expires_at);
CREATE TABLE network_wallet_mapping_consent (
 domain_hash bytea NOT NULL CHECK (octet_length(domain_hash)=32),
 network_id uuid NOT NULL,
 generation bigint NOT NULL CHECK (generation BETWEEN 1 AND 4096),
 original_hash bytea NOT NULL UNIQUE CHECK (octet_length(original_hash)=32),
 nonce bytea NOT NULL UNIQUE REFERENCES network_wallet_mapping_challenge(nonce),
 original bytea NOT NULL CHECK (octet_length(original) BETWEEN 1 AND 12288),
 accepted_at timestamp NOT NULL,
 PRIMARY KEY(domain_hash,network_id,generation)
);
CREATE TABLE st_payout_wallet_resolution (
 deployment_key varchar(96) NOT NULL,
 epoch bigint NOT NULL,
 no_id bigint NOT NULL,
 client_id uuid NOT NULL,
 network_id uuid NOT NULL,
 mode varchar(16) NOT NULL CHECK (mode IN ('provider','network')),
 coldkey bytea NOT NULL CHECK (octet_length(coldkey)=32),
 consent_hash bytea NOT NULL CHECK (octet_length(consent_hash)=32),
 consent_generation bigint NOT NULL CHECK (consent_generation BETWEEN 1 AND 4096),
 head_hash bytea NOT NULL CHECK (octet_length(head_hash)=32),
 head_generation bigint NOT NULL CHECK (head_generation BETWEEN 1 AND 4096),
 PRIMARY KEY(deployment_key,epoch,no_id,client_id)
);
CREATE TRIGGER network_wallet_mapping_challenge_guard BEFORE UPDATE OR DELETE ON network_wallet_mapping_challenge
 FOR EACH ROW EXECUTE FUNCTION wallet_mapping_original_guard();
CREATE TRIGGER network_wallet_mapping_challenge_truncate_guard BEFORE TRUNCATE ON network_wallet_mapping_challenge
 FOR EACH STATEMENT EXECUTE FUNCTION wallet_mapping_original_guard();
CREATE TRIGGER network_wallet_mapping_consent_guard BEFORE UPDATE OR DELETE ON network_wallet_mapping_consent
 FOR EACH ROW EXECUTE FUNCTION wallet_mapping_original_guard();
CREATE TRIGGER network_wallet_mapping_consent_truncate_guard BEFORE TRUNCATE ON network_wallet_mapping_consent
 FOR EACH STATEMENT EXECUTE FUNCTION wallet_mapping_original_guard();
CREATE TRIGGER st_payout_wallet_resolution_guard BEFORE UPDATE OR DELETE ON st_payout_wallet_resolution
 FOR EACH ROW EXECUTE FUNCTION wallet_mapping_original_guard();
CREATE TRIGGER st_payout_wallet_resolution_truncate_guard BEFORE TRUNCATE ON st_payout_wallet_resolution
 FOR EACH STATEMENT EXECUTE FUNCTION wallet_mapping_original_guard();
`

// The settled earning wallets by coldkey within an epoch, smallest client
// first, with the client's network. The settlement v2 earnings readers
// (model/st_ledger_v2_reader_model.go) give each recorded amount of an epoch
// to the network of the smallest client whose resolution in that epoch
// selected the amount's coldkey. With only the primary key (deployment_key,
// epoch, no_id, client_id), each read sorted every resolution of its epochs
// by (epoch, coldkey, client_id) to keep the first of each coldkey: on Main
// (2026-11-26) GET /account/epochs read about 300 amounts by sorting about
// 0.9M resolutions, 3.6 s and the whole table's 31k pages a call, 23 calls a
// minute, and every new epoch adds about 0.25M more. With this index each
// amount reads its coldkey's smallest client in one probe.
//
// The table is append-only and about 0.3 GB; the build reads it once.
// Apply before deploying the reader that probes it: without the index each
// probe walks its epoch's resolutions in client order until its coldkey.
var stPayoutWalletResolutionColdkeyIndexMigration = newRestartableOnlineSqlMigration(
	`DROP INDEX CONCURRENTLY IF EXISTS st_payout_wallet_resolution_coldkey`,
	`CREATE INDEX CONCURRENTLY st_payout_wallet_resolution_coldkey
		 ON st_payout_wallet_resolution (deployment_key, no_id, epoch, coldkey, client_id) INCLUDE (network_id)`,
	`DROP INDEX IF EXISTS st_payout_wallet_resolution_coldkey;
		 CREATE INDEX st_payout_wallet_resolution_coldkey
		 ON st_payout_wallet_resolution (deployment_key, no_id, epoch, coldkey, client_id) INCLUDE (network_id)`,
)
