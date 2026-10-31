// Package stmetagraph reads a subnet's on-chain incentive census at one
// finalized native block of the subtensor archive (LEADERTOP.md §6.2). It
// follows the subnet monitor's reader (monitor/subnet_weight_reader.go and
// monitor/subnet_weight_metadata.go), which the server cannot import:
//   - the checkpoint is the finalized head, read with its header and runtime
//     version, on a chain whose genesis matches the pinned one;
//   - every storage key and value is decoded with the block's own runtime
//     metadata, decoded once per runtime, after each item's key and value
//     shape is checked against the runtime's type registry, so a runtime that
//     changes an item is refused rather than silently misdecoded;
//   - a value decodes with every byte consumed, and an absent value takes the
//     runtime-declared default exactly as the chain applies it;
//   - the checkpoint is proven canonical at its height after the reads.
//
// Reads are bounded: one census is a few JSON-RPC batches whose response
// bytes are counted against a budget, so a reader never pulls more than the
// budget from the archive per read. The runtime metadata itself is fetched
// once per runtime version per process and has its own bound.
package stmetagraph

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/centrifuge/go-substrate-rpc-client/v4/scale"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types/codec"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/urfoundation/sn/v2026/chain"
)

// Rpc is one JSON-RPC endpoint that executes a batch in one exchange. The
// subtensor gateway serves the substrate methods beside the EVM ones, so the
// st client's *rpc.Client is one.
type Rpc interface {
	BatchCallContext(ctx context.Context, batch []rpc.BatchElem) error
}

// The reasons a read fails. Each is a fixed metric label; the cause stays on
// the error for logs.
const (
	ReasonRpc                  = "rpc"
	ReasonChainIdentity        = "chain_identity"
	ReasonFinalizedHead        = "finalized_head"
	ReasonMetadata             = "metadata"
	ReasonStorageShape         = "storage_shape"
	ReasonStorageMalformed     = "storage_malformed"
	ReasonVectorsInconsistent  = "vectors_inconsistent"
	ReasonBudget               = "budget"
	ReasonCheckpointNotCanonic = "checkpoint_not_canonical"
)

// Every reason, for metric label initialization.
var Reasons = []string{ReasonRpc, ReasonChainIdentity, ReasonFinalizedHead, ReasonMetadata, ReasonStorageShape, ReasonStorageMalformed, ReasonVectorsInconsistent, ReasonBudget, ReasonCheckpointNotCanonic}

// ReadError is a failed read with its fixed reason.
type ReadError struct {
	Reason string
	Cause  error
}

func (self *ReadError) Error() string {
	if self.Cause == nil {
		return "metagraph read: " + self.Reason
	}
	return fmt.Sprintf("metagraph read: %s: %v", self.Reason, self.Cause)
}

func (self *ReadError) Unwrap() error { return self.Cause }

func readError(reason string, cause error) error {
	return &ReadError{Reason: reason, Cause: cause}
}

// ErrorReason is the fixed reason of a read error, or ReasonRpc for any
// other error.
func ErrorReason(err error) string {
	var readErr *ReadError
	if errors.As(err, &readErr) {
		return readErr.Reason
	}
	return ReasonRpc
}

// One storage item the reader decodes, with its required registry shape.
type item struct {
	pallet string
	name   string
	key    string
	value  string
}

