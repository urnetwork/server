package main

// `sim-latency run`: stand up the environment and drive the load.
//
// Order matters: the sim region and ip_overrides settings are installed before
// the services start (so provider connections geolocate to the region), the
// fleet ramps and settles (so the reliability pipeline makes providers
// selectable) before the measured window, and only per-request performance
// stats go to stdout.

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/stats"
)

type RunOptions struct {
	ConfigPath string
	SiteHome   string
	Ramp       time.Duration
	// reliability history backfilled so providers are established without the
	// ~8.4h cold-start warm-up (0 = pure organic warm-up)
	Prewarm     time.Duration
	Settle      time.Duration
	Duration    time.Duration
	FleetShards int
	// site listen address (providers egress here over loopback)
	SiteListen string
	Services   *ServicesConfig
}

func Run(options *RunOptions) error {
	config, err := LoadConfig(options.ConfigPath)
	if err != nil {
		return err
	}
	if err := config.validate(); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// bring the local database schema up to date (no-op once migrated)
	logf("applying db migrations")
	server.ApplyDbMigrations(ctx)

	// install the site settings (ip_overrides + stats knobs) before anything
	// geolocates a fake ip
	if err := writeSiteSettings(options.SiteHome, config); err != nil {
		return err
	}
	statsHandle := stats.Enable(ctx, nil)
	defer statsHandle.Close()
	logf("stats enabled=%t instance=%s", statsHandle.Enabled(), statsHandle.InstanceId())

	// sim region + provider/client identities
	locationId, err := provisionRegion(ctx, config.Region)
	if err != nil {
		return err
	}
	logf("sim region country location=%s", locationId)

	if err := provisionProviders(ctx, config.Fleet); err != nil {
		return err
	}
	pool, err := provisionClientPool(ctx, config)
	if err != nil {
		return err
	}

	// services
	servicesConfig := options.Services
	if servicesConfig == nil {
		servicesConfig = DefaultServicesConfig()
	}
	services, err := NewServices(ctx, servicesConfig)
	if err != nil {
		return err
	}
	defer services.Close()
	logf("services up api=%s ws=%v", services.ApiUrl(), services.WsUrls())

	// fake site
	site, err := NewSite(ctx, options.SiteListen, config.Seed, config.Site)
	if err != nil {
		return err
	}
	logf("fake site at %s", site.Addr())

	// fleet: in-process, or sharded into subprocesses
	if 0 < options.FleetShards {
		procs, err := spawnFleetShards(options, config, services)
		if err != nil {
			return err
		}
		defer func() {
			for _, proc := range procs {
				proc.Process.Signal(syscall.SIGTERM)
			}
		}()
	} else {
		NewFleet(ctx, config, config.Fleet, services.ApiUrl(), services.WsUrls(), services.WsPorts(), options.Ramp)
	}

	// ramp: stagger provider connects, then give the announce loop a moment to
	// register connections + locations before the pipeline reads them.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

	logf("ramp=%s prewarm=%s settle=%s then measure=%s", options.Ramp, options.Prewarm, options.Settle, options.Duration)
	rampWait := options.Ramp + 15*time.Second
	select {
	case <-time.After(rampWait):
	case sig := <-signals:
		logf("signal %s during ramp; draining", sig)
		cancel()
		time.Sleep(1 * time.Second)
		return nil
	}

	// prewarm: backfill reliability history so the established market exists
	// without waiting the ~8.4h the 12h-lookback gate needs from cold. Then run
	// the pipeline once so providers are selectable, and let a short settle
	// propagate the redis sample export.
	if 0 < options.Prewarm {
		logf("prewarming: establishing the connected fleet (~%s reliability window)", options.Prewarm)
		services.SetPrewarmed(true)
		if err := provisionPrewarm(ctx, options.Prewarm); err != nil {
			return err
		}
		logf("prewarm complete; running pipeline")
		services.RunPipelineOnce(ctx)
	}

	select {
	case <-time.After(options.Settle):
	case sig := <-signals:
		logf("signal %s during settle; draining", sig)
		cancel()
		time.Sleep(1 * time.Second)
		return nil
	}

	// build the warm client pool during warm-up (before the measured window),
	// so pool-setup time is not counted
	driver := NewClientDriver(ctx, config, services.ApiUrl(), services.WsUrls(), site.Addr(), locationId, pool)
	driver.Warmup()

	// client driver, measured window
	measureStart := server.NowUtc()
	measureEnd := measureStart.Add(options.Duration)
	logf("MEASURE WINDOW: [%d, %d] unix-ms", measureStart.UnixMilli(), measureEnd.UnixMilli())
	go server.HandleError(driver.Run)

	select {
	case <-time.After(options.Duration):
		logf("measure window complete; draining")
	case sig := <-signals:
		logf("signal %s; draining", sig)
	}
	cancel()
	// give in-flight crawls and stats flush a moment
	time.Sleep(2 * time.Second)
	return nil
}

// spawnFleetShards launches the fleet as N subprocesses, each connecting to
// this run's services. They inherit the env (vault for jwt signing) and log to
// stderr; only this process writes stdout.
func spawnFleetShards(options *RunOptions, config *Config, services *Services) ([]*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	procs := []*exec.Cmd{}
	for i := 0; i < options.FleetShards; i += 1 {
		cmd := exec.Command(self,
			"fleet",
			"--providers", options.ConfigPath,
			"--shard", intToStr(i)+"/"+intToStr(options.FleetShards),
			"--api-url", services.ApiUrl(),
			"--ws-urls", strings.Join(services.WsUrls(), ","),
			"--ramp", options.Ramp.String(),
		)
		cmd.Env = os.Environ()
		cmd.Stdout = os.Stderr // fleet emits no CSV; keep stdout clean
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			return procs, err
		}
		logf("spawned fleet shard %d/%d pid=%d", i, options.FleetShards, cmd.Process.Pid)
		procs = append(procs, cmd)
	}
	return procs, nil
}
