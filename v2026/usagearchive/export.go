package usagearchive

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/klauspost/compress/zstd"
)

// One verified export of one week, as its manifest file holds it.
type Manifest struct {
	Format string `json:"format"`
	// the week leaf and its contract id bounds, [lower, upper)
	Leaf            string `json:"leaf"`
	WeekStart       string `json:"week_start"`
	LowerContractId string `json:"lower_contract_id"`
	UpperContractId string `json:"upper_contract_id"`
	// the figures PostgreSQL read in the export's snapshot; the COPY stream
	// and the stored file both match them
	Rows          int64  `json:"rows"`
	NullCloseRows int64  `json:"null_close_rows"`
	MinContractId string `json:"min_contract_id"`
	MaxContractId string `json:"max_contract_id"`
	MinCloseTime  string `json:"min_close_time"`
	MaxCloseTime  string `json:"max_close_time"`
	RowDigest     string `json:"row_digest"`
	// the uncompressed COPY stream and the stored file
	CopySha256 string `json:"copy_sha256"`
	CopyBytes  int64  `json:"copy_bytes"`
	File       string `json:"file"`
	FileSha256 string `json:"file_sha256"`
	FileBytes  int64  `json:"file_bytes"`
	// the export transaction's clock (UTC, PostgreSQL's text), the host that
	// wrote the file, and when the stored file was read back and matched
	SnapshotTime string `json:"snapshot_time"`
	ExportHost   string `json:"export_host"`
	VerifiedTime string `json:"verified_time,omitempty"`
}

const (
	copySuffix     = ".copy.zst"
	manifestSuffix = ".manifest.json"
	partialSuffix  = ".partial"
)

// The manifest of the file at path, by name.
func ManifestPathOf(copyPath string) string {
	return strings.TrimSuffix(copyPath, copySuffix) + manifestSuffix
}

// A week leaf's bounds, from its partition bound expression.
var rangeBoundPattern = regexp.MustCompile(`^FOR VALUES FROM \('([0-9a-f-]{36})'\) TO \('([0-9a-f-]{36})'\)$`)

// A week leaf of the archive and its contract id bounds.
type WeekLeaf struct {
	Leaf            string
	WeekStart       time.Time
	LowerContractId string
	UpperContractId string
}

