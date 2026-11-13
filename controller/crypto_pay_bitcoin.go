package controller

// Bitcoin on the crypto payment rail (mmm/BTCPAY.md §16.7, §17.3, §17.7): each
// invoice has a Coinbase deposit address of its own, and payments are read
// from the chain alone, by scanning every block through Alchemy's Bitcoin
// JSON-RPC, which Alchemy's free tier serves. The owner decided on 2026-11-11
// not to buy Alchemy's UTXO API add-on ($100 a month), so nothing here reads
// an address's history:
//
//   - the tip: getblockchaininfo, once a scan;
//   - each new block: getblockhash, then getblock at verbosity 2 (every
//     transaction with its outputs' addresses and values). The addresses a
//     block's outputs pay are matched against every address an invoice was
//     ever given, open or closed, credited, voided or past the retention
//     unlink, in one indexed lookup per block (cryptoBitcoinInvoiceAddresses),
//     never one per output or per invoice. An invoice made after the block
//     was mined is not matched in it. A payment to an old or expired invoice
//     is so recorded too, and judged by the late-payment rules
//     (cryptoEvaluateIntent): credited where they credit, else held for an
//     operator;
//   - one transaction again (the sanctions screen's senders, from its inputs'
//     prevouts; the scanner's and the reconciler's re-checks):
//     getrawtransaction at verbosity 2.
//
// The scan follows the tip from a persisted cursor: the recent blocks it read,
// with their hashes and what each paid the invoice addresses, are kept with the
// coin's cursor (crypto_scan_cursor.recent_blocks), written with the cursor
// only when a scan completes, so the next scan, on whichever taskworker it
// runs, starts from what the last complete one saw. Confirmations count from
// the block's height: the tip less the height, plus one.
//
// Reorgs: a new block that does not extend the kept chain makes the scan
// compare the kept hashes with the chain's, newest first, back to the fork
// point, and read the blocks after it again. A payment the dropped blocks
// held that the new ones do not is looked up again: mined elsewhere it is
// reported there, back in the mempool it is reported as seen (so an invoice
// not yet credited goes back to waiting), and gone from both it is left to the
// scanner's vanished-receipt check, which orphans it only while its invoice
// was not credited (crypto_pay_scan.go). A credited payment stays credited
// whatever the reorg (cryptoWalletKeepsCredits): no credit is ever taken back.
// Bitcoin keeps the first block it saw at a height, and so does the scan: a
// source that answers another block at the kept tip's height is a node that
// saw the other one first, and the scan waits for the longer chain.
//
// No mempool: Alchemy's free tier has no address-indexed mempool read, and
// reading every new mempool transaction (getrawmempool, then each new txid)
// would cost about 240 million compute units a month, eight times the free
// tier's 30 million. An invoice goes from pending to confirming, or paid, when
// its payment is mined: credited at 1 confirmation, final at 6. A payment is
// first seen at its block's time, so one sent before its quote expired is
// often mined after it: it counts as sent within the quote, at the quoted
// price, while its block is within cryptoBitcoinConfirmationGrace (3 hours)
// of the expiry, and only one mined later is valued as late, at its block's
// time (mmm/BTCPAY.md §16.8).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server/model"
)

// The best block older than this is a stalled source: its height is unknown.
const cryptoBitcoinStaleTip = 3 * time.Hour

// The most blocks one scan reads (a scanner that was down catches up this
// many a scan; the coin is not sold until it has).
const cryptoBitcoinBlocksPerScan = 10

// The recent blocks kept with the cursor, for the fork point of a reorg
// (about four hours).
const cryptoBitcoinKeptBlocks = 24

// The furthest behind the tip a block is read (a week): a scanner down longer
// passes the older blocks over, counted in
// urnetwork_crypto_pay_scan_blocks_skipped_total and logged, and a payment in
// them is found in the Coinbase account by support.
const cryptoBitcoinLookbackBlocks = 7 * 144

// A block's time may be this far from when it was mined (consensus allows two
// hours ahead, and a time as early as the median of the last eleven blocks).
const cryptoBitcoinClockTolerance = 3 * time.Hour

// Bitcoin Core's RPC_INVALID_ADDRESS_OR_KEY: getrawtransaction's "No such
// mempool or blockchain transaction".
const cryptoBitcoinRpcNotFound = -5

