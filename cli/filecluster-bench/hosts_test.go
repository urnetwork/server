package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// Four hosts per cluster, as admin info lists them, whose writes stop once two are down, as §19.6's parity row has it
// for both EC:4 and EC:8 at four hosts. The heal ends at the second read of admin info after every host is back.
type fakeHostsWorld struct {
	stateLock   sync.Mutex
	down        map[string]map[int]bool
	healReads   map[string]int
	controls    []string
	keepHealing bool
}

func (self *fakeHostsWorld) control(ctx context.Context, clusterId string, index int, host string, up bool) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.down[clusterId] == nil {
		self.down[clusterId] = map[int]bool{}
	}
	self.down[clusterId][index] = !up
	self.controls = append(self.controls, fmt.Sprintf("%s %s %d %s", clusterId, map[bool]string{true: "up", false: "down"}[up], index, host))
	return nil
}

func (self *fakeHostsWorld) downCount(clusterId string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	count := 0
	for _, down := range self.down[clusterId] {
		if down {
			count += 1
		}
	}
	return count
}

func (self *fakeHostsWorld) adminInfo(ctx context.Context, clusterId string) *server.FileClusterAdminInfo {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	info := &server.FileClusterAdminInfo{}
	anyDown := false
	for index := 1; index <= 4; index += 1 {
		endpoint := fmt.Sprintf("http://store-%d.example:9000", index)
		state := "online"
		if self.down[clusterId][index] {
			state = "offline"
			anyDown = true
		}
		info.Hosts = append(info.Hosts, &server.FileClusterHostInfo{Endpoint: endpoint, State: state})
	}
	if !anyDown && self.down[clusterId] != nil {
		self.healReads[clusterId] += 1
	}
	healing := anyDown || self.keepHealing || (self.down[clusterId] != nil && self.healReads[clusterId] < 2)
	for drive := range 8 {
		index := drive/2 + 1
		state := "ok"
		if self.down[clusterId][index] {
			state = "offline"
		}
		info.Drives = append(info.Drives, &server.FileClusterDriveInfo{
			HostEndpoint: fmt.Sprintf("http://store-%d.example:9000", index), State: state, Healing: healing && (index == 1 || index == 3),
			DiskIndex: drive, UsedByteCount: 1000 * 1000 * 1000,
		})
	}
	return info
}

// A cluster whose writes answer unknown, as a 503 does, while two of its hosts are down.
type fakeHostsCluster struct {
	sidecar.BlobCluster
	world     *fakeHostsWorld
	clusterId string
	// stores a write even with two down: a fault the verdict must catch
	broken bool
}

func (self *fakeHostsCluster) PutIfNoneMatch(ctx context.Context, key string, body []byte, options *sidecar.BlobPutOptions) (string, error) {
	if 2 <= self.world.downCount(self.clusterId) && !self.broken {
		return "", &sidecar.BlobUnknownError{Err: errors.New("503 Service Unavailable")}
	}
	return self.BlobCluster.PutIfNoneMatch(ctx, key, body, options)
}

type fakeHostsStore struct {
	sidecar.BlobStore
	world  *fakeHostsWorld
	broken string
}

func (self *fakeHostsStore) Cluster(clusterId string) (sidecar.BlobCluster, error) {
	cluster, err := self.BlobStore.Cluster(clusterId)
	if err != nil {
		return nil, err
	}
	return &fakeHostsCluster{BlobCluster: cluster, world: self.world, clusterId: clusterId, broken: clusterId == self.broken}, nil
}

// The test clock runs on while the drill's goroutine is slow, so a heal that must end waits 24 h of it, past anything
// scheduling reaches, and the test of one that never ends waits a minute.
func testHostsSettings(healTimeout time.Duration) *HostsSettings {
	settings := DefaultHostsSettings()
	settings.ProbeCount = 4
	settings.Settle = time.Second
	settings.HoldDown = 2 * time.Second
	settings.HealPollInterval = time.Second
	settings.HealTimeout = healTimeout
	return settings
}

func runTestHosts(t *testing.T, world *fakeHostsWorld, broken string, settings *HostsSettings) *HostsMeasurement {
	t.Helper()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), nil, nil, nil)
	store := &fakeHostsStore{BlobStore: blobs, world: world, broken: broken}
	measurement := &HostsMeasurement{RowHeader: RowHeader{CommitSet: fixtureCommitSet(), Settings: map[string]string{}}}
	kinds := map[string]string{"seg-1": server.FileClusterKindSegment, "meta-1": server.FileClusterKindMetadata}
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = RunHosts(context.Background(), store, []string{"seg-1", "meta-1"}, kinds, world.control, world.adminInfo, queuedClock{FakeClock: clock}, settings, "filecluster-bench/0123456789abcdef/", measurement)
	}()
	advanceUntil(t, clock, 100*time.Millisecond, done)
	if runErr != nil {
		t.Fatal(runErr)
	}
	return measurement
}

