package grafana

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// The inventory is the module's source, not whatever lies in its checkout. A
// fixture module holds one tracked metric and one untracked, unignored new
// file, beside an ignored scratch file, a nested repository, a linked worktree
// on a branch with its own metric (an agent worktree under .claude), a test
// file, an excluded acceptance package and a tracked file deleted from the
// working tree. Only the first two are inventoried. A directory walk read the
// scratch, nested and worktree metrics too, so the coverage test failed in a
// checkout carrying another branch's worktree.
func TestApplicationMetricInventoryReadsOnlyModuleSource(t *testing.T) {
	root := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", dir, "-c", "commit.gpgsign=false"}, args...)...)
		command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	write := func(path string, metric string) {
		t.Helper()
		path = filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		body := "package fixture\n\nimport \"github.com/prometheus/client_golang/prometheus\"\n\n" +
			"var _ = prometheus.NewCounter(prometheus.CounterOpts{Name: \"" + metric + "\"})\n"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("temp/*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	write("tracked.go", "urnetwork_fixture_tracked_total")
	write("deleted.go", "urnetwork_fixture_deleted_total")
	write("proxy/acceptance/acceptance.go", "urnetwork_fixture_acceptance_total")
	git(root, "add", ".")
	git(root, "commit", "-q", "-m", "fixture")
	git(root, "branch", "agent")
	git(root, "worktree", "add", "-q", ".claude/worktrees/agent", "agent")
	write(".claude/worktrees/agent/agent_only.go", "urnetwork_fixture_worktree_only_total")
	git(filepath.Join(root, ".claude", "worktrees", "agent"), "add", "agent_only.go")
	git(filepath.Join(root, ".claude", "worktrees", "agent"), "commit", "-q", "-m", "agent")
	if err := os.Remove(filepath.Join(root, "deleted.go")); err != nil {
		t.Fatal(err)
	}
	write("untracked.go", "urnetwork_fixture_untracked_total")
	write("temp/scratch.go", "urnetwork_fixture_ignored_total")
	write("tracked_test.go", "urnetwork_fixture_test_only_total")
	git(root, "init", "-q", "nested")
	write("nested/nested.go", "urnetwork_fixture_nested_total")

	metrics, err := inventoryApplicationMetrics(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"urnetwork_fixture_tracked_total", "urnetwork_fixture_untracked_total"}; !slices.Equal(metrics, want) {
		t.Fatalf("inventory = %v, want %v", metrics, want)
	}
}
