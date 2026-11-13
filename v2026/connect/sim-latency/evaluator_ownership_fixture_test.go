// Shared inert scope for helper-boundary tests, independent of daemon fixtures.
package main

import "strings"

// All scope identifiers are visibly synthetic and independent of live runs.
func subevaluationOwnershipEnvironment() []string {
	return []string{
		"URNETWORK_SUBEVALUATION_RUN_ID=" + strings.Repeat("a", 64),
		"URNETWORK_SUBEVALUATION_JOB_ID=11111111-2222-4333-8444-555555555555",
		"URNETWORK_SUBEVALUATION_ROUND_ID=66666666-7777-4888-8999-000000000000",
		"URNETWORK_SUBEVALUATION_ATTEMPT=2",
	}
}
