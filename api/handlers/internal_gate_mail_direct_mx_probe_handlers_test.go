package handlers

// api's direct-MX probe route (internal_gate_mail_direct_mx_probe_handlers.go; QD-1103, owner decision 639): only the
// edge class's bearer reaches it, every other class's, an unknown one and none answered 403 with nothing recorded;
// with the mail routes dark it answers 503 before reading the body; a body its format does not take is refused 400
// with nothing recorded; and what the record found is the answer: recorded or gone 200, an unknown network 404, a
// zone's refusal 409, a process that cannot record 503 with Retry-After. The record is a test's, so no store is
// read: model's tests and the composed test through the route run model.RecordMailDirectMxProbe itself.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// The route served on a verify file holding a bearer of every class, its record a test's.
type mailDirectMxProbeRouteTestWorld struct {
	t       *testing.T
	server  *httptest.Server
	bearers map[server.InternalGateCaller]string

	stateLock sync.Mutex
	// every probe the record was asked, in order
	probes []*model.MailDirectMxProbe
	// what the record answers, and whether the mail routes are open
	recorded bool
	err      error
	open     bool
}

func newMailDirectMxProbeRouteTestWorld(t *testing.T) *mailDirectMxProbeRouteTestWorld {
	self := &mailDirectMxProbeRouteTestWorld{t: t, bearers: map[server.InternalGateCaller]string{}, recorded: true, open: true}
	verifyFile := ""
	for _, caller := range server.InternalGateCallers() {
		bearer, digest := newInternalGateTestBearer(t)
		self.bearers[caller] = bearer
		verifyFile += string(caller) + ": {class: " + string(caller) + ", sha256: [" + digest + "]}\n"
	}
	verifier, err := server.NewInternalGateVerifier([]byte(verifyFile))
	if err != nil {
		t.Fatal(err)
	}
	handlers := NewInternalGateEdgeClientHandlers(&InternalGateEdgeClientDeps{
		Verifier: verifier,
		RecordMailDirectMxProbe: func(ctx context.Context, probe *model.MailDirectMxProbe) (bool, error) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.probes = append(self.probes, probe)
			return self.recorded, self.err
		},
		MailOpen: func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			return self.open
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	self.server = httptest.NewServer(router.NewRouter(ctx, handlers.Routes()))
	t.Cleanup(self.server.Close)
	return self
}

// Sets what the record answers.
func (self *mailDirectMxProbeRouteTestWorld) answer(recorded bool, err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.recorded = recorded
	self.err = err
}

func (self *mailDirectMxProbeRouteTestWorld) setOpen(open bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.open = open
}

// The probes the record was asked, and forgets them.
func (self *mailDirectMxProbeRouteTestWorld) takeProbes() []*model.MailDirectMxProbe {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	probes := self.probes
	self.probes = nil
	return probes
}

// One call with a bearer and a body: its status, its answer when 200, and its Retry-After.
func (self *mailDirectMxProbeRouteTestWorld) post(bearer string, body string) (int, *server.MailDirectMxProbeGateResult, string) {
	self.t.Helper()
	request, err := http.NewRequest(http.MethodPost, self.server.URL+server.MailDirectMxProbeGatePath, bytes.NewReader([]byte(body)))
	if err != nil {
		self.t.Fatal(err)
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := self.server.Client().Do(request)
	if err != nil {
		self.t.Fatal(err)
	}
	defer response.Body.Close()
	result := &server.MailDirectMxProbeGateResult{}
	if response.StatusCode == http.StatusOK {
		if err := json.NewDecoder(response.Body).Decode(result); err != nil {
			self.t.Fatal(err)
		}
	}
	return response.StatusCode, result, response.Header.Get("Retry-After")
}

// A probe's body as the prober sends it.
func mailDirectMxProbeRouteTestBody(t *testing.T, args *server.MailDirectMxProbeGateArgs) string {
	t.Helper()
	body, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestInternalGateMailDirectMxProbeTakesOnlyTheEdgeClass(t *testing.T) {
	world := newMailDirectMxProbeRouteTestWorld(t)
	args := &server.MailDirectMxProbeGateArgs{NetworkId: server.NewId(), ReceiverSidecarId: server.NewId(), Address: "192.0.2.25", Passed: true}
	body := mailDirectMxProbeRouteTestBody(t, args)
	unknown, _ := newInternalGateTestBearer(t)
	for name, bearer := range map[string]string{"no bearer": "", "an unknown bearer": unknown} {
		if status, _, _ := world.post(bearer, body); status != http.StatusForbidden {
			t.Errorf("%s answered %d", name, status)
		}
	}
	for caller, bearer := range world.bearers {
		if caller == server.InternalGateCallerEdge {
			continue
		}
		if status, _, _ := world.post(bearer, body); status != http.StatusForbidden {
			t.Errorf("%s's bearer answered %d", caller, status)
		}
	}
	if probes := world.takeProbes(); len(probes) != 0 {
		t.Fatalf("a refused caller reached the record %d times", len(probes))
	}

	status, result, _ := world.post(world.bearers[server.InternalGateCallerEdge], body)
	if status != http.StatusOK || !result.Recorded {
		t.Fatalf("the edge bearer answered %d, %+v", status, result)
	}
	probes := world.takeProbes()
	if len(probes) != 1 || probes[0].NetworkId != args.NetworkId || probes[0].ReceiverSidecarId != args.ReceiverSidecarId ||
		probes[0].Address != netip.MustParseAddr("192.0.2.25") || !probes[0].Passed {
		t.Fatalf("the record was asked %+v", probes)
	}
	// a failure is recorded as one
	args.Passed = false
	args.Address = "2001:db8::25"
	if status, _, _ := world.post(world.bearers[server.InternalGateCallerEdge], mailDirectMxProbeRouteTestBody(t, args)); status != http.StatusOK {
		t.Fatalf("a failing probe answered %d", status)
	}
	if probes := world.takeProbes(); len(probes) != 1 || probes[0].Passed || probes[0].Address != netip.MustParseAddr("2001:db8::25") {
		t.Fatalf("the record was asked %+v", probes)
	}
}

func TestInternalGateMailDirectMxProbeIsDarkWithTheMailRoutes(t *testing.T) {
	world := newMailDirectMxProbeRouteTestWorld(t)
	world.setOpen(false)
	body := mailDirectMxProbeRouteTestBody(t, &server.MailDirectMxProbeGateArgs{NetworkId: server.NewId(), ReceiverSidecarId: server.NewId(), Address: "192.0.2.25", Passed: true})
	if status, _, retryAfter := world.post(world.bearers[server.InternalGateCallerEdge], body); status != http.StatusServiceUnavailable || retryAfter != "60" {
		t.Errorf("dark, the edge bearer answered %d, Retry-After %q", status, retryAfter)
	}
	// the bearer is still checked first
	if status, _, _ := world.post(world.bearers[server.InternalGateCallerMxedge], body); status != http.StatusForbidden {
		t.Errorf("dark, mxedge's bearer answered %d", status)
	}
	if probes := world.takeProbes(); len(probes) != 0 {
		t.Fatalf("a dark route reached the record %d times", len(probes))
	}
}

func TestInternalGateMailDirectMxProbeRefusesWhatItsFormatDoesNotTake(t *testing.T) {
	world := newMailDirectMxProbeRouteTestWorld(t)
	edge := world.bearers[server.InternalGateCallerEdge]
	networkId, receiverId := server.NewId().String(), server.NewId().String()
	valid := `{"network_id":"` + networkId + `","receiver_sidecar_id":"` + receiverId + `","address":"192.0.2.25","passed":true}`
	if status, _, _ := world.post(edge, valid); status != http.StatusOK {
		t.Fatalf("a valid body answered %d", status)
	}
	world.takeProbes()
	for name, body := range map[string]string{
		"not json":                 "passed",
		"a field beyond":           strings.TrimSuffix(valid, "}") + `,"root":"root.example"}`,
		"a second value":           valid + valid,
		"no network":               `{"receiver_sidecar_id":"` + receiverId + `","address":"192.0.2.25","passed":true}`,
		"no receiver":              `{"network_id":"` + networkId + `","address":"192.0.2.25","passed":true}`,
		"no address":               `{"network_id":"` + networkId + `","receiver_sidecar_id":"` + receiverId + `","passed":true}`,
		"an address with a zone":   strings.Replace(valid, "192.0.2.25", "fe80::1%eth0", 1),
		"IPv4 mapped into IPv6":    strings.Replace(valid, "192.0.2.25", "::ffff:192.0.2.25", 1),
		"an address not canonical": strings.Replace(valid, "192.0.2.25", "2001:DB8::25", 1),
		"a name for an address":    strings.Replace(valid, "192.0.2.25", "mx.example", 1),
		"a body past its bound":    strings.TrimSuffix(valid, "}") + `,"pad":"` + strings.Repeat("x", int(DefaultInternalGateMailDirectMxProbeSettings().RequestMaxByteCount)) + `"}`,
	} {
		if status, _, _ := world.post(edge, body); status != http.StatusBadRequest {
			t.Errorf("%s answered %d", name, status)
		}
	}
	if probes := world.takeProbes(); len(probes) != 0 {
		t.Fatalf("a refused body reached the record %d times", len(probes))
	}
}

func TestInternalGateMailDirectMxProbeAnswersWhatTheRecordFound(t *testing.T) {
	world := newMailDirectMxProbeRouteTestWorld(t)
	edge := world.bearers[server.InternalGateCallerEdge]
	body := mailDirectMxProbeRouteTestBody(t, &server.MailDirectMxProbeGateArgs{NetworkId: server.NewId(), ReceiverSidecarId: server.NewId(), Address: "192.0.2.25", Passed: true})
	// a row gone in between: a success that recorded nothing
	world.answer(false, nil)
	if status, result, _ := world.post(edge, body); status != http.StatusOK || result.Recorded {
		t.Errorf("a row gone answered %d, %+v", status, result)
	}
	notTeam := model.NewQuotaRefusalError(&sidecar.QuotaRefusal{LimitKey: model.TeamTierLimitKey, Upgrade: true})
	for _, c := range []struct {
		name   string
		err    error
		status int
	}{
		{name: "an unknown network", err: model.ErrMailDirectMxProbeNetworkUnknown, status: http.StatusNotFound},
		{name: "no longer a team", err: notTeam, status: http.StatusConflict},
		{name: "mail off at the zone", err: model.ErrDnsDirectMxMailOff, status: http.StatusConflict},
		{name: "mail off", err: model.ErrMailDirectMxMailOff, status: http.StatusConflict},
		{name: "an address the A rules refuse", err: dnsservice.ErrDnsDirectMxRefused, status: http.StatusConflict},
		{name: "a suspended zone", err: model.ErrDnsZoneSuspended, status: http.StatusConflict},
		{name: "a binary without MG7", err: model.ErrMailUnlinked, status: http.StatusServiceUnavailable},
		{name: "a binary without the zone's groups", err: model.ErrDnsUnlinked, status: http.StatusServiceUnavailable},
		{name: "the dns restoring", err: dnsservice.ErrDnsRestoring, status: http.StatusServiceUnavailable},
		{name: "a record that failed", err: errors.New("the store did not answer"), status: http.StatusServiceUnavailable},
	} {
		world.answer(false, c.err)
		status, _, retryAfter := world.post(edge, body)
		if status != c.status {
			t.Errorf("%s answered %d, want %d", c.name, status, c.status)
		}
		if (status == http.StatusServiceUnavailable) != (retryAfter == "60") {
			t.Errorf("%s answered Retry-After %q", c.name, retryAfter)
		}
	}
}
