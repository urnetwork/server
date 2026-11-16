package monitor

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"
	"testing"
	"time"
)

// §28.6 over synthetic series: thirty one-minute evaluations, the longest
// hold, of the relay's offers in the trailing hour by outcome, summed over the
// connect processes. The relay has never run in production, so the healthy
// series are its designed shape, not a replay of Main.

const extenderWebRtcRelayTestPoints = 30

// §28.6's finding identities on Main; every class is fleet-wide.
const (
	extenderWebRtcRelayTestNoAnswer   = "extender-webrtc-relay-no-answer-high main/extender-webrtc-relay"
	extenderWebRtcRelayTestBusy       = "extender-webrtc-relay-busy-high main/extender-webrtc-relay"
	extenderWebRtcRelayTestVisibility = "extender-webrtc-relay-unobservable main/extender-webrtc-relay"
)

// The trailing hour's offers of each outcome at each evaluation, NaN for an
// evaluation without a value. An outcome with a nil slice is left out of the
// answer.
type extenderWebRtcRelayTestSeries map[string][]float64

// 400 offers an hour, nearly all answered: the uptime probes of some thirty
// webrtc addresses and a few activations.
func extenderWebRtcRelayTestHealthy() extenderWebRtcRelayTestSeries {
	return extenderWebRtcRelayTestSeries{
		"answered":     extenderWebRtcRelayTestRepeat(384),
		"no_answer":    extenderWebRtcRelayTestRepeat(12),
		"expired":      extenderWebRtcRelayTestRepeat(4),
		"rate_limited": extenderWebRtcRelayTestRepeat(0),
		"busy":         extenderWebRtcRelayTestRepeat(0),
		"oversized":    extenderWebRtcRelayTestRepeat(0),
		"malformed":    extenderWebRtcRelayTestRepeat(0),
	}
}

// The healthy series with the given outcomes replaced; a nil slice leaves an
// outcome out.
func extenderWebRtcRelayTestWith(replaced map[string][]float64) extenderWebRtcRelayTestSeries {
	series := extenderWebRtcRelayTestHealthy()
	for outcome, values := range replaced {
		series[outcome] = values
	}
	return series
}

func extenderWebRtcRelayTestResponse(t *testing.T, series extenderWebRtcRelayTestSeries) string {
	t.Helper()
	outcomes := []string{}
	for outcome, values := range series {
		if values != nil {
			outcomes = append(outcomes, outcome)
		}
	}
	sort.Strings(outcomes)
	synthetic := []stormSyntheticSeries{}
	for _, outcome := range outcomes {
		synthetic = append(synthetic, stormSyntheticSeries{part: "offers", labels: map[string]string{"outcome": outcome}, values: series[outcome]})
	}
	return stormSyntheticResponse(t, stormSyntheticStart(extenderWebRtcRelayTestPoints), time.Minute, synthetic)
}

// One value at every evaluation.
func extenderWebRtcRelayTestRepeat(value float64) []float64 {
	return stormRepeat(value, extenderWebRtcRelayTestPoints)
}

// Values that change at a step: before for the first count, after for the
// rest.
func extenderWebRtcRelayTestStep(before float64, after float64, count int) []float64 {
	values := extenderWebRtcRelayTestRepeat(after)
	for index := range count {
		values[index] = before
	}
	return values
}

type extenderWebRtcRelayTestCase struct {
	name   string
	series extenderWebRtcRelayTestSeries
	want   map[string]string
}

func runExtenderWebRtcRelayTestCases(t *testing.T, cases []extenderWebRtcRelayTestCase) {
	t.Helper()
	for _, test := range cases {
		states := stormFindingStates(t, extenderWebRtcRelayProbe{}, stormSyntheticNow, extenderWebRtcRelayTestResponse(t, test.series), nil)
		requireStormStates(t, test.name, states, test.want)
	}
}

