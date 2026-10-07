// Temporary compatibility for open contracts created before all writers publish
// Redis holes. Only unresolved Redis evidence can enter this bounded source
// check. Checkpoints are resumable, so removal requires cohort coverage, not age.
package connect

import (
	"context"
	"errors"
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
	readContract func(context.Context, server.Id, server.Id) (model.ContractHoleStatus, time.Time, error)
	metrics      *residentContractFallbackMetrics
}

// A successful read carries its strict source deadline to every cached offer.
// No boolean-only conversion may manufacture or extend authority.
type residentContractAllowance struct {
	active     bool
	validUntil time.Time
}

// Constructs one fixed budget; default settings never retain shared state.
func newResidentContractFallback() *residentContractFallback {
	return &residentContractFallback{
		slots:        make(chan struct{}, residentContractFallbackMaxConcurrent),
		readContract: model.ReadResumableContractLease,
		metrics:      defaultResidentContractFallbackMetrics,
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
) residentContractAllowance {
	if packetCtx.Err() != nil {
		defaultResidentContractAllowanceMetrics.add(contractAllowanceCanceled)
		return residentContractAllowance{}
	}
	status, validUntil, err := model.ReadContractHoleLease(packetCtx, source, destination)
	if err == nil {
		switch status {
		case model.ContractHolePositive:
			if !time.Now().Before(validUntil) {
				defaultResidentContractAllowanceMetrics.add(contractAllowanceRedisError)
				break
			}
			defaultResidentContractAllowanceMetrics.add(contractAllowanceRedisPositive)
			return residentContractAllowance{active: true, validUntil: validUntil}
		case model.ContractHoleNegative:
			defaultResidentContractAllowanceMetrics.add(contractAllowanceRedisNegative)
			return residentContractAllowance{}
		default:
			defaultResidentContractAllowanceMetrics.add(contractAllowanceRedisUnknown)
		}
	} else {
		defaultResidentContractAllowanceMetrics.add(contractAllowanceRedisError)
	}
	if packetCtx.Err() != nil || sourceCtx.Err() != nil {
		defaultResidentContractAllowanceMetrics.add(contractAllowanceCanceled)
		return residentContractAllowance{}
	}
	if fallback == nil {
		defaultResidentContractAllowanceMetrics.add(contractAllowanceFallbackDisabled)
		return residentContractAllowance{}
	}
	return fallback.check(sourceCtx, source, destination)
}

// Try once without queuing. The original lifetime context is used only for this
// specific source operation; inherited PostgreSQL guards are never removed.
// Deadline, error, saturation, and cancellation all refuse the packet.
func (self *residentContractFallback) check(ctx context.Context, source, destination server.Id) residentContractAllowance {
	if ctx.Err() != nil {
		defaultResidentContractAllowanceMetrics.add(contractAllowanceCanceled)
		return residentContractAllowance{}
	}
	select {
	case self.slots <- struct{}{}:
		defer func() { <-self.slots }()
	default:
		defaultResidentContractAllowanceMetrics.add(contractAllowanceFallbackSaturated)
		return residentContractAllowance{}
	}
	readCtx, cancel := context.WithTimeout(ctx, residentContractFallbackTimeout)
	defer cancel()
	if readCtx.Err() != nil {
		defaultResidentContractAllowanceMetrics.add(contractAllowanceCanceled)
		return residentContractAllowance{}
	}
	defaultResidentContractAllowanceMetrics.add(contractAllowanceFallbackStarted)
	started := time.Now()
	defer func() { self.metrics.sourceSeconds.Observe(time.Since(started).Seconds()) }()
	status, validUntil, err := self.readContract(readCtx, source, destination)
	if contextErr := readCtx.Err(); contextErr != nil {
		err = contextErr
	}
	if err != nil {
		defaultResidentContractAllowanceMetrics.add(contractAllowanceFallbackError)
		reason := contractFallbackPhysicalError
		if errors.Is(err, context.DeadlineExceeded) {
			reason = contractFallbackDeadline
		} else if errors.Is(err, context.Canceled) {
			reason = contractFallbackCanceled
		}
		self.metrics.add(reason)
		return residentContractAllowance{}
	}
	if status == model.ContractHolePositive && time.Now().Before(validUntil) {
		defaultResidentContractAllowanceMetrics.add(contractAllowanceFallbackPositive)
		return residentContractAllowance{active: true, validUntil: validUntil}
	} else if status == model.ContractHoleNegative {
		defaultResidentContractAllowanceMetrics.add(contractAllowanceFallbackNegative)
	} else {
		defaultResidentContractAllowanceMetrics.add(contractAllowanceFallbackError)
		reason := contractFallbackUnknown
		if status == model.ContractHolePositive && !validUntil.IsZero() {
			reason = contractFallbackExpiredLease
		}
		self.metrics.add(reason)
	}
	return residentContractAllowance{}
}
