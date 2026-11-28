package controller

// Native SOL on the crypto payment rail (mmm/BTCPAY.md §17.3, §17.7), and the
// Solana reconcile's chain leg that closes the USDC gap of §2.5.
//
// Each SOL invoice has a Coinbase deposit address of its own. Its payments
// are read from the chain alone, through the Helius RPC (helius.yml's
// api_key), at the finalized commitment:
//
//   - the source of truth: each invoice address's signatures
//     (getSignaturesForAddress, finalized), and each new transaction
//     (getTransaction, jsonParsed): the system-program transfers (transfer and
//     transferWithSeed, top-level and inner) to the address, summed per
//     transaction. A failed transaction pays nothing. The address's balance is
//     never read: Coinbase moves funds out of its deposit addresses at any
//     time, and a sweep is a transfer from the address, never to it;
//   - the fast path: the Helius webhook's native branch (subscription_controller.go,
//     heliusHintNativeInvoices). Helius sees a per-invoice address only if the
//     address is on its webhook; a delivery that names one only brings the
//     address's next read forward, and the read credits;
//   - the scan's sweep (crypto_pay_sweep.go): every address an invoice was
//     ever given, read hourly once its invoice is out of play, from the newest
//     signature examined (SweepAddress), so a payment to an old or expired
//     invoice is recorded too.
//
// Credited at finalized (with Alpenglow, confirmed and finalized describe the
// same state), and final at once: a finalized transaction never leaves the
// chain (Irreversible). The payment's senders (the transfers' sources) are
// what the sanctions screen reads.
//
// The chain leg (reconcileSolanaChainReferences) reads, for every open USDC
// Solana Pay intent, the transactions that name its reference, and runs the
// Helius webhook's own decision on each one Helius never delivered: a lost
// delivery is credited within the hour, through the same credit, once.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Solana mainnet's genesis hash.
const solanaMainnetGenesisHash = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"

// The system program, whose transfers move native SOL.
const solanaSystemProgramId = "11111111111111111111111111111111"

// The signatures one page of getSignaturesForAddress asks for, and the pages
// one address's read takes: a fresh deposit address has a handful.
const (
	solanaSignaturesPageSize = 100
	solanaSignaturesMaxPages = 3
)

// A finalized block older than this is a stalled source: the network's height
// is unknown, and SOL is not sold until it moves again.
const solanaStaleFinalized = 5 * time.Minute

// The most parsed finalized transactions one process keeps.
const solanaParsedCacheMax = 4096

// solanaChainRpc is the Solana JSON-RPC a reader calls (cryptoRpcClient over
// Helius, with the key in its url: its errors name the method and the status,
// never the url).
type solanaChainRpc interface {
	call(ctx context.Context, method string, params any, result any) error
}

// The JSON-RPC client for an RPC url. Replaceable only by tests, which answer
// from recorded fixtures.
var solanaChainRpcFor = func(rpcUrl string) solanaChainRpc {
	return newCryptoRpcClient(rpcUrl, "", "", cryptoRpcAuthNone, "2.0")
}

// ----- the jsonParsed shapes read here -----

type solanaSignatureInfo struct {
	Signature          string `json:"signature"`
	Slot               uint64 `json:"slot"`
	Err                any    `json:"err"`
	BlockTime          *int64 `json:"blockTime"`
	ConfirmationStatus string `json:"confirmationStatus"`
}

type solanaParsedInstruction struct {
	Program   string `json:"program"`
	ProgramId string `json:"programId"`
	// an object for the parsed programs (system, spl-token), a string for
	// spl-memo, absent for the rest
	Parsed json.RawMessage `json:"parsed"`
	// an unparsed instruction's data, base58
	Data     string   `json:"data"`
	Accounts []string `json:"accounts"`
}

type solanaTokenBalance struct {
	AccountIndex  int    `json:"accountIndex"`
	Mint          string `json:"mint"`
	Owner         string `json:"owner"`
	UiTokenAmount struct {
		Amount   string `json:"amount"`
		Decimals int    `json:"decimals"`
	} `json:"uiTokenAmount"`
}

type solanaParsedTransaction struct {
	Slot      uint64 `json:"slot"`
	BlockTime *int64 `json:"blockTime"`
	Meta      *struct {
		Err               any `json:"err"`
		InnerInstructions []struct {
			Index        int                       `json:"index"`
			Instructions []solanaParsedInstruction `json:"instructions"`
		} `json:"innerInstructions"`
		PreTokenBalances  []solanaTokenBalance `json:"preTokenBalances"`
		PostTokenBalances []solanaTokenBalance `json:"postTokenBalances"`
	} `json:"meta"`
	Transaction struct {
		Signatures []string `json:"signatures"`
		Message    struct {
			AccountKeys []struct {
				Pubkey string `json:"pubkey"`
				Signer bool   `json:"signer"`
			} `json:"accountKeys"`
			Instructions []solanaParsedInstruction `json:"instructions"`
		} `json:"message"`
	} `json:"transaction"`
}

