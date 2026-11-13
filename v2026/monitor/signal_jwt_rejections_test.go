// Deterministic public-reader controls for the missing signature metric arms.
// Metrics and process census are synthetic; no source contact is needed.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type jwtRejectionsTestProcess struct {
	service, host, block, instance string
	start, rate                    float64
}

type jwtRejectionsTestFixture struct {
	processes    []jwtRejectionsTestProcess
	rows, rates  []map[string]any
	census       map[string][]map[string]any
	warnings     []string
	rateWarnings []string
	queryCalls   int
	hostCalls    int
	queryErr     error
	censusErr    error
	onQuery      func()
	stateLock    sync.Mutex
}

// Every source point matches the real producer's two mandatory label groups
// and Grafana's common ingress clock. Other causes never enter the query.
func newJwtRejectionsTestFixture(processes ...jwtRejectionsTestProcess) *jwtRejectionsTestFixture {
	f := &jwtRejectionsTestFixture{processes: processes, census: map[string][]map[string]any{}}
	now := syntheticSettings(nil).Now()
	for i := range f.processes {
		p := &f.processes[i]
		if p.host == "" {
			p.host = "jwt-source"
		}
		if p.block == "" {
			p.block = "blue"
		}
		if p.instance == "" {
			p.instance = fmt.Sprintf("%08x", i+1)
		}
		if p.start == 0 {
			p.start = float64(now.Add(-time.Hour).Unix()) - float64(i)
		}
		labels := func(metric string) map[string]string {
			m := map[string]string{"env": "synthetic", "service": p.service, "job": p.service, "host": p.host, "block": p.block, "instance": p.instance}
			if metric != "rate" {
				m["__name__"] = metric
			}
			if metric != jwtRejectionsStart {
				m["cause"] = "signature"
			}
			return m
		}
		for _, metric := range []string{jwtRejectionsStart, jwtRejectionsCounter} {
			values := []any{}
			for sample := range 20 {
				value := p.start
				if metric == jwtRejectionsCounter {
					value = 10000 + float64(sample)*15*p.rate
				}
				values = append(values, []any{now.Add(time.Duration(sample-19) * 15 * time.Second).Unix(), fmt.Sprint(value)})
			}
			f.rows = append(f.rows, map[string]any{"metric": labels(metric), "values": values})
		}
		f.rates = append(f.rates, map[string]any{"metric": labels("rate"), "value": []any{now.Unix(), fmt.Sprint(p.rate)}})
		f.census[p.host] = append(f.census[p.host], map[string]any{"service": p.service, "block": p.block, "start": p.start})
	}
	return f
}

