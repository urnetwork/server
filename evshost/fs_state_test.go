//go:build sidecar_localstack && sidecar_testhooks

// EVS1-M2-41's fold, epoch, read and lifecycle rows on T3 (§22.4, §22.8), as fs_commit_test.go runs EVS1-M2-40's: the
// builders sidecartest/fault registers, on T3's Nets, through this package's block on the private Redis and the local
// stack's MinIO, under every rule each row takes with the nightly seeds.

package evshost_test

import (
	"testing"

	"github.com/urnetwork/server/sidecart3"
)

func TestFs4OnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-4", "", nil)
	})
}
