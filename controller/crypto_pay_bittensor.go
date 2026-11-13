package controller

// TAO on the crypto payment rail (mmm/BTCPAY.md §17.3, §17.7): each invoice
// has a Coinbase deposit address of its own on Bittensor (Finney), and
// payments are read from snow's Finney node alone (§14.3), in finalized
// blocks, and credited at finality. Our credit comes from the chain, so a
// delay in Coinbase's own TAO deposits (trackers listed seven send and
// receive delays in 2026, one of 4.5 days on Prime) never holds a customer.
//
// Every finalized block is read, and every Balances.Transfer in it is
// matched against every address a TAO invoice was ever given (open or closed,
// credited, voided or past the retention unlink), in one indexed lookup a
// scan: a payment to an old or expired invoice is recorded too, and judged by
// the late-payment rules (cryptoEvaluateIntent): credited where they credit,
// else held for an operator. An invoice made after its block was mined is
// not matched in it.
//
// snow's node is shared with the SN25 validator, so the reader is careful
// with it (xops snow/subtensor/README.md: a runtime call outside the node's
// cache of compiled runtimes stalls every client; on 2026-10-14 a reader that
// spanned runtimes stalled the validator). Its bounds are
// CryptoTaoReaderSettings:
//
//   - it reads recent finalized blocks only: at most BlocksPerScan a scan,
//     never further back than LookbackBlocks behind the finalized head. Older
//     blocks are passed over, counted in
//     urnetwork_crypto_pay_scan_blocks_skipped_total and logged;
//   - its requests to the node are paced (one per RequestSpacing at most) and
//     budgeted (RequestsPerScan a scan, the heads included: a scan stops
//     reading blocks before it would pass it, and the next scan goes on), and
//     counted by method in urnetwork_crypto_pay_tao_node_requests_total. In
//     the steady state a scan reads the few blocks finalized since the last
//     one: four storage and header reads a block (no runtime call), plus the
//     heads and the network check, about 13 requests a 20 s scan at Finney's
//     12 s blocks (0.65 a second; a scan that reads no block makes 6);
//   - it reads no block at all until a TAO invoice address has been issued,
//     so a coin that is configured but not yet sold costs only the heads;
//   - it never replays history across runtime upgrades: each block's runtime
//     is read from storage (System.LastRuntimeUpgrade, not a runtime call),
//     and its events and extrinsics are decoded with that runtime's own
//     metadata, never a pinned allowlist of runtimes. Metadata is fetched
//     (state_getMetadata, the one runtime call) only for the finalized head's
//     runtime and one older runtime per scan, within the lookback, and cached
//     per spec version. A block of any other runtime, or metadata or events
//     that do not decode, pauses the reader: the scan fails (the coin goes
//     unavailable), urnetwork_crypto_pay_tao_reader_paused is 1 until a scan
//     reads again, and nothing is credited meanwhile. It never panics:
//     decoding is recovered, so the scanner never crash-loops;
//   - the scanner's first run must pass the validator's deploy gate (memory
//     note validator-deploy-gate: roll only while the validator is idle,
//     finalized and applied, at least 15 minutes to the epoch boundary).
//
// What is credited (mmm/BTCPAY.md §17.3 and the owner's research of
// 2026-11-10): a Balances.Transfer event to an invoice address whose
// extrinsic's top-level call is one of cryptoTaoCreditedCalls, signed by the
// transfer's sender. Coinbase publishes nothing on which TAO transfer kinds it
// credits, so every other shape that moves TAO to an invoice address (a
// utility batch, a proxy or multisig call, an EVM precompile send through
// Ethereum.transact, a stake transfer, a hook, an extrinsic that does not
// decode) is still recorded, held for review (transfer_shape): an operator
// checks the Coinbase account before crediting it by hand. More shapes join
// the table after a small live test of each. The reader is wallet-agnostic:
// any wallet's plain transfer of an allowed shape is credited, whatever
// signs it (a quantum-resistant signer included, when its runtime's metadata
// describes it).
//
// An extrinsic the reader cannot decode (a signer of a kind its metadata does
// not describe, a format it does not read) is skipped and counted
// (urnetwork_crypto_pay_tao_extrinsics_undecodable_total), never fatal: a
// transfer to an invoice address in it is held for review.

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	gsrpcclient "github.com/centrifuge/go-substrate-rpc-client/v4/client"
	"github.com/centrifuge/go-substrate-rpc-client/v4/registry"
	"github.com/centrifuge/go-substrate-rpc-client/v4/registry/parser"
	"github.com/centrifuge/go-substrate-rpc-client/v4/scale"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urfoundation/sn/crv4"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server/model"
)

// The TAO reader's tunables: the bounds of its load on snow's node, which the
// SN25 validator shares.
type CryptoTaoReaderSettings struct {
	// The most finalized blocks one scan reads. Non-positive reads none.
	BlocksPerScan int
	// The furthest behind the finalized head a block is read: about an hour
	// of 12 s blocks, all of the node's recent runtime. A scanner down longer
	// passes the older blocks over and counts them.
	LookbackBlocks uint64
	// The most node requests one scan makes reading blocks, the heads and the
	// network check included: a full scan of BlocksPerScan blocks is four
	// requests a block, and a block that pays an invoice two more. A scan
	// stops reading blocks before it would pass it. Non-positive reads none.
	RequestsPerScan int
	// The least time between two requests to the node, so a scan that catches
	// up after an outage spreads its reads instead of bursting them.
	// Non-positive does not pace.
	RequestSpacing time.Duration
	// How far after its block's time an invoice may have been made and still
	// be matched in the block: the skew between our clock and the chain's.
	ClockTolerance time.Duration
}

// A new instance on every call.
func DefaultCryptoTaoReaderSettings() *CryptoTaoReaderSettings {
	return &CryptoTaoReaderSettings{
		BlocksPerScan:  100,
		LookbackBlocks: 300,
		// a full scan's 400 block reads, its heads, and the calls and times of
		// the few blocks that pay an invoice
		RequestsPerScan: 500,
		// at most 20 requests a second: a catch-up of a full scan takes 20 s
		RequestSpacing: 50 * time.Millisecond,
		ClockTolerance: 5 * time.Minute,
	}
}

// Requests the reader makes to the node per block it reads: its hash, its
// runtime, its header and its events.
const cryptoTaoBlockRequests = 4

// The most more requests one read block makes: its body and its time, when a
// transfer in it pays an invoice.
const cryptoTaoBlockPaymentRequests = 2

// A finalized head older than this is a node, or a finality, that stalled:
// the network's height is unknown and TAO is not sold.
const cryptoTaoStaleFinality = 5 * time.Minute

