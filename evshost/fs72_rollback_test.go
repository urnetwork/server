//go:build sidecar_localstack && sidecar_testhooks

// FS-72's M10 cases on T3 (EVS2-M10-30; §22.10's "a migration of a format-1 repo set and a rollback to format-1
// binaries"): the migration and rollback cases sidecartest/fault registers, and the migration killed at each step of its
// push, on T3's Nets, where sidecart3 names this package's block as each world's instances, so every push and read
// goes through server/cli/evs's handler on the private Redis and the local stack's MinIO.
//
// A rollback is a configuration of a real binary (the owner's answer to evs2-m10-30-format1-rollback-binary, QD-854):
// evs's and taskworker's blocks take EvsSettings.InstanceFormat from their block config, evs.yml's instance_format
// (block_config.go), 2 from M10's release and 1 in a rollback's, so format 1's guard for format-2 data
// (evsservice/format1_guard.go) is reachable in the build a rollback deploys. Every evs instance of a T3 world gets
// its format that way: sidecart3's evs tier stands evs.yml in with the format the world starts the instance at and
// evshost.NewHost reads it. TestFs72RollbackThroughTheBlockConfigOnT3 runs the rollback case with taskworker's
// evacuation runs taking their settings through taskworker's own loader as well, and checks that both blocks ran as
// format 1 and then as format 2 from their config.
//
// TestFs72RollbackOnThePinnedBuildOnT3 is the run on real binaries. sidecar/testdata/evs/format1_build.txt pins the
// release commit a rollback deploys, the first with the config field, with every sibling server's go.mod replaces. The
// test puts each repo's pinned commit in one directory with git archive and, offline from the module cache, builds:
//
//   - server's ./cli/evs and ./cli/taskworker, the binaries a rollback deploys. The evs binary is then started under a
//     block config naming format 1, and says it runs as a rollback's block, and under one naming a format it does not
//     know, and says it is not ready for that;
//   - the harness, the pinned tree's own evshost tests, which host that tree's block with T3's seams: a real evs block
//     stays not ready by its own design until ACL-M2-14's authorization state and its gate views land (host.go's
//     header), so the rollback's schedule runs on the pinned block through the harness, not through the process. It
//     runs the pinned TestFs72RollbackThroughTheBlockConfigOnT3 on the private test stack.
//
// So the rollback clause runs on code built from the pinned release commit with instance format 1 set through the
// block config, refusing format-2 pushes, purging nothing, writing nothing to a format-2 repo and leaving staged
// batches to a format-2 evacuator. What it does not run: the taskworker process itself, whose evs jobs the world
// stands in for with the evacuation taskworker's settings give (taskworker's own tests hold each job to those
// settings).
//
// The schedules hold instances at barriers, so this needs sidecar_testhooks; the stack must be up
// (sidecar/local/stack.sh), and these fail rather than skip without it.

package evshost_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server/evshost"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/sidecart3"
	"github.com/urnetwork/server/taskworker"
)

func TestFs72MigrationOnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-72", "migration", nil)
	})
}

func TestFs72RollbackOnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-72", "rollback", nil)
	})
}

