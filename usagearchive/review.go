package usagearchive

// The attribution review export: every contract a set of completed payments
// paid, with every row the contract horizon reaper deletes with it, copied off
// the primary before those payments' attribution review flag is cleared, so
// the reaper can delete the contracts (owner decision 2026-11-17: export them
// to the backup host's archive, then clear the flag).
//
// The reaper keeps a contract for good while a sweep of it was paid by a
// payment that completed with attribution_review_required set, the evidence
// of that review (model/contract_horizon_reaper.go, payment_review), and no
// code clears the flag. This export is that evidence's copy.
//
// # The copy
//
// The format is the week export's: each table's rows of a batch are one
// PostgreSQL COPY in text format, rows in primary key order, compressed with
// zstd, and read back from storage and matched before the batch's manifest is
// written. Each line's row digest is the week export's (LineDigest), summed
// over the table's rows of the batch.
//
// # Batches
//
// A contract is tied when one of its sweeps was paid by one of the run's
// payments. A batch is the tied contracts with after < contract_id <= upper,
// taken in contract_id order from the sweep index (payment_id, contract_id), at
// most a batch size of them. Its rows are read in one short REPEATABLE READ,
// READ ONLY transaction: the tables' figures (row count and row digest) and
// their COPY streams, which must agree line for line. No snapshot outlives a
// batch.
//
// The batch's manifest is written last, once every copy of the batch was read
// back and matched; a batch without its manifest did not happen. The next
// batch starts after the last manifest's upper bound, so batches tile the id
// space from the zero id and every tied contract lands in exactly one of them.
// A run resumes after its last manifest and first removes the files of the
// batch it was writing.
//
// # The clear
//
// The payments' flag is cleared in one small transaction on exactly those
// rows, guarded by their values when the export started, once a verification
// of every stored copy and of the coverage of every tied contract has passed
// and the coverage still holds (review_clear.go). The rows as they were are
// written to the export directory first, so the change can be reversed
// (RestoreReviewFlag).

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// The formats this package writes and reads for the review export.
const (
	ReviewRunFormat          = "attribution_review_export/v1"
	ReviewBatchFormat        = "attribution_review_export_batch/v1"
	ReviewVerificationFormat = "attribution_review_export_verification/v1"
	ReviewClearFormat        = "attribution_review_clear/v1"
)

// The journal events (audit_account_payment) of the clear and of its reversal.
const (
	ReviewClearedEventType  = "attribution_review_cleared"
	ReviewRestoredEventType = "attribution_review_restored"
)

// The tables a contract's delete takes with it: the contract horizon reaper's
// (model contractHorizonTables), contract first. Each has contract_id and its
// primary key leads with it.
var ReviewTables = []string{
	"transfer_contract",
	"transfer_escrow",
	"transfer_escrow_sweep",
	"contract_close",
	"contract_close_report",
	"contract_extender",
}

// The id every contract id follows.
const zeroContractId = "00000000-0000-0000-0000-000000000000"

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// File names in the run's directory.
const (
	reviewRunFile          = "review.json"
	reviewBatchPrefix      = "batch-"
	reviewManifestSuffix   = ".manifest.json"
	reviewVerificationGlob = "verify-*.json"
)

var reviewBatchFilePattern = regexp.MustCompile(`^batch-([0-9]{6,})\.`)

// Bounds and pace of the review export, its verification and its clear.
type ReviewSettings struct {
	// Tied contracts in the first batch of a run, and in any batch at most.
	BatchContracts int
	// The adaptive batch size never goes below this.
	MinBatchContracts int
	// A batch slower than this halves the next one; one faster than a quarter
	// of it grows the next by half, up to BatchContracts.
	TargetBatchDuration time.Duration
	// Tied contracts exported per second at most, paced between batches.
	// Non-positive is unpaced.
	MaxContractsPerSecond float64
	// No batch starts once a run has used this long. Non-positive is no bound.
	MaxDuration time.Duration
	// Batches one run commits at most. Non-positive is no bound.
	MaxBatches int
	// A batch whose statement timed out is retried this many times, each at
	// half the size.
	TimeoutRetries int
	// Bounds each statement of a batch, of the verification and of the plan.
	StatementTimeout time.Duration
	// Bounds each lock wait: a read never queues long behind DDL.
	LockTimeout time.Duration
	// effective_io_concurrency of a batch's bitmap heap reads (the contract
	// horizon reaper's).
	IoConcurrency int
	// No batch starts while the directory's filesystem has less than this
	// free.
	MinFreeBytes uint64
	// zstd's level and threads.
	Level       zstd.EncoderLevel
	Concurrency int
	// The newest verification the clear accepts is at most this old.
	MaxVerificationAge time.Duration
	// Tied contracts the plan reads rows of, as a sample of one batch.
	SampleContracts int
	// now; nil is time.Now
	Clock func() time.Time
	// Nil in production. Tests stop an export at an exact point: after the
	// named table's copy of the batch is stored, before the batch's manifest.
	AfterTableStored func(batch int64, table string) error
	// Nil in production. Tests change a payment between the clear's checks and
	// its update.
	BeforeClearUpdate func() error
}