// Use the actual native HTTP and host census seams, with source requests
// asserted independently of response generation and without a fallback path.
func (self *jwtRejectionsTestFixture) settings(t *testing.T) SignalSettings {
	t.Helper()
	marshal := func(value any) string {
		b, err := json.Marshal(value)
		if err != nil {
			t.Fatal("cannot encode deterministic JWT fixture")
		}
		return string(b)
	}
	settings := syntheticSettings(nil)
	settings.Hosts = nil
	settings.LogServiceHosts, settings.LogServiceBlocks = map[string][]string{}, map[string][]string{}
	services, hosts := map[string]bool{}, map[string]bool{}
	appendOnce := func(values []string, value string) []string {
		for _, existing := range values {
			if existing == value {
				return values
			}
		}
		return append(values, value)
	}
	for _, p := range self.processes {
		services[p.service], hosts[p.host] = true, true
		settings.LogServiceHosts[p.service] = appendOnce(settings.LogServiceHosts[p.service], p.host)
		settings.LogServiceBlocks[p.service] = appendOnce(settings.LogServiceBlocks[p.service], p.block)
	}
	for service := range services {
		settings.LogServices = append(settings.LogServices, service)
	}
	sort.Strings(settings.LogServices)
	for host := range hosts {
		settings.Hosts = append(settings.Hosts, HostSettings{Name: host, Roles: []string{"services"}})
	}
	sort.Slice(settings.Hosts, func(i, j int) bool { return settings.Hosts[i].Name < settings.Hosts[j].Name })
	settings.Source = &providerHttpsTestSource{
		syntheticSource: &syntheticSource{hostTimeoutFn: func(host HostSettings, command string, timeout time.Duration) (string, error) {
			self.stateLock.Lock()
			self.hostCalls++
			self.stateLock.Unlock()
			if timeout != jwtRejectionsCensusTimeout || !strings.HasPrefix(command, "timeout 14s python3 -c ") || !strings.Contains(command, "warp.service") || !strings.Contains(command, "/stat") {
				t.Fatal("JWT census lost its bounded owning source")
			}
			if self.censusErr != nil {
				return "", self.censusErr
			}
			short, _, _ := strings.Cut(host.Name, ".")
			return marshal(map[string]any{"schema": 1, "host": short, "at": settings.Now().Unix(), "processes": self.census[host.Name]}), nil
		}},
		do: func(request *http.Request) (*http.Response, error) {
			self.queryCalls++
			if self.onQuery != nil {
				self.onQuery()
			}
			if self.queryErr != nil {
				return nil, self.queryErr
			}
			user, password, ok := request.BasicAuth()
			if !ok || user != "query-synthetic" || password != "private-query-password" || request.Method != http.MethodPost || request.URL.String() != "https://synthetic-grafana.metrics.example/prometheus/api/v1/query" || request.ParseForm() != nil || len(request.PostForm) != 2 || request.PostForm.Get("time") != fmt.Sprint(settings.Now().Unix()) {
				t.Fatal("JWT query lost its bounded authenticated fixed-time contract")
			}
			query := request.PostForm.Get("query")
			if !strings.Contains(query, jwtRejectionsCounter) || !strings.Contains(query, "[5m]") || strings.Contains(query, "or vector(0)") {
				t.Fatal("JWT query changed its source window or manufactured zero")
			}
			kind, rows, warnings := "matrix", self.rows, self.warnings
			if strings.HasPrefix(query, "rate(") {
				if !strings.Contains(query, `cause="signature"`) {
					t.Fatal("rate query included another rejection cause")
				}
				kind, rows, warnings = "vector", self.rates, self.rateWarnings
			} else if !strings.Contains(query, jwtRejectionsStart) {
				t.Fatal("raw query lost process source witnesses")
			}
			return providerHttpsTestResponse(marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": kind, "result": rows}, "warnings": warnings})), nil
		},
	}
	providerHttpsTestCredentials(t, &settings)
	return settings
}

func (self *jwtRejectionsTestFixture) run(t *testing.T) Alerts {
	t.Helper()
	alerts, err := NewJwtRejectionsSignal().Run(context.Background(), self.settings(t))
	if err != nil {
		t.Fatal("synthetic JWT public reader failed")
	}
	return alerts
}

// Strict boundary controls exercise both arms through production reader
// wiring; API-only 2/s is the documented misleading-but-healthy control.
func TestJwtRejectionsStrictThresholdsAndApiLoop(t *testing.T) {
	for _, test := range []struct {
		service string
		rate    float64
		breach  bool
	}{
		{service: "api", rate: 2, breach: false}, {service: "api", rate: 9.999, breach: false}, {service: "api", rate: 10, breach: false}, {service: "api", rate: 10.001, breach: true},
		{service: "connect", rate: .999, breach: false}, {service: "connect", rate: 1, breach: false}, {service: "connect", rate: 1.001, breach: true},
		{service: "alt", rate: 1.001, breach: true}, {service: "proxy", rate: 1.001, breach: true}, {service: "taskworker", rate: 2, breach: false},
	} {
		f := newJwtRejectionsTestFixture(jwtRejectionsTestProcess{service: test.service, rate: test.rate})
		alerts := f.run(t)
		if f.queryCalls != 2 || f.hostCalls != 1 {
			t.Fatal("reader changed its finite source budget")
		}
		if !test.breach {
			if len(alerts) != 0 {
				t.Fatal("a strict threshold boundary or API loop became a failure")
			}
			continue
		}
		alert := requireAlertClass(t, alerts, "jwt-signature-rejection-rate")
		if len(alerts) != 1 || alert.Severity != SeverityWarn || alert.Sustain != 1 {
			t.Fatal("breach policy or complete-source result changed")
		}
		for _, qualifier := range []string{"token parsing", "does not by itself", "False-positive qualifier", "False-negative qualifier", "strictly greater"} {
			if !strings.Contains(alert.Markdown(), qualifier) {
				t.Fatal("JWT finding lost its causal qualifier")
			}
		}
		requireAlertOmits(t, alert, "jwt-source", "00000001", "private-query-password")
	}
}

// Several individually low API processes can exceed the total arm. Concurrent
// complete old/new generations both count instead of choosing the newest one.
func TestJwtRejectionsTotalAndConcurrentGenerations(t *testing.T) {
	f := newJwtRejectionsTestFixture(
		jwtRejectionsTestProcess{service: "api", rate: 5.1},
		jwtRejectionsTestProcess{service: "api", rate: 5.1},
	)
	alerts := f.run(t)
	alert := requireAlertClass(t, alerts, "jwt-signature-rejection-rate")
	if len(alerts) != 1 || !strings.Contains(alert.Observed, "qualified_generations=2") || !strings.Contains(alert.Observed, "observed_signature_rate_per_s=10.200000") {
		t.Fatal("generation reduction lost an older complete publisher")
	}
}

// The total comparison is strict even when it is assembled from independent
// process values; input row order cannot alter that decision or its receipt.
func TestJwtRejectionsAggregateBoundaryAndOrder(t *testing.T) {
	for _, rate := range []float64{4.999, 5, 5.001} {
		f := newJwtRejectionsTestFixture(jwtRejectionsTestProcess{service: "api", rate: rate}, jwtRejectionsTestProcess{service: "api", rate: rate})
		alerts := f.run(t)
		if rate <= 5 {
			if len(alerts) != 0 {
				t.Fatal("aggregate equality or below-threshold control alerted")
			}
		} else {
			before := requireAlertClass(t, alerts, "jwt-signature-rejection-rate").Observed
			for i, j := 0, len(f.rows)-1; i < j; i, j = i+1, j-1 {
				f.rows[i], f.rows[j] = f.rows[j], f.rows[i]
			}
			after := requireAlertClass(t, f.run(t), "jwt-signature-rejection-rate").Observed
			if before != after {
				t.Fatal("source order changed deterministic reduction")
			}
		}
	}
}

// An independently positive sibling survives unavailable or ambiguous evidence;
// none of those inputs can certify recovery or be treated as a zero series.
func TestJwtRejectionsPartialControlsPreservePositive(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*jwtRejectionsTestFixture)
	}{
		{name: "lazy child", mutate: func(f *jwtRejectionsTestFixture) { f.rows = f.rows[:3] }},
		{name: "missing expected publisher", mutate: func(f *jwtRejectionsTestFixture) { f.rows = f.rows[:2]; f.rates = f.rates[:1] }},
		{name: "stopped old generation", mutate: func(f *jwtRejectionsTestFixture) { f.census["jwt-source"] = f.census["jwt-source"][:1] }},
		{name: "unpublished running generation", mutate: func(f *jwtRejectionsTestFixture) {
			f.census["jwt-source"] = append(f.census["jwt-source"], map[string]any{"service": "connect", "block": "blue", "start": float64(syntheticSettings(nil).Now().Add(-2 * time.Hour).Unix())})
		}},
		{name: "partial raw warning", mutate: func(f *jwtRejectionsTestFixture) { f.warnings = []string{"private-token-kid-error"} }},
		{name: "partial rate warning", mutate: func(f *jwtRejectionsTestFixture) { f.rateWarnings = []string{"private-token-kid-error"} }},
		{name: "duplicate counter", mutate: func(f *jwtRejectionsTestFixture) { f.rows = append(f.rows, f.rows[3]) }},
		{name: "duplicate rate", mutate: func(f *jwtRejectionsTestFixture) { f.rates = append(f.rates, f.rates[1]) }},
		{name: "reset", mutate: func(f *jwtRejectionsTestFixture) { f.rows[3]["values"].([]any)[10].([]any)[1] = "1" }},
		{name: "changed process start", mutate: func(f *jwtRejectionsTestFixture) { f.rows[2]["values"].([]any)[10].([]any)[1] = "1" }},
		{name: "warm replacement beside old failure", mutate: func(f *jwtRejectionsTestFixture) {
			start := float64(syntheticSettings(nil).Now().Add(-time.Minute).Unix())
			for _, point := range f.rows[2]["values"].([]any) {
				point.([]any)[1] = fmt.Sprint(start)
			}
			f.census["jwt-source"][1]["start"] = start
		}},
		{name: "missing rate", mutate: func(f *jwtRejectionsTestFixture) { f.rates = f.rates[:1] }},
		{name: "query witness disagreement", mutate: func(f *jwtRejectionsTestFixture) { f.rates[1]["value"].([]any)[1] = "99" }},
		{name: "source gap", mutate: func(f *jwtRejectionsTestFixture) {
			for _, i := range []int{2, 3} {
				values := f.rows[i]["values"].([]any)
				f.rows[i]["values"] = append(values[:5], values[9:]...)
			}
		}},
		{name: "stale source", mutate: func(f *jwtRejectionsTestFixture) {
			for _, i := range []int{2, 3} {
				f.rows[i]["values"] = f.rows[i]["values"].([]any)[:17]
			}
		}},
		{name: "nonfinite", mutate: func(f *jwtRejectionsTestFixture) { f.rows[3]["values"].([]any)[0].([]any)[1] = "NaN" }},
		{name: "unexpected private label", mutate: func(f *jwtRejectionsTestFixture) {
			f.rows[3]["metric"].(map[string]string)["token"] = "private-token-kid-error"
		}},
		{name: "census transport", mutate: func(f *jwtRejectionsTestFixture) { f.censusErr = errors.New("private-token-kid-error") }},
	} {
		t.Logf("case: %s", test.name)
		f := newJwtRejectionsTestFixture(jwtRejectionsTestProcess{service: "connect", rate: 2}, jwtRejectionsTestProcess{service: "connect", rate: 0})
		test.mutate(f)
		alerts := f.run(t)
		requireAlertClass(t, alerts, "jwt-signature-rejection-rate")
		unknown := requireAlertClass(t, alerts, "jwt-rejections-unobservable")
		if !unknown.unobservable {
			t.Fatalf("%s: partial evidence can clear a prior incident", test.name)
		}
		for _, alert := range alerts {
			requireAlertOmits(t, alert, "private-token-kid-error", "jwt-source", "00000001", "00000002")
		}
	}
}

