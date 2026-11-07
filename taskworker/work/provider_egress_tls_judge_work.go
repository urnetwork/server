// The TLS finding judge (D1, model/provider_egress_tls_verdict.go): every few
// minutes, each unresolved exact-URL TLS finding is judged on evidence close in
// time to it, and only a finding judged the provider's own excludes the
// provider. A site or prober fault, a place where the site does not work, and
// a URL the pool has not served for a day are recorded as such and exclude
// nothing. A finding stays pending, excluding nothing, until a run judges it,
// so each judged run exports what it left behind and when it ran, and SIGNALS.md
// §2.31 alerts when no run has judged for a while or a finding stays undecided.
package work

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// How often the judge runs. Well inside the one-hour direct window, so a new
// finding is judged while a direct check is still close to it, and short
// beside the claim backoff, so a pending finding's capped recheck seldom runs
// before its first judgement.
const providerEgressTlsJudgeInterval = 5 * time.Minute

// The bound on one run: every direct check at the dial timeout, a few times
// over, beside reads and writes that take seconds.
const providerEgressTlsJudgeMaxTime = 10 * time.Minute

// The most findings one run reads, newest first. Production holds a few
// hundred; a fleet-wide site fault holds one per affected provider, and this
// keeps a run's memory and statement bounded even then.
const providerEgressTlsJudgeFindingLimit = 100000

// One direct check's dial and handshake bound, the pin observation's: a host
// that blackholes rather than refuses must not stall the run.
const providerEgressTlsDirectDialTimeout = 15 * time.Second

// Direct checks in flight at once, and the most hosts one run checks. Findings
// are on catalog hosts and their redirect hops, about a hundred and fifty at
// most; the bound keeps an unexpected spread of hop hosts from turning one run
// into a scan. A host left over is inconclusive for this run.
const (
	providerEgressTlsDirectConcurrency = 8
	providerEgressTlsDirectHostLimit   = 256
)

var providerEgressTlsJudgeRunsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_tls",
	Name:      "judge_runs_total",
	Help:      "TLS finding judge runs by result: judged, or pool_unavailable (the served pool could not be built, so no verdict changed)",
}, []string{"result"})

var providerEgressTlsDirectChecksTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_tls",
	Name:      "direct_checks_total",
	Help:      "Direct server-side TLS handshakes to the hosts of recent TLS findings, with no provider in the path, by outcome: verified, failed (certificate verification failed: a site or prober fault), inconclusive",
}, []string{"outcome"})

var providerEgressTlsVerdictsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_tls",
	Name:      "verdicts_total",
	Help:      "TLS findings the judge moved to each verdict: pending (including a retired finding whose URL is served again), provider (excludes the provider), site (site or prober fault), place (a national block: the URL's certificate fails across other networks in the provider's country), retired (the pool has not served the URL for a day)",
}, []string{"verdict"})

// What the newest judged run on this process left behind. The judge runs on
// one Taskworker at a time, so the fleet's view is the process whose
// last-judged timestamp is newest; the others keep what their own last run
// set.
var providerEgressTlsFindings = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_tls",
	Name:      "findings",
	Help:      "Unresolved exact-URL TLS findings by verdict after this process's newest judged run (urnetwork_egress_tls_judge_last_judged_timestamp_seconds): pending (not judged yet, excludes nobody), provider (excludes its provider), site (site or prober fault), place (a national block: the URL's certificate fails across other networks in the provider's country), retired (the pool has not served the URL for a day)",
}, []string{"verdict"})

var providerEgressTlsUndecidedFindings = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_tls",
	Name:      "undecided_findings",
	Help:      "Pending TLS findings this process's newest judged run left undecided although their failure is inside the one-hour direct window, so a direct check could still decide them: the check of their host was inconclusive or not made. They exclude nobody until decided",
})

var providerEgressTlsJudgeLastJudgedTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_tls",
	Name:      "judge_last_judged_timestamp_seconds",
	Help:      "Unix time of this process's newest TLS judge run that judged, 0 until one has. The fleet's newest judged run is the maximum over Taskworker processes; the findings and undecided gauges are that process's",
})

