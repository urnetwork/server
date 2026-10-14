// The expiry sweep's concurrent closes can be lowered from config while the
// primary is saturated, without a build or deploy. `contract_close.yml`:
//
//	expiry_parallel: 24            # closes one sweep page runs at once
//	expiry_owner_batch_parallel: 2 # owner batches one sweep page runs at once
//
// Each value is read when a sweep run starts, so an update applies to the next
// run. Like the other closer limits in the same resource, each can only lower
// its compiled limit: an absent or unreadable resource, a malformed value, or
// one above the compiled limit keeps the compiled limit, so a typo never adds
// load. Each key is decoded alone, so a typo in one never resets the other.
// Lowering either slows the drain; nothing is skipped, since every selected
// row of a page still starts before the page completes.
//
// expiry_parallel keeps its compiled limit for zero or less too. An owner batch
// closes one balance's due rows in one owned turn on the direct maintenance
// pool, and expiry_owner_batch_parallel can also be 0, which turns owner
// batches off: every row is then visited alone, as before owner batches. Only
// a negative value keeps its compiled limit.
package work

import (
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

type closeExpiredContractsConcurrencyConfig struct {
	ExpiryParallel *int `yaml:"expiry_parallel"`
}

type closeExpiredContractsOwnerBatchConfig struct {
	ExpiryOwnerBatchParallel *int `yaml:"expiry_owner_batch_parallel"`
}

// The configured limit, within [1, closeExpiredContractsParallel].
func closeExpiredContractsParallelLimit() int {
	resource, err := server.Config.SimpleResource(contractCloseConcurrencyResourceName)
	if err != nil {
		return closeExpiredContractsParallel
	}
	var config closeExpiredContractsConcurrencyConfig
	if err := resource.UnmarshalYamlE(&config); err != nil {
		return closeExpiredContractsParallel
	}
	if config.ExpiryParallel == nil || *config.ExpiryParallel < 1 || closeExpiredContractsParallel < *config.ExpiryParallel {
		return closeExpiredContractsParallel
	}
	return *config.ExpiryParallel
}

// The configured owner batches one page runs at once, within
// [0, model.ForceCloseOwnerBatchParallel]; 0 turns them off.
func closeExpiredContractsOwnerBatchLimit() int {
	resource, err := server.Config.SimpleResource(contractCloseConcurrencyResourceName)
	if err != nil {
		return model.ForceCloseOwnerBatchParallel
	}
	var config closeExpiredContractsOwnerBatchConfig
	if err := resource.UnmarshalYamlE(&config); err != nil {
		return model.ForceCloseOwnerBatchParallel
	}
	if config.ExpiryOwnerBatchParallel == nil || *config.ExpiryOwnerBatchParallel < 0 ||
		model.ForceCloseOwnerBatchParallel < *config.ExpiryOwnerBatchParallel {
		return model.ForceCloseOwnerBatchParallel
	}
	return *config.ExpiryOwnerBatchParallel
}
