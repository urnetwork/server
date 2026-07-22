// Command monitor runs automated analysis against a production environment in
// a loop and emits issue tickets when a signal in SIGNALS.md leaves its
// healthy band. It detects; it does not diagnose or fix. Design: MONITOR.md in
// this directory; signal catalog: SIGNALS.md.
//
// Local development: run against main over the vpn overlay with the console
// emitter —
//
//	WARP_ENV=main monitor --once        # one pass, print what would fire
//	WARP_ENV=main monitor               # the standing loop + log tailers
package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

func main() {
	usage := `BringYour production monitor.

Usage:
  monitor [--once] [--mode=<mode>] [--no-tail]
  monitor -h | --help
  monitor --version

Options:
  -h --help        Show this screen.
  --version        Show version.
  --once           Run every probe once, print findings, and exit (local dev).
  --mode=<mode>    Address mode override: lan | overlay (default: from monitor.yml).
  --no-tail        Disable the standing log tailers (loop mode).`

	opts, err := docopt.ParseArgs(usage, os.Args[1:], server.RequireVersion())
	if err != nil {
		panic(err)
	}

	cfg := loadConfig()
	if mode, e := opts.String("--mode"); e == nil && mode != "" {
		cfg.addressMode = mode
	}

	baseline, err := newBaselineStore(filepath.Join(cfg.stateDir, "baseline"))
	if err != nil {
		glog.Errorf("[monitor]baseline store unavailable (%s); static bands only\n", err)
	}

	env := &probeEnv{
		cfg:      cfg,
		runner:   newRunner(cfg),
		baseline: baseline,
	}
	manager := newTicketManager(cfg.env, newConsoleEmitter(os.Stdout, os.Stderr))

	probes := []probe{
		// tier-0 (SIGNALS.md §1)
		newPgStateProbe(),
		pgContractRateProbe{},
		taskCanaryProbe{},
		redisClusterProbe{},
		// tier-1 + daily (§2, §3)
		pgOpenSetProbe{},
		&pgConnectRateProbe{},
		pgSelectionFreshnessProbe{},
		pgbouncerProbe{},
		pgVacuumProbe{},
		pgStatsLandmineProbe{},
		taskDurationProbe{},
		// e2e encryption key publication (§15)
		pgE2eKeyProbe{},
		// taskworker drain / task-plane liveness (§12)
		taskworkerDrainProbe{},
		newRedisMemoryProbe(),
		redisTopologyProbe{},
		redisFamilyProbe{},
		redisKeyEventProbe{},
	}

	if once, _ := opts.Bool("--once"); once {
		manager.immediate = true
		glog.Infof("[monitor]single pass env=%s mode=%s hosts=%d probes=%d\n",
			cfg.env, cfg.addressMode, len(cfg.hosts), len(probes))
		runOnce(context.Background(), env, manager, probes)
		return
	}

	quitEvent := server.NewEventWithContext(context.Background())
	defer quitEvent.Set()
	closeFn := quitEvent.SetOnSignals(syscall.SIGQUIT, syscall.SIGTERM)
	defer closeFn()
	ctx := quitEvent.Ctx

	// standing log tailers (design §3.7) feed a probe that drains their
	// per-minute windows
	if noTail, _ := opts.Bool("--no-tail"); !noTail {
		tailers := []*logTailer{}
		for _, service := range warpServices(ctx, env) {
			tailer := newLogTailer(service, env)
			tailers = append(tailers, tailer)
			go server.HandleError(func() { tailer.run(ctx) })
		}
		probes = append(probes, &logTailProbe{tailers: tailers})
		glog.Infof("[monitor]tailing %d services\n", len(tailers))
	}

	glog.Infof("[monitor]start env=%s mode=%s hosts=%d probes=%d\n",
		cfg.env, cfg.addressMode, len(cfg.hosts), len(probes))
	runLoop(ctx, env, manager, probes)
}

