package main

import (
	"os"
	"strings"
	"testing"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/evidencemove"
)

// The move's command line parses to the defaults overridden by each option;
// a run that removes objects needs its checkpoint, and values the move
// refuses are refused before anything is read.
func TestEvidenceMoveFlatCommandParses(t *testing.T) {
	defaults := evidencemove.DefaultSettings()
	for _, test := range []struct {
		args       []string
		checkpoint string
		journal    string
		check      func(settings evidencemove.Settings) bool
	}{
		{
			args:       []string{"--checkpoint=/tmp/move.json"},
			checkpoint: "/tmp/move.json",
			check: func(settings evidencemove.Settings) bool {
				return settings == defaults
			},
		},
		{
			args:       []string{"--checkpoint=/tmp/move.json", "--journal=/tmp/journal.jsonl", "--objects-per-second=50", "--workers=8", "--batch-rows=200", "--max-moves=10000"},
			checkpoint: "/tmp/move.json",
			journal:    "/tmp/journal.jsonl",
			check: func(settings evidencemove.Settings) bool {
				return settings.ObjectsPerSecond == 50 && settings.Workers == 8 && settings.BatchRows == 200 && settings.MaxMoves == 10000 && !settings.DryRun
			},
		},
		{
			args: []string{"--dry-run", "--max-moves=500"},
			check: func(settings evidencemove.Settings) bool {
				return settings.DryRun && settings.MaxMoves == 500
			},
		},
	} {
		opts, err := docopt.ParseArgs(bringyourctlUsage, append([]string{"evidence", "move-flat"}, test.args...), "synthetic")
		if err != nil {
			t.Fatalf("%v: %v", test.args, err)
		}
		if command, _ := opts.Bool("move-flat"); !command {
			t.Fatalf("%v: not the move command", test.args)
		}
		settings, checkpoint, journal, err := evidenceMoveFlatOptions(opts)
		if err != nil {
			t.Fatalf("%v: %v", test.args, err)
		}
		if checkpoint != test.checkpoint || journal != test.journal || !test.check(settings) {
			t.Fatalf("%v: %+v %q %q", test.args, settings, checkpoint, journal)
		}
	}
	for _, args := range [][]string{
		{},
		{"--max-moves=10"},
		{"--checkpoint=/tmp/move.json", "--objects-per-second=0"},
		{"--checkpoint=/tmp/move.json", "--objects-per-second=fast"},
		{"--checkpoint=/tmp/move.json", "--workers=0"},
		{"--checkpoint=/tmp/move.json", "--workers=many"},
		{"--checkpoint=/tmp/move.json", "--batch-rows=0"},
		{"--checkpoint=/tmp/move.json", "--max-moves=-1"},
	} {
		opts, err := docopt.ParseArgs(bringyourctlUsage, append([]string{"evidence", "move-flat"}, args...), "synthetic")
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if _, _, _, err := evidenceMoveFlatOptions(opts); err == nil {
			t.Fatalf("%v was accepted", args)
		}
	}
	for _, args := range [][]string{
		{"evidence", "move-flat-verify", "--journal=/tmp/journal.jsonl", "--checkpoint=/tmp/move.json", "--fallback-sample=10"},
		{"evidence", "flat-census", "--sample-rows=100"},
	} {
		if _, err := docopt.ParseArgs(bringyourctlUsage, args, "synthetic"); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

// Pure: every evidence command makes its process read only before it opens a
// database session, so PostgreSQL refuses any write it might attempt, and
// main runs each command.
func TestEvidenceCommandsAreReadOnly(t *testing.T) {
	source, err := os.ReadFile("evidence_move_flat.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, command := range []struct {
		function string
		// the first database read of the command
		read string
	}{
		{"func evidenceMoveFlatCommand(", "mover.Run(ctx)"},
		{"func evidenceMoveFlatVerifyCommand(", "ClientKeyHistorySource{}).Next("},
		{"func evidenceFlatCensusCommand(", "ClientKeyHistoryRowEstimate("},
		{"func evidenceSnapshotHashesCommand(", "evidencemove.ExportSnapshotPages("},
	} {
		start := strings.Index(text, command.function)
		if start < 0 {
			t.Fatalf("%s is missing", command.function)
		}
		body := text[start:]
		if end := strings.Index(body[1:], "\nfunc "); 0 <= end {
			body = body[:end+1]
		}
		readOnly := strings.Index(body, "evidenceReadOnlyDb(")
		read := strings.Index(body, command.read)
		if readOnly < 0 || read < 0 || read < readOnly {
			t.Fatalf("%s reads the database before its process is read only", command.function)
		}
	}
	if !strings.Contains(text, "server.RequirePgReadOnly()") {
		t.Fatal("evidenceReadOnlyDb does not make the process read only")
	}
	main, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []string{"evidenceMoveFlatCommand(opts)", "evidenceMoveFlatVerifyCommand(opts)", "evidenceFlatCensusCommand(opts)", "evidenceSnapshotHashesCommand(opts)"} {
		if !strings.Contains(string(main), call) {
			t.Fatalf("main does not run %s", call)
		}
	}
}

// The snapshot's command line parses to the defaults overridden by each
// option (the usage requires --out), and refused values are refused before
// anything is read. move-flat takes a snapshot that exists as its source.
func TestEvidenceSnapshotCommandParses(t *testing.T) {
	defaults := evidencemove.DefaultSnapshotSettings()
	for _, test := range []struct {
		args           []string
		pagesPerSecond float64
		batchPages     int
	}{
		{args: []string{"--out=/tmp/hashes.bin"}, pagesPerSecond: defaults.PagesPerSecond, batchPages: defaults.BatchPages},
		{args: []string{"--out=/tmp/hashes.bin", "--pages-per-second=4000", "--batch-pages=2000"}, pagesPerSecond: 4000, batchPages: 2000},
	} {
		opts, err := docopt.ParseArgs(bringyourctlUsage, append([]string{"evidence", "snapshot-hashes"}, test.args...), "synthetic")
		if err != nil {
			t.Fatalf("%v: %v", test.args, err)
		}
		settings, out, err := evidenceSnapshotOptions(opts)
		if err != nil || out != "/tmp/hashes.bin" || settings.PagesPerSecond != test.pagesPerSecond || settings.BatchPages != test.batchPages {
			t.Fatalf("%v: %+v %q %v", test.args, settings, out, err)
		}
	}
	for _, args := range [][]string{
		{"--out=/tmp/hashes.bin", "--pages-per-second=0"},
		{"--out=/tmp/hashes.bin", "--pages-per-second=fast"},
		{"--out=/tmp/hashes.bin", "--batch-pages=0"},
		{"--out=/tmp/hashes.bin", "--batch-pages=100000"},
	} {
		opts, err := docopt.ParseArgs(bringyourctlUsage, append([]string{"evidence", "snapshot-hashes"}, args...), "synthetic")
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if _, _, err := evidenceSnapshotOptions(opts); err == nil {
			t.Fatalf("%v was accepted", args)
		}
	}
	snapshot := t.TempDir() + "/hashes.bin"
	if err := os.WriteFile(snapshot, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for snapshotPath, valid := range map[string]bool{snapshot: true, snapshot + ".missing": false} {
		opts, err := docopt.ParseArgs(bringyourctlUsage, []string{"evidence", "move-flat", "--checkpoint=/tmp/move.json", "--snapshot=" + snapshotPath}, "synthetic")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := evidenceMoveFlatOptions(opts); (err == nil) != valid {
			t.Fatalf("--snapshot=%s gave %v", snapshotPath, err)
		}
	}
}
