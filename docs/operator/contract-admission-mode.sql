-- Run only after the deployment and predecessor-retirement checks in
-- contract-admission.md. This program verifies database authority and a
-- compare-and-set; it cannot establish which application processes remain.
\set ON_ERROR_STOP on
\if :{?expected_database}
\else
  \echo 'expected_database is required'
  DO $guard$ BEGIN RAISE EXCEPTION 'contract admission guard rejected; policy unchanged'; END $guard$;
\endif
\if :{?expected_enabled}
\else
  \echo 'expected_enabled is required'
  DO $guard$ BEGIN RAISE EXCEPTION 'contract admission guard rejected; policy unchanged'; END $guard$;
\endif
\if :{?desired_enabled}
\else
  \echo 'desired_enabled is required'
  DO $guard$ BEGIN RAISE EXCEPTION 'contract admission guard rejected; policy unchanged'; END $guard$;
\endif

BEGIN;
SET LOCAL statement_timeout = '3s';
SET LOCAL lock_timeout = '250ms';
SET LOCAL idle_in_transaction_session_timeout = '5s';
SELECT current_database() = :'expected_database' AND NOT pg_is_in_recovery()
    AS authority_ok \gset
\if :authority_ok
\else
  \echo 'database authority mismatch; policy unchanged'
  DO $guard$ BEGIN RAISE EXCEPTION 'contract admission guard rejected; policy unchanged'; END $guard$;
\endif
SELECT EXISTS (
    SELECT 1 FROM migration_audit
    WHERE end_version_number = 755 AND status = 'success'
) AS compatibility_installed \gset
\if :compatibility_installed
\else
  \echo 'migration 755 has not completed; policy unchanged'
  DO $guard$ BEGIN RAISE EXCEPTION 'contract admission guard rejected; policy unchanged'; END $guard$;
\endif
SELECT enabled AS previous_enabled,
       enabled = :'expected_enabled'::boolean AS expected_mode
FROM redis_contract_admission_policy WHERE singleton FOR UPDATE \gset
\if :expected_mode
\else
  \echo 'policy changed since inspection; no update attempted'
  DO $guard$ BEGIN RAISE EXCEPTION 'contract admission guard rejected; policy unchanged'; END $guard$;
\endif
UPDATE redis_contract_admission_policy
SET enabled = :'desired_enabled'::boolean
WHERE singleton;
SELECT (clock_timestamp() AT TIME ZONE 'UTC') AS changed_at_utc,
       :'previous_enabled'::boolean AS previous_enabled,
       enabled
FROM redis_contract_admission_policy WHERE singleton;
COMMIT;
