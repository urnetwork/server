package controller

// st_ledger_v2_build_test.go — the server's ledger builder (VERSION2-FINAL.md
// §6, §8.2) with the real database and object store: provider rows from v1's
// inputs, fixed inputs, the allocation walk, previous accounts, the objects,
// and the validator's audit of what it builds.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/urfoundation/sn/ledger"
	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/startifact"
)

var ledgerV2BuildPolicy = [32]byte{9}

// ledgerV2BuildTime is a block's timestamp in the builder tests.
func ledgerV2BuildTime(block uint64) time.Time {
	return time.Unix(1_800_000_000+int64(block)*12, 0).UTC()
}

// Epoch e spans blocks [100e, 100e + 100).
func ledgerV2BuildAuthority(epoch uint64) *StPayoutEpochAuthority {
	start, end := 100*epoch, 100*epoch+100
	return &StPayoutEpochAuthority{Epoch: epoch, PolicyHash: ledgerV2BuildPolicy,
		Start:     protocol.ClientKeyEffectiveBoundary{Block: start, Hash: [32]byte{1, byte(epoch)}, Epoch: epoch},
		End:       protocol.ClientKeyEffectiveBoundary{Block: end, Hash: [32]byte{1, byte(epoch + 1)}, Epoch: epoch + 1},
		StartTime: ledgerV2BuildTime(start), EndTime: ledgerV2BuildTime(end)}
}

// ledgerV2BuildChain answers a new epoch's v1 chain reads: the epoch's
// authenticated window, a FleetBound log at block 5 for each candidate in
// bound, valid for every test epoch, and bindingAt, active for the candidates
// mapped to true.
type ledgerV2BuildChain struct {
	*stubStClient
	authorities    map[uint64]*StPayoutEpochAuthority
	authorityErr   error
	authorityCalls int
	bound          map[[16]byte]bool
	fleetBoundErr  error
	bindingsErr    error
}

func (self *ledgerV2BuildChain) FleetBoundEvents(ctx context.Context, fromBlock uint64, toBlock uint64) ([]*model.StChainEvent, error) {
	if self.fleetBoundErr != nil {
		return nil, self.fleetBoundErr
	}
	events := []*model.StChainEvent{}
	if toBlock < 5 || 5 < fromBlock {
		return events, nil
	}
	candidates := [][16]byte{}
	for clientId := range self.bound {
		candidates = append(candidates, clientId)
	}
	slices.SortFunc(candidates, func(a [16]byte, b [16]byte) int { return strings.Compare(string(a[:]), string(b[:])) })
	for index, clientId := range candidates {
		data, err := json.Marshal(model.StFleetBoundEventData{ClientId: fmt.Sprintf("0x%x", clientId), Generation: "1", ValidFromEpoch: "0", ValidToEpoch: "1000"})
		if err != nil {
			return nil, err
		}
		events = append(events, &model.StChainEvent{BlockNumber: 5, BlockHash: common.Hash{1}.Hex(), LogIndex: index, TxHash: common.Hash{2}.Hex(), Kind: model.StFleetBoundEventKind, DataJson: string(data)})
	}
	return events, nil
}

func (self *ledgerV2BuildChain) PayoutEpochAuthority(ctx context.Context, epoch uint64) (*StPayoutEpochAuthority, error) {
	self.authorityCalls++
	if self.authorityErr != nil {
		return nil, self.authorityErr
	}
	if authority, ok := self.authorities[epoch]; ok {
		return authority, nil
	}
	return ledgerV2BuildAuthority(epoch), nil
}

func (self *ledgerV2BuildChain) BindingsAt(ctx context.Context, clientIds [][16]byte, epoch uint64, startBlock uint64, closeBlock uint64) ([]*StFleetBindingState, error) {
	if self.bindingsErr != nil {
		return nil, self.bindingsErr
	}
	bindings := make([]*StFleetBindingState, len(clientIds))
	for index, clientId := range clientIds {
		bindings[index] = &StFleetBindingState{Active: self.bound[clientId], Generation: 1}
	}
	return bindings, nil
}

// ledgerV2BuildFixture configures one v2 pool whose epochs are built from
// usage and consents retained in the test's database.
type ledgerV2BuildFixture struct {
	t        testing.TB
	ctx      context.Context
	cfg      *StConfig
	chain    *ledgerV2BuildChain
	consents *ledgerV2Consents
}

func newLedgerV2BuildFixture(t testing.TB) *ledgerV2BuildFixture {
	t.Helper()
	return newLedgerV2BuildFixtureAt(t, ledgerV2BuildTime(0))
}

// newLedgerV2BuildFixtureAt schedules sn.yml with the cutoff_utc given.
func newLedgerV2BuildFixtureAt(t testing.TB, cutoff time.Time) *ledgerV2BuildFixture {
	t.Helper()
	cfg := ledgerV2TestConfig()
	key, err := crypto.HexToECDSA(strings.Repeat("47", 32))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Netuid, cfg.PolicyHash, cfg.DeployBlock, cfg.RootKey = 7, ledgerV2BuildPolicy, 1, key
	chain := &ledgerV2BuildChain{stubStClient: newStubStClient(&StEpochState{HeadBlock: 10_000, HeadBlockTime: time.Now().UTC()}), authorities: map[uint64]*StPayoutEpochAuthority{}, bound: map[[16]byte]bool{}}
	SetStConfig(cfg)
	SetStClient(chain)
	SetStLedgerV2ObjectStore(server.NewLocalBlobStore(t.TempDir(), "test"))
	t.Cleanup(func() {
		SetStConfig(nil)
		SetStClient(nil)
		SetStLedgerV2ObjectStore(nil)
	})
	controllerPayoutSchedule(t, cutoff)
	// approved at block 50 of epoch 0; every acceptance expired before epoch 1
	consents := newLedgerV2Consents(t, cfg, ledgerV2BuildPolicy, protocol.ClientKeyEffectiveBoundary{Epoch: 0, Block: 50, Hash: [32]byte{2}}, ledgerV2BuildTime(100))
	return &ledgerV2BuildFixture{t: t, ctx: context.Background(), cfg: cfg, chain: chain, consents: consents}
}