// One node request is abandoned after this long.
const cryptoTaoRpcTimeout = 30 * time.Second

// The runtimes' metadata kept per process.
const cryptoTaoMaxRuntimes = 3

// The decoded blocks kept per process.
const cryptoTaoBlockCacheSize = 1024

// Finney's genesis: a node of another chain is refused.
const cryptoTaoFinneyGenesis = "0x2f0555cc76fc2840a25a6ea3b9637146806f1f44b090c175ffde2a7e5ab36c03"

// The storage keys the reader reads (twox128 of the pallet and of the item):
// System.Events, System.LastRuntimeUpgrade and Timestamp.Now. Fixed by their
// names, whatever the runtime.
const (
	cryptoTaoKeySystemEvents       = "0x26aa394eea5630e07c48ae0c9558cef780d41e5e16056765bc8461851072c9d7"
	cryptoTaoKeyLastRuntimeUpgrade = "0x26aa394eea5630e07c48ae0c9558cef7f9cce9c888469bb1a0dceaa129672ef8"
	cryptoTaoKeyTimestampNow       = "0xf0c365c3cf59d671eb72da0e7a4113c49f1f0515f462cdcf84e0f1d6045dfcbb"
)

// The shapes the rail credits by itself: the top-level calls whose transfer to
// an invoice address is credited, signed by the transfer's sender. Every other
// shape is held for review (transfer_shape). A shape joins this table only
// after a small live payment of it was seen credited in our Coinbase account.
//
//	call                                        credited  why
//	Balances.transfer_keep_alive                yes       a plain transfer: what wallets and exchanges send
//	Balances.transfer_allow_death               yes       a plain transfer that may reap the sender
//	Balances.transfer_all                       yes       a plain transfer of the sender's whole balance
//	Utility.batch, batch_all, force_batch       no        a nested call
//	Proxy.*, Multisig.*                         no        a nested call; the signer is not the sender
//	Ethereum.transact (the EVM precompile)      no        sent from an EVM account's mirror
//	SubtensorModule.transfer_stake, staking     no        alpha or stake, never TAO in Coinbase
//	MevShield.submit_encrypted                  no        an encrypted call
//	anything else, a block hook, an extrinsic that does not decode   no
var cryptoTaoCreditedCalls = []string{
	"Balances.transfer_keep_alive",
	"Balances.transfer_allow_death",
	"Balances.transfer_all",
}

// The reader paused on a runtime it cannot read: 1 until a scan reads again.
var cryptoPayTaoReaderPaused = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "crypto_pay",
	Name:      "tao_reader_paused",
	Help:      "1 while the TAO reader is paused on a runtime it cannot read (no TAO payment is credited meanwhile), else 0",
})

// Extrinsics the reader could not decode, which it skipped.
var cryptoPayTaoExtrinsicsUndecodable = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "crypto_pay",
	Name:      "tao_extrinsics_undecodable_total",
	Help:      "TAO extrinsics the reader could not decode and skipped (a transfer to an invoice address in one is held for review)",
})

// The reader's requests to snow's node, by method: the load it adds to the
// node the SN25 validator shares.
var cryptoPayTaoNodeRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "crypto_pay",
	Name:      "tao_node_requests_total",
	Help:      "Requests the TAO reader made to snow's Finney node, which the SN25 validator shares, by method",
}, []string{"method"})

func init() {
	prometheus.MustRegister(cryptoPayTaoReaderPaused, cryptoPayTaoExtrinsicsUndecodable, cryptoPayTaoNodeRequests)
}

// The clock and the wait of the reader's pacing. Replaceable only by tests.
var (
	cryptoTaoPaceNow  = time.Now
	cryptoTaoPaceWait = func(ctx context.Context, wait time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
			return nil
		}
	}
)

// Whether a TAO invoice address was ever issued, and the issued addresses
// among the given ones. Replaceable only by tests.
var (
	cryptoTaoAddressesIssued = func(ctx context.Context) bool {
		return model.CryptoPaymentAddressesIssuedInConn(nil, ctx, model.CryptoAssetTao)
	}
	cryptoTaoIssuedAddresses = func(ctx context.Context, addresses []string) map[string]*model.CryptoPaymentIssuedAddress {
		return model.GetCryptoPaymentIssuedAddressesInConn(nil, ctx, model.CryptoAssetTao, addresses)
	}
)

// A runtime the reader will not read: it pauses.
var errCryptoTaoRuntimeUnreadable = errors.New("the TAO reader cannot read this runtime")

// ----- the node -----

// cryptoTaoRpc is the Finney node the reader reads.
type cryptoTaoRpc interface {
	call(ctx context.Context, result any, method string, params ...any) error
}

// The node of a url. Replaceable only by tests, which answer from recorded
// fixtures.
var cryptoTaoRpcFor = func(url string) cryptoTaoRpc {
	return &cryptoTaoNode{url: url}
}

// cryptoTaoNode calls snow's node over one WebSocket per process, dialed
// again after any failed call.
type cryptoTaoNode struct {
	url string
}

var cryptoTaoClientsLock sync.Mutex
var cryptoTaoClients = map[string]gsrpcclient.Client{}

func cryptoTaoClient(url string) (gsrpcclient.Client, error) {
	cryptoTaoClientsLock.Lock()
	defer cryptoTaoClientsLock.Unlock()
	if client, ok := cryptoTaoClients[url]; ok {
		return client, nil
	}
	client, err := gsrpcclient.Connect(url)
	if err != nil {
		return nil, fmt.Errorf("the Finney node could not be reached: %w", err)
	}
	cryptoTaoClients[url] = client
	return client, nil
}

func cryptoTaoDropClient(url string, client gsrpcclient.Client) {
	cryptoTaoClientsLock.Lock()
	defer cryptoTaoClientsLock.Unlock()
	if cryptoTaoClients[url] == client {
		delete(cryptoTaoClients, url)
		client.Close()
	}
}

func (self *cryptoTaoNode) call(ctx context.Context, result any, method string, params ...any) error {
	client, err := cryptoTaoClient(self.url)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, cryptoTaoRpcTimeout)
	defer cancel()
	if err := client.CallContext(callCtx, result, method, params...); err != nil {
		cryptoTaoDropClient(self.url, client)
		return fmt.Errorf("finney %s: %w", method, err)
	}
	return nil
}

// ----- runtimes -----

// cryptoTaoRuntime is what reading a runtime's blocks needs: its event and
// extrinsic decoders, and its call names by (pallet index, call index).
type cryptoTaoRuntime struct {
	spec       uint32
	events     registry.EventRegistry
	address    *registry.Field
	signature  *registry.Field
	extra      *registry.Field
	callNames  map[[2]byte]string
	lastUsedAt time.Time
}

