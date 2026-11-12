package controller

// st_ctf_driver.go — the capture the flag driver (sn CAPTURETHEFLAG.md §3.3,
// §4.3 to §4.7, §5, §11.2) beside the provider pool's v2 driver, whose order
// and rules it follows for the CTF pool P = 2^32 + n. Each sync turn plans
// steps from the epoch clock and the event mirror; each step re-reads the
// chain at the finalized head before it writes, so steps are idempotent and
// safe to retry or to repeat after a restart.
//
// Per epoch k, from E_c (the signed amendment entry, st_ctf_amendment.go) and
// the pool's first epoch:
//
//  1. census: once the census of k is built, commitCtfCensus(k, n, hash)
//     while the block after the latest is inside [end(k), end(k) + w/2], w the
//     epoch's commit window. The candidates are then fixed on chain before the
//     drand round that seeds the draw exists. A late or missed commitment is
//     alerted; a missed one means no draw for k, and everything carries.
//  2. epoch: close P inside [end(k), end(k) + w], as the provider pool is
//     closed; finalize the previous CTF root if it can be; wait until the census
//     window has an answer; with a census, wait for ρ_k, the first quicknet round
//     at least 60 s after the census block, and verify it; build with ctf.Build;
//     publish the objects, then the header; commit the root with the root signer.
//  3. finalize, answer and record, as for the provider pool.
//  4. recycle: once a root is final, the carry contract's recycle returns the
//     carry account's α to the pool hotkey, where the next close captures it.
//
// Shadow mode builds and publishes an unsigned report instead, and sends
// nothing (st_ctf_shadow.go).

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/urfoundation/sn/amendment"
	"github.com/urfoundation/sn/ctf"
	"github.com/urfoundation/sn/ledger"
	"github.com/urfoundation/sn/merkle"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server/model"
)

// The capture the flag driver's steps, for the task layer.
const (
	StCtfDriverStepCensus   = "ctf_commit_census"
	StCtfDriverStepEpoch    = "ctf_epoch"
	StCtfDriverStepFinalize = "ctf_finalize"
	StCtfDriverStepAnswer   = "ctf_answer"
	StCtfDriverStepRecord   = "ctf_record"
	StCtfDriverStepRecycle  = "ctf_recycle"
	StCtfDriverStepShadow   = "ctf_shadow"
)

// The capture the flag alerts. Each fires once per process and subject.
// StCtfAlertCensusUnavailable is a committed census whose objects the store
// has lost: every root covering its epoch stalls until they are published.
const (
	StCtfAlertConfig             = "ctf_config"
	StCtfAlertAmendment          = "ctf_amendment"
	StCtfAlertCensusLate         = "ctf_census_late"
	StCtfAlertCensusMissed       = "ctf_census_missed"
	StCtfAlertCensusMismatch     = "ctf_census_mismatch"
	StCtfAlertCensusInvalid      = "ctf_census_invalid"
	StCtfAlertCensusUnavailable  = "ctf_census_unavailable"
	StCtfAlertMissedClose        = "ctf_missed_close"
	StCtfAlertMissedCommitWindow = "ctf_missed_commit_window"
	StCtfAlertOpenDemand         = "ctf_open_demand"
	StCtfAlertUnfinalizedRoot    = "ctf_root_not_finalized"
	StCtfAlertCarryLate          = "ctf_carry_late"
	StCtfAlertPotCapture         = "ctf_pot_capture"
	StCtfAlertDrand              = "ctf_drand"
	StCtfAlertBuild              = "ctf_build"
	StCtfAlertCarry              = "ctf_carry"
	StCtfAlertMirror             = "ctf_mirror"
)

const (
	// stCtfPollDelay spaces the waits for a census, a pulse or a build input.
	stCtfPollDelay = 1 * time.Minute
	// stCtfCarryLateBlocks is how long a final root may go without its
	// carry returned before StCtfAlertCarryLate: about two hours.
	stCtfCarryLateBlocks = 600
)

// StCtfDriverStep is one planned driver step. An answer names its demand: the
// pending root's seq, the finalized root RootIndex and the account Index; a
// recycle names the final root's RootIndex.
type StCtfDriverStep struct {
	Kind      string
	Epoch     uint64
	Seq       uint64
	RootIndex uint64
	Index     uint64
	RunAt     time.Time
}

var errStCtfNotConfigured = errors.New("st ctf: capture the flag is off")

func stCtfRequire() (*StConfig, StLedgerV2Client, StCtfClient, error) {
	cfg, client, err := stLedgerV2Require()
	if err != nil {
		return nil, nil, nil, errStCtfNotConfigured
	}
	if !cfg.Ctf.On() {
		return nil, nil, nil, errStCtfNotConfigured
	}
	ctfClient := stCtfClient()
	if ctfClient == nil {
		return nil, nil, nil, errStCtfNotConfigured
	}
	return cfg, client, ctfClient, nil
}

// stCtfAdmission gates every live step as it gates every send (SV-9 and the
// mainnet admission); tests replace it.
var stCtfAdmission = stPayoutAdmission

// stCtfAlert raises an alert once per process and subject.
func stCtfAlert(cfg *StConfig, name string, subject string, format string, args ...any) {
	if _, raised := stLedgerV2Alerted.LoadOrStore(fmt.Sprintf("%s|%d|%s|%s", cfg.DeploymentKey(), cfg.NoId, name, subject), true); raised {
		return
	}
	stCtfAlerts.WithLabelValues(name).Inc()
	stLedgerV2AlertSink(name, subject, fmt.Sprintf(format, args...))
}

type stCtfPoolRegistration struct {
	registered bool
	firstEpoch uint64
	checked    time.Time
}

var stCtfPools sync.Map

// stCtfPool reads the CTF pool's registration from vault2.pools(P): a
// registered pool is cached for good, an unregistered answer read again after
// a minute.
func stCtfPool(ctx context.Context, cfg *StConfig, client StLedgerV2Client) (uint64, bool, error) {
	key := fmt.Sprintf("%s|%s|%d", cfg.DeploymentKey(), cfg.VaultV2.Hex(), ctf.PoolID(cfg.NoId))
	if value, ok := stCtfPools.Load(key); ok {
		pool := value.(stCtfPoolRegistration)
		if pool.registered || stCtfServerNow().Sub(pool.checked) < time.Minute {
			return pool.firstEpoch, pool.registered, nil
		}
	}
	registered, firstEpoch, err := client.LedgerV2Pool(ctx, ctf.PoolID(cfg.NoId))
	if err != nil {
		return 0, false, err
	}
	stCtfPools.Store(key, stCtfPoolRegistration{registered: registered, firstEpoch: firstEpoch, checked: stCtfServerNow()})
	return firstEpoch, registered, nil
}

var stCtfCarries sync.Map

