package controller

// st_ledger_v2_recovery_test.go — the phase C recovery component
// (VERSION2-FINAL.md §16 step 6) in the operator server: the preserved usage
// artifact, the recorded source rows, the plan read from the event mirror and
// vault1, and the receiving root's build, which the validator's audit
// approves only with exactly that component.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/holiman/uint256"

	"github.com/urfoundation/sn/ledger"
	"github.com/urfoundation/sn/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/startifact"
)

const (
	// ledgerV2RecoveryAmount is the source epoch's v1 funding, allocated by
	// its rows; ledgerV2RecoveryOrdinary is the rest of the swept surplus,
	// another missed epoch's share and reveal-lag stake, which is ordinary.
	ledgerV2RecoveryAmount   = 4_132_715_621_039
	ledgerV2RecoveryOrdinary = 3_496_017_057_872
	ledgerV2RecoveryCapture  = 1_450_000_000_000
)

// useLedgerV2RecoverySchedule gives the configured pool a schedule that
// recovers source epoch 1.
func useLedgerV2RecoverySchedule(t testing.TB) {
	t.Helper()
	stLedgerV2RecoverySchedule = func(vault [20]byte, noID uint64) *ledger.RecoverySchedule {
		return &ledger.RecoverySchedule{Vault: vault, NoID: noID, Sources: []ledger.Recovery{{Epoch: 1, Amount: ledgerV2RecoveryAmount}}, FromBlock: 1}
	}
	t.Cleanup(func() { stLedgerV2RecoverySchedule = ledger.ScheduledRecovery })
}

func ledgerV2RecoveryUsages(n int) []*model.StProviderUsage {
	usages := make([]*model.StProviderUsage, n)
	for i := range usages {
		usages[i] = &model.StProviderUsage{NetworkId: server.NewId(), PayoutByteCount: int64(i * 7)}
		usages[i].ClientId[15] = byte(i + 1)
		usages[i].ClientId[14] = byte((i + 1) >> 8)
		usages[i].ClientId[13] = byte((i + 1) >> 16)
	}
	return usages
}