var cryptoTaoRuntimesLock sync.Mutex
var cryptoTaoRuntimes = map[uint32]*cryptoTaoRuntime{}

// resetCryptoTaoCaches forgets the cached runtimes and blocks (tests).
func resetCryptoTaoCaches() {
	cryptoTaoRuntimesLock.Lock()
	cryptoTaoRuntimes = map[uint32]*cryptoTaoRuntime{}
	cryptoTaoRuntimesLock.Unlock()
	cryptoTaoBlocksLock.Lock()
	cryptoTaoBlocks = map[string]*cryptoTaoBlock{}
	cryptoTaoBlockOrder = []string{}
	cryptoTaoBlocksLock.Unlock()
}

func cryptoTaoCachedRuntime(spec uint32) *cryptoTaoRuntime {
	cryptoTaoRuntimesLock.Lock()
	defer cryptoTaoRuntimesLock.Unlock()
	runtime, ok := cryptoTaoRuntimes[spec]
	if ok {
		runtime.lastUsedAt = time.Now()
	}
	return runtime
}

func cryptoTaoCacheRuntime(runtime *cryptoTaoRuntime) {
	cryptoTaoRuntimesLock.Lock()
	defer cryptoTaoRuntimesLock.Unlock()
	runtime.lastUsedAt = time.Now()
	cryptoTaoRuntimes[runtime.spec] = runtime
	for len(cryptoTaoRuntimes) > cryptoTaoMaxRuntimes {
		oldest := runtime.spec
		for spec, cached := range cryptoTaoRuntimes {
			if cached.lastUsedAt.Before(cryptoTaoRuntimes[oldest].lastUsedAt) {
				oldest = spec
			}
		}
		delete(cryptoTaoRuntimes, oldest)
	}
}

// cryptoTaoNewRuntime builds the decoders of a runtime's metadata. A metadata
// the decoders cannot be built from is an error, never a panic.
func cryptoTaoNewRuntime(meta *types.Metadata, spec uint32) (runtime *cryptoTaoRuntime, returnErr error) {
	defer func() {
		if r := recover(); r != nil {
			runtime = nil
			returnErr = fmt.Errorf("%w: the metadata of spec %d could not be read: %v", errCryptoTaoRuntimeUnreadable, spec, r)
		}
	}()
	if meta == nil || meta.Version != 14 {
		return nil, fmt.Errorf("%w: spec %d has no v14 metadata", errCryptoTaoRuntimeUnreadable, spec)
	}
	events, err := registry.NewFactory().CreateEventRegistry(meta)
	if err != nil {
		return nil, fmt.Errorf("%w: the events of spec %d: %s", errCryptoTaoRuntimeUnreadable, spec, err)
	}
	extrinsics, err := registry.NewFactory().CreateExtrinsicDecoder(meta)
	if err != nil {
		return nil, fmt.Errorf("%w: the extrinsics of spec %d: %s", errCryptoTaoRuntimeUnreadable, spec, err)
	}
	runtime = &cryptoTaoRuntime{spec: spec, events: events, callNames: map[[2]byte]string{}}
	for _, field := range extrinsics.Fields {
		switch field.Name {
		case registry.ExtrinsicAddressName:
			runtime.address = field
		case registry.ExtrinsicSignatureName:
			runtime.signature = field
		case registry.ExtrinsicExtraName:
			runtime.extra = field
		}
	}
	if runtime.address == nil || runtime.signature == nil || runtime.extra == nil {
		return nil, fmt.Errorf("%w: spec %d does not describe its signed extrinsics", errCryptoTaoRuntimeUnreadable, spec)
	}
	for _, pallet := range meta.AsMetadataV14.Pallets {
		if !pallet.HasCalls {
			continue
		}
		callType, ok := meta.AsMetadataV14.EfficientLookup[pallet.Calls.Type.Int64()]
		if !ok || !callType.Def.IsVariant {
			continue
		}
		for _, variant := range callType.Def.Variant.Variants {
			runtime.callNames[[2]byte{byte(pallet.Index), byte(variant.Index)}] = string(pallet.Name) + "." + string(variant.Name)
		}
	}
	return runtime, nil
}

// ----- decoding -----

// cryptoTaoHexBytes reads a node's hex value.
func cryptoTaoHexBytes(value string) ([]byte, error) {
	if !strings.HasPrefix(value, "0x") {
		return nil, errors.New("not 0x hex")
	}
	return hex.DecodeString(value[2:])
}

// cryptoTaoDecodeSpec reads System.LastRuntimeUpgrade's spec_version (a
// compact u32 before the spec name).
func cryptoTaoDecodeSpec(value string) (uint32, error) {
	raw, err := cryptoTaoHexBytes(value)
	if err != nil || len(raw) == 0 {
		return 0, errors.New("no runtime upgrade record")
	}
	spec, err := scale.NewDecoder(bytes.NewReader(raw)).DecodeUintCompact()
	if err != nil || !spec.IsUint64() || spec.Uint64() == 0 || 1<<32 <= spec.Uint64() {
		return 0, errors.New("an unreadable runtime upgrade record")
	}
	return uint32(spec.Uint64()), nil
}

// cryptoTaoDecodeTimestamp reads Timestamp.Now (milliseconds, u64 little
// endian).
func cryptoTaoDecodeTimestamp(value string) (time.Time, error) {
	raw, err := cryptoTaoHexBytes(value)
	if err != nil || len(raw) != 8 {
		return time.Time{}, errors.New("an unreadable block time")
	}
	var millis uint64
	for i := 7; 0 <= i; i -= 1 {
		millis = millis<<8 | uint64(raw[i])
	}
	return time.UnixMilli(int64(millis)).UTC(), nil
}

// cryptoTaoBytes32 reads a decoded 32-byte account, however the dynamic
// decoder nests it (a composite of one [u8; 32]).
func cryptoTaoBytes32(value any) ([32]byte, bool) {
	var account [32]byte
	switch v := value.(type) {
	case registry.DecodedFields:
		if len(v) == 1 && v[0] != nil {
			return cryptoTaoBytes32(v[0].Value)
		}
	case *registry.DecodedField:
		if v != nil {
			return cryptoTaoBytes32(v.Value)
		}
	case []any:
		if len(v) != 32 {
			return account, false
		}
		for i, item := range v {
			b, ok := item.(types.U8)
			if !ok {
				return account, false
			}
			account[i] = byte(b)
		}
		return account, true
	case types.AccountID:
		return [32]byte(v), true
	case [32]byte:
		return v, true
	case []byte:
		if len(v) == 32 {
			copy(account[:], v)
			return account, true
		}
	}
	return account, false
}

