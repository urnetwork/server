//go:build sidecar_pending

package main

// FS-42's M6 dns step (§12.6's last clause; DNS-M6-07) through the restore step on Postgres and Redis: dnshost's
// Store and RestoreStore, the custom-host map's real tables, and a fake lbmap whose reports name the hosts the
// regenerated map adds and drops. A host removed after the dump comes back with the dump's rows and a host added
// after it is gone; both are listed with the network's admin event, and every host stays pending until each
// interface is current for the restore's version, then turns ready. The NS are scripted to follow the reload, as
// DnsHarness's edges would; TLS-M6-07 runs the same step through the real split.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/dnshost"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarhost"
)

// NS that serve the restore serial once the restore transaction has run, each posting an empty diff once.
type restoreT3Ns struct {
	store dnsservice.DnsStore

	stateLock sync.Mutex
	posted    map[string]bool
}

func (self *restoreT3Ns) ReadSoa(ctx context.Context, root string, ns string) (uint32, error) {
	state, err := self.store.RootState(ctx, root)
	if err != nil {
		return 0, err
	}
	if state.RestoreIncidentId == nil {
		return uint32(state.Serial), nil
	}
	post := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		post = !self.posted[ns]
		self.posted[ns] = true
	}()
	if post {
		diff := &dnsservice.DnsReloadDiff{Root: root, ReplacedSerial: state.RestoreSerial - 1, Serial: state.RestoreSerial, Rrsets: []dnsservice.DnsRecordKey{}}
		if err := self.store.RecordReloadDiff(ctx, diff); err != nil {
			return 0, err
		}
	}
	return uint32(state.RestoreSerial), nil
}