type ledgerV2BuildUsage struct {
	clientId  server.Id
	networkId server.Id
	bytes     int64
}

// contract retains one settled contract closed at closed with the providers'
// usage.
func (self *ledgerV2BuildFixture) contract(closed time.Time, providers ...ledgerV2BuildUsage) {
	providers = slices.Clone(providers)
	slices.SortFunc(providers, func(a ledgerV2BuildUsage, b ledgerV2BuildUsage) int { return a.clientId.Cmp(b.clientId) })
	entries, total := []string{}, int64(0)
	for _, provider := range providers {
		entries = append(entries, fmt.Sprintf(`{"client_id":%q,"network_id":%q,"byte_count":%d}`, provider.clientId.String(), provider.networkId.String(), provider.bytes))
		total += provider.bytes
	}
	usage := fmt.Sprintf(`{"version":1,"byte_count":%d,"providers":[%s]}`, total, strings.Join(entries, ","))
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(self.ctx, `INSERT INTO transfer_contract(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,create_time,close_time,outcome,provider_usage,usage_origin_is_source)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,'settled',$9,true)`, server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId(), total, closed.Add(-time.Hour), closed, usage))
	})
}

// covered is a covered epoch with its funding and recorded rows.
func ledgerV2Covered(epoch uint64, funding uint64, rows *model.StLedgerV2EpochRows) *StLedgerV2CoveredEpoch {
	return &StLedgerV2CoveredEpoch{Epoch: epoch, Funding: *uint256.NewInt(funding), Rows: rows}
}

// publish writes a build as the driver does: its chunks, the newly built
// epochs' records, then its header. It returns the root as vault2 would
// finalize it at index.
func (self *ledgerV2BuildFixture) publish(input *StLedgerV2BuildInput, build *StLedgerV2Build, index uint64) *StLedgerV2FinalizedRoot {
	self.t.Helper()
	tree, err := merkle.BuildLedgerSumTree(self.ctx, build.Accounts)
	if err != nil {
		self.t.Fatal(err)
	}
	if err := stLedgerV2Publish(self.ctx, self.cfg, input, build); err != nil {
		self.t.Fatal(err)
	}
	root, total := tree.Root()
	return &StLedgerV2FinalizedRoot{Index: index, Root: root, Total: total, LeafCount: tree.LeafCount(), Epoch: input.Epoch, LedgerHash: sha256.Sum256(build.Header)}
}

// ledgerV2StoreFetcher serves published objects from the configured store.
type ledgerV2StoreFetcher struct{}

func (ledgerV2StoreFetcher) Fetch(ctx context.Context, ref ledger.Ref) (io.ReadCloser, error) {
	data, err := GetStLedgerV2Object(ctx, ref.String())
	if err != nil {
		return nil, err
	}
	return io.NopCloser(strings.NewReader(string(data))), nil
}

// ledgerV2Heads is the validator's head oracle: per epoch, the candidates
// and whether each is bound.
type ledgerV2Heads map[uint64]map[[16]byte]bool

func (self ledgerV2Heads) HeadBindings(ctx context.Context, epoch uint64) (map[[16]byte]bool, error) {
	return self[epoch], nil
}

// audit runs the validator's §9.2 audit of a published root over the objects
// in the store, with fundedThrough from the input's funding.
func (self *ledgerV2BuildFixture) audit(input *StLedgerV2BuildInput, root *StLedgerV2FinalizedRoot, heads ledgerV2Heads) *ledger.Result {
	self.t.Helper()
	in := ledger.VerifyInput{NoID: input.NoId, FirstEpoch: input.Epochs[0].Epoch, Seq: root.Index + 1, FundedThrough: map[uint64]uint256.Int{},
		Pending: ledger.Root{Epoch: root.Epoch, Root: root.Root, Total: root.Total, LeafCount: root.LeafCount, LedgerHash: root.LedgerHash},
		Fetcher: ledgerV2StoreFetcher{}, Custody: ledger.NewMemoryCustody(), Heads: heads, MaxFetchBytes: ledger.DefaultMaxFetchBytes}
	var funded uint256.Int
	if previous := input.Previous; previous != nil {
		in.Previous = &ledger.Root{Epoch: previous.Epoch, Root: previous.Root, Total: previous.Total, LeafCount: previous.LeafCount, LedgerHash: previous.LedgerHash}
		funded = previous.Total
	}
	for _, covered := range input.Epochs {
		funded.Add(&funded, &covered.Funding)
		in.FundedThrough[covered.Epoch] = funded
	}
	result, err := ledger.Verify(self.ctx, in)
	if err != nil {
		self.t.Fatal(err)
	}
	return result
}

// rows reads a listed epoch's provider rows back from the store.
func (self *ledgerV2BuildFixture) rows(providers *StLedgerV2EpochProviders) []protocol.LedgerUsageRow {
	self.t.Helper()
	chunks := [][]byte{}
	for _, ref := range providers.ProviderChunks {
		chunk, err := GetStLedgerV2Object(self.ctx, ref)
		if err != nil {
			self.t.Fatal(err)
		}
		chunks = append(chunks, chunk)
	}
	rows, err := ledger.DecodeProviderChunks(chunks)
	if err != nil {
		self.t.Fatal(err)
	}
	return rows
}

// resolutionModes reads an epoch's retained wallet resolutions as modes by client.
func (self *ledgerV2BuildFixture) resolutionModes(epoch uint64) map[server.Id]string {
	modes := map[server.Id]string{}
	for _, resolution := range model.GetStPayoutWalletResolutions(self.ctx, self.cfg.DeploymentKey(), epoch, self.cfg.NoId) {
		modes[resolution.ClientId] = resolution.Mode
	}
	return modes
}

