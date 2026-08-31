// Log tailers: the always-on collectors (design §3.7, SIGNALS.md 1.5). One
// long-running `warpctl logs <env> <service> -f` per service, each line
// classified against the §4 error taxonomy as it arrives. Per minute, each
// tailer folds its counts into findings — (class, target, frame) identity,
// rate, one sample line — through the same evaluator/ticket path as every
// other probe. Unmatched error-shaped lines at rate are reported as class
// `novel` (new panic frames and unseen failure modes are exactly what a fixed
// taxonomy misses).
//
// A tailer that exits or goes silent while its service is running restarts
// with backoff; repeated failure raises a monitor/visibility finding through
// the same findings channel.
package monitor

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// logClass is one row of the SIGNALS.md §4 taxonomy.
type logClass struct {
	name string
	re   *regexp.Regexp
	// groupBy splits one class into independently actionable frames. Most log
	// classes intentionally aggregate retry volume by service; route-bound
	// configuration defects must retain their resource, route, and generation.
	groupBy func(string) string
	// per-minute rate above which the class is a finding; §4 healthy is ~0
	// for all classes, but transient blips (LOADING during a restart) are
	// tolerated by the higher thresholds
	rateThreshold     int
	pageRateThreshold int
	tier              string
	playbook          string
	meaning           string
	mechanism         string
	context           string
	action            string
	verify            string
	// redactIDs removes UUID/server.Id values from the retained sample. Some
	// classes need a representative site/error but their entity identifiers
	// must not be copied into alert artifacts.
	redactIDs bool
	// metricOnly classes are still recognized so their rate-limited
	// exemplars do not become "novel" log errors, but alerting comes from a
	// lossless counter rather than the sampled log volume.
	metricOnly bool
}

