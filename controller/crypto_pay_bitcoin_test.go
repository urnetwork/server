package controller

// The Bitcoin block scanner (crypto_pay_bitcoin.go) against a fake node that
// answers Alchemy's Bitcoin JSON-RPC from mainnet blocks recorded on
// 2026-11-11 through the free tier (testdata/crypto_pay/btc_block_975400.json
// to btc_block_975402.json: getblock at verbosity 2, trimmed to the coinbase,
// the payments the tests use and a few transactions of every output type,
// each transaction and every number exactly as recorded;
// btc_transactions.json: getrawtransaction at verbosity 2 of those payments,
// with their inputs' prevouts). Below the recorded blocks the node makes up
// empty blocks whose hashes link to the recorded ones; above them, blocks a
// test mines. Hermetic: no database, no network.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/model"
)

// The recorded blocks, and the recorded payments the tests use: the
// transaction, the address it pays (by its script type) and what its outputs
// to that address sum to, in satoshis.
const (
	cryptoTestBtcFirst = uint64(975400)
	cryptoTestBtcLast  = uint64(975402)

	// 975400: one transaction pays a P2WPKH, a P2SH and a P2WSH address
	cryptoTestBtcTxSplit     = "0f736d54b14fffe968316fb42ad59bdca01404631db5fd703a5bdee220612c34"
	cryptoTestBtcP2wpkhTwice = "bc1q8cjh5xzu3ayu9h9n06497d8h59jjxgh0dex808" // 1,320,000 here, 1,320,000 again in 975402
	cryptoTestBtcP2sh        = "35uMe41zhYTcLqwp3ddir4HFyboNvtBxGA"         // 700,000
	cryptoTestBtcP2wsh       = "bc1q5x9p784q6jkj4w3cw56h8vejz326yrf6u4tuksgg8kjnfkpg7l5qe8kpz6"
	cryptoTestBtcSplitSender = "bc1q5x9p784q6jkj4w3cw56h8vejz326yrf6u4tuksgg8kjnfkpg7l5qe8kpz6"
	cryptoTestBtcTxTaproot   = "5f95265a20a853e0a07c5f496ce7f992796b738f1a14250f8419937eb0d5e5c2"
	cryptoTestBtcP2tr        = "bc1plw83cs6xlm02ct2x6k46rjlkz4all8haaztmqf3a35aewdeylt5sdvqdhu" // 20,000
	cryptoTestBtcTxP2pkh     = "fb0470956aaa41ddc05629f31881113febf05d55c136a7aad1e59ade0ccf1cfd"
	cryptoTestBtcP2pkh       = "154mK6MCiYEAju4RNEPimgGNG3k7WYV9V9" // 28,637
	cryptoTestBtcTxSmall     = "78046a6d9bc8682fd6028fb3b641c923912fca05cb19863e6910aff924ea7950"
	// 544,301 in 975400 (cryptoTestBtcTxSmall), 1,039,512 in 975401
	cryptoTestBtcP2wpkhBlocks = "bc1qw090m60shtzedumwn35nyw8u7fqnl62r3pp4s4"
	// 975401
	cryptoTestBtcTxSmallAgain = "3fb6ff89ab35a36c7da2e29f46c8071047a55ec59ffbbbd72627a5f12c639f75"
	cryptoTestBtcTxSingle     = "fe8d615c4dc1ca4b63665665c16c6fc001b3449f33889b9783d908c793bb7135"
	cryptoTestBtcP2wpkhSingle = "bc1qxpjhlx9w22nccplvvtpxs5hlv768c92409cvew" // 140,682
	cryptoTestBtcSingleSender = "bc1q5evrmfdhev84y0j9upzjejvvsexj4p33v6ussv"
	// 975402
	cryptoTestBtcTxSecond     = "d9b6fa6b3727f7b44d101c1d0817f96841aab2c3520e41ae039e76dd5dd09f85"
	cryptoTestBtcP2pkhSecond  = "15TmgEhRCmHWqgYYLcvAQ1xwNPi6by1j1W" // 1,000,000, in cryptoTestBtcTxSecond
	cryptoTestBtcTxLarge      = "92bf589a48ffa84176a2df950d9cf970711607f78e39f65839e34be0fc4fa298"
	cryptoTestBtcP2wpkhLarge  = "bc1qayp3ydjkpey9n9rwlf9k94xj5g56n090l42jjr" // 157,049,473
	cryptoTestBtcLargeSender  = "bc1q7hqx9cmy80l42mk9dv3khmseryyps04prp4aca"
	cryptoTestBtcTxTwoOutputs = "76eb45428c6a5656180feb5369f38bd479d7ba526ec4a3d7078cb773380b97c6"
	// two outputs of one transaction: 546 + 57,857
	cryptoTestBtcP2trTwoOutputs = "bc1ph8afgv3zac6nwsh38kewt49wd6sutg52fg9ffnazn7hv0yzcwuzqnhs7j4"
	// the mining pool's coinbase payout in 975400 and 975402
	cryptoTestBtcCoinbasePayout = "1PuJjnF476W3zXfVYmJfGnouzFDAXakkL4"
)

// ----- the fake node -----

// cryptoTestBitcoinNode answers Bitcoin Core's JSON-RPC as Alchemy passes it
// through, from the recorded blocks and transactions.
type cryptoTestBitcoinNode struct {
	t testing.TB

	stateLock sync.Mutex
	// every block the node knows by hash, as getblock at verbosity 2 answers
	// it (numbers as json.Number, so they are written back as recorded)
	blocks map[string]map[string]any
	// the best chain: the block hash at each height
	chain map[uint64]string
	tip   uint64
	// the recorded blocks' hashes, whatever the best chain holds now
	recorded []string
	// the recorded getrawtransaction answers
	txs map[string]map[string]any
	// the transactions in the mempool
	mempool map[string]bool
	// seconds added to every recorded time, so recorded blocks read as recent
	shift int64
	// the chain getblockchaininfo names
	chainName string
	// answer every request 401 (a key the provider refuses)
	refuse bool
	calls  map[string]int
}

func cryptoTestJsonDecode(t testing.TB, data []byte, out any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		t.Fatalf("fixture: %s", err)
	}
}

// cryptoTestJsonClone copies a decoded json value, so an answer can be patched.
func cryptoTestJsonClone(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = cryptoTestJsonClone(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = cryptoTestJsonClone(item)
		}
		return out
	}
	return value
}

func cryptoTestBtcHash(parts ...any) string {
	sum := sha256.Sum256([]byte(fmt.Sprint(parts...)))
	return hex.EncodeToString(sum[:])
}

func cryptoTestJsonInt(value any) int64 {
	number, ok := value.(json.Number)
	if !ok {
		return 0
	}
	parsed, _ := number.Int64()
	return parsed
}

