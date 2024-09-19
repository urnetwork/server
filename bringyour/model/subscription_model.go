package model

import (
	"context"
	"fmt"
	"math"
	"time"

	// "crypto/rand"
	// "encoding/hex"
	// "slices"
	"errors"
	"strconv"
	"strings"

	// "golang.org/x/exp/maps"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/session"
)

type ByteCount = int64

const Kib = ByteCount(1024)
const Mib = ByteCount(1024 * 1024)
const Gib = ByteCount(1024 * 1024 * 1024)

func ByteCountHumanReadable(count ByteCount) string {
	trimFloatString := func(value float64, precision int, suffix string) string {
		s := fmt.Sprintf("%."+strconv.Itoa(precision)+"f", value)
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
		return s + suffix
	}

	if 1024*1024*1024*1024 <= count {
		return trimFloatString(
			float64(100*count/(1024*1024*1024*1024))/100.0,
			2,
			"TiB",
		)
	} else if 1024*1024*1024 <= count {
		return trimFloatString(
			float64(100*count/(1024*1024*1024))/100.0,
			2,
			"GiB",
		)
	} else if 1024*1024 <= count {
		return trimFloatString(
			float64(100*count/(1024*1024))/100.0,
			2,
			"MiB",
		)
	} else if 1024 <= count {
		return trimFloatString(
			float64(100*count/(1024))/100.0,
			2,
			"KiB",
		)
	} else {
		return fmt.Sprintf("%dB", count)
	}
}

func ParseByteCount(humanReadable string) (ByteCount, error) {
	humanReadableLower := strings.ToLower(humanReadable)
	tibLower := "tib"
	gibLower := "gib"
	mibLower := "mib"
	if strings.HasSuffix(humanReadableLower, tibLower) {
		countFloat, err := strconv.ParseFloat(
			humanReadableLower[0:len(humanReadableLower)-len(tibLower)],
			64,
		)
		if err != nil {
			return ByteCount(0), err
		}
		return ByteCount(countFloat * 1024 * 1024 * 1024 * 1024), nil
	} else if strings.HasSuffix(humanReadableLower, gibLower) {
		countFloat, err := strconv.ParseFloat(
			humanReadableLower[0:len(humanReadableLower)-len(gibLower)],
			64,
		)
		if err != nil {
			return ByteCount(0), err
		}
		return ByteCount(countFloat * 1024 * 1024 * 1024), nil
	} else if strings.HasSuffix(humanReadableLower, mibLower) {
		countFloat, err := strconv.ParseFloat(
			humanReadableLower[0:len(humanReadableLower)-len(mibLower)],
			64,
		)
		if err != nil {
			return ByteCount(0), err
		}
		return ByteCount(countFloat * 1024 * 1024), nil
	} else {
		countInt, err := strconv.ParseInt(humanReadableLower, 10, 63)
		if err != nil {
			return ByteCount(0), err
		}
		return ByteCount(countInt), nil
	}
}

type NanoCents = int64

func UsdToNanoCents(usd float64) NanoCents {
	return NanoCents(math.Round(usd * float64(1000000000)))
}

func NanoCentsToUsd(nanoCents NanoCents) float64 {
	return float64(nanoCents) / float64(1000000000)
}

// 12 months
const BalanceCodeDuration = 365 * 24 * time.Hour

// up to 4MiB
const AcceptableTransfersByteDifference = 4 * 1024 * 1024

var MinWalletPayoutThreshold = UsdToNanoCents(1.00)

// hold onto unpaid amounts for up to this time
const WalletPayoutTimeout = 15 * 24 * time.Hour

const ProviderRevenueShare float64 = 0.5

const MaxSubscriptionPaymentIdsPerHour = 5

type TransferPair struct {
	A bringyour.Id
	B bringyour.Id
}

func NewTransferPair(sourceId bringyour.Id, destinationId bringyour.Id) TransferPair {
	return TransferPair{
		A: sourceId,
		B: destinationId,
	}
}

func NewUnorderedTransferPair(a bringyour.Id, b bringyour.Id) TransferPair {
	// store in ascending order
	if a.Less(b) {
		return TransferPair{
			A: a,
			B: b,
		}
	} else {
		return TransferPair{
			A: b,
			B: a,
		}
	}
}

type BalanceCode struct {
	BalanceCodeId    bringyour.Id
	StartTime        time.Time
	CreateTime       time.Time
	EndTime          time.Time
	BalanceByteCount ByteCount
	NetRevenue       NanoCents
	Secret           string
	PurchaseEventId  string
	PurchaseRecord   string
	PurchaseEmail    string
}

func GetBalanceCodeIdForPurchaseEventId(ctx context.Context, purchaseEventId string) (balanceCodeId bringyour.Id, returnErr error) {
	bringyour.Db(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT balance_code_id FROM transfer_balance_code
                WHERE purchase_event_id = $1
            `,
			purchaseEventId,
		)
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				bringyour.Raise(result.Scan(&balanceCodeId))
			} else {
				returnErr = fmt.Errorf("Purchase event not found.")
			}
		})
	})
	return
}

func GetBalanceCode(
	ctx context.Context,
	balanceCodeId bringyour.Id,
) (balanceCode *BalanceCode, returnErr error) {
	bringyour.Db(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    create_time,
                    start_time,
                    end_time,
                    balance_byte_count,
                    net_revenue_nano_cents,
                    balance_code_secret,
                    purchase_event_id,
                    purchase_record,
                    purchase_email
                FROM transfer_balance_code
                WHERE balance_code_id = $1
            `,
			balanceCodeId,
		)
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				balanceCode = &BalanceCode{
					BalanceCodeId: balanceCodeId,
				}
				bringyour.Raise(result.Scan(
					&balanceCode.CreateTime,
					&balanceCode.StartTime,
					&balanceCode.EndTime,
					&balanceCode.BalanceByteCount,
					&balanceCode.NetRevenue,
					&balanceCode.Secret,
					&balanceCode.PurchaseEventId,
					&balanceCode.PurchaseRecord,
					&balanceCode.PurchaseEmail,
				))
			} else {
				returnErr = fmt.Errorf("Balance code not found.")
			}
		})
	})
	return
}

// typically called from a payment webhook
func CreateBalanceCode(
	ctx context.Context,
	balanceByteCount ByteCount,
	netRevenue NanoCents,
	purchaseEventId string,
	purchaseRecord string,
	purchaseEmail string,
) (balanceCode *BalanceCode, returnErr error) {
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		purchaseEventIdExists := false

		result, err := tx.Query(
			ctx,
			`
                SELECT balance_code_id FROM transfer_balance_code
                WHERE purchase_event_id = $1
            `,
			purchaseEventId,
		)
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				purchaseEventIdExists = true
			}
		})

		if purchaseEventIdExists {
			// the `purchaseEventId` was already converted into a balance code
			returnErr = fmt.Errorf("Purchase event already exists.")
			return
		}

		balanceCodeId := bringyour.NewId()

		secret, err := newCode()
		if err != nil {
			returnErr = err
			return
		}

		createTime := bringyour.NowUtc()
		// round down to 00:00 the day of create time
		startTime := time.Date(
			createTime.Year(),
			createTime.Month(),
			createTime.Day(),
			0, 0, 0, 0,
			createTime.Location(),
		)
		// round up to 00:00 the next day of create time + duration
		endTime := startTime.Add(24 * time.Hour).Add(BalanceCodeDuration)

		bringyour.RaisePgResult(tx.Exec(
			ctx,
			`
                INSERT INTO transfer_balance_code (
                    balance_code_id,
                    create_time,
                    start_time,
                    end_time,
                    balance_byte_count,
                    net_revenue_nano_cents,
                    balance_code_secret,
                    purchase_event_id,
                    purchase_record,
                    purchase_email
                )
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
            `,
			balanceCodeId,
			createTime,
			startTime,
			endTime,
			balanceByteCount,
			netRevenue,
			secret,
			purchaseEventId,
			purchaseRecord,
			purchaseEmail,
		))

		balanceCode = &BalanceCode{
			BalanceCodeId:    balanceCodeId,
			CreateTime:       createTime,
			StartTime:        startTime,
			EndTime:          endTime,
			BalanceByteCount: balanceByteCount,
			NetRevenue:       netRevenue,
			Secret:           secret,
			PurchaseEventId:  purchaseEventId,
			PurchaseRecord:   purchaseRecord,
			PurchaseEmail:    purchaseEmail,
		}
	})
	return
}

