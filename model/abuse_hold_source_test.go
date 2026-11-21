package model_test

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/gkmservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"
)

func TestLegalHoldSourceReadsOnlyPlacementLeaseAndAckHashes(t *testing.T) {
	at := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	host := sidecartest.NewFakeHost(t.Context(), sidecartest.NewFakeClock(at), nil, nil, nil)
	t.Cleanup(host.Redis.Close)
	checkHoldPlacementSoftState(t, host.ServiceHost(), at)
}

func checkHoldPlacementSoftState(t testing.TB, host *sidecar.ServiceHost, at time.Time) {
	t.Helper()
	ctx := t.Context()
	cluster, err := host.Blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	repo, group, client := sidecar.NewId(), sidecar.NewId(), sidecar.NewId()
	placement, err := (&sidecar.EvsRepoPlace{ClusterId: cluster.ClusterId(), CreatorSidecarId: client}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	put := func(key string, body []byte) {
		t.Helper()
		if _, err := cluster.PutIfNoneMatch(ctx, key, body, nil); err != nil {
			t.Fatal(err)
		}
	}
	encode := func(message proto.Message) []byte {
		t.Helper()
		body, err := proto.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	put(sidecar.EvsPlaceKey(repo), placement)
	putForm := func(groupId sidecar.Id, generation uint64) {
		t.Helper()
		form := encode(&protocol.GroupForm{RepoId: repo.Bytes(), GroupId: groupId.Bytes(), Generation: generation})
		put(gkmservice.GkmFormKey(repo, generation), encode(&bridge.Signed{Body: form, Type: uint32(bridge.SignedType_SIGNED_GROUP_FORM),
			SignerId: client.Bytes(), Signature: make([]byte, 64)}))
	}
	putForm(group, 1)
	leaseKey, ackKey := evsservice.LeaseKey(repo), gkmservice.GkmAckRedisKey(group)
	lease := strconv.FormatInt(at.Add(evsservice.DefaultEvsSettings().LeaseLife).UnixMilli(), 10)
	ack := "5:" + strconv.FormatInt(at.UnixMilli(), 10)
	if err := host.Redis.HashSet(ctx, leaseKey, map[string][]byte{"synthetic-lease": []byte(lease)}); err != nil {
		t.Fatal(err)
	}
	if err := host.Redis.HashSet(ctx, ackKey, map[string][]byte{"synthetic-client": []byte(ack)}); err != nil {
		t.Fatal(err)
	}
	otherGroup := sidecar.NewId()
	if err := host.Redis.HashSet(ctx, gkmservice.GkmAckRedisKey(otherGroup), map[string][]byte{"unrelated": []byte(ack)}); err != nil {
		t.Fatal(err)
	}
	source := model.NewAbuseHoldSoftStateSource(host, nil)
	requireNoCopy := func(reason string) {
		t.Helper()
		if keys, err := source.RepoSoftState(ctx, server.Id(repo), at); err == nil || len(keys) != 0 {
			t.Fatalf("%s returned %d keys, error %v", reason, len(keys), err)
		}
	}
	requireNoCopy("missing group owner")
	put(gkmservice.GkmGroupOwnerKey(group), encode(&protocol.GkmGroupOwner{RepoId: repo.Bytes(), NetworkId: sidecar.NewId().Bytes()}))
	keys, err := source.RepoSoftState(ctx, server.Id(repo), at)
	if err != nil || len(keys) != 2 || keys[0].Key != leaseKey || keys[0].Fields["synthetic-lease"] != lease || keys[1].Key != ackKey || keys[1].Fields["synthetic-client"] != ack {
		t.Fatalf("scoped placement fields: %+v, %v", keys, err)
	}
	if _, err := source.RepoSoftState(ctx, server.Id(repo), at.Add(-time.Millisecond)); !errors.Is(err, model.ErrHoldPlacementSnapshotUnavailable) {
		t.Fatalf("later fields reached an earlier placement: %v", err)
	}
	putForm(otherGroup, 2)
	put(gkmservice.GkmGroupOwnerKey(otherGroup), encode(&protocol.GkmGroupOwner{RepoId: sidecar.NewId().Bytes(), NetworkId: sidecar.NewId().Bytes()}))
	requireNoCopy("foreign group owner")
}
