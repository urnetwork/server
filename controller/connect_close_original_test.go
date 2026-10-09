// Hosted HTTP and resident frame routes retain one exact client-signed original.
package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
)

// Tracing the real hosted and frame owners catches schema checks hidden below
// either route. Setup is complete before the disposable pool scope is installed.
type closeOriginalCatalogTripwire struct {
	catalogReads atomic.Int64
	historyReads atomic.Int64
}

// Count attempted statements, including errors, without retaining report bytes.
func (self *closeOriginalCatalogTripwire) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
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

// The tripwire observes attempts regardless of the resulting command tag.
func (self *closeOriginalCatalogTripwire) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// This signs at creation, before either actual control route sees the frame.
func signedCloseControlReport(t testing.TB, f *closeReportControlFixture, domain [32]byte) *protocol.CloseContract {
	t.Helper()
	report := &protocol.CloseContract{ContractId: f.contractId.Bytes(), ReportId: server.NewId().Bytes(), AckedByteCount: 121, UnackedByteCount: 7, Checkpoint: true}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{61}, 32))
	original, err := protocol.SignOriginalCloseReport(protocol.OriginalCloseReport{DomainHash: domain, ClientId: [16]byte(f.sourceId), ContractId: [16]byte(f.contractId), ReportId: [16]byte(report.ReportId), AckedByteCount: 121, UnackedByteCount: 7, Checkpoint: true}, key)
	if err != nil {
		t.Fatal(err)
	}
	report.OriginalReport, err = original.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// A lost HTTP response followed by frame and HTTP retries retains the complete
// signed bytes. Each route admits a fresh original without querying the catalog;
// missing registration remains unknown in the required deployed history schema.
func TestCloseOriginalHttpFrameLostReplyRetainsExactSignature(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportControlFixture(t)
		report := signedCloseControlReport(t, f, [32]byte{3})
		tripwire := &closeOriginalCatalogTripwire{}
		scope, err := server.NewTestPgQueryScope(f.ctx, tripwire)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()
		if err := f.http(t, report); err != nil {
			t.Fatal(err)
		}
		frame := closeReportControlFrame(t, report)
		defer returnConnectControlFrames([]*protocol.Frame{frame})
		results, err := ConnectControlFrames(f.ctx, f.sourceId, []*protocol.Frame{frame}, connect.DefaultContractManagerSettings())
		defer returnConnectControlFrames(results)
		if err != nil || f.http(t, report) != nil {
			t.Fatal("exact original route retries did not converge", err)
		}
		frameReport := signedCloseControlReport(t, f, [32]byte{3})
		newFrame := closeReportControlFrame(t, frameReport)
		defer returnConnectControlFrames([]*protocol.Frame{newFrame})
		newResults, err := ConnectControlFrames(f.ctx, f.sourceId, []*protocol.Frame{newFrame}, connect.DefaultContractManagerSettings())
		defer returnConnectControlFrames(newResults)
		if err != nil || f.http(t, frameReport) != nil {
			t.Fatal("frame original admission or hosted retry failed", err)
		}
		if catalog, history := tripwire.catalogReads.Load(), tripwire.historyReads.Load(); catalog != 0 || history != 2 {
			t.Fatalf("hosted/frame signed close catalog tripwire: catalog=%d history=%d want=0/2", catalog, history)
		}
		assertControlCloseReportCensus(t, f, f.contractId, 2, 242)
		server.Db(f.ctx, func(conn server.PgConn) {
			for _, retainedReport := range []*protocol.CloseContract{report, frameReport} {
				var original, registration []byte
				var issue string
				server.Raise(conn.QueryRow(f.ctx, `SELECT original_report,original_key_registration,original_key_issue FROM contract_close_report_evidence WHERE client_id=$1 AND report_id=$2`, f.sourceId, server.RequireIdFromBytes(retainedReport.ReportId)).Scan(&original, &registration, &issue))
				if !bytes.Equal(original, retainedReport.OriginalReport) || len(registration) != 0 || issue != "history_not_found" {
					t.Fatal("HTTP/frame original changed or gained absent registration")
				}
			}
		})
	})
}

// A present false envelope is scoped to its frame. Unsigned ordinary reports
// from rolling peers still progress through the same healthy batch owner.
func TestCloseOriginalForeignEnvelopeKeepsHealthyLegacyBatch(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportControlFixture(t)
		bad := signedCloseControlReport(t, f, [32]byte{4})
		bad.AckedByteCount++
		healthy := &protocol.CloseContract{ContractId: f.otherContractId.Bytes(), ReportId: server.NewId().Bytes(), AckedByteCount: 73, Checkpoint: true}
		frames := []*protocol.Frame{closeReportControlFrame(t, bad), closeReportControlFrame(t, healthy)}
		defer returnConnectControlFrames(frames)
		results, _ := ConnectControlFrames(f.ctx, f.sourceId, frames, connect.DefaultContractManagerSettings())
		defer returnConnectControlFrames(results)
		assertControlCloseReportCensus(t, f, f.contractId, 0, 0)
		assertControlCloseReportCensus(t, f, f.otherContractId, 1, 73)
		bad.ReportId = nil
		if err := CloseContract(f.ctx, f.sourceId, bad); err == nil {
			t.Fatal("an identity-less envelope fell through to legacy accounting")
		}
		assertControlCloseReportCensus(t, f, f.contractId, 0, 0)
	})
}
