package usagearchive

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/klauspost/compress/zstd"
)

var restoreTablePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// What a restore loaded.
type RestoreReport struct {
	Table     string
	Rows      int64
	RowDigest string
	// archive rows already in the DEFAULT partition inside the week's bounds:
	// ATTACH PARTITION refuses the restored table while any remain
	DefaultRowsInWeek int64
}

// Restores the export a manifest describes into a new table of the archive's
// shape, in one transaction: the stored copy is verified first, the table is
// created LIKE the archive (columns, defaults, NOT NULL and the outcome
// check), the copy is loaded with COPY FROM, and the loaded rows' count and
// digest must equal the manifest's, or nothing is kept. The table is not
// attached: AttachSql gives the statements that make it the week's partition
// again, which the owner runs.
func Restore(ctx context.Context, conn *pgx.Conn, manifestPath string, table string) (*RestoreReport, error) {
	if !restoreTablePattern.MatchString(table) {
		return nil, fmt.Errorf("restore table %q is not a lowercase identifier", table)
	}
	if _, ok := WeekOfLeaf(table); ok {
		return nil, fmt.Errorf("restore into a new table, not the week leaf name %s", table)
	}
	manifest, err := VerifyManifest(manifestPath)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(filepath.Join(filepath.Dir(manifestPath), manifest.File))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder, err := zstd.NewReader(bufio.NewReaderSize(file, 1<<20), zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer decoder.Close()

	report := &RestoreReport{Table: table}
	quoted := `public."` + table + `"`
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	for _, setting := range sessionSettingsSql {
		if _, err := tx.Exec(ctx, setting); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = 0`); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('transaction_timeout', '0', true) WHERE current_setting('transaction_timeout', true) IS NOT NULL`); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (LIKE public.%s INCLUDING DEFAULTS INCLUDING CONSTRAINTS)`, quoted, ArchiveTable)); err != nil {
		return nil, err
	}
	if _, err := tx.Conn().PgConn().CopyFrom(ctx, decoder, CopyInSql(quoted)); err != nil {
		return nil, fmt.Errorf("load %s: %w", manifest.File, err)
	}
	if err := tx.QueryRow(ctx, DigestSql(quoted)).Scan(&report.Rows, &report.RowDigest); err != nil {
		return nil, err
	}
	if report.Rows != manifest.Rows || report.RowDigest != manifest.RowDigest {
		return nil, fmt.Errorf("restored %d rows with digest %s, the export has %d with %s", report.Rows, report.RowDigest, manifest.Rows, manifest.RowDigest)
	}
	if err := tx.QueryRow(
		ctx,
		`SELECT count(*) FROM public.st_provider_usage_archive_default WHERE $1::uuid <= contract_id AND contract_id < $2::uuid`,
		manifest.LowerContractId, manifest.UpperContractId,
	).Scan(&report.DefaultRowsInWeek); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return report, nil
}

// The statements that attach a restored table as its week's partition again,
// under the archive's guards: the primary key the partitioned parent requires
// is built by the attach, the row guard is cloned onto it by the attach, and
// the TRUNCATE guard is added as every week leaf has it. ATTACH checks the
// DEFAULT partition holds no row inside the bounds, and refuses otherwise.
func AttachSql(manifest *Manifest, table string) []string {
	return []string{
		fmt.Sprintf(`ALTER TABLE public.%s ATTACH PARTITION public."%s" FOR VALUES FROM ('%s') TO ('%s')`, ArchiveTable, table, manifest.LowerContractId, manifest.UpperContractId),
		fmt.Sprintf(`CREATE TRIGGER st_provider_usage_archive_truncate_guard BEFORE TRUNCATE ON public."%s" FOR EACH STATEMENT EXECUTE FUNCTION st_provider_usage_archive_guard()`, table),
	}
}
