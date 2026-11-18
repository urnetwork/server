//go:build sidecar_pending

package main

// §22.5's §12 row on T3 (DNS-M5-24; R-DNS-7, R-DNS-10, D-DNS-3, D-DNS-5): sidecartest's DnsHarness with its block on
// dnshost.Store over the private stack's Postgres and Redis, written through model's one write path, and both edges
// following the block over its feed as production's two NS do:
//
//   - both NS answer alike at one serial, and sign under DO;
//   - a failed or drained interface leaves *, @, every copy of *'s records and each mx on both NS;
//   - a key set failing its self-check never serves: the edges keep the window that passed, and an edge with no window
//     of its own serves nothing until the block names one that passes;
//   - R-DNS-7: a challenge written is served at both NS at p99 within 60 s on the private stack;
//   - CC4: a network's private name and the edges' keys reach no answer, no query, no metric and no local snapshot of
//     an edge, and a resolver's address no metric (I-13);
//   - §22.4's CROSSSLOT lint over the dns host's and model's Redis calls, read from the source. Its runtime half needs
//     T3's 3-master cluster, which test-env.sh does not attest yet (QD-028), so it is not run here.
//
// Each test has a root of its own, so its {dns}serial key and its rows share nothing with another test's.

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/crossslot"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/dnshost"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarhost"
)

// How long a T3 test waits for the stack to reach a state: a bound for a failure, never a pace.
const dnsT3WaitTimeout = 2 * time.Minute

// The private stack's dns: a root of its own with §12.7's <root> set reconciled through model, a harness on
// dnshost.Store whose two edges serve it, and the store.
type dnsT3 struct {
	net       *sidecartest.Net
	harness   *sidecartest.DnsHarness
	store     *dnshost.Store
	root      string
	settings  *dnsservice.DnsSettings
	directory string
}

// A root no other test holds: one label of its own below .example.
func newDnsT3Root() string {
	id := strings.ReplaceAll(server.NewId().String(), "-", "")
	return "t" + id[len(id)-16:] + ".example."
}

