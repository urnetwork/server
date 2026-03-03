package model

import (
	"context"
	"fmt"
	"math"
	"time"

	// "crypto/rand"
	// "encoding/hex"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"

	// "golang.org/x/exp/maps"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

type ByteCount = int64

const Kib = ByteCount(1024)
const Mib = ByteCount(1024 * 1024)
const Gib = ByteCount(1024 * 1024 * 1024)

type Priority = uint32

const UnpaidPriority = 0
const PaidPriority = 100
const TrustedPriority = 200

func ByteCountHumanReadable(count ByteCount) string {
	trimFloatString := func(value float64, precision int, suffix string) string {
		s := fmt.Sprintf("%."+strconv.Itoa(precision)+"f", value)
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
		return s + suffix
	}

	if 1024*1024*1024*1024 <= count {
		return trimFloatString(
			float64(1000*count/(1024*1024*1024*1024))/1000.0,
			2,
			"tib",
		)
	} else if 1024*1024*1024 <= count {
		return trimFloatString(
			float64(1000*count/(1024*1024*1024))/1000.0,
			2,
			"gib",
		)
	} else if 1024*1024 <= count {
		return trimFloatString(
			float64(1000*count/(1024*1024))/1000.0,
			2,
			"mib",
		)
	} else if 1024 <= count {
		return trimFloatString(
			float64(1000*count/(1024))/1000.0,
			2,
			"kib",
		)
	} else {
		return fmt.Sprintf("%db", count)
	}
}

func ParseByteCount(humanReadable string) (ByteCount, error) {
	humanReadableLower := strings.ToLower(humanReadable)
	tibLower := "tib"
	gibLower := "gib"
	mibLower := "mib"
	kibLower := "kib"
	bLower := "b"
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
	} else if strings.HasSuffix(humanReadableLower, kibLower) {
		countFloat, err := strconv.ParseFloat(
			humanReadableLower[0:len(humanReadableLower)-len(kibLower)],
			64,
		)
		if err != nil {
			return ByteCount(0), err
		}
		return ByteCount(countFloat * 1024), nil
	} else if strings.HasSuffix(humanReadableLower, bLower) {
		countFloat, err := strconv.ParseFloat(
			humanReadableLower[0:len(humanReadableLower)-len(bLower)],
			64,
		)
		if err != nil {
			return ByteCount(0), err
		}
		return ByteCount(countFloat), nil
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

type NanoPoints = int64

// 1 point = 1_000_000 nano points

func PointsToNanoPoints(points float64) NanoPoints {
	return NanoPoints(math.Round(float64(points) * 1_000_000))
}

func NanoPointsToPoints(nanoPoints NanoPoints) int {
	return int(math.Round(float64(nanoPoints) / 1_000_000))
}

// 12 months
// const BalanceCodeDuration = 365 * 24 * time.Hour

// up to 16MiB
const AcceptableTransfersByteDifference = 16 * 1024 * 1024

const ProviderRevenueShare float64 = 0.5

const MaxSubscriptionPaymentIdsPerHour = 5

type TransferPair struct {
	A server.Id
	B server.Id
}

func NewTransferPair(sourceId server.Id, destinationId server.Id) TransferPair {
	return TransferPair{
		A: sourceId,
		B: destinationId,
	}
}

func NewUnorderedTransferPair(a server.Id, b server.Id) TransferPair {
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

// the escrow model has been updated so that:
//   - `transfer_balance` tracks the balance not in a `transfer_escrow`
//   - the net balance in a `transfer_escrow` is tracked approximately in redis `netEscrowKey`
//     Because redis is not atomic with Postgres, the value in the `netEscrowKey` will
//     eventually be consistent with the real value, but may be off by some amount at any given time.
//
// note: net escrow counters do not have a ttl
func netEscrowKey(balanceId server.Id) string {
	return fmt.Sprintf("{escrow}net_%s", balanceId)
}

type TransferBalance struct {
	BalanceId             server.Id `json:"balance_id"`
	NetworkId             server.Id `json:"network_id"`
	StartTime             time.Time `json:"start_time"`
	EndTime               time.Time `json:"end_time"`
	StartBalanceByteCount ByteCount `json:"start_balance_byte_count"`
	// how much money the platform made after subtracting fees
	NetRevenue        NanoCents `json:"net_revenue_nano_cents"`
	SubsidyNetRevenue NanoCents `json:"subsidy_net_revenue_nano_cents,omitempty"`
	BalanceByteCount  ByteCount `json:"balance_byte_count"`
	PurchaseToken     string    `json:"purchase_token,omitempty"`
	Paid              bool      `json:"paid,omitempty"`
}

func GetActiveTransferBalances(ctx context.Context, networkId server.Id) []*TransferBalance {
	now := server.NowUtc()

	transferBalances := []*TransferBalance{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    balance_id,
                    start_time,
                    end_time,
                    start_balance_byte_count,
                    net_revenue_nano_cents,
                    balance_byte_count,
                    paid
                FROM transfer_balance
                WHERE
                    network_id = $1 AND
                    active = true AND
                    start_time <= $2 AND $2 < end_time
            `,
			networkId,
			now,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				transferBalance := &TransferBalance{
					NetworkId: networkId,
				}
				server.Raise(result.Scan(
					&transferBalance.BalanceId,
					&transferBalance.StartTime,
					&transferBalance.EndTime,
					&transferBalance.StartBalanceByteCount,
					&transferBalance.NetRevenue,
					&transferBalance.BalanceByteCount,
					&transferBalance.Paid,
				))
				transferBalances = append(transferBalances, transferBalance)
			}
		})
	})

	server.Redis(ctx, func(r server.RedisClient) {
		netEscrowCmds := map[server.Id]*redis.StringCmd{}
		r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			for _, transferBalance := range transferBalances {
				netEscrowCmds[transferBalance.BalanceId] = pipe.Get(ctx, netEscrowKey(transferBalance.BalanceId))
			}
			return nil
		})
		for _, transferBalance := range transferBalances {
			netEscrowCmd := netEscrowCmds[transferBalance.BalanceId]
			netEscrowBalanceByteCount, _ := netEscrowCmd.Int()
			transferBalance.BalanceByteCount = max(0, transferBalance.BalanceByteCount-ByteCount(netEscrowBalanceByteCount))
		}
	})

	return transferBalances
}

func GetActiveTransferBalanceByteCount(ctx context.Context, networkId server.Id) ByteCount {
	net := ByteCount(0)
	for _, transferBalance := range GetActiveTransferBalances(ctx, networkId) {
		net += transferBalance.BalanceByteCount
	}
	return net
}

func AddTransferBalanceInTx(ctx context.Context, tx server.PgTx, transferBalance *TransferBalance) {
	balanceId := server.NewId()

	server.RaisePgResult(tx.Exec(
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
                    purchase_token,
                    subsidy_net_revenue_nano_cents
                )
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
            `,
		balanceId,
		transferBalance.NetworkId,
		transferBalance.StartTime,
		transferBalance.EndTime,
		transferBalance.StartBalanceByteCount,
		transferBalance.BalanceByteCount,
		transferBalance.NetRevenue,
		transferBalance.PurchaseToken,
		transferBalance.SubsidyNetRevenue,
	))

	transferBalance.BalanceId = balanceId
}

func AddTransferBalance(ctx context.Context, transferBalance *TransferBalance) {
	server.Tx(ctx, func(tx server.PgTx) {
		AddTransferBalanceInTx(ctx, tx, transferBalance)
	})
}

// TODO GetLastTransferData returns the transfer data with
// 1. the given purhase record
// 2. that starte before and ends after sub.ExpiryTime
// TODO with the max end time
// TODO if none, return err
func GetOverlappingTransferBalance(ctx context.Context, purchaseToken string, expiryTime time.Time) (balanceId server.Id, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
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
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&balanceId))
			} else {
				returnErr = errors.New("Overlapping transfer balance not found.")
			}
		})
	})

	return
}

