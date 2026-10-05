package controller

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The browser-wallet transfer (POST /solana/payment-transaction). Built and
// decoded offline: the intent lookup and the rpc are seams, nothing is sent.

// pinned on purpose: the USDC mint and the programs
const (
	solanaTestUsdcMint     = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	solanaTestTokenProgram = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	solanaTestMemoProgram  = "MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr"

	// fixture keys (sha256 of a fixed phrase), not wallets: the merchant, a
	// stand-in for the receiver being retired, the payer, the reference and
	// the blockhash. The tests configure the merchant as the server's receiver
	// (solanaTestReceivers); the official address stays out of test data.
	solanaTestMerchant        = "835ygSFyB6b9Ghz5yXjv8QiiKDrzHA8rJzoVPChGJYmX"
	solanaTestRetiredReceiver = "3zNbojjtBaA1KUhY1SJurrkgjzsPyRAPHtmqST7W5Q1N"
	solanaTestPayer           = "EvfsGBeCMZcgYphDZkyitoEe9HqWn2GZDyrMpuSngBGE"
	solanaTestReference       = "9LyJFGoWExmu98m1wn2LACRfhwQtmKUE8Bv2hwkCktBR"
	solanaTestBlockhash       = "4Yj9uZgYU9PSzQVfJZWZEuVQxcAzcideeApkA27t3CCW"
	// their USDC token accounts (associated token accounts, derived)
	solanaTestMerchantUsdcAccount = "ALBe7k9w9FfxMUL2uzNYeV87Azurtjs69YLyPaSK7axo"
	solanaTestPayerUsdcAccount    = "61XgyVCyMAtnghXkEqCQDUPiAEWvyDadjLjytcXDbgcc"

	// the builder's wire bytes for the fixture transfer of 40.004317 USDC to
	// the fixture merchant; ur.io's transfer fixture (mmm
	// ur.io/react/tests/usdc-transfer-fixture.mjs, TRANSFER_BASE64) carries
	// the same bytes, so its verifier test checks what this builder makes
	solanaTestTransferBase64 = "AQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABAAQHzueo1YwjMkDqsndkJzm2PkQI84CByKoWyvW5KciyK+NKbSKRHGMtg6L1xyxZfLY/5k2GUin6u19r52sol+exnYqjP5IerVOQEx8NA1I8c31RKEOpn9hwCed0rVpLp4o4BUpTWpkpIQZNJOhxYNo4fHw1td28kruB5B+oQEEFRI0G3fbh12Whk9nL4UbO63msHLSF7V9bN5E6jPWFfv8AqXv7L7edG2plPJCI9sZyjADAQn9uQkDUYf6CdaTHvF9Qxvp6877brTo9ZfNqq8l0MbG75MLS9uDkfKYCA0UvXWE0s7qiQ/Ayt+7UBcpiZ8soc9YWjgff52GAwxOtOoP1JwIDACw5THlKRkdvV0V4bXU5OG0xd24yTEFDUmZod1F0bUtVRThCdjJod2tDa3RCUgQFAQYCAAUKDN1qYgIAAAAABg=="

	// the official merchant by its sha256 (hex), so the address itself stays
	// out of test data
	solanaOfficialMerchantSha256 = "b6fed7b0a3462afeda2f9703ecc17076b664bb8b1129bb0b62c70304bd50ab2c"
)

// Configures receivers as the server's receiving addresses for the test, the
// first the one every intent quotes, as a rotation does.
func solanaTestReceivers(t testing.TB, receivers ...string) {
	previousReceivers := solanaReceiverAddresses
	t.Cleanup(func() {
		solanaReceiverAddresses = previousReceivers
	})
	solanaReceiverAddresses = receivers
}

// A fixture key, not a wallet: sha256 of the phrase as a public key.
func solanaTestKey(phrase string) solana.PublicKey {
	sum := sha256.Sum256([]byte(phrase))
	return solana.PublicKeyFromBytes(sum[:])
}

// solanaDecodedTransfer is a built transaction read back from its wire bytes.
type solanaDecodedTransfer struct {
	transaction *solana.Transaction
	keys        []string
	// per instruction: the program, the accounts and the data
	programs []string
	accounts [][]string
	data     [][]byte
}

