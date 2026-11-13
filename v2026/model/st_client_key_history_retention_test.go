package model

// Key history retention for clients that are gone. The guard reads the
// database clock, so these tests place each client's gone time relative to
// it, and pass the database's own now to the retention.
//
// DB-backed: under the owner rule these run only after the branch, with its
// migrations, is merged to main.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/v2026"
)

var goneKeyHistoryTestDomain = bytes.Repeat([]byte{0x47}, 32)

// One synthetic generation and its current head. The rows satisfy the
// table's shape checks; neither the guard nor the retention decodes them.
func insertGoneKeyHistoryTestRows(t testing.TB, ctx context.Context, clientId server.Id, networkId server.Id) {
	t.Helper()
	registrationHash := sha256.Sum256(clientId.Bytes())
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO st_client_key_history (client_id, generation, domain_hash, registration_hash, evidence_hash, evidence)
			VALUES ($1, 1, $2, $3, $4, $5)
			`,
			clientId,
			goneKeyHistoryTestDomain,
			registrationHash[:],
			"sha256:"+hex.EncodeToString(registrationHash[:]),
			[]byte("{}"),
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO st_client_key_head (client_id, domain_hash, network_id, generation) VALUES ($1, $2, $3, 1)`,
			clientId,
			goneKeyHistoryTestDomain,
			networkId,
		))
	})
}

// Records the client as gone this many days before the database clock.
func setGoneKeyHistoryTestDays(t testing.TB, ctx context.Context, clientId server.Id, days int) {
	t.Helper()
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO st_client_key_gone (client_id, gone_time)
			VALUES ($1, (now() AT TIME ZONE 'UTC') - make_interval(days => $2))
			ON CONFLICT (client_id) DO UPDATE SET gone_time = EXCLUDED.gone_time
			`,
			clientId,
			days,
		))
	})
}

type goneKeyHistoryTestRows struct {
	history int
	heads   int
	gone    bool
}

func readGoneKeyHistoryTestRows(t testing.TB, ctx context.Context, clientId server.Id) (rows goneKeyHistoryTestRows) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
			SELECT
				(SELECT count(*) FROM st_client_key_history WHERE client_id = $1),
				(SELECT count(*) FROM st_client_key_head WHERE client_id = $1),
				EXISTS (SELECT 1 FROM st_client_key_gone WHERE client_id = $1)
			`,
			clientId,
		).Scan(&rows.history, &rows.heads, &rows.gone))
	})
	return
}

