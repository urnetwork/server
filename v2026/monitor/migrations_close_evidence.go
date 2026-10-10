// Optional original evidence has independent immutable lifetime. Published
// rolling receipt ownership remains in its unchanged v764 artifact predicate.
package monitor

import "github.com/urnetwork/server/v2026"

var contractCloseEvidenceArtifactQuery = `(` +
	migrationFp2Table("contract_close_report_evidence") + ` AND ` +
	financialColumnsArtifact("contract_close_report_evidence", `('client_id','uuid','NO',NULL),('report_id','uuid','NO',NULL),('contract_id','uuid','NO',NULL),('party','character varying','NO',NULL),('acked_byte_count','bigint','NO',NULL),('unacked_byte_count','numeric','NO',NULL),('checkpoint','boolean','NO',NULL),('accepted_at','timestamp without time zone','NO',NULL)`) + ` AND ` +
	financialConstraintsArtifact("contract_close_report_evidence", `('PRIMARY KEY (client_id, report_id)'),('CHECK (((party)::text = ANY ((ARRAY[''source''::character varying, ''destination''::character varying])::text[])))'),('CHECK ((acked_byte_count >= 0))'),('CHECK (((unacked_byte_count >= (0)::numeric) AND (unacked_byte_count <= ''18446744073709551615''::numeric)))')`) + ` AND ` +
	financialIndexArtifact("contract_close_report_evidence", "contract_close_report_evidence_contract", "contract_id, party, accepted_at, report_id") + ` AND ` +
	financialGuardArtifact("contract_close_report_evidence", "contract_close_report_evidence_guard", "contract_close_report_evidence_guard", server.ContractCloseEvidenceGuardBodySql(), 27) + ` AND ` +
	financialGuardArtifact("contract_close_report_evidence", "contract_close_report_evidence_truncate_guard", "contract_close_report_evidence_guard", server.ContractCloseEvidenceGuardBodySql(), 34) + ` AND
 EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='contract_close_report_evidence' AND column_name='party' AND character_maximum_length=16) AND
 EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='contract_close_report_evidence' AND column_name='unacked_byte_count' AND numeric_precision=20 AND numeric_scale=0) AND
 NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid=to_regclass('public.contract_close_report_evidence') AND contype='f') AND
 NOT EXISTS(SELECT 1 FROM pg_index WHERE indrelid=to_regclass('public.contract_close_report_evidence') AND indisunique
 AND pg_get_indexdef(indexrelid)<>'CREATE UNIQUE INDEX contract_close_report_evidence_pkey ON public.contract_close_report_evidence USING btree (client_id, report_id)'))`

var contractCloseOriginalArtifactQuery = `(` + contractCloseEvidenceArtifactQuery + ` AND ` +
	financialColumnsArtifact("contract_close_report_evidence", `('original_report','bytea','YES',NULL),('original_key_registration','bytea','YES',NULL),('original_key_issue','character varying','NO','''''::character varying')`) + ` AND ` +
	financialConstraintsArtifact("contract_close_report_evidence", `('CHECK (((original_report IS NULL) OR ((octet_length(original_report) >= 1) AND (octet_length(original_report) <= 1024))))'),('CHECK (((original_key_registration IS NULL) OR ((octet_length(original_key_registration) >= 1) AND (octet_length(original_key_registration) <= 8192))))'),('CHECK (((original_key_issue)::text = ANY ((ARRAY[''''::character varying, ''history_not_found''::character varying, ''history_capacity''::character varying, ''history_read_unavailable''::character varying])::text[])))'),('CHECK ((((original_key_issue)::text = ''''::text) OR ((original_report IS NOT NULL) AND (original_key_registration IS NULL))))'),('CHECK (((original_key_registration IS NULL) OR (original_report IS NOT NULL)))')`) + ` AND ` +
	financialIndexArtifact("contract_close_report_evidence", "contract_close_original_census", "contract_id, client_id, report_id") + ` AND
 EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='contract_close_report_evidence' AND column_name='original_key_issue' AND character_maximum_length=64))`

// The optional chain must retain original-report presence and unchanged custody.
var contractCloseInventoryArtifactQuery = `(` + contractCloseOriginalArtifactQuery + ` AND ` +
	financialColumnsArtifact("contract_close_report_evidence", `('original_inventory','bytea','YES',NULL)`) + ` AND ` +
	financialConstraintsArtifact("contract_close_report_evidence", `('CHECK (((original_inventory IS NULL) OR ((octet_length(original_inventory) >= 1) AND (octet_length(original_inventory) <= 1024))))'),('CHECK (((original_inventory IS NULL) OR (original_report IS NOT NULL)))')`) + `)`
