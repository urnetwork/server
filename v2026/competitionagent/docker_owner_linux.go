// Separate systemd ownership keeps Docker requests alive after evaluator exit.
// Cleanup freezes every precommitted parent before joining that request owner,
// then reaps resources and finally retires the frozen parents.
package competitionagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Test replacements preserve the composed agent's durable state machine while
// keeping unit tests away from real host cgroups, systemd and Docker.
type runContainment interface {
	Prepare(context.Context, cleanupPlan) error
	Start(context.Context, cleanupPlan) error
	Quiesce(context.Context, cleanupPlan, bool) error
	VerifyRetained(context.Context, cleanupPlan) error
	Release(context.Context, cleanupPlan) error
	EmergencyFreeze(context.Context, cleanupPlan) error
}

// All operations name only one authenticated immutable run plan. The runner
// belongs to the management owner, not the evaluator's service process group.
type hostRunContainment struct {
	config        Config
	settings      AgentSettings
	proxySettings DockerProxySettings
	runner        Runner
	slices        *hostRunSlices
	control       func(context.Context, DockerProxyPlan, bool) (DockerProxySnapshot, error)
	newProxy      func(DockerProxyPlan, DockerProxySettings) (*DockerProxy, error)
	credential    *syscall.Credential
}

func newHostRunContainment(config Config, settings AgentSettings, credential *syscall.Credential) *hostRunContainment {
	self := &hostRunContainment{config: config, settings: settings, proxySettings: DefaultDockerProxySettings(),
		runner: NewSystemdRunner(settings), slices: newHostRunSlices(config, settings), newProxy: NewDockerProxy, credential: credential}
	self.control = self.requestControl
	return self
}

// No daemon mutation is possible until every complete full-R9 parent exists
// and the kernel positively reports it empty and unfrozen.
func (self *hostRunContainment) Prepare(ctx context.Context, plan cleanupPlan) error {
	if plan.DockerOwner == nil {
		return errors.New("run has no authenticated Docker custody plan")
	}
	directory := filepath.Dir(plan.DockerOwner.SocketPath)
	if err := os.Mkdir(directory, 0710); err != nil {
		return errors.New("Docker socket identity already exists or is unavailable before preparation")
	}
	if self.credential != nil {
		if err := os.Chown(directory, 0, int(self.credential.Gid)); err != nil {
			return err
		}
	}
	if err := os.Chmod(directory, 0710); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(directory)); err != nil {
		return err
	}
	return self.slices.Prepare(ctx, plan)
}

// The owner is ready before the evaluator unit is admitted. A lost launch or
// ready acknowledgment cannot authorize another owner or another evaluator.
func (self *hostRunContainment) Start(ctx context.Context, plan cleanupPlan) error {
	if plan.DockerOwner == nil || !dockerProxyCpuSetPattern.MatchString(plan.ManagementCpuSet) {
		return errors.New("run has no finite Docker management identity")
	}
	err := self.runner.Start(ctx, Launch{UnitName: dockerOwnerUnitName(plan.RunId), Role: "docker-owner", RunId: plan.RunId,
		ConfigPath: self.config.configPath, ConfigSha256: self.config.configSha256, BaseDirectory: self.config.BaseDirectory, Agent: self.config.Agent,
		DeadlineAt: plan.DockerOwner.DeadlineAt.Add(self.proxySettings.DrainAllowance), ManagementCpuSet: plan.ManagementCpuSet})
	if err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		snapshot, err := self.control(ctx, *plan.DockerOwner, false)
		if err == nil {
			if snapshot.RunId != plan.RunId || snapshot.AdmissionsClosed || snapshot.Quiesced || snapshot.UnknownOperations != 0 {
				return errors.New("Docker owner refused its one execution lifetime")
			}
			return nil
		}
		unit, statusErr := self.runner.Status(ctx, dockerOwnerUnitName(plan.RunId))
		if statusErr != nil || !unit.Exists || !unit.Active {
			return errors.New("Docker owner did not reach its ready boundary")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(self.settings.OwnerPollInterval):
		}
	}
}

