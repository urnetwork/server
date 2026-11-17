package controller

// Pay with Gamma: the Subtensor chain reader and writer (mmm/BTCPAY.md §18).
//
// Gamma is SN25's dollar-valued service credit. A customer mints it into the
// deposit at the hotkey we assigned their account; our spender charges that
// deposit with spend_gamma, through the GammaSpend proxy key in the vault; and
// a purchase is credited only on our own finalized GammaSpent that carries
// the charge's reference. This file is everything that touches the chain:
//
//   - one JSON-RPC transport over HTTP, tried in order over the configured
//     endpoints (the archive node by default, st.yml's subtensor gateway)
//   - the runtime of each block, read at the block's PARENT state, with its
//     metadata cached by spec version. Events are decoded with the metadata of
//     the runtime that emitted them, so a runtime upgrade inside the scanned
//     range never decodes a block with the wrong layout.
//   - a shape check of every call, event, storage item and signed extension
//     the Gamma code reads or writes. A runtime whose shapes differ is
//     unsupported: the biller pauses and says so (metrics, the terms
//     endpoint), it never crash-loops and never decodes on a guess.
//   - typed reads: deposits, services, the TAO/USD oracle, the gamma flags,
//     the spender's proxies, accounts, hotkey owners
//   - the one write: Proxy.proxy(spender, GammaSpend,
//     SubtensorModule.spend_gamma(25, entries)), signed by the billing key with
//     a mortal era born at a finalized block, so a batch that is not included
//     within its era can be proven dead and its charges re-batched safely.
//
// The block reader is not Gamma-specific: it is meant to be shared with the
// plain-TAO reader of the crypto rail (BTCPAY.md §17.3), which needs the same
// per-block runtime and event decoding.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/centrifuge/go-substrate-rpc-client/v4/registry"
	"github.com/centrifuge/go-substrate-rpc-client/v4/registry/parser"
	"github.com/centrifuge/go-substrate-rpc-client/v4/scale"
	"github.com/centrifuge/go-substrate-rpc-client/v4/signature"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types/codec"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types/extrinsic"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types/extrinsic/extensions"
	"golang.org/x/crypto/blake2b"

	"github.com/urnetwork/server/v2026"
)

func init() {
	// Subtensor's own signed extensions. Each is zero-size (PhantomData) in
	// the runtimes the shape check admits, so it adds nothing to the payload;
	// the wrapper of ChargeTransactionPayment carries its tip. The shape check
	// refuses a runtime with any other unhandled, non-zero-size extension.
	fns := extrinsic.PayloadMutatorFns
	if _, ok := fns[extensions.SignedExtensionName("ChargeTransactionPaymentWrapper")]; !ok {
		fns[extensions.SignedExtensionName("ChargeTransactionPaymentWrapper")] = fns[extensions.ChargeTransactionPaymentSignedExtension]
	}
	for _, name := range []string{
		"SudoTransactionExtension",
		"CheckShieldedTxValidity",
		"SubtensorTransactionExtension",
		"DrandPriority",
	} {
		if _, ok := fns[extensions.SignedExtensionName(name)]; !ok {
			fns[extensions.SignedExtensionName(name)] = func(payload *extrinsic.Payload) {}
		}
	}
}

// One gamma is 10^9 units, and one gamma is one US dollar: a price in
// nano-dollars (model.NanoCents) is its amount in gamma units.
const gammaUnitsPerGamma = 1_000_000_000

// The chain's limits (PR #3300; the Gamma guide).
const (
	gammaMaxSpendsPerCall = 512
	// a mint needs a TAO/USD feed at most this many blocks old
	gammaPriceMaxAgeBlocks = 300
	// the smallest mint, in gamma units (0.1 gamma)
	gammaMinMintUnits = 100_000_000
	// Proxy.ProxyType::GammaSpend
	gammaSpendProxyTypeName = "GammaSpend"
)

// ----- the transport -----

// gammaRpcError is a JSON-RPC error answered by a node.
type gammaRpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (self *gammaRpcError) Error() string {
	if len(self.Data) == 0 {
		return fmt.Sprintf("rpc error %d: %s", self.Code, self.Message)
	}
	return fmt.Sprintf("rpc error %d: %s (%s)", self.Code, self.Message, string(self.Data))
}

// gammaRpc is a Subtensor JSON-RPC transport. The recorded-fixture transport
// of the tests implements it too.
type gammaRpc interface {
	Call(ctx context.Context, result any, method string, params ...any) error
}

const gammaRpcTimeout = 20 * time.Second

// gammaRpc answers can be large (a block's events, a runtime's metadata),
// but never this large.
const gammaRpcMaxResponseBytes = 32 << 20

// gammaHttpRpc posts each call to the first endpoint that answers. A node's
// own JSON-RPC error is an answer and is not retried elsewhere.
type gammaHttpRpc struct {
	urls   []string
	client *http.Client
}

func newGammaHttpRpc(urls []string) *gammaHttpRpc {
	return &gammaHttpRpc{
		urls:   slices.Clone(urls),
		client: server.NewHttpClient(gammaRpcTimeout),
	}
}

