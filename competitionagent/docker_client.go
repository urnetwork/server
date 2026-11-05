// Trusted prospective helpers use a pinned non-root client, never the daemon's
// root socket. Public role metadata is separate from all signing/private config.
package competitionagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// This root-owned public sibling document contains no credential or private
// fixture path. Only the executable's own directory selects it; a caller cannot
// override its configuration, daemon, endpoint, plugin or worker identity.
type DockerClientConfig struct {
	Schema                       int    `json:"schema"`
	Agent                        Pin    `json:"agent"`
	Docker                       Pin    `json:"docker"`
	Compose                      Pin    `json:"compose"`
	BaseDirectory                string `json:"base_directory"`
	WorkDirectory                string `json:"work_directory"`
	SocketDirectory              string `json:"socket_directory"`
	DockerConfigurationDirectory string `json:"docker_configuration_directory"`
	WorkerUid                    uint32 `json:"worker_uid"`
	TrustedOperatorUid           uint32 `json:"trusted_operator_uid"`
}

// Limits are owned by one client invocation. No wire request or environment
// variable can enlarge them or select the protected public metadata path.
type dockerClientSettings struct {
	ConfigBytes          int64
	MaximumArguments     int
	MaximumArgumentBytes int
	CancelGrace          time.Duration
	Proxy                DockerProxySettings
	HostFiles            HostFilesSettings
	currentUid           func() uint32
}

func defaultDockerClientSettings() dockerClientSettings {
	return dockerClientSettings{ConfigBytes: 64 * 1024, MaximumArguments: 4096, MaximumArgumentBytes: 2 * 1024 * 1024,
		CancelGrace: 10 * time.Second, Proxy: DefaultDockerProxySettings(), HostFiles: DefaultHostFilesSettings(), currentUid: func() uint32 { return uint32(os.Geteuid()) }}
}

// Transport/unknown failures remain evaluator infrastructure errors; complete
// ordinary build refusals retain the actual acknowledged CLI exit status.
type dockerClientExitError struct{ code int }

func (self dockerClientExitError) Error() string { return "restricted Docker client failed" }

func DockerClientExitStatus(err error) int {
	var status dockerClientExitError
	if errors.As(err, &status) && status.code > 0 && status.code <= 255 {
		return status.code
	}
	return 1
}

// The public role never loads the private agent config, signing key, server
// settings, main vault, Docker user credentials or an SSH environment.
func RunDockerClient(ctx context.Context, runId string, arguments []string, input io.Reader, output, diagnostic io.Writer) error {
	settings := defaultDockerClientSettings()
	executable, err := os.Executable()
	if err != nil {
		return dockerClientExitError{code: 2}
	}
	config, err := loadDockerClientConfig(executable, settings, true)
	if err != nil {
		return dockerClientExitError{code: 2}
	}
	return runDockerClient(ctx, config, runId, arguments, input, output, diagnostic, settings)
}

// This separate public role exposes only four fixed filesystem mutations and
// one owned-container namespace check. It cannot select a private agent file,
// root command, mode, uid/gid, mount source, daemon or arbitrary path grammar.
func RunHostFilesClient(ctx context.Context, runId string, arguments []string, output io.Writer) error {
	settings := defaultDockerClientSettings()
	executable, err := os.Executable()
	if err != nil {
		return dockerClientExitError{code: 2}
	}
	config, err := loadDockerClientConfig(executable, settings, true)
	if err != nil {
		return dockerClientExitError{code: 2}
	}
	return runHostFilesClient(ctx, config, runId, arguments, output, settings)
}

