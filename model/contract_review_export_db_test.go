// The attribution review export (usagearchive review.go) against the migrated
// schema: the export of the contracts that payments under attribution review
// paid, its resume after a stop inside a batch, its verification, the clear of
// the payments' flag and its refusals, its reversal, and what the contract
// horizon reaper does after the clear.
package model

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/klauspost/compress/zstd"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/usagearchive"
)

// Small batches, unpaced, with no free space floor.
func reviewTestSettings() *usagearchive.ReviewSettings {
	settings := usagearchive.DefaultReviewSettings()
	settings.BatchContracts = 3
	settings.MinBatchContracts = 1
	settings.MaxContractsPerSecond = 0
	settings.MinFreeBytes = 0
	settings.Concurrency = 1
	settings.StatementTimeout = 30 * time.Second
	return settings
}

func withReviewTestConn(ctx context.Context, fn func(conn *pgx.Conn)) {
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		fn(conn.Conn())
	})
}

func reviewTestPaymentIds(paymentIds ...server.Id) []string {
	ids := []string{}
	for _, paymentId := range paymentIds {
		ids = append(ids, paymentId.String())
	}
	slices.Sort(ids)
	return ids
}

// Every account_payment row, as to_jsonb prints it, by payment id.
func reviewTestPaymentRows(t testing.TB, ctx context.Context) map[server.Id]string {
	t.Helper()
	rows := map[server.Id]string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT payment_id, to_jsonb(p)::text FROM account_payment AS p`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var paymentId server.Id
				var row string
				server.Raise(result.Scan(&paymentId, &row))
				rows[paymentId] = row
			}
		})
	})
	return rows
}

func reviewTestFlag(t testing.TB, ctx context.Context, paymentId server.Id) (flag bool) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT attribution_review_required FROM account_payment WHERE payment_id = $1`, paymentId).Scan(&flag))
	})
	return
}

// The journal events of the type, by payment.
func reviewTestEvents(t testing.TB, ctx context.Context, eventType string) map[server.Id]int {
	t.Helper()
	events := map[server.Id]int{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT payment_id, count(*) FROM audit_account_payment WHERE event_type = $1 GROUP BY payment_id`, eventType)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var paymentId server.Id
				var count int
				server.Raise(result.Scan(&paymentId, &count))
				events[paymentId] = count
			}
		})
	})
	return events
}

// A second sweep of a contract, to another network, paid by paymentId, with
// the provider payouts given.
func insertReviewTestSweep(t testing.TB, ctx context.Context, contractId server.Id, balanceId server.Id, networkId server.Id, paymentId server.Id, sweepTime time.Time, payouts *string) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow_sweep
			(contract_id,balance_id,network_id,payout_byte_count,payout_net_revenue_nano_cents,payment_id,sweep_time,provider_payouts)
			VALUES($1,$2,$3,7,9,$4,$5,$6::jsonb)`, contractId, balanceId, networkId, paymentId, sweepTime.UTC(), payouts))
	}, server.TxReadCommitted, server.OptNoRetry())
}

// One COPY of each table over every contract the payments paid, in key
// order, with the export's session: what the batches' copies, in batch order,
// must hold together.
func reviewTestExpectedCopies(t testing.TB, ctx context.Context, paymentIds []string) map[string][]byte {
	t.Helper()
	copies := map[string][]byte{}
	withReviewTestConn(ctx, func(conn *pgx.Conn) {
		tables, err := usagearchive.ReadReviewSchema(ctx, conn)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		for _, setting := range usagearchive.SessionSettingsSql() {
			if _, err := tx.Exec(ctx, setting); err != nil {
				t.Fatal(err)
			}
		}
		literals := []string{}
		for _, paymentId := range paymentIds {
			literals = append(literals, "'"+paymentId+"'::uuid")
		}
		for _, table := range tables {
			columns := []string{}
			for _, column := range table.Columns {
				columns = append(columns, `"`+column.Name+`"`)
			}
			order := []string{}
			for _, key := range table.Key {
				part := `"` + key + `"`
				for _, column := range table.Columns {
					if column.Name == key && (strings.HasPrefix(column.Type, "character") || column.Type == "text") {
						part += ` COLLATE "C"`
					}
				}
				order = append(order, part)
			}
			var buffer bytes.Buffer
			sql := `COPY (SELECT ` + strings.Join(columns, ", ") + ` FROM ` + table.Table + ` WHERE contract_id IN (
				SELECT contract_id FROM transfer_escrow_sweep WHERE payment_id IN (` + strings.Join(literals, ", ") + `)
			) ORDER BY ` + strings.Join(order, ", ") + `) TO STDOUT`
			if _, err := tx.Conn().PgConn().CopyTo(ctx, &buffer, sql); err != nil {
				t.Fatal(err)
			}
			copies[table.Table] = buffer.Bytes()
		}
	})
	return copies
}

