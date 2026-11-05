// Read-only custody exports identify only this additive candidate migration.
// Integration owns registration, retirement boundaries and live application.
package server

import "strings"

// Exact signatures and argument names accompany bodies so catalog checks pin
// executable identity, volatility, privileges and enabled trigger wiring.
type CompetitionIntakeFirstFunction struct {
	Signature string
	Result    string
	Arguments []string
	Body      string
}

// A caller receives owned metadata; historical functions are not redefined by
// the monitor. Their intact bodies are embedded in the three scoped successors.
func CompetitionIntakeFirstFunctionsSql() []CompetitionIntakeFirstFunction {
	sql := CompetitionIntakeFirstMigrationSql()
	functions := []struct {
		signature string
		result    string
		arguments []string
		marker    string
	}{
		{signature: "competition_v2_intake_policy()", result: "jsonb", marker: "intake_policy"},
		{signature: "competition_v2_intake_handoff_valid(jsonb,jsonb,boolean)", result: "boolean", arguments: []string{"intent", "handoff", "positive"}, marker: "intake_handoff_valid"},
		{signature: "competition_v2_intake_require_handoff(jsonb)", result: "void", arguments: []string{"intent"}, marker: "intake_require_handoff"},
		{signature: "competition_v2_intake_handoff_guard()", result: "trigger", marker: "intake_handoff_guard"},
		{signature: "competition_v2_intake_grant_guard()", result: "trigger", marker: "intake_grant_guard"},
		{signature: "competition_v2_intake_pointer_guard()", result: "trigger", marker: "intake_pointer_guard"},
		{signature: "competition_v2_intake_round_authorized(jsonb)", result: "void", arguments: []string{"value"}, marker: "intake_round_authorized"},
		{signature: "competition_v2_intake_runtime_ready(uuid)", result: "boolean", arguments: []string{"target"}, marker: "intake_runtime_ready"},
		{signature: "competition_v2_intake_baseline_valid(uuid,bytea,text,text,text,text,text,text,text)", result: "boolean", arguments: []string{"target", "content", "digest", "base", "providers", "image", "scorer", "hardware", "qualification"}, marker: "intake_baseline_valid"},
		{signature: "competition_v2_intake_grading_ready(uuid)", result: "boolean", arguments: []string{"target"}, marker: "intake_grading_ready"},
		{signature: "competition_v2_intake_execution_guard()", result: "trigger", marker: "intake_execution_guard"},
		{signature: "competition_v2_intake_ledger_fence_guard()", result: "trigger", marker: "intake_ledger_fence"},
		{signature: "competition_v2_round_insert_guard()", result: "trigger", marker: "intake_round_wrapper"},
		{signature: "competition_v2_round_reserved_identity_guard()", result: "trigger", marker: "intake_reserved_wrapper"},
		{signature: "competition_round_baseline_insert_guard()", result: "trigger", marker: "intake_baseline_wrapper"},
	}
	result := make([]CompetitionIntakeFirstFunction, 0, len(functions))
	for _, function := range functions {
		parts := strings.Split(sql, "$"+function.marker+"$")
		if len(parts) != 3 {
			panic("intake-first migration function delimiter is absent or ambiguous")
		}
		result = append(result, CompetitionIntakeFirstFunction{Signature: function.signature, Result: function.result, Arguments: function.arguments, Body: parts[1]})
	}
	return result
}
