package evidencemove

// A snapshot of st_client_key_history's evidence hashes, taken before the
// key-history retention deletes the rows of gone clients: once a row is gone,
// the flat object its hash named has no reference left, and the mover, which
// never lists the flat folder, could not reach it. The snapshot is a file of
// fixed-size records in primary key order, so the mover reads it exactly as
// it reads the live table (SnapshotSource), cursor for cursor.
//
// Record (60 bytes, big endian):
//
//	client_id     16 bytes
//	generation     8 bytes
//	evidence hash 32 bytes (the sha256 of evidence_hash, without "sha256:")
//	evidence size  4 bytes (octet_length(evidence))
//
// A client's creation time is its id's leading 48 bits (Cursor.Time), so it
// is not stored. A manifest beside the file (<path>.manifest.json) records the
// row count, the file's sha256 and how the export ran.

import (
	"bufio"
	"container/heap"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/server/v2026"
)

const (
	SnapshotFormat      = "urnetwork-key-history-evidence-hashes-v1"
	SnapshotRecordBytes = 60
	snapshotManifestExt = ".manifest.json"
)

type SnapshotRecord struct {
	ClientId   server.Id
	Generation int64
	Hash       [32]byte
	Size       uint32
}

func (self SnapshotRecord) Cursor() Cursor {
	return Cursor{ClientId: self.ClientId, Generation: self.Generation}
}

func (self SnapshotRecord) encode(into []byte) {
	copy(into[0:16], self.ClientId[:])
	binary.BigEndian.PutUint64(into[16:24], uint64(self.Generation))
	copy(into[24:56], self.Hash[:])
	binary.BigEndian.PutUint32(into[56:60], self.Size)
}

func decodeSnapshotRecord(from []byte) SnapshotRecord {
	var record SnapshotRecord
	copy(record.ClientId[:], from[0:16])
	record.Generation = int64(binary.BigEndian.Uint64(from[16:24]))
	copy(record.Hash[:], from[24:56])
	record.Size = binary.BigEndian.Uint32(from[56:60])
	return record
}

// The item the mover moves for this record.
func (self SnapshotRecord) Item() Item {
	return Item{Cursor: self.Cursor(), Hash: "sha256:" + hex.EncodeToString(self.Hash[:]), Size: int64(self.Size)}
}

// Appends records to a snapshot file, resuming one an earlier run left: a
// partial trailing record (a crash mid-write) is cut off, and the next record
// must follow the last one in key order.
type SnapshotWriter struct {
	file *os.File
	rows int64
	last *SnapshotRecord
}

func OpenSnapshotWriter(path string) (*SnapshotWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	whole := info.Size() - info.Size()%SnapshotRecordBytes
	if whole != info.Size() {
		if err := file.Truncate(whole); err != nil {
			file.Close()
			return nil, err
		}
	}
	writer := &SnapshotWriter{file: file, rows: whole / SnapshotRecordBytes}
	if 0 < writer.rows {
		buffer := make([]byte, SnapshotRecordBytes)
		if _, err := file.ReadAt(buffer, whole-SnapshotRecordBytes); err != nil {
			file.Close()
			return nil, err
		}
		last := decodeSnapshotRecord(buffer)
		writer.last = &last
	}
	if _, err := file.Seek(whole, io.SeekStart); err != nil {
		file.Close()
		return nil, err
	}
	return writer, nil
}

// The cursor after the last record written: the zero cursor for none.
func (self *SnapshotWriter) Cursor() Cursor {
	if self.last == nil {
		return Cursor{}
	}
	return self.last.Cursor()
}

func (self *SnapshotWriter) Rows() int64 {
	return self.rows
}

// Appends records in key order, each after the last, and syncs.
func (self *SnapshotWriter) Append(records []SnapshotRecord) error {
	buffer := make([]byte, len(records)*SnapshotRecordBytes)
	for index, record := range records {
		if self.last != nil && compareCursors(self.last.Cursor(), record.Cursor()) >= 0 {
			return fmt.Errorf("snapshot record %s does not follow %s", record.Cursor(), self.last.Cursor())
		}
		record.encode(buffer[index*SnapshotRecordBytes:])
		last := record
		self.last = &last
	}
	if _, err := self.file.Write(buffer); err != nil {
		return err
	}
	self.rows += int64(len(records))
	return self.file.Sync()
}

