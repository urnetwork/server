package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// §21.9's copied_rows (abuse_hold_copy_model.go; ABUSE-M5-15): the part format in either build, and the copy, its
// resumption, the drop precondition and the parts' fate at a hold's end on MG5's tables, in a build with
// sidecar_pending.

// Both sides of a contract may identify the held network. A retry after deletion
// must serve the placement snapshot, and a missing source never certifies a hold.
func TestHoldPlacementSnapshotsContractsAndFailsClosed(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestLegalHoldStore(t)
		network, firstCase := newTestLegalHoldCase(t, ctx)
		other := server.NewId()
		at := server.NowUtc().Add(time.Hour).Truncate(time.Millisecond)
		var ids []server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			// Decision 529 names whole rows of these three tables. Check the explicit
			// copy columns against the installed schema, including nullable accounting fields.
			for _, table := range holdContractTables {
				var columns []string
				server.Raise(tx.QueryRow(ctx, `SELECT array_agg(attname::text ORDER BY attname)
					FROM pg_attribute WHERE attrelid=to_regclass($1) AND attnum>0 AND NOT attisdropped`, "public."+table.name).Scan(&columns))
				expected := slices.Clone(table.columns)
				slices.Sort(expected)
				if !slices.Equal(columns, expected) {
					t.Fatalf("%s holds columns %v, its placement copies %v", table.name, columns, expected)
				}
			}
			for _, pair := range [][2]server.Id{{network, other}, {other, network}, {other, other}} {
				id := server.NewId()
				ids = append(ids, id)
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				   (contract_id,source_id,source_network_id,destination_id,destination_network_id,transfer_byte_count,create_time)
				   VALUES ($1,$2,$3,$4,$5,32,$6)`, id, server.NewId(), pair[0], server.NewId(), pair[1], at.Add(-time.Minute)))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close (contract_id,party,used_transfer_byte_count,close_time) VALUES ($1,'source',17,$2)`, id, at.Add(-time.Second)))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close_report (contract_id,party,report_id,used_transfer_byte_count,checkpoint,create_time) VALUES ($1,'source',$2,17,false,$3)`, id, server.NewId(), at.Add(-time.Second)))
			}
		})
		args := &LegalHoldArgs{NetworkId: network, CaseId: firstCase, Kind: sidecar.HoldKindPreserve2703f}
		SetAbuseHoldSoftStateSource(nil)
		if _, _, err := PlaceLegalHold(ctx, args, at, nil); !errors.Is(err, ErrHoldPlacementSnapshotUnavailable) || GetLegalHold(ctx, network, firstCase) != nil {
			t.Fatalf("unavailable placement snapshot: %v", err)
		}
		repo := server.NewId()
		reads := 0
		SetAbuseHoldSoftStateSource(&testHoldSoftStateSource{repoIds: []server.Id{repo}, read: func(id server.Id) []*AbuseHoldSoftStateKey {
			reads++
			return []*AbuseHoldSoftStateKey{{Key: "synthetic-lease", Fields: map[string]string{"revision": fmt.Sprint(reads)}}}
		}})
		if _, _, err := PlaceLegalHold(ctx, args, at, nil); !errors.Is(err, ErrHoldPlacementSnapshotUnavailable) {
			t.Fatalf("a changing lease was accepted: %v", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var parts int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM abuse_legal_hold WHERE network_id=$1 AND case_id=$2`, network, firstCase).Scan(&parts))
			if parts != 0 {
				t.Fatalf("an incomplete snapshot published %d parts", parts)
			}
		})
		SetAbuseHoldSoftStateSource(&testHoldSoftStateSource{})
		if _, inserted, err := PlaceLegalHold(ctx, args, at, nil); err != nil || !inserted {
			t.Fatalf("placement: %t, %v", inserted, err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, "UPDATE abuse_legal_hold SET copy_state='copied' WHERE network_id=$1 AND case_id=$2 AND part=0", network, firstCase))
			if allowed := contractsOutsidePendingHolds(ctx, tx, ids); !slices.Equal(allowed, ids[2:]) {
				t.Fatalf("legacy copy released held contracts: %v", allowed)
			}
			server.RaisePgResult(tx.Exec(ctx, "UPDATE abuse_legal_hold SET copy_state=$3 WHERE network_id=$1 AND case_id=$2 AND part=0", network, firstCase, server.LegalHoldPlacementSnapshot))
			if allowed := contractsOutsidePendingHolds(ctx, tx, ids); len(allowed) != len(ids) {
				t.Fatalf("complete placement retained contracts: %v", allowed)
			}
			server.RaisePgResult(tx.Exec(ctx, "DELETE FROM contract_close_report WHERE contract_id=ANY($1::uuid[])", ids))
			server.RaisePgResult(tx.Exec(ctx, "DELETE FROM contract_close WHERE contract_id=ANY($1::uuid[])", ids))
			server.RaisePgResult(tx.Exec(ctx, "DELETE FROM transfer_contract WHERE contract_id=ANY($1::uuid[])", ids))
		})
		SetAbuseHoldSoftStateSource(nil)
		complete, err := RunAbuseHoldCopy(ctx, network, firstCase, at, time.Now().Add(time.Minute), DefaultAbuseHoldCopySettings())
		if err != nil || !complete {
			t.Fatalf("copy after source deletion: %t, %v", complete, err)
		}
		for _, table := range holdContractTables {
			copied, _ := testHoldCopiedRows(t, ctx, network, firstCase, table.name)
			if len(copied) != 2 || !slices.Contains(copied, ids[0].String()) || !slices.Contains(copied, ids[1].String()) {
				t.Fatalf("%s captured %v", table.name, copied)
			}
		}
		// A legacy "copied" state cannot vouch for a missing dataset.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, "DELETE FROM abuse_legal_hold WHERE network_id=$1 AND case_id=$2 AND part=1", network, firstCase))
			server.RaisePgResult(tx.Exec(ctx, "UPDATE abuse_legal_hold SET copy_state='copied' WHERE network_id=$1 AND case_id=$2 AND part=0", network, firstCase))
		})
		if complete, err = RunAbuseHoldCopy(ctx, network, firstCase, at, time.Now().Add(time.Minute), DefaultAbuseHoldCopySettings()); complete || !errors.Is(err, ErrHoldPlacementSnapshotUnavailable) {
			t.Fatalf("unverified legacy copy: %t, %v", complete, err)
		}
	})
}