// newCryptoTestBitcoinNode is the recorded chain with its tip at tip (one of
// the recorded heights).
func newCryptoTestBitcoinNode(t testing.TB, tip uint64) *cryptoTestBitcoinNode {
	node := &cryptoTestBitcoinNode{
		t:         t,
		blocks:    map[string]map[string]any{},
		chain:     map[uint64]string{},
		txs:       map[string]map[string]any{},
		mempool:   map[string]bool{},
		chainName: "main",
		calls:     map[string]int{},
	}
	for height := cryptoTestBtcFirst; height <= cryptoTestBtcLast; height += 1 {
		var block map[string]any
		cryptoTestJsonDecode(t, cryptoTestFixture(t, fmt.Sprintf("btc_block_%d.json", height)), &block)
		hash := block["hash"].(string)
		node.blocks[hash] = block
		node.chain[height] = hash
		node.recorded = append(node.recorded, hash)
	}
	cryptoTestJsonDecode(t, cryptoTestFixture(t, "btc_transactions.json"), &node.txs)
	node.tip = tip
	return node
}

// syntheticBelow is the made-up empty block at a height below the recorded
// ones, linked to them: 975399's hash is the one 975400 names as its parent.
func (self *cryptoTestBitcoinNode) syntheticBelow(height uint64) map[string]any {
	first := self.blocks[self.chain[cryptoTestBtcFirst]]
	hashAt := func(h uint64) string {
		if h == cryptoTestBtcFirst-1 {
			return first["previousblockhash"].(string)
		}
		return cryptoTestBtcHash("urnetwork btc test block ", h)
	}
	firstTime := cryptoTestJsonInt(first["time"])
	return cryptoTestEmptyBlock(hashAt(height), height, hashAt(height-1), firstTime-int64(cryptoTestBtcFirst-height)*600)
}

func cryptoTestEmptyBlock(hash string, height uint64, previous string, blockTime int64) map[string]any {
	return map[string]any{
		"hash":              hash,
		"height":            json.Number(fmt.Sprint(height)),
		"previousblockhash": previous,
		"time":              json.Number(fmt.Sprint(blockTime)),
		"mediantime":        json.Number(fmt.Sprint(blockTime - 3000)),
		"tx": []any{map[string]any{
			"txid": cryptoTestBtcHash("coinbase ", hash),
			"vin":  []any{map[string]any{"coinbase": "00", "sequence": json.Number("4294967295")}},
			"vout": []any{map[string]any{
				"value":        json.Number("3.12500000"),
				"n":            json.Number("0"),
				"scriptPubKey": map[string]any{"type": "nulldata"},
			}},
		}},
	}
}

// blockAt is the best chain's block at height (made up below the recorded
// ones), or nil above the tip.
func (self *cryptoTestBitcoinNode) blockAt(height uint64) map[string]any {
	if self.tip < height {
		return nil
	}
	if hash, ok := self.chain[height]; ok {
		return self.blocks[hash]
	}
	if height < cryptoTestBtcFirst {
		block := self.syntheticBelow(height)
		self.blocks[block["hash"].(string)] = block
		self.chain[height] = block["hash"].(string)
		return block
	}
	return nil
}

// recordedTx is a recorded transaction as a block holds it: its
// getrawtransaction answer, or its entry in a recorded block.
func (self *cryptoTestBitcoinNode) recordedTx(txId string) map[string]any {
	if tx, ok := self.txs[txId]; ok {
		return cryptoTestJsonClone(tx).(map[string]any)
	}
	for _, hash := range self.recorded {
		for _, item := range self.blocks[hash]["tx"].([]any) {
			if tx := item.(map[string]any); tx["txid"] == txId {
				return cryptoTestJsonClone(tx).(map[string]any)
			}
		}
	}
	self.t.Fatalf("no recorded transaction %s", txId)
	return nil
}

// mine adds a block on the tip holding the given recorded transactions.
func (self *cryptoTestBitcoinNode) mine(txIds ...string) string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	parent := self.blockAt(self.tip)
	height := self.tip + 1
	hash := cryptoTestBtcHash("urnetwork btc test mined ", height, " ", parent["hash"])
	block := cryptoTestEmptyBlock(hash, height, parent["hash"].(string), cryptoTestJsonInt(parent["time"])+600)
	for _, txId := range txIds {
		tx := self.recordedTx(txId)
		for _, key := range []string{"blockhash", "confirmations", "blocktime", "time"} {
			delete(tx, key)
		}
		block["tx"] = append(block["tx"].([]any), tx)
		delete(self.mempool, txId)
	}
	self.blocks[hash] = block
	self.chain[height] = hash
	self.tip = height
	return hash
}

// fork replaces the best chain from height on with copies of its blocks
// without the dropped transactions, under new hashes, one block longer than
// the chain it replaces (so it is the longer chain). The replaced blocks stay
// known, off the best chain.
func (self *cryptoTestBitcoinNode) fork(from uint64, salt string, dropped ...string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	oldTip := self.tip
	previous := self.blockAt(from - 1)["hash"].(string)
	for height := from; height <= oldTip+1; height += 1 {
		hash := cryptoTestBtcHash("urnetwork btc test fork ", salt, " ", height)
		var block map[string]any
		if old := self.blockAt(height); old != nil {
			block = cryptoTestJsonClone(old).(map[string]any)
			block["tx"] = slices.DeleteFunc(block["tx"].([]any), func(tx any) bool {
				return slices.Contains(dropped, tx.(map[string]any)["txid"].(string))
			})
		} else {
			parent := self.blocks[previous]
			block = cryptoTestEmptyBlock(hash, height, previous, cryptoTestJsonInt(parent["time"])+600)
		}
		block["hash"] = hash
		block["previousblockhash"] = previous
		self.blocks[hash] = block
		self.chain[height] = hash
		self.tip = height
		previous = hash
	}
}

// sibling replaces the tip with another block at its height (a node that saw
// that one first), the same length.
func (self *cryptoTestBitcoinNode) sibling(salt string) string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	old := self.blockAt(self.tip)
	block := cryptoTestJsonClone(old).(map[string]any)
	hash := cryptoTestBtcHash("urnetwork btc test sibling ", salt, " ", self.tip)
	block["hash"] = hash
	self.blocks[hash] = block
	self.chain[self.tip] = hash
	return hash
}

func (self *cryptoTestBitcoinNode) setTip(tip uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.tip = tip
}

func (self *cryptoTestBitcoinNode) callCount(method string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.calls[method]
}

func (self *cryptoTestBitcoinNode) totalCalls() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	total := 0
	for _, count := range self.calls {
		total += count
	}
	return total
}

