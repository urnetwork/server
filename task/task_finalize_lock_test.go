package task

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A completion's transaction waits for a lock up to FinalizeLockTimeout, not
// the main pool's 2 s: a failed completion reruns its task and interrupts its
// batch siblings. Unset uses the 20 s default, inside FinalizeTimeout.
func TestTaskFinalizeLockTimeout(t *testing.T) {
	if DefaultTaskFinalizeLockTimeout <= server.PgLockTimeout || DefaultTaskFinalizeTimeout <= DefaultTaskFinalizeLockTimeout {
		t.Fatal("the completion lock bound is not between the pool's bound and the completion's own")
	}
	if got := DefaultTaskWorkerSettings().FinalizeLockTimeout; got != DefaultTaskFinalizeLockTimeout {
		t.Fatalf("default settings bound = %s", got)
	}
	worker := &TaskWorker{settings: &TaskWorkerSettings{}}
	if got := worker.finalizeLockTimeout(); got != server.TxLockTimeout(DefaultTaskFinalizeLockTimeout) {
		t.Fatalf("unset bound = %s, want the default", time.Duration(got))
	}
	worker.settings.FinalizeLockTimeout = 7 * time.Second
	if got := worker.finalizeLockTimeout(); got != server.TxLockTimeout(7*time.Second) {
		t.Fatalf("set bound = %s, want 7s", time.Duration(got))
	}
	// every completion transaction passes it: single, owned and batch
	for path, want := range map[string]int{"task_completion.go": 2, "task_completion_batch.go": 2} {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(string(source), "self.finalizeLockTimeout())"); got != want {
			t.Errorf("%s passes the completion lock bound %d times, want %d", path, got, want)
		}
	}
}
