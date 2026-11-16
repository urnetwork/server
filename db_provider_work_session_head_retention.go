// The retention of the provider work session journal's heads (owner,
// 2026-11-16; DBDISK-STABILIZE.md S4; model/session_state_retention.go).
//
// provider_work_session_head holds each journaled client's next sequence, and
// provider_work_session_head_guard refused every delete ("journal birth cannot
// be reset"), so a head stayed for every client the append ever journaled:
// 609M rows on 2026-11-16, 96% of them clients seen for one connection. A
// head is read by the append, which advances it, by the handler retirement's
// busy check, which takes a missing head as not busy, and by the signer, which
// is not deployed and has written no receipt. Its sequence keeps the client's
// journal sequences unique while any of its events remains
// (db_provider_work_session_partition.go, "Sequence identity"); once the week
// of its last event drops at the contract horizon nothing reads it.
//
// The retention guard admits a delete only from the retention transaction,
// which opts in with urnetwork.provider_work_session_head_retention, and only
// of a head whose client has no event left in the journal and no receipt. A
// client journaled again later starts a new journal at sequence 1, which no
// retained event or receipt can collide with. Every other update and delete
// is refused as before.
package server

// The setting the head retention's transaction sets on before it deletes.
const ProviderWorkSessionHeadRetentionSetting = "urnetwork.provider_work_session_head_retention"

// What the retention guard adds to the v776 head guard, before its refusal.
const providerWorkSessionHeadRetentionClauseSql = ` IF TG_OP='DELETE' AND current_setting('urnetwork.provider_work_session_head_retention',true)='on'
  AND NOT EXISTS(SELECT 1 FROM provider_work_session_event WHERE client_id=OLD.client_id)
  AND NOT EXISTS(SELECT 1 FROM provider_work_session_receipt WHERE client_id=OLD.client_id) THEN
  RETURN OLD;
 END IF;
`

// The head guard with the retention clause. It replaces the function's body
// only, with no table lock; every update and delete the v776 body admits, it
// admits too, so it applies before or after any deploy.
const providerWorkSessionHeadRetentionGuardSql = `
SET LOCAL lock_timeout = '5s';
CREATE OR REPLACE FUNCTION provider_work_session_head_guard() RETURNS trigger LANGUAGE plpgsql AS $head_guard$
BEGIN
 IF TG_OP='UPDATE' AND NEW.client_id=OLD.client_id AND NEW.sequence=OLD.sequence+1 THEN RETURN NEW; END IF;
` + providerWorkSessionHeadRetentionClauseSql + ` RAISE EXCEPTION 'provider work journal birth cannot be reset';
END;
$head_guard$;
`
