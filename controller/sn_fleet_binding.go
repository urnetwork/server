// The live fleet binding sync for the SN top board (LEADERTOP.md §5.7). Until
// now FleetBound reached st_event only while a settlement v2 epoch was built
// (st_head_exclusion.go), and revocations and cleanups were never mirrored.
// This sync reads the coordinator's FleetBound, FleetBindingRevoked and
// FleetBindingCleaned logs continuously, from the deploy block up to the
// finalized head, in bounded ranges. A range is applied only when every log
// lies in it on a canonical block, and its rows, the binding versions they
// change and the checkpoint commit together (model/sn_fleet_binding.go). The
// checkpoint keeps the canonical hash of its last block, and a later run that
// finds a different hash there halts rather than building on another fork.
//
// SnClaimedFleets joins the census to clients and networks through
// network_client and applies bindingAt's live UID test with the newest
// metagraph census of the incentive reader (sn_incentive.go): the fleets
// whose members the validator counts at a settlement epoch.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The topics of STCoordinator's revocation and cleanup events.
var (
	stFleetBindingRevokedTopic = crypto.Keccak256Hash([]byte("FleetBindingRevoked(bytes16,uint64,uint64)"))
	stFleetBindingCleanedTopic = crypto.Keccak256Hash([]byte("FleetBindingCleaned(bytes16,uint64)"))
)

// The most log ranges one sync run reads: 20 ranges of the shared
// one-thousand-block log window are about 2.8 days of blocks, so a first
// sync from the deploy block catches up within a few runs.
const snFleetBindingSyncMaxRanges = 20

