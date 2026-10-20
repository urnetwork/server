package controller

// Paying a Solana Pay intent from a browser wallet.
//
// A desktop browser hands the solana: payment url to an app the OS registered
// for it, never to a browser extension wallet, so ur.io asks the wallet itself
// to sign and send the transfer (the Wallet Standard
// `solana:signAndSendTransaction` feature). POST /solana/payment-transaction
// builds that transfer here, unsigned, so the page needs no Solana library
// and no rpc endpoint of its own:
//
//   - Memo(reference), then TransferChecked of exactly the intent's quote from
//     the payer's USDC token account to the merchant's, with the reference
//     appended as a read-only account: the transaction a wallet builds from
//     the payment url, which the Helius webhook credits by reference. The
//     amount is the quote with its unique suffix, so the memo-less match
//     would identify it too.
//   - The recipient and the mint are the ones every intent quotes and the
//     webhook credits (solanaPaymentRecipient, solanaUsdcMint). Nothing in the
//     request can change them or the amount.
//   - The payer is the fee payer and the only signer. The server handles no
//     keys and signs nothing; the wallet shows the transfer before it signs.
//   - The recent blockhash comes from the server's Helius rpc. That request
//     carries nothing about the payer or the intent.
//
// Only an open intent of the caller's own network is built.

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The request of POST /solana/payment-transaction.
type SolanaPaymentTransactionArgs struct {
	// the reference of an open intent of the caller's network, from
	// /solana/payment-intent or /pay/data/solana-intent
	Reference string `json:"reference"`
	// the paying wallet's address (base58), as the wallet reported it
	Payer string `json:"payer"`
}

// The unsigned transfer, or why it was not built.
type SolanaPaymentTransactionResult struct {
	// the unsigned transaction (a legacy message, base64 wire format) for the
	// wallet to sign and send
	Transaction string                         `json:"transaction,omitempty"`
	Error       *SolanaPaymentTransactionError `json:"error,omitempty"`
}

// A refusal the page shows the user as is.
type SolanaPaymentTransactionError struct {
	Message string `json:"message"`
}

const (
	// the spl-token instruction that names the mint and its decimals, so a
	// transfer of the wrong token or scale fails instead of moving funds
	solanaTokenInstructionTransferChecked = 12
	// the USDC mint's decimals
	solanaUsdcDecimals = 6
)

// a click waits on this call, and a blockhash is only good for about a minute
const solanaRpcBlockhashTimeout = 10 * time.Second

// the intent lookup and the rpc, as seams so a test builds a transaction
// without Postgres or the network
var (
	solanaPaymentTransactionIntent = model.GetSolanaPaymentIntent
	solanaLatestBlockhash          = solanaRpcLatestBlockhash
)

// each build costs one rpc call
var solanaPaymentTransactionLimiter = &payDataIpLimiter{limitPerMinute: payDataCheckoutIpLimitPerMinute}

// a bounded result per request; the detail of a refused request is V(1)
var solanaPaymentTransactionResults = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "solana",
	Name:      "payment_transactions_total",
	Help:      "Unsigned wallet payment transactions requested for Solana Pay intents, by result",
}, []string{"result"})

// Registers the result counter with the default registry.
func init() {
	prometheus.MustRegister(solanaPaymentTransactionResults)
}

// Counts a refused request under result and answers it with message.
func solanaPaymentTransactionRefused(result string, message string) *SolanaPaymentTransactionResult {
	solanaPaymentTransactionResults.WithLabelValues(result).Inc()
	return &SolanaPaymentTransactionResult{
		Error: &SolanaPaymentTransactionError{Message: message},
	}
}

