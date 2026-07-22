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
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sync"
	"time"
)

// logClass is one row of the SIGNALS.md §4 taxonomy.
type logClass struct {
	name string
	re   *regexp.Regexp
	// per-minute rate above which the class is a finding; §4 healthy is ~0
	// for all classes, but transient blips (LOADING during a restart) are
	// tolerated by the higher thresholds
	rateThreshold int
	tier          string
	playbook      string
	meaning       string
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
		meaning: "the payout wallet balance cannot cover pending payouts (usdc) — fund the wallet or pause payouts; retries park AdvancePayment tasks until funded"},
	// server-side redis ttl guard (server/redis_ttl_warn.go): any command
	// carrying a raw time.Duration arg (serialized as nanoseconds) or an
	// effective ttl > 120 days logs this line — the 2026-07-20 stream-key
	// leak signature (EXPIRE <8h-in-ns> ≈ 913,000 years, ~1.1M orphaned keys)
	{name: "redis-ttl-suspect", re: regexp.MustCompile(`\[redis\]\[ttl\]`),
		rateThreshold: 1, tier: tierWarn, playbook: "SIGNALS.md §4",
		meaning: "a redis write carried a ttl beyond 120 days or a raw time.Duration arg — a unit conversion issue at the named command/key; find the write site and convert to seconds/ms"},
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
var novelNormalizeRes = []*regexp.Regexp{
	regexp.MustCompile(`[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}`),
	regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+(:[0-9]+)?`),
	regexp.MustCompile(`\b[0-9]+\b`),
}

const novelRateThreshold = 20

// targetRe extracts the ip:port a class line is about (the sick-node
// attribution from §4: identity is class + target + frame).
var targetRe = regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+:[0-9]+`)

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
	cmd.Wait()
	return scanErr
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

	for _, c := range logClasses {
		if c.re.MatchString(line) {
			self.classCounts[c.name] += 1
			if _, ok := self.classSamples[c.name]; !ok {
				self.classSamples[c.name] = truncateLine(line)
				if m := targetRe.FindString(line); m != "" {
					self.classTargets[c.name] = m
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
		count := self.classCounts[c.name]
		attribution := self.classTargets[c.name]
		if count >= c.rateThreshold {
			findings = append(findings, finding{
				probeId: "logs/" + c.name, tier: c.tier,
				class: c.name, target: self.service, frame: attribution, sustain: 1,
				symptom:  fmt.Sprintf("service %s: %d/min lines of class %s (threshold %d/min)", self.service, count, c.name, c.rateThreshold),
				baseline: "healthy ~0/min for all classes; volume is retry amplification, not incident size (1.5)",
				observed: fmt.Sprintf("rate=%d/min class=%s target=%s", count, c.name, attribution),
				evidence: "meaning: " + c.meaning + "\nsample: " + self.classSamples[c.name],
				playbook: c.playbook,
			})
		} else {
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
	if novelTotal >= novelRateThreshold {
		// identity discipline: the top shape varies minute to minute, so it
		// must NOT be the frame — frame is part of ticket identity, and a
		// shifting frame resets the sustain-2 streak so the ticket would
		// never open. The shape lives in the evidence instead.
		findings = append(findings, finding{
			probeId: "logs/novel", tier: tierWarn,
			class: "novel", target: self.service, sustain: 2,
			symptom:  fmt.Sprintf("service %s: %d/min error-shaped lines matching no known class (top shape %d/min)", self.service, novelTotal, topCount),
			baseline: "unmatched error-shaped lines ~0/min; a new signature at rate = new failure mode (1.5)",
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

func (self *logTailProbe) id() string             { return "logs/tail" }
func (self *logTailProbe) tier() string           { return tierWarn }
func (self *logTailProbe) cadence() time.Duration { return 60 * time.Second }

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
