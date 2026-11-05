//go:build linux

// Cleanup must address the same privileged runtime selected by the frozen
// evaluator. A cleanup-only endpoint or formatter override is not sufficient.
package competitionagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// This bounded pre-launch read verifies the actual fixed daemon hierarchy and
// changes no host state. Cleanup remains usable if that daemon later degrades.
func (self *Agent) verifyCleanupRuntime(parent context.Context) (*RuntimeResourceBoundary, error) {
	if !self.protected {
		return nil, nil
	}
	if filepath.Base(self.config.Evaluator.Path) != "evaluator-subevaluation.sh" {
		return nil, errors.New("private executor cannot reuse the independently sealed baseline producer")
	}
	if self.config.SubmissionBuilderIdentity.Path != filepath.Join(filepath.Dir(self.config.Evaluator.Path), "submission-builder-identity.sh") {
		return nil, errors.New("cleanup builder helper is not the evaluator's own protected release")
	}
	if self.config.SubevaluationOwnership.Path != filepath.Join(filepath.Dir(self.config.Evaluator.Path), "subevaluation-ownership.sh") {
		return nil, errors.New("cleanup ownership helper is not the evaluator's pinned prospective release")
	}
	for _, component := range []struct {
		pin  Pin
		name string
	}{{pin: self.config.DockerRole.Dockerfile, name: "Dockerfile.submission.tracked"},
		{pin: self.config.DockerRole.CandidateCoreProfile, name: "candidate-core.seccomp.json"},
		{pin: self.config.DockerRole.StockSeccompProfile, name: "docker-stock.seccomp.json"},
		{pin: self.config.DockerRole.PostgresInitialization, name: "postgres-init.sh"}, {pin: self.config.DockerRole.ResourcePolicy, name: "resource-boundary.sh"}} {
		if component.pin.Path != filepath.Join(filepath.Dir(self.config.Evaluator.Path), component.name) {
			return nil, errors.New("private Docker role differs from its own protected evaluator release")
		}
	}
	path, err := controlProgramPath("jq")
	if err != nil || path != self.config.JsonFormatter.Path {
		return nil, errors.New("cleanup JSON formatter differs from the fixed worker search path")
	}
	if self.credential == nil || self.credential.Uid == 0 {
		return nil, errors.New("private runtime admission requires the actual trusted non-root worker identity")
	}
	operatorUid, err := trustedOperatorUid(self.config.TrustedOperatorUser)
	if err != nil {
		return nil, err
	}
	boundary, err := VerifyRuntimeResourceBoundary(parent, self.config.DockerRole.ResourceBoundary, RuntimeResourceScope{BaseDirectory: self.config.BaseDirectory,
		StateDirectory: self.config.StateDirectory, WorkDirectory: self.config.WorkDirectory, ScratchDirectory: self.config.DockerRole.ScratchDirectory,
		TrustedOperatorUid: operatorUid, WorkerUid: self.credential.Uid, WorkerGid: self.credential.Gid}, DefaultRuntimeResourceBoundarySettings())
	if err != nil {
		return nil, err
	}
	if err := validateCleanupRuntimeBinding(self.config, boundary); err != nil {
		_ = boundary.Close()
		return nil, err
	}
	policy, err := loadManagementResourcePolicy(self.config.ManagementPolicy, DefaultManagementBoundarySettings(), self.settings.ConfigBytes, true)
	if err == nil {
		err = validateRuntimeBoundaryManagementCpuSet(policy.ManagementCpuSet, boundary.ManagementCpuSet)
	}
	if err == nil {
		err = verifyManagementOrdinaryBoundary(parent, policy, boundary.Systemctl, self.settings)
	}
	if err != nil {
		_ = boundary.Close()
		return nil, err
	}
	return boundary, nil
}

// A valid private engine is not authority to select a different endpoint or
// candidate filesystem from the one that this worker and its cleanup use.
func validateCleanupRuntimeBinding(config Config, boundary *RuntimeResourceBoundary) error {
	if boundary == nil || boundary.PrivateSocketPath != config.DockerSocketPath || boundary.WorkRoot != config.WorkDirectory ||
		boundary.ScratchRoot != config.DockerRole.ScratchDirectory || boundary.WorkDeviceId == 0 {
		return errors.New("qualified private runtime differs from the cleanup/client role")
	}
	return validateRuntimeNativeClassBinding(config, *boundary)
}

