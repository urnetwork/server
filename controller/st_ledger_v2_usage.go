package controller

// st_ledger_v2_usage.go — a settlement v2 epoch's usage statement
// (sn/demand/usage). A v2 ledger has no signed usage and no count of users, and the
// next epoch's demand deposit is sized from both, so the operator attests them
// here. These are their own steps, planned beside the v2 driver's and never part of
// them: a failure here delays no close, commit or phase C sweep. They run at zero
// price too, because the first priced epoch's deposit prices the last zero-price
// epoch's usage.
//
// Users are captured once, as soon as the epoch's boundary is finalized, because
// the count is exact only at the close and grows with any later activity. Bytes come
// from the provider chunks of the root committed for the epoch: the header of the one
// DistributionCommitted event inside the epoch's root window, which is what validators
// match a statement against (mainnet allows at most one there). So the statement waits
// for that commit, and a rebuild of the epoch's rows before it changes nothing a
// validator will read; the pool has no weight before the commit anyway. The statement
// is built only from the captured users, that committed header and the epoch's
// finalized boundaries, all fixed once they exist, so every retry signs and publishes
// identical bytes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/demand/usage"
	"github.com/urfoundation/sn/ledger"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/startifact"
)

// The usage steps, for the task layer.
const (
	StLedgerV2UsageStepUsers     = "usage_users"
	StLedgerV2UsageStepStatement = "usage_statement"
)

// StLedgerV2AlertUsageStatement is raised once per epoch when a statement cannot be
// built for a reason that retrying will not change.
const StLedgerV2AlertUsageStatement = "v2_usage_statement"

// stLedgerV2UsagePollDelay spaces the wait for the epoch's provider rows, and the
// retries of a failed read.
const stLedgerV2UsagePollDelay = 1 * time.Minute

// stLedgerV2UsageBackoffDelay is how long the sync leaves an epoch alone after it has
// alerted that the epoch can have no statement, or that its header cannot be read.
const stLedgerV2UsageBackoffDelay = 15 * time.Minute

// The epochs the sync leaves alone, and until when, and the reads of a committed header
// that have failed in a row. A restart reads each once more.
var (
	stLedgerV2UsageBackoff        sync.Map
	stLedgerV2UsageHeaderFailures sync.Map
)

// stLedgerV2UsageHeaderAttempts is how many reads of a committed header may fail in a
// row before the sync alerts and backs off.
const stLedgerV2UsageHeaderAttempts = 3

// StLedgerV2UsageStep is one planned usage step.
type StLedgerV2UsageStep struct {
	Kind  string
	Epoch uint64
	RunAt time.Time
}

// stLedgerV2UsageDue decides the epoch's next usage step from what is recorded:
// users first, then the statement once the root committed for the epoch is mirrored.
// committed is the epoch's entry in that root's header, nil before the commit. An epoch
// the header declares unrecoverable has no bytes to attest, so it has no step, and no
// chain restarting one every turn.
func stLedgerV2UsageDue(record *model.StLedgerV2EpochUsage, committed *ledger.HeaderEpoch) string {
	switch {
	case record == nil:
		return StLedgerV2UsageStepUsers
	case len(record.Statement) != 0, committed == nil, committed.Null:
		return ""
	}
	return StLedgerV2UsageStepStatement
}

// errStLedgerV2UsageRoots is more than one root committed inside an epoch's root
// window, which validators cannot match one statement against.
var errStLedgerV2UsageRoots = errors.New("st ledger v2: more than one root is committed in the epoch's root window")

