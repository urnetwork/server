// The bytes served per operator block that GET /account/epochs reports for a
// block whose points award has not committed yet: the open block and a
// completed block still waiting for its award (model/operator_block_traffic.go).
// A rollup reads each settled stretch of sweep time once, with the award's own
// statement, and adds each provider network's bytes to its block:
//
//   - operator_block_traffic: per block, the sweep time its window is read
//     through;
//   - operator_block_network_traffic: per block and provider network, the
//     payout bytes read so far.
//
// Both stay small: a block's rows are deleted once its award commits, so the
// tables hold the open block and the blocks still waiting for an award. A
// request reads them by primary key.
//
// The migration only creates the two tables, empty: no existing table is
// locked or rewritten. The five-second lock bound keeps a catalog wait from
// stalling behind a long reader, and a table that exists already is left as
// it is, so a failed lock wait is safe to retry. Old binaries never read
// them. Apply before deploying the taskworker that advances the rollup and the
// api and alt that read it.
package server

// The migration's statement.
const OperatorBlockTrafficSql = `
	DO $operator_block_traffic$
	BEGIN
		PERFORM set_config('lock_timeout', '5s', true);
		IF to_regclass('public.operator_block_traffic') IS NULL THEN
			CREATE TABLE operator_block_traffic (
				block_number bigint NOT NULL,
				read_through timestamp NOT NULL,
				update_time timestamp NOT NULL,
				PRIMARY KEY (block_number)
			);
		END IF;
		IF to_regclass('public.operator_block_network_traffic') IS NULL THEN
			CREATE TABLE operator_block_network_traffic (
				block_number bigint NOT NULL,
				network_id uuid NOT NULL,
				payout_byte_count bigint NOT NULL,
				PRIMARY KEY (block_number, network_id)
			);
		END IF;
	END
	$operator_block_traffic$
`

var operatorBlockTrafficMigration = newSqlMigration(OperatorBlockTrafficSql)

// The migration's version: its position in the list, from 1.
func OperatorBlockTrafficVersion() int {
	for i, migration := range migrations {
		if migration == operatorBlockTrafficMigration {
			return i + 1
		}
	}
	return 0
}
