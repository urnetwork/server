package controller

// A simulated Subtensor chain for the Gamma biller's database tests: runtime
// 493's recorded metadata, a finalized head the test moves, storage the test
// sets (deposits, the service), and blocks the test mines. A spend is
// spend_gamma(service, netuid, amount) signed by the deposit's own hotkey.
// The chain judges a spend as runtime 493 does (validate_transaction, and a
// submit): its signature against the hotkey, its nonce against the hotkey's,
// and the rules of gamma.rs gamma_deposit_after_spend; a spend that would
// fail never enters a block. A mined block includes what was submitted and is
// still valid, with the GammaSpent runtime 493 emits, and adds it to the
// hotkey's GammaSpent total.

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types/codec"

	"github.com/urnetwork/server/model"
)

const gammaSimBase = 1000

// Finney's genesis hash, which the biller signs over
const gammaSimGenesis = "0x2f0555cc76fc2840a25a6ea3b9637146806f1f44b090c175ffde2a7e5ab36c03"

type gammaSimBlock struct {
	number     uint64
	extrinsics [][]byte
	events     []byte
}

type gammaSimDeposit struct {
	coldkey [32]byte
	balance uint64
	cap     *uint64
}

// gammaSimSpend is a submitted spend, as the chain reads it.
type gammaSimSpend struct {
	signer  [32]byte
	nonce   uint64
	service [32]byte
	netuid  uint16
	amount  uint64
}

type gammaSimChain struct {
	t           testing.TB
	runtime     *gammaRuntime
	metadataHex string

	lock      sync.Mutex
	head      uint64
	blocks    map[uint64]*gammaSimBlock
	submitted [][]byte
	// the extrinsics the chain drops instead of including (lost from the pool)
	drop bool

	netuid        uint16
	serviceHotkey [32]byte
	servicesUrl   string
	serviceOk     bool
	subnetEnabled bool
	priceAge      uint64
	deposits      map[[32]byte]*gammaSimDeposit
	// each hotkey's nonce
	nonces map[[32]byte]uint32
	// hotkeys whose pool holds a transaction ahead of their finalized nonce
	poolAhead map[[32]byte]bool
	// GammaSpent: service then hotkey, to the total spent
	spent map[[64]byte]uint64
	// the chain refuses every spend it judges, for a rule the biller does not
	// mirror
	refuse bool
	// a block whose parent state runs an unreviewed runtime
	unsupportedParent map[uint64]bool
	// a block whose parent state runs a retired runtime (490, the spender model)
	retiredParent map[uint64]bool
	// extra events the next mined block carries
	extraEvents [][]byte

	hashes map[string]uint64
	hashed uint64
}

func newGammaSimChain(t testing.TB, settings *gammaSettings) *gammaSimChain {
	return &gammaSimChain{
		t:                 t,
		runtime:           gammaTestRuntime(t, 493),
		metadataHex:       gammaTestMetadataHex(t, 493),
		head:              gammaSimBase,
		blocks:            map[uint64]*gammaSimBlock{},
		netuid:            settings.Netuid,
		serviceHotkey:     settings.ServiceHotkey,
		servicesUrl:       settings.ServicesUrl,
		serviceOk:         true,
		subnetEnabled:     true,
		priceAge:          10,
		deposits:          map[[32]byte]*gammaSimDeposit{},
		nonces:            map[[32]byte]uint32{},
		poolAhead:         map[[32]byte]bool{},
		spent:             map[[64]byte]uint64{},
		unsupportedParent: map[uint64]bool{},
		retiredParent:     map[uint64]bool{},
	}
}

func gammaSimHash(number uint64) types.Hash {
	return types.NewHash(blake2bSum256([]byte(fmt.Sprintf("block %d", number))))
}

func (self *gammaSimChain) numberOf(hashText string) (uint64, bool) {
	if self.hashes == nil {
		self.hashes = map[string]uint64{}
	}
	for ; self.hashed <= self.head+1; self.hashed++ {
		self.hashes[gammaSimHash(self.hashed).Hex()] = self.hashed
	}
	number, ok := self.hashes[hashText]
	return number, ok
}

