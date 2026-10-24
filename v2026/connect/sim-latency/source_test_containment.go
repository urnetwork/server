// Authenticate the daemon's actual correctness-container configuration before
// candidate code starts. Requested Docker arguments alone are not evidence.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Preserve exact daemon bytes separately from lower-case evidence fields, so
// strict journal decoding does not reinterpret Docker's PascalCase schema.
type sourceContainmentObservation struct {
	ContainerId      string `json:"container_id"`
	Operation        string `json:"operation"`
	CommandSucceeded bool   `json:"command_succeeded"`
	Content          []byte `json:"content"`
}

// Retain the full private inspect response, while interpreting only the
// containment fields below. Docker's unrelated additive fields remain valid.
type sourceCorrectnessContainerInspect struct {
	Id     string
	Image  string
	Mounts []struct {
		Type        string
		Source      string
		Destination string
		Rw          bool `json:"RW"`
		Propagation string
	}
	State struct {
		Status  string
		Running bool
	}
	Config struct {
		User       string
		Entrypoint []string
		Cmd        []string
		Env        []string
		Labels     map[string]string
	}
	HostConfig struct {
		Privileged        bool
		ReadonlyRootfs    bool
		Init              *bool
		NetworkMode       string
		CgroupnsMode      string
		CgroupParent      string
		CpusetCpus        string
		Memory            int64
		MemorySwap        int64
		PidsLimit         *int64
		CapDrop           []string
		CapAdd            []string
		SecurityOpt       []string
		Binds             []string
		VolumesFrom       []string
		GroupAdd          []string
		Devices           []json.RawMessage
		DeviceRequests    []json.RawMessage
		DeviceCgroupRules []string
		PidMode           string
		IpcMode           string
		UsernsMode        string
		Mounts            []struct {
			Type     string
			Source   string
			Target   string
			ReadOnly bool
		}
		Tmpfs   map[string]string
		Ulimits []struct {
			Name string
			Soft int64
			Hard int64
		}
	}
}

// Compare unordered option sets, rejecting duplicates rather than silently
// allowing a later conflicting mount/security option to override an earlier one.
func sourceContainmentOptionsEqual(value string, expected []string) bool {
	actual := strings.Split(value, ",")
	if len(actual) != len(expected) {
		return false
	}
	seen := map[string]bool{}
	for _, option := range actual {
		if seen[option] || !slices.Contains(expected, option) {
			return false
		}
		seen[option] = true
	}
	return true
}

