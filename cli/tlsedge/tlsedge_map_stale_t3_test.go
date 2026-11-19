//go:build sidecar_pending

package main

// FS-122's readiness across two lb interfaces through the real splitter (TLS-M6-07; FS-122, FI-11, §13.4): two splits,
// each warp's lb process with lbmap over its own real nginx, both in the dns API's inventory, before one tlsedge block
// on the private stack, with a dns edge stand-in that runs §13.4's route probes as an edge does: a ClientHello offering
// ur-route-probe then http/1.1 for each interface's reported version's probe name and for each host that version added,
// through that interface's split, a pass being tlsedge's unrecognized_name, posted to the API's route-probe route. Two
// interfaces, since this is the one test that sees one interface behind while the other's probes pass.
//
//   - a host both interfaces loaded and probed turns ready;
//   - a host added while one interface's lb is stopped stays pending in the API's readiness, naming that interface
//     alone, while the other loads it, routes it and passes its probes; dns.yml's map windows judge the stored state
//     map-stale for the stopped interface past stale_after_s, never for the one that reported the newest version, and
//     for neither while the version is young;
//   - the stopped lb restarted loads the current version before it serves 443, passes the next round, and the host
//     turns ready.
//
//   - with nginx -t refusing the config, a host added stays out of the split, lbmap reporting the version nginx still
//     runs, and the host stays pending naming the interface; once nginx -t passes again lbmap loads it and it turns
//     ready.
//
//   - a false report: an lbmap that reports a version its nginx did not load, whose master and added hosts it names,
//     is current for no version, since that version's probe name routes to tlsedge only through a map of that version
//     and fails through the one nginx runs, as each host it says it added does; every host stays pending naming the
//     interface, and once its lbmap reaches the API again its first poll takes the report away and the version loads
//     for real, the hosts turning ready.
//   - a dropped host: an interface whose nginx runs a map that routes the version's probe name but drops a host the
//     version added fails that host's probe round after round, the host pending naming the interface, until lbmap's
//     load of the next version, which it renders whole, routes the host again and its probes pass.
//
// The edge stand-in probes what the feed gives an edge, the store's report for each interface, so a report lbmap never
// made is probed as an edge would probe it. TLS-ALPN-01 orders waiting on host_ready are tlsedge_custom_route_t3_test.go's.
// What waits: map-stale's page and the dns edges leaving the stale interface out of their answers, which DNS-M6-03 and
// §19.8 build. The test steps on lbmap's reports and the API's answers; the probes' hellos are the only traffic, sent as
// the edge would.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/tlsedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Whether a route probe for name through split passes: tlsedge's unrecognized_name alert alone, then the close, which
// the http branch never gives. A first record that is no alert is the http branch's ServerHello, and the probe reads no
// further, since that branch then waits for the rest of a handshake the probe never sends.
func (self *t3Split) routeProbe(name string) bool {
	t := self.world.t
	t.Helper()
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", self.port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(t3EdgeGuardTimeout))
	if _, err := conn.Write(t3EdgeHello(t, name, []string{"ur-route-probe", "http/1.1"})); err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 5)
	if _, err := io.ReadFull(conn, header); err != nil || header[0] != 21 {
		return false
	}
	rest, err := io.ReadAll(conn)
	return err == nil && bytes.Equal(append(header, rest...), t3EdgeAlert(tlsedgeservice.TlsAlertUnrecognizedName))
}

// The probe name of version, from the digest dns_custom_host_map keeps for it.
func t3SplitProbeName(t testing.TB, world *t3EdgeWorld, version uint64) string {
	t.Helper()
	var digest []byte
	server.Db(world.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(world.ctx, `SELECT digest FROM dns_custom_host_map WHERE version = $1`, int64(version)).Scan(&digest))
	})
	return dnsservice.DnsCustomHostMapProbeName(hex.EncodeToString(digest))
}

