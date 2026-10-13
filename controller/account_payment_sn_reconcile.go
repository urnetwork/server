// The one-time sn-reconcile command pays only the legacy USDC rows that neither
// the planner nor the sender pays on its own (SN-RECONCILE.md). Circle is only
// read here; the existing sender moves the money for each revived row.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Row classes (§5).
const (
	snReconcilePaid          = "paid"
	snReconcilePaidReplanned = "paid_replanned"
	snReconcileReplanned     = "replanned"
	snReconcilePayable       = "payable"
	snReconcileUnresolved    = "unresolved"
)

// Reasons of an unresolved row, and of a payable row skipped by `--send`.
const (
	snReconcileReviewRequired    = "review_required"
	snReconcileContradictory     = "contradictory"
	snReconcileInFlight          = "in_flight"
	snReconcileRecordUnavailable = "record_unavailable"
	snReconcileListingIncomplete = "listing_incomplete"
	snReconcileOrphanTransfer    = "orphan_transfer"
	snReconcileCensusMismatch    = "census_mismatch"
	snReconcileSweepsMissing     = "sweeps_missing"
	snReconcileDuplicateRisk     = "duplicate_risk"
	snReconcileNoPayoutWallet    = "no_payout_wallet"
	snReconcileBelowFee          = "below_fee"
	snReconcileChanged           = "changed"
	snReconcileUnattributed      = "unattributed_orphan"
	snReconcileInflated          = "inflated"
	snReconcileUpstreamScope     = "upstream_scope"
)

// Outcomes of the row's own attempt that do not stop its classification.
const (
	snReconcileAttemptComplete = "complete"
	snReconcileAttemptNoFunds  = "no_funds"
)

// An orphan may pay a row from a day before the row's earliest contract closed.
const snReconcileOrphanWindow = 24 * time.Hour

// Records stored this close to either end of a listing need not appear yet,
// and the send's re-listing starts this long before the census listing did.
const snReconcileCompletenessMargin = time.Hour

var ErrSnReconcileDigestMismatch = errors.New("sn-reconcile: --expect does not match the digest of this census; nothing was revived")
var ErrSnReconcileUnfunded = errors.New("sn-reconcile: a chain's payable send total exceeds its payout wallet's USDC balance; nothing was revived")

type SnReconcileReport struct {
	RunId        server.Id                `json:"run_id"`
	Time         time.Time                `json:"time"`
	Mode         string                   `json:"mode"`
	PolicySha256 string                   `json:"policy_sha256"`
	Digest       string                   `json:"digest"`
	Listing      *SnReconcileListing      `json:"listing"`
	Orphans      []*SnReconcileOrphan     `json:"orphans"`
	Classes      []*SnReconcileClassTotal `json:"classes"`
	Funding      []*SnReconcileFunding    `json:"funding"`
	Rows         []*SnReconcileRowReport  `json:"rows"`
	Duplicates   []*SnReconcileDuplicates `json:"duplicates"`
	Relisting    *SnReconcileListing      `json:"relisting"`
	Revived      []server.Id              `json:"revived"`
	Skipped      []*SnReconcileSkip       `json:"skipped"`
}

// With no stuck row nothing is listed and `From` and `Start` are nil.
type SnReconcileListing struct {
	Wallets          []string   `json:"wallets"`
	From             *time.Time `json:"from"`
	Start            *time.Time `json:"start"`
	TransactionCount int        `json:"transaction_count"`
	Complete         bool       `json:"complete"`
	MissingIds       []string   `json:"missing_ids"`
	MismatchedIds    []string   `json:"mismatched_ids"`
}

// A listed transfer that no payment record or receipt names, in a state other
// than FAILED, DENIED or CANCELLED and not INBOUND (RC-7).
type SnReconcileOrphan struct {
	Id              string      `json:"id"`
	WalletId        string      `json:"wallet_id"`
	TransactionType string      `json:"transaction_type"`
	State           string      `json:"state"`
	Amounts         []string    `json:"amounts"`
	CreateDate      string      `json:"create_date"`
	Destination     string      `json:"destination"`
	NetworkIds      []server.Id `json:"network_ids"`
	// A4: no network registered the destination, and it is not our own wallet.
	Unattributed bool `json:"unattributed"`

	created time.Time
}

type SnReconcileClassTotal struct {
	Class           string          `json:"class"`
	Reason          string          `json:"reason,omitempty"`
	Count           int             `json:"count"`
	PayoutNanoCents model.NanoCents `json:"payout_nano_cents"`
}

// The payable send total of one chain, with the rows earlier runs revived that
// are not yet completed, against its payout wallet (RC-13, A7).
type SnReconcileFunding struct {
	Chain              string          `json:"chain"`
	WalletId           string          `json:"wallet_id"`
	PayableCount       int             `json:"payable_count"`
	SendNanoCents      model.NanoCents `json:"send_nano_cents"`
	RevivedCount       int             `json:"revived_count"`
	RevivedNanoCents   model.NanoCents `json:"revived_nano_cents"`
	BalanceNanoCents   model.NanoCents `json:"balance_nano_cents"`
	ShortfallNanoCents model.NanoCents `json:"shortfall_nano_cents"`

	tokenAddress string
}

