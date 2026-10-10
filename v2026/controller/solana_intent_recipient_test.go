package controller

import (
	"encoding/base64"
	"encoding/json"
	"slices"
	"testing"

	"github.com/gagliardetto/solana-go"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Where the server tells a client to pay. Every intent path names the same
// receiver, the first of solanaReceiverAddresses, so a rotation reaches every
// client with its next quote. The data pack intent is checked against the
// database in TestPayDataSolanaIntentNamesWhereToPay.

// A receiver the server rotated to: a fixture key (sha256 of the phrase), not
// a wallet, and its USDC account as solana-go derives it. ur.io's transfer
// fixture pins the same pair (mmm ur.io/react/tests/usdc-transfer-fixture.mjs,
// ROTATED_MERCHANT), so both sides test the same rotation.
const (
	solanaTestRotatedReceiverPhrase      = "urnetwork test rotated receiver"
	solanaTestRotatedReceiver            = "Fi3zUczEY3MPrXMk4GTVgxXth1Mnv1ndknPPFFnLScXs"
	solanaTestRotatedReceiverUsdcAccount = "9FkEPt4nUc2cV3jzbYe96FFxyUD5FBJLRKPpkzdW7YmM"
)

// §4.3: a successful intent tells the client where to pay -- the merchant
// address and the USDC mint the webhook credits -- so no client hardcodes them.
// Pure: no db.
func TestSolanaPaymentIntentQuoteNamesWhereToPay(t *testing.T) {
	result := solanaPaymentIntentQuote(40, 40, "default", model.SolanaPlanYearly, false)

	resultJson, err := json.Marshal(result)
	connect.AssertEqual(t, err, nil)
	fields := map[string]any{}
	connect.AssertEqual(t, json.Unmarshal(resultJson, &fields), nil)

	recipient, _ := fields["recipient"].(string)
	splTokenMint, _ := fields["spl_token_mint"].(string)
	if recipient == "" || splTokenMint == "" {
		t.Fatalf("intent result %s does not say where to pay (recipient %q, spl_token_mint %q)", resultJson, recipient, splTokenMint)
	}

	// a payment to the quoted recipient in the quoted mint is one the webhook credits
	connect.AssertEqual(t, slices.Contains(solanaReceiverAddresses, recipient), true)
	connect.AssertEqual(t, splTokenMint, solanaUsdcMint)
	// and it is the current address, not the one being deprecated
	connect.AssertEqual(t, recipient, solanaReceiverAddresses[0])
}

// A rotated receiver (a new address first, the old one after it) is what the
// plan intent quotes and what the browser-wallet transfer for an intent pays.
// Pure: the intent lookup and the rpc are seams.
func TestSolanaPaymentPathsFollowRotatedReceiver(t *testing.T) {
	rotatedReceiver := solanaTestKey(solanaTestRotatedReceiverPhrase)
	connect.AssertEqual(t, rotatedReceiver.String(), solanaTestRotatedReceiver)
	solanaTestReceivers(t, rotatedReceiver.String(), solanaTestMerchant)

	quoteJson, err := json.Marshal(solanaPaymentIntentQuote(40, 40, "default", model.SolanaPlanYearly, false))
	connect.AssertEqual(t, err, nil)
	quote := map[string]any{}
	connect.AssertEqual(t, json.Unmarshal(quoteJson, &quote), nil)
	connect.AssertEqual(t, quote["recipient"], rotatedReceiver.String())
	connect.AssertEqual(t, quote["spl_token_mint"], solanaUsdcMint)

	networkId := server.NewId()
	installSolanaFakePaymentTransactionDeps(t, solanaTestOpenIntent(networkId))
	result, err := CreateSolanaPaymentTransaction(&SolanaPaymentTransactionArgs{
		Reference: solanaTestReference,
		Payer:     solanaTestPayer,
	}, solanaTestPaymentSession(networkId))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error, (*SolanaPaymentTransactionError)(nil))
	transactionBytes, err := base64.StdEncoding.DecodeString(result.Transaction)
	connect.AssertEqual(t, err, nil)

	// TransferChecked's accounts: the source, the mint, the destination, the
	// owner and the reference. The destination is the rotated receiver's USDC
	// account, not the one being retired.
	rotatedUsdcAccount, _, err := solana.FindAssociatedTokenAddress(rotatedReceiver, solana.MustPublicKeyFromBase58(solanaUsdcMint))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, rotatedUsdcAccount.String(), solanaTestRotatedReceiverUsdcAccount)
	connect.AssertEqual(t, solanaDecodeTransfer(t, transactionBytes).accounts[1][2], solanaTestRotatedReceiverUsdcAccount)
}
