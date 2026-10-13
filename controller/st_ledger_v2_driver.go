package controller

// st_ledger_v2_driver.go — the settlement v2 epoch driver (VERSION2-FINAL.md
// §8.1) on the existing engine. Each sync turn plans steps from the epoch
// clock and the event mirror; each step re-reads vault2 at the finalized head
// before it writes, so steps are idempotent and safe to retry.
//
// Per epoch e, in order: close inside the commit window; finalize the
// previous root if it can be finalized; read the latest finalized root and
// the root count; build; publish the chunks, then the header; commit with
// that root count, never when the total does not grow. A close or commit is
// sent only while the block after the latest is inside its window. Demands
// are answered with checkPath from a cached tree, finalized roots are
// recorded for readers, and the §6.4 and §8.1 alerts are raised.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"

	"github.com/urnetwork/glog"

	"github.com/urfoundation/sn/merkle"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/startifact"
)

const (
	// stLedgerV2ChallengeBlocks is vault2's fixed CHALLENGE_BLOCKS, used only
	// to time sync-turn plans; steps read the constant from chain.
	stLedgerV2ChallengeBlocks = 14_400
	// stLedgerV2MaxCoveredEpochs bounds the epochs one root covers (§6.4).
	stLedgerV2MaxCoveredEpochs = 96
	// stLedgerV2PollDelay spaces the waits for approvals, demands and pause.
	stLedgerV2PollDelay = 5 * time.Minute
	// stLedgerV2TreeCacheSize keeps the trees of the latest finalized root and
	// the pending root, with room for one replacement of each.
	stLedgerV2TreeCacheSize = 4
)

// The alerts of §6.4 and §8.1. Each fires once per process and subject.
const (
	StLedgerV2AlertMissedClose        = "v2_missed_close"
	StLedgerV2AlertMissedCommitWindow = "v2_missed_commit_window"
	StLedgerV2AlertOpenDemand         = "v2_open_demand_over_one_day"
	StLedgerV2AlertUnfinalizedRoot    = "v2_root_not_finalized"
	StLedgerV2AlertPruneRisk          = "v2_sn25_prune_risk"
)

// The v2 driver's steps, for the task layer.
const (
	StLedgerV2StepEpoch    = "epoch"
	StLedgerV2StepFinalize = "finalize"
	StLedgerV2StepAnswer   = "answer"
	StLedgerV2StepRecord   = "record"
)

// StLedgerV2Step is one planned driver step. An answer names its demand: the
// pending root's seq, the finalized root RootIndex and the account Index.
type StLedgerV2Step struct {
	Kind      string
	Epoch     uint64
	Seq       uint64
	RootIndex uint64
	Index     uint64
	RunAt     time.Time
}

var errStLedgerV2NotConfigured = errors.New("st ledger v2: not configured")

// LedgerV2Enabled reports whether settlement v2 addresses are configured.
func (self *StConfig) LedgerV2Enabled() bool {
	return self != nil && self.SettlementV2 != (common.Address{})
}

// Selects the optional v2 identity of an enabled st.yml profile.
func stLedgerV2ConfigAddresses(cfg *StConfig, settlement string, vault string) error {
	if err := server.ValidateLedgerV2Addresses(settlement, vault); err != nil {
		return err
	}
	if settlement == "" {
		return nil
	}
	cfg.SettlementV2, cfg.VaultV2 = common.HexToAddress(settlement), common.HexToAddress(vault)
	for _, v1 := range []common.Address{cfg.ContractAddress, cfg.SettlementVault, cfg.ReserveSink} {
		if cfg.SettlementV2 == v1 || cfg.VaultV2 == v1 {
			return errors.New("settlement v2 addresses must differ from the v1 contracts")
		}
	}
	return nil
}

// The lowercase form sn.yml declares, and "" while v2 is off.
func stLedgerV2Address(address common.Address) string {
	if address == (common.Address{}) {
		return ""
	}
	return fmt.Sprintf("0x%x", address)
}

func stLedgerV2Require() (*StConfig, StLedgerV2Client, error) {
	cfg := stConfig()
	if cfg == nil || !cfg.Enabled || !cfg.LedgerV2Enabled() {
		return nil, nil, errStLedgerV2NotConfigured
	}
	client := stLedgerV2Client()
	if client == nil {
		return nil, nil, errStLedgerV2NotConfigured
	}
	return cfg, client, nil
}

// ---------------------------------------------------------------------------
// Ledger objects
// ---------------------------------------------------------------------------

var stLedgerV2StoreLock sync.Mutex
var stLedgerV2StoreInstance server.BlobStore

// SetStLedgerV2ObjectStore overrides the blob store that holds ledger objects
// (for tests); nil restores the configured store.
func SetStLedgerV2ObjectStore(store server.BlobStore) {
	stLedgerV2StoreLock.Lock()
	defer stLedgerV2StoreLock.Unlock()
	stLedgerV2StoreInstance = store
}

func stLedgerV2Store() (server.BlobStore, error) {
	stLedgerV2StoreLock.Lock()
	instance := stLedgerV2StoreInstance
	stLedgerV2StoreLock.Unlock()
	if instance != nil {
		return instance, nil
	}
	store, ok := server.LoadBlobStore()
	if !ok {
		return nil, errors.New("st ledger v2: the ledger object store is unavailable")
	}
	return store, nil
}

// PutStLedgerV2Object stores a ledger object under its sha256 unless it is
// already present, and returns its "sha256:<hex>" reference.
func PutStLedgerV2Object(ctx context.Context, b []byte) (string, error) {
	store, err := stLedgerV2Store()
	if err != nil {
		return "", err
	}
	return startifact.PutLedgerObject(ctx, store, b)
}

// GetStLedgerV2Object returns the ledger object a reference names. A missing
// object is startifact.ErrLedgerObjectNotFound.
func GetStLedgerV2Object(ctx context.Context, ref string) ([]byte, error) {
	store, err := stLedgerV2Store()
	if err != nil {
		return nil, err
	}
	return startifact.GetLedgerObject(ctx, store, ref)
}