// stLedgerV2CommittedRoot is the root committed for an epoch inside its root window,
// [end, end + window], as mirrored from its DistributionCommitted event: nil while there
// is none. A root the mirror recorded without its commit, having seen only its
// finalization, is placed by the event itself, read from chain over the window as the
// validators read it. closed reports that the finalized head has passed the window with
// none, so none can come. finalized is the finalized head, or zero to read it.
func stLedgerV2CommittedRoot(ctx context.Context, cfg *StConfig, client StLedgerV2Client, chain StClient, epoch uint64, finalized uint64) (root *model.StLedgerV2Root, closed bool, err error) {
	end, window, err := client.LedgerV2EpochWindow(ctx, epoch)
	if err != nil {
		return nil, false, err
	}
	found := []*model.StLedgerV2Root{}
	unplaced := false
	for _, candidate := range model.GetStLedgerV2RootsFromEpoch(ctx, cfg.DeploymentKey(), cfg.NoId, epoch) {
		switch {
		case candidate.Epoch != epoch:
		case candidate.CommitBlock == nil:
			unplaced = true
		case end <= *candidate.CommitBlock && *candidate.CommitBlock <= end+window:
			found = append(found, candidate)
		}
	}
	if len(found) == 0 && unplaced {
		if finalized == 0 {
			if finalized, _, _, err = chain.FinalizedHead(ctx); err != nil {
				return nil, false, err
			}
		}
		if found, err = stLedgerV2ChainCommits(ctx, cfg, chain, epoch, end, min(end+window, finalized)); err != nil {
			return nil, false, err
		}
	}
	switch len(found) {
	case 0:
		return nil, end+window < finalized, nil
	case 1:
		return found[0], false, nil
	}
	return nil, false, fmt.Errorf("%w: %d for epoch %d", errStLedgerV2UsageRoots, len(found), epoch)
}

// stLedgerV2ChainCommits reads the pool's DistributionCommitted events for an epoch in
// the finalized blocks from … to, in the ranges the event mirror reads.
func stLedgerV2ChainCommits(ctx context.Context, cfg *StConfig, chain StClient, epoch uint64, from uint64, to uint64) ([]*model.StLedgerV2Root, error) {
	found := []*model.StLedgerV2Root{}
	for next := from; next <= to; {
		last, err := stBoundedInclusiveRangeEnd(next, to, stSyncEventsBlockRange)
		if err != nil {
			return nil, err
		}
		events, after, err := chain.SyncEvents(ctx, next, last)
		if err != nil {
			return nil, err
		}
		for _, event := range events {
			if event.Kind != "V2DistributionCommitted" || event.BlockNumber < from || to < event.BlockNumber {
				continue
			}
			var data stLedgerV2EventData
			if json.Unmarshal([]byte(event.DataJson), &data) != nil {
				return nil, fmt.Errorf("st ledger v2: a DistributionCommitted event at block %d does not decode", event.BlockNumber)
			}
			if noId, err := stParseUint(data.NoId); err != nil || noId != cfg.NoId {
				continue
			}
			committed, ok := stLedgerV2EventRoot(cfg.NoId, &data)
			if !ok {
				return nil, fmt.Errorf("st ledger v2: a DistributionCommitted event at block %d does not decode", event.BlockNumber)
			}
			if committed.Epoch == epoch {
				block := event.BlockNumber
				committed.CommitBlock = &block
				found = append(found, committed)
			}
		}
		if after <= next {
			return nil, errors.New("st ledger v2: the event reader made no progress")
		}
		next = after
	}
	return found, nil
}

// A committed header never changes, so the epoch entries read from one are kept.
var stLedgerV2CommittedEpochs sync.Map

// stLedgerV2CommittedEpoch reads the epoch's entry in a committed root's header, the
// object its ledger hash names.
func stLedgerV2CommittedEpoch(ctx context.Context, cfg *StConfig, root *model.StLedgerV2Root, epoch uint64) (*ledger.HeaderEpoch, error) {
	key := fmt.Sprintf("%s|%d|%x|%d", cfg.DeploymentKey(), cfg.NoId, root.LedgerHash, epoch)
	if cached, ok := stLedgerV2CommittedEpochs.Load(key); ok {
		return cached.(*ledger.HeaderEpoch), nil
	}
	data, err := GetStLedgerV2Object(ctx, ledger.Ref(root.LedgerHash).String())
	if err != nil {
		return nil, fmt.Errorf("st ledger v2: the header committed for epoch %d: %w", epoch, err)
	}
	header, err := ledger.DecodeHeader(data)
	if err != nil {
		return nil, fmt.Errorf("st ledger v2: the header committed for epoch %d: %w", epoch, err)
	}
	if header.NoID != cfg.NoId || header.Epoch != root.Epoch {
		return nil, fmt.Errorf("%w: the header committed for epoch %d names pool %d and epoch %d", errStLedgerV2UsageHeader, epoch, header.NoID, header.Epoch)
	}
	var entry *ledger.HeaderEpoch
	for i := range header.Epochs {
		if header.Epochs[i].Epoch != epoch {
			continue
		}
		if entry != nil {
			return nil, fmt.Errorf("%w: the header committed for epoch %d lists it twice", errStLedgerV2UsageHeader, epoch)
		}
		covered := header.Epochs[i]
		entry = &covered
	}
	if entry == nil {
		return nil, fmt.Errorf("%w: the header committed for epoch %d does not cover it", errStLedgerV2UsageHeader, epoch)
	}
	stLedgerV2CommittedEpochs.Store(key, entry)
	return entry, nil
}