// The exact shapes of the SubtensorModule items in recorded runtimes 482 and
// 486 (sn crv4/testdata), in the monitor's notation: primitives by index
// (prim0 bool, prim3 u8, prim4 u16, prim6 u64), sequences, tuples, arrays and
// composites with their paths. Incentive, LastUpdate and Weights are keyed by
// the mechanism storage index, which for mechanism 0 is the netuid itself.
var items = map[string]item{
	"SubnetworkN":         {pallet: "SubtensorModule", name: "SubnetworkN", key: "subtensor_runtime_common::NetUid{prim4}", value: "prim4"},
	"SubnetEpochIndex":    {pallet: "SubtensorModule", name: "SubnetEpochIndex", key: "subtensor_runtime_common::NetUid{prim4}", value: "prim6"},
	"Tempo":               {pallet: "SubtensorModule", name: "Tempo", key: "subtensor_runtime_common::NetUid{prim4}", value: "prim4"},
	"ActivityCutoff":      {pallet: "SubtensorModule", name: "ActivityCutoff", key: "subtensor_runtime_common::NetUid{prim4}", value: "prim4"},
	"Incentive":           {pallet: "SubtensorModule", name: "Incentive", key: "subtensor_runtime_common::NetUidStorageIndex{prim4}", value: "Vec<sp_arithmetic::per_things::PerU16{prim4}>"},
	"LastUpdate":          {pallet: "SubtensorModule", name: "LastUpdate", key: "subtensor_runtime_common::NetUidStorageIndex{prim4}", value: "Vec<prim6>"},
	"ValidatorPermit":     {pallet: "SubtensorModule", name: "ValidatorPermit", key: "subtensor_runtime_common::NetUid{prim4}", value: "Vec<prim0>"},
	"ValidatorTrust":      {pallet: "SubtensorModule", name: "ValidatorTrust", key: "subtensor_runtime_common::NetUid{prim4}", value: "Vec<sp_arithmetic::per_things::PerU16{prim4}>"},
	"Keys":                {pallet: "SubtensorModule", name: "Keys", key: "(subtensor_runtime_common::NetUid{prim4},prim4)", value: "sp_core::crypto::AccountId32{[32;prim3]}"},
	"BlockAtRegistration": {pallet: "SubtensorModule", name: "BlockAtRegistration", key: "(subtensor_runtime_common::NetUid{prim4},prim4)", value: "prim6"},
	"Weights":             {pallet: "SubtensorModule", name: "Weights", key: "(subtensor_runtime_common::NetUidStorageIndex{prim4},prim4)", value: "Vec<(prim4,prim4)>"},
}

// A decoded runtime metadata whose item shapes were checked once.
type runtime struct {
	meta *types.Metadata
	// "" when every item has its reviewed shape
	refusal string
}

// Reader holds the runtimes it has verified, keyed by spec name and version:
// a runtime upgrade always changes the spec version, so one fetch serves every
// block of a runtime. Safe for concurrent use.
type Reader struct {
	stateLock sync.Mutex
	runtimes  map[string]*runtime

	// The largest raw metadata accepted, in bytes.
	MaxMetadataBytes int
	// The most batch members sent in one exchange.
	MaxBatchCalls int
}

func NewReader() *Reader {
	return &Reader{runtimes: map[string]*runtime{}, MaxMetadataBytes: 4 * 1024 * 1024, MaxBatchCalls: 50}
}

// Checkpoint is one finalized native block and the runtime that produced it.
type Checkpoint struct {
	Number      uint64
	Hash        common.Hash
	SpecName    string
	SpecVersion uint32
}

func (self Checkpoint) runtimeIdentity() string {
	return self.SpecName + "/" + strconv.FormatUint(uint64(self.SpecVersion), 10)
}

// One read's exchange and its response byte budget. Not safe for concurrent
// use.
type session struct {
	reader *Reader
	rpc    Rpc
	budget int
	used   int
}

// One JSON-RPC call whose raw result is kept for exact decoding.
type call struct {
	method string
	args   []any
	result json.RawMessage
}

// Issues the calls in bounded batches; every member must succeed. Response
// bytes count against the session budget, metadata excepted.
func (self *session) call(ctx context.Context, counted bool, calls ...*call) error {
	batchSize := max(1, self.reader.MaxBatchCalls)
	for start := 0; start < len(calls); start += batchSize {
		end := min(start+batchSize, len(calls))
		batch := make([]rpc.BatchElem, end-start)
		for i, c := range calls[start:end] {
			c.result = nil
			batch[i] = rpc.BatchElem{Method: c.method, Args: c.args, Result: &c.result}
		}
		if err := self.rpc.BatchCallContext(ctx, batch); err != nil {
			return readError(ReasonRpc, err)
		}
		for i, element := range batch {
			if element.Error != nil {
				return readError(ReasonRpc, fmt.Errorf("%s: %w", calls[start+i].method, element.Error))
			}
			if counted {
				self.used += len(calls[start+i].result)
			}
		}
		if counted && self.budget < self.used {
			return readError(ReasonBudget, fmt.Errorf("read %d response bytes, budget %d", self.used, self.budget))
		}
	}
	return nil
}

