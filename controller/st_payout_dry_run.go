// The read-only release payout dry run for owner review before an epoch's root
// is published. It shares the close's preparation, so a closed epoch reports
// the exact leaves the close would publish, and it reads the operator pool's
// funding. It publishes nothing, stores nothing and sends no transaction.
package controller

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Preparing with a configured roster retains original work observations, so
// the read-only dry run refuses before it reads anything else.
var ErrStPayoutDryRunRosterConfigured = errors.New("st: a provider work roster is configured; the read-only payout dry run covers only payouts without one")

// Entitlement statuses of the settlement vault, by value.
var stPoolFundingStatusNames = []string{"unset", "funded", "finalized", "root_missed", "carried"}

// One operator pool's funding at the finalized head. Amounts are alpha rao.
type StPoolFunding struct {
	HeadBlock uint64
	Status    uint8
	// captured at the epoch's close; zero when the close was deferred
	FundedRao *big.Int
	// set at finalize: the funded amount plus the carry it received
	TotalRao   *big.Int
	ClaimedRao *big.Int
	// missed roots' funding, added to the operator's next finalized root
	CarryRao *big.Int
	// stake on the pool hotkey that the next timely close captures
	AccruingRao *big.Int
}

func (self *StPoolFunding) StatusName() string {
	if int(self.Status) < len(stPoolFundingStatusNames) {
		return stPoolFundingStatusNames[self.Status]
	}
	return fmt.Sprintf("status_%d", self.Status)
}

// Implemented by the core client. A client without it reports no funding.
type stPoolFundingReader interface {
	PoolFunding(ctx context.Context, epoch uint64, noId uint64) (*StPoolFunding, error)
}

func (self *CoreStClient) PoolFunding(ctx context.Context, epoch uint64, noId uint64) (*StPoolFunding, error) {
	if self.coordinator == nil || self.vault == nil || self.cfg.SettlementVault == (common.Address{}) {
		return nil, errors.New("st: pool funding requires a release deployment")
	}
	ctx, stopRead, err := beginStRpcRead(ctx, self.readHooks)
	if err != nil {
		return nil, err
	}
	defer stopRead()
	head, err := self.finalizedBlock(ctx)
	if err != nil {
		return nil, err
	}
	e, n := new(big.Int).SetUint64(epoch), new(big.Int).SetUint64(noId)
	entitlement, err := stViewAtBlock(self, ctx, self.cfg.SettlementVault, head.Number, self.vault.PackEntitlement(e, n), self.vault.UnpackEntitlement)
	if err != nil {
		return nil, fmt.Errorf("entitlement(): %w", err)
	}
	carry, err := stViewAtBlock(self, ctx, self.cfg.SettlementVault, head.Number, self.vault.PackCarry(n), self.vault.UnpackCarry)
	if err != nil {
		return nil, fmt.Errorf("carry(): %w", err)
	}
	pool, err := stViewAtBlock(self, ctx, self.cfg.SettlementVault, head.Number, self.vault.PackPools(n), self.vault.UnpackPools)
	if err != nil {
		return nil, fmt.Errorf("pools(): %w", err)
	}
	selfColdkey, err := stViewAtBlock(self, ctx, self.cfg.SettlementVault, head.Number, self.vault.PackSelfColdkey(), self.vault.UnpackSelfColdkey)
	if err != nil {
		return nil, fmt.Errorf("selfColdkey(): %w", err)
	}
	accruing, err := self.stakeAtBlock(ctx, head.Number, pool.Hotkey, selfColdkey)
	if err != nil {
		return nil, fmt.Errorf("pool getStake(): %w", err)
	}
	return &StPoolFunding{
		HeadBlock:   head.Number,
		Status:      entitlement.Status,
		FundedRao:   new(big.Int).Set(entitlement.Funded),
		TotalRao:    new(big.Int).Set(entitlement.Total),
		ClaimedRao:  new(big.Int).Set(entitlement.Claimed),
		CarryRao:    new(big.Int).Set(carry),
		AccruingRao: new(big.Int).Set(accruing),
	}, nil
}

// Counts and amounts are aggregates. Artifact, Wallets and NetworkForClient
// hold provider identifiers and wallet addresses for the owner only.
type StPayoutDryRun struct {
	Epoch uint64
	// the open epoch through the finalized head, not its final window
	Preview      bool
	StartBlock   uint64
	EndBlock     uint64
	StartTime    time.Time
	EndTime      time.Time
	EarningStart time.Time
	EarningEnd   time.Time
	Wallets      *StPayoutWalletSelection
	// signed but unpublished
	Artifact         *payoutartifact.Artifact
	ProviderWallets  map[server.Id]*model.StProviderWallet
	NetworkForClient map[[16]byte]server.Id
	// the root the close already published for the epoch, if any
	PublishedRoot      *[32]byte
	Funding            *StPoolFunding
	FundingErr         error
	PreviousFunding    *StPoolFunding
	PreviousFundingErr error
	CensusDuration     time.Duration
	Duration           time.Duration
}

