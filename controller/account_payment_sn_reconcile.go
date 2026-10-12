// The one-time sn-reconcile command pays only the legacy USDC rows that neither
// the planner nor the sender pays on its own (SN-RECONCILE.md). Circle is only
// read here; the existing sender moves the money for each revived row.
package controller

import (
	"context"
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

// Reasons of an unresolved row.
const (
	snReconcileReviewRequired    = "review_required"
	snReconcileContradictory     = "contradictory"
	snReconcileInFlight          = "in_flight"
	snReconcileRecordUnavailable = "record_unavailable"
	snReconcileListingIncomplete = "listing_incomplete"
	snReconcileOrphanTransfer    = "orphan_transfer"
	snReconcileCensusMismatch    = "census_mismatch"
	snReconcileDuplicateRisk     = "duplicate_risk"
	snReconcileNoPayoutWallet    = "no_payout_wallet"
	snReconcileBelowFee          = "below_fee"
)

// Outcomes of the row's own attempt that do not stop its classification.
const (
	snReconcileAttemptComplete = "complete"
	snReconcileAttemptNoFunds  = "no_funds"
)

// An orphan may pay a row created up to this long after it, and the listing
// starts this long before the oldest stuck row.
const snReconcileOrphanWindow = 24 * time.Hour

// Records stored this close to either end of the listing need not appear yet.
const snReconcileCompletenessMargin = time.Hour

var ErrSnReconcileUnfunded = errors.New("sn-reconcile: a chain's payable send total exceeds its payout wallet's USDC balance; nothing was revived")

type SnReconcileReport struct {
	RunId        server.Id                `json:"run_id"`
	Time         time.Time                `json:"time"`
	Mode         string                   `json:"mode"`
	PolicySha256 string                   `json:"policy_sha256"`
	Listing      *SnReconcileListing      `json:"listing"`
	Orphans      []*SnReconcileOrphan     `json:"orphans"`
	Classes      []*SnReconcileClassTotal `json:"classes"`
	Funding      []*SnReconcileFunding    `json:"funding"`
	Rows         []*SnReconcileRowReport  `json:"rows"`
	Duplicates   []*SnReconcileDuplicates `json:"duplicates"`
	Revived      []server.Id              `json:"revived"`
	Skipped      []server.Id              `json:"skipped"`
}

// With no stuck row nothing is listed and `From` and `Start` are nil.
type SnReconcileListing struct {
	Wallets          []string   `json:"wallets"`
	From             *time.Time `json:"from"`
	Start            *time.Time `json:"start"`
	TransactionCount int        `json:"transaction_count"`
	Complete         bool       `json:"complete"`
	MissingIds       []string   `json:"missing_ids"`
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

	created time.Time
}

type SnReconcileClassTotal struct {
	Class           string          `json:"class"`
	Reason          string          `json:"reason,omitempty"`
	Count           int             `json:"count"`
	PayoutNanoCents model.NanoCents `json:"payout_nano_cents"`
}

// The payable send total of one chain against its payout wallet (RC-13).
type SnReconcileFunding struct {
	Chain              string          `json:"chain"`
	WalletId           string          `json:"wallet_id"`
	PayableCount       int             `json:"payable_count"`
	SendNanoCents      model.NanoCents `json:"send_nano_cents"`
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

	row           *model.SnReconcileRow
	censusExact   bool
	attempt       string
	sendNanoCents model.NanoCents
}

// One network's possibly re-paid earnings (§6). Read only.
type SnReconcileDuplicates struct {
	NetworkId              server.Id                    `json:"network_id"`
	Sources                []*SnReconcileRowReport      `json:"sources"`
	PaidTwiceRiskNanoCents model.NanoCents              `json:"paid_twice_risk_nano_cents"`
	Queue                  []*model.SnReconcileQueueRow `json:"queue"`
	CandidateNanoCents     model.NanoCents              `json:"candidate_nano_cents"`
	AtRiskNanoCents        model.NanoCents              `json:"at_risk_nano_cents"`
}

// SnReconcile runs the census and, with `send`, revives the payable rows for
// the existing sender. The dry run writes nothing and calls only Circle GET
// endpoints (RC-18). A funding refusal still returns its report.
func SnReconcile(clientSession *session.ClientSession, send bool) (*SnReconcileReport, error) {
	ctx := clientSession.Ctx
	report := &SnReconcileReport{
		RunId:      server.NewId(),
		Time:       server.NowUtc(),
		Mode:       "dry_run",
		Listing:    &SnReconcileListing{Wallets: []string{}, Complete: true, MissingIds: []string{}},
		Orphans:    []*SnReconcileOrphan{},
		Classes:    []*SnReconcileClassTotal{},
		Rows:       []*SnReconcileRowReport{},
		Duplicates: []*SnReconcileDuplicates{},
		Revived:    []server.Id{},
		Skipped:    []server.Id{},
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
	circleClient := NewCircleClient()
	for _, row := range rows {
		item := &SnReconcileRowReport{PaymentId: row.PaymentId, NetworkId: row.NetworkId, Kind: row.Kind, PayoutNanoCents: row.Payout,
			row: row, censusExact: model.SnReconcileCensusExact(ctx, row.PaymentId, policy)}
		if row.PaymentRecord != nil {
			if walletId := snReconcileRecordAttempt(ctx, circleClient, item); walletId != "" {
				walletIds = append(walletIds, walletId)
			}
		}
		report.Rows = append(report.Rows, item)
	}
	ledger := NewCirclePayoutLedger()
	transactions := []CircleTransaction{}
	if 0 < len(rows) {
		transactions, err = snReconcileListing(ctx, ledger, report, rows[0].CreateTime.Add(-snReconcileOrphanWindow), walletIds)
		if err != nil {
			return nil, err
		}
	}
	snReconcileClassify(ctx, report, transactions)
	snReconcileDuplicates(ctx, report)
	snReconcileClassTotals(report)
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
		funding.ShortfallNanoCents = max(0, funding.SendNanoCents-funding.BalanceNanoCents)
	}
	if !send {
		return report, nil
	}
	// RC-13: refuse before any write.
	for _, funding := range report.Funding {
		if 0 < funding.ShortfallNanoCents {
			return report, ErrSnReconcileUnfunded
		}
	}
	for _, item := range report.Rows {
		if item.Class != snReconcilePayable {
			continue
		}
		paymentId := item.PaymentId
		revived := model.ReviveSnReconcileRow(ctx, item.row, policy, report.RunId, item.Evidence, func(tx server.PgTx) {
			ScheduleAdvancePayment(&AdvancePaymentArgs{PaymentId: paymentId}, clientSession, tx)
		})
		if revived {
			report.Revived = append(report.Revived, paymentId)
		} else {
			report.Skipped = append(report.Skipped, paymentId)
		}
	}
	return report, nil
}

// The configured developer wallet and USDC token of each payout chain.
func snReconcileConfiguredFunding() ([]*SnReconcileFunding, error) {
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

// RC-5 to RC-7: list every wallet of the wallet set from `from`, check the
// listing holds every record stored inside its margins, and report the
// transfers that no payment names. A listing error fails the command.
func snReconcileListing(ctx context.Context, ledger CirclePayoutLedger, report *SnReconcileReport, from time.Time, walletIds []string) ([]CircleTransaction, error) {
	walletIds = append(walletIds, model.GetSnReconcileReceiptWalletIds(ctx, from)...)
	slices.Sort(walletIds)
	walletIds = slices.Compact(walletIds)
	start := server.NowUtc()
	report.Listing.Wallets, report.Listing.From, report.Listing.Start = walletIds, &from, &start
	transactions := []CircleTransaction{}
	for _, walletId := range walletIds {
		listed, err := ledger.ListWalletTransactions(ctx, walletId, from)
		if err != nil {
			return nil, fmt.Errorf("sn-reconcile: listing Circle wallet %s: %w", walletId, err)
		}
		transactions = append(transactions, listed...)
	}
	report.Listing.TransactionCount = len(transactions)
	ids := []string{}
	listed := map[string]bool{}
	for _, transaction := range transactions {
		ids = append(ids, transaction.Id)
		listed[transaction.Id] = true
	}
	for _, record := range model.GetSnReconcileRecordsPaidBetween(ctx, from.Add(snReconcileCompletenessMargin), start.Add(-snReconcileCompletenessMargin)) {
		if !listed[record] {
			report.Listing.MissingIds = append(report.Listing.MissingIds, record)
		}
	}
	report.Listing.Complete = len(report.Listing.MissingIds) == 0
	referenced := model.GetSnReconcileReferencedIds(ctx, ids)
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
		report.Orphans = append(report.Orphans, &SnReconcileOrphan{Id: transaction.Id, WalletId: transaction.WalletId,
			TransactionType: transaction.TransactionType, State: transaction.State, Amounts: transaction.Amounts,
			CreateDate: transaction.CreateDate, Destination: transaction.DestinationAddress, created: created})
		destinations = append(destinations, strings.ToLower(transaction.DestinationAddress))
	}
	networks := model.GetSnReconcileAddressNetworks(ctx, destinations)
	for _, orphan := range report.Orphans {
		orphan.NetworkIds = networks[strings.ToLower(orphan.Destination)]
	}
	return transactions, nil
}

func snReconcileFailedState(state string) bool {
	switch strings.ToUpper(state) {
	case "FAILED", "DENIED", "CANCELLED":
		return true
	default:
		return false
	}
}

// §5 in order. Steps 1 to 5 decide every row a source could be; RC-21 then
// reads the sources of the rows that passed RC-8, before RC-10 to RC-12.
func snReconcileClassify(ctx context.Context, report *SnReconcileReport, transactions []CircleTransaction) {
	for _, item := range report.Rows {
		snReconcileClassifyAttempt(item, report, transactions)
	}
	sources := snReconcileSources(report)
	for _, item := range report.Rows {
		if item.Class == "" {
			snReconcileClassifyPayable(ctx, item, sources[item.NetworkId])
		}
	}
}

// Steps 1 to 5 of §5. A row whose census is exact and that nothing blocked
// keeps an empty class.
func snReconcileClassifyAttempt(item *SnReconcileRowReport, report *SnReconcileReport, transactions []CircleTransaction) {
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
	// RC-8
	if !item.censusExact {
		item.Class, item.Reason = snReconcileUnresolved, snReconcileCensusMismatch
		if row.SweepCount == 0 {
			item.Class, item.Reason = snReconcileReplanned, ""
		}
	}
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

// RC-7: the orphans created at or after the row's create time less the window
// whose destination is the pinned wallet or any wallet of the row's network.
func snReconcileBlockingOrphans(row *model.SnReconcileRow, orphans []*SnReconcileOrphan) []string {
	ids := []string{}
	for _, orphan := range orphans {
		if orphan.created.Before(row.CreateTime.Add(-snReconcileOrphanWindow)) {
			continue
		}
		if (row.PinnedWalletAddress != nil && strings.EqualFold(*row.PinnedWalletAddress, orphan.Destination)) ||
			slices.Contains(orphan.NetworkIds, row.NetworkId) {
			ids = append(ids, orphan.Id)
		}
	}
	return ids
}

// RC-21 sources by network: RC-1 rows whose census is not exact and that are
// not proven unpaid.
func snReconcileSources(report *SnReconcileReport) map[server.Id][]*SnReconcileRowReport {
	sources := map[server.Id][]*SnReconcileRowReport{}
	for _, item := range report.Rows {
		if item.Kind == model.SnReconcileCanceledWithEvidence && !item.censusExact &&
			(item.Class == snReconcilePaidReplanned || item.Class == snReconcileUnresolved) {
			sources[item.NetworkId] = append(sources[item.NetworkId], item)
		}
	}
	return sources
}

// The earliest COALESCE(cancel_time, create_time) and latest create_time of a
// network's sources, and their ids.
func snReconcileSourceWindow(sources []*SnReconcileRowReport) (after time.Time, before time.Time, ids []server.Id) {
	for i, source := range sources {
		start := source.row.CreateTime
		if source.row.CancelTime != nil {
			start = *source.row.CancelTime
		}
		if i == 0 || start.Before(after) {
			after = start
		}
		if i == 0 || source.row.CreateTime.After(before) {
			before = source.row.CreateTime
		}
		ids = append(ids, source.PaymentId)
	}
	return
}

// Steps 5 (RC-21) to 8 of §5 for a row whose census is exact.
func snReconcileClassifyPayable(ctx context.Context, item *SnReconcileRowReport, sources []*SnReconcileRowReport) {
	row := item.row
	// RC-21
	if 0 < len(sources) {
		after, before, ids := snReconcileSourceWindow(sources)
		if row.CreateTime.After(after) && model.SnReconcileHoldsEarlierEarnings(ctx, row.PaymentId, before, ids) {
			item.Class, item.Reason = snReconcileUnresolved, snReconcileDuplicateRisk
			return
		}
	}
	// RC-10
	if row.PayoutWalletId == nil || !row.PayoutWalletActive {
		item.Class, item.Reason = snReconcileUnresolved, snReconcileNoPayoutWallet
		return
	}
	chain, err := formatBlockchain(*row.PayoutWalletBlockchain)
	if err != nil {
		item.Class, item.Reason = snReconcileUnresolved, snReconcileNoPayoutWallet
		return
	}
	// RC-11: the sender's own fresh-attempt amount and threshold.
	sendUsd := model.NanoCentsToUsd(row.Payout) - providerPaymentFeeUsd
	if model.UsdToNanoCents(sendUsd) <= 0 {
		item.Class, item.Reason = snReconcileUnresolved, snReconcileBelowFee
		return
	}
	// RC-12
	item.Class, item.OwedNanoCents, item.SendUsd, item.Chain = snReconcilePayable, row.Payout, sendUsd, chain
	item.sendNanoCents = model.UsdToNanoCents(sendUsd)
}

// RC-17: each network with sources, with its pending rows that may re-pay them.
func snReconcileDuplicates(ctx context.Context, report *SnReconcileReport) {
	sources := snReconcileSources(report)
	networkIds := []server.Id{}
	for networkId := range sources {
		networkIds = append(networkIds, networkId)
	}
	slices.SortFunc(networkIds, func(a server.Id, b server.Id) int {
		return a.Cmp(b)
	})
	for _, networkId := range networkIds {
		group := sources[networkId]
		after, before, ids := snReconcileSourceWindow(group)
		duplicates := &SnReconcileDuplicates{NetworkId: networkId, Sources: group,
			Queue: model.GetSnReconcileQueueRows(ctx, networkId, after, before, ids)}
		for _, source := range group {
			duplicates.PaidTwiceRiskNanoCents += source.row.Payout - source.row.SubsidyPayout - source.row.ReliabilitySubsidy
		}
		for _, queued := range duplicates.Queue {
			duplicates.CandidateNanoCents += queued.Candidate
		}
		duplicates.AtRiskNanoCents = min(duplicates.PaidTwiceRiskNanoCents, duplicates.CandidateNanoCents)
		report.Duplicates = append(report.Duplicates, duplicates)
	}
}

func snReconcileClassTotals(report *SnReconcileReport) {
	for _, item := range report.Rows {
		index := slices.IndexFunc(report.Classes, func(total *SnReconcileClassTotal) bool {
			return total.Class == item.Class && total.Reason == item.Reason
		})
		if index < 0 {
			report.Classes = append(report.Classes, &SnReconcileClassTotal{Class: item.Class, Reason: item.Reason})
			index = len(report.Classes) - 1
		}
		report.Classes[index].Count += 1
		report.Classes[index].PayoutNanoCents += item.PayoutNanoCents
	}
	slices.SortFunc(report.Classes, func(a *SnReconcileClassTotal, b *SnReconcileClassTotal) int {
		return strings.Compare(a.Class+":"+a.Reason, b.Class+":"+b.Reason)
	})
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

// Text renders the report for an operator, with the same content as its JSON.
func (self *SnReconcileReport) Text() string {
	b := &strings.Builder{}
	fmt.Fprintf(b, "sn-reconcile run %s at %s, mode %s, policy %s\n", self.RunId, self.Time.Format(time.RFC3339), self.Mode, snReconcileOr(self.PolicySha256, "none"))
	listing := self.Listing
	if listing.From == nil {
		fmt.Fprintf(b, "listing: none, no stuck rows\n")
	} else {
		completeness := "complete"
		if !listing.Complete {
			completeness = fmt.Sprintf("INCOMPLETE, %d record(s) missing", len(listing.MissingIds))
		}
		fmt.Fprintf(b, "listing: from %s, started %s, %d wallet(s), %d transaction(s), %s\n",
			listing.From.Format(time.RFC3339), listing.Start.Format(time.RFC3339), len(listing.Wallets), listing.TransactionCount, completeness)
		for _, walletId := range listing.Wallets {
			fmt.Fprintf(b, "  wallet %s\n", walletId)
		}
		for _, id := range listing.MissingIds {
			fmt.Fprintf(b, "  missing %s\n", id)
		}
	}
	fmt.Fprintf(b, "orphans: %d\n", len(self.Orphans))
	for _, orphan := range self.Orphans {
		networkIds := []string{}
		for _, networkId := range orphan.NetworkIds {
			networkIds = append(networkIds, networkId.String())
		}
		fmt.Fprintf(b, "  %s %s %s %s created %s from wallet %s to %s, networks %s\n", orphan.Id, orphan.TransactionType, orphan.State,
			strings.Join(orphan.Amounts, ","), orphan.CreateDate, orphan.WalletId, orphan.Destination, snReconcileOr(strings.Join(networkIds, ","), "none"))
	}
	fmt.Fprintf(b, "classes:\n")
	for _, total := range self.Classes {
		class := total.Class
		if total.Reason != "" {
			class += ": " + total.Reason
		}
		fmt.Fprintf(b, "  %-32s %5d %s\n", class, total.Count, snReconcileUsd(total.PayoutNanoCents))
	}
	fmt.Fprintf(b, "funding:\n")
	for _, funding := range self.Funding {
		fmt.Fprintf(b, "  %-5s wallet %s: %d payable, send %s, balance %s, shortfall %s\n", funding.Chain, funding.WalletId, funding.PayableCount,
			snReconcileUsd(funding.SendNanoCents), snReconcileUsd(funding.BalanceNanoCents), snReconcileUsd(funding.ShortfallNanoCents))
	}
	fmt.Fprintf(b, "rows: %d\n", len(self.Rows))
	for _, item := range self.Rows {
		class := item.Class
		if item.Reason != "" {
			class += ": " + item.Reason
		}
		fmt.Fprintf(b, "  %s network %s %s %s, owed %s", item.PaymentId, item.NetworkId, item.Kind, class, snReconcileUsd(item.OwedNanoCents))
		if item.Class == snReconcilePayable {
			fmt.Fprintf(b, ", send %.6f USD on %s", item.SendUsd, item.Chain)
		}
		if item.Evidence != nil {
			fmt.Fprintf(b, ", circle %s %s %s", snReconcileOr(item.Evidence.TransactionId, "-"), snReconcileOr(item.Evidence.State, "-"), snReconcileOr(item.Evidence.TxHash, "-"))
		}
		if 0 < len(item.OrphanIds) {
			fmt.Fprintf(b, ", orphans %s", strings.Join(item.OrphanIds, ","))
		}
		fmt.Fprintf(b, "\n")
	}
	fmt.Fprintf(b, "duplicates: %d network(s)\n", len(self.Duplicates))
	for _, duplicates := range self.Duplicates {
		fmt.Fprintf(b, "  network %s: paid twice risk %s, candidates %s, at risk %s\n", duplicates.NetworkId,
			snReconcileUsd(duplicates.PaidTwiceRiskNanoCents), snReconcileUsd(duplicates.CandidateNanoCents), snReconcileUsd(duplicates.AtRiskNanoCents))
		for _, source := range duplicates.Sources {
			fmt.Fprintf(b, "    source %s %s, payout %s\n", source.PaymentId, snReconcileOr(source.Reason, source.Class), snReconcileUsd(source.PayoutNanoCents))
		}
		for _, queued := range duplicates.Queue {
			fmt.Fprintf(b, "    queue %s plan %s %s, payout %s, candidate %s\n", queued.PaymentId, queued.PaymentPlanId, queued.State,
				snReconcileUsd(queued.Payout), snReconcileUsd(queued.Candidate))
		}
	}
	if self.Mode == "send" {
		fmt.Fprintf(b, "revived: %d\n", len(self.Revived))
		for _, paymentId := range self.Revived {
			fmt.Fprintf(b, "  %s\n", paymentId)
		}
		fmt.Fprintf(b, "skipped: %d\n", len(self.Skipped))
		for _, paymentId := range self.Skipped {
			fmt.Fprintf(b, "  %s changed\n", paymentId)
		}
	}
	return b.String()
}
