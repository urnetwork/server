// The active bandwidth probe without tunnels or a database.
package work

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe/bandwidth"
	"github.com/urnetwork/server/qualityprobe/ingest"
	"github.com/urnetwork/server/qualityprobe/providertunnel"
	"github.com/urnetwork/server/session"
)

// The probe's slot is the shard registry's second to last, apart from every
// URL shard the URL probe's settings accept and from the capability probe's
// last slot, so no two probes retire each other's shard owner.
func TestProviderBandwidthProbeShardSlotIsItsOwn(t *testing.T) {
	connect.AssertEqual(t, providerBandwidthProbeShardIndex, 254)
	connect.AssertEqual(t, providerBandwidthProbeShardCount, 256)
	connect.AssertEqual(t, providerBandwidthProbeShardIndex != capabilityProbeShardIndex, true)
	connect.AssertEqual(t, maxProviderUrlProbeShardCount, providerBandwidthProbeShardIndex)
	for _, c := range []struct {
		urlShardCount int
		available     bool
	}{
		{urlShardCount: defaultProviderEgressProbeShardCount, available: true},
		{urlShardCount: 8, available: true},
		{urlShardCount: maxProviderUrlProbeShardCount, available: true},
		{urlShardCount: providerBandwidthProbeShardIndex + 1, available: false},
		{urlShardCount: maxProviderEgressProbeShardCount, available: false},
	} {
		if err := providerBandwidthProbeShardAvailable(c.urlShardCount); (err == nil) != c.available {
			t.Errorf("url shard_count %d: available %v, want %t", c.urlShardCount, err, c.available)
		}
	}
}

// The probe is on by default and needs no settings of its own: the URL
// probe's settings file supplies its endpoints and the bandwidth keys it has
// always carried. full.bandwidth switches it off, full.bandwidth_timeout_seconds
// sets the per-target cap, bandwidth_cdn_url and public_api_url its targets,
// and a switched-off URL probe switches it off too.
func TestProviderBandwidthProbeSettingsComeFromTheUrlProbeSettings(t *testing.T) {
	egress := defaultProviderEgressProbeSettings("probe.example")
	settings := providerBandwidthProbeSettingsFrom(egress)
	if !settings.Enabled || settings.validate() != nil {
		t.Fatalf("default settings: enabled=%t validate=%v", settings.Enabled, settings.validate())
	}
	connect.AssertEqual(t, settings.SampleTimeout, bandwidth.DefaultTimeout)
	connect.AssertEqual(t, settings.ApiUrl, "https://api.probe.example")
	connect.AssertEqual(t, settings.PlatformUrl, "wss://connect.probe.example")
	connect.AssertEqual(t, settings.PublicApiUrl, "https://api.probe.example")
	connect.AssertEqual(t, settings.CdnUrl, bandwidth.CdnTestUrl)

	off := egress
	off.Full.Bandwidth = false
	connect.AssertEqual(t, providerBandwidthProbeSettingsFrom(off).Enabled, false)
	disabled := egress
	disabled.Enabled = false
	connect.AssertEqual(t, providerBandwidthProbeSettingsFrom(disabled).Enabled, false)
	slower := egress
	slower.Full.BandwidthTimeoutSeconds = 9
	connect.AssertEqual(t, providerBandwidthProbeSettingsFrom(slower).SampleTimeout, 9*time.Second)

	// a pass's max time that cannot hold one turn, or outlasts its interval,
	// is refused before any pass runs
	short := settings
	short.MaxTime = time.Minute
	if short.validate() == nil {
		t.Error("a max time shorter than one turn validated")
	}
	long := settings
	long.MaxTime = long.PassInterval + time.Minute
	if long.validate() == nil {
		t.Error("a max time past the pass interval validated")
	}
}

