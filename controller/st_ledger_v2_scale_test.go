//go:build scale

package controller

// st_ledger_v2_scale_test.go — SV-6 through the operator server's real
// settlement v2 builder (VERSION2-FINAL.md §8.2, §15) at 100k and 1M
// providers: a pool's first root, built from the usage census, bulk wallet
// selection over consent chains verified once and head exclusion by FleetBound
// candidates, then published and recorded into the account index; the tree
// readers cache; and the next epoch's root on the first root's accounts. Each
// build with its publication must fit its share of one hour, well inside the
// 1,200-block commit window. Wall time and peak memory of each phase are
// logged. Run with -tags scale in its own release-gate shard.

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/holiman/uint256"

	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// SV-6 at 100k providers.
func TestStV2ScaleLedgerBuilder100k(t *testing.T) {
	runStV2ScaleLedgerBuilder(t, 100_000)
}

// SV-6 at 1M providers.
func TestStV2ScaleLedgerBuilder1M(t *testing.T) {
	runStV2ScaleLedgerBuilder(t, 1_000_000)
}

const (
	// One root's build and publication at 1M providers, scaled linearly to
	// the population: a quarter of the 1,200-block (four-hour) commit window.
	// Recording a finalized root and building the readers' tree have the same
	// share.
	stV2ScaleLedgerBudget = time.Hour
	// A phase's peak heap growth at 1M providers; the production host must
	// hold twice this beside its other work.
	stV2ScaleLedgerHeapBudget = 8 << 30
	// Providers per network and per settled contract, and every how many
	// providers one is bound to a head fleet.
	stV2ScaleLedgerNetworkProviders  = 4
	stV2ScaleLedgerContractProviders = 16
	stV2ScaleLedgerHeadEvery         = 500
)