type SnReconcileRowReport struct {
	PaymentId       server.Id                  `json:"payment_id"`
	NetworkId       server.Id                  `json:"network_id"`
	Kind            string                     `json:"kind"`
	Class           string                     `json:"class"`
	Reason          string                     `json:"reason,omitempty"`
	PayoutNanoCents model.NanoCents            `json:"payout_nano_cents"`
	OwedNanoCents   model.NanoCents            `json:"owed_nano_cents"`
	SendUsd         float64                    `json:"send_usd,omitempty"`
	Chain           string                     `json:"chain,omitempty"`
	Evidence        *model.SnReconcileEvidence `json:"circle_evidence,omitempty"`
	OrphanIds       []string                   `json:"orphan_ids,omitempty"`
	// An `inflated` row's payout under the legacy correction.
	CorrectedPayoutNanoCents *model.NanoCents `json:"corrected_payout_nano_cents,omitempty"`
	// A8: a `replanned` row's subsidy and reliability, each with its guidance.
	Subsidy     *SnReconcileReplannedAmount `json:"subsidy,omitempty"`
	Reliability *SnReconcileReplannedAmount `json:"reliability,omitempty"`

	row           *model.SnReconcileRow
	censusExact   bool
	attempt       string
	sendNanoCents model.NanoCents
	correction    *model.LegacyProviderPaymentCorrection
}

// A8: the subsidy or the reliability of a `replanned` row, with what the owner
// must know before deciding on it.
type SnReconcileReplannedAmount struct {
	NanoCents model.NanoCents `json:"nano_cents"`
	Guidance  string          `json:"guidance"`
}

// A8: the re-plan paid a `replanned` row's sweeps, and its epoch counted their
// bytes in its subsidy pool. Reliability is earned per window and the re-plan
// paid none of it.
const (
	snReconcileSubsidyGuidance     = "the re-plan's epoch already weighted these bytes in its subsidy pool; paying this by hand would reward them twice"
	snReconcileReliabilityGuidance = "per-window, not paid by the re-plan; owner decides"
)

// One network's rows that hold early earnings an earlier payment may already
// have paid, with the network's RC-1 rows (§6, A2). Read only.
type SnReconcileDuplicates struct {
	NetworkId             server.Id                  `json:"network_id"`
	Canceled              []*SnReconcileRowReport    `json:"canceled"`
	CanceledByClass       []*SnReconcileClassTotal   `json:"canceled_by_class"`
	Holders               []*model.SnReconcileHolder `json:"holders"`
	EarlyRevenueNanoCents model.NanoCents            `json:"early_revenue_nano_cents"`
	EarlyByteCount        model.ByteCount            `json:"early_byte_count"`
}

// A payable row `--send` left untouched: changed since the census, or blocked
// by the re-listing (A1, A9).
type SnReconcileSkip struct {
	PaymentId server.Id `json:"payment_id"`
	Reason    string    `json:"reason"`
	OrphanIds []string  `json:"orphan_ids,omitempty"`
}

