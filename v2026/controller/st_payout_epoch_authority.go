// A retained payout belongs to its immutable epoch policy, even after the
// operator restarts with only the successor policy configuration.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/urfoundation/sn/v2026/payoutartifact"
	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urfoundation/sn/v2026/stabi"
)

// Policy and the half-open epoch window come from one complete,
// canonical deployment read. Local epoch mirrors cannot supply this authority.
type StPayoutEpochAuthority struct {
	Epoch        uint64
	PolicyHash   [32]byte
	Start        protocol.ClientKeyEffectiveBoundary
	End          protocol.ClientKeyEffectiveBoundary
	StartTime    time.Time
	EndTime      time.Time
	StartHeader  []byte
	EndHeader    []byte
	ClockProfile string
}

// PayoutEpochAuthority restarts the entire read on failover. It authenticates
// the current operator before consuming the coordinator's retained policyAt
// history; a claimed hash from an artifact or an old local file is not a vote.
func (self *CoreStClient) PayoutEpochAuthority(ctx context.Context, epoch uint64) (*StPayoutEpochAuthority, error) {
	if self == nil || self.cfg == nil || self.coordinator == nil || self.cfg.RootKey == nil || self.cfg.Netuid == 0 || self.cfg.Netuid > 65535 {
		return nil, errors.New("st: payout epoch authority configuration is incomplete")
	}
	cfg := self.cfg
	domain := protocol.ClientKeyHistoryDomain{ChainID: cfg.ChainId, GenesisHash: cfg.GenesisHash, Netuid: uint16(cfg.Netuid), Coordinator: cfg.ContractAddress, SettlementVault: cfg.SettlementVault, DeploymentIDHash: sha256.Sum256([]byte(cfg.DeploymentId)), PolicyHash: cfg.PolicyHash, NoID: cfg.NoId}
	if err := domain.Validate(); err != nil {
		return nil, err
	}
	var result *StPayoutEpochAuthority
	err := self.eachRpc(ctx, func(ctx context.Context, client *ethclient.Client) error {
		callCtx, cancel := context.WithTimeout(ctx, stCallTimeout)
		defer cancel()
		boundary, operator, err := readStClientKeyAuthorityAt(callCtx, client, domain, nil)
		if err != nil {
			return fmt.Errorf("st: payout current deployment authority: %w", err)
		}
		if operator.RootSigner != crypto.PubkeyToAddress(cfg.RootKey.PublicKey) || epoch >= boundary.Epoch {
			return errors.New("st: payout operator authority differs or epoch is not closed")
		}
		coordinator := self.coordinator
		e := new(big.Int).SetUint64(epoch)
		views := []struct {
			method string
			data   []byte
			words  int
		}{
			{method: "policyAt", data: coordinator.PackPolicyAt(e), words: 13},
			{method: "epochStartBlock", data: coordinator.PackEpochStartBlock(e), words: 1},
			{method: "epochEndBlock", data: coordinator.PackEpochEndBlock(e), words: 1},
		}
		values := make([]hexutil.Bytes, len(views))
		batch := make([]rpc.BatchElem, len(views))
		for index, view := range views {
			batch[index] = rpc.BatchElem{Method: "eth_call", Args: []any{map[string]any{"to": cfg.ContractAddress, "data": hexutil.Bytes(view.data)}, rpc.BlockNumberOrHashWithHash(common.Hash(boundary.Hash), true)}, Result: &values[index]}
		}
		if err := client.Client().BatchCallContext(callCtx, batch); err != nil {
			return err
		}
		parsed, err := stabi.STCoordinatorMetaData.ParseABI()
		if err != nil {
			return err
		}
		for index, view := range views {
			if batch[index].Error != nil || len(values[index]) != view.words*32 {
				return errors.Join(fmt.Errorf("st: payout epoch %s unavailable or malformed", view.method), batch[index].Error)
			}
			outputs := parsed.Methods[view.method].Outputs
			decoded, err := outputs.Unpack(values[index])
			if err != nil {
				return err
			}
			canonical, err := outputs.Pack(decoded...)
			if err != nil || !bytes.Equal(canonical, values[index]) {
				return errors.Join(errors.New("st: payout epoch authority is not canonical"), err)
			}
		}
		policy, err := coordinator.UnpackPolicyAt(values[0])
		if err != nil {
			return err
		}
		start, end := new(big.Int).SetBytes(values[1]), new(big.Int).SetBytes(values[2])
		if policy.PolicyHash == ([32]byte{}) || policy.EffectiveEpoch > epoch || policy.EpochBlocks == 0 || !start.IsUint64() || !end.IsUint64() || start.Sign() <= 0 || start.Cmp(end) >= 0 || end.Uint64() > boundary.Block {
			return errors.New("st: payout epoch policy or finalized window is invalid")
		}
		expectedStart := new(big.Int).Mul(new(big.Int).SetUint64(epoch-policy.EffectiveEpoch), new(big.Int).SetUint64(policy.EpochBlocks))
		expectedStart.Add(expectedStart, new(big.Int).SetUint64(policy.EffectiveBlock))
		expectedEnd := new(big.Int).Add(expectedStart, new(big.Int).SetUint64(policy.EpochBlocks))
		if start.Cmp(expectedStart) != 0 || end.Cmp(expectedEnd) != 0 {
			return errors.New("st: payout epoch window differs from its retained policy")
		}
		// Raw identities are essential on Subtensor: recomputing an Ethereum
		// header hash would authenticate a different block representation.
		headers := make([]json.RawMessage, 2)
		blocks := []uint64{start.Uint64(), end.Uint64()}
		batch = make([]rpc.BatchElem, 2)
		for index, block := range blocks {
			batch[index] = rpc.BatchElem{Method: "eth_getBlockByNumber", Args: []any{hexutil.EncodeUint64(block), false}, Result: &headers[index]}
		}
		if err := client.Client().BatchCallContext(callCtx, batch); err != nil {
			return err
		}
		boundaries := []protocol.ClientKeyEffectiveBoundary{boundary}
		times := make([]time.Time, 2)
		for index, block := range blocks {
			if batch[index].Error != nil || len(headers[index]) == 0 || len(headers[index]) > 1024*1024 {
				return errors.Join(errors.New("st: payout epoch header unavailable or excessive"), batch[index].Error)
			}
			var header *struct {
				Number    *hexutil.Big    `json:"number"`
				Hash      common.Hash     `json:"hash"`
				Timestamp *hexutil.Uint64 `json:"timestamp"`
			}
			if err := json.Unmarshal(headers[index], &header); err != nil {
				return err
			}
			if header == nil || header.Number == nil || !(*big.Int)(header.Number).IsUint64() || (*big.Int)(header.Number).Uint64() != block || header.Hash == (common.Hash{}) || header.Timestamp == nil || uint64(*header.Timestamp) > math.MaxInt64 {
				return errors.New("st: payout epoch header identity or timestamp is invalid")
			}
			// The end is exclusive and belongs to the next epoch; the
			// closed-epoch check above also proves this addition cannot wrap.
			boundaries = append(boundaries, protocol.ClientKeyEffectiveBoundary{Block: block, Hash: [32]byte(header.Hash), Epoch: epoch + uint64(index)})
			times[index] = time.Unix(int64(*header.Timestamp), 0).UTC()
		}
		if !times[0].Before(times[1]) {
			return errors.New("st: payout epoch timestamps do not advance")
		}
		limits := stabi.ClientKeyAuthorityRpcLimits{MaximumRequests: protocol.MaxClientKeyObservationBatchRpcRequests, MaximumMethods: protocol.MaxClientKeyObservationBatchRpcMethods, MaximumBytes: protocol.MaxClientKeyObservationBatchControlBytes}
		if _, err := stabi.WitnessClientKeyAuthorityBoundariesContext(callCtx, client, domain, boundaries, limits); err != nil {
			return err
		}
		if err := callCtx.Err(); err != nil {
			return err
		}
		result = &StPayoutEpochAuthority{Epoch: epoch, PolicyHash: policy.PolicyHash, Start: boundaries[1], End: boundaries[2], StartTime: times[0], EndTime: times[1]}
		// Preserve only recovered exact committed Frontier RLP15. Missing old
		// projections remain unknown; a returned contradiction is never a clock.
		clockHeaders := make([][]byte, 2)
		for index, boundary := range boundaries[1:] {
			clockHeaders[index], err = payoutartifact.RecoverFrontierWindowHeader(callCtx, headers[index], payoutartifact.Boundary{Number: boundary.Block, Hash: common.Hash(boundary.Hash).Hex()})
			if err != nil {
				if errors.Is(err, payoutartifact.ErrClosedWorkUnavailable) {
					clockHeaders = nil
					break
				}
				return err
			}
		}
		if len(clockHeaders) == 2 {
			result.StartHeader, result.EndHeader = clockHeaders[0], clockHeaders[1]
			result.ClockProfile = payoutartifact.FrontierWindowClockProfile
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