func (self *gammaSimChain) key(pallet string, item string, args ...[]byte) string {
	key, err := types.CreateStorageKey(self.runtime.Meta, pallet, item, args...)
	if err != nil {
		self.t.Fatal(err)
	}
	return key.Hex()
}

func (self *gammaSimChain) setDeposit(hotkey [32]byte, deposit *gammaSimDeposit) {
	self.lock.Lock()
	defer self.lock.Unlock()
	if deposit == nil {
		delete(self.deposits, hotkey)
	} else {
		self.deposits[hotkey] = deposit
	}
}

func (self *gammaSimChain) deposit(hotkey [32]byte) *gammaSimDeposit {
	self.lock.Lock()
	defer self.lock.Unlock()
	return self.deposits[hotkey]
}

// setNonce sets a hotkey's nonce, as if something else had signed with it.
func (self *gammaSimChain) setNonce(hotkey [32]byte, nonce uint32) {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.nonces[hotkey] = nonce
}

// spentTotal is a hotkey's GammaSpent total at a service.
func (self *gammaSimChain) spentTotal(service [32]byte, hotkey [32]byte) uint64 {
	self.lock.Lock()
	defer self.lock.Unlock()
	var key [64]byte
	copy(key[:32], service[:])
	copy(key[32:], hotkey[:])
	return self.spent[key]
}

// storageValue is the current state's value of a key ("" absent).
func (self *gammaSimChain) storageValue(keyText string) string {
	u16 := func(v uint16) []byte { return []byte{byte(v), byte(v >> 8)} }
	switch keyText {
	case self.key("SubtensorModule", "GammaEnabled"):
		return "0x01"
	case self.key("SubtensorModule", "SubnetGammaEnabled", u16(self.netuid)):
		if self.subnetEnabled {
			return "0x01"
		}
		return ""
	case self.key("SubtensorModule", "TaoUsdPrice"):
		b := binary.LittleEndian.AppendUint64(nil, 217_808_001_475)
		b = binary.LittleEndian.AppendUint64(b, 217_808_001_475)
		b = binary.LittleEndian.AppendUint64(b, self.head-min(self.head, self.priceAge))
		return gammaHex(b)
	case self.key("SubtensorModule", "Services", u16(self.netuid), self.serviceHotkey[:]):
		if !self.serviceOk {
			return ""
		}
		b, err := codec.Encode([]byte(self.servicesUrl))
		if err != nil {
			self.t.Fatal(err)
		}
		return gammaHex(b)
	}
	for hotkey, deposit := range self.deposits {
		if keyText == self.key("SubtensorModule", "GammaDeposits", u16(self.netuid), hotkey[:]) {
			b := append([]byte{}, deposit.coldkey[:]...)
			b = binary.LittleEndian.AppendUint64(b, deposit.balance)
			if deposit.cap == nil {
				b = append(b, 0)
			} else {
				b = append(b, 1)
				b = binary.LittleEndian.AppendUint64(b, *deposit.cap)
				b = binary.LittleEndian.AppendUint64(b, *deposit.cap)
				b = binary.LittleEndian.AppendUint64(b, self.head)
			}
			return gammaHex(b)
		}
	}
	for key, total := range self.spent {
		if keyText == self.key("SubtensorModule", "GammaSpent", u16(self.netuid), key[:32], key[32:]) {
			return gammaHex(binary.LittleEndian.AppendUint64(nil, total))
		}
	}
	for hotkey, nonce := range self.nonces {
		if keyText == self.key("System", "Account", hotkey[:]) {
			b := binary.LittleEndian.AppendUint32(nil, nonce)
			// consumers, providers 0; a fee-free signer holds a sufficient
			b = binary.LittleEndian.AppendUint32(b, 0)
			b = binary.LittleEndian.AppendUint32(b, 0)
			b = binary.LittleEndian.AppendUint32(b, 1)
			b = append(b, make([]byte, 24+16)...)
			return gammaHex(b)
		}
	}
	return ""
}

