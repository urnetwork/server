// Historical bridge controls retain the retired bounded source implementation
// only in the test binary. Production constructors cannot configure or reach it.
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
	readContract func(context.Context, server.Id, server.Id) (model.ContractHoleStatus, time.Time, error)
}

// Constructs one fixed budget; default settings never retain shared state.
func newResidentContractFallback() *residentContractFallback {
	return &residentContractFallback{
		slots:        make(chan struct{}, residentContractFallbackMaxConcurrent),
		readContract: model.ReadResumableContractLease,
	}
}

// A validated negative is authoritative. Errors and absent/expired evidence are
// unknown, never positive; during the bridge only, they may try the same source
// predicate used to build the projection. No source boolean warms Redis.
func readResidentContractAllowanceWithFallbackForTest(
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
	status, validUntil, err := self.readContract(readCtx, source, destination)
	if readCtx.Err() != nil || err != nil {
		defaultResidentContractAllowanceMetrics.add(contractAllowanceFallbackError)
		return residentContractAllowance{}
	}
	if status == model.ContractHolePositive && time.Now().Before(validUntil) {
		defaultResidentContractAllowanceMetrics.add(contractAllowanceFallbackPositive)
		return residentContractAllowance{active: true, validUntil: validUntil}
	} else if status == model.ContractHoleNegative {
		defaultResidentContractAllowanceMetrics.add(contractAllowanceFallbackNegative)
	} else {
		defaultResidentContractAllowanceMetrics.add(contractAllowanceFallbackError)
	}
	return residentContractAllowance{}
}

// Historical controls may reconstruct the retired bridge explicitly. Ordinary
// resident construction uses the Redis-only manager and has no source context.
func newResidentContractManagerWithFallback(
	ctx context.Context,
	cancel context.CancelFunc,
	clientId server.Id,
	settings *ExchangeSettings,
	fallback *residentContractFallback,
) *residentContractManager {
	manager := newResidentContractManager(ctx, cancel, clientId, settings)
	if fallback == nil {
		return manager
	}
	manager.readContract = func(packetCtx context.Context, source, destination server.Id) residentContractAllowance {
		return readResidentContractAllowanceWithFallbackForTest(packetCtx, ctx, source, destination, fallback)
	}
	return manager
}
