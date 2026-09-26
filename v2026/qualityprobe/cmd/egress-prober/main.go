// Command egress-prober measures what each provider's exit actually carries,
// and submits it to the operator's server. For every due provider it opens a
// tunnel pinned to that provider and runs the egress-health check through it
// (see egresshealth/): first the operator's own /ip echo -- the warm-up, whose
// answer is the provider's exit address, which the server places with its own
// GeoLite2 -- then a sample of real sites from the server's destination pool,
// every load browser-shaped and retried at spaced intervals. The run is
// submitted as the provider's egress health, and the exit address as its
// probed location. No ip-intelligence source is consulted for anything
// (GEOMAP §11.3).
//
// The same tunnel then carries an active bandwidth measurement (see
// bandwidth/) against two independent targets -- the operator's own download
// endpoint and a public CDN -- reported and stored separately, never averaged.
// Every provider is measured, not only those without passive history: the
// server's hourly byte budget is what regulates the spend, answering 429 once
// the current hour's bucket is full, so a full fleet is covered across
// successive hours rather than in one expensive pass.
//
// Beside it, on its own cadence, a blackhole sweep asks every provider the one
// cheap question -- did any traffic get through -- with the same warm-up and
// retries.
//
// The prober host never contacts a probe destination directly: every request
// egresses through a provider tunnel. The only direct calls are to the
// operator's own server (the due lists, the destination pool, the pins,
// ingest).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026/qualityprobe/bandwidth"
	"github.com/urnetwork/server/v2026/qualityprobe/confinement"
	"github.com/urnetwork/server/v2026/qualityprobe/controlplane"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
	"github.com/urnetwork/server/v2026/qualityprobe/providertunnel"
)

