// Classification owns the kernel pipe without reading it. Every uncertain
// branch closes it, performs no handoff, and grants no cleanup/admission proof.
package competitioncore

import (
	"context"
	"errors"
	"io"
	"path/filepath"
)

// Production dependencies cannot be replaced through a public constructor.
type Handler struct {
	settings Settings
	host     host
}

// Construction is inert and never installs a pattern or changes host sysctls.
func NewHandler(settings Settings) *Handler {
	return &Handler{settings: settings, host: nativeHost{settings: settings}}
}

// Missing/zero limits refuse work; settings cannot relax the reviewed resource
// class or authorize ordinary script execution that has no native backend.
func validateSettings(settings Settings) error {
	if settings.DocumentBytes <= 0 || settings.ProcBytes <= 0 || settings.ArgumentBytes <= 0 || settings.DocumentDepth <= 0 || settings.DocumentTokens <= 0 ||
		settings.MaximumRuns <= 0 || settings.MaximumCgroupDepth <= 0 || settings.MaximumCgroupBytes <= 0 || settings.MaximumCpuIndex <= 0 ||
		settings.MaximumDescriptor == 0 || settings.HandlerTimeout <= 0 || settings.MaximumJobLifetime <= 0 || settings.HandlerMemoryBytes != 256*1024*1024 ||
		settings.HandlerTasksMaximum != 128 || settings.HandlerCpuQuota != 200000 || settings.HandlerCpuPeriod != 100000 {
		return errors.New("core dispatcher has no finite qualified resource class")
	}
	return nil
}

// Root metadata cannot select another Apport or silently waive a changed pipe
// limit. Interpreter identity is necessary metadata, never execution authority.
func validatePolicy(policy Policy, settings Settings) error {
	if policy.Schema != Schema || policy.ExpectedPipeLimit != ExpectedPipeLimit || policy.Apport.Path != ApportPath || !validHex(policy.Apport.Sha256, 64) ||
		!filepath.IsAbs(policy.Interpreter.Path) || filepath.Clean(policy.Interpreter.Path) != policy.Interpreter.Path || policy.Interpreter.Path == "/" ||
		!validHex(policy.Interpreter.Sha256, 64) || !validCpuSet(policy.ManagementCpuSet, settings.MaximumCpuIndex) {
		return errors.New("core dispatcher lacks its fixed protected policy")
	}
	return nil
}

// The only component which receives stdin's stream is a positively selected
// handoff backend; classification never reads, drains, logs or writes core data.
func (self *Handler) Handle(parent context.Context, arguments []string, core io.ReadCloser) Decision {
	if core == nil {
		return UnknownDropped
	}
	defer core.Close()
	if self == nil || self.host == nil || validateSettings(self.settings) != nil {
		return UnknownDropped
	}
	ctx, cancel := context.WithTimeout(parent, self.settings.HandlerTimeout)
	defer cancel()
	if ctx.Err() != nil {
		return UnknownDropped
	}
	invocation, err := ParseInvocation(arguments, self.settings)
	if err != nil {
		return UnknownDropped
	}
	policy, err := self.host.Policy(ctx)
	if err != nil || validatePolicy(policy, self.settings) != nil {
		return UnknownDropped
	}
	limit, err := self.host.PipeLimit(ctx)
	if err != nil || limit != policy.ExpectedPipeLimit || limit == 0 {
		return UnknownDropped
	}
	pattern, err := self.host.Pattern(ctx)
	if err != nil || pattern != DispatcherPattern || len(pattern) > 127 {
		return UnknownDropped
	}
	// Manager+kernel proof precedes the own-PID-only move, then all crash
	// classification. Its production manager backend is deliberately absent.
	placement, err := self.host.Place(ctx, policy)
	if err != nil {
		return UnknownDropped
	}
	defer placement.Close()
	snapshot, err := self.host.Snapshot(ctx)
	if err != nil || ValidateRegistry(snapshot.Registry, self.settings) != nil {
		return UnknownDropped
	}
	boot, err := self.host.BootId(ctx)
	if err != nil || boot != snapshot.Registry.BootId {
		return UnknownDropped
	}
	process, err := self.host.Process(ctx, invocation)
	if err != nil {
		return UnknownDropped
	}
	defer process.Close()
	first, err := process.Facts(ctx)
	if err != nil || first.HostPid != invocation.HostPid || first.NamespacePid != invocation.NamespacePid || first.Uid != invocation.Uid || first.Gid != invocation.Gid || first.Birth == 0 {
		return UnknownDropped
	}
	scoped, err := scopedCgroup(snapshot.Registry, first.Cgroup, self.settings)
	if err != nil {
		return UnknownDropped
	}
	second, err := process.Facts(ctx)
	if err != nil || second != first || self.host.Recheck(ctx, snapshot) != nil || placement.Recheck(ctx) != nil || ctx.Err() != nil {
		return UnknownDropped
	}
	if scoped {
		return CandidateDropped
	}
	// Restore is real ownership transfer, not just a nil return from exec.
	// Production fails closed here until per-thread affinity/cgroup/rlimit
	// restoration and script/interpreter/FD lifetime are actually implemented.
	if placement.Restore(ctx) != nil {
		return OrdinaryDropped
	}
	third, err := process.Facts(ctx)
	if err != nil || third != first || self.host.Recheck(ctx, snapshot) != nil || ctx.Err() != nil {
		return UnknownDropped
	}
	if err := self.host.Handoff(ctx, policy, invocation, core); err != nil {
		return OrdinaryDropped
	}
	return OrdinaryHanded
}