func solanaDecodeTransfer(t testing.TB, transactionBytes []byte) *solanaDecodedTransfer {
	t.Helper()
	transaction, err := solana.TransactionFromBytes(transactionBytes)
	connect.AssertEqual(t, err, nil)
	decoded := &solanaDecodedTransfer{transaction: transaction}
	for _, key := range transaction.Message.AccountKeys {
		decoded.keys = append(decoded.keys, key.String())
	}
	for _, instruction := range transaction.Message.Instructions {
		decoded.programs = append(decoded.programs, decoded.keys[instruction.ProgramIDIndex])
		accounts := []string{}
		for _, index := range instruction.Accounts {
			accounts = append(accounts, decoded.keys[index])
		}
		decoded.accounts = append(decoded.accounts, accounts)
		decoded.data = append(decoded.data, []byte(instruction.Data))
	}
	return decoded
}

func (self *solanaDecodedTransfer) writable(t *testing.T, key string) bool {
	writable, err := self.transaction.Message.IsWritable(solana.MustPublicKeyFromBase58(key))
	connect.AssertEqual(t, err, nil)
	return writable
}

func (self *solanaDecodedTransfer) signer(key string) bool {
	return self.transaction.Message.IsSigner(solana.MustPublicKeyFromBase58(key))
}

func solanaTransferCheckedData(amountMicro uint64) []byte {
	data := []byte{12, 0, 0, 0, 0, 0, 0, 0, 0, 6}
	binary.LittleEndian.PutUint64(data[1:9], amountMicro)
	return data
}

// The receiver every intent quotes is the official merchant, compared by its
// sha256 so the address stays out of test data, and the mint is USDC.
func TestSolanaPaymentRecipientIsTheOfficialMerchant(t *testing.T) {
	sum := sha256.Sum256([]byte(solanaPaymentRecipient()))
	connect.AssertEqual(t, hex.EncodeToString(sum[:]), solanaOfficialMerchantSha256)
	connect.AssertEqual(t, solanaUsdcMint, solanaTestUsdcMint)
}

// The transfer moves exactly the quote, in USDC, from the payer's USDC account
// to the merchant's, and does nothing else. Only the payer signs, and the
// server signs nothing.
func TestSolanaPaymentTransactionPaysOnlyTheMerchantTheQuotedUsdc(t *testing.T) {
	connect.AssertEqual(t, solanaUsdcMint, solanaTestUsdcMint)

	transaction, err := solanaPaymentTransaction(
		solana.MustPublicKeyFromBase58(solanaTestPayer),
		solana.MustPublicKeyFromBase58(solanaTestMerchant),
		solana.MustPublicKeyFromBase58(solanaTestReference),
		40_004_317,
		solana.MustHashFromBase58(solanaTestBlockhash),
	)
	connect.AssertEqual(t, err, nil)
	transactionBytes, err := transaction.MarshalBinary()
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, base64.StdEncoding.EncodeToString(transactionBytes), solanaTestTransferBase64)
	decoded := solanaDecodeTransfer(t, transactionBytes)

	// one signature slot, unsigned: the wallet signs
	connect.AssertEqual(t, len(decoded.transaction.Signatures), 1)
	connect.AssertEqual(t, decoded.transaction.Signatures[0], solana.Signature{})
	connect.AssertEqual(t, decoded.transaction.Message.IsVersioned(), false)
	connect.AssertEqual(t, decoded.transaction.Message.Header.NumRequiredSignatures, uint8(1))
	// the payer is the fee payer and the only signer
	connect.AssertEqual(t, decoded.keys[0], solanaTestPayer)
	for _, key := range decoded.keys {
		connect.AssertEqual(t, decoded.signer(key), key == solanaTestPayer)
	}
	connect.AssertEqual(t, decoded.transaction.Message.RecentBlockhash.String(), solanaTestBlockhash)

	// memo, then the transfer
	connect.AssertEqual(t, decoded.programs, []string{solanaTestMemoProgram, solanaTestTokenProgram})

	// the memo is the reference, with no accounts
	connect.AssertEqual(t, decoded.accounts[0], []string{})
	connect.AssertEqual(t, string(decoded.data[0]), solanaTestReference)

	// TransferChecked of exactly the quote at USDC's 6 decimals: from the
	// payer's USDC account, of the USDC mint, to the merchant's USDC account,
	// authorized by the payer, with the reference attached
	connect.AssertEqual(t, decoded.data[1], solanaTransferCheckedData(40_004_317))
	connect.AssertEqual(t, decoded.accounts[1], []string{
		solanaTestPayerUsdcAccount,
		solanaTestUsdcMint,
		solanaTestMerchantUsdcAccount,
		solanaTestPayer,
		solanaTestReference,
	})

	// the derived accounts are the associated token accounts
	merchantUsdcAccount, _, err := solana.FindAssociatedTokenAddress(
		solana.MustPublicKeyFromBase58(solanaTestMerchant),
		solana.MustPublicKeyFromBase58(solanaTestUsdcMint),
	)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, merchantUsdcAccount.String(), solanaTestMerchantUsdcAccount)
	payerUsdcAccount, _, err := solana.FindAssociatedTokenAddress(
		solana.MustPublicKeyFromBase58(solanaTestPayer),
		solana.MustPublicKeyFromBase58(solanaTestUsdcMint),
	)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, payerUsdcAccount.String(), solanaTestPayerUsdcAccount)

	// only the payer (fees), its USDC account and the merchant's USDC account
	// are writable: nothing else can be debited or changed
	writable := []string{}
	for _, key := range decoded.keys {
		if decoded.writable(t, key) {
			writable = append(writable, key)
		}
	}
	slices.Sort(writable)
	expectedWritable := []string{solanaTestPayer, solanaTestPayerUsdcAccount, solanaTestMerchantUsdcAccount}
	slices.Sort(expectedWritable)
	connect.AssertEqual(t, writable, expectedWritable)
	// the reference is attached read-only, the Solana Pay way
	connect.AssertEqual(t, decoded.writable(t, solanaTestReference), false)

	// exactly these accounts, nothing more
	keys := slices.Clone(decoded.keys)
	slices.Sort(keys)
	expectedKeys := []string{
		solanaTestPayer,
		solanaTestPayerUsdcAccount,
		solanaTestMerchantUsdcAccount,
		solanaTestUsdcMint,
		solanaTestReference,
		solanaTestMemoProgram,
		solanaTestTokenProgram,
	}
	slices.Sort(expectedKeys)
	connect.AssertEqual(t, keys, expectedKeys)
}

