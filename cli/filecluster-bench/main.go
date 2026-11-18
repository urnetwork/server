// server/cli/filecluster-bench (EVSB-M2-21, EVSB-M3-03; §22.8's storage benchmark, D-TEST-11, D-OPS-17, D-OPS-19):
// the harness for §22.8's nvme and mix rows on a metadata cluster, and its hdd, hosts and trash rows, and the report
// that turns the nvme row's runs into D-TEST-11's gate, §11.13's price table and the block-size choice. Run from a
// server checkout in the monoroot:
//
//	filecluster-bench fs116 -monoroot <dir> -out fs116.json
//	filecluster-bench ops -clusters <file> -cluster <id> -block <1024|4096> -monoroot <dir> -fs116 fs116.json -out ops-1k.json
//	filecluster-bench mix -clusters <file> -block <1024|4096> -monoroot <dir> -fs116 fs116.json -out mix-1k.json
//	filecluster-bench report -in ops-1k.json -in mix-1k.json -in ops-4k.json -in mix-4k.json -out report.txt -price_table price-table.json
//	filecluster-bench hdd|hosts|trash ... (hdd.go, hosts.go, trash.go)
//	filecluster-bench index -clusters <file> -load <name> -monoroot <dir> -out index.json (index.go, M10)
//	filecluster-bench drop -clusters <file> -capacity <ops> -monoroot <dir> -out drop.json -figures drop.txt (drop.go, M10)
//	filecluster-bench projection -m2 ops-1k.json -m2 mix-1k.json ... -index index.json -out projection.txt (projection.go, D-TEST-13)
//
// -clusters is a vault file's text (file-clusters-<service>.yml) the operator hands it, as filecluster-suite's: the
// harness reads no vault and prints no secret. fs116 runs FS-116 on the monoroot's sidecar commit and records it;
// every run but a smoke refuses to start until that record shows FS-116 passed on the very commit set it runs
// (D-TEST-11), on trees with no changes. -smoke runs anything at any scale, records it as a smoke, and no smoke is
// ever a gate's evidence. Each run writes one measurement file per block size; mkfs at the other block size, and the
// playbook run that does it, is the operator's (§19.6's mkfs row; FILE-CLUSTERS.md), so the two block sizes are two
// runs of each mode.
//
// Exit: 0 when the run finished, or the report's gate passed; 1 when the report's gate failed or a run failed;
// 2 on usage, or when it cannot start or judge. The runs themselves are external: on M2's staging cluster, the
// production hosts' PM9A3 set (§19.11), and again on production before M3's pilot.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest/load"

	"github.com/urnetwork/server"
)

// A usage error: exit 2.
var errUsage = errors.New("usage: filecluster-bench fs116|ops|mix|report|hdd|hosts|trash|verdict|hoststats|index|drop|projection [flags]")

// A run that could not start: exit 2.
type startError struct {
	err error
}

func (self *startError) Error() string { return self.err.Error() }
func (self *startError) Unwrap() error { return self.err }

func refuseStart(format string, args ...any) error {
	return &startError{err: fmt.Errorf(format, args...)}
}

// A mode: its flags parsed from args, its outcome as pass. stdout is where a mode without -out writes.
type benchMode func(ctx context.Context, args []string, stdout io.Writer) (pass bool, err error)

func benchModes() map[string]benchMode {
	return map[string]benchMode{
		"fs116":      runFs116Mode,
		"ops":        runOpsMode,
		"mix":        runMixMode,
		"report":     runReportMode,
		"hdd":        runHddMode,
		"hosts":      runHostsMode,
		"trash":      runTrashMode,
		"verdict":    runVerdictMode,
		"hoststats":  runHostStatsMode,
		"index":      runIndexMode,
		"drop":       runDropMode,
		"projection": runProjectionMode,
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// The exit code of one invocation: see the file header.
func run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, errUsage)
		return 2
	}
	mode, ok := benchModes()[args[0]]
	if !ok {
		fmt.Fprintln(stderr, errUsage)
		return 2
	}
	pass, err := mode(ctx, args[1:], stdout)
	if err != nil {
		fmt.Fprintf(stderr, "filecluster-bench %s: %v\n", args[0], err)
		var start *startError
		if errors.Is(err, flag.ErrHelp) || errors.As(err, &start) || errors.Is(err, errUsage) {
			return 2
		}
		return 1
	}
	if !pass {
		return 1
	}
	return 0
}

