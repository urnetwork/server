// The optional close component shares the exact original usage SQL statement.
// Bounded overflow omits the whole component; it never advertises a partial prefix.
package model

import "github.com/urfoundation/sn/payoutartifact"

// The indexed prefix includes one sentinel. The separate CASE subquery avoids
// building a large JSON aggregate once the complete count/byte allowance is lost.
const stEpochProviderOriginalUsageSql = `
 SELECT usage.*, originals.body
 FROM (` + stEpochProviderUsageSql + `) AS usage
 LEFT JOIN LATERAL (
  WITH selected AS MATERIALIZED (
   SELECT client_id,report_id,party,acked_byte_count,unacked_byte_count,checkpoint,original_report,original_key_registration,original_key_issue
   FROM contract_close_report_evidence WHERE contract_id=usage.contract_id
   ORDER BY client_id,report_id LIMIT 1025
  )
  SELECT CASE WHEN (SELECT count(*) FROM selected) BETWEEN 1 AND 1024
    AND (SELECT COALESCE(sum(512+COALESCE(octet_length(original_report),0)+COALESCE(octet_length(original_key_registration),0)),0) FROM selected) <= 524288
   THEN (SELECT convert_to(jsonb_build_object('schema','urnetwork-original-close-report-census-v1','count',count(*),
    'reports',jsonb_agg(jsonb_build_object(
     'client_id',client_id::text,'report_id',report_id::text,'party',party,
     'acked_bytes',acked_byte_count,'unacked_bytes',unacked_byte_count,'checkpoint',checkpoint,
     'original',encode(original_report,'base64'),'key_registration',encode(original_key_registration,'base64'),'key_issue',original_key_issue
    ) ORDER BY client_id,report_id))::text,'UTF8') FROM selected)
   ELSE NULL END AS body
 ) AS originals ON true
`

// Keep the source's finite prefix contract aligned with the public decoder.
const stOriginalCloseReportLimit = payoutartifact.MaxClosedWorkReportsPerContract