// SnReconcile runs the census and, with `send`, revives the payable rows for
// the existing sender. The dry run writes nothing and calls only Circle GET
// endpoints (RC-18). `--send` needs the digest of the dry run it executes
// (A7). A refusal still returns its report.
func SnReconcile(clientSession *session.ClientSession, send bool, expect string) (*SnReconcileReport, error) {
	ctx := clientSession.Ctx
	report := &SnReconcileReport{
		RunId:      server.NewId(),
		Time:       server.NowUtc(),
		Mode:       "dry_run",
		Listing:    &SnReconcileListing{Wallets: []string{}, Complete: true, MissingIds: []string{}, MismatchedIds: []string{}},
		Orphans:    []*SnReconcileOrphan{},
		Rows:       []*SnReconcileRowReport{},
		Duplicates: []*SnReconcileDuplicates{},
		Revived:    []server.Id{},
		Skipped:    []*SnReconcileSkip{},
	}
	if send {
		report.Mode = "send"
	}
	policy, err := server.LoadProviderPayoutEarningPolicy(ctx)
	if err != nil {
		return nil, err
	}
	if policy != nil {
		report.PolicySha256 = policy.ConfigSha256
	}
	report.Funding, err = snReconcileConfiguredFunding()
	if err != nil {
		return nil, err
	}
	rows, err := model.GetSnReconcileStuckRows(ctx)
	if err != nil {
		return nil, err
	}
	walletIds := []string{}
	for _, funding := range report.Funding {
		walletIds = append(walletIds, funding.WalletId)
	}
	corrections := snReconcileCorrections(ctx, rows)
	circleClient := NewCircleClient()
	for _, row := range rows {
		item := &SnReconcileRowReport{PaymentId: row.PaymentId, NetworkId: row.NetworkId, Kind: row.Kind, PayoutNanoCents: row.Payout,
			row: row, censusExact: model.SnReconcileCensusExact(ctx, row.PaymentId, policy), correction: corrections[row.PaymentId]}
		if row.PaymentRecord != nil {
			if walletId := snReconcileRecordAttempt(ctx, circleClient, item); walletId != "" {
				walletIds = append(walletIds, walletId)
			}
		}
		report.Rows = append(report.Rows, item)
	}
	ledger := NewCirclePayoutLedger()
	transactions := []CircleTransaction{}
	own := map[string]bool{}
	if 0 < len(rows) {
		// A4: every developer wallet is listed, and a transfer to one is ours.
		developerWallets, err := ledger.ListDeveloperWallets(ctx)
		if err != nil {
			return nil, fmt.Errorf("sn-reconcile: listing the Circle developer wallets: %w", err)
		}
		for _, wallet := range developerWallets {
			walletIds = append(walletIds, wallet.ID)
			own[model.SnReconcileAddress(wallet.Address)] = true
		}
		// A3: the listing reaches every row's orphan window.
		from := snReconcileWindowStart(rows[0])
		for _, row := range rows {
			from = server.MinTime(from, snReconcileWindowStart(row))
		}
		walletIds = append(walletIds, model.GetSnReconcileReceiptWalletIds(ctx, from)...)
		report.Listing, transactions, err = snReconcileList(ctx, ledger, walletIds, from)
		if err != nil {
			return nil, err
		}
		report.Orphans, err = snReconcileOrphans(ctx, transactions, own)
		if err != nil {
			return nil, err
		}
	}
	for _, item := range report.Rows {
		snReconcileClassify(item, report, transactions)
	}
	report.Digest = snReconcileDigest(report)
	snReconcileDuplicates(ctx, report)
	report.Classes = snReconcileTotals(report.Rows)
	revived := model.GetSnReconcileRevivedPending(ctx)
	for _, funding := range report.Funding {
		funding.BalanceNanoCents, err = ledger.GetWalletTokenBalance(ctx, funding.WalletId, funding.tokenAddress)
		if err != nil {
			return nil, fmt.Errorf("sn-reconcile: reading the %s payout wallet balance: %w", funding.Chain, err)
		}
		for _, item := range report.Rows {
			if item.Class == snReconcilePayable && item.Chain == funding.Chain {
				funding.PayableCount += 1
				funding.SendNanoCents += item.sendNanoCents
			}
		}
		for _, row := range revived {
			if chain, ok := snReconcilePayoutChain(row.PayoutWalletBlockchain, row.PayoutWalletActive); ok && chain == funding.Chain {
				_, sendNanoCents := snReconcileSendAmount(row.Payout)
				funding.RevivedCount += 1
				funding.RevivedNanoCents += sendNanoCents
			}
		}
		funding.ShortfallNanoCents = max(0, funding.SendNanoCents+funding.RevivedNanoCents-funding.BalanceNanoCents)
	}
	if !send {
		return report, nil
	}
	// A7, RC-13: refuse before any write.
	if expect != report.Digest {
		return report, ErrSnReconcileDigestMismatch
	}
	for _, funding := range report.Funding {
		if 0 < funding.ShortfallNanoCents {
			return report, ErrSnReconcileUnfunded
		}
	}
	payable := []*SnReconcileRowReport{}
	for _, item := range report.Rows {
		if item.Class == snReconcilePayable {
			payable = append(payable, item)
		}
	}
	if len(payable) == 0 {
		return report, nil
	}
	// A1: list again just before reviving; the re-listing may only skip rows.
	report.Relisting, transactions, err = snReconcileList(ctx, ledger, report.Listing.Wallets, report.Listing.Start.Add(-snReconcileCompletenessMargin))
	if err != nil {
		return nil, err
	}
	orphans, err := snReconcileOrphans(ctx, transactions, own)
	if err != nil {
		return nil, err
	}
	// A9: an unattributed transfer the census did not have may have paid any row.
	unattributed := []string{}
	for _, orphan := range orphans {
		if slices.ContainsFunc(report.Orphans, func(listed *SnReconcileOrphan) bool { return listed.Id == orphan.Id }) {
			continue
		}
		report.Orphans = append(report.Orphans, orphan)
		if orphan.Unattributed {
			unattributed = append(unattributed, orphan.Id)
		}
	}
	for _, item := range payable {
		if !report.Relisting.Complete {
			report.Skipped = append(report.Skipped, &SnReconcileSkip{PaymentId: item.PaymentId, Reason: snReconcileListingIncomplete})
			continue
		}
		if 0 < len(unattributed) {
			report.Skipped = append(report.Skipped, &SnReconcileSkip{PaymentId: item.PaymentId, Reason: snReconcileUnattributed, OrphanIds: unattributed})
			continue
		}
		if orphanIds := snReconcileBlockingOrphans(item.row, orphans); 0 < len(orphanIds) {
			report.Skipped = append(report.Skipped, &SnReconcileSkip{PaymentId: item.PaymentId, Reason: snReconcileOrphanTransfer, OrphanIds: orphanIds})
			continue
		}
		paymentId := item.PaymentId
		if model.ReviveSnReconcileRow(ctx, item.row, policy, report.RunId, item.Evidence, func(tx server.PgTx) {
			ScheduleAdvancePayment(&AdvancePaymentArgs{PaymentId: paymentId}, clientSession, tx)
		}) {
			report.Revived = append(report.Revived, paymentId)
		} else {
			report.Skipped = append(report.Skipped, &SnReconcileSkip{PaymentId: paymentId, Reason: snReconcileChanged})
		}
	}
	return report, nil
}