// cryptoBitcoinConfirmationGrace is how long past its quote's expiry a Bitcoin
// payment may be mined and still count as sent within the quote: credited at
// the quoted price, with the invoice open (pending, or seen while short) until
// then (cryptoAssetConfirmationGrace, mmm/BTCPAY.md §16.8). The scan sees a
// payment only once it is mined, never in the mempool, so it cannot tell when
// it was sent; a payment of the quoted amount whose block is at most this long
// past expires_at is taken as sent in time. The quote's window to send is the
// rail's quote expiry (30 minutes), unchanged: expires_at stays the time to
// send by.
//
// The grace is the time a payment sent at the very last second of the window
// needs to be mined. With blocks a Poisson process of mean μ, a payment whose
// fee gets it into the k-th block after it is broadcast misses a grace g with
// probability P(Poisson(g/μ) ≤ k-1). At 3 hours:
//
//	mined within     μ = 10 min   12.5 min   15 min   20 min
//	the next block   1.5e-8       5.6e-7     6.1e-6   1.2e-4
//	2 blocks         2.9e-7       8.6e-6     8.0e-5   1.2e-3
//	3 blocks         2.8e-6       6.6e-5     5.2e-4   6.2e-3
//	6 blocks         3.2e-4       4.2e-3     2.0e-2   1.2e-1
//
// so under 1e-4 of the payments sent at the end of the window miss it when
// their fee confirms them within 3 blocks while blocks run a fifth slow
// (hashrate 20% short of its difficulty, as in a curtailment: 6.6e-5), within
// 2 blocks while blocks run a third slow (8.0e-5), or in the next block while
// they run at half speed (1.2e-4). Two hours would miss 3-block inclusion at
// 10-minute blocks 5.2e-4 of the time (1-block 6.1e-6); 3.5 hours would also
// cover 3 blocks a third slow (9.4e-5). A payment sent earlier in the window
// has the rest of the window on top: sent at a uniform time in it, 3-block
// inclusion at 12.5-minute blocks misses 2.8e-5. The margin also absorbs a
// few minutes of block-time skew and of the scan (the first-seen time is the
// block's, never after the scan that read it): with 5 minutes of it gone, the
// design point still misses 9.4e-5.
//
// The price the quote holds is exposed for 3.5 hours instead of 30 minutes.
// At 50% a year Bitcoin's price moves about 1.0% (one standard deviation) in
// 3.5 hours: an honest payer pays at a random point of that, which costs
// nothing on average, and a payer who waits for a fall before sending gains an
// option worth about 0.4% of the price, under the 0.5% buffer every quote
// carries; the $100 invoice cap bounds even a 10% crash at $10 an invoice.
// The buffer and the 99.5% underpaid tolerance (judged in BTC, so the grace
// does not move it) stay as they are.
const cryptoBitcoinConfirmationGrace = 3 * time.Hour

type bitcoinWallet struct {
	*cryptoCoinbaseAddresses
	network string
	// the blocks a first scan reads back from the tip
	reorgWindow uint64
	rpc         *cryptoRpcClient

	stateLock sync.Mutex
	// the scan's view of the tip, read once
	info *bitcoinChainInfo
	// the chain as this scan left it, written with the cursor when the scan
	// completes; nil until IncomingSince read it
	chain *bitcoinChain
	// the blocks this scan read, by hash: only what the kept chain keeps of
	// each, never its transactions
	read map[string]*bitcoinReadBlock
}

// bitcoinReadBlock is a block as one scan read it: what the kept chain keeps
// of it, and its parent's hash.
type bitcoinReadBlock struct {
	kept     *bitcoinChainBlock
	previous string
}

func newBitcoinWallet(settings *cryptoWalletSettings, assetConfig *CryptoPayAssetConfig) *bitcoinWallet {
	return &bitcoinWallet{
		cryptoCoinbaseAddresses: newCryptoCoinbaseAddresses(model.CryptoAssetBtc, settings, cryptoBitcoinAddressCanonical),
		network:                 assetConfig.Network,
		reorgWindow:             uint64(max(1, assetConfig.ReorgWindowBlocks)),
		rpc:                     newCryptoRpcClient(settings.ChainRpcUrl, "", "", cryptoRpcAuthNone, "2.0"),
		read:                    map[string]*bitcoinReadBlock{},
	}
}