// A created, stopped container must bind the exact built image, fixed command,
// read-only source and metadata, offline network and management-safe limits.
func validateSourceCorrectnessContainment(content []byte, id string, input sourceBuildSandboxInput, settings sourceBuildSandboxSettings) error {
	args, err := sourceBuildSandboxArguments(input, settings)
	if err != nil || input.Phase != "correctness" || !validSha256(id) {
		return errors.New("correctness containment needs its exact bounded execution plan")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	if err := validateUniqueJSONValue(decoder); err != nil {
		return errors.New("correctness containment inspect response contains ambiguous JSON")
	}
	var observed sourceCorrectnessContainerInspect
	if json.Unmarshal(content, &observed) != nil || observed.Id != id || observed.Image != input.ImageId || observed.State.Status != "created" || observed.State.Running ||
		observed.Config.User != "65532:65532" || !slices.Equal(observed.Config.Entrypoint, []string{"/bin/bash"}) ||
		!slices.Equal(observed.Config.Cmd, args[len(args)-2:]) || observed.Config.Labels["com.urnetwork.review-build.owner"] != input.Owner ||
		observed.Config.Labels["com.urnetwork.review-build.phase"] != "correctness" {
		return errors.New("observed correctness container has another identity, command or lifecycle")
	}
	host := observed.HostConfig
	if host.Privileged || !host.ReadonlyRootfs || host.Init == nil || !*host.Init || host.NetworkMode != "none" || host.CgroupnsMode != "private" ||
		host.CgroupParent != settings.CgroupParent || host.CpusetCpus != settings.CpuSet || host.Memory != settings.MemoryBytes || host.MemorySwap != settings.MemoryBytes ||
		host.PidsLimit == nil || *host.PidsLimit != int64(settings.Pids) || len(host.CapDrop) != 1 || !strings.EqualFold(host.CapDrop[0], "ALL") || len(host.CapAdd) != 0 ||
		len(host.SecurityOpt) != 1 || (host.SecurityOpt[0] != "no-new-privileges:true" && host.SecurityOpt[0] != "no-new-privileges") ||
		len(host.Binds) != 0 || len(host.VolumesFrom) != 0 || len(host.GroupAdd) != 0 || len(host.Devices) != 0 || len(host.DeviceRequests) != 0 || len(host.DeviceCgroupRules) != 0 ||
		host.PidMode != "" || host.IpcMode != "private" || host.UsernsMode != "" {
		return errors.New("observed correctness container lost its isolation or reserved host resources")
	}
	expectedMounts := map[string]string{"/workspace": input.SourceDirectory, "/input/server-metadata": input.MetadataDirectory}
	if len(host.Mounts) != len(expectedMounts) {
		return errors.New("observed correctness container has unexpected host mounts")
	}
	for _, mount := range host.Mounts {
		if mount.Type != "bind" || !mount.ReadOnly || expectedMounts[mount.Target] == "" || mount.Source != expectedMounts[mount.Target] {
			return errors.New("observed correctness container has redirected or writable host mounts")
		}
		delete(expectedMounts, mount.Target)
	}
	expectedTmpfs := map[string][]string{
		"/work": {"rw", "exec", "nosuid", "nodev", "uid=65532", "gid=65532", "mode=0700", "size=" + strconv.FormatInt(settings.WorkBytes, 10)},
		"/tmp":  {"rw", "nosuid", "nodev", "noexec", "uid=65532", "gid=65532", "mode=1770", "size=" + strconv.FormatInt(settings.TmpBytes, 10)},
		"/opt/urnetwork/go/pkg/mod/cache/download/github.com/urnetwork/server/@v": {"rw", "nosuid", "nodev", "noexec", "uid=65532", "gid=65532", "mode=0700", "size=" + strconv.FormatInt(settings.MetadataBytes, 10)},
	}
	if len(host.Tmpfs) != len(expectedTmpfs) {
		return errors.New("observed correctness container has unexpected writable paths")
	}
	for path, options := range expectedTmpfs {
		if !sourceContainmentOptionsEqual(host.Tmpfs[path], options) {
			return errors.New("observed correctness tmpfs has changed permissions or capacity")
		}
	}
	// Image-declared anonymous volumes do not appear in HostConfig.Mounts.
	// Inspect resolved mounts too; only the two protected binds and optional
	// representations of the already-validated tmpfs mounts are permitted.
	expectedResolved := map[string]string{"/workspace": input.SourceDirectory, "/input/server-metadata": input.MetadataDirectory}
	resolvedTargets := map[string]bool{}
	for _, mount := range observed.Mounts {
		if resolvedTargets[mount.Destination] {
			return errors.New("observed correctness container has duplicate resolved mount targets")
		}
		resolvedTargets[mount.Destination] = true
		switch mount.Type {
		case "bind":
			if mount.Rw || expectedResolved[mount.Destination] == "" || mount.Source != expectedResolved[mount.Destination] ||
				(mount.Propagation != "rprivate" && mount.Propagation != "") {
				return errors.New("observed correctness resolved mount is writable, redirected or propagating")
			}
			delete(expectedResolved, mount.Destination)
		case "tmpfs":
			if _, ok := expectedTmpfs[mount.Destination]; !ok || mount.Source != "" || !mount.Rw {
				return errors.New("observed correctness container has an unexpected resolved tmpfs")
			}
		default:
			return errors.New("observed correctness container inherited an unexpected volume")
		}
	}
	if len(expectedResolved) != 0 {
		return errors.New("observed correctness container is missing its resolved protected binds")
	}
	expectedLimits := map[string]int64{"core": 0, "fsize": int64(settings.BinaryBytes), "nofile": int64(settings.NoFile)}
	if len(host.Ulimits) != len(expectedLimits) {
		return errors.New("observed correctness container has incomplete file/core limits")
	}
	for _, limit := range host.Ulimits {
		expected, ok := expectedLimits[limit.Name]
		if !ok || limit.Soft != expected || limit.Hard != expected {
			return errors.New("observed correctness container file/core limits changed")
		}
		delete(expectedLimits, limit.Name)
	}
	environment := map[string]string{}
	for _, entry := range observed.Config.Env {
		key, value, ok := strings.Cut(entry, "=")
		if _, duplicate := environment[key]; !ok || key == "" || duplicate {
			return errors.New("observed correctness environment is malformed or ambiguous")
		}
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "BRINGYOUR_") || strings.Contains(upper, "TOKEN") || strings.Contains(upper, "PASSWORD") ||
			strings.Contains(upper, "SECRET") || strings.Contains(upper, "API_KEY") {
			return errors.New("observed correctness environment contains a forbidden credential/config variable")
		}
		environment[key] = value
	}
	for key, value := range map[string]string{"WARP_ENV": "local", "WARP_SERVICE": "sim", "WARP_BLOCK": "sim"} {
		if environment[key] != value {
			return errors.New("observed correctness environment selected a non-local execution scope")
		}
	}
	for _, arg := range args {
		if entry, ok := strings.CutPrefix(arg, "--env="); ok {
			key, value, _ := strings.Cut(entry, "=")
			if environment[key] != value {
				return errors.New("observed correctness environment changed the trusted offline plan")
			}
		}
	}
	return nil
}

