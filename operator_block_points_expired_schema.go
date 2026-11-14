// The operator blocks the daily points award recorded expired
// (model/operator_block_points.go): a completed block whose reliability
// evidence passed client_reliability retention before any award of it ran.
// No award of such a block can be complete, so it never gets its points; the
// record keeps the daily award from stopping at it, and later blocks are
// awarded as usual. A block is recorded at most once, and never with an award
// (both under the awards' advisory lock); a commit refuses a recorded block.
//
// The migration only creates the table, empty: no existing table is locked or
// rewritten. The five-second lock bound keeps a catalog wait from stalling
// behind a long reader, and a table that exists already is left as it is, so
// a failed lock wait is safe to retry. Old binaries never read it. Apply
// before deploying the taskworker (the daily award and the served bytes
// rollup), bringyourctl (the backfill), and the api and alt (GET
// /account/epochs counts no bytes for an expired block) that use it.
package server

// The migration's statement.
const OperatorBlockPointsExpiredSql = `
	DO $operator_block_points_expired$
	BEGIN
		PERFORM set_config('lock_timeout', '5s', true);
		IF to_regclass('public.operator_block_points_expired') IS NULL THEN
			CREATE TABLE operator_block_points_expired (
				block_number bigint NOT NULL,
				window_start timestamp NOT NULL,
				window_end timestamp NOT NULL,
				reliability_min_block_number bigint NOT NULL,
				retention_min_block_number bigint NOT NULL,
				expire_time timestamp NOT NULL,
				create_time timestamp NOT NULL,
				PRIMARY KEY (block_number)
			);
		END IF;
	END
	$operator_block_points_expired$
`

var operatorBlockPointsExpiredMigration = newSqlMigration(OperatorBlockPointsExpiredSql)

// The migration's version: its position in the list, from 1.
func OperatorBlockPointsExpiredVersion() int {
	for i, migration := range migrations {
		if migration == operatorBlockPointsExpiredMigration {
			return i + 1
		}
	}
	return 0
}