// One round of the dns edge's route probes, from what the feed gives an edge (dnsservice's fillMapInterfaces): for each
// split, the version the store holds as its interface's report, unless it reported none or fell silent, that version's
// probe name and each host the report added, each probed through the split, posted to the API. So a report lbmap never
// made is probed as an edge would probe it.
func (self *t3SplitApi) probeRound(t testing.TB, world *t3EdgeWorld, splits []*t3Split) {
	t.Helper()
	state, err := model.GetDnsCustomHostMapState(world.ctx, world.root)
	if err != nil {
		t.Fatal(err)
	}
	pollSilence := time.Duration(dnsservice.DefaultDnsSettings().Map.PollSilenceSeconds) * time.Second
	report := &dnsservice.DnsRouteProbeReport{Format: dnsservice.DnsFormat}
	for _, split := range splits {
		index := slices.IndexFunc(state.Interfaces, func(lbInterface *dnsservice.DnsMapInterface) bool {
			return lbInterface.Name == split.interfaceName
		})
		if index < 0 {
			continue
		}
		lbInterface := state.Interfaces[index]
		if lbInterface.ReportedVersion == 0 || lbInterface.PollTime.IsZero() || pollSilence < time.Since(lbInterface.PollTime) {
			continue
		}
		probeName := dnsservice.DnsCustomHostMapProbeName(lbInterface.Digest)
		result := dnsservice.DnsRouteProbeResult{
			Name:        split.interfaceName,
			Version:     lbInterface.ReportedVersion,
			ProbeName:   probeName,
			ProbePassed: split.routeProbe(probeName),
		}
		for _, host := range lbInterface.AddedHosts {
			result.Hosts = append(result.Hosts, dnsservice.DnsRouteProbeHost{Host: host, Passed: split.routeProbe(host)})
		}
		report.Interfaces = append(report.Interfaces, result)
	}
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(world.ctx, http.MethodPost, self.url+dnsservice.DnsRouteProbesPath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+self.edgeBearer)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("the route probes' report answered %d", response.StatusCode)
	}
}

// host's readiness as GET /network/dns reads it.
func t3SplitReadiness(t testing.TB, world *t3EdgeWorld, host string) *model.DnsCustomHostReadinessRow {
	t.Helper()
	rows, err := model.DnsCustomHostReadiness(world.ctx, world.root, server.Id(world.networkId))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Host == host {
			return row
		}
	}
	t.Fatalf("no readiness for %s", host)
	return nil
}

// Waits until host reads ready, as the API sets it after a round.
func t3SplitAwaitReady(t testing.TB, world *t3EdgeWorld, host string) {
	t.Helper()
	guard := time.Now().Add(t3EdgeGuardTimeout)
	for {
		if row := t3SplitReadiness(t, world, host); row.ReadyTime != nil {
			return
		}
		if time.Now().After(guard) {
			t.Fatalf("%s never turned ready: %+v", host, t3SplitReadiness(t, world, host))
		}
	}
}

