package controller

// st_ctf_mirror.go — the carry contract's returns (sn CAPTURETHEFLAG.md §4.6)
// in the event mirror. While capture the flag runs, the chain sync also reads
// the configured carry contract's logs, and each Recycled(epoch, amount) is
// kept in ctf_recycle, whose sum per epoch is recycledFor[epoch], R_k's record.
// Every recycle counts, whoever sent it; a re-scanned range writes nothing new.

import (
	"context"
	"encoding/json"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"

	"github.com/urfoundation/sn/stabi"

	"github.com/urnetwork/server/model"
)

const stCtfEventRecycled = "CtfRecycled"

// stCtfCarryMirrored is the carry contract whose logs the chain sync reads,
// or zero while capture the flag is off or names none.
func stCtfCarryMirrored(cfg *StConfig) common.Address {
	if cfg == nil || !cfg.LedgerV2Enabled() || !cfg.Ctf.On() {
		return common.Address{}
	}
	return cfg.Ctf.CarryAddress
}

// stCtfEventDecoders returns the decoders for a log of the carry contract,
// and false for any other address.
func (self *CoreStClient) stCtfEventDecoders(address common.Address) ([]stEventDecoder, bool) {
	carry := stCtfCarryMirrored(self.cfg)
	if carry == (common.Address{}) || address != carry {
		return nil, false
	}
	return []stEventDecoder{
		stEvent(stCtfEventRecycled, stCtfCarryAbi.UnpackRecycledEvent, func(e *stabi.STCtfCarryRecycled) map[string]any {
			return map[string]any{"e": e.Epoch.String(), "amount": e.Amount.String()}
		}),
	}, true
}

// stApplyCtfEvents mirrors the carry contract's Recycled logs.
func stApplyCtfEvents(ctx context.Context, cfg *StConfig, events []*model.StChainEvent) {
	if stCtfCarryMirrored(cfg) == (common.Address{}) {
		return
	}
	recycles := []*model.CtfRecycle{}
	for _, event := range events {
		if event.Kind != stCtfEventRecycled {
			continue
		}
		var data stLedgerV2EventData
		if json.Unmarshal([]byte(event.DataJson), &data) != nil {
			continue
		}
		epoch, err := stParseUint(data.E)
		amount, amountErr := uint256.FromDecimal(data.Amount)
		if err != nil || amountErr != nil {
			continue
		}
		recycles = append(recycles, &model.CtfRecycle{BlockNumber: event.BlockNumber, LogIndex: event.LogIndex, Epoch: epoch, Amount: *amount, TxHash: event.TxHash})
	}
	if len(recycles) != 0 {
		model.AddCtfRecycles(ctx, cfg.DeploymentKey(), cfg.NoId, recycles)
	}
}
