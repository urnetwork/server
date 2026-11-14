// Package usagearchive exports whole weeks of st_provider_usage_archive off the
// primary, verifies each copy where it is stored, records it in the export
// ledger (st_provider_usage_archive_export), and restores a week from its copy.
// The server's drop fence (server/db_provider_usage_archive_export.go) drops a
// week before its 52 weeks only against a verified ledger row whose rows and
// digest equal the week's rows in PostgreSQL.
//
// This package never imports github.com/urnetwork/server: the exporter
// (usagearchivectl) runs on the backup host, outside warp, and the server
// imports this package for the statements both sides must compute alike.
//
// # The copy
//
// One week is one PostgreSQL COPY of its leaf in text format, rows in
// contract_id order, compressed with zstd: a file that `zstd -dc | psql -c
// "COPY ... FROM STDIN"` restores. A manifest beside it records the leaf, its
// bounds, the counts, the row digest, and the SHA-256 of the COPY stream and of
// the file.
//
// # The row digest
//
// Each line of the COPY stream, without its newline, is hashed with MD5; the
// first 8 bytes, read as a big-endian signed 64-bit integer, are summed over
// every line into an exact integer. The sum does not depend on row order, so
// PostgreSQL computes the same number over the leaf with one aggregate
// (DigestSql), which reproduces the COPY text of each row: the uuid and the
// timestamp print without escapes, and the outcome (a CHECK-constrained ASCII
// word) and the jsonb text need only their backslashes doubled, since jsonb's
// output escapes every control character inside its strings. An export whose
// stream digest differs from the digest PostgreSQL computed in the same
// snapshot is refused, so any difference in that reasoning fails closed.
//
// The archive is append-only (its guard refuses UPDATE, DELETE and TRUNCATE),
// so a leaf whose row count still equals an export's, and whose digest matched
// it once, holds exactly that export's rows.
package usagearchive

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"math/big"
	"math/bits"
	"regexp"
	"time"
)

// The archive's partitioned parent and the ledger of its exports.
const (
	ArchiveTable = "st_provider_usage_archive"
	LedgerTable  = "st_provider_usage_archive_export"
)

// The manifest format this package writes and reads.
const ManifestFormat = "st_provider_usage_archive_export/v1"

// The columns of the archive, in the order a copy holds them.
const archiveColumns = "contract_id, outcome, close_time, provider_usage"

var weekLeafPattern = regexp.MustCompile(`^st_provider_usage_archive_w([0-9]{8})$`)

// The Monday a week leaf's name holds, or false for any other name.
func WeekOfLeaf(leaf string) (time.Time, bool) {
	match := weekLeafPattern.FindStringSubmatch(leaf)
	if match == nil {
		return time.Time{}, false
	}
	week, err := time.Parse("20060102", match[1])
	if err != nil || week.Weekday() != time.Monday {
		return time.Time{}, false
	}
	return week.UTC(), true
}

// The week leaf of the Monday week.
func LeafOfWeek(week time.Time) string {
	return "st_provider_usage_archive_w" + week.UTC().Format("20060102")
}

// One COPY text line of a row, in SQL over the columns of the archive. COPY's
// text format escapes a backslash and the control characters \b \f \n \r \t
// \v; the uuid and the timestamp contain none, the outcome is one of three
// ASCII words, and jsonb's text escapes every control character in its
// strings, so only backslashes need doubling. A NULL prints as \N.
const copyLineSql = `contract_id::text || E'\t' || replace(outcome, E'\\', E'\\\\') || E'\t' || coalesce(close_time::text, E'\\N') || E'\t' || coalesce(replace(provider_usage::text, E'\\', E'\\\\'), E'\\N')`

// The row digest of a table with the archive's columns, as one numeric.
const digestExpressionSql = `coalesce(sum(('x' || left(md5(` + copyLineSql + `), 16))::bit(64)::bigint::numeric), 0)::text`

// The statement that reads a week's figures in one snapshot: rows, rows
// without a close time, the first and last contract id, the earliest and
// latest close time and the row digest. table must be a quoted or plain
// identifier the caller controls.
func StatsSql(table string) string {
	// uuid has no min or max aggregate; the key's index gives both ends
	return fmt.Sprintf(`SELECT count(*), count(*) FILTER (WHERE close_time IS NULL),
		coalesce((SELECT contract_id::text FROM %[2]s ORDER BY contract_id LIMIT 1), ''),
		coalesce((SELECT contract_id::text FROM %[2]s ORDER BY contract_id DESC LIMIT 1), ''),
		coalesce(min(close_time)::text, ''), coalesce(max(close_time)::text, ''),
		%[1]s
		FROM %[2]s`, digestExpressionSql, table)
}

