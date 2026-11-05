// Manager-owned resource ceilings are independently read back before kernel
// admission. Only the pinned systemctl and exact authenticated slices are used.
package competitionagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"syscall"
)

// These six properties are a closed systemd interface, not caller options.
const containmentManagerProperties = "MemoryMax,MemorySwapMax,TasksMax,CPUQuotaPerSecUSec,CPUQuotaPeriodUSec,AllowedCPUs"

// Only admitted slice names may select a unit; unknown/empty properties remain
// visible so they cannot be mistaken for successfully configured ceilings.
func containmentManagerReadArguments(name string) ([]string, error) {
	if _, err := cleanupSlicePath(name); err != nil {
		return nil, err
	}
	return []string{"show", "--no-pager", "--all", "--property=" + containmentManagerProperties, "--", name}, nil
}

// The root document's actual executable pin is resolved anew on every call.
// No executable is obtained from a worker request or a mutable ambient path.
func (self *hostCleanupCgroups) managerExecutable() (Pin, error) {
	content, err := (runtimeBoundaryNativeHost{}).Document(self.managerBoundary, self.managerMaximumBytes)
	if err != nil {
		return Pin{}, err
	}
	return containmentManagerExecutablePin(content, self.managerMaximumBytes)
}

// The declared tool has one fixed role/path even when a protected manifest is
// malformed. This parser cannot authorize a different executable vocabulary.
func containmentManagerExecutablePin(content []byte, maximum int64) (Pin, error) {
	boundary := RuntimeResourceBoundary{}
	if maximum <= 0 || int64(len(content)) > maximum || validateDocumentKeys(content) != nil || decodeStrict(content, &boundary) != nil || boundary.Schema != Schema || boundary.Systemctl.Path != "/usr/bin/systemctl" || !digestPattern.MatchString(boundary.Systemctl.Sha256) {
		return Pin{}, errors.New("containment manager has no fixed protected systemctl identity")
	}
	return boundary.Systemctl, nil
}

// Slice lifecycle commands use the same root-pinned native runner; cleanup's
// freeze/kill path never depends on a healthy manager or valid resource caps.
func (self *hostRunSlices) managerCommand(ctx context.Context, arguments []string) ([]byte, error) {
	owner := &hostCleanupCgroups{managerMaximumBytes: self.settings.DiagnosticBytes, limitCommandTimeout: self.settings.CleanupCommandTimeout,
		managerBoundary: self.config.DockerRole.ResourceBoundary}
	return owner.runManager(ctx, arguments)
}

