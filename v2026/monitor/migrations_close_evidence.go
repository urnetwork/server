// Optional original evidence has independent immutable lifetime. Published
// rolling receipt ownership remains in its unchanged v764 artifact predicate.
package monitor

import "github.com/urnetwork/server/v2026"

// v767 published the evidence custody with the contract lookup index; 825
// drops that index and keeps the rest, so both heads share these predicates.
func contractCloseEvidenceArtifact(contractIndex string) string {
	return contractCloseEvidenceArtifactWithGuard(contractIndex, server.ContractCloseEvidenceGuardBodySql())
}

// 857 keeps the custody and replaces only the guard body.
func contractCloseEvidenceArtifactWithGuard(contractIndex string, guardBody string) string {
	return `(` +
		migrationFp2Table("contract_close_report_evidence") + ` AND ` +
		financialColumnsArtifact("contract_close_report_evidence", `('client_id','uuid','NO',NULL),('report_id','uuid','NO',NULL),('contract_id','uuid','NO',NULL),('party','character varying','NO',NULL),('acked_byte_count','bigint','NO',NULL),('unacked_byte_count','numeric','NO',NULL),('checkpoint','boolean','NO',NULL),('accepted_at','timestamp without time zone','NO',NULL)`) + ` AND ` +
		financialConstraintsArtifact("contract_close_report_evidence", `('PRIMARY KEY (client_id, report_id)'),('CHECK (((party)::text = ANY ((ARRAY[''source''::character varying, ''destination''::character varying])::text[])))'),('CHECK ((acked_byte_count >= 0))'),('CHECK (((unacked_byte_count >= (0)::numeric) AND (unacked_byte_count <= ''18446744073709551615''::numeric)))')`) + ` AND ` +
		contractIndex + ` AND ` +
		financialGuardArtifact("contract_close_report_evidence", "contract_close_report_evidence_guard", "contract_close_report_evidence_guard", guardBody, 27) + ` AND ` +
		financialGuardArtifact("contract_close_report_evidence", "contract_close_report_evidence_truncate_guard", "contract_close_report_evidence_guard", guardBody, 34) + ` AND
 EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='contract_close_report_evidence' AND column_name='party' AND character_maximum_length=16) AND
 EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='contract_close_report_evidence' AND column_name='unacked_byte_count' AND numeric_precision=20 AND numeric_scale=0) AND
 NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid=to_regclass('public.contract_close_report_evidence') AND contype='f') AND
 NOT EXISTS(SELECT 1 FROM pg_index WHERE indrelid=to_regclass('public.contract_close_report_evidence') AND indisunique
 AND pg_get_indexdef(indexrelid)<>'CREATE UNIQUE INDEX contract_close_report_evidence_pkey ON public.contract_close_report_evidence USING btree (client_id, report_id)'))`
}

func contractCloseOriginalArtifact(evidence string) string {
	return `(` + evidence + ` AND ` +
		financialColumnsArtifact("contract_close_report_evidence", `('original_report','bytea','YES',NULL),('original_key_registration','bytea','YES',NULL),('original_key_issue','character varying','NO','''''::character varying')`) + ` AND ` +
		financialConstraintsArtifact("contract_close_report_evidence", `('CHECK (((original_report IS NULL) OR ((octet_length(original_report) >= 1) AND (octet_length(original_report) <= 1024))))'),('CHECK (((original_key_registration IS NULL) OR ((octet_length(original_key_registration) >= 1) AND (octet_length(original_key_registration) <= 8192))))'),('CHECK (((original_key_issue)::text = ANY ((ARRAY[''''::character varying, ''history_not_found''::character varying, ''history_capacity''::character varying, ''history_read_unavailable''::character varying])::text[])))'),('CHECK ((((original_key_issue)::text = ''''::text) OR ((original_report IS NOT NULL) AND (original_key_registration IS NULL))))'),('CHECK (((original_key_registration IS NULL) OR (original_report IS NOT NULL)))')`) + ` AND ` +
		financialIndexArtifact("contract_close_report_evidence", "contract_close_original_census", "contract_id, client_id, report_id") + ` AND
 EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='contract_close_report_evidence' AND column_name='original_key_issue' AND character_maximum_length=64))`
}