func ledgerV2Row(clientId server.Id, coldkey [32]byte, usage uint64, head bool) protocol.LedgerUsageRow {
	return protocol.LedgerUsageRow{ClientID: [16]byte(clientId), Coldkey: coldkey, UsageBytes: usage, HeadExcluded: head}
}

func ledgerV2SortedRows(rows ...protocol.LedgerUsageRow) []protocol.LedgerUsageRow {
	slices.SortFunc(rows, func(a protocol.LedgerUsageRow, b protocol.LedgerUsageRow) int {
		return strings.Compare(string(a.ClientID[:]), string(b.ClientID[:]))
	})
	return rows
}

// ledgerV2SortedAccounts orders new accounts by coldkey, as a root appends them.
func ledgerV2SortedAccounts(accounts ...merkle.LedgerAccount) []merkle.LedgerAccount {
	slices.SortFunc(accounts, func(a merkle.LedgerAccount, b merkle.LedgerAccount) int {
		return strings.Compare(string(a.Coldkey[:]), string(b.Coldkey[:]))
	})
	return accounts
}

func ledgerV2Account(coldkey [32]byte, cumulative uint64) merkle.LedgerAccount {
	return merkle.LedgerAccount{Coldkey: coldkey, Cumulative: *uint256.NewInt(cumulative)}
}

func ledgerV2Amounts(amounts []*model.StLedgerV2Amount) map[string]uint64 {
	out := map[string]uint64{}
	for _, amount := range amounts {
		out[fmt.Sprintf("%d %x", amount.Epoch, amount.Coldkey[:4])] = amount.Amount.Uint64()
	}
	return out
}

// The providers of the main builder fixture, one per selection outcome.
type ledgerV2BuildProviders struct {
	own, network, delegated, unmapped, head, shared server.Id
	networks                                        [5]server.Id
	ownKey, networkKey, hotkeyColdkey, headKey      [32]byte
}

// providers retains, for epochs from 1: a provider with its own consent; one
// whose network consents; one whose network delegates to a hotkey; one
// without any consent; one with its own consent that is head-bound; and one
// sharing the first provider's coldkey.
func (self *ledgerV2BuildFixture) providers() *ledgerV2BuildProviders {
	t := self.t
	p := &ledgerV2BuildProviders{own: server.NewId(), network: server.NewId(), delegated: server.NewId(), unmapped: server.NewId(), head: server.NewId(), shared: server.NewId()}
	for index := range p.networks {
		p.networks[index] = server.NewId()
	}
	self.consents.providerChain(t, p.own, p.networks[0], [2]uint64{11, 1})
	self.consents.networkChain(t, p.networks[1], 12, 1)
	self.consents.delegationChain(t, p.networks[2], 13, self.consents.hotkeyChain(t, 13, 14, 1), 1)
	self.consents.providerChain(t, p.head, p.networks[4], [2]uint64{15, 1})
	self.consents.providerChain(t, p.shared, p.networks[4], [2]uint64{11, 1})
	self.consents.flush(t)
	p.ownKey, p.networkKey, p.hotkeyColdkey, p.headKey = self.consents.public(t, 11), self.consents.public(t, 12), self.consents.public(t, 14), self.consents.public(t, 15)
	self.chain.bound[[16]byte(p.head)] = true
	return p
}

