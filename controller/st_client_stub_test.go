package controller

// st_client_stub_test.go — a stub StClient proving the swappable-client
// seam (the SetCoinbaseClient pattern): controller flows can be pointed at
// a canned chain for tests, and the open-epoch mirror row derives all its
// deadlines from the contract block clock.

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/model"
)

// stubStClient is a canned StClient for controller tests.
type stubStClient struct {
	state             *StEpochState
	pool              *StPoolState
	epochCloseBlocks  map[uint64]uint64
	unaccountedRao    *big.Int
	buybackTotalRao   *big.Int
	nextFinalizeEpoch uint64

	rollCount     int
	commitCount   int
	depositCount  int
	finalizeCount int
}

func newStubStClient(state *StEpochState) *stubStClient {
	return &stubStClient{
		state: state,
		pool: &StPoolState{
			PoolTotalRao: big.NewInt(0),
			ClaimedRao:   big.NewInt(0),
		},
		epochCloseBlocks: map[uint64]uint64{},
		unaccountedRao:   big.NewInt(0),
		buybackTotalRao:  big.NewInt(0),
	}
}

func (self *stubStClient) Epoch(ctx context.Context) (*StEpochState, error) {
	return self.state, nil
}

func (self *stubStClient) PendingEpoch(ctx context.Context) (uint64, error) {
	return self.state.PendingEpoch, nil
}

func (self *stubStClient) EpochCloseBlock(ctx context.Context, epoch uint64) (uint64, error) {
	return self.epochCloseBlocks[epoch], nil
}

func (self *stubStClient) BlockTime(ctx context.Context, block uint64) (time.Time, error) {
	return stEstimateBlockTime(self.state.HeadBlock, self.state.HeadBlockTime, block, stDefaultBlockSeconds), nil
}

func (self *stubStClient) RollEpochs(ctx context.Context) (string, error) {
	self.rollCount += 1
	return "0xstubroll", nil
}

func (self *stubStClient) DepositPush(ctx context.Context, alphaRao *big.Int) (string, error) {
	self.depositCount += 1
	return "0xstubpush", nil
}

func (self *stubStClient) DepositCredit(ctx context.Context, noId uint64, alphaRao *big.Int) (string, error) {
	self.depositCount += 1
	return "0xstubcredit", nil
}

func (self *stubStClient) UnaccountedStakeRao(ctx context.Context) (*big.Int, error) {
	return new(big.Int).Set(self.unaccountedRao), nil
}

func (self *stubStClient) BuybackTotal(ctx context.Context) (*big.Int, error) {
	return new(big.Int).Set(self.buybackTotalRao), nil
}

func (self *stubStClient) CommitPayoutRoot(ctx context.Context, epoch uint64, noId uint64, root [32]byte, off []byte) (string, error) {
	self.commitCount += 1
	return "0xstubcommit", nil
}

func (self *stubStClient) FinalizeEpoch(ctx context.Context, epoch uint64) (string, error) {
	self.finalizeCount += 1
	return "0xstubfinalize", nil
}

func (self *stubStClient) NextFinalizeEpoch(ctx context.Context) (uint64, error) {
	return self.nextFinalizeEpoch, nil
}

func (self *stubStClient) SyncEvents(ctx context.Context, fromBlock uint64, toBlock uint64) ([]*model.StChainEvent, uint64, error) {
	return nil, toBlock + 1, nil
}

func (self *stubStClient) PoolState(ctx context.Context, epoch uint64, noId uint64) (*StPoolState, error) {
	return self.pool, nil
}

// the stub must keep satisfying the full StClient surface
var _ StClient = (*stubStClient)(nil)

func TestStClientStubSwap(t *testing.T) {
	state := &StEpochState{
		Epoch:                4,
		PendingEpoch:         4,
		EpochStartBlock:      10_000,
		TEpochBlocks:         600,
		CommitWindowBlocks:   1_200,
		TrailsWindowBlocks:   7_200,
		FinalizeOffsetBlocks: 14_400,
		HeadBlock:            10_100,
		HeadBlockTime:        time.Date(2026, time.July, 2, 12, 0, 0, 0, time.UTC),
	}
	stub := newStubStClient(state)
	SetStClient(stub)
	defer SetStClient(nil)

	client := stClient()
	connect.AssertEqual(t, StClient(stub), client)

	got, err := client.Epoch(context.Background())
	connect.AssertEqual(t, nil, err)
	connect.AssertEqual(t, state, got)

	// the open-epoch mirror row derives every deadline from the contract
	// block clock: close = start + tEpoch, deadline = close + window
	row := stEpochRowFromState(got)
	connect.AssertEqual(t, uint64(4), row.Epoch)
	connect.AssertEqual(t, uint64(10_000), row.StartBlock)
	connect.AssertEqual(t, uint64(10_600+1_200), row.CommitDeadlineBlock)
	connect.AssertEqual(t, uint64(10_600+7_200), row.TrailsDeadlineBlock)
	connect.AssertEqual(t, uint64(10_600+14_400), row.FinalizeBlock)
	connect.AssertEqual(t, model.StEpochStatusOpen, row.Status)
}
