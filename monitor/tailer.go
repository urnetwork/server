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
	"crypto/sha256"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// logBurst derives a short-window software-amplification finding from one
// existing log class. eventRe selects exactly one canonical line per logical
// attempt; exact-line fingerprints prevent a tail-stream replay from inflating
// the peak.
type logBurst struct {
	name      string
	eventRe   *regexp.Regexp
	threshold int
	tier      string
	meaning   string
	mechanism string
	context   string
	action    string
	verify    string
	playbook  string
}

// logCanonical counts one canonical line per logical provider event while the
// parent class still retains every diagnostic line. Some client failures are
// logged once at the provider boundary and again by the task evaluator; the
// distinction prevents a 2/min diagnostic rate from being read as two remote
// rejections.
type logCanonical struct {
	eventRe *regexp.Regexp
	name    string
}

// logClass is one row of the SIGNALS.md §4 taxonomy.
type logClass struct {
	name string
	re   *regexp.Regexp
	// sample optionally preserves a class-specific discriminator that generic
	// left truncation would hide. It receives an already-redacted line and must
	// return a bounded human-readable sample.
	sample func(string) string
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
	// observationOnly means this service emitted evidence about an internal
	// shared layer but is not necessarily the affected application selector.
	// Its alert symptom and observed fields must state that attribution limit.
	observationOnly bool
	// redactIDs removes UUID/server.Id values from the retained sample. Some
	// classes need a representative site/error but their entity identifiers
	// must not be copied into alert artifacts.
	redactIDs bool
	// metricOnly classes are still recognized so their rate-limited
	// exemplars do not become "novel" log errors, but alerting comes from a
	// lossless counter rather than the sampled log volume.
	metricOnly bool
	// burst is an independently actionable per-second finding derived from
	// this class's canonical event lines. It stays separate from the parent
	// alert so an operational cause (for example absent wallet liquidity) does
	// not conceal or inherit a deployable retry-amplification defect.
	burst *logBurst
	// canonical optionally exposes a de-duplicated logical event count next to
	// the raw class line rate. The line-rate threshold remains fail-safe when a
	// canonical evaluator line is absent from the observation stream.
	canonical *logCanonical
}

