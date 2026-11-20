package controller

// Hermetic tests of Pay with Gamma's chain layer against answers recorded
// from the public Finney endpoints (testdata/gamma): runtime 493's metadata
// and a real spend_gamma under it (SN51, block 9,549,512: the deposit's own
// hotkey signed it, fee-free), with the storage around it; and, from
// 2026-11-10, runtime 488's and 490's metadata (the spender model, retired by
// 493), the first mainnet spend under it, and runtime 490's live verdicts. No
// network, no database.

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

	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types/codec"
	"gopkg.in/yaml.v3"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/model"
)

// The well-known development key //Alice's mini-secret: public test data.
const gammaTestAliceSeed = "0xe5be9a5092b81bca64be81d212e7f2f9eba183bb7a90954f7b76361f6edb5c0a"

// //Alice's public key, as the deposit master of the tests.
const gammaTestAliceAddress = "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY"

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
		for _, s := range []uint32{488, 490, 493} {
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

// gammaRecorded493 is testdata/gamma/finney-493-recorded.json: a block of
// runtime 493 with a real spend_gamma, and storage at that block.
type gammaRecorded493 struct {
	Genesis string `json:"genesis"`
	Spend   struct {
		Netuid     uint16 `json:"netuid"`
		Service    string `json:"service"`
		Hotkey     string `json:"hotkey"`
		TotalAfter uint64 `json:"total_after"`
	} `json:"spend"`
	Block struct {
		Number         uint64   `json:"number"`
		Hash           string   `json:"hash"`
		ParentHash     string   `json:"parent_hash"`
		Events         string   `json:"events"`
		Extrinsics     []string `json:"extrinsics"`
		SpendExtrinsic struct {
			Index        int    `json:"index"`
			EraBirth     uint64 `json:"era_birth"`
			EraBirthHash string `json:"era_birth_hash"`
		} `json:"spend_extrinsic"`
	} `json:"block"`
	Storage map[string]struct {
		Key   string  `json:"key"`
		Value *string `json:"value"`
	} `json:"storage"`
}

func gammaTestRecorded493(t testing.TB) *gammaRecorded493 {
	raw, err := os.ReadFile("testdata/gamma/finney-493-recorded.json")
	if err != nil {
		t.Fatal(err)
	}
	recorded := &gammaRecorded493{}
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

// Runtime 493's Gamma calls, events, storage and signed extensions are the
// reviewed ones.
func TestGammaRuntime493IsSupported(t *testing.T) {
	runtime := gammaTestRuntime(t, 493)
	connect.AssertEqual(t, runtime.Supported(), nil)
	connect.AssertEqual(t, runtime.Retired(), false)
	connect.AssertEqual(t, runtime.shape.subtensorPallet, uint8(7))
	for _, name := range gammaSpendErrors {
		found := false
		for _, errorName := range runtime.shape.subtensorErrors {
			found = found || errorName == name
		}
		connect.AssertEqual(t, found, true)
	}
}

// Runtimes 488 and 490 (whose Gamma shapes 492 kept) spend through a spender
// and its GammaSpend proxy: refused, naming spend_gamma, and retired (older
// than 493), so a block they ran is read only for what kept its names.
func TestGammaSpenderRuntimesAreRetired(t *testing.T) {
	for _, spec := range []uint32{488, 490} {
		runtime := gammaTestRuntime(t, spec)
		err := runtime.Supported()
		if err == nil || !strings.Contains(err.Error(), "call SubtensorModule.spend_gamma") {
			t.Fatalf("runtime %d: %v", spec, err)
		}
		connect.AssertEqual(t, runtime.Retired(), true)
		if _, err := gammaSpendCall(runtime, [32]byte{1}, 25, gammaMinSpendUnits); err == nil {
			t.Fatalf("runtime %d built a spend", spec)
		}
	}
}

// A runtime whose GammaSpent no longer has the fields reviewed is refused,
// with the event named: the biller pauses rather than decode on a guess. A
// runtime newer than the reviewed ones is never retired.
func TestGammaRuntimeWithAChangedEventIsUnsupported(t *testing.T) {
	runtime, err := newGammaRuntime(493, 1, gammaTestMetadataHex(t, 493))
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
				variant.Fields[2].Name = "receipt"
				changed = true
			}
		}
	}
	connect.AssertEqual(t, changed, true)
	_, err = gammaCheckShape(runtime.Meta)
	if err == nil || !strings.Contains(err.Error(), "SubtensorModule.GammaSpent") {
		t.Fatalf("a changed GammaSpent was admitted: %v", err)
	}

	newer, err := newGammaRuntime(999, 1, gammaSimChangedMetadataHex(t, gammaTestMetadataHex(t, 493)))
	if err != nil {
		t.Fatal(err)
	}
	if newer.Supported() == nil {
		t.Fatal("a changed runtime is supported")
	}
	connect.AssertEqual(t, newer.Retired(), false)
}