// The usage artifact round-trips through a directory: the manifest names
// every chunk by its sha256, chunks hold ledger.ChunkRows rows but the last,
// and anything that is not exactly what was written is refused.
func TestStLedgerV2RecoveryUsageArtifact(t *testing.T) {
	cfg := ledgerV2TestConfig()
	authority := ledgerV2BuildAuthority(1)
	usages := ledgerV2RecoveryUsages(ledger.ChunkRows + 3)
	usage, err := stLedgerV2RecoveryArtifact(cfg, authority, usages)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage.Chunks) != 2 || len(usage.Chunks[0]) != ledger.ChunkRows*stLedgerV2RecoveryUsageRowBytes || len(usage.Chunks[1]) != 3*stLedgerV2RecoveryUsageRowBytes {
		t.Fatalf("chunk shapes %d, %d", len(usage.Chunks[0]), len(usage.Chunks[1]))
	}
	manifest := usage.Manifest
	if manifest.Schema != StLedgerV2RecoveryUsageSchema || manifest.Epoch != 1 || manifest.NoId != cfg.NoId || manifest.DeploymentId != cfg.DeploymentId ||
		manifest.Start.Block != 100 || manifest.End.Block != 200 || manifest.Rows != uint64(len(usages)) || manifest.Start.Time != authority.StartTime.Format(time.RFC3339Nano) {
		t.Fatalf("manifest %+v", manifest)
	}
	var total uint64
	for _, u := range usages {
		total += uint64(u.PayoutByteCount)
	}
	if manifest.PayoutBytes != fmt.Sprint(total) || usage.Ref != startifact.LedgerObjectRef(usage.ManifestBytes) {
		t.Fatalf("manifest bytes %s, ref %s", manifest.PayoutBytes, usage.Ref)
	}
	dir := filepath.Join(t.TempDir(), "usage")
	if err := WriteStLedgerV2RecoveryUsage(usage, dir); err != nil {
		t.Fatal(err)
	}
	// Writing the same artifact again is a no-op.
	if err := WriteStLedgerV2RecoveryUsage(usage, dir); err != nil {
		t.Fatal(err)
	}
	read, readUsages, err := ReadStLedgerV2RecoveryUsage(context.Background(), usage.Ref, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(readUsages, usages) || !reflect.DeepEqual(read.Manifest, manifest) || !slices.EqualFunc(read.Chunks, usage.Chunks, func(a, b []byte) bool { return string(a) == string(b) }) {
		t.Fatal("the artifact does not read back as written")
	}
	// The same census builds the same artifact.
	if again, err := stLedgerV2RecoveryArtifact(cfg, authority, usages); err != nil || again.Ref != usage.Ref {
		t.Fatalf("not deterministic: %v", err)
	}

	refuse := func(name string, mutate func(dir string)) {
		t.Run(name, func(t *testing.T) {
			copied := filepath.Join(t.TempDir(), "usage")
			if err := WriteStLedgerV2RecoveryUsage(usage, copied); err != nil {
				t.Fatal(err)
			}
			mutate(copied)
			if _, _, err := ReadStLedgerV2RecoveryUsage(context.Background(), usage.Ref, copied); err == nil {
				t.Fatal("read")
			}
		})
	}
	refuse("a chunk changed", func(dir string) {
		path := recoveryUsageFile(dir, manifest.Chunks[1])
		data, _ := os.ReadFile(path)
		data[len(data)-1]++
		os.WriteFile(path, data, 0o600)
	})
	refuse("a chunk missing", func(dir string) { os.Remove(recoveryUsageFile(dir, manifest.Chunks[0])) })
	refuse("the manifest missing", func(dir string) { os.Remove(recoveryUsageFile(dir, usage.Ref)) })

	// A manifest that is not canonical, or does not count its chunks'
	// rows, is refused even when it names itself correctly.
	for name, edit := range map[string]func(m *StLedgerV2RecoveryUsageManifest) []byte{
		"spacing": func(m *StLedgerV2RecoveryUsageManifest) []byte {
			data, _ := json.MarshalIndent(m, "", " ")
			return data
		},
		"row count": func(m *StLedgerV2RecoveryUsageManifest) []byte {
			copied := *m
			copied.Rows++
			data, _ := json.Marshal(&copied)
			return data
		},
		"other schema": func(m *StLedgerV2RecoveryUsageManifest) []byte {
			copied := *m
			copied.Schema = "urnetwork-recovery-usage-v0"
			data, _ := json.Marshal(&copied)
			return data
		},
		"unknown field": func(m *StLedgerV2RecoveryUsageManifest) []byte {
			data, _ := json.Marshal(m)
			return append([]byte(`{"extra":1,`), data[1:]...)
		},
	} {
		t.Run(name, func(t *testing.T) {
			copied := filepath.Join(t.TempDir(), "usage")
			if err := WriteStLedgerV2RecoveryUsage(usage, copied); err != nil {
				t.Fatal(err)
			}
			data := edit(manifest)
			ref := startifact.LedgerObjectRef(data)
			if err := os.WriteFile(recoveryUsageFile(copied, ref), data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := ReadStLedgerV2RecoveryUsage(context.Background(), ref, copied); !errors.Is(err, ErrStLedgerV2RecoveryRefused) {
				t.Fatalf("%v", err)
			}
		})
	}

	// A census that is not one usage per provider in client order is refused.
	for name, mutate := range map[string]func([]*model.StProviderUsage){
		"unsorted":      func(u []*model.StProviderUsage) { u[0], u[1] = u[1], u[0] },
		"repeated":      func(u []*model.StProviderUsage) { u[1].ClientId = u[0].ClientId },
		"zero network":  func(u []*model.StProviderUsage) { u[2].NetworkId = server.Id{} },
		"zero client":   func(u []*model.StProviderUsage) { u[0].ClientId = server.Id{} },
		"negative byte": func(u []*model.StProviderUsage) { u[2].PayoutByteCount = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := ledgerV2RecoveryUsages(4)
			mutate(bad)
			if _, err := stLedgerV2RecoveryArtifact(cfg, authority, bad); !errors.Is(err, ErrStLedgerV2RecoveryRefused) {
				t.Fatalf("%v", err)
			}
		})
	}
	// An empty census is an artifact without chunks, and reads back empty.
	empty, err := stLedgerV2RecoveryArtifact(cfg, authority, nil)
	if err != nil || len(empty.Chunks) != 0 || empty.Manifest.Rows != 0 || empty.Manifest.Chunks == nil {
		t.Fatalf("empty artifact %+v: %v", empty, err)
	}
	emptyDir := filepath.Join(t.TempDir(), "empty")
	if err := WriteStLedgerV2RecoveryUsage(empty, emptyDir); err != nil {
		t.Fatal(err)
	}
	if _, got, err := ReadStLedgerV2RecoveryUsage(context.Background(), empty.Ref, emptyDir); err != nil || len(got) != 0 {
		t.Fatalf("empty read %v: %v", got, err)
	}
}

