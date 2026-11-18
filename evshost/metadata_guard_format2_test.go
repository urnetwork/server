package evshost

// §22.7's schema guard over format 2's families on server's side (EVS2-M10-32; I-18, R-TEST-10, §2.8's evs-28): what
// a DROP leaves in the stores, written through evs's own table and key builders over a host's stores as the evs block
// and taskworker write them (the repo's drop table at drops/<repo>/_, the {evsd_<repo_id>} hint each table write sets,
// and a job's dropscan/<repo>/<id>/<f> files), is each in a family a live row of metadata_list.yml names, so evs writes
// Redis only in its row's families, {evsd_} among them from M10. And the guard's self-test: an object family and a Redis
// family no row names each fail it, so a format-2 family that loses its row fails here rather than passing unread. The
// proto half, each format-2 field mapped to a row and back with a field no row names failing, is sidecar's
// (sidecartest/schema_guard_test.go's TestSchemaGuardMapsFormat2sValues and TestSchemaGuardFailsOnAProtoFieldNoRowNames).

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"
)

// A host's stores after a DROP's table write and a job's file: the fake host, the repo, and the keys written.
func metadataGuardFormat2Stores(t *testing.T) (*sidecartest.FakeHost, sidecar.Id) {
	t.Helper()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	fake := sidecartest.NewFakeHost(t.Context(), clock, nil, nil, nil)
	host := fake.ServiceHost()
	repoId := sidecar.Id{7, 1, 7}
	table, err := evsservice.NewDropTable(host, evsservice.NewBudgetedBlobs(host.Blobs), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := fake.Budget.AdmitP1(t.Context(), "meta-1", [16]byte{9}, sidecar.WriteCost{ImmediateByteCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer grant.Close()
	commit := &evsservice.DropCommit{
		ClusterId: "meta-1", RepoId: repoId, RefId: sidecar.Id{4}, PushId: sidecar.Id{5}, Version: 2,
		Place: &protocol.EvsPlace{Minute: evsservice.BatchMinute(clock.Now()), Length: 100},
		Body:  &protocol.EvsDropHistory{DropId: make([]byte, 16), Action: 1, NotBeforeMs: clock.Now().Add(7 * 24 * time.Hour).UnixMilli()},
	}
	if err := table.Append(grant.Context(), commit, clock.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	segments, err := host.Blobs.SegmentCluster()
	if err != nil {
		t.Fatal(err)
	}
	jobId := sidecar.Id{8, 8}
	for _, kind := range []evsservice.DropFileKind{evsservice.DropFileSnap, evsservice.DropFileList} {
		if _, err := segments.PutIfNoneMatch(t.Context(), evsservice.DropFileKey(repoId, jobId, kind, 0), []byte("a job's file"), nil); err != nil {
			t.Fatal(err)
		}
	}
	return fake, repoId
}

// Every key the host's stores hold: its objects by cluster and key, and its Redis keys.
func metadataGuardFormat2Keys(fake *sidecartest.FakeHost) ([]string, []string) {
	objects := []string{}
	for name := range fake.Blobs.Dump() {
		objects = append(objects, name)
	}
	redisKeys := []string{}
	for key := range fake.Redis.Dump() {
		redisKeys = append(redisKeys, key)
	}
	slices.Sort(objects)
	slices.Sort(redisKeys)
	return objects, redisKeys
}

func TestMetadataGuardFormat2FamiliesHaveRows(t *testing.T) {
	fake, repoId := metadataGuardFormat2Stores(t)
	objects, redisKeys := metadataGuardFormat2Keys(fake)
	held := func(keys []string, part string) bool {
		return slices.ContainsFunc(keys, func(key string) bool { return strings.Contains(key, part) })
	}
	if !held(objects, "drops/") || !held(objects, "dropscan/") || !slices.Contains(redisKeys, evsservice.DropHintKey(repoId)) {
		t.Fatalf("the stores hold no drop table, job file or hint: %v %v", objects, redisKeys)
	}
	guard, err := sidecartest.LoadSchemaGuard()
	if err != nil {
		t.Fatal(err)
	}
	findings, err := guard.CheckStores(sidecartest.InspectContext(context.Background()), &sidecartest.SchemaGuardStores{
		Blobs: fake.Blobs, RedisKeys: redisKeys,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range findings {
		t.Errorf("%s", finding)
	}
}

// The self-test: an object family and a Redis family beside format 2's that no row names each fail the guard.
func TestMetadataGuardFormat2FailsOnAnUnlistedFamily(t *testing.T) {
	fake, repoId := metadataGuardFormat2Stores(t)
	cluster, err := fake.Blobs.Cluster("meta-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cluster.PutIfNoneMatch(t.Context(), "dropstash/"+sidecar.EvsKeyId(repoId)+"/_", []byte("unlisted"), nil); err != nil {
		t.Fatal(err)
	}
	_, redisKeys := metadataGuardFormat2Keys(fake)
	redisKeys = append(redisKeys, "{evsx_"+sidecar.EvsKeyId(repoId)+"}")
	guard, err := sidecartest.LoadSchemaGuard()
	if err != nil {
		t.Fatal(err)
	}
	findings, err := guard.CheckStores(sidecartest.InspectContext(context.Background()), &sidecartest.SchemaGuardStores{
		Blobs: fake.Blobs, RedisKeys: redisKeys,
	})
	if err != nil {
		t.Fatal(err)
	}
	found := func(part string) bool {
		return slices.ContainsFunc(findings, func(finding string) bool { return strings.Contains(finding, part) })
	}
	if len(findings) != 2 || !found("dropstash") || !found("evsx_") {
		t.Fatalf("the guard's findings over two unlisted families: %v", findings)
	}
}