// SV-2, GL-8, §8.2: a first root covering epochs 1 and 2 is built from v1's
// inputs. Rows are exactly the providers with settled bytes in [start, end)
// of each epoch, by client id, with the coldkey of the provider's own
// consent, its network's, its network's hotkey delegation, or zero, and the
// head flag of a binding at either boundary; a contract closed at the start
// counts and one closed at the end belongs to the next epoch. The allocation
// pays shared coldkeys once, the root is tight, every wallet resolution is
// retained, and the validator's audit approves it.
func TestStLedgerV2BuilderBuildsRowsFromV1Inputs(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2BuildFixture(t)
		p := f.providers()
		one, two := ledgerV2BuildAuthority(1), ledgerV2BuildAuthority(2)
		f.contract(one.StartTime, ledgerV2BuildUsage{p.own, p.networks[0], 60}, ledgerV2BuildUsage{p.network, p.networks[1], 200})
		f.contract(one.EndTime.Add(-time.Microsecond), ledgerV2BuildUsage{p.own, p.networks[0], 40})
		f.contract(one.StartTime.Add(-time.Microsecond), ledgerV2BuildUsage{p.own, p.networks[0], 10_000})
		f.contract(one.StartTime.Add(time.Minute), ledgerV2BuildUsage{p.delegated, p.networks[2], 300}, ledgerV2BuildUsage{p.unmapped, p.networks[3], 50},
			ledgerV2BuildUsage{p.head, p.networks[4], 500}, ledgerV2BuildUsage{p.shared, p.networks[4], 400})
		f.contract(two.StartTime, ledgerV2BuildUsage{p.own, p.networks[0], 7})
		f.contract(two.EndTime, ledgerV2BuildUsage{p.network, p.networks[1], 9})

		input := &StLedgerV2BuildInput{NoId: 1, Epoch: 2, Epochs: []*StLedgerV2CoveredEpoch{ledgerV2Covered(1, 1_000_000, nil), ledgerV2Covered(2, 70, nil)}}
		build, err := stLedgerV2Builder{}.Build(f.ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		root := f.publish(input, build, 0)
		var zero [32]byte
		if got, want := f.rows(build.Epochs[0]), ledgerV2SortedRows(ledgerV2Row(p.own, p.ownKey, 100, false), ledgerV2Row(p.network, p.networkKey, 200, false), ledgerV2Row(p.delegated, p.hotkeyColdkey, 300, false),
			ledgerV2Row(p.unmapped, zero, 50, false), ledgerV2Row(p.head, p.headKey, 500, true), ledgerV2Row(p.shared, p.ownKey, 400, false)); !reflect.DeepEqual(got, want) {
			t.Fatalf("epoch 1 rows %+v\nwant %+v", got, want)
		}
		if got := f.rows(build.Epochs[1]); !reflect.DeepEqual(got, []protocol.LedgerUsageRow{ledgerV2Row(p.own, p.ownKey, 7, false)}) {
			t.Fatalf("epoch 2 rows %+v", got)
		}
		if want := ledgerV2SortedAccounts(ledgerV2Account(p.ownKey, 500_070), ledgerV2Account(p.networkKey, 200_000), ledgerV2Account(p.hotkeyColdkey, 300_000)); !reflect.DeepEqual(build.Accounts, want) {
			t.Fatalf("accounts %v", build.Accounts)
		}
		if root.Total.Uint64() != 1_000_070 || root.LeafCount != 3 {
			t.Fatalf("root %+v", root)
		}
		want := map[string]uint64{fmt.Sprintf("1 %x", p.ownKey[:4]): 500_000, fmt.Sprintf("1 %x", p.networkKey[:4]): 200_000, fmt.Sprintf("1 %x", p.hotkeyColdkey[:4]): 300_000, fmt.Sprintf("2 %x", p.ownKey[:4]): 70}
		if got := ledgerV2Amounts(build.Amounts); !reflect.DeepEqual(got, want) {
			t.Fatalf("amounts %v", got)
		}
		for _, amount := range build.Amounts {
			if amount.NoId != 1 {
				t.Fatalf("amount %+v", amount)
			}
		}
		header, err := ledger.DecodeHeader(build.Header)
		if err != nil {
			t.Fatal(err)
		}
		listed := slices.Concat(header.Epochs[0].Providers, header.Epochs[1].Providers, header.Accounts)
		if len(build.Chunks) != len(listed) {
			t.Fatalf("%d unpublished chunks for %d listed objects", len(build.Chunks), len(listed))
		}
		for _, chunk := range build.Chunks {
			if !slices.Contains(listed, ledger.RefOf(chunk)) {
				t.Fatalf("chunk %s is not listed", ledger.RefOf(chunk))
			}
		}
		if records := model.GetStLedgerV2EpochRows(f.ctx, f.cfg.DeploymentKey(), 1, 1, 2); len(records) != 2 || !reflect.DeepEqual(records[1].ProviderChunks, build.Epochs[0].ProviderChunks) {
			t.Fatalf("records %+v", records)
		}
		for epoch, modes := range map[uint64]map[server.Id]string{
			1: {p.own: protocol.EarningWalletModeProvider, p.network: protocol.EarningWalletModeNetwork, p.delegated: protocol.EarningWalletModeHotkey, p.head: protocol.EarningWalletModeProvider, p.shared: protocol.EarningWalletModeProvider},
			2: {p.own: protocol.EarningWalletModeProvider},
		} {
			if got := f.resolutionModes(epoch); !reflect.DeepEqual(got, modes) {
				t.Fatalf("epoch %d retained resolutions %v, want %v", epoch, got, modes)
			}
		}
		if result := f.audit(input, root, ledgerV2Heads{1: {[16]byte(p.head): true}, 2: {}}); result.Check != ledger.CheckNone || result.Action() != ledger.ActionApprove {
			t.Fatalf("audit %s: %v", result.Check, result.Err)
		}
	})
}

// §6.4 (fixed inputs), GL-6: a recorded epoch's rows are read back from its
// published chunks and never rebuilt from v1's inputs, even when its usage
// changes later, and its chunks are not listed as unpublished. Rebuilding a
// finalized root from its records is byte-identical. A later root covers its
// own epochs on the previous root's accounts read back from the store, keeps
// every position, and appends new accounts in coldkey order.
func TestStLedgerV2BuilderReusesRecordedRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2BuildFixture(t)
		p := f.providers()
		f.contract(ledgerV2BuildAuthority(1).StartTime, ledgerV2BuildUsage{p.own, p.networks[0], 100}, ledgerV2BuildUsage{p.network, p.networks[1], 300})
		first := &StLedgerV2BuildInput{NoId: 1, Epoch: 1, Epochs: []*StLedgerV2CoveredEpoch{ledgerV2Covered(1, 4_000, nil)}}
		build, err := stLedgerV2Builder{}.Build(f.ctx, first)
		if err != nil {
			t.Fatal(err)
		}
		root := f.publish(first, build, 0)
		if f.chain.authorityCalls != 1 {
			t.Fatalf("%d authority reads", f.chain.authorityCalls)
		}

		// usage that arrives after the epoch's rows are recorded changes nothing
		f.contract(ledgerV2BuildAuthority(1).StartTime.Add(time.Second), ledgerV2BuildUsage{p.delegated, p.networks[2], 600})
		records := model.GetStLedgerV2EpochRows(f.ctx, f.cfg.DeploymentKey(), 1, 1, 1)
		again := &StLedgerV2BuildInput{NoId: 1, Epoch: 1, Epochs: []*StLedgerV2CoveredEpoch{ledgerV2Covered(1, 4_000, records[1])}}
		rebuilt, err := stLedgerV2Builder{}.Build(f.ctx, again)
		if err != nil {
			t.Fatal(err)
		}
		header, err := ledger.DecodeHeader(rebuilt.Header)
		if err != nil {
			t.Fatal(err)
		}
		if string(rebuilt.Header) != string(build.Header) || !reflect.DeepEqual(rebuilt.Accounts, build.Accounts) || !reflect.DeepEqual(rebuilt.Amounts, build.Amounts) || f.chain.authorityCalls != 1 {
			t.Fatalf("a recorded epoch was rebuilt: authority reads %d", f.chain.authorityCalls)
		}
		if len(rebuilt.Chunks) != len(header.Accounts) || ledger.RefOf(rebuilt.Chunks[0]) != header.Accounts[0] {
			t.Fatalf("unpublished chunks %d for %d account chunks", len(rebuilt.Chunks), len(header.Accounts))
		}

		// epoch 2 on the finalized root: a new coldkey is appended after the
		// previous positions, which keep their coldkeys
		f.contract(ledgerV2BuildAuthority(2).StartTime, ledgerV2BuildUsage{p.own, p.networks[0], 100}, ledgerV2BuildUsage{p.delegated, p.networks[2], 100})
		second := &StLedgerV2BuildInput{NoId: 1, Epoch: 2, Previous: root, Epochs: []*StLedgerV2CoveredEpoch{ledgerV2Covered(2, 1_000, nil)}}
		next, err := stLedgerV2Builder{}.Build(f.ctx, second)
		if err != nil {
			t.Fatal(err)
		}
		previous := slices.Clone(build.Accounts)
		want := append(previous, ledgerV2Account(p.hotkeyColdkey, 500))
		for i := range want {
			if want[i].Coldkey == p.ownKey {
				want[i] = ledgerV2Account(p.ownKey, 1_000+500)
			}
		}
		if !reflect.DeepEqual(next.Accounts, want) {
			t.Fatalf("accounts %v\nwant %v", next.Accounts, want)
		}
		nextRoot := f.publish(second, next, 1)
		if result := f.audit(second, nextRoot, ledgerV2Heads{2: {}}); result.Check != ledger.CheckNone || result.Action() != ledger.ActionApprove {
			t.Fatalf("audit %s: %v", result.Check, result.Err)
		}
	})
}

