//go:build linux

// One declared shared job subtree delivers host evidence mounts to slave daemon
// views. The quota filesystem stays private; no arbitrary alias is admitted.
package competitionagent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
)

const runtimeWorkspacePropagation = "shared-work-slave-runtime/v1"
const runtimeWorkspaceUnitProperties = "Id,FragmentPath,DropInPaths,LoadState,ActiveState,SubState,InvocationID,What,Where,Slice,AllowedCPUs,ControlPID,Result"

// Bootstrap identity is pinned separately from the private-network unit group.
type RuntimeWorkspaceMount struct {
	Kind        string `json:"kind"`
	Unit        Pin    `json:"unit"`
	MountHelper Pin    `json:"mount_helper"`
}

// Mount and service generations remain fixed for the lifetime of one receipt.
type runtimeWorkspaceProof struct {
	Invocation string
	Parent     runtimeBoundaryMount
	Work       runtimeBoundaryMount
	Peers      map[string]runtimeBoundaryMount
}

// Match systemd's path-unit escaping, not shell escaping or a caller alias.
func runtimeWorkspaceUnitName(path string) string {
	var name strings.Builder
	for index, value := range []byte(strings.TrimPrefix(path, "/")) {
		switch {
		case value == '/':
			name.WriteByte('-')
		case value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || value == '_' || value == ':' || value == '.' && index != 0:
			name.WriteByte(value)
		default:
			fmt.Fprintf(&name, "\\x%02x", value)
		}
	}
	return name.String() + ".mount"
}

// Existing root/device/option checks still apply to the parent filesystem.
func validateRuntimeWorkspaceHost(boundary RuntimeResourceBoundary, parent runtimeBoundaryMount, work *runtimeBoundaryMount) error {
	relative, err := filepath.Rel(boundary.CandidateRoot, boundary.WorkRoot)
	if boundary.WorkspaceMount == nil || err != nil || relative == "." || strings.HasPrefix(relative, "..") || work == nil ||
		work.Target != boundary.WorkRoot || work.Root != "/"+filepath.ToSlash(relative) || work.DeviceId != parent.DeviceId || work.Source != parent.Source || work.Type != parent.Type ||
		work.SharedGroup == 0 || work.MasterGroup != 0 || !work.Options["rw"] || !work.Options["nosuid"] || !work.Options["nodev"] || work.Options["ro"] || parent.Shared {
		return errors.New("workspace is not the exact shared job-only self-bind below a private quota root")
	}
	return nil
}

// Only these two selected records enter generation comparisons. Unrelated
// mounts may change while Docker operates without invalidating this receipt.
func runtimeWorkspaceView(content []byte, boundary RuntimeResourceBoundary, settings RuntimeResourceBoundarySettings) (runtimeBoundaryMount, runtimeBoundaryMount, error) {
	mounts, err := parseRuntimeBoundaryMounts(content, settings)
	if err != nil {
		return runtimeBoundaryMount{}, runtimeBoundaryMount{}, err
	}
	var parent, work *runtimeBoundaryMount
	for index := range mounts {
		mount := &mounts[index]
		if mount.Target == boundary.CandidateRoot {
			if parent != nil {
				return runtimeBoundaryMount{}, runtimeBoundaryMount{}, errors.New("stacked workspace parent")
			}
			parent = mount
		}
		if mount.Target == boundary.WorkRoot {
			if work != nil {
				return runtimeBoundaryMount{}, runtimeBoundaryMount{}, errors.New("stacked workspace root")
			}
			work = mount
		}
	}
	if parent == nil || work == nil {
		return runtimeBoundaryMount{}, runtimeBoundaryMount{}, errors.New("workspace mount view is incomplete")
	}
	if parent.Shared || parent.Source != boundary.BlockDevice || parent.Root != "/" || parent.Type != "ext4" || !parent.Options["rw"] || !parent.Options["nosuid"] || !parent.Options["nodev"] || parent.Options["ro"] {
		return runtimeBoundaryMount{}, runtimeBoundaryMount{}, errors.New("workspace parent lost its private quota identity")
	}
	return *parent, *work, nil
}

