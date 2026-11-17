package evidencemove

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func testSnapshotRecords(count int, seed uint64) []SnapshotRecord {
	random := rand.New(rand.NewPCG(seed, seed))
	records := make([]SnapshotRecord, 0, count)
	for len(records) < count {
		clientId := server.NewId()
		for generation := int64(1); generation <= 1+random.Int64N(3) && len(records) < count; generation += 1 {
			var record SnapshotRecord
			record.ClientId, record.Generation = clientId, generation
			for index := range record.Hash {
				record.Hash[index] = byte(random.Uint32())
			}
			record.Size = 1000 + random.Uint32N(15000)
			records = append(records, record)
		}
	}
	slices.SortFunc(records, func(a SnapshotRecord, b SnapshotRecord) int { return compareCursors(a.Cursor(), b.Cursor()) })
	return records
}

func writeTestSnapshot(t *testing.T, path string, records []SnapshotRecord) {
	t.Helper()
	writer, err := OpenSnapshotWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Append(records); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

// Records round trip through the file; a partial trailing record a crash left
// is cut off on reopen and the export resumes after the last whole record;
// a record out of key order is refused.
func TestSnapshotWriterResumesAfterAPartialRecord(t *testing.T) {
	records := testSnapshotRecords(25, 1)
	path := filepath.Join(t.TempDir(), "hashes.bin")
	writeTestSnapshot(t, path, records[:10])
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	partial := make([]byte, SnapshotRecordBytes)
	records[10].encode(partial)
	if _, err := file.Write(partial[:17]); err != nil {
		t.Fatal(err)
	}
	file.Close()
	writer, err := OpenSnapshotWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	if writer.Rows() != 10 || writer.Cursor() != records[9].Cursor() {
		t.Fatalf("reopened at %d rows, %s", writer.Rows(), writer.Cursor())
	}
	if err := writer.Append(records[5:6]); err == nil {
		t.Fatal("an out-of-order record was appended")
	}
	if err := writer.Append(records[10:]); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	data, err := os.ReadFile(path)
	if err != nil || len(data) != 25*SnapshotRecordBytes {
		t.Fatalf("file %d bytes, %v", len(data), err)
	}
	for index, record := range records {
		if decoded := decodeSnapshotRecord(data[index*SnapshotRecordBytes:]); decoded != record {
			t.Fatalf("record %d decoded %+v, want %+v", index, decoded, record)
		}
	}
	manifest, err := SummarizeSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if manifest.Rows != 25 || manifest.Sha256 != hex.EncodeToString(sum[:]) || *manifest.First != records[0].Cursor() || *manifest.Last != records[24].Cursor() {
		t.Fatalf("manifest %+v", manifest)
	}
	if err := WriteSnapshotManifest(path, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(SnapshotManifestPath(path)); err != nil {
		t.Fatal(err)
	}
}

// The snapshot source answers every cursor and limit exactly as the same
// items in memory do, so the mover reads either the same way.
func TestSnapshotSourceReadsLikeTheTable(t *testing.T) {
	records := testSnapshotRecords(300, 2)
	path := filepath.Join(t.TempDir(), "hashes.bin")
	writeTestSnapshot(t, path, records)
	source, err := OpenSnapshotSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	items := make([]Item, len(records))
	for index, record := range records {
		items[index] = record.Item()
	}
	memory := NewSliceSource(items)
	random := rand.New(rand.NewPCG(3, 3))
	cursors := []Cursor{{}, records[0].Cursor(), records[len(records)-1].Cursor()}
	for range 200 {
		record := records[random.IntN(len(records))]
		cursor := record.Cursor()
		switch random.IntN(3) {
		case 1:
			// the next generation of the same client, stored or not
			cursor.Generation += 1
		case 2:
			// a client id between rows
			cursor.ClientId[15] ^= 0x01
		}
		cursors = append(cursors, cursor)
	}
	for _, cursor := range cursors {
		for _, limit := range []int{1, 7, 1000} {
			want, _ := memory.Next(t.Context(), cursor, limit)
			got, err := source.Next(t.Context(), cursor, limit)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("after %s limit %d: %d items, want %d", cursor, limit, len(got), len(want))
			}
		}
	}
}

// A move begun on the snapshot continues on the live table's order with the
// same checkpoint, and every object ends moved.
func TestMoverContinuesBetweenSnapshotAndTable(t *testing.T) {
	evidence := testEvidenceSet(t, 20)
	store := newMemoryStore()
	items := testItems(evidence)
	records := make([]SnapshotRecord, len(items))
	for index, entry := range evidence {
		store.put(testKeys(t, entry.hash).Flat, entry.bytes)
		hash, err := hex.DecodeString(entry.hash)
		if err != nil {
			t.Fatal(err)
		}
		records[index] = SnapshotRecord{ClientId: items[index].Cursor.ClientId, Generation: items[index].Cursor.Generation, Size: uint32(items[index].Size)}
		copy(records[index].Hash[:], hash)
	}
	path := filepath.Join(t.TempDir(), "hashes.bin")
	writeTestSnapshot(t, path, records)
	source, err := OpenSnapshotSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	checkpointPath := filepath.Join(t.TempDir(), "checkpoint.json")
	first := testMover(t, store, items, checkpointPath)
	first.Source = source
	first.Settings.MaxMoves = 8
	if _, err := first.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	first.Checkpoint.Close()
	rest := testMover(t, store, items, checkpointPath)
	report, err := rest.Run(t.Context())
	if err != nil || !report.Finished {
		t.Fatalf("%+v %v", report, err)
	}
	requireAllMoved(t, store, evidence)
	if counts := rest.Checkpoint.State.Counts; counts.Moved != 20 || counts.Items != 20 {
		t.Fatalf("checkpoint counts %+v: an object was read twice or skipped", counts)
	}
}

// A Prometheus gate opens only on one numeric value at most its bound; a
// larger value, no series, several, NaN or a failed query keeps it closed.
func TestPromGate(t *testing.T) {
	answers := map[string]string{
		"low":     `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"0.01"]}]}}`,
		"high":    `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"0.5"]}]}}`,
		"none":    `{"status":"success","data":{"resultType":"vector","result":[]}}`,
		"several": `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"0.01"]},{"metric":{},"value":[1,"0.01"]}]}}`,
		"nan":     `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"NaN"]}]}}`,
		"error":   `{"status":"error","error":"bad query"}`,
	}
	prometheus := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/prometheus/api/v1/query" {
			http.NotFound(writer, request)
			return
		}
		writer.Write([]byte(answers[request.URL.Query().Get("query")]))
	}))
	defer prometheus.Close()
	for query, open := range map[string]bool{"low": true, "high": false, "none": false, "several": false, "nan": false, "error": false} {
		gate := &PromGate{Label: query, Url: prometheus.URL + "/prometheus", Query: query, Max: 0.03}
		if state := gate.Check(t.Context()); state.Open != open {
			t.Fatalf("%s: open %v (%s), want %v", query, state.Open, state.Reason, open)
		}
	}
	gate := &PromGate{Label: "down", Url: "http://127.0.0.1:1/prometheus", Query: "low", Max: 1}
	if state := gate.Check(t.Context()); state.Open {
		t.Fatal("an unreachable Prometheus opened its gate")
	}
	if gates := MainHealthGates(DefaultPromUrl); len(gates) != 5 {
		t.Fatalf("%d main health gates", len(gates))
	}
}