func init() {
	for _, result := range []string{"judged", "pool_unavailable"} {
		providerEgressTlsJudgeRunsTotal.WithLabelValues(result)
	}
	for _, outcome := range []model.ProviderEgressTlsDirectOutcome{
		model.ProviderEgressTlsDirectVerified, model.ProviderEgressTlsDirectFailed, model.ProviderEgressTlsDirectInconclusive,
	} {
		providerEgressTlsDirectChecksTotal.WithLabelValues(outcome.String())
	}
	for _, verdict := range model.ProviderEgressTlsVerdicts {
		providerEgressTlsVerdictsTotal.WithLabelValues(verdict.String())
		providerEgressTlsFindings.WithLabelValues(verdict.String())
	}
	prometheus.MustRegister(
		providerEgressTlsJudgeRunsTotal,
		providerEgressTlsDirectChecksTotal,
		providerEgressTlsVerdictsTotal,
		providerEgressTlsFindings,
		providerEgressTlsUndecidedFindings,
		providerEgressTlsJudgeLastJudgedTimestamp,
	)
}

// The one thing a run carries to the next: when each finding's destination
// name was first read out of the served pool, on every run since
// (model.NewProviderEgressTlsPool). Everything else is read at run time.
type JudgeProviderEgressTlsFindingsArgs struct {
	UnservedSince map[string]time.Time `json:"unserved_since,omitempty"`
}

// What one run did; the same numbers are logged.
type JudgeProviderEgressTlsFindingsResult struct {
	// Skipped says why nothing was judged, empty for a run that judged.
	Skipped  string         `json:"skipped,omitempty"`
	Findings int            `json:"findings"`
	Changed  int            `json:"changed"`
	Verdicts map[string]int `json:"verdicts,omitempty"`
	// pending findings left undecided inside the direct window
	Undecided int `json:"undecided"`
	// the clock the next run carries; a skipped run carries its own on
	UnservedSince map[string]time.Time `json:"unserved_since,omitempty"`
}

// Schedules a run now, coalescing with a pending one, whose arguments, and so
// whose clock, it keeps.
func ScheduleJudgeProviderEgressTlsFindings(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleJudgeProviderEgressTlsFindingsAt(clientSession, tx, &JudgeProviderEgressTlsFindingsArgs{}, server.NowUtc())
}

func scheduleJudgeProviderEgressTlsFindingsAt(
	clientSession *session.ClientSession,
	tx server.PgTx,
	args *JudgeProviderEgressTlsFindingsArgs,
	runAt time.Time,
) {
	task.ScheduleTaskInTx(
		tx,
		JudgeProviderEgressTlsFindings,
		args,
		clientSession,
		task.RunOnce("judge_provider_egress_tls_findings"),
		task.RunAt(runAt),
		task.MaxTime(providerEgressTlsJudgeMaxTime),
	)
}

// One run of the judge.
func JudgeProviderEgressTlsFindings(
	args *JudgeProviderEgressTlsFindingsArgs,
	clientSession *session.ClientSession,
) (*JudgeProviderEgressTlsFindingsResult, error) {
	return judgeProviderEgressTlsFindings(clientSession.Ctx, server.NowUtc(), args, productionProviderEgressTlsJudgeSources())
}

// Schedules the next run one interval out, whatever this one did, carrying
// the clock on.
func JudgeProviderEgressTlsFindingsPost(
	_ *JudgeProviderEgressTlsFindingsArgs,
	result *JudgeProviderEgressTlsFindingsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	next := &JudgeProviderEgressTlsFindingsArgs{}
	if result != nil {
		next.UnservedSince = result.UnservedSince
	}
	scheduleJudgeProviderEgressTlsFindingsAt(clientSession, tx, next, server.NowUtc().Add(providerEgressTlsJudgeInterval))
	return nil
}

// What the judge reads beyond the findings, replaceable in tests.
type providerEgressTlsJudgeSources struct {
	// the served pool's destination names, general and every country's
	served func(context.Context) (map[string]bool, error)
	// the pool refresh's site settings, whose windows and shares the judge
	// reuses
	siteSettings func() (*model.ProviderEgressSiteSettings, error)
	// one direct, verified TLS handshake to a host
	direct func(context.Context, string) model.ProviderEgressTlsDirectOutcome
}