// The optional chain must retain original-report presence and unchanged custody.
func contractCloseInventoryArtifact(original string) string {
	return `(` + original + ` AND ` +
		financialColumnsArtifact("contract_close_report_evidence", `('original_inventory','bytea','YES',NULL)`) + ` AND ` +
		financialConstraintsArtifact("contract_close_report_evidence", `('CHECK (((original_inventory IS NULL) OR ((octet_length(original_inventory) >= 1) AND (octet_length(original_inventory) <= 1024))))'),('CHECK (((original_inventory IS NULL) OR (original_report IS NOT NULL)))')`) + `)`
}

// the published v767 form of the index that 825 drops
var contractCloseEvidenceContractIndexArtifact = financialIndexArtifact("contract_close_report_evidence", "contract_close_report_evidence_contract", "contract_id, party, accepted_at, report_id")

var contractCloseEvidenceArtifactQuery = contractCloseEvidenceArtifact(contractCloseEvidenceContractIndexArtifact)

var contractCloseOriginalArtifactQuery = contractCloseOriginalArtifact(contractCloseEvidenceArtifactQuery)

var contractCloseInventoryArtifactQuery = contractCloseInventoryArtifact(contractCloseOriginalArtifactQuery)

// The complete 767-769 evidence chain once 825 has dropped the contract index.
var contractCloseEvidenceChainWithoutContractIndexArtifactQuery = contractCloseInventoryArtifact(
	contractCloseOriginalArtifact(contractCloseEvidenceArtifact(
		migrationIndexRemoved("contract_close_report_evidence", "contract_close_report_evidence_contract"))))

// 857 admits age-bounded retention deletes in the guard; the 825 custody is
// otherwise unchanged.
var contractCloseEvidenceRetentionArtifactQuery = contractCloseInventoryArtifact(
	contractCloseOriginalArtifact(contractCloseEvidenceArtifactWithGuard(
		migrationIndexRemoved("contract_close_report_evidence", "contract_close_report_evidence_contract"),
		server.ContractCloseEvidenceRetentionGuardBodySql())))

// The version of the migration whose guard also admits moves between the
// evidence partitions. It is found by its SQL, so the contract follows the
// migration wherever the migration list places it.
var contractCloseEvidenceMoveGuardMigrationVersion = server.ContractCloseEvidenceMoveGuardMigrationVersion()