// KeepsCredits: a credited Bitcoin payment is never taken back, whatever the
// chain does after (mmm/BTCPAY.md §16.7).
func (self *bitcoinWallet) KeepsCredits() bool {
	return true
}

// ----- Bitcoin Core's answers (the fields read here) -----

type bitcoinChainInfo struct {
	Chain                string `json:"chain"`
	Blocks               uint64 `json:"blocks"`
	Headers              uint64 `json:"headers"`
	BestBlockHash        string `json:"bestblockhash"`
	InitialBlockDownload bool   `json:"initialblockdownload"`
	// the best block's time
	Time int64 `json:"time"`
}

type bitcoinScriptPubKey struct {
	// a standard output's address; "" for an output without one (OP_RETURN,
	// bare multisig, a public key)
	Address string `json:"address"`
	Type    string `json:"type"`
}

type bitcoinVout struct {
	// BTC, with 8 decimals, as Bitcoin Core writes it
	Value        json.Number         `json:"value"`
	N            uint32              `json:"n"`
	ScriptPubKey bitcoinScriptPubKey `json:"scriptPubKey"`
}

type bitcoinVin struct {
	// a coinbase input's script; "" for a spend
	Coinbase string `json:"coinbase"`
	// the output it spends (getrawtransaction at verbosity 2)
	Prevout *struct {
		ScriptPubKey bitcoinScriptPubKey `json:"scriptPubKey"`
	} `json:"prevout"`
}

type bitcoinTx struct {
	Txid string         `json:"txid"`
	Vin  []*bitcoinVin  `json:"vin"`
	Vout []*bitcoinVout `json:"vout"`
	// getrawtransaction: the block that holds it ("" in the mempool), and its
	// confirmations (0 for a block the chain left)
	BlockHash     string `json:"blockhash"`
	Confirmations int64  `json:"confirmations"`
	BlockTime     int64  `json:"blocktime"`
}

type bitcoinBlock struct {
	Hash              string       `json:"hash"`
	Height            uint64       `json:"height"`
	PreviousBlockHash string       `json:"previousblockhash"`
	Time              int64        `json:"time"`
	Tx                []*bitcoinTx `json:"tx"`
}

// ----- the kept chain -----

// bitcoinChain is the recent blocks the scan read, oldest first, each the
// parent of the next.
type bitcoinChain struct {
	Blocks []*bitcoinChainBlock `json:"blocks"`
}

type bitcoinChainBlock struct {
	Height uint64 `json:"height"`
	Hash   string `json:"hash"`
	// unix seconds
	Time int64 `json:"time"`
	// what the block paid the invoice addresses when it was read
	Payments []*bitcoinChainPayment `json:"payments,omitempty"`
}

// bitcoinChainPayment is one transaction's outputs to one invoice address,
// summed.
type bitcoinChainPayment struct {
	TxId         string `json:"txid"`
	Address      string `json:"address"`
	AddressIndex int64  `json:"address_index"`
	// satoshis
	Amount uint64 `json:"amount"`
}

func (self *bitcoinChainPayment) outputKey() string {
	return strconv.FormatInt(self.AddressIndex, 10)
}

func (self *bitcoinChain) tip() *bitcoinChainBlock {
	if len(self.Blocks) == 0 {
		return nil
	}
	return self.Blocks[len(self.Blocks)-1]
}

// dropAbove removes the blocks above height and returns them.
func (self *bitcoinChain) dropAbove(height uint64) []*bitcoinChainBlock {
	for i, block := range self.Blocks {
		if height < block.Height {
			dropped := slices.Clone(self.Blocks[i:])
			self.Blocks = self.Blocks[:i]
			return dropped
		}
	}
	return []*bitcoinChainBlock{}
}

// keepRecent drops all but the newest n blocks.
func (self *bitcoinChain) keepRecent(n int) {
	if n < len(self.Blocks) {
		self.Blocks = slices.Clone(self.Blocks[len(self.Blocks)-n:])
	}
}