func DefaultReviewSettings() *ReviewSettings {
	return &ReviewSettings{
		// about 50,000 rows of the six tables, a few seconds of reads on Main
		BatchContracts:      10000,
		MinBatchContracts:   500,
		TargetBatchDuration: 10 * time.Second,
		// about 14,000 heap pages a second on Main (3.4 a contract, measured
		// 2026-11-21), a third of the rate one batch reads at
		MaxContractsPerSecond: 4000,
		TimeoutRetries:        3,
		StatementTimeout:      60 * time.Second,
		LockTimeout:           5 * time.Second,
		IoConcurrency:         32,
		MinFreeBytes:          50 * 1000 * 1000 * 1000,
		Level:                 zstd.SpeedBetterCompression,
		Concurrency:           2,
		MaxVerificationAge:    24 * time.Hour,
		SampleContracts:       10000,
	}
}

func (self *ReviewSettings) now() time.Time {
	if self.Clock != nil {
		return self.Clock().UTC()
	}
	return time.Now().UTC()
}

// A column of an exported table, as the catalog names its type.
type ReviewColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// An exported table: its stored (not generated) columns in table order, which
// is the order of each COPY line, and its primary key.
type ReviewTable struct {
	Table   string         `json:"table"`
	Columns []ReviewColumn `json:"columns"`
	Key     []string       `json:"key"`
}

// A payment of the run, as to_jsonb printed its account_payment row.
type ReviewPayment struct {
	PaymentId string `json:"payment_id"`
	Row       string `json:"row"`
}

// The run: its payments as they were when it started, and the tables' shapes
// every batch is read with.
type ReviewRun struct {
	Format     string          `json:"format"`
	RunId      string          `json:"run_id"`
	CreateTime string          `json:"create_time"`
	ExportHost string          `json:"export_host"`
	Payments   []ReviewPayment `json:"payments"`
	Tables     []ReviewTable   `json:"tables"`
}

func (self *ReviewRun) PaymentIds() []string {
	paymentIds := make([]string, 0, len(self.Payments))
	for _, payment := range self.Payments {
		paymentIds = append(paymentIds, payment.PaymentId)
	}
	return paymentIds
}

// One table's stored copy of a batch.
type ReviewTableCopy struct {
	Table      string `json:"table"`
	Rows       int64  `json:"rows"`
	RowDigest  string `json:"row_digest"`
	CopySha256 string `json:"copy_sha256"`
	CopyBytes  int64  `json:"copy_bytes"`
	File       string `json:"file"`
	FileSha256 string `json:"file_sha256"`
	FileBytes  int64  `json:"file_bytes"`
}

// A batch's manifest.
type ReviewBatch struct {
	Format string `json:"format"`
	RunId  string `json:"run_id"`
	// 1-based
	Batch int64 `json:"batch"`
	// the batch's tied contracts are after < contract_id <= upper
	AfterContractId string `json:"after_contract_id"`
	UpperContractId string `json:"upper_contract_id"`
	Contracts       int64  `json:"contracts"`
	// the row digest of the contract ids' text
	ContractIdDigest string `json:"contract_id_digest"`
	// tied contracts by payment; a contract two payments paid counts in each
	PaymentContracts map[string]int64 `json:"payment_contracts"`
	// tied contracts without a transfer_contract row: their sweeps are copied
	MissingContractIds []string          `json:"missing_contract_ids,omitempty"`
	Tables             []ReviewTableCopy `json:"tables"`
	// the batch transaction's clock (UTC, PostgreSQL's text) and host
	SnapshotTime string `json:"snapshot_time"`
	ExportHost   string `json:"export_host"`
	// when every copy was read back from storage and matched
	VerifiedTime string `json:"verified_time"`
}

