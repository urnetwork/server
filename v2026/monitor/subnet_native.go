// SIGNALS.md §22.0 native Subtensor reads at explicit finalized hashes. The
// runtime identity is the complete version/code/metadata tuple; storage keys
// and absent-value defaults come from metadata verified against an approved
// identity, never from a pinned guess, so a changed layout cannot be misread.
package monitor

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"

	gsrpc "github.com/centrifuge/go-substrate-rpc-client/v4"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types/codec"
	"github.com/centrifuge/go-substrate-rpc-client/v4/xxhash"

	snchain "github.com/urfoundation/sn/v2026/chain"
	"github.com/urfoundation/sn/v2026/crv4"
)

// The well-known storage key of the runtime Wasm.
const subnetRuntimeCodeKey = "0x3a636f6465"

// The runtime approval states. Only approved runtimes bind storage layouts.
const (
	subnetRuntimeApproved   = "approved"
	subnetRuntimeUnreviewed = "unreviewed"
	subnetRuntimeConflict   = "conflict"
)

// One native endpoint of a run session.
type subnetNativeReader struct {
	session  *subnetRpcSession
	endpoint subnetRpcEndpoint
}

func (self subnetNativeReader) client() *subnetNativeClient {
	return &subnetNativeClient{session: self.session, endpoint: self.endpoint}
}

// A lowercase 0x-prefixed 32-byte hash.
func subnetParseHash(value string) (types.Hash, bool) {
	if len(value) != 66 || !strings.HasPrefix(value, "0x") {
		return types.Hash{}, false
	}
	raw, err := hex.DecodeString(value[2:])
	if err != nil || len(raw) != 32 {
		return types.Hash{}, false
	}
	return types.NewHash(raw), true
}

// A canonical 0x hex quantity without leading zeros.
func subnetParseQuantity(value string) (uint64, bool) {
	if len(value) < 3 || len(value) > 18 || !strings.HasPrefix(value, "0x") || (len(value) > 3 && value[2] == '0') {
		return 0, false
	}
	number, err := strconv.ParseUint(value[2:], 16, 64)
	return number, err == nil
}

func subnetHashHex(hash types.Hash) string {
	return "0x" + hex.EncodeToString(hash[:])
}

func (self subnetNativeReader) finalizedHead(ctx context.Context) (types.Hash, error) {
	var raw string
	if err := self.session.call(ctx, self.endpoint, "chain_getFinalizedHead", nil, &raw); err != nil {
		return types.Hash{}, err
	}
	hash, ok := subnetParseHash(raw)
	if !ok {
		return types.Hash{}, subnetReadFailure("finalized-head-invalid", nil)
	}
	return hash, nil
}

// The header number and parent of one block hash.
func (self subnetNativeReader) header(ctx context.Context, hash types.Hash) (uint64, types.Hash, error) {
	var header *struct {
		ParentHash string `json:"parentHash"`
		Number     string `json:"number"`
	}
	if err := self.session.call(ctx, self.endpoint, "chain_getHeader", []any{subnetHashHex(hash)}, &header); err != nil {
		return 0, types.Hash{}, err
	}
	if header == nil {
		return 0, types.Hash{}, subnetReadFailure("header-absent", nil)
	}
	number, numberOk := subnetParseQuantity(header.Number)
	parent, parentOk := subnetParseHash(header.ParentHash)
	if !numberOk || !parentOk {
		return 0, types.Hash{}, subnetReadFailure("header-invalid", nil)
	}
	return number, parent, nil
}

// The canonical hash at a height; false when the endpoint has no such block.
func (self subnetNativeReader) blockHash(ctx context.Context, number uint64) (types.Hash, bool, error) {
	var raw *string
	if err := self.session.call(ctx, self.endpoint, "chain_getBlockHash", []any{number}, &raw); err != nil {
		return types.Hash{}, false, err
	}
	if raw == nil {
		return types.Hash{}, false, nil
	}
	hash, ok := subnetParseHash(*raw)
	if !ok {
		return types.Hash{}, false, subnetReadFailure("block-hash-invalid", nil)
	}
	return hash, true, nil
}

