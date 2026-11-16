package controller

// Hermetic tests of Pay with Gamma's chain layer against answers recorded
// from the public Finney endpoints on 2026-11-10 (testdata/gamma): runtime
// 488's and 490's metadata, the blocks of the first mainnet spend_gamma (SN103,
// block 9,477,012) and the mint before it (9,477,009), the published services
// and deposits, and the live verdicts of runtime 490 on a spend we signed.
// No network, no database.

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/centrifuge/go-substrate-rpc-client/v4/signature"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types/codec"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026/model"
)

// The well-known development key //Alice's mini-secret: public test data.
const gammaTestAliceSeed = "0xe5be9a5092b81bca64be81d212e7f2f9eba183bb7a90954f7b76361f6edb5c0a"

var gammaTestRuntimesOnce sync.Once
var gammaTestRuntimes map[uint32]*gammaRuntime

func gammaTestMetadataHex(t testing.TB, spec uint32) string {
	f, err := os.Open(fmt.Sprintf("testdata/gamma/metadata-%d.hex.gz", spec))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	reader, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(bytes.TrimSpace(raw))
}

// gammaTestRuntime is a recorded runtime, decoded once.
func gammaTestRuntime(t testing.TB, spec uint32) *gammaRuntime {
	gammaTestRuntimesOnce.Do(func() {
		gammaTestRuntimes = map[uint32]*gammaRuntime{}
		for _, s := range []uint32{488, 490} {
			runtime, err := newGammaRuntime(s, 1, gammaTestMetadataHex(t, s))
			if err != nil {
				t.Fatal(err)
			}
			gammaTestRuntimes[s] = runtime
		}
	})
	return gammaTestRuntimes[spec]
}

type gammaRecordedBlock struct {
	Hash        string   `json:"hash"`
	ParentHash  string   `json:"parent_hash"`
	SpecVersion uint32   `json:"spec_version"`
	Events      string   `json:"events"`
	Extrinsics  []string `json:"extrinsics"`
}

type gammaRecorded struct {
	Blocks  map[string]*gammaRecordedBlock `json:"blocks"`
	Storage map[string]json.RawMessage     `json:"storage"`
}

func gammaTestRecorded(t testing.TB) *gammaRecorded {
	raw, err := os.ReadFile("testdata/gamma/finney-recorded.json")
	if err != nil {
		t.Fatal(err)
	}
	recorded := &gammaRecorded{}
	if err := json.Unmarshal(raw, recorded); err != nil {
		t.Fatal(err)
	}
	return recorded
}

