package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/evsservice"
)

// A monoroot of two repositories, sidecar and server, each one commit of one file.
func writeMonoroot(t *testing.T) string {
	t.Helper()
	monoroot := t.TempDir()
	for _, repo := range benchRepos {
		dir := filepath.Join(monoroot, repo)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "README"), []byte(repo+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{
			{"init", "-q"},
			{"add", "README"},
			{"-c", "user.name=bench", "-c", "user.email=bench@example.invalid", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "one"},
		} {
			command := exec.Command("git", args...)
			command.Dir = dir
			command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
			if out, err := command.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v: %s", args, err, out)
			}
		}
	}
	return monoroot
}

// go test as the fs116 mode runs it: a listing of two tests, then each one's events.
func fakeGoTest(failTest string) commandRunner {
	return func(ctx context.Context, dir string, name string, args ...string) ([]byte, error) {
		if !strings.HasSuffix(dir, "sidecar") {
			return nil, errors.New("not run in the sidecar checkout")
		}
		if len(args) < 2 || args[0] != "test" {
			return nil, errors.New("not go test")
		}
		if args[1] == "-list" {
			return []byte("TestFs116SteadyLoadAdmitsTenPushesAnInterval\nTestFs116AllowanceRampsBackAfterAStorm\nok  \tgithub.com/urnetwork/sidecar/sidecartest/fault\t0.1s\n"), nil
		}
		out := &bytes.Buffer{}
		encoder := json.NewEncoder(out)
		var err error
		for _, test := range []string{"TestFs116SteadyLoadAdmitsTenPushesAnInterval", "TestFs116AllowanceRampsBackAfterAStorm"} {
			action := "pass"
			if test == failTest {
				action = "fail"
				err = errors.New("exit status 1")
			}
			encoder.Encode(map[string]string{"Action": "run", "Test": test})
			encoder.Encode(map[string]string{"Action": action, "Test": test})
		}
		encoder.Encode(map[string]string{"Action": "pass"})
		return out.Bytes(), err
	}
}

func TestFs116ModeRecordsWhatPassedOnTheCommitSet(t *testing.T) {
	monoroot := writeMonoroot(t)
	now := func() time.Time { return time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC) }
	evidence, err := RunFs116(context.Background(), monoroot, "go", fakeGoTest(""), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Listed) != 2 || len(evidence.Passed) != 2 || len(evidence.CommitSet) != 2 {
		t.Fatalf("evidence %+v", evidence)
	}
	if err := evidence.Check(evidence.CommitSet); err != nil {
		t.Fatal(err)
	}

	// a failing test is refused, its record kept
	evidence, err = RunFs116(context.Background(), monoroot, "go", fakeGoTest("TestFs116AllowanceRampsBackAfterAStorm"), now)
	if err == nil || evidence == nil || evidence.Check(evidence.CommitSet) == nil {
		t.Fatalf("a failing FS-116 passed: %v", err)
	}

	// a tree with changes names no commit
	if err := os.WriteFile(filepath.Join(monoroot, "sidecar", "scratch"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RunFs116(context.Background(), monoroot, "go", fakeGoTest(""), now); err == nil {
		t.Fatal("ran FS-116 on a tree with changes")
	}
}

func TestRunsThatAreNotSmokesRefuseToStartWithoutFs116(t *testing.T) {
	monoroot := writeMonoroot(t)
	measurement := &Measurement{Settings: map[string]string{}}
	flags := &runFlags{monoroot: monoroot}
	var start *startError
	if err := flags.evidence(measurement); !errors.As(err, &start) {
		t.Fatalf("started without FS-116: %v", err)
	}
	// a record of another commit set is refused too
	other := fixtureFs116()
	path := filepath.Join(t.TempDir(), "fs116.json")
	data, _ := json.Marshal(other)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	flags.fs116Path = path
	if err := flags.evidence(measurement); !errors.As(err, &start) {
		t.Fatalf("started on another build's FS-116: %v", err)
	}
	// a smoke starts, noting what it lacks
	flags.smoke = true
	measurement = &Measurement{Settings: map[string]string{}}
	if err := flags.evidence(measurement); err != nil {
		t.Fatal(err)
	}
	if len(measurement.Notes) == 0 {
		t.Fatal("a smoke noted nothing")
	}
	// no monoroot at all: only a smoke
	if err := (&runFlags{}).evidence(&Measurement{}); !errors.As(err, &start) {
		t.Fatalf("started without naming a build: %v", err)
	}
}

func TestReportModeExitsByTheGate(t *testing.T) {
	dir := t.TempDir()
	inArgs := []string{}
	for i, measurement := range fixtureRuns() {
		data, err := measurement.Json()
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, string(rune('a'+i))+".json")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		inArgs = append(inArgs, "-in", path)
	}
	priceTablePath := filepath.Join(dir, "price-table.json")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	args := append([]string{"report"}, inArgs...)
	args = append(args, "-price_table", priceTablePath, "-json", filepath.Join(dir, "report.json"))
	if code := run(context.Background(), args, stdout, stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	data, err := os.ReadFile(priceTablePath)
	if err != nil {
		t.Fatal(err)
	}
	table := &evsservice.PriceTable{}
	if err := json.Unmarshal(data, table); err != nil || table.BlockByteCount != 1024 {
		t.Fatalf("price table %+v, %v", table, err)
	}
	if !strings.Contains(stdout.String(), "verdict pass") {
		t.Fatalf("report:\n%s", stdout.String())
	}

	// a failed gate: exit 1 and no price table
	failing := fixtureRuns()
	failing[1].Phase(MixPhaseTwiceYear).PushesRefusedByBudget = 1
	failing[3].Phase(MixPhaseTwiceYear).PushesRefusedByBudget = 1
	inArgs = []string{}
	for i, measurement := range failing {
		data, _ := measurement.Json()
		path := filepath.Join(dir, "f"+string(rune('a'+i))+".json")
		os.WriteFile(path, data, 0o644)
		inArgs = append(inArgs, "-in", path)
	}
	failedTablePath := filepath.Join(dir, "failed-table.json")
	args = append(append([]string{"report"}, inArgs...), "-price_table", failedTablePath)
	if code := run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}); code != 1 {
		t.Fatalf("a failed gate exited %d", code)
	}
	if _, err := os.Stat(failedTablePath); !os.IsNotExist(err) {
		t.Fatal("a failed gate wrote a price table")
	}

	for _, args := range [][]string{{}, {"nvme"}, {"report"}, {"report", "-in", filepath.Join(dir, "missing.json")}, {"ops", "-cluster", "meta-1"}} {
		if code := run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}); code != 2 {
			t.Fatalf("%v exited %d", args, code)
		}
	}
}
