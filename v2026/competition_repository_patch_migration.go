// Prospectively admits reviewed dependency changes only under the evaluated
// round's explicit mono-root policy. Historical source authority stays narrow.
package server

import "strings"

// Reinstalls the latest ten-repository source guard with one scoped admission
// change; review, threshold, deadline and immutable source evidence stay intact.
func competitionRepositoryPatchMigration() string {
	previous := competitionSourceRetirementMigration()
	start := strings.Index(previous, "CREATE OR REPLACE FUNCTION competition_v2_source_authorized")
	if start < 0 {
		panic("latest source authorization migration is absent")
	}
	source := previous[start:]
	anchor := "OR ((intent->'commits')-'server') IS DISTINCT FROM ((previous->'commits')-'server') THEN"
	if strings.Count(source, anchor) != 1 {
		panic("source repository protection guard is ambiguous")
	}
	replacement := "OR (COALESCE(evaluated_round.policy_json->'patch_policy'->>'path_base','')<>'mono_root_v1' AND ((intent->'commits')-'server') IS DISTINCT FROM ((previous->'commits')-'server')) THEN"
	return "SET LOCAL lock_timeout = '5s';\n" + strings.Replace(source, anchor, replacement, 1)
}