// A report can reach its absolute deadline while the worker verifies the second reading.
func TestHoldExpiryRollsBackPlacementSnapshot(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		_, clock := newTestLegalHoldStore(t)
		networkId, caseId := newTestLegalHoldCase(t, ctx)
		reported := clock.Now().AddDate(-LegalHoldReportYears, 0, 0).Add(time.Second)
		reads := 0
		SetAbuseHoldSoftStateSource(&testHoldSoftStateSource{repoIds: []server.Id{server.NewId()}, read: func(server.Id) []*AbuseHoldSoftStateKey {
			reads++
			if reads == 2 {
				clock.Advance(time.Second)
			}
			return nil
		}})
		args := &LegalHoldArgs{NetworkId: networkId, CaseId: caseId, Kind: sidecar.HoldKindReport2258a, ReportTime: &reported}
		hold, _, err := PlaceLegalHoldWithClock(ctx, args, clock.Now, nil)
		if !errors.Is(err, ErrLegalHoldExpired) || hold != nil || reads != 2 {
			t.Fatalf("expired snapshot returned a hold %t, error %v after %d reads", hold != nil, err, reads)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(ctx, "SELECT count(*) FROM abuse_legal_hold WHERE network_id=$1 AND case_id=$2", networkId, caseId).Scan(&count))
			if count != 0 {
				t.Fatalf("expired placement left %d parts", count)
			}
		})
	})
}

// A table registered for the copy that only the one test reading it links, so no other hold copies it.
var testHoldCopyTableLinked atomic.Bool

const testHoldCopyTable = "abuse_hold_copy_test_rows"

func init() {
	RegisterHoldCopy(&HoldCopyTable{
		Table:         testHoldCopyTable,
		NetworkColumn: "network_id",
		TimeColumn:    "write_time",
		KeyColumn:     "row_id",
		Columns:       []string{"row_id", "network_id", "write_time"},
		Linked:        testHoldCopyTableLinked.Load,
	})
}