// ---------------------------------------------------------------------------
// Alerts and immutable chain facts
// ---------------------------------------------------------------------------

var stLedgerV2Alerted sync.Map

// stLedgerV2AlertSink receives each alert; tests replace it.
var stLedgerV2AlertSink = func(name string, subject string, message string) {
	glog.Errorf("[st]v2 ALERT %s %s: %s\n", name, subject, message)
}

func stLedgerV2Alert(cfg *StConfig, name string, subject string, format string, args ...any) {
	if _, raised := stLedgerV2Alerted.LoadOrStore(fmt.Sprintf("%s|%d|%s|%s", cfg.DeploymentKey(), cfg.NoId, name, subject), true); raised {
		return
	}
	stLedgerV2AlertSink(name, subject, fmt.Sprintf(format, args...))
}

type stLedgerV2Pool struct {
	registered bool
	firstEpoch uint64
	checked    time.Time
}

var stLedgerV2Pools sync.Map

// stLedgerV2FirstEpoch reads the pool's first v2 epoch from vault2.pools.
// Registration is permanent, so a registered pool is recorded durably as the
// deployment's activation (SV-9) and cached for good; an unregistered answer
// is read again after a minute. A registration that cannot be recorded, under
// an STSettlement2 that is not vault2's coordinator or against a different
// record, is an error.
func stLedgerV2FirstEpoch(ctx context.Context, cfg *StConfig, client StLedgerV2Client) (uint64, bool, error) {
	key := fmt.Sprintf("%s|%s|%d", cfg.DeploymentKey(), cfg.VaultV2.Hex(), cfg.NoId)
	if value, ok := stLedgerV2Pools.Load(key); ok {
		pool := value.(stLedgerV2Pool)
		if pool.registered || server.NowUtc().Sub(pool.checked) < time.Minute {
			return pool.firstEpoch, pool.registered, nil
		}
	}
	registered, firstEpoch, err := client.LedgerV2Pool(ctx, cfg.NoId)
	if err != nil {
		return 0, false, err
	}
	if registered {
		if err := stLedgerV2RecordActivation(ctx, cfg, client, firstEpoch); err != nil {
			return 0, false, err
		}
	}
	stLedgerV2Pools.Store(key, stLedgerV2Pool{registered: registered, firstEpoch: firstEpoch, checked: server.NowUtc()})
	return firstEpoch, registered, nil
}

type stLedgerV2EpochWindow struct {
	end    uint64
	window uint64
}

var stLedgerV2Windows sync.Map

// stLedgerV2Window returns end(e) and the epoch's commit window. Policies are
// scheduled only for future epochs, so an epoch that has started keeps its
// window, and the driver asks only for those.
func stLedgerV2Window(ctx context.Context, cfg *StConfig, client StLedgerV2Client, epoch uint64) (uint64, uint64, error) {
	key := fmt.Sprintf("%s|%d", cfg.DeploymentKey(), epoch)
	if value, ok := stLedgerV2Windows.Load(key); ok {
		window := value.(stLedgerV2EpochWindow)
		return window.end, window.window, nil
	}
	end, window, err := client.LedgerV2EpochWindow(ctx, epoch)
	if err != nil {
		return 0, 0, err
	}
	stLedgerV2Windows.Store(key, stLedgerV2EpochWindow{end: end, window: window})
	return end, window, nil
}

// ---------------------------------------------------------------------------
// v1 cutoff (SV-4)
// ---------------------------------------------------------------------------

// ErrStV1EpochSettledByV2 refuses a v1 close or commit of an epoch at or
// after the pool's first v2 epoch.
var ErrStV1EpochSettledByV2 = errors.New("st: the epoch is settled by v2")

// stLedgerV1SweepEpoch is the phase C hook (VERSION2-FINAL.md §16): the one
// epoch at or after the pool's first v2 epoch that the v1 driver still closes
// and commits, to sweep v1 carry into vault2. Phase A has no sweep.
var stLedgerV1SweepEpoch = func(ctx context.Context, cfg *StConfig) (uint64, bool) {
	return 0, false
}

// StV1EpochAllowed reports whether the v1 driver may close or commit an
// epoch: always without settlement v2 or before vault2 registers the pool,
// and afterwards only below the pool's first v2 epoch, or for the phase C
// sweep epoch. Once the pool's activation is recorded, its first epoch is the
// cutoff whatever vault2 is read to answer, and a close or commit without the
// recorded v2 pair is refused before this is asked (SV-9).
func StV1EpochAllowed(ctx context.Context, epoch uint64) (bool, error) {
	return stV1EpochAllowed(ctx, stConfig(), epoch)
}

func stV1EpochAllowed(ctx context.Context, cfg *StConfig, epoch uint64) (bool, error) {
	if !cfg.LedgerV2Enabled() {
		return true, nil
	}
	firstEpoch, registered := uint64(0), false
	if activation := stLedgerV2RecordedActivation(ctx, cfg); activation != nil {
		// A read that lags behind the registration, or its cached answer,
		// cannot return the recorded v2 epochs to v1.
		firstEpoch, registered = activation.FirstEpoch, true
	} else {
		client := stLedgerV2Client()
		if client == nil {
			return false, errStLedgerV2NotConfigured
		}
		var err error
		if firstEpoch, registered, err = stLedgerV2FirstEpoch(ctx, cfg, client); err != nil {
			return false, err
		}
	}
	if !registered || epoch < firstEpoch {
		return true, nil
	}
	sweepEpoch, sweep := stLedgerV1SweepEpoch(ctx, cfg)
	return sweep && sweepEpoch == epoch, nil
}

