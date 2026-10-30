// `bringyourctl payouts reimburse` repays listed networks fixed USDC amounts,
// each to one pinned wallet of its own, through the ordinary Circle payout path
// (owner-approved, 2026-10-26: networks whose May 2026 payouts went to
// model.ManualReimbursementRefusedAddress instead of their own wallets).
//
// The dry run only reads. It prints every row, the total, the fee handling and
// a digest over the exact rows and reason. `--send --expect=<digest>` runs the
// same census again and refuses unless its digest matches, USDC payments are on
// and the earning policy loads. It then creates one plan, with one payment and
// its marker per row, in one transaction (model.CreateManualReimbursementPlan),
// and advances each payment with AdvancePayment. The plan and payment ids derive
// from the digest, so a rerun with the same digest creates nothing and only
// advances what is not yet sent.
//
// A network that already has an open manual reimbursement of another plan is
// refused, unless --allow_prior_reimbursement is given (owner-approved,
// 2026-10-27: per-window reliability subsidies owed to networks that were
// already reimbursed). With it, each row lists its network's prior
// reimbursements with their plan, amount and reason; the digest also covers the
// flag and each prior reimbursement's payment id, and each marker lists them,
// so a send refuses when its dry run ran without the flag or saw other prior
// reimbursements. The plan and payment ids still derive from the digest of the
// rows, reason and fee alone: a run names its own plan before it reads which
// reimbursements are prior to it, so its own payments are never prior, and the
// same rows and reason name the same plan with or without the flag.
package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

const payoutReimbursementCsvHeader = "network_id,wallet_id,amount_usdc"

const payoutReimbursementMaxRows = 1000

const payoutReimbursementMaxReasonLength = 1024

var ErrPayoutReimbursementRefused = errors.New("payouts reimburse: refused; nothing was written")

var ErrPayoutReimbursementDigestMismatch = errors.New("payouts reimburse: --expect does not match the digest of these rows and reason; nothing was written")

// 1 to 9 whole digits, then at most 6 decimals: USDC has 6.
var payoutReimbursementAmountPattern = regexp.MustCompile(`^([0-9]{1,9})(?:\.([0-9]{1,6}))?$`)

// The amount in micro USDC.
func parsePayoutReimbursementAmount(text string) (int64, error) {
	match := payoutReimbursementAmountPattern.FindStringSubmatch(text)
	if match == nil {
		return 0, fmt.Errorf("amount %q is not a decimal USDC amount with at most 6 decimals", text)
	}
	whole, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return 0, err
	}
	fraction := int64(0)
	if match[2] != "" {
		fraction, err = strconv.ParseInt(match[2]+strings.Repeat("0", 6-len(match[2])), 10, 64)
		if err != nil {
			return 0, err
		}
	}
	microUsdc := whole*1_000_000 + fraction
	if microUsdc <= 0 {
		return 0, fmt.Errorf("amount %q is not positive", text)
	}
	return microUsdc, nil
}

func formatPayoutReimbursementMicroUsdc(microUsdc int64) string {
	return fmt.Sprintf("%d.%06d", microUsdc/1_000_000, microUsdc%1_000_000)
}

func formatPayoutReimbursementNanoCents(nanoCents model.NanoCents) string {
	return fmt.Sprintf("%.6f", model.NanoCentsToUsd(nanoCents))
}

type payoutReimbursementInput struct {
	line      int
	networkId server.Id
	walletId  server.Id
	microUsdc int64
}