// Source corruption, another cause and a duplicate publisher cannot inflate a
// rate. The uncertain source is not silently removed to make the result green.
func TestJwtRejectionsInvalidSingleProcessUnknown(t *testing.T) {
	for _, mode := range []string{"empty", "expired", "no_active_row", "credential_rotated", "duplicate publisher", "warm generation", "oversize history", "bad rate clock"} {
		f := newJwtRejectionsTestFixture(jwtRejectionsTestProcess{service: "connect", rate: 2})
		switch mode {
		case "empty":
			f.rows = nil
			f.rates = nil
		case "expired", "no_active_row", "credential_rotated":
			f.rows[1]["metric"].(map[string]string)["cause"] = mode
		case "duplicate publisher":
			copyFixture := newJwtRejectionsTestFixture(jwtRejectionsTestProcess{service: "connect", rate: 2, instance: "00000002", start: f.processes[0].start})
			f.rows = append(f.rows, copyFixture.rows...)
			f.rates = append(f.rates, copyFixture.rates...)
		case "warm generation":
			start := float64(syntheticSettings(nil).Now().Add(-time.Minute).Unix())
			for _, sample := range f.rows[0]["values"].([]any) {
				sample.([]any)[1] = fmt.Sprint(start)
			}
			f.census["jwt-source"][0]["start"] = start
		case "oversize history":
			f.rows[1]["values"] = make([]any, jwtRejectionsMaxSamples+1)
		case "bad rate clock":
			f.rates[0]["value"].([]any)[0] = syntheticSettings(nil).Now().Add(-time.Minute).Unix()
		}
		alerts := f.run(t)
		requireAlertClass(t, alerts, "jwt-rejections-unobservable")
		if len(alerts) != 1 {
			t.Fatal("unqualified single process fabricated a rate breach")
		}
	}
}

