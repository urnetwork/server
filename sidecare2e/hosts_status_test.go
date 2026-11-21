//go:build sidecar_e2e && sidecar_pending

// Each new server service's block starts in the test process through its exported Run, on ports the OS picks, with
// the harness's throwaway credentials and none from vault, and answers GET /status (§19.4: from the first moment, ok
// once its readiness latches); then every block at once in one process, each answering, stopped cleanly with no
// goroutine it started left behind. Each test runs on its own Postgres database and Redis lease
// (server.DefaultTestEnv), with no rerun, so a flake shows.

package sidecare2e

import (
	"net/http"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/socketsservice"

	"github.com/urnetwork/server"
)

// One test's env: no rerun, so a flake shows.
func e2eTestEnv() *server.TestEnv {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	return env
}

// evs latches ready over its fake clusters with its receipt keys and the harness's svc- verifier.
func TestE2eEvsHostAnswersItsStatusOnAnOsPort(t *testing.T) {
	e2eTestEnv().Run(t, func(t testing.TB) {
		process := newE2eProcess(t)
		host := process.startEvs(t)
		process.awaitStatus(t, host, "ok", e2eStatusReady)
		process.stop(t, host)
	})
}

// The blocks verify their callers under the svc- key set their deps carry, not the process's: a token the harness's
// key signs is past authentication at sockets and evs, and one a key they were not given signs answers 401, which no
// block could say without a verifier, the fixture vault holding no svc- jwks.
func TestE2eHostsVerifyServiceTokensUnderTheirInjectedKeys(t *testing.T) {
	e2eTestEnv().Run(t, func(t testing.TB) {
		process := newE2eProcess(t)
		sockets := process.startSockets(t)
		evs := process.startEvs(t)
		process.awaitStatus(t, sockets, "ok", e2eStatusReady)
		process.awaitStatus(t, evs, "ok", e2eStatusReady)
		foreignKey, foreignKid := process.foreignServiceKey(t)
		credentials := process.credentials
		repoPath := "/evs/v1/repos/" + sidecar.EvsKeyId(sidecar.Id{7})
		for _, check := range []struct {
			host     *e2eHost
			audience string
			method   string
			path     string
			body     string
		}{
			{host: sockets, audience: socketsservice.SocketsAudience, method: http.MethodPost, path: "/socket/registrations", body: `{"set_version": 1}`},
			{host: evs, audience: evsservice.EvsAudience, method: http.MethodPut, path: repoPath},
		} {
			ours := process.serviceToken(t, credentials.serviceKey, credentials.serviceKid, check.audience)
			if code := process.routeCode(t, check.host, check.method, check.path, ours, check.body); code == http.StatusUnauthorized {
				t.Fatalf("%s refused a token its injected key signed: %d", check.host.name, code)
			}
			foreign := process.serviceToken(t, foreignKey, foreignKid, check.audience)
			if code := process.routeCode(t, check.host, check.method, check.path, foreign, check.body); code != http.StatusUnauthorized {
				t.Fatalf("%s answered %d to a token a key it was not given signed", check.host.name, code)
			}
		}
		process.stop(t, evs)
		process.stop(t, sockets)
	})
}

// gkm latches ready over its fake clusters, its authorization state on the env's Redis.
func TestE2eGkmHostAnswersItsStatusOnAnOsPort(t *testing.T) {
	e2eTestEnv().Run(t, func(t testing.TB) {
		process := newE2eProcess(t)
		host := process.startGkm(t)
		process.awaitStatus(t, host, "ok", e2eStatusReady)
		process.stop(t, host)
	})
}

// sockets latches ready on the env's Redis with its ticket key and its rev/ reader.
func TestE2eSocketsHostAnswersItsStatusOnAnOsPort(t *testing.T) {
	e2eTestEnv().Run(t, func(t testing.TB) {
		process := newE2eProcess(t)
		host := process.startSockets(t)
		process.awaitStatus(t, host, "ok", e2eStatusReady)
		process.stop(t, host)
	})
}

// tlsedge latches ready with its label map read and its K clients connected, its four ports on ports the OS picks.
func TestE2eTlsedgeHostAnswersItsStatusOnAnOsPort(t *testing.T) {
	e2eTestEnv().Run(t, func(t testing.TB) {
		process := newE2eProcess(t)
		host := process.startTlsedge(t)
		process.awaitStatus(t, host, "ok", e2eStatusReady)
		process.stop(t, host)
	})
}