// cryptoTaoU64 reads a decoded balance (a newtype of one u64).
func cryptoTaoU64(value any) (uint64, bool) {
	switch v := value.(type) {
	case registry.DecodedFields:
		if len(v) == 1 && v[0] != nil {
			return cryptoTaoU64(v[0].Value)
		}
	case *registry.DecodedField:
		if v != nil {
			return cryptoTaoU64(v.Value)
		}
	case types.U64:
		return uint64(v), true
	case types.U128:
		if v.Int != nil && v.IsUint64() {
			return v.Uint64(), true
		}
	case uint64:
		return v, true
	}
	return 0, false
}

// cryptoTaoFieldNamed reports a decoded event field by its name, which the
// dynamic decoder prefixes with the field's type path
// ("sp_core.crypto.AccountId32.to").
func cryptoTaoFieldNamed(field *registry.DecodedField, name string) bool {
	return field != nil && (field.Name == name || strings.HasSuffix(field.Name, "."+name))
}

// cryptoTaoTransfer is one Balances.Transfer event of a block.
type cryptoTaoTransfer struct {
	// the extrinsic that emitted it; -1 at the block's initialization, -2 at
	// its finalization
	extrinsic int
	from      [32]byte
	to        [32]byte
	amount    uint64
}

// cryptoTaoDecodeTransfers reads a block's Balances.Transfer events with its
// runtime's decoders.
func cryptoTaoDecodeTransfers(runtime *cryptoTaoRuntime, value string) (transfers []*cryptoTaoTransfer, returnErr error) {
	defer func() {
		if r := recover(); r != nil {
			transfers = nil
			returnErr = fmt.Errorf("%w: the events of spec %d did not decode: %v", errCryptoTaoRuntimeUnreadable, runtime.spec, r)
		}
	}()
	raw, err := cryptoTaoHexBytes(value)
	if err != nil {
		return nil, fmt.Errorf("%w: the block's events: %s", errCryptoTaoRuntimeUnreadable, err)
	}
	storage := types.NewStorageDataRaw(raw)
	events, err := parser.NewEventParser().ParseEvents(runtime.events, &storage)
	if err != nil {
		return nil, fmt.Errorf("%w: the events of spec %d did not decode: %s", errCryptoTaoRuntimeUnreadable, runtime.spec, err)
	}
	transfers = []*cryptoTaoTransfer{}
	for _, event := range events {
		if event == nil || event.Name != "Balances.Transfer" {
			continue
		}
		transfer := &cryptoTaoTransfer{extrinsic: -1}
		if event.Phase != nil {
			switch {
			case event.Phase.IsApplyExtrinsic:
				transfer.extrinsic = int(event.Phase.AsApplyExtrinsic)
			case event.Phase.IsFinalization:
				transfer.extrinsic = -2
			}
		}
		var haveFrom, haveTo, haveAmount bool
		for _, field := range event.Fields {
			switch {
			case cryptoTaoFieldNamed(field, "from"):
				transfer.from, haveFrom = cryptoTaoBytes32(field.Value)
			case cryptoTaoFieldNamed(field, "to"):
				transfer.to, haveTo = cryptoTaoBytes32(field.Value)
			case cryptoTaoFieldNamed(field, "amount"):
				transfer.amount, haveAmount = cryptoTaoU64(field.Value)
			}
		}
		if !haveFrom || !haveTo || !haveAmount {
			return nil, fmt.Errorf("%w: a Balances.Transfer of spec %d has fields the reader does not know", errCryptoTaoRuntimeUnreadable, runtime.spec)
		}
		transfers = append(transfers, transfer)
	}
	return transfers, nil
}

// cryptoTaoCall is an extrinsic's top-level call: its name, whether it is
// signed, and its signer when the signer is a plain account (MultiAddress::Id).
type cryptoTaoCall struct {
	name   string
	signed bool
	signer *[32]byte
}

// cryptoTaoDecodeCall reads an extrinsic's top-level call with its runtime's
// decoders: the address, the signature and the extensions of a signed
// extrinsic come first, then the call's pallet and call indexes. An extrinsic
// that does not decode (a signer the metadata does not describe, a format the
// reader does not read) is an error, never a panic.
func cryptoTaoDecodeCall(runtime *cryptoTaoRuntime, value string) (call *cryptoTaoCall, returnErr error) {
	defer func() {
		if r := recover(); r != nil {
			call = nil
			returnErr = fmt.Errorf("the extrinsic did not decode: %v", r)
		}
	}()
	raw, err := cryptoTaoHexBytes(value)
	if err != nil {
		return nil, err
	}
	reader := bytes.NewReader(raw)
	decoder := scale.NewDecoder(reader)
	if _, err := decoder.DecodeUintCompact(); err != nil {
		return nil, fmt.Errorf("the extrinsic's length: %w", err)
	}
	version, err := decoder.ReadOneByte()
	if err != nil {
		return nil, fmt.Errorf("the extrinsic's version: %w", err)
	}
	call = &cryptoTaoCall{}
	switch version {
	case 0x84:
		// a signed extrinsic, format 4
		call.signed = true
		offset := len(raw) - reader.Len()
		if offset+33 <= len(raw) && raw[offset] == 0x00 {
			signer := [32]byte{}
			copy(signer[:], raw[offset+1:offset+33])
			call.signer = &signer
		}
		for _, field := range []*registry.Field{runtime.address, runtime.signature, runtime.extra} {
			if _, err := field.Decode(decoder); err != nil {
				return nil, fmt.Errorf("the extrinsic's %s did not decode: %s", field.Name, err)
			}
		}
	case 0x04, 0x05:
		// unsigned (bare): the call follows at once
	default:
		// a general transaction (format 5 with extensions), or a format the
		// reader does not read
		return nil, fmt.Errorf("an extrinsic of format %#x the reader does not read", version)
	}
	offset := len(raw) - reader.Len()
	if len(raw) < offset+2 {
		return nil, errors.New("the extrinsic has no call")
	}
	name, ok := runtime.callNames[[2]byte{raw[offset], raw[offset+1]}]
	if !ok {
		return nil, fmt.Errorf("a call (%d, %d) spec %d does not name", raw[offset], raw[offset+1], runtime.spec)
	}
	call.name = name
	return call, nil
}

// cryptoTaoShapeCredited reports a transfer the rail credits by itself: its
// extrinsic's top-level call is in cryptoTaoCreditedCalls, signed, and, when
// the signer is a plain account, signed by the transfer's sender.
func cryptoTaoShapeCredited(call *cryptoTaoCall, from [32]byte) bool {
	if call == nil || !call.signed || !slices.Contains(cryptoTaoCreditedCalls, call.name) {
		return false
	}
	return call.signer == nil || *call.signer == from
}

