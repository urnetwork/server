//go:build linux

// Host-file RPC ownership tests inject native work into a private real proxy
// journal. No privileged syscall, real daemon or production endpoint is used.
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
	"strings"
	"sync"
	"syscall"
	"testing"
)

func TestHostFilesRpcClosedShapeRejectsBeforeNativeIntent(t *testing.T) {
	fixture := newDockerProxyTest(t)
	info, err := os.Stat(fixture.plan.ContextRoot)
	if err != nil {
		t.Fatal(err)
	}
	plan := fixture.plan
	plan.HostFiles = &HostFilesPlan{WorkerGid: 12345, ContainerRootUid: 200000, ContainerRootGid: 300000, ContainerIdMapLength: 65536, EvidenceBytes: 1024 * 1024, WorkDeviceId: uint64(info.Sys().(*syscall.Stat_t).Dev)}
	broker, err := newHostFileBroker(plan, DefaultHostFilesSettings())
	if err != nil {
		t.Fatal(err)
	}
	fixture.proxy.hostFiles = broker
	calls := 0
	broker.mount = func(context.Context, cleanupPlan, uint32, uint32, int64) error { calls++; return nil }
	broker.beforePath = func(context.Context, string) error { return nil }
	valid, err := json.Marshal(HostFilesRequest{Schema: Schema, RunId: plan.RunId, Operation: "mount-evidence"})
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{strings.Replace(string(valid), `"schema":1`, `"schema":1,"schema":1`, 1),
		strings.Replace(string(valid), `"schema":1`, `"Schema":1`, 1), strings.TrimSuffix(string(valid), "}") + `,"command":"/bin/sh"}`,
		strings.Replace(string(valid), "mount-evidence", "arbitrary-command", 1)} {
		response := httptest.NewRecorder()
		fixture.proxy.workerHandler(response, httptest.NewRequest(http.MethodPost, "http://worker/_urnetwork/host-files", strings.NewReader(content)))
		if response.Code == http.StatusOK || calls != 0 {
			t.Fatal("expanded host RPC reached native work")
		}
	}
	entries, err := os.ReadDir(fixture.proxy.operationDirectory())
	if err != nil || len(entries) != 0 {
		t.Fatal("rejected host RPC manufactured issued intent", err)
	}
}

func TestHostFilesRpcRetainsIntentAndJoinsAfterClientDisconnect(t *testing.T) {
	fixture := newDockerProxyTest(t)
	info, err := os.Stat(fixture.plan.ContextRoot)
	if err != nil {
		t.Fatal(err)
	}
	plan := fixture.plan
	plan.HostFiles = &HostFilesPlan{WorkerGid: 12345, ContainerRootUid: 200000, ContainerRootGid: 300000, ContainerIdMapLength: 65536, EvidenceBytes: 1024 * 1024, WorkDeviceId: uint64(info.Sys().(*syscall.Stat_t).Dev)}
	broker, err := newHostFileBroker(plan, DefaultHostFilesSettings())
	if err != nil {
		t.Fatal(err)
	}
	fixture.proxy.hostFiles = broker
	entered, release := make(chan struct{}), make(chan struct{})
	broker.beforePath = func(context.Context, string) error { return nil }
	broker.mount = func(ctx context.Context, _ cleanupPlan, _ uint32, _ uint32, _ int64) error {
		close(entered)
		entries, err := os.ReadDir(fixture.proxy.operationDirectory())
		if err != nil || len(entries) != 1 {
			t.Error("native mount preceded synced intent", err)
			return errors.New("synthetic missing host intent")
		}
		content, err := readBounded(filepath.Join(fixture.proxy.operationDirectory(), entries[0].Name()), fixture.settings.MaximumDocumentBytes)
		operation := dockerProxyOperation{}
		if err != nil || decodeStrict(content, &operation) != nil || operation.State != "issued" || operation.Kind != "host-files" {
			t.Error("host intent lost its exact state", err)
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	body, err := json.Marshal(HostFilesRequest{Schema: Schema, RunId: plan.RunId, Operation: "mount-evidence"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	request := httptest.NewRequest(http.MethodPost, "http://worker/_urnetwork/host-files", bytes.NewReader(body)).WithContext(ctx)
	writer := &dockerProxyBrokenWriter{header: http.Header{}, entered: make(chan struct{})}
	done := make(chan struct{})
	go func() { defer close(done); fixture.proxy.workerHandler(writer, request) }()
	var releaseOnce sync.Once
	t.Cleanup(func() { cancel(); releaseOnce.Do(func() { close(release) }); <-done })
	<-entered
	cancel()
	stopCtx, stop := context.WithCancel(t.Context())
	stop()
	if err := fixture.proxy.Quiesce(stopCtx); !errors.Is(err, context.Canceled) || fixture.proxy.Snapshot().Inflight != 1 {
		t.Fatal("client disconnect released native host custody", err)
	}
	releaseOnce.Do(func() { close(release) })
	<-done
	if err := fixture.proxy.Quiesce(t.Context()); err != nil {
		t.Fatal("joined definite host response became unknown", err)
	}
}

func TestDockerProxySpoolDirectoryDurabilityPrecedesAcknowledgment(t *testing.T) {
	fixture := newDockerProxyTest(t)
	fixture.daemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "synthetic completed response")
	})
	fixture.proxy.syncSpoolDirectory = func(string) error { return errors.New("synthetic directory sync failure") }
	fixture.proxy.workerHandler(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "http://worker/containers/create", bytes.NewReader(fixture.create)))
	if fixture.proxy.Snapshot().UnknownOperations != 1 {
		t.Fatal("unsynced quota-filesystem response became durable acknowledgment")
	}
	if err := fixture.proxy.Quiesce(t.Context()); !errors.Is(err, ErrDockerProxyUnknown) {
		t.Fatal("lost response durability released custody", err)
	}
}
