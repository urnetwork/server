package server

import "strings"

// Preserve the prior guard, including its lease, clock, scope and evidence
// predicates. Only the exact epoch-three builder repair may retain its agent.
func competitionEpochThreeExecutorBuilderCorrectionSql() string {
	const start = "CREATE FUNCTION competition_executor_control_amendment_guard()"
	const end = "END $executor_control_amendment$;"
	const old = "   OR host->>'agent_sha256' IS NOT DISTINCT FROM host->>'previous_agent_sha256'"
	const replacement = old + ` AND NOT (
        epoch_round.epoch_number=3 AND NEW.round_id='01a1f4d0-47c9-ac2d-ce45-55ef159a9b41'::uuid
        AND value->>'environment'='main'
        AND host->>'builder_sha256' IS DISTINCT FROM host->>'previous_builder_sha256')`
	_, tail, found := strings.Cut(competitionExecutorControlAmendmentSql, start)
	guard, _, terminated := strings.Cut(tail, end)
	if !found || !terminated || strings.Count(guard, old) != 1 {
		panic("published executor amendment guard changed")
	}
	return "CREATE OR REPLACE FUNCTION competition_executor_control_amendment_guard()" +
		strings.Replace(guard, old, replacement, 1) + end
}