// ledgerV2RecoveryScenario retains the providers' usage in source epoch 1 and
// the covered epochs 2 and 3, records epoch 1's rows from its preserved
// artifact, and shows vault1's RootMissed entitlement of epoch 1.
type ledgerV2RecoveryScenario struct {
	*ledgerV2BuildFixture
	p      *ledgerV2BuildProviders
	usage  *StLedgerV2RecoveryUsage
	dir    string
	record *StLedgerV2RecoveryRecord
}

func newLedgerV2RecoveryScenario(t testing.TB, sourceUsage bool) *ledgerV2RecoveryScenario {
	t.Helper()
	useLedgerV2RecoverySchedule(t)
	s := &ledgerV2RecoveryScenario{ledgerV2BuildFixture: newLedgerV2BuildFixture(t)}
	s.p = s.providers()
	p := s.p
	if sourceUsage {
		s.contract(ledgerV2BuildAuthority(1).StartTime, ledgerV2BuildUsage{p.own, p.networks[0], 100}, ledgerV2BuildUsage{p.network, p.networks[1], 300},
			ledgerV2BuildUsage{p.unmapped, p.networks[3], 50}, ledgerV2BuildUsage{p.head, p.networks[4], 500}, ledgerV2BuildUsage{p.shared, p.networks[4], 400})
	}
	s.contract(ledgerV2BuildAuthority(2).StartTime, ledgerV2BuildUsage{p.own, p.networks[0], 60}, ledgerV2BuildUsage{p.delegated, p.networks[2], 40})
	s.contract(ledgerV2BuildAuthority(3).StartTime, ledgerV2BuildUsage{p.network, p.networks[1], 70}, ledgerV2BuildUsage{p.delegated, p.networks[2], 30})
	s.chain.pool = &StPoolState{Status: ledger.V1EpochRootMissed, PoolTotalRao: big.NewInt(ledgerV2RecoveryAmount), ClaimedRao: big.NewInt(0)}
	var err error
	if s.usage, err = BuildStLedgerV2RecoveryUsage(s.ctx, 1); err != nil {
		t.Fatal(err)
	}
	s.dir = filepath.Join(t.TempDir(), "usage")
	if err := WriteStLedgerV2RecoveryUsage(s.usage, s.dir); err != nil {
		t.Fatal(err)
	}
	if s.record, err = StLedgerV2RecordRecoveryRows(s.ctx, 1, s.usage.Ref, s.dir, "recorder@example"); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *ledgerV2RecoveryScenario) sourceRecord() *model.StLedgerV2EpochRows {
	return model.GetStLedgerV2EpochRows(s.ctx, s.cfg.DeploymentKey(), s.cfg.NoId, 1, 1)[1]
}

