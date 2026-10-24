package monitor

// v858 retired operator-proxy from production's source requirement: the
// source authorization counts the same ten git refs for production and
// staging. The guard is published by
// server.competitionSourceRetirementMigration, which replaces the staging
// case expression with the single count.
const competitionMigration858ArtifactQuery = `EXISTS (SELECT 1 FROM pg_proc p
		WHERE p.oid = to_regprocedure('public.competition_v2_source_authorized(jsonb,text,text,text)')
		AND position('jsonb_object_keys(intent->''git_refs''))<>11' in p.prosrc) > 0
		AND position('THEN 11 ELSE 12 END' in p.prosrc) = 0)`

// v859 binds a scheduled round's opening to the time its intent announced
// (server.competitionScheduledOpeningMigration), independently of the CLI.
const competitionMigration859ArtifactQuery = `EXISTS (SELECT 1 FROM pg_proc p
		WHERE p.oid = to_regprocedure('public.competition_v2_round_insert_guard()')
		AND position('NEW.opens_at IS DISTINCT FROM (intent->>''opens_at'')::timestamp' in p.prosrc) > 0)`