func productionProviderEgressTlsJudgeSources() *providerEgressTlsJudgeSources {
	return &providerEgressTlsJudgeSources{
		served:       servedProviderEgressDestinationNames,
		siteSettings: model.GetProviderEgressSiteSettings,
		direct:       observeProviderEgressTlsDirect,
	}
}

// The served pool's destination names: what the prober is offered now. A URL
// whose destination is not among them can no longer be drawn by any provider,
// so a finding on it is retired once that has held for a day.
func servedProviderEgressDestinationNames(ctx context.Context) (map[string]bool, error) {
	pool, err := controller.GetProviderEgressDestinationPool(ctx)
	if err != nil {
		return nil, err
	}
	served := map[string]bool{}
	for _, destination := range pool.Destinations {
		served[destination.Name] = true
	}
	for _, destinations := range pool.Countries {
		for _, destination := range destinations {
			served[destination.Name] = true
		}
	}
	return served, nil
}

// One direct handshake to host:443 from this host, verified against the
// system roots for the host's own name, with no provider in the path. It
// shares the prober's CA bundle and clock, so a verification failure here is
// the site's or the prober's, never a provider's. Verification failures are
// classified as the probe classifies them; anything else, a dial error, a
// timeout or a reset, is inconclusive. Like the pin observation, there is no
// way to supply a transport or a root pool.
//
// The host is a catalog URL's, or a redirect hop's that a site served over a
// verified session, and it is dialed only at a public address a probe target
// may have (egresshealth.PublicAddress): a name that resolves only inward is
// inconclusive, never a handshake from the taskworker into its own network.
func observeProviderEgressTlsDirect(ctx context.Context, host string) model.ProviderEgressTlsDirectOutcome {
	addr, ok := providerEgressTlsDirectAddress(ctx, host)
	if !ok {
		return model.ProviderEgressTlsDirectInconclusive
	}
	return observeProviderEgressTlsDirectAt(ctx, host, net.JoinHostPort(addr.String(), "443"))
}

// The first public address of host, an address literal or a resolved name.
func providerEgressTlsDirectAddress(ctx context.Context, host string) (netip.Addr, bool) {
	if host == "" {
		return netip.Addr{}, false
	}
	if literal, err := netip.ParseAddr(host); err == nil {
		return literal, egresshealth.PublicAddress(literal)
	}
	resolveCtx, cancel := context.WithTimeout(ctx, providerEgressTlsDirectDialTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(resolveCtx, "ip", host)
	if err != nil {
		return netip.Addr{}, false
	}
	for _, addr := range addrs {
		if addr = addr.Unmap(); egresshealth.PublicAddress(addr) {
			return addr, true
		}
	}
	return netip.Addr{}, false
}

// The same, dialing addr; tests point a host at a local listener.
func observeProviderEgressTlsDirectAt(ctx context.Context, host string, addr string) model.ProviderEgressTlsDirectOutcome {
	if host == "" {
		return model.ProviderEgressTlsDirectInconclusive
	}
	dialer := &tls.Dialer{
		NetDialer: server.NewDialer(providerEgressTlsDirectDialTimeout),
		Config: &tls.Config{
			ServerName:         host,
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: false,
		},
	}
	dialCtx, cancel := context.WithTimeout(ctx, providerEgressTlsDirectDialTimeout)
	defer cancel()
	conn, err := dialer.DialContext(dialCtx, "tcp", addr)
	if err != nil {
		if egresshealth.IsTlsAuthenticationFailure(err) {
			return model.ProviderEgressTlsDirectFailed
		}
		return model.ProviderEgressTlsDirectInconclusive
	}
	defer conn.Close()
	tlsConn, ok := conn.(*tls.Conn)
	if !ok || len(tlsConn.ConnectionState().VerifiedChains) == 0 {
		return model.ProviderEgressTlsDirectInconclusive
	}
	return model.ProviderEgressTlsDirectVerified
}

// Checks each host once, a bounded number at a time, in a stable order. A host
// past the limit, or left when the run's context ends, is inconclusive.
func checkProviderEgressTlsHosts(
	ctx context.Context,
	hosts []string,
	direct func(context.Context, string) model.ProviderEgressTlsDirectOutcome,
) map[string]model.ProviderEgressTlsDirectOutcome {
	sort.Strings(hosts)
	if providerEgressTlsDirectHostLimit < len(hosts) {
		hosts = hosts[:providerEgressTlsDirectHostLimit]
	}
	outcomes := map[string]model.ProviderEgressTlsDirectOutcome{}
	var outcomesLock sync.Mutex
	work := make(chan string)
	var wait sync.WaitGroup
	for range min(providerEgressTlsDirectConcurrency, len(hosts)) {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for host := range work {
				outcome := model.ProviderEgressTlsDirectInconclusive
				if ctx.Err() == nil {
					outcome = direct(ctx, host)
				}
				outcomesLock.Lock()
				outcomes[host] = outcome
				outcomesLock.Unlock()
			}
		}()
	}
	for _, host := range hosts {
		work <- host
	}
	close(work)
	wait.Wait()
	for _, outcome := range outcomes {
		providerEgressTlsDirectChecksTotal.WithLabelValues(outcome.String()).Inc()
	}
	return outcomes
}

