package controller

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

var errStDepositBelowRuntimeMinimum = errors.New("exact deposit is below the native transfer minimum")

// Reject a known non-executable reserve move before staging or send reserves a
// durable transaction nonce. The usage-based deposit amount is never rounded
// up, and this read-only check does not modify an existing failed intent.
func (self *CoreStClient) preflightDepositRuntimeMinimum(ctx context.Context, amount *big.Int) error {
	if self == nil || self.cfg == nil {
		return errors.New("st: deposit runtime minimum configuration is unavailable")
	}
	if self.coordinator == nil || self.cfg.SettlementVault == (common.Address{}) {
		return nil
	}
	if self.vault == nil || self.cfg.Netuid == 0 || self.cfg.Netuid > 65535 || amount == nil || amount.Sign() <= 0 {
		return errors.New("st: deposit runtime minimum identity or amount is invalid")
	}
	head, err := self.finalizedBlock(ctx)
	if err != nil {
		return fmt.Errorf("st: deposit runtime minimum finalized head: %w", err)
	}
	minimumTao, err := stViewAtBlock(self, ctx, self.cfg.SettlementVault, head.Number,
		self.vault.PackMinimumTransferTaoRao(), self.vault.UnpackMinimumTransferTaoRao)
	if err != nil {
		return fmt.Errorf("st: deposit native transfer minimum: %w", err)
	}
	if minimumTao == 0 {
		return errors.New("st: deposit native transfer minimum is zero")
	}
	calldata := make([]byte, 36)
	copy(calldata[:4], crypto.Keccak256([]byte("getAlphaPrice(uint16)"))[:4])
	new(big.Int).SetUint64(self.cfg.Netuid).FillBytes(calldata[4:])
	price, err := stViewAtBlock(self, ctx, common.HexToAddress("0x0000000000000000000000000000000000000808"), head.Number,
		calldata, func(raw []byte) (*big.Int, error) {
			if len(raw) != 32 {
				return nil, fmt.Errorf("getAlphaPrice returned %d bytes", len(raw))
			}
			return new(big.Int).SetBytes(raw), nil
		})
	if err != nil {
		return fmt.Errorf("st: deposit canonical alpha price: %w", err)
	}
	if price == nil || price.Sign() <= 0 {
		return errors.New("st: deposit canonical alpha price is zero")
	}
	allowance, err := stViewAtBlock(self, ctx, self.cfg.ContractAddress, head.Number,
		self.coordinator.PackRESERVEROUNDINGALLOWANCERAO(), self.coordinator.UnpackRESERVEROUNDINGALLOWANCERAO)
	if err != nil {
		return fmt.Errorf("st: deposit reserve rounding allowance: %w", err)
	}
	if allowance == nil || allowance.Sign() < 0 {
		return errors.New("st: deposit reserve rounding allowance is invalid")
	}

	// Match STSettlementVault's conversion: IAlpha's price uses 18-decimal
	// EVM units, while the transfer minimum and deposit use native rao.
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	moveAmount := new(big.Int).Add(new(big.Int).Set(amount), allowance)
	taoEquivalent := new(big.Int).Quo(new(big.Int).Mul(moveAmount, price), scale)
	minimum := new(big.Int).SetUint64(minimumTao)
	if taoEquivalent.Cmp(minimum) >= 0 {
		return nil
	}
	minimumMove := new(big.Int).Mul(minimum, scale)
	minimumMove.Add(minimumMove, new(big.Int).Sub(new(big.Int).Set(price), big.NewInt(1)))
	minimumMove.Quo(minimumMove, price)
	return fmt.Errorf("%w: usage-based amount %s alpha rao requires reserve move %s alpha rao, worth %s tao rao at finalized block %d/0x%x (alpha price %s); minimum %d tao rao requires at least %s alpha rao in that move; deposit amount unchanged, no new transaction nonce reserved",
		errStDepositBelowRuntimeMinimum, amount, moveAmount, taoEquivalent, head.Number, head.Hash, price, minimumTao, minimumMove)
}
