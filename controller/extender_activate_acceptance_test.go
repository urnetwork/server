package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The acceptance rule of an activation (connect/EXTENDER.md C2a), judged over
// fake probes. The required test -- the tcp carrier, the forward through it
// and the family the forward arrived on -- alone decides whether an extender
// is accepted. An optional carrier (quic; dns per port) that does not answer
// is left out and listed, and never refuses an activation the required test
// passed (connect#230). Nothing here dials or reads the database, so these
// run anywhere; extender_controller_test.go runs the same rule end to end
// against the real extender.

const (
	testAcceptanceTcpPort = 8443
	testAcceptanceUdpPort = 8444
)

// 53 and the whodis port, which is what the sn miner offers (L2), ascending
// as an activation passes them on.
var testAcceptanceDnsPorts = []int{53, 4053}

// Fake probes that answer from a table, and log each probe in the order it
// was made as "<carrier>:<port>" or "forward".
type testActivationProbes struct {
	// the error of each failing carrier probe, by name; a probe not listed
	// answers
	carrierErrs map[string]error
	// answers each carrier probe in place of carrierErrs when set
	answer func(ctx context.Context, probe string) error
	// the caller address the forward reports
	forwardClientAddress string
	forwardErr           error
	// answers the forward in place of forwardErr when set
	forwardAnswer func(ctx context.Context) error

	// the clock the activation is judged on, which only the fakes move
	clock *testActivationClock

	probes []string
}

func newTestActivationProbes() *testActivationProbes {
	return &testActivationProbes{
		carrierErrs:          map[string]error{},
		forwardClientAddress: "192.0.2.10:443",
		clock:                newTestActivationClock(),
	}
}

func testActivationProbeName(connectMode connect.ExtenderConnectMode, port int) string {
	carrier := string(connectMode)
	switch connectMode {
	case connect.ExtenderConnectModeTcpTls:
		carrier = connect.ExtenderCarrierTcp
	case connect.ExtenderConnectModeQuic:
		carrier = connect.ExtenderCarrierQuic
	case connect.ExtenderConnectModeDns:
		carrier = connect.ExtenderCarrierDns
	}
	return fmt.Sprintf("%s:%d", carrier, port)
}

func (self *testActivationProbes) activationProbes() *extenderActivationProbes {
	return &extenderActivationProbes{
		carrier: func(ctx context.Context, connectMode connect.ExtenderConnectMode, port int) error {
			probe := testActivationProbeName(connectMode, port)
			self.probes = append(self.probes, probe)
			if self.answer != nil {
				return self.answer(ctx, probe)
			}
			return self.carrierErrs[probe]
		},
		forward: func(ctx context.Context) (string, error) {
			self.probes = append(self.probes, "forward")
			if self.forwardAnswer != nil {
				if err := self.forwardAnswer(ctx); err != nil {
					return "", err
				}
			} else if self.forwardErr != nil {
				return "", self.forwardErr
			}
			return self.forwardClientAddress, nil
		},
	}
}

// Judges an ipv4 activation that offers the given carriers, in that order,
// with both dns ports.
func (self *testActivationProbes) judge(ctx context.Context, carriers ...string) *extenderActivationOutcome {
	return probeExtenderActivation(
		ctx,
		self.clock.activationClock(),
		self.activationProbes(),
		carriers,
		testAcceptanceTcpPort,
		testAcceptanceUdpPort,
		testAcceptanceDnsPorts,
		4,
	)
}

// The refused carriers of an outcome as probe names. Each must say why.
func testRefusedCarrierNames(t testing.TB, outcome *extenderActivationOutcome) []string {
	t.Helper()
	names := []string{}
	for _, refusedCarrier := range outcome.refusedCarriers {
		if refusedCarrier.Error == "" {
			t.Fatalf("%s:%d was left out with no reason", refusedCarrier.Carrier, refusedCarrier.Port)
		}
		names = append(names, fmt.Sprintf("%s:%d", refusedCarrier.Carrier, refusedCarrier.Port))
	}
	return names
}

var testOptionalCarrierProbes = struct {
	quic    string
	dns53   string
	dns4053 string
}{
	quic:    fmt.Sprintf("%s:%d", connect.ExtenderCarrierQuic, testAcceptanceUdpPort),
	dns53:   fmt.Sprintf("%s:%d", connect.ExtenderCarrierDns, 53),
	dns4053: fmt.Sprintf("%s:%d", connect.ExtenderCarrierDns, 4053),
}

