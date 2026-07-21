package main

// sim-latency: a local simulation environment for the egress-provider stack,
// used to run an algorithmic competition to improve FindProviders2, the
// client, LocalUserNat, and related latency-sensitive code. See README.md.
//
// Commands:
//   init   sample the mixture and write providers.yml (locks a run's fleet)
//   run    stand up the environment (services, fake site, provider fleet) and
//          drive Poisson client load, printing per-request stats as CSV to
//          stdout (only stats go to stdout; all logs go to stderr)
//   fleet  run one provider fleet shard against an existing run's services
//          (used internally by `run --fleet-shards`, also usable standalone
//          to add providers from another machine)

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
	usage := `sim-latency: local latency/load simulation for the egress provider stack.

Usage:
  sim-latency init [--out=<path>] [--count=<n>] [--clients=<n>] [--rate=<m>] [--seed=<s>]
  sim-latency run [--providers=<path>] [--site-home=<dir>] [--ramp=<d>] [--prewarm=<d>] [--settle=<d>] [--duration=<d>] [--fleet-shards=<n>] [--site-listen=<addr>] [--hosts=<n>] [--api-port=<p>] [--pipeline-interval=<d>] [--test-timeout=<d>] [--announce-timeout=<d>] [--no-impair]
  sim-latency fleet --providers=<path> --shard=<i/n> --api-url=<url> --ws-urls=<urls> [--ramp=<d>]
  sim-latency -h | --help
  sim-latency --version

Options:
  -h --help              Show this screen.
  --version              Show version.
  --out=<path>           providers.yml output path [default: providers.yml].
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
  --duration=<d>         Measured window duration [default: 30m].
  --fleet-shards=<n>     Provider fleet subprocess count (0 = in-process) [default: 0].
  --site-listen=<addr>   Fake site listen address [default: 127.0.0.1:0].
  --hosts=<n>            Exchange host/ws-port count [default: 4].
  --api-port=<p>         Api listen port [default: 7640].
  --shard=<i/n>          Fleet shard index/count.
  --api-url=<url>        Api url (fleet).
  --ws-urls=<urls>       Comma-separated exchange ws urls (fleet).`

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
	}
}

func runInit(opts docopt.Opts) {
	out, _ := opts.String("--out")
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
// service state from running against a non-local environment. `init` is
// intentionally excluded: it only writes the requested providers.yml file.
func validateEnvironment(command string, env string) error {
	switch command {
	case "run", "fleet":
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
