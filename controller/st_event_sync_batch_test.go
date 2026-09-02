package controller

// PostgreSQL-backed regression for event-sync canonicalization. The worker
// must verify all distinct event blocks plus the range endpoint in one logical
// BlockHashes call before mutating the mirror or checkpoint.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// eventSyncBatchStClient records canonical block batches while returning one
// deterministic event range.
type eventSyncBatchStClient struct {
	*stubStClient
	events           []*model.StChainEvent
	blockHashBatches [][]uint64
	singleHashCalls  int
	nextBlock        uint64
	truncateHashes   bool
}

// eventSyncTestHash returns a unique nonzero hash for one block number.
func eventSyncTestHash(block uint64) [32]byte {
	return common.HexToHash(fmt.Sprintf("0x%064x", block+1))
}

// BlockHash records legacy single-block reads. A fresh sync must not need one.
func (self *eventSyncBatchStClient) BlockHash(_ context.Context, block uint64) ([32]byte, error) {
	self.singleHashCalls++
	return eventSyncTestHash(block), nil
}

// BlockHashes records and answers one ordered canonical block surface.
func (self *eventSyncBatchStClient) BlockHashes(_ context.Context, blocks []uint64) ([][32]byte, error) {
	self.blockHashBatches = append(self.blockHashBatches, append([]uint64(nil), blocks...))
	hashes := make([][32]byte, len(blocks))
	for index, block := range blocks {
		hashes[index] = eventSyncTestHash(block)
	}
	if self.truncateHashes {
		return hashes[:len(hashes)-1], nil
	}
	return hashes, nil
}

// SyncEvents returns the exact requested next-block boundary with fixture
// events already pinned to canonical hashes.
func (self *eventSyncBatchStClient) SyncEvents(_ context.Context, fromBlock uint64, toBlock uint64) ([]*model.StChainEvent, uint64, error) {
	if fromBlock != 100 || toBlock != 1_099 {
		return nil, fromBlock, fmt.Errorf("unexpected event range %d..%d", fromBlock, toBlock)
	}
	if self.nextBlock != 0 {
		return self.events, self.nextBlock, nil
	}
	return self.events, toBlock + 1, nil
}

// Multiple logs from one block and a log at the range endpoint must produce
// one deduplicated canonical batch before the durable checkpoint advances.
func TestStSyncChainEventsBatchesCanonicalEventBlocks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		state := &StEpochState{HeadBlock: 1_099, HeadBlockTime: time.Unix(1_700_000_000, 0).UTC()}
		client := &eventSyncBatchStClient{stubStClient: newStubStClient(state)}
		for index, block := range []uint64{101, 101, 1_099} {
			hash := eventSyncTestHash(block)
			client.events = append(client.events, &model.StChainEvent{
				BlockNumber: block, BlockHash: common.BytesToHash(hash[:]).Hex(), LogIndex: index,
				TxHash: fmt.Sprintf("0x%064x", index+1), Kind: "FixtureEvent", DataJson: "{}",
			})
		}
		SetStConfig(&StConfig{Enabled: true, DeployBlock: 100})
		SetStClient(client)
		t.Cleanup(func() {
			SetStClient(nil)
			SetStConfig(nil)
		})
		synced, err := StSyncChainEvents(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		if synced != 3 || client.singleHashCalls != 0 || len(client.blockHashBatches) != 1 || !slices.Equal(client.blockHashBatches[0], []uint64{101, 1_099}) {
			t.Fatalf("synced/single/batches=%d/%d/%v", synced, client.singleHashCalls, client.blockHashBatches)
		}
		checkpoint := model.GetStChainCheckpoint(ctx)
		endHash := eventSyncTestHash(1_099)
		if checkpoint.NextBlock != 1_100 || checkpoint.BlockHash != common.BytesToHash(endHash[:]).Hex() {
			t.Fatalf("checkpoint=%+v", checkpoint)
		}
	})
}

// Malformed range progress, event membership, canonical hashes, and batch
// cardinality all fail before either the durable event mirror or checkpoint
// changes.
func TestStSyncChainEventsRejectsIncompleteCanonicalBatchBeforeMutation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		state := &StEpochState{HeadBlock: 1_099, HeadBlockTime: time.Unix(1_700_000_000, 0).UTC()}
		validHash := eventSyncTestHash(101)
		validEvent := &model.StChainEvent{
			BlockNumber: 101, BlockHash: common.BytesToHash(validHash[:]).Hex(), LogIndex: 0,
			TxHash: fmt.Sprintf("0x%064x", 1), Kind: "FixtureEvent", DataJson: "{}",
		}
		outsideEvent := *validEvent
		outsideEvent.BlockNumber = 1_100
		mismatchedEvent := *validEvent
		mismatchedEvent.BlockHash = common.HexToHash("0xbad").Hex()
		cases := []struct {
			label          string
			events         []*model.StChainEvent
			nextBlock      uint64
			truncateHashes bool
			want           string
		}{
			{label: "range progress", events: []*model.StChainEvent{validEvent}, nextBlock: 1_099, want: "returned next block"},
			{label: "nil event", events: []*model.StChainEvent{nil}, want: "outside the requested range"},
			{label: "outside event", events: []*model.StChainEvent{&outsideEvent}, want: "outside the requested range"},
			{label: "hash mismatch", events: []*model.StChainEvent{&mismatchedEvent}, want: "log block hash mismatch"},
			{label: "batch cardinality", events: []*model.StChainEvent{validEvent}, truncateHashes: true, want: "returned 1 hashes, want 2"},
		}
		SetStConfig(&StConfig{Enabled: true, DeployBlock: 100})
		t.Cleanup(func() {
			SetStClient(nil)
			SetStConfig(nil)
		})
		for _, testCase := range cases {
			client := &eventSyncBatchStClient{
				stubStClient: newStubStClient(state), events: testCase.events,
				nextBlock: testCase.nextBlock, truncateHashes: testCase.truncateHashes,
			}
			SetStClient(client)
			if synced, err := StSyncChainEvents(ctx, 0); err == nil || !strings.Contains(err.Error(), testCase.want) || synced != 0 {
				t.Errorf("%s synced/error=%d/%v", testCase.label, synced, err)
			}
			checkpoint := model.GetStChainCheckpoint(ctx)
			if checkpoint.NextBlock != 0 || checkpoint.BlockHash != "" || len(model.GetStEvents(ctx, 100, 1_099)) != 0 {
				t.Fatalf("%s mutated checkpoint/events: %+v", testCase.label, checkpoint)
			}
		}
	})
}