func gammaTestHex(t testing.TB, text string) []byte {
	b, err := codec.HexDecodeString(text)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func gammaTestAccount(t testing.TB, address string) [32]byte {
	publicKey, err := model.DecodeBittensorAddress(address)
	if err != nil {
		t.Fatal(err)
	}
	return publicKey
}

func gammaTest32(t testing.TB, text string) [32]byte {
	var out [32]byte
	b := gammaTestHex(t, text)
	if len(b) != 32 {
		t.Fatalf("%s is not 32 bytes", text)
	}
	copy(out[:], b)
	return out
}

func TestGammaReviewedRuntimesAreSupported(t *testing.T) {
	for _, spec := range []uint32{488, 490} {
		runtime := gammaTestRuntime(t, spec)
		connect.AssertEqual(t, runtime.Supported(), nil)
		connect.AssertEqual(t, runtime.shape.subtensorPallet, uint8(7))
		connect.AssertEqual(t, runtime.shape.gammaSpendProxyType, uint8(22))
		for _, name := range gammaSkipReasonErrors {
			found := false
			for _, errorName := range runtime.shape.subtensorErrors {
				found = found || errorName == name
			}
			connect.AssertEqual(t, found, true)
		}
	}
}

// A runtime whose GammaSpent no longer has the fields reviewed is refused,
// with the event named: the biller pauses rather than decode on a guess.
func TestGammaRuntimeWithAChangedEventIsUnsupported(t *testing.T) {
	runtime, err := newGammaRuntime(490, 1, gammaTestMetadataHex(t, 490))
	if err != nil {
		t.Fatal(err)
	}
	m := &runtime.Meta.AsMetadataV14
	changed := false
	for i := range m.Pallets {
		if string(m.Pallets[i].Name) != "SubtensorModule" {
			continue
		}
		events := m.EfficientLookup[m.Pallets[i].Events.Type.Int64()]
		for v := range events.Def.Variant.Variants {
			variant := &events.Def.Variant.Variants[v]
			if string(variant.Name) == "GammaSpent" {
				variant.Fields[5].Name = "receipt"
				changed = true
			}
		}
	}
	connect.AssertEqual(t, changed, true)
	_, err = gammaCheckShape(runtime.Meta)
	if err == nil || !strings.Contains(err.Error(), "SubtensorModule.GammaSpent") {
		t.Fatalf("a changed GammaSpent was admitted: %v", err)
	}
}

// The first mainnet spend_gamma (SN103 Terza, block 9,477,012, extrinsic 24)
// decodes with runtime 488's metadata, field by field.
func TestGammaDecodesTheRecordedSpendAndMint(t *testing.T) {
	recorded := gammaTestRecorded(t)
	runtime := gammaTestRuntime(t, 488)

	spendBlock := recorded.Blocks["9477012"]
	events, err := gammaDecodeEvents(runtime, gammaTestHex(t, spendBlock.Events))
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, len(events), 249)
	var spend *gammaSpendEvent
	fee := uint64(0)
	for _, event := range events {
		switch event.Name {
		case "SubtensorModule.GammaSpent":
			spend, err = gammaSpendEventFrom(runtime, event)
			if err != nil {
				t.Fatal(err)
			}
		case "TransactionPayment.TransactionFeePaid":
			if event.ExtrinsicIndex == 24 {
				fee, _ = gammaEventUint(event, "actual_fee")
			}
		}
	}
	if spend == nil {
		t.Fatal("no GammaSpent")
	}
	connect.AssertEqual(t, spend.Event.Index, 226)
	connect.AssertEqual(t, spend.Event.ExtrinsicIndex, int64(24))
	connect.AssertEqual(t, spend.Skipped, false)
	connect.AssertEqual(t, spend.Netuid, uint16(103))
	connect.AssertEqual(t, spend.Amount, uint64(417_339_334))
	connect.AssertEqual(t, spend.Coldkey, gammaTest32(t, "0xcaa87dba3cc841b91cde7126fad7f8d1bf5d6a8e0444082fceef076c9f5fa806"))
	connect.AssertEqual(t, spend.Hotkey, gammaTest32(t, "0xd222428f0dd79f4172882d0d7a696f4c949dbc92070d3ee585fa77f37fea1a6e"))
	connect.AssertEqual(t, spend.Spender, gammaTest32(t, "0x1453b8a74ce17194427bcfb30c485862acf89f5ff64c6d5884e62e2e00447e2a"))
	connect.AssertEqual(t, spend.Reference, gammaTest32(t, "0x77a60576632dac148a7b2a0ba17ed41eb5c71c3f9e688b8774108862011f50a6"))
	connect.AssertEqual(t, fee, uint64(92_248))

	// the block's extrinsic 24 is the spend, by hash
	connect.AssertEqual(t, gammaExtrinsicHash(gammaTestHex(t, spendBlock.Extrinsics[24])).Hex(), "0x9288e7a62437cd508236f6437fe45b8d4b18dea35b0b2eb9c9041ebd6a82ee91")

	mintBlock := recorded.Blocks["9477009"]
	events, err = gammaDecodeEvents(runtime, gammaTestHex(t, mintBlock.Events))
	if err != nil {
		t.Fatal(err)
	}
	minted := 0
	for _, event := range events {
		if event.Name != "SubtensorModule.GammaMinted" {
			continue
		}
		minted++
		netuid, _ := gammaEventUint(event, "netuid")
		gamma, _ := gammaEventUint(event, "gamma")
		hotkey, _ := gammaEventBytes32(event, "hotkey")
		spender, _ := gammaEventBytes32(event, "spender")
		connect.AssertEqual(t, netuid, uint64(103))
		connect.AssertEqual(t, gamma, uint64(417_339_334))
		connect.AssertEqual(t, hotkey, spend.Hotkey)
		connect.AssertEqual(t, spender, spend.Spender)
	}
	connect.AssertEqual(t, minted, 1)
}

