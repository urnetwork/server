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

// Shuffled records sort into key order through several runs; a row read
// twice is written once, and two different records for one key fail.
func TestSortSnapshot(t *testing.T) {
	records := testSnapshotRecords(1000, 4)
	shuffled := slices.Clone(records)
	shuffled = append(shuffled, records[10], records[500])
	rand.New(rand.NewPCG(5, 5)).Shuffle(len(shuffled), func(a int, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
	directory := t.TempDir()
	rawPath := filepath.Join(directory, "hashes.bin.raw")
	raw := make([]byte, 0, len(shuffled)*SnapshotRecordBytes)
	for _, record := range shuffled {
		var encoded [SnapshotRecordBytes]byte
		record.encode(encoded[:])
		raw = append(raw, encoded[:]...)
	}
	if err := os.WriteFile(rawPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(directory, "hashes.bin")
	report, err := SortSnapshot(rawPath, outPath, 97)
	if err != nil {
		t.Fatal(err)
	}
	if report.Rows != 1000 || report.Duplicates != 2 || report.Runs != 11 {
		t.Fatalf("sort report %+v", report)
	}
	data, err := os.ReadFile(outPath)
	if err != nil || len(data) != 1000*SnapshotRecordBytes {
		t.Fatalf("sorted %d bytes, %v", len(data), err)
	}
	for index, record := range records {
		if got := decodeSnapshotRecord(data[index*SnapshotRecordBytes:]); got != record {
			t.Fatalf("record %d %+v, want %+v", index, got, record)
		}
	}
	if matches, _ := filepath.Glob(outPath + ".run.*"); len(matches) != 0 {
		t.Fatalf("runs left behind: %v", matches)
	}
	conflict := records[3]
	conflict.Size += 1
	var encoded [SnapshotRecordBytes]byte
	conflict.encode(encoded[:])
	if err := os.WriteFile(rawPath, append(raw, encoded[:]...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SortSnapshot(rawPath, outPath, 97); err == nil || !strings.Contains(err.Error(), "different evidence") {
		t.Fatalf("a conflicting duplicate sorted: %v", err)
	}
}

// The export reads the heap in page order from a read-only process, resumes
// after a stop and a crash's unrecorded tail with no duplicate or gap, sorts
// into key order, and a sample of its records matches the table; a tampered
// record is found.
func TestSnapshotExportMatchesTheTable(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		domain := sha256.Sum256([]byte("evidencemove-snapshot-test"))
		want := []SnapshotRecord{}
		type row struct {
			record   SnapshotRecord
			evidence []byte
		}
		rows := []row{}
		for client := range 60 {
			clientId := server.NewId()
			for generation := int64(1); generation <= int64(1+client%3); generation += 1 {
				registration := sha256.Sum256(fmt.Appendf(nil, "%s/%d", clientId, generation))
				evidence := fmt.Appendf(nil, `{"client":%d,"generation":%d,"padding":"%s"}`, client, generation, strings.Repeat("y", 200+client))
				record := SnapshotRecord{ClientId: clientId, Generation: generation, Hash: registration, Size: uint32(len(evidence))}
				rows = append(rows, row{record: record, evidence: evidence})
				want = append(want, record)
			}
		}
		// insert out of key order, so heap order is not key order
		rand.New(rand.NewPCG(7, 7)).Shuffle(len(rows), func(a int, b int) { rows[a], rows[b] = rows[b], rows[a] })
		for _, row := range rows {
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO st_client_key_history (client_id, generation, domain_hash, registration_hash, evidence_hash, evidence)
					VALUES ($1, $2, $3, $4, $5, $6)`,
					row.record.ClientId, row.record.Generation, domain[:], row.record.Hash[:], "sha256:"+hex.EncodeToString(row.record.Hash[:]), row.evidence,
				))
			})
		}
		slices.SortFunc(want, func(a SnapshotRecord, b SnapshotRecord) int { return compareCursors(a.Cursor(), b.Cursor()) })

		server.RequirePgReadOnly()
		// before the environment drops its database
		defer server.Testing_ReleasePgReadOnly()
		pages, err := snapshotTablePages(ctx)
		if err != nil || pages < 4 {
			t.Fatalf("the fixture fills %d pages, %v", pages, err)
		}
		keeper := NewGateKeeper(t.Logf, newTestGate(true), &PgLockWaitersGate{Max: 20})
		keeper.CheckNow(ctx)
		if open, reason := keeper.Open(); !open {
			t.Fatalf("gates closed: %s", reason)
		}
		settings := DefaultSnapshotSettings()
		settings.PagesPerSecond, settings.BatchPages = 1000, 1
		outPath := filepath.Join(t.TempDir(), "hashes.bin")
		// a first run stopped after two batches
		stopCtx, stop := context.WithCancel(ctx)
		defer stop()
		batches := 0
		stopping := func(format string, args ...any) {
			t.Logf(format, args...)
			if strings.HasPrefix(format, "snapshot:") {
				batches += 1
				if batches == 2 {
					stop()
				}
			}
		}
		stoppingSettings := settings
		stoppingSettings.ProgressInterval = time.Nanosecond
		stopped, err := ExportSnapshotPages(stopCtx, outPath, keeper, stoppingSettings, stopping)
		if err != nil || stopped.Finished || stopped.Pages != 2 {
			t.Fatalf("stopped export %+v, %v", stopped, err)
		}
		// a crash after writing a batch, before recording it
		file, err := os.OpenFile(SnapshotRawPath(outPath), os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		file.Write(bytes.Repeat([]byte{0xff}, 3*SnapshotRecordBytes+7))
		file.Close()
		report, err := ExportSnapshotPages(ctx, outPath, keeper, settings, t.Logf)
		if err != nil || !report.Finished || report.Rows != int64(len(want)) || report.Pages != pages {
			t.Fatalf("resumed export %+v, %v; want %d rows on %d pages", report, err, len(want), pages)
		}
		sorted, err := SortSnapshot(SnapshotRawPath(outPath), outPath, 50)
		if err != nil || sorted.Rows != int64(len(want)) || sorted.Duplicates != 0 {
			t.Fatalf("sort %+v, %v", sorted, err)
		}
		data, err := os.ReadFile(outPath)
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
		sample, err := SampleSnapshot(outPath, 50)
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

// The validator's progress from its health exporter in Mimir: a fresh, current
// sample names the intent's epoch and status; a stale sample, an intent
// observation that is not current, or a missing series is unreadable.
func TestPromProgressReader(t *testing.T) {
	type answers struct {
		age, current, epoch, status string
	}
	var current answers
	prometheus := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query().Get("query")
		value := ""
		switch {
		case strings.Contains(query, "sample_timestamp_seconds"):
			value = current.age
		case strings.Contains(query, "intent_current"):
			value = current.current
		case strings.Contains(query, "intent_native_epoch"):
			value = current.epoch
		case strings.Contains(query, "intent_status"):
			value = current.status
		}
		if !strings.Contains(query, `host="snow"`) || value == "" {
			writer.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
			return
		}
		fmt.Fprintf(writer, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,%q]}]}}`, value)
	}))
	defer prometheus.Close()
	reader := &PromProgressReader{Url: prometheus.URL + "/prometheus"}
	for _, test := range []struct {
		answers answers
		want    *ValidatorProgress
	}{
		{answers{"60", "1", "26405", "2"}, &ValidatorProgress{26405, "finalized"}},
		{answers{"5", "1", "26405", "3"}, &ValidatorProgress{26405, "applied"}},
		{answers{"5", "1", "26406", "1"}, &ValidatorProgress{26406, "pending"}},
		{answers{"5", "1", "26406", "4"}, &ValidatorProgress{26406, "failed"}},
		{answers{"5", "1", "26406", "-1"}, &ValidatorProgress{26406, "unrecognized"}},
		// stale, not current, or missing: unreadable
		{answers{"600", "1", "26405", "2"}, nil},
		{answers{"5", "0", "26405", "2"}, nil},
		{answers{"5", "1", "", "2"}, nil},
		{answers{"", "1", "26405", "2"}, nil},
		{answers{"5", "1", "26405.5", "2"}, nil},
	} {
		current = test.answers
		progress, err := reader.ReadProgress(t.Context())
		if test.want == nil {
			if err == nil {
				t.Fatalf("%+v read %+v", test.answers, progress)
			}
			continue
		}
		if err != nil || progress != *test.want {
			t.Fatalf("%+v read %+v, %v; want %+v", test.answers, progress, err, *test.want)
		}
	}
	// the production gates read it, not ssh
	gates := DefaultGates("http://minio.invalid/minio/health/cluster", NewLatencies(), DefaultPromUrl)
	validator, ok := gates[0].(*ValidatorGate)
	if !ok {
		t.Fatalf("the first gate is %T", gates[0])
	}
	if reader, ok := validator.Progress.(*PromProgressReader); !ok || reader.Url != DefaultPromUrl {
		t.Fatalf("the validator gate reads %T", validator.Progress)
	}
}

// A client id created at the given time, as connect.NewId lays it out.
func testClientIdAt(at time.Time, entropy byte) server.Id {
	var id server.Id
	ms := uint64(at.UnixMilli())
	for index := range 6 {
		id[index] = byte(ms >> (8 * (5 - index)))
	}
	id[15] = entropy
	return id
}

// A run with UntilCreated ends before the first row of a client created at or
// after it, wherever the batch boundary falls, leaves those rows' objects
// alone, and records the cursor of the last row before it.
func TestMoveStopsAtItsCreationBound(t *testing.T) {
	evidence := testEvidenceSet(t, 10)
	store := newMemoryStore()
	start := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	items := make([]Item, len(evidence))
	for index, entry := range evidence {
		store.put(testKeys(t, entry.hash).Flat, entry.bytes)
		items[index] = Item{
			Cursor: Cursor{ClientId: testClientIdAt(start.Add(time.Duration(index)*time.Hour), byte(index)), Generation: 1},
			Hash:   "sha256:" + entry.hash,
			Size:   int64(len(entry.bytes)),
		}
	}
	if items[3].Cursor.Time() != start.Add(3*time.Hour) {
		t.Fatalf("cursor time %s", items[3].Cursor.Time())
	}
	for _, batchRows := range []int{1, 3, 7, 100} {
		t.Run(fmt.Sprintf("batches of %d", batchRows), func(t *testing.T) {
			store := newMemoryStore()
			for _, entry := range evidence {
				store.put(testKeys(t, entry.hash).Flat, entry.bytes)
			}
			mover := testMover(t, store, items, filepath.Join(t.TempDir(), "checkpoint.json"))
			mover.Settings.BatchRows = batchRows
			mover.Settings.UntilCreated = start.Add(5 * time.Hour)
			report, err := mover.Run(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if !report.Finished || report.Counts.Moved != 5 || !strings.Contains(report.Stopped, "2026-11-16T05:00:00Z") {
				t.Fatalf("report %+v", report)
			}
			if mover.Checkpoint.State.Cursor != items[4].Cursor {
				t.Fatalf("checkpoint at %s, want %s", mover.Checkpoint.State.Cursor, items[4].Cursor)
			}
			for index, entry := range evidence {
				_, flat := store.get(testKeys(t, entry.hash).Flat)
				if flat != (5 <= index) {
					t.Fatalf("item %d: flat object kept %v", index, flat)
				}
			}
		})
	}
}