// failed reports a transaction that moved nothing (meta.err set), or one
// whose meta the RPC did not give (nothing can be read from it).
func (self *solanaParsedTransaction) failed() bool {
	return self.Meta == nil || self.Meta.Err != nil
}

// allInstructions are the top-level instructions, then the inner ones.
func (self *solanaParsedTransaction) allInstructions() []solanaParsedInstruction {
	instructions := slices.Clone(self.Transaction.Message.Instructions)
	if self.Meta != nil {
		for _, inner := range self.Meta.InnerInstructions {
			instructions = append(instructions, inner.Instructions...)
		}
	}
	return instructions
}

// solanaSystemTransfer is one system-program transfer of native SOL.
type solanaSystemTransfer struct {
	source      string
	destination string
	lamports    uint64
}

// solanaSystemTransfers are a transaction's system-program transfers
// (transfer, transferWithSeed), top-level and inner, in order.
func solanaSystemTransfers(transaction *solanaParsedTransaction) []*solanaSystemTransfer {
	transfers := []*solanaSystemTransfer{}
	for _, instruction := range transaction.allInstructions() {
		if instruction.ProgramId != solanaSystemProgramId || len(instruction.Parsed) == 0 {
			continue
		}
		var parsed struct {
			Type string `json:"type"`
			Info struct {
				Source      string `json:"source"`
				Destination string `json:"destination"`
				Lamports    uint64 `json:"lamports"`
			} `json:"info"`
		}
		if err := json.Unmarshal(instruction.Parsed, &parsed); err != nil {
			continue
		}
		if parsed.Type != "transfer" && parsed.Type != "transferWithSeed" {
			continue
		}
		if parsed.Info.Destination == "" || parsed.Info.Lamports == 0 {
			continue
		}
		transfers = append(transfers, &solanaSystemTransfer{
			source:      parsed.Info.Source,
			destination: parsed.Info.Destination,
			lamports:    parsed.Info.Lamports,
		})
	}
	return transfers
}

// solanaNativeReceipts are a finalized transaction's native SOL payments to
// our addresses, summed per address, with the transfers' sources as the
// senders. A failed transaction pays nothing.
func solanaNativeReceipts(signature string, transaction *solanaParsedTransaction, ours map[string]int64) []*cryptoWalletReceipt {
	if transaction == nil || transaction.failed() {
		return []*cryptoWalletReceipt{}
	}
	sums := map[string]*big.Int{}
	senders := map[string][]string{}
	order := []string{}
	for _, transfer := range solanaSystemTransfers(transaction) {
		if _, ok := ours[transfer.destination]; !ok {
			continue
		}
		if _, seen := sums[transfer.destination]; !seen {
			sums[transfer.destination] = new(big.Int)
			order = append(order, transfer.destination)
		}
		sums[transfer.destination].Add(sums[transfer.destination], new(big.Int).SetUint64(transfer.lamports))
		if !slices.Contains(senders[transfer.destination], transfer.source) {
			senders[transfer.destination] = append(senders[transfer.destination], transfer.source)
		}
	}
	timestamp := time.Time{}
	if transaction.BlockTime != nil && 0 < *transaction.BlockTime {
		timestamp = time.Unix(*transaction.BlockTime, 0).UTC()
	}
	receipts := []*cryptoWalletReceipt{}
	for _, address := range order {
		index := ours[address]
		receipts = append(receipts, &cryptoWalletReceipt{
			TxId:          signature,
			OutputKey:     fmt.Sprintf("%d", index),
			Address:       address,
			AddressIndex:  cryptoIndexOf(index),
			AmountAtomic:  cryptoClampUint64(sums[address]),
			BlockHeight:   transaction.Slot,
			Confirmations: 1,
			Timestamp:     timestamp,
			Senders:       senders[address],
		})
	}
	return receipts
}

// ----- the parsed transactions of this process -----

// A finalized transaction never changes, so each is fetched once per
// process.
type solanaParsedCache struct {
	stateLock    sync.Mutex
	transactions map[string]*solanaParsedTransaction
	order        []string
}

var solanaParsedTransactions = &solanaParsedCache{transactions: map[string]*solanaParsedTransaction{}}

func (self *solanaParsedCache) get(signature string) (*solanaParsedTransaction, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	transaction, ok := self.transactions[signature]
	return transaction, ok
}

func (self *solanaParsedCache) put(signature string, transaction *solanaParsedTransaction) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if _, ok := self.transactions[signature]; ok {
		return
	}
	self.transactions[signature] = transaction
	self.order = append(self.order, signature)
	for solanaParsedCacheMax < len(self.order) {
		delete(self.transactions, self.order[0])
		self.order = self.order[1:]
	}
}