func (self *SnapshotWriter) Close() error {
	return self.file.Close()
}

// The rows on heap pages [first, end), by a TID range scan, which reads just
// those pages in order. A client's rows sit wherever its registrations were
// inserted, so a scan in key order reads the heap at random: 2-3 s for 5,000
// cold rows on Main (2026-11-17). The export reads the heap in page order
// instead and sorts the records afterwards (SortSnapshot).
func readSnapshotPages(ctx context.Context, first int64, end int64, statementTimeout time.Duration) (records []SnapshotRecord, resultErr error) {
	query := fmt.Sprintf(`
		SELECT client_id, generation, decode(substr(evidence_hash, 8), 'hex'), octet_length(evidence)
		FROM st_client_key_history
		WHERE ctid >= '(%d,0)'::tid AND ctid < '(%d,0)'::tid
	`, first, end)
	err := server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", statementTimeout.Milliseconds())))
			result, err := tx.Query(ctx, query)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var record SnapshotRecord
					var hash []byte
					var size int64
					server.Raise(result.Scan(&record.ClientId, &record.Generation, &hash, &size))
					if len(hash) != 32 || size < 0 || 1<<32 <= size {
						server.Raise(fmt.Errorf("row %s/%d has a malformed evidence hash or size", record.ClientId, record.Generation))
					}
					copy(record.Hash[:], hash)
					record.Size = uint32(size)
					records = append(records, record)
				}
			})
		}, server.OptReadOnly())
	})
	if err != nil {
		return nil, raisedError(err)
	}
	return records, nil
}

// The table's heap pages now.
func snapshotTablePages(ctx context.Context) (pages int64, resultErr error) {
	err := server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `
				SELECT pg_relation_size('public.st_client_key_history') / current_setting('block_size')::bigint
			`)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&pages))
				}
			})
		}, server.OptReadOnly())
	})
	if err != nil {
		return 0, raisedError(err)
	}
	return pages, nil
}

type SnapshotSettings struct {
	// heap pages read a second
	PagesPerSecond float64
	// heap pages a statement
	BatchPages int
	// bounds each statement
	StatementTimeout time.Duration
	// a statement slower than this closes the gates for a backoff
	SlowStatement    time.Duration
	ProgressInterval time.Duration
}

// About ten rows a page on Main: 2,000 pages a second is about 20,000 rows
// and 16 MB of sequential heap a second, and all 25.6M pages in 3.6 hours.
func DefaultSnapshotSettings() SnapshotSettings {
	return SnapshotSettings{
		PagesPerSecond:   2000,
		BatchPages:       1000,
		StatementTimeout: 30 * time.Second,
		SlowStatement:    2 * time.Second,
		ProgressInterval: 30 * time.Second,
	}
}

func (self SnapshotSettings) Validate() error {
	switch {
	case !(0 < self.PagesPerSecond && self.PagesPerSecond <= 50000):
		return fmt.Errorf("pages a second must be in (0, 50000], not %v", self.PagesPerSecond)
	case self.BatchPages < 1 || 20000 < self.BatchPages:
		return fmt.Errorf("batch pages must be 1 to 20000, not %d", self.BatchPages)
	case self.StatementTimeout <= 0 || self.SlowStatement <= 0 || self.ProgressInterval <= 0:
		return errors.New("statement bounds and the progress interval must be positive")
	}
	return nil
}

type SnapshotReport struct {
	Rows int64 `json:"rows"`
	// heap pages read, of the table's pages
	Pages          int64          `json:"pages"`
	TablePages     int64          `json:"table_pages"`
	Written        int64          `json:"written"`
	Finished       bool           `json:"finished"`
	Stopped        string         `json:"stopped,omitempty"`
	Elapsed        time.Duration  `json:"elapsed"`
	Paused         time.Duration  `json:"paused"`
	Statement      LatencySummary `json:"statement"`
	SlowStatements int64          `json:"slow_statements"`
}