// The kept chain's read. Replaceable only by tests.
var cryptoBitcoinLoadChain = func(ctx context.Context) *model.CryptoScanChain {
	return model.GetCryptoScanChainInConn(nil, ctx, model.CryptoAssetBtc)
}

// cryptoBitcoinKeptChain is the chain the last complete scan left, and the
// cursor's height (0 before the first scan). A kept chain that does not read
// is an error: the scan stops rather than guess where it was.
func cryptoBitcoinKeptChain(ctx context.Context) (*bitcoinChain, uint64, error) {
	stored := cryptoBitcoinLoadChain(ctx)
	chain := &bitcoinChain{}
	if stored == nil {
		return chain, 0, nil
	}
	if stored.RecentBlocks != "" {
		if err := json.Unmarshal([]byte(stored.RecentBlocks), chain); err != nil {
			return nil, 0, fmt.Errorf("the kept Bitcoin chain (crypto_scan_cursor.recent_blocks) does not read: %w", err)
		}
		for i := 1; i < len(chain.Blocks); i += 1 {
			if chain.Blocks[i].Height != chain.Blocks[i-1].Height+1 {
				return nil, 0, errors.New("the kept Bitcoin chain (crypto_scan_cursor.recent_blocks) is not contiguous")
			}
		}
	}
	return chain, uint64(max(0, stored.Height)), nil
}

// ScanChain is the chain this scan leaves, for the cursor's write: nil when
// IncomingSince did not run.
func (self *bitcoinWallet) ScanChain() *model.CryptoScanChain {
	self.stateLock.Lock()
	chain := self.chain
	self.stateLock.Unlock()
	if chain == nil || chain.tip() == nil {
		return nil
	}
	encoded, err := json.Marshal(chain)
	if err != nil {
		return nil
	}
	tip := chain.tip()
	return &model.CryptoScanChain{Height: int64(tip.Height), BlockHash: tip.Hash, RecentBlocks: string(encoded)}
}

// ----- amounts and addresses -----

// cryptoBitcoinSats reads an amount as Bitcoin Core writes it (BTC, at most 8
// decimals, never an exponent or a sign) in satoshis, exactly.
func cryptoBitcoinSats(value json.Number) (uint64, error) {
	text := strings.TrimSpace(value.String())
	whole, fraction, _ := strings.Cut(text, ".")
	digits := func(s string) bool {
		for _, r := range s {
			if r < '0' || '9' < r {
				return false
			}
		}
		return true
	}
	if whole == "" || 8 < len(whole) || 8 < len(fraction) || !digits(whole) || !digits(fraction) {
		return 0, fmt.Errorf("not a Bitcoin amount: %q", text)
	}
	coins, err := strconv.ParseUint(whole, 10, 64)
	if err != nil || 21_000_000 < coins {
		return 0, fmt.Errorf("not a Bitcoin amount: %q", text)
	}
	sats, _ := strconv.ParseUint((fraction + "00000000")[:8], 10, 64)
	return coins*100_000_000 + sats, nil
}

// cryptoBitcoinOutputAddress is an output's address in its canonical form, or
// ok = false for an output that has none.
func cryptoBitcoinOutputAddress(vout *bitcoinVout) (string, bool) {
	if vout == nil || vout.ScriptPubKey.Address == "" {
		return "", false
	}
	address, err := cryptoBitcoinAddressCanonical(vout.ScriptPubKey.Address)
	if err != nil {
		return "", false
	}
	return address, true
}

// cryptoBitcoinTxPayments sums a transaction's outputs to each of ours, in
// output order.
func cryptoBitcoinTxPayments(tx *bitcoinTx, ours map[string]int64) ([]*bitcoinChainPayment, error) {
	payments := []*bitcoinChainPayment{}
	byAddress := map[string]*bitcoinChainPayment{}
	for _, vout := range tx.Vout {
		address, ok := cryptoBitcoinOutputAddress(vout)
		if !ok {
			continue
		}
		index, mine := ours[address]
		if !mine {
			continue
		}
		sats, err := cryptoBitcoinSats(vout.Value)
		if err != nil {
			return nil, fmt.Errorf("transaction %s output %d: %w", tx.Txid, vout.N, err)
		}
		if sats == 0 {
			continue
		}
		payment, seen := byAddress[address]
		if !seen {
			payment = &bitcoinChainPayment{TxId: tx.Txid, Address: address, AddressIndex: index}
			byAddress[address] = payment
			payments = append(payments, payment)
		}
		payment.Amount = min(payment.Amount+sats, 1<<62)
	}
	return payments, nil
}

