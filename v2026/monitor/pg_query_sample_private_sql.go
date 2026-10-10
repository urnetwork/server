// Private representative SQL is a separate, bounded evidence object. Public
// receipts and alerts retain only fixed coverage fields and an immutable hash.
package monitor

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"unicode/utf8"
)

const pgSampleSqlPrefixBytes = 4096
const pgSampleReceiptBytes = 262144

// Counts describe selected group-samples, not distinct backend invocations.
type pgSampleSqlCoverage struct {
	Schema                int    `json:"schema"`
	CapturedGroupSamples  int    `json:"captured_group_samples"`
	OmittedGroupSamples   int    `json:"omitted_group_samples"`
	MissingGroupSamples   int    `json:"missing_text_group_samples"`
	TruncatedGroupSamples int    `json:"truncated_text_group_samples"`
	DistinctStatements    int    `json:"distinct_representatives"`
	RetainedStatements    int    `json:"retained_representatives"`
	OmittedStatements     int    `json:"omitted_representatives"`
	MappedQueryTokens     int    `json:"mapped_query_tokens"`
	ReceiptSha256         string `json:"receipt_sha256"`
}

// These fixed dimensions bind a representative to its exact activity group.
// A NULL query ID is still unknown; it cannot identify another unknown group.
type pgSampleSqlGroup struct {
	Query       string `json:"query_token"`
	Family      string `json:"family"`
	State       string `json:"state"`
	Wait        string `json:"wait"`
	Owner       string `json:"client_owner"`
	Application string `json:"declared_application"`
	Backend     string `json:"backend"`
	Scope       string `json:"database_scope"`
}

// The byte prefix is base64 because its fixed byte boundary may split UTF-8.
// Sample indexes refer to the companion's clocks; no backend PID is retained.
type pgSampleSqlRow struct {
	pgSampleSqlGroup
	Samples                      []int  `json:"samples"`
	SourceBytes                  int    `json:"source_bytes"`
	Prefix                       []byte `json:"sql_prefix_base64"`
	Missing                      bool   `json:"text_missing"`
	PrefixTruncated              bool   `json:"prefix_truncated"`
	ActivityBufferMaybeTruncated bool   `json:"activity_buffer_maybe_truncated"`
}

// Only this private object contains query IDs and SQL bytes. Query-ID equality
// does not establish the originating process, payer or statement invocation.
type pgSampleSqlReceipt struct {
	Schema         int               `json:"schema"`
	SampleClocks   []float64         `json:"sample_clocks"`
	TrackQuerySize int               `json:"track_activity_query_size"`
	QueryIds       map[string]string `json:"query_ids_by_token"`
	Rows           []pgSampleSqlRow  `json:"representatives"`
}

// Keep the private group's identity identical to the public load projection.
func pgSampleSqlGroupForLoad(load pgSampleLoad) pgSampleSqlGroup {
	return pgSampleSqlGroup{Query: load.Query, Family: load.Family, State: load.State,
		Wait: load.Wait, Owner: load.Owner, Application: load.Application,
		Backend: load.Backend, Scope: load.Scope}
}

// Reject malformed private columns with a fixed error, never source content.
func pgSampleParseSql(raw json.RawMessage, load pgSampleLoad, sample, trackQuerySize int) (pgSampleSqlRow, error) {
	fail := func() (pgSampleSqlRow, error) {
		return pgSampleSqlRow{}, errors.New("invalid private sample column")
	}
	var parts []json.RawMessage
	if len(raw) > 6000 || json.Unmarshal(raw, &parts) != nil || len(parts) != 3 {
		return fail()
	}
	sourceBytes, err := pgSampleNumber(parts[0])
	if err != nil || math.Trunc(sourceBytes) != sourceBytes || sourceBytes > float64(trackQuerySize) {
		return fail()
	}
	var encoded string
	if string(parts[1]) == "null" || json.Unmarshal(parts[1], &encoded) != nil || len(encoded) > 5600 {
		return fail()
	}
	prefix, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(prefix) != min(int(sourceBytes), pgSampleSqlPrefixBytes) || bytes.IndexByte(prefix, 0) >= 0 {
		return fail()
	}
	validPrefix := utf8.Valid(prefix)
	if !validPrefix && sourceBytes > pgSampleSqlPrefixBytes {
		for tail := 1; tail < utf8.UTFMax; tail++ {
			if utf8.Valid(prefix[:len(prefix)-tail]) && !utf8.FullRune(prefix[len(prefix)-tail:]) {
				validPrefix = true
				break
			}
		}
	}
	if !validPrefix {
		return fail()
	}
	if string(parts[2]) != "true" && string(parts[2]) != "false" {
		return fail()
	}
	missing := string(parts[2]) == "true"
	if missing && sourceBytes != 0 {
		return fail()
	}
	return pgSampleSqlRow{pgSampleSqlGroup: pgSampleSqlGroupForLoad(load),
		Samples: []int{sample}, SourceBytes: int(sourceBytes), Prefix: prefix, Missing: missing,
		PrefixTruncated:              sourceBytes > pgSampleSqlPrefixBytes,
		ActivityBufferMaybeTruncated: sourceBytes >= float64(trackQuerySize-1)}, nil
}