func AddBasicTransferBalanceInTx(
	tx server.PgTx,
	ctx context.Context,
	networkId server.Id,
	transferBalance ByteCount,
	startTime time.Time,
	endTime time.Time,
) (returnErr error) {
	balanceId := server.NewId()

	_, err := tx.Exec(
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
	)

	if err != nil {
		returnErr = err
	}
	return
}

// add balance to a network at no cost
func AddBasicTransferBalance(
	ctx context.Context,
	networkId server.Id,
	transferBalance ByteCount,
	startTime time.Time,
	endTime time.Time,
) (returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		returnErr = AddBasicTransferBalanceInTx(
			tx,
			ctx,
			networkId,
			transferBalance,
			startTime,
			endTime,
		)
	})

	return
}

// this finds networks with no entries in transfer_balance
// this is potentially different than networks with zero transfer balance
func FindNetworksWithoutTransferBalance(ctx context.Context) (networkIds []server.Id) {
	server.Db(ctx, func(conn server.PgConn) {
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

		networkIds = []server.Id{}
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				server.Raise(result.Scan(&networkId))
				networkIds = append(networkIds, networkId)
			}
		})
	})
	return
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
	ContractId        server.Id
	Priority          Priority
	TransferByteCount ByteCount
	Balances          []*TransferEscrowBalance
}

type TransferEscrowBalance struct {
	BalanceId        server.Id
	BalanceByteCount ByteCount
}