// stCtfCheckCarry requires the carry contract to be the CTF pool's, on this
// deployment's vault2, with the coldkey every EVM contract has, before the
// driver trusts its records or sends to it, and returns what it read. A
// carry that checks is cached: it is immutable.
func stCtfCheckCarry(ctx context.Context, cfg *StConfig, ctfClient StCtfClient, carry common.Address) (*StCtfCarry, error) {
	key := fmt.Sprintf("%s|%s", cfg.DeploymentKey(), carry.Hex())
	if value, ok := stCtfCarries.Load(key); ok {
		return value.(*StCtfCarry), nil
	}
	read, err := ctfClient.CtfCarry(ctx, carry)
	if err != nil {
		return nil, err
	}
	if read.Vault != cfg.VaultV2 || read.PoolId != ctf.PoolID(cfg.NoId) || read.Coldkey != stCtfCarryColdkey(carry) {
		err := fmt.Errorf("st ctf: carry %s is for vault %s pool %d with coldkey 0x%x, not this deployment's pool %d", carry.Hex(), read.Vault.Hex(), read.PoolId, read.Coldkey, ctf.PoolID(cfg.NoId))
		stCtfAlert(cfg, StCtfAlertCarry, carry.Hex(), "%s", err)
		return nil, err
	}
	stCtfCarries.Store(key, read)
	return read, nil
}

// ---------------------------------------------------------------------------
// Sync turn
// ---------------------------------------------------------------------------

// StCtfDriverSync plans one sync turn's steps from the epoch clock and the mirror,
// and raises the alerts the mirror shows. Chain reads are limited to the CTF
// pool's registration and epoch windows, both cached.
func StCtfDriverSync(ctx context.Context, state *StEpochState) ([]*StCtfDriverStep, error) {
	cfg, client, _, err := stCtfRequire()
	if errors.Is(err, errStCtfNotConfigured) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	records := stCtfRecordsFor(cfg)
	now := stCtfServerNow()
	runAt := func(block uint64) time.Time {
		at := stEstimateBlockTime(state.HeadBlock, state.HeadBlockTime, block, cfg.BlockSeconds)
		if at.Before(now) {
			return now
		}
		return at
	}
	if cfg.Ctf.Mode == StCtfModeShadow {
		if state.Epoch == 0 {
			return nil, nil
		}
		epoch := state.Epoch - 1
		if status, _ := records.Outcome(ctx, epoch, model.StPublishKindCtfShadow); status != "" {
			return nil, nil
		}
		end, window, err := stLedgerV2Window(ctx, cfg, client, epoch)
		if err != nil {
			return nil, err
		}
		return []*StCtfDriverStep{{Kind: StCtfDriverStepShadow, Epoch: epoch, RunAt: runAt(stCtfShadowCensusBlock(end, window))}}, nil
	}

	firstEpoch, registered, err := stCtfPool(ctx, cfg, client)
	if err != nil || !registered {
		return nil, err
	}
	poolId, head := ctf.PoolID(cfg.NoId), state.HeadBlock
	mirrored := records.MirroredBlock(ctx)
	steps := []*StCtfDriverStep{}

	if firstEpoch < state.Epoch {
		epoch := state.Epoch - 1
		end, window, err := stLedgerV2Window(ctx, cfg, client, epoch)
		if err != nil {
			return nil, err
		}
		if head <= end+window {
			if status, _ := records.Outcome(ctx, epoch, model.StPublishKindCtfCensus); status == "" && head <= end+window/2 {
				steps = append(steps, &StCtfDriverStep{Kind: StCtfDriverStepCensus, Epoch: epoch, RunAt: runAt(end)})
			}
			if status, _ := records.Outcome(ctx, epoch, model.StPublishKindCtfCommit); status == "" {
				steps = append(steps, &StCtfDriverStep{Kind: StCtfDriverStepEpoch, Epoch: epoch, RunAt: runAt(end)})
			}
		} else if end+window < mirrored {
			// A commit step that ended skipped found the epoch outside capture
			// the flag, or its root committed: nothing was missed.
			status, _ := records.Outcome(ctx, epoch, model.StPublishKindCtfCommit)
			if funding := records.Funding(ctx, poolId, epoch); status != model.StPublishStatusSkipped && (funding == nil || funding.Deferred) {
				stCtfAlert(cfg, StCtfAlertMissedClose, fmt.Sprintf("epoch %d", epoch), "no close of capture the flag pool %d by block %d; the next close defers the epoch", poolId, end+window)
			}
			committed := status == model.StPublishStatusSkipped
			for _, root := range records.RootsFromEpoch(ctx, poolId, epoch) {
				committed = committed || root.Epoch == epoch
			}
			if !committed {
				stCtfAlert(cfg, StCtfAlertMissedCommitWindow, fmt.Sprintf("epoch %d", epoch), "no capture the flag root committed by block %d; the next root covers the epoch", end+window)
			}
		}
	}

	if pending := records.PendingRoot(ctx, poolId); pending != nil {
		if status, _ := records.Outcome(ctx, pending.Epoch, model.StPublishKindCtfFinalize); pending.CommitBlock != nil && status == "" {
			steps = append(steps, &StCtfDriverStep{Kind: StCtfDriverStepFinalize, Epoch: pending.Epoch, RunAt: runAt(*pending.CommitBlock + stLedgerV2ChallengeBlocks)})
		}
		oneDay := uint64(24*time.Hour/time.Second) / uint64(stLedgerV2BlockSeconds(cfg))
		for _, demand := range records.OpenDemands(ctx, poolId, pending.Seq) {
			steps = append(steps, &StCtfDriverStep{Kind: StCtfDriverStepAnswer, Epoch: pending.Epoch, Seq: demand.Seq, RootIndex: demand.RootIndex, Index: demand.Index, RunAt: now})
			if demand.DemandBlock+oneDay < head {
				stCtfAlert(cfg, StCtfAlertOpenDemand, fmt.Sprintf("seq %d root %d index %d", demand.Seq, demand.RootIndex, demand.Index), "demanded at block %d and still open at block %d", demand.DemandBlock, head)
			}
		}
	}

	if 2 <= state.Epoch {
		for _, root := range records.RootsFromEpoch(ctx, poolId, state.Epoch-2) {
			// The ledgers the API reads follow their roots on chain.
			if update := stCtfLedgerStatus(root); update != nil {
				records.SetLedgerStatus(ctx, root.Epoch, root.LedgerHash, update)
			}
			if root.Status == model.StLedgerV2RootFinalized {
				continue
			}
			end, window, err := stLedgerV2Window(ctx, cfg, client, root.Epoch)
			if err != nil {
				return nil, err
			}
			if deadline := end + stLedgerV2ChallengeBlocks + window; deadline < head {
				stCtfAlert(cfg, StCtfAlertUnfinalizedRoot, fmt.Sprintf("seq %d", root.Seq), "the capture the flag root of epoch %d is %s at block %d, past block %d", root.Epoch, root.Status, head, deadline)
			}
		}
	}

	if root := records.NextUnrecordedRoot(ctx, poolId); root != nil {
		steps = append(steps, &StCtfDriverStep{Kind: StCtfDriverStepRecord, Epoch: root.Epoch, Seq: root.Seq, RunAt: now})
	}

	// The carry of the latest final root goes back before the next close, or
	// that close's epoch is built unmatured (§4.7).
	if latest := records.LatestFinalizedRoot(ctx, poolId); latest != nil && latest.RootIndex != nil {
		status, _ := records.Outcome(ctx, latest.Epoch, model.StPublishKindCtfRecycle)
		if status == "" {
			steps = append(steps, &StCtfDriverStep{Kind: StCtfDriverStepRecycle, Epoch: latest.Epoch, RootIndex: *latest.RootIndex, RunAt: now})
		}
		if status == "" && latest.StatusBlock != nil && *latest.StatusBlock+stCtfCarryLateBlocks < head {
			stCtfAlert(cfg, StCtfAlertCarryLate, fmt.Sprintf("root %d", *latest.RootIndex), "root %d of epoch %d is final since block %d and its carry is not returned at block %d", *latest.RootIndex, latest.Epoch, *latest.StatusBlock, head)
		}
	}
	return steps, nil
}

