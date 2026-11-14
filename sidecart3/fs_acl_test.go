//go:build sidecar_localstack

// ACL-M2-16's schedules on T3 (sidecartest/fault/fs_acl.go): FS-19's config read under every rule its case takes, FS-62's
// gate clause, and §22.5's §8 rows on server's sequencer over the local stack's MinIO, so every append is judged by
// api's real verifier (config_log.go). FS-19's forged base runs on T2 alone, as the schedule says; FS-62's gates read
// server's Redis and MinIO through AclGateDirectSource, the read cache written by the schedule as api's script writes
// it, since this package's sequencer installs no EntryCache.

package sidecart3

import (
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"
)

func TestFs19ConfigReadOnT3(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		RunRow(tb, "FS-19", "config-read", nil)
	})
}

// ACL-M4-04's FS-19 cases (sidecartest/fault/fs_acl_m4.go): a tagged ephemeral agent in a Pro network, its tag's
// grant, a read grant and an EvsShare each opening exactly their repo, and their revocations shrinks, judged by api's
// verifier on server's sequencer.
func TestFs19GrantOnT3(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		RunRow(tb, "FS-19", "grant", nil)
	})
}

func TestFs19ShareOnT3(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		RunRow(tb, "FS-19", "share", nil)
	})
}

func TestFs62AclGateOnT3(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		fault.RunClause(tb, "FS-62", "acl gate", fault.Fs62AclGate, RuleSettings(tb, nil))
	})
}

func TestAclReadersRowsEqualTheWritersCompileOnT3(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		Run(tb, fault.AclRowsEqualTheWriters, nil)
	})
}

func TestAclRowsALyingWriterForksEveryFullReaderOnT3(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		Run(tb, fault.AclRowsALyingWriter, nil)
	})
}
