// The operator's settlement of the unsettled escrows the contract tables'
// legacy partition keeps (CONTRACT-PARTITION-CUTOVER-NOW.md 6, decision 9:
// settle them before the legacy partition drops), `bringyourctl db
// contract-legacy-settle`.
//
// A terminal contract whose escrow rows still read settled = false is kept for
// good by the contract horizon reaper (its unsettled rule), and its grants by
// balance retention, so the legacy partition can never empty. Two retired
// paths left such contracts:
//   - until 2026-10-03 settlement committed the contract's outcome and then
//     wrote the escrows' settled flag, the provider sweeps and the payer's
//     debit as separate best-effort posts after the commit. A lost post was
//     never retried, and nothing repaired the flag;
//   - the force close quarantined a contract the ordinary rule could not
//     settle (not two reports, both checkpoints, a payer that is not an
//     endpoint, or reports beyond its escrow) by writing its outcome with no
//     payout and no flag.
//
// For each such contract this writes what its settlement would have written,
// with the ordinary settlement's arithmetic, in one transaction that takes the
// deadline close's locks in its order (contract, grant owners, grants, escrow
// rows):
//   - the used bytes of the reports (contractSettlementReportAmounts), less
//     what settled escrows already record;
//   - the grants in the funding order (deadlineEscrowRowsSql); a grant that no
//     longer exists, or is not the payer's, absorbs its share at no charge and
//     funds no sweep, as the deadline close's missing or invalid grant pays
//     nothing;
//   - the charge: a legacy grant's debit, or a debit journal for a
//     Redis-reserved grant, which the debit worker applies (as the deadline
//     close does);
//   - the billing split's sweeps (allocateContractParticipantPayouts);
//   - every unsettled escrow's settled flag with its payout bytes.
//
// The contract row is never written: its outcome and close time stay as they
// were claimed. A contract that already has sweeps had its sweep post run, and
// maybe its debit post, so it gets only the flags.
//
// A plan reads only. A settle plans again first and refuses unless that plan's
// token is the one the operator confirms from a dry run, then writes only the
// planned contracts, each only while its category under the locks is the
// planned one. A settled contract has no unsettled escrow, so a second run is a
// no-op; a contract that waits (a busy grant owner, a lock timeout) is counted
// and left for the next run.
package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// The categories of a candidate contract, in the order they are decided.
const (
	// the outcome is not claimed: the closers own it
	ContractLegacySettlementExcludedOpen = "excluded_open"
	// it closed less than MinCloseAge ago: its own settlement may still run
	ContractLegacySettlementExcludedRecent = "excluded_recent"
	// a debit journal row exists: the debit worker owns it and flags it
	ContractLegacySettlementExcludedDebitPending = "excluded_debit_pending"
	// no escrow row is unsettled any more
	ContractLegacySettlementAlreadySettled = "already_settled"
	// its sweeps exist: flags only, no charge and no sweep
	ContractLegacySettlementFlagSwept = "flag_swept"
	// the ordinary rule cannot settle it, as the force close's quarantine
	// found: flags with no payout, no charge and no sweep
	ContractLegacySettlementFlagQuarantine = "flag_quarantine"
	// no bytes used: flags with no payout
	ContractLegacySettlementFlagZero = "flag_zero"
	// every byte falls on grants that no longer exist: flags only
	ContractLegacySettlementFlagUnfunded = "flag_unfunded"
	// it would charge a grant that is still live: held for the owner
	// unless ChargeLiveGrants; its totals are what a settle would write
	ContractLegacySettlementHoldLiveGrant = "hold_live_grant"
	// charge, sweeps and flags
	ContractLegacySettlementSettle = "settle"
	// the contract row is gone
	ContractLegacySettlementMissing = "missing"
	// a settle only: the grant owner was busy or a lock wait ran out
	ContractLegacySettlementBusy = "busy"
	// a settle only: the category under the locks is not the planned one
	ContractLegacySettlementChanged = "changed"
	// a settle only: the transaction failed
	ContractLegacySettlementFailed = "failed"
)