// The rows of the file, or why it is refused. The header must be exactly
// network_id,wallet_id,amount_usdc, and no network may repeat.
func parsePayoutReimbursementCsv(data []byte) ([]*payoutReimbursementInput, []string) {
	problems := []string{}
	reader := csv.NewReader(bytes.NewReader(data))
	reader.FieldsPerRecord = 3
	header, err := reader.Read()
	if err != nil {
		return nil, []string{fmt.Sprintf("the file has no %s header: %v", payoutReimbursementCsvHeader, err)}
	}
	if strings.Join(header, ",") != payoutReimbursementCsvHeader {
		return nil, []string{fmt.Sprintf("the header is %q, not %s", strings.Join(header, ","), payoutReimbursementCsvHeader)}
	}
	inputs := []*payoutReimbursementInput{}
	networkLines := map[server.Id]int{}
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			problems = append(problems, err.Error())
			break
		}
		line, _ := reader.FieldPos(0)
		input := &payoutReimbursementInput{line: line}
		var networkErr, walletErr, amountErr error
		input.networkId, networkErr = server.ParseId(record[0])
		input.walletId, walletErr = server.ParseId(record[1])
		input.microUsdc, amountErr = parsePayoutReimbursementAmount(record[2])
		if networkErr != nil {
			problems = append(problems, fmt.Sprintf("line %d: network_id %q is not an id", line, record[0]))
		}
		if walletErr != nil {
			problems = append(problems, fmt.Sprintf("line %d: wallet_id %q is not an id", line, record[1]))
		}
		if amountErr != nil {
			problems = append(problems, fmt.Sprintf("line %d: %s", line, amountErr))
		}
		if networkErr != nil || walletErr != nil || amountErr != nil {
			continue
		}
		if earlier, ok := networkLines[input.networkId]; ok {
			problems = append(problems, fmt.Sprintf("line %d: network %s repeats line %d", line, input.networkId, earlier))
			continue
		}
		networkLines[input.networkId] = line
		inputs = append(inputs, input)
	}
	if len(inputs) == 0 && len(problems) == 0 {
		problems = append(problems, "the file lists no rows")
	}
	if payoutReimbursementMaxRows < len(inputs) {
		problems = append(problems, fmt.Sprintf("the file lists %d rows, more than %d", len(inputs), payoutReimbursementMaxRows))
	}
	return inputs, problems
}

// What the sender asks Circle for, from a payout: advancePayment deducts
// providerPaymentFeeUsd from a fresh attempt's payout, and CreateTransferTransaction
// formats the amount with %f.
func payoutReimbursementCircleAmount(payout model.NanoCents) (float64, string) {
	amount := model.NanoCentsToUsd(payout) - providerPaymentFeeUsd
	return amount, fmt.Sprintf("%f", amount)
}

// The payout whose transfer carries exactly the listed amount: the amount plus
// the sender's fixed fee. Refuses an amount the float arithmetic cannot carry
// exactly.
func payoutReimbursementPayout(microUsdc int64) (amount model.NanoCents, fee model.NanoCents, payout model.NanoCents, err error) {
	amount = model.NanoCents(microUsdc) * 1000
	fee = model.UsdToNanoCents(providerPaymentFeeUsd)
	payout = amount + fee
	circleAmount, circleText := payoutReimbursementCircleAmount(payout)
	if circleText != formatPayoutReimbursementMicroUsdc(microUsdc) || model.UsdToNanoCents(circleAmount) != amount {
		err = fmt.Errorf("the sender would ask Circle for %s, not %s", circleText, formatPayoutReimbursementMicroUsdc(microUsdc))
	}
	return
}

// A plan or payment id derived from the digest: the same rows and reason name
// the same plan and payments.
func payoutReimbursementId(parts ...string) server.Id {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	id := sum[:16]
	// a version 8 (custom) RFC 9562 UUID
	id[6] = (id[6] & 0x0f) | 0x80
	id[8] = (id[8] & 0x3f) | 0x80
	return server.RequireIdFromBytes(id)
}

func payoutReimbursementPlanId(digest string) server.Id {
	return payoutReimbursementId("urnetwork/payouts-reimburse/plan/v1", digest)
}

func payoutReimbursementPaymentId(digest string, networkId server.Id) server.Id {
	return payoutReimbursementId("urnetwork/payouts-reimburse/payment/v1", digest, networkId.String())
}

// sha256 over the reason, the fee, and each row's network, wallet, chain,
// address, amount and payout, in network order. The plan and payment ids derive
// from it, with or without --allow_prior_reimbursement.
func payoutReimbursementDigest(reason string, fee model.NanoCents, rows []*PayoutReimbursementRow) string {
	return payoutReimbursementSum(payoutReimbursementDigestLines(reason, fee, rows, false))
}

