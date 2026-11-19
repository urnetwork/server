//go:build sidecar_localstack && sidecar_testhooks

// M8's indexer paths and FS-82's M8 case on T3 (§22.8's FS-82, T2+T3; R-MAIL-8, D-MAIL-15, owner decisions 150 and
// 197; MAIL-M8-10): sidecartest/mailworld's row, the one T2 runs, on a T3 Net. What is server's here: each network's
// authorization log is server's sequencer over the local stack's MinIO (config_log.go), so the receiver's admission,
// its owner confirm and its own IndexerSet for mail at member scope are judged and appended by model's
// ConfigLogSequencer, and the coverage a MailClient reads is over the state that log folds; evs's engine, the
// mailbox's and mailcfg's objects, the gkm objects and the router's {mxr_<label>} are on the private Redis and the
// local stack's MinIO. A desktop ur daemon, a NAS and a VPS each: before a designation an online receiver takes mail
// while coverage reads foreground-only and Status carries the prompt, then coverage reads indexer, the prompt is
// gone and the designated receiver takes the next message.
//
// What stays the world's stand-in on T3 as on T2: api's mail routes, §5.4's gate, the receiver's stream from the
// router, the tokens evs verifies, and the opener of a designation's sealed services, which reads the statement's
// header from the log's entry, since no running Sidecar opens them until ConfigIndexer carries the header's nonce
// (QD-1096).
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

func TestT3MailM8IndexerPathsTakeMailAndEndThePrompt(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		net := NewNet(t, nil)
		t.Cleanup(net.Close)
		for _, host := range mailworld.M8IndexerHosts() {
			// a label of the run's own, so its {mxr_<label>} meets no other run's on the private Redis
			id := strings.ReplaceAll(server.NewId().String(), "-", "")
			world := mailworld.NewM8World(t, net, &mailworld.M8WorldSettings{
				ReceiverName: host,
				Label:        "m8" + id[len(id)-12:],
				Host:         mailworld.NewM8FakeHost(t, net),
				Stores: func(service string) *sidecar.ServiceHost {
					return net.Host()
				},
			})
			mailworld.RunM8IndexerPath(t, world, host)
		}
	})
}
