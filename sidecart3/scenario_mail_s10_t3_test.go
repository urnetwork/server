//go:build sidecar_localstack && sidecar_testhooks && sidecar_pending

// S10 on T3 (§22.7; §3.7's F10; R-MAIL-6, R-MAIL-7, R-MAIL-11, D-MAIL-7, D-DNS-11, I-13, R-PHASE-7): its receive-only
// half (MAIL-M8-10) and its replying half (MAIL-M9-06), sidecartest/mailworld's builders, the ones T2 runs, on a T3 Net
// under the harness and the guard. What is server's here:
//
//   - the network's authorization log on server's sequencer over the local stack's MinIO (config_log.go);
//   - evs's engine, the mailbox's and mailcfg's objects, the gkm objects and the router's Redis, {mxr_<label>} and
//     the fleet's closed-handshake memory, on the private Redis and the local stack's MinIO, so a burner's record, a
//     deposit and a burn's record are written to and read from real stores, which CaptureStores then scans whole for
//     the burner's label, its address and every canary;
//   - the zone: the network's row and label, §12.7's edge records and mail's records written through model's own
//     paths into the test's database, served by a DnsHarness whose block reads dnshost.Store, so "no DNS change" is
//     dns_root's serial and network_dns_record's rows in Postgres and what an edge answers from them;
//   - the replying half's selectors: POST /smtp/dkim-keys's write through model.PublishSmtpDkimKeys, its smtp_dkim_key
//     row and DNS-M9-01's writers in one transaction, and key expiry through model.ExpireSmtpDkimKeys, run at the
//     moment past a selector's keep, so a burner's first selector, its label's copies and their removal with its
//     last are model's writes in that zone (mailS10T3Dkim).
//
// What stays the world's stand-in on T3 as on T2, so not shown here: api's mail routes (the world counts the calls a
// MailClient makes, and a burn makes none), §5.4's gate, the receiver's stream from the router (the world's dialer,
// not SocketEdgeDialer on server's exchange), the tokens evs verifies (the fake host's authenticator) and
// mail_connection_record (the world's FakeMxEdgeStore; mx-2 on Postgres is S7's router run, cli/mxedge); and for the
// replying half, POST /smtp/dkim-keys's handler and its mail_send check (the model's call answers the route's write,
// the check allowed, as api's gate cache would answer a granted sender) and the outbound path, SMTP's stub edge and
// ScriptedMx (smtpedge's T3 run is SMTP-M9-31's).
//
// The stack must be up (sidecar/local/stack.sh), and this fails rather than skips without it.

package sidecart3

import (
	"context"
	"fmt"
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

// The replying half's server half on T3 (mailworld.S10ReplyingSettings.Dkim; the file header's selectors row): the
// sending sidecar's calls of POST /smtp/dkim-keys applied by model.PublishSmtpDkimKeys with the route's mail_send check
// allowed, and key expiry by model.ExpireSmtpDkimKeys at the moment a row names, under the zone's own root as the
// user root; each answers once every edge serves the root's serial after it.
type mailS10T3Dkim struct {
	harness   *sidecartest.DnsHarness
	root      string
	caller    *model.SmtpDkimKeysCaller
	clock     sidecar.Clock
	api       *model.SmtpApiSettings
	retention *model.SmtpRetentionSettings
}

func mailS10T3DkimOf(ctx context.Context, t testing.TB, run *sidecartest.ScenarioRun, zone *mailworld.S10Zone, networkId sidecar.Id, sidecarId sidecar.Id) (mailworld.S10Dkim, error) {
	root := zone.Harness.Root()
	// the routes write under dns.yml's user root, which here is the zone's own
	t.Cleanup(model.Testing_SetDnsUserRoot(root))
	return &mailS10T3Dkim{
		harness:   zone.Harness,
		root:      root,
		caller:    &model.SmtpDkimKeysCaller{NetworkId: server.Id(networkId), SidecarId: server.Id(sidecarId)},
		clock:     run.Net.Clock(),
		api:       model.DefaultSmtpApiSettings(),
		retention: model.DefaultSmtpRetentionSettings(),
	}, nil
}

func (self *mailS10T3Dkim) SmtpDkimKeys(ctx context.Context, args *sidecar.SmtpDkimKeysArgs) (*sidecar.SmtpDkimKeysResult, error) {
	mailSend := func() (bool, error) {
		return true, nil
	}
	result, err := model.PublishSmtpDkimKeys(ctx, self.caller, args, mailSend, self.clock.Now(), self.api)
	if err != nil {
		return nil, err
	}
	return result, self.awaitServed(ctx)
}

func (self *mailS10T3Dkim) RetiredKeepTimeout() time.Duration {
	return self.retention.DkimRetiredKeepTimeout
}

func (self *mailS10T3Dkim) Expire(ctx context.Context, now time.Time) error {
	if result := model.ExpireSmtpDkimKeys(ctx, now, self.retention); result.FailedCount != 0 {
		return fmt.Errorf("%d retired selectors stayed in dns", result.FailedCount)
	}
	return self.awaitServed(ctx)
}

// Waits until every edge serves the root's serial as it stands.
func (self *mailS10T3Dkim) awaitServed(ctx context.Context) error {
	state, err := model.GetDnsRootState(ctx, self.root)
	if err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, mailS10T3WaitTimeout)
	defer cancel()
	return self.harness.AwaitServed(waitCtx, self.root, state.Serial)
}

func TestScenarioS10ReplyingOnT3(t *testing.T) {
	testEnvWithSchema().Run(t, func(tb testing.TB) {
		settings := &mailworld.S10ReplyingSettings{Zone: mailS10T3Zone, Dkim: mailS10T3DkimOf}
		sidecartest.RunScenario(t, mailworld.S10Replying(t, settings), ScenarioSettings(t, nil))
	})
}