func stV1EpochAdmission(ctx context.Context, cfg *StConfig, epoch uint64) error {
	allowed, err := stV1EpochAllowed(ctx, cfg, epoch)
	if err != nil {
		return err
	}
	if !allowed {
		return fmt.Errorf("%w: epoch %d", ErrStV1EpochSettledByV2, epoch)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Sync turn
// ---------------------------------------------------------------------------

// The latest terminal status of a v2 step's publish rows for an epoch, or "".
func stLedgerV2Outcome(ctx context.Context, deploymentKey model.StDeploymentKey, epoch uint64, kind string) string {
	status := ""
	for _, publish := range model.GetStPublishes(ctx, deploymentKey, epoch) {
		if publish.Kind == kind && (publish.Status == model.StPublishStatusConfirmed || publish.Status == model.StPublishStatusSkipped || publish.Status == model.StPublishStatusMissed) {
			status = publish.Status
		}
	}
	return status
}

// StLedgerV2Sync plans one sync turn's steps from the epoch clock and the
// mirror, and raises the alerts. Chain reads are limited to the pool's
// registration and epoch windows, both cached, and the subnet census, read
// at most once per stLedgerV2PruneCheckInterval.
func StLedgerV2Sync(ctx context.Context, state *StEpochState) ([]*StLedgerV2Step, error) {
	cfg, client, err := stLedgerV2Require()
	if errors.Is(err, errStLedgerV2NotConfigured) {
		return nil, nil
	}
	// Dissolution strands v1 and v2 custody alike, so the check does not wait
	// for the pool's registration.
	if err := stLedgerV2CheckPrune(ctx, cfg, client, server.NowUtc()); err != nil {
		glog.Infof("[st]v2 prune check failed: %s\n", err)
	}
	firstEpoch, registered, err := stLedgerV2FirstEpoch(ctx, cfg, client)
	if err != nil || !registered {
		return nil, err
	}
	deploymentKey, noId, head := cfg.DeploymentKey(), cfg.NoId, state.HeadBlock
	now := server.NowUtc()
	runAt := func(block uint64) time.Time {
		at := stEstimateBlockTime(state.HeadBlock, state.HeadBlockTime, block, cfg.BlockSeconds)
		if at.Before(now) {
			return now
		}
		return at
	}
	steps := []*StLedgerV2Step{}
	// The mirror has applied every event before this block.
	mirrored := model.GetStChainCheckpoint(ctx, deploymentKey).NextBlock

	if firstEpoch < state.Epoch {
		epoch := state.Epoch - 1
		end, window, err := stLedgerV2Window(ctx, cfg, client, epoch)
		if err != nil {
			return nil, err
		}
		if head <= end+window {
			if stLedgerV2Outcome(ctx, deploymentKey, epoch, model.StPublishKindV2Commit) == "" {
				steps = append(steps, &StLedgerV2Step{Kind: StLedgerV2StepEpoch, Epoch: epoch, RunAt: runAt(end)})
			}
		} else if end+window < mirrored {
			if funding := model.GetStLedgerV2Funding(ctx, deploymentKey, noId, epoch); funding == nil || funding.Deferred {
				stLedgerV2Alert(cfg, StLedgerV2AlertMissedClose, fmt.Sprintf("epoch %d", epoch), "no close by block %d; the next close defers the epoch", end+window)
			}
			committed := stLedgerV2Outcome(ctx, deploymentKey, epoch, model.StPublishKindV2Commit) == model.StPublishStatusSkipped
			for _, root := range model.GetStLedgerV2RootsFromEpoch(ctx, deploymentKey, noId, epoch) {
				committed = committed || root.Epoch == epoch
			}
			if !committed {
				stLedgerV2Alert(cfg, StLedgerV2AlertMissedCommitWindow, fmt.Sprintf("epoch %d", epoch), "no root committed by block %d", end+window)
			}
		}
	}

	if pending := model.GetStLedgerV2PendingRoot(ctx, deploymentKey, noId); pending != nil {
		if pending.CommitBlock != nil && stLedgerV2Outcome(ctx, deploymentKey, pending.Epoch, model.StPublishKindV2Finalize) == "" {
			steps = append(steps, &StLedgerV2Step{Kind: StLedgerV2StepFinalize, Epoch: pending.Epoch, RunAt: runAt(*pending.CommitBlock + stLedgerV2ChallengeBlocks)})
		}
		oneDay := uint64(24*time.Hour/time.Second) / uint64(stLedgerV2BlockSeconds(cfg))
		for _, demand := range model.GetOpenStLedgerV2Demands(ctx, deploymentKey, noId, pending.Seq) {
			steps = append(steps, &StLedgerV2Step{Kind: StLedgerV2StepAnswer, Epoch: pending.Epoch, Seq: demand.Seq, RootIndex: demand.RootIndex, Index: demand.Index, RunAt: now})
			if demand.DemandBlock+oneDay < head {
				stLedgerV2Alert(cfg, StLedgerV2AlertOpenDemand, fmt.Sprintf("seq %d root %d index %d", demand.Seq, demand.RootIndex, demand.Index), "demanded at block %d and still open at block %d", demand.DemandBlock, head)
			}
		}
	}

	if 2 <= state.Epoch {
		for _, root := range model.GetStLedgerV2RootsFromEpoch(ctx, deploymentKey, noId, state.Epoch-2) {
			if root.Status == model.StLedgerV2RootFinalized {
				continue
			}
			end, window, err := stLedgerV2Window(ctx, cfg, client, root.Epoch)
			if err != nil {
				return nil, err
			}
			if deadline := end + stLedgerV2ChallengeBlocks + window; deadline < head {
				stLedgerV2Alert(cfg, StLedgerV2AlertUnfinalizedRoot, fmt.Sprintf("seq %d", root.Seq), "the root of epoch %d is %s at block %d, past block %d", root.Epoch, root.Status, head, deadline)
			}
		}
	}

	if root := model.GetNextUnrecordedStLedgerV2Root(ctx, deploymentKey, noId); root != nil {
		steps = append(steps, &StLedgerV2Step{Kind: StLedgerV2StepRecord, Epoch: root.Epoch, Seq: root.Seq, RunAt: now})
	}
	return steps, nil
}

// ---------------------------------------------------------------------------
// Steps
// ---------------------------------------------------------------------------

// StLedgerV2RunStep runs one planned step. Each step re-reads vault2 at the
// finalized head, so a repeated or late step only does what is still due.
func StLedgerV2RunStep(ctx context.Context, step *StLedgerV2Step) (*StPublishOutcome, error) {
	cfg, client, err := stLedgerV2Require()
	if err != nil {
		return nil, err
	}
	if err := stPayoutAdmission(ctx, cfg); err != nil {
		return nil, err
	}
	switch step.Kind {
	case StLedgerV2StepEpoch:
		return stLedgerV2RunEpoch(ctx, cfg, client, step.Epoch)
	case StLedgerV2StepFinalize:
		return stLedgerV2RunFinalize(ctx, cfg, client, step.Epoch)
	case StLedgerV2StepAnswer:
		return stLedgerV2RunAnswer(ctx, cfg, client, step.Seq, step.RootIndex, step.Index)
	case StLedgerV2StepRecord:
		return stLedgerV2RunRecord(ctx, cfg, client, step.Seq)
	}
	return nil, fmt.Errorf("st ledger v2: unknown step %q", step.Kind)
}

func stLedgerV2Retry(err error) *StPublishOutcome {
	return &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: err.Error(), Retry: true}
}

func stLedgerV2BlockSeconds(cfg *StConfig) int64 {
	if cfg.BlockSeconds <= 0 {
		return stDefaultBlockSeconds
	}
	return cfg.BlockSeconds
}

// A wait until a block, or for the poll delay when block is not ahead.
func stLedgerV2Wait(cfg *StConfig, state *StLedgerV2State, reason string, block uint64) *StPublishOutcome {
	at := server.NowUtc().Add(stLedgerV2PollDelay)
	if state.HeadBlock < block {
		at = server.NowUtc().Add(time.Duration(block-state.HeadBlock) * time.Duration(stLedgerV2BlockSeconds(cfg)) * time.Second)
	}
	return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: reason, Retry: true, RetryAt: at}
}

