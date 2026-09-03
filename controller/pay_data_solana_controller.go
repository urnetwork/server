package controller

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mr-tron/base58"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Buy data with USDC on Solana, without signing in.
//
// POST /pay/data/solana-intent quotes a data pack for a NAMED network and records
// the payment intent the Helius webhook (POST /pay/solana) credits when the USDC
// arrives at one of our receiving addresses. The buyer sends exactly the quoted
// amount either through a Solana Pay url, whose wallet attaches the reference as
// an account key of the transfer, or by hand from any wallet or exchange with the
// reference as the transfer MEMO. The webhook matches both
// (solanaReferenceCandidates), so there is no email, no hosted checkout and no
// code: the data lands on the network.
//
// POST /pay/data/solana-status is what the buy-data page polls while it waits.
//
// A data pack intent is a solana_payment_intent row whose subscription_plan is
// the data item id ("data_1tib", "data_10tib") instead of a plan name, which is
// how the webhook tells the two apart when it credits (solanaCreditPaymentIntent).
//
// Both endpoints are unauthenticated and per-ip rate limited.

const (
	// a hand-typed transfer from an exchange can take a while; the plan intents
	// made by a signed-in wallet flow expire after an hour
	payDataSolanaIntentDuration = 24 * time.Hour

	payDataSolanaReferenceMinLength = 8
	payDataSolanaReferenceMaxLength = 128
)

const (
	PayDataSolanaStatusPending = "pending"
	PayDataSolanaStatusPaid    = "paid"
	PayDataSolanaStatusExpired = "expired"
	PayDataSolanaStatusUnknown = "unknown"
)

type PayDataSolanaIntentArgs struct {
	// "data_1tib" or "data_10tib"
	ItemId string `json:"item_id"`
	// the network that receives the data. Required: with no sign-in and no
	// email, the network is the only place the data can go.
	NetworkName string `json:"network_name"`
	// the Solana Pay reference the client generated (a base58 public key). It is
	// also the transfer memo for a payment sent by hand.
	Reference string `json:"reference"`
}