// One run: read the findings, check the hosts of those it judges on evidence
// that are recent enough to judge, judge each finding, store the changed
// verdicts, export what the run left behind, and report. An unavailable pool
// changes nothing and carries the clock on unchanged: retiring every finding
// because the pool could not be built would be a guess, and so would keeping
// a stale served set.
func judgeProviderEgressTlsFindings(
	ctx context.Context,
	now time.Time,
	args *JudgeProviderEgressTlsFindingsArgs,
	sources *providerEgressTlsJudgeSources,
) (*JudgeProviderEgressTlsFindingsResult, error) {
	var previous map[string]time.Time
	if args != nil {
		previous = args.UnservedSince
	}
	served, err := sources.served(ctx)
	if err != nil {
		providerEgressTlsJudgeRunsTotal.WithLabelValues("pool_unavailable").Inc()
		glog.Errorf("[egress]TLS judge: the served pool is unavailable, so no finding was judged this run: %s\n", err)
		return &JudgeProviderEgressTlsFindingsResult{Skipped: "pool_unavailable", UnservedSince: previous}, nil
	}
	siteSettings, err := sources.siteSettings()
	if err != nil {
		siteSettings = model.DefaultProviderEgressSiteSettings()
	}
	settings := model.ProviderEgressTlsJudgeSettingsFor(siteSettings, providerUrlProbeCommonModeNetworkCount)

	findings := model.GetProviderEgressTlsFindings(ctx, providerEgressTlsJudgeFindingLimit)
	pool := model.NewProviderEgressTlsPool(now, findings, served, previous)
	direct := checkProviderEgressTlsHosts(ctx, model.ProviderEgressTlsDirectHosts(now, findings, pool, settings), sources.direct)

	judgements := model.JudgeProviderEgressTlsFindings(ctx, now, findings, pool, direct, settings)
	stored := model.SetProviderEgressTlsVerdicts(ctx, judgements)

	result := &JudgeProviderEgressTlsFindingsResult{
		Findings:      len(findings),
		Changed:       len(stored),
		Verdicts:      map[string]int{},
		UnservedSince: pool.UnservedSince,
	}
	storedSet := make(map[*model.ProviderEgressTlsJudgement]bool, len(stored))
	for _, judgement := range stored {
		storedSet[judgement] = true
	}
	for _, judgement := range judgements {
		verdict := judgement.Verdict
		if judgement.Changed() && !storedSet[judgement] {
			// a newer receipt won; the finding keeps what it had
			verdict = judgement.Finding.Verdict
		}
		result.Verdicts[verdict.String()] += 1
		if verdict == model.ProviderEgressTlsVerdictPending && settings.DirectCheckJudges(now, judgement.Finding) {
			result.Undecided += 1
		}
	}
	// A stored judgement that kept its verdict only recorded that it decided
	// a newer failure; the counts and the log are of verdicts that moved.
	moved := make([]*model.ProviderEgressTlsJudgement, 0, len(stored))
	for _, judgement := range stored {
		if judgement.Verdict != judgement.Finding.Verdict {
			moved = append(moved, judgement)
			providerEgressTlsVerdictsTotal.WithLabelValues(judgement.Verdict.String()).Inc()
		}
	}
	logProviderEgressTlsJudgements(moved)
	for _, verdict := range model.ProviderEgressTlsVerdicts {
		providerEgressTlsFindings.WithLabelValues(verdict.String()).Set(float64(result.Verdicts[verdict.String()]))
	}
	providerEgressTlsUndecidedFindings.Set(float64(result.Undecided))
	providerEgressTlsJudgeLastJudgedTimestamp.Set(float64(now.UnixMilli()) / 1000)
	providerEgressTlsJudgeRunsTotal.WithLabelValues("judged").Inc()
	glog.Infof(
		"[egress]TLS judge: findings=%d changed=%d hosts_checked=%d undecided=%d unserved_names=%d verdicts=%s\n",
		result.Findings, result.Changed, len(direct), result.Undecided, len(result.UnservedSince), providerEgressTlsVerdictSummary(result.Verdicts),
	)
	return result, nil
}

