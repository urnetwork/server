package controller

// Native SOL on the rail, the Helius native branch and the Solana reconcile's
// chain leg, on recorded fixtures (testdata/crypto_pay/sol_*.json: public
// mainnet transactions of an exchange hot wallet, read from
// api.mainnet-beta.solana.com on 2026-11-10; the Helius payload is the
// documented enhanced shape of the same native transfer). Hermetic: the RPC,
// the database, Redis and the credit are seams.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The fixtures' accounts and signatures.
const (
	// the exchange hot wallet the fixtures were read for: it receives the
	// native transfer, and sends the USDC
	solanaFixtureWallet = "5tzFkiKscXHK5ZXCGbXZxdw7gTjjD1mBwuoFbhUvuAi9"
	// the native transfer's source
	solanaFixtureSender = "mveQ2LeVFwW3kN9Hc7PAwB5rwdUymZm8GdzRRQDQg1u"
	// the native transfer: 108,600 lamports to the wallet, slot 467670518
	solanaFixtureNativeSignature = "4stgejbJ2bV2B2ypq9d4oxM8TwFsyeUVNwRj5ZRBdFGNxvGNBsgWPK7FFpnZhoQq3y81q173xJgcPVNJMyR7JQ4U"
	// the USDC transfer: from the wallet to the owner below
	solanaFixtureUsdcSignature = "2cHjqh6JDhfnCQdhXMq8r1uBciSWJ3sXtci35caxst3Kdpv8FWx7n2fAuwwHfbC27VBAdMdANjXex6BxiBX2bQaj"
	// the USDC transfer's receiving owner, our receiver in these tests
	solanaFixtureUsdcReceiver = "H8BgJgae6qhMtf7BM2JtddywSQt11WdxHHxkGLNX5hss"
	// an account key of the USDC transfer (its durable nonce account), the
	// reference the tests' intent is found by
	solanaFixtureUsdcReference = "7B1tVMUE6zGyPFsx6fun6VoX1yCwfeX6D1r1CM3L1fct"
	// what the USDC transfer moved: 732,773,872.399 USDC
	solanaFixtureUsdcAmount = 732773872.399
)

func solanaTestFixture(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/crypto_pay/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func solanaTestParsedTransaction(t testing.TB, name string) *solanaParsedTransaction {
	t.Helper()
	var transaction *solanaParsedTransaction
	if err := json.Unmarshal(solanaTestFixture(t, name), &transaction); err != nil {
		t.Fatal(err)
	}
	return transaction
}

// fakeSolanaRpc answers the reader's calls from fixtures and counts them.
type fakeSolanaRpc struct {
	t            testing.TB
	signatures   map[string]string
	transactions map[string]any
	finalized    uint64
	confirmed    uint64
	blockTime    int64
	genesis      string
	err          error
	calls        map[string]int
}

func newFakeSolanaRpc(t testing.TB) *fakeSolanaRpc {
	return &fakeSolanaRpc{
		t:            t,
		signatures:   map[string]string{},
		transactions: map[string]any{},
		genesis:      solanaMainnetGenesisHash,
		calls:        map[string]int{},
	}
}

func (self *fakeSolanaRpc) answer(value any, result any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, result)
}

func (self *fakeSolanaRpc) call(ctx context.Context, method string, params any, result any) error {
	self.calls[method] += 1
	if self.err != nil {
		return self.err
	}
	args, _ := params.([]any)
	switch method {
	case "getSignaturesForAddress":
		address, _ := args[0].(string)
		options, _ := args[1].(map[string]any)
		connect.AssertEqual(self.t, options["commitment"], "finalized")
		if fixture, ok := self.signatures[address]; ok {
			return json.Unmarshal(solanaTestFixture(self.t, fixture), result)
		}
		return self.answer([]any{}, result)
	case "getTransaction":
		signature, _ := args[0].(string)
		options, _ := args[1].(map[string]any)
		connect.AssertEqual(self.t, options["encoding"], "jsonParsed")
		connect.AssertEqual(self.t, options["commitment"], "finalized")
		switch transaction := self.transactions[signature].(type) {
		case string:
			return json.Unmarshal(solanaTestFixture(self.t, transaction), result)
		case nil:
			return self.answer(nil, result)
		default:
			return self.answer(transaction, result)
		}
	case "getSlot":
		options, _ := args[0].(map[string]any)
		if options["commitment"] == "finalized" {
			return self.answer(self.finalized, result)
		}
		return self.answer(self.confirmed, result)
	case "getBlockTime":
		return self.answer(self.blockTime, result)
	case "getGenesisHash":
		return self.answer(self.genesis, result)
	}
	return fmt.Errorf("unexpected method %s", method)
}