type RedeemBalanceCodeArgs struct {
	Secret string `json:"secret"`
}

type RedeemBalanceCodeResult struct {
	TransferBalance *RedeemBalanceCodeTransferBalance `json:"transfer_balance,omitempty"`
	Error           *RedeemBalanceCodeError           `json:"error,omitempty"`
}

type RedeemBalanceCodeTransferBalance struct {
	TransferBalanceId bringyour.Id `json:"transfer_balance_id"`
	StartTime         time.Time    `json:"start_time"`
	EndTime           time.Time    `json:"end_time"`
	BalanceByteCount  ByteCount    `json:"balance_byte_count"`
}

type RedeemBalanceCodeError struct {
	Message string `json:"message"`
}

func RedeemBalanceCode(
	redeemBalanceCode *RedeemBalanceCodeArgs,
	session *session.ClientSession,
) (redeemBalanceCodeResult *RedeemBalanceCodeResult, returnErr error) {
	bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {
		result, err := tx.Query(
			session.Ctx,
			`
                SELECT 
                    balance_code_id,
                    start_time,
                    end_time,
                    balance_byte_count,
                    net_revenue_nano_cents
                FROM transfer_balance_code
                WHERE
                    balance_code_secret = $1 AND
                    redeem_balance_id IS NULL
                FOR UPDATE
            `,
			redeemBalanceCode.Secret,
		)
		var balanceCode *BalanceCode
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				balanceCode = &BalanceCode{}
				bringyour.Raise(result.Scan(
					&balanceCode.BalanceCodeId,
					&balanceCode.StartTime,
					&balanceCode.EndTime,
					&balanceCode.BalanceByteCount,
					&balanceCode.NetRevenue,
				))
			}
		})
		if balanceCode == nil {
			redeemBalanceCodeResult = &RedeemBalanceCodeResult{
				Error: &RedeemBalanceCodeError{
					Message: "Unknown balance code.",
				},
			}
			return
		}

		balanceId := bringyour.NewId()

		bringyour.RaisePgResult(tx.Exec(
			session.Ctx,
			`
                UPDATE transfer_balance_code
                SET
                    redeem_time = $2,
                    redeem_balance_id = $3
                WHERE
                    balance_code_id = $1
            `,
			balanceCode.BalanceCodeId,
			bringyour.NowUtc(),
			balanceId,
		))

		bringyour.RaisePgResult(tx.Exec(
			session.Ctx,
			`
                INSERT INTO transfer_balance (
                    balance_id,
                    network_id,
                    start_time,
                    end_time,
                    start_balance_byte_count,
                    balance_byte_count,
                    net_revenue_nano_cents
                )
                VALUES ($1, $2, $3, $4, $5, $5, $6)
            `,
			balanceId,
			session.ByJwt.NetworkId,
			balanceCode.StartTime,
			balanceCode.EndTime,
			balanceCode.BalanceByteCount,
			balanceCode.NetRevenue,
		))

		redeemBalanceCodeResult = &RedeemBalanceCodeResult{
			TransferBalance: &RedeemBalanceCodeTransferBalance{
				TransferBalanceId: balanceId,
				StartTime:         balanceCode.StartTime,
				EndTime:           balanceCode.EndTime,
				BalanceByteCount:  balanceCode.BalanceByteCount,
			},
		}
	}, bringyour.TxReadCommitted)

	return
}

type CheckBalanceCodeArgs struct {
	Secret string `json:"secret"`
}

type CheckBalanceCodeResult struct {
	Balance *CheckBalanceCodeBalance `json:"balance,omitempty"`
	Error   *CheckBalanceCodeError   `json:"error,omitempty"`
}

type CheckBalanceCodeBalance struct {
	StartTime        time.Time `json:"start_time"`
	EndTime          time.Time `json:"end_time"`
	BalanceByteCount ByteCount `json:"balance_byte_count"`
}

type CheckBalanceCodeError struct {
	Message string `json:"message"`
}

func CheckBalanceCode(
	checkBalanceCode *CheckBalanceCodeArgs,
	session *session.ClientSession,
) (checkBalanceCodeResult *CheckBalanceCodeResult, returnErr error) {
	bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {
		var balanceCode *BalanceCode

		result, err := tx.Query(
			session.Ctx,
			`
                SELECT 
                    balance_code_id,
                    start_time,
                    end_time,
                    balance_byte_count,
                    net_revenue_nano_cents
                FROM transfer_balance_code
                WHERE
                    balance_code_secret = $1 AND
                    redeem_balance_id IS NULL
                FOR UPDATE
            `,
			checkBalanceCode.Secret,
		)
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				balanceCode = &BalanceCode{}
				bringyour.Raise(result.Scan(
					&balanceCode.BalanceCodeId,
					&balanceCode.StartTime,
					&balanceCode.EndTime,
					&balanceCode.BalanceByteCount,
					&balanceCode.NetRevenue,
				))
			}
		})

		if balanceCode == nil {
			checkBalanceCodeResult = &CheckBalanceCodeResult{
				Error: &CheckBalanceCodeError{
					Message: "Unknown balance code.",
				},
			}
			return
		}

		checkBalanceCodeResult = &CheckBalanceCodeResult{
			Balance: &CheckBalanceCodeBalance{
				StartTime:        balanceCode.StartTime,
				EndTime:          balanceCode.EndTime,
				BalanceByteCount: balanceCode.BalanceByteCount,
			},
		}
	}, bringyour.TxReadCommitted)

	return
}

// FIXME add payment_token
type TransferBalance struct {
	BalanceId             bringyour.Id `json:"balance_id"`
	NetworkId             bringyour.Id `json:"network_id"`
	StartTime             time.Time    `json:"start_time"`
	EndTime               time.Time    `json:"end_time"`
	StartBalanceByteCount ByteCount    `json:"start_balance_byte_count"`
	// how much money the platform made after subtracting fees
	NetRevenue       NanoCents `json:"net_revenue_nano_cents"`
	BalanceByteCount ByteCount `json:"balance_byte_count"`
	PurchaseToken    string    `json:"purchase_token,omitempty"`
}