// Parses and validates the flags, runs the startup self-checks and fetches,
// then runs full passes on -interval and, beside them, blackhole sweeps on
// -blackhole-interval, until a signal or, with -interval 0, after one pass.
func main() {
	apiUrl := flag.String("api-url", "", "operator server api url, e.g. https://api.example.net (required)")
	platformUrl := flag.String("platform-url", "", "operator platform websocket url, e.g. wss://connect.example.net (required)")
	// The two secret flags must declare an empty default and read their env
	// var only after Parse (see envFallback below). Passing os.Getenv(...) as
	// the flag default instead makes flag.PrintDefaults render the live secret
	// as `(default "...")`, so every flag.Usage() call -- any missing required
	// flag, any parse error, and plain -h, which an operator runs routinely --
	// echoes both secrets verbatim to stderr, into journald or a CI log. That
	// would invert the README's own advice, which presents these env vars as
	// the way to keep secrets out of logs and ps.
	byJwt := flag.String("by-jwt", "", "the prober's network client jwt; prefer the UR_PROBER_BY_JWT env var, which keeps it out of ps. Leave it EMPTY to fetch it from the server's /network/prober-credential endpoint using -operator-secret, which is the unattended mode: the server mints the prober's identity in a bootstrap task and this waits for it. An explicitly supplied value always wins and is never overwritten")
	operatorSecret := flag.String("operator-secret", "", "ingest secret, must match ingest_secret in provider_egress.yml; prefer the UR_OPERATOR_SECRET env var, which keeps it out of ps (required)")
	concurrency := flag.Int("concurrency", fleetprobe.DefaultFullConcurrency, "max simultaneous provider tunnels for full health runs. A run spends most of its wall clock waiting out spaced retries, so tunnels mostly wait: size this against the host's MEMORY -- every tunnel carries its own network stack -- not its CPU or bandwidth")
	cacheTtl := flag.Duration("cache-ttl", 24*time.Hour, "do not re-probe a provider within this window. Only applies to the enumeration fallback used against a server with no due endpoint; when the server supplies the due list it owns the schedule")
	interval := flag.Duration("interval", time.Hour, "sleep AFTER a pass finishes, not a fixed period: the cycle is pass-duration + interval, so throughput is -due-limit / (pass-duration + interval) rather than -due-limit per interval. A 500-provider pass taking ~30m at -interval 1h yields ~390/hour with the prober idle two thirds of every cycle. Size it against how long a pass actually takes; 0 runs a single pass and exits")
	blackholeInterval := flag.Duration("blackhole-interval", time.Hour, "how often to sweep the WHOLE fleet with the cheap blackhole check (did any traffic get through). Separate from -interval on purpose: the full pass sweeps a fleet over hours to days, and a provider that goes dark keeps its last passing measurement for that whole window. 0 disables the sweep")
	blackholeLimit := flag.Int("blackhole-limit", 500, "providers per blackhole sweep request; the server clamps it to its own maximum")
	blackholeConcurrency := flag.Int("blackhole-concurrency", fleetprobe.DefaultBlackholeConcurrency, "simultaneous blackhole checks. A check whose loads fail waits minutes between retries, so these tunnels mostly wait too: size it against the host's memory, like -concurrency")
	blackholeTimeout := flag.Duration("blackhole-timeout", 15*time.Second, "per-attempt deadline for each of a blackhole check's loads. The tunnel's cold start is paid by the check's /ip warm-up, on -probe-timeout, so this only has to cover a load over a path that is already up")
	probeTimeout := flag.Duration("probe-timeout", 60*time.Second, "the cold-start allowance: the timeout of each run's and check's /ip warm-up, which pays for a tunnel whose path is not up yet, and the ceiling of any one request. Each egress-health load attempt gets the smaller of this and "+egresshealth.DefaultPerRequestTimeout.String())
	skipConfinementCheck := flag.Bool("skip-confinement-check", false, "DANGEROUS: start even if this host can reach a probe destination directly. Only for a one-shot manual probe on a host you know is not the operator's; a direct request measures the OPERATOR's own egress instead of the provider's, and would certify a blackholing provider as healthy")
	confinementTimeout := flag.Duration("confinement-timeout", 3*time.Second, "per-address deadline for the startup confinement self-check; a timeout counts as blocked. Must be at least "+confinement.MinTimeout.String())
	var confinementAddrs addressList
	publicApiUrl := flag.String("public-api-url", "", "the address the api answers on FROM THE PUBLIC INTERNET, used as the operator bandwidth target. This is not -api-url: control-plane calls go prober -> api directly (an internal name on docker), but the bandwidth target travels prober -> platform -> provider -> internet -> api, so it needs the public address. Empty drops the operator target and measures the cdn only")
	egressHealthAll := flag.Bool("egress-health-all", false, "run EVERY destination of the pool (that works from the provider's place) instead of a random sample. The full table is the only way this exercises CONCURRENCY, since a sample never asks the provider to carry the full parallel load a real client would; it costs about three times the requests of a sample and gives up the sample's unpredictability, so it is for inspection, not scheduled passes")
	flag.Var(&confinementAddrs, "confinement-address", "ip:port the confinement self-check should dial instead of resolving the probe hosts; repeatable. For a jail where dns is legitimately blocked: supply the address of every egress-health destination of the built-in table here and the check stays real. The host part must be an ip literal, not a name")
	dueUrl := flag.String("due-url", "", "url of the server's due-provider endpoint; empty derives <api-url>/network/provider-egress-due")
	dueLimit := flag.Int("due-limit", 100, "how many due providers to ask the server for per pass; the server clamps this to its own configured maximum, which defaults to 500 but is raised per deployment (provider_egress_due.yml)")
	shardCount := flag.Int("shard-count", 1, "number of probers sharing this server's due queue. 1 (the default) means this prober takes the whole queue. Above 1 the server hands this prober only the slice matching -shard-index, so N probers divide the fleet instead of each probing all of it -- without this the queue hands the SAME rows to every prober and adding hosts buys nothing")
	shardIndex := flag.Int("shard-index", 0, "which slice of the due queue this prober takes, 0 <= index < -shard-count. Ignored when -shard-count is 1")
	skipBandwidth := flag.Bool("skip-bandwidth", false, "do not measure provider bandwidth. The measurement rides the tunnel the health run already opened and is regulated by the server's hourly byte budget, so leaving it on is the intended mode; this is for a pass where the extra wall clock per provider matters more than the data")
	bandwidthTimeout := flag.Duration("bandwidth-timeout", bandwidth.DefaultTimeout, "per-target wall-clock cap for one bandwidth measurement. There are two targets, so this bounds the added time per provider at twice this value")
	pinRefreshInterval := flag.Duration("pin-refresh-interval", time.Hour, "how often to re-fetch the certificate pins from the server. A host the server serves a pin for is pinned; every other host is verified by ordinary WebPKI. The server re-observes them every 6h, so an hour is ample. A refresh that fails keeps the last good set, but a set that is never refreshed goes stale, so this is not disableable")
	poolUrl := flag.String("pool-url", "", "url of the server's destination pool, fetched once per pass; empty derives <api-url>"+egresshealth.PoolPath+". A pass whose fetch fails runs the built-in table, which is the pool's seed")
	ipEchoUrl := flag.String("ip-echo-url", "", "url of the operator's /ip echo, the first fetch of every run and check and the source of each provider's exit address. It is reached THROUGH the provider's tunnel, so it must be the public address: empty derives <public-api-url>"+egresshealth.IpEchoPath+", or <api-url>"+egresshealth.IpEchoPath+" when -public-api-url is empty")
	bandwidthCdnUrl := flag.String("bandwidth-cdn-url", bandwidth.CdnTestUrl, "the second bandwidth target: a size-parameterised public download (<url>?bytes=N). Measured separately from the operator target and never averaged with it -- a provider prioritising one path and not the other is only visible in two figures")
	flag.Parse()

	// Env fallback, applied only after parsing so the secret is never a flag
	// default and can never be rendered by flag.Usage(). An explicit flag
	// still wins, matching the previous precedence.
	envFallback(byJwt, "UR_PROBER_BY_JWT")
	envFallback(operatorSecret, "UR_OPERATOR_SECRET")

	var missing []string
	if *apiUrl == "" {
		missing = append(missing, "-api-url")
	}
	if *platformUrl == "" {
		missing = append(missing, "-platform-url")
	}
	// -by-jwt is deliberately not in this list any more: an empty one is
	// fetched from the server below (see fetchByJwtIfEmpty), which is the
	// whole point of the prober-credential endpoint. -operator-secret stays
	// required precisely because that fetch authenticates with it.
	if *operatorSecret == "" {
		missing = append(missing, "-operator-secret (or UR_OPERATOR_SECRET)")
	}
	if 0 < len(missing) {
		fmt.Fprintf(os.Stderr, "egress-prober: missing required flag(s): %s\n\n", strings.Join(missing, ", "))
		flag.Usage()
		os.Exit(2)
	}

	// M1: a negative -interval would otherwise fall straight into
	// time.After, which treats a negative duration as "fire immediately" --
	// degenerating the sleep loop into back-to-back passes with no pause
	// between them and no indication why. Fail fast instead.
	if *interval < 0 {
		fmt.Fprintf(os.Stderr, "egress-prober: -interval must not be negative (got %s)\n\n", *interval)
		flag.Usage()
		os.Exit(2)
	}
	if *blackholeInterval < 0 {
		fmt.Fprintf(os.Stderr, "egress-prober: -blackhole-interval must not be negative (got %s); use 0 to disable the sweep\n\n", *blackholeInterval)
		flag.Usage()
		os.Exit(2)
	}
	if 0 < *blackholeInterval {
		if *blackholeConcurrency < 1 {
			fmt.Fprintf(os.Stderr, "egress-prober: -blackhole-concurrency must be positive (got %d)\n\n", *blackholeConcurrency)
			flag.Usage()
			os.Exit(2)
		}
		if *blackholeTimeout <= 0 {
			fmt.Fprintf(os.Stderr, "egress-prober: -blackhole-timeout must be positive (got %s)\n\n", *blackholeTimeout)
			flag.Usage()
			os.Exit(2)
		}
		if *blackholeLimit < 1 {
			fmt.Fprintf(os.Stderr, "egress-prober: -blackhole-limit must be positive (got %d)\n\n", *blackholeLimit)
			flag.Usage()
			os.Exit(2)
		}
	}

	// M5: -probe-timeout 0 disables both the http.Client.Timeout and the
	// manual TLS handshake timeout in providertunnel's DialTLSContext (see
	// providertunnel/tunnel.go: `if 0 < timeout`), since a zero
	// time.Duration is the Go idiom for "no timeout" in both places. A
	// negative value is nonsensical for either. Either way, a provider that
	// simply never responds would hang a probe (and the goroutine slot it
	// holds) forever instead of freeing up for the next pass.
	if *probeTimeout <= 0 {
		fmt.Fprintf(os.Stderr, "egress-prober: -probe-timeout must be positive (got %s)\n\n", *probeTimeout)
		flag.Usage()
		os.Exit(2)
	}

	// A tiny -confinement-timeout turns the self-check off while it keeps
	// logging success, which is worse than turning it off honestly. The dial
	// budget has to be long enough that a failure means "the packet did not get
	// through"; below that every dial fails on the clock instead, every address
	// looks blocked whether or not it is, and the check reports a pass having
	// tested nothing. Observed on one unconfined host in the same second:
	// -confinement-timeout 10ms correctly reported "not confined", 1ms reported
	// "passed". Rejecting <= 0 was never enough -- the same reasoning applies to
	// any value too short to complete a connection.
	if *confinementTimeout < confinement.MinTimeout {
		fmt.Fprintf(os.Stderr, "egress-prober: -confinement-timeout must be at least %s (got %s): a shorter deadline expires before a direct connection could complete, so every probe address would look blocked whether or not it is and the self-check would report a pass having tested nothing\n\n", confinement.MinTimeout, *confinementTimeout)
		flag.Usage()
		os.Exit(2)
	}

	// The server answers 400 to a non-positive limit rather than clamping it:
	// limit=0 would come back as an empty list, indistinguishable from
	// "nothing is due". Fail here instead of once per pass.
	if *dueLimit < 1 {
		fmt.Fprintf(os.Stderr, "egress-prober: -due-limit must be positive (got %d)\n\n", *dueLimit)
		flag.Usage()
		os.Exit(2)
	}

	// Same reasoning as -due-limit. A shard outside the range makes the server
	// return an empty slice on every pass, which reads as "nothing is due"
	// rather than as a misconfiguration, so a prober would sit probing nothing
	// indefinitely. Fail at startup instead.
	if *shardCount < 1 {
		fmt.Fprintf(os.Stderr, "egress-prober: -shard-count must be at least 1 (got %d)\n\n", *shardCount)
		flag.Usage()
		os.Exit(2)
	}
	if *shardIndex < 0 || *shardCount <= *shardIndex {
		fmt.Fprintf(
			os.Stderr,
			"egress-prober: -shard-index must satisfy 0 <= index < -shard-count (got index %d, count %d)\n\n",
			*shardIndex, *shardCount,
		)
		flag.Usage()
		os.Exit(2)
	}

	// A non-positive -pin-refresh-interval would make every pass re-fetch the
	// pin set (<= 0 elapsed is always true), turning a control-plane call the
	// server expects hourly into one per pass, and a zero value reads like
	// "off" -- which it must never be, since a set that is never refreshed is
	// exactly the stale-pin problem this replaced.
	if *pinRefreshInterval <= 0 {
		fmt.Fprintf(os.Stderr, "egress-prober: -pin-refresh-interval must be positive (got %s)\n\n", *pinRefreshInterval)
		flag.Usage()
		os.Exit(2)
	}

	// Each load attempt gets the smaller of -probe-timeout and the
	// per-request floor, so a -probe-timeout under the floor would give every
	// attempt less than a request over a warm path needs -- and a warm-up
	// less than a cold one. Every load would then risk timing out and being
	// recorded as a failed site the provider had nothing to do with. Refuse to
	// start rather than manufacture that.
	if opts := egressHealthOptions(*probeTimeout, *egressHealthAll); opts.PerRequestTimeout < egresshealth.DefaultPerRequestTimeout {
		fmt.Fprintf(os.Stderr,
			"egress-prober: -probe-timeout %s leaves %s per egress-health load attempt, below the %s floor;\n"+
				"  every load would be at risk of timing out and being recorded as a failed site.\n"+
				"  Raise -probe-timeout to at least %s.\n\n",
			*probeTimeout, opts.PerRequestTimeout.Round(time.Millisecond), egresshealth.DefaultPerRequestTimeout, egresshealth.DefaultPerRequestTimeout)
		flag.Usage()
		os.Exit(2)
	}

	// A negative cache ttl makes recentlyProbed always false, silently
	// disabling the enumeration cache. Every other duration flag is validated;
	// this one degraded quietly instead, which is the opposite of how the rest
	// of this startup path treats a value it cannot honour.
	if *cacheTtl < 0 {
		fmt.Fprintf(os.Stderr, "egress-prober: -cache-ttl must not be negative (got %s); use 0 to disable the enumeration cache\n\n", *cacheTtl)
		flag.Usage()
		os.Exit(2)
	}

	// A non-positive bandwidth timeout would hand context.WithTimeout an
	// already-expired deadline, so every measurement would fail instantly and
	// still have spent a byte reservation getting there.
	if *bandwidthTimeout <= 0 {
		fmt.Fprintf(os.Stderr, "egress-prober: -bandwidth-timeout must be positive (got %s)\n\n", *bandwidthTimeout)
		flag.Usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// The first signal cancels ctx and begins a graceful wind-down (the
	// scheduler stops spawning and the in-flight probes fail fast). Undo the
	// signal capture at that point rather than at exit: NotifyContext keeps
	// swallowing signals for as long as it is registered, so without this a
	// second Ctrl-C during the wind-down would be silently discarded and the
	// operator could not force-quit a probe stuck in teardown.
	context.AfterFunc(ctx, stop)

	// The confinement self-check runs before anything else touches the
	// network. See checkConfinement.
	if *skipConfinementCheck {
		log.Printf("egress-prober: WARNING -skip-confinement-check is set: the startup confinement self-check is DISABLED.")
		log.Printf("egress-prober: WARNING if this host can reach a probe destination directly, a probe whose tunnel fails could measure the OPERATOR's own egress and certify a blackholing provider as healthy. Do not set this on the operator's deployment.")
	} else if err := checkConfinement(ctx, (&net.Dialer{}).DialContext, net.DefaultResolver.LookupHost, confinementAddrs, *confinementTimeout, append(bandwidthProbeHosts(*skipBandwidth, *bandwidthCdnUrl), egresshealth.BlackholeHosts()...)...); err != nil {
		log.Printf("egress-prober: confinement self-check failed: %s", err)
		// ErrNoEvidence is not a claim that this host is unconfined -- it is
		// the check saying it could not find out -- so the "go and confine it"
		// advice would be misleading. Its own message carries the two remedies.
		if !errors.Is(err, confinement.ErrNoEvidence) {
			log.Printf("egress-prober: this process must not be able to reach a probe destination except through a provider tunnel. Confine it (a restricted docker network, or systemd IPAddressDeny=any with IPAddressAllow for the operator server only) and start it again.")
		}
		os.Exit(1)
	}

	// Built here rather than after the jwt because the credential fetch below
	// needs it. Nothing in it depends on the jwt: it authenticates with the
	// operator secret alone, which is what makes fetching the jwt possible at
	// all.
	operator := &ingest.Client{
		ServerUrl:      *apiUrl,
		OperatorSecret: *operatorSecret,
		DueUrl:         *dueUrl,
		ShardIndex:     *shardIndex,
		ShardCount:     *shardCount,
		Http:           controlplane.NewHTTPClient(30 * time.Second),
	}

	// The jwt, if the deployment did not supply one. This sits above
	// parseByJwtClientId on purpose: a fetched jwt then travels the identical
	// path an explicitly supplied one does -- same parse, same client id, same
	// tunnel config -- so a credential the server hands over but that this
	// process cannot use still fails loudly, right here, instead of becoming a
	// fleet-wide outage that looks like nothing at all.
	//
	// It also sits below the confinement self-check, which must stay the first
	// thing that touches the network. The operator's own server is the one
	// direct call the prober is allowed to make, so this is the earliest point
	// at which it may run.
	switch err := fetchByJwtIfEmpty(ctx, byJwt, operator, credentialPollInitial, credentialPollMax); {
	case err == nil:
	case ctx.Err() != nil:
		// Interrupted while waiting for the server's bootstrap task. That is a
		// shutdown, not a broken deployment, and the same reasoning as the
		// pass-result exit codes below applies: it must not exit non-zero and
		// blame a configuration that is fine.
		log.Printf("egress-prober: interrupted while waiting for the prober credential (%v); nothing was probed", ctx.Err())
		return
	default:
		log.Printf("egress-prober: %s", err)
		os.Exit(1)
	}

	clientId, err := parseByJwtClientId(*byJwt)
	if err != nil {
		log.Fatalf("parse by-jwt client id: %s", err)
	}

	// The credential self-check runs before the first tunnel, for the same
	// reason the confinement self-check runs before the first request: a fault
	// the prober cannot detect at runtime has to be caught here or not at all.
	switch err := checkCredential(ctx, operator.Http, *apiUrl, *byJwt); {
	case err == nil:
		log.Printf("egress-prober: credential self-check passed: the server accepts the byJwt")
	case errors.Is(err, errCredentialRejected):
		log.Printf("egress-prober: credential self-check FAILED: %s", err)
		log.Printf("egress-prober: the byJwt parses but the server refuses it -- it has expired, or it predates a claim the server now enforces. Probes would not fail loudly: the operator-secret calls would keep working while every tunnel carried nothing and every provider was recorded as run_not_measured or with ok=0/N. Mint a fresh network client jwt (POST /network/auth-client) and set UR_PROBER_BY_JWT to it.")
		os.Exit(1)
	default:
		log.Printf("egress-prober: WARNING credential self-check inconclusive: %s", err)
		log.Printf("egress-prober: continuing, because being unable to check is not evidence the credential is bad. If every provider comes back with nothing measured or ok=0/N, suspect the byJwt first.")
	}

	// Pins is deliberately not set here: it is fetched from the server below
	// and read afresh on every tunnel Open, so an hourly refresh reaches the
	// next provider rather than only the next process.
	tunnelCfg := providertunnel.Config{
		ApiUrl:            *apiUrl,
		PlatformUrl:       *platformUrl,
		ByJwt:             *byJwt,
		ClientId:          clientId,
		DeviceDescription: "egress prober",
		DeviceSpec:        "egress-prober",
		Version:           "0.0.0",
	}

	// The startup fetch. This is a separate call site from the refresh in the
	// pass loop below, and the difference between them is the whole fail-closed
	// property: this one exits, the other one keeps the last good set. Folding
	// them into one helper with a "tolerate failure" flag would put both
	// behaviours one wrong argument apart.
	//
	// A failed fetch means no probing. No host is required to be pinned any
	// more -- an empty set is a valid answer, and a host without a pin is
	// verified by WebPKI -- but a pin the server serves is one it wants
	// enforced on a request that rides the provider under test, and starting
	// without the set would silently drop every one of them.
	pins := &pinSet{}
	initialPins, err := fetchPins(ctx, operator)
	if err != nil {
		log.Printf("egress-prober: %s", err)
		log.Printf("egress-prober: refusing to start. Every request rides the tunnel of the provider being measured, and a pin the server serves is what stops that provider answering for the host with someone else's certificate -- so a pin set that cannot be fetched means no probing. Check -api-url and -operator-secret, and that the server is up.")
		os.Exit(1)
	}
	pins.set(initialPins)
	// The host list of a validated pin map, for a stable log line.
	sortedHosts := func(pins map[string][]string) []string {
		out := make([]string, 0, len(pins))
		for host := range pins {
			out = append(out, host)
		}
		sort.Strings(out)
		return out
	}
	log.Printf("egress-prober: certificate pins fetched from the server for %d host(s): %s", len(initialPins), strings.Join(sortedHosts(initialPins), " "))

	// The pool is fetched once per pass, below; the echo url is fixed. The
	// echo is reached through each provider's tunnel, so it wants the api's
	// public address, the same one the operator bandwidth target uses.
	if strings.TrimSpace(*poolUrl) == "" {
		*poolUrl = fleetprobe.PoolUrl(*apiUrl)
	}
	if strings.TrimSpace(*ipEchoUrl) == "" {
		echoBase := *apiUrl
		if strings.TrimSpace(*publicApiUrl) != "" {
			echoBase = *publicApiUrl
		}
		*ipEchoUrl = fleetprobe.IpEchoUrl(echoBase)
	}
	pools := &poolSet{}
	pools.set(egresshealth.BuiltinPool())

	// Both bandwidth targets are reached through the provider tunnel, so both
	// their hosts have to be in the tunnel's allowlist (see newProber) or the
	// dialer refuses them before a byte moves.
	//
	// Note what this means for the operator target: the X-UR-Operator-Secret
	// header now traverses a provider-controlled path. The connection is
	// ordinary WebPKI-verified TLS, so a provider on the path cannot read it
	// without a mis-issued certificate for the operator's own api host -- the
	// same protection any https client has, and the same one the egress-health
	// destinations rely on. It is called out because the consequence differs:
	// that secret gates location ingest for the whole fleet, where an
	// egress-health destination gates nothing. Pinning the api host would close
	// it, but the pin is deployment-specific and not knowable here.
	bandwidthSampler := (*bandwidth.Sampler)(nil)
	bandwidthTargets := []bandwidth.Target{}
	if !*skipBandwidth {
		// The cdn target is always present; the operator target is dropped when
		// no public api url is configured, because an internal name fails from
		// the far side of the tunnel for every provider and reads as a
		// fleet-wide fault rather than a misconfiguration.
		bandwidthTargets = []bandwidth.Target{
			{Name: "cdn", Source: bandwidth.SourceCdn, Url: *bandwidthCdnUrl},
		}
		if strings.TrimSpace(*publicApiUrl) != "" {
			bandwidthTargets = append([]bandwidth.Target{
				bandwidth.OperatorTarget(*publicApiUrl, *operatorSecret),
			}, bandwidthTargets...)
		} else {
			log.Printf("egress-prober: no -public-api-url, measuring the cdn target only (the operator target cannot be reached through a provider tunnel by its internal name)")
		}
		bandwidthSampler = &bandwidth.Sampler{
			Targets: bandwidthTargets,
			Reserve: operator,
			Submit:  operator,
			Timeout: *bandwidthTimeout,
		}
	}

	dueScheduler, enumScheduler := newSchedulers(
		newProber(tunnelCfg, pins, pools, *probeTimeout, *ipEchoUrl, operator, *egressHealthAll, bandwidthSampler, bandwidth.TargetHosts(bandwidthTargets)),
		*concurrency,
		*cacheTtl,
	)

	if 0 < *blackholeInterval {
		sweeper := &blackholeSweeper{
			operator:    operator,
			tunnelCfg:   tunnelCfg,
			pins:        pins,
			poolUrl:     *poolUrl,
			ipEchoUrl:   *ipEchoUrl,
			timeout:     *blackholeTimeout,
			echoTimeout: *probeTimeout,
			concurrency: *blackholeConcurrency,
			limit:       *blackholeLimit,
		}
		go sweeper.run(ctx, *blackholeInterval)
	}

	// Fetches the pool for one pass. A pass always gets a pool: the
	// server's, or -- when it cannot be had -- the built-in table, the pool's own
	// seed, which is logged so a server that stopped serving one is visible.
	refreshPool := func(ctx context.Context, client *ingest.Client, pools *poolSet, poolUrl string) {
		pool, err := fleetprobe.LoadPool(ctx, client.Http, poolUrl, client.OperatorSecret)
		if err != nil {
			log.Printf("egress-prober: running the built-in destination table this pass: %s", err)
		}
		pools.set(pool)
	}

	// I3: a single-shot run (-interval 0) is the mode the README recommends
	// for external cron/systemd scheduling, which decides success or
	// failure purely from the exit code -- so this process must not report
	// success (exit 0) when it accomplished nothing. Two cases are treated
	// as failure below: the provider list could not be fetched at all, and
	// a pass that ran but submitted nothing while recording failures (e.g.
	// every probe hit the same wrong -platform-url, or the jwt was
	// revoked -- a permanently broken configuration, not a fluke). A pass
	// with zero providers to probe (submitted=0, failed=0) is not a
	// failure -- there was simply nothing to do.
	//
	// The long-running loop (-interval > 0) deliberately does not exit on
	// either condition: a systemd-managed process should keep retrying
	// through a transient server blip rather than dying, but it does still
	// log clearly so the failure is visible (e.g. via journalctl) even
	// though the process itself stays up.
	for {
		// The refresh call site. Unlike the startup fetch it never exits and
		// never clears the set: a server blip must not be able to stop the
		// fleet, and a stale-but-valid pin still fails closed on a real MITM.
		// The intermediate pin is what keeps a set usable across routine leaf
		// rotation in the meantime.
		refreshPins(ctx, operator, pins, *pinRefreshInterval)

		// Once per pass: the server's pool, or the built-in table when it
		// cannot be had. Every probe of the pass draws from what this sets.
		refreshPool(ctx, operator, pools, *poolUrl)

		providers, serverDriven, err := selectProviders(ctx, operator, *dueLimit, *apiUrl, *byJwt)
		if err != nil {
			log.Printf("select providers: %s", err)
			// Same reasoning as the pass result below: a fetch that failed
			// because the operator interrupted the process is a shutdown, not
			// a broken deployment, and must not exit non-zero.
			if *interval == 0 && ctx.Err() == nil {
				log.Printf("egress-prober: single-shot pass could not fetch the provider list; exiting non-zero")
				os.Exit(1)
			}
		} else {
			scheduler := enumScheduler
			if serverDriven {
				scheduler = dueScheduler
			}
			sum := scheduler.Run(ctx, providers)
			log.Printf("pass: server_driven=%t attempted=%d submitted=%d skipped=%d failed=%d not_measured=%d pool_version=%d",
				serverDriven, sum.Attempted, sum.Submitted, sum.Skipped, sum.Failed, sum.NotMeasured, pools.get().Version)
			// A pass cut short by SIGTERM is not a pass that failed. The
			// scheduler stops spawning on cancellation, but the probes
			// already in flight fail on the dead context and land in
			// sum.Failed -- so without this guard an operator pressing Ctrl-C
			// got exit 1 and a message blaming the providers, which is the
			// same misdiagnosis the cancellation fix was written to remove.
			// The shutdown is logged instead and the exit stays 0: nothing
			// about the fleet was learned either way.
			if *interval == 0 {
				switch {
				case ctx.Err() != nil:
					log.Printf("egress-prober: single-shot pass interrupted (%v) after %d submitted, %d failed; exiting zero", ctx.Err(), sum.Submitted, sum.Failed)
				case sum.Submitted == 0 && 0 < sum.Failed:
					log.Printf("egress-prober: single-shot pass submitted nothing and recorded %d failure(s); exiting non-zero", sum.Failed)
					os.Exit(1)
				}
			}
		}
		if *interval == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(*interval):
		}
	}
}