// cryptoBitcoinSenders are a transaction's input addresses, for the
// sanctions screen: "" for an input that names none (a coinbase, a bare
// multisig or public key output, or no prevout), which cannot be screened.
func cryptoBitcoinSenders(tx *bitcoinTx) []string {
	senders := []string{}
	for _, vin := range tx.Vin {
		sender := ""
		if vin != nil && vin.Coinbase == "" && vin.Prevout != nil {
			sender = strings.TrimSpace(vin.Prevout.ScriptPubKey.Address)
		}
		senders = append(senders, sender)
	}
	return senders
}

// ----- the source -----

// The invoice addresses among a block's output addresses: every address an
// invoice was ever given (open or closed, credited, voided or past the
// retention unlink) by an invoice made before the block was mined, in one
// indexed lookup a block. Replaceable only by tests.
var cryptoBitcoinInvoiceAddresses = func(ctx context.Context, addresses []string, blockTime time.Time) map[string]int64 {
	// an invoice made after the block was mined could not have been paid in it
	createdBefore := blockTime.Add(cryptoBitcoinClockTolerance)
	indexes := map[string]int64{}
	for address, issuedAddress := range model.GetCryptoPaymentIssuedAddressesInConn(nil, ctx, model.CryptoAssetBtc, addresses) {
		if issuedAddress.CreateTime.Before(createdBefore) {
			indexes[address] = issuedAddress.AddressIndex
		}
	}
	return indexes
}

func (self *bitcoinWallet) chainInfo(ctx context.Context) (*bitcoinChainInfo, error) {
	self.stateLock.Lock()
	info := self.info
	self.stateLock.Unlock()
	if info != nil {
		return info, nil
	}
	info = &bitcoinChainInfo{}
	if err := self.rpc.call(ctx, "getblockchaininfo", []any{}, info); err != nil {
		return nil, err
	}
	self.stateLock.Lock()
	self.info = info
	self.stateLock.Unlock()
	return info, nil
}

func (self *bitcoinWallet) blockHash(ctx context.Context, height uint64) (string, error) {
	var hash string
	if err := self.rpc.call(ctx, "getblockhash", []any{height}, &hash); err != nil {
		return "", err
	}
	if len(hash) != 64 {
		return "", fmt.Errorf("getblockhash %d: not a block hash", height)
	}
	return hash, nil
}

// readBlock reads the best chain's block at height, at most once a scan: its
// transactions are matched as soon as they are read, and only what the kept
// chain keeps of the block is held after (one block's transactions in memory
// at a time).
func (self *bitcoinWallet) readBlock(ctx context.Context, height uint64) (*bitcoinReadBlock, error) {
	hash, err := self.blockHash(ctx, height)
	if err != nil {
		return nil, err
	}
	self.stateLock.Lock()
	read := self.read[hash]
	self.stateLock.Unlock()
	if read != nil {
		return read, nil
	}
	var block *bitcoinBlock
	if err := self.rpc.call(ctx, "getblock", []any{hash, 2}, &block); err != nil {
		return nil, err
	}
	if block == nil || block.Hash != hash || block.Height != height {
		return nil, fmt.Errorf("getblock %d: the source answered another block than the one asked", height)
	}
	payments, err := self.blockPayments(ctx, block)
	if err != nil {
		return nil, err
	}
	read = &bitcoinReadBlock{
		kept:     &bitcoinChainBlock{Height: block.Height, Hash: block.Hash, Time: block.Time, Payments: payments},
		previous: block.PreviousBlockHash,
	}
	self.stateLock.Lock()
	self.read[hash] = read
	self.stateLock.Unlock()
	return read, nil
}