// A verification of the run's stored copies and of the coverage of its tied
// contracts on Main.
type ReviewVerification struct {
	Format     string `json:"format"`
	RunId      string `json:"run_id"`
	VerifyTime string `json:"verify_time"`
	Host       string `json:"host"`
	Batches    int64  `json:"batches"`
	// the SHA-256 of the batch manifests' SHA-256s, in batch order
	ManifestsSha256  string           `json:"manifests_sha256"`
	Contracts        int64            `json:"contracts"`
	PaymentContracts map[string]int64 `json:"payment_contracts"`
	TableRows        map[string]int64 `json:"table_rows"`
	// every stored copy was read back and matched its manifest
	FilesChecked bool `json:"files_checked"`
	// every batch's tied contracts were counted again on Main and matched,
	// and no tied contract follows the last batch
	MainChecked bool `json:"main_checked"`
	// every batch's rows were read again on Main and matched
	MainRowsChecked bool     `json:"main_rows_checked"`
	Failures        []string `json:"failures"`
	Ok              bool     `json:"ok"`
}

// The record a clear (or its reversal) writes before and after its change.
type ReviewClearRecord struct {
	Format string `json:"format"`
	// before or after
	Stage              string          `json:"stage"`
	Change             string          `json:"change"`
	RunId              string          `json:"run_id"`
	Dir                string          `json:"dir"`
	Host               string          `json:"host"`
	Time               string          `json:"time"`
	PlanDigest         string          `json:"plan_digest"`
	ManifestsSha256    string          `json:"manifests_sha256"`
	VerificationFile   string          `json:"verification_file,omitempty"`
	VerificationSha256 string          `json:"verification_sha256,omitempty"`
	Contracts          int64           `json:"contracts"`
	Payments           []ReviewPayment `json:"payments"`
	EventIds           []string        `json:"event_ids,omitempty"`
}

// Payment ids: lowercase uuids, at least one, none twice, in id order.
func ParseReviewPaymentIds(value string) ([]string, error) {
	paymentIds := []string{}
	for _, part := range strings.Split(value, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if !uuidPattern.MatchString(part) {
			return nil, fmt.Errorf("payment id %q is not a uuid", part)
		}
		if slices.Contains(paymentIds, part) {
			return nil, fmt.Errorf("payment %s is named twice", part)
		}
		paymentIds = append(paymentIds, part)
	}
	sort.Strings(paymentIds)
	return paymentIds, nil
}

// The contract id floor of t: its Unix milliseconds in the first six bytes, as
// server.ContractIdFloor makes it.
func ContractIdFloor(t time.Time) string {
	var id [16]byte
	milliseconds := uint64(t.UTC().UnixMilli())
	var buffer [8]byte
	binary.BigEndian.PutUint64(buffer[:], milliseconds)
	copy(id[:6], buffer[2:])
	return formatUuid(id)
}