// Kill admission is independent of a blocked daemon or owner control socket.
// The positive persisted reap marker permits retrying an interrupted slice
// retirement; otherwise every parent must remain present and frozen.
func (self *hostRunContainment) Quiesce(ctx context.Context, plan cleanupPlan, reaped bool) error {
	if plan.DockerOwner == nil {
		return errors.New("cleanup has no authenticated Docker request owner")
	}
	if !reaped {
		if err := self.slices.FreezeAndReap(ctx, plan); err != nil {
			return err
		}
	}
	// The RPC may lose its reply after committing the barrier. Stop/join the
	// exact owner and recover its journal independently before accepting it.
	_, _ = self.control(ctx, *plan.DockerOwner, true)
	unit, err := self.runner.Status(ctx, dockerOwnerUnitName(plan.RunId))
	if err != nil {
		return err
	}
	if unit.Exists && unit.Active {
		if err := self.runner.Stop(ctx, dockerOwnerUnitName(plan.RunId)); err != nil {
			return err
		}
	}
	unit, err = self.runner.Status(ctx, dockerOwnerUnitName(plan.RunId))
	if err != nil || unit.Active {
		return errors.New("Docker request owner has not positively joined")
	}
	proxy, err := self.newProxy(*plan.DockerOwner, self.proxySettings)
	if err != nil {
		return err
	}
	defer proxy.Close()
	if err := proxy.Quiesce(ctx); err != nil {
		return err
	}
	if snapshot := proxy.Snapshot(); snapshot.RunId != plan.RunId || !snapshot.Quiesced || snapshot.Inflight != 0 || snapshot.UnknownOperations != 0 {
		return errors.New("Docker journal cannot prove complete mutation quiescence")
	}
	return nil
}

func (self *hostRunContainment) VerifyRetained(ctx context.Context, plan cleanupPlan) error {
	return self.slices.VerifyRetained(ctx, plan)
}

// Only a durable zero-unknown owner barrier and completed exact resource reap
// can precede this call. Retrying stop never thaws or reuses an old parent.
func (self *hostRunContainment) Release(ctx context.Context, plan cleanupPlan) error {
	return self.slices.Release(ctx, plan)
}

// Owner ExecStopPost never asks the evaluator to stop, joins its unit, takes
// cleanup's lock or waits for its own unit. This avoids mutual post-stop joins.
func (self *hostRunContainment) EmergencyFreeze(ctx context.Context, plan cleanupPlan) error {
	return self.slices.FreezeAndReap(ctx, plan)
}

// Only root peers can invoke these fixed local management operations. A lost
// response is uncertainty, not a substitute for reading the durable journal.
func (self *hostRunContainment) requestControl(ctx context.Context, plan DockerProxyPlan, quiesce bool) (DockerProxySnapshot, error) {
	transport := &http.Transport{Proxy: nil, DisableCompression: true, MaxConnsPerHost: 1,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", plan.SocketPath)
		}}
	defer transport.CloseIdleConnections()
	method, target := http.MethodGet, "http://owner/_urnetwork/status"
	if quiesce {
		method, target = http.MethodPost, "http://owner/_urnetwork/quiesce?run_id="+plan.RunId
	}
	request, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return DockerProxySnapshot{}, err
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		return DockerProxySnapshot{}, err
	}
	defer response.Body.Close()
	if quiesce {
		if response.StatusCode != http.StatusNoContent {
			return DockerProxySnapshot{}, ErrDockerProxyUnknown
		}
		return self.requestControl(ctx, plan, false)
	}
	if response.StatusCode != http.StatusOK {
		return DockerProxySnapshot{}, errors.New("Docker owner status was not acknowledged")
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, self.proxySettings.MaximumDocumentBytes+1))
	if err != nil || int64(len(content)) > self.proxySettings.MaximumDocumentBytes || validateDocumentKeys(content) != nil {
		return DockerProxySnapshot{}, errors.New("Docker owner status exceeds its fixed document shape")
	}
	snapshot := DockerProxySnapshot{}
	if decodeStrict(content, &snapshot) != nil || snapshot.RunId != plan.RunId || snapshot.Inflight < 0 || snapshot.Inflight > self.proxySettings.MaximumConnections ||
		snapshot.Operations < 0 || snapshot.Operations > self.proxySettings.MaximumOperations || snapshot.UnknownOperations < 0 || snapshot.UnknownOperations > snapshot.Operations ||
		snapshot.Quiesced && (!snapshot.AdmissionsClosed || snapshot.Inflight != 0 || snapshot.UnknownOperations != 0) {
		return DockerProxySnapshot{}, errors.New("Docker owner status has invalid custody facts")
	}
	return snapshot, nil
}

