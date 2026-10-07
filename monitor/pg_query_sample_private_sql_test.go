// Exercise the actual reducer/persistence boundary with visibly synthetic SQL.
package monitor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Source frames keep the old fixed fields and add only the bounded SQL column.
func pgSampleSqlTestFrames(now time.Time) []map[string]any {
	frames := pgSampleTestFrames(now)
	frames[0]["track_activity_query_size"] = 16384
	for _, frame := range frames {
		if frame["kind"] != "activity" {
			continue
		}
		frame["sql_capture_version"] = 1
		rows := frame["rows"].([][]any)
		for i := range rows {
			text := []byte("SELECT 'synthetic-private-literal' FROM contract_close /* synthetic-sql-owner */")
			rows[i][6] = "contract_close_access"
			rows[i] = append(rows[i], []any{len(text), base64.StdEncoding.EncodeToString(text), false})
		}
	}
	return frames
}

func TestPgQuerySamplePrivateSqlRetainsSourceWithoutPublicDisclosure(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	r, err := parsePgQuerySample(pgSampleTestEncode(t, pgSampleSqlTestFrames(now)), now)
	if err != nil {
		t.Fatal("qualified source rejected")
	}
	public, private, err := pgSampleEncodeReceipts(&r)
	if err != nil || len(private) == 0 || r.PrivateSql == nil || r.PrivateSql.RetainedStatements != 2 || r.PrivateSql.CapturedGroupSamples != 24 || r.PrivateSql.OmittedGroupSamples != 0 {
		t.Fatal("generic family lost its private source discriminator")
	}
	var detail pgSampleSqlReceipt
	if json.Unmarshal(private, &detail) != nil || len(detail.Rows) != 2 || len(detail.Rows[0].Samples) != 12 || detail.QueryIds[r.Load[0].Query] != "714727414314" {
		t.Fatal("private SQL/token or snapshot binding was lost")
	}
	if _, exists := detail.QueryIds["unknown"]; exists {
		t.Fatal("NULL query identity became an attributed mapping")
	}
	if !bytes.Contains(detail.Rows[0].Prefix, []byte("synthetic-sql-owner")) || detail.Rows[0].PrefixTruncated || detail.Rows[0].ActivityBufferMaybeTruncated {
		t.Fatal("healthy exact prefix became incomplete")
	}
	if r.PrivateSql.ReceiptSha256 != fmt.Sprintf("%x", sha256.Sum256(private)) {
		t.Fatal("private companion hash is not bound to the public projection")
	}
	findings := fmt.Sprint(pgSampleFindings(r, "synthetic"))
	for _, value := range []string{"714727414314", "5432167", "7865423", "9087614561", "SELECT '", "synthetic-private-literal", "synthetic-sql-owner", base64.StdEncoding.EncodeToString(detail.Rows[0].Prefix)} {
		if bytes.Contains(public, []byte(value)) || strings.Contains(findings, value) {
			t.Fatal("SQL or identity escaped into the public projection")
		}
	}
	for _, finding := range pgSampleFindings(r, "synthetic") {
		if finding.class == "pg-query-sample-coverage" {
			t.Fatal("complete healthy SQL coverage emitted a false coverage warning")
		}
	}
}

func TestPgQuerySamplePrivateSqlMalformedFailsClosed(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, kind := range []string{"version", "mixed-version", "field-count", "bad-base64", "bad-utf8", "nul", "oversize", "fraction", "negative", "null-string", "null-bool", "missing-nonempty", "short-prefix", "all-omitted", "wrong-kind", "too-many"} {
		frames := pgSampleSqlTestFrames(now)
		row := frames[2]["rows"].([][]any)[0]
		column := row[12].([]any)
		switch kind {
		case "version":
			frames[2]["sql_capture_version"] = 2
		case "mixed-version":
			delete(frames[3], "sql_capture_version")
		case "field-count":
			row[12] = append(column, true)
		case "bad-base64":
			column[1] = "synthetic-private-invalid"
		case "bad-utf8":
			column[0], column[1] = 1, base64.StdEncoding.EncodeToString([]byte{255})
		case "nul":
			column[0], column[1] = 1, base64.StdEncoding.EncodeToString([]byte{0})
		case "oversize":
			column[0], column[1] = 4097, base64.StdEncoding.EncodeToString(make([]byte, 4097))
		case "fraction":
			column[0] = 1.5
		case "negative":
			column[0] = -1
		case "null-string":
			column[1] = nil
		case "null-bool":
			column[2] = nil
		case "missing-nonempty":
			column[2] = true
		case "short-prefix":
			column[1] = ""
		case "all-omitted":
			row[12] = nil
		case "wrong-kind":
			frames[0]["sql_capture_version"] = 1
		case "too-many":
			rows := [][]any{}
			for i := range 17 {
				copyRow := append([]any(nil), row...)
				copyRow[0], copyRow[8] = fmt.Sprint(i+1), 1
				rows = append(rows, copyRow)
			}
			frames[2]["rows"], frames[2]["total"], frames[2]["groups"] = rows, 17, 17
		}
		_, err := parsePgQuerySample(pgSampleTestEncode(t, frames), now)
		if err == nil || err.Error() != "monitor: invalid bounded PostgreSQL sample" {
			t.Fatalf("malformed private column did not fail closed: %s", kind)
		}
	}
}

