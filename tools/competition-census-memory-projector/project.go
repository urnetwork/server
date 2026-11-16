// Reassemble the exact diagnostic test's ordered Output bytes before parsing
// complete logical lines. Go test-json may split one t.Log line across records.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

const diagnosticPackage = "github.com/urnetwork/server/monitor"
const diagnosticTest = "TestMigrationsSignalMergedCatalogUsesExactColumns"
const diagnosticMarker = "MIGRATION_CENSUS_SESSION_DIAGNOSTIC "

// Every memory/work ceiling belongs to this projection, not the native test.
type projectionSettings struct {
	MaximumInputBytes  int64
	MaximumRecordBytes int
	MaximumLineBytes   int
	MaximumOutputBytes int64
	MaximumRecords     int
	MaximumEvents      int
	MaximumJsonDepth   int
}

// Bounds cover the source-owned diagnostic without silently clipping evidence.
func defaultProjectionSettings() projectionSettings {
	return projectionSettings{MaximumInputBytes: 128 * 1024 * 1024, MaximumRecordBytes: 256 * 1024,
		MaximumLineBytes: 64 * 1024, MaximumOutputBytes: 64 * 1024 * 1024,
		MaximumRecords: 262144, MaximumEvents: 32768, MaximumJsonDepth: 16}
}

// Projection completion is not a native test pass or release authorization.
type projectionStats struct {
	InputBytes          int64  `json:"input_bytes"`
	InputSha256Read     string `json:"input_sha256_of_bytes_read"`
	InputComplete       bool   `json:"input_complete"`
	Records             int    `json:"records"`
	TargetOutputRecords int    `json:"target_output_records"`
	LogicalLines        int    `json:"logical_lines"`
	Events              int    `json:"events"`
	BeforeApply         int    `json:"before_apply"`
	AfterCensus         int    `json:"after_census"`
	OutputBytes         int64  `json:"output_bytes"`
	OutputSha256        string `json:"output_sha256"`
	TestOutcome         string `json:"test_outcome,omitempty"`
}

// Only fixed classifications reach error/report output; raw log data does not.
type projectionError struct{ code string }

func (self *projectionError) Error() string { return self.code }

// Unknown Go metadata is accepted, but duplicate keys and invalid JSON are not.
type testJsonRecord struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Output  *string `json:"Output,omitempty"`
}

// The full event remains raw JSON, preserving every number and diagnostic field.
type diagnosticEventHeader struct {
	Event int    `json:"event"`
	Phase string `json:"phase"`
}