// A pass takes its share of the deployment's hourly budget: the budget's
// reservations an hour, over the passes an hour, over one reservation per
// target and provider; never under one provider, never over the cap. The
// default budget (40 an hour) with both targets and a pass every fifteen
// minutes samples five providers a pass, twenty an hour.
func TestProviderBandwidthProbePassLimitIsItsShareOfTheHourlyBudget(t *testing.T) {
	settings := defaultProviderBandwidthProbeSettings()
	for _, c := range []struct {
		probesPerHour int64
		targets       int
		limit         int
	}{
		{probesPerHour: 40, targets: 2, limit: 5},
		{probesPerHour: 40, targets: 1, limit: 10},
		{probesPerHour: 400, targets: 2, limit: settings.MaxLimit},
		{probesPerHour: 1, targets: 2, limit: 1},
		{probesPerHour: 0, targets: 2, limit: 0},
		{probesPerHour: 40, targets: 0, limit: 0},
	} {
		if got := providerBandwidthProbePassLimit(settings, c.probesPerHour, c.targets); got != c.limit {
			t.Errorf("budget %d/h with %d targets: limit %d, want %d", c.probesPerHour, c.targets, got, c.limit)
		}
	}
}

// Both targets with the operator secret and the public api url, the CDN alone
// without either, and the configured CDN url in place of the default. A
// tunnel's allowlist is the targets' hosts and nothing else.
func TestProviderBandwidthProbeTargets(t *testing.T) {
	settings := providerBandwidthProbeSettingsFrom(defaultProviderEgressProbeSettings("probe.example"))
	both := providerBandwidthProbeTargets(settings, "operator-secret")
	connect.AssertEqual(t, len(both), 2)
	connect.AssertEqual(t, both[0].Source, bandwidth.SourceOperator)
	connect.AssertEqual(t, both[0].Url, "https://api.probe.example/network/provider-bandwidth-test")
	connect.AssertEqual(t, both[1].Source, bandwidth.SourceCdn)
	connect.AssertEqual(t, bandwidth.TargetHosts(both), []string{"api.probe.example", "speed.cloudflare.com"})

	connect.AssertEqual(t, len(providerBandwidthProbeTargets(settings, "")), 1)
	noPublic := settings
	noPublic.PublicApiUrl = ""
	cdnOnly := providerBandwidthProbeTargets(noPublic, "operator-secret")
	connect.AssertEqual(t, len(cdnOnly), 1)
	connect.AssertEqual(t, cdnOnly[0].Source, bandwidth.SourceCdn)
	custom := settings
	custom.CdnUrl = "https://cdn.example/__down"
	connect.AssertEqual(t, providerBandwidthProbeTargets(custom, "")[0].Url, "https://cdn.example/__down")
}

// A bandwidth tunnel is an ordinary client's tunnel in the pass's own
// identity. Whatever the base it is built from carries, it never takes the
// URL probe's client traits -- the data-only admission without provider
// pings, unpublished client keys -- so a provider that recognizes a bandwidth
// tunnel by its fixed targets learns no trait that marks a URL tunnel. It has
// no pins, so its allowlist is the targets' hosts alone, and every tunnel a
// transport budget of its own.
func TestProviderBandwidthTunnelsCarryNoUrlProbeTrait(t *testing.T) {
	settings := defaultProviderBandwidthProbeSettings()
	clientId := connect.NewId()
	base := providertunnel.Config{
		ApiUrl:                       "https://api.probe.example",
		ByJwt:                        "shard-jwt",
		ClientId:                     clientId,
		DataOnlyProbe:                true,
		KeepClientKeysLocal:          true,
		Pins:                         map[string][]string{"www.example.com": {"pin"}},
		RedirectPins:                 map[string][]string{"www.example.com": {"pin"}},
		ContractReservationByteCount: 1024,
	}
	first := providerBandwidthTunnelConfig(base, settings)
	second := providerBandwidthTunnelConfig(base, settings)
	if first.DataOnlyProbe || first.KeepClientKeysLocal {
		t.Fatalf("a bandwidth tunnel carries a URL probe trait: data-only=%t keys-local=%t", first.DataOnlyProbe, first.KeepClientKeysLocal)
	}
	if first.Pins != nil || first.RedirectPins != nil || first.ContractReservationByteCount != 0 {
		t.Fatalf("a bandwidth tunnel kept the base's pins or contract ramp: %+v", first)
	}
	if first.ByJwt != "shard-jwt" || first.ClientId != clientId || first.ApiUrl != base.ApiUrl {
		t.Fatalf("a bandwidth tunnel left the pass's identity: %+v", first)
	}
	if first.PlatformTransportBudget == nil || first.PlatformTransportBudget == second.PlatformTransportBudget {
		t.Fatal("bandwidth tunnels share a platform transport budget")
	}
	connect.AssertEqual(t, first.CloseTimeout, settings.TunnelCloseTimeout)
}