// The flags every run against a cluster takes.
type runFlags struct {
	clustersPath     string
	clusterId        string
	blockByteCount   int64
	dataShards       int
	monoroot         string
	fs116Path        string
	outPath          string
	label            string
	smoke            bool
	counterPort      int
	hostStatsCommand string
}

func (self *runFlags) register(flags *flag.FlagSet, clusterRequired bool) {
	flags.StringVar(&self.clustersPath, "clusters", "", "a vault file's text naming the clusters (file-clusters-<service>.yml)")
	if clusterRequired {
		flags.StringVar(&self.clusterId, "cluster", "", "the cluster to run on")
	}
	flags.Int64Var(&self.blockByteCount, "block", 0, "the XFS block size the cluster's drives were formatted with: 1024 or 4096")
	flags.IntVar(&self.dataShards, "data_shards", 4, "the data shards an object spreads over: 4 at EC:4 on 8 drives")
	flags.StringVar(&self.monoroot, "monoroot", "", "the monoroot whose sidecar and server commits this build is")
	flags.StringVar(&self.fs116Path, "fs116", "", "the fs116 mode's record that FS-116 passed on this build")
	flags.StringVar(&self.outPath, "out", "", "where the measurement file goes; stdout when empty")
	flags.StringVar(&self.label, "label", "", "a name for the run")
	flags.BoolVar(&self.smoke, "smoke", false, "a run at any scale that proves nothing and needs no FS-116 record")
	flags.IntVar(&self.counterPort, "counter_port", server.DefaultFileClusterCountersSettings().Port, "each storage host's counter exporter port")
	flags.StringVar(&self.hostStatsCommand, "host_stats_command", "", "a command whose output is a host's hoststats answer; {host} names the host and {paths} its drives' paths")
}

// The run's commit set and FS-116 record, refusing a run that is not a smoke unless FS-116 passed on exactly this
// build with no changes in its trees (D-TEST-11).
func (self *runFlags) evidence(measurement *Measurement) error {
	if self.monoroot == "" {
		if !self.smoke {
			return refuseStart("a run that is not a smoke needs -monoroot, so its build is named")
		}
		measurement.Notes = append(measurement.Notes, "no -monoroot: the build is not named")
		return nil
	}
	commitSet, err := load.ReadCommitSet(self.monoroot, benchRepos...)
	if err != nil {
		return refuseStart("%v", err)
	}
	measurement.CommitSet = commitSet
	if err := checkBinaryCommit(commitSet); err != nil {
		if !self.smoke {
			return refuseStart("%v", err)
		}
		measurement.Notes = append(measurement.Notes, err.Error())
	}
	if self.fs116Path == "" {
		if !self.smoke {
			return refuseStart("FS-116 must pass on this build before the run (D-TEST-11): give the fs116 mode's record with -fs116")
		}
		measurement.Notes = append(measurement.Notes, "no FS-116 record")
		return nil
	}
	evidence := &Fs116Evidence{}
	data, err := os.ReadFile(self.fs116Path)
	if err != nil {
		return refuseStart("%v", err)
	}
	if err := json.Unmarshal(data, evidence); err != nil {
		return refuseStart("%s: %v", self.fs116Path, err)
	}
	if err := evidence.Check(commitSet); err != nil {
		if !self.smoke {
			return refuseStart("FS-116 has not passed on this build (D-TEST-11): %v", err)
		}
		measurement.Notes = append(measurement.Notes, err.Error())
	}
	measurement.Fs116 = evidence
	return nil
}

// The repositories whose commits name a build of the harness and what it runs.
var benchRepos = []string{"sidecar", "server"}