// the §4 taxonomy. Order matters: first match wins.
var logClasses = []logClass{
	{name: "dial-io-timeout", re: regexp.MustCompile(`dial tcp ([0-9.]+:[0-9]+).*i/o timeout`),
		rateThreshold: 10, tier: tierPage, playbook: "SIGNALS.md 5.2",
		meaning: "node accept path starving — process alive but event loop wedged (or syn drop)"},
	{name: "connection-refused", re: regexp.MustCompile(`connect: connection refused`),
		rateThreshold: 10, tier: tierPage, playbook: "SIGNALS.md 5.2",
		meaning: "port closed: process dead or bound to wrong interface after manual restart"},
	{name: "port-exhaustion", re: regexp.MustCompile(`connect: cannot assign requested address`),
		rateThreshold: 100, tier: tierWarn, playbook: "SIGNALS.md 3.5",
		meaning: "client-side ephemeral-port exhaustion (redial storm to one destination); self-drains ~60s after the target is fixed"},
	{name: "pool-timeout", re: regexp.MustCompile(`redis: connection pool timeout`),
		rateThreshold: 10, tier: tierWarn, playbook: "SIGNALS.md §4",
		meaning: "local pool exhausted — backpressure, not the root; find what is slow consuming the pool"},
	{name: "clusterdown", re: regexp.MustCompile(`CLUSTERDOWN`),
		rateThreshold: 5, tier: tierPage, playbook: "SIGNALS.md 5.3",
		meaning: "slot coverage lost; transient during elections is expected and retried"},
	{name: "oom-writes", re: regexp.MustCompile(`OOM command not allowed`),
		rateThreshold: 1, tier: tierPage, playbook: "SIGNALS.md 5.4",
		meaning: "a node at maxmemory with nothing evictable — writes fail, reads work, cluster_state stays ok"},
	{name: "pubsub-drops", re: regexp.MustCompile(`channel is full for .* \(message is dropped\)`),
		rateThreshold: 10, tier: tierWarn, playbook: "SIGNALS.md 5.5",
		meaning: "in-process consumer stall: the app is not draining go-redis's channel"},
	{name: "conn-reset", re: regexp.MustCompile(`connection reset by peer|unexpected EOF`),
		rateThreshold: 50, tier: tierWarn, playbook: "SIGNALS.md §4",
		meaning: "server closed the conn (buffer-limit kill, maxmemory-clients eviction, restart); retried in-client"},
	{name: "redis-loading", re: regexp.MustCompile(`LOADING|READONLY`),
		rateThreshold: 50, tier: tierWarn, playbook: "SIGNALS.md §4",
		meaning: "node restarting (rdb load) / replica mid-failover; only sustained > 2 min matters"},
	{name: "required-vault-resource", re: regexp.MustCompile(`Resource not found in vault \([^\)]+\.yml\)`),
		rateThreshold: 1, tier: tierWarn, playbook: "SIGNALS.md 8.7",
		groupBy:   requiredVaultLogGroup,
		meaning:   "a route reached a lazily resolved vault file absent from the active config generation; gate an intentionally disabled subsystem before vault access, otherwise treat the missing enabled-subsystem config as a deployment blocker",
		mechanism: "Required vault resources are resolved lazily, so the process and /hello can stay green until one dependent request reaches the loader and returns 500. If the optional subsystem is disabled, the defect is an unguarded HTTP boundary; if it is enabled, the deployed vault generation is incomplete.",
		context:   "Branch on the subsystem feature state before changing configuration. A deliberately absent secret for a disabled subsystem is not evidence that a new secret should be fabricated. Resource, route, and active binary generation are retained as the alert frame because mixed generations can return different results.",
		action:    "When the subsystem is disabled, fail closed with the documented 503 and Retry-After before parsing, vault access, or database work, and stop its recurring task chain. When it is enabled, provision and validate the required resource through the supported vault mechanism before release. Do not invent or commit signing material merely to turn the 500 into a 200.",
		verify:    "For five minutes, every active generation emits zero missing-resource lines. An enabled route returns its documented success response; an intentionally disabled route returns its documented 503 without touching the resource. A green /hello alone is not verification.",
	},
	{name: "grafana-plugin-unregistered", re: regexp.MustCompile(`plugin\.notRegistered|plugin not registered`),
		rateThreshold: 1, tier: tierWarn, playbook: "SIGNALS.md 11.15",
		meaning:   "Grafana accepted the provisioned datasource but cannot load its datasource plugin; dashboards and every rule using that datasource fail even while Grafana and Mimir health endpoints stay green",
		mechanism: "Grafana 13 extracted the formerly core Prometheus datasource into a standalone native plugin. A provisioned warp-mimir row can still exist while an image without that plugin returns plugin.notRegistered for every dashboard query and alert evaluation.",
		context:   "A direct Mimir query, Grafana /api/health, or the datasource database row cannot exercise plugin loading. Correlate the sample's exact Grafana generation and image: a newer generation that independently fails provisioning must not be mistaken for evidence that the older serving generation has the plugin.",
		action:    "Publish a corrected Grafana image with the pinned Prometheus plugin and catalog SHA-256 for every supported architecture, together with any independently required provisioning fix. Run Warp's Prometheus plugin and provisioned-alert interval tests before release. Do not recreate the datasource, install plugins from the network at startup, silence the scheduler errors, or restart the same image.",
		verify:    "Query vector(1) through Grafana /api/ds/query on every active exact-edge generation, observe a successful provisioned-rule evaluation, and require zero new grafana-plugin-unregistered lines after log-ingestion delay.",
	},
	{name: "source-attribution", re: regexp.MustCompile(`X-UR-Forwarded-For .*was not one ip:port value|X-UR-Forwarded-For from untrusted peer`),
		rateThreshold: 1, tier: tierWarn, playbook: "SIGNALS.md 8.8",
		meaning: "the service rejected the trusted ingress source tuple and fell back to the proxy peer, collapsing unrelated users onto one rate-limit identity"},
	{name: "netescrow-negative", re: regexp.MustCompile(`\[netescrow\]negative counter after`),
		rateThreshold: 1, pageRateThreshold: netEscrowNegativePageRate, tier: tierWarn, playbook: "SIGNALS.md 5.11", redactIDs: true,
		groupBy:   netEscrowNegativeLogGroup,
		meaning:   "a settlement/release found fewer bytes in a Redis reservation mirror than PostgreSQL durably released; old binaries leave that negative value available until reconciliation, while a clamped_to=0 line means the current atomic release retained the diagnostic result and deleted the bad mirror in the same command",
		mechanism: "Two reconciliation paths can create this aftermath. A legacy full-fleet reconciler overwrites live mirror traffic with an old absolute SET or DEL snapshot. On the current page-local additive path, a PostgreSQL statement fixes its page snapshot before the reservation query runs; a live settlement can commit and update Redis while a slow page is still executing, so the later Redis GET sees the newer mirror and the correction re-adds bytes from the stale PostgreSQL snapshot. A separate smaller commit/post race occurs when reconciliation observes a committed settlement before its delayed Redis release. Current release Lua clamps a resulting negative atomically. This line is mutation-site aftermath, not evidence that the site independently created fleet-wide drift.",
		context:   "Correlate the line with the nearest ReconcileNetEscrow duration and aggregate correction, query taskworker, API, and Connect for the complete interval after allowing for log-ingestion delay, and retain whether clamped_to=0 was present. The rate is observed settlement/release exposure, not overwritten bytes or necessarily unique balances. Samples retain the non-sensitive site while redacting balance and contract ids.",
		action:    "Do not manually zero/delete individual mirrors or invoke reconciliation. Confirm the exact executor, reservation statement shape and timing, page-local additive semantics, and atomic release Lua. A legacy executor needs the additive path. A current executor with slow legacy-ANY or historical bounded-lateral pages needs migration 601 plus the unsettled-partial query to shrink the PostgreSQL-snapshot-to-Redis-GET window. If every residual line says clamped_to=0 and the matching aggregate stays small, observe the contained commit/post ordering window through a full quiet interval; if matched reversals persist on fast unsettled-partial pages, add durable per-balance fencing/versioning rather than redeploying the already-present fixes.",
		verify:    "After allowing for log-ingestion delay, require unsettled-partial pages below 1 second, one scheduled pass below 120 seconds, its aggregate correction below 256GiB and back in the ordinary tens-of-GiB band, and zero netescrow-negative lines from taskworker, API, and Connect for a full following interval. After rollout, any residual race line must say clamped_to=0 and its key must already be absent.",
	},
	{name: "panic", re: regexp.MustCompile(`panic:|Unexpected error|goroutine [0-9]+ \[`),
		rateThreshold: 5, tier: tierPage, playbook: "SIGNALS.md §4",
		meaning: "panic stack — the innermost app frame identifies the load-bearing call path"},
	// Contract errors are emitted here only as rate-limited exemplars. Their
	// lossless rates come from urnetwork_connect_contract_failures_total and
	// are evaluated by the provisioned Grafana rules; counting these sampled
	// lines would under-report the actual rate.
	{name: "insufficient-balance", re: regexp.MustCompile(`\[contract\]\[error\].*Insufficient balance`),
		rateThreshold: 4000, tier: tierWarn, playbook: "SIGNALS.md §4",
		meaning:    "payer has no usable balance — routine at a background rate for out-of-data users; a step-change = netEscrow drift re-emerging (contracts reconcile-net-escrow) or a balance-grant regression",
		metricOnly: true},
	{name: "missing-origin-contract", re: regexp.MustCompile(`Missing origin contract for companion`),
		rateThreshold: 500, tier: tierWarn, playbook: "SIGNALS.md §4",
		meaning:    "companion contract creation cannot find its origin contract — a spike = companion-path regression (origin closed early or client sequence bug)",
		metricOnly: true},
	// decoded from the taskworker novel class 2026-07-18: the payout wallet is
	// out of funds — a finance action, not an api bug
	{name: "payout-wallet-insufficient", re: regexp.MustCompile(`asset amount owned by the wallet is insufficient|insufficient token balance .* in wallet`),
		rateThreshold: 5, tier: tierWarn, playbook: "SIGNALS.md §4",
		meaning:   "the payout wallet balance cannot cover pending payouts (USDC); AdvancePayment remains pending until finance/ops restores liquidity or pauses payouts",
		mechanism: "The payment processor rejected a submit because the configured source wallet lacks enough token balance. Each affected AdvancePayment row remains pending and retries on the task system's consecutive-error backoff with a one-hour nominal cap. Current task code disperses saturated retries across 30–90 minutes with a one-hour mean; older code used only two seconds of jitter and preserved outage-created waves. N parked rows still produce roughly N retry lines per hour on average, so this rate measures retry amplification rather than unique payouts or transfer attempts that reached chain submission.",
		context:   "This is primarily an operational liquidity boundary, not an API or PostgreSQL defect. Proportional capped jitter contains synchronized processor bursts but cannot create wallet liquidity; accelerating retries only increases noise and load. A software release cannot fund the custodial wallet, and deleting task rows would discard owed payouts.",
		action:    "Finance/ops must fund the exact network/token payout wallet identified in protected source logs, or pause payouts using the supported operational control until it is funded. Do not delete or manually replay pending_task rows, rotate payment idempotency keys, or loosen the retry cap.",
		verify:    "Deploy the proportional-jitter taskworker, then verify a saturated cohort no longer repeats as a narrow hourly wave and processor-rate-limit remains bounded. After funding or an intentional resume, allow up to 90 minutes plus log-ingestion delay; AdvancePayment wallet-insufficient rows and this log rate converge to zero without manual row changes, while payment records show no duplicate Circle transfers.",
		redactIDs: true},
	// A durable transfer balance can intentionally span decades, but its Redis
	// escrow counter is a derived, reconciled mirror. The old creation path
	// copied the durable end time into EXPIREAT and therefore retained a cache
	// key for the balance's complete lifetime.
	{name: "redis-netescrow-ttl", re: regexp.MustCompile(`\[redis\]\[ttl\].*"expireat".*key="\{escrow_[^}]+\}net"`),
		rateThreshold: 1, tier: tierWarn, playbook: "SIGNALS.md §3.3a and §5.11", redactIDs: true,
		meaning:   "a net-escrow Redis mirror inherited a decades-long durable balance end time instead of a bounded rolling cache horizon",
		mechanism: "The transfer balance may legitimately remain valid for decades, but `{escrow_<balance-id>}net` is a derived reservation mirror rebuilt by reconciliation. Copying end_time plus slack into EXPIREAT retains the Redis key for the full durable lifetime and repeats the warning on every reservation.",
		context:   "This is independent of the legacy stream duration-as-seconds residue. Do not shorten the durable PostgreSQL balance or delete an active reservation mirror to silence it.",
		action:    "Roll out the net-escrow expiry cap that chooses the earlier of balance end_time plus 30 days and a rolling 90-day horizon. Let normal writes/reconciliation refresh active mirrors.",
		verify:    "New net-escrow writes have TTL at most 90 days, the durable long-lived balance remains unchanged, and no redis-netescrow-ttl line recurs.",
	},
	// server-side redis ttl guard (server/redis_ttl_warn.go): any other command
	// carrying a raw time.Duration arg (serialized as nanoseconds) or an
	// effective ttl > 120 days logs this line — the 2026-07-20 stream-key
	// leak signature (EXPIRE <8h-in-ns> ≈ 913,000 years, ~1.1M orphaned keys)
	{name: "redis-ttl-suspect", re: regexp.MustCompile(`\[redis\]\[ttl\]`),
		rateThreshold: 1, tier: tierWarn, playbook: "SIGNALS.md §4", redactIDs: true,
		meaning: "a redis write carried a ttl beyond its family limit or a raw time.Duration arg — inspect the named command/key to distinguish a unit conversion from an unbounded durable deadline"},
	// taskworker drain outcome (§12.1): the drain phases log exactly one
	// outcome line; "finished cleanly" / "finished after cancel" are healthy
	// and not classified — only "gave up" means a ctx-ignoring task rode to
	// SIGKILL and stuck leases follow (pg/task-lease-stranded confirms which)
	{name: "taskworker-drain-gave-up", re: regexp.MustCompile(`\[taskworker\]drain gave up`),
		rateThreshold: 1, tier: tierWarn, playbook: "SIGNALS.md 12.1",
		meaning: "a taskworker drain exceeded finish+cancel timeouts and the process was killed with tasks running — every in-flight claim is leased until its max time; check pg/task-lease-stranded and release stranded claims"},
	// e2e encryption (post-quantum) sessions — SIGNALS.md §15. The client-side
	// [tls]/[key] lines surface in server logs through the connect stacks the
	// server itself hosts (the proxy service's devices; providers run with the
	// e2e responder always on).
	{name: "tls-key-mitm", re: regexp.MustCompile(`CONTRACT vs FETCHED peer client public key MISMATCH`),
		rateThreshold: 1, tier: tierPage, playbook: "SIGNALS.md 15.2",
		meaning: "a session's contract-delivered peer identity key disagreed with the /key api — the platform is serving inconsistent keys (data bug) or something is substituting keys (possible MITM); compare the peer's client_tls_certificate/key rows against the contract path immediately"},
	{name: "tls-key-rotate-refused", re: regexp.MustCompile(`peer client public key mismatch with prior commitment`),
		rateThreshold: 5, tier: tierWarn, playbook: "SIGNALS.md 15.3",
		meaning: "a peer presented a different identity key mid-session (rotation refused by design) — occasional lines are reinstalls racing old sessions; a sustained rate = client identity bug or key churn upstream"},
	{name: "tls-cert-publish-invalid", re: regexp.MustCompile(`Invalid PEM in certificate chain|Invalid X\.509 certificate in chain`),
		rateThreshold: 5, tier: tierWarn, playbook: "SIGNALS.md 15.3",
		meaning: "EncryptedKey publications failing validation in SetEncryptedKey — a client build shipping malformed cert chains, or probing of the oob path; the error text carries the chain index"},
}

