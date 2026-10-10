// Observed-zero reliability requires durable counts and mixed-writer detection.
package monitor

var migrationReliabilityObservationArtifactQuery = "(" +
	migrationFp2Columns("client_reliability_running",
		migrationFp2Column{name: "observed_row_count", kind: "bigint", notNull: true, defaultExpression: "0"}) + " AND " +
	migrationFp2Constraint("client_reliability_running", "c", "CHECK ((observed_row_count >= 0))") + " AND " +
	migrationFp2Columns("client_reliability_running_window",
		migrationFp2Column{name: "observation_version", kind: "smallint", notNull: true, defaultExpression: "0"},
		migrationFp2Column{name: "observation_write_token", kind: "uuid"}) + ` AND EXISTS (
		SELECT 1 FROM pg_trigger AS trigger_record
		JOIN pg_proc AS function_record ON function_record.oid=trigger_record.tgfoid
		JOIN pg_namespace AS namespace ON namespace.oid=function_record.pronamespace
		JOIN pg_language AS language ON language.oid=function_record.prolang
		WHERE trigger_record.tgrelid=to_regclass('public.client_reliability_running_window')
		AND trigger_record.tgname='client_reliability_running_window_observation_guard'
		AND NOT trigger_record.tgisinternal AND trigger_record.tgtype=23
		AND trigger_record.tgenabled IN ('O','A') AND trigger_record.tgqual IS NULL
		AND trigger_record.tgnargs=0 AND trigger_record.tgargs=''::bytea AND trigger_record.tgattr=''::int2vector
		AND namespace.nspname='public' AND function_record.proname='client_reliability_running_window_observation_guard'
		AND function_record.pronargs=0 AND function_record.prorettype='trigger'::regtype
		AND function_record.prokind='f' AND function_record.provolatile='v'
		AND NOT function_record.prosecdef AND NOT function_record.proisstrict
		AND function_record.proconfig IS NULL AND language.lanname='plpgsql'
		AND btrim(regexp_replace(function_record.prosrc,'[[:space:]]+',' ','g'))=
			btrim(regexp_replace($observation_body$
				BEGIN
					IF TG_OP = 'INSERT' THEN
						IF NEW.observation_write_token IS NULL THEN
							NEW.observation_version := 0;
						END IF;
					ELSIF NEW.observation_write_token IS NULL OR
						NEW.observation_write_token IS NOT DISTINCT FROM OLD.observation_write_token THEN
						NEW.observation_version := 0;
					END IF;
					RETURN NEW;
				END
			$observation_body$,'[[:space:]]+',' ','g'))
	))`