func goneKeyHistoryTestDbNow(t testing.TB, ctx context.Context) time.Time {
	t.Helper()
	var now time.Time
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT clock_timestamp() AT TIME ZONE 'UTC'`).Scan(&now))
	})
	return now
}

// A cursor whose discovery pass just completed, so a run only deletes.
func goneKeyHistoryTestDeleteOnly(now time.Time) StClientKeyHistoryRetentionCursor {
	return StClientKeyHistoryRetentionCursor{DiscoverCompleteTime: &now}
}

const (
	goneKeyHistoryTestDeleteHeadsSql   = `DELETE FROM st_client_key_head WHERE client_id = $1`
	goneKeyHistoryTestDeleteHistorySql = `DELETE FROM st_client_key_history WHERE client_id = $1`
	goneKeyHistoryTestUpdateHistorySql = `UPDATE st_client_key_history SET evidence = '{"changed":true}' WHERE client_id = $1`
)

// Runs the statements, each with the client as $1, in one maintenance
// transaction, opted in or not, and returns what refused them.
func goneKeyHistoryTestStatements(ctx context.Context, optIn bool, clientId server.Id, statements ...string) (returnErr error) {
	server.HandleError(func() {
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			if optIn {
				server.RaisePgResult(tx.Exec(ctx, `SET LOCAL urnetwork.client_key_history_retention = 'on'`))
			}
			for _, statement := range statements {
				server.RaisePgResult(tx.Exec(ctx, statement, clientId))
			}
		}, server.TxReadCommitted, server.OptNoRetry())
	}, func(err error) { returnErr = err })
	return
}

// A truncate of the history, opted in or not.
func goneKeyHistoryTestTruncate(ctx context.Context, optIn bool) (returnErr error) {
	server.HandleError(func() {
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			if optIn {
				server.RaisePgResult(tx.Exec(ctx, `SET LOCAL urnetwork.client_key_history_retention = 'on'`))
			}
			server.RaisePgResult(tx.Exec(ctx, `TRUNCATE st_client_key_history CASCADE`))
		}, server.TxReadCommitted, server.OptNoRetry())
	}, func(err error) { returnErr = err })
	return
}

func requireKeyHistoryImmutable(t testing.TB, err error, what string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "signed client-key history is immutable") {
		t.Fatalf("%s was not refused by the history guard: %v", what, err)
	}
}

// A client gone 90 days keeps its history; one gone 92 days loses its heads,
// its history and its record, heads first.
func TestGoneStClientKeyHistoryKeptAt90DaysDeletedAt92Days(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		keptId, removedId := server.NewId(), server.NewId()
		for _, clientId := range []server.Id{keptId, removedId} {
			insertGoneKeyHistoryTestRows(t, ctx, clientId, server.NewId())
		}
		setGoneKeyHistoryTestDays(t, ctx, keptId, 90)
		setGoneKeyHistoryTestDays(t, ctx, removedId, 92)

		now := goneKeyHistoryTestDbNow(t, ctx)
		report, _ := RemoveGoneStClientKeyHistory(ctx, now, goneKeyHistoryTestDeleteOnly(now), DefaultStClientKeyHistoryRetentionSettings())
		if report.RemovedClientCount != 1 || report.RemovedHistoryCount != 1 || report.More {
			t.Fatalf("retention report = %+v, want one client and one registration", report)
		}
		if rows := readGoneKeyHistoryTestRows(t, ctx, keptId); rows != (goneKeyHistoryTestRows{history: 1, heads: 1, gone: true}) {
			t.Fatalf("a client gone 90 days lost key history: %+v", rows)
		}
		if rows := readGoneKeyHistoryTestRows(t, ctx, removedId); rows != (goneKeyHistoryTestRows{}) {
			t.Fatalf("a client gone 92 days kept key history: %+v", rows)
		}
	})
}

// The guard admits a delete only with the opt-in, only after the heads, and
// only for a client gone longer than the floor without a client row. An
// update and a truncate are refused, opted in or not.
func TestStClientKeyHistoryGuardAdmitsOnlyRetentionDeletes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		goneId, recentId, activeId := server.NewId(), server.NewId(), server.NewId()
		networkId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), activeId, "test", "test")
		for _, clientId := range []server.Id{goneId, recentId, activeId} {
			insertGoneKeyHistoryTestRows(t, ctx, clientId, networkId)
		}
		floorDays := int(server.ClientKeyHistoryRetentionFloor / (24 * time.Hour))
		setGoneKeyHistoryTestDays(t, ctx, goneId, floorDays+1)
		setGoneKeyHistoryTestDays(t, ctx, recentId, floorDays-1)
		// a record a client that exists must never have
		setGoneKeyHistoryTestDays(t, ctx, activeId, 92)

		requireKeyHistoryImmutable(t, goneKeyHistoryTestStatements(ctx, false, goneId, goneKeyHistoryTestDeleteHeadsSql, goneKeyHistoryTestDeleteHistorySql), "a delete without the opt-in")
		requireKeyHistoryImmutable(t, goneKeyHistoryTestStatements(ctx, true, goneId, goneKeyHistoryTestDeleteHistorySql), "a delete before the heads")
		requireKeyHistoryImmutable(t, goneKeyHistoryTestStatements(ctx, true, recentId, goneKeyHistoryTestDeleteHeadsSql, goneKeyHistoryTestDeleteHistorySql), "a delete of a client gone a day less than the floor")
		requireKeyHistoryImmutable(t, goneKeyHistoryTestStatements(ctx, true, activeId, goneKeyHistoryTestDeleteHeadsSql, goneKeyHistoryTestDeleteHistorySql), "a delete of an existing client")
		for _, optIn := range []bool{false, true} {
			requireKeyHistoryImmutable(t, goneKeyHistoryTestStatements(ctx, optIn, goneId, goneKeyHistoryTestUpdateHistorySql), "an update")
			requireKeyHistoryImmutable(t, goneKeyHistoryTestTruncate(ctx, optIn), "a truncate")
		}
		for _, clientId := range []server.Id{goneId, recentId, activeId} {
			if rows := readGoneKeyHistoryTestRows(t, ctx, clientId); rows.history != 1 || rows.heads != 1 {
				t.Fatalf("a refused statement changed rows: %+v", rows)
			}
		}

		if err := goneKeyHistoryTestStatements(ctx, true, goneId, goneKeyHistoryTestDeleteHeadsSql, goneKeyHistoryTestDeleteHistorySql); err != nil {
			t.Fatalf("the guard refused a retention delete: %v", err)
		}
		if rows := readGoneKeyHistoryTestRows(t, ctx, goneId); rows.history != 0 || rows.heads != 0 {
			t.Fatalf("the retention delete left rows: %+v", rows)
		}
	})
}

// A client with a row keeps its history, even under a record that claims it
// went long ago.
func TestGoneStClientKeyHistoryNeverDeletesAnExistingClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId, activeId := server.NewId(), server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), activeId, "test", "test")
		insertGoneKeyHistoryTestRows(t, ctx, activeId, networkId)
		setGoneKeyHistoryTestDays(t, ctx, activeId, 365)

		now := goneKeyHistoryTestDbNow(t, ctx)
		report, _ := RemoveGoneStClientKeyHistory(ctx, now, goneKeyHistoryTestDeleteOnly(now), DefaultStClientKeyHistoryRetentionSettings())
		if report.RemovedClientCount != 0 {
			t.Fatalf("retention removed an existing client: %+v", report)
		}
		if rows := readGoneKeyHistoryTestRows(t, ctx, activeId); rows.history != 1 || rows.heads != 1 {
			t.Fatalf("an existing client lost key history: %+v", rows)
		}
	})
}

// The client delete trigger records when each client with key history went:
// a child at its deactivation, or at its delete when it was active; a
// top-level client at its own reap. A client without history records nothing.
func TestStClientKeyGoneRecordsWhenAClientWent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		topLevelId, topLevelDeviceId := server.NewId(), server.NewId()
		Testing_CreateDevice(ctx, networkId, topLevelDeviceId, topLevelId, "test", "test")
		retiredChildId := insertRetiredHostedTestChild(t, networkId, topLevelId, topLevelDeviceId)
		activeChildId := insertRetiredHostedTestChild(t, networkId, topLevelId, topLevelDeviceId)
		withoutHistoryId := insertRetiredHostedTestChild(t, networkId, topLevelId, topLevelDeviceId)
		for _, clientId := range []server.Id{topLevelId, retiredChildId, activeChildId} {
			insertGoneKeyHistoryTestRows(t, ctx, clientId, networkId)
		}

		retireRetiredHostedTestClient(t, networkId, retiredChildId)
		retireRetiredHostedTestClient(t, networkId, topLevelId)
		// the reaps delete these rows a day and 30 days after deactivation;
		// moving the deactivations back tells the two rules apart
		var childDeactivateTime time.Time
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_client SET deactivate_time = deactivate_time - interval '10 days' WHERE client_id = ANY($1)`,
				[]server.Id{retiredChildId, topLevelId},
			))
			server.Raise(tx.QueryRow(ctx, `SELECT deactivate_time FROM network_client WHERE client_id = $1`, retiredChildId).Scan(&childDeactivateTime))
		})
		before := goneKeyHistoryTestDbNow(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`DELETE FROM network_client WHERE client_id = ANY($1)`,
				[]server.Id{topLevelId, retiredChildId, activeChildId, withoutHistoryId},
			))
		})
		after := goneKeyHistoryTestDbNow(t, ctx)

		goneTimes := map[server.Id]time.Time{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT client_id, gone_time FROM st_client_key_gone`)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var clientId server.Id
					var goneTime time.Time
					server.Raise(result.Scan(&clientId, &goneTime))
					goneTimes[clientId] = goneTime
				}
			})
		})
		if len(goneTimes) != 3 {
			t.Fatalf("recorded %d gone clients, want the three with key history", len(goneTimes))
		}
		if !goneTimes[retiredChildId].Equal(childDeactivateTime) {
			t.Fatalf("a retired child went at %v, want its deactivation %v", goneTimes[retiredChildId], childDeactivateTime)
		}
		for _, clientId := range []server.Id{topLevelId, activeChildId} {
			if goneTime := goneTimes[clientId]; goneTime.Before(before.Add(-time.Second)) || after.Before(goneTime) {
				t.Fatalf("client %s went at %v, want its delete in [%v, %v]", clientId, goneTime, before, after)
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			var unretired int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM st_client_key_head WHERE NOT retired`).Scan(&unretired))
			if unretired != 0 {
				t.Fatalf("%d heads of deleted clients stayed unretired", unretired)
			}
		})
	})
}