// Only the pinned resource helper supplies CPU/memory identity. A peer cannot
// replace the qualified resource split with Docker command-line arguments.
type runResourceIdentity struct {
	Schema                    int    `json:"schema"`
	Kind                      string `json:"kind"`
	EvaluationCpuSet          string `json:"evaluation_cpuset"`
	ManagementCpuSet          string `json:"management_cpuset"`
	BuildMemoryLimit          string `json:"build_memory_limit"`
	BuildMemoryLimitBytes     int64  `json:"build_memory_limit_bytes"`
	BuildPidsLimit            int64  `json:"build_pids_limit"`
	BuildWorkLimit            string `json:"build_work_limit"`
	BuildWorkLimitBytes       int64  `json:"build_work_limit_bytes"`
	RunnerMemoryLimitBytes    int64  `json:"runner_memory_limit_bytes"`
	PostgresMemoryLimitBytes  int64  `json:"postgres_memory_limit_bytes"`
	RedisMemoryLimitBytes     int64  `json:"redis_memory_limit_bytes"`
	RunnerPidsLimit           int64  `json:"runner_pids_limit"`
	PostgresPidsLimit         int64  `json:"postgres_pids_limit"`
	RedisPidsLimit            int64  `json:"redis_pids_limit"`
	ActiveMemoryLimitBytes    int64  `json:"active_memory_limit_bytes"`
	ActivePidsLimit           int64  `json:"active_pids_limit"`
	ScorerMemoryLimitBytes    int64  `json:"scorer_memory_limit_bytes"`
	ScorerPidsLimit           int64  `json:"scorer_pids_limit"`
	PreflightMemoryLimitBytes int64  `json:"preflight_memory_limit_bytes"`
	PreflightPidsLimit        int64  `json:"preflight_pids_limit"`
	DisjointCpuSets           bool   `json:"disjoint_cpu_sets"`
	MemoryCapacityPassed      bool   `json:"memory_capacity_passed"`
	EvidenceMemoryLimitBytes  int64  `json:"evidence_memory_limit_bytes"`
	HostMemoryBytes           int64  `json:"host_memory_bytes"`
	ManagementReserveBytes    int64  `json:"minimum_management_memory_reserve_bytes"`
}

// The helper has additional retained capacity fields. Duplicate/case aliases
// are still rejected before decoding its finite required custody facts.
func decodeRunResourceIdentity(content []byte) (runResourceIdentity, error) {
	identity := runResourceIdentity{}
	if validateDocumentKeys(content) != nil || json.Unmarshal(content, &identity) != nil {
		return runResourceIdentity{}, errors.New("pinned resource helper identity is malformed")
	}
	if err := validateRunResourceIdentity(identity); err != nil {
		return runResourceIdentity{}, err
	}
	return identity, nil
}

// The exact helper-derived role totals are finite and internally consistent.
// This same static check authenticates committed metadata without a live probe.
func validateRunResourceIdentity(identity runResourceIdentity) error {
	if identity.Schema != Schema || identity.Kind != "sim-latency-resource-boundary" ||
		!identity.DisjointCpuSets || !identity.MemoryCapacityPassed || !dockerProxyCpuSetPattern.MatchString(identity.EvaluationCpuSet) ||
		!dockerProxyCpuSetPattern.MatchString(identity.ManagementCpuSet) || !dockerProxyMemoryPattern.MatchString(identity.BuildMemoryLimit) || identity.EvidenceMemoryLimitBytes <= 0 {
		return errors.New("pinned resource helper did not prove the qualified management/evaluation split")
	}
	if err := validateRunResourceLimits(identity); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, set := range []string{identity.EvaluationCpuSet, identity.ManagementCpuSet} {
		for _, cpu := range strings.Split(set, ",") {
			parsed, err := strconv.ParseUint(cpu, 10, 32)
			if err != nil || strconv.FormatUint(parsed, 10) != cpu || seen[cpu] {
				return errors.New("resource helper CPU sets overlap or are noncanonical")
			}
			seen[cpu] = true
		}
	}
	return nil
}