// An optional carrier that does not answer costs that carrier and nothing
// else: the activation passes on the required test, the carriers that
// answered are what is stored and signed, in the order offered, and the
// outcome lists each one left out with its port and its error. A dns port
// that does not answer beside one that does keeps the dns carrier on the port
// that answered.
func TestExtenderActivationLeavesOutOptionalCarriersThatDoNotAnswer(t *testing.T) {
	tcp := connect.ExtenderCarrierTcp
	quic := connect.ExtenderCarrierQuic
	dns := connect.ExtenderCarrierDns
	probeNames := testOptionalCarrierProbes

	cases := []struct {
		name         string
		failing      []string
		wantCarriers []string
		wantDnsPorts []int
		wantRefused  []string
	}{
		{
			name:         "every carrier answers",
			wantCarriers: []string{tcp, quic, dns},
			wantDnsPorts: []int{53, 4053},
			wantRefused:  []string{},
		},
		{
			name:         "quic does not answer",
			failing:      []string{probeNames.quic},
			wantCarriers: []string{tcp, dns},
			wantDnsPorts: []int{53, 4053},
			wantRefused:  []string{probeNames.quic},
		},
		{
			name:         "one dns port does not answer",
			failing:      []string{probeNames.dns53},
			wantCarriers: []string{tcp, quic, dns},
			wantDnsPorts: []int{4053},
			wantRefused:  []string{probeNames.dns53},
		},
		{
			name:         "no dns port answers",
			failing:      []string{probeNames.dns53, probeNames.dns4053},
			wantCarriers: []string{tcp, quic},
			wantDnsPorts: []int{},
			wantRefused:  []string{probeNames.dns53, probeNames.dns4053},
		},
		{
			name:         "no optional carrier answers",
			failing:      []string{probeNames.dns53, probeNames.quic, probeNames.dns4053},
			wantCarriers: []string{tcp},
			wantDnsPorts: []int{},
			// in the order offered, quic before dns, whatever order they
			// were probed in
			wantRefused: []string{probeNames.quic, probeNames.dns53, probeNames.dns4053},
		},
	}
	for _, c := range cases {
		probes := newTestActivationProbes()
		for _, probe := range c.failing {
			probes.carrierErrs[probe] = fmt.Errorf("no answer from %s", probe)
		}
		outcome := probes.judge(context.Background(), tcp, quic, dns)
		if outcome.refusal != "" {
			t.Fatalf("%s: the activation was refused for an optional carrier: %s", c.name, outcome.refusal)
		}
		if !slices.Equal(outcome.carriers, c.wantCarriers) {
			t.Fatalf("%s: carriers = %v, want %v", c.name, outcome.carriers, c.wantCarriers)
		}
		if !slices.Equal(outcome.dnsPorts, c.wantDnsPorts) {
			t.Fatalf("%s: dns ports = %v, want %v", c.name, outcome.dnsPorts, c.wantDnsPorts)
		}
		if refused := testRefusedCarrierNames(t, outcome); !slices.Equal(refused, c.wantRefused) {
			t.Fatalf("%s: refused carriers = %v, want %v", c.name, refused, c.wantRefused)
		}
		for _, refusedCarrier := range outcome.refusedCarriers {
			probe := fmt.Sprintf("%s:%d", refusedCarrier.Carrier, refusedCarrier.Port)
			if refusedCarrier.Error != probes.carrierErrs[probe].Error() {
				t.Fatalf("%s: %s was left out for %q, want its probe's error %q", c.name, probe, refusedCarrier.Error, probes.carrierErrs[probe])
			}
		}
	}
}

// Only the required test refuses an activation. A tcp carrier or a forward
// that fails, or a forward that arrived on the other family, refuses with the
// failure named while every optional carrier would answer, and nothing after
// the failure is probed: a refused activation stores nothing, so an optional
// carrier can never stand in for the required test.
func TestExtenderActivationRefusesOnlyOnTheRequiredTest(t *testing.T) {
	tcpProbe := fmt.Sprintf("%s:%d", connect.ExtenderCarrierTcp, testAcceptanceTcpPort)

	cases := []struct {
		name        string
		setup       func(probes *testActivationProbes)
		wantRefusal []string
		wantProbes  []string
	}{
		{
			name: "the tcp carrier",
			setup: func(probes *testActivationProbes) {
				probes.carrierErrs[tcpProbe] = errors.New("connection refused")
			},
			wantRefusal: []string{"the tcp carrier did not answer", "connection refused"},
			wantProbes:  []string{tcpProbe},
		},
		{
			name: "the forward",
			setup: func(probes *testActivationProbes) {
				probes.forwardErr = errors.New("extender hello answered with status 502")
			},
			wantRefusal: []string{"the forward did not answer", "status 502"},
			wantProbes:  []string{tcpProbe, "forward"},
		},
		{
			name: "the forward on the other family",
			setup: func(probes *testActivationProbes) {
				probes.forwardClientAddress = "[2001:db8::10]:443"
			},
			wantRefusal: []string{"ipv6", "ipv4"},
			wantProbes:  []string{tcpProbe, "forward"},
		},
	}
	for _, c := range cases {
		probes := newTestActivationProbes()
		c.setup(probes)
		outcome := probes.judge(
			context.Background(),
			connect.ExtenderCarrierTcp,
			connect.ExtenderCarrierQuic,
			connect.ExtenderCarrierDns,
		)
		for _, want := range c.wantRefusal {
			if !strings.Contains(outcome.refusal, want) {
				t.Fatalf("%s: refusal = %q, want it to name %q", c.name, outcome.refusal, want)
			}
		}
		if !slices.Equal(probes.probes, c.wantProbes) {
			t.Fatalf("%s: probes = %v, want %v", c.name, probes.probes, c.wantProbes)
		}
		connect.AssertEqual(t, len(outcome.carriers), 0)
		connect.AssertEqual(t, len(outcome.dnsPorts), 0)
		connect.AssertEqual(t, len(outcome.refusedCarriers), 0)
	}
}