func TestPgQuerySamplePrivateSqlByteTruncationAndMissingRemainUnknown(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	frames := pgSampleSqlTestFrames(now)
	// The byte cap deliberately splits a multibyte rune. Preserve exact bytes
	// as base64; do not silently replace that partial prefix with another string.
	text := []byte(strings.Repeat("x", 4095) + "界" + strings.Repeat("z", 16383-4098))
	for _, frame := range frames {
		if frame["kind"] != "activity" {
			continue
		}
		rows := frame["rows"].([][]any)
		rows[0][12] = []any{len(text), base64.StdEncoding.EncodeToString(text[:4096]), false}
		rows[1][12] = []any{0, "", true}
		frame["query_text_truncated"] = 5
	}
	r, err := parsePgQuerySample(pgSampleTestEncode(t, frames), now)
	if err != nil {
		t.Fatal("bounded multibyte prefix rejected")
	}
	_, body, err := pgSampleEncodeReceipts(&r)
	var private pgSampleSqlReceipt
	if err != nil || json.Unmarshal(body, &private) != nil || len(private.Rows) != 2 || !bytes.Equal(private.Rows[0].Prefix, text[:4096]) || !private.Rows[0].PrefixTruncated || !private.Rows[0].ActivityBufferMaybeTruncated || !private.Rows[1].Missing || r.PrivateSql.MissingGroupSamples != 12 || r.PrivateSql.TruncatedGroupSamples != 12 {
		t.Fatal("byte or missing-source boundary became complete attribution")
	}
	coverage := false
	for _, finding := range pgSampleFindings(r, "synthetic") {
		coverage = coverage || finding.class == "pg-query-sample-coverage"
	}
	if !coverage {
		t.Fatal("partial source lost the coverage warning")
	}
}

func TestPgQuerySamplePrivateSqlCombinedRetentionCap(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	frames := pgSampleSqlTestFrames(now)
	for _, frame := range frames {
		if frame["kind"] != "activity" {
			continue
		}
		rows := [][]any{}
		for i := range 16 {
			row := append([]any(nil), frame["rows"].([][]any)[0]...)
			row[0], row[8] = fmt.Sprint(i+1), 1
			text := fmt.Sprintf("/* synthetic sample %v row %d */", frame["sample"], i) + strings.Repeat("x", 4096)
			row[12] = []any{len(text), base64.StdEncoding.EncodeToString([]byte(text)[:4096]), false}
			rows = append(rows, row)
		}
		frame["rows"], frame["total"], frame["groups"] = rows, 16, 16
		frame["query_text_truncated"] = 16
	}
	r, err := parsePgQuerySample(pgSampleTestEncode(t, frames), now)
	if err != nil {
		t.Fatal("bounded saturated input rejected")
	}
	public, private, err := pgSampleEncodeReceipts(&r)
	if err != nil || len(public)+len(private) > pgSampleReceiptBytes || r.PrivateSql.CapturedGroupSamples != 192 || r.PrivateSql.DistinctStatements != 192 || r.PrivateSql.RetainedStatements == 0 || r.PrivateSql.OmittedStatements == 0 || r.PrivateSql.RetainedStatements+r.PrivateSql.OmittedStatements != 192 {
		t.Fatal("combined receipt cap or explicit omission accounting failed")
	}
	publicAgain, privateAgain, err := pgSampleEncodeReceipts(&r)
	if err != nil || !bytes.Equal(public, publicAgain) || !bytes.Equal(private, privateAgain) {
		t.Fatal("bounded retention changed across an identical encoding")
	}
}