func newSolanaTestWallet(t testing.TB, rpc *fakeSolanaRpc) *solanaNativeWallet {
	solanaParsedTransactions.reset()
	t.Cleanup(solanaParsedTransactions.reset)
	return &solanaNativeWallet{
		cryptoCoinbaseAddresses: newCryptoCoinbaseAddresses(model.CryptoAssetSol, nil, cryptoSolanaAddressCanonical),
		network:                 cryptoNetworkMainnet,
		rpc:                     rpc,
	}
}

// A native SOL transfer to an invoice address is a receipt at its finalized
// slot, with its source as the sender. A failed signature is never fetched, a
// transaction of the address that pays it nothing (the wallet sending USDC)
// is no receipt, and a second read takes the transactions from the cache.
func TestSolanaNativeReaderReadsTransfersToInvoiceAddresses(t *testing.T) {
	rpc := newFakeSolanaRpc(t)
	rpc.signatures[solanaFixtureWallet] = "sol_signatures.json"
	rpc.transactions[solanaFixtureNativeSignature] = "sol_tx_native.json"
	rpc.transactions[solanaFixtureUsdcSignature] = "sol_tx_usdc.json"
	wallet := newSolanaTestWallet(t, rpc)
	wallet.SetWatched([]*model.CryptoPaymentWatchedAddress{{Address: solanaFixtureWallet, AddressIndex: 7, Hot: true}})

	receipts, _, err := wallet.IncomingSince(context.Background(), 0)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(receipts), 1)
	receipt := receipts[0]
	connect.AssertEqual(t, receipt.TxId, solanaFixtureNativeSignature)
	connect.AssertEqual(t, receipt.OutputKey, "7")
	connect.AssertEqual(t, receipt.Address, solanaFixtureWallet)
	connect.AssertEqual(t, *receipt.AddressIndex, uint64(7))
	connect.AssertEqual(t, receipt.AmountAtomic, uint64(108600))
	connect.AssertEqual(t, receipt.BlockHeight, uint64(467670518))
	connect.AssertEqual(t, receipt.Confirmations, uint64(1))
	connect.AssertEqual(t, receipt.Timestamp, time.Unix(1794302994, 0).UTC())
	connect.AssertEqual(t, receipt.Senders, []string{solanaFixtureSender})
	// the failed signature (57i2325s…) was never fetched
	connect.AssertEqual(t, rpc.calls["getTransaction"], 2)

	// finalized transactions are fetched once per process
	receipts, _, err = wallet.IncomingSince(context.Background(), 0)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(receipts), 1)
	connect.AssertEqual(t, rpc.calls["getTransaction"], 2)
	connect.AssertEqual(t, wallet.Irreversible(), true)

	// nothing watched: nothing read
	wallet.SetWatched(nil)
	receipts, _, err = wallet.IncomingSince(context.Background(), 0)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(receipts), 0)

	// an RPC failure is the scan's failure, and names no url
	rpc.err = errors.New("getSignaturesForAddress: http status 429")
	wallet.SetWatched([]*model.CryptoPaymentWatchedAddress{{Address: solanaFixtureWallet, AddressIndex: 7, Hot: true}})
	solanaParsedTransactions.reset()
	_, _, err = wallet.IncomingSince(context.Background(), 0)
	connect.AssertEqual(t, err != nil, true)
}