// ReadCheckpoint reads the finalized head, its number and runtime, after
// checking that the endpoint serves the chain with the pinned genesis.
// Response bytes count against budget.
func (self *Reader) ReadCheckpoint(ctx context.Context, endpoint Rpc, genesis common.Hash, budget int) (Checkpoint, int, error) {
	s := &session{reader: self, rpc: endpoint, budget: budget}
	checkpoint, err := s.readCheckpoint(ctx, genesis)
	return checkpoint, s.used, err
}

func (self *session) readCheckpoint(ctx context.Context, genesis common.Hash) (Checkpoint, error) {
	head, genesisCall := &call{method: "chain_getFinalizedHead"}, &call{method: "chain_getBlockHash", args: []any{0}}
	if err := self.call(ctx, true, head, genesisCall); err != nil {
		return Checkpoint{}, err
	}
	var headHash, genesisHash *common.Hash
	if json.Unmarshal(head.result, &headHash) != nil || json.Unmarshal(genesisCall.result, &genesisHash) != nil {
		return Checkpoint{}, readError(ReasonFinalizedHead, errors.New("finalized head or genesis is not a hash"))
	}
	if genesisHash == nil || *genesisHash != genesis {
		return Checkpoint{}, readError(ReasonChainIdentity, fmt.Errorf("genesis %v is not the pinned %s", genesisHash, genesis.Hex()))
	}
	if headHash == nil || *headHash == (common.Hash{}) {
		return Checkpoint{}, readError(ReasonFinalizedHead, errors.New("finalized head is empty"))
	}
	header, version := &call{method: "chain_getHeader", args: []any{headHash.Hex()}}, &call{method: "state_getRuntimeVersion", args: []any{headHash.Hex()}}
	if err := self.call(ctx, true, header, version); err != nil {
		return Checkpoint{}, err
	}
	var decodedHeader *struct {
		Number *hexutil.Big `json:"number"`
	}
	var decodedVersion *struct {
		SpecName    string `json:"specName"`
		SpecVersion uint32 `json:"specVersion"`
	}
	if json.Unmarshal(header.result, &decodedHeader) != nil || json.Unmarshal(version.result, &decodedVersion) != nil ||
		decodedHeader == nil || decodedHeader.Number == nil || !(*big.Int)(decodedHeader.Number).IsUint64() ||
		decodedVersion == nil || decodedVersion.SpecName == "" || decodedVersion.SpecVersion == 0 {
		return Checkpoint{}, readError(ReasonFinalizedHead, errors.New("finalized header or runtime version is invalid"))
	}
	return Checkpoint{Number: (*big.Int)(decodedHeader.Number).Uint64(), Hash: *headHash, SpecName: decodedVersion.SpecName, SpecVersion: decodedVersion.SpecVersion}, nil
}

// Recheck proves the checkpoint is still canonical at its height.
func (self *Reader) Recheck(ctx context.Context, endpoint Rpc, checkpoint Checkpoint, budget int) (int, error) {
	s := &session{reader: self, rpc: endpoint, budget: budget}
	return s.used, s.recheck(ctx, checkpoint)
}

func (self *session) recheck(ctx context.Context, checkpoint Checkpoint) error {
	atHeight := &call{method: "chain_getBlockHash", args: []any{checkpoint.Number}}
	if err := self.call(ctx, true, atHeight); err != nil {
		return err
	}
	var hash *common.Hash
	if json.Unmarshal(atHeight.result, &hash) != nil || hash == nil || *hash != checkpoint.Hash {
		return readError(ReasonCheckpointNotCanonic, fmt.Errorf("block %d is %v, checkpoint %s", checkpoint.Number, hash, checkpoint.Hash.Hex()))
	}
	return nil
}

