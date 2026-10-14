package controller

import (
	"context"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi/bind/v2"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/urfoundation/sn/stabi"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func ledgerV2TestConfig() *StConfig {
	cfg := &StConfig{
		Profile: "testnet", DeploymentId: "v2-test", ChainId: 945, Enabled: true, NoId: 1, BlockSeconds: 12,
		ContractAddress: common.HexToAddress("0x2000000000000000000000000000000000000002"),
		SettlementVault: common.HexToAddress("0x2000000000000000000000000000000000000003"),
		ReserveSink:     common.HexToAddress("0x2000000000000000000000000000000000000004"),
		SettlementV2:    common.HexToAddress(testLedgerV2Settlement),
		VaultV2:         common.HexToAddress(testLedgerV2Vault),
	}
	cfg.GenesisHash[0] = 1
	return cfg
}

// Packs one event of a v2 contract as a log at address.
func ledgerV2EventLog(t *testing.T, meta *bind.MetaData, address common.Address, name string, indexed []common.Hash, values ...any) *types.Log {
	t.Helper()
	parsed, err := meta.ParseABI()
	if err != nil {
		t.Fatal(err)
	}
	event, ok := parsed.Events[name]
	if !ok {
		t.Fatalf("ABI has no %s event", name)
	}
	data, err := event.Inputs.NonIndexed().Pack(values...)
	if err != nil {
		t.Fatal(err)
	}
	return &types.Log{Address: address, Topics: append([]common.Hash{event.ID}, indexed...), Data: data}
}

func decodeLedgerV2Event(t *testing.T, cfg *StConfig, log *types.Log) (string, map[string]any, bool) {
	t.Helper()
	decoders, ok := (&CoreStClient{cfg: cfg}).stLedgerV2EventDecoders(log.Address)
	if !ok {
		t.Fatalf("no v2 decoders for %s", log.Address)
	}
	for _, decode := range decoders {
		if kind, data, ok := decode(log); ok {
			return kind, data, true
		}
	}
	return "", nil, false
}

func ledgerV2Word(value uint64) common.Hash {
	return common.BigToHash(new(big.Int).SetUint64(value))
}

// Mirror decoding: every vault2 and STSettlement2 event, with exact fields.
// SV-5 (mirror)
func TestStLedgerV2EventDecodersExactFields(t *testing.T) {
	cfg := ledgerV2TestConfig()
	vault, settlement := &stabi.STSettlementVault2MetaData, &stabi.STSettlement2MetaData
	hotkey, root, ledgerHash, coldkey := common.HexToHash("0x11"), common.HexToHash("0x22"), common.HexToHash("0x33"), common.HexToHash("0x44")
	actor := common.HexToAddress("0x00000000000000000000000000000000000000c1")
	other := common.HexToAddress("0x00000000000000000000000000000000000000c2")
	actorTopic := common.BytesToHash(actor.Bytes())
	for _, test := range []struct {
		log  *types.Log
		kind string
		want map[string]any
	}{
		{ledgerV2EventLog(t, vault, cfg.VaultV2, "CoordinatorFixed", []common.Hash{actorTopic}),
			"V2CoordinatorFixed", map[string]any{"coordinator": actor.Hex()}},
		{ledgerV2EventLog(t, vault, cfg.VaultV2, "EscrowRegistered", []common.Hash{hotkey}, uint16(7)),
			"V2EscrowRegistered", map[string]any{"hotkey": hotkey.Hex(), "uid": "7"}},
		{ledgerV2EventLog(t, vault, cfg.VaultV2, "PoolRegistered", []common.Hash{ledgerV2Word(1), hotkey}, uint16(9), uint64(5)),
			"V2PoolRegistered", map[string]any{"no_id": "1", "hotkey": hotkey.Hex(), "uid": "9", "first_epoch": "5"}},
		{ledgerV2EventLog(t, vault, cfg.VaultV2, "PoolUidRegistered", []common.Hash{ledgerV2Word(1), hotkey}, uint16(10), actor),
			"V2PoolUidRegistered", map[string]any{"no_id": "1", "hotkey": hotkey.Hex(), "uid": "10", "payer": actor.Hex()}},
		{ledgerV2EventLog(t, vault, cfg.VaultV2, "EmissionCaptured", []common.Hash{ledgerV2Word(5), ledgerV2Word(1)}, big.NewInt(1001), big.NewInt(1000), big.NewInt(3000)),
			"V2EmissionCaptured", map[string]any{"e": "5", "no_id": "1", "observed": "1001", "amount": "1000", "funded_through": "3000"}},
		{ledgerV2EventLog(t, vault, cfg.VaultV2, "EmissionDeferred", []common.Hash{ledgerV2Word(6), ledgerV2Word(1)}, big.NewInt(3000)),
			"V2EmissionDeferred", map[string]any{"e": "6", "no_id": "1", "funded_through": "3000"}},
		{ledgerV2EventLog(t, vault, cfg.VaultV2, "DistributionCommitted", []common.Hash{ledgerV2Word(1), ledgerV2Word(3)}, uint64(5), root, big.NewInt(3000), uint64(2), ledgerHash, uint64(2)),
			"V2DistributionCommitted", map[string]any{"no_id": "1", "seq": "3", "e": "5", "root": root.Hex(), "total": "3000", "leaf_count": "2", "ledger_hash": ledgerHash.Hex(), "replaced_seq": "2"}},
		{ledgerV2EventLog(t, vault, cfg.VaultV2, "PathDemanded", []common.Hash{ledgerV2Word(1), ledgerV2Word(3)}, big.NewInt(2), big.NewInt(4), actor),
			"V2PathDemanded", map[string]any{"no_id": "1", "seq": "3", "root_index": "2", "index": "4", "demander": actor.Hex()}},
		{ledgerV2EventLog(t, vault, cfg.VaultV2, "PathAnswered", []common.Hash{ledgerV2Word(1), ledgerV2Word(3)}, big.NewInt(2), big.NewInt(4), other),
			"V2PathAnswered", map[string]any{"no_id": "1", "seq": "3", "root_index": "2", "index": "4", "answerer": other.Hex()}},
		{ledgerV2EventLog(t, vault, cfg.VaultV2, "DistributionDeleted", []common.Hash{ledgerV2Word(1), ledgerV2Word(3)}, big.NewInt(2), big.NewInt(4), uint8(2), big.NewInt(1), actor),
			"V2DistributionDeleted", map[string]any{"no_id": "1", "seq": "3", "root_index": "2", "index": "4", "fault": "2", "level": "1", "challenger": actor.Hex()}},
		{ledgerV2EventLog(t, vault, cfg.VaultV2, "DistributionFinalized", []common.Hash{ledgerV2Word(1), ledgerV2Word(3)}, big.NewInt(0), uint64(5), root, big.NewInt(3000), uint64(2), ledgerHash),
			"V2DistributionFinalized", map[string]any{"no_id": "1", "seq": "3", "root_index": "0", "e": "5", "root": root.Hex(), "total": "3000", "leaf_count": "2", "ledger_hash": ledgerHash.Hex()}},
		{ledgerV2EventLog(t, vault, cfg.VaultV2, "Claimed", []common.Hash{ledgerV2Word(1), ledgerV2Word(4), coldkey}, big.NewInt(0), big.NewInt(2000), big.NewInt(1500), other),
			"V2Claimed", map[string]any{"no_id": "1", "root_index": "0", "index": "4", "coldkey": coldkey.Hex(), "cumulative": "2000", "amount": "1500", "caller": other.Hex()}},
		{ledgerV2EventLog(t, vault, cfg.VaultV2, "StakeAbsorbed", []common.Hash{hotkey}, big.NewInt(70), big.NewInt(69), actor),
			"V2StakeAbsorbed", map[string]any{"hotkey": hotkey.Hex(), "observed": "70", "amount": "69", "caller": actor.Hex()}},
		{ledgerV2EventLog(t, vault, cfg.VaultV2, "SurplusAssigned", []common.Hash{ledgerV2Word(1), ledgerV2Word(8)}, big.NewInt(69)),
			"V2SurplusAssigned", map[string]any{"no_id": "1", "e": "8", "amount": "69"}},
		{ledgerV2EventLog(t, settlement, cfg.SettlementV2, "AuditorsSet", []common.Hash{ledgerV2Word(2)}, []common.Address{actor, other}, big.NewInt(1)),
			"V2AuditorsSet", map[string]any{"version": "2", "auditors": []string{actor.Hex(), other.Hex()}, "quorum": "1"}},
		{ledgerV2EventLog(t, settlement, cfg.SettlementV2, "DistributionApproved", []common.Hash{ledgerV2Word(1), ledgerV2Word(3), actorTopic}, uint64(2), big.NewInt(1)),
			"V2DistributionApproved", map[string]any{"no_id": "1", "seq": "3", "auditor": actor.Hex(), "auditor_set_version": "2", "approvals": "1"}},
	} {
		kind, data, ok := decodeLedgerV2Event(t, cfg, test.log)
		if !ok || kind != test.kind || !reflect.DeepEqual(data, test.want) {
			t.Fatalf("decoded %s %#v, want %s %#v", kind, data, test.kind, test.want)
		}
	}
}

// Mirror decoding: v2 decoders serve only the configured v2 addresses, each
// contract decodes only its own events, and nothing is decoded with v2 off.
// SV-5 (mirror)
func TestStLedgerV2EventDecodersRouteByAddress(t *testing.T) {
	cfg := ledgerV2TestConfig()
	client := &CoreStClient{cfg: cfg}
	for _, address := range []common.Address{cfg.ContractAddress, cfg.SettlementVault, cfg.ReserveSink, {}} {
		if _, ok := client.stLedgerV2EventDecoders(address); ok {
			t.Fatalf("v2 decoders served %s", address)
		}
	}
	approved := ledgerV2EventLog(t, &stabi.STSettlement2MetaData, cfg.VaultV2, "DistributionApproved", []common.Hash{ledgerV2Word(1), ledgerV2Word(3), ledgerV2Word(9)}, uint64(2), big.NewInt(1))
	if kind, _, ok := decodeLedgerV2Event(t, cfg, approved); ok {
		t.Fatalf("an STSettlement2 event at the vault2 address decoded as %s", kind)
	}
	deferred := ledgerV2EventLog(t, &stabi.STSettlementVault2MetaData, cfg.SettlementV2, "EmissionDeferred", []common.Hash{ledgerV2Word(6), ledgerV2Word(1)}, big.NewInt(3000))
	if kind, _, ok := decodeLedgerV2Event(t, cfg, deferred); ok {
		t.Fatalf("a vault2 event at the STSettlement2 address decoded as %s", kind)
	}
	off := ledgerV2TestConfig()
	off.SettlementV2, off.VaultV2 = common.Address{}, common.Address{}
	if _, ok := (&CoreStClient{cfg: off}).stLedgerV2EventDecoders(common.Address{}); ok {
		t.Fatal("v2 decoders served with v2 off")
	}
}

// Mirror decoding through SyncEvents: the log filter adds the v2 contracts,
// and a v2 log is decoded only by the v2 decoders.
// SV-5 (mirror)
func TestStSyncEventsMirrorsLedgerV2Contracts(t *testing.T) {
	cfg := ledgerV2TestConfig()
	v2Log := ledgerV2EventLog(t, &stabi.STSettlementVault2MetaData, cfg.VaultV2, "EmissionDeferred", []common.Hash{ledgerV2Word(6), ledgerV2Word(1)}, big.NewInt(3000))
	unknownLog := ledgerV2EventLog(t, &stabi.STSettlement2MetaData, cfg.VaultV2, "DistributionApproved", []common.Hash{ledgerV2Word(1), ledgerV2Word(3), ledgerV2Word(9)}, uint64(2), big.NewInt(1))
	for i, log := range []*types.Log{v2Log, unknownLog} {
		log.BlockNumber, log.Index, log.BlockHash, log.TxHash = 10, uint(i), common.HexToHash("0xb10c"), common.HexToHash("0x7a")
	}
	var filterAddresses []string
	fixture := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		var call struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &call); err != nil || call.Method != "eth_getLogs" {
			t.Errorf("unexpected RPC %s: %v", body, err)
			return
		}
		var filter struct {
			Address []string `json:"address"`
		}
		if err := json.Unmarshal(call.Params[0], &filter); err != nil {
			t.Error(err)
		}
		filterAddresses = filter.Address
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": []*types.Log{v2Log, unknownLog}})
	}))
	defer fixture.Close()
	rpcClient, err := ethclient.Dial(fixture.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer rpcClient.Close()
	cfg.RpcUrls = []string{fixture.URL}
	client := &CoreStClient{cfg: cfg, coordinator: stabi.NewSTCoordinator(), vault: stabi.NewSTSettlementVault(), reserve: stabi.NewSTReserveSink(), clients: map[string]*ethclient.Client{fixture.URL: rpcClient}}
	events, next, err := client.SyncEvents(context.Background(), 10, 10)
	if err != nil || next != 11 {
		t.Fatalf("sync events: next %d, %v", next, err)
	}
	wantAddresses := []string{cfg.ContractAddress.Hex(), cfg.SettlementVault.Hex(), cfg.ReserveSink.Hex(), cfg.SettlementV2.Hex(), cfg.VaultV2.Hex()}
	if !reflect.DeepEqual(lowerAll(filterAddresses), lowerAll(wantAddresses)) {
		t.Fatalf("log filter addresses %v, want %v", filterAddresses, wantAddresses)
	}
	if len(events) != 1 || events[0].Kind != "V2EmissionDeferred" || events[0].DataJson != `{"e":"6","funded_through":"3000","no_id":"1"}` {
		t.Fatalf("mirrored events %+v", events)
	}
}

