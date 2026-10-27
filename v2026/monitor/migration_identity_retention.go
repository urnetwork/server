// Identity churn retention (PARTITION-MAINTENANCE-PLAN.md Part D): the retired
// child reap index, and the key history retention custody. Their versions are
// looked up by migration content, since branches merge in any order.
package monitor

import "github.com/urnetwork/server/v2026"

var retiredChildNetworkClientIndexMigrationContract = migrationCurrentSuffix(
	server.RetiredChildNetworkClientIndexMigrationVersion(),
	"retired child client reap index",
	migrationFp2Index("network_client", "network_client_retired_child_deactivate_time", "deactivate_time, client_id",
		"((NOT active) AND (source_client_id IS NOT NULL))", false))

// The history guard keeps its row trigger and gains a truncate trigger, both
// on the replaced body; the client delete trigger records when a client went.
var clientKeyHistoryRetentionArtifactQuery = `(` +
	migrationFp2Table("st_client_key_gone") + ` AND ` +
	financialColumnsArtifact("st_client_key_gone", `('client_id','uuid','NO',NULL),('gone_time','timestamp without time zone','NO',NULL)`) + ` AND ` +
	financialConstraintsArtifact("st_client_key_gone", `('PRIMARY KEY (client_id)')`) + ` AND ` +
	financialIndexArtifact("st_client_key_gone", "st_client_key_gone_gone_time", "gone_time") + ` AND ` +
	financialGuardArtifact("st_client_key_history", "st_client_key_history_immutable", "st_client_key_history_immutable_guard", server.ClientKeyHistoryGuardBodySql(), 27) + ` AND ` +
	financialGuardArtifact("st_client_key_history", "st_client_key_history_truncate_guard", "st_client_key_history_immutable_guard", server.ClientKeyHistoryGuardBodySql(), 34) + ` AND ` +
	financialGuardArtifact("network_client", "st_client_key_retire_on_client_delete", "st_client_key_retire_deleted_client", server.ClientKeyRetireDeletedClientBodySql(), 9) + `)`

var clientKeyHistoryRetentionMigrationContract = migrationCurrentSuffix(
	server.ClientKeyHistoryRetentionMigrationVersion(),
	"client key history retention for clients that are gone",
	clientKeyHistoryRetentionArtifactQuery)
