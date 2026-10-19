// Public close admission reads immutable history without rediscovering schema.
package model

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

// Driver callbacks cover every query from the public entry, including helpers
// that acquire another owner. Fixture setup completes before this is installed.
type originalCloseCatalogTripwire struct {
	catalogReads atomic.Int64
	historyReads atomic.Int64
}

// Count attempted catalog reads even when the statement itself fails.
func (self *originalCloseCatalogTripwire) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	sql := strings.ToLower(data.SQL)
	for _, catalog := range []string{"pg_catalog", "information_schema", "pg_attribute", "pg_class", "pg_namespace", "pg_constraint", "pg_index", "pg_type", "to_regclass", "to_regprocedure"} {
		if strings.Contains(sql, catalog) {
			self.catalogReads.Add(1)
			break
		}
	}
	if strings.Contains(sql, "from st_client_key_history") {
		self.historyReads.Add(1)
	}
	return ctx
}

// Admission assertions use attempts, so success cannot hide a catalog check.
func (self *originalCloseCatalogTripwire) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// An exact history read is still required for each newly signed admission;
// retained retries and unsigned reports must not acquire that authority again.
func (self *originalCloseCatalogTripwire) assertReads(t testing.TB, expectedHistory int64) {
	t.Helper()
	if catalog, history := self.catalogReads.Load(), self.historyReads.Load(); catalog != 0 || history != expectedHistory {
		t.Fatalf("signed close catalog tripwire: catalog=%d history=%d want=0/%d", catalog, history, expectedHistory)
	}
}

// New checkpoints, missing-key originals and final reports use the real public
// owner. The old readiness statement trips this test on the first admission.
func TestContractCloseOriginalPublicAdmissionsNeverReadCatalog(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f, input, record, key := originalCloseAvailabilityFixture(t)
		domain, err := input.Domain.Digest()
		server.Raise(err)
		foreignDomain := domain
		foreignDomain[0] ^= 1
		tripwire := &originalCloseCatalogTripwire{}
		scope, err := server.NewTestPgQueryScope(f.ctx, tripwire)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()
		for _, check := range []struct {
			checkpoint   bool
			signed       bool
			domain       [32]byte
			registration []byte
			issue        string
			historyReads int64
		}{
			{checkpoint: true, signed: true, domain: domain, registration: record.RegistrationBytes, historyReads: 1},
			{checkpoint: true, signed: true, domain: foreignDomain, issue: originalCloseKeyMissing, historyReads: 2},
			{checkpoint: true, historyReads: 2},
			{signed: true, domain: domain, registration: record.RegistrationBytes, historyReads: 3},
		} {
			report := f.report()
			report.Checkpoint = check.checkpoint
			if check.signed {
				report = signCloseReportOriginal(t, report, check.domain, key)
			}
			if applied, err := CloseContractWithReport(f.ctx, report); !applied || err != nil {
				t.Fatal("public close admission failed", applied, err)
			}
			tripwire.assertReads(t, check.historyReads)
			if applied, err := CloseContractWithReport(f.ctx, report); applied || err != nil {
				t.Fatal("retained close replay changed custody", applied, err)
			}
			tripwire.assertReads(t, check.historyReads)
			original, registration := retainedCloseOriginal(t, report)
			if !bytes.Equal(original, report.OriginalReport) || !bytes.Equal(registration, check.registration) || retainedCloseKeyIssue(t, report) != check.issue {
				t.Fatal("public admission changed original history or its diagnostic")
			}
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 4, 80)
		assertCloseReportLegacyReceipts(t, f.ctx, f.contractId, 4)
		tripwire.assertReads(t, 3)
	})
}

// Retained rollout-era unknown evidence stays immutable even with healthy
// matching history now present. Removing readiness cannot backfill its authority.
func TestContractCloseOriginalRetainedUnavailableHistoryNeverBackfills(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f, input, _, key := originalCloseAvailabilityFixture(t)
		domain, err := input.Domain.Digest()
		server.Raise(err)
		report := signCloseReportOriginal(t, f.report(), domain, key)
		server.Tx(f.ctx, func(tx server.PgTx) {
			// Seed the historical admission using the unchanged custody schema.
			server.RaisePgResult(tx.Exec(f.ctx, `INSERT INTO contract_close_report_evidence
 (client_id,report_id,contract_id,party,acked_byte_count,unacked_byte_count,checkpoint,accepted_at,original_report,original_key_issue)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'history_read_unavailable')`,
				report.ClientId, report.ReportId, report.ContractId, ContractPartySource, report.AckedByteCount, strconv.FormatUint(report.UnackedByteCount, 10), report.Checkpoint, server.NowUtc(), report.OriginalReport))
			applied, _, err := applyContractCloseReportInTx(f.ctx, tx, report.ContractId, report.ClientId, report.AckedByteCount, report.Checkpoint, &report.ReportId)
			server.Raise(err)
			if !applied {
				t.Fatal("historical close fixture failed to retain its byte increment")
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		tripwire := &originalCloseCatalogTripwire{}
		scope, err := server.NewTestPgQueryScope(f.ctx, tripwire)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()
		for _, omitOriginal := range []bool{false, true} {
			retry := report
			if omitOriginal {
				retry.OriginalReport = nil
			}
			if applied, err := CloseContractWithReport(f.ctx, retry); applied || err != nil {
				t.Fatal("historical unknown close failed exact replay", applied, err)
			}
		}
		original, registration := retainedCloseOriginal(t, report)
		if !bytes.Equal(original, report.OriginalReport) || len(registration) != 0 || retainedCloseKeyIssue(t, report) != "history_read_unavailable" {
			t.Fatal("historical unknown evidence acquired registration authority")
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 1, 20)
		assertCloseReportLegacyReceipts(t, f.ctx, f.contractId, 1)
		tripwire.assertReads(t, 0)
	})
}
