//go:build linux

// Native bootstrap proof uses only a new temporary filesystem and mount unit.
package competitionagent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The opt-in never starts, stops, or edits an evaluator service or its mounts.
func TestRuntimeWorkspaceSystemdNative(t *testing.T) {
	if os.Getenv("COMPETITION_WORKSPACE_SYSTEMD_TEST") != "1" || os.Geteuid() != 0 {
		t.Skip("requires explicit root native bootstrap opt-in")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	run := func(args ...string) []byte {
		t.Helper()
		output, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("systemctl %v: %v: %s", args, err, output)
		}
		return output
	}
	root, err := os.MkdirTemp("/run", "synthetic-workspace-")
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mount("synthetic-workspace", root, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "size=1048576,mode=0700"); err != nil {
		_ = os.Remove(root)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := syscall.Unmount(root, 0); err != nil {
			t.Error(err)
			return
		}
		if err := os.Remove(root); err != nil {
			t.Error(err)
		}
	})
	if err := syscall.Mount("", root, "", syscall.MS_PRIVATE, ""); err != nil {
		t.Fatal(err)
	}
	jobs := filepath.Join(root, "jobs")
	if err := os.Mkdir(jobs, 0700); err != nil {
		t.Fatal(err)
	}
	fixture := newRuntimeBoundaryTest(t)
	fixture.boundary.CandidateRoot, fixture.boundary.WorkRoot = root, jobs
	fixture.boundary.BlockDevice = "synthetic-workspace"
	allowed := run("show", "--value", "--property=AllowedCPUs", "--", managementPrivateSliceName)
	fixture.boundary.ManagementCpuSet = strings.Join(strings.Fields(string(allowed)), ",")
	name := runtimeWorkspaceUnitName(jobs)
	fixture.boundary.WorkspaceMount = &RuntimeWorkspaceMount{Kind: runtimeWorkspacePropagation, Unit: Pin{Path: filepath.Join("/etc/systemd/system", name)}}
	body, err := RenderRuntimeWorkspaceMount(fixture.boundary, fixture.settings)
	if err != nil {
		t.Fatal(err)
	}
	// A synthetic unit must not acquire recovery or daemon dependencies.
	for _, prefix := range []string{"Before=", "OnFailure=", "OnFailureJobMode=", "RequiredBy="} {
		var filtered []byte
		for _, line := range bytes.Split(body, []byte("\n")) {
			if !bytes.HasPrefix(line, []byte(prefix)) {
				filtered = append(filtered, line...)
				filtered = append(filtered, '\n')
			}
		}
		body = filtered
	}
	unitPath := filepath.Join("/run/systemd/system", name)
	file, err := os.OpenFile(unitPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.Write(body)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		_ = os.Remove(unitPath)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		output, stopErr := exec.CommandContext(cleanupCtx, "systemctl", "stop", "--", name).CombinedOutput()
		if stopErr != nil {
			t.Errorf("stop synthetic mount: %v: %s", stopErr, output)
			return
		}
		if err := os.Remove(unitPath); err != nil {
			t.Error(err)
		}
		if output, err := exec.CommandContext(cleanupCtx, "systemctl", "daemon-reload").CombinedOutput(); err != nil {
			t.Errorf("reload after synthetic cleanup: %v: %s", err, output)
		}
	})
	run("daemon-reload")
	run("start", "--", name)
	projection := run("show", "--no-pager", "--property="+runtimeWorkspaceUnitProperties, "--", name)
	fixture.boundary.WorkspaceMount.Unit.Path = unitPath
	if _, err := parseRuntimeWorkspaceUnit(projection, fixture.boundary, fixture.settings); err != nil {
		t.Fatalf("native manager projection: %v: %s", err, projection)
	}
	present, shared, slave := workspacePropagationMountFlags(jobs)
	_, parentShared, parentSlave := workspacePropagationMountFlags(root)
	if !present || !shared || slave || parentShared || parentSlave {
		t.Fatal("bootstrap did not keep shared jobs below a private parent")
	}
	// PrivateMounts alone still makes the peer's mounts shared internally.
	// Exercise the actual manager, then require the explicit slave correction.
	for _, mode := range []string{"default", "slave"} {
		peerName := strings.TrimSuffix(name, ".mount") + "-" + mode + ".service"
		peerPath := filepath.Join("/run/systemd/system", peerName)
		outputPath := filepath.Join(root, "peer-"+mode+".mountinfo")
		body := "[Service]\nType=oneshot\nPrivateMounts=yes\nExecStart=/usr/bin/cat /proc/self/mountinfo\nStandardOutput=file:" + outputPath + "\n"
		if mode == "slave" {
			body += "MountFlags=slave\n"
		}
		peerFile, err := os.OpenFile(peerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := peerFile.WriteString(body)
		if err := errors.Join(writeErr, peerFile.Close()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
			defer cleanupCancel()
			if output, err := exec.CommandContext(cleanupCtx, "systemctl", "stop", "--", peerName).CombinedOutput(); err != nil {
				t.Errorf("stop synthetic peer: %v: %s", err, output)
			}
			if err := os.Remove(peerPath); err != nil {
				t.Error(err)
			}
			if output, err := exec.CommandContext(cleanupCtx, "systemctl", "daemon-reload").CombinedOutput(); err != nil {
				t.Errorf("reload after peer cleanup: %v: %s", err, output)
			}
		})
		run("daemon-reload")
		run("start", "--", peerName)
		peerContent, err := os.ReadFile(outputPath)
		if err != nil {
			t.Fatal(err)
		}
		peerMounts, err := parseRuntimeBoundaryMounts(peerContent, fixture.settings)
		if err != nil {
			t.Fatal(err)
		}
		hostContent, err := os.ReadFile("/proc/self/mountinfo")
		if err != nil {
			t.Fatal(err)
		}
		hostMounts, err := parseRuntimeBoundaryMounts(hostContent, fixture.settings)
		if err != nil {
			t.Fatal(err)
		}
		var hostWork, peerWork, peerParent runtimeBoundaryMount
		for _, mount := range hostMounts {
			if mount.Target == jobs {
				hostWork = mount
			}
		}
		for _, mount := range peerMounts {
			if mount.Target == jobs {
				peerWork = mount
			} else if mount.Target == root {
				peerParent = mount
			}
		}
		peerErr := validateRuntimeWorkspacePeer(hostWork, peerWork)
		if mode == "default" && (peerErr == nil || !peerParent.Shared || peerWork.SharedGroup == 0) {
			t.Fatal("counterfactual did not reproduce the manager's internally shared namespace")
		}
		if mode == "slave" && (peerErr != nil || peerParent.Shared || peerParent.Id == 0) {
			t.Fatalf("explicit slave mode did not preserve one-way propagation: %v", peerErr)
		}
	}
	t.Log("native mount manager identity and job-only propagation verified")
}
