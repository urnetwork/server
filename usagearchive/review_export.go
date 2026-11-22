package usagearchive

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"
)

// The statements of a connection or of a transaction.
type reviewQuerier interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// The exported tables' stored columns, in table order, and primary keys, as
// the catalog has them now.
func ReadReviewSchema(ctx context.Context, q reviewQuerier) ([]ReviewTable, error) {
	tables := []ReviewTable{}
	for _, name := range ReviewTables {
		table := ReviewTable{Table: name, Columns: []ReviewColumn{}, Key: []string{}}
		rows, err := q.Query(ctx, `
			SELECT a.attname::text, format_type(a.atttypid, a.atttypmod)
			FROM pg_attribute AS a
			WHERE a.attrelid = to_regclass($1) AND 0 < a.attnum AND NOT a.attisdropped AND a.attgenerated = ''
			ORDER BY a.attnum
		`, "public."+name)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var column ReviewColumn
			if err := rows.Scan(&column.Name, &column.Type); err != nil {
				rows.Close()
				return nil, err
			}
			table.Columns = append(table.Columns, column)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		rows, err = q.Query(ctx, `
			SELECT a.attname::text
			FROM pg_index AS i
			CROSS JOIN LATERAL unnest(i.indkey::int2[]) WITH ORDINALITY AS k(attnum, ordinal)
			JOIN pg_attribute AS a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
			WHERE i.indrelid = to_regclass($1) AND i.indisprimary
			ORDER BY k.ordinal
		`, "public."+name)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				rows.Close()
				return nil, err
			}
			table.Key = append(table.Key, key)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if len(table.Columns) == 0 {
			return nil, fmt.Errorf("%s does not exist", name)
		}
		if table.columnIndex("contract_id") < 0 || len(table.Key) == 0 || table.Key[0] != "contract_id" {
			return nil, fmt.Errorf("%s's primary key does not lead with contract_id: %v", name, table.Key)
		}
		tables = append(tables, table)
	}
	return tables, nil
}

// A payment as it is now.
type reviewPaymentState struct {
	ReviewPayment
	Completed bool
	Canceled  bool
	Review    bool
}

// The payments that exist, by payment id.
func readReviewPayments(ctx context.Context, q reviewQuerier, paymentIds []string) (map[string]*reviewPaymentState, error) {
	rows, err := q.Query(ctx, `
		SELECT p.payment_id::text, to_jsonb(p)::text, p.completed, p.canceled, p.attribution_review_required
		FROM account_payment AS p
		WHERE p.payment_id = ANY($1::text[]::uuid[])
		ORDER BY p.payment_id
	`, paymentIds)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	payments := map[string]*reviewPaymentState{}
	for rows.Next() {
		payment := &reviewPaymentState{}
		if err := rows.Scan(&payment.PaymentId, &payment.Row, &payment.Completed, &payment.Canceled, &payment.Review); err != nil {
			return nil, err
		}
		payments[payment.PaymentId] = payment
	}
	return payments, rows.Err()
}

// Why a payment cannot be in a run, or "".
func reviewPaymentBlocker(paymentId string, payment *reviewPaymentState) string {
	switch {
	case payment == nil:
		return fmt.Sprintf("payment %s does not exist", paymentId)
	case !payment.Completed || payment.Canceled:
		return fmt.Sprintf("payment %s is not completed (completed %t, canceled %t)", paymentId, payment.Completed, payment.Canceled)
	case !payment.Review:
		return fmt.Sprintf("payment %s is not under attribution review", paymentId)
	}
	return ""
}