// smtpedge answers /status: its client reaches no urnetwork-edge in this harness, so ur/smtpedge is not served and
// the block says it is not ready, which is the world's to change.
func TestE2eSmtpedgeHostAnswersItsStatusOnAnOsPort(t *testing.T) {
	e2eTestEnv().Run(t, func(t testing.TB) {
		process := newE2eProcess(t)
		host := process.startSmtpedge(t)
		process.awaitStatus(t, host, "up", e2eStatusAnswers)
		process.stop(t, host)
	})
}

// mxedge's router latches ready with its store probed and its K clients connected, TCP 25 on a port the OS picks.
func TestE2eMxedgeRouterHostAnswersItsStatusOnAnOsPort(t *testing.T) {
	e2eTestEnv().Run(t, func(t testing.TB) {
		process := newE2eProcess(t)
		host := process.startMxedgeRouter(t)
		process.awaitStatus(t, host, "ok", e2eStatusReady)
		process.stop(t, host)
	})
}

// mxedge's platform listener latches ready with the harness's certificate current.
func TestE2eMxedgePlatformHostAnswersItsStatusOnAnOsPort(t *testing.T) {
	e2eTestEnv().Run(t, func(t testing.TB) {
		process := newE2eProcess(t)
		host := process.startMxedgePlatform(t)
		process.awaitStatus(t, host, "ok", e2eStatusReady)
		process.stop(t, host)
	})
}

// The dns API latches ready on the env's tables, releasing the harness's key window, and an edge feeding from it over
// its bearer answers its own /status, its DNS on TCP and UDP ports the OS picks. The edge stays not ready here: in a
// run of 2026-11-21 it took no checked view from the in-process API within two minutes, which the world's tracks are
// to find out (e2e-hosts-in-process's report).
func TestE2eDnsApiAndEdgeHostsAnswerTheirStatusOnOsPorts(t *testing.T) {
	e2eTestEnv().Run(t, func(t testing.TB) {
		process := newE2eProcess(t)
		api := process.startDnsApi(t)
		process.awaitStatus(t, api, "ok", e2eStatusReady)
		edge := process.startDnsEdge(t, api)
		process.awaitStatus(t, edge, "up", e2eStatusAnswers)
		process.stop(t, edge)
		process.stop(t, api)
	})
}

// mcp latches ready on a listener the OS picked, sealing under the harness's secret.
func TestE2eMcpHostAnswersItsStatusOnAnOsPort(t *testing.T) {
	e2eTestEnv().Run(t, func(t testing.TB) {
		process := newE2eProcess(t)
		host := process.startMcp(t)
		process.awaitStatus(t, host, "ok", e2eStatusReady)
		process.stop(t, host)
	})
}

// Every block at once in one process, beside each other on the process's shared state, each answering its /status
// as alone; then every one stopped, each Run returning nil, and no goroutine any of them started left running.
func TestE2eEveryHostRunsInOneProcessAndStopsClean(t *testing.T) {
	e2eTestEnv().Run(t, func(t testing.TB) {
		process := newE2eProcess(t)
		process.openProcessPools(t)
		before := e2eGoroutines()

		dnsApi := process.startDnsApi(t)
		type expectation struct {
			host *e2eHost
			what string
			want func(status string) bool
		}
		expectations := []*expectation{
			{host: process.startEvs(t), what: "ok", want: e2eStatusReady},
			{host: process.startGkm(t), what: "ok", want: e2eStatusReady},
			{host: process.startSockets(t), what: "ok", want: e2eStatusReady},
			{host: process.startTlsedge(t), what: "ok", want: e2eStatusReady},
			{host: process.startSmtpedge(t), what: "up", want: e2eStatusAnswers},
			{host: process.startMxedgeRouter(t), what: "ok", want: e2eStatusReady},
			{host: process.startMxedgePlatform(t), what: "ok", want: e2eStatusReady},
			{host: dnsApi, what: "ok", want: e2eStatusReady},
			{host: process.startDnsEdge(t, dnsApi), what: "up", want: e2eStatusAnswers},
			{host: process.startMcp(t), what: "ok", want: e2eStatusReady},
		}
		for _, expectation := range expectations {
			process.awaitStatus(t, expectation.host, expectation.what, expectation.want)
		}
		// each still answering once all of them run: one block's start took nothing another holds
		for _, expectation := range expectations {
			status, ok := process.status(expectation.host)
			if !ok || !expectation.want(status) {
				t.Fatalf("%s's /status is %q (answered %t) with every block running", expectation.host.name, status, ok)
			}
			t.Logf("%s's /status with every block running: %q", expectation.host.name, status)
		}
		// the edge before the API it feeds from, then the rest
		for i := len(expectations) - 1; 0 <= i; i -= 1 {
			process.stop(t, expectations[i].host)
		}
		process.awaitNoGoroutinesBeyond(t, before)
	})
}