// The categories that write, and the order a report prints every category in.
var contractLegacySettlementWriteCategories = []string{
	ContractLegacySettlementFlagSwept,
	ContractLegacySettlementFlagQuarantine,
	ContractLegacySettlementFlagZero,
	ContractLegacySettlementFlagUnfunded,
	ContractLegacySettlementSettle,
}

var ContractLegacySettlementCategories = []string{
	ContractLegacySettlementSettle,
	ContractLegacySettlementFlagZero,
	ContractLegacySettlementFlagQuarantine,
	ContractLegacySettlementFlagUnfunded,
	ContractLegacySettlementFlagSwept,
	ContractLegacySettlementHoldLiveGrant,
	ContractLegacySettlementExcludedOpen,
	ContractLegacySettlementExcludedRecent,
	ContractLegacySettlementExcludedDebitPending,
	ContractLegacySettlementAlreadySettled,
	ContractLegacySettlementMissing,
	ContractLegacySettlementBusy,
	ContractLegacySettlementChanged,
	ContractLegacySettlementFailed,
}

// The bounds and pace of a plan or a settle.
type ContractLegacySettlementSettings struct {
	// Only contracts that closed at least this long ago: a younger close may
	// still have its own settlement in flight.
	MinCloseAge time.Duration
	// Contract ids at or above this are out of scope. Nil reads the upper
	// bound of transfer_contract's legacy partition, and no bound while the
	// contract tables are not partitioned (every contract is then legacy).
	MaxContractId *server.Id
	// Unsettled escrow rows read per page of the discovery, a walk of the
	// unsettled escrow index.
	DiscoveryPageSize int
	// Contracts read per read only transaction of a plan.
	PlanBatchSize int
	// Pause between a plan's transactions.
	PlanBatchPause time.Duration
	// Contracts a settle writes per second at most, one transaction each.
	// Must be positive.
	MaxContractsPerSecond float64
	// Bounds each statement of a transaction.
	StatementTimeout time.Duration
	// Bounds each lock wait of a settle's transaction.
	LockTimeout time.Duration
	// Settles a contract whose charge falls on a grant that is still live.
	// The posts of the retired settlement may have debited it already, so this
	// is the owner's choice; such a contract is held otherwise.
	ChargeLiveGrants bool
	// The most contracts a settle writes; non-positive writes every planned
	// one.
	MaxContracts int
}

func DefaultContractLegacySettlementSettings() *ContractLegacySettlementSettings {
	return &ContractLegacySettlementSettings{
		MinCloseAge:           48 * time.Hour,
		DiscoveryPageSize:     5000,
		PlanBatchSize:         200,
		PlanBatchPause:        100 * time.Millisecond,
		MaxContractsPerSecond: 50,
		StatementTimeout:      10 * time.Second,
		LockTimeout:           time.Second,
	}
}

// Refuses settings that would not bound the run.
func (self *ContractLegacySettlementSettings) Validate() error {
	if self.MinCloseAge < 0 {
		return fmt.Errorf("the close age %s is negative", self.MinCloseAge)
	}
	if self.DiscoveryPageSize < 1 || self.PlanBatchSize < 1 {
		return fmt.Errorf("the discovery page %d and plan batch %d must be positive", self.DiscoveryPageSize, self.PlanBatchSize)
	}
	if !(0 < self.MaxContractsPerSecond) {
		return fmt.Errorf("the settle rate %v must be positive", self.MaxContractsPerSecond)
	}
	if self.StatementTimeout <= 0 || self.LockTimeout <= 0 {
		return fmt.Errorf("the statement bound %s and lock wait %s must be positive", self.StatementTimeout, self.LockTimeout)
	}
	return nil
}