// reset forgets every cached transaction (tests).
func (self *solanaParsedCache) reset() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.transactions = map[string]*solanaParsedTransaction{}
	self.order = nil
}

// solanaFinalizedTransaction reads one finalized transaction, jsonParsed,
// from the cache or the RPC. nil, nil: the RPC has no finalized transaction
// of that signature.
func solanaFinalizedTransaction(ctx context.Context, rpc solanaChainRpc, signature string) (*solanaParsedTransaction, error) {
	if transaction, ok := solanaParsedTransactions.get(signature); ok {
		return transaction, nil
	}
	var transaction *solanaParsedTransaction
	if err := rpc.call(ctx, "getTransaction", []any{
		signature,
		map[string]any{
			"encoding":                       "jsonParsed",
			"commitment":                     "finalized",
			"maxSupportedTransactionVersion": 0,
		},
	}, &transaction); err != nil {
		return nil, err
	}
	if transaction == nil {
		return nil, nil
	}
	solanaParsedTransactions.put(signature, transaction)
	return transaction, nil
}

// solanaFinalizedSignatures reads an address's finalized signatures, newest
// first, at most maxPages pages.
func solanaFinalizedSignatures(ctx context.Context, rpc solanaChainRpc, address string, maxPages int) ([]*solanaSignatureInfo, error) {
	signatures := []*solanaSignatureInfo{}
	before := ""
	for page := 0; page < maxPages; page += 1 {
		options := map[string]any{
			"commitment": "finalized",
			"limit":      solanaSignaturesPageSize,
		}
		if before != "" {
			options["before"] = before
		}
		var pageSignatures []*solanaSignatureInfo
		if err := rpc.call(ctx, "getSignaturesForAddress", []any{address, options}, &pageSignatures); err != nil {
			return nil, err
		}
		for _, signature := range pageSignatures {
			if signature != nil && signature.Signature != "" {
				signatures = append(signatures, signature)
			}
		}
		if len(pageSignatures) < solanaSignaturesPageSize {
			break
		}
		before = pageSignatures[len(pageSignatures)-1].Signature
	}
	return signatures, nil
}

// ----- the reader -----

type solanaNativeWallet struct {
	*cryptoCoinbaseAddresses
	cryptoAddressWatch
	network string
	rpc     solanaChainRpc

	stateLock       sync.Mutex
	genesisVerified bool
}

func newSolanaNativeWallet(settings *cryptoWalletSettings, assetConfig *CryptoPayAssetConfig) *solanaNativeWallet {
	return &solanaNativeWallet{
		cryptoCoinbaseAddresses: newCryptoCoinbaseAddresses(model.CryptoAssetSol, settings, cryptoSolanaAddressCanonical),
		network:                 assetConfig.Network,
		rpc:                     solanaChainRpcFor(settings.ChainRpcUrl),
	}
}

// Irreversible: only finalized transactions are read.
func (self *solanaNativeWallet) Irreversible() bool {
	return true
}

// IncomingSince reads every watched address's finalized transactions (the
// history of a deposit address is short): minHeight bounds nothing here.
func (self *solanaNativeWallet) IncomingSince(ctx context.Context, minHeight uint64) ([]*cryptoWalletReceipt, uint64, error) {
	watched := self.watchedAddresses()
	ours := map[string]int64{}
	for _, address := range watched {
		ours[address.Address] = address.AddressIndex
	}
	receipts := []*cryptoWalletReceipt{}
	seen := map[string]bool{}
	for _, address := range watched {
		signatures, err := solanaFinalizedSignatures(ctx, self.rpc, address.Address, solanaSignaturesMaxPages)
		if err != nil {
			return nil, 0, err
		}
		for _, signature := range signatures {
			if signature.Err != nil || seen[signature.Signature] {
				// a failed transaction moved nothing
				continue
			}
			seen[signature.Signature] = true
			transaction, err := solanaFinalizedTransaction(ctx, self.rpc, signature.Signature)
			if err != nil {
				return nil, 0, err
			}
			receipts = append(receipts, solanaNativeReceipts(signature.Signature, transaction, ours)...)
		}
	}
	return receipts, 0, nil
}

