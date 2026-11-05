//go:build linux

// The durable Docker owner closes build admission before retiring BuildKit.
// Candidate measurement requires both the private receipt and fresh kernel facts.
package competitionagent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// The helper receives only finite proof summaries, never a host target or path.
type BuildRetirementSummary struct {
	OutcomeKind                string `json:"outcome_kind"`
	RetainedMemoryMaximumBytes int64  `json:"retained_memory_maximum_bytes"`
	MemoryCurrentBytes         uint64 `json:"memory_current_bytes"`
	MemoryStatSha256           string `json:"memory_stat_sha256"`
	ReceiptSha256              string `json:"receipt_sha256"`
}

// One immutable owner/build acknowledgment tuple precedes every native action.
type buildRetirementIntent struct {
	Schema                     int    `json:"schema"`
	RunId                      string `json:"run_id"`
	OwnerPlanSha256            string `json:"owner_plan_sha256"`
	OutcomeKind                string `json:"outcome_kind"`
	BuildAckSha256             string `json:"build_ack_sha256,omitempty"`
	RetainedMemoryMaximumBytes int64  `json:"retained_memory_maximum_bytes"`
}

// Root state, not the candidate filesystem quota, retains the native proof.
type buildRetirementReceipt struct {
	Intent buildRetirementIntent `json:"intent"`
	Memory buildRetirementMemory `json:"memory"`
}

// Completion never reopens build admission, including failed/unknown attempts.
type dockerBuildRetirement struct {
	done    chan struct{}
	receipt *buildRetirementReceipt
	summary *BuildRetirementSummary
	err     error
}

// The policy's complete canonical owner plan transitively binds the agent's
// root-hashed cleanup plan. No circular self-hash or candidate sidecar is used.
func (self *DockerProxy) buildRetirementIdentity() (buildRetirementIntent, error) {
	if self.plan.BuildRetirement == nil {
		return buildRetirementIntent{}, errors.New("build retirement has no root-pinned policy")
	}
	self.stateLock.Lock()
	build := self.build
	valid := build != nil && build.issued && build.finished && build.result.Acknowledged && build.result.ExitCode == 0
	noIssued := build == nil && self.unknown == 0 && len(self.sessions) == 0 && self.buildAdmissionsClosed
	requestSha := ""
	if build != nil {
		requestSha = build.requestSha256
	}
	self.stateLock.Unlock()
	if !valid && !noIssued {
		return buildRetirementIntent{}, errors.New("retirement requires a definitively completed successful build")
	}
	kind, acknowledgment := "no-build-issued", ""
	if noIssued {
		for _, path := range []string{filepath.Join(self.plan.StateDirectory, "build.json"), self.buildDirectory()} {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				return buildRetirementIntent{}, errors.New("no-build provenance cannot pass a partial or existing build artifact")
			}
		}
	} else {
		content, err := readBounded(filepath.Join(self.plan.StateDirectory, "build.json"), self.settings.MaximumDocumentBytes)
		record := dockerProxyBuildRecord{}
		if err != nil || decodeStrict(content, &record) != nil || record.Schema != Schema || record.RunId != self.plan.RunId ||
			record.State != "acknowledged" || !record.Result.Acknowledged || record.Result.ExitCode != 0 || record.RequestSha256 != requestSha {
			return buildRetirementIntent{}, errors.New("retirement requires the original synced terminal build acknowledgment")
		}
		kind, acknowledgment = "acknowledged-build", digestDockerBytes(content)
	}
	planContent, err := json.Marshal(self.plan)
	if err != nil || int64(len(planContent)) > self.settings.MaximumDocumentBytes {
		return buildRetirementIntent{}, errors.New("retirement owner identity exceeds its finite bound")
	}
	return buildRetirementIntent{Schema: Schema, RunId: self.plan.RunId, OwnerPlanSha256: digestDockerBytes(planContent),
		OutcomeKind: kind, BuildAckSha256: acknowledgment, RetainedMemoryMaximumBytes: self.plan.BuildRetirement.RetainedMemoryMaximumBytes}, nil
}