// Serves the bandwidth targets from memory: every request answers 200 with
// the byte count it asked for.
type bandwidthTestTransport struct{}

type bandwidthTestZeroes struct{}

func (bandwidthTestZeroes) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func (bandwidthTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	byteCount, err := strconv.ParseInt(request.URL.Query().Get("bytes"), 10, 64)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode:    http.StatusOK,
		Body:          io.NopCloser(io.LimitReader(bandwidthTestZeroes{}, byteCount)),
		ContentLength: byteCount,
		Header:        http.Header{},
		Request:       request,
	}, nil
}

// The deployment's budget, as the operator answers it: reservations until the
// hour's are spent, then bandwidth.ErrNoBudget; and every submitted figure.
type bandwidthTestBudget struct {
	stateLock sync.Mutex
	remaining int
	reserved  []string
	submitted []string
}

func (self *bandwidthTestBudget) ReserveBandwidth(_ context.Context, providerClientId string, _ int64) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.remaining <= 0 {
		return fmt.Errorf("%w (provider %s)", bandwidth.ErrNoBudget, providerClientId)
	}
	self.remaining -= 1
	self.reserved = append(self.reserved, providerClientId)
	return nil
}

func (self *bandwidthTestBudget) SubmitBandwidth(_ context.Context, providerClientId string, _ string, _ float64, _ int64) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.submitted = append(self.submitted, providerClientId)
	return nil
}

// Records every tunnel the pass opens and closes; failOpen fails the open of
// the providers it names.
type bandwidthTestTunnels struct {
	stateLock sync.Mutex
	opened    []server.Id
	closed    int
	failOpen  map[server.Id]bool
}

func (self *bandwidthTestTunnels) open(_ context.Context, providerId server.Id) (*http.Client, func() error, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.opened = append(self.opened, providerId)
	if self.failOpen[providerId] {
		return nil, nil, errors.New("fixture tunnel refused")
	}
	return &http.Client{Transport: bandwidthTestTransport{}}, func() error {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.closed += 1
		return nil
	}, nil
}

func bandwidthTestProviders(count int) []server.Id {
	providers := make([]server.Id, count)
	for index := range providers {
		providers[index] = server.NewId()
	}
	return providers
}

// The budget governs, through the real sampler: every target is reserved
// before it is measured, a provider whose reservation is refused is the
// pass's last, and no further tunnel opens once the hour's budget is spent.
// Every opened tunnel closes, and the samples count by source and result.
func TestProviderBandwidthProbePassStopsAtTheFirstBudgetRefusal(t *testing.T) {
	providerBandwidthProbeSamplesTotal.Reset()
	settings := defaultProviderBandwidthProbeSettings()
	settings.Concurrency = 1
	providers := bandwidthTestProviders(5)
	budget := &bandwidthTestBudget{remaining: 3}
	sampler := &bandwidth.Sampler{Targets: []bandwidth.Target{bandwidth.CdnTarget()}, Reserve: budget, Submit: budget, Timeout: time.Second}
	tunnels := &bandwidthTestTunnels{}

	result := measureProviderBandwidthProbePass(context.Background(), settings, providers, 1, tunnels.open, sampler)

	if result.Started != 4 || result.Measured != 3 || !result.BudgetSpent || result.TunnelFailed != 0 {
		t.Fatalf("pass = %+v, want 4 started, 3 measured, the budget spent", result)
	}
	connect.AssertEqual(t, tunnels.opened, providers[:4])
	connect.AssertEqual(t, tunnels.closed, 4)
	connect.AssertEqual(t, budget.submitted, []string{providers[0].String(), providers[1].String(), providers[2].String()})
	connect.AssertEqual(t, budget.reserved, budget.submitted)
	connect.AssertEqual(t, testutil.ToFloat64(providerBandwidthProbeSamplesTotal.WithLabelValues(bandwidth.SourceCdn, "measured")), 3.0)
	connect.AssertEqual(t, testutil.ToFloat64(providerBandwidthProbeSamplesTotal.WithLabelValues(bandwidth.SourceCdn, "no_budget")), 1.0)
}

