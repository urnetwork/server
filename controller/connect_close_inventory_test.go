// A serialized inventory pair is identical through hosted, resident and retry
// routes. The Server retains original bytes in the actual accounting transaction.
package controller

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
)

func TestCloseInventoryHttpFrameLostReplyRetainsExactPair(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportControlFixture(t)
		report := signedCloseControlReport(t, f, [32]byte{3})
		original, err := protocol.DecodeOriginalCloseReport(report.OriginalReport)
		if err != nil {
			t.Fatal(err)
		}
		inventory, err := protocol.SignOriginalCloseInventory(protocol.OriginalCloseInventory{DomainHash: original.DomainHash, ClientId: original.ClientId, ContractId: original.ContractId, ReportHash: sha256.Sum256(report.OriginalReport), Sequence: 1, CumulativeAckedBytes: 121, Terminal: false}, ed25519.NewKeyFromSeed(bytes.Repeat([]byte{61}, 32)))
		if err != nil {
			t.Fatal(err)
		}
		report.OriginalInventory, err = inventory.Bytes()
		if err != nil {
			t.Fatal(err)
		}
		if err := f.http(t, report); err != nil {
			t.Fatal(err)
		}
		frame := closeReportControlFrame(t, report)
		defer returnConnectControlFrames([]*protocol.Frame{frame})
		results, err := ConnectControlFrames(f.ctx, f.sourceId, []*protocol.Frame{frame}, connect.DefaultContractManagerSettings())
		defer returnConnectControlFrames(results)
		if err != nil || f.http(t, report) != nil {
			t.Fatal("original inventory route retries did not converge", err)
		}
		assertControlCloseReportCensus(t, f, f.contractId, 1, 121)
		server.Db(f.ctx, func(conn server.PgConn) {
			var original, companion []byte
			server.Raise(conn.QueryRow(f.ctx, `SELECT original_report,original_inventory FROM contract_close_report_evidence WHERE client_id=$1 AND report_id=$2`, f.sourceId, server.RequireIdFromBytes(report.ReportId)).Scan(&original, &companion))
			if !bytes.Equal(original, report.OriginalReport) || !bytes.Equal(companion, report.OriginalInventory) {
				t.Fatal("Http/frame retry changed the original inventory pair")
			}
		})
	})
}