// The circle.yml keys the payout getters read. A getter panics when its key is
// missing or not a string.
var snReconcileCircleKeys = []string{"polygon_wallet_id", "polygon_usdc_address", "solana_wallet_id", "solana_usdc_address"}

// The configured developer wallet and USDC token of each payout chain. A12:
// every key is checked before any getter reads it, so a missing key refuses
// the command instead of panicking.
func snReconcileConfiguredFunding() ([]*SnReconcileFunding, error) {
	var config map[string]any
	resource, err := server.Vault.SimpleResource("circle.yml")
	if err == nil {
		config, err = resource.ParseE()
	}
	if err != nil {
		return nil, fmt.Errorf("sn-reconcile: reading circle.yml: %w", err)
	}
	circle, _ := config["circle"].(map[string]any)
	for _, key := range snReconcileCircleKeys {
		if _, ok := circle[key].(string); !ok {
			return nil, fmt.Errorf("sn-reconcile: circle.yml has no string value for circle.%s", key)
		}
	}
	fundings := []*SnReconcileFunding{}
	for _, chain := range []string{"MATIC", "SOL"} {
		// Both chain names are supported, so neither lookup can refuse them.
		walletId, _ := getWalletIdByNetwork(chain)
		tokenAddress, _ := getUsdcAddressByNetwork(chain)
		if walletId == "" || tokenAddress == "" {
			return nil, fmt.Errorf("sn-reconcile: circle.yml has no %s payout wallet id or USDC token address", chain)
		}
		fundings = append(fundings, &SnReconcileFunding{Chain: chain, WalletId: walletId, tokenAddress: tokenAddress})
	}
	return fundings, nil
}

// The legacy correction (account_payment_legacy_correction.go) of each stuck
// row with a subsidy or reliability subsidy whose plan has its subsidy epoch.
// The exact census requires that epoch for such a row, and a plan has at most
// one.
func snReconcileCorrections(ctx context.Context, rows []*model.SnReconcileRow) map[server.Id]*model.LegacyProviderPaymentCorrection {
	paymentIds := []server.Id{}
	for _, row := range rows {
		if 0 < row.SubsidyPayout || 0 < row.ReliabilitySubsidy {
			paymentIds = append(paymentIds, row.PaymentId)
		}
	}
	corrections := map[server.Id]*model.LegacyProviderPaymentCorrection{}
	if len(paymentIds) == 0 {
		// the policy reads subsidy.yml, which only such a row needs
		return corrections
	}
	windows, err := model.GetLegacyProviderPaymentSubsidyWindows(ctx, paymentIds)
	server.Raise(err)
	policy := model.DefaultLegacyProviderPaymentCorrectionPolicy()
	for _, row := range rows {
		if window := windows[row.PaymentId]; window != nil {
			corrections[row.PaymentId] = model.CorrectLegacyProviderPayment(&model.AccountPayment{PaymentId: row.PaymentId, Payout: row.Payout,
				SubsidyPayout: row.SubsidyPayout, ReliabilitySubsidy: row.ReliabilitySubsidy}, window, policy)
		}
	}
	return corrections
}

// RC-3: Circle's record of the row's own attempt. Returns the record's wallet.
func snReconcileRecordAttempt(ctx context.Context, circleClient CircleApi, item *SnReconcileRowReport) string {
	record := *item.row.PaymentRecord
	item.Evidence = &model.SnReconcileEvidence{TransactionId: record}
	result, err := circleClient.GetTransaction(ctx, record)
	if err != nil || result.Transaction.Id != record {
		item.attempt = snReconcileRecordUnavailable
		return ""
	}
	item.Evidence.State, item.Evidence.TxHash = result.Transaction.State, result.Transaction.TxHash
	item.attempt = snReconcileJudge(item.row, &result.Transaction)
	return result.Transaction.WalletId
}

// RC-3's state rules for one Circle transaction of the row's own attempt.
func snReconcileJudge(row *model.SnReconcileRow, transaction *CircleTransaction) string {
	switch strings.ToUpper(transaction.State) {
	case "COMPLETE":
		return snReconcileAttemptComplete
	case "FAILED", "DENIED", "CANCELLED":
		if transaction.TxHash == "" && row.TxHash == nil {
			return snReconcileAttemptNoFunds
		}
		return snReconcileContradictory
	default:
		return snReconcileInFlight
	}
}

// A3: a row's orphan window opens a day before its earliest contract closed,
// or before the row was created when that is earlier.
func snReconcileWindowStart(row *model.SnReconcileRow) time.Time {
	start := row.CreateTime
	if row.EarliestClose != nil && row.EarliestClose.Before(start) {
		start = *row.EarliestClose
	}
	return start.Add(-snReconcileOrphanWindow)
}