// Records a step outcome that ends the step for its epoch.
func stLedgerV2Conclude(ctx context.Context, cfg *StConfig, epoch uint64, kind string, status string, format string, args ...any) *StPublishOutcome {
	outcome := &StPublishOutcome{Status: status, Reason: fmt.Sprintf(format, args...)}
	stResolvePublish(ctx, model.AddStPublish(ctx, cfg.DeploymentKey(), epoch, kind), outcome)
	return outcome
}

// Records one chain write as a publish row. A send error is retried, except a
// spent commit intent, which ends the epoch's commit, and a revert showing the
// write already done by another sender (SPEC A6).
func stLedgerV2Send(ctx context.Context, cfg *StConfig, epoch uint64, kind string, send func() (string, error)) *StPublishOutcome {
	publishId := model.AddStPublish(ctx, cfg.DeploymentKey(), epoch, kind)
	txHash, err := send()
	outcome := &StPublishOutcome{Status: model.StPublishStatusConfirmed, TxHash: txHash}
	if errors.Is(err, ErrStLedgerV2CommitSpent) {
		outcome = &StPublishOutcome{Status: model.StPublishStatusMissed, TxHash: txHash, Reason: err.Error()}
	} else if name, done := stLedgerV2AlreadyDone(err); done {
		outcome = &StPublishOutcome{Status: model.StPublishStatusSkipped, TxHash: txHash, Reason: "already done: " + name}
	} else if err != nil {
		outcome = &StPublishOutcome{Status: model.StPublishStatusFailed, TxHash: txHash, Reason: err.Error(), Retry: true}
	}
	stResolvePublish(ctx, publishId, outcome)
	return outcome
}

// stLedgerV2LateSend holds a close or commit whose window ends at or before
// the latest block, and returns nil when the block after the latest is still
// inside the window. A send's eth_call and eth_estimateGas run at the latest
// block, and the transaction executes in a later one (R battery). A held send
// waits for the finalized head to pass the window; the step then sees from
// finalized state whether the epoch was closed or committed, or missed.
func stLedgerV2LateSend(ctx context.Context, cfg *StConfig, client StLedgerV2Client, state *StLedgerV2State, step string, deadline uint64) *StPublishOutcome {
	latest, err := client.LedgerV2LatestBlock(ctx)
	if err != nil {
		return stLedgerV2Retry(err)
	}
	if latest < deadline {
		return nil
	}
	return stLedgerV2Wait(cfg, state, fmt.Sprintf("the %s window ends at block %d and the latest block is %d", step, deadline, latest), deadline+1)
}

// stLedgerV2AlreadyDone reports a revert that shows a write already done by
// another sender (SPEC A6): NoFault, the demand is answered, or
// NoPendingRoot, the root was finalized, replaced or deleted.
func stLedgerV2AlreadyDone(err error) (string, bool) {
	selector, reverted := stLedgerV2RevertSelector(err)
	if name := stLedgerV2ErrorNames[selector]; reverted && (name == "NoFault" || name == "NoPendingRoot") {
		return name, true
	}
	return "", false
}

func stLedgerV2Finalizable(state *StLedgerV2State) bool {
	pending := state.Pending
	return pending != nil && !state.Paused && pending.CommittedBlock+state.ChallengeBlocks <= state.HeadBlock &&
		0 < state.Quorum && state.Quorum <= state.Approvals && pending.OpenDemands == 0
}

