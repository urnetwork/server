// Only the reviewed legacy and distributed graphs can prove their exact
// returned transaction continuations. SQL never enters diagnostics or caller
// authority; distributed business updates stay outside the read/config prefix.
package controller

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const competitionHeartbeatLegacySlotShape = "legacy-automated-slot-update/v1"
const competitionHeartbeatScopeLockShape = "automated-first-scope-lock/v1"
const competitionHeartbeatDistributedSlotShape = "distributed-automated-slot-update/v1"

// Source-owned statement descriptors never carry SQL arguments or private data.
type competitionHeartbeatSqlStatement struct {
	step competitionHeartbeatDatabaseStep
	kind string
	sql  string
}

// These literal statements match the exact current-main helper graph. A
// changed helper, extra method, reordered statement or transaction-control SQL
// invalidates continuation without changing the underlying database call.
func competitionHeartbeatLegacySlotStatements() []competitionHeartbeatSqlStatement {
	return []competitionHeartbeatSqlStatement{
		{step: competitionHeartbeatDatabaseScopeLock, kind: "exec", sql: "SELECT pg_advisory_xact_lock(hashtextextended($1,0))"},
		{step: competitionHeartbeatDatabaseSlotLock, kind: "row_scan", sql: "SELECT lease_expires_at FROM competition_worker_slot WHERE slot_id=1 AND worker_id=$1 AND job_id=$2 AND lease_expires_at IS NOT NULL FOR UPDATE"},
		{step: competitionHeartbeatDatabaseJobLock, kind: "row_scan", sql: "SELECT lease_expires_at,round_id,evaluation_cycle,attempt_count FROM competition_job WHERE job_id=$1 AND state='running' AND lease_owner=$2 AND lease_expires_at IS NOT NULL FOR UPDATE"},
		{step: competitionHeartbeatDatabaseJobAuthority, kind: "exec", sql: "SELECT set_config('urnetwork.competition_job_authority_id',$1,true), set_config('urnetwork.competition_job_authority_cycle',$2,true),set_config('urnetwork.competition_job_authority_attempt',$3,true)"},
		{step: competitionHeartbeatDatabaseSupervisorAuthority, kind: "row_scan", sql: "SELECT EXISTS(SELECT 1 FROM competition_supervisor_lease WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND owner_id=$4 AND fencing_token=$5 AND $6<lease_expires_at)"},
		{step: competitionHeartbeatDatabaseSupervisorAuthority, kind: "exec", sql: "SELECT set_config('urnetwork.competition_v2_instrument_approval',$1,true),set_config('urnetwork.competition_v2_instrument_approval_json',$2,true)"},
		{step: competitionHeartbeatDatabaseSupervisorAuthority, kind: "exec", sql: "SELECT set_config('urnetwork.competition_v2_owner',$1,true),set_config('urnetwork.competition_v2_fence',$2,true), set_config('urnetwork.competition_v2_bootstrap_lock',$3,true),set_config('urnetwork.competition_v2_bootstrap_base',$4,true), set_config('urnetwork.competition_v2_bootstrap_margin',$5,true),set_config('urnetwork.competition_v2_policy_approval',$6,true), set_config('urnetwork.competition_v2_staging',$7,true),set_config('urnetwork.competition_v2_staging_window',$8,true),set_config('urnetwork.competition_v2_staging_preparation',$9,true)"},
		{step: competitionHeartbeatDatabaseWorkerMode, kind: "exec", sql: "SET LOCAL urnetwork.competition_v2_worker = 'on'"},
		{step: competitionHeartbeatDatabaseSlotUpdate, kind: "exec", sql: "UPDATE competition_worker_slot SET lease_expires_at = $3, heartbeat_at = $4 WHERE slot_id = 1 AND worker_id = $1 AND job_id = $2 AND $4 < lease_expires_at"},
	}
}

// A distributed heartbeat authenticates its host/fence, then updates the job
// before its physical slot. This exact prefix is distinct from the singleton.
func competitionHeartbeatDistributedPreBusinessStatements() []competitionHeartbeatSqlStatement {
	legacy := competitionHeartbeatLegacySlotStatements()
	return []competitionHeartbeatSqlStatement{
		legacy[0],
		{step: competitionHeartbeatDatabaseSlotLock, kind: "row_scan", sql: "SELECT lease_expires_at FROM competition_execution_slot WHERE slot_id=$3 AND worker_id=$1 AND job_id=$2 AND lease_expires_at IS NOT NULL FOR UPDATE"},
		legacy[2], legacy[3],
		{step: competitionHeartbeatDatabaseJobAuthority, kind: "row_scan", sql: "SELECT EXISTS(SELECT 1 FROM competition_job j JOIN competition_execution_slot s ON s.slot_id=j.executor_slot_id WHERE j.job_id=$1 AND j.executor_host_id=$2 AND j.executor_slot_id=$3 AND j.executor_fencing_token=$4 AND s.host_id=j.executor_host_id AND s.job_id=j.job_id AND s.fencing_token=j.executor_fencing_token)"},
		{step: competitionHeartbeatDatabaseJobAuthority, kind: "exec", sql: "SELECT set_config('urnetwork.competition_executor_host',$1,true), set_config('urnetwork.competition_executor_slot',$2,true),set_config('urnetwork.competition_executor_fence',$3,true)"},
		legacy[4], legacy[5], legacy[6], legacy[7],
	}
}