// Wires the production prober: a tunnel per provider, re-created if
// it dies part-way, the egress-health run through it, and the run, the exit
// address and every attempt reported to the operator's server.
//
// Attempts is not optional in production. The server defers a provider from
// the due queue when a probe was recently attempted, not only when one
// succeeded, so a prober that does not report leaves every unprobeable
// provider at the head of the queue forever -- see prober.ProbeOne.
//
// The pin set and the pool are passed in rather than baked into the options
// because both are refreshed while the process runs: every Open reads the
// current set and pool, so an hourly pin refresh and a per-pass pool fetch
// take effect on the next provider instead of the next restart.
func newProber(
	tunnelCfg providertunnel.Config,
	pins *pinSet,
	pools *poolSet,
	probeTimeout time.Duration,
	ipEchoUrl string,
	operator *ingest.Client,
	allDestinations bool,
	bandwidthSampler *bandwidth.Sampler,
	bandwidthHosts []string,
) *prober.Prober {
	return fleetprobe.NewFullProber(fleetprobe.FullOptions{
		TunnelConfig:    tunnelCfg,
		Pins:            pins.get,
		Pool:            pools.get,
		ProbeTimeout:    probeTimeout,
		IpEchoUrl:       ipEchoUrl,
		AllDestinations: allDestinations,
		Submit:          operator,
		Attempts:        operator,
		HealthResults:   operator,
		Bandwidth:       bandwidthSampler,
		BandwidthHosts:  bandwidthHosts,
	})
}