type logReconcileQuery func(ctx context.Context, start time.Time, blocks []string) (string, error)

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
	// Loki's tail querier uses the same message for an expected client-driven
	// context cancellation and for an internal backend transport loss. Match
	// only the unquoted EOF form: a watcher shutdown can produce a large burst
	// of quoted Canceled/context-canceled errors without losing a live stream.
	{name: "loki-tail-backend-eof", re: regexp.MustCompile(`caller=tail\.go:[0-9]+\s+component=tail-querier\b.*\bmsg="Error receiving response from grpc tail client"\s+err=EOF(?:\s|$)`),
		rateThreshold: 5, tier: tierWarn, playbook: "SIGNALS.md §1.5 and §4",
		meaning:   "Loki's external WebSocket tail remained available while its querier lost an internal gRPC tail backend with EOF; entries assigned to that backend can be absent from the live stream",
		mechanism: "The incident's exact 59-61-second recurrence came from Warp's Grafana ring TCP proxy applying a 60-second application read deadline to long-lived HTTP/2 and gRPC connections. A valid idle stream can carry no application bytes for longer than that, so the proxy closed a healthy backend connection and Loki reported EOF. Ring connection counts remained far below the independent 256-session ceiling.",
		context:   "A quoted rpc Canceled/context-canceled error during explicit watcher retirement is a separate client lifecycle and this class deliberately excludes it. If EOF recurs on an image already containing the fix, preserve its cadence and exact route before attributing it to the old deadline. Bounded log reconciliation remains required for late source timestamps and the Search-to-LiveTail handoff even after this transport fix.",
		action:    "Publish and deploy a Grafana image containing Warp commit 1e95aef, which removes TCP application read deadlines, enables 30-second TCP keepalives, retains bounded write deadlines, and leaves the UDP idle timeout intact. Do not raise Loki tail-request limits, raise the ring session cap, or restart the same image to hide EOFs.",
		verify:    "Every Grafana block runs an image containing Warp commit 1e95aef; with stable standing tails, no loki-tail-backend-eof line recurs for 10 minutes, the external tails remain connected, and each bounded two-minute reconciliation completes below its result cap.",
	},
	// This is the ingester-side queue, before the querier's independent HTTP
	// tail response queue. Loki 3.7.3 drops the accompanying DroppedStreams
	// field in pushTailResponseFromIngester, so the Grafana log is affirmative
	// internal loss but cannot name the affected external selector.
	{name: "loki-tail-dropped-streams", re: regexp.MustCompile(`caller=tailer\.go:[0-9]+\b.*\bmsg="tailer dropped streams is reset"(?:\s|$)`),
		rateThreshold: 1, tier: tierWarn, playbook: "SIGNALS.md §1.5 and §4", observationOnly: true,
		meaning:   "an ingester-side Loki live tail could not send streams through its bounded internal gRPC path, filled and reset its dropped-stream metadata list, and omitted records before they reached the querier",
		mechanism: "Loki 3.7.3 gives each ingester tailer a 100-stream processing queue and a five-stream send queue. A blocked internal gRPC send, including the observed 60-second Warp ring read-deadline EOF, or a producer burst drops streams. After ten retained drop descriptors the ingester emits this reset. Its querier pushTailResponseFromIngester path forwards resp.Stream but discards resp.DroppedStreams, so HTTP dropped_entries and Warpctl cannot expose or attribute this earlier loss. On 2026-08-31, one Proxy block emitted 19,995 per-peer installation lines in one minute while Grafana emitted 18,165 resets. An independent 23:53Z recurrence while v163 was the sole watcher paired 9,891 Fireside g10 sync lines with 5,470 resets in the same wall minute, versus 119 and 226 resets in the adjacent minutes. At 00:09Z on 2026-09-01, a service-wide query saturated at 20,000 records; block-partitioned Loki reads and direct host journals agreed on 39,378 default-info sync lines from Fireside and Crisp g1/g4/g5 while Grafana emitted 6,961 resets, versus 181 in the prior minute. Reconcile starts were aligned within 13 seconds and their preceding g1/g4 starts were one 30-minute interval earlier, pinning the producer to expected periodic full syncs from near-synchronous process starts rather than a deploy or watcher handoff.",
		context:   "This is affirmative internal live-tail loss even when the external WebSocket process remains connected. Grafana is the observation service that emitted the reset, not the affected application-tail identity. The standing range reconciliation across every service is the recovery path, but a capped service-wide query is not a total: repeat the same absolute window for each configured block and require every partition to drain. The Proxy burst was default info-log amplification, not distinct peer-installation failures. Do not jitter or disable the correctness reconciliation merely to hide its logs.",
		action:    "Deploy a Grafana image containing Warp commit 1e95aef to stop the ring TCP read deadline from tearing down valid idle gRPC tails, and deploy server Proxy commit e055c98c or later to remove per-client full-sync amplification. Retain bounded reconciliation continuation. Do not raise Loki's fixed queues, suppress the reset, or claim service attribution that Loki 3.7.3 does not preserve.",
		verify:    "Every Grafana block contains Warp commit 1e95aef; during the next full Proxy synchronization default logs contain one aggregate summary per reconciling instance, every reconciliation partition drains through bounded continuation, and loki-tail-dropped-streams remains zero for 10 minutes with all external tails connected.",
	},
	// This is a later loss boundary in the querier response channel. Unlike the
	// ingester reset above, Warpctl receives this metadata on the affected
	// service's WebSocket and can attribute it without retaining labels.
	{name: "loki-tail-dropped-entries", re: regexp.MustCompile(`^\[warpctl\]\[loki-tail-dropped-entries\]\s+service=[A-Za-z0-9._-]+\s+count=[1-9][0-9]*(?:\s|$)`),
		rateThreshold: 1, tier: tierWarn, playbook: "SIGNALS.md §1.5 and §4",
		meaning:   "Loki's querier-to-WebSocket response channel overflowed for this service's standing tail and the API returned a non-empty dropped_entries list",
		mechanism: "The Loki querier buffers ten tail responses. While that channel is full it drops entries and attaches up to 1,000 descriptors to a later successful WebSocket response. Older Warpctl decoded dropped_entries but silently discarded it; Warp commit 26089b2 emits one local service/count summary for every non-empty response.",
		context:   "The owning monitor tail supplies exact affected-service attribution. The summary is privacy-safe: Warpctl deliberately omits dropped stream labels and timestamps. This downstream signal is independent of the earlier ingester dropped-stream reset, which Loki 3.7.3 fails to propagate. Bounded range reconciliation remains the content-recovery path.",
		action:    "Run the monitor with Warpctl containing Warp commit 26089b2, retain bounded reconciliation, and remove the producer or consumer stall that blocked the named service tail. Do not print dropped labels, raise response queues, or suppress the summary.",
		verify:    "The named service tail stays connected, two consecutive overlap reconciliations complete, and no direct loki-tail-dropped-entries summary appears for 10 minutes through the workload that triggered the loss.",
	},
	// Mimir 3.1 logs any store-gateway bucket-index version behind the
	// querier's requested version as a warning. The live fleet's independent
	// jittered 15-minute loops produce an exact, harmless -873-second
	// one-generation gap. Match only gaps of 30 minutes or more so the expected
	// phase skew remains visible in metrics without becoming a log incident.
	{name: "mimir-bucket-index-lag", re: regexp.MustCompile(`caller=bucket\.go:[0-9]+\b.*\bdiff=-(?:1[89][0-9]{2}|[2-9][0-9]{3}|[1-9][0-9]{4,})\b.*\bmsg="bucket index version \(updated_at\) is older than requested"`),
		sample: mimirBucketIndexLagLogSample, groupBy: mimirBucketIndexLagLogGroup,
		rateThreshold: 1, tier: tierWarn, playbook: "SIGNALS.md §11.18",
		meaning:   "a Mimir store-gateway answered with a local bucket index at least 30 minutes older than the version requested by its querier",
		mechanism: "The querier sends its most recently discovered bucket-index update time to each store-gateway. A gateway compares that request metadata with its local index. Independent jittered 15-minute discovery loops can leave one gateway one generation behind; the production control's exact -873-second gap was that normal phase skew. A gap of at least 1,800 seconds crosses two nominal generations or one full additional sync interval and can indicate a missed gateway sync.",
		context:   "This warning is emitted after the gateway receives the RPC and is not by itself a failed query. Correlate the exact host with the mimir-index gateway freshness and tenant-coverage findings plus err-mimir-store-consistency-check-failed. Mimir 3.2 replaces this noisy 3.1 warning with a histogram, but a feature upgrade solely to hide the line is not a root-cause fix.",
		action:    "Run the §11.18 mimir-index probe, then inspect the framed host/generation for store-gateway sync, object-store, and ring errors. Restore periodic sync or tenant coverage. Do not suppress every bucket warning, increase max_stale_period, or restart all replicas together.",
		verify:    "For two 15-minute discovery cadences, every gateway's last successful sync remains under 30 minutes old, discovered tenants equal synced tenants, the shared bucket index remains under 35 minutes old, and no >=1,800-second warning or Mimir consistency error recurs.",
	},
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
	// net/http emits one WriteHeader diagnostic per invalid recovery attempt;
	// match that canonical first line rather than its paired body-write line so
	// the alert rate remains one logical recovery boundary per occurrence.
	{name: "http-hijack-write", re: regexp.MustCompile(`http: response\.WriteHeader on hijacked connection`),
		rateThreshold: 1, tier: tierWarn, playbook: "SIGNALS.md §1.5 and §4",
		sample:    httpHijackWriteLogSample,
		meaning:   "router recovery attempted to synthesize an HTTP response after a handler transferred ownership of the H1 connection through Hijack",
		mechanism: "Connect's GET / route hands its socket to Gorilla. The router correctly recognized a later Done panic as expected cancellation and suppressed its error accounting, but then fell through to http.Error. net/http rejected both the 500 header and body because it no longer owned the connection. A two-hour production control found 131 canonical WriteHeader warnings and zero paired [h]unhandled route errors, isolating the expected-Done fallthrough rather than an application panic.",
		context:   "The rejected 500 does not itself prove a failed handshake or active transport; it is recovery-path log amplification during connection teardown. A warning paired with [h]unhandled error from route is a different case: preserve and fix that underlying panic rather than hiding all net/http diagnostics.",
		action:    "Deploy the router recovery fix that returns immediately for server.IsDoneError before http.Error. Do not suppress net/http's error logger globally. If any post-fix occurrence has a paired [h]unhandled route error, diagnose that route panic and its hijack ownership boundary independently.",
		verify:    "Every Connect block runs the fixed router; normal H1 connection teardown produces zero http-hijack-write lines for 10 minutes, while deterministic recovery tests prove a Done panic performs no write after Hijack and an unexpected pre-hijack panic retains the ordinary 500 path.",
	},
	{name: "netescrow-negative", re: regexp.MustCompile(`\[netescrow\]negative counter after`),
		rateThreshold: 1, pageRateThreshold: netEscrowNegativePageRate, tier: tierWarn, playbook: "SIGNALS.md 5.11", redactIDs: true,
		groupBy:   netEscrowNegativeLogGroup,
		sample:    netEscrowNegativeLogSample,
		meaning:   "a settlement/release found fewer bytes in a Redis reservation mirror than PostgreSQL durably released; old binaries leave that negative value available until reconciliation, while a clamped_to=0 line means the current atomic release retained the diagnostic result and deleted the nonpositive mirror in the same command (a later legitimate reservation or reconciliation can recreate a positive key)",
		mechanism: "Two reconciliation paths can create this aftermath. A legacy full-fleet reconciler overwrites live mirror traffic with an old absolute SET or DEL snapshot. On the current page-local additive path, a PostgreSQL statement fixes its page snapshot before the reservation query runs; a live settlement can commit and update Redis while a slow page is still executing, so the later Redis GET sees the newer mirror and the correction re-adds bytes from the stale PostgreSQL snapshot. A separate smaller commit/post race occurs when reconciliation observes a committed settlement before its delayed Redis release. Current release Lua clamps a resulting negative atomically. This line is mutation-site aftermath, not evidence that the site independently created fleet-wide drift.",
		context:   "Correlate the line with the nearest ReconcileNetEscrow duration and aggregate correction, query taskworker, API, and Connect for the complete interval after allowing for log-ingestion delay, and retain whether clamped_to=0 was present. Production controls have paired isolated clamped taskworker lines and quiet API/Connect emitters with 14-19 second passes, zero new legacy calls, and roughly 26ms unsettled-partial pages; that shape is a contained residual race, not evidence that the access-path rollout failed. The rate is observed settlement/release exposure, not overwritten bytes or necessarily unique balances. A UTC activation boundary can legitimately change the scanned balance count, so use executor and statement deltas rather than the count alone to diagnose a duplicate walk. Samples retain the non-sensitive site while redacting balance and contract ids.",
		action:    "Do not manually zero/delete individual mirrors or invoke reconciliation. Confirm the exact executor, reservation statement shape and timing, page-local additive semantics, and atomic release Lua. A legacy executor needs the additive path. A current executor with slow legacy-ANY or historical bounded-lateral pages needs migration 601 plus the unsettled-partial query to shrink the PostgreSQL-snapshot-to-Redis-GET window. If every residual line says clamped_to=0 and the matching aggregate stays small, observe the contained commit/post ordering window through a full quiet interval; if matched reversals persist on fast unsettled-partial pages, add durable per-balance fencing/versioning rather than redeploying the already-present fixes.",
		verify:    "After allowing for log-ingestion delay, require unsettled-partial pages below 1 second, one scheduled pass below 120 seconds, its aggregate correction below 256GiB and back in the ordinary tens-of-GiB band, and zero netescrow-negative lines from taskworker, API, and Connect for a full following interval. After rollout, any residual race line must say clamped_to=0. A later key read must be absent when there are no new reservations, or exactly equal the current PostgreSQL open-reservation sum when legitimate work recreated it; key presence alone does not disprove the atomic clamp.",
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
		verify:    "First verify every taskworker block's embedded source revision. Deploy the proportional-jitter taskworker only to blocks older than commit 70b0d269; if all blocks are already current, do not redeploy from this alert and instead verify that a saturated cohort no longer repeats as a narrow hourly wave and processor-rate-limit remains bounded. After funding or an intentional resume, allow up to 90 minutes plus log-ingestion delay; AdvancePayment wallet-insufficient rows and this log rate converge to zero without manual row changes, while payment records show no duplicate Circle transfers.",
		redactIDs: true,
		burst: &logBurst{
			name:      "payout-retry-microburst",
			eventRe:   regexp.MustCompile(`\[task\.go:[0-9]+\]`),
			threshold: 4,
			tier:      tierWarn,
			meaning:   "four or more distinct AdvancePayment wallet-rejection attempts landed in one second, the exact short-window shape that preceded live Circle 429s",
			mechanism: "The old capped task backoff added only 0–2 seconds of jitter, so outage-created rows retained second-scale cohorts across their hourly retries. The standing tailer counts only task evaluator lines (one canonical line per attempt), groups their embedded timestamps by second, and de-duplicates exact replayed lines before computing the peak. This separates attempt concurrency from the parent class's two diagnostic lines per failure.",
			context:   "This is a deployable software-amplification alert, separate from the operational liquidity alert. The threshold is an empirical incident discriminator: four wallet failures in one second immediately preceded a 429 on 2026-08-31; it is not a claim about Circle's account-specific quota. A post-provenance control on deployed source 1d8f01e5 found 28 canonical attempts from 56 diagnostic lines, peaked at six attempts at 00:30:43Z, and produced one canonical 429 in that exact second. The standing monitor independently rendered the same peak and event.",
			action:    "Verify every taskworker block's embedded source revision and deploy commit 70b0d269 or later only to older blocks. Do not accelerate, manually replay, or delete payment tasks. If every block is already current, do not redeploy from this alert; correlate all Circle request sources and the account's authoritative quota after one 90-minute drain window.",
			verify:    "After complete taskworker convergence, observe a full 90-minute window with peak_task_attempts_per_second below 4, no payment-processor-rate-limit event, and unchanged payment idempotency keys. Funding or pausing the wallet remains a separate operational verification.",
			playbook:  "SIGNALS.md §1.2 and §5.7",
		}},
	{name: "payout-invalid-destination", re: regexp.MustCompile(`(?i)Bad status: 400 Bad Request.*invalid destination address`),
		rateThreshold: 1, tier: tierWarn, playbook: "SIGNALS.md §1.2 and §5.7",
		canonical: &logCanonical{
			eventRe: regexp.MustCompile(`\[task\.go:[0-9]+\]`),
			name:    "invalid_destination_events",
		},
		meaning:   "Circle definitively rejected a payout destination before creating a transfer because the configured wallet address is invalid for its declared chain",
		mechanism: "A pre-fix chain-blind validator allowed cross-chain wallet shapes such as a Solana base58 key declared as MATIC. Current taskworker code recognizes Circle's typed invalid-destination response and safely releases only that pre-chain submit attempt, but the next retry selects the same invalid payout_wallet configuration until the account owner or operator corrects it.",
		context:   "One failure is normally logged at the Circle client and again by the task evaluator, so diagnostic lines are not unique attempts. OCI/SLSA provenance for deployed taskworker 2026.8.31-outerwerld+1033655820 identifies source 1d8f01e5: it contains typed-reset commit b8af229f but predates retry-dispersion commit 70b0d269. A later three-hour control found 18 canonical evaluator attempts: the same six payments recurred at the same minute in each UTC hour across both taskworker blocks. Persistence after the deployed safe reset is operational wallet-configuration evidence; exact hourly alignment independently proves the old jitter, but retry dispersion cannot repair wallet data.",
		action:    "Correct the affected network's payout wallet through the supported account API so its address matches its declared payout chain. Separately deploy taskworker commit 70b0d269 or later to disperse saturated retries, but do not present that deployment as the wallet correction. Do not edit account_payment or pending_task rows, manually release attempts, rotate processor idempotency keys, or invent a replacement wallet without account-owner or operator authority.",
		verify:    "After the payout wallet is corrected, the next natural retry selects it with a fresh key released only by the prior definitive rejection, completes without a duplicate transfer, invalid_destination_events remains zero, and the durable processor-invalid-destination count converges to zero within 90 minutes plus log-ingestion delay.",
		redactIDs: true},
	{name: "payment-processor-rate-limit", re: regexp.MustCompile(`Bad status: 429 Too Many Requests.*API rate limit error`),
		rateThreshold: 1, tier: tierWarn, playbook: "SIGNALS.md §1.2 and §5.7",
		canonical: &logCanonical{
			eventRe: regexp.MustCompile(`\[task\.go:[0-9]+\]`),
			name:    "processor_rate_limit_events",
		},
		meaning:   "Circle refused a payment API request because the shared processor identity crossed a short-window request limit",
		mechanism: "One failed AdvancePayment attempt is normally logged once by the Circle client and again by the task evaluator, so this diagnostic line rate is not a unique-submit rate. In the 2026-08-31 pre-fix retry cohort, five distinct wallet-insufficient attempts landed in one second before a sixth request received 429; ten seconds later, four attempts preceded another 429. The one-hour capped backoff's old 0–2-second jitter preserved these second-scale microbursts across taskworkers even though the minute average looked modest. After artifact provenance proved deployed source 1d8f01e5 predates the jitter fix, the UTC-rollover recurrence produced 28 canonical wallet attempts, a six-attempt peak at 00:30:43Z, and one canonical 429 from two diagnostic lines in that same second.",
		context:   "A 429 is an ambiguous submit outcome: it is not safe evidence that Circle created no transaction, so the existing payment idempotency key must be retained. Co-residency with the synchronized wallet-insufficient cohort demonstrates application-side amplification for this incident, not a general Circle outage or proof that every future 429 has the same cause.",
		action:    "Do not manually retry, delete, or pull payment tasks forward. Verify every taskworker block's embedded source revision and deploy commit 70b0d269 or later only to older blocks so saturated retries disperse across 30–90 minutes. If every block is already current, do not redeploy from this alert; after one full 90-minute drain window, measure exact per-second Create/Get traffic across every taskworker and any other Circle client against the account's authoritative quota before adding or tuning a shared provider limiter.",
		verify:    "Every taskworker block runs the proportional-jitter build; the next saturated cohort has no narrow second-scale cluster, the durable processor-rate-limit count does not increase, and retries preserve their original idempotency keys. A remaining 429 must be correlated with all Circle request sources and the provider's account-specific limit rather than inferred from a minute rate.",
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

// warpctl wraps the authoritative source timestamp in brackets and preserves
// either Z or an explicit offset. Normalize equivalent instants to one UTC
// second before grouping and rendering: an offset-free peak cannot be joined
// safely to processor, database, or kernel evidence. Whole-second resolution
// remains intentional because the incident discriminator is concurrent task
// attempts inside the provider's short admission window, not log arrival time.
var logTimestampSecondRe = regexp.MustCompile(`\[(20[0-9]{2}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]+)?(?:Z|[+-][0-9]{2}:[0-9]{2}))\]`)

func parseLogTimestamp(line string) (time.Time, bool) {
	match := logTimestampSecondRe.FindStringSubmatch(line)
	if len(match) < 2 {
		return time.Time{}, false
	}
	observedAt, err := time.Parse(time.RFC3339Nano, match[1])
	if err != nil {
		return time.Time{}, false
	}
	return observedAt, true
}

func logTimestampSecond(line string) string {
	observedAt, ok := parseLogTimestamp(line)
	if !ok {
		return ""
	}
	return observedAt.UTC().Truncate(time.Second).Format(time.RFC3339)
}

var requiredVaultResourceRe = regexp.MustCompile(`Resource not found in vault \(([^\)]+\.yml)\)`)
var requiredVaultRouteRe = regexp.MustCompile(`route ([A-Z]+) \^?([^$:\s]+)\$?:`)
var netEscrowNegativeSiteRe = regexp.MustCompile(`\[netescrow\]negative counter after ([a-z][a-z -]{0,40}):`)
var netEscrowClampMarkerRe = regexp.MustCompile(`\bclamped_to=[^\s]+`)
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

// netEscrowNegativeLogSample makes the atomic-clamp discriminator explicit.
// The live line puts clamped_to=0 after two identifiers and the negative
// result, beyond the generic 200-byte sample boundary even after ID redaction.
// An absent marker is also explicit so an operator can distinguish legacy
// behavior from truncation without reopening protected raw logs.
func netEscrowNegativeLogSample(line string) string {
	marker := netEscrowClampMarkerRe.FindString(line)
	if marker == "" {
		marker = "clamp_marker=absent"
	}
	return truncateLinePreservingSuffix(line, marker)
}

// httpHijackWriteLogSample retains the complete net/http source frame. The
// warp identity and its duplicate timestamps consume most of the generic
// 200-byte prefix, while the final ServeHTTP frame is the discriminator that
// separates the known router recovery fallthrough from another post-hijack
// writer with a different root cause.
func httpHijackWriteLogSample(line string) string {
	const marker = "http: response.WriteHeader on hijacked connection"
	markerIndex := strings.Index(line, marker)
	if markerIndex < 0 {
		return truncateLine(line)
	}
	return truncateLinePreservingSuffix(line, line[markerIndex:])
}

var mimirBucketIndexLagSampleRe = regexp.MustCompile(`caller=bucket\.go:[0-9]+\b.*?\bours=[^[:space:]]+[[:space:]]+requested=[^[:space:]]+[[:space:]]+diff=-[0-9]+[[:space:]]+msg="bucket index version \(updated_at\) is older than requested"`)

// mimirBucketIndexLagLogGroup retains the exact replica identity whose local
// cache lagged. The raw tenant remains deliberately absent; this deployment
// uses anonymous today, but the alert identity must stay privacy-safe if that
// changes.
func mimirBucketIndexLagLogGroup(line string) string {
	identity := parseWarpLogIdentity(line)
	parts := []string{}
	if identity.host != "" {
		parts = append(parts, "host="+identity.host)
	}
	if identity.generation != "" {
		parts = append(parts, "generation="+identity.generation)
	}
	return strings.Join(parts, " ")
}

// mimirBucketIndexLagLogSample preserves the two versions and their exact
// difference even when the warp identity and timestamps consume the generic
// sample budget.
func mimirBucketIndexLagLogSample(line string) string {
	if sample := mimirBucketIndexLagSampleRe.FindString(line); sample != "" {
		return truncateLine(sample)
	}
	return truncateLinePreservingSuffix(line, "bucket index version (updated_at) is older than requested")
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
	if !strings.Contains(line, " query=") {
		return false
	}
	if strings.Contains(line, "caller=metrics.go:") {
		// Loki's completed-query metrics line carries the same literal plus
		// bounded execution metadata. Requiring both fields keeps an unrelated
		// metrics.go info line visible.
		return strings.Contains(line, " query_hash=") &&
			strings.Contains(line, " status=")
	}
	if !strings.Contains(line, "caller=engine.go:") &&
		!strings.Contains(line, "caller=roundtrip.go:") {
		return false
	}
	return strings.Contains(line, `msg=\"executing query\"`) ||
		strings.Contains(line, `msg="executing query"`)
}

// logTailer tails one service's logs and aggregates per-minute class counts.
// Safe for one run goroutine plus concurrent snapshot calls.
type logTailer struct {
	service string
	blocks  []string
	env     *probeEnv
	clock   func() time.Time
	// startedAt separates history that predates this collector from records
	// eligible for its first complete rate window. The first reconciliation
	// remembers older records without counting them.
	startedAt time.Time

	stateLock sync.Mutex
	// class -> count in the current minute window
	classCounts map[string]int
	// class -> one sample line + one target from the window
	classSamples map[string]string
	classTargets map[string]string
	// A configured burst counts canonical logical-event lines by their embedded
	// log second. Exact-line hashes make a one-second stream replay idempotent.
	// Retaining the immediately previous drain window covers a reconnect that
	// straddles the cadence boundary without making this state grow forever.
	burstSecondCounts     map[string]int
	burstPeaks            map[string]int
	burstPeakSeconds      map[string]string
	burstEventTotals      map[string]int
	burstSamples          map[string]string
	burstSeen             map[[sha256.Size]byte]struct{}
	burstSeenPrevious     map[[sha256.Size]byte]struct{}
	canonicalCounts       map[string]int
	canonicalSeen         map[[sha256.Size]byte]struct{}
	canonicalSeenPrevious map[[sha256.Size]byte]struct{}
	// The WebSocket tail is an arrival stream, while the reconciliation query
	// replays an overlapping source-time window. Retain fingerprints only for
	// alert-relevant records long enough to make the two transports and
	// successive overlap queries idempotent without retaining ordinary logs.
	standingSeen map[[sha256.Size]byte]time.Time
	// normalized novel shape -> count
	novelCounts map[string]int
	novelSample string
	// tailer self-health (§3.7), read by the logTailProbe health findings
	lastLineTime   time.Time
	restartCount   int
	scanErrorCount int
	// A successful bounded query closes the Search -> LiveTail gap and catches
	// Loki records ingested behind the WebSocket timestamp cursor. Its health
	// is independent from the still-connected stream health above.
	reconcileInitialized bool
	lastReconcileTime    time.Time
	lastReconcileError   string

	// stream is a test seam over runner.warpctlStream; nil = the real stream
	stream func(ctx context.Context) (*exec.Cmd, io.ReadCloser, error)
	// reconcile is a test seam over a bounded runner.warpctl query. It is nil
	// for isolated classifier tests and configured for every production tailer.
	reconcile logReconcileQuery
}

func newLogTailer(service string, env *probeEnv) *logTailer {
	clock := time.Now
	if env != nil && env.now != nil {
		clock = env.now
	}
	startedAt := clock()
	tailer := &logTailer{
		service:               service,
		env:                   env,
		clock:                 clock,
		startedAt:             startedAt,
		classCounts:           map[string]int{},
		classSamples:          map[string]string{},
		classTargets:          map[string]string{},
		burstSecondCounts:     map[string]int{},
		burstPeaks:            map[string]int{},
		burstPeakSeconds:      map[string]string{},
		burstEventTotals:      map[string]int{},
		burstSamples:          map[string]string{},
		burstSeen:             map[[sha256.Size]byte]struct{}{},
		burstSeenPrevious:     map[[sha256.Size]byte]struct{}{},
		canonicalCounts:       map[string]int{},
		canonicalSeen:         map[[sha256.Size]byte]struct{}{},
		canonicalSeenPrevious: map[[sha256.Size]byte]struct{}{},
		standingSeen:          map[[sha256.Size]byte]time.Time{},
		novelCounts:           map[string]int{},
		// silence is measured from tailer start until the first line arrives
		lastLineTime: startedAt,
	}
	if env != nil && env.runner != nil && env.cfg != nil {
		tailer.blocks = append([]string(nil), env.cfg.logServiceBlocks[service]...)
		tailer.reconcile = func(ctx context.Context, start time.Time, blocks []string) (string, error) {
			args := []string{"logs", env.cfg.env, service}
			args = append(args, blocks...)
			args = append(
				args,
				"--since="+start.UTC().Format(time.RFC3339Nano),
				fmt.Sprintf("--limit=%d", logReconcileLimit),
			)
			return env.runner.warpctl(ctx, args...)
		}
	}
	return tailer
}

const (
	// Loki accepts out-of-order writes, but its tail cursor advances by source
	// timestamp. A record ingested after that cursor has passed is recoverable
	// only through an overlapping range query. Two minutes covers more than two
	// query cadences and the observed production delay while retaining enough
	// headroom below Loki's all-line result cap on the noisiest services.
	logReconcileLookback  = 2 * time.Minute
	logReconcileInterval  = 45 * time.Second
	logReconcileRetention = logReconcileLookback + 2*time.Minute
	logReconcileLimit     = 20000
	// Eight pages bound one hot partition to 160,000 returned records per
	// cadence. A partition whose source-time boundary keeps advancing can be
	// recovered; one that cannot drain inside this budget remains an explicit
	// visibility failure rather than an unbounded query loop.
	logReconcileMaxPages = 8
)

// run tails and independently reconciles one service until ctx is done.
// Reconciliation cannot block or distort the in-memory one-minute drain.
func (self *logTailer) run(ctx context.Context) {
	var reconcileGroup sync.WaitGroup
	if self.reconcile != nil {
		reconcileGroup.Add(1)
		go func() {
			defer reconcileGroup.Done()
			self.runReconcile(ctx)
		}()
	}
	self.runStream(ctx)
	reconcileGroup.Wait()
}

// runStream owns the standing warpctl child and its restart lifecycle.
func (self *logTailer) runStream(ctx context.Context) {
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

// runReconcile periodically re-queries a bounded overlap. It runs immediately
// so the stream/search handoff is covered from collector startup, then on an
// independent cadence shorter than the alert drain cadence.
func (self *logTailer) runReconcile(ctx context.Context) {
	self.reconcileOnce(ctx)
	ticker := time.NewTicker(logReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			self.reconcileOnce(ctx)
		}
	}
}

type reconcileLine struct {
	line       string
	observedAt time.Time
}

func parseReconcileLines(out string) []reconcileLine {
	lines := []reconcileLine{}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		observedAt, ok := parseLogTimestamp(line)
		if !ok {
			// runner.warpctl retains local retry diagnostics on stderr. Only
			// warpctl's timestamp-framed remote records may enter a service
			// classifier.
			continue
		}
		lines = append(lines, reconcileLine{line: line, observedAt: observedAt})
	}
	return lines
}

