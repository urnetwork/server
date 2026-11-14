//go:build sidecar_localstack

// T3 for gkm (KEYS-M2-17; §22.9, D-TEST-2): gkm's store suite and its routes on server's real stores,
// server.FileClusters over the local stack's MinIO (sidecar's local/docker-compose.yml: one process, 8 drive
// directories at EC:4, container sidecar-test-minio on 127.0.0.1:59000), in buckets of this test's own, and
// sidecarhost.Redis on the private test Redis that test-env.sh attests (.stack/server-test.sh). Like the clusters' own
// T3 it carries the sidecar_localstack tag and fails, rather than skips, when the stack is down.
//
// The stack runs a stock MinIO until the fork has a build, so nothing here shows D-OPS-20's settle rules; what it
// shows is gkm's conditional writes, If-None-Match, If-Match and its deletes, as the store answers them.

package gkm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/gkmservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/localstack"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecarhost"
)

// The local stack's MinIO: loopback above port 50000, with the synthetic root user its compose file starts it with,
// which reaches nothing but this stack.
const (
	gkmStackEndpoint  = "127.0.0.1:59000"
	gkmStackAccessKey = "sidecar-local-test"
	gkmStackSecretKey = "sidecar-local-test-only"
)

// Two metadata clusters, the first holding place/ and the per-sidecar objects and the second a repo's group, each a
// bucket of this test's own, made unversioned as xops makes production's.
func gkmStackClusters(t *testing.T) *server.FileClusters {
	t.Helper()
	ctx := t.Context()
	if err := localstack.CheckStackAddress(gkmStackEndpoint); err != nil {
		t.Fatal(err)
	}
	configs := []*server.FileClusterConfig{}
	for i, clusterId := range []string{"meta-1", "meta-2"} {
		configs = append(configs, &server.FileClusterConfig{
			ClusterId: clusterId,
			Kind:      server.FileClusterKindMetadata,
			First:     i == 0,
			Endpoints: []string{gkmStackEndpoint},
			AccessKey: gkmStackAccessKey,
			SecretKey: gkmStackSecretKey,
			Bucket:    "sidecar-test-server-gkm-" + clusterId,
			Region:    "us-east-1",
		})
	}
	client, err := minio.New(gkmStackEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(gkmStackAccessKey, gkmStackSecretKey, ""),
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, config := range configs {
		exists, err := client.BucketExists(ctx, config.Bucket)
		if err != nil {
			t.Fatalf("the local stack's MinIO does not answer (local/stack.sh up): %v", err)
		}
		if !exists {
			if err := client.MakeBucket(ctx, config.Bucket, minio.MakeBucketOptions{Region: config.Region}); err != nil {
				t.Fatal(err)
			}
		}
	}
	clusters, err := server.NewFileClusters(ctx, configs, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(clusters.Close)
	return clusters
}

// gkm's store suite on server's clusters and Redis (sidecart3.Suites names this test).
func TestGkmStoreSuiteOnTheLocalStack(t *testing.T) {
	clusters := gkmStackClusters(t)
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		cluster, err := clusters.FirstCluster()
		if err != nil {
			tb.Fatal(err)
		}
		sidecartest.RunGkmStoreSuite(tb, &sidecartest.GkmStoreSuiteTarget{
			Cluster:            cluster,
			Redis:              sidecarhost.NewRedis(nil),
			EpochKey:           gkmservice.GkmEpochKey,
			GenerationKey:      gkmservice.GkmGenerationKey,
			GenerationPrefix:   gkmservice.GkmGenerationPrefix,
			GroupInfoKey:       gkmservice.GkmGroupInfoKey,
			GroupPrefix:        gkmservice.GkmGroupPrefix,
			KeyPackageKey:      gkmservice.GkmKeyPackageKey,
			KeyPackageClaimKey: gkmservice.GkmKeyPackageClaimKey,
			AckRedisKey:        gkmservice.GkmAckRedisKey,
		})
	})
}