// The health run's geometry for -probe-timeout (see
// fleetprobe.EgressHealthOptions): -probe-timeout is the warm-up's own timeout
// -- the one fetch sized for a tunnel's cold start -- and each load attempt
// gets the smaller of it and the per-request floor.
//
// It no longer bounds the whole run. It used to: every load had one try, and a
// run that swallowed a provider's every request cost one -probe-timeout. A run
// now spans its retry schedule by design --
// up to about ten to fifteen minutes when a site keeps failing, bounded by
// egresshealth.Options.RunBudget -- because a site's momentary block must not
// become a failed site (GEOMAP §11.3). What keeps a pass moving is
// -concurrency, which is sized for tunnels that mostly wait.
func egressHealthOptions(probeTimeout time.Duration, allDestinations bool) egresshealth.Options {
	return fleetprobe.EgressHealthOptions(probeTimeout, allDestinations)
}

// Returns the scheduler for a server-driven pass and the one for
// the enumeration fallback.
//
// They are separate because the two passes have different schedules and must
// not share a cache. When the server picks the batch it owns the schedule --
// observed_at and attempt_at in its database, which survive a prober restart --
// so re-filtering that batch through an in-memory ttl would drop providers the
// server just said were due, with the two schedules disagreeing and no way to
// tell which won. The fallback pass has no server-side schedule behind it, so
// it keeps the -cache-ttl behaviour exactly as before.
func newSchedulers(p *prober.Prober, concurrency int, cacheTtl time.Duration) (dueScheduler *prober.Scheduler, enumScheduler *prober.Scheduler) {
	return &prober.Scheduler{
			Prober:      p,
			Concurrency: concurrency,
			CacheTtl:    0,
		}, &prober.Scheduler{
			Prober:      p,
			Concurrency: concurrency,
			CacheTtl:    cacheTtl,
		}
}

