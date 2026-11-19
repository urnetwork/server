//go:build sidecar_testhooks && !windows

package main

// SEARCH-M10-40's index writers' load driver as the index mode's load search-notes (sidecar's
// sidecartest/bench/index_load.go): writers' in-ref pushes to a notes vault on evsservice's engine at format 2, in this
// process over the clusters under test, the index on or off by the leg's classes. It links only under
// sidecar_testhooks, since the in-ref writer is installed through the root's test seam until a daemon's composition
// installs one; the classes it does not drive yet fail their legs closed (its file header).

import (
	"context"

	"github.com/urnetwork/sidecar/sidecartest/bench"
)

func init() {
	indexLoadRegistry[indexLoadSearchNotes] = func(ctx context.Context) (IndexLoad, error) {
		return bench.NewIndexLoad(nil)
	}
}
