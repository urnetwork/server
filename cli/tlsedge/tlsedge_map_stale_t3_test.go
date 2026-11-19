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
// What waits: map-stale's page and the dns edges leaving the stale interface out of their answers, which DNS-M6-03 and
// §19.8 build, a false report and a dropped host, and TLS-ALPN-01 orders waiting on host_ready (TLS-M6-05's orderer). The test steps on lbmap's reports and the
// API's answers; the probes' hellos are the only traffic, sent as the edge would.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/tlsedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Whether a route probe for name through split passes: tlsedge's unrecognized_name, which the http branch never gives.
func (self *t3Split) routeProbe(name string) bool {
	self.world.t.Helper()
	answer := self.raw(t3EdgeHello(self.world.t, name, []string{"ur-route-probe", "http/1.1"}))
	return bytes.Equal(answer, t3EdgeAlert(tlsedgeservice.TlsAlertUnrecognizedName))
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

// One round of the dns edge's route probes: for each split, the version its lbmap last reported, that version's probe
// name and the hosts given, each probed through the split, posted to the API.
func (self *t3SplitApi) probeRound(t testing.TB, world *t3EdgeWorld, splits []*t3Split, hosts []string) {
	t.Helper()
	report := &dnsservice.DnsRouteProbeReport{Format: dnsservice.DnsFormat}
	for _, split := range splits {
		reported := split.reports.reported()
		if len(reported) == 0 {
			continue
		}
		version := reported[len(reported)-1]
		probeName := t3SplitProbeName(t, world, version)
		result := dnsservice.DnsRouteProbeResult{
			Name:        split.interfaceName,
			Version:     version,
			ProbeName:   probeName,
			ProbePassed: split.routeProbe(probeName),
		}
		for _, host := range hosts {
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
		api.probeRound(t, world, splits, []string{first})
		t3SplitAwaitReady(t, world, first)

		// one lb stops; a host is added: the other loads and routes it, and it stays pending naming the stopped one
		down.stopLb()
		version = world.mapCustomHost(second)
		world.reloadNames()
		up.reports.await(t, version)
		if !up.reachesDevice(second, []string{"h2"}, up.customPool) {
			t.Fatal("the running interface did not route the new host")
		}
		api.probeRound(t, world, []*t3Split{up}, []string{second})
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
		api.probeRound(t, world, splits, []string{second})
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
		api.probeRound(t, world, []*t3Split{split}, []string{first})
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
		api.probeRound(t, world, []*t3Split{split}, []string{second})
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
		api.probeRound(t, world, []*t3Split{split}, []string{second})
		t3SplitAwaitReady(t, world, second)
	})
}
