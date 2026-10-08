// The optional close component shares the exact original usage SQL statement.
// Bounded overflow omits the whole component; it never advertises a partial prefix.
package model

import "github.com/urfoundation/sn/v2026/payoutartifact"

// The indexed prefix includes one sentinel. The separate CASE subquery avoids
// building a large JSON aggregate once the complete count/byte allowance is lost.
const stEpochProviderOriginalUsageSql = `
 WITH usage_rows AS MATERIALIZED (` + stEpochProviderUsageSql + `),
 window_rows AS MATERIALIZED (
  SELECT * FROM (
   SELECT contract_id, 'credited'::text AS disposition, close_time, provider_usage FROM usage_rows
   UNION ALL
   SELECT contract_id, CASE WHEN outcome IS NULL THEN 'open' WHEN close_time IS NULL THEN 'unassigned_canceled' ELSE 'canceled' END,
     close_time, NULL::jsonb FROM transfer_contract
   WHERE (outcome IS NULL AND create_time < $2)
      OR (outcome='canceled' AND (close_time IS NULL OR ($1<=close_time AND close_time<$2)))
  ) AS all_rows ORDER BY contract_id LIMIT 32769
 ), window_body AS MATERIALIZED (
  SELECT CASE WHEN count(*)<=32768 AND COALESCE(sum(512+octet_length(COALESCE(provider_usage::text,''))),0)<=524288
   THEN jsonb_build_object('schema','urnetwork-closed-work-window-inventory-v1',
    'start',to_char($1::timestamp,'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
    'end',to_char($2::timestamp,'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
    'records',COALESCE(jsonb_agg(jsonb_build_object('contract_id',contract_id::text,'disposition',disposition,
       'closed_at',to_char(close_time,'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
       'original_snapshot',CASE WHEN provider_usage IS NULL THEN NULL ELSE encode(convert_to(provider_usage::text,'UTF8'),'base64') END)
      ORDER BY contract_id),'[]'::jsonb)) ELSE NULL END AS body FROM window_rows
 )
 SELECT usage.*, CASE WHEN octet_length(combined.body)<=1048576 THEN combined.body ELSE originals.body END, false AS window_only
 FROM usage_rows AS usage CROSS JOIN window_body
 LEFT JOIN LATERAL (
  WITH selected AS MATERIALIZED (
   SELECT client_id,report_id,party,acked_byte_count,unacked_byte_count,checkpoint,original_report,original_key_registration,original_key_issue,original_inventory
   FROM contract_close_report_evidence WHERE contract_id=usage.contract_id
   ORDER BY client_id,report_id LIMIT 1025
  )
  SELECT CASE WHEN (SELECT count(*) FROM selected) BETWEEN 1 AND 1024
    AND (SELECT COALESCE(sum(512+COALESCE(octet_length(original_report),0)+COALESCE(octet_length(original_key_registration),0)+COALESCE(octet_length(original_inventory),0)),0) FROM selected) <= 524288
   THEN (SELECT convert_to(jsonb_build_object('schema',CASE WHEN bool_or(original_inventory IS NOT NULL) THEN 'urnetwork-original-close-report-census-v2' ELSE 'urnetwork-original-close-report-census-v1' END,'count',count(*),
    'reports',jsonb_agg(jsonb_build_object(
     'client_id',client_id::text,'report_id',report_id::text,'party',party,
     'acked_bytes',acked_byte_count,'unacked_bytes',unacked_byte_count,'checkpoint',checkpoint,
     'original',encode(original_report,'base64'),'key_registration',encode(original_key_registration,'base64'),'key_issue',original_key_issue
    ) || CASE WHEN original_inventory IS NULL THEN '{}'::jsonb ELSE jsonb_build_object('inventory',encode(original_inventory,'base64')) END ORDER BY client_id,report_id))::text,'UTF8') FROM selected)
   ELSE NULL END AS body
 ) AS originals ON true
 LEFT JOIN LATERAL (
  SELECT CASE WHEN usage.contract_id=(SELECT min(contract_id::text)::uuid FROM usage_rows) AND window_body.body IS NOT NULL
   THEN convert_to((COALESCE(convert_from(originals.body,'UTF8')::jsonb,jsonb_build_object('count',0,'reports','[]'::jsonb))
      || jsonb_build_object('schema','urnetwork-original-close-report-census-v2','window',window_body.body))::text,'UTF8')
   ELSE originals.body END AS body
 ) AS combined ON true
 UNION ALL
 SELECT '00000000-0000-0000-0000-000000000000'::uuid,NULL::jsonb,NULL::timestamp,false,
   convert_to(window_body.body::text,'UTF8'),true FROM window_body
`

// Keep the source's finite prefix contract aligned with the public decoder.
const stOriginalCloseReportLimit = payoutartifact.MaxClosedWorkReportsPerContract