// Our encoding of spend_gamma reproduces, byte for byte, the call the chain
// accepted in block 9,477,012.
func TestGammaSpendCallReproducesTheRecordedCall(t *testing.T) {
	recorded := gammaTestRecorded(t)
	runtime := gammaTestRuntime(t, 488)
	extrinsic := hex.EncodeToString(gammaTestHex(t, recorded.Blocks["9477012"].Extrinsics[24]))
	call, err := gammaInnerSpendCall(runtime, 103, []gammaSpendEntry{{
		Hotkey:    gammaTest32(t, "0xd222428f0dd79f4172882d0d7a696f4c949dbc92070d3ee585fa77f37fea1a6e"),
		Amount:    417_339_334,
		Reference: gammaTest32(t, "0x77a60576632dac148a7b2a0ba17ed41eb5c71c3f9e688b8774108862011f50a6"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := codec.Encode(call)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(extrinsic, hex.EncodeToString(encoded)) {
		t.Fatalf("our call %x is not the recorded extrinsic's %s", encoded, extrinsic)
	}
}

// The proxied spend is Proxy.proxy(Id(spender), Some(GammaSpend),
// spend_gamma(25, entries)) in runtime 490's indices.
func TestGammaProxiedSpendCallEncoding(t *testing.T) {
	runtime := gammaTestRuntime(t, 490)
	spender := gammaTestAccount(t, "5EUQU938kFzJsVBjn9ch5RNNG4191j7mSz5Djf5vghrmWgUC")
	hotkey := gammaTestAccount(t, "5CMWj1VteMAXa2Popwo7PvjoCtwEC7HYZraqaXLk1FKktqVw")
	var reference [32]byte
	for i := range reference {
		reference[i] = byte(i)
	}
	call, err := gammaSpendCall(runtime, 25, spender, []gammaSpendEntry{{Hotkey: hotkey, Amount: 40_000_000_000, Reference: reference}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := codec.Encode(call)
	if err != nil {
		t.Fatal(err)
	}
	amount := make([]byte, 8)
	binary.LittleEndian.PutUint64(amount, 40_000_000_000)
	want := "1000" + "00" + hex.EncodeToString(spender[:]) + "0116" +
		"07a4" + "1900" + "04" + hex.EncodeToString(hotkey[:]) + hex.EncodeToString(amount) + hex.EncodeToString(reference[:])
	connect.AssertEqual(t, hex.EncodeToString(encoded), want)

	_, err = gammaSpendCall(runtime, 25, spender, nil)
	if err == nil {
		t.Fatal("an empty spend was built")
	}
}

// Substrate's own vectors (sp-runtime generic/era.rs).
func TestGammaMortalEraEncoding(t *testing.T) {
	for _, test := range []struct {
		birth, period uint64
		want          []byte
	}{
		{42, 64, []byte{165, 2}},
		{20000, 32768, []byte{78, 156}},
	} {
		era, err := gammaMortalEra(test.birth, test.period)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, []byte{era.AsMortalEra.First, era.AsMortalEra.Second}, test.want)
	}
	if _, err := gammaMortalEra(1, 100); err == nil {
		t.Fatal("a period that is not a power of two was taken")
	}
}

// gammaTestCompact reads a SCALE compact integer.
func gammaTestCompact(t testing.TB, b []byte) (uint64, int) {
	switch b[0] & 3 {
	case 0:
		return uint64(b[0] >> 2), 1
	case 1:
		return uint64(binary.LittleEndian.Uint16(b[0:2]) >> 2), 2
	case 2:
		return uint64(binary.LittleEndian.Uint32(b[0:4]) >> 2), 4
	}
	t.Fatal("big compact")
	return 0, 0
}

// gammaTestSignedPayload rebuilds, independently of the signing library, the
// payload a signed extrinsic's signature covers under runtime 488/490's
// signed extensions: the call, then the explicit extras (era, nonce, tip,
// metadata-hash mode), then the implicit ones (spec version, transaction
// version, genesis, the era's birth block, no metadata hash).
func gammaTestSignedPayload(t testing.TB, encoded []byte, spec uint32, txVersion uint32, genesis types.Hash, birth types.Hash) (signer [32]byte, sig []byte, payload []byte) {
	_, n := gammaTestCompact(t, encoded)
	b := encoded[n:]
	if b[0] != 0x84 || b[1] != 0x00 || b[34] != 0x01 {
		t.Fatalf("not a signed sr25519 extrinsic: %x", b[:36])
	}
	copy(signer[:], b[2:34])
	sig = b[35:99]
	rest := b[99:]
	// era: 1 byte immortal or 2 mortal
	eraLen := 2
	if rest[0] == 0 {
		eraLen = 1
	}
	_, nonceLen := gammaTestCompact(t, rest[eraLen:])
	_, tipLen := gammaTestCompact(t, rest[eraLen+nonceLen:])
	extraLen := eraLen + nonceLen + tipLen + 1
	extra := rest[:extraLen]
	call := rest[extraLen:]
	payload = append(payload, call...)
	payload = append(payload, extra...)
	payload = binary.LittleEndian.AppendUint32(payload, spec)
	payload = binary.LittleEndian.AppendUint32(payload, txVersion)
	payload = append(payload, genesis[:]...)
	payload = append(payload, birth[:]...)
	payload = append(payload, 0)
	if 256 < len(payload) {
		payload = blake2bSum256(payload)
	}
	return
}

// A spend we sign verifies over the payload Substrate defines, with the
// signer's key, in the substrate signing context.
func TestGammaSignedSpendVerifies(t *testing.T) {
	runtime := gammaTestRuntime(t, 490)
	signer, err := signature.KeyringPairFromSecret(gammaTestAliceSeed, 42)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, signer.Address, "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY")
	spender := gammaTestAccount(t, "5EUQU938kFzJsVBjn9ch5RNNG4191j7mSz5Djf5vghrmWgUC")
	entries := []gammaSpendEntry{}
	for i := range 3 {
		var reference [32]byte
		reference[0] = byte(i)
		entries = append(entries, gammaSpendEntry{Hotkey: gammaTestAccount(t, "5CMWj1VteMAXa2Popwo7PvjoCtwEC7HYZraqaXLk1FKktqVw"), Amount: uint64(i+1) * 1_000_000_000, Reference: reference})
	}
	call, err := gammaSpendCall(runtime, 25, spender, entries)
	if err != nil {
		t.Fatal(err)
	}
	genesis := types.NewHash(gammaTestHex(t, "0x2f0555cc76fc2840a25a6ea3b9637146806f1f44b090c175ffde2a7e5ab36c03"))
	birth := types.NewHash(blake2bSum256([]byte("birth")))
	signed, err := gammaSignSpend(runtime, genesis, 9_479_625, birth, 64, 7, call, signer)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, signed.Hash, gammaExtrinsicHash(signed.Encoded))
	publicKey, sig, payload := gammaTestSignedPayload(t, signed.Encoded, 490, 1, genesis, birth)
	connect.AssertEqual(t, publicKey, gammaTestAccount(t, signer.Address))
	ok, err := model.VerifyBittensorSignature(signer.Address, string(payload), hex.EncodeToString(sig))
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, ok, true)

	// another birth block is another payload: the signature does not carry over
	_, _, otherPayload := gammaTestSignedPayload(t, signed.Encoded, 490, 1, genesis, types.NewHash(blake2bSum256([]byte("other"))))
	ok, _ = model.VerifyBittensorSignature(signer.Address, string(otherPayload), hex.EncodeToString(sig))
	connect.AssertEqual(t, ok, false)
}

type gammaLiveRecorded struct {
	Block       uint64 `json:"block"`
	SpecVersion uint32 `json:"spec_version"`
	SimMint25   struct {
		Result string `json:"result"`
	} `json:"sim_mint_gamma_25"`
	SimMint58 struct {
		Result string `json:"result"`
	} `json:"sim_mint_gamma_58"`
	Validate struct {
		Signer    string `json:"signer"`
		Birth     uint64 `json:"birth"`
		BirthHash string `json:"birth_hash"`
		Genesis   string `json:"genesis"`
		Extrinsic string `json:"extrinsic"`
		Hash      string `json:"hash"`
		Verdict   string `json:"verdict"`
		FeeRao    uint64 `json:"fee_rao"`
	} `json:"validate_transaction"`
	OwnerBatchAll struct {
		Call          string `json:"call"`
		CallHash      string `json:"call_hash"`
		QueryCallInfo string `json:"query_call_info"`
	} `json:"owner_batch_all"`
}

func gammaTestLiveRecorded(t testing.TB) *gammaLiveRecorded {
	raw, err := os.ReadFile("testdata/gamma/finney-live-recorded.json")
	if err != nil {
		t.Fatal(err)
	}
	recorded := &gammaLiveRecorded{}
	if err := json.Unmarshal(raw, recorded); err != nil {
		t.Fatal(err)
	}
	return recorded
}

// Runtime 490 itself judged a spend this code signed (with a random, unfunded
// key, never submitted): TaggedTransactionQueue_validate_transaction answered
// Invalid(Payment), not BadProof. The signature, era, nonce and every signed
// extension passed; only the fee was missing. The recorded extrinsic still
// verifies over the payload rebuilt here.
func TestGammaRuntime490AcceptedOurSignedSpend(t *testing.T) {
	live := gammaTestLiveRecorded(t)
	connect.AssertEqual(t, live.SpecVersion, uint32(490))
	// Err(TransactionValidityError::Invalid(InvalidTransaction::Payment))
	connect.AssertEqual(t, live.Validate.Verdict, "0x010001")
	encoded := gammaTestHex(t, live.Validate.Extrinsic)
	connect.AssertEqual(t, gammaExtrinsicHash(encoded).Hex(), live.Validate.Hash)
	genesis := types.NewHash(gammaTestHex(t, live.Validate.Genesis))
	birth := types.NewHash(gammaTestHex(t, live.Validate.BirthHash))
	_, sig, payload := gammaTestSignedPayload(t, encoded, 490, 1, genesis, birth)
	ok, err := model.VerifyBittensorSignature(live.Validate.Signer, string(payload), hex.EncodeToString(sig))
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, ok, true)
	// a one-entry proxied spend costs about 0.0002 TAO
	if live.Validate.FeeRao < 100_000 || 1_000_000 < live.Validate.FeeRao {
		t.Fatalf("fee %d rao", live.Validate.FeeRao)
	}
}

// The chain's own simulation of a mint, as runtime 490 answered on
// 2026-11-10: SN25 GammaDisabled (not switched on), SN58 TaoUsdPriceUnavailable
// (the oracle more than 300 blocks old).
func TestGammaDecodesRecordedSimMint(t *testing.T) {
	live := gammaTestLiveRecorded(t)
	runtime := gammaTestRuntime(t, 490)
	sim, err := gammaDecodeSimMint(runtime, gammaTestHex(t, live.SimMint25.Result))
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, sim.Error, "GammaDisabled")
	sim, err = gammaDecodeSimMint(runtime, gammaTestHex(t, live.SimMint58.Result))
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, sim.Error, "TaoUsdPriceUnavailable")
	ok := []byte{0}
	ok = binary.LittleEndian.AppendUint64(ok, 1)
	ok = binary.LittleEndian.AppendUint64(ok, 2)
	ok = binary.LittleEndian.AppendUint64(ok, 217_000_000_000)
	sim, err = gammaDecodeSimMint(runtime, ok)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, *sim, gammaSimMint{Alpha: 1, Value: 2, Gamma: 217_000_000_000})
}