// errStLedgerV2UsageHeader is a committed header that no statement can be built from:
// it names another pool or epoch, does not cover the epoch, or lists it twice.
var errStLedgerV2UsageHeader = errors.New("st ledger v2: the committed header cannot attest the epoch")

// StLedgerV2UsageSync plans one sync turn's usage steps: the epoch that has just
// ended, once its boundary is finalized, on a registered pool. Nothing is due without
// settlement v2, and the epoch the deposit does not price any more is left alone.
func StLedgerV2UsageSync(ctx context.Context, state *StEpochState) ([]*StLedgerV2UsageStep, error) {
	cfg, client, err := stLedgerV2Require()
	if errors.Is(err, errStLedgerV2NotConfigured) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	firstEpoch, registered, err := stLedgerV2FirstEpoch(ctx, cfg, client)
	if err != nil || !registered || state.Epoch == 0 || state.Epoch <= firstEpoch {
		return nil, err
	}
	epoch := state.Epoch - 1
	deploymentKey := cfg.DeploymentKey()
	backoffKey := fmt.Sprintf("%s|%d|%d", deploymentKey, cfg.NoId, epoch)
	if until, ok := stLedgerV2UsageBackoff.Load(backoffKey); ok && server.NowUtc().Before(until.(time.Time)) {
		return nil, nil
	}
	// An epoch that can have no statement, or whose header keeps failing, alerts once
	// and is not read again for a while: nothing a turn can do changes it.
	giveUp := func(format string, args ...any) {
		stLedgerV2Alert(cfg, StLedgerV2AlertUsageStatement, fmt.Sprintf("epoch %d", epoch), format, args...)
		stLedgerV2UsageBackoff.Store(backoffKey, server.NowUtc().Add(stLedgerV2UsageBackoffDelay))
	}
	record := model.GetStLedgerV2EpochUsage(ctx, deploymentKey, cfg.NoId, epoch)
	var committed *ledger.HeaderEpoch
	if record != nil && len(record.Statement) == 0 {
		_, chain, err := stRequire()
		if err != nil {
			return nil, err
		}
		root, closed, err := stLedgerV2CommittedRoot(ctx, cfg, client, chain, epoch, state.HeadBlock)
		switch {
		case errors.Is(err, errStLedgerV2UsageRoots):
			giveUp("%s, so it has no usage statement validators can match, and a deposit priced from it cannot be sized", err)
			return nil, nil
		case err != nil:
			return nil, err
		case root == nil && closed:
			giveUp("no root was committed for epoch %d in its root window, so it has no usage statement, and a deposit priced from it cannot be sized", epoch)
			return nil, nil
		case root != nil:
			committed, err = stLedgerV2CommittedEpoch(ctx, cfg, root, epoch)
			if err != nil {
				failures := 1
				if previous, ok := stLedgerV2UsageHeaderFailures.Load(backoffKey); ok {
					failures += previous.(int)
				}
				// A header that cannot attest the epoch never will. One that cannot be
				// read is tried again, and given up on for a while once it keeps failing.
				if !errors.Is(err, errStLedgerV2UsageHeader) && failures < stLedgerV2UsageHeaderAttempts {
					stLedgerV2UsageHeaderFailures.Store(backoffKey, failures)
					return nil, err
				}
				stLedgerV2UsageHeaderFailures.Delete(backoffKey)
				giveUp("%s, so no usage statement is built from it, and a deposit priced from it cannot be sized", stScrubEndpoints(err.Error()))
				return nil, nil
			}
			stLedgerV2UsageHeaderFailures.Delete(backoffKey)
		}
	}
	if committed != nil && committed.Null {
		giveUp("the root committed for epoch %d declares its rows unrecoverable, so it has no usage statement and the next epoch's priced deposit cannot be sized", epoch)
	}
	steps := []*StLedgerV2UsageStep{}
	if kind := stLedgerV2UsageDue(record, committed); kind != "" {
		steps = append(steps, &StLedgerV2UsageStep{Kind: kind, Epoch: epoch, RunAt: server.NowUtc()})
	}
	// The extender census is captured beside the users, as close to the
	// boundary as a failed read allows (st_ledger_v2_extenders.go), and an epoch
	// before E_x then gets its shadow report (st_ledger_v2_extender_shadow.go).
	if stLedgerV2ExtenderCensusDue(ctx, cfg, epoch) {
		steps = append(steps, &StLedgerV2UsageStep{Kind: StLedgerV2UsageStepExtenderCensus, Epoch: epoch, RunAt: server.NowUtc()})
	} else if stLedgerV2ExtenderShadowDue(ctx, cfg, epoch) {
		steps = append(steps, &StLedgerV2UsageStep{Kind: StLedgerV2UsageStepExtenderShadow, Epoch: epoch, RunAt: server.NowUtc()})
	}
	if len(steps) == 0 {
		return nil, nil
	}
	return steps, nil
}

