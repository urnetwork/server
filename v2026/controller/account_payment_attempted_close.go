// The one-time pass over the canceled USDC payments an attempt of which reached
// Circle, and that the USDC close-out never closed
// (model/account_payment_attempted_close.go; owner decision 2026-11-13).
// `bringyourctl payouts close-attempted` runs it: a dry run reads and decides,
// and writes nothing; `--commit --expect=<digest>` reads and decides again,
// and writes only when the decisions still have the digest the dry run
// printed.
// Main is read with SELECTs and Circle with GETs only; nothing is sent.
//
// Each payment is decided from Circle:
//   - one with a payment record by that transfer: COMPLETE with a chain hash,
//     paid; FAILED, DENIED or CANCELLED without one, closed as forfeited
//     ("circle_failed"); any other state, in flight. The transfer must be the
//     payment's: to its wallet's address on its chain, of the configured USDC,
//     from the payout wallet, of the amount the legacy sender submitted (the
//     payout less the fixed fee) or its token amount.
//   - a keyed one without, as the legacy reconciliation decided one, by its
//     destination's complete Circle history (Circle has no lookup by
//     idempotency key): every live transfer there since the earliest of the
//     destination's payments, less a day, that no payment owns must be
//     attributed by its exact amount to exactly one of the destination's
//     payments. A payment with none created no transfer and is closed as
//     forfeited ("circle_absent"); one with a transfer is paid or in flight by
//     its state. A FAILED, DENIED or CANCELLED transfer without a chain hash
//     moved no funds.
//
// Anything else holds the payment, listed with its reason and not written: a
// failed lookup, a wallet that is missing or another network's, a transfer that
// is foreign or matches several payments, an unverified asset, a tx hash
// without a record, and records of earlier attempts in the payment's journal.
//
// The owner can forfeit held payments by id instead
// (`--forfeit=<payment_ids>`, owner decision 2026-11-13 on the six whose
// destinations Circle could not list): no Circle read, each named payment
// closed as forfeited ("owner_forfeit") with the same event, all or none. Each
// must be an unclosed attempted payment, one the pass reads; any other refuses
// the whole run. A forfeit closes payments the pass would read, so the pass's
// dry run digest changes once one commits.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The decisions of the pass.
const (
	// forfeited: no transfer moved funds
	UsdcAttemptedClosed = "closed"
	// a transfer completed: recorded paid
	UsdcAttemptedPaid = "paid"
	// a transfer is neither complete nor failed: listed, not written
	UsdcAttemptedInFlight = "in_flight"
	// not decidable: listed, not written
	UsdcAttemptedHeld = "held"
	// named for a forfeit and not an unclosed attempted payment: listed, and
	// nothing is written
	UsdcAttemptedRefused = "refused"
)

// The reason of a payment the owner forfeited by id, without a Circle read.
const UsdcAttemptedOwnerForfeit = "owner_forfeit"

// The fixed fee every legacy submit subtracted from the payout, in USDC.
const usdcAttemptedLegacyFeeUsdc = 0.01

// Transfers created this long before a payment cannot be its attempt; the
// margin absorbs clock differences between hosts and Circle, as the legacy
// reconciliation's did.
const usdcAttemptedWindowMargin = 24 * time.Hour

var ErrUsdcAttemptedDigestMismatch = errors.New("payouts close-attempted: --expect does not match the digest of these decisions; nothing was written")

var ErrUsdcAttemptedForfeitIds = errors.New("payouts close-attempted --forfeit: name each payment once, at least one; nothing was written")

var ErrUsdcAttemptedForfeitRefused = errors.New("payouts close-attempted --forfeit: a named payment is not an unclosed attempted payment; nothing was written")

var ErrUsdcAttemptedForfeitChanged = errors.New("payouts close-attempted --forfeit: a named payment changed since the census; nothing was written")

// Settings of the pass.
type UsdcAttemptedCloseSettings struct {
	// The pause between Circle lookups, well under Circle's GET limit, as the
	// legacy reconciliation paused.
	LookupInterval time.Duration
}

func DefaultUsdcAttemptedCloseSettings() *UsdcAttemptedCloseSettings {
	return &UsdcAttemptedCloseSettings{
		LookupInterval: 500 * time.Millisecond,
	}
}