// The owner's ready-to-sign call: batch_all([set_service, gamma_enabled]).
// Pinned so the call data in BTCPAY.md §18 is what this runtime encodes.
func TestGammaOwnerBatchCallData(t *testing.T) {
	live := gammaTestLiveRecorded(t)
	runtime := gammaTestRuntime(t, 490)
	ownerHotkey := gammaTestAccount(t, "5CyQFykVpfa9xgZVGgBsiNUpjbo1EihBTSkPvDC1FZbL1wxG")
	spender := gammaTestAccount(t, "5EUQU938kFzJsVBjn9ch5RNNG4191j7mSz5Djf5vghrmWgUC")
	setService, err := types.NewCall(runtime.Meta, "SubtensorModule.set_service", types.U16(25), ownerHotkey, []byte("https://api.bringyour.com/pay/gamma/services.json"), spender)
	if err != nil {
		t.Fatal(err)
	}
	enable, err := types.NewCall(runtime.Meta, "AdminUtils.sudo_set_subnet_gamma_enabled", types.U16(25), types.NewBool(true))
	if err != nil {
		t.Fatal(err)
	}
	batch, err := types.NewCall(runtime.Meta, "Utility.batch_all", []types.Call{setService, enable})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := codec.Encode(batch)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, gammaHex(encoded), live.OwnerBatchAll.Call)
	connect.AssertEqual(t, gammaHex(blake2bSum256(encoded)), live.OwnerBatchAll.CallHash)
	connect.AssertEqual(t, live.OwnerBatchAll.CallHash, "0x390407699ee5f6ab6d9cfef031c145ff32415b56d4ab2ccda111ae145c4c0beb")
}

