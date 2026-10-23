// The §22.6 native weight census at one finalized native block: the
// metagraph vectors, every permitted validator's hotkey and applied weight
// row, the enrolled validators' UIDs, the pending timelocked CRv4 commits,
// the stored drand round, and the vault escrow's UID. Native reads use the
// block's own verified runtime metadata; the escrow hotkey is a hash-pinned
// evm view at the finalized evm checkpoint of the same run.
package monitor

import (
	"context"
	"encoding/binary"
	"math/big"
	"strconv"

	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// One finalized native block and the runtime that produced it.
type subnetWeightCheckpoint struct {
	number      uint64
	hash        common.Hash
	specName    string
	specVersion uint32
}

func (self subnetWeightCheckpoint) runtimeIdentity() string {
	return self.specName + "/" + strconv.FormatUint(uint64(self.specVersion), 10)
}

// The SCALE tuple (u16, u16) of one applied weight.
type subnetWeightPair struct {
	Uid   types.U16
	Value types.U16
}

// The SCALE tuple of one pending timelocked commit.
type subnetWeightCommit struct {
	Account     [32]byte
	CommitBlock types.U64
	Ciphertext  types.Bytes
	RevealRound types.U64
}

// One permitted validator.
type subnetWeightValidator struct {
	uid        uint16
	hotkey     [32]byte
	row        []subnetWeightPair
	lastUpdate uint64
	dividends  uint16
	trust      uint16
}

// One enrolled validator; ordinal is its 1-based enrollment position.
type subnetWeightEnrolled struct {
	ordinal    int
	hotkey     [32]byte
	uid        uint16
	registered bool
}

// The complete or explicitly partial weight census.
type subnetWeightState struct {
	checkpoint         subnetWeightCheckpoint
	evmCheckpoint      subnetReplayEvmCheckpoint
	subnetworkN        uint16
	tempo              uint16
	revealPeriodEpochs uint64
	lastStoredRound    uint64
	permits            []bool
	validators         []*subnetWeightValidator
	validatorsComplete bool
	enrolled           []subnetWeightEnrolled
	commits            []subnetWeightCommit
	commitsComplete    bool
	escrowUid          uint16
	escrowRegistered   bool
}

func subnetWeightLittleEndian16(value uint16) []byte {
	var encoded [2]byte
	binary.LittleEndian.PutUint16(encoded[:], value)
	return encoded[:]
}

// Reads the native finalized head, its number, the genesis and the runtime.
func readSubnetWeightCheckpoint(ctx context.Context, rpc *subnetReplayRpc, pin *SubnetReplayPin) (subnetWeightCheckpoint, error) {
	var head *common.Hash
	var genesis *common.Hash
	calls := []*subnetReplayRpcCall{
		{method: "chain_getFinalizedHead", result: &head},
		{method: "chain_getBlockHash", params: []any{0}, result: &genesis},
	}
	if err := rpc.call(ctx, calls...); err != nil {
		return subnetWeightCheckpoint{}, err
	}
	if err := subnetReplayRpcFailed(calls...); err != nil {
		return subnetWeightCheckpoint{}, err
	}
	if genesis == nil || *genesis != pin.genesisHash {
		return subnetWeightCheckpoint{}, newSubnetReplayUnknown("chain-identity-mismatch", nil)
	}
	if head == nil || *head == (common.Hash{}) {
		return subnetWeightCheckpoint{}, newSubnetReplayUnknown("finalized-head-invalid", nil)
	}
	var header *struct {
		Number *hexutil.Big `json:"number"`
	}
	var version *struct {
		SpecName    string `json:"specName"`
		SpecVersion uint32 `json:"specVersion"`
	}
	calls = []*subnetReplayRpcCall{
		{method: "chain_getHeader", params: []any{head.Hex()}, result: &header},
		{method: "state_getRuntimeVersion", params: []any{head.Hex()}, result: &version},
	}
	if err := rpc.call(ctx, calls...); err != nil {
		return subnetWeightCheckpoint{}, err
	}
	if err := subnetReplayRpcFailed(calls...); err != nil {
		return subnetWeightCheckpoint{}, err
	}
	if header == nil || header.Number == nil || !(*big.Int)(header.Number).IsUint64() || version == nil || version.SpecName == "" || version.SpecVersion == 0 {
		return subnetWeightCheckpoint{}, newSubnetReplayUnknown("finalized-head-invalid", nil)
	}
	return subnetWeightCheckpoint{number: (*big.Int)(header.Number).Uint64(), hash: *head, specName: version.SpecName, specVersion: version.SpecVersion}, nil
}

// Proves the native checkpoint is still canonical at its height.
func recheckSubnetWeightCheckpoint(ctx context.Context, rpc *subnetReplayRpc, checkpoint subnetWeightCheckpoint) error {
	var atHeight *common.Hash
	call := &subnetReplayRpcCall{method: "chain_getBlockHash", params: []any{checkpoint.number}, result: &atHeight}
	if err := rpc.call(ctx, call); err != nil {
		return err
	}
	if err := subnetReplayRpcFailed(call); err != nil {
		return err
	}
	if atHeight == nil || *atHeight != checkpoint.hash {
		return newSubnetReplayUnknown("checkpoint-not-canonical", nil)
	}
	return nil
}

// Reads the census. Vectors must agree with the subnet's size; a census past
// its bounds is explicitly partial.
func readSubnetWeightState(ctx context.Context, rpc *subnetReplayRpc, runtimes *subnetWeightRuntimes, pin *SubnetReplayPin, enrolledHotkeys [][32]byte, settings *SubnetReplaySettings) (*subnetWeightState, error) {
	contracts, err := subnetSettlementAbis()
	if err != nil {
		return nil, newSubnetReplayUnknown("binding-unavailable", err)
	}
	evmCheckpoint, err := readSubnetReplayEvmCheckpoint(ctx, rpc, pin)
	if err != nil {
		return nil, err
	}
	escrowView := newSubnetReplayView(contracts.vault, pin.settlementVault, "escrowHotkey")
	if err := readSubnetReplayViews(ctx, rpc, evmCheckpoint, escrowView); err != nil {
		return nil, err
	}
	escrowHotkey, err := subnetReplayValue[[32]byte](escrowView, 0)
	if err != nil {
		return nil, err
	}
	checkpoint, err := readSubnetWeightCheckpoint(ctx, rpc, pin)
	if err != nil {
		return nil, err
	}
	runtime, err := readSubnetWeightRuntime(ctx, rpc, runtimes, checkpoint.runtimeIdentity(), checkpoint.hash.Hex(), settings)
	if err != nil {
		return nil, err
	}
	if runtime.reason != "" {
		return nil, newSubnetReplayUnknown(runtime.reason, nil)
	}
	meta := runtime.meta
	block := checkpoint.hash.Hex()
	netuid := subnetWeightLittleEndian16(pin.netuid)
	state := &subnetWeightState{checkpoint: checkpoint, evmCheckpoint: evmCheckpoint, validatorsComplete: true, commitsComplete: true}

	var subnetworkN, tempo types.U16
	var revealPeriod, lastStoredRound types.U64
	var permits []types.Bool
	var lastUpdate []types.U64
	var dividends, trust []types.U16
	reads := []*subnetWeightRead{}
	add := func(name string, value any, args ...[]byte) error {
		read, err := newSubnetWeightRead(meta, name, value, args...)
		if err != nil {
			return err
		}
		reads = append(reads, read)
		return nil
	}
	for _, request := range []struct {
		name  string
		value any
		args  [][]byte
	}{
		{name: "SubnetworkN", value: &subnetworkN, args: [][]byte{netuid}},
		{name: "Tempo", value: &tempo, args: [][]byte{netuid}},
		{name: "RevealPeriodEpochs", value: &revealPeriod, args: [][]byte{netuid}},
		{name: "ValidatorPermit", value: &permits, args: [][]byte{netuid}},
		{name: "LastUpdate", value: &lastUpdate, args: [][]byte{netuid}},
		{name: "Dividends", value: &dividends, args: [][]byte{netuid}},
		{name: "ValidatorTrust", value: &trust, args: [][]byte{netuid}},
		{name: "LastStoredRound", value: &lastStoredRound},
	} {
		if err := add(request.name, request.value, request.args...); err != nil {
			return nil, err
		}
	}
	enrolledUids := make([]types.U16, len(enrolledHotkeys))
	enrolledReads := make([]*subnetWeightRead, len(enrolledHotkeys))
	for i, hotkey := range enrolledHotkeys {
		if err := add("Uids", &enrolledUids[i], netuid, hotkey[:]); err != nil {
			return nil, err
		}
		enrolledReads[i] = reads[len(reads)-1]
	}
	var escrowUid types.U16
	if err := add("Uids", &escrowUid, netuid, escrowHotkey[:]); err != nil {
		return nil, err
	}
	escrowRead := reads[len(reads)-1]
	if err := readSubnetWeightStorage(ctx, rpc, meta, block, reads...); err != nil {
		return nil, err
	}
	size := int(subnetworkN)
	if len(permits) != size || len(lastUpdate) != size || len(dividends) != size || len(trust) != size {
		return nil, newSubnetReplayUnknown("metagraph-vectors-inconsistent", nil)
	}
	state.subnetworkN, state.tempo, state.revealPeriodEpochs, state.lastStoredRound = uint16(subnetworkN), uint16(tempo), uint64(revealPeriod), uint64(lastStoredRound)
	state.escrowUid, state.escrowRegistered = uint16(escrowUid), escrowRead.present
	state.permits = make([]bool, size)
	for i, permit := range permits {
		state.permits[i] = bool(permit)
	}
	for i, hotkey := range enrolledHotkeys {
		state.enrolled = append(state.enrolled, subnetWeightEnrolled{ordinal: i + 1, hotkey: hotkey, uid: uint16(enrolledUids[i]), registered: enrolledReads[i].present && int(enrolledUids[i]) < size})
	}

	// Permitted validators: hotkey and applied row, within the row bound.
	reads = reads[:0]
	for uid := 0; uid < size; uid++ {
		if !state.permits[uid] {
			continue
		}
		if settings.MaximumValidatorRows <= len(state.validators) {
			state.validatorsComplete = false
			break
		}
		validator := &subnetWeightValidator{uid: uint16(uid), lastUpdate: uint64(lastUpdate[uid]), dividends: uint16(dividends[uid]), trust: uint16(trust[uid])}
		state.validators = append(state.validators, validator)
		if err := add("Keys", &validator.hotkey, netuid, subnetWeightLittleEndian16(uint16(uid))); err != nil {
			return nil, err
		}
		if err := add("Weights", &validator.row, netuid, subnetWeightLittleEndian16(uint16(uid))); err != nil {
			return nil, err
		}
	}
	if err := readSubnetWeightStorage(ctx, rpc, meta, block, reads...); err != nil {
		return nil, err
	}

	// Pending timelocked commits of this subnet's main mechanism.
	prefix, err := subnetWeightPrefix(meta, "TimelockedWeightCommits", netuid)
	if err != nil {
		return nil, err
	}
	keys, complete, err := readSubnetWeightKeys(ctx, rpc, prefix, block, settings.NativeKeysPerPage, settings.NativeMaximumKeyPages)
	if err != nil {
		return nil, err
	}
	state.commitsComplete = complete
	queues := make([][]subnetWeightCommit, len(keys))
	reads = reads[:0]
	item := subnetWeightItems["TimelockedWeightCommits"]
	for i, key := range keys {
		reads = append(reads, &subnetWeightRead{item: item, key: key, value: &queues[i]})
	}
	if err := readSubnetWeightStorage(ctx, rpc, meta, block, reads...); err != nil {
		return nil, err
	}
	for _, queue := range queues {
		state.commits = append(state.commits, queue...)
	}
	return state, nil
}
