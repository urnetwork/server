// Identity churn retention (PARTITION-MAINTENANCE-PLAN.md Part D): the retired
// child reap index, and the key history retention custody. Their versions are
// looked up by migration content, since branches merge in any order.
package monitor

import "github.com/urnetwork/server"

var retiredChildNetworkClientIndexMigrationContract = migrationCurrentSuffix(
	server.RetiredChildNetworkClientIndexMigrationVersion(),
	"retired child client reap index",
	migrationFp2Index("network_client", "network_client_retired_child_deactivate_time", "deactivate_time, client_id",
		"((NOT active) AND (source_client_id IS NOT NULL))", false))

// The record of when a client went, which both key history contracts keep.
var clientKeyGoneArtifactQuery = migrationFp2Table("st_client_key_gone") + ` AND ` +
	financialColumnsArtifact("st_client_key_gone", `('client_id','uuid','NO',NULL),('gone_time','timestamp without time zone','NO',NULL)`) + ` AND ` +
	financialConstraintsArtifact("st_client_key_gone", `('PRIMARY KEY (client_id)')`) + ` AND ` +
	financialIndexArtifact("st_client_key_gone", "st_client_key_gone_gone_time", "gone_time")

// The history guard keeps its row trigger and gains a truncate trigger, both
// on the replaced body; the client delete trigger records when a client went.
var clientKeyHistoryRetentionArtifactQuery = `(` +
	clientKeyGoneArtifactQuery + ` AND ` +
	financialGuardArtifact("st_client_key_history", "st_client_key_history_immutable", "st_client_key_history_immutable_guard", server.ClientKeyHistoryGuardBodySql(), 27) + ` AND ` +
	financialGuardArtifact("st_client_key_history", "st_client_key_history_truncate_guard", "st_client_key_history_immutable_guard", server.ClientKeyHistoryGuardBodySql(), 34) + ` AND ` +
	financialGuardArtifact("network_client", "st_client_key_retire_on_client_delete", "st_client_key_retire_deleted_client", server.ClientKeyRetireDeletedClientBodySql(), 9) + `)`

// The floor migration replaces the guard body this pins, so the floor's
// contract republishes the rest from its version on.
var clientKeyHistoryRetentionMigrationContract = migrationCurrentSuffix(
	server.ClientKeyHistoryRetentionMigrationVersion(),
	"client key history retention for clients that are gone",
	clientKeyHistoryRetentionArtifactQuery).removedAt(server.ClientKeyHistoryRetentionFloorMigrationVersion())

// Both history triggers on the body that reads the floor, the floor function
// with its exact body, and the unchanged record of when a client went. The
// 91-day body is drift from here on, as is a floor with any other body.
var clientKeyHistoryRetentionFloorArtifactQuery = `(` +
	clientKeyGoneArtifactQuery + ` AND ` +
	financialGuardArtifact("st_client_key_history", "st_client_key_history_immutable", "st_client_key_history_immutable_guard", server.ClientKeyHistoryFloorGuardBodySql(), 27) + ` AND ` +
	financialGuardArtifact("st_client_key_history", "st_client_key_history_truncate_guard", "st_client_key_history_immutable_guard", server.ClientKeyHistoryFloorGuardBodySql(), 34) + ` AND ` +
	financialGuardArtifact("network_client", "st_client_key_retire_on_client_delete", "st_client_key_retire_deleted_client", server.ClientKeyRetireDeletedClientBodySql(), 9) + ` AND ` +
	snMainnetMigrationFunction("st_client_key_history_retention_floor()", "interval", "sql", "s", false, server.ClientKeyHistoryRetentionFloorBodySql()) + `)`

var clientKeyHistoryRetentionFloorMigrationContract = migrationCurrentSuffix(
	server.ClientKeyHistoryRetentionFloorMigrationVersion(),
	"client key history retention floor below the contract horizon",
	clientKeyHistoryRetentionFloorArtifactQuery)
