//go:build sidecar_pending

package controller

// POST /sidecar/wake through the controller on the private stack (PROTO-M8-02; §6.16, D-TRUST-10): one of a
// member's devices summons another, a forced content-free core hint to that device's current client; another
// member's device, a revoked or parked one, the caller itself and a caller with no sidecar are refused; the same
// device is summoned at most once an hour, 429 past it; and a device with no wake token is told and not woken, while
// one holding a token gets a wake-queue entry due at once. Bindings are real, on MG1's tables, so this runs under
// sidecar_pending; the verified state's rows are the test's, through sidecarWakeRows.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/pushservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// One network's bound devices and the rows the test says its verified state holds.
type sidecarWakeTestNet struct {
	t           testing.TB
	ctx         context.Context
	networkId   server.Id
	userSession *session.ClientSession
	rows        map[sidecar.Id]*bridge.SidecarRow
}

func newSidecarWakeTestNet(t testing.TB, ctx context.Context) *sidecarWakeTestNet {
	t.Helper()
	networkId, userId := server.NewId(), server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, "sidecar-wake-"+networkId.String(), userId)
	self := &sidecarWakeTestNet{
		t:           t,
		ctx:         ctx,
		networkId:   networkId,
		userSession: session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId, NetworkName: "sidecar-wake"}),
		rows:        map[sidecar.Id]*bridge.SidecarRow{},
	}
	previous := sidecarWakeRows
	sidecarWakeRows = func(ctx context.Context, networkId server.Id, sidecarIds []sidecar.Id) (map[sidecar.Id]*bridge.SidecarRow, error) {
		rows := map[sidecar.Id]*bridge.SidecarRow{}
		if networkId != self.networkId {
			return rows, nil
		}
		for _, sidecarId := range sidecarIds {
			if row, ok := self.rows[sidecarId]; ok {
				rows[sidecarId] = row
			}
		}
		return rows, nil
	}
	t.Cleanup(func() { sidecarWakeRows = previous })
	t.Cleanup(server.Testing_PushSidecarConfig("sidecar:\n  wake: {enabled: true}\n"))
	return self
}

// A bound device of member: its client's session, its sidecar and its client, its row admitted.
func (self *sidecarWakeTestNet) device(member sidecar.Id) (*session.ClientSession, server.Id, server.Id) {
	self.t.Helper()
	authResult, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{Description: "synthetic", DeviceSpec: "synthetic"}, self.userSession)
	if err != nil || authResult.Error != nil || authResult.ByClientJwt == nil {
		self.t.Fatalf("client: %v %+v", err, authResult)
	}
	byJwt, err := session.ParseByJwt(self.ctx, *authResult.ByClientJwt)
	if err != nil {
		self.t.Fatal(err)
	}
	clientSession := session.Testing_CreateClientSession(self.ctx, byJwt)
	keys := newSidecarBindTestKeys(self.t)
	if _, err := SidecarBind(&SidecarBindArgs{Binding: keys.bind(self.t, clientSession, 1)}, clientSession); err != nil {
		self.t.Fatal(err)
	}
	self.rows[sidecar.Id(keys.sidecarId)] = &bridge.SidecarRow{
		SidecarId:  keys.sidecarId.Bytes(),
		AdmittedAt: 1,
		MemberId:   member.Bytes(),
	}
	return clientSession, keys.sidecarId, *byJwt.ClientId
}

// The status a refusal declares, 0 for none.
func sidecarWakeStatus(err error) int {
	if err == nil {
		return 0
	}
	text, _, _ := strings.Cut(err.Error(), " ")
	status, _ := strconv.Atoi(text)
	return status
}

// The client's core version, 0 for none.
func sidecarWakeCoreVersion(t testing.TB, ctx context.Context, clientId server.Id) string {
	t.Helper()
	fieldValues, err := model.NewSidecarPushRedis(nil).HashGetAll(ctx, pushservice.HintVersionsKey(connect.Id(clientId)))
	if err != nil {
		t.Fatal(err)
	}
	return string(fieldValues[sidecar.PushTopicCore])
}