// gammaFixtureRpc answers from fixed tables: storage values by key, and
// handlers by method.
type gammaFixtureRpc struct {
	storage  map[string]string
	handlers map[string]func(params []any) (any, error)
	calls    []string
}

func (self *gammaFixtureRpc) Call(ctx context.Context, result any, method string, params ...any) error {
	self.calls = append(self.calls, method)
	var answer any
	switch {
	case self.handlers[method] != nil:
		var err error
		answer, err = self.handlers[method](params)
		if err != nil {
			return err
		}
	case method == "state_getStorage":
		key := strings.ToLower(params[0].(string))
		if value, ok := self.storage[key]; ok {
			answer = value
		}
	default:
		return fmt.Errorf("unexpected %s", method)
	}
	encoded, err := json.Marshal(answer)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, result)
}

// The recorded storage decodes through the typed reads, whose storage keys
// are the recorded keys (so the hashers are right).
func TestGammaDecodesRecordedStorage(t *testing.T) {
	recorded := gammaTestRecorded(t)
	runtime := gammaTestRuntime(t, 490)
	storage := map[string]string{}
	for _, name := range []string{"GammaEnabled", "TaoUsdPrice", "SubnetOwnerHotkey_25", "SubnetGammaEnabled_58"} {
		var value struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		if err := json.Unmarshal(recorded.Storage[name], &value); err != nil {
			t.Fatal(err)
		}
		storage[value.Key] = value.Value
	}
	for _, name := range []string{"Services", "GammaDeposits"} {
		var values map[string]string
		if err := json.Unmarshal(recorded.Storage[name], &values); err != nil {
			t.Fatal(err)
		}
		for key, value := range values {
			storage[key] = value
		}
	}
	chain := newGammaChain(&gammaFixtureRpc{storage: storage})
	ctx := context.Background()
	at := types.NewHash(make([]byte, 32))

	enabled, err := chain.Bool(ctx, runtime, at, "GammaEnabled")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, enabled, true)
	enabled, err = chain.Bool(ctx, runtime, at, "SubnetGammaEnabled", gammaNetuidArg(58))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, enabled, true)
	// SN25's flag was unset (off)
	enabled, err = chain.Bool(ctx, runtime, at, "SubnetGammaEnabled", gammaNetuidArg(25))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, enabled, false)

	price, err := chain.TaoUsdPrice(ctx, runtime, at)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, price.FedAt, uint64(9_478_200))
	connect.AssertEqual(t, gammaFormatUnits(int64(price.Accepted)), "217.808001475")

	owner, err := chain.AccountId(ctx, runtime, at, "SubnetOwnerHotkey", gammaNetuidArg(25))
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, gammaAddress(*owner), "5CyQFykVpfa9xgZVGgBsiNUpjbo1EihBTSkPvDC1FZbL1wxG")

	attune, err := chain.Service(ctx, runtime, at, 58, gammaTestAccount(t, "5DsnuvVczKJvrFphbzNnuRhiGhH48co7im1EzzvnWibWBp9S"))
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, attune.Url, "https://api.robotensor.ai/services.json")
	connect.AssertEqual(t, gammaAddress(attune.Spender), "5DCsuLtioCCfgiNQvfe3kiUBDXMs6X4y9PrEiAX7rMZ2e5E3")
	terza, err := chain.Service(ctx, runtime, at, 103, gammaTestAccount(t, "5FCSL8R69CXweUjewpoGUpDnYFcFg3Q1J4tQbNHj23SbLgUc"))
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, terza.Url, "https://terza.si")

	engy, err := chain.Deposit(ctx, runtime, at, 53, gammaTest32(t, "0xfeda65b42b11beeda0c958b3271828982230057e39628582c9e3bea44d93bf58"))
	if err != nil {
		t.Fatal(err)
	}
	if engy == nil {
		t.Fatal("no SN53 deposit")
	}
	connect.AssertEqual(t, engy.Coldkey, engy.Spender)
	connect.AssertEqual(t, gammaFormatUnits(int64(engy.Balance)), "3.6863198")
	connect.AssertEqual(t, engy.DailySpendCap == nil, true)
	connect.AssertEqual(t, engy.Spendable(9_479_410), engy.Balance)
	spent, err := chain.Deposit(ctx, runtime, at, 103, gammaTest32(t, "0xd222428f0dd79f4172882d0d7a696f4c949dbc92070d3ee585fa77f37fea1a6e"))
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, spent.Balance, uint64(0))
	connect.AssertEqual(t, gammaAddress(spent.Spender), "5CXMghXHQEDf7xgp2KHfXitDPo55yG1xLtq6jASxqjMSrHv1")
	none, err := chain.Deposit(ctx, runtime, at, 25, gammaTestAccount(t, "5CMWj1VteMAXa2Popwo7PvjoCtwEC7HYZraqaXLk1FKktqVw"))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, none == nil, true)
}