// Builds the unsigned USDC transfer of an open intent of the caller's network
// for the paying wallet to sign and send. A refusal is a result error, never
// an error.
func CreateSolanaPaymentTransaction(
	args *SolanaPaymentTransactionArgs,
	clientSession *session.ClientSession,
) (*SolanaPaymentTransactionResult, error) {
	reference := strings.TrimSpace(args.Reference)
	// a Solana Pay reference is a public key; anything else cannot be attached
	// to the transfer. The memo carries its canonical form, which must be the
	// stored reference exactly.
	referenceKey, err := solana.PublicKeyFromBase58(reference)
	if err == nil && referenceKey.String() != reference {
		err = errors.New("not in canonical form")
	}
	if err != nil {
		if glog.V(1) {
			glog.Infof("[sub]solana payment transaction: the reference is not a public key: %s\n", err)
		}
		return solanaPaymentTransactionRefused(
			"invalid",
			"This payment cannot be sent from a browser wallet. Scan the QR code or send it by hand.",
		), nil
	}
	payer, err := solana.PublicKeyFromBase58(strings.TrimSpace(args.Payer))
	if err != nil {
		if glog.V(1) {
			glog.Infof("[sub]solana payment transaction: the payer is not a public key: %s\n", err)
		}
		return solanaPaymentTransactionRefused("invalid", "The wallet did not give a Solana address."), nil
	}

	if !solanaPaymentTransactionLimiter.allow(clientSession) {
		return solanaPaymentTransactionRefused(
			"rate_limited",
			"Too many payment attempts from this address. Try again in a minute.",
		), nil
	}

	intent := solanaPaymentTransactionIntent(clientSession.Ctx, reference)
	// another network's intent reads as no intent
	if intent == nil || intent.NetworkId != clientSession.ByJwt.NetworkId {
		if glog.V(1) {
			glog.Infof("[sub]solana payment transaction: no intent %s for the caller's network\n", reference)
		}
		return solanaPaymentTransactionRefused("no_intent", "This payment was not found. Start again."), nil
	}
	switch payDataSolanaStatusOf(intent, server.NowUtc()) {
	case PayDataSolanaStatusPaid:
		return solanaPaymentTransactionRefused("paid", "This payment was already received."), nil
	case PayDataSolanaStatusExpired:
		return solanaPaymentTransactionRefused("expired", "This payment request expired. Start again."), nil
	}
	// the quote recorded on the intent: the price plus its unique suffix
	amountMicro := model.SolanaUsdToMicro(intent.ExpectedAmountUsd)
	if amountMicro <= 0 {
		glog.Errorf("[sub]solana payment transaction: intent %s has no quote\n", reference)
		return solanaPaymentTransactionRefused("no_intent", "This payment was not found. Start again."), nil
	}

	recentBlockhash, err := solanaLatestBlockhash(clientSession.Ctx)
	if err != nil {
		glog.Errorf("[sub]solana payment transaction: no recent blockhash: %s\n", solanaRpcErrorText(err))
		return solanaPaymentTransactionRefused(
			"rpc_error",
			"Could not reach the Solana network. Try again, or scan the QR code.",
		), nil
	}

	// the receiver every intent quotes; a client checks the transfer against
	// the recipient its own quote named before the wallet signs
	recipient, err := solana.PublicKeyFromBase58(solanaPaymentRecipient())
	var transaction *solana.Transaction
	if err == nil {
		transaction, err = solanaPaymentTransaction(payer, recipient, referenceKey, uint64(amountMicro), recentBlockhash)
	}
	var transactionBytes []byte
	if err == nil {
		transactionBytes, err = transaction.MarshalBinary()
	}
	if err != nil {
		glog.Errorf("[sub]solana payment transaction: could not build the transfer for %s: %s\n", reference, err)
		return solanaPaymentTransactionRefused(
			"build_error",
			"Could not prepare the payment. Scan the QR code or send it by hand.",
		), nil
	}

	solanaPaymentTransactionResults.WithLabelValues("built").Inc()
	return &SolanaPaymentTransactionResult{
		Transaction: base64.StdEncoding.EncodeToString(transactionBytes),
	}, nil
}