// The raw export's progress, beside it: the records it holds and the next page.
type snapshotRawState struct {
	NextPage int64     `json:"next_page"`
	Rows     int64     `json:"rows"`
	Started  time.Time `json:"started"`
}

func SnapshotRawPath(outPath string) string {
	return outPath + ".raw"
}

// Removes the raw export and its state, once the sorted snapshot is verified.
func RemoveSnapshotRaw(outPath string) error {
	for _, path := range []string{SnapshotRawPath(outPath), snapshotRawStatePath(outPath)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func snapshotRawStatePath(outPath string) string {
	return SnapshotRawPath(outPath) + ".state.json"
}

func saveSnapshotRawState(path string, state snapshotRawState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// Copies every row's (client_id, generation, evidence hash, evidence size)
// into <out>.raw in heap page order, one short TID range statement per batch
// of pages, at the paced rate while the gates are open; a slow statement or a
// failed one closes the gates for the keeper's backoff. Its progress is
// durable in <out>.raw.state.json, written after each batch's records are
// synced, so a rerun resumes at the next page without a duplicate or a gap.
// Pages the table grows during the export are read before it ends; a row
// inserted into a page already read is not, and every such row is new
// evidence, written sharded. Ends when the table has no unread page, or with
// ctx.
func ExportSnapshotPages(ctx context.Context, outPath string, gates *GateKeeper, settings SnapshotSettings, logf func(format string, args ...any)) (*SnapshotReport, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	statePath := snapshotRawStatePath(outPath)
	var state snapshotRawState
	if data, err := os.ReadFile(statePath); err == nil {
		if err := json.Unmarshal(data, &state); err != nil {
			return nil, fmt.Errorf("%s: %w", statePath, err)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		state.Started = time.Now().UTC()
	} else {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o700); err != nil {
		return nil, err
	}
	raw, err := os.OpenFile(SnapshotRawPath(outPath), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer raw.Close()
	// records past the state are an unrecorded batch: it is read again
	if err := raw.Truncate(state.Rows * SnapshotRecordBytes); err != nil {
		return nil, err
	}
	if _, err := raw.Seek(state.Rows*SnapshotRecordBytes, io.SeekStart); err != nil {
		return nil, err
	}
	pacer := NewPacer(settings.PagesPerSecond / float64(settings.BatchPages))
	latency := NewLatencies()
	report := &SnapshotReport{Rows: state.Rows, Pages: state.NextPage}
	start := time.Now()
	pausedAtStart := gates.Paused()
	lastLog, lastRows := start, report.Rows
	finish := func(err error) (*SnapshotReport, error) {
		report.Elapsed = time.Since(start)
		report.Paused = gates.Paused() - pausedAtStart
		report.Statement = latency.Summaries()["statement"]
		return report, err
	}
	pages, err := snapshotTablePages(ctx)
	if err != nil {
		return finish(err)
	}
	report.TablePages = pages
	failures := 0
	buffer := []byte{}
	for {
		if pages <= state.NextPage {
			// the table may have grown since
			if pages, err = snapshotTablePages(ctx); err != nil {
				return finish(err)
			}
			report.TablePages = pages
			if pages <= state.NextPage {
				report.Finished = true
				return finish(nil)
			}
		}
		if err := gates.Wait(ctx); err != nil {
			report.Stopped = "stopped: " + err.Error()
			return finish(nil)
		}
		if err := pacer.Wait(ctx); err != nil {
			report.Stopped = "stopped: " + err.Error()
			return finish(nil)
		}
		if open, _ := gates.Open(); !open {
			continue
		}
		end := min(state.NextPage+int64(settings.BatchPages), pages)
		statementStart := time.Now()
		records, err := readSnapshotPages(ctx, state.NextPage, end, settings.StatementTimeout)
		elapsed := time.Since(statementStart)
		if err != nil {
			if ctx.Err() != nil {
				report.Stopped = "stopped: " + ctx.Err().Error()
				return finish(nil)
			}
			failures += 1
			if 10 <= failures {
				return finish(fmt.Errorf("export at page %d: %d failures in a row: %w", state.NextPage, failures, err))
			}
			logf("export at page %d failed: %v", state.NextPage, err)
			gates.Failed(err, false)
			continue
		}
		failures = 0
		latency.Observe("statement", elapsed)
		if settings.SlowStatement < elapsed {
			report.SlowStatements += 1
			gates.Failed(fmt.Errorf("a %d-page statement took %s", end-state.NextPage, elapsed.Round(time.Millisecond)), false)
		} else {
			gates.Succeeded()
		}
		buffer = buffer[:0]
		for _, record := range records {
			var encoded [SnapshotRecordBytes]byte
			record.encode(encoded[:])
			buffer = append(buffer, encoded[:]...)
		}
		if _, err := raw.Write(buffer); err != nil {
			return finish(fmt.Errorf("snapshot write: %w", err))
		}
		if err := raw.Sync(); err != nil {
			return finish(fmt.Errorf("snapshot sync: %w", err))
		}
		state.NextPage = end
		state.Rows += int64(len(records))
		if err := saveSnapshotRawState(statePath, state); err != nil {
			return finish(fmt.Errorf("snapshot state: %w", err))
		}
		report.Rows, report.Pages = state.Rows, state.NextPage
		report.Written += int64(len(records))
		if now := time.Now(); settings.ProgressInterval <= now.Sub(lastLog) {
			open, reason := gates.Open()
			gate := "open"
			if !open {
				gate = "closed: " + reason
			}
			logf(
				"snapshot: page %d of %d (%.1f%%), %d rows (%.0f/s); %s; gates %s",
				state.NextPage, pages, 100*float64(state.NextPage)/float64(max(1, pages)), state.Rows,
				float64(state.Rows-lastRows)/now.Sub(lastLog).Seconds(), latency.Line(), gate,
			)
			lastLog, lastRows = now, state.Rows
		}
	}
}

type SortReport struct {
	Rows       int64         `json:"rows"`
	Runs       int           `json:"runs"`
	Duplicates int64         `json:"duplicates"`
	Elapsed    time.Duration `json:"elapsed"`
}

// Sorts the raw export's records into key order at outPath, through sorted
// runs of at most chunkRecords records and one merge. A row read twice is
// written once; two different records for one key are an error.
func SortSnapshot(rawPath string, outPath string, chunkRecords int) (*SortReport, error) {
	start := time.Now()
	if chunkRecords < 1 {
		return nil, errors.New("a sort needs a positive chunk")
	}
	raw, err := os.Open(rawPath)
	if err != nil {
		return nil, err
	}
	defer raw.Close()
	info, err := raw.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size()%SnapshotRecordBytes != 0 {
		return nil, fmt.Errorf("%s holds %d bytes, not whole records", rawPath, info.Size())
	}
	report := &SortReport{}
	runPaths := []string{}
	defer func() {
		for _, path := range runPaths {
			os.Remove(path)
		}
	}()
	chunk := make([]byte, chunkRecords*SnapshotRecordBytes)
	for {
		count, err := io.ReadFull(raw, chunk)
		if count == 0 && (err == io.EOF || err == io.ErrUnexpectedEOF) {
			break
		}
		if err != nil && err != io.ErrUnexpectedEOF {
			return nil, err
		}
		records := make([]SnapshotRecord, count/SnapshotRecordBytes)
		for index := range records {
			records[index] = decodeSnapshotRecord(chunk[index*SnapshotRecordBytes:])
		}
		sort.Slice(records, func(a int, b int) bool { return compareCursors(records[a].Cursor(), records[b].Cursor()) < 0 })
		runPath := fmt.Sprintf("%s.run.%d", outPath, len(runPaths))
		runPaths = append(runPaths, runPath)
		if err := writeSnapshotRun(runPath, records); err != nil {
			return nil, err
		}
		if err == io.ErrUnexpectedEOF {
			break
		}
	}
	report.Runs = len(runPaths)
	if err := os.Remove(outPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	writer, err := OpenSnapshotWriter(outPath)
	if err != nil {
		return nil, err
	}
	defer writer.Close()
	if err := mergeSnapshotRuns(runPaths, writer, report); err != nil {
		return nil, err
	}
	report.Rows = writer.Rows()
	report.Elapsed = time.Since(start)
	return report, writer.Close()
}

func writeSnapshotRun(path string, records []SnapshotRecord) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	buffered := bufio.NewWriterSize(file, 4*1024*1024)
	var encoded [SnapshotRecordBytes]byte
	for _, record := range records {
		record.encode(encoded[:])
		if _, err := buffered.Write(encoded[:]); err != nil {
			file.Close()
			return err
		}
	}
	if err := buffered.Flush(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// One run's reader and its current record.
type snapshotRunReader struct {
	reader  *bufio.Reader
	file    *os.File
	current SnapshotRecord
}

func (self *snapshotRunReader) next() (bool, error) {
	var encoded [SnapshotRecordBytes]byte
	if _, err := io.ReadFull(self.reader, encoded[:]); err != nil {
		if err == io.EOF {
			return false, nil
		}
		return false, err
	}
	self.current = decodeSnapshotRecord(encoded[:])
	return true, nil
}

type snapshotRunHeap []*snapshotRunReader

func (self snapshotRunHeap) Len() int { return len(self) }
func (self snapshotRunHeap) Less(a int, b int) bool {
	return compareCursors(self[a].current.Cursor(), self[b].current.Cursor()) < 0
}
func (self snapshotRunHeap) Swap(a int, b int) { self[a], self[b] = self[b], self[a] }
func (self *snapshotRunHeap) Push(value any)   { *self = append(*self, value.(*snapshotRunReader)) }
func (self *snapshotRunHeap) Pop() any {
	old := *self
	last := old[len(old)-1]
	*self = old[:len(old)-1]
	return last
}

func mergeSnapshotRuns(runPaths []string, writer *SnapshotWriter, report *SortReport) error {
	runs := &snapshotRunHeap{}
	for _, path := range runPaths {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		run := &snapshotRunReader{reader: bufio.NewReaderSize(file, 1024*1024), file: file}
		ok, err := run.next()
		if err != nil {
			return err
		}
		if ok {
			*runs = append(*runs, run)
		}
	}
	heap.Init(runs)
	batch := make([]SnapshotRecord, 0, 65536)
	var last *SnapshotRecord
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := writer.Append(batch)
		batch = batch[:0]
		return err
	}
	for 0 < runs.Len() {
		run := (*runs)[0]
		record := run.current
		if last != nil && compareCursors(last.Cursor(), record.Cursor()) == 0 {
			if *last != record {
				return fmt.Errorf("row %s was read twice with different evidence", record.Cursor())
			}
			report.Duplicates += 1
		} else {
			batch = append(batch, record)
			copied := record
			last = &copied
			if len(batch) == cap(batch) {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		ok, err := run.next()
		if err != nil {
			return err
		}
		if ok {
			heap.Fix(runs, 0)
		} else {
			heap.Pop(runs)
		}
	}
	return flush()
}

// Beside the snapshot: what it holds and how it was taken.
type SnapshotManifest struct {
	Format      string         `json:"format"`
	RecordBytes int            `json:"record_bytes"`
	Fields      []string       `json:"fields"`
	Source      string         `json:"source"`
	Rows        int64          `json:"rows"`
	Bytes       int64          `json:"bytes"`
	Sha256      string         `json:"sha256"`
	First       *Cursor        `json:"first,omitempty"`
	Last        *Cursor        `json:"last,omitempty"`
	Finished    time.Time      `json:"finished"`
	Export      SnapshotReport `json:"export"`
	Sort        *SortReport    `json:"sort,omitempty"`
	Sample      *SampleCheck   `json:"sample,omitempty"`
}

func SnapshotManifestPath(path string) string {
	return path + snapshotManifestExt
}

// The file's sha256 and its first and last records, read whole.
func SummarizeSnapshot(path string) (manifest SnapshotManifest, resultErr error) {
	file, err := os.Open(path)
	if err != nil {
		return manifest, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return manifest, err
	}
	if info.Size()%SnapshotRecordBytes != 0 {
		return manifest, fmt.Errorf("snapshot %s holds %d bytes, not whole records", path, info.Size())
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return manifest, err
	}
	manifest = SnapshotManifest{
		Format:      SnapshotFormat,
		RecordBytes: SnapshotRecordBytes,
		Fields:      []string{"client_id uuid 16", "generation int64be 8", "evidence_hash sha256 32", "octet_length(evidence) uint32be 4"},
		Source:      ClientKeyHistorySourceName,
		Rows:        info.Size() / SnapshotRecordBytes,
		Bytes:       info.Size(),
		Sha256:      hex.EncodeToString(hash.Sum(nil)),
	}
	if 0 < manifest.Rows {
		buffer := make([]byte, SnapshotRecordBytes)
		if _, err := file.ReadAt(buffer, 0); err != nil {
			return manifest, err
		}
		first := decodeSnapshotRecord(buffer).Cursor()
		if _, err := file.ReadAt(buffer, info.Size()-SnapshotRecordBytes); err != nil {
			return manifest, err
		}
		last := decodeSnapshotRecord(buffer).Cursor()
		manifest.First, manifest.Last = &first, &last
	}
	return manifest, nil
}

func WriteSnapshotManifest(path string, manifest SnapshotManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	temporary := SnapshotManifestPath(path) + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, SnapshotManifestPath(path))
}

// The mover's source over a snapshot file: the same cursor space as the live
// table, so a checkpoint begun on either continues on the other.
type SnapshotSource struct {
	lock sync.Mutex
	file *os.File
	rows int64
}

func OpenSnapshotSource(path string) (*SnapshotSource, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if info.Size()%SnapshotRecordBytes != 0 {
		file.Close()
		return nil, fmt.Errorf("snapshot %s holds %d bytes, not whole records", path, info.Size())
	}
	return &SnapshotSource{file: file, rows: info.Size() / SnapshotRecordBytes}, nil
}

func (self *SnapshotSource) Rows() int64 {
	return self.rows
}

func (self *SnapshotSource) record(index int64) (SnapshotRecord, error) {
	buffer := make([]byte, SnapshotRecordBytes)
	if _, err := self.file.ReadAt(buffer, index*SnapshotRecordBytes); err != nil {
		return SnapshotRecord{}, err
	}
	return decodeSnapshotRecord(buffer), nil
}

func (self *SnapshotSource) Next(ctx context.Context, after Cursor, limit int) ([]Item, error) {
	if limit < 1 {
		return nil, errors.New("a source read needs a positive limit")
	}
	self.lock.Lock()
	defer self.lock.Unlock()
	var searchErr error
	// the first record after the cursor
	start := int64(sort.Search(int(self.rows), func(index int) bool {
		record, err := self.record(int64(index))
		if err != nil {
			searchErr = err
			return true
		}
		return compareCursors(record.Cursor(), after) > 0
	}))
	if searchErr != nil {
		return nil, searchErr
	}
	count := min(int64(limit), self.rows-start)
	if count <= 0 {
		return nil, nil
	}
	buffer := make([]byte, count*SnapshotRecordBytes)
	if _, err := self.file.ReadAt(buffer, start*SnapshotRecordBytes); err != nil {
		return nil, err
	}
	items := make([]Item, count)
	for index := range items {
		items[index] = decodeSnapshotRecord(buffer[index*SnapshotRecordBytes:]).Item()
	}
	return items, nil
}

func (self *SnapshotSource) Close() error {
	return self.file.Close()
}

// A sample of snapshot records checked against the live table.
type SampleCheck struct {
	Sampled int `json:"sampled"`
	// the row exists and its hash and size are the snapshot's
	Matched int `json:"matched"`
	// the row is gone from the table
	Missing int `json:"missing"`
	// the row's hash or size differs
	Differ   int      `json:"differ"`
	Failures []string `json:"failures,omitempty"`
}

// Reads count records at uniformly random positions.
func SampleSnapshot(path string, count int, seed uint64) ([]SnapshotRecord, error) {
	source, err := OpenSnapshotSource(path)
	if err != nil {
		return nil, err
	}
	defer source.Close()
	random := rand.New(rand.NewPCG(seed, seed^0x5eed))
	records := []SnapshotRecord{}
	for range min(int64(count), source.rows) {
		record, err := source.record(random.Int64N(source.rows))
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

// Looks each sampled record up by primary key, in batches of short
// statements, and compares its hash and size.
func CheckSnapshotSample(ctx context.Context, records []SnapshotRecord) (*SampleCheck, error) {
	check := &SampleCheck{Sampled: len(records)}
	for start := 0; start < len(records); start += 500 {
		batch := records[start:min(len(records), start+500)]
		clientIds := make([]server.Id, len(batch))
		generations := make([]int64, len(batch))
		for index, record := range batch {
			clientIds[index], generations[index] = record.ClientId, record.Generation
		}
		found := map[Cursor]SnapshotRecord{}
		err := server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, "SET LOCAL statement_timeout = 30000"))
				result, err := tx.Query(ctx, `
					SELECT h.client_id, h.generation, decode(substr(h.evidence_hash, 8), 'hex'), octet_length(h.evidence)
					FROM unnest($1::uuid[], $2::bigint[]) AS k(client_id, generation)
					JOIN st_client_key_history h ON h.client_id = k.client_id AND h.generation = k.generation
				`, clientIds, generations)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						var record SnapshotRecord
						var hash []byte
						var size int64
						server.Raise(result.Scan(&record.ClientId, &record.Generation, &hash, &size))
						copy(record.Hash[:], hash)
						record.Size = uint32(size)
						found[record.Cursor()] = record
					}
				})
			}, server.OptReadOnly())
		})
		if err != nil {
			return check, raisedError(err)
		}
		for _, record := range batch {
			live, ok := found[record.Cursor()]
			switch {
			case !ok:
				check.Missing += 1
			case live.Hash != record.Hash || live.Size != record.Size:
				check.Differ += 1
				if len(check.Failures) < 20 {
					check.Failures = append(check.Failures, fmt.Sprintf("%s: snapshot %x/%d, table %x/%d", record.Cursor(), record.Hash, record.Size, live.Hash, live.Size))
				}
			default:
				check.Matched += 1
			}
		}
	}
	return check, nil
}