func TestPgQuerySamplePrivateSqlActualProbePersistsAndDoesNotRetry(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	calls := 0
	env := pgSampleTestEnv(t, func([]string, string) (string, string, error) {
		calls++
		return pgSampleTestEncode(t, pgSampleSqlTestFrames(now)), "", nil
	})
	probe := pgQuerySampleProbe{}
	findings, err := probe.check(context.Background(), env)
	if err != nil {
		t.Fatal("actual probe rejected private persistence")
	}
	dir := filepath.Join(env.cfg.stateDir, "pg-query-sample")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 3 {
		t.Fatal("actual probe did not persist marker, projection and private SQL")
	}
	var receipt pgQuerySampleReceipt
	var private []byte
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("sample evidence is not private")
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("evidence read failed")
		}
		if strings.HasPrefix(entry.Name(), "private-sql-") {
			private = body
		} else if strings.HasPrefix(entry.Name(), "receipt-") {
			if json.Unmarshal(body, &receipt) != nil || bytes.Contains(body, []byte("714727414314")) {
				t.Fatal("projection exposed private source")
			}
		}
	}
	if !receipt.Complete || receipt.PrivateSql == nil || receipt.PrivateSql.ReceiptSha256 != fmt.Sprintf("%x", sha256.Sum256(private)) || strings.Contains(fmt.Sprint(findings), "synthetic-private-literal") {
		t.Fatal("actual private/public binding failed")
	}
	if _, err := probe.check(context.Background(), env); err != nil || calls != 1 {
		t.Fatal("private SQL observation bypassed the spent-attempt guard")
	}
	if _, err := pgSampleStorePrivateSql(dir, private); err != nil {
		t.Fatal("identical immutable private evidence refused")
	}
	path := filepath.Join(dir, "private-sql-"+receipt.PrivateSql.ReceiptSha256+".json")
	if err := os.WriteFile(path, []byte("synthetic conflict"), 0600); err != nil {
		t.Fatal("conflict fixture failed")
	}
	if _, err := pgSampleStorePrivateSql(dir, private); err == nil {
		t.Fatal("immutable private evidence was overwritten")
	}
}

func TestPgQuerySamplePrivateSqlDirectoryRefusalConsumesAttempt(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	calls := 0
	env := pgSampleTestEnv(t, func([]string, string) (string, string, error) {
		calls++
		return pgSampleTestEncode(t, pgSampleSqlTestFrames(now)), "", nil
	})
	dir := filepath.Join(env.cfg.stateDir, "pg-query-sample")
	if os.Mkdir(dir, 0755) != nil || os.Chmod(dir, 0755) != nil {
		t.Fatal("directory fixture failed")
	}
	probe := pgQuerySampleProbe{}
	if _, err := probe.check(context.Background(), env); err == nil || strings.Contains(err.Error(), "synthetic") {
		t.Fatal("nonprivate destination admitted private SQL")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".attempt") {
		t.Fatal("failed private evidence gained a complete receipt")
	}
	if _, err := probe.check(context.Background(), env); err != nil || calls != 1 {
		t.Fatal("private persistence failure retried the source")
	}
}

func TestPgQuerySamplePrivateSqlWireBoundsAndLegacyUnknown(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	legacy, err := parsePgQuerySample(pgSampleTestEncode(t, pgSampleTestFrames(now)), now)
	if err != nil || legacy.PrivateSql != nil {
		t.Fatal("old receipt acquired unobserved SQL authority")
	}
	_, private, err := pgSampleEncodeReceipts(&legacy)
	if err != nil || len(private) != 0 {
		t.Fatal("old source manufactured a private SQL companion")
	}
	frames := pgSampleSqlTestFrames(now)
	for _, frame := range frames {
		if frame["kind"] == "history" {
			rows := [][]any{}
			for i := range 5000 {
				rows = append(rows, []any{fmt.Sprint(900000000000 + i), 1, 2, 3, "other"})
			}
			frame["rows"], frame["total"] = rows, 5000
		} else if frame["kind"] == "activity" {
			rows := [][]any{}
			for i := range 128 {
				row := append([]any(nil), frame["rows"].([][]any)[0]...)
				row[0], row[8], row[12] = fmt.Sprint(800000000000+i), 1, nil
				if i < 16 {
					row[12] = []any{4096, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'x'}, 4096)), false}
				}
				rows = append(rows, row)
			}
			frame["rows"], frame["groups"], frame["total"] = rows, 128, 128
			frame["query_text_truncated"] = 16
		}
	}
	wire := pgSampleTestEncode(t, frames)
	if len(wire) >= pgQuerySampleMaxBytes {
		t.Fatal("bounded SQL extension exhausted the unchanged wire cap")
	}
	receipt, err := parsePgQuerySample(wire, now)
	if err != nil || receipt.PrivateSql.CapturedGroupSamples != 192 || receipt.PrivateSql.OmittedGroupSamples != 1344 {
		t.Fatal("maximal finite source lost SQL omission accounting")
	}
	public, private, err := pgSampleEncodeReceipts(&receipt)
	if err != nil || len(public)+len(private) > pgSampleReceiptBytes {
		t.Fatal("maximal finite source exceeded combined retention")
	}
}