// Starts the stack for one test. change, when set, adjusts the harness's settings before it starts.
func newDnsT3(t testing.TB, ctx context.Context, change func(settings *sidecartest.DnsHarnessSettings)) *dnsT3 {
	t.Helper()
	// the store's raise wakes the block's holds through keyspace events
	server.Testing_EnableKeyspaceNotifications(ctx)
	directory := t.TempDir()
	root := newDnsT3Root()
	harnessSettings := sidecartest.DefaultDnsHarnessSettings()
	settings := harnessSettings.Dns
	settings.Roots = []string{root}
	settings.RootRecords = map[string]*dnsservice.DnsRootRecordSettings{root: {ReportMx: "rpt.report.example", ApexEdge: true}}
	if err := settings.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := model.DnsReconcileRootRecords(ctx, settings); err != nil {
		t.Fatal(err)
	}
	store, err := dnshost.NewStore(sidecarhost.NewRedis(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	net := sidecartest.NewNet(t.Context(), nil)
	t.Cleanup(net.Close)
	harnessSettings.Directory = directory
	harnessSettings.Store = store
	harnessSettings.Challenges = dnshost.NewChallenges()
	harnessSettings.SeedApex = false
	if change != nil {
		change(harnessSettings)
	}
	startCtx, cancel := context.WithTimeout(ctx, dnsT3WaitTimeout)
	defer cancel()
	harness, err := sidecartest.NewDnsHarness(startCtx, net, harnessSettings)
	if err != nil {
		t.Fatal(err)
	}
	// before the directory goes, which an edge writes until it stops
	t.Cleanup(harness.Close)
	return &dnsT3{net: net, harness: harness, store: store, root: root, settings: settings, directory: directory}
}

// The root's serial in Postgres now.
func (self *dnsT3) serial(t testing.TB, ctx context.Context) uint64 {
	t.Helper()
	state, err := model.GetDnsRootState(ctx, self.root)
	if err != nil {
		t.Fatal(err)
	}
	return state.Serial
}

// Waits until the edges named, both when none is, serve the root at its serial now.
func (self *dnsT3) awaitLatest(t testing.TB, ctx context.Context, nsLabels ...string) uint64 {
	t.Helper()
	serial := self.serial(t, ctx)
	waitCtx, cancel := context.WithTimeout(ctx, dnsT3WaitTimeout)
	defer cancel()
	if err := self.harness.AwaitServed(waitCtx, self.root, serial, nsLabels...); err != nil {
		t.Fatal(err)
	}
	return serial
}

// A network with its label in the root and its zone written through model: §12.7's @ and * with edge records, mail
// on, so mx answers the router interfaces, and a user's SRV below blog, which makes blog an empty non-terminal that
// takes a copy of *'s records (D-DNS-11). Returns the network and its zone's apex.
func (self *dnsT3) network(t testing.TB, ctx context.Context, networkName string) (server.Id, string) {
	t.Helper()
	networkId := server.NewId()
	if networkName == "" {
		networkName = "dnst3-" + strings.ToLower(strings.ReplaceAll(networkId.String()[24:], "-", ""))
	}
	model.Testing_CreateNetwork(ctx, networkId, networkName, server.NewId())
	label, err := model.EnsureNetworkLabel(ctx, networkId, self.root)
	if err != nil {
		t.Fatal(err)
	}
	apex := label + "." + self.root
	if _, err := model.DnsSyncEdgeRecords(ctx, networkId, self.root, true, true); err != nil {
		t.Fatal(err)
	}
	if _, err := model.DnsSetMailRecords(ctx, networkId, self.root, true); err != nil {
		t.Fatal(err)
	}
	if _, err := model.DnsWrite(ctx, &model.DnsWriteArgs{Root: self.root, NetworkId: networkId, Upserts: []dnsservice.DnsRecordSet{{
		Owner: "_sip._tcp.blog." + apex, Type: dns.TypeSRV, Ttl: 300, Kind: dnsservice.DnsRecordKindUser, Rdata: []string{"10 5 5060 sip.call.example."},
	}}}); err != nil {
		t.Fatal(err)
	}
	return networkId, apex
}

// What ns answers for name and rrType over TCP, with DO as asked.
func (self *dnsT3) query(t testing.TB, ctx context.Context, ns string, name string, rrType uint16, signed bool) *dns.Msg {
	t.Helper()
	message := &dns.Msg{}
	message.SetQuestion(name, rrType)
	message.SetEdns0(1232, signed)
	reply, err := self.harness.Query(ctx, ns, message)
	if err != nil {
		t.Fatalf("%s %s at %s: %v", name, dns.TypeToString[rrType], ns, err)
	}
	return reply
}

// A section's records but RRSIGs, one line each with the TTL left out, sorted.
func dnsT3Lines(records []dns.RR) []string {
	lines := []string{}
	for _, record := range records {
		if record.Header().Rrtype == dns.TypeRRSIG {
			continue
		}
		copied := dns.Copy(record)
		copied.Header().Ttl = 0
		lines = append(lines, copied.String())
	}
	slices.Sort(lines)
	return lines
}

// The addresses in an answer's A and AAAA records, sorted.
func dnsT3Addresses(reply *dns.Msg) []string {
	addresses := []string{}
	for _, record := range reply.Answer {
		switch typed := record.(type) {
		case *dns.A:
			addresses = append(addresses, typed.A.String())
		case *dns.AAAA:
			addresses = append(addresses, typed.AAAA.String())
		}
	}
	slices.Sort(addresses)
	return addresses
}

// Both NS answer every name alike from one serial, the root's, and under DO each answer's RRsets come signed.
func TestDnsT3BothNsAnswerAlikeAtOneSerial(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		stack := newDnsT3(t, ctx, nil)
		networkId, apex := stack.network(t, ctx, "")
		serial := stack.awaitLatest(t, ctx)
		questions := []struct {
			name   string
			rrType uint16
		}{
			{name: stack.root, rrType: dns.TypeSOA},
			{name: stack.root, rrType: dns.TypeNS},
			{name: stack.root, rrType: dns.TypeMX},
			{name: stack.root, rrType: dns.TypeCAA},
			{name: "_spf." + stack.root, rrType: dns.TypeTXT},
			{name: "_dmarc." + stack.root, rrType: dns.TypeTXT},
			{name: apex, rrType: dns.TypeMX},
			{name: apex, rrType: dns.TypeA},
			{name: "www." + apex, rrType: dns.TypeA},
			{name: "www." + apex, rrType: dns.TypeAAAA},
			{name: "blog." + apex, rrType: dns.TypeA},
			{name: "mx." + apex, rrType: dns.TypeA},
			{name: "_sip._tcp.blog." + apex, rrType: dns.TypeSRV},
			{name: "_dmarc." + apex, rrType: dns.TypeTXT},
			{name: "nobody." + stack.root, rrType: dns.TypeA},
		}
		for _, question := range questions {
			replies := map[string]*dns.Msg{}
			for _, ns := range stack.settings.Ns {
				replies[ns] = stack.query(t, ctx, ns, question.name, question.rrType, false)
			}
			first, second := replies[stack.settings.Ns[0]], replies[stack.settings.Ns[1]]
			if first.Rcode != second.Rcode || !slices.Equal(dnsT3Lines(first.Answer), dnsT3Lines(second.Answer)) || !slices.Equal(dnsT3Lines(first.Ns), dnsT3Lines(second.Ns)) {
				t.Errorf("%s %s: the NS differ\n%v\n%v", question.name, dns.TypeToString[question.rrType], first, second)
			}
			if !first.Authoritative || (first.Rcode != dns.RcodeSuccess && first.Rcode != dns.RcodeNameError) {
				t.Errorf("%s %s: %v", question.name, dns.TypeToString[question.rrType], first)
			}
			// under DO both sign what they answer
			for _, ns := range stack.settings.Ns {
				signed := stack.query(t, ctx, ns, question.name, question.rrType, true)
				if !slices.ContainsFunc(append(append([]dns.RR{}, signed.Answer...), signed.Ns...), func(record dns.RR) bool {
					return record.Header().Rrtype == dns.TypeRRSIG
				}) {
					t.Errorf("%s %s at %s: unsigned under DO: %v", question.name, dns.TypeToString[question.rrType], ns, signed)
				}
			}
		}
		for _, ns := range stack.settings.Ns {
			soa := stack.query(t, ctx, ns, stack.root, dns.TypeSOA, false)
			if len(soa.Answer) != 1 || soa.Answer[0].(*dns.SOA).Serial != uint32(serial) {
				t.Errorf("%s's SOA %v, want serial %d", ns, soa.Answer, serial)
			}
		}
		// a write moves both on together
		if _, err := model.DnsWrite(ctx, &model.DnsWriteArgs{Root: stack.root, NetworkId: networkId, Upserts: []dnsservice.DnsRecordSet{{
			Owner: "www." + apex, Type: dns.TypeTXT, Ttl: 300, Kind: dnsservice.DnsRecordKindUser, Rdata: []string{`"both"`},
		}}}); err != nil {
			t.Fatal(err)
		}
		next := stack.awaitLatest(t, ctx)
		if next != serial+1 {
			t.Errorf("serial %d after %d", next, serial)
		}
		for _, ns := range stack.settings.Ns {
			reply := stack.query(t, ctx, ns, "www."+apex, dns.TypeTXT, false)
			if len(reply.Answer) != 1 || reply.Answer[0].(*dns.TXT).Txt[0] != "both" {
				t.Errorf("%s after the write: %v", ns, reply)
			}
		}
	})
}

