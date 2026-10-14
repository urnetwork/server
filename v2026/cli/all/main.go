// Command all runs the operator's API, Connect exchange and complete task
// worker in one process. Schema changes and initial task seeding are explicit
// commands; serving never migrates a database. Extender gossip is opt-in.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/api"
	connectserver "github.com/urnetwork/server/v2026/connect"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/gossip"
	"github.com/urnetwork/server/v2026/router"
	"github.com/urnetwork/server/v2026/taskworker"
)

const usage = `URnetwork operator in one process.

Usage:
  all [run] [options]
  all db migrate
  all init-tasks
  all --help
  all --version

Run options:
  --api-port=8080                 API HTTP service port.
  --connect-port=8081             Connect WebSocket HTTP service port.
  --taskworker-port=8082          Task worker status HTTP service port.
  --worker-count=8               Concurrent task workers.
  --worker-batch-size=4          Task claim batch size.
  --gossip-port=0                Extender gossip WebSocket port; 0 disables it.
  --require-subnet               Refuse missing, disabled or invalid st.yml.
  --memory-owner-ledger          Enable Connect transfer-owner accounting.
  --private-heap-profile-target=config  Config, disabled, or exact host/block.

Set WARP_ENV, WARP_HOST, WARP_SERVICE, WARP_BLOCK, WARP_DOMAIN,
WARP_VERSION, WARP_HOST_IPV4 and WARP_PORTS before starting. WARP_PORTS
must map each enabled service port and the Connect internal port 5080.
Connect UDP 443/4053/8053 listeners are enabled only when mapped; they use
the existing Warp ingress protocol. Check all three HTTP /status endpoints.
PostgreSQL, Redis, TLS ingress and hosted-device proxy remain external.
`

// Parsing has no configuration, database, socket or chain side effects.
type commandOptions struct {
	command           string
	apiPort           int
	connectPort       int
	taskworkerPort    int
	workerCount       int
	workerBatchSize   int
	gossipPort        int
	requireSubnet     bool
	memoryOwnerLedger bool
	privateHeapTarget string
}

// The complete command line is validated before loading credentials.
func parseCommand(args []string) (commandOptions, error) {
	options := commandOptions{command: "run"}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		options.command = "help"
		return options, nil
	}
	if len(args) == 1 && args[0] == "--version" {
		options.command = "version"
		return options, nil
	}
	if len(args) > 0 && args[0] == "init-tasks" {
		if len(args) != 1 {
			return options, errors.New("init-tasks accepts no arguments")
		}
		options.command = "init-tasks"
		return options, nil
	}
	if len(args) >= 2 && args[0] == "db" && args[1] == "migrate" {
		if len(args) != 2 {
			return options, errors.New("db migrate accepts no arguments")
		}
		options.command = "migrate"
		return options, nil
	}
	flags := flag.NewFlagSet("all", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	if len(args) > 0 && args[0] == "run" {
		args = args[1:]
	}
	flags.IntVar(&options.apiPort, "api-port", 8080, "API service port")
	flags.IntVar(&options.connectPort, "connect-port", 8081, "Connect service port")
	flags.IntVar(&options.taskworkerPort, "taskworker-port", 8082, "task worker status port")
	flags.IntVar(&options.workerCount, "worker-count", 8, "task workers")
	flags.IntVar(&options.workerBatchSize, "worker-batch-size", 4, "task claim batch size")
	flags.IntVar(&options.gossipPort, "gossip-port", 0, "optional gossip service port")
	flags.BoolVar(&options.requireSubnet, "require-subnet", false, "require enabled subnet settlement")
	flags.BoolVar(&options.memoryOwnerLedger, "memory-owner-ledger", false, "Connect memory accounting")
	flags.StringVar(&options.privateHeapTarget, "private-heap-profile-target", "config", "Connect private heap scope")
	if err := flags.Parse(args); err != nil {
		return options, err
	}
	if flags.NArg() != 0 {
		return options, errors.New("unknown all command or positional argument")
	}
	if err := (api.RunOptions{Port: options.apiPort}).Validate(); err != nil {
		return options, err
	}
	if err := (connectserver.RunOptions{Port: options.connectPort}).Validate(); err != nil {
		return options, err
	}
	if err := (taskworker.RunOptions{Port: options.taskworkerPort, Count: options.workerCount, BatchSize: options.workerBatchSize}).Validate(); err != nil {
		return options, err
	}
	if options.gossipPort != 0 {
		if err := (gossip.RunOptions{Port: options.gossipPort}).Validate(); err != nil {
			return options, err
		}
	}
	seenPorts := map[int]bool{}
	for _, port := range []int{options.apiPort, options.connectPort, options.taskworkerPort, options.gossipPort} {
		if port == 0 {
			continue
		}
		if seenPorts[port] {
			return options, fmt.Errorf("operator services share port %d", port)
		}
		seenPorts[port] = true
	}
	return options, nil
}

