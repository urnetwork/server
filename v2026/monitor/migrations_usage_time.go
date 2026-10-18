// Missing terminal timestamps cannot be assigned to an epoch. Admission must
// retain the bounded lookup that exposes this debt before constructing payouts.
package monitor

import "strings"

const providerUsageTimeIndexDefinition = "CREATE INDEX transfer_contract_usage_missing_time ON public.transfer_contract USING btree (contract_id) WHERE ((close_time IS NULL) AND ((outcome)::text = ANY (ARRAY[('settled'::character varying)::text, ('dispute_resolved_to_source'::character varying)::text, ('dispute_resolved_to_destination'::character varying)::text])))"

// PostgreSQL's dump/restore of the same varchar predicate moves the text
// coercion from each array element onto the array. Admit both exact retained
// renderings, never a weaker predicate or differently ordered index.
const providerUsageTimeRestoredIndexDefinition = "CREATE INDEX transfer_contract_usage_missing_time ON public.transfer_contract USING btree (contract_id) WHERE ((close_time IS NULL) AND ((outcome)::text = ANY ((ARRAY['settled'::character varying, 'dispute_resolved_to_source'::character varying, 'dispute_resolved_to_destination'::character varying])::text[])))"

var providerUsageTimeIndexArtifactQuery = `EXISTS (
	SELECT 1 FROM pg_index AS index_record
	JOIN pg_class AS index_relation ON index_relation.oid = index_record.indexrelid
	WHERE index_record.indrelid = to_regclass('public.transfer_contract')
		AND index_relation.relname = 'transfer_contract_usage_missing_time'
		AND index_record.indisvalid AND index_record.indisready AND index_record.indislive
		AND regexp_replace(pg_get_indexdef(index_relation.oid), '[[:space:]]+', ' ', 'g')
			IN ('` + strings.ReplaceAll(providerUsageTimeIndexDefinition, "'", "''") + `',
				'` + strings.ReplaceAll(providerUsageTimeRestoredIndexDefinition, "'", "''") + `')
)`