func GetActiveTransferBalances(ctx context.Context, networkId bringyour.Id) []*TransferBalance {
	now := bringyour.NowUtc()

	transferBalances := []*TransferBalance{}

	bringyour.Db(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    balance_id,
                    start_time,
                    end_time,
                    start_balance_byte_count,
                    net_revenue_nano_cents,
                    balance_byte_count
                FROM transfer_balance
                WHERE
                    network_id = $1 AND
                    active = true AND
                    start_time <= $2 AND $2 < end_time
            `,
			networkId,
			now,
		)
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				transferBalance := &TransferBalance{
					NetworkId: networkId,
				}
				bringyour.Raise(result.Scan(
					&transferBalance.BalanceId,
					&transferBalance.StartTime,
					&transferBalance.EndTime,
					&transferBalance.StartBalanceByteCount,
					&transferBalance.NetRevenue,
					&transferBalance.BalanceByteCount,
				))
				transferBalances = append(transferBalances, transferBalance)
			}
		})
	})

	return transferBalances
}

func GetActiveTransferBalanceByteCount(ctx context.Context, networkId bringyour.Id) ByteCount {
	net := ByteCount(0)
	for _, transferBalance := range GetActiveTransferBalances(ctx, networkId) {
		net += transferBalance.BalanceByteCount
	}
	return net
}

func AddTransferBalance(ctx context.Context, transferBalance *TransferBalance) {
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		balanceId := bringyour.NewId()

		bringyour.RaisePgResult(tx.Exec(
			ctx,
			`
                INSERT INTO transfer_balance (
                    balance_id,
                    network_id,
                    start_time,
                    end_time,
                    start_balance_byte_count,
                    balance_byte_count,
                    net_revenue_nano_cents,
                    purchase_token
                )
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
            `,
			balanceId,
			transferBalance.NetworkId,
			transferBalance.StartTime,
			transferBalance.EndTime,
			transferBalance.StartBalanceByteCount,
			transferBalance.BalanceByteCount,
			transferBalance.NetRevenue,
			transferBalance.PurchaseToken,
		))

		transferBalance.BalanceId = balanceId
	})
}

// TODO GetLastTransferData returns the transfer data with
// 1. the given purhase record
// 2. that starte before and ends after sub.ExpiryTime
// TODO with the max end time
// TODO if none, return err
func GetOverlappingTransferBalance(ctx context.Context, purchaseToken string, expiryTime time.Time) (balanceId bringyour.Id, returnErr error) {
	bringyour.Db(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    balance_id
                FROM transfer_balance
                WHERE
                    purchase_token = $1 AND
                    $2 < end_time AND
                    start_time <= $2
                ORDER BY end_time DESC
                LIMIT 1
            `,
			purchaseToken,
			expiryTime,
		)
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				bringyour.Raise(result.Scan(&balanceId))
			} else {
				returnErr = errors.New("Overlapping transfer balance not found.")
			}
		})
	})

	return
}

// add balance to a network at no cost
func AddBasicTransferBalance(
	ctx context.Context,
	networkId bringyour.Id,
	transferBalance ByteCount,
	startTime time.Time,
	endTime time.Time,
) (success bool) {
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		balanceId := bringyour.NewId()

		tag := bringyour.RaisePgResult(tx.Exec(
			ctx,
			`
                INSERT INTO transfer_balance (
                    balance_id,
                    network_id,
                    start_time,
                    end_time,
                    start_balance_byte_count,
                    net_revenue_nano_cents,
                    balance_byte_count
                )
                VALUES ($1, $2, $3, $4, $5, $6, $5)
            `,
			balanceId,
			networkId,
			startTime,
			endTime,
			transferBalance,
			NanoCents(0),
		))

		success = (tag.RowsAffected() == 1)
	})
	return
}

// this finds networks with no entries in transfer_balance
// this is potentially different than networks with zero transfer balance
func FindNetworksWithoutTransferBalance(ctx context.Context) (networkIds []bringyour.Id) {
	bringyour.Db(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT 
                    network.network_id
                FROM network
                
                LEFT JOIN transfer_balance ON transfer_balance.network_id = network.network_id

                GROUP BY (network.network_id)
                HAVING COUNT(transfer_balance.balance_id) = 0
            `,
		)

		networkIds = []bringyour.Id{}
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId bringyour.Id
				bringyour.Raise(result.Scan(&networkId))
				networkIds = append(networkIds, networkId)
			}
		})
	})
	return
}

type EscrowId struct {
	ContractId bringyour.Id
	BalanceId  bringyour.Id
}

// `bringyour.ComplexValue`
func (self *EscrowId) Values() []any {
	return []any{
		self.ContractId,
		self.BalanceId,
	}
}

type ContractOutcome = string

const (
	ContractOutcomeSettled                      ContractOutcome = "settled"
	ContractOutcomeDisputeResolvedToSource      ContractOutcome = "dispute_resolved_to_source"
	ContractOutcomeDisputeResolvedToDestination ContractOutcome = "dispute_resolved_to_destination"
)

type ContractParty = string

const (
	ContractPartySource      ContractParty = "source"
	ContractPartyDestination ContractParty = "destination"
	ContractPartyCheckpoint  ContractParty = "checkpoint"
)

type TransferEscrow struct {
	ContractId bringyour.Id
	Balances   []*TransferEscrowBalance
}

type TransferEscrowBalance struct {
	BalanceId        bringyour.Id
	BalanceByteCount ByteCount
}

func createTransferEscrowInTx(
	ctx context.Context,
	tx bringyour.PgTx,
	sourceNetworkId bringyour.Id,
	sourceId bringyour.Id,
	destinationNetworkId bringyour.Id,
	destinationId bringyour.Id,
	payerNetworkId bringyour.Id,
	contractTransferByteCount ByteCount,
	companionContractId *bringyour.Id,
) (transferEscrow *TransferEscrow, returnErr error) {
	contractId := bringyour.NewId()

	// order balances by end date, ascending
	// take from the earlier before the later
	result, err := tx.Query(
		ctx,
		`
            SELECT
                balance_id,
                balance_byte_count
            FROM transfer_balance
            WHERE
                network_id = $1 AND
                active = true AND
                start_time <= $2 AND $2 < end_time

            ORDER BY end_time ASC

            FOR UPDATE
        `,
		payerNetworkId,
		bringyour.NowUtc(),
	)
	// add up the balance_byte_count until >= contractTransferByteCount
	// if not enough, error
	escrow := map[bringyour.Id]ByteCount{}
	netEscrowBalanceByteCount := ByteCount(0)
	bringyour.WithPgResult(result, err, func() {
		for result.Next() {
			var balanceId bringyour.Id
			var balanceByteCount ByteCount
			bringyour.Raise(result.Scan(
				&balanceId,
				&balanceByteCount,
			))
			escrowBalanceByteCount := min(contractTransferByteCount-netEscrowBalanceByteCount, balanceByteCount)
			escrow[balanceId] = escrowBalanceByteCount
			netEscrowBalanceByteCount += escrowBalanceByteCount
			if contractTransferByteCount <= netEscrowBalanceByteCount {
				break
			}
		}
	})

	if netEscrowBalanceByteCount < contractTransferByteCount {
		returnErr = fmt.Errorf("Insufficient balance (%d).", netEscrowBalanceByteCount)
		return
	}

	/*
	   bringyour.CreateTempJoinTableInTx(
	       ctx,
	       tx,
	       "escrow(balance_id uuid -> balance_byte_count bigint)",
	       escrow,
	   )
	*/

	bringyour.BatchInTx(ctx, tx, func(batch bringyour.PgBatch) {
		for balanceId, balanceByteCount := range escrow {
			batch.Queue(
				`
                    UPDATE transfer_balance
                    SET
                        balance_byte_count = transfer_balance.balance_byte_count - $2
                    WHERE
                        transfer_balance.balance_id = $1
                `,
				balanceId,
				balanceByteCount,
			)

			batch.Queue(
				`
                    INSERT INTO transfer_escrow (
                        contract_id,
                        balance_id,
                        balance_byte_count
                    )
                    VALUES ($1, $2, $3)
                `,
				contractId,
				balanceId,
				balanceByteCount,
			)
		}
	})

	bringyour.RaisePgResult(tx.Exec(
		ctx,
		`
            INSERT INTO transfer_contract (
                contract_id,
                source_network_id,
                source_id,
                destination_network_id,
                destination_id,
                transfer_byte_count,
                companion_contract_id,
                payer_network_id,
                create_time
            )
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
        `,
		contractId,
		sourceNetworkId,
		sourceId,
		destinationNetworkId,
		destinationId,
		contractTransferByteCount,
		companionContractId,
		payerNetworkId,
		bringyour.NowUtc(),
	))

	balances := []*TransferEscrowBalance{}
	for balanceId, escrowBalanceByteCount := range escrow {
		balance := &TransferEscrowBalance{
			BalanceId:        balanceId,
			BalanceByteCount: escrowBalanceByteCount,
		}
		balances = append(balances, balance)
	}

	transferEscrow = &TransferEscrow{
		ContractId: contractId,
		Balances:   balances,
	}

	return
}

// renaming of `CreateTransferEscrow` since contract is the top level concept
func CreateContract(
	ctx context.Context,
	sourceNetworkId bringyour.Id,
	sourceId bringyour.Id,
	destinationNetworkId bringyour.Id,
	destinationId bringyour.Id,
	contractTransferByteCount ByteCount,
) (contractId bringyour.Id, transferEscrow *TransferEscrow, returnErr error) {
	transferEscrow, returnErr = CreateTransferEscrow(
		ctx,
		sourceNetworkId,
		sourceId,
		destinationNetworkId,
		destinationId,
		contractTransferByteCount,
	)
	if transferEscrow != nil {
		contractId = transferEscrow.ContractId
	}
	return
}

func CreateTransferEscrow(
	ctx context.Context,
	sourceNetworkId bringyour.Id,
	sourceId bringyour.Id,
	destinationNetworkId bringyour.Id,
	destinationId bringyour.Id,
	contractTransferByteCount ByteCount,
) (transferEscrow *TransferEscrow, returnErr error) {
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {

		transferEscrow, returnErr = createTransferEscrowInTx(
			ctx,
			tx,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			// source is payer
			sourceNetworkId,
			contractTransferByteCount,
			nil,
		)

	}, bringyour.TxReadCommitted)

	return
}

// renaming of `CreateCompanionTransferEscrow` since contract is the top level concept
func CreateCompanionContract(
	ctx context.Context,
	sourceNetworkId bringyour.Id,
	sourceId bringyour.Id,
	destinationNetworkId bringyour.Id,
	destinationId bringyour.Id,
	contractTransferByteCount ByteCount,
	originContractTimeout time.Duration,
) (contractId bringyour.Id, transferEscrow *TransferEscrow, returnErr error) {
	transferEscrow, returnErr = CreateCompanionTransferEscrow(
		ctx,
		sourceNetworkId,
		sourceId,
		destinationNetworkId,
		destinationId,
		contractTransferByteCount,
		originContractTimeout,
	)
	if transferEscrow != nil {
		contractId = transferEscrow.ContractId
	}
	return
}

func CreateCompanionTransferEscrow(
	ctx context.Context,
	sourceNetworkId bringyour.Id,
	sourceId bringyour.Id,
	destinationNetworkId bringyour.Id,
	destinationId bringyour.Id,
	contractTransferByteCount ByteCount,
	originContractTimeout time.Duration,
) (transferEscrow *TransferEscrow, returnErr error) {
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		// find the earliest open transfer contract in the opposite direction
		// with null companion_contract_id
		// there can be many companion contracts for an original contract

		result, err := tx.Query(
			ctx,
			`
                SELECT
                    contract_id
                FROM transfer_contract
                WHERE
                    (
                        open = true AND
                        source_id = $1 AND
                        destination_id = $2 AND
                        companion_contract_id IS NULL
                    )

                    OR

                    (
                        open = false AND
                        $3 <= close_time AND
                        source_id = $1 AND
                        destination_id = $2 AND
                        companion_contract_id IS NULL
                    )
                ORDER BY create_time ASC
                LIMIT 1
            `,
			// note the origin direction is reversed
			destinationId,
			sourceId,
			bringyour.NowUtc().Add(-originContractTimeout),
		)
		var companionContractId *bringyour.Id
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				bringyour.Raise(result.Scan(&companionContractId))
			}
		})

		if companionContractId == nil {
			returnErr = fmt.Errorf("Missing origin contract for companion.")
			return
		}

		transferEscrow, returnErr = createTransferEscrowInTx(
			ctx,
			tx,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			// destination is payer
			destinationNetworkId,
			contractTransferByteCount,
			companionContractId,
		)
	}, bringyour.TxReadCommitted)

	return
}

// contract_ids ordered by create time with:
// - at least `contractTransferByteCount` available
// - not closed by any party
// - with transfer escrow
func GetOpenTransferEscrowsOrderedByCreateTime(
	ctx context.Context,
	sourceId bringyour.Id,
	destinationId bringyour.Id,
	contractTransferByteCount ByteCount,
) map[bringyour.Id]ByteCount {
	contractIdTransferByteCounts := map[bringyour.Id]ByteCount{}

	bringyour.Db(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    
                    transfer_contract.contract_id,
                    transfer_contract.transfer_byte_count

                FROM transfer_contract

                LEFT OUTER JOIN contract_close ON
                    contract_close.contract_id = transfer_contract.contract_id

                INNER JOIN transfer_escrow ON
                    transfer_escrow.contract_id = transfer_contract.contract_id

                WHERE
                    transfer_contract.open = true AND
                    transfer_contract.source_id = $1 AND
                    transfer_contract.destination_id = $2 AND
                    transfer_contract.transfer_byte_count <= $3 AND
                    contract_close.contract_id IS NULL

                ORDER BY transfer_contract.create_time ASC
            `,
			sourceId,
			destinationId,
			contractTransferByteCount,
		)
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				var contractId bringyour.Id
				var transferByteCount ByteCount
				bringyour.Raise(result.Scan(&contractId, &transferByteCount))
				contractIdTransferByteCounts[contractId] = transferByteCount
			}
		})
	})

	return contractIdTransferByteCounts
}