// Reads an issued address's finalized transactions newer than its cursor's
// signature (its whole history before the first read), oldest first, and
// reports their native SOL payments to it. The listing must reach the cursor
// within settings.MaxPages pages, or nothing of it is acted on (an older
// transaction it did not reach would be passed over for good). Each signature
// page and each transaction is a request taken from spend; when spend refuses
// one, the cursor is the last transaction examined, and the next read goes on
// from it. A transaction listed as finalized that the RPC does not serve yet
// stops the read there too (crypto_pay_sweep.go).
func (self *solanaNativeWallet) SweepAddress(
	ctx context.Context,
	address *model.CryptoAddressSweepAddress,
	settings *CryptoAddressSweepSettings,
	spend func() bool,
) ([]*cryptoWalletReceipt, *model.CryptoAddressSweepCursor, bool, error) {
	cursor := address.Cursor
	signatures := []*solanaSignatureInfo{}
	listed := false
	beforeSignature := ""
	for page := 0; page < settings.MaxPages; page += 1 {
		if !spend() {
			return nil, nil, false, nil
		}
		options := map[string]any{
			"commitment": "finalized",
			"limit":      settings.PageSize,
		}
		if cursor != nil && cursor.Signature != "" {
			options["until"] = cursor.Signature
		}
		if beforeSignature != "" {
			options["before"] = beforeSignature
		}
		var pageSignatures []*solanaSignatureInfo
		if err := self.rpc.call(ctx, "getSignaturesForAddress", []any{address.Address, options}, &pageSignatures); err != nil {
			return nil, nil, false, fmt.Errorf("getSignaturesForAddress: %w", err)
		}
		for _, signature := range pageSignatures {
			if signature != nil && signature.Signature != "" {
				signatures = append(signatures, signature)
			}
		}
		if len(pageSignatures) < settings.PageSize {
			listed = true
			break
		}
		beforeSignature = pageSignatures[len(pageSignatures)-1].Signature
	}
	if !listed {
		return nil, nil, false, fmt.Errorf("more than %d pages of history since the cursor; raise the sweep's page limit", settings.MaxPages)
	}
	slices.Reverse(signatures)

	ours := map[string]int64{address.Address: address.AddressIndex}
	receipts := []*cryptoWalletReceipt{}
	var next *model.CryptoAddressSweepCursor
	if cursor != nil {
		copied := *cursor
		next = &copied
	}
	for _, signature := range signatures {
		if signature.Err == nil {
			if !spend() {
				return receipts, next, false, nil
			}
			transaction, err := solanaFinalizedTransaction(ctx, self.rpc, signature.Signature)
			if err != nil {
				// what was read before it is kept
				return receipts, next, false, fmt.Errorf("getTransaction: %w", err)
			}
			if transaction == nil {
				// listed as finalized but not served yet: read again next scan
				return receipts, next, false, nil
			}
			receipts = append(receipts, solanaNativeReceipts(signature.Signature, transaction, ours)...)
		}
		// a failed transaction moved nothing
		slot := int64(min(signature.Slot, 1<<62))
		next = &model.CryptoAddressSweepCursor{Signature: signature.Signature, Height: &slot}
	}
	if next == nil {
		// an address with no history yet: read again from its start
		next = &model.CryptoAddressSweepCursor{}
	}
	return receipts, next, true, nil
}

// Transaction reads one transaction again and reports its payments to any
// address the pool holds.
func (self *solanaNativeWallet) Transaction(ctx context.Context, txId string) ([]*cryptoWalletReceipt, bool, error) {
	transaction, err := solanaFinalizedTransaction(ctx, self.rpc, txId)
	if err != nil {
		return nil, false, err
	}
	if transaction == nil {
		return nil, false, nil
	}
	destinations := []string{}
	for _, transfer := range solanaSystemTransfers(transaction) {
		if !slices.Contains(destinations, transfer.destination) {
			destinations = append(destinations, transfer.destination)
		}
	}
	ours := cryptoResolveAddresses(ctx, model.CryptoAssetSol, destinations)
	return solanaNativeReceipts(txId, transaction, ours), true, nil
}

// Heights: the wallet's and the node's height are the finalized slot, the
// network's the confirmed slot. A finalized block older than
// solanaStaleFinalized is an unknown network height.
func (self *solanaNativeWallet) Heights(ctx context.Context) (uint64, uint64, uint64, error) {
	var finalized uint64
	if err := self.rpc.call(ctx, "getSlot", []any{map[string]any{"commitment": "finalized"}}, &finalized); err != nil {
		return 0, 0, 0, err
	}
	var confirmed uint64
	if err := self.rpc.call(ctx, "getSlot", []any{map[string]any{"commitment": "confirmed"}}, &confirmed); err != nil {
		return 0, 0, 0, err
	}
	networkHeight := max(finalized, confirmed)
	var blockTime *int64
	if err := self.rpc.call(ctx, "getBlockTime", []any{finalized}, &blockTime); err != nil || blockTime == nil ||
		solanaStaleFinalized < time.Since(time.Unix(*blockTime, 0)) {
		networkHeight = 0
	}
	return finalized, finalized, networkHeight, nil
}

