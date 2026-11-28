package controller

// Pay with Gamma: claims of direct payments (the owner, 2026-11-20: "Build the
// claim flow now"; model/gamma_claim_model.go).
//
// Since runtime 493 a deposit's own hotkey can spend its gamma at any service
// listed on the subnet, ours included, without our API: a direct payment. The
// chain keeps what each hotkey has spent at our service (GammaSpent, a total
// that only grows), so a direct payment is never lost, but nothing says which
// account it was for. A claim says it:
//
//	POST /pay/gamma/claim/challenge  bearer: the one line to sign, and what is claimable now
//	POST /pay/gamma/claim            bearer: the signed line; records the claim
//
// The customer signs the challenge with the paying hotkey (btcli >= 11.3.5:
// btcli wallet sign --use-hotkey --message "<line>"), and the claim takes the
// part of that hotkey's total that no claim has taken yet, read from the chain
// at the finalized head. That is retroactive by construction: a payment made
// before this flow existed is in the total. The claim is recorded as an intent
// of purpose claim, already paid, and the biller credits it like any other:
// data at the 1 TiB pack's gamma price, or the Pro plan the claim names when
// the claimable amount covers its price (the rest stays claimable).
//
// It fails closed. The challenge names the network, the hotkey and our
// service, and lasts 15 minutes. A signature that does not verify, a hotkey we
// assigned (its spends are our own charges), Gamma unavailable, or a chain
// read that fails claims nothing. One hotkey's claims are serialized in the
// database, so the same gamma is never claimed twice, and a claim answered
// twice credits once.

