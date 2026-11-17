//go:build linux

// Only the pinned root host audit can use this read-only private-engine role.
// It never creates an untracked namespace probe or touches the user's daemon.
package competitionagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// No worker sudo rule grants this entrypoint. Its closed CLI grammar allows
// bounded local metadata only; daemon mutations remain per-run owned requests.
func RunDockerProbe(ctx context.Context, config *Config, arguments []string, output io.Writer) error {
	if os.Geteuid() != 0 || config == nil {
		return errors.New("private Docker facts require the root host-audit role")
	}
	executable, err := os.Executable()
	if err != nil || executable != config.Agent.Path {
		return errors.New("Docker host audit is not its pinned installed executable")
	}
	if err := config.validateForPurpose(true, true); err != nil {
		return err
	}
	if len(arguments) == 1 && arguments[0] == "engine-profile" {
		operator, err := trustedOperatorUid(config.TrustedOperatorUser)
		if err != nil {
			return err
		}
		worker, err := workerCredential(config.WorkerUser)
		if err != nil {
			return err
		}
		scope := RuntimeResourceScope{BaseDirectory: config.BaseDirectory, StateDirectory: config.StateDirectory, WorkDirectory: config.WorkDirectory, ScratchDirectory: config.DockerRole.ScratchDirectory, TrustedOperatorUid: operator, WorkerUid: worker.Uid, WorkerGid: worker.Gid}
		profile, err := readDockerEngineProfile(config.DockerRole.ResourceBoundary, scope, runtimeBoundaryNativeHost{}, DefaultRuntimeResourceBoundarySettings())
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(profile)
	}
	settings := defaultDockerClientSettings()
	client, err := loadDockerClientConfig(config.Agent.Path, settings, true)
	if err != nil || client.Docker != config.Docker || client.BaseDirectory != config.BaseDirectory || client.WorkDirectory != config.WorkDirectory {
		return errors.New("host audit selected another public Docker role")
	}
	arguments, err = dockerProbeArguments(config, client, arguments)
	if err != nil {
		return err
	}
	agentSettings := DefaultAgentSettings()
	operationCtx, cancel := context.WithTimeout(ctx, agentSettings.CleanupCommandTimeout)
	defer cancel()
	buffer := &boundedOutput{maximum: agentSettings.DiagnosticBytes}
	command := exec.Command(config.Docker.Path, arguments...)
	command.Env = append(controlEnvironment(), "HOME="+client.DockerConfigurationDirectory,
		"DOCKER_CONFIG="+client.DockerConfigurationDirectory, "DOCKER_HOST=unix://"+config.DockerSocketPath)
	command.Stdout, command.Stderr = buffer, &discardWriter{}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	code, err := runProcessGroup(operationCtx, command, agentSettings.CancelGrace)
	if err != nil || code != 0 || buffer.exceeded {
		return errors.New("bounded private Docker fact query failed")
	}
	return writeDockerClientBytes(output, buffer.Bytes())
}

// Configuration identity is separate from kernel/pressure qualification. The
// host audit uses the exact native private profile, never the legacy daemon's
// different log/network settings. This operation executes no Docker command.
type dockerEngineProfile struct {
	Schema         int    `json:"schema"`
	Kind           string `json:"kind"`
	BoundarySha256 string `json:"private_runtime_boundary_sha256"`
	EngineSha256   string `json:"engine_config_sha256"`
}

// The seam replaces protected file I/O only; production policy comparison is
// shared with native admission and the provisioning renderer.
func readDockerEngineProfile(pin Pin, scope RuntimeResourceScope, host interface {
	Document(Pin, int64) ([]byte, error)
}, settings RuntimeResourceBoundarySettings) (*dockerEngineProfile, error) {
	if err := validateRuntimeResourceBoundarySettings(settings); err != nil {
		return nil, err
	}
	content, err := host.Document(pin, settings.DocumentBytes)
	if err != nil {
		return nil, err
	}
	boundary := RuntimeResourceBoundary{}
	if validateDocumentKeys(content) != nil || decodeStrict(content, &boundary) != nil {
		return nil, errors.New("invalid private engine audit boundary")
	}
	if err = boundary.validate(scope); err != nil {
		return nil, err
	}
	engine, err := host.Document(boundary.EngineConfig, settings.DocumentBytes)
	if err != nil {
		return nil, err
	}
	containerd, err := host.Document(boundary.ContainerdConfig, settings.DocumentBytes)
	if err != nil {
		return nil, err
	}
	network, err := host.Document(boundary.NetworkPolicy, settings.DocumentBytes)
	if err != nil {
		return nil, err
	}
	if err = validateRuntimeBoundaryDocuments(boundary, engine, containerd, network, settings); err != nil {
		return nil, err
	}
	return &dockerEngineProfile{Schema: Schema, Kind: "sim-latency-private-engine-profile/v1", BoundarySha256: pin.Sha256, EngineSha256: boundary.EngineConfig.Sha256}, nil
}

// Fixed format strings prevent general template evaluation, while immutable
// local image references cannot pull, exec, mount, copy or inspect a container.
func dockerProbeArguments(config *Config, client *DockerClientConfig, arguments []string) ([]string, error) {
	if config == nil || client == nil || len(arguments) < 3 {
		return nil, errors.New("invalid fixed Docker fact query")
	}
	valid := false
	if len(arguments) == 3 && arguments[1] == "--format" {
		valid = arguments[0] == "version" && arguments[2] == "{{.Server.Version}}" ||
			arguments[0] == "info" && (arguments[2] == "{{.CgroupVersion}}" || arguments[2] == "{{json .SecurityOptions}}")
	}
	if len(arguments) == 5 && arguments[0] == "image" && arguments[1] == "inspect" && arguments[2] == "--format" && arguments[3] == "{{.Id}}" {
		reference := arguments[4]
		isDigest := func(value string) bool {
			return strings.HasPrefix(value, "sha256:") && digestPattern.MatchString(strings.TrimPrefix(value, "sha256:"))
		}
		valid = isDigest(reference)
		if !valid {
			parts := strings.Split(reference, "@")
			valid = len(parts) == 2 && parts[0] != "" && isDigest(parts[1]) &&
				!strings.ContainsAny(parts[0], "\x00\r\n\t ") && !strings.HasPrefix(parts[0], "-")
		}
	}
	if !valid || config.DockerSocketPath == "" || client.DockerConfigurationDirectory == "" {
		return nil, errors.New("Docker host audit has no such read-only operation")
	}
	result := []string{"--host", "unix://" + config.DockerSocketPath, "--config", client.DockerConfigurationDirectory}
	return append(result, arguments...), nil
}