// One transfer as Circle lists or reads it, with the fields the decisions
// read.
type circleAttemptedTransfer struct {
	Id                 string    `json:"id"`
	State              string    `json:"state"`
	Blockchain         string    `json:"blockchain"`
	TransactionType    string    `json:"transactionType"`
	Operation          string    `json:"operation"`
	CustodyType        string    `json:"custodyType"`
	WalletId           string    `json:"walletId"`
	DestinationAddress string    `json:"destinationAddress"`
	Amounts            []string  `json:"amounts"`
	TokenId            string    `json:"tokenId"`
	TxHash             string    `json:"txHash"`
	CreateDate         time.Time `json:"createDate"`
	// set only after the token id resolves to the configured chain USDC
	usdcAssetVerified bool
}

// FAILED, DENIED or CANCELLED without a chain hash: no funds moved.
func (self *circleAttemptedTransfer) failed() bool {
	switch strings.ToUpper(self.State) {
	case "FAILED", "DENIED", "CANCELLED":
		return self.TxHash == ""
	default:
		return false
	}
}

// From the developer payout wallet `payoutWalletId`, as a legacy payout was.
func (self *circleAttemptedTransfer) payout(payoutWalletId string) bool {
	return self.WalletId == payoutWalletId && strings.EqualFold(self.CustodyType, "DEVELOPER") &&
		strings.EqualFold(self.TransactionType, "OUTBOUND") && strings.EqualFold(self.Operation, "TRANSFER") &&
		len(self.Amounts) == 1
}

// The transfers the decisions read: by destination, and by id.
type usdcAttemptedCircle interface {
	// the developer wallet USDC payouts were sent from on the chain
	payoutWalletId(blockchain string) (string, error)
	// every transfer Circle lists to the destination, the relevant ones with
	// their asset verified; a partial listing is an error, never absence
	listDestination(ctx context.Context, blockchain string, address string, relevant func(*circleAttemptedTransfer) bool) ([]*circleAttemptedTransfer, error)
	// the transfer, with its asset verified
	getTransfer(ctx context.Context, blockchain string, id string) (*circleAttemptedTransfer, error)
}

// The circle.yml value of `key`.
func circleAttemptedConfig(key string) (string, error) {
	value, ok := circleConfig()[key].(string)
	if !ok || value == "" || strings.TrimSpace(value) != value {
		return "", fmt.Errorf("circle.yml has no %s", key)
	}
	return value, nil
}

// Reads Circle with the server's own read client and credentials.
type coreUsdcAttemptedCircle struct {
	core *CoreCircleApiClient
}

func (self *coreUsdcAttemptedCircle) payoutWalletId(blockchain string) (string, error) {
	switch blockchain {
	case "SOL":
		return circleAttemptedConfig("solana_wallet_id")
	case "MATIC":
		return circleAttemptedConfig("polygon_wallet_id")
	default:
		return "", fmt.Errorf("unsupported payout chain")
	}
}

func (self *coreUsdcAttemptedCircle) usdcAddress(blockchain string) (string, error) {
	switch blockchain {
	case "SOL":
		return circleAttemptedConfig("solana_usdc_address")
	case "MATIC":
		return circleAttemptedConfig("polygon_usdc_address")
	default:
		return "", fmt.Errorf("unsupported payout chain")
	}
}

// One Circle token's identity.
type circleAttemptedToken struct {
	Id           string `json:"id"`
	Blockchain   string `json:"blockchain"`
	TokenAddress string `json:"tokenAddress"`
	IsNative     *bool  `json:"isNative"`
}

// Only EVM address casing is insignificant. Solana mint addresses are
// case-sensitive; a matching symbol on any chain is never asset authority.
func circleAttemptedTokenMatches(token *circleAttemptedToken, blockchain string, address string) bool {
	if token == nil || token.Blockchain != blockchain || token.IsNative == nil || *token.IsNative ||
		address == "" || token.TokenAddress == "" {
		return false
	}
	switch blockchain {
	case "MATIC":
		return strings.EqualFold(token.TokenAddress, address)
	case "SOL":
		return token.TokenAddress == address
	default:
		return false
	}
}

