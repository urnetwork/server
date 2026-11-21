//go:build sidecar_pending

package main

// FS-42's M6 dns step (§12.6's last clause; DNS-M6-07) through the restore step on Postgres and Redis: dnshost's
// Store and RestoreStore, the custom-host map's real tables, and a fake lbmap whose reports name the hosts the
// regenerated map adds and drops. A host removed after the dump comes back with the dump's rows and a host added
// after it is gone; both are listed with the network's admin event, and every host stays pending until each
// interface is current for the restore's version, then turns ready. The NS are scripted to follow the reload, as
// DnsHarness's edges would; TLS-M6-07 runs the same step through the real split.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/dnshost"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarhost"
	"github.com/urnetwork/server/sidecarrestore"
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
			if err := model.RecordDnsCustomHostMapReportForRoot(ctx, root, &dnsservice.DnsCustomHostMapReport{
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
			if err := model.RecordDnsCustomHostMapReportForRoot(ctx, root, &dnsservice.DnsCustomHostMapReport{
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

// dns's tables as a sidecar dump holds them and a restore loads them (xops's sidecar-tables.conf and its reconcile):
// these are dumped; the journal and the custom-host map's versions are not, so a restore leaves them empty, and
// dns_root.restoring goes up as the dump loads.
var dnsT3DumpedTables = []string{
	"dns_label", "dns_root", "network_dns_zone", "network_dns_record", "network_dns_custom_host",
	"dns_custom_host_map_applied", "dns_restore_incident",
}
var dnsT3UndumpedTables = []string{"dns_zone_journal", "dns_custom_host_map"}

// A dump of dns's tables taken in the test's own database, copied beside them.
type dnsT3Dump struct {
	prefix string
}

// Copies each dumped table as it stands now.
func takeDnsT3Dump(t testing.TB, ctx context.Context) *dnsT3Dump {
	t.Helper()
	self := &dnsT3Dump{prefix: "dnst3_dump_" + strings.ReplaceAll(server.NewId().String()[24:], "-", "") + "_"}
	server.Tx(ctx, func(tx server.PgTx) {
		for _, table := range dnsT3DumpedTables {
			server.RaisePgResult(tx.Exec(ctx, `CREATE TABLE `+self.prefix+table+` AS SELECT * FROM `+table))
		}
	})
	return self
}

// The primary is lost and the dump loads in its place: every dumped table back as the dump holds it, the journal and
// the map's versions empty, and restoring up, as xops's restore leaves the database for §19.10's steps.
func (self *dnsT3Dump) restore(t testing.TB, ctx context.Context) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		for _, table := range append(slices.Clone(dnsT3DumpedTables), dnsT3UndumpedTables...) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM `+table))
		}
		for _, table := range dnsT3DumpedTables {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO `+table+` SELECT * FROM `+self.prefix+table))
		}
		server.RaisePgResult(tx.Exec(ctx, `UPDATE dns_root SET restoring = true`))
	})
}

// dns_ns's stand-in until its table lands (plan-DNS-running-ns; its dns-1 row is in metadata_list.yml, its table not
// yet in MG5): the step's NS state in memory, mirrored into the harness's ready store, so /dns/internal/ready counts
// only what the step leaves running, beside dnshost.RestoreStore's lists on Postgres. Safe for concurrent use.
type dnsT3NsState struct {
	*dnshost.RestoreStore
	harness *sidecartest.DnsHarness

	stateLock   sync.Mutex
	rootStopped map[string][]string
}

func newDnsT3NsState(harness *sidecartest.DnsHarness) *dnsT3NsState {
	return &dnsT3NsState{RestoreStore: &dnshost.RestoreStore{}, harness: harness, rootStopped: map[string][]string{}}
}

func (self *dnsT3NsState) StoppedNs(ctx context.Context, root string) ([]string, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.rootStopped[root]), nil
}

func (self *dnsT3NsState) StopNs(ctx context.Context, root string, ns string, reason string) error {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !slices.Contains(self.rootStopped[root], ns) {
			self.rootStopped[root] = append(self.rootStopped[root], ns)
		}
	}()
	self.harness.SetNsStopped(root, ns, true)
	return nil
}