// FS-72's rollback with both blocks' instance formats taken from their config (QD-854): each evs instance is
// evshost's block started under an evs.yml naming the format the schedule rolls it to, as on every T3 Net, and each
// of taskworker's evacuation runs takes the settings taskworker's own loader reads under one. The case rolls every
// block back to format 1 and forward again, so each block must have read both formats from its config. The harness
// TestFs72RollbackOnThePinnedBuildOnT3 builds from the pinned trees runs this test.
func TestFs72RollbackThroughTheBlockConfigOnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		var stateLock sync.Mutex
		blockFormatCounts := map[string]map[uint32]int{}
		settings := sidecart3.DefaultSettings()
		if fault.DefaultCatalog().Case("FS-72", "rollback").StoreClock {
			settings.StoreClock = sidecart3.DefaultStoreClockSettings()
		}
		settings.EvsTier = &sidecart3.EvsTierSettings{
			LoadJobSettings: taskworker.LoadEvsJobSettings,
			BlockConfigRead: func(block string, instanceFormat uint32) {
				stateLock.Lock()
				defer stateLock.Unlock()
				formatCounts := blockFormatCounts[block]
				if formatCounts == nil {
					formatCounts = map[uint32]int{}
					blockFormatCounts[block] = formatCounts
				}
				formatCounts[instanceFormat] += 1
			},
		}
		// a merge's seeds: the row's nightly seeds are TestFs72RollbackOnT3's, whose evs instances take their formats
		// the same way
		ruleSettings := sidecart3.RuleSettings(tb, settings)
		ruleSettings.Seeds = fault.MergeSeeds()
		sidecart3.RunRow(tb, "FS-72", "rollback", ruleSettings)
		stateLock.Lock()
		defer stateLock.Unlock()
		for _, block := range []string{sidecart3.EvsTierBlockEvs, sidecart3.EvsTierBlockTaskworker} {
			for _, instanceFormat := range []uint32{sidecar.EvsRepoFormat1, sidecar.EvsRepoFormat2} {
				if blockFormatCounts[block][instanceFormat] == 0 {
					tb.Errorf("%s never ran as instance format %d from its block config", block, instanceFormat)
				}
			}
			tb.Logf("FS-72's rollback through %s: %s took instance format 1 from it %d times and format 2 %d times",
				evshost.BlockConfigFileName, block, blockFormatCounts[block][sidecar.EvsRepoFormat1], blockFormatCounts[block][sidecar.EvsRepoFormat2])
		}
	})
}

func TestFs72MigrationKilledAtStepSixOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-72", "migration killed after step 6", fault.Fs72MigrationKilledAt(evsservice.BarrierPushStep6After))
}

func TestFs72MigrationKilledBeforeTheRaiseOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-72", "migration killed before the raise", fault.Fs72MigrationKilledAt(evsservice.BarrierMigrateRaiseBefore))
}

func TestFs72MigrationKilledAfterTheRaiseOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-72", "migration killed after the raise", fault.Fs72MigrationKilledAt(evsservice.BarrierMigrateRaiseAfter))
}

func TestFs72MigrationKilledBeforeTheHeadCasOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-72", "migration killed before the head CAS", fault.Fs72MigrationKilledAt(evsservice.BarrierHeadCasBefore))
}

func TestFs72MigrationKilledAfterTheHeadCasOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-72", "migration killed after the head CAS", fault.Fs72MigrationKilledAt(evsservice.BarrierHeadCasAfter))
}

// format1_build.txt's path from this package, in the sibling sidecar checkout.
const fs72Format1BuildPath = "../../sidecar/testdata/evs/format1_build.txt"

// The pinned tree's test the harness runs.
const fs72PinnedHarnessTestName = "TestFs72RollbackThroughTheBlockConfigOnT3"

// One repo's pin in format1_build.txt.
type fs72Format1Pin struct {
	repo   string
	commit string
}