func createTransferEscrowInTx(
	ctx context.Context,
	tx server.PgTx,
	sourceNetworkId server.Id,
	sourceId server.Id,
	destinationNetworkId server.Id,
	destinationId server.Id,
	payerNetworkId server.Id,
	contractTransferByteCount ByteCount,
	companionContractId *server.Id,
) (transferEscrow *TransferEscrow, posts []func() any, returnErr error) {
	// *important note* this function is one of the hotspots in the system,
	// since it is called before every transfer pair.
	// a small regression here can cause a backlog in the overall throughput of the network.
	// You must make sure the queries here are optimized correctly.
	// TODO we need better performance regression tools to measure small regressions in hotspots like this

	// note it is possible to create a contract with `contractTransferByteCount = 0`

	contractId := server.NewId()

	type escrow struct {
		balanceId        server.Id
		paid             bool
		balanceByteCount ByteCount
		startTime        time.Time
		endTime          time.Time
	}

	now := server.NowUtc()

	// add up the balance_byte_count until >= contractTransferByteCount
	// if not enough, error
	balanceEscrows := map[server.Id]*escrow{}

	// attempt to split up across remaining transfer balances

	orderedTransferBalances := []*escrow{}
	result, err := tx.Query(
		ctx,
		`
            SELECT
                balance_id,
                paid,
                balance_byte_count,
                start_time,
                end_time
            FROM transfer_balance
            WHERE
                network_id = $1 AND
                active = true
        `,
		payerNetworkId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			transferBalance := &escrow{}
			server.Raise(result.Scan(
				&transferBalance.balanceId,
				&transferBalance.paid,
				&transferBalance.balanceByteCount,
				&transferBalance.startTime,
				&transferBalance.endTime,
			))
			if !transferBalance.startTime.After(now) && now.Before(transferBalance.endTime) {
				orderedTransferBalances = append(orderedTransferBalances, transferBalance)
			}
		}
	})

	server.Redis(ctx, func(r server.RedisClient) {
		netEscrowCmds := map[server.Id]*redis.StringCmd{}
		r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			for _, transferBalance := range orderedTransferBalances {
				netEscrowCmds[transferBalance.balanceId] = r.Get(ctx, netEscrowKey(transferBalance.balanceId))
			}
			return nil
		})
		for _, transferBalance := range orderedTransferBalances {
			netEscrowCmd := netEscrowCmds[transferBalance.balanceId]
			netEscrowBalanceByteCount, _ := netEscrowCmd.Int()
			transferBalance.balanceByteCount = max(0, transferBalance.balanceByteCount-ByteCount(netEscrowBalanceByteCount))
		}
	})

	slices.SortFunc(orderedTransferBalances, func(a *escrow, b *escrow) int {
		if a.endTime.Before(b.endTime) {
			return -1
		} else if b.endTime.Before(a.endTime) {
			return 1
		}

		if a.startTime.Before(b.startTime) {
			return -1
		} else if b.startTime.Before(a.startTime) {
			return 1
		}

		return a.balanceId.Cmp(b.balanceId)
	})

	netEscrowBalanceByteCount := ByteCount(0)

	for _, transferBalance := range orderedTransferBalances {
		escrowBalanceByteCount := min(
			contractTransferByteCount-netEscrowBalanceByteCount,
			transferBalance.balanceByteCount,
		)

		balanceEscrows[transferBalance.balanceId] = &escrow{
			balanceId:        transferBalance.balanceId,
			paid:             transferBalance.paid,
			balanceByteCount: escrowBalanceByteCount,
		}
		netEscrowBalanceByteCount += escrowBalanceByteCount
		if contractTransferByteCount <= netEscrowBalanceByteCount {
			// we have enough balances for this escrow
			break
		}
	}

	if netEscrowBalanceByteCount < contractTransferByteCount {
		returnErr = fmt.Errorf("Insufficient balance (%d).", netEscrowBalanceByteCount)
		return
	}

	// the priority is blended between 0 and 100 depending on escrows
	var priority Priority
	if 0 < len(balanceEscrows) {
		for _, escrow := range balanceEscrows {
			if escrow.paid {
				priority += PaidPriority
			} else {
				priority += UnpaidPriority
			}
		}
		priority /= Priority(len(balanceEscrows))
	} else {
		priority = UnpaidPriority
	}

	server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
		for balanceId, escrow := range balanceEscrows {
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
				escrow.balanceByteCount,
			)
		}

		batch.Queue(
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
	                create_time,
	                priority
	            )
	            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	        `,
			contractId,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			contractTransferByteCount,
			companionContractId,
			payerNetworkId,
			now,
			priority,
		)
	})

	posts = append(posts, func() any {
		server.Redis(ctx, func(r server.RedisClient) {
			r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				for balanceId, escrow := range balanceEscrows {
					pipe.IncrBy(ctx, netEscrowKey(balanceId), escrow.balanceByteCount)
				}
				return nil
			})
		})
		return nil
	})

	balances := []*TransferEscrowBalance{}
	for balanceId, escrow := range balanceEscrows {
		balance := &TransferEscrowBalance{
			BalanceId:        balanceId,
			BalanceByteCount: escrow.balanceByteCount,
		}
		balances = append(balances, balance)
	}

	transferEscrow = &TransferEscrow{
		ContractId:        contractId,
		TransferByteCount: contractTransferByteCount,
		Priority:          priority,
		Balances:          balances,
	}

	return
}

// renaming of `CreateTransferEscrow` since contract is the top level concept
func CreateContract(
	ctx context.Context,
	sourceNetworkId server.Id,
	sourceId server.Id,
	destinationNetworkId server.Id,
	destinationId server.Id,
	contractTransferByteCount ByteCount,
) (contractId server.Id, transferEscrow *TransferEscrow, returnErr error) {
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
	sourceNetworkId server.Id,
	sourceId server.Id,
	destinationNetworkId server.Id,
	destinationId server.Id,
	contractTransferByteCount ByteCount,
) (transferEscrow *TransferEscrow, returnErr error) {
	var posts []func() any

	server.Tx(ctx, func(tx server.PgTx) {
		transferEscrow, posts, returnErr = createTransferEscrowInTx(
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
	})

	runPosts(posts)

	return
}

// renaming of `CreateCompanionTransferEscrow` since contract is the top level concept
func CreateCompanionContract(
	ctx context.Context,
	sourceNetworkId server.Id,
	sourceId server.Id,
	destinationNetworkId server.Id,
	destinationId server.Id,
	contractTransferByteCount ByteCount,
	originContractTimeout time.Duration,
) (contractId server.Id, transferEscrow *TransferEscrow, returnErr error) {
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
	sourceNetworkId server.Id,
	sourceId server.Id,
	destinationNetworkId server.Id,
	destinationId server.Id,
	contractTransferByteCount ByteCount,
	originContractTimeout time.Duration,
) (transferEscrow *TransferEscrow, returnErr error) {
	var posts []func() any

	server.Tx(ctx, func(tx server.PgTx) {
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
			server.NowUtc().Add(-originContractTimeout),
		)
		var companionContractId *server.Id
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&companionContractId))
			}
		})

		if companionContractId == nil {
			returnErr = fmt.Errorf("Missing origin contract for companion.")
			return
		}

		transferEscrow, posts, returnErr = createTransferEscrowInTx(
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
	})

	runPosts(posts)

	return
}

// contract_ids ordered by create time with:
// - at least `contractTransferByteCount` available
// - not closed by any party
// - with transfer escrow
func GetOpenTransferEscrowsOrderedByPriorityCreateTime(
	ctx context.Context,
	sourceId server.Id,
	destinationId server.Id,
	contractTransferByteCount ByteCount,
) []*TransferEscrow {
	transferEscrows := []*TransferEscrow{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT

                    transfer_contract.contract_id,
                    transfer_contract.transfer_byte_count,
                    transfer_contract.priority

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

                ORDER BY transfer_contract.priority DESC, transfer_contract.create_time ASC
            `,
			sourceId,
			destinationId,
			contractTransferByteCount,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var contractId server.Id
				var transferByteCount ByteCount
				var priority Priority
				server.Raise(result.Scan(&contractId, &transferByteCount, &priority))
				transferEscrow := &TransferEscrow{
					ContractId:        contractId,
					Priority:          priority,
					TransferByteCount: transferByteCount,
				}
				transferEscrows = append(transferEscrows, transferEscrow)
			}
		})
	})

	return transferEscrows
}

func GetTransferEscrow(ctx context.Context, contractId server.Id) (transferEscrow *TransferEscrow) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    transfer_byte_count,
                    priority

                FROM transfer_byte_count
                WHERE
                    contract_id = $1
            `,
			contractId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				transferEscrow = &TransferEscrow{}
				server.Raise(result.Scan(
					&transferEscrow.TransferByteCount,
					&transferEscrow.Priority,
				))
			}
		})
		if transferEscrow == nil {
			// not found
			return
		}

		result, err = conn.Query(
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
		server.WithPgResult(result, err, func() {
			for result.Next() {
				balance := &TransferEscrowBalance{}
				server.Raise(result.Scan(
					&balance.BalanceId,
					&balance.BalanceByteCount,
				))
				transferEscrow.Balances = append(transferEscrow.Balances, balance)
			}
		})
	})

	return
}

// some clients - platform, friends and family, etc - do not need an escrow
// typically `provide_mode < Public` does not use an escrow1
func CreateContractNoEscrow(
	ctx context.Context,
	sourceNetworkId server.Id,
	sourceId server.Id,
	destinationNetworkId server.Id,
	destinationId server.Id,
	contractTransferByteCount ByteCount,
) (contractId server.Id, returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		contractId = server.NewId()

		server.RaisePgResult(tx.Exec(
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
			server.NowUtc(),
		))
	})
	return
}

// this will create a close entry,
// then settle if all parties agree, or set dispute if there is a dispute
func CloseContract(
	ctx context.Context,
	contractId server.Id,
	clientId server.Id,
	usedTransferByteCount ByteCount,
	checkpoint bool,
) (returnErr error) {
	// settle := false
	// dispute := false

	server.Tx(ctx, func(tx server.PgTx) {
		found := false
		var sourceId server.Id
		var destinationId server.Id
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
            `,
			contractId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				found = true
				server.Raise(result.Scan(&sourceId, &destinationId, &outcome, &dispute))
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
			server.RaisePgResult(tx.Exec(
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
				server.NowUtc(),
			))

		} else {
			server.RaisePgResult(tx.Exec(
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
				server.NowUtc(),
			))
		}
	})

	if returnErr != nil {
		return
	}

	returnErr = settleContract(ctx, contractId)
	return
}

