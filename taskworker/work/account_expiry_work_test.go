package work

import (
	"crypto/rand"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// An id minted at t: the rate limit tables key their rows by server.NewId,
// which leads with its creation millisecond.
func accountExpiryTestIdAt(t time.Time) server.Id {
	id := server.ContractIdFloor(t)
	if _, err := rand.Read(id[6:]); err != nil {
		panic(err)
	}
	return id
}

// The hourly account expiry removes the rate limit histories, the reset codes
// and the name cool-downs that are past every reader, and keeps every row a
// reader can still count: an attempt inside its 24-hour window or its day of
// margin, a reset code issued an hour ago, a device add inside the last 30
// minutes, and a name still cooling down or just released.
func TestRemoveExpiredAuthCodesRemovesEndedAccountRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		userId, networkId := server.NewId(), server.NewId()
		ago := func(duration time.Duration) time.Time { return now.Add(-duration) }

		type row struct {
			table string
			id    server.Id
			keep  bool
		}
		rows := []row{}
		insertAttempt := func(at time.Time, keep bool) {
			id := accountExpiryTestIdAt(at)
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `
					INSERT INTO network_user_action_attempt (network_user_action_attempt_id, user_id, action, create_time)
					VALUES ($1, $2, 'test_action', $3)
				`, id, userId, at))
			})
			rows = append(rows, row{"network_user_action_attempt", id, keep})
		}
		insertCreateAttempt := func(at time.Time, keep bool) {
			id := accountExpiryTestIdAt(at)
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `
					INSERT INTO network_create_attempt (network_create_attempt_id, client_address_hash, create_time)
					VALUES ($1, $2, $3)
				`, id, id.Bytes(), at))
			})
			rows = append(rows, row{"network_create_attempt", id, keep})
		}
		insertReset := func(at time.Time, keep bool) {
			id := accountExpiryTestIdAt(at)
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `
					INSERT INTO user_auth_reset (user_auth_reset_id, user_id, reset_time, reset_code)
					VALUES ($1, $2, $3, $4)
				`, id, userId, at, id.String()))
			})
			rows = append(rows, row{"user_auth_reset", id, keep})
		}
		insertDeviceAdd := func(at time.Time, keep bool) {
			id := server.NewId()
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `
					INSERT INTO device_add_history (network_id, add_time, device_add_id, code)
					VALUES ($1, $2, $3, 'test')
				`, networkId, at, id))
			})
			rows = append(rows, row{"device_add_history", id, keep})
		}
		names := map[server.Id]string{}
		insertNameReclaim := func(coolDownUntil time.Time, keep bool) {
			id := server.NewId()
			names[id] = "reclaim-" + id.String()
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `
					INSERT INTO network_name_reclaim (old_name, cool_down_until) VALUES ($1, $2)
				`, names[id], coolDownUntil))
			})
			rows = append(rows, row{"network_name_reclaim", id, keep})
		}

		insertAttempt(ago(72*time.Hour), false)
		insertAttempt(ago(30*time.Hour), true)
		insertAttempt(ago(time.Hour), true)
		insertCreateAttempt(ago(72*time.Hour), false)
		insertCreateAttempt(ago(30*time.Hour), true)
		insertCreateAttempt(ago(time.Hour), true)
		insertReset(ago(8*24*time.Hour), false)
		insertReset(ago(time.Hour), true)
		insertDeviceAdd(ago(8*24*time.Hour), false)
		insertDeviceAdd(ago(10*time.Minute), true)
		insertNameReclaim(ago(8*24*time.Hour), false)
		insertNameReclaim(ago(24*time.Hour), true)
		insertNameReclaim(now.Add(12*time.Hour), true)

		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		if _, err := RemoveExpiredAuthCodes(&RemoveExpiredAuthCodesArgs{}, clientSession); err != nil {
			t.Fatal(err)
		}

		for _, r := range rows {
			var exists bool
			server.Db(ctx, func(conn server.PgConn) {
				var sql string
				var arg any = r.id
				switch r.table {
				case "network_user_action_attempt":
					sql = `SELECT EXISTS (SELECT 1 FROM network_user_action_attempt WHERE network_user_action_attempt_id = $1)`
				case "network_create_attempt":
					sql = `SELECT EXISTS (SELECT 1 FROM network_create_attempt WHERE network_create_attempt_id = $1)`
				case "user_auth_reset":
					sql = `SELECT EXISTS (SELECT 1 FROM user_auth_reset WHERE user_auth_reset_id = $1)`
				case "device_add_history":
					sql = `SELECT EXISTS (SELECT 1 FROM device_add_history WHERE device_add_id = $1)`
				case "network_name_reclaim":
					sql = `SELECT EXISTS (SELECT 1 FROM network_name_reclaim WHERE old_name = $1)`
					arg = names[r.id]
				}
				server.Raise(conn.QueryRow(ctx, sql, arg).Scan(&exists))
			})
			if exists != r.keep {
				t.Fatalf("%s row kept=%t after the account expiry, want kept=%t", r.table, exists, r.keep)
			}
		}
	})
}
