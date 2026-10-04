// Hosted HTTP and resident frame routes retain one exact client-signed original.
package controller

import (
	"bytes"
	"crypto/ed25519"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
)

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
// signed bytes; absence of historical registration is not a startup dependency.
func TestCloseOriginalHttpFrameLostReplyRetainsExactSignature(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportControlFixture(t)
		report := signedCloseControlReport(t, f, [32]byte{3})
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
		assertControlCloseReportCensus(t, f, f.contractId, 1, 121)
		server.Db(f.ctx, func(conn server.PgConn) {
			var original, registration []byte
			server.Raise(conn.QueryRow(f.ctx, `SELECT original_report,original_key_registration FROM contract_close_report WHERE client_id=$1 AND report_id=$2`, f.sourceId, server.RequireIdFromBytes(report.ReportId)).Scan(&original, &registration))
			if !bytes.Equal(original, report.OriginalReport) || len(registration) != 0 {
				t.Fatal("HTTP/frame original changed or gained absent registration")
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
