package nginx_test

// The parity harness of LBALT.md §8 step 2: the same requests go to the
// pinned lb image's nginx and to the lb_alt front, both generated from one
// services fixture, and what clients and upstreams see is diffed. A difference
// passes only when the accepted list (parity_compare_test.go) names it, each
// entry citing where it was accepted: LBALT-IMPL.md §4 or §7, a unit's parity
// decisions, or a proposal of this harness's own.
//
// The fixture is testdata/parity: nginx.conf, the lb config warpctl's
// NewNginxConfig writes for edge-a.example's eth1 block, and route.json, the
// route file renderLbAltRouteTable writes for a connect generation on that
// host, both from the warp repo's warpctl/testdata/lbalt_parity_services.yml.
// Regenerate both from the warp checkout:
//
//	LBALT_PARITY_FIXTURE_OUT=<server>/nginx/testdata/parity go test -count=1 -run '^TestLbAltParityFixture$' ./warpctl
//
// Running it (opt-in; it needs Docker and the pinned image, and takes about
// a minute after the build):
//
//	export GOFLAGS=-trimpath GOCACHE=<the shared cache>
//	NGINX_PARITY=1 go test -count=1 -v -timeout 30m -run '^TestNginxParity$' ./nginx
//
// TestNginxParity, on the host, cross-compiles this package's tests for
// linux/arm64 and runs them in one container of the pinned lb image, with
// NET_ADMIN only and no network. In there, TestNginxParityContainer
// (parity_container_test.go):
//   - adds the fixture's interface, upstream and client addresses to the
//     loopback device;
//   - generates a certificate at every path the nginx config names;
//   - runs recording upstreams on the upstream address at every block port;
//   - starts nginx through the image's own lb binary, which rewrites its
//     listens to the interface address and the internal ports, as in
//     production;
//   - starts the front with nginx.NewFront, behind http.Servers set up as
//     connect's newHttpServer sets them up (connect/front_http.go), since
//     Front.ConfigureServer (LBALT-IMPL.md §7) does not exist yet;
//   - sends the corpus (parity_corpus_test.go) to both, and compares.
//
// Time is scaled down nowhere: the fixture's own route override (20 s) is the
// only timeout shortened, and nginx reads it from the same services config.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The parity harness's tunables.
type parityHarnessSettings struct {
	// the pinned lb image, linux/arm64, whose nginx is the reference
	Image string
	// the least free space, in bytes, the host's build cache filesystem must
	// have before the harness cross-compiles: the contract's 25 GB floor
	MinHostFreeBytes uint64
	// the least free space, in bytes, the Docker VM's root must have before
	// the harness starts its container
	MinDockerFreeBytes uint64
	// the longest the container's test run may take; its own go test timeout
	// is set a little under it
	ContainerTimeout time.Duration
	// how long nginx and the front have to accept their first connection
	StartTimeout time.Duration
	// how often the harness checks whether they accept
	StartPollInterval time.Duration
	// the longest one exchange may take, outside the timeout cases' own waits
	ExchangeTimeout time.Duration
	// how long one side of a barrier waits for the other before the case
	// records that it never came
	BarrierTimeout time.Duration
	// how long nginx has to exit after the harness asks it to
	StopTimeout time.Duration
	// the most bytes of nginx's output kept for a failure report
	NginxOutputBytes int
	// the pause between the pieces of an upload that keeps progressing:
	// under nginx's client_body_timeout of 15 s, while the upload as a
	// whole outlasts it
	ProgressingUploadPause time.Duration
	// a stall past the server-wide client_body_timeout (15 s) and within the
	// fixture's artifact route override (20 s)
	StallWithinOverride time.Duration
	// a stall past the artifact route's override (20 s)
	StallPastOverride time.Duration
	// how long a client that sent part of its head waits for the server,
	// past nginx's client_header_timeout of 15 s
	HeaderStall time.Duration
	// the most body bytes a recording upstream keeps of one attempt
	UpstreamMaxBodyBytes int64
	// how long before the run a generated certificate becomes valid, so a
	// clock skew between the harness and nginx can't make it not yet valid
	CertificateBackdate time.Duration
	// how long a generated certificate stays valid; the front refuses an
	// expired one at start
	CertificateLifetime time.Duration
}

