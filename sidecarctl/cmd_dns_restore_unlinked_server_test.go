//go:build !sidecar_pending

// `sidecarctl dns restoring` and `sidecarctl dns restore` in a binary that does not link MG5's dns tables, which is
// every binary until the group is registered: each refuses and changes nothing. Needs the private test stack,
// through .stack/server-test.sh, for the env's config and Redis.

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Both commands exit 1 naming the unlinked tables, and print no change.
func TestDnsRestoreCommandsRefuseWithoutTheDnsTables(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		server.Testing_EnableKeyspaceNotifications(context.Background())
		defer server.Config.PushSimpleResource("dns.yml", []byte("roots: [restore-command.example]\nns: [ns1, ns2]\nsignatures_per_second: 500\nrrl: {responses_per_second: 20}\ntcp: {connections_per_source: 8}\n"))()
		for _, args := range [][]string{
			{"dns", "restoring", "on"},
			{"dns", "restoring", "off"},
			{"dns", "restore", "--incident=" + server.NewId().String()},
		} {
			stdout := &bytes.Buffer{}
			stderr := &bytes.Buffer{}
			if code := sidecarctlCommands.run(args, stdout, stderr); code != 1 || !strings.Contains(stderr.String(), "dns tables are not linked in this binary") {
				tb.Errorf("%q: exited %d with %q, want 1 and the unlinked tables", args, code, stderr.String())
			}
			if strings.Contains(stderr.String(), "rerun") || strings.Contains(stdout.String(), "restoring is") || strings.Contains(stdout.String(), "done:") {
				tb.Errorf("%q: printed %q", args, stdout.String())
			}
		}
	})
}
