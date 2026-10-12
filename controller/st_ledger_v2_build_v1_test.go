package controller

// st_ledger_v2_build_v1_test.go — SV-2: one epoch through v1's release payout
// and through the settlement v2 builder, on the same usage, consents, roster
// and head binding (VERSION2-FINAL.md §6.2, §8.2).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/urfoundation/sn/ledger"
	"github.com/urfoundation/sn/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/startifact"
)

// ledgerV2V1Chain is the payout policy fixture's chain with providers bound
// to a head fleet: bindingAt for v1, and FleetBound logs at block 150 for v2's
// candidates.
type ledgerV2V1Chain struct {
	*CoreStClient
	bound map[[16]byte]bool
}

func (self *ledgerV2V1Chain) BindingsAt(ctx context.Context, clientIds [][16]byte, epoch uint64, startBlock uint64, closeBlock uint64) ([]*StFleetBindingState, error) {
	bindings := make([]*StFleetBindingState, len(clientIds))
	for index, clientId := range clientIds {
		bindings[index] = &StFleetBindingState{Active: self.bound[clientId], Generation: 1}
	}
	return bindings, nil
}

func (self *ledgerV2V1Chain) FinalizedHead(ctx context.Context) (uint64, [32]byte, time.Time, error) {
	return 275, [32]byte{1}, stPayoutPolicyTime(275), nil
}

func (self *ledgerV2V1Chain) BlockHashes(ctx context.Context, blocks []uint64) ([][32]byte, error) {
	hashes := make([][32]byte, len(blocks))
	for index := range hashes {
		hashes[index] = [32]byte{1}
	}
	return hashes, nil
}

func (self *ledgerV2V1Chain) FleetBoundEvents(ctx context.Context, fromBlock uint64, toBlock uint64) ([]*model.StChainEvent, error) {
	events := []*model.StChainEvent{}
	if toBlock < 150 || 150 < fromBlock {
		return events, nil
	}
	for clientId := range self.bound {
		data, err := json.Marshal(model.StFleetBoundEventData{ClientId: fmt.Sprintf("0x%x", clientId), Generation: "1", ValidFromEpoch: "0", ValidToEpoch: "1000"})
		if err != nil {
			return nil, err
		}
		events = append(events, &model.StChainEvent{BlockNumber: 150, BlockHash: common.Hash{1}.Hex(), LogIndex: len(events), TxHash: common.Hash{2}.Hex(), Kind: model.StFleetBoundEventKind, DataJson: string(data)})
	}
	return events, nil
}

