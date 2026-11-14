package server

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// A fixed reader makes the bounded flush split the address after its first three bytes.
// This is a privacy qualification control, not a claim about live pipe scheduling.
func TestJsonlPrivacyAddressCrossingBoundedFlush(t *testing.T) {
	prefix := `{"evidence":"`
	address := "192.0.2.23"
	raw := prefix + strings.Repeat("x", scrubMaxLineSize-len(prefix)-3) + address + `"}` + "\n"
	var captured bytes.Buffer
	scrubLoop(strings.NewReader(raw), &captured)
	expected := strings.Replace(raw, address, "[scrubbed]", 1)
	if captured.String() != expected {
		t.Fatalf("bounded-flush address privacy failed: captured_bytes=%d expected_bytes=%d documentation_address_retained=%t", captured.Len(), len(expected), strings.Contains(captured.String(), address))
	}
}

func TestJsonlPrivacyAddressWithinOrdinaryRecord(t *testing.T) {
	raw := `{"evidence":"synthetic peer 192.0.2.23; peer 2001:db8::7"}` + "\n"
	var captured bytes.Buffer
	scrubLoop(strings.NewReader(raw), &captured)
	expected := `{"evidence":"synthetic peer [scrubbed]; peer [scrubbed]"}` + "\n"
	if captured.String() != expected {
		t.Fatalf("ordinary record privacy mismatch: captured_bytes=%d expected_bytes=%d", captured.Len(), len(expected))
	}
}

func TestJsonlPrivacyEveryAddressSplitPreservesPortAndBracket(t *testing.T) {
	for _, token := range []string{"192.0.2.23", "198.51.100.7:443", "2001:db8::7", "[2001:db8::7]:443"} {
		for split := 1; split < len(token); split++ {
			raw := strings.Repeat("x", scrubMaxLineSize-split) + token + " synthetic tail\n"
			var captured bytes.Buffer
			scrubLoop(strings.NewReader(raw), &captured)
			expected := string(scrubAddrs([]byte(raw)))
			if captured.String() != expected {
				t.Fatalf("address split mismatch: token_bytes=%d split=%d captured_bytes=%d expected_bytes=%d", len(token), split, captured.Len(), len(expected))
			}
		}
	}
}

func TestJsonlPrivacyOversizedTokenRedactsOnceAndKeepsSibling(t *testing.T) {
	token := strings.Repeat("f", 4*scrubMaxLineSize)
	for _, brackets := range []bool{false, true} {
		raw := "synthetic " + token + " tail\npeer 192.0.2.23\n"
		if brackets {
			raw = "synthetic [" + token + "]:443 tail\npeer 192.0.2.23\n"
		}
		var captured bytes.Buffer
		scrubLoop(strings.NewReader(raw), &captured)
		expected := "synthetic [scrubbed] tail\npeer [scrubbed]\n"
		if brackets {
			expected = "synthetic [scrubbed]:443 tail\npeer [scrubbed]\n"
		}
		if captured.String() != expected {
			t.Fatalf("oversized token redaction lost delimiter or sibling: bracketed=%t captured_bytes=%d expected_bytes=%d", brackets, captured.Len(), len(expected))
		}
	}
}

func TestJsonlPrivacyLongNonAddressRecordKeepsAllBytes(t *testing.T) {
	raw := strings.Repeat("x", 4*scrubMaxLineSize) + " synthetic tail"
	var captured bytes.Buffer
	scrubLoop(strings.NewReader(raw), &captured)
	if captured.String() != raw {
		t.Fatalf("non-address long record changed: captured_bytes=%d expected_bytes=%d", captured.Len(), len(raw))
	}
}

type jsonlPrivacyTerminalReader struct {
	raw []byte
}

func (self *jsonlPrivacyTerminalReader) Read(destination []byte) (int, error) {
	n := copy(destination, self.raw)
	self.raw = self.raw[n:]
	if len(self.raw) == 0 {
		return n, io.ErrUnexpectedEOF
	}
	return n, nil
}

func TestJsonlPrivacyFinalDataAndReadErrorFlushCarry(t *testing.T) {
	raw := strings.Repeat("x", scrubMaxLineSize-3) + "192.0.2.23"
	var captured bytes.Buffer
	scrubLoop(&jsonlPrivacyTerminalReader{raw: []byte(raw)}, &captured)
	expected := strings.Repeat("x", scrubMaxLineSize-3) + "[scrubbed]"
	if captured.String() != expected {
		t.Fatalf("terminal read error lost or leaked address carry: captured_bytes=%d expected_bytes=%d", captured.Len(), len(expected))
	}
}

type jsonlPrivacyRefusingWriter struct {
	calls int
}

func (self *jsonlPrivacyRefusingWriter) Write(raw []byte) (int, error) {
	self.calls++
	return 0, nil
}

func TestJsonlPrivacyShortDestinationWriteStopsForwarding(t *testing.T) {
	source := strings.NewReader(strings.Repeat("x", 4*scrubMaxLineSize))
	captured := &jsonlPrivacyRefusingWriter{}
	scrubLoop(source, captured)
	if captured.calls != 1 || source.Len() != 3*scrubMaxLineSize {
		t.Fatalf("destination refusal retried or consumed later source: calls=%d remaining_bytes=%d", captured.calls, source.Len())
	}
}

func TestJsonlPrivacyLongCrashPassthroughRetainsExplicitException(t *testing.T) {
	raw := "panic: synthetic dump " + strings.Repeat("x", scrubMaxLineSize) + " 192.0.2.23\n"
	var captured bytes.Buffer
	scrubLoop(strings.NewReader(raw), &captured)
	notice := "[scrub] crash output detected; log scrubbing disabled for this process\n"
	if captured.String() != notice+raw {
		t.Fatalf("explicit crash exception changed: captured_bytes=%d expected_bytes=%d", captured.Len(), len(notice+raw))
	}
}