// This operation owns one host-RPC admission. Its independent context survives
// helper disconnect, but remains inside the original absolute job deadline.
func (self *DockerProxy) retireBuild(ctx context.Context) HostFilesResult {
	result := HostFilesResult{RunId: self.plan.RunId, Operation: "retire-build", ExitCode: 2, ErrorCode: "build_retirement_unverified"}
	self.stateLock.Lock()
	existing := self.retirement
	if existing != nil {
		// A duplicate owns a real HTTP admission until its receipt is durable,
		// but cannot mutate the host. Exclude only these explicit observers
		// from the initiating retirement's join, not from final quiescence.
		self.retirementWaiters++
		self.notifyWithLock()
		done := existing.done
		self.stateLock.Unlock()
		defer func() {
			self.stateLock.Lock()
			self.retirementWaiters--
			self.notifyWithLock()
			self.stateLock.Unlock()
		}()
		select {
		case <-done:
			self.stateLock.Lock()
			summary, err := existing.summary, existing.err
			self.stateLock.Unlock()
			if err == nil && summary != nil {
				copy := *summary
				result.Acknowledged, result.ExitCode, result.ErrorCode, result.BuildRetirement = true, 0, "", &copy
			}
		case <-ctx.Done():
		}
		return result
	}
	if !self.accepting || self.unknown != 0 || self.fileHandoff || self.plan.BuildRetirement == nil ||
		self.ownerCtx.Err() != nil || !self.settings.Now().Before(self.plan.DeadlineAt) {
		self.stateLock.Unlock()
		return result
	}
	state := &dockerBuildRetirement{done: make(chan struct{})}
	self.retirement, self.buildAdmissionsClosed = state, true
	self.notifyWithLock()
	self.stateLock.Unlock()
	var receipt *buildRetirementReceipt
	var summary *BuildRetirementSummary
	err := func() error {
		release, err := self.beginHostFileHandoffExceptRetirementWaiters(ctx, true)
		if err != nil {
			return err
		}
		defer release()
		self.stateLock.Lock()
		joined := self.inflight == 1+self.retirementWaiters && len(self.sessions) == 0 && self.unknown == 0 && self.accepting
		self.stateLock.Unlock()
		if !joined || ctx.Err() != nil || !self.settings.Now().Before(self.plan.DeadlineAt) {
			return errors.New("retirement cannot pass outstanding or uncertain daemon work")
		}
		intent, err := self.buildRetirementIdentity()
		if err != nil {
			return err
		}
		content, err := json.Marshal(intent)
		if err != nil || atomicFile(self.plan.StateDirectory, "build-retiring.json", content) != nil {
			return errors.New("build retirement intent could not be retained")
		}
		memory, err := self.buildRetirer.Retire(ctx, *self.plan.BuildRetirement)
		if err != nil {
			return err
		}
		if ctx.Err() != nil || !self.settings.Now().Before(self.plan.DeadlineAt) {
			return errors.New("original job deadline elapsed before retirement publication")
		}
		receipt = &buildRetirementReceipt{Intent: intent, Memory: memory}
		body, err := json.Marshal(receipt)
		if err != nil || int64(len(body)) > self.settings.MaximumDocumentBytes ||
			atomicFile(self.plan.StateDirectory, "build-retired.json", body) != nil {
			return errors.New("build retirement receipt could not be retained")
		}
		summary = &BuildRetirementSummary{OutcomeKind: intent.OutcomeKind, RetainedMemoryMaximumBytes: intent.RetainedMemoryMaximumBytes,
			MemoryCurrentBytes: memory.CurrentBytes, MemoryStatSha256: memory.StatSha256, ReceiptSha256: digestDockerBytes(body)}
		return nil
	}()
	self.stateLock.Lock()
	state.err, state.receipt, state.summary = err, receipt, summary
	close(state.done)
	self.notifyWithLock()
	self.stateLock.Unlock()
	if err == nil {
		copy := *summary
		result.Acknowledged, result.ExitCode, result.ErrorCode, result.BuildRetirement = true, 0, "", &copy
	}
	return result
}

