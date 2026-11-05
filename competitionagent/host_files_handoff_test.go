//go:build linux

// Handoff regressions force a pending daemon response and a concurrent file
// operation. Fake boundaries prove ordering without host mounts or sleeps.
package competitionagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
)

func TestHostFilesHandoffJoinsDaemonAcknowledgmentBeforeNativeCensus(t *testing.T) {
	fixture := newDockerProxyTest(t)
	info, err := os.Stat(fixture.plan.ContextRoot)
	if err != nil {
		t.Fatal(err)
	}
	plan := fixture.plan
	plan.HostFiles = &HostFilesPlan{WorkerGid: 12345, ContainerRootUid: 200000, ContainerRootGid: 300000,
		ContainerIdMapLength: 65536, EvidenceBytes: 1024 * 1024, WorkDeviceId: uint64(info.Sys().(*syscall.Stat_t).Dev)}
	broker, err := newHostFileBroker(plan, DefaultHostFilesSettings())
	if err != nil {
		t.Fatal(err)
	}
	fixture.proxy.hostFiles = broker
	var nativeCalls atomic.Int64
	broker.beforePath = func(context.Context, string) error {
		entries, err := os.ReadDir(fixture.proxy.operationDirectory())
		if err != nil {
			return err
		}
		for _, entry := range entries {
			content, err := readBounded(filepath.Join(fixture.proxy.operationDirectory(), entry.Name()), fixture.settings.MaximumDocumentBytes)
			var operation dockerProxyOperation
			if err != nil || decodeStrict(content, &operation) != nil {
				return errors.New("synthetic operation receipt unavailable")
			}
			if operation.Kind == "http" && operation.State != "acknowledged" {
				t.Error("native filesystem census preceded terminal daemon acknowledgment")
				return errors.New("synthetic late daemon operation")
			}
		}
		return nil
	}
	broker.mount = func(context.Context, cleanupPlan, uint32, uint32, int64) error { nativeCalls.Add(1); return nil }
	daemonEntered, daemonRelease := make(chan struct{}), make(chan struct{})
	fixture.daemon(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(daemonEntered)
		<-daemonRelease
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "synthetic completed create")
	})
	mutationDone := make(chan struct{})
	go func() {
		defer close(mutationDone)
		fixture.proxy.workerHandler(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "http://worker/containers/create", bytes.NewReader(fixture.create)))
	}()
	<-daemonEntered
	barrierEntered, barrierRelease := make(chan struct{}), make(chan struct{})
	fixture.proxy.beforeHandoffWait = func() { close(barrierEntered); <-barrierRelease }
	request, err := json.Marshal(HostFilesRequest{Schema: Schema, RunId: plan.RunId, Operation: "mount-evidence"})
	if err != nil {
		t.Fatal(err)
	}
	hostDone := make(chan struct{})
	response := httptest.NewRecorder()
	go func() {
		defer close(hostDone)
		fixture.proxy.workerHandler(response, httptest.NewRequest(http.MethodPost, "http://worker/_urnetwork/host-files", bytes.NewReader(request)))
	}()
	var daemonOnce, barrierOnce sync.Once
	t.Cleanup(func() {
		daemonOnce.Do(func() { close(daemonRelease) })
		barrierOnce.Do(func() { close(barrierRelease) })
		<-mutationDone
		<-hostDone
	})
	<-barrierEntered
	if release, err := fixture.proxy.admit(false); err == nil {
		release()
		t.Fatal("new mutation crossed an active filesystem handoff barrier")
	}
	if nativeCalls.Load() != 0 {
		t.Fatal("filesystem ownership changed while the admitted create was pending")
	}
	barrierOnce.Do(func() { close(barrierRelease) })
	daemonOnce.Do(func() { close(daemonRelease) })
	<-mutationDone
	<-hostDone
	var result HostFilesResult
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || !result.Acknowledged || result.ExitCode != 0 || nativeCalls.Load() != 1 {
		t.Fatal("joined definite daemon completion did not release exactly one native handoff", response.Code, result)
	}
	if release, err := fixture.proxy.admit(false); err != nil {
		t.Fatal("successful temporary handoff permanently closed the healthy owner", err)
	} else {
		release()
	}
}

func TestHostFilesHandoffStopDoesNotWaitForOrReopenTemporaryBarrier(t *testing.T) {
	fixture := newDockerProxyTest(t)
	releasePending, err := fixture.proxy.admit(false)
	if err != nil {
		t.Fatal(err)
	}
	releaseHost, err := fixture.proxy.admit(false)
	if err != nil {
		releasePending()
		t.Fatal(err)
	}
	entered, resume := make(chan struct{}), make(chan struct{})
	fixture.proxy.beforeHandoffWait = func() { close(entered); <-resume }
	done := make(chan struct{})
	var handoffErr error
	var pendingOnce, hostOnce, resumeOnce sync.Once
	go func() {
		defer close(done)
		release, err := fixture.proxy.beginHostFileHandoff(t.Context())
		if release != nil {
			release()
		}
		handoffErr = err
	}()
	t.Cleanup(func() {
		resumeOnce.Do(func() { close(resume) })
		pendingOnce.Do(releasePending)
		hostOnce.Do(releaseHost)
		<-done
	})
	<-entered
	stopCtx, stop := context.WithCancel(t.Context())
	stop()
	if err := fixture.proxy.Quiesce(stopCtx); !errors.Is(err, context.Canceled) {
		t.Fatal("stop cleanup waited for the file handoff barrier", err)
	}
	resumeOnce.Do(func() { close(resume) })
	<-done
	if handoffErr == nil {
		t.Fatal("a stopped owner allowed filesystem handoff")
	}
	pendingOnce.Do(releasePending)
	hostOnce.Do(releaseHost)
	if release, err := fixture.proxy.admit(false); err == nil {
		release()
		t.Fatal("retiring the temporary barrier reopened stopped daemon custody")
	}
	if err := fixture.proxy.Quiesce(t.Context()); err != nil {
		t.Fatal("joined unissued test admissions became unknown", err)
	}
}