// The archive's week leaves, oldest first. The DEFAULT partition and any leaf
// not named for a Monday week are left out.
func ListWeekLeaves(ctx context.Context, conn *pgx.Conn) ([]WeekLeaf, error) {
	rows, err := conn.Query(ctx, `
		SELECT child.relname, pg_get_expr(child.relpartbound, child.oid)
		FROM pg_inherits AS inheritance
		JOIN pg_class AS child ON child.oid = inheritance.inhrelid
		WHERE inheritance.inhparent = to_regclass('public.`+ArchiveTable+`')
		ORDER BY child.relname
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	leaves := []WeekLeaf{}
	for rows.Next() {
		var name, bound string
		if err := rows.Scan(&name, &bound); err != nil {
			return nil, err
		}
		week, ok := WeekOfLeaf(name)
		if !ok {
			continue
		}
		match := rangeBoundPattern.FindStringSubmatch(bound)
		if match == nil {
			return nil, fmt.Errorf("%s has bound %q, not a contract id range", name, bound)
		}
		leaves = append(leaves, WeekLeaf{Leaf: name, WeekStart: week, LowerContractId: match[1], UpperContractId: match[2]})
	}
	return leaves, rows.Err()
}

// Settings of one export.
type ExportSettings struct {
	// the directory the files go to; it must exist
	Dir string
	// recorded in the manifest
	Host string
	// zstd's level
	Level zstd.EncoderLevel
	// zstd's threads
	Concurrency int
	// now, for the names of the files; nil is time.Now
	Clock func() time.Time
}

// Exports one week leaf to settings.Dir and verifies the stored file: in one
// REPEATABLE READ READ ONLY transaction it reads the week's figures and its
// COPY, which must agree line for line; the file is synced and renamed into
// place, then read back from storage and matched again. Returns the verified
// manifest, which is also on disk. A failed export leaves no file.
func ExportWeek(ctx context.Context, conn *pgx.Conn, leaf WeekLeaf, settings *ExportSettings) (manifest *Manifest, returnErr error) {
	clock := settings.Clock
	if clock == nil {
		clock = time.Now
	}
	base := filepath.Join(settings.Dir, fmt.Sprintf("%s-%s", leaf.Leaf, clock().UTC().Format("20060102T150405Z")))
	copyPath := base + copySuffix
	partialPath := copyPath + partialSuffix
	file, err := os.OpenFile(partialPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return nil, err
	}
	defer func() {
		if file != nil {
			file.Close()
		}
		if returnErr != nil {
			os.Remove(partialPath)
		}
	}()

	manifest = &Manifest{
		Format:          ManifestFormat,
		Leaf:            leaf.Leaf,
		WeekStart:       leaf.WeekStart.Format(time.DateOnly),
		LowerContractId: leaf.LowerContractId,
		UpperContractId: leaf.UpperContractId,
		File:            filepath.Base(copyPath),
		ExportHost:      settings.Host,
	}
	fileHash := sha256.New()
	fileCount := &countingWriter{}
	buffered := bufio.NewWriterSize(io.MultiWriter(file, fileHash, fileCount), 1<<20)
	concurrency := settings.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	level := settings.Level
	if level == 0 {
		level = zstd.SpeedBetterCompression
	}
	encoder, err := zstd.NewWriter(buffered, zstd.WithEncoderLevel(level), zstd.WithEncoderConcurrency(concurrency))
	if err != nil {
		return nil, err
	}
	copyHash := sha256.New()
	copyCount := &countingWriter{}
	figures := &streamFigures{}

	table := `public."` + leaf.Leaf + `"`
	err = func() error {
		tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return err
		}
		// read only, so a rollback ends it
		defer tx.Rollback(context.WithoutCancel(ctx))
		for _, setting := range sessionSettingsSql {
			if _, err := tx.Exec(ctx, setting); err != nil {
				return err
			}
		}
		for _, setting := range []string{`SET LOCAL statement_timeout = 0`, `SET LOCAL idle_in_transaction_session_timeout = 0`} {
			if _, err := tx.Exec(ctx, setting); err != nil {
				return err
			}
		}
		// a server without transaction_timeout (before PostgreSQL 17) has
		// nothing to lift
		if _, err := tx.Exec(ctx, `SELECT set_config('transaction_timeout', '0', true) WHERE current_setting('transaction_timeout', true) IS NOT NULL`); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT (now() AT TIME ZONE 'UTC')::text`).Scan(&manifest.SnapshotTime); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, StatsSql(table)).Scan(
			&manifest.Rows, &manifest.NullCloseRows,
			&manifest.MinContractId, &manifest.MaxContractId,
			&manifest.MinCloseTime, &manifest.MaxCloseTime,
			&manifest.RowDigest,
		); err != nil {
			return err
		}
		_, err = tx.Conn().PgConn().CopyTo(ctx, io.MultiWriter(encoder, copyHash, copyCount, figures), CopyOutSql(table))
		return err
	}()
	if err != nil {
		encoder.Close()
		return nil, fmt.Errorf("export %s: %w", leaf.Leaf, err)
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	if err := buffered.Flush(); err != nil {
		return nil, err
	}
	if err := figures.Close(); err != nil {
		return nil, fmt.Errorf("export %s: %w", leaf.Leaf, err)
	}
	manifest.CopySha256 = hex.EncodeToString(copyHash.Sum(nil))
	manifest.CopyBytes = copyCount.n
	manifest.FileSha256 = hex.EncodeToString(fileHash.Sum(nil))
	manifest.FileBytes = fileCount.n
	if err := figures.match(manifest, "the COPY stream"); err != nil {
		return nil, fmt.Errorf("export %s: %w", leaf.Leaf, err)
	}
	if err := file.Sync(); err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		file = nil
		return nil, err
	}
	file = nil
	if err := os.Rename(partialPath, copyPath); err != nil {
		return nil, err
	}
	if err := syncDir(settings.Dir); err != nil {
		os.Remove(copyPath)
		return nil, err
	}
	// the stored copy, read back from storage
	if err := VerifyCopy(copyPath, manifest); err != nil {
		os.Remove(copyPath)
		return nil, fmt.Errorf("export %s: the stored copy does not verify: %w", leaf.Leaf, err)
	}
	manifest.VerifiedTime = clock().UTC().Format(time.RFC3339Nano)
	if err := WriteManifest(ManifestPathOf(copyPath), manifest); err != nil {
		os.Remove(copyPath)
		return nil, err
	}
	return manifest, nil
}