// StLedgerV2RunUsageStep runs one planned usage step. Each is idempotent.
func StLedgerV2RunUsageStep(ctx context.Context, step *StLedgerV2UsageStep) (*StPublishOutcome, error) {
	cfg, client, err := stRequire()
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled || !cfg.LedgerV2Enabled() {
		return nil, errStLedgerV2NotConfigured
	}
	if err := stPayoutAdmission(ctx, cfg); err != nil {
		return nil, err
	}
	switch step.Kind {
	case StLedgerV2UsageStepUsers:
		return stLedgerV2CaptureUsers(ctx, cfg, client, step.Epoch), nil
	case StLedgerV2UsageStepExtenderCensus:
		return stLedgerV2CaptureExtenderCensus(ctx, cfg, client, step.Epoch), nil
	case StLedgerV2UsageStepExtenderShadow:
		_, v2, err := stLedgerV2Require()
		if err != nil {
			return nil, err
		}
		return stLedgerV2RunExtenderShadow(ctx, cfg, stLedgerV2ExtenderShadowViews{client: v2}, step.Epoch), nil
	case StLedgerV2UsageStepStatement:
		_, v2, err := stLedgerV2Require()
		if err != nil {
			return nil, err
		}
		return stLedgerV2PublishStatement(ctx, cfg, client, v2, step.Epoch), nil
	}
	return nil, fmt.Errorf("st ledger v2: unknown usage step %q", step.Kind)
}

func stLedgerV2UsageRetry(reason string) *StPublishOutcome {
	return &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: stScrubEndpoints(reason), Retry: true, RetryAt: server.NowUtc().Add(stLedgerV2UsagePollDelay)}
}

// stLedgerV2CaptureUsers records the epoch's user count once, over the epoch's
// authenticated window. An epoch already recorded is never counted again.
func stLedgerV2CaptureUsers(ctx context.Context, cfg *StConfig, client StClient, epoch uint64) *StPublishOutcome {
	if record := model.GetStLedgerV2EpochUsage(ctx, cfg.DeploymentKey(), cfg.NoId, epoch); record != nil {
		return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: fmt.Sprintf("epoch %d users already captured: %d", epoch, record.TotalUsers)}
	}
	authority, err := client.PayoutEpochAuthority(ctx, epoch)
	if err != nil {
		return stLedgerV2UsageRetry(err.Error())
	}
	if authority == nil || authority.Epoch != epoch {
		return stLedgerV2UsageRetry(fmt.Sprintf("the payout authority of epoch %d is absent or differs", epoch))
	}
	counted := model.CountTopLevelClientsWithContractInEpoch(ctx, authority.StartTime, authority.EndTime)
	if counted < 0 {
		return stLedgerV2UsageRetry(fmt.Sprintf("epoch %d user count is negative", epoch))
	}
	captured := model.CaptureStLedgerV2EpochUsers(ctx, cfg.DeploymentKey(), cfg.NoId, epoch, uint64(counted))
	return &StPublishOutcome{Status: model.StPublishStatusConfirmed, Reason: fmt.Sprintf("epoch %d users captured: %d", epoch, captured)}
}