// The required test is probed before any optional carrier, whatever order
// the carriers were offered in, and the dns carrier before quic. The
// carriers that answered keep the order offered, which is the order the
// address row and the record list them in.
func TestExtenderActivationProbesTheRequiredTestFirst(t *testing.T) {
	probeNames := testOptionalCarrierProbes
	probes := newTestActivationProbes()
	outcome := probes.judge(
		context.Background(),
		connect.ExtenderCarrierQuic,
		connect.ExtenderCarrierDns,
		connect.ExtenderCarrierTcp,
	)
	if outcome.refusal != "" {
		t.Fatalf("the activation was refused: %s", outcome.refusal)
	}
	wantProbes := []string{
		fmt.Sprintf("%s:%d", connect.ExtenderCarrierTcp, testAcceptanceTcpPort),
		"forward",
		probeNames.dns53,
		probeNames.dns4053,
		probeNames.quic,
	}
	if !slices.Equal(probes.probes, wantProbes) {
		t.Fatalf("probes = %v, want %v", probes.probes, wantProbes)
	}
	wantCarriers := []string{
		connect.ExtenderCarrierQuic,
		connect.ExtenderCarrierDns,
		connect.ExtenderCarrierTcp,
	}
	if !slices.Equal(outcome.carriers, wantCarriers) {
		t.Fatalf("carriers = %v, want the order offered %v", outcome.carriers, wantCarriers)
	}
}

// A udp carrier gives no answer to fail fast on, so one that never answers
// waits out the whole of what is left of the shared probe budget. That must
// cost it alone (connect#230): the required test has already passed and the
// dns ports have already been probed, each under its own budget, so the
// activation is accepted on what answered. Here quic is offered first, and
// the budget runs out while it waits.
func TestExtenderActivationOptionalCarrierThatSpendsTheBudgetLosesOnlyItself(t *testing.T) {
	probeNames := testOptionalCarrierProbes
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	probes := newTestActivationProbes()
	probes.answer = func(probeCtx context.Context, probe string) error {
		if probe == probeNames.quic {
			// the budget runs out with no answer
			cancel()
			<-probeCtx.Done()
			return probeCtx.Err()
		}
		if err := probeCtx.Err(); err != nil {
			return fmt.Errorf("%s was probed with the budget spent: %w", probe, err)
		}
		return nil
	}
	outcome := probes.judge(
		ctx,
		connect.ExtenderCarrierQuic,
		connect.ExtenderCarrierTcp,
		connect.ExtenderCarrierDns,
	)
	if outcome.refusal != "" {
		t.Fatalf("the activation was refused for a carrier that spent the budget: %s", outcome.refusal)
	}
	wantCarriers := []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierDns}
	if !slices.Equal(outcome.carriers, wantCarriers) {
		t.Fatalf("carriers = %v, want %v", outcome.carriers, wantCarriers)
	}
	if !slices.Equal(outcome.dnsPorts, testAcceptanceDnsPorts) {
		t.Fatalf("dns ports = %v, want %v", outcome.dnsPorts, testAcceptanceDnsPorts)
	}
	if refused := testRefusedCarrierNames(t, outcome); !slices.Equal(refused, []string{probeNames.quic}) {
		t.Fatalf("refused carriers = %v, want only quic", refused)
	}
	if !strings.Contains(outcome.refusedCarriers[0].Error, context.Canceled.Error()) {
		t.Fatalf("quic was left out for %q, want the spent budget", outcome.refusedCarriers[0].Error)
	}
}