func TestT3Fs122AHostAddedWhileOneInterfaceIsDownStaysPendingNamingIt(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3EdgeWorld(t, nil)
		first, second := t3CustomHost(t, "app"), t3CustomHost(t, "shop")
		hosts := []string{first, second}
		interfaces := []string{"edge-1-eth0", "edge-2-eth0"}
		api := newT3SplitApi(t, world, interfaces)
		version := world.mapCustomHost(first)
		world.reloadNames()
		up := newT3SplitOn(t, world, api, interfaces[0], hosts)
		down := newT3SplitOn(t, world, api, interfaces[1], hosts)
		up.startLb(version)
		down.startLb(version)
		splits := []*t3Split{up, down}

		// both interfaces load and pass: the host turns ready
		api.probeRound(t, world, splits)
		t3SplitAwaitReady(t, world, first)

		// one lb stops; a host is added: the other loads and routes it, and it stays pending naming the stopped one
		down.stopLb()
		version = world.mapCustomHost(second)
		world.reloadNames()
		up.reports.await(t, version)
		if !up.reachesDevice(second, []string{"h2"}, up.customPool) {
			t.Fatal("the running interface did not route the new host")
		}
		api.probeRound(t, world, []*t3Split{up})
		row := t3SplitReadiness(t, world, second)
		if row.ReadyTime != nil || !slices.Equal(row.PendingInterfaces, []string{interfaces[1]}) {
			t.Fatalf("the host added with one interface down: %+v", row)
		}
		if ready := t3SplitReadiness(t, world, first); ready.ReadyTime == nil {
			t.Fatalf("a host both had loaded went pending: %+v", ready)
		}
		// map-stale, judged by dns.yml's map windows over the stored state: none while the version is young; past
		// stale_after_s the stopped interface is stale and the one that reported the newest version is never left out,
		// though past poll_silence_s it is not current either (§13.4's never the last one)
		state, err := model.GetDnsCustomHostMapState(world.ctx, world.root)
		if err != nil {
			t.Fatal(err)
		}
		mapSettings := dnsservice.DefaultDnsSettings().Map
		if judgment := mapSettings.Judge(state, time.Now()); 0 < len(judgment.MapStale) {
			t.Fatalf("map-stale while the version is young: %v", judgment.MapStale)
		}
		later := time.Now().Add(time.Duration(mapSettings.StaleAfterSeconds+60) * time.Second)
		if judgment := mapSettings.Judge(state, later); !judgment.MapStale[interfaces[1]] || judgment.MapStale[interfaces[0]] {
			t.Fatalf("map-stale past stale_after_s: %v", judgment.MapStale)
		}

		// the stopped lb starts again, loads the current version before it serves, passes, and the host turns ready
		down.startLb(version)
		if !down.reachesDevice(second, []string{"h2"}, down.customPool) {
			t.Fatal("the restarted interface did not route the new host from its first connection")
		}
		api.probeRound(t, world, splits)
		t3SplitAwaitReady(t, world, second)
	})
}

// FS-122's nginx -t clause: while the config nginx -t reads is one it refuses, lbmap keeps the map nginx runs and reports
// that version, so a host added then reaches the http branch and stays pending naming the interface; once nginx -t
// passes again lbmap loads the version, the host routes, passes its probe and turns ready.
func TestT3Fs122AMapNginxRefusesKeepsTheHostPending(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3EdgeWorld(t, nil)
		first, second := t3CustomHost(t, "app"), t3CustomHost(t, "shop")
		api := newT3SplitApi(t, world, []string{t3SplitInterface})
		version := world.mapCustomHost(first)
		world.reloadNames()
		split := newT3SplitOn(t, world, api, t3SplitInterface, []string{first, second})
		split.startLb(version)
		api.probeRound(t, world, []*t3Split{split})
		t3SplitAwaitReady(t, world, first)

		// the config breaks under nginx: lbmap's nginx -t refuses every load from here
		config, err := os.ReadFile(split.configPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(split.configPath, append(slices.Clone(config), []byte("\nthis_is_no_directive on;\n")...), 0644); err != nil {
			t.Fatal(err)
		}
		reportCount := len(split.reports.reported())
		refused := world.mapCustomHost(second)
		world.reloadNames()
		// a report follows the failed load, still naming the version nginx runs
		guard := time.Now().Add(t3EdgeGuardTimeout)
		for len(split.reports.reported()) <= reportCount {
			if time.Now().After(guard) {
				t.Fatal("lbmap reported nothing after the refused load")
			}
			runtime.Gosched()
		}
		if reported := split.reports.reported(); reported[len(reported)-1] != version {
			t.Fatalf("lbmap reported %v with nginx -t refusing %d", reported, refused)
		}
		if !split.reachesPlatform(second) {
			t.Fatal("a host nginx -t refused routed")
		}
		api.probeRound(t, world, []*t3Split{split})
		if row := t3SplitReadiness(t, world, second); row.ReadyTime != nil || !slices.Equal(row.PendingInterfaces, []string{t3SplitInterface}) {
			t.Fatalf("the refused host: %+v", row)
		}

		// nginx -t passes again: lbmap loads the version on its next try
		if err := os.WriteFile(split.configPath, config, 0644); err != nil {
			t.Fatal(err)
		}
		split.reports.await(t, refused)
		if !split.reachesDevice(second, []string{"h2"}, split.customPool) {
			t.Fatal("the host did not route once nginx -t passed")
		}
		api.probeRound(t, world, []*t3Split{split})
		t3SplitAwaitReady(t, world, second)
	})
}

