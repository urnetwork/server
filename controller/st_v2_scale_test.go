//go:build scale

package controller

// Settlement v2 scale (VERSION2-FINAL.md §15, SV-6): head exclusion at 100k
// and 1M providers reads bindingAt only for the FleetBound candidates, so the
// reads stay bounded by bindings, not providers. The first read scans the
// FleetBound history from the deploy block; a later epoch scans only its own
// blocks. Wall time and peak memory are logged. Run with -tags scale in its
// own release-gate shard.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand/v2"
	"os"
	"runtime"
	"runtime/metrics"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// SV-6 at 100k providers.
func TestStV2ScaleHeadExclusion100k(t *testing.T) {
	runStV2ScaleHeadExclusion(t, 100_000)
}

// SV-6 at 1M providers.
func TestStV2ScaleHeadExclusion1M(t *testing.T) {
	runStV2ScaleHeadExclusion(t, 1_000_000)
}

// Head exclusion's share of the one-hour build. The first read's scan of two
// million blocks of FleetBound history dominates it whatever the population.
const stV2ScaleHeadExclusionBudget = 5 * time.Minute

// The heap a phase adds at its peak over the live heap it starts from, and
// the process's peak resident memory, sampled without stopping the world.
type stV2ScaleMemory struct {
	stop     chan struct{}
	done     chan struct{}
	baseline uint64
	peakHeap atomic.Uint64
	peakRss  atomic.Uint64
}

func startStV2ScaleMemory() *stV2ScaleMemory {
	runtime.GC()
	memory := &stV2ScaleMemory{stop: make(chan struct{}), done: make(chan struct{})}
	samples := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(samples)
	memory.baseline = samples[0].Value.Uint64()
	sample := func() {
		metrics.Read(samples)
		if heap := samples[0].Value.Uint64(); memory.peakHeap.Load() < heap {
			memory.peakHeap.Store(heap)
		}
		if statm, err := os.ReadFile("/proc/self/statm"); err == nil {
			if fields := strings.Fields(string(statm)); 1 < len(fields) {
				if pages, err := strconv.ParseUint(fields[1], 10, 64); err == nil && memory.peakRss.Load() < pages*uint64(os.Getpagesize()) {
					memory.peakRss.Store(pages * uint64(os.Getpagesize()))
				}
			}
		}
	}
	go func() {
		defer close(memory.done)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			sample()
			select {
			case <-memory.stop:
				sample()
				return
			case <-ticker.C:
			}
		}
	}()
	return memory
}

func (self *stV2ScaleMemory) finish() (heap uint64, rss uint64) {
	close(self.stop)
	<-self.done
	return max(self.peakHeap.Load(), self.baseline) - self.baseline, self.peakRss.Load()
}

// A chain with a long FleetBound history. Each range read returns the logs in
// it; bindingAt answers active for every other candidate.
type stV2ScaleChain struct {
	*stubStClient
	events        []*model.StChainEvent
	ranges        atomic.Int64
	bindingInputs []int
}

func (self *stV2ScaleChain) FleetBoundEvents(_ context.Context, fromBlock uint64, toBlock uint64) ([]*model.StChainEvent, error) {
	self.ranges.Add(1)
	start := sort.Search(len(self.events), func(index int) bool { return fromBlock <= self.events[index].BlockNumber })
	end := sort.Search(len(self.events), func(index int) bool { return toBlock < self.events[index].BlockNumber })
	return self.events[start:end], nil
}

func (self *stV2ScaleChain) BindingsAt(_ context.Context, clientIds [][16]byte, _ uint64, _ uint64, _ uint64) ([]*StFleetBindingState, error) {
	self.bindingInputs = append(self.bindingInputs, len(clientIds))
	bindings := make([]*StFleetBindingState, len(clientIds))
	for index := range clientIds {
		bindings[index] = &StFleetBindingState{Active: index%2 == 0, Generation: 1}
	}
	return bindings, nil
}

func runStV2ScaleHeadExclusion(t *testing.T, providerCount int) {
	// a scale failure is a measurement, not a flake to rerun
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		random := rand.New(rand.NewPCG(uint64(providerCount), 7))
		clientIds := make([][16]byte, providerCount)
		for index := range clientIds {
			clientIds[index] = [16]byte(server.NewId())
		}
		// 20,000 historical bindings that ended before the epoch and 2,000
		// covering it, three quarters of each for observed providers, over
		// two million blocks
		const historical, covering, history = 20_000, 2_000, 2_000_000
		chain := &stV2ScaleChain{stubStClient: newStubStClient(&StEpochState{HeadBlock: history + 200_000})}
		observedCovering := 0
		for index := range historical + covering {
			clientId := [16]byte(server.NewId())
			if index%4 != 0 {
				clientId = clientIds[random.IntN(providerCount)]
				if historical <= index {
					observedCovering++
				}
			}
			from, through := "1", "40"
			if historical <= index {
				from, through = "45", "60"
			}
			data, err := json.Marshal(model.StFleetBoundEventData{ClientId: fmt.Sprintf("0x%x", clientId), FleetId: "0x01", Hotkey: "0x02", Uid: "3", Generation: "1", ValidFromEpoch: from, ValidToEpoch: through})
			if err != nil {
				t.Fatal(err)
			}
			block := uint64(100 + index*(history/(historical+covering)))
			chain.events = append(chain.events, &model.StChainEvent{BlockNumber: block, BlockHash: common.Hash{1}.Hex(), LogIndex: 0, TxHash: common.BigToHash(new(big.Int).SetUint64(block)).Hex(), Kind: model.StFleetBoundEventKind, DataJson: string(data)})
		}
		cfg := &StConfig{ChainId: 945, ContractAddress: common.HexToAddress("0x2000000000000000000000000000000000000002"), DeployBlock: 100}

		memory := startStV2ScaleMemory()
		started := time.Now()
		excluded, err := stHeadExcludedClients(ctx, cfg, chain, 50, history, history+50_400, clientIds)
		elapsed := time.Since(started)
		heap, rss := memory.finish()
		if err != nil {
			t.Fatal(err)
		}
		firstRanges := chain.ranges.Load()
		// the next epoch scans only its own blocks
		nextStarted := time.Now()
		if _, err := stHeadExcludedClients(ctx, cfg, chain, 51, history+50_400, history+100_800, clientIds); err != nil {
			t.Fatal(err)
		}
		nextElapsed := time.Since(nextStarted)
		nextRanges := chain.ranges.Load() - firstRanges
		budget := stV2ScaleHeadExclusionBudget
		t.Logf("SV-6 scale providers=%d fleet_bound=%d first: %s (budget %s) ranges=%d bindingAt_clients=%v excluded=%d heap_growth=%dMiB peak_rss=%dMiB; next epoch: %s ranges=%d",
			providerCount, historical+covering, elapsed.Round(time.Millisecond), budget, firstRanges, chain.bindingInputs, len(excluded), heap>>20, rss>>20, nextElapsed.Round(time.Millisecond), nextRanges)
		// the candidates are the observed covering bindings (a client drawn
		// twice is one candidate), never the population
		if len(chain.bindingInputs) != 2 || observedCovering < chain.bindingInputs[0] || chain.bindingInputs[0] < observedCovering*9/10 || covering < chain.bindingInputs[1] {
			t.Fatalf("bindingAt read %v clients for %d observed covering bindings", chain.bindingInputs, observedCovering)
		}
		if nextRanges != 51 || budget < elapsed {
			t.Fatalf("the first read took %s over %s, or the next epoch scanned %d ranges", elapsed, budget, nextRanges)
		}
	})
}
