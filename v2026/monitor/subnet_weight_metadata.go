// Native storage access for the §22.6 weight reader. Storage keys come from
// the finalized block's own runtime metadata, and every item's key and value
// shape is checked against that runtime's type registry before decoding, so a
// runtime that changes an item is unknown rather than silently misdecoded.
// Values must decode with every byte consumed; an absent value uses the
// runtime-declared default exactly as the chain applies it.
package monitor

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/centrifuge/go-substrate-rpc-client/v4/scale"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types/codec"
	"github.com/centrifuge/go-substrate-rpc-client/v4/xxhash"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/urfoundation/sn/v2026/chain"
)

// One storage item the reader decodes, with its required registry shape.
type subnetWeightItem struct {
	pallet string
	name   string
	key    string
	value  string
}

// The exact shapes of the reviewed SubtensorModule and Drand items. A shape is
// the scale-info structure: primitives by index, sequences, tuples, arrays and
// composites with their paths.
var subnetWeightItems = map[string]subnetWeightItem{
	"SubnetworkN":             {pallet: "SubtensorModule", name: "SubnetworkN", key: "subtensor_runtime_common::NetUid{prim4}", value: "prim4"},
	"Tempo":                   {pallet: "SubtensorModule", name: "Tempo", key: "subtensor_runtime_common::NetUid{prim4}", value: "prim4"},
	"RevealPeriodEpochs":      {pallet: "SubtensorModule", name: "RevealPeriodEpochs", key: "subtensor_runtime_common::NetUid{prim4}", value: "prim6"},
	"ValidatorPermit":         {pallet: "SubtensorModule", name: "ValidatorPermit", key: "subtensor_runtime_common::NetUid{prim4}", value: "Vec<prim0>"},
	"LastUpdate":              {pallet: "SubtensorModule", name: "LastUpdate", key: "subtensor_runtime_common::NetUidStorageIndex{prim4}", value: "Vec<prim6>"},
	"Dividends":               {pallet: "SubtensorModule", name: "Dividends", key: "subtensor_runtime_common::NetUid{prim4}", value: "Vec<sp_arithmetic::per_things::PerU16{prim4}>"},
	"ValidatorTrust":          {pallet: "SubtensorModule", name: "ValidatorTrust", key: "subtensor_runtime_common::NetUid{prim4}", value: "Vec<sp_arithmetic::per_things::PerU16{prim4}>"},
	"Keys":                    {pallet: "SubtensorModule", name: "Keys", key: "(subtensor_runtime_common::NetUid{prim4},prim4)", value: "sp_core::crypto::AccountId32{[32;prim3]}"},
	"Uids":                    {pallet: "SubtensorModule", name: "Uids", key: "(subtensor_runtime_common::NetUid{prim4},sp_core::crypto::AccountId32{[32;prim3]})", value: "prim4"},
	"Weights":                 {pallet: "SubtensorModule", name: "Weights", key: "(subtensor_runtime_common::NetUidStorageIndex{prim4},prim4)", value: "Vec<(prim4,prim4)>"},
	"TimelockedWeightCommits": {pallet: "SubtensorModule", name: "TimelockedWeightCommits", key: "(subtensor_runtime_common::NetUidStorageIndex{prim4},prim6)", value: "Vec<(sp_core::crypto::AccountId32{[32;prim3]},prim6,bounded_collections::bounded_vec::BoundedVec{Vec<prim3>},prim6)>"},
	"LastStoredRound":         {pallet: "Drand", name: "LastStoredRound", value: "prim6"},
}

// A decoded runtime metadata whose item shapes were verified once.
type subnetWeightRuntime struct {
	meta   *types.Metadata
	reason string
}

// Decoded runtimes keyed by spec name and version. A runtime upgrade always
// changes the spec version, so one fetch serves every block of a runtime.
// Safe for concurrent use.
type subnetWeightRuntimes struct {
	stateLock sync.Mutex
	runtimes  map[string]*subnetWeightRuntime
}

func newSubnetWeightRuntimes() *subnetWeightRuntimes {
	return &subnetWeightRuntimes{runtimes: map[string]*subnetWeightRuntime{}}
}

func (self *subnetWeightRuntimes) Get(identity string) (*subnetWeightRuntime, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	runtime, ok := self.runtimes[identity]
	return runtime, ok
}

func (self *subnetWeightRuntimes) Put(identity string, runtime *subnetWeightRuntime) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.runtimes[identity] = runtime
}

// Reads and decodes the metadata of one finalized block, then verifies the
// shape of every item; a verified runtime is retained for its spec version.
func readSubnetWeightRuntime(ctx context.Context, rpc *subnetReplayRpc, runtimes *subnetWeightRuntimes, identity string, block string, settings *SubnetReplaySettings) (*subnetWeightRuntime, error) {
	if runtime, ok := runtimes.Get(identity); ok {
		return runtime, nil
	}
	var encoded *string
	call := &subnetReplayRpcCall{method: "state_getMetadata", params: []any{block}, result: &encoded}
	if err := rpc.call(ctx, call); err != nil {
		return nil, err
	}
	if err := subnetReplayRpcFailed(call); err != nil {
		return nil, err
	}
	if encoded == nil || settings.MetadataMaximumBytes*2+2 < len(*encoded) {
		return nil, newSubnetReplayUnknown("metadata-unavailable-or-oversized", nil)
	}
	raw, err := hexutil.Decode(*encoded)
	if err != nil {
		return nil, newSubnetReplayUnknown("metadata-malformed", err)
	}
	meta := &types.Metadata{}
	if err := codec.Decode(raw, meta); err != nil || meta.Version != 14 {
		return nil, newSubnetReplayUnknown("metadata-unsupported", err)
	}
	runtime := &subnetWeightRuntime{meta: meta, reason: verifySubnetWeightShapes(meta)}
	runtimes.Put(identity, runtime)
	return runtime, nil
}