// Arguments are produced only by the typed write/read builders. Protected
// content is checked before and after the bounded management process joins.
func (self *hostCleanupCgroups) runManager(parent context.Context, arguments []string) ([]byte, error) {
	if self.limitCommandTimeout <= 0 || self.managerMaximumBytes <= 0 {
		return nil, errors.New("containment manager has no finite command bounds")
	}
	ctx, cancel := context.WithTimeout(parent, self.limitCommandTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	executable, err := self.managerExecutable()
	if err != nil {
		return nil, err
	}
	file, err := openContainmentManagerExecutable(executable, DefaultRuntimeResourceBoundarySettings().ExecutableBytes, true)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	command := exec.CommandContext(ctx, "/proc/self/fd/3", arguments...)
	command.Args[0] = executable.Path
	command.ExtraFiles = []*os.File{file}
	command.Env = controlEnvironment()
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = self.limitCommandTimeout
	output := &boundedOutput{maximum: self.managerMaximumBytes}
	command.Stdout, command.Stderr = output, &discardWriter{}
	runErr := command.Run()
	if output.exceeded || ctx.Err() != nil {
		return nil, errors.New("containment manager did not return bounded acknowledged properties")
	}
	verified, err := self.managerExecutable()
	if err != nil || verified != executable {
		return nil, errors.New("containment manager executable identity changed")
	}
	if runErr != nil {
		return append([]byte(nil), output.Bytes()...), errors.New("containment manager command was not acknowledged")
	}
	return append([]byte(nil), output.Bytes()...), nil
}

// Hash and execute the same descriptor so replacing a checked pathname cannot
// select different executable bytes. Tests use private ordinary files only.
func openContainmentManagerExecutable(pin Pin, maximum int64, protected bool) (*os.File, error) {
	if maximum <= 0 || !digestPattern.MatchString(pin.Sha256) {
		return nil, errors.New("containment executable has no finite pinned identity")
	}
	if protected {
		if err := protectedRootPath(pin.Path, false, false); err != nil {
			return nil, err
		}
	}
	file, err := openRegularNoFollow(pin.Path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || info.Mode().Perm()&0111 == 0 || info.Size() > maximum {
		file.Close()
		return nil, errors.New("containment executable is missing, nonexecutable or oversized")
	}
	if protected && !rootOwnedContainmentNode(info) {
		file.Close()
		return nil, errors.New("containment executable descriptor is not protected root authority")
	}
	digest := sha256.New()
	count, err := io.Copy(digest, io.LimitReader(file, maximum+1))
	if err != nil || count > maximum || hex.EncodeToString(digest.Sum(nil)) != pin.Sha256 {
		file.Close()
		return nil, errors.New("containment executable descriptor differs from its pin")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

// The independent read seam is private to tests. Successful writes alone can
// never synthesize a readback or bypass the production manager observation.
func (self *hostCleanupCgroups) verifyManagerLimits(parent context.Context, name string, limit runSliceLimits) error {
	arguments, err := containmentManagerReadArguments(name)
	if err != nil || self.managerMaximumBytes <= 0 || self.limitCommandTimeout <= 0 {
		return errors.New("containment manager readback has no exact finite owner")
	}
	ctx, cancel := context.WithTimeout(parent, self.limitCommandTimeout)
	defer cancel()
	read := self.limitReadCommand
	if read == nil {
		read = self.runManager
	}
	content, err := read(ctx, arguments)
	if err != nil || ctx.Err() != nil {
		return errors.New("containment manager resource readback is unavailable")
	}
	if err := validateContainmentManagerLimits(content, limit, self.managerMaximumBytes); err != nil {
		return err
	}
	return ctx.Err()
}

// systemctl show uses decimal byte/task ceilings, formatted microsecond times
// and CPU ranges. Missing, duplicate, unlimited or extra properties all refuse.
func validateContainmentManagerLimits(content []byte, limit runSliceLimits, maximum int64) error {
	if maximum <= 0 || int64(len(content)) > maximum || validateRunSliceLimits(limit) != nil {
		return errors.New("containment manager reply or expected limits are unbounded")
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(content), "\n"), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || value == "" || values[key] != "" {
			return errors.New("containment manager reply is incomplete or ambiguous")
		}
		switch key {
		case "MemoryMax", "MemorySwapMax", "TasksMax", "CPUQuotaPerSecUSec", "CPUQuotaPeriodUSec", "AllowedCPUs":
			values[key] = value
		default:
			return errors.New("containment manager returned an unrelated property")
		}
	}
	if len(values) != 6 {
		return errors.New("containment manager omitted a required resource property")
	}
	for key, want := range map[string]int64{"MemoryMax": limit.MemoryBytes, "MemorySwapMax": limit.SwapBytes, "TasksMax": limit.TasksMaximum} {
		got, err := strconv.ParseInt(values[key], 10, 64)
		if err != nil || strconv.FormatInt(got, 10) != values[key] || got != want {
			return errors.New("containment manager memory or task ceiling differs from its frozen role")
		}
	}
	// CPUQuotaPerSecUSec is quota per second, not the kernel period's quota.
	// Frozen role quotas are integral multiples of their assigned CPU count.
	quotaPerSecond := limit.CpuQuotaMicros / limit.CpuPeriodMicros * 1000000
	for key, want := range map[string]int64{"CPUQuotaPerSecUSec": quotaPerSecond, "CPUQuotaPeriodUSec": limit.CpuPeriodMicros} {
		got, err := containmentManagerMicroseconds(values[key])
		if err != nil || got != want {
			return errors.New("containment manager CPU quota or period differs from its frozen role")
		}
	}
	actual := values["AllowedCPUs"]
	if strings.ContainsAny(actual, "\t\r\n") || strings.TrimSpace(actual) != actual {
		return errors.New("containment manager CPU set is not a finite range list")
	}
	maximumCpu := DefaultDockerProxySettings().MaximumCpuIndex
	gotCpus, err := dockerPolicyCpuSet(strings.ReplaceAll(actual, " ", ","), maximumCpu)
	wantCpus, wantErr := dockerPolicyCpuSet(limit.CpuSet, maximumCpu)
	if err != nil || wantErr != nil || !reflect.DeepEqual(gotCpus, wantCpus) {
		return errors.New("containment manager processors differ from the exact frozen role")
	}
	return nil
}

// systemd's format_timespan emits descending units and decimal fractions, never
// floats/exponents. Parse exact integer microseconds with checked arithmetic.
func containmentManagerMicroseconds(value string) (int64, error) {
	if value == "" || strings.TrimSpace(value) != value {
		return 0, errors.New("invalid manager microsecond duration")
	}
	total, previous := int64(0), int64(math.MaxInt64)
	for _, token := range strings.Split(value, " ") {
		var scale int64
		for _, unit := range []struct {
			suffix string
			scale  int64
		}{{suffix: "min", scale: 60 * 1000000}, {suffix: "ms", scale: 1000}, {suffix: "us", scale: 1}, {suffix: "h", scale: 60 * 60 * 1000000}, {suffix: "s", scale: 1000000}} {
			if strings.HasSuffix(token, unit.suffix) {
				scale, token = unit.scale, strings.TrimSuffix(token, unit.suffix)
				break
			}
		}
		if scale == 0 || scale >= previous {
			return 0, errors.New("manager duration has unknown or repeated units")
		}
		previous = scale
		whole, fraction, fractional := strings.Cut(token, ".")
		count, err := strconv.ParseInt(whole, 10, 64)
		if err != nil || count < 0 || strconv.FormatInt(count, 10) != whole || count > math.MaxInt64/scale {
			return 0, errors.New("manager duration is noncanonical or overflows")
		}
		part := count * scale
		if fractional {
			if fraction == "" || len(fraction) > 6 {
				return 0, errors.New("manager duration fraction is unavailable")
			}
			denominator := int64(1)
			for range fraction {
				denominator *= 10
			}
			decimal, err := strconv.ParseInt(fraction, 10, 64)
			if err != nil || strings.IndexFunc(fraction, func(c rune) bool { return c < '0' || c > '9' }) >= 0 || decimal*scale%denominator != 0 {
				return 0, errors.New("manager duration is not an exact microsecond value")
			}
			fractionalPart := decimal * scale / denominator
			if fractionalPart > math.MaxInt64-part {
				return 0, errors.New("manager duration fraction overflows")
			}
			part += fractionalPart
		}
		if part > math.MaxInt64-total {
			return 0, errors.New("manager duration sum overflows")
		}
		total += part
	}
	if total <= 0 {
		return 0, errors.New("manager duration is not positive and finite")
	}
	return total, nil
}