// stLedgerV2StatementInput is everything a statement is built from.
type stLedgerV2StatementInput struct {
	Epoch      uint64
	Authority  *StPayoutEpochAuthority
	Chunks     []string
	TotalBytes uint64
	Rows       uint64
	Users      uint64
	// Extenders is the extender pool of an epoch whose committed header carries
	// an extender component, which makes the statement version 2 (sn
	// demand/usage, WHITEPAPER.md §8.6.4); nil for every other epoch, whose
	// statement stays version 1, byte for byte. TotalBytes then holds all of Z.
	Extenders *usage.ExtenderPool
}

// stLedgerV2SealStatement builds, signs and wraps one epoch's statement. Its only
// inputs are fixed once they exist: the captured users, the rows of the committed
// header and the epoch's finalized boundaries, and CreatedAt is the epoch's end, so the same epoch
// always yields the same bytes.
func stLedgerV2SealStatement(cfg *StConfig, input *stLedgerV2StatementInput) (*usage.Statement, []byte, *startifact.EvidenceEnvelope, error) {
	if cfg == nil || cfg.ArtifactKey == nil || cfg.Netuid == 0 || cfg.Netuid > 65535 {
		return nil, nil, nil, errors.New("st ledger v2: the usage statement needs the artifact key and netuid")
	}
	authority := input.Authority
	chunks := make([]string, len(input.Chunks))
	for i, text := range input.Chunks {
		ref, err := ledger.ParseRef(text)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("st ledger v2: provider chunk %d: %w", i, err)
		}
		chunks[i] = ref.String()
	}
	statement := &usage.Statement{
		DeploymentID: cfg.DeploymentId, ChainID: cfg.ChainId, GenesisHash: fmt.Sprintf("0x%x", cfg.GenesisHash), Netuid: uint16(cfg.Netuid),
		Coordinator: cfg.ContractAddress, VaultV2: cfg.VaultV2, PolicyHash: fmt.Sprintf("0x%x", authority.PolicyHash), NoID: cfg.NoId, Epoch: input.Epoch,
		Start:          usage.Boundary{Number: authority.Start.Block, Hash: strings.ToLower(common.Hash(authority.Start.Hash).Hex())},
		End:            usage.Boundary{Number: authority.End.Block, Hash: strings.ToLower(common.Hash(authority.End.Hash).Hex())},
		ProviderChunks: chunks, ProviderRows: input.Rows, TotalUsageBytes: input.TotalBytes, TotalUsers: input.Users,
		ExtenderPool: input.Extenders,
	}
	if err := statement.Seal(cfg.ArtifactKey); err != nil {
		return nil, nil, nil, err
	}
	payload, err := statement.Bytes()
	if err != nil {
		return nil, nil, nil, err
	}
	envelope := &startifact.EvidenceEnvelope{
		DeploymentID: cfg.DeploymentId, ChainID: cfg.ChainId, GenesisHash: statement.GenesisHash, Netuid: uint16(cfg.Netuid),
		Kind: statement.Kind(), RunID: usage.RunID(cfg.NoId, input.Epoch), CreatedAt: authority.EndTime.UTC().Format(time.RFC3339Nano), Payload: payload,
	}
	if err := startifact.SignEvidence(envelope, cfg.ArtifactKey); err != nil {
		return nil, nil, nil, err
	}
	return statement, payload, envelope, nil
}

