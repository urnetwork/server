package model


import (
    "context"
    "time"
    "fmt"
    "math"
    "crypto/rand"
    "encoding/hex"

    // "golang.org/x/exp/maps"
    // "golang.org/x/exp/slices"

    "bringyour.com/bringyour"
    "bringyour.com/bringyour/session"
)


type NanoCents = uint64

func USDToNanoCents(usd float64) NanoCents {
    return NanoCents(math.Round(usd * float64(1000000000)))
}

func NanoCentsToUSD(nanoCents NanoCents) float64 {
    return float64(nanoCents) / float64(1000000000)
}


// 1 year
const BalanceCodeDuration = 365 * 24 * time.Hour

// up to 32MiB
const AcceptableTransfersByteDifference = 32 * 1024 * 1024

var MinWalletPayoutThreshold = USDToNanoCents(1.00)

// hold onto unpaid amounts for up to this time
const WalletPayoutTimeout = 15 * 24 * time.Hour

const ProviderRevenueShare = 0.5


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
    BalanceBytes int
    NetRevenue NanoCents
    Secret string
}


// typically called from a payment webhook
func CreateBalanceCode(
    ctx context.Context,
    balanceBytes int,
    netRevenue NanoCents,
) *BalanceCode {
    var balanceCode *BalanceCode

    bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        balanceCodeId := bringyour.NewId()

        b := make([]byte, 48)
        _, err := rand.Read(b)
        if err != nil {
            panic(err)
        }
        secret := hex.EncodeToString(b)

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

        tx.Exec(
            ctx,
            `
                INSERT INTO transfer_balance_code (
                    balance_code_id,
                    create_time,
                    start_time,
                    end_time,
                    balance_bytes,
                    net_revenue_nano_cents,
                    balance_code_secret
                )
                VALUES ($1, $2, $3, $4, $5, $6, $7)
            `,
            balanceCodeId,
            createTime,
            startTime,
            endTime,
            balanceBytes,
            netRevenue,
            secret,
        )

        balanceCode = &BalanceCode{
            BalanceCodeId: balanceCodeId,
            CreateTime: createTime,
            StartTime: startTime,
            EndTime: endTime,
            BalanceBytes: balanceBytes,
            NetRevenue: netRevenue,
            Secret: secret,
        }
    })

    return balanceCode
}


type RedeemBalanceCodeArgs struct {
    Secret string
}

type RedeemBalanceCodeResult struct {
    TransferBalance *RedeemBalanceCodeTransferBalance
    Error *RedeemBalanceCodeError
}

type RedeemBalanceCodeTransferBalance struct {
    TransferBalanceId bringyour.Id
    StartTime time.Time
    EndTime time.Time
    BalanceBytes int
}

type RedeemBalanceCodeError struct {
    Message string
}

func RedeemBalanceCode(redeemBalanceCode *RedeemBalanceCodeArgs, session *session.ClientSession) *RedeemBalanceCodeResult {
    redeemBalanceCodeResult := &RedeemBalanceCodeResult{}

    bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {        
        result, err := tx.Query(
            session.Ctx,
            `
                SELECT 
                    balance_code_id,
                    start_time,
                    end_time,
                    balance_bytes,
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
                    &balanceCode.BalanceBytes,
                    &balanceCode.NetRevenue,
                ))
            }   
        })
        if balanceCode == nil {
            redeemBalanceCodeResult.Error = &RedeemBalanceCodeError{
                Message: "Unknown balance code.",
            }
            return
        }

        balanceId := bringyour.NewId()

        tx.Exec(
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
        )

        tx.Exec(
            session.Ctx,
            `
                INSERT INTO transfer_balance (
                    balance_id,
                    network_id,
                    start_time,
                    end_time,
                    start_balance_bytes,
                    balance_bytes,
                    net_revenue_nano_cents
                )
                VALUES ($1, $2, $3, $4, $5, $5, $6)
            `,
            balanceId,
            session.ByJwt.NetworkId,
            balanceCode.StartTime,
            balanceCode.EndTime,
            balanceCode.BalanceBytes,
            balanceCode.NetRevenue,
        )

        redeemBalanceCodeResult.TransferBalance = &RedeemBalanceCodeTransferBalance{
            TransferBalanceId: balanceId,
            StartTime: balanceCode.StartTime,
            EndTime: balanceCode.EndTime,
            BalanceBytes: balanceCode.BalanceBytes,
        }
    }, bringyour.TxSerializable)

    return redeemBalanceCodeResult
}


type CheckBalanceCodeArgs struct {
    Secret string
}

type CheckBalanceCodeResult struct {
    Balance *CheckBalanceCodeBalance
    Error *CheckBalanceCodeError
}

type CheckBalanceCodeBalance struct {
    StartTime time.Time
    EndTime time.Time
    BalanceBytes int
}

type CheckBalanceCodeError struct {
    Message string
}

func CheckBalanceCode(checkBalanceCode *CheckBalanceCodeArgs, session *session.ClientSession) *CheckBalanceCodeResult {
    checkBalanceCodeResult := &CheckBalanceCodeResult{}

    bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {
        var balanceCode *BalanceCode

        result, err := tx.Query(
            session.Ctx,
            `
                SELECT 
                    balance_code_id,
                    start_time,
                    end_time,
                    balance_bytes,
                    net_revenue_nano_cents
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
                    &balanceCode.BalanceBytes,
                    &balanceCode.NetRevenue,
                ))
            }   
        })

        if balanceCode == nil {
            checkBalanceCodeResult.Error = &CheckBalanceCodeError{
                Message: "Unknown balance code.",
            }
            return
        }

        checkBalanceCodeResult.Balance = &CheckBalanceCodeBalance{
            StartTime: balanceCode.StartTime,
            EndTime: balanceCode.EndTime,
            BalanceBytes: balanceCode.BalanceBytes,
        }
    }, bringyour.TxSerializable)

    return checkBalanceCodeResult
}


