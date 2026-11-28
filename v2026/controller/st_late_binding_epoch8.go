package controller

// The epoch 8 late-binding window (sn protocol/fleet_late_binding_epoch8.go)
// on the coordinator's bindingAt reads: a binding signed from epoch 9 that
// landed in blocks 9,600,028 through 9,615,060 also counts for epoch 8. The
// release client applies it to every bindingAt it reads, through sn's
// stabi.Epoch8BindingsAt, so head exclusion, the extender and capture the flag
// censuses, the capture the flag draw and GET /sn/head answer as the SN25
// validator does.

import (
	"context"

	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/urfoundation/sn/v2026/stabi"
)

func stBindingAtOutput(binding *StFleetBindingState) stabi.BindingAtOutput {
	return stabi.BindingAtOutput{Active: binding.Active, Record: stabi.STCoordinatorBindingRecord{
		FleetId: binding.FleetId, Hotkey: binding.Hotkey, ClientKey: binding.ClientKey, CommitmentHash: binding.CommitmentHash,
		Generation: binding.Generation, ValidFromEpoch: binding.ValidFrom, ValidToEpoch: binding.ValidTo,
		CleanedAtEpoch: binding.CleanedAtEpoch, Uid: binding.Uid, Cleaned: binding.Cleaned,
	}}
}

func stFleetBindingStateOf(out stabi.BindingAtOutput) *StFleetBindingState {
	return &StFleetBindingState{
		Active: out.Active, FleetId: out.Record.FleetId, Hotkey: out.Record.Hotkey,
		ClientKey: out.Record.ClientKey, CommitmentHash: out.Record.CommitmentHash,
		Generation: out.Record.Generation, ValidFrom: out.Record.ValidFromEpoch,
		ValidTo: out.Record.ValidToEpoch, CleanedAtEpoch: out.Record.CleanedAtEpoch,
		Uid: out.Record.Uid, Cleaned: out.Record.Cleaned,
	}
}

// stEpoch8BindingsAt applies the window to bindings, bindingAt(clientIds[i],
// epoch) read at block through client. The window's own reads go through the
// same client: bindingAt(client, 9) at block and at the window's two fixed
// blocks. Outside the window it returns bindings unchanged and reads nothing.
func (self *CoreStClient) stEpoch8BindingsAt(ctx context.Context, client *ethclient.Client, block uint64, clientIds [][16]byte, epoch uint64, bindings []*StFleetBindingState) ([]*StFleetBindingState, error) {
	answers := make([]stabi.BindingAtOutput, len(bindings))
	for index, binding := range bindings {
		if binding == nil {
			return bindings, nil
		}
		answers[index] = stBindingAtOutput(binding)
	}
	read := func(ctx context.Context, at uint64, ids [][16]byte, epoch uint64) ([]stabi.BindingAtOutput, error) {
		states, err := stReadBindingsAt(ctx, client, self.coordinator, self.cfg.ContractAddress, at, ids, epoch)
		if err != nil {
			return nil, err
		}
		out := make([]stabi.BindingAtOutput, len(states))
		for index, state := range states {
			out[index] = stBindingAtOutput(state)
		}
		return out, nil
	}
	windowed, err := stabi.Epoch8BindingsAt(ctx, self.cfg.ChainId, self.cfg.ContractAddress, epoch, block, clientIds, answers, read)
	if err != nil {
		return nil, err
	}
	out := make([]*StFleetBindingState, len(windowed))
	for index := range windowed {
		if windowed[index] == answers[index] {
			out[index] = bindings[index]
		} else {
			out[index] = stFleetBindingStateOf(windowed[index])
		}
	}
	return out, nil
}