// The server's due-provider endpoint, injected so selectProviders
// is testable without one.
type dueLister interface {
	Due(ctx context.Context, limit int) ([]ingest.DueProvider, error)
}

// Returns the providers to probe this pass, each with the
// place it is published under, and whether the server chose them.
//
// The server's due list is authoritative when it answers: it is computed from
// observed_at and attempt_at in the database, so it survives a prober restart
// and does not re-probe the whole population after one. It carries each
// provider's place, which decides the destinations its sample may draw from;
// the enumeration fallback has none to give.
//
// Exactly one error falls back to enumerating the population locally: 404,
// meaning the server has not deployed the endpoint. Everything else surfaces.
// A 401 in particular must not fall back -- that is a wrong operator secret,
// and quietly degrading to enumeration would produce a full-looking pass whose
// every submission is rejected by that same secret, hiding the actual fault.
func selectProviders(ctx context.Context, due dueLister, limit int, apiUrl string, byJwt string) ([]prober.Provider, bool, error) {
	entries, err := due.Due(ctx, limit)
	switch {
	case err == nil:
		return fleetprobe.ProvidersFromDue(entries), true, nil
	case errors.Is(err, ingest.ErrDueUnsupported):
		log.Printf("egress-prober: the server has no %s endpoint; falling back to enumerating every provider (upgrade the server to let it schedule probes)", "/network/provider-egress-due")
		ids, err := listProviders(ctx, apiUrl, byJwt)
		return fleetprobe.ProvidersFromClientIds(ids), false, err
	case errors.Is(err, ingest.ErrUnauthorized):
		return nil, false, fmt.Errorf("the server rejected the operator secret; check -operator-secret against ingest_secret in the server's provider_egress.yml: %w", err)
	default:
		return nil, false, err
	}
}

// The port the self-check dials. Every egresshealth
// destination is https on the default port -- a pooled one too, which
// egresshealth.Destination.Validate refuses otherwise -- and https is the only
// thing a probe from this process would ever be. egresshealth's
// TestEveryDestinationIsHttpsOn443 keeps that true from the other side -- a
// destination on another port would silently fall outside this check.
const confinementPort = "443"

// Every third-party host this process reaches through a tunnel
// that is known at startup: the built-in table's egress-health destinations
// and any extra hosts the caller names -- in practice the bandwidth CDN target,
// which is third-party and configurable via -bandwidth-cdn-url. It is what the
// confinement self-check must prove unreachable directly, and what an
// operator translates into -confinement-address entries.
//
// The operator's own api host is deliberately absent, the /ip echo included:
// it is not third-party, and a deployment may legitimately allow the prober to
// reach it directly. The echo still only ever goes through a tunnel -- the
// client that fetches it has no other dialer.
//
// The server's pool is not here either, because it is fetched per pass, after
// this check has run, and changes daily. The built-in table is the pool's
// seed, so a host confined against its ~140 hosts is confined against
// essentially everything the pool will hold; a pooled host beyond it is kept
// off the direct route by the same Go-level boundary as every other request --
// the tunnel client is the only dialer a probe has.
//
// The list is derived from the table that owns it (egresshealth.
// DestinationHosts): a hand-maintained second copy drifts on the first table
// change, and the check keeps reporting a pass while no longer covering a real
// endpoint. A direct egress-health request is the one mistake that inverts
// the signal: it would pass -- the operator's own host can obviously reach
// Cloudflare and Amazon -- and so would certify a blackholing provider as
// healthy.
func probeHosts(extra ...string) []string {
	seen := map[string]bool{}
	var hosts []string
	all := append(egresshealth.DestinationHosts(), extra...)
	for _, h := range all {
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		hosts = append(hosts, h)
	}
	return hosts
}

// Returns the bandwidth CDN target's host, if the
// bandwidth probe will run at all. It is a third-party host reached through
// the tunnel, so the confinement check covers it like any other; the operator
// target is deliberately excluded, being the operator's own api.
func bandwidthProbeHosts(skipBandwidth bool, cdnUrl string) []string {
	if skipBandwidth {
		return nil
	}
	u, err := url.Parse(cdnUrl)
	if err != nil || u.Hostname() == "" {
		// A malformed -bandwidth-cdn-url is caught where it is used; the
		// self-check simply has nothing to add for it here.
		return nil
	}
	return []string{u.Hostname()}
}

// Collects the repeatable -confinement-address flag.
//
// Each value must be "ip:port" with a literal address, not a name. A name here
// would defeat the flag's entire purpose: it exists for the deployment where
// dns is blocked, so a name could not be resolved at dial time either and the
// check would be back to dialing something guaranteed to fail at resolution --
// no evidence, reported as a pass.
type addressList []string

