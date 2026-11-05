//go:build linux

// Explicit barriers reproduce two already-admitted retirement RPCs. Only fake
// kernel operations and real private journals are used; no native host is touched.
package competitionagent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestBuildRetirementDuplicateObserversCannotDeadlockItsAdmissionJoin(t *testing.T) {
	fixture, kernel, _ := newBuildRetirementProxyTest(t)
	acknowledgeBuildRetirementTest(t, fixture)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	issued, releaseIssue := make(chan struct{}, 2), make(chan struct{})
	barrier, releaseBarrier := make(chan struct{}), make(chan struct{})
	fixture.proxy.beforeIssue = func() { issued <- struct{}{}; <-releaseIssue }
	fixture.proxy.beforeHandoffWait = func() { close(barrier); <-releaseBarrier }
	body, err := json.Marshal(HostFilesRequest{Schema: Schema, RunId: fixture.plan.RunId, Operation: "retire-build"})
	if err != nil {
		t.Fatal(err)
	}
	responses := [2]*httptest.ResponseRecorder{httptest.NewRecorder(), httptest.NewRecorder()}
	joined := make(chan struct{})
	var workers sync.WaitGroup
	var issueOnce, barrierOnce sync.Once
	t.Cleanup(func() {
		issueOnce.Do(func() { close(releaseIssue) })
		barrierOnce.Do(func() { close(releaseBarrier) })
		_ = fixture.proxy.Close()
		workers.Wait()
		<-joined
	})
	for _, response := range responses {
		workers.Add(1)
		go func() {
			defer workers.Done()
			fixture.proxy.workerHandler(response, httptest.NewRequest(http.MethodPost, "http://worker/_urnetwork/host-files", bytes.NewReader(body)).WithContext(ctx))
		}()
	}
	go func() { workers.Wait(); close(joined) }()
	for range responses {
		select {
		case <-issued:
		case <-ctx.Done():
			t.Fatal("both retirement requests did not commit admission before the join")
		}
	}
	issueOnce.Do(func() { close(releaseIssue) })
	select {
	case <-barrier:
	case <-ctx.Done():
		t.Fatal("initiating retirement did not reach its handoff boundary")
	}
	for {
		fixture.proxy.stateLock.Lock()
		waiters, inflight, changed := fixture.proxy.retirementWaiters, fixture.proxy.inflight, fixture.proxy.changed
		fixture.proxy.stateLock.Unlock()
		if waiters == 1 {
			if inflight != 2 {
				t.Fatal("duplicate observer disappeared from final owner quiescence", inflight)
			}
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("duplicate retirement did not become an explicit observer")
		}
	}
	barrierOnce.Do(func() { close(releaseBarrier) })
	select {
	case <-joined:
	case <-ctx.Done():
		t.Fatal("duplicate held its admission while the first waited for it")
	}
	var expected *BuildRetirementSummary
	for _, response := range responses {
		result := HostFilesResult{}
		if response.Code != http.StatusOK || decodeStrict(response.Body.Bytes(), &result) != nil || !result.Acknowledged || result.ExitCode != 0 || result.BuildRetirement == nil {
			t.Fatal("joined duplicate did not receive a durable known receipt", response.Code, response.Body.String())
		}
		if expected != nil && *expected != *result.BuildRetirement {
			t.Fatal("duplicate retirement changed its single native receipt")
		}
		expected = result.BuildRetirement
	}
	freezes := 0
	for _, event := range kernel.events.snapshot() {
		if event == "freeze" {
			freezes++
		}
	}
	if snapshot := fixture.proxy.Snapshot(); snapshot.Inflight != 0 || snapshot.UnknownOperations != 0 || freezes != 1 {
		t.Fatal("observer join lost requests or repeated native retirement", snapshot, freezes)
	}
}