// The verified runtime of one checkpoint, fetched once per runtime.
func (self *session) runtime(ctx context.Context, checkpoint Checkpoint) (*runtime, error) {
	identity := checkpoint.runtimeIdentity()
	self.reader.stateLock.Lock()
	cached, ok := self.reader.runtimes[identity]
	self.reader.stateLock.Unlock()
	if !ok {
		metadata := &call{method: "state_getMetadata", args: []any{checkpoint.Hash.Hex()}}
		if err := self.call(ctx, false, metadata); err != nil {
			return nil, err
		}
		var encoded *string
		if json.Unmarshal(metadata.result, &encoded) != nil || encoded == nil || self.reader.MaxMetadataBytes*2+2 < len(*encoded) {
			return nil, readError(ReasonMetadata, errors.New("metadata is absent or oversized"))
		}
		raw, err := hexutil.Decode(*encoded)
		if err != nil {
			return nil, readError(ReasonMetadata, err)
		}
		meta := &types.Metadata{}
		if err := codec.Decode(raw, meta); err != nil || meta.Version != 14 {
			return nil, readError(ReasonMetadata, fmt.Errorf("metadata is not v14: %v", err))
		}
		cached = &runtime{meta: meta, refusal: verifyShapes(meta)}
		self.reader.stateLock.Lock()
		self.reader.runtimes[identity] = cached
		self.reader.stateLock.Unlock()
	}
	if cached.refusal != "" {
		return nil, readError(ReasonStorageShape, fmt.Errorf("runtime %s: %s", identity, cached.refusal))
	}
	return cached, nil
}

// VerifyShapes returns "" when every item the reader decodes has its reviewed
// key and value shape in meta, and otherwise the first refusal.
func VerifyShapes(meta *types.Metadata) string {
	return verifyShapes(meta)
}

func verifyShapes(meta *types.Metadata) string {
	lookup := map[int64]types.PortableTypeV14{}
	for _, entry := range meta.AsMetadataV14.Lookup.Types {
		lookup[entry.ID.Int64()] = entry
	}
	var shape func(id int64, depth int) string
	shape = func(id int64, depth int) string {
		entry, ok := lookup[id]
		if !ok || 8 < depth {
			return "?"
		}
		path := make([]string, 0, len(entry.Type.Path))
		for _, segment := range entry.Type.Path {
			path = append(path, string(segment))
		}
		definition := entry.Type.Def
		switch {
		case definition.IsComposite:
			fields := make([]string, 0, len(definition.Composite.Fields))
			for _, field := range definition.Composite.Fields {
				fields = append(fields, shape(field.Type.Int64(), depth+1))
			}
			return strings.Join(path, "::") + "{" + strings.Join(fields, ",") + "}"
		case definition.IsSequence:
			return "Vec<" + shape(definition.Sequence.Type.Int64(), depth+1) + ">"
		case definition.IsArray:
			return fmt.Sprintf("[%d;%s]", definition.Array.Len, shape(definition.Array.Type.Int64(), depth+1))
		case definition.IsTuple:
			fields := make([]string, 0, len(definition.Tuple))
			for _, field := range definition.Tuple {
				fields = append(fields, shape(field.Int64(), depth+1))
			}
			return "(" + strings.Join(fields, ",") + ")"
		case definition.IsPrimitive:
			return fmt.Sprintf("prim%d", definition.Primitive.Si0TypeDefPrimitive)
		}
		return "?"
	}
	names := make([]string, 0, len(items))
	for name := range items {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		item := items[name]
		entry, err := meta.FindStorageEntryMetadata(item.pallet, item.name)
		if err != nil {
			return item.name + " is absent"
		}
		v14, ok := entry.(types.StorageEntryMetadataV14)
		if !ok || !v14.Type.IsMap {
			return item.name + " is not a map"
		}
		if shape(v14.Type.AsMap.Key.Int64(), 0) != item.key || shape(v14.Type.AsMap.Value.Int64(), 0) != item.value {
			return item.name + " changed shape"
		}
	}
	return ""
}

