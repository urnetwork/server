// Synthetic test-json exercises real chunk reassembly and all refusal bounds;
// no captured log, production identity, native process or database is required.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Encode the real Go test-json field shape, with visibly synthetic content.
func encodeProjectionRecord(t *testing.T, action, pkg, test, output string) []byte {
	t.Helper()
	content, err := json.Marshal(testJsonRecord{Action: action, Package: pkg, Test: test, Output: &output})
	if err != nil {
		t.Fatal(err)
	}
	return append(content, '\n')
}

// Each independently valid record may contain only part of one logical line.
func projectionLog(t *testing.T, fragments ...string) []byte {
	t.Helper()
	var result []byte
	for _, fragment := range fragments {
		result = append(result, encodeProjectionRecord(t, "output", diagnosticPackage, diagnosticTest, fragment)...)
	}
	return result
}

// One closed public-source marker with no live identities or private values.
func projectionEvent(id int, phase, extra string) string {
	content, _ := json.Marshal(struct {
		Event int    `json:"event"`
		Phase string `json:"phase"`
		Extra string `json:"synthetic_extra"`
	}{Event: id, Phase: phase, Extra: extra})
	return "    synthetic_test.go:12: " + diagnosticMarker + string(content) + "\n"
}

// Error classifications must be exact, never accepted timeout/setup failures.
func requireProjectionCode(t *testing.T, err error, expected string) {
	t.Helper()
	var actual *projectionError
	if !errors.As(err, &actual) || actual.code != expected {
		t.Fatalf("expected %s, got %v", expected, err)
	}
}

// Original per-record parsing is causally red on a 1-KiB split, while the
// streaming projection preserves one complete event and every long-string byte.
func TestProjectionReassemblesOneKiBAndMultipleChunks(t *testing.T) {
	line := projectionEvent(1, "before-apply", strings.Repeat("synthetic-", 400))
	fragments := []string{line[:1024], line[1024:2048], line[2048:]}
	broken := strings.SplitN(fragments[0], diagnosticMarker, 2)[1]
	if json.Valid([]byte(broken)) {
		t.Fatal("old per-record parser control did not reproduce incomplete JSON")
	}
	var output bytes.Buffer
	stats, err := projectDiagnostic(bytes.NewReader(projectionLog(t, fragments...)), &output, defaultProjectionSettings())
	if err != nil || stats.Events != 1 || stats.TargetOutputRecords != 3 || !stats.InputComplete || !json.Valid(bytes.TrimSpace(output.Bytes())) || !bytes.Contains(output.Bytes(), []byte(strings.Repeat("synthetic-", 400))) {
		t.Fatal("chunked logical line was not preserved", stats, err)
	}
}

// Unrelated records and non-output records cannot interrupt a pending line.
func TestProjectionMixedRecordsKeepExactTestOwnership(t *testing.T) {
	line := projectionEvent(1, "before-apply", "synthetic")
	input := projectionLog(t, line[:45])
	input = append(input, encodeProjectionRecord(t, "run", diagnosticPackage, diagnosticTest, "")...)
	input = append(input, encodeProjectionRecord(t, "output", diagnosticPackage, "TestSyntheticOther", projectionEvent(999, "after-census", "ignored"))...)
	input = append(input, encodeProjectionRecord(t, "output", "example.invalid/other", diagnosticTest, "ignored\n")...)
	input = append(input, projectionLog(t, line[45:])...)
	input = append(input, encodeProjectionRecord(t, "fail", diagnosticPackage, diagnosticTest, "")...)
	var output bytes.Buffer
	stats, err := projectDiagnostic(bytes.NewReader(input), &output, defaultProjectionSettings())
	if err != nil || stats.Events != 1 || stats.BeforeApply != 1 || stats.TestOutcome != "fail" || bytes.Contains(output.Bytes(), []byte("ignored")) {
		t.Fatal("projection mixed roles or converted original failure into success", stats, err)
	}
}

// A fragmented reader can split inside a multibyte rune or JSON escape; only
// complete encoded records and logical lines reach UTF-8 validation.
type oneByteProjectionReader struct{ input []byte }

func (self *oneByteProjectionReader) Read(buffer []byte) (int, error) {
	if len(self.input) == 0 {
		return 0, io.EOF
	}
	buffer[0] = self.input[0]
	self.input = self.input[1:]
	return 1, nil
}