// CheckNetwork refuses an RPC of another chain than mainnet's.
func (self *solanaNativeWallet) CheckNetwork(ctx context.Context) error {
	self.stateLock.Lock()
	verified := self.genesisVerified
	self.stateLock.Unlock()
	if verified {
		return nil
	}
	if self.network != cryptoNetworkMainnet {
		return fmt.Errorf("native SOL is received on mainnet only, the coin's network is %s", self.network)
	}
	var genesis string
	if err := self.rpc.call(ctx, "getGenesisHash", []any{}, &genesis); err != nil {
		return err
	}
	if genesis != solanaMainnetGenesisHash {
		return fmt.Errorf("the Solana source serves another chain than mainnet")
	}
	self.stateLock.Lock()
	self.genesisVerified = true
	self.stateLock.Unlock()
	return nil
}

// ----- a jsonParsed transaction as the Helius webhook's decision reads it -----

// A finalized jsonParsed transaction in the shape of a Helius enhanced
// transaction, as far as solanaDecidePayment reads one:
//
//   - Type TRANSFER when a USDC token account owned by one of our receivers
//     gained USDC, else UNKNOWN (the decision skips it);
//   - one USDC token transfer per receiver account that gained, its owner as
//     the receiver and the UI amount as the gain, from the transaction's own
//     pre and post token balances (a transfer, a transferChecked, a CPI from a
//     smart wallet alike), the sender as the owner whose USDC fell;
//   - the token account that paid each of those, when one instruction names
//     it, and that account's other USDC transfers in the transaction
//     (solanaParsedNameUsdcSources), as Helius names them;
//   - the account keys as the account data (a Solana Pay reference is one),
//     and the memo instructions with their text as the data (solanaMemoTexts
//     keeps a text verbatim), top-level and inner;
//   - the signature, the slot and the block time.
func solanaTransactionFromParsed(signature string, parsed *solanaParsedTransaction) *SolanaTransaction {
	transaction := &SolanaTransaction{
		Signature:      signature,
		Slot:           int64(parsed.Slot),
		Type:           "UNKNOWN",
		TokenTransfers: []TokenTransfer{},
		AccountData:    []AccountData{},
		Instructions:   []Instruction{},
	}
	if parsed.BlockTime != nil {
		transaction.Timestamp = *parsed.BlockTime
	}
	if parsed.Meta != nil {
		transaction.TransactionError = parsed.Meta.Err
	}
	keys := []string{}
	for _, key := range parsed.Transaction.Message.AccountKeys {
		keys = append(keys, key.Pubkey)
		transaction.AccountData = append(transaction.AccountData, AccountData{Account: key.Pubkey})
	}
	if 0 < len(keys) {
		transaction.FeePayer = keys[0]
	}
	memoInstruction := func(instruction solanaParsedInstruction) (Instruction, bool) {
		if !slices.Contains(solanaMemoProgramIds, instruction.ProgramId) {
			return Instruction{}, false
		}
		data := instruction.Data
		var text string
		if 0 < len(instruction.Parsed) && json.Unmarshal(instruction.Parsed, &text) == nil {
			data = text
		}
		return Instruction{ProgramId: instruction.ProgramId, Data: data}, data != ""
	}
	for index, instruction := range parsed.Transaction.Message.Instructions {
		converted := Instruction{ProgramId: instruction.ProgramId, Accounts: instruction.Accounts, Data: instruction.Data}
		if memo, ok := memoInstruction(instruction); ok {
			converted = memo
		}
		if parsed.Meta != nil {
			for _, inner := range parsed.Meta.InnerInstructions {
				if inner.Index != index {
					continue
				}
				for _, innerInstruction := range inner.Instructions {
					if memo, ok := memoInstruction(innerInstruction); ok {
						converted.InnerInstructions = append(converted.InnerInstructions, InnerInstruction{ProgramId: memo.ProgramId, Data: memo.Data})
					}
				}
			}
		}
		transaction.Instructions = append(transaction.Instructions, converted)
	}
	if parsed.failed() {
		return transaction
	}

	// USDC balances per token account: before and after
	type usdcAccount struct {
		owner  string
		before *big.Int
		after  *big.Int
	}
	accounts := map[int]*usdcAccount{}
	account := func(index int, owner string) *usdcAccount {
		found, ok := accounts[index]
		if !ok {
			found = &usdcAccount{owner: owner, before: new(big.Int), after: new(big.Int)}
			accounts[index] = found
		}
		if found.owner == "" {
			found.owner = owner
		}
		return found
	}
	for _, balance := range parsed.Meta.PreTokenBalances {
		if balance.Mint != solanaUsdcMint {
			continue
		}
		if amount, ok := new(big.Int).SetString(balance.UiTokenAmount.Amount, 10); ok {
			account(balance.AccountIndex, balance.Owner).before = amount
		}
	}
	for _, balance := range parsed.Meta.PostTokenBalances {
		if balance.Mint != solanaUsdcMint {
			continue
		}
		if amount, ok := new(big.Int).SetString(balance.UiTokenAmount.Amount, 10); ok {
			account(balance.AccountIndex, balance.Owner).after = amount
		}
	}
	// the sender: the owner whose USDC fell the most
	sender := ""
	senderFall := new(big.Int)
	indexes := []int{}
	for index := range accounts {
		indexes = append(indexes, index)
	}
	slices.Sort(indexes)
	for _, index := range indexes {
		found := accounts[index]
		fall := new(big.Int).Sub(found.before, found.after)
		if 0 < fall.Sign() && senderFall.Cmp(fall) < 0 {
			sender = found.owner
			senderFall = fall
		}
	}
	for _, index := range indexes {
		found := accounts[index]
		gain := new(big.Int).Sub(found.after, found.before)
		if gain.Sign() <= 0 || !slices.Contains(solanaReceiverAddresses, found.owner) {
			continue
		}
		tokenAccount := ""
		if index < len(keys) {
			tokenAccount = keys[index]
		}
		// the UI amount, from its exact decimal (USDC has 6 decimals)
		uiAmount := 0.0
		if gain.IsInt64() {
			uiAmount, _ = strconv.ParseFloat(cryptoFormatAtomic(gain.Int64(), 6), 64)
		}
		transaction.TokenTransfers = append(transaction.TokenTransfers, TokenTransfer{
			FromUserAccount: sender,
			Mint:            solanaUsdcMint,
			ToTokenAccount:  tokenAccount,
			ToUserAccount:   found.owner,
			TokenAmount:     uiAmount,
			TokenStandard:   "Fungible",
		})
		transaction.Type = "TRANSFER"
	}
	solanaParsedNameUsdcSources(transaction, parsed, keys)
	return transaction
}