// Every batch's copy of each table, decoded and joined in batch order.
func reviewTestStoredCopies(t testing.TB, dir string) (map[string][]byte, []*usagearchive.ReviewBatch) {
	t.Helper()
	_, batches, unfinished, err := usagearchive.ReviewStatus(dir)
	if err != nil || 0 < len(unfinished) {
		t.Fatalf("status: unfinished %v, %v", unfinished, err)
	}
	copies := map[string][]byte{}
	for _, batch := range batches {
		for _, copied := range batch.Tables {
			file, err := os.Open(filepath.Join(dir, copied.File))
			if err != nil {
				t.Fatal(err)
			}
			decoder, err := zstd.NewReader(file)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(decoder)
			decoder.Close()
			file.Close()
			if err != nil {
				t.Fatal(err)
			}
			copies[copied.Table] = append(copies[copied.Table], data...)
		}
	}
	return copies, batches
}

func exportReviewTest(t testing.TB, ctx context.Context, dir string, paymentIds []string, settings *usagearchive.ReviewSettings) (report *usagearchive.ReviewExportReport, err error) {
	t.Helper()
	withReviewTestConn(ctx, func(conn *pgx.Conn) {
		report, err = usagearchive.ExportReview(ctx, conn, dir, paymentIds, "test", settings)
	})
	return
}

func verifyReviewTest(t testing.TB, ctx context.Context, dir string, mainRows bool) *usagearchive.ReviewVerification {
	t.Helper()
	var verification *usagearchive.ReviewVerification
	var err error
	withReviewTestConn(ctx, func(conn *pgx.Conn) {
		verification, _, err = usagearchive.VerifyReview(ctx, conn, dir, "test", false, mainRows, reviewTestSettings())
	})
	if err != nil {
		t.Fatal(err)
	}
	return verification
}

func clearReviewTest(t testing.TB, ctx context.Context, dir string, commit bool, expect string, settings *usagearchive.ReviewSettings) (plan *usagearchive.ReviewClearPlan, record *usagearchive.ReviewClearRecord, err error) {
	t.Helper()
	withReviewTestConn(ctx, func(conn *pgx.Conn) {
		plan, record, err = usagearchive.ClearReview(ctx, conn, dir, "test", commit, expect, settings)
	})
	return
}

// Two completed payments under attribution review and the contracts they
// paid, minted past a 35-day horizon: one paid by both, one with an escaped
// key and a provider payout of escaped text, one whose contract row is gone
// (an orphan sweep); with contracts of a third payment under review, of an
// ordinary payment and of none between them.
type reviewTestFixture struct {
	f             netEscrowOrderingTestFixture
	now           time.Time
	reviewA       server.Id
	reviewB       server.Id
	reviewC       server.Id
	ordinary      server.Id
	tied          []*horizonTestContract
	both          *horizonTestContract
	escaped       *horizonTestContract
	orphanId      server.Id
	others        []*horizonTestContract
	otherNetworks []server.Id
}