// stLedgerV2RunEpoch drives epoch e through §8.1's order.
func stLedgerV2RunEpoch(ctx context.Context, cfg *StConfig, client StLedgerV2Client, epoch uint64) (*StPublishOutcome, error) {
	noId := cfg.NoId
	state, err := client.LedgerV2State(ctx, noId)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	if !state.Registered || epoch < state.FirstEpoch {
		return stLedgerV2Conclude(ctx, cfg, epoch, model.StPublishKindV2Commit, model.StPublishStatusSkipped, "epoch %d is not a v2 epoch of pool %d", epoch, noId), nil
	}
	end, window, err := stLedgerV2Window(ctx, cfg, client, epoch)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	if state.HeadBlock < end {
		return stLedgerV2Wait(cfg, state, fmt.Sprintf("epoch %d ends at block %d", epoch, end), end), nil
	}

	// 1. Close inside the window. A missed close defers the epoch at the next
	// close, which also leaves the epoch without a commit.
	if state.LastFundedEpoch < epoch {
		if end+window < state.HeadBlock {
			stLedgerV2Alert(cfg, StLedgerV2AlertMissedClose, fmt.Sprintf("epoch %d", epoch), "no close by block %d; the next close defers the epoch", end+window)
			return stLedgerV2Conclude(ctx, cfg, epoch, model.StPublishKindV2Commit, model.StPublishStatusMissed, "close window closed at block %d", end+window), nil
		}
		if state.Paused {
			return stLedgerV2Wait(cfg, state, "settlement is paused", 0), nil
		}
		if outcome := stLedgerV2LateSend(ctx, cfg, client, state, "close", end+window); outcome != nil {
			return outcome, nil
		}
		if outcome := stLedgerV2Send(ctx, cfg, epoch, model.StPublishKindV2Close, func() (string, error) {
			return client.LedgerV2Close(ctx, epoch, noId)
		}); outcome.Retry {
			return outcome, nil
		}
		if state, err = client.LedgerV2State(ctx, noId); err != nil {
			return stLedgerV2Retry(err), nil
		}
		if state.LastFundedEpoch < epoch {
			return stLedgerV2Retry(fmt.Errorf("the close of epoch %d is not visible at block %d", epoch, state.HeadBlock)), nil
		}
	}
	if (state.Pending != nil && epoch <= state.Pending.Epoch) || (state.Latest != nil && epoch <= state.Latest.Epoch) {
		return stLedgerV2Conclude(ctx, cfg, epoch, model.StPublishKindV2Commit, model.StPublishStatusSkipped, "a root for epoch %d is already committed", epoch), nil
	}

	// 2. Finalize the previous root if it can be finalized. Otherwise the new
	// root is built on the latest finalized root and replaces it.
	if stLedgerV2Finalizable(state) {
		pendingEpoch := state.Pending.Epoch
		if outcome := stLedgerV2Send(ctx, cfg, pendingEpoch, model.StPublishKindV2Finalize, func() (string, error) {
			return client.LedgerV2Finalize(ctx, pendingEpoch, noId)
		}); outcome.Retry {
			return outcome, nil
		}
		if state, err = client.LedgerV2State(ctx, noId); err != nil {
			return stLedgerV2Retry(err), nil
		}
	}
	return stLedgerV2CommitEpoch(ctx, cfg, client, state, epoch, end+window)
}

// Steps 3-6 for epoch e: read, build, publish, commit.
func stLedgerV2CommitEpoch(ctx context.Context, cfg *StConfig, client StLedgerV2Client, state *StLedgerV2State, epoch uint64, deadline uint64) (*StPublishOutcome, error) {
	noId := cfg.NoId
	if deadline < state.HeadBlock {
		return stLedgerV2MissedCommit(ctx, cfg, epoch, deadline), nil
	}

	// 3. The latest finalized root and the root count.
	previous, rootCount := state.Latest, state.RootCount
	funded, err := client.LedgerV2FundedThrough(ctx, noId, epoch)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	var finalizedTotal uint256.Int
	if previous != nil {
		finalizedTotal = previous.Total
	}
	if !funded.Gt(&finalizedTotal) {
		return stLedgerV2Conclude(ctx, cfg, epoch, model.StPublishKindV2Commit, model.StPublishStatusSkipped, "the total %s does not exceed the finalized total %s", funded.Dec(), finalizedTotal.Dec()), nil
	}

	// 4. Build.
	input, err := stLedgerV2Input(ctx, cfg, client, previous, state.FirstEpoch, epoch)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	stLedgerV2Rebuilds(state, input)
	builder := stLedgerBuilder()
	if builder == nil {
		return stLedgerV2Retry(errors.New("st ledger v2: no ledger builder is installed")), nil
	}
	build, err := builder.Build(ctx, input)
	if errors.Is(err, ErrStLedgerV2NoRoot) {
		return stLedgerV2Conclude(ctx, cfg, epoch, model.StPublishKindV2Commit, model.StPublishStatusSkipped, "%s", err), nil
	}
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	tree, commitment, err := stLedgerV2Commitment(ctx, input, build, &funded)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}

	// 5. Publish the chunks, then the header.
	if err := stLedgerV2Publish(ctx, cfg, input, build); err != nil {
		return stLedgerV2Retry(err), nil
	}
	stLedgerV2CacheTree(commitment.LedgerHash, &stLedgerV2Tree{tree: tree, accounts: build.Accounts})

	// 6. Commit with that root count.
	if state, err = client.LedgerV2State(ctx, noId); err != nil {
		return stLedgerV2Retry(err), nil
	}
	if deadline < state.HeadBlock {
		return stLedgerV2MissedCommit(ctx, cfg, epoch, deadline), nil
	}
	if state.RootCount != rootCount {
		return stLedgerV2Retry(fmt.Errorf("the root count moved from %d to %d during the build", rootCount, state.RootCount)), nil
	}
	if state.Paused {
		return stLedgerV2Wait(cfg, state, "settlement is paused", 0), nil
	}
	if next := state.LastCommitBlock + state.ChallengeBlocks; state.LastCommitBlock != 0 && state.HeadBlock < next {
		return stLedgerV2Wait(cfg, state, fmt.Sprintf("vault2 accepts the pool's next commit at block %d", next), next), nil
	}
	if outcome := stLedgerV2LateSend(ctx, cfg, client, state, "commit", deadline); outcome != nil {
		return outcome, nil
	}
	return stLedgerV2Send(ctx, cfg, epoch, model.StPublishKindV2Commit, func() (string, error) {
		return client.LedgerV2Commit(ctx, epoch, noId, rootCount, commitment)
	}), nil
}