// errorShaped marks lines that count toward the novel class when no taxonomy
// row matches.
var errorShapedRe = regexp.MustCompile(`(?i)\berror\b|\bfatal\b|\bpanic\b|\bfail(ed|ure)\b`)

// novelNormalizeRes strip identifiers so distinct occurrences of one shape
// group together: hex ids, uuids, ips, ports, numbers.
var logIDRe = regexp.MustCompile(`[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}`)

var novelNormalizeRes = []*regexp.Regexp{
	logIDRe,
	regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+(:[0-9]+)?`),
	regexp.MustCompile(`\b[0-9]+\b`),
}

const novelRateThreshold = 20

// One negative mirror is an integrity defect. A hundred mutation exposures in
// one service/site minute is a fleet-scale availability/accounting incident,
// as opposed to the small irreducible cross-store race retained for diagnosis.
// The standing stream collector is required for this threshold: bounded log
// searches can truncate the exact burst that makes the severity actionable.
const netEscrowNegativePageRate = 100

// targetRe extracts the ip:port a class line is about (the sick-node
// attribution from §4: identity is class + target + frame).
var targetRe = regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+:[0-9]+`)

var requiredVaultResourceRe = regexp.MustCompile(`Resource not found in vault \(([^\)]+\.yml)\)`)
var requiredVaultRouteRe = regexp.MustCompile(`route ([A-Z]+) \^?([^$:\s]+)\$?:`)
var netEscrowNegativeSiteRe = regexp.MustCompile(`\[netescrow\]negative counter after ([a-z][a-z -]{0,40}):`)
var warpLogIdentityRe = regexp.MustCompile(`^\[([^\]]+)\]\[([^\]]+)\]\[([^\]]+)\]\[cid:([^\]]+)\]`)