// blockPayments reads what a block paid the invoice addresses: every
// address its outputs pay, matched in one lookup.
func (self *bitcoinWallet) blockPayments(ctx context.Context, block *bitcoinBlock) ([]*bitcoinChainPayment, error) {
	addresses := []string{}
	seen := map[string]bool{}
	for _, tx := range block.Tx {
		for _, vout := range tx.Vout {
			if address, ok := cryptoBitcoinOutputAddress(vout); ok && !seen[address] {
				seen[address] = true
				addresses = append(addresses, address)
			}
		}
	}
	ours := cryptoBitcoinInvoiceAddresses(ctx, addresses, time.Unix(block.Time, 0).UTC())
	payments := []*bitcoinChainPayment{}
	if len(ours) == 0 {
		return payments, nil
	}
	for _, tx := range block.Tx {
		txPayments, err := cryptoBitcoinTxPayments(tx, ours)
		if err != nil {
			return nil, fmt.Errorf("block %d: %w", block.Height, err)
		}
		payments = append(payments, txPayments...)
	}
	return payments, nil
}

// forkPoint is the newest kept block the source's chain still holds: the
// kept hashes are compared with the chain's, newest first, at most down to
// the oldest kept block.
func (self *bitcoinWallet) forkPoint(ctx context.Context, chain *bitcoinChain, tip uint64) (uint64, error) {
	kept := chain.tip()
	for i := len(chain.Blocks) - 1; 0 <= i; i -= 1 {
		block := chain.Blocks[i]
		if tip < block.Height {
			continue
		}
		hash, err := self.blockHash(ctx, block.Height)
		if err != nil {
			return 0, err
		}
		if hash != block.Hash {
			continue
		}
		if block.Height == kept.Height {
			// the chain holds the kept tip, yet its next block is another's
			// child: nodes behind the source on different chains
			return 0, fmt.Errorf("the Bitcoin source's block %d is not a child of its block %d: its nodes answer from different chains", kept.Height+1, kept.Height)
		}
		return block.Height, nil
	}
	return 0, fmt.Errorf(
		"the Bitcoin chain changed below the %d blocks the scan keeps (from %d): a reorg deeper than that, or a source on another chain; nothing is read until an operator checks the chain",
		len(chain.Blocks), chain.Blocks[0].Height,
	)
}

// ----- the scan -----