// The root-only audit may read only its exact local qualification tuple. A
// report for the default engine or another host is not this runtime's proof.
func (self *Agent) validateHostCheckConfiguration(boundary *RuntimeResourceBoundary) error {
	if boundary == nil {
		return errors.New("host audit has no protected local qualification")
	}
	content, err := readHostCheckConfiguration(self.config.HostCheckConfiguration, self.settings.ConfigBytes, true)
	if err != nil {
		return errors.New("host audit configuration is unavailable")
	}
	return validateHostCheckIdentity(self.config, boundary, content)
}

// Host configuration is non-executable data. Its exact bytes still require a
// bounded protected content pin before the root audit may consume any path.
func readHostCheckConfiguration(pin Pin, maximum int64, protected bool) ([]byte, error) {
	if err := validateFilePin(pin, maximum, protected); err != nil {
		return nil, err
	}
	return readBounded(pin.Path, maximum)
}

// The legacy host document has additional hardware/marker fields. Only this
// exact tuple may bind those fields to the independently admitted private role.
func validateHostCheckIdentity(config Config, boundary *RuntimeResourceBoundary, content []byte) error {
	if boundary == nil || validateDocumentKeys(content) != nil {
		return errors.New("host audit configuration is ambiguous or unavailable")
	}
	var identity struct {
		HostId                       string `json:"host_id"`
		ImageDigest                  string `json:"image_digest"`
		QualificationSha256          string `json:"qualification_sha256"`
		DockerDaemonConfigSha256     string `json:"docker_daemon_config_sha256"`
		PrivateRuntimeBoundarySha256 string `json:"private_runtime_boundary_sha256"`
	}
	if json.Unmarshal(content, &identity) != nil || identity.HostId != config.HostId || identity.ImageDigest != config.EvaluatorImageDigest ||
		identity.QualificationSha256 != config.QualificationSha256 || identity.DockerDaemonConfigSha256 != boundary.EngineConfig.Sha256 ||
		identity.PrivateRuntimeBoundarySha256 != config.DockerRole.ResourceBoundary.Sha256 {
		return errors.New("host audit configuration differs from the private runtime identity")
	}
	return nil
}

// A cgroup-v2 mount alone does not determine Docker's hierarchy layout.
func validateCleanupDaemonCgroups(content []byte) error {
	fields := strings.Fields(string(content))
	if len(fields) != 2 || fields[0] != "systemd" || fields[1] != "2" {
		return errors.New("Docker cleanup hierarchy is not systemd cgroup v2")
	}
	return nil
}

// Lookups use the fresh child PATH, never this process's ambient PATH. The
// selected executable still undergoes its independent root/content pin check.
func controlProgramPath(name string) (string, error) {
	for _, directory := range strings.Split(strings.TrimPrefix(controlEnvironment()[0], "PATH="), ":") {
		path := filepath.Join(directory, name)
		info, err := os.Stat(path)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return path, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", errors.New("fixed evaluator executable resolution failed")
		}
	}
	return "", errors.New("fixed evaluator executable is absent")
}

// Docker CLI v29 configfile.CurrentContext selects a daemon when --host is
// absent. Permit only its local default and no command/builder aliases. Other
// fields (including registry authentication) are never exposed or used by this
// check, and the bounded input buffer is cleared immediately by its owner.
func validateDefaultDockerContext(content []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return errors.New("default Docker configuration is not an object")
	}
	seen := map[string]bool{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, ok := keyToken.(string)
		folded := strings.ToLower(key)
		if err != nil || !ok || seen[folded] {
			return errors.New("default Docker configuration has ambiguous fields")
		}
		seen[folded] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return errors.New("default Docker configuration is malformed")
		}
		switch folded {
		case "currentcontext":
			var selected *string
			if key != "currentContext" || json.Unmarshal(value, &selected) != nil || selected == nil || *selected != "" && *selected != "default" {
				clear(value)
				return errors.New("evaluator Docker context is not its fixed local default")
			}
		case "aliases":
			var aliases map[string]string
			if key != "aliases" || json.Unmarshal(value, &aliases) != nil || len(aliases) != 0 {
				clear(value)
				return errors.New("evaluator Docker aliases cannot substitute a builder or command")
			}
		}
		clear(value)
	}
	if last, err := decoder.Token(); err != nil || last != json.Delim('}') {
		return errors.New("default Docker configuration did not finish")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("default Docker configuration has trailing data")
	}
	return nil
}
