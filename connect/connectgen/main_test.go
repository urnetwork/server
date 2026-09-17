package main

import (
	"os"
	"strings"
	"testing"
)

// The generated matrix must be what this generator produces right now.
//
// It is the only thing keeping the two in step: the file is 174 ordinary tests
// that a reader can grep and -run, and nothing stops someone editing one by
// hand. This turns that into a failure instead of a divergence nobody notices.
func TestGeneratedMatrixIsCurrent(t *testing.T) {
	want, count, err := generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	got, err := os.ReadFile(generatedPath)
	if err != nil {
		t.Fatalf("read %s: %v", generatedPath, err)
	}
	if string(got) != string(want) {
		t.Fatalf(
			"%s is stale (%d cases expected): re-run `go generate ./connectgen`",
			generatedPath, count,
		)
	}
}

// The matrix is a cross product, so its size is arithmetic rather than a
// number to keep up to date by hand. A base variant that loses its extender
// twin, or an axis that silently stops multiplying, shows up here.
func TestMatrixSizeIsTheCrossProduct(t *testing.T) {
	extenderCapable := 0
	for _, base := range baseCases {
		if base.extenderCapable() {
			extenderCapable += 1
		}
	}
	direct := len(baseCases) * len(encryptionModes)
	withExtender := extenderCapable * len(encryptionModes)

	_, count, err := generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if count != direct+withExtender {
		t.Fatalf("generated %d cases, want %d direct + %d extender", count, direct, withExtender)
	}
	if direct != 93 {
		t.Errorf("the direct matrix is %d cases, want the 93 it replaced", direct)
	}
}

// Every variant has an extender twin, h3 included. That is only true because
// an extender relays datagrams as well as streams; before that an h3 carrier
// had no extender path at all.
func TestEveryVariantHasAnExtenderTwin(t *testing.T) {
	for _, base := range baseCases {
		if !base.extenderCapable() {
			t.Errorf("%s (mode %q) has no extender twin", base.name, base.mode)
		}
	}
}

// Every base variant carries the name its hand-written wrapper had, so the
// existing 93 tests keep their names through the move.
func TestBaseNamesAreUniqueAndConnectPrefixed(t *testing.T) {
	seen := map[string]bool{}
	for _, base := range baseCases {
		if !strings.HasPrefix(base.name, "Connect") {
			t.Errorf("base case %q does not start with Connect", base.name)
		}
		if seen[base.name] {
			t.Errorf("duplicate base case %q", base.name)
		}
		seen[base.name] = true
	}
	if len(seen) != 31 {
		t.Errorf("%d base variants, want the 31 the matrix had", len(seen))
	}
}