// One category's contracts and amounts.
type ContractLegacySettlementTotals struct {
	Contracts  int64
	EscrowRows int64
	// the unsettled escrow rows' reserved bytes
	EscrowByteCount ByteCount
	// bytes charged to existing grants
	ChargeByteCount ByteCount
	// payout bytes and provider revenue of the sweeps written
	SweepByteCount ByteCount
	SweepNanoCents NanoCents
	// distinct networks paid by a sweep, and charged
	ProviderNetworks int64
	PayerNetworks    int64

	providerNetworkIds map[server.Id]bool
	payerNetworkIds    map[server.Id]bool
}

func (self *ContractLegacySettlementTotals) add(decision *contractLegacySettlementDecision) {
	if self.providerNetworkIds == nil {
		self.providerNetworkIds = map[server.Id]bool{}
		self.payerNetworkIds = map[server.Id]bool{}
	}
	self.Contracts += 1
	self.EscrowRows += int64(len(decision.unsettledEscrows))
	for _, escrow := range decision.unsettledEscrows {
		self.EscrowByteCount += escrow.amount
	}
	for _, charge := range decision.charges {
		self.ChargeByteCount += charge.byteCount
	}
	if 0 < len(decision.charges) && decision.payerNetworkId != nil {
		self.payerNetworkIds[*decision.payerNetworkId] = true
	}
	for key, payout := range decision.sweeps {
		self.SweepByteCount += payout.payoutByteCount
		self.SweepNanoCents += payout.payout
		self.providerNetworkIds[key.networkId] = true
	}
	self.ProviderNetworks = int64(len(self.providerNetworkIds))
	self.PayerNetworks = int64(len(self.payerNetworkIds))
}

// A plan or a settle: the totals of each category.
type ContractLegacySettlementReport struct {
	// contract ids at or above it were out of scope; nil without a bound
	MaxContractId *server.Id
	// candidates closed before this were eligible
	CloseBefore time.Time
	Categories  map[string]*ContractLegacySettlementTotals
	// a plan's confirmation: a digest of its scope, its settings and every
	// category's totals
	Token string

	// a plan's candidates, in id order, with their categories
	contractIds        []server.Id
	contractCategories map[server.Id]string
}

func newContractLegacySettlementReport(maxContractId *server.Id, closeBefore time.Time) *ContractLegacySettlementReport {
	report := &ContractLegacySettlementReport{
		MaxContractId:      maxContractId,
		CloseBefore:        closeBefore,
		Categories:         map[string]*ContractLegacySettlementTotals{},
		contractCategories: map[server.Id]string{},
	}
	for _, category := range ContractLegacySettlementCategories {
		report.Categories[category] = &ContractLegacySettlementTotals{}
	}
	return report
}

func (self *ContractLegacySettlementReport) add(contractId server.Id, decision *contractLegacySettlementDecision) {
	self.Categories[decision.category].add(decision)
	self.contractIds = append(self.contractIds, contractId)
	self.contractCategories[contractId] = decision.category
}

// The contracts planned to write.
func (self *ContractLegacySettlementReport) WriteContracts() int64 {
	var count int64
	for _, category := range contractLegacySettlementWriteCategories {
		count += self.Categories[category].Contracts
	}
	return count
}

// The digest of the scope, the settings that decide categories, and every
// category's totals: equal only when a settle would find what the dry run did.
func contractLegacySettlementToken(report *ContractLegacySettlementReport, settings *ContractLegacySettlementSettings) string {
	var builder strings.Builder
	builder.WriteString("contract-legacy-settle v1\n")
	if report.MaxContractId == nil {
		builder.WriteString("bound none\n")
	} else {
		fmt.Fprintf(&builder, "bound %s\n", report.MaxContractId.String())
	}
	fmt.Fprintf(&builder, "min_close_age %d charge_live_grants %t\n", settings.MinCloseAge.Nanoseconds(), settings.ChargeLiveGrants)
	for _, category := range ContractLegacySettlementCategories {
		totals := report.Categories[category]
		fmt.Fprintf(&builder, "%s %d %d %d %d %d %d %d %d\n", category, totals.Contracts, totals.EscrowRows,
			totals.EscrowByteCount, totals.ChargeByteCount, totals.SweepByteCount, totals.SweepNanoCents,
			totals.ProviderNetworks, totals.PayerNetworks)
	}
	sum := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(sum[:8])
}