// format1_build.txt's pins, in its order: each line not a comment names a repo and a 40-hex commit.
func fs72Format1Pins(t *testing.T) []*fs72Format1Pin {
	t.Helper()
	file, err := os.Open(fs72Format1BuildPath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	pins := []*fs72Format1Pin{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || len(fields[1]) != 40 {
			t.Fatalf("format1_build.txt's line %q names no repo and commit", line)
		}
		pins = append(pins, &fs72Format1Pin{repo: fields[0], commit: fields[1]})
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return pins
}

// Each pin's commit put in dir/<repo> by git archive from the sibling checkout, server's connect/sim-latency left
// out: it is most of server's bytes, and nothing the binaries or the harness link is in it.
func fs72ArchivePins(t *testing.T, pins []*fs72Format1Pin, dir string) {
	t.Helper()
	for _, pin := range pins {
		into := filepath.Join(dir, pin.repo)
		if err := os.MkdirAll(into, 0o755); err != nil {
			t.Fatal(err)
		}
		args := []string{"-C", filepath.Join("..", "..", pin.repo), "archive", "--format=tar", pin.commit}
		if pin.repo == "server" {
			args = append(args, "--", ".", ":(exclude)connect/sim-latency")
		}
		archive := exec.CommandContext(t.Context(), "git", args...)
		extract := exec.CommandContext(t.Context(), "tar", "-x", "-C", into)
		pipe, err := archive.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		extract.Stdin = pipe
		var archiveErr strings.Builder
		archive.Stderr = &archiveErr
		if err := archive.Start(); err != nil {
			t.Fatal(err)
		}
		extractOut, extractErr := extract.CombinedOutput()
		if err := archive.Wait(); err != nil {
			t.Fatalf("git archive of %s at %s: %v\n%s", pin.repo, pin.commit, err, archiveErr.String())
		}
		if extractErr != nil {
			t.Fatalf("extracting %s at %s: %v\n%s", pin.repo, pin.commit, extractErr, extractOut)
		}
	}
}

// The pinned trees in a directory of the test's, every repo server's go.mod replaces among them, or the build would
// read a newer tree.
func fs72PinnedTrees(t *testing.T) string {
	t.Helper()
	pins := fs72Format1Pins(t)
	pinned := map[string]bool{}
	for _, pin := range pins {
		pinned[pin.repo] = true
	}
	for _, repo := range []string{"sidecar", "server"} {
		if !pinned[repo] {
			t.Fatalf("format1_build.txt pins no %s commit", repo)
		}
	}
	dir := t.TempDir()
	fs72ArchivePins(t, pins, dir)
	goMod, err := os.ReadFile(filepath.Join(dir, "server", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(goMod), "\n") {
		_, target, ok := strings.Cut(line, "=> ../")
		if !ok || !strings.HasPrefix(strings.TrimSpace(line), "replace ") {
			continue
		}
		repo, _, _ := strings.Cut(strings.TrimSpace(target), "/")
		if !pinned[repo] {
			t.Errorf("server's go.mod at its pin replaces ../%s, which format1_build.txt does not pin", repo)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	return dir
}

// A go command in the pinned server tree, offline from the module cache, failing the test with its output.
func fs72PinnedGo(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()
	command := exec.CommandContext(t.Context(), "go", args...)
	command.Dir = filepath.Join(dir, "server")
	command.Env = append(append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "GOFLAGS=-mod=readonly -trimpath", "GOPROXY=off"), env...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("go %s in the pinned tree: %v\n%s", strings.Join(args, " "), err, output)
	}
}

// The pinned evs binary started under a block config holding file and read until its log has said each of wants, in
// order; check, if any, then gets the block's base url while it runs, and the block is killed. The words are the
// pinned build's own, so they change only when the pin moves. Real time bounds the waits only to fail them.
func fs72RunPinnedEvs(t *testing.T, binary string, file string, wants []string, check func(base string)) {
	t.Helper()
	configHome := filepath.Join(t.TempDir(), "config")
	if err := os.MkdirAll(configHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, evshost.BlockConfigFileName), []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	// a loopback port of the test's for the block's status port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	command := exec.CommandContext(ctx, binary, "--port", "80")
	command.Env = append(os.Environ(), "WARP_CONFIG_HOME="+configHome, "WARP_SERVICE=evs", "WARP_HOST_IPV4=127.0.0.1", fmt.Sprintf("WARP_PORTS=80:%d", port))
	stderr, err := command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	// it serves /status until it is told to stop, so it is stopped here, however the test leaves
	defer func() {
		command.Process.Kill()
		command.Wait()
	}()
	lines := []string{}
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for 0 < len(wants) && scanner.Scan() {
		lines = append(lines, scanner.Text())
		if strings.Contains(scanner.Text(), wants[0]) {
			wants = wants[1:]
		}
	}
	if 0 < len(wants) {
		t.Fatalf("the pinned evs under a block config %q never said %q:\n%s", file, wants[0], strings.Join(lines, "\n"))
	}
	if check == nil {
		return
	}
	// what it logs from here on is read and dropped, so it never waits on a full pipe
	go io.Copy(io.Discard, stderr)
	address := fmt.Sprintf("127.0.0.1:%d", port)
	for {
		// the bind is the operating system's, with no hook to wait on
		conn, err := net.Dial("tcp", address)
		if err == nil {
			conn.Close()
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the pinned evs's status port never bound: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
	check("http://" + address)
}

// GET /status's status over a block's port.
func fs72BlockStatus(t *testing.T, base string) string {
	t.Helper()
	response, err := http.Get(base + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("/status answered %d", response.StatusCode)
	}
	result := &router.WarpStatusResult{}
	if err := json.NewDecoder(response.Body).Decode(result); err != nil {
		t.Fatal(err)
	}
	return result.Status
}

// FS-72's rollback on real binaries built from the pinned release commit, the instance format set to 1 through the
// block config. See the file header.
func TestFs72RollbackOnThePinnedBuildOnT3(t *testing.T) {
	dir := fs72PinnedTrees(t)
	bin := filepath.Join(dir, "bin")
	// the binaries a rollback deploys, as a release builds them
	fs72PinnedGo(t, dir, []string{"CGO_ENABLED=0"}, "build", "-o", bin+string(filepath.Separator), "./cli/evs", "./cli/taskworker")
	for _, binary := range []string{"evs", "taskworker"} {
		info, err := os.Stat(filepath.Join(bin, binary))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o100 == 0 {
			t.Fatalf("no %s binary: %v", binary, err)
		}
	}
	// the harness from the same trees: the pinned evshost's own tests, with T3's seams
	harness := filepath.Join(bin, "evshost.test")
	fs72PinnedGo(t, dir, nil, "test", "-c", "-tags", "sidecar_localstack sidecar_testhooks", "-o", harness, "./evshost")

	// the evs process reads its block config, on the private test stack's env: a rollback's, and one it refuses to
	// start under
	fs72RunPinnedEvs(t, filepath.Join(bin, "evs"), "instance_format: 1\n", []string{
		fmt.Sprintf("[evs]%s sets instance format 1, a rollback's", evshost.BlockConfigFileName),
	}, nil)
	fs72RunPinnedEvs(t, filepath.Join(bin, "evs"), "instance_format: 3\n", []string{
		fmt.Sprintf("[evs]not ready: evs block config: %s sets instance_format 3", evshost.BlockConfigFileName),
		"[evs]serving status on port",
	}, func(base string) {
		if status := fs72BlockStatus(t, base); !strings.HasPrefix(status, "error not ready") {
			t.Fatalf("the pinned evs under an unknown instance format answers /status %q", status)
		}
	})

	// the rollback on the pinned block, its formats through the block config, in the harness's own test env on the
	// private test stack. The harness ends by itself before this test's own deadline, so a run this test's timeout
	// cuts off leaves no process behind.
	args := []string{"-test.run", "^" + fs72PinnedHarnessTestName + "$", "-test.count=1", "-test.v"}
	if deadline, ok := t.Deadline(); ok {
		args = append(args, "-test.timeout="+max(time.Until(deadline)-30*time.Second, time.Second).String())
	} else {
		args = append(args, "-test.timeout=0")
	}
	run := exec.CommandContext(t.Context(), harness, args...)
	run.Dir = filepath.Join(dir, "server", "evshost")
	outputPath := filepath.Join(dir, "harness.log")
	output, err := os.Create(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	run.Stdout = output
	run.Stderr = output
	runErr := run.Run()
	logged, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	// the harness's own lines, without the blocks' request logs
	said := []string{}
	for _, line := range strings.Split(string(logged), "\n") {
		if strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "=== ") || strings.HasPrefix(line, "    ") ||
			strings.HasPrefix(line, "PASS") || strings.HasPrefix(line, "FAIL") || strings.HasPrefix(line, "ok") || strings.HasPrefix(line, "panic") {
			said = append(said, line)
		}
	}
	if runErr != nil {
		tail := string(logged)
		if 16*1024 < len(tail) {
			tail = tail[len(tail)-16*1024:]
		}
		t.Fatalf("the pinned harness's %s: %v\n%s\n...\n%s", fs72PinnedHarnessTestName, runErr, strings.Join(said, "\n"), tail)
	}
	// a harness holding no such test passes having run nothing
	if !strings.Contains(string(logged), "--- PASS: "+fs72PinnedHarnessTestName) {
		t.Fatalf("the pinned harness ran no %s:\n%s", fs72PinnedHarnessTestName, strings.Join(said, "\n"))
	}
	t.Logf("the pinned harness:\n%s", strings.Join(said, "\n"))
}