func newReviewTestFixture(t testing.TB, ctx context.Context, orphan bool) *reviewTestFixture {
	t.Helper()
	const day = 24 * time.Hour
	fixture := &reviewTestFixture{now: server.NowUtc().Truncate(time.Microsecond)}
	usePayoutTransitionAt(t, fixture.now.Add(-70*day).Truncate(time.Second))
	fixture.f = newNetEscrowOrderingTestFixture(t, ctx)
	f := fixture.f
	completed := horizonTestPointer(fixture.now.Add(-30 * day))
	fixture.reviewA = insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{completed: true, completeTime: completed, attributionReview: true})
	fixture.reviewB = insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{completed: true, completeTime: completed, attributionReview: true})
	fixture.reviewC = insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{completed: true, completeTime: completed, attributionReview: true})
	fixture.ordinary = insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{completed: true, completeTime: completed})
	old := fixture.now.Add(-60 * day)
	at := func(i int) time.Time { return old.Add(time.Duration(i) * time.Second) }
	payments := []server.Id{fixture.reviewA, fixture.reviewB, fixture.reviewA, fixture.reviewC, fixture.reviewA, fixture.ordinary, fixture.reviewB, fixture.reviewA, fixture.reviewB, fixture.reviewC, fixture.reviewA}
	all := []*horizonTestContract{}
	for i, paymentId := range payments {
		contract := newHorizonTestContract(at(2*i), "settled")
		contract.paymentId = &paymentId
		all = append(all, contract)
		switch paymentId {
		case fixture.reviewA, fixture.reviewB:
			fixture.tied = append(fixture.tied, contract)
		default:
			fixture.others = append(fixture.others, contract)
		}
	}
	unpaid := newHorizonTestContract(at(3), "canceled")
	all = append(all, unpaid)
	fixture.others = append(fixture.others, unpaid)
	insertHorizonTestContracts(t, ctx, f, all...)

	// paid by A, and by B on a second sweep to another network
	fixture.both = fixture.tied[1]
	fixture.otherNetworks = []server.Id{server.NewId(), server.NewId()}
	payouts := `[{"note": "tab\there \"quoted\" back\\slash line\nbreak \u0001 über ☃"}]`
	insertReviewTestSweep(t, ctx, fixture.both.contractId, f.balanceId, fixture.otherNetworks[0], fixture.reviewA, fixture.both.createTime.Add(time.Hour), &payouts)
	// a key holding a tab and a backslash, which COPY escapes
	fixture.escaped = fixture.tied[2]
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, "INSERT INTO contract_close(contract_id,party,used_transfer_byte_count) VALUES($1,$2,0),($1,$3,0)",
			fixture.escaped.contractId, "\t", `\`))
		server.RaisePgResult(tx.Exec(ctx, "INSERT INTO contract_extender(contract_id,extender_id,party,client_id,network_id) VALUES($1,$2,$3,$4,$5)",
			fixture.escaped.contractId, server.NewId(), "a\\b\tc", f.sourceId, f.sourceNetworkId))
	}, server.TxReadCommitted, server.OptNoRetry())
	if orphan {
		// a sweep whose contract row is gone
		fixture.orphanId = horizonTestContractId(at(40))
		insertReviewTestSweep(t, ctx, fixture.orphanId, f.balanceId, fixture.otherNetworks[1], fixture.reviewB, at(41), nil)
	}
	return fixture
}

func (self *reviewTestFixture) paymentIds() []string {
	return reviewTestPaymentIds(self.reviewA, self.reviewB)
}

// The export stops inside a batch, after some of the batch's tables are
// stored and before its manifest; then a run bounded to one batch; then a run
// to the end. The batches' copies, joined in batch order, hold every row of
// every table of every contract the two payments paid exactly once, as one
// COPY of all of them does, with the escaped text, the contract paid by both
// and the orphan sweep (whose missing contract row the batch records). The
// verification reads every copy back and every batch on Main again; a copy
// changed in one byte fails it, and a file of no batch is refused.
func TestReviewExportWritesEveryRowOnceAcrossAResume(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newReviewTestFixture(t, ctx, true)
		paymentIds := fixture.paymentIds()
		dir := filepath.Join(t.TempDir(), "review")

		at := fixture.now.Add(-60 * 24 * time.Hour)
		if usagearchive.ContractIdFloor(at) != server.ContractIdFloor(at).String() {
			t.Fatalf("the contract id floor %s is not the server's %s", usagearchive.ContractIdFloor(at), server.ContractIdFloor(at))
		}

		settings := reviewTestSettings()
		crashed := false
		settings.AfterTableStored = func(batch int64, table string) error {
			if batch == 2 && table == "transfer_escrow_sweep" && !crashed {
				crashed = true
				return errors.New("the export stopped here")
			}
			return nil
		}
		if _, err := exportReviewTest(t, ctx, dir, paymentIds, settings); err == nil || !crashed {
			t.Fatalf("the stop inside batch 2 did not stop the export: %v", err)
		}
		_, batches, unfinished, err := usagearchive.ReviewStatus(dir)
		if err != nil || len(batches) != 1 || len(unfinished) != 3 {
			t.Fatalf("after the stop: %d batches, unfinished %v, %v", len(batches), unfinished, err)
		}

		settings.MaxBatches = 1
		report, err := exportReviewTest(t, ctx, dir, nil, settings)
		if err != nil || report.Batches != 1 || report.TotalBatches != 2 || report.Done || report.Stop != "max_batches" {
			t.Fatalf("the bounded run: %+v, %v", report, err)
		}
		settings.MaxBatches = 0
		report, err = exportReviewTest(t, ctx, dir, paymentIds, settings)
		if err != nil || !report.Done || report.Stop != "done" {
			t.Fatalf("the last run: %+v, %v", report, err)
		}
		// the payments of another run are refused
		if _, err := exportReviewTest(t, ctx, dir, reviewTestPaymentIds(fixture.reviewA), settings); !usagearchive.IsReviewRefusal(err) {
			t.Fatalf("another run's payments were not refused: %v", err)
		}

		expected := reviewTestExpectedCopies(t, ctx, paymentIds)
		stored, batches := reviewTestStoredCopies(t, dir)
		for table, want := range expected {
			if len(want) == 0 {
				t.Fatalf("the fixture holds no %s row", table)
			}
			if !bytes.Equal(stored[table], want) {
				t.Fatalf("the copies of %s hold\n%s\nnot\n%s", table, stored[table], want)
			}
		}
		if !bytes.Contains(expected["transfer_escrow_sweep"], []byte(`\\"quoted\\"`)) || !bytes.Contains(expected["contract_close"], []byte(`\\`)) {
			t.Fatal("the fixture's escaped text is not in the copies")
		}
		var contracts int64
		missing := []string{}
		for _, batch := range batches {
			contracts += batch.Contracts
			missing = append(missing, batch.MissingContractIds...)
		}
		// eight contracts, and the orphan sweep's
		if contracts != 9 || len(batches) != 3 || !slices.Equal(missing, []string{fixture.orphanId.String()}) {
			t.Fatalf("%d batches of %d contracts, missing %v", len(batches), contracts, missing)
		}

		verification := verifyReviewTest(t, ctx, dir, true)
		if !verification.Ok || !verification.FilesChecked || !verification.MainChecked || !verification.MainRowsChecked || verification.Contracts != 9 {
			t.Fatalf("the verification: %s", verification.Json())
		}

		copyPath := filepath.Join(dir, batches[1].Tables[0].File)
		original, err := os.ReadFile(copyPath)
		if err != nil {
			t.Fatal(err)
		}
		changed := slices.Clone(original)
		changed[len(changed)/2] ^= 0x01
		if err := os.WriteFile(copyPath, changed, 0o640); err != nil {
			t.Fatal(err)
		}
		if verification := verifyReviewTest(t, ctx, dir, false); verification.Ok || verification.FilesChecked {
			t.Fatalf("a copy changed in one byte verified: %s", verification.Json())
		}
		if err := os.WriteFile(copyPath, original, 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "batch-000007.transfer_contract.copy.zst"), original, 0o640); err != nil {
			t.Fatal(err)
		}
		withReviewTestConn(ctx, func(conn *pgx.Conn) {
			if _, _, err := usagearchive.VerifyReview(ctx, conn, dir, "test", true, false, reviewTestSettings()); err == nil {
				t.Fatal("a file of no batch passed")
			}
		})
	})
}

// The plan counts each payment's sweeps and contracts and the contracts of
// both exactly, from chunks of mint time (here a minute, so the fixture spans
// several), counts one sample batch's rows of each table, and names a payment
// not under review as a blocker. It reads only.
func TestReviewPlanCountsTheTiedContracts(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newReviewTestFixture(t, ctx, true)
		settings := reviewTestSettings()
		settings.PlanChunk = time.Minute
		settings.MinPlanChunk = time.Second
		settings.SampleContracts = 100
		var report *usagearchive.ReviewPlanReport
		var err error
		withReviewTestConn(ctx, func(conn *pgx.Conn) {
			report, err = usagearchive.PlanReview(ctx, conn, fixture.paymentIds(), "", settings, io.Discard)
		})
		if err != nil {
			t.Fatal(err)
		}
		byPayment := map[string]*usagearchive.ReviewPlanPayment{}
		for _, planned := range report.Payments {
			byPayment[planned.PaymentId] = planned
		}
		// A paid five contracts and B's second sweep of a sixth; B three and the
		// orphan sweep
		a, b := byPayment[fixture.reviewA.String()], byPayment[fixture.reviewB.String()]
		if a.Contracts != 6 || a.Sweeps != 6 || b.Contracts != 4 || b.Sweeps != 4 || report.Contracts != 9 || 0 < len(report.Blockers) {
			t.Fatalf("plan: %s", report.Json())
		}
		var weeks int64
		for _, week := range report.Weeks {
			weeks += week.Contracts
		}
		rows := map[string]int64{}
		for _, table := range report.Tables {
			rows[table.Table] = table.SampleRows
		}
		if weeks != 9 || report.SampleContracts != 9 || rows["transfer_contract"] != 8 || rows["transfer_escrow_sweep"] != 10 || rows["contract_close"] != 18 {
			t.Fatalf("plan weeks %d, sample %d, rows %v", weeks, report.SampleContracts, rows)
		}
		withReviewTestConn(ctx, func(conn *pgx.Conn) {
			report, err = usagearchive.PlanReview(ctx, conn, reviewTestPaymentIds(fixture.reviewA, fixture.ordinary), "", settings, io.Discard)
		})
		if err != nil || len(report.Blockers) != 1 || !strings.Contains(report.Blockers[0], "not under attribution review") {
			t.Fatalf("a payment not under review was not a blocker: %v, %v", report, err)
		}
	})
}

// The clear refuses, writing nothing, while the export is partial, before it
// is verified, while its newest verification failed or is of other batches,
// and once a contract the export does not hold is paid by a payment of the
// run. A payment that is not completed under review cannot start a run.
func TestReviewClearRefusesAPartialOrUnverifiedExport(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newReviewTestFixture(t, ctx, false)
		dir := filepath.Join(t.TempDir(), "review")
		settings := reviewTestSettings()

		if _, err := exportReviewTest(t, ctx, filepath.Join(t.TempDir(), "ordinary"), reviewTestPaymentIds(fixture.ordinary), settings); !usagearchive.IsReviewRefusal(err) {
			t.Fatalf("a payment not under review started a run: %v", err)
		}

		refused := func(what string) {
			t.Helper()
			if _, _, err := clearReviewTest(t, ctx, dir, false, "", settings); !usagearchive.IsReviewRefusal(err) {
				t.Fatalf("%s: the clear's plan was not refused: %v", what, err)
			}
		}
		settings.MaxBatches = 1
		if _, err := exportReviewTest(t, ctx, dir, fixture.paymentIds(), settings); err != nil {
			t.Fatal(err)
		}
		refused("a partial export, unverified")
		if verification := verifyReviewTest(t, ctx, dir, false); verification.Ok || verification.MainChecked {
			t.Fatalf("a partial export verified: %s", verification.Json())
		}
		refused("a partial export whose verification failed")

		settings.MaxBatches = 0
		if _, err := exportReviewTest(t, ctx, dir, nil, settings); err != nil {
			t.Fatal(err)
		}
		refused("a complete export verified only while partial")
		if verification := verifyReviewTest(t, ctx, dir, false); !verification.Ok {
			t.Fatalf("the complete export did not verify: %s", verification.Json())
		}
		plan, _, err := clearReviewTest(t, ctx, dir, false, "", settings)
		if err != nil || plan.Contracts != 8 || len(plan.Payments) != 2 {
			t.Fatalf("the verified export's plan: %+v, %v", plan, err)
		}

		// a sweep of a contract inside the batches, paid by A after the
		// verification
		late := newHorizonTestContract(fixture.tied[0].createTime.Add(time.Second/2), "settled")
		late.paymentId = &fixture.reviewA
		insertHorizonTestContracts(t, ctx, fixture.f, late)
		if _, _, err := clearReviewTest(t, ctx, dir, false, "", settings); !usagearchive.IsReviewRefusal(err) || !strings.Contains(err.Error(), "does not cover") {
			t.Fatalf("a contract the export does not hold was not refused: %v", err)
		}
		if _, _, err := clearReviewTest(t, ctx, dir, true, plan.Digest, settings); !usagearchive.IsReviewRefusal(err) {
			t.Fatalf("the commit was not refused: %v", err)
		}
		if !reviewTestFlag(t, ctx, fixture.reviewA) || !reviewTestFlag(t, ctx, fixture.reviewB) || 0 < len(reviewTestEvents(t, ctx, usagearchive.ReviewClearedEventType)) {
			t.Fatal("a refused clear wrote")
		}
	})
}

// The clear refuses, writing nothing, when a payment changed since the export
// started, when the commit names another digest, and when a payment changes
// between the plan's reads and the update; the rows as they were are written
// first. A row changed back plans to the same digest.
func TestReviewClearRefusesChangedPaymentValues(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newReviewTestFixture(t, ctx, false)
		dir := filepath.Join(t.TempDir(), "review")
		settings := reviewTestSettings()
		if _, err := exportReviewTest(t, ctx, dir, fixture.paymentIds(), settings); err != nil {
			t.Fatal(err)
		}
		if verification := verifyReviewTest(t, ctx, dir, false); !verification.Ok {
			t.Fatalf("the export did not verify: %s", verification.Json())
		}
		plan, _, err := clearReviewTest(t, ctx, dir, false, "", settings)
		if err != nil {
			t.Fatal(err)
		}
		shift := func(paymentId server.Id, by time.Duration) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE account_payment SET complete_time = complete_time + $2::text::interval WHERE payment_id = $1`,
					paymentId, fmt.Sprintf("%d microseconds", by.Microseconds())))
			}, server.TxReadCommitted, server.OptNoRetry())
		}
		before := reviewTestPaymentRows(t, ctx)

		shift(fixture.reviewA, time.Second)
		if _, _, err := clearReviewTest(t, ctx, dir, true, plan.Digest, settings); !usagearchive.IsReviewRefusal(err) || !strings.Contains(err.Error(), "changed since the export started") {
			t.Fatalf("a changed payment was not refused: %v", err)
		}
		shift(fixture.reviewA, -time.Second)
		again, _, err := clearReviewTest(t, ctx, dir, false, "", settings)
		if err != nil || again.Digest != plan.Digest {
			t.Fatalf("the payment changed back plans to %v, %v; was %s", again, err, plan.Digest)
		}
		if _, _, err := clearReviewTest(t, ctx, dir, true, strings.Repeat("0", 64), settings); !usagearchive.IsReviewRefusal(err) {
			t.Fatalf("another digest was not refused: %v", err)
		}

		// B changes after the plan's reads, before the update
		settings.BeforeClearUpdate = func() error {
			shift(fixture.reviewB, time.Second)
			return nil
		}
		if _, _, err := clearReviewTest(t, ctx, dir, true, plan.Digest, settings); !usagearchive.IsReviewRefusal(err) || !strings.Contains(err.Error(), "no longer the row") {
			t.Fatalf("a payment changed before the update was not refused: %v", err)
		}
		after := reviewTestPaymentRows(t, ctx)
		for paymentId, row := range before {
			if paymentId != fixture.reviewB && after[paymentId] != row {
				t.Fatalf("payment %s changed: %s, was %s", paymentId, after[paymentId], row)
			}
		}
		if !reviewTestFlag(t, ctx, fixture.reviewA) || !reviewTestFlag(t, ctx, fixture.reviewB) || 0 < len(reviewTestEvents(t, ctx, usagearchive.ReviewClearedEventType)) {
			t.Fatal("a refused clear wrote")
		}
		records, _ := filepath.Glob(filepath.Join(dir, "clear-*-before.json"))
		finished, _ := filepath.Glob(filepath.Join(dir, "clear-*-after.json"))
		if len(records) != 1 || len(finished) != 0 {
			t.Fatalf("before records %v, after records %v", records, finished)
		}
	})
}

