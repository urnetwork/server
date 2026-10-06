package main

import (
	"context"
	"errors"
	"io"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

const privateLegacySettlementDrainInputKind = "customer-contract-legacy-settlement-v1"
const privateLegacySettlementDrainOutputKind = "customer-contract-legacy-settlement-private-v1"

func invokeLegacySettlementDrain(ctx context.Context, input privateContractExpiryRepairInput, apply bool) (any, error) {
	payer, err := server.ParseId(input.ExpectedPayerNetworkId)
	if err != nil {
		return nil, errors.New("private_legacy_input_refused")
	}
	ids := make([]server.Id, 0, len(input.ContractIds))
	for _, raw := range input.ContractIds {
		id, err := server.ParseId(raw)
		if err != nil {
			return nil, errors.New("private_legacy_input_refused")
		}
		ids = append(ids, id)
	}
	return model.DrainLegacySettlements(ctx, model.LegacySettlementDrainRequest{ExpectedPayerNetworkId: payer, ContractIds: ids, Apply: apply})
}

func runPrivateLegacySettlementDrain(parent context.Context, reader io.Reader, writer io.Writer, apply bool,
	invoke func(context.Context, privateContractExpiryRepairInput, bool) (any, error),
) int {
	return runPrivateContractRepairKind(parent, reader, writer, apply, privateLegacySettlementDrainInputKind, privateLegacySettlementDrainOutputKind, invoke)
}