// One storage read: its key, raw result and decode target.
type storageRead struct {
	item    item
	key     string
	value   any
	present bool
	call    call
}

func (self *runtime) read(name string, value any, args ...[]byte) (*storageRead, error) {
	item, ok := items[name]
	if !ok {
		return nil, readError(ReasonStorageShape, fmt.Errorf("%s is not a reviewed item", name))
	}
	key, err := types.CreateStorageKey(self.meta, item.pallet, item.name, args...)
	if err != nil {
		return nil, readError(ReasonStorageShape, err)
	}
	return &storageRead{item: item, key: key.Hex(), value: value}, nil
}

// Executes every read at one block and decodes each value with every byte
// consumed; an absent value takes the runtime-declared default.
func (self *session) readStorage(ctx context.Context, runtime *runtime, block common.Hash, reads ...*storageRead) error {
	calls := make([]*call, len(reads))
	for i, read := range reads {
		read.call = call{method: "state_getStorage", args: []any{read.key, block.Hex()}}
		calls[i] = &read.call
	}
	if err := self.call(ctx, true, calls...); err != nil {
		return err
	}
	for _, read := range reads {
		var encoded *string
		if err := json.Unmarshal(read.call.result, &encoded); err != nil {
			return readError(ReasonStorageMalformed, fmt.Errorf("%s: %w", read.item.name, err))
		}
		if encoded == nil {
			entry, err := runtime.meta.FindStorageEntryMetadata(read.item.pallet, read.item.name)
			if err != nil {
				return readError(ReasonStorageShape, err)
			}
			present, err := chain.DecodeStorageFallback(entry, read.value)
			if err != nil {
				return readError(ReasonStorageMalformed, fmt.Errorf("%s default: %w", read.item.name, err))
			}
			// a defaulted value is the chain's value, but not a stored one
			_ = present
			read.present = false
			continue
		}
		raw, err := hexutil.Decode(*encoded)
		if err != nil {
			return readError(ReasonStorageMalformed, fmt.Errorf("%s: %w", read.item.name, err))
		}
		reader := bytes.NewReader(raw)
		if err := scale.NewDecoder(reader).Decode(read.value); err != nil {
			return readError(ReasonStorageMalformed, fmt.Errorf("%s: %w", read.item.name, err))
		}
		if reader.Len() != 0 {
			return readError(ReasonStorageMalformed, fmt.Errorf("%s has %d trailing bytes", read.item.name, reader.Len()))
		}
		read.present = true
	}
	return nil
}

func littleEndian16(value uint16) []byte {
	return binary.LittleEndian.AppendUint16(nil, value)
}

// SubnetEpochIndex reads the subnet's epoch index at a checkpoint: the
// cheap read that tells whether a new tempo has closed.
func (self *Reader) SubnetEpochIndex(ctx context.Context, endpoint Rpc, checkpoint Checkpoint, netuid uint16, budget int) (uint64, int, error) {
	s := &session{reader: self, rpc: endpoint, budget: budget}
	runtime, err := s.runtime(ctx, checkpoint)
	if err != nil {
		return 0, s.used, err
	}
	var index types.U64
	read, err := runtime.read("SubnetEpochIndex", &index, littleEndian16(netuid))
	if err != nil {
		return 0, s.used, err
	}
	if err := s.readStorage(ctx, runtime, checkpoint.Hash, read); err != nil {
		return 0, s.used, err
	}
	return uint64(index), s.used, nil
}

// WeightEntry is one applied weight of a validator's row.
type WeightEntry struct {
	Uid   uint16
	Value uint16
}

// Identity is the holder of one UID: its hotkey and the block it registered
// at. A hotkey that deregisters and registers again gets a new block.
type Identity struct {
	Hotkey              [32]byte
	BlockAtRegistration uint64
}

// ValidatorRow is one permitted validator's applied weight row.
type ValidatorRow struct {
	Uid uint16
	Row []WeightEntry
}

