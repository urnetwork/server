//go:build sidecar_pending

package controller

// smtpedge's taskworker jobs (SMTP-M9-17) on a private database: each chain fleet-wide run-once, its next run
// carrying the RUA check's cursor; the RUA check over the real store opening §21's case for a network over by
// 21% + 5 and none at 20% + 5, with no halt; and a retention run dropping a day past 30 days and the rows past 13
// months in one run.

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The pending rows of a task function and their args.
func smtpTestPendingTasks(t testing.TB, ctx context.Context, functionName string) (argsJsons []string) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT args_json FROM pending_task WHERE function_name = $1`, functionName)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var argsJson string
				server.Raise(result.Scan(&argsJson))
				argsJsons = append(argsJsons, argsJson)
			}
		})
	})
	return
}

func smtpTestClientSession(t testing.TB, ctx context.Context) *session.ClientSession {
	t.Helper()
	// task scheduling hashes the local synthetic client address
	t.Cleanup(server.Vault.PushSimpleResource(
		"client.yml",
		[]byte("client_ip_hash_pepper: synthetic-smtp-task-test-only\n"),
	))
	clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
	t.Cleanup(clientSession.Cancel)
	return clientSession
}

// Each of the four chains has one pending row however often it is seeded, and a pending RUA run keeps its cursor
// when the start seeds the chain again.
func TestSmtpTasksAreFleetWideRunOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := smtpTestClientSession(t, ctx)
		cursor := &SmtpReconcileArgs{Day: time.Date(2031, time.March, 10, 0, 0, 0, 0, time.UTC), AfterNetworkId: server.NewId()}
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleSmtpRetention(clientSession, tx)
			ScheduleSmtpRetention(clientSession, tx)
			scheduleSmtpReconcile(clientSession, tx, cursor, server.NowUtc().Add(time.Hour))
			ScheduleSmtpReconcile(clientSession, tx)
		})
		if rows := smtpTestPendingTasks(t, ctx, task.NewTaskTarget(SmtpRetention).TargetFunctionName()); len(rows) != 1 {
			t.Fatalf("retention has %d pending rows", len(rows))
		}
		rows := smtpTestPendingTasks(t, ctx, task.NewTaskTarget(SmtpReconcileTask).TargetFunctionName())
		if len(rows) != 1 || !slices.ContainsFunc(rows, func(argsJson string) bool {
			return argsJson == fmt.Sprintf(`{"day":"2031-03-10T00:00:00Z","after_network_id":"%s"}`, cursor.AfterNetworkId)
		}) {
			t.Fatalf("the reconcile chain's rows %v", rows)
		}
	})
}

// A send of networkId at start to recipientCount recipients, as the edge records it.
func smtpTestSend(networkId sidecar.Id, start time.Time, recipientCount uint32) *smtpedgeservice.SmtpSendRecord {
	sendId := make([]byte, 12)
	if _, err := rand.Read(sendId); err != nil {
		panic(err)
	}
	return &smtpedgeservice.SmtpSendRecord{
		SendId:           sendId,
		NetworkId:        networkId,
		ClientId:         sidecar.NewId(),
		SendingAddress:   netip.MustParseAddr("192.0.2.10"),
		RecipientDomain:  "recipient.example",
		MxHost:           "mx.recipient.example",
		StartTime:        start,
		EndTime:          start.Add(time.Minute),
		ByteCount:        4096,
		XUrRefKeyId:      1,
		RecipientCount:   recipientCount,
		MessageByteCount: 2048,
		TlsVersion:       0x0304,
		TlsLevel:         smtpedgeservice.SmtpTlsLevelDane,
		Result:           "sent",
	}
}

// A DMARC report of messageCount messages for networkId on day.
func smtpTestAggregate(networkId sidecar.Id, day time.Time, messageCount uint64) *smtpedgeservice.SmtpAggregateReport {
	reportId := make([]byte, 8)
	if _, err := rand.Read(reportId); err != nil {
		panic(err)
	}
	return &smtpedgeservice.SmtpAggregateReport{
		Reporter:    "reporter.example",
		ReportId:    fmt.Sprintf("report-%x", reportId),
		Kind:        smtpedgeservice.SmtpAggregateKindDmarc,
		Day:         day,
		ArrivalTime: day.Add(30 * time.Hour),
		Counts: []*smtpedgeservice.SmtpAggregateCount{{
			NetworkId:     networkId,
			Domain:        "quietnet.root.example",
			MessageCount:  messageCount,
			DkimPassCount: messageCount,
		}},
	}
}

// The network's open cases as §21 holds them: their category, target kind, reporter class and join kind.
func smtpTestOpenCases(t testing.TB, ctx context.Context, networkId server.Id) (cases []string) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT category, target_kind, reporter_class, join_kind FROM abuse_case WHERE network_id = $1 AND state = 'open'`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var category, targetKind, reporterClass, joinKind string
				server.Raise(result.Scan(&category, &targetKind, &reporterClass, &joinKind))
				cases = append(cases, category+" "+targetKind+" "+reporterClass+" "+joinKind)
			}
		})
	})
	return
}

