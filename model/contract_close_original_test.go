// Actual report transactions retain client originals through rotation and cleanup.
package model

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/urfoundation/sn/payoutartifact"
	coreprotocol "github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
)

// Only locally generated synthetic keys sign test reports.
func signCloseReportOriginal(t testing.TB, report ContractCloseReport, domain [32]byte, key ed25519.PrivateKey) ContractCloseReport {
	t.Helper()
	original, err := coreprotocol.SignOriginalCloseReport(coreprotocol.OriginalCloseReport{DomainHash: domain, ClientId: [16]byte(report.ClientId), ContractId: [16]byte(report.ContractId), ReportId: [16]byte(report.ReportId), AckedByteCount: uint64(report.AckedByteCount), UnackedByteCount: report.UnackedByteCount, Checkpoint: report.Checkpoint}, key)
	if err != nil {
		t.Fatal(err)
	}
	report.OriginalReport, err = original.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// Retain exactly what production accepted, not a regenerated signature or head.
func retainedCloseOriginal(t testing.TB, report ContractCloseReport) ([]byte, []byte) {
	t.Helper()
	var original, registration []byte
	server.Db(t.Context(), func(conn server.PgConn) {
		server.Raise(conn.QueryRow(t.Context(), `SELECT original_report,original_key_registration FROM contract_close_report_evidence WHERE client_id=$1 AND report_id=$2`, report.ClientId, report.ReportId).Scan(&original, &registration))
	})
	return original, registration
}

// Rotation and directory retirement cannot reinterpret one previously accepted
// domain/key or demand today's registration before acknowledging an exact retry.
func TestContractCloseOriginalRetainsKeyPolicyAcrossRotationAndCleanup(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		input := newStClientKeyHistoryTestInput(t)
		input.ClientID = f.sourceId
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{51}, 32))
		input.PublicKey = key[32:]
		registration, err := StoreStClientKeyRegistration(f.ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		domain, _ := input.Domain.Digest()
		report := signCloseReportOriginal(t, f.report(), domain, key)
		if applied, err := CloseContractWithReport(f.ctx, report); err != nil || !applied {
			t.Fatal("original signature was not admitted with its increment", applied, err)
		}
		input.PublicKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{52}, 32))[32:]
		input.Boundary.Block++
		input.Boundary.Hash[0]++
		if _, err := StoreStClientKeyRegistration(f.ctx, input); err != nil {
			t.Fatal(err)
		}
		input.Domain.PolicyHash[0]++
		input.Boundary.Block++
		input.Boundary.Hash[0]++
		if _, err := StoreStClientKeyRegistration(f.ctx, input); err != nil {
			t.Fatal(err)
		}
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `DELETE FROM network_client WHERE client_id=$1`, f.sourceId))
		})
		for _, retry := range []ContractCloseReport{report, func() ContractCloseReport { copy := report; copy.OriginalReport = nil; return copy }()} {
			if applied, err := CloseContractWithReport(f.ctx, retry); err != nil || applied {
				t.Fatal("rotation or old intermediary changed original retry", applied, err)
			}
		}
		original, retained := retainedCloseOriginal(t, report)
		if !bytes.Equal(original, report.OriginalReport) || !bytes.Equal(retained, registration.RegistrationBytes) {
			t.Fatal("exact original signature or key registration was replaced")
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 1, 20)
	})
}

// A cryptographically valid report without matching history remains useful
// signed input, but it cannot acquire a registration from a foreign namespace.
func TestContractCloseOriginalMissingAndForeignHistoryRemainUnknown(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		input := newStClientKeyHistoryTestInput(t)
		input.ClientID = f.sourceId
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{53}, 32))
		input.PublicKey = key[32:]
		if _, err := StoreStClientKeyRegistration(f.ctx, input); err != nil {
			t.Fatal(err)
		}
		foreign := input.Domain
		foreign.NoID++
		domain, _ := foreign.Digest()
		report := signCloseReportOriginal(t, f.report(), domain, key)
		if applied, err := CloseContractWithReport(f.ctx, report); err != nil || !applied {
			t.Fatal("unknown optional registration blocked ordinary close", applied, err)
		}
		original, registration := retainedCloseOriginal(t, report)
		if !bytes.Equal(original, report.OriginalReport) || len(registration) != 0 {
			t.Fatal("foreign history became this report's registration")
		}
		legacy := f.report()
		legacy.OriginalReport = []byte{}
		if applied, err := CloseContractWithReport(f.ctx, legacy); err != nil || !applied {
			t.Fatal("empty optional legacy bytes blocked a healthy sibling", applied, err)
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 2, 40)
	})
}

