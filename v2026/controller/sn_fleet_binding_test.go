package controller

// The live fleet binding sync (LEADERTOP.md §5.7): the coordinator's
// FleetBound, FleetBindingRevoked and FleetBindingCleaned logs decode into
// mirror rows, a finalized canonical range applies them with the
// coordinator's rules, and the claimed fleets join them to networks and the
// metagraph's UID holders. Identities are synthetic.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/urfoundation/sn/v2026/stabi"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A fake EVM endpoint for one coordinator's binding logs and canonical blocks.
type snFleetBindingRPC struct {
	t         testing.TB
	address   common.Address
	finalized uint64
	logs      []types.Log
	// when set, these blocks' canonical hashes differ from their logs'
	forked map[uint64]bool

	stateLock sync.Mutex
	logRanges [][2]uint64
}

func (self *snFleetBindingRPC) ranges() [][2]uint64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([][2]uint64(nil), self.logRanges...)
}

func (self *snFleetBindingRPC) blockHash(block uint64) string {
	if self.forked[block] {
		return fmt.Sprintf("0x%064x", block+0xf0f0)
	}
	return stBlockBatchHash(block)
}

func (self *snFleetBindingRPC) answer(call stEpochRPCRequest) map[string]any {
	response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
	var result any
	var err error
	switch call.Method {
	case "eth_chainId":
		result = "0x3b1"
	case "eth_getBlockByNumber":
		var selector string
		if len(call.Params) != 2 || json.Unmarshal(call.Params[0], &selector) != nil {
			err = errors.New("invalid block selector")
			break
		}
		block := self.finalized
		if selector != "finalized" {
			block, err = hexutil.DecodeUint64(selector)
		}
		object := stBlockBatchResult(block)
		object["hash"] = self.blockHash(block)
		result = object
	case "eth_getLogs":
		var filter struct {
			FromBlock string          `json:"fromBlock"`
			ToBlock   string          `json:"toBlock"`
			Address   json.RawMessage `json:"address"`
			Topics    [][]string      `json:"topics"`
		}
		if len(call.Params) != 1 || json.Unmarshal(call.Params[0], &filter) != nil {
			err = errors.New("invalid log filter")
			break
		}
		from, fromErr := hexutil.DecodeUint64(filter.FromBlock)
		to, toErr := hexutil.DecodeUint64(filter.ToBlock)
		want := []string{strings.ToLower(stFleetBoundTopic.Hex()), strings.ToLower(stFleetBindingRevokedTopic.Hex()), strings.ToLower(stFleetBindingCleanedTopic.Hex())}
		topics := []string{}
		if len(filter.Topics) == 1 {
			for _, topic := range filter.Topics[0] {
				topics = append(topics, strings.ToLower(topic))
			}
		}
		if fromErr != nil || toErr != nil || !strings.Contains(strings.ToLower(string(filter.Address)), strings.ToLower(self.address.Hex())) || !slices.Equal(topics, want) {
			err = fmt.Errorf("unexpected log filter %+v", filter)
			break
		}
		self.stateLock.Lock()
		self.logRanges = append(self.logRanges, [2]uint64{from, to})
		self.stateLock.Unlock()
		logs := []types.Log{}
		for _, log := range self.logs {
			if from <= log.BlockNumber && log.BlockNumber <= to {
				logs = append(logs, log)
			}
		}
		result = logs
	default:
		err = fmt.Errorf("unexpected method %s", call.Method)
	}
	if err != nil {
		response["error"] = map[string]any{"code": -32000, "message": err.Error()}
	} else {
		response["result"] = result
	}
	return response
}

func (self *snFleetBindingRPC) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		self.t.Errorf("read binding RPC body: %v", err)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	if !strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
		var call stEpochRPCRequest
		if json.Unmarshal(body, &call) != nil {
			self.t.Errorf("invalid binding RPC request: %s", body)
			return
		}
		_ = json.NewEncoder(writer).Encode(self.answer(call))
		return
	}
	var calls []stEpochRPCRequest
	if json.Unmarshal(body, &calls) != nil || len(calls) == 0 || len(calls) > stMaximumEVMRPCBatchCalls {
		self.t.Errorf("invalid binding RPC batch: %s", body)
		return
	}
	responses := make([]map[string]any, len(calls))
	for index, call := range calls {
		responses[index] = self.answer(call)
	}
	_ = json.NewEncoder(writer).Encode(responses)
}