// A part reads back what was written, header and lines; one past its bound, or a header past its own, is refused.
func TestHoldCopyPartRoundTripsWithinItsBound(t *testing.T) {
	settings := DefaultAbuseHoldCopySettings()
	caseId := server.NewId()
	header := &HoldCopyPartHeader{
		Version:  holdCopyPartVersion,
		CaseId:   caseId,
		Kind:     HoldCopyPartKindRows,
		Table:    "t",
		Columns:  []string{"a", "b"},
		LastTime: "2026-03-02 12:00:00",
		LastKey:  "k",
		Complete: true,
		RowCount: 2,
	}
	lines := [][]byte{[]byte(`["1",null]`), []byte(`["2","\\x00ff"]`)}
	body, err := encodeHoldCopyPart(header, lines, settings)
	if err != nil {
		t.Fatal(err)
	}
	readHeader, readLines, err := ReadHoldCopyPart(body, settings)
	if err != nil {
		t.Fatal(err)
	}
	if readHeader.CaseId != caseId || readHeader.Table != "t" || !readHeader.Complete || readHeader.LastKey != "k" || len(readLines) != 2 ||
		string(readLines[0]) != string(lines[0]) || string(readLines[1]) != string(lines[1]) {
		t.Fatalf("read back %+v %q", readHeader, readLines)
	}
	prefixHeader, _, err := readHoldCopyPartHeader(body[:min(len(body), holdCopyPartPrefixByteCount+settings.PartHeaderMaxByteCount)], settings)
	if err != nil || prefixHeader.CaseId != caseId {
		t.Fatalf("the header from the prefix: %+v, %v", prefixHeader, err)
	}

	small := DefaultAbuseHoldCopySettings()
	small.PartMaxByteCount = 32
	random := make([]byte, 4096)
	for i := range random {
		random[i] = byte(i * 7919 % 251)
	}
	encoded, _ := json.Marshal(string(random))
	if _, err := encodeHoldCopyPart(header, [][]byte{encoded}, small); !errors.Is(err, ErrHoldCopyPartInvalid) {
		t.Fatalf("a part past its bound: %v", err)
	}
	small = DefaultAbuseHoldCopySettings()
	small.PartHeaderMaxByteCount = 16
	if _, err := encodeHoldCopyPart(header, nil, small); !errors.Is(err, ErrHoldCopyPartInvalid) {
		t.Fatalf("a header past its bound: %v", err)
	}
	for _, bad := range [][]byte{nil, {2, 0, 0, 0, 1, '{'}, {1, 0, 0, 0xff, 0xff}} {
		if _, _, err := ReadHoldCopyPart(bad, settings); !errors.Is(err, ErrHoldCopyPartInvalid) {
			t.Errorf("%v read", bad)
		}
	}
}

// A soft state source of fixed keys by repo, and the network's repos.
type testHoldSoftStateSource struct {
	repoIds  []server.Id
	repoKeys map[server.Id][]*AbuseHoldSoftStateKey
	read     func(server.Id) []*AbuseHoldSoftStateKey
}

func (self *testHoldSoftStateSource) NetworkRepoIds(ctx context.Context, tx server.PgTx, networkId server.Id) ([]server.Id, error) {
	return self.repoIds, nil
}

func (self *testHoldSoftStateSource) RepoSoftState(ctx context.Context, repoId server.Id, at time.Time) ([]*AbuseHoldSoftStateKey, error) {
	if self.read != nil {
		return self.read(repoId), nil
	}
	return self.repoKeys[repoId], nil
}

// Writes tls-3 rows of network ending at each time.
func testHoldCopyRecords(t testing.TB, ctx context.Context, networkId server.Id, endTimes ...time.Time) []*TlsConnectionRecord {
	t.Helper()
	records := []*TlsConnectionRecord{}
	for _, endTime := range endTimes {
		record := tlsTestRecord(networkId, endTime)
		result, err := writeTlsConnectionRecordsAt(ctx, []*TlsConnectionRecord{record}, endTime.Add(time.Minute), DefaultTlsConnectionSettings())
		if err != nil || result.WrittenCount != 1 {
			t.Fatalf("write %+v, %v", result, err)
		}
		records = append(records, record)
	}
	return records
}