// A daily cap refills linearly over 7200 blocks, up to the cap
// (gamma.rs gamma_allowance).
func TestGammaDepositSpendableUnderACap(t *testing.T) {
	limit := uint64(10_000_000_000)
	deposit := &gammaDeposit{Balance: 50_000_000_000, DailySpendCap: &limit, BucketAvailable: 2_000_000_000, BucketBlock: 1000}
	connect.AssertEqual(t, deposit.Spendable(1000), uint64(2_000_000_000))
	connect.AssertEqual(t, deposit.Spendable(1000+3600), uint64(7_000_000_000))
	connect.AssertEqual(t, deposit.Spendable(1000+72000), limit)
	deposit.Balance = 1_000_000_000
	connect.AssertEqual(t, deposit.Spendable(1000+72000), uint64(1_000_000_000))
	// a cap of zero pauses the deposit
	zero := uint64(0)
	paused := &gammaDeposit{Balance: 5, DailySpendCap: &zero}
	connect.AssertEqual(t, paused.Spendable(1_000_000), uint64(0))
}

// gammaTestEventId is a runtime's (pallet, variant) index of an event.
func gammaTestEventId(t testing.TB, runtime *gammaRuntime, palletName string, eventName string) (byte, byte) {
	m := runtime.Meta.AsMetadataV14
	for _, pallet := range m.Pallets {
		if string(pallet.Name) != palletName {
			continue
		}
		for _, variant := range m.EfficientLookup[pallet.Events.Type.Int64()].Def.Variant.Variants {
			if string(variant.Name) == eventName {
				return byte(pallet.Index), byte(variant.Index)
			}
		}
	}
	t.Fatalf("no event %s.%s", palletName, eventName)
	return 0, 0
}

// A GammaSpendSkipped's reason is the SubtensorModule error, by name, and
// the API's code for it.
func TestGammaSkipReasonFromASkippedSpend(t *testing.T) {
	runtime := gammaTestRuntime(t, 490)
	pallet, variant := gammaTestEventId(t, runtime, "SubtensorModule", "GammaSpendSkipped")
	hotkey := gammaTestAccount(t, "5CMWj1VteMAXa2Popwo7PvjoCtwEC7HYZraqaXLk1FKktqVw")
	spender := gammaTestAccount(t, "5EUQU938kFzJsVBjn9ch5RNNG4191j7mSz5Djf5vghrmWgUC")
	var reference [32]byte
	reference[31] = 9
	record := []byte{4, 0, 3, 0, 0, 0, pallet, variant}
	record = binary.LittleEndian.AppendUint16(record, 25)
	record = append(record, hotkey[:]...)
	record = append(record, spender[:]...)
	record = binary.LittleEndian.AppendUint64(record, 40_000_000_000)
	record = append(record, reference[:]...)
	// DispatchError::Module(ModuleError { index: 7, error: [199, 0, 0, 0] })
	record = append(record, 3, 7, 199, 0, 0, 0)
	record = append(record, 0)
	events, err := gammaDecodeEvents(runtime, record)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, len(events), 1)
	connect.AssertEqual(t, events[0].ExtrinsicIndex, int64(3))
	spend, err := gammaSpendEventFrom(runtime, events[0])
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, spend.Skipped, true)
	connect.AssertEqual(t, spend.Reason, "InsufficientGammaBalance")
	connect.AssertEqual(t, spend.Reference, reference)
	connect.AssertEqual(t, gammaSkipReasonCode(spend.Reason), GammaErrorCodeInsufficientGamma)
	connect.AssertEqual(t, gammaSkipReasonCode("GammaDepositDailySpendCapExceeded"), GammaErrorCodeDailyCapExceeded)
	connect.AssertEqual(t, gammaSkipReasonCode("NotGammaDepositSpender"), GammaErrorCodeWrongSpender)
	connect.AssertEqual(t, gammaSkipReasonCode("GammaDepositDoesNotExist"), GammaErrorCodeNoDeposit)
}

