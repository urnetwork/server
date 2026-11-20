// Retires the deleted repository from all prospective source transitions.
// Historical migrations and immutable activation/evaluation rows stay unchanged.
package server

import "strings"

// Reinstalls the latest complete source guard, retaining elected-winner checks
// while replacing only the obsolete production reference count.
func competitionSourceRetirementMigration() string {
	latest := competitionWinnerReviewMigration()
	start := strings.Index(latest, "CREATE OR REPLACE FUNCTION competition_v2_source_authorized")
	anchor := "(CASE WHEN COALESCE((intent->>'staging')::boolean,false) THEN 11 ELSE 12 END)"
	if start < 0 {
		panic("latest source authorization migration is absent")
	}
	end := strings.Index(latest[start:], "CREATE OR REPLACE FUNCTION competition_v2_round_insert_guard")
	if end < 0 {
		panic("latest round authorization migration is absent")
	}
	source := latest[start : start+end]
	if strings.Count(source, anchor) != 1 {
		panic("latest source reference count is ambiguous")
	}
	return competitionSourceRetirementGraph + strings.Replace(source, anchor, "11", 1)
}

const competitionSourceRetirementGraph = `
SET LOCAL lock_timeout = '5s';
CREATE OR REPLACE FUNCTION competition_v2_source_graph_valid(commits jsonb, staging_mode boolean)
RETURNS boolean LANGUAGE plpgsql IMMUTABLE AS $graph$
BEGIN
 IF jsonb_typeof(commits) IS DISTINCT FROM 'object' OR staging_mode IS NULL THEN RETURN false; END IF;
 RETURN (SELECT count(*) FROM jsonb_object_keys(commits))=10
  AND commits ?& ARRAY['server','connect','sdk','proxy','glog','goidenticons','userwireguard','sn','warp','gvisor']
  AND NOT EXISTS(SELECT 1 FROM jsonb_each_text(commits) c WHERE c.value IS NULL OR c.value !~ '^[0-9a-f]{40}$');
END $graph$;
`