type warpLogIdentity struct {
	host       string
	service    string
	generation string
	container  string
}

func parseWarpLogIdentity(line string) warpLogIdentity {
	match := warpLogIdentityRe.FindStringSubmatch(line)
	if len(match) < 5 {
		return warpLogIdentity{}
	}
	return warpLogIdentity{
		host:       match[1],
		service:    match[2],
		generation: match[3],
		container:  match[4],
	}
}

func requiredVaultLogGroup(line string) string {
	parts := []string{}
	if match := requiredVaultResourceRe.FindStringSubmatch(line); len(match) > 1 {
		parts = append(parts, "resource="+match[1])
	}
	if match := requiredVaultRouteRe.FindStringSubmatch(line); len(match) > 2 {
		parts = append(parts, "route="+match[1]+" "+match[2])
	}
	if identity := parseWarpLogIdentity(line); identity.generation != "" {
		parts = append(parts, "generation="+identity.generation)
	}
	return strings.Join(parts, " ")
}

// netEscrowNegativeLogGroup retains the non-sensitive mutation site as the
// structured alert frame. Without a group, the generic log target extractor
// looks for an ip:port and renders `target=` for these lines, even though
// `settle` versus `quarantine release` is the first discriminator an operator
// needs. Balance and contract ids remain confined to the redacted sample.
func netEscrowNegativeLogGroup(line string) string {
	match := netEscrowNegativeSiteRe.FindStringSubmatch(line)
	if len(match) < 2 {
		return ""
	}
	return "site=" + strings.TrimSpace(match[1])
}