func (self *gammaSimChain) Call(ctx context.Context, result any, method string, params ...any) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	param := func(i int) string {
		if len(params) <= i {
			return ""
		}
		return fmt.Sprint(params[i])
	}
	var answer any
	switch method {
	case "chain_getFinalizedHead":
		answer = gammaSimHash(self.head).Hex()
	case "chain_getHeader":
		number, ok := self.numberOf(param(0))
		if !ok {
			return fmt.Errorf("no header %s", param(0))
		}
		answer = map[string]string{"number": fmt.Sprintf("0x%x", number), "parentHash": gammaSimHash(number - 1).Hex()}
	case "chain_getBlockHash":
		var number uint64
		fmt.Sscan(param(0), &number)
		if number == 0 {
			answer = gammaSimGenesis
		} else if number <= self.head {
			answer = gammaSimHash(number).Hex()
		}
	case "state_getRuntimeVersion":
		number, _ := self.numberOf(param(0))
		spec := 493
		if self.unsupportedParent[number+1] {
			spec = 999
		} else if self.retiredParent[number+1] {
			spec = 490
		}
		answer = map[string]any{"specVersion": spec, "transactionVersion": 1}
	case "state_getMetadata":
		number, _ := self.numberOf(param(0))
		if self.unsupportedParent[number+1] {
			answer = gammaSimChangedMetadataHex(self.t, self.metadataHex)
		} else if self.retiredParent[number+1] {
			answer = gammaTestMetadataHex(self.t, 490)
		} else {
			answer = self.metadataHex
		}
	case "state_getStorage":
		keyText := strings.ToLower(param(0))
		if keyText == self.key("System", "Events") {
			number, _ := self.numberOf(param(1))
			if block := self.blocks[number]; block != nil && 0 < len(block.events) {
				answer = gammaHex(block.events)
			} else {
				answer = "0x00"
			}
			break
		}
		if value := self.storageValue(keyText); value != "" {
			answer = value
		}
	case "chain_getBlock":
		number, _ := self.numberOf(param(0))
		extrinsics := []string{}
		if block := self.blocks[number]; block != nil {
			for _, extrinsic := range block.extrinsics {
				extrinsics = append(extrinsics, gammaHex(extrinsic))
			}
		}
		answer = map[string]any{"block": map[string]any{"extrinsics": extrinsics}}
	case "state_call":
		switch param(0) {
		case "GammaRuntimeApi_sim_mint_gamma":
			b := []byte{0}
			b = binary.LittleEndian.AppendUint64(b, 1)
			b = binary.LittleEndian.AppendUint64(b, 1_000_000_000)
			b = binary.LittleEndian.AppendUint64(b, 217_000_000_000)
			answer = gammaHex(b)
		case "TaggedTransactionQueue_validate_transaction":
			args, err := codec.HexDecodeString(param(1))
			if err != nil {
				return err
			}
			if len(args) < 1+32 || args[0] != 2 {
				return fmt.Errorf("validate_transaction args %x", args)
			}
			answer = gammaHex(self.judge(args[1 : len(args)-32]))
		default:
			return fmt.Errorf("no runtime api %s", param(0))
		}
	case "author_submitExtrinsic":
		encoded, err := codec.HexDecodeString(param(0))
		if err != nil {
			return err
		}
		for _, submitted := range self.submitted {
			if string(submitted) == string(encoded) {
				return &gammaRpcError{Code: 1013, Message: "Transaction Already Imported"}
			}
		}
		if verdict := self.judge(encoded); verdict[0] != 0 {
			return &gammaRpcError{Code: 1010, Message: "Invalid Transaction", Data: json.RawMessage(`"` + hex.EncodeToString(verdict) + `"`)}
		}
		self.submitted = append(self.submitted, encoded)
		answer = gammaExtrinsicHash(encoded).Hex()
	case "system_accountNextIndex":
		hotkey, err := model.DecodeBittensorAddress(param(0))
		if err != nil {
			return err
		}
		next := self.nonces[hotkey]
		if self.poolAhead[hotkey] {
			next++
		}
		answer = next
	default:
		return fmt.Errorf("unexpected %s", method)
	}
	if result == nil {
		return nil
	}
	encoded, err := json.Marshal(answer)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, result)
}

