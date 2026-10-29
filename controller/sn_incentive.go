// The on-chain incentive reader for the SN top board (LEADERTOP.md §6.2,
// design question 10). Once per native tempo, after SubnetEpochIndex has
// advanced at a finalized block, it reads the subnet's incentive census at the
// finalized head of the st archive endpoint (st.yml) with the subnet
// monitor's reader discipline (stmetagraph): Incentive, LastUpdate,
// ValidatorPermit and ValidatorTrust for every UID, Keys and
// BlockAtRegistration for every permitted validator's and every bound
// fleet's UID, and the applied Weights rows of the permitted validators.
//
// The archive is not loaded beyond that: the reader skips without any RPC
// until a tempo has nearly passed since its last census, then checks only the
// finalized head and SubnetEpochIndex (about a kilobyte) until the index
// advances, and reads one census of at most snIncentiveCensusBudget response
// bytes. A failed census waits snIncentiveRetryDelay before the next attempt.
// The runtime metadata is fetched once per runtime version per process.
//
// A census is marked stale when the incentive does not follow the reference
// validator's head ranking: its applied row is the stopgap fallback row, its
// LastUpdate is outside the subnet's activity cutoff, or its ValidatorTrust is
// well below 1. Incentive lags the validator's commit by one to two tempos.
package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/stmetagraph"
)

const (
	// the response bytes one census may read, metadata excluded
	snIncentiveCensusBudget = 100_000
	// the finalized head and SubnetEpochIndex check
	snIncentiveIndexBudget = 8_000
	// the most permitted validators whose rows are read
	snIncentiveMaxValidatorRows = 64
	// a failed census is not attempted again sooner
	snIncentiveRetryDelay = 10 * time.Minute
	// SN25's tempo, used until a census records the subnet's own
	snIncentiveDefaultTempoBlocks = 360
	// the index is checked once this fraction of a tempo has passed since the
	// last census
	snIncentiveTempoFraction = 0.9
	// snapshots are kept for the board's 7 days
	snIncentiveRetention = 7 * 24 * time.Hour
	// UR's validator is UID 1 on SN25 (LEADERTOP.md §3.6): the reference
	// validator when st.yml lists no validator hotkeys
	snIncentiveDefaultReferenceUid = 1
	// ValidatorTrust is a PerU16; below 0.9 consensus no longer follows the
	// reference validator's row
	snIncentiveLowValidatorTrust = 58_982
)

// The stopgap fallback row: pool UID 169 at 1/10 and reserve UIDs 170 and
// 250 at 9/20 each, max-normalized as the chain stores it. It ranks no head
// fleet, so incentive read under it is stale.
var snIncentiveFallbackRow = []model.SnWeightEntry{{Uid: 169, Value: 14563}, {Uid: 170, Value: 65535}, {Uid: 250, Value: 65535}}

// The native reads of the reader. CoreStClient implements it; tests supply a
// recorded chain.
type snNativeSource interface {
	// Runs op against one endpoint at a time until one completes.
	nativeRead(ctx context.Context, op func(context.Context, stmetagraph.Rpc) error) error
}

// Runs a native read once at each configured endpoint in order, moving on only
// after a transport failure: a census is never retried at the same endpoint,
// and a refused census is not read again from another.
func (self *CoreStClient) nativeRead(ctx context.Context, op func(context.Context, stmetagraph.Rpc) error) error {
	if self == nil || self.cfg == nil || len(self.cfg.RpcUrls) == 0 {
		return errors.New("st: native read endpoint is absent")
	}
	failures := []error{}
	for _, url := range self.cfg.RpcUrls {
		attempt, cancel := context.WithTimeout(ctx, stReadAttemptBudget)
		client, err := self.client(attempt, url)
		if err == nil {
			err = op(attempt, client.Client())
		}
		cancel()
		if err == nil {
			return nil
		}
		failures = append(failures, fmt.Errorf("%s: %w", url, err))
		if stmetagraph.ErrorReason(err) != stmetagraph.ReasonRpc {
			break
		}
	}
	return errors.Join(failures...)
}

