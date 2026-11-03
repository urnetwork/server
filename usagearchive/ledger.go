package usagearchive

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
)

// The ledger is absent until its migration is applied: exports made before
// then are registered later (Register), from their manifests.
var ErrLedgerAbsent = errors.New("the export ledger " + LedgerTable + " does not exist yet")

// Whether the ledger table exists.
func LedgerExists(ctx context.Context, conn *pgx.Conn) (bool, error) {
	var exists bool
	err := conn.QueryRow(ctx, `SELECT to_regclass('public.`+LedgerTable+`') IS NOT NULL`).Scan(&exists)
	return exists, err
}

// Records a verified export in the ledger, after reading the stored copy back
// once more. location is where the copy is kept, for example
// backup-host:/srv/usage-archive. Registering the same file again records
// nothing; registered reports whether this call wrote the row.
func Register(ctx context.Context, conn *pgx.Conn, manifestPath string, location string) (manifest *Manifest, registered bool, returnErr error) {
	manifest, err := VerifyManifest(manifestPath)
	if err != nil {
		return manifest, false, err
	}
	if manifest.VerifiedTime == "" {
		return manifest, false, fmt.Errorf("%s was never verified", manifestPath)
	}
	verifiedTime, err := time.Parse(time.RFC3339Nano, manifest.VerifiedTime)
	if err != nil {
		return manifest, false, err
	}
	exists, err := LedgerExists(ctx, conn)
	if err != nil {
		return manifest, false, err
	}
	if !exists {
		return manifest, false, ErrLedgerAbsent
	}
	nullable := func(value string) any {
		if value == "" {
			return nil
		}
		return value
	}
	tag, err := conn.Exec(
		ctx,
		`
		INSERT INTO `+LedgerTable+` (
			export_id, create_time, week_start, leaf_name, lower_contract_id, upper_contract_id,
			row_count, null_close_count, min_contract_id, max_contract_id,
			min_close_time, max_close_time, row_digest,
			copy_sha256, copy_bytes, file_name, file_sha256, file_bytes,
			location, export_host, snapshot_time, verified_time
		) VALUES (
			gen_random_uuid(), now() AT TIME ZONE 'UTC', $1::timestamp, $2, $3::uuid, $4::uuid,
			$5, $6, $7::uuid, $8::uuid,
			$9::timestamp, $10::timestamp, $11::numeric,
			$12, $13, $14, $15, $16,
			$17, $18, $19::timestamp, $20::timestamp
		)
		ON CONFLICT (file_sha256) DO NOTHING
		`,
		manifest.WeekStart, manifest.Leaf, manifest.LowerContractId, manifest.UpperContractId,
		manifest.Rows, manifest.NullCloseRows, nullable(manifest.MinContractId), nullable(manifest.MaxContractId),
		nullable(manifest.MinCloseTime), nullable(manifest.MaxCloseTime), manifest.RowDigest,
		manifest.CopySha256, manifest.CopyBytes, manifest.File, manifest.FileSha256, manifest.FileBytes,
		filepath.Clean(location), manifest.ExportHost, manifest.SnapshotTime, verifiedTime.UTC().Format("2006-01-02 15:04:05.999999"),
	)
	if err != nil {
		return manifest, false, err
	}
	return manifest, tag.RowsAffected() == 1, nil
}

// A week's newest export the ledger holds, if any.
type LedgerExport struct {
	Leaf        string
	RowCount    int64
	RowDigest   string
	FileName    string
	FileSha256  string
	DroppedTime *time.Time
}

// The ledger's newest export of each leaf.
func LatestExports(ctx context.Context, conn *pgx.Conn) (map[string]*LedgerExport, error) {
	rows, err := conn.Query(ctx, `
		SELECT DISTINCT ON (leaf_name) leaf_name, row_count, row_digest::text, file_name, file_sha256, dropped_time
		FROM `+LedgerTable+`
		ORDER BY leaf_name, snapshot_time DESC, create_time DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	exports := map[string]*LedgerExport{}
	for rows.Next() {
		export := &LedgerExport{}
		if err := rows.Scan(&export.Leaf, &export.RowCount, &export.RowDigest, &export.FileName, &export.FileSha256, &export.DroppedTime); err != nil {
			return nil, err
		}
		exports[export.Leaf] = export
	}
	return exports, rows.Err()
}
