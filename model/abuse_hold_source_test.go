package model

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/server"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/gkmservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"
)

func TestLegalHoldSourceReadsOnlyPlacementLeaseAndAckHashes(t *testing.T) {
	ctx := t.Context()
	at := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	host := sidecartest.NewFakeHost(ctx, sidecartest.NewFakeClock(at), nil, nil, nil)
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
	form := encode(&protocol.GroupForm{RepoId: repo.Bytes(), GroupId: group.Bytes(), Generation: 1})
	put(gkmservice.GkmFormKey(repo, 1), encode(&bridge.Signed{Body: form, Type: uint32(bridge.SignedType_SIGNED_GROUP_FORM),
		SignerId: client.Bytes(), Signature: make([]byte, 64)}))
	put(gkmservice.GkmGroupOwnerKey(group), encode(&protocol.GkmGroupOwner{RepoId: repo.Bytes(), NetworkId: sidecar.NewId().Bytes()}))
	leaseKey, ackKey := evsservice.LeaseKey(repo), gkmservice.GkmAckRedisKey(group)
	lease := strconv.FormatInt(at.Add(evsservice.DefaultEvsSettings().LeaseLife).UnixMilli(), 10)
	ack := "5:" + strconv.FormatInt(at.UnixMilli(), 10)
	if err := host.Redis.HashSet(ctx, leaseKey, map[string][]byte{"synthetic-lease": []byte(lease)}); err != nil {
		t.Fatal(err)
	}
	if err := host.Redis.HashSet(ctx, ackKey, map[string][]byte{"synthetic-client": []byte(ack)}); err != nil {
		t.Fatal(err)
	}
	if err := host.Redis.HashSet(ctx, gkmservice.GkmAckRedisKey(sidecar.NewId()), map[string][]byte{"unrelated": []byte(ack)}); err != nil {
		t.Fatal(err)
	}
	source := NewAbuseHoldSoftStateSource(host.ServiceHost(), nil)
	keys, err := source.RepoSoftState(ctx, server.Id(repo), at)
	if err != nil || len(keys) != 2 || keys[0].Key != leaseKey || keys[0].Fields["synthetic-lease"] != lease || keys[1].Key != ackKey || keys[1].Fields["synthetic-client"] != ack {
		t.Fatalf("scoped placement fields: %+v, %v", keys, err)
	}
	if _, err := source.RepoSoftState(ctx, server.Id(repo), at.Add(-time.Millisecond)); !errors.Is(err, ErrHoldPlacementSnapshotUnavailable) {
		t.Fatalf("later fields reached an earlier placement: %v", err)
	}
}