// The finite positional form has no shell parsing or configurable operation
// surface. Unknown/transport failure stops this evaluation without replay.
func runHostFilesClient(ctx context.Context, config *DockerClientConfig, runId string, arguments []string, output io.Writer, settings dockerClientSettings) error {
	if config == nil || settings.currentUid == nil || settings.currentUid() != config.WorkerUid || !digestPattern.MatchString(runId) || len(arguments) == 0 || len(arguments) > 2 {
		return dockerClientExitError{code: 2}
	}
	request := HostFilesRequest{Schema: Schema, RunId: runId, Operation: arguments[0]}
	switch request.Operation {
	case "mount-evidence", "unmount-evidence", "retire-build":
		if len(arguments) != 1 {
			return dockerClientExitError{code: 2}
		}
	case "prepare-output", "reclaim-output":
		if len(arguments) != 2 || settings.HostFiles.MaximumPathBytes <= 0 || len(arguments[1]) > settings.HostFiles.MaximumPathBytes || strings.ContainsAny(arguments[1], "\x00\r\n") ||
			!filepath.IsAbs(arguments[1]) || filepath.Clean(arguments[1]) != arguments[1] || !pathWithin(filepath.Join(config.WorkDirectory, runId, ".evidence-runtime"), arguments[1]) {
			return dockerClientExitError{code: 2}
		}
		request.Path = arguments[1]
	case "verify-id-map":
		if len(arguments) != 2 || !digestPattern.MatchString(arguments[1]) {
			return dockerClientExitError{code: 2}
		}
		request.ContainerId = arguments[1]
	case "verify-network":
		if len(arguments) != 2 || !digestPattern.MatchString(arguments[1]) {
			return dockerClientExitError{code: 2}
		}
		request.NetworkId = arguments[1]
	default:
		return dockerClientExitError{code: 2}
	}
	result, err := requestHostFiles(ctx, filepath.Join(config.SocketDirectory, runId, "docker.sock"), request, settings.Proxy)
	if err != nil || !result.Acknowledged || result.RunId != runId || result.Operation != request.Operation {
		return dockerClientExitError{code: 2}
	}
	if result.ExitCode != 0 {
		return dockerClientExitError{code: result.ExitCode}
	}
	if request.Operation == "verify-id-map" {
		if result.Namespace == nil || !digestPattern.MatchString(result.Namespace.UidMapSha256) || !digestPattern.MatchString(result.Namespace.GidMapSha256) {
			return dockerClientExitError{code: 2}
		}
		content, err := json.Marshal(result.Namespace)
		if err != nil || writeDockerClientBytes(output, append(content, '\n')) != nil {
			return dockerClientExitError{code: 2}
		}
	}
	if request.Operation == "verify-network" {
		if result.Network == nil || validateHostNetworkProof(*result.Network, runId, request.NetworkId) != nil {
			return dockerClientExitError{code: 2}
		}
		content, err := json.Marshal(result.Network)
		if err != nil || writeDockerClientBytes(output, append(content, '\n')) != nil {
			return dockerClientExitError{code: 2}
		}
	}
	if request.Operation == "retire-build" {
		proof := result.BuildRetirement
		if result.ErrorCode != "" || result.Namespace != nil || result.Network != nil || proof == nil ||
			(proof.OutcomeKind != "acknowledged-build" && proof.OutcomeKind != "no-build-issued") || proof.RetainedMemoryMaximumBytes <= 0 ||
			proof.MemoryCurrentBytes > uint64(proof.RetainedMemoryMaximumBytes) || !digestPattern.MatchString(proof.MemoryStatSha256) || !digestPattern.MatchString(proof.ReceiptSha256) {
			return dockerClientExitError{code: 2}
		}
		content, err := json.Marshal(proof)
		if err != nil || writeDockerClientBytes(output, append(content, '\n')) != nil {
			return dockerClientExitError{code: 2}
		}
	}
	return nil
}