// A failed interface and a drained one each leave *, @, the copy at blog and mx on both NS, a router leaving mx too,
// and each comes back once it answers or is undrained (§22.5's §12 row; §12.6's health drops).
func TestDnsT3FailedOrDrainedInterfaceLeavesEveryAnswerOnBothNs(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		stack := newDnsT3(t, ctx, nil)
		_, apex := stack.network(t, ctx, "")
		stack.awaitLatest(t, ctx)
		check := func(what string, wantEdge []string, wantRouter []string) {
			t.Helper()
			for _, ns := range stack.settings.Ns {
				for _, name := range []string{"www." + apex, apex, "blog." + apex} {
					if got := dnsT3Addresses(stack.query(t, ctx, ns, name, dns.TypeA, false)); !slices.Equal(got, wantEdge) {
						t.Errorf("%s: %s answers %s with %v, want %v", what, ns, name, got, wantEdge)
					}
				}
				if got := dnsT3Addresses(stack.query(t, ctx, ns, "mx."+apex, dns.TypeA, false)); !slices.Equal(got, wantRouter) {
					t.Errorf("%s: %s answers mx with %v, want %v", what, ns, got, wantRouter)
				}
			}
		}
		all := []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"}
		check("all healthy", all, all[1:])
		probeCtx, cancel := context.WithTimeout(ctx, dnsT3WaitTimeout)
		defer cancel()
		if err := stack.harness.FailInterface(probeCtx, "lb-2"); err != nil {
			t.Fatal(err)
		}
		check("lb-2 failed", []string{"192.0.2.1", "192.0.2.3"}, all[2:])
		if err := stack.harness.RestoreInterface(probeCtx, "lb-2"); err != nil {
			t.Fatal(err)
		}
		check("lb-2 restored", all, all[1:])
		if err := stack.harness.DrainInterface(probeCtx, "lb-3"); err != nil {
			t.Fatal(err)
		}
		check("lb-3 drained", all[:2], all[1:2])
		if err := stack.harness.UndrainInterface(probeCtx, "lb-3"); err != nil {
			t.Fatal(err)
		}
		check("lb-3 back", all, all[1:])
	})
}