// Heads of clients deleted before the trigger recorded them get a record at
// discovery, once per pass; a pass completes, and the next waits its interval.
func TestGoneStClientKeyHistoryDiscoveryRecordsEarlierDeletes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId, activeId := server.NewId(), server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), activeId, "test", "test")
		firstGoneId, secondGoneId := server.NewId(), server.NewId()
		for _, clientId := range []server.Id{firstGoneId, activeId, secondGoneId} {
			insertGoneKeyHistoryTestRows(t, ctx, clientId, networkId)
		}
		settings := DefaultStClientKeyHistoryRetentionSettings()
		settings.DiscoverBatchCount = 1

		now := goneKeyHistoryTestDbNow(t, ctx)
		report, cursor := RemoveGoneStClientKeyHistory(ctx, now, StClientKeyHistoryRetentionCursor{}, settings)
		if report.DiscoveredClientCount != 2 || report.More {
			t.Fatalf("discovery report = %+v, want two gone clients and a complete pass", report)
		}
		if cursor.DiscoverAfterClientId != nil || cursor.DiscoverCompleteTime == nil || !cursor.DiscoverCompleteTime.Equal(now) {
			t.Fatalf("a complete pass left cursor %+v", cursor)
		}
		for clientId, wantGone := range map[server.Id]bool{firstGoneId: true, secondGoneId: true, activeId: false} {
			if rows := readGoneKeyHistoryTestRows(t, ctx, clientId); rows.gone != wantGone || rows.history != 1 {
				t.Fatalf("client %s after discovery: %+v, want gone=%t", clientId, rows, wantGone)
			}
		}

		laterGoneId := server.NewId()
		insertGoneKeyHistoryTestRows(t, ctx, laterGoneId, networkId)
		if report, _ := RemoveGoneStClientKeyHistory(ctx, now.Add(time.Hour), cursor, settings); report.DiscoveredClientCount != 0 {
			t.Fatal("a discovery pass ran inside its interval")
		}
		if report, _ := RemoveGoneStClientKeyHistory(ctx, now.Add(settings.DiscoverInterval+time.Hour), cursor, settings); report.DiscoveredClientCount != 1 {
			t.Fatalf("the next discovery pass missed a gone client: %+v", report)
		}
		if rows := readGoneKeyHistoryTestRows(t, ctx, laterGoneId); !rows.gone {
			t.Fatal("the next pass recorded nothing")
		}
	})
}