// The deposit hotkeys are the master public key soft-derived with /index:
// the same addresses the Python SDK's Keypair.create_from_uri gives for
// <seed>/index (recorded from bittensor 11.3.2 for the //Alice mini-secret).
func TestGammaDepositHotkeysMatchTheSdk(t *testing.T) {
	master := gammaTest32(t, "0xd43593c715fdd31c61141abd04a99fd6822c8558854ccde39a5684e7a56da27d")
	for _, test := range []struct {
		index   int64
		public  string
		address string
	}{
		{0, "9057db4878163172ea51d570612043a98971737bf608b544991130ac110b0801", "5FKxrLQM24ZhLxcaQfJR3uMxMZh5gU6E4CP3yghPJLzCDnHN"},
		{1, "96f56ff25557d90198ffdea4a317664140e7f33a3cb4ec5edc9640104ddbbd79", "5FUdx3xPJdh2ZdD7DwPQRN2eMAFVUKpfPeoEZZiSraow9iVQ"},
		{41, "48de36f80a40a175d08caafe4e754d7bd1a9a41f12182a2d096bbaec96abd636", "5DiFKJgzUCf6hjJStn2Gq3F8bLXEJA4feQPDx8u5jg5buCBC"},
	} {
		publicKey, address, err := gammaDepositHotkeyAt(master, test.index)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, hex.EncodeToString(publicKey[:]), test.public)
		connect.AssertEqual(t, address, test.address)
	}
	if _, _, err := gammaDepositHotkeyAt(master, -1); err == nil {
		t.Fatal("a negative index derived")
	}
}

func gammaTestYaml(mutate func(*gammaYaml)) *gammaYaml {
	y := &gammaYaml{}
	y.Gamma.BillingKeySeed = gammaTestAliceSeed
	y.Gamma.BillingAddress = "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY"
	y.Gamma.DepositHotkeyMaster = "5Dw7bk4T1mcJMiWz7mNGL12XXPvUPXPaPAJ5W2aUgnexnVxy"
	y.Gamma.Spender = "5EUQU938kFzJsVBjn9ch5RNNG4191j7mSz5Djf5vghrmWgUC"
	y.Gamma.ServiceHotkey = "5CyQFykVpfa9xgZVGgBsiNUpjbo1EihBTSkPvDC1FZbL1wxG"
	y.Gamma.ServicesUrl = "https://api.bringyour.com/pay/gamma/services.json"
	if mutate != nil {
		mutate(y)
	}
	return y
}

