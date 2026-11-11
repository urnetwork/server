package controller

// st_ctf_records.go — what the capture the flag driver keeps (sn
// CAPTURETHEFLAG.md §5, §11.2): its publish rows, each epoch's fixed awards,
// and the event mirror's rows of the CTF pool P = 2^32 + n. Nothing here is
// new storage: the v2 tables are scoped by no_id, so the CTF pool's rows sit
// beside the provider pool's (model/st_ctf_driver_model.go). The driver reaches
// them through stCtfRecords, which the database implements and the driver's
// tests replace with memory.

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
	// FixedAwards returns the recorded fixed awards of epochs first … last,
	// and RecordFixedAwards records an epoch's (st_ledger_v2_epoch_rows).
	FixedAwards(ctx context.Context, poolId uint64, first uint64, last uint64) map[uint64]*model.StLedgerV2EpochRows
	RecordFixedAwards(ctx context.Context, poolId uint64, epoch uint64, awardsRef string) error
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
}

// stCtfDbRecords keeps the records in the database.
type stCtfDbRecords struct {
	deploymentKey model.StDeploymentKey
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

func (self *stCtfDbRecords) FixedAwards(ctx context.Context, poolId uint64, first uint64, last uint64) map[uint64]*model.StLedgerV2EpochRows {
	return model.GetStLedgerV2EpochRows(ctx, self.deploymentKey, poolId, first, last)
}

func (self *stCtfDbRecords) RecordFixedAwards(ctx context.Context, poolId uint64, epoch uint64, awardsRef string) error {
	return model.RecordStLedgerV2EpochRows(ctx, self.deploymentKey, poolId, epoch, []string{awardsRef})
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
	return &stCtfDbRecords{deploymentKey: cfg.DeploymentKey()}
}