// Interrupted native retirement is held, not restarted or mistaken for an ACK.
// A recovered completed receipt still needs fresh kernel proof at measurement.
func (self *DockerProxy) recoverBuildRetirement() error {
	intentContent, err := readBounded(filepath.Join(self.plan.StateDirectory, "build-retiring.json"), self.settings.MaximumDocumentBytes)
	if errors.Is(err, os.ErrNotExist) {
		if _, receiptErr := os.Lstat(filepath.Join(self.plan.StateDirectory, "build-retired.json")); !errors.Is(receiptErr, os.ErrNotExist) {
			return errors.New("retirement receipt has no prior durable intent")
		}
		return nil
	}
	if err != nil {
		return err
	}
	intent := buildRetirementIntent{}
	self.buildAdmissionsClosed = true
	expected, identityErr := self.buildRetirementIdentity()
	if decodeStrict(intentContent, &intent) != nil || identityErr != nil || intent != expected {
		return errors.New("retirement intent differs from its root owner/build tuple")
	}
	state := &dockerBuildRetirement{done: make(chan struct{}), err: ErrDockerProxyUnknown}
	self.retirement, self.buildAdmissionsClosed = state, true
	defer close(state.done)
	body, err := readBounded(filepath.Join(self.plan.StateDirectory, "build-retired.json"), self.settings.MaximumDocumentBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	receipt := &buildRetirementReceipt{}
	if decodeStrict(body, receipt) != nil || receipt.Intent != intent || receipt.Memory.CurrentBytes > uint64(intent.RetainedMemoryMaximumBytes) ||
		receipt.Memory.SwapCurrentBytes != 0 || validateBuildRetirementMemory(receipt.Memory, self.buildRetirer.settings) != nil {
		return errors.New("retirement receipt omitted its bounded native proof")
	}
	state.err, state.receipt = nil, receipt
	state.summary = &BuildRetirementSummary{OutcomeKind: intent.OutcomeKind, RetainedMemoryMaximumBytes: intent.RetainedMemoryMaximumBytes,
		MemoryCurrentBytes: receipt.Memory.CurrentBytes, MemoryStatSha256: receipt.Memory.StatSha256, ReceiptSha256: digestDockerBytes(body)}
	return nil
}

// Preparation/source probes have their own four-GiB management boundary.
// Every measured stage, scorer and live comparison requires durable retirement.
func (self *DockerProxy) requireBuildRetired(ctx context.Context, cgroup string) error {
	if self.plan.BuildRetirement == nil || strings.HasSuffix(cgroup, "-preflight.slice") {
		return nil
	}
	if cgroup == self.plan.BuildCgroupName {
		self.stateLock.Lock()
		open := !self.buildAdmissionsClosed && self.retirement == nil && self.build == nil && self.accepting && self.unknown == 0
		self.stateLock.Unlock()
		if !open {
			return errors.New("tracked compiler admission closed before final assembly or retirement")
		}
		return ctx.Err()
	}
	self.stateLock.Lock()
	state := self.retirement
	valid := state != nil && state.err == nil && state.receipt != nil && state.summary != nil && self.buildAdmissionsClosed && self.unknown == 0
	self.stateLock.Unlock()
	if !valid {
		return errors.New("candidate measurement requires the durable build retirement receipt")
	}
	if _, err := self.buildRetirer.Verify(ctx, *self.plan.BuildRetirement); err != nil {
		return errors.New("candidate measurement lacks fresh frozen bounded build memory proof")
	}
	self.stateLock.Lock()
	valid = self.retirement == state && state.err == nil && self.unknown == 0 && self.accepting
	self.stateLock.Unlock()
	if !valid {
		return errors.New("build retirement custody changed during kernel verification")
	}
	return nil
}

// An exact no-argument finite operation cannot become a host target selector.
func (self *DockerProxy) validateBuildRetirementRequest(request HostFilesRequest) error {
	if request.Schema != Schema || request.RunId != self.plan.RunId || request.Operation != "retire-build" ||
		request.Path != "" || request.ContainerId != "" || request.NetworkId != "" || self.plan.BuildRetirement == nil {
		return errors.New("build retirement request expanded its finite role")
	}
	return nil
}

// Cross-field policy checks bind the same exact precommitted build identity.
func validateDockerBuildRetirementPlan(plan DockerProxyPlan, protected bool) error {
	if plan.BuildRetirement == nil {
		if protected {
			return errors.New("protected executor requires a build retirement admission policy")
		}
		return nil
	}
	policy := *plan.BuildRetirement
	if err := validateBuildRetirementPolicy(policy); err != nil {
		return err
	}
	if policy.BuildCgroupName != plan.BuildCgroupName || policy.ResourcePolicySha256 != plan.ResourcePolicySha256 || policy.Limits.CpuSet != plan.CpuSet {
		return errors.New("build retirement policy changed its original owner scope")
	}
	// The existing string/byte check is reused without another unit parser.
	bytes, err := pinnedResourceMemoryBytes(plan.BuildMemory)
	if err != nil || bytes != policy.OriginalBuildMemoryBytes {
		return errors.New("retirement original build ceiling differs from its owner")
	}
	return nil
}
