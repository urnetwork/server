package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

// The operator's cap on extenders accepted on the webrtc carrier without tcp
// (ExtenderActivateSettings.MaxWebRtcOnlyExtenders, connect/EXTENDER.md S8),
// judged over the fake probes of extender_activate_acceptance_test.go. The
// count itself is the model's (CountActiveWebRtcOnlyNetworkExtenders) and is
// tested against the database there.

// Judges an ipv4 activation that offers the given carriers with the cap
// answered by room, which records each time it is asked.
func (self *testActivationProbes) judgeWithWebRtcOnlyRoom(
	room func(ctx context.Context) error,
	carriers ...string,
) (outcome *extenderActivationOutcome, asked int) {
	activationProbes := self.activationProbes()
	activationProbes.webRtcOnlyRoom = func(ctx context.Context) error {
		asked += 1
		return room(ctx)
	}
	outcome = probeExtenderActivation(
		context.Background(),
		self.clock.activationClock(),
		activationProbes,
		testActivationTcpChallengeTimeout,
		carriers,
		testAcceptanceTcpPort,
		testAcceptanceUdpPort,
		testAcceptanceDnsPorts,
		4,
	)
	return
}

// Root cause: nothing bounded how many extenders the operator accepts on the
// webrtc carrier without tcp, each of which costs every uptime pass a relayed
// offer and an ICE join at the same probe concurrency. Observable: past the
// cap an activation whose tcp path failed is refused with the webrtc test
// untried -- the extender's device is sent no offer -- naming the tcp failure
// and the cap, at the webrtc stage with the cap outcome, which has a series;
// one that offers webrtc alone is refused before anything is probed; one
// whose tcp path passes is accepted on tcp and never asks the cap; below the
// cap the webrtc test runs; and a cap that cannot be read refuses the webrtc
// test untried, counted as an error.
func TestExtenderActivationCapsExtendersOnWebRtcWithoutTcp(t *testing.T) {
	tcpProbe := fmt.Sprintf("%s:%d", connect.ExtenderCarrierTcp, testAcceptanceTcpPort)
	full := func(ctx context.Context) error {
		return extenderWebRtcOnlyRoom(100, 100)
	}

	refusedTcp := newTestActivationProbes()
	refusedTcp.carrierErrs[tcpProbe] = errors.New("connection refused")
	outcome, asked := refusedTcp.judgeWithWebRtcOnlyRoom(full, connect.ExtenderCarrierTcp, connect.ExtenderCarrierWebRtc, connect.ExtenderCarrierQuic)
	connect.AssertEqual(t, asked, 1)
	connect.AssertEqual(t, refusedTcp.probesSoFar(), []string{tcpProbe})
	for _, want := range []string{
		"the tcp carrier failed after",
		"; the webrtc carrier was not tried: " + ErrExtenderWebRtcOnlyCap.Error(),
		"(100 accepted, the cap is 100)",
		"is accepted on tcp",
	} {
		if !strings.Contains(outcome.refusal, want) {
			t.Fatalf("refusal = %q, want it to name %q", outcome.refusal, want)
		}
	}
	connect.AssertEqual(t, outcome.refusalStep, connect.ExtenderCarrierWebRtc)
	connect.AssertEqual(t, outcome.refusalOutcome, extenderActivationOutcomeCap)
	if outcomes := extenderActivationStageOutcomes[outcome.refusalStep]; !slices.Contains(outcomes, outcome.refusalOutcome) {
		t.Fatalf("the refusal is counted at %s %s, which has no series", outcome.refusalStep, outcome.refusalOutcome)
	}
	// the tcp step ran; nothing was probed over webrtc
	connect.AssertEqual(t, testStepNames(outcome), []string{connect.ExtenderCarrierTcp})
	connect.AssertEqual(t, len(outcome.carriers), 0)

	webRtcAlone := newTestActivationProbes()
	outcome, asked = webRtcAlone.judgeWithWebRtcOnlyRoom(full, connect.ExtenderCarrierWebRtc)
	connect.AssertEqual(t, asked, 1)
	connect.AssertEqual(t, len(webRtcAlone.probesSoFar()), 0)
	connect.AssertEqual(t, len(outcome.steps), 0)
	if !strings.HasPrefix(outcome.refusal, "the webrtc carrier was not tried: "+ErrExtenderWebRtcOnlyCap.Error()) {
		t.Fatalf("refusal = %q, want the cap's", outcome.refusal)
	}
	connect.AssertEqual(t, outcome.refusalOutcome, extenderActivationOutcomeCap)

	passingTcp := newTestActivationProbes()
	outcome, asked = passingTcp.judgeWithWebRtcOnlyRoom(full, connect.ExtenderCarrierTcp, connect.ExtenderCarrierWebRtc)
	if outcome.refusal != "" {
		t.Fatalf("a tcp path that passed was refused: %s", outcome.refusal)
	}
	connect.AssertEqual(t, asked, 0)
	connect.AssertEqual(t, outcome.carriers, []string{connect.ExtenderCarrierTcp})

	room := newTestActivationProbes()
	room.carrierErrs[tcpProbe] = errors.New("connection refused")
	outcome, asked = room.judgeWithWebRtcOnlyRoom(func(ctx context.Context) error {
		return extenderWebRtcOnlyRoom(100, 99)
	}, connect.ExtenderCarrierTcp, connect.ExtenderCarrierWebRtc)
	if outcome.refusal != "" {
		t.Fatalf("refused below the cap: %s", outcome.refusal)
	}
	connect.AssertEqual(t, asked, 1)
	connect.AssertEqual(t, outcome.carriers, []string{connect.ExtenderCarrierWebRtc})
	connect.AssertEqual(t, room.probesSoFar(), []string{tcpProbe, testWebRtcProbe})

	unreadable := newTestActivationProbes()
	outcome, _ = unreadable.judgeWithWebRtcOnlyRoom(func(ctx context.Context) error {
		panic("synthetic count failure")
	}, connect.ExtenderCarrierWebRtc)
	if !strings.Contains(outcome.refusal, "the webrtc carrier was not tried") ||
		!strings.Contains(outcome.refusal, "synthetic count failure") {
		t.Fatalf("refusal = %q, want the unread cap", outcome.refusal)
	}
	connect.AssertEqual(t, outcome.refusalOutcome, extenderActivationOutcomeError)
	connect.AssertEqual(t, len(unreadable.probesSoFar()), 0)
}