// Implements flag.Value.
func (self *addressList) String() string { return strings.Join(*self, " ") }

// Implements flag.Value: adds one ip:port, refusing a name.
func (self *addressList) Set(v string) error {
	host, port, err := net.SplitHostPort(v)
	if err != nil {
		return fmt.Errorf("%q is not host:port: %w", v, err)
	}
	if net.ParseIP(host) == nil {
		return fmt.Errorf("%q: the host part must be an ip literal, not a name -- this flag exists for a jail where dns is blocked, and a name could not be resolved at dial time either", v)
	}
	if port == "" {
		return fmt.Errorf("%q: a port is required", v)
	}
	*self = append(*self, v)
	return nil
}

// Refuses to let the prober start unless a direct connection
// to every probe destination it knows of fails.
//
// The prober's whole guarantee is that a destination only ever sees a
// provider's address, never the operator's. That is enforced outside this
// process, because the beta deployment runs docker compose and the mainstream
// deployment does not use docker at all: a restricted docker network there,
// systemd IPAddressDeny=any plus a narrow IPAddressAllow here. Neither
// mechanism is inspectable portably, and creating a namespace for itself would
// need CAP_NET_ADMIN on a component that runs completely unprivileged today.
//
// So the check tests the property rather than the mechanism. If a direct
// connection succeeds, the confinement is absent and the prober exits: it does
// not "try anyway", because the failure mode of trying is silent -- a probe
// that could not tunnel would measure the operator's egress, and certify a
// provider that carries nothing as healthy.
//
// The addresses come from probeHosts -- egresshealth.DestinationHosts plus the
// bandwidth target -- resolved here at startup, so the repo holds exactly one
// copy of the endpoint list. A hand-maintained second copy would drift on the
// first endpoint change and the check would keep passing while no longer
// covering a real endpoint.
//
// This does not replace the Go-level fail-closed behaviour (requests only ever
// run on a tunnel-bound http.Client; providertunnel refuses any host outside
// its allowlist). It is the outer layer that backs it.
//
// Inability to verify is not evidence of confinement. Every outcome in which
// the check could not obtain real evidence is a refusal to start, never a
// pass: no resolvable host (confinement.ErrNoEvidence), a timeout too short to
// mean anything (confinement.ErrInvalidTimeout, rejected at flag validation),
// an empty endpoint list. Hosts that fail to resolve are not dialed by name --
// that dial fails at resolution and proves nothing -- and when only some of
// them resolve, the shortfall is logged as a warning so a degraded check never
// reads like a complete one.
func checkConfinement(ctx context.Context, dial confinement.DialFunc, lookup confinement.LookupFunc, explicitAddrs []string, timeout time.Duration, extraHosts ...string) error {
	hosts := probeHosts(extraHosts...)

	var addrs, unresolved []string
	if 0 < len(explicitAddrs) {
		// The escape hatch for a jail where dns legitimately cannot work.
		// Resolution is skipped entirely and exactly these addresses are
		// dialed, which keeps a real check available there instead of pushing
		// the operator towards -skip-confinement-check, which is no check at
		// all. Keeping them in sync with the probe endpoints is the
		// operator's job, so the endpoint list is logged alongside them.
		addrs = explicitAddrs
		log.Printf("egress-prober: confinement self-check: dialing the %d address(es) given with -confinement-address, skipping resolution: %s (probe hosts: %s -- keep these addresses current with that list)",
			len(addrs), strings.Join(addrs, " "), strings.Join(hosts, " "))
	} else {
		// Resolution is bounded so that a blocked resolver cannot hang startup
		// -- under a real deny-all confinement dns is blocked too. The budget is
		// per host, not for the whole list: confinement.Addresses resolves
		// sequentially, and this list grew from 3 hosts to over a hundred when
		// the egress-health destinations joined it. Keeping one flat budget for
		// the whole loop would have quietly made the check far more likely
		// to time out mid-list on a slow resolver -- which does not fail
		// loudly, it degrades: the later hosts land in `unresolved`, the check
		// reports a degraded pass, and the endpoints it stopped covering are
		// exactly the ones added last. (Measured here with a working resolver:
		// 12 hosts -> 49 addresses in 25ms, so this is headroom, not a
		// requirement.)
		resolveCtx, cancel := context.WithTimeout(ctx, timeout*time.Duration(len(hosts)))
		var err error
		addrs, unresolved, err = confinement.Addresses(resolveCtx, lookup, hosts, confinementPort)
		cancel()
		if err != nil {
			return err
		}

		log.Printf("egress-prober: confinement self-check: %d probe host(s) -> %d address(es): %s", len(hosts), len(addrs), strings.Join(addrs, " "))
		if 0 < len(unresolved) && 0 < len(addrs) {
			log.Printf("egress-prober: WARNING confinement self-check is DEGRADED: %d of %d probe host(s) could not be resolved and were NOT tested: %s. Whether this host can reach them directly is unknown. Allow dns resolution for the prober, or pass -confinement-address <ip:port> for each of them.",
				len(unresolved), len(hosts), strings.Join(unresolved, " "))
		}
	}

	if err := confinement.Verify(ctx, dial, addrs, unresolved, timeout); err != nil {
		if errors.Is(err, confinement.ErrNoEvidence) {
			// The vacuous pass this replaced: with dns blocked, every host fell
			// back to a bare name, every dial failed at resolution, and the
			// check reported success without having tested one address.
			return fmt.Errorf("%w -- allow dns resolution for the prober, or pass -confinement-address <ip:port> once per probe endpoint (%s) so the check dials them directly", err, strings.Join(hosts, " "))
		}
		return err
	}
	log.Printf("egress-prober: confinement self-check passed: %d address(es) tested, none directly reachable", len(addrs))
	return nil
}

// The server's prober-credential endpoint, injected so
// fetchByJwtIfEmpty is testable without one.
type credentialFetcher interface {
	ProberCredential(ctx context.Context) (*ingest.ProberCredential, error)
}

// credentialPollInitial and credentialPollMax bound the wait for a credential
// the server has not minted yet.
//
// The server's bootstrap task runs immediately and then every 6h, so a prober
// brought up alongside a fresh deployment can still win the startup race. That is a wait, not a
// crash: exiting would put a supervised process into a restart loop that
// re-runs the confinement self-check and re-fetches on every restart, and
// which reads in journald as a broken prober rather than as one patiently
// doing the right thing. Starting at 30s keeps the pickup prompt when the task
// runs minutes later; capping at 5m keeps a six-hour wait to ~75 requests.
const (
	credentialPollInitial = 30 * time.Second
	credentialPollMax     = 5 * time.Minute
)

// Doubles cur without exceeding maxBackoff. Split out from the loop so the
// schedule is testable without sleeping through it.
func nextBackoff(cur, maxBackoff time.Duration) time.Duration {
	if maxBackoff <= cur {
		return maxBackoff
	}
	if doubled := cur * 2; doubled < maxBackoff {
		return doubled
	}
	return maxBackoff
}