// SV-2: for an epoch both versions see, every v2 row is a provider v1 counted
// with settled bytes, with v1's usage, coldkey and head flag, and none is
// missing; a provider whose contracts settled no bytes has no row. v2 retains
// exactly the wallet resolutions v1 retained, that provider's included. The
// earning window is [timestamp(start), timestamp(end)): a contract closed at
// the start counts and one closed at the end does not, in both. v1 also weighs
// reliability, which here leaves every provider ineligible for lack of
// confirmations; v2 has no reliability input and pays the eligible rows.
func TestStLedgerV2RowsEqualV1Inputs(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		controllerPayoutSchedule(t, stPayoutPolicyTime(100))
		fixture, _, cfg, restart := newStPayoutPolicyFixture(t, 1)
		cfg.DeployBlock = 100
		chain := &ledgerV2V1Chain{CoreStClient: restart(), bound: map[[16]byte]bool{}}
		fixture.configure(275, "")
		SetStClient(chain)
		SetStLedgerV2ObjectStore(server.NewLocalBlobStore(t.TempDir(), "test"))
		t.Cleanup(func() { SetStLedgerV2ObjectStore(nil) })

		// consents approved at block 150 of epoch 0; acceptances expired before
		// epoch 1 starts at block 200
		consents := newLedgerV2Consents(t, cfg, stPayoutPolicySnapshot(1).PolicyHash, protocol.ClientKeyEffectiveBoundary{Epoch: 0, Block: 150, Hash: [32]byte{2}}, stPayoutPolicyTime(150))
		own, network, delegated, unmapped, head, shared, idle := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
		networks := [5]server.Id{server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()}
		consents.providerChain(t, own, networks[0], [2]uint64{11, 1})
		consents.providerChain(t, idle, networks[0], [2]uint64{16, 1})
		consents.networkChain(t, networks[1], 12, 1)
		consents.delegationChain(t, networks[2], 13, consents.hotkeyChain(t, 13, 14, 1), 1)
		consents.providerChain(t, head, networks[4], [2]uint64{15, 1})
		consents.providerChain(t, shared, networks[4], [2]uint64{11, 1})
		consents.flush(t)
		chain.bound[[16]byte(head)] = true

		// one provider per contract: v1's census requires each contract's
		// providers to split its bytes evenly
		f := &ledgerV2BuildFixture{t: t, ctx: ctx, cfg: cfg}
		start, end := stPayoutPolicyTime(200), stPayoutPolicyTime(250)
		f.contract(start, ledgerV2BuildUsage{own, networks[0], 60})
		f.contract(start, ledgerV2BuildUsage{network, networks[1], 200})
		f.contract(end.Add(-time.Microsecond), ledgerV2BuildUsage{own, networks[0], 40})
		f.contract(start.Add(-time.Microsecond), ledgerV2BuildUsage{own, networks[0], 10_000})
		f.contract(end, ledgerV2BuildUsage{own, networks[0], 7})
		for _, usage := range []ledgerV2BuildUsage{{delegated, networks[2], 300}, {unmapped, networks[3], 50}, {head, networks[4], 500}, {shared, networks[4], 400}, {idle, networks[0], 0}} {
			f.contract(start.Add(time.Minute), usage)
		}

		if _, _, err := stComputeReleasePayout(ctx, cfg, chain, 1, start, end, 200, 250, nil); err != nil {
			t.Fatal(err)
		}
		record := model.GetStPayoutArtifact(ctx, cfg.DeploymentKey(), 1, cfg.NoId)
		store, ok := server.LoadBlobStore()
		if record == nil || !ok {
			t.Fatal("v1 published no artifact")
		}
		artifact, _, err := startifact.Read(ctx, store, record.ContentHash)
		if err != nil {
			t.Fatal(err)
		}
		v1Resolutions := model.GetStPayoutWalletResolutions(ctx, cfg.DeploymentKey(), 1, cfg.NoId)

		build, err := stLedgerV2Builder{}.Build(ctx, &StLedgerV2BuildInput{NoId: cfg.NoId, Epoch: 1, Epochs: []*StLedgerV2CoveredEpoch{ledgerV2Covered(1, 1_000_000, nil)}})
		if err != nil {
			t.Fatal(err)
		}
		objects := ledger.Objects{}
		for _, chunk := range build.Chunks {
			objects[ledger.RefOf(chunk)] = chunk
		}
		chunks := [][]byte{}
		for _, ref := range build.Epochs[0].ProviderChunks {
			parsed, err := ledger.ParseRef(ref)
			if err != nil {
				t.Fatal(err)
			}
			chunks = append(chunks, objects[parsed])
		}
		rows, err := ledger.DecodeProviderChunks(chunks)
		if err != nil {
			t.Fatal(err)
		}

		counted := 0
		for _, provider := range artifact.Providers {
			if provider.UsageBytes == 0 {
				continue
			}
			counted++
			index := slices.IndexFunc(rows, func(row protocol.LedgerUsageRow) bool { return row.ClientID == provider.ClientID })
			if index < 0 {
				t.Fatalf("v1 provider %x has no v2 row", provider.ClientID)
			}
			if row := rows[index]; row.UsageBytes != provider.UsageBytes || row.Coldkey != provider.Coldkey || row.HeadExcluded != provider.HeadExcluded {
				t.Fatalf("provider %x: v2 row %+v, v1 %+v", provider.ClientID, row, provider)
			}
			wantReason := map[server.Id]string{unmapped: "missing_payout_wallet", head: "head_fleet_active"}[server.Id(provider.ClientID)]
			if wantReason == "" {
				wantReason = "reliability_exposure_floor"
			}
			if provider.Eligible || provider.ExclusionReason != wantReason {
				t.Fatalf("v1 provider %x eligible %t because %q", provider.ClientID, provider.Eligible, provider.ExclusionReason)
			}
		}
		if counted != 6 || len(rows) != counted || len(artifact.Leaves) != 0 {
			t.Fatalf("v1 counted %d providers with %d leaves, v2 %d rows", counted, len(artifact.Leaves), len(rows))
		}
		if index := slices.IndexFunc(rows, func(row protocol.LedgerUsageRow) bool { return row.ClientID == [16]byte(own) }); rows[index].UsageBytes != 100 {
			t.Fatalf("the window [start, end) counted %d bytes for the provider", rows[index].UsageBytes)
		}
		if after := model.GetStPayoutWalletResolutions(ctx, cfg.DeploymentKey(), 1, cfg.NoId); len(v1Resolutions) != 6 || !reflect.DeepEqual(after, v1Resolutions) {
			t.Fatalf("v1 retained %d resolutions, %d after v2", len(v1Resolutions), len(after))
		}
		ownKey, networkKey, hotkeyColdkey := consents.public(t, 11), consents.public(t, 12), consents.public(t, 14)
		want := ledgerV2SortedAccounts(ledgerV2Account(ownKey, 500_000), ledgerV2Account(networkKey, 200_000), ledgerV2Account(hotkeyColdkey, 300_000))
		if !reflect.DeepEqual(build.Accounts, want) {
			t.Fatalf("v2 accounts %v", build.Accounts)
		}
		var total uint256.Int
		for _, account := range build.Accounts {
			total.Add(&total, &account.Cumulative)
		}
		if total.Uint64() != 1_000_000 {
			t.Fatalf("v2 allocated %s", total.Dec())
		}
	})
}

