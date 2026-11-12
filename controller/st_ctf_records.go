package controller

// st_ctf_records.go — what the capture the flag driver keeps (sn
// CAPTURETHEFLAG.md §4.3 to §5, §11.2): its publish rows; the event mirror's
// rows of the CTF pool P = 2^32 + n, which the v2 tables scope by no_id; and
// what it writes for readers into the capture the flag tables
// (model/ctf_model.go), scoped by the operator's no_id n: each epoch's census
// commitment and draw, each covered epoch's result fixed at the first live
// root that covers it (§5, fixed inputs), and every ledger it builds. The
// driver reaches them through stCtfRecords, which the database implements and
// the driver's tests replace with memory.

import (
	"context"
	"sync"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// stCtfRecords is the driver's durable records of one deployment.
type stCtfRecords interface {
	// Outcome is the latest terminal status (confirmed, skipped or missed) of
	// an epoch's publish rows of a kind, and its reason; "" when none ended.
	Outcome(ctx context.Context, epoch uint64, kind string) (status string, reason string)
	// Publish records a pending publish row, and Resolve its outcome.
	Publish(ctx context.Context, epoch uint64, kind string) server.Id
	Resolve(ctx context.Context, publishId server.Id, outcome *StPublishOutcome)
	// The event mirror's rows of a pool.
	PendingRoot(ctx context.Context, poolId uint64) *model.StLedgerV2Root
	Root(ctx context.Context, poolId uint64, seq uint64) *model.StLedgerV2Root
	FinalizedRoot(ctx context.Context, poolId uint64, rootIndex uint64) *model.StLedgerV2Root
	LatestFinalizedRoot(ctx context.Context, poolId uint64) *model.StLedgerV2Root
	NextUnrecordedRoot(ctx context.Context, poolId uint64) *model.StLedgerV2Root
	RootsFromEpoch(ctx context.Context, poolId uint64, epoch uint64) []*model.StLedgerV2Root
	OpenDemands(ctx context.Context, poolId uint64, seq uint64) []*model.StLedgerV2Demand
	Funding(ctx context.Context, poolId uint64, epoch uint64) *model.StLedgerV2Funding
	// MirroredBlock is the block before which the mirror has applied every
	// event.
	MirroredBlock(ctx context.Context) uint64
	// RecordRoot writes a finalized root's new accounts and amounts.
	RecordRoot(ctx context.Context, poolId uint64, seq uint64, firstIndex uint64, newColdkeys [][32]byte, amounts []*model.StLedgerV2Amount) error
	// SetCommitment writes an epoch's census commitment, false for another
	// census hash.
	SetCommitment(ctx context.Context, commitment *model.CtfCensusCommitment) bool
	// CreateDraw writes an epoch's draw once.
	CreateDraw(ctx context.Context, draw *model.CtfDraw) bool
	// EpochResults returns the recorded results of epochs first … last;
	// CreateEpochResult records one, once; RemoveEpochResult removes one,
	// only ever a result no committed root carries.
	EpochResults(ctx context.Context, first uint64, last uint64) map[uint64]*model.CtfEpochResult
	CreateEpochResult(ctx context.Context, result *model.CtfEpochResult, awards []*model.CtfAward, pots []*model.CtfPot, pending []*model.CtfPendingAward) bool
	RemoveEpochResult(ctx context.Context, epoch uint64)
	// CreateLedger writes a built ledger once, SetLedgerStatus records its
	// change of state, and Ledger reads it.
	CreateLedger(ctx context.Context, ledger *model.CtfLedger) bool
	SetLedgerStatus(ctx context.Context, epoch uint64, ledgerHash [32]byte, update *model.CtfLedgerStatusUpdate) bool
	Ledger(ctx context.Context, epoch uint64, ledgerHash [32]byte) *model.CtfLedger
}

// stCtfDbRecords keeps the records in the database.
type stCtfDbRecords struct {
	deploymentKey model.StDeploymentKey
	noId          uint64
}

func (self *stCtfDbRecords) Outcome(ctx context.Context, epoch uint64, kind string) (string, string) {
	status, reason := "", ""
	for _, publish := range model.GetStPublishes(ctx, self.deploymentKey, epoch) {
		if publish.Kind == kind && (publish.Status == model.StPublishStatusConfirmed || publish.Status == model.StPublishStatusSkipped || publish.Status == model.StPublishStatusMissed) {
			status, reason = publish.Status, ""
			if publish.Error != nil {
				reason = *publish.Error
			}
		}
	}
	return status, reason
}

func (self *stCtfDbRecords) Publish(ctx context.Context, epoch uint64, kind string) server.Id {
	return model.AddStPublish(ctx, self.deploymentKey, epoch, kind)
}

func (self *stCtfDbRecords) Resolve(ctx context.Context, publishId server.Id, outcome *StPublishOutcome) {
	stResolvePublish(ctx, publishId, outcome)
}

func (self *stCtfDbRecords) PendingRoot(ctx context.Context, poolId uint64) *model.StLedgerV2Root {
	return model.GetStLedgerV2PendingRoot(ctx, self.deploymentKey, poolId)
}

func (self *stCtfDbRecords) Root(ctx context.Context, poolId uint64, seq uint64) *model.StLedgerV2Root {
	return model.GetStLedgerV2Root(ctx, self.deploymentKey, poolId, seq)
}

func (self *stCtfDbRecords) FinalizedRoot(ctx context.Context, poolId uint64, rootIndex uint64) *model.StLedgerV2Root {
	return model.GetStLedgerV2FinalizedRoot(ctx, self.deploymentKey, poolId, rootIndex)
}

func (self *stCtfDbRecords) LatestFinalizedRoot(ctx context.Context, poolId uint64) *model.StLedgerV2Root {
	return model.GetLatestStLedgerV2FinalizedRoot(ctx, self.deploymentKey, poolId)
}

func (self *stCtfDbRecords) NextUnrecordedRoot(ctx context.Context, poolId uint64) *model.StLedgerV2Root {
	return model.GetNextUnrecordedStLedgerV2Root(ctx, self.deploymentKey, poolId)
}

func (self *stCtfDbRecords) RootsFromEpoch(ctx context.Context, poolId uint64, epoch uint64) []*model.StLedgerV2Root {
	return model.GetStLedgerV2RootsFromEpoch(ctx, self.deploymentKey, poolId, epoch)
}

func (self *stCtfDbRecords) OpenDemands(ctx context.Context, poolId uint64, seq uint64) []*model.StLedgerV2Demand {
	return model.GetOpenStLedgerV2Demands(ctx, self.deploymentKey, poolId, seq)
}

func (self *stCtfDbRecords) Funding(ctx context.Context, poolId uint64, epoch uint64) *model.StLedgerV2Funding {
	return model.GetStLedgerV2Funding(ctx, self.deploymentKey, poolId, epoch)
}

func (self *stCtfDbRecords) MirroredBlock(ctx context.Context) uint64 {
	return model.GetStChainCheckpoint(ctx, self.deploymentKey).NextBlock
}

func (self *stCtfDbRecords) RecordRoot(ctx context.Context, poolId uint64, seq uint64, firstIndex uint64, newColdkeys [][32]byte, amounts []*model.StLedgerV2Amount) error {
	return model.RecordStLedgerV2Root(ctx, self.deploymentKey, poolId, seq, firstIndex, newColdkeys, amounts)
}

func (self *stCtfDbRecords) SetCommitment(ctx context.Context, commitment *model.CtfCensusCommitment) bool {
	commitment.DeploymentKey, commitment.NoId = self.deploymentKey, self.noId
	return model.SetCtfCensusCommitment(ctx, commitment)
}

func (self *stCtfDbRecords) CreateDraw(ctx context.Context, draw *model.CtfDraw) bool {
	draw.DeploymentKey, draw.NoId = self.deploymentKey, self.noId
	return model.CreateCtfDraw(ctx, draw)
}

func (self *stCtfDbRecords) EpochResults(ctx context.Context, first uint64, last uint64) map[uint64]*model.CtfEpochResult {
	results := map[uint64]*model.CtfEpochResult{}
	for _, result := range model.GetCtfEpochResults(ctx, self.deploymentKey, self.noId, first, last) {
		results[result.Epoch] = result
	}
	return results
}

func (self *stCtfDbRecords) CreateEpochResult(ctx context.Context, result *model.CtfEpochResult, awards []*model.CtfAward, pots []*model.CtfPot, pending []*model.CtfPendingAward) bool {
	result.DeploymentKey, result.NoId = self.deploymentKey, self.noId
	return model.CreateCtfEpochResult(ctx, result, awards, pots, pending)
}

func (self *stCtfDbRecords) RemoveEpochResult(ctx context.Context, epoch uint64) {
	model.RemoveCtfEpochResult(ctx, self.deploymentKey, self.noId, epoch)
}

func (self *stCtfDbRecords) CreateLedger(ctx context.Context, ledger *model.CtfLedger) bool {
	ledger.DeploymentKey, ledger.NoId = self.deploymentKey, self.noId
	return model.CreateCtfLedger(ctx, ledger)
}

func (self *stCtfDbRecords) SetLedgerStatus(ctx context.Context, epoch uint64, ledgerHash [32]byte, update *model.CtfLedgerStatusUpdate) bool {
	return model.SetCtfLedgerStatus(ctx, self.deploymentKey, self.noId, epoch, ledgerHash, update)
}

func (self *stCtfDbRecords) Ledger(ctx context.Context, epoch uint64, ledgerHash [32]byte) *model.CtfLedger {
	return model.GetCtfLedger(ctx, self.deploymentKey, self.noId, epoch, ledgerHash)
}

var stCtfRecordsLock sync.Mutex
var stCtfRecordsInstance stCtfRecords

// setStCtfRecords replaces the driver's records (for tests); nil restores the
// database.
func setStCtfRecords(records stCtfRecords) {
	stCtfRecordsLock.Lock()
	defer stCtfRecordsLock.Unlock()
	stCtfRecordsInstance = records
}

func stCtfRecordsFor(cfg *StConfig) stCtfRecords {
	stCtfRecordsLock.Lock()
	instance := stCtfRecordsInstance
	stCtfRecordsLock.Unlock()
	if instance != nil {
		return instance
	}
	return &stCtfDbRecords{deploymentKey: cfg.DeploymentKey(), noId: cfg.NoId}
}
