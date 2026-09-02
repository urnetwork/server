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
	"net/netip"
	"os/exec"
	"regexp"
	"runtime"
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
	// correlation optionally joins each canonical source second to canonical
	// attempts already counted by another class's burst detector. This proves
	// incident co-residency without treating a minute-wide rate as ordering or
	// a provider quota.
	correlation *logCanonicalCorrelation
}

type logCanonicalCorrelation struct {
	burstClass string
	eventName  string
	threshold  int
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

var framerRejectRe = regexp.MustCompile(`\[framer\]\[reject\](?:read|write(?: batch)?) messageLen=[0-9]+ > MaxMessageLen=[0-9]+(?: \(maxFrameLen=[0-9]+\))?`)

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
	{name: "loki-tail-backend-eof", re: regexp.MustCompile(`caller=tail\.go:[0-9]+\s+component=tail-querier\b.*\bmsg="Error receiving response from grpc tail client"(?:\s+addr=(?:"[^"]+"|[^[:space:]]+))?\s+err=EOF(?:\s|$)`),
		groupBy:       lokiTailBackendEOFLogGroup,
		rateThreshold: 5, tier: tierWarn, playbook: "SIGNALS.md §1.5 and §4",
		meaning:   "Loki's external WebSocket tail remained available while its querier lost an internal gRPC tail backend with EOF; entries assigned to that backend can be absent from the live stream",
		mechanism: "The historical exact 59-61-second recurrence came from Warp's Grafana ring TCP proxy applying a 60-second application read deadline to long-lived HTTP/2 and gRPC connections; Warp commit 1e95aef removes it. The current off-grid EOF wave has a separate Loki 3.7.3 backpressure path. Each ingester tailer has a 100-stream processing queue and a five-stream send queue. Its first dropped stream records blockedAt; continued traffic more than 15 seconds later closes that server-side tail. Ingester.Tail then returns nil, the querier's Recv observes EOF and drops the client, and its five-second connection ticker reconnects. A backend process exit or ring loss can still produce the same EOF text, so the backend address and paired reset evidence are required discriminators.",
		context:   "A quoted rpc Canceled/context-canceled error during explicit watcher retirement is a separate lifecycle and this class deliberately excludes it. In a fixed 04:15:08Z-04:29:30Z window on 2026-09-01, current 2026.8.31+1034210530 containers emitted 182 exact EOFs alongside 11,583 dropped-stream resets; 48 quoted cancellations from watcher retirement were excluded. A later clean post-handoff window from 22:44:05Z through 22:49:22Z used 2026.9.1-outerwerld+1035004200 from clean Warp source 71731e4, which already contained 42168fe and bca37cf. Query-frontend `query stats` fell to zero, but the window still had 2,825 unconditional Mimir `evaluation stats`, 1,084 Loki table lookup records, 942 bucket-index warnings, 1,229 reset records, and 22 attributed EOFs split across every one of the six backend addresses. This rules out a stale rollout and a single failed backend. Fireside and Crisp's controlled LAN activation had already restored all six active ring nodes, so missing LAN identity is no longer a current prerequisite. Bounded log reconciliation remains required for late source timestamps and the Search-to-LiveTail handoff.",
		action:    "Verify the running Grafana artifacts contain Warp commits 1e95aef and bca37cf, and deploy them only to older Grafana blocks. For the already-current fleet, deploy one Grafana image containing Warp commit 5927527. It includes 13fcd05's bounded self-telemetry reduction and forwards the ingester's bounded drop descriptors through Loki's existing HTTP dropped_entries response, allowing the affected standing service tail to be named without exposing labels or timestamps. Do not claim producer reduction or attribution itself fixes loss. Correlate a residual service-attributed drop with backend EOFs and the same window's reset records, then inspect the named backend's tailer, process, ring, and network state. Do not raise Loki's fixed queues, tail-request limits, or ring session cap, and do not restart the same image.",
		verify:    "Every Grafana block contains Warp commit 5927527; Mimir query-frontend/evaluator statistics and Loki table lookup info records are zero while alert rules, query metrics, warnings, and errors remain healthy; bucket-index version gaps converge below two minutes; every configured active ring member owns its LAN identity and heartbeats; and loki-tail-dropped-streams, service-attributed loki-tail-dropped-entries, plus loki-tail-backend-eof remain zero for 10 minutes with stable standing tails and complete bounded reconciliation. Any residual EOF must carry a backend frame and be reconciled against that exact node.",
	},
	// This is the ingester-side queue, before the querier's independent HTTP
	// tail response queue. Upstream Loki 3.7.3 drops the accompanying
	// DroppedStreams field in pushTailResponseFromIngester; Warp 5927527
	// forwards it, but this raw Grafana log remains observation-only and must
	// be paired with the service-attributed Warpctl summary.
	{name: "loki-tail-dropped-streams", re: regexp.MustCompile(`caller=tailer\.go:[0-9]+\b.*\bmsg="tailer dropped streams is reset"(?:\s|$)`),
		rateThreshold: 1, tier: tierWarn, playbook: "SIGNALS.md §1.5 and §4", observationOnly: true,
		meaning:   "an ingester-side Loki live tail could not send streams through its bounded internal gRPC path, filled and reset its dropped-stream metadata list, and omitted records before they reached the querier",
		mechanism: "Loki 3.7.3 gives each ingester tailer a 100-stream processing queue and a five-stream send queue. A blocked internal gRPC send or producer burst drops streams. After ten retained drop descriptors the ingester emits this reset. Upstream and the currently deployed querier pushTailResponseFromIngester path forward resp.Stream but discard resp.DroppedStreams, so HTTP dropped_entries and Warpctl cannot attribute this earlier loss. Warp 5927527 closes that observability defect with a bounded forwarding patch; it does not enlarge either queue. If more traffic arrives over 15 seconds after blockedAt was set, the ingester closes that tail; the querier then logs a backend EOF and reconnects on its five-second ticker. Historical reset waves paired with pre-fix per-peer Proxy logs. After those fixes, a 2026-09-01 04:15:08Z-04:29:30Z six-host window contained 35,909 Grafana records, including 11,583 resets, 5,511 Mimir query-frontend statistics, 5,511 evaluator statistics, 4,596 Loki `get or create table` records, and 182 exact backend EOFs. Warp 42168fe removed only the query-frontend stream: a clean 22:44:05Z-22:49:22Z post-deployment window still contained 2,825 unconditional evaluator records, 1,084 table lookup records, 942 bucket-index warnings, 1,229 resets, and 22 attributed EOFs across all six backends. Self-telemetry remains the next controlled producer reduction, not a proven sole cause.",
		context:   "This is affirmative internal live-tail loss even when the external WebSocket process remains connected. Grafana is the observation service that emitted the reset, not the affected application-tail identity. The current 2026.9.1-outerwerld+1035004200 binary came from clean Warp source 71731e4 and already contains the earlier ring, query-frontend-statistics, and backend-attribution changes, so neither a stale rollout nor one failed node explains the residual fleet-wide wave. All six active Grafana nodes were healthy after controlled Fireside/Crisp LAN activation. That removes missing LAN identity as the current prerequisite and strengthens the blocked-tail chain without identifying which backend lost each stream. The standing range reconciliation across every service is the recovery path, but a capped service-wide query is not a total: repeat the same absolute window for each configured block and require every partition to drain. Provenance-check historical fixes; do not redeploy already-current blocks from historical prose.",
		action:    "Deploy Warp commit 1e95aef and server Proxy commit e055c98c only to blocks whose artifacts predate them. For already-current Grafana blocks, deploy Warp commit 5927527. It includes 13fcd05's producer reductions and a bounded Loki querier patch that forwards ingester drop descriptors into the existing dropped_entries response. Warp bca37cf already preserves the backend address on residual EOFs. Retain bounded reconciliation continuation and pair this observation-only reset with service-attributed loki-tail-dropped-entries after the new artifact is live. Do not raise Loki's fixed queues, suppress the reset, or disable correctness reconciliation.",
		verify:    "Every Grafana and Proxy block contains the required fixes including Warp 5927527, every Grafana ring node is healthy, Mimir query-frontend/evaluator statistics and Loki table lookup info records remain zero while alert rules, query metrics, warnings, and errors remain healthy, bucket-index version gaps converge below two minutes, default Proxy logs contain one aggregate summary per reconciling instance, every reconciliation partition drains through bounded continuation, and loki-tail-dropped-streams, service-attributed loki-tail-dropped-entries, plus loki-tail-backend-eof remain zero for 10 minutes with all external tails connected. Any residual EOF is framed by backend address.",
	},
	// Both Loki's ingester descriptor path (after Warp 5927527) and its later
	// querier response-channel path use the existing dropped_entries field.
	// Warpctl receives that metadata on the affected service's WebSocket and
	// can attribute it without retaining labels.
	{name: "loki-tail-dropped-entries", re: regexp.MustCompile(`^\[warpctl\]\[loki-tail-dropped-entries\]\s+service=[A-Za-z0-9._-]+\s+count=[1-9][0-9]*(?:\s|$)`),
		rateThreshold: 1, tier: tierWarn, playbook: "SIGNALS.md §1.5 and §4",
		meaning:   "Loki declared live-tail loss for this service's standing tail and returned a non-empty dropped_entries list",
		mechanism: "Two bounded Loki stages can populate this API field. The querier buffers ten tail responses and attaches up to 1,000 descriptors after that downstream channel overflows. Separately, each ingester already sends up to ten DroppedStreams descriptors with its next stream, but upstream Loki 3.7.3 silently discards them in pushTailResponseFromIngester; Warp 5927527 forwards those descriptors into the same existing response without enlarging a queue. Older Warpctl decoded dropped_entries but silently discarded it; Warp commit 26089b2 emits one local service/count summary for every non-empty response.",
		context:   "The owning monitor tail supplies exact affected-service attribution, but the summary alone does not distinguish the two stages. A same-window ingester reset identifies the earlier path; otherwise inspect the querier-to-WebSocket consumer path as well. The summary is privacy-safe: Warpctl deliberately omits dropped stream labels and timestamps. Bounded range reconciliation remains the content-recovery path.",
		action:    "Verify the running Grafana and Warpctl artifacts contain Warp 5927527 and 26089b2 or later, retain bounded overlap reconciliation for the named service, and remove the correlated producer burst or slow consumer. Do not print dropped labels or timestamps, disable reconciliation, or raise any Loki response or ingester queue.",
		verify:    "The named service tail stays connected, two consecutive overlap reconciliations complete, and no service-attributed loki-tail-dropped-entries summary, ingester reset, or backend EOF appears for 10 minutes through the workload that triggered the loss.",
	},
	// Mimir 3.1 logs any store-gateway bucket-index version behind the
	// querier's requested version as a warning. The live fleet's independent
	// jittered 15-minute loops produced an exact, harmless -873-second
	// one-generation gap. Warp 13fcd05 tightens discovery to one minute, but
	// match only gaps of 30 minutes or more across rolling/older generations so
	// routine phase skew remains visible in metrics without becoming an alert.
	{name: "mimir-bucket-index-lag", re: regexp.MustCompile(`caller=bucket\.go:[0-9]+\b.*\bdiff=-(?:1[89][0-9]{2}|[2-9][0-9]{3}|[1-9][0-9]{4,})\b.*\bmsg="bucket index version \(updated_at\) is older than requested"`),
		sample: mimirBucketIndexLagLogSample, groupBy: mimirBucketIndexLagLogGroup,
		rateThreshold: 1, tier: tierWarn, playbook: "SIGNALS.md §11.18",
		meaning:   "a Mimir store-gateway answered with a local bucket index at least 30 minutes older than the version requested by its querier",
		mechanism: "The querier sends its most recently discovered bucket-index update time to each store-gateway. A gateway compares that request metadata with its local index. Independent jittered 15-minute discovery loops can leave one gateway one generation behind; the production control's exact -873-second gap was that default phase skew. Warp 13fcd05 sets the single-tenant fleet's store-gateway discovery interval to one minute. The 1,800-second alert threshold remains fail-safe across rolling or older generations and indicates many missed current syncs.",
		context:   "This warning is emitted after the gateway receives the RPC and is not by itself a failed query. Correlate the exact host with the mimir-index gateway freshness and tenant-coverage findings plus err-mimir-store-consistency-check-failed. Mimir 3.2 replaces this noisy 3.1 warning with a histogram, but a feature upgrade solely to hide the line is not a root-cause fix.",
		action:    "Verify the running Grafana artifact contains Warp 13fcd05. If it is absent, deploy it through the normal Grafana rollout; if present, run the §11.18 mimir-index probe and inspect the framed host/generation for store-gateway sync, object-store, and ring errors. Restore periodic sync or tenant coverage. Do not suppress every bucket warning, increase max_stale_period, or restart all replicas together.",
		verify:    "On Warp 13fcd05, every gateway's last successful sync remains under two minutes old, discovered tenants equal synced tenants, the shared bucket index remains under 35 minutes old, and no >=1,800-second warning or Mimir consistency error recurs for 10 minutes.",
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
		sample:    grafanaPluginUnregisteredLogSample,
		meaning:   "one Grafana request asked this process to resolve a plugin type that was not registered; this is a request-level failure until exact datasource controls distinguish a missing native plugin from a stale or unsupported request payload",
		mechanism: "Grafana returns plugin.notRegistered whenever the request names a plugin type absent from that process's registry. Grafana 13 packages the formerly core Prometheus and Loki datasource implementations as standalone native plugins, so an image omission is one cause; a stale browser/dashboard payload or another unsupported request plugin type can produce the same generic line while warp-mimir and warp-loki controls both succeed. The Logs Drilldown app is a frontend and does not register datasource type loki.",
		context:   "A direct Mimir or Loki query, Grafana /api/health, or the datasource database row cannot exercise plugin loading. Correlate the bounded referer and request path with the exact Grafana generation, then run both authenticated /api/ds/query controls on that same generation. The dedicated grafana-datasources probe owns the native-plugin packaging diagnosis.",
		action:    "If vector(1) through warp-mimir or bounded count_over_time through warp-loki fails on the same generation, publish a corrected image with the pinned native plugin and catalog SHA-256 and run Warp's packaging and provisioning tests. If both controls succeed and both plugin processes remain present, inspect the retained referer and request payload for a stale dashboard/browser state or another unsupported plugin type and correct that caller. Do not recreate a healthy datasource, install plugins from the network at startup, silence the query error, or restart an unchanged image.",
		verify:    "Query vector(1) through warp-mimir and a bounded count_over_time query through warp-loki via Grafana /api/ds/query on every active exact-edge generation, confirm the request's dashboard or client now names a registered type and Logs Drilldown selects var-ds=warp-loki, observe a successful provisioned-rule evaluation, and require zero new grafana-plugin-unregistered lines after log-ingestion delay.",
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
		mechanism: "Three paths can create this aftermath. A legacy full-fleet reconciler overwrites live mirror traffic with an old absolute SET or DEL snapshot. On the page-local additive path, a PostgreSQL statement fixes its page snapshot before the reservation query runs; a live mirror update visible before the later Redis GET can be corrected back toward that stale snapshot. At the expiry boundary, an uncertain post-commit Redis pipeline can either omit a reservation increment or replay a release after Redis applied it but its response was lost. The old scheduled scan then stopped selecting the balance at end_time even though CloseExpiredContracts deliberately kept final-interval contracts open for at least five more minutes, so it could not repair a short mirror before release; this is the expired-balance reconciliation blind spot. A smaller commit/post race occurs when reconciliation observes a committed settlement before its delayed Redis release. Current release Lua clamps every resulting negative atomically. This line is mutation-site aftermath, not evidence that the site independently created fleet-wide drift.",
		context:   "Correlate the line with the nearest ReconcileNetEscrow duration and aggregate correction, query taskworker, API, and Connect for the complete interval after allowing for log-ingestion delay, and retain whether clamped_to=0 was present. At the 2026-09-01 UTC boundary, 425 contracts on 20 balances were all created after the last pass, reserved 6,937,052,501 bytes, and closed after every balance ended. Under one Redis release per durable settlement, reverse reconstruction yields a wave-start shortfall of 586,862,592 bytes, exactly the sum of 52 clamped negative results. The old client could retry an uncertain pipeline as a whole, however, so that reconstruction cannot distinguish a missed/partial create mirror from a replayed release. No Redis restart, failover, eviction, or Connect/API process restart occurred. The cohort excludes the preceding reservation-page snapshot race but preserves this write-outcome ambiguity. Earlier database-wide statement deltas cannot be attributed to an exact container when immutable artifact provenance excludes that query. Samples retain the non-sensitive site while redacting balance and contract ids.",
		action:    "Do not manually zero/delete individual mirrors or invoke reconciliation. Confirm the exact executor, immutable source revision, reservation statement shape and timing, checked mirror-pipeline results, a command-and-callback single-attempt client for non-idempotent mutations, page-local additive semantics, the non-current-open reconciliation fix, and atomic release Lua. Deploy migration 601 plus the unsettled-partial query and these mutation/reconciliation fixes. If matched reversals persist on fast pages after that exact artifact is proven, add durable per-balance fencing/versioning rather than replaying Redis increments.",
		verify:    "After allowing for log-ingestion delay, require unsettled-partial pages below 1 second; an index-bounded pass over every non-current balance with outcome-NULL escrow; one scheduled run below 120 seconds with aggregate correction below 256GiB and back in the ordinary sub-GiB band; and zero netescrow-negative lines from taskworker, API, and Connect through the next balance-expiry and close interval. After rollout, any residual race line must say clamped_to=0. A later key read must be absent when there are no new reservations, or exactly equal the current PostgreSQL open-reservation sum when legitimate work recreated it; key presence alone does not disprove the atomic clamp.",
	},
	{name: "netescrow-mirror-write", re: regexp.MustCompile(`\[netescrow\]mirror write failed after`),
		rateThreshold: 1, tier: tierWarn, playbook: "SIGNALS.md 5.11",
		meaning:   "a PostgreSQL escrow state change committed but its following non-idempotent Redis mirror pipeline returned an error, so the Redis result is unknown until source-of-truth reconciliation",
		mechanism: "Reservation, settlement, quarantine release, and reconciliation all mutate a derived Redis counter after reading or committing authoritative PostgreSQL state. The old paths discarded some pipeline results and used a retry-capable client; a lost response could therefore hide either partial application or a whole-pipeline replay. The checked path emits this line, stops after one command attempt, and raises the error. Reconciliation also covers affected balances after their validity window ends because CloseExpiredContracts intentionally leaves final-interval contracts open for at least five minutes.",
		context:   "One line is a correctness precursor, not merely Redis log noise and not proof that every command in the pipeline failed. The site field distinguishes reservation, settle, quarantine release, and reconciliation. Never replay INCRBY, DECRBY, or additive correction blindly after an uncertain response. PostgreSQL open escrow is the source of truth and the next independently recomputed reconciliation is the repair path.",
		action:    "Inspect the exact service generation, mutation site, and Redis error, then confirm the next ReconcileNetEscrow run includes current balances plus non-current balances with open escrow. Deploy checked single-attempt mirror mutations and the non-current-open reconciliation fix; do not manually change the key or rerun the failed mutation.",
		verify:    "The next scheduled pass uses the unsettled partial index, reconciles any expired balance that still owns outcome-NULL escrow, finishes below 120 seconds, and the following close interval emits zero netescrow-negative lines. The mirror equals the PostgreSQL open-reservation sum before release.",
	},
	{name: "pg-client-capacity", re: regexp.MustCompile(`server login has been failing, cached error: sorry, too many clients already`),
		rateThreshold: 1, tier: tierPage, playbook: "SIGNALS.md §1.3a and §2.11",
		meaning:   "PgBouncer could not open a PostgreSQL server connection because the database had exhausted the connection slots available to the configured login role",
		mechanism: "PostgreSQL rejected a new backend at max_connections while PgBouncer cached the failure under server_login_retry. A request that times out during a stalled transaction or COMMIT can make PgBouncer discard the uncertain server session and open a replacement while the old backend is still unwinding. With 32 independent PgBouncer processes, that replacement overlap plus direct maintenance sessions can fill the normal-role ceiling even when each local pool is within its own limit.",
		context:   "The raw line rate is diagnostic amplification, not unique rejected PostgreSQL sessions: one failed request can emit both trace.go Unexpected error and a router unhandled-error record, and both carry goroutine-shaped JSON that the generic panic class previously counted. This class takes precedence over panic. It is distinct from query_wait_timeout (server sessions all busy) and a :6432 client-write timeout (request may not reach PgBouncer). The 2026-09-01 control found 60-66-second COMMITs, WALSync/WALInsert/WALWrite waits, client-loss records, and replacement connection churn during repeated legacy reindex attempts; PostgreSQL never restarted. A later idle snapshot was recovery turnover, not evidence that idle retention caused the burst.",
		action:    "Run §1.3a through direct 5432 and split active, young idle-in-transaction, idle, and starting owners. Correlate them with wait events, completed PostgreSQL COMMIT latency, and PgBouncer connection logs or SHOW POOLS where permitted. If db-maintenance-legacy-reindex is present, wait for pg_stat_progress_create_index to become empty, then deploy a clean Taskworker containing current-main commits 908a8b2c and d8392c83; do not interrupt a live rebuild implicitly. Remove the proved upstream stall or leak before tuning pools. Do not raise max_connections first, restart PostgreSQL/PgBouncer, or mass-terminate sessions; this host's large work_mem makes a blind slot increase a memory-risk change.",
		verify:    "For ten minutes through the workload that triggered the burst, normal-role connection headroom stays above 25%, direct 5432 remains available, completed COMMIT latency and WAL waits return to their ordinary band, and neither pg-client-capacity nor query_wait_timeout recurs. Correlate unique request failures separately from diagnostic lines.",
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
	{name: "circle-transfer-admission-failed", re: regexp.MustCompile(`\[circlec\]\[transfer-admission\] failed closed`),
		rateThreshold: 1, tier: tierWarn, playbook: "SIGNALS.md §2.14", redactIDs: true,
		meaning:   "the Taskworker could not obtain the shared Circle transfer admission and deliberately returned before the financial POST",
		mechanism: "The Redis-time rolling gate failed or the task context ended while waiting. Fail-closed behavior prevents an uncounted, ambiguous transfer submit; bypassing the gate would recreate the fleet burst the limiter exists to contain.",
		context:   "A deploy drain can cancel a waiter once. Repetition outside a drain implicates Redis liveness/latency or a task context exhausted by admission pressure. The durable Circle idempotency key is retained.",
		action:    "Correlate §2.14 admission-error metrics with exact Taskworker drain state and Redis health. Repair that boundary while keeping the gate fail closed; do not manually replay the payment, pull its task forward, or loosen the three-per-second ceiling.",
		verify:    "Admission-error metrics and this log class remain zero for two five-minute windows, Redis is healthy, payout idempotency keys are stable, and no Circle 429 occurs.",
	},
	// decoded from the taskworker novel class 2026-07-18: the payout wallet is
	// out of funds — a finance action, not an api bug
	{name: "payout-wallet-insufficient", re: regexp.MustCompile(`asset amount owned by the wallet is insufficient|insufficient token balance .* in wallet`),
		rateThreshold: 5, tier: tierWarn, playbook: "SIGNALS.md §4",
		meaning:   "the payout wallet balance cannot cover pending payouts (USDC); AdvancePayment remains pending until finance/ops restores liquidity or pauses payouts",
		mechanism: "The payment processor rejected a submit because the configured source wallet lacks enough token balance. Each affected AdvancePayment row remains pending and retries on the task system's consecutive-error backoff with a one-hour nominal cap. Current task code disperses saturated retries across 30–90 minutes with a one-hour mean; older code used only two seconds of jitter and preserved outage-created waves. N parked rows still produce roughly N canonical task attempts per hour on average, but one attempt normally emits both a Circle-client diagnostic and a task-evaluator line.",
		context:   "This is primarily an operational liquidity boundary, not an API or PostgreSQL defect. The displayed line rate measures diagnostic amplification; wallet_insufficient_events is the exact-replay-deduplicated logical-attempt count and still is not the number of unique payouts. Proportional capped jitter contains synchronized processor bursts but cannot create wallet liquidity; accelerating retries only increases noise and load. A software release cannot fund the custodial wallet, and deleting task rows would discard owed payouts.",
		action:    "Finance/ops must fund the exact network/token payout wallet identified in protected source logs, or pause payouts using the supported operational control until it is funded. Do not delete or manually replay pending_task rows, rotate payment idempotency keys, or loosen the retry cap.",
		verify:    "First use §8.12 to verify every taskworker block's source/digest identity. An artifact from an intentional local checkout containing current-main server commit 66525afc includes proportional retry jitter, the shared Circle transfer gate, and complete fail-closed telemetry; deploy it only to blocks that lack the runtime capabilities. Then use §2.14 to prove complete admission metrics, zero fail-closed errors, fewer than four canonical attempts/second, and zero processor 429s for a full 90-minute retry window. After funding or an intentional pause/resume, allow the same window plus ingestion delay for AdvancePayment wallet-insufficient rows and this log rate to converge to zero without manual row changes or duplicate Circle transfers.",
		redactIDs: true,
		canonical: &logCanonical{
			eventRe: regexp.MustCompile(`\[task\.go:[0-9]+\]`),
			name:    "wallet_insufficient_events",
		},
		burst: &logBurst{
			name:      "payout-retry-microburst",
			eventRe:   regexp.MustCompile(`\[task\.go:[0-9]+\]`),
			threshold: 4,
			tier:      tierWarn,
			meaning:   "four or more distinct AdvancePayment wallet-rejection attempts landed in one second, the exact short-window shape that preceded live Circle 429s",
			mechanism: "The old capped task backoff added only 0–2 seconds of jitter, so outage-created rows retained second-scale cohorts across hourly retries. Proportional 30–90-minute jitter removes that deterministic wave but independent random choices still cannot impose a fleet-wide per-second ceiling. The standing tailer counts only task evaluator lines (one canonical line per attempt), groups their embedded timestamps by second, and de-duplicates exact replayed lines before computing the peak.",
			context:   "This is a deployable software-amplification alert, separate from the operational liquidity alert. Circle documents a default five POST requests/second for Wallets API endpoints. At 07:12:48Z on 2026-09-01, five wallet rejections completed and a sixth transfer request received 429; four of those five rejections came from blocks whose exact executable already contained proportional jitter. Three more rejections and another 429 followed at 07:12:49Z. That post-deployment control proves random dispersion alone lacks a hard ceiling. Current-main server commit 14928f69 adds an atomic Redis-time rolling gate of three transfer submits/second, leaving two requests/second of headroom. The four-attempt threshold remains the pre-gate incident discriminator and the post-gate invariant.",
			action:    "Use §8.12 to verify every taskworker's source/digest identity and deploy an artifact from an intentional local checkout containing current-main server commit 66525afc only to blocks that lack its runtime capabilities. Preserve normal backoff and payment idempotency keys; do not accelerate, manually replay, or delete tasks. If every block already contains the gate, use §2.14 admission errors/waits and all Circle request sources before changing its ceiling.",
			verify:    "Every newest Taskworker exports the complete §2.14 admission metrics from an artifact containing current-main server commit 66525afc; for one full 90-minute retry window, admission errors and payment-processor-rate-limit events remain zero, peak_task_attempts_per_second stays below 4, and payment idempotency keys do not change. Ordinary deferrals prove the gate is working. Funding or pausing the wallet remains a separate operational verification.",
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
		context:   "One failure is normally logged at the Circle client and again by the task evaluator, so diagnostic lines are not unique attempts. Historical bounded controls on a pre-dispersion taskworker found the same six payments recurring at the same minute in each UTC hour. That history proves persistent invalid wallet selection after the safe typed reset, but it is not a statement about the currently deployed artifact and this alert intentionally does not infer runtime provenance from a historical version. Retry dispersion cannot repair wallet data; the separate payout-retry-microburst finding owns any remaining software-rollout diagnosis.",
		action:    "Correct the affected network's payout wallet through the supported account API so its address matches its declared payout chain. This alert is an account-owner/operations action only. If payout-retry-microburst also fires, follow that separate finding's artifact-provenance and 90-minute observation gate; do not redeploy taskworker solely from this invalid-destination alert. Do not edit account_payment or pending_task rows, manually release attempts, rotate processor idempotency keys, or invent a replacement wallet without account-owner or operator authority.",
		verify:    "After the payout wallet is corrected, the next natural retry selects it with a fresh key released only by the prior definitive rejection, completes without a duplicate transfer, invalid_destination_events remains zero, and the durable processor-invalid-destination count converges to zero within 90 minutes plus log-ingestion delay.",
		redactIDs: true},
	{name: "payment-processor-rate-limit", re: regexp.MustCompile(`Bad status: 429 Too Many Requests.*API rate limit error`),
		rateThreshold: 1, tier: tierWarn, playbook: "SIGNALS.md §1.2 and §5.7",
		canonical: &logCanonical{
			eventRe: regexp.MustCompile(`\[task\.go:[0-9]+\]`),
			name:    "processor_rate_limit_events",
			correlation: &logCanonicalCorrelation{
				burstClass: "payout-wallet-insufficient",
				eventName:  "coincident_wallet_attempts",
				threshold:  4,
			},
		},
		meaning:   "Circle refused a payment API request because the shared processor identity crossed a short-window request limit",
		mechanism: "One failed AdvancePayment attempt is normally logged once by the Circle client and again by the task evaluator, so this diagnostic line rate is not a unique-submit rate. Historical pre-jitter cohorts repeatedly placed five or six distinct wallet-insufficient attempts in one second with a 429. The decisive post-jitter recurrence at 07:12:48Z placed five wallet rejections and a sixth 429 in one source second; four of those five rejections were on exact executables already proven to contain proportional jitter. Independent random retry times reduce average synchronization but cannot enforce the processor's hard boundary.",
		context:   "A 429 is an ambiguous submit outcome: it is not safe evidence that Circle created no transaction, so the existing payment idempotency key must be retained. Circle documents a default five POST requests/second for Wallets API endpoints, matching the observed sixth-request boundary without proving a private account override. The monitor joins exact-replay-deduplicated evaluator records by normalized source second and retains wallet cohort counts across its bounded reconciliation/drain boundary. This is not a general Circle outage diagnosis. The durable cause breakdown stores only each row's latest error, so its rate-limit count can fall while a different row receives a new 429.",
		action:    "Do not manually retry, delete, or pull payment tasks forward. Use §8.12 to verify every taskworker's source/digest identity and deploy an artifact from an intentional local checkout containing current-main server commit 66525afc only to blocks that lack its fleet-wide Redis-time transfer gate and fail-closed telemetry. If every block contains it, inspect §2.14 fail-closed errors and admission pressure plus all other Circle request sources before changing the conservative three-per-second ceiling.",
		verify:    "Every newest Taskworker contains current-main server commit 66525afc and exports all §2.14 admission metrics; admission errors and processor-rate-limit events stay zero, canonical payout attempts stay below four/second for a full 90-minute retry window, and retries preserve their original idempotency keys. Any remaining 429 must be correlated with all Circle request sources and the account's authoritative quota rather than inferred from a minute rate.",
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
	{name: "db-maintenance-legacy-reindex", re: dbMaintenanceLegacyReindexRe,
		groupBy:       dbMaintenanceLegacyReindexLogGroup,
		rateThreshold: 1, tier: tierPage, playbook: "SIGNALS.md §2.2a",
		meaning:   "a Taskworker selected a table that current maintenance policy excludes and entered the legacy full-table concurrent-reindex path",
		mechanism: "Before current-main server commits 908a8b2c and d8392c83, the daily hash rotation could select these large or high-churn tables, and incomplete-index cleanup ran only after the whole rotation. An interrupted rebuild can leave numbered _ccnew/_ccold artifacts; the same durable DbMaintenance epoch can then be lease-recovered and launch another full-table rebuild before reaching cleanup. The current policy excludes these tables, keeps cleanup adjacent to every permitted rebuild, and preserves task ownership when only the pooled timestamp refresh stalls. Stable patch IDs prove these commits are patch-identical to the former 7676014f and abfd976b hashes after main was rewritten.",
		context:   "The exact start line proves legacy selection and entry into the call path; because old code logs before opening the maintenance connection, it does not by itself prove that PostgreSQL began the statement, that the operation completed or remains active, or that it created any particular artifact. It pages because two same-day contract_close retries independently drove hundreds of ordinary backends active, exhausted PostgreSQL admission, lost the pooled task heartbeat, left another invalid index, and reclaimed the same durable task into a new retry. Correlate the table with pg_stat_progress_create_index, the reindex-debris lower-bound/active-candidate split, direct client-slot capacity, and the exact DbMaintenance task/worker lifecycle. A shift between those two byte buckets is not cleanup.",
		action:    "Inspect the active database operation before changing Taskworker. Do not let a rollout or manual cancellation implicitly interrupt a protected rebuild. Operations must explicitly choose whether a bounded active operation should finish or be canceled; cancellation is a database mutation and requires authorization. Once pg_stat_progress_create_index is empty, satisfy §8.13 identity observation and deploy Taskworker from an intentional local checkout containing current-main server commits 908a8b2c and d8392c83. Clean existing artifacts separately with the supported cleanup-only maintenance command and explicit database-maintenance authorization; never wildcard-drop _ccnew/_ccold indexes.",
		verify:    "§8.12 proves every active Taskworker's source/digest identity and runtime behavior for both fixes; one complete maintenance epoch emits no legacy start line and pg_stat_progress_create_index never names an excluded table; after authorized cleanup the reindex-debris probe reports zero inactive artifacts; and no related DataFileExtend/WAL wait cluster or Connect login-timeout wave recurs.",
	},
	{name: "framer-message-too-large", re: framerRejectRe,
		sample:        framerRejectLogSample,
		rateThreshold: 1, tier: tierWarn, playbook: "SIGNALS.md §14.6",
		meaning:   "a reliable Connect carrier exceeded the H1 framer admission envelope; retries of the same immutable Pack cannot refill that route",
		mechanism: "The integrated post-quantum TLS handshake carrier can exceed a legacy component-estimated cap even when every ordinary data group stays below its target. A write rejection strands the sender; a read rejection closes the receiving route. Multi-window retry remains live but cannot make an unchanged oversized carrier fit, so a hosted H1-only provider window can drain to platform-unreachable and retire exits carrying active proxy flows.",
		context:   "This is a transport admission defect, not evidence that the origin, public proxy listener, rate limiter, policy route, TUN handoff, or WireGuard socket dropped the flow. Preserve direction plus message and cap lengths; correlate the same interval with window target/readiness and exit retirement.",
		action:    "After §8.13 can read the exact Warpctl identity, deploy Connect and Proxy artifacts from intentional local checkouts containing current-main server commit c1403f16 and Connect module commit 096414ac on both ends of the hosted H1 path. Keep ordinary data coalescing and per-device memory admission bounded; do not suppress the rejection or make the buffer unbounded.",
		verify:    "§8.12 proves every active Connect and Proxy block runs the compatible commit pair with valid source/digest identities; no framer-message-too-large line recurs; provider windows stay at target without platform-unreachable; and three sustained HTTP/SOCKS/WireGuard overlap campaigns pass.",
	},
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
var lokiTailBackendAddrRe = regexp.MustCompile(`[[:space:]]addr=("[^"]+"|[^[:space:]]+)`)
var warpLogIdentityRe = regexp.MustCompile(`^\[([^\]]+)\]\[([^\]]+)\]\[([^\]]+)\]\[cid:([^\]]+)\]`)
var dbMaintenanceLegacyReindexRe = regexp.MustCompile(`\[db\]maintenance reindex\[[0-9]+/[0-9]+\] (client_reliability(?:_p[0-9]{8})?|contract_close|network_client_location_reliability|network_client_connection|transfer_contract|transfer_escrow|transfer_escrow_sweep)[[:space:]]*$`)

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

// lokiTailBackendEOFLogGroup turns the address added by the patched Loki
// querier into an actionable alert frame. Older rolling-deployment lines omit
// addr and intentionally remain service-framed rather than inventing backend
// attribution from the Grafana host that happened to run the querier.
func lokiTailBackendEOFLogGroup(line string) string {
	match := lokiTailBackendAddrRe.FindStringSubmatch(line)
	if len(match) < 2 {
		return ""
	}
	address := strings.Trim(match[1], `"`)
	if address == "" {
		return ""
	}
	return "backend=" + address
}

// dbMaintenanceLegacyReindexLogGroup retains the excluded table that the old
// maintenance path selected. The start-only regexp deliberately rejects the
// later "reindex took" completion line and the fixed table/step log format.
func dbMaintenanceLegacyReindexLogGroup(line string) string {
	match := dbMaintenanceLegacyReindexRe.FindStringSubmatch(line)
	if len(match) < 2 {
		return ""
	}
	return "table=" + match[1]
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

var (
	grafanaPluginRequestRe = regexp.MustCompile(`\bmethod=[^[:space:]]+[[:space:]]+path=[^[:space:]]+[[:space:]]+status=[0-9]+`)
	grafanaPluginRefererRe = regexp.MustCompile(`\breferer=(?:"[^"]*"|[^[:space:]]+)`)
	grafanaPluginMarkerRe  = regexp.MustCompile(`errorMessageID=plugin\.notRegistered|\[plugin\.notRegistered\]|plugin\.notRegistered|plugin not registered`)
)

// grafanaPluginUnregisteredLogSample retains the request discriminator rather
// than only the long Warp/Grafana identity prefix. A generic
// plugin.notRegistered line is not enough to name the missing plugin: the
// request path and referer tell an operator which caller must be compared with
// the dedicated warp-mimir and warp-loki controls.
func grafanaPluginUnregisteredLogSample(line string) string {
	marker := grafanaPluginMarkerRe.FindString(line)
	if marker == "" {
		marker = "plugin_marker=absent"
	}
	parts := make([]string, 0, 3)
	if request := grafanaPluginRequestRe.FindString(line); request != "" {
		parts = append(parts, request)
	}
	if referer := grafanaPluginRefererRe.FindString(line); referer != "" {
		parts = append(parts, referer)
	}
	parts = append(parts, marker)
	if len(parts) == 1 {
		return truncateLinePreservingSuffix(line, marker)
	}
	return truncateLinePreservingSuffix(strings.Join(parts, " "), marker)
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

// framerRejectLogSample drops the Warp identity and any customer correlation
// fields while retaining the only actionable dimensions: direction, observed
// carrier size, and configured admission envelope.
func framerRejectLogSample(line string) string {
	if sample := framerRejectRe.FindString(line); sample != "" {
		return sample
	}
	return "[framer][reject]"
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
	burstSecondCounts map[string]int
	burstPeaks        map[string]int
	burstPeakSeconds  map[string]string
	burstEventTotals  map[string]int
	burstSamples      map[string]string
	burstSeen         map[[sha256.Size]byte]struct{}
	burstSeenPrevious map[[sha256.Size]byte]struct{}
	// Recent burst seconds survive cadence drains so a provider result that is
	// ingested or reconciled in the next minute can still join to the attempts
	// that caused it. Entries remain bounded by the standing overlap retention.
	burstRecentSecondCounts map[string]int
	burstRecentSecondSeen   map[string]time.Time
	canonicalCounts         map[string]int
	canonicalSecondCounts   map[string]int
	canonicalSeen           map[[sha256.Size]byte]struct{}
	canonicalSeenPrevious   map[[sha256.Size]byte]struct{}
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
	// warpctl owns WebSocket reconnects internally, so those path failures do
	// not end the child and cannot increment restartCount. Capture only the
	// narrow structured transport diagnostic on stderr; remote service records
	// remain exclusively on stdout.
	transportRouteEvents map[string]tailTransportRouteEvent
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
		service:                 service,
		env:                     env,
		clock:                   clock,
		startedAt:               startedAt,
		classCounts:             map[string]int{},
		classSamples:            map[string]string{},
		classTargets:            map[string]string{},
		burstSecondCounts:       map[string]int{},
		burstPeaks:              map[string]int{},
		burstPeakSeconds:        map[string]string{},
		burstEventTotals:        map[string]int{},
		burstSamples:            map[string]string{},
		burstSeen:               map[[sha256.Size]byte]struct{}{},
		burstSeenPrevious:       map[[sha256.Size]byte]struct{}{},
		burstRecentSecondCounts: map[string]int{},
		burstRecentSecondSeen:   map[string]time.Time{},
		canonicalCounts:         map[string]int{},
		canonicalSecondCounts:   map[string]int{},
		canonicalSeen:           map[[sha256.Size]byte]struct{}{},
		canonicalSeenPrevious:   map[[sha256.Size]byte]struct{}{},
		standingSeen:            map[[sha256.Size]byte]time.Time{},
		novelCounts:             map[string]int{},
		transportRouteEvents:    map[string]tailTransportRouteEvent{},
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
	return self.env.runner.warpctlStream(
		ctx,
		&tailTransportDiagnosticWriter{tailer: self},
		"logs", self.env.cfg.env, self.service, "--since=1s", "-f",
	)
}

var tailIPv6RouteLossPattern = regexp.MustCompile(
	`^([0-9]{4}/[0-9]{2}/[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}).*Tail read error .*->\[([0-9A-Fa-f:]+)\]:[0-9]+: read: no route to host`,
)

type tailTransportRouteEvent struct {
	address string
	count   int
	first   string
	last    string
}

// tailTransportDiagnosticWriter reconstructs stderr lines across arbitrary
// os/exec Write chunks. It intentionally recognizes only explicit warpctl
// transport diagnostics; generic error text must not become a remote service
// finding or a recursive log classifier input.
type tailTransportDiagnosticWriter struct {
	lock    sync.Mutex
	pending string
	tailer  *logTailer
}

const tailTransportDiagnosticMaxLine = 64 * 1024

func (w *tailTransportDiagnosticWriter) Write(p []byte) (int, error) {
	w.lock.Lock()
	defer w.lock.Unlock()

	w.pending += string(p)
	for {
		newline := strings.IndexByte(w.pending, '\n')
		if newline < 0 {
			break
		}
		line := strings.TrimSuffix(w.pending[:newline], "\r")
		w.pending = w.pending[newline+1:]
		w.tailer.recordTransportDiagnostic(line)
	}
	if len(w.pending) > tailTransportDiagnosticMaxLine {
		// A malformed local diagnostic must not create an unbounded monitor
		// allocation. Discard it; stdout has an independent 1 MiB guard.
		w.pending = ""
	}
	return len(p), nil
}

func (self *logTailer) recordTransportDiagnostic(line string) {
	match := tailIPv6RouteLossPattern.FindStringSubmatch(strings.TrimSpace(line))
	if len(match) != 3 {
		return
	}
	address := normalizeTransportIPv6(match[2])
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	event := self.transportRouteEvents[address]
	event.address = address
	event.count++
	if event.first == "" || match[1] < event.first {
		event.first = match[1]
	}
	if event.last == "" || event.last < match[1] {
		event.last = match[1]
	}
	self.transportRouteEvents[address] = event
}

func normalizeTransportIPv6(value string) string {
	value = strings.TrimSpace(value)
	if address, err := netip.ParseAddr(value); err == nil && address.Is6() {
		return address.String()
	}
	return strings.ToLower(value)
}

func (self *logTailer) drainTransportRouteEvents() []tailTransportRouteEvent {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	addresses := make([]string, 0, len(self.transportRouteEvents))
	for address := range self.transportRouteEvents {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	events := make([]tailTransportRouteEvent, 0, len(addresses))
	for _, address := range addresses {
		events = append(events, self.transportRouteEvents[address])
	}
	self.transportRouteEvents = map[string]tailTransportRouteEvent{}
	return events
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
					if second := logTimestampSecond(line); second != "" {
						self.canonicalSecondCounts[key+"\x00"+second] += 1
					}
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
						self.burstRecentSecondCounts[secondKey] += 1
						self.burstRecentSecondSeen[secondKey] = now
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

func (self *logTailer) canonicalCorrelationLocked(c logClass, key string) (string, string) {
	if c.canonical == nil || c.canonical.correlation == nil {
		return "", ""
	}
	correlation := c.canonical.correlation
	sourcePrefix := key + "\x00"
	correlatedKey := correlation.burstClass
	// A grouped canonical class can correlate only within the same frame. The
	// current payout classes are service-wide, but retaining this suffix makes
	// the helper safe for a future route- or host-framed class.
	if separator := strings.IndexByte(key, '\x00'); separator >= 0 {
		correlatedKey += key[separator:]
	}

	sourceSeconds := 0
	qualifyingSeconds := 0
	coincidentEvents := 0
	peakCoincidentEvents := 0
	for secondKey, count := range self.canonicalSecondCounts {
		if count <= 0 || !strings.HasPrefix(secondKey, sourcePrefix) {
			continue
		}
		second := strings.TrimPrefix(secondKey, sourcePrefix)
		if second == "" {
			continue
		}
		sourceSeconds++
		coincident := self.burstRecentSecondCounts[correlatedKey+"\x00"+second]
		coincidentEvents += coincident
		peakCoincidentEvents = max(peakCoincidentEvents, coincident)
		if correlation.threshold <= coincident {
			qualifyingSeconds++
		}
	}

	observed := fmt.Sprintf(
		" correlated_source_seconds=%d correlated_cohort_seconds=%d %s=%d peak_%s_per_second=%d correlation_threshold=%d/s",
		sourceSeconds,
		qualifyingSeconds,
		correlation.eventName,
		coincidentEvents,
		correlation.eventName,
		peakCoincidentEvents,
		correlation.threshold,
	)
	if sourceSeconds == 0 {
		return observed, "\nsource-second correlation: no new canonical evaluator source second was present; diagnostic replay is not a new provider event"
	}
	evidence := fmt.Sprintf(
		"\nsource-second correlation: %d/%d %s source second(s) shared at least %d canonical %s attempt(s); %d attempt(s) shared those seconds, peaking at %d/s",
		qualifyingSeconds,
		sourceSeconds,
		c.name,
		correlation.threshold,
		correlation.burstClass,
		coincidentEvents,
		peakCoincidentEvents,
	)
	return observed, evidence
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
				correlationObserved, correlationEvidence := self.canonicalCorrelationLocked(c, key)
				observed += correlationObserved
				canonicalEvidence += correlationEvidence
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
	self.canonicalSecondCounts = map[string]int{}
	self.canonicalSeenPrevious = self.canonicalSeen
	self.canonicalSeen = map[[sha256.Size]byte]struct{}{}
	self.novelCounts = map[string]int{}
	self.novelSample = ""
	now := self.clock()
	self.pruneStandingSeenLocked(now)
	for secondKey, seenAt := range self.burstRecentSecondSeen {
		if !now.Before(seenAt.Add(logReconcileRetention)) {
			delete(self.burstRecentSecondSeen, secondKey)
			delete(self.burstRecentSecondCounts, secondKey)
		}
	}

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
	// monitorRouteEvidence is enabled only by the production standing runner.
	// It is a narrow, fail-soft local discriminator: a remote tail transport
	// event must remain visible even when the monitor host cannot expose recent
	// router-advertisement state. Tests inject it explicitly.
	monitorRouteEvidence tailTransportMonitorRouteEvidenceCollector
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
	routeEvents := map[string]*tailTransportRouteAggregate{}
	now := time.Now()
	if env != nil && env.now != nil {
		now = env.now()
	}
	for i, tailer := range self.tailers {
		findings = append(findings, tailer.drainWindow()...)
		for _, event := range tailer.drainTransportRouteEvents() {
			aggregate := routeEvents[event.address]
			if aggregate == nil {
				aggregate = &tailTransportRouteAggregate{
					address:  event.address,
					services: map[string]struct{}{},
				}
				routeEvents[event.address] = aggregate
			}
			aggregate.count += event.count
			aggregate.services[tailer.service] = struct{}{}
			if aggregate.first == "" || event.first < aggregate.first {
				aggregate.first = event.first
			}
			if aggregate.last == "" || aggregate.last < event.last {
				aggregate.last = event.last
			}
		}

		lastLine, restarts, scanErrors := tailer.healthSnapshot()
		restartDelta := restarts - self.lastRestartCounts[i]
		self.lastRestartCounts[i] = restarts
		findings = append(findings, tailerHealthFindings(tailer.service, now, lastLine, restartDelta, scanErrors)...)
		enabled, startedAt, lastSuccess, lastError := tailer.reconcileSnapshot()
		if enabled {
			findings = append(findings, tailerReconcileFinding(tailer.service, now, startedAt, lastSuccess, lastError))
		}
	}
	monitorEvidence := tailTransportMonitorRouteEvidence{}
	if len(routeEvents) != 0 && self.monitorRouteEvidence != nil {
		monitorEvidence = self.monitorRouteEvidence(ctx, env, routeEvents)
	}
	findings = append(findings, tailTransportRouteFindings(env, routeEvents, monitorEvidence)...)
	return findings, nil
}

type tailTransportRouteAggregate struct {
	address  string
	count    int
	first    string
	last     string
	services map[string]struct{}
}

type tailTransportMonitorRouteEvidence struct {
	interfaceName              string
	routerLifetimeExpiredAt    time.Time
	routerLifetimeExpiredCount int
	autoconfDetachCount        int
	ipv6AbsentAt               time.Time
	ipv6RestoredAt             time.Time
}

type tailTransportMonitorRouteEvidenceCollector func(
	context.Context,
	*probeEnv,
	map[string]*tailTransportRouteAggregate,
) tailTransportMonitorRouteEvidence

const (
	monitorIPv6LogTimeLayout          = "2006-01-02 15:04:05.000"
	monitorIPv6RouteCorrelationWindow = 15 * time.Second
)

var (
	monitorIPv6LogTimestampPattern      = regexp.MustCompile(`^([0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3})`)
	monitorRouterLifetimeExpiredPattern = regexp.MustCompile(`RTADV ([[:alnum:]_.-]+): router lifetime became zero`)
	monitorAutoconfDetachedPattern      = regexp.MustCompile(`AUTOMATIC-V6 ([[:alnum:]_.-]+): all autoconf addresses detached/deprecated`)
)

// collectTailTransportMonitorRouteEvidence asks only for the few local
// configd records that can distinguish a production edge failure from the
// monitor losing its own IPv6 default router. The current durable monitor runs
// on macOS. Other platforms retain the generic exact-address battery without
// turning an unavailable platform log into a second visibility incident.
func collectTailTransportMonitorRouteEvidence(
	ctx context.Context,
	env *probeEnv,
	events map[string]*tailTransportRouteAggregate,
) tailTransportMonitorRouteEvidence {
	if runtime.GOOS != "darwin" {
		return tailTransportMonitorRouteEvidence{}
	}
	return collectTailTransportMonitorRouteEvidenceFromRunner(ctx, env, events)
}

func collectTailTransportMonitorRouteEvidenceFromRunner(
	ctx context.Context,
	env *probeEnv,
	events map[string]*tailTransportRouteAggregate,
) tailTransportMonitorRouteEvidence {
	if env == nil || env.runner == nil {
		return tailTransportMonitorRouteEvidence{}
	}
	first, last, ok := tailTransportRouteEventBounds(events)
	if !ok {
		return tailTransportMonitorRouteEvidence{}
	}
	out, err := env.runner.local(
		ctx,
		"/usr/bin/log",
		"show",
		"--style", "compact",
		"--start", first.Add(-monitorIPv6RouteCorrelationWindow).Format("2006-01-02 15:04:05"),
		"--end", last.Add(monitorIPv6RouteCorrelationWindow).Format("2006-01-02 15:04:05"),
		"--predicate", `process == "configd" AND (eventMessage CONTAINS "RTADV " OR eventMessage CONTAINS "AUTOMATIC-V6 " OR eventMessage CONTAINS "network changed:")`,
	)
	if err != nil {
		return tailTransportMonitorRouteEvidence{}
	}
	return parseTailTransportMonitorRouteEvidence(out)
}

func tailTransportRouteEventBounds(events map[string]*tailTransportRouteAggregate) (time.Time, time.Time, bool) {
	var first time.Time
	var last time.Time
	for _, event := range events {
		for _, value := range []string{event.first, event.last} {
			parsed, err := time.ParseInLocation("2006/01/02 15:04:05", value, time.Local)
			if err != nil {
				continue
			}
			if first.IsZero() || parsed.Before(first) {
				first = parsed
			}
			if last.IsZero() || last.Before(parsed) {
				last = parsed
			}
		}
	}
	return first, last, !first.IsZero() && !last.IsZero()
}

func parseTailTransportMonitorRouteEvidence(out string) tailTransportMonitorRouteEvidence {
	evidence := tailTransportMonitorRouteEvidence{}
	scanner := bufio.NewScanner(strings.NewReader(out))
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		timestampMatch := monitorIPv6LogTimestampPattern.FindStringSubmatch(line)
		if len(timestampMatch) != 2 {
			continue
		}
		observedAt, err := time.ParseInLocation(monitorIPv6LogTimeLayout, timestampMatch[1], time.Local)
		if err != nil {
			continue
		}
		if match := monitorRouterLifetimeExpiredPattern.FindStringSubmatch(line); len(match) == 2 {
			if evidence.interfaceName == "" {
				evidence.interfaceName = match[1]
			}
			if match[1] == evidence.interfaceName {
				evidence.routerLifetimeExpiredCount++
				if evidence.routerLifetimeExpiredAt.IsZero() || observedAt.Before(evidence.routerLifetimeExpiredAt) {
					evidence.routerLifetimeExpiredAt = observedAt
				}
			}
			continue
		}
		if match := monitorAutoconfDetachedPattern.FindStringSubmatch(line); len(match) == 2 {
			// A detach on another interface is not causal evidence for this
			// route loss. The default-router expiry chooses the identity.
			if evidence.interfaceName != "" && match[1] == evidence.interfaceName {
				evidence.autoconfDetachCount++
			}
			continue
		}
		if evidence.interfaceName != "" &&
			!evidence.routerLifetimeExpiredAt.IsZero() &&
			evidence.routerLifetimeExpiredAt.Before(observedAt) &&
			strings.Contains(line, "network changed:") &&
			!strings.Contains(line, "v6("+evidence.interfaceName) &&
			evidence.ipv6AbsentAt.IsZero() {
			evidence.ipv6AbsentAt = observedAt
			continue
		}
		if evidence.interfaceName != "" &&
			!evidence.routerLifetimeExpiredAt.IsZero() &&
			evidence.routerLifetimeExpiredAt.Before(observedAt) &&
			strings.Contains(line, "network changed:") &&
			strings.Contains(line, "v6("+evidence.interfaceName) &&
			evidence.ipv6RestoredAt.IsZero() {
			evidence.ipv6RestoredAt = observedAt
		}
	}
	return evidence
}

func (e tailTransportMonitorRouteEvidence) matches(event *tailTransportRouteAggregate) bool {
	if e.interfaceName == "" || e.routerLifetimeExpiredAt.IsZero() || e.ipv6AbsentAt.IsZero() || event == nil {
		return false
	}
	first, last, ok := tailTransportRouteEventBounds(map[string]*tailTransportRouteAggregate{"event": event})
	if !ok {
		return false
	}
	return !e.routerLifetimeExpiredAt.Before(first.Add(-monitorIPv6RouteCorrelationWindow)) &&
		!last.Add(monitorIPv6RouteCorrelationWindow).Before(e.routerLifetimeExpiredAt) &&
		!e.ipv6AbsentAt.Before(first.Add(-monitorIPv6RouteCorrelationWindow)) &&
		!last.Add(monitorIPv6RouteCorrelationWindow).Before(e.ipv6AbsentAt)
}

type tailTransportRouteTarget struct {
	host          string
	interfaceName string
	address       string
}

func tailTransportRouteFindings(
	env *probeEnv,
	events map[string]*tailTransportRouteAggregate,
	monitorEvidence tailTransportMonitorRouteEvidence,
) []finding {
	targets := map[string]tailTransportRouteTarget{}
	if env != nil && env.cfg != nil {
		for _, configuredHost := range env.cfg.hosts {
			for _, configured := range configuredHost.edgeIPv6 {
				address := normalizeTransportIPv6(configured.Address)
				if address == "" {
					continue
				}
				targets[address] = tailTransportRouteTarget{
					host:          configuredHost.name,
					interfaceName: configured.Interface,
					address:       address,
				}
			}
		}
	}

	addresses := make([]string, 0, len(targets)+len(events))
	seen := map[string]struct{}{}
	for address := range targets {
		seen[address] = struct{}{}
		addresses = append(addresses, address)
	}
	for address := range events {
		if _, ok := seen[address]; ok {
			continue
		}
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)

	findings := make([]finding, 0, len(addresses))
	for _, address := range addresses {
		target := targets[address]
		targetName := "unknown-edge-ipv6"
		frame := address
		if target.host != "" {
			targetName = target.host
			frame = target.interfaceName + "/" + target.address
		}
		event := events[address]
		if event == nil {
			findings = append(findings, finding{
				probeId: "monitor/visibility", tier: tierWarn,
				class: "tailer-ipv6-route-loss", target: targetName, frame: frame,
				healthy: true,
			})
			continue
		}

		services := make([]string, 0, len(event.services))
		for service := range event.services {
			services = append(services, service)
		}
		sort.Strings(services)
		correlation := "One tail observed the path failure; use same-second configured-edge and unrelated-provider IPv6 controls before deciding whether the scope was shared."
		if len(services) > 1 {
			correlation = "Multiple independent service tails converged on one exact edge address, proving a shared path event rather than failures in those services."
		}
		mechanism := "Warpctl's live-tail client received an explicit no-route transport error and reconnected internally, so the child process stayed alive and its ordinary restart counter could not expose the interruption. " + correlation + " The diagnostic alone does not distinguish monitor-side default-route loss, an upstream withdrawal or unreachable response, router neighbor resolution, or the edge interface; the exact-address battery localizes those layers."
		observed := fmt.Sprintf(
			"route_errors=%d services=%d service_sample=%s first_local=%s last_local=%s address=%s",
			event.count,
			len(services),
			strings.Join(services, ","),
			event.first,
			event.last,
			address,
		)
		evidence := "The finding is parsed only from warpctl stderr's `Tail read error ... no route to host ... Reconnecting` diagnostic. Stderr is duplicated to the operator console but remains isolated from remote service stdout, so it cannot become a service panic or novel-error alert."
		contextText := "A later pinned HTTP 200 proves recovery, not absence of the earlier interruption. Check the monitor's own IPv6 default-router/advertisement state first, then compare same-second controls to another configured edge and to an unrelated provider IPv6 prefix before attributing the failure to this edge; two prefixes behind one site router are not independent controls. Preserve disabled-host exclusions."
		action := "First inspect the monitor host's IPv6 default route and router-advertisement state at the recorded local time. If they remained stable, run the §18.1 exact-address battery: compare active services.yml with the live interface and LB unit, inspect the edge journal, and inspect the upstream router's exact neighbor and interface counters. On recurrence, capture ICMPv6 Router Advertisements locally plus the pinned SYN/ICMPv6 along the remote path. Do not restart services or change an edge address, route, firewall rule, or neighbor entry from this diagnostic alone."
		verify := "The monitor retains its IPv6 default router, the exact Vault address equals the live edge interface, three pinned HTTP/1.1 requests return 200, and all standing tails remain free of route-loss diagnostics for ten minutes while both another configured edge and an unrelated provider IPv6 prefix stay available as controls."
		if monitorEvidence.matches(event) {
			expiredAt := monitorEvidence.routerLifetimeExpiredAt.Format(monitorIPv6LogTimeLayout)
			absentAt := monitorEvidence.ipv6AbsentAt.Format(monitorIPv6LogTimeLayout)
			restoredAt := "not observed in the bounded window"
			if !monitorEvidence.ipv6RestoredAt.IsZero() {
				restoredAt = monitorEvidence.ipv6RestoredAt.Format(monitorIPv6LogTimeLayout)
			}
			mechanism = fmt.Sprintf(
				"The monitor host recorded that its IPv6 default-router lifetime on %s reached zero at %s inside the transport-failure window. The monitor then detached/deprecated its autoconfigured IPv6 state, so independent warpctl tails lost their common local route while their child processes remained alive. This proves local default-router expiration and supersedes edge attribution, but the configd record alone does not distinguish an explicit zero-lifetime Router Advertisement from missed or late refresh advertisements.",
				monitorEvidence.interfaceName,
				expiredAt,
			)
			observed += fmt.Sprintf(
				" monitor_interface=%s monitor_router_lifetime_expired=%d monitor_autoconf_detach=%d monitor_ipv6_absent=%s monitor_ipv6_restored=%s",
				monitorEvidence.interfaceName,
				monitorEvidence.routerLifetimeExpiredCount,
				monitorEvidence.autoconfDetachCount,
				absentAt,
				restoredAt,
			)
			evidence += fmt.Sprintf(
				" The monitor's bounded configd record independently reports that the stored router lifetime reached zero on %s at %s, loss of that interface's IPv6 network state at %s, and %d autoconfiguration detach/deprecate transition(s).",
				monitorEvidence.interfaceName,
				expiredAt,
				absentAt,
				monitorEvidence.autoconfDetachCount,
			)
			contextText = "This event is a monitor-side first-hop Router Advertisement/default-router expiration, not evidence that the named production edge, its interface, LB, or LAN neighbor failed. A later edge HTTP 200 is expected after the monitor's IPv6 route returns. The first-hop router, its RA/failover owner, or the local path carrying its advertisements is operational infrastructure and cannot be repaired by deploying an edge service."
			action = "Inspect the monitor's local first-hop IPv6 Router Advertisement path: correlate router and RA-daemon uptime, WAN/failover state, and local-link health at the recorded time, then capture timestamped ICMPv6 type 134 traffic on the monitor interface during recurrence. Use the capture to distinguish an explicit zero-lifetime withdrawal from missed or late refresh advertisements, and repair the identified RA source or delivery path. Do not change the named production edge, its Vault address, LB, firewall, or neighbor state for this locally proven event."
			verify = "For at least 30 minutes, the monitor's stored default-router lifetime is refreshed before expiry, an unrelated-provider IPv6 control and configured edges remain reachable in the same seconds, and every standing tail remains free of route-loss diagnostics."
		}
		findings = append(findings, finding{
			probeId: "monitor/visibility", tier: tierWarn,
			class: "tailer-ipv6-route-loss", target: targetName, frame: frame, sustain: 1,
			symptom: fmt.Sprintf(
				"%d standing log transport(s) across %d service(s) briefly lost the IPv6 route to %s",
				event.count,
				len(services),
				targetName,
			),
			mechanism: mechanism,
			baseline:  "No standing observation stream reports an IPv6 no-route reconnect; every configured edge address remains externally reachable while its host identity, router neighbor, and return path stay valid.",
			observed:  observed,
			evidence:  evidence,
			context:   contextText,
			action:    action,
			verify:    verify,
			playbook:  "SIGNALS.md §18.1 and §1.5",
		})
	}
	return findings
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