// The verdict counts as one stable log field.
func providerEgressTlsVerdictSummary(verdicts map[string]int) string {
	parts := []string{}
	for _, verdict := range model.ProviderEgressTlsVerdicts {
		parts = append(parts, fmt.Sprintf("%s:%d", verdict, verdicts[verdict.String()]))
	}
	return strings.Join(parts, ",")
}

// Logs what changed, one line per URL for a site or prober fault and per site
// and country for a place fault, with the evidence. Provider verdicts are
// counted in the summary and the metric; their providers are in the database,
// not the log.
func logProviderEgressTlsJudgements(moved []*model.ProviderEgressTlsJudgement) {
	type siteKey struct {
		urlKey string
	}
	type placeKey struct {
		name        string
		countryCode string
	}
	sites := map[siteKey][]*model.ProviderEgressTlsJudgement{}
	places := map[placeKey][]*model.ProviderEgressTlsJudgement{}
	retired := map[string]int{}
	returned := map[string]int{}
	for _, judgement := range moved {
		switch judgement.Verdict {
		case model.ProviderEgressTlsVerdictSite:
			key := siteKey{urlKey: judgement.Finding.UrlKey}
			sites[key] = append(sites[key], judgement)
		case model.ProviderEgressTlsVerdictPlace:
			key := placeKey{name: judgement.Finding.Name, countryCode: judgement.Finding.CountryCode}
			places[key] = append(places[key], judgement)
		case model.ProviderEgressTlsVerdictRetired:
			retired[judgement.Finding.Name] += 1
		case model.ProviderEgressTlsVerdictPending:
			if judgement.Finding.Verdict == model.ProviderEgressTlsVerdictRetired {
				returned[judgement.Finding.Name] += 1
			}
		}
	}
	for _, judgements := range sites {
		first := judgements[0]
		// alert class: a site's certificate, or the prober's CA bundle or
		// clock, fails TLS through many networks at once
		glog.Errorf(
			"[egress]TLS site or prober fault: destination=%s host=%s networks=%d direct=%s findings=%d; recorded as a site or prober fault, and no provider is excluded for it\n",
			first.Finding.Name, first.Finding.Host(), first.CommonModeNetworks, first.Direct, len(judgements),
		)
	}
	for key, judgements := range places {
		first := judgements[0]
		glog.Infof(
			"[egress]TLS place fault: destination=%s country=%s other_networks=%d other_failed=%d other_verified=%d country_loads=%d country_failures=%d findings=%d; the URL's certificate fails across this country's networks, and no provider is excluded for it\n",
			key.name, key.countryCode, first.PlaceNetworks, first.PlaceFailed, first.PlaceVerified, first.CountryLoads, first.CountryFailures, len(judgements),
		)
	}
	for name, count := range retired {
		glog.Infof("[egress]TLS findings retired: destination=%s findings=%d; the pool has not served it for a whole site refresh interval, so they exclude nothing and are not rechecked until it is served again\n", name, count)
	}
	for name, count := range returned {
		glog.Infof("[egress]TLS findings returned: destination=%s findings=%d; the pool serves it again, so they are pending and rechecked, and a provider that fails a recheck is judged again\n", name, count)
	}
}