// Over the real store and §21's case opening: 100 proven recipients and 126 reported messages, 21% + 5 above
// them, opens the network's spam case for review; 125, 20% + 5, opens none; a one-connection send to ten recipients
// with ten reported raises nothing; no case acts, and the cursor moves past the day; a second pass over the same
// day adds to the open case, never a second one.
func TestSmtpReconcileOpensACaseOverTwentyOnePercentAndFive(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		day := server.PartitionGranularityDaily.RangeStart(now.AddDate(0, 0, -5))
		over := sidecar.NewId()
		within := sidecar.NewId()
		single := sidecar.NewId()
		for _, networkId := range []sidecar.Id{over, within, single} {
			model.Testing_CreateNetwork(ctx, server.Id(networkId), "smtp-"+server.Id(networkId).String()[24:], server.NewId())
		}
		sends := []*smtpedgeservice.SmtpSendRecord{
			smtpTestSend(over, day.Add(time.Hour), 60),
			smtpTestSend(over, day.Add(2*time.Hour), 40),
			smtpTestSend(within, day.Add(time.Hour), 100),
			smtpTestSend(single, day.Add(time.Hour), 10),
		}
		if result, err := model.InsertSmtpSends(ctx, sends); err != nil || result.StoredCount != len(sends) {
			t.Fatalf("sends %+v, %v", result, err)
		}
		for networkId, messageCount := range map[sidecar.Id]uint64{over: 126, within: 125, single: 10} {
			if inserted, err := model.InsertSmtpAggregate(ctx, smtpTestAggregate(networkId, day, messageCount)); err != nil || !inserted {
				t.Fatalf("aggregate %t, %v", inserted, err)
			}
		}

		settings := DefaultSmtpTaskSettings()
		result := runSmtpReconcile(ctx, &SmtpReconcileArgs{Day: day}, now, model.NewSmtpEdgeStore(nil), NewSmtpReconcileCaseSink(nil), settings)
		if result.Failed || result.JudgedCount != 3 || result.ExceededCount != 1 {
			t.Fatalf("pass %+v", result)
		}
		if !result.Next.Day.After(day) || result.Next.AfterNetworkId != (server.Id{}) {
			t.Fatalf("the cursor %+v", result.Next)
		}
		want := []string{string(model.AbuseCategorySpam) + " " + string(model.AbuseTargetKindNetwork) + " " + string(model.AbuseReporterClassMailReport) + " " + string(model.AbuseJoinKindNetwork)}
		if cases := smtpTestOpenCases(t, ctx, server.Id(over)); !slices.Equal(cases, want) {
			t.Fatalf("the network over has %v", cases)
		}
		for _, networkId := range []sidecar.Id{within, single} {
			if cases := smtpTestOpenCases(t, ctx, server.Id(networkId)); len(cases) != 0 {
				t.Fatalf("a network within has %v", cases)
			}
		}
		// the signal is review alone: no action, no halt
		server.Db(ctx, func(conn server.PgConn) {
			var actionCount int
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM abuse_action WHERE network_id = $1`, server.Id(over)).Scan(&actionCount))
			var haltCount int
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM smtp_halt WHERE network_id = $1`, server.Id(over)).Scan(&haltCount))
			if actionCount != 0 || haltCount != 0 {
				t.Fatalf("%d actions and %d halts", actionCount, haltCount)
			}
		})
		// the day judged again adds to the open case
		again := runSmtpReconcile(ctx, &SmtpReconcileArgs{Day: day}, now, model.NewSmtpEdgeStore(nil), NewSmtpReconcileCaseSink(nil), settings)
		if again.Failed || again.ExceededCount != 1 {
			t.Fatalf("again %+v", again)
		}
		if cases := smtpTestOpenCases(t, ctx, server.Id(over)); len(cases) != 1 {
			t.Fatalf("a second pass made %v", cases)
		}
	})
}