// GL-5, §6.4 (carry): an epoch declared unrecoverable lists providers: null,
// an epoch without usage lists providers: [], and both carry their funding to
// the next epoch that allocates. A root whose own epoch does not allocate is
// ErrStLedgerV2NoRoot, so the driver commits nothing for it.
func TestStLedgerV2BuilderCarriesNullAndEmptyEpochs(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2BuildFixture(t)
		p := f.providers()
		if err := model.DeclareStLedgerV2EpochUnrecoverable(f.ctx, f.cfg.DeploymentKey(), 1, 1, "operator@example"); err != nil {
			t.Fatal(err)
		}
		f.contract(ledgerV2BuildAuthority(3).StartTime, ledgerV2BuildUsage{p.own, p.networks[0], 10})
		f.contract(ledgerV2BuildAuthority(4).StartTime, ledgerV2BuildUsage{p.unmapped, p.networks[3], 10}, ledgerV2BuildUsage{p.head, p.networks[4], 10})
		records := model.GetStLedgerV2EpochRows(f.ctx, f.cfg.DeploymentKey(), 1, 1, 1)
		input := &StLedgerV2BuildInput{NoId: 1, Epoch: 3, Epochs: []*StLedgerV2CoveredEpoch{ledgerV2Covered(1, 100, records[1]), ledgerV2Covered(2, 20, nil), ledgerV2Covered(3, 3, nil)}}
		build, err := stLedgerV2Builder{}.Build(f.ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		if got := build.Epochs; len(got) != 3 || !got[0].Null || got[0].ProviderChunks != nil || got[1].Null || got[1].ProviderChunks == nil || len(got[1].ProviderChunks) != 0 || len(got[2].ProviderChunks) != 1 {
			t.Fatalf("epochs %+v %+v %+v", got[0], got[1], got[2])
		}
		if !reflect.DeepEqual(build.Accounts, []merkle.LedgerAccount{ledgerV2Account(p.ownKey, 123)}) || !reflect.DeepEqual(ledgerV2Amounts(build.Amounts), map[string]uint64{fmt.Sprintf("3 %x", p.ownKey[:4]): 123}) {
			t.Fatalf("accounts %v amounts %v", build.Accounts, ledgerV2Amounts(build.Amounts))
		}
		if !strings.Contains(string(build.Header), `{"epoch":1,"providers":null},{"epoch":2,"providers":[]}`) {
			t.Fatalf("header %s", build.Header)
		}
		root := f.publish(input, build, 0)
		if result := f.audit(input, root, ledgerV2Heads{2: {}, 3: {}}); result.Check != ledger.CheckNone || result.Action() != ledger.ActionApprove {
			t.Fatalf("audit %s: %v", result.Check, result.Err)
		}

		// epoch 4's rows are all ineligible: unmapped or head-bound
		input = &StLedgerV2BuildInput{NoId: 1, Epoch: 4, Previous: root, Epochs: []*StLedgerV2CoveredEpoch{ledgerV2Covered(4, 50, nil)}}
		if _, err := (stLedgerV2Builder{}).Build(f.ctx, input); !errors.Is(err, ErrStLedgerV2NoRoot) || err.Error() != ErrStLedgerV2NoRoot.Error()+": epoch 4" {
			t.Fatalf("an unallocated epoch built a root: %v", err)
		}
	})
}