func (self *bitcoinWallet) IncomingSince(ctx context.Context, minHeight uint64) ([]*cryptoWalletReceipt, uint64, error) {
	info, err := self.chainInfo(ctx)
	if err != nil {
		return nil, 0, err
	}
	if info.InitialBlockDownload {
		return nil, 0, errors.New("the Bitcoin source is still in its initial block download")
	}
	chain, cursorHeight, err := cryptoBitcoinKeptChain(ctx)
	if err != nil {
		return nil, 0, err
	}
	tip := info.Blocks

	// The next block is the kept tip's child. With nothing new, or another
	// block at the kept tip's height (a node that saw that one first), there
	// is none to read yet: the scan waits for the longer chain, as Bitcoin
	// does.
	var start uint64
	switch kept := chain.tip(); {
	case kept == nil:
		// a first scan reads back the reorg window, so the chain it keeps has
		// a fork point for a shallow reorg from the start; a cursor without a
		// kept chain reads from its own height back
		from := tip
		if 0 < cursorHeight && cursorHeight < tip {
			from = cursorHeight
		}
		start = max(1, from-min(from, self.reorgWindow))
	case tip < kept.Height:
		return nil, 0, fmt.Errorf("the Bitcoin source is at block %d, behind block %d the scan read: a node that lags; nothing is read until it catches up", tip, kept.Height)
	case cryptoBitcoinLookbackBlocks < tip-kept.Height:
		// down for longer than the lookback: the kept chain is too old to
		// follow
		start = kept.Height + 1
		chain = &bitcoinChain{}
	}
	if chain.tip() == nil && cryptoBitcoinLookbackBlocks < tip-min(tip, start) {
		floor := tip - cryptoBitcoinLookbackBlocks
		cryptoBitcoinSkipped(start, floor)
		start = floor
	}

	dropped := []*bitcoinChainBlock{}
	reorged := false
	for read := 0; read < cryptoBitcoinBlocksPerScan; read += 1 {
		height := start
		if kept := chain.tip(); kept != nil {
			height = kept.Height + 1
		}
		if tip < height {
			break
		}
		read, err := self.readBlock(ctx, height)
		if err != nil {
			return nil, 0, err
		}
		if kept := chain.tip(); kept != nil && read.previous != kept.Hash {
			if reorged {
				// the chain moved again during this scan: what was read
				// holds, and the next scan goes on from it
				break
			}
			reorged = true
			fork, err := self.forkPoint(ctx, chain, tip)
			if err != nil {
				return nil, 0, err
			}
			left := chain.dropAbove(fork)
			dropped = append(dropped, left...)
			glog.Warningf("[cryptopay]btc: a reorg replaced blocks %d to %d; the scan reads them again from the fork at %d\n", left[0].Height, left[len(left)-1].Height, fork)
			continue
		}
		chain.Blocks = append(chain.Blocks, read.kept)
	}
	chain.keepRecent(cryptoBitcoinKeptBlocks)
	if chain.tip() == nil {
		return nil, 0, errors.New("the Bitcoin source answered no block")
	}

	// every payment of the kept blocks at or above minHeight, with its
	// confirmations now
	receipts := []*cryptoWalletReceipt{}
	held := map[string]bool{}
	confirmationsAt := func(height uint64) uint64 {
		return max(tip, chain.tip().Height) - height + 1
	}
	for _, block := range chain.Blocks {
		for _, payment := range block.Payments {
			held[payment.TxId+"/"+payment.outputKey()] = true
			if block.Height < minHeight {
				continue
			}
			receipts = append(receipts, &cryptoWalletReceipt{
				TxId:          payment.TxId,
				OutputKey:     payment.outputKey(),
				Address:       payment.Address,
				AddressIndex:  cryptoIndexOf(payment.AddressIndex),
				AmountAtomic:  payment.Amount,
				BlockHeight:   block.Height,
				Confirmations: confirmationsAt(block.Height),
				Timestamp:     time.Unix(block.Time, 0).UTC(),
			})
		}
	}
	// a payment of a block the reorg replaced, which the new blocks do not
	// hold: where the chain has it now. Gone from the chain and the mempool,
	// it is not reported, and the scanner's vanished-receipt check settles it.
	for _, block := range dropped {
		for _, payment := range block.Payments {
			if held[payment.TxId+"/"+payment.outputKey()] {
				continue
			}
			again, found, err := self.Transaction(ctx, payment.TxId)
			if err != nil {
				return nil, 0, err
			}
			if !found {
				glog.Infof("[cryptopay]btc: a payment of a replaced block is in neither the new chain nor the mempool\n")
				continue
			}
			for _, receipt := range again {
				if receipt.OutputKey == payment.outputKey() {
					receipts = append(receipts, receipt)
				}
			}
		}
	}

	self.stateLock.Lock()
	self.chain = chain
	self.stateLock.Unlock()
	return receipts, chain.tip().Height, nil
}

// cryptoBitcoinSkipped counts and logs the blocks [from, to) a scan passes
// over: older than the lookback.
func cryptoBitcoinSkipped(from uint64, to uint64) {
	if to <= from {
		return
	}
	cryptoPayScanBlocksSkipped.WithLabelValues(model.CryptoAssetBtc).Add(float64(to - from))
	glog.Errorf("[cryptopay]btc: %d blocks are past the scan's lookback and were not read (from %d to %d)\n", to-from, from, to-1)
}

