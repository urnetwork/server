// Deployment proves the close-owner schema; runtime workers do not probe it.
package monitor

import "github.com/urnetwork/server"

var contractCloseOwnerArtifactQueries = []string{
	`(EXISTS (
 SELECT 1 FROM pg_attribute a
 WHERE a.attrelid=to_regclass('public.legacy_settlement_intent')
 AND a.attname='source_client_id' AND a.attnum>0 AND NOT a.attisdropped
 AND a.atttypid='uuid'::regtype AND a.atttypmod=-1 AND NOT a.attnotnull
 AND a.attgenerated='' AND a.attidentity=''
 AND NOT EXISTS(SELECT 1 FROM pg_attrdef d WHERE d.adrelid=a.attrelid AND d.adnum=a.attnum)
) AND ` + financialGuardArtifact("legacy_settlement_intent", "legacy_settlement_intent_resolve_close_owner",
		"assign_legacy_settlement_close_owner", server.ContractCloseOwnerGuardBody, 23, "payer_network_id", "source_client_id") + `)`,
	migrationFp2Index("legacy_settlement_intent", "legacy_settlement_intent_source_due",
		"shard, source_client_id, next_attempt_time, contract_id", "((payer_network_id IS NULL) AND (source_client_id IS NOT NULL))", false),
	migrationFp2Index("legacy_settlement_intent", "legacy_settlement_intent_owner_missing",
		"shard, contract_id", "(source_client_id IS NULL)", false),
}
