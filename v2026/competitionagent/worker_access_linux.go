// Runtime traversal is checked as the real non-root evaluator identity. A
// root-only mounted base may use a scoped search ACL; permissions are never
// broadened or home ownership changed by the agent.
package competitionagent

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
)

// A test seam preserves exact credential/path checks without requiring tests
// to own system users, change ACLs or possess setuid/setgroups capabilities.
type workerRuntimeAccessProbe func(context.Context, string, string, *syscall.Credential) error

// Fail before any state creation or qualification when a mounted runtime base
// is inaccessible through its home/mount ancestors to the evaluator account.
func verifyWorkerRuntimeAccess(ctx context.Context, config *Config, credential *syscall.Credential, probe workerRuntimeAccessProbe) error {
	if credential == nil || credential.Uid == 0 || probe == nil {
		return errors.New("runtime traversal requires the actual non-root evaluator identity")
	}
	for _, check := range []struct {
		predicate string
		path      string
	}{
		{predicate: "-x", path: config.WorkDirectory},
		{predicate: "-x", path: config.ConfigLocalDirectory},
		{predicate: "-r", path: config.ConfigLocalDirectory},
		{predicate: "-x", path: config.VaultLocalDirectory},
		{predicate: "-r", path: config.VaultLocalDirectory},
		{predicate: "-x", path: config.Evaluator.Path},
		{predicate: "-r", path: config.Evaluator.Path},
		{predicate: "-x", path: config.SelfCheck.Path},
		{predicate: "-x", path: config.SubmissionBuilderIdentity.Path},
		{predicate: "-r", path: config.SubmissionBuilderIdentity.Path},
		{predicate: "-r", path: config.SubevaluationOwnership.Path},
		{predicate: "-x", path: config.JsonFormatter.Path},
		{predicate: "-r", path: config.DockerRole.CandidateCoreProfile.Path},
		{predicate: "-r", path: config.DockerRole.StockSeccompProfile.Path},
	} {
		if err := probe(ctx, check.predicate, check.path, credential); err != nil {
			return errors.New("trusted evaluator account cannot traverse its protected runtime and fixture paths")
		}
	}
	return nil
}

// A fixed coreutils probe follows the actual ACL and every ancestor under the
// worker's uid/gid/groups. No shell or inherited service environment is used.
func probeWorkerRuntimeAccess(ctx context.Context, predicate, path string, credential *syscall.Credential) error {
	command := exec.CommandContext(ctx, "/usr/bin/test", predicate, path)
	command.Env = controlEnvironment()
	command.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
	command.Stdout, command.Stderr = &discardWriter{}, &discardWriter{}
	return command.Run()
}
