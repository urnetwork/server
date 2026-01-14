package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

type BalanceCode struct {
	BalanceCodeId    server.Id
	StartTime        time.Time
	CreateTime       time.Time
	EndTime          time.Time
	RedeemTime       time.Time
	BalanceByteCount ByteCount
	NetRevenue       NanoCents
	Secret           string
	PurchaseEventId  string
	PurchaseRecord   string
	PurchaseEmail    string
}

type RedeemBalanceCodeArgs struct {
	Secret    string    `json:"secret"`
	NetworkId server.Id `json:"network_id"`
}

type RedeemBalanceCodeResult struct {
	TransferBalance *RedeemBalanceCodeTransferBalance `json:"transfer_balance,omitempty"`
	Error           *RedeemBalanceCodeError           `json:"error,omitempty"`
}

type RedeemBalanceCodeTransferBalance struct {
	TransferBalanceId server.Id `json:"transfer_balance_id"`
	StartTime         time.Time `json:"start_time"`
	EndTime           time.Time `json:"end_time"`
	BalanceByteCount  ByteCount `json:"balance_byte_count"`
}

type RedeemBalanceCodeError struct {
	Message string `json:"message"`
}

func RedeemBalanceCodeInTx(
	redeemBalanceCode *RedeemBalanceCodeArgs,
	ctx context.Context,
	tx server.PgTx,
) (redeemBalanceCodeResult *RedeemBalanceCodeResult, returnErr error) {

	result, err := tx.Query(
		ctx,
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
	server.WithPgResult(result, err, func() {
		if result.Next() {
			balanceCode = &BalanceCode{}
			server.Raise(result.Scan(
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

	balanceId := server.NewId()

	now := server.NowUtc()
	duration := balanceCode.EndTime.Sub(balanceCode.StartTime)
	endTime := now.Add(duration)

	server.RaisePgResult(tx.Exec(
		ctx,
		`
            UPDATE transfer_balance_code
            SET
                redeem_time = $2,
                redeem_balance_id = $3,
                start_time = $4,
                end_time = $5,
                network_id = $6
            WHERE
                balance_code_id = $1
        `,
		balanceCode.BalanceCodeId,
		now,
		balanceId,
		now,
		endTime,
		redeemBalanceCode.NetworkId,
	))

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
                    net_revenue_nano_cents
                )
                VALUES ($1, $2, $3, $4, $5, $5, $6)
            `,
		balanceId,
		// note: we don't use session.Jwt.NetworkId here
		// users can redeem when creating a network, in which case the jwt is not yet threaded
		redeemBalanceCode.NetworkId,
		now,
		endTime,
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

	return
}

func RedeemBalanceCode(
	redeemBalanceCode *RedeemBalanceCodeArgs,
	ctx context.Context,
) (redeemBalanceCodeResult *RedeemBalanceCodeResult, returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		redeemBalanceCodeResult, returnErr = RedeemBalanceCodeInTx(
			redeemBalanceCode,
			ctx,
			tx,
		)
	}, server.TxReadCommitted)

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
	server.Tx(session.Ctx, func(tx server.PgTx) {
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
		server.WithPgResult(result, err, func() {
			if result.Next() {
				balanceCode = &BalanceCode{}
				server.Raise(result.Scan(
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
	}, server.TxReadCommitted)

	return
}

// typically called from a payment webhook
func CreateBalanceCode(
	ctx context.Context,
	balanceByteCount ByteCount,
	duration time.Duration,
	netRevenue NanoCents,
	purchaseEventId string,
	purchaseRecord string,
	purchaseEmail string,
) (balanceCode *BalanceCode, returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		purchaseEventIdExists := false

		result, err := tx.Query(
			ctx,
			`
                SELECT balance_code_id FROM transfer_balance_code
                WHERE purchase_event_id = $1
            `,
			purchaseEventId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				purchaseEventIdExists = true
			}
		})

		if purchaseEventIdExists {
			// the `purchaseEventId` was already converted into a balance code
			returnErr = fmt.Errorf("Purchase event already exists.")
			return
		}

		balanceCodeId := server.NewId()

		secret, err := newCodeBase32()
		if err != nil {
			returnErr = err
			return
		}

		createTime := server.NowUtc()
		// round down to 00:00 the day of create time
		startTime := time.Date(
			createTime.Year(),
			createTime.Month(),
			createTime.Day(),
			0, 0, 0, 0,
			createTime.Location(),
		)
		// round up to 00:00 the next day of create time + duration
		endTime := startTime.Add(24 * time.Hour).Add(duration)

		server.RaisePgResult(tx.Exec(
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

func GetBalanceCode(
	ctx context.Context,
	balanceCodeId server.Id,
) (balanceCode *BalanceCode, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
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
		server.WithPgResult(result, err, func() {
			if result.Next() {
				balanceCode = &BalanceCode{
					BalanceCodeId: balanceCodeId,
				}
				server.Raise(result.Scan(
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

func GetBalanceCodeIdForPurchaseEventId(ctx context.Context, purchaseEventId string) (balanceCodeId server.Id, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT balance_code_id FROM transfer_balance_code
                WHERE purchase_event_id = $1
            `,
			purchaseEventId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&balanceCodeId))
			} else {
				returnErr = fmt.Errorf("Purchase event not found.")
			}
		})
	})
	return
}

type RedeemedBalanceCode struct {
	BalanceCodeId    server.Id `json:"balance_code_id"`
	BalanceByteCount ByteCount `json:"balance_byte_count"`
	RedeemTime       time.Time `json:"redeem_time"`
	EndTime          time.Time `json:"end_time"`
}

func FetchNetworkRedeemedBalanceCodes(
	session *session.ClientSession,
) ([]*RedeemedBalanceCode, error) {

	var balanceCodes []*RedeemedBalanceCode

	server.Db(session.Ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			session.Ctx,
			`
			SELECT
				balance_code_id,
				balance_byte_count,
				redeem_time,
				end_time
			FROM transfer_balance_code
			WHERE
				network_id = $1 AND redeemed = TRUE AND end_time > NOW()
			ORDER BY end_time ASC
           `,
			session.ByJwt.NetworkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				balanceCode := &RedeemedBalanceCode{}
				server.Raise(result.Scan(
					&balanceCode.BalanceCodeId,
					&balanceCode.BalanceByteCount,
					&balanceCode.RedeemTime,
					&balanceCode.EndTime,
				))
				balanceCodes = append(balanceCodes, balanceCode)
			}
		})

	})
	return balanceCodes, nil
}
