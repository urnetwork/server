//go:build linux

// Isolated kernel proof: private candidate root, optional shared job subtree,
// slave daemon view, synchronized mount visibility and normal unmount cleanup.
package competitionagent

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Bound the projection to the synthetic tree, never the host's mount inventory.
type workspacePropagationObservation struct {
	Visible      bool   `json:"visible"`
	Sha256       string `json:"sha256,omitempty"`
	MountPresent bool   `json:"mount_present"`
	Shared       bool   `json:"shared"`
	Slave        bool   `json:"slave"`
	RootPrivate  bool   `json:"root_private"`
	DeviceId     uint64 `json:"device_id,omitempty"`
	Inode        uint64 `json:"inode,omitempty"`
}

// Unwind the fixture's deferred cleanup on an unexpected native failure.
func workspacePropagationMust(err error) {
	if err != nil {
		panic(err)
	}
}

// Observe only the exact fixture mountpoint in the current namespace.
func workspacePropagationMountFlags(path string) (present, shared, slave bool) {
	content, err := os.ReadFile("/proc/self/mountinfo")
	workspacePropagationMust(err)
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 || fields[4] != path {
			continue
		}
		present = true
		for _, field := range fields[6:] {
			if field == "-" {
				break
			}
			shared = shared || strings.HasPrefix(field, "shared:")
			slave = slave || strings.HasPrefix(field, "master:")
		}
	}
	return
}

// Read after a pipe barrier so visibility never depends on scheduler timing.
func workspacePropagationObserve(root string) workspacePropagationObservation {
	jobs := filepath.Join(root, "jobs")
	_, shared, slave := workspacePropagationMountFlags(jobs)
	_, rootShared, rootSlave := workspacePropagationMountFlags(root)
	present, _, _ := workspacePropagationMountFlags(filepath.Join(jobs, "run"))
	content, err := os.ReadFile(filepath.Join(jobs, "run", "marker"))
	result := workspacePropagationObservation{Visible: err == nil, MountPresent: present, Shared: shared, Slave: slave, RootPrivate: !rootShared && !rootSlave}
	if err == nil {
		var stat syscall.Stat_t
		workspacePropagationMust(syscall.Stat(filepath.Join(jobs, "run", "marker"), &stat))
		result.DeviceId, result.Inode = uint64(stat.Dev), stat.Ino
		digest := sha256.Sum256(content)
		result.Sha256 = hex.EncodeToString(digest[:])
	} else if !errors.Is(err, os.ErrNotExist) {
		workspacePropagationMust(err)
	}
	return result
}

// One leased peer reads two bounded observations and exits when stdin closes.
func workspacePropagationChild(root string) {
	ready := os.NewFile(3, "ready")
	commands := bufio.NewScanner(os.NewFile(4, "commands"))
	output := json.NewEncoder(os.NewFile(5, "observations"))
	_, err := ready.Write([]byte("ready\n"))
	workspacePropagationMust(err)
	workspacePropagationMust(ready.Close())
	for commands.Scan() {
		if commands.Text() != "workspacePropagationObserve" {
			panic("unknown finite phase")
		}
		workspacePropagationMust(output.Encode(workspacePropagationObserve(root)))
	}
	workspacePropagationMust(commands.Err())
}

