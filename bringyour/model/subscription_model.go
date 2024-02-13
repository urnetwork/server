package model


import (
    "context"
    "time"
    "fmt"
    "math"
    // "crypto/rand"
    // "encoding/hex"
    // "slices"
    "errors"

    // "golang.org/x/exp/maps"

    "bringyour.com/bringyour"
    "bringyour.com/bringyour/session"
)


type ByteCount = int64


type NanoCents = int64

func UsdToNanoCents(usd float64) NanoCents {
    return NanoCents(math.Round(usd * float64(1000000000)))
}

func NanoCentsToUsd(nanoCents NanoCents) float64 {
    return float64(nanoCents) / float64(1000000000)
}


// 31 days
const BalanceCodeDuration = 31 * 24 * time.Hour

// up to 32MiB
const AcceptableTransfersByteDifference = 32 * 1024 * 1024

var MinWalletPayoutThreshold = UsdToNanoCents(1.00)

// hold onto unpaid amounts for up to this time
const WalletPayoutTimeout = 15 * 24 * time.Hour

const ProviderRevenueShare = 0.5

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
    BalanceCodeId bringyour.Id
    StartTime time.Time
    CreateTime time.Time
    EndTime time.Time
    BalanceByteCount ByteCount
    NetRevenue NanoCents
    Secret string
    PurchaseRecord string
    PurchaseEmail string
}


func GetBalanceCodeIdForPurchaseEventId(ctx context.Context, purchaseEventId bringyour.Id) (balanceCodeId bringyour.Id, returnErr error) {
    bringyour.Raise(bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        result, err := tx.Query(
            ctx,
            `
                SELECT balance_code_id FROM transfer_balance_code
                WHERE purchase_event_id = $1
            `,
            balanceCodeId,
        )
        bringyour.WithPgResult(result, err, func() {
            if result.Next() {
                bringyour.Raise(result.Scan(&balanceCodeId))
            } else {
            	returnErr = fmt.Errorf("Purchase event not found.")
            }
        })
    }))
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
    bringyour.Raise(bringyour.Tx(ctx, func(tx bringyour.PgTx) {
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

        createTime := time.Now()
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
            BalanceCodeId: balanceCodeId,
            CreateTime: createTime,
            StartTime: startTime,
            EndTime: endTime,
            BalanceByteCount: balanceByteCount,
            NetRevenue: netRevenue,
            Secret: secret,
            PurchaseRecord: purchaseRecord,
            PurchaseEmail: purchaseEmail,
        }
    }))
    return
}


type RedeemBalanceCodeArgs struct {
    Secret string `json:"secret"`
}

type RedeemBalanceCodeResult struct {
    TransferBalance *RedeemBalanceCodeTransferBalance `json:"transfer_balance,omitempty"`
    Error *RedeemBalanceCodeError `json:"error,omitempty"`
}

type RedeemBalanceCodeTransferBalance struct {
    TransferBalanceId bringyour.Id `json:"transfer_balance_id"`
    StartTime time.Time `json:"start_time"`
    EndTime time.Time `json:"end_time"`
    BalanceByteCount ByteCount `json:"balance_byte_count"`
}

type RedeemBalanceCodeError struct {
    Message string `json:"message"`
}

func RedeemBalanceCode(
    redeemBalanceCode *RedeemBalanceCodeArgs,
    session *session.ClientSession,
) (redeemBalanceCodeResult *RedeemBalanceCodeResult, returnErr error) {
    bringyour.Raise(bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {        
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
            time.Now(),
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
                StartTime: balanceCode.StartTime,
                EndTime: balanceCode.EndTime,
                BalanceByteCount: balanceCode.BalanceByteCount,
            },
        }
    }, bringyour.TxSerializable))

    return
}


type CheckBalanceCodeArgs struct {
    Secret string `json:"secret"`
}

type CheckBalanceCodeResult struct {
    Balance *CheckBalanceCodeBalance `json:"balance,omitempty"`
    Error *CheckBalanceCodeError `json:"error,omitempty"`
}

type CheckBalanceCodeBalance struct {
    StartTime time.Time `json:"start_time"`
    EndTime time.Time `json:"end_time"`
    BalanceByteCount ByteCount `json:"balance_byte_count"`
}

type CheckBalanceCodeError struct {
    Message string `json:"message"`
}

func CheckBalanceCode(
    checkBalanceCode *CheckBalanceCodeArgs,
    session *session.ClientSession,
) (checkBalanceCodeResult *CheckBalanceCodeResult, returnErr error) {
    bringyour.Raise(bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {
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
                StartTime: balanceCode.StartTime,
                EndTime: balanceCode.EndTime,
                BalanceByteCount: balanceCode.BalanceByteCount,
            },
        }
    }, bringyour.TxSerializable))

    return
}


