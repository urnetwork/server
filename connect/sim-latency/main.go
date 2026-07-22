package main

// sim-latency: a local simulation environment for the egress-provider stack,
// used to run an algorithmic competition to improve FindProviders2, the
// client, LocalUserNat, and related latency-sensitive code. See README.md.
//
// Commands:
//   init      sample the mixture and write providers.yml (locks a run's fleet)
//   run       stand up the environment (services, fake site, provider fleet)
//             and drive Poisson client load, printing per-request stats as CSV
//             to stdout (only stats go to stdout; all logs go to stderr) plus
//             a run.json side-car (--meta)
//   fleet     run one provider fleet shard against an existing run's services
//             (used internally by `run --fleet-shards`, also usable standalone
//             to add providers from another machine)
//   analyze   summarize one run's metrics from its CSV + side-car
//   baseline  measure the environment noise floor + its convergence
//             (baseline.json), from existing artifacts (--runs) or by
//             running n fresh replicates; the error term for compare
//   compare   decide whether run set A is statistically better than B
//   reset     clear cross-run reliability state so runs are independent

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
)

func main() {
	// the sdk redirects os.Stderr to os.Stdout at package init (the mobile
	// convention: diagnostics to stdout). This tool is the opposite: stdout
	// carries only measurement output (CSV/tables/json), logs go to stderr.
	// Restore the real stderr before anything logs.
	os.Stderr = os.NewFile(2, "/dev/stderr")

	usage := `sim-latency: local latency/load simulation for the egress provider stack.

Usage:
  sim-latency init [--out=<path>] [--count=<n>] [--clients=<n>] [--rate=<m>] [--seed=<s>]
  sim-latency run [--providers=<path>] [--site-home=<dir>] [--ramp=<d>] [--prewarm=<d>] [--settle=<d>] [--duration=<d>] [--fleet-shards=<n>] [--site-listen=<addr>] [--hosts=<n>] [--api-port=<p>] [--pipeline-interval=<d>] [--test-timeout=<d>] [--announce-timeout=<d>] [--no-impair] [--reset] [--meta=<path>]
  sim-latency fleet --providers=<path> --shard=<i/n> --api-url=<url> --ws-urls=<urls> [--ramp=<d>]
  sim-latency analyze --run=<path> [--window=<w>] [--out=<path>] [--json]
  sim-latency baseline --runs=<paths> [--alpha=<a>] [--out=<path>]
  sim-latency baseline [--replicates=<n>] [--out-dir=<dir>] [--alpha=<a>] [--out=<path>] [--providers=<path>] [--site-home=<dir>] [--ramp=<d>] [--prewarm=<d>] [--settle=<d>] [--duration=<d>] [--fleet-shards=<n>] [--site-listen=<addr>] [--hosts=<n>] [--api-port=<p>] [--pipeline-interval=<d>] [--test-timeout=<d>] [--announce-timeout=<d>] [--no-impair]
  sim-latency compare --a=<paths> --b=<paths> [--baseline=<path>] [--p=<a>] [--window=<w>] [--json]
  sim-latency reset
  sim-latency -h | --help
  sim-latency --version

Options:
  -h --help              Show this screen.
  --version              Show version.
  --out=<path>           Output path (init: providers.yml; analyze: run.json; variance: environment.json).
  --count=<n>            Number of providers [default: 100000].
  --clients=<n>          Client identity pool size [default: 4000].
  --rate=<m>             Mean client arrivals per minute [default: 1000].
  --seed=<s>             Master seed [default: 1].
  --providers=<path>     providers.yml path [default: providers.yml].
  --site-home=<dir>      Site dir for settings + stats [default: .sim-site].
  --ramp=<d>             Warm-up: fleet connect-stagger duration [default: 1m].
  --prewarm=<d>          Warm-up: establish providers (reliability window; 0 = cold ~8.4h warm-up) [default: 13h].
  --settle=<d>           Warm-up: stabilization after prewarm, before measurement [default: 1m].
  --pipeline-interval=<d>  Warm-up: reliability/score/sample refresh cadence [default: 10s].
  --test-timeout=<d>     Warm-up: synthetic speed-test trigger delay [default: 3s].
  --announce-timeout=<d>  Warm-up: connection register/test delay [default: 2s].
  --no-impair            Disable provider network impairment (clean baseline).
  --reset                Clear cross-run reliability state (local pg + redis) before the run.
  --meta=<path>          Run summary side-car (run.json) output [default: run.json].
  --duration=<d>         Measured window duration [default: 30m].
  --fleet-shards=<n>     Provider fleet subprocess count (0 = in-process) [default: 0].
  --site-listen=<addr>   Fake site listen address [default: 127.0.0.1:0].
  --hosts=<n>            Exchange host/ws-port count [default: 4].
  --api-port=<p>         Api listen port [default: 7640].
  --shard=<i/n>          Fleet shard index/count.
  --api-url=<url>        Api url (fleet).
  --ws-urls=<urls>       Comma-separated exchange ws urls (fleet).
  --run=<path>           A run artifact: results csv (with run.json side-car) or run.json.
  --runs=<paths>         Comma-separated existing replicate artifacts (csv or run.json);
                         without it, baseline measures --replicates fresh runs itself.
  --replicates=<n>       Baseline replicate runs to measure [default: 5].
  --out-dir=<dir>        Directory for measured replicate artifacts [default: baseline-runs].
  --a=<paths>            Side A run artifact(s), comma-separated.
  --b=<paths>            Side B run artifact(s), comma-separated.
  --baseline=<path>      baseline.json noise floor written by sim-latency baseline.
  --window=<w>           Measure window override "<startMs>,<endMs>" for a csv without run.json.
  --alpha=<a>            Significance level stored with the baseline [default: 0.05].
  --p=<a>                One-sided significance threshold [default: 0.05].
  --json                 Machine-readable output.`

	setDefaultEnv()

	opts, err := docopt.ParseArgs(usage, os.Args[1:], server.RequireVersion())
	if err != nil {
		panic(err)
	}

	switch {
	case optBool(opts, "init"):
		runInit(opts)
	case optBool(opts, "run"):
		requireLocalEnvironment("run")
		runRun(opts)
	case optBool(opts, "fleet"):
		requireLocalEnvironment("fleet")
		runFleet(opts)
	case optBool(opts, "analyze"):
		runAnalyze(opts)
	case optBool(opts, "baseline"):
		// computing from existing artifacts is file-only; measuring fresh
		// replicates stands up the full local environment
		if optString(opts, "--runs", "") == "" {
			requireLocalEnvironment("baseline")
		}
		runBaselineCmd(opts)
	case optBool(opts, "compare"):
		runCompare(opts)
	case optBool(opts, "reset"):
		requireLocalEnvironment("reset")
		runReset()
	}
}