// Fills *byJwt from the server's prober-credential endpoint
// when it is empty, waiting for the server's bootstrap task if it has to.
//
// The precedence is envFallback's, deliberately: an explicitly supplied value
// is left alone and the server is not even asked. That is what keeps the
// existing deployment -- which supplies UR_PROBER_BY_JWT today -- working
// exactly as it does now, and it is why this cannot be written as "fetch, then
// prefer the explicit one": that would still block startup on a server which
// has no credential yet, for a prober that never needed one.
//
// The three outcomes of the fetch map to three different behaviours, and
// keeping them apart is the whole point:
//
//   - not ready (404): the expected state before the bootstrap task has run.
//     Log it as a wait and ask again, forever, on a capped backoff. There is no
//     total deadline: "wait rather than crash-loop" has no useful upper bound
//     here, and a supervisor's own start timeout is the right place to impose
//     one if a deployment wants it. ctx is what ends the wait.
//   - unauthorized (401): a wrong -operator-secret. Fatal and immediate. A
//     secret the server rejects will be rejected on every retry, so polling
//     would turn a two-minute fix into an outage nobody is paged for.
//   - anything else: transient. Retry on the same backoff, but log the actual
//     error each time rather than the reassuring "not ready" line, because
//     these are the ones that might need a human.
func fetchByJwtIfEmpty(ctx context.Context, byJwt *string, f credentialFetcher, initial, maxBackoff time.Duration) error {
	if *byJwt != "" {
		return nil
	}

	// Guards against a zero or negative interval turning the loop below into a
	// busy wait against the server (time.After fires immediately, and doubling
	// zero stays zero).
	if initial <= 0 {
		initial = time.Second
	}
	if maxBackoff < initial {
		maxBackoff = initial
	}

	log.Printf("egress-prober: no -by-jwt (or UR_PROBER_BY_JWT) was supplied; asking the server for the prober credential")

	backoff := initial
	for {
		cred, err := f.ProberCredential(ctx)
		switch {
		case err == nil:
			*byJwt = cred.ByClientJwt
			// The client id, never the jwt: this line goes to journald, and
			// the jwt is a credential. The id is what an operator needs to
			// match the prober against the server's record of it.
			log.Printf("egress-prober: got the prober credential from the server for client %s", cred.ClientId)
			return nil
		case errors.Is(err, ingest.ErrUnauthorized):
			return fmt.Errorf("the server rejected the operator secret when asked for the prober credential; check -operator-secret against ingest_secret in the server's provider_egress.yml: %w", err)
		case errors.Is(err, ingest.ErrCredentialNotReady):
			log.Printf("egress-prober: the server has not minted the prober credential yet; asking again in %s (its bootstrap task runs immediately and then every 6h)", backoff)
		default:
			log.Printf("egress-prober: could not get the prober credential: %s; asking again in %s", err, backoff)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = nextBackoff(backoff, maxBackoff)
	}
}

// Fills *value from the named environment variable when the flag
// was not given. Reading the environment here, rather than through the flag's
// default, is what keeps the value out of flag.Usage() output.
func envFallback(value *string, envName string) {
	if *value == "" {
		*value = os.Getenv(envName)
	}
}

// Holds the certificate pins currently in force.
//
// One writer (the pass loop's refresh) and one reader per tunnel Open, which
// runs on the scheduler's worker goroutines, so the mutex is load-bearing
// rather than decorative even though the refresh happens between passes today.
//
// It is only ever written with a set that has already passed
// fleetprobe.ValidatePins: there is exactly one path from the wire into this
// struct. Each probe then cuts it down to the hosts that probe dials, so a host
// the server serves that nothing dials never widens a tunnel's allowlist.
type pinSet struct {
	stateLock sync.Mutex
	pins      map[string][]string
	fetchedAt time.Time
}

// Returns a copy, so a refresh cannot mutate the map a tunnel is already
// verifying against mid-probe.
func (self *pinSet) get() map[string][]string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	out := make(map[string][]string, len(self.pins))
	for host, allowed := range self.pins {
		out[host] = append([]string(nil), allowed...)
	}
	return out
}

// Replaces the set with one already validated, and stamps the fetch time.
func (self *pinSet) set(pins map[string][]string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.pins = pins
	self.fetchedAt = time.Now()
}

// Reports how long ago the set was last successfully fetched.
func (self *pinSet) age() time.Duration {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return time.Since(self.fetchedAt)
}

// Gets the observed pin set from the server and validates it,
// returning the map the tunnels take.
//
// This is the only path from the server's answer into a pin set the prober will
// use. No host is required to be in it -- a host without a pin is verified by
// ordinary WebPKI (see fleetprobe.ValidatePins for why that is the right bar)
// -- but an incomplete pin is refused, never quietly dropped.
func fetchPins(ctx context.Context, client *ingest.Client) (map[string][]string, error) {
	served, err := client.GeolocationPins(ctx)
	if err != nil {
		return nil, err
	}
	return fleetprobe.ValidatePins(served)
}

// Re-fetches the pin set once pinRefreshInterval has elapsed since
// the last successful fetch.
//
// A failure here keeps the previous set and logs; it never clears it and never
// substitutes an empty map. The startup fetch in main is the one that refuses
// to continue -- deliberately a different call site, because the two must
// never be one parameter apart.
//
// Keeping a stale set is the right trade and not merely the convenient one: the
// pins were observed by the server on a direct WebPKI-validated connection, so
// an old one still rejects a provider substituting its own certificate. What it
// eventually stops doing is matching the legitimate host after a CA change --
// which fails closed, loudly, in the same place a fresh set would.
func refreshPins(ctx context.Context, client *ingest.Client, pins *pinSet, pinRefreshInterval time.Duration) {
	if pins.age() < pinRefreshInterval {
		return
	}
	refreshed, err := fetchPins(ctx, client)
	if err != nil {
		log.Printf("egress-prober: certificate pin refresh failed, keeping the set fetched %s ago: %s", pins.age().Round(time.Second), err)
		return
	}
	pins.set(refreshed)
}

// Holds the destination pool the current pass draws from. Like
// pinSet it is written between passes and read by every tunnel Open.
type poolSet struct {
	stateLock sync.Mutex
	pool      *egresshealth.Pool
}

// Returns the pool the current pass draws from.
func (self *poolSet) get() *egresshealth.Pool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.pool
}

// Replaces the pool, between passes.
func (self *poolSet) set(pool *egresshealth.Pool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.pool = pool
}

// The count requested in each per-location
// find-providers2 call. It is intentionally high: the server's selection
// (model.FindProviders2) weighted-shuffles and then truncates to `count`
// (`clientIds[:min(count, len(clientIds))]`), so a count well above any
// single location's real provider population effectively returns all of
// them, not a sample -- which is the whole point of I1 (a stable, complete
// enumeration, not a moving subset).
const findProvidersCountPerLocation = 5000