// stLedgerV2PublishStatement sums the provider rows the root committed for the epoch
// lists, seals its statement, publishes it and records it. It waits until that commit
// is mirrored. An epoch the committed header declares unrecoverable can have none: the
// sync alerts it, and the deposit it would size waits on the operator.
func stLedgerV2PublishStatement(ctx context.Context, cfg *StConfig, client StClient, v2 StLedgerV2Client, epoch uint64) *StPublishOutcome {
	deploymentKey, noId := cfg.DeploymentKey(), cfg.NoId
	record := model.GetStLedgerV2EpochUsage(ctx, deploymentKey, noId, epoch)
	if record == nil {
		return stLedgerV2UsageRetry(fmt.Sprintf("epoch %d users are not captured yet", epoch))
	}
	if len(record.Statement) != 0 {
		return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: fmt.Sprintf("epoch %d statement already recorded: %s", epoch, record.ContentHash)}
	}
	root, _, err := stLedgerV2CommittedRoot(ctx, cfg, v2, client, epoch, 0)
	if err != nil {
		return stLedgerV2UsageRetry(fmt.Sprintf("epoch %d committed root: %s", epoch, err))
	}
	if root == nil {
		return stLedgerV2UsageRetry(fmt.Sprintf("no root is committed for epoch %d yet", epoch))
	}
	committed, err := stLedgerV2CommittedEpoch(ctx, cfg, root, epoch)
	if errors.Is(err, errStLedgerV2UsageHeader) {
		return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: stScrubEndpoints(err.Error())}
	}
	if err != nil {
		return stLedgerV2UsageRetry(err.Error())
	}
	if committed.Null {
		return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: fmt.Sprintf("the root committed for epoch %d declares it unrecoverable", epoch)}
	}
	refs := make([]string, len(committed.Providers))
	for i, ref := range committed.Providers {
		refs[i] = ref.String()
	}
	authority, err := client.PayoutEpochAuthority(ctx, epoch)
	if err != nil {
		return stLedgerV2UsageRetry(err.Error())
	}
	if authority == nil || authority.Epoch != epoch {
		return stLedgerV2UsageRetry(fmt.Sprintf("the payout authority of epoch %d is absent or differs", epoch))
	}
	chunks := make([][]byte, len(refs))
	for i, ref := range refs {
		if chunks[i], err = GetStLedgerV2Object(ctx, ref); err != nil {
			return stLedgerV2UsageRetry(fmt.Sprintf("epoch %d provider chunk %d: %s", epoch, i, err))
		}
	}
	decoded, err := ledger.DecodeProviderChunks(chunks)
	if err != nil {
		return stLedgerV2UsageRetry(fmt.Sprintf("epoch %d provider chunks: %s", epoch, err))
	}
	providerBytes, count, err := usage.SumRows(decoded)
	if err != nil {
		return stLedgerV2UsageRetry(fmt.Sprintf("epoch %d provider rows: %s", epoch, err))
	}
	// An epoch whose committed header carries an extender component has a
	// version 2 statement: its extender pool, and all of Z in its total.
	totalBytes := providerBytes
	var pool *usage.ExtenderPool
	var committedExtenders *usage.Extenders
	if committed.Extenders != nil {
		pool, committedExtenders, err = stLedgerV2StatementExtenderPool(ctx, cfg, epoch, committed.Extenders)
		if errors.Is(err, errStLedgerV2UsageHeader) {
			// nothing a turn can do changes it: alert once, and leave the epoch
			// alone for a while, as the sync does
			stLedgerV2Alert(cfg, StLedgerV2AlertUsageStatement, fmt.Sprintf("epoch %d", epoch), "%s, so it has no usage statement, and a deposit priced from it cannot be sized", stScrubEndpoints(err.Error()))
			stLedgerV2UsageBackoff.Store(fmt.Sprintf("%s|%d|%d", deploymentKey, noId, epoch), server.NowUtc().Add(stLedgerV2UsageBackoffDelay))
			return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: stScrubEndpoints(err.Error())}
		}
		if err != nil {
			return stLedgerV2UsageRetry(fmt.Sprintf("epoch %d extender pool: %s", epoch, err))
		}
		if totalBytes, err = usage.TotalUsage(providerBytes, pool.ExtenderPoolBytes); err != nil {
			return stLedgerV2UsageRetry(fmt.Sprintf("epoch %d: %s", epoch, err))
		}
	}
	statement, payload, envelope, err := stLedgerV2SealStatement(cfg, &stLedgerV2StatementInput{Epoch: epoch, Authority: authority, Chunks: refs, TotalBytes: totalBytes, Rows: count, Users: record.TotalUsers, Extenders: pool})
	if err != nil {
		return stLedgerV2UsageRetry(fmt.Sprintf("epoch %d statement: %s", epoch, err))
	}
	// What validators will check before a deposit is priced from it.
	if err := errors.Join(statement.CheckSums(providerBytes, count), statement.CheckExtenders(committedExtenders)); err != nil {
		return stLedgerV2UsageRetry(fmt.Sprintf("epoch %d statement: %s", epoch, err))
	}
	store, err := stLedgerV2Store()
	if err != nil {
		return stLedgerV2UsageRetry(err.Error())
	}
	published, err := startifact.PublishEvidence(ctx, store, envelope)
	if err != nil {
		return stLedgerV2UsageRetry(fmt.Sprintf("epoch %d statement publish: %s", epoch, err))
	}
	if err := model.RecordStLedgerV2EpochStatement(ctx, deploymentKey, noId, epoch, totalBytes, count, published.ContentHash, payload); err != nil {
		glog.Errorf("[st]v2 epoch %d usage statement could not be recorded: %s\n", epoch, err)
		return stLedgerV2UsageRetry(err.Error())
	}
	return &StPublishOutcome{Status: model.StPublishStatusConfirmed, Reason: fmt.Sprintf("epoch %d statement %s (%s): %d bytes in %d rows, %d users", epoch, published.ContentHash, statement.Kind(), totalBytes, count, record.TotalUsers)}
}

