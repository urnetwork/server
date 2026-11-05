// Native class records are immutable descriptor-owned metadata, not a probe
// output shortcut. Kernel and image reads are fixed read-only observations;
// no helper, core handler, daemon, sysctl or candidate is started here.
package competitionagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Root review records use private single-link 0400 leaves outside candidate
// storage. The descriptor reader additionally refuses every relative alias.
func (runtimeBoundaryNativeHost) NativeClassRecord(pin Pin, maximum int64, scope RuntimeResourceScope) ([]byte, error) {
	return readRuntimeNativeRecord(pin, maximum, scope, true)
}

// Protected=false exists only for private-file descriptor regressions; the
// concrete native host above unconditionally installs root custody checks.
func readRuntimeNativeRecord(pin Pin, maximum int64, scope RuntimeResourceScope, protected bool) ([]byte, error) {
	if maximum <= 0 || maximum == math.MaxInt64 || !digestPattern.MatchString(pin.Sha256) || !recoveryAuthorityPath(pin.Path) {
		return nil, errors.New("native class sealed record has invalid authority or read bounds")
	}
	if protected {
		if err := protectedRuntimeBoundaryFile(pin.Path, scope); err != nil {
			return nil, err
		}
	}
	root, err := openDirectoryNoFollow("/")
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := openRelativeNoFollow(root, strings.TrimPrefix(pin.Path, "/"), os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm() != 0400 || before.Size() <= 0 || before.Size() > maximum {
		return nil, errors.New("native class record is not a bounded immutable private leaf")
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 || protected && stat.Uid != 0 {
		return nil, errors.New("native class record has ambiguous ownership or aliases")
	}
	content, err := io.ReadAll(io.LimitReader(file, maximum+1))
	after, afterErr := file.Stat()
	if err != nil || afterErr != nil || int64(len(content)) != before.Size() || !os.SameFile(before, after) ||
		after.Size() != before.Size() || after.Mode() != before.Mode() || !after.ModTime().Equal(before.ModTime()) || digestDockerBytes(content) != pin.Sha256 {
		return nil, errors.New("native class sealed record changed during bounded capture")
	}
	afterStat, ok := after.Sys().(*syscall.Stat_t)
	if !ok || afterStat.Nlink != 1 || afterStat.Ctim != stat.Ctim || afterStat.Uid != stat.Uid || afterStat.Gid != stat.Gid {
		return nil, errors.New("native class record generation changed during capture")
	}
	return content, nil
}

// All names are fixed kernel files below an actual procfs descriptor. Hashes
// preserve the exact release/build and raw core-pattern byte representations.
func (runtimeBoundaryNativeHost) NativeClassKernel(ctx context.Context, settings RuntimeResourceBoundarySettings) (RuntimeNativeKernelIdentity, error) {
	identity := RuntimeNativeKernelIdentity{}
	if ctx.Err() != nil || settings.DocumentBytes <= 0 || settings.DocumentBytes == math.MaxInt64 {
		return identity, errors.New("native class kernel read has no bounded owner")
	}
	root, err := openDirectoryNoFollow("/proc")
	if err != nil {
		return identity, err
	}
	defer root.Close()
	values := map[string][]byte{}
	for _, name := range []string{"sys/kernel/osrelease", "version", "sys/kernel/core_pattern", "sys/kernel/core_pipe_limit"} {
		if err := ctx.Err(); err != nil {
			return identity, err
		}
		content, err := readManagementSshFile(root, name, settings.DocumentBytes, 0x9fa0)
		if err != nil || len(content) == 0 {
			return identity, errors.New("native class kernel facts are absent or changed filesystem")
		}
		values[name] = content
	}
	release, rawLimit := string(values["sys/kernel/osrelease"]), string(values["sys/kernel/core_pipe_limit"])
	limit, err := strconv.Atoi(strings.TrimSuffix(rawLimit, "\n"))
	identity = RuntimeNativeKernelIdentity{Release: strings.TrimSuffix(release, "\n"), VersionSha256: digestDockerBytes(values["version"]),
		CorePatternSha256: digestDockerBytes(values["sys/kernel/core_pattern"]), CorePipeLimit: limit}
	if err != nil || rawLimit != strconv.Itoa(limit)+"\n" || release != identity.Release+"\n" || !validRuntimeNativeKernel(identity, settings.MaximumCorePipeLimit) ||
		values["sys/kernel/core_pattern"][0] != '|' || ctx.Err() != nil {
		return RuntimeNativeKernelIdentity{}, errors.New("native class kernel facts are incomplete or not its exact piped-handler class")
	}
	return identity, nil
}

// Old non-piped admission is byte-for-byte the same policy. Selecting the new
// class cannot convert a missing/invalid receipt into an allow-pipe fallback.
func validateRuntimeBoundaryCorePolicy(boundary RuntimeResourceBoundary, content []byte, settings RuntimeResourceBoundarySettings) error {
	if boundary.NativeClass == nil {
		return validateRuntimeBoundaryCorePattern(content, settings)
	}
	if settings.DocumentBytes <= 0 || int64(len(content)) > settings.DocumentBytes || len(content) == 0 || content[0] != '|' || boundary.nativeClass == nil ||
		digestDockerBytes(content) != boundary.nativeClass.kernel.CorePatternSha256 || boundary.NativeClass.Receipt != boundary.nativeClass.acceptance {
		return errors.New("piped host core policy has no matching reviewed native class")
	}
	identity, err := runtimeNativeIdentity(boundary)
	if err != nil || identity != boundary.nativeClass.identity {
		return errors.New("native class runtime changed after its qualification was read")
	}
	return nil
}

// The full immutable evaluator image must actually exist on this independently
// authenticated private engine; a declared digest or another image id is not it.
func verifyRuntimeNativeClassImage(ctx context.Context, boundary RuntimeResourceBoundary, settings RuntimeResourceBoundarySettings, host runtimeBoundaryHost) error {
	if boundary.NativeClass == nil {
		return nil
	}
	owner, ok := host.(runtimeNativeClassHost)
	if !ok || boundary.nativeClass == nil {
		return errors.New("native class image has no exact concrete runtime owner")
	}
	image := boundary.NativeClass.Release.EvaluatorImageDigest
	content, err := owner.NativeClassImage(ctx, boundary, image, settings)
	if err != nil || int64(len(content)) > settings.NativeBytes || validateDockerMutationJson(content, settings.DocumentDepth) != nil {
		return errors.New("native class immutable image readback is unavailable or unbounded")
	}
	var object map[string]json.RawMessage
	var id, osName, architecture string
	if json.Unmarshal(content, &object) != nil || json.Unmarshal(object["Id"], &id) != nil || json.Unmarshal(object["Os"], &osName) != nil ||
		json.Unmarshal(object["Architecture"], &architecture) != nil || id != image || osName != "linux" || architecture != "amd64" || ctx.Err() != nil {
		return errors.New("native class daemon does not hold the exact qualified Linux/amd64 image")
	}
	boundary.nativeClass.imageVerified = true
	return nil
}

// This is only a fixed immutable-image GET through the same verified private
// daemon peer. It cannot load/pull/tag/build or accept an alternate endpoint.
func (runtimeBoundaryNativeHost) NativeClassImage(ctx context.Context, boundary RuntimeResourceBoundary, image string, settings RuntimeResourceBoundarySettings) ([]byte, error) {
	if boundary.NativeClass == nil || image != boundary.NativeClass.Release.EvaluatorImageDigest || !strings.HasPrefix(image, "sha256:") ||
		!digestPattern.MatchString(strings.TrimPrefix(image, "sha256:")) {
		return nil, errors.New("native class image query selected another immutable authority")
	}
	return runtimeBoundaryEngineRead(ctx, boundary, settings, "/v1.56/images/"+image+"/json")
}