// Marks each relevant transfer whose token is the configured chain USDC.
// A token lookup that fails fails the whole read.
func (self *coreUsdcAttemptedCircle) verifyAssets(ctx context.Context, blockchain string, transfers []*circleAttemptedTransfer, relevant func(*circleAttemptedTransfer) bool) error {
	address, err := self.usdcAddress(blockchain)
	if err != nil {
		return err
	}
	tokens := map[string]*circleAttemptedToken{}
	for _, transfer := range transfers {
		transfer.usdcAssetVerified = false
		if relevant != nil && !relevant(transfer) {
			continue
		}
		if !validCircleReadId(transfer.TokenId) {
			continue
		}
		token, ok := tokens[transfer.TokenId]
		if !ok {
			if len(tokens) == maximumCircleTokens {
				return fmt.Errorf("Circle token capacity exhausted")
			}
			token, err = paymentHttpGet(ctx, self.core.readSettings, self.core.readHooks,
				"https://api.circle.com/v1/w3s/tokens/"+url.PathEscape(transfer.TokenId), self.core.readHeaders(""),
				func(_ *http.Response, body []byte) (*circleAttemptedToken, error) {
					value, err := circleReadData(body, "token")
					if err != nil {
						return nil, err
					}
					var token circleAttemptedToken
					if err := json.Unmarshal(value, &token); err != nil {
						return nil, err
					}
					if token.Id != transfer.TokenId || token.Blockchain == "" || token.IsNative == nil {
						return nil, fmt.Errorf("Circle token observation is missing or another token's")
					}
					return &token, nil
				})
			if err != nil {
				return err
			}
			tokens[transfer.TokenId] = token
		}
		transfer.usdcAssetVerified = circleAttemptedTokenMatches(token, blockchain, address)
	}
	return nil
}