// All listeners use one stable Warp identity and one immutable port map.
// Validate actual host-port collisions as well as service-port collisions.
func validateEnvironment(options commandOptions, getenv func(string) string) error {
	for _, name := range []string{"WARP_ENV", "WARP_HOST", "WARP_SERVICE", "WARP_BLOCK", "WARP_DOMAIN", "WARP_VERSION"} {
		if strings.TrimSpace(getenv(name)) == "" {
			return fmt.Errorf("%s must be set before starting all", name)
		}
	}
	if ip := net.ParseIP(getenv("WARP_HOST_IPV4")); ip == nil || ip.To4() == nil {
		return errors.New("WARP_HOST_IPV4 must be an IPv4 listen address")
	}
	portMappings := map[int]int{}
	for _, pair := range strings.Split(getenv("WARP_PORTS"), ",") {
		parts := strings.Split(pair, ":")
		if len(parts) != 2 {
			return errors.New("WARP_PORTS must contain service_port:host_port pairs")
		}
		servicePort, serviceErr := strconv.Atoi(parts[0])
		hostPort, hostErr := strconv.Atoi(parts[1])
		if serviceErr != nil || hostErr != nil || servicePort < 1 || servicePort > 65535 || hostPort < 1 || hostPort > 65535 {
			return errors.New("WARP_PORTS ports must be integers in [1,65535]")
		}
		if _, exists := portMappings[servicePort]; exists {
			return fmt.Errorf("WARP_PORTS repeats service port %d", servicePort)
		}
		portMappings[servicePort] = hostPort
	}
	tcpPorts := []int{options.apiPort, options.connectPort, options.taskworkerPort}
	if options.gossipPort != 0 {
		tcpPorts = append(tcpPorts, options.gossipPort)
	}
	// This is Connect's existing contiguous internal exchange allocation.
	for port := 5080; port == 5080 || portMappings[port] != 0; port++ {
		tcpPorts = append(tcpPorts, port)
	}
	for groupIndex, servicePorts := range [][]int{tcpPorts, {443, 4053, 8053}} {
		usedPorts := map[int]bool{}
		for _, servicePort := range servicePorts {
			hostPort := portMappings[servicePort]
			if hostPort == 0 {
				if groupIndex == 0 {
					return fmt.Errorf("WARP_PORTS must map service port %d", servicePort)
				}
				continue
			}
			if usedPorts[hostPort] {
				return fmt.Errorf("operator listeners share host port %d", hostPort)
			}
			usedPorts[hostPort] = true
		}
	}
	return nil
}

// A runner owns its listeners and drain. Returning early cancels its peers.
type operatorService struct {
	name string
	run  func(context.Context, func(context.Context) func()) error
}

// Retains the original panic cause without allowing a cancellation-valued
// panic to be mistaken for an ordinary graceful runner return.
type operatorPanic struct {
	cause any
}

// Reports the panic without losing its service wrapper supplied by supervision.
func (self *operatorPanic) Error() string {
	return fmt.Sprintf("panic: %v", self.cause)
}

