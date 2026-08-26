package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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

func migration_20260807_ScrubTaskAndAuditClientAddresses(ctx context.Context) {
	ScrubTaskAndAuditClientAddresses(ctx)
}

// ScrubTaskAndAuditClientAddresses rewrites the raw client ip:port persisted
// before the task/audit hashing change into the peppered address hash
// (ClientIpHash), then blanks the raw value. Three sites stored raw addresses
// past the request: pending_task and finished_task rows (the address column,
// now written as ”), and the network-create audit blob in
// audit_network_event.event_details, which is permanent. Idempotent: every
// rewrite makes the row stop matching its selection predicate, so a re-run
// (or a crash partway) just continues where it left off. An unparseable
// stored address is blanked without a hash -- removing the raw value is the
// point; the hash is best effort.
//
// Beyond the one-time migration (index 544), this is exposed for
// `bringyourctl db scrub-client-addresses`: the migration ran while binaries
// that still write raw addresses were deployed, so rows written between the
// migration and the deploy carry raw addresses again. Task rows wash out via
// finished-task retention, but audit blobs are permanent -- re-run once after
// the address-hashing deploy.
func ScrubTaskAndAuditClientAddresses(ctx context.Context) (scrubbedTaskCount int, scrubbedAuditCount int) {
	// ReadCommitted, NOT the default RepeatableRead: this runs against a live
	// system where task workers continuously claim/reschedule pending_task
	// rows and the cleanup task deletes finished_task rows, so a
	// repeatable-read batch that selects 1000 rows and rewrites them
	// essentially always aborts on a concurrent update (40001) and exhausts
	// the retry window. Under read committed each rewrite applies to the
	// latest row version; it only touches the client_address columns, so
	// interleaving with claim updates is safe, and a concurrently deleted row
	// is a no-op. The rewrites are pipelined in one batch round trip because
	// this runs over an operator tunnel, where a round trip per row would
	// hold the batch's row locks for many seconds.
	// the task tables: client_address -> client_address_hash + port
	for _, table := range []string{"pending_task", "finished_task"} {
		for {
			type row struct {
				taskId        Id
				clientAddress string
			}
			rows := []row{}
			Tx(ctx, func(tx PgTx) {
				// the tx is retried on transient errors; do not carry rows
				// from a rolled-back attempt
				rows = rows[:0]
				result, err := tx.Query(
					ctx,
					`
						SELECT task_id, client_address
						FROM `+table+`
						WHERE client_address != ''
						LIMIT 1000
					`,
				)
				WithPgResult(result, err, func() {
					for result.Next() {
						r := row{}
						Raise(result.Scan(&r.taskId, &r.clientAddress))
						rows = append(rows, r)
					}
				})

				BatchInTx(ctx, tx, func(batch PgBatch) {
					for _, r := range rows {
						var hash []byte
						port := 0
						if ip, p, err := SplitClientAddress(r.clientAddress); err == nil {
							if h, err := ClientIpHash(ip); err == nil {
								hash = h[:]
								port = p
							}
						}
						batch.Queue(
							`
								UPDATE `+table+`
								SET client_address = '',
									client_address_hash = $2,
									client_address_port = $3
								WHERE task_id = $1
							`,
							r.taskId,
							hash,
							port,
						)
					}
				})
			}, TxReadCommitted)
			if len(rows) == 0 {
				break
			}
			scrubbedTaskCount += len(rows)
		}
	}

	// the network-create audit blobs: the raw address lives inside the
	// event_details json. only the top-level "client_address" key is touched;
	// the nested network_create payload is preserved as decoded. keyset
	// pagination on event_id (not bare LIMIT re-selection) so a row that
	// cannot be parsed is passed over exactly once instead of spinning the
	// loop forever; such rows are left intact rather than risk destroying an
	// audit event, and a later run passes over them again the same way. the
	// cursor advances only after the batch's tx commits, so a retried tx
	// re-selects the same batch instead of skipping past rolled-back
	// rewrites.
	lastEventId := Id{}
	for {
		type row struct {
			eventId      Id
			eventDetails string
		}
		rows := []row{}
		rewrittenCount := 0
		Tx(ctx, func(tx PgTx) {
			rows = rows[:0]
			rewrittenCount = 0
			result, err := tx.Query(
				ctx,
				`
					SELECT event_id, event_details
					FROM audit_network_event
					WHERE event_type = 'network_created'
						AND event_details LIKE '%"client_address"%'
						AND event_id > $1
					ORDER BY event_id
					LIMIT 1000
				`,
				lastEventId,
			)
			WithPgResult(result, err, func() {
				for result.Next() {
					r := row{}
					Raise(result.Scan(&r.eventId, &r.eventDetails))
					rows = append(rows, r)
				}
			})

			BatchInTx(ctx, tx, func(batch PgBatch) {
				for _, r := range rows {
					details := map[string]any{}
					if err := json.Unmarshal([]byte(r.eventDetails), &details); err != nil {
						// not json we understand; do not risk destroying the
						// event. the cursor advances past this row when the
						// batch commits, so skipping cannot loop.
						continue
					}
					clientAddress, ok := details["client_address"].(string)
					if !ok {
						// key absent or not a string; nothing raw stored here.
						// rewrite anyway to remove the literal key and release
						// the row from the selection predicate.
						delete(details, "client_address")
					} else {
						delete(details, "client_address")
						if ip, port, err := SplitClientAddress(clientAddress); err == nil {
							if h, err := ClientIpHash(ip); err == nil {
								details["client_address_hash"] = hex.EncodeToString(h[:])
								details["client_port"] = port
							}
						}
					}
					detailsJson, err := json.Marshal(details)
					if err != nil {
						continue
					}
					batch.Queue(
						`
							UPDATE audit_network_event
							SET event_details = $2
							WHERE event_id = $1
						`,
						r.eventId,
						string(detailsJson),
					)
					rewrittenCount += 1
				}
			})
		}, TxReadCommitted)
		if len(rows) == 0 {
			break
		}
		scrubbedAuditCount += rewrittenCount
		lastEventId = rows[len(rows)-1].eventId
	}
	return
}