func TestRestoreStepRegeneratesTheCustomHostMap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		id := strings.ReplaceAll(server.NewId().String()[24:], "-", "")
		root := "t" + id + ".example."
		if err := model.EnsureDnsRoot(ctx, root); err != nil {
			t.Fatal(err)
		}
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "restore-"+id, server.NewId())
		label, err := model.EnsureNetworkLabel(ctx, networkId, root)
		if err != nil {
			t.Fatal(err)
		}
		apex := label + "." + root
		domain := "corp" + id + ".example"
		server.Tx(ctx, func(tx server.PgTx) {
			now := server.NowUtc()
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_domain (network_id, domain, challenge, verify_time, acme_token) VALUES ($1, $2, 'challenge', $3, $4)`,
				networkId, domain, now, "a"+id,
			))
			for _, host := range []string{"kept." + domain, "removed." + domain} {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						INSERT INTO network_dns_custom_host (host, root, network_id, owner, state, caa_mode, effective_caa_chain_ok, create_time)
						VALUES ($1, $2, $3, $4, $5, $6, true, $7)
					`,
					host, root, networkId, "blog."+apex, sidecar.DnsCustomHostStateActive, sidecar.DnsCustomHostModeWeak, now,
				))
			}
			_, err := model.DnsRaiseCustomHostMapInTx(ctx, tx, root)
			server.Raise(err)
		}, server.TxReadCommitted)
		interfaces := []string{"edge-1-eth0", "edge-2-eth0"}
		if err := model.SyncDnsCustomHostMapInterfaces(ctx, interfaces); err != nil {
			t.Fatal(err)
		}
		// the lost primary ran on to version 7: removed was taken out and late added after the dump, which lbmap
		// loaded
		lostDigest := dnsservice.DnsCustomHostMapDigest([]string{"kept." + domain, "late." + domain})
		for _, name := range interfaces {
			if err := model.RecordDnsCustomHostMapReport(ctx, &dnsservice.DnsCustomHostMapReport{
				Format: dnsservice.DnsFormat, Interface: name, Version: 7, Digest: lostDigest,
				Master: &dnsservice.DnsMapMaster{Pid: 1, StartTimeMillis: 1},
			}, server.NowUtc(), 16, 16); err != nil {
				t.Fatal(err)
			}
		}

		store, err := dnshost.NewStore(sidecarhost.NewRedis(nil))
		if err != nil {
			t.Fatal(err)
		}
		settings := dnsservice.DefaultDnsSettings()
		settings.Roots = []string{root}
		settings.SignaturesPerSecond = 1
		settings.Tcp.ConnectionsPerSource = 1
		settings.Rrl.ResponsesPerSecond = 1
		settings.Restore.DiffWaitSeconds = 0
		settings.Restore.MapWaitSeconds = 0
		clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
		restoreStore := &dnshost.RestoreStore{}
		step, err := dnsservice.NewRestoreStep(settings, &dnsservice.RestoreStepDeps{
			Store:          store,
			Restore:        restoreStore,
			SoaReader:      &restoreT3Ns{store: store, posted: map[string]bool{}},
			CustomHostMaps: restoreStore,
			Clock:          clock,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := step.SetRestoring(ctx, root, true); err != nil {
			t.Fatal(err)
		}
		incidentId := sidecar.NewId()
		progress, err := step.Run(ctx, root, incidentId)
		if err != nil {
			t.Fatal(err)
		}
		want := 7 + dnsservice.DnsRestoreSerialJump
		if progress.MapVersion != want || !progress.Done || progress.MapReportedCount != 0 || progress.MapInterfaceCount != 2 {
			t.Fatalf("progress %+v", progress)
		}
		readiness, err := model.DnsCustomHostReadiness(ctx, root, networkId)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range readiness {
			if row.MapVersion != want || row.ReadyTime != nil || len(row.PendingInterfaces) != 2 {
				t.Fatalf("%s across the restore %+v", row.Host, row)
			}
		}
		customHostMap, err := model.GetDnsCustomHostMap(ctx, root)
		if err != nil || customHostMap.Version != want || len(customHostMap.Hosts) != 2 {
			t.Fatalf("the regenerated map %+v %v", customHostMap, err)
		}
		// each lbmap loads the restored map: removed back, late gone
		for _, name := range interfaces {
			if err := model.RecordDnsCustomHostMapReport(ctx, &dnsservice.DnsCustomHostMapReport{
				Format: dnsservice.DnsFormat, Interface: name, Version: want, Digest: customHostMap.Digest,
				Master: &dnsservice.DnsMapMaster{Pid: 1, StartTimeMillis: 1},
				Added:  []string{"removed." + domain}, Dropped: []string{"late." + domain},
			}, server.NowUtc(), settings.Probe.RouteHostCount, settings.Map.ReportHostMaxCount); err != nil {
				t.Fatal(err)
			}
		}
		// a rerun lists what the reports added and dropped
		progress, err = step.Run(ctx, root, incidentId)
		if err != nil {
			t.Fatal(err)
		}
		listed := map[string]bool{}
		for _, entry := range progress.Listing.Entries {
			if entry.CustomHost != "" && entry.NetworkId != nil && server.Id(*entry.NetworkId) == networkId {
				listed[entry.CustomHost+" "+entry.CustomHostChange] = true
			}
		}
		if !listed["removed."+domain+" added"] || !listed["late."+domain+" dropped"] || len(listed) != 2 {
			t.Fatalf("listed %v", listed)
		}
		// the hosts turn ready once both interfaces pass the restored version's probes
		now := server.NowUtc()
		for _, name := range interfaces {
			if err := model.RecordDnsRouteProbes(ctx, &dnsservice.DnsRouteProbeReport{Format: dnsservice.DnsFormat, Interfaces: []dnsservice.DnsRouteProbeResult{{
				Name: name, Version: want, ProbeName: dnsservice.DnsCustomHostMapProbeName(customHostMap.Digest), ProbePassed: true,
				Hosts: []dnsservice.DnsRouteProbeHost{{Host: "removed." + domain, Passed: true}},
			}}}, now); err != nil {
				t.Fatal(err)
			}
		}
		state, err := model.GetDnsCustomHostMapState(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		if err := model.SetDnsCustomHostReadiness(ctx, root, settings.Map.Judge(state, now).Ready, now); err != nil {
			t.Fatal(err)
		}
		readiness, err = model.DnsCustomHostReadiness(ctx, root, networkId)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range readiness {
			if row.ReadyTime == nil {
				t.Fatalf("%s not ready once every interface is current", row.Host)
			}
		}
		if _, err := step.Run(ctx, root, sidecar.NewId()); !errors.Is(err, dnsservice.ErrDnsNotRestoring) {
			t.Fatalf("another incident after the end: %v", err)
		}
	})
}