// isGrafanaQueryEcho identifies query-engine metadata that repeats the query
// text verbatim. A standing search for an error signature therefore causes
// Grafana to log that same signature in an innocuous `executing query` line;
// classifying the metadata feeds the monitor's observation back into itself.
// Keep this service- and shape-specific so a real Grafana error carrying the
// same signature is still classified.
func isGrafanaQueryEcho(service string, line string) bool {
	if service != "grafana" || !strings.Contains(line, "level=info ") {
		return false
	}
	if !strings.Contains(line, "caller=engine.go:") &&
		!strings.Contains(line, "caller=roundtrip.go:") {
		return false
	}
	return (strings.Contains(line, `msg=\"executing query\"`) ||
		strings.Contains(line, `msg="executing query"`)) &&
		strings.Contains(line, " query=")
}

// logTailer tails one service's logs and aggregates per-minute class counts.
// Safe for one run goroutine plus concurrent snapshot calls.
type logTailer struct {
	service string
	env     *probeEnv

	stateLock sync.Mutex
	// class -> count in the current minute window
	classCounts map[string]int
	// class -> one sample line + one target from the window
	classSamples map[string]string
	classTargets map[string]string
	// normalized novel shape -> count
	novelCounts map[string]int
	novelSample string
	// tailer self-health (§3.7), read by the logTailProbe health findings
	lastLineTime   time.Time
	restartCount   int
	scanErrorCount int

	// stream is a test seam over runner.warpctlStream; nil = the real stream
	stream func(ctx context.Context) (*exec.Cmd, io.ReadCloser, error)
}