// The cleanup plan is root-committed before launch. Its nested owner plan has
// no authority derived from workspace sidecars or a later caller environment.
func (self *Agent) makeDockerOwnerPlan(manifest StartManifest, plan cleanupPlan, resource runResourceIdentity, backingImages []string) (*DockerProxyPlan, error) {
	workerUid := uint32(os.Geteuid())
	workerGid := uint32(os.Getegid())
	operatorUid := workerUid
	if self.credential != nil {
		workerUid = self.credential.Uid
		workerGid = self.credential.Gid
	}
	if !self.protected && workerUid == 0 {
		workerUid = 65534
	}
	if !self.protected && workerGid == 0 {
		workerGid = 65534
	}
	idMap, err := self.config.DockerRole.namespaceMetadata(self.protected)
	if err != nil {
		return nil, err
	}
	if self.protected {
		var err error
		operatorUid, err = trustedOperatorUid(self.config.TrustedOperatorUser)
		if err != nil {
			return nil, err
		}
	}
	workerSocket, buildSocket, err := dockerRunSocketPaths(filepath.Join(self.config.BaseDirectory, "clients"), manifest.Identity.RunId)
	if err != nil {
		return nil, err
	}
	owner := &DockerProxyPlan{Schema: Schema, RunId: manifest.Identity.RunId, JobId: manifest.Identity.JobId, RoundId: manifest.Identity.RoundId,
		Attempt: manifest.Identity.Attempt, DeadlineAt: manifest.Identity.DeadlineAt, BaseDirectory: self.config.BaseDirectory, TrustedOperatorUid: operatorUid,
		StateDirectory: filepath.Join(self.stateDirectory(manifest.Identity.RunId), "docker-owner"), ScratchDirectory: filepath.Join(self.config.DockerRole.ScratchDirectory, manifest.Identity.RunId),
		SocketPath: workerSocket, BuildSocketPath: buildSocket,
		ContextRoot: self.workDirectory(manifest.Identity.RunId), UpstreamSocketPath: self.config.DockerSocketPath, Docker: self.config.Docker, Buildx: self.config.DockerRole.Buildx,
		Dockerfile: self.config.DockerRole.Dockerfile, BaseImageDigest: manifest.EvaluatorImageDigest, CgroupNames: append([]string(nil), plan.CgroupNames...),
		StockSeccompProfile: self.config.DockerRole.StockSeccompProfile, CandidateCoreProfile: self.config.DockerRole.CandidateCoreProfile,
		BackingImages: append([]string(nil), backingImages...),
		CpuSet:        resource.EvaluationCpuSet, ManagementCpuSet: resource.ManagementCpuSet, BuildMemory: resource.BuildMemoryLimit, WorkerUid: workerUid,
		ReadOnlyBindPaths: []string{self.config.ConfigLocalDirectory, self.config.VaultLocalDirectory, self.config.DockerRole.PostgresInitialization.Path}, ResourcePolicySha256: self.config.DockerRole.ResourcePolicy.Sha256,
		HostFiles: &HostFilesPlan{WorkerGid: workerGid, ContainerRootUid: idMap.ContainerRootUid, ContainerRootGid: idMap.ContainerRootGid,
			ResourceBoundary:     self.config.DockerRole.ResourceBoundary,
			ContainerIdMapLength: idMap.ContainerIdMapLength, EvidenceBytes: resource.EvidenceMemoryLimitBytes, WorkDeviceId: plan.WorkDeviceId}}
	for _, name := range plan.CgroupNames {
		if cleanupBuildSlicePattern.MatchString(name) {
			owner.BuildCgroupName = name
		}
	}
	if plan.SliceResources != nil {
		owner.BuildRetirement, err = newBuildRetirementPolicy(*plan.SliceResources, owner.BuildCgroupName, int64(os.Getpagesize()))
		if err != nil {
			return nil, err
		}
	}
	if err := validateHostFilesPlan(*owner, DefaultHostFilesSettings()); err != nil {
		return nil, err
	}
	if err := validateDockerProxyPlan(*owner, DefaultDockerProxySettings(), false); err != nil {
		return nil, err
	}
	return owner, nil
}