// Business updates are never part of the separately reviewed read/config
// prefix. The existing final slot proof still requires this complete graph.
func competitionHeartbeatDistributedSlotStatements() []competitionHeartbeatSqlStatement {
	return append(competitionHeartbeatDistributedPreBusinessStatements(),
		competitionHeartbeatSqlStatement{step: competitionHeartbeatDatabaseJobUpdate, kind: "exec", sql: "UPDATE competition_job SET lease_expires_at = $3 WHERE job_id = $1 AND state = 'running' AND lease_owner = $2 AND $4 < lease_expires_at"},
		competitionHeartbeatSqlStatement{step: competitionHeartbeatDatabaseSlotUpdate, kind: "exec", sql: "UPDATE competition_execution_slot SET lease_expires_at = $3, heartbeat_at = $4 WHERE slot_id = $5 AND worker_id = $1 AND job_id = $2 AND $4 < lease_expires_at"},
	)
}

// One source-selected class spans the complete invocation, never a caller's
// error text. The cumulative SQL index intentionally does not reset on retry.
func (self *competitionHeartbeatDatabaseObservation) observeSql(statement *competitionHeartbeatStatementObservation, sql string) {
	if self.sqlShape == "" {
		return
	}
	self.sqlCount++
	statements := competitionHeartbeatLegacySlotStatements()
	if self.sqlShape == competitionHeartbeatScopeLockShape {
		if self.sqlCount != 1 || statement == nil || statement.Step != competitionHeartbeatDatabaseScopeLock || statement.Kind != "exec" || strings.Join(strings.Fields(sql), " ") != statements[0].sql {
			self.sqlValid = false
		}
		return
	}
	if self.sqlShape == competitionHeartbeatDistributedSlotShape {
		statements = competitionHeartbeatDistributedSlotStatements()
	} else if self.sqlShape != competitionHeartbeatLegacySlotShape {
		self.sqlValid = false
		return
	}
	if self.sqlCount == 0 || self.sqlCount > uint64(len(statements)) || statement == nil {
		self.sqlValid = false
		return
	}
	expected := statements[self.sqlCount-1]
	if statement.Step != expected.step || statement.Kind != expected.kind || strings.Join(strings.Fields(sql), " ") != expected.sql {
		self.sqlValid = false
	}
}

// Manual/nested control cannot borrow the engine's zero-COMMIT counter.
func (self *competitionHeartbeatObservedTx) Begin(ctx context.Context) (pgx.Tx, error) {
	self.observation.unknown++
	return self.tx.Begin(ctx)
}

// Calling this directly invalidates proof even when its reply is lost.
func (self *competitionHeartbeatObservedTx) Commit(ctx context.Context) error {
	self.observation.unknown++
	return self.tx.Commit(ctx)
}

// Only the engine-owned rollback census may establish normal cleanup.
func (self *competitionHeartbeatObservedTx) Rollback(ctx context.Context) error {
	self.observation.unknown++
	return self.tx.Rollback(ctx)
}

// Unreviewed streaming queries cannot be hidden by the last observed Exec.
func (self *competitionHeartbeatObservedTx) Query(ctx context.Context, sql string, arguments ...any) (pgx.Rows, error) {
	self.observation.unknown++
	return self.tx.Query(ctx, sql, arguments...)
}

// Batch execution is outside the fixed heartbeat statement shape.
func (self *competitionHeartbeatObservedTx) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	self.observation.unknown++
	return self.tx.SendBatch(ctx, batch)
}

// Bulk writes are outside the fixed heartbeat statement shape.
func (self *competitionHeartbeatObservedTx) CopyFrom(ctx context.Context, table pgx.Identifier, columns []string, source pgx.CopyFromSource) (int64, error) {
	self.observation.unknown++
	return self.tx.CopyFrom(ctx, table, columns, source)
}

// Prepared operations cannot supply unobserved transaction control.
func (self *competitionHeartbeatObservedTx) Prepare(ctx context.Context, name, sql string) (*pgconn.StatementDescription, error) {
	self.observation.unknown++
	return self.tx.Prepare(ctx, name, sql)
}

// Access to the raw connection invalidates proof before it escapes.
func (self *competitionHeartbeatObservedTx) Conn() *pgx.Conn {
	self.observation.unknown++
	return self.tx.Conn()
}

// Large-object methods are outside the reviewed heartbeat graph.
func (self *competitionHeartbeatObservedTx) LargeObjects() pgx.LargeObjects {
	self.observation.unknown++
	return self.tx.LargeObjects()
}