// §8.2: Accounts reads a published ledger's account rows; a missing header or
// account chunk, an object that is not a header, and account chunks of the
// wrong shape are each refused, and Build refuses a previous root it cannot
// read or whose accounts do not rebuild it.
func TestStLedgerV2BuilderReadsPublishedAccounts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2BuildFixture(t)
		p := f.providers()
		f.contract(ledgerV2BuildAuthority(1).StartTime, ledgerV2BuildUsage{p.own, p.networks[0], 100})
		input := &StLedgerV2BuildInput{NoId: 1, Epoch: 1, Epochs: []*StLedgerV2CoveredEpoch{ledgerV2Covered(1, 10, nil)}}
		build, err := stLedgerV2Builder{}.Build(f.ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		root := f.publish(input, build, 0)
		if accounts, err := (stLedgerV2Builder{}).Accounts(f.ctx, root.LedgerHash); err != nil || !reflect.DeepEqual(accounts, build.Accounts) {
			t.Fatalf("accounts %v %v", accounts, err)
		}
		put := func(data []byte) [32]byte {
			if _, err := PutStLedgerV2Object(f.ctx, data); err != nil {
				t.Fatal(err)
			}
			return sha256.Sum256(data)
		}
		headerOf := func(accounts ...ledger.Ref) [32]byte {
			data, err := ledger.EncodeHeader(&ledger.Header{NoID: 1, Epoch: 1, Epochs: []ledger.HeaderEpoch{{Epoch: 1}}, Accounts: accounts})
			if err != nil {
				t.Fatal(err)
			}
			return put(data)
		}
		for _, test := range []struct {
			name       string
			ledgerHash [32]byte
			want       error
		}{
			{"missing header", [32]byte{7}, nil},
			{"not a header", put([]byte("not a header")), ledger.ErrHeaderDecode},
			{"missing account chunk", headerOf(ledger.Ref{8}), nil},
			{"malformed account chunk", headerOf(ledger.Ref(put(make([]byte, 47)))), ledger.ErrChunkLength},
		} {
			_, err := stLedgerV2Builder{}.Accounts(f.ctx, test.ledgerHash)
			if test.want == nil && !errors.Is(err, startifact.ErrLedgerObjectNotFound) || test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("%s: %v", test.name, err)
			}
		}

		missing := *root
		missing.LedgerHash = [32]byte{7}
		if _, err := (stLedgerV2Builder{}).Build(f.ctx, &StLedgerV2BuildInput{NoId: 1, Epoch: 2, Previous: &missing, Epochs: []*StLedgerV2CoveredEpoch{ledgerV2Covered(2, 10, nil)}}); !errors.Is(err, startifact.ErrLedgerObjectNotFound) {
			t.Fatalf("an unreadable previous root: %v", err)
		}
		lowered := *root
		lowered.Total.AddUint64(&lowered.Total, 1)
		if _, err := (stLedgerV2Builder{}).Build(f.ctx, &StLedgerV2BuildInput{NoId: 1, Epoch: 2, Previous: &lowered, Epochs: []*StLedgerV2CoveredEpoch{ledgerV2Covered(2, 10, nil)}}); !errors.Is(err, ledger.ErrPrevious) {
			t.Fatalf("a previous root its accounts do not rebuild: %v", err)
		}
	})
}

// §6.4 (fixed inputs): a recorded epoch whose chunk is missing from the store,
// or does not decode as provider rows, is refused rather than rebuilt.
func TestStLedgerV2BuilderRefusesUnreadableRecords(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2BuildFixture(t)
		malformed, err := PutStLedgerV2Object(f.ctx, make([]byte, 56))
		if err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			name   string
			chunks []string
			want   error
		}{
			{"missing chunk", []string{ledger.Ref{9}.String()}, startifact.ErrLedgerObjectNotFound},
			{"malformed chunk", []string{malformed}, ledger.ErrChunkLength},
		} {
			rows := &model.StLedgerV2EpochRows{Epoch: 1, ProviderChunks: test.chunks}
			_, err := stLedgerV2Builder{}.Build(f.ctx, &StLedgerV2BuildInput{NoId: 1, Epoch: 1, Epochs: []*StLedgerV2CoveredEpoch{ledgerV2Covered(1, 10, rows)}})
			if !errors.Is(err, test.want) || !strings.Contains(err.Error(), "st ledger v2: epoch 1: ") || f.chain.authorityCalls != 0 {
				t.Fatalf("%s: %v (authority reads %d)", test.name, err, f.chain.authorityCalls)
			}
		}
	})
}

// ledgerV2PushSchedule replaces sn.yml with another cutoff_utc without
// preparing its earning boundary.
func ledgerV2PushSchedule(cutoff time.Time) func() {
	data := fmt.Sprintf("schema: urnetwork-provider-payout-transition-v1\ncutoff_utc: %q\nattribution: settled_contract_close_time\nlegacy_usdc: finish_pre_cutoff_obligations\nmainnet:\n  profile: mainnet\n  chain_id: 964\n  genesis_hash: %q\n  netuid: 25\n  activation: blocked\n", cutoff.UTC().Format(time.RFC3339Nano), "0x"+strings.Repeat("11", 32))
	return server.Config.PushSimpleResource("sn.yml", []byte(data))
}