func newSnFleetBindingClient(t testing.TB, fixture *snFleetBindingRPC) *CoreStClient {
	t.Helper()
	client := newStBlockBatchClient(t, fixture)
	client.cfg.ContractAddress = fixture.address
	client.coordinator = stabi.NewSTCoordinator()
	return client
}

var snFleetBindingTestAddress = common.HexToAddress("0x2000000000000000000000000000000000000025")

func snFleetBindingTestTopic(clientId [16]byte) common.Hash {
	var topic common.Hash
	copy(topic[:16], clientId[:])
	return topic
}

func snFleetBindingTestLog(block uint64, index uint, topics []common.Hash, data []byte) types.Log {
	return types.Log{Address: snFleetBindingTestAddress, Topics: topics, Data: data, BlockNumber: block,
		TxHash: common.Hash{byte(block >> 8), byte(block), byte(index), 0x5b}, BlockHash: common.HexToHash(stBlockBatchHash(block)), Index: index}
}

// FleetBound(clientId, fleetId, hotkey, uid, generation, validFrom, validTo).
func snFleetBoundTestLog(t testing.TB, block uint64, index uint, clientId [16]byte, fleet byte, hotkey byte, uid uint16, generation uint64, from uint64, through uint64) types.Log {
	t.Helper()
	parsed, err := stabi.STCoordinatorMetaData.ParseABI()
	if err != nil {
		t.Fatal(err)
	}
	event := parsed.Events["FleetBound"]
	data, err := event.Inputs.NonIndexed().Pack(uid, generation, from, through)
	if err != nil {
		t.Fatal(err)
	}
	return snFleetBindingTestLog(block, index, []common.Hash{event.ID, snFleetBindingTestTopic(clientId), {fleet, 0xf1}, {hotkey, 0xf2}}, data)
}

// FleetBindingRevoked(clientId, generation, effectiveEpoch).
func snFleetRevokedTestLog(t testing.TB, block uint64, index uint, clientId [16]byte, generation uint64, effective uint64) types.Log {
	t.Helper()
	parsed, err := stabi.STCoordinatorMetaData.ParseABI()
	if err != nil {
		t.Fatal(err)
	}
	event := parsed.Events["FleetBindingRevoked"]
	data, err := event.Inputs.NonIndexed().Pack(generation, effective)
	if err != nil {
		t.Fatal(err)
	}
	return snFleetBindingTestLog(block, index, []common.Hash{event.ID, snFleetBindingTestTopic(clientId)}, data)
}

// FleetBindingCleaned(clientId, cleanedAtEpoch), both indexed.
func snFleetCleanedTestLog(t testing.TB, block uint64, index uint, clientId [16]byte, cleanedAt uint64) types.Log {
	t.Helper()
	parsed, err := stabi.STCoordinatorMetaData.ParseABI()
	if err != nil {
		t.Fatal(err)
	}
	return snFleetBindingTestLog(block, index, []common.Hash{parsed.Events["FleetBindingCleaned"].ID, snFleetBindingTestTopic(clientId), common.BigToHash(new(big.Int).SetUint64(cleanedAt))}, nil)
}

// The topics the sync filters on are STCoordinator's binding events, and the
// mirrored kinds are the coordinator's event names.
func TestSnFleetBindingTopicsAreTheCoordinatorEvents(t *testing.T) {
	parsed, err := stabi.STCoordinatorMetaData.ParseABI()
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Events["FleetBindingRevoked"].ID != stFleetBindingRevokedTopic || parsed.Events["FleetBindingCleaned"].ID != stFleetBindingCleanedTopic ||
		model.SnFleetBindingRevokedEventKind != stabi.STCoordinatorFleetBindingRevokedEventName || model.SnFleetBindingCleanedEventKind != stabi.STCoordinatorFleetBindingCleanedEventName {
		t.Fatal("a binding topic or kind differs from the coordinator ABI")
	}
}