// The binary's own server commit, where go build stamped one, against the checkout's: a binary built from another
// commit than the one named is not this build.
func checkBinaryCommit(commitSet map[string]string) error {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return nil
	}
	revision := ""
	modified := false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if revision == "" {
		return nil
	}
	if modified {
		return errors.New("the binary was built from a server tree with changes")
	}
	if checkout := strings.TrimSuffix(commitSet["server"], "+dirty"); checkout != revision {
		return fmt.Errorf("the binary was built from server %s, the checkout is %s", revision, checkout)
	}
	return nil
}

// The clusters the file names, their checks run once so admin info is read before the first request.
func openClusters(ctx context.Context, clustersPath string) (*server.FileClusters, error) {
	if clustersPath == "" {
		return nil, refuseStart("-clusters names no file")
	}
	text, err := os.ReadFile(clustersPath)
	if err != nil {
		return nil, refuseStart("%v", err)
	}
	configs, err := server.ParseFileClusterConfigs(text)
	if err != nil {
		return nil, refuseStart("%v", err)
	}
	clusters, err := server.NewFileClusters(ctx, configs, nil)
	if err != nil {
		return nil, refuseStart("%v", err)
	}
	clusters.Check(ctx)
	return clusters, nil
}

// A target on one cluster of clusters, with its counters and host stats.
func (self *runFlags) target(ctx context.Context, clusters *server.FileClusters, clusterId string) (*BenchTarget, error) {
	cluster, err := clusters.Cluster(clusterId)
	if err != nil {
		return nil, refuseStart("%v", err)
	}
	countersSettings := server.DefaultFileClusterCountersSettings()
	countersSettings.Port = self.counterPort
	target := &BenchTarget{
		ClusterId: clusterId,
		Cluster:   cluster,
		Counters:  server.NewFileClusterCounters(clusters, countersSettings),
	}
	if self.hostStatsCommand != "" {
		target.HostStats = newCommandHostStats(self.hostStatsCommand, func(clusterId string) *server.FileClusterAdminInfo {
			info, err := clusters.ReadAdminInfo(ctx, clusterId)
			if err != nil {
				return nil
			}
			return info
		}, nil)
	}
	return target, nil
}

// The measurement's header: who ran what, on which build and cluster.
func (self *runFlags) header(ctx context.Context, mode string, clusters *server.FileClusters, clusterId string, settings map[string]string) (*Measurement, error) {
	if self.blockByteCount != 1024 && self.blockByteCount != 4*1024 {
		return nil, refuseStart("-block must be 1024 or 4096, the XFS block size the drives were formatted with (§19.6)")
	}
	host, _ := os.Hostname()
	measurement := &Measurement{
		Mode:           mode,
		Label:          self.label,
		Host:           host,
		Smoke:          self.smoke,
		Settings:       settings,
		ClusterId:      clusterId,
		BlockByteCount: self.blockByteCount,
		DataShardCount: self.dataShards,
		CommitSet:      map[string]string{},
	}
	if self.smoke {
		measurement.Notes = append(measurement.Notes, "run with -smoke: no evidence")
	}
	if info, err := clusters.ReadAdminInfo(ctx, clusterId); err == nil {
		builds := []string{}
		for _, host := range info.Hosts {
			if !slices.Contains(builds, host.CommitId) {
				builds = append(builds, host.CommitId)
			}
		}
		sort.Strings(builds)
		measurement.Settings["minio_build"] = strings.Join(builds, ",")
		if measurement.DriveCount == 0 {
			measurement.DriveCount = len(info.Drives)
		}
	}
	if err := self.evidence(measurement); err != nil {
		return nil, err
	}
	return measurement, nil
}