// gammaSimParse reads a submitted spend: its signer, nonce and call.
func (self *gammaSimChain) parse(encoded []byte) *gammaSimSpend {
	_, n := gammaTestCompact(self.t, encoded)
	b := encoded[n:]
	spend := &gammaSimSpend{}
	copy(spend.signer[:], b[2:34])
	rest := b[99:]
	eraLen := 2
	if rest[0] == 0 {
		eraLen = 1
	}
	nonce, nonceLen := gammaTestCompact(self.t, rest[eraLen:])
	spend.nonce = nonce
	_, tipLen := gammaTestCompact(self.t, rest[eraLen+nonceLen:])
	call := rest[eraLen+nonceLen+tipLen+1:]
	// SubtensorModule.spend_gamma(service, netuid, amount): pallet 7, call 168
	if len(call) != 2+32+2+8 || call[0] != 7 || call[1] != 168 {
		self.t.Fatalf("not a spend_gamma: %x", call)
	}
	copy(spend.service[:], call[2:34])
	spend.netuid = binary.LittleEndian.Uint16(call[34:36])
	spend.amount = binary.LittleEndian.Uint64(call[36:44])
	return spend
}

// judge is the chain's verdict on a signed spend at the head, as
// TaggedTransactionQueue_validate_transaction encodes it: its signature (over
// the era's birth block, as the chain reads it), its nonce, then the
// transaction extension's checks of the spend.
func (self *gammaSimChain) judge(encoded []byte) []byte {
	spend := self.parse(encoded)
	// the era's birth block, as the chain computes it for the next block: a
	// spend past its era names another birth, so its signature fails, as on
	// the chain
	_, n := gammaTestCompact(self.t, encoded)
	era := encoded[n+99:]
	if era[0] == 0 {
		self.t.Fatal("an immortal spend")
	}
	encodedEra := uint64(era[0]) | uint64(era[1])<<8
	period := uint64(2) << (encodedEra & 0xf)
	phase := (encodedEra >> 4) * max(period>>12, 1)
	current := self.head + 1
	birth := (max(current, phase)-phase)/period*period + phase
	_, sig, payload := gammaTestSignedPayload(self.t, encoded, 493, 1, types.NewHash(gammaTestHex(self.t, gammaSimGenesis)), gammaSimHash(birth))
	if ok, err := model.VerifyBittensorSignature(gammaAddress(spend.signer), string(payload), hex.EncodeToString(sig)); err != nil || !ok {
		// BadProof
		return []byte{1, 0, 4}
	}
	switch nonce := uint64(self.nonces[spend.signer]); {
	case spend.nonce < nonce:
		// Stale
		return []byte{1, 0, 3}
	case nonce < spend.nonce:
		// Future
		return []byte{1, 0, 2}
	}
	if self.refuse || !self.spendPasses(spend) {
		// Custom(CustomTransactionError::BadRequest)
		return []byte{1, 0, 7, 255}
	}
	valid := []byte{0}
	valid = binary.LittleEndian.AppendUint64(valid, 0)
	valid = append(valid, 0, 0)
	valid = binary.LittleEndian.AppendUint64(valid, 64)
	return append(valid, 1)
}

// spendPasses is gamma.rs gamma_deposit_after_spend, for the sim's state.
func (self *gammaSimChain) spendPasses(spend *gammaSimSpend) bool {
	if spend.netuid != self.netuid || spend.service != self.serviceHotkey || !self.serviceOk {
		return false
	}
	deposit := self.deposits[spend.signer]
	switch {
	case deposit == nil:
		return false
	case spend.amount == 0 || spend.amount < gammaMinSpendUnits && spend.amount != deposit.balance:
		return false
	case deposit.balance < spend.amount:
		return false
	case deposit.cap != nil && *deposit.cap < spend.amount:
		return false
	}
	return true
}

// gammaSimEvent encodes one event record of extrinsic index.
func (self *gammaSimChain) event(index uint32, pallet string, name string, fields []byte) []byte {
	palletIndex, variant := gammaTestEventId(self.t, self.runtime, pallet, name)
	b := []byte{0}
	b = binary.LittleEndian.AppendUint32(b, index)
	b = append(b, palletIndex, variant)
	b = append(b, fields...)
	return append(b, 0)
}