// ----- blocks -----

// cryptoTaoBlock is one finalized block as the reader read it.
type cryptoTaoBlock struct {
	number     uint64
	hash       string
	parentHash string
	spec       uint32
	transfers  []*cryptoTaoTransfer

	readLock sync.Mutex
	// the extrinsics' hex, read once a transfer to an invoice address needs
	// its call; nil until then
	extrinsics []string
	calls      map[int]*cryptoTaoCall
	timestamp  time.Time
}

var cryptoTaoBlocksLock sync.Mutex
var cryptoTaoBlocks = map[string]*cryptoTaoBlock{}
var cryptoTaoBlockOrder = []string{}

func cryptoTaoCachedBlock(hash string) *cryptoTaoBlock {
	cryptoTaoBlocksLock.Lock()
	defer cryptoTaoBlocksLock.Unlock()
	return cryptoTaoBlocks[hash]
}

func cryptoTaoCacheBlock(block *cryptoTaoBlock) {
	cryptoTaoBlocksLock.Lock()
	defer cryptoTaoBlocksLock.Unlock()
	if _, ok := cryptoTaoBlocks[block.hash]; ok {
		return
	}
	cryptoTaoBlocks[block.hash] = block
	cryptoTaoBlockOrder = append(cryptoTaoBlockOrder, block.hash)
	for len(cryptoTaoBlockOrder) > cryptoTaoBlockCacheSize {
		delete(cryptoTaoBlocks, cryptoTaoBlockOrder[0])
		cryptoTaoBlockOrder = cryptoTaoBlockOrder[1:]
	}
}

// ----- the reader -----

// One scan's reader (or one reconcile's): its request budget and pacing are
// its own.
type bittensorWallet struct {
	*cryptoCoinbaseAddresses
	network  string
	rpc      cryptoTaoRpc
	settings *CryptoTaoReaderSettings

	stateLock sync.Mutex
	// the scan's view of the chain, read once per scan
	heads *cryptoTaoHeads
	// the last block IncomingSince read (or the finalized head, idle)
	lastRead uint64
	// a runtime other than the head's this scan read (at most one)
	olderSpec uint32
	// the requests made to the node, and when the last was let through
	requests      int
	lastRequestAt time.Time
}

// cryptoTaoHeads is the finalized and best heads.
type cryptoTaoHeads struct {
	finalizedHash string
	finalized     uint64
	finalizedTime time.Time
	finalizedSpec uint32
	best          uint64
}

func newBittensorWallet(settings *cryptoWalletSettings, assetConfig *CryptoPayAssetConfig) *bittensorWallet {
	return &bittensorWallet{
		cryptoCoinbaseAddresses: newCryptoCoinbaseAddresses(model.CryptoAssetTao, settings, cryptoTaoAddressCanonical),
		network:                 assetConfig.Network,
		rpc:                     cryptoTaoRpcFor(settings.ChainRpcUrl),
		settings:                DefaultCryptoTaoReaderSettings(),
	}
}

// Irreversible: only finalized blocks are read, and they never leave the
// chain.
func (self *bittensorWallet) Irreversible() bool {
	return true
}

// One request to the node, at most one each RequestSpacing (the wait holds
// no lock and ends with ctx), counted.
func (self *bittensorWallet) call(ctx context.Context, result any, method string, params ...any) error {
	var wait time.Duration
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		now := cryptoTaoPaceNow()
		allowedAt := now
		if 0 < self.settings.RequestSpacing && !self.lastRequestAt.IsZero() {
			allowedAt = self.lastRequestAt.Add(self.settings.RequestSpacing)
		}
		if now.Before(allowedAt) {
			wait = allowedAt.Sub(now)
		} else {
			allowedAt = now
		}
		self.lastRequestAt = allowedAt
		self.requests += 1
	}()
	if 0 < wait {
		if err := cryptoTaoPaceWait(ctx, wait); err != nil {
			return err
		}
	}
	cryptoPayTaoNodeRequests.WithLabelValues(method).Inc()
	return self.rpc.call(ctx, result, method, params...)
}

// What remains of the scan's request budget.
func (self *bittensorWallet) requestsLeft() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.settings.RequestsPerScan - self.requests
}

type cryptoTaoHeader struct {
	ParentHash string `json:"parentHash"`
	Number     string `json:"number"`
}

func cryptoTaoHeaderNumber(header *cryptoTaoHeader) (uint64, error) {
	if header == nil || !strings.HasPrefix(header.Number, "0x") {
		return 0, errors.New("a header without a number")
	}
	return strconv.ParseUint(header.Number[2:], 16, 64)
}

func (self *bittensorWallet) storage(ctx context.Context, key string, blockHash string) (string, error) {
	var value *string
	if err := self.call(ctx, &value, "state_getStorage", key, blockHash); err != nil {
		return "", err
	}
	if value == nil {
		return "", nil
	}
	return *value, nil
}

// readHeads reads the finalized head (its number, time and runtime) and the
// best head, once per scan.
func (self *bittensorWallet) readHeads(ctx context.Context) (*cryptoTaoHeads, error) {
	self.stateLock.Lock()
	heads := self.heads
	self.stateLock.Unlock()
	if heads != nil {
		return heads, nil
	}
	heads = &cryptoTaoHeads{}
	if err := self.call(ctx, &heads.finalizedHash, "chain_getFinalizedHead"); err != nil {
		return nil, err
	}
	var finalized cryptoTaoHeader
	if err := self.call(ctx, &finalized, "chain_getHeader", heads.finalizedHash); err != nil {
		return nil, err
	}
	number, err := cryptoTaoHeaderNumber(&finalized)
	if err != nil {
		return nil, err
	}
	heads.finalized = number
	var best cryptoTaoHeader
	if err := self.call(ctx, &best, "chain_getHeader"); err != nil {
		return nil, err
	}
	if heads.best, err = cryptoTaoHeaderNumber(&best); err != nil {
		return nil, err
	}
	now, err := self.storage(ctx, cryptoTaoKeyTimestampNow, heads.finalizedHash)
	if err != nil {
		return nil, err
	}
	if heads.finalizedTime, err = cryptoTaoDecodeTimestamp(now); err != nil {
		return nil, err
	}
	upgrade, err := self.storage(ctx, cryptoTaoKeyLastRuntimeUpgrade, heads.finalizedHash)
	if err != nil {
		return nil, err
	}
	if heads.finalizedSpec, err = cryptoTaoDecodeSpec(upgrade); err != nil {
		return nil, cryptoTaoPause(fmt.Errorf("%w: the finalized head: %s", errCryptoTaoRuntimeUnreadable, err))
	}
	self.stateLock.Lock()
	self.heads = heads
	self.stateLock.Unlock()
	return heads, nil
}

