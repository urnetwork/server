// Range partitioning and retention of the contract tables (CONTRACT-PARTITION.md).
package server

// The smallest contract id of a millisecond. Contract ids lead with their
// 48-bit creation millisecond (connect.NewId), so an id range is a time range:
// the partition bounds and the legacy partition's bound check derive from this
// function. It is IMMUTABLE, which a CHECK constraint requires: an epoch does
// not depend on the session time zone.
const contractIdFloorSql = `
		CREATE OR REPLACE FUNCTION contract_id_floor(t timestamptz) RETURNS uuid
		LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
		AS $$
			SELECT (lpad(to_hex(floor(extract(epoch FROM t) * 1000)::bigint), 12, '0') || '00000000000000000000')::uuid
		$$
	`
