// Synthetic identities verify one producer/durable-owner key contract without
// depending on invocation payloads, prefixes, or accidental id encodings.
package task

import (
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

func TestTaskQueueOwnershipUsesCompleteStoredRunOnceIdentity(t *testing.T) {
	identity := server.NewId()
	runOnce := RunOnce("synthetic_queue_owner", identity, strings.Repeat("tail", 1024))
	stored := runOnce.String()
	producerKey := RunOnceOwnershipKey(runOnce)
	if producerKey != PendingTaskOwnershipKey(server.NewId(), &stored) ||
		producerKey != server.NewPgOwnershipKeyFromString("pending_task/run_once", stored) {
		t.Fatal("producer and durable owner disagree on exact run-once identity")
	}
	changed := stored[:len(stored)-1] + " ]"
	if producerKey == PendingTaskOwnershipKey(identity, &changed) {
		t.Fatal("stored key bytes were normalized or truncated")
	}
	other := RunOnce("synthetic_queue_owner", identity, strings.Repeat("tail", 1024)+"other")
	if producerKey == RunOnceOwnershipKey(other) {
		t.Fatal("complete trailing identity did not participate in ownership")
	}
}

func TestTaskQueueOwnershipSeparatesTaskAndRunOnceKinds(t *testing.T) {
	id := server.NewId()
	stored := id.String()
	key := PendingTaskOwnershipKey(id, nil)
	if key != server.NewPgOwnershipKey("pending_task/task_id", id) ||
		key == PendingTaskOwnershipKey(id, &stored) ||
		key == PendingTaskOwnershipKey(server.NewId(), nil) ||
		key != taskQueueOwnershipKey(id, "") {
		t.Fatal("non-deduplicated task ownership lost its exact identity or kind")
	}
}

func TestTaskQueueOwnershipRejectsMissingIdentity(t *testing.T) {
	empty := ""
	checks := []func(){
		func() { RunOnceOwnershipKey(nil) },
		func() { PendingTaskOwnershipKey(server.Id{}, nil) },
		func() { PendingTaskOwnershipKey(server.NewId(), &empty) },
	}
	for index, check := range checks {
		panicked := false
		func() {
			defer func() { panicked = recover() != nil }()
			check()
		}()
		if !panicked {
			t.Fatalf("missing identity %d was accepted", index)
		}
	}
}