func settleContract(ctx context.Context, contractId server.Id) (returnErr error) {
	var posts []func() any

	server.Tx(ctx, func(tx server.PgTx) {
		// party -> used transfer byte count
		closes := map[ContractParty]ByteCount{}
		result, err := tx.Query(
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
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var closeParty ContractParty
				var closeUsedTransferByteCount ByteCount
				server.Raise(result.Scan(
					&closeParty,
					&closeUsedTransferByteCount,
				))
				closes[closeParty] = closeUsedTransferByteCount
			}
		})
		// for closeParty, closeUsedTransferByteCount := range closes {
		// 	fmt.Printf("CLOSE CONTRACT PARTY %s=%d: %s %s %s->%s\n", closeParty, closeUsedTransferByteCount, contractId.String(), clientId.String(), sourceId.String(), destinationId.String())
		// }

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
			server.WithPgResult(result, err, func() {
				if result.Next() {
					hasEscrow = true
				}
			})

			if hasEscrow {
				diff := sourceUsedTransferByteCount - destinationUsedTransferByteCount
				if math.Abs(float64(diff)) <= AcceptableTransfersByteDifference {
					// fmt.Printf("CLOSE CONTRACT SETTLE (%s) %s\n", clientId.String(), contractId.String())
					posts, returnErr = settleEscrowInTx(ctx, tx, contractId, ContractOutcomeSettled)
				} else {
					glog.Infof("[sub]contract[%s]diff %d (%d <> %d)\n", contractId.String(), diff, sourceUsedTransferByteCount, destinationUsedTransferByteCount)
					// fmt.Printf("CLOSE CONTRACT DISPUTE (%s) %s\n", clientId.String(), contractId.String())
					setContractDisputeInTx(ctx, tx, contractId, true)
				}
			} else {
				// nothing to settle, just close the transaction
				server.RaisePgResult(tx.Exec(
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
					server.NowUtc(),
				))
			}
		}
	}, server.TxReadCommitted)

	if returnErr != nil {
		return
	}

	runPosts(posts)
	return
}

func SettleEscrow(ctx context.Context, contractId server.Id, outcome ContractOutcome) (returnErr error) {
	var posts []func() any

	server.Tx(ctx, func(tx server.PgTx) {
		posts, returnErr = settleEscrowInTx(ctx, tx, contractId, outcome)
	})

	runPosts(posts)

	return
}

