// Settlement v2 head exclusion at a million providers (VERSION2-FINAL.md
// §6.2, §8.2). v1 reads bindingAt for every provider at both epoch boundaries.
// Here only the client ids whose FleetBound validity covers the epoch are
// read, so the reads are bounded by bindings, not by providers. The st event
// mirror does not decode FleetBound, so a scan with its own checkpoint mirrors
// every FleetBound log from the deploy block into st_event.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/urnetwork/server/model"
)

// The topic of STCoordinator's FleetBound event.
var stFleetBoundTopic = crypto.Keccak256Hash([]byte("FleetBound(bytes16,bytes32,bytes32,uint16,uint64,uint64,uint64)"))

// Reads the coordinator's FleetBound logs in [fromBlock, toBlock]. Removed
// logs are dropped; a log carrying the topic that does not decode fails the
// range. The caller checks each log's block against the canonical chain.
func (self *CoreStClient) FleetBoundEvents(ctx context.Context, fromBlock uint64, toBlock uint64) ([]*model.StChainEvent, error) {
	if self.coordinator == nil {
		return nil, errors.New("FleetBound logs require the release coordinator")
	}
	var logs []types.Log
	err := self.eachRpc(ctx, func(ctx context.Context, client *ethclient.Client) error {
		callCtx, cancel := context.WithTimeout(ctx, stCallTimeout)
		defer cancel()
		out, err := client.FilterLogs(callCtx, ethereum.FilterQuery{
			FromBlock: new(big.Int).SetUint64(fromBlock),
			ToBlock:   new(big.Int).SetUint64(toBlock),
			Addresses: []common.Address{self.cfg.ContractAddress},
			Topics:    [][]common.Hash{{stFleetBoundTopic}},
		})
		if err != nil {
			return err
		}
		logs = out
		return nil
	})
	if err != nil {
		return nil, err
	}
	events := make([]*model.StChainEvent, 0, len(logs))
	for index := range logs {
		log := &logs[index]
		if log.Removed {
			continue
		}
		bound, err := self.coordinator.UnpackFleetBoundEvent(log)
		if err != nil {
			return nil, fmt.Errorf("FleetBound log %d:%d: %w", log.BlockNumber, log.Index, err)
		}
		// a struct of strings and a log always encode
		data, _ := json.Marshal(model.StFleetBoundEventData{
			ClientId: fmt.Sprintf("0x%x", bound.ClientId), FleetId: fmt.Sprintf("0x%x", bound.FleetId), Hotkey: fmt.Sprintf("0x%x", bound.Hotkey),
			Uid: fmt.Sprintf("%d", bound.Uid), Generation: fmt.Sprintf("%d", bound.Generation),
			ValidFromEpoch: fmt.Sprintf("%d", bound.ValidFromEpoch), ValidToEpoch: fmt.Sprintf("%d", bound.ValidToEpoch),
		})
		originalLog, _ := json.Marshal(log)
		events = append(events, &model.StChainEvent{
			BlockNumber: log.BlockNumber, BlockHash: log.BlockHash.Hex(), LogIndex: int(log.Index), TxHash: log.TxHash.Hex(),
			Kind: model.StFleetBoundEventKind, DataJson: string(data), OriginalLog: originalLog,
		})
	}
	return events, nil
}