// A tunnel that does not open is counted and costs no budget, and the pass
// goes on to the next provider.
func TestProviderBandwidthProbePassGoesOnPastATunnelFailure(t *testing.T) {
	providerBandwidthProbeSamplesTotal.Reset()
	settings := defaultProviderBandwidthProbeSettings()
	settings.Concurrency = 1
	providers := bandwidthTestProviders(3)
	budget := &bandwidthTestBudget{remaining: 10}
	sampler := &bandwidth.Sampler{Targets: []bandwidth.Target{bandwidth.CdnTarget()}, Reserve: budget, Submit: budget, Timeout: time.Second}
	tunnels := &bandwidthTestTunnels{failOpen: map[server.Id]bool{providers[1]: true}}

	result := measureProviderBandwidthProbePass(context.Background(), settings, providers, 1, tunnels.open, sampler)

	if result.Started != 3 || result.Measured != 2 || result.TunnelFailed != 1 || result.BudgetSpent {
		t.Fatalf("pass = %+v, want 3 started, 2 measured, 1 tunnel failed", result)
	}
	connect.AssertEqual(t, budget.reserved, []string{providers[0].String(), providers[2].String()})
	connect.AssertEqual(t, tunnels.closed, 2)
	connect.AssertEqual(t, testutil.ToFloat64(providerBandwidthProbeSamplesTotal.WithLabelValues("tunnel", "failed")), 1.0)
}

// No turn starts that the pass's deadline cannot hold: a reservation spent on
// a measurement the deadline cuts short charges the fleet's budget for
// nothing.
func TestProviderBandwidthProbePassStartsNoTurnItCannotFinish(t *testing.T) {
	settings := defaultProviderBandwidthProbeSettings()
	ctx, cancel := context.WithTimeout(context.Background(), settings.turnBudget(1)-time.Second)
	defer cancel()
	budget := &bandwidthTestBudget{remaining: 10}
	sampler := &bandwidth.Sampler{Targets: []bandwidth.Target{bandwidth.CdnTarget()}, Reserve: budget, Submit: budget, Timeout: time.Second}
	tunnels := &bandwidthTestTunnels{}

	result := measureProviderBandwidthProbePass(ctx, settings, bandwidthTestProviders(2), 1, tunnels.open, sampler)

	if result.Started != 0 || len(tunnels.opened) != 0 || len(budget.reserved) != 0 {
		t.Fatalf("pass = %+v with %d tunnels and %d reservations, want nothing started", result, len(tunnels.opened), len(budget.reserved))
	}
}

// Replaces the probe's draw and Online judgement until the test ends.
func withProviderBandwidthProbeSelection(t *testing.T, sample func(context.Context, int, time.Time) []model.ProviderBandwidthProbeCandidate, eligible func(context.Context, []server.Id) []server.Id) {
	t.Helper()
	previousSample, previousEligible := getProviderBandwidthProbeSample, filterProviderBandwidthProbeEligible
	getProviderBandwidthProbeSample, filterProviderBandwidthProbeEligible = sample, eligible
	t.Cleanup(func() {
		getProviderBandwidthProbeSample, filterProviderBandwidthProbeEligible = previousSample, previousEligible
	})
}

