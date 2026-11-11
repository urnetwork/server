package connect

// §22.5's §6 row "team hints to acl_enforcing device clients" (PROTO-M8-01, §6.8, §18.6): a resident of an
// acl_enforcing device client hosts a publisher of team alone, whose reset names only team and whose
// other topics' hints reach the client as their seq alone, so its forwarding follows the config and
// learns nothing else; a sidecar's client keeps every topic; and the publisher keeps {scp_<C>}live while
// it runs. On sidecartest's FakeRedis and FakeClock, as sidecar_publisher_test.go's.

import (
	"context"
	"slices"
	"testing"

	connectlib "github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/pushservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The marks decide the topics: every one for a strict_exp client, team for an acl_enforcing device
// client, and no publisher for a client with neither.
func TestSidecarPublisherTopicsByMarks(t *testing.T) {
	cases := []struct {
		marks  model.SidecarPushMarks
		topics []string
		hosted bool
	}{
		{marks: model.SidecarPushMarks{StrictExp: true}, topics: nil, hosted: true},
		{marks: model.SidecarPushMarks{StrictExp: true, AclEnforcing: true}, topics: nil, hosted: true},
		{marks: model.SidecarPushMarks{AclEnforcing: true}, topics: []string{sidecar.PushTopicTeam}, hosted: true},
		{marks: model.SidecarPushMarks{}, topics: nil, hosted: false},
	}
	for _, c := range cases {
		topics, hosted := sidecarPublisherTopics(c.marks)
		if hosted != c.hosted || !slices.Equal(topics, c.topics) {
			t.Errorf("%+v: %v %t", c.marks, topics, hosted)
		}
	}
}

// An acl_enforcing device client's publisher tells it team and nothing else: its reset holds team's
// version only, a team hint arrives whole, and every other topic's hint is its seq alone, so the client
// sees no gap and no topic beyond its own.
func TestAclEnforcingDeviceClientHearsTeamAlone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	fakeRedis := sidecartest.NewFakeRedis(ctx, clock, nil, nil, nil)
	defer fakeRedis.Close()
	sidecartest.RegisterPushScripts(fakeRedis)
	writer, err := pushservice.NewHintWriter(fakeRedis, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	clientId := server.NewId()
	hint := func(topic string, version uint64) {
		t.Helper()
		if _, err := writer.HintClient(ctx, connectlib.Id(clientId), &sidecar.ClientHint{Topic: topic, Version: version, Kind: 0x0A01, Body: []byte{7}}); err != nil {
			t.Fatal(err)
		}
	}
	// what its network's owners wrote before the resident came up
	hint(sidecar.PushTopicTeam, 4)
	hint(sidecar.PushTopicCore, 5)
	hint(sidecar.PushTopicMail, 6)

	topics, hosted := sidecarPublisherTopics(model.SidecarPushMarks{AclEnforcing: true})
	if !hosted {
		t.Fatal("no publisher for an acl_enforcing device client")
	}
	settings := pushservice.DefaultSidecarPublisherSettings()
	publisher, err := pushservice.NewSidecarPublisher(settings, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	subscriber := newTestSidecarPushSubscriber(ctx, true)
	remove := subscriber.AddSidecarPushListener(clientId, publisher)
	defer remove()
	send, messages := newTestSidecarPushSend(t)
	recording := &testSidecarPushRedis{FakeRedis: fakeRedis, gets: make(chan struct{}, 16)}
	publisher.Start(ctx, &pushservice.PublisherEnv{ClientId: connectlib.Id(clientId), Send: send, Redis: recording, Topics: topics})

	reset := awaitTestSidecarPushMessage(t, messages)
	if reset.reset == nil || !reset.reset.Complete || len(reset.reset.Versions) != 1 ||
		reset.reset.Versions[0].Topic != sidecar.PushTopicTeam || reset.reset.Versions[0].Version != 4 {
		t.Fatalf("the reset: %+v", reset)
	}
	// the stream holds what the reset already covered: the read past its tail finds nothing
	passTestSidecarPushTimer(t, clock, settings.HintCoalesceTimeout)
	<-recording.gets
	if _, found, err := fakeRedis.Get(ctx, pushservice.HintLiveKey(connectlib.Id(clientId))); err != nil || !found {
		t.Fatalf("no live marker: %v", err)
	}

	hint(sidecar.PushTopicEvs(sidecar.NewId()), 9)
	hint(sidecar.PushTopicTeam, 10)
	subscriber.resyncSidecarPushListeners()
	passTestSidecarPushTimer(t, clock, settings.HintCoalesceTimeout)
	evs := awaitTestSidecarPushMessage(t, messages)
	team := awaitTestSidecarPushMessage(t, messages)
	if evs.hint == nil || evs.hint.Seq != 4 || evs.hint.Topic != "" || evs.hint.Version != 0 || len(evs.hint.Hint) != 0 {
		t.Errorf("another topic's hint reached the device client as %+v", evs.hint)
	}
	if team.hint == nil || team.hint.Seq != 5 || team.hint.Topic != sidecar.PushTopicTeam || team.hint.Version != 10 || len(team.hint.Hint) != 1 {
		t.Errorf("the team hint arrived as %+v", team.hint)
	}
}
