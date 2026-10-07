// Packet allowance reads only the bounded Redis lease. Unknown or failed
// evidence refuses; durable setup and repair have separate asynchronous owners.
package connect

import (
	"context"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A successful read carries its strict source deadline to every cached offer.
// No boolean-only conversion may manufacture or extend authority.
type residentContractAllowance struct {
	active     bool
	validUntil time.Time
}

// The strict lease deadline applies before every live-forward offer. Missing,
// expired, malformed and unavailable state never obtains a source capability.
func readResidentContractAllowance(
	packetCtx context.Context,
	source, destination server.Id,
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
	if packetCtx.Err() != nil {
		defaultResidentContractAllowanceMetrics.add(contractAllowanceCanceled)
	} else {
		// Retain the predeclared rollout counter for compatible observation. No
		// setting or source context can re-enable the retired packet fallback.
		defaultResidentContractAllowanceMetrics.add(contractAllowanceFallbackDisabled)
	}
	return residentContractAllowance{}
}