// The providers of a network earn to their own consents (70% of networks),
// the network's consent (15%), the network's hotkey delegation (10%), or
// nothing (5%). Each provider's own consent names its own coldkey, so the
// ledger holds about three quarters of a million accounts at 1M providers.
func stV2ScaleLedgerKind(network int) string {
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

// Each provider's settled bytes in an epoch.
func stV2ScaleLedgerUsage(epoch uint64, index int) int64 {
	return int64(1_000*epoch) + int64(index%7_919)
}

// stV2ScaleLedgerContracts retains settled contracts closed at closed, each
// carrying the usage of stV2ScaleLedgerContractProviders providers.
func stV2ScaleLedgerContracts(t testing.TB, ctx context.Context, closed time.Time, epoch uint64, clientIds []server.Id, networkIds []server.Id) {
	t.Helper()
	const contractsPerInsert = 2_000
	providers := stV2ScaleLedgerContractProviders
	for start := 0; start < len(clientIds); start += providers * contractsPerInsert {
		end := min(start+providers*contractsPerInsert, len(clientIds))
		count := (end - start + providers - 1) / providers
		ids := make([][]server.Id, 5)
		for column := range ids {
			ids[column] = make([]server.Id, count)
		}
		totals, usages := make([]int64, count), make([]string, count)
		for contract := range count {
			members := make([]int, 0, providers)
			for index := start + contract*providers; index < min(start+(contract+1)*providers, end); index++ {
				members = append(members, index)
			}
			slices.SortFunc(members, func(a int, b int) int { return clientIds[a].Cmp(clientIds[b]) })
			var entries strings.Builder
			for position, index := range members {
				if 0 < position {
					entries.WriteByte(',')
				}
				fmt.Fprintf(&entries, `{"client_id":%q,"network_id":%q,"byte_count":%d}`, clientIds[index].String(), networkIds[index/stV2ScaleLedgerNetworkProviders].String(), stV2ScaleLedgerUsage(epoch, index))
				totals[contract] += stV2ScaleLedgerUsage(epoch, index)
			}
			usages[contract] = fmt.Sprintf(`{"version":1,"byte_count":%d,"providers":[%s]}`, totals[contract], entries.String())
			for column := range ids {
				ids[column][contract] = server.NewId()
			}
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,create_time,close_time,outcome,provider_usage,usage_origin_is_source)
				SELECT c, sn, s, dn, d, b, $1, $2, 'settled', u::jsonb, true FROM unnest($3::uuid[], $4::uuid[], $5::uuid[], $6::uuid[], $7::uuid[], $8::bigint[], $9::text[]) AS t(c, sn, s, dn, d, b, u)`,
				closed.Add(-time.Hour), closed, ids[0], ids[1], ids[2], ids[3], ids[4], totals, usages))
		})
	}
}

// A measured phase: wall time against its share of the budget, and memory.
type stV2ScaleLedgerPhase struct {
	label   string
	elapsed time.Duration
	heap    uint64
	rss     uint64
}

func stV2ScaleLedgerMeasure(label string, phases *[]stV2ScaleLedgerPhase, run func()) {
	memory := startStV2ScaleMemory()
	started := time.Now()
	run()
	phase := stV2ScaleLedgerPhase{label: label, elapsed: time.Since(started)}
	phase.heap, phase.rss = memory.finish()
	*phases = append(*phases, phase)
}

func runStV2ScaleLedgerBuilder(t *testing.T, providerCount int) {
	// a scale failure is a measurement, not a flake to rerun
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newLedgerV2BuildFixture(t)
		ctx, key := f.ctx, f.cfg.DeploymentKey()
		share := float64(providerCount) / 1_000_000
		budget := time.Duration(float64(stV2ScaleLedgerBudget) * share)
		heapBudget := uint64(float64(stV2ScaleLedgerHeapBudget) * share)

		// the fixture: every chain signed in parallel and retained in chunks,
		// with each provider's expected coldkey
		fixtureStarted := time.Now()
		networkCount := (providerCount + stV2ScaleLedgerNetworkProviders - 1) / stV2ScaleLedgerNetworkProviders
		clientIds, networkIds := make([]server.Id, providerCount), make([]server.Id, networkCount)
		for index := range clientIds {
			clientIds[index] = server.NewId()
		}
		for network := range networkIds {
			networkIds[network] = server.NewId()
		}
		providersOf := func(network int) []int {
			indexes := []int{}
			for index := network * stV2ScaleLedgerNetworkProviders; index < min((network+1)*stV2ScaleLedgerNetworkProviders, providerCount); index++ {
				indexes = append(indexes, index)
			}
			return indexes
		}
		// a hundred delegating networks share each hotkey and its global head
		type global struct {
			hotkey, coldkey uint32
			head            [32]byte
		}
		globals := map[int]global{}
		for network := range networkCount {
			if _, ok := globals[network/2_000]; !ok && stV2ScaleLedgerKind(network) == protocol.EarningWalletModeHotkey {
				g := global{hotkey: uint32(3_000_000 + network/2_000), coldkey: uint32(3_100_000 + network/2_000)}
				g.head = f.consents.hotkeyChain(t, g.hotkey, g.coldkey, 1)
				globals[network/2_000] = g
			}
		}
		expected := make([][32]byte, providerCount)
		const chunk = 40_000
		for start := 0; start < networkCount; start += chunk {
			end := min(start+chunk, networkCount)
			var next atomic.Int64
			next.Store(int64(start))
			var workers sync.WaitGroup
			for range runtime.GOMAXPROCS(0) {
				workers.Go(func() {
					for network := int(next.Add(1) - 1); network < end; network = int(next.Add(1) - 1) {
						switch stV2ScaleLedgerKind(network) {
						case protocol.EarningWalletModeProvider:
							for _, index := range providersOf(network) {
								seed := uint32(1 + index)
								f.consents.providerChain(t, clientIds[index], networkIds[network], [2]uint64{uint64(seed), 1})
								expected[index] = f.consents.public(t, seed)
							}
						case protocol.EarningWalletModeNetwork:
							seed := uint32(2_000_000 + network)
							f.consents.networkChain(t, networkIds[network], seed, 1)
							for _, index := range providersOf(network) {
								expected[index] = f.consents.public(t, seed)
							}
						case protocol.EarningWalletModeHotkey:
							g := globals[network/2_000]
							f.consents.delegationChain(t, networkIds[network], g.hotkey, g.head, 1)
							for _, index := range providersOf(network) {
								expected[index] = f.consents.public(t, g.coldkey)
							}
						}
					}
				})
			}
			workers.Wait()
			f.consents.flush(t)
		}
		// the signing keys are the fixture's, not the builder's memory
		f.consents.keys = nil
		for epoch := uint64(1); epoch <= 2; epoch++ {
			stV2ScaleLedgerContracts(t, ctx, ledgerV2BuildAuthority(epoch).StartTime.Add(time.Minute), epoch, clientIds, networkIds)
		}
		for index := 0; index < providerCount; index += stV2ScaleLedgerHeadEvery {
			f.chain.bound[[16]byte(clientIds[index])] = true
		}
		fixtureElapsed := time.Since(fixtureStarted)

		// the driver's chain for the record steps: vault2 funded both epochs
		chain := newFakeLedgerV2Chain(&[]string{})
		chain.firstEpoch = 1
		funding := []uint64{1_000_000_000_000, 2_000_000_000_000}
		chain.funded[1], chain.funded[2] = *uint256.NewInt(funding[0]), *uint256.NewInt(funding[0] + funding[1])
		SetStLedgerV2Client(chain)
		t.Cleanup(func() { SetStLedgerV2Client(nil) })

		SetStLedgerBuilder(stLedgerV2Builder{})
		t.Cleanup(func() { SetStLedgerBuilder(nil) })

		phases := []stV2ScaleLedgerPhase{}
		var previous *StLedgerV2FinalizedRoot
		var previousAccounts []merkle.LedgerAccount
		for epoch := uint64(1); epoch <= 2; epoch++ {
			input := &StLedgerV2BuildInput{NoId: 1, Epoch: epoch, Previous: previous, Epochs: []*StLedgerV2CoveredEpoch{ledgerV2Covered(epoch, funding[epoch-1], nil)}}
			var build *StLedgerV2Build
			var root *StLedgerV2FinalizedRoot
			stV2ScaleLedgerMeasure(fmt.Sprintf("root %d build and publication", epoch), &phases, func() {
				var err error
				if build, err = (stLedgerV2Builder{}).Build(ctx, input); err != nil {
					t.Fatal(err)
				}
				root = f.publish(input, build, epoch-1)
			})

			// every provider has its row: its usage, its selected coldkey and
			// its head flag; the eligible coldkeys hold every account
			position := make(map[[16]byte]int, providerCount)
			for index, clientId := range clientIds {
				position[[16]byte(clientId)] = index
			}
			rows := f.rows(build.Epochs[0])
			accounts := map[[32]byte]bool{}
			for _, row := range rows {
				index, ok := position[row.ClientID]
				head := index%stV2ScaleLedgerHeadEvery == 0
				if !ok || row.UsageBytes != uint64(stV2ScaleLedgerUsage(epoch, index)) || row.Coldkey != expected[index] || row.HeadExcluded != head {
					t.Fatalf("epoch %d provider %d row %+v", epoch, index, row)
				}
				if row.Coldkey != ([32]byte{}) && !head {
					accounts[row.Coldkey] = true
				}
			}
			var total uint256.Int
			for epochIndex := range epoch {
				total.AddUint64(&total, funding[epochIndex])
			}
			if len(rows) != providerCount || root.LeafCount != uint64(len(accounts)) || !root.Total.Eq(&total) {
				t.Fatalf("epoch %d: %d rows, root %+v for %d coldkeys", epoch, len(rows), root, len(accounts))
			}
			// previous positions keep their coldkeys and never decrease
			for position, account := range previousAccounts {
				if next := build.Accounts[position]; next.Coldkey != account.Coldkey || next.Cumulative.Lt(&account.Cumulative) {
					t.Fatalf("epoch %d account %d %+v, was %+v", epoch, position, next, account)
				}
			}
			previousAccounts = build.Accounts

			// the finalized root is recorded into the account index
			index := epoch - 1
			mirrored := &model.StLedgerV2Root{NoId: 1, Seq: epoch, Epoch: epoch, Root: root.Root, Total: root.Total, LeafCount: root.LeafCount, LedgerHash: root.LedgerHash, RootIndex: &index}
			model.AddStLedgerV2Commit(ctx, key, mirrored, 100*epoch+110, 0)
			model.SetStLedgerV2RootFinalized(ctx, key, mirrored, 100*epoch+150)
			stV2ScaleLedgerMeasure(fmt.Sprintf("root %d record", epoch), &phases, func() {
				outcome, err := stLedgerV2RunRecord(ctx, f.cfg, chain, epoch)
				if err != nil || outcome.Status != model.StPublishStatusConfirmed {
					t.Fatalf("record root %d: %+v %v", index, outcome, err)
				}
			})
			if count := model.GetStLedgerV2AccountCount(ctx, key, 1); count != root.LeafCount {
				t.Fatalf("the account index holds %d accounts for %d leaves", count, root.LeafCount)
			}

			// readers build the committed tree from its account chunks once
			resetLedgerV2Caches()
			stV2ScaleLedgerMeasure(fmt.Sprintf("root %d reader tree", epoch), &phases, func() {
				if _, accounts, err := StLedgerV2Tree(ctx, root.LedgerHash, root.Root, &root.Total, root.LeafCount); err != nil || uint64(len(accounts)) != root.LeafCount {
					t.Fatalf("tree of root %d: %d accounts, %v", index, len(accounts), err)
				}
			})
			cachedStarted := time.Now()
			if _, _, err := StLedgerV2Tree(ctx, root.LedgerHash, root.Root, &root.Total, root.LeafCount); err != nil {
				t.Fatal(err)
			}
			if cached := time.Since(cachedStarted); time.Second < cached {
				t.Fatalf("the cached tree of root %d took %s", index, cached)
			}
			previous = root
		}

		resolutions := len(model.GetStPayoutWalletResolutions(ctx, key, 2, 1))
		t.Logf("SV-6 scale providers=%d networks=%d accounts=%d resolutions=%d heads=%d fixture=%s", providerCount, networkCount, previous.LeafCount, resolutions, len(f.chain.bound), fixtureElapsed.Round(time.Second))
		for _, phase := range phases {
			t.Logf("SV-6 scale providers=%d %s=%s (budget %s) heap_growth=%dMiB peak_rss=%dMiB", providerCount, phase.label, phase.elapsed.Round(time.Millisecond), budget, phase.heap>>20, phase.rss>>20)
		}
		for _, phase := range phases {
			if budget < phase.elapsed || heapBudget < phase.heap {
				t.Fatalf("%s took %s and %dMiB, over its %s and %dMiB share", phase.label, phase.elapsed, phase.heap>>20, budget, heapBudget>>20)
			}
		}
	})
}
