package controller

// A simulated Subtensor chain for the Gamma biller's database tests: runtime
// 490's recorded metadata, a finalized head the test moves, storage the test
// sets (deposits, the service, the spender's proxies, the billing account),
// and blocks the test mines. A mined block includes what the biller
// submitted and spends or skips each entry by the chain's own rules
// (gamma.rs spend_gamma_entry), with the events runtime 490 emits.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/centrifuge/go-substrate-rpc-client/v4/signature"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types/codec"
)

const gammaSimBase = 1000

type gammaSimBlock struct {
	number     uint64
	extrinsics [][]byte
	events     []byte
}

type gammaSimDeposit struct {
	coldkey [32]byte
	spender [32]byte
	balance uint64
	cap     *uint64
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
	billing       [32]byte
	spender       [32]byte
	serviceHotkey [32]byte
	servicesUrl   string
	nonce         uint32
	free          uint64
	proxyOk       bool
	serviceOk     bool
	subnetEnabled bool
	priceAge      uint64
	deposits      map[[32]byte]*gammaSimDeposit
	// a block whose parent state runs an unreviewed runtime
	unsupportedParent map[uint64]bool
	fee               uint64
	// extra events the next mined block carries
	extraEvents [][]byte

	hashes map[string]uint64
	hashed uint64
}