func settleEscrowInTx(
	ctx context.Context,
	tx server.PgTx,
	contractId server.Id,
	outcome ContractOutcome,
) (posts []func() any, returnErr error) {
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
            `,
			contractId,
		)
		netUsedTransferByteCount := ByteCount(0)
		partyCount := 0
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var usedTransferByteCountForParty ByteCount
				var party ContractParty
				var checkpoint bool
				server.Raise(result.Scan(
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
				// fmt.Printf("SETTLE %s: found party %s used %d\n", contractId.String(), party, usedTransferByteCountForParty)
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
            `,
			contractId,
			party,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&usedTransferByteCount))
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
	sweepPayouts := map[server.Id]sweepPayout{}
	netPayoutByteCount := ByteCount(0)
	netPayout := NanoCents(0)

	server.WithPgResult(result, err, func() {
		for result.Next() {
			var balanceId server.Id
			var escrowBalanceByteCount ByteCount
			var startBalanceByteCount ByteCount
			var netRevenue NanoCents
			server.Raise(result.Scan(
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
				escrowBalanceByteCount: escrowBalanceByteCount,
				payoutByteCount:        payoutByteCount,
				returnByteCount:        returnByteCount,
				payout:                 payout,
			}
			// fmt.Printf("SETTLE %s %s: payout %d (%d nanocents) return %d\n", contractId.String(), balanceId.String(), payoutByteCount, payout, returnByteCount)
		}
	})

	// if len(sweepPayouts) == 0 {
	// 	returnErr = fmt.Errorf("Invalid contract.")
	// 	return
	// }

	if netPayoutByteCount < usedTransferByteCount {
		returnErr = fmt.Errorf("Escrow does not have enough value to pay out the full amount.")
		return
	}

	var payoutNetworkId *server.Id
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
        `,
		contractId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			var sourceNetworkId server.Id
			var destinationNetworkId server.Id
			var payerNetworkId *server.Id
			var companionContractId *server.Id
			server.Raise(result.Scan(
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

	server.RaisePgResult(tx.Exec(
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
		server.NowUtc(),
	))

	// run all the posts in parallel in as small blocks as reasonable to minimize the work for serialization errors

	if 0 < len(sweepPayouts) {
		posts = append(posts, func() any {
			server.Tx(ctx, func(tx server.PgTx) {
				server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
					for balanceId, sweepPayout := range sweepPayouts {
						batch.Queue(
							`
					            UPDATE transfer_escrow
					            SET
					                settled = true,
					                settle_time = $2,
					                payout_byte_count = $4
					            WHERE
					                transfer_escrow.contract_id = $1 AND
					                transfer_escrow.balance_id = $3
					        `,
							contractId,
							server.NowUtc(),
							balanceId,
							sweepPayout.payoutByteCount,
						)

					}
				})
			})
			return nil
		})

		posts = append(posts, func() any {
			server.Tx(ctx, func(tx server.PgTx) {
				server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
					for balanceId, sweepPayout := range sweepPayouts {

						if 0 < sweepPayout.payoutByteCount {
							batch.Queue(
								`
						            INSERT INTO transfer_escrow_sweep (
						                contract_id,
						                balance_id,
						                network_id,
						                payout_byte_count,
						                payout_net_revenue_nano_cents
						            )
						            VALUES ($1, $2, $3, $4, $5)
						        `,
								contractId,
								balanceId,
								payoutNetworkId,
								sweepPayout.payoutByteCount,
								sweepPayout.payout,
							)

						}
					}
				})
			})
			return nil
		})

		posts = append(posts, func() any {
			server.Tx(ctx, func(tx server.PgTx) {
				server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
					for balanceId, sweepPayout := range sweepPayouts {

						if 0 < sweepPayout.payoutByteCount {

							batch.Queue(
								`
						            UPDATE transfer_balance
						            SET
						                balance_byte_count = transfer_balance.balance_byte_count - $2
						            WHERE
						                transfer_balance.balance_id = $1
						        `,
								balanceId,
								sweepPayout.payoutByteCount,
							)
						}
					}
				})
			})
			return nil
		})

		posts = append(posts, func() any {
			server.Redis(ctx, func(r server.RedisClient) {
				r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
					for balanceId, sweepPayout := range sweepPayouts {
						pipe.DecrBy(ctx, netEscrowKey(balanceId), sweepPayout.escrowBalanceByteCount)
					}
					return nil
				})
			})
			return nil
		})
	}

	posts = append(posts, func() any {
		server.Redis(ctx, func(r server.RedisClient) {
			r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.IncrBy(ctx, accountBalanceNetPayoutByteCountKey(*payoutNetworkId), netPayoutByteCount)
				pipe.IncrBy(ctx, accountBalanceNetPayout(*payoutNetworkId), netPayout)
				return nil
			})
		})
		return nil
	})

	return
}

type sweepPayout struct {
	escrowBalanceByteCount ByteCount
	payoutByteCount        ByteCount
	returnByteCount        ByteCount
	payout                 NanoCents
}

// `server.ComplexValue`
func (self *sweepPayout) Values() []any {
	return []any{
		self.payoutByteCount,
		self.returnByteCount,
		self.payout,
	}
}

func SetContractDispute(ctx context.Context, contractId server.Id, dispute bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		setContractDisputeInTx(ctx, tx, contractId, dispute)
	})
}

func setContractDisputeInTx(
	ctx context.Context,
	tx server.PgTx,
	contractId server.Id,
	dispute bool,
) {
	server.RaisePgResult(tx.Exec(
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
		server.NowUtc(),
	))
}

func GetOpenContractIdsWithNoPartialClose(
	ctx context.Context,
	sourceId server.Id,
	destinationId server.Id,
) map[server.Id]bool {
	contractIds := map[server.Id]bool{}
	for contractId, parties := range GetOpenContractIds(ctx, sourceId, destinationId) {
		if len(parties) == 0 {
			contractIds[contractId] = true
		}
	}
	return contractIds
}

func GetOpenContractIdsWithPartialClose(
	ctx context.Context,
	sourceId server.Id,
	destinationId server.Id,
) map[server.Id]ContractParty {
	contractIdPartialCloseParties := map[server.Id]ContractParty{}
	for contractId, parties := range GetOpenContractIds(ctx, sourceId, destinationId) {
		if len(parties) == 1 {
			contractIdPartialCloseParties[contractId] = parties[0]
		}
	}
	return contractIdPartialCloseParties
}

// contract id -> partially closed contract party, or "" if none
func GetOpenContractIds(
	ctx context.Context,
	sourceId server.Id,
	destinationId server.Id,
) map[server.Id][]ContractParty {
	contractIdPartialCloseParties := map[server.Id][]ContractParty{}

	server.Tx(ctx, func(tx server.PgTx) {
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
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var contractId server.Id
				var party_ *ContractParty
				var checkpoint_ *bool
				server.Raise(result.Scan(&contractId, &party_, &checkpoint_))
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
				// if checkpoint {
				// 	if contractIdPartialCloseParties[contractId] == "" {
				// 		contractIdPartialCloseParties[contractId] = ContractPartyCheckpoint
				// 	}
				// } else {
				// 	contractIdPartialCloseParties[contractId] = party
				// }
				if checkpoint {
					contractIdPartialCloseParties[contractId] = append(contractIdPartialCloseParties[contractId], ContractPartyCheckpoint)
				} else if party != "" {
					contractIdPartialCloseParties[contractId] = append(contractIdPartialCloseParties[contractId], party)
				} else if _, ok := contractIdPartialCloseParties[contractId]; !ok {
					contractIdPartialCloseParties[contractId] = []ContractParty{}
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
) map[server.Id]bool {
	contractIdPartialCloseParties := map[server.Id]map[ContractParty]bool{}

	server.Tx(ctx, func(tx server.PgTx) {
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
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var contractId server.Id
				var party_ *ContractParty
				var checkpoint_ *bool
				server.Raise(result.Scan(&contractId, &party_, &checkpoint_))
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

	contractIdCloses := map[server.Id]bool{}
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
    clientId server.Id,
) map[TransferPair]map[server.Id]bool {
    pairContractIdPartialCloseParties := GetOpenContractIdsForSourceOrDestination(ctx, clientId)
    pairContractIds := map[TransferPair]map[server.Id]bool{}
    for transferPair, contractIdPartialCloseParties := range pairContractIdPartialCloseParties {
        for contractId, party := range contractIdPartialCloseParties {
            if party == "" {
                contractIds, ok := pairContractIds[transferPair]
                if !ok {
                    contractIds = map[server.Id]bool{}
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
	clientId server.Id,
) map[TransferPair]map[server.Id]ContractParty {
	pairContractIdPartialCloseParties := map[TransferPair]map[server.Id]ContractParty{}

	server.Tx(ctx, func(tx server.PgTx) {
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
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var sourceId server.Id
				var destinationId server.Id
				var contractId server.Id
				var party_ *ContractParty
				var checkpoint_ *bool
				server.Raise(result.Scan(
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
					contractIdPartialCloseParties = map[server.Id]ContractParty{}
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

func ForceCloseAllOpenContractIds(ctx context.Context, minTime time.Time) error {
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("Done.")
		default:
		}
		c, err := ForceCloseOpenContractIds(ctx, minTime, 1000, 1, 0, 0)
		if err != nil {
			return err
		}
		if c == 0 {
			return nil
		}
	}
}

// closes all open contracts with no update in the last `timeout`
// cases handled:
// - no closes
// - single close
// - one or more checkpoints
func ForceCloseOpenContractIds(
	ctx context.Context,
	minTime time.Time,
	maxCount int,
	parallel int,
	blockSize int,
	blockIndex int,
) (
	closeCount int64,
	err error,
) {

	/*
		// force close contracts where there is nothing to do
		server.Tx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
				UPDATE transfer_contract
				SET
			        outcome = $5,
			        close_time = $6
				FROM (
					SELECT
			            t.contract_id

			        FROM (
			            SELECT
			                transfer_contract.contract_id,
			                transfer_contract.source_id,
			                transfer_contract.destination_id

			            FROM transfer_contract

			            WHERE
			                transfer_contract.open AND
			                transfer_contract.create_time <= $3

			            LIMIT $4

			        ) t

			        LEFT JOIN contract_close source_contract_close ON
			            source_contract_close.contract_id = t.contract_id AND
			            source_contract_close.party = $1

			        LEFT JOIN contract_close destination_contract_close ON
			            destination_contract_close.contract_id = t.contract_id AND
			            destination_contract_close.party = $2

			        WHERE
			        	destination_contract_close.contract_id IS NOT NULL AND
			        	source_contract_close.contract_id IS NOT NULL
			    ) t

			    WHERE
			        transfer_contract.contract_id = t.contract_id

				`,
				ContractPartySource,
				ContractPartyDestination,
				minTime.UTC(),
				maxCount,
				ContractOutcomeSettled,
				server.NowUtc(),
			))

			if c := tag.RowsAffected(); 0 < c {
				glog.Infof("[sm]force closed %d malformed contracts\n", c)
				closeCount += c
			}
		})
	*/

	type OpenContract struct {
		contractId    server.Id
		sourceId      server.Id
		destinationId server.Id
		dispute       bool

		sourceCloseTime             *time.Time
		sourceUsedTransferByteCount *ByteCount
		sourceCheckpoint            *bool

		destinationCloseTime             *time.Time
		destinationUsedTransferByteCount *ByteCount
		destinationCheckpoint            *bool
	}

	openContracts := []*OpenContract{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    t.contract_id,
                    t.source_id,
                    t.destination_id,
                    t.dispute,

                    source_contract_close.close_time AS source_close_time,
                    source_contract_close.used_transfer_byte_count AS source_used_transfer_byte_count,
                    source_contract_close.checkpoint AS source_checkpoint,

                    destination_contract_close.close_time AS destination_close_time,
                    destination_contract_close.used_transfer_byte_count AS destination_used_transfer_byte_count,
                    destination_contract_close.checkpoint AS destination_checkpoint

                FROM (
                    SELECT
                        transfer_contract.contract_id,
                        transfer_contract.source_id,
                        transfer_contract.destination_id,
                        transfer_contract.dispute

                    FROM transfer_contract

                    WHERE
                        transfer_contract.open AND
                        transfer_contract.create_time <= $3

                    ORDER BY transfer_contract.create_time

                    LIMIT $4
                ) t

                LEFT JOIN contract_close source_contract_close ON
                    source_contract_close.contract_id = t.contract_id AND
                    source_contract_close.party = $1

                LEFT JOIN contract_close destination_contract_close ON
                    destination_contract_close.contract_id = t.contract_id AND
                    destination_contract_close.party = $2

            `,
			ContractPartySource,
			ContractPartyDestination,
			minTime.UTC(),
			maxCount,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				openContract := &OpenContract{}

				server.Raise(result.Scan(
					&openContract.contractId,
					&openContract.sourceId,
					&openContract.destinationId,
					&openContract.dispute,
					&openContract.sourceCloseTime,
					&openContract.sourceUsedTransferByteCount,
					&openContract.sourceCheckpoint,
					&openContract.destinationCloseTime,
					&openContract.destinationUsedTransferByteCount,
					&openContract.destinationCheckpoint,
				))

				if 0 < blockSize {
					if int(openContract.contractId.Hash()%uint64(blockSize)) == blockIndex%blockSize {
						openContracts = append(openContracts, openContract)
					}
				} else {
					openContracts = append(openContracts, openContract)
				}
			}
		})
	})

	glog.Infof("[sm]found %d contracts to close\n", len(openContracts))

	closeMalformedContract := func(tag string, openContract *OpenContract, err error) {
		glog.Infof("%sforce close malformed contract: %s\n", tag, err)

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
                    UPDATE transfer_contract
                    SET
                        outcome = $2,
                        close_time = $3
                    WHERE
                        contract_id = $1
                `,
				openContract.contractId,
				ContractOutcomeSettled,
				server.NowUtc(),
			))
		})
	}

	closeContract := func(tag string, openContract *OpenContract) error {
		if openContract.dispute {
			// todo: improve this with better detection of th eroot causes
			glog.Infof("%ssettle contract dispute: both sides\n", tag)
			var posts []func() any
			var err error
			server.Tx(ctx, func(tx server.PgTx) {
				setContractDisputeInTx(ctx, tx, openContract.contractId, false)
				posts, err = settleEscrowInTx(ctx, tx, openContract.contractId, ContractOutcomeSettled)
			})
			runPosts(posts)
			if err != nil {
				return err
			}

		} else if openContract.sourceCloseTime == nil && openContract.destinationCloseTime == nil {
			// close with both sides 0
			glog.Infof("%sforce close contract: both sides\n", tag)

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
			glog.Infof("%sforce close contract: source accepts destination\n", tag)

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
			glog.Infof("%sforce close contract: destination accepts source\n", tag)

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

		} else if *openContract.sourceCheckpoint || *openContract.destinationCheckpoint {
			// finalize one or more checkpoints

			if *openContract.sourceCheckpoint {
				glog.Infof("%sforce close contract: finalize source checkpoint\n", tag)

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
				glog.Infof("%sforce close contract: finalize destination checkpoint\n", tag)
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

		} else {
			// nothing to settle, just close the transaction
			var posts []func() any
			var err error
			server.Tx(ctx, func(tx server.PgTx) {
				posts, err = settleEscrowInTx(ctx, tx, openContract.contractId, ContractOutcomeSettled)
			})
			runPosts(posts)
			if err != nil {
				return err
			}
		}

		return nil
	}

	nextIndex := 0
	var nextIndexLock sync.Mutex
	getAndIncrNextIndex := func() int {
		nextIndexLock.Lock()
		defer nextIndexLock.Unlock()

		i := nextIndex
		nextIndex += 1
		return i
	}

	var wg sync.WaitGroup

	for range parallel {
		wg.Add(1)
		go server.HandleError(func() {
			defer wg.Done()
			for j := getAndIncrNextIndex(); j < len(openContracts); j = getAndIncrNextIndex() {
				select {
				case <-ctx.Done():
					return
				default:
				}

				server.HandleError(func() {
					openContract := openContracts[j]
					tag := fmt.Sprintf("[sm][%s][%d/%d]", openContract.contractId, j+1, len(openContracts))
					err := closeContract(tag, openContracts[j])
					if err != nil {
						// glog.Infof("%sforce close contract err = %s\n", tag, err)
						closeMalformedContract(tag, openContract, err)
					}
				})
			}
		})
	}

	wg.Wait()

	closeCount += int64(len(openContracts))

	return
}

