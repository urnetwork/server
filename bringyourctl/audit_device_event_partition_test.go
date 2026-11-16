package main

import (
	"testing"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
)

// Each step of the audit_device_event cutover parses to its option, and the
// rollback's copy limit defaults to the settings' unless given.
func TestAuditDeviceEventPartitionCommandParses(t *testing.T) {
	defaultLimit := server.DefaultAuditDeviceEventPartitionSettings().RollbackMaxCopyRows
	for _, test := range []struct {
		args        []string
		cutoverDay  time.Time
		dryRun      bool
		finalize    bool
		rollback    bool
		abort       bool
		maxCopyRows int64
	}{
		{args: []string{"--cutover-day=2026-11-30"}, cutoverDay: time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC), maxCopyRows: defaultLimit},
		{args: []string{"--cutover-day=2026-11-30", "--dry-run"}, cutoverDay: time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC), dryRun: true, maxCopyRows: defaultLimit},
		{args: []string{"--finalize"}, finalize: true, maxCopyRows: defaultLimit},
		{args: []string{"--rollback", "--max-copy-rows=5000000"}, rollback: true, maxCopyRows: 5000000},
		{args: []string{"--abort", "--dry-run"}, abort: true, dryRun: true, maxCopyRows: defaultLimit},
	} {
		opts, err := docopt.ParseArgs(bringyourctlUsage, append([]string{"model", "migrate", "audit-device-event-partition"}, test.args...), "synthetic")
		if err != nil {
			t.Fatal(test.args, err)
		}
		if partition, _ := opts.Bool("audit-device-event-partition"); !partition {
			t.Fatal(test.args, "did not parse as the audit_device_event partition command")
		}
		options, err := auditDeviceEventPartitionMigrateOptions(opts)
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
		{"--cutover-day=11/30/2026"},
		{"--rollback", "--max-copy-rows=-1"},
	} {
		opts, err := docopt.ParseArgs(bringyourctlUsage, append([]string{"model", "migrate", "audit-device-event-partition"}, args...), "synthetic")
		if err != nil {
			t.Fatal(args, err)
		}
		if _, err := auditDeviceEventPartitionMigrateOptions(opts); err == nil {
			t.Fatal(args, "parsed")
		}
	}
}