// shifted is a recorded time plus the shift.
func (self *cryptoTestBitcoinNode) shifted(value any) json.Number {
	return json.Number(fmt.Sprint(cryptoTestJsonInt(value) + self.shift))
}

// answerBlock is getblock's answer: the block with its confirmations now.
func (self *cryptoTestBitcoinNode) answerBlock(hash string) map[string]any {
	block := cryptoTestJsonClone(self.blocks[hash]).(map[string]any)
	height := uint64(cryptoTestJsonInt(block["height"]))
	if self.chain[height] == hash && height <= self.tip {
		block["confirmations"] = json.Number(fmt.Sprint(self.tip - height + 1))
	} else {
		block["confirmations"] = json.Number("-1")
	}
	delete(block, "nextblockhash")
	block["time"] = self.shifted(block["time"])
	if _, ok := block["mediantime"]; ok {
		block["mediantime"] = self.shifted(block["mediantime"])
	}
	return block
}

// answerTx is getrawtransaction's answer: in the best chain with its block
// and confirmations, in the mempool without, in a block the chain left with
// 0 confirmations; nil when the node knows it nowhere.
func (self *cryptoTestBitcoinNode) answerTx(txId string) map[string]any {
	recorded, ok := self.txs[txId]
	if !ok {
		return nil
	}
	tx := cryptoTestJsonClone(recorded).(map[string]any)
	for _, key := range []string{"blockhash", "confirmations", "blocktime", "time"} {
		delete(tx, key)
	}
	holds := func(block map[string]any) bool {
		for _, item := range block["tx"].([]any) {
			if item.(map[string]any)["txid"] == txId {
				return true
			}
		}
		return false
	}
	for height := uint64(cryptoTestBtcFirst); height <= self.tip; height += 1 {
		block := self.blockAt(height)
		if block != nil && holds(block) {
			tx["blockhash"] = block["hash"]
			tx["confirmations"] = json.Number(fmt.Sprint(self.tip - height + 1))
			tx["blocktime"] = self.shifted(block["time"])
			tx["time"] = self.shifted(block["time"])
			return tx
		}
	}
	if self.mempool[txId] {
		return tx
	}
	for hash, block := range self.blocks {
		if holds(block) {
			tx["blockhash"] = hash
			tx["confirmations"] = json.Number("0")
			return tx
		}
	}
	return nil
}

func (self *cryptoTestBitcoinNode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.refuse {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var request struct {
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	self.calls[request.Method] += 1
	answer := func(result any) {
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": "0", "result": result})
	}
	refuse := func(code int, message string) {
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": "0", "error": map[string]any{"code": code, "message": message}})
	}
	param := func(i int, out any) bool {
		return i < len(request.Params) && json.Unmarshal(request.Params[i], out) == nil
	}
	switch request.Method {
	case "getblockchaininfo":
		var info map[string]any
		cryptoTestJsonDecode(self.t, cryptoTestFixture(self.t, "btc_blockchaininfo.json"), &info)
		tipBlock := self.blockAt(self.tip)
		info["chain"] = self.chainName
		info["blocks"] = json.Number(fmt.Sprint(self.tip))
		info["headers"] = json.Number(fmt.Sprint(self.tip))
		info["bestblockhash"] = tipBlock["hash"]
		info["time"] = self.shifted(tipBlock["time"])
		info["initialblockdownload"] = false
		answer(info)
	case "getblockcount":
		answer(self.tip)
	case "getblockhash":
		var height uint64
		if !param(0, &height) {
			refuse(-1, "getblockhash height")
			return
		}
		block := self.blockAt(height)
		if block == nil {
			refuse(-8, "Block height out of range")
			return
		}
		answer(block["hash"])
	case "getblock":
		var hash string
		var verbosity int
		if !param(0, &hash) || !param(1, &verbosity) || verbosity != 2 {
			refuse(-1, "getblock hash verbosity")
			return
		}
		if _, ok := self.blocks[hash]; !ok {
			refuse(-5, "Block not found")
			return
		}
		answer(self.answerBlock(hash))
	case "getrawtransaction":
		var txId string
		var verbosity int
		if !param(0, &txId) || !param(1, &verbosity) || verbosity != 2 {
			refuse(-1, "getrawtransaction txid verbosity")
			return
		}
		tx := self.answerTx(txId)
		if tx == nil {
			refuse(-5, "No such mempool or blockchain transaction. Use gettransaction for wallet transactions.")
			return
		}
		answer(tx)
	default:
		refuse(-32601, "Method not found")
	}
}

// serve starts the node's http server; the url carries a synthetic key, as
// Alchemy's does.
func (self *cryptoTestBitcoinNode) serve() string {
	testServer := httptest.NewServer(self)
	self.t.Cleanup(testServer.Close)
	return testServer.URL + "/v2/synthetic-key"
}

// ----- the seams -----

// cryptoTestBitcoinState stands in for the database: the cursor row's kept
// chain, the invoice addresses, the pool, and every lookup made.
type cryptoTestBitcoinState struct {
	stateLock sync.Mutex
	// the kept chain the last complete scan wrote
	chain *model.CryptoScanChain
	// the invoice addresses, and the pool, by address
	ours map[string]int64
	// the addresses each block lookup was asked, in order
	lookups [][]string
}

func cryptoTestBitcoinSeams(t testing.TB, ours map[string]int64) *cryptoTestBitcoinState {
	state := &cryptoTestBitcoinState{ours: ours}
	previousLoad := cryptoBitcoinLoadChain
	previousLookup := cryptoBitcoinInvoiceAddresses
	previousResolve := cryptoResolveAddresses
	cryptoBitcoinLoadChain = func(ctx context.Context) *model.CryptoScanChain {
		state.stateLock.Lock()
		defer state.stateLock.Unlock()
		if state.chain == nil {
			return nil
		}
		chain := *state.chain
		return &chain
	}
	match := func(addresses []string) map[string]int64 {
		state.stateLock.Lock()
		defer state.stateLock.Unlock()
		matched := map[string]int64{}
		for _, address := range addresses {
			if index, ok := state.ours[address]; ok {
				matched[address] = index
			}
		}
		return matched
	}
	cryptoBitcoinInvoiceAddresses = func(ctx context.Context, addresses []string, blockTime time.Time) map[string]int64 {
		state.stateLock.Lock()
		state.lookups = append(state.lookups, slices.Clone(addresses))
		state.stateLock.Unlock()
		return match(addresses)
	}
	cryptoResolveAddresses = func(ctx context.Context, asset string, addresses []string) map[string]int64 {
		return match(addresses)
	}
	t.Cleanup(func() {
		cryptoBitcoinLoadChain = previousLoad
		cryptoBitcoinInvoiceAddresses = previousLookup
		cryptoResolveAddresses = previousResolve
	})
	return state
}

func cryptoTestBitcoinWallet(endpoint string) *bitcoinWallet {
	return newBitcoinWallet(
		&cryptoWalletSettings{Asset: model.CryptoAssetBtc, ChainRpcUrl: endpoint},
		&CryptoPayAssetConfig{Network: cryptoNetworkMainnet, ReorgWindowBlocks: 6},
	)
}

// cryptoTestBitcoinScanResult is one scan as the scanner makes it.
type cryptoTestBitcoinScanResult struct {
	receipts     []*cryptoWalletReceipt
	walletHeight uint64
	nodeHeight   uint64
	err          error
}

// scan is one scan in the scanner's order (crypto_pay_scan.go cryptoScan),
// with a wallet of its own, as each scan has: the network check, the reads
// from the cursor less the reorg window, the heights, and the kept chain
// written with the cursor when the scan completes.
func (self *cryptoTestBitcoinState) scan(t testing.TB, endpoint string) *cryptoTestBitcoinScanResult {
	ctx := context.Background()
	wallet := cryptoTestBitcoinWallet(endpoint)
	result := &cryptoTestBitcoinScanResult{}
	if result.err = wallet.CheckNetwork(ctx); result.err != nil {
		return result
	}
	var minHeight uint64
	self.stateLock.Lock()
	if self.chain != nil && 6 < self.chain.Height {
		minHeight = uint64(self.chain.Height) - 6
	}
	self.stateLock.Unlock()
	var height uint64
	result.receipts, height, result.err = wallet.IncomingSince(ctx, minHeight)
	if result.err != nil {
		return result
	}
	walletHeight, nodeHeight, _, err := wallet.Heights(ctx)
	if result.err = err; err != nil {
		return result
	}
	result.walletHeight = max(height, walletHeight)
	result.nodeHeight = nodeHeight
	if chain := wallet.ScanChain(); chain != nil {
		self.stateLock.Lock()
		self.chain = chain
		self.stateLock.Unlock()
	}
	return result
}

// kept is the kept chain as the cursor row holds it.
func (self *cryptoTestBitcoinState) kept(t testing.TB) *bitcoinChain {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	chain := &bitcoinChain{}
	if self.chain != nil {
		if err := json.Unmarshal([]byte(self.chain.RecentBlocks), chain); err != nil {
			t.Fatalf("kept chain: %s", err)
		}
	}
	return chain
}

func (self *cryptoTestBitcoinState) lookupCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.lookups)
}