// All three binding logs decode into mirror rows in one filtered range; a
// FleetBound row is byte-identical to the head-exclusion scan's row, so the
// two writers share st_event; a removed log is dropped and an undecodable one
// fails the range.
func TestCoreStClientFleetBindingEvents(t *testing.T) {
	clientId := [16]byte{7, 1}
	bound := snFleetBoundTestLog(t, 120, 3, clientId, 0x21, 0x31, 27, 9, 41, 52)
	revoked := snFleetRevokedTestLog(t, 130, 0, clientId, 9, 45)
	cleaned := snFleetCleanedTestLog(t, 140, 1, clientId, 46)
	removed := snFleetRevokedTestLog(t, 150, 0, clientId, 9, 44)
	removed.Removed = true
	fixture := &snFleetBindingRPC{t: t, address: snFleetBindingTestAddress, finalized: 999, logs: []types.Log{bound, revoked, cleaned, removed}}
	client := newSnFleetBindingClient(t, fixture)
	events, err := client.FleetBindingEvents(context.Background(), 100, 199)
	if err != nil || len(events) != 3 {
		t.Fatal("binding range", events, err)
	}
	wantKinds := []string{model.StFleetBoundEventKind, model.SnFleetBindingRevokedEventKind, model.SnFleetBindingCleanedEventKind}
	for index, event := range events {
		if event.Kind != wantKinds[index] || len(event.OriginalLog) == 0 || event.BlockHash != common.HexToHash(stBlockBatchHash(event.BlockNumber)).Hex() {
			t.Fatalf("event %d %+v", index, event)
		}
	}
	var revokedData model.SnFleetBindingRevokedEventData
	var cleanedData model.SnFleetBindingCleanedEventData
	if json.Unmarshal([]byte(events[1].DataJson), &revokedData) != nil || json.Unmarshal([]byte(events[2].DataJson), &cleanedData) != nil {
		t.Fatal("event data does not decode")
	}
	if revokedData != (model.SnFleetBindingRevokedEventData{ClientId: "0x07010000000000000000000000000000", Generation: "9", EffectiveEpoch: "45"}) ||
		cleanedData != (model.SnFleetBindingCleanedEventData{ClientId: "0x07010000000000000000000000000000", CleanedAtEpoch: "46"}) {
		t.Fatal("event data", revokedData, cleanedData)
	}
	exclusion := &stHeadExclusionRPC{t: t, address: snFleetBindingTestAddress, finalized: 999, logs: []types.Log{bound}}
	exclusionEvents, err := newStHeadExclusionClient(t, exclusion).FleetBoundEvents(context.Background(), 100, 199)
	if err != nil || len(exclusionEvents) != 1 || exclusionEvents[0].Kind != events[0].Kind || exclusionEvents[0].DataJson != events[0].DataJson ||
		exclusionEvents[0].BlockHash != events[0].BlockHash || exclusionEvents[0].TxHash != events[0].TxHash || exclusionEvents[0].LogIndex != events[0].LogIndex ||
		string(exclusionEvents[0].OriginalLog) != string(events[0].OriginalLog) {
		t.Fatal("the FleetBound rows of the two scans differ", exclusionEvents, events[0], err)
	}
	if ranges := fixture.ranges(); !slices.Equal(ranges, [][2]uint64{{100, 199}}) {
		t.Fatal("binding filter ranges", ranges)
	}
	undecodable := revoked
	undecodable.Data = undecodable.Data[:32]
	fixture.logs = []types.Log{undecodable}
	if events, err := client.FleetBindingEvents(context.Background(), 100, 199); events != nil || err == nil || !strings.Contains(err.Error(), "FleetBindingRevoked log 130:0") {
		t.Fatal("an undecodable revocation was accepted", events, err)
	}
	client.coordinator = nil
	if _, err := client.FleetBindingEvents(context.Background(), 100, 199); err == nil {
		t.Fatal("a client without the release coordinator read binding logs")
	}
}