func (self *dnsT3NsState) StartNs(ctx context.Context, root string, ns string) error {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.rootStopped[root] = slices.DeleteFunc(self.rootStopped[root], func(stopped string) bool { return stopped == ns })
	}()
	self.harness.SetNsStopped(root, ns, false)
	return nil
}

// The restore step over the stack's store, the edges' SOAs and restore, the custom-host map's half on Postgres, polling
// once a second on the wall clock: the step waits on the edges, whose reloads take no time on the Net's clock.
func (self *dnsT3) restoreStep(t testing.TB, restore dnsservice.DnsRestoreStore, maps dnsservice.DnsRestoreMapStore) *dnsservice.RestoreStep {
	t.Helper()
	settings := *self.settings
	settings.Restore.PollSeconds = 1
	settings.Restore.DiffWaitSeconds = 30
	settings.Restore.MapWaitSeconds = 0
	step, err := dnsservice.NewRestoreStep(&settings, &dnsservice.RestoreStepDeps{
		Store:          self.store,
		Restore:        restore,
		SoaReader:      self.harness.ApiDeps().SoaReader,
		CustomHostMaps: maps,
		Clock:          sidecar.RealClock{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return step
}

// What ns answers for name's type over TCP: the rcode and each record's RDATA, sorted.
func (self *dnsT3) answer(t testing.TB, ctx context.Context, ns string, name string, rrType uint16) (int, []string) {
	t.Helper()
	reply := self.query(t, ctx, ns, name, rrType, false)
	rdatas := []string{}
	for _, record := range reply.Answer {
		if record.Header().Rrtype == rrType {
			rdatas = append(rdatas, strings.TrimPrefix(record.String(), record.Header().String()))
		}
	}
	slices.Sort(rdatas)
	return reply.Rcode, rdatas
}

// A user TXT row at owner.
func dnsT3Txt(owner string, text string) dnsservice.DnsRecordSet {
	return dnsservice.DnsRecordSet{Owner: owner, Type: dns.TypeTXT, Ttl: 300, Kind: dnsservice.DnsRecordKindUser, Rdata: []string{`"` + text + `"`}}
}

// FS-42's M5 dns step (§12.6's restore step, §19.10's dns step; FI-12, I-14) through both edges on Postgres and Redis,
// every clause, and FS-41 and FS-42's labels step after it:
//
//   - the lost primary ran on past the dump: a record, a zone and a host suspension, a lift and a network's label
//     allocated after it, which both NS served; then both NS stopped, and a challenge committed above every SOA
//   - the dump loads with restoring up: every write answers 503 and takes no serial, and §21.10's abuse step re-places
//     the suspension, the host's and the lift from the abuse mailbox, setting only their rows
//   - ns2 is down for the step's SOA read, so it is stopped; ns1 reloads at restore_serial, which lands above every
//     serial the lost primary issued, the Redis key's included, the suspensions and the lift holding through that
//     first reload; writes reopen once ns1 serves it, and /dns/internal/ready counts ns1 alone
//   - both challenges, the one above every SOA and the one at an older serial, read stale and are rewritten
//   - every RRset that differs between ns1's replaced snapshot and the restored zone is listed with its network, and
//     the label lost with the dump with none
//   - ns2 serves again only from a reload, its local snapshot the replaced one, posting its diff; a rerun starts it,
//     and a record written after the restore publishes on both
//   - the CT replay inserts the lost label ownerless, which routes nothing, and its network's leaf key re-binds it,
//     both NS answering its zone again
func TestFs42DnsStepThroughBothEdges(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		runFs42DnsStep(t, &dnsT3CopyLoss{})
	})
}

// How a test loses the primary: its dumps taken at one moment, the primary lost under an incident later, the restored
// database then the one the env's pools reach with restoring up, and §19.10's dns step run under the incident.
type dnsT3Loss interface {
	takeDumps(t testing.TB, ctx context.Context)
	lose(t testing.TB, ctx context.Context, incidentId sidecar.Id)
	runDnsStep(t testing.TB, ctx context.Context, step *dnsservice.RestoreStep, root string, incidentId sidecar.Id) (*dnsservice.DnsRestoreProgress, error)
}

// The loss by copy, in the test's own database: dns's tables as a sidecar dump holds them, put back as a restore does,
// and the step run as `sidecarctl dns restore` runs it.
type dnsT3CopyLoss struct {
	dump *dnsT3Dump
}

func (self *dnsT3CopyLoss) takeDumps(t testing.TB, ctx context.Context) {
	self.dump = takeDnsT3Dump(t, ctx)
}

func (self *dnsT3CopyLoss) lose(t testing.TB, ctx context.Context, incidentId sidecar.Id) {
	self.dump.restore(t, ctx)
}

func (self *dnsT3CopyLoss) runDnsStep(t testing.TB, ctx context.Context, step *dnsservice.RestoreStep, root string, incidentId sidecar.Id) (*dnsservice.DnsRestoreProgress, error) {
	return step.Run(ctx, root, incidentId)
}

// The loss through OPS's frame (OPS-M3-04's PostgresLossFixture, FS-42 on T3): both dumps by xops's own scripts, the
// primary lost and the dumps restored into a new database that the env's pools then reach, restoring raised by xops's
// reconcile, and §19.10's dns step run by the restore frame, each step before it ended by hand and every step run
// twice, as the drill runs them.
type dnsT3FrameLoss struct {
	fixture  *sidecarrestore.PostgresLossFixture
	incident *sidecarrestore.Incident
}

func (self *dnsT3FrameLoss) takeDumps(t testing.TB, ctx context.Context) {
	self.fixture.TakeMainDump()
	self.fixture.TakeSidecarDump()
}

func (self *dnsT3FrameLoss) lose(t testing.TB, ctx context.Context, incidentId sidecar.Id) {
	self.incident = self.fixture.LosePrimary(server.Id(incidentId))
}

func (self *dnsT3FrameLoss) runDnsStep(t testing.TB, ctx context.Context, step *dnsservice.RestoreStep, root string, incidentId sidecar.Id) (*dnsservice.DnsRestoreProgress, error) {
	frame := sidecarrestore.NewFrame()
	frame.Register(sidecarrestore.StepDns, func(ctx context.Context, run *sidecarrestore.StepRun) error {
		return sidecarrestore.RunDnsStep(ctx, &sidecarrestore.DnsStepDeps{
			Open: func(ctx context.Context, run *sidecarrestore.StepRun) ([]string, sidecarrestore.DnsRestoreRunner, func(), error) {
				return []string{root}, step, func() {}, nil
			},
		}, run)
	})
	out := &bytes.Buffer{}
	if err := self.fixture.RunFrameThrough(ctx, frame, sidecarrestore.StepDns, nil, out, nil); err != nil {
		return nil, fmt.Errorf("the frame through its dns step: %w\n%s", err, out)
	}
	if !self.incident.StepEnded(sidecarrestore.StepDns) {
		return nil, fmt.Errorf("the frame's dns step did not end:\n%s", out)
	}
	// where the frame left the step: a rerun under the incident answers it
	return step.Run(ctx, root, incidentId)
}

// FS-42's M5 dns step through xops's real dumps and restore and OPS's frame, on T3 alone: it needs Docker with the
// private server's image, gpg, python3 and xops's sibling checkout, and server/local/sidecar-restore-t3/run.sh sets
// SIDECAR_RESTORE_T3=1 where they are.
func TestFs42DnsStepOnTheRestoreFrame(t *testing.T) {
	if os.Getenv("SIDECAR_RESTORE_T3") != "1" {
		t.Skip("not run, which is no pass: FS-42 through xops's real dumps runs on T3, where server/local/sidecar-restore-t3/run.sh sets SIDECAR_RESTORE_T3=1")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := sidecarrestore.NewPostgresLossFixture(t, nil)
		defer fixture.Close()
		runFs42DnsStep(t, &dnsT3FrameLoss{fixture: fixture})
	})
}