// The export copies every row in key order from a read-only process, resumes
// a file a crash cut short, and a sample of its records matches the table;
// a tampered record is found.
func TestSnapshotExportMatchesTheTable(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		domain := sha256.Sum256([]byte("evidencemove-snapshot-test"))
		want := []SnapshotRecord{}
		for client := range 40 {
			clientId := server.NewId()
			for generation := int64(1); generation <= int64(1+client%3); generation += 1 {
				registration := sha256.Sum256(fmt.Appendf(nil, "%s/%d", clientId, generation))
				evidence := fmt.Appendf(nil, `{"client":%d,"generation":%d,"padding":"%s"}`, client, generation, strings.Repeat("y", client))
				server.MaintenanceTx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(
						ctx,
						`INSERT INTO st_client_key_history (client_id, generation, domain_hash, registration_hash, evidence_hash, evidence)
						VALUES ($1, $2, $3, $4, $5, $6)`,
						clientId, generation, domain[:], registration[:], "sha256:"+hex.EncodeToString(registration[:]), evidence,
					))
				})
				want = append(want, SnapshotRecord{ClientId: clientId, Generation: generation, Hash: registration, Size: uint32(len(evidence))})
			}
		}
		slices.SortFunc(want, func(a SnapshotRecord, b SnapshotRecord) int { return compareCursors(a.Cursor(), b.Cursor()) })

		server.RequirePgReadOnly()
		// before the environment drops its database
		defer server.Testing_ReleasePgReadOnly()
		keeper := NewGateKeeper(t.Logf, newTestGate(true), &PgLockWaitersGate{Max: 20})
		keeper.CheckNow(ctx)
		if open, reason := keeper.Open(); !open {
			t.Fatalf("gates closed: %s", reason)
		}
		settings := DefaultSnapshotSettings()
		settings.RowsPerSecond, settings.BatchRows = 100000, 7
		path := filepath.Join(t.TempDir(), "hashes.bin")
		writer, err := OpenSnapshotWriter(path)
		if err != nil {
			t.Fatal(err)
		}
		// a first run stopped after three batches, then a crash's partial record
		stopCtx, stop := context.WithCancel(ctx)
		defer stop()
		batches := 0
		stopping := func(format string, args ...any) {
			t.Logf(format, args...)
			if strings.HasPrefix(format, "snapshot:") {
				batches += 1
				if batches == 3 {
					stop()
				}
			}
		}
		stoppingSettings := settings
		stoppingSettings.ProgressInterval = time.Nanosecond
		stopped, err := ExportSnapshot(stopCtx, writer, keeper, stoppingSettings, stopping)
		if err != nil || stopped.Finished || writer.Rows() != 21 {
			t.Fatalf("stopped export %+v, %v at %d rows", stopped, err, writer.Rows())
		}
		writer.Close()
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		file.Write(bytes.Repeat([]byte{0xff}, 23))
		file.Close()
		writer, err = OpenSnapshotWriter(path)
		if err != nil {
			t.Fatal(err)
		}
		report, err := ExportSnapshot(ctx, writer, keeper, settings, t.Logf)
		if err != nil || !report.Finished {
			t.Fatalf("resumed export %+v, %v", report, err)
		}
		writer.Close()
		data, err := os.ReadFile(path)
		if err != nil || len(data) != len(want)*SnapshotRecordBytes {
			t.Fatalf("snapshot %d bytes, want %d rows: %v", len(data), len(want), err)
		}
		for index, record := range want {
			if got := decodeSnapshotRecord(data[index*SnapshotRecordBytes:]); got != record {
				t.Fatalf("record %d %+v, want %+v", index, got, record)
			}
		}
		check, err := CheckSnapshotSample(ctx, want)
		if err != nil || check.Matched != len(want) || check.Differ != 0 || check.Missing != 0 {
			t.Fatalf("sample check %+v, %v", check, err)
		}
		tampered := slices.Clone(want[:3])
		tampered[1].Hash[0] ^= 0xff
		tampered[2].Generation += 100
		check, err = CheckSnapshotSample(ctx, tampered)
		if err != nil || check.Matched != 1 || check.Differ != 1 || check.Missing != 1 {
			t.Fatalf("tampered sample check %+v, %v", check, err)
		}
		sample, err := SampleSnapshot(path, 50, 9)
		if err != nil || len(sample) != 50 {
			t.Fatalf("sampled %d, %v", len(sample), err)
		}
		// the process really is read only
		writeRefused := false
		func() {
			defer func() {
				writeRefused = recover() != nil
			}()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM st_client_key_head`))
			}, server.OptNoRetry())
		}()
		if !writeRefused {
			t.Fatal("the export's process accepted a write")
		}
	})
}