// The token programs whose transfers move USDC.
var solanaTokenProgramIds = []string{
	"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA",
	"TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb",
}

// One transfer of USDC between token accounts an instruction of a parsed
// transaction made, top-level or inner.
type solanaParsedUsdcTransfer struct {
	source      string
	destination string
	// micro-USDC
	amount int64
}

// A parsed transaction's token program transfers of USDC (transfer,
// transferChecked), top-level and inner, in order, with the owners of the USDC
// token accounts the transaction's token balances name.
func solanaParsedUsdcTransfers(parsed *solanaParsedTransaction, keys []string) (transfers []*solanaParsedUsdcTransfer, owners map[string]string) {
	transfers = []*solanaParsedUsdcTransfer{}
	owners = map[string]string{}
	if parsed.Meta == nil {
		return
	}
	for _, balances := range [][]solanaTokenBalance{parsed.Meta.PreTokenBalances, parsed.Meta.PostTokenBalances} {
		for _, balance := range balances {
			if balance.Mint == solanaUsdcMint && 0 <= balance.AccountIndex && balance.AccountIndex < len(keys) && owners[keys[balance.AccountIndex]] == "" {
				owners[keys[balance.AccountIndex]] = balance.Owner
			}
		}
	}
	for _, instruction := range parsed.allInstructions() {
		if !slices.Contains(solanaTokenProgramIds, instruction.ProgramId) || len(instruction.Parsed) == 0 {
			continue
		}
		var parsedInstruction struct {
			Type string `json:"type"`
			Info struct {
				Source      string `json:"source"`
				Destination string `json:"destination"`
				Mint        string `json:"mint"`
				Amount      string `json:"amount"`
				TokenAmount struct {
					Amount string `json:"amount"`
				} `json:"tokenAmount"`
			} `json:"info"`
		}
		if err := json.Unmarshal(instruction.Parsed, &parsedInstruction); err != nil {
			continue
		}
		info := parsedInstruction.Info
		amountText := info.Amount
		switch parsedInstruction.Type {
		case "transferChecked":
			if info.Mint != solanaUsdcMint {
				continue
			}
			amountText = info.TokenAmount.Amount
		case "transfer":
			// a plain transfer names no mint: both accounts must hold USDC
			_, sourceUsdc := owners[info.Source]
			_, destinationUsdc := owners[info.Destination]
			if !sourceUsdc || !destinationUsdc {
				continue
			}
		default:
			continue
		}
		amount, err := strconv.ParseInt(amountText, 10, 64)
		if err != nil || amount <= 0 || info.Source == "" || info.Destination == "" {
			continue
		}
		transfers = append(transfers, &solanaParsedUsdcTransfer{source: info.Source, destination: info.Destination, amount: amount})
	}
	return
}

