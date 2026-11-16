package controller

// The chain reads of the Top 200 claim on the server: the coordinator's mirror of a
// hotkey's commitment, and the burn, fee and balance a top-up is sized from. Each read is
// of one exact storage value or runtime call, on the endpoints the st subsystem reads
// from. A read that fails is an error, and the caller does nothing.

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/ethclient"
)

const (
	// SubtensorModule's and register_limit's indices on runtime 486. A runtime that moved
	// them fails the shape check below, or quotes another call at about the same fee; the
	// top-up's ceiling on the burn holds either way.
	snFleetRegisterPalletIndex = 7
	snFleetRegisterCallIndex   = 134
	// about what a signed register_limit extrinsic weighs
	snFleetRegisterExtrinsicBytes = 154
	// no quote above this is believed: a registration's fee on mainnet is about 1,066,000
	snFleetRegisterMaxFeeRao = 10_000_000
)

// the System.Account layout on Subtensor: nonce, consumers, providers and sufficients as
// u32, then free as u64
const snFleetAccountFreeOffset = 16

func (self *CoreStClient) snFleetReadStorage(ctx context.Context, key []byte) (value []byte, err error) {
	err = self.eachRpc(ctx, func(ctx context.Context, client *ethclient.Client) error {
		callCtx, cancel := context.WithTimeout(ctx, stCallTimeout)
		defer cancel()
		var result *string
		if err := client.Client().CallContext(callCtx, &result, "state_getStorage", "0x"+hex.EncodeToString(key)); err != nil {
			return err
		}
		value = nil
		if result == nil {
			return nil
		}
		decoded, err := hex.DecodeString(strings.TrimPrefix(*result, "0x"))
		if err != nil {
			return err
		}
		value = decoded
		return nil
	})
	return value, err
}

// MirroredCommitment reads mirroredCommitments(hotkey) at the coordinator.
func (self *CoreStClient) MirroredCommitment(ctx context.Context, hotkey [32]byte) (hash [32]byte, finalizedBlock uint64, err error) {
	if self.coordinator == nil {
		return hash, 0, errors.New("fleet commitments require the release coordinator")
	}
	err = self.eachRpc(ctx, func(ctx context.Context, client *ethclient.Client) error {
		callCtx, cancel := context.WithTimeout(ctx, stCallTimeout)
		defer cancel()
		to := self.cfg.ContractAddress
		raw, err := client.CallContract(callCtx, ethereum.CallMsg{To: &to, Data: self.coordinator.PackMirroredCommitments(hotkey)}, nil)
		if err != nil {
			return err
		}
		output, err := self.coordinator.UnpackMirroredCommitments(raw)
		if err != nil {
			return err
		}
		hash, finalizedBlock = output.CommitmentHash, output.FinalizedBlock
		return nil
	})
	return hash, finalizedBlock, err
}

// RegistrationBurnRao reads SubtensorModule.Burn(netuid).
func (self *CoreStClient) RegistrationBurnRao(ctx context.Context, netuid uint16) (uint64, error) {
	key := append(twox128([]byte("SubtensorModule")), twox128([]byte("Burn"))...)
	key = binary.LittleEndian.AppendUint16(key, netuid)
	value, err := self.snFleetReadStorage(ctx, key)
	if err != nil {
		return 0, err
	}
	if len(value) != 8 {
		return 0, fmt.Errorf("Burn is %d bytes", len(value))
	}
	return binary.LittleEndian.Uint64(value), nil
}

// FreeBalanceRao reads an account's free TAO. An account that doesn't exist holds none.
func (self *CoreStClient) FreeBalanceRao(ctx context.Context, account [32]byte) (uint64, error) {
	value, err := self.snFleetReadStorage(ctx, snSubstrateAccountKey(account))
	if err != nil {
		return 0, err
	}
	if value == nil {
		return 0, nil
	}
	if len(value) < snFleetAccountFreeOffset+8 {
		return 0, fmt.Errorf("System.Account is %d bytes", len(value))
	}
	return binary.LittleEndian.Uint64(value[snFleetAccountFreeOffset:]), nil
}

// snFleetParseFeeQuote reads a RuntimeDispatchInfo: compact ref_time and proof_size, the
// class, then Subtensor's u64 fee. Only the fee's position matters here, and it's the end.
func snFleetParseFeeQuote(reply []byte) (uint64, error) {
	if len(reply) < 1+1+1+8 {
		return 0, fmt.Errorf("the fee quote is %d bytes", len(reply))
	}
	fee := binary.LittleEndian.Uint64(reply[len(reply)-8:])
	if snFleetRegisterMaxFeeRao < fee {
		return 0, fmt.Errorf("the fee quote of %d rao is over the limit", fee)
	}
	return fee, nil
}

// RegistrationFeeRao asks the runtime what a register_limit call costs, with no signature.
// The hotkey and limit don't change it.
func (self *CoreStClient) RegistrationFeeRao(ctx context.Context, netuid uint16) (fee uint64, err error) {
	args := []byte{snFleetRegisterPalletIndex, snFleetRegisterCallIndex}
	args = binary.LittleEndian.AppendUint16(args, netuid)
	args = append(args, make([]byte, 32)...)
	args = binary.LittleEndian.AppendUint64(args, 1)
	args = binary.LittleEndian.AppendUint32(args, snFleetRegisterExtrinsicBytes)
	err = self.eachRpc(ctx, func(ctx context.Context, client *ethclient.Client) error {
		callCtx, cancel := context.WithTimeout(ctx, stCallTimeout)
		defer cancel()
		var result string
		if err := client.Client().CallContext(callCtx, &result, "state_call", "TransactionPaymentCallApi_query_call_info", "0x"+hex.EncodeToString(args)); err != nil {
			return err
		}
		reply, err := hex.DecodeString(strings.TrimPrefix(result, "0x"))
		if err != nil {
			return err
		}
		fee, err = snFleetParseFeeQuote(reply)
		return err
	})
	return fee, err
}
