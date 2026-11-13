// Canonical migration762 custody is independent of the legacy settlement queue at763.
package monitor

const circleTransferRequestGuardBody = `
BEGIN
 IF TG_OP IN ('DELETE','TRUNCATE') THEN RAISE EXCEPTION 'customer transfer request history is retained'; END IF;
 IF (NEW.network_id,NEW.user_id,NEW.request_id,NEW.idempotency_key,NEW.basis,NEW.request_body,NEW.create_time)
   IS DISTINCT FROM (OLD.network_id,OLD.user_id,OLD.request_id,OLD.idempotency_key,OLD.basis,OLD.request_body,OLD.create_time)
   OR (OLD.challenge_id IS NOT NULL AND NEW.challenge_id IS DISTINCT FROM OLD.challenge_id)
   OR NEW.submission_count<OLD.submission_count
   OR (OLD.review_required AND NOT NEW.review_required)
   OR (OLD.challenge_status IN ('COMPLETE','FAILED','EXPIRED') AND NEW.challenge_status<>OLD.challenge_status)
 THEN RAISE EXCEPTION 'customer transfer request identity is immutable'; END IF;
 RETURN NEW;
END `
const circleTransferObservationGuardBody = `
BEGIN RAISE EXCEPTION 'customer transfer observations are append only'; END `

var circleTransferRequestArtifactQuery = `(` +
	migrationFp2Table("circle_transfer_request") + ` AND ` + migrationFp2Table("circle_transfer_observation") + ` AND ` +
	financialColumnsArtifact("circle_transfer_request", `('network_id','uuid','NO',NULL),('user_id','uuid','NO',NULL),('request_id','uuid','NO',NULL),('idempotency_key','uuid','NO',NULL),('basis','text','NO',NULL),('request_body','text','NO',NULL),('challenge_id','text','YES',NULL),('challenge_status','text','NO','''UNKNOWN''::text'),('submission_count','bigint','NO','0'),('review_required','boolean','NO','false'),('create_time','timestamp with time zone','NO','now()')`) + ` AND ` +
	financialColumnsArtifact("circle_transfer_observation", `('network_id','uuid','NO',NULL),('user_id','uuid','NO',NULL),('request_id','uuid','NO',NULL),('digest','text','NO',NULL),('detail','text','NO',NULL),('create_time','timestamp with time zone','NO','now()')`) + ` AND ` +
	financialConstraintsArtifact("circle_transfer_request", `('PRIMARY KEY (network_id, user_id, request_id)'),('UNIQUE (idempotency_key)'),('CHECK (((octet_length(basis) >= 1) AND (octet_length(basis) <= 8192)))'),('CHECK (((octet_length(request_body) >= 1) AND (octet_length(request_body) <= 8192)))'),('CHECK (((challenge_id IS NULL) OR ((octet_length(challenge_id) >= 1) AND (octet_length(challenge_id) <= 256))))'),('CHECK ((challenge_status = ANY (ARRAY[''UNKNOWN''::text, ''PENDING''::text, ''IN_PROGRESS''::text, ''COMPLETE''::text, ''FAILED''::text, ''EXPIRED''::text])))'),('CHECK ((submission_count >= 0))')`) + ` AND ` +
	financialConstraintsArtifact("circle_transfer_observation", `('PRIMARY KEY (network_id, user_id, request_id, digest)'),('FOREIGN KEY (network_id, user_id, request_id) REFERENCES circle_transfer_request(network_id, user_id, request_id)'),('CHECK ((length(digest) = 64))'),('CHECK (((octet_length(detail) >= 1) AND (octet_length(detail) <= 16384)))')`) + ` AND ` +
	financialGuardArtifact("circle_transfer_request", "circle_transfer_request_guard", "circle_transfer_request_guard", circleTransferRequestGuardBody, 27) + ` AND ` +
	financialGuardArtifact("circle_transfer_request", "circle_transfer_request_truncate_guard", "circle_transfer_request_guard", circleTransferRequestGuardBody, 34) + ` AND ` +
	financialGuardArtifact("circle_transfer_observation", "circle_transfer_observation_guard", "circle_transfer_observation_guard", circleTransferObservationGuardBody, 27) + ` AND ` +
	financialGuardArtifact("circle_transfer_observation", "circle_transfer_observation_truncate_guard", "circle_transfer_observation_guard", circleTransferObservationGuardBody, 34) + ` AND
 NOT EXISTS(SELECT 1 FROM (VALUES
 ('circle_transfer_observation','RI_FKey_check_ins',5),
 ('circle_transfer_observation','RI_FKey_check_upd',17),
 ('circle_transfer_request','RI_FKey_noaction_del',9),
 ('circle_transfer_request','RI_FKey_noaction_upd',17)
 ) AS expected(table_name,function_name,trigger_type)
 WHERE NOT EXISTS(SELECT 1 FROM pg_constraint k JOIN pg_trigger t ON t.tgconstraint=k.oid
 JOIN pg_proc p ON p.oid=t.tgfoid JOIN pg_namespace n ON n.oid=p.pronamespace
 WHERE k.conrelid=to_regclass('public.circle_transfer_observation') AND k.contype='f'
 AND pg_get_constraintdef(k.oid)='FOREIGN KEY (network_id, user_id, request_id) REFERENCES circle_transfer_request(network_id, user_id, request_id)'
 AND t.tgrelid=to_regclass('public.'||expected.table_name) AND t.tgisinternal
 AND t.tgenabled IN ('O','A') AND t.tgtype=expected.trigger_type AND t.tgqual IS NULL
 AND NOT t.tgdeferrable AND NOT t.tginitdeferred AND t.tgnargs=0
 AND n.nspname='pg_catalog' AND p.proname=expected.function_name)))`