type ContractClose struct {
	CloseTime time.Time
	Dispute   bool
	Outcome   string
}

func GetContractClose(ctx context.Context, contractId server.Id) (contractClose *ContractClose, closed bool) {
	server.Db(ctx, func(conn server.PgConn) {
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
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var closeTime *time.Time
				var dispute bool
				var outcome *string
				server.Raise(result.Scan(&closeTime, &dispute, &outcome))
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

// update 2026-01-30: the net payout byte count and net payout are now tracked in redis
// FIXME this should be merged back into the database at regular checkpoint intervals

func accountBalanceNetPayoutByteCountKey(networkId server.Id) string {
	return fmt.Sprintf("{account_balance_%s}npbc", networkId)
}

func accountBalanceNetPayout(networkId server.Id) string {
	return fmt.Sprintf("{account_balance_%s}np", networkId)
}

type AccountBalance struct {
	NetworkId          server.Id
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
	balance := &AccountBalance{
		NetworkId: session.ByJwt.NetworkId,
	}
	server.Db(session.Ctx, func(conn server.PgConn) {
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
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(
					&balance.ProvidedByteCount,
					&balance.ProvidedNetRevenue,
					&balance.PaidByteCount,
					&balance.PaidNetRevenue,
				))
			}
			// else empty balance
		})
	})
	server.Redis(session.Ctx, func(r server.RedisClient) {
		var providedNetByteCountCmd *redis.StringCmd
		var providedNetPayoutCmd *redis.StringCmd
		r.Pipelined(session.Ctx, func(pipe redis.Pipeliner) error {
			providedNetByteCountCmd = pipe.Get(session.Ctx, accountBalanceNetPayoutByteCountKey(session.ByJwt.NetworkId))
			providedNetPayoutCmd = pipe.Get(session.Ctx, accountBalanceNetPayout(session.ByJwt.NetworkId))
			return nil
		})
		providedNetByteCount, _ := providedNetByteCountCmd.Int64()
		balance.ProvidedByteCount += providedNetByteCount
		providedNetRevenue, _ := providedNetPayoutCmd.Int64()
		balance.ProvidedNetRevenue += providedNetRevenue
	})
	return &GetAccountBalanceResult{
		Balance: balance,
	}
}

