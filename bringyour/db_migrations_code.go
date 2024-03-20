package bringyour

import (
    "context"
    // "time"
)

// create entries for `network_client.device_id`
func migration_20240124_PopulateDevice(ctx context.Context) {
    Raise(Tx(ctx, func(tx PgTx) {
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
            deviceId Id
            networkId Id
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
    }))
}