// Every copied row of table across the hold's parts, by its key, and how many parts carried rows of it.
func testHoldCopiedRows(t testing.TB, ctx context.Context, networkId server.Id, caseId server.Id, table string) (keys []string, partCount int) {
	t.Helper()
	settings := DefaultAbuseHoldCopySettings()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT copied_rows FROM abuse_legal_hold WHERE network_id = $1 AND case_id = $2 AND part > 0 ORDER BY part`, networkId, caseId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var body []byte
				server.Raise(result.Scan(&body))
				header, lines, err := ReadHoldCopyPart(body, settings)
				server.Raise(err)
				if header.Kind != HoldCopyPartKindRows || header.Table != table {
					continue
				}
				partCount += 1
				for _, line := range lines {
					values := []*string{}
					server.Raise(json.Unmarshal(line, &values))
					keys = append(keys, *values[0])
				}
			}
		})
	})
	return
}

// The hold's soft state parts' headers and keys.
func testHoldSoftStateParts(t testing.TB, ctx context.Context, networkId server.Id, caseId server.Id) (headers []*HoldCopyPartHeader, keys []*AbuseHoldSoftStateKey) {
	t.Helper()
	settings := DefaultAbuseHoldCopySettings()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT copied_rows FROM abuse_legal_hold WHERE network_id = $1 AND case_id = $2 AND part > 0 ORDER BY part`, networkId, caseId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var body []byte
				server.Raise(result.Scan(&body))
				header, lines, err := ReadHoldCopyPart(body, settings)
				server.Raise(err)
				if header.Kind != HoldCopyPartKindSoftState {
					continue
				}
				headers = append(headers, header)
				for _, line := range lines {
					key := &AbuseHoldSoftStateKey{}
					server.Raise(json.Unmarshal(line, key))
					keys = append(keys, key)
				}
			}
		})
	})
	return
}

// The hour the copy tests write in: months ahead, clear of every other test's hours.
func testHoldCopyHour(monthsAhead int) time.Time {
	return server.PartitionGranularityDaily.RangeStart(server.NowUtc().AddDate(0, monthsAhead, 0)).Add(6 * time.Hour)
}

// The copy takes the network's rows from before the placement and nothing written after it, nor another network's,
// in parts that read back equal; it ends copied, and a rerun adds nothing.
func TestHoldCopyTakesOnlyWhatExistedAtPlacement(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestLegalHoldStore(t)
		hour := testHoldCopyHour(4)
		networkId, caseId := newTestLegalHoldCase(t, ctx)
		before := testHoldCopyRecords(t, ctx, networkId, hour.Add(5*time.Minute), hour.Add(10*time.Minute), hour.Add(20*time.Minute))
		testHoldCopyRecords(t, ctx, server.NewId(), hour.Add(12*time.Minute))
		placeTime := hour.Add(30 * time.Minute)
		if _, _, err := PlaceLegalHold(ctx, &LegalHoldArgs{NetworkId: networkId, CaseId: caseId, Kind: sidecar.HoldKindPreserve2703f}, placeTime, nil); err != nil {
			t.Fatal(err)
		}
		testHoldCopyRecords(t, ctx, networkId, hour.Add(40*time.Minute))

		settings := DefaultAbuseHoldCopySettings()
		complete, err := RunAbuseHoldCopy(ctx, networkId, caseId, placeTime.Add(time.Minute), time.Now().Add(time.Minute), settings)
		if err != nil || !complete {
			t.Fatalf("%t, %v", complete, err)
		}
		keys, _ := testHoldCopiedRows(t, ctx, networkId, caseId, tlsConnectionRecordTable)
		want := []string{}
		for _, record := range before {
			want = append(want, record.ConnectionId.String())
		}
		if !slices.Equal(keys, want) {
			t.Fatalf("copied %v, want %v", keys, want)
		}
		if hold := GetLegalHold(ctx, networkId, caseId); hold.CopyState != LegalHoldCopyStateCopied {
			t.Fatalf("copy state %q", hold.CopyState)
		}
		// A successful read of an empty source is a completed placement snapshot.
		headers, _ := testHoldSoftStateParts(t, ctx, networkId, caseId)
		if len(headers) != 1 || headers[0].Unavailable || !headers[0].Complete {
			t.Fatalf("soft state parts %+v", headers)
		}
		if complete, err := RunAbuseHoldCopy(ctx, networkId, caseId, placeTime.Add(time.Hour), time.Now().Add(time.Minute), settings); err != nil || !complete {
			t.Fatalf("a rerun: %t, %v", complete, err)
		}
		if again, _ := testHoldCopiedRows(t, ctx, networkId, caseId, tlsConnectionRecordTable); len(again) != len(want) {
			t.Fatalf("a rerun copied %d rows", len(again))
		}
	})
}