func reconcilePartitionLabel(service string, blocks []string) string {
	if len(blocks) == 0 {
		return service
	}
	return fmt.Sprintf("%s block %s", service, strings.Join(blocks, ","))
}

// completeReconcilePartition continues a cap-sized page from its inclusive
// final source timestamp. Inclusive overlap is required because Loki's range
// API has no cursor within one timestamp; exact-line fingerprints remove the
// repeated boundary while preserving every later record. A boundary that does
// not advance, or a partition still full after logReconcileMaxPages, fails
// closed because completeness cannot be proven.
func (self *logTailer) completeReconcilePartition(
	ctx context.Context,
	start time.Time,
	blocks []string,
	first []reconcileLine,
) ([]reconcileLine, error) {
	partition := reconcilePartitionLabel(self.service, blocks)
	lines := make([]reconcileLine, 0, len(first))
	seen := make(map[[sha256.Size]byte]struct{}, len(first))
	appendUnique := func(page []reconcileLine) {
		for _, entry := range page {
			fingerprint := sha256.Sum256([]byte(entry.line))
			if _, ok := seen[fingerprint]; ok {
				continue
			}
			seen[fingerprint] = struct{}{}
			lines = append(lines, entry)
		}
	}
	appendUnique(first)

	page := first
	pageStart := start
	pages := 1
	for len(page) >= logReconcileLimit {
		if pages >= logReconcileMaxPages {
			return nil, fmt.Errorf(
				"bounded overlap reached the %d-line limit for %s across %d pages; late-entry coverage is incomplete",
				logReconcileLimit,
				partition,
				pages,
			)
		}

		boundary := page[0].observedAt
		for _, entry := range page[1:] {
			if boundary.Before(entry.observedAt) {
				boundary = entry.observedAt
			}
		}
		if !boundary.After(pageStart) {
			return nil, fmt.Errorf(
				"bounded overlap reached the %d-line limit for %s and continuation did not advance beyond %s; late-entry coverage is incomplete",
				logReconcileLimit,
				partition,
				pageStart.UTC().Format(time.RFC3339Nano),
			)
		}

		pageStart = boundary
		out, err := self.reconcile(ctx, pageStart, blocks)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			return nil, fmt.Errorf("%s continuation: %w", partition, err)
		}
		page = parseReconcileLines(out)
		pages++
		appendUnique(page)
	}
	return lines, nil
}