// The wrapper observes and retains the actual stopped container before start.
// Existing sandbox cleanup independently recovers a lost create acknowledgment
// even when this post-create inspection fails. No candidate starts on failure.
// The returned attempt-owned executor is serial, like the sandbox it wraps.
func sourceCorrectnessContainmentDocker(input sourceBuildSandboxInput, settings sourceBuildSandboxSettings, docker sourceBuildDocker) (sourceBuildDocker, error) {
	expectedArgs, err := sourceBuildSandboxArguments(input, settings)
	if err != nil || input.Phase != "correctness" || docker == nil {
		return nil, errors.New("correctness observation requires its fixed private sandbox")
	}
	observedId := ""
	createdId := ""
	createAttempted := false
	startAttempted := false
	return func(ctx context.Context, args []string, out, diagnostics io.Writer) error {
		if len(args) == 0 {
			return errors.New("empty correctness Docker operation")
		}
		if args[0] == "start" {
			if startAttempted || observedId == "" || !slices.Equal(args, []string{"start", "--attach", observedId}) {
				return errors.New("correctness container cannot start without retained observed containment")
			}
			startAttempted = true
		}
		operation := ""
		if observedId != "" && slices.Equal(args, []string{"inspect", "--format", "{{json .State}}", observedId}) {
			operation = "terminal-state"
		} else if createdId != "" && slices.Equal(args, []string{"container", "ls", "--all", "--no-trunc", "--quiet", "--filter", "id=" + createdId}) {
			operation = "cleanup"
		}
		if operation != "" {
			observed := &sourceSupervisorOutput{limit: settings.OutputBytes}
			commandErr := docker(ctx, args, io.MultiWriter(out, observed), diagnostics)
			content, err := json.Marshal(sourceContainmentObservation{ContainerId: createdId, Operation: operation,
				CommandSucceeded: commandErr == nil && !observed.exceeded, Content: observed.content})
			if err != nil {
				return errors.Join(commandErr, err)
			}
			path := filepath.Join(filepath.Dir(input.OutputDirectory), "containment-"+operation+".json")
			if err := publishSourceTestRecord(path, content); err != nil {
				return errors.Join(commandErr, err)
			}
			if observed.exceeded {
				return errors.Join(commandErr, errors.New("correctness containment observation exceeded its bound"))
			}
			return commandErr
		}
		if args[0] != "create" {
			return docker(ctx, args, out, diagnostics)
		}
		if createAttempted || !slices.Equal(args, expectedArgs) {
			return errors.New("correctness container cannot change or repeat its fixed create plan")
		}
		createAttempted = true
		identity := &sourceSupervisorOutput{limit: settings.OutputBytes}
		if err := docker(ctx, args, io.MultiWriter(out, identity), diagnostics); err != nil {
			return err
		}
		id := strings.TrimSpace(string(identity.content))
		if identity.exceeded || !validSha256(id) {
			return errors.New("correctness daemon returned an invalid container identity")
		}
		createdId = id
		observation := &sourceSupervisorOutput{limit: settings.OutputBytes}
		if err := docker(ctx, []string{"inspect", "--format", "{{json .}}", id}, observation, diagnostics); err != nil || observation.exceeded {
			return errors.New("correctness daemon containment observation is unavailable or oversized")
		}
		content, err := json.Marshal(sourceContainmentObservation{ContainerId: id, Operation: "before-start", CommandSucceeded: true, Content: observation.content})
		if err != nil {
			return err
		}
		path := filepath.Join(filepath.Dir(input.OutputDirectory), "containment-before-start.json")
		if err := publishSourceTestRecord(path, content); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		} else if errors.Is(err, os.ErrExist) {
			// A stopped-container observation is unique to one actual execution;
			// an existing file is not permission to dispatch another execution.
			return errors.New("correctness containment observation already exists; reconcile the original execution")
		}
		if err := validateSourceCorrectnessContainment(observation.content, id, input, settings); err != nil {
			return err
		}
		observedId = id
		return nil
	}, nil
}