// The mint time of a contract id: its first six bytes, Unix milliseconds.
func ContractIdTime(contractId string) (time.Time, error) {
	if !uuidPattern.MatchString(contractId) {
		return time.Time{}, fmt.Errorf("%q is not a contract id", contractId)
	}
	milliseconds, err := strconv.ParseUint(strings.ReplaceAll(contractId, "-", "")[:12], 16, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.UnixMilli(int64(milliseconds)).UTC(), nil
}

// A new id ordered by creation time, as the server's ids are: Unix
// milliseconds in the first six bytes, then random bytes.
func newTimeOrderedId(now time.Time) (string, error) {
	var id [16]byte
	var buffer [8]byte
	binary.BigEndian.PutUint64(buffer[:], uint64(now.UTC().UnixMilli()))
	copy(id[:6], buffer[2:])
	if _, err := rand.Read(id[6:]); err != nil {
		return "", err
	}
	return formatUuid(id), nil
}

func formatUuid(id [16]byte) string {
	text := hex.EncodeToString(id[:])
	return text[:8] + "-" + text[8:12] + "-" + text[12:16] + "-" + text[16:20] + "-" + text[20:]
}

// The row digest of the ids' text, as PostgreSQL sums it over their lines.
func reviewIdDigest(contractIds []string) string {
	var sum digestSum
	for _, contractId := range contractIds {
		sum.Add(LineDigest([]byte(contractId)))
	}
	return sum.String()
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func reviewTableSql(table string) string {
	return "public." + quoteIdent(table)
}

// Types whose output never holds a backslash or one of the control characters
// COPY escapes, so their text is their COPY text.
var reviewCopySafeTypes = map[string]bool{
	"uuid":                        true,
	"bigint":                      true,
	"integer":                     true,
	"smallint":                    true,
	"boolean":                     true,
	"numeric":                     true,
	"double precision":            true,
	"real":                        true,
	"timestamp without time zone": true,
	"timestamp with time zone":    true,
	"date":                        true,
	"oid":                         true,
}

func reviewCopySafeType(columnType string) bool {
	if i := strings.IndexByte(columnType, '('); 0 <= i {
		columnType = columnType[:i]
	}
	return reviewCopySafeTypes[strings.TrimSpace(columnType)]
}

// COPY's text escapes, applied to a text expression: a backslash, then the
// control characters it writes as \b \f \n \r \t \v. COPY writes every other
// byte as it is.
func reviewCopyEscapeSql(text string) string {
	return `replace(replace(replace(replace(replace(replace(replace(` + text +
		`, E'\\', E'\\\\'), chr(8), E'\\b'), chr(12), E'\\f'), chr(10), E'\\n'), chr(13), E'\\r'), chr(9), E'\\t'), chr(11), E'\\v')`
}

// One COPY text line of a row of the table aliased alias. format's %s prints a
// value with its type's output function, as COPY does (a boolean's cast to
// text would print true, not t); a NULL prints as \N.
func ReviewCopyLineSql(table ReviewTable, alias string) string {
	parts := make([]string, 0, len(table.Columns))
	for _, column := range table.Columns {
		value := alias + "." + quoteIdent(column.Name)
		text := "format('%s', " + value + ")"
		if !reviewCopySafeType(column.Type) {
			text = reviewCopyEscapeSql(text)
		}
		parts = append(parts, "CASE WHEN "+value+" IS NULL THEN E'\\\\N' ELSE "+text+" END")
	}
	return strings.Join(parts, " || E'\\t' || ")
}

// The row digest of the rows of alias, as one numeric's text.
func reviewDigestSql(table ReviewTable, alias string) string {
	return `coalesce(sum(('x' || left(md5(` + ReviewCopyLineSql(table, alias) + `), 16))::bit(64)::bigint::numeric), 0)::text`
}

// The batch's tied contract ids, by its bounds, as a CTE named ids. Every value
// is a uuid this package parsed, so the statement stays short: a COPY slower
// than log_min_duration_statement is logged whole.
func reviewIdsCteSql(paymentIds []string, after string, upper string) string {
	literals := make([]string, 0, len(paymentIds))
	for _, paymentId := range paymentIds {
		literals = append(literals, "'"+paymentId+"'::uuid")
	}
	return `WITH ids AS MATERIALIZED (
		SELECT DISTINCT contract_id FROM transfer_escrow_sweep
		WHERE payment_id IN (` + strings.Join(literals, ", ") + `)
			AND '` + after + `'::uuid < contract_id AND contract_id <= '` + upper + `'::uuid
	) `
}

// The table's rows of the batch's contracts.
func reviewRowsWhereSql(table ReviewTable) string {
	return `FROM ` + reviewTableSql(table.Table) + ` AS t WHERE t.contract_id = ANY(ARRAY(SELECT contract_id FROM ids))`
}

// The key order of the table's COPY: text columns in byte order, as the
// stream's key check compares them.
func reviewOrderSql(table ReviewTable) string {
	parts := make([]string, 0, len(table.Key))
	for _, key := range table.Key {
		part := "t." + quoteIdent(key)
		if !reviewCopySafeType(table.columnType(key)) {
			part += ` COLLATE "C"`
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

func (self ReviewTable) columnType(name string) string {
	for _, column := range self.Columns {
		if column.Name == name {
			return column.Type
		}
	}
	return ""
}

func (self ReviewTable) columnIndex(name string) int {
	for i, column := range self.Columns {
		if column.Name == name {
			return i
		}
	}
	return -1
}

// The statement that reads a table's figures for the batch: rows and row
// digest.
func reviewStatsSql(table ReviewTable, idsCte string) string {
	return idsCte + `SELECT count(*), ` + reviewDigestSql(table, "t") + ` ` + reviewRowsWhereSql(table)
}

// The COPY of a table's rows for the batch, in key order.
func reviewCopyOutSql(table ReviewTable, idsCte string) string {
	columns := make([]string, 0, len(table.Columns))
	for _, column := range table.Columns {
		columns = append(columns, "t."+quoteIdent(column.Name))
	}
	return `COPY (` + idsCte + `SELECT ` + strings.Join(columns, ", ") + ` ` + reviewRowsWhereSql(table) + ` ORDER BY ` + reviewOrderSql(table) + `) TO STDOUT`
}

// The COPY that loads a table's stored copy into a table of its shape.
func ReviewCopyInSql(table ReviewTable, target string) string {
	columns := make([]string, 0, len(table.Columns))
	for _, column := range table.Columns {
		columns = append(columns, quoteIdent(column.Name))
	}
	return fmt.Sprintf(`COPY %s (%s) FROM STDIN`, target, strings.Join(columns, ", "))
}

// One COPY text field without its escapes, for key comparisons.
func unescapeCopyField(field []byte) []byte {
	if bytes.IndexByte(field, '\\') < 0 {
		return field
	}
	out := make([]byte, 0, len(field))
	for i := 0; i < len(field); i++ {
		c := field[i]
		if c != '\\' || i+1 == len(field) {
			out = append(out, c)
			continue
		}
		i++
		switch field[i] {
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'v':
			out = append(out, '\v')
		default:
			out = append(out, field[i])
		}
	}
	return out
}

// What one table's COPY stream of a batch holds, read line by line as it
// streams: the line count and row digest, each line's field count, its
// contract id inside the batch (and, while ids is set, one of the batch's
// contracts), and keys that strictly increase, so no row repeats.
type reviewFigures struct {
	columns    int
	idIndex    int
	keyIndexes []int
	after      string
	upper      string
	// the batch's contract ids; nil checks the bounds only
	ids map[string]bool
	// contract ids seen; nil keeps none
	seenIds map[string]bool
	Rows    int64
	digest  digestSum
	pending []byte
	lastKey [][]byte
	err     error
}

func newReviewFigures(table ReviewTable, after string, upper string) (*reviewFigures, error) {
	figures := &reviewFigures{
		columns: len(table.Columns),
		idIndex: table.columnIndex("contract_id"),
		after:   after,
		upper:   upper,
	}
	if figures.idIndex < 0 {
		return nil, fmt.Errorf("%s has no contract_id column", table.Table)
	}
	for _, key := range table.Key {
		index := table.columnIndex(key)
		if index < 0 {
			return nil, fmt.Errorf("%s's key column %s is not a column", table.Table, key)
		}
		figures.keyIndexes = append(figures.keyIndexes, index)
	}
	return figures, nil
}

func (self *reviewFigures) Digest() string {
	return self.digest.String()
}

func (self *reviewFigures) line(line []byte) {
	if self.err != nil {
		return
	}
	fields := bytes.Split(line, []byte{'\t'})
	if len(fields) != self.columns {
		self.err = fmt.Errorf("line %d has %d fields, not %d", self.Rows+1, len(fields), self.columns)
		return
	}
	contractId := string(fields[self.idIndex])
	if !uuidPattern.MatchString(contractId) {
		self.err = fmt.Errorf("line %d has no contract id", self.Rows+1)
		return
	}
	if !(self.after < contractId && contractId <= self.upper) {
		self.err = fmt.Errorf("line %d: contract %s is outside the batch (%s, %s]", self.Rows+1, contractId, self.after, self.upper)
		return
	}
	if self.ids != nil && !self.ids[contractId] {
		self.err = fmt.Errorf("line %d: contract %s is not one of the batch's", self.Rows+1, contractId)
		return
	}
	if self.seenIds != nil {
		self.seenIds[contractId] = true
	}
	key := make([][]byte, len(self.keyIndexes))
	for i, index := range self.keyIndexes {
		key[i] = append([]byte(nil), unescapeCopyField(fields[index])...)
	}
	if self.lastKey != nil && compareKeys(self.lastKey, key) >= 0 {
		self.err = fmt.Errorf("line %d: its key does not follow the key before it", self.Rows+1)
		return
	}
	self.lastKey = key
	self.digest.Add(LineDigest(line))
	self.Rows++
}

func compareKeys(a [][]byte, b [][]byte) int {
	for i := range a {
		if c := bytes.Compare(a[i], b[i]); c != 0 {
			return c
		}
	}
	return 0
}

// Write splits the stream into lines. It never fails; Close reports a
// malformed stream.
func (self *reviewFigures) Write(p []byte) (int, error) {
	n := len(p)
	for 0 < len(p) {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			self.pending = append(self.pending, p...)
			break
		}
		if 0 < len(self.pending) {
			self.pending = append(self.pending, p[:i]...)
			self.line(self.pending)
			self.pending = self.pending[:0]
		} else {
			self.line(p[:i])
		}
		p = p[i+1:]
	}
	return n, nil
}

func (self *reviewFigures) Close() error {
	if self.err != nil {
		return self.err
	}
	if 0 < len(self.pending) {
		return fmt.Errorf("the stream ends inside line %d", self.Rows+1)
	}
	return nil
}

// The run file, the batch manifests and the verification records are written
// as the week export's manifests are: a partial file, synced, renamed into
// place, and the directory synced.
func writeReviewJson(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	partial := path + partialSuffix
	file, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(partial)
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(partial)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(partial)
		return err
	}
	if err := os.Rename(partial, path); err != nil {
		os.Remove(partial)
		return err
	}
	return syncDir(filepath.Dir(path))
}

func readReviewJson(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func fileSha256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// The run in dir, or os.ErrNotExist.
func ReadReviewRun(dir string) (*ReviewRun, error) {
	run := &ReviewRun{}
	if err := readReviewJson(filepath.Join(dir, reviewRunFile), run); err != nil {
		return nil, err
	}
	if run.Format != ReviewRunFormat {
		return nil, fmt.Errorf("%s: format %q is not %q", reviewRunFile, run.Format, ReviewRunFormat)
	}
	if len(run.Payments) == 0 || len(run.Tables) != len(ReviewTables) {
		return nil, fmt.Errorf("%s names %d payments and %d tables", reviewRunFile, len(run.Payments), len(run.Tables))
	}
	for i, table := range run.Tables {
		if table.Table != ReviewTables[i] {
			return nil, fmt.Errorf("%s: table %d is %s, not %s", reviewRunFile, i, table.Table, ReviewTables[i])
		}
	}
	return run, nil
}

func reviewManifestName(batch int64) string {
	return fmt.Sprintf("%s%06d%s", reviewBatchPrefix, batch, reviewManifestSuffix)
}

func reviewCopyName(batch int64, table string) string {
	return fmt.Sprintf("%s%06d.%s%s", reviewBatchPrefix, batch, table, copySuffix)
}

// The run's batches, in order, and the files of the batch after the last one,
// which a run was writing when it stopped. Refuses batches that do not tile the
// id space from the zero id, a manifest of another run or format, and any other
// file of a batch that its manifest does not name.
func reviewBatchState(dir string, run *ReviewRun) (batches []*ReviewBatch, unfinished []string, returnErr error) {
	paths, err := filepath.Glob(filepath.Join(dir, reviewBatchPrefix+"*"+reviewManifestSuffix))
	if err != nil {
		return nil, nil, err
	}
	for _, path := range paths {
		batch := &ReviewBatch{}
		if err := readReviewJson(path, batch); err != nil {
			return nil, nil, err
		}
		if batch.Format != ReviewBatchFormat || batch.RunId != run.RunId {
			return nil, nil, fmt.Errorf("%s is not a batch of run %s", path, run.RunId)
		}
		if filepath.Base(path) != reviewManifestName(batch.Batch) {
			return nil, nil, fmt.Errorf("%s holds batch %d", path, batch.Batch)
		}
		batches = append(batches, batch)
	}
	sort.Slice(batches, func(i, j int) bool { return batches[i].Batch < batches[j].Batch })
	named := map[string]bool{}
	after := zeroContractId
	for i, batch := range batches {
		if batch.Batch != int64(i+1) {
			return nil, nil, fmt.Errorf("batch %d is missing", i+1)
		}
		if batch.AfterContractId != after {
			return nil, nil, fmt.Errorf("batch %d starts after %s, not after %s", batch.Batch, batch.AfterContractId, after)
		}
		if !(batch.AfterContractId < batch.UpperContractId) || batch.Contracts <= 0 || batch.VerifiedTime == "" {
			return nil, nil, fmt.Errorf("batch %d is not a verified batch of contracts", batch.Batch)
		}
		if len(batch.Tables) != len(run.Tables) {
			return nil, nil, fmt.Errorf("batch %d holds %d tables, not %d", batch.Batch, len(batch.Tables), len(run.Tables))
		}
		for j, copied := range batch.Tables {
			if copied.Table != run.Tables[j].Table || copied.File != reviewCopyName(batch.Batch, copied.Table) {
				return nil, nil, fmt.Errorf("batch %d names %s as its %s copy", batch.Batch, copied.File, run.Tables[j].Table)
			}
			named[copied.File] = true
		}
		named[reviewManifestName(batch.Batch)] = true
		after = batch.UpperContractId
	}
	next := int64(len(batches) + 1)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, partialSuffix) {
			unfinished = append(unfinished, filepath.Join(dir, name))
			continue
		}
		match := reviewBatchFilePattern.FindStringSubmatch(name)
		if match == nil || named[name] {
			continue
		}
		index, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil || index != next {
			return nil, nil, fmt.Errorf("%s belongs to no batch of the run", name)
		}
		unfinished = append(unfinished, filepath.Join(dir, name))
	}
	return batches, unfinished, nil
}

// The SHA-256 of the manifests' SHA-256s, in batch order.
func reviewManifestsSha256(dir string, batches []*ReviewBatch) (string, error) {
	hash := sha256.New()
	for _, batch := range batches {
		sum, err := fileSha256(filepath.Join(dir, reviewManifestName(batch.Batch)))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(hash, "%s %s\n", sum, reviewManifestName(batch.Batch))
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// Reads a stored copy back from storage, past the page cache where the
// platform allows, and matches it against its manifest entry: the file's size
// and SHA-256, the decompressed stream's size and SHA-256, and its lines'
// count, digest, fields, contract ids and key order.
func verifyReviewCopy(dir string, table ReviewTable, batch *ReviewBatch, copied ReviewTableCopy) error {
	file, err := os.Open(filepath.Join(dir, copied.File))
	if err != nil {
		return err
	}
	defer file.Close()
	dropCachedPages(file)
	fileHash := sha256.New()
	fileCount := &countingWriter{}
	// every byte read from the file passes the file's hash once, under the
	// buffer the decoder reads from
	stored := bufio.NewReaderSize(io.TeeReader(file, io.MultiWriter(fileHash, fileCount)), 1024*1024)
	decoder, err := zstd.NewReader(stored, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return err
	}
	defer decoder.Close()
	copyHash := sha256.New()
	copyCount := &countingWriter{}
	figures, err := newReviewFigures(table, batch.AfterContractId, batch.UpperContractId)
	if err != nil {
		return err
	}
	if _, err := io.Copy(io.MultiWriter(copyHash, copyCount, figures), decoder); err != nil {
		return fmt.Errorf("%s: %w", copied.File, err)
	}
	// whatever follows the last frame still counts in the file's hash
	if _, err := io.Copy(io.Discard, stored); err != nil {
		return err
	}
	if err := figures.Close(); err != nil {
		return fmt.Errorf("%s: %w", copied.File, err)
	}
	switch {
	case hex.EncodeToString(fileHash.Sum(nil)) != copied.FileSha256 || fileCount.n != copied.FileBytes:
		return fmt.Errorf("%s is %d bytes with SHA-256 %s, not %d bytes with %s", copied.File, fileCount.n, hex.EncodeToString(fileHash.Sum(nil)), copied.FileBytes, copied.FileSha256)
	case hex.EncodeToString(copyHash.Sum(nil)) != copied.CopySha256 || copyCount.n != copied.CopyBytes:
		return fmt.Errorf("%s's COPY stream is %d bytes with SHA-256 %s, not %d bytes with %s", copied.File, copyCount.n, hex.EncodeToString(copyHash.Sum(nil)), copied.CopyBytes, copied.CopySha256)
	case figures.Rows != copied.Rows || figures.Digest() != copied.RowDigest:
		return fmt.Errorf("%s holds %d rows with digest %s, its manifest %d with %s", copied.File, figures.Rows, figures.Digest(), copied.Rows, copied.RowDigest)
	}
	return nil
}

var errReviewRefused = errors.New("refused")

// A refusal: the run is not in the state the step needs, and nothing was
// written.
func reviewRefusal(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errReviewRefused, fmt.Sprintf(format, a...))
}

// Whether err is a refusal rather than a failure.
func IsReviewRefusal(err error) bool {
	return errors.Is(err, errReviewRefused)
}

// The refusal of a plan that found blockers.
func ReviewPlanBlocked(blockers []string) error {
	return reviewRefusal("the plan found blockers: %s", strings.Join(blockers, "; "))
}