// solanaFakePaymentTransactionDeps replaces the intent lookup and the rpc.
type solanaFakePaymentTransactionDeps struct {
	intents       map[string]*model.SolanaPaymentIntent
	lookups       int
	blockhashErr  error
	blockhashGets int
}

func (self *solanaFakePaymentTransactionDeps) install(t *testing.T) {
	intent := solanaPaymentTransactionIntent
	latestBlockhash := solanaLatestBlockhash
	t.Cleanup(func() {
		solanaPaymentTransactionIntent = intent
		solanaLatestBlockhash = latestBlockhash
	})
	solanaPaymentTransactionIntent = func(_ context.Context, reference string) *model.SolanaPaymentIntent {
		self.lookups += 1
		return self.intents[reference]
	}
	solanaLatestBlockhash = func(_ context.Context) (solana.Hash, error) {
		self.blockhashGets += 1
		if self.blockhashErr != nil {
			return solana.Hash{}, self.blockhashErr
		}
		return solana.MustHashFromBase58(solanaTestBlockhash), nil
	}
}

func solanaTestPaymentSession(networkId server.Id) *session.ClientSession {
	return &session.ClientSession{
		Ctx:   context.Background(),
		ByJwt: &jwt.ByJwt{NetworkId: networkId, UserId: server.NewId()},
	}
}