func loadDockerClientConfig(executable string, settings dockerClientSettings, protected bool) (*DockerClientConfig, error) {
	if !filepath.IsAbs(executable) || filepath.Clean(executable) != executable || settings.ConfigBytes <= 0 {
		return nil, errors.New("invalid fixed Docker client executable")
	}
	path := filepath.Join(filepath.Dir(executable), "docker-client.json")
	if protected {
		if err := protectedRootPath(path, false, false); err != nil {
			return nil, err
		}
	}
	content, err := readBounded(path, settings.ConfigBytes)
	if err != nil {
		return nil, err
	}
	config := &DockerClientConfig{}
	if err := validateDocumentKeys(content); err != nil {
		return nil, errors.New("Docker client metadata has noncanonical authority fields")
	}
	if err := decodeStrict(content, config); err != nil {
		return nil, err
	}
	if config.Agent.Path != executable || config.Schema != Schema || config.WorkerUid == 0 || filepath.Base(config.Compose.Path) != "docker-compose" {
		return nil, errors.New("Docker client metadata selected another role")
	}
	for _, pin := range []Pin{config.Agent, config.Docker, config.Compose} {
		if err := validatePin(pin, protected); err != nil {
			return nil, err
		}
	}
	if !filepath.IsAbs(config.BaseDirectory) || filepath.Clean(config.BaseDirectory) != config.BaseDirectory || config.BaseDirectory == "/" ||
		!filepath.IsAbs(config.WorkDirectory) || filepath.Clean(config.WorkDirectory) != config.WorkDirectory || config.WorkDirectory == config.BaseDirectory || !pathWithin(config.BaseDirectory, config.WorkDirectory) ||
		config.SocketDirectory != filepath.Join(config.BaseDirectory, "clients") || pathWithin(config.WorkDirectory, config.SocketDirectory) || pathWithin(config.SocketDirectory, config.WorkDirectory) ||
		config.DockerConfigurationDirectory != filepath.Join(filepath.Dir(executable), "docker-client-config") {
		return nil, errors.New("Docker client runtime roots differ from the installed role")
	}
	if protected {
		for _, path := range []string{config.BaseDirectory, config.WorkDirectory, config.SocketDirectory} {
			if err := protectedRuntimePath(path, config.BaseDirectory, config.TrustedOperatorUid); err != nil {
				return nil, err
			}
		}
		if err := protectedRootPath(config.DockerConfigurationDirectory, true, false); err != nil {
			return nil, err
		}
	}
	if err := validateDockerClientConfiguration(config, settings, protected); err != nil {
		return nil, err
	}
	return config, nil
}

// Docker29 selects plugins from its config dir, then cliPluginsExtraDirs and
// system defaults. This dedicated root-owned dir has only the one fixed JSON
// file, so a writable user plugin, credential helper or context never wins.
// Primary source: docker/cli v29.9.2 cli/config/configfile/file.go and
// cli-plugins/manager/manager_unix.go.
func validateDockerClientConfiguration(config *DockerClientConfig, settings dockerClientSettings, protected bool) error {
	directory, err := os.Open(config.DockerConfigurationDirectory)
	if err != nil {
		return err
	}
	entries, readErr := directory.Readdirnames(2)
	closeErr := directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) || closeErr != nil || len(entries) != 1 || entries[0] != "config.json" {
		return errors.New("Docker client configuration contains unexpected authority")
	}
	path := filepath.Join(config.DockerConfigurationDirectory, "config.json")
	if protected {
		if err := protectedRootPath(path, false, false); err != nil {
			return err
		}
	}
	content, err := readBounded(path, settings.ConfigBytes)
	if err != nil {
		return err
	}
	document := struct {
		PluginDirectories []string `json:"cliPluginsExtraDirs"`
	}{PluginDirectories: []string{filepath.Dir(config.Compose.Path)}}
	expected, err := json.Marshal(document)
	if err != nil || !bytes.Equal(bytes.TrimSpace(content), expected) {
		return errors.New("Docker client configuration must pin only its Compose plugin directory")
	}
	return nil
}