// cryptoTestBtcReceipt is the receipt of a transaction to an address, or nil.
func cryptoTestBtcReceipt(receipts []*cryptoWalletReceipt, txId string, address string) *cryptoWalletReceipt {
	for _, receipt := range receipts {
		if receipt.TxId == txId && receipt.Address == address {
			return receipt
		}
	}
	return nil
}

// cryptoTestBtcBlockAddresses is every distinct address a recorded block's
// outputs pay, read from the fixture on its own.
func cryptoTestBtcBlockAddresses(t testing.TB, height uint64) []string {
	var block struct {
		Tx []struct {
			Vout []struct {
				ScriptPubKey struct {
					Address string `json:"address"`
				} `json:"scriptPubKey"`
			} `json:"vout"`
		} `json:"tx"`
	}
	cryptoTestJsonDecode(t, cryptoTestFixture(t, fmt.Sprintf("btc_block_%d.json", height)), &block)
	addresses := []string{}
	for _, tx := range block.Tx {
		for _, vout := range tx.Vout {
			if address := vout.ScriptPubKey.Address; address != "" && !slices.Contains(addresses, address) {
				addresses = append(addresses, address)
			}
		}
	}
	return addresses
}

// ----- the tests -----

// A first scan reads back the reorg window from the tip, so the chain it keeps
// has a fork point from the start, and finds a payment in a block: one
// transaction's outputs to the invoice address, summed, at the block's height
// and time, with 1 confirmation at the tip. Each block is matched in one
// lookup, with every address its outputs pay.
func TestCryptoBitcoinScanFindsAPaymentInABlock(t *testing.T) {
	node := newCryptoTestBitcoinNode(t, 975401)
	endpoint := node.serve()
	state := cryptoTestBitcoinSeams(t, map[string]int64{cryptoTestBtcP2wpkhSingle: 11})

	result := state.scan(t, endpoint)
	connect.AssertEqual(t, result.err, nil)
	// 975395 to 975401: the reorg window and the tip
	connect.AssertEqual(t, node.callCount("getblock"), 7)
	connect.AssertEqual(t, state.lookupCount(), 7)
	connect.AssertEqual(t, result.walletHeight, uint64(975401))
	connect.AssertEqual(t, result.nodeHeight, uint64(975401))
	if len(result.receipts) != 1 {
		t.Fatalf("receipts = %d, want the one payment", len(result.receipts))
	}
	receipt := result.receipts[0]
	connect.AssertEqual(t, receipt.TxId, cryptoTestBtcTxSingle)
	connect.AssertEqual(t, receipt.Address, cryptoTestBtcP2wpkhSingle)
	connect.AssertEqual(t, receipt.OutputKey, "11")
	connect.AssertEqual(t, *receipt.AddressIndex, uint64(11))
	connect.AssertEqual(t, receipt.AmountAtomic, uint64(140_682))
	connect.AssertEqual(t, receipt.BlockHeight, uint64(975401))
	connect.AssertEqual(t, receipt.Confirmations, uint64(1))
	connect.AssertEqual(t, receipt.Timestamp, time.Unix(1794380897, 0).UTC())
	// the block's lookup was asked every address its outputs pay, once each
	lookup := state.lookups[len(state.lookups)-1]
	want := cryptoTestBtcBlockAddresses(t, 975401)
	slices.Sort(lookup)
	slices.Sort(want)
	connect.AssertEqual(t, lookup, want)

	// the kept chain: the blocks read, each the parent of the next, and the
	// payment with its block
	kept := state.kept(t)
	connect.AssertEqual(t, len(kept.Blocks), 7)
	connect.AssertEqual(t, kept.tip().Height, uint64(975401))
	connect.AssertEqual(t, kept.tip().Hash, "0000000000000000000062b3a83a32bd97201640c2a41e3ce43b55b998cd79c9")
	connect.AssertEqual(t, kept.Blocks[5].Hash, "000000000000000000002029eb0126c85f672b73f4365c47f189ea811c34bc9d")
	connect.AssertEqual(t, len(kept.tip().Payments), 1)
	connect.AssertEqual(t, state.chain.Height, int64(975401))
	connect.AssertEqual(t, state.chain.BlockHash, kept.tip().Hash)
}