func newLogTailer(service string, env *probeEnv) *logTailer {
	return &logTailer{
		service:      service,
		env:          env,
		classCounts:  map[string]int{},
		classSamples: map[string]string{},
		classTargets: map[string]string{},
		novelCounts:  map[string]int{},
		// silence is measured from tailer start until the first line arrives
		lastLineTime: time.Now(),
	}
}

// run tails the service's logs until ctx is done, restarting the stream with
// backoff on exit. Started by the scheduler in main.
func (self *logTailer) run(ctx context.Context) {
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := self.tailOnce(ctx); err == nil {
			// a clean stream end (warpctl exited without a scan error):
			// restart promptly. Start failures and scan errors keep the
			// escalating backoff.
			backoff = time.Second
		}

		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.restartCount += 1
		}()

		// rate-limit restarts; the stream dying repeatedly surfaces via the
		// probe's tailer-restarting visibility finding
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// tailOnce runs one log stream to completion: start, scan lines, reap the
// child. On a scanner error (a > 1MB line overflows the buffer as
// bufio.ErrTooLong) the child is killed and the read end closed BEFORE Wait —
// otherwise the child keeps writing into a pipe nobody reads, blocks when the
// pipe fills, and Wait never returns (a silently dead tailer). An oversized
// line costs one counted stream restart, never a wedge.
func (self *logTailer) tailOnce(ctx context.Context) error {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd, stdout, err := self.openStream(streamCtx)
	if err != nil {
		return err
	}
	// the parent's read end of the pipe is owned here; without this close
	// every stream restart leaks one fd
	defer stdout.Close()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		self.classify(scanner.Text())
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		// the scan loop stopped consuming mid-stream: kill the child now so
		// Wait cannot block on the full pipe
		cancel()
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.scanErrorCount += 1
		}()
	}
	waitErr := cmd.Wait()
	if scanErr != nil {
		return scanErr
	}
	// A nonzero child exit is not a clean stream rotation. Preserve it so
	// run() keeps increasing its restart backoff instead of resetting every
	// failed Loki query to a one-second retry loop.
	return waitErr
}

// openStream starts the warpctl log stream (or the injected test stream).
func (self *logTailer) openStream(ctx context.Context) (*exec.Cmd, io.ReadCloser, error) {
	if self.stream != nil {
		return self.stream(ctx)
	}
	// --since=1s: without it, warpctl -f first replays a 5-minute
	// search window (up to 10k lines) before live-tailing, and the
	// replay lands in one minute window as a false rate spike (observed
	// 2026-07-19: a monitor restart during incident recovery opened
	// page-tier panic tickets from replayed restart-era lines)
	return self.env.runner.warpctlStream(ctx, "logs", self.env.cfg.env, self.service, "--since=1s", "-f")
}

// healthSnapshot returns the tailer's self-health counters (§3.7 visibility).
func (self *logTailer) healthSnapshot() (lastLine time.Time, restarts int, scanErrors int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.lastLineTime, self.restartCount, self.scanErrorCount
}

// classify folds one log line into the current window.
func (self *logTailer) classify(line string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lastLineTime = time.Now()
	if isGrafanaQueryEcho(self.service, line) {
		return
	}

	for _, c := range logClasses {
		if c.re.MatchString(line) {
			key := c.name
			attribution := ""
			if c.groupBy != nil {
				attribution = c.groupBy(line)
				if attribution != "" {
					key += "\x00" + attribution
				}
			}
			self.classCounts[key] += 1
			if _, ok := self.classSamples[key]; !ok {
				sample := line
				if c.redactIDs {
					sample = logIDRe.ReplaceAllString(sample, "<id>")
				}
				self.classSamples[key] = truncateLine(sample)
				if attribution != "" {
					self.classTargets[key] = attribution
				} else if target := targetRe.FindString(line); target != "" {
					self.classTargets[key] = target
				}
			}
			return
		}
	}
	if errorShapedRe.MatchString(line) {
		shape := line
		for _, re := range novelNormalizeRes {
			shape = re.ReplaceAllString(shape, "#")
		}
		if len(shape) > 160 {
			shape = shape[:160]
		}
		self.novelCounts[shape] += 1
		if self.novelSample == "" {
			self.novelSample = truncateLine(line)
		}
	}
}