// Names, as Helius's enhanced transactions do, the token account that paid
// each transfer into one of our receivers (FromTokenAccount, when one
// instruction transfer names it), and adds that account's other USDC transfers
// in the transaction (a Solana Pay app's fee) as transfers to their own
// owners: a Shopify order's payment is recognized by all its source sent out
// (solana_shopify_order.go). The transfers into our receivers, which every
// decision reads, are unchanged.
func solanaParsedNameUsdcSources(transaction *SolanaTransaction, parsed *solanaParsedTransaction, keys []string) {
	instructionTransfers, owners := solanaParsedUsdcTransfers(parsed, keys)
	receiverTokenAccounts := []string{}
	sources := []string{}
	for i := range transaction.TokenTransfers {
		receiverTransfer := &transaction.TokenTransfers[i]
		receiverTokenAccounts = append(receiverTokenAccounts, receiverTransfer.ToTokenAccount)
		named := []string{}
		for _, instructionTransfer := range instructionTransfers {
			if instructionTransfer.destination == receiverTransfer.ToTokenAccount && !slices.Contains(named, instructionTransfer.source) {
				named = append(named, instructionTransfer.source)
			}
		}
		if len(named) == 1 {
			receiverTransfer.FromTokenAccount = named[0]
			if !slices.Contains(sources, named[0]) {
				sources = append(sources, named[0])
			}
		}
	}
	for _, instructionTransfer := range instructionTransfers {
		owner := owners[instructionTransfer.destination]
		if !slices.Contains(sources, instructionTransfer.source) ||
			slices.Contains(receiverTokenAccounts, instructionTransfer.destination) ||
			slices.Contains(solanaReceiverAddresses, owner) {
			continue
		}
		uiAmount, _ := strconv.ParseFloat(cryptoFormatAtomic(instructionTransfer.amount, 6), 64)
		transaction.TokenTransfers = append(transaction.TokenTransfers, TokenTransfer{
			FromTokenAccount: instructionTransfer.source,
			FromUserAccount:  owners[instructionTransfer.source],
			Mint:             solanaUsdcMint,
			ToTokenAccount:   instructionTransfer.destination,
			ToUserAccount:    owner,
			TokenAmount:      uiAmount,
			TokenStandard:    "Fungible",
		})
	}
}

// ----- the chain leg of the Solana reconcile -----

// Intents made in the last few minutes are left to their webhook.
const solanaChainLegMinAge = 10 * time.Minute

// Intents that expired more than a day ago are not read again.
const solanaChainLegMaxExpiredAge = 24 * time.Hour

// The most intents one run reads, and the signatures per reference.
const (
	solanaChainLegMaxIntents     = 200
	solanaChainLegMaxSignatures  = 20
	solanaChainLegReferencePages = 1
)

// The open references' read. Replaceable only by tests.
var solanaChainOpenReferences = func(connOwner server.PgConn, ctx context.Context, createdBefore time.Time, expiresAfter time.Time, limit int) []string {
	return model.ListOpenSolanaPaymentReferencesInConn(connOwner, ctx, createdBefore, expiresAfter, limit)
}

// Whether a signature is already credited or recorded. Replaceable only by
// tests.
var solanaChainSignatureHandled = func(connOwner server.PgConn, ctx context.Context, signature string) bool {
	return model.IsSolanaPaymentCompletedInConn(connOwner, ctx, signature) ||
		model.IsUnfulfilledSolanaPaymentRecordedInConn(connOwner, ctx, signature)
}

// The decision's reads, on the run's session. Replaceable only by tests.
var solanaChainLookupsFor = func(run *paymentReconcileRun) solanaPaymentLookups {
	return &solanaReconcileLookups{connOwner: run.connOwner, clientSession: run.clientSession}
}

// The credit and the unfulfilled record, as the webhook makes them.
// Replaceable only by tests.
var (
	solanaChainCredit     = solanaCreditPaymentIntentInConn
	solanaChainRecord     = model.RecordUnfulfilledSolanaPaymentInConn
	solanaChainOnboarding = solanaRecordOnboarding
)

// The Helius RPC url, when helius.yml names a key. Replaceable only by tests.
var solanaChainRpcUrl = func() (string, bool) {
	if !solanaReconcileHasCredentials() {
		return "", false
	}
	return solanaRpcUrlFunc(), true
}

// solanaReconcileLookups is solanaDbPaymentLookups on the reconcile's session.
type solanaReconcileLookups struct {
	connOwner     server.PgConn
	clientSession *session.ClientSession
}

// The open intent one of references names, on the run's session.
func (self *solanaReconcileLookups) searchIntentsByReference(references []string) (*model.PaymentIntentSearchResult, error) {
	return model.SearchPaymentIntentsInConn(self.connOwner, references, self.clientSession)
}

// Whether the signature consumed an intent, on the run's session.
func (self *solanaReconcileLookups) isPaymentCompleted(signature string) bool {
	return model.IsSolanaPaymentCompletedInConn(self.connOwner, self.clientSession.Ctx, signature)
}

// Every intent quoted the amount, on the run's session: the run holds its
// connection for its whole pass, so a second one is never taken.
func (self *solanaReconcileLookups) intentsByAmountMicro(amountMicro int64, minExpiresAt time.Time) ([]*model.SolanaPaymentIntent, error) {
	return model.ListSolanaPaymentIntentsByAmountMicroInConn(self.connOwner, self.clientSession.Ctx, amountMicro, minExpiresAt)
}