// SV-2 (roster path): with provider_work.yml configured, the epoch's signed
// roster fixes the provider universe of v2 as of v1. With an empty roster and
// no usage, v1 publishes no providers and v2 builds no rows; usage from a
// provider the roster does not list is refused.
func TestStLedgerV2RowsFollowTheSignedRoster(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		controllerPayoutSchedule(t, stPayoutPolicyTime(100))
		fixture, _, cfg, restart := newStPayoutPolicyFixture(t, 1)
		cfg.DeployBlock = 100
		chain := &ledgerV2V1Chain{CoreStClient: restart(), bound: map[[16]byte]bool{}}
		fixture.configure(275, "")
		SetStClient(chain)
		SetStLedgerV2ObjectStore(server.NewLocalBlobStore(t.TempDir(), "test"))
		t.Cleanup(func() { SetStLedgerV2ObjectStore(nil) })
		roster, domainHash := stPayoutPolicyEmptyWorkAuthority(t, cfg, chain.CoreStClient, 1)
		raw, err := roster.Bytes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := model.RetainProviderWorkAuthority(ctx, raw, domainHash, roster.RequestPublicKey, crypto.PubkeyToAddress(cfg.RootKey.PublicKey)); err != nil {
			t.Fatal(err)
		}
		start, end := stPayoutPolicyTime(200), stPayoutPolicyTime(250)

		if _, _, err := stComputeReleasePayout(ctx, cfg, chain, 1, start, end, 200, 250, nil); err != nil {
			t.Fatal(err)
		}
		store, ok := server.LoadBlobStore()
		if !ok {
			t.Fatal("the payout store is unavailable")
		}
		artifact, _, err := startifact.Read(ctx, store, model.GetStPayoutArtifact(ctx, cfg.DeploymentKey(), 1, cfg.NoId).ContentHash)
		if err != nil || len(artifact.Providers) != 0 {
			t.Fatalf("v1 published %d providers: %v", len(artifact.Providers), err)
		}
		if rows, err := stLedgerV2NewEpochRows(ctx, 1); err != nil || rows == nil || len(rows) != 0 {
			t.Fatalf("v2 rows %v: %v", rows, err)
		}

		f := &ledgerV2BuildFixture{t: t, ctx: ctx, cfg: cfg}
		f.contract(start, ledgerV2BuildUsage{server.NewId(), server.NewId(), 10})
		if rows, err := stLedgerV2NewEpochRows(ctx, 1); !errors.Is(err, model.ErrProviderWorkConflict) {
			t.Fatalf("a provider outside the roster: rows %v, %v", rows, err)
		}
	})
}
