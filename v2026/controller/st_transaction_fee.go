package controller

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

type stTransactionFeePreparation struct {
	estimatedGas uint64
	header       *types.Header
	fee          *big.Int
	legacy       bool
}

// Read the same live values in one bounded HTTP batch, so estimating gas does
// not consume the queue budget of a later fee request. No quote survives this
// preparation or substitutes for the durable signed transaction attempt.
func readStTransactionFeePreparation(ctx context.Context, client *ethclient.Client, from, to common.Address, calldata []byte, estimate, forceLegacy bool) (*stTransactionFeePreparation, error) {
	if ctx == nil || client == nil {
		return nil, errors.New("st: fee preparation owner is unavailable")
	}
	callCtx, cancel := context.WithTimeout(ctx, stCallTimeout)
	defer cancel()
	var header *types.Header
	var quote *hexutil.Big
	var estimated *hexutil.Uint64
	feeMethod := "eth_maxPriorityFeePerGas"
	if forceLegacy {
		feeMethod = "eth_gasPrice"
	}
	batch := []rpc.BatchElem{
		{Method: "eth_getBlockByNumber", Args: []any{"latest", false}, Result: &header},
		{Method: feeMethod, Args: []any{}, Result: &quote},
	}
	if estimate {
		call := map[string]any{"from": from, "to": to}
		if len(calldata) != 0 {
			call["input"] = hexutil.Bytes(calldata)
		}
		batch = append(batch, rpc.BatchElem{Method: "eth_estimateGas", Args: []any{call}, Result: &estimated})
	}
	if err := errors.Join(client.Client().BatchCallContext(callCtx, batch), callCtx.Err()); err != nil {
		return nil, fmt.Errorf("st: fee preparation batch: %w", err)
	}
	if err := batch[0].Error; err != nil {
		return nil, fmt.Errorf("st: latest fee header: %w", err)
	}
	if header == nil || header.Number == nil || header.BaseFee != nil && header.BaseFee.Sign() < 0 {
		return nil, errors.New("st: latest fee header is absent or invalid")
	}
	result := &stTransactionFeePreparation{header: header, legacy: forceLegacy || header.BaseFee == nil}
	if estimate {
		if err := batch[2].Error; err != nil {
			return nil, fmt.Errorf("st: estimate gas: %w", err)
		}
		if estimated == nil || *estimated == 0 {
			return nil, errors.New("st: gas estimate is absent or zero")
		}
		result.estimatedGas = uint64(*estimated)
	}
	if result.legacy && !forceLegacy {
		// A pre-London endpoint may reject the unused tip method. Its actual
		// gas-price response, never that tip or a default, prices this attempt.
		var price *hexutil.Big
		if err := client.Client().CallContext(callCtx, &price, "eth_gasPrice"); err != nil {
			return nil, fmt.Errorf("st: suggest gas price: %w", err)
		}
		quote = price
	} else if err := batch[1].Error; err != nil {
		return nil, fmt.Errorf("st: fee quote %s: %w", feeMethod, err)
	}
	if quote == nil || (*big.Int)(quote).Sign() < 0 || (*big.Int)(quote).BitLen() > 256 {
		return nil, errors.New("st: fee quote is absent or invalid")
	}
	if err := callCtx.Err(); err != nil {
		return nil, err
	}
	result.fee = new(big.Int).Set((*big.Int)(quote))
	return result, nil
}