// Every kind of address Coinbase may give an invoice is matched by the
// address Bitcoin Core names for the output: P2PKH, P2SH (Coinbase issued one
// for BTC on 2026-11-11), P2WPKH, P2WSH and P2TR; and two outputs of one
// transaction to one address are summed.
func TestCryptoBitcoinScanMatchesEveryAddressType(t *testing.T) {
	node := newCryptoTestBitcoinNode(t, 975402)
	endpoint := node.serve()
	state := cryptoTestBitcoinSeams(t, map[string]int64{
		cryptoTestBtcP2pkh:          1,
		cryptoTestBtcP2sh:           2,
		cryptoTestBtcP2wpkhSingle:   3,
		cryptoTestBtcP2wsh:          4,
		cryptoTestBtcP2tr:           5,
		cryptoTestBtcP2trTwoOutputs: 6,
	})
	result := state.scan(t, endpoint)
	connect.AssertEqual(t, result.err, nil)
	wants := []struct {
		txId    string
		address string
		sats    uint64
		height  uint64
	}{
		{cryptoTestBtcTxP2pkh, cryptoTestBtcP2pkh, 28_637, 975400},
		{cryptoTestBtcTxSplit, cryptoTestBtcP2sh, 700_000, 975400},
		{cryptoTestBtcTxSingle, cryptoTestBtcP2wpkhSingle, 140_682, 975401},
		{cryptoTestBtcTxSplit, cryptoTestBtcP2wsh, 1_841_115, 975400},
		// the P2WSH address spends its own outputs in both transactions:
		// only what they pay back to it is a payment, never what it spends
		{cryptoTestBtcTxSecond, cryptoTestBtcP2wsh, 2_332_278, 975402},
		{cryptoTestBtcTxTaproot, cryptoTestBtcP2tr, 20_000, 975400},
		{cryptoTestBtcTxTwoOutputs, cryptoTestBtcP2trTwoOutputs, 546 + 57_857, 975402},
	}
	for _, want := range wants {
		receipt := cryptoTestBtcReceipt(result.receipts, want.txId, want.address)
		if receipt == nil {
			t.Fatalf("no receipt for %s in %s", want.address, want.txId)
		}
		connect.AssertEqual(t, receipt.AmountAtomic, want.sats)
		connect.AssertEqual(t, receipt.BlockHeight, want.height)
		connect.AssertEqual(t, receipt.Confirmations, 975402-want.height+1)
	}
	connect.AssertEqual(t, len(result.receipts), len(wants))
}

// A payment in two parts, in two blocks: the first block's receipt alone is
// short (seen), the second makes the invoice whole (confirming, then credit at
// the coin's depth), and the first part is reported again with its
// confirmations as the tip moves, without its block being read again.
func TestCryptoBitcoinScanPartialThenFull(t *testing.T) {
	node := newCryptoTestBitcoinNode(t, 975400)
	endpoint := node.serve()
	state := cryptoTestBitcoinSeams(t, map[string]int64{cryptoTestBtcP2wpkhTwice: 7})
	now := time.Unix(1794380740, 0).UTC().Add(-5 * time.Minute)
	intent := cryptoTestCoinIntent(model.CryptoAssetBtc, cryptoTestBtcP2wpkhTwice, 2_640_000)
	index := int64(7)
	intent.AddressIndex = &index
	intent.QuotedAt = now
	intent.CreateTime = now
	intent.ExpiresAt = now.Add(time.Hour)
	intents := map[int64]*model.CryptoPaymentIntent{7: intent}
	policy := cryptoTestPolicy(model.CryptoAssetBtc)
	policy.creditConfirmations = 2
	judge := func(result *cryptoTestBitcoinScanResult, at time.Time) *cryptoIntentView {
		receipts := []*model.CryptoPaymentReceipt{}
		for _, walletReceipt := range result.receipts {
			receipts = append(receipts, cryptoReceiptOf(model.CryptoAssetBtc, walletReceipt, intents, at))
		}
		return cryptoEvaluateIntent(intent, receipts, policy, at)
	}

	first := state.scan(t, endpoint)
	connect.AssertEqual(t, first.err, nil)
	connect.AssertEqual(t, len(first.receipts), 1)
	connect.AssertEqual(t, first.receipts[0].TxId, cryptoTestBtcTxSplit)
	connect.AssertEqual(t, first.receipts[0].AmountAtomic, uint64(1_320_000))
	view := judge(first, now.Add(10*time.Minute))
	connect.AssertEqual(t, view.Status, CryptoPaymentStatusSeen)
	connect.AssertEqual(t, view.RemainingAtomic, int64(1_320_000))

	node.setTip(975401)
	reads := node.callCount("getblock")
	second := state.scan(t, endpoint)
	connect.AssertEqual(t, second.err, nil)
	connect.AssertEqual(t, node.callCount("getblock"), reads+1)
	connect.AssertEqual(t, len(second.receipts), 1)
	connect.AssertEqual(t, second.receipts[0].Confirmations, uint64(2))
	connect.AssertEqual(t, judge(second, now.Add(20*time.Minute)).Status, CryptoPaymentStatusSeen)

	node.setTip(975402)
	third := state.scan(t, endpoint)
	connect.AssertEqual(t, third.err, nil)
	connect.AssertEqual(t, len(third.receipts), 2)
	connect.AssertEqual(t, cryptoTestBtcReceipt(third.receipts, cryptoTestBtcTxSplit, cryptoTestBtcP2wpkhTwice).Confirmations, uint64(3))
	connect.AssertEqual(t, cryptoTestBtcReceipt(third.receipts, cryptoTestBtcTxSecond, cryptoTestBtcP2wpkhTwice).Confirmations, uint64(1))
	view = judge(third, now.Add(30*time.Minute))
	// whole, at 1 confirmation: confirming, not yet credited at a depth of 2
	connect.AssertEqual(t, view.Status, CryptoPaymentStatusConfirming)
	connect.AssertEqual(t, view.Confirmations, 1)
	connect.AssertEqual(t, view.Action, cryptoIntentActionNone)

	node.mine()
	fourth := state.scan(t, endpoint)
	connect.AssertEqual(t, fourth.err, nil)
	view = judge(fourth, now.Add(40*time.Minute))
	connect.AssertEqual(t, view.Confirmations, 2)
	connect.AssertEqual(t, view.Action, cryptoIntentActionCredit)
	connect.AssertEqual(t, len(view.CreditReceipts), 2)
}

