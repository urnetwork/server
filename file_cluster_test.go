package server

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/sidecar"
)

func parseFileClustersText(t *testing.T, text string) ([]*FileClusterConfig, error) {
	t.Helper()
	y := &fileClustersYaml{}
	if err := yaml.Unmarshal([]byte(text), y); err != nil {
		t.Fatal(err)
	}
	return parseFileClusterConfigs(y)
}

// A block's file: the first first, then the other metadata clusters by id, then the segment cluster.
func TestFileClusterConfigOrdersTheFirstFirst(t *testing.T) {
	configs, err := parseFileClustersText(t, `
clusters:
  seg-1: {kind: segment, endpoints: [s.example:9000], access_key: a, secret_key: s, bucket: b, region: r}
  meta-2: {kind: metadata, endpoints: [m.example:9000], access_key: a, secret_key: s, bucket: b, region: r}
  meta-1: {kind: metadata, first: true, endpoints: [m.example:9000], access_key: a, secret_key: s, bucket: b, region: r}
  meta-0: {kind: metadata, endpoints: [m.example:9000], access_key: a, secret_key: s, bucket: b, region: r}
`)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, config := range configs {
		ids = append(ids, config.ClusterId)
	}
	if want := []string{"meta-1", "meta-0", "meta-2", "seg-1"}; len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] || ids[3] != want[3] {
		t.Errorf("ids %v, want %v", ids, want)
	}
}

// gkm's block holds no segment cluster, and api's only the first.
func TestFileClusterConfigTakesASubset(t *testing.T) {
	configs, err := parseFileClustersText(t, `
clusters:
  meta-1: {kind: metadata, first: true, endpoints: [m.example:9000], access_key: a, secret_key: s, bucket: b, region: r}
`)
	if err != nil || len(configs) != 1 {
		t.Errorf("configs %v, err %v", configs, err)
	}
}

func TestFileClusterConfigRefusesWhatBreaksARule(t *testing.T) {
	for _, text := range []string{
		// no first
		`clusters: {meta-1: {kind: metadata, endpoints: [m:1], access_key: a, secret_key: s, bucket: b, region: r}}`,
		// two firsts
		`clusters: {meta-1: {kind: metadata, first: true, endpoints: [m:1], access_key: a, secret_key: s, bucket: b, region: r}, meta-2: {kind: metadata, first: true, endpoints: [m:1], access_key: a, secret_key: s, bucket: b, region: r}}`,
		// a first segment cluster
		`clusters: {seg-1: {kind: segment, first: true, endpoints: [m:1], access_key: a, secret_key: s, bucket: b, region: r}}`,
		// two segment clusters
		`clusters: {meta-1: {kind: metadata, first: true, endpoints: [m:1], access_key: a, secret_key: s, bucket: b, region: r}, seg-1: {kind: segment, endpoints: [m:1], access_key: a, secret_key: s, bucket: b, region: r}, seg-2: {kind: segment, endpoints: [m:1], access_key: a, secret_key: s, bucket: b, region: r}}`,
		// an unknown kind
		`clusters: {meta-1: {kind: cache, first: true, endpoints: [m:1], access_key: a, secret_key: s, bucket: b, region: r}}`,
		// no endpoint, no credentials, no bucket, no region
		`clusters: {meta-1: {kind: metadata, first: true, endpoints: [], access_key: a, secret_key: s, bucket: b, region: r}}`,
		`clusters: {meta-1: {kind: metadata, first: true, endpoints: [m:1], access_key: a, bucket: b, region: r}}`,
		`clusters: {meta-1: {kind: metadata, first: true, endpoints: [m:1], access_key: a, secret_key: s, region: r}}`,
		`clusters: {meta-1: {kind: metadata, first: true, endpoints: [m:1], access_key: a, secret_key: s, bucket: b}}`,
	} {
		if _, err := parseFileClustersText(t, text); !errors.Is(err, ErrFileClusterConfig) {
			t.Errorf("%s: %v", text, err)
		}
	}
}