// Static reconstruction never invokes the resource helper during emergency
// cleanup. Its prelaunch measured split remains authenticated by the plan hash.
func (self *Agent) verifyDockerOwnerPlan(manifest StartManifest, plan cleanupPlan) error {
	if plan.DockerOwner == nil || !dockerProxyCpuSetPattern.MatchString(plan.ManagementCpuSet) {
		return errors.New("run cleanup omitted its independent Docker owner")
	}
	if plan.DockerOwner.HostFiles == nil || plan.WorkDeviceId == 0 {
		return errors.New("run cleanup omitted its independently admitted work device and file scope")
	}
	resource := runResourceIdentity{EvaluationCpuSet: plan.DockerOwner.CpuSet, ManagementCpuSet: plan.ManagementCpuSet, BuildMemoryLimit: plan.DockerOwner.BuildMemory,
		EvidenceMemoryLimitBytes: plan.DockerOwner.HostFiles.EvidenceBytes}
	owner := self
	if self.recoveryOnly {
		if plan.DockerOwner.WorkerUid == 0 || plan.DockerOwner.HostFiles.WorkerGid == 0 {
			return errors.New("authenticated cleanup plan omitted its original worker identity")
		}
		copy := *self
		copy.credential = &syscall.Credential{Uid: plan.DockerOwner.WorkerUid, Gid: plan.DockerOwner.HostFiles.WorkerGid}
		owner = &copy
	}
	expected, err := owner.makeDockerOwnerPlan(manifest, plan, resource, plan.DockerOwner.BackingImages)
	if err != nil || !reflect.DeepEqual(expected, plan.DockerOwner) {
		return errors.New("Docker custody plan differs from the root-pinned run role")
	}
	return nil
}

// A per-run completed reap proof is persisted before retiring any slice. It is
// not a host self-check or a permission to replay execution after a hard exit.
func (self *Agent) storeResourcesReaped(runId, planSha256 string) error {
	content, err := json.Marshal(struct {
		Schema     int    `json:"schema"`
		RunId      string `json:"run_id"`
		PlanSha256 string `json:"plan_sha256"`
	}{Schema: Schema, RunId: runId, PlanSha256: planSha256})
	if err != nil {
		return err
	}
	return atomicFile(self.stateDirectory(runId), "resources-reaped.json", content)
}

// Only exact root-committed current-run evidence permits retrying a partially
// completed slice retirement without demanding an already-removed parent.
func (self *Agent) loadResourcesReaped(runId, planSha256 string) (bool, error) {
	content, err := readBounded(filepath.Join(self.stateDirectory(runId), "resources-reaped.json"), self.settings.ConfigBytes)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var receipt struct {
		Schema     int    `json:"schema"`
		RunId      string `json:"run_id"`
		PlanSha256 string `json:"plan_sha256"`
	}
	if validateDocumentKeys(content) != nil || decodeStrict(content, &receipt) != nil || receipt.Schema != Schema || receipt.RunId != runId || receipt.PlanSha256 != planSha256 {
		return false, errors.New("completed resource reap proof differs from the current authenticated run")
	}
	return true, nil
}

// Owner post-stop can freeze/kill without cleanup's publication/per-run lock.
// A stalled daemon response or admission upload cannot delay this kill phase.
func (self *Agent) EmergencyFreeze(ctx context.Context, runId string) error {
	state, _, plan, err := self.authenticatedOwnedRun(runId)
	if err != nil {
		return err
	}
	reaped, err := self.loadResourcesReaped(runId, state.CleanupPlanSha256)
	if err != nil || reaped || state.StopCleanupVerified {
		return err
	}
	return self.containment.EmergencyFreeze(ctx, *plan)
}