// A charge of one existing grant.
type contractLegacySettlementCharge struct {
	balanceId     server.Id
	byteCount     ByteCount
	redisReserved bool
}

// One contract's reads, under the settle's locks or in a plan's snapshot.
type contractLegacySettlementInput struct {
	found     bool
	outcome   *ContractOutcome
	closeTime *time.Time
	reports   []contractSettlementReport
	// in the funding order
	escrows        []contractDeadlineEscrow
	grantEndTimes  map[server.Id]time.Time
	sweepCount     int64
	journalPending bool
	participants   []ContractParticipant
	// the payer network the billing split excludes
	originNetworkId server.Id
	participantsErr error
	payerNetworkId  *server.Id
}

// One contract's category and the writes it implies.
type contractLegacySettlementDecision struct {
	category         string
	unsettledEscrows []contractDeadlineEscrow
	// every unsettled escrow's payout bytes, for its flag
	payouts        map[server.Id]sweepPayout
	charges        []contractLegacySettlementCharge
	sweeps         map[participantSweepKey]*participantSweepPayout
	payerNetworkId *server.Id
}

// Decides a contract's category and writes from its reads (see the file
// comment). Pure.
func decideContractLegacySettlement(input *contractLegacySettlementInput, settings *ContractLegacySettlementSettings, now time.Time) *contractLegacySettlementDecision {
	decision := &contractLegacySettlementDecision{
		payouts:        map[server.Id]sweepPayout{},
		payerNetworkId: input.payerNetworkId,
	}
	if !input.found {
		decision.category = ContractLegacySettlementMissing
		return decision
	}
	var priorPayoutByteCount ByteCount
	for _, escrow := range input.escrows {
		if escrow.settled {
			if escrow.priorPayout != nil {
				priorPayoutByteCount += max(0, *escrow.priorPayout)
			}
		} else {
			decision.unsettledEscrows = append(decision.unsettledEscrows, escrow)
		}
	}
	// every unsettled escrow is flagged; the categories that pay say how much
	flagOnly := func(category string) *contractLegacySettlementDecision {
		decision.category = category
		for _, escrow := range decision.unsettledEscrows {
			decision.payouts[escrow.balanceId] = sweepPayout{escrowBalanceByteCount: escrow.amount, returnByteCount: max(0, escrow.amount)}
		}
		return decision
	}
	switch {
	case input.outcome == nil:
		decision.category = ContractLegacySettlementExcludedOpen
		return decision
	case input.closeTime == nil || !input.closeTime.Before(now.Add(-settings.MinCloseAge)):
		decision.category = ContractLegacySettlementExcludedRecent
		return decision
	case input.journalPending:
		decision.category = ContractLegacySettlementExcludedDebitPending
		return decision
	case len(decision.unsettledEscrows) == 0:
		decision.category = ContractLegacySettlementAlreadySettled
		return decision
	case 0 < input.sweepCount:
		return flagOnly(ContractLegacySettlementFlagSwept)
	case input.participantsErr != nil:
		return flagOnly(ContractLegacySettlementFlagQuarantine)
	}

	used, _, err := contractSettlementReportAmounts(input.reports, *input.outcome)
	if err != nil {
		return flagOnly(ContractLegacySettlementFlagQuarantine)
	}
	used -= min(used, priorPayoutByteCount)
	if used == 0 {
		return flagOnly(ContractLegacySettlementFlagZero)
	}
	// the ordinary arithmetic over the unsettled escrows in the funding order;
	// a missing grant values its share at nothing
	settlementEscrows := make([]contractSettlementEscrow, 0, len(decision.unsettledEscrows))
	for _, escrow := range decision.unsettledEscrows {
		settlementEscrow := escrow.contractSettlementEscrow
		if escrow.networkId == nil || *escrow.networkId != input.originNetworkId || settlementEscrow.start <= 0 || settlementEscrow.revenue < 0 {
			settlementEscrow.start = 1
			settlementEscrow.revenue = 0
		}
		settlementEscrows = append(settlementEscrows, settlementEscrow)
	}
	payouts, err := contractSettlementSweepPayouts(used, settlementEscrows)
	if err != nil {
		return flagOnly(ContractLegacySettlementFlagQuarantine)
	}
	// a flag records what this charges: a missing grant's share is
	// absorbed, not charged
	fundedPayouts := map[server.Id]sweepPayout{}
	live := false
	for _, escrow := range decision.unsettledEscrows {
		payout := payouts[escrow.balanceId]
		// as the deadline close, only an existing grant of the payer funds
		if payout.payoutByteCount <= 0 || escrow.networkId == nil || *escrow.networkId != input.originNetworkId {
			decision.payouts[escrow.balanceId] = sweepPayout{escrowBalanceByteCount: escrow.amount, returnByteCount: max(0, escrow.amount)}
			continue
		}
		decision.payouts[escrow.balanceId] = payout
		fundedPayouts[escrow.balanceId] = payout
		decision.charges = append(decision.charges, contractLegacySettlementCharge{
			balanceId:     escrow.balanceId,
			byteCount:     payout.payoutByteCount,
			redisReserved: escrow.redisReserved,
		})
		if endTime, ok := input.grantEndTimes[escrow.balanceId]; ok && now.Before(endTime) {
			live = true
		}
	}
	switch {
	case len(decision.charges) == 0:
		decision.category = ContractLegacySettlementFlagUnfunded
	case live && !settings.ChargeLiveGrants:
		// the charges and sweeps a settle would write, for the report; a held
		// contract is never written
		decision.category = ContractLegacySettlementHoldLiveGrant
		decision.payouts = map[server.Id]sweepPayout{}
		decision.sweeps = allocateContractParticipantPayouts(input.participants, input.originNetworkId, fundedPayouts)
	default:
		decision.category = ContractLegacySettlementSettle
		decision.sweeps = allocateContractParticipantPayouts(input.participants, input.originNetworkId, fundedPayouts)
	}
	return decision
}

