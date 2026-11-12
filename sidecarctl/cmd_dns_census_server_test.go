// `sidecarctl dns census` against Postgres: the registered command's counts are model.CensusDnsNetworkNames's over
// the same rows, on sessions that can only read. Needs the private test stack, through .stack/server-test.sh.

package main

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The binary's own command prints the model's counts for the env's names, fixtures of each class among them, and
// leaves the process read-only: a write after it is refused.
func TestDnsCensusCountsTheEnvsNamesReadOnly(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		for _, name := range []string{"abc", "-census-lead", "ce--nsus", "postmaster", "census-ordinary"} {
			model.Testing_CreateNetwork(ctx, server.NewId(), name, server.NewId())
		}
		defer server.Testing_ReleasePgReadOnly()
		stdout := &bytes.Buffer{}
		stderr := &bytes.Buffer{}
		if code := sidecarctlCommands.run([]string{"dns", "census"}, stdout, stderr); code != 0 {
			tb.Fatalf("exited %d: %s", code, stderr.String())
		}
		want := model.CensusDnsNetworkNames(ctx)
		if want.PreRule < 1 || want.EdgeHyphen < 1 || want.Dash34 < 1 || want.Reserved < 1 {
			tb.Fatalf("the fixtures do not cover each class: %+v", want)
		}
		wantText := fmt.Sprintf("pre_rule %d\nedge_hyphen %d\ndash34 %d\nreserved %d\n", want.PreRule, want.EdgeHyphen, want.Dash34, want.Reserved)
		if !bytes.HasSuffix(stdout.Bytes(), []byte(wantText)) {
			tb.Errorf("printed %q, want it to end %q", stdout.String(), wantText)
		}
		// the process is read-only from the command on
		err := sidecarctlServerCall(func() error {
			model.Testing_CreateNetwork(ctx, server.NewId(), "census-after", server.NewId())
			return nil
		})
		if err == nil {
			tb.Error("a write after the census went through, so its sessions were not read-only")
		}
	})
}
