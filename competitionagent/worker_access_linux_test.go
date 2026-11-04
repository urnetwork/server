// Traversal regression tests exercise fail-closed admission under the exact
// worker credentials, without changing real users, ACLs or mounted paths.
package competitionagent

import (
	"context"
	"errors"
	"syscall"
	"testing"
)

// An inaccessible mounted/home ancestor must stop admission immediately.
func TestWorkerRuntimeAccessRejectsMissingTraversePermission(t *testing.T) {
	config := &Config{WorkDirectory: "/synthetic-runtime/jobs", ConfigLocalDirectory: "/synthetic-fixtures/config/local", VaultLocalDirectory: "/synthetic-fixtures/vault/local"}
	credential := &syscall.Credential{Uid: 65534, Gid: 65534}
	calls := 0
	err := verifyWorkerRuntimeAccess(context.Background(), config, credential, func(_ context.Context, predicate, path string, actual *syscall.Credential) error {
		calls++
		if predicate != "-x" || path != config.WorkDirectory || actual != credential {
			t.Fatal("traversal probe changed its fixed path or worker identity")
		}
		return errors.New("synthetic missing search ACL")
	})
	if err == nil || calls != 1 {
		t.Fatal("inaccessible runtime admission was not rejected at its first probe")
	}
}

// Scoped search ACLs are accepted based on effective worker access, not global
// mode broadening; every direct local fixture and trusted command is checked.
func TestWorkerRuntimeAccessChecksFixtureAndExecutablePermissions(t *testing.T) {
	config := &Config{WorkDirectory: "/synthetic-runtime/jobs", ConfigLocalDirectory: "/synthetic-fixtures/config/local", VaultLocalDirectory: "/synthetic-fixtures/vault/local",
		Evaluator: Pin{Path: "/synthetic-release/evaluator"}, SelfCheck: Pin{Path: "/synthetic-release/self-check"}}
	credential := &syscall.Credential{Uid: 65534, Gid: 65534}
	paths := map[string]int{}
	err := verifyWorkerRuntimeAccess(context.Background(), config, credential, func(_ context.Context, predicate, path string, actual *syscall.Credential) error {
		if actual != credential || predicate != "-x" && predicate != "-r" {
			t.Fatal("runtime probe lost the non-root credential or fixed operation")
		}
		paths[path]++
		return nil
	})
	if err != nil || paths[config.WorkDirectory] != 1 || paths[config.ConfigLocalDirectory] != 2 || paths[config.VaultLocalDirectory] != 2 || paths[config.Evaluator.Path] != 2 || paths[config.SelfCheck.Path] != 1 {
		t.Fatalf("required worker access was not completely checked: paths=%v err=%v", paths, err)
	}
}