// A copy cut short between parts resumes from its last part with no row twice, and its parts stay inside their
// bound.
func TestHoldCopyResumesWithoutDuplicates(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestLegalHoldStore(t)
		hour := testHoldCopyHour(5)
		networkId, caseId := newTestLegalHoldCase(t, ctx)
		endTimes := []time.Time{}
		for i := range 12 {
			endTimes = append(endTimes, hour.Add(time.Duration(i+1)*time.Minute))
		}
		records := testHoldCopyRecords(t, ctx, networkId, endTimes...)
		placeTime := hour.Add(30 * time.Minute)
		if _, _, err := PlaceLegalHold(ctx, &LegalHoldArgs{NetworkId: networkId, CaseId: caseId, Kind: sidecar.HoldKindPreserve2703f}, placeTime, nil); err != nil {
			t.Fatal(err)
		}
		settings := DefaultAbuseHoldCopySettings()
		settings.ReadBatchRowCount = 2
		// a part after every batch
		settings.PartRawMaxByteCount = 1

		parts := 0
		Testing_HoldCopyBeforePart = func(what string) {
			if what != tlsConnectionRecordTable {
				return
			}
			parts += 1
			if parts == 3 {
				runtime.Goexit()
			}
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			RunAbuseHoldCopy(ctx, networkId, caseId, placeTime, time.Now().Add(time.Minute), settings)
		}()
		<-done
		Testing_HoldCopyBeforePart = nil
		if keys, partCount := testHoldCopiedRows(t, ctx, networkId, caseId, tlsConnectionRecordTable); len(keys) != 4 || partCount != 2 {
			t.Fatalf("before the crash %d rows in %d parts", len(keys), partCount)
		}
		if GetLegalHold(ctx, networkId, caseId).CopyState == LegalHoldCopyStateCopied {
			t.Fatal("a copy cut short reads copied")
		}

		complete, err := RunAbuseHoldCopy(ctx, networkId, caseId, placeTime, time.Now().Add(time.Minute), settings)
		if err != nil || !complete {
			t.Fatalf("the resume: %t, %v", complete, err)
		}
		keys, partCount := testHoldCopiedRows(t, ctx, networkId, caseId, tlsConnectionRecordTable)
		want := []string{}
		for _, record := range records {
			want = append(want, record.ConnectionId.String())
		}
		if !slices.Equal(keys, want) || partCount < 6 {
			t.Fatalf("after the resume %d rows in %d parts: %v", len(keys), partCount, keys)
		}
		var largest int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(MAX(length(copied_rows)), 0) FROM abuse_legal_hold WHERE network_id = $1`, networkId).Scan(&largest))
		})
		if settings.PartMaxByteCount < largest {
			t.Fatalf("a part of %d bytes", largest)
		}
	})
}

// A table whose copy fails leaves the others copied, and the hold not copied until it too is; then it completes.
func TestHoldCopyFailingOnOneTableLeavesTheOthers(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestLegalHoldStore(t)
		hour := testHoldCopyHour(6)
		networkId, caseId := newTestLegalHoldCase(t, ctx)
		testHoldCopyRecords(t, ctx, networkId, hour.Add(5*time.Minute))
		placeTime := hour.Add(30 * time.Minute)
		if _, _, err := PlaceLegalHold(ctx, &LegalHoldArgs{NetworkId: networkId, CaseId: caseId, Kind: sidecar.HoldKindPreserve2703f}, placeTime, nil); err != nil {
			t.Fatal(err)
		}
		testHoldCopyTableLinked.Store(true)
		defer testHoldCopyTableLinked.Store(false)

		// the test table does not exist yet, so its read fails
		settings := DefaultAbuseHoldCopySettings()
		complete, err := RunAbuseHoldCopy(ctx, networkId, caseId, placeTime, time.Now().Add(time.Minute), settings)
		if err == nil || complete || !strings.Contains(err.Error(), testHoldCopyTable) {
			t.Fatalf("%t, %v", complete, err)
		}
		if keys, _ := testHoldCopiedRows(t, ctx, networkId, caseId, tlsConnectionRecordTable); len(keys) != 1 {
			t.Fatalf("the other table copied %d rows", len(keys))
		}
		if GetLegalHold(ctx, networkId, caseId).CopyState == LegalHoldCopyStateCopied {
			t.Fatal("a copy with a failed table reads copied")
		}

		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (row_id uuid NOT NULL, network_id uuid NOT NULL, write_time timestamp NOT NULL)`, testHoldCopyTable)))
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES ($1, $2, $3), ($4, $2, $5)`, testHoldCopyTable), server.NewId(), networkId, placeTime.Add(-time.Minute), server.NewId(), placeTime.Add(time.Minute)))
		})
		complete, err = RunAbuseHoldCopy(ctx, networkId, caseId, placeTime, time.Now().Add(time.Minute), settings)
		if err != nil || !complete {
			t.Fatalf("after the table exists: %t, %v", complete, err)
		}
		if keys, _ := testHoldCopiedRows(t, ctx, networkId, caseId, testHoldCopyTable); len(keys) != 1 {
			t.Fatalf("the test table copied %d rows", len(keys))
		}
		if keys, _ := testHoldCopiedRows(t, ctx, networkId, caseId, tlsConnectionRecordTable); len(keys) != 1 {
			t.Fatalf("the other table was copied again: %d rows", len(keys))
		}
	})
}

// The soft state of the repos in the hold's scope is recorded as it existed, a repo with no key recording nothing
// and a repo outside the scope left out.
func TestHoldCopyRecordsSoftStatePresentAndAbsent(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestLegalHoldStore(t)
		networkId, caseId := newTestLegalHoldCase(t, ctx)
		leasedRepo := server.NewId()
		quietRepo := server.NewId()
		outsideRepo := server.NewId()
		SetAbuseHoldSoftStateSource(&testHoldSoftStateSource{
			repoIds: []server.Id{leasedRepo, quietRepo, outsideRepo},
			repoKeys: map[server.Id][]*AbuseHoldSoftStateKey{
				leasedRepo:  {{Key: "{evsl_" + leasedRepo.String() + "}", Fields: map[string]string{"main": "lease"}}},
				outsideRepo: {{Key: "{evsl_" + outsideRepo.String() + "}", Fields: map[string]string{"main": "lease"}}},
			},
		})
		defer SetAbuseHoldSoftStateSource(nil)
		placeTime := testHoldCopyHour(7)
		if _, _, err := PlaceLegalHold(ctx, &LegalHoldArgs{
			NetworkId: networkId,
			CaseId:    caseId,
			Kind:      sidecar.HoldKindPreserve2703f,
			Scope:     sidecar.HoldScopeRepos(sidecar.Id(leasedRepo), sidecar.Id(quietRepo)),
		}, placeTime, nil); err != nil {
			t.Fatal(err)
		}
		if complete, err := RunAbuseHoldCopy(ctx, networkId, caseId, placeTime, time.Now().Add(time.Minute), DefaultAbuseHoldCopySettings()); err != nil || !complete {
			t.Fatalf("%t, %v", complete, err)
		}
		headers, keys := testHoldSoftStateParts(t, ctx, networkId, caseId)
		if len(headers) != 1 || headers[0].RepoId == nil || *headers[0].RepoId != leasedRepo || headers[0].Unavailable {
			t.Fatalf("soft state parts %+v", headers)
		}
		if len(keys) != 1 || keys[0].Fields["main"] != "lease" {
			t.Fatalf("keys %+v", keys)
		}
	})
}

// While a hold placed inside an hour has not copied, that hour stays and the unheld hours drop; the precondition
// holds on the leaf just detached; once copied, the hour drops on time.
func TestHoldCopyPendingKeepsOnlyItsRangeUntilCopied(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestLegalHoldStore(t)
		family := tlsConnectionRecordPartitionFamily
		hour := testHoldCopyHour(8)
		networkId, caseId := newTestLegalHoldCase(t, ctx)
		testHoldCopyRecords(t, ctx, server.NewId(), hour.Add(-30*time.Minute))
		testHoldCopyRecords(t, ctx, networkId, hour.Add(10*time.Minute))
		testHoldCopyRecords(t, ctx, networkId, hour.Add(70*time.Minute))
		placeTime := hour.Add(30 * time.Minute)
		if _, _, err := PlaceLegalHold(ctx, &LegalHoldArgs{NetworkId: networkId, CaseId: caseId, Kind: sidecar.HoldKindPreserve2703f}, placeTime, nil); err != nil {
			t.Fatal(err)
		}
		heldLeaf := family.LeafName(tlsConnectionRecordTable, server.PartitionRange{Lower: hour, Upper: hour.Add(time.Hour)})
		heldRange := server.PartitionRange{Lower: hour, Upper: hour.Add(time.Hour)}

		// the signature is DropPrecondition's
		var precondition func(ctx context.Context, conn server.PgQuerier, r server.PartitionRange) string = func(ctx context.Context, conn server.PgQuerier, r server.PartitionRange) string {
			return AbuseHoldCopyPending(ctx, conn, tlsConnectionRecordTable, family.LeafName(tlsConnectionRecordTable, r), r, server.NowUtc())
		}
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			if reason := precondition(ctx, conn, heldRange); !strings.Contains(reason, caseId.String()) {
				t.Fatalf("the held hour may drop: %q", reason)
			}
		})
		// across the detach: the leaf out of its parent still keeps the range
		func() {
			defer func() {
				recover()
			}()
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE public.%s DETACH PARTITION public.%s`, tlsConnectionRecordTable, heldLeaf)))
				if reason := precondition(ctx, tx, heldRange); !strings.Contains(reason, caseId.String()) {
					t.Errorf("the detached hour may drop: %q", reason)
				}
				panic("roll the detach back")
			})
		}()

		report, _ := family.Retire(ctx, hour.Add(26*time.Hour), server.PartitionFamilyState{}, time.Now().Add(time.Minute))
		if slices.Contains(report.Dropped, heldLeaf) || !strings.Contains(report.Blocked[heldLeaf], "has not copied") {
			t.Fatalf("dropped %v, blocked %v", report.Dropped, report.Blocked)
		}
		if len(report.Dropped) != 2 {
			t.Fatalf("the unheld hours: dropped %v", report.Dropped)
		}

		if complete, err := RunAbuseHoldCopy(ctx, networkId, caseId, placeTime, time.Now().Add(time.Minute), DefaultAbuseHoldCopySettings()); err != nil || !complete {
			t.Fatalf("%t, %v", complete, err)
		}
		report, _ = family.Retire(ctx, hour.Add(26*time.Hour), server.PartitionFamilyState{}, time.Now().Add(time.Minute))
		if !slices.Equal(report.Dropped, []string{heldLeaf}) {
			t.Fatalf("after the copy: dropped %v, blocked %v", report.Dropped, report.Blocked)
		}
		if keys, _ := testHoldCopiedRows(t, ctx, networkId, caseId, tlsConnectionRecordTable); len(keys) != 1 {
			t.Fatalf("the hold kept %d rows of the dropped hour", len(keys))
		}
	})
}