// FIXME add payment_token
type TransferBalance struct {
    BalanceId bringyour.Id `json:"balance_id"`
    NetworkId bringyour.Id `json:"network_id"`
    StartTime time.Time `json:"start_time"`
    EndTime time.Time `json:"end_time"`
    StartBalanceByteCount ByteCount `json:"start_balance_byte_count"`
    // how much money the platform made after subtracting fees
    NetRevenue NanoCents `json:"net_revenue_nano_cents"`
    BalanceByteCount ByteCount `json:"balance_byte_count"`
    PurchaseToken string `json:"purchase_token,omitempty"`
}


func GetActiveTransferBalances(ctx context.Context, networkId bringyour.Id) []*TransferBalance {
    now := time.Now()

    transferBalances := []*TransferBalance{}

    bringyour.Raise(bringyour.Db(ctx, func(conn bringyour.PgConn) {
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
    }))
    
    return transferBalances
}


func AddTransferBalance(ctx context.Context, transferBalance *TransferBalance) {
    bringyour.Raise(bringyour.Tx(ctx, func(tx bringyour.PgTx) {
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
    }))
}




// TODO GetLastTransferData returns the transfer data with
// 1. the given purhase record
// 2. that starte before and ends after sub.ExpiryTime
// TODO with the max end time
// TODO if none, return err
func GetOverlappingTransferBalance(ctx context.Context, purchaseToken string, expiryTime time.Time) (balanceId bringyour.Id, returnErr error) {
    bringyour.Raise(bringyour.Db(ctx, func(conn bringyour.PgConn) {
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
    }))

    return
}


type EscrowId struct {
    ContractId bringyour.Id
    BalanceId bringyour.Id
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
    ContractOutcomeSettled ContractOutcome = "settled"
    ContractOutcomeDisputeResolvedToSource ContractOutcome = "dispute_resolved_to_source"
    ContractOutcomeDisputeResolvedToDestination ContractOutcome = "dispute_resolved_to_destination"
)


type ContractParty = string

const (
    ContractPartySource ContractParty = "source"
    ContractPartyDestination ContractParty = "destination"
)


type TransferEscrow struct {
    ContractId bringyour.Id
    Balances []*TransferEscrowBalance
}


type TransferEscrowBalance struct {
    BalanceId bringyour.Id
    BalanceByteCount ByteCount
}


// FIXME support a third payerId and payerNetwork
func CreateTransferEscrow(
    ctx context.Context,
    sourceNetworkId bringyour.Id,
    sourceId bringyour.Id,
    destinationNetworkId bringyour.Id,
    destinationId bringyour.Id,
    contractTransferByteCount ByteCount,
) (transferEscrow *TransferEscrow, returnErr error) {
    bringyour.Raise(bringyour.Tx(ctx, func(tx bringyour.PgTx) {
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
            `,
            sourceNetworkId,
            time.Now(),
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
                escrowBalanceByteCount := min(contractTransferByteCount - netEscrowBalanceByteCount, balanceByteCount)
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

        bringyour.CreateTempJoinTableInTx(
            ctx,
            tx,
            "escrow(balance_id uuid -> balance_byte_count bigint)",
            escrow,
        )

        bringyour.RaisePgResult(tx.Exec(
            ctx,
            `
                UPDATE transfer_balance
                SET
                    balance_byte_count = transfer_balance.balance_byte_count - escrow.balance_byte_count
                FROM escrow
                WHERE
                    transfer_balance.balance_id = escrow.balance_id
            `,
        ))

        bringyour.RaisePgResult(tx.Exec(
            ctx,
            `
                INSERT INTO transfer_escrow (
                    contract_id,
                    balance_id,
                    balance_byte_count
                )
                SELECT
                    $1 AS contract_id,
                    balance_id,
                    balance_byte_count
                FROM escrow
            `,
            contractId,
        ))

        bringyour.RaisePgResult(tx.Exec(
            ctx,
            `
                INSERT INTO transfer_contract (
                    contract_id,
                    source_network_id,
                    source_id,
                    destination_network_id,
                    destination_id,
                    transfer_byte_count
                )
                VALUES ($1, $2, $3, $4, $5, $6)
            `,
            contractId,
            sourceNetworkId,
            sourceId,
            destinationNetworkId,
            destinationId,
            contractTransferByteCount,
        ))

        balances := []*TransferEscrowBalance{}
        for balanceId, escrowBalanceByteCount := range escrow {
            balance := &TransferEscrowBalance{
                BalanceId: balanceId,
                BalanceByteCount: escrowBalanceByteCount,
            }
            balances = append(balances, balance)
        }

        transferEscrow = &TransferEscrow{
            ContractId: contractId,
            Balances: balances,
        }
    }, bringyour.TxSerializable))

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
    contractTransferByteCount int,
) []bringyour.Id {
    contractIds := []bringyour.Id{}

    bringyour.Raise(bringyour.Db(ctx, func(conn bringyour.PgConn) {
        result, err := conn.Query(
            ctx,
            `
                SELECT
                    DISTINCT transfer_contract.contract_id
                FROM transfer_contract

                LEFT OUTER JOIN contract_close ON
                    contract_close.contract_id = transfer_contract.contract_id

                INNER JOIN transfer_escrow ON
                    transfer_escrow.contract_id = transfer_contract.contract_id

                WHERE
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
                bringyour.Raise(result.Scan(&contractId))
                contractIds = append(contractIds, contractId)
            }
        })
    }))
    
    return contractIds
}