// A failed transaction pays nothing, and a transfer made by a program (an
// inner instruction of a smart wallet's call) pays like a top-level one.
func TestSolanaNativeReceiptsSkipFailedAndReadInnerTransfers(t *testing.T) {
	ours := map[string]int64{solanaFixtureWallet: 3}

	failed := solanaTestParsedTransaction(t, "sol_tx_native.json")
	failed.Meta.Err = map[string]any{"InstructionError": []any{2, "Custom"}}
	connect.AssertEqual(t, len(solanaNativeReceipts(solanaFixtureNativeSignature, failed, ours)), 0)

	inner := solanaTestParsedTransaction(t, "sol_tx_native.json")
	transfer := inner.Transaction.Message.Instructions[2]
	inner.Transaction.Message.Instructions = inner.Transaction.Message.Instructions[:2]
	inner.Meta.InnerInstructions = append(inner.Meta.InnerInstructions, struct {
		Index        int                       `json:"index"`
		Instructions []solanaParsedInstruction `json:"instructions"`
	}{Index: 1, Instructions: []solanaParsedInstruction{transfer, transfer}})
	receipts := solanaNativeReceipts(solanaFixtureNativeSignature, inner, ours)
	connect.AssertEqual(t, len(receipts), 1)
	// two inner transfers to the address in one transaction sum
	connect.AssertEqual(t, receipts[0].AmountAtomic, uint64(2*108600))
	connect.AssertEqual(t, receipts[0].OutputKey, "3")

	// a transfer to an address that is not ours is no receipt
	connect.AssertEqual(t, len(solanaNativeReceipts(solanaFixtureNativeSignature, solanaTestParsedTransaction(t, "sol_tx_native.json"), map[string]int64{})), 0)
}

// Transaction reads one transaction again and reports what it paid any
// address the pool holds; a transaction the RPC does not have is not found.
func TestSolanaNativeTransactionResolvesPoolAddresses(t *testing.T) {
	restore := cryptoTestHermeticSeams()
	t.Cleanup(restore)
	cryptoResolveAddresses = func(ctx context.Context, asset string, addresses []string) map[string]int64 {
		connect.AssertEqual(t, asset, model.CryptoAssetSol)
		resolved := map[string]int64{}
		if slices.Contains(addresses, solanaFixtureWallet) {
			resolved[solanaFixtureWallet] = 11
		}
		return resolved
	}
	rpc := newFakeSolanaRpc(t)
	rpc.transactions[solanaFixtureNativeSignature] = "sol_tx_native.json"
	wallet := newSolanaTestWallet(t, rpc)

	receipts, found, err := wallet.Transaction(context.Background(), solanaFixtureNativeSignature)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, found, true)
	connect.AssertEqual(t, len(receipts), 1)
	connect.AssertEqual(t, receipts[0].OutputKey, "11")
	connect.AssertEqual(t, receipts[0].Senders, []string{solanaFixtureSender})

	receipts, found, err = wallet.Transaction(context.Background(), "unknownsignature")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, found, false)
	connect.AssertEqual(t, len(receipts), 0)
}

// The heights: finalized for the wallet and the node, confirmed for the
// network; a finalized block older than five minutes is an unknown network
// height. Only mainnet's genesis passes the network check.
func TestSolanaNativeHeightsAndNetwork(t *testing.T) {
	rpc := newFakeSolanaRpc(t)
	rpc.finalized = 467670518
	rpc.confirmed = 467670550
	rpc.blockTime = time.Now().Add(-10 * time.Second).Unix()
	wallet := newSolanaTestWallet(t, rpc)

	walletHeight, nodeHeight, networkHeight, err := wallet.Heights(context.Background())
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, walletHeight, uint64(467670518))
	connect.AssertEqual(t, nodeHeight, uint64(467670518))
	connect.AssertEqual(t, networkHeight, uint64(467670550))

	rpc.blockTime = time.Now().Add(-6 * time.Minute).Unix()
	_, _, networkHeight, err = wallet.Heights(context.Background())
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, networkHeight, uint64(0))

	connect.AssertEqual(t, wallet.CheckNetwork(context.Background()), nil)
	other := newSolanaTestWallet(t, rpc)
	rpc.genesis = "EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG"
	connect.AssertEqual(t, other.CheckNetwork(context.Background()) != nil, true)
	devnet := newSolanaTestWallet(t, rpc)
	devnet.network = cryptoNetworkTestnet
	connect.AssertEqual(t, devnet.CheckNetwork(context.Background()) != nil, true)
}

