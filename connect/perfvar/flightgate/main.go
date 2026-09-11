// The pinned-provider collapse campaign driver and readout
// (connect/FLIGHTGATEFIX.md §6.2, §10 Phase 1).
//
//	campaign  builds one PERFVAR test binary per arm (stock and each candidate
//	          Connect tree) and runs the selected scenarios interleaved,
//	          repetition by repetition, writing one log per arm and run;
//	readout   parses the [perfvar] run records of a campaign directory and
//	          prints the per-cell attribution table: dead windows, failures,
//	          goodput, and the FLIGHTGATEFIX mechanism counters per arm.
//
// The shell entry is ../flightgate.sh. Measurements need the local server
// test stack (server/local/run-local.sh) and the documented WARP_* variables
// in the environment; the driver passes the environment through unchanged.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "campaign":
		err = runCampaign(os.Args[2:])
	case "readout":
		err = runReadout(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "flightgate: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  flightgate campaign -out DIR -arm stock=/path/to/connect [-arm a1=/path/to/connect-a1 ...] [filters]
  flightgate readout  -out DIR [-md FILE]

campaign filters (comma-separated sets, same values as CONNECT_PERFVAR_*):
  -route -profile -workload -direction -topology -resource -byte-count -lanes
  -runs N (repetitions, default 5)  -seed S (base seed; repetition r uses S+r-1)
  -server DIR (server module directory, default: the directory containing connect/perfvar)`)
}

// ---- campaign ----

type arm struct {
	name        string
	connectPath string
	sdkPath     string
}

// armSdkFlag maps an arm name to the SDK tree its binary must build against
// when the server module's default SDK replace is incompatible with that
// arm's Connect tree.
type armSdkFlag map[string]string

func (self *armSdkFlag) String() string { return fmt.Sprint(map[string]string(*self)) }

func (self *armSdkFlag) Set(value string) error {
	name, path, ok := strings.Cut(value, "=")
	if !ok || name == "" || path == "" {
		return fmt.Errorf("arm-sdk %q must be name=/path/to/sdk", value)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if *self == nil {
		*self = armSdkFlag{}
	}
	(*self)[name] = absolute
	return nil
}

type armFlag []arm

func (self *armFlag) String() string { return fmt.Sprint(*self) }

func (self *armFlag) Set(value string) error {
	name, path, ok := strings.Cut(value, "=")
	if !ok || name == "" || path == "" {
		return fmt.Errorf("arm %q must be name=/path/to/connect", value)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	*self = append(*self, arm{name: name, connectPath: absolute})
	return nil
}

type campaignManifest struct {
	Started   time.Time         `json:"started"`
	Finished  time.Time         `json:"finished"`
	Server    string            `json:"server"`
	ServerRev string            `json:"server_revision"`
	Arms      []manifestArm     `json:"arms"`
	Filters   map[string]string `json:"filters"`
	BaseSeed  int64             `json:"base_seed"`
	RunCount  int               `json:"run_count"`
	Runs      []manifestRun     `json:"runs"`
	Host      string            `json:"host"`
	GoVersion string            `json:"go_version"`
}

type manifestArm struct {
	Name       string `json:"name"`
	Connect    string `json:"connect"`
	ConnectRev string `json:"connect_revision"`
	Dirty      bool   `json:"connect_dirty"`
	Sdk        string `json:"sdk,omitempty"`
	SdkRev     string `json:"sdk_revision,omitempty"`
	SdkDirty   bool   `json:"sdk_dirty,omitempty"`
	Binary     string `json:"binary"`
}

type manifestRun struct {
	Arm      string        `json:"arm"`
	Run      int           `json:"run"`
	Seed     int64         `json:"seed"`
	Log      string        `json:"log"`
	Duration time.Duration `json:"duration_nanoseconds"`
	ExitCode int           `json:"exit_code"`
}

func runCampaign(args []string) error {
	flags := flag.NewFlagSet("campaign", flag.ContinueOnError)
	var arms armFlag
	out := flags.String("out", "", "campaign output directory (created)")
	serverDir := flags.String("server", "", "server module directory")
	flags.Var(&arms, "arm", "name=/path/to/connect (first arm is the control)")
	var armSdks armSdkFlag
	flags.Var(&armSdks, "arm-sdk", "name=/path/to/sdk (optional per-arm SDK tree)")
	runs := flags.Int("runs", 5, "repetitions per arm")
	seed := flags.Int64("seed", 20260910, "base seed")
	filters := map[string]*string{}
	for _, name := range []string{"route", "profile", "workload", "direction", "topology", "resource", "byte-count", "lanes", "feature"} {
		filters[name] = flags.String(name, "", "CONNECT_PERFVAR_"+strings.ToUpper(strings.ReplaceAll(name, "-", "_")))
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *out == "" || len(arms) == 0 {
		return errors.New("campaign needs -out and at least one -arm")
	}
	if *serverDir == "" {
		here, err := os.Getwd()
		if err != nil {
			return err
		}
		*serverDir = here
	}
	serverAbsolute, err := filepath.Abs(*serverDir)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(serverAbsolute, "connect", "perfvar")); err != nil {
		return fmt.Errorf("%s is not the server module directory: %w", serverAbsolute, err)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	outAbsolute, err := filepath.Abs(*out)
	if err != nil {
		return err
	}
	manifest := campaignManifest{
		Started:   time.Now(),
		Server:    serverAbsolute,
		ServerRev: gitRevision(serverAbsolute),
		Filters:   map[string]string{},
		BaseSeed:  *seed,
		RunCount:  *runs,
		GoVersion: commandOutput("go", "version"),
	}
	manifest.Host, _ = os.Hostname()
	envFilters := map[string]string{}
	for name, value := range filters {
		if *value == "" {
			continue
		}
		key := "CONNECT_PERFVAR_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
		if name == "lanes" {
			key = "CONNECT_PERFVAR_LOGICAL_LANES"
		}
		envFilters[key] = *value
		manifest.Filters[key] = *value
	}
	// Build one test binary per arm against its Connect tree through a
	// private modfile, so the server worktree's own go.mod is never edited.
	for _, arm := range arms {
		arm.sdkPath = armSdks[arm.name]
		binary, err := buildArm(serverAbsolute, outAbsolute, arm)
		if err != nil {
			return fmt.Errorf("build arm %s: %w", arm.name, err)
		}
		entry := manifestArm{
			Name:       arm.name,
			Connect:    arm.connectPath,
			ConnectRev: gitRevision(arm.connectPath),
			Dirty:      gitDirty(arm.connectPath),
			Binary:     binary,
		}
		if arm.sdkPath != "" {
			entry.Sdk = arm.sdkPath
			entry.SdkRev = gitRevision(arm.sdkPath)
			entry.SdkDirty = gitDirty(arm.sdkPath)
		}
		manifest.Arms = append(manifest.Arms, entry)
		fmt.Printf("built %s -> %s\n", arm.name, binary)
	}
	writeManifest := func() {
		encoded, _ := json.MarshalIndent(manifest, "", "  ")
		_ = os.WriteFile(filepath.Join(outAbsolute, "campaign.json"), encoded, 0o644)
	}
	writeManifest()
	// Interleave: every repetition runs every arm, rotating the starting arm
	// so no arm always follows a warm or cold host.
	packageDir := filepath.Join(serverAbsolute, "connect", "perfvar")
	for run := 1; run <= *runs; run += 1 {
		rotation := (run - 1) % len(manifest.Arms)
		order := append(slices.Clone(manifest.Arms[rotation:]), manifest.Arms[:rotation]...)
		for _, arm := range order {
			runSeed := *seed + int64(run-1)
			logPath := filepath.Join(outAbsolute, arm.Name, fmt.Sprintf("run-%02d.log", run))
			if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
				return err
			}
			fmt.Printf("run %d arm %s seed %d -> %s\n", run, arm.Name, runSeed, logPath)
			start := time.Now()
			exitCode, err := executeRun(packageDir, arm.Binary, logPath, runSeed, envFilters)
			manifest.Runs = append(manifest.Runs, manifestRun{
				Arm:      arm.Name,
				Run:      run,
				Seed:     runSeed,
				Log:      logPath,
				Duration: time.Since(start),
				ExitCode: exitCode,
			})
			writeManifest()
			if err != nil {
				fmt.Printf("  exit %d (%v)\n", exitCode, err)
			} else {
				fmt.Printf("  ok in %s\n", time.Since(start).Round(time.Second))
			}
		}
	}
	manifest.Finished = time.Now()
	writeManifest()
	return nil
}

func buildArm(serverDir string, outDir string, arm arm) (string, error) {
	buildDir := filepath.Join(outDir, "build", arm.name)
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		return "", err
	}
	modfile := filepath.Join(buildDir, "go.mod")
	for _, name := range []string{"go.mod", "go.sum"} {
		content, err := os.ReadFile(filepath.Join(serverDir, name))
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(buildDir, name), content, 0o644); err != nil {
			return "", err
		}
	}
	editArgs := []string{"mod", "edit", "-modfile=" + modfile,
		"-replace=github.com/urnetwork/connect=" + arm.connectPath}
	if arm.sdkPath != "" {
		editArgs = append(editArgs, "-replace=github.com/urnetwork/sdk="+arm.sdkPath)
	}
	edit := exec.Command("go", editArgs...)
	edit.Dir = serverDir
	if output, err := edit.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go mod edit: %v: %s", err, output)
	}
	binary := filepath.Join(buildDir, "perfvar.test")
	build := exec.Command("go", "test", "-c", "-modfile="+modfile, "-o", binary, "./connect/perfvar")
	build.Dir = serverDir
	build.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if output, err := build.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go test -c: %v: %s", err, output)
	}
	return binary, nil
}

func executeRun(
	packageDir string,
	binary string,
	logPath string,
	seed int64,
	envFilters map[string]string,
) (int, error) {
	logFile, err := os.Create(logPath)
	if err != nil {
		return -1, err
	}
	defer logFile.Close()
	command := exec.Command(binary, "-test.run", "^TestPerformanceVariations$", "-test.v", "-test.timeout", "0", "-test.count", "1")
	command.Dir = packageDir
	env := os.Environ()
	env = append(env,
		"CONNECT_PERFVAR_MEASURE=1",
		"CONNECT_PERFVAR_RUN_COUNT=1",
		"CONNECT_PERFVAR_SEED="+strconv.FormatInt(seed, 10),
	)
	for key, value := range envFilters {
		env = append(env, key+"="+value)
	}
	command.Env = env
	command.Stdout = logFile
	command.Stderr = logFile
	err = command.Run()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), err
	}
	return -1, err
}

func gitRevision(dir string) string {
	return strings.TrimSpace(commandOutputIn(dir, "git", "rev-parse", "HEAD"))
}

func gitDirty(dir string) bool {
	return strings.TrimSpace(commandOutputIn(dir, "git", "status", "--porcelain")) != ""
}

func commandOutput(name string, args ...string) string {
	return commandOutputIn("", name, args...)
}

func commandOutputIn(dir string, name string, args ...string) string {
	command := exec.Command(name, args...)
	command.Dir = dir
	output, err := command.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// ---- readout ----

// runRecord is the subset of a [perfvar] run record the readout uses,
// decoded generically so the readout never lags the harness schema.
type runRecord struct {
	arm       string
	run       int
	scenario  string
	route     string
	profile   string
	workload  string
	dir       string
	correct   bool
	stage     string
	reason    string
	invalid   string
	goodput   float64 // Mbit/s
	duration  time.Duration
	windows   int
	dead      int
	firstDead time.Duration
	worst     float64
	memP95    float64
	memMax    float64
	memAbove  int
	// latency-under-load primary: probe latency and delivery under load
	loadedP95     float64 // ms
	loadedSuccess float64
	loadedAttempt float64
	// counters keyed by name; provider and device are summed where the
	// mechanism is symmetric and kept apart where the direction matters.
	counters map[string]float64
}

var counterNames = []string{
	"flight_wait",
	"flight_blocked_with_reliable_capacity",
	"flight_gap",
	"flight_gap_reorder_suspected",
	"flight_timeout",
	"flight_reduction",
	"timeout_resend_writes",
	"timeout_resend_with_recent_progress",
	"timeout_resend_deferred",
	"selective_gap_writes",
	"ack_writes_p2p",
	"ack_writes_relay",
	"ack_wait_p2p_ms",
	"ack_timeouts_p2p",
	"fast_send_msgs",
	"fast_recv_msgs",
	"reassembly_evictions",
	"egress_pkts_p2p",
	"egress_pkts_relay",
}

func runReadout(args []string) error {
	flags := flag.NewFlagSet("readout", flag.ContinueOnError)
	out := flags.String("out", "", "campaign output directory")
	markdown := flags.String("md", "", "write the markdown report to this file as well")
	control := flags.String("control", "", "arm to attribute against (default: stock, else the first arm)")
	extra := flags.String("extra", "", "comma-separated campaign directories to fold in as extra arms")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("readout needs -out")
	}
	records, err := loadRecords(*out)
	if err != nil {
		return err
	}
	for _, directory := range strings.Split(*extra, ",") {
		directory = strings.TrimSpace(directory)
		if directory == "" {
			continue
		}
		more, err := loadRecords(directory)
		if err != nil {
			return err
		}
		suffix := filepath.Base(directory)
		for index := range more {
			more[index].arm = more[index].arm + "@" + suffix
		}
		records = append(records, more...)
	}
	if len(records) == 0 {
		return fmt.Errorf("no [perfvar] run records under %s", *out)
	}
	report := renderReport(*out, records, *control)
	fmt.Print(report)
	if *markdown != "" {
		return os.WriteFile(*markdown, []byte(report), 0o644)
	}
	return nil
}

func loadRecords(root string) ([]runRecord, error) {
	var records []runRecord
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			return err
		}
		arm := filepath.Base(filepath.Dir(path))
		run := 0
		if _, err := fmt.Sscanf(entry.Name(), "run-%d.log", &run); err != nil {
			run = 0
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		reader := bufio.NewReaderSize(file, 1<<20)
		for {
			line, err := reader.ReadString('\n')
			if index := strings.Index(line, "[perfvar] {"); 0 <= index {
				if record, ok := parseRecord(line[index+len("[perfvar] "):]); ok {
					record.arm = arm
					record.run = run
					records = append(records, record)
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	return records, err
}

func parseRecord(text string) (runRecord, bool) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &raw); err != nil {
		return runRecord{}, false
	}
	if str(raw, "record_type") != "run" {
		return runRecord{}, false
	}
	record := runRecord{
		scenario:      str(raw, "scenario_hash"),
		route:         str(raw, "scenario", "route"),
		profile:       str(raw, "scenario", "application_access_and_p2p_profile", "name"),
		workload:      str(raw, "scenario", "workload"),
		dir:           str(raw, "scenario", "direction"),
		correct:       boolean(raw, "correct"),
		stage:         str(raw, "failure_stage"),
		reason:        str(raw, "failure_reason"),
		invalid:       str(raw, "invalid_reason"),
		goodput:       num(raw, "tunneled", "goodput_gigabits_per_second") * 1000,
		duration:      time.Duration(num(raw, "tunneled", "duration_nanoseconds")),
		windows:       int(num(raw, "progress", "window_count")),
		dead:          int(num(raw, "progress", "dead_window_count")),
		firstDead:     time.Duration(num(raw, "progress", "first_dead_window_offset_nanoseconds")),
		worst:         num(raw, "progress", "worst_window_megabits_per_second"),
		memP95:        num(raw, "memory", "heap_and_stack_inuse_p95_bytes"),
		memMax:        num(raw, "memory", "heap_and_stack_inuse_max_bytes"),
		memAbove:      int(num(raw, "memory", "samples_above_ceiling")),
		loadedP95:     num(raw, "tunneled", "loaded_latency", "p95_nanoseconds") / float64(time.Millisecond),
		loadedSuccess: num(raw, "tunneled", "loaded_probe_success_count"),
		loadedAttempt: num(raw, "tunneled", "loaded_probe_attempt_count"),
		counters:      map[string]float64{},
	}
	both := func(name string, path ...string) {
		record.counters[name] = num(raw, append([]string{"carrier", "device_" + path[0]}, path[1:]...)...) +
			num(raw, append([]string{"carrier", "provider_" + path[0]}, path[1:]...)...)
	}
	both("flight_wait", "send_recovery", "unreliable_flight_wait_count")
	both("flight_blocked_with_reliable_capacity", "send_recovery", "unreliable_flight_blocked_with_reliable_capacity")
	both("flight_gap", "send_recovery", "unreliable_flight_gap_count")
	both("flight_gap_reorder_suspected", "send_recovery", "unreliable_flight_gap_reorder_suspected")
	both("flight_timeout", "send_recovery", "unreliable_flight_timeout_count")
	both("flight_reduction", "send_recovery", "unreliable_flight_reduction_count")
	both("timeout_resend_writes", "send_recovery", "timeout_resend_write_count")
	both("timeout_resend_with_recent_progress", "send_recovery", "timeout_resend_with_recent_cumulative_progress")
	both("timeout_resend_deferred", "send_recovery", "timeout_resend_defer_count")
	both("selective_gap_writes", "send_recovery", "selective_gap_write_count")
	both("ack_writes_p2p", "receive_handoff", "ack_route_write_count_by_transport", "p2p")
	// The exchange lane of a mixed route was labelled "unknown" by campaigns
	// recorded before the harness wrapper exposed its transport type, and
	// "h1" afterwards; both are the relay.
	both("ack_writes_relay", "receive_handoff", "ack_route_write_count_by_transport", "h1")
	record.counters["ack_writes_relay"] += num(raw, "carrier", "device_receive_handoff", "ack_route_write_count_by_transport", "unknown") +
		num(raw, "carrier", "provider_receive_handoff", "ack_route_write_count_by_transport", "unknown")
	both("ack_wait_p2p_ms", "receive_handoff", "ack_route_write_wait_by_transport_nanoseconds", "p2p")
	record.counters["ack_wait_p2p_ms"] /= float64(time.Millisecond)
	both("ack_timeouts_p2p", "receive_handoff", "ack_route_write_timeout_by_transport", "p2p")
	both("fast_send_msgs", "p2p", "FastSendMessageCount")
	both("fast_recv_msgs", "p2p", "FastReceiveMessageCount")
	both("reassembly_evictions", "p2p", "FastReassemblyEvictionCount")
	both("egress_pkts_p2p", "packet_stats", "transport_stats", "p2p", "remote_egress_packet_count")
	both("egress_pkts_relay", "packet_stats", "transport_stats", "h1", "remote_egress_packet_count")
	record.counters["egress_pkts_relay"] += num(raw, "carrier", "device_packet_stats", "transport_stats", "unknown", "remote_egress_packet_count") +
		num(raw, "carrier", "provider_packet_stats", "transport_stats", "unknown", "remote_egress_packet_count")
	return record, true
}

func lookup(raw map[string]any, path ...string) any {
	var current any = raw
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current, ok = object[key]
		if !ok {
			return nil
		}
	}
	return current
}

func str(raw map[string]any, path ...string) string {
	value, _ := lookup(raw, path...).(string)
	return value
}

func num(raw map[string]any, path ...string) float64 {
	value, _ := lookup(raw, path...).(float64)
	return value
}

func boolean(raw map[string]any, path ...string) bool {
	value, _ := lookup(raw, path...).(bool)
	return value
}

type cellKey struct {
	route, profile, workload, dir string
}

func (self cellKey) String() string {
	return fmt.Sprintf("%s / %s / %s / %s", self.route, self.profile, self.workload, self.dir)
}

type cellSummary struct {
	runs, correct, failed, invalid int
	headroomInvalid                int
	deadWindows, windows, deadRuns int
	worst                          float64
	memP95s                        []float64
	memMax                         float64
	loadedP95s                     []float64
	loadedSuccess, loadedAttempt   float64
	memAbove                       int
	goodputs                       []float64
	stages                         map[string]int
	counters                       map[string]float64
}

func summarize(records []runRecord) map[string]map[cellKey]*cellSummary {
	summary := map[string]map[cellKey]*cellSummary{}
	for _, record := range records {
		cells := summary[record.arm]
		if cells == nil {
			cells = map[cellKey]*cellSummary{}
			summary[record.arm] = cells
		}
		key := cellKey{record.route, record.profile, record.workload, record.dir}
		cell := cells[key]
		if cell == nil {
			cell = &cellSummary{stages: map[string]int{}, counters: map[string]float64{}, worst: math.Inf(1)}
			cells[key] = cell
		}
		cell.runs += 1
		if record.correct {
			cell.correct += 1
			cell.goodputs = append(cell.goodputs, record.goodput)
			if 0 < record.loadedAttempt {
				cell.loadedP95s = append(cell.loadedP95s, record.loadedP95)
				cell.loadedSuccess += record.loadedSuccess
				cell.loadedAttempt += record.loadedAttempt
			}
			if record.invalid != "" {
				cell.invalid += 1
				if strings.Contains(record.invalid, "calibration") {
					cell.headroomInvalid += 1
				}
			}
		} else {
			cell.failed += 1
			cell.stages[record.stage] += 1
		}
		cell.deadWindows += record.dead
		cell.windows += record.windows
		if 0 < record.dead {
			cell.deadRuns += 1
		}
		if 0 < record.windows && record.worst < cell.worst {
			cell.worst = record.worst
		}
		if 0 < record.memP95 {
			cell.memP95s = append(cell.memP95s, record.memP95)
		}
		cell.memMax = math.Max(cell.memMax, record.memMax)
		cell.memAbove += record.memAbove
		for name, value := range record.counters {
			cell.counters[name] += value
		}
	}
	return summary
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Clone(values)
	sort.Float64s(sorted)
	return sorted[(len(sorted)-1)/2]
}

func renderReport(root string, records []runRecord, controlArm string) string {
	summary := summarize(records)
	arms := make([]string, 0, len(summary))
	for arm := range summary {
		arms = append(arms, arm)
	}
	sort.Strings(arms)
	// The control is the requested arm, else "stock" when present, else the
	// first arm in name order.
	control := arms[0]
	if slices.Contains(arms, "stock") {
		control = "stock"
	}
	if controlArm != "" {
		if !slices.Contains(arms, controlArm) {
			return fmt.Sprintf("control arm %q is not one of %s\n", controlArm, strings.Join(arms, ", "))
		}
		control = controlArm
	}
	cellSet := map[cellKey]bool{}
	for _, cells := range summary {
		for key := range cells {
			cellSet[key] = true
		}
	}
	cells := make([]cellKey, 0, len(cellSet))
	for key := range cellSet {
		cells = append(cells, key)
	}
	sort.Slice(cells, func(i, j int) bool { return cells[i].String() < cells[j].String() })
	var b strings.Builder
	fmt.Fprintf(&b, "# flightgate readout: %s\n\n", root)
	fmt.Fprintf(&b, "%d run records, arms: %s (control: %s)\n\n", len(records), strings.Join(arms, ", "), control)
	fmt.Fprintln(&b, "## Outcome per cell")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| Cell | Arm | Runs | Correct | Failed (stage) | Harness-invalid (calibration) | Dead windows / windows | Runs with dead window | Worst window Mbit/s | Median goodput Mbit/s | Loaded p95 ms | Loaded probes delivered % | Memory p95 median MiB | Memory max MiB | Samples > 24 MiB |")
	fmt.Fprintln(&b, "| --- | --- | ---: | ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |")
	for _, key := range cells {
		for _, arm := range arms {
			cell := summary[arm][key]
			if cell == nil {
				continue
			}
			stages := []string{}
			for stage, count := range cell.stages {
				stages = append(stages, fmt.Sprintf("%s:%d", stage, count))
			}
			sort.Strings(stages)
			worst := "n/a"
			if !math.IsInf(cell.worst, 1) {
				worst = fmt.Sprintf("%.2f", cell.worst)
			}
			goodput := "n/a"
			if 0 < len(cell.goodputs) {
				// Low-bar cells run at tens to hundreds of kbit/s.
				goodput = fmt.Sprintf("%.1f", median(cell.goodputs))
				if median(cell.goodputs) < 1 {
					goodput = fmt.Sprintf("%.3f", median(cell.goodputs))
				}
			}
			loadedP95, delivered := "–", "–"
			if 0 < cell.loadedAttempt {
				loadedP95 = fmt.Sprintf("%.0f", median(cell.loadedP95s))
				delivered = fmt.Sprintf("%.1f", 100*cell.loadedSuccess/cell.loadedAttempt)
			}
			fmt.Fprintf(&b, "| %s | %s | %d | %d | %d (%s) | %d (%d) | %d / %d | %d | %s | %s | %s | %s | %.2f | %.2f | %d |\n",
				key, arm, cell.runs, cell.correct, cell.failed, strings.Join(stages, " "),
				cell.invalid, cell.headroomInvalid,
				cell.deadWindows, cell.windows, cell.deadRuns, worst, goodput, loadedP95, delivered,
				median(cell.memP95s)/mib, cell.memMax/mib, cell.memAbove)
		}
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Mechanism counters per cell (device + provider, summed over runs)")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "| Cell | Arm | %s |\n", strings.Join(counterNames, " | "))
	fmt.Fprintf(&b, "| --- | --- |%s\n", strings.Repeat(" ---: |", len(counterNames)))
	for _, key := range cells {
		for _, arm := range arms {
			cell := summary[arm][key]
			if cell == nil {
				continue
			}
			fmt.Fprintf(&b, "| %s | %s |", key, arm)
			for _, name := range counterNames {
				fmt.Fprintf(&b, " %s |", formatCounter(cell.counters[name]))
			}
			fmt.Fprintln(&b)
		}
	}
	if 1 < len(arms) {
		fmt.Fprintln(&b)
		fmt.Fprintf(&b, "## Attribution against %s (candidate minus control, per cell)\n", control)
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "| Cell | Candidate | Dead windows | Failed runs | Median goodput Mbit/s | selective_gap_writes | flight_wait | blocked_with_reliable_capacity | gap_reorder_suspected | flight_timeout | timeout_resend_recent_progress | ack_writes_p2p | ack_writes_relay | ack_timeouts_p2p | Loaded p95 ms | Loaded delivered % | Memory p95 median MiB | Memory gate |")
		fmt.Fprintln(&b, "| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |")
		for _, key := range cells {
			base := summary[control][key]
			if base == nil {
				continue
			}
			for _, arm := range arms {
				if arm == control {
					continue
				}
				cell := summary[arm][key]
				if cell == nil {
					continue
				}
				delta := func(name string) string {
					return formatCounter(cell.counters[name] - base.counters[name])
				}
				// MEMSTEADY guardrail: a candidate p95 above the control's, or any
				// sample above 24 MiB where the control had none, is a REGRESSION
				// for the item regardless of throughput.
				memoryGate := "ok"
				if median(cell.memP95s) > median(base.memP95s) || (0 < cell.memAbove && base.memAbove == 0) {
					memoryGate = "REGRESSION"
				}
				fmt.Fprintf(&b, "| %s | %s | %+d | %+d | %+.1f | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %+.2f | %s |\n",
					key, arm,
					cell.deadWindows-base.deadWindows,
					cell.failed-base.failed,
					median(cell.goodputs)-median(base.goodputs),
					delta("selective_gap_writes"),
					delta("flight_wait"),
					delta("flight_blocked_with_reliable_capacity"),
					delta("flight_gap_reorder_suspected"),
					delta("flight_timeout"),
					delta("timeout_resend_with_recent_progress"),
					delta("ack_writes_p2p"),
					delta("ack_writes_relay"),
					delta("ack_timeouts_p2p"),
					loadedDelta(cell, base),
					deliveredDelta(cell, base),
					(median(cell.memP95s)-median(base.memP95s))/mib,
					memoryGate,
				)
			}
		}
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Failed runs")
	fmt.Fprintln(&b)
	for _, record := range records {
		if record.correct {
			continue
		}
		reason := record.reason
		if 160 < len(reason) {
			reason = reason[:160] + "…"
		}
		fmt.Fprintf(&b, "- %s run %d %s: %s: %s (dead windows %d/%d, first dead at %s)\n",
			record.arm, record.run, cellKey{record.route, record.profile, record.workload, record.dir},
			record.stage, reason, record.dead, record.windows, record.firstDead.Round(time.Second))
	}
	return b.String()
}

const mib = 1024 * 1024

// loadedDelta and deliveredDelta render the latency-under-load primary
// against the control, or a dash for cells without probes.
func loadedDelta(cell *cellSummary, base *cellSummary) string {
	if cell.loadedAttempt == 0 || base.loadedAttempt == 0 {
		return "–"
	}
	return fmt.Sprintf("%+.0f", median(cell.loadedP95s)-median(base.loadedP95s))
}

func deliveredDelta(cell *cellSummary, base *cellSummary) string {
	if cell.loadedAttempt == 0 || base.loadedAttempt == 0 {
		return "–"
	}
	return fmt.Sprintf("%+.1f", 100*cell.loadedSuccess/cell.loadedAttempt-100*base.loadedSuccess/base.loadedAttempt)
}

func formatCounter(value float64) string {
	if value == math.Trunc(value) {
		return strconv.FormatInt(int64(value), 10)
	}
	return strconv.FormatFloat(value, 'f', 1, 64)
}