func TestProjectionPreservesUtf8AndJsonEscapes(t *testing.T) {
	line := projectionEvent(1, "observation-start", "synthetic-λ雪\"\\\n")
	input := projectionLog(t, line[:30], line[30:])
	var output bytes.Buffer
	stats, err := projectDiagnostic(&oneByteProjectionReader{input: input}, &output, defaultProjectionSettings())
	if err != nil || stats.Events != 1 || !bytes.Contains(output.Bytes(), []byte("λ雪")) {
		t.Fatal("UTF-8/escaped metadata changed across transport boundaries", stats, err)
	}
}

func TestProjectionRejectsTruncatedOuterJsonAndLastLine(t *testing.T) {
	for _, input := range [][]byte{
		[]byte(`{"Action":"output"`),
		bytes.TrimSuffix(projectionLog(t, projectionEvent(1, "before-apply", "synthetic")), []byte("\n")),
	} {
		_, err := projectDiagnostic(bytes.NewReader(input), io.Discard, defaultProjectionSettings())
		requireProjectionCode(t, err, "truncated_input_record")
	}
	input := projectionLog(t, strings.TrimSuffix(projectionEvent(1, "before-apply", "synthetic"), "\n"))
	_, err := projectDiagnostic(bytes.NewReader(input), io.Discard, defaultProjectionSettings())
	requireProjectionCode(t, err, "truncated_output_line")
}

func TestProjectionRejectsInvalidAndDuplicateJson(t *testing.T) {
	for _, input := range [][]byte{
		[]byte("{invalid}\n"),
		[]byte("{\"Action\":\"output\",\"Action\":\"output\"}\n"),
		[]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}', '\n'},
	} {
		_, err := projectDiagnostic(bytes.NewReader(input), io.Discard, defaultProjectionSettings())
		requireProjectionCode(t, err, "invalid_input_json")
	}
	_, err := projectDiagnostic(bytes.NewReader(projectionLog(t, diagnosticMarker+"{invalid}\n")), io.Discard, defaultProjectionSettings())
	requireProjectionCode(t, err, "invalid_diagnostic_json")
}

func TestProjectionRequiresExactMarkerAndContiguousEvents(t *testing.T) {
	input := projectionLog(t, "MIGRATION_CENSUS_SESSION_DIAGNOSTIC_EXTRA {}\n", "NOT_"+diagnosticMarker+"{}\n", projectionEvent(1, "before-apply", "synthetic"))
	stats, err := projectDiagnostic(bytes.NewReader(input), io.Discard, defaultProjectionSettings())
	if err != nil || stats.Events != 1 {
		t.Fatal("marker lookalike entered the event stream", stats, err)
	}
	_, err = projectDiagnostic(bytes.NewReader(projectionLog(t, projectionEvent(2, "after-census", "synthetic"))), io.Discard, defaultProjectionSettings())
	requireProjectionCode(t, err, "invalid_diagnostic_identity_or_sequence")
}

func TestProjectionAllBoundsRefuseWithoutSilentTruncation(t *testing.T) {
	input := projectionLog(t, projectionEvent(1, "before-apply", "synthetic"), projectionEvent(2, "after-census", "synthetic"))
	cases := []struct {
		code   string
		change func(*projectionSettings)
	}{
		{code: "input_byte_bound", change: func(settings *projectionSettings) { settings.MaximumInputBytes = 8 }},
		{code: "record_byte_bound", change: func(settings *projectionSettings) { settings.MaximumRecordBytes = 8 }},
		{code: "logical_line_bound", change: func(settings *projectionSettings) { settings.MaximumLineBytes = 8 }},
		{code: "input_record_bound", change: func(settings *projectionSettings) { settings.MaximumRecords = 1 }},
		{code: "output_or_event_bound", change: func(settings *projectionSettings) { settings.MaximumEvents = 1 }},
		{code: "output_or_event_bound", change: func(settings *projectionSettings) { settings.MaximumOutputBytes = 8 }},
	}
	for _, c := range cases {
		settings := defaultProjectionSettings()
		c.change(&settings)
		stats, err := projectDiagnostic(bytes.NewReader(input), io.Discard, settings)
		requireProjectionCode(t, err, c.code)
		if stats.InputComplete {
			t.Fatal("bounded failure asserted complete input")
		}
	}
}