// A real spend_gamma under runtime 493 (SN51, block 9,549,512, extrinsic 2):
// its GammaSpent decodes field by field; the extrinsic is signed by the
// deposit's own hotkey; our encoding of the call is its call, byte for byte;
// and its signature verifies over the payload this code signs.
func TestGammaDecodesTheRecorded493Spend(t *testing.T) {
	recorded := gammaTestRecorded493(t)
	runtime := gammaTestRuntime(t, 493)
	hotkey := gammaTest32(t, recorded.Spend.Hotkey)
	service := gammaTest32(t, recorded.Spend.Service)

	events, err := gammaDecodeEvents(runtime, gammaTestHex(t, recorded.Block.Events))
	if err != nil {
		t.Fatal(err)
	}
	var spends []*gammaSpendEvent
	for _, event := range events {
		if event.Name == "SubtensorModule.GammaSpent" {
			spend, err := gammaSpendEventFrom(event)
			if err != nil {
				t.Fatal(err)
			}
			spends = append(spends, spend)
		}
	}
	connect.AssertEqual(t, len(spends), 1)
	spend := spends[0]
	connect.AssertEqual(t, spend.Netuid, recorded.Spend.Netuid)
	connect.AssertEqual(t, spend.Netuid, uint16(51))
	connect.AssertEqual(t, spend.Hotkey, hotkey)
	connect.AssertEqual(t, spend.Service, service)
	connect.AssertEqual(t, spend.Amount, uint64(1_000_000_000))
	connect.AssertEqual(t, spend.Event.ExtrinsicIndex, int64(recorded.Block.SpendExtrinsic.Index))

	extrinsic := gammaTestHex(t, recorded.Block.Extrinsics[recorded.Block.SpendExtrinsic.Index])
	call, err := gammaSpendCall(runtime, service, spend.Netuid, spend.Amount)
	if err != nil {
		t.Fatal(err)
	}
	encodedCall, err := codec.Encode(call)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(extrinsic, encodedCall) {
		t.Fatalf("our call %x is not the recorded extrinsic's %x", encodedCall, extrinsic)
	}

	genesis := types.NewHash(gammaTestHex(t, recorded.Genesis))
	birth := types.NewHash(gammaTestHex(t, recorded.Block.SpendExtrinsic.EraBirthHash))
	signer, sig, payload := gammaTestSignedPayload(t, extrinsic, 493, 1, genesis, birth)
	// signed by the deposit's hotkey, not a spender or a proxy
	connect.AssertEqual(t, signer, hotkey)
	ok, err := model.VerifyBittensorSignature(gammaAddress(signer), string(payload), hex.EncodeToString(sig))
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, ok, true)
}