// Transaction reads one transaction again: its outputs to pool addresses as
// receipts (summed per address), with its inputs as the senders. found =
// false when it is neither in the best chain nor in the mempool (a
// transaction whose block the chain left, and that is not back in the
// mempool, is gone).
func (self *bitcoinWallet) Transaction(ctx context.Context, txId string) ([]*cryptoWalletReceipt, bool, error) {
	var tx *bitcoinTx
	err := self.rpc.call(ctx, "getrawtransaction", []any{txId, 2}, &tx)
	var rpcErr *cryptoRpcError
	if errors.As(err, &rpcErr) && rpcErr.Code == cryptoBitcoinRpcNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if tx == nil || tx.Txid == "" {
		return nil, false, nil
	}
	if tx.Txid != txId {
		return nil, false, errors.New("getrawtransaction: the source answered another transaction than the one asked")
	}
	var height uint64
	var confirmations uint64
	timestamp := time.Now().UTC()
	if tx.BlockHash != "" {
		if tx.Confirmations <= 0 {
			return nil, false, nil
		}
		confirmations = uint64(tx.Confirmations)
		if height, err = self.heightOf(ctx, tx.BlockHash, confirmations); err != nil {
			return nil, false, err
		}
		if 0 < tx.BlockTime {
			timestamp = time.Unix(tx.BlockTime, 0).UTC()
		}
	}
	addresses := []string{}
	for _, vout := range tx.Vout {
		if address, ok := cryptoBitcoinOutputAddress(vout); ok && !slices.Contains(addresses, address) {
			addresses = append(addresses, address)
		}
	}
	payments, err := cryptoBitcoinTxPayments(tx, cryptoResolveAddresses(ctx, model.CryptoAssetBtc, addresses))
	if err != nil {
		return nil, false, err
	}
	senders := cryptoBitcoinSenders(tx)
	receipts := []*cryptoWalletReceipt{}
	for _, payment := range payments {
		receipts = append(receipts, &cryptoWalletReceipt{
			TxId:          payment.TxId,
			OutputKey:     payment.outputKey(),
			Address:       payment.Address,
			AddressIndex:  cryptoIndexOf(payment.AddressIndex),
			AmountAtomic:  payment.Amount,
			BlockHeight:   height,
			Confirmations: confirmations,
			Senders:       slices.Clone(senders),
			Timestamp:     timestamp,
		})
	}
	return receipts, true, nil
}

// heightOf is the height of a best-chain block a transaction's answer names,
// with its confirmations: the kept chain's, or the tip's less them, checked
// against the chain's hash at that height (a block may land between the two
// reads, or another node may answer).
func (self *bitcoinWallet) heightOf(ctx context.Context, blockHash string, confirmations uint64) (uint64, error) {
	self.stateLock.Lock()
	chain := self.chain
	self.stateLock.Unlock()
	if chain != nil {
		for _, block := range chain.Blocks {
			if block.Hash == blockHash {
				return block.Height, nil
			}
		}
	}
	var count uint64
	if err := self.rpc.call(ctx, "getblockcount", []any{}, &count); err != nil {
		return 0, err
	}
	if count+1 < confirmations {
		return 0, errors.New("the Bitcoin source counts more confirmations than blocks")
	}
	guess := count + 1 - confirmations
	candidates := []uint64{guess}
	if 1 < guess {
		candidates = append(candidates, guess-1)
	}
	candidates = append(candidates, guess+1)
	for _, height := range candidates {
		if height == 0 {
			continue
		}
		hash, err := self.blockHash(ctx, height)
		if err != nil {
			return 0, err
		}
		if hash == blockHash {
			return height, nil
		}
	}
	return 0, errors.New("the Bitcoin source's heights for a transaction's block disagree; retried next scan")
}

// Heights: the wallet's height is the last block the scan read, the node's
// the source's best block, the network's its best header. A best block older
// than cryptoBitcoinStaleTip, or a node still syncing, is an unknown network
// height: the coin is not sold until it moves again.
func (self *bitcoinWallet) Heights(ctx context.Context) (uint64, uint64, uint64, error) {
	info, err := self.chainInfo(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	walletHeight := info.Blocks
	self.stateLock.Lock()
	if self.chain != nil && self.chain.tip() != nil {
		walletHeight = self.chain.tip().Height
	}
	self.stateLock.Unlock()
	networkHeight := max(info.Blocks, info.Headers)
	if info.InitialBlockDownload || (0 < info.Time && cryptoBitcoinStaleTip < cryptoChainNow().Sub(time.Unix(info.Time, 0))) {
		networkHeight = 0
	}
	return walletHeight, info.Blocks, networkHeight, nil
}

// CheckNetwork refuses a node of another chain than the coin's.
func (self *bitcoinWallet) CheckNetwork(ctx context.Context) error {
	info, err := self.chainInfo(ctx)
	if err != nil {
		return err
	}
	want := map[string]string{cryptoNetworkMainnet: "main", cryptoNetworkTestnet: "test", cryptoNetworkRegtest: "regtest"}[self.network]
	if info.Chain != want {
		return fmt.Errorf("the Bitcoin source serves chain %q, the coin's network is %s", info.Chain, self.network)
	}
	return nil
}
