package main

import (
	"testing"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026"
)

// Each step of the finished_task partition cutover parses to its option, and
// the rollback's copy limit defaults to the settings' unless given.
func TestFinishedTaskPartitionCommandParses(t *testing.T) {
	defaultLimit := server.DefaultFinishedTaskPartitionSettings().RollbackMaxCopyRows
	for _, test := range []struct {
		args        []string
		cutoverDay  time.Time
		dryRun      bool
		finalize    bool
		rollback    bool
		abort       bool
		maxCopyRows int64
	}{
		{args: []string{"--cutover-day=2026-10-27"}, cutoverDay: time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC), maxCopyRows: defaultLimit},
		{args: []string{"--cutover-day=2026-10-27", "--dry-run"}, cutoverDay: time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC), dryRun: true, maxCopyRows: defaultLimit},
		{args: []string{"--finalize"}, finalize: true, maxCopyRows: defaultLimit},
		{args: []string{"--finalize", "--dry-run"}, finalize: true, dryRun: true, maxCopyRows: defaultLimit},
		{args: []string{"--rollback"}, rollback: true, maxCopyRows: defaultLimit},
		{args: []string{"--rollback", "--max-copy-rows=900000"}, rollback: true, maxCopyRows: 900000},
		{args: []string{"--rollback", "--dry-run"}, rollback: true, dryRun: true, maxCopyRows: defaultLimit},
		{args: []string{"--abort"}, abort: true, maxCopyRows: defaultLimit},
	} {
		opts, err := docopt.ParseArgs(bringyourctlUsage, append([]string{"model", "migrate", "finished-task-partition"}, test.args...), "synthetic")
		if err != nil {
			t.Fatal(test.args, err)
		}
		if partition, _ := opts.Bool("finished-task-partition"); !partition {
			t.Fatal(test.args, "did not parse as the finished_task partition command")
		}
		options, err := finishedTaskPartitionMigrateOptions(opts)
		if err != nil {
			t.Fatal(test.args, err)
		}
		if !options.CutoverDay.Equal(test.cutoverDay) || options.DryRun != test.dryRun || options.Finalize != test.finalize ||
			options.Rollback != test.rollback || options.Abort != test.abort || options.Settings == nil ||
			options.Settings.RollbackMaxCopyRows != test.maxCopyRows {
			t.Fatal(test.args, options)
		}
	}
	for _, args := range [][]string{
		{"--cutover-day=10/27/2026"},
		{"--rollback", "--max-copy-rows=many"},
		{"--rollback", "--max-copy-rows=-1"},
	} {
		opts, err := docopt.ParseArgs(bringyourctlUsage, append([]string{"model", "migrate", "finished-task-partition"}, args...), "synthetic")
		if err != nil {
			t.Fatal(args, err)
		}
		if _, err := finishedTaskPartitionMigrateOptions(opts); err == nil {
			t.Fatal(args, "parsed")
		}
	}
}