// runtimeFor is the runtime that executed a block (its spec read from the
// block's own storage), with its metadata fetched only for the finalized
// head's runtime or one other, older runtime per scan, at the block's parent
// (whose code executed the block). Any other is errCryptoTaoRuntimeUnreadable.
func (self *bittensorWallet) runtimeFor(ctx context.Context, heads *cryptoTaoHeads, spec uint32, parentHash string, fetchAllowed bool) (*cryptoTaoRuntime, error) {
	if runtime := cryptoTaoCachedRuntime(spec); runtime != nil {
		return runtime, nil
	}
	self.stateLock.Lock()
	olderSpec := self.olderSpec
	self.stateLock.Unlock()
	switch {
	case !fetchAllowed:
		return nil, fmt.Errorf("%w: spec %d of a block past the lookback is not cached", errCryptoTaoRuntimeUnreadable, spec)
	case spec == heads.finalizedSpec:
	case spec < heads.finalizedSpec && (olderSpec == 0 || olderSpec == spec):
	default:
		return nil, fmt.Errorf("%w: spec %d is neither the finalized head's (%d) nor the one before it", errCryptoTaoRuntimeUnreadable, spec, heads.finalizedSpec)
	}
	var version struct {
		SpecVersion uint32 `json:"specVersion"`
	}
	if err := self.call(ctx, &version, "state_getRuntimeVersion", parentHash); err != nil {
		return nil, err
	}
	if version.SpecVersion != spec {
		return nil, fmt.Errorf("%w: the parent of a block of spec %d runs spec %d", errCryptoTaoRuntimeUnreadable, spec, version.SpecVersion)
	}
	var encoded string
	if err := self.call(ctx, &encoded, "state_getMetadata", parentHash); err != nil {
		return nil, err
	}
	meta, _, err := crv4.DecodeRuntimeMetadata(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: the metadata of spec %d: %s", errCryptoTaoRuntimeUnreadable, spec, err)
	}
	runtime, err := cryptoTaoNewRuntime(meta, spec)
	if err != nil {
		return nil, err
	}
	cryptoTaoCacheRuntime(runtime)
	if spec != heads.finalizedSpec {
		self.stateLock.Lock()
		self.olderSpec = spec
		self.stateLock.Unlock()
	}
	return runtime, nil
}

// readBlock reads one finalized block's transfers, from the cache or the
// node. fetchAllowed = false reads only a block whose runtime is cached.
func (self *bittensorWallet) readBlock(ctx context.Context, heads *cryptoTaoHeads, number uint64, fetchAllowed bool) (*cryptoTaoBlock, error) {
	var hash string
	if err := self.call(ctx, &hash, "chain_getBlockHash", number); err != nil {
		return nil, err
	}
	if hash == "" {
		return nil, fmt.Errorf("the node has no block %d", number)
	}
	if block := cryptoTaoCachedBlock(hash); block != nil {
		return block, nil
	}
	upgrade, err := self.storage(ctx, cryptoTaoKeyLastRuntimeUpgrade, hash)
	if err != nil {
		return nil, err
	}
	spec, err := cryptoTaoDecodeSpec(upgrade)
	if err != nil {
		return nil, fmt.Errorf("%w: block %d: %s", errCryptoTaoRuntimeUnreadable, number, err)
	}
	var header cryptoTaoHeader
	if err := self.call(ctx, &header, "chain_getHeader", hash); err != nil {
		return nil, err
	}
	runtime, err := self.runtimeFor(ctx, heads, spec, header.ParentHash, fetchAllowed)
	if err != nil {
		return nil, err
	}
	events, err := self.storage(ctx, cryptoTaoKeySystemEvents, hash)
	if err != nil {
		return nil, err
	}
	transfers, err := cryptoTaoDecodeTransfers(runtime, events)
	if err != nil {
		return nil, err
	}
	block := &cryptoTaoBlock{
		number:     number,
		hash:       hash,
		parentHash: header.ParentHash,
		spec:       spec,
		transfers:  transfers,
	}
	cryptoTaoCacheBlock(block)
	return block, nil
}

// blockCall is the top-level call of one of a block's extrinsics, read from
// the block's body the first time a transfer to an invoice address needs it.
// nil when the extrinsic does not decode (skipped and counted).
func (self *bittensorWallet) blockCall(ctx context.Context, block *cryptoTaoBlock, extrinsic int) (*cryptoTaoCall, error) {
	block.readLock.Lock()
	defer block.readLock.Unlock()
	if call, ok := block.calls[extrinsic]; ok {
		return call, nil
	}
	if block.extrinsics == nil {
		var body struct {
			Block struct {
				Extrinsics []string `json:"extrinsics"`
			} `json:"block"`
		}
		if err := self.call(ctx, &body, "chain_getBlock", block.hash); err != nil {
			return nil, err
		}
		block.extrinsics = body.Block.Extrinsics
		if block.extrinsics == nil {
			block.extrinsics = []string{}
		}
	}
	if block.calls == nil {
		block.calls = map[int]*cryptoTaoCall{}
	}
	var call *cryptoTaoCall
	if 0 <= extrinsic && extrinsic < len(block.extrinsics) {
		runtime := cryptoTaoCachedRuntime(block.spec)
		if runtime == nil {
			return nil, fmt.Errorf("%w: spec %d is no longer cached", errCryptoTaoRuntimeUnreadable, block.spec)
		}
		decoded, err := cryptoTaoDecodeCall(runtime, block.extrinsics[extrinsic])
		if err != nil {
			cryptoPayTaoExtrinsicsUndecodable.Inc()
			glog.Infof("[cryptopay]tao: extrinsic %d-%d skipped, it did not decode: %s\n", block.number, extrinsic, err)
		} else {
			call = decoded
		}
	}
	block.calls[extrinsic] = call
	return call, nil
}

// blockTime is the block's Timestamp.Now, read once.
func (self *bittensorWallet) blockTime(ctx context.Context, block *cryptoTaoBlock) (time.Time, error) {
	block.readLock.Lock()
	defer block.readLock.Unlock()
	if !block.timestamp.IsZero() {
		return block.timestamp, nil
	}
	now, err := self.storage(ctx, cryptoTaoKeyTimestampNow, block.hash)
	if err != nil {
		return time.Time{}, err
	}
	timestamp, err := cryptoTaoDecodeTimestamp(now)
	if err != nil {
		return time.Time{}, err
	}
	block.timestamp = timestamp
	return timestamp, nil
}