// A new instance on every call.
func defaultParityHarnessSettings() *parityHarnessSettings {
	return &parityHarnessSettings{
		Image:              "bringyour/main-lb:2026.11.16-outerwerld-1100770680",
		MinHostFreeBytes:   25 * 1000 * 1000 * 1000,
		MinDockerFreeBytes: 4 * 1024 * 1024 * 1024,
		ContainerTimeout:   20 * time.Minute,
		StartTimeout:       60 * time.Second,
		StartPollInterval:  100 * time.Millisecond,
		ExchangeTimeout:    30 * time.Second,
		BarrierTimeout:     10 * time.Second,
		StopTimeout:        10 * time.Second,
		NginxOutputBytes:   256 * 1024,

		ProgressingUploadPause: 6 * time.Second,
		StallWithinOverride:    17 * time.Second,
		StallPastOverride:      25 * time.Second,
		HeaderStall:            20 * time.Second,

		UpstreamMaxBodyBytes: 32 * 1024 * 1024,
		CertificateBackdate:  1 * time.Hour,
		CertificateLifetime:  24 * time.Hour,
	}
}

// The env vars that opt in: on the host, and inside the container.
const (
	parityHostEnvName      = "NGINX_PARITY"
	parityContainerEnvName = "NGINX_PARITY_CONTAINER"
)

// Where the container finds the harness's files.
const (
	parityContainerBinDir     = "/parity/bin"
	parityContainerFixtureDir = "/parity/fixture"
	parityContainerNginxDir   = "/srv/warp/nginx.conf"
	parityContainerBinaryName = "nginx-parity.test"
)

// The line buffer the container's output is read with, and the longest line
// it takes, in bytes: a reported value is at most a few KiB.
const (
	parityOutputLineBytes    = 64 * 1024
	parityOutputMaxLineBytes = 1024 * 1024
)

// The fixture files, in testdata.
const (
	parityFixtureDir       = "testdata/parity"
	parityNginxConfigName  = "nginx.conf"
	parityRouteFileName    = "route.json"
	parityContainerTestRun = "^TestNginxParityContainer$"
)