// Our spend is SubtensorModule.spend_gamma(service, netuid, amount) in
// runtime 493's indices: pallet 7, call 168.
func TestGammaSpendCallEncoding(t *testing.T) {
	runtime := gammaTestRuntime(t, 493)
	service := gammaTestAccount(t, "5CyQFykVpfa9xgZVGgBsiNUpjbo1EihBTSkPvDC1FZbL1wxG")
	call, err := gammaSpendCall(runtime, service, 25, 40_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := codec.Encode(call)
	if err != nil {
		t.Fatal(err)
	}
	amount := binary.LittleEndian.AppendUint64(nil, 40_000_000_000)
	connect.AssertEqual(t, hex.EncodeToString(encoded), "07a8"+hex.EncodeToString(service[:])+"1900"+hex.EncodeToString(amount))

	if _, err := gammaSpendCall(runtime, service, 25, 0); err == nil {
		t.Fatal("a spend of nothing was built")
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
// payload a signed extrinsic's signature covers under runtime 488 to 493's
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

// A spend we sign as a deposit hotkey (the key of <seed>/index) verifies over
// the payload Substrate defines, with that hotkey's key, in the substrate
// signing context; and it is the same extrinsic shape as the recorded one.
func TestGammaSignedSpendVerifies(t *testing.T) {
	runtime := gammaTestRuntime(t, 493)
	settings, err := parseGammaSettings(gammaTestYaml(nil))
	if err != nil {
		t.Fatal(err)
	}
	hotkey, address, err := gammaDepositHotkeyAt(settings.DepositHotkeyMaster, 3)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gammaDepositSigner(settings, 3, hotkey)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, signer.Address, address)
	service := gammaTestAccount(t, "5CyQFykVpfa9xgZVGgBsiNUpjbo1EihBTSkPvDC1FZbL1wxG")
	call, err := gammaSpendCall(runtime, service, 25, 5_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	genesis := types.NewHash(gammaTestHex(t, "0x2f0555cc76fc2840a25a6ea3b9637146806f1f44b090c175ffde2a7e5ab36c03"))
	birth := types.NewHash(blake2bSum256([]byte("birth")))
	signed, err := gammaSignSpend(runtime, genesis, 9_549_600, birth, 64, 0, call, signer)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, signed.Hash, gammaExtrinsicHash(signed.Encoded))
	publicKey, sig, payload := gammaTestSignedPayload(t, signed.Encoded, 493, 1, genesis, birth)
	connect.AssertEqual(t, publicKey, hotkey)
	ok, err := model.VerifyBittensorSignature(address, string(payload), hex.EncodeToString(sig))
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, ok, true)
	encodedCall, err := codec.Encode(call)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(signed.Encoded, encodedCall) {
		t.Fatal("the signed spend does not end with its call")
	}

	// another birth block is another payload: the signature does not carry over
	_, _, otherPayload := gammaTestSignedPayload(t, signed.Encoded, 493, 1, genesis, types.NewHash(blake2bSum256([]byte("other"))))
	ok, _ = model.VerifyBittensorSignature(address, string(otherPayload), hex.EncodeToString(sig))
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
// verifies over the payload rebuilt here, and the verdict decodes as the
// biller reads it.
func TestGammaRuntime490AcceptedOurSignedSpend(t *testing.T) {
	live := gammaTestLiveRecorded(t)
	connect.AssertEqual(t, live.SpecVersion, uint32(490))
	// Err(TransactionValidityError::Invalid(InvalidTransaction::Payment))
	connect.AssertEqual(t, live.Validate.Verdict, "0x010001")
	validity, err := gammaDecodeValidity(gammaTestHex(t, live.Validate.Verdict))
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, *validity, gammaValidity{Verdict: "invalid/payment"})
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
}

// The chain's verdicts, as the biller reads them: valid; a custom error (the
// transaction extension's checks of the deposit, amount and service, which
// refuse the spend itself); a used nonce; and the rest, which say nothing
// about the deposit.
func TestGammaValidityVerdicts(t *testing.T) {
	valid := []byte{0}
	valid = binary.LittleEndian.AppendUint64(valid, 1)
	valid = append(valid, 0, 4, 2, 0xaa, 0xbb)
	valid = binary.LittleEndian.AppendUint64(valid, 64)
	valid = append(valid, 1)
	for _, test := range []struct {
		raw       []byte
		valid     bool
		verdict   string
		refused   bool
		nonceUsed bool
	}{
		{valid, true, "", false, false},
		// Err(Invalid(Custom(255))): CustomTransactionError::BadRequest
		{[]byte{1, 0, 7, 255}, false, "invalid/custom/255", true, false},
		{[]byte{1, 0, 3}, false, "invalid/stale", false, true},
		{[]byte{1, 0, 2}, false, "invalid/future", false, false},
		{[]byte{1, 0, 4}, false, "invalid/bad_proof", false, false},
		{[]byte{1, 1, 0}, false, "unknown/cannot_lookup", false, false},
	} {
		validity, err := gammaDecodeValidity(test.raw)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, validity.Valid, test.valid)
		connect.AssertEqual(t, validity.Verdict, test.verdict)
		connect.AssertEqual(t, validity.Refused(), test.refused)
		connect.AssertEqual(t, validity.NonceUsed(), test.nonceUsed)
	}
	for _, raw := range [][]byte{{}, {2}, {1, 2, 0}, {1, 0}} {
		if _, err := gammaDecodeValidity(raw); err == nil {
			t.Fatalf("%x decoded", raw)
		}
	}
}

