//go:build sidecar_localstack && sidecar_testhooks

// EVS1-M2-40's commit and outcome rows on T3 (§22.4, §22.8): the builders sidecartest/fault registers and exports,
// run on T3's Nets, where sidecart3 names this package's block as each evs world's instances (sidecart3/evs_tier.go),
// so every push goes through server/cli/evs's handler and evshost's segment route on the private Redis and the local
// stack's MinIO, its devices presenting tokens server's oauth mints. Each row's registered case runs under every rule
// it takes with T3's nightly seeds (sidecart3.RunRow), and each clause beside it under the same rules
// (fault.RunClause). The schedules hold instances at barriers, so this needs sidecar_testhooks; the stack must be up
// (sidecar/local/stack.sh), and these fail rather than skip without it.

package evshost_test

import (
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecart3"
)

// The private stores, and no schema: an evs world opens no connect client.
func fsCommitTestEnv() *server.TestEnv {
	return &server.TestEnv{ApplyDbMigrations: false}
}

// A row's clause on T3 under every rule the row takes, with the nightly seeds.
func runClauseOnT3(t *testing.T, rowId string, clause string, builder fault.Builder) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		fault.RunClause(tb, rowId, clause, builder, sidecart3.RuleSettings(tb, nil))
	})
}

func TestFs1OnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-1", "", nil)
	})
}

func TestFs1ConcurrentRetriesCommitOnceOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-1", "concurrent retries", fault.Fs1ConcurrentRetries)
}

func TestFs1UnknownDeletesNothingOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-1", "unknown deletes nothing", fault.Fs1UnknownDeletesNothing)
}

func TestFs2OnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-2", "", nil)
	})
}

func TestFs3OnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-3", "", nil)
	})
}

func TestFs7OnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-7", "", nil)
	})
}

func TestFs57OnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-57", "", nil)
	})
}

func TestFs57StoreUnreachableCommitsNothingOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-57", "store unreachable", fault.Fs57StoreUnreachable)
}

func TestFs113OnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-113", "", nil)
	})
}