// Before the clear the reaper keeps every contract the payments under review
// paid. After the export, its verification and the clear of A and B, the next
// pass deletes exactly A's and B's contracts, every row of them, each archived
// as a deleted contract is, and the export holds every one of them; C's
// contracts and everything else stay as they were. The clear changed the flag
// of A and B and nothing else of any payment, and journaled each once. Its
// reversal sets the flag again, and the reaper keeps what is left.
func TestReviewClearLetsTheReaperDeleteOnlyTheExportedContracts(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer removeHorizonTestControl(t, ctx)
		fixture := newReviewTestFixture(t, ctx, false)
		const day = 24 * time.Hour
		// paid by A, minted inside the horizon
		recent := newHorizonTestContract(fixture.now.Add(-10*day), "settled")
		recent.paymentId = &fixture.reviewA
		// paid by B, with an escrow still unsettled
		unsettled := newHorizonTestContract(fixture.now.Add(-59*day), "settled")
		unsettled.paymentId = &fixture.reviewB
		unsettled.settled = false
		insertHorizonTestContracts(t, ctx, fixture.f, recent, unsettled)
		setHorizonTestControl(t, ctx, map[string]string{horizonTestDaysField: "35"})

		all := append(slices.Clone(fixture.tied), fixture.others...)
		all = append(all, recent, unsettled)
		rowsOf := func() map[server.Id][6]int {
			rows := map[server.Id][6]int{}
			for _, contract := range all {
				rows[contract.contractId], _ = countHorizonTestRows(t, ctx, contract.contractId)
			}
			return rows
		}
		start := rowsOf()

		report, _, err := RemoveContractsPastHorizon(ctx, fixture.now, ContractHorizonCursor{}, horizonTestSettings())
		if err != nil {
			t.Fatal(err)
		}
		// eight of A and B, two of C
		if report.Protected[contractHorizonProtectedPaymentReview] != 10 || report.Protected[contractHorizonProtectedUnsettled] != 1 {
			t.Fatalf("the pass before the clear: %+v", report)
		}
		kept := rowsOf()
		for _, contract := range all {
			gone := contract.paymentId != nil && *contract.paymentId == fixture.ordinary || contract.outcome == "canceled"
			if gone != (kept[contract.contractId] == [6]int{}) {
				t.Fatalf("before the clear, contract %s has rows %v, had %v", contract.contractId, kept[contract.contractId], start[contract.contractId])
			}
		}

		dir := filepath.Join(t.TempDir(), "review")
		settings := reviewTestSettings()
		if _, err := exportReviewTest(t, ctx, dir, fixture.paymentIds(), settings); err != nil {
			t.Fatal(err)
		}
		if verification := verifyReviewTest(t, ctx, dir, true); !verification.Ok {
			t.Fatalf("the export did not verify: %s", verification.Json())
		}
		paymentsBefore := reviewTestPaymentRows(t, ctx)
		plan, _, err := clearReviewTest(t, ctx, dir, false, "", settings)
		if err != nil {
			t.Fatal(err)
		}
		_, record, err := clearReviewTest(t, ctx, dir, true, plan.Digest, settings)
		if err != nil || len(record.Payments) != 2 || len(record.EventIds) != 2 {
			t.Fatalf("the clear: %+v, %v", record, err)
		}
		paymentsAfter := reviewTestPaymentRows(t, ctx)
		for paymentId, row := range paymentsBefore {
			changed := paymentId == fixture.reviewA || paymentId == fixture.reviewB
			if changed {
				var onlyTheFlag bool
				server.Db(ctx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(ctx, `SELECT $1::jsonb = ($2::jsonb || '{"attribution_review_required": false}'::jsonb)`, paymentsAfter[paymentId], row).Scan(&onlyTheFlag))
				})
				if !onlyTheFlag {
					t.Fatalf("payment %s became %s, was %s", paymentId, paymentsAfter[paymentId], row)
				}
			} else if paymentsAfter[paymentId] != row {
				t.Fatalf("payment %s changed: %s, was %s", paymentId, paymentsAfter[paymentId], row)
			}
		}
		if events := reviewTestEvents(t, ctx, usagearchive.ReviewClearedEventType); len(events) != 2 || events[fixture.reviewA] != 1 || events[fixture.reviewB] != 1 {
			t.Fatalf("journal events %v", events)
		}

		report, _, err = RemoveContractsPastHorizon(ctx, fixture.now, ContractHorizonCursor{}, horizonTestSettings())
		if err != nil {
			t.Fatal(err)
		}
		if report.Eligible != 8 || report.Archived != 8 || report.Protected[contractHorizonProtectedPaymentReview] != 2 || report.Protected[contractHorizonProtectedUnsettled] != 1 {
			t.Fatalf("the pass after the clear: %+v", report)
		}
		stored, _ := reviewTestStoredCopies(t, dir)
		reaped := rowsOf()
		for _, contract := range all {
			deleted := slices.Contains(fixture.tied, contract)
			var archived int
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM st_provider_usage_archive WHERE contract_id = $1`, contract.contractId).Scan(&archived))
			})
			switch {
			case deleted:
				if reaped[contract.contractId] != [6]int{} || archived != 1 {
					t.Fatalf("contract %s of A or B kept rows %v, archived %d", contract.contractId, reaped[contract.contractId], archived)
				}
				if !bytes.Contains(stored["transfer_contract"], []byte(contract.contractId.String())) {
					t.Fatalf("the export does not hold deleted contract %s", contract.contractId)
				}
			default:
				if reaped[contract.contractId] != kept[contract.contractId] {
					t.Fatalf("contract %s changed: %v, was %v", contract.contractId, reaped[contract.contractId], kept[contract.contractId])
				}
			}
		}

		// the reversal, from the record written before the clear
		priors, _ := filepath.Glob(filepath.Join(dir, "clear-*-before.json"))
		if len(priors) != 1 {
			t.Fatalf("before records %v", priors)
		}
		var restorePlan *usagearchive.ReviewClearPlan
		withReviewTestConn(ctx, func(conn *pgx.Conn) {
			restorePlan, _, err = usagearchive.RestoreReviewFlag(ctx, conn, dir, priors[0], "test", false, "", settings)
		})
		if err != nil {
			t.Fatal(err)
		}
		withReviewTestConn(ctx, func(conn *pgx.Conn) {
			_, _, err = usagearchive.RestoreReviewFlag(ctx, conn, dir, priors[0], "test", true, restorePlan.Digest, settings)
		})
		if err != nil {
			t.Fatal(err)
		}
		restored := reviewTestPaymentRows(t, ctx)
		for paymentId, row := range paymentsBefore {
			if restored[paymentId] != row {
				t.Fatalf("payment %s after the reversal is %s, was %s", paymentId, restored[paymentId], row)
			}
		}
		if events := reviewTestEvents(t, ctx, usagearchive.ReviewRestoredEventType); len(events) != 2 {
			t.Fatalf("reversal journal events %v", events)
		}
		withReviewTestConn(ctx, func(conn *pgx.Conn) {
			_, _, err = usagearchive.RestoreReviewFlag(ctx, conn, dir, priors[0], "test", false, "", settings)
		})
		if !usagearchive.IsReviewRefusal(err) {
			t.Fatalf("a second reversal was not refused: %v", err)
		}
		report, _, err = RemoveContractsPastHorizon(ctx, fixture.now, ContractHorizonCursor{}, horizonTestSettings())
		if err != nil || report.Eligible != 0 || report.Protected[contractHorizonProtectedPaymentReview] != 2 {
			t.Fatalf("the pass after the reversal: %+v, %v", report, err)
		}
	})
}