// ---------------------------------------------------------------------------
// Steps
// ---------------------------------------------------------------------------

// StCtfDriverRunStep runs one planned step. Each step re-reads the chain at the
// finalized head, so a repeated or late step only does what is still due. A
// step of the mode not configured does nothing.
func StCtfDriverRunStep(ctx context.Context, step *StCtfDriverStep) (*StPublishOutcome, error) {
	cfg, client, ctfClient, err := stCtfRequire()
	if err != nil {
		return nil, err
	}
	records := stCtfRecordsFor(cfg)
	outcome, err := stCtfDriverRunStep(ctx, cfg, client, ctfClient, records, step)
	status := "error"
	if outcome != nil {
		status = outcome.Status
	}
	stCtfSteps.WithLabelValues(step.Kind, status).Inc()
	return outcome, err
}

func stCtfDriverRunStep(ctx context.Context, cfg *StConfig, client StLedgerV2Client, ctfClient StCtfClient, records stCtfRecords, step *StCtfDriverStep) (*StPublishOutcome, error) {
	if step.Kind == StCtfDriverStepShadow {
		if cfg.Ctf.Mode != StCtfModeShadow {
			return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: "capture the flag is not in shadow mode"}, nil
		}
		// The shadow run reaches the chain only through views.
		return stCtfRunShadow(ctx, cfg, stCtfViews{client: client, ctf: ctfClient}, records, step.Epoch)
	}
	if cfg.Ctf.Mode != StCtfModeLive {
		return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: "capture the flag is not live"}, nil
	}
	if err := stCtfAdmission(ctx, cfg); err != nil {
		return nil, err
	}
	switch step.Kind {
	case StCtfDriverStepCensus:
		return stCtfRunCensus(ctx, cfg, client, ctfClient, records, step.Epoch)
	case StCtfDriverStepEpoch:
		return stCtfRunEpoch(ctx, cfg, client, ctfClient, records, step.Epoch)
	case StCtfDriverStepFinalize:
		return stCtfRunFinalize(ctx, cfg, client, records, step.Epoch)
	case StCtfDriverStepAnswer:
		return stCtfRunAnswer(ctx, cfg, client, records, step.Seq, step.RootIndex, step.Index)
	case StCtfDriverStepRecord:
		return stCtfRunRecord(ctx, cfg, records, step.Seq)
	case StCtfDriverStepRecycle:
		return stCtfRunRecycle(ctx, cfg, client, ctfClient, records, step.RootIndex)
	}
	return nil, fmt.Errorf("st ctf: unknown step %q", step.Kind)
}

// Records a step outcome that ends the step for its epoch.
func stCtfConclude(ctx context.Context, records stCtfRecords, epoch uint64, kind string, status string, format string, args ...any) *StPublishOutcome {
	outcome := &StPublishOutcome{Status: status, Reason: fmt.Sprintf(format, args...)}
	records.Resolve(ctx, records.Publish(ctx, epoch, kind), outcome)
	return outcome
}

// A wait until a time.
func stCtfWaitUntil(reason string, at time.Time) *StPublishOutcome {
	if now := stCtfServerNow(); at.Before(now.Add(time.Second)) {
		at = now.Add(stCtfPollDelay)
	}
	return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: reason, Retry: true, RetryAt: at}
}

// The selectors of the reverts that show a CTF write already done by another
// sender or another attempt: a census already committed, or nothing left on
// the carry to recycle.
var (
	stCtfCensusAlreadyCommitted = [4]byte(crypto.Keccak256([]byte("CensusAlreadyCommitted()"))[:4])
	stCtfNothingToRecycle       = [4]byte(crypto.Keccak256([]byte("NothingToRecycle()"))[:4])
)

// Records one chain write as a publish row, as stLedgerV2Send does. A revert
// that shows the write already done ends it as skipped.
func stCtfSend(ctx context.Context, records stCtfRecords, epoch uint64, kind string, send func() (string, error)) *StPublishOutcome {
	publishId := records.Publish(ctx, epoch, kind)
	txHash, err := send()
	outcome := &StPublishOutcome{Status: model.StPublishStatusConfirmed, TxHash: txHash}
	selector, reverted := stLedgerV2RevertSelector(err)
	switch {
	case errors.Is(err, ErrStLedgerV2CommitSpent):
		outcome = &StPublishOutcome{Status: model.StPublishStatusMissed, TxHash: txHash, Reason: err.Error()}
	case reverted && selector == stCtfCensusAlreadyCommitted:
		outcome = &StPublishOutcome{Status: model.StPublishStatusSkipped, TxHash: txHash, Reason: "already done: CensusAlreadyCommitted"}
	case reverted && selector == stCtfNothingToRecycle:
		outcome = &StPublishOutcome{Status: model.StPublishStatusSkipped, TxHash: txHash, Reason: "already done: NothingToRecycle"}
	default:
		if name, done := stLedgerV2AlreadyDone(err); done {
			outcome = &StPublishOutcome{Status: model.StPublishStatusSkipped, TxHash: txHash, Reason: "already done: " + name}
		} else if err != nil {
			outcome = &StPublishOutcome{Status: model.StPublishStatusFailed, TxHash: txHash, Reason: err.Error(), Retry: true}
		}
	}
	records.Resolve(ctx, publishId, outcome)
	return outcome
}