type SubscriptionCreatePaymentIdArgs struct {
}

type SubscriptionCreatePaymentIdResult struct {
	SubscriptionPaymentId server.Id                         `json:"subscription_payment_id,omitempty"`
	Error                 *SubscriptionCreatePaymentIdError `json:"error,omitempty"`
}

type SubscriptionCreatePaymentIdError struct {
	Message string `json:"message"`
}

func SubscriptionCreatePaymentId(createPaymentId *SubscriptionCreatePaymentIdArgs, clientSession *session.ClientSession) (createPaymentIdResult *SubscriptionCreatePaymentIdResult, returnErr error) {
	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
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
			server.NowUtc().Add(-1*time.Hour),
		)

		limitExceeded := false

		server.WithPgResult(result, err, func() {
			if result.Next() {
				var count int
				server.Raise(result.Scan(&count))
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

		subscriptionPaymentId := server.NewId()

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

func SubscriptionGetNetworkIdForPaymentId(ctx context.Context, subscriptionPaymentId server.Id) (networkId server.Id, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
            SELECT network_id FROM subscription_payment
            WHERE subscription_payment_id = $1
            `,
			subscriptionPaymentId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&networkId))
			} else {
				returnErr = errors.New("Invalid subscription payment.")
			}
		})
	})
	return
}

type SubscriptionType = string

const SubscriptionTypeSupporter = "supporter"

type SubscriptionMarket = string

const SubscriptionMarketApple = "apple"
const SubscriptionMarketGoogle = "google"
const SubscriptionMarketStripe = "stripe"
const SubscriptionMarketSolana = "solana"
const SubscriptionMarketManual = "manual"

type SubscriptionRenewal struct {
	NetworkId          server.Id
	SubscriptionType   SubscriptionType
	StartTime          time.Time
	EndTime            time.Time
	NetRevenue         NanoCents
	PurchaseToken      string
	SubscriptionMarket SubscriptionMarket // google or apple
	TransactionId      string             // for tracking on Google Play or Apple App Store
}

func AddSubscriptionRenewalInTx(tx server.PgTx, ctx context.Context, renewal *SubscriptionRenewal) (returnErr error) {
	_, err := tx.Exec(
		ctx,
		`
			INSERT INTO subscription_renewal (
				network_id,
		        subscription_type,
		        start_time,
		        end_time,
		        net_revenue_nano_cents,
		        purchase_token,
						market,
						transaction_id
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (network_id, subscription_type, end_time, start_time) DO UPDATE
			SET
				net_revenue_nano_cents = $5,
				purchase_token = $6
		`,
		renewal.NetworkId,
		renewal.SubscriptionType,
		renewal.StartTime,
		renewal.EndTime,
		renewal.NetRevenue,
		renewal.PurchaseToken,
		renewal.SubscriptionMarket,
		renewal.TransactionId,
	)

	if err != nil {
		returnErr = err
	}

	return
}

func AddSubscriptionRenewal(ctx context.Context, renewal *SubscriptionRenewal) (returnErr error) {

	server.Tx(ctx, func(tx server.PgTx) {

		returnErr = AddSubscriptionRenewalInTx(tx, ctx, renewal)

	})

	return
}

func HasSubscriptionRenewal(
	ctx context.Context,
	networkId server.Id,
	subscriptionType SubscriptionType,
) (bool, *string) {
	active := false
	var market *string
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				MIN(market) AS market,
				COUNT(*) AS subscription_renewal_count
			FROM subscription_renewal
			WHERE
				network_id = $1
				AND subscription_type = $2
				AND start_time <= $3
				AND $3 < end_time;
			`,
			networkId,
			subscriptionType,
			server.NowUtc(),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var count int
				server.Raise(result.Scan(
					&market,
					&count,
				))
				active = (0 < count)
			}
		})
	})
	return active, market
}

func IsPro(
	ctx context.Context,
	networkId *server.Id,
) bool {

	// todo:
	// instead of checking if PAID, add a column 'is_pro'
	//
	// In some cases, like Apple TestFlight or Stripe promo codes,
	// we want to mark the user as pro even if payment is 0

	isPro := false

	server.Tx(ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			ctx,
			`
            SELECT
                balance_id,
                paid
            FROM transfer_balance
            WHERE
                network_id = $1 AND
                active = true
        `,
			networkId,
		)

		server.WithPgResult(result, err, func() {
			for result.Next() {
				var balanceId server.Id
				var paid bool

				server.Raise(result.Scan(
					&balanceId,
					&paid,
				))
				if paid {
					// check if any active paid balance exists
					isPro = true
					break
				}
			}
		})

	})

	return isPro

}

func AddRefreshTransferBalanceToAllNetworks(
	ctx context.Context,
	startTime time.Time,
	endTime time.Time,
	supporterTransferBalances map[bool]ByteCount,
) (addedTransferBalances map[server.Id]ByteCount) {
	addedTransferBalances = map[server.Id]ByteCount{}

	type supporterStatus struct {
		supporter           bool
		netRevenueNanoCents NanoCents
	}

	server.Tx(ctx, func(tx server.PgTx) {
		networkSupporterStatuses := map[server.Id]supporterStatus{}

		result, err := tx.Query(
			ctx,
			`
				SELECT
					network.network_id,
					subscription_renewal.net_revenue_nano_cents,
					subscription_renewal.start_time,
					subscription_renewal.end_time
				FROM network

				LEFT JOIN subscription_renewal ON
					subscription_renewal.network_id = network.network_id AND
					subscription_renewal.subscription_type = $1 AND
					subscription_renewal.start_time <= $2 AND
					$2 < subscription_renewal.end_time
			`,
			SubscriptionTypeSupporter,
			server.NowUtc(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				var netRevenueNanoCents *NanoCents
				var supporterStartTime *time.Time
				var supporterEndTime *time.Time
				server.Raise(result.Scan(
					&networkId,
					&netRevenueNanoCents,
					&supporterStartTime,
					&supporterEndTime,
				))
				status := supporterStatus{
					supporter: false,
				}
				if netRevenueNanoCents != nil {
					supportDurationFraction := float64(endTime.Sub(startTime)/time.Second) / float64(supporterEndTime.Sub(*supporterStartTime)/time.Second)
					status.supporter = true
					status.netRevenueNanoCents = NanoCents(supportDurationFraction * float64(*netRevenueNanoCents))
				}
				networkSupporterStatuses[networkId] = status
			}
		})

		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for networkId, status := range networkSupporterStatuses {
				balanceByteCount := supporterTransferBalances[status.supporter]
				batch.Queue(
					`
		                INSERT INTO transfer_balance (
		                    balance_id,
		                    network_id,
		                    start_time,
		                    end_time,
		                    start_balance_byte_count,
		                    net_revenue_nano_cents,
		                    subsidy_net_revenue_nano_cents,
		                    balance_byte_count
		                )
		                VALUES ($1, $2, $3, $4, $5, $6, $7, $5)
		            `,
					server.NewId(),
					networkId,
					startTime,
					endTime,
					balanceByteCount,
					0,
					status.netRevenueNanoCents,
				)
				addedTransferBalances[networkId] = balanceByteCount
			}
		})
	})
	return
}

