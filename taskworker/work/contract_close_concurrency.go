// The background closer's write concurrency can be lowered from config while
// the primary is saturated, without a build or deploy. `contract_close.yml`:
//
//	deadline_close_parallel: 4  # owner batches one deadline page runs at once
//	steal_concurrency: 4        # scheduled-close steals one taskworker runs at once
//	scheduled_close_parallel: 4 # queued closes one taskworker runs at once
//
// The same resource enables the reaper of queued closes that outlived their
// contracts (scheduled_close_reap, see scheduled_contract_close_reap.go).
//
// Each value is read when a page, a steal or a queued close starts, so an
// update applies to the next one. A value can only lower its compiled limit: an
// absent or unreadable resource, a malformed or non-positive value, or one above
// the compiled limit keeps the compiled limit, so a typo never adds load.
//
// Lowering any limit slows the backlog drain in proportion. Nothing is
// skipped: a page still starts every owner before it completes, a steal that is
// not admitted leaves its own queued close to finish with its ordinary result,
// a queued close that finds no slot in time defers as a held owner's does, and
// the queued closes and the expiry sweep still retire every due contract.
package work

import (
	"sync"

	"github.com/urnetwork/server"
)

const contractCloseConcurrencyResourceName = "contract_close.yml"

type contractCloseConcurrencyConfig struct {
	DeadlineCloseParallel *int `yaml:"deadline_close_parallel"`
	StealConcurrency      *int `yaml:"steal_concurrency"`
}

// The configured limits, each within [1, its compiled limit].
func contractCloseConcurrencyLimits() (deadlineCloseParallel int, stealConcurrency int) {
	deadlineCloseParallel = contractDeadlineCloseParallel
	stealConcurrency = scheduledCloseStealConcurrency
	resource, err := server.Config.SimpleResource(contractCloseConcurrencyResourceName)
	if err != nil {
		return
	}
	var config contractCloseConcurrencyConfig
	if err := resource.UnmarshalYamlE(&config); err != nil {
		return
	}
	lower := func(value *int, compiledLimit int) int {
		if value == nil || *value < 1 || compiledLimit < *value {
			return compiledLimit
		}
		return *value
	}
	deadlineCloseParallel = lower(config.DeadlineCloseParallel, contractDeadlineCloseParallel)
	stealConcurrency = lower(config.StealConcurrency, scheduledCloseStealConcurrency)
	return
}

// Runs closeOwner for each owner, at most parallel at once, starting owners
// while more reports true. Complete when every owner started. Returns after
// every started owner finished.
func runContractOwnerBatches[T any](owners []T, parallel int, more func() bool, closeOwner func(T)) (complete bool) {
	var waitGroup sync.WaitGroup
	slots := make(chan struct{}, max(1, parallel))
	complete = true
	for _, owner := range owners {
		if !more() {
			complete = false
			break
		}
		slots <- struct{}{}
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			defer func() { <-slots }()
			closeOwner(owner)
		}()
	}
	waitGroup.Wait()
	return
}

// Admits one steal into slots, a channel whose capacity is the compiled limit,
// when at most limit steals including this one hold a slot. Each admitted steal
// counts itself before it compares, so steals that race past one another can
// only refuse each other, never exceed the limit.
func admitScheduledCloseSteal(slots chan struct{}, limit int) (release func(), ok bool) {
	select {
	case slots <- struct{}{}:
	default:
		return nil, false
	}
	if limit < len(slots) {
		<-slots
		return nil, false
	}
	return func() { <-slots }, true
}
