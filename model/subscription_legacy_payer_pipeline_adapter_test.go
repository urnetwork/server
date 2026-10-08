// Register the exact production payer target; no fixture function replaces it.
package model

import "github.com/urnetwork/server/task"

func legacyPayerPipelineAdditionalTargets() []task.Target {
	return []task.Target{NewLegacyPayerSettlementTaskTarget()}
}