// A finalized USDC transfer read from the chain feeds the Helius webhook's
// own decision: TRANSFER, the receiving owner and the amount from the
// transaction's token balances, the sender as the owner whose USDC fell, and
// every account key a reference can be found under. A transfer to an owner
// that is not our receiver is skipped, as Helius's would be.
func TestSolanaParsedUsdcTransferFeedsTheWebhookDecision(t *testing.T) {
	previousReceivers := solanaReceiverAddresses
	solanaReceiverAddresses = []string{solanaFixtureUsdcReceiver}
	t.Cleanup(func() { solanaReceiverAddresses = previousReceivers })
	parsed := solanaTestParsedTransaction(t, "sol_tx_usdc.json")

	transaction := solanaTransactionFromParsed(solanaFixtureUsdcSignature, parsed)
	connect.AssertEqual(t, transaction.Type, "TRANSFER")
	connect.AssertEqual(t, transaction.Signature, solanaFixtureUsdcSignature)
	connect.AssertEqual(t, transaction.Timestamp, int64(1793314949))
	connect.AssertEqual(t, len(transaction.TokenTransfers), 1)
	transfer := transaction.TokenTransfers[0]
	connect.AssertEqual(t, transfer.Mint, solanaUsdcMint)
	connect.AssertEqual(t, transfer.ToUserAccount, solanaFixtureUsdcReceiver)
	connect.AssertEqual(t, transfer.FromUserAccount, solanaFixtureWallet)
	connect.AssertEqual(t, transfer.TokenAmount, solanaFixtureUsdcAmount)
	connect.AssertEqual(t, slices.Contains(solanaReferenceCandidates(transaction), solanaFixtureUsdcReference), true)

	lookups := &fakeSolanaChainLookups{intents: map[string]*model.PaymentIntentSearchResult{}}
	networkId := server.NewId()
	lookups.intents[solanaFixtureUsdcReference] = &model.PaymentIntentSearchResult{
		NetworkId:         &networkId,
		PaymentReference:  solanaFixtureUsdcReference,
		ExpectedAmountUsd: solanaFixtureUsdcAmount,
		SubscriptionPlan:  model.SolanaPlanYearly,
	}
	decision := solanaDecidePayment(transaction, lookups)
	connect.AssertEqual(t, decision.action, solanaPaymentActionCredit)
	connect.AssertEqual(t, decision.intent.PaymentReference, solanaFixtureUsdcReference)
	connect.AssertEqual(t, decision.tokenAmountUsd, solanaFixtureUsdcAmount)

	// quoted more than arrived: recorded underpaid, as the webhook does
	lookups.intents[solanaFixtureUsdcReference].ExpectedAmountUsd = solanaFixtureUsdcAmount + 1
	decision = solanaDecidePayment(transaction, lookups)
	connect.AssertEqual(t, decision.action, solanaPaymentActionRecord)
	connect.AssertEqual(t, decision.unfulfilled.Reason, model.SolanaUnfulfilledReasonUnderpaid)

	// not our receiver: skipped
	solanaReceiverAddresses = []string{"4Fj9RCwJqHLdLNK28DwWHunHqWapxKbbzeYZLmreSYCM"}
	other := solanaTransactionFromParsed(solanaFixtureUsdcSignature, parsed)
	connect.AssertEqual(t, other.Type, "UNKNOWN")
	connect.AssertEqual(t, len(other.TokenTransfers), 0)
	connect.AssertEqual(t, solanaDecidePayment(other, lookups).action, solanaPaymentActionSkip)

	// a failed transaction moved no USDC
	solanaReceiverAddresses = []string{solanaFixtureUsdcReceiver}
	failed := solanaTestParsedTransaction(t, "sol_tx_usdc.json")
	failed.Meta.Err = map[string]any{"InstructionError": []any{3, "Custom"}}
	connect.AssertEqual(t, solanaTransactionFromParsed(solanaFixtureUsdcSignature, failed).Type, "UNKNOWN")
}

// A memo read from the chain is a reference candidate, as a memo Helius
// delivers is: the parsed memo text stands for the instruction's data.
func TestSolanaParsedMemoIsAReferenceCandidate(t *testing.T) {
	parsed := solanaTestParsedTransaction(t, "sol_tx_memo.json")
	transaction := solanaTransactionFromParsed("memo", parsed)
	connect.AssertEqual(t, slices.Contains(solanaReferenceCandidates(transaction), "genny-airdrop:0ce59391b0c9bd082a319504a09c4c50f29d8a0d849d8fab962d4dd0f3099d2e"), true)
}

type fakeSolanaChainLookups struct {
	intents   map[string]*model.PaymentIntentSearchResult
	completed map[string]bool
}