// stCtfTerms reads the entry in force for an epoch. Unknown terms are a wait,
// never "not in force".
func stCtfTerms(ctx context.Context, cfg *StConfig, state *StLedgerV2State, epoch uint64) (*amendment.Entry, *StPublishOutcome) {
	entry, err := stCtfInForce(ctx, cfg, epoch)
	if err != nil {
		return nil, stLedgerV2Wait(cfg, state, fmt.Sprintf("capture the flag's terms for epoch %d are unknown: %s", epoch, err), 0)
	}
	return entry, nil
}

// stCtfRunCensus commits epoch k's census inside its window (§4.3).
func stCtfRunCensus(ctx context.Context, cfg *StConfig, client StLedgerV2Client, ctfClient StCtfClient, records stCtfRecords, epoch uint64) (*StPublishOutcome, error) {
	kind, poolId := model.StPublishKindCtfCensus, ctf.PoolID(cfg.NoId)
	state, err := client.LedgerV2State(ctx, poolId)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	if !state.Registered || epoch < state.FirstEpoch {
		return stCtfConclude(ctx, records, epoch, kind, model.StPublishStatusSkipped, "epoch %d is not an epoch of capture the flag pool %d", epoch, poolId), nil
	}
	entry, wait := stCtfTerms(ctx, cfg, state, epoch)
	if wait != nil {
		return wait, nil
	}
	if entry == nil {
		return stCtfConclude(ctx, records, epoch, kind, model.StPublishStatusSkipped, "capture the flag is not in force for epoch %d", epoch), nil
	}
	end, window, err := stLedgerV2Window(ctx, cfg, client, epoch)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	if state.HeadBlock < end {
		return stLedgerV2Wait(cfg, state, fmt.Sprintf("epoch %d ends at block %d", epoch, end), end), nil
	}
	deadline := end + window/2
	census, err := stCtfCensusOf(ctx, epoch)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	hash, block, err := ctfClient.CtfCensus(ctx, cfg.NoId, epoch)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	if hash != ([32]byte{}) {
		if census != nil && census.Hash != hash {
			stCtfAlert(cfg, StCtfAlertCensusMismatch, fmt.Sprintf("epoch %d", epoch), "the chain holds census %v committed at block %d, not the census built, %v", ledger.Ref(hash), block, census.Hash)
		}
		// Every committed census object stays published: a lost one stalls
		// the epoch's roots.
		if census != nil && census.Hash == hash {
			if err := stCtfRepublishCensus(ctx, census); err != nil {
				return stLedgerV2Retry(err), nil
			}
		}
		if err := stCtfRecordCommitment(ctx, ctfClient, records, epoch, hash, block, ""); err != nil {
			return stLedgerV2Retry(err), nil
		}
		stCtfLastEpoch.WithLabelValues("census").Set(float64(epoch))
		return stCtfConclude(ctx, records, epoch, kind, model.StPublishStatusSkipped, "census %v is committed at block %d", ledger.Ref(hash), block), nil
	}
	if deadline < state.HeadBlock {
		stCtfAlert(cfg, StCtfAlertCensusMissed, fmt.Sprintf("epoch %d", epoch), "no census committed by block %d: epoch %d has no draw and everything carries", deadline, epoch)
		if census != nil {
			records.SetCommitment(ctx, &model.CtfCensusCommitment{Epoch: epoch, CensusHash: census.Hash, Status: model.CtfCommitmentStatusMissed})
		}
		return stCtfConclude(ctx, records, epoch, kind, model.StPublishStatusMissed, "the census window closed at block %d", deadline), nil
	}
	if late := end + window/4; late < state.HeadBlock {
		stCtfAlert(cfg, StCtfAlertCensusLate, fmt.Sprintf("epoch %d", epoch), "no census committed at block %d, a quarter of the way into its window, which closes at block %d", state.HeadBlock, deadline)
	}
	if census == nil {
		return stLedgerV2Wait(cfg, state, fmt.Sprintf("the census of epoch %d is not built yet", epoch), 0), nil
	}
	// Only a census whose every object the census part has published is
	// committed; publishing again first costs nothing and makes sure.
	if !census.Published {
		return stLedgerV2Wait(cfg, state, fmt.Sprintf("the census of epoch %d is not published yet", epoch), 0), nil
	}
	if state.Paused {
		return stLedgerV2Wait(cfg, state, "settlement is paused", 0), nil
	}
	if err := stCtfRepublishCensus(ctx, census); err != nil {
		return stLedgerV2Retry(err), nil
	}
	if outcome := stLedgerV2LateSend(ctx, cfg, client, state, "census", deadline); outcome != nil {
		return outcome, nil
	}
	records.SetCommitment(ctx, &model.CtfCensusCommitment{Epoch: epoch, CensusHash: census.Hash, Status: model.CtfCommitmentStatusPending})
	outcome := stCtfSend(ctx, records, epoch, kind, func() (string, error) {
		return ctfClient.CtfCommitCensus(ctx, epoch, cfg.NoId, census.Hash)
	})
	if outcome.Retry {
		records.SetCommitment(ctx, &model.CtfCensusCommitment{Epoch: epoch, CensusHash: census.Hash, Status: model.CtfCommitmentStatusSent, TxHash: outcome.TxHash})
		return outcome, nil
	}
	if hash, block, err = ctfClient.CtfCensus(ctx, cfg.NoId, epoch); err != nil {
		return stLedgerV2Retry(err), nil
	}
	if hash != census.Hash {
		records.SetCommitment(ctx, &model.CtfCensusCommitment{Epoch: epoch, CensusHash: census.Hash, Status: model.CtfCommitmentStatusSent, TxHash: outcome.TxHash})
		return stLedgerV2Retry(fmt.Errorf("the census commitment of epoch %d is not visible at the finalized head", epoch)), nil
	}
	if err := stCtfRecordCommitment(ctx, ctfClient, records, epoch, hash, block, outcome.TxHash); err != nil {
		return stLedgerV2Retry(err), nil
	}
	stCtfLastEpoch.WithLabelValues("census").Set(float64(epoch))
	return outcome, nil
}

// stCtfRecordCommitment records a census commitment seen at the finalized
// head, with its block's EVM timestamp and the draw round ρ_k it fixes.
func stCtfRecordCommitment(ctx context.Context, ctfClient StCtfClient, records stCtfRecords, epoch uint64, hash [32]byte, block uint64, txHash string) error {
	blockTime, err := ctfClient.CtfBlockTime(ctx, block)
	if err != nil {
		return err
	}
	round := ctf.DrawRound(blockTime)
	records.SetCommitment(ctx, &model.CtfCensusCommitment{Epoch: epoch, CensusHash: hash, Status: model.CtfCommitmentStatusCommitted, TxHash: txHash,
		CommitBlock: &block, CommitBlockTime: &blockTime, Round: &round})
	return nil
}

