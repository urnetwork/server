// Agent and managed-SSH bounds are plain values shared by every platform. The
// linux agent enforces them; a coordinator on any host, including an
// operator's Mac running bringyourctl, sizes its joins to the remote agent's
// cleanup from the same defaults.
package competitionagent

import (
	"time"
)

// All scheduling and diagnostic bounds belong to an agent instance. Nonpositive
// values are invalid; Now is a test seam and is never accepted from a peer.
type AgentSettings struct {
	ControlTimeout time.Duration
	// Native runtime admission and owned-service readiness need their own
	// budget. Nonpositive refuses construction; never extends the job deadline.
	LaunchTimeout    time.Duration
	SelfCheckTimeout time.Duration
	CancelGrace      time.Duration
	LockPollInterval time.Duration
	MaximumClockSkew time.Duration
	DiagnosticBytes  int64
	ConfigBytes      int64
	DirectoryBatch   int
	// Each file-body direction receives its own two-minute native I/O budget,
	// distinct from framing. Nonpositive durations refuse the agent owner.
	InputTransferTimeout      time.Duration
	OutputTransferTimeout     time.Duration
	CleanupTimeout            time.Duration
	CleanupCommandTimeout     time.Duration
	CleanupPollInterval       time.Duration
	CleanupObjectCount        int
	OwnerPollInterval         time.Duration
	ManagementMemoryBytes     int64
	ManagementCpuQuotaPercent int
	// Bytes/tasks left available in the independent cleanup aggregate before
	// admitting ordinary work. Nonpositive values refuse arming; these are
	// source class margins, not measured native recovery peak guarantees.
	RecoveryMemoryHeadroomBytes int64
	RecoveryTasksHeadroom       int64
	// Ordinary admission inventories every descendant within the fixed parent.
	// Nonpositive depth or runnable margins refuse admission. These source-class
	// margins require separate native pressure qualification, not arithmetic alone.
	ManagementGroupDepth          int
	ManagementMemoryHeadroomBytes int64
	ManagementTasksHeadroom       int64
	// Build tasks include the retained BuildKit daemon and all of its children;
	// 4096 is an independent parent ceiling. Nonpositive values refuse launch.
	BuildTasksMaximum int64
	// The 100000-microsecond kernel quota period bounds each exact CPU role.
	// Values outside the cgroup v2 period range refuse launch.
	ContainmentCpuPeriodMicros int64
	Now                        func() time.Time
	// Set only from the authenticated original-role config during native proof.
	existingRecoveryProfile string
}

// Control calls are short; only the detached systemd service owns evaluation.
func DefaultAgentSettings() AgentSettings {
	return AgentSettings{ControlTimeout: 30 * time.Second, LaunchTimeout: 2 * time.Minute, SelfCheckTimeout: 2 * time.Minute,
		CancelGrace: 10 * time.Second, LockPollInterval: 25 * time.Millisecond,
		MaximumClockSkew: time.Minute, DiagnosticBytes: 4 * 1024 * 1024,
		ConfigBytes: 64 * 1024, DirectoryBatch: 128, InputTransferTimeout: 2 * time.Minute, OutputTransferTimeout: 2 * time.Minute, CleanupTimeout: 2 * time.Minute,
		CleanupCommandTimeout: 10 * time.Second, CleanupPollInterval: 25 * time.Millisecond, CleanupObjectCount: 1024,
		OwnerPollInterval: 25 * time.Millisecond, ManagementMemoryBytes: 8 * 1024 * 1024 * 1024, ManagementCpuQuotaPercent: 200,
		RecoveryMemoryHeadroomBytes: 256 * 1024 * 1024, RecoveryTasksHeadroom: 128,
		ManagementGroupDepth: 32, ManagementMemoryHeadroomBytes: 256 * 1024 * 1024, ManagementTasksHeadroom: 128,
		BuildTasksMaximum: 4096, ContainmentCpuPeriodMicros: 100000, Now: time.Now}
}

// All session budgets are owned by one wrapper. Child caps are inside the
// existing private aggregate, never another allowance outside the reserve.
type ManagementSshSettings struct {
	// Selected only by the separate fixed entrypoint, never request/config data.
	existingOnly    bool
	Agent           AgentSettings
	MemoryBytes     int64         // Per-control child maximum; default 256 MiB requires separate native peak proof.
	TasksMaximum    int64         // Per-control tasks; default 128 includes bounded native control helpers.
	MaximumControls int           // Retained unknown sessions; a full registry refuses new admission.
	MaximumRuntime  time.Duration // Full healthy RPC allowance, not a shorter framing or evaluation clock.
	JoinTimeout     time.Duration // Finite stop/manager/client join owner after transport withdrawal.
	PollInterval    time.Duration // Positive rate limit while monitoring the exact ephemeral unit.
}

// Framing/transfer/cleanup each retain their full allowance in addition to the
// largest signed job lifetime. These defaults do not assert native readiness.
func DefaultManagementSshSettings() ManagementSshSettings {
	agent := DefaultAgentSettings()
	maximum := DefaultLimits().MaximumJobDuration + agent.ControlTimeout + agent.SelfCheckTimeout + agent.InputTransferTimeout + agent.OutputTransferTimeout + agent.CleanupTimeout + agent.CancelGrace
	return ManagementSshSettings{Agent: agent, MemoryBytes: 256 * 1024 * 1024, TasksMaximum: 128, MaximumControls: 128,
		MaximumRuntime: maximum, JoinTimeout: agent.ControlTimeout + agent.ControlTimeout + agent.CancelGrace, PollInterval: agent.OwnerPollInterval}
}