// The digest with --allow_prior_reimbursement: sha256 over the same lines, then
// the flag, then the network and payment id of each row's prior
// reimbursements, in network and payment id order.
func payoutReimbursementAllowPriorDigest(reason string, fee model.NanoCents, rows []*PayoutReimbursementRow) string {
	return payoutReimbursementSum(payoutReimbursementDigestLines(reason, fee, rows, true))
}

// The canonical text of both digests, one line each. A reason or address is
// quoted, so no field can add a line.
func payoutReimbursementDigestLines(reason string, fee model.NanoCents, rows []*PayoutReimbursementRow, allowPriorReimbursement bool) []string {
	lines := []string{
		"urnetwork payouts reimburse v1",
		"reason " + strconv.Quote(reason),
		fmt.Sprintf("fee_nano_cents %d", fee),
	}
	sorted := slices.Clone(rows)
	slices.SortFunc(sorted, func(a *PayoutReimbursementRow, b *PayoutReimbursementRow) int {
		return a.NetworkId.Cmp(b.NetworkId)
	})
	for _, row := range sorted {
		lines = append(lines, fmt.Sprintf("row %s %s %s %s %s %d", row.NetworkId, row.WalletId, strconv.Quote(row.Blockchain),
			strconv.Quote(row.WalletAddress), row.Amount, row.PayoutNanoCents))
	}
	if allowPriorReimbursement {
		lines = append(lines, "allow_prior_reimbursement")
		for _, row := range sorted {
			for _, priorPaymentId := range row.priorPaymentIds() {
				lines = append(lines, fmt.Sprintf("prior %s %s", row.NetworkId, priorPaymentId))
			}
		}
	}
	return lines
}

