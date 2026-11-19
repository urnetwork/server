//go:build sidecar_localstack && sidecar_testhooks && sidecar_pending

// S10's receive-only half on T3 (§22.7; §3.7's F10; R-MAIL-6, R-MAIL-7, R-MAIL-11, D-MAIL-7, I-13, R-PHASE-7;
// MAIL-M8-10): sidecartest/mailworld's builder, the one T2 runs, on a T3 Net under the harness and the guard. What
// is server's here:
//
//   - the network's authorization log on server's sequencer over the local stack's MinIO (config_log.go);
//   - evs's engine, the mailbox's and mailcfg's objects, the gkm objects and the router's Redis, {mxr_<label>} and
//     the fleet's closed-handshake memory, on the private Redis and the local stack's MinIO, so a burner's record, a
//     deposit and a burn's record are written to and read from real stores, which CaptureStores then scans whole for
//     the burner's label, its address and every canary;
//   - the zone: the network's row and label, §12.7's edge records and mail's records written through model's own
//     paths into the test's database, served by a DnsHarness whose block reads dnshost.Store, so "no DNS change" is
//     dns_root's serial and network_dns_record's rows in Postgres and what an edge answers from them.
//
// What stays the world's stand-in on T3 as on T2, so not shown here: api's mail routes (the world counts the calls a
// MailClient makes, and a burn makes none), §5.4's gate, the receiver's stream from the router (the world's dialer,
// not SocketEdgeDialer on server's exchange), the tokens evs verifies (the fake host's authenticator) and
// mail_connection_record (the world's FakeMxEdgeStore; mx-2 on Postgres is S7's router run, cli/mxedge). The replying
// half is MAIL-M9-06's.
//
// The stack must be up (sidecar/local/stack.sh), and this fails rather than skips without it.

package sidecart3

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/mailworld"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/dnshost"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarhost"
)

// How long the zone's start may wait for the stack to serve it: a bound for a failure, never a pace.
const mailS10T3WaitTimeout = 2 * time.Minute

// The network's zone on T3 (mailworld.S10Settings.Zone): a root of the test's own, so its {dns}serial key meets no
// other run's on the private Redis, reconciled through model; the network's row, its label as model allocates it, and
// its edge and mail records through model's writes; a harness on dnshost.Store whose edges serve them.
func mailS10T3Zone(ctx context.Context, t testing.TB, run *sidecartest.ScenarioRun, networkId sidecar.Id) (*mailworld.S10Zone, error) {
	// the store's raise wakes the block's holds through keyspace events
	server.Testing_EnableKeyspaceNotifications(ctx)
	id := strings.ReplaceAll(server.NewId().String(), "-", "")
	root := "t" + id[len(id)-16:] + ".example."
	harnessSettings := sidecartest.DefaultDnsHarnessSettings()
	settings := harnessSettings.Dns
	settings.Roots = []string{root}
	settings.RootRecords = map[string]*dnsservice.DnsRootRecordSettings{root: {ReportMx: "rpt.report.example", ApexEdge: true}}
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	if err := model.DnsReconcileRootRecords(ctx, settings); err != nil {
		return nil, err
	}
	store, err := dnshost.NewStore(sidecarhost.NewRedis(nil))
	if err != nil {
		return nil, err
	}
	t.Cleanup(store.Close)
	harnessSettings.Directory = t.TempDir()
	harnessSettings.Store = store
	harnessSettings.Challenges = dnshost.NewChallenges()
	harnessSettings.SeedApex = false
	waitCtx, cancel := context.WithTimeout(ctx, mailS10T3WaitTimeout)
	defer cancel()
	harness, err := sidecartest.NewDnsHarness(waitCtx, run.Net, harnessSettings)
	if err != nil {
		return nil, err
	}
	zone, err := func() (*mailworld.S10Zone, error) {
		serverNetworkId := server.Id(networkId)
		model.Testing_CreateNetwork(ctx, serverNetworkId, "mails10-"+id[len(id)-12:], server.NewId())
		label, err := model.EnsureNetworkLabel(ctx, serverNetworkId, root)
		if err != nil {
			return nil, err
		}
		if _, err := model.DnsSyncEdgeRecords(ctx, serverNetworkId, root, true, true); err != nil {
			return nil, err
		}
		// mail on, as POST /mail/enable's transaction turns it on (MAIL-M7-22)
		if _, err := model.DnsSetMailRecords(ctx, serverNetworkId, root, true); err != nil {
			return nil, err
		}
		state, err := model.GetDnsRootState(ctx, root)
		if err != nil {
			return nil, err
		}
		if err := harness.AwaitServed(waitCtx, root, state.Serial); err != nil {
			return nil, err
		}
		return &mailworld.S10Zone{Harness: harness, Label: label, Serial: state.Serial}, nil
	}()
	if err != nil {
		harness.Close()
		return nil, err
	}
	return zone, nil
}

func TestScenarioS10ReceiveOnlyOnT3(t *testing.T) {
	testEnvWithSchema().Run(t, func(tb testing.TB) {
		sidecartest.RunScenario(t, mailworld.S10ReceiveOnly(t, &mailworld.S10Settings{Zone: mailS10T3Zone}), ScenarioSettings(t, nil))
	})
}