// drainWindow returns findings for the window since the last call and resets
// the counters. Called once per minute by the tailer probe.
func (self *logTailer) drainWindow() []finding {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	// ticket identity discipline: the STABLE part (service) is the target and
	// the VARYING attribution (ip:port from the lines) is the frame. Healthy
	// resolution matches (probe, class, target) ignoring frame — with ip:port
	// as target, tickets opened during an incident could never match a later
	// healthy finding and lingered forever (61 zombie dial-io-timeout tickets
	// observed after the 2026-07-18 crash-loop outage).
	findings := []finding{}
	for _, c := range logClasses {
		if c.metricOnly {
			// The matching line is a sampled diagnostic exemplar, not a
			// cardinality-preserving signal. Keep old log-derived tickets
			// resolved and let Grafana evaluate the counter.
			findings = append(findings, healthyFinding("logs/"+c.name, c.tier, c.name, self.service))
			continue
		}
		keys := []string{c.name}
		if c.groupBy != nil {
			keys = keys[:0]
			prefix := c.name + "\x00"
			for key := range self.classCounts {
				if key == c.name || strings.HasPrefix(key, prefix) {
					keys = append(keys, key)
				}
			}
			sort.Strings(keys)
		}
		broken := false
		for _, key := range keys {
			count := self.classCounts[key]
			if count < c.rateThreshold {
				continue
			}
			broken = true
			tier := c.tier
			baseline := "healthy ~0/min for all classes; volume is retry amplification, not incident size (1.5)"
			observed := fmt.Sprintf("rate=%d/min class=%s", count, c.name)
			if c.pageRateThreshold > 0 {
				observed += fmt.Sprintf(" page_threshold=%d/min", c.pageRateThreshold)
				baseline += fmt.Sprintf("; page at >= %d/min", c.pageRateThreshold)
				if count >= c.pageRateThreshold {
					tier = tierPage
				}
			}
			attribution := self.classTargets[key]
			attributionLabel := "target"
			if c.groupBy != nil {
				attributionLabel = "frame"
			}
			observed += fmt.Sprintf(" %s=%s", attributionLabel, attribution)
			findings = append(findings, finding{
				probeId: "logs/" + c.name, tier: tier,
				class: c.name, target: self.service, frame: attribution, sustain: 1,
				symptom:   fmt.Sprintf("service %s: %d/min lines of class %s (threshold %d/min)", self.service, count, c.name, c.rateThreshold),
				baseline:  baseline,
				observed:  observed,
				mechanism: c.mechanism,
				evidence:  "meaning: " + c.meaning + "\nsample: " + self.classSamples[key],
				context:   c.context,
				action:    c.action,
				verify:    c.verify,
				playbook:  c.playbook,
			})
		}
		if !broken {
			findings = append(findings, healthyFinding("logs/"+c.name, c.tier, c.name, self.service))
		}
	}

	// novel shapes at rate
	novelTotal := 0
	topShape := ""
	topCount := 0
	for shape, count := range self.novelCounts {
		novelTotal += count
		if count > topCount {
			topShape, topCount = shape, count
		}
	}
	// A novel failure mode is one normalized signature repeating at rate.
	// Do not sum unrelated one-off shapes: public web services routinely see
	// scanner bursts across many nonexistent paths, and 20 different nginx
	// ENOENT lines are not 20 occurrences of one server defect.
	if topCount >= novelRateThreshold {
		// identity discipline: the top shape varies minute to minute, so it
		// must NOT be the frame — frame is part of ticket identity, and a
		// shifting frame resets the sustain-2 streak so the ticket would
		// never open. The shape lives in the evidence instead.
		findings = append(findings, finding{
			probeId: "logs/novel", tier: tierWarn,
			class: "novel", target: self.service, sustain: 2,
			symptom:  fmt.Sprintf("service %s: %d/min error-shaped lines matching no known class (top shape %d/min)", self.service, novelTotal, topCount),
			baseline: "each unmatched normalized error shape < 20/min; one new signature at rate = new failure mode (1.5)",
			observed: fmt.Sprintf("rate=%d/min distinct_shapes=%d", novelTotal, len(self.novelCounts)),
			evidence: "top shape: " + topShape + "\nsample: " + self.novelSample,
			playbook: "SIGNALS.md §4",
		})
	} else {
		findings = append(findings, healthyFinding("logs/novel", tierWarn, "novel", self.service))
	}

	// reset the window
	self.classCounts = map[string]int{}
	self.classSamples = map[string]string{}
	self.classTargets = map[string]string{}
	self.novelCounts = map[string]int{}
	self.novelSample = ""

	return findings
}