// sha256 of the lines joined by newlines, in hex.
func payoutReimbursementSum(lines []string) string {
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

type PayoutReimbursementRow struct {
	Line          int       `json:"line"`
	NetworkId     server.Id `json:"network_id"`
	NetworkName   string    `json:"network_name"`
	WalletId      server.Id `json:"wallet_id"`
	WalletAddress string    `json:"wallet_address"`
	// account_wallet.blockchain as stored, and the chain the sender pays on
	Blockchain string `json:"blockchain"`
	Chain      string `json:"chain"`
	// the listed USDC
	Amount          string          `json:"amount_usdc"`
	AmountNanoCents model.NanoCents `json:"amount_nano_cents"`
	PayoutNanoCents model.NanoCents `json:"payout_nano_cents"`
	// what the sender will ask Circle for
	CircleAmount string    `json:"circle_amount"`
	PaymentId    server.Id `json:"payment_id"`
	// --allow_prior_reimbursement: the network's open manual reimbursements of
	// other plans, which this payment adds to
	PriorReimbursements []*PayoutReimbursementPrior `json:"prior_reimbursements,omitempty"`

	// The payment, once the plan exists: pending, reserved (an attempt without
	// a Circle record), submitted, complete or canceled.
	State      string `json:"state,omitempty"`
	TransferId string `json:"circle_transfer_id,omitempty"`
	TxHash     string `json:"tx_hash,omitempty"`
	// --send: the advance of this run
	Outcome string `json:"outcome,omitempty"`
}

// The payment ids of the prior reimbursements in increasing order, as the
// digest and the marker list them; nil without any.
func (self *PayoutReimbursementRow) priorPaymentIds() []server.Id {
	var priorPaymentIds []server.Id
	for _, prior := range self.PriorReimbursements {
		priorPaymentIds = append(priorPaymentIds, prior.PaymentId)
	}
	slices.SortFunc(priorPaymentIds, func(a server.Id, b server.Id) int {
		return a.Cmp(b)
	})
	return priorPaymentIds
}

// An open (not canceled) manual reimbursement of another plan for a row's
// network, listed with --allow_prior_reimbursement.
type PayoutReimbursementPrior struct {
	PaymentId     server.Id `json:"payment_id"`
	PaymentPlanId server.Id `json:"payment_plan_id"`
	// from its marker: the USDC its wallet receives, and its reason
	AmountNanoCents model.NanoCents `json:"amount_nano_cents"`
	Reason          string          `json:"reason"`
	Completed       bool            `json:"completed"`
}

type PayoutReimbursementChainTotal struct {
	Chain           string          `json:"chain"`
	Count           int             `json:"count"`
	AmountNanoCents model.NanoCents `json:"amount_nano_cents"`
}

type PayoutReimbursementReport struct {
	RunId         server.Id `json:"run_id"`
	Time          time.Time `json:"time"`
	Mode          string    `json:"mode"`
	Reason        string    `json:"reason"`
	Digest        string    `json:"digest"`
	PaymentPlanId server.Id `json:"payment_plan_id"`
	// open manual reimbursements of other plans are listed on their rows
	// instead of refused, and the digest binds them
	AllowPriorReimbursement bool `json:"allow_prior_reimbursement,omitempty"`
	// the sender's fixed fee per payout
	FeeNanoCents model.NanoCents `json:"fee_nano_cents"`
	// usdc_payout.yml payments: on, off, or why it is unreadable
	UsdcPayments string `json:"usdc_payments"`
	// sn.yml: none, its sha256, or why it is unavailable
	EarningPolicy   string                           `json:"earning_policy"`
	Rows            []*PayoutReimbursementRow        `json:"rows"`
	Chains          []*PayoutReimbursementChainTotal `json:"chains"`
	AmountNanoCents model.NanoCents                  `json:"amount_nano_cents"`
	PayoutNanoCents model.NanoCents                  `json:"payout_nano_cents"`
	// payments the plan holds now; 0 until --send creates them
	PlanPaymentCount int                                  `json:"plan_payment_count"`
	Conflicts        []*model.ManualReimbursementConflict `json:"conflicts"`
	Problems         []string                             `json:"problems"`
	// --send created the plan in this run
	Created bool `json:"created"`
}

// Runs the dry run, or with `send` the send, of the file's rows. With
// `allowPriorReimbursement`, a network's open manual reimbursements of other
// plans are listed on its row instead of refused.
func PayoutsReimburse(clientSession *session.ClientSession, csvData []byte, reason string, send bool, expect string, allowPriorReimbursement bool) (*PayoutReimbursementReport, error) {
	return payoutsReimburse(clientSession, csvData, reason, send, expect, allowPriorReimbursement, server.NowUtc)
}

func payoutsReimburse(clientSession *session.ClientSession, csvData []byte, reason string, send bool, expect string, allowPriorReimbursement bool, now func() time.Time) (*PayoutReimbursementReport, error) {
	ctx := clientSession.Ctx
	report := &PayoutReimbursementReport{
		RunId:                   server.NewId(),
		Time:                    now().UTC(),
		Mode:                    "dry_run",
		Reason:                  reason,
		AllowPriorReimbursement: allowPriorReimbursement,
		FeeNanoCents:            model.UsdToNanoCents(providerPaymentFeeUsd),
		Rows:                    []*PayoutReimbursementRow{},
		Chains:                  []*PayoutReimbursementChainTotal{},
		Conflicts:               []*model.ManualReimbursementConflict{},
		Problems:                []string{},
	}
	if send {
		report.Mode = "send"
	}

	usdcPayout, usdcErr := loadUsdcPayoutPolicy()
	switch {
	case usdcErr != nil:
		report.UsdcPayments = "unreadable: " + usdcErr.Error()
	case usdcPayout.PaymentsOn():
		report.UsdcPayments = server.UsdcPayoutOn
	default:
		report.UsdcPayments = server.UsdcPayoutOff
	}
	policy, policyErr := server.LoadProviderPayoutEarningPolicy(ctx)
	switch {
	case policyErr != nil:
		report.EarningPolicy = "unavailable: " + policyErr.Error()
	case policy == nil:
		report.EarningPolicy = "none"
	default:
		report.EarningPolicy = policy.ConfigSha256
	}

	if strings.TrimSpace(reason) == "" || payoutReimbursementMaxReasonLength < len(reason) {
		report.Problems = append(report.Problems, fmt.Sprintf("--reason must be set and at most %d bytes", payoutReimbursementMaxReasonLength))
	}
	inputs, problems := parsePayoutReimbursementCsv(csvData)
	report.Problems = append(report.Problems, problems...)
	if 0 < len(report.Problems) {
		return report, payoutReimbursementRefusal(report)
	}

	walletIds := []server.Id{}
	networkIds := []server.Id{}
	for _, input := range inputs {
		walletIds = append(walletIds, input.walletId)
		networkIds = append(networkIds, input.networkId)
	}
	wallets, networkNames := model.GetManualReimbursementCensus(ctx, walletIds, networkIds)
	for _, input := range inputs {
		row := &PayoutReimbursementRow{
			Line:      input.line,
			NetworkId: input.networkId,
			WalletId:  input.walletId,
			Amount:    formatPayoutReimbursementMicroUsdc(input.microUsdc),
		}
		report.Rows = append(report.Rows, row)
		networkName, ok := networkNames[input.networkId]
		if !ok {
			report.Problems = append(report.Problems, fmt.Sprintf("line %d: network %s does not exist", input.line, input.networkId))
		}
		row.NetworkName = networkName
		wallet := wallets[input.walletId]
		if wallet == nil {
			report.Problems = append(report.Problems, fmt.Sprintf("line %d: wallet %s does not exist", input.line, input.walletId))
			continue
		}
		row.WalletAddress, row.Blockchain = wallet.WalletAddress, wallet.Blockchain
		if wallet.NetworkId != input.networkId {
			report.Problems = append(report.Problems, fmt.Sprintf("line %d: wallet %s belongs to network %s, not %s", input.line, input.walletId, wallet.NetworkId, input.networkId))
		}
		if !wallet.Active {
			report.Problems = append(report.Problems, fmt.Sprintf("line %d: wallet %s is not active", input.line, input.walletId))
		}
		if model.ManualReimbursementAddressRefused(wallet.WalletAddress) {
			report.Problems = append(report.Problems, fmt.Sprintf("line %d: wallet %s is the refused address %s", input.line, input.walletId, model.ManualReimbursementRefusedAddress))
		}
		chain, err := formatBlockchain(wallet.Blockchain)
		if err != nil {
			report.Problems = append(report.Problems, fmt.Sprintf("line %d: wallet %s is on %q; only SOL and MATIC are paid", input.line, input.walletId, wallet.Blockchain))
		}
		row.Chain = chain
		row.AmountNanoCents, _, row.PayoutNanoCents, err = payoutReimbursementPayout(input.microUsdc)
		if err != nil {
			report.Problems = append(report.Problems, fmt.Sprintf("line %d: %s", input.line, err))
		}
		_, row.CircleAmount = payoutReimbursementCircleAmount(row.PayoutNanoCents)
	}
	if 0 < len(report.Problems) {
		return report, payoutReimbursementRefusal(report)
	}

	// The ids derive from the digest without --allow_prior_reimbursement, so the
	// run names its own plan before it reads the conflicts, which leave that
	// plan out.
	planDigest := payoutReimbursementDigest(reason, report.FeeNanoCents, report.Rows)
	report.PaymentPlanId = payoutReimbursementPlanId(planDigest)
	for _, row := range report.Rows {
		row.PaymentId = payoutReimbursementPaymentId(planDigest, row.NetworkId)
		report.AmountNanoCents += row.AmountNanoCents
		report.PayoutNanoCents += row.PayoutNanoCents
		index := slices.IndexFunc(report.Chains, func(total *PayoutReimbursementChainTotal) bool { return total.Chain == row.Chain })
		if index < 0 {
			report.Chains = append(report.Chains, &PayoutReimbursementChainTotal{Chain: row.Chain})
			index = len(report.Chains) - 1
		}
		report.Chains[index].Count += 1
		report.Chains[index].AmountNanoCents += row.AmountNanoCents
	}
	report.PlanPaymentCount = model.GetManualReimbursementPlanPaymentCount(ctx, report.PaymentPlanId)
	payoutReimbursementStates(clientSession, report)
	conflicts := model.GetManualReimbursementConflicts(ctx, networkIds, report.PaymentPlanId)
	if allowPriorReimbursement {
		payoutReimbursementPriors(clientSession, report, conflicts)
		report.Digest = payoutReimbursementAllowPriorDigest(reason, report.FeeNanoCents, report.Rows)
	} else {
		report.Digest = planDigest
		report.Conflicts = conflicts
		for _, conflict := range report.Conflicts {
			report.Problems = append(report.Problems, fmt.Sprintf("network %s already has manual reimbursement %s in plan %s", conflict.NetworkId, conflict.PaymentId, conflict.PaymentPlanId))
		}
	}
	if 0 < len(report.Problems) {
		return report, payoutReimbursementRefusal(report)
	}
	if !send {
		return report, nil
	}

	// refuse before any write
	if err := RequireUsdcPaymentsOn(); err != nil {
		return report, err
	}
	if policyErr != nil {
		return report, policyErr
	}
	if expect != report.Digest {
		return report, ErrPayoutReimbursementDigestMismatch
	}
	created, err := model.CreateManualReimbursementPlan(ctx, payoutReimbursementPlan(report), report.Time)
	if err != nil {
		return report, err
	}
	report.Created = created
	errs := []error{}
	for _, row := range report.Rows {
		if err := advancePayoutReimbursement(clientSession, row); err != nil {
			errs = append(errs, fmt.Errorf("network %s payment %s: %w", row.NetworkId, row.PaymentId, err))
		}
	}
	report.PlanPaymentCount = model.GetManualReimbursementPlanPaymentCount(ctx, report.PaymentPlanId)
	payoutReimbursementStates(clientSession, report)
	return report, errors.Join(errs...)
}

func payoutReimbursementRefusal(report *PayoutReimbursementReport) error {
	errs := []error{ErrPayoutReimbursementRefused}
	for _, problem := range report.Problems {
		errs = append(errs, errors.New(problem))
	}
	return errors.Join(errs...)
}

// Lists each open manual reimbursement of another plan on its network's row,
// with the amount and reason of its marker. A marker that does not read is a
// problem.
func payoutReimbursementPriors(clientSession *session.ClientSession, report *PayoutReimbursementReport, conflicts []*model.ManualReimbursementConflict) {
	networkIdRows := map[server.Id]*PayoutReimbursementRow{}
	for _, row := range report.Rows {
		networkIdRows[row.NetworkId] = row
	}
	for _, conflict := range conflicts {
		row := networkIdRows[conflict.NetworkId]
		prior := &PayoutReimbursementPrior{
			PaymentId:     conflict.PaymentId,
			PaymentPlanId: conflict.PaymentPlanId,
			Completed:     conflict.Completed,
		}
		row.PriorReimbursements = append(row.PriorReimbursements, prior)
		marker, err := model.GetManualReimbursementMarker(clientSession.Ctx, conflict.PaymentId)
		if err != nil || marker == nil {
			report.Problems = append(report.Problems, fmt.Sprintf("line %d: prior manual reimbursement %s of network %s has no readable marker", row.Line, conflict.PaymentId, conflict.NetworkId))
			continue
		}
		prior.AmountNanoCents, prior.Reason = marker.AmountNanoCents, marker.Reason
	}
}

// One payment and marker per row of a census without problems. Each marker
// carries the digest the send matched, and its network's prior reimbursements.
func payoutReimbursementPlan(report *PayoutReimbursementReport) *model.ManualReimbursementPlan {
	plan := &model.ManualReimbursementPlan{
		PaymentPlanId:           report.PaymentPlanId,
		Digest:                  report.Digest,
		Reason:                  report.Reason,
		AllowPriorReimbursement: report.AllowPriorReimbursement,
	}
	for _, row := range report.Rows {
		plan.Markers = append(plan.Markers, &model.ManualReimbursementMarker{
			Version:                 1,
			Digest:                  report.Digest,
			Reason:                  report.Reason,
			PaymentPlanId:           report.PaymentPlanId,
			PaymentId:               row.PaymentId,
			NetworkId:               row.NetworkId,
			WalletId:                row.WalletId,
			WalletAddress:           row.WalletAddress,
			Blockchain:              row.Blockchain,
			AmountNanoCents:         row.AmountNanoCents,
			FeeNanoCents:            report.FeeNanoCents,
			PayoutNanoCents:         row.PayoutNanoCents,
			AllowPriorReimbursement: report.AllowPriorReimbursement,
			PriorPaymentIds:         row.priorPaymentIds(),
		})
	}
	return plan
}

// The state of each row's payment, when it exists.
func payoutReimbursementStates(clientSession *session.ClientSession, report *PayoutReimbursementReport) {
	for _, row := range report.Rows {
		row.State, row.TransferId, row.TxHash = "", "", ""
		payment, err := model.GetPayment(clientSession.Ctx, row.PaymentId)
		server.Raise(err)
		if payment == nil {
			continue
		}
		switch {
		case payment.Completed:
			row.State = "complete"
		case payment.Canceled:
			row.State = "canceled"
		case payment.PaymentRecord != nil:
			row.State = "submitted"
		case payment.CircleIdempotencyKey != nil:
			row.State = "reserved"
		default:
			row.State = "pending"
		}
		if payment.PaymentRecord != nil {
			row.TransferId = *payment.PaymentRecord
		}
		if payment.TxHash != nil {
			row.TxHash = *payment.TxHash
		}
	}
}

// Advances one payment through the ordinary path. A completed payment is left
// alone; a canceled one is reported, never paid again by this plan. The pinned
// wallet is restored first in case a writer older than the pin re-pointed it.
// A payment holding an attempt is then followed to completion by the
// taskworker, as AdvancePaymentPost does for any payout.
func advancePayoutReimbursement(clientSession *session.ClientSession, row *PayoutReimbursementRow) error {
	ctx := clientSession.Ctx
	payment, err := model.GetPayment(ctx, row.PaymentId)
	if err != nil {
		return err
	}
	if payment == nil {
		return fmt.Errorf("the plan has no payment %s", row.PaymentId)
	}
	if payment.Completed {
		row.Outcome = "already complete"
		return nil
	}
	if payment.Canceled {
		row.Outcome = "canceled; this plan does not pay it again"
		return errors.New(row.Outcome)
	}
	if _, err := model.RepinManualReimbursementWallet(ctx, row.PaymentId); err != nil {
		return err
	}
	submitted := payment.PaymentRecord != nil
	args := &AdvancePaymentArgs{PaymentId: row.PaymentId}
	result, advanceErr := AdvancePayment(args, clientSession)
	if result == nil {
		result = &AdvancePaymentResult{}
	}
	payment, err = model.GetPayment(ctx, row.PaymentId)
	if err != nil {
		return errors.Join(advanceErr, err)
	}
	switch {
	case advanceErr != nil:
		row.Outcome = "error: " + advanceErr.Error()
	case result.Complete:
		row.Outcome = "complete"
	case result.Canceled:
		row.Outcome = "canceled by Circle"
	case result.PaymentsOff:
		row.Outcome = "held: " + result.HeldReason
	case result.Retryable:
		row.Outcome = "retry: " + result.HeldReason
	case payment != nil && payment.PaymentRecord != nil && !submitted:
		row.Outcome = "sent to Circle"
	case payment != nil && payment.PaymentRecord != nil:
		row.Outcome = "pending at Circle"
	default:
		row.Outcome = "pending"
	}
	if payment != nil && !payment.Completed && !payment.Canceled && (payment.PaymentRecord != nil || payment.CircleIdempotencyKey != nil) {
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleAdvancePayment(args, clientSession, tx)
		})
	}
	if advanceErr == nil && result.Canceled {
		return errors.New("Circle canceled the transfer")
	}
	return advanceErr
}