// Census is the incentive census at one finalized block. Vectors have
// SubnetworkN entries, indexed by UID.
type Census struct {
	Checkpoint       Checkpoint
	Netuid           uint16
	SubnetworkN      uint16
	SubnetEpochIndex uint64
	Tempo            uint16
	ActivityCutoff   uint16
	Incentive        []uint16
	LastUpdate       []uint64
	ValidatorPermit  []bool
	ValidatorTrust   []uint16
	// Keys and BlockAtRegistration of every permitted validator and every
	// requested UID below SubnetworkN.
	Identities map[uint16]Identity
	// The applied rows of permitted validators: CensusOptions.FirstRowUids
	// first, then by UID, at most CensusOptions.MaxValidatorRows of them.
	ValidatorRows []ValidatorRow
	// false when more validators hold a permit than rows were read, by the
	// row bound or the response budget
	ValidatorRowsComplete bool
	// response bytes the census read, metadata excluded
	ResponseBytes int
}

// CensusOptions bound one census read.
type CensusOptions struct {
	// UIDs whose holder is read besides the permitted validators'.
	IdentityUids []uint16
	// Permitted validators whose rows are read before the others, such as the
	// reference validator's.
	FirstRowUids []uint16
	// The most permitted validators whose rows are read.
	MaxValidatorRows int
	// The most response bytes the census may read, metadata excluded. Rows
	// stop early, and the census says so, rather than pass it.
	MaxResponseBytes int
}

// Rows are read a few at a time so the budget stops them between batches,
// always leaving room for the final canonical recheck.
const (
	rowBatch       = 8
	recheckReserve = 256
)

// ReadCensus reads the incentive census at the finalized head: the subnet's
// size, epoch index, tempo and activity cutoff; the Incentive, LastUpdate,
// ValidatorPermit and ValidatorTrust vectors; the holder of every permitted
// validator's UID and of every requested UID; and the applied Weights rows of
// the permitted validators. The checkpoint is rechecked as canonical after
// the reads.
func (self *Reader) ReadCensus(ctx context.Context, endpoint Rpc, genesis common.Hash, netuid uint16, options CensusOptions) (*Census, error) {
	s := &session{reader: self, rpc: endpoint, budget: options.MaxResponseBytes}
	if s.budget <= 0 {
		return nil, readError(ReasonBudget, errors.New("no response byte budget"))
	}
	checkpoint, err := s.readCheckpoint(ctx, genesis)
	if err != nil {
		return nil, err
	}
	census, err := s.readCensusAt(ctx, checkpoint, netuid, options)
	if err != nil {
		return nil, err
	}
	if err := s.recheck(ctx, checkpoint); err != nil {
		return nil, err
	}
	census.ResponseBytes = s.used
	return census, nil
}