// stLedgerV2Rebuilds builds again each covered epoch whose rebuild the operator
// requested (§6.4 fixed inputs), unless the pending root lists the epoch's
// recorded rows and can still finalize, which keeps them final. A root for
// epoch e is built at end(e) or later, when approvals of a pending root for an
// earlier epoch have closed (ApprovalClosed at end(pending epoch + 1)), so a
// pending root short of its quorum here never finalizes.
func stLedgerV2Rebuilds(state *StLedgerV2State, input *StLedgerV2BuildInput) {
	pending := state.Pending
	for _, covered := range input.Epochs {
		mayFinalize := pending != nil && covered.Epoch <= pending.Epoch && 0 < state.Quorum && state.Quorum <= state.Approvals
		if covered.Rows != nil && covered.Rows.RebuildRequestedBy != "" && !mayFinalize {
			covered.Rows = nil
		}
	}
}

func stLedgerV2MissedCommit(ctx context.Context, cfg *StConfig, epoch uint64, deadline uint64) *StPublishOutcome {
	stLedgerV2Alert(cfg, StLedgerV2AlertMissedCommitWindow, fmt.Sprintf("epoch %d", epoch), "no root committed by block %d", deadline)
	return stLedgerV2Conclude(ctx, cfg, epoch, model.StPublishKindV2Commit, model.StPublishStatusMissed, "commit window closed at block %d", deadline)
}

// stLedgerV2Input reads one root's inputs: the covered epochs p+1 … e with
// their funding from vault2 and their recorded provider rows.
func stLedgerV2Input(ctx context.Context, cfg *StConfig, client StLedgerV2Client, previous *StLedgerV2FinalizedRoot, firstEpoch uint64, epoch uint64) (*StLedgerV2BuildInput, error) {
	covered := firstEpoch - 1
	if previous != nil {
		covered = previous.Epoch
	}
	if epoch <= covered || stLedgerV2MaxCoveredEpochs < epoch-covered {
		return nil, fmt.Errorf("st ledger v2: a root for epoch %d cannot cover epochs %d to %d", epoch, covered+1, epoch)
	}
	records := model.GetStLedgerV2EpochRows(ctx, cfg.DeploymentKey(), cfg.NoId, covered+1, epoch)
	input := &StLedgerV2BuildInput{NoId: cfg.NoId, Epoch: epoch, Previous: previous}
	var before uint256.Int
	if firstEpoch <= covered {
		var err error
		if before, err = client.LedgerV2FundedThrough(ctx, cfg.NoId, covered); err != nil {
			return nil, err
		}
	}
	for k := covered + 1; k <= epoch; k++ {
		through, err := client.LedgerV2FundedThrough(ctx, cfg.NoId, k)
		if err != nil {
			return nil, err
		}
		if through.Lt(&before) {
			return nil, fmt.Errorf("st ledger v2: fundedThrough decreases at epoch %d", k)
		}
		coveredEpoch := &StLedgerV2CoveredEpoch{Epoch: k, Rows: records[k]}
		coveredEpoch.Funding.Sub(&through, &before)
		input.Epochs = append(input.Epochs, coveredEpoch)
		before = through
	}
	return input, nil
}

// stLedgerV2Commitment builds the sum tree of a build and checks it against
// the chain and the fixed inputs before anything is published.
func stLedgerV2Commitment(ctx context.Context, input *StLedgerV2BuildInput, build *StLedgerV2Build, funded *uint256.Int) (*merkle.LedgerSumTree, *StLedgerV2Commitment, error) {
	tree, err := merkle.BuildLedgerSumTree(ctx, build.Accounts)
	if err != nil {
		return nil, nil, err
	}
	root, total := tree.Root()
	if !total.Eq(funded) {
		return nil, nil, fmt.Errorf("st ledger v2: the built total %s differs from fundedThrough %s", total.Dec(), funded.Dec())
	}
	if input.Previous != nil && tree.LeafCount() < input.Previous.LeafCount {
		return nil, nil, fmt.Errorf("st ledger v2: the build has %d accounts, fewer than the previous root's %d", tree.LeafCount(), input.Previous.LeafCount)
	}
	if len(build.Epochs) != len(input.Epochs) {
		return nil, nil, fmt.Errorf("st ledger v2: the build lists %d epochs, not %d", len(build.Epochs), len(input.Epochs))
	}
	for i, covered := range input.Epochs {
		listed := build.Epochs[i]
		switch {
		case listed.Epoch != covered.Epoch:
			return nil, nil, fmt.Errorf("st ledger v2: the build lists epoch %d in place of %d", listed.Epoch, covered.Epoch)
		case covered.Rows == nil && listed.Null:
			return nil, nil, fmt.Errorf("st ledger v2: epoch %d is null without the operator's declaration", covered.Epoch)
		case covered.Rows != nil && (covered.Rows.Null() != listed.Null || !slices.Equal(covered.Rows.ProviderChunks, listed.ProviderChunks)):
			return nil, nil, fmt.Errorf("st ledger v2: the build changes epoch %d's recorded provider rows", covered.Epoch)
		}
	}
	return tree, &StLedgerV2Commitment{Root: root, Total: total, LeafCount: tree.LeafCount(), LedgerHash: sha256.Sum256(build.Header)}, nil
}

// stLedgerV2Publish writes the chunks, records the newly built epochs' rows,
// then writes the header, so nothing published names a missing object.
func stLedgerV2Publish(ctx context.Context, cfg *StConfig, input *StLedgerV2BuildInput, build *StLedgerV2Build) error {
	for _, chunk := range build.Chunks {
		if _, err := PutStLedgerV2Object(ctx, chunk); err != nil {
			return err
		}
	}
	for i, covered := range input.Epochs {
		if covered.Rows == nil {
			if err := model.RecordStLedgerV2EpochRows(ctx, cfg.DeploymentKey(), cfg.NoId, covered.Epoch, build.Epochs[i].ProviderChunks); err != nil {
				return err
			}
		}
	}
	_, err := PutStLedgerV2Object(ctx, build.Header)
	return err
}