// The chain's own simulation of a mint, as runtime 490 answered on
// 2026-11-10: SN25 GammaDisabled (not switched on), SN58 TaoUsdPriceUnavailable
// (the oracle more than 300 blocks old). Runtime 493 names the same errors at
// the same indices, so the answers read the same under it.
func TestGammaDecodesRecordedSimMint(t *testing.T) {
	live := gammaTestLiveRecorded(t)
	runtime := gammaTestRuntime(t, 493)
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

// The owner's call of 2026-11-11 under runtime 490: batch_all([set_service,
// gamma_enabled]). Pinned so the call data in BTCPAY.md §18 is what that
// runtime encoded; runtime 493's migration kept the service's url and dropped
// its spender, so the service needs no new call.
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

// The storage recorded at the block of the real 493 spend decodes through the
// typed reads, whose storage keys are the recorded keys (so the hashers are
// right, the GammaSpent N-map's included): our service is published with its
// url only, the spender's deposit names no spender, the hotkey's GammaSpent
// total is what it spent, and the fee-free signer holds its nonce.
func TestGammaDecodesRecorded493Storage(t *testing.T) {
	recorded := gammaTestRecorded493(t)
	runtime := gammaTestRuntime(t, 493)
	storage := map[string]string{}
	for _, value := range recorded.Storage {
		if value.Value != nil {
			storage[value.Key] = *value.Value
		}
	}
	chain := newGammaChain(&gammaFixtureRpc{storage: storage})
	ctx := context.Background()
	at := types.NewHash(gammaTestHex(t, recorded.Block.Hash))

	enabled, err := chain.Bool(ctx, runtime, at, "GammaEnabled")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, enabled, true)
	enabled, err = chain.Bool(ctx, runtime, at, "SubnetGammaEnabled", gammaNetuidArg(25))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, enabled, true)

	price, err := chain.TaoUsdPrice(ctx, runtime, at)
	if err != nil {
		t.Fatal(err)
	}
	if price.FedAt == 0 || recorded.Block.Number < price.FedAt || 0 == price.Accepted {
		t.Fatalf("price %+v at block %d", price, recorded.Block.Number)
	}

	owner, err := chain.AccountId(ctx, runtime, at, "SubnetOwnerHotkey", gammaNetuidArg(25))
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, gammaAddress(*owner), "5CyQFykVpfa9xgZVGgBsiNUpjbo1EihBTSkPvDC1FZbL1wxG")
	ours, err := chain.Service(ctx, runtime, at, 25, *owner)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, ours.Url, "https://api.bringyour.com/pay/gamma/services.json")

	hotkey := gammaTest32(t, recorded.Spend.Hotkey)
	service := gammaTest32(t, recorded.Spend.Service)
	theirs, err := chain.Service(ctx, runtime, at, recorded.Spend.Netuid, service)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(theirs.Url, "https://") {
		t.Fatalf("service url %q", theirs.Url)
	}
	deposit, err := chain.Deposit(ctx, runtime, at, recorded.Spend.Netuid, hotkey)
	if err != nil {
		t.Fatal(err)
	}
	if deposit == nil || deposit.Coldkey == hotkey {
		t.Fatalf("deposit %+v", deposit)
	}
	connect.AssertEqual(t, deposit.DailySpendCap == nil, true)
	connect.AssertEqual(t, deposit.Spendable(recorded.Block.Number), deposit.Balance)

	spent, err := chain.Spent(ctx, runtime, at, recorded.Spend.Netuid, service, hotkey)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, spent, recorded.Spend.TotalAfter)
	connect.AssertEqual(t, spent, uint64(1_000_000_000))
	// absent: nothing spent
	none, err := chain.Spent(ctx, runtime, at, 25, *owner, hotkey)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, none, uint64(0))

	// the fee-free spend held its nonce: nonce 1, no TAO
	account, err := chain.Account(ctx, runtime, at, hotkey)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, account.Nonce, uint32(1))
	connect.AssertEqual(t, account.Free, uint64(0))
}

// The spender model's storage values (runtime 490, recorded 2026-11-10) do
// not decode as runtime 493's: a deposit or a service with a spender is
// refused, never read with its fields shifted.
func TestGammaRefusesSpenderModelStorage(t *testing.T) {
	recorded := gammaTestRecorded(t)
	for _, name := range []string{"Services", "GammaDeposits"} {
		var values map[string]string
		if err := json.Unmarshal(recorded.Storage[name], &values); err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, 0 < len(values), true)
		for _, value := range values {
			raw := gammaTestHex(t, value)
			var err error
			if name == "Services" {
				_, err = gammaDecodeService(raw)
			} else {
				_, err = gammaDecodeDeposit(raw)
			}
			if err == nil {
				t.Fatalf("a %s value of the spender model decoded: %s", name, value)
			}
		}
	}
}