// More than half of the trailing hour's offers unanswered, with at least 20
// offers, at every evaluation of the 30-minute hold opens the incident. A
// shorter run only holds it; too few offers, or a share at or under half,
// clears it.
func TestExtenderWebRtcRelayJudgesNoAnswer(t *testing.T) {
	const (
		noAnswer   = extenderWebRtcRelayTestNoAnswer
		busy       = extenderWebRtcRelayTestBusy
		visibility = extenderWebRtcRelayTestVisibility
	)
	repeat := extenderWebRtcRelayTestRepeat
	runExtenderWebRtcRelayTestCases(t, []extenderWebRtcRelayTestCase{
		{name: "healthy", series: extenderWebRtcRelayTestHealthy(), want: map[string]string{
			noAnswer: "healthy", busy: "healthy", visibility: "healthy",
		}},
		// the answerer stopped answering: 280 of 400 offers for the whole hold
		{name: "unanswered for the hold", series: extenderWebRtcRelayTestWith(map[string][]float64{
			"answered": repeat(108), "no_answer": repeat(280), "expired": repeat(12),
		}), want: map[string]string{noAnswer: "warn", busy: "healthy", visibility: "healthy"}},
		// the same share for the last twenty evaluations only
		{name: "unanswered for twenty minutes", series: extenderWebRtcRelayTestWith(map[string][]float64{
			"answered":  extenderWebRtcRelayTestStep(384, 108, extenderWebRtcRelayTestPoints-20),
			"no_answer": extenderWebRtcRelayTestStep(12, 280, extenderWebRtcRelayTestPoints-20),
		}), want: map[string]string{noAnswer: "hold", busy: "healthy", visibility: "healthy"}},
		// 15 of 18 offers unanswered: too few to judge, a quiet relay and not
		// an unknown one
		{name: "too few offers", series: extenderWebRtcRelayTestWith(map[string][]float64{
			"answered": repeat(2), "no_answer": repeat(15), "expired": repeat(1),
		}), want: map[string]string{noAnswer: "healthy", busy: "healthy", visibility: "healthy"}},
		// 11 of exactly 20 offers: the minimum is judged
		{name: "at the minimum", series: extenderWebRtcRelayTestWith(map[string][]float64{
			"answered": repeat(9), "no_answer": repeat(11), "expired": repeat(0),
		}), want: map[string]string{noAnswer: "warn", busy: "healthy", visibility: "healthy"}},
		// exactly half is not more than half
		{name: "half unanswered", series: extenderWebRtcRelayTestWith(map[string][]float64{
			"answered": repeat(196), "no_answer": repeat(200),
		}), want: map[string]string{noAnswer: "healthy", visibility: "healthy"}},
		{name: "under half unanswered", series: extenderWebRtcRelayTestWith(map[string][]float64{
			"answered": repeat(236), "no_answer": repeat(160),
		}), want: map[string]string{noAnswer: "healthy", visibility: "healthy"}},
	})
}

// More than 5% of the trailing hour's offers refused busy, with at least 20
// offers, at every evaluation of the 30-minute hold opens the incident. A
// shorter run only holds it; too few offers, or a share at or under 5%,
// clears it.
func TestExtenderWebRtcRelayJudgesBusy(t *testing.T) {
	const (
		noAnswer   = extenderWebRtcRelayTestNoAnswer
		busy       = extenderWebRtcRelayTestBusy
		visibility = extenderWebRtcRelayTestVisibility
	)
	repeat := extenderWebRtcRelayTestRepeat
	runExtenderWebRtcRelayTestCases(t, []extenderWebRtcRelayTestCase{
		// the exchanges refuse 40 of 400 offers busy for the whole hold
		{name: "busy for the hold", series: extenderWebRtcRelayTestWith(map[string][]float64{
			"answered": repeat(344), "busy": repeat(40),
		}), want: map[string]string{busy: "warn", noAnswer: "healthy", visibility: "healthy"}},
		// the same share for the last twenty evaluations only
		{name: "busy for twenty minutes", series: extenderWebRtcRelayTestWith(map[string][]float64{
			"answered": extenderWebRtcRelayTestStep(384, 344, extenderWebRtcRelayTestPoints-20),
			"busy":     extenderWebRtcRelayTestStep(0, 40, extenderWebRtcRelayTestPoints-20),
		}), want: map[string]string{busy: "hold", noAnswer: "healthy", visibility: "healthy"}},
		// 5 of 15 offers busy: too few to judge
		{name: "too few offers", series: extenderWebRtcRelayTestWith(map[string][]float64{
			"answered": repeat(10), "no_answer": repeat(0), "expired": repeat(0), "busy": repeat(5),
		}), want: map[string]string{busy: "healthy", noAnswer: "healthy", visibility: "healthy"}},
		// 2 of exactly 20 offers busy: the minimum is judged
		{name: "at the minimum", series: extenderWebRtcRelayTestWith(map[string][]float64{
			"answered": repeat(18), "no_answer": repeat(0), "expired": repeat(0), "busy": repeat(2),
		}), want: map[string]string{busy: "warn", noAnswer: "healthy", visibility: "healthy"}},
		// exactly 5% is not more than 5%
		{name: "five percent busy", series: extenderWebRtcRelayTestWith(map[string][]float64{
			"answered": repeat(364), "busy": repeat(20),
		}), want: map[string]string{busy: "healthy", visibility: "healthy"}},
		{name: "under five percent busy", series: extenderWebRtcRelayTestWith(map[string][]float64{
			"answered": repeat(374), "busy": repeat(10),
		}), want: map[string]string{busy: "healthy", visibility: "healthy"}},
	})
}