// FS-63: a cluster reporting a second pool takes no write, metadata and segment clusters alike, and
// reads still serve.
func TestFileClusterWithTwoPoolsTakesNoWrite(t *testing.T) {
	fixture := newFileClusterFixture(t, newFakeS3(2, "build-1"), "")
	ctx := t.Context()
	for _, clusterId := range []string{"meta-1", "seg-1"} {
		cluster := fixture.clusters.clusterIdClusters[clusterId]
		if _, err := cluster.PutIfNoneMatch(ctx, "head/a/_", []byte("v1"), nil); !errors.Is(err, ErrFileClusterPools) {
			t.Errorf("%s put: %v", clusterId, err)
		}
		if err := cluster.Delete(ctx, "head/a/_"); !errors.Is(err, ErrFileClusterPools) {
			t.Errorf("%s delete: %v", clusterId, err)
		}
		if _, err := cluster.Head(ctx, "head/a/_"); !errors.Is(err, sidecar.ErrBlobNotFound) {
			t.Errorf("%s head: %v", clusterId, err)
		}
		if cluster.Ready() {
			t.Errorf("%s is ready with two pools", clusterId)
		}
	}
	for _, request := range fixture.store.takeRequests() {
		if request.method == http.MethodPut || request.method == http.MethodDelete {
			t.Errorf("a %s reached the store", request.method)
		}
	}
}

// Its twin: one pool takes writes.
func TestFileClusterWithOnePoolTakesWrites(t *testing.T) {
	fixture := newFileClusterFixture(t, newFakeS3(1, "build-1"), "")
	if _, err := fixture.cluster.PutIfNoneMatch(t.Context(), "head/a/_", []byte("v1"), nil); err != nil {
		t.Error(err)
	}
}

// No admin info read, or one gone stale, is no pool check: writes wait for one.
func TestFileClusterUnreadOrStaleAdminInfoTakesNoWrite(t *testing.T) {
	store := newFakeS3(1, "build-1")
	store.adminInfo = []byte("not json")
	fixture := newFileClusterFixture(t, store, "")
	if _, err := fixture.cluster.PutIfNoneMatch(t.Context(), "head/a/_", []byte("v1"), nil); !errors.Is(err, ErrFileClusterPools) {
		t.Errorf("unread: %v", err)
	}

	store.adminInfo = fakeAdminInfo(1, "build-1")
	fixture.clusters.Check(t.Context())
	if _, err := fixture.cluster.PutIfNoneMatch(t.Context(), "head/a/_", []byte("v1"), nil); err != nil {
		t.Errorf("read: %v", err)
	}
	fixture.clock.Advance(fixture.clusters.settings.CheckStaleAfter)
	if _, err := fixture.cluster.PutIfNoneMatch(t.Context(), "head/b/_", []byte("v1"), nil); !errors.Is(err, ErrFileClusterPools) {
		t.Errorf("stale: %v", err)
	}
}

// plan-EVSB-settle-gate: SettlePassed only while every host's build equals the recorded one.
func TestFileClusterSettlePassedFailsClosed(t *testing.T) {
	recorded := "sidecar:\n  file_clusters:\n    meta-1: {settle_passed_build: build-1}\n"
	for _, c := range []struct {
		name        string
		sidecarYaml string
		commitId    string
		adminInfo   []byte
		stale       bool
		want        bool
	}{
		{name: "the recorded build on every host", sidecarYaml: recorded, commitId: "build-1", want: true},
		{name: "no entry", sidecarYaml: "", commitId: "build-1"},
		{name: "an entry for another cluster", sidecarYaml: "sidecar:\n  file_clusters:\n    meta-2: {settle_passed_build: build-1}\n", commitId: "build-1"},
		{name: "an upgrade not yet recorded", sidecarYaml: recorded, commitId: "build-2"},
		{name: "unreadable admin info", sidecarYaml: recorded, commitId: "build-1", adminInfo: []byte("{")},
		{name: "admin info gone stale", sidecarYaml: recorded, commitId: "build-1", stale: true},
	} {
		store := newFakeS3(1, c.commitId)
		if c.adminInfo != nil {
			store.adminInfo = c.adminInfo
		}
		fixture := newFileClusterFixture(t, store, c.sidecarYaml)
		if c.stale {
			fixture.clock.Advance(fixture.clusters.settings.CheckStaleAfter)
		}
		if got := fixture.cluster.SettlePassed(); got != c.want {
			t.Errorf("%s: SettlePassed %v", c.name, got)
		}
	}
}