// The run in dir, started now when dir holds none. A new run needs payments
// that completed under attribution review and an empty or absent directory;
// it records the payments' rows and the tables' shapes. An existing run needs
// the same payments (or none named) and the same table shapes as it recorded.
func StartReview(ctx context.Context, conn *pgx.Conn, dir string, paymentIds []string, host string, settings *ReviewSettings) (*ReviewRun, error) {
	tables, err := ReadReviewSchema(ctx, conn)
	if err != nil {
		return nil, err
	}
	run, err := ReadReviewRun(dir)
	if err == nil {
		if len(paymentIds) != 0 && !slices.Equal(paymentIds, run.PaymentIds()) {
			return nil, reviewRefusal("the run in %s exports payments %v, not %v", dir, run.PaymentIds(), paymentIds)
		}
		if !reflect.DeepEqual(tables, run.Tables) {
			return nil, reviewRefusal("the exported tables changed since the run started: %v, the run has %v", tables, run.Tables)
		}
		return run, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(paymentIds) == 0 {
		return nil, reviewRefusal("%s holds no run, and no payment was named", dir)
	}
	payments, err := readReviewPayments(ctx, conn, paymentIds)
	if err != nil {
		return nil, err
	}
	run = &ReviewRun{
		Format:     ReviewRunFormat,
		CreateTime: settings.now().Format(time.RFC3339Nano),
		ExportHost: host,
		Payments:   []ReviewPayment{},
		Tables:     tables,
	}
	for _, paymentId := range paymentIds {
		if blocker := reviewPaymentBlocker(paymentId, payments[paymentId]); blocker != "" {
			return nil, reviewRefusal("%s", blocker)
		}
		run.Payments = append(run.Payments, payments[paymentId].ReviewPayment)
	}
	// a lock file of the run's command is not the files of a run
	if entries, err := os.ReadDir(dir); err == nil {
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), ".") {
				return nil, reviewRefusal("%s holds files but no run", dir)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	if run.RunId, err = newTimeOrderedId(settings.now()); err != nil {
		return nil, err
	}
	if err := writeReviewJson(filepath.Join(dir, reviewRunFile), run); err != nil {
		return nil, err
	}
	return run, nil
}

// What one export run did, and where the run stands.
type ReviewExportReport struct {
	// committed by this run
	Batches   int64
	Contracts int64
	TableRows map[string]int64
	// the run's batches and tied contracts so far
	TotalBatches   int64
	TotalContracts int64
	// the last batch's upper bound
	LastContractId string
	// no tied contract follows the last batch
	Done bool
	// why the run ended: done, max_batches, max_duration or canceled
	Stop string
}

// Exports batches of the run in dir (started now when dir holds none), from
// after its last batch, until no tied contract is left or a bound of settings
// stops it. The files of a batch that a stopped run left without its manifest
// are removed first, and that batch is exported again. A batch whose statement
// timed out, or whose lock wait did, is exported again at half the size, at
// most settings.TimeoutRetries times in a row.
func ExportReview(ctx context.Context, conn *pgx.Conn, dir string, paymentIds []string, host string, settings *ReviewSettings) (report *ReviewExportReport, returnErr error) {
	run, err := StartReview(ctx, conn, dir, paymentIds, host, settings)
	if err != nil {
		return nil, err
	}
	report = &ReviewExportReport{TableRows: map[string]int64{}, LastContractId: zeroContractId}
	batches, err := clearUnfinishedReviewBatch(dir, run)
	if err != nil {
		return report, err
	}
	for _, batch := range batches {
		report.TotalBatches++
		report.TotalContracts += batch.Contracts
		report.LastContractId = batch.UpperContractId
	}
	start := time.Now()
	size := max(settings.MinBatchContracts, settings.BatchContracts)
	retries := 0
	for {
		if ctx.Err() != nil {
			report.Stop = "canceled"
			return report, nil
		}
		if 0 < settings.MaxBatches && int64(settings.MaxBatches) <= report.Batches {
			report.Stop = "max_batches"
			return report, nil
		}
		if 0 < settings.MaxDuration && settings.MaxDuration <= time.Since(start) {
			report.Stop = "max_duration"
			return report, nil
		}
		var stat unix.Statfs_t
		if err := unix.Statfs(dir, &stat); err != nil {
			return report, err
		}
		if free := uint64(stat.Bavail) * uint64(stat.Bsize); free < settings.MinFreeBytes {
			return report, fmt.Errorf("%s has %d bytes free, under %d; the export waits", dir, free, settings.MinFreeBytes)
		}
		batchStart := time.Now()
		batch, err := exportReviewBatch(ctx, conn, dir, run, report.TotalBatches+1, report.LastContractId, size, host, settings)
		if err != nil {
			if ctx.Err() != nil {
				report.Stop = "canceled"
				return report, nil
			}
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && (pgErr.Code == "57014" || pgErr.Code == "55P03") && retries < settings.TimeoutRetries {
				retries++
				size = max(settings.MinBatchContracts, size/2)
				if _, err := clearUnfinishedReviewBatch(dir, run); err != nil {
					return report, err
				}
				continue
			}
			return report, fmt.Errorf("batch %d after %s: %w", report.TotalBatches+1, report.LastContractId, err)
		}
		retries = 0
		if batch == nil {
			report.Done = true
			report.Stop = "done"
			return report, nil
		}
		report.Batches++
		report.Contracts += batch.Contracts
		report.TotalBatches++
		report.TotalContracts += batch.Contracts
		report.LastContractId = batch.UpperContractId
		for _, copied := range batch.Tables {
			report.TableRows[copied.Table] += copied.Rows
		}
		duration := time.Since(batchStart)
		if duration > settings.TargetBatchDuration {
			size = size / 2
		} else if duration < settings.TargetBatchDuration/4 {
			size = size + max(1, size/2)
		}
		size = max(settings.MinBatchContracts, min(settings.BatchContracts, size))
		if 0 < settings.MaxContractsPerSecond {
			paced := time.Duration(float64(batch.Contracts) / settings.MaxContractsPerSecond * float64(time.Second))
			if wait := paced - duration; 0 < wait {
				select {
				case <-ctx.Done():
				case <-time.After(wait):
				}
			}
		}
	}
}

// The run's batches, after removing the files of the batch a stopped run was
// writing.
func clearUnfinishedReviewBatch(dir string, run *ReviewRun) ([]*ReviewBatch, error) {
	batches, unfinished, err := reviewBatchState(dir, run)
	if err != nil {
		return nil, err
	}
	for _, path := range unfinished {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if 0 < len(unfinished) {
		if err := syncDir(dir); err != nil {
			return nil, err
		}
	}
	return batches, nil
}

// The first size tied contract ids after after, in id order, from each
// payment's range of the sweep index, merged; and how many of them each
// payment paid. Each payment's list holds every one of its contracts up to
// the merged list's last id, so the merged list is every tied contract in
// (after, last].
func readReviewBatchIds(ctx context.Context, q reviewQuerier, paymentIds []string, after string, size int) (contractIds []string, paymentContracts map[string]int64, returnErr error) {
	lists := map[string][]string{}
	merged := map[string]bool{}
	for _, paymentId := range paymentIds {
		var ids []string
		if err := q.QueryRow(ctx, `
			SELECT ARRAY(
				SELECT tied.contract_id FROM (
					SELECT DISTINCT contract_id FROM transfer_escrow_sweep
					WHERE payment_id = $1::uuid AND $2::uuid < contract_id
					ORDER BY contract_id
					LIMIT $3
				) AS tied
				ORDER BY tied.contract_id
			)::text[]
		`, paymentId, after, size).Scan(&ids); err != nil {
			return nil, nil, err
		}
		lists[paymentId] = ids
		for _, id := range ids {
			merged[id] = true
		}
	}
	for id := range merged {
		contractIds = append(contractIds, id)
	}
	sort.Strings(contractIds)
	if size < len(contractIds) {
		contractIds = contractIds[:size]
	}
	paymentContracts = map[string]int64{}
	if len(contractIds) == 0 {
		return contractIds, paymentContracts, nil
	}
	upper := contractIds[len(contractIds)-1]
	for _, paymentId := range paymentIds {
		for _, id := range lists[paymentId] {
			if id <= upper {
				paymentContracts[paymentId]++
			}
		}
	}
	return contractIds, paymentContracts, nil
}

// The session and bounds of a batch transaction, and of the verification's
// and the plan's reads.
func reviewSessionSql(settings *ReviewSettings) []string {
	return append(SessionSettingsSql(),
		fmt.Sprintf(`SET LOCAL statement_timeout = %d`, settings.StatementTimeout.Milliseconds()),
		fmt.Sprintf(`SET LOCAL lock_timeout = %d`, settings.LockTimeout.Milliseconds()),
		// the rows' digests are short expressions over many rows; compiling
		// them costs more than it saves
		`SET LOCAL jit = off`,
	)
}

// After the ids, a batch reads its rows by their ids with bitmap scans, as
// the contract horizon reaper does: each heap page once, in block order, with
// prefetch, rather than one synchronous random read per row.
func reviewBitmapSql(settings *ReviewSettings) []string {
	return []string{
		`SET LOCAL enable_indexscan = off`,
		`SET LOCAL enable_seqscan = off`,
		fmt.Sprintf(`SET LOCAL effective_io_concurrency = %d`, max(1, settings.IoConcurrency)),
	}
}

func execAll(ctx context.Context, q reviewQuerier, statements []string) error {
	for _, statement := range statements {
		if _, err := q.Exec(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

// One batch: the next tied contracts after after, at most size of them, and
// every row of the exported tables they have, read in one REPEATABLE READ READ
// ONLY transaction, each table's figures and COPY stream agreeing; then every
// stored copy read back and matched, then the manifest. Nil when no tied
// contract follows after.
func exportReviewBatch(
	ctx context.Context,
	conn *pgx.Conn,
	dir string,
	run *ReviewRun,
	index int64,
	after string,
	size int,
	host string,
	settings *ReviewSettings,
) (*ReviewBatch, error) {
	paymentIds := run.PaymentIds()
	batch := &ReviewBatch{
		Format:          ReviewBatchFormat,
		RunId:           run.RunId,
		Batch:           index,
		AfterContractId: after,
		ExportHost:      host,
		Tables:          []ReviewTableCopy{},
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	// read only, so a rollback ends it
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := execAll(ctx, tx, reviewSessionSql(settings)); err != nil {
		return nil, err
	}
	if err := tx.QueryRow(ctx, `SELECT (now() AT TIME ZONE 'UTC')::text`).Scan(&batch.SnapshotTime); err != nil {
		return nil, err
	}
	contractIds, paymentContracts, err := readReviewBatchIds(ctx, tx, paymentIds, after, size)
	if err != nil {
		return nil, err
	}
	if len(contractIds) == 0 {
		return nil, nil
	}
	batch.UpperContractId = contractIds[len(contractIds)-1]
	batch.Contracts = int64(len(contractIds))
	batch.ContractIdDigest = reviewIdDigest(contractIds)
	batch.PaymentContracts = paymentContracts

	if err := execAll(ctx, tx, reviewBitmapSql(settings)); err != nil {
		return nil, err
	}
	idsCte := reviewIdsCteSql(paymentIds, after, batch.UpperContractId)
	// the statements below select the batch by its bounds: they must select
	// exactly these ids
	var cteContracts int64
	var cteDigest string
	if err := tx.QueryRow(ctx, idsCte+`SELECT count(*), coalesce(sum(('x' || left(md5(contract_id::text), 16))::bit(64)::bigint::numeric), 0)::text FROM ids`).Scan(&cteContracts, &cteDigest); err != nil {
		return nil, err
	}
	if cteContracts != batch.Contracts || cteDigest != batch.ContractIdDigest {
		return nil, fmt.Errorf("the batch's bounds select %d contracts with digest %s, its ids are %d with %s", cteContracts, cteDigest, batch.Contracts, batch.ContractIdDigest)
	}
	idSet := make(map[string]bool, len(contractIds))
	for _, id := range contractIds {
		idSet[id] = true
	}

	for _, table := range run.Tables {
		copied, seenIds, err := exportReviewTable(ctx, tx, dir, table, batch, idsCte, idSet, settings)
		if err != nil {
			return nil, err
		}
		batch.Tables = append(batch.Tables, *copied)
		if seenIds != nil {
			for _, id := range contractIds {
				if !seenIds[id] {
					batch.MissingContractIds = append(batch.MissingContractIds, id)
				}
			}
		}
		if settings.AfterTableStored != nil {
			if err := settings.AfterTableStored(index, table.Table); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if err := syncDir(dir); err != nil {
		return nil, err
	}
	// the stored copies, read back from storage
	for i, table := range run.Tables {
		if err := verifyReviewCopy(dir, table, batch, batch.Tables[i]); err != nil {
			return nil, fmt.Errorf("the stored copy does not verify: %w", err)
		}
	}
	batch.VerifiedTime = settings.now().Format(time.RFC3339Nano)
	if err := writeReviewJson(filepath.Join(dir, reviewManifestName(index)), batch); err != nil {
		return nil, err
	}
	return batch, nil
}

// One table's rows of the batch: its figures, then its COPY into a partial
// file, which must agree with them line for line; the file is synced and
// renamed into place. For transfer_contract, also the contract ids it holds.
func exportReviewTable(
	ctx context.Context,
	tx pgx.Tx,
	dir string,
	table ReviewTable,
	batch *ReviewBatch,
	idsCte string,
	idSet map[string]bool,
	settings *ReviewSettings,
) (copied *ReviewTableCopy, seenIds map[string]bool, returnErr error) {
	copied = &ReviewTableCopy{Table: table.Table, File: reviewCopyName(batch.Batch, table.Table)}
	if err := tx.QueryRow(ctx, reviewStatsSql(table, idsCte)).Scan(&copied.Rows, &copied.RowDigest); err != nil {
		return nil, nil, err
	}
	path := filepath.Join(dir, copied.File)
	partialPath := path + partialSuffix
	file, err := os.OpenFile(partialPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if file != nil {
			file.Close()
		}
		if returnErr != nil {
			os.Remove(partialPath)
		}
	}()
	fileHash := sha256.New()
	fileCount := &countingWriter{}
	buffered := bufio.NewWriterSize(io.MultiWriter(file, fileHash, fileCount), 1024*1024)
	encoder, err := zstd.NewWriter(buffered, zstd.WithEncoderLevel(settings.Level), zstd.WithEncoderConcurrency(max(1, settings.Concurrency)))
	if err != nil {
		return nil, nil, err
	}
	copyHash := sha256.New()
	copyCount := &countingWriter{}
	figures, err := newReviewFigures(table, batch.AfterContractId, batch.UpperContractId)
	if err != nil {
		encoder.Close()
		return nil, nil, err
	}
	figures.ids = idSet
	if table.Table == "transfer_contract" {
		seenIds = map[string]bool{}
		figures.seenIds = seenIds
	}
	if _, err := tx.Conn().PgConn().CopyTo(ctx, io.MultiWriter(encoder, copyHash, copyCount, figures), reviewCopyOutSql(table, idsCte)); err != nil {
		encoder.Close()
		return nil, nil, fmt.Errorf("copy %s: %w", table.Table, err)
	}
	if err := encoder.Close(); err != nil {
		return nil, nil, err
	}
	if err := buffered.Flush(); err != nil {
		return nil, nil, err
	}
	if err := figures.Close(); err != nil {
		return nil, nil, fmt.Errorf("copy %s: %w", table.Table, err)
	}
	if figures.Rows != copied.Rows || figures.Digest() != copied.RowDigest {
		return nil, nil, fmt.Errorf("the COPY of %s has %d rows with digest %s, PostgreSQL counted %d with %s", table.Table, figures.Rows, figures.Digest(), copied.Rows, copied.RowDigest)
	}
	copied.CopySha256 = hex.EncodeToString(copyHash.Sum(nil))
	copied.CopyBytes = copyCount.n
	copied.FileSha256 = hex.EncodeToString(fileHash.Sum(nil))
	copied.FileBytes = fileCount.n
	if err := file.Sync(); err != nil {
		return nil, nil, err
	}
	if err := file.Close(); err != nil {
		file = nil
		return nil, nil, err
	}
	file = nil
	if err := os.Rename(partialPath, path); err != nil {
		return nil, nil, err
	}
	return copied, seenIds, nil
}

// The batches a run in dir holds and their totals, with no database. Refuses a
// directory whose batches do not tile the id space.
func ReviewStatus(dir string) (*ReviewRun, []*ReviewBatch, []string, error) {
	run, err := ReadReviewRun(dir)
	if err != nil {
		return nil, nil, nil, err
	}
	batches, unfinished, err := reviewBatchState(dir, run)
	return run, batches, unfinished, err
}

// The SQL of a table restore: a new table of the exported table's columns,
// whose copies load with ReviewCopyInSql. The column types are the catalog's
// when the run started.
func ReviewRestoreTableSql(table ReviewTable, target string) string {
	columns := make([]string, 0, len(table.Columns))
	for _, column := range table.Columns {
		columns = append(columns, quoteIdent(column.Name)+" "+column.Type)
	}
	return fmt.Sprintf("CREATE TABLE %s (%s)", target, strings.Join(columns, ", "))
}