// Only an open intent of the caller's own network is built, for its quote; a
// refused request never reaches the rpc.
func TestSolanaPaymentTransactionUsesTheCallersOpenIntent(t *testing.T) {
	solanaTestReceivers(t, solanaTestMerchant, solanaTestRetiredReceiver)
	networkId := server.NewId()
	otherNetworkId := server.NewId()
	future := server.NowUtc().Add(time.Hour)
	past := server.NowUtc().Add(-time.Minute)
	signature := "5yZ7RHQD8xhCF6pPyP2CP3xS5Ld9rU4mSGBYqXdyPr6m"

	newDeps := func(intent *model.SolanaPaymentIntent) *solanaFakePaymentTransactionDeps {
		deps := &solanaFakePaymentTransactionDeps{intents: map[string]*model.SolanaPaymentIntent{}}
		if intent != nil {
			deps.intents[intent.PaymentReference] = intent
		}
		return deps
	}
	openIntent := func() *model.SolanaPaymentIntent {
		return &model.SolanaPaymentIntent{
			PaymentReference: solanaTestReference,
			NetworkId:        networkId,
			// the price plus its unique sub-cent suffix
			ExpectedAmountUsd: 40.004317,
			SubscriptionPlan:  model.SolanaPlanYearly,
			ExpiresAt:         &future,
		}
	}
	openIntentWith := func(change func(intent *model.SolanaPaymentIntent)) *model.SolanaPaymentIntent {
		intent := openIntent()
		change(intent)
		return intent
	}

	t.Run("an open intent of the caller's network is built for its quote", func(t *testing.T) {
		deps := newDeps(openIntent())
		deps.install(t)
		result, err := CreateSolanaPaymentTransaction(&SolanaPaymentTransactionArgs{
			Reference: solanaTestReference,
			Payer:     solanaTestPayer,
		}, solanaTestPaymentSession(networkId))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, (*SolanaPaymentTransactionError)(nil))
		connect.AssertEqual(t, deps.blockhashGets, 1)

		transactionBytes, err := base64.StdEncoding.DecodeString(result.Transaction)
		connect.AssertEqual(t, err, nil)
		decoded := solanaDecodeTransfer(t, transactionBytes)
		connect.AssertEqual(t, decoded.keys[0], solanaTestPayer)
		connect.AssertEqual(t, string(decoded.data[0]), solanaTestReference)
		connect.AssertEqual(t, decoded.data[1], solanaTransferCheckedData(40_004_317))
		connect.AssertEqual(t, decoded.accounts[1][2], solanaTestMerchantUsdcAccount)
	})

	t.Run("a data pack intent of the caller's network is built too", func(t *testing.T) {
		deps := newDeps(openIntentWith(func(intent *model.SolanaPaymentIntent) {
			intent.SubscriptionPlan = "data_1tib"
			intent.ExpectedAmountUsd = 3.000042
		}))
		deps.install(t)
		result, err := CreateSolanaPaymentTransaction(&SolanaPaymentTransactionArgs{
			Reference: solanaTestReference,
			Payer:     solanaTestPayer,
		}, solanaTestPaymentSession(networkId))
		connect.AssertEqual(t, err, nil)
		transactionBytes, err := base64.StdEncoding.DecodeString(result.Transaction)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, solanaDecodeTransfer(t, transactionBytes).data[1], solanaTransferCheckedData(3_000_042))
	})

	refused := []struct {
		name    string
		intent  *model.SolanaPaymentIntent
		args    *SolanaPaymentTransactionArgs
		message string
		lookups int
	}{
		{
			name:    "another network's intent",
			intent:  openIntentWith(func(intent *model.SolanaPaymentIntent) { intent.NetworkId = otherNetworkId }),
			message: "This payment was not found. Start again.",
			lookups: 1,
		},
		{
			name:    "no intent",
			message: "This payment was not found. Start again.",
			lookups: 1,
		},
		{
			name:    "a paid intent",
			intent:  openIntentWith(func(intent *model.SolanaPaymentIntent) { intent.TxSignature = &signature }),
			message: "This payment was already received.",
			lookups: 1,
		},
		{
			name:    "an expired intent",
			intent:  openIntentWith(func(intent *model.SolanaPaymentIntent) { intent.ExpiresAt = &past }),
			message: "This payment request expired. Start again.",
			lookups: 1,
		},
		{
			name:    "a payer that is not a Solana address",
			intent:  openIntent(),
			args:    &SolanaPaymentTransactionArgs{Reference: solanaTestReference, Payer: "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"},
			message: "The wallet did not give a Solana address.",
		},
		{
			name:    "a reference that is not a public key",
			intent:  openIntent(),
			args:    &SolanaPaymentTransactionArgs{Reference: "not-a-reference", Payer: solanaTestPayer},
			message: "This payment cannot be sent from a browser wallet. Scan the QR code or send it by hand.",
		},
	}
	for _, c := range refused {
		t.Run(c.name, func(t *testing.T) {
			deps := newDeps(c.intent)
			deps.install(t)
			args := c.args
			if args == nil {
				args = &SolanaPaymentTransactionArgs{Reference: solanaTestReference, Payer: solanaTestPayer}
			}
			result, err := CreateSolanaPaymentTransaction(args, solanaTestPaymentSession(networkId))
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Transaction, "")
			connect.AssertEqual(t, result.Error.Message, c.message)
			connect.AssertEqual(t, deps.lookups, c.lookups)
			connect.AssertEqual(t, deps.blockhashGets, 0)
		})
	}

	t.Run("an rpc failure is a plain message", func(t *testing.T) {
		deps := newDeps(openIntent())
		deps.blockhashErr = &url.Error{
			Op:  "Post",
			URL: "https://mainnet.helius-rpc.com/?api-key=00000000-test-key",
			Err: errors.New("dial tcp: i/o timeout"),
		}
		deps.install(t)
		result, err := CreateSolanaPaymentTransaction(&SolanaPaymentTransactionArgs{
			Reference: solanaTestReference,
			Payer:     solanaTestPayer,
		}, solanaTestPaymentSession(networkId))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Transaction, "")
		connect.AssertEqual(t, result.Error.Message, "Could not reach the Solana network. Try again, or scan the QR code.")
	})
}