// Error-valued panics retain their original identity for callers and tests.
func (self *operatorPanic) Unwrap() error {
	err, _ := self.cause.(error)
	return err
}

// The production entry points are injectable only at the command boundary.
type serviceRunners struct {
	api        func(context.Context, api.RunOptions) error
	connect    func(context.Context, connectserver.RunOptions) error
	taskworker func(context.Context, taskworker.RunOptions) error
	gossip     func(context.Context, gossip.RunOptions) error
}

// The full task profile includes existing subnet settlement and recovery
// flows; the simulator's narrower subnet-operator profile is never selected.
func operatorServices(options commandOptions, runners serviceRunners) []operatorService {
	apiStatus := &router.WarpStatusState{}
	connectStatus := &router.WarpStatusState{}
	taskworkerStatus := &router.WarpStatusState{}
	privateHeapTarget := options.privateHeapTarget
	if privateHeapTarget == "config" {
		privateHeapTarget = ""
	}
	services := []operatorService{
		{name: "api", run: func(ctx context.Context, start func(context.Context) func()) error {
			return runners.api(ctx, api.RunOptions{Port: options.apiPort, WarpStatus: apiStatus, StartStatsPusher: start})
		}},
		{name: "connect", run: func(ctx context.Context, start func(context.Context) func()) error {
			return runners.connect(ctx, connectserver.RunOptions{Port: options.connectPort, MemoryOwnerLedger: options.memoryOwnerLedger, PrivateHeapProfileTarget: privateHeapTarget, WarpStatus: connectStatus, StartStatsPusher: start})
		}},
		{name: "taskworker", run: func(ctx context.Context, start func(context.Context) func()) error {
			return runners.taskworker(ctx, taskworker.RunOptions{Port: options.taskworkerPort, Count: options.workerCount, BatchSize: options.workerBatchSize, WorkloadProfile: taskworker.WorkloadProfileProduction, WarpStatus: taskworkerStatus, StartStatsPusher: start})
		}},
	}
	if options.gossipPort != 0 {
		services = append(services, operatorService{name: "gossip", run: func(ctx context.Context, start func(context.Context) func()) error {
			return runners.gossip(ctx, gossip.RunOptions{Port: options.gossipPort, StartStatsPusher: start})
		}})
	}
	return services
}

