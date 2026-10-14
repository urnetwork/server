//go:build scale

// Settlement v2 scale (VERSION2-FINAL.md §15, SV-6): bulk epoch wallet
// selection and batched wallet resolutions at 100k and 1M providers. The whole
// v2 ledger build must fit one hour at 1M providers, inside the 1,200-block
// commit window, so each input here has a share of that hour. Wall time and
// peak memory of each phase are logged; v1's per-provider cost is measured on
// a sample and extrapolated. Run with -tags scale in its own release-gate
// shard.
package model

import (
	"context"
	"os"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
)

// SV-6 at 100k providers.
func TestStV2ScaleWallets100k(t *testing.T) {
	runStV2ScaleWallets(t, 100_000)
}

// SV-6 at 1M providers.
func TestStV2ScaleWallets1M(t *testing.T) {
	runStV2ScaleWallets(t, 1_000_000)
}

// The shares of the one-hour build at 1M providers, scaled linearly to the
// population: selecting from stored chains, verifying every chain once when
// none is stored (the first v2 epoch), and retaining every resolution.
const (
	stV2ScaleWarmBudget       = 10 * time.Minute
	stV2ScaleColdBudget       = 30 * time.Minute
	stV2ScaleResolutionBudget = 5 * time.Minute
	// the selection's own peak heap at 1M providers; the production host
	// must hold twice this beside its other work
	stV2ScaleHeapBudget = 4 << 30
)

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

// The provider mix by network: 70% of networks' providers hold their own
// consent (a tenth of them rotated from epoch 101), 15% earn to a network
// consent, 10% to a hotkey delegation and 5% have no evidence.
func stV2ScaleKind(network int) string {
	switch remainder := network % 20; {
	case remainder < 14:
		return protocol.EarningWalletModeProvider
	case remainder < 17:
		return protocol.EarningWalletModeNetwork
	case remainder < 19:
		return protocol.EarningWalletModeHotkey
	default:
		return ""
	}
}

// Four providers per network.
const stV2ScaleNetworkProviders = 4