// The upper bound of transfer_contract's legacy partition.
var contractLegacyBoundPattern = regexp.MustCompile(`TO \('([0-9a-f-]{36})'\)`)

// The legacy partition's upper bound, or nil while the contract tables are not
// partitioned.
func readContractLegacyUpperBound(ctx context.Context) (bound *server.Id, returnErr error) {
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		var expressions []string
		rows, err := conn.Query(ctx, `
			SELECT pg_get_expr(child.relpartbound, child.oid)
			FROM pg_inherits
			INNER JOIN pg_class AS child ON child.oid = pg_inherits.inhrelid
			INNER JOIN pg_class AS parent ON parent.oid = pg_inherits.inhparent
			WHERE parent.relname = 'transfer_contract' AND child.relname = $1
		`, server.ContractLegacyPartitionName("transfer_contract"))
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var expression string
				server.Raise(rows.Scan(&expression))
				expressions = append(expressions, expression)
			}
		})
		if len(expressions) == 0 {
			return
		}
		match := contractLegacyBoundPattern.FindStringSubmatch(expressions[0])
		if match == nil {
			returnErr = fmt.Errorf("the legacy partition's bound %q has no upper id", expressions[0])
			return
		}
		id, err := server.ParseId(match[1])
		if err != nil {
			returnErr = err
			return
		}
		bound = &id
	}, server.OptNoRetry())
	return
}