// stLedgerV2RunFinalize finalizes the pending root of an epoch once its window
// has passed with a quorum of approvals and no open demand.
func stLedgerV2RunFinalize(ctx context.Context, cfg *StConfig, client StLedgerV2Client, epoch uint64) (*StPublishOutcome, error) {
	state, err := client.LedgerV2State(ctx, cfg.NoId)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	pending := state.Pending
	if pending == nil || pending.Epoch != epoch {
		return stLedgerV2Conclude(ctx, cfg, epoch, model.StPublishKindV2Finalize, model.StPublishStatusSkipped, "no pending root for epoch %d", epoch), nil
	}
	if next := pending.CommittedBlock + state.ChallengeBlocks; state.HeadBlock < next {
		return stLedgerV2Wait(cfg, state, fmt.Sprintf("the challenge window ends at block %d", next), next), nil
	}
	if state.Quorum == 0 || state.Approvals < state.Quorum {
		end, _, err := stLedgerV2Window(ctx, cfg, client, epoch+1)
		if err != nil {
			return stLedgerV2Retry(err), nil
		}
		if end <= state.HeadBlock {
			return stLedgerV2Conclude(ctx, cfg, epoch, model.StPublishKindV2Finalize, model.StPublishStatusMissed, "approvals closed at block %d with %d of %d", end, state.Approvals, state.Quorum), nil
		}
		return stLedgerV2Wait(cfg, state, fmt.Sprintf("%d of %d approvals", state.Approvals, state.Quorum), 0), nil
	}
	if pending.OpenDemands != 0 {
		return stLedgerV2Wait(cfg, state, fmt.Sprintf("%d open demands", pending.OpenDemands), 0), nil
	}
	if state.Paused {
		return stLedgerV2Wait(cfg, state, "settlement is paused", 0), nil
	}
	return stLedgerV2Send(ctx, cfg, epoch, model.StPublishKindV2Finalize, func() (string, error) {
		return client.LedgerV2Finalize(ctx, epoch, cfg.NoId)
	}), nil
}

// stLedgerV2RunAnswer answers a demand on the pending root with a clean
// opening of the path at the demanded account of the finalized root
// rootIndex, the latest or an older one.
func stLedgerV2RunAnswer(ctx context.Context, cfg *StConfig, client StLedgerV2Client, seq uint64, rootIndex uint64, index uint64) (*StPublishOutcome, error) {
	state, err := client.LedgerV2State(ctx, cfg.NoId)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	pending := state.Pending
	if pending == nil || pending.Seq != seq || state.RootCount <= rootIndex {
		return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: fmt.Sprintf("seq %d is not pending over finalized root %d", seq, rootIndex)}, nil
	}
	demand, err := client.LedgerV2Demand(ctx, cfg.NoId, seq, rootIndex, index)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	if demand != StLedgerV2DemandOpen {
		return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: fmt.Sprintf("seq %d root %d index %d has no open demand", seq, rootIndex, index)}, nil
	}
	root := state.Latest
	if rootIndex != root.Index {
		if root, err = client.LedgerV2Root(ctx, cfg.NoId, rootIndex); err != nil {
			return stLedgerV2Retry(err), nil
		}
	}
	answer, err := stLedgerV2Opening(ctx, root, pending, index)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	return stLedgerV2Send(ctx, cfg, pending.Epoch, model.StPublishKindV2Answer, func() (string, error) {
		return client.LedgerV2Answer(ctx, cfg.NoId, seq, answer)
	}), nil
}

type stLedgerV2Tree struct {
	tree     *merkle.LedgerSumTree
	accounts []merkle.LedgerAccount
}

var stLedgerV2TreeLock sync.Mutex
var stLedgerV2Trees = map[[32]byte]*stLedgerV2Tree{}
var stLedgerV2TreeOrder [][32]byte

func stLedgerV2CacheTree(ledgerHash [32]byte, tree *stLedgerV2Tree) {
	stLedgerV2TreeLock.Lock()
	defer stLedgerV2TreeLock.Unlock()
	if _, ok := stLedgerV2Trees[ledgerHash]; ok {
		return
	}
	if len(stLedgerV2TreeOrder) == stLedgerV2TreeCacheSize {
		delete(stLedgerV2Trees, stLedgerV2TreeOrder[0])
		stLedgerV2TreeOrder = stLedgerV2TreeOrder[1:]
	}
	stLedgerV2Trees[ledgerHash] = tree
	stLedgerV2TreeOrder = append(stLedgerV2TreeOrder, ledgerHash)
}

// stLedgerV2CachedTree returns the tree of a committed root, building it from
// the stored account rows on a miss, and requires it to match the chain.
func stLedgerV2CachedTree(ctx context.Context, ledgerHash [32]byte, root [32]byte, total *uint256.Int, leafCount uint64) (*stLedgerV2Tree, error) {
	stLedgerV2TreeLock.Lock()
	cached := stLedgerV2Trees[ledgerHash]
	stLedgerV2TreeLock.Unlock()
	if cached == nil {
		builder := stLedgerBuilder()
		if builder == nil {
			return nil, errors.New("st ledger v2: no ledger builder is installed")
		}
		accounts, err := builder.Accounts(ctx, ledgerHash)
		if err != nil {
			return nil, err
		}
		tree, err := merkle.BuildLedgerSumTree(ctx, accounts)
		if err != nil {
			return nil, err
		}
		cached = &stLedgerV2Tree{tree: tree, accounts: accounts}
	}
	builtRoot, builtTotal := cached.tree.Root()
	if builtRoot != root || !builtTotal.Eq(total) || cached.tree.LeafCount() != leafCount {
		return nil, fmt.Errorf("st ledger v2: the stored accounts of ledger 0x%x do not rebuild its root", ledgerHash)
	}
	stLedgerV2CacheTree(ledgerHash, cached)
	return cached, nil
}