// A chain with no cursor starts at the newest settled day, so its first pass asks §21 about no older one.
func TestSmtpReconcileWithNoCursorStartsAtTheNewestSettledDay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		settings := DefaultSmtpTaskSettings()
		older := server.PartitionGranularityDaily.RangeStart(now.AddDate(0, 0, -6))
		networkId := sidecar.NewId()
		if inserted, err := model.InsertSmtpAggregate(ctx, smtpTestAggregate(networkId, older, 1000)); err != nil || !inserted {
			t.Fatalf("aggregate %t, %v", inserted, err)
		}
		result := runSmtpReconcile(ctx, &SmtpReconcileArgs{}, now, model.NewSmtpEdgeStore(nil), NewSmtpReconcileCaseSink(nil), settings)
		if result.Failed || result.ExceededCount != 0 {
			t.Fatalf("pass %+v", result)
		}
		newestSettled := server.PartitionGranularityDaily.RangeStart(now.Add(-settings.Reconcile.SettleTimeout)).Add(-24 * time.Hour)
		if !result.Next.Day.Equal(newestSettled.Add(24 * time.Hour)) {
			t.Fatalf("the cursor %v, want the day after %v", result.Next.Day, newestSettled)
		}
	})
}

// One retention run makes the runway, drops a day past its 30 days and removes the rows past their 13 months.
func TestSmtpRetentionRunDropsExpiredDaysAndRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		settings := DefaultSmtpTaskSettings()
		networkId := sidecar.NewId()
		// a day 31 days past, its leaf made as its insert would have then
		day := server.PartitionGranularityDaily.RangeStart(now.AddDate(0, 0, -32))
		if _, err := server.GetPartitionFamily("smtp_send").EnsureRangeFor(ctx, day); err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			send := smtpTestSend(networkId, day.Add(time.Hour), 1)
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO smtp_send (send_day, send_id, network_id, client_id, sending_address, recipient_domain, mx_host,
					start_time, end_time, byte_count, x_ur_ref_key_id, recipient_count, message_byte_count, tls_version, tls_level, result)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 1, 1, 1, 1, 772, 1, 'sent')
				`,
				day,
				send.SendId,
				server.Id(networkId),
				server.Id(send.ClientId),
				send.SendingAddress,
				send.RecipientDomain,
				send.MxHost,
				send.StartTime,
				send.EndTime,
			))
		})
		halt := &smtpedgeservice.SmtpHalt{HaltId: sidecar.NewId(), NetworkId: networkId, Trigger: smtpedgeservice.SmtpHaltTriggerFraud, StartTime: now.AddDate(0, -14, 0)}
		if err := model.SetSmtpHalt(ctx, halt); err != nil {
			t.Fatal(err)
		}

		result := runSmtpRetention(ctx, now, settings)
		if result.More || len(result.Kept) != 0 {
			t.Fatalf("run %+v", result)
		}
		if !slices.Contains(result.Dropped, "smtp_send_p"+day.Format("20060102")) {
			t.Fatalf("dropped %v", result.Dropped)
		}
		if result.RemovedCounts[model.RetentionTableSmtpHalt] != 1 {
			t.Fatalf("removed %v", result.RemovedCounts)
		}
		halts, err := model.FindSmtpHalts(ctx, server.Id(networkId), time.Time{})
		if err != nil || len(halts) != 0 {
			t.Fatalf("halts %v, %v", halts, err)
		}
	})
}
