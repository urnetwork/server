// Temporary compatibility for open contracts created before all writers publish
// Redis holes. Only unresolved Redis evidence can enter this bounded source
// check. Checkpoints are resumable, so removal requires cohort coverage, not age.
package connect

import (
	"context"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const (
	residentContractFallbackMaxConcurrent = 8
	residentContractFallbackTimeout       = 250 * time.Millisecond
)

// The exchange explicitly shares this budget with its resident descendants.
// Independent exchanges do not share mutable admission, and no waiting work is
// stored here. Source calls remain joined by each resident's read ownership.
type residentContractFallback struct {
	slots        chan struct{}
	readContract func(context.Context, server.Id, server.Id) (bool, error)
}

// Constructs one fixed budget; default settings never retain shared state.
func newResidentContractFallback() *residentContractFallback {
	return &residentContractFallback{
		slots:        make(chan struct{}, residentContractFallbackMaxConcurrent),
		readContract: model.HasResumableContractForPair,
	}
}

// A validated negative is authoritative. Errors and absent/expired evidence are
// unknown, never positive; during the bridge only, they may try the same source
// predicate used to build the projection. No source boolean warms Redis.
func readResidentContractAllowance(
	packetCtx context.Context,
	sourceCtx context.Context,
	source, destination server.Id,
	fallback *residentContractFallback,
) bool {
	if packetCtx.Err() != nil {
		defaultResidentContractAllowanceMetrics.add(contractAllowanceCanceled)
		return false
	}
	status, err := model.ReadContractHole(packetCtx, source, destination)
	if err == nil {
		switch status {
		case model.ContractHolePositive:
			defaultResidentContractAllowanceMetrics.add(contractAllowanceRedisPositive)
			return true
		case model.ContractHoleNegative:
			defaultResidentContractAllowanceMetrics.add(contractAllowanceRedisNegative)
			return false
		default:
			defaultResidentContractAllowanceMetrics.add(contractAllowanceRedisUnknown)
		}
	} else {
		defaultResidentContractAllowanceMetrics.add(contractAllowanceRedisError)
	}
	if packetCtx.Err() != nil || sourceCtx.Err() != nil {
		defaultResidentContractAllowanceMetrics.add(contractAllowanceCanceled)
		return false
	}
	if fallback == nil {
		defaultResidentContractAllowanceMetrics.add(contractAllowanceFallbackDisabled)
		return false
	}
	return fallback.check(sourceCtx, source, destination)
}

// Try once without queuing. The original lifetime context is used only for this
// specific source operation; inherited PostgreSQL guards are never removed.
// Deadline, error, saturation, and cancellation all refuse the packet.
func (self *residentContractFallback) check(ctx context.Context, source, destination server.Id) bool {
	if ctx.Err() != nil {
		defaultResidentContractAllowanceMetrics.add(contractAllowanceCanceled)
		return false
	}
	select {
	case self.slots <- struct{}{}:
		defer func() { <-self.slots }()
	default:
		defaultResidentContractAllowanceMetrics.add(contractAllowanceFallbackSaturated)
		return false
	}
	readCtx, cancel := context.WithTimeout(ctx, residentContractFallbackTimeout)
	defer cancel()
	if readCtx.Err() != nil {
		defaultResidentContractAllowanceMetrics.add(contractAllowanceCanceled)
		return false
	}
	defaultResidentContractAllowanceMetrics.add(contractAllowanceFallbackStarted)
	active, err := self.readContract(readCtx, source, destination)
	if readCtx.Err() != nil || err != nil {
		defaultResidentContractAllowanceMetrics.add(contractAllowanceFallbackError)
		return false
	}
	if active {
		defaultResidentContractAllowanceMetrics.add(contractAllowanceFallbackPositive)
	} else {
		defaultResidentContractAllowanceMetrics.add(contractAllowanceFallbackNegative)
	}
	return active
}
