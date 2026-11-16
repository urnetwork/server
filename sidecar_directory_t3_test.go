//go:build sidecar_localstack && sidecar_testhooks

// FS-99's evs clause on T3 (§22.4, §22.8; §11.12's directory row, I-EVS-16; RECORD-M6-25): the builder sidecartest/fault
// exports (fs99_evs.go), run on T3's Nets, where sidecart3 names evshost's block as the evs world's instance
// (sidecart3/evs_tier.go). So the team directory's repo, its writers' streams and their slot objects stand on the
// private Redis and the local stack's MinIO; every token is one server's oauth mints, an rw token for agent X and the
// device parked for consent, an r token for the read_only sidecar; and X's ERASE of another writer's entry slots, its
// ?closure=1 upload and its POST .../slots/delete are refused 403 SCOPE by the handler server runs, with every slot's
// ETag in the store as it was, before a preservation hold and under one, beside the admin's ERASE and upload, which
// are taken. The clause runs under every rule FS-99 takes with T3's nightly seeds (fault.RunClause).
//
// The stack must be up (sidecar/local/stack.sh), and these fail rather than skip without it.
//
// Not here, and so not run on T3: FS-99's directory legs (a repo owner's revocation, the erasure runner, readers
// counting an ERASE only from a directory admin, the fresh device opening entries through the directory's own reader)
// and the rest of this file's plan entry, FS-84's and FS-101's none of which is T3's. Those run on T2 in the
// authorship world over sidecartest's streams (sidecar's fs99_test.go), and reach T3 once a DirectoryCards and
// directory streams binding to evs exists outside sidecartest's fakes. Which token §7.8's exchange mints a parked
// device or a read_only sidecar is not judged here either: the tokens' roles are the clause's own.

package server_test

import (
	"strings"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecart3"
)

// The private stores, and no schema: an evs world opens no connect client.
func sidecarDirectoryT3TestEnv() *server.TestEnv {
	return &server.TestEnv{ApplyDbMigrations: false}
}

func TestFs99EvsScopeOnT3(t *testing.T) {
	sidecarDirectoryT3TestEnv().Run(t, func(tb testing.TB) {
		fault.RunClause(tb, "FS-99", "evs scope", fault.Fs99EvsScope, sidecart3.RuleSettings(tb, nil))
	})
}

// §7.8's directory rule gone on T3: where X's tokens are minted with what its client asks for, admin among it, the
// handler takes its ERASE of A's entry slots, and the clause fails at that step.
func TestFs99EvsScopeMutantExchangeMintingXAdminFailsOnT3(t *testing.T) {
	sidecarDirectoryT3TestEnv().Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		net := sidecartest.NewNet(ctx, sidecart3.NetSettings(tb, nil))
		defer net.Close()
		settings := fault.DefaultFs99EvsSettings()
		settings.Mutant = true
		schedule := fault.Fs99EvsScopeOf(net, settings)
		schedule.Id = "FS-99's evs scope mutant"
		result := fault.Run(ctx, schedule, net)
		if result.Err == nil {
			tb.Fatal("the mutant passed")
		}
		if !strings.Contains(result.Err.Error(), "X's ERASE naming A's entry slots was not refused") {
			tb.Fatalf("the mutant failed elsewhere: %v", result.Err)
		}
	})
}