// An hdd, hosts or trash file's header: the build when -monoroot names one, which a run that is not a smoke must.
func (self *runFlags) rowHeader(ctx context.Context, header *RowHeader, mode string, clusters *server.FileClusters, clusterId string, settings map[string]string) error {
	host, _ := os.Hostname()
	*header = RowHeader{Mode: mode, Label: self.label, Host: host, Smoke: self.smoke, Settings: settings, ClusterId: clusterId, CommitSet: map[string]string{}}
	if self.smoke {
		header.Notes = append(header.Notes, "run with -smoke: no evidence")
	}
	if clusterId != "" {
		if info, err := clusters.ReadAdminInfo(ctx, clusterId); err == nil {
			builds := []string{}
			for _, host := range info.Hosts {
				if !slices.Contains(builds, host.CommitId) {
					builds = append(builds, host.CommitId)
				}
			}
			sort.Strings(builds)
			header.Settings["minio_build"] = strings.Join(builds, ",")
		}
	}
	if self.monoroot == "" {
		if !self.smoke {
			return refuseStart("a run that is not a smoke needs -monoroot, so its build is named")
		}
		header.Notes = append(header.Notes, "no -monoroot: the build is not named")
		return nil
	}
	commitSet, err := load.ReadCommitSet(self.monoroot, benchRepos...)
	if err != nil {
		return refuseStart("%v", err)
	}
	header.CommitSet = commitSet
	return nil
}

// Writes an hdd, hosts or trash file to -out, or stdout.
func writeRowFile(outPath string, stdout io.Writer, measurement any) error {
	data, err := json.MarshalIndent(measurement, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if outPath == "" {
		_, err := stdout.Write(data)
		return err
	}
	return os.WriteFile(outPath, data, 0o644)
}

// An hdd, hosts or trash file into measurement, strict when asked: a field the shape lacks refuses it.
func parseRowFile(data []byte, measurement any, strict bool) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if strict {
		decoder.DisallowUnknownFields()
	}
	return decoder.Decode(measurement)
}

// Writes a measurement to -out, or stdout.
func writeMeasurement(outPath string, stdout io.Writer, measurement *Measurement) error {
	data, err := measurement.Json()
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if outPath == "" {
		_, err := stdout.Write(data)
		return err
	}
	return os.WriteFile(outPath, data, 0o644)
}

// A run's prefix: filecluster-bench/<16 random hex digits>/.
func runPrefix() string {
	var random [8]byte
	// never fails: since Go 1.24 a broken system generator crashes the program instead
	rand.Read(random[:])
	return "filecluster-bench/" + hex.EncodeToString(random[:]) + "/"
}

func runOpsMode(ctx context.Context, args []string, stdout io.Writer) (bool, error) {
	defaults := DefaultOpsSettings()
	settings := *defaults
	flags := flag.NewFlagSet("filecluster-bench ops", flag.ContinueOnError)
	common := &runFlags{}
	common.register(flags, true)
	flags.DurationVar(&settings.Duration, "duration", defaults.Duration, "each class's measured window")
	flags.Float64Var(&settings.Rate, "rate", defaults.Rate, "each class's operations a second, open loop")
	flags.DurationVar(&settings.IdleWindow, "idle", defaults.IdleWindow, "the window with no load before the classes")
	flags.IntVar(&settings.MaxInFlight, "max_in_flight", defaults.MaxInFlight, "the most operations in flight")
	if err := flags.Parse(args); err != nil {
		return false, err
	}
	if common.clusterId == "" {
		return false, refuseStart("-cluster names no cluster")
	}
	clusters, err := openClusters(ctx, common.clustersPath)
	if err != nil {
		return false, err
	}
	defer clusters.Close()
	if kind := clusters.ClusterKind(common.clusterId); kind != server.FileClusterKindMetadata {
		return false, refuseStart("%s is a %q cluster: the nvme row runs on a metadata cluster", common.clusterId, kind)
	}
	measurement, err := common.header(ctx, ModeOps, clusters, common.clusterId, map[string]string{
		"duration":     settings.Duration.String(),
		"rate":         fmt.Sprint(settings.Rate),
		"idle":         settings.IdleWindow.String(),
		"sizes":        fmt.Sprint(settings.ObjectByteCounts),
		"read_size":    fmt.Sprint(settings.ReadObjectByteCount),
		"list_page":    fmt.Sprint(settings.ListPageCount),
		"max_inflight": fmt.Sprint(settings.MaxInFlight),
	})
	if err != nil {
		return false, err
	}
	target, err := common.target(ctx, clusters, common.clusterId)
	if err != nil {
		return false, err
	}
	runErr := RunOps(ctx, target, &settings, runPrefix(), measurement)
	if err := writeMeasurement(common.outPath, stdout, measurement); err != nil {
		return false, err
	}
	if runErr != nil {
		return false, runErr
	}
	return true, nil
}