// Open while a Prometheus (Mimir) instant query answers at most Max; any
// other answer, or none, keeps it closed.
type PromGate struct {
	Label string
	Url   string
	Query string
	Max   float64
	Every time.Duration
	// how a value reads in a reason, such as "%.0f ms" of value*1000
	Format func(value float64) string
}

func (self *PromGate) Name() string { return self.Label }

func (self *PromGate) Interval() time.Duration {
	if self.Every <= 0 {
		return 15 * time.Second
	}
	return self.Every
}

func (self *PromGate) Check(ctx context.Context) GateState {
	value, err := promInstantQuery(ctx, self.Url, self.Query)
	if err != nil {
		return GateState{Reason: fmt.Sprintf("unreadable: %v", err)}
	}
	text := fmt.Sprintf("%g", value)
	if self.Format != nil {
		text = self.Format(value)
	}
	if self.Max < value {
		return GateState{Reason: fmt.Sprintf("%s, more than %s", text, self.formatMax())}
	}
	return GateState{Open: true, Reason: text}
}

func (self *PromGate) formatMax() string {
	if self.Format != nil {
		return self.Format(self.Max)
	}
	return fmt.Sprintf("%g", self.Max)
}

// Open while at most Max PostgreSQL backends wait on a lock.
type PgLockWaitersGate struct {
	Max   int
	Every time.Duration
}

