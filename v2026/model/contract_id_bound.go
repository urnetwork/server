// Contract ids lead with their 48-bit creation millisecond (connect.NewId), so
// a lower bound on the id is a lower bound on the creation time. A read of
// unexpired or open contracts bounds the id below, which keeps it to the newest
// entries of each index that has the id as a key, and to the newest partitions
// once the contract tables are partitioned by contract_id
// (CONTRACT-PARTITION.md). PostgreSQL does not carry an inequality across a
// join equality, so each joined contract table repeats the bound.
//
// The bound is rendered inline, not as a call to contract_id_floor: that
// function comes from a migration that production applies as a separate
// operator step, and a hot read must not depend on deploy order. The rendered
// expression is the function's body, so the two agree for every timestamptz.
package model

import (
	"fmt"
	"time"
)

// An unexpired contract's id is at least the smallest id of this long ago: the
// lifetime plus 15 minutes for the id preceding the database creation clock
// (admission waits, server clock skew). Every creation path sets the deadline
// to the creation clock plus the lifetime, and a deadline is only ever lowered
// afterwards. An open contract older than this is past its deadline, and
// closing it belongs to the force-close sweep.
const unexpiredContractIdAge = DefaultContractExpiration + 15*time.Minute

// The smallest contract id of the millisecond of a timestamptz SQL expression,
// contract_id_floor's body. A timestamp without time zone would be read as UTC
// here but converted in the session time zone by contract_id_floor, so callers
// pass timestamptz.
func contractIdFloorSql(timeExpression string) string {
	return fmt.Sprintf(
		`(lpad(to_hex(floor(extract(epoch FROM (%s)) * 1000)::bigint), 12, '0') || '00000000000000000000')::uuid`,
		timeExpression,
	)
}

// The smallest id an unexpired contract can have at the start of the statement.
// The interval has no day part, so the session time zone cannot change it.
var unexpiredContractIdFloorSql = contractIdFloorSql(fmt.Sprintf(
	`statement_timestamp() - interval '%d milliseconds'`,
	unexpiredContractIdAge.Milliseconds(),
))
