// A closed SQL prefix excludes helper changes and manual transaction control
// before any no-COMMIT retry. No query text or arguments enter the proof.
package controller

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// Cumulative invocation counts cannot erase an earlier statement or bypass.
// Returned counts lazy rows only at Scan, not at QueryRow construction.
type competitionSupervisorReadPrefixSnapshot struct {
	workerFence                          bool
	valid                                bool
	count, returned, succeeded, bypasses uint64
}

// These are exact current-source statements, not a tunable SQL allowlist. An
// extra write, transaction-control suffix or reordered helper fails closed.
func competitionSupervisorReadPrefixStatements(workerFence bool) []struct {
	step      competitionSupervisorRenewalStep
	kind, sql string
} {
	statements := []struct {
		step      competitionSupervisorRenewalStep
		kind, sql string
	}{
		{step: competitionSupervisorRenewalScopeLock, kind: "exec", sql: "SELECT pg_advisory_xact_lock(hashtextextended($1,0))"},
		{step: competitionSupervisorRenewalLeaseRowLock, kind: "row_scan", sql: "SELECT lease_expires_at FROM competition_supervisor_lease WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND owner_id=$4 AND fencing_token=$5 FOR UPDATE"},
		{step: competitionSupervisorRenewalAuthority, kind: "row_scan", sql: "SELECT EXISTS(SELECT 1 FROM competition_supervisor_lease WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND owner_id=$4 AND fencing_token=$5 AND $6<lease_expires_at)"},
		{step: competitionSupervisorRenewalAuthority, kind: "exec", sql: "SELECT set_config('urnetwork.competition_v2_instrument_approval',$1,true),set_config('urnetwork.competition_v2_instrument_approval_json',$2,true)"},
		{step: competitionSupervisorRenewalAuthority, kind: "exec", sql: "SELECT set_config('urnetwork.competition_v2_owner',$1,true),set_config('urnetwork.competition_v2_fence',$2,true), set_config('urnetwork.competition_v2_bootstrap_lock',$3,true),set_config('urnetwork.competition_v2_bootstrap_base',$4,true), set_config('urnetwork.competition_v2_bootstrap_margin',$5,true),set_config('urnetwork.competition_v2_policy_approval',$6,true), set_config('urnetwork.competition_v2_staging',$7,true),set_config('urnetwork.competition_v2_staging_window',$8,true),set_config('urnetwork.competition_v2_staging_preparation',$9,true)"},
	}
	if workerFence {
		statements = statements[2:]
		statements[0].sql = "SELECT lease_expires_at FROM competition_supervisor_lease WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND owner_id=$4 AND fencing_token=$5 AND $6<lease_expires_at"
		for index := range statements {
			statements[index].step = competitionSupervisorWorkerFenceRead
		}
	}
	return statements
}

// Other methods pass through the already-closed heartbeat transaction surface,
// whose explicit invalidators also fail compilation if pgx adds another method.
type competitionSupervisorReadPrefixTx struct {
	*competitionHeartbeatObservedTx
	observation *competitionSupervisorRenewObservation
}

var _ server.PgTx = (*competitionSupervisorReadPrefixTx)(nil)

// Callback retries do not reset the cumulative SQL history or its validity.
func (self *competitionSupervisorRenewObservation) observeReadPrefix(tx server.PgTx, workerFence bool) server.PgTx {
	if self.readPrefix.count != 0 && self.readPrefix.workerFence != workerFence {
		self.readPrefix.valid = false
	}
	self.readPrefix.workerFence = workerFence
	return &competitionSupervisorReadPrefixTx{competitionHeartbeatObservedTx: &competitionHeartbeatObservedTx{tx: tx, observation: self.readPrefixBypass}, observation: self}
}

// Validate before delegation, retaining only finite statement counts.
func (self *competitionSupervisorReadPrefixTx) enter(kind, sql string) {
	prefix := &self.observation.readPrefix
	prefix.count++
	expected := competitionSupervisorReadPrefixStatements(prefix.workerFence)
	if prefix.count > uint64(len(expected)) {
		prefix.valid = false
		return
	}
	statement := expected[prefix.count-1]
	if statement.kind != kind || statement.step != self.observation.step || strings.Join(strings.Fields(sql), " ") != statement.sql {
		prefix.valid = false
	}
}

// Original arguments, context and reply are unchanged by observation.
func (self *competitionSupervisorReadPrefixTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	self.enter("exec", sql)
	result, err := self.tx.Exec(ctx, sql, args...)
	self.observation.readPrefix.returned++
	if err == nil {
		self.observation.readPrefix.succeeded++
	}
	return result, err
}

// A lazy row cannot establish return or success before its actual Scan.
func (self *competitionSupervisorReadPrefixTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	self.enter("row_scan", sql)
	return &competitionSupervisorReadPrefixRow{Row: self.tx.QueryRow(ctx, sql, args...), observation: self.observation}
}

// A repeated Scan or a returned-but-unscanned row cannot erase its history.
type competitionSupervisorReadPrefixRow struct {
	pgx.Row
	observation *competitionSupervisorRenewObservation
	returned    bool
}

// Preserve the exact scan reply while closing one observed statement.
func (self *competitionSupervisorReadPrefixRow) Scan(dest ...any) error {
	if self.returned {
		self.observation.readPrefix.valid = false
	}
	self.returned = true
	err := self.Row.Scan(dest...)
	self.observation.readPrefix.returned++
	if err == nil {
		self.observation.readPrefix.succeeded++
	}
	return err
}