func (self subnetNativeReader) runtimeVersion(ctx context.Context, at types.Hash) (crv4.RuntimeVersionIdentity, error) {
	var raw json.RawMessage
	if err := self.session.call(ctx, self.endpoint, "state_getRuntimeVersion", []any{subnetHashHex(at)}, &raw); err != nil {
		return crv4.RuntimeVersionIdentity{}, err
	}
	version, err := crv4.DecodeRuntimeVersionIdentity(raw)
	if err != nil {
		return crv4.RuntimeVersionIdentity{}, subnetReadFailure("runtime-version-invalid", err)
	}
	return version, nil
}

// The node-computed BLAKE2b-256 of the runtime Wasm at a block.
func (self subnetNativeReader) codeHash(ctx context.Context, at types.Hash) (string, error) {
	var raw *string
	if err := self.session.call(ctx, self.endpoint, "state_getStorageHash", []any{subnetRuntimeCodeKey, subnetHashHex(at)}, &raw); err != nil {
		return "", err
	}
	if raw == nil {
		return "", subnetReadFailure("runtime-code-absent", nil)
	}
	value := strings.ToLower(*raw)
	if _, ok := subnetParseHash(value); !ok {
		return "", subnetReadFailure("runtime-code-hash-invalid", nil)
	}
	return value, nil
}

// Exact metadata bytes and their BLAKE2b-256, through sn's bounded decoder.
func (self subnetNativeReader) metadata(ctx context.Context, at types.Hash) (*types.Metadata, string, error) {
	var encoded string
	if err := self.session.call(ctx, self.endpoint, "state_getMetadata", []any{subnetHashHex(at)}, &encoded); err != nil {
		return nil, "", err
	}
	metadata, digest, err := crv4.DecodeRuntimeMetadata(encoded)
	if err != nil {
		return nil, "", subnetReadFailure("runtime-metadata-invalid", err)
	}
	return metadata, strings.ToLower(digest), nil
}

// The key prefix of a storage map's first argument. Only an Identity first
// hasher yields a prefix that enumerates exactly that argument's entries.
func subnetStorageMapPrefix(metadata *types.Metadata, pallet, item string, first []byte) ([]byte, error) {
	entry, err := metadata.FindStorageEntryMetadata(pallet, item)
	if err != nil {
		return nil, subnetReadFailure("storage-entry-invalid", err)
	}
	hashers, err := entry.Hashers()
	if err != nil || len(hashers) < 2 {
		return nil, subnetReadFailure("storage-layout-unsupported", err)
	}
	entryV14, ok := entry.(types.StorageEntryMetadataV14)
	if !ok || !entryV14.Type.AsMap.Hashers[0].IsIdentity {
		return nil, subnetReadFailure("storage-layout-unsupported", nil)
	}
	prefix := append(xxhash.New128([]byte(pallet)).Sum(nil), xxhash.New128([]byte(item)).Sum(nil)...)
	return append(prefix, first...), nil
}