// Every exit path joins every runner. The one metrics owner starts only after
// every service admits its runtime and stays alive through their final drains.
func supervise(ctx context.Context, services []operatorService, startStatsPusher func(context.Context) func()) error {
	if ctx == nil || len(services) == 0 || startStatsPusher == nil {
		return errors.New("operator supervisor requires context, services and metrics owner")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	metricsCtx, cancelMetrics := context.WithCancel(context.Background())
	defer cancelMetrics()
	var stateLock sync.Mutex
	readyCount := 0
	flushStats := func() {}
	type serviceResult struct {
		name string
		err  error
	}
	results := make(chan serviceResult, len(services))
	for _, service := range services {
		go func() {
			result := serviceResult{name: service.name}
			defer func() {
				if recovered := recover(); recovered != nil {
					result.err = &operatorPanic{cause: recovered}
				}
				if result.err == nil && runCtx.Err() == nil {
					result.err = errors.New("stopped before operator shutdown")
				}
				results <- result
			}()
			var admitted sync.Once
			start := func(serviceCtx context.Context) func() {
				admitted.Do(func() {
					last := func() bool {
						stateLock.Lock()
						defer stateLock.Unlock()
						if runCtx.Err() != nil || serviceCtx.Err() != nil {
							return false
						}
						readyCount++
						return readyCount == len(services)
					}()
					if last && runCtx.Err() == nil {
						flushStats = startStatsPusher(metricsCtx)
					}
				})
				// The final process flush belongs after every runner has joined.
				return func() {}
			}
			result.err = service.run(runCtx, start)
		}()
	}
	var runErr error
	for range services {
		result := <-results
		if result.err != nil && (runCtx.Err() == nil || !cancellationOnly(result.err)) {
			runErr = errors.Join(runErr, fmt.Errorf("%s: %w", result.name, result.err))
		}
		cancel()
	}
	cancelMetrics()
	flushStats()
	return runErr
}

// Cancellation may be wrapped, but a joined operational failure must still
// fail the process even if its sibling was canceled during the same drain.
func cancellationOnly(err error) bool {
	if _, panicked := err.(*operatorPanic); panicked {
		return false
	}
	if err == context.Canceled {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !cancellationOnly(cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return cancellationOnly(wrapped.Unwrap())
	}
	return false
}

// The same ordinary migration as bringyourctl db migrate: every migration, then
// the provider earning boundary is retained or prepared from the loaded sn.yml.
// Neither step authorizes chain readiness.
func migrateDatabase(ctx context.Context, output io.Writer, migrate func(context.Context), ensure func(context.Context) (*server.ProviderEarningBoundary, bool, error)) error {
	if ctx == nil {
		return errors.New("database migration requires context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	migrate(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	boundary, prepared, err := ensure(ctx)
	if err != nil || boundary == nil {
		return err
	}
	return server.WriteProviderEarningBoundary(output, boundary, prepared)
}

// Omitted gossip is allowed for a feed-only extender network, but never when
// hello would advertise a peer identity with no listener in this operator.
func validateGossip(port int, config *controller.ExtenderConfig, configErr error) error {
	if configErr != nil {
		if port == 0 && errors.Is(configErr, server.ErrResourceNotFound) {
			return nil
		}
		return configErr
	}
	if config == nil {
		return errors.New("extender configuration returned no value")
	}
	if port == 0 && strings.TrimSpace(config.GossipIdentityKeyHex) != "" {
		return errors.New("extender.yml advertises gossip: set --gossip-port and map it in WARP_PORTS")
	}
	return nil
}

// Startup reads the same Warp resources as standalone services. Optional
// subnet operation becomes a required gate when explicitly requested.
func runCommand(ctx context.Context, options commandOptions, output io.Writer) error {
	switch options.command {
	case "help":
		_, err := io.WriteString(output, usage)
		return err
	case "version":
		version, err := server.Version()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(output, version)
		return err
	case "migrate":
		return migrateDatabase(ctx, output, server.ApplyDbMigrations, server.EnsureProviderPayoutBoundary)
	case "init-tasks":
		if err := router.CheckStartupReadiness(ctx); err != nil {
			return err
		}
		return taskworker.InitTasksForProfile(ctx, taskworker.WorkloadProfileProduction)
	case "run":
		if err := validateEnvironment(options, os.Getenv); err != nil {
			return err
		}
		if options.requireSubnet && !controller.StEnabled() {
			return errors.New("subnet required: configure valid enabled st.yml and URNETWORK_ST_PROFILE")
		}
		extenderConfig, extenderErr := controller.EnvExtenderConfig()
		if err := validateGossip(options.gossipPort, extenderConfig, extenderErr); err != nil {
			return err
		}
		services := operatorServices(options, serviceRunners{api: api.Run, connect: connectserver.Run, taskworker: taskworker.Run, gossip: gossip.Run})
		return supervise(ctx, services, server.StartStatsPusher)
	default:
		return errors.New("unknown all command")
	}
}

// Signals request one shared shutdown and return only after all service drains.
func main() {
	server.ScrubProcessLogs()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer stop()
	err := func() (returnErr error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				returnErr = fmt.Errorf("operator: %v", recovered)
			}
		}()
		options, err := parseCommand(os.Args[1:])
		if err != nil {
			return err
		}
		return runCommand(ctx, options, os.Stdout)
	}()
	if err != nil && (ctx.Err() == nil || !cancellationOnly(err)) {
		fmt.Fprintf(os.Stderr, "all: %v\n", err)
		os.Exit(1)
	}
}
