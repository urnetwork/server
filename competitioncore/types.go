// A prospective core dispatcher classifies only authenticated process and
// registry facts. Production remains drop-only until every native gate exists.
package competitioncore

import (
	"context"
	"io"
	"time"
)

// Fixed kernel/Apport contracts are not deployment options or caller input.
const (
	Schema            = 1
	DispatcherPath    = "/usr/lib/urnetwork/core"
	ConfigurationPath = "/etc/urnetwork/core-dispatcher.json"
	RegistryDirectory = "/var/lib/urnetwork/core"
	OriginalPattern   = "|/usr/share/apport/apport -p%p -s%s -c%c -d%d -P%P -u%u -g%g -F%F -- %E"
	DispatcherPattern = "|/usr/lib/urnetwork/core -p%p -s%s -c%c -d%d -P%P -u%u -g%g -F%F -- %E"
	ApportPath        = "/usr/share/apport/apport"
	HandlerSliceName  = "urnetwork-core-handler.slice"
	HandlerCgroup     = "/urnetwork.slice/urnetwork-core.slice/urnetwork-core-handler.slice"
	ExpectedPipeLimit = 10
)

// All byte/count/time bounds belong to the dispatcher instance. Nonpositive
// settings refuse work rather than selecting a permissive fallback.
type Settings struct {
	DocumentBytes       int64
	ProcBytes           int64
	ArgumentBytes       int
	DocumentDepth       int
	DocumentTokens      int
	MaximumRuns         int
	MaximumCgroupDepth  int
	MaximumCgroupBytes  int
	MaximumCpuIndex     int
	MaximumDescriptor   uint64
	HandlerTimeout      time.Duration
	MaximumJobLifetime  time.Duration
	HandlerMemoryBytes  uint64
	HandlerTasksMaximum uint64
	HandlerCpuQuota     uint64
	HandlerCpuPeriod    uint64
}

// Ten kernel-admitted helpers still have pre-main Go/runtime overhead. These
// limits do not qualify that overhead or kernel/journal pressure.
func DefaultSettings() Settings {
	return Settings{DocumentBytes: 128 * 1024, ProcBytes: 128 * 1024, ArgumentBytes: 4096,
		DocumentDepth: 32, DocumentTokens: 32768, MaximumRuns: 16, MaximumCgroupDepth: 64, MaximumCgroupBytes: 4096,
		MaximumCpuIndex: 4095, MaximumDescriptor: 1048576, HandlerTimeout: 2 * time.Second, MaximumJobLifetime: 3 * time.Hour,
		HandlerMemoryBytes: 256 * 1024 * 1024, HandlerTasksMaximum: 128, HandlerCpuQuota: 200000, HandlerCpuPeriod: 100000}
}

// Metadata pins are data only; no native execution backend is supplied here.
type Pin struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
}

// The fixed root document has no path/program/argument override for the kernel
// invocation. Interpreter metadata cannot enable the absent handoff backend.
type Policy struct {
	Schema            int    `json:"schema"`
	ExpectedPipeLimit uint64 `json:"expected_pipe_limit"`
	ManagementCpuSet  string `json:"management_cpu_set"`
	Apport            Pin    `json:"apport"`
	Interpreter       Pin    `json:"interpreter"`
}

// The exact kernel-provided argument vector survives classification unchanged.
// Descriptor may be absent on kernels which do not expand the newer %F token.
type Invocation struct {
	NamespacePid uint64
	Signal       uint64
	CoreLimit    uint64
	DumpMode     uint64
	HostPid      uint64
	Uid          uint64
	Gid          uint64
	Descriptor   string
	Executable   string
	arguments    []string
}

// One root-precommitted run covers exactly 31 independently derived parents.
// Deadlines never expire this custody: removal requires a positive joined proof.
type Run struct {
	RunId      string    `json:"run_id"`
	JobId      string    `json:"job_id"`
	Attempt    int       `json:"attempt"`
	BuildKey   string    `json:"build_key"`
	PlanSha256 string    `json:"plan_sha256"`
	StartedAt  time.Time `json:"started_at"`
	DeadlineAt time.Time `json:"deadline_at"`
	Cgroups    []string  `json:"cgroups"`
}

// A generation change invalidates classification, even for another run. The
// private engine inventory remains present when there are no candidate runs.
type Registry struct {
	Schema        int      `json:"schema"`
	Generation    uint64   `json:"generation"`
	BootId        string   `json:"boot_id"`
	HandlerCgroup string   `json:"handler_cgroup"`
	EngineCgroups []string `json:"engine_cgroups"`
	Runs          []Run    `json:"runs"`
}

// Snapshots bind exact durable bytes, not a later mutable registry pointer.
type Snapshot struct {
	Registry Registry
	digest   string
}

// Facts are captured through a held proc directory and checked against pidfd.
type ProcessFacts struct {
	HostPid      uint64
	NamespacePid uint64
	Birth        uint64
	Uid          uint64
	Gid          uint64
	Cgroup       string
}

// Result names distinguish positively scoped drops from an uncertain drop.
type Decision string

const (
	UnknownDropped   Decision = "unknown-dropped"
	CandidateDropped Decision = "candidate-dropped"
	OrdinaryDropped  Decision = "ordinary-handoff-unimplemented"
	OrdinaryHanded   Decision = "ordinary-handed-off"
)

// Private seams can be replaced only inside package tests. No public Boolean
// or command option bypasses the production manager/restoration/exec gaps.
type host interface {
	Policy(context.Context) (Policy, error)
	PipeLimit(context.Context) (uint64, error)
	Pattern(context.Context) (string, error)
	BootId(context.Context) (string, error)
	Snapshot(context.Context) (Snapshot, error)
	Recheck(context.Context, Snapshot) error
	Place(context.Context, Policy) (placement, error)
	Process(context.Context, Invocation) (process, error)
	Handoff(context.Context, Policy, Invocation, io.ReadCloser) error
}

// A placement owns only the helper's own PID. Restore is mandatory before any
// positive ordinary handoff and is currently unimplemented in production.
type placement interface {
	Recheck(context.Context) error
	Restore(context.Context) error
	Close() error
}

// Repeated checks must return the same PID generation, credentials and cgroup.
type process interface {
	Facts(context.Context) (ProcessFacts, error)
	Close() error
}