func (self *session) readCensusAt(ctx context.Context, checkpoint Checkpoint, netuid uint16, options CensusOptions) (*Census, error) {
	runtime, err := self.runtime(ctx, checkpoint)
	if err != nil {
		return nil, err
	}
	netuidArg := littleEndian16(netuid)
	var subnetworkN, tempo, activityCutoff types.U16
	var epochIndex types.U64
	var incentive, trust []types.U16
	var lastUpdate []types.U64
	var permits []types.Bool
	reads := []*storageRead{}
	for _, request := range []struct {
		name  string
		value any
	}{
		{"SubnetworkN", &subnetworkN},
		{"SubnetEpochIndex", &epochIndex},
		{"Tempo", &tempo},
		{"ActivityCutoff", &activityCutoff},
		{"Incentive", &incentive},
		{"LastUpdate", &lastUpdate},
		{"ValidatorPermit", &permits},
		{"ValidatorTrust", &trust},
	} {
		read, err := runtime.read(request.name, request.value, netuidArg)
		if err != nil {
			return nil, err
		}
		reads = append(reads, read)
	}
	if err := self.readStorage(ctx, runtime, checkpoint.Hash, reads...); err != nil {
		return nil, err
	}
	size := int(subnetworkN)
	if len(incentive) != size || len(lastUpdate) != size || len(permits) != size || len(trust) != size {
		return nil, readError(ReasonVectorsInconsistent, fmt.Errorf("subnet size %d, incentive %d, last update %d, permits %d, trust %d", size, len(incentive), len(lastUpdate), len(permits), len(trust)))
	}
	census := &Census{
		Checkpoint: checkpoint, Netuid: netuid, SubnetworkN: uint16(subnetworkN), SubnetEpochIndex: uint64(epochIndex),
		Tempo: uint16(tempo), ActivityCutoff: uint16(activityCutoff),
		Incentive: make([]uint16, size), LastUpdate: make([]uint64, size), ValidatorPermit: make([]bool, size), ValidatorTrust: make([]uint16, size),
		Identities: map[uint16]Identity{}, ValidatorRowsComplete: true,
	}
	permitted := []uint16{}
	for uid := 0; uid < size; uid++ {
		census.Incentive[uid] = uint16(incentive[uid])
		census.LastUpdate[uid] = uint64(lastUpdate[uid])
		census.ValidatorPermit[uid] = bool(permits[uid])
		census.ValidatorTrust[uid] = uint16(trust[uid])
		if census.ValidatorPermit[uid] {
			permitted = append(permitted, uint16(uid))
		}
	}

	// holders of the permitted and requested UIDs, then the permitted rows
	uids := append([]uint16(nil), permitted...)
	for _, uid := range options.IdentityUids {
		if int(uid) < size {
			uids = append(uids, uid)
		}
	}
	slices.Sort(uids)
	uids = slices.Compact(uids)
	hotkeys := make([]types.AccountID, len(uids))
	registrations := make([]types.U64, len(uids))
	reads = reads[:0]
	for i, uid := range uids {
		hotkeyRead, err := runtime.read("Keys", &hotkeys[i], netuidArg, littleEndian16(uid))
		if err != nil {
			return nil, err
		}
		registrationRead, err := runtime.read("BlockAtRegistration", &registrations[i], netuidArg, littleEndian16(uid))
		if err != nil {
			return nil, err
		}
		reads = append(reads, hotkeyRead, registrationRead)
	}
	if err := self.readStorage(ctx, runtime, checkpoint.Hash, reads...); err != nil {
		return nil, err
	}
	for i, uid := range uids {
		census.Identities[uid] = Identity{Hotkey: hotkeys[i], BlockAtRegistration: uint64(registrations[i])}
	}

	// the first rows, then every other permitted row by UID
	rowUids := []uint16{}
	for _, uid := range options.FirstRowUids {
		if int(uid) < size && census.ValidatorPermit[uid] && !slices.Contains(rowUids, uid) {
			rowUids = append(rowUids, uid)
		}
	}
	for _, uid := range permitted {
		if !slices.Contains(rowUids, uid) {
			rowUids = append(rowUids, uid)
		}
	}
	if options.MaxValidatorRows < len(rowUids) {
		rowUids = rowUids[:max(0, options.MaxValidatorRows)]
		census.ValidatorRowsComplete = false
	}
	type rawEntry struct {
		Uid   types.U16
		Value types.U16
	}
	// a row holds at most one (u16, u16) per UID: 8 hex digits each
	rowBytes := size*8 + 16
	for start := 0; start < len(rowUids); start += rowBatch {
		batch := rowUids[start:min(start+rowBatch, len(rowUids))]
		if self.budget < self.used+len(batch)*rowBytes+recheckReserve {
			census.ValidatorRowsComplete = false
			break
		}
		rows := make([][]rawEntry, len(batch))
		reads = reads[:0]
		for i, uid := range batch {
			read, err := runtime.read("Weights", &rows[i], netuidArg, littleEndian16(uid))
			if err != nil {
				return nil, err
			}
			reads = append(reads, read)
		}
		if err := self.readStorage(ctx, runtime, checkpoint.Hash, reads...); err != nil {
			return nil, err
		}
		for i, uid := range batch {
			row := make([]WeightEntry, len(rows[i]))
			for j, entry := range rows[i] {
				row[j] = WeightEntry{Uid: uint16(entry.Uid), Value: uint16(entry.Value)}
			}
			census.ValidatorRows = append(census.ValidatorRows, ValidatorRow{Uid: uid, Row: row})
		}
	}
	return census, nil
}
