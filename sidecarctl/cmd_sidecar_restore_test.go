// `sidecarctl sidecar restore` over a frame of the test's own (OPS-M3-03): a step runs only after the steps before it,
// end records a step run by hand, status reads only the list, and the incident's kind is recorded once.

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecarrestore"
)

// A list as the restore script leaves one for incidentId.
func testSidecarRestoreList(t *testing.T, incidentId server.Id) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "incident-"+incidentId.String()+".tsv")
	lines := "snapshot-time\tmain dump\t" + incidentId.String() + "\t2026-11-06T01:00:00Z\n" +
		"snapshot-time\tsidecar dump\t" + incidentId.String() + "\t2026-11-06T02:30:00.000001Z\n"
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Settings over a frame whose ops and api steps count their runs, in an env named test.
func testSidecarRestoreSettings(opsCount *int, apiCount *int) *sidecarRestoreSettings {
	frame := sidecarrestore.NewFrame()
	frame.Register(sidecarrestore.StepOps, func(ctx context.Context, run *sidecarrestore.StepRun) error {
		*opsCount += 1
		return nil
	})
	frame.Register(sidecarrestore.StepApi, func(ctx context.Context, run *sidecarrestore.StepRun) error {
		*apiCount += 1
		if run.Statement != "hold held by the lb" {
			return errors.New("no statement")
		}
		return nil
	})
	settings := defaultSidecarRestoreSettings()
	settings.frame = frame
	settings.requireEnv = func() (string, error) { return "test", nil }
	return settings
}

// A step waits for the steps before it, end records one run by hand, run stops where no function is registered, and
// status reads the list.
func TestSidecarRestoreRunsStepsInOrder(t *testing.T) {
	opsCount, apiCount := 0, 0
	settings := testSidecarRestoreSettings(&opsCount, &apiCount)
	incidentId := server.NewId()
	list := testSidecarRestoreList(t, incidentId)
	run := func(args ...string) (string, error) {
		out := &bytes.Buffer{}
		err := runSidecarRestoreWithSettings(t.Context(), append(args, "--incident="+incidentId.String(), "--list="+list), settings, out)
		return out.String(), err
	}

	// no kind yet
	_, err := run("ops")
	usageErr := &sidecarctlUsageError{}
	connect.AssertEqual(t, errors.As(err, &usageErr), true)
	connect.AssertEqual(t, opsCount, 0)

	_, err = run("api", "--kind=postgres")
	connect.AssertEqual(t, errors.Is(err, sidecarrestore.ErrStepOutOfOrder), true)
	_, err = run("ops")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, opsCount, 1)
	_, err = run("ops", "--kind=site")
	connect.AssertEqual(t, errors.Is(err, sidecarrestore.ErrIncidentList), true)

	_, err = run("end", "abuse", "--by-hand=no mailbox action after the dump")
	connect.AssertEqual(t, err, nil)
	_, err = run("end", "dns", "--by-hand=sidecarctl dns restore ended")
	connect.AssertEqual(t, err, nil)
	_, err = run("run", "--statement=hold held by the lb")
	connect.AssertEqual(t, errors.Is(err, sidecarrestore.ErrStepUnregistered), true)
	connect.AssertEqual(t, apiCount, 1)

	out, err := run("status")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, strings.Contains(out, "kind: postgres"), true)
	connect.AssertEqual(t, strings.Contains(out, "api        ended "), true)
	connect.AssertEqual(t, strings.Contains(out, "labels     open (by hand)"), true)
	connect.AssertEqual(t, strings.Contains(out, "hardware"), false)
}

// A command line the frame cannot take is a usage mistake, another incident's list is refused, and nothing runs outside
// an env.
func TestSidecarRestoreRefusesWhatItCannotRun(t *testing.T) {
	opsCount, apiCount := 0, 0
	settings := testSidecarRestoreSettings(&opsCount, &apiCount)
	incidentId := server.NewId()
	list := testSidecarRestoreList(t, incidentId)
	usage := func(args ...string) bool {
		err := runSidecarRestoreWithSettings(t.Context(), args, settings, &bytes.Buffer{})
		usageErr := &sidecarctlUsageError{}
		return errors.As(err, &usageErr)
	}
	connect.AssertEqual(t, usage("ops"), true)
	connect.AssertEqual(t, usage("ops", "--incident=nope", "--list="+list), true)
	connect.AssertEqual(t, usage("compact", "--incident="+incidentId.String(), "--list="+list, "--kind=postgres"), true)
	connect.AssertEqual(t, usage("end", "abuse", "--incident="+incidentId.String(), "--list="+list), true)
	connect.AssertEqual(t, usage("ops", "--by-hand=x", "--incident="+incidentId.String(), "--list="+list), true)
	connect.AssertEqual(t, usage("ops", "--kind=lunar", "--incident="+incidentId.String(), "--list="+list), true)

	// another incident's list
	err := runSidecarRestoreWithSettings(t.Context(), []string{"status", "--incident=" + server.NewId().String(), "--list=" + list}, settings, &bytes.Buffer{})
	connect.AssertEqual(t, errors.Is(err, sidecarrestore.ErrIncidentList), true)

	// outside an env nothing runs
	settings.requireEnv = func() (string, error) { return "", errors.New("WARP_ENV is not set") }
	err = runSidecarRestoreWithSettings(t.Context(), []string{"ops", "--kind=postgres", "--incident=" + incidentId.String(), "--list=" + list}, settings, &bytes.Buffer{})
	connect.AssertEqual(t, err != nil, true)
	connect.AssertEqual(t, opsCount, 0)
}

// The binary takes sidecar restore by its words.
func TestSidecarctlRegistersSidecarRestore(t *testing.T) {
	command, args := sidecarctlCommands.find([]string{"sidecar", "restore", "status"})
	if command == nil || command.words != "sidecar restore" {
		t.Fatalf("sidecar restore is not registered: %+v", command)
	}
	connect.AssertEqual(t, args, []string{"status"})
}