// The reader's process state: its verified runtimes and when it last tried.
type snIncentiveReaderState struct {
	stateLock    sync.Mutex
	reader       *stmetagraph.Reader
	lastFailedAt time.Time
}

var snIncentiveReader = &snIncentiveReaderState{reader: stmetagraph.NewReader()}

// The outcome of one reader run.
const (
	// a new tempo's census was stored
	SnIncentiveReadStored = "stored"
	// the index has not advanced since the newest census
	SnIncentiveReadUnchanged = "unchanged"
	// no RPC: the tempo has not nearly passed, or a failed census is waiting
	SnIncentiveReadSkipped = "skipped"
)

var snIncentiveReadResults = []string{SnIncentiveReadStored, SnIncentiveReadUnchanged, SnIncentiveReadSkipped}

type SnIncentiveReadResult struct {
	Result           string
	SubnetEpochIndex uint64
	Snapshot         *model.SnIncentiveSnapshot
}

// SnReadIncentive runs the reader once; the taskworker runs it about every
// two minutes.
func SnReadIncentive(ctx context.Context) (*SnIncentiveReadResult, error) {
	cfg, client, err := stRequire()
	if err != nil {
		return nil, err
	}
	source, ok := client.(snNativeSource)
	if !ok {
		return nil, errors.New("the st client cannot read native storage")
	}
	return snReadIncentive(ctx, cfg, source, snIncentiveReader, server.NowUtc())
}