import (
	"crypto/sha256"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Error codes of the claim endpoints, beside the shared ones
// (invalid_request, asset_unavailable, rate_limited, plan_unavailable,
// already_subscribed) and insufficient_gamma.
const (
	// the hotkey is one of the deposit hotkeys we assigned: what it spent at
	// our service were our own charges, credited already
	GammaErrorCodeDepositHotkey = "deposit_hotkey"
	// the hotkey has spent nothing at our service that is not claimed already
	GammaErrorCodeNothingToClaim = "nothing_to_claim"
	// the signature is not the hotkey's of the challenge
	GammaErrorCodeSignatureInvalid = "signature_invalid"
	// the challenge is older than its 15 minutes: ask for a new one
	GammaErrorCodeClaimExpired = "claim_expired"
)

// The challenge's form: one line, so a shell command can carry it.
const gammaClaimMessagePrefix = "URnetwork Gamma claim v1:"

// How long a challenge may be signed and sent, and the clock skew allowed.
const (
	gammaClaimTtl  = 15 * time.Minute
	gammaClaimSkew = time.Minute
)

// The claims and challenges one network may ask for in a minute.
const gammaClaimsPerNetworkPerMinute = 10

var gammaClaimLimiter = &payDataIpLimiter{limitPerMinute: gammaClaimsPerNetworkPerMinute}

func gammaClaimAllow(clientSession *session.ClientSession) bool {
	networkId := clientSession.ByJwt.NetworkId
	return gammaClaimLimiter.allowHash(sha256.Sum256(append([]byte("gamma-claim/"), networkId.Bytes()...)), server.NowUtc())
}

// gammaClaimMessage is the challenge a network signs to claim a hotkey's
// direct payments to our service.
func gammaClaimMessage(networkId server.Id, hotkey string, service string, issued time.Time) string {
	return fmt.Sprintf("%s network %s hotkey %s service %s issued %d", gammaClaimMessagePrefix, networkId, hotkey, service, issued.Unix())
}

// gammaClaimChallenge is a challenge as signed.
type gammaClaimChallenge struct {
	NetworkId server.Id
	Hotkey    string
	Service   string
	Issued    time.Time
}

// gammaParseClaimMessage reads a challenge; ok = false for anything not in
// its exact form.
func gammaParseClaimMessage(message string) (*gammaClaimChallenge, bool) {
	rest, found := strings.CutPrefix(message, gammaClaimMessagePrefix)
	if !found {
		return nil, false
	}
	fields := strings.Split(rest, " ")
	if len(fields) != 9 || fields[0] != "" || fields[1] != "network" || fields[3] != "hotkey" || fields[5] != "service" || fields[7] != "issued" {
		return nil, false
	}
	networkId, err := server.ParseId(fields[2])
	if err != nil {
		return nil, false
	}
	issued, err := strconv.ParseInt(fields[8], 10, 64)
	if err != nil || issued <= 0 {
		return nil, false
	}
	challenge := &gammaClaimChallenge{
		NetworkId: networkId,
		Hotkey:    fields[4],
		Service:   fields[6],
		Issued:    time.Unix(issued, 0).UTC(),
	}
	// the exact form only: what was parsed writes the same line back
	if gammaClaimMessage(challenge.NetworkId, challenge.Hotkey, challenge.Service, challenge.Issued) != message {
		return nil, false
	}
	return challenge, true
}

// gammaClaimSignCommand is how to sign the challenge with btcli 11.3.5 or
// later: the hotkey's wallet signs, and btcli prints the signature as bare
// hex (signed_message).
func gammaClaimSignCommand(message string) string {
	return fmt.Sprintf("btcli wallet sign --use-hotkey --message \"%s\" -w <your_wallet> -H <your_hotkey>", message)
}

// gammaClaimDataBytes is the data a claimed amount buys: at the gamma price of
// the 1 TiB data pack (pro.yml), rounded down to a whole MiB. ok = false when
// that pack is not sold with Gamma.
func gammaClaimDataBytes(amount int64) (model.ByteCount, bool) {
	priceUsd, ok := dataPackPriceUsd(StripeItemData1Tib, model.DataRailGamma)
	if !ok || priceUsd <= 0 || amount <= 0 {
		return 0, false
	}
	return gammaClaimDataBytesAt(amount, model.UsdToNanoCents(priceUsd)), true
}

// gammaClaimDataBytesAt is the data amount buys at priceUnits per TiB, rounded
// down to a whole MiB.
func gammaClaimDataBytesAt(amount int64, priceUnits int64) model.ByteCount {
	if amount <= 0 || priceUnits <= 0 {
		return 0
	}
	bytes := new(big.Int).Mul(big.NewInt(amount), big.NewInt(int64(model.Tib)))
	bytes.Div(bytes, big.NewInt(priceUnits))
	mib := new(big.Int).Div(bytes, big.NewInt(int64(model.Mib)))
	return model.ByteCount(mib.Int64()) * model.Mib
}

// gammaClaimRead is a hotkey's total at our service, read at the finalized
// head, with what was claimed of it already.
type gammaClaimRead struct {
	spend     *model.GammaClaimSpend
	claimed   int64
	claimable int64
}

// gammaReadClaimable reads a hotkey's GammaSpent total at our service at the
// finalized head, under a runtime this code reviewed.
func gammaReadClaimable(clientSession *session.ClientSession, settings *gammaSettings, address string, hotkey [32]byte) (*gammaClaimRead, error) {
	ctx := clientSession.Ctx
	chain := gammaChainFor(settings.Endpoints())
	head, headNumber, err := chain.FinalizedHead(ctx)
	if err != nil {
		return nil, err
	}
	runtime, err := chain.RuntimeAt(ctx, head)
	if err != nil {
		return nil, err
	}
	if err := runtime.Supported(); err != nil {
		return nil, err
	}
	total, err := chain.Spent(ctx, runtime, head, settings.Netuid, settings.ServiceHotkey, hotkey)
	if err != nil {
		return nil, err
	}
	if total > uint64(1<<62) {
		return nil, fmt.Errorf("GammaSpent of %s is out of range", address)
	}
	read := &gammaClaimRead{
		spend: &model.GammaClaimSpend{
			OnChainTotal: int64(total),
			BlockNumber:  int64(headNumber),
			BlockHash:    head.Hex(),
		},
		claimed: model.GetGammaClaimedTotal(ctx, address),
	}
	read.claimable = read.spend.OnChainTotal - read.claimed
	return read, nil
}

func gammaClaimError(code string, message string) *CryptoPaymentError {
	return &CryptoPaymentError{Code: code, Message: message}
}

// gammaClaimHotkey reads the hotkey a claim names: its address and key, or
// the refusal (not an address; one of our deposit hotkeys).
func gammaClaimHotkey(clientSession *session.ClientSession, text string) (string, [32]byte, *CryptoPaymentError) {
	address := strings.TrimSpace(text)
	publicKey, err := model.DecodeBittensorAddress(address)
	if err != nil {
		return "", [32]byte{}, gammaClaimError(PurchaseErrorCodeInvalidRequest, "Name the SS58 address of the hotkey that paid.")
	}
	if model.GetGammaDepositHotkey(clientSession.Ctx, address) != nil {
		return "", [32]byte{}, gammaClaimError(GammaErrorCodeDepositHotkey, "That is a deposit hotkey URnetwork assigned: what it spent were URnetwork's own charges, credited already. There is nothing to claim.")
	}
	return address, publicKey, nil
}

// ----- the challenge -----

type GammaClaimChallengeArgs struct {
	// the SS58 address of the hotkey that paid
	Hotkey string `json:"hotkey"`
}

type GammaClaimChallengeResult struct {
	Hotkey        string `json:"hotkey"`
	Netuid        uint16 `json:"netuid"`
	ServiceHotkey string `json:"service_hotkey"`
	// the one line to sign with the hotkey, exactly
	Message   string     `json:"message"`
	IssuedAt  *time.Time `json:"issued_at,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// how to sign it with btcli 11.3.5 or later
	SignCommand string `json:"sign_command"`
	// what the hotkey spent at our service that no claim has taken, at the
	// finalized head, and the data it buys
	ClaimableGamma     string              `json:"claimable_gamma"`
	ClaimableDataBytes model.ByteCount     `json:"claimable_data_bytes"`
	Error              *CryptoPaymentError `json:"error,omitempty"`
}

// GammaClaimChallenge answers POST /pay/gamma/claim/challenge.
func GammaClaimChallenge(args *GammaClaimChallengeArgs, clientSession *session.ClientSession) (*GammaClaimChallengeResult, error) {
	refuse := func(err *CryptoPaymentError) (*GammaClaimChallengeResult, error) {
		return &GammaClaimChallengeResult{Hotkey: strings.TrimSpace(args.Hotkey), Error: err}, nil
	}
	if !gammaClaimAllow(clientSession) {
		return refuse(gammaClaimError(PurchaseErrorCodeRateLimited, "Too many claims from this account. Try again in a minute."))
	}
	view, refusal := gammaPurchaseView(clientSession)
	if refusal != nil {
		return refuse(refusal.Error)
	}
	address, publicKey, hotkeyErr := gammaClaimHotkey(clientSession, args.Hotkey)
	if hotkeyErr != nil {
		return refuse(hotkeyErr)
	}
	read, err := gammaReadClaimable(clientSession, view.settings, address, publicKey)
	if err != nil {
		glog.Warningf("[gamma]claim challenge for %s: the chain read failed: %s\n", address, err)
		return refuse(gammaClaimError(PurchaseErrorCodeAssetUnavailable, gammaUnavailableMessage))
	}
	result := &GammaClaimChallengeResult{
		Hotkey:         address,
		Netuid:         view.settings.Netuid,
		ServiceHotkey:  view.settings.ServiceAddress,
		ClaimableGamma: gammaFormatUnits(max(read.claimable, 0)),
	}
	if read.claimable <= 0 {
		result.Error = gammaClaimError(GammaErrorCodeNothingToClaim, "This hotkey has spent nothing at URnetwork's SN25 service that is not claimed already.")
		return result, nil
	}
	if bytes, ok := gammaClaimDataBytes(read.claimable); ok {
		result.ClaimableDataBytes = bytes
	}
	issued := gammaNow().Truncate(time.Second)
	expires := issued.Add(gammaClaimTtl)
	result.Message = gammaClaimMessage(clientSession.ByJwt.NetworkId, address, view.settings.ServiceAddress, issued)
	result.IssuedAt = &issued
	result.ExpiresAt = &expires
	result.SignCommand = gammaClaimSignCommand(result.Message)
	return result, nil
}

// ----- the claim -----

type GammaClaimArgs struct {
	// the SS58 address of the hotkey that paid
	Hotkey string `json:"hotkey"`
	// the challenge, exactly as POST /pay/gamma/claim/challenge gave it
	Message string `json:"message"`
	// the hotkey's signature of it, in hex (btcli's signed_message)
	Signature string `json:"signature"`
	// "" credits everything claimable as data; monthly or yearly buys that Pro
	// plan when the claimable amount covers its price, and leaves the rest
	Plan string `json:"plan,omitempty"`
}

type GammaClaimResult struct {
	IntentId    string `json:"intent_id"`
	StatusToken string `json:"status_token"`
	Hotkey      string `json:"hotkey"`
	Plan        string `json:"plan"`
	// what this claim took, in gamma, and the data it credits (0 for a plan)
	AmountGamma string          `json:"amount_gamma"`
	DataBytes   model.ByteCount `json:"data_bytes"`
	// what stays claimable after it
	RemainingGamma string                    `json:"remaining_gamma"`
	Status         *GammaPaymentStatusResult `json:"status,omitempty"`
	Error          *CryptoPaymentError       `json:"error,omitempty"`
}

// GammaClaim answers POST /pay/gamma/claim.
func GammaClaim(args *GammaClaimArgs, clientSession *session.ClientSession) (*GammaClaimResult, error) {
	ctx := clientSession.Ctx
	networkId := clientSession.ByJwt.NetworkId
	refuse := func(err *CryptoPaymentError) (*GammaClaimResult, error) {
		return &GammaClaimResult{Hotkey: strings.TrimSpace(args.Hotkey), Plan: strings.TrimSpace(args.Plan), Error: err}, nil
	}
	if !gammaClaimAllow(clientSession) {
		return refuse(gammaClaimError(PurchaseErrorCodeRateLimited, "Too many claims from this account. Try again in a minute."))
	}
	view, refusal := gammaPurchaseView(clientSession)
	if refusal != nil {
		return refuse(refusal.Error)
	}
	settings := view.settings
	address, publicKey, hotkeyErr := gammaClaimHotkey(clientSession, args.Hotkey)
	if hotkeyErr != nil {
		return refuse(hotkeyErr)
	}

	// the challenge: this network, this hotkey, our service, fresh
	challenge, ok := gammaParseClaimMessage(args.Message)
	if !ok || challenge.NetworkId != networkId || challenge.Hotkey != address || challenge.Service != settings.ServiceAddress {
		return refuse(gammaClaimError(PurchaseErrorCodeInvalidRequest, "The message is not this account's challenge for this hotkey. Ask for a new one and sign it exactly."))
	}
	now := gammaNow()
	if challenge.Issued.After(now.Add(gammaClaimSkew)) || challenge.Issued.Add(gammaClaimTtl).Before(now) {
		return refuse(gammaClaimError(GammaErrorCodeClaimExpired, "The challenge has expired. Ask for a new one and sign it."))
	}
	if verified, err := model.VerifyBittensorSignature(address, args.Message, args.Signature); err != nil || !verified {
		return refuse(gammaClaimError(GammaErrorCodeSignatureInvalid, "The signature is not this hotkey's signature of the challenge. Sign it with the hotkey that paid (btcli wallet sign --use-hotkey)."))
	}

	plan := strings.TrimSpace(args.Plan)
	tierName := ""
	var planPrice int64
	switch plan {
	case "":
	case model.SolanaPlanMonthly, model.SolanaPlanYearly:
		if gammaOtherProRenews(clientSession) {
			return refuse(gammaClaimError(PurchaseErrorCodeAlreadySubscribed, "You already have a Pro subscription that renews, so this claim would pay for the same days twice. Claim it as data, or cancel that subscription first."))
		}
		if charging, _ := model.GetGammaPlanChargeState(ctx, networkId, gammaKnownPlans, nil, nil); charging {
			return refuse(gammaClaimError(PurchaseErrorCodeAlreadySubscribed, gammaPlanChargingPurchaseMessage))
		}
		tier := ResolvePriceTier(clientSession, "")
		priceUsd, ok := solanaPlanPriceUsdForTier(plan, tier.Tier)
		if !ok {
			return refuse(gammaClaimError(PurchaseErrorCodePlanUnavailable, "That plan is not available."))
		}
		tierName = tier.Tier.Name
		planPrice = model.UsdToNanoCents(priceUsd)
	default:
		return refuse(gammaClaimError(PurchaseErrorCodePlanUnavailable, "Claim as data, or name the monthly or yearly plan."))
	}

	read, err := gammaReadClaimable(clientSession, settings, address, publicKey)
	if err != nil {
		glog.Warningf("[gamma]claim for %s: the chain read failed: %s\n", address, err)
		return refuse(gammaClaimError(PurchaseErrorCodeAssetUnavailable, gammaUnavailableMessage))
	}
	if read.claimable <= 0 {
		return refuse(gammaClaimError(GammaErrorCodeNothingToClaim, "This hotkey has spent nothing at URnetwork's SN25 service that is not claimed already."))
	}

	amount := read.claimable
	item := plan
	var dataBytes model.ByteCount
	if plan != "" {
		if read.claimable < planPrice {
			return refuse(gammaClaimError(GammaErrorCodeInsufficientGamma, fmt.Sprintf("The plan is %s gamma; this hotkey has %s gamma to claim. Claim it as data instead.", gammaFormatUnits(planPrice), gammaFormatUnits(read.claimable))))
		}
		amount = planPrice
	} else {
		bytes, ok := gammaClaimDataBytes(amount)
		if !ok || bytes <= 0 {
			return refuse(gammaClaimError(PurchaseErrorCodeAssetUnavailable, gammaUnavailableMessage))
		}
		dataBytes = bytes
		item = model.GammaClaimDataItem(bytes)
	}

	token, tokenHash := cryptoNewStatusToken()
	intent := &model.GammaPaymentIntent{
		IntentId:                 server.NewId(),
		StatusTokenHash:          tokenHash,
		NetworkId:                networkId,
		Hotkey:                   address,
		Purpose:                  model.GammaPurposeClaim,
		Reference:                gammaNewReference(),
		Delivery:                 model.CryptoDeliveryAccount,
		Item:                     item,
		Tier:                     tierName,
		PriceUsdNanoCents:        amount,
		RegularPriceUsdNanoCents: amount,
		CreateTime:               now,
		ExpiresAt:                now.Add(settings.IntentTtl),
	}
	created := false
	claimable := read.claimable
	if err := server.HandleError(func() {
		created, claimable = model.CreateGammaClaim(ctx, intent, read.spend, now)
	}); err != nil {
		glog.Errorf("[gamma]claim for %s was not recorded: %v\n", address, err)
		return refuse(gammaClaimError(PurchaseErrorCodeStartFailed, purchaseStartFailedMessage))
	}
	if !created {
		if claimable <= 0 {
			return refuse(gammaClaimError(GammaErrorCodeNothingToClaim, "This hotkey's payments were claimed already."))
		}
		return refuse(gammaClaimError(GammaErrorCodeNothingToClaim, fmt.Sprintf("Another claim took part of it: %s gamma is claimable now. Ask for a new challenge.", gammaFormatUnits(claimable))))
	}
	gammaIntentsCreated.WithLabelValues(model.GammaPurposeClaim).Inc()
	glog.Infof("[gamma]claim %s: %s gamma of hotkey %s's direct payments for network %s (%s)\n", intent.IntentId, gammaFormatUnits(amount), address, networkId, item)
	return &GammaClaimResult{
		IntentId:       intent.IntentId.String(),
		StatusToken:    token,
		Hotkey:         address,
		Plan:           plan,
		AmountGamma:    gammaFormatUnits(amount),
		DataBytes:      dataBytes,
		RemainingGamma: gammaFormatUnits(max(claimable-amount, 0)),
		Status:         gammaStatusResult(clientSession, view, intent, now),
	}, nil
}