// Use only new temporary mounts; normal unmount and process join are mandatory.
func workspacePropagationTrial(shared bool) (result map[string]any, err error) {
	root, err := os.MkdirTemp(os.TempDir(), "workspace-propagation-")
	if err != nil {
		return nil, err
	}
	rootMounted, jobsMounted, runMounted := false, false, false
	defer func() {
		if runMounted {
			err = errors.Join(err, syscall.Unmount(filepath.Join(root, "jobs", "run"), 0))
		}
		if jobsMounted {
			err = errors.Join(err, syscall.Unmount(filepath.Join(root, "jobs"), 0))
		}
		if rootMounted {
			err = errors.Join(err, syscall.Unmount(root, 0))
		}
		if err == nil {
			err = os.Remove(root)
		}
	}()
	if err = syscall.Mount("synthetic-candidate", root, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "size=16777216,mode=0700"); err != nil {
		return nil, err
	}
	rootMounted = true
	if err = syscall.Mount("", root, "", syscall.MS_PRIVATE, ""); err != nil {
		return nil, err
	}
	jobs := filepath.Join(root, "jobs")
	if err = os.Mkdir(jobs, 0700); err != nil {
		return nil, err
	}
	if err = syscall.Mount(jobs, jobs, "", syscall.MS_BIND, ""); err != nil {
		return nil, err
	}
	jobsMounted = true
	flag := uintptr(syscall.MS_PRIVATE)
	if shared {
		flag = syscall.MS_SHARED
	}
	if err = syscall.Mount("", jobs, "", flag, ""); err != nil {
		return nil, err
	}
	run := filepath.Join(jobs, "run")
	if err = os.Mkdir(run, 0700); err != nil {
		return nil, err
	}
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer readyRead.Close()
	defer readyWrite.Close()
	commandRead, commandWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer commandRead.Close()
	defer commandWrite.Close()
	resultRead, resultWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer resultRead.Close()
	defer resultWrite.Close()
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	process := exec.CommandContext(ctx, "/usr/bin/unshare", "--mount", "--propagation", "slave", executable, "-test.run=^TestWorkspaceMountPropagationPeer$", "-test.v=false")
	process.ExtraFiles = []*os.File{readyWrite, commandRead, resultWrite}
	process.Env = []string{"PATH=/usr/bin:/bin", "SYNTHETIC_WORKSPACE_ROOT=" + root}
	process.Stderr = os.Stderr
	if err = process.Start(); err != nil {
		return nil, err
	}
	joined := false
	defer func() {
		if !joined {
			_ = process.Process.Kill()
			_ = process.Wait()
		}
	}()
	_ = readyWrite.Close()
	_ = commandRead.Close()
	_ = resultWrite.Close()
	ready := bufio.NewReader(readyRead)
	line, err := ready.ReadString('\n')
	if err != nil || line != "ready\n" {
		return nil, errors.Join(err, errors.New("peer barrier failed"))
	}
	if err = syscall.Mount("synthetic-evidence", run, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "size=1048576,mode=0700"); err != nil {
		return nil, err
	}
	runMounted = true
	payload := []byte("synthetic-visible-evidence")
	if err = os.WriteFile(filepath.Join(run, "marker"), payload, 0600); err != nil {
		return nil, err
	}
	_, err = commandWrite.Write([]byte("workspacePropagationObserve\n"))
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(resultRead)
	var before workspacePropagationObservation
	if err = decoder.Decode(&before); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(payload)
	var markerStat syscall.Stat_t
	if err = syscall.Stat(filepath.Join(run, "marker"), &markerStat); err != nil {
		return nil, err
	}
	if before.Visible != shared || before.MountPresent != shared || before.Shared || before.Slave != shared || !before.RootPrivate || shared && before.Sha256 != hex.EncodeToString(digest[:]) {
		return nil, fmt.Errorf("unexpected propagated view shared=%v: %+v", shared, before)
	}
	if shared && (before.DeviceId != uint64(markerStat.Dev) || before.Inode != markerStat.Ino) {
		return nil, errors.New("peer did not read the exact mounted inode")
	}
	if err = syscall.Unmount(run, 0); err != nil {
		return nil, err
	}
	runMounted = false
	_, err = commandWrite.Write([]byte("workspacePropagationObserve\n"))
	if err != nil {
		return nil, err
	}
	var after workspacePropagationObservation
	if err = decoder.Decode(&after); err != nil {
		return nil, err
	}
	if after.Visible || after.MountPresent || !after.RootPrivate {
		return nil, fmt.Errorf("unmount not propagated: %+v", after)
	}
	if err = commandWrite.Close(); err != nil {
		return nil, err
	}
	if err = process.Wait(); err != nil {
		return nil, err
	}
	joined = true
	_, hostShared, hostSlave := workspacePropagationMountFlags(root)
	if hostShared || hostSlave {
		return nil, errors.New("candidate root lost private propagation")
	}
	result = map[string]any{"shared_job_subtree": shared, "during": before, "after_normal_unmount": after, "peer_joined": true, "candidate_root_stayed_private": true, "test_directory": root}
	return result, nil
}

// Only the parent native test launches this exact pipe-framed peer role.
func TestWorkspaceMountPropagationPeer(t *testing.T) {
	root := os.Getenv("SYNTHETIC_WORKSPACE_ROOT")
	if root == "" {
		t.Skip("native namespace peer helper only")
	}
	if os.Geteuid() != 0 {
		t.Fatal("native namespace helper requires root")
	}
	workspacePropagationChild(root)
}

// A real kernel counterfactual demonstrates the missing visibility without any
// production mount changes. Explicit barriers, not sleeps, order observations.
func TestWorkspaceMountPropagationNative(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("native mount propagation proof requires root")
	}
	for _, shared := range []bool{false, true} {
		result, err := workspacePropagationTrial(shared)
		if err != nil {
			t.Fatalf("shared=%v: %v", shared, err)
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("isolated native result: %s", encoded)
	}
}
