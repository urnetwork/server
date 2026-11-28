package model

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server/v2026"
)

func TestSetDeviceClientVersion(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		deviceId := server.NewId()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, server.NewId(), deviceId, clientId, "test", "test")

		read := func() (version *string) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT client_version FROM device WHERE device_id = $1`, deviceId).Scan(&version))
			})
			return
		}

		// an empty version writes nothing
		SetDeviceClientVersion(ctx, clientId, " ")
		assert.Equal(t, read(), (*string)(nil))

		SetDeviceClientVersion(ctx, clientId, "2026.10.18-1075514170")
		assert.Equal(t, *read(), "2026.10.18-1075514170")

		SetDeviceClientVersion(ctx, clientId, "2026.10.20-1076869840")
		assert.Equal(t, *read(), "2026.10.20-1076869840")

		// a client with no device is a no-op
		SetDeviceClientVersion(ctx, server.NewId(), "2026.10.20-1076869840")
	})
}