// This root-only entrypoint is launched once in its own immutable systemd
// unit. It never executes candidate code, loads main credentials, or restarts
// an issued build after process loss. ExecStopPost owns emergency freezing.
func (self *Agent) RunDockerOwner(ctx context.Context, runId string) (operationErr error) {
	if self.recoveryOnly {
		return errors.New("recovery role cannot launch or resume a Docker owner")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	state, manifest, plan, err := self.authenticatedOwnedRun(runId)
	if err != nil {
		return err
	}
	stage := dockerOwnerFailureAuthority
	defer func() {
		if operationErr != nil {
			operationErr = errors.Join(operationErr, self.recordDockerOwnerFailure(runId, state, stage, operationErr))
		}
	}()
	reaped, err := self.loadResourcesReaped(runId, state.CleanupPlanSha256)
	if err != nil || reaped || state.RecoveryActive || state.CleanupVerified || state.StopCleanupVerified || state.CancelRequested || terminalState(state.State) || !self.settings.Now().Before(manifest.Identity.DeadlineAt) {
		return errors.New("Docker owner execution authority is closed")
	}
	stage = dockerOwnerFailureMarker
	marker, err := json.Marshal(struct {
		RunId      string `json:"run_id"`
		PlanSha256 string `json:"plan_sha256"`
	}{RunId: runId, PlanSha256: state.CleanupPlanSha256})
	if err != nil {
		return err
	}
	if err := exclusiveFile(filepath.Join(self.stateDirectory(runId), "docker-owner-started.json"), marker, 0400); err != nil {
		return errors.New("independent Docker owner was already attempted")
	}
	if err := syncDirectory(self.stateDirectory(runId)); err != nil {
		return err
	}
	directory := filepath.Dir(plan.DockerOwner.SocketPath)
	stage = dockerOwnerFailureSocket
	if self.protected {
		if err := protectedRuntimePath(directory, self.config.BaseDirectory, plan.DockerOwner.TrustedOperatorUid); err != nil {
			return err
		}
	}
	stage = dockerOwnerFailureProxy
	proxy, err := newDockerProxy(*plan.DockerOwner, DefaultDockerProxySettings(), self.protected)
	if err != nil {
		return err
	}
	defer proxy.Close()
	stage = dockerOwnerFailureRuntime
	if self.protected {
		boundary, err := self.runtimeAdmission(ctx)
		if err != nil {
			return err
		}
		if err := proxy.bindRuntimeBoundary(boundary); err != nil {
			_ = boundary.Close()
			return err
		}
	}
	if proxy.Snapshot().AdmissionsClosed {
		return errors.New("persisted Docker custody is already closed or uncertain")
	}
	listen := func(path string, worker bool) (*net.UnixListener, error) {
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			return nil, err
		}
		if err := os.Chmod(path, 0600); err != nil {
			listener.Close()
			return nil, err
		}
		if worker && self.credential != nil {
			if err := os.Chown(path, int(self.credential.Uid), int(self.credential.Gid)); err != nil {
				listener.Close()
				return nil, err
			}
		}
		return listener, nil
	}
	stage = dockerOwnerFailureWorker
	worker, err := listen(plan.DockerOwner.SocketPath, true)
	if err != nil {
		return err
	}
	defer worker.Close()
	stage = dockerOwnerFailureBuild
	build, err := listen(plan.DockerOwner.BuildSocketPath, false)
	if err != nil {
		return err
	}
	defer build.Close()
	stage = dockerOwnerFailureDurability
	if err := syncDirectory(directory); err != nil {
		return err
	}
	issueCtx, stopIssue := context.WithDeadline(ctx, manifest.Identity.DeadlineAt)
	defer stopIssue()
	stage = dockerOwnerFailureServe
	return proxy.serve(issueCtx, worker, build, func(closingCtx context.Context) error {
		return self.containment.EmergencyFreeze(closingCtx, *plan)
	})
}

// Immutable authority is safe to read without the global publication lock.
// Neither a stale stopped unit nor candidate files can choose another run.
func (self *Agent) authenticatedOwnedRun(runId string) (*runState, *StartManifest, *cleanupPlan, error) {
	if !digestPattern.MatchString(runId) {
		return nil, nil, nil, errors.New("invalid fixed Docker owner identity")
	}
	state, err := self.loadState(runId)
	if err != nil || !state.LaunchAttempted {
		return nil, nil, nil, errors.New("Docker owner has no committed launch authority")
	}
	manifest, err := self.loadManifest(runId, state.ManifestSha256)
	if err != nil {
		return nil, nil, nil, err
	}
	slot, err := self.loadSlot()
	if err != nil || slot.RunId != runId {
		return nil, nil, nil, errors.New("Docker owner no longer holds the physical host slot")
	}
	plan, err := self.loadCleanupPlan(runId, state, manifest)
	return state, manifest, plan, err
}

// A deterministic systemd identity is distinct from the evaluator service.
func dockerOwnerUnitName(runId string) string {
	return fmt.Sprintf("urnetwork-subeval-docker-%s.service", runId)
}