// stCtfRunEpoch drives epoch k of the CTF pool through the provider pool's
// order: close, finalize the previous root, then draw, build, publish and
// commit once the census and the pulse are fixed.
func stCtfRunEpoch(ctx context.Context, cfg *StConfig, client StLedgerV2Client, ctfClient StCtfClient, records stCtfRecords, epoch uint64) (*StPublishOutcome, error) {
	kind, poolId := model.StPublishKindCtfCommit, ctf.PoolID(cfg.NoId)
	state, err := client.LedgerV2State(ctx, poolId)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	if !state.Registered || epoch < state.FirstEpoch {
		return stCtfConclude(ctx, records, epoch, kind, model.StPublishStatusSkipped, "epoch %d is not an epoch of capture the flag pool %d", epoch, poolId), nil
	}
	entry, wait := stCtfTerms(ctx, cfg, state, epoch)
	if wait != nil {
		return wait, nil
	}
	if entry == nil {
		return stCtfConclude(ctx, records, epoch, kind, model.StPublishStatusSkipped, "capture the flag is not in force for epoch %d", epoch), nil
	}
	end, window, err := stLedgerV2Window(ctx, cfg, client, epoch)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	if state.HeadBlock < end {
		return stLedgerV2Wait(cfg, state, fmt.Sprintf("epoch %d ends at block %d", epoch, end), end), nil
	}

	// 1. Close inside the window, as the provider pool is closed. A missed
	// close defers the epoch at the next close.
	if state.LastFundedEpoch < epoch {
		if end+window < state.HeadBlock {
			stCtfAlert(cfg, StCtfAlertMissedClose, fmt.Sprintf("epoch %d", epoch), "no close of capture the flag pool %d by block %d; the next close defers the epoch", poolId, end+window)
			return stCtfConclude(ctx, records, epoch, kind, model.StPublishStatusMissed, "close window closed at block %d", end+window), nil
		}
		if state.Paused {
			return stLedgerV2Wait(cfg, state, "settlement is paused", 0), nil
		}
		if outcome := stLedgerV2LateSend(ctx, cfg, client, state, "close", end+window); outcome != nil {
			return outcome, nil
		}
		if outcome := stCtfSend(ctx, records, epoch, model.StPublishKindCtfClose, func() (string, error) {
			return client.LedgerV2Close(ctx, epoch, poolId)
		}); outcome.Retry {
			return outcome, nil
		}
		if state, err = client.LedgerV2State(ctx, poolId); err != nil {
			return stLedgerV2Retry(err), nil
		}
		if state.LastFundedEpoch < epoch {
			return stLedgerV2Retry(fmt.Errorf("the close of capture the flag epoch %d is not visible at block %d", epoch, state.HeadBlock)), nil
		}
	}
	if (state.Pending != nil && epoch <= state.Pending.Epoch) || (state.Latest != nil && epoch <= state.Latest.Epoch) {
		return stCtfConclude(ctx, records, epoch, kind, model.StPublishStatusSkipped, "a capture the flag root for epoch %d is already committed", epoch), nil
	}

	// 2. Finalize the previous root if it can be finalized. Otherwise the new
	// root is built on the latest finalized root and replaces it.
	if stLedgerV2Finalizable(state) {
		pendingEpoch := state.Pending.Epoch
		if outcome := stCtfSend(ctx, records, pendingEpoch, model.StPublishKindCtfFinalize, func() (string, error) {
			return client.LedgerV2Finalize(ctx, pendingEpoch, poolId)
		}); outcome.Retry {
			return outcome, nil
		}
		if state, err = client.LedgerV2State(ctx, poolId); err != nil {
			return stLedgerV2Retry(err), nil
		}
	}
	return stCtfCommitEpoch(ctx, cfg, client, ctfClient, records, state, entry, epoch, end, window)
}

// Steps 3 to 6 for epoch k: wait for the census and the pulse, build,
// publish, commit.
func stCtfCommitEpoch(ctx context.Context, cfg *StConfig, client StLedgerV2Client, ctfClient StCtfClient, records stCtfRecords, state *StLedgerV2State, entry *amendment.Entry, epoch uint64, end uint64, window uint64) (*StPublishOutcome, error) {
	kind, poolId := model.StPublishKindCtfCommit, ctf.PoolID(cfg.NoId)
	deadline := end + window
	if deadline < state.HeadBlock {
		return stCtfMissedCommit(ctx, cfg, records, epoch, deadline), nil
	}

	// 3. The census of k is fixed once it is committed, or once its window has
	// passed without one, which gives no draw. With a census, the draw waits
	// for ρ_k.
	hash, block, err := ctfClient.CtfCensus(ctx, cfg.NoId, epoch)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	if censusDeadline := end + window/2; hash == ([32]byte{}) {
		if state.HeadBlock <= censusDeadline {
			return stLedgerV2Wait(cfg, state, fmt.Sprintf("the census of epoch %d may be committed until block %d", epoch, censusDeadline), censusDeadline+1), nil
		}
		stCtfAlert(cfg, StCtfAlertCensusMissed, fmt.Sprintf("epoch %d", epoch), "no census committed by block %d: epoch %d has no draw and everything carries", censusDeadline, epoch)
	} else {
		blockTime, err := ctfClient.CtfBlockTime(ctx, block)
		if err != nil {
			return stLedgerV2Retry(err), nil
		}
		if err := stCtfRecordCommitment(ctx, ctfClient, records, epoch, hash, block, ""); err != nil {
			return stLedgerV2Retry(err), nil
		}
		round := ctf.DrawRound(blockTime)
		if at := stCtfRoundTime(round); stCtfServerNow().Before(at) {
			return stCtfWaitUntil(fmt.Sprintf("drand round %d of epoch %d's draw is published at %s", round, epoch, at.Format(time.RFC3339)), at.Add(time.Second)), nil
		}
	}

	// 4. The latest finalized root, the root count and the funding.
	previous, rootCount := state.Latest, state.RootCount
	funded, err := client.LedgerV2FundedThrough(ctx, poolId, epoch)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	var finalizedTotal uint256.Int
	if previous != nil {
		finalizedTotal = previous.Total
	}
	if !funded.Gt(&finalizedTotal) {
		return stCtfConclude(ctx, records, epoch, kind, model.StPublishStatusSkipped, "the total %s does not exceed the finalized total %s", funded.Dec(), finalizedTotal.Dec()), nil
	}
	carry := entry.Ctf.Carry
	if _, err := stCtfCheckCarry(ctx, cfg, ctfClient, carry); err != nil {
		return stLedgerV2Retry(err), nil
	}

	// 5. Build.
	in, err := stCtfBuildInput(ctx, cfg, client, ctfClient, records, state, carry, epoch)
	if err != nil {
		return stCtfInputWait(cfg, state, epoch, err), nil
	}
	built, err := ctf.Build(ctx, *in)
	if errors.Is(err, ledger.ErrTotalNotIncreased) {
		return stCtfConclude(ctx, records, epoch, kind, model.StPublishStatusSkipped, "%s", err), nil
	}
	if errors.Is(err, ctf.ErrCensusUnavailable) {
		stCtfAlert(cfg, StCtfAlertCensusUnavailable, fmt.Sprintf("epoch %d", epoch), "a committed census object is not in the store, so no capture the flag root through epoch %d can be built until it is published: %s", epoch, err)
		return stLedgerV2Retry(err), nil
	}
	if err != nil {
		stCtfAlert(cfg, StCtfAlertBuild, fmt.Sprintf("epoch %d", epoch), "the capture the flag root of epoch %d cannot be built: %s", epoch, err)
		return stLedgerV2Retry(err), nil
	}
	if !built.Commit.Total.Eq(&funded) || (previous != nil && built.Commit.LeafCount < previous.LeafCount) {
		return stLedgerV2Retry(fmt.Errorf("st ctf: the build's total %s and %d accounts do not extend fundedThrough %s", built.Commit.Total.Dec(), built.Commit.LeafCount, funded.Dec())), nil
	}

	// 6. Publish the objects, then the header.
	if err := stCtfPublish(ctx, cfg, records, in, built); err != nil {
		return stLedgerV2Retry(err), nil
	}
	stCtfCacheTree(built.Commit.LedgerHash, &stLedgerV2Tree{tree: built.Tree, accounts: built.Accounts})
	stCtfReport(cfg, built, "")

	// 7. Commit with that root count.
	if state, err = client.LedgerV2State(ctx, poolId); err != nil {
		return stLedgerV2Retry(err), nil
	}
	if deadline < state.HeadBlock {
		return stCtfMissedCommit(ctx, cfg, records, epoch, deadline), nil
	}
	if state.RootCount != rootCount {
		return stLedgerV2Retry(fmt.Errorf("the capture the flag root count moved from %d to %d during the build", rootCount, state.RootCount)), nil
	}
	if state.Paused {
		return stLedgerV2Wait(cfg, state, "settlement is paused", 0), nil
	}
	if next := state.LastCommitBlock + state.ChallengeBlocks; state.LastCommitBlock != 0 && state.HeadBlock < next {
		return stLedgerV2Wait(cfg, state, fmt.Sprintf("vault2 accepts the capture the flag pool's next commit at block %d", next), next), nil
	}
	if outcome := stLedgerV2LateSend(ctx, cfg, client, state, "commit", deadline); outcome != nil {
		return outcome, nil
	}
	commitment := &StLedgerV2Commitment{Root: built.Commit.Root, Total: built.Commit.Total, LeafCount: built.Commit.LeafCount, LedgerHash: built.Commit.LedgerHash}
	outcome := stCtfSend(ctx, records, epoch, kind, func() (string, error) {
		return client.LedgerV2Commit(ctx, epoch, poolId, rootCount, commitment)
	})
	if outcome.Status == model.StPublishStatusConfirmed {
		records.SetLedgerStatus(ctx, epoch, built.Commit.LedgerHash, &model.CtfLedgerStatusUpdate{Status: model.CtfLedgerStatusCommitted})
		stCtfLastEpoch.WithLabelValues("commit").Set(float64(epoch))
	}
	return outcome, nil
}

