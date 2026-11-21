package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadT6GatesUsesTheWholeServiceSnapshot(t *testing.T) {
	file := filepath.Join(t.TempDir(), "sidecar.yml")
	net := "0a0b0c0d-0e0f-4010-8111-121314151617"
	read := func() (int, string) {
		var out, errs bytes.Buffer
		status := run([]string{"read-t6-gates", file, net}, &out, &errs)
		return status, out.String()
	}
	if status, out := read(); status == 0 || out != "" {
		t.Fatalf("missing: %d %q", status, out)
	}
	for _, tc := range []struct{ yaml, contains string }{
		{"sidecar:\n  evs: {enabled: true, percent: 101}\n  certs: {enabled: true}\n", "evs\toff\tthe evs gate does not admit the acceptance network: not listed, and its bucket 22 is not under percent 0"},
		{"sidecar:\n  evs:\n    enabled: true\n    percent: 23\n", "evs\ton\n"},
		{"sidecar:\n  dns: {enabled: true, writes: false}\n", "dns\toff\tthe dns gate has writes: false"},
		{"sidecar:\n  certs: {enabled: true}\n", "certs\ton\n"},
		{"sidecar:\n  evs: {enabled: false}\n  restore_hold: [broken", ""},
	} {
		if err := os.WriteFile(file, []byte(tc.yaml), 0600); err != nil {
			t.Fatal(err)
		}
		status, out := read()
		if tc.contains == "" {
			if status == 0 || out != "" {
				t.Fatalf("malformed: %d %q", status, out)
			}
		} else if status != 0 || !strings.Contains(out, tc.contains) {
			t.Fatalf("got %d %q, want %q", status, out, tc.contains)
		}
	}
}