func RemoveCompletedContracts(ctx context.Context, minTime time.Time) {
	maxRowCount := 50000

	var balanceIds []server.Id

	server.MaintenanceTx(ctx, func(tx server.PgTx) {

		// remove completed transfer balances
		result, err := tx.Query(
			ctx,
			`
			DELETE FROM transfer_balance
			WHERE
				end_time <= $1
			RETURNING balance_id
			`,
			minTime.UTC(),
		)
		balanceIds = []server.Id{}
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var balanceId server.Id
				server.Raise(result.Scan(&balanceId))
				balanceIds = append(balanceIds, balanceId)
			}
		})

	}, server.TxReadCommitted)

	server.Redis(ctx, func(r server.RedisClient) {
		r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			for _, balanceId := range balanceIds {
				pipe.Del(ctx, netEscrowKey(balanceId))
			}
			return nil
		})
	})

	server.MaintenanceTx(ctx, func(tx server.PgTx) {

		// remove completed transfer contracts
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM transfer_contract
		    USING (
		    	SELECT
				    DISTINCT contract_id
				FROM account_payment
				INNER JOIN transfer_escrow_sweep ON
				    transfer_escrow_sweep.payment_id = account_payment.payment_id

				WHERE account_payment.completed AND account_payment.complete_time <= $1
				ORDER BY contract_id
				LIMIT $2
		    ) t
			WHERE
			    transfer_contract.contract_id = t.contract_id
			`,
			minTime.UTC(),
			maxRowCount,
		))

	}, server.TxReadCommitted)

	server.MaintenanceTx(ctx, func(tx server.PgTx) {

		// remove closed transfer contracts that do not have a corresponding sweep
		// these are the result of some db corruption and we cannot recover them
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM transfer_contract
			USING (
				SELECT
					DISTINCT transfer_contract.contract_id
				FROM transfer_contract
				LEFT JOIN transfer_escrow_sweep ON transfer_escrow_sweep.contract_id = transfer_contract.contract_id
				WHERE
					transfer_contract.create_time < $1 AND
					transfer_contract.open = false AND
					transfer_escrow_sweep.contract_id IS NULL
				ORDER BY contract_id
				LIMIT $2
			) t
			WHERE transfer_contract.contract_id = t.contract_id
			`,
			minTime.UTC(),
			maxRowCount,
		))

	}, server.TxReadCommitted)

	server.MaintenanceTx(ctx, func(tx server.PgTx) {

		// (cascade) remove contract close where the transfer contract no longer exists
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM contract_close
			USING (
				SELECT
					DISTINCT contract_close.contract_id
		    	FROM contract_close
		        LEFT JOIN transfer_contract ON transfer_contract.contract_id = contract_close.contract_id
				WHERE transfer_contract.contract_id IS NULL
				ORDER BY contract_id
				LIMIT $1
			) t
			WHERE contract_close.contract_id = t.contract_id
			`,
			maxRowCount,
		))

	}, server.TxReadCommitted)

	server.MaintenanceTx(ctx, func(tx server.PgTx) {

		// (cascade) remove contract escrow where the transfer contract no longer exists
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM transfer_escrow
			USING (
				SELECT
					DISTINCT transfer_escrow.contract_id
	       		FROM transfer_escrow
				LEFT JOIN transfer_contract ON transfer_contract.contract_id = transfer_escrow.contract_id
				WHERE transfer_contract.contract_id IS NULL
				ORDER BY contract_id
				LIMIT $1
			) t
			WHERE transfer_escrow.contract_id = t.contract_id
			`,
			maxRowCount,
		))

	}, server.TxReadCommitted)

	server.MaintenanceTx(ctx, func(tx server.PgTx) {

		// (cascade) remove contract escrow sweep where the transfer contract no longer exists
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM transfer_escrow_sweep
			USING (
				SELECT
					DISTINCT transfer_escrow_sweep.contract_id
			   	FROM transfer_escrow_sweep
			    LEFT JOIN transfer_contract ON transfer_contract.contract_id = transfer_escrow_sweep.contract_id
			    WHERE transfer_contract.contract_id IS NULL
			    ORDER BY contract_id
			    LIMIT $1
			) t
			WHERE transfer_escrow_sweep.contract_id = t.contract_id;
			`,
			maxRowCount,
		))
	}, server.TxReadCommitted)
}

func GetOpenTransferByteCount(
	ctx context.Context,
	payerNetworkId server.Id,
) ByteCount {

	var openTransferByteCount ByteCount = 0

	server.Tx(ctx, func(tx server.PgTx) {

		result, err := tx.Query(
			ctx,
			`
			SELECT
			   COALESCE(SUM(transfer_byte_count), 0)
			FROM transfer_contract
			WHERE
			    payer_network_id = $1 AND
			    open = TRUE
			`,
			payerNetworkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {

				server.Raise(result.Scan(
					&openTransferByteCount,
				))

			}
		})

	})

	return openTransferByteCount

}

func runPosts(posts []func() any) {
	for 0 < len(posts) {
		// run all posts in parallel
		next := make(chan any)
		for _, post := range posts {
			go server.HandleError(func() {
				next <- post()
			}, func() {
				next <- nil
			})
		}

		var nextPosts []func() any
		for range len(posts) {
			r := <-next
			if r != nil {
				switch v := r.(type) {
				case []func() any:
					nextPosts = append(nextPosts, v...)
				case func() any:
					nextPosts = append(nextPosts, v)
				}
			}
		}
		posts = nextPosts
	}
}