// Original-close verification reads the reporting client's key history, and
// still finds it 90 days after the client went. Past the horizon a report
// is admitted without a registration, as for any client without history.
func TestOriginalCloseVerificationReadsHistoryWithinRetention(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		input := newStClientKeyHistoryTestInput(t)
		input.ClientID = f.sourceId
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x5a}, 32))
		input.PublicKey = key[32:]
		registration, err := StoreStClientKeyRegistration(f.ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		domain, _ := input.Domain.Digest()
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `DELETE FROM network_client WHERE client_id = $1`, f.sourceId))
		})
		if rows := readGoneKeyHistoryTestRows(t, f.ctx, f.sourceId); !rows.gone {
			t.Fatal("the delete recorded no gone time")
		}

		setGoneKeyHistoryTestDays(t, f.ctx, f.sourceId, 90)
		now := goneKeyHistoryTestDbNow(t, f.ctx)
		RemoveGoneStClientKeyHistory(f.ctx, now, goneKeyHistoryTestDeleteOnly(now), DefaultStClientKeyHistoryRetentionSettings())
		report := signCloseReportOriginal(t, f.report(), domain, key)
		if applied, err := CloseContractWithReport(f.ctx, report); err != nil || !applied {
			t.Fatal("a report within the retention was not admitted", applied, err)
		}
		if _, retained := retainedCloseOriginal(t, report); !bytes.Equal(retained, registration.RegistrationBytes) {
			t.Fatal("verification did not find the registration 90 days after the client went")
		}

		setGoneKeyHistoryTestDays(t, f.ctx, f.sourceId, 92)
		now = goneKeyHistoryTestDbNow(t, f.ctx)
		RemoveGoneStClientKeyHistory(f.ctx, now, goneKeyHistoryTestDeleteOnly(now), DefaultStClientKeyHistoryRetentionSettings())
		late := signCloseReportOriginal(t, f.report(), domain, key)
		if applied, err := CloseContractWithReport(f.ctx, late); err != nil || !applied {
			t.Fatal("a report past the retention was refused", applied, err)
		}
		var issue string
		server.Db(f.ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(f.ctx, `SELECT original_key_issue FROM contract_close_report_evidence WHERE client_id = $1 AND report_id = $2`, late.ClientId, late.ReportId).Scan(&issue))
		})
		if issue != originalCloseKeyMissing {
			t.Fatalf("a report past the retention recorded issue %q, want %q", issue, originalCloseKeyMissing)
		}
		if _, retained := retainedCloseOriginal(t, report); !bytes.Equal(retained, registration.RegistrationBytes) {
			t.Fatal("retention changed a registration a report already retained")
		}
	})
}

// The legacy Redis key of a gone client goes with its heads, so an orphaned
// key cannot answer for it again.
func TestGoneStClientKeyHistoryRemovesLegacyKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		goneId := server.NewId()
		insertGoneKeyHistoryTestRows(t, ctx, goneId, server.NewId())
		setGoneKeyHistoryTestDays(t, ctx, goneId, 92)
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, clientPublicKeyRedisKey(goneId), bytes.Repeat([]byte{0x6b}, 32), 0).Err())
		})

		now := goneKeyHistoryTestDbNow(t, ctx)
		RemoveGoneStClientKeyHistory(ctx, now, goneKeyHistoryTestDeleteOnly(now), DefaultStClientKeyHistoryRetentionSettings())
		publicKey, err := GetClientPublicKey(ctx, goneId)
		if err != nil || publicKey != nil {
			t.Fatalf("a gone client's key answered after retention: %x, %v", publicKey, err)
		}
	})
}

// The retention transaction refuses nothing it selected: a batch of many gone
// clients goes in pages, and a run that ends its budget reports more.
func TestGoneStClientKeyHistoryDeletesInPages(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		goneIds := []server.Id{}
		for range 5 {
			goneId := server.NewId()
			insertGoneKeyHistoryTestRows(t, ctx, goneId, server.NewId())
			setGoneKeyHistoryTestDays(t, ctx, goneId, 100)
			goneIds = append(goneIds, goneId)
		}
		settings := DefaultStClientKeyHistoryRetentionSettings()
		settings.DeleteBatchCount = 2
		settings.RunBudget = 0

		now := goneKeyHistoryTestDbNow(t, ctx)
		report, _ := RemoveGoneStClientKeyHistory(ctx, now, goneKeyHistoryTestDeleteOnly(now), settings)
		if report.RemovedClientCount != 2 || !report.More {
			t.Fatalf("an out-of-budget run = %+v, want one page and more", report)
		}
		settings.RunBudget = time.Minute
		report, _ = RemoveGoneStClientKeyHistory(ctx, now, goneKeyHistoryTestDeleteOnly(now), settings)
		if report.RemovedClientCount != 3 || report.More {
			t.Fatalf("a budgeted run = %+v, want the remaining three", report)
		}
		for _, goneId := range goneIds {
			if rows := readGoneKeyHistoryTestRows(t, ctx, goneId); rows != (goneKeyHistoryTestRows{}) {
				t.Fatalf("client %s kept rows %+v", goneId, rows)
			}
		}
	})
}

