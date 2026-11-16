// The session state retention against PostgreSQL with the migrated schema
// (session_state_retention.go): heads go only once their client's events have
// left the journal and never with an open connection, offline device state
// goes a day after the device went offline and online state never, the passes
// resume from their cursors at their pace, the brake stops them, and a failed
// batch keeps its cursor.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
)

// Sets the brake, or removes it with nil fields.
func setSessionStateRetentionTestBrake(t testing.TB, ctx context.Context, fields map[string]string) {
	t.Helper()
	controlCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := RemoveSessionStateRetentionControl(controlCtx); err != nil {
		t.Fatal(err)
	}
	if err := SetSessionStateRetentionControl(controlCtx, fields); err != nil {
		t.Fatal(err)
	}
}

// A head at sequence for a client, as the append left it.
func insertSessionStateRetentionTestHead(t testing.TB, ctx context.Context, clientId server.Id, sequence int64) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_work_session_head (client_id, sequence) VALUES ($1, $2)`, clientId, sequence))
	})
}

func sessionStateRetentionTestHeadExists(t testing.TB, ctx context.Context, clientId server.Id) (exists bool) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM provider_work_session_head WHERE client_id = $1)`, clientId).Scan(&exists))
	})
	return
}

// A device state row as the sweep writes it.
func insertSessionStateRetentionTestDeviceState(t testing.TB, ctx context.Context, deviceId server.Id, online bool, eventTime time.Time) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO audit_device_state (device_id, network_id, online, event_time) VALUES ($1, $2, $3, $4)`, deviceId, server.NewId(), online, eventTime))
	})
}

func sessionStateRetentionTestDeviceStateExists(t testing.TB, ctx context.Context, deviceId server.Id) (exists bool) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_device_state WHERE device_id = $1)`, deviceId).Scan(&exists))
	})
	return
}

// Device ids in key order, minted a millisecond apart from at.
func sessionStateRetentionTestOrderedIds(at time.Time, count int) []server.Id {
	ids := make([]server.Id, count)
	for i := range ids {
		ids[i] = horizonTestContractId(at.Add(time.Duration(i) * time.Millisecond))
	}
	return ids
}