// The wake-queue entries of clientId.
func sidecarWakeQueued(t testing.TB, ctx context.Context, clientId server.Id) []*pushservice.WakeEntry {
	t.Helper()
	settings := pushservice.DefaultWakeQueueSettings()
	key := pushservice.WakeQueueKey(pushservice.WakeQueueShard(connect.Id(clientId), settings.ShardCount))
	streamEntries, err := model.NewSidecarPushRedis(nil).StreamRange(ctx, key, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	entries := []*pushservice.WakeEntry{}
	for _, streamEntry := range streamEntries {
		entry, err := pushservice.ParseWakeEntry(streamEntry)
		if err != nil {
			t.Fatal(err)
		}
		if entry.ClientId == connect.Id(clientId) {
			entries = append(entries, entry)
		}
	}
	return entries
}

// An own device is summoned with a forced core hint once an hour; another member's, a revoked or parked one, the
// caller itself and a client with no sidecar are refused.
func TestSidecarWakeOwnDeviceHourly(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		net := newSidecarWakeTestNet(t, ctx)
		member, other := sidecar.NewId(), sidecar.NewId()
		caller, callerSidecarId, _ := net.device(member)
		_, ownSidecarId, ownClientId := net.device(member)
		_, otherSidecarId, otherClientId := net.device(other)
		_, revokedSidecarId, _ := net.device(member)
		_, parkedSidecarId, _ := net.device(member)
		net.rows[sidecar.Id(revokedSidecarId)].RevokedAt = 2
		net.rows[sidecar.Id(parkedSidecarId)].ParkedAt = 3

		if _, err := SidecarWake(&SidecarWakeArgs{SidecarId: ownSidecarId}, caller); err != nil {
			t.Fatalf("an own device: %v", err)
		}
		if version := sidecarWakeCoreVersion(t, ctx, ownClientId); version == "" {
			t.Fatal("no core hint reached the summoned client")
		}
		if _, err := SidecarWake(&SidecarWakeArgs{SidecarId: ownSidecarId}, caller); sidecarWakeStatus(err) != http.StatusTooManyRequests {
			t.Errorf("a second summons within the hour: %v", err)
		}

		for name, sidecarId := range map[string]server.Id{"another member's": otherSidecarId, "revoked": revokedSidecarId, "parked": parkedSidecarId, "unknown": server.NewId()} {
			if _, err := SidecarWake(&SidecarWakeArgs{SidecarId: sidecarId}, caller); sidecarWakeStatus(err) != http.StatusForbidden {
				t.Errorf("%s device: %v", name, err)
			}
		}
		if version := sidecarWakeCoreVersion(t, ctx, otherClientId); version != "" {
			t.Error("another member's device was hinted")
		}
		if _, err := SidecarWake(&SidecarWakeArgs{SidecarId: callerSidecarId}, caller); sidecarWakeStatus(err) != http.StatusBadRequest {
			t.Errorf("the caller itself: %v", err)
		}

		// a client of the network bound to no sidecar
		authResult, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{Description: "synthetic", DeviceSpec: "synthetic"}, net.userSession)
		if err != nil || authResult.ByClientJwt == nil {
			t.Fatal(err)
		}
		byJwt, err := session.ParseByJwt(ctx, *authResult.ByClientJwt)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := SidecarWake(&SidecarWakeArgs{SidecarId: ownSidecarId}, session.Testing_CreateClientSession(ctx, byJwt)); sidecarWakeStatus(err) != http.StatusForbidden {
			t.Errorf("a client with no sidecar: %v", err)
		}
	})
}

// A summoned device with no wake token is hinted and not woken; one holding a token gets a wake-queue entry due at
// once, whatever its live marker says, since the hint is forced.
func TestSidecarWakeOnlyATokenHolderIsWoken(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		net := newSidecarWakeTestNet(t, ctx)
		member := sidecar.NewId()
		caller, _, _ := net.device(member)
		_, desktopSidecarId, desktopClientId := net.device(member)
		_, phoneSidecarId, phoneClientId := net.device(member)
		redis := model.NewSidecarPushRedis(nil)
		token := pushservice.WakeTokenValue(sidecar.PushTokenPlatformApns, "synthetic-apns-token-0123456789")
		if err := redis.Set(ctx, pushservice.HintTokenKey(connect.Id(phoneClientId)), token, 0); err != nil {
			t.Fatal(err)
		}
		if err := redis.Set(ctx, pushservice.HintLiveKey(connect.Id(phoneClientId)), []byte("1"), 90*time.Second); err != nil {
			t.Fatal(err)
		}

		if _, err := SidecarWake(&SidecarWakeArgs{SidecarId: desktopSidecarId}, caller); err != nil {
			t.Fatal(err)
		}
		if sidecarWakeCoreVersion(t, ctx, desktopClientId) == "" || len(sidecarWakeQueued(t, ctx, desktopClientId)) != 0 {
			t.Error("a device with no token was woken, or not hinted")
		}

		before := time.Now().UnixMilli()
		if _, err := SidecarWake(&SidecarWakeArgs{SidecarId: phoneSidecarId}, caller); err != nil {
			t.Fatal(err)
		}
		entries := sidecarWakeQueued(t, ctx, phoneClientId)
		if len(entries) != 1 || entries[0].TopicClass != sidecar.PushTopicCore || entries[0].DueMs < before || time.Now().UnixMilli() < entries[0].DueMs {
			t.Errorf("the token holder's wake: %+v", entries)
		}
	})
}

// Dark until M8: with the wake switch off the route is closed, and with no config log sequencer the rows cannot be
// read, which answers 503 rather than waking anyone unverified.
func TestSidecarWakeClosedAndUnverified(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		func() {
			defer server.Testing_PushSidecarConfig("sidecar:\n  wake: {enabled: false}\n")()
			if SidecarWakeOpen() {
				t.Error("the route is open with the wake switch off")
			}
		}()
		net := newSidecarWakeTestNet(t, ctx)
		if !SidecarWakeOpen() {
			t.Fatal("the route is closed with the switch on")
		}
		member := sidecar.NewId()
		caller, _, _ := net.device(member)
		_, ownSidecarId, ownClientId := net.device(member)
		sidecarWakeRows = func(ctx context.Context, networkId server.Id, sidecarIds []sidecar.Id) (map[sidecar.Id]*bridge.SidecarRow, error) {
			sequencer, err := model.CurrentConfigLogSequencer()
			if err != nil {
				return nil, err
			}
			return sequencer.SidecarRows(ctx, networkId, sidecarIds)
		}
		_, err := SidecarWake(&SidecarWakeArgs{SidecarId: ownSidecarId}, caller)
		if sidecarWakeStatus(err) != http.StatusServiceUnavailable || errors.Is(err, context.Canceled) {
			t.Errorf("no sequencer: %v", err)
		}
		if sidecarWakeCoreVersion(t, ctx, ownClientId) != "" {
			t.Error("a device was hinted with no verified state")
		}
	})
}
