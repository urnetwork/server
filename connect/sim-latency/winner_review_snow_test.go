// Durable model intent prevents retries after ambiguous provider execution.
package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The exact production journal operation must refuse a second intent even when
// the requested content changes; a crash is not a fresh review opportunity.
func TestWinnerSnowMemberJournalCannotReroll(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := persistWinnerSnowMember(directory, 1, "started", map[string]any{"schema": 1, "model": "synthetic-model"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(directory, "member-01.started.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := persistWinnerSnowMember(directory, 1, "started", map[string]any{"schema": 1, "model": "replacement-model"}); err == nil {
		t.Fatal("ambiguous model call was rerolled")
	}
	after, err := os.ReadFile(filepath.Join(directory, "member-01.started.json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("original durable intent changed")
	}
}

// Journal directory permissions are an authority boundary, not a convenience.
func TestWinnerSnowMemberJournalRequiresPrivateDirectory(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0755); err != nil {
		t.Fatal(err)
	}
	if err := persistWinnerSnowMember(directory, 1, "started", struct{}{}); err == nil {
		t.Fatal("public state directory accepted")
	}
}

// A member cannot escape the fixed six-leaf journal census.
func TestWinnerSnowMemberJournalRejectsUnknownIdentity(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{0, 4} {
		if err := persistWinnerSnowMember(directory, index, "started", struct{}{}); err == nil {
			t.Fatal("unknown member accepted")
		}
	}
	if err := persistWinnerSnowMember(directory, 1, "../escape", struct{}{}); err == nil {
		t.Fatal("unknown journal phase accepted")
	}
}