// Confirmations count from the block's height: the tip less the height, plus
// one, for every kept payment the scan reports (those at or above the cursor
// less the reorg window), without its block being read again; a payment the
// final depth deep is reported once at it or past it, then left behind.
func TestCryptoBitcoinScanCountsConfirmationsFromHeight(t *testing.T) {
	node := newCryptoTestBitcoinNode(t, 975400)
	endpoint := node.serve()
	state := cryptoTestBitcoinSeams(t, map[string]int64{cryptoTestBtcP2wpkhBlocks: 3})
	result := state.scan(t, endpoint)
	connect.AssertEqual(t, result.err, nil)
	reads := node.callCount("getblock")
	seen := map[uint64][]uint64{}
	for tip := uint64(975401); tip <= 975410; tip += 1 {
		if tip <= cryptoTestBtcLast {
			node.setTip(tip)
		} else {
			node.mine()
		}
		result = state.scan(t, endpoint)
		connect.AssertEqual(t, result.err, nil)
		for _, receipt := range result.receipts {
			connect.AssertEqual(t, receipt.Confirmations, tip-receipt.BlockHeight+1)
			seen[receipt.BlockHeight] = append(seen[receipt.BlockHeight], receipt.Confirmations)
		}
	}
	// one getblock per new block, none for the kept ones
	connect.AssertEqual(t, node.callCount("getblock"), reads+10)
	// each payment is reported at every depth until its block is below the
	// cursor less the window (6): 975400's at 2 to 8 (the first scan saw it at
	// 1), 975401's at 1 to 8, so each is reported past its final depth (6)
	connect.AssertEqual(t, seen[975400], []uint64{2, 3, 4, 5, 6, 7, 8})
	connect.AssertEqual(t, seen[975401], []uint64{1, 2, 3, 4, 5, 6, 7, 8})
}

// A reorg that drops a block: the next block the source has is not a child of
// the kept tip, so the scan compares the kept hashes with the chain's back to
// the fork point and reads the new blocks from there. The dropped block's
// payments are looked up again: one the new chain holds again is reported
// there, one back in the mempool is reported as seen (0 confirmations), and
// one gone from both is not reported (the scanner's vanished-receipt check
// settles it, and a credited one stays credited).
func TestCryptoBitcoinScanReorgDropsABlock(t *testing.T) {
	node := newCryptoTestBitcoinNode(t, 975402)
	endpoint := node.serve()
	state := cryptoTestBitcoinSeams(t, map[string]int64{
		cryptoTestBtcP2wpkhLarge:    21,
		cryptoTestBtcP2pkhSecond:    22,
		cryptoTestBtcP2trTwoOutputs: 23,
		cryptoTestBtcP2wpkhSingle:   24,
	})
	before := state.scan(t, endpoint)
	connect.AssertEqual(t, before.err, nil)
	connect.AssertEqual(t, len(before.receipts), 4)
	oldHash := state.kept(t).tip().Hash

	// 975402 is replaced by a block without three of its payments, and the
	// new chain is longer: one payment goes back to the mempool, one is mined
	// again in the new chain's 975404, one is gone
	node.fork(975402, "a", cryptoTestBtcTxLarge, cryptoTestBtcTxSecond, cryptoTestBtcTxTwoOutputs)
	node.stateLock.Lock()
	node.mempool[cryptoTestBtcTxLarge] = true
	node.stateLock.Unlock()
	node.mine(cryptoTestBtcTxTwoOutputs)
	hashes := node.callCount("getblockhash")
	reads := node.callCount("getblock")
	after := state.scan(t, endpoint)
	connect.AssertEqual(t, after.err, nil)
	connect.AssertEqual(t, after.walletHeight, uint64(975404))
	// 975403, which showed the fork, is read once and used again after the
	// fork point: 975402, 975403 and 975404 of the new chain, three reads
	connect.AssertEqual(t, node.callCount("getblock")-reads, 3)

	kept := state.kept(t)
	connect.AssertEqual(t, kept.tip().Height, uint64(975404))
	var replaced *bitcoinChainBlock
	for _, block := range kept.Blocks {
		if block.Height == 975402 {
			replaced = block
		}
	}
	connect.AssertEqual(t, replaced.Hash != oldHash, true)
	connect.AssertEqual(t, len(replaced.Payments), 0)
	// the fork point was found from the kept hashes, newest first: 975402
	// differs, 975401 holds
	connect.AssertEqual(t, 2 <= node.callCount("getblockhash")-hashes, true)

	// back in the mempool: reported as seen
	large := cryptoTestBtcReceipt(after.receipts, cryptoTestBtcTxLarge, cryptoTestBtcP2wpkhLarge)
	if large == nil {
		t.Fatal("the payment back in the mempool is not reported")
	}
	connect.AssertEqual(t, large.BlockHeight, uint64(0))
	connect.AssertEqual(t, large.Confirmations, uint64(0))
	connect.AssertEqual(t, large.Senders, []string{cryptoTestBtcLargeSender})
	// mined again in the new chain's 975404, at 1 confirmation
	two := cryptoTestBtcReceipt(after.receipts, cryptoTestBtcTxTwoOutputs, cryptoTestBtcP2trTwoOutputs)
	if two == nil {
		t.Fatal("the payment mined again is not reported")
	}
	connect.AssertEqual(t, two.BlockHeight, uint64(975404))
	connect.AssertEqual(t, two.Confirmations, uint64(1))
	connect.AssertEqual(t, two.AmountAtomic, uint64(546+57_857))
	// gone from the chain and the mempool: not reported
	connect.AssertEqual(t, cryptoTestBtcReceipt(after.receipts, cryptoTestBtcTxSecond, cryptoTestBtcP2pkhSecond), (*cryptoWalletReceipt)(nil))
	connect.AssertEqual(t, len(after.receipts), 3)
	// the blocks before the fork keep their payments
	single := cryptoTestBtcReceipt(after.receipts, cryptoTestBtcTxSingle, cryptoTestBtcP2wpkhSingle)
	connect.AssertEqual(t, single.BlockHeight, uint64(975401))
	connect.AssertEqual(t, single.Confirmations, uint64(4))

	// the reader keeps credits over a reorg (the reconciler ends none)
	var reader cryptoWallet = cryptoTestBitcoinWallet(endpoint)
	keeper, ok := reader.(cryptoWalletKeepsCredits)
	connect.AssertEqual(t, ok && keeper.KeepsCredits(), true)
	_, irreversible := reader.(cryptoWalletIrreversible)
	connect.AssertEqual(t, irreversible, false)
}