// Has this interface's nginx run a map that drops host and keeps the rest, its version's probe name among them, as a
// load gone wrong past lbmap's own checks would leave it: the map file rewritten through a rename, as lbmap writes it,
// and the master lbmap last reported sent SIGHUP, lbmap told nothing. Returns once a hello for host reaches the http
// branch through the split.
func (self *t3Split) dropFromMap(host string) {
	t := self.world.t
	t.Helper()
	path := filepath.Join(self.runDir, "tls-custom-hosts.map")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	kept := []string{}
	dropped := false
	for _, line := range strings.Split(string(data), "\n") {
		if fields := strings.Fields(line); 0 < len(fields) && fields[0] == host {
			dropped = true
			continue
		}
		kept = append(kept, line)
	}
	if !dropped {
		t.Fatalf("the interface's map holds no %s to drop", host)
	}
	temp := path + ".t3-drop"
	if err := os.WriteFile(temp, []byte(strings.Join(kept, "\n")), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temp, path); err != nil {
		t.Fatal(err)
	}
	master := self.reports.lastMaster()
	if master == nil {
		t.Fatal("lbmap reported no nginx master")
	}
	if err := syscall.Kill(master.Pid, syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	guard := time.Now().Add(t3EdgeGuardTimeout)
	for !self.reachesPlatform(host) {
		if time.Now().After(guard) {
			t.Fatalf("the dropped host still routes to tlsedge after the master's reload")
		}
		runtime.Gosched()
	}
}

// FS-122's false-report clause: an lbmap that reports a version its nginx did not load stays not current, its route
// probe failing. One interface's lbmap loses the API while its nginx serves the first version's map; a host is added,
// which the other interface loads; the lost lbmap's report then claims the new version, with its digest, the master
// nginx runs and the host it would have added, as a lying lbmap would send it. The version's probe name and the host
// fail through that interface, whose nginx sends both to the http branch, so the interface is current for no version
// and both hosts stay pending naming it alone. Once its lbmap reaches the API again, its first poll, naming the version
// nginx runs, takes the report away, the version loads for real, and both hosts turn ready.
func TestT3Fs122AnLbmapReportingAVersionItsNginxDidNotLoadStaysNotCurrent(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3EdgeWorld(t, nil)
		first, second := t3CustomHost(t, "app"), t3CustomHost(t, "shop")
		interfaces := []string{"edge-1-eth0", "edge-2-eth0"}
		api := newT3SplitApi(t, world, interfaces)
		firstVersion := world.mapCustomHost(first)
		world.reloadNames()
		honest := newT3SplitOn(t, world, api, interfaces[0], []string{first, second})
		liar := newT3SplitOn(t, world, api, interfaces[1], []string{first, second})
		honest.startLb(firstVersion)
		liar.startLb(firstVersion)
		splits := []*t3Split{honest, liar}
		api.probeRound(t, world, splits)
		t3SplitAwaitReady(t, world, first)

		// the liar's lbmap loses the API; a host is added, which the honest interface loads and routes
		api.mute(t, liar.interfaceName)
		secondVersion := world.mapCustomHost(second)
		world.reloadNames()
		honest.reports.await(t, secondVersion)
		if !honest.reachesDevice(second, []string{"h2"}, honest.customPool) {
			t.Fatal("the honest interface did not route the new host")
		}
		if !liar.reachesPlatform(second) {
			t.Fatal("the liar's nginx routes a host of a version it never loaded")
		}
		// the report the liar would send: the newest version, as if loaded into the master nginx runs
		customHostMap, err := model.GetDnsCustomHostMap(world.ctx, world.root)
		if err != nil {
			t.Fatal(err)
		}
		if customHostMap.Version != secondVersion {
			t.Fatalf("the newest version is %d, not %d", customHostMap.Version, secondVersion)
		}
		api.forgeReport(t, &dnsservice.DnsCustomHostMapReport{
			Format:    dnsservice.DnsFormat,
			Interface: liar.interfaceName,
			Version:   secondVersion,
			Digest:    customHostMap.Digest,
			Master:    liar.reports.lastMaster(),
			Added:     []string{second},
		})
		// two rounds: the version's probe name and the host fail through the liar each time, and every host stays
		// pending naming it alone
		for range 2 {
			api.probeRound(t, world, splits)
			for _, host := range []string{first, second} {
				if row := t3SplitReadiness(t, world, host); row.ReadyTime != nil || !slices.Equal(row.PendingInterfaces, []string{liar.interfaceName}) {
					t.Fatalf("%s with the liar's report standing: %+v", host, row)
				}
			}
		}
		if liar.routeProbe(dnsservice.DnsCustomHostMapProbeName(customHostMap.Digest)) {
			t.Fatal("the reported version's probe name passed through a map of another version")
		}
		if !honest.routeProbe(dnsservice.DnsCustomHostMapProbeName(customHostMap.Digest)) {
			t.Fatal("the honest interface's probe name failed")
		}

		// the liar's lbmap reaches the API again: its poll takes the report away, and it loads the version for real
		api.unmute(liar.interfaceName)
		liar.reports.await(t, secondVersion)
		if !liar.reachesDevice(second, []string{"h2"}, liar.customPool) {
			t.Fatal("the liar's interface did not route the host once it loaded the version")
		}
		api.probeRound(t, world, splits)
		t3SplitAwaitReady(t, world, first)
		t3SplitAwaitReady(t, world, second)
	})
}

