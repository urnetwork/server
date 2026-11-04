// Per-run Docker request custody is independent of the evaluator's process
// tree. Mutations and build sessions retain one durable owner until a complete
// daemon response, or remain explicitly unknown without replay after restart.
package competitionagent

import (
	"errors"
	"time"
)

// An uncertain issued mutation cannot be erased by an empty resource census.
var ErrDockerProxyUnknown = errors.New("Docker request outcome remains unknown")

// All transport, disk, memory and lifecycle limits belong to this one owner.
// Nonpositive values are invalid; increasing them never changes the signed
// submission deadline or the frozen build-resource policy.
type DockerProxySettings struct {
	MaximumOperations       int
	MaximumConnections      int
	MaximumHeaderBytes      int
	MaximumDocumentBytes    int64
	MaximumBuildOutputBytes int64
	MaximumRequestBytes     int64
	MaximumImageLoadBytes   int64
	MaximumResponseBytes    int64
	MaximumSpoolBytes       int64
	MaximumContextBytes     int64
	MaximumContextFileBytes int64
	MaximumContextEntries   int
	MaximumContextDepth     int
	MaximumDocumentDepth    int
	DirectoryBatch          int
	CopyBufferBytes         int
	HeaderTimeout           time.Duration
	UploadTimeout           time.Duration
	ClientWriteTimeout      time.Duration
	OperationTimeout        time.Duration
	BuildTimeout            time.Duration
	BuildStopGrace          time.Duration
	QuiesceTimeout          time.Duration
	DrainAllowance          time.Duration
	Now                     func() time.Time
}

// Contexts contain the trusted validator and immutable input documents, not
// source trees or caches. Response streaming remains bounded for image exports.
func DefaultDockerProxySettings() DockerProxySettings {
	return DockerProxySettings{MaximumOperations: 4096, MaximumConnections: 64, MaximumHeaderBytes: 64 * 1024,
		MaximumDocumentBytes: 2 * 1024 * 1024, MaximumBuildOutputBytes: 4 * 1024 * 1024, MaximumRequestBytes: 64 * 1024 * 1024, MaximumImageLoadBytes: 16 * 1024 * 1024 * 1024,
		MaximumResponseBytes: 16 * 1024 * 1024 * 1024, MaximumSpoolBytes: 32 * 1024 * 1024 * 1024,
		MaximumContextBytes: 1024 * 1024 * 1024, MaximumContextFileBytes: 512 * 1024 * 1024,
		MaximumContextEntries: 16384, MaximumContextDepth: 64, MaximumDocumentDepth: 64, DirectoryBatch: 128, CopyBufferBytes: 64 * 1024,
		HeaderTimeout: 15 * time.Second, UploadTimeout: 2 * time.Minute, ClientWriteTimeout: 15 * time.Second,
		OperationTimeout: 3 * time.Hour, BuildTimeout: 10 * time.Minute, BuildStopGrace: 30 * time.Second,
		QuiesceTimeout: 2 * time.Minute, DrainAllowance: 2 * time.Minute, Now: time.Now}
}

// This root-persisted plan names only the installed local daemon and pinned
// tools. The worker and owned-build sockets are separate authority boundaries.
// ContextRoot is exactly the run's private work directory; callers cannot use
// another checkout, Dockerfile, builder, context, daemon or resource setting.
type DockerProxyPlan struct {
	Schema               int       `json:"schema"`
	RunId                string    `json:"run_id"`
	JobId                string    `json:"job_id"`
	RoundId              string    `json:"round_id"`
	Attempt              int       `json:"attempt"`
	DeadlineAt           time.Time `json:"deadline_at"`
	BaseDirectory        string    `json:"base_directory"`
	TrustedOperatorUid   uint32    `json:"trusted_operator_uid"`
	StateDirectory       string    `json:"state_directory"`
	ScratchDirectory     string    `json:"scratch_directory"`
	SocketPath           string    `json:"socket_path"`
	BuildSocketPath      string    `json:"build_socket_path"`
	ContextRoot          string    `json:"context_root"`
	UpstreamSocketPath   string    `json:"upstream_socket_path"`
	Docker               Pin       `json:"docker"`
	Buildx               Pin       `json:"buildx"`
	Dockerfile           Pin       `json:"dockerfile"`
	BaseImageDigest      string    `json:"base_image_digest"`
	CgroupNames          []string  `json:"cgroup_names"`
	BuildCgroupName      string    `json:"build_cgroup_name"`
	CpuSet               string    `json:"cpu_set"`
	BuildMemory          string    `json:"build_memory"`
	WorkerUid            uint32    `json:"worker_uid"`
	ReadOnlyBindPaths    []string  `json:"read_only_bind_paths"`
	ResourcePolicySha256 string    `json:"resource_policy_sha256"`
}

// Arguments are only the existing fixed build family, not a command launcher.
// ContextDirectory must be below ContextRoot and is snapshotted before issue.
type DockerBuildRequest struct {
	RunId            string   `json:"run_id"`
	Arguments        []string `json:"arguments"`
	ContextDirectory string   `json:"context_directory"`
}

// A nonzero exit can still be definitively acknowledged when the complete
// BuildKit Solve response carries an ordinary compiler/validation failure.
type DockerBuildResult struct {
	RunId        string `json:"run_id"`
	Acknowledged bool   `json:"acknowledged"`
	ExitCode     int    `json:"exit_code"`
	GrpcStatus   string `json:"grpc_status,omitempty"`
	ErrorCode    string `json:"error_code,omitempty"`
	Stdout       []byte `json:"stdout,omitempty"`
	Stderr       []byte `json:"stderr,omitempty"`
}

// Status contains bounded custody facts, never request bodies or credentials.
type DockerProxySnapshot struct {
	RunId             string `json:"run_id"`
	Quiesced          bool   `json:"quiesced"`
	AdmissionsClosed  bool   `json:"admissions_closed"`
	Inflight          int    `json:"inflight"`
	Operations        int    `json:"operations"`
	UnknownOperations int    `json:"unknown_operations"`
	BuildStarted      bool   `json:"build_started"`
	BuildAcknowledged bool   `json:"build_acknowledged"`
}