func (self *PgLockWaitersGate) Name() string { return "lock waiters" }

func (self *PgLockWaitersGate) Interval() time.Duration {
	if self.Every <= 0 {
		return 15 * time.Second
	}
	return self.Every
}

func (self *PgLockWaitersGate) Check(ctx context.Context) GateState {
	var waiters int
	err := server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock'`)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&waiters))
				}
			})
		}, server.OptReadOnly(), server.OptNoRetry())
	})
	if err != nil {
		return GateState{Reason: fmt.Sprintf("unreadable: %v", raisedError(err))}
	}
	if self.Max < waiters {
		return GateState{Reason: fmt.Sprintf("%d backends wait on locks, more than %d", waiters, self.Max)}
	}
	return GateState{Open: true, Reason: fmt.Sprintf("%d", waiters)}
}

// The Mimir query front the release gates read through (the 127.0.0.1:3100
// forward to edge-0's Grafana front, see the factury notes).
const DefaultPromUrl = "http://127.0.0.1:3100/prometheus"

// Main's health, read only: the api's mean request latency, edge-2's busy CPU
// and iowait, the taskworker's oldest overdue task (with a fresh snapshot),
// and backends waiting on locks. Baselines on 2026-11-17 ~17:40Z: 10 ms,
// 0.36, 0.04, 0 s and 2.
func MainHealthGates(promUrl string) []Gate {
	millis := func(value float64) string { return fmt.Sprintf("%.0f ms", value*1000) }
	share := func(value float64) string { return fmt.Sprintf("%.2f", value) }
	seconds := func(value float64) string { return fmt.Sprintf("%.0f s", value) }
	edge2 := `host="by-us-fmt-5-edge-2"`
	return []Gate{
		&PromGate{
			Label:  "api latency",
			Url:    promUrl,
			Query:  `sum(rate(urnetwork_http_request_duration_seconds_sum{job="api"}[2m])) / sum(rate(urnetwork_http_request_duration_seconds_count{job="api"}[2m]))`,
			Max:    0.030,
			Format: millis,
		},
		&PromGate{
			Label:  "edge-2 busy",
			Url:    promUrl,
			Query:  `sum(rate(node_cpu_seconds_total{` + edge2 + `,mode=~"user|system|softirq|irq|nice|steal"}[2m])) / count(node_cpu_seconds_total{` + edge2 + `,mode="idle"})`,
			Max:    0.70,
			Format: share,
		},
		&PromGate{
			Label:  "edge-2 iowait",
			Url:    promUrl,
			Query:  `sum(rate(node_cpu_seconds_total{` + edge2 + `,mode="iowait"}[2m])) / count(node_cpu_seconds_total{` + edge2 + `,mode="idle"})`,
			Max:    0.15,
			Format: share,
		},
		&PromGate{
			Label: "taskworker overdue",
			Url:   promUrl,
			// a stale queue snapshot reads as overdue
			Query:  `max(urnetwork_taskworker_queue_oldest_overdue_seconds) + (time() - max(urnetwork_taskworker_queue_snapshot_timestamp_seconds) > bool 300) * 3600`,
			Max:    60,
			Format: seconds,
		},
		&PgLockWaitersGate{Max: 20},
	}
}

// One instant query's single value. No series, several, NaN or a failed query
// is an error.
func promInstantQuery(ctx context.Context, base string, query string) (float64, error) {
	readCtx, cancel := context.WithTimeout(ctx, gateRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(readCtx, http.MethodGet, strings.TrimRight(base, "/")+"/api/v1/query?"+url.Values{"query": {query}}.Encode(), nil)
	if err != nil {
		return 0, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024))
	if err != nil {
		return 0, err
	}
	var answer struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return 0, fmt.Errorf("answer %d is not JSON", response.StatusCode)
	}
	if answer.Status != "success" {
		return 0, fmt.Errorf("query failed: %s", answer.Error)
	}
	if len(answer.Data.Result) != 1 || len(answer.Data.Result[0].Value) != 2 {
		return 0, fmt.Errorf("%d series answered", len(answer.Data.Result))
	}
	text, _ := answer.Data.Result[0].Value[1].(string)
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("value %q is not a number", text)
	}
	return value, nil
}