// What one spend can take: the balance within what a daily cap allows (it
// refills linearly over 7200 blocks, up to the cap), or nothing when that is
// below 0.1 gamma and short of the balance (gamma_info.rs).
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
	// under 0.1 gamma allowed and more held: nothing can be spent yet
	nearlyEmpty := &gammaDeposit{Balance: 5_000_000_000, DailySpendCap: &limit, BucketAvailable: 50_000_000, BucketBlock: 1000}
	connect.AssertEqual(t, nearlyEmpty.Spendable(1000), uint64(0))
	// under 0.1 gamma held: all of it
	dust := &gammaDeposit{Balance: 50_000_000}
	connect.AssertEqual(t, dust.Spendable(1000), uint64(50_000_000))
}

// gammaSpendRefusal mirrors gamma.rs gamma_deposit_after_spend, as the API's
// codes: the biller skips a charge the chain would refuse, before it signs.
func TestGammaSpendRefusalMirrorsTheChain(t *testing.T) {
	limit := uint64(10_000_000_000)
	zero := uint64(0)
	price := uint64(5_000_000_000)
	for _, test := range []struct {
		name    string
		deposit *gammaDeposit
		amount  uint64
		now     uint64
		want    string
	}{
		{"no deposit", nil, price, 100, GammaErrorCodeNoDeposit},
		{"enough", &gammaDeposit{Balance: 6_000_000_000}, price, 100, ""},
		{"the whole balance", &gammaDeposit{Balance: price}, price, 100, ""},
		{"too little", &gammaDeposit{Balance: 4_000_000_000}, price, 100, GammaErrorCodeInsufficientGamma},
		{"under 0.1 gamma, leaving a balance", &gammaDeposit{Balance: 6_000_000_000}, 50_000_000, 100, GammaErrorCodeInvalidAmount},
		{"under 0.1 gamma, the whole balance", &gammaDeposit{Balance: 50_000_000}, 50_000_000, 100, ""},
		{"nothing", &gammaDeposit{Balance: 6_000_000_000}, 0, 100, GammaErrorCodeInvalidAmount},
		{"a paused deposit", &gammaDeposit{Balance: 6_000_000_000, DailySpendCap: &zero}, price, 100, GammaErrorCodeDailyCapTooLow},
		{"a cap below the price", &gammaDeposit{Balance: 60_000_000_000, DailySpendCap: &limit}, 20_000_000_000, 100, GammaErrorCodeDailyCapTooLow},
		{"a cap not refilled yet", &gammaDeposit{Balance: 60_000_000_000, DailySpendCap: &limit, BucketAvailable: 1_000_000_000, BucketBlock: 100}, price, 100, GammaErrorCodeDailyCapExceeded},
		{"a cap refilled", &gammaDeposit{Balance: 60_000_000_000, DailySpendCap: &limit, BucketAvailable: 1_000_000_000, BucketBlock: 100}, price, 100 + 7200, ""},
	} {
		if got := gammaSpendRefusal(test.deposit, test.amount, test.now); got != test.want {
			t.Fatalf("%s: %q, want %q", test.name, got, test.want)
		}
	}
}

// A GammaSpent record of runtime 493, as the chain encodes it, decodes to its
// hotkey, service and amount, with the extrinsic that emitted it.
func TestGammaDecodesA493SpendRecord(t *testing.T) {
	runtime := gammaTestRuntime(t, 493)
	pallet, variant := gammaTestEventId(t, runtime, "SubtensorModule", "GammaSpent")
	hotkey := gammaTestAccount(t, "5CMWj1VteMAXa2Popwo7PvjoCtwEC7HYZraqaXLk1FKktqVw")
	service := gammaTestAccount(t, "5CyQFykVpfa9xgZVGgBsiNUpjbo1EihBTSkPvDC1FZbL1wxG")
	record := []byte{4, 0, 3, 0, 0, 0, pallet, variant}
	record = binary.LittleEndian.AppendUint16(record, 25)
	record = append(record, hotkey[:]...)
	record = append(record, service[:]...)
	record = binary.LittleEndian.AppendUint64(record, 40_000_000_000)
	record = append(record, 0)
	events, err := gammaDecodeEvents(runtime, record)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, len(events), 1)
	spend, err := gammaSpendEventFrom(events[0])
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, *spend, gammaSpendEvent{Event: events[0], Netuid: 25, Hotkey: hotkey, Service: service, Amount: 40_000_000_000})
	connect.AssertEqual(t, spend.Event.ExtrinsicIndex, int64(3))
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

