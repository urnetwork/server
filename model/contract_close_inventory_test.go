// Actual accounting transactions retain optional inventory independently of
// delivery retries, mutable client directories and the original report clock.
package model

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/urfoundation/sn/payoutartifact"
	coreprotocol "github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
)

func signCloseInventory(t testing.TB, report ContractCloseReport, sequence, total uint64, previous [32]byte, key ed25519.PrivateKey) ContractCloseReport {
	t.Helper()
	report = signCloseReportOriginal(t, report, [32]byte{71}, key)
	original, err := coreprotocol.DecodeOriginalCloseReport(report.OriginalReport)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := coreprotocol.SignOriginalCloseInventory(coreprotocol.OriginalCloseInventory{DomainHash: original.DomainHash, ClientId: original.ClientId, ContractId: original.ContractId, ReportHash: sha256.Sum256(report.OriginalReport), Sequence: sequence, CumulativeAckedBytes: total, Previous: previous, Terminal: !report.Checkpoint}, key)
	if err != nil {
		t.Fatal(err)
	}
	report.OriginalInventory, err = inventory.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestContractInventoryActualCloseRetainsOriginalAcrossRetryAndRotation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{72}, 32))
		first := signCloseInventory(t, f.report(), 1, 20, [32]byte{}, key)
		if applied, err := CloseContractWithReport(f.ctx, first); err != nil || !applied {
			t.Fatal("original inventory was not retained with accounting", applied, err)
		}
		for _, retry := range []ContractCloseReport{first, func() ContractCloseReport { v := first; v.OriginalInventory = nil; return v }()} {
			if applied, err := CloseContractWithReport(f.ctx, retry); err != nil || applied {
				t.Fatal("inventory retry recounted work", applied, err)
			}
		}
		next := f.report()
		next.Checkpoint = false
		next = signCloseInventory(t, next, 2, 40, sha256.Sum256(first.OriginalInventory), ed25519.NewKeyFromSeed(bytes.Repeat([]byte{73}, 32)))
		if applied, err := CloseContractWithReport(f.ctx, next); err != nil || !applied {
			t.Fatal("rotation lost original inventory", applied, err)
		}
		server.Db(f.ctx, func(conn server.PgConn) {
			for _, report := range []ContractCloseReport{first, next} {
				var raw []byte
				server.Raise(conn.QueryRow(f.ctx, `SELECT original_inventory FROM contract_close_report_evidence WHERE client_id=$1 AND report_id=$2`, report.ClientId, report.ReportId).Scan(&raw))
				if !bytes.Equal(raw, report.OriginalInventory) {
					t.Fatal("retained inventory was regenerated")
				}
			}
		})
		assertCloseReportCounts(t, f.ctx, f.contractId, 2, 40)
	})
}

func TestContractInventoryChangedCompanionAndForeignTupleRefuse(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{74}, 32))
		report := signCloseInventory(t, f.report(), 1, 20, [32]byte{}, key)
		if _, err := CloseContractWithReport(f.ctx, report); err != nil {
			t.Fatal(err)
		}
		changed := report
		changed.OriginalInventory = bytes.Clone(report.OriginalInventory)
		changed.OriginalInventory[len(changed.OriginalInventory)-1] ^= 1
		if _, err := CloseContractWithReport(f.ctx, changed); !errors.Is(err, ErrContractCloseOriginalIntegrity) {
			t.Fatal("invalid inventory signature reached accounting", err)
		}
		changed = signCloseInventory(t, report, 2, 40, [32]byte{1}, key)
		if _, err := CloseContractWithReport(f.ctx, changed); !errors.Is(err, ErrContractCloseReportConflict) {
			t.Fatal("same report accepted a changed original inventory", err)
		}
		changed = f.report()
		changed.OriginalInventory = report.OriginalInventory
		if _, err := CloseContractWithReport(f.ctx, changed); !errors.Is(err, ErrContractCloseOriginalIntegrity) {
			t.Fatal("foreign inventory without matching original was accepted", err)
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 1, 20)
	})
}

func TestContractInventoryCanceledOwnerRollsBackAndHealthyRetryContinues(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		report := signCloseInventory(t, f.report(), 1, 20, [32]byte{}, ed25519.NewKeyFromSeed(bytes.Repeat([]byte{75}, 32)))
		ctx, cancel := context.WithCancel(f.ctx)
		cancel()
		if applied, err := CloseContractWithReport(ctx, report); applied || !errors.Is(err, context.Canceled) {
			t.Fatal("canceled inventory acquired accounting", applied, err)
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 0, 0)
		if applied, err := CloseContractWithReport(f.ctx, report); err != nil || !applied {
			t.Fatal("healthy original inventory did not recover", applied, err)
		}
	})
}

func TestStClosedWorkWindowRetainsCanceledOpenAndCreditedOriginals(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		start := server.NowUtc().Add(-time.Minute)
		end := start.Add(time.Hour)
		// Keep the other actual contract open and close this original normally.
		for _, client := range []server.Id{f.sourceId, f.destinationId} {
			report := f.report()
			report.ClientId = client
			report.Checkpoint = false
			report.AckedByteCount = 121
			if _, err := CloseContractWithReport(f.ctx, report); err != nil {
				t.Fatal(err)
			}
		}
		canceled := server.NewId()
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `INSERT INTO transfer_contract(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,outcome,close_time) SELECT $1,source_network_id,source_id,destination_network_id,destination_id,0,'canceled',$2 FROM transfer_contract WHERE contract_id=$3`, canceled, server.NowUtc(), f.contractId))
		})
		usage, census, err := GetStEpochProviderUsageCensus(f.ctx, 17, start, end)
		if err != nil || census == nil || len(census.Records) != 1 || len(usage) != 1 || usage[0].PayoutByteCount != 121 {
			t.Fatal("window changed actual completed credit", usage, census, err)
		}
		var originals payoutartifact.ClosedWorkReports
		if err := json.Unmarshal(census.Records[0].OriginalReports, &originals); err != nil {
			t.Fatal(err)
		}
		if originals.Window == nil || len(originals.Window.Records) != 3 {
			t.Fatal("same statement lost canceled/open original inventory", originals.Window)
		}
		seen := map[string]int{}
		for _, row := range originals.Window.Records {
			seen[row.Disposition]++
		}
		if seen["credited"] != 1 || seen["open"] != 1 || seen["canceled"] != 1 {
			t.Fatal("window promoted canceled or open contracts to earned work", seen)
		}
		artifact := &payoutartifact.Artifact{ClosedWork: census}
		value, err := payoutartifact.VerifyClosedWorkWindow(f.ctx, artifact, originals.Window, nil)
		if err != nil || value.Credited != 1 || value.Canceled != 1 || value.Open != 1 || value.EpochClockMatched {
			t.Fatal("SQL inventory fabricated independent epoch authority", value, err)
		}
	})
}