func TestProjectionPreservesValidPrefixOnLaterCorruption(t *testing.T) {
	input := projectionLog(t, projectionEvent(1, "before-apply", "synthetic"))
	input = append(input, []byte("{bad}\n")...)
	var output bytes.Buffer
	stats, err := projectDiagnostic(bytes.NewReader(input), &output, defaultProjectionSettings())
	requireProjectionCode(t, err, "invalid_input_json")
	if stats.Events != 1 || stats.InputComplete || !json.Valid(bytes.TrimSpace(output.Bytes())) {
		t.Fatal("failed projection lost or misrepresented its valid prefix", stats)
	}
}

func TestProjectionRefusesNullOutputMissingEventsAndInvalidSettings(t *testing.T) {
	input, err := json.Marshal(map[string]any{"Action": "output", "Package": diagnosticPackage, "Test": diagnosticTest, "Output": nil})
	if err != nil {
		t.Fatal(err)
	}
	_, err = projectDiagnostic(bytes.NewReader(append(input, '\n')), io.Discard, defaultProjectionSettings())
	requireProjectionCode(t, err, "invalid_input_record")
	_, err = projectDiagnostic(bytes.NewReader(projectionLog(t, "synthetic non-diagnostic line\n")), io.Discard, defaultProjectionSettings())
	requireProjectionCode(t, err, "no_diagnostic_events")
	settings := defaultProjectionSettings()
	settings.MaximumLineBytes = 0
	_, err = projectDiagnostic(bytes.NewReader(nil), io.Discard, settings)
	requireProjectionCode(t, err, "invalid_settings")
}

// A short write cannot be published as one complete projected event.
type shortProjectionWriter struct{}

func (self shortProjectionWriter) Write(content []byte) (int, error) { return 1, nil }

func TestProjectionRejectsShortOutputAndExcessiveJsonDepth(t *testing.T) {
	_, err := projectDiagnostic(bytes.NewReader(projectionLog(t, projectionEvent(1, "before-apply", "synthetic"))), shortProjectionWriter{}, defaultProjectionSettings())
	requireProjectionCode(t, err, "output_write_failed")
	settings := defaultProjectionSettings()
	settings.MaximumJsonDepth = 1
	line := diagnosticMarker + `{"event":1,"phase":"before-apply","nested":[{"synthetic":"value"}]}` + "\n"
	_, err = projectDiagnostic(bytes.NewReader(projectionLog(t, line)), io.Discard, settings)
	requireProjectionCode(t, err, "invalid_diagnostic_json")
}

// Receipt semantics retain a valid prefix but never equate parse failure with
// complete projection or with the independently recorded native test outcome.
func TestProjectionFailureReceiptRetainsIndependentStatus(t *testing.T) {
	directory := t.TempDir()
	input := filepath.Join(directory, "synthetic-corrupt.jsonl")
	events := filepath.Join(directory, "partial-events.jsonl")
	receiptPath := filepath.Join(directory, "failure-receipt.json")
	content := append(projectionLog(t, projectionEvent(1, "before-apply", "synthetic")), []byte("{bad}\n")...)
	if err := os.WriteFile(input, content, 0600); err != nil {
		t.Fatal(err)
	}
	if err := runProjection(input, events, receiptPath); err == nil {
		t.Fatal("corrupt source was called a complete projection")
	}
	encoded, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	var receipt projectionReceipt
	if json.Unmarshal(encoded, &receipt) != nil || receipt.ProjectionComplete || receipt.ErrorCode != "invalid_input_json" || receipt.Stats.Events != 1 {
		t.Fatal("partial projection receipt lost its exact independent failure")
	}
}

// New outputs are exclusive and the actual input may never be a symlink alias.
func TestProjectionFilesPreserveExistingEvidenceAndRejectLinks(t *testing.T) {
	directory := t.TempDir()
	input := filepath.Join(directory, "synthetic-input.jsonl")
	events := filepath.Join(directory, "new-events.jsonl")
	receipt := filepath.Join(directory, "new-receipt.json")
	if err := os.WriteFile(input, projectionLog(t, projectionEvent(1, "before-apply", "synthetic")), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runProjection(input, events, receipt); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(events)
	if err != nil {
		t.Fatal(err)
	}
	if err := runProjection(input, events, receipt); err == nil {
		t.Fatal("existing projection evidence was silently reused")
	}
	after, err := os.ReadFile(events)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("replay overwrote original projected bytes")
	}
	link := filepath.Join(directory, "input-link")
	if err := os.Symlink(input, link); err != nil {
		t.Fatal(err)
	}
	if file, err := openProjectionInput(link); err == nil {
		file.Close()
		t.Fatal("input symlink was accepted")
	}
}