// reconcileSolanaChainReferences is leg 3 of the Solana reconcile: for every
// open or recently expired USDC Solana Pay intent, the finalized transactions
// that name its reference, each one the webhook never credited or recorded
// put through the webhook's own decision and acted on as the webhook acts: a
// credit through the intent's one-shot (a linked crypto invoice's too), an
// unfulfilled record for an operator, or nothing. A dry run writes nothing
// but its would_credit events. complete = false when the budget ran out.
func reconcileSolanaChainReferences(run *paymentReconcileRun) (bool, error) {
	ctx := run.clientSession.Ctx
	store := model.SubscriptionMarketSolana
	rpcUrl, ok := solanaChainRpcUrl()
	if !ok {
		return true, nil
	}
	rpc := solanaChainRpcFor(rpcUrl)
	references := solanaChainOpenReferences(
		run.connOwner,
		ctx,
		run.now.Add(-solanaChainLegMinAge),
		run.now.Add(-solanaChainLegMaxExpiredAge),
		solanaChainLegMaxIntents,
	)
	lookups := solanaChainLookupsFor(run)
	read := 0
	for _, reference := range references {
		if !run.spend(store) {
			return false, nil
		}
		signatures, err := solanaFinalizedSignatures(ctx, rpc, reference, solanaChainLegReferencePages)
		if err != nil {
			return false, fmt.Errorf("getSignaturesForAddress: %w", err)
		}
		for i, signature := range signatures {
			if solanaChainLegMaxSignatures <= i {
				break
			}
			if signature.Err != nil || solanaChainSignatureHandled(run.connOwner, ctx, signature.Signature) {
				continue
			}
			if !run.spend(store) {
				return false, nil
			}
			parsed, err := solanaFinalizedTransaction(ctx, rpc, signature.Signature)
			if err != nil {
				return false, fmt.Errorf("getTransaction: %w", err)
			}
			if parsed == nil {
				continue
			}
			read += 1
			run.examine(store)
			solanaChainAct(run, store, signature.Signature, solanaDecidePayment(solanaTransactionFromParsed(signature.Signature, parsed), lookups), solanaChainLegName)
		}
	}
	run.storeDetail(store)["chain_references"] = len(references)
	run.storeDetail(store)["chain_transactions"] = read
	return true, nil
}

// The legs that act on a webhook decision, as their events name them.
const (
	// leg 3: the transactions that name an open intent's reference
	solanaChainLegName = "chain"
	// the receiver leg: every transaction of our receivers' USDC token
	// accounts (solana_receiver_sweep.go)
	solanaReceiverLegName = "receiver"
)

// Carries out one decision as the webhook does: a credit only through the
// intent's one-shot (solanaChainCredit), an unfulfilled record for an
// operator, or nothing. ok = false when it failed and recorded an error event,
// so the receiver leg reads the transaction again next run.
func solanaChainAct(run *paymentReconcileRun, store string, signature string, decision *solanaPaymentDecision, leg string) (ok bool) {
	ctx := run.clientSession.Ctx
	switch decision.action {
	case solanaPaymentActionError:
		message := "a payment lookup failed"
		if decision.err != nil {
			message = decision.err.Error()
		}
		run.record(store, model.PaymentReconcileActionError, nil, signature, map[string]any{
			"error": message,
			"leg":   leg,
		})
		return false
	case solanaPaymentActionRecord:
		if run.dryRun {
			return true
		}
		if err := solanaChainRecord(run.connOwner, ctx, decision.unfulfilled); err != nil {
			run.record(store, model.PaymentReconcileActionError, nil, signature, map[string]any{
				"error": err.Error(),
				"leg":   leg,
			})
			return false
		}
		glog.Infof("[reconcile]solana: a payment no webhook delivered bought nothing; recorded unfulfilled by the %s leg (%s)\n", leg, decision.skipMessage)
	case solanaPaymentActionCredit:
		details := map[string]any{"reference": decision.intent.PaymentReference, "leg": leg}
		if run.dryRun {
			run.record(store, model.PaymentReconcileActionWouldCredit, decision.intent.NetworkId, signature, details)
			return true
		}
		credited, err := solanaChainCredit(run.connOwner, run.clientSession, decision.intent, signature, decision.tokenAmountUsd)
		if err != nil {
			run.record(store, model.PaymentReconcileActionError, decision.intent.NetworkId, signature, map[string]any{
				"error": err.Error(),
				"leg":   leg,
			})
			return false
		}
		if credited {
			solanaChainOnboarding(run.clientSession, decision.intent)
			run.record(store, model.PaymentReconcileActionCredited, decision.intent.NetworkId, signature, details)
			glog.Infof("[reconcile]solana: credited reference %s from the chain (%s leg); its webhook delivery was lost\n", decision.intent.PaymentReference, leg)
		}
	}
	return true
}
