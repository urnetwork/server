// The permissive step must keep every source authority and change only the two
// census counts, each under the sidecar condition.
package server

import (
	"slices"
	"strings"
	"testing"
)

// Each guard is the registered one with one count under the sidecar condition,
// so a deployment cannot drop the elected-winner, exact-source, lease or
// readiness requirements with it, or require sidecar of the running release.
func TestCompetitionSidecarSourceMigrationOnlyWidensTheLatestGuards(t *testing.T) {
	migration := competitionSidecarSourceMigration()
	if migration != competitionSidecarSourceMigration() {
		t.Fatal("the permissive step is nondeterministic")
	}
	functions := sidecarGroupFunctions(migration)
	if len(functions) != 2 {
		t.Fatalf("the permissive step replaces %d functions", len(functions))
	}
	for name, condition := range map[string]string{
		"competition_v2_source_graph_valid": "commits ? 'sidecar'",
		"competition_v2_source_authorized":  "intent->'commits' ? 'sidecar'",
	} {
		registered, ok := sidecarGroupRegisteredFunction(name)
		if !ok || registered == "" {
			t.Fatalf("no registered migration creates %s", name)
		}
		conditions, ok := sidecarGroupWidenedConditions(registered, functions[name])
		if !ok || !slices.Equal(conditions, []string{condition}) {
			t.Errorf("%s differs from the registered guard by more than its count under %q: %q, %t", name, condition, conditions, ok)
		}
	}
	for _, expected := range []string{
		"SET LOCAL lock_timeout = '5s';",
		"CREATE OR REPLACE FUNCTION competition_v2_source_graph_valid",
		"jsonb_object_keys(commits))=(CASE WHEN commits ? 'sidecar' THEN 11 ELSE 10 END)",
		"commits ?& ARRAY['server','connect','sdk','proxy','glog','goidenticons','userwireguard','sn','warp','gvisor']",
		"CREATE OR REPLACE FUNCTION competition_v2_source_authorized",
		"jsonb_object_keys(intent->'git_refs'))<>(CASE WHEN intent->'commits' ? 'sidecar' THEN 12 ELSE 11 END)",
		"intent:=competition_v2_instrument_product_intent(intent,env,competition,season);",
		"PERFORM competition_v2_source_mode(intent)",
		"competition_v2_winner_source_authorized(intent,predecessor_id)",
		"source_lock_sha256", "policy_approval", "reviewed_source_promotion",
		"path_base','')<>'mono_root_v1' AND ((intent->'commits')-'server') IS DISTINCT FROM ((previous->'commits')-'server'))",
	} {
		if !strings.Contains(migration, expected) {
			t.Errorf("the permissive step lost %q", expected)
		}
	}
	for _, forbidden := range []string{
		"jsonb_object_keys(commits))=10", "jsonb_object_keys(commits))=11",
		"jsonb_object_keys(intent->'git_refs'))<>11", "jsonb_object_keys(intent->'git_refs'))<>12",
		"'gvisor','sidecar'", "operator-proxy",
		"CREATE TRIGGER", "ALTER FUNCTION", "UPDATE competition_", "DELETE FROM competition_", "CREATE FUNCTION",
	} {
		if strings.Contains(migration, forbidden) {
			t.Errorf("the permissive step requires sidecar, rewrites evidence or keeps an old count: %q", forbidden)
		}
	}
}