// RC-5 and RC-6: list every wallet from `from`. The listing must hold every
// record stored inside its margins, and a listed transaction a record names
// must reach that row's wallet under the A4 address rule. A listing error
// fails the command; it is never a short result.
func snReconcileList(ctx context.Context, ledger CirclePayoutLedger, walletIds []string, from time.Time) (*SnReconcileListing, []CircleTransaction, error) {
	walletIds = slices.Clone(walletIds)
	slices.Sort(walletIds)
	walletIds = slices.Compact(walletIds)
	start := server.NowUtc()
	listing := &SnReconcileListing{Wallets: walletIds, From: &from, Start: &start, MissingIds: []string{}, MismatchedIds: []string{}}
	transactions := []CircleTransaction{}
	for _, walletId := range walletIds {
		listed, err := ledger.ListWalletTransactions(ctx, walletId, from)
		if err != nil {
			return nil, nil, fmt.Errorf("sn-reconcile: listing Circle wallet %s: %w", walletId, err)
		}
		transactions = append(transactions, listed...)
	}
	listing.TransactionCount = len(transactions)
	ids := []string{}
	byId := map[string]*CircleTransaction{}
	for i := range transactions {
		ids = append(ids, transactions[i].Id)
		byId[transactions[i].Id] = &transactions[i]
	}
	for _, record := range model.GetSnReconcileRecordsPaidBetween(ctx, from.Add(snReconcileCompletenessMargin), start.Add(-snReconcileCompletenessMargin)) {
		if byId[record] == nil {
			listing.MissingIds = append(listing.MissingIds, record)
		}
	}
	for _, address := range model.GetSnReconcileRecordAddresses(ctx, ids) {
		if address.WalletAddress == nil || model.SnReconcileAddress(*address.WalletAddress) != model.SnReconcileAddress(byId[address.Record].DestinationAddress) {
			listing.MismatchedIds = append(listing.MismatchedIds, address.Record)
		}
	}
	listing.MismatchedIds = slices.Compact(listing.MismatchedIds)
	listing.Complete = len(listing.MissingIds) == 0 && len(listing.MismatchedIds) == 0
	return listing, transactions, nil
}

// RC-7 and A4: the listed transfers no payment names, with the networks that
// registered each destination. A transfer to no network's address and to none
// of our own developer wallets is unattributed.
func snReconcileOrphans(ctx context.Context, transactions []CircleTransaction, own map[string]bool) ([]*SnReconcileOrphan, error) {
	ids := []string{}
	for _, transaction := range transactions {
		ids = append(ids, transaction.Id)
	}
	referenced := model.GetSnReconcileReferencedIds(ctx, ids)
	orphans := []*SnReconcileOrphan{}
	destinations := []string{}
	for _, transaction := range transactions {
		// Only a deposit into the payout wallet is never a payment; an empty or
		// unknown type still counts.
		if referenced[transaction.Id] || snReconcileFailedState(transaction.State) || transaction.TransactionType == "INBOUND" {
			continue
		}
		created, err := time.Parse(time.RFC3339, transaction.CreateDate)
		if err != nil {
			return nil, fmt.Errorf("sn-reconcile: Circle transaction %s has an unreadable createDate %q", transaction.Id, transaction.CreateDate)
		}
		orphans = append(orphans, &SnReconcileOrphan{Id: transaction.Id, WalletId: transaction.WalletId,
			TransactionType: transaction.TransactionType, State: transaction.State, Amounts: transaction.Amounts,
			CreateDate: transaction.CreateDate, Destination: transaction.DestinationAddress, created: created})
		destinations = append(destinations, model.SnReconcileAddress(transaction.DestinationAddress))
	}
	networks := model.GetSnReconcileAddressNetworks(ctx, destinations)
	for _, orphan := range orphans {
		destination := model.SnReconcileAddress(orphan.Destination)
		orphan.NetworkIds = networks[destination]
		orphan.Unattributed = len(orphan.NetworkIds) == 0 && !own[destination]
	}
	return orphans, nil
}

func snReconcileFailedState(state string) bool {
	switch strings.ToUpper(state) {
	case "FAILED", "DENIED", "CANCELLED":
		return true
	default:
		return false
	}
}