// FS-42's M5 dns step over loss (see TestFs42DnsStepThroughBothEdges).
func runFs42DnsStep(t testing.TB, loss dnsT3Loss) {
	ctx := context.Background()
	defer model.Testing_SetDnsChallengesPerNameHour(100)()
	stack := newDnsT3(t, ctx, nil)
	ns1, ns2 := stack.settings.Ns[0], stack.settings.Ns[1]
	root := stack.root
	incidentId := sidecar.NewId()

	// before the dump: two networks, a host of the first suspended, and a challenge both NS serve
	net1, apex1 := stack.network(t, ctx, "")
	net2, apex2 := stack.network(t, ctx, "")
	for _, host := range []string{"h1", "h2"} {
		if _, err := model.DnsWrite(ctx, &model.DnsWriteArgs{Root: root, NetworkId: net1, Upserts: []dnsservice.DnsRecordSet{dnsT3Txt(host+"."+apex1, host)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := model.SuspendHost(ctx, net1, "h2"); err != nil {
		t.Fatal(err)
	}
	olderChallenge, err := stack.harness.PutChallenge(ctx, root, apex1, []string{"older-digest-0123456789abcdefghijklmnopqrstuv"})
	if err != nil {
		t.Fatal(err)
	}
	stack.awaitLatest(t, ctx)
	loss.takeDumps(t, ctx)
	dumpSerial := stack.serial(t, ctx)
	t.Logf("phase: dumped at %d", dumpSerial)

	// the lost primary runs on: a record, a zone suspension, a host suspension, a lift and a new network
	if _, err := model.DnsWrite(ctx, &model.DnsWriteArgs{Root: root, NetworkId: net1, Upserts: []dnsservice.DnsRecordSet{dnsT3Txt("after."+apex1, "after the dump")}}); err != nil {
		t.Fatal(err)
	}
	applyAbuse := func() {
		t.Helper()
		if err := model.SuspendZone(ctx, net2, "phishing", nil); err != nil {
			t.Fatal(err)
		}
		if err := model.SuspendHost(ctx, net1, "h1"); err != nil {
			t.Fatal(err)
		}
		if err := model.Restore(ctx, net1, "h2"); err != nil {
			t.Fatal(err)
		}
	}
	applyAbuse()
	net3, apex3 := stack.network(t, ctx, "")
	label3 := strings.TrimSuffix(apex3, "."+root)
	lostSerial := stack.awaitLatest(t, ctx)
	t.Logf("phase: the lost primary at %d", lostSerial)
	// what both NS serve of the lost primary, and what must hold through the restore
	requireHeld := func(what string, ns string) {
		t.Helper()
		if _, texts := stack.answer(t, ctx, ns, "h1."+apex1, dns.TypeTXT); len(texts) != 0 {
			t.Errorf("%s: %s answers the suspended h1: %v", what, ns, texts)
		}
		if _, texts := stack.answer(t, ctx, ns, "h2."+apex1, dns.TypeTXT); !slices.Equal(texts, []string{`"h2"`}) {
			t.Errorf("%s: %s withholds the lifted h2: %v", what, ns, texts)
		}
		if _, mxs := stack.answer(t, ctx, ns, apex2, dns.TypeMX); len(mxs) != 0 {
			t.Errorf("%s: %s answers the suspended zone: %v", what, ns, mxs)
		}
	}
	for _, ns := range stack.settings.Ns {
		requireHeld("the lost primary", ns)
	}

	// both NS stop, and a challenge commits above every SOA
	for _, ns := range stack.settings.Ns {
		if err := stack.harness.StopEdge(ns); err != nil {
			t.Fatal(err)
		}
	}
	newerChallenge, err := stack.harness.PutChallenge(ctx, root, apex1, []string{"newer-digest-0123456789abcdefghijklmnopqrstuv"})
	if err != nil {
		t.Fatal(err)
	}
	if newerChallenge <= lostSerial {
		t.Fatalf("the newer challenge took %d, under %d", newerChallenge, lostSerial)
	}

	t.Logf("phase: both NS stopped, the newer challenge at %d", newerChallenge)
	// the loss: the dump loads with restoring up, and the abuse step re-places what mail says, setting only rows
	loss.lose(t, ctx, incidentId)
	applyAbuse()
	if serial := stack.serial(t, ctx); serial != dumpSerial {
		t.Fatalf("the abuse step took a serial: %d, the dump's %d", serial, dumpSerial)
	}
	if _, err := stack.harness.PutChallenge(ctx, root, apex1, []string{"refused-digest-0123456789abcdefghijklmnopqrst"}); err == nil {
		t.Error("a challenge wrote while restoring")
	}
	if _, err := model.DnsWrite(ctx, &model.DnsWriteArgs{Root: root, NetworkId: net1, Upserts: []dnsservice.DnsRecordSet{dnsT3Txt("refused."+apex1, "refused")}}); !errors.Is(err, dnsservice.ErrDnsRestoring) {
		t.Errorf("a write while restoring: %v", err)
	}

	// ns1 comes back on its own snapshot, the lost primary's; ns2 stays down through the SOA read
	if err := stack.net.Restart(stack.harness.EdgeProcess(ns1), stack.harness.EdgeProcess(ns1)); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, dnsT3WaitTimeout)
	defer cancel()
	if err := stack.harness.AwaitServed(waitCtx, root, lostSerial, ns1); err != nil {
		t.Fatal(err)
	}
	requireHeld("ns1 on its own snapshot", ns1)
	t.Logf("phase: ns1 back on its snapshot")

	nsState := newDnsT3NsState(stack.harness)
	step := stack.restoreStep(t, nsState, nsState)
	progress, err := loss.runDnsStep(t, waitCtx, step, root, incidentId)
	if err != nil {
		t.Fatalf("the step: %v (%+v)", err, progress)
	}
	restoreSerial := progress.RestoreSerial
	t.Logf("phase: the step ran, restore serial %d", restoreSerial)
	if restoreSerial != newerChallenge+dnsservice.DnsRestoreSerialJump || !progress.Done {
		t.Fatalf("restore serial %d, want the Redis key's %d plus the jump: %+v", restoreSerial, newerChallenge, progress)
	}
	if !slices.Equal(progress.StoppedNs, []string{ns2}) || len(progress.WaitingNs) != 0 {
		t.Errorf("stopped %v waiting %v", progress.StoppedNs, progress.WaitingNs)
	}
	if soa := stack.query(t, ctx, ns1, root, dns.TypeSOA, false); soa.Answer[0].(*dns.SOA).Serial != uint32(restoreSerial) {
		t.Errorf("ns1 serves SOA %d, not the restore serial", soa.Answer[0].(*dns.SOA).Serial)
	}
	requireHeld("ns1 after its first reload", ns1)

	// every RRset that differs from ns1's replaced snapshot is listed: the lost record with its network, the
	// lost label's rows with none, no challenge
	listed := map[string]string{}
	for _, entry := range progress.Listing.Entries {
		network := ""
		if entry.NetworkId != nil {
			network = server.Id(*entry.NetworkId).String()
		}
		listed[entry.Owner+" "+dns.TypeToString[entry.Type]] = network
	}
	if network, ok := listed["after."+apex1+" TXT"]; !ok || network != net1.String() {
		t.Errorf("the record after the dump is listed as %q %t", network, ok)
	}
	if network, ok := listed[apex3+" MX"]; !ok || network != "" {
		t.Errorf("the lost label's MX is listed as %q %t", network, ok)
	}
	for key := range listed {
		if strings.Contains(key, model.DnsChallengeLabel) {
			t.Errorf("a challenge is listed: %s", key)
		}
	}

	// writes reopen with ns2 stopped, and land above restore_serial
	if _, err := model.DnsWrite(ctx, &model.DnsWriteArgs{Root: root, NetworkId: net1, Upserts: []dnsservice.DnsRecordSet{dnsT3Txt("reopened."+apex1, "reopened")}}); err != nil {
		t.Fatal(err)
	}
	if serial := stack.awaitLatest(t, ctx, ns1); serial <= restoreSerial {
		t.Errorf("a write after the restore took %d", serial)
	}

	// both challenges read stale, and certs' rewrite reads ready, ns1 alone counted while ns2 is stopped
	for _, serial := range []uint64{olderChallenge, newerChallenge} {
		answer, err := stack.harness.Ready(ctx, root, serial)
		if err != nil || answer.Answer != dnsservice.DnsReadyStale {
			t.Errorf("the challenge at %d reads %+v %v, not stale", serial, answer, err)
		}
	}
	rewritten, err := stack.harness.PutChallenge(ctx, root, apex1, []string{"rewritten-digest-0123456789abcdefghijklmnopqr"})
	if err != nil {
		t.Fatal(err)
	}
	stack.harness.ReportCanary(root, dnsservice.DnsCanarySourceSite, rewritten, true)
	// the block reads the NS's SOAs at most once an interval on the Net's clock, so the read after ns1 serves the
	// rewrite is the next interval's
	if err := stack.harness.AwaitServed(waitCtx, root, rewritten, ns1); err != nil {
		t.Fatal(err)
	}
	stack.net.Clock().Advance(time.Duration(stack.settings.Api.SoaReadIntervalSeconds) * time.Second)
	if answer, err := stack.harness.Ready(ctx, root, rewritten); err != nil || answer.Answer != dnsservice.DnsReadyReady {
		t.Errorf("the rewritten challenge at %d reads %+v %v, not ready with ns2 stopped", rewritten, answer, err)
	}

	// ns2 serves again only from a reload, its snapshot the replaced one, and a rerun starts it
	stack.harness.SetRequireReload(ns2, true)
	if err := stack.net.Restart(stack.harness.EdgeProcess(ns2), stack.harness.EdgeProcess(ns2)); err != nil {
		t.Fatal(err)
	}
	latest := stack.awaitLatest(t, ctx)
	t.Logf("phase: ns2 reloaded at %d", latest)
	progress, err = step.Run(waitCtx, root, incidentId)
	if err != nil {
		t.Fatalf("the rerun: %v (%+v)", err, progress)
	}
	if len(progress.StoppedNs) != 0 || progress.DiffCount != 2 {
		t.Errorf("after ns2's reload: stopped %v, %d diffs", progress.StoppedNs, progress.DiffCount)
	}
	requireHeld("ns2 after its reload", ns2)
	if _, texts := stack.answer(t, ctx, ns2, "reopened."+apex1, dns.TypeTXT); !slices.Equal(texts, []string{`"reopened"`}) {
		t.Errorf("ns2 after its reload: %v", texts)
	}
	// a record written now publishes on both, and ready counts ns2 again
	if _, err := model.DnsWrite(ctx, &model.DnsWriteArgs{Root: root, NetworkId: net1, Upserts: []dnsservice.DnsRecordSet{dnsT3Txt("both."+apex1, "both")}}); err != nil {
		t.Fatal(err)
	}
	if serial := stack.awaitLatest(t, ctx); serial <= latest {
		t.Errorf("serial %d after %d", serial, latest)
	}
	for _, ns := range stack.settings.Ns {
		if _, texts := stack.answer(t, ctx, ns, "both."+apex1, dns.TypeTXT); !slices.Equal(texts, []string{`"both"`}) {
			t.Errorf("%s: %v", ns, texts)
		}
	}

	// FS-41 and FS-42's labels step: the CT replay inserts the lost label ownerless, which routes nothing, and the
	// key of its network's logged leaf re-binds it, both NS answering its zone again
	key, leaf := dnsT3ReclaimLeaf(t, apex3)
	if _, err := model.ReplayDnsCtNames(ctx, root, server.Id(incidentId), []*model.DnsCtName{{Name: strings.TrimSuffix(apex3, "."), LeafSha256: sidecar.DnsReclaimLeafSha256(leaf)}}, server.NowUtc()); err != nil {
		t.Fatal(err)
	}
	stack.awaitLatest(t, ctx)
	for _, ns := range stack.settings.Ns {
		if rcode, mxs := stack.answer(t, ctx, ns, apex3, dns.TypeMX); rcode != dns.RcodeNameError || len(mxs) != 0 {
			t.Errorf("%s answers the ownerless %s: %s %v", ns, label3, dns.RcodeToString[rcode], mxs)
		}
	}
	proof := &sidecar.DnsReclaimProof{Name: strings.TrimSuffix(apex3, "."), NetworkId: sidecar.Id(net3), IncidentId: incidentId}
	signature, err := proof.Sign(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := model.ReclaimDnsLabel(ctx, &model.DnsLabelReclaim{Root: root, NetworkId: net3, ActorKind: model.AdminEventActorSystem, Request: &sidecar.DnsLabelReclaimRequest{
		Name: proof.Name, NetworkId: proof.NetworkId, IncidentId: incidentId, Leaf: leaf, Signature: signature,
	}}); err != nil {
		t.Fatal(err)
	}
	stack.awaitLatest(t, ctx)
	for _, ns := range stack.settings.Ns {
		if rcode, mxs := stack.answer(t, ctx, ns, apex3, dns.TypeMX); rcode != dns.RcodeSuccess || len(mxs) != 1 {
			t.Errorf("%s answers the re-bound %s: %s %v", ns, label3, dns.RcodeToString[rcode], mxs)
		}
	}
}

// A P-256 key and a leaf on it naming name and its wildcard, as a device's certificate does.
func dnsT3ReclaimLeaf(t testing.TB, name string) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	name = strings.TrimSuffix(name, ".")
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name, "*." + name},
		NotBefore:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:     time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
	}
	leaf, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return key, leaf
}

// The persisted NS inventory lets the healthy authority reopen writes and retains the stopped authority
// until its reloaded snapshot reaches the restore serial.
func TestFs42DnsStepPersistsAnNsStopAcrossRerun(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		stack := newDnsT3(t, ctx, nil)
		ns1, ns2 := stack.settings.Ns[0], stack.settings.Ns[1]
		net1, apex1 := stack.network(t, ctx, "")
		stack.awaitLatest(t, ctx)
		dump := takeDnsT3Dump(t, ctx)
		if _, err := model.DnsWrite(ctx, &model.DnsWriteArgs{Root: stack.root, NetworkId: net1, Upserts: []dnsservice.DnsRecordSet{dnsT3Txt("after."+apex1, "after")}}); err != nil {
			t.Fatal(err)
		}
		stack.awaitLatest(t, ctx)
		if err := stack.harness.StopEdge(ns2); err != nil {
			t.Fatal(err)
		}
		dump.restore(t, ctx)
		step := stack.restoreStep(t, &dnshost.RestoreStore{}, &dnshost.RestoreStore{})
		incidentId := sidecar.NewId()
		waitCtx, cancelWait := context.WithTimeout(ctx, dnsT3WaitTimeout)
		defer cancelWait()
		progress, err := step.Run(waitCtx, stack.root, incidentId)
		if err != nil || !progress.Done || !slices.Equal(progress.StoppedNs, []string{ns2}) || len(progress.WaitingNs) != 0 {
			t.Fatalf("with ns2 down: %v %+v", err, progress)
		}
		if err := stack.harness.AwaitServed(waitCtx, stack.root, progress.RestoreSerial, ns1); err != nil {
			t.Fatal(err)
		}
		if err := model.SyncDnsNameServers(ctx, stack.root, stack.settings.Ns); err != nil {
			t.Fatal(err)
		}
		if stopped, err := model.GetStoppedDnsNameServers(ctx, stack.root); err != nil || !slices.Equal(stopped, []string{ns2}) {
			t.Fatalf("configuration reread lost stopped authority: %v %v", stopped, err)
		}
		// ns2 reloads, and the rerun ends the step
		stack.harness.SetRequireReload(ns2, true)
		if err := stack.net.Restart(stack.harness.EdgeProcess(ns2), stack.harness.EdgeProcess(ns2)); err != nil {
			t.Fatal(err)
		}
		if err := stack.harness.AwaitServed(waitCtx, stack.root, progress.RestoreSerial, ns2); err != nil {
			t.Fatal(err)
		}
		progress, err = step.Run(waitCtx, stack.root, incidentId)
		if err != nil || !progress.Done || len(progress.StoppedNs) != 0 || len(progress.WaitingNs) != 0 {
			t.Fatalf("after ns2's reload: %v %+v", err, progress)
		}
		if _, err := model.DnsWrite(ctx, &model.DnsWriteArgs{Root: stack.root, NetworkId: net1, Upserts: []dnsservice.DnsRecordSet{dnsT3Txt("reopened."+apex1, "reopened")}}); err != nil {
			t.Fatal(err)
		}
		stack.awaitLatest(t, ctx)
	})
}