func GetTransferEscrow(ctx context.Context, contractId bringyour.Id) (transferEscrow *TransferEscrow) {
    bringyour.Raise(bringyour.Db(ctx, func(conn bringyour.PgConn) {
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
            Balances: balances,
        }
    }))

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
    bringyour.Raise(bringyour.Tx(ctx, func(tx bringyour.PgTx) {
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
                    transfer_byte_count
                )
                VALUES ($1, $2, $3, $4, $5, $6)
            `,
            contractId,
            sourceNetworkId,
            sourceId,
            destinationNetworkId,
            destinationId,
            contractTransferByteCount,
        ))
    }))
    return
}


// this will create a close entry,
// then settle if all parties agree, or set dispute if there is a dispute
func CloseContract(
    ctx context.Context,
    contractId bringyour.Id,
    clientId bringyour.Id,
    usedTransferByteCount ByteCount,
) (returnErr error) {
    settle := false
    dispute := false

    bringyour.Raise(bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        var party ContractParty
        result, err := tx.Query(
            ctx,
            `
                SELECT
                    source_id,
                    destination_id
                FROM transfer_contract
                WHERE
                    contract_id = $1 AND
                    outcome IS NULL
            `,
            contractId,
        )
        bringyour.WithPgResult(result, err, func() {
            if result.Next() {
                var sourceId bringyour.Id
                var destinationId bringyour.Id
                bringyour.Raise(result.Scan(&sourceId, &destinationId))
                if clientId == sourceId {
                    party = ContractPartySource
                } else if clientId == destinationId {
                    party = ContractPartyDestination
                }
            }
        })

        if party == "" {
            returnErr = fmt.Errorf("Client is not a party to the contract.")
            return
        }

        // make sure the reported amount does not exceed the contract value
        bringyour.RaisePgResult(tx.Exec(
            ctx,
            `
                INSERT INTO contract_close (
                    contract_id,
                    party,
                    used_transfer_byte_count
                )
                SELECT
                    contract_id,
                    $2 AS party,
                    LEAST($3, transfer_byte_count) AS used_transfer_byte_count
                FROM transfer_contract
                WHERE
                    contract_id = $1
                ON CONFLICT (contract_id, party) DO UPDATE
                SET
                    used_transfer_byte_count = $3
            `,
            contractId,
            party,
            usedTransferByteCount,
        ))

        result, err = tx.Query(
            ctx,
            `
            SELECT
                party,
                used_transfer_byte_count
            FROM contract_close
            WHERE
                contract_id = $1
            `,
            contractId,
        )
        // party -> used transfer byte count
        closes := map[ContractParty]ByteCount{}
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
	            if math.Abs(float64(diff)) <= AcceptableTransfersByteDifference {
	                settle = true
	            } else {
	                dispute = true
	            }
	        } else {
	        	// nothing to settle, just close the transaction
	        	bringyour.RaisePgResult(tx.Exec(
		        	ctx,
		            `
		                UPDATE transfer_contract
		                SET
		                    outcome = $2
		                WHERE
		                    contract_id = $1
		            `,
		            contractId,
		            ContractOutcomeSettled,
		        ))
	        }
        }
    }))

    if returnErr != nil {
        return
    }

    if settle {
        returnErr = SettleEscrow(ctx, contractId, ContractOutcomeSettled)
    } else if dispute {
        SetContractDispute(ctx, contractId, true)
    }
    return
}


func SettleEscrow(ctx context.Context, contractId bringyour.Id, outcome ContractOutcome) (returnErr error) {
    bringyour.Raise(bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        var usedTransferByteCount ByteCount

        switch outcome {
        case ContractOutcomeSettled:
            result, err := tx.Query(
                ctx,
                `
                    SELECT
                        used_transfer_byte_count
                    FROM contract_close
                    WHERE
                        contract_id = $1
                `,
                contractId,
            )
            bringyour.WithPgResult(result, err, func() {
                netUsedTransferByteCount := ByteCount(0)
                partyCount := 0
                for result.Next() {
                    var usedTransferByteCountForParty ByteCount
                    bringyour.Raise(result.Scan(&usedTransferByteCountForParty))
                    netUsedTransferByteCount += usedTransferByteCountForParty
                    partyCount += 1
                }
                usedTransferByteCount = netUsedTransferByteCount / ByteCount(partyCount)
            })
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

                payoutByteCount := min(usedTransferByteCount - netPayoutByteCount, escrowBalanceByteCount)
                returnByteCount := escrowBalanceByteCount - payoutByteCount
                netPayoutByteCount += payoutByteCount
                payout := NanoCents(math.Round(
                    ProviderRevenueShare * float64(netRevenue) * float64(payoutByteCount) / float64(startBalanceByteCount),
                ))
                netPayout += payout
                sweepPayouts[balanceId] = sweepPayout{
                	payoutByteCount: payoutByteCount,
                	returnByteCount: returnByteCount,
                	payout: payout,
                }
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
                    destination_network_id
                FROM transfer_contract
                WHERE
                    contract_id = $1
            `,
            contractId,
        )
        bringyour.WithPgResult(result, err, func() {
            if result.Next() {
                bringyour.Raise(result.Scan(&payoutNetworkId))
            }
        })

        if payoutNetworkId == nil {
            err = fmt.Errorf("Destination client does not exist.")
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
                    outcome = $2
                WHERE
                    contract_id = $1
            `,
            contractId,
            ContractOutcomeSettled,
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
            time.Now(),
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
    }, bringyour.TxSerializable))

    return
}

type sweepPayout struct {
	payoutByteCount ByteCount
	returnByteCount ByteCount
	payout NanoCents
}

// `bringyour.ComplexValue`
func (self *sweepPayout) Values() []any {
	return []any{self.payoutByteCount, self.returnByteCount, self.payout}
}


func SetContractDispute(ctx context.Context, contractId bringyour.Id, dispute bool) {
    bringyour.Raise(bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        bringyour.RaisePgResult(tx.Exec(
            ctx,
            `
                UPDATE transfer_contract
                SET
                    dispute = $2
                WHERE
                    contract_id = $1 AND
                    outcome IS NULL
            `,
        ))
    }))
}


func GetOpenContractIds(
    ctx context.Context,
    sourceId bringyour.Id,
    destinationId bringyour.Id,
) []bringyour.Id {
    contractIds := []bringyour.Id{}

    bringyour.Raise(bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        result, err := tx.Query(
            ctx,
            `
                SELECT
                    contract_id
                FROM transfer_contract
                WHERE
                    open = true AND
                    source_id = $1 AND
                    destination_id = $2
            `,
            sourceId,
            destinationId,
        )
        bringyour.WithPgResult(result, err, func() {
            for result.Next() {
                var contractId bringyour.Id
                bringyour.Raise(result.Scan(&contractId))
                contractIds = append(contractIds, contractId)
            }
        })
    }))

    return contractIds
}


// return key is unordered transfer pair
func GetOpenContractIdsForSourceOrDestination(
    ctx context.Context,
    clientId bringyour.Id,
) map[TransferPair]map[bringyour.Id]bool {
    pairContractIds := map[TransferPair]map[bringyour.Id]bool{}

    bringyour.Raise(bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        result, err := tx.Query(
            ctx,
            `
                SELECT
                    source_id,
                    destination_id,
                    contract_id
                FROM transfer_contract
                WHERE
                    open = true AND (
                        source_id = $1 OR
                        destination_id = $1
                    )
            `,
            clientId,
        )
        bringyour.WithPgResult(result, err, func() {
            for result.Next() {
                var sourceId bringyour.Id
                var destinationId bringyour.Id
                var contractId bringyour.Id
                bringyour.Raise(result.Scan(
                    &sourceId,
                    &destinationId,
                    &contractId,
                ))
                transferPair := NewUnorderedTransferPair(sourceId, destinationId)
                contractIds, ok := pairContractIds[transferPair]
                if !ok {
                    contractIds = map[bringyour.Id]bool{}
                    pairContractIds[transferPair] = contractIds
                }
                contractIds[contractId] = true
            }
        })
    }))

    return pairContractIds
}


type WalletType = string
const (
    WalletTypeCircleUserControlled = "circle_uc"
    WalletTypeXch = "xch"
    WalletTypeSol = "sol"
)


type AccountWallet struct {
    WalletId bringyour.Id
    NetworkId bringyour.Id
    WalletType WalletType
    Blockchain string
    WalletAddress string
    Active bool
    DefaultTokenType string
    CreateTime time.Time
}


func dbGetAccountWallet(ctx context.Context, conn bringyour.PgConn, walletId bringyour.Id) *AccountWallet {
    var wallet *AccountWallet
    result, err := conn.Query(
        ctx,
        `
            SELECT
                network_id,
                wallet_type,
                blockchain,
                wallet_address,
                active,
                default_token_type,
                create_time
            FROM account_wallet
            WHERE
                wallet_id = $1
        `,
        walletId,
    )
    bringyour.WithPgResult(result, err, func() {
        if result.Next() {
            wallet = &AccountWallet{}
            bringyour.Raise(result.Scan(
                &wallet.NetworkId,
                &wallet.WalletType,
                &wallet.Blockchain,
                &wallet.WalletAddress,
                &wallet.Active,
                &wallet.DefaultTokenType,
                &wallet.CreateTime,
            ))
        }
    })
    return wallet
}


func GetAccountWallet(ctx context.Context, walletId bringyour.Id) *AccountWallet {
    var wallet *AccountWallet
    bringyour.Raise(bringyour.Db(ctx, func(conn bringyour.PgConn) {
        wallet = dbGetAccountWallet(ctx, conn, walletId)
    }))
    return wallet
}


func CreateAccountWallet(ctx context.Context, wallet *AccountWallet) {
    bringyour.Raise(bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        wallet.WalletId = bringyour.NewId()
        wallet.Active = true
        wallet.CreateTime = time.Now()

        bringyour.RaisePgResult(tx.Exec(
            ctx,
            `
                INSERT INTO account_wallet (
                    wallet_id,
                    network_id,
                    wallet_type,
                    blockchain,
                    wallet_address,
                    active,
                    default_token_type,
                    create_time
                )
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
            `,
            wallet.WalletId,
            wallet.NetworkId,
            wallet.WalletType,
            wallet.Blockchain,
            wallet.WalletAddress,
            wallet.Active,
            wallet.DefaultTokenType,
            wallet.CreateTime,
        ))
    }))
}


func FindActiveAccountWallets(
    ctx context.Context,
    networkId bringyour.Id,
    walletType WalletType,
    walletAddress string,
) []*AccountWallet {
    wallets := []*AccountWallet{}

    bringyour.Raise(bringyour.Db(ctx, func(conn bringyour.PgConn) {
        result, err := conn.Query(
            ctx,
            `
                SELECT
                    wallet_id
                FROM account_wallet
                WHERE
                    active = true AND
                    network_id = $1 AND
                    wallet_type = $2 AND
                    wallet_address = $3
            `,
            networkId,
            walletType,
            walletAddress,
        )
        walletIds := []bringyour.Id{}
        bringyour.WithPgResult(result, err, func() {
            for result.Next() {
                var walletId bringyour.Id
                bringyour.Raise(result.Scan(&walletId))
                walletIds = append(walletIds, walletId)
            }
        })

        for _, walletId := range walletIds {
            wallet := dbGetAccountWallet(ctx, conn, walletId)
            if wallet != nil && wallet.Active {
                wallets = append(wallets, wallet)
            }
        }
    }))

    return wallets
}


func SetPayoutWallet(ctx context.Context, networkId bringyour.Id, walletId bringyour.Id) {
    bringyour.Raise(bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        bringyour.RaisePgResult(tx.Exec(
            ctx,
            `
                INSERT INTO payout_wallet (
                    network_id,
                    wallet_id
                )
                VALUES ($1, $2)
                ON CONFLICT (network_id) DO UPDATE
                SET
                    wallet_id = $2
            `,
            networkId,
            walletId,
        ))
    }))
}


type GetAccountWalletsResult struct {
    Wallets []*AccountWallet
}

func GetActiveAccountWallets(ctx context.Context, session *session.ClientSession) *GetAccountWalletsResult {
    wallets := []*AccountWallet{}

    bringyour.Raise(bringyour.Db(ctx, func(conn bringyour.PgConn) {
        result, err := conn.Query(
            ctx,
            `
                SELECT
                    wallet_id
                FROM account_wallet
                WHERE
                    active = true AND
                    network_id = $1
            `,
            session.ByJwt.NetworkId,
        )
        walletIds := []bringyour.Id{}
        bringyour.WithPgResult(result, err, func() {
            for result.Next() {
                var walletId bringyour.Id
                bringyour.Raise(result.Scan(&walletId))
                walletIds = append(walletIds, walletId)
            }
        })

        for _, walletId := range walletIds {
            wallet := dbGetAccountWallet(ctx, conn, walletId)
            if wallet != nil && wallet.Active {
                wallets = append(wallets, wallet)
            }
        }
    }))

    return &GetAccountWalletsResult{
        Wallets: wallets,
    }
}


type AccountPayment struct {
    PaymentId bringyour.Id
    PaymentPlanId bringyour.Id
    WalletId bringyour.Id
    PayoutByteCount ByteCount
    Payout NanoCents
    MinSweepTime time.Time
    CreateTime time.Time

    PaymentRecord string
    TokenType string
    TokenAmount float64
    PaymentTime time.Time
    PaymentReceipt string

    Completed bool
    CompleteTime time.Time

    Canceled bool
    CancelTime time.Time
}


func dbGetPayment(ctx context.Context, conn bringyour.PgConn, paymentId bringyour.Id) *AccountPayment {
    var payment *AccountPayment
    result, err := conn.Query(
        ctx,
        `
            SELECT
                payment_plan_id,
                wallet_id,
                payout_byte_count,
                payout_nano_cents,
                min_sweep_time,
                create_time,
                payment_record,
                token_type,
                token_amount,
                payment_time,
                payment_receipt,
                completed,
                complete_time,
                canceled,
                cancel_time
            FROM account_payment
            WHERE
                payment_id = $1
        `,
        paymentId,
    )
    bringyour.WithPgResult(result, err, func() {
        if result.Next() {
            payment = &AccountPayment{}
            bringyour.Raise(result.Scan(
                &payment.PaymentPlanId,
                &payment.WalletId,
                &payment.PayoutByteCount,
                &payment.Payout,
                &payment.MinSweepTime,
                &payment.CreateTime,
                &payment.PaymentRecord,
                &payment.TokenType,
                &payment.TokenAmount,
                &payment.PaymentTime,
                &payment.PaymentReceipt,
                &payment.Completed,
                &payment.CompleteTime,
                &payment.Canceled,
                &payment.CancelTime,
            ))
        }
    })
    return payment
}


func GetPayment(ctx context.Context, paymentId bringyour.Id) *AccountPayment {
    var payment *AccountPayment
    bringyour.Raise(bringyour.Db(ctx, func(conn bringyour.PgConn) {
        payment = dbGetPayment(ctx, conn, paymentId)
    }))
    return payment
}


func GetPendingPayments(ctx context.Context) []*AccountPayment {
    payments := []*AccountPayment{}

    bringyour.Raise(bringyour.Db(ctx, func(conn bringyour.PgConn) {
        result, err := conn.Query(
            ctx,
            `
                SELECT
                    payment_id
                FROM account_payment
                WHERE
                    NOT completed AND NOT canceled
            `,
        )
        paymentIds := []bringyour.Id{}
        bringyour.WithPgResult(result, err, func() {
            var paymentId bringyour.Id
            bringyour.Raise(result.Scan(&paymentId))
            paymentIds = append(paymentIds, paymentId)
        })

        for _, paymentId := range paymentIds {
            payment := dbGetPayment(ctx, conn, paymentId)
            if payment != nil {
                payments = append(payments, payment)
            }
        }
    }))
    
    return payments
}


func GetPendingPaymentsInPlan(ctx context.Context, paymentPlanId bringyour.Id) []*AccountPayment {
    payments := []*AccountPayment{}

    bringyour.Raise(bringyour.Db(ctx, func(conn bringyour.PgConn) {
        result, err := conn.Query(
            ctx,
            `
                SELECT
                    payment_id
                FROM account_payment
                WHERE
                    payment_plan_id = $1 AND
                    NOT completed AND NOT canceled
            `,
            paymentPlanId,
        )
        paymentIds := []bringyour.Id{}
        bringyour.WithPgResult(result, err, func() {
            var paymentId bringyour.Id
            bringyour.Raise(result.Scan(&paymentId))
            paymentIds = append(paymentIds, paymentId)
        })

        for _, paymentId := range paymentIds {
            payment := dbGetPayment(ctx, conn, paymentId)
            if payment != nil {
                payments = append(payments, payment)
            }
        }
    }))
    
    return payments
}


type PaymentPlan struct {
    PaymentPlanId bringyour.Id
    // wallet_id -> payment
    WalletPayments map[bringyour.Id]*AccountPayment
    // these wallets have pending payouts but were not paid due to thresholds or other rules
    WithheldWalletIds []bringyour.Id
}


// plan, manually check out and add balance to funding account, then complete
// minimum net_revenue_nano_cents to include in a payout
// all of the returned payments are tagged with the same payment_plan_id
func PlanPayments(ctx context.Context) *PaymentPlan {
    var paymentPlan *PaymentPlan
    bringyour.Raise(bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        result, err := tx.Query(
            ctx,
            `
            SELECT
                transfer_escrow_sweep.contract_id,
                transfer_escrow_sweep.balance_id,
                transfer_escrow_sweep.payout_byte_count,
                transfer_escrow_sweep.payout_net_revenue_nano_cents,
                transfer_escrow_sweep.sweep_time,
                payout_wallet.wallet_id

            FROM transfer_escrow_sweep

            LEFT JOIN account_payment ON
                account_payment.payment_id = transfer_escrow_sweep.payment_id

            INNER JOIN payout_wallet ON
                payout_wallet.network_id = transfer_escrow_sweep.network_id

            INNER JOIN account_wallet ON
            	account_wallet.wallet_id = payout_wallet.wallet_id AND
            	account_wallet.active = true

            WHERE
                account_payment.payment_id IS NULL OR
                NOT account_payment.completed AND account_payment.canceled
            `,
        )
        paymentPlanId := bringyour.NewId()
        // walletId -> AccountPayment
        walletPayments := map[bringyour.Id]*AccountPayment{}
        // escrow ids -> payment id
        escrowPaymentIds := map[EscrowId]bringyour.Id{}
        bringyour.WithPgResult(result, err, func() {
            for result.Next() {
                var contractId bringyour.Id
                var balanceId bringyour.Id
                var payoutByteCount ByteCount
                var payoutNetRevenue NanoCents
                var sweepTime time.Time
                var walletId bringyour.Id
                bringyour.Raise(result.Scan(
                    &contractId,
                    &balanceId,
                    &payoutByteCount,
                    &payoutNetRevenue,
                    &sweepTime,
                    &walletId,
                ))

                payment, ok := walletPayments[walletId]
                if !ok {
                    paymentId := bringyour.NewId()
                    payment = &AccountPayment{
                        PaymentId: paymentId,
                        PaymentPlanId: paymentPlanId,
                        WalletId: walletId,
                        CreateTime: time.Now(),
                    }
                    walletPayments[walletId] = payment
                }
                payment.PayoutByteCount += payoutByteCount
                payment.Payout += payoutNetRevenue

                if payment.MinSweepTime.IsZero() {
                    payment.MinSweepTime = sweepTime
                } else {
                    payment.MinSweepTime = bringyour.MinTime(payment.MinSweepTime, sweepTime)
                }

                escrowId := EscrowId{
                    ContractId: contractId,
                    BalanceId: balanceId,
                }
                escrowPaymentIds[escrowId] = payment.PaymentId
            }
        })

        // apply wallet minimum payout threshold
        // any wallet that does not meet the threshold will not be included in this plan
        walletIdsToRemove := []bringyour.Id{}
        payoutExpirationTime := time.Now().Add(-WalletPayoutTimeout)
        for walletId, payment := range walletPayments {
            // cannot remove payments that have `MinSweepTime <= payoutExpirationTime`
            if payment.Payout < MinWalletPayoutThreshold && payoutExpirationTime.Before(payment.MinSweepTime) {
                walletIdsToRemove = append(walletIdsToRemove, walletId)
            }
        }
        for _, walletId := range walletIdsToRemove {
            delete(walletPayments, walletId)
        }

        bringyour.Raise(bringyour.BatchInTx(ctx, tx, func(batch bringyour.PgBatch) {
            for _, payment := range walletPayments {
                batch.Queue(
                    `
                        INSERT INTO account_payment (
                            payment_id,
                            payment_plan_id,
                            wallet_id,
                            payout_byte_count,
                            payout_nano_cents,
                            min_sweep_time,
                            create_time
                        )
                        VALUES ($1, $2, $3, $4, $5, $6, $7)
                    `,
                    payment.PaymentId,
                    payment.PaymentPlanId,
                    payment.WalletId,
                    payment.PayoutByteCount,
                    payment.Payout,
                    payment.MinSweepTime,
                    payment.CreateTime,
                )
            }
        }))

        bringyour.CreateTempJoinTableInTx(
            ctx,
            tx,
            "payment_escrow_ids(contract_id uuid, balance_id uuid -> payment_id uuid)",
            escrowPaymentIds,
        )

        bringyour.RaisePgResult(tx.Exec(
            ctx,
            `
                UPDATE transfer_escrow_sweep
                SET
                    payment_id = payment_escrow_ids.payment_id
                FROM payment_escrow_ids
                WHERE
                    transfer_escrow_sweep.contract_id = payment_escrow_ids.contract_id AND
                    transfer_escrow_sweep.balance_id = payment_escrow_ids.balance_id
            `,
        ))

        paymentPlan = &PaymentPlan{
            PaymentPlanId: paymentPlanId,
            WalletPayments: walletPayments,
            WithheldWalletIds: walletIdsToRemove,
        }
    }, bringyour.TxSerializable))

    return paymentPlan
}


// set the record before submitting to the processor
// the controller should check if the payment already has a record before processing - 
//    these are in a bad state and need to be investigated manually
func SetPaymentRecord(
    ctx context.Context,
    paymentId bringyour.Id,
    tokenType string,
    tokenAmount float64,
    paymentRecord string,
) (returnErr error) {
    bringyour.Raise(bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        tag := bringyour.RaisePgResult(tx.Exec(
            ctx,
            `
                UPDATE account_payment
                SET
                    token_type = $2,
                    token_amount = $3,
                    payment_record = $4
                WHERE
                    payment_id = $1 AND
                    NOT completed AND NOT canceled
            `,
            paymentId,
            tokenType,
            tokenAmount,
            paymentRecord,
        ))
        if tag.RowsAffected() != 1 {
            returnErr = fmt.Errorf("Invalid payment.")
            return 
        }
    }))
    return
}


func CompletePayment(ctx context.Context, paymentId bringyour.Id, paymentReceipt string) (returnErr error) {
    bringyour.Raise(bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        tag := bringyour.RaisePgResult(tx.Exec(
            ctx,
            `
                UPDATE account_payment
                SET
                    payment_receipt = $2,
                    completed = true,
                    complete_time = $3
                WHERE
                    payment_id = $1 AND
                    NOT completed AND NOT canceled
            `,
            paymentId,
            paymentReceipt,
            time.Now(),
        ))
        if tag.RowsAffected() != 1 {
            returnErr = fmt.Errorf("Invalid payment.")
            return
        }

        bringyour.RaisePgResult(tx.Exec(
            ctx,
            `
                UPDATE account_balance
                SET
                    paid_byte_count = paid_byte_count + account_payment.payout_byte_count,
                    paid_net_revenue_nano_cents = paid_net_revenue_nano_cents + account_payment.payout_nano_cents
                FROM account_payment, account_wallet
                WHERE
                    account_payment.payment_id = $1 AND
                    account_wallet.wallet_id = account_payment.wallet_id AND
                    account_balance.network_id = account_wallet.network_id
            `,
            paymentId,
        ))
    }))
    return
}


func CancelPayment(ctx context.Context, paymentId bringyour.Id) (returnErr error) {
    bringyour.Raise(bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        tag := bringyour.RaisePgResult(tx.Exec(
            ctx,
            `
                UPDATE account_payment
                SET
                    canceled = true,
                    cancel_time = $2
                WHERE
                    payment_id = $1 AND
                    NOT completed AND NOT canceled
            `,
            paymentId,
            time.Now(),
        ))
        if tag.RowsAffected() != 1 {
            returnErr = fmt.Errorf("Invalid payment.")
            return 
        }
    }))
    return
}


type AccountBalance struct {
    NetworkId bringyour.Id
    ProvidedByteCount ByteCount
    ProvidedNetRevenue NanoCents
    PaidByteCount ByteCount
    PaidNetRevenue NanoCents
}


type GetAccountBalanceResult struct {
    Balance *AccountBalance
    Error *GetAccountBalanceError
}

type GetAccountBalanceError struct {
    Message string
}

func GetAccountBalance(session *session.ClientSession) *GetAccountBalanceResult {
    getAccountBalanceResult := &GetAccountBalanceResult{}
    bringyour.Raise(bringyour.Db(session.Ctx, func(conn bringyour.PgConn) {
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
    }))
    return getAccountBalanceResult
}


type Subscription struct {
    SubscriptionId bringyour.Id `json:"subscription_id"`
    Store string `json:"store"`
    Plan string `json:"plan"`
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
    SubscriptionPaymentId bringyour.Id
    Error *SubscriptionCreatePaymentIdError
}

type SubscriptionCreatePaymentIdError struct {
    Message string
}

func SubscriptionCreatePaymentId(createPaymentId *SubscriptionCreatePaymentIdArgs, clientSession *session.ClientSession) (createPaymentIdResult *SubscriptionCreatePaymentIdResult, returnErr error) {
    bringyour.Raise(bringyour.Tx(clientSession.Ctx, func(tx bringyour.PgTx) {
        result, err := tx.Query(
            clientSession.Ctx,
            `
            SELECT
                COUNT(subscription_payment_id) AS subscription_payment_id_count
            FROM subscription_payment_id
            WHERE
                network_id = $1 AND
                $2 <= create_time
            `,
            clientSession.ByJwt.NetworkId,
            time.Now().Add(-1 * time.Hour),
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
    }))

    return
}


func SubscriptionGetNetworkIdForPaymentId(ctx context.Context, subscriptionPaymentId bringyour.Id) (networkId bringyour.Id, returnErr error) {
    bringyour.Raise(bringyour.Db(ctx, func(conn bringyour.PgConn) {
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
    }))
    return
}