// §5 in order, as amended by A2 and A5; the first match decides.
func snReconcileClassify(item *SnReconcileRowReport, report *SnReconcileReport, transactions []CircleTransaction) {
	row := item.row
	// RC-9
	if row.AttributionReviewRequired {
		item.Class, item.Reason = snReconcileUnresolved, snReconcileReviewRequired
		return
	}
	if row.PaymentRecord == nil && row.TxHash != nil {
		item.attempt = snReconcileHashAttempt(item, transactions)
	}
	switch item.attempt {
	case snReconcileAttemptComplete:
		item.Class = snReconcilePaidReplanned
		if item.censusExact {
			item.Class = snReconcilePaid
		}
		return
	case "", snReconcileAttemptNoFunds:
	default:
		item.Class, item.Reason = snReconcileUnresolved, item.attempt
		return
	}
	// RC-6
	if !report.Listing.Complete {
		item.Class, item.Reason = snReconcileUnresolved, snReconcileListingIncomplete
		return
	}
	// RC-7
	if item.OrphanIds = snReconcileBlockingOrphans(row, report.Orphans); 0 < len(item.OrphanIds) {
		item.Class, item.Reason = snReconcileUnresolved, snReconcileOrphanTransfer
		return
	}
	// RC-8, A5, A8: a row without sweeps is `replanned` only as RC-1, when its
	// network created a row after its cancellation and before the fix, or a
	// later row holds its early sweeps. Its subsidy and reliability are listed
	// with their guidance.
	if !item.censusExact {
		switch {
		case 0 < row.SweepCount:
			item.Class, item.Reason = snReconcileUnresolved, snReconcileCensusMismatch
		case row.Kind == model.SnReconcileCanceledWithEvidence && (row.ReplannedBeforeFix || row.ReplanEvidence):
			item.Class = snReconcileReplanned
			item.Subsidy = &SnReconcileReplannedAmount{NanoCents: row.SubsidyPayout, Guidance: snReconcileSubsidyGuidance}
			item.Reliability = &SnReconcileReplannedAmount{NanoCents: row.ReliabilitySubsidy, Guidance: snReconcileReliabilityGuidance}
		default:
			item.Class, item.Reason = snReconcileUnresolved, snReconcileSweepsMissing
		}
		return
	}
	// A2
	if row.EarlyEarnings {
		item.Class, item.Reason = snReconcileUnresolved, snReconcileDuplicateRisk
		return
	}
	// RC-23: the July to September planner inflated subsidies and reliability
	// subsidies, and a payout is immutable, so a row the legacy correction
	// lowers is never paid as recorded.
	if item.correction != nil && item.correction.Changed() {
		item.Class, item.Reason, item.CorrectedPayoutNanoCents = snReconcileUnresolved, snReconcileInflated, &item.correction.Payout
		return
	}
	// RC-10
	chain, ok := snReconcilePayoutChain(row.PayoutWalletBlockchain, row.PayoutWalletActive)
	if !ok {
		item.Class, item.Reason = snReconcileUnresolved, snReconcileNoPayoutWallet
		return
	}
	// RC-11
	sendUsd, sendNanoCents := snReconcileSendAmount(row.Payout)
	if sendNanoCents <= 0 {
		item.Class, item.Reason = snReconcileUnresolved, snReconcileBelowFee
		return
	}
	// RC-24: upstream's reconcile-legacy-attempts decides RC-2 rows; one that
	// passes every check is reported, never revived.
	if row.Kind == model.SnReconcileUnretainedKey {
		item.Class, item.Reason = snReconcileUnresolved, snReconcileUpstreamScope
		return
	}
	// RC-12
	item.Class, item.OwedNanoCents, item.SendUsd, item.Chain, item.sendNanoCents = snReconcilePayable, row.Payout, sendUsd, chain, sendNanoCents
}

// RC-4: the listed transaction with the row's hash, judged by RC-3's rules.
// Two listed transactions with one hash contradict each other.
func snReconcileHashAttempt(item *SnReconcileRowReport, transactions []CircleTransaction) string {
	item.Evidence = &model.SnReconcileEvidence{TxHash: *item.row.TxHash}
	matches := []*CircleTransaction{}
	for i := range transactions {
		if strings.EqualFold(transactions[i].TxHash, *item.row.TxHash) {
			matches = append(matches, &transactions[i])
		}
	}
	switch len(matches) {
	case 0:
		return snReconcileRecordUnavailable
	case 1:
		item.Evidence.TransactionId, item.Evidence.State = matches[0].Id, matches[0].State
		return snReconcileJudge(item.row, matches[0])
	default:
		return snReconcileContradictory
	}
}

// RC-7: the orphans created inside the row's A3 window whose destination is the
// pinned wallet or any wallet of the row's network.
func snReconcileBlockingOrphans(row *model.SnReconcileRow, orphans []*SnReconcileOrphan) []string {
	start := snReconcileWindowStart(row)
	ids := []string{}
	for _, orphan := range orphans {
		if orphan.created.Before(start) {
			continue
		}
		if (row.PinnedWalletAddress != nil && model.SnReconcileAddress(*row.PinnedWalletAddress) == model.SnReconcileAddress(orphan.Destination)) ||
			slices.Contains(orphan.NetworkIds, row.NetworkId) {
			ids = append(ids, orphan.Id)
		}
	}
	return ids
}

// RC-10: the chain of an active payout wallet on a chain the sender supports.
func snReconcilePayoutChain(blockchain *string, active bool) (string, bool) {
	if blockchain == nil || !active {
		return "", false
	}
	chain, err := formatBlockchain(*blockchain)
	return chain, err == nil
}

// RC-11: the sender's own fresh-attempt amount.
func snReconcileSendAmount(payout model.NanoCents) (float64, model.NanoCents) {
	sendUsd := model.NanoCentsToUsd(payout) - providerPaymentFeeUsd
	return sendUsd, model.UsdToNanoCents(sendUsd)
}

// A7: what `--send` must find again: each payable row with its owed and send
// amounts and chain, and each unattributed orphan.
func snReconcileDigest(report *SnReconcileReport) string {
	lines := []string{}
	for _, item := range report.Rows {
		if item.Class == snReconcilePayable {
			lines = append(lines, fmt.Sprintf("payable %s %d %d %s", item.PaymentId, item.OwedNanoCents, item.sendNanoCents, item.Chain))
		}
	}
	for _, orphan := range report.Orphans {
		if orphan.Unattributed {
			lines = append(lines, "unattributed "+orphan.Id)
		}
	}
	slices.Sort(lines)
	digest := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(digest[:])
}