// Returns "" when every item has its reviewed key and value shape.
func verifySubnetWeightShapes(meta *types.Metadata) string {
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
	for _, item := range subnetWeightItems {
		entry, err := meta.FindStorageEntryMetadata(item.pallet, item.name)
		if err != nil {
			return "storage-item-absent"
		}
		v14, ok := entry.(types.StorageEntryMetadataV14)
		if !ok {
			return "storage-item-unsupported"
		}
		if item.key == "" {
			if !v14.Type.IsPlainType || shape(v14.Type.AsPlainType.Int64(), 0) != item.value {
				return "storage-shape-unsupported"
			}
			continue
		}
		if !v14.Type.IsMap || shape(v14.Type.AsMap.Key.Int64(), 0) != item.key || shape(v14.Type.AsMap.Value.Int64(), 0) != item.value {
			return "storage-shape-unsupported"
		}
	}
	return ""
}

// One storage read in a batch: its key, raw result and decode target.
type subnetWeightRead struct {
	item    subnetWeightItem
	key     string
	value   any
	present bool
	encoded *string
	call    subnetReplayRpcCall
}

// Builds one keyed read; args are the exact scale encodings of the keys.
func newSubnetWeightRead(meta *types.Metadata, name string, value any, args ...[]byte) (*subnetWeightRead, error) {
	item, ok := subnetWeightItems[name]
	if !ok {
		return nil, newSubnetReplayUnknown("storage-item-unknown", nil)
	}
	key, err := types.CreateStorageKey(meta, item.pallet, item.name, args...)
	if err != nil {
		return nil, newSubnetReplayUnknown("storage-key-underivable", err)
	}
	return &subnetWeightRead{item: item, key: key.Hex(), value: value}, nil
}

// Executes every read at one native block and decodes each value with every
// byte consumed; an absent value takes the runtime-declared default.
func readSubnetWeightStorage(ctx context.Context, rpc *subnetReplayRpc, meta *types.Metadata, block string, reads ...*subnetWeightRead) error {
	calls := make([]*subnetReplayRpcCall, len(reads))
	for i, read := range reads {
		read.encoded = nil
		read.call = subnetReplayRpcCall{method: "state_getStorage", params: []any{read.key, block}, result: &read.encoded}
		calls[i] = &read.call
	}
	if err := rpc.call(ctx, calls...); err != nil {
		return err
	}
	if err := subnetReplayRpcFailed(calls...); err != nil {
		return err
	}
	for _, read := range reads {
		if read.encoded == nil {
			entry, err := meta.FindStorageEntryMetadata(read.item.pallet, read.item.name)
			if err != nil {
				return newSubnetReplayUnknown("storage-item-absent", err)
			}
			present, err := chain.DecodeStorageFallback(entry, read.value)
			if err != nil {
				return newSubnetReplayUnknown("storage-default-malformed", err)
			}
			read.present = present
			continue
		}
		raw, err := hexutil.Decode(*read.encoded)
		if err != nil {
			return newSubnetReplayUnknown("storage-value-malformed", err)
		}
		if err := subnetWeightDecodeExact(raw, read.value); err != nil {
			return err
		}
		read.present = true
	}
	return nil
}

// Decodes and refuses trailing bytes.
func subnetWeightDecodeExact(raw []byte, value any) error {
	reader := bytes.NewReader(raw)
	if err := scale.NewDecoder(reader).Decode(value); err != nil {
		return newSubnetReplayUnknown("storage-value-malformed", err)
	}
	if reader.Len() != 0 {
		return newSubnetReplayUnknown("storage-value-trailing", nil)
	}
	return nil
}

// The storage prefix of one first-key group of a map: both twox128 names and
// the first key's own metadata hasher, exactly as the full key begins.
func subnetWeightPrefix(meta *types.Metadata, name string, first []byte) (string, error) {
	item := subnetWeightItems[name]
	entry, err := meta.FindStorageEntryMetadata(item.pallet, item.name)
	if err != nil {
		return "", newSubnetReplayUnknown("storage-item-absent", err)
	}
	hashers, err := entry.Hashers()
	if err != nil || len(hashers) == 0 {
		return "", newSubnetReplayUnknown("storage-key-underivable", err)
	}
	if _, err := hashers[0].Write(first); err != nil {
		return "", newSubnetReplayUnknown("storage-key-underivable", err)
	}
	prefix := append(xxhash.New128([]byte(item.pallet)).Sum(nil), xxhash.New128([]byte(item.name)).Sum(nil)...)
	return hexutil.Encode(append(prefix, hashers[0].Sum(nil)...)), nil
}

// Enumerates every key under a prefix at one block within the page bound.
func readSubnetWeightKeys(ctx context.Context, rpc *subnetReplayRpc, prefix string, block string, pageKeys int, maximumPages int) ([]string, bool, error) {
	keys := []string{}
	start := prefix
	for page := 0; page < maximumPages; page++ {
		var batch []string
		params := []any{prefix, pageKeys, start, block}
		call := &subnetReplayRpcCall{method: "state_getKeysPaged", params: params, result: &batch}
		if err := rpc.call(ctx, call); err != nil {
			return nil, false, err
		}
		if err := subnetReplayRpcFailed(call); err != nil {
			return nil, false, err
		}
		for _, key := range batch {
			if !strings.HasPrefix(key, prefix) || key <= start && start != prefix {
				return nil, false, newSubnetReplayUnknown("storage-keys-invalid", nil)
			}
			keys = append(keys, key)
			start = key
		}
		if len(batch) < pageKeys {
			return keys, true, nil
		}
	}
	return keys, false, nil
}