// The unsettled escrow rows' contracts below the bound, in id order: a keyset
// walk of the unsettled escrow index (balance_id, contract_id) WHERE NOT
// settled, a page per statement.
func discoverContractLegacySettlementCandidates(ctx context.Context, settings *ContractLegacySettlementSettings, bound *server.Id) ([]server.Id, error) {
	contractIds := map[server.Id]bool{}
	var cursorBalanceId, cursorContractId server.Id
	upper := server.Id{}
	for index := range upper {
		upper[index] = 0xff
	}
	if bound != nil {
		upper = *bound
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pageCount := 0
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			pageCount = 0
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`SET LOCAL statement_timeout = %d`, settings.StatementTimeout.Milliseconds())))
			rows, err := tx.Query(ctx, `
				SELECT balance_id, contract_id
				FROM transfer_escrow
				WHERE settled = false AND (balance_id, contract_id) > ($1, $2) AND contract_id < $3
				ORDER BY balance_id, contract_id
				LIMIT $4
			`, cursorBalanceId, cursorContractId, upper, settings.DiscoveryPageSize)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					server.Raise(rows.Scan(&cursorBalanceId, &cursorContractId))
					contractIds[cursorContractId] = true
					pageCount += 1
				}
			})
		}, server.TxReadCommitted, pgx.ReadOnly, server.OptNoRetry())
		if pageCount < settings.DiscoveryPageSize {
			break
		}
	}
	ordered := make([]server.Id, 0, len(contractIds))
	for contractId := range contractIds {
		ordered = append(ordered, contractId)
	}
	slices.SortFunc(ordered, server.Id.Cmp)
	return ordered, nil
}

// Reads one contract for its decision. With lock, the reads take the deadline
// close's locks in its order, and an owner refusal raises
// errTransferBalanceOwnershipBusy.
func readContractLegacySettlementInTx(ctx context.Context, tx server.PgTx, contractId server.Id, lock bool) *contractLegacySettlementInput {
	input := &contractLegacySettlementInput{grantEndTimes: map[server.Id]time.Time{}}
	lockSql := ""
	if lock {
		lockSql = " FOR UPDATE"
	}
	owner := &contractSettlementOwner{}
	rows, err := tx.Query(ctx, `SELECT source_network_id,source_id,destination_network_id,destination_id,
		payer_network_id,companion_contract_id,stream_id,outcome,close_time
		FROM transfer_contract WHERE contract_id=$1`+lockSql, contractId)
	server.WithPgResult(rows, err, func() {
		if rows.Next() {
			input.found = true
			server.Raise(rows.Scan(&owner.participants.sourceNetworkId, &owner.participants.sourceId,
				&owner.participants.destinationNetworkId, &owner.participants.destinationId,
				&owner.participants.payerNetworkId, &owner.participants.companionContractId, &owner.participants.streamId,
				&input.outcome, &input.closeTime))
		}
	})
	if !input.found {
		return input
	}
	rows, err = tx.Query(ctx, `SELECT party,used_transfer_byte_count,checkpoint FROM contract_close WHERE contract_id=$1 ORDER BY party`, contractId)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var report contractSettlementReport
			server.Raise(rows.Scan(&report.party, &report.byteCount, &report.checkpoint))
			input.reports = append(input.reports, report)
		}
	})
	if resolved, err := resolveContractSettlementOwnerInTx(ctx, tx, contractId, owner); err == nil {
		owner = resolved
	} else {
		input.participantsErr = err
	}
	input.payerNetworkId = owner.participants.payerNetworkId
	if input.participantsErr == nil {
		input.participants, input.originNetworkId, input.participantsErr = contractParticipantsFromOwnerInTx(ctx, tx, contractId, owner.participants, nil)
	}

	balanceIds, err := contractTransferBalanceIdsInTx(ctx, tx, []server.Id{contractId})
	server.Raise(err)
	if lock && 0 < len(balanceIds) {
		admitted, err := server.TryTxOwnership(ctx, tx, deadlineFinancialOwnershipKeys(balanceIds))
		server.Raise(err)
		if !admitted {
			server.Raise(errTransferBalanceOwnershipBusy)
		}
		rows, err = tx.Query(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=ANY($1) ORDER BY balance_id FOR UPDATE`, balanceIds)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
			}
		})
		lockSettlementReservations(ctx, tx, contractId, balanceIds)
	}
	rows, err = tx.Query(ctx, `SELECT balance_id,end_time FROM transfer_balance WHERE balance_id=ANY($1)`, balanceIds)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var balanceId server.Id
			var endTime time.Time
			server.Raise(rows.Scan(&balanceId, &endTime))
			input.grantEndTimes[balanceId] = endTime
		}
	})
	server.RaisePgResult(tx.Exec(ctx, deadlineEscrowRowsJitSql))
	rows, err = tx.Query(ctx, deadlineEscrowRowsSql, contractId)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var escrow contractDeadlineEscrow
			server.Raise(rows.Scan(&escrow.balanceId, &escrow.amount, &escrow.start, &escrow.revenue, &escrow.networkId,
				&escrow.available, &escrow.settled, &escrow.priorPayout, &escrow.redisReserved, &escrow.journal))
			input.escrows = append(input.escrows, escrow)
		}
	})
	if deadlineEscrowRowLimit < len(input.escrows) {
		server.Raise(fmt.Errorf("contract %s has more escrow rows than the deadline close reads", contractId))
	}
	server.Raise(tx.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM transfer_escrow_sweep WHERE contract_id=$1),
		EXISTS (SELECT 1 FROM transfer_debit_journal WHERE contract_id=$1)`, contractId).Scan(&input.sweepCount, &input.journalPending))
	return input
}