// StLedgerV2Tree returns the sum tree and the accounts of a committed ledger,
// built from its stored account rows and cached, after checking them against
// the committed root, total and account count. Proofs for readers come from it.
func StLedgerV2Tree(ctx context.Context, ledgerHash [32]byte, root [32]byte, total *uint256.Int, leafCount uint64) (*merkle.LedgerSumTree, []merkle.LedgerAccount, error) {
	cached, err := stLedgerV2CachedTree(ctx, ledgerHash, root, total, leafCount)
	if err != nil {
		return nil, nil, err
	}
	return cached.tree, cached.accounts, nil
}

// stLedgerV2Opening is the checkPath argument set for an index of a finalized
// root, whose accounts the builder reads back from its ledger. It refuses to
// send an opening that shows a fault.
func stLedgerV2Opening(ctx context.Context, root *StLedgerV2FinalizedRoot, pending *StLedgerV2Pending, index uint64) (*StLedgerV2Answer, error) {
	if root.LeafCount <= index {
		return nil, fmt.Errorf("st ledger v2: index %d is outside the finalized root's %d accounts", index, root.LeafCount)
	}
	old, err := stLedgerV2CachedTree(ctx, root.LedgerHash, root.Root, &root.Total, root.LeafCount)
	if err != nil {
		return nil, err
	}
	next, err := stLedgerV2CachedTree(ctx, pending.LedgerHash, pending.Root, &pending.Total, pending.LeafCount)
	if err != nil {
		return nil, err
	}
	account := old.accounts[index]
	proof, _ := old.tree.Proof(index)
	path, err := next.tree.Path(index)
	if err != nil {
		return nil, err
	}
	fault, level, err := merkle.WalkLedgerPath(pending.Root, &pending.Total, pending.LeafCount, index, account.Coldkey, &account.Cumulative, path)
	if err != nil || fault != merkle.LedgerFaultNone {
		return nil, fmt.Errorf("st ledger v2: the pending root opens index %d with fault %s at level %d: %v", index, fault, level, err)
	}
	return &StLedgerV2Answer{RootIndex: root.Index, Index: index, Account: account, Proof: proof, Path: path}, nil
}

// stLedgerV2RunRecord writes a finalized root's new accounts and per-epoch
// amounts for readers, after rebuilding the root from its recorded inputs.
func stLedgerV2RunRecord(ctx context.Context, cfg *StConfig, client StLedgerV2Client, seq uint64) (*StPublishOutcome, error) {
	deploymentKey, noId := cfg.DeploymentKey(), cfg.NoId
	root := model.GetStLedgerV2Root(ctx, deploymentKey, noId, seq)
	if root == nil || root.RootIndex == nil || root.RecordedTime != nil {
		return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: fmt.Sprintf("seq %d is not an unrecorded finalized root", seq)}, nil
	}
	firstEpoch, _, err := stLedgerV2FirstEpoch(ctx, cfg, client)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	var previous *StLedgerV2FinalizedRoot
	if 0 < *root.RootIndex {
		prior := model.GetStLedgerV2FinalizedRoot(ctx, deploymentKey, noId, *root.RootIndex-1)
		if prior == nil {
			return stLedgerV2Retry(fmt.Errorf("st ledger v2: root %d is not mirrored", *root.RootIndex-1)), nil
		}
		previous = &StLedgerV2FinalizedRoot{Index: *prior.RootIndex, Root: prior.Root, Total: prior.Total, LeafCount: prior.LeafCount, Epoch: prior.Epoch, LedgerHash: prior.LedgerHash}
	}
	input, err := stLedgerV2Input(ctx, cfg, client, previous, firstEpoch, root.Epoch)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	for _, covered := range input.Epochs {
		if covered.Rows == nil {
			return stLedgerV2Retry(fmt.Errorf("st ledger v2: epoch %d of root %d has no recorded provider rows", covered.Epoch, *root.RootIndex)), nil
		}
	}
	builder := stLedgerBuilder()
	if builder == nil {
		return stLedgerV2Retry(errors.New("st ledger v2: no ledger builder is installed")), nil
	}
	build, err := builder.Build(ctx, input)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	_, commitment, err := stLedgerV2Commitment(ctx, input, build, &root.Total)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	if commitment.Root != root.Root || commitment.LeafCount != root.LeafCount || commitment.LedgerHash != root.LedgerHash {
		return stLedgerV2Retry(fmt.Errorf("st ledger v2: the rebuilt root %d differs from the finalized one", *root.RootIndex)), nil
	}
	firstIndex := uint64(0)
	if previous != nil {
		firstIndex = previous.LeafCount
	}
	newColdkeys := make([][32]byte, 0, len(build.Accounts)-int(firstIndex))
	for _, account := range build.Accounts[firstIndex:] {
		newColdkeys = append(newColdkeys, account.Coldkey)
	}
	for _, amount := range build.Amounts {
		amount.NoId, amount.RootIndex = noId, *root.RootIndex
	}
	if err := model.RecordStLedgerV2Root(ctx, deploymentKey, noId, seq, firstIndex, newColdkeys, build.Amounts); err != nil {
		return stLedgerV2Retry(err), nil
	}
	stLedgerV2Finalized(ctx, deploymentKey, input.Epochs)
	return &StPublishOutcome{Status: model.StPublishStatusConfirmed, Reason: fmt.Sprintf("root %d recorded", *root.RootIndex)}, nil
}

// StLedgerV2DeclareUnrecoverable is the operator's explicit command naming an
// epoch whose inputs are unrecoverable, so roots list it as providers: null
// (§6.4). It records who declared it, and it is refused once the epoch's
// provider rows are recorded, and under a v2 pair other than the recorded
// activation's (SV-9). The driver never declares null itself.
func StLedgerV2DeclareUnrecoverable(ctx context.Context, epoch uint64, declaredBy string) error {
	cfg := stConfig()
	if !cfg.LedgerV2Enabled() {
		return errStLedgerV2NotConfigured
	}
	if err := stLedgerV2ActivationAdmission(ctx, cfg); err != nil {
		return err
	}
	return model.DeclareStLedgerV2EpochUnrecoverable(ctx, cfg.DeploymentKey(), cfg.NoId, epoch, declaredBy)
}
