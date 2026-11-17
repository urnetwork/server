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
//
// A hot read of unexpired contracts bounds the id twice
// (unexpiredContractIdBoundSql):
//   - by the floor on statement_timestamp(), which decides the rows;
//   - by the partition weeks that hold every id the floor admits, computed in
//     the process and rendered into the statement as literals before it runs
//     (unexpiredContractWeeksAt), which only prune.
//
// PostgreSQL prunes partitions at plan time only by constants. With the floor
// alone, each read planned every partition of the partitioned layout and
// locked it and its indexes, 106-141 locks against 16-21 on the plain table,
// beyond the 64 fast-path slots (C1 readiness, 2026-11-17). The week literals
// prune the plan PostgreSQL caches, and change only when a week boundary
// passes the window, twice a week. The floor stays the deciding bound: a bound
// parameter in its place prunes no cached plan, since a generic plan keeps
// every partition and never costs less than a pruned one, and PostgreSQL
// estimates a range on a parameter at 0.5% of the table, which turns cached
// plans on the plain tables to the contract id indexes
// (TestCompanionCoveredRequestActualQueryAvoidsEligibleMaximum). PostgreSQL
// estimates the floor by its value, as before, and the weeks hold it.
package model

import (
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/server"
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

// Where a statement takes the bounds of its contract weeks: SQL comments, so a
// statement run without rendering them fails to parse instead of reading
// unbounded.
const (
	contractWeeksLowerSql = `/*contract weeks: lower*/`
	contractWeeksUpperSql = `/*contract weeks: upper*/`
)

// A hot read's bound on a contract id column: the floor, which decides the
// rows, and the unexpired contract weeks, which only prune. The statement runs
// rendered with unexpiredContractWeeksAt.
func unexpiredContractIdBoundSql(column string) string {
	return fmt.Sprintf(
		`%[1]s >= %[2]s AND %[1]s >= %[3]s AND %[1]s < %[4]s`,
		column,
		unexpiredContractIdFloorSql,
		contractWeeksLowerSql,
		contractWeeksUpperSql,
	)
}

// The contract ids [lower, upper) of whole partition weeks (CONTRACT-PARTITION.md,
// "Partition key and bounds": weekly, from Monday 00:00 UTC).
type contractWeeks struct {
	lower server.Id
	upper server.Id
}

// The fewest whole partition weeks that hold every id minted in [start, end].
func contractWeeksHolding(start time.Time, end time.Time) contractWeeks {
	return contractWeeks{
		lower: server.ContractIdFloor(server.ContractPartitionWeekStart(start)),
		upper: server.ContractIdFloor(server.ContractPartitionWeekStart(end).AddDate(0, 0, 7)),
	}
}

// The weeks that hold every id an unexpired read admits at now, by this
// process's clock. The floor reads the database clock, which may differ from
// this one by the margin either way, so the weeks cover the margin once more
// on each side: they start with the week of now less the unexpired age and the
// margin, and end with the week of now plus twice the margin. An id after them
// was minted by a host more than twice the margin ahead of this one.
func unexpiredContractWeeksAt(now time.Time) contractWeeks {
	return contractWeeksHolding(
		now.Add(-unexpiredContractIdAge-ContractIdCreateTimeMargin),
		now.Add(2*ContractIdCreateTimeMargin),
	)
}

// The statement with the weeks' bounds written in as uuid literals.
func (self contractWeeks) render(sql string) string {
	return strings.NewReplacer(
		contractWeeksLowerSql, contractIdLiteralSql(self.lower),
		contractWeeksUpperSql, contractIdLiteralSql(self.upper),
	).Replace(sql)
}

// A contract id as a uuid literal.
func contractIdLiteralSql(contractId server.Id) string {
	return "'" + contractId.String() + "'::uuid"
}