// runReset clears the cross-run simulation state (standalone form of
// `run --reset`).
func runReset() {
	ctx, cancel := signalContext()
	defer cancel()
	resetLocalState(ctx)
}

func runInit(opts docopt.Opts) {
	out := optString(opts, "--out", "providers.yml")
	count := optInt(opts, "--count", 100000)
	clients := optInt(opts, "--clients", 4000)
	rate := float64(optInt(opts, "--rate", 1000))
	seed := int64(optInt(opts, "--seed", 1))

	config := defaultConfig(seed, count, clients, rate)
	if err := generateFleet(config); err != nil {
		fatalf("generate fleet: %s", err)
	}
	if err := SaveConfig(out, config); err != nil {
		fatalf("save config: %s", err)
	}
	logf("wrote %s: %d providers, %d client pool, seed %d", out, count, clients, seed)
}

func runRun(opts docopt.Opts) {
	providers, _ := opts.String("--providers")
	siteHome, _ := opts.String("--site-home")
	absSiteHome := mustAbs(siteHome)
	if noImpair, _ := opts.Bool("--no-impair"); noImpair {
		impairEnabled = false
	}
	// the in-process services read the site settings/stats via WARP_SITE_HOME
	os.Setenv("WARP_SITE_HOME", absSiteHome)

	servicesConfig := DefaultServicesConfig()
	servicesConfig.HostCount = optInt(opts, "--hosts", 4)
	servicesConfig.ApiPort = optInt(opts, "--api-port", 7640)
	// warm-up tuning knobs (see DefaultServicesConfig for the fast defaults)
	servicesConfig.PipelineInterval = optDuration(opts, "--pipeline-interval", servicesConfig.PipelineInterval)
	servicesConfig.SpeedTestTimeout = optDuration(opts, "--test-timeout", servicesConfig.SpeedTestTimeout)
	servicesConfig.AnnounceTimeout = optDuration(opts, "--announce-timeout", servicesConfig.AnnounceTimeout)

	options := &RunOptions{
		ConfigPath:  providers,
		SiteHome:    absSiteHome,
		Ramp:        optDuration(opts, "--ramp", 1*time.Minute),
		Prewarm:     optDuration(opts, "--prewarm", 13*time.Hour),
		Settle:      optDuration(opts, "--settle", 1*time.Minute),
		Duration:    optDuration(opts, "--duration", 30*time.Minute),
		FleetShards: optInt(opts, "--fleet-shards", 0),
		SiteListen:  optString(opts, "--site-listen", "127.0.0.1:0"),
		Services:    servicesConfig,
		MetaPath:    optString(opts, "--meta", "run.json"),
		Reset:       optBool(opts, "--reset"),
	}
	if err := Run(options); err != nil {
		fatalf("run: %s", err)
	}
}