// stCtfInputWait turns a build input that cannot be read yet into a wait: a
// pulse whose round is not published, or a relay outage, which is alerted
// once its round is stCtfDrandAlertAfter old. Inputs that retrying will not
// bring are alerted at once: a committed census without its recorded
// coldkeys, or a previous root whose published objects do not rebuild it.
func stCtfInputWait(cfg *StConfig, state *StLedgerV2State, epoch uint64, err error) *StPublishOutcome {
	if errors.Is(err, errStCtfPulseNotYet) {
		return stLedgerV2Wait(cfg, state, err.Error(), 0)
	}
	stCtfDrandAlert(cfg, err)
	switch {
	case errors.Is(err, errStCtfColdkeysUnknown), errors.Is(err, ctf.ErrCensusUnavailable):
		stCtfAlert(cfg, StCtfAlertCensusUnavailable, fmt.Sprintf("epoch %d", epoch), "the capture the flag root of epoch %d cannot be built: %s", epoch, err)
	case errors.Is(err, ledger.ErrPrevious):
		stCtfAlert(cfg, StCtfAlertBuild, fmt.Sprintf("epoch %d", epoch), "the capture the flag root of epoch %d cannot be built on the latest final root: %s", epoch, err)
	}
	glog.Infof("[st]ctf epoch %d build input: %s\n", epoch, err)
	return &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: err.Error(), Retry: true, RetryAt: stCtfServerNow().Add(stCtfPollDelay)}
}

func stCtfMissedCommit(ctx context.Context, cfg *StConfig, records stCtfRecords, epoch uint64, deadline uint64) *StPublishOutcome {
	stCtfAlert(cfg, StCtfAlertMissedCommitWindow, fmt.Sprintf("epoch %d", epoch), "no capture the flag root committed by block %d; the next root covers the epoch", deadline)
	return stCtfConclude(ctx, records, epoch, model.StPublishKindCtfCommit, model.StPublishStatusMissed, "commit window closed at block %d", deadline)
}

// stCtfReport alerts what a build shows and sets the build metrics: a
// committed census that is not canonical, an epoch built while the carry is
// still out, and every pot share at or above the configured size. prefix
// names a shadow build in the alerts.
func stCtfReport(cfg *StConfig, built *ctf.Ledger, prefix string) {
	threshold := uint256.NewInt(cfg.Ctf.PotCaptureAlertRao)
	for _, e := range built.Epochs {
		if e.CensusErr != nil {
			stCtfAlert(cfg, StCtfAlertCensusInvalid, fmt.Sprintf("%sepoch %d", prefix, e.Epoch), "the committed census %v is not canonical, so epoch %d has no draw: %s", e.Census, e.Epoch, e.CensusErr)
		}
		if !e.Matured {
			stCtfAlert(cfg, StCtfAlertCarryLate, fmt.Sprintf("%sepoch %d", prefix, e.Epoch), "epoch %d is built before its carry is back: %s rao owed, %s returned; its pot shares are pending awards", e.Epoch, e.Owed.Dec(), e.Returned.Dec())
		}
		for _, award := range e.Awards {
			if award.Pot.Lt(threshold) {
				continue
			}
			stCtfAlert(cfg, StCtfAlertPotCapture, fmt.Sprintf("%sepoch %d %s %s rank %d", prefix, e.Epoch, award.Country, award.Role, award.Rank),
				"client %x (coldkey 0x%x, unit %x) takes a pot share of %s rao in %s as %s rank %d, pending %t", award.ClientID, award.Coldkey, award.Unit, award.Pot.Dec(), award.Country, award.Role, award.Rank, award.Deferred)
		}
	}
	if len(built.Epochs) == 0 {
		return
	}
	last := built.Epochs[len(built.Epochs)-1]
	paid := last.Paid()
	stCtfLastBuildRao.WithLabelValues("fresh").Set(stCtfRaoGauge(&last.Fresh))
	stCtfLastBuildRao.WithLabelValues("paid").Set(stCtfRaoGauge(&paid))
	stCtfLastBuildRao.WithLabelValues("pending").Set(stCtfRaoGauge(&last.PendingTotal))
	stCtfLastBuildRao.WithLabelValues("carried").Set(stCtfRaoGauge(&last.Carried))
	stCtfLastBuildWinners.Set(float64(len(last.Awards)))
}

