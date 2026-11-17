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

	"github.com/urnetwork/server"
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

// One batch of the export: at most limit rows after cursor in key order.
const snapshotExportSql = `
	SELECT client_id, generation, decode(substr(evidence_hash, 8), 'hex'), octet_length(evidence)
	FROM st_client_key_history
	WHERE (client_id, generation) > ($1, $2)
	ORDER BY client_id, generation
	LIMIT $3
`

func readSnapshotBatch(ctx context.Context, after Cursor, limit int, statementTimeout time.Duration) (records []SnapshotRecord, resultErr error) {
	err := server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", statementTimeout.Milliseconds())))
			result, err := tx.Query(ctx, snapshotExportSql, after.ClientId, after.Generation, limit)
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

type SnapshotSettings struct {
	// rows read a second
	RowsPerSecond float64
	// rows a statement
	BatchRows int
	// bounds each statement; a slower statement backs the export off
	StatementTimeout time.Duration
	// a statement slower than this closes the gates for a backoff
	SlowStatement    time.Duration
	ProgressInterval time.Duration
}

func DefaultSnapshotSettings() SnapshotSettings {
	return SnapshotSettings{
		RowsPerSecond:    20000,
		BatchRows:        5000,
		StatementTimeout: 30 * time.Second,
		SlowStatement:    2 * time.Second,
		ProgressInterval: 30 * time.Second,
	}
}

func (self SnapshotSettings) Validate() error {
	switch {
	case !(0 < self.RowsPerSecond && self.RowsPerSecond <= 200000):
		return fmt.Errorf("rows a second must be in (0, 200000], not %v", self.RowsPerSecond)
	case self.BatchRows < 1 || 50000 < self.BatchRows:
		return fmt.Errorf("batch rows must be 1 to 50000, not %d", self.BatchRows)
	case self.StatementTimeout <= 0 || self.SlowStatement <= 0 || self.ProgressInterval <= 0:
		return errors.New("statement bounds and the progress interval must be positive")
	}
	return nil
}

type SnapshotReport struct {
	Rows           int64          `json:"rows"`
	Written        int64          `json:"written"`
	Cursor         Cursor         `json:"cursor"`
	Finished       bool           `json:"finished"`
	Stopped        string         `json:"stopped,omitempty"`
	Elapsed        time.Duration  `json:"elapsed"`
	Paused         time.Duration  `json:"paused"`
	Statement      LatencySummary `json:"statement"`
	SlowStatements int64          `json:"slow_statements"`
}

// Copies every row's (client_id, generation, evidence hash, evidence size)
// into writer, after the rows it already holds, in short keyset statements
// at the paced rate while the gates are open. A slow statement or a failed one
// closes the gates for the keeper's backoff. Ends at the table's end or ctx's.
func ExportSnapshot(ctx context.Context, writer *SnapshotWriter, gates *GateKeeper, settings SnapshotSettings, logf func(format string, args ...any)) (*SnapshotReport, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	pacer := NewPacer(settings.RowsPerSecond / float64(settings.BatchRows))
	latency := NewLatencies()
	report := &SnapshotReport{Rows: writer.Rows(), Cursor: writer.Cursor()}
	start := time.Now()
	pausedAtStart := gates.Paused()
	lastLog, lastRows := start, report.Rows
	finish := func(err error) (*SnapshotReport, error) {
		report.Elapsed = time.Since(start)
		report.Paused = gates.Paused() - pausedAtStart
		report.Statement = latency.Summaries()["statement"]
		return report, err
	}
	failures := 0
	for {
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
		statementStart := time.Now()
		records, err := readSnapshotBatch(ctx, report.Cursor, settings.BatchRows, settings.StatementTimeout)
		elapsed := time.Since(statementStart)
		if err != nil {
			if ctx.Err() != nil {
				report.Stopped = "stopped: " + ctx.Err().Error()
				return finish(nil)
			}
			failures += 1
			if 10 <= failures {
				return finish(fmt.Errorf("export after %s: %d failures in a row: %w", report.Cursor, failures, err))
			}
			logf("export after %s failed: %v", report.Cursor, err)
			gates.Failed(err, false)
			continue
		}
		failures = 0
		latency.Observe("statement", elapsed)
		if settings.SlowStatement < elapsed {
			report.SlowStatements += 1
			gates.Failed(fmt.Errorf("a %d-row statement took %s", settings.BatchRows, elapsed.Round(time.Millisecond)), false)
		} else {
			gates.Succeeded()
		}
		if len(records) == 0 {
			report.Finished = true
			return finish(nil)
		}
		if err := writer.Append(records); err != nil {
			return finish(fmt.Errorf("snapshot write: %w", err))
		}
		report.Rows = writer.Rows()
		report.Written += int64(len(records))
		report.Cursor = writer.Cursor()
		if now := time.Now(); settings.ProgressInterval <= now.Sub(lastLog) {
			open, reason := gates.Open()
			gate := "open"
			if !open {
				gate = "closed: " + reason
			}
			logf(
				"snapshot: %d rows (%.0f/s); cursor %s; statement %s; gates %s",
				report.Rows, float64(report.Rows-lastRows)/now.Sub(lastLog).Seconds(), report.Cursor, latency.Line(), gate,
			)
			lastLog, lastRows = now, report.Rows
		}
	}
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
