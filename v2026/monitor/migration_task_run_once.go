// The wake handshake requires exact constant-default counters and a nullable
// requested time. Schema presence alone does not prove compatible workers.
package monitor

const taskRunOnceGenerationArtifactQuery = `(
 SELECT count(*)=3 FROM (VALUES
  ('run_once_generation','bigint',true,true),
  ('claim_generation','bigint',true,true),
  ('run_once_wake_at','timestamp without time zone',false,false)
 ) AS expected(name,type_name,required,zero_default)
 JOIN pg_attribute a ON a.attrelid=to_regclass('public.pending_task')
  AND a.attname=expected.name AND a.attnum>0 AND NOT a.attisdropped
 LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum
 WHERE format_type(a.atttypid,a.atttypmod)=expected.type_name
  AND a.atttypmod=-1 AND a.attnotnull=expected.required
  AND a.attgenerated='' AND a.attidentity=''
  AND CASE WHEN expected.zero_default
   THEN pg_get_expr(d.adbin,d.adrelid) IN ('0','0::bigint','''0''::bigint')
   ELSE d.oid IS NULL END
)`