// The heads of an idle client (no event, no connection) goes; one whose
// client still has an open connection stays; a provider's with events in the
// journal stays; and one whose events sit in an old week stays until that
// week drops, and then goes, all through the head guard.
func TestSessionStateRetentionRemovesHeadsOnceTheirEventsAreGone(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		oldWeek := server.PartitionGranularityWeekly.AddRanges(server.PartitionGranularityWeekly.RangeStart(now), -14)
		server.Testing_PartitionProviderWorkSessionEventsAt(ctx, oldWeek)
		var family *server.PartitionFamily
		for _, registered := range server.PartitionFamilies() {
			if registered.Name == "provider_work_session_event" {
				family = registered
			}
		}
		if family == nil {
			t.Fatal("missing event family")
		}
		if _, err := family.EnsureRangeFor(ctx, now); err != nil {
			t.Fatal(err)
		}

		idle := server.NewId()
		insertSessionStateRetentionTestHead(t, ctx, idle, 2)

		connected, _, connectedHandlerId := newProviderWorkSessionProvidersClient(t, ctx)
		insertSessionStateRetentionTestHead(t, ctx, connected, 2)
		if _, _, _, _, err := ConnectNetworkClientWithIpFamily(ctx, connected, "192.0.2.60:10001", connectedHandlerId, 4); err != nil {
			t.Fatal(err)
		}

		journaled, _, journaledHandlerId := newProviderWorkSessionProvidersClient(t, ctx)
		addProviderWorkSessionTestProvideKeys(ctx, journaled)
		connectAndDisconnectProviderWorkSessionClient(t, ctx, journaled, journaledHandlerId, "192.0.2.61:10001")

		expiring, expiringNetworkId := server.NewId(), server.NewId()
		insertSessionStateRetentionTestHead(t, ctx, expiring, 2)
		server.Tx(ctx, func(tx server.PgTx) {
			for sequence, kind := range []string{"admit", "retire"} {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_work_session_event (client_id, sequence, network_id, connection_id, kind, observed_at, transaction_id)
					VALUES ($1, $2, $3, $4, $5, $6, txid_current())`, expiring, sequence+1, expiringNetworkId, horizonTestContractId(oldWeek), kind, oldWeek.Add(time.Duration(sequence+1)*time.Hour)))
			}
		}, server.TxReadCommitted)

		settings := DefaultSessionStateRetentionSettings()
		report, cursor := RemoveExpiredSessionState(ctx, now, SessionStateRetentionCursor{}, settings)
		if report.Head.Stop != SessionStateRetentionStopPass || report.Head.RemovedCount != 1 || report.Head.ExaminedCount != 4 {
			t.Fatalf("the first head pass %+v, want 1 of 4 removed", report.Head)
		}
		for name, want := range map[server.Id]bool{idle: false, connected: true, journaled: true, expiring: true} {
			if sessionStateRetentionTestHeadExists(t, ctx, name) != want {
				t.Fatalf("head %s exists %t after the first pass", name, !want)
			}
		}

		// the old week drops at its retention; the next pass, a day later,
		// removes the head its events kept
		if _, err := family.DropRange(ctx, server.PartitionGranularityWeekly.RangeOf(oldWeek)); err != nil {
			t.Fatal(err)
		}
		if report, cursor = RemoveExpiredSessionState(ctx, now, cursor, settings); report.Head.Stop != SessionStateRetentionStopWaiting || report.Head.ExaminedCount != 0 {
			t.Fatalf("a head pass before its interval %+v, want waiting", report.Head)
		}
		report, _ = RemoveExpiredSessionState(ctx, now.Add(settings.HeadPassInterval), cursor, settings)
		if report.Head.Stop != SessionStateRetentionStopPass || report.Head.RemovedCount != 1 {
			t.Fatalf("the next head pass %+v, want the expired client's head", report.Head)
		}
		for name, want := range map[server.Id]bool{connected: true, journaled: true, expiring: false} {
			if sessionStateRetentionTestHeadExists(t, ctx, name) != want {
				t.Fatalf("head %s exists %t after the week dropped", name, !want)
			}
		}
	})
}

// An offline row goes once it is a day past going offline, strictly; a row at
// the cutoff, a recent one and an online one stay, however old. The deletes
// and the run count in the retention's metrics.
func TestSessionStateRetentionRemovesOfflineDeviceStatesAfterADay(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultSessionStateRetentionSettings()
		now := server.NowUtc()
		cutoff := now.Add(-settings.DeviceStateRetention)
		due, atCutoff, recent, online := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		insertSessionStateRetentionTestDeviceState(t, ctx, due, false, cutoff.Add(-time.Second))
		insertSessionStateRetentionTestDeviceState(t, ctx, atCutoff, false, cutoff)
		insertSessionStateRetentionTestDeviceState(t, ctx, recent, false, cutoff.Add(time.Second))
		insertSessionStateRetentionTestDeviceState(t, ctx, online, true, now.Add(-200*24*time.Hour))

		deletedBefore := testutil.ToFloat64(retentionDeletedRowsCounter.WithLabelValues(RetentionTableAuditDeviceState))
		passesBefore := testutil.ToFloat64(sessionStateRetentionRunsCounter.WithLabelValues(SessionStateRetentionTableDeviceState, SessionStateRetentionStopPass))
		report, cursor := RemoveExpiredSessionState(ctx, now, SessionStateRetentionCursor{}, settings)
		if report.DeviceState.Stop != SessionStateRetentionStopPass || report.DeviceState.RemovedCount != 1 || report.DeviceState.ExaminedCount != 4 || report.More() {
			t.Fatalf("the device state pass %+v, want 1 of 4 removed", report.DeviceState)
		}
		for name, want := range map[server.Id]bool{due: false, atCutoff: true, recent: true, online: true} {
			if sessionStateRetentionTestDeviceStateExists(t, ctx, name) != want {
				t.Fatalf("device state %s exists %t", name, !want)
			}
		}
		if cursor.DeviceStateAfterDeviceId != nil || cursor.DeviceStatePassStartTime == nil || !cursor.DeviceStatePassStartTime.Equal(now) {
			t.Fatalf("a complete pass left cursor %+v", cursor)
		}
		if deleted := testutil.ToFloat64(retentionDeletedRowsCounter.WithLabelValues(RetentionTableAuditDeviceState)) - deletedBefore; deleted != 1 {
			t.Fatalf("%v deletes counted", deleted)
		}
		if passes := testutil.ToFloat64(sessionStateRetentionRunsCounter.WithLabelValues(SessionStateRetentionTableDeviceState, SessionStateRetentionStopPass)) - passesBefore; passes != 1 {
			t.Fatalf("%v passes counted", passes)
		}
	})
}

// At one row a second a batch of two ends the run, since the next batch could
// not start inside its budget, and the next run resumes after the cursor: a
// due row before the cursor waits for the next pass, which starts no sooner
// than its interval after the last one began.
func TestSessionStateRetentionResumesFromItsCursorAtItsPace(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultSessionStateRetentionSettings()
		settings.DeviceStateBatchCount = 2
		settings.RunBudget = time.Second
		now := server.NowUtc()
		offlineAt := now.Add(-settings.DeviceStateRetention - time.Hour)
		ids := sessionStateRetentionTestOrderedIds(now.Add(-time.Hour), 6)
		earlier, due := ids[0], ids[1:]
		for _, deviceId := range due {
			insertSessionStateRetentionTestDeviceState(t, ctx, deviceId, false, offlineAt)
		}
		setSessionStateRetentionTestBrake(t, ctx, map[string]string{
			SessionStateRetentionControlFieldMaxHeadsPerSecond:        "0",
			SessionStateRetentionControlFieldMaxDeviceStatesPerSecond: "1",
		})
		run := func(at time.Time, cursor SessionStateRetentionCursor) (SessionStateRetentionPassReport, SessionStateRetentionCursor) {
			t.Helper()
			report, next := RemoveExpiredSessionState(ctx, at, cursor, settings)
			if report.Head.Stop != SessionStateRetentionStopBrakeOff {
				t.Fatalf("the head pass ran with its rate at zero: %+v", report.Head)
			}
			return report.DeviceState, next
		}

		report, cursor := run(now, SessionStateRetentionCursor{})
		if report.Stop != SessionStateRetentionStopBudget || !report.More || report.RemovedCount != 2 || cursor.DeviceStateAfterDeviceId == nil || *cursor.DeviceStateAfterDeviceId != due[1] {
			t.Fatalf("the first run %+v left cursor %+v, want one batch of 2 and more", report, cursor)
		}
		insertSessionStateRetentionTestDeviceState(t, ctx, earlier, false, offlineAt)
		if report, cursor = run(now, cursor); report.Stop != SessionStateRetentionStopBudget || report.RemovedCount != 2 || *cursor.DeviceStateAfterDeviceId != due[3] {
			t.Fatalf("the resumed run %+v left cursor %+v", report, cursor)
		}
		if !sessionStateRetentionTestDeviceStateExists(t, ctx, earlier) {
			t.Fatal("the resumed run went back before its cursor")
		}

		setSessionStateRetentionTestBrake(t, ctx, map[string]string{SessionStateRetentionControlFieldMaxHeadsPerSecond: "0"})
		settings.RunBudget = time.Minute
		if report, cursor = run(now, cursor); report.Stop != SessionStateRetentionStopPass || report.RemovedCount != 1 || cursor.DeviceStateAfterDeviceId != nil {
			t.Fatalf("the last run of the pass %+v left cursor %+v", report, cursor)
		}
		if report, cursor = run(now.Add(settings.DeviceStatePassInterval-time.Second), cursor); report.Stop != SessionStateRetentionStopWaiting || report.ExaminedCount != 0 {
			t.Fatalf("a pass before its interval %+v", report)
		}
		if report, _ = run(now.Add(settings.DeviceStatePassInterval), cursor); report.Stop != SessionStateRetentionStopPass || report.RemovedCount != 1 {
			t.Fatalf("the next pass %+v, want the earlier row", report)
		}
		for _, deviceId := range ids {
			if sessionStateRetentionTestDeviceStateExists(t, ctx, deviceId) {
				t.Fatalf("device state %s stayed", deviceId)
			}
		}
	})
}

// The brake stops both passes when off, one when its rate is zero, and both
// when it cannot be read; without it both run.
func TestSessionStateRetentionBrake(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultSessionStateRetentionSettings()
		now := server.NowUtc()
		head, deviceState := server.NewId(), server.NewId()
		insertSessionStateRetentionTestHead(t, ctx, head, 2)
		insertSessionStateRetentionTestDeviceState(t, ctx, deviceState, false, now.Add(-48*time.Hour))
		run := func() *SessionStateRetentionReport {
			report, _ := RemoveExpiredSessionState(ctx, now, SessionStateRetentionCursor{}, settings)
			return report
		}

		setSessionStateRetentionTestBrake(t, ctx, map[string]string{SessionStateRetentionControlFieldMode: "off"})
		if report := run(); report.Head.Stop != SessionStateRetentionStopBrakeOff || report.DeviceState.Stop != SessionStateRetentionStopBrakeOff || report.Head.RemovedCount+report.DeviceState.RemovedCount != 0 {
			t.Fatalf("mode off: %+v", report)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, sessionStateRetentionControlKey, "not a hash", 0).Err())
		})
		if report := run(); report.Head.Stop != SessionStateRetentionStopBrakeUnread || report.DeviceState.Stop != SessionStateRetentionStopBrakeUnread {
			t.Fatalf("an unreadable brake: %+v", report)
		}
		setSessionStateRetentionTestBrake(t, ctx, map[string]string{SessionStateRetentionControlFieldMaxDeviceStatesPerSecond: "0"})
		if report := run(); report.Head.RemovedCount != 1 || report.DeviceState.Stop != SessionStateRetentionStopBrakeOff || !sessionStateRetentionTestDeviceStateExists(t, ctx, deviceState) {
			t.Fatalf("device states at zero: %+v", report)
		}
		setSessionStateRetentionTestBrake(t, ctx, nil)
		if report := run(); report.DeviceState.RemovedCount != 1 {
			t.Fatalf("without a brake: %+v", report)
		}
	})
}

// A batch that fails ends its pass's run with an error and the cursor of its
// last committed batch, and the next run retries it; the other pass runs on.
func TestSessionStateRetentionFailedBatchKeepsItsCursor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultSessionStateRetentionSettings()
		now := server.NowUtc()
		head, deviceState := server.NewId(), server.NewId()
		insertSessionStateRetentionTestHead(t, ctx, head, 2)
		insertSessionStateRetentionTestDeviceState(t, ctx, deviceState, false, now.Add(-48*time.Hour))
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE audit_device_state RENAME TO audit_device_state_away`))
		})
		report, cursor := RemoveExpiredSessionState(ctx, now, SessionStateRetentionCursor{}, settings)
		if report.DeviceState.Stop != SessionStateRetentionStopError || !report.More() || cursor.DeviceStateAfterDeviceId != nil || report.Head.RemovedCount != 1 {
			t.Fatalf("a failing device state batch: %+v, cursor %+v", report, cursor)
		}
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE audit_device_state_away RENAME TO audit_device_state`))
		})
		if report, _ = RemoveExpiredSessionState(ctx, now, cursor, settings); report.DeviceState.Stop != SessionStateRetentionStopPass || report.DeviceState.RemovedCount != 1 {
			t.Fatalf("the retry: %+v", report.DeviceState)
		}
	})
}