// stCtfRunFinalize finalizes the CTF pool's pending root of an epoch once its
// window has passed with a quorum of approvals and no open demand.
func stCtfRunFinalize(ctx context.Context, cfg *StConfig, client StLedgerV2Client, records stCtfRecords, epoch uint64) (*StPublishOutcome, error) {
	kind, poolId := model.StPublishKindCtfFinalize, ctf.PoolID(cfg.NoId)
	state, err := client.LedgerV2State(ctx, poolId)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	pending := state.Pending
	if pending == nil || pending.Epoch != epoch {
		return stCtfConclude(ctx, records, epoch, kind, model.StPublishStatusSkipped, "no pending capture the flag root for epoch %d", epoch), nil
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
			return stCtfConclude(ctx, records, epoch, kind, model.StPublishStatusMissed, "approvals closed at block %d with %d of %d", end, state.Approvals, state.Quorum), nil
		}
		return stLedgerV2Wait(cfg, state, fmt.Sprintf("%d of %d approvals", state.Approvals, state.Quorum), 0), nil
	}
	if pending.OpenDemands != 0 {
		return stLedgerV2Wait(cfg, state, fmt.Sprintf("%d open demands", pending.OpenDemands), 0), nil
	}
	if state.Paused {
		return stLedgerV2Wait(cfg, state, "settlement is paused", 0), nil
	}
	return stCtfSend(ctx, records, epoch, kind, func() (string, error) {
		return client.LedgerV2Finalize(ctx, epoch, poolId)
	}), nil
}

// stCtfRunAnswer answers a demand on the CTF pool's pending root with a clean
// opening of the path at the demanded account of the finalized root
// rootIndex, as the provider pool's driver does.
func stCtfRunAnswer(ctx context.Context, cfg *StConfig, client StLedgerV2Client, records stCtfRecords, seq uint64, rootIndex uint64, index uint64) (*StPublishOutcome, error) {
	poolId := ctf.PoolID(cfg.NoId)
	state, err := client.LedgerV2State(ctx, poolId)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	pending := state.Pending
	if pending == nil || pending.Seq != seq || state.RootCount <= rootIndex {
		return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: fmt.Sprintf("seq %d is not pending over finalized root %d", seq, rootIndex)}, nil
	}
	demand, err := client.LedgerV2Demand(ctx, poolId, seq, rootIndex, index)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	if demand != StLedgerV2DemandOpen {
		return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: fmt.Sprintf("seq %d root %d index %d has no open demand", seq, rootIndex, index)}, nil
	}
	root := state.Latest
	if rootIndex != root.Index {
		if root, err = client.LedgerV2Root(ctx, poolId, rootIndex); err != nil {
			return stLedgerV2Retry(err), nil
		}
	}
	answer, err := stCtfOpening(ctx, root, pending, index)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	return stCtfSend(ctx, records, pending.Epoch, model.StPublishKindCtfAnswer, func() (string, error) {
		return client.LedgerV2Answer(ctx, poolId, seq, answer)
	}), nil
}

// stCtfOpening is the checkPath argument set for an index of a finalized CTF
// root. It refuses to send an opening that shows a fault.
func stCtfOpening(ctx context.Context, root *StLedgerV2FinalizedRoot, pending *StLedgerV2Pending, index uint64) (*StLedgerV2Answer, error) {
	if root.LeafCount <= index {
		return nil, fmt.Errorf("st ctf: index %d is outside the finalized root's %d accounts", index, root.LeafCount)
	}
	old, err := stCtfCachedTree(ctx, root.LedgerHash, root.Root, &root.Total, root.LeafCount)
	if err != nil {
		return nil, err
	}
	next, err := stCtfCachedTree(ctx, pending.LedgerHash, pending.Root, &pending.Total, pending.LeafCount)
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
		return nil, fmt.Errorf("st ctf: the pending root opens index %d with fault %s at level %d: %v", index, fault, level, err)
	}
	return &StLedgerV2Answer{RootIndex: root.Index, Index: index, Account: account, Proof: proof, Path: path}, nil
}

// stCtfRunRecord writes a finalized CTF root's new accounts and amounts for
// readers, after checking its published accounts rebuild it. A root's
// amounts are its coldkeys' deltas, recorded under the root's epoch.
func stCtfRunRecord(ctx context.Context, cfg *StConfig, records stCtfRecords, seq uint64) (*StPublishOutcome, error) {
	poolId := ctf.PoolID(cfg.NoId)
	root := records.Root(ctx, poolId, seq)
	if root == nil || root.RootIndex == nil || root.RecordedTime != nil {
		return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: fmt.Sprintf("seq %d is not an unrecorded finalized capture the flag root", seq)}, nil
	}
	var previousAccounts []merkle.LedgerAccount
	if 0 < *root.RootIndex {
		prior := records.FinalizedRoot(ctx, poolId, *root.RootIndex-1)
		if prior == nil {
			return stLedgerV2Retry(fmt.Errorf("st ctf: root %d is not mirrored", *root.RootIndex-1)), nil
		}
		previous, err := stCtfCachedTree(ctx, prior.LedgerHash, prior.Root, &prior.Total, prior.LeafCount)
		if err != nil {
			return stLedgerV2Retry(err), nil
		}
		previousAccounts = previous.accounts
	}
	tree, err := stCtfCachedTree(ctx, root.LedgerHash, root.Root, &root.Total, root.LeafCount)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	if len(tree.accounts) < len(previousAccounts) {
		return stLedgerV2Retry(fmt.Errorf("st ctf: root %d has fewer accounts than the one before it", *root.RootIndex)), nil
	}
	amounts := []*model.StLedgerV2Amount{}
	newColdkeys := [][32]byte{}
	for i, account := range tree.accounts {
		amount := account.Cumulative
		if i < len(previousAccounts) {
			if previousAccounts[i].Coldkey != account.Coldkey || account.Cumulative.Lt(&previousAccounts[i].Cumulative) {
				return stLedgerV2Retry(fmt.Errorf("st ctf: root %d changes account %d", *root.RootIndex, i)), nil
			}
			amount.Sub(&account.Cumulative, &previousAccounts[i].Cumulative)
		} else {
			newColdkeys = append(newColdkeys, account.Coldkey)
		}
		if !amount.IsZero() {
			amounts = append(amounts, &model.StLedgerV2Amount{NoId: poolId, Epoch: root.Epoch, Coldkey: account.Coldkey, Amount: amount, RootIndex: *root.RootIndex})
		}
	}
	if err := records.RecordRoot(ctx, poolId, seq, uint64(len(previousAccounts)), newColdkeys, amounts); err != nil {
		return stLedgerV2Retry(err), nil
	}
	records.SetLedgerStatus(ctx, root.Epoch, root.LedgerHash, stCtfLedgerStatus(root))
	return &StPublishOutcome{Status: model.StPublishStatusConfirmed, Reason: fmt.Sprintf("capture the flag root %d recorded", *root.RootIndex)}, nil
}