// Bitcoin keeps the first block it saw at a height, and so does the scan: a
// source that answers another block at the kept tip's height (a node that saw
// that one first) is not followed until its chain is the longer one.
func TestCryptoBitcoinScanWaitsForTheLongerChain(t *testing.T) {
	node := newCryptoTestBitcoinNode(t, 975402)
	endpoint := node.serve()
	state := cryptoTestBitcoinSeams(t, map[string]int64{cryptoTestBtcP2wpkhLarge: 21})
	connect.AssertEqual(t, state.scan(t, endpoint).err, nil)
	kept := state.kept(t).tip().Hash

	node.sibling("b")
	reads := node.callCount("getblock")
	result := state.scan(t, endpoint)
	connect.AssertEqual(t, result.err, nil)
	connect.AssertEqual(t, node.callCount("getblock"), reads)
	connect.AssertEqual(t, state.kept(t).tip().Hash, kept)
	connect.AssertEqual(t, len(result.receipts), 1)

	// the sibling's chain grows: it is followed, from the fork at 975401
	node.mine()
	result = state.scan(t, endpoint)
	connect.AssertEqual(t, result.err, nil)
	connect.AssertEqual(t, state.kept(t).tip().Height, uint64(975403))
	connect.AssertEqual(t, state.kept(t).Blocks[len(state.kept(t).Blocks)-2].Hash != kept, true)
	// the sibling is a copy of the block with the same payment
	connect.AssertEqual(t, cryptoTestBtcReceipt(result.receipts, cryptoTestBtcTxLarge, cryptoTestBtcP2wpkhLarge).Confirmations, uint64(2))
}

// The cursor restart: a scan on another taskworker starts from the chain the
// last complete scan kept with the cursor, reads only the blocks after it, and
// reports the kept payments with their confirmations; a scan with no new
// block makes one call, getblockchaininfo. A scan that does not complete keeps
// nothing, and the next one reads the same blocks again.
func TestCryptoBitcoinScanRestartsFromTheKeptChain(t *testing.T) {
	node := newCryptoTestBitcoinNode(t, 975401)
	endpoint := node.serve()
	state := cryptoTestBitcoinSeams(t, map[string]int64{cryptoTestBtcP2wpkhBlocks: 3, cryptoTestBtcP2wpkhTwice: 7})
	connect.AssertEqual(t, state.scan(t, endpoint).err, nil)

	node.setTip(975402)
	reads := node.callCount("getblock")
	result := state.scan(t, endpoint)
	connect.AssertEqual(t, result.err, nil)
	connect.AssertEqual(t, node.callCount("getblock"), reads+1)
	connect.AssertEqual(t, len(result.receipts), 4)
	connect.AssertEqual(t, cryptoTestBtcReceipt(result.receipts, cryptoTestBtcTxSmall, cryptoTestBtcP2wpkhBlocks).Confirmations, uint64(3))
	connect.AssertEqual(t, cryptoTestBtcReceipt(result.receipts, cryptoTestBtcTxSmallAgain, cryptoTestBtcP2wpkhBlocks).Confirmations, uint64(2))
	connect.AssertEqual(t, cryptoTestBtcReceipt(result.receipts, cryptoTestBtcTxSecond, cryptoTestBtcP2wpkhTwice).Confirmations, uint64(1))

	// nothing new: one call
	calls := node.totalCalls()
	result = state.scan(t, endpoint)
	connect.AssertEqual(t, result.err, nil)
	connect.AssertEqual(t, node.totalCalls(), calls+1)
	connect.AssertEqual(t, node.callCount("getblockchaininfo") > 0, true)
	connect.AssertEqual(t, len(result.receipts), 4)

	// a scan that does not complete (its scanner failed after the reads)
	// keeps nothing: the next reads the same block again
	node.mine()
	wallet := cryptoTestBitcoinWallet(endpoint)
	_, _, err := wallet.IncomingSince(context.Background(), 975396)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, state.chain.Height, int64(975402))
	reads = node.callCount("getblock")
	result = state.scan(t, endpoint)
	connect.AssertEqual(t, result.err, nil)
	connect.AssertEqual(t, node.callCount("getblock"), reads+1)
	connect.AssertEqual(t, state.chain.Height, int64(975403))

	// the kept chain is what the cursor row holds: it reads back, and one
	// that does not is an error, never a guess
	kept, height, err := cryptoBitcoinKeptChain(context.Background())
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, height, uint64(975403))
	connect.AssertEqual(t, kept.tip().Height, uint64(975403))
	state.chain.RecentBlocks = `{"blocks":[{"height":5,"hash":"a"},{"height":7,"hash":"b"}]}`
	_, _, err = cryptoBitcoinKeptChain(context.Background())
	connect.AssertEqual(t, err != nil, true)
	state.chain.RecentBlocks = `{"blocks":`
	_, _, err = cryptoBitcoinKeptChain(context.Background())
	connect.AssertEqual(t, err != nil, true)
}

// With many open invoices the scan still asks one lookup per block, never
// one per output or per invoice, and finds the payments among them.
func TestCryptoBitcoinScanOneLookupPerBlockWithManyOpenInvoices(t *testing.T) {
	node := newCryptoTestBitcoinNode(t, 975402)
	endpoint := node.serve()
	ours := map[string]int64{}
	for i := range 5000 {
		ours[fmt.Sprintf("bc1qsyntheticinvoice%05d", i)] = int64(1000 + i)
	}
	ours[cryptoTestBtcP2sh] = 1
	ours[cryptoTestBtcP2wpkhSingle] = 2
	ours[cryptoTestBtcP2wpkhLarge] = 3
	state := cryptoTestBitcoinSeams(t, ours)
	result := state.scan(t, endpoint)
	connect.AssertEqual(t, result.err, nil)
	connect.AssertEqual(t, len(result.receipts), 3)
	// one lookup a block read: 975396 to 975402
	connect.AssertEqual(t, node.callCount("getblock"), 7)
	connect.AssertEqual(t, state.lookupCount(), 7)
	for i, height := range []uint64{975400, 975401, 975402} {
		lookup := slices.Clone(state.lookups[4+i])
		want := cryptoTestBtcBlockAddresses(t, height)
		slices.Sort(lookup)
		slices.Sort(want)
		connect.AssertEqual(t, lookup, want)
	}
}

// A source the scan cannot trust is an error, and the url (which carries the
// key) is in none: a node behind the kept tip, a chain changed below every
// kept block, another chain, a refused key.
func TestCryptoBitcoinScanRefusesWhatItCannotTrust(t *testing.T) {
	node := newCryptoTestBitcoinNode(t, 975402)
	endpoint := node.serve()
	state := cryptoTestBitcoinSeams(t, map[string]int64{})
	connect.AssertEqual(t, state.scan(t, endpoint).err, nil)
	errorsSeen := []error{}

	// a lagging node
	node.setTip(975401)
	result := state.scan(t, endpoint)
	connect.AssertEqual(t, result.err != nil && strings.Contains(result.err.Error(), "behind block 975402"), true)
	errorsSeen = append(errorsSeen, result.err)
	node.setTip(975402)

	// every kept block changed: deeper than the scan keeps
	node.fork(975390, "deep")
	result = state.scan(t, endpoint)
	connect.AssertEqual(t, result.err != nil && strings.Contains(result.err.Error(), "below the 7 blocks the scan keeps"), true)
	errorsSeen = append(errorsSeen, result.err)

	// another chain
	node.stateLock.Lock()
	node.chainName = "test"
	node.stateLock.Unlock()
	result = state.scan(t, endpoint)
	connect.AssertEqual(t, result.err != nil && strings.Contains(result.err.Error(), "chain \"test\""), true)
	errorsSeen = append(errorsSeen, result.err)
	node.stateLock.Lock()
	node.chainName = "main"
	// a refused key
	node.refuse = true
	node.stateLock.Unlock()
	result = state.scan(t, endpoint)
	connect.AssertEqual(t, result.err != nil, true)
	errorsSeen = append(errorsSeen, result.err)
	for _, err := range errorsSeen {
		connect.AssertEqual(t, strings.Contains(err.Error(), "synthetic-key"), false)
		connect.AssertEqual(t, strings.Contains(err.Error(), endpoint), false)
	}
}