// At a hold's end its parts move to a live hold of the network placed after it, rows always and soft state only
// for a repo the receiver covers; with none, they go with the hold.
func TestEndedHoldCopiesMoveToALaterHoldOrGo(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestLegalHoldStore(t)
		hour := testHoldCopyHour(9)
		networkId, firstCase := newTestLegalHoldCase(t, ctx)
		laterCase := testHoldCaseNamed(t, ctx, networkId, "later")
		repoId := server.NewId()
		SetAbuseHoldSoftStateSource(&testHoldSoftStateSource{
			repoIds:  []server.Id{repoId},
			repoKeys: map[server.Id][]*AbuseHoldSoftStateKey{repoId: {{Key: "{evsl_" + repoId.String() + "}", Fields: map[string]string{"f": "v"}}}},
		})
		defer SetAbuseHoldSoftStateSource(nil)
		testHoldCopyRecords(t, ctx, networkId, hour.Add(5*time.Minute))
		firstPlace := hour.Add(30 * time.Minute)
		if _, _, err := PlaceLegalHold(ctx, &LegalHoldArgs{NetworkId: networkId, CaseId: firstCase, Kind: sidecar.HoldKindPreserve2703f}, firstPlace, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := RunAbuseHoldCopy(ctx, networkId, firstCase, firstPlace, time.Now().Add(time.Minute), DefaultAbuseHoldCopySettings()); err != nil {
			t.Fatal(err)
		}
		// the later hold covers another repo only: the first's soft state does not move to it
		laterPlace := firstPlace.AddDate(0, 0, 10)
		reportTime := laterPlace
		if _, _, err := PlaceLegalHold(ctx, &LegalHoldArgs{
			NetworkId:  networkId,
			CaseId:     laterCase,
			Kind:       sidecar.HoldKindReport2258a,
			Scope:      sidecar.HoldScopeRepos(sidecar.NewId()),
			ReportTime: &reportTime,
		}, laterPlace, nil); err != nil {
			t.Fatal(err)
		}

		var ended bool
		server.Tx(ctx, func(tx server.PgTx) {
			_, ended = EndLegalHoldInTx(ctx, tx, networkId, firstCase, firstPlace.AddDate(0, 0, LegalHoldPreserveDays))
		})
		if !ended {
			t.Fatal("the first hold did not end")
		}
		keys, _ := testHoldCopiedRows(t, ctx, networkId, laterCase, tlsConnectionRecordTable)
		if len(keys) != 1 {
			t.Fatalf("the later hold took %d rows", len(keys))
		}
		if headers, _ := testHoldSoftStateParts(t, ctx, networkId, laterCase); len(headers) != 1 || headers[0].RepoId != nil || headers[0].CaseId != laterCase {
			t.Fatalf("soft state of a repo the later hold does not cover moved: %+v", headers)
		}
		settings := DefaultAbuseHoldCopySettings()
		server.Db(ctx, func(conn server.PgConn) {
			parts, _ := readHoldCopyParts(ctx, conn, networkId, laterCase, settings)
			for _, part := range parts {
				if part.header.CaseId != firstCase && part.header.CaseId != laterCase {
					t.Errorf("a moved part says it came from %s", part.header.CaseId)
				}
			}
		})

		// the later hold ends with no hold after it: its parts go with it
		server.Tx(ctx, func(tx server.PgTx) {
			_, ended = EndLegalHoldInTx(ctx, tx, networkId, laterCase, reportTime.AddDate(LegalHoldReportYears, 0, 0))
		})
		var parts int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM abuse_legal_hold WHERE network_id = $1`, networkId).Scan(&parts))
		})
		if !ended || parts != 0 {
			t.Fatalf("ended %t, %d parts stayed", ended, parts)
		}
	})
}

// Another staff-filed case naming the network, its target told apart by name so it opens its own case.
func testHoldCaseNamed(t testing.TB, ctx context.Context, networkId server.Id, name string) (caseId server.Id) {
	t.Helper()
	var err error
	server.Tx(ctx, func(tx server.PgTx) {
		caseId, _, err = OpenAbuseCaseInTx(ctx, tx, &AbuseCaseArgs{
			NetworkId:     &networkId,
			Category:      AbuseCategoryOther,
			Target:        AbuseTarget{Kind: AbuseTargetKindNetwork, Name: name + "." + networkId.String()},
			ReporterClass: AbuseReporterClassLawEnforcement,
			JoinKind:      AbuseJoinKindNetwork,
			JoinKey:       networkId.String(),
		}, server.NowUtc(), DefaultAbuseSettings())
	})
	if err != nil {
		t.Fatal(err)
	}
	return
}