// A version 2 statement's extender pool for an epoch whose committed header
// carries component: what the header and its census and share-row chunks hold,
// with Z's parts and the contracts and payers behind it as the epoch's rows
// record kept them when the component was first built. A record that does not
// hold the committed component, the same Z and census chunks, cannot attest it
// (errStLedgerV2UsageHeader).
func stLedgerV2StatementExtenderPool(ctx context.Context, cfg *StConfig, epoch uint64, component *ledger.HeaderExtenders) (*usage.ExtenderPool, *usage.Extenders, error) {
	recorded := model.GetStLedgerV2EpochRows(ctx, cfg.DeploymentKey(), cfg.NoId, epoch, epoch)[epoch]
	if recorded == nil || recorded.Extenders == nil {
		return nil, nil, fmt.Errorf("%w: epoch %d's committed extender component has no recorded pool", errStLedgerV2UsageHeader, epoch)
	}
	census := make([]string, len(component.Census))
	for i, ref := range component.Census {
		census[i] = ref.String()
	}
	if recorded.Extenders.Bytes != component.Bytes || !slices.Equal(recorded.Extenders.CensusChunks, census) {
		return nil, nil, fmt.Errorf("%w: epoch %d's committed extender component is not the one its rows recorded", errStLedgerV2UsageHeader, epoch)
	}
	read := func(refs []ledger.Ref) ([][]byte, error) {
		chunks := make([][]byte, len(refs))
		for i, ref := range refs {
			chunk, err := GetStLedgerV2Object(ctx, ref.String())
			if err != nil {
				return nil, err
			}
			chunks[i] = chunk
		}
		return chunks, nil
	}
	censusChunks, err := read(component.Census)
	if err != nil {
		return nil, nil, fmt.Errorf("census chunk: %w", err)
	}
	rows, err := ledger.DecodeCensusChunks(censusChunks)
	if err == nil {
		err = ledger.CheckCensusRows(rows)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%w: epoch %d census: %s", errStLedgerV2UsageHeader, epoch, err)
	}
	shareChunks, err := read(component.Rows)
	if err != nil {
		return nil, nil, fmt.Errorf("share-row chunk: %w", err)
	}
	shares, err := ledger.DecodeProviderChunks(shareChunks)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: epoch %d share rows: %s", errStLedgerV2UsageHeader, epoch, err)
	}
	committed, err := usage.CommittedExtenders(component, rows, shares)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: epoch %d: %s", errStLedgerV2UsageHeader, epoch, err)
	}
	parts := recorded.Extenders
	pool, err := usage.NewExtenderPool(parts.IntraPlatform, parts.IntraNetwork, parts.Contracts, parts.Payers, committed)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: epoch %d: %s", errStLedgerV2UsageHeader, epoch, err)
	}
	return pool, committed, nil
}