// The sync's census: one client bound, revoked and rebound under a new
// generation; one bound then cleaned after its hotkey lost the UID; one bound
// for a later epoch; two clients of one fleet. Each range is applied, the
// checkpoint moves to the finalized head, and a second run reads only new
// blocks. The claimed fleets at each epoch follow bindingAt.
func TestSnSyncFleetBindingsAppliesRevocationAndCleanup(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		rebound, cleaned, later, memberA, memberB := [16]byte{0x11}, [16]byte{0x12}, [16]byte{0x13}, [16]byte{0x14}, [16]byte{0x15}
		fixture := &snFleetBindingRPC{t: t, address: snFleetBindingTestAddress, finalized: 2_400, logs: []types.Log{
			// rebound: generation 1 for 40..51, revoked from 45, rebound as
			// generation 2 for 45..56 to another fleet
			snFleetBoundTestLog(t, 400, 0, rebound, 0x21, 0x31, 21, 1, 40, 51),
			snFleetRevokedTestLog(t, 900, 0, rebound, 1, 45),
			snFleetBoundTestLog(t, 900, 1, rebound, 0x22, 0x32, 22, 2, 45, 56),
			// cleaned: bound for 40..51, cleaned at 43
			snFleetBoundTestLog(t, 401, 0, cleaned, 0x23, 0x33, 23, 5, 40, 51),
			snFleetCleanedTestLog(t, 1_500, 0, cleaned, 43),
			// later: bound from 60
			snFleetBoundTestLog(t, 1_600, 0, later, 0x24, 0x34, 24, 1, 60, 70),
			// one fleet of two members
			snFleetBoundTestLog(t, 1_700, 0, memberA, 0x25, 0x35, 25, 3, 41, 50),
			snFleetBoundTestLog(t, 1_700, 1, memberB, 0x25, 0x35, 25, 3, 41, 52),
		}}
		client := newSnFleetBindingClient(t, fixture)
		cfg := &StConfig{ChainId: 945, ContractAddress: snFleetBindingTestAddress, DeployBlock: 100}
		// two ranges this run, the rest of the way the next
		result, err := snSyncFleetBindings(ctx, cfg, client, 2)
		if err != nil || result.Applied != 8 || result.NextBlock != 2_100 {
			t.Fatal("first run", result, err)
		}
		result, err = snSyncFleetBindings(ctx, cfg, client, snFleetBindingSyncMaxRanges)
		if err != nil || result.Applied != 0 || result.NextBlock != 2_401 {
			t.Fatal("second run", result, err)
		}
		if ranges := fixture.ranges(); !slices.Equal(ranges, [][2]uint64{{100, 1_099}, {1_100, 2_099}, {2_100, 2_400}}) {
			t.Fatal("log ranges", ranges)
		}
		checkpoint := model.GetSnFleetBindingCheckpoint(ctx, cfg.DeploymentKey())
		if checkpoint.NextBlock != 2_401 || !strings.EqualFold(checkpoint.BlockHash, common.HexToHash(stBlockBatchHash(2_400)).Hex()) {
			t.Fatal("checkpoint", checkpoint)
		}
		// a run with nothing new reads nothing and changes nothing
		result, err = snSyncFleetBindings(ctx, cfg, client, snFleetBindingSyncMaxRanges)
		if err != nil || result.Applied != 0 || len(fixture.ranges()) != 3 {
			t.Fatal("idle run", result, err)
		}
		// every log is mirrored once, revocations and cleanups included
		for kind, want := range map[string]int{model.StFleetBoundEventKind: 6, model.SnFleetBindingRevokedEventKind: 1, model.SnFleetBindingCleanedEventKind: 1} {
			count := 0
			for _, event := range model.GetStEvents(ctx, cfg.DeploymentKey(), 0, 3_000) {
				if event.Kind == kind {
					count++
				}
			}
			if count != want {
				t.Fatalf("%d %s rows, want %d", count, kind, want)
			}
		}

		versions := model.GetSnFleetBindingVersions(ctx, cfg.DeploymentKey())
		if len(versions) != 6 {
			t.Fatalf("%d versions", len(versions))
		}
		clientIds := func(epoch uint64) []server.Id {
			ids := []server.Id{}
			for _, version := range model.SnFleetBindingsAt(versions, epoch) {
				ids = append(ids, version.ClientId)
			}
			return ids
		}
		id := func(clientId [16]byte) server.Id { return server.Id(clientId) }
		// epoch 42: generation 1 of rebound, cleaned before its cleanup, both members
		if got := clientIds(42); !slices.Equal(got, []server.Id{id(rebound), id(cleaned), id(memberA), id(memberB)}) {
			t.Fatal("epoch 42", got)
		}
		// epoch 44: the revocation ends generation 1 at 44; cleaned is gone at 43
		if got := clientIds(44); !slices.Equal(got, []server.Id{id(rebound), id(memberA), id(memberB)}) {
			t.Fatal("epoch 44", got)
		}
		at45 := model.SnFleetBindingsAt(versions, 45)
		if len(at45) != 3 || at45[0].ClientId != id(rebound) || at45[0].Generation != 2 || at45[0].Uid != 22 {
			t.Fatal("epoch 45: the rebound generation", at45)
		}
		if got := clientIds(51); !slices.Equal(got, []server.Id{id(rebound), id(memberB)}) {
			t.Fatal("epoch 51", got)
		}
		if got := clientIds(60); !slices.Equal(got, []server.Id{id(later)}) {
			t.Fatal("epoch 60", got)
		}

		// the claimed fleets at 42: UID 25 is held, UID 21 lost to another
		// hotkey, UID 23's holder unread
		holders := &model.SnUidHolders{SubnetEpochIndex: 9, Block: 2_390, ReadAt: time.Unix(1_800_000_000, 0), Holders: map[uint16]model.SnUidHolder{
			25: {Hotkey: [32]byte{0x35, 0xf2}, BlockAtRegistration: 77},
			21: {Hotkey: [32]byte{0x99}, BlockAtRegistration: 88},
		}}
		network := server.NewId()
		claimed := model.SnClaimedFleetsAt(versions, 42, holders, map[server.Id]server.Id{id(memberA): network})
		if len(claimed.Fleets) != 3 || claimed.UidBlock != 2_390 {
			t.Fatal("claimed fleets", claimed)
		}
		byUid := map[uint16]*model.SnClaimedFleet{}
		for _, fleet := range claimed.Fleets {
			byUid[fleet.Key.Uid] = fleet
		}
		fleet := byUid[25]
		if fleet == nil || !fleet.Live() || fleet.UidRegisteredBlock != 77 || fleet.ValidToEpoch != 52 || len(fleet.Members) != 2 ||
			fleet.Key != (model.SnFleetKey{FleetId: [32]byte{0x25, 0xf1}, Hotkey: [32]byte{0x35, 0xf2}, Generation: 3, Uid: 25}) ||
			fleet.Members[0].NetworkId == nil || *fleet.Members[0].NetworkId != network || fleet.Members[1].NetworkId != nil {
			t.Fatal("the two-member fleet", fleet)
		}
		if byUid[21] == nil || byUid[21].UidHeld != model.SnUidHeldNo || byUid[23] == nil || byUid[23].UidHeld != model.SnUidHeldUnknown {
			t.Fatal("lost and unknown UIDs", byUid[21], byUid[23])
		}
		if live := claimed.LiveFleets(); len(live) != 1 || live[0] != fleet {
			t.Fatal("live fleets", live)
		}
		if uids := model.SnFleetBindingUids(versions, 44, 45); !slices.Equal(uids, []uint16{21, 22, 25}) {
			t.Fatal("bound UIDs", uids)
		}
	})
}