func GetTransferEscrow(ctx context.Context, contractId bringyour.Id) (transferEscrow *TransferEscrow) {
	bringyour.Db(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    balance_id,
                    balance_byte_count

                FROM transfer_escrow
                WHERE
                    contract_id = $1
            `,
			contractId,
		)

		balances := []*TransferEscrowBalance{}

		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				balance := &TransferEscrowBalance{}
				bringyour.Raise(result.Scan(
					&balance.BalanceId,
					&balance.BalanceByteCount,
				))
				balances = append(balances, balance)
			}
		})

		if len(balances) == 0 {
			return
		}

		transferEscrow = &TransferEscrow{
			ContractId: contractId,
			Balances:   balances,
		}
	})

	return
}

// some clients - platform, friends and family, etc - do not need an escrow
// typically `provide_mode < Public` does not use an escrow1
func CreateContractNoEscrow(
	ctx context.Context,
	sourceNetworkId bringyour.Id,
	sourceId bringyour.Id,
	destinationNetworkId bringyour.Id,
	destinationId bringyour.Id,
	contractTransferByteCount ByteCount,
) (contractId bringyour.Id, returnErr error) {
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		contractId = bringyour.NewId()

		bringyour.RaisePgResult(tx.Exec(
			ctx,
			`
                INSERT INTO transfer_contract (
                    contract_id,
                    source_network_id,
                    source_id,
                    destination_network_id,
                    destination_id,
                    transfer_byte_count,
                    create_time
                )
                VALUES ($1, $2, $3, $4, $5, $6, $7)
            `,
			contractId,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			contractTransferByteCount,
			bringyour.NowUtc(),
		))
	})
	return
}

// this will create a close entry,
// then settle if all parties agree, or set dispute if there is a dispute
func CloseContract(
	ctx context.Context,
	contractId bringyour.Id,
	clientId bringyour.Id,
	usedTransferByteCount ByteCount,
	checkpoint bool,
) (returnErr error) {
	// settle := false
	// dispute := false

	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		found := false
		var sourceId bringyour.Id
		var destinationId bringyour.Id
		var outcome *ContractOutcome
		var dispute bool
		var party ContractParty

		result, err := tx.Query(
			ctx,
			`
                SELECT
                    source_id,
                    destination_id,
                    outcome,
                    dispute
                FROM transfer_contract
                WHERE
                    contract_id = $1
                FOR UPDATE
            `,
			contractId,
		)
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				found = true
				bringyour.Raise(result.Scan(&sourceId, &destinationId, &outcome, &dispute))
				if clientId == sourceId {
					party = ContractPartySource
				} else if clientId == destinationId {
					party = ContractPartyDestination
				}
			}
		})

		if !found {
			returnErr = fmt.Errorf("Contract not found: %s", contractId.String())
			return
		}
		if party == "" {
			returnErr = fmt.Errorf("Client is not a party to the contract: %s %s %s->%s", contractId.String(), clientId.String(), sourceId.String(), destinationId.String())
			return
		}
		if outcome != nil {
			returnErr = fmt.Errorf("Contract already closed with outcome %s: %s %s %s->%s", *outcome, contractId.String(), clientId.String(), sourceId.String(), destinationId.String())
			return
		}
		if dispute {
			returnErr = fmt.Errorf("Contract in dispute: %s %s %s->%s", contractId.String(), clientId.String(), sourceId.String(), destinationId.String())
			return
		}

		if checkpoint {
			bringyour.RaisePgResult(tx.Exec(
				ctx,
				`
                    INSERT INTO contract_close (
                        contract_id,
                        party,
                        used_transfer_byte_count,
                        close_time,
                        checkpoint
                    )
                    VALUES ($1, $2, $3, $4, true)
                    ON CONFLICT (contract_id, party) DO UPDATE
                    SET
                        used_transfer_byte_count = contract_close.used_transfer_byte_count + $3,
                        close_time = $4
                    WHERE
                        contract_close.checkpoint = true
                `,
				contractId,
				party,
				usedTransferByteCount,
				bringyour.NowUtc(),
			))

		} else {
			bringyour.RaisePgResult(tx.Exec(
				ctx,
				`
                    INSERT INTO contract_close (
                        contract_id,
                        party,
                        used_transfer_byte_count,
                        close_time,
                        checkpoint
                    )
                    VALUES ($1, $2, $3, $4, false)
                    ON CONFLICT (contract_id, party) DO UPDATE
                    SET
                        used_transfer_byte_count = contract_close.used_transfer_byte_count + $3,
                        close_time = $4,
                        checkpoint = false
                    WHERE
                        contract_close.checkpoint = true
                `,
				contractId,
				party,
				usedTransferByteCount,
				bringyour.NowUtc(),
			))

			// party -> used transfer byte count
			closes := map[ContractParty]ByteCount{}
			result, err = tx.Query(
				ctx,
				`
                SELECT
                    party,
                    used_transfer_byte_count
                FROM contract_close
                WHERE
                    contract_id = $1 AND
                    checkpoint = false
                `,
				contractId,
			)
			bringyour.WithPgResult(result, err, func() {
				for result.Next() {
					var closeParty ContractParty
					var closeUsedTransferByteCount ByteCount
					bringyour.Raise(result.Scan(
						&closeParty,
						&closeUsedTransferByteCount,
					))
					closes[closeParty] = closeUsedTransferByteCount
				}
			})
			for closeParty, closeUsedTransferByteCount := range closes {
				fmt.Printf("CLOSE CONTRACT PARTY %s=%d: %s %s %s->%s\n", closeParty, closeUsedTransferByteCount, contractId.String(), clientId.String(), sourceId.String(), destinationId.String())
			}

			sourceUsedTransferByteCount, sourceOk := closes[ContractPartySource]
			destinationUsedTransferByteCount, destinationOk := closes[ContractPartyDestination]

			if sourceOk && destinationOk {
				hasEscrow := false

				result, err := tx.Query(
					ctx,
					`
                        SELECT balance_id FROM transfer_escrow
                        WHERE contract_id = $1
                        LIMIT 1
                    `,
					contractId,
				)
				bringyour.WithPgResult(result, err, func() {
					if result.Next() {
						hasEscrow = true
					}
				})

				if hasEscrow {
					diff := sourceUsedTransferByteCount - destinationUsedTransferByteCount
					if 0 != diff {
						fmt.Printf("CONTRACT (%s) PARTY DIFF %d (%d <> %d)\n", contractId.String(), diff, sourceUsedTransferByteCount, destinationUsedTransferByteCount)
					}
					if math.Abs(float64(diff)) <= AcceptableTransfersByteDifference {
						fmt.Printf("CLOSE CONTRACT SETTLE (%s) %s\n", clientId.String(), contractId.String())
						returnErr = settleEscrowInTx(ctx, tx, contractId, ContractOutcomeSettled)
					} else {
						fmt.Printf("CLOSE CONTRACT DISPUTE (%s) %s\n", clientId.String(), contractId.String())
						setContractDisputeInTx(ctx, tx, contractId, true)
					}
				} else {
					// nothing to settle, just close the transaction
					bringyour.RaisePgResult(tx.Exec(
						ctx,
						`
                            UPDATE transfer_contract
                            SET
                                outcome = $2,
                                close_time = $3
                            WHERE
                                contract_id = $1
                        `,
						contractId,
						ContractOutcomeSettled,
						bringyour.NowUtc(),
					))
				}
			}
		}
	}, bringyour.TxReadCommitted)

	return
}

func SettleEscrow(ctx context.Context, contractId bringyour.Id, outcome ContractOutcome) (returnErr error) {
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		returnErr = settleEscrowInTx(ctx, tx, contractId, outcome)
	}, bringyour.TxReadCommitted)

	return
}

func settleEscrowInTx(
	ctx context.Context,
	tx bringyour.PgTx,
	contractId bringyour.Id,
	outcome ContractOutcome,
) (returnErr error) {
	var usedTransferByteCount ByteCount

	switch outcome {
	case ContractOutcomeSettled:
		result, err := tx.Query(
			ctx,
			`
                SELECT
                    used_transfer_byte_count,
                    party,
                    checkpoint
                FROM contract_close
                WHERE
                    contract_id = $1
                FOR UPDATE
            `,
			contractId,
		)
		netUsedTransferByteCount := ByteCount(0)
		partyCount := 0
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				var usedTransferByteCountForParty ByteCount
				var party ContractParty
				var checkpoint bool
				bringyour.Raise(result.Scan(
					&usedTransferByteCountForParty,
					&party,
					&checkpoint,
				))
				if checkpoint {
					returnErr = fmt.Errorf("Cannot settle party %s with a checkpoint.", party)
					return
				}
				netUsedTransferByteCount += usedTransferByteCountForParty
				partyCount += 1
				fmt.Printf("SETTLE %s: found party %s used %d\n", contractId.String(), party, usedTransferByteCountForParty)
			}
		})
		if partyCount != 2 {
			returnErr = fmt.Errorf("Must have 2 parties to settle contract (found %d).", partyCount)
			return
		}
		usedTransferByteCount = netUsedTransferByteCount / ByteCount(partyCount)
	case ContractOutcomeDisputeResolvedToSource, ContractOutcomeDisputeResolvedToDestination:
		var party ContractParty
		switch outcome {
		case ContractOutcomeDisputeResolvedToSource:
			party = ContractPartySource
		default:
			party = ContractPartyDestination
		}
		result, err := tx.Query(
			ctx,
			`
                SELECT
                    used_transfer_byte_count
                FROM contract_close
                WHERE
                    contract_id = $1 AND
                    party = $2
                FOR UPDATE
            `,
			contractId,
			party,
		)
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				bringyour.Raise(result.Scan(&usedTransferByteCount))
			}
		})
	default:
		returnErr = fmt.Errorf("Unknown contract outcome: %s", outcome)
		return
	}

	// order balances by end date, ascending
	// take from the earlier before the later
	result, err := tx.Query(
		ctx,
		`
            SELECT
                transfer_escrow.balance_id,
                transfer_escrow.balance_byte_count,
                transfer_balance.start_balance_byte_count,
                transfer_balance.net_revenue_nano_cents
            FROM transfer_contract

            INNER JOIN transfer_escrow ON
                transfer_escrow.contract_id = transfer_contract.contract_id

            INNER JOIN transfer_balance ON
                transfer_balance.balance_id = transfer_escrow.balance_id

            WHERE
                transfer_contract.contract_id = $1 AND
                transfer_contract.outcome IS NULL

            ORDER BY transfer_balance.end_time ASC

            FOR UPDATE
        `,
		contractId,
	)

	// balance id -> payout byte count, return byte count, payout
	sweepPayouts := map[bringyour.Id]sweepPayout{}
	netPayoutByteCount := ByteCount(0)
	netPayout := NanoCents(0)

	bringyour.WithPgResult(result, err, func() {
		for result.Next() {
			var balanceId bringyour.Id
			var escrowBalanceByteCount ByteCount
			var startBalanceByteCount ByteCount
			var netRevenue NanoCents
			bringyour.Raise(result.Scan(
				&balanceId,
				&escrowBalanceByteCount,
				&startBalanceByteCount,
				&netRevenue,
			))

			payoutByteCount := min(usedTransferByteCount-netPayoutByteCount, escrowBalanceByteCount)
			returnByteCount := escrowBalanceByteCount - payoutByteCount
			netPayoutByteCount += payoutByteCount
			payout := NanoCents(math.Round(
				ProviderRevenueShare * float64(netRevenue) * float64(payoutByteCount) / float64(startBalanceByteCount),
			))
			netPayout += payout
			sweepPayouts[balanceId] = sweepPayout{
				payoutByteCount: payoutByteCount,
				returnByteCount: returnByteCount,
				payout:          payout,
			}
			fmt.Printf("SETTLE %s %s: payout %d (%d nanocents) return %d\n", contractId.String(), balanceId.String(), payoutByteCount, payout, returnByteCount)
		}
	})

	if len(sweepPayouts) == 0 {
		returnErr = fmt.Errorf("Invalid contract.")
		return
	}

	if netPayoutByteCount < usedTransferByteCount {
		returnErr = fmt.Errorf("Escrow does not have enough value to pay out the full amount.")
		return
	}

	var payoutNetworkId *bringyour.Id
	result, err = tx.Query(
		ctx,
		`
            SELECT
                source_network_id,
                destination_network_id,
                payer_network_id,
                companion_contract_id
            FROM transfer_contract
            WHERE
                contract_id = $1
            FOR UPDATE
        `,
		contractId,
	)
	bringyour.WithPgResult(result, err, func() {
		if result.Next() {
			var sourceNetworkId bringyour.Id
			var destinationNetworkId bringyour.Id
			var payerNetworkId *bringyour.Id
			var companionContractId *bringyour.Id
			bringyour.Raise(result.Scan(
				&sourceNetworkId,
				&destinationNetworkId,
				&payerNetworkId,
				&companionContractId,
			))
			if payerNetworkId != nil {
				if *payerNetworkId == sourceNetworkId {
					payoutNetworkId = &destinationNetworkId
				} else {
					payoutNetworkId = &sourceNetworkId
				}
			} else {
				// migration, infer for older contracts
				if companionContractId == nil {
					payoutNetworkId = &destinationNetworkId
				} else {
					payoutNetworkId = &sourceNetworkId
				}
			}
		}
	})

	if payoutNetworkId == nil {
		returnErr = fmt.Errorf("Destination client does not exist.")
		return
	}

	bringyour.CreateTempJoinTableInTx(
		ctx,
		tx,
		"sweep_payout(balance_id uuid -> payout_byte_count bigint, return_byte_count bigint, payout_net_revenue_nano_cents bigint)",
		sweepPayouts,
	)

	bringyour.RaisePgResult(tx.Exec(
		ctx,
		`
            UPDATE transfer_contract
            SET
                outcome = $2,
                close_time = $3
            WHERE
                contract_id = $1
        `,
		contractId,
		ContractOutcomeSettled,
		bringyour.NowUtc(),
	))

	bringyour.RaisePgResult(tx.Exec(
		ctx,
		`
            UPDATE transfer_escrow
            SET
                settled = true,
                settle_time = $2,
                payout_byte_count = sweep_payout.payout_byte_count
            FROM sweep_payout
            WHERE
                transfer_escrow.contract_id = $1 AND
                transfer_escrow.balance_id = sweep_payout.balance_id
        `,
		contractId,
		bringyour.NowUtc(),
	))

	bringyour.RaisePgResult(tx.Exec(
		ctx,
		`
            INSERT INTO transfer_escrow_sweep (
                contract_id,
                balance_id,
                network_id,
                payout_byte_count,
                payout_net_revenue_nano_cents
            )
            SELECT
                $1 AS contract_id,
                sweep_payout.balance_id,
                $2 AS network_id,
                sweep_payout.payout_byte_count,
                sweep_payout.payout_net_revenue_nano_cents
            FROM sweep_payout

            WHERE
                0 < sweep_payout.payout_byte_count
        `,
		contractId,
		payoutNetworkId,
	))

	bringyour.RaisePgResult(tx.Exec(
		ctx,
		`
            UPDATE transfer_balance
            SET
                balance_byte_count = transfer_balance.balance_byte_count + sweep_payout.return_byte_count
            FROM sweep_payout
            WHERE
                transfer_balance.balance_id = sweep_payout.balance_id
        `,
	))

	bringyour.RaisePgResult(tx.Exec(
		ctx,
		`
        INSERT INTO account_balance (
            network_id,
            provided_byte_count,
            provided_net_revenue_nano_cents
        )
        VALUES ($1, $2, $3)
        ON CONFLICT (network_id) DO UPDATE
        SET
            provided_byte_count = account_balance.provided_byte_count + $2,
            provided_net_revenue_nano_cents = account_balance.provided_net_revenue_nano_cents + $3
        `,
		payoutNetworkId,
		netPayoutByteCount,
		netPayout,
	))

	return
}

type sweepPayout struct {
	payoutByteCount ByteCount
	returnByteCount ByteCount
	payout          NanoCents
}

// `bringyour.ComplexValue`
func (self *sweepPayout) Values() []any {
	return []any{self.payoutByteCount, self.returnByteCount, self.payout}
}

func SetContractDispute(ctx context.Context, contractId bringyour.Id, dispute bool) {
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		setContractDisputeInTx(ctx, tx, contractId, dispute)
	})
}

func setContractDisputeInTx(
	ctx context.Context,
	tx bringyour.PgTx,
	contractId bringyour.Id,
	dispute bool,
) {
	bringyour.RaisePgResult(tx.Exec(
		ctx,
		`
            UPDATE transfer_contract
            SET
                dispute = $2,
                close_time = $3
            WHERE
                contract_id = $1 AND
                outcome IS NULL
        `,
		contractId,
		dispute,
		bringyour.NowUtc(),
	))
}

func GetOpenContractIdsWithNoPartialClose(
	ctx context.Context,
	sourceId bringyour.Id,
	destinationId bringyour.Id,
) map[bringyour.Id]bool {
	contractIds := map[bringyour.Id]bool{}
	for contractId, party := range GetOpenContractIds(ctx, sourceId, destinationId) {
		if party == "" {
			contractIds[contractId] = true
		}
	}
	return contractIds
}

func GetOpenContractIdsWithPartialClose(
	ctx context.Context,
	sourceId bringyour.Id,
	destinationId bringyour.Id,
) map[bringyour.Id]ContractParty {
	contractIdPartialCloseParties := map[bringyour.Id]ContractParty{}
	for contractId, party := range GetOpenContractIds(ctx, sourceId, destinationId) {
		if party != "" {
			contractIdPartialCloseParties[contractId] = party
		}
	}
	return contractIdPartialCloseParties
}

// contract id -> partially closed contract party, or "" if none
func GetOpenContractIds(
	ctx context.Context,
	sourceId bringyour.Id,
	destinationId bringyour.Id,
) map[bringyour.Id]ContractParty {
	contractIdPartialCloseParties := map[bringyour.Id]ContractParty{}

	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		result, err := tx.Query(
			ctx,
			`
                SELECT
                    transfer_contract.contract_id,
                    contract_close.party,
                    contract_close.checkpoint
                FROM transfer_contract

                LEFT JOIN contract_close ON contract_close.contract_id = transfer_contract.contract_id

                WHERE
                    transfer_contract.open = true AND
                    transfer_contract.source_id = $1 AND
                    transfer_contract.destination_id = $2
            `,
			sourceId,
			destinationId,
		)
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				var contractId bringyour.Id
				var party_ *ContractParty
				var checkpoint_ *bool
				bringyour.Raise(result.Scan(&contractId, &party_, &checkpoint_))
				var party ContractParty
				if party_ != nil {
					party = *party_
				}
				var checkpoint bool
				if checkpoint_ != nil {
					checkpoint = *checkpoint_
				}
				// there can be up to two rows per contractId (one checkpoint)
				// non-checkpoint takes precedence
				if checkpoint {
					if contractIdPartialCloseParties[contractId] == "" {
						contractIdPartialCloseParties[contractId] = ContractPartyCheckpoint
					}
				} else {
					contractIdPartialCloseParties[contractId] = party
				}
			}
		})
	})

	return contractIdPartialCloseParties
}

// expired contracts are open:
// - 2 closes - one source and one checkpoint
// TODO - 0 closes can be used if the contract has a max lived time
// TODO   add this to the protocol
// TODO there may be some overlap with https://github.com/bringyour/bringyour/commit/4a8150083083161be04737f0cc4b087906d9b449
func GetExpiredOpenContractIds(
	ctx context.Context,
	contractCloseTimeout time.Duration,
) map[bringyour.Id]bool {
	contractIdPartialCloseParties := map[bringyour.Id]map[ContractParty]bool{}

	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		result, err := tx.Query(
			ctx,
			`
                SELECT
                    transfer_contract.contract_id,
                    contract_close.party,
                    contract_close.checkpoint
                FROM transfer_contract

                INNER JOIN contract_close ON
                    contract_close.contract_id = transfer_contract.contract_id AND
                    contract_close.close_time < $1

                WHERE
                    transfer_contract.open = true
            `,
			time.Now().Add(-contractCloseTimeout),
		)
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				var contractId bringyour.Id
				var party_ *ContractParty
				var checkpoint_ *bool
				bringyour.Raise(result.Scan(&contractId, &party_, &checkpoint_))
				var party ContractParty
				if party_ != nil {
					party = *party_
				}
				var checkpoint bool
				if checkpoint_ != nil {
					checkpoint = *checkpoint_
				}
				// there can be up to two rows per contractId (one checkpoint)
				// checkpoint takes precedence
				if checkpoint {
					party = ContractPartyCheckpoint
				}
				partialCloseParties, ok := contractIdPartialCloseParties[contractId]
				if !ok {
					partialCloseParties = map[ContractParty]bool{}
					contractIdPartialCloseParties[contractId] = partialCloseParties
				}
				partialCloseParties[party] = true
			}
		})
	})

	contractIdCloses := map[bringyour.Id]bool{}
	for contractId, partialCloseParties := range contractIdPartialCloseParties {
		hasSource := partialCloseParties[ContractPartySource]
		hasCheckpoint := partialCloseParties[ContractPartyCheckpoint]
		if hasSource && hasCheckpoint {
			contractIdCloses[contractId] = true
		}
	}

	return contractIdCloses
}

/*
func GetOpenContractIdsForSourceOrDestinationWithNoPartialClose(
    ctx context.Context,
    clientId bringyour.Id,
) map[TransferPair]map[bringyour.Id]bool {
    pairContractIdPartialCloseParties := GetOpenContractIdsForSourceOrDestination(ctx, clientId)
    pairContractIds := map[TransferPair]map[bringyour.Id]bool{}
    for transferPair, contractIdPartialCloseParties := range pairContractIdPartialCloseParties {
        for contractId, party := range contractIdPartialCloseParties {
            if party == "" {
                contractIds, ok := pairContractIds[transferPair]
                if !ok {
                    contractIds = map[bringyour.Id]bool{}
                    pairContractIds[transferPair] = contractIds
                }
                contractIds[contractId] = true
            }
        }
    }
    return pairContractIds
}
*/

// return key is unordered transfer pair
func GetOpenContractIdsForSourceOrDestination(
	ctx context.Context,
	clientId bringyour.Id,
) map[TransferPair]map[bringyour.Id]ContractParty {
	pairContractIdPartialCloseParties := map[TransferPair]map[bringyour.Id]ContractParty{}

	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		result, err := tx.Query(
			ctx,
			`
                SELECT
                    transfer_contract.source_id,
                    transfer_contract.destination_id,
                    transfer_contract.contract_id,
                    contract_close.party,
                    contract_close.checkpoint
                FROM transfer_contract

                LEFT JOIN contract_close ON 
                    contract_close.contract_id = transfer_contract.contract_id

                WHERE
                    transfer_contract.open = true AND (
                        transfer_contract.source_id = $1 OR
                        transfer_contract.destination_id = $1
                    )
            `,
			clientId,
		)
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				var sourceId bringyour.Id
				var destinationId bringyour.Id
				var contractId bringyour.Id
				var party_ *ContractParty
				var checkpoint_ *bool
				bringyour.Raise(result.Scan(
					&sourceId,
					&destinationId,
					&contractId,
					&party_,
					&checkpoint_,
				))
				transferPair := NewUnorderedTransferPair(sourceId, destinationId)
				contractIdPartialCloseParties, ok := pairContractIdPartialCloseParties[transferPair]
				var party ContractParty
				if party_ != nil {
					party = *party_
				}
				if !ok {
					contractIdPartialCloseParties = map[bringyour.Id]ContractParty{}
					pairContractIdPartialCloseParties[transferPair] = contractIdPartialCloseParties
				}
				var checkpoint bool
				if checkpoint_ != nil {
					checkpoint = *checkpoint_
				}
				// there can be up to two rows per contractId (one checkpoint)
				// non-checkpoint takes precedence
				if checkpoint {
					if contractIdPartialCloseParties[contractId] == "" {
						contractIdPartialCloseParties[contractId] = ContractPartyCheckpoint
					}
				} else {
					contractIdPartialCloseParties[contractId] = party
				}
			}
		})
	})

	return pairContractIdPartialCloseParties
}

// closes all open contracts with no update in the last `timeout`
// cases handled:
// - no closes
// - single close
// - one or more checkpoints
func ForceCloseOpenContractIds(ctx context.Context, timeout time.Duration) error {
	type OpenContract struct {
		contractId    bringyour.Id
		sourceId      bringyour.Id
		destinationId bringyour.Id

		sourceCloseTime             *time.Time
		sourceUsedTransferByteCount *ByteCount
		sourceCheckpoint            *bool

		destinationCloseTime             *time.Time
		destinationUsedTransferByteCount *ByteCount
		destinationCheckpoint            *bool
	}

	openContracts := []*OpenContract{}

	bringyour.Db(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    transfer_contract.contract_id,
                    transfer_contract.source_id,
                    transfer_contract.destination_id,
                    
                    source_contract_close.close_time AS source_close_time,
                    source_contract_close.used_transfer_byte_count AS source_used_transfer_byte_count,
                    source_contract_close.checkpoint AS source_checkpoint,

                    destination_contract_close.close_time AS destination_close_time,
                    destination_contract_close.used_transfer_byte_count AS destination_used_transfer_byte_count,
                    destination_contract_close.checkpoint AS destination_checkpoint
                    
                FROM transfer_contract
                    
                LEFT JOIN contract_close source_contract_close ON
                    source_contract_close.contract_id = transfer_contract.contract_id AND
                    source_contract_close.party = $1

                LEFT JOIN contract_close destination_contract_close ON
                    destination_contract_close.contract_id = transfer_contract.contract_id AND
                    destination_contract_close.party = $2

                WHERE
                    transfer_contract.open AND
                    transfer_contract.create_time <= $3 AND
                    (source_contract_close.close_time IS NULL OR source_contract_close.close_time <= $3) AND
                    (destination_contract_close.close_time IS NULL OR destination_contract_close.close_time <= $3)

            `,
			ContractPartySource,
			ContractPartyDestination,
			bringyour.NowUtc().Add(-timeout),
		)
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				openContract := &OpenContract{}

				bringyour.Raise(result.Scan(
					&openContract.contractId,
					&openContract.sourceId,
					&openContract.destinationId,
					&openContract.sourceCloseTime,
					&openContract.sourceUsedTransferByteCount,
					&openContract.sourceCheckpoint,
					&openContract.destinationCloseTime,
					&openContract.destinationUsedTransferByteCount,
					&openContract.destinationCheckpoint,
				))

				openContracts = append(openContracts, openContract)
			}
		})
	})

	for _, openContract := range openContracts {

		fmt.Printf("FORCE CLOSE %s\n", openContract.contractId)

		if openContract.sourceCloseTime == nil && openContract.destinationCloseTime == nil {
			// close with both sides 0

			err := CloseContract(
				ctx,
				openContract.contractId,
				openContract.sourceId,
				ByteCount(0),
				false,
			)
			if err != nil {
				return err
			}

			err = CloseContract(
				ctx,
				openContract.contractId,
				openContract.destinationId,
				ByteCount(0),
				false,
			)
			if err != nil {
				return err
			}

		} else if openContract.sourceCloseTime == nil {
			// source accepts destination

			err := CloseContract(
				ctx,
				openContract.contractId,
				openContract.sourceId,
				*openContract.destinationUsedTransferByteCount,
				false,
			)
			if err != nil {
				return err
			}

		} else if openContract.destinationCloseTime == nil {
			// destination accepts source

			err := CloseContract(
				ctx,
				openContract.contractId,
				openContract.destinationId,
				*openContract.sourceUsedTransferByteCount,
				false,
			)
			if err != nil {
				return err
			}

		} else {
			// finalize one or more checkpoints

			if *openContract.sourceCheckpoint {
				err := CloseContract(
					ctx,
					openContract.contractId,
					openContract.sourceId,
					ByteCount(0),
					false,
				)
				if err != nil {
					return err
				}
			}

			if *openContract.destinationCheckpoint {
				err := CloseContract(
					ctx,
					openContract.contractId,
					openContract.destinationId,
					ByteCount(0),
					false,
				)
				if err != nil {
					return err
				}
			}

		}
	}

	return nil
}

