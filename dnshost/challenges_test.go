//go:build sidecar_pending

package dnshost

// The hourly cap on a name's challenge writes as certs meets it through the hosted API block (QD-1204):
// the model's refusal, this writer's mapping, the route's 429 with its Retry-After, certs' DNS client and
// its solver, on the private stack's Postgres.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mholt/acmez/v3/acme"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"
	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A name's eleventh challenge write in the hour, which certs' solver makes, is refused with a 429 whose
// Retry-After says when the name's oldest write leaves the window, rounded up to the whole second past it,
// and certs' client answers it as a refusal of its own naming the name and that wait, which the solver
// hands back with no retry and no wait on ready. The writer answers the route's error with the model's
// beside it.
func TestHostedDnsChallengeCapNamesItsRetryAfterToCerts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer model.Testing_SetDnsChallengesPerNameHour(10)()
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		fake := sidecartest.NewFakeHost(ctx, nil, nil, nil, nil)
		host := fake.ServiceHost()
		host.Clock = sidecar.RealClock{}
		if err := fake.Gates.Set("sidecar:\n  dns: {enabled: true, writes: true}\n"); err != nil {
			t.Fatal(err)
		}
		projectionApi := httptest.NewServer(http.NotFoundHandler())
		defer projectionApi.Close()
		directory := hostTestSecrets(t, map[string]string{
			HostVerifyFileName: fmt.Sprintf("%s: {class: %s, sha256: [%x]}\n", dnsservice.DnsCallerCerts, dnsservice.DnsCallerCerts, sha256.Sum256([]byte(dnsservice.DnsCallerCerts))),
			server.InternalGateCredentialFileName(server.InternalGateCallerDns): "current: " + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)) + "\n",
		})
		engine, err := NewApiHost(ctx, host, &HostSettings{Dns: hostTestSettings(t, nil), SecretDirectory: directory, ApiUrl: projectionApi.URL}, &HostDeps{SoaReader: hostTestServedSoas{}})
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
		clientSettings := server.DefaultCertsDnsClientSettings()
		clientSettings.BaseUrl, clientSettings.Root = httpApi.URL, hostTestRoot
		client, err := server.NewCertsDnsClient(clientSettings, string(dnsservice.DnsCallerCerts), nil)
		if err != nil {
			t.Fatal(err)
		}
		network := server.NewId()
		model.Testing_CreateNetwork(ctx, network, "dns-cap-"+network.String()[24:], server.NewId())
		label, err := model.EnsureNetworkLabel(ctx, network, hostTestRoot)
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimSuffix(label+"."+hostTestRoot, ".")

		beforeFirst := time.Now()
		var afterFirst time.Time
		for index := range 10 {
			if _, err := client.WriteAcmeChallenge(ctx, name, []string{fmt.Sprintf("digest-%d", index)}); err != nil {
				t.Fatalf("write %d: %v", index+1, err)
			}
			if index == 0 {
				afterFirst = time.Now()
			}
		}

		solver := certsservice.NewCertsDnsSolver(client, certsservice.DefaultCertsSettings(), nil)
		challenge := acme.Challenge{Type: acme.ChallengeTypeDNS01, Token: "capped", KeyAuthorization: "capped.thumbprint", Identifier: acme.Identifier{Type: "dns", Value: name}}
		if err := solver.Present(ctx, challenge); err != nil {
			t.Fatal(err)
		}
		beforeRefusal := time.Now()
		err = solver.Wait(ctx, challenge)
		afterRefusal := time.Now()
		var rateLimit *certsservice.CertsDnsWriteRateLimitError
		if !errors.As(err, &rateLimit) || !errors.Is(err, certsservice.ErrCertsDnsWriteRateLimited) || !errors.Is(err, certsservice.ErrCertsDnsWriteRefused) {
			t.Fatalf("the eleventh write answered %v", err)
		}
		// whole seconds past the oldest write's leaving, as the model read the clock between the bounds
		window := model.DefaultDnsChallengeSettings().RateWindow
		low := (beforeFirst.Add(window).Sub(afterRefusal)/time.Second + 1) * time.Second
		high := (afterFirst.Add(window).Sub(beforeRefusal)/time.Second + 1) * time.Second
		if rateLimit.Name != name || rateLimit.RetryAfter%time.Second != 0 || rateLimit.RetryAfter < low || high < rateLimit.RetryAfter {
			t.Fatalf("the refusal named %q, Retry-After %v, not whole seconds in [%v, %v]", rateLimit.Name, rateLimit.RetryAfter, low, high)
		}
		if err := solver.CleanUp(ctx, challenge); err != nil {
			t.Fatal(err)
		}

		// the writer itself: the route's error, carrying the model's wait and the model's refusal
		_, err = NewChallenges().PutChallenge(ctx, hostTestRoot, name, []string{"again"}, 0)
		var routeRateErr *dnsservice.DnsChallengeRateError
		var modelRateErr *model.DnsChallengeRateError
		if !errors.As(err, &routeRateErr) || !errors.As(err, &modelRateErr) || !errors.Is(err, dnsservice.ErrDnsChallengeRate) || !errors.Is(err, model.ErrDnsChallengeRate) ||
			routeRateErr.RetryAfter != modelRateErr.RetryAfter || routeRateErr.RetryAfter <= 0 {
			t.Fatalf("the writer answered %v", err)
		}
	})
}