// Builds this package's tests for the image's platform and runs
// TestNginxParityContainer in one container of the pinned lb image, failing
// when it fails. Opt-in: it runs only with NGINX_PARITY=1, Docker and the
// image, and otherwise skips with the reason.
func TestNginxParity(t *testing.T) {
	if os.Getenv(parityHostEnvName) != "1" {
		t.Skipf("set %s=1 to run the nginx parity harness (it needs Docker and the pinned lb image)", parityHostEnvName)
	}
	settings := defaultParityHarnessSettings()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker is not on the path: %v", err)
	}
	if output, err := exec.Command("docker", "image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", settings.Image).CombinedOutput(); err != nil {
		t.Skipf("the pinned lb image %s is not present (the harness never pulls): %s", settings.Image, strings.TrimSpace(string(output)))
	} else if platform := strings.TrimSpace(string(output)); platform != "linux/arm64" {
		t.Skipf("the pinned lb image is %s, and the harness builds for linux/arm64", platform)
	}

	// the host's build cache, and the Docker VM's root
	cacheDir := os.Getenv("GOCACHE")
	if cacheDir == "" {
		cacheDir = os.TempDir()
	}
	var statfs syscall.Statfs_t
	if err := syscall.Statfs(cacheDir, &statfs); err != nil {
		t.Fatalf("free space of %s: %v", cacheDir, err)
	}
	if hostFreeBytes := statfs.Bavail * uint64(statfs.Bsize); hostFreeBytes < settings.MinHostFreeBytes {
		t.Fatalf("%s has %d bytes free, under the %d the harness needs to build; stopping", cacheDir, hostFreeBytes, settings.MinHostFreeBytes)
	}
	dfOutput, err := exec.Command(
		"docker", "run", "--rm", "--network", "none", "--entrypoint", "df",
		settings.Image, "-P", "-B1", "/",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("the Docker VM's free space: %v: %s", err, dfOutput)
	}
	dockerFreeBytes, err := parseParityDfAvailable(string(dfOutput))
	if err != nil {
		t.Fatal(err)
	}
	if dockerFreeBytes < settings.MinDockerFreeBytes {
		t.Fatalf("the Docker VM has %d bytes free, under the %d the harness needs; stopping", dockerFreeBytes, settings.MinDockerFreeBytes)
	}

	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	fixtureDir := filepath.Join(workDir, "fixture")
	nginxDir := filepath.Join(workDir, "nginx")
	for _, dir := range []string{binDir, fixtureDir, nginxDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}

	build := exec.Command("go", "test", "-c", "-o", filepath.Join(binDir, parityContainerBinaryName), ".")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the container's test binary: %v: %s", err, output)
	}

	routeFile, err := os.ReadFile(filepath.Join(parityFixtureDir, parityRouteFileName))
	if err != nil {
		t.Fatal(err)
	}
	nginxConfig, err := os.ReadFile(filepath.Join(parityFixtureDir, parityNginxConfigName))
	if err != nil {
		t.Fatal(err)
	}
	// the route file names the lb block, whose config the lb binary loads,
	// and the upstream host nginx resolves
	var routeHeader struct {
		Upstream struct {
			Host string `json:"host"`
			Ipv4 string `json:"ipv4"`
		} `json:"upstream"`
		Interfaces []struct {
			Block string `json:"block"`
		} `json:"interfaces"`
	}
	if err := json.Unmarshal(routeFile, &routeHeader); err != nil {
		t.Fatal(err)
	}
	if len(routeHeader.Interfaces) != 1 {
		t.Fatalf("the fixture has %d interfaces, and the harness compares one", len(routeHeader.Interfaces))
	}
	if err := os.WriteFile(filepath.Join(fixtureDir, parityRouteFileName), routeFile, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixtureDir, parityNginxConfigName), nginxConfig, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nginxDir, routeHeader.Interfaces[0].Block+".conf"), nginxConfig, 0644); err != nil {
		t.Fatal(err)
	}

	containerName := fmt.Sprintf("lbalt-parity-%d-%016x", os.Getpid(), rand.Uint64())
	defer func() {
		// --rm removes it when it exits; this covers a run cut short
		exec.Command("docker", "rm", "--force", containerName).Run()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), settings.ContainerTimeout)
	defer cancel()
	containerTestTimeout := settings.ContainerTimeout - settings.StartTimeout
	run := exec.CommandContext(
		ctx,
		"docker", "run", "--rm", "--init",
		"--name", containerName,
		"--network", "none",
		"--cap-add", "NET_ADMIN",
		"--sysctl", "net.ipv6.conf.all.disable_ipv6=0",
		"--add-host", routeHeader.Upstream.Host+":"+routeHeader.Upstream.Ipv4,
		"--env", parityContainerEnvName+"=1",
		"--env", "WARP_ENV=test",
		"--volume", binDir+":"+parityContainerBinDir+":ro",
		"--volume", fixtureDir+":"+parityContainerFixtureDir+":ro",
		"--volume", nginxDir+":"+parityContainerNginxDir,
		settings.Image,
		parityContainerBinDir+"/"+parityContainerBinaryName,
		"-test.run", parityContainerTestRun,
		"-test.count", "1",
		"-test.v",
		"-test.timeout", containerTestTimeout.String(),
	)
	// the docker cli's own exit is not waited on past the deadline
	run.WaitDelay = settings.StopTimeout
	output, err := run.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	run.Stderr = run.Stdout
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, parityOutputLineBytes), parityOutputMaxLineBytes)
	for scanner.Scan() {
		t.Log(scanner.Text())
	}
	if err := run.Wait(); err != nil {
		t.Fatalf("the container's parity run failed: %v", err)
	}
}

// The available bytes of `df -P -B1 /`'s one filesystem line.
func parseParityDfAvailable(dfOutput string) (uint64, error) {
	lines := strings.Split(strings.TrimSpace(dfOutput), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("df printed no filesystem: %q", dfOutput)
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		return 0, fmt.Errorf("df printed an unexpected line: %q", lines[len(lines)-1])
	}
	return strconv.ParseUint(fields[3], 10, 64)
}

// Whether the harness runs inside its container; the in-container test skips
// with the reason otherwise.
func parityInContainer(t *testing.T) {
	t.Helper()
	if os.Getenv(parityContainerEnvName) != "1" {
		t.Skipf("runs only inside the parity harness's container; see TestNginxParity (%s=1)", parityHostEnvName)
	}
	if runtime.GOOS != "linux" {
		t.Skipf("the parity container is linux, not %s", runtime.GOOS)
	}
}