func (self *fakeSolanaChainLookups) searchIntentsByReference(references []string) (*model.PaymentIntentSearchResult, error) {
	for _, reference := range references {
		if intent, ok := self.intents[reference]; ok {
			return intent, nil
		}
	}
	return nil, nil
}

func (self *fakeSolanaChainLookups) isPaymentCompleted(signature string) bool {
	return self.completed[signature]
}

func (self *fakeSolanaChainLookups) intentsByAmountMicro(amountMicro int64, minExpiresAt time.Time) ([]*model.SolanaPaymentIntent, error) {
	return []*model.SolanaPaymentIntent{}, nil
}

// The Helius webhook's native branch: a native transfer to a SOL invoice
// address asks the SOL scanner to read the address next, and the delivery
// says so; it credits nothing and leaves the USDC decision as it was. A
// native transfer to another address hints nothing.
func TestHeliusNativeBranchHintsInvoiceAddresses(t *testing.T) {
	restore := cryptoTestHermeticSeams()
	t.Cleanup(restore)
	invoices := []string{solanaFixtureWallet}
	cryptoInvoiceAddresses = func(ctx context.Context, asset string, addresses []string) []string {
		connect.AssertEqual(t, asset, model.CryptoAssetSol)
		matched := []string{}
		for _, address := range addresses {
			if slices.Contains(invoices, address) {
				matched = append(matched, address)
			}
		}
		return matched
	}
	hints := map[string][]string{}
	cryptoAddScanHints = func(ctx context.Context, asset string, addresses []string) {
		hints[asset] = append(hints[asset], addresses...)
	}
	recorded := 0
	previousRecord := heliusRecordUnfulfilledSolanaPayment
	heliusRecordUnfulfilledSolanaPayment = func(ctx context.Context, payment *model.UnfulfilledSolanaPayment) error {
		recorded += 1
		return nil
	}
	t.Cleanup(func() { heliusRecordUnfulfilledSolanaPayment = previousRecord })

	var transactions []*SolanaTransaction
	connect.AssertEqual(t, json.Unmarshal(solanaTestFixture(t, "sol_helius_native.json"), &transactions), nil)
	clientSession := &session.ClientSession{Ctx: context.Background()}

	result, err := HeliusWebhook(transactions, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Message, "1 SOL invoice payments to read")
	connect.AssertEqual(t, hints[model.CryptoAssetSol], []string{solanaFixtureWallet})
	connect.AssertEqual(t, recorded, 0)

	// not an invoice address: nothing hinted, and the USDC decision's own
	// answer stands
	invoices = nil
	hints = map[string][]string{}
	result, err = HeliusWebhook(transactions, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Message, "Ignoring transaction with no token transfers")
	connect.AssertEqual(t, len(hints), 0)

	// a lookup that fails is logged, never the delivery's failure
	cryptoInvoiceAddresses = func(ctx context.Context, asset string, addresses []string) []string {
		panic(errors.New("synthetic database failure"))
	}
	result, err = HeliusWebhook(transactions, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Message, "Ignoring transaction with no token transfers")
}

// solanaChainLegTest installs the chain leg's seams for one test.
type solanaChainLegTest struct {
	rpc        *fakeSolanaRpc
	references []string
	handled    map[string]bool
	lookups    *fakeSolanaChainLookups
	credits    []string
	creditErr  error
	records    []*model.UnfulfilledSolanaPayment
	onboarded  int
	events     []*model.PaymentReconciliationEvent
}

