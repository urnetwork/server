// Source completeness, recovery and privacy controls for SIGNALS.md §2.19b.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
)

type sitePoolCoverageTestSeries struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value"`
}

// The legacy and paired responses describe the same synthetic counters. The
// query, not the test, chooses which observation contract the probe receives.
func sitePoolCoverageTestResponse(t testing.TB, fixture *egressSitePoolFixture, command string, mutate func([]sitePoolCoverageTestSeries) []sitePoolCoverageTestSeries) string {
	t.Helper()
	var response struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string                       `json:"resultType"`
			Result     []sitePoolCoverageTestSeries `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(fixture.mimirResponse()), &response); err != nil {
		t.Fatal(err)
	}
	now := syntheticSettings(nil).Now()
	at := float64(now.Unix())
	if strings.Contains(command, "monitor_egress_sample") {
		filtered := []sitePoolCoverageTestSeries{}
		for _, row := range response.Data.Result {
			rank, schedule := row.Metric["rank_mode"], row.Metric["schedule"]
			if rank == "quality" || rank == "speed" || schedule == "full" || schedule == "blackhole" {
				row.Value[0] = at
				filtered = append(filtered, row)
			}
		}
		response.Data.Result = filtered
		add := func(service, host, instance, key string, value float64) {
			response.Data.Result = append(response.Data.Result, sitePoolCoverageTestSeries{
				Metric: map[string]string{"env": "synthetic", "job": service, "host": host, "block": "blue", "instance": instance, "monitor_egress_sample": key},
				Value:  []any{at, strconv.FormatFloat(value, 'g', -1, 64)},
			})
		}
		for _, service := range []string{"api", "taskworker"} {
			hosts := []string{"api-a.example", "api-b.example"}
			fields := map[string]float64{}
			if service == "api" {
				for _, rank := range []string{"quality", "speed"} {
					if v, ok := fixture.borrowed[rank]; ok {
						fields["borrowed-"+rank] = v / 2
					}
					if v, ok := fixture.answered[rank]; ok {
						fields["answered-"+rank] = v / 2
					}
				}
			} else {
				hosts = []string{"worker-a.example"}
				for _, schedule := range []string{"full", "blackhole"} {
					if v, ok := fixture.guardTrips[schedule]; ok {
						fields["guard-"+schedule] = v
					}
				}
			}
			for _, host := range hosts {
				instance := host + "/one"
				add(service, host, instance, "present", 60)
				for field := range fields {
					add(service, host, instance, "resets/"+field, 0)
				}
				for _, bound := range []string{"now", "prior"} {
					sampleAt := at - 10
					if bound == "prior" {
						sampleAt -= 3600
					}
					add(service, host, instance, bound+"/start", at-7200)
					add(service, host, instance, bound+"/start/time", sampleAt)
					for field, delta := range fields {
						value := 100.0
						if bound == "now" {
							value += delta
						}
						add(service, host, instance, bound+"/"+field, value)
						add(service, host, instance, bound+"/"+field+"/time", sampleAt)
					}
				}
			}
		}
	}
	if mutate != nil {
		response.Data.Result = mutate(response.Data.Result)
	}
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Run the real source adapter and reducer; never replace coverage with an
// injected healthy verdict.
func sitePoolCoverageTestFindings(t *testing.T, fixture *egressSitePoolFixture, mutate func([]sitePoolCoverageTestSeries) []sitePoolCoverageTestSeries, settingsMutation func(*SignalSettings)) []finding {
	t.Helper()
	source := fixture.source(t)
	shell := source.hostFn
	source.hostFn = func(host HostSettings, command string) (string, error) {
		if fixture.mimirDown {
			return shell(host, command)
		}
		return sitePoolCoverageTestResponse(t, fixture, command, mutate), nil
	}
	settings := testEgressSitePoolSettings(source)
	if settingsMutation != nil {
		settingsMutation(&settings)
	}
	probe := egressSitePoolProbe{settings: DefaultEgressSitePoolSettings(), loadPoolSettings: func() (*egressSitePoolContext, error) { return testEgressSitePoolContext(), nil }}
	findings, err := probe.check(context.Background(), mustEgressSitePoolProbeEnv(t, settings))
	if err != nil {
		t.Fatal(err)
	}
	return findings
}

func requireSitePoolCoverageUnknown(t *testing.T, findings []finding, classes ...string) {
	t.Helper()
	visible := false
	for _, f := range findings {
		visible = visible || f.class == "egress-site-pool-unobservable" && !f.healthy
		for _, class := range classes {
			if f.class == class && f.healthy {
				t.Fatalf("unobserved %s falsely certified healthy", class)
			}
		}
	}
	if !visible {
		t.Fatal("missing explicit metrics coverage finding")
	}
}

func TestEgressSitePoolCoverageUnreadableDoesNotResolveGuard(t *testing.T) {
	f := healthyEgressSitePoolFixture()
	f.mimirDown = true
	requireSitePoolCoverageUnknown(t, sitePoolCoverageTestFindings(t, f, nil, nil), "egress-prober-fault", "egress-backfill-sustained")
}

func TestEgressSitePoolCoverageEmptyDoesNotCertifyHealth(t *testing.T) {
	requireSitePoolCoverageUnknown(t, sitePoolCoverageTestFindings(t, healthyEgressSitePoolFixture(), func([]sitePoolCoverageTestSeries) []sitePoolCoverageTestSeries { return nil }, nil), "egress-prober-fault", "egress-backfill-sustained")
}

func TestEgressSitePoolCoverageLazyChildrenAreUnknown(t *testing.T) {
	for _, part := range []string{"borrowed", "answered", "guard"} {
		f := healthyEgressSitePoolFixture()
		class := "egress-backfill-sustained"
		switch part {
		case "borrowed":
			delete(f.borrowed, "quality")
		case "answered":
			delete(f.answered, "quality")
		case "guard":
			delete(f.guardTrips, "full")
			class = "egress-prober-fault"
		}
		requireSitePoolCoverageUnknown(t, sitePoolCoverageTestFindings(t, f, nil, nil), class)
	}
}

func TestEgressSitePoolCoverageMissingSlotCannotBorrowSibling(t *testing.T) {
	requireSitePoolCoverageUnknown(t, sitePoolCoverageTestFindings(t, healthyEgressSitePoolFixture(), func(rows []sitePoolCoverageTestSeries) []sitePoolCoverageTestSeries {
		out := []sitePoolCoverageTestSeries{}
		for _, r := range rows {
			if r.Metric["host"] != "api-b.example" {
				out = append(out, r)
			}
		}
		return out
	}, nil), "egress-backfill-sustained")
}

func TestEgressSitePoolCoverageOneFreshCannotHideStale(t *testing.T) {
	requireSitePoolCoverageUnknown(t, sitePoolCoverageTestFindings(t, healthyEgressSitePoolFixture(), func(rows []sitePoolCoverageTestSeries) []sitePoolCoverageTestSeries {
		for i := range rows {
			if rows[i].Metric["host"] == "api-b.example" && strings.HasSuffix(rows[i].Metric["monitor_egress_sample"], "/time") {
				v, _ := strconv.ParseFloat(rows[i].Value[1].(string), 64)
				rows[i].Value[1] = strconv.FormatFloat(v-300, 'g', -1, 64)
			}
		}
		return rows
	}, nil), "egress-backfill-sustained")
}

// The extra generation exists only inside the window. Endpoints alone must
// not silently declare this rollout interval complete.
func TestEgressSitePoolCoverageRangeOnlyGeneration(t *testing.T) {
	requireSitePoolCoverageUnknown(t, sitePoolCoverageTestFindings(t, healthyEgressSitePoolFixture(), func(rows []sitePoolCoverageTestSeries) []sitePoolCoverageTestSeries {
		for _, r := range rows {
			if r.Metric["host"] == "worker-a.example" && r.Metric["monitor_egress_sample"] == "present" {
				copyRow := sitePoolCoverageTestSeries{Metric: map[string]string{}, Value: append([]any(nil), r.Value...)}
				for k, v := range r.Metric {
					copyRow.Metric[k] = v
				}
				copyRow.Metric["instance"] = "worker-a.example/retired"
				return append(rows, copyRow)
			}
		}
		return rows
	}, nil), "egress-prober-fault")
}

func TestEgressSitePoolCoverageCounterResetIsUnknown(t *testing.T) {
	requireSitePoolCoverageUnknown(t, sitePoolCoverageTestFindings(t, healthyEgressSitePoolFixture(), func(rows []sitePoolCoverageTestSeries) []sitePoolCoverageTestSeries {
		for i := range rows {
			if rows[i].Metric["monitor_egress_sample"] == "resets/guard-full" {
				rows[i].Value[1] = "1"
			}
		}
		return rows
	}, nil), "egress-prober-fault")
}

func TestEgressSitePoolCoverageMixedScrapeIsUnknown(t *testing.T) {
	requireSitePoolCoverageUnknown(t, sitePoolCoverageTestFindings(t, healthyEgressSitePoolFixture(), func(rows []sitePoolCoverageTestSeries) []sitePoolCoverageTestSeries {
		for i := range rows {
			if rows[i].Metric["monitor_egress_sample"] == "now/guard-full/time" {
				v, _ := strconv.ParseFloat(rows[i].Value[1].(string), 64)
				rows[i].Value[1] = strconv.FormatFloat(v-1, 'g', -1, 64)
			}
		}
		return rows
	}, nil), "egress-prober-fault")
}

func TestEgressSitePoolCoverageExcludedAndMissingInventory(t *testing.T) {
	for _, absent := range []bool{false, true} {
		requireSitePoolCoverageUnknown(t, sitePoolCoverageTestFindings(t, healthyEgressSitePoolFixture(), nil, func(s *SignalSettings) {
			if absent {
				s.LogServiceHosts = nil
			} else {
				s.ExcludedHosts = []string{"api-b.example"}
			}
		}), "egress-backfill-sustained")
	}
}

func TestEgressSitePoolCoverageIdleAnswersDoNotResolve(t *testing.T) {
	f := healthyEgressSitePoolFixture()
	f.answered = map[string]float64{"quality": 0, "speed": 0}
	f.borrowed = map[string]float64{"quality": 0, "speed": 0}
	requireSitePoolCoverageUnknown(t, sitePoolCoverageTestFindings(t, f, nil, nil), "egress-backfill-sustained")
}

func TestEgressSitePoolCoverageRetainsPositivePartialEvidence(t *testing.T) {
	f := healthyEgressSitePoolFixture()
	f.guardTrips = map[string]float64{"full": 2}
	findings := sitePoolCoverageTestFindings(t, f, nil, nil)
	guard := false
	for _, v := range findings {
		guard = guard || v.class == "egress-prober-fault" && v.frame == "guard-full" && !v.healthy
	}
	if !guard {
		t.Fatal("known positive guard trip was suppressed by a missing sibling")
	}
	requireSitePoolCoverageUnknown(t, findings, "egress-prober-fault")
}

func TestEgressSitePoolCoverageDatabaseFailureSurvivesMimirLoss(t *testing.T) {
	f := healthyEgressSitePoolFixture()
	f.mimirDown = true
	f.classLoads = [][]string{{"cdn", "1", "1000"}, {"connectivity", "1", "1000"}, {"dns", "1", "1000"}, {"site", "1", "1000"}}
	findings := sitePoolCoverageTestFindings(t, f, nil, nil)
	for _, v := range findings {
		if v.class == "egress-prober-fault" && v.frame == "failure-share" && !v.healthy {
			return
		}
	}
	t.Fatal("independent measured failure pattern disappeared with Mimir")
}

func TestEgressSitePoolCoverageCompleteHealthyControl(t *testing.T) {
	findings := sitePoolCoverageTestFindings(t, healthyEgressSitePoolFixture(), nil, nil)
	healthy := map[string]bool{}
	for _, f := range findings {
		if !f.healthy {
			t.Fatalf("complete healthy fixture warned: %s", f.class)
		}
		healthy[f.class] = true
	}
	for _, class := range []string{"egress-prober-fault", "egress-backfill-sustained", "egress-site-pool-unobservable"} {
		if !healthy[class] {
			t.Fatalf("complete evidence cannot resolve %s", class)
		}
	}
}

func TestEgressSitePoolCoverageTicketRequiresCompleteRecovery(t *testing.T) {
	manager := newTicketManager("synthetic", &ticketEscalationEmitter{})
	manager.resolveTicks = 3
	bad := healthyEgressSitePoolFixture()
	bad.guardTrips["full"] = 2
	for range 2 {
		manager.ingest(context.Background(), sitePoolCoverageTestFindings(t, bad, nil, nil))
	}
	if manager.openCount() != 1 {
		t.Fatal("synthetic guard incident did not open")
	}
	missing := healthyEgressSitePoolFixture()
	missing.mimirDown = true
	for range manager.resolveTicks {
		manager.ingest(context.Background(), sitePoolCoverageTestFindings(t, missing, nil, nil))
	}
	guardOpen := false
	for _, ticket := range manager.tickets {
		guardOpen = guardOpen || ticket.open && ticket.class == "egress-prober-fault"
	}
	if !guardOpen {
		t.Fatal("Mimir outage resolved the guard incident")
	}
	for range manager.resolveTicks {
		manager.ingest(context.Background(), sitePoolCoverageTestFindings(t, healthyEgressSitePoolFixture(), nil, nil))
	}
	if manager.openCount() != 0 {
		t.Fatal("complete paired recovery did not close incidents")
	}
}

func TestEgressSitePoolCoverageMalformedDuplicateAndPrivacy(t *testing.T) {
	for _, bad := range []string{"NaN", "-1", "duplicate", "foreign"} {
		findings := sitePoolCoverageTestFindings(t, healthyEgressSitePoolFixture(), func(rows []sitePoolCoverageTestSeries) []sitePoolCoverageTestSeries {
			if bad == "duplicate" {
				return append(rows, rows[0])
			}
			if bad == "foreign" {
				rows[0].Metric["rank_mode"] = "secret.example/private"
			} else {
				rows[0].Value[1] = bad
			}
			return rows
		}, nil)
		requireSitePoolCoverageUnknown(t, findings, "egress-backfill-sustained", "egress-prober-fault")
		for _, f := range findings {
			if strings.Contains(f.observed+f.context+f.evidence+f.action, "secret.example") {
				t.Fatal("raw response label escaped")
			}
		}
	}
}

func TestEgressSitePoolCoverageQueryRetainsAuthority(t *testing.T) {
	f := healthyEgressSitePoolFixture()
	source := f.source(t)
	shell := source.hostFn
	seen := false
	source.hostFn = func(host HostSettings, command string) (string, error) {
		seen = true
		for _, part := range []string{"monitor_egress_sample", "process_start_time_seconds", "timestamp(", "count_over_time(", "resets(", " offset ", "--max-time 15", "--max-filesize", "time="} {
			if !strings.Contains(command, part) {
				t.Errorf("bounded query omits %q", part)
			}
		}
		return shell(host, command)
	}
	_, err := testEgressSitePoolSignal(testEgressSitePoolContext()).Run(context.Background(), testEgressSitePoolSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("synthetic source was not invoked")
	}
}

// A canceled read must not emit a replacement healthy result.
func TestEgressSitePoolCoverageCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	source := healthyEgressSitePoolFixture().source(t)
	source.hostFn = func(HostSettings, string) (string, error) { return "", context.Canceled }
	_, err := testEgressSitePoolSignal(testEgressSitePoolContext()).Run(ctx, testEgressSitePoolSettings(source))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

// A too-small output budget must never parse a truncated response as zero.
func TestEgressSitePoolCoverageBodyBound(t *testing.T) {
	f := healthyEgressSitePoolFixture()
	signal := testEgressSitePoolSignal(testEgressSitePoolContext()).(*signalAdapter)
	settings := DefaultEgressSitePoolSettings()
	settings.MaxResponseBytes = 1
	signal.probe = egressSitePoolProbe{settings: settings, loadPoolSettings: func() (*egressSitePoolContext, error) { return testEgressSitePoolContext(), nil }}
	alerts, err := signal.Run(context.Background(), testEgressSitePoolSettings(f.source(t)))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "egress-site-pool-unobservable")
}

func TestEgressSitePoolCoverageGenerationAndClockControls(t *testing.T) {
	for _, change := range []string{"restart", "future", "decrease", "missing-presence"} {
		findings := sitePoolCoverageTestFindings(t, healthyEgressSitePoolFixture(), func(rows []sitePoolCoverageTestSeries) []sitePoolCoverageTestSeries {
			out := []sitePoolCoverageTestSeries{}
			for _, row := range rows {
				if row.Metric["host"] == "worker-a.example" {
					key := row.Metric["monitor_egress_sample"]
					if change == "missing-presence" && key == "present" {
						continue
					}
					if change == "restart" && key == "now/start" || change == "future" && key == "now/start/time" {
						row.Value[1] = strconv.FormatInt(syntheticSettings(nil).Now().Unix()+60, 10)
					}
					if change == "decrease" && key == "now/guard-full" {
						row.Value[1] = "1"
					}
				}
				out = append(out, row)
			}
			return out
		}, nil)
		requireSitePoolCoverageUnknown(t, findings, "egress-prober-fault")
	}
}

func TestEgressSitePoolCoverageGuardZeroNeedsPassingControl(t *testing.T) {
	f := healthyEgressSitePoolFixture()
	f.classLoads = [][]string{{"cdn", "1", "2"}, {"connectivity", "1", "2"}, {"dns", "1", "2"}, {"site", "1", "2"}}
	for _, finding := range sitePoolCoverageTestFindings(t, f, nil, nil) {
		if finding.class == "egress-prober-fault" && finding.healthy {
			t.Fatal("zero guard counters resolved an unmeasured database failure pattern")
		}
	}
}

func TestEgressSitePoolCoverageCatalog(t *testing.T) {
	raw, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	start := strings.Index(text, "### 2.19b")
	end := strings.Index(text, "### 2.19c")
	if start < 0 || end <= start {
		t.Fatal("missing owning catalog section")
	}
	section := strings.Join(strings.Fields(text[start:end]), " ")
	for _, phrase := range []string{"range-presence", "absence is not zero", "passing-class control", "partial positive", "not executable-artifact attestation"} {
		if !strings.Contains(section, phrase) {
			t.Errorf("coverage contract omits %q", phrase)
		}
	}
}