func runMixMode(ctx context.Context, args []string, stdout io.Writer) (bool, error) {
	defaults := DefaultMixSettings()
	settings := *defaults
	engineDefaults := DefaultEngineMixSettings()
	engineSettings := *engineDefaults
	flags := flag.NewFlagSet("filecluster-bench mix", flag.ContinueOnError)
	common := &runFlags{}
	common.register(flags, true)
	targetName := flags.String("target", "engine", "what drives the mix: engine, evsservice's engines in this process over the clusters")
	chronyAddress := flags.String("chrony_address", "", "chronyd's NTP address as host:port, for the engines' clock check; empty takes this host's clock as sure, which the run notes")
	flags.DurationVar(&settings.Preload, "preload", defaults.Preload, "each phase's preload, so the cluster holds that much of batches")
	flags.DurationVar(&settings.Warmup, "warmup", defaults.Warmup, "each phase's warm-up")
	flags.DurationVar(&settings.Steady, "steady", defaults.Steady, "each phase's measured window")
	flags.Float64Var(&settings.YearOnePushesPerSecond, "year_one", defaults.YearOnePushesPerSecond, "year one's pushes a second")
	flags.DurationVar(&settings.SampleInterval, "sample", defaults.SampleInterval, "how often the stack's series are read")
	phases := flags.String("phases", strings.Join(defaults.Phases, ","), "the phases in order")
	flags.IntVar(&engineSettings.ProcessCount, "processes", engineDefaults.ProcessCount, "evs processes the engine target runs")
	flags.IntVar(&engineSettings.NetworkCount, "networks", engineDefaults.NetworkCount, "networks the engine target writes")
	flags.IntVar(&engineSettings.WritersPerNetwork, "writers", engineDefaults.WritersPerNetwork, "writers in each network")
	flags.IntVar(&engineSettings.EntryWritesPerPush, "entries", engineDefaults.EntryWritesPerPush, "entry writes a push")
	if err := flags.Parse(args); err != nil {
		return false, err
	}
	settings.Phases = strings.Split(*phases, ",")
	if *targetName != "engine" {
		return false, refuseStart("no %q target: the engine target is the one built; a deployed stack's needs enrolled test networks' identities, which this harness does not hold", *targetName)
	}
	clusters, err := openClusters(ctx, common.clustersPath)
	if err != nil {
		return false, err
	}
	defer clusters.Close()
	if common.clusterId == "" {
		common.clusterId = clusters.MetadataClusterIds()[0]
	}
	if kind := clusters.ClusterKind(common.clusterId); kind != server.FileClusterKindMetadata {
		return false, refuseStart("%s is a %q cluster: the mix is measured on a metadata cluster", common.clusterId, kind)
	}
	measurement, err := common.header(ctx, ModeMix, clusters, common.clusterId, map[string]string{
		"target":     *targetName,
		"preload":    settings.Preload.String(),
		"warmup":     settings.Warmup.String(),
		"steady":     settings.Steady.String(),
		"year_one":   fmt.Sprint(settings.YearOnePushesPerSecond),
		"refill":     fmt.Sprint(settings.RefillDwpd),
		"phases":     *phases,
		"processes":  fmt.Sprint(engineSettings.ProcessCount),
		"networks":   fmt.Sprint(engineSettings.NetworkCount),
		"writers":    fmt.Sprint(engineSettings.WritersPerNetwork),
		"entries":    fmt.Sprint(engineSettings.EntryWritesPerPush),
		"entry_size": fmt.Sprint(engineSettings.EntryByteCount),
	})
	if err != nil {
		return false, err
	}
	bench, err := common.target(ctx, clusters, common.clusterId)
	if err != nil {
		return false, err
	}
	bench.Store = clusters
	var clockCheck sidecar.ClockCheck
	if *chronyAddress != "" {
		chrony := server.DefaultChronyClockCheckSettings()
		chrony.ChronyAddress = *chronyAddress
		clockCheck = server.NewChronyClockCheck(ctx, chrony)
	} else {
		measurement.Notes = append(measurement.Notes, "no -chrony_address: this host's clock was taken as sure")
	}
	target, err := newEngineMixTarget(ctx, clusters, common.clusterId, bench.Counters, clockCheck, settings.RefillDwpd, settings.LatencySampleCount, &engineSettings)
	if err != nil {
		return false, refuseStart("the engine target: %v", err)
	}
	defer target.Close()
	runErr := RunMix(ctx, bench, target, target, &settings, measurement)
	if err := writeMeasurement(common.outPath, stdout, measurement); err != nil {
		return false, err
	}
	if runErr != nil {
		return false, runErr
	}
	return true, nil
}