type ContractClose struct {
	CloseTime time.Time
	Dispute   bool
	Outcome   string
}

func GetContractClose(ctx context.Context, contractId bringyour.Id) (contractClose *ContractClose, closed bool) {
	bringyour.Db(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    close_time,
                    dispute,
                    outcome
                FROM transfer_contract
                WHERE
                    contract_id = $1
            `,
			contractId,
		)
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				var closeTime *time.Time
				var dispute bool
				var outcome *string
				bringyour.Raise(result.Scan(&closeTime, &dispute, &outcome))
				if outcome != nil {
					closed = true
					contractClose = &ContractClose{
						CloseTime: *closeTime,
						Dispute:   dispute,
						Outcome:   *outcome,
					}
				}
			}
		})
	})

	return
}

type AccountBalance struct {
	NetworkId          bringyour.Id
	ProvidedByteCount  ByteCount
	ProvidedNetRevenue NanoCents
	PaidByteCount      ByteCount
	PaidNetRevenue     NanoCents
}

type GetAccountBalanceResult struct {
	Balance *AccountBalance
	Error   *GetAccountBalanceError
}

type GetAccountBalanceError struct {
	Message string
}

func GetAccountBalance(session *session.ClientSession) *GetAccountBalanceResult {
	getAccountBalanceResult := &GetAccountBalanceResult{}
	bringyour.Db(session.Ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			session.Ctx,
			`
            SELECT
                provided_byte_count,
                provided_net_revenue_nano_cents,
                paid_byte_count,
                paid_net_revenue_nano_cents
            FROM account_balance
            WHERE
                network_id = $1
            `,
			session.ByJwt.NetworkId,
		)
		bringyour.WithPgResult(result, err, func() {
			balance := &AccountBalance{
				NetworkId: session.ByJwt.NetworkId,
			}
			if result.Next() {
				bringyour.Raise(result.Scan(
					&balance.ProvidedByteCount,
					&balance.ProvidedNetRevenue,
					&balance.PaidByteCount,
					&balance.PaidNetRevenue,
				))
			}
			// else empty balance
			getAccountBalanceResult.Balance = balance
		})
	})
	return getAccountBalanceResult
}

type Subscription struct {
	SubscriptionId bringyour.Id `json:"subscription_id"`
	Store          string       `json:"store"`
	Plan           string       `json:"plan"`
}

func CurrentSubscription(ctx context.Context, networkId bringyour.Id) *Subscription {
	// FIXME
	return nil
}

func GetNetPendingPayout(ctx context.Context, networkId bringyour.Id) NanoCents {
	// add up
	// - transfer_escrow_sweep
	// - pending payments

	// FIXME
	return 0
}

type SubscriptionCreatePaymentIdArgs struct {
}

type SubscriptionCreatePaymentIdResult struct {
	SubscriptionPaymentId bringyour.Id                      `json:"subscription_payment_id,omitempty"`
	Error                 *SubscriptionCreatePaymentIdError `json:"error,omitempty"`
}

type SubscriptionCreatePaymentIdError struct {
	Message string `json:"message"`
}

func SubscriptionCreatePaymentId(createPaymentId *SubscriptionCreatePaymentIdArgs, clientSession *session.ClientSession) (createPaymentIdResult *SubscriptionCreatePaymentIdResult, returnErr error) {
	bringyour.Tx(clientSession.Ctx, func(tx bringyour.PgTx) {
		result, err := tx.Query(
			clientSession.Ctx,
			`
            SELECT
                COUNT(subscription_payment_id) AS subscription_payment_id_count
            FROM subscription_payment
            WHERE
                network_id = $1 AND
                $2 <= create_time
            `,
			clientSession.ByJwt.NetworkId,
			bringyour.NowUtc().Add(-1*time.Hour),
		)

		limitExceeded := false

		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				var count int
				bringyour.Raise(result.Scan(&count))
				if MaxSubscriptionPaymentIdsPerHour <= count {
					limitExceeded = true
				}
			}
		})

		if limitExceeded {
			createPaymentIdResult = &SubscriptionCreatePaymentIdResult{
				Error: &SubscriptionCreatePaymentIdError{
					Message: "Too many subscription payments in the last hour. Try again later.",
				},
			}
			return
		}

		subscriptionPaymentId := bringyour.NewId()

		tx.Exec(
			clientSession.Ctx,
			`
            INSERT INTO subscription_payment (
                subscription_payment_id,
                network_id,
                user_id
            ) VALUES ($1, $2, $3)
            `,
			subscriptionPaymentId,
			clientSession.ByJwt.NetworkId,
			clientSession.ByJwt.UserId,
		)

		createPaymentIdResult = &SubscriptionCreatePaymentIdResult{
			SubscriptionPaymentId: subscriptionPaymentId,
		}
	})

	return
}

func SubscriptionGetNetworkIdForPaymentId(ctx context.Context, subscriptionPaymentId bringyour.Id) (networkId bringyour.Id, returnErr error) {
	bringyour.Db(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
            SELECT network_id FROM subscription_payment
            WHERE subscription_payment_id = $1
            `,
			subscriptionPaymentId,
		)
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				bringyour.Raise(result.Scan(&networkId))
			} else {
				returnErr = errors.New("Invalid subscription payment.")
			}
		})
	})
	return
}