func validateDockerClientArguments(arguments []string, settings dockerClientSettings) error {
	if settings.MaximumArguments <= 0 || settings.MaximumArgumentBytes <= 0 || len(arguments) == 0 || len(arguments) > settings.MaximumArguments {
		return errors.New("restricted Docker command exceeds its argument count")
	}
	allowed := map[string]bool{"info": true, "image": true, "container": true, "network": true, "create": true, "cp": true,
		"rm": true, "run": true, "inspect": true, "ps": true, "compose": true, "wait": true, "logs": true, "build": true}
	if !allowed[arguments[0]] {
		return errors.New("restricted Docker command is outside the helper role")
	}
	used := 0
	for _, argument := range arguments {
		if len(argument) > settings.MaximumArgumentBytes-used || strings.ContainsAny(argument, "\x00\r\n") {
			return errors.New("restricted Docker command exceeds its byte/content bound")
		}
		used += len(argument)
		if argument == "--host" || strings.HasPrefix(argument, "--host=") || strings.HasPrefix(argument, "-H") ||
			argument == "--context" || strings.HasPrefix(argument, "--context=") || strings.HasPrefix(argument, "-c") ||
			argument == "--config" || strings.HasPrefix(argument, "--config=") {
			return errors.New("restricted Docker command cannot override endpoint, context or config")
		}
	}
	return nil
}

// Caller cancellation stops only this local client. An already committed build
// stays with the independent durable owner and cannot be replayed as new work.
func runDockerClient(ctx context.Context, config *DockerClientConfig, runId string, arguments []string, input io.Reader, output, diagnostic io.Writer, settings dockerClientSettings) error {
	if config == nil || settings.currentUid == nil || settings.currentUid() != config.WorkerUid || !digestPattern.MatchString(runId) || settings.CancelGrace <= 0 {
		return dockerClientExitError{code: 2}
	}
	if err := validateDockerClientArguments(arguments, settings); err != nil {
		return dockerClientExitError{code: 2}
	}
	socket := filepath.Join(config.SocketDirectory, runId, "docker.sock")
	work := filepath.Join(config.WorkDirectory, runId)
	if arguments[0] == "build" {
		if len(arguments) < 2 {
			return dockerClientExitError{code: 2}
		}
		result, err := RequestDockerBuild(ctx, socket, DockerBuildRequest{RunId: runId, Arguments: append([]string(nil), arguments...), ContextDirectory: arguments[len(arguments)-1]}, settings.Proxy)
		if err != nil || !result.Acknowledged || result.RunId != runId || result.ExitCode < 0 || result.ExitCode > 255 {
			return dockerClientExitError{code: 2}
		}
		for _, pipe := range []struct {
			writer  io.Writer
			content []byte
		}{{output, result.Stdout}, {diagnostic, result.Stderr}} {
			if err := writeDockerClientBytes(pipe.writer, pipe.content); err != nil {
				return dockerClientExitError{code: 2}
			}
		}
		if result.ExitCode != 0 {
			return dockerClientExitError{code: result.ExitCode}
		}
		return nil
	}
	args := append([]string{"--host", "unix://" + socket, "--config", config.DockerConfigurationDirectory}, arguments...)
	command := exec.Command(config.Docker.Path, args...)
	command.Dir = work
	command.Env = append(controlEnvironment(), "HOME="+config.DockerConfigurationDirectory, "DOCKER_CONFIG="+config.DockerConfigurationDirectory, "DOCKER_HOST=unix://"+socket)
	command.Stdin, command.Stdout, command.Stderr = input, output, diagnostic
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	exitCode, err := runProcessGroup(ctx, command, settings.CancelGrace)
	if err != nil || exitCode != 0 {
		if exitCode <= 0 || exitCode > 255 {
			exitCode = 2
		}
		return dockerClientExitError{code: exitCode}
	}
	return nil
}

func writeDockerClientBytes(writer io.Writer, content []byte) error {
	if len(content) == 0 {
		return nil
	}
	if writer == nil {
		return errors.New("missing restricted Docker output pipe")
	}
	written, err := writer.Write(content)
	if err == nil && written != len(content) {
		return io.ErrShortWrite
	}
	return err
}