var gammaTestSdkHotkeys = []struct {
	index   int64
	public  string
	address string
}{
	{0, "9057db4878163172ea51d570612043a98971737bf608b544991130ac110b0801", "5FKxrLQM24ZhLxcaQfJR3uMxMZh5gU6E4CP3yghPJLzCDnHN"},
	{1, "96f56ff25557d90198ffdea4a317664140e7f33a3cb4ec5edc9640104ddbbd79", "5FUdx3xPJdh2ZdD7DwPQRN2eMAFVUKpfPeoEZZiSraow9iVQ"},
	{41, "48de36f80a40a175d08caafe4e754d7bd1a9a41f12182a2d096bbaec96abd636", "5DiFKJgzUCf6hjJStn2Gq3F8bLXEJA4feQPDx8u5jg5buCBC"},
}

// The deposit hotkeys are the master public key soft-derived with /index:
// the same addresses the Python SDK's Keypair.create_from_uri gives for
// <seed>/index (recorded from bittensor 11.3.2 for the //Alice mini-secret).
func TestGammaDepositHotkeysMatchTheSdk(t *testing.T) {
	master := gammaTest32(t, "0xd43593c715fdd31c61141abd04a99fd6822c8558854ccde39a5684e7a56da27d")
	for _, test := range gammaTestSdkHotkeys {
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

// Each deposit hotkey's key is the seed's key at /index: the very hotkeys the
// SDK derives (and the api assigns from the public key alone). A hotkey the
// seed does not derive at its index, or a copy without the seed, signs
// nothing; and the error never quotes the seed.
func TestGammaDepositSignerDerivesEachHotkeysKey(t *testing.T) {
	settings, err := parseGammaSettings(gammaTestYaml(nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range gammaTestSdkHotkeys {
		hotkey, _, err := gammaDepositHotkeyAt(settings.DepositHotkeyMaster, test.index)
		if err != nil {
			t.Fatal(err)
		}
		signer, err := gammaDepositSigner(settings, test.index, hotkey)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, signer.Address, test.address)
		connect.AssertEqual(t, hex.EncodeToString(signer.PublicKey), test.public)
	}
	first, _, _ := gammaDepositHotkeyAt(settings.DepositHotkeyMaster, 0)
	// the hotkey of another index
	_, err = gammaDepositSigner(settings, 1, first)
	connect.AssertEqual(t, err, errGammaNotSignable)
	// a hotkey of another master
	other, _, _ := gammaDepositHotkeyAt(gammaTestAccount(t, "5CyQFykVpfa9xgZVGgBsiNUpjbo1EihBTSkPvDC1FZbL1wxG"), 0)
	_, err = gammaDepositSigner(settings, 0, other)
	connect.AssertEqual(t, err, errGammaNotSignable)
	connect.AssertEqual(t, gammaDepositHotkeyCurrent(settings, &model.GammaDepositHotkey{Hotkey: gammaAddress(first), DerivationIndex: 0}), true)
	connect.AssertEqual(t, gammaDepositHotkeyCurrent(settings, &model.GammaDepositHotkey{Hotkey: gammaAddress(other), DerivationIndex: 0}), false)
	// a serving copy
	serving, err := parseGammaSettings(gammaTestYaml(func(y *gammaYaml) { y.Gamma.DepositHotkeySeed = "" }))
	if err != nil {
		t.Fatal(err)
	}
	_, err = gammaDepositSigner(serving, 0, first)
	connect.AssertEqual(t, err, errGammaNotSignable)
	if strings.Contains(errGammaNotSignable.Error(), strings.TrimPrefix(gammaTestAliceSeed, "0x")[:16]) {
		t.Fatal("the error quotes the seed")
	}
}

func gammaTestYaml(mutate func(*gammaYaml)) *gammaYaml {
	y := &gammaYaml{}
	y.Gamma.DepositHotkeySeed = gammaTestAliceSeed
	y.Gamma.DepositHotkeyMaster = gammaTestAliceAddress
	y.Gamma.ServiceHotkey = "5CyQFykVpfa9xgZVGgBsiNUpjbo1EihBTSkPvDC1FZbL1wxG"
	y.Gamma.ServicesUrl = "https://api.bringyour.com/pay/gamma/services.json"
	if mutate != nil {
		mutate(y)
	}
	return y
}

// gamma.yml is checked; a refusal names the key, never a value. The keys of
// runtime 492's spender model are no longer read.
func TestGammaSettingsParsing(t *testing.T) {
	settings, err := parseGammaSettings(gammaTestYaml(nil))
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, settings.Netuid, uint16(25))
	connect.AssertEqual(t, settings.EraPeriod, uint64(64))
	connect.AssertEqual(t, settings.IntentTtl, gammaDefaultIntentTtl)
	connect.AssertEqual(t, settings.canSign, true)
	connect.AssertEqual(t, gammaAddress(settings.DepositHotkeyMaster), gammaTestAliceAddress)

	// the api's copy leaves the seed out: it serves, and cannot bill
	serving, err := parseGammaSettings(gammaTestYaml(func(y *gammaYaml) { y.Gamma.DepositHotkeySeed = "" }))
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, serving.canSign, false)
	connect.AssertEqual(t, serving.depositSeed, "")
	connect.AssertEqual(t, serving.DepositHotkeyMaster, settings.DepositHotkeyMaster)

	// a vault file still holding the spender model's keys parses: they are
	// not read
	var y gammaYaml
	text := "gamma:\n" +
		"  billing_key_seed: \"" + gammaTestAliceSeed + "\"\n" +
		"  billing_address: 5EUQU938kFzJsVBjn9ch5RNNG4191j7mSz5Djf5vghrmWgUC\n" +
		"  spender: 5EUQU938kFzJsVBjn9ch5RNNG4191j7mSz5Djf5vghrmWgUC\n" +
		"  max_fee_rao: 5000000\n" +
		"  min_float_rao: 1000000\n" +
		"  deposit_hotkey_master: " + gammaTestAliceAddress + "\n" +
		"  service_hotkey: 5CyQFykVpfa9xgZVGgBsiNUpjbo1EihBTSkPvDC1FZbL1wxG\n" +
		"  services_url: https://api.bringyour.com/pay/gamma/services.json\n"
	if err := yaml.Unmarshal([]byte(text), &y); err != nil {
		t.Fatal(err)
	}
	legacy, err := parseGammaSettings(&y)
	if err != nil {
		t.Fatal(err)
	}
	// without deposit_hotkey_seed, it does not bill
	connect.AssertEqual(t, legacy.canSign, false)

	for _, test := range []struct {
		name   string
		mutate func(*gammaYaml)
		key    string
	}{
		{"no deposit master", func(y *gammaYaml) { y.Gamma.DepositHotkeyMaster = "" }, "deposit_hotkey_master"},
		{"a bad deposit master", func(y *gammaYaml) { y.Gamma.DepositHotkeyMaster = "5Gx" }, "deposit_hotkey_master"},
		{"a seed of another master", func(y *gammaYaml) { y.Gamma.DepositHotkeyMaster = "5CyQFykVpfa9xgZVGgBsiNUpjbo1EihBTSkPvDC1FZbL1wxG" }, "deposit_hotkey_seed"},
		{"a seed with a password", func(y *gammaYaml) { y.Gamma.DepositHotkeySeed = gammaTestAliceSeed + "///secret" }, "deposit_hotkey_seed"},
		{"a seed that is not one", func(y *gammaYaml) { y.Gamma.DepositHotkeySeed = "0x1234" }, "deposit_hotkey_seed"},
		{"no service hotkey", func(y *gammaYaml) { y.Gamma.ServiceHotkey = "" }, "service_hotkey"},
		{"the master as the service", func(y *gammaYaml) { y.Gamma.ServiceHotkey = gammaTestAliceAddress }, "service_hotkey"},
		{"a long services url", func(y *gammaYaml) { y.Gamma.ServicesUrl = "https://api.bringyour.com/" + strings.Repeat("x", 240) }, "services_url"},
		{"a plain http services url", func(y *gammaYaml) { y.Gamma.ServicesUrl = "http://api.bringyour.com/pay/gamma/services.json" }, "services_url"},
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
