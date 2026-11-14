// The live outcome owner retains its first observation for one exact boundary.
package server

// Migration 778 adds immutable observations without rewriting session originals
// or terminal outcomes. A later close cannot replace the original open clock.
const providerWorkOpenSchemaSql = `
CREATE TABLE provider_work_open_original (
 contract_id uuid NOT NULL,
 epoch numeric(20,0) NOT NULL CHECK(epoch BETWEEN 0 AND 18446744073709551615),
 block_number numeric(20,0) NOT NULL CHECK(block_number BETWEEN 0 AND 18446744073709551615),
 block_hash bytea NOT NULL CHECK(octet_length(block_hash)=32),
 boundary_time timestamp NOT NULL,
 observed_at timestamp NOT NULL CHECK(observed_at>=boundary_time),
 reservation_hash bytea NOT NULL CHECK(octet_length(reservation_hash)=32),
 receipt_hash bytea NOT NULL UNIQUE CHECK(octet_length(receipt_hash)=32),
 original bytea NOT NULL CHECK(octet_length(original) BETWEEN 1 AND 65536),
 PRIMARY KEY(contract_id,epoch,block_hash)
);
CREATE TRIGGER provider_work_open_guard BEFORE UPDATE OR DELETE ON provider_work_open_original
 FOR EACH ROW EXECUTE FUNCTION provider_work_original_guard();
CREATE TRIGGER provider_work_open_truncate_guard BEFORE TRUNCATE ON provider_work_open_original
 FOR EACH STATEMENT EXECUTE FUNCTION provider_work_original_guard();
`
