package server

// mxedge's end of the direct-MX probe route (mail_direct_mx_probe_gate.go; QD-1103, owner decision 639): the block's
// PlatformClientSource posts each outcome on MailDirectMxProbeGatePath with its edge bearer, the format's four fields
// and nothing more, and reads {recorded}; a request the format does not take is refused before any call; any answer
// but 200, an answer that does not read, a closed source and a call past its RequestTimeout are errors, which leave
// the outcome unrecorded, and none names the bearer. api is an httptest server and time a fake clock: nothing sleeps.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/sidecartest"
)

// The bearer an edge block's source sends in these tests.
const mailDirectMxProbeTestBearer = "edge-bearer-secret"

// api's routes as a test stands them up: the mint answers 503, so the source's own loops wait on the fake clock, and
// the probe route answers what the test sets, after recording each request it took.
type mailDirectMxProbeTestApi struct {
	t      testing.TB
	server *httptest.Server

	stateLock sync.Mutex
	// each probe request's headers and body, in order
	headers []http.Header
	bodies  [][]byte
	// what the probe route answers: a status and a body
	status int
	answer string
	// while set, the probe route waits for it to close before it answers, telling arrived first
	hold    chan struct{}
	arrived chan struct{}
}

func newMailDirectMxProbeTestApi(t testing.TB) *mailDirectMxProbeTestApi {
	self := &mailDirectMxProbeTestApi{t: t, status: http.StatusOK, answer: `{"recorded":true}`}
	self.server = httptest.NewServer(http.HandlerFunc(self.serve))
	t.Cleanup(self.server.Close)
	return self
}

func (self *mailDirectMxProbeTestApi) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != MailDirectMxProbeGatePath {
		http.Error(w, "Unavailable.", http.StatusServiceUnavailable)
		return
	}
	body, _ := io.ReadAll(r.Body)
	hold, arrived, status, answer := func() (chan struct{}, chan struct{}, int, string) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		header := r.Header.Clone()
		header.Set("X-Test-Method", r.Method)
		self.headers = append(self.headers, header)
		self.bodies = append(self.bodies, body)
		return self.hold, self.arrived, self.status, self.answer
	}()
	if hold != nil {
		close(arrived)
		<-hold
	}
	w.WriteHeader(status)
	io.WriteString(w, answer)
}

func (self *mailDirectMxProbeTestApi) set(status int, answer string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.status = status
	self.answer = answer
}

func (self *mailDirectMxProbeTestApi) requests() ([]http.Header, [][]byte) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.headers), slices.Clone(self.bodies)
}

// A source for mxedge's router on the test's api and clock, one client.
func (self *mailDirectMxProbeTestApi) newSource(ctx context.Context, clock *sidecartest.FakeClock) *PlatformClientSource {
	settings := DefaultPlatformClientSourceSettings()
	settings.ClientCount = 1
	source, err := NewPlatformClientSource(ctx, &PlatformClientSourceArgs{
		ApiUrl:   self.server.URL,
		Block:    "mxedge",
		Host:     "lb-a.example",
		Bearer:   mailDirectMxProbeTestBearer,
		Clock:    clock,
		Settings: settings,
	})
	if err != nil {
		self.t.Fatal(err)
	}
	self.t.Cleanup(source.Close)
	return source
}

// Each outcome goes to api's route on the edge bearer as the format's four fields, and the answer reads back as
// recorded or gone.
func TestMailDirectMxProbeGatePostsTheOutcomeOnTheEdgeBearer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	api := newMailDirectMxProbeTestApi(t)
	source := api.newSource(ctx, sidecartest.NewFakeClock(sidecartest.DefaultClockStart()))
	sent := []*MailDirectMxProbeGateArgs{
		{NetworkId: NewId(), ReceiverSidecarId: NewId(), Address: "192.0.2.25", Passed: true},
		{NetworkId: NewId(), ReceiverSidecarId: NewId(), Address: "2001:db8::25", Passed: false},
	}
	for _, args := range sent {
		result, err := source.RecordMailDirectMxProbe(ctx, args)
		if err != nil || result == nil || !result.Recorded {
			t.Fatalf("recorded %+v: %v", result, err)
		}
	}
	// a row gone in between: a success that recorded nothing
	api.set(http.StatusOK, `{"recorded":false}`)
	if result, err := source.RecordMailDirectMxProbe(ctx, sent[0]); err != nil || result.Recorded {
		t.Fatalf("a row gone answered %+v: %v", result, err)
	}
	headers, bodies := api.requests()
	if len(bodies) != 3 {
		t.Fatalf("api took %d requests", len(bodies))
	}
	for index, body := range bodies {
		header := headers[index]
		if header.Get("X-Test-Method") != http.MethodPost || header.Get("Authorization") != "Bearer "+mailDirectMxProbeTestBearer ||
			header.Get("Content-Type") != "application/json" {
			// the bearer itself is never printed
			t.Errorf("request %d: %s, %q, a bearer %t", index, header.Get("X-Test-Method"), header.Get("Content-Type"), header.Get("Authorization") != "")
		}
		// the format's four fields, and nothing more
		fields := map[string]json.RawMessage{}
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatal(err)
		}
		names := []string{}
		for name := range fields {
			names = append(names, name)
		}
		slices.Sort(names)
		if !slices.Equal(names, []string{"address", "network_id", "passed", "receiver_sidecar_id"}) {
			t.Errorf("request %d carries %v", index, names)
		}
		args := &MailDirectMxProbeGateArgs{}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(args); err != nil {
			t.Fatal(err)
		}
		if want := sent[index%2]; *args != *want {
			t.Errorf("request %d carried %+v, want %+v", index, args, want)
		}
	}
}