func newFakeHostsWorld() *fakeHostsWorld {
	return &fakeHostsWorld{down: map[string]map[int]bool{}, healReads: map[string]int{}}
}

func TestHostsDrillTakesTwoHostsDownAndTimesTheHeal(t *testing.T) {
	world := newFakeHostsWorld()
	measurement := runTestHosts(t, world, "", testHostsSettings(24*time.Hour))
	verdict := JudgeHosts(measurement, DefaultHostsSettings())
	if !verdict.Pass {
		t.Fatalf("verdict %v", verdict.Reasons)
	}
	want := []string{
		"seg-1 down 1 store-1.example", "seg-1 down 3 store-3.example", "seg-1 up 3 store-3.example", "seg-1 up 1 store-1.example",
		"meta-1 down 1 store-1.example", "meta-1 down 3 store-3.example", "meta-1 up 3 store-3.example", "meta-1 up 1 store-1.example",
	}
	if strings.Join(world.controls, ",") != strings.Join(want, ",") {
		t.Fatalf("controls %v", world.controls)
	}
	for _, cluster := range measurement.Clusters {
		twoDown := cluster.probe(HostsStateTwoDown)
		if twoDown.WritesStored != 0 || twoDown.WritesUnknown != 4 || twoDown.ProtectedRead != 4 || twoDown.DegradedRead != 4 {
			t.Fatalf("%s two down %+v", cluster.ClusterId, twoDown)
		}
		if !cluster.HealEnded || cluster.HealSeconds <= 0 || cluster.HealByteCount != 4*1000*1000*1000 || cluster.WritesRefusedSeconds <= 0 {
			t.Fatalf("%s heal %+v", cluster.ClusterId, cluster)
		}
	}
	var text strings.Builder
	if err := WriteHostsVerdict(&text, measurement, verdict); err != nil || !strings.Contains(text.String(), "pass") {
		t.Fatalf("%v:\n%s", err, text.String())
	}
}

func TestHostsVerdictFailsAWriteStoredWithTwoHostsDown(t *testing.T) {
	measurement := runTestHosts(t, newFakeHostsWorld(), "meta-1", testHostsSettings(24*time.Hour))
	verdict := JudgeHosts(measurement, DefaultHostsSettings())
	if verdict.Pass || !strings.Contains(strings.Join(verdict.Reasons, ";"), "meta-1: with a second host down 4 writes were stored") {
		t.Fatalf("verdict %v", verdict.Reasons)
	}
}

func TestHostsVerdictFailsAHealThatDoesNotEnd(t *testing.T) {
	world := newFakeHostsWorld()
	world.keepHealing = true
	measurement := runTestHosts(t, world, "", testHostsSettings(time.Minute))
	verdict := JudgeHosts(measurement, DefaultHostsSettings())
	if verdict.Pass || !strings.Contains(strings.Join(verdict.Reasons, ";"), "the heal did not end") {
		t.Fatalf("verdict %v", verdict.Reasons)
	}
}

func TestHostsVerdictNeedsBothKindsAndTheRto(t *testing.T) {
	measurement := runTestHosts(t, newFakeHostsWorld(), "", testHostsSettings(24*time.Hour))
	measurement.Clusters = measurement.Clusters[1:]
	measurement.Clusters[0].WritesRefusedSeconds = 5 * 3600
	verdict := JudgeHosts(measurement, DefaultHostsSettings())
	reasons := strings.Join(verdict.Reasons, ";")
	if verdict.Pass || !strings.Contains(reasons, "not on both a segment and a metadata cluster") || !strings.Contains(reasons, "past §19.6's 4h0m0s RTO") {
		t.Fatalf("verdict %v", verdict.Reasons)
	}
}

func TestTemplateHostControlNamesTheHostAndRefusesShellText(t *testing.T) {
	commands := []string{}
	control := templateHostControl("docker stop sidecar-test-fc-{cluster}-host{index}", "docker start sidecar-test-fc-{cluster}-host{index}", func(ctx context.Context, command string) ([]byte, error) {
		commands = append(commands, command)
		return nil, nil
	})
	if err := control(context.Background(), "meta-1", 3, "store-3.example", false); err != nil {
		t.Fatal(err)
	}
	if err := control(context.Background(), "meta-1", 3, "store-3.example", true); err != nil {
		t.Fatal(err)
	}
	if strings.Join(commands, ",") != "docker stop sidecar-test-fc-meta-1-host3,docker start sidecar-test-fc-meta-1-host3" {
		t.Fatalf("commands %v", commands)
	}
	if err := control(context.Background(), "meta-1", 1, "a;rm", false); err == nil {
		t.Fatal("ran a template with a host the shell must not see")
	}
}