// The move guard republishes the whole evidence custody, now in either layout
// (PARTITION-MAINTENANCE-PLAN.md C5): the plain table, or after its owner-run
// cutover a parent partitioned by report_id whose every leaf is an ordinary
// table. The parent keeps the columns, the key, the checks and both guards;
// its census index reads without ONLY and is valid only once every partition
// has its copy; every leaf carries the row guard's clone and its own TRUNCATE
// guard; no foreign key exists on the parent or a leaf; and the only unique
// indexes are the key and, on the leaves, its attached copies.
var contractCloseEvidenceMoveArtifactQuery = `(` +
	migrationContractTable("contract_close_report_evidence") + ` AND ` +
	financialColumnsArtifact("contract_close_report_evidence", `('client_id','uuid','NO',NULL),('report_id','uuid','NO',NULL),('contract_id','uuid','NO',NULL),('party','character varying','NO',NULL),('acked_byte_count','bigint','NO',NULL),('unacked_byte_count','numeric','NO',NULL),('checkpoint','boolean','NO',NULL),('accepted_at','timestamp without time zone','NO',NULL),('original_report','bytea','YES',NULL),('original_key_registration','bytea','YES',NULL),('original_key_issue','character varying','NO','''''::character varying'),('original_inventory','bytea','YES',NULL)`) + ` AND ` +
	financialConstraintsArtifact("contract_close_report_evidence", `('PRIMARY KEY (client_id, report_id)'),('CHECK (((party)::text = ANY ((ARRAY[''source''::character varying, ''destination''::character varying])::text[])))'),('CHECK ((acked_byte_count >= 0))'),('CHECK (((unacked_byte_count >= (0)::numeric) AND (unacked_byte_count <= ''18446744073709551615''::numeric)))'),('CHECK (((original_report IS NULL) OR ((octet_length(original_report) >= 1) AND (octet_length(original_report) <= 1024))))'),('CHECK (((original_key_registration IS NULL) OR ((octet_length(original_key_registration) >= 1) AND (octet_length(original_key_registration) <= 8192))))'),('CHECK (((original_key_issue)::text = ANY ((ARRAY[''''::character varying, ''history_not_found''::character varying, ''history_capacity''::character varying, ''history_read_unavailable''::character varying])::text[])))'),('CHECK ((((original_key_issue)::text = ''''::text) OR ((original_report IS NOT NULL) AND (original_key_registration IS NULL))))'),('CHECK (((original_key_registration IS NULL) OR (original_report IS NOT NULL)))'),('CHECK (((original_inventory IS NULL) OR ((octet_length(original_inventory) >= 1) AND (octet_length(original_inventory) <= 1024))))'),('CHECK (((original_inventory IS NULL) OR (original_report IS NOT NULL)))')`) + ` AND ` +
	`EXISTS(SELECT 1 FROM pg_index i JOIN pg_class index_relation ON index_relation.oid=i.indexrelid
 WHERE i.indrelid=to_regclass('public.contract_close_report_evidence')
 AND i.indexrelid=to_regclass('public.contract_close_original_census') AND i.indisvalid AND i.indisready
 AND regexp_replace(pg_get_indexdef(i.indexrelid), '^(CREATE (UNIQUE )?INDEX \S+ ON )ONLY ', '\1')='CREATE INDEX contract_close_original_census ON public.contract_close_report_evidence USING btree (contract_id, client_id, report_id)') AND ` +
	migrationIndexRemoved("contract_close_report_evidence", "contract_close_report_evidence_contract") + ` AND ` +
	financialGuardArtifact("contract_close_report_evidence", "contract_close_report_evidence_guard", "contract_close_report_evidence_guard", server.ContractCloseEvidenceMoveGuardBodySql(), 27) + ` AND ` +
	financialGuardArtifact("contract_close_report_evidence", "contract_close_report_evidence_truncate_guard", "contract_close_report_evidence_guard", server.ContractCloseEvidenceMoveGuardBodySql(), 34) + ` AND ` +
	migrationContractLeafTriggers("contract_close_report_evidence", "contract_close_report_evidence_guard", "contract_close_report_evidence_truncate_guard") + ` AND
 EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='contract_close_report_evidence' AND column_name='party' AND character_maximum_length=16) AND
 EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='contract_close_report_evidence' AND column_name='unacked_byte_count' AND numeric_precision=20 AND numeric_scale=0) AND
 EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='contract_close_report_evidence' AND column_name='original_key_issue' AND character_maximum_length=64) AND
 NOT EXISTS(SELECT 1 FROM pg_constraint WHERE contype='f' AND (conrelid=to_regclass('public.contract_close_report_evidence')
 OR conrelid IN (SELECT relid FROM pg_partition_tree(to_regclass('public.contract_close_report_evidence'))))) AND
 NOT EXISTS(SELECT 1 FROM pg_index i WHERE i.indrelid=to_regclass('public.contract_close_report_evidence') AND i.indisunique
 AND regexp_replace(pg_get_indexdef(i.indexrelid), '^(CREATE (UNIQUE )?INDEX \S+ ON )ONLY ', '\1')<>'CREATE UNIQUE INDEX contract_close_report_evidence_pkey ON public.contract_close_report_evidence USING btree (client_id, report_id)') AND
 NOT EXISTS(SELECT 1 FROM pg_partition_tree(to_regclass('public.contract_close_report_evidence')) tree
 JOIN pg_index i ON i.indrelid=tree.relid
 WHERE tree.relid<>to_regclass('public.contract_close_report_evidence') AND i.indisunique
 AND NOT EXISTS(SELECT 1 FROM pg_inherits attached WHERE attached.inhrelid=i.indexrelid
 AND attached.inhparent=to_regclass('public.contract_close_report_evidence_pkey'))))`