// reconcileOnce folds entries absent from the live stream into the current
// window. Failed and truncated queries are never treated as complete history;
// their state is exposed by tailerReconcileFinding. A service-wide query is
// cheapest during ordinary traffic. If it reaches Loki's result cap, retry
// the same absolute lower boundary once per configured block. A cap-sized
// block (or a single-block service) then continues from its inclusive final
// source timestamp under a fixed page budget.
func (self *logTailer) reconcileOnce(ctx context.Context) {
	if self.reconcile == nil {
		return
	}
	start := self.clock().Add(-logReconcileLookback)
	out, err := self.reconcile(ctx, start, nil)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		self.recordReconcile(err)
		return
	}

	lines := parseReconcileLines(out)
	if len(lines) >= logReconcileLimit {
		if len(self.blocks) <= 1 {
			lines, err = self.completeReconcilePartition(ctx, start, nil, lines)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				self.recordReconcile(err)
				return
			}
		} else {
			lines = nil
			for _, block := range self.blocks {
				blockPartition := []string{block}
				out, err := self.reconcile(ctx, start, blockPartition)
				if ctx.Err() != nil {
					return
				}
				if err != nil {
					self.recordReconcile(fmt.Errorf("block %s overlap: %w", block, err))
					return
				}
				blockLines, err := self.completeReconcilePartition(
					ctx,
					start,
					blockPartition,
					parseReconcileLines(out),
				)
				if ctx.Err() != nil {
					return
				}
				if err != nil {
					self.recordReconcile(err)
					return
				}
				lines = append(lines, blockLines...)
			}
		}
	}

	self.stateLock.Lock()
	initial := !self.reconcileInitialized
	self.reconcileInitialized = true
	self.stateLock.Unlock()
	for _, entry := range lines {
		count := !initial || !entry.observedAt.Before(self.startedAt)
		self.ingestStanding(entry.line, false, count)
	}
	self.recordReconcile(nil)
}