// Without the counter nothing is judged, and its absence is unknown, never a
// quiet relay. An outcome missing at an evaluation, or from the whole answer,
// is a gap: it keeps both shares from opening or clearing, and is named.
func TestExtenderWebRtcRelayReportsMissingSeries(t *testing.T) {
	probe := extenderWebRtcRelayProbe{}
	nothing := extenderWebRtcRelayTestSeries{}
	states := stormFindingStates(t, probe, stormSyntheticNow, extenderWebRtcRelayTestResponse(t, nothing), nil)
	if len(states) != 1 || states[extenderWebRtcRelayTestVisibility] != "unobservable" {
		t.Fatalf("a connect build without the counter: %v", states)
	}
	alerts := runStormSynthetic(t, NewExtenderWebRtcRelaySignal(), stormSyntheticNow, extenderWebRtcRelayTestResponse(t, nothing), nil)
	if len(alerts) != 1 || alerts[0].Class != "extender-webrtc-relay-unobservable" || !alerts[0].unobservable ||
		!strings.Contains(alerts[0].Observed, "relay_state_missing") {
		t.Fatalf("a connect build without the counter alerted %+v", alerts)
	}

	// unanswered past the line at every evaluation but one, which has no
	// answered offers
	gappy := extenderWebRtcRelayTestWith(map[string][]float64{
		"answered": extenderWebRtcRelayTestRepeat(108), "no_answer": extenderWebRtcRelayTestRepeat(280),
	})
	gappy["answered"][20] = math.NaN()
	// an answer without the busy outcome at all
	noBusy := extenderWebRtcRelayTestWith(map[string][]float64{"busy": nil})
	for _, test := range []struct {
		name   string
		series extenderWebRtcRelayTestSeries
		gap    string
	}{
		{name: "an evaluation without its answered offers", series: gappy, gap: "answered[missing]"},
		{name: "no busy outcome", series: noBusy, gap: "busy[missing]"},
	} {
		states := stormFindingStates(t, probe, stormSyntheticNow, extenderWebRtcRelayTestResponse(t, test.series), nil)
		requireStormStates(t, test.name, states, map[string]string{
			extenderWebRtcRelayTestNoAnswer:   "hold",
			extenderWebRtcRelayTestBusy:       "hold",
			extenderWebRtcRelayTestVisibility: "unobservable",
		})
		alerts := runStormSynthetic(t, NewExtenderWebRtcRelaySignal(), stormSyntheticNow, extenderWebRtcRelayTestResponse(t, test.series), nil)
		if len(alerts) != 1 || alerts[0].Class != "extender-webrtc-relay-unobservable" ||
			!strings.Contains(alerts[0].Observed, "evaluation_missing") || !strings.Contains(alerts[0].Observed, test.gap) {
			t.Fatalf("%s: alerts %+v", test.name, alerts)
		}
	}
}