func runFleet(opts docopt.Opts) {
	providers, _ := opts.String("--providers")
	shard, _ := opts.String("--shard")
	apiUrl, _ := opts.String("--api-url")
	wsUrlsStr, _ := opts.String("--ws-urls")
	ramp := optDuration(opts, "--ramp", 10*time.Minute)

	index, count := parseShard(shard)
	wsUrls := strings.Split(wsUrlsStr, ",")
	wsPorts := wsPortsFromUrls(wsUrls)

	config, err := LoadConfig(providers)
	if err != nil {
		fatalf("load providers: %s", err)
	}
	entries := config.shard(index, count)
	logf("fleet shard %d/%d: %d providers", index, count, len(entries))

	ctx, cancel := signalContext()
	defer cancel()

	NewFleet(ctx, config, entries, apiUrl, wsUrls, wsPorts, ramp)

	<-ctx.Done()
	logf("fleet shard %d/%d draining", index, count)
	time.Sleep(1 * time.Second)
}

// ---- env + option helpers ----

// setDefaultEnv fills the local-env vars a self-contained invocation needs,
// without clobbering anything the caller set (e.g. via run-local.sh).
func setDefaultEnv() {
	setIfUnset := func(key string, value string) {
		if os.Getenv(key) == "" {
			os.Setenv(key, value)
		}
	}
	setIfUnset("WARP_ENV", "local")
	setIfUnset("WARP_SERVICE", "sim")
	setIfUnset("WARP_BLOCK", "sim")
	setIfUnset("WARP_VERSION", "0.0.0-sim")
	setIfUnset("WARP_HOST", "127.0.0.1")
	setIfUnset("BRINGYOUR_POSTGRES_HOSTNAME", "local-pg.bringyour.com")
	setIfUnset("BRINGYOUR_REDIS_HOSTNAME", "local-redis.bringyour.com")
}

// validateEnvironment prevents every command that can connect to or mutate
// service state from running against a non-local environment. `init` and the
// analysis commands (`analyze`, `compare`, `baseline --runs`) are
// intentionally excluded: they only read and write local files. `baseline`
// appears here for its measuring mode (no --runs), which runs full
// replicates.
func validateEnvironment(command string, env string) error {
	switch command {
	case "run", "fleet", "reset", "baseline":
		if env != "local" {
			return fmt.Errorf(
				"sim-latency %s is local-only: refusing WARP_ENV=%q",
				command,
				env,
			)
		}
	}
	return nil
}

func requireLocalEnvironment(command string) {
	if err := validateEnvironment(command, server.RequireEnv()); err != nil {
		fatalf("%s", err)
	}
}

func optBool(opts docopt.Opts, key string) bool {
	v, _ := opts.Bool(key)
	return v
}

func optInt(opts docopt.Opts, key string, fallback int) int {
	s, err := opts.String(key)
	if err != nil || s == "" {
		return fallback
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return v
}

func optString(opts docopt.Opts, key string, fallback string) string {
	s, err := opts.String(key)
	if err != nil || s == "" {
		return fallback
	}
	return s
}

func optDuration(opts docopt.Opts, key string, fallback time.Duration) time.Duration {
	s, err := opts.String(key)
	if err != nil || s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
}

func parseShard(shard string) (index int, count int) {
	parts := strings.SplitN(shard, "/", 2)
	if len(parts) != 2 {
		return 0, 1
	}
	index, _ = strconv.Atoi(parts[0])
	count, _ = strconv.Atoi(parts[1])
	if count <= 0 {
		count = 1
	}
	return index, count
}

func wsPortsFromUrls(wsUrls []string) map[int]bool {
	ports := map[int]bool{}
	for _, wsUrl := range wsUrls {
		u, err := url.Parse(wsUrl)
		if err != nil {
			continue
		}
		if port, err := strconv.Atoi(u.Port()); err == nil {
			ports[port] = true
		}
	}
	return ports
}

func intToStr(v int) string {
	return strconv.Itoa(v)
}

func mustAbs(path string) string {
	if abs, err := absPath(path); err == nil {
		return abs
	}
	return path
}