// A synthetic pin of position version.
func gkmStackPin(version uint64) *bridge.AuthPin {
	chain := sha256.Sum256([]byte("chain " + strconv.FormatUint(version, 10)))
	return &bridge.AuthPin{Version: version, Chain: chain[:]}
}

// gkm's routes on the real stores: KF1's creation writes the form, epoch/0 and gen/1 on the repo's cluster; two
// commits racing for epoch 1 leave one, the loser answered 409 with the winner; a REMOVE-free rotation stages its
// delivery and claims gen/2; a leaf's ack lands in {gka_}; and ShredGroup leaves nothing of the group behind. QD-747's
// HEAD of welcome/1 answers 404 before the uploader's Welcome and 200 after it, as the store's HEAD does.
func TestGkmRoutesOnTheLocalStack(t *testing.T) {
	clusters := gkmStackClusters(t)
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
		fakeHost := sidecartest.NewFakeHost(ctx, clock, nil, nil, nil)
		host := fakeHost.ServiceHost()
		host.Blobs = clusters
		host.Redis = sidecarhost.NewRedis(nil)
		state := newHostTestAuthState()
		gkmHost, err := NewHost(ctx, host, nil, &HostDeps{AuthState: state})
		if err != nil {
			tb.Fatal(err)
		}
		defer gkmHost.Close()
		handler := gkmHost.Handler()
		networkId := connect.NewId()
		fakeHost.Gates.OpenService(sidecar.ServiceGateGkm, networkId)
		repoId, groupId := sidecar.NewId(), sidecar.NewId()
		first, err := clusters.FirstCluster()
		if err != nil {
			tb.Fatal(err)
		}
		repoCluster, err := clusters.Cluster("meta-2")
		if err != nil {
			tb.Fatal(err)
		}
		// evs's creation: place/ on the first cluster and repo/ keying on the repo's
		place, err := (&sidecar.EvsRepoPlace{ClusterId: "meta-2", CreatorSidecarId: sidecar.NewId()}).Marshal()
		if err != nil {
			tb.Fatal(err)
		}
		repoObject := func(state protocol.EvsRepoState) []byte {
			objectBytes, _ := proto.Marshal(&protocol.EvsRepoObject{RepoId: repoId.Bytes(), Kind: protocol.RepoKind_REPO_KIND_GENERAL, State: state, Format: 1, NetworkId: sidecar.Id(networkId).Bytes()})
			return objectBytes
		}
		if _, err := first.PutIfNoneMatch(ctx, sidecar.EvsPlaceKey(repoId), place, nil); err != nil {
			tb.Fatal(err)
		}
		repoEtag, err := repoCluster.PutIfNoneMatch(ctx, gkmservice.GkmEvsRepoKey(repoId), repoObject(protocol.EvsRepoState_EVS_REPO_KEYING), nil)
		if err != nil {
			tb.Fatal(err)
		}

		admin := newHostTestDevice(tb)
		state.admit(admin.sidecarId, admin.publicKey, sidecar.AclEvsAdmin|sidecar.AclEvsWrite|sidecar.AclEvsRead)
		adminToken := hostTestToken(fakeHost, networkId, admin, "repo:"+repoId.String()+":admin")
		reader := newHostTestDevice(tb)
		state.admit(reader.sidecarId, reader.publicKey, sidecar.AclEvsRead)

		// KF1: the form, epoch/0 and gen/1
		formBody, _ := proto.Marshal(&protocol.GroupForm{RepoId: repoId.Bytes(), GroupId: groupId.Bytes(), Generation: 1, Auth: gkmStackPin(10)})
		form, _ := admin.sign("group-form", bridge.SignedType_SIGNED_GROUP_FORM, formBody, nil)
		key1, rotate1, err := sidecar.NewGkmFirstGeneration(repoId, groupId, &sidecar.GkmGenerationArgs{
			Cause:         sidecar.GkmCauseRequest,
			Auth:          gkmStackPin(10),
			Time:          clock.Now(),
			HoldersSha256: bytes.Repeat([]byte{1}, 32),
		})
		if err != nil {
			tb.Fatal(err)
		}
		signed1, err := sidecar.SignGenRotate(rotate1, admin.sign)
		if err != nil {
			tb.Fatal(err)
		}
		createBody, _ := proto.Marshal(&protocol.GkmGroupCreateRequest{
			Form:            form,
			GenRotate:       signed1,
			GenWrap:         bytes.Repeat([]byte{7}, 24+32+16),
			SealedGroupInfo: []byte("a sealed GroupInfo of epoch 0"),
			CipherSuite:     0x0010,
		})
		if code, _ := hostTestServe(handler, http.MethodPost, "/gkm/groups", adminToken, createBody); code != http.StatusOK {
			tb.Fatalf("KF1's creation: %d", code)
		}
		for _, key := range []string{gkmservice.GkmFormKey(repoId, 1), gkmservice.GkmEpochKey(groupId, 0), gkmservice.GkmGenerationKey(groupId, 1)} {
			if _, err := repoCluster.Head(ctx, key); err != nil {
				tb.Fatalf("%s is not on the repo's cluster: %v", key, err)
			}
			if _, err := first.Head(ctx, key); err == nil {
				tb.Fatalf("%s is on the first cluster", key)
			}
		}
		// evs activates the repo once its first wraps exist, and gkm's 5 s cache of repo/ lapses
		if _, err := repoCluster.PutIfMatch(ctx, gkmservice.GkmEvsRepoKey(repoId), repoEtag, repoObject(protocol.EvsRepoState_EVS_REPO_ACTIVE), nil); err != nil {
			tb.Fatal(err)
		}
		clock.Advance(gkmservice.DefaultGkmServiceSettings().RepoCacheTtl)

		// two commits racing for epoch 1
		admission := sha256.Sum256(admin.sidecarId.Bytes())
		mls, err := sidecar.NewMlsEngine(&sidecar.MlsEngineArgs{
			SidecarId:         admin.sidecarId,
			IdentityPublicKey: admin.publicKey,
			AdmissionSha256:   admission[:],
			Sign:              admin.sign,
			Store:             sidecartest.NewFakeStore().Root(),
			CheckCredential:   func(credential *protocol.UrMlsCredential) error { return nil },
			Clock:             clock,
		})
		if err != nil {
			tb.Fatal(err)
		}
		group, err := mls.CreateGroup(groupId, &sidecar.MlsUrGroup{NetworkId: sidecar.Id(networkId), RepoId: repoId, Kind: protocol.RepoKind_REPO_KIND_GENERAL})
		if err != nil {
			tb.Fatal(err)
		}
		header, _ := proto.Marshal(&protocol.GkmCommitHeader{Generation: 1, Auth: gkmStackPin(10)})
		commits := [][]byte{}
		for range 2 {
			staged, err := group.Commit(&sidecar.MlsCommitArgs{Path: true, AuthenticatedData: header})
			if err != nil {
				tb.Fatal(err)
			}
			commits = append(commits, staged.Message())
			staged.Discard()
		}
		codes := make([]int, len(commits))
		var waitGroup sync.WaitGroup
		for i, commit := range commits {
			waitGroup.Add(1)
			go func() {
				defer waitGroup.Done()
				body, _ := proto.Marshal(&protocol.GkmEpochRequest{Commit: commit, GenWrap: bytes.Repeat([]byte{9}, 24+32+16)})
				codes[i], _ = hostTestServe(handler, http.MethodPost, "/gkm/groups/"+sidecar.EvsKeyId(groupId)+"/epochs/1", adminToken, body)
			}()
		}
		waitGroup.Wait()
		if !(codes[0] == http.StatusOK && codes[1] == http.StatusConflict || codes[0] == http.StatusConflict && codes[1] == http.StatusOK) {
			tb.Fatalf("two commits racing for epoch 1: %v", codes)
		}

		// QD-747: whether epoch 1's Welcome stands, as the store answers the HEAD of its object: 404 before the
		// uploader wrote it and 200 after, with no body either way, and nothing to a token without rw
		readerToken := hostTestToken(fakeHost, networkId, reader, "repo:"+repoId.String()+":r")
		welcomePath := "/gkm/groups/" + sidecar.EvsKeyId(groupId) + "/welcome/"
		welcomeStands := func(token string, epoch string) int {
			code, body := hostTestServe(handler, http.MethodHead, welcomePath+epoch, token, nil)
			if len(body) != 0 {
				tb.Fatalf("HEAD welcome/%s answered %d with a body of %d bytes", epoch, code, len(body))
			}
			return code
		}
		if code := welcomeStands(adminToken, "1"); code != http.StatusNotFound {
			tb.Fatalf("epoch 1's Welcome before its uploader wrote it: %d", code)
		}
		if code := welcomeStands(readerToken, "1"); code != http.StatusForbidden {
			tb.Fatalf("a reader's token before the Welcome: %d", code)
		}
		welcomeBody, _ := proto.Marshal(&protocol.GkmWelcomeRequest{
			Welcome:    []byte("a Welcome without the tree"),
			Recipients: []*protocol.GkmWelcomeRecipient{{SidecarId: reader.sidecarId.Bytes(), KeyPackageRef: bytes.Repeat([]byte{0xab}, 32)}},
		})
		if code, _ := hostTestServe(handler, http.MethodPost, welcomePath+"1", adminToken, welcomeBody); code != http.StatusOK {
			tb.Fatalf("the uploader's Welcome: %d", code)
		}
		welcomePointerKey := gkmservice.GkmWelcomePointerKey(reader.sidecarId, groupId, 1)
		if _, err := repoCluster.Head(ctx, gkmservice.GkmWelcomeKey(groupId, 1)); err != nil {
			tb.Fatalf("the Welcome is not on the repo's cluster: %v", err)
		}
		if _, err := first.Head(ctx, welcomePointerKey); err != nil {
			tb.Fatalf("the Welcome's pointer is not on the first cluster: %v", err)
		}
		if code := welcomeStands(adminToken, "1"); code != http.StatusOK {
			tb.Fatalf("epoch 1's Welcome after its uploader wrote it: %d", code)
		}
		if code := welcomeStands(readerToken, "1"); code != http.StatusForbidden {
			tb.Fatalf("a reader's token after the Welcome: %d", code)
		}
		if code := welcomeStands(adminToken, "2"); code != http.StatusNotFound {
			tb.Fatalf("an epoch that does not stand: %d", code)
		}

		// a rotation stages its delivery, then claims gen/2
		key2, rotate2, err := key1.NextGeneration(&sidecar.GkmGenerationArgs{
			Cause:         sidecar.GkmCauseRequest,
			Auth:          gkmStackPin(10),
			Time:          clock.Now(),
			HoldersSha256: bytes.Repeat([]byte{2}, 32),
		})
		if err != nil {
			tb.Fatal(err)
		}
		readerKeySigned, _, err := sidecar.NewGkmReaderKey(reader.sidecarId, clock.Now(), clock.Now().Add(6*24*time.Hour), reader.sign)
		if err != nil {
			tb.Fatal(err)
		}
		readerKey := &protocol.ReaderKey{}
		if err := proto.Unmarshal(readerKeySigned.GetBody(), readerKey); err != nil {
			tb.Fatal(err)
		}
		delivery, err := sidecar.SealGkmDelivery(key2, &sidecar.GkmDeliveryArgs{
			GroupId:            groupId,
			Type:               sidecar.GkmWrapTypeReader,
			Recipient:          reader.sidecarId.Bytes(),
			RecipientPublicKey: readerKey.GetXwingPublicKey(),
			Kind:               sidecar.GkmDeliveryStaged,
		})
		if err != nil {
			tb.Fatal(err)
		}
		signedDelivery, err := sidecar.SignGkmDelivery(delivery, sidecar.GkmWrapTypeReader, admin.sign)
		if err != nil {
			tb.Fatal(err)
		}
		deliveryBytes, _ := proto.Marshal(signedDelivery)
		batch, _ := proto.Marshal(&protocol.GkmWrapBatch{Deliveries: [][]byte{deliveryBytes}})
		code, replyBytes := hostTestServe(handler, http.MethodPost, "/gkm/groups/"+sidecar.EvsKeyId(groupId)+"/wraps/2/"+hex.EncodeToString(key2.Commitment()[:8]), adminToken, batch)
		reply := &protocol.GkmWrapBatchReply{}
		if code != http.StatusOK || proto.Unmarshal(replyBytes, reply) != nil || reply.GetResults()[0].GetStatus() != http.StatusCreated {
			tb.Fatalf("the staged delivery: %d %v", code, reply.GetResults())
		}
		signed2, err := sidecar.SignGenRotate(rotate2, admin.sign)
		if err != nil {
			tb.Fatal(err)
		}
		claim, _ := proto.Marshal(signed2)
		if code, _ := hostTestServe(handler, http.MethodPost, "/gkm/groups/"+sidecar.EvsKeyId(groupId)+"/gen/2", adminToken, claim); code != http.StatusCreated {
			tb.Fatalf("the claim of gen/2: %d", code)
		}
		if code, _ := hostTestServe(handler, http.MethodPost, "/gkm/groups/"+sidecar.EvsKeyId(groupId)+"/gen/2", adminToken, claim); code != http.StatusCreated {
			tb.Fatalf("the claim's retry, a lost answer's: %d", code)
		}

		// the admin's leaf acks epoch 1
		ack, _ := proto.Marshal(&protocol.GkmAckRequest{Epoch: 1})
		if code, _ := hostTestServe(handler, http.MethodPost, "/gkm/groups/"+sidecar.EvsKeyId(groupId)+"/ack", adminToken, ack); code != http.StatusOK {
			tb.Fatalf("the ack: %d", code)
		}
		value, found, err := host.Redis.HashGet(ctx, gkmservice.GkmAckRedisKey(groupId), sidecar.EvsKeyId(admin.sidecarId))
		if err != nil || !found || !bytes.HasPrefix(value, []byte("1:")) {
			tb.Fatalf("the ack in {gka_}: %q %t %v", value, found, err)
		}

		// §11.12's shred, twice, leaves nothing of the group
		for _, final := range []bool{false, true} {
			if err := gkmservice.ShredGroup(ctx, host, repoCluster, repoId, final, nil); err != nil {
				tb.Fatal(err)
			}
		}
		left, _, err := repoCluster.List(ctx, gkmservice.GkmGroupPrefix(groupId), "", 10)
		if err != nil || len(left) != 0 {
			tb.Fatalf("the shred left %d of the group's keys: %v", len(left), err)
		}
		if fieldValues, err := host.Redis.HashGetAll(ctx, gkmservice.GkmAckRedisKey(groupId)); err != nil || len(fieldValues) != 0 {
			tb.Fatalf("the shred left {gka_}: %v", err)
		}
		// the shred took the Welcome with the group, and its pointer is this test's to clear
		if _, err := repoCluster.Head(ctx, gkmservice.GkmWelcomeKey(groupId, 1)); err == nil {
			tb.Fatalf("the shred left the Welcome")
		}
		first.Delete(context.WithoutCancel(ctx), welcomePointerKey)
		repoCluster.Delete(context.WithoutCancel(ctx), gkmservice.GkmEvsRepoKey(repoId))
		first.Delete(context.WithoutCancel(ctx), sidecar.EvsPlaceKey(repoId))
	})
}