func (self *PayoutReimbursementReport) Text() string {
	b := &strings.Builder{}
	fmt.Fprintf(b, "payouts reimburse run %s at %s, mode %s\n", self.RunId, self.Time.Format(time.RFC3339), self.Mode)
	fmt.Fprintf(b, "reason %s\n", strconv.Quote(self.Reason))
	fmt.Fprintf(b, "usdc payments: %s (usdc_payout.yml)\n", self.UsdcPayments)
	fmt.Fprintf(b, "earning policy: %s (a manual reimbursement is admitted by its marker, not the earning census)\n", self.EarningPolicy)
	fmt.Fprintf(b, "fee: the sender deducts a fixed %s USDC (%d nano cents) from each payout before asking Circle for the rest,\n", formatPayoutReimbursementNanoCents(self.FeeNanoCents), self.FeeNanoCents)
	fmt.Fprintf(b, "  so each payout_nano_cents is the listed amount plus %d and Circle sends exactly the listed amount\n", self.FeeNanoCents)
	if self.AllowPriorReimbursement {
		priorCount := 0
		for _, row := range self.Rows {
			priorCount += len(row.PriorReimbursements)
		}
		fmt.Fprintf(b, "prior reimbursements: allowed (--allow_prior_reimbursement); the %d open manual reimbursement(s) of other plans listed below are not refused, and the digest binds exactly them\n", priorCount)
	}
	fmt.Fprintf(b, "rows: %d\n", len(self.Rows))
	for _, row := range self.Rows {
		chain := row.Chain
		if chain == "" {
			chain = strconv.Quote(row.Blockchain)
		}
		fmt.Fprintf(b, "  line %d network %s %s\n", row.Line, row.NetworkId, strconv.Quote(row.NetworkName))
		fmt.Fprintf(b, "    wallet %s %s on %s\n", row.WalletId, row.WalletAddress, chain)
		fmt.Fprintf(b, "    amount %s USDC, payout_nano_cents %d, circle amount %s\n", row.Amount, row.PayoutNanoCents, row.CircleAmount)
		if row.PaymentId != (server.Id{}) {
			fmt.Fprintf(b, "    payment %s", row.PaymentId)
			if row.State != "" {
				fmt.Fprintf(b, ", %s", row.State)
			}
			if row.TransferId != "" {
				fmt.Fprintf(b, ", circle transfer %s", row.TransferId)
			}
			if row.TxHash != "" {
				fmt.Fprintf(b, ", tx %s", row.TxHash)
			}
			fmt.Fprintf(b, "\n")
		}
		for _, prior := range row.PriorReimbursements {
			state := "not complete"
			if prior.Completed {
				state = "complete"
			}
			fmt.Fprintf(b, "    prior reimbursement %s in plan %s: %s USDC, %s, reason %s\n", prior.PaymentId, prior.PaymentPlanId,
				formatPayoutReimbursementNanoCents(prior.AmountNanoCents), state, strconv.Quote(prior.Reason))
		}
		if row.Outcome != "" {
			fmt.Fprintf(b, "    this run: %s\n", row.Outcome)
		}
	}
	for _, chain := range self.Chains {
		fmt.Fprintf(b, "  %-5s %d row(s), %s USDC\n", chain.Chain, chain.Count, formatPayoutReimbursementNanoCents(chain.AmountNanoCents))
	}
	fmt.Fprintf(b, "total: %s USDC to %d wallet(s); payouts %s USD with fees\n", formatPayoutReimbursementNanoCents(self.AmountNanoCents), len(self.Rows), formatPayoutReimbursementNanoCents(self.PayoutNanoCents))
	if self.Digest != "" {
		fmt.Fprintf(b, "plan %s (manual reimbursement), %d payment(s) created\n", self.PaymentPlanId, self.PlanPaymentCount)
		fmt.Fprintf(b, "digest %s\n", self.Digest)
	}
	if self.Mode == "send" && self.Digest != "" {
		fmt.Fprintf(b, "created in this run: %t\n", self.Created)
	}
	for _, problem := range self.Problems {
		fmt.Fprintf(b, "refused: %s\n", problem)
	}
	return b.String()
}