func runReportMode(ctx context.Context, args []string, stdout io.Writer) (returnPass bool, returnErr error) {
	flags := flag.NewFlagSet("filecluster-bench report", flag.ContinueOnError)
	inPaths := stringsFlag{}
	flags.Var(&inPaths, "in", "a measurement file, repeatable: each block size's ops and mix runs")
	outPath := flags.String("out", "", "where the text report goes; stdout when empty")
	jsonPath := flags.String("json", "", "where the report as JSON goes, if anywhere")
	priceTablePath := flags.String("price_table", "", "where the chosen block size's price table file goes, if anywhere; written only when the gate passed")
	if err := flags.Parse(args); err != nil {
		return false, err
	}
	if len(inPaths) == 0 {
		return false, refuseStart("no -in measurement file")
	}
	measurements := []*Measurement{}
	for _, path := range inPaths {
		measurement, err := ReadMeasurement(path)
		if err != nil {
			return false, refuseStart("%v", err)
		}
		measurements = append(measurements, measurement)
	}
	report, err := BuildNvmeReport(measurements, nil, time.Now())
	if err != nil {
		return false, refuseStart("%v", err)
	}
	out := stdout
	if *outPath != "" {
		file, err := os.Create(*outPath)
		if err != nil {
			return false, err
		}
		defer func() {
			if err := file.Close(); err != nil && returnErr == nil {
				returnErr = err
			}
		}()
		out = file
	}
	if err := WriteNvmeReport(out, report); err != nil {
		return false, err
	}
	if *jsonPath != "" {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return false, err
		}
		if err := os.WriteFile(*jsonPath, append(data, '\n'), 0o644); err != nil {
			return false, err
		}
	}
	if *priceTablePath != "" && report.Pass {
		file, err := os.Create(*priceTablePath)
		if err != nil {
			return false, err
		}
		if err := WritePriceTableFile(file, report); err != nil {
			file.Close()
			return false, err
		}
		if err := file.Close(); err != nil {
			return false, err
		}
	}
	return report.Pass, nil
}

// A repeatable string flag.
type stringsFlag []string

func (self *stringsFlag) String() string         { return strings.Join(*self, ",") }
func (self *stringsFlag) Set(value string) error { *self = append(*self, value); return nil }

// Runs a command in a directory: its stdout, and an error carrying its stderr when it fails.
type commandRunner func(ctx context.Context, dir string, name string, args ...string) ([]byte, error)