func newGammaSimChain(t testing.TB, settings *gammaSettings) *gammaSimChain {
	return &gammaSimChain{
		t:                 t,
		runtime:           gammaTestRuntime(t, 490),
		metadataHex:       gammaTestMetadataHex(t, 490),
		head:              gammaSimBase,
		blocks:            map[uint64]*gammaSimBlock{},
		netuid:            settings.Netuid,
		billing:           settings.BillingPublicKey,
		spender:           settings.Spender,
		serviceHotkey:     settings.ServiceHotkey,
		servicesUrl:       settings.ServicesUrl,
		free:              50_000_000,
		proxyOk:           true,
		serviceOk:         true,
		subnetEnabled:     true,
		priceAge:          10,
		deposits:          map[[32]byte]*gammaSimDeposit{},
		unsupportedParent: map[uint64]bool{},
		fee:               204_795,
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
		return gammaHex(append(b, self.spender[:]...))
	case self.key("Proxy", "Proxies", self.spender[:]):
		if !self.proxyOk {
			return ""
		}
		b := []byte{4}
		b = append(b, self.billing[:]...)
		b = append(b, self.runtime.shape.gammaSpendProxyType)
		b = binary.LittleEndian.AppendUint32(b, 0)
		b = binary.LittleEndian.AppendUint64(b, 93_000_000)
		return gammaHex(b)
	case self.key("System", "Account", self.billing[:]):
		b := binary.LittleEndian.AppendUint32(nil, self.nonce)
		b = append(b, make([]byte, 12)...)
		b = binary.LittleEndian.AppendUint64(b, self.free)
		b = append(b, make([]byte, 16)...)
		b = append(b, make([]byte, 16)...)
		return gammaHex(b)
	}
	for hotkey, deposit := range self.deposits {
		if keyText == self.key("SubtensorModule", "GammaDeposits", u16(self.netuid), hotkey[:]) {
			b := append(append([]byte{}, deposit.coldkey[:]...), deposit.spender[:]...)
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
			answer = "0x2f0555cc76fc2840a25a6ea3b9637146806f1f44b090c175ffde2a7e5ab36c03"
		} else if number <= self.head {
			answer = gammaSimHash(number).Hex()
		}
	case "state_getRuntimeVersion":
		number, _ := self.numberOf(param(0))
		spec := 490
		if self.unsupportedParent[number+1] {
			spec = 999
		}
		answer = map[string]any{"specVersion": spec, "transactionVersion": 1}
	case "state_getMetadata":
		number, _ := self.numberOf(param(0))
		if self.unsupportedParent[number+1] {
			answer = gammaSimChangedMetadataHex(self.t, self.metadataHex)
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
		if param(0) == "GammaRuntimeApi_sim_mint_gamma" {
			b := []byte{0}
			b = binary.LittleEndian.AppendUint64(b, 1)
			b = binary.LittleEndian.AppendUint64(b, 1_000_000_000)
			b = binary.LittleEndian.AppendUint64(b, 217_000_000_000)
			answer = gammaHex(b)
		} else {
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
		self.submitted = append(self.submitted, encoded)
		answer = gammaExtrinsicHash(encoded).Hex()
	case "payment_queryInfo":
		answer = map[string]any{"partialFee": fmt.Sprintf("%d", self.fee)}
	case "system_accountNextIndex":
		answer = self.nonce
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

// gammaSimEvent encodes one event record of extrinsic index.
func (self *gammaSimChain) event(index uint32, pallet string, name string, fields []byte) []byte {
	palletIndex, variant := gammaTestEventId(self.t, self.runtime, pallet, name)
	b := []byte{0}
	b = binary.LittleEndian.AppendUint32(b, index)
	b = append(b, palletIndex, variant)
	b = append(b, fields...)
	return append(b, 0)
}

// gammaSimParse reads a submitted proxied spend: its nonce and its entries.
func (self *gammaSimChain) parse(encoded []byte) (signer [32]byte, nonce uint64, entries []gammaSpendEntry) {
	_, n := gammaTestCompact(self.t, encoded)
	b := encoded[n:]
	copy(signer[:], b[2:34])
	rest := b[99:]
	eraLen := 2
	if rest[0] == 0 {
		eraLen = 1
	}
	nonce, nonceLen := gammaTestCompact(self.t, rest[eraLen:])
	_, tipLen := gammaTestCompact(self.t, rest[eraLen+nonceLen:])
	call := rest[eraLen+nonceLen+tipLen+1:]
	// Proxy.proxy(Id(real), Some(GammaSpend), spend_gamma(netuid, entries))
	inner := call[2+1+32+2:]
	count, countLen := gammaTestCompact(self.t, inner[4:])
	body := inner[4+countLen:]
	for i := uint64(0); i < count; i++ {
		var entry gammaSpendEntry
		copy(entry.Hotkey[:], body[0:32])
		entry.Amount = binary.LittleEndian.Uint64(body[32:40])
		copy(entry.Reference[:], body[40:72])
		entries = append(entries, entry)
		body = body[72:]
	}
	return
}

// mine finalizes the next block. It includes what was submitted (unless the
// chain drops it), each spend entry spent or skipped as gamma.rs would.
func (self *gammaSimChain) mine() *gammaSimBlock {
	self.lock.Lock()
	defer self.lock.Unlock()
	number := self.head + 1
	block := &gammaSimBlock{number: number}
	records := [][]byte{}
	if !self.drop {
		for _, encoded := range self.submitted {
			signer, nonce, entries := self.parse(encoded)
			if signer != self.billing || nonce != uint64(self.nonce) {
				continue
			}
			index := uint32(len(block.extrinsics))
			block.extrinsics = append(block.extrinsics, encoded)
			self.nonce++
			fee := append(append([]byte{}, self.billing[:]...), binary.LittleEndian.AppendUint64(nil, self.fee)...)
			fee = binary.LittleEndian.AppendUint64(fee, 0)
			records = append(records, self.event(index, "TransactionPayment", "TransactionFeePaid", fee))
			for _, entry := range entries {
				reason := uint8(0)
				deposit := self.deposits[entry.Hotkey]
				switch {
				case deposit == nil:
					reason = 194 // GammaDepositDoesNotExist
				case deposit.spender != self.spender:
					reason = 198 // NotGammaDepositSpender
				case entry.Amount == 0:
					reason = 82 // AmountTooLow
				case deposit.balance < entry.Amount:
					reason = 199 // InsufficientGammaBalance
				case deposit.cap != nil && *deposit.cap < entry.Amount:
					reason = 200 // GammaDepositDailySpendCapExceeded
				}
				fields := binary.LittleEndian.AppendUint16(nil, self.netuid)
				if reason == 0 {
					deposit.balance -= entry.Amount
					fields = append(fields, deposit.coldkey[:]...)
					fields = append(fields, entry.Hotkey[:]...)
					fields = append(fields, self.spender[:]...)
					fields = binary.LittleEndian.AppendUint64(fields, entry.Amount)
					fields = append(fields, entry.Reference[:]...)
					records = append(records, self.event(index, "SubtensorModule", "GammaSpent", fields))
				} else {
					fields = append(fields, entry.Hotkey[:]...)
					fields = append(fields, self.spender[:]...)
					fields = binary.LittleEndian.AppendUint64(fields, entry.Amount)
					fields = append(fields, entry.Reference[:]...)
					fields = append(fields, 3, self.runtime.shape.subtensorPallet, reason, 0, 0, 0)
					records = append(records, self.event(index, "SubtensorModule", "GammaSpendSkipped", fields))
				}
			}
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

// advance finalizes n empty blocks.
func (self *gammaSimChain) advance(n int) {
	for range n {
		self.lock.Lock()
		self.head++
		self.lock.Unlock()
	}
}

// spentEvent is a GammaSpent from our spender the biller never asked for.
func (self *gammaSimChain) spentEvent(hotkey [32]byte, coldkey [32]byte, amount uint64, reference [32]byte) []byte {
	fields := binary.LittleEndian.AppendUint16(nil, self.netuid)
	fields = append(fields, coldkey[:]...)
	fields = append(fields, hotkey[:]...)
	fields = append(fields, self.spender[:]...)
	fields = binary.LittleEndian.AppendUint64(fields, amount)
	fields = append(fields, reference[:]...)
	return self.event(5, "SubtensorModule", "GammaSpent", fields)
}

// gammaSimChangedMetadataHex is runtime 490's metadata with GammaSpent's
// reference renamed: a runtime the shape check must refuse.
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
			if string(events.Def.Variant.Variants[v].Name) == "GammaSpent" {
				events.Def.Variant.Variants[v].Fields[5].Name = "receipt"
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
// billing key (public test data), the spender is the generated spender's
// public address, and the deposit master is //Alice's public key.
func gammaTestSettings(t testing.TB) *gammaSettings {
	y := gammaTestYaml(func(y *gammaYaml) {
		y.Gamma.DepositHotkeyMaster = "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY"
		y.Gamma.RpcUrls = []string{"sim://gamma"}
	})
	settings, err := parseGammaSettings(y)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signature.KeyringPairFromSecret(gammaTestAliceSeed, 42); err != nil {
		t.Fatal(err)
	}
	return settings
}