// §6 (A2): the rows holding early earnings, by network, with each network's
// RC-1 rows. Networks with a paid RC-1 row or an orphan come first.
func snReconcileDuplicates(ctx context.Context, report *SnReconcileReport) {
	byNetwork := map[server.Id]*SnReconcileDuplicates{}
	for _, holder := range model.GetSnReconcileEarlyHolders(ctx) {
		duplicates := byNetwork[holder.NetworkId]
		if duplicates == nil {
			duplicates = &SnReconcileDuplicates{NetworkId: holder.NetworkId, Canceled: []*SnReconcileRowReport{}}
			byNetwork[holder.NetworkId] = duplicates
			report.Duplicates = append(report.Duplicates, duplicates)
		}
		duplicates.Holders = append(duplicates.Holders, holder)
		duplicates.EarlyRevenueNanoCents += holder.EarlyRevenue
		duplicates.EarlyByteCount += holder.EarlyByteCount
	}
	first := map[server.Id]bool{}
	for _, item := range report.Rows {
		if duplicates := byNetwork[item.NetworkId]; duplicates != nil && item.Kind == model.SnReconcileCanceledWithEvidence {
			duplicates.Canceled = append(duplicates.Canceled, item)
			first[item.NetworkId] = first[item.NetworkId] || item.Class == snReconcilePaid || item.Class == snReconcilePaidReplanned
		}
	}
	for _, orphan := range report.Orphans {
		for _, networkId := range orphan.NetworkIds {
			first[networkId] = true
		}
	}
	for _, duplicates := range report.Duplicates {
		duplicates.CanceledByClass = snReconcileTotals(duplicates.Canceled)
	}
	slices.SortStableFunc(report.Duplicates, func(a *SnReconcileDuplicates, b *SnReconcileDuplicates) int {
		if first[a.NetworkId] != first[b.NetworkId] {
			if first[a.NetworkId] {
				return -1
			}
			return 1
		}
		return a.NetworkId.Cmp(b.NetworkId)
	})
}

// The count and total payout of each class and reason.
func snReconcileTotals(items []*SnReconcileRowReport) []*SnReconcileClassTotal {
	totals := []*SnReconcileClassTotal{}
	for _, item := range items {
		index := slices.IndexFunc(totals, func(total *SnReconcileClassTotal) bool {
			return total.Class == item.Class && total.Reason == item.Reason
		})
		if index < 0 {
			totals = append(totals, &SnReconcileClassTotal{Class: item.Class, Reason: item.Reason})
			index = len(totals) - 1
		}
		totals[index].Count += 1
		totals[index].PayoutNanoCents += item.PayoutNanoCents
	}
	slices.SortFunc(totals, func(a *SnReconcileClassTotal, b *SnReconcileClassTotal) int {
		return strings.Compare(a.Class+":"+a.Reason, b.Class+":"+b.Reason)
	})
	return totals
}

func snReconcileUsd(nanoCents model.NanoCents) string {
	return fmt.Sprintf("%.6f USD", model.NanoCentsToUsd(nanoCents))
}

func snReconcileOr(value string, empty string) string {
	if value == "" {
		return empty
	}
	return value
}

func snReconcileClassText(class string, reason string) string {
	if reason == "" {
		return class
	}
	return class + ": " + reason
}

func snReconcileListingText(b *strings.Builder, name string, listing *SnReconcileListing) {
	completeness := "complete"
	if !listing.Complete {
		completeness = fmt.Sprintf("INCOMPLETE, %d record(s) missing, %d record(s) at another address", len(listing.MissingIds), len(listing.MismatchedIds))
	}
	fmt.Fprintf(b, "%s: from %s, started %s, %d wallet(s), %d transaction(s), %s\n", name,
		listing.From.Format(time.RFC3339), listing.Start.Format(time.RFC3339), len(listing.Wallets), listing.TransactionCount, completeness)
	for _, walletId := range listing.Wallets {
		fmt.Fprintf(b, "  wallet %s\n", walletId)
	}
	for _, id := range listing.MissingIds {
		fmt.Fprintf(b, "  missing %s\n", id)
	}
	for _, id := range listing.MismatchedIds {
		fmt.Fprintf(b, "  mismatched %s\n", id)
	}
}