func snReadIncentive(ctx context.Context, cfg *StConfig, source snNativeSource, state *snIncentiveReaderState, now time.Time) (*SnIncentiveReadResult, error) {
	deploymentKey := cfg.DeploymentKey()
	if deploymentKey == "" || cfg.Netuid == 0 || cfg.Netuid > uint64(^uint16(0)) || cfg.GenesisHash == ([32]byte{}) {
		return nil, errors.New("st deployment, netuid and genesis_hash must be set before the incentive reader")
	}
	netuid := uint16(cfg.Netuid)
	storedIndex, storedAt, storedTempo, stored := model.GetLatestSnIncentiveRead(ctx, deploymentKey)
	if stored {
		snIncentiveAgeSeconds.Set(now.Sub(storedAt).Seconds())
	}

	state.stateLock.Lock()
	defer state.stateLock.Unlock()
	if !state.lastFailedAt.IsZero() && now.Sub(state.lastFailedAt) < snIncentiveRetryDelay {
		snIncentiveReads.WithLabelValues(SnIncentiveReadSkipped).Inc()
		return &SnIncentiveReadResult{Result: SnIncentiveReadSkipped, SubnetEpochIndex: storedIndex}, nil
	}
	if stored {
		blockSeconds := cfg.BlockSeconds
		if blockSeconds <= 0 {
			blockSeconds = stDefaultBlockSeconds
		}
		tempoBlocks := uint64(storedTempo)
		if tempoBlocks == 0 {
			tempoBlocks = snIncentiveDefaultTempoBlocks
		}
		tempo := time.Duration(float64(tempoBlocks)*snIncentiveTempoFraction) * time.Duration(blockSeconds) * time.Second
		if now.Sub(storedAt) < tempo {
			snIncentiveReads.WithLabelValues(SnIncentiveReadSkipped).Inc()
			return &SnIncentiveReadResult{Result: SnIncentiveReadSkipped, SubnetEpochIndex: storedIndex}, nil
		}
	}
	genesis := common.Hash(cfg.GenesisHash)

	var index uint64
	err := source.nativeRead(ctx, func(ctx context.Context, rpc stmetagraph.Rpc) error {
		checkpoint, checkpointBytes, err := state.reader.ReadCheckpoint(ctx, rpc, genesis, snIncentiveIndexBudget)
		snIncentiveResponseBytes.WithLabelValues("index").Add(float64(checkpointBytes))
		if err != nil {
			return err
		}
		value, indexBytes, err := state.reader.SubnetEpochIndex(ctx, rpc, checkpoint, netuid, snIncentiveIndexBudget-checkpointBytes)
		snIncentiveResponseBytes.WithLabelValues("index").Add(float64(indexBytes))
		index = value
		return err
	})
	if err != nil {
		state.lastFailedAt = now
		snIncentiveReadErrors.WithLabelValues(stmetagraph.ErrorReason(err)).Inc()
		return nil, err
	}
	if stored {
		snIncentiveLagTempos.Set(float64(max(index, storedIndex) - storedIndex))
	}
	if stored && index <= storedIndex {
		snIncentiveReads.WithLabelValues(SnIncentiveReadUnchanged).Inc()
		return &SnIncentiveReadResult{Result: SnIncentiveReadUnchanged, SubnetEpochIndex: index}, nil
	}

	// the UIDs bound fleets name now or from the next epoch
	identityUids := []uint16{}
	if summary := model.GetStEpochSummaryCache(ctx, deploymentKey); summary != nil {
		identityUids = model.SnFleetBindingUids(model.GetSnFleetBindingVersions(ctx, deploymentKey), summary.Epoch, summary.Epoch+1)
	}
	if !slices.Contains(identityUids, snIncentiveDefaultReferenceUid) {
		identityUids = append(identityUids, snIncentiveDefaultReferenceUid)
	}
	// the reference validator's row first: UID 1, and the UIDs the configured
	// validator hotkeys held at the last census
	firstRowUids := []uint16{snIncentiveDefaultReferenceUid}
	if prior := model.GetLatestSnIncentiveSnapshot(ctx, deploymentKey); prior != nil {
		for _, row := range prior.Uids {
			if row.HolderRead && slices.Contains(cfg.ValidatorHotkeys, row.Hotkey) {
				firstRowUids = append([]uint16{row.Uid}, firstRowUids...)
			}
		}
	}
	var census *stmetagraph.Census
	err = source.nativeRead(ctx, func(ctx context.Context, rpc stmetagraph.Rpc) error {
		value, err := state.reader.ReadCensus(ctx, rpc, genesis, netuid, stmetagraph.CensusOptions{
			IdentityUids: identityUids, FirstRowUids: firstRowUids, MaxValidatorRows: snIncentiveMaxValidatorRows, MaxResponseBytes: snIncentiveCensusBudget,
		})
		census = value
		return err
	})
	if err != nil {
		state.lastFailedAt = now
		snIncentiveReadErrors.WithLabelValues(stmetagraph.ErrorReason(err)).Inc()
		return nil, err
	}
	state.lastFailedAt = time.Time{}
	snIncentiveResponseBytes.WithLabelValues("census").Add(float64(census.ResponseBytes))
	snapshot := snIncentiveSnapshot(census, cfg.ValidatorHotkeys, now)
	if model.InsertSnIncentiveSnapshot(ctx, deploymentKey, snapshot) {
		snIncentiveReads.WithLabelValues(SnIncentiveReadStored).Inc()
	} else {
		snIncentiveReads.WithLabelValues(SnIncentiveReadUnchanged).Inc()
	}
	observeSnIncentiveSnapshot(snapshot, now)
	if removed := model.RemoveSnIncentiveSnapshotsBefore(ctx, deploymentKey, now.Add(-snIncentiveRetention)); 0 < removed {
		glog.Infof("[sn]removed %d incentive snapshots past retention\n", removed)
	}
	return &SnIncentiveReadResult{Result: SnIncentiveReadStored, SubnetEpochIndex: snapshot.SubnetEpochIndex, Snapshot: snapshot}, nil
}

// The reference validator of a census: the lowest permitted UID whose holder
// is a configured validator hotkey, or UID 1 when st.yml lists none.
func snIncentiveReferenceUid(census *stmetagraph.Census, validatorHotkeys [][32]byte) (uint16, bool) {
	if len(validatorHotkeys) == 0 {
		uid := uint16(snIncentiveDefaultReferenceUid)
		if int(uid) < len(census.ValidatorPermit) && census.ValidatorPermit[uid] {
			return uid, true
		}
		return 0, false
	}
	for uid, permitted := range census.ValidatorPermit {
		if !permitted {
			continue
		}
		if identity, ok := census.Identities[uint16(uid)]; ok && slices.Contains(validatorHotkeys, identity.Hotkey) {
			return uint16(uid), true
		}
	}
	return 0, false
}