// Follows Circle's pagination to the end. A page limit, a repeated page or
// transfer, or a transfer to another destination fails the whole listing.
func (self *coreUsdcAttemptedCircle) listDestination(ctx context.Context, blockchain string, address string, relevant func(*circleAttemptedTransfer) bool) ([]*circleAttemptedTransfer, error) {
	ctx, cancel, err := self.core.beginReads(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	if address == "" || len(address) > 512 || strings.TrimSpace(address) != address || strings.ContainsAny(address, "\x00\r\n") {
		return nil, fmt.Errorf("invalid Circle listing destination")
	}
	query := url.Values{}
	query.Set("blockchain", blockchain)
	query.Set("destinationAddress", address)
	query.Set("includeAll", "true")
	query.Set("pageSize", fmt.Sprintf("%d", circleReadPageSize))
	first := "https://api.circle.com/v1/w3s/transactions?" + query.Encode()
	headers := self.core.readHeaders("")
	transfers := []*circleAttemptedTransfer{}
	seenTransfers := map[string]bool{}
	seenPages := map[string]bool{}
	pages := 0
	for uri := first; uri != ""; {
		if pages == maximumCircleObservationPages {
			return nil, fmt.Errorf("Circle transaction listing page capacity exhausted")
		}
		pages += 1
		if seenPages[uri] {
			return nil, fmt.Errorf("Circle transaction pagination repeated a page")
		}
		seenPages[uri] = true
		var next string
		page, err := paymentHttpGet(ctx, self.core.readSettings, self.core.readHooks, uri, headers, func(response *http.Response, body []byte) ([]*circleAttemptedTransfer, error) {
			var err error
			if next, err = circleReadNext(first, response.Header); err != nil {
				return nil, err
			}
			return circleReadPage[*circleAttemptedTransfer](body, "transactions")
		})
		if err != nil {
			return nil, err
		}
		for _, transfer := range page {
			if transfer == nil || !validCircleReadId(transfer.Id) || seenTransfers[transfer.Id] || transfer.State == "" ||
				transfer.Blockchain != blockchain || transfer.DestinationAddress != address || transfer.CreateDate.IsZero() {
				return nil, fmt.Errorf("Circle transaction listing is invalid, repeated or outside its destination")
			}
			seenTransfers[transfer.Id] = true
			transfers = append(transfers, transfer)
		}
		uri = next
	}
	if err := self.verifyAssets(ctx, blockchain, transfers, relevant); err != nil {
		return nil, err
	}
	return transfers, nil
}

func (self *coreUsdcAttemptedCircle) getTransfer(ctx context.Context, blockchain string, id string) (*circleAttemptedTransfer, error) {
	ctx, cancel, err := self.core.beginReads(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	if !validCircleReadId(id) {
		return nil, fmt.Errorf("invalid Circle transaction id")
	}
	transfer, err := paymentHttpGet(ctx, self.core.readSettings, self.core.readHooks,
		"https://api.circle.com/v1/w3s/transactions/"+url.PathEscape(id), self.core.readHeaders(""),
		func(_ *http.Response, body []byte) (*circleAttemptedTransfer, error) {
			value, err := circleReadData(body, "transaction")
			if err != nil {
				return nil, err
			}
			var transfer circleAttemptedTransfer
			if err := json.Unmarshal(value, &transfer); err != nil {
				return nil, err
			}
			if transfer.Id != id {
				return nil, fmt.Errorf("Circle transaction observation is another transaction's")
			}
			return &transfer, nil
		})
	if err != nil {
		return nil, err
	}
	if err := self.verifyAssets(ctx, blockchain, []*circleAttemptedTransfer{transfer}, nil); err != nil {
		return nil, err
	}
	return transfer, nil
}

// The chain of a wallet's blockchain, or "" when USDC was never paid on it.
func usdcAttemptedChain(blockchain string) string {
	switch strings.ToUpper(strings.TrimSpace(blockchain)) {
	case "SOL", "SOLANA":
		return "SOL"
	case "MATIC", "POLY", "POLYGON":
		return "MATIC"
	default:
		return ""
	}
}

// The exact amount a legacy submit sent for the payout: the payout less the
// fee, formatted to six decimals. Not positive for a payout the fee consumes.
func usdcAttemptedLegacyAmount(payout model.NanoCents) (amount float64, formatted string) {
	amount = model.NanoCentsToUsd(payout) - usdcAttemptedLegacyFeeUsdc
	return amount, fmt.Sprintf("%f", amount)
}

// Equal decimal strings, as numbers.
func usdcAttemptedDecimalEqual(a string, b string) bool {
	x, ok := new(big.Rat).SetString(strings.TrimSpace(a))
	if !ok {
		return false
	}
	y, ok := new(big.Rat).SetString(strings.TrimSpace(b))
	if !ok {
		return false
	}
	return x.Cmp(y) == 0
}

// One payment's decision.
type UsdcAttemptedRow struct {
	PaymentId       server.Id       `json:"payment_id"`
	NetworkId       server.Id       `json:"network_id"`
	Class           string          `json:"class"`
	Reason          string          `json:"reason"`
	PayoutNanoCents model.NanoCents `json:"payout_nano_cents"`
	Replaced        bool            `json:"replaced,omitempty"`
	Record          string          `json:"record,omitempty"`
	RecordState     string          `json:"record_state,omitempty"`
	TxHash          string          `json:"tx_hash,omitempty"`

	payment       *model.UsdcAttemptedPayment
	lookup        string
	listedCount   int
	failedRecords []string
	transfer      *circleAttemptedTransfer
}

func newUsdcAttemptedRow(payment *model.UsdcAttemptedPayment) *UsdcAttemptedRow {
	return &UsdcAttemptedRow{
		PaymentId:       payment.PaymentId,
		NetworkId:       payment.NetworkId,
		PayoutNanoCents: payment.Payout,
		Replaced:        payment.Replaced,
		payment:         payment,
	}
}

func (self *UsdcAttemptedRow) decide(class string, reason string) {
	self.Class = class
	self.Reason = reason
}

// Decides by its transfer's state: COMPLETE with a chain hash, paid; failed,
// forfeited; any other state, in flight.
func (self *UsdcAttemptedRow) decideByTransfer(transfer *circleAttemptedTransfer, failedReason string) {
	self.transfer = transfer
	self.Record = transfer.Id
	self.RecordState = transfer.State
	switch {
	case transfer.failed():
		self.decide(UsdcAttemptedClosed, failedReason)
	case strings.EqualFold(transfer.State, "COMPLETE") && transfer.TxHash != "":
		self.TxHash = transfer.TxHash
		self.decide(UsdcAttemptedPaid, "circle_complete")
	case strings.EqualFold(transfer.State, "COMPLETE"):
		self.decide(UsdcAttemptedHeld, "complete_without_hash")
	case strings.EqualFold(transfer.State, "FAILED") || strings.EqualFold(transfer.State, "DENIED") || strings.EqualFold(transfer.State, "CANCELLED"):
		self.decide(UsdcAttemptedHeld, "failed_with_hash")
	default:
		self.decide(UsdcAttemptedInFlight, "circle_"+strings.ToLower(transfer.State))
	}
}

// Decides a payment from its own payment record's transfer. Pure.
func decideUsdcRecordedPayment(row *UsdcAttemptedRow, chain string, transfer *circleAttemptedTransfer, payoutWalletId string) {
	payment := row.payment
	_, legacyAmount := usdcAttemptedLegacyAmount(payment.Payout)
	amountMatches := len(transfer.Amounts) == 1 && (usdcAttemptedDecimalEqual(transfer.Amounts[0], legacyAmount) ||
		payment.TokenAmount != nil && usdcAttemptedDecimalEqual(transfer.Amounts[0], fmt.Sprintf("%f", *payment.TokenAmount)))
	if transfer.Blockchain != chain || transfer.DestinationAddress != *payment.WalletAddress || !transfer.usdcAssetVerified ||
		!transfer.payout(payoutWalletId) || !amountMatches {
		row.Record = transfer.Id
		row.RecordState = transfer.State
		row.decide(UsdcAttemptedHeld, "record_mismatch")
		return
	}
	row.decideByTransfer(transfer, "circle_failed")
}

// Decides one destination's keyed payments from its listing, as the legacy
// reconciliation decided a destination group. Pure. `owners` are the payments
// owning each transfer.
func decideUsdcKeyedDestination(rows []*UsdcAttemptedRow, transfers []*circleAttemptedTransfer, owners map[string][]server.Id, payoutWalletId string) {
	hold := func(reason string) {
		for _, row := range rows {
			row.decide(UsdcAttemptedHeld, reason)
		}
	}
	relevant := usdcAttemptedRelevant(rows, owners)
	live := []*circleAttemptedTransfer{}
	failedRecords := []string{}
	for _, transfer := range transfers {
		if !relevant(transfer) {
			continue
		}
		if !transfer.usdcAssetVerified {
			hold("asset_unverified")
			return
		}
		if transfer.failed() {
			failedRecords = append(failedRecords, transfer.Id)
			continue
		}
		live = append(live, transfer)
	}
	attributed := map[server.Id]*circleAttemptedTransfer{}
	for _, transfer := range live {
		if !transfer.payout(payoutWalletId) {
			hold("unattributable_transfer")
			return
		}
		var matched *UsdcAttemptedRow
		matches := 0
		for _, row := range rows {
			amount, formatted := usdcAttemptedLegacyAmount(row.payment.Payout)
			if 0 < amount && usdcAttemptedDecimalEqual(transfer.Amounts[0], formatted) &&
				!transfer.CreateDate.Before(row.payment.CreateTime.Add(-usdcAttemptedWindowMargin)) {
				matched = row
				matches += 1
			}
		}
		if matches != 1 {
			hold("unattributable_transfer")
			return
		}
		if _, ok := attributed[matched.PaymentId]; ok {
			hold("several_transfers")
			return
		}
		attributed[matched.PaymentId] = transfer
	}
	for _, row := range rows {
		row.listedCount = len(transfers)
		row.failedRecords = slices.Clone(failedRecords)
		if transfer, ok := attributed[row.PaymentId]; ok {
			row.decideByTransfer(transfer, "circle_failed")
		} else {
			// every live transfer to the destination is another payment's, so
			// this attempt created none
			row.decide(UsdcAttemptedClosed, "circle_absent")
		}
	}
}

// A transfer the destination's decision reads: since the earliest of its
// payments less the margin, outbound, from developer custody, and owned by no
// payment.
func usdcAttemptedRelevant(rows []*UsdcAttemptedRow, owners map[string][]server.Id) func(*circleAttemptedTransfer) bool {
	if len(rows) == 0 {
		return func(*circleAttemptedTransfer) bool { return false }
	}
	windowStart := rows[0].payment.CreateTime
	for _, row := range rows {
		windowStart = server.MinTime(windowStart, row.payment.CreateTime)
	}
	windowStart = windowStart.Add(-usdcAttemptedWindowMargin)
	return func(transfer *circleAttemptedTransfer) bool {
		return !transfer.CreateDate.Before(windowStart) &&
			!strings.EqualFold(transfer.TransactionType, "INBOUND") &&
			!strings.EqualFold(transfer.CustodyType, "ENDUSER") &&
			len(owners[transfer.Id]) == 0
	}
}

// The decisions of one run.
type UsdcAttemptedReport struct {
	RunId server.Id `json:"run_id"`
	Time  time.Time `json:"time"`
	Mode  string    `json:"mode"`
	// the payments an owner's forfeit names, in payment id order; none for
	// the Circle pass
	Forfeit []server.Id `json:"forfeit,omitempty"`
	// none when a forfeit refused
	Digest string              `json:"digest"`
	Rows   []*UsdcAttemptedRow `json:"rows"`
	// commit: rows written, and rows that changed since the census and were not
	Closed    int `json:"closed"`
	Completed int `json:"completed"`
	Skipped   int `json:"skipped"`
}

// A digest over every decision, in payment id order: its payment, network,
// class, reason, payout, replacement, transfer, state and chain hash.
func usdcAttemptedDigest(rows []*UsdcAttemptedRow) string {
	digest := sha256.New()
	for _, row := range rows {
		line, err := json.Marshal(row)
		if err != nil {
			panic(err)
		}
		digest.Write(line)
		digest.Write([]byte{'\n'})
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// Decides every payment, reading Circle once per record and once per
// destination, with the settings' pause between lookups. Read only.
func usdcAttemptedCensus(ctx context.Context, circle usdcAttemptedCircle, settings *UsdcAttemptedCloseSettings) ([]*UsdcAttemptedRow, error) {
	payments := model.GetUsdcAttemptedPayments(ctx)
	owners := model.GetUsdcPaymentRecordOwners(ctx)
	ownedRecords := map[server.Id][]string{}
	for record, recordOwners := range owners {
		for _, paymentId := range recordOwners {
			ownedRecords[paymentId] = append(ownedRecords[paymentId], record)
		}
	}
	rows := []*UsdcAttemptedRow{}
	type destination struct {
		chain   string
		address string
	}
	destinations := map[destination][]*UsdcAttemptedRow{}
	destinationOrder := []destination{}
	recorded := []*UsdcAttemptedRow{}
	for _, payment := range payments {
		row := newUsdcAttemptedRow(payment)
		rows = append(rows, row)
		chain := ""
		if payment.Blockchain != nil {
			chain = usdcAttemptedChain(*payment.Blockchain)
		}
		earlierRecords := false
		for _, record := range ownedRecords[payment.PaymentId] {
			if payment.PaymentRecord == nil || record != *payment.PaymentRecord {
				earlierRecords = true
			}
		}
		switch {
		case payment.WalletAddress == nil || payment.Blockchain == nil:
			row.decide(UsdcAttemptedHeld, "wallet_missing")
		case chain == "":
			row.decide(UsdcAttemptedHeld, "chain_unsupported")
		case payment.TxHash != nil && payment.PaymentRecord == nil:
			row.decide(UsdcAttemptedHeld, "tx_hash_without_record")
		case earlierRecords:
			row.decide(UsdcAttemptedHeld, "earlier_attempt_records")
		case payment.PaymentRecord != nil && 1 < len(owners[*payment.PaymentRecord]):
			row.decide(UsdcAttemptedHeld, "record_shared")
		case payment.PaymentRecord != nil:
			recorded = append(recorded, row)
		default:
			key := destination{chain: chain, address: *payment.WalletAddress}
			if _, ok := destinations[key]; !ok {
				destinationOrder = append(destinationOrder, key)
			}
			destinations[key] = append(destinations[key], row)
		}
	}

	lookups := 0
	pause := func() error {
		if 0 < lookups && 0 < settings.LookupInterval {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(settings.LookupInterval):
			}
		}
		lookups += 1
		return nil
	}
	for _, row := range recorded {
		if err := pause(); err != nil {
			return nil, err
		}
		row.lookup = "circle_transaction"
		chain := usdcAttemptedChain(*row.payment.Blockchain)
		payoutWalletId, err := circle.payoutWalletId(chain)
		if err != nil {
			return nil, err
		}
		transfer, err := circle.getTransfer(ctx, chain, *row.payment.PaymentRecord)
		if err != nil {
			glog.Warningf("[payout]attempted close: the record of payment %s did not read: %s\n", row.PaymentId, err)
			row.decide(UsdcAttemptedHeld, "lookup_failed")
			continue
		}
		decideUsdcRecordedPayment(row, chain, transfer, payoutWalletId)
	}
	for _, key := range destinationOrder {
		if err := pause(); err != nil {
			return nil, err
		}
		group := destinations[key]
		for _, row := range group {
			row.lookup = "circle_destination_transactions"
		}
		payoutWalletId, err := circle.payoutWalletId(key.chain)
		if err != nil {
			return nil, err
		}
		transfers, err := circle.listDestination(ctx, key.chain, key.address, usdcAttemptedRelevant(group, owners))
		if err != nil {
			glog.Warningf("[payout]attempted close: a %s destination of %d payments did not list: %s\n", key.chain, len(group), err)
			for _, row := range group {
				row.decide(UsdcAttemptedHeld, "lookup_failed")
			}
			continue
		}
		decideUsdcKeyedDestination(group, transfers, owners, payoutWalletId)
	}
	return rows, nil
}

// The journal details of a written decision.
type usdcAttemptedCloseEvent struct {
	RunId           server.Id       `json:"run_id"`
	Digest          string          `json:"digest"`
	Class           string          `json:"class"`
	Reason          string          `json:"reason"`
	Lookup          string          `json:"lookup"`
	ObservedAt      time.Time       `json:"observed_at"`
	ListedCount     int             `json:"listed_count,omitempty"`
	FailedRecords   []string        `json:"failed_records,omitempty"`
	Record          string          `json:"record,omitempty"`
	RecordState     string          `json:"record_state,omitempty"`
	TxHash          string          `json:"tx_hash,omitempty"`
	PayoutNanoCents model.NanoCents `json:"payout_nano_cents"`
}

// Decides every payment. A dry run returns the decisions. A commit writes the
// closed and paid ones, each in its own transaction that reads the payment
// again, and only when the decisions' digest is `expect`.
func usdcAttemptedClose(
	ctx context.Context,
	circle usdcAttemptedCircle,
	settings *UsdcAttemptedCloseSettings,
	commit bool,
	expect string,
	now func() time.Time,
) (*UsdcAttemptedReport, error) {
	report := &UsdcAttemptedReport{
		RunId: server.NewId(),
		Time:  now(),
		Mode:  "dry_run",
	}
	if commit {
		report.Mode = "commit"
	}
	rows, err := usdcAttemptedCensus(ctx, circle, settings)
	if err != nil {
		return nil, err
	}
	report.Rows = rows
	report.Digest = usdcAttemptedDigest(rows)
	if !commit {
		return report, nil
	}
	if expect != report.Digest {
		return report, ErrUsdcAttemptedDigestMismatch
	}
	for _, row := range rows {
		if row.Class != UsdcAttemptedClosed && row.Class != UsdcAttemptedPaid {
			continue
		}
		writeTime := now()
		details, err := json.Marshal(&usdcAttemptedCloseEvent{
			RunId:           report.RunId,
			Digest:          report.Digest,
			Class:           row.Class,
			Reason:          row.Reason,
			Lookup:          row.lookup,
			ObservedAt:      report.Time,
			ListedCount:     row.listedCount,
			FailedRecords:   row.failedRecords,
			Record:          row.Record,
			RecordState:     row.RecordState,
			TxHash:          row.TxHash,
			PayoutNanoCents: row.PayoutNanoCents,
		})
		if err != nil {
			return report, err
		}
		if row.Class == UsdcAttemptedClosed {
			if model.CloseUsdcAttemptedPayment(ctx, row.payment, writeTime, string(details)) {
				report.Closed += 1
			} else {
				report.Skipped += 1
			}
			continue
		}
		receipt, err := json.Marshal(row.transfer)
		if err != nil {
			return report, err
		}
		if model.CompleteUsdcAttemptedPayment(ctx, row.payment, row.Record, row.TxHash, string(receipt), writeTime, string(details)) {
			report.Completed += 1
		} else {
			report.Skipped += 1
		}
	}
	return report, nil
}

// Runs the pass against Main's Circle with the default settings.
func UsdcAttemptedClose(ctx context.Context, commit bool, expect string) (*UsdcAttemptedReport, error) {
	return usdcAttemptedClose(ctx, &coreUsdcAttemptedCircle{core: &CoreCircleApiClient{}}, DefaultUsdcAttemptedCloseSettings(), commit, expect, server.NowUtc)
}

// The owner's forfeit of the named payments, read from Main only. Each must be
// an unclosed attempted payment; otherwise the run refuses, listing each named
// payment with why: unknown, completed, not_canceled, not_attempted or closed.
// A dry run returns the decisions and their digest. A commit with the dry run's
// digest closes every named payment in one transaction, or none when any
// changed since the commit's census.
func usdcAttemptedForfeit(ctx context.Context, paymentIds []server.Id, commit bool, expect string, now func() time.Time) (*UsdcAttemptedReport, error) {
	forfeit := slices.Clone(paymentIds)
	slices.SortFunc(forfeit, func(a server.Id, b server.Id) int {
		return a.Cmp(b)
	})
	if len(forfeit) == 0 || len(slices.Compact(slices.Clone(forfeit))) != len(forfeit) {
		return nil, ErrUsdcAttemptedForfeitIds
	}
	report := &UsdcAttemptedReport{
		RunId:   server.NewId(),
		Time:    now(),
		Mode:    "dry_run",
		Forfeit: forfeit,
		Rows:    []*UsdcAttemptedRow{},
	}
	if commit {
		report.Mode = "commit"
	}
	payments := model.GetUsdcForfeitPayments(ctx, forfeit)
	refused := false
	for _, paymentId := range forfeit {
		payment, ok := payments[paymentId]
		if !ok {
			report.Rows = append(report.Rows, &UsdcAttemptedRow{PaymentId: paymentId, Class: UsdcAttemptedRefused, Reason: "unknown"})
			refused = true
			continue
		}
		row := newUsdcAttemptedRow(&payment.UsdcAttemptedPayment)
		row.lookup = "owner_decision"
		switch {
		case payment.Completed:
			row.decide(UsdcAttemptedRefused, "completed")
		case !payment.Canceled:
			row.decide(UsdcAttemptedRefused, "not_canceled")
		case !payment.Attempted:
			row.decide(UsdcAttemptedRefused, "not_attempted")
		case payment.Closed:
			row.decide(UsdcAttemptedRefused, "closed")
		default:
			row.decide(UsdcAttemptedClosed, UsdcAttemptedOwnerForfeit)
		}
		refused = refused || row.Class == UsdcAttemptedRefused
		report.Rows = append(report.Rows, row)
	}
	if refused {
		return report, ErrUsdcAttemptedForfeitRefused
	}
	report.Digest = usdcAttemptedDigest(report.Rows)
	if !commit {
		return report, nil
	}
	if expect != report.Digest {
		return report, ErrUsdcAttemptedDigestMismatch
	}
	closing := []*model.UsdcAttemptedPayment{}
	details := map[server.Id]string{}
	for _, row := range report.Rows {
		detail, err := json.Marshal(&usdcAttemptedCloseEvent{
			RunId:           report.RunId,
			Digest:          report.Digest,
			Class:           row.Class,
			Reason:          row.Reason,
			Lookup:          row.lookup,
			ObservedAt:      report.Time,
			PayoutNanoCents: row.PayoutNanoCents,
		})
		if err != nil {
			return report, err
		}
		closing = append(closing, row.payment)
		details[row.PaymentId] = string(detail)
	}
	if !model.CloseUsdcAttemptedPayments(ctx, closing, now(), details) {
		report.Skipped = len(closing)
		return report, ErrUsdcAttemptedForfeitChanged
	}
	report.Closed = len(closing)
	return report, nil
}

// Runs the owner's forfeit of the named payments against Main.
func UsdcAttemptedForfeit(ctx context.Context, paymentIds []server.Id, commit bool, expect string) (*UsdcAttemptedReport, error) {
	return usdcAttemptedForfeit(ctx, paymentIds, commit, expect, server.NowUtc)
}

// The report for a person: counts and payout by class and reason, every row
// that is not closed (a forfeit's every row), the digest, and on a dry run the
// command that writes exactly these decisions. A refused forfeit has no digest
// and no command.
func (self *UsdcAttemptedReport) Text() string {
	b := &strings.Builder{}
	forfeitIds := []string{}
	for _, paymentId := range self.Forfeit {
		forfeitIds = append(forfeitIds, paymentId.String())
	}
	forfeitArg := ""
	if 0 < len(forfeitIds) {
		forfeitArg = " --forfeit=" + strings.Join(forfeitIds, ",")
	}
	fmt.Fprintf(b, "payouts close-attempted%s %s run %s at %s\n", forfeitArg, self.Mode, self.RunId, self.Time.Format(time.RFC3339))
	type total struct {
		count  int
		payout model.NanoCents
	}
	totals := map[[2]string]*total{}
	keys := [][2]string{}
	for _, row := range self.Rows {
		key := [2]string{row.Class, row.Reason}
		t, ok := totals[key]
		if !ok {
			t = &total{}
			totals[key] = t
			keys = append(keys, key)
		}
		t.count += 1
		t.payout += row.PayoutNanoCents
	}
	slices.SortFunc(keys, func(a [2]string, b [2]string) int {
		return strings.Compare(a[0]+"/"+a[1], b[0]+"/"+b[1])
	})
	fmt.Fprintf(b, "%d payments\n", len(self.Rows))
	for _, key := range keys {
		fmt.Fprintf(b, "  %s %s: %d payments, %.6f USD\n", key[0], key[1], totals[key].count, model.NanoCentsToUsd(totals[key].payout))
	}
	for _, row := range self.Rows {
		if row.Class == UsdcAttemptedClosed && len(self.Forfeit) == 0 {
			continue
		}
		fmt.Fprintf(b, "  %s %s payment %s network %s %.6f USD", row.Class, row.Reason, row.PaymentId, row.NetworkId, model.NanoCentsToUsd(row.PayoutNanoCents))
		if row.Replaced {
			b.WriteString(" replaced")
		}
		if row.Record != "" {
			fmt.Fprintf(b, " record %s %s", row.Record, row.RecordState)
		}
		if row.TxHash != "" {
			fmt.Fprintf(b, " tx %s", row.TxHash)
		}
		b.WriteString("\n")
	}
	if self.Mode == "commit" {
		fmt.Fprintf(b, "closed %d, completed %d, skipped as changed %d\n", self.Closed, self.Completed, self.Skipped)
	}
	if self.Digest == "" {
		return b.String()
	}
	fmt.Fprintf(b, "digest %s\n", self.Digest)
	if self.Mode == "dry_run" {
		fmt.Fprintf(b, "commit with: bringyourctl payouts close-attempted%s --commit --expect=%s\n", forfeitArg, self.Digest)
	}
	return b.String()
}
