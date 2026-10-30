// The migration that publishes the provider work session journal's partition
// preparation (server/db_provider_work_session_partition.go,
// PARTITION-MAINTENANCE-PLAN.md C4; its version is its position,
// server.ProviderWorkSessionPartitionMigrationVersion) and republishes the rest
// of the custody 824 kept. Its predicates read either layout of
// provider_work_session_event, since the owner-run cutover
// (server.MigrateProviderWorkSessionEventsToPartitions) changes it without a
// migration:
//   - plain, as migrated, with the v776 primary key and the receipt foreign
//     key;
//   - prepared, still plain, with the identity primary key (client_id,
//     sequence, observed_at) swapped in and the receipt foreign key dropped;
//   - partitioned by RANGE (observed_at) into durable plain leaves, every leaf
//     carrying the append-only guards, its indexes read through the parent
//     (pg_get_indexdef renders a parent index ON ONLY).
//
// The receipt's reference to its event is the published trigger on every
// layout; the foreign key may remain only as the operative v776 key.
package monitor

import (
	"fmt"

	"github.com/urnetwork/server/v2026"
)

// the receipt key the cutover drops
const providerWorkSessionReceiptForeignKeyDefinition = "FOREIGN KEY (client_id, sequence) REFERENCES provider_work_session_event(client_id, sequence)"

// The journal is a durable plain table, or a parent partitioned by observed_at
// ranges whose leaves are durable plain tables.
const providerWorkSessionEventLayoutArtifactQuery = `COALESCE((SELECT relkind='r' OR (relkind='p' AND pg_get_partkeydef(oid)='RANGE (observed_at)')
 FROM pg_class WHERE oid=to_regclass('public.provider_work_session_event')), false)`

// An index of the journal by its complete definition, valid and ready, read
// with ONLY dropped from a parent index: a partitioned index is valid only once
// every leaf has its attached copy.
func providerWorkSessionEventIndexArtifact(name string, columns string) string {
	return fmt.Sprintf(`EXISTS(SELECT 1 FROM pg_index i WHERE i.indrelid=to_regclass('public.provider_work_session_event')
 AND i.indexrelid=to_regclass('public.%s') AND i.indisvalid AND i.indisready
 AND regexp_replace(pg_get_indexdef(i.indexrelid), '^(CREATE (UNIQUE )?INDEX \S+ ON )ONLY ', '\1')=%s)`,
		name, snMainnetMigrationLiteral("CREATE INDEX "+name+" ON public.provider_work_session_event USING btree ("+columns+")"))
}

// The receipt foreign key is absent, or present as the operative v776 key;
// no other key may make the receipts depend on a mutable parent.
var providerWorkSessionReceiptForeignKeyArtifactQuery = `(NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid=to_regclass('public.provider_work_session_receipt') AND contype='f') OR ` +
	snMainnetMigrationForeignKey("provider_work_session_receipt", "provider_work_session_event", providerWorkSessionReceiptForeignKeyDefinition) + `) AND ` +
	snMainnetMigrationForeignKeyCensus("provider_work_session_receipt", providerWorkSessionReceiptForeignKeyDefinition)