// runOnce runs every probe once, sequentially, and ingests the findings.
func runOnce(ctx context.Context, env *probeEnv, manager *ticketManager, probes []probe) {
	all := []finding{}
	for _, p := range probes {
		findings, err := p.check(ctx, env)
		if err != nil {
			all = append(all, visibilityFinding(p, err))
			continue
		}
		all = append(all, findings...)
	}
	manager.ingest(ctx, all)
	glog.Infof("[monitor]tick complete: %d probes, %d findings, %d open tickets\n",
		len(probes), len(all), manager.openCount())
}

// runLoop schedules each probe on its own jittered ticker. A probe never
// overlaps itself; findings are ingested under a shared lock so ticket state
// is consistent.
func runLoop(ctx context.Context, env *probeEnv, manager *ticketManager, probes []probe) {
	var ingestLock sync.Mutex
	var wg sync.WaitGroup

	for _, p := range probes {
		wg.Add(1)
		go server.HandleError(func() {
			defer wg.Done()
			// jitter the first fire so probes do not all hit at once
			jitter := time.Duration(rand.Int63n(int64(p.cadence())))
			select {
			case <-ctx.Done():
				return
			case <-time.After(jitter):
			}
			for {
				findings, err := p.check(ctx, env)
				func() {
					ingestLock.Lock()
					defer ingestLock.Unlock()
					if err != nil {
						manager.ingest(ctx, []finding{visibilityFinding(p, err)})
					} else {
						manager.ingest(ctx, findings)
					}
				}()
				select {
				case <-ctx.Done():
					return
				case <-time.After(p.cadence()):
				}
			}
		})
	}

	// heartbeat so a silent monitor is detectable (design §3.6)
	go server.HandleError(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Minute):
			}
			open := func() int {
				ingestLock.Lock()
				defer ingestLock.Unlock()
				return manager.openCount()
			}()
			glog.Infof("[monitor]heartbeat: %d probes scheduled, %d open tickets\n", len(probes), open)
		}
	})

	<-ctx.Done()
	glog.Infof("[monitor]draining on signal\n")
	wg.Wait()
}

// visibilityFinding turns a probe execution error (host unreachable / command
// timeout) into a monitor/visibility ticket — blindness during an incident is
// itself urgent (design §3.6).
func visibilityFinding(p probe, err error) finding {
	return finding{
		probeId: "monitor/visibility", tier: tierWarn,
		class: "cannot-observe", target: p.id(), sustain: 2,
		symptom:  fmt.Sprintf("probe %q could not run: %s", p.id(), err),
		baseline: "every probe runs each cadence; a probe that cannot run leaves its signal unmonitored",
		observed: err.Error(),
		playbook: "SIGNALS.md 1.4 (is the target itself wedged?)",
	}
}

// warpServices lists the environment's services for the log tailers, from the
// docker repo registry (`warpctl ls services <env>` prints one "Found repo
// names <env>-a, <env>-b, ..." line covering every env). Repos for this env
// are "<env>-<service>"; lb and config-updater are infrastructure and are not
// tailed. Falls back to the core service set if warpctl is unavailable.
func warpServices(ctx context.Context, env *probeEnv) []string {
	coreServices := []string{"api", "connect", "taskworker"}
	// the repo-names line prints immediately but the command then keeps
	// polling version state and can outlive the timeout — parse whatever
	// stdout accumulated even on a timeout error
	out, err := env.runner.warpctl(ctx, "ls", "services", env.cfg.env)
	marker := "repo names "
	i := strings.Index(out, marker)
	if i < 0 {
		if err != nil {
			glog.Warningf("[monitor]warpctl ls services failed (%s); using core set\n", err)
		}
		return coreServices
	}
	repoNames := strings.Split(strings.TrimSpace(out[i+len(marker):]), ",")
	envPrefix := env.cfg.env + "-"
	services := []string{}
	for _, repoName := range repoNames {
		repoName = strings.TrimSpace(repoName)
		if !strings.HasPrefix(repoName, envPrefix) {
			continue
		}
		service := strings.TrimPrefix(repoName, envPrefix)
		if service == "" || service == "lb" || service == "config-updater" {
			continue
		}
		services = append(services, service)
	}
	if len(services) == 0 {
		return coreServices
	}
	return services
}