// Mirrors every FleetBound log from the deploy block through closeBlock into
// st_event, continuing from the scan's checkpoint in bounded ranges. Each
// range's logs must lie in the range and on canonical blocks before the
// mirror or the checkpoint changes. closeBlock must be final.
func stSyncFleetBoundEvents(ctx context.Context, cfg *StConfig, client StClient, closeBlock uint64) error {
	if cfg.DeployBlock == 0 {
		return errors.New("st deploy_block must be nonzero before the FleetBound scan")
	}
	finalized, _, _, err := client.FinalizedHead(ctx)
	if err != nil {
		return fmt.Errorf("st finalized head: %w", err)
	}
	if finalized < closeBlock {
		return fmt.Errorf("FleetBound scan close block %d is ahead of finalized head %d", closeBlock, finalized)
	}
	fromBlock := max(model.GetStFleetBoundNextBlock(ctx, cfg.DeploymentKey()), cfg.DeployBlock)
	for fromBlock <= closeBlock {
		toBlock := closeBlock
		if closeBlock-fromBlock >= stSyncEventsBlockRange {
			toBlock = fromBlock + stSyncEventsBlockRange - 1
		}
		events, err := client.FleetBoundEvents(ctx, fromBlock, toBlock)
		if err != nil {
			return err
		}
		blocks := []uint64{}
		seen := map[uint64]bool{}
		for _, event := range events {
			if event == nil || event.Kind != model.StFleetBoundEventKind || event.BlockNumber < fromBlock || toBlock < event.BlockNumber {
				return fmt.Errorf("FleetBound scan range %d..%d returned an event outside the requested range", fromBlock, toBlock)
			}
			if !seen[event.BlockNumber] {
				seen[event.BlockNumber] = true
				blocks = append(blocks, event.BlockNumber)
			}
		}
		if 0 < len(blocks) {
			hashes, err := client.BlockHashes(ctx, blocks)
			if err != nil {
				return err
			}
			if len(hashes) != len(blocks) {
				return fmt.Errorf("st canonical block batch returned %d hashes, want %d", len(hashes), len(blocks))
			}
			canonical := make(map[uint64]string, len(blocks))
			for index, block := range blocks {
				canonical[block] = common.BytesToHash(hashes[index][:]).Hex()
			}
			for _, event := range events {
				if !strings.EqualFold(event.BlockHash, canonical[event.BlockNumber]) {
					return fmt.Errorf("st FleetBound log block hash mismatch at %d: log %s canonical %s", event.BlockNumber, event.BlockHash, canonical[event.BlockNumber])
				}
			}
			model.UpsertStEvents(ctx, cfg.DeploymentKey(), events)
		}
		model.SetStFleetBoundNextBlock(ctx, cfg.DeploymentKey(), toBlock+1)
		if toBlock == closeBlock {
			break
		}
		fromBlock = toBlock + 1
	}
	return nil
}

// StHeadExcludedClients returns the providers among clientIds that hold an
// active fleet binding at the closed epoch's start or end boundary block
// (VERSION2-FINAL.md §6.2), each with its binding merged across the two
// boundaries as BindingsAt merges it. bindingAt is read only for the client
// ids whose mirrored FleetBound validity covers the epoch: a revocation only
// shortens a validity and a cleanup only ends one, so no other client can be
// bound at either boundary.
func StHeadExcludedClients(ctx context.Context, epoch uint64, startBlock uint64, closeBlock uint64, clientIds [][16]byte) (map[[16]byte]*StFleetBindingState, error) {
	cfg, client, err := stRequire()
	if err != nil {
		return nil, err
	}
	return stHeadExcludedClients(ctx, cfg, client, epoch, startBlock, closeBlock, clientIds)
}

func stHeadExcludedClients(ctx context.Context, cfg *StConfig, client StClient, epoch uint64, startBlock uint64, closeBlock uint64, clientIds [][16]byte) (map[[16]byte]*StFleetBindingState, error) {
	if closeBlock < startBlock {
		return nil, fmt.Errorf("head exclusion block window [%d,%d] is invalid", startBlock, closeBlock)
	}
	if err := stSyncFleetBoundEvents(ctx, cfg, client, closeBlock); err != nil {
		return nil, err
	}
	bound, err := model.GetStFleetBoundClientIds(ctx, cfg.DeploymentKey(), epoch, closeBlock)
	if err != nil {
		return nil, err
	}
	covered := make(map[[16]byte]bool, len(bound))
	for _, clientId := range bound {
		covered[clientId] = true
	}
	candidates := [][16]byte{}
	for _, clientId := range clientIds {
		if covered[clientId] {
			candidates = append(candidates, clientId)
		}
	}
	// an empty candidate set still proves the boundaries final
	bindings, err := client.BindingsAt(ctx, candidates, epoch, startBlock, closeBlock)
	if err != nil {
		return nil, fmt.Errorf("bindingAt batch: %w", err)
	}
	if len(bindings) != len(candidates) {
		return nil, fmt.Errorf("bindingAt batch returned %d rows for %d clients", len(bindings), len(candidates))
	}
	excluded := map[[16]byte]*StFleetBindingState{}
	for index, binding := range bindings {
		if binding == nil {
			return nil, fmt.Errorf("bindingAt batch row %d for %x is nil", index, candidates[index])
		}
		if binding.Active {
			excluded[candidates[index]] = binding
		}
	}
	return excluded, nil
}
