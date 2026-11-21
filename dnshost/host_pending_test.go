//go:build sidecar_pending

package dnshost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/mholt/acmez/v3/acme"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"
	"testing"

	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecarhost"
)

// On §12.14's tables the API role's store reads every root's row, so the block is ready, and its start
// wrote <root>'s own records (DNS-M5-11).
func TestApiHostReadyOnItsTables(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		server.Testing_EnableKeyspaceNotifications(ctx)
		fakeHost := sidecartest.NewFakeHost(ctx, nil, nil, nil, nil)
		host := fakeHost.ServiceHost()
		if err := fakeHost.Gates.Set("sidecar:\n  dns: {enabled: true}\n"); err != nil {
			t.Fatal(err)
		}
		store, err := NewStore(sidecarhost.NewRedis(nil))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		settings := hostTestSettings(t, nil)
		settings.RootRecords = map[string]*dnsservice.DnsRootRecordSettings{hostTestRoot: {ReportMx: "rpt.report.example"}}
		engine, err := NewApiHost(ctx, host, &HostSettings{Dns: settings, SecretDirectory: t.TempDir()}, &HostDeps{Store: store, RecordGate: dnsservice.DnsUnloadedRecordGate{}, KeyRecordGate: dnsservice.NewDnsAclKeyRecordGate(nil)})
		if err != nil {
			t.Fatal(err)
		}
		api := engine.(*apiHost)
		defer api.Close()
		for !api.Ready() {
			_, changed := api.Probes()
			if api.Ready() {
				break
			}
			select {
			case <-changed:
			case <-t.Context().Done():
				t.Fatal("never ready")
			}
		}
		state, err := store.RootState(ctx, hostTestRoot)
		if err != nil || state.Serial == 0 {
			t.Fatalf("<root>'s records were not written at start: %+v %v", state, err)
		}
	})
}

// Local authorities report the serial served after each fixture publication.
type hostTestServedSoas struct{}

func (hostTestServedSoas) ReadSoa(ctx context.Context, root, ns string) (uint32, error) {
	state, err := model.GetDnsRootState(ctx, root)
	if err != nil {
		return 0, err
	}
	return uint32(state.Serial), nil
}

func TestHostedDnsCanaryDrivesDns01IssuanceAndRenewal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		fake := sidecartest.NewFakeHost(ctx, nil, nil, nil, nil)
		host := fake.ServiceHost()
		host.Clock = sidecar.RealClock{}
		if err := fake.Gates.Set("sidecar:\n  dns: {enabled: true, writes: true}\n"); err != nil {
			t.Fatal(err)
		}
		settings := hostTestSettings(t, nil)
		projectionApi := httptest.NewServer(http.NotFoundHandler())
		defer projectionApi.Close()
		verify := ""
		for _, class := range []dnsservice.DnsCallerClass{dnsservice.DnsCallerCerts, dnsservice.DnsCallerMonitor, dnsservice.DnsCallerMonitorOutside} {
			verify += fmt.Sprintf("%s: {class: %s, sha256: [%x]}\n", class, class, sha256.Sum256([]byte(class)))
		}
		directory := hostTestSecrets(t, map[string]string{
			HostVerifyFileName: verify,
			server.InternalGateCredentialFileName(server.InternalGateCallerDns): "current: " + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)) + "\n",
		})
		engine, err := NewApiHost(ctx, host, &HostSettings{Dns: settings, SecretDirectory: directory, ApiUrl: projectionApi.URL}, &HostDeps{SoaReader: hostTestServedSoas{}})
		if err != nil {
			t.Fatal(err)
		}
		defer engine.Close()
		api := engine.(*apiHost)
		for !api.Ready() {
			_, changed := api.Probes()
			if api.Ready() {
				break
			}
			select {
			case <-changed:
			case <-ctx.Done():
				t.Fatal("DNS API never reached readiness")
			}
		}
		httpApi := httptest.NewServer(api.Handler())
		defer httpApi.Close()
		certSettings := server.DefaultCertsDnsClientSettings()
		certSettings.BaseUrl, certSettings.Root = httpApi.URL, hostTestRoot
		client, err := server.NewCertsDnsClient(certSettings, string(dnsservice.DnsCallerCerts), nil)
		if err != nil {
			t.Fatal(err)
		}
		network := server.NewId()
		model.Testing_CreateNetwork(ctx, network, "dns-renewal-"+network.String()[24:], server.NewId())
		label, err := model.EnsureNetworkLabel(ctx, network, hostTestRoot)
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimSuffix(label+"."+hostTestRoot, ".")
		if _, err := client.WriteAcmeChallenge(ctx, name, []string{"initial"}); err != nil {
			t.Fatal(err)
		}
		report := func(class dnsservice.DnsCallerClass, source string, ok bool, serial uint64) {
			t.Helper()
			body, err := json.Marshal(dnsservice.DnsCanaryReport{Root: hostTestRoot, Source: source, Ok: ok, Serial: serial})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, dnsservice.DnsCanaryReportPath, bytes.NewReader(body)).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer "+string(class))
			reply := httptest.NewRecorder()
			api.Handler().ServeHTTP(reply, req)
			if reply.Code != http.StatusNoContent {
				t.Fatalf("%s report: %d", source, reply.Code)
			}
		}
		state, err := model.GetDnsRootState(ctx, hostTestRoot)
		if err != nil {
			t.Fatal(err)
		}
		serial := state.Serial
		checkClean := func(want bool) {
			t.Helper()
			clean, err := client.CanaryClean(ctx, serial)
			if clean != want || err != nil {
				t.Fatalf("canary clean = %t, want %t: %v", clean, want, err)
			}
		}
		checkClean(false)
		report(dnsservice.DnsCallerMonitor, dnsservice.DnsCanarySourceSite, true, serial)
		checkClean(true)
		report(dnsservice.DnsCallerMonitorOutside, dnsservice.DnsCanarySourceOutside, false, serial)
		checkClean(false)
		report(dnsservice.DnsCallerMonitorOutside, dnsservice.DnsCanarySourceOutside, true, serial)
		checkClean(true)
		records, err := NewCanaries().CanaryVerdicts(ctx, hostTestRoot)
		if err != nil || len(records) != 2 {
			t.Fatalf("persisted verdicts: %+v %v", records, err)
		}
		for _, token := range []string{"issuance", "renewal"} {
			solver := certsservice.NewCertsDnsSolver(client, certsservice.DefaultCertsSettings(), nil)
			challenge := acme.Challenge{Type: acme.ChallengeTypeDNS01, Token: token, KeyAuthorization: token + ".thumbprint", Identifier: acme.Identifier{Type: "dns", Value: name}}
			if err := solver.Present(ctx, challenge); err != nil {
				t.Fatal(err)
			}
			if err := solver.Wait(ctx, challenge); err != nil {
				t.Fatal(err)
			}
			if err := solver.CleanUp(ctx, challenge); err != nil {
				t.Fatal(err)
			}
		}
	})
}
