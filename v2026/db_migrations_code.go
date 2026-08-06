package server

import (
	"context"
	"crypto/rand"
	// "time"
)

// create entries for `network_client.device_id`
func migration_20240124_PopulateDevice(ctx context.Context) {
	Tx(ctx, func(tx PgTx) {
		result, err := tx.Query(
			ctx,
			`
					SELECT
							client_id,
							network_id,
							description,
							device_spec
					FROM network_client
					WHERE
							device_id IS NULL
			`,
		)
		type Device struct {
			deviceId   Id
			networkId  Id
			deviceName string
			deviceSpec string
		}
		devices := map[Id]*Device{}
		WithPgResult(result, err, func() {
			for result.Next() {
				var clientId Id
				device := &Device{
					deviceId: NewId(),
				}
				Raise(result.Scan(
					&clientId,
					&device.networkId,
					&device.deviceName,
					&device.deviceSpec,
				))
				devices[clientId] = device
			}
		})

		createTime := NowUtc()

		for clientId, device := range devices {
			RaisePgResult(tx.Exec(
				ctx,
				`
                INSERT INTO device (
                    device_id,
                    network_id,
                    device_name,
                    device_spec,
                    create_time
                ) VALUES ($1, $2, $3, $4, $5)
                `,
				device.deviceId,
				device.networkId,
				device.deviceName,
				device.deviceSpec,
				createTime,
			))

			RaisePgResult(tx.Exec(
				ctx,
				`
                UPDATE network_client
                SET
                    device_id = $2
                WHERE
                    client_id = $1
                `,
				clientId,
				device.deviceId,
			))
		}
	})
}

func migration_20240725_PopulateNetworkReferralCodes(ctx context.Context) {
	Tx(ctx, func(tx PgTx) {
		result, err := tx.Query(
			ctx,
			`
					SELECT
							network_id
					FROM network
			`,
		)
		networkIds := []Id{}
		WithPgResult(result, err, func() {
			for result.Next() {
				var networkId Id
				Raise(result.Scan(
					&networkId,
				))
				networkIds = append(networkIds, networkId)
			}
		})

		for _, networkId := range networkIds {
			code := NewId()
			RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO network_referral_code (
							network_id,
							referral_code
					) VALUES ($1, $2)
				`,
				networkId,
				code,
			))
		}
	})
}

func migration_20240802_AccountPaymentPopulateCircleWalletId(ctx context.Context) {
	Tx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE account_wallet
				SET circle_wallet_id = wallet_id
			`,
		))
	})
}

func migration_20250402_ReferralCodeToAlphaNumeric(ctx context.Context) {

	Tx(ctx, func(tx PgTx) {

		result, err := tx.Query(
			ctx,
			`
	        SELECT network_id FROM network_referral_code
			`,
		)
		networkIds := []Id{}
		WithPgResult(result, err, func() {
			for result.Next() {

				var networkId Id

				Raise(result.Scan(
					&networkId,
				))

				networkIds = append(
					networkIds,
					networkId,
				)
			}
		})

		for _, networkId := range networkIds {

			code := generateAlphanumericCode(6)

			RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE network_referral_code
					SET referral_code = $2
					WHERE network_id = $1
				`,
				networkId,
				code,
			))

		}

	})

}

func generateAlphanumericCode(length int) string {
	const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	code := make([]byte, length)

	randomBytes := make([]byte, length)
	if _, err := rand.Read(randomBytes); err != nil {
		panic(err)
	}

	for i := range randomBytes {
		code[i] = charset[randomBytes[i]%byte(len(charset))]
	}

	return string(code)
}
