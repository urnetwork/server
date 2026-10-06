// The staging approval is an append-only, explicitly reviewed exception. Its
// catalog contract proves the durable guards, not the quality of any approval.
package monitor

// Scope the column and relation evidence before testing the published shape;
// missing future tables produce empty evidence rather than a query failure.
const competitionStagingApprovalCatalogQuery = `
	staging_approval_relation_artifact AS (
		SELECT relkind::text AS table_kind
		FROM pg_class
		WHERE oid = to_regclass('public.competition_staging_winner_approval')
	), staging_approval_column_artifact AS (
		SELECT column_name::text, data_type::text, is_nullable::text,
		       column_default::text, character_maximum_length
		FROM information_schema.columns
		WHERE table_schema = 'public'
		  AND table_name = 'competition_staging_winner_approval'
	)
`

// Pin both guard bodies and their unconditional event bindings. Substring
// matches would accept a no-op containing the intended predicate in a comment.
const competitionStagingApprovalArtifactQuery = `(
	COALESCE((SELECT table_kind = 'r' FROM staging_approval_relation_artifact), false)
	AND (SELECT count(*) = 7 FROM staging_approval_column_artifact)
	AND NOT EXISTS (
		SELECT 1 FROM (VALUES
			('round_id', 'uuid', NULL::integer),
			('job_id', 'uuid', NULL::integer),
			('reviewer_id', 'character varying', 128),
			('reason', 'text', NULL::integer),
			('evidence_json', 'json', NULL::integer),
			('evidence_sha256', 'character varying', 64),
			('reviewed_at', 'timestamp without time zone', NULL::integer)
		) AS expected(column_name, data_type, character_maximum_length)
		WHERE NOT EXISTS (
			SELECT 1 FROM staging_approval_column_artifact AS actual
			WHERE actual.column_name = expected.column_name
			  AND actual.data_type = expected.data_type
			  AND actual.character_maximum_length IS NOT DISTINCT FROM expected.character_maximum_length
			  AND actual.is_nullable = 'NO'
			  AND actual.column_default IS NULL
		)
	)
	AND NOT EXISTS (
		SELECT 1 FROM (VALUES
			('p', 'PRIMARY KEY (round_id)'),
			('f', 'FOREIGN KEY (round_id) REFERENCES competition_round(round_id)'),
			('f', 'FOREIGN KEY (job_id) REFERENCES competition_job(job_id)'),
			('c', $reviewer_check$CHECK (((reviewer_id)::text ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'::text))$reviewer_check$),
			('c', 'CHECK (((octet_length(reason) >= 1) AND (octet_length(reason) <= 4096)))'),
			('c', $evidence_check$CHECK ((json_typeof(evidence_json) = 'object'::text))$evidence_check$),
			('c', $hash_check$CHECK (((evidence_sha256)::text ~ '^[0-9a-f]{64}$'::text))$hash_check$)
		) AS expected(constraint_type, definition)
		WHERE NOT EXISTS (
			SELECT 1 FROM constraint_artifact AS actual
			WHERE actual.table_name = 'competition_staging_winner_approval'
			  AND actual.constraint_type = expected.constraint_type
			  AND actual.definition = expected.definition
			  AND actual.validated
		)
	)
	AND NOT EXISTS (
		SELECT 1 FROM (VALUES
			('competition_staging_winner_approval_append_only', 'competition_append_only_guard', 27),
			('competition_staging_winner_approval_insert_guard', 'competition_staging_winner_approval_guard', 7)
		) AS expected(trigger_name, function_name, trigger_type)
		WHERE NOT EXISTS (
			SELECT 1 FROM competition_trigger_artifact AS actual
			JOIN competition_function_artifact AS function_artifact
			  ON function_artifact.function_oid = actual.function_oid
			WHERE actual.table_name = 'competition_staging_winner_approval'
			  AND actual.trigger_name = expected.trigger_name
			  AND actual.trigger_type = expected.trigger_type
			  AND function_artifact.function_name = expected.function_name
			  AND actual.enabled AND actual.unconditional
			  AND actual.update_columns = ARRAY[]::text[]
		)
	)
	AND NOT EXISTS (
		SELECT 1 FROM (VALUES
			('competition_append_only_guard', $approval_append_body$
				BEGIN
					RAISE EXCEPTION 'competition append-only record changed';
				END
			$approval_append_body$),
			('competition_staging_winner_approval_guard', $approval_insert_body$
				BEGIN
					IF NOT EXISTS (
						SELECT 1 FROM competition_round AS round
						JOIN competition_job AS job ON job.job_id = NEW.job_id
						WHERE round.round_id = NEW.round_id AND round.staging = true
						  AND round.canceled = false AND round.finalized_at IS NOT NULL
						  AND round.winner_job_id = NEW.job_id AND job.round_id = NEW.round_id
						  AND job.state = 'succeeded'
					) THEN
						RAISE EXCEPTION 'staging approval requires the finalized winning job';
					END IF;
					RETURN NEW;
				END
			$approval_insert_body$)
		) AS expected(function_name, definition)
		WHERE NOT EXISTS (
			SELECT 1 FROM competition_function_artifact AS actual
			WHERE actual.function_name = expected.function_name
			  AND actual.definition = btrim(regexp_replace(expected.definition, '[[:space:]]+', ' ', 'g'))
		)
	)
)`