func truncateLine(line string) string {
	if len(line) > 200 {
		return line[:200]
	}
	return line
}

// logTailProbe adapts a set of tailers to the probe interface: each check
// drains every tailer's minute window and evaluates each tailer's own health
// (§3.7 promises a monitor/visibility finding for a tailer that cannot stay
// up). The tailers themselves run as standing goroutines started in main.
type logTailProbe struct {
	tailers []*logTailer
	// cadenceOverride is a deterministic scheduler-test seam. Production
	// always uses the one-minute default below.
	cadenceOverride time.Duration
	// restart counts at the previous check, per tailer, for the hot-restart
	// delta. Only the probe goroutine touches this (a probe never overlaps
	// itself).
	lastRestartCounts []int
}

// tailerSilentThreshold: no line for this long means the monitor is blind to
// that service's logs — the stream is dead, or the service itself is.
const tailerSilentThreshold = 10 * time.Minute

// tailerHotRestartThreshold: restarts within one check window (60s) at or
// above which the stream is flapping rather than recovering (e.g. a poison
// oversized line replayed at each reconnect, or warpctl itself failing).
const tailerHotRestartThreshold = 3

func (self *logTailProbe) id() string   { return "logs/tail" }
func (self *logTailProbe) tier() string { return tierWarn }
func (self *logTailProbe) cadence() time.Duration {
	if self.cadenceOverride > 0 {
		return self.cadenceOverride
	}
	return 60 * time.Second
}

func (self *logTailProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if self.lastRestartCounts == nil {
		self.lastRestartCounts = make([]int, len(self.tailers))
	}
	findings := []finding{}
	now := time.Now()
	for i, tailer := range self.tailers {
		findings = append(findings, tailer.drainWindow()...)

		lastLine, restarts, scanErrors := tailer.healthSnapshot()
		restartDelta := restarts - self.lastRestartCounts[i]
		self.lastRestartCounts[i] = restarts
		findings = append(findings, tailerHealthFindings(tailer.service, now, lastLine, restartDelta, scanErrors)...)
	}
	return findings, nil
}

// tailerHealthFindings evaluates one tailer's self-health: silent beyond the
// threshold, or restarting hot within one check window. Pure so the thresholds
// are unit-testable.
func tailerHealthFindings(service string, now time.Time, lastLine time.Time, restartDelta int, scanErrors int) []finding {
	findings := []finding{}
	target := "logs/" + service

	if silent := now.Sub(lastLine); silent >= tailerSilentThreshold {
		findings = append(findings, finding{
			probeId: "monitor/visibility", tier: tierWarn,
			class: "tailer-silent", target: target, sustain: 2,
			symptom:  fmt.Sprintf("log tailer for %s has read no line in %s (threshold %s)", service, silent.Round(time.Second), tailerSilentThreshold),
			baseline: "every tailed service logs continuously; a silent tailer means the monitor is blind to that service's logs (§3.7)",
			observed: fmt.Sprintf("silent_for=%s scan_errors_total=%d", silent.Round(time.Second), scanErrors),
			context:  "either the warpctl stream is broken (restart the monitor / check warpctl auth) or the service itself is down (warpctl ls versions)",
			playbook: "SIGNALS.md 1.5",
		})
	} else {
		findings = append(findings, healthyFinding("monitor/visibility", tierWarn, "tailer-silent", target))
	}

	if restartDelta >= tailerHotRestartThreshold {
		findings = append(findings, finding{
			probeId: "monitor/visibility", tier: tierWarn,
			class: "tailer-restarting", target: target, sustain: 2,
			symptom:  fmt.Sprintf("log tailer for %s restarted %d times in the last check window (threshold %d)", service, restartDelta, tailerHotRestartThreshold),
			baseline: "a healthy stream restarts rarely; hot restarts = the stream dies immediately after starting (oversized lines, warpctl failure)",
			observed: fmt.Sprintf("restart_delta=%d scan_errors_total=%d", restartDelta, scanErrors),
			playbook: "SIGNALS.md 1.5",
		})
	} else {
		findings = append(findings, healthyFinding("monitor/visibility", tierWarn, "tailer-restarting", target))
	}

	return findings
}
