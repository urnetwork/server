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

	"github.com/urnetwork/server/v2026"
)

// A contract's id leads with the millisecond its creator minted it, and its
// create_time is the database clock of its insert (clock_timestamp() in every
// creation path). The id precedes the insert by the admission wait, and the
// hosts' clocks differ by their skew, so the two agree to within this margin
// either way.
const ContractIdCreateTimeMargin = 15 * time.Minute

// An unexpired contract's id is at least the smallest id of this long ago: the
// lifetime plus the margin for the id preceding the database creation clock.
// Every creation path sets the deadline to the creation clock plus the
// lifetime, and a deadline is only ever lowered afterwards. An open contract
// older than this is past its deadline, and closing it belongs to the
// force-close sweep.
const unexpiredContractIdAge = DefaultContractExpiration + ContractIdCreateTimeMargin

// The contract id range [lower, upper) that holds every contract created in
// [start, end). A read of a create_time window bounds the contract id by it,
// so it reads a range of the primary key instead of needing an index on
// create_time, and keeps its create_time predicate, so the window is still
// exact.
//
// Write that predicate as (…) IS TRUE, which keeps the window out of every
// index condition and keeps its selectivity estimate. PostgreSQL cannot
// interpolate inside a uuid histogram bucket, so it estimates any contract id
// range at 0.5% of the table. A bare create_time range is an index condition
// of transfer_contract_destination_id_create_time through a skip scan, one
// descent per destination (332k on Main), and once no index leads with
// create_time the planner would prefer that over the primary key range. With
// create_time a filter, the primary key range is the only index path, with or
// without an index on create_time.
func ContractIdCreateTimeRange(start time.Time, end time.Time) (lower server.Id, upper server.Id) {
	return server.ContractIdFloor(start.Add(-ContractIdCreateTimeMargin)), server.ContractIdFloor(end.Add(ContractIdCreateTimeMargin))
}

// ContractIdCreateTimeRange's lower bound for a timestamptz SQL expression: the
// smallest id a contract created at or after start can have.
func ContractIdCreatedFromSql(startExpression string) string {
	return contractIdFloorSql(fmt.Sprintf(
		`(%s) - interval '%d milliseconds'`,
		startExpression,
		ContractIdCreateTimeMargin.Milliseconds(),
	))
}

// ContractIdCreateTimeRange's upper bound for a timestamptz SQL expression:
// every contract created before end has a smaller id.
func ContractIdCreatedBeforeSql(endExpression string) string {
	return contractIdFloorSql(fmt.Sprintf(
		`(%s) + interval '%d milliseconds'`,
		endExpression,
		ContractIdCreateTimeMargin.Milliseconds(),
	))
}

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
