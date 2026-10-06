// The Redis expiry command shares the bounded private command boundary and
// delegates every lifecycle and financial decision to its scoped model owner.
package main

import (
	"context"
	"errors"
	"io"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const privateRedisContractExpiryRepairInputKind = "customer-contract-redis-expiry-repair-v1"
const privateRedisContractExpiryRepairOutputKind = "customer-contract-redis-expiry-repair-private-v1"

// Only this command's kind is admitted; the shared parser bounds exact scope.
func parsePrivateRedisContractExpiryRepair(reader io.Reader) (privateContractExpiryRepairInput, error) {
	return parsePrivateContractRepairKind(reader, privateRedisContractExpiryRepairInputKind)
}

// Only the expected payer, exact contracts and explicit apply flag reach the
// model. Usage and amounts are never supplied by this adapter.
func invokeRedisContractExpiryRepair(ctx context.Context, input privateContractExpiryRepairInput, apply bool) (any, error) {
	payerNetworkId, err := server.ParseId(input.ExpectedPayerNetworkId)
	if err != nil {
		return nil, errors.New("private_redis_expiry_input_refused")
	}
	contractIds := make([]server.Id, 0, len(input.ContractIds))
	for _, rawContractId := range input.ContractIds {
		contractId, err := server.ParseId(rawContractId)
		if err != nil {
			return nil, errors.New("private_redis_expiry_input_refused")
		}
		contractIds = append(contractIds, contractId)
	}
	return model.RepairRedisContractExpiry(ctx, model.ContractExpiryRepairRequest{
		ExpectedPayerNetworkId: payerNetworkId,
		ContractIds:            contractIds,
		Apply:                  apply,
	})
}

// The shared runner supplies the deadline, output cap and finite private errors.
func runPrivateRedisContractExpiryRepair(parent context.Context, reader io.Reader, writer io.Writer, apply bool,
	invoke func(context.Context, privateContractExpiryRepairInput, bool) (any, error),
) int {
	return runPrivateContractRepairKind(parent, reader, writer, apply,
		privateRedisContractExpiryRepairInputKind, privateRedisContractExpiryRepairOutputKind, invoke)
}