// stDepositStatementUsage pins a recorded statement to this operator and the
// source epoch's finalized boundaries before a deposit consumes its totals, as
// stDepositArtifactUsage does for a payout artifact.
func stDepositStatementUsage(statement *usage.Statement, cfg *StConfig, epoch uint64, authority *StPayoutEpochAuthority) (usageBytes uint64, users uint64, err error) {
	if statement == nil || cfg == nil || cfg.ArtifactKey == nil || authority == nil {
		return 0, 0, errors.New("deposit usage statement identity is incomplete")
	}
	if err := statement.Verify(crypto.PubkeyToAddress(cfg.ArtifactKey.PublicKey)); err != nil {
		return 0, 0, fmt.Errorf("epoch %d usage statement: %w", epoch, err)
	}
	if statement.DeploymentID != cfg.DeploymentId || statement.ChainID != cfg.ChainId || statement.Netuid != uint16(cfg.Netuid) || statement.Coordinator != cfg.ContractAddress || statement.VaultV2 != cfg.VaultV2 || statement.Epoch != epoch || statement.NoID != cfg.NoId ||
		!strings.EqualFold(statement.GenesisHash, fmt.Sprintf("0x%x", cfg.GenesisHash)) || !strings.EqualFold(statement.PolicyHash, fmt.Sprintf("0x%x", authority.PolicyHash)) {
		return 0, 0, fmt.Errorf("epoch %d usage statement deployment identity mismatch", epoch)
	}
	if statement.Start.Number != authority.Start.Block || statement.End.Number != authority.End.Block || !strings.EqualFold(statement.Start.Hash, common.Hash(authority.Start.Hash).Hex()) || !strings.EqualFold(statement.End.Hash, common.Hash(authority.End.Hash).Hex()) {
		return 0, 0, fmt.Errorf("epoch %d usage statement finalized boundary mismatch", epoch)
	}
	return statement.TotalUsageBytes, statement.TotalUsers, nil
}

// stDemandReadEpochUsage reads an epoch's usage record; a failed read, including a
// table not migrated yet, is an error and never "no statement yet".
func stDemandReadEpochUsage(ctx context.Context, cfg *StConfig, epoch uint64) (record *model.StLedgerV2EpochUsage, err error) {
	defer stDemandStoreRecover("usage statement read", &err)
	return model.GetStLedgerV2EpochUsage(ctx, cfg.DeploymentKey(), cfg.NoId, epoch), nil
}

func stDemandReadLatestStatement(ctx context.Context, cfg *StConfig) (record *model.StLedgerV2EpochUsage, err error) {
	defer stDemandStoreRecover("usage statement read", &err)
	return model.GetLatestStLedgerV2EpochStatement(ctx, cfg.DeploymentKey(), cfg.NoId), nil
}

// stDepositStatementTotals reads a v2 source epoch's recorded usage statement and
// returns its totals, or a retryable outcome when it is not recorded yet.
func stDepositStatementTotals(ctx context.Context, cfg *StConfig, client StClient, sourceEpoch uint64) (usageBytes uint64, users uint64, failed *StPublishOutcome) {
	record, err := stDemandReadEpochUsage(ctx, cfg, sourceEpoch)
	if err != nil {
		return 0, 0, &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: err.Error(), Retry: true}
	}
	if record == nil || len(record.Statement) == 0 {
		return 0, 0, &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: fmt.Sprintf("epoch %d usage statement is not recorded yet", sourceEpoch), Retry: true}
	}
	authority, err := client.PayoutEpochAuthority(ctx, sourceEpoch)
	if err != nil {
		return 0, 0, &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: err.Error(), Retry: true}
	}
	if authority == nil || authority.Epoch != sourceEpoch {
		return 0, 0, &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: "payout epoch authority is absent or differs", Retry: true}
	}
	statement, err := usage.Decode(record.Statement)
	if err != nil {
		return 0, 0, &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: fmt.Sprintf("epoch %d usage statement: %v", sourceEpoch, err), Retry: true}
	}
	usageBytes, users, err = stDepositStatementUsage(statement, cfg, sourceEpoch, authority)
	if err != nil {
		return 0, 0, &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: err.Error(), Retry: true}
	}
	return usageBytes, users, nil
}

// stDepositSourceTotals reads a priced deposit's source totals from what settled
// the source epoch: the payout artifact under v1, the usage statement under v2. The
// pool's first v2 epoch decides that, never whether a v1 root exists, because the
// phase C sweep commits one on a v2 epoch.
func stDepositSourceTotals(ctx context.Context, cfg *StConfig, client StClient, sourceEpoch uint64) (usageBytes uint64, users uint64, failed *StPublishOutcome) {
	v1, err := stV1EpochAllowed(ctx, cfg, sourceEpoch)
	if err != nil {
		return 0, 0, &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: err.Error(), Retry: true}
	}
	if v1 {
		return stDepositArtifactTotals(ctx, cfg, client, sourceEpoch)
	}
	return stDepositStatementTotals(ctx, cfg, client, sourceEpoch)
}
