//go:build sidecar_load

package perfvar

// §22.8's push, config run (PROTO-M2-07) at DefaultSidecarPushCellSettings, for T5's stack: real time,
// so it builds under sidecar_load and stays out of test.sh. It writes the cell's two reports and its
// Redis CPU to SIDECAR_PUSH_REPORT_DIR, with the commit set of the monoroot SIDECAR_MONOROOT names, and
// fails unless they stand as the push gate's evidence.
//
//	SIDECAR_MONOROOT=<monoroot> SIDECAR_PUSH_REPORT_DIR=<dir> go test -tags sidecar_load \
//	    -run '^TestSidecarPushCellLoad$' -timeout 30m ./connect/perfvar

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/load"

	"github.com/urnetwork/server"
)

func TestSidecarPushCellLoad(t *testing.T) {
	monoroot, reportDir := os.Getenv("SIDECAR_MONOROOT"), os.Getenv("SIDECAR_PUSH_REPORT_DIR")
	if monoroot == "" || reportDir == "" {
		t.Fatal("SIDECAR_MONOROOT and SIDECAR_PUSH_REPORT_DIR name where the run reads its commits and writes its reports")
	}
	commitSet, err := load.ReadCommitSet(monoroot, "connect", "sidecar", "server")
	if err != nil {
		t.Fatal(err)
	}
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		server.Testing_EnableKeyspaceNotifications(ctx)
		report, err := RunSidecarPushCell(ctx, DefaultSidecarPushCellSettings())
		if err != nil {
			t.Fatal(err)
		}
		for _, each := range []*load.Report{report.Hints, report.ConfigStorm} {
			each.CommitSet = commitSet
			each.Host = host
		}
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(reportDir, "sidecar-push.json"), encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := report.GateEvidence(); err != nil {
			t.Error(err)
		}
	})
}