// Text renders the report for an operator, with the same content as its JSON.
func (self *SnReconcileReport) Text() string {
	b := &strings.Builder{}
	fmt.Fprintf(b, "sn-reconcile run %s at %s, mode %s, policy %s\n", self.RunId, self.Time.Format(time.RFC3339), self.Mode, snReconcileOr(self.PolicySha256, "none"))
	fmt.Fprintf(b, "digest %s\n", self.Digest)
	if self.Listing.From == nil {
		fmt.Fprintf(b, "listing: none, no stuck rows\n")
	} else {
		snReconcileListingText(b, "listing", self.Listing)
	}
	fmt.Fprintf(b, "orphans: %d\n", len(self.Orphans))
	for _, orphan := range self.Orphans {
		networkIds := []string{}
		for _, networkId := range orphan.NetworkIds {
			networkIds = append(networkIds, networkId.String())
		}
		attribution := snReconcileOr(strings.Join(networkIds, ","), "none")
		if orphan.Unattributed {
			attribution += ", UNATTRIBUTED"
		}
		fmt.Fprintf(b, "  %s %s %s %s created %s from wallet %s to %s, networks %s\n", orphan.Id, orphan.TransactionType, orphan.State,
			strings.Join(orphan.Amounts, ","), orphan.CreateDate, orphan.WalletId, orphan.Destination, attribution)
	}
	fmt.Fprintf(b, "classes:\n")
	for _, total := range self.Classes {
		fmt.Fprintf(b, "  %-32s %5d %s\n", snReconcileClassText(total.Class, total.Reason), total.Count, snReconcileUsd(total.PayoutNanoCents))
	}
	fmt.Fprintf(b, "funding:\n")
	for _, funding := range self.Funding {
		fmt.Fprintf(b, "  %-5s wallet %s: %d payable, send %s, %d revived pending, %s, balance %s, shortfall %s\n", funding.Chain, funding.WalletId,
			funding.PayableCount, snReconcileUsd(funding.SendNanoCents), funding.RevivedCount, snReconcileUsd(funding.RevivedNanoCents),
			snReconcileUsd(funding.BalanceNanoCents), snReconcileUsd(funding.ShortfallNanoCents))
	}
	fmt.Fprintf(b, "rows: %d\n", len(self.Rows))
	for _, item := range self.Rows {
		fmt.Fprintf(b, "  %s network %s %s %s, owed %s", item.PaymentId, item.NetworkId, item.Kind, snReconcileClassText(item.Class, item.Reason), snReconcileUsd(item.OwedNanoCents))
		if item.Class == snReconcilePayable {
			fmt.Fprintf(b, ", send %.6f USD on %s", item.SendUsd, item.Chain)
		}
		if item.Evidence != nil {
			fmt.Fprintf(b, ", circle %s %s %s", snReconcileOr(item.Evidence.TransactionId, "-"), snReconcileOr(item.Evidence.State, "-"), snReconcileOr(item.Evidence.TxHash, "-"))
		}
		if 0 < len(item.OrphanIds) {
			fmt.Fprintf(b, ", orphans %s", strings.Join(item.OrphanIds, ","))
		}
		if item.CorrectedPayoutNanoCents != nil {
			fmt.Fprintf(b, ", corrected payout %s", snReconcileUsd(*item.CorrectedPayoutNanoCents))
		}
		fmt.Fprintf(b, "\n")
		if item.Subsidy != nil {
			fmt.Fprintf(b, "    subsidy %s: %s\n", snReconcileUsd(item.Subsidy.NanoCents), item.Subsidy.Guidance)
		}
		if item.Reliability != nil {
			fmt.Fprintf(b, "    reliability %s: %s\n", snReconcileUsd(item.Reliability.NanoCents), item.Reliability.Guidance)
		}
	}
	fmt.Fprintf(b, "duplicates: %d network(s)\n", len(self.Duplicates))
	for _, duplicates := range self.Duplicates {
		fmt.Fprintf(b, "  network %s: early revenue %s, early bytes %d\n", duplicates.NetworkId, snReconcileUsd(duplicates.EarlyRevenueNanoCents), duplicates.EarlyByteCount)
		for _, total := range duplicates.CanceledByClass {
			fmt.Fprintf(b, "    canceled %s: %d row(s), payout %s\n", snReconcileClassText(total.Class, total.Reason), total.Count, snReconcileUsd(total.PayoutNanoCents))
		}
		for _, item := range duplicates.Canceled {
			fmt.Fprintf(b, "    canceled %s %s, payout %s\n", item.PaymentId, snReconcileClassText(item.Class, item.Reason), snReconcileUsd(item.PayoutNanoCents))
		}
		for _, holder := range duplicates.Holders {
			fmt.Fprintf(b, "    holder %s plan %s %s, early revenue %s, early bytes %d, early bonus %s, bytes %d, subsidy %s, reliability %s\n",
				holder.PaymentId, holder.PaymentPlanId, holder.State, snReconcileUsd(holder.EarlyRevenue), holder.EarlyByteCount, snReconcileUsd(holder.EarlyBonus),
				holder.PayoutByteCount, snReconcileUsd(holder.SubsidyPayout), snReconcileUsd(holder.ReliabilitySubsidy))
		}
	}
	if self.Mode == "send" {
		if self.Relisting != nil {
			snReconcileListingText(b, "relisting", self.Relisting)
		}
		fmt.Fprintf(b, "revived: %d\n", len(self.Revived))
		for _, paymentId := range self.Revived {
			fmt.Fprintf(b, "  %s\n", paymentId)
		}
		fmt.Fprintf(b, "skipped: %d\n", len(self.Skipped))
		for _, skip := range self.Skipped {
			fmt.Fprintf(b, "  %s %s", skip.PaymentId, skip.Reason)
			if 0 < len(skip.OrphanIds) {
				fmt.Fprintf(b, ", orphans %s", strings.Join(skip.OrphanIds, ","))
			}
			fmt.Fprintf(b, "\n")
		}
	}
	return b.String()
}