// Keep the existing 256 KiB retention ceiling across both files. Deduplicate
// identical representatives and prefer the retained load groups in their order.
// A storage omission is explicit and never changes public load selection.
func pgSampleEncodeReceipts(receipt *pgQuerySampleReceipt) ([]byte, []byte, error) {
	fail := func() ([]byte, []byte, error) {
		return nil, nil, errors.New("bounded sample evidence exceeds limit")
	}
	if receipt.PrivateSql == nil {
		body, err := json.MarshalIndent(receipt, "", "  ")
		if err != nil || len(body)+1 > pgSampleReceiptBytes {
			return fail()
		}
		return append(body, '\n'), nil, nil
	}
	private := pgSampleSqlReceipt{Schema: 1, SampleClocks: receipt.SampleClocks,
		TrackQuerySize: receipt.TrackQuerySize, QueryIds: map[string]string{}, Rows: []pgSampleSqlRow{}}
	addToken := func(token string) {
		if id, ok := receipt.privateQueryIds[token]; ok {
			private.QueryIds[token] = id
		}
	}
	ranks := map[pgSampleSqlGroup]int{}
	for i, load := range receipt.Load {
		ranks[pgSampleSqlGroupForLoad(load)] = i
		addToken(load.Query)
	}
	for _, completed := range receipt.Completed {
		addToken(completed.Query)
	}
	for _, blocker := range receipt.Blockers {
		addToken(blocker.WaiterQuery)
		addToken(blocker.BlockerQuery)
	}
	unique := []pgSampleSqlRow{}
	positions := map[string]int{}
	for _, row := range receipt.privateSqlRows {
		keyRow := row
		keyRow.Samples = nil
		key, err := json.Marshal(keyRow)
		if err != nil {
			return fail()
		}
		if index, ok := positions[string(key)]; ok {
			unique[index].Samples = append(unique[index].Samples, row.Samples...)
		} else {
			positions[string(key)] = len(unique)
			unique = append(unique, row)
		}
	}
	coverage := receipt.PrivateSql
	coverage.DistinctStatements = len(unique)
	coverage.MappedQueryTokens = len(private.QueryIds)
	coverage.ReceiptSha256 = strings.Repeat("0", 64)
	sort.SliceStable(unique, func(i, j int) bool {
		a, aOk := ranks[unique[i].pgSampleSqlGroup]
		b, bOk := ranks[unique[j].pgSampleSqlGroup]
		if aOk != bOk {
			return aOk
		}
		return a < b
	})
	encode := func() ([]byte, []byte, error) {
		coverage.RetainedStatements = len(private.Rows)
		coverage.OmittedStatements = len(unique) - len(private.Rows)
		publicBody, err := json.MarshalIndent(receipt, "", "  ")
		if err != nil {
			return fail()
		}
		privateBody, err := json.Marshal(private)
		if err != nil || len(publicBody)+len(privateBody)+2 > pgSampleReceiptBytes {
			return fail()
		}
		return append(publicBody, '\n'), append(privateBody, '\n'), nil
	}
	if _, _, err := encode(); err != nil {
		return fail()
	}
	for _, row := range unique {
		if _, retained := ranks[row.pgSampleSqlGroup]; !retained {
			continue
		}
		private.Rows = append(private.Rows, row)
		if _, _, err := encode(); err != nil {
			private.Rows = private.Rows[:len(private.Rows)-1]
			break
		}
	}
	_, privateBody, err := encode()
	if err != nil {
		return fail()
	}
	coverage.ReceiptSha256 = fmt.Sprintf("%x", sha256.Sum256(privateBody))
	return encode()
}

// Private SQL requires the existing state subdirectory to remain private.
// Immutable companion creation precedes the public receipt that references it.
func pgSampleStorePrivateSql(dir string, raw []byte) (string, error) {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("private sample directory unavailable")
	}
	return pgSampleStoreObject(dir, "private-sql-", raw)
}