type TransferBalance struct {
    BalanceId bringyour.Id
    NetworkId bringyour.Id
    StartTime time.Time
    EndTime time.Time
    StartBalanceBytes int
    BalanceBytes int
    // how much money the platform made after subtracting fees
    NetRevenue NanoCents
}


func GetActiveTransferBalances(ctx context.Context, networkId bringyour.Id) []*TransferBalance {
    now := time.Now()

    transferBalances := []*TransferBalance{}

    bringyour.Db(ctx, func(conn bringyour.PgConn) {
        result, err := conn.Query(
            ctx,
            `
                SELECT
                    balance_id,
                    start_time,
                    end_time,
                    start_balance_bytes,
                    balance_bytes,
                    net_revenue_nano_cents
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
                    &transferBalance.StartBalanceBytes,
                    &transferBalance.BalanceBytes,
                    &transferBalance.NetRevenue,
                ))
                transferBalances = append(transferBalances, transferBalance)
            }
        })
    })
    
    return transferBalances
}


func AddTransferBalance(ctx context.Context, transferBalance *TransferBalance) {
    bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        balanceId := bringyour.NewId()

        tx.Exec(
            ctx,
            `
                INSERT INTO transfer_balance (
                    balance_id,
                    network_id,
                    start_time,
                    end_time,
                    start_balance_bytes,
                    balance_bytes,
                    net_revenue_nano_cents
                )
                VALUES ($1, $2, $3, $4, $5, $5, $6, $7)
            `,
            balanceId,
            transferBalance.NetworkId,
            transferBalance.StartTime,
            transferBalance.EndTime,
            transferBalance.StartBalanceBytes,
            transferBalance.BalanceBytes,
            transferBalance.NetRevenue,
        )

        transferBalance.BalanceId = balanceId
    })
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
    BalanceBytes int
}


func CreateTransferEscrow(
    ctx context.Context,
    sourceNetworkId bringyour.Id,
    sourceId bringyour.Id,
    destinationNetworkId bringyour.Id,
    destinationId bringyour.Id,
    contractTransferBytes int,
) (transferEscrow *TransferEscrow, returnErr error) {
    bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        contractId := bringyour.NewId()

        // order balances by end date, ascending
        // take from the earlier before the later
        result, err := tx.Query(
            ctx,
            `
                SELECT
                    balance_id,
                    balance_bytes
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
        // add up the balance_bytes until >= contractTransferBytes
        // if not enough, error
        escrow := map[bringyour.Id]int{}
        netEscrowBalanceBytes := 0
        bringyour.WithPgResult(result, err, func() {
            for result.Next() {
                var balanceId bringyour.Id
                var balanceBytes int
                bringyour.Raise(result.Scan(
                    &balanceId,
                    &balanceBytes,
                ))
                escrowBalanceBytes := min(contractTransferBytes - netEscrowBalanceBytes, balanceBytes)
                escrow[balanceId] = escrowBalanceBytes
                netEscrowBalanceBytes += escrowBalanceBytes
                if netEscrowBalanceBytes <= contractTransferBytes {
                    break
                }
            }
        })

        if netEscrowBalanceBytes < contractTransferBytes {
            returnErr = fmt.Errorf("Insufficient balance.")
            return
        }

        bringyour.CreateTempJoinTableInTx(
            ctx,
            tx,
            "escrow(balance_id uuid -> balance_bytes int)",
            escrow,
        )

        tx.Exec(
            ctx,
            `
                UPDATE transfer_balance
                SET
                    balance_bytes = transfer_balance.balance_bytes - escrow.balance_bytes
                FROM escrow
                WHERE
                    transfer_balance.balance_id = escrow.balance_id
            `,
        )

        tx.Exec(
            ctx,
            `
                INSERT INTO transfer_escrow (
                    contract_id,
                    balance_id,
                    balance_bytes
                )
                SELECT
                    $1 AS contract_id,
                    balance_id,
                    balance_bytes
                FROM escrow
            `,
            contractId,
        )

        tx.Exec(
            ctx,
            `
                INSERT INTO transfer_contract (
                    contract_id,
                    source_network_id,
                    source_id,
                    destination_network_id,
                    destination_id,
                    transfer_bytes
                )
                VALUES ($1, $2, $3, $4, $5, $6)
            `,
            contractId,
            sourceNetworkId,
            sourceId,
            destinationNetworkId,
            destinationId,
            contractTransferBytes,
        )

        balances := []*TransferEscrowBalance{}
        for balanceId, escrowBalanceBytes := range escrow {
            balance := &TransferEscrowBalance{
                BalanceId: balanceId,
                BalanceBytes: escrowBalanceBytes,
            }
            balances = append(balances, balance)
        }

        transferEscrow = &TransferEscrow{
            ContractId: contractId,
            Balances: balances,
        }
    }, bringyour.TxSerializable)

    return
}


func GetTransferEscrow(ctx context.Context, contractId bringyour.Id) (transferEscrow *TransferEscrow) {
    bringyour.Db(ctx, func(conn bringyour.PgConn) {
        result, err := conn.Query(
            ctx,
            `
                SELECT
                    balance_id,
                    balance_bytes

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
                    &balance.BalanceBytes,
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
    })

    return
}


// this will create a close entry,
// then settle if all parties agree, or set dispute if there is a dispute
func CloseContract(
    ctx context.Context,
    contractId bringyour.Id,
    clientId bringyour.Id,
    usedTransferBytes int,
) (returnErr error) {
    settle := false
    dispute := false

    bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        var party ContractParty
        result, err := tx.Query(
            ctx,
            `
                SELECT
                    source_id,
                    destination_id
                FROM transfer_contract
                WHERE
                    contract_id = $1
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
        tx.Exec(
            ctx,
            `
                INSERT INTO contract_close (
                    contract_id,
                    party,
                    used_transfer_bytes
                )
                SELECT
                    contract_id,
                    $2 AS party,
                    LEAST($3, transfer_bytes) AS used_transfer_bytes
                FROM transfer_contract
                WHERE
                    contract_id = $1
                ON CONFLICT (contract_id, party) DO UPDATE
                SET
                    used_transfer_bytes = $3
            `,
            contractId,
            party,
            usedTransferBytes,
        )

        result, err = tx.Query(
            ctx,
            `
            SELECT
                party,
                used_transfer_bytes
            FROM contract_close
            WHERE
                contract_id = $1
            `,
            contractId,
        )
        // party -> used transfer bytes
        closes := map[ContractParty]int{}
        bringyour.WithPgResult(result, err, func() {
            for result.Next() {
                var closeParty ContractParty
                var closeUsedTransferBytes int
                bringyour.Raise(result.Scan(
                    &closeParty,
                    &closeUsedTransferBytes,
                ))
                closes[closeParty] = closeUsedTransferBytes
            }
        })

        sourceUsedTransferBytes, sourceOk := closes[ContractPartySource]
        destinationUsedTransferBytes, destinationOk := closes[ContractPartyDestination]

        if sourceOk && destinationOk {
            diff := sourceUsedTransferBytes - destinationUsedTransferBytes
            if math.Abs(float64(diff)) <= AcceptableTransfersByteDifference {
                settle = true
            } else {
                dispute = true
            }
        }
    })

    if returnErr != nil {
        return
    }

    if settle {
        SettleEscrow(ctx, contractId, ContractOutcomeSettled)
    } else if dispute {
        SetContractDispute(ctx, contractId, true)
    }
    return
}


func SettleEscrow(ctx context.Context, contractId bringyour.Id, outcome ContractOutcome) (returnErr error) {
    bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        var usedTransferBytes int

        switch outcome {
        case ContractOutcomeSettled:
            result, err := tx.Query(
                ctx,
                `
                    SELECT
                        used_transfer_bytes
                    FROM contract_close
                    WHERE
                        contract_id = $1
                `,
                contractId,
            )
            bringyour.WithPgResult(result, err, func() {
                netUsedTransferBytes := 0
                partyCount := 0
                for result.Next() {
                    var usedTransferBytesForParty int
                    bringyour.Raise(result.Scan(&usedTransferBytesForParty))
                    netUsedTransferBytes += usedTransferBytesForParty
                    partyCount += 1
                }
                usedTransferBytes = netUsedTransferBytes / partyCount
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
                        used_transfer_bytes
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
                    bringyour.Raise(result.Scan(&usedTransferBytes))
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
                    transfer_escrow.balance_bytes,
                    transfer_balance.start_balance_bytes,
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

        // balance id -> payout bytes
        sweep := map[bringyour.Id]int{}
        netPayoutBytes := 0

        // balance id -> payout net revenue
        sweepNetRevenue := map[bringyour.Id]NanoCents{}
        var netPayout NanoCents = 0

        bringyour.WithPgResult(result, err, func() {
            for result.Next() {
                var balanceId bringyour.Id
                var escrowBalanceBytes int
                var startBalanceBytes int
                var netRevenue NanoCents
                bringyour.Raise(result.Scan(
                    &balanceId,
                    &escrowBalanceBytes,
                    &startBalanceBytes,
                    &netRevenue,
                ))

                payoutBytes := min(usedTransferBytes - netPayoutBytes, escrowBalanceBytes)
                netPayoutBytes += payoutBytes
                sweep[balanceId] = payoutBytes
                payout := NanoCents(math.Round(
                    ProviderRevenueShare * float64(netRevenue) * float64(payoutBytes) / float64(startBalanceBytes),
                ))
                netPayout += payout
                sweepNetRevenue[balanceId] = payout
            }
        })

        if len(sweep) == 0 {
            returnErr = fmt.Errorf("Invalid contract.")
            return
        }

        if netPayoutBytes < usedTransferBytes {
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
            "sweep(balance_id uuid -> payout_bytes bigint)",
            sweep,
        )

        bringyour.CreateTempJoinTableInTx(
            ctx,
            tx,
            "sweep_net_revenue(balance_id uuid -> payout_net_revenue_nano_cents bigint)",
            sweepNetRevenue,
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
                    payout_bytes = sweep.payout_bytes
                FROM sweep
                WHERE
                    transfer_escrow.contract_id = $1 AND
                    transfer_escrow.balance_id = sweep.balance_id
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
                    payout_bytes,
                    payout_net_revenue_nano_cents
                )
                SELECT
                    $1 AS contract_id,
                    sweep.balance_id,
                    $2 AS network_id,
                    sweep.payout_bytes,
                    sweep_net_revenue.payout_net_revenue_nano_cents
                FROM sweep

                INNER JOIN sweep_net_revenue ON
                    sweep_net_revenue.balance_id = sweep.balance_id

                WHERE
                    0 < sweep.payout_bytes
            `,
            contractId,
            payoutNetworkId,
        ))

        bringyour.RaisePgResult(tx.Exec(
        	ctx,
            `
            INSERT INTO account_balance (
            	network_id,
                provided_bytes,
                provided_net_revenue_nano_cents
            )
            VALUES ($1, $2, $3)
            ON CONFLICT (network_id) DO UPDATE
            SET
                provided_bytes = account_balance.provided_bytes + $2,
                provided_net_revenue_nano_cents = account_balance.provided_net_revenue_nano_cents + $3
            `,
            payoutNetworkId,
            netPayoutBytes,
            netPayout,
        ))
    }, bringyour.TxSerializable)

    return
}


func SetContractDispute(ctx context.Context, contractId bringyour.Id, dispute bool) {
    bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        tx.Exec(
            ctx,
            `
                UPDATE transfer_contract
                SET
                    dispute = $2
                WHERE
                    contract_id = $1 AND
                    outcome IS NULL
            `,
        )
    })
}


func GetOpenContractIds(
    ctx context.Context,
    sourceId bringyour.Id,
    destinationId bringyour.Id,
) []bringyour.Id {
    contractIds := []bringyour.Id{}

    bringyour.Tx(ctx, func(tx bringyour.PgTx) {
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
    })

    return contractIds
}


// return key is unordered transfer pair
func GetOpenContractIdsForSourceOrDestination(
    ctx context.Context,
    clientId bringyour.Id,
) map[TransferPair]map[bringyour.Id]bool {
    pairContractIds := map[TransferPair]map[bringyour.Id]bool{}

    bringyour.Tx(ctx, func(tx bringyour.PgTx) {
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
    })

    return pairContractIds
}


type WalletType = string
const (
    WalletTypeCircleUsdcMatic = "circle_usdc_matic"
    WalletTypeXch = "xch"
    WalletTypeSol = "sol"
)


type AccountWallet struct {
    WalletId bringyour.Id
    NetworkId bringyour.Id
    WalletType WalletType
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
    bringyour.Db(ctx, func(conn bringyour.PgConn) {
        wallet = dbGetAccountWallet(ctx, conn, walletId)
    })
    return wallet
}


func CreateAccountWallet(ctx context.Context, wallet *AccountWallet) {
    bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        wallet.WalletId = bringyour.NewId()
        wallet.Active = true
        wallet.CreateTime = time.Now()

        tx.Exec(
            ctx,
            `
                INSERT INTO account_wallet (
                    wallet_id,
                    network_id,
                    wallet_type,
                    wallet_address,
                    active,
                    default_token_type,
                    create_time
                )
                VALUES ($1, $2, $3, $4, $5, $6)
            `,
            wallet.WalletId,
            wallet.NetworkId,
            wallet.WalletType,
            wallet.WalletAddress,
            wallet.Active,
            wallet.DefaultTokenType,
            wallet.CreateTime,
        )
    })
}


func FindActiveAccountWallets(
    ctx context.Context,
    networkId bringyour.Id,
    walletType WalletType,
    walletAddress string,
) []*AccountWallet {
    wallets := []*AccountWallet{}

    bringyour.Db(ctx, func(conn bringyour.PgConn) {
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
    })

    return wallets
}


func SetPayoutWallet(ctx context.Context, networkId bringyour.Id, walletId bringyour.Id) {
    bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        tx.Exec(
            ctx,
            `
                INSERT INTO payout_wallet (
                    network_id,
                    wallet_id
                )
                VALUES ($1, $2)
                ON CONFLICT DO UPDATE
                SET
                    wallet_id = $2
            `,
            networkId,
            walletId,
        )
    })
}


type GetAccountWalletsResult struct {
    Wallets []*AccountWallet
}

func GetActiveAccountWallets(ctx context.Context, session *session.ClientSession) *GetAccountWalletsResult {
    wallets := []*AccountWallet{}

    bringyour.Db(ctx, func(conn bringyour.PgConn) {
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
    })

    return &GetAccountWalletsResult{
        Wallets: wallets,
    }
}


type AccountPayment struct {
    PaymentId bringyour.Id
    PaymentPlanId bringyour.Id
    WalletId bringyour.Id
    PayoutBytes int
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
                payout_bytes,
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
                &payment.PayoutBytes,
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
    bringyour.Db(ctx, func(conn bringyour.PgConn) {
        payment = dbGetPayment(ctx, conn, paymentId)
    })
    return payment
}


func GetPendingPayments(ctx context.Context) []*AccountPayment {
    payments := []*AccountPayment{}

    bringyour.Db(ctx, func(conn bringyour.PgConn) {
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
    })
    
    return payments
}


func GetPendingPaymentsInPlan(ctx context.Context, paymentPlanId bringyour.Id) []*AccountPayment {
    payments := []*AccountPayment{}

    bringyour.Db(ctx, func(conn bringyour.PgConn) {
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
    })
    
    return payments
}


type PaymentPlan struct {
    PaymentPlanId bringyour.Id
    // wallet_id -> payment
    WalletPayments map[bringyour.Id]*AccountPayment
}


// plan, manually check out and add balance to funding account, then complete
// minimum net_revenue_nano_cents to include in a payout
// all of the returned payments are tagged with the same payment_plan_id
func PlanPayments(ctx context.Context) *PaymentPlan {
    var paymentPlan *PaymentPlan
    bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        result, err := tx.Query(
            ctx,
            `
            SELECT
                transfer_escrow_sweep.contract_id,
                transfer_escrow_sweep.balance_id,
                transfer_escrow_sweep.payoutBytes,
                transfer_escrow_sweep.payout_net_revenue_nano_cents,
                transfer_escrow_sweep.sweep_time,
                payout_wallet.wallet_id,

            FROM transfer_escrow_sweep

            LEFT JOIN account_payment ON
                account_payment.payment_id = transfer_escrow_sweep.payment_id

            INNER JOIN payout_wallet ON
                payout_wallet.network_id = transfer_escrow_sweep.network_id AND
                payout_wallet.active

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
                var payoutBytes int
                var payoutNetRevenue NanoCents
                var sweepTime time.Time
                var walletId bringyour.Id
                bringyour.Raise(result.Scan(
                    &contractId,
                    &balanceId,
                    &payoutBytes,
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
                }
                payment.PayoutBytes += payoutBytes
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

        bringyour.BatchInTx(ctx, tx, func(batch bringyour.PgBatch) {
            for _, payment := range walletPayments {
                batch.Queue(
                    `
                        INSERT INTO account_payment (
                            payment_id,
                            payment_plan_id,
                            wallet_id,
                            payout_bytes,
                            payout_nano_cents,
                            min_sweep_time,
                            create_time,
                        )
                        VALUES ($1, $2, $3, $4, $5)
                    `,
                    payment.PaymentId,
                    payment.PaymentPlanId,
                    payment.WalletId,
                    payment.PayoutBytes,
                    payment.Payout,
                    payment.MinSweepTime,
                    payment.CreateTime,
                )
            }
        })

        bringyour.CreateTempJoinTableInTx(
            ctx,
            tx,
            "payment_escrow_ids(contract_id uuid, balance_id uuid -> payment_id uuid)",
            escrowPaymentIds,
        )

        tx.Exec(
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
        )

        paymentPlan = &PaymentPlan{
            PaymentPlanId: paymentPlanId,
            WalletPayments: walletPayments,
        }
    }, bringyour.TxSerializable)

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

    bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        tag, err := tx.Exec(
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
        )
        bringyour.Raise(err)
        if tag.RowsAffected() != 1 {
            returnErr = fmt.Errorf("Invalid payment.")
            return 
        }
    })

    return
}


func CompletePayment(ctx context.Context, paymentId bringyour.Id, paymentReceipt string) (returnErr error) {
    bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        tag, err := tx.Exec(
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
        )
        bringyour.Raise(err)
        if tag.RowsAffected() != 1 {
            returnErr = fmt.Errorf("Invalid payment.")
            return
        }

        tx.Exec(
            ctx,
            `
                UPDATE account_balance
                SET
                    paid_bytes = paid_bytes + account_payment.payout_bytes,
                    paid_net_revenue_nano_cents = paid_net_revenue_nano_cents + account_payment.payout_nano_cents
                FROM account_payment, account_wallet
                WHERE
                    account_payment.payment_id = $1 AND
                    account_wallet.wallet_id = account_payment.wallet_id AND
                    account_balance.network_id = account_wallet.network_id
            `,
            paymentId,
        )
    })

    return
}


func CancelPayment(ctx context.Context, paymentId bringyour.Id) (returnErr error) {
    bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        tag, err := tx.Exec(
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
        )
        bringyour.Raise(err)
        if tag.RowsAffected() != 1 {
            returnErr = fmt.Errorf("Invalid payment.")
            return 
        }
    })

    return
}


type AccountBalance struct {
    NetworkId bringyour.Id
    ProvidedBytes int
    ProvidedNetRevenue NanoCents
    PaidBytes int
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
    bringyour.Db(session.Ctx, func(conn bringyour.PgConn) {
        result, err := conn.Query(
            session.Ctx,
            `
            SELECT
                provided_bytes,
                provided_net_revenue_nano_cents,
                paid_bytes,
                paid_balance_net_revenue_nano_cents
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
                    &balance.ProvidedBytes,
                    &balance.ProvidedNetRevenue,
                    &balance.PaidBytes,
                    &balance.PaidNetRevenue,
                ))
            }
            // else empty balance
            getAccountBalanceResult.Balance = balance
        })
    })
    return getAccountBalanceResult
}