// The unsigned USDC transfer of amountMicro from payer to recipient (the
// merchant), carrying reference as the memo and as a read-only account, the
// way a wallet builds it from a Solana Pay url (memo first).
func solanaPaymentTransaction(
	payer solana.PublicKey,
	recipient solana.PublicKey,
	reference solana.PublicKey,
	amountMicro uint64,
	recentBlockhash solana.Hash,
) (*solana.Transaction, error) {
	mint, err := solana.PublicKeyFromBase58(solanaUsdcMint)
	if err != nil {
		return nil, err
	}
	source, _, err := solana.FindAssociatedTokenAddress(payer, mint)
	if err != nil {
		return nil, err
	}
	destination, _, err := solana.FindAssociatedTokenAddress(recipient, mint)
	if err != nil {
		return nil, err
	}

	// the memo text is the instruction data; no signer accounts
	memoInstruction := solana.NewInstruction(
		solana.MemoProgramID,
		solana.AccountMetaSlice{},
		[]byte(reference.String()),
	)

	// TransferChecked: the instruction, the amount (little-endian u64) and the
	// decimals. Its accounts are the source, the mint, the destination and the
	// owner (the signer), then the reference as a read-only, non-signer
	// account (Solana Pay), which the webhook finds among the transaction's
	// account keys.
	transferData := make([]byte, 10)
	transferData[0] = solanaTokenInstructionTransferChecked
	binary.LittleEndian.PutUint64(transferData[1:9], amountMicro)
	transferData[9] = solanaUsdcDecimals
	transfer := solana.NewInstruction(
		solana.TokenProgramID,
		solana.AccountMetaSlice{
			solana.Meta(source).WRITE(),
			solana.Meta(mint),
			solana.Meta(destination).WRITE(),
			solana.Meta(payer).SIGNER(),
			solana.Meta(reference),
		},
		transferData,
	)

	return solana.NewTransaction(
		[]solana.Instruction{memoInstruction, transfer},
		recentBlockhash,
		solana.TransactionPayer(payer),
	)
}

// The getLatestBlockhash answer: a result or an rpc error.
type solanaLatestBlockhashResponse struct {
	Result *struct {
		Value struct {
			Blockhash string `json:"blockhash"`
		} `json:"value"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Asks the server's Helius rpc for a recent blockhash. The request names no
// account.
func solanaRpcLatestBlockhash(ctx context.Context) (solana.Hash, error) {
	ctx, cancel := context.WithTimeout(ctx, solanaRpcBlockhashTimeout)
	defer cancel()

	response, err := server.HttpPostRequireStatusOk(
		ctx,
		solanaRpcUrlFunc(),
		map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "getLatestBlockhash",
			"params": []any{
				map[string]any{"commitment": "confirmed"},
			},
		},
		server.NoCustomHeaders,
		server.ResponseJsonObject[*solanaLatestBlockhashResponse],
	)
	if err != nil {
		return solana.Hash{}, fmt.Errorf("getLatestBlockhash: %w", err)
	}
	if response == nil {
		return solana.Hash{}, errors.New("getLatestBlockhash: empty response")
	}
	if response.Error != nil {
		return solana.Hash{}, fmt.Errorf("getLatestBlockhash: rpc error %d: %s", response.Error.Code, response.Error.Message)
	}
	if response.Result == nil {
		return solana.Hash{}, errors.New("getLatestBlockhash: no result")
	}
	return solana.HashFromBase58(response.Result.Value.Blockhash)
}

var solanaRpcApiKeyPattern = regexp.MustCompile(`api-key=[^&\s"']*`)

// An rpc failure for the log without the endpoint url, whose query carries the
// api key.
func solanaRpcErrorText(err error) string {
	text := err.Error()
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		text = fmt.Sprintf("%s: %s", urlErr.Op, urlErr.Err)
	}
	return solanaRpcApiKeyPattern.ReplaceAllString(text, "api-key=<redacted>")
}