// cryptoTaoTxId is the receipt's transaction id: the block and the
// extrinsic's index ("<block>-init" and "<block>-final" for a transfer made
// by the block's hooks).
func cryptoTaoTxId(number uint64, extrinsic int) string {
	switch extrinsic {
	case -1:
		return fmt.Sprintf("%d-init", number)
	case -2:
		return fmt.Sprintf("%d-final", number)
	}
	return fmt.Sprintf("%d-%d", number, extrinsic)
}

// cryptoTaoParseTxId reads a receipt's transaction id.
func cryptoTaoParseTxId(txId string) (number uint64, extrinsic int, err error) {
	blockText, extrinsicText, ok := strings.Cut(txId, "-")
	if !ok {
		return 0, 0, errors.New("not a TAO transaction id")
	}
	number, err = strconv.ParseUint(blockText, 10, 64)
	if err != nil {
		return 0, 0, errors.New("not a TAO transaction id")
	}
	switch extrinsicText {
	case "init":
		return number, -1, nil
	case "final":
		return number, -2, nil
	}
	index, err := strconv.ParseUint(extrinsicText, 10, 32)
	if err != nil {
		return 0, 0, errors.New("not a TAO transaction id")
	}
	return number, int(index), nil
}

// cryptoTaoOurs is an invoice account the reader looks for, by its 32 bytes.
type cryptoTaoOurs struct {
	address string
	index   int64
	// when its invoice was made, for the match of a block mined before it;
	// zero matches every block (a re-read of a recorded receipt)
	createdAt time.Time
}

// The invoice accounts among the destinations of the blocks' transfers: every
// address a TAO invoice was ever given, in one lookup.
func cryptoTaoIssuedOurs(ctx context.Context, blocks []*cryptoTaoBlock) map[[32]byte]*cryptoTaoOurs {
	ours := map[[32]byte]*cryptoTaoOurs{}
	destinations := []string{}
	seen := map[[32]byte]bool{}
	for _, block := range blocks {
		for _, transfer := range block.transfers {
			if !seen[transfer.to] {
				seen[transfer.to] = true
				destinations = append(destinations, cryptoTaoAddressOf(transfer.to))
			}
		}
	}
	if len(destinations) == 0 {
		return ours
	}
	for address, issuedAddress := range cryptoTaoIssuedAddresses(ctx, destinations) {
		account, err := cryptoTaoAccountOf(address)
		if err != nil {
			continue
		}
		ours[account] = &cryptoTaoOurs{address: address, index: issuedAddress.AddressIndex, createdAt: issuedAddress.CreateTime}
	}
	return ours
}

// blockReceipts reads a block's transfers to our accounts as receipts: summed
// per account and extrinsic, held for review (transfer_shape) unless the
// extrinsic's shape is one the rail credits. An account whose invoice was
// made after the block was mined (less the clock tolerance) is not matched
// in it.
func (self *bittensorWallet) blockReceipts(ctx context.Context, block *cryptoTaoBlock, ours map[[32]byte]*cryptoTaoOurs, onlyExtrinsic *int) ([]*cryptoWalletReceipt, error) {
	type key struct {
		extrinsic int
		to        [32]byte
	}
	sums := map[key]uint64{}
	senders := map[key][]string{}
	credited := map[key]bool{}
	order := []key{}
	for _, transfer := range block.transfers {
		if onlyExtrinsic != nil && transfer.extrinsic != *onlyExtrinsic {
			continue
		}
		if _, ok := ours[transfer.to]; !ok {
			continue
		}
		k := key{extrinsic: transfer.extrinsic, to: transfer.to}
		if _, seen := sums[k]; !seen {
			order = append(order, k)
			credited[k] = true
		}
		sums[k] = min(sums[k]+transfer.amount, 1<<62)
		sender := cryptoTaoAddressOf(transfer.from)
		if !slices.Contains(senders[k], sender) {
			senders[k] = append(senders[k], sender)
		}
		var call *cryptoTaoCall
		if 0 <= transfer.extrinsic {
			var err error
			if call, err = self.blockCall(ctx, block, transfer.extrinsic); err != nil {
				return nil, err
			}
		}
		if !cryptoTaoShapeCredited(call, transfer.from) {
			credited[k] = false
		}
	}
	if len(order) == 0 {
		return []*cryptoWalletReceipt{}, nil
	}
	timestamp, err := self.blockTime(ctx, block)
	if err != nil {
		return nil, err
	}
	receipts := []*cryptoWalletReceipt{}
	for _, k := range order {
		account := ours[k.to]
		if !account.createdAt.IsZero() && timestamp.Add(self.settings.ClockTolerance).Before(account.createdAt) {
			// an invoice made after the block was mined was not paid in it
			continue
		}
		receipt := &cryptoWalletReceipt{
			TxId:          cryptoTaoTxId(block.number, k.extrinsic),
			OutputKey:     strconv.FormatInt(account.index, 10),
			Address:       account.address,
			AddressIndex:  cryptoIndexOf(account.index),
			AmountAtomic:  sums[k],
			BlockHeight:   block.number,
			Confirmations: 1,
			Timestamp:     timestamp,
			Senders:       senders[k],
		}
		if !credited[k] {
			receipt.ReviewReason = model.CryptoReviewReasonTransferShape
		}
		receipts = append(receipts, receipt)
	}
	return receipts, nil
}

// cryptoTaoScanRange is which finalized blocks one scan reads: from
// minHeight (the cursor less its window), never further back than the
// lookback (the blocks passed over are counted), at most BlocksPerScan, up to
// the finalized head. A first scan (minHeight 0) starts at the lookback
// without counting. ok = false: the settings read no block.
func cryptoTaoScanRange(settings *CryptoTaoReaderSettings, minHeight uint64, finalized uint64) (start uint64, end uint64, skipped uint64, ok bool) {
	if settings.BlocksPerScan <= 0 {
		return 0, 0, 0, false
	}
	floor := uint64(0)
	if settings.LookbackBlocks < finalized {
		floor = finalized - settings.LookbackBlocks
	}
	start = minHeight
	if start < floor {
		if 0 < minHeight {
			skipped = floor - minHeight
		}
		start = floor
	}
	if finalized < start {
		start = finalized
	}
	end = min(finalized, start+uint64(settings.BlocksPerScan)-1)
	return start, end, skipped, true
}