// Writes a decision's charges, sweeps and flags in the caller's transaction,
// and returns the posts to run after its commit.
func writeContractLegacySettlementInTx(ctx context.Context, tx server.PgTx, contractId server.Id, decision *contractLegacySettlementDecision, settleTime time.Time) []server.PostFunction {
	var posts []server.PostFunction
	journaledBalanceIds := []server.Id{}
	for _, charge := range decision.charges {
		if charge.redisReserved {
			// the debit worker applies it to the grant, as for a deadline
			// close; the unique journal key makes it once
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_debit_journal
				(contract_id,balance_id,debit_byte_count,shard) VALUES($1,$2,$3,$4)`,
				contractId, charge.balanceId, charge.byteCount, transferDebitShard(charge.balanceId)))
			journaledBalanceIds = append(journaledBalanceIds, charge.balanceId)
		} else {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance
				SET balance_byte_count=balance_byte_count-$2 WHERE balance_id=$1`, charge.balanceId, charge.byteCount))
		}
	}
	server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
		for key, payout := range decision.sweeps {
			batch.Queue(participantSweepInsertSQL, contractId, key.balanceId, key.networkId,
				payout.payoutByteCount, payout.payout, payout.providerPayouts)
		}
		queueEscrowSettlementUpdates(batch, contractId, settleTime, decision.payouts)
	})
	if 0 < len(journaledBalanceIds) {
		posts = append(posts, transferDebitWakePost(ctx, journaledBalanceIds))
	}
	return posts
}

// Reads the planned scope: the bound, the candidates and each one's decision,
// in read only transactions.
func PlanContractLegacySettlement(ctx context.Context, settings *ContractLegacySettlementSettings, logf func(string, ...any)) (*ContractLegacySettlementReport, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	bound := settings.MaxContractId
	if bound == nil {
		var err error
		if bound, err = readContractLegacyUpperBound(ctx); err != nil {
			return nil, err
		}
	}
	now := server.NowUtc()
	report := newContractLegacySettlementReport(bound, now.Add(-settings.MinCloseAge))
	contractIds, err := discoverContractLegacySettlementCandidates(ctx, settings, bound)
	if err != nil {
		return nil, err
	}
	logf("%d candidate contracts with an unsettled escrow", len(contractIds))
	for start := 0; start < len(contractIds); start += settings.PlanBatchSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		batch := contractIds[start:min(start+settings.PlanBatchSize, len(contractIds))]
		decisions := make([]*contractLegacySettlementDecision, len(batch))
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`SET LOCAL statement_timeout = %d`, settings.StatementTimeout.Milliseconds())))
			for index, contractId := range batch {
				decisions[index] = decideContractLegacySettlement(readContractLegacySettlementInTx(ctx, tx, contractId, false), settings, now)
			}
		}, server.TxReadCommitted, pgx.ReadOnly, server.OptNoRetry())
		for index, contractId := range batch {
			report.add(contractId, decisions[index])
		}
		if 0 < settings.PlanBatchPause {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(settings.PlanBatchPause):
			}
		}
	}
	report.Token = contractLegacySettlementToken(report, settings)
	return report, nil
}