// input is the receiving root's input: epochs 2 and 3, the swept surplus and
// epoch 1's recovery in epoch 3.
func (s *ledgerV2RecoveryScenario) input(recovery bool) *StLedgerV2BuildInput {
	input := &StLedgerV2BuildInput{NoId: 1, Epoch: 3, Epochs: []*StLedgerV2CoveredEpoch{
		ledgerV2Covered(2, 1_000_000, nil),
		ledgerV2Covered(3, ledgerV2RecoveryCapture+ledgerV2RecoveryAmount+ledgerV2RecoveryOrdinary, nil),
	}}
	if recovery {
		input.Epochs[1].Recovery = []*StLedgerV2Recovery{{Recovery: ledger.Recovery{Epoch: 1, Amount: ledgerV2RecoveryAmount}, Rows: s.sourceRecord()}}
	}
	return input
}

// audit runs the validator's audit with the recovery the chain calls for.
func (s *ledgerV2RecoveryScenario) audit(input *StLedgerV2BuildInput, root *StLedgerV2FinalizedRoot, recoveries map[uint64][]ledger.Recovery) *ledger.Result {
	s.t.Helper()
	in := ledger.VerifyInput{NoID: input.NoId, FirstEpoch: input.Epochs[0].Epoch, Seq: root.Index + 1, FundedThrough: map[uint64]uint256.Int{},
		Pending: ledger.Root{Epoch: root.Epoch, Root: root.Root, Total: root.Total, LeafCount: root.LeafCount, LedgerHash: root.LedgerHash},
		Fetcher: ledgerV2StoreFetcher{}, Custody: ledger.NewMemoryCustody(), Heads: ledgerV2Heads{1: {[16]byte(s.p.head): true}, 2: {}, 3: {}}, MaxFetchBytes: ledger.DefaultMaxFetchBytes, Recoveries: recoveries}
	var funded uint256.Int
	for _, covered := range input.Epochs {
		funded.Add(&funded, &covered.Funding)
		in.FundedThrough[covered.Epoch] = funded
	}
	result, err := ledger.Verify(s.ctx, in)
	if err != nil {
		s.t.Fatal(err)
	}
	return result
}

func ledgerV2RecoveryExpected() map[uint64][]ledger.Recovery {
	return map[uint64][]ledger.Recovery{3: {{Epoch: 1, Amount: ledgerV2RecoveryAmount}}}
}

// The record command builds source epoch 1's rows with the v2 rules from
// the preserved usage: each provider's consent-selected coldkey, the head
// flag of a binding at the source epoch's boundaries, the wallet
// resolutions retained; it publishes the chunks and records them once.
func TestStLedgerV2RecoveryRecordsSourceRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		s := newLedgerV2RecoveryScenario(t, true)
		p := s.p
		var zero [32]byte
		record := s.sourceRecord()
		if record == nil || record.Null() || !slices.Equal(record.ProviderChunks, s.record.ProviderChunks) {
			t.Fatalf("record %+v", record)
		}
		want := ledgerV2SortedRows(ledgerV2Row(p.own, p.ownKey, 100, false), ledgerV2Row(p.network, p.networkKey, 300, false),
			ledgerV2Row(p.unmapped, zero, 50, false), ledgerV2Row(p.head, p.headKey, 500, true), ledgerV2Row(p.shared, p.ownKey, 400, false))
		if got := s.rows(&StLedgerV2EpochProviders{ProviderChunks: record.ProviderChunks}); !reflect.DeepEqual(got, want) {
			t.Fatalf("source rows %+v\nwant %+v", got, want)
		}
		if s.record.Rows != 5 || s.record.EligibleRows != 3 || s.record.Coldkeys != 2 || s.record.EligibleBytes.Uint64() != 800 || s.record.PayoutBytes.Uint64() != 1_350 {
			t.Fatalf("summary %+v", s.record)
		}
		if modes := s.resolutionModes(1); len(modes) != 4 || modes[p.network] != protocol.EarningWalletModeNetwork {
			t.Fatalf("retained resolutions %v", modes)
		}
		// Recording the same rows again changes nothing.
		if again, err := StLedgerV2RecordRecoveryRows(s.ctx, 1, s.usage.Ref, s.dir, "recorder@example"); err != nil || !slices.Equal(again.ProviderChunks, record.ProviderChunks) {
			t.Fatalf("repeat: %v", err)
		}
		// The usage artifact also reads from the blob store's recovery prefix,
		// which the public ledger object route does not serve.
		if err := StoreStLedgerV2RecoveryUsage(s.ctx, s.usage); err != nil {
			t.Fatal(err)
		}
		if _, usages, err := ReadStLedgerV2RecoveryUsage(s.ctx, s.usage.Ref, ""); err != nil || len(usages) != 5 {
			t.Fatalf("stored artifact %d: %v", len(usages), err)
		}
		if _, err := GetStLedgerV2Object(s.ctx, s.usage.Ref); !errors.Is(err, startifact.ErrLedgerObjectNotFound) {
			t.Fatalf("the usage manifest is served as a ledger object: %v", err)
		}
	})
}

