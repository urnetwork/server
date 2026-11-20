//go:build sidecar_localstack && sidecar_testhooks

// M10's mailbox cases on T3 (§22.8's FS-76, FS-78, FS-111 and FS-112, T2+T3; FI-18, FI-19; MAIL-M10-04):
// sidecartest/mailworld's rows, the ones T2 runs, each on a T3 Net and a mailbox of its own. What is server's here:
// evs's engine, the mailbox's objects, the gkm objects and the router's {mxr_<label>} are on the private Redis and the
// local stack's MinIO, so the ingest's pushes, the index files they write, an erasure's CUT, ERASEs and CLOSUREs, the
// roll-ups and every read of the index back go through evsservice to those stores.
//
// What stays the world's stand-in on T3 as on T2: gkm's service and the tokens evs verifies, the receiver's stream
// from the router, the in-ref index writer installed by the test hook, and the search world FS-76's query set runs
// on, which is the root's stand-in for evs and no store of server's. What no row shows on either tier is in
// mailworld's m10_rows.go: no erasure is shown complete, and none runs at the storage limit.
//
// The stack must be up (sidecar/local/stack.sh), and this fails rather than skips without it.

package sidecart3

import (
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest/mailworld"

	"github.com/urnetwork/server"
)

// One of mailworld's M10 rows on a T3 Net and a mailbox of the run's own.
func runT3MailM10(t *testing.T, row func(world *mailworld.M10World)) {
	t.Helper()
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		net := NewNet(t, nil)
		t.Cleanup(net.Close)
		// a label of the run's own, so its {mxr_<label>} meets no other run's on the private Redis
		id := strings.ReplaceAll(server.NewId().String(), "-", "")
		row(mailworld.NewM10World(t, net, &mailworld.M10WorldSettings{
			Label: "ma" + id[len(id)-12:],
			Host:  mailworld.NewM8FakeHost(t, net),
			Stores: func(service string) *sidecar.ServiceHost {
				return net.Host()
			},
		}))
	})
}

func TestT3MailM10Fs76MailboxCase(t *testing.T) {
	runT3MailM10(t, func(world *mailworld.M10World) {
		mailworld.RunM10Fs76Mailbox(t, world, nil)
	})
}

func TestT3MailM10Fs78MailboxCase(t *testing.T) {
	runT3MailM10(t, func(world *mailworld.M10World) {
		mailworld.RunM10Fs78Mailbox(t, world)
	})
}

func TestT3MailM10Fs111MailboxCase(t *testing.T) {
	runT3MailM10(t, func(world *mailworld.M10World) {
		mailworld.RunM10Fs111Mailbox(t, world)
	})
}

func TestT3MailM10Fs112MailboxCase(t *testing.T) {
	runT3MailM10(t, func(world *mailworld.M10World) {
		mailworld.RunM10Fs112Mailbox(t, world, nil)
	})
}