// Plans, refuses unless the plan's token is expectToken, then writes each
// planned contract of a writing category in its own transaction, at most
// MaxContractsPerSecond. Returns the plan and what the settle wrote.
func SettleContractLegacyEscrows(
	ctx context.Context,
	settings *ContractLegacySettlementSettings,
	expectToken string,
	logf func(string, ...any),
) (plan *ContractLegacySettlementReport, settled *ContractLegacySettlementReport, returnErr error) {
	plan, returnErr = PlanContractLegacySettlement(ctx, settings, logf)
	if returnErr != nil {
		return
	}
	if expectToken == "" || plan.Token != expectToken {
		returnErr = fmt.Errorf("the plan's token %s is not the confirmed %q: the candidates or settings changed since that dry run, so run the dry run again", plan.Token, expectToken)
		return
	}
	settled = newContractLegacySettlementReport(plan.MaxContractId, plan.CloseBefore)
	interval := time.Duration(float64(time.Second) / settings.MaxContractsPerSecond)
	written := 0
	for _, contractId := range plan.contractIds {
		plannedCategory := plan.contractCategories[contractId]
		if !slices.Contains(contractLegacySettlementWriteCategories, plannedCategory) {
			continue
		}
		if 0 < settings.MaxContracts && settings.MaxContracts <= written {
			break
		}
		if err := ctx.Err(); err != nil {
			returnErr = err
			return
		}
		decision := settleContractLegacyContract(ctx, contractId, plannedCategory, settings)
		settled.add(contractId, decision)
		if decision.category == ContractLegacySettlementFailed {
			logf("contract %s failed to settle", contractId)
		}
		written += 1
		if written%1000 == 0 {
			logf("%d of %d planned contracts processed", written, plan.WriteContracts())
		}
		select {
		case <-ctx.Done():
			returnErr = ctx.Err()
			return
		case <-time.After(interval):
		}
	}
	return
}

// One contract's transaction: the locked reads, the decision, and its writes
// only while the decision is the planned one.
func settleContractLegacyContract(ctx context.Context, contractId server.Id, plannedCategory string, settings *ContractLegacySettlementSettings) (decision *contractLegacySettlementDecision) {
	var posts []server.PostFunction
	err := server.HandleError(func() {
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			posts = nil
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`SET LOCAL statement_timeout = %d`, settings.StatementTimeout.Milliseconds())))
			now := server.NowUtc()
			decision = decideContractLegacySettlement(readContractLegacySettlementInTx(ctx, tx, contractId, true), settings, now)
			if decision.category != plannedCategory {
				if decision.category != ContractLegacySettlementAlreadySettled {
					decision = &contractLegacySettlementDecision{category: ContractLegacySettlementChanged}
				}
				return
			}
			posts = writeContractLegacySettlementInTx(ctx, tx, contractId, decision, now)
		}, server.TxReadCommitted, server.TxLockTimeout(settings.LockTimeout), server.OptNoRetry())
	})
	if err != nil {
		category := ContractLegacySettlementFailed
		if cause, ok := err.(error); ok && (errors.Is(cause, errTransferBalanceOwnershipBusy) || server.IsLockTimeout(cause)) {
			category = ContractLegacySettlementBusy
		}
		return &contractLegacySettlementDecision{category: category}
	}
	server.RunPosts(ctx, posts...)
	return
}