// The record command refuses what it must not record: an epoch the schedule
// does not recover, usage of another epoch, an artifact that is not the
// census over the epoch's authenticated boundaries, a record without a name,
// and other rows once rows are recorded.
func TestStLedgerV2RecoveryRecordRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		s := newLedgerV2RecoveryScenario(t, true)
		if _, err := StLedgerV2RecordRecoveryRows(s.ctx, 2, s.usage.Ref, s.dir, "recorder@example"); !errors.Is(err, ErrStLedgerV2RecoveryRefused) {
			t.Fatalf("an unscheduled epoch: %v", err)
		}
		if _, err := BuildStLedgerV2RecoveryUsage(s.ctx, 2); !errors.Is(err, ErrStLedgerV2RecoveryRefused) {
			t.Fatalf("usage of an unscheduled epoch: %v", err)
		}
		if _, err := StLedgerV2RecordRecoveryRows(s.ctx, 1, s.usage.Ref, s.dir, ""); !errors.Is(err, ErrStLedgerV2RecoveryRefused) {
			t.Fatalf("a nameless record: %v", err)
		}
		// An artifact whose boundaries are not the chain's.
		moved := ledgerV2BuildAuthority(1)
		moved.End.Hash[0] ^= 1
		_, usages, err := ReadStLedgerV2RecoveryUsage(s.ctx, s.usage.Ref, s.dir)
		if err != nil {
			t.Fatal(err)
		}
		other, err := stLedgerV2RecoveryArtifact(s.cfg, moved, usages)
		if err != nil {
			t.Fatal(err)
		}
		otherDir := filepath.Join(t.TempDir(), "other")
		if err := WriteStLedgerV2RecoveryUsage(other, otherDir); err != nil {
			t.Fatal(err)
		}
		if _, err := StLedgerV2RecordRecoveryRows(s.ctx, 1, other.Ref, otherDir, "recorder@example"); !errors.Is(err, ErrStLedgerV2RecoveryRefused) {
			t.Fatalf("an artifact over other boundaries: %v", err)
		}
		// A census with another provider's usage builds other rows, which the
		// recorded epoch refuses.
		usages = append(usages, &model.StProviderUsage{ClientId: server.Id{0xff}, NetworkId: s.p.networks[0], PayoutByteCount: 9})
		changed, err := stLedgerV2RecoveryArtifact(s.cfg, ledgerV2BuildAuthority(1), usages)
		if err != nil {
			t.Fatal(err)
		}
		changedDir := filepath.Join(t.TempDir(), "changed")
		if err := WriteStLedgerV2RecoveryUsage(changed, changedDir); err != nil {
			t.Fatal(err)
		}
		if _, err := StLedgerV2RecordRecoveryRows(s.ctx, 1, changed.Ref, changedDir, "recorder@example"); !errors.Is(err, model.ErrStLedgerV2EpochRowsFixed) {
			t.Fatalf("other rows after the record: %v", err)
		}
	})
}