// Each dns port is probed under a budget of its own (L2), and the other
// steps under the shared one, on the activation's clock: the tcp challenge,
// the forward and quic until the shared budget ends, 10 s after the judging
// starts, and each dns port until its own 3 s are up. So a dns port that
// waits out its budget costs 3 s and leaves the rest of the shared budget to
// what comes after it.
func TestExtenderActivationBoundsEachDnsPortOnItsOwn(t *testing.T) {
	probes := newTestActivationProbes()
	start := probes.clock.Now()
	probeDeadline := start.Add(ExtenderActivateProbeTimeout)
	deadlines := map[string]time.Time{}
	probes.answer = func(probeCtx context.Context, probe string) error {
		deadline, ok := probeCtx.Deadline()
		if !ok {
			return fmt.Errorf("%s was probed with no deadline", probe)
		}
		deadlines[probe] = deadline
		if probe == testOptionalCarrierProbes.dns53 {
			return probes.clock.WaitOut(probeCtx)
		}
		return nil
	}
	probes.forwardAnswer = func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			return errors.New("the forward was probed with no deadline")
		}
		deadlines["forward"] = deadline
		return nil
	}
	outcome := probes.judge(
		context.Background(),
		connect.ExtenderCarrierTcp,
		connect.ExtenderCarrierQuic,
		connect.ExtenderCarrierDns,
	)
	if outcome.refusal != "" {
		t.Fatalf("the activation was refused: %s", outcome.refusal)
	}
	tcpProbe := fmt.Sprintf("%s:%d", connect.ExtenderCarrierTcp, testAcceptanceTcpPort)
	for probe, wantDeadline := range map[string]time.Time{
		tcpProbe:  probeDeadline,
		"forward": probeDeadline,
		// its own 3 s, which it waits out
		testOptionalCarrierProbes.dns53: start.Add(ExtenderActivateDnsPortProbeTimeout),
		// its own 3 s from when the first port's ended
		testOptionalCarrierProbes.dns4053: start.Add(2 * ExtenderActivateDnsPortProbeTimeout),
		testOptionalCarrierProbes.quic:    probeDeadline,
	} {
		deadline, probed := deadlines[probe]
		if !probed {
			t.Fatalf("%s was not probed", probe)
		}
		if !deadline.Equal(wantDeadline) {
			t.Fatalf("%s was probed with a deadline %s after the start, want %s", probe, deadline.Sub(start), wantDeadline.Sub(start))
		}
	}
	wantCarriers := []string{
		connect.ExtenderCarrierTcp,
		connect.ExtenderCarrierQuic,
		connect.ExtenderCarrierDns,
	}
	if !slices.Equal(outcome.carriers, wantCarriers) {
		t.Fatalf("carriers = %v, want %v", outcome.carriers, wantCarriers)
	}
	if !slices.Equal(outcome.dnsPorts, []int{4053}) {
		t.Fatalf("dns ports = %v, want only 4053", outcome.dnsPorts)
	}
}

// A dns port's own budget never runs past the shared one: a port probed with
// less than its 3 s left of the shared budget is probed until the shared
// budget ends, and the ports after it get nothing more.
func TestExtenderActivationBoundsADnsPortByWhatIsLeftOfTheBudget(t *testing.T) {
	probes := newTestActivationProbes()
	start := probes.clock.Now()
	probeDeadline := start.Add(ExtenderActivateProbeTimeout)
	tcpProbe := fmt.Sprintf("%s:%d", connect.ExtenderCarrierTcp, testAcceptanceTcpPort)
	deadlines := map[string]time.Time{}
	probes.answer = func(probeCtx context.Context, probe string) error {
		deadline, ok := probeCtx.Deadline()
		if !ok {
			return fmt.Errorf("%s was probed with no deadline", probe)
		}
		deadlines[probe] = deadline
		if err := probeCtx.Err(); err != nil {
			// a probe whose budget is already spent fails at once
			return err
		}
		switch probe {
		case tcpProbe:
			// the challenge answers with 2 s of the budget left
			probes.clock.Advance(ExtenderActivateProbeTimeout - 2*time.Second)
			return nil
		case testOptionalCarrierProbes.dns53:
			return probes.clock.WaitOut(probeCtx)
		}
		return nil
	}
	outcome := probes.judge(context.Background(), connect.ExtenderCarrierTcp, connect.ExtenderCarrierDns)
	if outcome.refusal != "" {
		t.Fatalf("the activation was refused: %s", outcome.refusal)
	}
	if deadline := deadlines[testOptionalCarrierProbes.dns53]; !deadline.Equal(probeDeadline) {
		t.Fatalf("dns:53 was probed with a deadline %s after the start, want the shared budget's %s", deadline.Sub(start), ExtenderActivateProbeTimeout)
	}
	if deadline, probed := deadlines[testOptionalCarrierProbes.dns4053]; probed && deadline.After(probeDeadline) {
		t.Fatalf("dns:4053 was probed past the shared budget, %s after the start", deadline.Sub(start))
	}
	if refused := testRefusedCarrierNames(t, outcome); !slices.Equal(refused, []string{testOptionalCarrierProbes.dns53, testOptionalCarrierProbes.dns4053}) {
		t.Fatalf("refused carriers = %v, want both dns ports", refused)
	}
}
