// Namespace handoff is proved from one live, exactly owned container and its
// captured proc directory. Declared subuid metadata is not a live uid_map proof.
package competitionagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Reads never accept a caller PID, cgroup path, map filename or daemon. Exact
// full-container scope is rechecked before capturing its kernel proc inode.
func (self *DockerProxy) proveHostNamespace(ctx context.Context, containerId string) (HostNamespaceMap, error) {
	if !digestPattern.MatchString(containerId) || self.plan.HostFiles == nil {
		return HostNamespaceMap{}, errors.New("namespace probe has no full owned container identity")
	}
	if _, err := self.requireRuntimeBoundary(ctx); err != nil {
		return HostNamespaceMap{}, err
	}
	owned, err := self.inspectOwnedDocker(ctx, containerId, false)
	if err != nil || owned != containerId {
		return HostNamespaceMap{}, errors.New("namespace probe belongs to another container")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/containers/"+containerId+"/json", nil)
	if err != nil {
		return HostNamespaceMap{}, err
	}
	response, err := self.transport.RoundTrip(request)
	if err != nil {
		return HostNamespaceMap{}, err
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, self.settings.MaximumDocumentBytes+1))
	if err != nil || response.StatusCode != http.StatusOK || int64(len(content)) > self.settings.MaximumDocumentBytes || validateDockerMutationJson(content, self.settings.MaximumDocumentDepth) != nil {
		return HostNamespaceMap{}, errors.New("namespace container observation is unavailable")
	}
	var object struct {
		Id         string
		Config     struct{ Labels map[string]string }
		HostConfig struct{ CgroupParent string }
		State      struct {
			Running bool
			Pid     uint32
		}
	}
	if json.Unmarshal(content, &object) != nil || object.Id != containerId || !self.labelsMatch(object.Config.Labels) || !self.allowedCgroup(object.HostConfig.CgroupParent) ||
		!object.State.Running || object.State.Pid == 0 {
		return HostNamespaceMap{}, errors.New("namespace container is not positively live and scoped")
	}
	group, err := cleanupSlicePath(object.HostConfig.CgroupParent)
	if err != nil {
		return HostNamespaceMap{}, err
	}
	expectedCgroup := "/" + filepath.ToSlash(group) + "/docker-" + containerId + ".scope"
	return readHostNamespace(ctx, fmt.Sprintf("/proc/%d", object.State.Pid), expectedCgroup, *self.plan.HostFiles, DefaultHostFilesSettings())
}

// A proc-directory fd cannot be redirected by PID reuse after capture. Both
// cgroup reads must name the exact same owned scope around the map reads.
func readHostNamespace(ctx context.Context, processPath, expectedCgroup string, plan HostFilesPlan, settings HostFilesSettings) (HostNamespaceMap, error) {
	if err := ctx.Err(); err != nil {
		return HostNamespaceMap{}, err
	}
	fd, err := syscall.Open(processPath, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return HostNamespaceMap{}, err
	}
	directory := os.NewFile(uintptr(fd), "owned-container-proc")
	defer directory.Close()
	read := func(name string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fd, err := syscall.Openat(int(directory.Fd()), name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, err
		}
		file := os.NewFile(uintptr(fd), "owned-container-kernel-map")
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || settings.MaximumProcBytes <= 0 {
			return nil, errors.New("container namespace property is not a bounded regular kernel document")
		}
		content, err := io.ReadAll(io.LimitReader(file, settings.MaximumProcBytes+1))
		if err != nil || int64(len(content)) > settings.MaximumProcBytes {
			return nil, errors.New("container namespace property exceeds its bound")
		}
		return content, nil
	}
	before, err := read("cgroup")
	if err != nil || string(bytes.TrimSpace(before)) != "0::"+expectedCgroup {
		return HostNamespaceMap{}, errors.New("container process is outside its exact precommitted cgroup")
	}
	uidMap, err := read("uid_map")
	if err != nil {
		return HostNamespaceMap{}, err
	}
	gidMap, err := read("gid_map")
	if err != nil {
		return HostNamespaceMap{}, err
	}
	if validateHostNamespaceMap(uidMap, plan.ContainerRootUid, plan.ContainerIdMapLength) != nil || validateHostNamespaceMap(gidMap, plan.ContainerRootGid, plan.ContainerIdMapLength) != nil {
		return HostNamespaceMap{}, errors.New("live container namespace differs from its declared host-id allocation")
	}
	after, err := read("cgroup")
	if err != nil || !bytes.Equal(before, after) || ctx.Err() != nil {
		return HostNamespaceMap{}, errors.New("container process identity changed during namespace capture")
	}
	return HostNamespaceMap{ContainerRootUid: plan.ContainerRootUid, ContainerRootGid: plan.ContainerRootGid, ContainerIdMapLength: plan.ContainerIdMapLength,
		UidMapSha256: digestDockerBytes(uidMap), GidMapSha256: digestDockerBytes(gidMap), CgroupPath: expectedCgroup}, nil
}

// The qualified engine uses one explicit complete subordinate-id range. Extra,
// overlapping, noncanonical or host-root ranges are a different runtime class.
func validateHostNamespaceMap(content []byte, root, length uint32) error {
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != 1 || root == 0 || length <= hostFilesContainerUid || uint64(root)+uint64(length) > uint64(^uint32(0)) {
		return errors.New("container namespace is not the one declared non-root range")
	}
	fields := strings.Fields(lines[0])
	if len(fields) != 3 {
		return errors.New("container namespace has an invalid range shape")
	}
	expected := []uint32{0, root, length}
	for index, field := range fields {
		parsed, err := strconv.ParseUint(field, 10, 32)
		if err != nil || strconv.FormatUint(parsed, 10) != field || uint32(parsed) != expected[index] {
			return errors.New("container namespace contains an unexpected id mapping")
		}
	}
	return nil
}