// The cap's comparison: room while the count is under it, the cap's refusal
// at it and past it, and none at all for a cap that is not positive.
func TestExtenderWebRtcOnlyRoom(t *testing.T) {
	if err := extenderWebRtcOnlyRoom(100, 0); err != nil {
		t.Fatalf("an empty cap refused: %v", err)
	}
	if err := extenderWebRtcOnlyRoom(100, 99); err != nil {
		t.Fatalf("the last place refused: %v", err)
	}
	for _, c := range []struct {
		maxCount int
		count    int
		want     string
	}{
		{maxCount: 100, count: 100, want: "(100 accepted, the cap is 100)"},
		{maxCount: 100, count: 120, want: "(120 accepted, the cap is 100)"},
		{maxCount: 0, count: 0, want: "(0 accepted, the cap is 0)"},
		{maxCount: -1, count: 0, want: "(0 accepted, the cap is 0)"},
	} {
		err := extenderWebRtcOnlyRoom(c.maxCount, c.count)
		if !errors.Is(err, ErrExtenderWebRtcOnlyCap) || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("cap %d at %d: err = %v, want %q", c.maxCount, c.count, err, c.want)
		}
	}
	connect.AssertEqual(t, DefaultExtenderActivateSettings().MaxWebRtcOnlyExtenders, 100)
}