// stCtfRunRecycle returns the carry of the CTF pool's latest final root
// (§4.6): the carry contract claims the carry account from that root, unless
// someone has, and moves everything it holds back onto the pool hotkey. Only
// a final root is ever named, and only the latest, whose cumulative carry
// account covers every earlier one.
func stCtfRunRecycle(ctx context.Context, cfg *StConfig, client StLedgerV2Client, ctfClient StCtfClient, records stCtfRecords, rootIndex uint64) (*StPublishOutcome, error) {
	kind, poolId := model.StPublishKindCtfRecycle, ctf.PoolID(cfg.NoId)
	state, err := client.LedgerV2State(ctx, poolId)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	if state.RootCount <= rootIndex || state.Latest == nil {
		return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: fmt.Sprintf("capture the flag root %d is not final", rootIndex)}, nil
	}
	root := state.Latest
	if root.Index != rootIndex {
		return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: fmt.Sprintf("root %d is final after root %d; its recycle returns the carry", root.Index, rootIndex)}, nil
	}
	entry, wait := stCtfTerms(ctx, cfg, state, root.Epoch)
	if wait != nil {
		return wait, nil
	}
	if entry == nil {
		return stCtfConclude(ctx, records, root.Epoch, kind, model.StPublishStatusSkipped, "capture the flag is not in force for epoch %d", root.Epoch), nil
	}
	carry := entry.Ctf.Carry
	carryInfo, err := stCtfCheckCarry(ctx, cfg, ctfClient, carry)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	tree, err := stCtfCachedTree(ctx, root.LedgerHash, root.Root, &root.Total, root.LeafCount)
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	coldkey := stCtfCarryColdkey(carry)
	index := -1
	for i, account := range tree.accounts {
		if account.Coldkey == coldkey {
			index = i
			break
		}
	}
	if index < 0 {
		return stCtfConclude(ctx, records, root.Epoch, kind, model.StPublishStatusSkipped, "root %d has no carry account", rootIndex), nil
	}
	// The carry account is always named, with its proof, so the call under
	// the root's transaction key never changes: the carry claims it only while
	// vault2's claimed is below its cumulative, and otherwise moves only what
	// it holds.
	account := tree.accounts[index]
	proof, err := tree.tree.Proof(uint64(index))
	if err != nil {
		return stLedgerV2Retry(err), nil
	}
	if outcome := stCtfFreeStakingHotkey(ctx, cfg, ctfClient, records, root, carryInfo); outcome != nil {
		return outcome, nil
	}
	outcome := stCtfSend(ctx, records, root.Epoch, kind, func() (string, error) {
		return ctfClient.CtfRecycle(ctx, carry, poolId, rootIndex, uint64(index), &account, proof)
	})
	if outcome.Status == model.StPublishStatusConfirmed {
		stCtfLastEpoch.WithLabelValues("recycle").Set(float64(root.Epoch))
	}
	return outcome, nil
}

const (
	// stCtfStakingHotkeysMax is the runtime's limit on a coldkey's
	// StakingHotkeys list.
	stCtfStakingHotkeysMax = 128
	// stCtfAbsorbTries bounds the foreign entries one attempt tries to free.
	stCtfAbsorbTries = 8
)

// The (root, hotkey) pairs absorbed in this process: an absorb's transaction
// key names both, so a position refilled on the same hotkey is passed over
// for another rather than sent again.
var stCtfAbsorbed sync.Map

// stCtfFreeStakingHotkey clears the way for a recycle (sn-contracts' open
// issue 3). The carry's transfer to vault2 on the pool hotkey adds that hotkey
// to vault2's StakingHotkeys list whenever vault2 holds nothing on it, and the
// runtime refuses a list that would pass 128 entries (TooManyStakingHotkeys).
// Anyone can fill vault2's list with dust on hotkeys of their own; vault2's
// absorbStake moves such a foreign position into its escrow, as surplus, and
// so frees its entry. It returns nil when the recycle may be sent now, and
// otherwise this attempt's outcome. A list that cannot be read does not hold
// the recycle back: if it fails, the step retries.
func stCtfFreeStakingHotkey(ctx context.Context, cfg *StConfig, ctfClient StCtfClient, records stCtfRecords, root *StLedgerV2FinalizedRoot, carry *StCtfCarry) *StPublishOutcome {
	list, err := ctfClient.CtfStakingHotkeys(ctx, carry.VaultColdkey)
	if err != nil {
		glog.Infof("[st]ctf vault2's staking hotkeys could not be read before the recycle of root %d: %s\n", root.Index, err)
		return nil
	}
	if len(list) < stCtfStakingHotkeysMax || slices.Contains(list, carry.PoolHotkey) {
		return nil
	}
	tries := 0
	for _, hotkey := range list {
		key := fmt.Sprintf("%s|%d|%x", cfg.DeploymentKey(), root.Index, hotkey)
		if _, done := stCtfAbsorbed.Load(key); done || hotkey == carry.EscrowHotkey || hotkey == carry.PoolHotkey {
			continue
		}
		if tries == stCtfAbsorbTries {
			break
		}
		tries++
		outcome := stCtfSend(ctx, records, root.Epoch, model.StPublishKindCtfAbsorb, func() (string, error) {
			return ctfClient.CtfAbsorbStake(ctx, ctf.PoolID(cfg.NoId), root.Index, hotkey)
		})
		stCtfAbsorbed.Store(key, true)
		if outcome.Status == model.StPublishStatusConfirmed {
			glog.Infof("[st]ctf absorbed vault2's stake on hotkey 0x%x to free an entry of its staking hotkeys for the recycle of root %d\n", hotkey, root.Index)
			return stCtfWaitUntil(fmt.Sprintf("an entry of vault2's staking hotkeys was freed on hotkey 0x%x", hotkey), stCtfServerNow())
		}
	}
	stCtfAlert(cfg, StCtfAlertCarryLate, fmt.Sprintf("root %d staking hotkeys", root.Index), "vault2's staking hotkey list is full without the pool hotkey and no foreign entry could be absorbed, so the recycle of root %d waits", root.Index)
	return stCtfWaitUntil("vault2's staking hotkey list is full", stCtfServerNow().Add(5*stCtfPollDelay))
}