func execCommand(ctx context.Context, dir string, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	stderr := &bytes.Buffer{}
	command.Stderr = stderr
	out, err := command.Output()
	if err != nil {
		return out, fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// FS-116's tests in the sidecar module, which the fs116 mode lists and runs.
const (
	fs116Package = "./sidecartest/fault/"
	fs116Pattern = "^TestFs116"
)

// The fs116 mode's bounds.
type Fs116Settings struct {
	// The longest line of go test's JSON it reads: 16 MiB, past any test's output line.
	MaxLineByteCount int
}

func DefaultFs116Settings() *Fs116Settings {
	return &Fs116Settings{MaxLineByteCount: 16 * 1024 * 1024}
}

// Runs FS-116 on the monoroot's sidecar commit and records what passed (D-TEST-11: the nvme run starts only after
// it). A tree with changes is refused, so the record names a commit.
func runFs116Mode(ctx context.Context, args []string, stdout io.Writer) (bool, error) {
	flags := flag.NewFlagSet("filecluster-bench fs116", flag.ContinueOnError)
	monoroot := flags.String("monoroot", "", "the monoroot whose sidecar and server commits to run FS-116 on")
	goCommand := flags.String("go", "go", "the go command")
	outPath := flags.String("out", "", "where the record goes; stdout when empty")
	if err := flags.Parse(args); err != nil {
		return false, err
	}
	if *monoroot == "" {
		return false, refuseStart("-monoroot names no monoroot")
	}
	evidence, err := RunFs116(ctx, *monoroot, *goCommand, execCommand, time.Now)
	if evidence != nil {
		data, marshalErr := json.MarshalIndent(evidence, "", "  ")
		if marshalErr != nil {
			return false, marshalErr
		}
		data = append(data, '\n')
		if *outPath == "" {
			stdout.Write(data)
		} else if writeErr := os.WriteFile(*outPath, data, 0o644); writeErr != nil {
			return false, writeErr
		}
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Lists and runs FS-116 in monoroot's sidecar checkout through runCommand: the record, and an error unless every
// listed test passed on trees with no changes.
func RunFs116(ctx context.Context, monoroot string, goCommand string, runCommand commandRunner, now func() time.Time) (*Fs116Evidence, error) {
	commitSet, err := load.ReadCommitSet(monoroot, benchRepos...)
	if err != nil {
		return nil, refuseStart("%v", err)
	}
	for repo, commit := range commitSet {
		if strings.HasSuffix(commit, "+dirty") {
			return nil, refuseStart("%s's tree holds changes: FS-116's record must name a commit", repo)
		}
	}
	sidecarDir := filepath.Join(monoroot, "sidecar")
	listed, err := runCommand(ctx, sidecarDir, goCommand, "test", "-list", fs116Pattern, fs116Package)
	if err != nil {
		return nil, err
	}
	evidence := &Fs116Evidence{
		CommitSet: commitSet,
		Listed:    []string{},
		Passed:    []string{},
		Command:   strings.Join([]string{goCommand, "test", "-count=1", "-json", "-run", fs116Pattern, fs116Package}, " "),
	}
	scanner := bufio.NewScanner(bytes.NewReader(listed))
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); strings.HasPrefix(line, "TestFs116") {
			evidence.Listed = append(evidence.Listed, line)
		}
	}
	if len(evidence.Listed) == 0 {
		return nil, errors.New("go test listed no FS-116 test")
	}
	out, runErr := runCommand(ctx, sidecarDir, goCommand, "test", "-count=1", "-json", "-run", fs116Pattern, fs116Package)
	evidence.Time = now()
	type testEvent struct {
		Action string
		Test   string
	}
	failed := []string{}
	scanner = bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(nil, DefaultFs116Settings().MaxLineByteCount)
	for scanner.Scan() {
		event := &testEvent{}
		if json.Unmarshal(scanner.Bytes(), event) != nil || event.Test == "" || strings.Contains(event.Test, "/") {
			continue
		}
		switch event.Action {
		case "pass":
			evidence.Passed = append(evidence.Passed, event.Test)
		case "fail":
			failed = append(failed, event.Test)
		}
	}
	if 0 < len(failed) {
		return evidence, fmt.Errorf("FS-116 failed: %s", strings.Join(failed, ", "))
	}
	if runErr != nil {
		return evidence, runErr
	}
	if err := evidence.Check(commitSet); err != nil {
		return evidence, err
	}
	return evidence, nil
}
