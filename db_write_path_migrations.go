// The write-path migrations of PARTITION-MAINTENANCE-PLAN.md Part B. Each is
// a named value the migration list holds, so its version is its position in
// the list wherever a merge places it: the monitor's catalog contracts and the
// tests read a version through WritePathMigrationVersion and never name one by
// number. The list in db_migrations.go keeps each migration's reasons.
package server

import (
	"fmt"
)

// One write-path migration, by what it changes.
type WritePathMigration int

const (
	WritePathUserAuthAttemptDrop WritePathMigration = iota
	WritePathHandlerHeartbeatIndexDrop
	WritePathTransferBalanceFillfactor
	WritePathDeviceFillfactor
	WritePathSearchProviderStatsFillfactor
	WritePathDebitJournalShardTimeIndex
	WritePathDebitJournalAgeIndexDrop
	WritePathDebitJournalFillfactor
	WritePathProbeCycleNextAttemptIndexDrop
	WritePathClientScoreWindowIndexDrop
	WritePathClientScoreFillfactor
	WritePathLocationReliabilityFillfactor
)

// The user_auth_attempt drop.
var userAuthAttemptDropMigration = newSqlMigration(`
		SET LOCAL lock_timeout = '5s';
		DROP TABLE IF EXISTS user_auth_attempt;
	`)

// The network_client_handler heartbeat_time index drop.
var handlerHeartbeatIndexDropMigration = newOnlineSqlMigration(
	`DROP INDEX CONCURRENTLY IF EXISTS network_client_handler_heartbeat_time`,
	`DROP INDEX IF EXISTS network_client_handler_heartbeat_time`,
)

// The transfer_balance fillfactor.
var transferBalanceFillfactorMigration = newSqlMigration(`
		SET LOCAL lock_timeout = '5s';
		ALTER TABLE transfer_balance SET (fillfactor = 85);
	`)

// The device fillfactor.
var deviceFillfactorMigration = newSqlMigration(`
		SET LOCAL lock_timeout = '5s';
		ALTER TABLE device SET (fillfactor = 90);
	`)

// The search_provider_stats fillfactor.
var searchProviderStatsFillfactorMigration = newSqlMigration(`
		SET LOCAL lock_timeout = '5s';
		ALTER TABLE search_provider_stats SET (fillfactor = 70);
	`)

// The transfer debit journal's (shard, create_time) age index.
var transferDebitJournalShardTimeIndexMigration = newRestartableOnlineSqlMigration(
	`DROP INDEX CONCURRENTLY IF EXISTS transfer_debit_journal_shard_create_time`,
	`CREATE INDEX CONCURRENTLY transfer_debit_journal_shard_create_time
		 ON transfer_debit_journal (shard, create_time)`,
	`DROP INDEX IF EXISTS transfer_debit_journal_shard_create_time;
		 CREATE INDEX transfer_debit_journal_shard_create_time
		 ON transfer_debit_journal (shard, create_time)`,
)

// The drop of the transfer debit journal's applied age index.
var transferDebitJournalAgeIndexDropMigration = newOnlineSqlMigration(
	`DROP INDEX CONCURRENTLY IF EXISTS transfer_debit_journal_age`,
	`DROP INDEX IF EXISTS transfer_debit_journal_age`,
)

// The transfer_debit_journal fillfactor.
var transferDebitJournalFillfactorMigration = newSqlMigration(`
		SET LOCAL lock_timeout = '5s';
		ALTER TABLE transfer_debit_journal SET (fillfactor = 70);
	`)

// The drop of the URL probe queue's full next-attempt index.
var probeCycleNextAttemptIndexDropMigration = newOnlineSqlMigration(
	`DROP INDEX CONCURRENTLY IF EXISTS provider_egress_probe_cycle_next_attempt`,
	`DROP INDEX IF EXISTS provider_egress_probe_cycle_next_attempt`,
)

// The drop of the client score lookback window index.
var clientScoreWindowIndexDropMigration = newOnlineSqlMigration(
	`DROP INDEX CONCURRENTLY IF EXISTS client_connection_reliability_score_lookback_max_block`,
	`DROP INDEX IF EXISTS client_connection_reliability_score_lookback_max_block`,
)

// The client_connection_reliability_score fillfactor.
var clientScoreFillfactorMigration = newSqlMigration(`
		SET LOCAL lock_timeout = '5s';
		ALTER TABLE client_connection_reliability_score SET (fillfactor = 50);
	`)

// The network_client_location_reliability fillfactor.
var locationReliabilityFillfactorMigration = newSqlMigration(`
		SET LOCAL lock_timeout = '5s';
		ALTER TABLE network_client_location_reliability SET (fillfactor = 85);
	`)

var writePathMigrations = map[WritePathMigration]any{
	WritePathUserAuthAttemptDrop:            userAuthAttemptDropMigration,
	WritePathHandlerHeartbeatIndexDrop:      handlerHeartbeatIndexDropMigration,
	WritePathTransferBalanceFillfactor:      transferBalanceFillfactorMigration,
	WritePathDeviceFillfactor:               deviceFillfactorMigration,
	WritePathSearchProviderStatsFillfactor:  searchProviderStatsFillfactorMigration,
	WritePathDebitJournalShardTimeIndex:     transferDebitJournalShardTimeIndexMigration,
	WritePathDebitJournalAgeIndexDrop:       transferDebitJournalAgeIndexDropMigration,
	WritePathDebitJournalFillfactor:         transferDebitJournalFillfactorMigration,
	WritePathProbeCycleNextAttemptIndexDrop: probeCycleNextAttemptIndexDropMigration,
	WritePathClientScoreWindowIndexDrop:     clientScoreWindowIndexDropMigration,
	WritePathClientScoreFillfactor:          clientScoreFillfactorMigration,
	WritePathLocationReliabilityFillfactor:  locationReliabilityFillfactorMigration,
}

// The version a database reaches once the migration has applied: its position
// in the migration list plus one.
func WritePathMigrationVersion(migration WritePathMigration) int {
	target, ok := writePathMigrations[migration]
	if !ok {
		panic(fmt.Errorf("unknown write-path migration %d", migration))
	}
	for index, listed := range migrations {
		if listed == target {
			return index + 1
		}
	}
	panic(fmt.Errorf("write-path migration %d is not in the migration list", migration))
}