// The custody that migration publishes, in 824's order, with the journal, the
// receipt and the published functions read as they are after it.
func providerWorkSessionPartitionCustodyPredicates() []string {
	admitFloor, sessionAppend, receiptReference, dropGuard := server.ProviderWorkSessionPartitionBodiesSql()
	return []string{
		migrationFp2Table("provider_work_session_head"),
		snMainnetMigrationColumns("provider_work_session_head",
			snMainnetMigrationColumn{name: "client_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "sequence", kind: "bigint", notNull: true}),
		snMainnetMigrationConstraints("provider_work_session_head",
			"PRIMARY KEY (client_id)",
			"CHECK ((sequence > 0))"),
		snMainnetMigrationForeignKeyCensus("provider_work_session_head"),
		snMainnetMigrationTrigger("provider_work_session_head", "provider_work_session_head_guard", "provider_work_session_head_guard", 27),
		snMainnetMigrationTrigger("provider_work_session_head", "provider_work_session_head_truncate_guard", "provider_work_original_guard", 34),
		migrationContractTable("provider_work_session_event"),
		providerWorkSessionEventLayoutArtifactQuery,
		snMainnetMigrationColumns("provider_work_session_event",
			snMainnetMigrationColumn{name: "client_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "sequence", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "network_id", kind: "uuid"},
			snMainnetMigrationColumn{name: "connection_id", kind: "uuid"},
			snMainnetMigrationColumn{name: "kind", kind: "text", notNull: true},
			snMainnetMigrationColumn{name: "observed_at", kind: "timestamp without time zone", notNull: true},
			snMainnetMigrationColumn{name: "extender_id", kind: "uuid"},
			snMainnetMigrationColumn{name: "transaction_id", kind: "bigint", notNull: true}),
		snMainnetMigrationConstraints("provider_work_session_event",
			"CHECK ((sequence > 0))",
			"CHECK ((kind = ANY (ARRAY['baseline'::text, 'admit'::text, 'retire'::text])))"),
		// the v776 key, or the identity key the cutover swaps in: the
		// generator alone keeps a client's sequences unique
		"(" + snMainnetMigrationConstraints("provider_work_session_event", "PRIMARY KEY (client_id, sequence)") + " OR " +
			snMainnetMigrationConstraints("provider_work_session_event", "PRIMARY KEY (client_id, sequence, observed_at)") + ")",
		migrationIndexRemoved("provider_work_session_event", "provider_work_session_event_transaction"),
		providerWorkSessionEventIndexArtifact("provider_work_session_event_connection", "client_id, connection_id, sequence"),
		snMainnetMigrationForeignKeyCensus("provider_work_session_event"),
		snMainnetMigrationTrigger("provider_work_session_event", "original_guard", "provider_work_original_guard", 27),
		snMainnetMigrationTrigger("provider_work_session_event", "original_truncate_guard", "provider_work_original_guard", 34),
		migrationContractLeafTriggers("provider_work_session_event", "original_guard", "original_truncate_guard"),
		migrationFp2Table("provider_work_session_receipt"),
		snMainnetMigrationColumns("provider_work_session_receipt",
			snMainnetMigrationColumn{name: "client_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "sequence", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "receipt_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "original", kind: "bytea", notNull: true}),
		snMainnetMigrationConstraints("provider_work_session_receipt",
			"PRIMARY KEY (client_id, sequence)",
			"UNIQUE (receipt_hash)",
			"CHECK ((octet_length(receipt_hash) = 32))",
			"CHECK (((octet_length(original) >= 1) AND (octet_length(original) <= 65536)))"),
		providerWorkSessionReceiptForeignKeyArtifactQuery,
		snMainnetMigrationTrigger("provider_work_session_receipt", "original_guard", "provider_work_original_guard", 27),
		snMainnetMigrationTrigger("provider_work_session_receipt", "original_truncate_guard", "provider_work_original_guard", 34),
		// BEFORE INSERT FOR EACH ROW
		snMainnetMigrationTrigger("provider_work_session_receipt", "provider_work_session_receipt_reference", "provider_work_session_receipt_reference", 7),
		migrationFp2Table("provider_work_reservation_original"),
		snMainnetMigrationColumns("provider_work_reservation_original",
			snMainnetMigrationColumn{name: "contract_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "source_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "source_sequence", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "destination_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "destination_sequence", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "receipt_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "original", kind: "bytea", notNull: true}),
		snMainnetMigrationConstraints("provider_work_reservation_original",
			"PRIMARY KEY (contract_id)",
			"UNIQUE (receipt_hash)",
			"CHECK ((octet_length(receipt_hash) = 32))",
			"CHECK (((octet_length(original) >= 1) AND (octet_length(original) <= 65536)))"),
		snMainnetMigrationForeignKeyCensus("provider_work_reservation_original"),
		snMainnetMigrationTrigger("provider_work_reservation_original", "original_guard", "provider_work_original_guard", 27),
		snMainnetMigrationTrigger("provider_work_reservation_original", "original_truncate_guard", "provider_work_original_guard", 34),
		migrationFp2Table("provider_work_stream_original"),
		snMainnetMigrationColumns("provider_work_stream_original",
			snMainnetMigrationColumn{name: "stream_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "origin_contract_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "receipt_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "original", kind: "bytea", notNull: true}),
		snMainnetMigrationConstraints("provider_work_stream_original",
			"PRIMARY KEY (stream_id)",
			"UNIQUE (receipt_hash)",
			"CHECK ((octet_length(receipt_hash) = 32))",
			"CHECK (((octet_length(original) >= 1) AND (octet_length(original) <= 65536)))"),
		snMainnetMigrationForeignKeyCensus("provider_work_stream_original"),
		snMainnetMigrationTrigger("provider_work_stream_original", "original_guard", "provider_work_original_guard", 27),
		snMainnetMigrationTrigger("provider_work_stream_original", "original_truncate_guard", "provider_work_original_guard", 34),
		migrationFp2Table("provider_work_stream_contract"),
		snMainnetMigrationColumns("provider_work_stream_contract",
			snMainnetMigrationColumn{name: "contract_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "stream_id", kind: "uuid", notNull: true}),
		snMainnetMigrationConstraints("provider_work_stream_contract",
			"PRIMARY KEY (contract_id)",
			"FOREIGN KEY (stream_id) REFERENCES provider_work_stream_original(stream_id)"),
		snMainnetMigrationForeignKey("provider_work_stream_contract", "provider_work_stream_original", "FOREIGN KEY (stream_id) REFERENCES provider_work_stream_original(stream_id)"),
		snMainnetMigrationForeignKeyCensus("provider_work_stream_contract", "FOREIGN KEY (stream_id) REFERENCES provider_work_stream_original(stream_id)"),
		snMainnetMigrationTrigger("provider_work_stream_contract", "original_guard", "provider_work_original_guard", 27),
		snMainnetMigrationTrigger("provider_work_stream_contract", "original_truncate_guard", "provider_work_original_guard", 34),
		migrationFp2Table("provider_work_outcome_original"),
		snMainnetMigrationColumns("provider_work_outcome_original",
			snMainnetMigrationColumn{name: "contract_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "receipt_hash", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "original", kind: "bytea", notNull: true}),
		snMainnetMigrationConstraints("provider_work_outcome_original",
			"PRIMARY KEY (contract_id)",
			"UNIQUE (receipt_hash)",
			"CHECK ((octet_length(receipt_hash) = 32))",
			"CHECK (((octet_length(original) >= 1) AND (octet_length(original) <= 65536)))"),
		snMainnetMigrationForeignKeyCensus("provider_work_outcome_original"),
		snMainnetMigrationTrigger("provider_work_outcome_original", "original_guard", "provider_work_original_guard", 27),
		snMainnetMigrationTrigger("provider_work_outcome_original", "original_truncate_guard", "provider_work_original_guard", 34),
		snMainnetMigrationTrigger("network_client_connection", "provider_work_session_statement_fence", "provider_work_session_statement_fence", 30),
		snMainnetMigrationTrigger("network_client_connection", "provider_work_session_mutation", "provider_work_session_mutation", 29),
		snMainnetMigrationFunction("provider_work_endpoint_lock(uuid)", "void", "plpgsql", "v", false, snMainnetProviderEndpointBody, "client"),
		snMainnetMigrationFunction("provider_work_session_append(uuid,uuid,text,uuid)", "bigint", "plpgsql", "v", false, sessionAppend, "client", "connection", "event_kind", "extender"),
		snMainnetMigrationFunction("provider_work_session_mutation()", "trigger", "plpgsql", "v", false, snMainnetProviderSessionMutationBody),
		snMainnetMigrationFunction("provider_work_session_statement_fence()", "trigger", "plpgsql", "v", false, snMainnetProviderStatementFenceBody),
		snMainnetMigrationFunction("provider_work_session_head_guard()", "trigger", "plpgsql", "v", false, snMainnetProviderHeadBody),
		snMainnetMigrationFunction("provider_work_session_admit_floor(uuid)", "timestamp without time zone", "sql", "i", true, admitFloor, "connection"),
		snMainnetMigrationFunction("provider_work_session_receipt_reference()", "trigger", "plpgsql", "v", false, receiptReference),
		snMainnetMigrationFunction("provider_work_session_event_drop_guard(text)", "void", "plpgsql", "v", false, dropGuard, "leaf"),
	}
}