// A read that fails, does not parse or carries a series of another shape is
// the visibility class with a fixed reason; the source's text never reaches
// an alert, and a canceled owner publishes nothing.
func TestExtenderWebRtcRelayReportsUnreadableSeries(t *testing.T) {
	for _, test := range []struct {
		name   string
		answer string
		err    error
		reason string
	}{
		{name: "transport", answer: "private-canary", err: errors.New("private-canary 192.0.2.7"), reason: "source_unavailable"},
		{name: "malformed", answer: "private-canary", reason: "invalid_response"},
		{
			name:   "a series of another shape",
			answer: `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"monitor_part":"offers","outcome":"answered","instance":"private-canary"},"values":[]}]}}`,
			reason: "unrecognized_label",
		},
	} {
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return test.answer, test.err }}
		alerts, err := NewExtenderWebRtcRelaySignal().Run(context.Background(), stormMainSettings(stormSyntheticNow, source))
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		found := false
		for _, alert := range alerts {
			requireAlertOmits(t, alert, "private-canary", "192.0.2.7")
			switch alert.Class {
			case "extender-webrtc-relay-unobservable":
				found = alert.unobservable && alert.Severity == SeverityWarn && strings.Contains(alert.Observed, test.reason)
			case "extender-webrtc-relay-no-answer-high", "extender-webrtc-relay-busy-high":
				t.Fatalf("%s: judged an unreadable relay: %+v", test.name, alert)
			}
		}
		if !found {
			t.Fatalf("%s: no visibility alert naming %s in %+v", test.name, test.reason, alerts)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		cancel()
		return "", context.Canceled
	}}
	alerts, err := NewExtenderWebRtcRelaySignal().Run(ctx, stormMainSettings(stormSyntheticNow, source))
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
		t.Fatalf("a canceled owner published %v (%v)", alerts, err)
	}
}

// The alerts name the share, the window's offers by outcome and where to
// look, and an incident class reaches the notifier as an incident, not as an
// unknown state.
func TestExtenderWebRtcRelayAlertsNameShareCountsAndWhereToLook(t *testing.T) {
	repeat := extenderWebRtcRelayTestRepeat
	series := extenderWebRtcRelayTestWith(map[string][]float64{
		"answered": repeat(68), "no_answer": repeat(280), "expired": repeat(12), "busy": repeat(40),
	})
	alerts := runStormSynthetic(t, NewExtenderWebRtcRelaySignal(), stormSyntheticNow, extenderWebRtcRelayTestResponse(t, series), nil)
	const counts = "window=1h offers=400 answered=68 no_answer=280 expired=12 rate_limited=0 busy=40 oversized=0 malformed=0 no_answer_share=0.70 busy_share=0.10"
	found := map[string]bool{}
	for _, alert := range alerts {
		if alert.SignalNumber != "28.6" || alert.SignalID != "mimir/extender-webrtc-relay" || alert.Target != "main/extender-webrtc-relay" ||
			alert.Severity != SeverityWarn || alert.unobservable || alert.Playbook != "SIGNALS.md §28.6" ||
			!strings.Contains(alert.Observed, counts) {
			t.Fatalf("alert %+v", alert)
		}
		var wants []string
		switch alert.Class {
		case "extender-webrtc-relay-no-answer-high":
			wants = []string{
				"More than 50% of the webrtc extender offers", "with at least 20 offers",
				"net_extender_webrtc_signal.go", "connect/extender_webrtc_relay.go", "control path",
			}
		case "extender-webrtc-relay-busy-high":
			wants = []string{
				"More than 5% of the webrtc extender offers", "with at least 20 offers",
				"before raising MaxConcurrentRelays", "sum by (host, block)",
			}
		default:
			t.Fatalf("unexpected alert %+v", alert)
		}
		text := alert.Symptom + "\n" + alert.Action
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Fatalf("%s does not say %q:\n%s", alert.Class, want, text)
			}
		}
		found[alert.Class] = true
	}
	if !found["extender-webrtc-relay-no-answer-high"] || !found["extender-webrtc-relay-busy-high"] {
		t.Fatalf("alerts %+v", alerts)
	}
}