// Alt absent from the active version is optional. Temporary host exclusions
// preserve an unknown denominator and never issue a host or metric request
// for the excluded placement.
func TestJwtRejectionsOptionalAltAndExcludedHost(t *testing.T) {
	f := newJwtRejectionsTestFixture(jwtRejectionsTestProcess{service: "connect", rate: 0})
	if alerts := f.run(t); len(alerts) != 0 {
		t.Fatal("undeployed Alt became a required process")
	}
	settings := f.settings(t)
	settings.LogServices = append(settings.LogServices, "alt")
	settings.LogServiceHosts["alt"] = []string{}
	settings.LogServiceBlocks["alt"] = []string{}
	if alerts, err := NewJwtRejectionsSignal().Run(context.Background(), settings); err != nil || len(alerts) != 0 {
		t.Fatal("explicit empty Alt placement became required")
	}
	f = newJwtRejectionsTestFixture(jwtRejectionsTestProcess{service: "connect", rate: 0, host: "paused-edge5"})
	settings = f.settings(t)
	settings.ExcludedHosts = []string{"paused-edge5"}
	alerts, err := NewJwtRejectionsSignal().Run(context.Background(), settings)
	if err != nil || f.hostCalls != 0 || f.queryCalls != 0 {
		t.Fatal("excluded placement was contacted")
	}
	requireAlertClass(t, alerts, "jwt-rejections-unobservable")
}