// A log on a block that is not canonical, and a checkpoint whose block has
// since forked, each stop the sync without applying anything.
func TestSnSyncFleetBindingsRefusesNonCanonicalBlocks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := [16]byte{0x21}
		fixture := &snFleetBindingRPC{t: t, address: snFleetBindingTestAddress, finalized: 1_500, logs: []types.Log{
			snFleetBoundTestLog(t, 300, 0, clientId, 0x21, 0x31, 21, 1, 40, 51),
			snFleetRevokedTestLog(t, 1_200, 0, clientId, 1, 45),
		}, forked: map[uint64]bool{1_200: true}}
		client := newSnFleetBindingClient(t, fixture)
		cfg := &StConfig{ChainId: 945, ContractAddress: snFleetBindingTestAddress, DeployBlock: 100}
		result, err := snSyncFleetBindings(ctx, cfg, client, snFleetBindingSyncMaxRanges)
		var syncErr *snFleetBindingSyncError
		if !errors.As(err, &syncErr) || syncErr.reason != snFleetBindingSyncErrorCanonical || result.NextBlock != 1_100 {
			t.Fatal("a non-canonical log was applied", result, err)
		}
		versions := model.GetSnFleetBindingVersions(ctx, cfg.DeploymentKey())
		if len(versions) != 1 || versions[0].ValidToEpoch != 51 || versions[0].Revoked {
			t.Fatal("the first range is applied and the forked revocation is not", versions)
		}
		// the stored checkpoint's block forks: the sync halts
		fixture.forked = map[uint64]bool{1_099: true}
		_, err = snSyncFleetBindings(ctx, cfg, client, snFleetBindingSyncMaxRanges)
		if !errors.As(err, &syncErr) || syncErr.reason != snFleetBindingSyncErrorCanonical || !strings.Contains(err.Error(), "halts") {
			t.Fatal("a forked checkpoint did not halt the sync", err)
		}
		if model.GetSnFleetBindingCheckpoint(ctx, cfg.DeploymentKey()).NextBlock != 1_100 {
			t.Fatal("the checkpoint moved")
		}
	})
}