func TestExtenderWebRtcRelayRegisteredAndClassified(t *testing.T) {
	found := 0
	for _, signal := range NewSignals() {
		if signal.Key() == "extender-webrtc-relay" {
			found++
			if signal.Number() != "28.6" || signal.ID() != "mimir/extender-webrtc-relay" || signal.Cadence() != 5*time.Minute {
				t.Fatalf("registered as §%s %s every %s", signal.Number(), signal.ID(), signal.Cadence())
			}
		}
	}
	if found != 1 {
		t.Fatalf("extender-webrtc-relay registered %d times", found)
	}
	for class, kind := range map[string]alertClassKind{
		"extender-webrtc-relay-no-answer-high": incidentAlertClass,
		"extender-webrtc-relay-busy-high":      incidentAlertClass,
		"extender-webrtc-relay-unobservable":   visibilityAlertClass,
	} {
		if got, ok := alertClassKinds[class]; !ok || got != kind {
			t.Errorf("%s is classified %v (listed %t)", class, got, ok)
		}
	}
}

// The query reads the counter §28.6 names over its window, the defaults are
// §28.6's, and an invalid policy is the visibility class before any read.
func TestExtenderWebRtcRelayQueryAndPolicy(t *testing.T) {
	settings := DefaultExtenderWebRtcRelaySettings()
	if settings.Window != time.Hour || settings.MinOffers != 20 || settings.NoAnswerShare != 0.5 ||
		settings.NoAnswerHold != 30*time.Minute || settings.BusyShare != 0.05 || settings.BusyHold != 30*time.Minute ||
		settings.Cadence != 5*time.Minute || settings.Step != time.Minute {
		t.Fatalf("§28.6 policy changed: %+v", settings)
	}
	want := `sum by (outcome) (increase(urnetwork_extender_webrtc_relay_offers_total{env="main"}[1h]))`
	if query := extenderWebRtcRelayQuery("main", settings); !strings.Contains(query, want) {
		t.Fatalf("query lost %s:\n%s", want, query)
	}

	for _, test := range []struct {
		name   string
		mutate func(*ExtenderWebRtcRelaySettings)
	}{
		{name: "a share of one", mutate: func(settings *ExtenderWebRtcRelaySettings) { settings.NoAnswerShare = 1 }},
		{name: "a share of zero", mutate: func(settings *ExtenderWebRtcRelaySettings) { settings.BusyShare = 0 }},
		{name: "no minimum", mutate: func(settings *ExtenderWebRtcRelaySettings) { settings.MinOffers = 0 }},
		{name: "a window of seconds", mutate: func(settings *ExtenderWebRtcRelaySettings) { settings.Window = 90 * time.Second }},
		{name: "a hold past one read", mutate: func(settings *ExtenderWebRtcRelaySettings) { settings.NoAnswerHold = 121 * time.Minute }},
		{name: "a hold of seconds", mutate: func(settings *ExtenderWebRtcRelaySettings) { settings.BusyHold = 90 * time.Second }},
		{name: "nothing listed", mutate: func(settings *ExtenderWebRtcRelaySettings) { settings.MaxListed = 0 }},
		{name: "empty"},
	} {
		invalid := DefaultExtenderWebRtcRelaySettings()
		if test.mutate == nil {
			invalid = &ExtenderWebRtcRelaySettings{}
		} else {
			test.mutate(invalid)
		}
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			t.Fatalf("%s: read its source under an invalid policy", test.name)
			return "", nil
		}}
		alerts, err := NewExtenderWebRtcRelaySignalWithSettings(invalid).Run(context.Background(), stormMainSettings(stormSyntheticNow, source))
		if err != nil || len(alerts) != 1 || alerts[0].Class != "extender-webrtc-relay-unobservable" || !alerts[0].unobservable ||
			!strings.Contains(alerts[0].Observed, "invalid_policy") {
			t.Fatalf("%s: %v %+v", test.name, err, alerts)
		}
	}
	if cadence := NewExtenderWebRtcRelaySignalWithSettings(&ExtenderWebRtcRelaySettings{}).Cadence(); cadence != time.Minute {
		t.Fatalf("an invalid cadence reached the scheduler: %s", cadence)
	}
}
