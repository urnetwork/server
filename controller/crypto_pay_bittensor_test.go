package controller

// The TAO reader against recorded Finney data, with no network and no
// database: a fake node answers from fixtures (testdata/crypto_pay/tao_*),
// decoded with the recorded runtime 488 metadata the sn repository keeps.
//
// The block 9478199 fixture is real (public Finney, read 2026-11-10): its
// Balances.Transfer events sit in a Utility.batch_all and a
// SubtensorModule.remove_stake, two shapes the rail never credits by itself.
// The node that served it keeps no state that old any more, so the
// credited-path block is built from a real signed top-level
// Balances.transfer_keep_alive (block 9478202, extrinsic 13) and
// Balances.Transfer events SCALE-encoded with the same metadata.

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/centrifuge/go-substrate-rpc-client/v4/scale"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types/codec"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"

	"github.com/urfoundation/sn/ss58"

	"github.com/urnetwork/server/model"
)

// The recorded runtime 488 metadata, relative to this package.
const cryptoTaoTestMetadataPath = "../../sn/crv4/testdata/runtime488-metadata.scale.gz.base64"

func cryptoTaoTestMetadataRaw(t *testing.T) []byte {
	t.Helper()
	encoded, err := os.ReadFile(cryptoTaoTestMetadataPath)
	if err != nil {
		t.Fatalf("the recorded runtime metadata is unavailable: %v", err)
	}
	compressed, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func cryptoTaoTestMeta(t *testing.T) *types.Metadata {
	t.Helper()
	meta := &types.Metadata{}
	if err := codec.Decode(cryptoTaoTestMetadataRaw(t), meta); err != nil {
		t.Fatal(err)
	}
	return meta
}

// cryptoTaoTestUpgrade is a System.LastRuntimeUpgrade value of a spec.
func cryptoTaoTestUpgrade(t *testing.T, spec uint32) string {
	t.Helper()
	var buffer bytes.Buffer
	encoder := scale.NewEncoder(&buffer)
	if err := encoder.EncodeUintCompact(*big.NewInt(int64(spec))); err != nil {
		t.Fatal(err)
	}
	name := "node-subtensor"
	if err := encoder.EncodeUintCompact(*big.NewInt(int64(len(name)))); err != nil {
		t.Fatal(err)
	}
	buffer.WriteString(name)
	return "0x" + hex.EncodeToString(buffer.Bytes())
}

// cryptoTaoTestTimestamp is a Timestamp.Now value.
func cryptoTaoTestTimestamp(at time.Time) string {
	millis := uint64(at.UnixMilli())
	raw := make([]byte, 8)
	for i := 0; i < 8; i += 1 {
		raw[i] = byte(millis >> (8 * i))
	}
	return "0x" + hex.EncodeToString(raw)
}

// cryptoTaoTestTransfer is one synthesized Balances.Transfer event.
type cryptoTaoTestTransfer struct {
	extrinsic uint32
	from      [32]byte
	to        [32]byte
	amount    uint64
}

// cryptoTaoTestEvents SCALE-encodes System.Events of Balances.Transfer
// records with the metadata's own pallet and event indexes.
func cryptoTaoTestEvents(t *testing.T, meta *types.Metadata, transfers []cryptoTaoTestTransfer) string {
	t.Helper()
	palletIndex, eventIndex := -1, -1
	for _, pallet := range meta.AsMetadataV14.Pallets {
		if string(pallet.Name) != "Balances" || !pallet.HasEvents {
			continue
		}
		palletIndex = int(pallet.Index)
		eventType := meta.AsMetadataV14.EfficientLookup[pallet.Events.Type.Int64()]
		for _, variant := range eventType.Def.Variant.Variants {
			if string(variant.Name) == "Transfer" {
				eventIndex = int(variant.Index)
			}
		}
	}
	if palletIndex < 0 || eventIndex < 0 {
		t.Fatal("the metadata has no Balances.Transfer")
	}
	var buffer bytes.Buffer
	encoder := scale.NewEncoder(&buffer)
	if err := encoder.EncodeUintCompact(*big.NewInt(int64(len(transfers)))); err != nil {
		t.Fatal(err)
	}
	for _, transfer := range transfers {
		// Phase::ApplyExtrinsic(index)
		buffer.WriteByte(0x00)
		index := make([]byte, 4)
		for i := 0; i < 4; i += 1 {
			index[i] = byte(transfer.extrinsic >> (8 * i))
		}
		buffer.Write(index)
		buffer.WriteByte(byte(palletIndex))
		buffer.WriteByte(byte(eventIndex))
		buffer.Write(transfer.from[:])
		buffer.Write(transfer.to[:])
		amount := make([]byte, 8)
		for i := 0; i < 8; i += 1 {
			amount[i] = byte(transfer.amount >> (8 * i))
		}
		buffer.Write(amount)
		// no topics
		buffer.WriteByte(0x00)
	}
	return "0x" + hex.EncodeToString(buffer.Bytes())
}

// The recorded block of real events and extrinsics.
type cryptoTaoTestRecordedBlock struct {
	Number             uint64   `json:"number"`
	Hash               string   `json:"hash"`
	ParentHash         string   `json:"parent_hash"`
	Events             string   `json:"events"`
	LastRuntimeUpgrade string   `json:"last_runtime_upgrade"`
	TimestampNow       string   `json:"timestamp_now"`
	Extrinsics         []string `json:"extrinsics"`
}

func cryptoTaoTestRecorded(t *testing.T) *cryptoTaoTestRecordedBlock {
	t.Helper()
	raw, err := os.ReadFile("testdata/crypto_pay/tao_finney_block_9478199.json")
	if err != nil {
		t.Fatal(err)
	}
	block := &cryptoTaoTestRecordedBlock{}
	if err := json.Unmarshal(raw, block); err != nil {
		t.Fatal(err)
	}
	return block
}

// The recorded signed top-level Balances.transfer_keep_alive.
func cryptoTaoTestPlainTransfer(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/crypto_pay/tao_finney_transfer_keep_alive.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Extrinsic string `json:"extrinsic"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture.Extrinsic
}

// cryptoTaoTestSigner is a signed extrinsic's MultiAddress::Id signer, and
// the bytes after it (the signature, the extensions and the call).
func cryptoTaoTestSigner(t *testing.T, extrinsic string) (signer [32]byte, rest []byte) {
	t.Helper()
	raw, err := hex.DecodeString(strings.TrimPrefix(extrinsic, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	reader := bytes.NewReader(raw)
	decoder := scale.NewDecoder(reader)
	if _, err := decoder.DecodeUintCompact(); err != nil {
		t.Fatal(err)
	}
	offset := len(raw) - reader.Len()
	if raw[offset] != 0x84 || raw[offset+1] != 0x00 {
		t.Fatalf("not a signed extrinsic with a plain account signer")
	}
	copy(signer[:], raw[offset+2:offset+34])
	return signer, raw[offset+34:]
}

// cryptoTaoTestRawSigner rewrites a signed extrinsic with a
// MultiAddress::Raw signer of blobBytes bytes (a quantum-resistant signer's
// shape); truncated cuts it short so it cannot decode.
func cryptoTaoTestRawSigner(t *testing.T, extrinsic string, blobBytes int, truncated bool) string {
	t.Helper()
	_, rest := cryptoTaoTestSigner(t, extrinsic)
	var body bytes.Buffer
	body.WriteByte(0x84)
	body.WriteByte(0x02)
	encoder := scale.NewEncoder(&body)
	if err := encoder.EncodeUintCompact(*big.NewInt(int64(blobBytes))); err != nil {
		t.Fatal(err)
	}
	if truncated {
		body.Write(bytes.Repeat([]byte{0xab}, 100))
	} else {
		body.Write(bytes.Repeat([]byte{0xab}, blobBytes))
		body.Write(rest)
	}
	var out bytes.Buffer
	if err := scale.NewEncoder(&out).EncodeUintCompact(*big.NewInt(int64(body.Len()))); err != nil {
		t.Fatal(err)
	}
	out.Write(body.Bytes())
	return "0x" + hex.EncodeToString(out.Bytes())
}

// ----- the fake node -----

type fakeCryptoTaoBlock struct {
	number     uint64
	hash       string
	parentHash string
	upgrade    string
	events     string
	timestamp  string
	extrinsics []string
}

type fakeCryptoTaoNode struct {
	stateLock sync.Mutex
	genesis   string
	finalized uint64
	best      uint64
	blocks    map[uint64]*fakeCryptoTaoBlock
	// the metadata and spec served at a parent hash
	metadata map[string]string
	versions map[string]uint32
	calls    []string
}

func newFakeCryptoTaoNode() *fakeCryptoTaoNode {
	return &fakeCryptoTaoNode{
		genesis:  cryptoTaoFinneyGenesis,
		blocks:   map[uint64]*fakeCryptoTaoBlock{},
		metadata: map[string]string{},
		versions: map[string]uint32{},
	}
}

func fakeCryptoTaoHash(number uint64) string {
	return fmt.Sprintf("0x%064x", number+0xb10c)
}

// addBlock serves a block at number of spec, with its events, its time and
// its extrinsics; its parent serves spec's metadata.
func (self *fakeCryptoTaoNode) addBlock(t *testing.T, number uint64, spec uint32, events string, at time.Time, extrinsics []string) *fakeCryptoTaoBlock {
	block := &fakeCryptoTaoBlock{
		number:     number,
		hash:       fakeCryptoTaoHash(number),
		parentHash: fakeCryptoTaoHash(number - 1),
		upgrade:    cryptoTaoTestUpgrade(t, spec),
		events:     events,
		timestamp:  cryptoTaoTestTimestamp(at),
		extrinsics: extrinsics,
	}
	self.blocks[number] = block
	self.versions[block.parentHash] = spec
	return block
}

func (self *fakeCryptoTaoNode) byHash(hash string) *fakeCryptoTaoBlock {
	for _, block := range self.blocks {
		if block.hash == hash {
			return block
		}
	}
	return nil
}

func (self *fakeCryptoTaoNode) methodCalls(method string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	count := 0
	for _, call := range self.calls {
		if strings.HasPrefix(call, method+" ") || call == method {
			count += 1
		}
	}
	return count
}

func (self *fakeCryptoTaoNode) call(ctx context.Context, result any, method string, params ...any) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	call := method
	if 0 < len(params) {
		call += fmt.Sprintf(" %v", params[0])
	}
	self.calls = append(self.calls, call)
	var answer any
	switch method {
	case "chain_getFinalizedHead":
		answer = fakeCryptoTaoHash(self.finalized)
	case "chain_getHeader":
		number := self.best
		if 0 < len(params) {
			block := self.byHash(params[0].(string))
			if block == nil {
				return errors.New("unknown block")
			}
			answer = map[string]string{"parentHash": block.parentHash, "number": fmt.Sprintf("0x%x", block.number)}
			break
		}
		answer = map[string]string{"parentHash": fakeCryptoTaoHash(number - 1), "number": fmt.Sprintf("0x%x", number)}
	case "chain_getBlockHash":
		number := fmt.Sprintf("%v", params[0])
		if number == "0" {
			answer = self.genesis
			break
		}
		var n uint64
		fmt.Sscanf(number, "%d", &n)
		if block, ok := self.blocks[n]; ok {
			answer = block.hash
		} else {
			answer = ""
		}
	case "state_getStorage":
		key := params[0].(string)
		block := self.byHash(params[1].(string))
		if block == nil {
			return errors.New("unknown block")
		}
		switch key {
		case cryptoTaoKeySystemEvents:
			answer = block.events
		case cryptoTaoKeyLastRuntimeUpgrade:
			answer = block.upgrade
		case cryptoTaoKeyTimestampNow:
			answer = block.timestamp
		default:
			answer = nil
		}
	case "state_getRuntimeVersion":
		spec, ok := self.versions[params[0].(string)]
		if !ok {
			return errors.New("unknown block")
		}
		answer = map[string]any{"specName": "node-subtensor", "specVersion": spec}
	case "state_getMetadata":
		metadata, ok := self.metadata[params[0].(string)]
		if !ok {
			return errors.New("no metadata here")
		}
		answer = metadata
	case "chain_getBlock":
		block := self.byHash(params[0].(string))
		if block == nil {
			return errors.New("unknown block")
		}
		answer = map[string]any{"block": map[string]any{"extrinsics": block.extrinsics}}
	default:
		return fmt.Errorf("the fake node does not answer %s", method)
	}
	encoded, err := json.Marshal(answer)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, result)
}

// serveMetadata serves the recorded metadata at the parents of the given
// blocks.
func (self *fakeCryptoTaoNode) serveMetadata(t *testing.T, numbers ...uint64) {
	metadataHex := "0x" + hex.EncodeToString(cryptoTaoTestMetadataRaw(t))
	for _, number := range numbers {
		self.metadata[self.blocks[number].parentHash] = metadataHex
	}
}

// A reader over the fake node, unpaced.
func cryptoTaoTestWallet(node *fakeCryptoTaoNode) *bittensorWallet {
	wallet := newBittensorWallet(&cryptoWalletSettings{Asset: model.CryptoAssetTao, ChainRpcUrl: "ws://fake"}, &CryptoPayAssetConfig{Network: cryptoNetworkMainnet})
	wallet.rpc = node
	wallet.settings.RequestSpacing = 0
	return wallet
}

// Forgets the cached runtimes and blocks, and makes no TAO address issued
// until the test issues some (cryptoTaoTestIssue).
func cryptoTaoTestReset(t *testing.T) {
	resetCryptoTaoCaches()
	t.Cleanup(resetCryptoTaoCaches)
	cryptoTaoTestIssue(t, map[string]*model.CryptoPaymentIssuedAddress{})
}

// Makes the given addresses the TAO addresses invoices were given, without a
// database, for the rest of the test.
func cryptoTaoTestIssue(t *testing.T, issued map[string]*model.CryptoPaymentIssuedAddress) {
	previousIssued := cryptoTaoAddressesIssued
	previousAddresses := cryptoTaoIssuedAddresses
	cryptoTaoAddressesIssued = func(ctx context.Context) bool {
		return 0 < len(issued)
	}
	cryptoTaoIssuedAddresses = func(ctx context.Context, addresses []string) map[string]*model.CryptoPaymentIssuedAddress {
		found := map[string]*model.CryptoPaymentIssuedAddress{}
		for _, address := range addresses {
			if issuedAddress, ok := issued[address]; ok {
				found[address] = issuedAddress
			}
		}
		return found
	}
	t.Cleanup(func() {
		cryptoTaoAddressesIssued = previousIssued
		cryptoTaoIssuedAddresses = previousAddresses
	})
}

// One issued address at index, made long before any block the tests read.
func cryptoTaoTestIssued(index int64) *model.CryptoPaymentIssuedAddress {
	return &model.CryptoPaymentIssuedAddress{AddressIndex: index, CreateTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func cryptoTaoTestAccount(seed byte) [32]byte {
	var account [32]byte
	for i := range account {
		account[i] = seed + byte(i)
	}
	return account
}

// ----- the tests -----

// The storage keys are the ones the metadata derives from the names.
func TestCryptoTaoStorageKeysAreTheMetadatas(t *testing.T) {
	meta := cryptoTaoTestMeta(t)
	for _, c := range []struct {
		pallet string
		item   string
		want   string
	}{
		{"System", "Events", cryptoTaoKeySystemEvents},
		{"System", "LastRuntimeUpgrade", cryptoTaoKeyLastRuntimeUpgrade},
		{"Timestamp", "Now", cryptoTaoKeyTimestampNow},
	} {
		key, err := types.CreateStorageKey(meta, c.pallet, c.item)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, key.Hex(), c.want)
	}
}

// The recorded block decodes: its runtime (488), its five Balances.Transfer
// events in extrinsics 5 and 7, and those extrinsics' top-level calls, which
// the rail does not credit by itself.
func TestCryptoTaoDecodesARecordedFinneyBlock(t *testing.T) {
	recorded := cryptoTaoTestRecorded(t)
	spec, err := cryptoTaoDecodeSpec(recorded.LastRuntimeUpgrade)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, spec, uint32(488))
	at, err := cryptoTaoDecodeTimestamp(recorded.TimestampNow)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, at.Year(), 2026)

	runtime, err := cryptoTaoNewRuntime(cryptoTaoTestMeta(t), 488)
	connect.AssertEqual(t, err, nil)
	transfers, err := cryptoTaoDecodeTransfers(runtime, recorded.Events)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(transfers), 5)
	extrinsics := []int{}
	for _, transfer := range transfers {
		extrinsics = append(extrinsics, transfer.extrinsic)
		connect.AssertEqual(t, 0 < transfer.amount, true)
	}
	connect.AssertEqual(t, extrinsics, []int{5, 5, 5, 7, 7})
	// the first transfer's amount, as the chain recorded it
	connect.AssertEqual(t, transfers[0].amount, uint64(12875739032))

	for _, c := range []struct {
		index  int
		name   string
		signed bool
	}{
		{0, "Timestamp.set", false},
		{2, "Ethereum.transact", false},
		{5, "Utility.batch_all", true},
		{7, "SubtensorModule.remove_stake", true},
	} {
		call, err := cryptoTaoDecodeCall(runtime, recorded.Extrinsics[c.index])
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, call.name, c.name)
		connect.AssertEqual(t, call.signed, c.signed)
		connect.AssertEqual(t, cryptoTaoShapeCredited(call, transfers[0].from), false)
	}
}

// A scan of the recorded block finds the payment to an invoice address and
// holds it for review: a batch is not a shape the rail credits by itself.
func TestCryptoTaoHoldsATransferInABatchForReview(t *testing.T) {
	cryptoTaoTestReset(t)
	recorded := cryptoTaoTestRecorded(t)
	runtime, err := cryptoTaoNewRuntime(cryptoTaoTestMeta(t), 488)
	connect.AssertEqual(t, err, nil)
	transfers, err := cryptoTaoDecodeTransfers(runtime, recorded.Events)
	connect.AssertEqual(t, err, nil)

	node := newFakeCryptoTaoNode()
	at := time.Now().Add(-30 * time.Second)
	node.addBlock(t, recorded.Number, 488, recorded.Events, at, recorded.Extrinsics)
	node.finalized, node.best = recorded.Number, recorded.Number+2
	node.serveMetadata(t, recorded.Number)
	wallet := cryptoTaoTestWallet(node)
	invoice := cryptoTaoAddressOf(transfers[0].to)
	cryptoTaoTestIssue(t, map[string]*model.CryptoPaymentIssuedAddress{invoice: cryptoTaoTestIssued(7)})

	receipts, walletHeight, err := wallet.IncomingSince(context.Background(), recorded.Number)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, walletHeight, recorded.Number)
	connect.AssertEqual(t, 0 < len(receipts), true)
	for _, receipt := range receipts {
		connect.AssertEqual(t, receipt.Address, invoice)
		connect.AssertEqual(t, *receipt.AddressIndex, uint64(7))
		connect.AssertEqual(t, receipt.OutputKey, "7")
		connect.AssertEqual(t, receipt.BlockHeight, recorded.Number)
		connect.AssertEqual(t, receipt.Confirmations, uint64(1))
		connect.AssertEqual(t, receipt.ReviewReason, model.CryptoReviewReasonTransferShape)
		connect.AssertEqual(t, strings.HasPrefix(receipt.TxId, fmt.Sprintf("%d-", recorded.Number)), true)
		connect.AssertEqual(t, 0 < len(receipt.Senders), true)
		for _, sender := range receipt.Senders {
			_, prefix, err := ss58.Decode(sender)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, prefix, uint16(42))
		}
	}
	connect.AssertEqual(t, receipts[0].TxId, fmt.Sprintf("%d-5", recorded.Number))
	connect.AssertEqual(t, wallet.Irreversible(), true)
}

// cryptoTaoTestPlainBlock serves block number as a block whose extrinsic 1 is
// the recorded Balances.transfer_keep_alive, with the given transfers.
func cryptoTaoTestPlainBlock(t *testing.T, node *fakeCryptoTaoNode, number uint64, extrinsic string, transfers []cryptoTaoTestTransfer, at time.Time) {
	t.Helper()
	recorded := cryptoTaoTestRecorded(t)
	events := cryptoTaoTestEvents(t, cryptoTaoTestMeta(t), transfers)
	// extrinsic 0 is the block's Timestamp.set, extrinsic 1 the payment
	node.addBlock(t, number, 488, events, at, []string{recorded.Extrinsics[0], extrinsic})
}

// A top-level Balances.transfer_keep_alive signed by the sender is credited
// (no review reason); one whose sender is not its signer is held.
func TestCryptoTaoCreditsATopLevelTransfer(t *testing.T) {
	cryptoTaoTestReset(t)
	plain := cryptoTaoTestPlainTransfer(t)
	signer, _ := cryptoTaoTestSigner(t, plain)
	invoice := cryptoTaoTestAccount(0x40)
	other := cryptoTaoTestAccount(0x60)
	at := time.Now().Add(-20 * time.Second)
	node := newFakeCryptoTaoNode()
	cryptoTaoTestPlainBlock(t, node, 9478300, plain, []cryptoTaoTestTransfer{
		{extrinsic: 1, from: signer, to: invoice, amount: 72_386_000},
	}, at)
	cryptoTaoTestPlainBlock(t, node, 9478301, plain, []cryptoTaoTestTransfer{
		// a sender that is not the extrinsic's signer
		{extrinsic: 1, from: other, to: invoice, amount: 1_000},
	}, at.Add(12*time.Second))
	node.finalized, node.best = 9478301, 9478303
	node.serveMetadata(t, 9478300)
	wallet := cryptoTaoTestWallet(node)
	address := cryptoTaoAddressOf(invoice)
	cryptoTaoTestIssue(t, map[string]*model.CryptoPaymentIssuedAddress{address: cryptoTaoTestIssued(3)})

	receipts, walletHeight, err := wallet.IncomingSince(context.Background(), 9478300)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, walletHeight, uint64(9478301))
	connect.AssertEqual(t, len(receipts), 2)
	credited := receipts[0]
	connect.AssertEqual(t, credited.TxId, "9478300-1")
	connect.AssertEqual(t, credited.AmountAtomic, uint64(72_386_000))
	connect.AssertEqual(t, credited.ReviewReason, "")
	connect.AssertEqual(t, credited.Senders, []string{cryptoTaoAddressOf(signer)})
	connect.AssertEqual(t, credited.Timestamp.UnixMilli(), at.UnixMilli())
	connect.AssertEqual(t, receipts[1].TxId, "9478301-1")
	connect.AssertEqual(t, receipts[1].ReviewReason, model.CryptoReviewReasonTransferShape)

	// the heights: the last block read, the finalized and the best heads
	walletHeight, nodeHeight, networkHeight, err := wallet.Heights(context.Background())
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, []uint64{walletHeight, nodeHeight, networkHeight}, []uint64{9478301, 9478301, 9478303})
}

// A second transfer to the same address in another extrinsic is its own
// receipt (a top-up); two transfers in one extrinsic are summed.
func TestCryptoTaoSumsPerExtrinsicAndKeepsTopUpsApart(t *testing.T) {
	cryptoTaoTestReset(t)
	plain := cryptoTaoTestPlainTransfer(t)
	signer, _ := cryptoTaoTestSigner(t, plain)
	invoice := cryptoTaoTestAccount(0x41)
	node := newFakeCryptoTaoNode()
	recorded := cryptoTaoTestRecorded(t)
	events := cryptoTaoTestEvents(t, cryptoTaoTestMeta(t), []cryptoTaoTestTransfer{
		{extrinsic: 1, from: signer, to: invoice, amount: 500},
		{extrinsic: 1, from: signer, to: invoice, amount: 700},
		{extrinsic: 2, from: signer, to: invoice, amount: 900},
	})
	node.addBlock(t, 9478400, 488, events, time.Now(), []string{recorded.Extrinsics[0], plain, plain})
	node.finalized, node.best = 9478400, 9478400
	node.serveMetadata(t, 9478400)
	wallet := cryptoTaoTestWallet(node)
	cryptoTaoTestIssue(t, map[string]*model.CryptoPaymentIssuedAddress{cryptoTaoAddressOf(invoice): cryptoTaoTestIssued(4)})
	receipts, _, err := wallet.IncomingSince(context.Background(), 9478400)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(receipts), 2)
	connect.AssertEqual(t, receipts[0].TxId, "9478400-1")
	connect.AssertEqual(t, receipts[0].AmountAtomic, uint64(1200))
	connect.AssertEqual(t, receipts[1].TxId, "9478400-2")
	connect.AssertEqual(t, receipts[1].AmountAtomic, uint64(900))
}

// A quantum-resistant signer (MultiAddress::Raw, a 5,367-byte blob) of a
// plain transfer decodes and is credited, whatever the signer; one that does
// not decode is skipped and counted, and its transfer held for review, while
// the rest of the block is still read.
func TestCryptoTaoQuantumResistantSigners(t *testing.T) {
	cryptoTaoTestReset(t)
	plain := cryptoTaoTestPlainTransfer(t)
	signer, _ := cryptoTaoTestSigner(t, plain)
	quantum := cryptoTaoTestRawSigner(t, plain, 5367, false)
	broken := cryptoTaoTestRawSigner(t, plain, 5367, true)
	runtime, err := cryptoTaoNewRuntime(cryptoTaoTestMeta(t), 488)
	connect.AssertEqual(t, err, nil)
	call, err := cryptoTaoDecodeCall(runtime, quantum)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, call.name, "Balances.transfer_keep_alive")
	connect.AssertEqual(t, call.signer == nil, true)
	_, err = cryptoTaoDecodeCall(runtime, broken)
	connect.AssertEqual(t, err != nil, true)

	invoice := cryptoTaoTestAccount(0x42)
	recorded := cryptoTaoTestRecorded(t)
	node := newFakeCryptoTaoNode()
	events := cryptoTaoTestEvents(t, cryptoTaoTestMeta(t), []cryptoTaoTestTransfer{
		{extrinsic: 1, from: signer, to: invoice, amount: 1_000},
		{extrinsic: 2, from: signer, to: invoice, amount: 2_000},
		{extrinsic: 3, from: signer, to: invoice, amount: 3_000},
	})
	node.addBlock(t, 9478500, 488, events, time.Now(), []string{recorded.Extrinsics[0], quantum, broken, plain})
	node.finalized, node.best = 9478500, 9478500
	node.serveMetadata(t, 9478500)
	wallet := cryptoTaoTestWallet(node)
	cryptoTaoTestIssue(t, map[string]*model.CryptoPaymentIssuedAddress{cryptoTaoAddressOf(invoice): cryptoTaoTestIssued(5)})
	before := testutil.ToFloat64(cryptoPayTaoExtrinsicsUndecodable)
	receipts, _, err := wallet.IncomingSince(context.Background(), 9478500)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, testutil.ToFloat64(cryptoPayTaoExtrinsicsUndecodable)-before, float64(1))
	connect.AssertEqual(t, len(receipts), 3)
	connect.AssertEqual(t, receipts[0].ReviewReason, "")
	connect.AssertEqual(t, receipts[1].ReviewReason, model.CryptoReviewReasonTransferShape)
	connect.AssertEqual(t, receipts[2].ReviewReason, "")
}

// A block of a runtime the reader cannot read pauses it: an error, the pause
// gauge at 1 and nothing credited. A scan that reads again clears the gauge.
func TestCryptoTaoPausesOnAnUnknownRuntime(t *testing.T) {
	cryptoTaoTestReset(t)
	plain := cryptoTaoTestPlainTransfer(t)
	signer, _ := cryptoTaoTestSigner(t, plain)
	invoice := cryptoTaoTestAccount(0x43)
	recorded := cryptoTaoTestRecorded(t)
	meta := cryptoTaoTestMeta(t)
	transfer := []cryptoTaoTestTransfer{{extrinsic: 1, from: signer, to: invoice, amount: 1_000}}
	issued := map[string]*model.CryptoPaymentIssuedAddress{cryptoTaoAddressOf(invoice): cryptoTaoTestIssued(6)}

	// the finalized head runs a runtime (999) whose metadata does not decode:
	// block 9478600 (488, the one before it) reads, then 9478601 pauses
	node := newFakeCryptoTaoNode()
	node.addBlock(t, 9478600, 488, cryptoTaoTestEvents(t, meta, transfer), time.Now(), []string{recorded.Extrinsics[0], plain})
	node.addBlock(t, 9478601, 999, cryptoTaoTestEvents(t, meta, transfer), time.Now(), []string{recorded.Extrinsics[0], plain})
	node.finalized, node.best = 9478601, 9478601
	node.serveMetadata(t, 9478600)
	node.metadata[node.blocks[9478601].parentHash] = "0xdeadbeef"
	wallet := cryptoTaoTestWallet(node)
	cryptoTaoTestIssue(t, issued)
	receipts, _, err := wallet.IncomingSince(context.Background(), 9478600)
	connect.AssertEqual(t, receipts == nil, true)
	connect.AssertEqual(t, errors.Is(err, errCryptoTaoRuntimeUnreadable), true)
	connect.AssertEqual(t, testutil.ToFloat64(cryptoPayTaoReaderPaused), float64(1))

	// a third runtime in one scan pauses too
	cryptoTaoTestReset(t)
	node = newFakeCryptoTaoNode()
	node.addBlock(t, 9478700, 486, cryptoTaoTestEvents(t, meta, transfer), time.Now(), []string{recorded.Extrinsics[0], plain})
	node.addBlock(t, 9478701, 487, cryptoTaoTestEvents(t, meta, transfer), time.Now(), []string{recorded.Extrinsics[0], plain})
	node.addBlock(t, 9478702, 488, cryptoTaoTestEvents(t, meta, transfer), time.Now(), []string{recorded.Extrinsics[0], plain})
	node.finalized, node.best = 9478702, 9478702
	node.serveMetadata(t, 9478700, 9478701, 9478702)
	wallet = cryptoTaoTestWallet(node)
	cryptoTaoTestIssue(t, issued)
	_, _, err = wallet.IncomingSince(context.Background(), 9478700)
	connect.AssertEqual(t, errors.Is(err, errCryptoTaoRuntimeUnreadable), true)
	connect.AssertEqual(t, strings.Contains(err.Error(), "spec 487"), true)
	connect.AssertEqual(t, testutil.ToFloat64(cryptoPayTaoReaderPaused), float64(1))

	// a node that reads again clears the pause
	cryptoTaoTestReset(t)
	node = newFakeCryptoTaoNode()
	node.addBlock(t, 9478800, 488, cryptoTaoTestEvents(t, meta, transfer), time.Now(), []string{recorded.Extrinsics[0], plain})
	node.finalized, node.best = 9478800, 9478800
	node.serveMetadata(t, 9478800)
	wallet = cryptoTaoTestWallet(node)
	cryptoTaoTestIssue(t, issued)
	receipts, _, err = wallet.IncomingSince(context.Background(), 9478800)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(receipts), 1)
	connect.AssertEqual(t, testutil.ToFloat64(cryptoPayTaoReaderPaused), float64(0))
}

// Events and extrinsics that do not decode are errors, never a panic.
func TestCryptoTaoReaderNeverPanics(t *testing.T) {
	runtime, err := cryptoTaoNewRuntime(cryptoTaoTestMeta(t), 488)
	connect.AssertEqual(t, err, nil)
	_, err = cryptoTaoDecodeTransfers(runtime, "0x0401ffff")
	connect.AssertEqual(t, errors.Is(err, errCryptoTaoRuntimeUnreadable), true)
	_, err = cryptoTaoDecodeTransfers(runtime, "nothex")
	connect.AssertEqual(t, err != nil, true)
	for _, garbage := range []string{"0x", "0x00", "0x0884", "0x10840700ff", "zz"} {
		_, err := cryptoTaoDecodeCall(runtime, garbage)
		connect.AssertEqual(t, err != nil, true)
	}
	_, err = cryptoTaoNewRuntime(&types.Metadata{Version: 13}, 1)
	connect.AssertEqual(t, errors.Is(err, errCryptoTaoRuntimeUnreadable), true)
	_, err = cryptoTaoDecodeSpec("0x")
	connect.AssertEqual(t, err != nil, true)
}

// Until a TAO invoice address is issued no block is read: only the heads.
func TestCryptoTaoIdleScanReadsNoBlock(t *testing.T) {
	cryptoTaoTestReset(t)
	recorded := cryptoTaoTestRecorded(t)
	node := newFakeCryptoTaoNode()
	node.addBlock(t, recorded.Number, 488, recorded.Events, time.Now(), recorded.Extrinsics)
	node.finalized, node.best = recorded.Number, recorded.Number
	wallet := cryptoTaoTestWallet(node)
	receipts, walletHeight, err := wallet.IncomingSince(context.Background(), 1)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(receipts), 0)
	connect.AssertEqual(t, walletHeight, recorded.Number)
	connect.AssertEqual(t, node.methodCalls("chain_getBlockHash"), 0)
	connect.AssertEqual(t, node.methodCalls("chain_getBlock"), 0)
	connect.AssertEqual(t, node.methodCalls("state_getMetadata"), 0)
}

// The scan range: from the cursor, never past the lookback (counted), at most
// a scan's worth of blocks.
func TestCryptoTaoScanRange(t *testing.T) {
	settings := DefaultCryptoTaoReaderSettings()
	for _, c := range []struct {
		minHeight, finalized uint64
		start, end, skipped  uint64
	}{
		// a first scan starts at the lookback, without counting
		{0, 10_000, 9_700, 9_799, 0},
		// a scanner that was down past the lookback skips the oldest blocks
		{9_650, 10_000, 9_700, 9_799, 50},
		{9_990, 10_000, 9_990, 10_000, 0},
		// a cursor ahead of the node reads the head only
		{12_000, 10_000, 10_000, 10_000, 0},
		// a young chain
		{0, 120, 0, 99, 0},
	} {
		start, end, skipped, ok := cryptoTaoScanRange(settings, c.minHeight, c.finalized)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, []uint64{start, end, skipped}, []uint64{c.start, c.end, c.skipped})
	}
	_, _, _, ok := cryptoTaoScanRange(&CryptoTaoReaderSettings{LookbackBlocks: 300}, 9_990, 10_000)
	connect.AssertEqual(t, ok, false)

	// the reader counts the blocks it passed over
	cryptoTaoTestReset(t)
	recorded := cryptoTaoTestRecorded(t)
	node := newFakeCryptoTaoNode()
	lookback := settings.LookbackBlocks
	head := uint64(9_478_199) + lookback
	node.addBlock(t, head-lookback, 488, recorded.Events, time.Now(), recorded.Extrinsics)
	node.finalized, node.best = head, head
	node.addBlock(t, head, 488, recorded.Events, time.Now(), recorded.Extrinsics)
	node.serveMetadata(t, head-lookback)
	for number := head - lookback + 1; number < head; number += 1 {
		node.addBlock(t, number, 488, "0x00", time.Now(), []string{})
	}
	wallet := cryptoTaoTestWallet(node)
	cryptoTaoTestIssue(t, map[string]*model.CryptoPaymentIssuedAddress{cryptoTaoAddressOf(cryptoTaoTestAccount(0x44)): cryptoTaoTestIssued(9)})
	before := testutil.ToFloat64(cryptoPayScanBlocksSkipped.WithLabelValues(model.CryptoAssetTao))
	_, walletHeight, err := wallet.IncomingSince(context.Background(), head-lookback-25)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, testutil.ToFloat64(cryptoPayScanBlocksSkipped.WithLabelValues(model.CryptoAssetTao))-before, float64(25))
	connect.AssertEqual(t, walletHeight, head-lookback+uint64(settings.BlocksPerScan)-1)
}

// Transaction re-reads a receipt's block: within the lookback as a scan does,
// past it only with its runtime cached, never "not found".
func TestCryptoTaoTransactionReRead(t *testing.T) {
	cryptoTaoTestReset(t)
	plain := cryptoTaoTestPlainTransfer(t)
	signer, _ := cryptoTaoTestSigner(t, plain)
	invoice := cryptoTaoTestAccount(0x45)
	address := cryptoTaoAddressOf(invoice)
	previousResolve := cryptoResolveAddresses
	cryptoResolveAddresses = func(ctx context.Context, asset string, addresses []string) map[string]int64 {
		resolved := map[string]int64{}
		for _, candidate := range addresses {
			if candidate == address {
				resolved[candidate] = 11
			}
		}
		return resolved
	}
	t.Cleanup(func() {
		cryptoResolveAddresses = previousResolve
	})
	node := newFakeCryptoTaoNode()
	cryptoTaoTestPlainBlock(t, node, 9479000, plain, []cryptoTaoTestTransfer{{extrinsic: 1, from: signer, to: invoice, amount: 4_242}}, time.Now())
	node.finalized, node.best = 9479000, 9479000
	node.serveMetadata(t, 9479000)
	wallet := cryptoTaoTestWallet(node)
	receipts, found, err := wallet.Transaction(context.Background(), "9479000-1")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, found, true)
	connect.AssertEqual(t, len(receipts), 1)
	connect.AssertEqual(t, receipts[0].AmountAtomic, uint64(4_242))
	connect.AssertEqual(t, receipts[0].OutputKey, "11")
	connect.AssertEqual(t, receipts[0].Senders, []string{cryptoTaoAddressOf(signer)})
	// an extrinsic of the block that paid none of ours
	receipts, found, err = wallet.Transaction(context.Background(), "9479000-0")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, found, true)
	connect.AssertEqual(t, len(receipts), 0)

	// past the lookback: read with its runtime cached, an error without
	node.finalized = 9479000 + DefaultCryptoTaoReaderSettings().LookbackBlocks + 10
	node.addBlock(t, node.finalized, 488, "0x00", time.Now(), []string{})
	node.best = node.finalized
	wallet = cryptoTaoTestWallet(node)
	resetCryptoTaoBlockCache := func() {
		cryptoTaoBlocksLock.Lock()
		cryptoTaoBlocks = map[string]*cryptoTaoBlock{}
		cryptoTaoBlockOrder = []string{}
		cryptoTaoBlocksLock.Unlock()
	}
	resetCryptoTaoBlockCache()
	receipts, found, err = wallet.Transaction(context.Background(), "9479000-1")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, found, true)
	connect.AssertEqual(t, len(receipts), 1)
	resetCryptoTaoCaches()
	wallet = cryptoTaoTestWallet(node)
	_, found, err = wallet.Transaction(context.Background(), "9479000-1")
	connect.AssertEqual(t, errors.Is(err, errCryptoTaoRuntimeUnreadable), true)
	connect.AssertEqual(t, found, false)

	// a block beyond the finalized head is not there yet
	_, found, err = wallet.Transaction(context.Background(), fmt.Sprintf("%d-1", node.finalized+1))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, found, false)
	_, _, err = wallet.Transaction(context.Background(), "not-a-tx")
	connect.AssertEqual(t, err != nil, true)
	connect.AssertEqual(t, cryptoTaoTxId(12, -1), "12-init")
	number, extrinsic, err := cryptoTaoParseTxId("12-final")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, []int{int(number), extrinsic}, []int{12, -2})
}

// A finalized head older than five minutes is an unknown network height; a
// node not on Finney is refused.
func TestCryptoTaoHeightsAndNetwork(t *testing.T) {
	cryptoTaoTestReset(t)
	recorded := cryptoTaoTestRecorded(t)
	node := newFakeCryptoTaoNode()
	node.addBlock(t, recorded.Number, 488, recorded.Events, time.Now().Add(-10*time.Minute), recorded.Extrinsics)
	node.finalized, node.best = recorded.Number, recorded.Number+3
	wallet := cryptoTaoTestWallet(node)
	walletHeight, nodeHeight, networkHeight, err := wallet.Heights(context.Background())
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, []uint64{walletHeight, nodeHeight, networkHeight}, []uint64{recorded.Number, recorded.Number, 0})

	connect.AssertEqual(t, wallet.CheckNetwork(context.Background()), nil)
	node.genesis = "0x" + strings.Repeat("11", 32)
	connect.AssertEqual(t, wallet.CheckNetwork(context.Background()) != nil, true)
	testnet := newBittensorWallet(&cryptoWalletSettings{Asset: model.CryptoAssetTao}, &CryptoPayAssetConfig{Network: cryptoNetworkTestnet})
	testnet.rpc = node
	connect.AssertEqual(t, testnet.CheckNetwork(context.Background()) != nil, true)
}

// A refund goes to an SS58 address the customer gave, under any prefix,
// written with Bittensor's 42. A tao_ address is not taken (its format is
// unconfirmed).
func TestCryptoTaoRefundAddress(t *testing.T) {
	account := cryptoTaoTestAccount(0x46)
	polkadot, err := ss58.Encode(account, 0)
	connect.AssertEqual(t, err, nil)
	canonical, err := cryptoTaoRefundAddressCanonical(polkadot)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, canonical, cryptoTaoAddressOf(account))
	_, prefix, err := ss58.Decode(canonical)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, prefix, uint16(42))
	for _, refused := range []string{"", "tao_1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh", "5notanaddress", "0x" + strings.Repeat("ab", 32)} {
		_, err := cryptoTaoRefundAddressCanonical(refused)
		connect.AssertEqual(t, err != nil, true)
	}
}

// Every address a TAO invoice was ever given is matched in the blocks read,
// whatever its invoice's state (no list of addresses in play): a transfer to
// one is a receipt, a transfer to an address no invoice was given is not,
// and the issued addresses are looked up once a scan for every destination
// the read blocks pay. Root cause (2026-11-13): the reader matched only
// invoice addresses in play and read no block while none was, so a payment
// to an old or expired invoice was never recorded.
func TestCryptoTaoMatchesEveryIssuedAddress(t *testing.T) {
	cryptoTaoTestReset(t)
	plain := cryptoTaoTestPlainTransfer(t)
	signer, _ := cryptoTaoTestSigner(t, plain)
	issued := cryptoTaoTestAccount(0x70)
	stranger := cryptoTaoTestAccount(0x71)
	at := time.Now().Add(-30 * time.Second)
	node := newFakeCryptoTaoNode()
	cryptoTaoTestPlainBlock(t, node, 9478900, plain, []cryptoTaoTestTransfer{{extrinsic: 1, from: signer, to: issued, amount: 5_000}}, at)
	cryptoTaoTestPlainBlock(t, node, 9478901, plain, []cryptoTaoTestTransfer{{extrinsic: 1, from: signer, to: stranger, amount: 6_000}}, at.Add(12*time.Second))
	node.finalized, node.best = 9478901, 9478901
	node.serveMetadata(t, 9478900)
	cryptoTaoTestIssue(t, map[string]*model.CryptoPaymentIssuedAddress{cryptoTaoAddressOf(issued): cryptoTaoTestIssued(12)})
	lookups := [][]string{}
	previousLookup := cryptoTaoIssuedAddresses
	cryptoTaoIssuedAddresses = func(ctx context.Context, addresses []string) map[string]*model.CryptoPaymentIssuedAddress {
		lookups = append(lookups, addresses)
		return previousLookup(ctx, addresses)
	}
	t.Cleanup(func() {
		cryptoTaoIssuedAddresses = previousLookup
	})

	wallet := cryptoTaoTestWallet(node)
	receipts, walletHeight, err := wallet.IncomingSince(context.Background(), 9478900)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, walletHeight, uint64(9478901))
	connect.AssertEqual(t, len(receipts), 1)
	connect.AssertEqual(t, receipts[0].Address, cryptoTaoAddressOf(issued))
	connect.AssertEqual(t, receipts[0].OutputKey, "12")
	connect.AssertEqual(t, receipts[0].AmountAtomic, uint64(5_000))
	connect.AssertEqual(t, len(lookups), 1)
	connect.AssertEqual(t, len(lookups[0]), 2)
	// the stranger's block paid none of ours: its body was never read
	connect.AssertEqual(t, node.methodCalls("chain_getBlock"), 1)
}

// An invoice made after a block was mined, past the clock tolerance, was not
// paid in it; one made within the tolerance is matched.
func TestCryptoTaoSkipsAnInvoiceMadeAfterItsBlock(t *testing.T) {
	cryptoTaoTestReset(t)
	plain := cryptoTaoTestPlainTransfer(t)
	signer, _ := cryptoTaoTestSigner(t, plain)
	invoice := cryptoTaoTestAccount(0x72)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	node := newFakeCryptoTaoNode()
	cryptoTaoTestPlainBlock(t, node, 9479000, plain, []cryptoTaoTestTransfer{{extrinsic: 1, from: signer, to: invoice, amount: 7_000}}, at)
	node.finalized, node.best = 9479000, 9479000
	node.serveMetadata(t, 9479000)
	tolerance := DefaultCryptoTaoReaderSettings().ClockTolerance
	for _, c := range []struct {
		madeAt   time.Time
		receipts int
	}{
		{at.Add(tolerance + time.Minute), 0},
		{at.Add(tolerance - time.Minute), 1},
		{at.Add(-48 * time.Hour), 1},
	} {
		cryptoTaoTestIssue(t, map[string]*model.CryptoPaymentIssuedAddress{
			cryptoTaoAddressOf(invoice): {AddressIndex: 13, CreateTime: c.madeAt},
		})
		receipts, _, err := cryptoTaoTestWallet(node).IncomingSince(context.Background(), 9479000)
		connect.AssertEqual(t, err, nil)
		if len(receipts) != c.receipts {
			t.Fatalf("an invoice made %s after its block: %d receipts, want %d", c.madeAt.Sub(at), len(receipts), c.receipts)
		}
	}
}

// A scan stops reading blocks before its requests could pass
// RequestsPerScan and reports the last block it read; the next scan goes on
// from there, and together they read every block.
func TestCryptoTaoStopsAtItsRequestBudget(t *testing.T) {
	cryptoTaoTestReset(t)
	plain := cryptoTaoTestPlainTransfer(t)
	signer, _ := cryptoTaoTestSigner(t, plain)
	invoice := cryptoTaoTestAccount(0x73)
	recorded := cryptoTaoTestRecorded(t)
	node := newFakeCryptoTaoNode()
	const first = uint64(9479100)
	for number := first; number < first+9; number += 1 {
		node.addBlock(t, number, 488, cryptoTaoTestEvents(t, cryptoTaoTestMeta(t), nil), time.Now(), []string{recorded.Extrinsics[0]})
	}
	cryptoTaoTestPlainBlock(t, node, first+9, plain, []cryptoTaoTestTransfer{{extrinsic: 1, from: signer, to: invoice, amount: 8_000}}, time.Now())
	node.finalized, node.best = first+9, first+9
	node.serveMetadata(t, first)
	cryptoTaoTestIssue(t, map[string]*model.CryptoPaymentIssuedAddress{cryptoTaoAddressOf(invoice): cryptoTaoTestIssued(14)})

	limited := cryptoTaoTestWallet(node)
	limited.settings.RequestsPerScan = 30
	receipts, walletHeight, err := limited.IncomingSince(context.Background(), first)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(receipts), 0)
	if first+9 <= walletHeight || walletHeight < first {
		t.Fatalf("a budget of 30 requests read through block %d", walletHeight)
	}
	connect.AssertEqual(t, limited.requests <= 30, true)
	readHeight, _, _, err := limited.Heights(context.Background())
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, readHeight, walletHeight)

	// the next scan, from the cursor less its window of one block
	receipts, walletHeight, err = cryptoTaoTestWallet(node).IncomingSince(context.Background(), walletHeight-1)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, walletHeight, first+9)
	connect.AssertEqual(t, len(receipts), 1)
	connect.AssertEqual(t, receipts[0].AmountAtomic, uint64(8_000))

	// a budget too small for a block reads none, and holds the cursor
	starved := cryptoTaoTestWallet(node)
	starved.settings.RequestsPerScan = 5
	receipts, walletHeight, err = starved.IncomingSince(context.Background(), first+4)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(receipts), 0)
	connect.AssertEqual(t, walletHeight, first+3)
}

// The reader paces its requests to the node: one each RequestSpacing at
// most, and time that passed between requests is not waited again. A request
// whose wait is canceled never reaches the node. Every request is counted by
// method.
func TestCryptoTaoPacesItsRequests(t *testing.T) {
	cryptoTaoTestReset(t)
	recorded := cryptoTaoTestRecorded(t)
	node := newFakeCryptoTaoNode()
	node.addBlock(t, recorded.Number, 488, recorded.Events, time.Now(), recorded.Extrinsics)
	node.finalized, node.best = recorded.Number, recorded.Number
	clock := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	waits := []time.Duration{}
	previousNow := cryptoTaoPaceNow
	previousWait := cryptoTaoPaceWait
	cryptoTaoPaceNow = func() time.Time {
		return clock
	}
	cryptoTaoPaceWait = func(ctx context.Context, wait time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		waits = append(waits, wait)
		clock = clock.Add(wait)
		return nil
	}
	t.Cleanup(func() {
		cryptoTaoPaceNow = previousNow
		cryptoTaoPaceWait = previousWait
	})
	wallet := cryptoTaoTestWallet(node)
	wallet.settings.RequestSpacing = 50 * time.Millisecond
	before := testutil.ToFloat64(cryptoPayTaoNodeRequests.WithLabelValues("chain_getHeader"))

	// the heads: five requests, the first at once
	_, err := wallet.readHeads(context.Background())
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, waits, []time.Duration{50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond})
	connect.AssertEqual(t, wallet.requests, 5)
	connect.AssertEqual(t, testutil.ToFloat64(cryptoPayTaoNodeRequests.WithLabelValues("chain_getHeader"))-before, float64(2))

	// a request a second later waits for nothing
	clock = clock.Add(time.Second)
	var head string
	connect.AssertEqual(t, wallet.call(context.Background(), &head, "chain_getFinalizedHead"), nil)
	connect.AssertEqual(t, len(waits), 4)

	// a paced request whose wait is canceled never reaches the node
	calls := node.methodCalls("chain_getFinalizedHead")
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	err = wallet.call(canceled, &head, "chain_getFinalizedHead")
	connect.AssertEqual(t, errors.Is(err, context.Canceled), true)
	connect.AssertEqual(t, node.methodCalls("chain_getFinalizedHead"), calls)
}