// The receiving root: epoch 1's share is allocated exactly by epoch 1's
// recorded rows; the rest of the swept surplus is ordinary funding of epoch
// 3, whose rows allocate f_3 − R. The recovery's amounts are epoch 1's, and
// the validator approves the root only with the component the chain calls for.
func TestStLedgerV2BuilderBuildsRecovery(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		s := newLedgerV2RecoveryScenario(t, true)
		p := s.p
		input := s.input(true)
		build, err := stLedgerV2Builder{}.Build(s.ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		listed := build.Epochs[1].Recovery
		if len(listed) != 1 || listed[0].Epoch != 1 || listed[0].Amount != ledgerV2RecoveryAmount || listed[0].Null || !slices.Equal(listed[0].ProviderChunks, s.sourceRecord().ProviderChunks) || len(build.Epochs[0].Recovery) != 0 {
			t.Fatalf("listed recovery %+v", listed)
		}
		amounts := map[uint64]map[[32]byte]uint64{}
		for _, amount := range build.Amounts {
			if amounts[amount.Epoch] == nil {
				amounts[amount.Epoch] = map[[32]byte]uint64{}
			}
			amounts[amount.Epoch][amount.Coldkey] = amount.Amount.Uint64()
		}
		// The recovery leaves sum exactly to epoch 1's share, split by the
		// source rows: the own and shared clients' 500 bytes and the network's
		// 300; the unmapped and head-bound providers get nothing.
		var recovered uint64
		for _, value := range amounts[1] {
			recovered += value
		}
		sourceRows := s.rows(&StLedgerV2EpochProviders{ProviderChunks: s.sourceRecord().ProviderChunks})
		wantRecovery, err := protocol.AllocateLedgerEpoch(s.ctx, uint256.NewInt(ledgerV2RecoveryAmount), sourceRows)
		if err != nil {
			t.Fatal(err)
		}
		if recovered != ledgerV2RecoveryAmount || len(amounts[1]) != 2 || amounts[1][p.ownKey] != wantRecovery[slices.IndexFunc(wantRecovery, func(d protocol.LedgerDelta) bool { return d.Coldkey == p.ownKey })].Delta.Uint64() {
			t.Fatalf("recovery amounts %v sum %d", amounts[1], recovered)
		}
		if _, ok := amounts[1][p.headKey]; ok {
			t.Fatal("the head-bound provider was paid by the recovery")
		}
		// Epoch 3's own rows allocate the capture plus the ordinary surplus.
		var ordinary uint64
		for _, value := range amounts[3] {
			ordinary += value
		}
		if ordinary != ledgerV2RecoveryCapture+ledgerV2RecoveryOrdinary {
			t.Fatalf("epoch 3 allocated %d, want f_3 − R = %d", ordinary, ledgerV2RecoveryCapture+ledgerV2RecoveryOrdinary)
		}
		root := s.publish(input, build, 0)
		if root.Total.Uint64() != 1_000_000+ledgerV2RecoveryCapture+ledgerV2RecoveryAmount+ledgerV2RecoveryOrdinary {
			t.Fatalf("total %s", root.Total.Dec())
		}
		// The commitment check accepts the build against its inputs ...
		funded := root.Total
		if _, _, err := stLedgerV2Commitment(s.ctx, input, build, &funded); err != nil {
			t.Fatal(err)
		}
		// ... and refuses a build that lists other recovery components.
		without := s.input(false)
		if _, _, err := stLedgerV2Commitment(s.ctx, without, build, &funded); err == nil {
			t.Fatal("the commitment accepted a recovery its input does not carry")
		}
		altered := *build.Epochs[1].Recovery[0]
		altered.Amount++
		build.Epochs[1].Recovery[0] = &altered
		if _, _, err := stLedgerV2Commitment(s.ctx, input, build, &funded); err == nil {
			t.Fatal("the commitment accepted another recovery amount")
		}
		// The validator approves the root with the recovery the chain calls
		// for, and refuses it when the chain calls for none.
		if result := s.audit(input, root, ledgerV2RecoveryExpected()); result.Check != ledger.CheckNone || result.Action() != ledger.ActionApprove {
			t.Fatalf("audit %s: %v", result.Check, result.Err)
		}
		if result := s.audit(input, root, nil); result.Check != ledger.CheckRecovery || result.Action() != ledger.ActionRefuse {
			t.Fatalf("audit without a recovery: %s %v", result.Check, result.Err)
		}
	})
}