// A pass draws DrawFactor providers per provider it may sample, keeps the
// due ones the Online judgement keeps, in the draw's order, and at most its
// limit; providers that are not due are never judged.
func TestProviderBandwidthProbeSelectionKeepsDueOnlineProvidersOfTheDraw(t *testing.T) {
	settings := defaultProviderBandwidthProbeSettings()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	drawn := bandwidthTestProviders(12)
	offline := map[server.Id]bool{drawn[2]: true, drawn[5]: true}
	var drawSize int
	var refreshBefore time.Time
	judged := []server.Id{}
	withProviderBandwidthProbeSelection(t,
		func(_ context.Context, size int, before time.Time) []model.ProviderBandwidthProbeCandidate {
			drawSize, refreshBefore = size, before
			candidates := []model.ProviderBandwidthProbeCandidate{}
			for index, clientId := range drawn {
				// every third provider has a fresh figure
				candidates = append(candidates, model.ProviderBandwidthProbeCandidate{ClientId: clientId, Due: index%3 != 1})
			}
			return candidates
		},
		func(_ context.Context, clientIds []server.Id) []server.Id {
			judged = append(judged, clientIds...)
			eligible := []server.Id{}
			for _, clientId := range clientIds {
				if !offline[clientId] {
					eligible = append(eligible, clientId)
				}
			}
			return eligible
		},
	)

	selection, err := selectProviderBandwidthProbePass(context.Background(), settings, 4, now)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, drawSize, 4*settings.DrawFactor)
	connect.AssertEqual(t, refreshBefore, now.Add(-settings.RefreshAge))
	connect.AssertEqual(t, selection.clientIds, []server.Id{drawn[0], drawn[3], drawn[6], drawn[8]})
	connect.AssertEqual(t, selection.drawn, 12)
	connect.AssertEqual(t, selection.due, 8)
	for _, clientId := range judged {
		if index := slices.Index(drawn, clientId); index%3 == 1 {
			t.Errorf("a provider with a fresh figure was judged: %s", clientId)
		}
	}
}

// A switched-off probe runs nothing and its chain ends; an enabled one
// schedules its next pass a pass interval on. Settings that cannot be read
// fail the pass for a retry and run nothing.
func TestProviderBandwidthProbeTaskFollowsItsSettings(t *testing.T) {
	previousSettings, previousExecute := getProviderBandwidthProbeSettings, executeProviderBandwidthProbe
	t.Cleanup(func() {
		getProviderBandwidthProbeSettings, executeProviderBandwidthProbe = previousSettings, previousExecute
	})
	ran := 0
	executeProviderBandwidthProbe = func(ctx context.Context, settings providerBandwidthProbeSettings, maxTime time.Duration) (*ProviderBandwidthProbeResult, error) {
		ran += 1
		if deadline, ok := ctx.Deadline(); !ok || maxTime < time.Until(deadline) {
			t.Errorf("the pass runs without its max time %s as its deadline", maxTime)
		}
		return &ProviderBandwidthProbeResult{Started: 1, Measured: 1}, nil
	}
	clientSession := session.NewLocalClientSession(context.Background(), "0.0.0.0:0", nil)
	defer clientSession.Cancel()

	disabled := defaultProviderBandwidthProbeSettings()
	disabled.Enabled = false
	getProviderBandwidthProbeSettings = func() (providerBandwidthProbeSettings, error) { return disabled, nil }
	result, err := ProviderBandwidthProbe(&ProviderBandwidthProbeArgs{}, clientSession)
	if err != nil || !result.Disabled || ran != 0 {
		t.Fatalf("a switched-off probe: %+v, %v, ran %d", result, err, ran)
	}
	if _, scheduled := providerBandwidthProbeSuccessor(result, disabled, time.Now()); scheduled {
		t.Fatal("a switched-off probe's pass scheduled a successor")
	}

	enabled := providerBandwidthProbeSettingsFrom(defaultProviderEgressProbeSettings("probe.example"))
	getProviderBandwidthProbeSettings = func() (providerBandwidthProbeSettings, error) { return enabled, nil }
	result, err = ProviderBandwidthProbe(&ProviderBandwidthProbeArgs{MaxTimeSeconds: 300}, clientSession)
	if err != nil || result.Disabled || ran != 1 {
		t.Fatalf("an enabled probe: %+v, %v, ran %d", result, err, ran)
	}
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	if runAt, scheduled := providerBandwidthProbeSuccessor(result, enabled, now); !scheduled || !runAt.Equal(now.Add(enabled.PassInterval)) {
		t.Fatalf("next pass at %s (scheduled %t), want %s", runAt, scheduled, now.Add(enabled.PassInterval))
	}

	getProviderBandwidthProbeSettings = func() (providerBandwidthProbeSettings, error) {
		return providerBandwidthProbeSettings{}, errors.New("fixture settings unreadable")
	}
	if _, err := ProviderBandwidthProbe(&ProviderBandwidthProbeArgs{}, clientSession); err == nil || ran != 1 {
		t.Fatalf("unreadable settings: %v, ran %d", err, ran)
	}
}

