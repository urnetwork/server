package controller

// §6.8's sync routes behind the router (PROTO-M1-10), on the private test Redis: the long-poll's hold, its
// wake by a hint through the process's shared {scp_*}n subscription, its gate, and that its held path
// reaches no PostgreSQL; the versions pages and core's body.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/pushservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

const testSidecarSyncPushOn = "sidecar:\n  push:\n    enabled: true\n"

// A session as the router's wrapper leaves it for a client of a new network.
func testSidecarSyncSession(ctx context.Context) (*session.ClientSession, server.Id) {
	networkId := server.NewId()
	clientId := server.NewId()
	byJwt := session.NewByJwt(networkId, server.NewId(), "syncnet", false, false).Client(server.NewId(), clientId)
	return session.Testing_CreateClientSession(ctx, byJwt), clientId
}

type testSidecarSyncPoll struct {
	response *protocol.PollResponse
	err      error
}

// The long-poll holds with nothing to say and answers a hint written later, woken through the shared
// subscription; its held path takes no PostgreSQL connection.
func TestSidecarSyncPollHoldsUntilAHint(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		server.Testing_EnableKeyspaceNotifications(ctx)
		defer server.Testing_PushSidecarConfig(testSidecarSyncPushOn)()
		defer Testing_CloseSidecarSyncLongPoll()

		clientSession, clientId := testSidecarSyncSession(ctx)
		if err := model.SidecarHint(ctx, []server.Id{clientId}, &sidecar.ClientHint{Topic: sidecar.PushTopicTeam, Version: 4}); err != nil {
			t.Fatal(err)
		}
		attempts, stopDeny := server.DenyPostgresForTest(t)
		first, err := SidecarSyncPoll(nil, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if first.Reset_ == nil || len(first.Reset_.Versions) != 1 || first.Reset_.Versions[0].Version != 4 || len(first.Hints) != 1 {
			t.Fatalf("first %v", first)
		}

		results := make(chan testSidecarSyncPoll, 1)
		go func() {
			response, err := SidecarSyncPoll(first.Cursor, clientSession)
			results <- testSidecarSyncPoll{response: response, err: err}
		}()
		// a hint until one lands after the poll holds: each is a later version, and the answer is the
		// first the hold sees, so no wait here decides the outcome
		var result testSidecarSyncPoll
		for version := uint64(5); ; version += 1 {
			if err := model.SidecarHint(ctx, []server.Id{clientId}, &sidecar.ClientHint{Topic: sidecar.PushTopicTeam, Version: version}); err != nil {
				t.Fatal(err)
			}
			select {
			case result = <-results:
			case <-time.After(200 * time.Millisecond):
				continue
			}
			break
		}
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.response.Reset_ != nil || len(result.response.Hints) == 0 || result.response.Hints[0].Seq != 2 {
			t.Fatalf("held answer %v", result.response)
		}
		stopDeny()
		if count := attempts(); count != 0 {
			t.Errorf("the poll's path took %d PostgreSQL connections", count)
		}
	})
}

// With the push switch off, a poll is refused as a closed gate, and a long-poll the switch admitted
// before is closed.
func TestSidecarSyncPollGateClosed(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer Testing_CloseSidecarSyncLongPoll()
		clientSession, _ := testSidecarSyncSession(ctx)
		pop := server.Testing_PushSidecarConfig(testSidecarSyncPushOn)
		if _, err := SidecarSyncPoll(nil, clientSession); err != nil {
			t.Fatal(err)
		}
		pop()
		defer server.Testing_PushSidecarConfig("sidecar:\n  push:\n    enabled: false\n")()
		if _, err := SidecarSyncPoll(nil, clientSession); !errors.Is(err, ErrSidecarSyncGateClosed) {
			t.Fatalf("err %v", err)
		}
		sidecarSyncLongPoll.stateLock.Lock()
		closed := sidecarSyncLongPoll.longPoll == nil
		sidecarSyncLongPoll.stateLock.Unlock()
		if !closed {
			t.Errorf("the long-poll outlived its gate")
		}
	})
}

// versions pages the client's ver at 48 under one epoch, with core always present; core's body comes
// from ver or, when ver holds none, from api's clock; any other topic is 404.
func TestSidecarSyncVersionsAndCore(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession, clientId := testSidecarSyncSession(ctx)

		// no ver yet: core from api's clock, which is newer than any stored value a client could hold
		before := uint64(server.NowUtc().UnixMicro())
		core, err := SidecarSyncTopic(sidecar.PushTopicCore, "", clientSession)
		if err != nil {
			t.Fatal(err)
		}
		coreVersion, err := pushservice.ParseVersion(core.Version)
		if err != nil || coreVersion < before || core.Revoked || core.MinSecureVersion != 0 || core.Gates == nil {
			t.Fatalf("core %+v, %v", core, err)
		}

		writer, err := pushservice.NewHintWriter(model.NewSidecarPushRedis(nil), nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		for range 60 {
			if _, err := writer.HintClient(ctx, connect.Id(clientId), &sidecar.ClientHint{Topic: sidecar.PushTopicEvs(sidecar.NewId()), Version: 1_800_000_000_000_000_001}); err != nil {
				t.Fatal(err)
			}
		}
		if err := model.SidecarHint(ctx, []server.Id{clientId}, &sidecar.ClientHint{Topic: sidecar.PushTopicCore, Version: 1_700_000_000_000_000}); err != nil {
			t.Fatal(err)
		}
		topics := map[string]uint64{}
		cursor := []byte(nil)
		for page := 0; page < 3; page += 1 {
			response, err := SidecarSyncVersions(cursor, clientSession)
			if err != nil {
				t.Fatal(err)
			}
			for _, version := range response.Reset_.Versions {
				topics[version.Topic] = version.Version
			}
			if response.Reset_.Complete {
				break
			}
			cursor = response.Cursor
		}
		if len(topics) != 61 || topics[sidecar.PushTopicCore] != 1_700_000_000_000_000 {
			t.Errorf("%d topics, core %d", len(topics), topics[sidecar.PushTopicCore])
		}
		if _, err := SidecarSyncVersions([]byte("not a cursor"), clientSession); err == nil {
			t.Errorf("a malformed cursor was taken")
		}

		core, err = SidecarSyncTopic(sidecar.PushTopicCore, "12", clientSession)
		if err != nil || core.Version != "1700000000000000" {
			t.Fatalf("core %+v, %v", core, err)
		}
		if _, err := SidecarSyncTopic(sidecar.PushTopicCore, "012", clientSession); err == nil {
			t.Errorf("a malformed since was taken")
		}
		for _, topic := range []string{sidecar.PushTopicTeam, sidecar.PushTopicGkm, "anything"} {
			if _, err := SidecarSyncTopic(topic, "", clientSession); err == nil || err.Error()[:3] != "404" {
				t.Errorf("%s: %v", topic, err)
			}
		}
	})
}