func (self *gammaHttpRpc) Call(ctx context.Context, result any, method string, params ...any) error {
	if len(self.urls) == 0 {
		return errors.New("no subtensor rpc endpoint is configured")
	}
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return err
	}
	var lastErr error
	for _, url := range self.urls {
		err := self.callOne(ctx, url, body, result)
		if err == nil {
			return nil
		}
		var rpcErr *gammaRpcError
		if errors.As(err, &rpcErr) {
			return err
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return lastErr
}

func (self *gammaHttpRpc) callOne(ctx context.Context, url string, body []byte, result any) error {
	callCtx, cancel := context.WithTimeout(ctx, gammaRpcTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := self.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("subtensor rpc: http %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, gammaRpcMaxResponseBytes))
	if err != nil {
		return err
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *gammaRpcError  `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("subtensor rpc: malformed answer: %w", err)
	}
	if envelope.Error != nil {
		return envelope.Error
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(envelope.Result, result)
}

// ----- runtimes -----

// gammaRuntime is one runtime version's metadata, its event decoders and the
// Gamma shapes it was checked for.
type gammaRuntime struct {
	SpecVersion        uint32
	TransactionVersion uint32
	Meta               *types.Metadata

	events registry.EventRegistry
	shape  *gammaShape
	// why the runtime is not supported; nil when it is
	shapeErr error
}

// Supported reports whether every shape the Gamma code relies on is as
// reviewed.
func (self *gammaRuntime) Supported() error {
	if self.shapeErr != nil {
		return self.shapeErr
	}
	return nil
}

// gammaShape is what the Gamma code reads from a supported runtime's
// metadata.
type gammaShape struct {
	subtensorPallet     uint8
	gammaSpendProxyType uint8
	// SubtensorModule error index -> name
	subtensorErrors map[uint8]string
}

var errGammaRuntimeUnsupported = errors.New("the runtime's Gamma calls, events or storage are not as reviewed")

// newGammaRuntime decodes a runtime's metadata and checks its shapes. A
// shape mismatch is not an error here: the runtime is returned, unsupported,
// so a caller can say which runtime it is and why it pauses.
func newGammaRuntime(specVersion uint32, transactionVersion uint32, metadataHex string) (*gammaRuntime, error) {
	meta := &types.Metadata{}
	if err := codec.DecodeFromHex(metadataHex, meta); err != nil {
		return nil, fmt.Errorf("metadata of spec %d does not decode: %w", specVersion, err)
	}
	runtime := &gammaRuntime{
		SpecVersion:        specVersion,
		TransactionVersion: transactionVersion,
		Meta:               meta,
	}
	if meta.Version != 14 {
		runtime.shapeErr = fmt.Errorf("%w: metadata v%d, not v14", errGammaRuntimeUnsupported, meta.Version)
		return runtime, nil
	}
	events, err := registry.NewFactory().CreateEventRegistry(meta)
	if err != nil {
		runtime.shapeErr = fmt.Errorf("%w: no event decoders: %v", errGammaRuntimeUnsupported, err)
		return runtime, nil
	}
	runtime.events = events
	shape, err := gammaCheckShape(meta)
	if err != nil {
		runtime.shapeErr = fmt.Errorf("%w (spec %d): %v", errGammaRuntimeUnsupported, specVersion, err)
		return runtime, nil
	}
	runtime.shape = shape
	return runtime, nil
}

// The shapes every call, event and storage item the Gamma code uses must
// have, as gammaTypeShape writes them (newtypes such as NetUid and
// GammaBalance unwrapped). Reviewed against runtimes 488 and 490.
var gammaRequiredCalls = map[string][]string{
	"SubtensorModule.spend_gamma": {"netuid:u16", "spends:vec<([u8;32],u64,[u8;32])>"},
	"Proxy.proxy":                 {"real:multiaddress", "force_proxy_type:option<proxytype>", "call:runtimecall"},
}

var gammaRequiredEvents = map[string][]string{
	"SubtensorModule.GammaSpent":        {"netuid:u16", "coldkey:[u8;32]", "hotkey:[u8;32]", "spender:[u8;32]", "amount:u64", "reference:[u8;32]"},
	"SubtensorModule.GammaSpendSkipped": {"netuid:u16", "hotkey:[u8;32]", "spender:[u8;32]", "amount:u64", "reference:[u8;32]", "reason:dispatcherror"},
	"SubtensorModule.GammaMinted": {
		"netuid:u16", "coldkey:[u8;32]", "hotkey:[u8;32]", "spender:[u8;32]", "daily_spend_cap:option<u64>",
		"tao:u64", "alpha:u64", "value:u64", "gamma:u64",
	},
	"SubtensorModule.GammaMoved":            {"netuid:u16", "coldkey:[u8;32]", "origin_hotkey:[u8;32]", "destination_hotkey:[u8;32]", "amount:u64"},
	"Proxy.ProxyAdded":                      {"delegator:[u8;32]", "delegatee:[u8;32]", "proxy_type:proxytype", "delay:u32"},
	"Proxy.ProxyRemoved":                    {"delegator:[u8;32]", "delegatee:[u8;32]", "proxy_type:proxytype", "delay:u32"},
	"Balances.Transfer":                     {"from:[u8;32]", "to:[u8;32]", "amount:u64"},
	"TransactionPayment.TransactionFeePaid": {"who:[u8;32]", "actual_fee:u64", "tip:u64"},
}

var gammaRequiredStorage = map[string]string{
	"SubtensorModule.GammaDeposits":      "composite{coldkey:[u8;32],spender:[u8;32],balance:u64,daily_spend_cap:option<composite{cap:u64,bucket:(u64,u64)}>}",
	"SubtensorModule.Services":           "composite{url:vec<u8>,spender:[u8;32]}",
	"SubtensorModule.TaoUsdPrice":        "composite{accepted:u64,feed:u64,fed_at:u64}",
	"SubtensorModule.GammaEnabled":       "bool",
	"SubtensorModule.SubnetGammaEnabled": "bool",
	"SubtensorModule.SubnetOwnerHotkey":  "[u8;32]",
	"SubtensorModule.Owner":              "[u8;32]",
	"Proxy.Proxies":                      "(vec<composite{delegate:[u8;32],proxy_type:proxytype,delay:u32}>,u64)",
	"System.Account":                     "composite{nonce:u32,consumers:u32,providers:u32,sufficients:u32,data:composite{free:u64,reserved:u64,frozen:u64,flags:u128}}",
}

// The skip reasons spend_gamma_entry gives, by SubtensorModule error name.
var gammaSkipReasonErrors = []string{
	"GammaDepositDoesNotExist",
	"NotGammaDepositSpender",
	"AmountTooLow",
	"InsufficientGammaBalance",
	"GammaDepositDailySpendCapExceeded",
}

func gammaCheckShape(meta *types.Metadata) (*gammaShape, error) {
	m := meta.AsMetadataV14
	pallets := map[string]*types.PalletMetadataV14{}
	for i := range m.Pallets {
		pallets[string(m.Pallets[i].Name)] = &m.Pallets[i]
	}
	problems := []string{}
	splitName := func(name string) (string, string) {
		pallet, item, _ := strings.Cut(name, ".")
		return pallet, item
	}
	variants := func(typeId int64) []types.Si1Variant {
		ty, ok := m.EfficientLookup[typeId]
		if !ok || !ty.Def.IsVariant {
			return nil
		}
		return ty.Def.Variant.Variants
	}
	fieldShapes := func(fields []types.Si1Field) []string {
		out := []string{}
		for _, field := range fields {
			out = append(out, string(field.Name)+":"+gammaTypeShape(m, field.Type.Int64(), 0))
		}
		return out
	}

	for name, want := range gammaRequiredCalls {
		palletName, callName := splitName(name)
		pallet := pallets[palletName]
		if pallet == nil || !pallet.HasCalls {
			problems = append(problems, "no calls in "+palletName)
			continue
		}
		var found *types.Si1Variant
		for _, variant := range variants(pallet.Calls.Type.Int64()) {
			if string(variant.Name) == callName {
				found = &variant
				break
			}
		}
		if found == nil {
			problems = append(problems, "no call "+name)
			continue
		}
		if got := fieldShapes(found.Fields); !slices.Equal(got, want) {
			problems = append(problems, fmt.Sprintf("call %s is %v, reviewed %v", name, got, want))
		}
	}
	for name, want := range gammaRequiredEvents {
		palletName, eventName := splitName(name)
		pallet := pallets[palletName]
		if pallet == nil || !pallet.HasEvents {
			problems = append(problems, "no events in "+palletName)
			continue
		}
		var found *types.Si1Variant
		for _, variant := range variants(pallet.Events.Type.Int64()) {
			if string(variant.Name) == eventName {
				found = &variant
				break
			}
		}
		if found == nil {
			problems = append(problems, "no event "+name)
			continue
		}
		if got := fieldShapes(found.Fields); !slices.Equal(got, want) {
			problems = append(problems, fmt.Sprintf("event %s is %v, reviewed %v", name, got, want))
		}
	}
	for name, want := range gammaRequiredStorage {
		palletName, itemName := splitName(name)
		pallet := pallets[palletName]
		if pallet == nil || !pallet.HasStorage {
			problems = append(problems, "no storage in "+palletName)
			continue
		}
		var got string
		found := false
		for _, item := range pallet.Storage.Items {
			if string(item.Name) != itemName {
				continue
			}
			found = true
			if item.Type.IsPlainType {
				got = gammaTypeShape(m, item.Type.AsPlainType.Int64(), 0)
			} else {
				got = gammaTypeShape(m, item.Type.AsMap.Value.Int64(), 0)
			}
		}
		if !found {
			problems = append(problems, "no storage "+name)
		} else if got != want {
			problems = append(problems, fmt.Sprintf("storage %s is %s, reviewed %s", name, got, want))
		}
	}

	shape := &gammaShape{subtensorErrors: map[uint8]string{}}
	if pallet := pallets["SubtensorModule"]; pallet != nil {
		shape.subtensorPallet = uint8(pallet.Index)
		if pallet.HasErrors {
			for _, variant := range variants(pallet.Errors.Type.Int64()) {
				shape.subtensorErrors[uint8(variant.Index)] = string(variant.Name)
			}
		}
	}
	for _, name := range gammaSkipReasonErrors {
		found := false
		for _, errorName := range shape.subtensorErrors {
			if errorName == name {
				found = true
			}
		}
		if !found {
			problems = append(problems, "no SubtensorModule error "+name)
		}
	}

	// Proxy.ProxyType::GammaSpend, found through the proxy call's argument
	proxyTypeFound := false
	if pallet := pallets["Proxy"]; pallet != nil && pallet.HasCalls {
		for _, variant := range variants(pallet.Calls.Type.Int64()) {
			if string(variant.Name) != "proxy" || len(variant.Fields) != 3 {
				continue
			}
			option, ok := m.EfficientLookup[variant.Fields[1].Type.Int64()]
			if !ok || !option.Def.IsVariant || len(option.Def.Variant.Variants) != 2 || len(option.Def.Variant.Variants[1].Fields) != 1 {
				continue
			}
			for _, proxyType := range variants(option.Def.Variant.Variants[1].Fields[0].Type.Int64()) {
				if string(proxyType.Name) == gammaSpendProxyTypeName && len(proxyType.Fields) == 0 {
					shape.gammaSpendProxyType = uint8(proxyType.Index)
					proxyTypeFound = true
				}
			}
		}
	}
	if !proxyTypeFound {
		problems = append(problems, "no ProxyType::GammaSpend")
	}

	// every signed extension is one the signer encodes, or adds nothing
	for _, signedExtension := range m.Extrinsic.SignedExtensions {
		ty, ok := m.EfficientLookup[signedExtension.Type.Int64()]
		if !ok || len(ty.Path) == 0 {
			problems = append(problems, "signed extension "+string(signedExtension.Identifier)+" has no type")
			continue
		}
		typeName := string(ty.Path[len(ty.Path)-1])
		_, handled := extrinsic.PayloadMutatorFns[extensions.SignedExtensionName(typeName)]
		if !handled {
			problems = append(problems, fmt.Sprintf("signed extension %s (%s) is not handled", signedExtension.Identifier, typeName))
		}
	}

	if 0 < len(problems) {
		slices.Sort(problems)
		return nil, errors.New(strings.Join(problems, "; "))
	}
	return shape, nil
}

// gammaTypeShape is a type's structure in a short canonical form: newtypes
// (single-field composites) unwrapped, a named enum by its last path segment.
func gammaTypeShape(m types.MetadataV14, id int64, depth int) string {
	if 8 < depth {
		return "?deep"
	}
	ty, ok := m.EfficientLookup[id]
	if !ok {
		return "?unknown"
	}
	name := ""
	if 0 < len(ty.Path) {
		name = strings.ToLower(string(ty.Path[len(ty.Path)-1]))
	}
	def := ty.Def
	switch {
	case def.IsPrimitive:
		return gammaPrimitiveName(def.Primitive.Si0TypeDefPrimitive)
	case def.IsCompact:
		return "compact<" + gammaTypeShape(m, def.Compact.Type.Int64(), depth+1) + ">"
	case def.IsSequence:
		return "vec<" + gammaTypeShape(m, def.Sequence.Type.Int64(), depth+1) + ">"
	case def.IsArray:
		return fmt.Sprintf("[%s;%d]", gammaTypeShape(m, def.Array.Type.Int64(), depth+1), def.Array.Len)
	case def.IsTuple:
		parts := []string{}
		for _, t := range def.Tuple {
			parts = append(parts, gammaTypeShape(m, t.Int64(), depth+1))
		}
		return "(" + strings.Join(parts, ",") + ")"
	case def.IsComposite:
		if len(def.Composite.Fields) == 1 && def.Composite.Fields[0].Name == "" || len(def.Composite.Fields) == 1 && name == "boundedvec" {
			return gammaTypeShape(m, def.Composite.Fields[0].Type.Int64(), depth+1)
		}
		if name == "accountid32" || name == "h256" {
			return gammaTypeShape(m, def.Composite.Fields[0].Type.Int64(), depth+1)
		}
		parts := []string{}
		for _, field := range def.Composite.Fields {
			parts = append(parts, string(field.Name)+":"+gammaTypeShape(m, field.Type.Int64(), depth+1))
		}
		return "composite{" + strings.Join(parts, ",") + "}"
	case def.IsVariant:
		if name == "option" && len(def.Variant.Variants) == 2 && len(def.Variant.Variants[1].Fields) == 1 {
			return "option<" + gammaTypeShape(m, def.Variant.Variants[1].Fields[0].Type.Int64(), depth+1) + ">"
		}
		return name
	}
	return "?other"
}

func gammaPrimitiveName(p types.Si0TypeDefPrimitive) string {
	switch p {
	case types.IsBool:
		return "bool"
	case types.IsU8:
		return "u8"
	case types.IsU16:
		return "u16"
	case types.IsU32:
		return "u32"
	case types.IsU64:
		return "u64"
	case types.IsU128:
		return "u128"
	case types.IsStr:
		return "str"
	}
	return "?primitive"
}

// ----- the chain -----

// gammaBlock is a finalized block as the reader sees it.
type gammaBlock struct {
	Number     uint64
	Hash       types.Hash
	ParentHash types.Hash
	// the runtime that executed the block (the runtime of its parent state)
	Runtime *gammaRuntime
}

// gammaChain reads and writes the chain through a transport, with the
// runtimes it has seen cached by spec version.
type gammaChain struct {
	rpc gammaRpc

	stateLock sync.Mutex
	runtimes  map[uint32]*gammaRuntime
	genesis   *types.Hash
}

func newGammaChain(rpc gammaRpc) *gammaChain {
	return &gammaChain{
		rpc:      rpc,
		runtimes: map[uint32]*gammaRuntime{},
	}
}

func gammaParseHash(text string) (types.Hash, error) {
	b, err := codec.HexDecodeString(text)
	if err != nil || len(b) != 32 {
		return types.Hash{}, fmt.Errorf("not a block hash: %q", text)
	}
	return types.NewHash(b), nil
}

func gammaParseHexNumber(text string) (uint64, error) {
	return strconv.ParseUint(strings.TrimPrefix(text, "0x"), 16, 64)
}

// FinalizedHead is the last GRANDPA-finalized block.
func (self *gammaChain) FinalizedHead(ctx context.Context) (types.Hash, uint64, error) {
	var hashText string
	if err := self.rpc.Call(ctx, &hashText, "chain_getFinalizedHead"); err != nil {
		return types.Hash{}, 0, err
	}
	hash, err := gammaParseHash(hashText)
	if err != nil {
		return types.Hash{}, 0, err
	}
	number, _, err := self.Header(ctx, hash)
	return hash, number, err
}

// Header is a block's number and parent.
func (self *gammaChain) Header(ctx context.Context, hash types.Hash) (number uint64, parent types.Hash, returnErr error) {
	var header struct {
		Number     string `json:"number"`
		ParentHash string `json:"parentHash"`
	}
	if err := self.rpc.Call(ctx, &header, "chain_getHeader", hash.Hex()); err != nil {
		return 0, types.Hash{}, err
	}
	if header.Number == "" {
		return 0, types.Hash{}, fmt.Errorf("no header for %s", hash.Hex())
	}
	number, err := gammaParseHexNumber(header.Number)
	if err != nil {
		return 0, types.Hash{}, err
	}
	parent, err = gammaParseHash(header.ParentHash)
	return number, parent, err
}

// BlockHash is the canonical hash of a block number.
func (self *gammaChain) BlockHash(ctx context.Context, number uint64) (types.Hash, error) {
	var hashText *string
	if err := self.rpc.Call(ctx, &hashText, "chain_getBlockHash", number); err != nil {
		return types.Hash{}, err
	}
	if hashText == nil {
		return types.Hash{}, fmt.Errorf("no block %d", number)
	}
	return gammaParseHash(*hashText)
}

// GenesisHash is block 0's hash, for the signed payload.
func (self *gammaChain) GenesisHash(ctx context.Context) (types.Hash, error) {
	self.stateLock.Lock()
	genesis := self.genesis
	self.stateLock.Unlock()
	if genesis != nil {
		return *genesis, nil
	}
	hash, err := self.BlockHash(ctx, 0)
	if err != nil {
		return types.Hash{}, err
	}
	self.stateLock.Lock()
	self.genesis = &hash
	self.stateLock.Unlock()
	return hash, nil
}

// RuntimeAt is the runtime of the state at a block: the runtime the NEXT
// block executes with. A block's own events come from the runtime at its
// parent (BlockAt).
func (self *gammaChain) RuntimeAt(ctx context.Context, hash types.Hash) (*gammaRuntime, error) {
	var version types.RuntimeVersion
	if err := self.rpc.Call(ctx, &version, "state_getRuntimeVersion", hash.Hex()); err != nil {
		return nil, err
	}
	spec := uint32(version.SpecVersion)
	self.stateLock.Lock()
	runtime := self.runtimes[spec]
	self.stateLock.Unlock()
	if runtime != nil {
		if runtime.TransactionVersion != uint32(version.TransactionVersion) {
			return nil, fmt.Errorf("spec %d changed its transaction version (%d, now %d)", spec, runtime.TransactionVersion, version.TransactionVersion)
		}
		return runtime, nil
	}
	var metadataHex string
	if err := self.rpc.Call(ctx, &metadataHex, "state_getMetadata", hash.Hex()); err != nil {
		return nil, err
	}
	runtime, err := newGammaRuntime(spec, uint32(version.TransactionVersion), metadataHex)
	if err != nil {
		return nil, err
	}
	self.stateLock.Lock()
	self.runtimes[spec] = runtime
	self.stateLock.Unlock()
	return runtime, nil
}

// BlockAt is a canonical block with the runtime that executed it.
func (self *gammaChain) BlockAt(ctx context.Context, number uint64) (*gammaBlock, error) {
	hash, err := self.BlockHash(ctx, number)
	if err != nil {
		return nil, err
	}
	headerNumber, parent, err := self.Header(ctx, hash)
	if err != nil {
		return nil, err
	}
	if headerNumber != number {
		return nil, fmt.Errorf("block %d answered as %d", number, headerNumber)
	}
	runtime, err := self.RuntimeAt(ctx, parent)
	if err != nil {
		return nil, err
	}
	return &gammaBlock{Number: number, Hash: hash, ParentHash: parent, Runtime: runtime}, nil
}

// Storage is a storage value at a block, or nil when it is absent.
func (self *gammaChain) Storage(ctx context.Context, key types.StorageKey, at types.Hash) ([]byte, error) {
	var valueText *string
	if err := self.rpc.Call(ctx, &valueText, "state_getStorage", key.Hex(), at.Hex()); err != nil {
		return nil, err
	}
	if valueText == nil || *valueText == "" || *valueText == "0x" {
		return nil, nil
	}
	return codec.HexDecodeString(*valueText)
}

// Extrinsics are a block's encoded extrinsics, in order.
func (self *gammaChain) Extrinsics(ctx context.Context, hash types.Hash) ([][]byte, error) {
	var block struct {
		Block struct {
			Extrinsics []string `json:"extrinsics"`
		} `json:"block"`
	}
	if err := self.rpc.Call(ctx, &block, "chain_getBlock", hash.Hex()); err != nil {
		return nil, err
	}
	out := make([][]byte, 0, len(block.Block.Extrinsics))
	for _, text := range block.Block.Extrinsics {
		b, err := codec.HexDecodeString(text)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// StateCall runs a runtime API at a block.
func (self *gammaChain) StateCall(ctx context.Context, method string, args []byte, at types.Hash) ([]byte, error) {
	var resultText string
	if err := self.rpc.Call(ctx, &resultText, "state_call", method, codec.HexEncodeToString(args), at.Hex()); err != nil {
		return nil, err
	}
	return codec.HexDecodeString(resultText)
}

// Submit hands a signed extrinsic to the node's pool.
func (self *gammaChain) Submit(ctx context.Context, encoded []byte) error {
	var hashText string
	return self.rpc.Call(ctx, &hashText, "author_submitExtrinsic", codec.HexEncodeToString(encoded))
}

// QueryFee is the fee the chain would charge for a signed extrinsic, in rao.
func (self *gammaChain) QueryFee(ctx context.Context, encoded []byte, at types.Hash) (uint64, error) {
	var info struct {
		PartialFee json.RawMessage `json:"partialFee"`
	}
	if err := self.rpc.Call(ctx, &info, "payment_queryInfo", codec.HexEncodeToString(encoded), at.Hex()); err != nil {
		return 0, err
	}
	text := strings.Trim(string(info.PartialFee), `"`)
	fee, ok := new(big.Int).SetString(text, 0)
	if !ok || !fee.IsUint64() {
		return 0, fmt.Errorf("unreadable fee %q", text)
	}
	return fee.Uint64(), nil
}

// ----- events -----

// gammaEvent is one decoded event of a block.
type gammaEvent struct {
	// the position in the block's events
	Index int
	// the extrinsic that emitted it, or -1 outside any extrinsic
	ExtrinsicIndex int64
	// "Pallet.Event"
	Name   string
	Fields registry.DecodedFields
}

// Events are a block's events, decoded with the runtime that emitted them.
// An event the runtime's metadata cannot decode fails the whole block: an
// event stream has no lengths, so nothing after it could be trusted.
func (self *gammaChain) Events(ctx context.Context, block *gammaBlock) ([]*gammaEvent, error) {
	if err := block.Runtime.Supported(); err != nil {
		return nil, err
	}
	key, err := types.CreateStorageKey(block.Runtime.Meta, "System", "Events")
	if err != nil {
		return nil, err
	}
	raw, err := self.Storage(ctx, key, block.Hash)
	if err != nil {
		return nil, err
	}
	return gammaDecodeEvents(block.Runtime, raw)
}

func gammaDecodeEvents(runtime *gammaRuntime, raw []byte) ([]*gammaEvent, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	data := types.NewStorageDataRaw(raw)
	records, err := parser.NewEventParser().ParseEvents(runtime.events, &data)
	if err != nil {
		return nil, fmt.Errorf("the events do not decode with spec %d: %w", runtime.SpecVersion, err)
	}
	events := make([]*gammaEvent, 0, len(records))
	for i, record := range records {
		event := &gammaEvent{Index: i, ExtrinsicIndex: -1, Name: record.Name, Fields: record.Fields}
		if record.Phase != nil && record.Phase.IsApplyExtrinsic {
			event.ExtrinsicIndex = int64(record.Phase.AsApplyExtrinsic)
		}
		events = append(events, event)
	}
	return events, nil
}

// gammaEventField is a named field of an event. The registry names a field
// by its type path and its name ("sp_core.crypto.AccountId32.hotkey"); the
// name is the last segment.
func gammaEventField(fields registry.DecodedFields, name string) (any, bool) {
	for _, field := range fields {
		if field == nil {
			continue
		}
		if field.Name == name || strings.HasSuffix(field.Name, "."+name) {
			return field.Value, true
		}
	}
	return nil, false
}

// gammaLeaf unwraps newtype composites down to their one value.
func gammaLeaf(value any) any {
	for {
		fields, ok := value.(registry.DecodedFields)
		if !ok || len(fields) != 1 || fields[0] == nil {
			return value
		}
		value = fields[0].Value
	}
}

func gammaDecodedBytes32(value any) ([32]byte, error) {
	var out [32]byte
	items, ok := gammaLeaf(value).([]any)
	if !ok || len(items) != 32 {
		return out, fmt.Errorf("not 32 bytes: %T", value)
	}
	for i, item := range items {
		b, ok := item.(types.U8)
		if !ok {
			return out, fmt.Errorf("not a byte: %T", item)
		}
		out[i] = byte(b)
	}
	return out, nil
}

func gammaDecodedUint(value any) (uint64, error) {
	switch v := gammaLeaf(value).(type) {
	case types.U8:
		return uint64(v), nil
	case types.U16:
		return uint64(v), nil
	case types.U32:
		return uint64(v), nil
	case types.U64:
		return uint64(v), nil
	case types.U128:
		if v.Int == nil || !v.Int.IsUint64() {
			return 0, errors.New("u128 does not fit u64")
		}
		return v.Int.Uint64(), nil
	case types.UCompact:
		i := big.Int(v)
		if !i.IsUint64() {
			return 0, errors.New("compact does not fit u64")
		}
		return i.Uint64(), nil
	case byte:
		// a unit enum variant decodes to its index
		return uint64(v), nil
	}
	return 0, fmt.Errorf("not an integer: %T", value)
}

func gammaEventBytes32(event *gammaEvent, name string) ([32]byte, error) {
	value, ok := gammaEventField(event.Fields, name)
	if !ok {
		return [32]byte{}, fmt.Errorf("%s has no %s", event.Name, name)
	}
	return gammaDecodedBytes32(value)
}

func gammaEventUint(event *gammaEvent, name string) (uint64, error) {
	value, ok := gammaEventField(event.Fields, name)
	if !ok {
		return 0, fmt.Errorf("%s has no %s", event.Name, name)
	}
	return gammaDecodedUint(value)
}

// gammaSpendEvent is a GammaSpent or a GammaSpendSkipped.
type gammaSpendEvent struct {
	Event     *gammaEvent
	Skipped   bool
	Netuid    uint16
	Coldkey   [32]byte
	Hotkey    [32]byte
	Spender   [32]byte
	Amount    uint64
	Reference [32]byte
	// skipped: the SubtensorModule error name, or "other"
	Reason string
}

func gammaSpendEventFrom(runtime *gammaRuntime, event *gammaEvent) (*gammaSpendEvent, error) {
	spend := &gammaSpendEvent{Event: event}
	switch event.Name {
	case "SubtensorModule.GammaSpent":
	case "SubtensorModule.GammaSpendSkipped":
		spend.Skipped = true
	default:
		return nil, fmt.Errorf("%s is not a spend", event.Name)
	}
	netuid, err := gammaEventUint(event, "netuid")
	if err != nil {
		return nil, err
	}
	spend.Netuid = uint16(netuid)
	if !spend.Skipped {
		if spend.Coldkey, err = gammaEventBytes32(event, "coldkey"); err != nil {
			return nil, err
		}
	}
	if spend.Hotkey, err = gammaEventBytes32(event, "hotkey"); err != nil {
		return nil, err
	}
	if spend.Spender, err = gammaEventBytes32(event, "spender"); err != nil {
		return nil, err
	}
	if spend.Amount, err = gammaEventUint(event, "amount"); err != nil {
		return nil, err
	}
	if spend.Reference, err = gammaEventBytes32(event, "reference"); err != nil {
		return nil, err
	}
	if spend.Skipped {
		spend.Reason = "other"
		if value, ok := gammaEventField(event.Fields, "reason"); ok {
			if moduleIndex, errorIndex, ok := gammaModuleError(value); ok &&
				runtime.shape != nil && moduleIndex == runtime.shape.subtensorPallet {
				if name, ok := runtime.shape.subtensorErrors[errorIndex]; ok {
					spend.Reason = name
				}
			}
		}
	}
	return spend, nil
}

// gammaModuleError finds the pallet and error index of a decoded
// DispatchError::Module.
func gammaModuleError(value any) (moduleIndex uint8, errorIndex uint8, ok bool) {
	fields, isFields := value.(registry.DecodedFields)
	if !isFields {
		return 0, 0, false
	}
	var indexValue, errorValue any
	haveIndex, haveError := false, false
	for _, field := range fields {
		if field == nil {
			continue
		}
		switch {
		case field.Name == "index" || strings.HasSuffix(field.Name, ".index"):
			indexValue, haveIndex = field.Value, true
		case field.Name == "error" || strings.HasSuffix(field.Name, ".error"):
			errorValue, haveError = field.Value, true
		}
	}
	if haveIndex && haveError {
		index, err := gammaDecodedUint(indexValue)
		if err != nil {
			return 0, 0, false
		}
		items, isItems := gammaLeaf(errorValue).([]any)
		if !isItems || len(items) == 0 {
			return 0, 0, false
		}
		first, isByte := items[0].(types.U8)
		if !isByte {
			return 0, 0, false
		}
		return uint8(index), uint8(first), true
	}
	for _, field := range fields {
		if field == nil {
			continue
		}
		if moduleIndex, errorIndex, ok := gammaModuleError(field.Value); ok {
			return moduleIndex, errorIndex, true
		}
	}
	return 0, 0, false
}

// ----- storage reads -----

// gammaDeposit is a GammaDeposits entry.
type gammaDeposit struct {
	Coldkey [32]byte
	Spender [32]byte
	Balance uint64
	// nil: no daily spend cap
	DailySpendCap *uint64
	// the cap's bucket: what it allowed at BucketBlock
	BucketAvailable uint64
	BucketBlock     uint64
}

// gammaGammaRefillBlocks is how many blocks a daily cap takes to refill.
const gammaRefillBlocks = 7200

// Spendable is what a spend can take from the deposit at block now: the
// balance within what its daily cap allows (gamma.rs gamma_allowance).
func (self *gammaDeposit) Spendable(now uint64) uint64 {
	if self.DailySpendCap == nil {
		return self.Balance
	}
	limit := *self.DailySpendCap
	elapsed := uint64(0)
	if self.BucketBlock < now {
		elapsed = now - self.BucketBlock
	}
	refill := new(big.Int).Div(
		new(big.Int).Mul(new(big.Int).SetUint64(limit), new(big.Int).SetUint64(elapsed)),
		big.NewInt(gammaRefillBlocks),
	)
	allowance := new(big.Int).Add(new(big.Int).SetUint64(self.BucketAvailable), refill)
	if allowance.Cmp(new(big.Int).SetUint64(limit)) > 0 {
		allowance.SetUint64(limit)
	}
	return min(self.Balance, allowance.Uint64())
}

func gammaDecodeDeposit(raw []byte) (*gammaDeposit, error) {
	decoder := scale.NewDecoder(bytes.NewReader(raw))
	deposit := &gammaDeposit{}
	if err := decoder.Read(deposit.Coldkey[:]); err != nil {
		return nil, err
	}
	if err := decoder.Read(deposit.Spender[:]); err != nil {
		return nil, err
	}
	var balance types.U64
	if err := decoder.Decode(&balance); err != nil {
		return nil, err
	}
	deposit.Balance = uint64(balance)
	hasCap, err := decoder.ReadOneByte()
	if err != nil {
		return nil, err
	}
	switch hasCap {
	case 0:
	case 1:
		var cap, available, block types.U64
		for _, value := range []*types.U64{&cap, &available, &block} {
			if err := decoder.Decode(value); err != nil {
				return nil, err
			}
		}
		limit := uint64(cap)
		deposit.DailySpendCap = &limit
		deposit.BucketAvailable = uint64(available)
		deposit.BucketBlock = uint64(block)
	default:
		return nil, fmt.Errorf("bad option tag %d", hasCap)
	}
	return deposit, nil
}

func gammaNetuidArg(netuid uint16) []byte {
	return []byte{byte(netuid), byte(netuid >> 8)}
}

// Deposit is the deposit at a hotkey, or nil when there is none.
func (self *gammaChain) Deposit(ctx context.Context, runtime *gammaRuntime, at types.Hash, netuid uint16, hotkey [32]byte) (*gammaDeposit, error) {
	key, err := types.CreateStorageKey(runtime.Meta, "SubtensorModule", "GammaDeposits", gammaNetuidArg(netuid), hotkey[:])
	if err != nil {
		return nil, err
	}
	raw, err := self.Storage(ctx, key, at)
	if err != nil || raw == nil {
		return nil, err
	}
	return gammaDecodeDeposit(raw)
}

// gammaService is a Services entry.
type gammaService struct {
	Url     string
	Spender [32]byte
}

func gammaDecodeService(raw []byte) (*gammaService, error) {
	decoder := scale.NewDecoder(bytes.NewReader(raw))
	var url []byte
	if err := decoder.Decode(&url); err != nil {
		return nil, err
	}
	service := &gammaService{Url: string(url)}
	if err := decoder.Read(service.Spender[:]); err != nil {
		return nil, err
	}
	return service, nil
}

// Service is the service published under a hotkey, or nil.
func (self *gammaChain) Service(ctx context.Context, runtime *gammaRuntime, at types.Hash, netuid uint16, hotkey [32]byte) (*gammaService, error) {
	key, err := types.CreateStorageKey(runtime.Meta, "SubtensorModule", "Services", gammaNetuidArg(netuid), hotkey[:])
	if err != nil {
		return nil, err
	}
	raw, err := self.Storage(ctx, key, at)
	if err != nil || raw == nil {
		return nil, err
	}
	return gammaDecodeService(raw)
}

// gammaTaoUsdPrice is the oracle's TaoUsdPrice: gamma units per TAO.
type gammaTaoUsdPrice struct {
	Accepted uint64
	Feed     uint64
	FedAt    uint64
}

func gammaDecodeTaoUsdPrice(raw []byte) (*gammaTaoUsdPrice, error) {
	if len(raw) != 24 {
		return nil, fmt.Errorf("TaoUsdPrice is %d bytes, not 24", len(raw))
	}
	return &gammaTaoUsdPrice{
		Accepted: binary.LittleEndian.Uint64(raw[0:8]),
		Feed:     binary.LittleEndian.Uint64(raw[8:16]),
		FedAt:    binary.LittleEndian.Uint64(raw[16:24]),
	}, nil
}

// TaoUsdPrice is the oracle's price, or nil when no oracle has fed.
func (self *gammaChain) TaoUsdPrice(ctx context.Context, runtime *gammaRuntime, at types.Hash) (*gammaTaoUsdPrice, error) {
	key, err := types.CreateStorageKey(runtime.Meta, "SubtensorModule", "TaoUsdPrice")
	if err != nil {
		return nil, err
	}
	raw, err := self.Storage(ctx, key, at)
	if err != nil || raw == nil {
		return nil, err
	}
	return gammaDecodeTaoUsdPrice(raw)
}

// Bool is a bool storage value (default false), as GammaEnabled and
// SubnetGammaEnabled are.
func (self *gammaChain) Bool(ctx context.Context, runtime *gammaRuntime, at types.Hash, item string, args ...[]byte) (bool, error) {
	key, err := types.CreateStorageKey(runtime.Meta, "SubtensorModule", item, args...)
	if err != nil {
		return false, err
	}
	raw, err := self.Storage(ctx, key, at)
	if err != nil || raw == nil {
		return false, err
	}
	if len(raw) != 1 || 1 < raw[0] {
		return false, fmt.Errorf("%s is not a bool", item)
	}
	return raw[0] == 1, nil
}

// AccountId is a 32-byte account storage value (SubnetOwnerHotkey, Owner),
// or nil when absent.
func (self *gammaChain) AccountId(ctx context.Context, runtime *gammaRuntime, at types.Hash, item string, args ...[]byte) (*[32]byte, error) {
	key, err := types.CreateStorageKey(runtime.Meta, "SubtensorModule", item, args...)
	if err != nil {
		return nil, err
	}
	raw, err := self.Storage(ctx, key, at)
	if err != nil || raw == nil {
		return nil, err
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("%s is not an account", item)
	}
	var out [32]byte
	copy(out[:], raw)
	return &out, nil
}

// gammaProxyDefinition is one of an account's proxies.
type gammaProxyDefinition struct {
	Delegate  [32]byte
	ProxyType uint8
	Delay     uint32
}

func gammaDecodeProxies(raw []byte) ([]gammaProxyDefinition, error) {
	decoder := scale.NewDecoder(bytes.NewReader(raw))
	count, err := decoder.DecodeUintCompact()
	if err != nil {
		return nil, err
	}
	if 32 < count.Uint64() {
		return nil, fmt.Errorf("%d proxies", count.Uint64())
	}
	out := []gammaProxyDefinition{}
	for i := uint64(0); i < count.Uint64(); i++ {
		var definition gammaProxyDefinition
		if err := decoder.Read(definition.Delegate[:]); err != nil {
			return nil, err
		}
		proxyType, err := decoder.ReadOneByte()
		if err != nil {
			return nil, err
		}
		definition.ProxyType = proxyType
		var delay types.U32
		if err := decoder.Decode(&delay); err != nil {
			return nil, err
		}
		definition.Delay = uint32(delay)
		out = append(out, definition)
	}
	return out, nil
}

// Proxies are an account's proxies.
func (self *gammaChain) Proxies(ctx context.Context, runtime *gammaRuntime, at types.Hash, account [32]byte) ([]gammaProxyDefinition, error) {
	key, err := types.CreateStorageKey(runtime.Meta, "Proxy", "Proxies", account[:])
	if err != nil {
		return nil, err
	}
	raw, err := self.Storage(ctx, key, at)
	if err != nil || raw == nil {
		return nil, err
	}
	return gammaDecodeProxies(raw)
}

// gammaAccount is the part of System.Account the biller reads.
type gammaAccount struct {
	Nonce  uint32
	Free   uint64
	Frozen uint64
}

// Usable is what the account can pay fees from.
func (self *gammaAccount) Usable() uint64 {
	if self.Free < self.Frozen {
		return 0
	}
	return self.Free - self.Frozen
}

func gammaDecodeAccount(raw []byte) (*gammaAccount, error) {
	// nonce, consumers, providers, sufficients: u32; then free, reserved,
	// frozen: u64 (TaoBalance); then flags: u128
	if len(raw) != 16+24+16 {
		return nil, fmt.Errorf("System.Account is %d bytes", len(raw))
	}
	return &gammaAccount{
		Nonce:  binary.LittleEndian.Uint32(raw[0:4]),
		Free:   binary.LittleEndian.Uint64(raw[16:24]),
		Frozen: binary.LittleEndian.Uint64(raw[32:40]),
	}, nil
}

// Account is an account at a block; an absent account is nonce 0 and
// nothing to spend.
func (self *gammaChain) Account(ctx context.Context, runtime *gammaRuntime, at types.Hash, account [32]byte) (*gammaAccount, error) {
	key, err := types.CreateStorageKey(runtime.Meta, "System", "Account", account[:])
	if err != nil {
		return nil, err
	}
	raw, err := self.Storage(ctx, key, at)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return &gammaAccount{}, nil
	}
	return gammaDecodeAccount(raw)
}

// gammaSimMint is GammaRuntimeApi_sim_mint_gamma: what minting with TAO
// would give now, or the module error that stops it.
type gammaSimMint struct {
	Alpha uint64
	Value uint64
	Gamma uint64
	// set when the mint would fail: the SubtensorModule error name, or "other"
	Error string
}

// SimMintGamma runs the chain's own simulation of a mint.
func (self *gammaChain) SimMintGamma(ctx context.Context, runtime *gammaRuntime, at types.Hash, netuid uint16, amountRao uint64) (*gammaSimMint, error) {
	args := make([]byte, 10)
	binary.LittleEndian.PutUint16(args[0:2], netuid)
	binary.LittleEndian.PutUint64(args[2:10], amountRao)
	raw, err := self.StateCall(ctx, "GammaRuntimeApi_sim_mint_gamma", args, at)
	if err != nil {
		return nil, err
	}
	return gammaDecodeSimMint(runtime, raw)
}

func gammaDecodeSimMint(runtime *gammaRuntime, raw []byte) (*gammaSimMint, error) {
	if len(raw) == 0 {
		return nil, errors.New("empty sim_mint_gamma answer")
	}
	switch raw[0] {
	case 0:
		if len(raw) != 1+24 {
			return nil, fmt.Errorf("sim_mint_gamma Ok is %d bytes", len(raw))
		}
		return &gammaSimMint{
			Alpha: binary.LittleEndian.Uint64(raw[1:9]),
			Value: binary.LittleEndian.Uint64(raw[9:17]),
			Gamma: binary.LittleEndian.Uint64(raw[17:25]),
		}, nil
	case 1:
		sim := &gammaSimMint{Error: "other"}
		// DispatchError::Module(ModuleError { index: u8, error: [u8; 4] })
		if 1+1+1+4 <= len(raw) && raw[1] == 3 && runtime.shape != nil && raw[2] == runtime.shape.subtensorPallet {
			if name, ok := runtime.shape.subtensorErrors[raw[3]]; ok {
				sim.Error = name
			}
		}
		return sim, nil
	}
	return nil, fmt.Errorf("sim_mint_gamma answered tag %d", raw[0])
}

// ----- the spend -----

// gammaSpendEntry is one spend_gamma entry.
type gammaSpendEntry struct {
	Hotkey    [32]byte
	Amount    uint64
	Reference [32]byte
}

// gammaSpendEntryScale is how an entry encodes: (AccountId, GammaBalance,
// [u8; 32]).
type gammaSpendEntryScale struct {
	Hotkey    [32]byte
	Amount    types.U64
	Reference [32]byte
}

// gammaSpendCall is Proxy.proxy(spender, Some(GammaSpend),
// SubtensorModule.spend_gamma(netuid, entries)): the billing key spends as
// the spender's GammaSpend proxy.
func gammaSpendCall(runtime *gammaRuntime, netuid uint16, spender [32]byte, entries []gammaSpendEntry) (types.Call, error) {
	if err := runtime.Supported(); err != nil {
		return types.Call{}, err
	}
	if len(entries) == 0 || gammaMaxSpendsPerCall < len(entries) {
		return types.Call{}, fmt.Errorf("a spend takes 1 to %d entries, not %d", gammaMaxSpendsPerCall, len(entries))
	}
	inner, err := gammaInnerSpendCall(runtime, netuid, entries)
	if err != nil {
		return types.Call{}, err
	}
	real, err := types.NewMultiAddressFromAccountID(spender[:])
	if err != nil {
		return types.Call{}, err
	}
	return types.NewCall(
		runtime.Meta,
		"Proxy.proxy",
		real,
		types.NewOption[types.U8](types.U8(runtime.shape.gammaSpendProxyType)),
		inner,
	)
}

// gammaInnerSpendCall is SubtensorModule.spend_gamma(netuid, entries).
func gammaInnerSpendCall(runtime *gammaRuntime, netuid uint16, entries []gammaSpendEntry) (types.Call, error) {
	spends := make([]gammaSpendEntryScale, 0, len(entries))
	for _, entry := range entries {
		spends = append(spends, gammaSpendEntryScale{Hotkey: entry.Hotkey, Amount: types.U64(entry.Amount), Reference: entry.Reference})
	}
	return types.NewCall(runtime.Meta, "SubtensorModule.spend_gamma", types.U16(netuid), spends)
}

// gammaMortalEra is a mortal era of period blocks born at block birth: the
// extrinsic is valid in blocks birth through birth+period-1. period is a
// power of two from 4 to 65536.
func gammaMortalEra(birth uint64, period uint64) (types.ExtrinsicEra, error) {
	if period < 4 || 1<<16 < period || period&(period-1) != 0 {
		return types.ExtrinsicEra{}, fmt.Errorf("era period %d is not a power of two in [4, 65536]", period)
	}
	quantize := max(period>>12, 1)
	phase := birth % period / quantize * quantize
	trailingZeros := uint64(0)
	for p := period; p&1 == 0; p >>= 1 {
		trailingZeros++
	}
	encoded := min(max(trailingZeros-1, 1), 15) | (phase/quantize)<<4
	return types.ExtrinsicEra{
		IsMortalEra: true,
		AsMortalEra: types.MortalEra{First: byte(encoded), Second: byte(encoded >> 8)},
	}, nil
}

// gammaSigned is a signed spend batch, ready to submit and to persist.
type gammaSigned struct {
	Encoded []byte
	Hash    types.Hash
}

// gammaSignSpend signs a call with the billing key: a mortal era born at a
// finalized block (birthNumber, birthHash), the given nonce, no tip and no
// metadata hash.
func gammaSignSpend(
	runtime *gammaRuntime,
	genesis types.Hash,
	birthNumber uint64,
	birthHash types.Hash,
	period uint64,
	nonce uint32,
	call types.Call,
	signer signature.KeyringPair,
) (*gammaSigned, error) {
	era, err := gammaMortalEra(birthNumber, period)
	if err != nil {
		return nil, err
	}
	ext := extrinsic.NewExtrinsic(call)
	err = ext.Sign(signer, runtime.Meta,
		extrinsic.WithEra(era, birthHash),
		extrinsic.WithNonce(types.NewUCompactFromUInt(uint64(nonce))),
		extrinsic.WithTip(types.NewUCompactFromUInt(0)),
		extrinsic.WithSpecVersion(types.U32(runtime.SpecVersion)),
		extrinsic.WithTransactionVersion(types.U32(runtime.TransactionVersion)),
		extrinsic.WithGenesisHash(genesis),
		extrinsic.WithMetadataMode(extensions.CheckMetadataModeDisabled, extensions.CheckMetadataHash{Hash: types.NewEmptyOption[types.H256]()}),
	)
	if err != nil {
		return nil, fmt.Errorf("sign the spend: %w", err)
	}
	encoded, err := codec.Encode(ext)
	if err != nil {
		return nil, err
	}
	return &gammaSigned{Encoded: encoded, Hash: gammaExtrinsicHash(encoded)}, nil
}

// gammaExtrinsicHash is the hash a block's extrinsic is known by.
func gammaExtrinsicHash(encoded []byte) types.Hash {
	return types.NewHash(blake2bSum256(encoded))
}

func blake2bSum256(b []byte) []byte {
	sum := blake2b.Sum256(b)
	return sum[:]
}

// gammaHex is a 0x-prefixed lowercase hex string.
func gammaHex(b []byte) string {
	return "0x" + hex.EncodeToString(b)
}