// The blockhash request to the rpc names no account: nothing about the payer
// or the intent leaves the server. The rpc is a local stand-in.
func TestSolanaPaymentTransactionRpcCarriesNothingAboutThePayment(t *testing.T) {
	bodies := []string{}
	rpc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":1},"value":{"blockhash":"`+solanaTestBlockhash+`","lastValidBlockHeight":2}}}`)
	}))
	defer rpc.Close()
	rpcUrl := solanaRpcUrlFunc
	t.Cleanup(func() { solanaRpcUrlFunc = rpcUrl })
	solanaRpcUrlFunc = func() string {
		return rpc.URL + "/?api-key=00000000-test-key"
	}

	networkId := server.NewId()
	future := server.NowUtc().Add(time.Hour)
	intent := solanaPaymentTransactionIntent
	t.Cleanup(func() { solanaPaymentTransactionIntent = intent })
	solanaPaymentTransactionIntent = func(_ context.Context, reference string) *model.SolanaPaymentIntent {
		return &model.SolanaPaymentIntent{
			PaymentReference:  reference,
			NetworkId:         networkId,
			ExpectedAmountUsd: 40.004317,
			SubscriptionPlan:  model.SolanaPlanYearly,
			ExpiresAt:         &future,
		}
	}

	result, err := CreateSolanaPaymentTransaction(&SolanaPaymentTransactionArgs{
		Reference: solanaTestReference,
		Payer:     solanaTestPayer,
	}, solanaTestPaymentSession(networkId))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error, (*SolanaPaymentTransactionError)(nil))
	transactionBytes, err := base64.StdEncoding.DecodeString(result.Transaction)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, solanaDecodeTransfer(t, transactionBytes).transaction.Message.RecentBlockhash.String(), solanaTestBlockhash)

	connect.AssertEqual(t, len(bodies), 1)
	var request map[string]any
	connect.AssertEqual(t, json.Unmarshal([]byte(bodies[0]), &request), nil)
	connect.AssertEqual(t, request, map[string]any{
		"jsonrpc": "2.0",
		"id":      float64(1),
		"method":  "getLatestBlockhash",
		"params":  []any{map[string]any{"commitment": "confirmed"}},
	})
	for _, private := range []string{solanaTestPayer, solanaTestPayerUsdcAccount, solanaTestReference} {
		connect.AssertEqual(t, strings.Contains(bodies[0], private), false)
	}
}

// An rpc error answer is an error, not a zero blockhash.
func TestSolanaRpcLatestBlockhashRefusesAnErrorAnswer(t *testing.T) {
	for _, answer := range []string{
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"rate limited"}}`,
		`{"jsonrpc":"2.0","id":1}`,
		`null`,
		`{"jsonrpc":"2.0","id":1,"result":{"value":{"blockhash":"not base58 0OIl"}}}`,
	} {
		rpc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, answer)
		}))
		rpcUrl := solanaRpcUrlFunc
		solanaRpcUrlFunc = func() string { return rpc.URL }
		_, err := solanaRpcLatestBlockhash(context.Background())
		solanaRpcUrlFunc = rpcUrl
		rpc.Close()
		connect.AssertNotEqual(t, err, nil)
	}
}

// The rpc url carries the api key in its query; a logged failure must not.
func TestSolanaRpcErrorTextHidesTheApiKey(t *testing.T) {
	key := "00000000-test-key"
	for _, err := range []error{
		&url.Error{Op: "Post", URL: "https://mainnet.helius-rpc.com/?api-key=" + key, Err: errors.New("dial tcp: i/o timeout")},
		errors.New(`Post "https://mainnet.helius-rpc.com/?api-key=` + key + `": EOF`),
		&server.HttpStatusError{StatusCode: 401, Status: "401 Unauthorized", ResponseBody: "invalid api-key=" + key},
	} {
		text := solanaRpcErrorText(err)
		connect.AssertEqual(t, strings.Contains(text, key), false)
	}
	connect.AssertEqual(
		t,
		solanaRpcErrorText(&url.Error{Op: "Post", URL: "https://mainnet.helius-rpc.com/?api-key=" + key, Err: errors.New("dial tcp: i/o timeout")}),
		"Post: dial tcp: i/o timeout",
	)
}
