//go:build sidecar_localstack

// T3 for gkm (KEYS-M2-17; §22.9, D-TEST-2): gkm's store suite and its routes on server's real stores,
// server.FileClusters over the local stack's MinIO (sidecar's local/docker-compose.yml: one process, 8 drive
// directories at EC:4, container sidecar-test-minio on 127.0.0.1:59000), in buckets of each test's own, made for it and
// emptied and removed after it, so no test reads another's objects or an earlier run's, and sidecarhost.Redis on the
// private test Redis that test-env.sh attests (.stack/server-test.sh). Like the clusters' own T3 it carries the
// sidecar_localstack tag and fails, rather than skips, when the stack is down.
//
// The stack runs a stock MinIO until the fork has a build, so nothing here shows D-OPS-20's settle rules; what it
// shows is gkm's conditional writes, If-None-Match, If-Match and its deletes, as the store answers them.
//
// v0 keeps the MLS wire dark (owner decision 610): at its defaults gkm's host serves no epoch, Welcome, KeyPackage,
// GroupInfo or ack route, and a group's creation is its form and first generation alone. gkm's routes run here at
// those defaults, and the draft MLS wire's, which stays on the branch, with DraftMls on, as gkmschedule's worlds keep it.

package gkm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
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
// bucket of this test's own, unversioned as xops makes production's, which the test's end empties and removes. A
// bucket a failed removal leaves behind is the stack's to drop (sidecar/local/stack.sh down), never another test's to
// read: every test's buckets are new.
func gkmStackClusters(t *testing.T) *server.FileClusters {
	t.Helper()
	ctx := t.Context()
	if err := localstack.CheckStackAddress(gkmStackEndpoint); err != nil {
		t.Fatal(err)
	}
	client, err := minio.New(gkmStackEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(gkmStackAccessKey, gkmStackSecretKey, ""),
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	run := strings.ToLower(server.NewId().String())
	configs := []*server.FileClusterConfig{}
	for i, clusterId := range []string{"meta-1", "meta-2"} {
		configs = append(configs, &server.FileClusterConfig{
			ClusterId: clusterId,
			Kind:      server.FileClusterKindMetadata,
			First:     i == 0,
			Endpoints: []string{gkmStackEndpoint},
			AccessKey: gkmStackAccessKey,
			SecretKey: gkmStackSecretKey,
			Bucket:    "sidecar-test-gkm-" + run + "-" + clusterId,
			Region:    "us-east-1",
		})
	}
	// registered before the clusters' close, so it runs after it
	t.Cleanup(func() {
		ctx := context.Background()
		for _, config := range configs {
			exists, err := client.BucketExists(ctx, config.Bucket)
			if err != nil || !exists {
				continue
			}
			objects := client.ListObjects(ctx, config.Bucket, minio.ListObjectsOptions{Recursive: true})
			for removal := range client.RemoveObjects(ctx, config.Bucket, objects, minio.RemoveObjectsOptions{}) {
				t.Logf("%s kept %s: %v", config.Bucket, removal.ObjectName, removal.Err)
			}
			if err := client.RemoveBucket(ctx, config.Bucket); err != nil {
				t.Logf("%s stays: %v", config.Bucket, err)
			}
		}
	})
	for _, config := range configs {
		err := client.MakeBucket(ctx, config.Bucket, minio.MakeBucketOptions{Region: config.Region})
		// the name is this test's alone, so one this user already owns is the create minio-go retried
		if err != nil && minio.ToErrorResponse(err).Code != "BucketAlreadyOwnedByYou" {
			t.Fatalf("the local stack's MinIO made no bucket %s (local/stack.sh up): %v", config.Bucket, err)
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

// One repo's group on gkm's host over the local stack's clusters: evs's placement on the first cluster and repo/
// keying on the repo's, an admin and a reader admitted, and KF1's signed form and first generation, not yet sent.
type gkmStackGroup struct {
	ctx      context.Context
	clock    *sidecartest.FakeClock
	fakeHost *sidecartest.FakeHost
	host     *sidecar.ServiceHost
	gkmHost  *Host
	handler  http.Handler
	// how long gkm caches repo/, which an activation waits out
	repoCacheTtl time.Duration

	networkId   connect.Id
	repoId      sidecar.Id
	groupId     sidecar.Id
	first       sidecar.BlobCluster
	repoCluster sidecar.BlobCluster
	repoEtag    string

	admin      *hostTestDevice
	reader     *hostTestDevice
	adminToken string
	form       *bridge.Signed
	key1       *sidecar.EvsRepoKey
	genRotate1 *bridge.Signed
}

// A group on a gkm host whose engine runs on settings, nil for the host's defaults. The caller closes it.
func newGkmStackGroup(tb testing.TB, clusters *server.FileClusters, settings *HostSettings) *gkmStackGroup {
	tb.Helper()
	ctx := tb.Context()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	fakeHost := sidecartest.NewFakeHost(ctx, clock, nil, nil, nil)
	host := fakeHost.ServiceHost()
	host.Blobs = clusters
	host.Redis = sidecarhost.NewRedis(nil)
	state := newHostTestAuthState()
	gkmHost, err := NewHost(ctx, host, settings, &HostDeps{AuthState: state})
	if err != nil {
		tb.Fatal(err)
	}
	if settings == nil {
		settings = DefaultHostSettings()
	}
	group := &gkmStackGroup{
		ctx:          ctx,
		clock:        clock,
		fakeHost:     fakeHost,
		host:         host,
		gkmHost:      gkmHost,
		handler:      gkmHost.Handler(),
		repoCacheTtl: settings.Gkm.RepoCacheTtl,
		networkId:    connect.NewId(),
		repoId:       sidecar.NewId(),
		groupId:      sidecar.NewId(),
	}
	fakeHost.Gates.OpenService(sidecar.ServiceGateGkm, group.networkId)
	if group.first, err = clusters.FirstCluster(); err != nil {
		tb.Fatal(err)
	}
	if group.repoCluster, err = clusters.Cluster("meta-2"); err != nil {
		tb.Fatal(err)
	}
	// evs's creation: place/ on the first cluster and repo/ keying on the repo's
	place, err := (&sidecar.EvsRepoPlace{ClusterId: "meta-2", CreatorSidecarId: sidecar.NewId()}).Marshal()
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := group.first.PutIfNoneMatch(ctx, sidecar.EvsPlaceKey(group.repoId), place, nil); err != nil {
		tb.Fatal(err)
	}
	if group.repoEtag, err = group.repoCluster.PutIfNoneMatch(ctx, gkmservice.GkmEvsRepoKey(group.repoId), group.repoObject(tb, protocol.EvsRepoState_EVS_REPO_KEYING), nil); err != nil {
		tb.Fatal(err)
	}

	group.admin = newHostTestDevice(tb)
	state.admit(group.admin.sidecarId, group.admin.publicKey, sidecar.AclEvsAdmin|sidecar.AclEvsWrite|sidecar.AclEvsRead)
	group.adminToken = hostTestToken(fakeHost, group.networkId, group.admin, "repo:"+group.repoId.String()+":admin")
	group.reader = newHostTestDevice(tb)
	state.admit(group.reader.sidecarId, group.reader.publicKey, sidecar.AclEvsRead)

	// KF1's form and first generation
	formBody, err := proto.Marshal(&protocol.GroupForm{RepoId: group.repoId.Bytes(), GroupId: group.groupId.Bytes(), Generation: 1, Auth: gkmStackPin(10)})
	if err != nil {
		tb.Fatal(err)
	}
	if group.form, err = group.admin.sign("group-form", bridge.SignedType_SIGNED_GROUP_FORM, formBody, nil); err != nil {
		tb.Fatal(err)
	}
	key1, rotate1, err := sidecar.NewGkmFirstGeneration(group.repoId, group.groupId, &sidecar.GkmGenerationArgs{
		Cause:         sidecar.GkmCauseRequest,
		Auth:          gkmStackPin(10),
		Time:          clock.Now(),
		HoldersSha256: bytes.Repeat([]byte{1}, 32),
	})
	if err != nil {
		tb.Fatal(err)
	}
	group.key1 = key1
	if group.genRotate1, err = sidecar.SignGenRotate(rotate1, group.admin.sign); err != nil {
		tb.Fatal(err)
	}
	return group
}

func (self *gkmStackGroup) close() {
	self.gkmHost.Close()
}

// repo/ in state, as evs writes it.
func (self *gkmStackGroup) repoObject(tb testing.TB, state protocol.EvsRepoState) []byte {
	objectBytes, err := proto.Marshal(&protocol.EvsRepoObject{
		RepoId:    self.repoId.Bytes(),
		Kind:      protocol.RepoKind_REPO_KIND_GENERAL,
		State:     state,
		Format:    1,
		NetworkId: sidecar.Id(self.networkId).Bytes(),
	})
	if err != nil {
		tb.Fatal(err)
	}
	return objectBytes
}

// A path under the group's routes.
func (self *gkmStackGroup) path(rest string) string {
	return "/gkm/groups/" + sidecar.EvsKeyId(self.groupId) + rest
}

// KF1's creation: the form and GenRotate_1, and with epochZero the draft MLS wire's epoch/0, its GenWrap, sealed
// GroupInfo and suite.
func (self *gkmStackGroup) createBody(tb testing.TB, epochZero bool) []byte {
	request := &protocol.GkmGroupCreateRequest{Form: self.form, GenRotate: self.genRotate1}
	if epochZero {
		request.GenWrap = bytes.Repeat([]byte{7}, 24+32+16)
		request.SealedGroupInfo = []byte("a sealed GroupInfo of epoch 0")
		request.CipherSuite = 0x0010
	}
	body, err := proto.Marshal(request)
	if err != nil {
		tb.Fatal(err)
	}
	return body
}

// Fails tb unless every key is on the repo's cluster and none on the first.
func (self *gkmStackGroup) onTheRepoCluster(tb testing.TB, keys ...string) {
	tb.Helper()
	for _, key := range keys {
		if _, err := self.repoCluster.Head(self.ctx, key); err != nil {
			tb.Fatalf("%s is not on the repo's cluster: %v", key, err)
		}
		if _, err := self.first.Head(self.ctx, key); err == nil {
			tb.Fatalf("%s is on the first cluster", key)
		}
	}
}

// evs activates the repo once its first wraps exist, and gkm's cache of repo/ lapses.
func (self *gkmStackGroup) activate(tb testing.TB) {
	tb.Helper()
	if _, err := self.repoCluster.PutIfMatch(self.ctx, gkmservice.GkmEvsRepoKey(self.repoId), self.repoEtag, self.repoObject(tb, protocol.EvsRepoState_EVS_REPO_ACTIVE), nil); err != nil {
		tb.Fatal(err)
	}
	self.clock.Advance(self.repoCacheTtl)
}

// §11.12's shred, twice, leaves nothing of the group.
func (self *gkmStackGroup) shred(tb testing.TB) {
	tb.Helper()
	for _, final := range []bool{false, true} {
		if err := gkmservice.ShredGroup(self.ctx, self.host, self.repoCluster, self.repoId, final, nil); err != nil {
			tb.Fatal(err)
		}
	}
	left, _, err := self.repoCluster.List(self.ctx, gkmservice.GkmGroupPrefix(self.groupId), "", 10)
	if err != nil || len(left) != 0 {
		tb.Fatalf("the shred left %d of the group's keys: %v", len(left), err)
	}
}

// gkm's routes at v0's defaults on the real stores (owner decision 610): a creation carrying the draft MLS wire's
// epoch/0 is refused, 400, writing nothing; KF1's creation writes the form and gen/1 on the repo's cluster and no
// epoch/0, as its retry does again; the draft's epoch, Welcome and ack routes are not served; a REMOVE-free rotation
// stages its delivery and claims gen/2, as a lost answer's retry does again; and ShredGroup leaves nothing of the group
// behind.
func TestGkmRoutesOnTheLocalStack(t *testing.T) {
	clusters := gkmStackClusters(t)
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		group := newGkmStackGroup(tb, clusters, nil)
		defer group.close()
		ctx := group.ctx

		// the draft's epoch/0, refused before any write
		if code, _ := hostTestServe(group.handler, http.MethodPost, "/gkm/groups", group.adminToken, group.createBody(tb, true)); code != http.StatusBadRequest {
			tb.Fatalf("a creation carrying the draft's epoch/0: %d", code)
		}
		if _, err := group.repoCluster.Head(ctx, gkmservice.GkmGenerationKey(group.groupId, 1)); !errors.Is(err, sidecar.ErrBlobNotFound) {
			tb.Fatalf("the refused creation left gen/1: %v", err)
		}
		// KF1: the form and gen/1, and the retry of a creation whose answer was lost
		for range 2 {
			if code, _ := hostTestServe(group.handler, http.MethodPost, "/gkm/groups", group.adminToken, group.createBody(tb, false)); code != http.StatusOK {
				tb.Fatalf("KF1's creation: %d", code)
			}
		}
		group.onTheRepoCluster(tb, gkmservice.GkmFormKey(group.repoId, 1), gkmservice.GkmGenerationKey(group.groupId, 1))
		if _, err := group.repoCluster.Head(ctx, gkmservice.GkmEpochKey(group.groupId, 0)); !errors.Is(err, sidecar.ErrBlobNotFound) {
			tb.Fatalf("KF1's creation wrote epoch/0: %v", err)
		}
		group.activate(tb)

		// the draft's routes, which v0 does not serve
		for _, route := range []struct {
			method string
			path   string
		}{
			{method: http.MethodPost, path: group.path("/epochs/1")},
			{method: http.MethodHead, path: group.path("/welcome/1")},
			{method: http.MethodPost, path: group.path("/ack")},
		} {
			if code, _ := hostTestServe(group.handler, route.method, route.path, group.adminToken, nil); code != http.StatusNotFound {
				tb.Fatalf("%s %s at v0's defaults: %d", route.method, route.path, code)
			}
		}

		// a rotation stages its delivery, then claims gen/2
		key2, rotate2, err := group.key1.NextGeneration(&sidecar.GkmGenerationArgs{
			Cause:         sidecar.GkmCauseRequest,
			Auth:          gkmStackPin(10),
			Time:          group.clock.Now(),
			HoldersSha256: bytes.Repeat([]byte{2}, 32),
		})
		if err != nil {
			tb.Fatal(err)
		}
		readerKeySigned, _, err := sidecar.NewGkmReaderKey(group.reader.sidecarId, group.clock.Now(), group.clock.Now().Add(6*24*time.Hour), group.reader.sign)
		if err != nil {
			tb.Fatal(err)
		}
		readerKey := &protocol.ReaderKey{}
		if err := proto.Unmarshal(readerKeySigned.GetBody(), readerKey); err != nil {
			tb.Fatal(err)
		}
		delivery, err := sidecar.SealGkmDelivery(key2, &sidecar.GkmDeliveryArgs{
			GroupId:            group.groupId,
			Type:               sidecar.GkmWrapTypeReader,
			Recipient:          group.reader.sidecarId.Bytes(),
			RecipientPublicKey: readerKey.GetXwingPublicKey(),
			Kind:               sidecar.GkmDeliveryStaged,
		})
		if err != nil {
			tb.Fatal(err)
		}
		signedDelivery, err := sidecar.SignGkmDelivery(delivery, sidecar.GkmWrapTypeReader, group.admin.sign)
		if err != nil {
			tb.Fatal(err)
		}
		deliveryBytes, _ := proto.Marshal(signedDelivery)
		batch, _ := proto.Marshal(&protocol.GkmWrapBatch{Deliveries: [][]byte{deliveryBytes}})
		code, replyBytes := hostTestServe(group.handler, http.MethodPost, group.path("/wraps/2/"+hex.EncodeToString(key2.Commitment()[:8])), group.adminToken, batch)
		reply := &protocol.GkmWrapBatchReply{}
		if code != http.StatusOK || proto.Unmarshal(replyBytes, reply) != nil || reply.GetResults()[0].GetStatus() != http.StatusCreated {
			tb.Fatalf("the staged delivery: %d %v", code, reply.GetResults())
		}
		signed2, err := sidecar.SignGenRotate(rotate2, group.admin.sign)
		if err != nil {
			tb.Fatal(err)
		}
		claim, _ := proto.Marshal(signed2)
		if code, _ := hostTestServe(group.handler, http.MethodPost, group.path("/gen/2"), group.adminToken, claim); code != http.StatusCreated {
			tb.Fatalf("the claim of gen/2: %d", code)
		}
		if code, _ := hostTestServe(group.handler, http.MethodPost, group.path("/gen/2"), group.adminToken, claim); code != http.StatusCreated {
			tb.Fatalf("the claim's retry, a lost answer's: %d", code)
		}

		group.shred(tb)
	})
}

// The draft MLS wire's routes on the real stores, with DraftMls on as gkmschedule's worlds keep it (owner decision 610
// keeps it dark in v0): KF1's creation writes the form, epoch/0 and gen/1 on the repo's cluster; two commits racing for
// epoch 1 leave one, the loser answered 409 with the winner; QD-747's HEAD of welcome/1 answers 404 before the
// uploader's Welcome and 200 after it, as the store's HEAD does, and 403 to a reader's token; a leaf's ack lands in
// {gka_}; and ShredGroup leaves nothing of the group, its Welcome or {gka_} behind.
func TestGkmDraftMlsRoutesOnTheLocalStack(t *testing.T) {
	clusters := gkmStackClusters(t)
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		settings := DefaultHostSettings()
		settings.Gkm.DraftMls = true
		group := newGkmStackGroup(tb, clusters, settings)
		defer group.close()
		ctx := group.ctx

		// KF1: the form, epoch/0 and gen/1
		if code, _ := hostTestServe(group.handler, http.MethodPost, "/gkm/groups", group.adminToken, group.createBody(tb, true)); code != http.StatusOK {
			tb.Fatalf("KF1's creation: %d", code)
		}
		group.onTheRepoCluster(tb, gkmservice.GkmFormKey(group.repoId, 1), gkmservice.GkmEpochKey(group.groupId, 0), gkmservice.GkmGenerationKey(group.groupId, 1))
		group.activate(tb)

		// two commits racing for epoch 1
		admission := sha256.Sum256(group.admin.sidecarId.Bytes())
		mls, err := sidecar.NewMlsEngine(&sidecar.MlsEngineArgs{
			SidecarId:         group.admin.sidecarId,
			IdentityPublicKey: group.admin.publicKey,
			AdmissionSha256:   admission[:],
			Sign:              group.admin.sign,
			Store:             sidecartest.NewFakeStore().Root(),
			CheckCredential:   func(credential *protocol.UrMlsCredential) error { return nil },
			Clock:             group.clock,
		})
		if err != nil {
			tb.Fatal(err)
		}
		mlsGroup, err := mls.CreateGroup(group.groupId, &sidecar.MlsUrGroup{NetworkId: sidecar.Id(group.networkId), RepoId: group.repoId, Kind: protocol.RepoKind_REPO_KIND_GENERAL})
		if err != nil {
			tb.Fatal(err)
		}
		header, _ := proto.Marshal(&protocol.GkmCommitHeader{Generation: 1, Auth: gkmStackPin(10)})
		commits := [][]byte{}
		for range 2 {
			staged, err := mlsGroup.Commit(&sidecar.MlsCommitArgs{Path: true, AuthenticatedData: header})
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
				codes[i], _ = hostTestServe(group.handler, http.MethodPost, group.path("/epochs/1"), group.adminToken, body)
			}()
		}
		waitGroup.Wait()
		if !(codes[0] == http.StatusOK && codes[1] == http.StatusConflict || codes[0] == http.StatusConflict && codes[1] == http.StatusOK) {
			tb.Fatalf("two commits racing for epoch 1: %v", codes)
		}

		// QD-747: whether epoch 1's Welcome stands, as the store answers the HEAD of its object: 404 before the
		// uploader wrote it and 200 after, with no body either way, and nothing to a token without rw
		readerToken := hostTestToken(group.fakeHost, group.networkId, group.reader, "repo:"+group.repoId.String()+":r")
		welcomeStands := func(token string, epoch string) int {
			code, body := hostTestServe(group.handler, http.MethodHead, group.path("/welcome/"+epoch), token, nil)
			if len(body) != 0 {
				tb.Fatalf("HEAD welcome/%s answered %d with a body of %d bytes", epoch, code, len(body))
			}
			return code
		}
		if code := welcomeStands(group.adminToken, "1"); code != http.StatusNotFound {
			tb.Fatalf("epoch 1's Welcome before its uploader wrote it: %d", code)
		}
		if code := welcomeStands(readerToken, "1"); code != http.StatusForbidden {
			tb.Fatalf("a reader's token before the Welcome: %d", code)
		}
		welcomeBody, _ := proto.Marshal(&protocol.GkmWelcomeRequest{
			Welcome:    []byte("a Welcome without the tree"),
			Recipients: []*protocol.GkmWelcomeRecipient{{SidecarId: group.reader.sidecarId.Bytes(), KeyPackageRef: bytes.Repeat([]byte{0xab}, 32)}},
		})
		if code, _ := hostTestServe(group.handler, http.MethodPost, group.path("/welcome/1"), group.adminToken, welcomeBody); code != http.StatusOK {
			tb.Fatalf("the uploader's Welcome: %d", code)
		}
		if _, err := group.repoCluster.Head(ctx, gkmservice.GkmWelcomeKey(group.groupId, 1)); err != nil {
			tb.Fatalf("the Welcome is not on the repo's cluster: %v", err)
		}
		if _, err := group.first.Head(ctx, gkmservice.GkmWelcomePointerKey(group.reader.sidecarId, group.groupId, 1)); err != nil {
			tb.Fatalf("the Welcome's pointer is not on the first cluster: %v", err)
		}
		if code := welcomeStands(group.adminToken, "1"); code != http.StatusOK {
			tb.Fatalf("epoch 1's Welcome after its uploader wrote it: %d", code)
		}
		if code := welcomeStands(readerToken, "1"); code != http.StatusForbidden {
			tb.Fatalf("a reader's token after the Welcome: %d", code)
		}
		if code := welcomeStands(group.adminToken, "2"); code != http.StatusNotFound {
			tb.Fatalf("an epoch that does not stand: %d", code)
		}

		// the admin's leaf acks epoch 1
		ack, _ := proto.Marshal(&protocol.GkmAckRequest{Epoch: 1})
		if code, _ := hostTestServe(group.handler, http.MethodPost, group.path("/ack"), group.adminToken, ack); code != http.StatusOK {
			tb.Fatalf("the ack: %d", code)
		}
		value, found, err := group.host.Redis.HashGet(ctx, gkmservice.GkmAckRedisKey(group.groupId), sidecar.EvsKeyId(group.admin.sidecarId))
		if err != nil || !found || !bytes.HasPrefix(value, []byte("1:")) {
			tb.Fatalf("the ack in {gka_}: %q %t %v", value, found, err)
		}

		group.shred(tb)
		if fieldValues, err := group.host.Redis.HashGetAll(ctx, gkmservice.GkmAckRedisKey(group.groupId)); err != nil || len(fieldValues) != 0 {
			tb.Fatalf("the shred left {gka_}: %v", err)
		}
		// the shred took the Welcome with the group; its pointer goes with the test's buckets
		if _, err := group.repoCluster.Head(ctx, gkmservice.GkmWelcomeKey(group.groupId, 1)); err == nil {
			tb.Fatalf("the shred left the Welcome")
		}
	})
}
