// SIGNALS.md §8.9. The builder correction replaces only the executor
// amendment insert guard. Its table and trigger remain under prior custody.
package monitor

import "github.com/urnetwork/server"

const competitionExecutorAmendmentGuardSignature = "competition_executor_control_amendment_guard()"

var competitionEpochThreeExecutorBuilderCorrectionVersion = server.CompetitionEpochThreeExecutorBuilderCorrectionMigrationVersion()

var competitionEpochThreeExecutorBuilderCorrectionMigrationContract = migrationCurrentSuffix(competitionEpochThreeExecutorBuilderCorrectionVersion,
	"epoch-three executor builder amendment guard",
	snMainnetMigrationFunction(competitionExecutorAmendmentGuardSignature, "trigger", "plpgsql", "v", false, server.CompetitionEpochThreeExecutorBuilderCorrectionBodySql()))