// A signature for another tuple is refused before reservation. A valid newly
// signed envelope cannot retrofit an already admitted unsigned increment.
func TestContractCloseOriginalTupleConflictAndLateBackfillRefuse(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{54}, 32))
		report := signCloseReportOriginal(t, f.report(), [32]byte{1}, key)
		changed := report
		changed.AckedByteCount++
		if applied, err := CloseContractWithReport(f.ctx, changed); applied || !errors.Is(err, ErrContractCloseOriginalIntegrity) {
			t.Fatal("foreign signed tuple reached original accounting", applied, err)
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 0, 0)
		legacy := report
		legacy.OriginalReport = nil
		if _, err := CloseContractWithReport(f.ctx, legacy); err != nil {
			t.Fatal(err)
		}
		if applied, err := CloseContractWithReport(f.ctx, report); applied || !errors.Is(err, ErrContractCloseReportConflict) {
			t.Fatal("later signature was relabeled as an original admission", applied, err)
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 1, 20)
	})
}

// Both settled parties remain visible in the same statement snapshot after the
// financial row and directory move away. The query carries original signed bytes.
func TestStClosedWorkCensusCarriesOriginalSignaturesAcrossRetention(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		start := server.NowUtc().Add(-time.Minute)
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{55}, 32))
		var originals [][]byte
		for _, clientId := range []server.Id{f.sourceId, f.destinationId} {
			report := f.report()
			report.ClientId, report.Checkpoint, report.AckedByteCount = clientId, false, 121
			report = signCloseReportOriginal(t, report, [32]byte{2}, key)
			if _, err := CloseContractWithReport(f.ctx, report); err != nil {
				t.Fatal(err)
			}
			originals = append(originals, report.OriginalReport)
		}
		usages, census, err := GetStEpochProviderUsageCensus(f.ctx, 17, start, start.Add(time.Hour))
		if err != nil || census == nil || len(census.Records) != 1 || len(usages) != 1 || usages[0].PayoutByteCount != 121 {
			t.Fatal("actual signed closes did not produce original usage census", usages, census, err)
		}
		var reports payoutartifact.ClosedWorkReports
		if err := json.Unmarshal(census.Records[0].OriginalReports, &reports); err != nil || reports.Schema != payoutartifact.ClosedWorkReportsSchema || reports.Count != 2 || len(reports.Reports) != 2 {
			t.Fatal("same statement omitted original close reports", reports, err)
		}
		for _, original := range originals {
			found := false
			for _, retained := range reports.Reports {
				found = found || bytes.Equal(retained.Original, original)
			}
			if !found {
				t.Fatal("original close bytes were regenerated or omitted")
			}
		}
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, f.contractId))
			server.RaisePgResult(tx.Exec(f.ctx, `DELETE FROM network_client WHERE client_id=ANY($1::uuid[])`, []server.Id{f.sourceId, f.destinationId}))
		})
		_, after, err := GetStEpochProviderUsageCensus(f.ctx, 17, start, start.Add(time.Hour))
		if err != nil || after == nil || after.Hash() != census.Hash() {
			t.Fatal("retention changed original signed close census", after, err)
		}
	})
}

// The indexed sentinel proves overflow; no bounded prefix is exported as a
// complete original. Ordinary completed-work amounts remain available.
func TestStClosedWorkOriginalReportCapacityOmitsWholeProof(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		if stOriginalCloseReportLimit != 1024 {
			t.Fatal("original SQL sentinel differs from shared decoder capacity")
		}
		f := newCloseReportFixture(t)
		start := server.NowUtc().Add(-time.Minute)
		// Zero-byte checkpoint rows exercise the actual immutable census without
		// introducing any additional byte credit or fabricated signatures.
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `INSERT INTO contract_close_report_evidence(client_id,report_id,contract_id,party,acked_byte_count,unacked_byte_count,checkpoint,accepted_at)
 SELECT $1,md5('synthetic-original-capacity-'||n::text)::uuid,$2,'source',0,0,true,$3 FROM generate_series(1,$4) n`, f.sourceId, f.contractId, server.NowUtc(), stOriginalCloseReportLimit+1))
		})
		for _, clientId := range []server.Id{f.sourceId, f.destinationId} {
			report := f.report()
			report.ClientId, report.Checkpoint, report.AckedByteCount = clientId, false, 121
			if _, err := CloseContractWithReport(f.ctx, report); err != nil {
				t.Fatal(err)
			}
		}
		usages, census, err := GetStEpochProviderUsageCensus(f.ctx, 17, start, start.Add(time.Hour))
		if err != nil || census == nil || len(census.Records) != 1 || len(census.Records[0].OriginalReports) != 0 || len(usages) != 1 || usages[0].PayoutByteCount != 121 {
			t.Fatal("overflow published an original prefix or erased ordinary work", usages, census, err)
		}
	})
}