// FS-122's dropped-host clause: an interface whose map routes the probe name but drops an added host fails that host's
// probe until a reload fixes it. Both interfaces load a version that adds a host; one's nginx is then made to run a
// map with that host's line gone and the version's probe name kept, lbmap none the wiser. Round after round the probe
// name passes through it and the host fails, so the host stays pending naming that interface alone; lbmap's load of
// the next version, which it renders whole from the version's hosts, routes the host again, its probes pass, and it
// turns ready beside the next version's host.
func TestT3Fs122AMapThatDropsAnAddedHostFailsItsProbeUntilAReload(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3EdgeWorld(t, nil)
		first, second, third := t3CustomHost(t, "app"), t3CustomHost(t, "shop"), t3CustomHost(t, "docs")
		hosts := []string{first, second, third}
		interfaces := []string{"edge-1-eth0", "edge-2-eth0"}
		api := newT3SplitApi(t, world, interfaces)
		version := world.mapCustomHost(first)
		world.reloadNames()
		whole := newT3SplitOn(t, world, api, interfaces[0], hosts)
		lossy := newT3SplitOn(t, world, api, interfaces[1], hosts)
		whole.startLb(version)
		lossy.startLb(version)
		splits := []*t3Split{whole, lossy}
		api.probeRound(t, world, splits)
		t3SplitAwaitReady(t, world, first)

		// a host added, which both load; then the lossy interface's nginx runs a map without it
		version = world.mapCustomHost(second)
		world.reloadNames()
		whole.reports.await(t, version)
		lossy.reports.await(t, version)
		lossy.dropFromMap(second)
		probeName := t3SplitProbeName(t, world, version)
		for range 2 {
			api.probeRound(t, world, splits)
			if row := t3SplitReadiness(t, world, second); row.ReadyTime != nil || !slices.Equal(row.PendingInterfaces, []string{lossy.interfaceName}) {
				t.Fatalf("the dropped host: %+v", row)
			}
		}
		if !lossy.routeProbe(probeName) {
			t.Fatal("the lossy map's probe name failed, so the host's probe is not what holds it back")
		}
		if !whole.reachesDevice(second, []string{"h2"}, whole.customPool) {
			t.Fatal("the whole interface stopped routing the host")
		}

		// lbmap's load of the next version writes the whole map again
		version = world.mapCustomHost(third)
		world.reloadNames()
		whole.reports.await(t, version)
		lossy.reports.await(t, version)
		if !lossy.reachesDevice(second, []string{"h2"}, lossy.customPool) {
			t.Fatal("the reload left the dropped host out")
		}
		api.probeRound(t, world, splits)
		t3SplitAwaitReady(t, world, second)
		t3SplitAwaitReady(t, world, third)
	})
}
