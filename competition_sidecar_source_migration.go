// Admits the sidecar repository into prospective source graphs, beside the ten
// that stay valid. Historical migrations and immutable activation/evaluation
// rows stay unchanged.
package server

import "strings"

// The permissive step of sidecar's entry into the competition source graph
// (connect/CODESTYLE.md, "Database migrations and deploy order"): a graph of
// exactly the ten live repositories still passes, as the running release
// sends it, and so does a graph of those ten with sidecar. It reinstalls the
// latest complete source guards and puts only their two census counts under a
// condition, so each guard answers as before for every graph without a
// sidecar key. Requiring eleven is a later cleanup migration, once every
// competition job runs a release that sends eleven.
func competitionSidecarSourceMigration() string {
	latest := competitionTrustedInstrumentMigration()
	start := strings.Index(latest, "CREATE OR REPLACE FUNCTION competition_v2_source_authorized")
	if start < 0 {
		panic("latest source authorization migration is absent")
	}
	closing := "END $source$;\n"
	end := strings.Index(latest[start:], closing)
	if end < 0 {
		panic("latest source authorization body is unterminated")
	}
	source := latest[start : start+end+len(closing)]
	anchor := "(SELECT count(*) FROM jsonb_object_keys(intent->'git_refs'))<>11"
	if strings.Count(source, anchor) != 1 {
		panic("latest source reference count is ambiguous")
	}
	graph := competitionSourceRetirementGraph
	graphAnchor := "(SELECT count(*) FROM jsonb_object_keys(commits))=10"
	if strings.Count(graph, graphAnchor) != 1 {
		panic("latest source graph count is ambiguous")
	}
	return strings.Replace(graph, graphAnchor,
		"(SELECT count(*) FROM jsonb_object_keys(commits))="+competitionSidecarSourceGraphCount, 1) +
		strings.Replace(source, anchor,
			"(SELECT count(*) FROM jsonb_object_keys(intent->'git_refs'))<>"+competitionSidecarSourceReferenceCount, 1)
}

// The repository count of a graph: the ten, or eleven when sidecar is one. The
// guard still requires the ten by name, so an eleventh key of any other name
// leaves the count at ten and the graph refused.
const competitionSidecarSourceGraphCount = `(CASE WHEN commits ? 'sidecar' THEN 11 ELSE 10 END)`

// The git ref count of an intent: one per repository of its graph, and config.
const competitionSidecarSourceReferenceCount = `(CASE WHEN intent->'commits' ? 'sidecar' THEN 12 ELSE 11 END)`

// The version of the permissive step, found by its SQL. Zero while MG0, the
// group that carries it, is not linked (db_migrations_sidecar.go).
func CompetitionSidecarSourceMigrationVersion() int {
	return sqlMigrationVersion(competitionSidecarSourceMigration())
}