type PayDataSolanaIntentResult struct {
	// the exact USDC amount to send, quoted from pro.yml
	AmountUsd float64 `json:"amount_usd,omitempty"`
	Reference string  `json:"reference,omitempty"`
	// what to put in the transfer memo when paying by hand: the reference
	Memo      string     `json:"memo,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// the resolved network
	NetworkName string                `json:"network_name,omitempty"`
	NetworkId   *server.Id            `json:"network_id,omitempty"`
	Error       *PayDataCheckoutError `json:"error,omitempty"`
}

func payDataSolanaIntentError(message string) *PayDataSolanaIntentResult {
	return &PayDataSolanaIntentResult{
		Error: &PayDataCheckoutError{Message: message},
	}
}

type PayDataSolanaStatusArgs struct {
	Reference string `json:"reference"`
}

type PayDataSolanaStatusResult struct {
	// "pending", "paid", "expired" or "unknown"
	Status string `json:"status"`
	// the data item the intent is for, when known
	ItemId      string     `json:"item_id,omitempty"`
	NetworkName string     `json:"network_name,omitempty"`
	AmountUsd   float64    `json:"amount_usd,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

// dataPackPriceUsd is the price of a data item, from pro.yml (data_code.skus):
// the same number the site shows, Stripe charges and x402 quotes, so a buyer is
// never shown two prices. ok = false for an unknown item or an unusable price.
func dataPackPriceUsd(itemId string) (float64, bool) {
	byteCount, ok := stripeDataPackByteCount(itemId)
	if !ok {
		return 0, false
	}
	priceUsd, ok := stripeDataPackPriceUsd(byteCount)
	if !ok || priceUsd <= 0 {
		return 0, false
	}
	return priceUsd, true
}

// solanaIsDataPackPlan reports whether an intent's subscription_plan names a
// data item rather than a subscription plan.
func solanaIsDataPackPlan(subscriptionPlan string) bool {
	_, ok := stripeDataPackByteCount(subscriptionPlan)
	return ok
}

// payDataSolanaValidReference is a shape check on the client-generated
// reference. A Solana Pay reference is a base58 public key (32 to 44
// characters); the check only refuses what could never be matched or stored
// sensibly: too short to be unique, too long to be a memo, whitespace or
// control characters (the memo is compared trimmed and exact).
func payDataSolanaValidReference(reference string) bool {
	if len(reference) < payDataSolanaReferenceMinLength || payDataSolanaReferenceMaxLength < len(reference) {
		return false
	}
	for _, r := range reference {
		if r <= ' ' || '~' < r {
			return false
		}
	}
	return true
}

// -----------------------------------------------------------------------------
// intent
// -----------------------------------------------------------------------------

func PayDataSolanaIntent(
	args *PayDataSolanaIntentArgs,
	clientSession *session.ClientSession,
) (*PayDataSolanaIntentResult, error) {
	itemId := strings.TrimSpace(args.ItemId)
	if _, ok := stripeDataPackByteCount(itemId); !ok {
		return payDataSolanaIntentError("Unknown item."), nil
	}
	reference := strings.TrimSpace(args.Reference)
	if !payDataSolanaValidReference(reference) {
		return payDataSolanaIntentError("Invalid payment reference."), nil
	}
	networkName := strings.TrimSpace(args.NetworkName)
	if networkName == "" {
		return payDataSolanaIntentError("Enter the network that should receive the data."), nil
	}

	if !payDataCheckoutLimiter.allow(clientSession) {
		return payDataSolanaIntentError("Too many payment attempts from this address. Try again in a minute."), nil
	}

	// The price comes from pro.yml, keyed by the item. It is NEVER taken from the
	// client: the webhook checks the received amount against this quote.
	priceUsd, ok := dataPackPriceUsd(itemId)
	if !ok {
		glog.Errorf("[paydata]no usable price in pro.yml for data pack %s\n", itemId)
		return payDataSolanaIntentError("That data pack is not available."), nil
	}

	networkId, storedName := model.FindNetworkByName(clientSession.Ctx, networkName)
	if networkId == nil {
		return payDataSolanaIntentError(fmt.Sprintf("No network named %s", networkName)), nil
	}

	expiresAt := server.NowUtc().Add(payDataSolanaIntentDuration)
	err := model.CreateSolanaPaymentIntentForNetwork(
		clientSession.Ctx,
		reference,
		*networkId,
		priceUsd,
		itemId,
		expiresAt,
	)
	if err != nil {
		// a duplicate reference or a failed insert must not send the buyer off to
		// pay against an intent that does not exist
		glog.Errorf("[paydata]could not create solana intent %s for %s: %s\n", reference, itemId, err)
		return payDataSolanaIntentError("Could not start the payment. Please try again."), nil
	}

	glog.Infof(
		"[paydata]solana intent %s: %s for network %s, %.2f USDC\n",
		reference, itemId, *networkId, priceUsd,
	)

	return &PayDataSolanaIntentResult{
		AmountUsd:   priceUsd,
		Reference:   reference,
		Memo:        reference,
		ExpiresAt:   &expiresAt,
		NetworkName: storedName,
		NetworkId:   networkId,
	}, nil
}

// -----------------------------------------------------------------------------
// status
// -----------------------------------------------------------------------------

func PayDataSolanaStatus(
	args *PayDataSolanaStatusArgs,
	clientSession *session.ClientSession,
) (*PayDataSolanaStatusResult, error) {
	reference := strings.TrimSpace(args.Reference)
	if !payDataSolanaValidReference(reference) {
		return &PayDataSolanaStatusResult{Status: PayDataSolanaStatusUnknown}, nil
	}
	if !payDataLookupLimiter.allow(clientSession) {
		return nil, errors.New("Too many status checks from this address. Try again in a minute.")
	}

	intent := model.GetSolanaPaymentIntent(clientSession.Ctx, reference)
	// only data pack intents are reported here: a plan intent belongs to a
	// signed-in account, which sees its own status through its session
	if intent == nil || !solanaIsDataPackPlan(intent.SubscriptionPlan) {
		return &PayDataSolanaStatusResult{Status: PayDataSolanaStatusUnknown}, nil
	}

	result := &PayDataSolanaStatusResult{
		Status:    payDataSolanaStatusOf(intent, server.NowUtc()),
		ItemId:    intent.SubscriptionPlan,
		AmountUsd: intent.ExpectedAmountUsd,
		ExpiresAt: intent.ExpiresAt,
	}
	if networkName, _, ok := model.GetNetworkAdminUserAuth(clientSession.Ctx, intent.NetworkId); ok {
		result.NetworkName = networkName
	}
	return result, nil
}

// payDataSolanaStatusOf maps an intent row to the status the page shows. A
// consumed intent is paid whatever its expiry (a late payment still credits);
// an open one past its expiry is expired.
func payDataSolanaStatusOf(intent *model.SolanaPaymentIntent, now time.Time) string {
	if intent.TxSignature != nil {
		return PayDataSolanaStatusPaid
	}
	if intent.ExpiresAt != nil && intent.ExpiresAt.Before(now) {
		return PayDataSolanaStatusExpired
	}
	return PayDataSolanaStatusPending
}

// -----------------------------------------------------------------------------
// webhook: reference candidates
// -----------------------------------------------------------------------------

// The SPL memo program, both versions. A memo instruction's data is the utf8
// memo text.
var solanaMemoProgramIds = []string{
	"Memo1UhkJRfHyvLMcVucJwxXeuD728EqVDDwQDxFMNo",
	"MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr",
}

// the memo program refuses anything longer
const solanaMemoMaxBytes = 566

// solanaReferenceCandidates lists every string an intent's reference could be
// found under in a Helius transaction, in order and without duplicates:
//
//   - the account keys: a Solana Pay wallet attaches the `reference` of the
//     payment url as a read-only account of the transfer
//   - the memo texts: a buyer paying by hand from an exchange or a wallet
//     includes the reference as the transfer memo (top-level and inner
//     instructions of either memo program)
//
// Helius carries instruction data base58 encoded; the decoded utf8 text is the
// memo. The raw data string is kept as a candidate too, so a payload that
// carries the memo text verbatim still matches. Matching is exact, so an extra
// candidate can never credit the wrong intent.
func solanaReferenceCandidates(transaction *SolanaTransaction) []string {
	candidates := []string{}
	seen := map[string]bool{}
	add := func(candidate string) {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || seen[candidate] {
			return
		}
		seen[candidate] = true
		candidates = append(candidates, candidate)
	}

	for _, accountData := range transaction.AccountData {
		add(accountData.Account)
	}
	for _, instruction := range transaction.Instructions {
		for _, memo := range solanaMemoTexts(instruction.ProgramId, instruction.Data) {
			add(memo)
		}
		for _, inner := range instruction.InnerInstructions {
			for _, memo := range solanaMemoTexts(inner.ProgramId, inner.Data) {
				add(memo)
			}
		}
	}
	return candidates
}

// solanaMemoTexts returns the memo texts a memo program instruction could
// carry: the base58-decoded data when it is utf8 text, and the data verbatim.
// Empty for any other program.
func solanaMemoTexts(programId string, data string) []string {
	if !slices.Contains(solanaMemoProgramIds, programId) {
		return nil
	}
	data = strings.TrimSpace(data)
	if data == "" || solanaMemoMaxBytes < len(data) {
		return nil
	}
	memos := []string{}
	if decoded, err := base58.Decode(data); err == nil && 0 < len(decoded) && utf8.Valid(decoded) {
		memos = append(memos, string(decoded))
	}
	memos = append(memos, data)
	return memos
}

// -----------------------------------------------------------------------------
// webhook: credit a data pack
// -----------------------------------------------------------------------------

// solanaCreditDataPack consumes a data pack intent and lands the data on the
// network it was bought for, in ONE tx, the way solanaCreditPaymentIntent grants
// a plan. The balance is data only (pro = false), valid for the data code
// duration from pro.yml and carries the received USDC as revenue. There is no
// code: nothing was emailed and nothing needs redeeming.
//
// The applied note goes to the network's admin login when it is an email,
// after the credit and best effort: the data is already there.
func solanaCreditDataPack(
	clientSession *session.ClientSession,
	paymentSearchResult *model.PaymentIntentSearchResult,
	signature string,
	tokenAmountReceivedUsd float64,
) (credited bool, returnErr error) {
	itemId := paymentSearchResult.SubscriptionPlan
	byteCount, ok := stripeDataPackByteCount(itemId)
	if !ok || byteCount <= 0 {
		return false, fmt.Errorf("unknown data pack %s", itemId)
	}
	if paymentSearchResult.NetworkId == nil {
		return false, fmt.Errorf("data pack intent %s has no network", paymentSearchResult.PaymentReference)
	}
	// This is a PAID path. A zero duration would land data that expires the
	// instant it is created; refuse instead, so the webhook retries and the
	// failure is visible (the intent stays open).
	duration := model.Pro().DataCodeDuration
	if duration <= 0 {
		glog.Errorf(
			"[paydata]refusing to apply data pack %s with a zero duration (reference %s). Is pro.yml present?\n",
			itemId, paymentSearchResult.PaymentReference,
		)
		return false, fmt.Errorf("data code duration is not configured (pro.yml)")
	}

	networkId := *paymentSearchResult.NetworkId
	now := server.NowUtc()
	netRevenue := model.UsdToNanoCents(tokenAmountReceivedUsd)

	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		completed, err := model.MarkPaymentIntentCompletedInTx(
			tx,
			paymentSearchResult.PaymentReference,
			signature,
			clientSession,
		)
		if err != nil {
			returnErr = err
			return
		}
		if !completed {
			glog.Infof("[paydata]solana data pack: intent %s already completed; not applying again\n", paymentSearchResult.PaymentReference)
			return
		}
		// pro = false: a data pack is DATA ONLY, exactly like a redeemed code
		model.AddTransferBalanceInTx(
			clientSession.Ctx,
			tx,
			&model.TransferBalance{
				NetworkId:             networkId,
				StartTime:             now,
				EndTime:               now.Add(duration),
				StartBalanceByteCount: byteCount,
				BalanceByteCount:      byteCount,
				NetRevenue:            netRevenue,
				Pro:                   false,
			},
		)
		credited = true
	})
	if returnErr != nil {
		return false, returnErr
	}
	if !credited {
		return false, nil
	}

	glog.Infof(
		"[paydata]solana data pack %s applied to network %s (%s, %.2f USDC, %s)\n",
		itemId, networkId, model.ByteCountHumanReadable(byteCount), tokenAmountReceivedUsd, signature,
	)
	solanaSendDataAppliedNote(clientSession, networkId, byteCount)
	return true, nil
}

// solanaSendDataAppliedNote emails the network's admin that the data is on the
// network, when their login is an email. Never fails the credit: the money
// moved and the data landed; a missed note is not worth a webhook retry.
func solanaSendDataAppliedNote(
	clientSession *session.ClientSession,
	networkId server.Id,
	byteCount model.ByteCount,
) {
	networkName, userAuth, ok := model.GetNetworkAdminUserAuth(clientSession.Ctx, networkId)
	if !ok || userAuth == "" {
		return
	}
	normalUserAuth, userAuthType := model.NormalUserAuth(userAuth)
	if userAuthType != model.UserAuthTypeEmail {
		return
	}
	err := GetAWSMessageSender().SendAccountMessageTemplate(
		normalUserAuth,
		&SubscriptionDataAppliedTemplate{
			BalanceByteCount: byteCount,
			NetworkName:      networkName,
		},
	)
	if err != nil {
		glog.Infof("[paydata]could not send the data applied note for network %s: %s\n", networkId, err)
	}
}