// A window whose KSK signature does not verify, as a fault or a forger would release one.
func dnsT3CorruptWindow(window *dnsservice.DnsKeyWindow) *dnsservice.DnsKeyWindow {
	corrupt := *window
	corrupt.Rrsigs = slices.Clone(window.Rrsigs)
	fields := strings.Fields(corrupt.Rrsigs[0])
	signature, err := base64.StdEncoding.DecodeString(fields[len(fields)-1])
	if err != nil {
		panic(err)
	}
	signature[0] ^= 0xff
	fields[len(fields)-1] = base64.StdEncoding.EncodeToString(signature)
	corrupt.Rrsigs[0] = strings.Join(fields, " ")
	return &corrupt
}

// The inception of the DNSKEY RRSIG ns serves: which window it serves.
func (self *dnsT3) dnskeyInception(t testing.TB, ctx context.Context, ns string) uint32 {
	t.Helper()
	reply := self.query(t, ctx, ns, self.root, dns.TypeDNSKEY, true)
	for _, record := range reply.Answer {
		if rrsig, ok := record.(*dns.RRSIG); ok && rrsig.TypeCovered == dns.TypeDNSKEY {
			return rrsig.Inception
		}
	}
	t.Fatalf("%s serves no DNSKEY RRSIG: %v", ns, reply)
	return 0
}

// An http.RoundTripper of one function.
type dnsT3RoundTrip func(request *http.Request) (*http.Response, error)

func (self dnsT3RoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return self(request)
}