// gamma.yml is checked; a refusal names the key, never a value.
func TestGammaSettingsParsing(t *testing.T) {
	settings, err := parseGammaSettings(gammaTestYaml(nil))
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, settings.Netuid, uint16(25))
	connect.AssertEqual(t, settings.BillingAddress, "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY")
	connect.AssertEqual(t, settings.MaxFeeRao, uint64(gammaDefaultMaxFeeRao))
	connect.AssertEqual(t, settings.EraPeriod, uint64(64))
	connect.AssertEqual(t, settings.IntentTtl, gammaDefaultIntentTtl)
	connect.AssertEqual(t, settings.canSign, true)

	// the api's copy may leave the seed out: it serves, and cannot bill
	serving, err := parseGammaSettings(gammaTestYaml(func(y *gammaYaml) {
		y.Gamma.BillingKeySeed = ""
		y.Gamma.BillingAddress = "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY"
	}))
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, serving.canSign, false)
	connect.AssertEqual(t, serving.BillingPublicKey, settings.BillingPublicKey)
	connect.AssertEqual(t, serving.DepositHotkeyMaster, settings.DepositHotkeyMaster)

	for _, test := range []struct {
		name   string
		mutate func(*gammaYaml)
		key    string
	}{
		{"no billing key or address", func(y *gammaYaml) { y.Gamma.BillingKeySeed = ""; y.Gamma.BillingAddress = "" }, "billing_key_seed"},
		{"a serving copy with a bad billing address", func(y *gammaYaml) { y.Gamma.BillingKeySeed = ""; y.Gamma.BillingAddress = "5Gx" }, "billing_address"},
		{"billing address of another key", func(y *gammaYaml) { y.Gamma.BillingAddress = "5CyQFykVpfa9xgZVGgBsiNUpjbo1EihBTSkPvDC1FZbL1wxG" }, "billing_address"},
		{"no spender", func(y *gammaYaml) { y.Gamma.Spender = "" }, "spender"},
		{"the billing key as the spender", func(y *gammaYaml) { y.Gamma.Spender = "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY" }, "spender"},
		{"a long services url", func(y *gammaYaml) { y.Gamma.ServicesUrl = "https://api.bringyour.com/" + strings.Repeat("x", 240) }, "services_url"},
		{"a plain http services url", func(y *gammaYaml) { y.Gamma.ServicesUrl = "http://api.bringyour.com/pay/gamma/services.json" }, "services_url"},
		{"no deposit master", func(y *gammaYaml) { y.Gamma.DepositHotkeyMaster = "" }, "deposit_hotkey_master"},
		{"an era that is not a power of two", func(y *gammaYaml) { y.Gamma.EraPeriod = 100 }, "era_period"},
	} {
		_, err := parseGammaSettings(gammaTestYaml(test.mutate))
		if err == nil || !strings.Contains(err.Error(), test.key) {
			t.Fatalf("%s: %v", test.name, err)
		}
		if strings.Contains(err.Error(), strings.TrimPrefix(gammaTestAliceSeed, "0x")[:16]) {
			t.Fatalf("%s: the error quotes the seed", test.name)
		}
	}
}

func TestGammaUnitsFormatAndParse(t *testing.T) {
	connect.AssertEqual(t, gammaFormatUnits(40_000_000_000), "40")
	connect.AssertEqual(t, gammaFormatUnits(500_000_000), "0.5")
	connect.AssertEqual(t, gammaFormatUnits(3_686_319_800), "3.6863198")
	connect.AssertEqual(t, gammaFormatUnits(1), "0.000000001")
	connect.AssertEqual(t, gammaFormatUnits(0), "0")
	for text, want := range map[string]int64{"40": 40_000_000_000, "0.5": 500_000_000, "": 0, "0": 0, "1.000000001": 1_000_000_001} {
		got, ok := gammaParseUnits(text)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, got, want)
	}
	for _, text := range []string{"-1", "0.0000000001", "x", "1e30"} {
		_, ok := gammaParseUnits(text)
		connect.AssertEqual(t, ok, false)
	}
}

func TestGammaIndicativeTao(t *testing.T) {
	// 217.8 gamma per TAO: 40 gamma is about 0.1836 TAO, plus 3%
	connect.AssertEqual(t, gammaIndicativeTao(40_000_000_000, 217_800_000_000), "0.189165")
	connect.AssertEqual(t, gammaIndicativeTao(40_000_000_000, 0), "")
}

// A top-up is due under the floor, or when the use seen since the last check
// would empty the balance within the next one.
func TestGammaTopupDue(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	interval := 5 * time.Minute
	gib := int64(model.Gib)
	last := now.Add(-5 * time.Minute)
	ref := func(v int64) *int64 { return &v }
	connect.AssertEqual(t, gammaTopupDue(0, nil, nil, now, interval), true)
	connect.AssertEqual(t, gammaTopupDue(int64(gammaTopupFloorBytes), nil, nil, now, interval), true)
	connect.AssertEqual(t, gammaTopupDue(10*gib, nil, nil, now, interval), false)
	// 1 GiB used in the last 5 minutes: 2 GiB left lasts through the next check
	// only at the margin of two intervals
	connect.AssertEqual(t, gammaTopupDue(2*gib, ref(3*gib), &last, now, interval), true)
	connect.AssertEqual(t, gammaTopupDue(3*gib, ref(4*gib), &last, now, interval), false)
	// a balance that grew is not due
	connect.AssertEqual(t, gammaTopupDue(5*gib, ref(1*gib), &last, now, interval), false)
}

func TestGammaSubmitAccepted(t *testing.T) {
	connect.AssertEqual(t, gammaSubmitAccepted(&gammaRpcError{Code: 1013, Message: "Transaction Already Imported"}), true)
	connect.AssertEqual(t, gammaSubmitAccepted(&gammaRpcError{Code: 1010, Message: "Invalid Transaction", Data: json.RawMessage(`"Transaction is outdated"`)}), true)
	connect.AssertEqual(t, gammaSubmitAccepted(&gammaRpcError{Code: 1010, Message: "Invalid Transaction", Data: json.RawMessage(`"Inability to pay some fees (e.g. account balance too low)"`)}), false)
	connect.AssertEqual(t, gammaSubmitAccepted(fmt.Errorf("dial tcp: refused")), false)
}

// keep big imported for the U128 path of gammaDecodedUint
var _ = big.NewInt