// A hard delete through the retired hosted reap records the window's
// deactivation as its gone time.
func TestRetiredHostedReapRecordsWindowGoneAtDeactivation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		proxy := newRetiredHostedTestProxy(t)
		windowId := insertRetiredHostedTestChild(t, proxy.networkId, proxy.clientId, proxy.deviceId)
		insertGoneKeyHistoryTestRows(t, ctx, windowId, proxy.networkId)
		deactivateTime := retireRetiredHostedTestClient(t, proxy.networkId, windowId)

		removedCount, _, _ := RemoveRetiredHostedNetworkClients(ctx, deactivateTime.Add(25*time.Hour), RetiredHostedNetworkClientCursor{}, DefaultRetiredHostedNetworkClientReapSettings())
		if removedCount != 1 {
			t.Fatal("the window was not reaped")
		}
		var goneTime time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT gone_time FROM st_client_key_gone WHERE client_id = $1`, windowId).Scan(&goneTime))
		})
		if !goneTime.Equal(deactivateTime) {
			t.Fatalf("the reaped window went at %v, want its deactivation %v", goneTime, deactivateTime)
		}
		requireKeyHistoryImmutable(t, goneKeyHistoryTestStatements(ctx, true, windowId, goneKeyHistoryTestDeleteHeadsSql, goneKeyHistoryTestDeleteHistorySql), "a delete of a window that went today")
	})
}

// The run reports the horizon it applied and counts what it deleted by table.
func TestGoneStClientKeyHistoryReportsItsHorizonAndCountsDeletes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		removedId := server.NewId()
		insertGoneKeyHistoryTestRows(t, ctx, removedId, server.NewId())
		setGoneKeyHistoryTestDays(t, ctx, removedId, 50)

		setGoneKeyHistoryTestContractHorizon(t, ctx, nil)
		now := goneKeyHistoryTestDbNow(t, ctx)
		report, _ := RemoveGoneStClientKeyHistory(ctx, now, goneKeyHistoryTestDeleteOnly(now), DefaultStClientKeyHistoryRetentionSettings())
		if report.RemovedClientCount != 0 || report.Horizon != server.ContractRetention {
			t.Fatalf("without a control the retention = %+v, want none removed at %v", report, server.ContractRetention)
		}

		setGoneKeyHistoryTestContractHorizon(t, ctx, map[string]string{ContractHorizonControlFieldMode: "on", ContractHorizonControlFieldHorizonDays: "45"})
		before := map[string]float64{}
		for _, table := range retentionTables {
			before[table] = testutil.ToFloat64(retentionDeletedRowsCounter.WithLabelValues(table))
		}
		now = goneKeyHistoryTestDbNow(t, ctx)
		report, _ = RemoveGoneStClientKeyHistory(ctx, now, goneKeyHistoryTestDeleteOnly(now), DefaultStClientKeyHistoryRetentionSettings())
		if report.RemovedClientCount != 1 || report.Horizon != 45*24*time.Hour {
			t.Fatalf("at a 45-day horizon the retention = %+v, want one client at 45 days", report)
		}
		for _, table := range []string{RetentionTableStClientKeyHistory, RetentionTableStClientKeyHead, RetentionTableStClientKeyGone} {
			if moved := testutil.ToFloat64(retentionDeletedRowsCounter.WithLabelValues(table)) - before[table]; moved != 1 {
				t.Fatalf("the %s deleted row counter moved by %v, want 1", table, moved)
			}
		}
	})
}

// A control below the reaper's floor counts as the floor, as for the reaper:
// a client gone a day less than it keeps its history, one gone a day more
// loses it.
func TestGoneStClientKeyHistoryHorizonStopsAtTheReaperFloor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		floorDays := int(ContractHorizonFloor / (24 * time.Hour))
		keptId, removedId := server.NewId(), server.NewId()
		for _, clientId := range []server.Id{keptId, removedId} {
			insertGoneKeyHistoryTestRows(t, ctx, clientId, server.NewId())
		}
		setGoneKeyHistoryTestDays(t, ctx, keptId, floorDays-1)
		setGoneKeyHistoryTestDays(t, ctx, removedId, floorDays+1)
		setGoneKeyHistoryTestContractHorizon(t, ctx, map[string]string{ContractHorizonControlFieldMode: "on", ContractHorizonControlFieldHorizonDays: "1"})

		now := goneKeyHistoryTestDbNow(t, ctx)
		report, _ := RemoveGoneStClientKeyHistory(ctx, now, goneKeyHistoryTestDeleteOnly(now), DefaultStClientKeyHistoryRetentionSettings())
		if report.RemovedClientCount != 1 || report.Horizon != ContractHorizonFloor {
			t.Fatalf("a one-day control = %+v, want one client at the reaper's floor %v", report, ContractHorizonFloor)
		}
		if rows := readGoneKeyHistoryTestRows(t, ctx, keptId); rows.history != 1 || rows.heads != 1 {
			t.Fatalf("a client gone a day less than the reaper's floor lost key history: %+v", rows)
		}
		if rows := readGoneKeyHistoryTestRows(t, ctx, removedId); rows != (goneKeyHistoryTestRows{}) {
			t.Fatalf("a client gone a day more than the reaper's floor kept key history: %+v", rows)
		}
	})
}

// Pure: whatever the settings, a run's horizon is never below the guard's
// floor, and a horizon that is not followed is the settings' own.
func TestStClientKeyHistoryRetentionHorizonKeepsTheGuardFloor(t *testing.T) {
	settings := DefaultStClientKeyHistoryRetentionSettings()
	settings.FollowContractHorizon = false
	settings.Retention = 24 * time.Hour
	if horizon := settings.horizon(context.Background()); horizon != server.ClientKeyHistoryRetentionFloor {
		t.Fatalf("a one-day retention ran at %v, want the floor %v", horizon, server.ClientKeyHistoryRetentionFloor)
	}
	settings.Retention = 60 * 24 * time.Hour
	if horizon := settings.horizon(context.Background()); horizon != settings.Retention {
		t.Fatalf("an unfollowed retention ran at %v, want %v", horizon, settings.Retention)
	}
	if server.ClientKeyHistoryRetentionFloor >= ContractHorizonFloor {
		t.Fatalf("the guard's floor %v is not below the reaper's %v, so the reaper's shortest horizon would be refused", server.ClientKeyHistoryRetentionFloor, ContractHorizonFloor)
	}
}

// Sets the retention's brake for the test's Redis database; nil removes it.
func setGoneKeyHistoryTestBrake(t testing.TB, ctx context.Context, fields map[string]string) {
	t.Helper()
	controlCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if fields == nil {
		if err := RemoveStClientKeyHistoryRetentionControl(controlCtx); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := SetStClientKeyHistoryRetentionControl(controlCtx, fields); err != nil {
		t.Fatal(err)
	}
}

// Makes a control key unreadable as a hash.
func setGoneKeyHistoryTestUnreadable(t testing.TB, ctx context.Context, key string) {
	t.Helper()
	server.Redis(ctx, func(r server.RedisClient) {
		err := r.Set(ctx, key, "not a hash", 0).Err()
		server.Raise(err)
	})
}

// The reaper's horizon applies only while its mode is on: a dry run, the
// mode off, and an unreadable control each keep 91 days, so a client gone 50
// days keeps its history; mode on at 45 days deletes it.
func TestGoneStClientKeyHistoryFollowsTheReaperOnlyWhileItsModeIsOn(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		goneId := server.NewId()
		insertGoneKeyHistoryTestRows(t, ctx, goneId, server.NewId())
		setGoneKeyHistoryTestDays(t, ctx, goneId, 50)
		run := func() *StClientKeyHistoryRetentionReport {
			now := goneKeyHistoryTestDbNow(t, ctx)
			report, _ := RemoveGoneStClientKeyHistory(ctx, now, goneKeyHistoryTestDeleteOnly(now), DefaultStClientKeyHistoryRetentionSettings())
			return report
		}
		for _, mode := range []string{"dry_run", "off"} {
			setGoneKeyHistoryTestContractHorizon(t, ctx, nil)
			setGoneKeyHistoryTestContractHorizon(t, ctx, map[string]string{ContractHorizonControlFieldMode: mode, ContractHorizonControlFieldHorizonDays: "45"})
			if report := run(); report.RemovedClientCount != 0 || report.Horizon != server.ContractRetention {
				t.Fatalf("with the reaper in %s the retention = %+v, want none removed at 91 days", mode, report)
			}
		}
		setGoneKeyHistoryTestContractHorizon(t, ctx, nil)
		setGoneKeyHistoryTestUnreadable(t, ctx, contractHorizonControlKey)
		if report := run(); report.RemovedClientCount != 0 || report.Horizon != server.ContractRetention {
			t.Fatalf("with an unreadable reaper control the retention = %+v, want none removed at 91 days", report)
		}
		if rows := readGoneKeyHistoryTestRows(t, ctx, goneId); rows.history != 1 || rows.heads != 1 {
			t.Fatalf("a client gone 50 days lost key history without an on reaper: %+v", rows)
		}

		setGoneKeyHistoryTestContractHorizon(t, ctx, nil)
		setGoneKeyHistoryTestContractHorizon(t, ctx, map[string]string{ContractHorizonControlFieldMode: "on", ContractHorizonControlFieldHorizonDays: "45"})
		if report := run(); report.RemovedClientCount != 1 || report.Horizon != 45*24*time.Hour {
			t.Fatalf("with the reaper on at 45 days the retention = %+v, want one removed", report)
		}
	})
}

// The brake stops the deletes when off, when it sets no rate, and when it
// cannot be read, and paces them at its rate; without it the default rate
// applies.
func TestGoneStClientKeyHistoryBrake(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		goneIds := []server.Id{}
		for range 3 {
			goneId := server.NewId()
			insertGoneKeyHistoryTestRows(t, ctx, goneId, server.NewId())
			setGoneKeyHistoryTestDays(t, ctx, goneId, 100)
			goneIds = append(goneIds, goneId)
		}
		settings := DefaultStClientKeyHistoryRetentionSettings()
		settings.DeleteBatchCount = 2
		run := func() *StClientKeyHistoryRetentionReport {
			now := goneKeyHistoryTestDbNow(t, ctx)
			report, _ := RemoveGoneStClientKeyHistory(ctx, now, goneKeyHistoryTestDeleteOnly(now), settings)
			return report
		}
		for _, brake := range []map[string]string{
			{StClientKeyHistoryRetentionControlFieldMode: "off"},
			{StClientKeyHistoryRetentionControlFieldMode: "on", StClientKeyHistoryRetentionControlFieldMaxClientsPerSecond: "0"},
			{StClientKeyHistoryRetentionControlFieldMode: "on", StClientKeyHistoryRetentionControlFieldMaxClientsPerSecond: "fast"},
		} {
			setGoneKeyHistoryTestBrake(t, ctx, nil)
			setGoneKeyHistoryTestBrake(t, ctx, brake)
			if report := run(); report.RemovedClientCount != 0 || report.DeleteStop != StClientKeyHistoryRetentionStopBrakeOff {
				t.Fatalf("brake %v: retention = %+v, want nothing removed and brake_off", brake, report)
			}
		}
		setGoneKeyHistoryTestBrake(t, ctx, nil)
		setGoneKeyHistoryTestUnreadable(t, ctx, stClientKeyHistoryRetentionControlKey)
		if report := run(); report.RemovedClientCount != 0 || report.DeleteStop != StClientKeyHistoryRetentionStopBrakeUnread {
			t.Fatalf("an unreadable brake: retention = %+v, want nothing removed and brake_unread", report)
		}

		// one client a second: the second batch would start after the delete
		// half of a 2-second budget, so a run deletes one batch and stops
		setGoneKeyHistoryTestBrake(t, ctx, nil)
		setGoneKeyHistoryTestBrake(t, ctx, map[string]string{StClientKeyHistoryRetentionControlFieldMode: "on", StClientKeyHistoryRetentionControlFieldMaxClientsPerSecond: "1"})
		settings.RunBudget = 2 * time.Second
		if report := run(); report.RemovedClientCount != 2 || !report.More || report.DeleteStop != StClientKeyHistoryRetentionStopBudget {
			t.Fatalf("at one client a second the retention = %+v, want one batch of 2 and more", report)
		}
		setGoneKeyHistoryTestBrake(t, ctx, nil)
		settings.RunBudget = time.Minute
		if report := run(); report.RemovedClientCount != 1 || report.DeleteStop != StClientKeyHistoryRetentionStopDue {
			t.Fatalf("without a brake the retention = %+v, want the last client", report)
		}
		for _, goneId := range goneIds {
			if rows := readGoneKeyHistoryTestRows(t, ctx, goneId); rows != (goneKeyHistoryTestRows{}) {
				t.Fatalf("client kept rows %+v", rows)
			}
		}
	})
}

// Pure: the brake narrows to the code's bounds.
func TestNarrowStClientKeyHistoryRetentionControl(t *testing.T) {
	settings := DefaultStClientKeyHistoryRetentionSettings()
	for _, test := range []struct {
		fields map[string]string
		on     bool
		rate   float64
	}{
		{nil, true, settings.DefaultDeleteClientsPerSecond},
		{map[string]string{"mode": "on"}, true, settings.DefaultDeleteClientsPerSecond},
		{map[string]string{"mode": "off"}, false, settings.DefaultDeleteClientsPerSecond},
		{map[string]string{"mode": "paused"}, false, settings.DefaultDeleteClientsPerSecond},
		{map[string]string{"maxClientsPerSecond": "500"}, true, 500},
		{map[string]string{"maxClientsPerSecond": "1e9"}, true, settings.MaxDeleteClientsPerSecond},
		{map[string]string{"maxClientsPerSecond": "0"}, false, 0},
		{map[string]string{"maxClientsPerSecond": "NaN"}, false, 0},
	} {
		control := NarrowStClientKeyHistoryRetentionControl(test.fields, settings)
		if control.On != test.on || control.MaxClientsPerSecond != test.rate || control.Present != (0 < len(test.fields)) {
			t.Fatalf("fields %v narrowed to %+v, want on=%t rate=%v", test.fields, control, test.on, test.rate)
		}
	}
	if !(settings.DefaultDeleteClientsPerSecond <= 200) {
		t.Fatalf("the default rate %v is not a cautious start", settings.DefaultDeleteClientsPerSecond)
	}
}

// A pass resumes from its cursor, so a record older than the cursor that came
// due mid-pass waits for the next pass; a short batch ends the pass and the
// next starts at the oldest record.
func TestGoneStClientKeyHistoryResumesFromItsCursor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		goneIds := []server.Id{}
		for _, days := range []int{100, 99, 98, 97, 96} {
			goneId := server.NewId()
			insertGoneKeyHistoryTestRows(t, ctx, goneId, server.NewId())
			setGoneKeyHistoryTestDays(t, ctx, goneId, days)
			goneIds = append(goneIds, goneId)
		}
		settings := DefaultStClientKeyHistoryRetentionSettings()
		settings.DeleteBatchCount = 2
		// one batch a run
		settings.RunBudget = 0
		now := goneKeyHistoryTestDbNow(t, ctx)
		cursor := goneKeyHistoryTestDeleteOnly(now)
		removed := func(want int) {
			t.Helper()
			var report *StClientKeyHistoryRetentionReport
			report, cursor = RemoveGoneStClientKeyHistory(ctx, now, cursor, settings)
			if report.RemovedClientCount != int64(want) {
				t.Fatalf("a run removed %d, want %d: %+v", report.RemovedClientCount, want, report)
			}
		}
		gone := func(goneId server.Id) bool {
			return readGoneKeyHistoryTestRows(t, ctx, goneId) == goneKeyHistoryTestRows{}
		}

		removed(2)
		if cursor.DeleteAfterGoneTime == nil || cursor.DeleteAfterClientId == nil || *cursor.DeleteAfterClientId != goneIds[1] {
			t.Fatalf("the first run left cursor %+v, want its second client", cursor)
		}
		olderId := server.NewId()
		insertGoneKeyHistoryTestRows(t, ctx, olderId, server.NewId())
		setGoneKeyHistoryTestDays(t, ctx, olderId, 200)
		removed(2)
		if !gone(goneIds[2]) || !gone(goneIds[3]) || gone(olderId) {
			t.Fatal("the resumed run did not continue after its cursor")
		}
		settings.RunBudget = time.Minute
		removed(1)
		if cursor.DeleteAfterGoneTime != nil || cursor.DeleteAfterClientId != nil {
			t.Fatalf("a short batch left cursor %+v, want the pass ended", cursor)
		}
		removed(1)
		if !gone(olderId) {
			t.Fatal("the next pass did not start at the oldest record")
		}
	})
}

// After the floor's rollback the batch keeps 91 days on the database clock,
// whatever the horizon, without a refused delete; the restore brings the
// shorter horizon back.
func TestGoneStClientKeyHistoryAfterAFloorRollback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		recentId, oldId := server.NewId(), server.NewId()
		for _, clientId := range []server.Id{recentId, oldId} {
			insertGoneKeyHistoryTestRows(t, ctx, clientId, server.NewId())
		}
		setGoneKeyHistoryTestDays(t, ctx, recentId, 50)
		setGoneKeyHistoryTestDays(t, ctx, oldId, 92)
		setGoneKeyHistoryTestContractHorizon(t, ctx, map[string]string{ContractHorizonControlFieldMode: "on", ContractHorizonControlFieldHorizonDays: "45"})
		run := func() *StClientKeyHistoryRetentionReport {
			now := goneKeyHistoryTestDbNow(t, ctx)
			report, _ := RemoveGoneStClientKeyHistory(ctx, now, goneKeyHistoryTestDeleteOnly(now), DefaultStClientKeyHistoryRetentionSettings())
			return report
		}

		server.SetClientKeyHistoryRetentionFloor(ctx, true)
		if report := run(); report.RemovedClientCount != 1 {
			t.Fatalf("after the rollback the retention = %+v, want only the client gone 92 days", report)
		}
		if rows := readGoneKeyHistoryTestRows(t, ctx, recentId); rows.history != 1 {
			t.Fatalf("after the rollback a client gone 50 days lost key history: %+v", rows)
		}
		server.SetClientKeyHistoryRetentionFloor(ctx, false)
		if report := run(); report.RemovedClientCount != 1 {
			t.Fatalf("after the restore the retention = %+v, want the client gone 50 days", report)
		}
	})
}

// On a database without the floor migration the retention's batch cannot
// read the floor, so it raises and deletes nothing, even of a client gone
// past the old 91-day guard. A binary that has the change never runs there:
// it requires its schema head for readiness.
func TestGoneStClientKeyHistoryWithoutTheFloorMigrationFailsClosed(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.ApplyDbMigrationsUpTo(ctx, server.ClientKeyHistoryRetentionFloorMigrationVersion()-1)
		goneId := server.NewId()
		insertGoneKeyHistoryTestRows(t, ctx, goneId, server.NewId())
		setGoneKeyHistoryTestDays(t, ctx, goneId, 100)

		var runErr error
		server.HandleError(func() {
			now := goneKeyHistoryTestDbNow(t, ctx)
			RemoveGoneStClientKeyHistory(ctx, now, goneKeyHistoryTestDeleteOnly(now), DefaultStClientKeyHistoryRetentionSettings())
		}, func(err error) { runErr = err })
		if runErr == nil || !strings.Contains(runErr.Error(), "st_client_key_history_retention_floor") {
			t.Fatalf("the retention ran without the floor: %v", runErr)
		}
		if rows := readGoneKeyHistoryTestRows(t, ctx, goneId); rows.history != 1 || rows.heads != 1 {
			t.Fatalf("the retention deleted without the floor: %+v", rows)
		}
	})
}