func runStV2ScaleWallets(t *testing.T, providerCount int) {
	// a scale failure is a measurement, not a flake to rerun
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		share := float64(providerCount) / 1_000_000
		budget := func(full time.Duration) time.Duration {
			return time.Duration(float64(full) * share)
		}
		f := newStWalletBulkFixture(t)
		networkCount := (providerCount + stV2ScaleNetworkProviders - 1) / stV2ScaleNetworkProviders
		providers := make([]StProviderWalletIdentity, providerCount)
		networkIds := make([]server.Id, networkCount)
		for network := range networkCount {
			networkIds[network] = server.NewId()
		}
		for index := range providers {
			providers[index] = StProviderWalletIdentity{ClientId: server.NewId(), NetworkId: networkIds[index/stV2ScaleNetworkProviders]}
		}

		// the fixture: every chain signed in parallel and retained in chunks
		fixtureStarted := time.Now()
		// a hundred delegating networks share each hotkey and global head
		globals := map[int]stWalletConsentSpec{}
		for network := range networkCount {
			if _, ok := globals[network/2000]; !ok && stV2ScaleKind(network) == protocol.EarningWalletModeHotkey {
				hotkey := uint32(500_000 + network/2000)
				heads := f.hotkeyChain(t, hotkey, stWalletConsentSpec{key: uint32(600_000 + network/2000), from: 95})
				globals[network/2000] = stWalletConsentSpec{key: hotkey, from: 95, consentHead: heads[0], consentGeneration: 1}
			}
		}
		const chunk = 40_000
		for start := 0; start < networkCount; start += chunk {
			end := min(start+chunk, networkCount)
			var next atomic.Int64
			next.Store(int64(start))
			var workers sync.WaitGroup
			for range runtime.GOMAXPROCS(0) {
				workers.Go(func() {
					for network := int(next.Add(1) - 1); network < end; network = int(next.Add(1) - 1) {
						switch stV2ScaleKind(network) {
						case protocol.EarningWalletModeProvider:
							for index := network * stV2ScaleNetworkProviders; index < min((network+1)*stV2ScaleNetworkProviders, providerCount); index++ {
								specs := []stWalletConsentSpec{{key: uint32(index % 1000), from: 95}}
								if index%10 == 0 {
									specs = append(specs, stWalletConsentSpec{key: uint32(1000 + index%1000), from: 101})
								}
								f.providerChain(t, providers[index].ClientId, providers[index].NetworkId, specs...)
							}
						case protocol.EarningWalletModeNetwork:
							f.networkChain(t, networkIds[network], stWalletConsentSpec{key: uint32(2000 + network%1000), from: 95})
						case protocol.EarningWalletModeHotkey:
							f.delegationChain(t, networkIds[network], globals[network/2000])
						}
					}
				})
			}
			workers.Wait()
			f.flush(t)
		}
		fixtureElapsed := time.Since(fixtureStarted)

		// the first epoch verifies every chain once and stores it
		scope := f.scopeAt(100)
		memory := startStV2ScaleMemory()
		coldStarted := time.Now()
		cold, err := GetStRetainedProviderWalletResolutionsForEpoch(ctx, f.root, scope, providers)
		coldElapsed := time.Since(coldStarted)
		coldHeap, coldRss := memory.finish()
		if err != nil {
			t.Fatal(err)
		}
		// every later epoch selects from the stored chains
		memory = startStV2ScaleMemory()
		warmStarted := time.Now()
		warm, err := GetStRetainedProviderWalletResolutionsForEpoch(ctx, f.root, scope, providers)
		warmElapsed := time.Since(warmStarted)
		warmHeap, warmRss := memory.finish()
		if err != nil {
			t.Fatal(err)
		}
		modes := map[string]int{}
		for index, provider := range providers {
			want := stV2ScaleKind(index / stV2ScaleNetworkProviders)
			got := warm[provider.ClientId]
			if want == "" && got != nil || want != "" && (got == nil || got.Mode != want) || got != nil && *got != *cold[provider.ClientId] {
				t.Fatalf("provider %d: selected %+v, want mode %q", index, got, want)
			}
			if got != nil {
				modes[got.Mode]++
			}
		}
		// v1 on an even sample selects the same resolutions
		sample := []StProviderWalletIdentity{}
		for index := 0; index < providerCount; index += providerCount / 1000 {
			sample = append(sample, providers[index])
		}
		v1Started := time.Now()
		v1, v1Err := GetStRetainedProviderWalletsForEpoch(ctx, f.root, scope, sample)
		v1PerProvider := time.Since(v1Started) / time.Duration(len(sample))
		sampled := map[server.Id]*StPayoutWalletResolution{}
		for _, provider := range sample {
			if resolution := warm[provider.ClientId]; resolution != nil {
				sampled[provider.ClientId] = resolution
			}
		}
		requireStWalletResolutionsEqualV1(t, "scale sample", v1, v1Err, sampled, nil)

		// batched resolutions, beside v1's per-provider writer on a sample
		resolutions := make([]*StPayoutWalletResolution, 0, len(warm))
		for _, resolution := range warm {
			resolutions = append(resolutions, resolution)
		}
		memory = startStV2ScaleMemory()
		writeStarted := time.Now()
		if err := AddStPayoutWalletResolutionsBatched(ctx, stResolutionBatchTestKey, 100, 1, resolutions); err != nil {
			t.Fatal(err)
		}
		writeElapsed := time.Since(writeStarted)
		writeHeap, writeRss := memory.finish()
		readStarted := time.Now()
		retained := GetStPayoutWalletResolutions(ctx, stResolutionBatchTestKey, 100, 1)
		readElapsed := time.Since(readStarted)
		if len(retained) != len(resolutions) {
			t.Fatalf("retained %d resolutions, want %d", len(retained), len(resolutions))
		}
		v1Sample := resolutions[:min(10_000, len(resolutions))]
		v1WriteStarted := time.Now()
		if err := AddStPayoutWalletResolutions(ctx, stResolutionBatchTestKey, 101, 1, v1Sample); err != nil {
			t.Fatal(err)
		}
		v1WritePerProvider := time.Since(v1WriteStarted) / time.Duration(len(v1Sample))

		t.Logf("SV-6 scale providers=%d networks=%d mapped=%v fixture=%s", providerCount, networkCount, modes, fixtureElapsed.Round(time.Second))
		t.Logf("SV-6 scale providers=%d selection cold=%s (budget %s) heap_growth=%dMiB peak_rss=%dMiB; warm=%s (budget %s) heap_growth=%dMiB peak_rss=%dMiB; v1 sample=%s/provider, extrapolated %s",
			providerCount, coldElapsed.Round(time.Millisecond), budget(stV2ScaleColdBudget), coldHeap>>20, coldRss>>20, warmElapsed.Round(time.Millisecond), budget(stV2ScaleWarmBudget), warmHeap>>20, warmRss>>20,
			v1PerProvider, (v1PerProvider * time.Duration(providerCount)).Round(time.Second))
		t.Logf("SV-6 scale providers=%d resolutions=%d batched write=%s (budget %s) heap_growth=%dMiB peak_rss=%dMiB read=%s; v1 write sample=%s/provider, extrapolated %s",
			providerCount, len(resolutions), writeElapsed.Round(time.Millisecond), budget(stV2ScaleResolutionBudget), writeHeap>>20, writeRss>>20, readElapsed.Round(time.Millisecond),
			v1WritePerProvider, (v1WritePerProvider * time.Duration(len(resolutions))).Round(time.Second))
		for _, phase := range []struct {
			label   string
			elapsed time.Duration
			budget  time.Duration
		}{
			{"cold selection", coldElapsed, budget(stV2ScaleColdBudget)},
			{"warm selection", warmElapsed, budget(stV2ScaleWarmBudget)},
			{"batched resolutions", writeElapsed, budget(stV2ScaleResolutionBudget)},
		} {
			if phase.budget < phase.elapsed {
				t.Fatalf("%s took %s, over its %s share of the one-hour build", phase.label, phase.elapsed, phase.budget)
			}
		}
		if heapBudget := uint64(float64(stV2ScaleHeapBudget) * share); heapBudget < max(coldHeap, warmHeap, writeHeap) {
			t.Fatalf("peak heap growth %dMiB over its %dMiB budget", max(coldHeap, warmHeap, writeHeap)>>20, heapBudget>>20)
		}
	})
}