// A closed epoch uses its authenticated window, so the result is what the
// close publishes. The open epoch is previewed through the finalized head.
func StDryRunEpochPayout(ctx context.Context, epoch uint64) (*StPayoutDryRun, error) {
	start := time.Now()
	cfg, client, err := stRequire()
	if err != nil {
		return nil, err
	}
	if cfg.SettlementVault == (common.Address{}) {
		return nil, errors.New("st: the payout dry run requires a release deployment")
	}
	if _, _, _, err := LoadProviderWorkAuthorityPolicy(); !errors.Is(err, server.ErrResourceNotFound) {
		if err != nil {
			return nil, err
		}
		return nil, ErrStPayoutDryRunRosterConfigured
	}
	state, err := client.Epoch(ctx)
	if err != nil {
		return nil, err
	}
	var authority *StPayoutEpochAuthority
	preview := false
	switch {
	case epoch < state.PendingEpoch:
		authority, err = client.PayoutEpochAuthority(ctx, epoch)
	case epoch == state.PendingEpoch:
		preview = true
		authority, err = stPreviewPayoutEpochAuthority(ctx, cfg, client, state)
	default:
		return nil, fmt.Errorf("st: epoch %d has not started (current epoch %d)", epoch, state.PendingEpoch)
	}
	if err != nil {
		return nil, err
	}
	if authority == nil || authority.Epoch != epoch {
		return nil, errors.New("st: payout epoch authority is absent or differs")
	}
	payout, err := stPrepareReleasePayout(ctx, cfg, client, epoch, authority.StartTime, authority.EndTime, authority.Start.Block, authority.End.Block, authority, true)
	if err != nil {
		return nil, err
	}
	result := &StPayoutDryRun{
		Epoch:            epoch,
		Preview:          preview,
		StartBlock:       authority.Start.Block,
		EndBlock:         authority.End.Block,
		StartTime:        authority.StartTime,
		EndTime:          authority.EndTime,
		EarningStart:     payout.UsageStart,
		EarningEnd:       payout.UsageEnd,
		Wallets:          payout.WalletSelection,
		Artifact:         payout.Artifact,
		ProviderWallets:  payout.Wallets,
		NetworkForClient: payout.NetworkForClient,
		CensusDuration:   payout.UsageReadDuration,
	}
	if prior := model.GetStPayoutArtifact(ctx, cfg.DeploymentKey(), epoch, cfg.NoId); prior != nil {
		root := prior.PayoutRoot
		result.PublishedRoot = &root
	}
	if reader, ok := client.(stPoolFundingReader); ok {
		result.Funding, result.FundingErr = reader.PoolFunding(ctx, epoch, cfg.NoId)
		if 0 < epoch {
			result.PreviousFunding, result.PreviousFundingErr = reader.PoolFunding(ctx, epoch-1, cfg.NoId)
		}
	} else {
		result.FundingErr = errors.New("st client does not read pool funding")
	}
	result.Duration = time.Since(start)
	return result, nil
}

// The open epoch from its start through the finalized head under the
// configured policy. It is not an authenticated closed window, so only the
// dry run uses it.
func stPreviewPayoutEpochAuthority(ctx context.Context, cfg *StConfig, client StClient, state *StEpochState) (*StPayoutEpochAuthority, error) {
	headBlock, headHash, headTime, err := client.FinalizedHead(ctx)
	if err != nil {
		return nil, err
	}
	if headBlock <= state.EpochStartBlock {
		return nil, fmt.Errorf("st: epoch %d has no finalized block after its start block %d", state.PendingEpoch, state.EpochStartBlock)
	}
	hashes, err := client.BlockHashes(ctx, []uint64{state.EpochStartBlock})
	if err != nil {
		return nil, err
	}
	if len(hashes) != 1 || hashes[0] == ([32]byte{}) {
		return nil, errors.New("st: epoch start block hash is unavailable")
	}
	startTime, err := client.BlockTime(ctx, state.EpochStartBlock)
	if err != nil {
		return nil, err
	}
	if !startTime.Before(headTime) {
		return nil, errors.New("st: epoch preview window does not advance")
	}
	return &StPayoutEpochAuthority{
		Epoch:      state.PendingEpoch,
		PolicyHash: cfg.PolicyHash,
		Start:      protocol.ClientKeyEffectiveBoundary{Block: state.EpochStartBlock, Hash: hashes[0], Epoch: state.PendingEpoch},
		End:        protocol.ClientKeyEffectiveBoundary{Block: headBlock, Hash: headHash, Epoch: state.PendingEpoch},
		StartTime:  startTime.UTC(),
		EndTime:    headTime.UTC(),
	}, nil
}