// Storage keys under a prefix, at most count, after an optional start key.
func (self subnetNativeReader) keysPaged(ctx context.Context, prefix []byte, count int, start []byte, at types.Hash) ([][]byte, error) {
	var startParam any
	if len(start) != 0 {
		startParam = codec.HexEncodeToString(start)
	}
	var raw []string
	if err := self.session.call(ctx, self.endpoint, "state_getKeysPaged", []any{codec.HexEncodeToString(prefix), count, startParam, subnetHashHex(at)}, &raw); err != nil {
		return nil, err
	}
	if len(raw) > count {
		return nil, subnetReadFailure("keys-overflow", nil)
	}
	keys := make([][]byte, 0, len(raw))
	for _, item := range raw {
		key, err := codec.HexDecodeString(item)
		if err != nil || len(key) < len(prefix) || string(key[:len(prefix)]) != string(prefix) {
			return nil, subnetReadFailure("keys-invalid", err)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// A bound view for sn's reviewed storage readers over this endpoint.
func (self subnetNativeReader) bound(metadata *types.Metadata, genesis types.Hash) *crv4.Chain {
	return &crv4.Chain{API: &gsrpc.SubstrateAPI{Client: self.client()}, Meta: metadata, GenesisHash: genesis}
}

// One storage value at the pinned block, with the runtime's absent-value
// semantics: a ValueQuery default decodes, an absent OptionQuery is false.
func subnetReadStorage(ctx context.Context, chain *crv4.Chain, at types.Hash, pallet, item string, value any, args ...[]byte) (bool, error) {
	key, err := types.CreateStorageKey(chain.Meta, pallet, item, args...)
	if err != nil {
		return false, subnetReadFailure("storage-key-invalid", err)
	}
	present, err := snchain.ReadStorageAtContext(ctx, chain, key, pallet, item, value, at)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		var readErr *subnetReadError
		if !errors.As(err, &readErr) {
			return false, subnetReadFailure("storage-decode-invalid", err)
		}
		return false, err
	}
	return present, nil
}

// The approval of one observed runtime against the code-owned sn catalog and
// the reviewed successors. A known version with other code is a conflict.
type subnetRuntimeApproval struct {
	status   string
	artifact crv4.RuntimeArtifactIdentity
}

func subnetApproveRuntime(version crv4.RuntimeVersionIdentity, codeHash string, reviewed []SubnetReviewedRuntime) subnetRuntimeApproval {
	candidates := crv4.ReviewedRuntimeArtifacts()
	for _, runtime := range reviewed {
		candidates = append(candidates, crv4.RuntimeArtifactIdentity{
			Version:      crv4.RuntimeVersionIdentity{SpecName: runtime.SpecName, SpecVersion: runtime.SpecVersion, TransactionVersion: runtime.TransactionVersion, StateVersion: runtime.StateVersion},
			CodeHash:     runtime.CodeHash,
			MetadataHash: runtime.MetadataHash,
		})
	}
	conflict := false
	for _, candidate := range candidates {
		if candidate.Version != version {
			continue
		}
		if strings.ToLower(candidate.CodeHash) == codeHash {
			candidate.CodeHash = strings.ToLower(candidate.CodeHash)
			candidate.MetadataHash = strings.ToLower(candidate.MetadataHash)
			return subnetRuntimeApproval{status: subnetRuntimeApproved, artifact: candidate}
		}
		conflict = true
	}
	if conflict {
		return subnetRuntimeApproval{status: subnetRuntimeConflict}
	}
	return subnetRuntimeApproval{status: subnetRuntimeUnreviewed}
}

// An approved runtime whose exact metadata hash was verified once.
type subnetVerifiedRuntime struct {
	artifact crv4.RuntimeArtifactIdentity
	metadata *types.Metadata
}

// Verified runtimes keyed by their complete identity, so a different approval
// can never reuse another runtime's layout. Bounded; eviction costs one read.
type subnetRuntimeCache struct {
	stateLock sync.Mutex
	runtimes  map[crv4.RuntimeArtifactIdentity]*subnetVerifiedRuntime
	order     []crv4.RuntimeArtifactIdentity
}

const maximumSubnetCachedRuntimes = 4

func newSubnetRuntimeCache() *subnetRuntimeCache {
	return &subnetRuntimeCache{runtimes: map[crv4.RuntimeArtifactIdentity]*subnetVerifiedRuntime{}}
}

func (self *subnetRuntimeCache) lookup(artifact crv4.RuntimeArtifactIdentity) (*subnetVerifiedRuntime, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	runtime, ok := self.runtimes[artifact]
	return runtime, ok
}

func (self *subnetRuntimeCache) remember(runtime *subnetVerifiedRuntime) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if _, ok := self.runtimes[runtime.artifact]; ok {
		return
	}
	if len(self.order) == maximumSubnetCachedRuntimes {
		delete(self.runtimes, self.order[0])
		self.order = self.order[1:]
	}
	self.runtimes[runtime.artifact] = runtime
	self.order = append(self.order, runtime.artifact)
}

// Reads and verifies metadata for an approved runtime unless already known.
// A metadata hash other than the approved one is returned as a conflict.
func (self *subnetRuntimeCache) verify(ctx context.Context, reader subnetNativeReader, at types.Hash, artifact crv4.RuntimeArtifactIdentity) (*subnetVerifiedRuntime, string, error) {
	if runtime, ok := self.lookup(artifact); ok {
		return runtime, subnetRuntimeApproved, nil
	}
	metadata, digest, err := reader.metadata(ctx, at)
	if err != nil {
		return nil, "", err
	}
	if digest != artifact.MetadataHash {
		return nil, subnetRuntimeConflict, nil
	}
	runtime := &subnetVerifiedRuntime{artifact: artifact, metadata: metadata}
	self.remember(runtime)
	return runtime, subnetRuntimeApproved, nil
}

// Little-endian u16 storage argument.
func subnetNetuidArg(netuid uint16) []byte {
	return snchain.NetuidArg(netuid)
}

// The exact stored bytes of a value, for a reviewed decoder that takes the
// raw encoding rather than a Scale target.
type subnetRawStorageValue []byte

// One storage value in a batch, with its own presence and error.
type subnetStorageRead struct {
	pallet  string
	item    string
	args    [][]byte
	value   any
	present bool
	err     error
}

// Reads many storage values at the pinned block in bounded batches, with the
// same absent-value semantics as sn's reader: a ValueQuery default decodes
// from metadata, an absent OptionQuery stays absent. A transport failure
// fails the whole read; a decode failure is that value's error.
func subnetReadStorageBatch(ctx context.Context, chain *crv4.Chain, at types.Hash, reads []*subnetStorageRead) error {
	client, ok := chain.API.Client.(*subnetNativeClient)
	if !ok {
		return subnetReadFailure("storage-client-invalid", nil)
	}
	limit := client.session.limits.maxBatchCalls
	for start := 0; start < len(reads); start += limit {
		end := min(start+limit, len(reads))
		raws := make([]*string, end-start)
		calls := make([]subnetRpcCall, 0, end-start)
		for i := start; i < end; i++ {
			key, err := types.CreateStorageKey(chain.Meta, reads[i].pallet, reads[i].item, reads[i].args...)
			if err != nil {
				return subnetReadFailure("storage-key-invalid", err)
			}
			calls = append(calls, subnetRpcCall{method: "state_getStorage", params: []any{codec.HexEncodeToString(key), subnetHashHex(at)}, result: &raws[i-start]})
		}
		if err := client.session.batch(ctx, client.endpoint, calls); err != nil {
			return err
		}
		for i := start; i < end; i++ {
			read := reads[i]
			if err := calls[i-start].err; err != nil {
				read.err = err
				continue
			}
			raw := raws[i-start]
			if raw == nil {
				entry, err := chain.Meta.FindStorageEntryMetadata(read.pallet, read.item)
				if err != nil {
					read.err = subnetReadFailure("storage-entry-invalid", err)
					continue
				}
				read.present, err = snchain.DecodeStorageFallback(entry, read.value)
				if err != nil {
					read.err = subnetReadFailure("storage-decode-invalid", err)
				}
				continue
			}
			value, err := codec.HexDecodeString(*raw)
			if target, ok := read.value.(*subnetRawStorageValue); ok && err == nil {
				*target = value
			} else if err == nil {
				err = codec.Decode(value, read.value)
			}
			if err != nil {
				read.err = subnetReadFailure("storage-decode-invalid", err)
				continue
			}
			read.present = true
		}
	}
	return nil
}

// One applied native weight pair.
type subnetWeightPair struct {
	Uid    uint16
	Weight uint16
}