// SV-2, §8.2: each refusal of a new epoch's rows, one at a time: st not
// configured; the epoch's authority unreadable, absent, of another epoch or
// of another policy; wallet selection refused for the configured subnet; the
// FleetBound scan or bindingAt failing; a wallet resolution that differs from
// the one retained for the epoch; a provider_work.yml that does not parse; a
// configured roster signer without the epoch's roster, which holds the epoch
// as v1 does; an earning policy that differs from its prepared boundary; and
// a usage census the window refuses.
func TestStLedgerV2BuilderRefusesUnbuildableRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2BuildFixture(t)
		p := f.providers()
		f.contract(ledgerV2BuildAuthority(1).StartTime, ledgerV2BuildUsage{p.own, p.networks[0], 100}, ledgerV2BuildUsage{p.head, p.networks[4], 100})
		refused := func(name string, want error, text string) {
			t.Helper()
			_, err := stLedgerV2Builder{}.Build(f.ctx, &StLedgerV2BuildInput{NoId: 1, Epoch: 1, Epochs: []*StLedgerV2CoveredEpoch{ledgerV2Covered(1, 10, nil)}})
			if err == nil || want != nil && !errors.Is(err, want) || !strings.Contains(err.Error(), text) {
				t.Fatalf("%s: %v", name, err)
			}
		}

		SetStConfig(nil)
		SetStClient(nil)
		refused("st not configured", errStNotConfigured, "")
		SetStConfig(f.cfg)
		SetStClient(f.chain)

		f.chain.authorityErr = errors.New("authority unavailable")
		refused("authority unreadable", nil, "authority unavailable")
		f.chain.authorityErr = nil
		otherPolicy := ledgerV2BuildAuthority(1)
		otherPolicy.PolicyHash = [32]byte{8}
		for name, authority := range map[string]*StPayoutEpochAuthority{"absent authority": nil, "another epoch's authority": ledgerV2BuildAuthority(2), "another policy's authority": otherPolicy} {
			f.chain.authorities[1] = authority
			refused(name, nil, "st ledger v2: the payout authority of epoch 1 is absent or differs")
		}
		delete(f.chain.authorities, 1)

		f.cfg.Netuid = 0
		refused("unusable subnet", protocol.ErrWalletMappingIntegrity, "")
		f.cfg.Netuid = 7

		f.chain.fleetBoundErr = errors.New("FleetBound logs unavailable")
		refused("FleetBound scan", nil, "FleetBound logs unavailable")
		f.chain.fleetBoundErr = nil
		f.chain.bindingsErr = errors.New("bindingAt unavailable")
		refused("bindingAt", nil, "bindingAt unavailable")
		f.chain.bindingsErr = nil

		conflicting := &model.StPayoutWalletResolution{ClientId: p.own, NetworkId: p.networks[0], Mode: protocol.EarningWalletModeProvider, Coldkey: [32]byte{9}, ConsentHash: [32]byte{9}, ConsentGeneration: 1, HeadHash: [32]byte{9}, HeadGeneration: 1}
		if err := model.AddStPayoutWalletResolutionsBatched(f.ctx, f.cfg.DeploymentKey(), 1, 1, []*model.StPayoutWalletResolution{conflicting}); err != nil {
			t.Fatal(err)
		}
		refused("a differing retained resolution", protocol.ErrWalletMappingIntegrity, "")

		restore := server.Vault.PushSimpleResource("provider_work.yml", []byte("schema: [unclosed"))
		refused("unparsable provider_work.yml", nil, "")
		restore()
		artifactKey, err := crypto.HexToECDSA(strings.Repeat("48", 32))
		if err != nil {
			t.Fatal(err)
		}
		f.cfg.ArtifactKey = artifactKey
		root := crypto.PubkeyToAddress(f.cfg.RootKey.PublicKey)
		restore = server.Vault.PushSimpleResource("provider_work.yml", []byte(fmt.Sprintf("schema: %s\ndomain_hash: %x\nrequest_public_key: %x\nauthority_signer: %s\nclient_key_root_signer: %s\n", ProviderWorkPolicySchema, f.consents.domainHash, [32]byte{83}, common.Address{84}.Hex(), root.Hex())))
		refused("a configured roster signer without the epoch's roster", protocol.ErrWalletMappingUnavailable, "")
		restore()

		restore = ledgerV2PushSchedule(ledgerV2BuildTime(1))
		refused("an earning policy unlike its boundary", server.ErrProviderEarningBoundaryMismatch, "")
		restore()

		empty := ledgerV2BuildAuthority(1)
		empty.EndTime = empty.StartTime
		f.chain.authorities[1] = empty
		refused("a census of an empty window", nil, "invalid subnet usage window")
	})
}

// SV-2 (cutoff clip), §6.2: every v2 epoch starts after the cutoff_utc, and
// the builder refuses an epoch whose earning window the cutoff would clip,
// one ending or starting before it; an epoch starting at the cutoff or after
// it is built.
func TestStLedgerV2BuilderRefusesClippedWindows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := ledgerV2BuildTime(250)
		f := newLedgerV2BuildFixtureAt(t, cutoff)
		p := f.providers()
		atCutoff := ledgerV2BuildAuthority(4)
		atCutoff.StartTime = cutoff
		f.chain.authorities[4] = atCutoff
		f.contract(ledgerV2BuildTime(300), ledgerV2BuildUsage{p.own, p.networks[0], 100})
		for _, test := range []struct {
			name    string
			epoch   uint64
			clipped bool
		}{
			{"ends before the cutoff", 1, true},
			{"starts before the cutoff", 2, true},
			{"starts after the cutoff", 3, false},
			{"starts at the cutoff", 4, false},
		} {
			_, err := stLedgerV2Builder{}.Build(f.ctx, &StLedgerV2BuildInput{NoId: 1, Epoch: test.epoch, Epochs: []*StLedgerV2CoveredEpoch{ledgerV2Covered(test.epoch, 10, nil)}})
			if test.clipped != errors.Is(err, ErrStLedgerV2WindowClipped) || !test.clipped && err != nil {
				t.Fatalf("%s: %v", test.name, err)
			}
			if test.clipped && !strings.Contains(err.Error(), fmt.Sprintf("epoch %d starts at %s, before %s", test.epoch, ledgerV2BuildAuthority(test.epoch).StartTime.Format(time.RFC3339), cutoff.Format(time.RFC3339))) {
				t.Fatalf("%s: %v", test.name, err)
			}
		}
	})
}