func lowerAll(values []string) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = strings.ToLower(value)
	}
	return out
}

func ledgerV2MirrorEvent(t testing.TB, block uint64, index int, kind string, data map[string]any) *model.StChainEvent {
	t.Helper()
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return &model.StChainEvent{BlockNumber: block, LogIndex: index, Kind: kind, DataJson: string(encoded)}
}

// The mirror applies funding, roots and demands in block order, idempotently,
// keys each demand by its seq, finalized root and account index, and alerts
// on a deferred epoch of this pool.
// SV-5 (mirror), SV-1 (missed-close alert)
func TestStLedgerV2MirrorAppliesEvents(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cfg := ledgerV2TestConfig()
		alerts := captureLedgerV2Alerts(t)
		root, hash := "0x"+strings.Repeat("22", 32), "0x"+strings.Repeat("33", 32)
		events := []*model.StChainEvent{
			// out of order on purpose: application sorts by (block, log)
			ledgerV2MirrorEvent(t, 30, 1, "V2DistributionFinalized", map[string]any{"no_id": "1", "seq": "1", "root_index": "0", "e": "5", "root": root, "total": "3000", "leaf_count": "2", "ledger_hash": hash}),
			ledgerV2MirrorEvent(t, 10, 0, "V2EmissionCaptured", map[string]any{"e": "5", "no_id": "1", "observed": "3001", "amount": "3000", "funded_through": "3000"}),
			ledgerV2MirrorEvent(t, 11, 0, "V2DistributionCommitted", map[string]any{"no_id": "1", "seq": "1", "e": "5", "root": root, "total": "3000", "leaf_count": "2", "ledger_hash": hash, "replaced_seq": "0"}),
			ledgerV2MirrorEvent(t, 12, 0, "V2PathDemanded", map[string]any{"no_id": "1", "seq": "1", "root_index": "0", "index": "1", "demander": "0xd1"}),
			ledgerV2MirrorEvent(t, 13, 0, "V2PathAnswered", map[string]any{"no_id": "1", "seq": "1", "root_index": "0", "index": "1", "answerer": "0xa1"}),
			ledgerV2MirrorEvent(t, 40, 0, "V2EmissionDeferred", map[string]any{"e": "6", "no_id": "1", "funded_through": "3000"}),
			ledgerV2MirrorEvent(t, 41, 0, "V2EmissionDeferred", map[string]any{"e": "6", "no_id": "2", "funded_through": "0"}),
			ledgerV2MirrorEvent(t, 42, 0, "V2DistributionCommitted", map[string]any{"no_id": "1", "seq": "2", "e": "7", "root": root, "total": "5000", "leaf_count": "3", "ledger_hash": hash, "replaced_seq": "0"}),
			// the same account index demanded against two finalized roots
			ledgerV2MirrorEvent(t, 43, 0, "V2PathDemanded", map[string]any{"no_id": "1", "seq": "2", "root_index": "0", "index": "0", "demander": "0xd1"}),
			ledgerV2MirrorEvent(t, 43, 1, "V2PathDemanded", map[string]any{"no_id": "1", "seq": "2", "root_index": "1", "index": "0", "demander": "0xd2"}),
			ledgerV2MirrorEvent(t, 43, 2, "V2PathAnswered", map[string]any{"no_id": "1", "seq": "2", "root_index": "1", "index": "0", "answerer": "0xa1"}),
			ledgerV2MirrorEvent(t, 44, 0, "V2DistributionCommitted", map[string]any{"no_id": "1", "seq": "3", "e": "8", "root": root, "total": "6000", "leaf_count": "3", "ledger_hash": hash, "replaced_seq": "2"}),
			ledgerV2MirrorEvent(t, 45, 0, "V2DistributionDeleted", map[string]any{"no_id": "1", "seq": "3", "root_index": "0", "index": "0", "fault": "1", "level": "1", "challenger": "0xc1"}),
			// malformed and foreign rows are skipped
			ledgerV2MirrorEvent(t, 46, 0, "V2PathDemanded", map[string]any{"no_id": "x", "seq": "3", "root_index": "0", "index": "0"}),
			ledgerV2MirrorEvent(t, 46, 1, "V2PathDemanded", map[string]any{"no_id": "1", "seq": "3", "root_index": "0", "index": "-1"}),
			ledgerV2MirrorEvent(t, 46, 9, "V2PathDemanded", map[string]any{"no_id": "1", "seq": "x", "root_index": "0", "index": "0"}),
			ledgerV2MirrorEvent(t, 46, 10, "V2PathDemanded", map[string]any{"no_id": "1", "seq": "3", "root_index": "x", "index": "0"}),
			ledgerV2MirrorEvent(t, 46, 2, "V2EmissionCaptured", map[string]any{"e": "x", "no_id": "1", "observed": "1", "amount": "1", "funded_through": "1"}),
			ledgerV2MirrorEvent(t, 46, 3, "V2EmissionCaptured", map[string]any{"e": "9", "no_id": "1", "observed": "1", "amount": "1", "funded_through": "-1"}),
			ledgerV2MirrorEvent(t, 46, 4, "V2DistributionCommitted", map[string]any{"no_id": "1", "seq": "4", "e": "9", "root": root, "total": "x", "leaf_count": "3", "ledger_hash": hash, "replaced_seq": "0"}),
			ledgerV2MirrorEvent(t, 46, 5, "V2DistributionCommitted", map[string]any{"no_id": "1", "seq": "4", "e": "9", "root": root, "total": "1", "leaf_count": "3", "ledger_hash": hash, "replaced_seq": "x"}),
			ledgerV2MirrorEvent(t, 46, 6, "V2DistributionFinalized", map[string]any{"no_id": "1", "seq": "4", "root_index": "x", "e": "9", "root": root, "total": "1", "leaf_count": "3", "ledger_hash": hash}),
			ledgerV2MirrorEvent(t, 46, 7, "V2DistributionFinalized", map[string]any{"no_id": "1", "seq": "4", "root_index": "1", "e": "9", "root": "0x12", "total": "1", "leaf_count": "3", "ledger_hash": hash}),
			{BlockNumber: 46, LogIndex: 8, Kind: "V2PathAnswered", DataJson: "not json"},
			ledgerV2MirrorEvent(t, 47, 0, "OperatorCommitted", map[string]any{"e": "9", "no_id": "1"}),
		}
		for range 2 {
			stApplyLedgerV2Events(ctx, cfg, events)
		}
		key := cfg.DeploymentKey()
		funding := model.GetStLedgerV2Funding(ctx, key, 1, 5)
		if funding == nil || funding.FundedThrough.Uint64() != 3000 || funding.Observed.Uint64() != 3001 || funding.Amount.Uint64() != 3000 || funding.Deferred || funding.BlockNumber != 10 {
			t.Fatalf("captured funding %+v", funding)
		}
		if deferred := model.GetStLedgerV2Funding(ctx, key, 1, 6); deferred == nil || !deferred.Deferred || deferred.FundedThrough.Uint64() != 3000 || !deferred.Amount.IsZero() || deferred.BlockNumber != 40 {
			t.Fatalf("deferred funding %+v", deferred)
		}
		first := model.GetStLedgerV2Root(ctx, key, 1, 1)
		if first == nil || first.Status != model.StLedgerV2RootFinalized || *first.RootIndex != 0 || *first.CommitBlock != 11 || *first.StatusBlock != 30 || first.Epoch != 5 || first.Total.Uint64() != 3000 || first.LeafCount != 2 || first.Root != common.HexToHash(root) || first.LedgerHash != common.HexToHash(hash) {
			t.Fatalf("finalized root %+v", first)
		}
		if replaced := model.GetStLedgerV2Root(ctx, key, 1, 2); replaced == nil || replaced.Status != model.StLedgerV2RootReplaced || *replaced.StatusBlock != 44 {
			t.Fatalf("replaced root %+v", replaced)
		}
		if deleted := model.GetStLedgerV2Root(ctx, key, 1, 3); deleted == nil || deleted.Status != model.StLedgerV2RootDeleted || *deleted.StatusBlock != 45 {
			t.Fatalf("deleted root %+v", deleted)
		}
		if pending := model.GetStLedgerV2PendingRoot(ctx, key, 1); pending != nil {
			t.Fatalf("pending root %+v after deletion", pending)
		}
		if open := model.GetOpenStLedgerV2Demands(ctx, key, 1, 1); len(open) != 0 {
			t.Fatalf("answered demand still open: %+v", open)
		}
		if open := model.GetOpenStLedgerV2Demands(ctx, key, 1, 2); len(open) != 1 || open[0].RootIndex != 0 || open[0].Index != 0 || open[0].Demander != "0xd1" || open[0].DemandBlock != 43 {
			t.Fatalf("open demands %+v", open)
		}
		if got := model.GetStLedgerV2RootsFromEpoch(ctx, key, 1, 9); len(got) != 0 || model.GetStLedgerV2Funding(ctx, key, 1, 9) != nil || len(model.GetOpenStLedgerV2Demands(ctx, key, 1, 3)) != 0 {
			t.Fatalf("malformed events were mirrored: %+v", got)
		}
		if got := *alerts; len(got) != 1 || got[0] != StLedgerV2AlertMissedClose+" epoch 6" {
			t.Fatalf("alerts %v, want one missed close for this pool's deferred epoch", got)
		}
	})
}

// captureLedgerV2Alerts records each raised alert as "name subject".
func captureLedgerV2Alerts(t testing.TB) *[]string {
	alerts := &[]string{}
	sink := stLedgerV2AlertSink
	stLedgerV2AlertSink = func(name string, subject string, message string) {
		*alerts = append(*alerts, name+" "+subject)
	}
	stLedgerV2Alerted.Clear()
	t.Cleanup(func() {
		stLedgerV2AlertSink = sink
		stLedgerV2Alerted.Clear()
	})
	return alerts
}