// The URL probe's pass is sampled-only: it attaches no bandwidth sampler and
// no fixed target host to its tunnels, whatever the deprecated full.bandwidth
// key says; only this probe's own shard samples bandwidth.
func TestProviderUrlProbePassCarriesNoBandwidthSampler(t *testing.T) {
	settings := defaultProviderEgressProbeSettings("example.test")
	if !settings.Full.Bandwidth {
		t.Fatal("the fixture expects full.bandwidth on, as deployed")
	}
	args := providerEgressProbeArgs(settings, 0)
	operator := &ingest.Client{ServerUrl: "https://operator.example", OperatorSecret: "synthetic", ShardIndex: args.ShardIndex, ShardCount: args.ShardCount}
	pass := newProviderUrlProbePass(args, operator, providertunnel.Config{ApiUrl: "https://api.example"}, server.NewId())
	if pass.fullOptions.Bandwidth != nil || len(pass.fullOptions.BandwidthHosts) != 0 {
		t.Fatalf("the URL probe's pass attaches a bandwidth sampler (%v) or target hosts (%v)", pass.fullOptions.Bandwidth, pass.fullOptions.BandwidthHosts)
	}
}

// Each figure the probe submits is also observed by source and the provider's
// egress country, as a sum and a count; the figure reaches the operator
// unchanged.
func TestProviderBandwidthFiguresAreObservedByCountry(t *testing.T) {
	egressProbeBandwidthCountryBytesPerSecond.Reset()
	inner := newFakeEgressProbeIngest()
	reporter := newEgressProbeMetricsReporter(inner, func(context.Context, string) string { return "VN" })
	clientId := server.NewId().String()
	for _, bytesPerSecond := range []float64{1e6, 3e6} {
		if err := reporter.SubmitBandwidth(context.Background(), clientId, bandwidth.SourceCdn, bytesPerSecond, 16*1024*1024); err != nil {
			t.Fatal(err)
		}
	}
	connect.AssertEqual(t, inner.bandwidth, []string{clientId, clientId})
	connect.AssertEqual(t, testutil.CollectAndCount(egressProbeBandwidthCountryBytesPerSecond), 1)
	count, sum := bandwidthTestSummary(t, bandwidth.SourceCdn, "vn")
	if count != 2 || sum != 4e6 {
		t.Fatalf("vn cdn figures count=%d sum=%v, want 2 and 4e6", count, sum)
	}
}

// The count and sum of one source and country's figures.
func bandwidthTestSummary(t *testing.T, source string, country string) (uint64, float64) {
	t.Helper()
	observer, err := egressProbeBandwidthCountryBytesPerSecond.GetMetricWithLabelValues(source, country)
	if err != nil {
		t.Fatal(err)
	}
	metric := &dto.Metric{}
	if err := observer.(prometheus.Metric).Write(metric); err != nil {
		t.Fatal(err)
	}
	return metric.GetSummary().GetSampleCount(), metric.GetSummary().GetSampleSum()
}