// snIncentiveSnapshot turns a census into a stored snapshot with its stale
// flags.
func snIncentiveSnapshot(census *stmetagraph.Census, validatorHotkeys [][32]byte, readAt time.Time) *model.SnIncentiveSnapshot {
	snapshot := &model.SnIncentiveSnapshot{
		SubnetEpochIndex: census.SubnetEpochIndex, Block: census.Checkpoint.Number, BlockHash: census.Checkpoint.Hash.Hex(), ReadAt: readAt,
		Netuid: census.Netuid, SubnetworkN: census.SubnetworkN, Tempo: census.Tempo, ActivityCutoff: uint64(census.ActivityCutoff),
		ValidatorRowsComplete: census.ValidatorRowsComplete, ResponseBytes: int64(census.ResponseBytes),
		StaleReasons: []string{}, ValidatorRows: []model.SnValidatorWeightRow{},
	}
	for uid := 0; uid < int(census.SubnetworkN); uid++ {
		row := &model.SnIncentiveUid{
			Uid: uint16(uid), Incentive: census.Incentive[uid], LastUpdate: census.LastUpdate[uid],
			ValidatorPermit: census.ValidatorPermit[uid], ValidatorTrust: census.ValidatorTrust[uid],
		}
		if identity, ok := census.Identities[uint16(uid)]; ok {
			row.HolderRead, row.Hotkey, row.BlockAtRegistration = true, identity.Hotkey, identity.BlockAtRegistration
		}
		snapshot.Uids = append(snapshot.Uids, row)
	}
	rows := map[uint16][]model.SnWeightEntry{}
	for _, validatorRow := range census.ValidatorRows {
		row := make([]model.SnWeightEntry, len(validatorRow.Row))
		for i, entry := range validatorRow.Row {
			row[i] = model.SnWeightEntry{Uid: entry.Uid, Value: entry.Value}
		}
		rows[validatorRow.Uid] = row
		snapshot.ValidatorRows = append(snapshot.ValidatorRows, model.SnValidatorWeightRow{Uid: validatorRow.Uid, Row: row})
	}
	referenceUid, ok := snIncentiveReferenceUid(census, validatorHotkeys)
	if !ok {
		snapshot.Stale, snapshot.StaleReasons = true, []string{model.SnIncentiveStaleNoReference}
		return snapshot
	}
	snapshot.ReferenceUid = &referenceUid
	snapshot.ReferenceLastUpdate = census.LastUpdate[referenceUid]
	snapshot.ReferenceValidatorTrust = census.ValidatorTrust[referenceUid]
	if row, read := rows[referenceUid]; read && slices.Equal(row, snIncentiveFallbackRow) {
		snapshot.FallbackRow = true
		snapshot.StaleReasons = append(snapshot.StaleReasons, model.SnIncentiveStaleFallbackRow)
	}
	if snapshot.ReferenceLastUpdate < census.Checkpoint.Number && uint64(census.ActivityCutoff) < census.Checkpoint.Number-snapshot.ReferenceLastUpdate {
		snapshot.StaleReasons = append(snapshot.StaleReasons, model.SnIncentiveStaleReferenceInactive)
	}
	if snapshot.ReferenceValidatorTrust < snIncentiveLowValidatorTrust {
		snapshot.StaleReasons = append(snapshot.StaleReasons, model.SnIncentiveStaleLowValidatorTrust)
	}
	snapshot.Stale = 0 < len(snapshot.StaleReasons)
	return snapshot
}

// SnLatestIncentive returns the newest stored census for the board, or nil
// before the first.
func SnLatestIncentive(ctx context.Context) (*model.SnIncentiveSnapshot, error) {
	cfg := stConfig()
	if cfg == nil || cfg.DeploymentKey() == "" {
		return nil, errStNotConfigured
	}
	return model.GetLatestSnIncentiveSnapshot(ctx, cfg.DeploymentKey()), nil
}