// FleetBindingEvents reads the coordinator's FleetBound, FleetBindingRevoked
// and FleetBindingCleaned logs in [fromBlock, toBlock] as mirror rows, in log
// order. Removed logs are dropped; a log carrying one of the topics that does
// not decode fails the range. The caller checks each log's block against the
// canonical chain.
func (self *CoreStClient) FleetBindingEvents(ctx context.Context, fromBlock uint64, toBlock uint64) ([]*model.StChainEvent, error) {
	if self.coordinator == nil {
		return nil, errors.New("fleet binding logs require the release coordinator")
	}
	var logs []types.Log
	err := self.eachRpc(ctx, func(ctx context.Context, client *ethclient.Client) error {
		callCtx, cancel := context.WithTimeout(ctx, stCallTimeout)
		defer cancel()
		out, err := client.FilterLogs(callCtx, ethereum.FilterQuery{
			FromBlock: new(big.Int).SetUint64(fromBlock),
			ToBlock:   new(big.Int).SetUint64(toBlock),
			Addresses: []common.Address{self.cfg.ContractAddress},
			Topics:    [][]common.Hash{{stFleetBoundTopic, stFleetBindingRevokedTopic, stFleetBindingCleanedTopic}},
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
		if len(log.Topics) == 0 {
			return nil, fmt.Errorf("binding log %d:%d has no topic", log.BlockNumber, log.Index)
		}
		switch log.Topics[0] {
		case stFleetBoundTopic:
			bound, err := self.coordinator.UnpackFleetBoundEvent(log)
			if err != nil {
				return nil, fmt.Errorf("FleetBound log %d:%d: %w", log.BlockNumber, log.Index, err)
			}
			events = append(events, stFleetBoundChainEvent(log, bound))
		case stFleetBindingRevokedTopic:
			revoked, err := self.coordinator.UnpackFleetBindingRevokedEvent(log)
			if err != nil {
				return nil, fmt.Errorf("FleetBindingRevoked log %d:%d: %w", log.BlockNumber, log.Index, err)
			}
			events = append(events, snFleetBindingChainEvent(log, model.SnFleetBindingRevokedEventKind, model.SnFleetBindingRevokedEventData{
				ClientId: fmt.Sprintf("0x%x", revoked.ClientId), Generation: fmt.Sprintf("%d", revoked.Generation), EffectiveEpoch: fmt.Sprintf("%d", revoked.EffectiveEpoch),
			}))
		case stFleetBindingCleanedTopic:
			cleaned, err := self.coordinator.UnpackFleetBindingCleanedEvent(log)
			if err != nil {
				return nil, fmt.Errorf("FleetBindingCleaned log %d:%d: %w", log.BlockNumber, log.Index, err)
			}
			events = append(events, snFleetBindingChainEvent(log, model.SnFleetBindingCleanedEventKind, model.SnFleetBindingCleanedEventData{
				ClientId: fmt.Sprintf("0x%x", cleaned.ClientId), CleanedAtEpoch: fmt.Sprintf("%d", cleaned.CleanedAtEpoch),
			}))
		default:
			return nil, fmt.Errorf("binding log %d:%d has an unrequested topic %s", log.BlockNumber, log.Index, log.Topics[0].Hex())
		}
	}
	return events, nil
}

// The st_event row of one revocation or cleanup log.
func snFleetBindingChainEvent(log *types.Log, kind string, data any) *model.StChainEvent {
	// a struct of strings and a log always encode
	dataJson, _ := json.Marshal(data)
	originalLog, _ := json.Marshal(log)
	return &model.StChainEvent{
		BlockNumber: log.BlockNumber, BlockHash: log.BlockHash.Hex(), LogIndex: int(log.Index), TxHash: log.TxHash.Hex(),
		Kind: kind, DataJson: string(dataJson), OriginalLog: originalLog,
	}
}

// The chain reads of the binding sync. CoreStClient implements it; tests
// supply their own.
type snFleetBindingChain interface {
	FinalizedHead(ctx context.Context) (uint64, [32]byte, time.Time, error)
	BlockHashes(ctx context.Context, blocks []uint64) ([][32]byte, error)
	FleetBindingEvents(ctx context.Context, fromBlock uint64, toBlock uint64) ([]*model.StChainEvent, error)
}

// The outcome of one sync run.
type SnFleetBindingSyncResult struct {
	// logs applied this run
	Applied int
	// every binding log before this block is applied
	NextBlock uint64
	// the finalized head the run read up to
	FinalizedBlock uint64
}

// SnSyncFleetBindings advances the live binding census toward the finalized
// head. A run reads at most snFleetBindingSyncMaxRanges log ranges; the
// taskworker runs it about every minute.
func SnSyncFleetBindings(ctx context.Context) (*SnFleetBindingSyncResult, error) {
	cfg, client, err := stRequire()
	if err != nil {
		return nil, err
	}
	chain, ok := client.(snFleetBindingChain)
	if !ok {
		return nil, errors.New("the st client cannot read fleet binding logs")
	}
	result, err := snSyncFleetBindings(ctx, cfg, chain, snFleetBindingSyncMaxRanges)
	observeSnFleetBindingSync(result, err)
	return result, err
}

// The reasons a binding sync run fails, as metric labels.
const (
	snFleetBindingSyncErrorConfig    = "config"
	snFleetBindingSyncErrorChain     = "chain"
	snFleetBindingSyncErrorCanonical = "canonical"
	snFleetBindingSyncErrorCensus    = "census"
)

var snFleetBindingSyncErrorReasons = []string{snFleetBindingSyncErrorConfig, snFleetBindingSyncErrorChain, snFleetBindingSyncErrorCanonical, snFleetBindingSyncErrorCensus}

type snFleetBindingSyncError struct {
	reason string
	err    error
}

func (self *snFleetBindingSyncError) Error() string { return self.err.Error() }
func (self *snFleetBindingSyncError) Unwrap() error { return self.err }

func snFleetBindingSyncFail(reason string, format string, args ...any) error {
	return &snFleetBindingSyncError{reason: reason, err: fmt.Errorf(format, args...)}
}

func snSyncFleetBindings(ctx context.Context, cfg *StConfig, chain snFleetBindingChain, maxRanges int) (*SnFleetBindingSyncResult, error) {
	deploymentKey := cfg.DeploymentKey()
	if deploymentKey == "" || cfg.DeployBlock == 0 {
		return nil, snFleetBindingSyncFail(snFleetBindingSyncErrorConfig, "st deployment and deploy_block must be set before the binding sync")
	}
	finalized, _, _, err := chain.FinalizedHead(ctx)
	if err != nil {
		return nil, snFleetBindingSyncFail(snFleetBindingSyncErrorChain, "st finalized head: %w", err)
	}
	checkpoint := model.GetSnFleetBindingCheckpoint(ctx, deploymentKey)
	result := &SnFleetBindingSyncResult{NextBlock: checkpoint.NextBlock, FinalizedBlock: finalized}
	fromBlock := checkpoint.NextBlock
	if fromBlock == 0 {
		fromBlock = cfg.DeployBlock
		result.NextBlock = fromBlock
	} else {
		// the last applied block must still be canonical
		hashes, err := chain.BlockHashes(ctx, []uint64{fromBlock - 1})
		if err != nil {
			return result, snFleetBindingSyncFail(snFleetBindingSyncErrorChain, "binding checkpoint block %d: %w", fromBlock-1, err)
		}
		if len(hashes) != 1 || !strings.EqualFold(checkpoint.BlockHash, common.BytesToHash(hashes[0][:]).Hex()) {
			return result, snFleetBindingSyncFail(snFleetBindingSyncErrorCanonical, "binding checkpoint block %d: stored %s is not canonical; the sync halts for reconciliation", fromBlock-1, checkpoint.BlockHash)
		}
	}
	for range maxRanges {
		if finalized < fromBlock {
			break
		}
		toBlock, err := stBoundedInclusiveRangeEnd(fromBlock, finalized, stSyncEventsBlockRange)
		if err != nil {
			return result, snFleetBindingSyncFail(snFleetBindingSyncErrorConfig, "%w", err)
		}
		if toBlock == math.MaxUint64 || math.MaxInt64 <= toBlock {
			return result, snFleetBindingSyncFail(snFleetBindingSyncErrorChain, "binding range end %d is out of range", toBlock)
		}
		events, err := chain.FleetBindingEvents(ctx, fromBlock, toBlock)
		if err != nil {
			return result, snFleetBindingSyncFail(snFleetBindingSyncErrorChain, "binding logs %d..%d: %w", fromBlock, toBlock, err)
		}
		blocks := []uint64{}
		seen := map[uint64]bool{}
		for _, event := range events {
			if event == nil || event.BlockNumber < fromBlock || toBlock < event.BlockNumber {
				return result, snFleetBindingSyncFail(snFleetBindingSyncErrorChain, "binding range %d..%d returned a log outside the range", fromBlock, toBlock)
			}
			switch event.Kind {
			case model.StFleetBoundEventKind, model.SnFleetBindingRevokedEventKind, model.SnFleetBindingCleanedEventKind:
			default:
				return result, snFleetBindingSyncFail(snFleetBindingSyncErrorChain, "binding range %d..%d returned a %q log", fromBlock, toBlock, event.Kind)
			}
			if !seen[event.BlockNumber] {
				seen[event.BlockNumber] = true
				blocks = append(blocks, event.BlockNumber)
			}
		}
		if !seen[toBlock] {
			blocks = append(blocks, toBlock)
		}
		hashes, err := chain.BlockHashes(ctx, blocks)
		if err != nil {
			return result, snFleetBindingSyncFail(snFleetBindingSyncErrorChain, "binding range %d..%d canonical blocks: %w", fromBlock, toBlock, err)
		}
		if len(hashes) != len(blocks) {
			return result, snFleetBindingSyncFail(snFleetBindingSyncErrorChain, "st canonical block batch returned %d hashes, want %d", len(hashes), len(blocks))
		}
		canonical := make(map[uint64]string, len(blocks))
		for index, block := range blocks {
			canonical[block] = common.BytesToHash(hashes[index][:]).Hex()
		}
		for _, event := range events {
			if !strings.EqualFold(event.BlockHash, canonical[event.BlockNumber]) {
				return result, snFleetBindingSyncFail(snFleetBindingSyncErrorCanonical, "binding log %d:%d block hash %s is not canonical %s", event.BlockNumber, event.LogIndex, event.BlockHash, canonical[event.BlockNumber])
			}
		}
		next := model.SnFleetBindingCheckpoint{NextBlock: toBlock + 1, BlockHash: canonical[toBlock]}
		if err := model.ApplySnFleetBindingRange(ctx, deploymentKey, events, next); err != nil {
			reason := snFleetBindingSyncErrorChain
			if errors.Is(err, model.ErrSnFleetBindingCensus) {
				reason = snFleetBindingSyncErrorCensus
			}
			return result, snFleetBindingSyncFail(reason, "binding range %d..%d: %w", fromBlock, toBlock, err)
		}
		for _, event := range events {
			snFleetBindingSyncEvents.WithLabelValues(event.Kind).Inc()
		}
		result.Applied += len(events)
		result.NextBlock = next.NextBlock
		fromBlock = next.NextBlock
	}
	return result, nil
}

// SnClaimedFleets returns every fleet with a binding active at the
// settlement epoch, joined to clients and networks through network_client,
// with bindingAt's live UID test applied from the newest metagraph census.
// Fleets whose UID test passes are Live; the board treats only those as
// claimed. The census is complete through SyncedNextBlock-1.
func SnClaimedFleets(ctx context.Context, epoch uint64) (*model.SnClaimedFleets, error) {
	cfg := stConfig()
	if cfg == nil || cfg.DeploymentKey() == "" {
		return nil, errStNotConfigured
	}
	return snClaimedFleets(ctx, cfg.DeploymentKey(), epoch), nil
}

// SnClaimedFleetsNow is SnClaimedFleets at the current settlement epoch of
// the st sync task's mirror; ok is false while that epoch is unknown.
func SnClaimedFleetsNow(ctx context.Context) (claimed *model.SnClaimedFleets, ok bool, err error) {
	cfg := stConfig()
	if cfg == nil || cfg.DeploymentKey() == "" {
		return nil, false, errStNotConfigured
	}
	summary := model.GetStEpochSummaryCache(ctx, cfg.DeploymentKey())
	if summary == nil {
		return nil, false, nil
	}
	return snClaimedFleets(ctx, cfg.DeploymentKey(), summary.Epoch), true, nil
}

func snClaimedFleets(ctx context.Context, deploymentKey model.StDeploymentKey, epoch uint64) *model.SnClaimedFleets {
	versions := model.GetSnFleetBindingVersions(ctx, deploymentKey)
	active := model.SnFleetBindingsAt(versions, epoch)
	networkIds := model.GetNetworkIdsForClientIds(ctx, model.SnFleetBindingClientIds(active))
	var holders *model.SnUidHolders
	if snapshot := model.GetLatestSnIncentiveSnapshot(ctx, deploymentKey); snapshot != nil {
		holders = snapshot.Holders()
	}
	claimed := model.SnClaimedFleetsAt(versions, epoch, holders, networkIds)
	claimed.SyncedNextBlock = model.GetSnFleetBindingCheckpoint(ctx, deploymentKey).NextBlock
	return claimed
}

// Refreshes what the claimed fleets at the current settlement epoch drive,
// after each sync run: the dashboard counts, and the verify cohort's priority
// set of live claimed-fleet members, at most a per-fleet share of each
// (LEADERTOP.md design question 6).
func refreshSnClaimedFleetsNow(ctx context.Context) {
	server.HandleError(func() {
		claimed, ok, err := SnClaimedFleetsNow(ctx)
		if err != nil || !ok {
			return
		}
		counts := map[string]int{model.SnUidHeldYes: 0, model.SnUidHeldNo: 0, model.SnUidHeldUnknown: 0}
		members := 0
		for _, fleet := range claimed.Fleets {
			counts[fleet.UidHeld] += 1
			if fleet.Live() {
				members += len(fleet.Members)
			}
		}
		for status, count := range counts {
			snClaimedFleetsGauge.WithLabelValues(status).Set(float64(count))
		}
		snClaimedFleetMembersGauge.Set(float64(members))

		settings := VerifySettings()
		if settings.CohortSize <= 0 || settings.CohortDeploymentKey == "" {
			return
		}
		fleets := [][]server.Id{}
		for _, fleet := range claimed.LiveFleets() {
			clientIds := make([]server.Id, 0, len(fleet.Members))
			for _, member := range fleet.Members {
				clientIds = append(clientIds, member.ClientId)
			}
			fleets = append(fleets, clientIds)
		}
		priority := model.SetVerifyCohortPriority(ctx, settings.CohortDeploymentKey, fleets, model.VerifyCohortPriorityPerFleet(settings))
		snVerifyCohortPriorityMembers.Set(float64(priority))
	}, func(err error) {
		glog.Infof("[sn]claimed fleet refresh failed: %s\n", err)
	})
}

// RefreshSnClaimedFleetsNow is refreshSnClaimedFleetsNow for the taskworker.
func RefreshSnClaimedFleetsNow(ctx context.Context) {
	refreshSnClaimedFleetsNow(ctx)
}