func (self *logTailer) recordReconcile(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if err != nil {
		self.lastReconcileError = err.Error()
		return
	}
	self.lastReconcileTime = self.clock()
	self.lastReconcileError = ""
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
		self.ingestStanding(scanner.Text(), true, true)
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

func (self *logTailer) reconcileSnapshot() (enabled bool, startedAt time.Time, lastSuccess time.Time, lastError string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.reconcile != nil, self.startedAt, self.lastReconcileTime, self.lastReconcileError
}

// classify folds one log line into the current window.
func (self *logTailer) classify(line string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lastLineTime = self.clock()
	self.classifyLocked(line, false, true, self.clock())
}

// ingestStanding is used by both standing transports. Exact replay
// suppression applies only to alert-relevant records, and a reconciliation
// record never refreshes WebSocket liveness.
func (self *logTailer) ingestStanding(line string, updateLiveness bool, count bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	now := self.clock()
	if updateLiveness {
		self.lastLineTime = now
	}
	self.classifyLocked(line, true, count, now)
}

func (self *logTailer) classifyLocked(line string, deduplicate bool, count bool, now time.Time) {
	if isGrafanaQueryEcho(self.service, line) {
		return
	}

	for _, c := range logClasses {
		if c.re.MatchString(line) {
			if deduplicate && self.standingReplayLocked(line, now) {
				return
			}
			if !count {
				return
			}
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
				self.classSamples[key] = logClassSample(c, line)
				if attribution != "" {
					self.classTargets[key] = attribution
				} else if target := targetRe.FindString(line); target != "" {
					self.classTargets[key] = target
				}
			}
			if c.canonical != nil && c.canonical.eventRe.MatchString(line) {
				fingerprint := sha256.Sum256([]byte("canonical\x00" + key + "\x00" + line))
				_, replayedCurrent := self.canonicalSeen[fingerprint]
				_, replayedPrevious := self.canonicalSeenPrevious[fingerprint]
				if !replayedCurrent && !replayedPrevious {
					self.canonicalSeen[fingerprint] = struct{}{}
					self.canonicalCounts[key] += 1
				}
			}
			if c.burst != nil && c.burst.eventRe.MatchString(line) {
				if second := logTimestampSecond(line); second != "" {
					fingerprint := sha256.Sum256([]byte(key + "\x00" + line))
					_, replayedCurrent := self.burstSeen[fingerprint]
					_, replayedPrevious := self.burstSeenPrevious[fingerprint]
					if !replayedCurrent && !replayedPrevious {
						self.burstSeen[fingerprint] = struct{}{}
						secondKey := key + "\x00" + second
						self.burstSecondCounts[secondKey] += 1
						self.burstEventTotals[key] += 1
						if self.burstPeaks[key] < self.burstSecondCounts[secondKey] {
							self.burstPeaks[key] = self.burstSecondCounts[secondKey]
							self.burstPeakSeconds[key] = second
							self.burstSamples[key] = logClassSample(c, line)
						}
					}
				}
			}
			return
		}
	}
	if errorShapedRe.MatchString(line) {
		if deduplicate && self.standingReplayLocked(line, now) {
			return
		}
		if !count {
			return
		}
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

func (self *logTailer) standingReplayLocked(line string, now time.Time) bool {
	fingerprint := sha256.Sum256([]byte(line))
	if seenAt, ok := self.standingSeen[fingerprint]; ok && now.Before(seenAt.Add(logReconcileRetention)) {
		return true
	}
	self.standingSeen[fingerprint] = now
	return false
}

func (self *logTailer) pruneStandingSeenLocked(now time.Time) {
	for fingerprint, seenAt := range self.standingSeen {
		if !now.Before(seenAt.Add(logReconcileRetention)) {
			delete(self.standingSeen, fingerprint)
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
			if c.observationOnly {
				observed += fmt.Sprintf(" observation_service=%s affected_selector=unknown", self.service)
			} else {
				attributionLabel := "target"
				if c.groupBy != nil {
					attributionLabel = "frame"
				}
				observed += fmt.Sprintf(" %s=%s", attributionLabel, attribution)
			}
			canonicalEvidence := ""
			if c.canonical != nil {
				observed += fmt.Sprintf(
					" %s=%d diagnostic_lines=%d canonical_source=exact-replay-deduplicated-task-evaluator",
					c.canonical.name,
					self.canonicalCounts[key],
					count,
				)
				canonicalEvidence = fmt.Sprintf(
					"\nlogical event count: %d exact-replay-deduplicated task evaluator line(s) from %d diagnostic line(s)",
					self.canonicalCounts[key],
					count,
				)
			}
			symptom := fmt.Sprintf("service %s: %d/min lines of class %s (threshold %d/min)", self.service, count, c.name, c.rateThreshold)
			if c.observationOnly {
				symptom = fmt.Sprintf("observation service %s emitted %d/min lines of class %s (threshold %d/min); the affected live-tail selector is unknown", self.service, count, c.name, c.rateThreshold)
			}
			findings = append(findings, finding{
				probeId: "logs/" + c.name, tier: tier,
				class: c.name, target: self.service, frame: attribution, sustain: 1,
				symptom:   symptom,
				baseline:  baseline,
				observed:  observed,
				mechanism: c.mechanism,
				evidence:  "meaning: " + c.meaning + canonicalEvidence + "\nsample: " + self.classSamples[key],
				context:   c.context,
				action:    c.action,
				verify:    c.verify,
				playbook:  c.playbook,
			})
		}
		if !broken {
			findings = append(findings, healthyFinding("logs/"+c.name, c.tier, c.name, self.service))
		}

		if c.burst != nil {
			burstBroken := false
			for _, key := range keys {
				peak := self.burstPeaks[key]
				if peak < c.burst.threshold {
					continue
				}
				burstBroken = true
				attribution := self.classTargets[key]
				observed := fmt.Sprintf(
					"peak_task_attempts_per_second=%d peak_source_second=%s threshold=%d/s task_attempts=%d diagnostic_lines=%d",
					peak,
					self.burstPeakSeconds[key],
					c.burst.threshold,
					self.burstEventTotals[key],
					self.classCounts[key],
				)
				if attribution != "" {
					observed += " target=" + attribution
				}
				findings = append(findings, finding{
					probeId: "logs/" + c.burst.name, tier: c.burst.tier,
					class: c.burst.name, target: self.service, frame: attribution, sustain: 1,
					symptom: fmt.Sprintf(
						"service %s: %s peaked at %d distinct task attempts/s (threshold %d/s)",
						self.service,
						c.burst.name,
						peak,
						c.burst.threshold,
					),
					baseline:  fmt.Sprintf("peak distinct task evaluator attempts < %d/s; minute volume alone does not prove a synchronized retry wave", c.burst.threshold),
					observed:  observed,
					mechanism: c.burst.mechanism,
					evidence:  "meaning: " + c.burst.meaning + "\npeak source second: " + self.burstPeakSeconds[key] + " (normalized UTC; exact-replay-deduplicated task evaluator lines grouped by embedded source second)\nsample from peak second: " + self.burstSamples[key],
					context:   c.burst.context,
					action:    c.burst.action,
					verify:    c.burst.verify,
					playbook:  c.burst.playbook,
				})
			}
			if !burstBroken {
				findings = append(findings, healthyFinding("logs/"+c.burst.name, c.burst.tier, c.burst.name, self.service))
			}
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
	self.burstSecondCounts = map[string]int{}
	self.burstPeaks = map[string]int{}
	self.burstPeakSeconds = map[string]string{}
	self.burstEventTotals = map[string]int{}
	self.burstSamples = map[string]string{}
	self.burstSeenPrevious = self.burstSeen
	self.burstSeen = map[[sha256.Size]byte]struct{}{}
	self.canonicalCounts = map[string]int{}
	self.canonicalSeenPrevious = self.canonicalSeen
	self.canonicalSeen = map[[sha256.Size]byte]struct{}{}
	self.novelCounts = map[string]int{}
	self.novelSample = ""
	self.pruneStandingSeenLocked(self.clock())

	return findings
}

func truncateLine(line string) string {
	if len(line) > 200 {
		return line[:200]
	}
	return line
}

func truncateLinePreservingSuffix(line string, suffix string) string {
	if suffix == "" {
		return truncateLine(line)
	}
	truncated := truncateLine(line)
	if strings.Contains(truncated, suffix) {
		return truncated
	}
	const limit = 200
	separator := " "
	budget := limit - len(separator) - len(suffix)
	if budget <= 0 {
		return truncateLine(suffix)
	}
	prefix := line
	if budget < len(prefix) {
		prefix = prefix[:budget]
	}
	return strings.TrimSpace(prefix) + separator + suffix
}

func logClassSample(class logClass, line string) string {
	if class.redactIDs {
		line = logIDRe.ReplaceAllString(line, "<id>")
	}
	if class.sample != nil {
		return class.sample(line)
	}
	return truncateLine(line)
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
	if env != nil && env.now != nil {
		now = env.now()
	}
	for i, tailer := range self.tailers {
		findings = append(findings, tailer.drainWindow()...)

		lastLine, restarts, scanErrors := tailer.healthSnapshot()
		restartDelta := restarts - self.lastRestartCounts[i]
		self.lastRestartCounts[i] = restarts
		findings = append(findings, tailerHealthFindings(tailer.service, now, lastLine, restartDelta, scanErrors)...)
		enabled, startedAt, lastSuccess, lastError := tailer.reconcileSnapshot()
		if enabled {
			findings = append(findings, tailerReconcileFinding(tailer.service, now, startedAt, lastSuccess, lastError))
		}
	}
	return findings, nil
}

func tailerReconcileFinding(service string, now time.Time, startedAt time.Time, lastSuccess time.Time, lastError string) finding {
	target := "logs/" + service
	stale := !lastSuccess.IsZero() && now.Sub(lastSuccess) >= 2*logReconcileInterval
	neverCompleted := lastSuccess.IsZero() && now.Sub(startedAt) >= 2*logReconcileInterval
	if lastError == "" && !stale && !neverCompleted {
		return healthyFinding("monitor/visibility", tierWarn, "tailer-reconcile", target)
	}

	observed := "last_success=never"
	if !lastSuccess.IsZero() {
		observed = "last_success=" + lastSuccess.UTC().Format(time.RFC3339)
	}
	if lastError != "" {
		observed += " error=" + lastError
	} else if stale {
		observed += " error=reconciliation has not completed on cadence"
	} else {
		observed += " error=initial reconciliation has not completed"
	}
	return finding{
		probeId: "monitor/visibility", tier: tierWarn,
		class: "tailer-reconcile", target: target, sustain: 2,
		symptom: fmt.Sprintf(
			"log tailer for %s cannot reconcile records ingested behind its live timestamp cursor",
			service,
		),
		baseline: fmt.Sprintf(
			"a bounded %s overlap completes every %s: aggregate below %d lines, or configured block partitions continued from inclusive source-time boundaries for at most %d pages each",
			logReconcileLookback,
			logReconcileInterval,
			logReconcileLimit,
			logReconcileMaxPages,
		),
		observed:  observed,
		mechanism: "Loki accepts out-of-order records, but a WebSocket tail advances by source timestamp. Without a successful overlapping query, a late-ingested record older than that cursor can remain absent even while the tail process is connected and reading newer lines.",
		action:    "Restore bounded warpctl/Loki query visibility. Retain active services.yml block partitioning and inclusive boundary continuation. If one boundary cannot advance or a partition consumes the eight-page budget, diagnose and remove the high-cardinality log producer before changing query limits. Keep the live tail running; do not interpret missing reconciliation as a healthy error window.",
		verify:    "Two consecutive overlap windows complete below the cap in aggregate or drain every configured block through bounded continuation, and the standing monitor remains free of tailer-reconcile and loki-tail-dropped-streams alerts.",
		playbook:  "SIGNALS.md 1.5",
	}
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