// A key set failing its self-check never serves (§12.6, §22.5's §12 row): once the block names a window whose KSK
// signature does not verify, the edges keep serving the window that passed, an edge started with no snapshot serves
// nothing at all, and once the block names a good window again every edge serves it.
func TestDnsT3KeySetFailingSelfCheckNeverServes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		stack := newDnsT3(t, ctx, nil)
		networkId, apex := stack.network(t, ctx, "")
		stack.awaitLatest(t, ctx)
		before := map[string]uint32{}
		for _, ns := range stack.settings.Ns {
			before[ns] = stack.dnskeyInception(t, ctx, ns)
		}
		write := func(text string) {
			t.Helper()
			if _, err := model.DnsWrite(ctx, &model.DnsWriteArgs{Root: stack.root, NetworkId: networkId, Upserts: []dnsservice.DnsRecordSet{{
				Owner: "keys." + apex, Type: dns.TypeTXT, Ttl: 300, Kind: dnsservice.DnsRecordKindUser, Rdata: []string{`"` + text + `"`},
			}}}); err != nil {
				t.Fatal(err)
			}
		}

		// a week on, the block names the next window, which fails: both NS keep the one that passed
		stack.harness.SetKeyWindowFault(dnsT3CorruptWindow)
		stack.net.Clock().Advance(7 * 24 * time.Hour)
		write("after the fault")
		stack.awaitLatest(t, ctx)
		for _, ns := range stack.settings.Ns {
			if inception := stack.dnskeyInception(t, ctx, ns); inception != before[ns] {
				t.Errorf("%s serves a window from %d, not the one that passed from %d", ns, inception, before[ns])
			}
		}

		// an edge that must reload, as a stopped NS starts (§19.10), may serve only a view the block's window
		// checks, so it serves nothing while that window fails, though its own snapshot holds the one that passed
		ns := stack.settings.Ns[1]
		stack.harness.SetRequireReload(ns, true)
		restart := func() {
			t.Helper()
			if err := stack.net.Kill(stack.harness.EdgeProcess(ns)); err != nil {
				t.Fatal(err)
			}
			if err := stack.net.Restart(stack.harness.EdgeProcess(ns), stack.harness.EdgeProcess(ns)); err != nil {
				t.Fatal(err)
			}
		}
		restart()
		// a write answers its poll at once, so its reload runs with no clock moved; a failed check is counted just
		// before the loop sets its backoff timer, which wakes this wait
		write("for the restart")
		waitCtx, cancel := context.WithTimeout(ctx, dnsT3WaitTimeout)
		defer cancel()
		for {
			_, timerAdded := stack.net.Clock().WatchTimers()
			if failed := stack.harness.EdgeMetrics(ns).Value("urnetwork_dnsedge_self_checks_total", "fail"); 1 <= failed {
				break
			}
			select {
			case <-timerAdded:
			case <-waitCtx.Done():
				t.Fatalf("%s never checked the failing window: %v", ns, waitCtx.Err())
			}
		}
		if edge := stack.harness.Edge(ns); edge == nil || edge.Ready() {
			t.Fatalf("an edge with only a failing window is ready")
		}
		message := &dns.Msg{}
		message.SetQuestion("www."+apex, dns.TypeA)
		if reply, err := stack.harness.Query(ctx, ns, message); err == nil {
			t.Errorf("an edge with only a failing window answered %v", reply)
		}

		// the block names a good window again: the running NS takes it, and the restarted one reloads and serves it
		stack.harness.SetKeyWindowFault(nil)
		restart()
		write("after the fix")
		stack.awaitLatest(t, ctx)
		for _, ns := range stack.settings.Ns {
			if inception := stack.dnskeyInception(t, ctx, ns); inception == before[ns] {
				t.Errorf("%s still serves the first window", ns)
			}
		}
	})
}