func newSolanaChainLegTest(t *testing.T) *solanaChainLegTest {
	test := &solanaChainLegTest{
		rpc:     newFakeSolanaRpc(t),
		handled: map[string]bool{},
		lookups: &fakeSolanaChainLookups{intents: map[string]*model.PaymentIntentSearchResult{}, completed: map[string]bool{}},
	}
	solanaParsedTransactions.reset()
	previousReceivers := solanaReceiverAddresses
	previousUrl := solanaChainRpcUrl
	previousRpcFor := solanaChainRpcFor
	previousReferences := solanaChainOpenReferences
	previousHandled := solanaChainSignatureHandled
	previousLookups := solanaChainLookupsFor
	previousCredit := solanaChainCredit
	previousRecord := solanaChainRecord
	previousOnboarding := solanaChainOnboarding
	previousEvent := addPaymentReconciliationEvent
	solanaReceiverAddresses = []string{solanaFixtureUsdcReceiver}
	solanaChainRpcUrl = func() (string, bool) { return "https://synthetic.invalid/?api-key=synthetic", true }
	solanaChainRpcFor = func(rpcUrl string) solanaChainRpc { return test.rpc }
	solanaChainOpenReferences = func(connOwner server.PgConn, ctx context.Context, createdBefore time.Time, expiresAfter time.Time, limit int) []string {
		return test.references
	}
	solanaChainSignatureHandled = func(connOwner server.PgConn, ctx context.Context, signature string) bool {
		return test.handled[signature]
	}
	solanaChainLookupsFor = func(run *paymentReconcileRun) solanaPaymentLookups { return test.lookups }
	solanaChainCredit = func(connOwner server.PgConn, clientSession *session.ClientSession, intent *model.PaymentIntentSearchResult, signature string, usd float64) (bool, error) {
		if test.creditErr != nil {
			return false, test.creditErr
		}
		test.credits = append(test.credits, fmt.Sprintf("%s %s %.3f", intent.PaymentReference, signature, usd))
		return true, nil
	}
	solanaChainRecord = func(connOwner server.PgConn, ctx context.Context, payment *model.UnfulfilledSolanaPayment) error {
		test.records = append(test.records, payment)
		return nil
	}
	solanaChainOnboarding = func(clientSession *session.ClientSession, intent *model.PaymentIntentSearchResult) {
		test.onboarded += 1
	}
	addPaymentReconciliationEvent = func(connOwner server.PgConn, ctx context.Context, event *model.PaymentReconciliationEvent) error {
		test.events = append(test.events, event)
		return nil
	}
	t.Cleanup(func() {
		solanaReceiverAddresses = previousReceivers
		solanaChainRpcUrl = previousUrl
		solanaChainRpcFor = previousRpcFor
		solanaChainOpenReferences = previousReferences
		solanaChainSignatureHandled = previousHandled
		solanaChainLookupsFor = previousLookups
		solanaChainCredit = previousCredit
		solanaChainRecord = previousRecord
		solanaChainOnboarding = previousOnboarding
		addPaymentReconciliationEvent = previousEvent
		solanaParsedTransactions.reset()
	})
	return test
}

func solanaChainLegTestRun(dryRun bool) *paymentReconcileRun {
	return &paymentReconcileRun{
		clientSession: &session.ClientSession{Ctx: context.Background()},
		runId:         server.NewId(),
		now:           time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC),
		dryRun:        dryRun,
		budget:        paymentReconcileApiBudget,
		storeDetails:  map[string]map[string]any{},
		storeResults:  map[string]*PaymentReconcileStoreResult{},
	}
}

func (self *solanaChainLegTest) intent() *model.PaymentIntentSearchResult {
	networkId := server.NewId()
	intent := &model.PaymentIntentSearchResult{
		NetworkId:         &networkId,
		PaymentReference:  solanaFixtureUsdcReference,
		ExpectedAmountUsd: solanaFixtureUsdcAmount,
		SubscriptionPlan:  model.SolanaPlanYearly,
	}
	self.references = []string{solanaFixtureUsdcReference}
	self.lookups.intents[solanaFixtureUsdcReference] = intent
	self.rpc.signatures[solanaFixtureUsdcReference] = "sol_signatures.json"
	self.rpc.transactions[solanaFixtureUsdcSignature] = "sol_tx_usdc.json"
	self.rpc.transactions[solanaFixtureNativeSignature] = "sol_tx_native.json"
	return intent
}