// A request the format does not take is refused before any call; any answer but a readable 200 is an error naming
// the status or the part, never the bearer; a closed source calls no more.
func TestMailDirectMxProbeGateRefusalsLeaveTheOutcomeUnrecorded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	api := newMailDirectMxProbeTestApi(t)
	source := api.newSource(ctx, sidecartest.NewFakeClock(sidecartest.DefaultClockStart()))
	valid := func() *MailDirectMxProbeGateArgs {
		return &MailDirectMxProbeGateArgs{NetworkId: NewId(), ReceiverSidecarId: NewId(), Address: "192.0.2.25", Passed: true}
	}
	invalid := map[string]*MailDirectMxProbeGateArgs{"no request": nil}
	for name, change := range map[string]func(*MailDirectMxProbeGateArgs){
		"no network":               func(args *MailDirectMxProbeGateArgs) { args.NetworkId = Id{} },
		"no receiver":              func(args *MailDirectMxProbeGateArgs) { args.ReceiverSidecarId = Id{} },
		"no address":               func(args *MailDirectMxProbeGateArgs) { args.Address = "" },
		"an address out of range":  func(args *MailDirectMxProbeGateArgs) { args.Address = "192.0.2.256" },
		"an address with a zone":   func(args *MailDirectMxProbeGateArgs) { args.Address = "fe80::1%eth0" },
		"IPv4 mapped into IPv6":    func(args *MailDirectMxProbeGateArgs) { args.Address = "::ffff:192.0.2.25" },
		"an address not canonical": func(args *MailDirectMxProbeGateArgs) { args.Address = "2001:DB8::25" },
		"a name for an address":    func(args *MailDirectMxProbeGateArgs) { args.Address = "mx.example" },
	} {
		args := valid()
		change(args)
		invalid[name] = args
	}
	for name, args := range invalid {
		if _, err := source.RecordMailDirectMxProbe(ctx, args); !errors.Is(err, ErrMailDirectMxProbeGateArgsInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, bodies := api.requests(); len(bodies) != 0 {
		t.Fatalf("a refused request reached api %d times", len(bodies))
	}

	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable, http.StatusInternalServerError} {
		api.set(status, "Refused.")
		_, err := source.RecordMailDirectMxProbe(ctx, valid())
		if !errors.Is(err, ErrMailDirectMxProbeGate) || !strings.Contains(err.Error(), strconv.Itoa(status)) {
			t.Errorf("api answered %d: %v", status, err)
		}
		if err != nil && strings.Contains(err.Error(), mailDirectMxProbeTestBearer) {
			t.Errorf("an error names the bearer: %v", err)
		}
	}
	for name, answer := range map[string]string{
		"not json":    "recorded",
		"past bounds": `{"recorded":true,"pad":"` + strings.Repeat("x", int(DefaultPlatformClientSourceSettings().AnswerMaxByteCount)) + `"}`,
	} {
		api.set(http.StatusOK, answer)
		if _, err := source.RecordMailDirectMxProbe(ctx, valid()); !errors.Is(err, ErrMailDirectMxProbeGate) {
			t.Errorf("an answer %s read: %v", name, err)
		}
	}

	api.set(http.StatusOK, `{"recorded":true}`)
	_, before := api.requests()
	source.Close()
	if _, err := source.RecordMailDirectMxProbe(ctx, valid()); !errors.Is(err, ErrMailDirectMxProbeGate) {
		t.Errorf("a closed source recorded: %v", err)
	}
	if _, after := api.requests(); len(after) != len(before) {
		t.Error("a closed source called api")
	}
	var none *PlatformClientSource
	if _, err := none.RecordMailDirectMxProbe(ctx, valid()); !errors.Is(err, ErrMailDirectMxProbeGate) {
		t.Errorf("no source recorded: %v", err)
	}
}

// A call api holds past the source's RequestTimeout on its clock ends as an error, so a stalled api never holds the
// prober's pass.
func TestMailDirectMxProbeGateCallEndsAtItsTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	api := newMailDirectMxProbeTestApi(t)
	hold := make(chan struct{})
	arrived := make(chan struct{})
	func() {
		api.stateLock.Lock()
		defer api.stateLock.Unlock()
		api.hold = hold
		api.arrived = arrived
	}()
	defer close(hold)
	source := api.newSource(ctx, clock)
	done := make(chan error, 1)
	go func() {
		_, err := source.RecordMailDirectMxProbe(ctx, &MailDirectMxProbeGateArgs{NetworkId: NewId(), ReceiverSidecarId: NewId(), Address: "192.0.2.25", Passed: true})
		done <- err
	}()
	select {
	case <-arrived:
	case <-ctx.Done():
		t.Fatal("the call never reached api")
	}
	// the call's bound was armed before it was sent
	clock.Advance(DefaultPlatformClientSourceSettings().RequestTimeout)
	select {
	case err := <-done:
		if !errors.Is(err, ErrMailDirectMxProbeGate) {
			t.Errorf("a call past its bound answered %v", err)
		}
	case <-ctx.Done():
		t.Fatal("a call past its bound did not end")
	}
}
