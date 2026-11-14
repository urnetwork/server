package server

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

type jsonlPrivacyCancelledReader struct {
	raw []byte
}

func (self *jsonlPrivacyCancelledReader) Read(destination []byte) (int, error) {
	n := copy(destination, self.raw)
	self.raw = self.raw[n:]
	if len(self.raw) == 0 {
		return n, context.Canceled
	}
	return n, nil
}

// A canceled source must flush already-read carry without retrying its read.
func TestJsonlPrivacyCancelledReadFlushesAlreadyAcceptedCarry(t *testing.T) {
	raw := strings.Repeat("x", scrubMaxLineSize-3) + "192.0.2.23"
	var captured bytes.Buffer
	scrubLoop(&jsonlPrivacyCancelledReader{raw: []byte(raw)}, &captured)
	expected := strings.Repeat("x", scrubMaxLineSize-3) + "[scrubbed]"
	if captured.String() != expected {
		t.Fatalf("canceled read lost or leaked already-accepted address carry: captured_bytes=%d expected_bytes=%d", captured.Len(), len(expected))
	}
}