// A scanner that was down longer than the lookback passes the older blocks
// over (counted and logged) and reads from the lookback on.
func TestCryptoBitcoinScanPassesOverBlocksPastTheLookback(t *testing.T) {
	node := newCryptoTestBitcoinNode(t, 975402)
	endpoint := node.serve()
	state := cryptoTestBitcoinSeams(t, map[string]int64{})
	node.stateLock.Lock()
	old := node.syntheticBelow(975402 - 2000)
	node.stateLock.Unlock()
	encoded, _ := json.Marshal(&bitcoinChain{Blocks: []*bitcoinChainBlock{{Height: 975402 - 2000, Hash: old["hash"].(string)}}})
	state.chain = &model.CryptoScanChain{Height: 975402 - 2000, BlockHash: old["hash"].(string), RecentBlocks: string(encoded)}
	skipped := testutil.ToFloat64(cryptoPayScanBlocksSkipped.WithLabelValues(model.CryptoAssetBtc))
	result := state.scan(t, endpoint)
	connect.AssertEqual(t, result.err, nil)
	floor := uint64(975402 - cryptoBitcoinLookbackBlocks)
	connect.AssertEqual(t, testutil.ToFloat64(cryptoPayScanBlocksSkipped.WithLabelValues(model.CryptoAssetBtc))-skipped, float64(floor-(975402-2000+1)))
	kept := state.kept(t)
	connect.AssertEqual(t, kept.Blocks[0].Height, floor)
	connect.AssertEqual(t, kept.tip().Height, floor+cryptoBitcoinBlocksPerScan-1)
	// the coin is behind until it catches up: the wallet height lags the node's
	connect.AssertEqual(t, result.walletHeight < result.nodeHeight, true)
}

// A transaction read again (the screen's senders, the scanner's and the
// reconciler's re-checks): its outputs to pool addresses, its inputs'
// addresses from their prevouts, its block's height; in the mempool, no
// block; in a block the chain left and not back in the mempool, gone.
func TestCryptoBitcoinTransaction(t *testing.T) {
	node := newCryptoTestBitcoinNode(t, 975402)
	endpoint := node.serve()
	cryptoTestBitcoinSeams(t, map[string]int64{cryptoTestBtcP2wpkhTwice: 7, cryptoTestBtcP2sh: 8, cryptoTestBtcP2wpkhSingle: 9})
	wallet := cryptoTestBitcoinWallet(endpoint)
	ctx := context.Background()

	receipts, found, err := wallet.Transaction(ctx, cryptoTestBtcTxSplit)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, found, true)
	connect.AssertEqual(t, len(receipts), 2)
	connect.AssertEqual(t, receipts[0].Address, cryptoTestBtcP2wpkhTwice)
	connect.AssertEqual(t, receipts[0].AmountAtomic, uint64(1_320_000))
	connect.AssertEqual(t, receipts[1].Address, cryptoTestBtcP2sh)
	connect.AssertEqual(t, receipts[1].OutputKey, "8")
	connect.AssertEqual(t, receipts[1].AmountAtomic, uint64(700_000))
	connect.AssertEqual(t, receipts[1].BlockHeight, uint64(975400))
	connect.AssertEqual(t, receipts[1].Confirmations, uint64(3))
	connect.AssertEqual(t, receipts[1].Senders, []string{cryptoTestBtcSplitSender})

	// the height is checked against the chain's hash at it
	receipts, found, err = wallet.Transaction(ctx, cryptoTestBtcTxSingle)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, found, true)
	connect.AssertEqual(t, receipts[0].BlockHeight, uint64(975401))
	connect.AssertEqual(t, receipts[0].Senders, []string{cryptoTestBtcSingleSender})

	// in a block the chain left, and nowhere else: gone
	node.fork(975401, "c", cryptoTestBtcTxSingle)
	_, found, err = wallet.Transaction(ctx, cryptoTestBtcTxSingle)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, found, false)
	// back in the mempool: found, with no block
	node.stateLock.Lock()
	node.mempool[cryptoTestBtcTxSingle] = true
	node.stateLock.Unlock()
	receipts, found, err = wallet.Transaction(ctx, cryptoTestBtcTxSingle)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, found, true)
	connect.AssertEqual(t, receipts[0].BlockHeight, uint64(0))
	connect.AssertEqual(t, receipts[0].Confirmations, uint64(0))
	// unknown to the node
	_, found, err = wallet.Transaction(ctx, strings.Repeat("ab", 32))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, found, false)

	// a coinbase's input names no address: it cannot be screened
	coinbase := &bitcoinTx{Vin: []*bitcoinVin{{Coinbase: "0329e20e"}}}
	connect.AssertEqual(t, cryptoBitcoinSenders(coinbase), []string{""})
}

// Amounts are read as Bitcoin Core writes them, to the satoshi, never as a
// float: 8 decimals at most, no exponent, no sign, no more than 21 million.
func TestCryptoBitcoinSats(t *testing.T) {
	for _, test := range []struct {
		value string
		sats  uint64
		ok    bool
	}{
		{"0.01320000", 1_320_000, true},
		{"0.00000001", 1, true},
		{"3.14010929", 314_010_929, true},
		{"1.57049473", 157_049_473, true},
		{"0.00000000", 0, true},
		{"21000000.00000000", 2_100_000_000_000_000, true},
		{"0.0132", 1_320_000, true},
		{"7", 700_000_000, true},
		{"1e-08", 0, false},
		{"0.000000001", 0, false},
		{"-0.1", 0, false},
		{"21000000.00000001", 2_100_000_000_000_001, true},
		{"21000001", 0, false},
		{"", 0, false},
		{".5", 0, false},
	} {
		sats, err := cryptoBitcoinSats(json.Number(test.value))
		connect.AssertEqual(t, err == nil, test.ok)
		if test.ok {
			connect.AssertEqual(t, sats, test.sats)
		}
	}
}