// The zero-row edge case: a source epoch without usage records no rows, its
// share stays in epoch 3's ordinary funding, and the validator approves.
func TestStLedgerV2BuilderRecoveryWithoutSourceRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		s := newLedgerV2RecoveryScenario(t, false)
		record := s.sourceRecord()
		if record == nil || record.Null() || len(record.ProviderChunks) != 0 || s.record.Rows != 0 {
			t.Fatalf("record %+v", record)
		}
		input := s.input(true)
		build, err := stLedgerV2Builder{}.Build(s.ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		var ordinary uint64
		for _, amount := range build.Amounts {
			if amount.Epoch == 1 {
				t.Fatalf("a recovery amount without source rows: %+v", amount)
			}
			if amount.Epoch == 3 {
				ordinary += amount.Amount.Uint64()
			}
		}
		if ordinary != ledgerV2RecoveryCapture+ledgerV2RecoveryAmount+ledgerV2RecoveryOrdinary {
			t.Fatalf("epoch 3 allocated %d, want all of f_3", ordinary)
		}
		root := s.publish(input, build, 0)
		if result := s.audit(input, root, ledgerV2RecoveryExpected()); result.Check != ledger.CheckNone || result.Action() != ledger.ActionApprove {
			t.Fatalf("audit %s: %v", result.Check, result.Err)
		}
	})
}

// A receiving root whose source rows are not recorded cannot be built: the
// driver retries and raises the alert, and never builds the source epoch's
// rows itself.
func TestStLedgerV2BuilderRefusesUnrecordedRecovery(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2BuildFixture(t)
		p := f.providers()
		f.contract(ledgerV2BuildAuthority(3).StartTime, ledgerV2BuildUsage{p.own, p.networks[0], 70})
		input := &StLedgerV2BuildInput{NoId: 1, Epoch: 3, Epochs: []*StLedgerV2CoveredEpoch{ledgerV2Covered(3, ledgerV2RecoveryAmount+1, nil)}}
		input.Epochs[0].Recovery = []*StLedgerV2Recovery{{Recovery: ledger.Recovery{Epoch: 1, Amount: ledgerV2RecoveryAmount}}}
		authorityCalls := f.chain.authorityCalls
		if _, err := (stLedgerV2Builder{}).Build(f.ctx, input); !errors.Is(err, ErrStLedgerV2RecoveryRows) {
			t.Fatalf("%v", err)
		}
		if f.chain.authorityCalls != authorityCalls {
			t.Fatal("the build read an epoch's authority before refusing")
		}
	})
}