// Whether the streamed figures equal the manifest's.
func (self *streamFigures) match(manifest *Manifest, what string) error {
	switch {
	case self.Rows != manifest.Rows:
		return fmt.Errorf("%s has %d rows, PostgreSQL counted %d", what, self.Rows, manifest.Rows)
	case self.NullCloseRows != manifest.NullCloseRows:
		return fmt.Errorf("%s has %d rows without a close time, PostgreSQL counted %d", what, self.NullCloseRows, manifest.NullCloseRows)
	case self.MinContractId != manifest.MinContractId || self.MaxContractId != manifest.MaxContractId:
		return fmt.Errorf("%s spans [%s, %s], PostgreSQL read [%s, %s]", what, self.MinContractId, self.MaxContractId, manifest.MinContractId, manifest.MaxContractId)
	case self.Digest() != manifest.RowDigest:
		return fmt.Errorf("%s has row digest %s, PostgreSQL computed %s", what, self.Digest(), manifest.RowDigest)
	}
	if 0 < manifest.Rows {
		if manifest.MinContractId < manifest.LowerContractId || manifest.UpperContractId <= manifest.MaxContractId {
			return fmt.Errorf("%s holds contract ids [%s, %s] outside the week [%s, %s)", what, manifest.MinContractId, manifest.MaxContractId, manifest.LowerContractId, manifest.UpperContractId)
		}
	}
	return nil
}

// Reads the stored copy back from storage, past the page cache where the
// platform allows, and matches it against the manifest: the file's size and
// SHA-256, the decompressed stream's size and SHA-256, and its lines' count,
// digest, ids and rows without a close time.
func VerifyCopy(copyPath string, manifest *Manifest) error {
	if manifest.Format != ManifestFormat {
		return fmt.Errorf("manifest format %q is not %q", manifest.Format, ManifestFormat)
	}
	if filepath.Base(copyPath) != manifest.File {
		return fmt.Errorf("%s is not the manifest's file %s", copyPath, manifest.File)
	}
	file, err := os.Open(copyPath)
	if err != nil {
		return err
	}
	defer file.Close()
	dropCachedPages(file)
	fileHash := sha256.New()
	fileCount := &countingWriter{}
	// every byte read from the file passes the file's hash once, under the
	// buffer the decoder reads from
	stored := bufio.NewReaderSize(io.TeeReader(file, io.MultiWriter(fileHash, fileCount)), 1<<20)
	decoder, err := zstd.NewReader(stored, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return err
	}
	defer decoder.Close()
	copyHash := sha256.New()
	copyCount := &countingWriter{}
	figures := &streamFigures{}
	if _, err := io.Copy(io.MultiWriter(copyHash, copyCount, figures), decoder); err != nil {
		return err
	}
	// whatever follows the last frame still counts in the file's hash
	if _, err := io.Copy(io.Discard, stored); err != nil {
		return err
	}
	if err := figures.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(fileHash.Sum(nil)); got != manifest.FileSha256 || fileCount.n != manifest.FileBytes {
		return fmt.Errorf("the file is %d bytes with SHA-256 %s, not %d bytes with %s", fileCount.n, got, manifest.FileBytes, manifest.FileSha256)
	}
	if got := hex.EncodeToString(copyHash.Sum(nil)); got != manifest.CopySha256 || copyCount.n != manifest.CopyBytes {
		return fmt.Errorf("the COPY stream is %d bytes with SHA-256 %s, not %d bytes with %s", copyCount.n, got, manifest.CopyBytes, manifest.CopySha256)
	}
	return figures.match(manifest, "the stored copy")
}

// Reads a manifest and its file and verifies the file.
func VerifyManifest(manifestPath string) (*Manifest, error) {
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		return nil, err
	}
	copyPath := filepath.Join(filepath.Dir(manifestPath), manifest.File)
	if err := VerifyCopy(copyPath, manifest); err != nil {
		return manifest, fmt.Errorf("%s: %w", copyPath, err)
	}
	return manifest, nil
}

func ReadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	manifest := &Manifest{}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(manifest); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if manifest.Format != ManifestFormat {
		return nil, fmt.Errorf("%s: format %q is not %q", path, manifest.Format, ManifestFormat)
	}
	return manifest, nil
}

// Writes the manifest beside its file: a partial file, synced, renamed into
// place, and the directory synced.
func WriteManifest(path string, manifest *Manifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
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

// The verified manifests in dir, by leaf, each leaf's newest last.
func ListManifests(dir string) (map[string][]*Manifest, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "st_provider_usage_archive_w*"+manifestSuffix))
	if err != nil {
		return nil, err
	}
	byLeaf := map[string][]*Manifest{}
	for _, path := range paths {
		manifest, err := ReadManifest(path)
		if err != nil {
			return nil, err
		}
		if manifest.VerifiedTime == "" {
			continue
		}
		byLeaf[manifest.Leaf] = append(byLeaf[manifest.Leaf], manifest)
	}
	return byLeaf, nil
}

func syncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer handle.Close()
	if err := handle.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return err
	}
	return nil
}

type countingWriter struct {
	n int64
}

func (self *countingWriter) Write(p []byte) (int, error) {
	self.n += int64(len(p))
	return len(p), nil
}
