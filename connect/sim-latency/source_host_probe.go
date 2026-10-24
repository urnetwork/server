// Collect prospective-image hardware facts using the actual pinned root host
// checker and a separate protected document. Active host configuration is never
// replaced and these facts cannot open or qualify a future round.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/urnetwork/server/controller"
)

// These settings are installed and digest-pinned, never selected by a patch.
type sourceHostProbeSettings struct {
	Command              string        `json:"command"`
	CommandSha256        string        `json:"command_sha256"`
	ActiveConfig         string        `json:"active_config"`
	Timeout              time.Duration `json:"timeout"`
	CleanupTimeout       time.Duration `json:"cleanup_timeout"`
	MaximumDocumentBytes int           `json:"maximum_document_bytes"`
}

// A host check is short and read-only; its output budget is not a candidate log.
func defaultSourceHostProbeSettings() sourceHostProbeSettings {
	return sourceHostProbeSettings{ActiveConfig: "/etc/urnetwork/competition-host.json", Timeout: 2 * time.Minute,
		CleanupTimeout: 30 * time.Second, MaximumDocumentBytes: 256 * 1024}
}

// Check every ancestor, not just the leaf. The stat seam tests policy with
// synthetic ownership; real callers always supply os.Lstat.
func validateSourceHostTrustedPath(path string, lstat func(string) (os.FileInfo, error)) error {
	canonical, err := filepath.EvalSymlinks(path)
	if lstat == nil || err != nil || !filepath.IsAbs(path) || canonical != path || path == "/" || strings.ContainsAny(path, "\r\n\x00") {
		return errors.New("host probe requires a canonical protected path")
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := lstat(current)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || info.Mode().Perm()&0o022 != 0 || info.Mode()&os.ModeSymlink != 0 ||
			(current == path && !info.Mode().IsRegular()) || (current != path && !info.IsDir()) {
			return errors.New("host probe path or ancestor is not root-owned and protected")
		}
		if current == "/" {
			return nil
		}
	}
}

// Root ownership and protected ancestry prevent another user from substituting
// a configuration or executable after its leaf digest has been checked.
func readSourceHostTrustedFile(path string, limit int) ([]byte, error) {
	if limit <= 0 {
		return nil, errors.New("host probe document budget is invalid")
	}
	if err := validateSourceHostTrustedPath(path, os.Lstat); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil || info.Size() <= 0 || info.Size() > int64(limit) {
		return nil, errors.New("host probe document is absent or oversized")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("host probe document changed while opening")
	}
	content, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil || len(content) > limit || int64(len(content)) != info.Size() {
		return nil, errors.New("host probe document changed or exceeded its budget")
	}
	return content, nil
}

// A checker exit 1 carries negative readiness facts for a prospective image.
// Preserve those facts; only malformed output, transport failure or another exit
// code is a probe error. Subsequent physical/profile validation is mandatory.
func executeSourceHostCheck(ctx context.Context, configPath string, settings sourceHostProbeSettings) (*controller.HostSelfCheck, error) {
	if settings.Timeout <= 0 || settings.CleanupTimeout <= 0 || settings.MaximumDocumentBytes <= 0 {
		return nil, errors.New("host probe limits are incomplete")
	}
	if err := validateSourceHostTrustedPath(settings.Command, os.Lstat); err != nil {
		return nil, err
	}
	if err := verifySourceSupervisorProgram(settings.Command, settings.CommandSha256); err != nil {
		return nil, err
	}
	if err := validateSourceHostTrustedPath(configPath, os.Lstat); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	command := exec.CommandContext(ctx, settings.Command, "--json", "--config", configPath)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	command.WaitDelay = settings.CleanupTimeout
	output, diagnostics := &sourceSupervisorOutput{limit: settings.MaximumDocumentBytes}, &sourceSupervisorOutput{limit: settings.MaximumDocumentBytes}
	command.Stdout, command.Stderr = output, diagnostics
	runErr := command.Run()
	var exitErr *exec.ExitError
	if ctx.Err() != nil || output.exceeded || diagnostics.exceeded || (runErr != nil && (!errors.As(runErr, &exitErr) || exitErr.ExitCode() != 1)) {
		return nil, errors.New("pinned host probe did not return a complete bounded observation")
	}
	var report controller.HostSelfCheck
	if rejectWinnerReviewCaseAliases(output.content) != nil || decodeStrictJSONBytes(output.content, &report, "host probe") != nil || report.Schema != 1 {
		return nil, errors.New("pinned host probe returned malformed host facts")
	}
	return &report, nil
}

// Invoke only after serial candidate tests finish. This records the actual
// current boot and both pinned checker reports, never fabricated round IDs or
// runtime success. Interrupted work reuses only exact retained evidence bytes.
func collectProspectiveSourceHostProfile(ctx context.Context, request reviewedSourceBuildRequest, artifacts sourceBuildArtifacts, owner string,
	sandbox sourceBuildSandboxSettings, settings sourceHostProbeSettings) (*sourceProspectiveHostProfile, string, error) {
	if os.Geteuid() != 0 || artifacts.Directory != filepath.Join(sandbox.StateDirectory, owner) || !validSha256(owner) {
		return nil, "", errors.New("prospective host evidence requires its private privileged build owner")
	}
	if err := validateSourceHostTrustedPath(settings.ActiveConfig, os.Lstat); err != nil {
		return nil, "", err
	}
	content, err := readSourceHostTrustedFile(settings.ActiveConfig, settings.MaximumDocumentBytes)
	if err != nil {
		return nil, "", err
	}
	config, err := prospectiveSourceHostConfig(content, request, artifacts)
	if err != nil {
		return nil, "", err
	}
	path := filepath.Join(artifacts.Directory, "prospective-host-config.json")
	configDigest := sha256.Sum256(config.Content)
	if err := publishSourceTestRecord(path, config.Content); errors.Is(err, os.ErrExist) {
		if err := verifySourceTestLogBundle(path, hex.EncodeToString(configDigest[:]), int64(settings.MaximumDocumentBytes)); err != nil {
			return nil, "", err
		}
	} else if err != nil {
		return nil, "", err
	}
	before, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, "", err
	}
	incumbent, err := executeSourceHostCheck(ctx, settings.ActiveConfig, settings)
	if err != nil {
		return nil, "", err
	}
	prospective, err := executeSourceHostCheck(ctx, path, settings)
	if err != nil {
		return nil, "", err
	}
	after, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, "", err
	}
	profile, err := prospectiveSourceHostProfile(request, artifacts, config, *incumbent, *prospective, strings.TrimSpace(string(before)), strings.TrimSpace(string(after)))
	if err != nil {
		return nil, "", err
	}
	encoded, err := json.Marshal(profile)
	if err != nil || len(encoded) > settings.MaximumDocumentBytes {
		return nil, "", errors.New("prospective host evidence exceeds its document budget")
	}
	digest := sha256.Sum256(encoded)
	expected := hex.EncodeToString(digest[:])
	path = filepath.Join(artifacts.Directory, "prospective-host-profile.json")
	if err := publishSourceTestRecord(path, encoded); errors.Is(err, os.ErrExist) {
		if err := verifySourceTestLogBundle(path, expected, int64(settings.MaximumDocumentBytes)); err != nil {
			return nil, "", err
		}
	} else if err != nil {
		return nil, "", err
	}
	return profile, expected, nil
}
