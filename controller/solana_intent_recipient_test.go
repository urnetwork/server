package controller

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/model"
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