// §6.4 (fixed inputs): the operator's rebuild command records, naming who
// made it, a request to build an epoch's recorded rows or null declaration
// again, and changes nothing else. It is refused without settlement v2, when
// vault2 cannot be read, for an epoch the latest finalized root covers,
// without a requester, and for an epoch with nothing recorded. A root built
// for the request reads the epoch's rows from v1's inputs again, counting
// usage added since; the wallet resolutions retained for the epoch stay, and
// the new rows replace the record and end the request.
func TestStLedgerV2RebuildEpoch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2BuildFixture(t)
		chain := newFakeLedgerV2Chain(&[]string{})
		SetStLedgerV2Client(chain)
		t.Cleanup(func() { SetStLedgerV2Client(nil) })
		key := f.cfg.DeploymentKey()
		p := f.providers()
		f.contract(ledgerV2BuildAuthority(1).StartTime, ledgerV2BuildUsage{p.own, p.networks[0], 100})
		input := &StLedgerV2BuildInput{NoId: 1, Epoch: 1, Epochs: []*StLedgerV2CoveredEpoch{ledgerV2Covered(1, 10, nil)}}
		build, err := stLedgerV2Builder{}.Build(f.ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		f.publish(input, build, 0)
		if err := model.DeclareStLedgerV2EpochUnrecoverable(f.ctx, key, 1, 3, "operator@example"); err != nil {
			t.Fatal(err)
		}
		records := func() map[uint64]*model.StLedgerV2EpochRows {
			return model.GetStLedgerV2EpochRows(f.ctx, key, 1, 1, 3)
		}
		before := records()

		off := *f.cfg
		off.SettlementV2, off.VaultV2 = common.Address{}, common.Address{}
		SetStConfig(&off)
		if err := StLedgerV2RebuildEpoch(f.ctx, 1, "rebuilder@example"); !errors.Is(err, errStLedgerV2NotConfigured) {
			t.Fatalf("v2 off: %v", err)
		}
		SetStConfig(f.cfg)
		chain.failNext("state", errors.New("state unreadable"))
		if err := StLedgerV2RebuildEpoch(f.ctx, 1, "rebuilder@example"); err == nil || err.Error() != "state unreadable" {
			t.Fatalf("state: %v", err)
		}
		chain.roots = []*StLedgerV2FinalizedRoot{{Index: 0, Epoch: 1}}
		if err := StLedgerV2RebuildEpoch(f.ctx, 1, "rebuilder@example"); !errors.Is(err, ErrStLedgerV2RebuildRefused) || err.Error() != ErrStLedgerV2RebuildRefused.Error()+": epoch 1 is covered by finalized root 0" {
			t.Fatalf("finalized: %v", err)
		}
		chain.roots = nil
		if err := StLedgerV2RebuildEpoch(f.ctx, 1, ""); err == nil || err.Error() != "st ledger v2: a rebuild request must name who makes it" {
			t.Fatalf("no requester: %v", err)
		}
		if err := StLedgerV2RebuildEpoch(f.ctx, 2, "rebuilder@example"); !errors.Is(err, ErrStLedgerV2RebuildRefused) || err.Error() != ErrStLedgerV2RebuildRefused.Error()+": epoch 2 has no recorded provider rows" {
			t.Fatalf("unrecorded: %v", err)
		}
		if !reflect.DeepEqual(records(), before) {
			t.Fatalf("a refused rebuild changed a record: %+v", records())
		}
		for _, epoch := range []uint64{1, 3} {
			if err := StLedgerV2RebuildEpoch(f.ctx, epoch, "rebuilder@example"); err != nil {
				t.Fatalf("rebuild epoch %d: %v", epoch, err)
			}
		}
		requested := records()
		for _, epoch := range []uint64{1, 3} {
			record, earlier := *requested[epoch], *before[epoch]
			earlier.RebuildRequestedBy = "rebuilder@example"
			if !reflect.DeepEqual(record, earlier) {
				t.Fatalf("requested epoch %d %+v", epoch, record)
			}
		}

		// the root built for the request reads epoch 1's rows again
		f.contract(ledgerV2BuildAuthority(1).StartTime.Add(time.Second), ledgerV2BuildUsage{p.network, p.networks[1], 300})
		calls := f.chain.authorityCalls
		again := &StLedgerV2BuildInput{NoId: 1, Epoch: 1, Epochs: []*StLedgerV2CoveredEpoch{ledgerV2Covered(1, 8, nil)}}
		rebuilt, err := stLedgerV2Builder{}.Build(f.ctx, again)
		if err != nil {
			t.Fatal(err)
		}
		if f.chain.authorityCalls != calls+1 || !reflect.DeepEqual(rebuilt.Accounts, ledgerV2SortedAccounts(ledgerV2Account(p.ownKey, 2), ledgerV2Account(p.networkKey, 6))) {
			t.Fatalf("rebuilt accounts %v after %d authority reads", rebuilt.Accounts, f.chain.authorityCalls-calls)
		}
		f.publish(again, rebuilt, 0)
		if record := records()[1]; !reflect.DeepEqual(record.ProviderChunks, rebuilt.Epochs[0].ProviderChunks) || reflect.DeepEqual(record.ProviderChunks, before[1].ProviderChunks) || record.RebuildRequestedBy != "" {
			t.Fatalf("rebuilt record %+v", record)
		}
		if modes := f.resolutionModes(1); !reflect.DeepEqual(modes, map[server.Id]string{p.own: protocol.EarningWalletModeProvider, p.network: protocol.EarningWalletModeNetwork}) {
			t.Fatalf("retained resolutions %v", modes)
		}
	})
}

// §8.2 (server start): a process installs the builder only when settlement
// v2 is configured and st is enabled.
func TestStLedgerV2BuilderInstallsOnlyWithV2(t *testing.T) {
	defer SetStConfig(nil)
	defer SetStLedgerBuilder(nil)
	off := ledgerV2TestConfig()
	off.SettlementV2, off.VaultV2 = common.Address{}, common.Address{}
	disabled := ledgerV2TestConfig()
	disabled.Enabled = false
	for name, cfg := range map[string]*StConfig{"v2 off": off, "st disabled": disabled} {
		SetStConfig(cfg)
		if InstallStLedgerBuilder() || stLedgerBuilder() != nil {
			t.Fatalf("%s installed a builder", name)
		}
	}
	SetStConfig(ledgerV2TestConfig())
	if !InstallStLedgerBuilder() || stLedgerBuilder() != (stLedgerV2Builder{}) {
		t.Fatalf("builder %v", stLedgerBuilder())
	}
}
