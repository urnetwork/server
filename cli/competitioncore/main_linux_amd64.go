// The prospective fixed kernel helper is silent and drop-only in production.
// This draft has no installation, sysctl, registry-admin or admission command.
package main

import (
	"context"
	"io"
	"os"

	"github.com/urnetwork/server/competitioncore"
)

// The tiny seam proves malformed/unknown invocations close stdin without
// emitting diagnostics or consulting caller-selected programs/configuration.
type dispatcher interface {
	Handle(context.Context, []string, io.ReadCloser) competitioncore.Decision
}

// No logging includes crashing-process arguments, paths, identities or core.
func run(ctx context.Context, arguments []string, core io.ReadCloser, handler dispatcher) {
	if handler == nil {
		if core != nil {
			core.Close()
		}
		return
	}
	handler.Handle(ctx, arguments, core)
}

// All rejected/uncertain cores are simply dropped. Exit status is not a proof
// of joined helper lifetime and must never authorize releasing frozen custody.
func main() {
	run(context.Background(), os.Args[1:], os.Stdin, competitioncore.NewHandler(competitioncore.DefaultSettings()))
}