// The slave must refer to this host's exact master and the same bounded subtree.
func validateRuntimeWorkspacePeer(host runtimeBoundaryMount, peer runtimeBoundaryMount) error {
	if host.SharedGroup == 0 || host.MasterGroup != 0 || peer.SharedGroup != 0 || peer.MasterGroup != host.SharedGroup || peer.Target != host.Target || peer.Root != host.Root ||
		peer.DeviceId != host.DeviceId || peer.Type != host.Type || peer.Source != host.Source || !peer.Options["rw"] || !peer.Options["nosuid"] || !peer.Options["nodev"] || peer.Options["ro"] {
		return errors.New("runtime workspace is not a one-way slave of the exact host job mount")
	}
	return nil
}

// Mount units carry no live helper PID after setup. Their pinned fragment,
// invocation, exact backing What/Where and management placement stay required.
// systemd normalizes a bind mount's What to its backing filesystem source.
func parseRuntimeWorkspaceUnit(content []byte, boundary RuntimeResourceBoundary, settings RuntimeResourceBoundarySettings) (string, error) {
	if boundary.WorkspaceMount == nil || int64(len(content)) > settings.DocumentBytes {
		return "", errors.New("workspace unit projection is absent or unbounded")
	}
	properties := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(content), "\n"), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			return "", errors.New("invalid workspace unit projection")
		}
		if _, found := properties[key]; found {
			return "", errors.New("duplicated workspace unit property")
		}
		properties[key] = value
	}
	expected := strings.Split(runtimeWorkspaceUnitProperties, ",")
	if len(properties) != len(expected) {
		return "", errors.New("workspace projection omitted or added properties")
	}
	for _, key := range expected {
		if _, present := properties[key]; !present {
			return "", errors.New("workspace projection omitted a mandatory property")
		}
	}
	if properties["Id"] != filepath.Base(boundary.WorkspaceMount.Unit.Path) || properties["FragmentPath"] != boundary.WorkspaceMount.Unit.Path || properties["DropInPaths"] != "" ||
		properties["LoadState"] != "loaded" || properties["ActiveState"] != "active" || properties["SubState"] != "mounted" || properties["What"] != boundary.BlockDevice || properties["Where"] != boundary.WorkRoot ||
		properties["Slice"] != managementPrivateSliceName || properties["ControlPID"] != "0" || properties["Result"] != "success" || len(properties["InvocationID"]) != 32 || !isDockerProxyHex(properties["InvocationID"]) {
		return "", errors.New("workspace mount unit changed its bootstrap identity or lifetime")
	}
	if err := validateRuntimeBoundaryManagementManager(properties["AllowedCPUs"], boundary); err != nil {
		return "", err
	}
	return properties["InvocationID"], nil
}

// Capture/recheck only after the ordinary unit and namespace generation checks.
// I/O is outside stateLock; concurrent checks cannot renew a changed mount.
func (self *runtimeBoundaryNativeNamespace) verifyWorkspace(ctx context.Context, units map[string]runtimeBoundaryUnit) error {
	if self.boundary.WorkspaceMount == nil {
		return nil
	}
	if err := validateFilePin(self.boundary.WorkspaceMount.Unit, self.settings.DocumentBytes, true); err != nil {
		return err
	}
	if err := validatePin(self.boundary.WorkspaceMount.MountHelper, true); err != nil {
		return err
	}
	output, err := runtimeBoundaryRunCommand(ctx, self.boundary.Systemctl, []string{"show", "--no-pager", "--property=" + runtimeWorkspaceUnitProperties, "--", filepath.Base(self.boundary.WorkspaceMount.Unit.Path)}, nil, self.settings)
	if err != nil {
		return err
	}
	invocation, err := parseRuntimeWorkspaceUnit(output, self.boundary, self.settings)
	if err != nil {
		return err
	}
	content, err := readBounded("/proc/self/mountinfo", self.settings.NativeBytes)
	if err != nil {
		return err
	}
	parent, work, err := runtimeWorkspaceView(content, self.boundary, self.settings)
	if err != nil {
		return err
	}
	if err := validateRuntimeWorkspaceHost(self.boundary, parent, &work); err != nil {
		return err
	}
	proof := &runtimeWorkspaceProof{Invocation: invocation, Parent: parent, Work: work, Peers: map[string]runtimeBoundaryMount{}}
	for _, name := range []string{runtimeBoundaryEngineUnitName, runtimeBoundaryContainerdUnitName} {
		unit := units[name]
		if unit.Pid <= 1 {
			return errors.New("workspace peer has no leased process")
		}
		peerContent, err := readBounded(filepath.Join("/proc", strconv.Itoa(unit.Pid), "mountinfo"), self.settings.NativeBytes)
		if err != nil {
			return err
		}
		peerParent, peer, err := runtimeWorkspaceView(peerContent, self.boundary, self.settings)
		if err != nil {
			return err
		}
		if peerParent.DeviceId != parent.DeviceId {
			return errors.New("workspace peer changed its bounded filesystem")
		}
		if err := validateRuntimeWorkspacePeer(work, peer); err != nil {
			return err
		}
		after, err := runtimeBoundaryParticipant(unit, self.boundary, self.settings)
		if err != nil || after.ProcessStart != unit.ProcessStart || after.MountNamespace != unit.MountNamespace {
			return errors.New("workspace peer restarted during observation")
		}
		proof.Peers[name] = peer
	}
	content, err = readBounded("/proc/self/mountinfo", self.settings.NativeBytes)
	if err != nil {
		return err
	}
	afterParent, afterWork, err := runtimeWorkspaceView(content, self.boundary, self.settings)
	if err != nil || !reflect.DeepEqual(parent, afterParent) || !reflect.DeepEqual(work, afterWork) {
		return errors.New("workspace mount generation changed during observation")
	}
	return self.acceptWorkspaceProof(proof)
}