// cryptoTaoPause marks the reader paused on a runtime it cannot read; a scan
// that reads again clears it.
func cryptoTaoPause(err error) error {
	if errors.Is(err, errCryptoTaoRuntimeUnreadable) {
		cryptoPayTaoReaderPaused.Set(1)
		cryptoScanFailureReports.report("tao/runtime", func() {
			glog.Errorf("[cryptopay]the TAO reader paused, nothing is credited until it reads again: %s\n", err)
		})
	}
	return err
}

// Reads the finalized blocks of the scan's range (see the file header) and
// reports their transfers to every address a TAO invoice was ever
// given. It reads no block until an address has been issued. It stops before
// a block whose reads could pass the scan's request budget, and reports the
// last block it read: the next scan goes on from there.
func (self *bittensorWallet) IncomingSince(ctx context.Context, minHeight uint64) ([]*cryptoWalletReceipt, uint64, error) {
	heads, err := self.readHeads(ctx)
	if err != nil {
		return nil, 0, err
	}
	idle := func() ([]*cryptoWalletReceipt, uint64, error) {
		self.stateLock.Lock()
		self.lastRead = heads.finalized
		self.stateLock.Unlock()
		cryptoPayTaoReaderPaused.Set(0)
		return []*cryptoWalletReceipt{}, heads.finalized, nil
	}
	if !cryptoTaoAddressesIssued(ctx) {
		// no TAO invoice address was ever issued: no block is read
		return idle()
	}
	start, end, skipped, ok := cryptoTaoScanRange(self.settings, minHeight, heads.finalized)
	if !ok {
		return idle()
	}
	if 0 < skipped {
		cryptoPayScanBlocksSkipped.WithLabelValues(model.CryptoAssetTao).Add(float64(skipped))
		glog.Errorf("[cryptopay]tao: %d finalized blocks are past the reader's lookback and were not read (from %d to %d)\n", skipped, minHeight, start-1)
	}
	blocks := []*cryptoTaoBlock{}
	for number := start; number <= end; number += 1 {
		if self.requestsLeft() < cryptoTaoBlockRequests+cryptoTaoBlockPaymentRequests {
			// the scan's budget: the next scan goes on from here
			break
		}
		block, err := self.readBlock(ctx, heads, number, true)
		if err != nil {
			return nil, 0, cryptoTaoPause(err)
		}
		blocks = append(blocks, block)
	}
	if len(blocks) == 0 {
		// nothing read: the next scan starts where this one would have
		lastRead := max(start, 2) - 1
		self.stateLock.Lock()
		self.lastRead = lastRead
		self.stateLock.Unlock()
		return []*cryptoWalletReceipt{}, lastRead, nil
	}
	ours := cryptoTaoIssuedOurs(ctx, blocks)
	receipts := []*cryptoWalletReceipt{}
	for _, block := range blocks {
		blockReceipts, err := self.blockReceipts(ctx, block, ours, nil)
		if err != nil {
			return nil, 0, cryptoTaoPause(err)
		}
		receipts = append(receipts, blockReceipts...)
	}
	lastRead := blocks[len(blocks)-1].number
	self.stateLock.Lock()
	self.lastRead = lastRead
	self.stateLock.Unlock()
	cryptoPayTaoReaderPaused.Set(0)
	return receipts, lastRead, nil
}

// Transaction re-reads one receipt's block for its transfers to the pool's
// addresses. A block within the lookback is read as a scan reads it; one past
// it only when its runtime's metadata is already cached (no runtime call is
// made for it), else an error, never "not found": a finalized block never
// leaves the chain.
func (self *bittensorWallet) Transaction(ctx context.Context, txId string) ([]*cryptoWalletReceipt, bool, error) {
	number, extrinsic, err := cryptoTaoParseTxId(txId)
	if err != nil {
		return nil, false, err
	}
	heads, err := self.readHeads(ctx)
	if err != nil {
		return nil, false, err
	}
	if heads.finalized < number {
		return nil, false, nil
	}
	withinLookback := heads.finalized < number+self.settings.LookbackBlocks
	block, err := self.readBlock(ctx, heads, number, withinLookback)
	if err != nil {
		return nil, false, err
	}
	addresses := []string{}
	for _, transfer := range block.transfers {
		if transfer.extrinsic == extrinsic {
			addresses = append(addresses, cryptoTaoAddressOf(transfer.to))
		}
	}
	ours := map[[32]byte]*cryptoTaoOurs{}
	for address, index := range cryptoResolveAddresses(ctx, model.CryptoAssetTao, addresses) {
		if account, err := cryptoTaoAccountOf(address); err == nil {
			ours[account] = &cryptoTaoOurs{address: address, index: index}
		}
	}
	receipts, err := self.blockReceipts(ctx, block, ours, &extrinsic)
	if err != nil {
		return nil, false, err
	}
	return receipts, true, nil
}

// Heights: the wallet's height is the last block read, the node's the
// finalized head, the network's the best head. A finalized head older than
// cryptoTaoStaleFinality (a node or a finality that stalled) is an unknown
// network height: TAO is not sold until it moves again.
func (self *bittensorWallet) Heights(ctx context.Context) (uint64, uint64, uint64, error) {
	heads, err := self.readHeads(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	self.stateLock.Lock()
	walletHeight := self.lastRead
	self.stateLock.Unlock()
	if walletHeight == 0 {
		walletHeight = heads.finalized
	}
	networkHeight := max(heads.best, heads.finalized)
	if cryptoTaoStaleFinality < time.Since(heads.finalizedTime) {
		networkHeight = 0
	}
	return walletHeight, heads.finalized, networkHeight, nil
}

// CheckNetwork refuses a node of another chain than Finney.
func (self *bittensorWallet) CheckNetwork(ctx context.Context) error {
	if self.network != cryptoNetworkMainnet {
		return fmt.Errorf("TAO is received on mainnet only, not %s", self.network)
	}
	var genesis string
	if err := self.call(ctx, &genesis, "chain_getBlockHash", 0); err != nil {
		return err
	}
	if !strings.EqualFold(genesis, cryptoTaoFinneyGenesis) {
		return errors.New("the TAO node is not on Finney")
	}
	return nil
}

// cryptoTaoRefundAddressCanonical is a refund destination a customer gave, as
// the SS58 address (prefix 42) a refund is sent to: an SS58 account under any
// prefix. A quantum-resistant `tao_` address is not taken: no primary source
// (Bittensor's or subtensor's docs or code) was found for its format on
// 2026-11-10, so the customer gives the SS58 address of the account instead.
// The destination is never the payment's observed sender, which may be an EVM
// mirror, a multisig, a proxy or an exchange.
func cryptoTaoRefundAddressCanonical(address string) (string, error) {
	return cryptoTaoAddressCanonical(address)
}