// A revocation of a generation the census never saw, and a second cleanup,
// contradict the census: the range is not applied, not even its st_event rows.
func TestSnSyncFleetBindingsRefusesAContradictoryCensus(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := [16]byte{0x31}
		cfg := &StConfig{ChainId: 945, ContractAddress: snFleetBindingTestAddress, DeployBlock: 100}
		fixture := &snFleetBindingRPC{t: t, address: snFleetBindingTestAddress, finalized: 600, logs: []types.Log{
			snFleetBoundTestLog(t, 300, 0, clientId, 0x21, 0x31, 21, 1, 40, 51),
			snFleetRevokedTestLog(t, 400, 0, clientId, 2, 45),
		}}
		client := newSnFleetBindingClient(t, fixture)
		_, err := snSyncFleetBindings(ctx, cfg, client, snFleetBindingSyncMaxRanges)
		var syncErr *snFleetBindingSyncError
		if !errors.As(err, &syncErr) || syncErr.reason != snFleetBindingSyncErrorCensus || !errors.Is(err, model.ErrSnFleetBindingCensus) {
			t.Fatal("an unknown revocation was applied", err)
		}
		if len(model.GetSnFleetBindingVersions(ctx, cfg.DeploymentKey())) != 0 || len(model.GetStEvents(ctx, cfg.DeploymentKey(), 0, 1_000)) != 0 || model.GetSnFleetBindingCheckpoint(ctx, cfg.DeploymentKey()).NextBlock != 0 {
			t.Fatal("a contradictory range left rows behind")
		}
		// two cleanups of one version
		fixture.logs = []types.Log{
			snFleetBoundTestLog(t, 300, 0, clientId, 0x21, 0x31, 21, 1, 40, 51),
			snFleetCleanedTestLog(t, 400, 0, clientId, 43),
			snFleetCleanedTestLog(t, 500, 0, clientId, 44),
		}
		_, err = snSyncFleetBindings(ctx, cfg, client, snFleetBindingSyncMaxRanges)
		if !errors.As(err, &syncErr) || syncErr.reason != snFleetBindingSyncErrorCensus {
			t.Fatal("a second cleanup was applied", err)
		}
		// a range that applies cleanly applies the same way again: the mirror
		// and the derived rows are idempotent
		fixture.logs = fixture.logs[:2]
		if _, err := snSyncFleetBindings(ctx, cfg, client, snFleetBindingSyncMaxRanges); err != nil {
			t.Fatal(err)
		}
		events := []*model.StChainEvent{}
		for _, log := range fixture.logs {
			batch, err := client.FleetBindingEvents(ctx, log.BlockNumber, log.BlockNumber)
			if err != nil {
				t.Fatal(err)
			}
			events = append(events, batch...)
		}
		if err := model.ApplySnFleetBindingRange(ctx, cfg.DeploymentKey(), events, model.SnFleetBindingCheckpoint{NextBlock: 601, BlockHash: common.HexToHash(stBlockBatchHash(600)).Hex()}); err != nil {
			t.Fatal("reapplying an applied range", err)
		}
		versions := model.GetSnFleetBindingVersions(ctx, cfg.DeploymentKey())
		if len(versions) != 1 || !versions[0].Cleaned || versions[0].CleanedAtEpoch != 43 {
			t.Fatal("reapplied census", versions)
		}
	})
}
