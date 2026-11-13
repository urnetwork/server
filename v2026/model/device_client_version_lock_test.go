package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The version write never waits behind the device row's lockers: with the row
// held by another session it returns within its short bound, without
// remembering the version, so a later connection writes it once the row is
// free. Before, the write waited for the holder while pinning a pooled server.
func TestSetDeviceClientVersionDoesNotWaitOnABusyDevice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		deviceId := server.NewId()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, server.NewId(), deviceId, clientId, "test", "test")

		held := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `SELECT device_id FROM device WHERE device_id = $1 FOR UPDATE`, deviceId))
				close(held)
				time.Sleep(6 * time.Second)
			}, server.OptReadWrite())
		}()
		<-held

		start := time.Now()
		SetDeviceClientVersion(ctx, clientId, "2026.10.25-synthetic-1")
		if elapsed := time.Since(start); 3*time.Second < elapsed {
			t.Fatalf("the version write waited %s behind the device's locker", elapsed)
		}
		<-done

		SetDeviceClientVersion(ctx, clientId, "2026.10.25-synthetic-1")
		var version *string
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT client_version FROM device WHERE device_id = $1`, deviceId).Scan(&version))
		})
		if version == nil || *version != "2026.10.25-synthetic-1" {
			t.Fatalf("the version refused by the busy row was remembered, not written later: %v", version)
		}
	})
}