// A passing containment artifact binds observed creation, clean terminal state
// and an empty post-removal census to the actual tests and exact image. It does
// not replace separate physical-host or future-round runtime qualification.
func retainSourceCorrectnessContainment(input sourceBuildSandboxInput, settings sourceBuildSandboxSettings, execution sourceTestExecution) (string, error) {
	if execution.ImageId != input.ImageId || execution.SuiteSha256 != sourceCorrectnessSuiteDigest() || execution.ExitCode != 0 || execution.TestCount <= 0 ||
		!validSha256(execution.TestLogsSha256) || settings.OutputBytes <= 0 {
		return "", errors.New("containment evidence requires complete exact-image test execution")
	}
	root := filepath.Dir(input.OutputDirectory)
	observations := map[string]sourceContainmentObservation{}
	digests := map[string]string{}
	for _, operation := range []string{"before-start", "terminal-state", "cleanup"} {
		var observation sourceContainmentObservation
		path := filepath.Join(root, "containment-"+operation+".json")
		limit := int64(settings.OutputBytes) * 2
		if err := readSourceTestRecord(path, limit, &observation); err != nil {
			return "", err
		}
		if !observation.CommandSucceeded || observation.Operation != operation || !validSha256(observation.ContainerId) {
			return "", errors.New("retained containment observation failed or changed identity")
		}
		content, err := json.Marshal(observation)
		if err != nil {
			return "", err
		}
		digest := sha256.Sum256(content)
		digests[operation] = hex.EncodeToString(digest[:])
		// Authenticate exact persisted bytes, not just a normalized decoded value.
		if err := verifySourceTestLogBundle(path, digests[operation], limit); err != nil {
			return "", err
		}
		observations[operation] = observation
	}
	before, terminal, cleanup := observations["before-start"], observations["terminal-state"], observations["cleanup"]
	if before.ContainerId != terminal.ContainerId || before.ContainerId != cleanup.ContainerId || len(bytes.TrimSpace(cleanup.Content)) != 0 {
		return "", errors.New("correctness cleanup did not prove the original container is absent")
	}
	if err := validateSourceCorrectnessContainment(before.Content, before.ContainerId, input, settings); err != nil {
		return "", err
	}
	var state struct {
		Status    string
		Running   bool
		ExitCode  int
		OomKilled bool `json:"OOMKilled"`
	}
	if json.Unmarshal(terminal.Content, &state) != nil || state.Status != "exited" || state.Running || state.ExitCode != 0 || state.OomKilled {
		return "", errors.New("correctness containment did not observe a clean terminal exit")
	}
	content, err := json.Marshal(struct {
		Schema         int               `json:"schema"`
		Owner          string            `json:"owner"`
		ImageId        string            `json:"image_id"`
		ContainerId    string            `json:"container_id"`
		Observations   map[string]string `json:"observations"`
		TestLogsSha256 string            `json:"test_logs_sha256"`
		SuiteSha256    string            `json:"suite_sha256"`
	}{Schema: 1, Owner: input.Owner, ImageId: input.ImageId, ContainerId: before.ContainerId, Observations: digests,
		TestLogsSha256: execution.TestLogsSha256, SuiteSha256: execution.SuiteSha256})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(content)
	expected := hex.EncodeToString(digest[:])
	path := filepath.Join(filepath.Dir(root), "independent-test-containment.json")
	if err := publishSourceTestRecord(path, content); errors.Is(err, os.ErrExist) {
		if err := verifySourceTestLogBundle(path, expected, int64(settings.OutputBytes)); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	return expected, nil
}
