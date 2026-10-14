// The expiry sweep's concurrent closes can be lowered from config while the
// primary is saturated, without a build or deploy. `contract_close.yml`:
//
//	expiry_parallel: 24 # closes one sweep page runs at once
//
// The value is read when a sweep run starts, so an update applies to the next
// run. Like the other closer limits in the same resource, it can only lower the
// compiled limit: an absent or unreadable resource, a malformed or non-positive
// value, or one above the compiled limit keeps the compiled limit, so a typo
// never adds load. Lowering it slows the drain; nothing is skipped, since every
// selected row of a page still starts before the page completes.
package work

import (
	"github.com/urnetwork/server"
)

type closeExpiredContractsConcurrencyConfig struct {
	ExpiryParallel *int `yaml:"expiry_parallel"`
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