// The chain leg credits a USDC payment its webhook never delivered: the
// open intent's reference names the transaction on the chain, and the
// webhook's decision credits it through the same credit, once, with the
// onboarding record the webhook writes. A dry run credits nothing and says
// what it would.
func TestSolanaChainLegCreditsALostDelivery(t *testing.T) {
	test := newSolanaChainLegTest(t)
	intent := test.intent()

	run := solanaChainLegTestRun(false)
	complete, err := reconcileSolanaChainReferences(run)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, complete, true)
	connect.AssertEqual(t, test.credits, []string{fmt.Sprintf("%s %s %.3f", solanaFixtureUsdcReference, solanaFixtureUsdcSignature, solanaFixtureUsdcAmount)})
	connect.AssertEqual(t, test.onboarded, 1)
	connect.AssertEqual(t, len(test.events), 1)
	connect.AssertEqual(t, test.events[0].Store, model.SubscriptionMarketSolana)
	connect.AssertEqual(t, test.events[0].Action, model.PaymentReconcileActionCredited)
	connect.AssertEqual(t, test.events[0].Evidence, solanaFixtureUsdcSignature)
	connect.AssertEqual(t, test.events[0].NetworkId, intent.NetworkId)
	connect.AssertEqual(t, test.events[0].Details["leg"], "chain")
	// the native transfer of the same address's history pays no USDC intent:
	// skipped, unrecorded
	connect.AssertEqual(t, len(test.records), 0)
	connect.AssertEqual(t, run.storeResult(model.SubscriptionMarketSolana).Credited, 1)

	// a dry run writes nothing but its would_credit event
	dry := newSolanaChainLegTest(t)
	dry.intent()
	dryRun := solanaChainLegTestRun(true)
	complete, err = reconcileSolanaChainReferences(dryRun)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, complete, true)
	connect.AssertEqual(t, len(dry.credits), 0)
	connect.AssertEqual(t, dry.onboarded, 0)
	connect.AssertEqual(t, len(dry.events), 1)
	connect.AssertEqual(t, dry.events[0].Action, model.PaymentReconcileActionWouldCredit)
}

// What the chain leg leaves alone and what it records: a signature already
// credited or recorded is not read again, an underpayment is recorded for an
// operator as the webhook records it, a failed credit is an error event, and
// a run out of budget stops incomplete.
func TestSolanaChainLegDecisions(t *testing.T) {
	// handled already: not read, nothing done
	handled := newSolanaChainLegTest(t)
	handled.intent()
	handled.handled[solanaFixtureUsdcSignature] = true
	handled.handled[solanaFixtureNativeSignature] = true
	complete, err := reconcileSolanaChainReferences(solanaChainLegTestRun(false))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, complete, true)
	connect.AssertEqual(t, len(handled.credits)+len(handled.records)+len(handled.events), 0)
	connect.AssertEqual(t, handled.rpc.calls["getTransaction"], 0)

	// underpaid: recorded, not credited
	underpaid := newSolanaChainLegTest(t)
	underpaid.intent().ExpectedAmountUsd = solanaFixtureUsdcAmount + 1
	complete, err = reconcileSolanaChainReferences(solanaChainLegTestRun(false))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, complete, true)
	connect.AssertEqual(t, len(underpaid.credits), 0)
	connect.AssertEqual(t, len(underpaid.records), 1)
	connect.AssertEqual(t, underpaid.records[0].Reason, model.SolanaUnfulfilledReasonUnderpaid)
	connect.AssertEqual(t, underpaid.records[0].TxSignature, solanaFixtureUsdcSignature)

	// a credit that fails is an error event for the run
	failing := newSolanaChainLegTest(t)
	failing.intent()
	failing.creditErr = errors.New("synthetic credit failure")
	run := solanaChainLegTestRun(false)
	complete, err = reconcileSolanaChainReferences(run)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, complete, true)
	connect.AssertEqual(t, len(failing.events), 1)
	connect.AssertEqual(t, failing.events[0].Action, model.PaymentReconcileActionError)
	connect.AssertEqual(t, run.storeResult(model.SubscriptionMarketSolana).Errors, 1)

	// out of budget: incomplete, so the watermark does not advance
	budget := newSolanaChainLegTest(t)
	budget.intent()
	run = solanaChainLegTestRun(false)
	run.budget = 1
	complete, err = reconcileSolanaChainReferences(run)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, complete, false)
	connect.AssertEqual(t, len(budget.credits), 0)

	// the RPC failing fails the leg
	down := newSolanaChainLegTest(t)
	down.intent()
	down.rpc.err = errors.New("getSignaturesForAddress: http status 503")
	complete, err = reconcileSolanaChainReferences(solanaChainLegTestRun(false))
	connect.AssertEqual(t, err != nil, true)
	connect.AssertEqual(t, complete, false)

	// no Helius key: nothing to read, complete
	off := newSolanaChainLegTest(t)
	off.intent()
	solanaChainRpcUrl = func() (string, bool) { return "", false }
	complete, err = reconcileSolanaChainReferences(solanaChainLegTestRun(false))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, complete, true)
	connect.AssertEqual(t, off.rpc.calls["getSignaturesForAddress"], 0)
}