// The plan comes from the chain as the server mirrors it: the pool's
// SurplusAssigned events through the receiving epoch's funding, which the
// mirror must have applied, placed at the first epoch whose surplus reaches
// the scheduled total, each source checked against vault1.
func TestStLedgerV2RecoveryPlanReadsTheMirror(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2BuildFixture(t)
		useLedgerV2RecoverySchedule(t)
		key := f.cfg.DeploymentKey()
		f.chain.pool = &StPoolState{Status: ledger.V1EpochRootMissed, PoolTotalRao: big.NewInt(ledgerV2RecoveryAmount), ClaimedRao: big.NewInt(0)}
		surplusEvent := func(block uint64, index int, noId uint64, epoch uint64, amount uint64) *model.StChainEvent {
			data, err := json.Marshal(map[string]string{"no_id": fmt.Sprint(noId), "e": fmt.Sprint(epoch), "amount": fmt.Sprint(amount)})
			if err != nil {
				t.Fatal(err)
			}
			return &model.StChainEvent{BlockNumber: block, BlockHash: fmt.Sprintf("0x%064x", block), LogIndex: index, TxHash: fmt.Sprintf("0x%064x", block*10+uint64(index)), Kind: model.StLedgerV2SurplusAssignedEventKind, DataJson: string(data)}
		}
		// A small unrelated assignment to epoch 2, another pool's surplus,
		// then the sweep's two assignments to epoch 3.
		model.UpsertStEvents(f.ctx, key, []*model.StChainEvent{
			surplusEvent(250, 0, 1, 2, 5),
			surplusEvent(260, 0, 2, 3, ledgerV2RecoveryAmount),
			surplusEvent(270, 0, 1, 3, ledgerV2RecoveryAmount),
			surplusEvent(271, 1, 1, 3, ledgerV2RecoveryOrdinary),
		})
		for epoch, block := range map[uint64]uint64{2: 205, 3: 305} {
			funding := &model.StLedgerV2Funding{NoId: 1, Epoch: epoch, BlockNumber: block}
			model.SetStLedgerV2Funding(f.ctx, key, funding)
		}
		// The mirror has not passed epoch 3's funding.
		model.SetStChainCheckpoint(f.ctx, key, model.StChainCheckpoint{NextBlock: 305, BlockHash: "0x01"})
		if _, err := stLedgerV2RecoveryPlan(f.ctx, f.cfg, 2, 1, 3); !errors.Is(err, ErrStLedgerV2RecoveryMirror) {
			t.Fatalf("a lagging mirror: %v", err)
		}
		model.SetStChainCheckpoint(f.ctx, key, model.StChainCheckpoint{NextBlock: 306, BlockHash: "0x02"})
		plan, err := stLedgerV2RecoveryPlan(f.ctx, f.cfg, 2, 1, 3)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(plan, ledgerV2RecoveryExpected()) {
			t.Fatalf("plan %v", plan)
		}
		// A root that does not reach epoch 3, or one after a root covering
		// it, carries nothing.
		for _, c := range []struct{ p, e uint64 }{{1, 2}, {3, 4}} {
			if c.e == 4 {
				model.SetStLedgerV2Funding(f.ctx, key, &model.StLedgerV2Funding{NoId: 1, Epoch: 4, BlockNumber: 305})
			}
			if plan, err := stLedgerV2RecoveryPlan(f.ctx, f.cfg, 2, c.p, c.e); err != nil || len(plan) != 0 {
				t.Fatalf("root over (%d, %d]: %v %v", c.p, c.e, plan, err)
			}
		}
		// vault1 must show the source RootMissed with exactly its funding.
		f.chain.pool = &StPoolState{Status: 1, PoolTotalRao: big.NewInt(0), ClaimedRao: big.NewInt(0)}
		if _, err := stLedgerV2RecoveryPlan(f.ctx, f.cfg, 2, 1, 3); !errors.Is(err, ledger.ErrRecoverySource) {
			t.Fatalf("an unconfirmed source: %v", err)
		}
		f.chain.pool = &StPoolState{Status: ledger.V1EpochRootMissed, PoolTotalRao: big.NewInt(ledgerV2RecoveryAmount - 1), ClaimedRao: big.NewInt(0)}
		if _, err := stLedgerV2RecoveryPlan(f.ctx, f.cfg, 2, 1, 3); !errors.Is(err, ledger.ErrRecoverySource) {
			t.Fatalf("another source amount: %v", err)
		}
		// A pool without a schedule reads nothing, whatever the mirror holds.
		stLedgerV2RecoverySchedule = ledger.ScheduledRecovery
		if plan, err := stLedgerV2RecoveryPlan(f.ctx, f.cfg, 2, 1, 3); err != nil || plan != nil {
			t.Fatalf("an unscheduled pool: %v %v", plan, err)
		}
		// A mirrored event that does not parse refuses the plan.
		useLedgerV2RecoverySchedule(t)
		f.chain.pool = &StPoolState{Status: ledger.V1EpochRootMissed, PoolTotalRao: big.NewInt(ledgerV2RecoveryAmount), ClaimedRao: big.NewInt(0)}
		model.UpsertStEvents(f.ctx, key, []*model.StChainEvent{{BlockNumber: 280, BlockHash: "0x03", LogIndex: 0, TxHash: "0x04", Kind: model.StLedgerV2SurplusAssignedEventKind, DataJson: `{"no_id":"1","e":"x","amount":"1"}`}})
		if _, err := stLedgerV2RecoveryPlan(f.ctx, f.cfg, 2, 1, 3); err == nil || !strings.Contains(err.Error(), "does not parse") {
			t.Fatalf("an unparsable event: %v", err)
		}
	})
}
