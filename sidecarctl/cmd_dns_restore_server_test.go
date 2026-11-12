//go:build sidecar_pending

// `sidecarctl dns restoring` and `sidecarctl dns restore` against the env as server reaches it: dns.yml through
// server's config, and the step over dnshost.Store on Postgres and Redis. The dns tables are MG5's, so these build
// under sidecar_pending, and they need the private test stack, through .stack/server-test.sh.

package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/dnshost"
	"github.com/urnetwork/server/sidecarhost"
)

// dns.yml for one root with two NS and no glue, so no SOA read dials anything: each NS reads as not answering.
const dnsRestoreServerTestYaml = `roots: [restore-command.example]
ns: [ns1, ns2]
signatures_per_second: 500
rrl: {responses_per_second: 20}
tcp: {connections_per_source: 8}
`

// The root dnsRestoreServerTestYaml serves.
const dnsRestoreServerTestRoot = "restore-command.example."

// Writes through to a buffer and cancels a run once the step logs that its restore transaction committed, so a
// test stops the step at that point with no clock: what follows is the wait for NS that never answer.
type dnsRestoreCancelWriter struct {
	buffer bytes.Buffer
	cancel context.CancelFunc
}

// See io.Writer.
func (self *dnsRestoreCancelWriter) Write(data []byte) (int, error) {
	if strings.Contains(string(data), "lands at serial") {
		self.cancel()
	}
	return self.buffer.Write(data)
}

// Runs one of the binary's own dns commands under ctx, and returns its exit code and what it printed.
func runServerDnsRestoreCommand(ctx context.Context, out io.Writer, args ...string) (int, string) {
	commands := newSidecarctlCommandSet()
	commands.add(&sidecarctlCommand{words: "dns restoring", usage: "on|off", summary: "the command under test", run: runDnsRestoring})
	commands.add(&sidecarctlCommand{
		words: "dns restore", usage: "--incident=<id>", summary: "the command under test",
		run: func(args []string, out io.Writer) error {
			return runDnsRestoreWithSettings(ctx, args, defaultDnsRestoreSettings(), out)
		},
	})
	stderr := &bytes.Buffer{}
	code := commands.run(append([]string{"dns"}, args...), out, stderr)
	return code, stderr.String()
}

// The runbook's order on a real store: a restore refuses before restoring is on; on sets dns_root.restoring; the
// restore lands the serial 2^24 above the root's under its incident and then waits for NS that do not answer, so
// a run that ends there exits 1 and says where it stood; another incident is refused; and off clears restoring.
func TestDnsRestoreCommandsRunTheStepOnTheEnvsStore(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		server.Testing_EnableKeyspaceNotifications(ctx)
		defer server.Config.PushSimpleResource("dns.yml", []byte(dnsRestoreServerTestYaml))()
		store, err := dnshost.NewStore(sidecarhost.NewRedis(nil))
		if err != nil {
			tb.Fatal(err)
		}
		defer store.Close()
		rootState := func() *dnsservice.DnsRootState {
			state, err := store.RootState(ctx, dnsRestoreServerTestRoot)
			if err != nil {
				tb.Fatal(err)
			}
			return state
		}
		incidentId := server.NewId()
		otherIncidentId := server.NewId()

		// no root row yet: nothing to restore, and nothing made
		if code, stderr := runServerDnsRestoreCommand(ctx, io.Discard, "restore", "--incident="+incidentId.String()); code != 1 || !strings.Contains(stderr, "has no dns_root row") {
			tb.Fatalf("before any root: exited %d with %q", code, stderr)
		}

		stdout := &bytes.Buffer{}
		if code, stderr := runServerDnsRestoreCommand(ctx, stdout, "restoring", "on"); code != 0 {
			tb.Fatalf("restoring on exited %d: %s", code, stderr)
		}
		if !strings.Contains(stdout.String(), "restoring is on for "+dnsRestoreServerTestRoot+" in "+server.RequireEnv()) {
			tb.Errorf("restoring on printed %q", stdout.String())
		}
		if state := rootState(); !state.Restoring || state.RestoreIncidentId != nil {
			tb.Fatalf("after restoring on: %+v", state)
		}
		serialBefore := rootState().Serial

		runCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		writer := &dnsRestoreCancelWriter{cancel: cancel}
		code, stderr := runServerDnsRestoreCommand(runCtx, writer, "restore", "--incident="+incidentId.String())
		if code != 1 || !strings.Contains(stderr, "has not ended; rerun with --incident="+incidentId.String()) {
			tb.Fatalf("a restore whose NS never answer exited %d with %q", code, stderr)
		}
		state := rootState()
		if !state.Restoring || state.RestoreIncidentId == nil || *state.RestoreIncidentId != sidecar.Id(incidentId) {
			tb.Fatalf("after the restore transaction: %+v", state)
		}
		if state.RestoreSerial < serialBefore+1<<24 || state.Serial != state.RestoreSerial {
			tb.Errorf("the restore landed at %d from %d, want at least 2^24 above", state.RestoreSerial, serialBefore)
		}
		for _, want := range []string{"ns that did not answer and are still counted: ns1 ns2", "ns not yet serving it: ns1 ns2"} {
			if !strings.Contains(writer.buffer.String(), want) {
				tb.Errorf("the restore printed %q, want %q in it", writer.buffer.String(), want)
			}
		}
		if strings.Contains(writer.buffer.String(), "done:") {
			tb.Errorf("the restore printed an end: %q", writer.buffer.String())
		}

		if code, stderr := runServerDnsRestoreCommand(ctx, io.Discard, "restore", "--incident="+otherIncidentId.String()); code != 1 || !strings.Contains(stderr, "committed under another incident") {
			tb.Errorf("another incident exited %d with %q", code, stderr)
		}
		if after := rootState(); after.RestoreSerial != state.RestoreSerial || *after.RestoreIncidentId != sidecar.Id(incidentId) {
			tb.Errorf("another incident moved the restore: %+v", after)
		}

		if code, stderr := runServerDnsRestoreCommand(ctx, io.Discard, "restoring", "off"); code != 0 {
			tb.Fatalf("restoring off exited %d: %s", code, stderr)
		}
		if rootState().Restoring {
			tb.Fatal("restoring off left restoring on")
		}
		if code, stderr := runServerDnsRestoreCommand(ctx, io.Discard, "restore", "--incident="+otherIncidentId.String()); code != 1 || !strings.Contains(stderr, "is not restoring") {
			tb.Errorf("a restore with restoring off exited %d with %q", code, stderr)
		}
	})
}