// One host on another build is enough to read false.
func TestFileClusterSettlePassedNeedsEveryHost(t *testing.T) {
	store := newFakeS3(1, "build-1")
	store.adminInfo = []byte(`{"backend": {"totalSets": [1]}, "servers": [` +
		`{"state": "online", "endpoint": "a", "commitID": "build-1"},` +
		`{"state": "online", "endpoint": "b", "commitID": "build-2"}]}`)
	fixture := newFileClusterFixture(t, store, "sidecar:\n  file_clusters:\n    meta-1: {settle_passed_build: build-1}\n")
	if fixture.cluster.SettlePassed() {
		t.Error("a mixed cluster passed")
	}
}

// §19.4's readiness: the sentinel read within CheckStaleAfter, with one pool, and nothing else.
func TestFileClusterSentinelReadinessFailsClosed(t *testing.T) {
	store := newFakeS3(1, "build-1")
	fixture := newFileClusterFixture(t, store, "")
	if fixture.cluster.Ready() || fixture.clusters.Ready() {
		t.Error("ready with no sentinel")
	}
	for _, clusterId := range []string{"meta-1", "seg-1"} {
		if _, err := fixture.clusters.clusterIdClusters[clusterId].PutIfNoneMatch(t.Context(), fileClusterSentinelKey, []byte("sentinel"), nil); err != nil {
			t.Fatal(err)
		}
	}
	fixture.clusters.Check(t.Context())
	if !fixture.cluster.Ready() || !fixture.clusters.Ready() {
		t.Error("not ready with the sentinel read")
	}
	fixture.clock.Advance(fixture.clusters.settings.CheckStaleAfter)
	if fixture.cluster.Ready() {
		t.Error("ready on a stale read")
	}
	// a store that stops answering is not ready once its last read goes stale
	store.failStatus = http.StatusServiceUnavailable
	fixture.clusters.Check(t.Context())
	if fixture.cluster.Ready() {
		t.Error("ready while the sentinel read fails")
	}
}

// §19.9's lines for {nvb_<set>}'s at_limit, from admin info's raw usage and inodes.
func TestFileClusterAtLimitReadsRawUseAndInodes(t *testing.T) {
	fixture := newFileClusterFixture(t, newFakeS3(1, "build-1"), "")
	// the fake reports 10% raw and 10% inodes
	if atLimit, ok := fixture.cluster.AtLimit(80, 70); !ok || atLimit {
		t.Errorf("at 10%%: %v %v", atLimit, ok)
	}
	if atLimit, ok := fixture.cluster.AtLimit(10, 70); !ok || !atLimit {
		t.Errorf("raw at its line: %v %v", atLimit, ok)
	}
	if atLimit, ok := fixture.cluster.AtLimit(80, 10); !ok || !atLimit {
		t.Errorf("inodes at their line: %v %v", atLimit, ok)
	}
	fixture.clock.Advance(fixture.clusters.settings.CheckStaleAfter)
	if _, ok := fixture.cluster.AtLimit(80, 70); ok {
		t.Error("a stale read answered")
	}
}

