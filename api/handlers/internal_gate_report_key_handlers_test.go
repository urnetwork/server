package handlers

// api's report-key route (plan-SMTP-report-seal-key, QD-075; §14.8): only the platform listener's bearer reaches it, and
// no edge block's, router's or other caller's does, each refused before the source is asked; the source's key, none
// and unavailable reach the listener's own client, smtpedgeservice.SmtpReportKeyGateSource, as a key, none and an
// error, the last a 503 with Retry-After; a network id that is none is refused 400; and before api's projection
// installs a source every read is unavailable.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

// The route on a verify file of the platform listener's, the edge class's and mxedge's router's bearers, with a
// source that records each network asked and answers what the test sets.
type reportKeyTestWorld struct {
	t        *testing.T
	ctx      context.Context
	server   *httptest.Server
	platform string
	edge     string
	mxedge   string
	keys     *sidecartest.SmtpReportKeyFixture
	// generation 1's key, as api's checks answer it
	key *smtpedgeservice.SmtpReportSealKey

	stateLock sync.Mutex
	asked     []sidecar.Id
	answer    *smtpedgeservice.SmtpReportSealKey
	err       error
}

func newReportKeyTestWorld(t *testing.T, deps *InternalGateReportKeyDeps) *reportKeyTestWorld {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	platform, platformDigest := newInternalGateTestBearer(t)
	edge, edgeDigest := newInternalGateTestBearer(t)
	mxedge, mxedgeDigest := newInternalGateTestBearer(t)
	verifier, err := server.NewInternalGateVerifier([]byte(
		"mxedge-platform: {class: mxedge-platform, sha256: [" + platformDigest + "]}\n" +
			"edge: {class: edge, sha256: [" + edgeDigest + "]}\n" +
			"mxedge: {class: mxedge, sha256: [" + mxedgeDigest + "]}\n",
	))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := sidecartest.NewSmtpReportKeyFixture(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := keys.DropKeyReply(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	key, err := smtpedgeservice.CheckSmtpReportSealKey(keys.View(), keys.RepoId, reply, keys.Clock.Now(), sidecar.DefaultGkmSettings().DropKeyLifetime)
	if err != nil {
		t.Fatal(err)
	}
	self := &reportKeyTestWorld{t: t, ctx: ctx, platform: platform, edge: edge, mxedge: mxedge, keys: keys, key: key}
	if deps == nil {
		deps = &InternalGateReportKeyDeps{
			Key: func(ctx context.Context, networkId sidecar.Id) (*smtpedgeservice.SmtpReportSealKey, error) {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.asked = append(self.asked, networkId)
				return self.answer, self.err
			},
		}
	}
	deps.Verifier = verifier
	self.server = httptest.NewServer(router.NewRouter(ctx, NewInternalGateReportKeyHandlers(deps).Routes()))
	t.Cleanup(self.server.Close)
	return self
}

// What the source answers from now on.
func (self *reportKeyTestWorld) set(answer *smtpedgeservice.SmtpReportSealKey, err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.answer, self.err = answer, err
}

// Every network the source was asked for.
func (self *reportKeyTestWorld) askedNetworks() []sidecar.Id {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]sidecar.Id{}, self.asked...)
}

// One GET of the route for path's network id, on authorization.
func (self *reportKeyTestWorld) get(networkIdText string, authorization string) *http.Response {
	self.t.Helper()
	request, err := http.NewRequestWithContext(self.ctx, http.MethodGet, self.server.URL+smtpedgeservice.SmtpReportKeyGatePath+networkIdText, nil)
	if err != nil {
		self.t.Fatal(err)
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		self.t.Fatal(err)
	}
	response.Body.Close()
	return response
}

// The listener's own client on bearer.
func (self *reportKeyTestWorld) source(bearer string) *smtpedgeservice.SmtpReportKeyGateSource {
	self.t.Helper()
	source, err := smtpedgeservice.NewSmtpReportKeyGateSource(&smtpedgeservice.SmtpReportKeyGateSourceArgs{ApiUrl: self.server.URL, Bearer: bearer, Clock: self.keys.Clock})
	if err != nil {
		self.t.Fatal(err)
	}
	return source
}

func TestInternalGateReportKeyTakesThePlatformListenersBearerAlone(t *testing.T) {
	world := newReportKeyTestWorld(t, nil)
	world.set(world.key, nil)
	networkId := sidecar.NewId()
	for _, authorization := range []string{"", "Bearer ", "Bearer not-a-bearer", "Bearer " + world.edge, "Bearer " + world.mxedge, world.platform} {
		if response := world.get(networkId.String(), authorization); response.StatusCode != http.StatusForbidden {
			t.Errorf("%q: %d", authorization, response.StatusCode)
		}
	}
	if len(world.askedNetworks()) != 0 {
		t.Fatal("the source was asked for a refused request")
	}
	if response := world.get(networkId.String(), "Bearer "+world.platform); response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("the platform listener's bearer: %d", response.StatusCode)
	}
	if asked := world.askedNetworks(); len(asked) != 1 || asked[0] != networkId {
		t.Fatalf("the source was asked for %v", asked)
	}
	// another class's bearer through the listener's own client is a key not told
	if _, err := world.source(world.edge).ReportSealKey(world.ctx, networkId); !errors.Is(err, smtpedgeservice.ErrSmtpReportKeyUnavailable) {
		t.Fatalf("the edge bearer through the client: %v", err)
	}
}

func TestInternalGateReportKeyAnswersAKeyNoneOrUnavailableToTheListener(t *testing.T) {
	world := newReportKeyTestWorld(t, nil)
	source := world.source(world.platform)
	networkId := sidecar.NewId()

	world.set(world.key, nil)
	publicKey, err := source.ReportSealKey(world.ctx, networkId)
	if err != nil || !bytes.Equal(publicKey, world.key.PublicKey) {
		t.Fatalf("a key: %v", err)
	}
	world.set(nil, nil)
	if publicKey, err := source.ReportSealKey(world.ctx, networkId); err != nil || publicKey != nil {
		t.Fatalf("none: %v", err)
	}
	world.set(nil, fmt.Errorf("%w: gkm did not answer", smtpedgeservice.ErrSmtpReportKeyUnavailable))
	if publicKey, err := source.ReportSealKey(world.ctx, networkId); !errors.Is(err, smtpedgeservice.ErrSmtpReportKeyUnavailable) || publicKey != nil {
		t.Fatalf("unavailable: %v", err)
	}
	response := world.get(networkId.String(), "Bearer "+world.platform)
	if response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Retry-After") != "60" {
		t.Fatalf("unavailable: %d, Retry-After %q", response.StatusCode, response.Header.Get("Retry-After"))
	}
	// an error of no known kind is a key not told too
	world.set(nil, errors.New("a source that failed"))
	if response := world.get(networkId.String(), "Bearer "+world.platform); response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("an unknown error: %d", response.StatusCode)
	}
	for _, text := range []string{"not-an-id", server.Id{}.String()} {
		if response := world.get(text, "Bearer "+world.platform); response.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d", text, response.StatusCode)
		}
	}
	for _, asked := range world.askedNetworks() {
		if asked != networkId {
			t.Fatalf("the source was asked for %s", asked)
		}
	}
}

func TestInternalGateReportKeyIsUnavailableBeforeApisSourceIsInstalled(t *testing.T) {
	world := newReportKeyTestWorld(t, &InternalGateReportKeyDeps{})
	response := world.get(sidecar.NewId().String(), "Bearer "+world.platform)
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("with no source installed: %d", response.StatusCode)
	}
}
