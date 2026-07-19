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
	// contract-error classes measured at steady state 2026-07-17 (they ran as
	// `novel` on the tailers' first live pass): insufficient-balance ~1,000+/min
	// background from out-of-data free users — only a step-change matters (the
	// netEscrow drift signature); missing-origin ~90/min background
	{name: "insufficient-balance", re: regexp.MustCompile(`\[contract\]\[error\].*Insufficient balance`),
		rateThreshold: 4000, tier: tierWarn, playbook: "SIGNALS.md §4",
		meaning: "payer has no usable balance — routine at a background rate for out-of-data users; a step-change = netEscrow drift re-emerging (contracts reconcile-net-escrow) or a balance-grant regression"},
	{name: "missing-origin-contract", re: regexp.MustCompile(`Missing origin contract for companion`),
		rateThreshold: 500, tier: tierWarn, playbook: "SIGNALS.md §4",
		meaning: "companion contract creation cannot find its origin contract — a spike = companion-path regression (origin closed early or client sequence bug)"},
	// decoded from the taskworker novel class 2026-07-18: the payout wallet is
	// out of funds — a finance action, not an api bug
	{name: "payout-wallet-insufficient", re: regexp.MustCompile(`asset amount owned by the wallet is insufficient|insufficient token balance .* in wallet`),
		rateThreshold: 5, tier: tierWarn, playbook: "SIGNALS.md §4",
		meaning: "the payout wallet balance cannot cover pending payouts (usdc) — fund the wallet or pause payouts; retries park AdvancePayment tasks until funded"},
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
	// tailer self-health
	lastLineTime time.Time
	restartCount int
}

func newLogTailer(service string, env *probeEnv) *logTailer {
	return &logTailer{
		service:      service,
		env:          env,
		classCounts:  map[string]int{},
		classSamples: map[string]string{},
		classTargets: map[string]string{},
		novelCounts:  map[string]int{},
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

		streamCtx, cancel := context.WithCancel(ctx)
		cmd, stdout, err := self.env.runner.warpctlStream(streamCtx, "logs", self.env.cfg.env, self.service, "-f")
		if err == nil {
			scanner := bufio.NewScanner(stdout)
			scanner.Buffer(make([]byte, 64*1024), 1024*1024)
			for scanner.Scan() {
				self.classify(scanner.Text())
			}
			cmd.Wait()
			backoff = time.Second
		}
		cancel()

		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.restartCount += 1
		}()

		// rate-limit restarts; the stream dying repeatedly surfaces via the
		// probe's visibility finding
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
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
		findings = append(findings, finding{
			probeId: "logs/novel", tier: tierWarn,
			class: "novel", target: self.service, frame: topShape, sustain: 2,
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
// drains every tailer's minute window. The tailers themselves run as standing
// goroutines started in main.
type logTailProbe struct {
	tailers []*logTailer
}

func (self *logTailProbe) id() string             { return "logs/tail" }
func (self *logTailProbe) tier() string           { return tierWarn }
func (self *logTailProbe) cadence() time.Duration { return 60 * time.Second }

func (self *logTailProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	findings := []finding{}
	for _, tailer := range self.tailers {
		findings = append(findings, tailer.drainWindow()...)
	}
	return findings, nil
}