func TestFileClusterAdminInfoParsesDrivesAndHealing(t *testing.T) {
	info, err := parseFileClusterAdminInfo(fakeAdminInfo(1, "build-1"), time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if info.PoolCount != 1 || len(info.Hosts) != 1 || info.Hosts[0].CommitId != "build-1" || len(info.Drives) != 2 {
		t.Fatalf("info %+v", info)
	}
	if info.Drives[0].Healing || !info.Drives[1].Healing {
		t.Errorf("healing %v %v", info.Drives[0].Healing, info.Drives[1].Healing)
	}
	if info.RawUsageByteCount != 100 || info.RawCapacityByteCount != 1000 {
		t.Errorf("raw %d of %d", info.RawUsageByteCount, info.RawCapacityByteCount)
	}
}

// place_new names the cluster for new repos, and the first takes them while it names none.
func TestFileClusterNewRepoClusterFollowsPlaceNew(t *testing.T) {
	fixture := newFileClusterFixture(t, newFakeS3(1, "build-1"), "")
	if got := fixture.clusters.NewRepoClusterId(); got != "meta-1" {
		t.Errorf("no place_new: %q", got)
	}
	config, err := parseSidecarConfig([]byte("sidecar:\n  place_new: meta-2\n"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.config = config
	if got := fixture.clusters.NewRepoClusterId(); got != "meta-2" {
		t.Errorf("place_new: %q", got)
	}
	if _, err := fixture.clusters.Cluster("meta-2"); !errors.Is(err, sidecar.ErrBlobClusterUnknown) {
		t.Errorf("a cluster this block holds no credentials for: %v", err)
	}
}

// MinIO's own numbering, as the fork's getLocalServerProperty and the drives' endpoints fill it: a host
// counts pools from 1, poolNumber is math.MaxInt when a host has several, and a drive counts from 0. One
// pool is one however the fields mix, and a second shows in any of them.
func TestFileClusterAdminInfoCountsPoolsAsMinioNumbersThem(t *testing.T) {
	onePool := `{"backend": {"backendType": "Erasure", "totalSets": [1], "totalDrivesPerSet": [8]},
		"servers": [{"state": "online", "endpoint": "host-a.example:9000", "commitID": "build-1", "poolNumber": 1, "poolNumbers": [1],
			"drives": [{"endpoint": "/drives/drive1", "state": "ok", "pool_index": 0, "set_index": 0, "disk_index": 0},
				{"endpoint": "/drives/drive2", "state": "ok", "pool_index": 0, "set_index": 0, "disk_index": 1}]}],
		"pools": {"0": {"0": {"id": 0, "rawUsage": 100, "rawCapacity": 1000}}}}`
	twoPoolsByHost := `{"backend": {"totalSets": [1]},
		"servers": [{"state": "online", "endpoint": "host-a.example:9000", "commitID": "build-1", "poolNumber": 1, "poolNumbers": [1],
				"drives": [{"endpoint": "d1", "state": "ok", "pool_index": 0}]},
			{"state": "online", "endpoint": "host-b.example:9000", "commitID": "build-1", "poolNumber": 2, "poolNumbers": [2],
				"drives": [{"endpoint": "d1", "state": "ok", "pool_index": 1}]}],
		"pools": {"0": {"0": {}}}}`
	twoPoolsOnOneHost := `{"backend": {"totalSets": [1]},
		"servers": [{"state": "online", "endpoint": "host-a.example:9000", "commitID": "build-1", "poolNumber": 9223372036854775807, "poolNumbers": [1, 2],
			"drives": [{"endpoint": "d1", "state": "ok", "pool_index": 0}]}],
		"pools": {"0": {"0": {}}}}`
	twoPoolsByDrive := `{"backend": {"totalSets": [1]},
		"servers": [{"state": "online", "endpoint": "host-a.example:9000", "commitID": "build-1", "poolNumber": 1, "poolNumbers": [1],
			"drives": [{"endpoint": "d1", "state": "ok", "pool_index": 0}, {"endpoint": "d2", "state": "ok", "pool_index": 1}]}]}`
	twoPoolsBySets := `{"backend": {"totalSets": [1, 1]},
		"servers": [{"state": "online", "endpoint": "host-a.example:9000", "commitID": "build-1", "poolNumber": 1, "poolNumbers": [1],
			"drives": [{"endpoint": "d1", "state": "ok", "pool_index": 0}]}]}`
	// an older answer with poolNumber alone
	onePoolByNumberAlone := `{"backend": {"totalSets": [1]},
		"servers": [{"state": "online", "endpoint": "host-a.example:9000", "commitID": "build-1", "poolNumber": 1,
			"drives": [{"endpoint": "d1", "state": "ok", "pool_index": 0}]}]}`
	for _, c := range []struct {
		name string
		body string
		want int
	}{
		{name: "one pool", body: onePool, want: 1},
		{name: "a second host in a second pool", body: twoPoolsByHost, want: 2},
		{name: "one host in two pools", body: twoPoolsOnOneHost, want: 2},
		{name: "a drive in a second pool", body: twoPoolsByDrive, want: 2},
		{name: "a second pool's sets", body: twoPoolsBySets, want: 2},
		{name: "poolNumber alone", body: onePoolByNumberAlone, want: 1},
	} {
		info, err := parseFileClusterAdminInfo([]byte(c.body), time.Unix(0, 0))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if info.PoolCount != c.want {
			t.Errorf("%s: %d pools, want %d", c.name, info.PoolCount, c.want)
		}
	}
	// a number neither count allows is no answer, so the cluster stays unread and takes no write
	for _, body := range []string{
		`{"servers": [{"endpoint": "a", "poolNumbers": [0]}]}`,
		`{"servers": [{"endpoint": "a", "poolNumbers": [1], "drives": [{"endpoint": "d1", "pool_index": -1}]}]}`,
	} {
		if _, err := parseFileClusterAdminInfo([]byte(body), time.Unix(0, 0)); err == nil {
			t.Errorf("parsed %s", body)
		}
	}
}