// R-DNS-7 on the private stack: a challenge certs writes through the block is served at both NS, at p99 within 60 s.
// The time measured is real, from the block's answer to both edges serving the serial, while every timer of the stack
// is held on the Net's clock, so nothing waits on a timer and the time is the write path's own.
func TestDnsT3ChallengeServedAtBothNsWithinSixtySeconds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		// every write is the one challenge name a label has, so the hourly cap is raised past the test's count
		defer model.Testing_SetDnsChallengesPerNameHour(1000)()
		stack := newDnsT3(t, ctx, nil)
		_, apex := stack.network(t, ctx, "")
		stack.awaitLatest(t, ctx)
		const writeCount = 100
		durations := []time.Duration{}
		for index := range writeCount {
			start := time.Now()
			serial, err := stack.harness.PutChallenge(ctx, stack.root, apex, []string{fmt.Sprintf("digest-%03d-0123456789abcdefghijklmnopqrstuvw", index)})
			if err != nil {
				t.Fatal(err)
			}
			waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			err = stack.harness.AwaitServed(waitCtx, stack.root, serial)
			cancel()
			if err != nil {
				t.Fatalf("challenge %d was not served on both NS within 60 s: %v", index, err)
			}
			durations = append(durations, time.Since(start))
		}
		slices.Sort(durations)
		p50 := durations[writeCount/2]
		p99 := durations[(writeCount*99+99)/100-1]
		t.Logf("R-DNS-7 on the private stack: %d challenges, p50 %s, p99 %s, max %s", writeCount, p50, p99, durations[writeCount-1])
		if 60*time.Second < p99 {
			t.Errorf("p99 %s is past R-DNS-7's 60 s", p99)
		}
	})
}

// §22.7's CC4 with the dns edge queries: a network's name, private behind its opaque label (§12.5), and the edges'
// keys reach none of the queries the edges take, their answers, their metrics or their local snapshots, and the
// address a resolver asks from reaches no metric (I-13, dns-2).
func TestDnsT3NoNameOrKeyReachesTheEdgesQueriesOrMetrics(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		stack := newDnsT3(t, ctx, nil)
		capture := stack.net.Capture()
		networkName := "c" + strings.ToLower(capture.CanaryText("network name"))
		_, apex := stack.network(t, ctx, networkName)
		stack.awaitLatest(t, ctx)
		resolver := netip.MustParseAddr("198.51.100.77")
		capture.Forbid("resolver address", []byte(resolver.String()))
		for _, ns := range stack.settings.Ns {
			for _, name := range []string{apex, "www." + apex, "mx." + apex, "_dmarc." + apex, "nobody." + apex, stack.root} {
				for _, signed := range []bool{false, true} {
					message := &dns.Msg{}
					message.SetQuestion(name, dns.TypeANY)
					message.SetEdns0(1232, signed)
					reply, err := stack.harness.QueryUdp(ctx, ns, message, resolver)
					if err != nil {
						t.Fatal(err)
					}
					query, _ := message.Pack()
					answer, _ := reply.Pack()
					capture.Add("CC4", "dns edge query at "+ns, query)
					capture.Add("CC4", "dns edge answer at "+ns, answer)
				}
			}
		}
		// each edge's local snapshot, written once it stops
		stack.harness.Close()
		for _, ns := range stack.settings.Ns {
			filepath.WalkDir(filepath.Join(stack.directory, stack.harness.EdgeProcess(ns)), func(path string, entry os.DirEntry, err error) error {
				if err != nil || entry.IsDir() {
					return err
				}
				file, err := os.Open(path)
				if err != nil {
					return err
				}
				defer file.Close()
				body, err := io.ReadAll(file)
				if err != nil {
					return err
				}
				capture.Add("CC4", "dns edge state "+filepath.Base(path), body)
				return nil
			})
		}
		if capture.ItemCount("CC4") == 0 {
			t.Fatal("nothing captured")
		}
		for _, finding := range capture.Scan() {
			t.Errorf("%v", finding)
		}
	})
}

// §22.4's CROSSSLOT lint, statically, over the dns host and model's dns files: every multi-key call and script
// names its keys under one hash tag. The runtime half sends each call to T3's 3 masters, which test-env.sh does not
// attest until QD-028's change lands, so it is not run here.
func TestDnsT3RedisCallsNameOneHashTag(t *testing.T) {
	findings, err := crossslot.Check("../..", "./dnshost", "./model")
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range findings {
		file := filepath.Base(strings.SplitN(finding.Position, ":", 2)[0])
		if strings.HasPrefix(file, "dns_") || strings.Contains(finding.Position, "dnshost") {
			t.Errorf("%v", finding)
		}
	}
}