// The row digest of a table with the archive's columns.
func DigestSql(table string) string {
	return fmt.Sprintf(`SELECT count(*), %s FROM %s`, digestExpressionSql, table)
}

// The COPY a week's export reads.
func CopyOutSql(table string) string {
	return fmt.Sprintf(`COPY (SELECT %s FROM %s ORDER BY contract_id) TO STDOUT`, archiveColumns, table)
}

// The COPY a restore writes.
func CopyInSql(table string) string {
	return fmt.Sprintf(`COPY %s (%s) FROM STDIN`, table, archiveColumns)
}

// The session settings both sides print rows under, so a timestamp prints the
// same in the COPY stream and in the digest.
var sessionSettingsSql = []string{
	`SET LOCAL DateStyle = 'ISO, MDY'`,
	`SET LOCAL IntervalStyle = 'postgres'`,
	`SET LOCAL TimeZone = 'UTC'`,
	`SET LOCAL client_encoding = 'UTF8'`,
	`SET LOCAL standard_conforming_strings = on`,
}

// The session settings, for a transaction of the caller's that computes a
// digest.
func SessionSettingsSql() []string {
	return append([]string(nil), sessionSettingsSql...)
}

// The digest of one COPY line, without its newline.
func LineDigest(line []byte) int64 {
	sum := md5.Sum(line)
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

// What a COPY stream holds, read line by line as it streams: the line count,
// the row digest, the rows without a close time, the first and last contract
// id, and whether the ids strictly increase (so no row repeats).
type streamFigures struct {
	Rows          int64
	NullCloseRows int64
	MinContractId string
	MaxContractId string
	digest        digestSum
	pending       []byte
	lastId        []byte
	err           error
}

// An exact sum of signed 64-bit values in 128-bit two's complement: exact for
// fewer than 2^63 values.
type digestSum struct {
	high uint64
	low  uint64
}

func (self *digestSum) Add(value int64) {
	extension := uint64(0)
	if value < 0 {
		extension = ^uint64(0)
	}
	var carry uint64
	self.low, carry = bits.Add64(self.low, uint64(value), 0)
	self.high, _ = bits.Add64(self.high, extension, carry)
}

// The sum in decimal, as PostgreSQL prints the numeric.
func (self *digestSum) String() string {
	var total big.Int
	total.SetUint64(self.high)
	total.Lsh(&total, 64)
	total.Or(&total, new(big.Int).SetUint64(self.low))
	if self.high>>63 == 1 {
		// negative: subtract 2^128
		total.Sub(&total, new(big.Int).Lsh(big.NewInt(1), 128))
	}
	return total.String()
}

// The exact row digest, in decimal.
func (self *streamFigures) Digest() string {
	return self.digest.String()
}

func (self *streamFigures) line(line []byte) {
	if self.err != nil {
		return
	}
	fields := bytes.SplitN(line, []byte{'\t'}, 4)
	if len(fields) != 4 {
		self.err = fmt.Errorf("line %d has %d fields, not 4", self.Rows+1, len(fields))
		return
	}
	id := fields[0]
	if len(id) != 36 {
		self.err = fmt.Errorf("line %d has no contract id", self.Rows+1)
		return
	}
	if self.lastId != nil && bytes.Compare(self.lastId, id) >= 0 {
		self.err = fmt.Errorf("line %d: contract id %s does not follow %s", self.Rows+1, id, self.lastId)
		return
	}
	self.lastId = append(self.lastId[:0], id...)
	if self.Rows == 0 {
		self.MinContractId = string(id)
	}
	self.MaxContractId = string(id)
	if bytes.Equal(fields[2], []byte(`\N`)) {
		self.NullCloseRows++
	}
	self.digest.Add(LineDigest(line))
	self.Rows++
}

// Write splits the stream into lines. It never fails; Close reports a
// malformed stream.
func (self *streamFigures) Write(p []byte) (int, error) {
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

// The stream's first error: a line without its newline at the end, or a
// malformed line.
func (self *streamFigures) Close() error {
	if self.err != nil {
		return self.err
	}
	if 0 < len(self.pending) {
		return fmt.Errorf("the stream ends inside line %d", self.Rows+1)
	}
	return nil
}
