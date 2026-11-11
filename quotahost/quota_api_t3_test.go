//go:build sidecar_pending

package quotahost_test

// QUOTA-M2-11's T3 run over the wire: quotasuite.Run on NewHttpQuotaApi, the transport of the hosts with no
// Postgres, against api's own handlers (api/handlers' QuotaServiceHandlers, on the private stack's Postgres
// through httptest), so what those hosts reach is the body FakeQuota passes at T2 and NewLocalQuota at T3, and a
// divergence the wire brings fails here. It is this package's external test because the handlers import
// quotahost. Each service holds its own bearer, as each host would: the reads and the leases' calls go on
// mxedge's, which every route takes, and a batch on its producer's, so a batch naming a producer no host with
// no Postgres is refused as api refuses it.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest/quotasuite"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/api/handlers"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/quotahost"
	"github.com/urnetwork/server/router"
)

// Several hosts' transports as one source: the reads on one host's, each batch on its producer's.
type testHostsQuotaApi struct {
	quotahost.QuotaApiTransport
	producerTransports map[string]quotahost.QuotaApiTransport
}

func (self *testHostsQuotaApi) ApplyUsageBatch(ctx context.Context, batch *sidecar.UsageBatch, carves []*model.UsageCarve) error {
	transport, ok := self.producerTransports[batch.Producer]
	if !ok {
		// a producer that holds Postgres, or none: api refuses it on any host's bearer
		transport = self.producerTransports[sidecar.QuotaProducerEvs]
	}
	return transport.ApplyUsageBatch(ctx, batch, carves)
}

// api's handlers on a test server at clock, and each host's transport to it on a bearer of its own.
func newTestHostsQuotaApi(t *testing.T, clock sidecar.Clock) quotahost.QuotaSource {
	callers := []server.InternalGateCaller{server.InternalGateCallerEvs, server.InternalGateCallerTlsedge, server.InternalGateCallerMxedge}
	callerBearers := map[server.InternalGateCaller]string{}
	verifyFile := ""
	for _, caller := range callers {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		bearer := base64.RawURLEncoding.EncodeToString(raw)
		digest := sha256.Sum256([]byte(bearer))
		callerBearers[caller] = bearer
		verifyFile += string(caller) + ": {class: " + string(caller) + ", sha256: [" + hex.EncodeToString(digest[:]) + "]}\n"
	}
	verifier, err := server.NewInternalGateVerifier([]byte(verifyFile))
	if err != nil {
		t.Fatal(err)
	}
	quotaService := handlers.NewQuotaServiceHandlers(nil, &handlers.QuotaServiceDeps{Verifier: verifier, Now: clock.Now})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	httpServer := httptest.NewServer(router.NewRouter(ctx, quotaService.Routes()))
	t.Cleanup(httpServer.Close)
	transport := func(caller server.InternalGateCaller) quotahost.QuotaApiTransport {
		transport, err := quotahost.NewHttpQuotaApi(httpServer.URL, callerBearers[caller], httpServer.Client(), nil)
		if err != nil {
			t.Fatal(err)
		}
		return transport
	}
	return &testHostsQuotaApi{
		QuotaApiTransport: transport(server.InternalGateCallerMxedge),
		producerTransports: map[string]quotahost.QuotaApiTransport{
			sidecar.QuotaProducerEvs:     transport(server.InternalGateCallerEvs),
			sidecar.QuotaProducerTlsEdge: transport(server.InternalGateCallerTlsedge),
			sidecar.QuotaProducerMxEdge:  transport(server.InternalGateCallerMxedge),
		},
	}
}

// quotasuite on NewApiQuota's transport at T3: the body FakeQuota passes at T2, with api between the Quota and
// model.
func TestApiQuotaOverHttpConformsToTheSuite(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		defer model.Testing_SetEnforceSidecarLimits(true)()
		quotasuite.Run(t, quotahost.NewT3TargetOn(t.Context(), newTestHostsQuotaApi))
	})
}