// Retain owned copies, so a later observation cannot mutate an earlier proof.
// Close and capture are ordered under the same lock; no I/O runs while held.
func (self *runtimeBoundaryNativeNamespace) acceptWorkspaceProof(proof *runtimeWorkspaceProof) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return errors.New("workspace proof is closed")
	}
	if self.workspace != nil {
		if !reflect.DeepEqual(self.workspace, proof) {
			return errors.New("workspace mount or unit generation changed")
		}
		return nil
	}
	copyMount := func(mount runtimeBoundaryMount) runtimeBoundaryMount {
		options := map[string]bool{}
		for key, value := range mount.Options {
			options[key] = value
		}
		mount.Options = options
		return mount
	}
	copy := &runtimeWorkspaceProof{Invocation: proof.Invocation, Parent: copyMount(proof.Parent), Work: copyMount(proof.Work), Peers: map[string]runtimeBoundaryMount{}}
	for role, mount := range proof.Peers {
		copy.Peers[role] = copyMount(mount)
	}
	self.workspace = copy
	return nil
}

// systemd creates the host master before daemon namespaces are cloned. This
// mounts only WorkRoot onto itself; the parent filesystem is never made shared.
func RenderRuntimeWorkspaceMount(boundary RuntimeResourceBoundary, settings RuntimeResourceBoundarySettings) ([]byte, error) {
	if err := validateRuntimeResourceBoundarySettings(settings); err != nil {
		return nil, err
	}
	if boundary.WorkspaceMount == nil || boundary.WorkspaceMount.Kind != runtimeWorkspacePropagation || !filepath.IsAbs(boundary.WorkRoot) || filepath.Clean(boundary.WorkRoot) != boundary.WorkRoot ||
		!pathWithin(boundary.CandidateRoot, boundary.WorkRoot) || strings.ContainsAny(boundary.WorkRoot+boundary.CandidateRoot, "$%\\ \t\r\n\x00") ||
		boundary.WorkspaceMount.Unit.Path != "/etc/systemd/system/"+runtimeWorkspaceUnitName(boundary.WorkRoot) {
		return nil, errors.New("workspace mount rendering requires exact closed authority paths")
	}
	if _, err := dockerPolicyCpuSet(boundary.ManagementCpuSet, DefaultDockerProxySettings().MaximumCpuIndex); err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf("[Unit]\nDescription=Bounded evaluation job mount propagation\nRequiresMountsFor=%s\nConditionPathIsMountPoint=%s\nBefore=%s %s\nOnFailure=%s\nOnFailureJobMode=replace\n\n[Mount]\nWhat=%s\nWhere=%s\nType=none\nOptions=bind,shared,nosuid,nodev\nTimeoutSec=%s\nSlice=%s\nAllowedCPUs=%s\n\n[Install]\nRequiredBy=%s %s\n", boundary.CandidateRoot, boundary.CandidateRoot, runtimeBoundaryContainerdUnitName, runtimeBoundaryEngineUnitName, managementRecoveryServiceName, boundary.WorkRoot, boundary.WorkRoot, strconv.FormatFloat(settings.CommandTimeout.Seconds(), 'f', 6, 64), managementPrivateSliceName, boundary.ManagementCpuSet, runtimeBoundaryContainerdUnitName, runtimeBoundaryEngineUnitName)), nil
}