// Recursive token validation rejects duplicate keys within the finite depth.
func validateProjectionJson(raw []byte, maximumDepth int) error {
	if !utf8.Valid(raw) {
		return errors.New("invalid utf8")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var read func(int) error
	read = func(depth int) error {
		if depth > maximumDepth {
			return errors.New("json depth exceeded")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delimiter {
		case '{':
			keys := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				name, valid := key.(string)
				if err != nil || !valid || keys[name] {
					return errors.New("duplicate or invalid json key")
				}
				keys[name] = true
				if err := read(depth + 1); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return errors.New("unclosed json object")
			}
		case '[':
			for decoder.More() {
				if err := read(depth + 1); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return errors.New("unclosed json array")
			}
		default:
			return errors.New("unexpected json delimiter")
		}
		return nil
	}
	if err := read(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing json data")
	}
	return nil
}

// One fixed role is projected; unrelated output/non-output records cannot flush
// a partial line or masquerade as this test's terminal outcome.
func projectDiagnostic(input io.Reader, output io.Writer, settings projectionSettings) (stats projectionStats, resultErr error) {
	fail := func(code string) error { return &projectionError{code: code} }
	if settings.MaximumInputBytes <= 0 || settings.MaximumRecordBytes <= 0 || settings.MaximumLineBytes <= 0 ||
		settings.MaximumOutputBytes <= 0 || settings.MaximumRecords <= 0 || settings.MaximumEvents <= 0 || settings.MaximumJsonDepth <= 0 {
		return stats, fail("invalid_settings")
	}
	inputHash, outputHash := sha256.New(), sha256.New()
	limited := &io.LimitedReader{R: input, N: settings.MaximumInputBytes + 1}
	reader := bufio.NewReaderSize(io.TeeReader(limited, inputHash), settings.MaximumRecordBytes+1)
	defer func() {
		stats.InputBytes = settings.MaximumInputBytes + 1 - limited.N
		stats.InputSha256Read = hex.EncodeToString(inputHash.Sum(nil))
		stats.OutputSha256 = hex.EncodeToString(outputHash.Sum(nil))
	}()
	var pending []byte
	consumeLine := func(line []byte) error {
		stats.LogicalLines++
		if !utf8.Valid(line) {
			return fail("invalid_output_utf8")
		}
		index := bytes.Index(line, []byte(diagnosticMarker))
		if index < 0 || index > 0 && line[index-1] != ' ' && line[index-1] != '\t' {
			return nil
		}
		payload := bytes.TrimSpace(line[index+len(diagnosticMarker):])
		if validateProjectionJson(payload, settings.MaximumJsonDepth) != nil {
			return fail("invalid_diagnostic_json")
		}
		var event diagnosticEventHeader
		if json.Unmarshal(payload, &event) != nil || event.Event != stats.Events+1 || event.Phase == "" {
			return fail("invalid_diagnostic_identity_or_sequence")
		}
		if stats.Events >= settings.MaximumEvents || stats.OutputBytes+int64(len(payload))+1 > settings.MaximumOutputBytes {
			return fail("output_or_event_bound")
		}
		encoded := append(append([]byte(nil), payload...), '\n')
		count, err := output.Write(encoded)
		if count > 0 {
			_, _ = outputHash.Write(encoded[:count])
			stats.OutputBytes += int64(count)
		}
		if err != nil || count != len(encoded) {
			return fail("output_write_failed")
		}
		stats.Events++
		if event.Phase == "before-apply" {
			stats.BeforeApply++
		}
		if event.Phase == "after-census" {
			stats.AfterCensus++
		}
		return nil
	}
	for {
		raw, err := reader.ReadSlice('\n')
		if settings.MaximumInputBytes+1-limited.N > settings.MaximumInputBytes {
			return stats, fail("input_byte_bound")
		}
		if len(raw) > settings.MaximumRecordBytes || err == bufio.ErrBufferFull {
			return stats, fail("record_byte_bound")
		}
		if err == io.EOF {
			if len(raw) != 0 {
				return stats, fail("truncated_input_record")
			}
			break
		}
		if err != nil {
			return stats, fail("input_read_failed")
		}
		stats.Records++
		if stats.Records > settings.MaximumRecords {
			return stats, fail("input_record_bound")
		}
		if validateProjectionJson(raw, settings.MaximumJsonDepth) != nil {
			return stats, fail("invalid_input_json")
		}
		var record testJsonRecord
		if json.Unmarshal(raw, &record) != nil || record.Action == "" {
			return stats, fail("invalid_input_record")
		}
		if record.Package != diagnosticPackage || record.Test != diagnosticTest {
			continue
		}
		if record.Action != "output" {
			if record.Action == "pass" || record.Action == "fail" || record.Action == "skip" {
				stats.TestOutcome = record.Action
				if len(pending) != 0 {
					return stats, fail("truncated_output_line")
				}
			}
			continue
		}
		stats.TargetOutputRecords++
		if record.Output == nil {
			return stats, fail("invalid_input_record")
		}
		fragment := []byte(*record.Output)
		for len(fragment) != 0 {
			end := bytes.IndexByte(fragment, '\n')
			length := len(fragment)
			if end >= 0 {
				length = end
			}
			if len(pending)+length > settings.MaximumLineBytes {
				return stats, fail("logical_line_bound")
			}
			pending = append(pending, fragment[:length]...)
			if end < 0 {
				break
			}
			if err := consumeLine(pending); err != nil {
				return stats, err
			}
			pending = pending[:0]
			fragment = fragment[end+1:]
		}
	}
	if len(pending) != 0 {
		return stats, fail("truncated_output_line")
	}
	if stats.Events == 0 {
		return stats, fail("no_diagnostic_events")
	}
	stats.InputComplete = true
	return stats, nil
}