// mine finalizes the next block. It includes what was submitted (unless the
// chain drops it) and is still valid, each spend applied as gamma.rs applies
// it; a spend no longer valid leaves the pool without entering the block.
func (self *gammaSimChain) mine() *gammaSimBlock {
	self.lock.Lock()
	defer self.lock.Unlock()
	number := self.head + 1
	block := &gammaSimBlock{number: number}
	records := [][]byte{}
	if !self.drop {
		for _, encoded := range self.submitted {
			if verdict := self.judge(encoded); verdict[0] != 0 {
				continue
			}
			spend := self.parse(encoded)
			index := uint32(len(block.extrinsics))
			block.extrinsics = append(block.extrinsics, encoded)
			self.nonces[spend.signer]++
			self.deposits[spend.signer].balance -= spend.amount
			var key [64]byte
			copy(key[:32], spend.service[:])
			copy(key[32:], spend.signer[:])
			self.spent[key] += spend.amount
			records = append(records, self.event(index, "SubtensorModule", "GammaSpent", self.spentFields(spend.signer, spend.service, spend.amount)))
		}
	}
	self.submitted = nil
	records = append(records, self.extraEvents...)
	self.extraEvents = nil
	if 0 < len(records) {
		count, err := codec.Encode(types.NewUCompactFromUInt(uint64(len(records))))
		if err != nil {
			self.t.Fatal(err)
		}
		block.events = count
		for _, record := range records {
			block.events = append(block.events, record...)
		}
	}
	self.blocks[number] = block
	self.head = number
	return block
}

func (self *gammaSimChain) spentFields(hotkey [32]byte, service [32]byte, amount uint64) []byte {
	fields := binary.LittleEndian.AppendUint16(nil, self.netuid)
	fields = append(fields, hotkey[:]...)
	fields = append(fields, service[:]...)
	return binary.LittleEndian.AppendUint64(fields, amount)
}

// advance finalizes n empty blocks.
func (self *gammaSimChain) advance(n int) {
	for range n {
		self.lock.Lock()
		self.head++
		self.lock.Unlock()
	}
}

// spentEvent is a GammaSpent the biller never asked for, emitted by
// extrinsic 5 of the block (an extrinsic none of ours).
func (self *gammaSimChain) spentEvent(hotkey [32]byte, service [32]byte, amount uint64) []byte {
	return self.event(5, "SubtensorModule", "GammaSpent", self.spentFields(hotkey, service, amount))
}

// gammaSimChangedMetadataHex is runtime metadata with GammaSpent's service
// (or, in the spender model, its reference) renamed: a runtime the shape
// check must refuse.
func gammaSimChangedMetadataHex(t testing.TB, metadataHex string) string {
	runtime, err := newGammaRuntime(999, 1, metadataHex)
	if err != nil {
		t.Fatal(err)
	}
	m := &runtime.Meta.AsMetadataV14
	for i := range m.Pallets {
		if string(m.Pallets[i].Name) != "SubtensorModule" {
			continue
		}
		events := m.EfficientLookup[m.Pallets[i].Events.Type.Int64()]
		for v := range events.Def.Variant.Variants {
			if string(events.Def.Variant.Variants[v].Name) != "GammaSpent" {
				continue
			}
			for f := range events.Def.Variant.Variants[v].Fields {
				field := &events.Def.Variant.Variants[v].Fields[f]
				if string(field.Name) == "service" || string(field.Name) == "reference" {
					field.Name = "receipt"
				}
			}
		}
	}
	encoded, err := codec.EncodeToHex(*runtime.Meta)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// gammaTestSettings are gamma.yml's settings for the tests: //Alice is the
// deposit master (public test data), so its seed signs as every deposit
// hotkey, and the service is SN25's owner hotkey.
func gammaTestSettings(t testing.TB) *gammaSettings {
	y := gammaTestYaml(func(y *gammaYaml) {
		y.Gamma.RpcUrls = []string{"sim://gamma"}
	})
	settings, err := parseGammaSettings(y)
	if err != nil {
		t.Fatal(err)
	}
	return settings
}