// Inventory and body refusal bounds stop before unbounded work or a fabricated
// empty fleet. Oversized JSON is rejected before decoding process histories.
func TestJwtRejectionsSourceBounds(t *testing.T) {
	f := newJwtRejectionsTestFixture(jwtRejectionsTestProcess{service: "connect", rate: 0})
	settings := f.settings(t)
	settings.LogServiceBlocks["connect"] = make([]string, jwtRejectionsMaxSlots+1)
	for i := range settings.LogServiceBlocks["connect"] {
		settings.LogServiceBlocks["connect"][i] = fmt.Sprintf("block-%d", i)
	}
	alerts, err := NewJwtRejectionsSignal().Run(context.Background(), settings)
	if err != nil || f.hostCalls != 0 || f.queryCalls != 0 {
		t.Fatal("over-bound inventory started source work")
	}
	requireAlertClass(t, alerts, "jwt-rejections-unobservable")
	scope := jwtRejectionsScope{slots: map[jwtRejectionsSlot]bool{{service: "connect", host: "jwt-source", block: "blue"}: true}, complete: true}
	evidence := parseJwtRejections(strings.Repeat(" ", jwtRejectionsMaxBytes+1), "", "synthetic", syntheticSettings(nil).Now(), scope, jwtRejectionsCensus{})
	if evidence.complete || evidence.qualified != 0 || evidence.reason != "invalid-source-response" {
		t.Fatal("over-bound source became complete")
	}
}

// A canceled transport retains its typed source cause and schedules no second
// query. No token, key or native error text reaches a normal finding.
func TestJwtRejectionsCancellationAndErrorPrivacy(t *testing.T) {
	f := newJwtRejectionsTestFixture(jwtRejectionsTestProcess{service: "connect", rate: 0})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if alerts, err := NewJwtRejectionsSignal().Run(ctx, f.settings(t)); !errors.Is(err, context.Canceled) || len(alerts) != 0 || f.queryCalls != 0 || f.hostCalls != 0 {
		t.Fatal("pre-cancellation contacted a JWT source")
	}
	f = newJwtRejectionsTestFixture(jwtRejectionsTestProcess{service: "connect", rate: 0})
	ctx, cancel = context.WithCancel(context.Background())
	sourceErr := errors.New("private-native-token-key")
	f.onQuery = cancel
	f.queryErr = sourceErr
	alerts, err := NewJwtRejectionsSignal().Run(ctx, f.settings(t))
	if !errors.Is(err, context.Canceled) || !errors.Is(err, sourceErr) || len(alerts) != 0 || f.queryCalls != 1 {
		t.Fatal("mid-read cancellation lost source cause or started another read")
	}
	f = newJwtRejectionsTestFixture(jwtRejectionsTestProcess{service: "connect", rate: 0})
	f.queryErr = sourceErr
	alerts = f.run(t)
	requireAlertOmits(t, requireAlertClass(t, alerts, "jwt-rejections-unobservable"), "private-native-token-key")
}

// Recovery requires a complete same-cohort five-minute raw history and a
// current independent process census; replacing a process cannot clear it.
func TestJwtRejectionsRecoveryRequiresCompleteCohort(t *testing.T) {
	signal := NewJwtRejectionsSignal()
	for _, step := range []string{"breach", "lazy", "replacement", "healthy"} {
		f := newJwtRejectionsTestFixture(jwtRejectionsTestProcess{service: "connect", rate: 0})
		switch step {
		case "breach":
			f = newJwtRejectionsTestFixture(jwtRejectionsTestProcess{service: "connect", rate: 2})
		case "lazy":
			f.rows = f.rows[:1]
			f.rates = nil
		case "replacement":
			f.census["jwt-source"][0]["start"] = float64(syntheticSettings(nil).Now().Add(-time.Minute).Unix())
		}
		alerts, err := signal.Run(context.Background(), f.settings(t))
		if err != nil {
			t.Fatal("recovery fixture failed")
		}
		if step == "breach" {
			requireAlertClass(t, alerts, "jwt-signature-rejection-rate")
		} else if step == "healthy" {
			if len(alerts) != 0 {
				t.Fatal("complete quiet cohort did not recover")
			}
		} else {
			requireAlertClass(t, alerts, "jwt-rejections-unobservable")
		}
	}
}