// Enumerates every provider client id the server currently
// knows about, broadly and stably. This directly fixes I1: the previous
// implementation asked for `{"best_available":true}`, which on the server
// resolves to `countryCodeLocationIds()["us"]` (model.FindProviders2 in
// model/network_client_location_model.go) -- so it only ever enumerated
// providers the server's own geo database already believes are in the US.
// That is backwards for a tool whose entire purpose is finding providers
// whose location the database gets wrong. It was also
// weighted-random-sampled and shuffled server-side, so successive passes
// returned different subsets rather than a stable enumeration.
//
// The fix enumerates by location instead of by (wrong) assumption:
//  1. GET /network/provider-locations -- no auth required
//     (router.WrapNoAuth in the server's
//     api/handlers/network_client_location_handlers.go) -- which returns
//     every location, at every granularity (city/region/country) that
//     currently has at least one provider (model.GetProviderLocations,
//     filtered to locations present in loadLocationStables). Verified
//     against the server source at api/api.go and
//     model/network_client_location_model.go: the response is a
//     model.FindLocationsResult, whose `locations` field is
//     []*model.LocationResult with `location_id` (model.LocationResult).
//  2. For each returned location, POST /network/find-providers2 with an
//     explicit `{"specs":[{"location_id":"<id>"}],"count":<high>}` spec
//     (model.FindProviders2Args / model.ProviderSpec), and union the
//     `client_id`s (model.FindProvidersProvider) across all locations,
//     de-duplicating. A single provider legitimately appears under more
//     than one location (its city, region, and country entries each
//     resolve back to it -- confirmed by how the server populates its
//     per-location score cache in model.go's export path), so
//     de-duplication here is expected, not defensive overkill.
//
// Resilient by design: if one location's find-providers2 call fails
// (timeout, transient 5xx), it is logged and skipped rather than aborting
// the whole pass -- a broad enumeration that misses one location out of
// what can be hundreds is far more useful than a pass that produces
// nothing because one of them hiccupped.
func listProviders(ctx context.Context, apiUrl string, byJwt string) ([]string, error) {
	httpClient := controlplane.NewHTTPClient(30 * time.Second)

	// Calls GET /network/provider-locations and returns the location_id of
	// every location in the response. See the doc above for the full rationale
	// and the server-side source verified against.
	listProviderLocationIds := func(ctx context.Context, client *http.Client, apiUrl string) ([]string, error) {
		endpointUrl := strings.TrimRight(apiUrl, "/") + "/network/provider-locations"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpointUrl, nil)
		if err != nil {
			return nil, err
		}

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
		}

		// Mirrors model.FindLocationsResult / model.LocationResult
		// (model/network_client_location_model.go) exactly: only the fields
		// this CLI actually needs are decoded.
		var out struct {
			Locations []struct {
				LocationId string `json:"location_id"`
			} `json:"locations"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, err
		}

		ids := make([]string, 0, len(out.Locations))
		for _, l := range out.Locations {
			ids = append(ids, l.LocationId)
		}
		return ids, nil
	}

	locationIds, err := listProviderLocationIds(ctx, httpClient, apiUrl)
	if err != nil {
		return nil, fmt.Errorf("list provider locations: %w", err)
	}

	seen := make(map[string]struct{})
	succeeded := 0
	var lastErr error
	for _, locationId := range locationIds {
		clientIds, err := findProvidersAtLocation(ctx, httpClient, apiUrl, byJwt, locationId)
		if err != nil {
			log.Printf("egress-prober: find-providers2 for location %s: %s (skipping this location for this pass)", locationId, err)
			lastErr = err
			continue
		}
		succeeded++
		for _, id := range clientIds {
			seen[id] = struct{}{}
		}
	}
	// Skipping some locations is resilience; skipping all of them is a
	// failed enumeration wearing a success return. The two endpoints can
	// genuinely diverge -- provider-locations is unauthenticated GET,
	// find-providers2 is an authenticated POST -- and an empty nil-error
	// result here flows into the "nothing to do (no providers, no failures)"
	// exit-0 path, which the exit-code contract explicitly promises an
	// external cron will never see from a pass that accomplished nothing.
	if 0 < len(locationIds) && succeeded == 0 {
		return nil, fmt.Errorf("find-providers2 failed for all %d locations (last: %w)", len(locationIds), lastErr)
	}

	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	return ids, nil
}

// Calls POST /network/find-providers2 with a
// single explicit location_id spec and returns the client_id of every
// provider returned for it. See listProviders for the full rationale and
// the server-side source verified against.
func findProvidersAtLocation(ctx context.Context, client *http.Client, apiUrl string, byJwt string, locationId string) ([]string, error) {
	// Mirrors model.FindProviders2Args / model.ProviderSpec
	// (model/network_client_location_model.go) exactly: Specs is
	// []*ProviderSpec, here a single spec with only LocationId set (json
	// "location_id"); Count and RankMode (json "rank_mode") match the
	// struct's json tags.
	//
	// ForceMinimum bypasses the PassesMinimums filter in loadClientScores.
	// That filter exists to keep low-quality providers out of user-facing
	// selection; a census of the fleet wants every provider that can accept a
	// contract, so leaving it off returned 1 of 39 providers on beta.
	reqBody, err := json.Marshal(struct {
		Specs        []map[string]string `json:"specs"`
		Count        int                 `json:"count"`
		RankMode     string              `json:"rank_mode"`
		ForceMinimum bool                `json:"force_minimum"`
	}{
		Specs:        []map[string]string{{"location_id": locationId}},
		Count:        findProvidersCountPerLocation,
		RankMode:     "quality",
		ForceMinimum: true,
	})
	if err != nil {
		return nil, err
	}

	endpointUrl := strings.TrimRight(apiUrl, "/") + "/network/find-providers2"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointUrl, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// find-providers2 does not require auth (router.WrapWithInputNoAuth on
	// the server), but the byJwt is sent anyway, matching the previous
	// implementation and every other call this CLI makes -- harmless, and
	// keeps this call consistent with the rest of the CLI's requests should
	// that ever change.
	req.Header.Set("Authorization", "Bearer "+byJwt)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	// Mirrors model.FindProviders2Result / model.FindProvidersProvider
	// exactly: only client_id is decoded.
	var out struct {
		Providers []struct {
			ClientId string `json:"client_id"`
		} `json:"providers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(out.Providers))
	for _, p := range out.Providers {
		ids = append(ids, p.ClientId)
	}
	return ids, nil
}

// Reports that the server refused the prober's byJwt.
//
// This is kept distinct from "could not check" for the same reason
// ingest.ErrUnauthorized is kept distinct from ingest.ErrDueUnsupported: a
// rejected credential is a broken deployment, and anything that lets it look
// like an ordinary runtime hiccup hides the fault behind work that appears to
// continue.
var errCredentialRejected = errors.New("egress-prober: the server rejected the prober's byJwt")

// Reports that the check could not reach a verdict --
// an old server without the endpoint, a transport error, a 5xx. It is not a
// claim that the credential is bad, so it must never stop the prober: doing so
// would turn "we could not ask" into a new outage of its own.
var errCredentialUnverified = errors.New("egress-prober: could not verify the byJwt")

// Asks the server whether it accepts the byJwt, which is not
// what parseByJwtClientId establishes -- that only proves the token decodes and
// carries a client_id.
//
// The gap between those two is a real outage mode, not a hypothetical. A token
// that parses perfectly is still refused once it expires (jwt.expiryDuration is
// 24h) or once the server begins enforcing a claim the token predates, and the
// prober has no way to notice: the byJwt authenticates only the provider
// tunnel, while the due queue, attempt reporting and pin fetch all authenticate
// with the operator secret. So every one of those keeps working, the prober
// goes on reporting attempts and looks healthy, and the tunnel silently carries
// nothing -- every probe fails with ok=0/N.
//
// That reads as "the whole fleet is bad", which is a convincing wrong answer.
// On one deployment it ran 8 hours and 870 consecutive failures before the
// credential was suspected. One request at startup turns that into a message.
func checkCredential(ctx context.Context, client *http.Client, apiUrl string, byJwt string) error {
	endpointUrl := strings.TrimRight(apiUrl, "/") + "/network/clients"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpointUrl, nil)
	if err != nil {
		return fmt.Errorf("%w: %w", errCredentialUnverified, err)
	}
	req.Header.Set("Authorization", "Bearer "+byJwt)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", errCredentialUnverified, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return errCredentialRejected
	default:
		// Everything else -- notably 404 from a server that predates this
		// endpoint -- is inconclusive by design.
		return fmt.Errorf("%w: status %d", errCredentialUnverified, resp.StatusCode)
	}
}

// Extracts the client_id claim from byJwt and parses it as
// a connect.Id. This mirrors the proven implementation in
// urnetwork/proxy/socks/main.go:parseByJwtClientId: the jwt is parsed
// unverified (the prober is not the one who issued it; the server it talks to
// is the authority that already validated it when minting a session from it),
// and the claim is type-switched rather than unmarshaled into a typed struct,
// since some issuers emit client_id as something other than a bare string.
func parseByJwtClientId(byJwt string) (connect.Id, error) {
	claims := gojwt.MapClaims{}
	if _, _, err := gojwt.NewParser().ParseUnverified(byJwt, claims); err != nil {
		return connect.Id{}, fmt.Errorf("parse jwt: %w", err)
	}

	jwtClientId, ok := claims["client_id"]
	if !ok {
		return connect.Id{}, fmt.Errorf("byJwt does not contain claim client_id")
	}
	switch v := jwtClientId.(type) {
	case string:
		return connect.ParseId(v)
	default:
		return connect.Id{}, fmt.Errorf("byJwt has invalid type for client_id: %T", v)
	}
}
