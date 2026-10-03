package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// These run without Postgres or Redis.

type fakeTestBalanceDrainStore struct {
	drainCalls   []server.Id
	restoreCalls []server.Id
	durations    []time.Duration
	// network id -> hidden available bytes while drained
	drained   map[server.Id]model.ByteCount
	available map[server.Id]model.ByteCount
}

func newFakeTestBalanceDrainStore() *fakeTestBalanceDrainStore {
	return &fakeTestBalanceDrainStore{drained: map[server.Id]model.ByteCount{}, available: map[server.Id]model.ByteCount{}}
}

func (self *fakeTestBalanceDrainStore) Drain(ctx context.Context, networkId server.Id, duration time.Duration) (*model.TestBalanceDrain, error) {
	self.drainCalls = append(self.drainCalls, networkId)
	self.durations = append(self.durations, duration)
	if _, ok := self.drained[networkId]; !ok {
		self.drained[networkId] = self.available[networkId]
	}
	return &model.TestBalanceDrain{
		DrainId:                 server.NewId(),
		NetworkId:               networkId,
		DrainedBalanceByteCount: self.drained[networkId],
	}, nil
}

func (self *fakeTestBalanceDrainStore) Restore(ctx context.Context, networkId server.Id) int64 {
	self.restoreCalls = append(self.restoreCalls, networkId)
	if _, ok := self.drained[networkId]; ok {
		delete(self.drained, networkId)
		return 1
	}
	return 0
}

func (self *fakeTestBalanceDrainStore) balance(networkId server.Id) model.ByteCount {
	if _, ok := self.drained[networkId]; ok {
		return 0
	}
	return self.available[networkId]
}

func testBalanceDrainSession(networkId server.Id) *session.ClientSession {
	return &session.ClientSession{Ctx: context.Background(), ByJwt: &jwt.ByJwt{NetworkId: networkId}}
}

func TestTestBalanceDrainArgsCannotNameANetwork(t *testing.T) {
	for _, v := range []any{TestBalanceDrainArgs{}, TestBalanceRestoreArgs{}} {
		ty := reflect.TypeOf(v)
		for i := 0; i < ty.NumField(); i++ {
			field := ty.Field(i)
			name := strings.ToLower(field.Name + " " + field.Tag.Get("json"))
			if strings.Contains(name, "network") || strings.Contains(name, "client") || field.Type == reflect.TypeOf(server.Id{}) {
				t.Fatalf("%s.%s lets a caller target another network", ty.Name(), field.Name)
			}
		}
	}
}

func TestTestBalanceDrainUsesOnlyTheCallerNetwork(t *testing.T) {
	caller := server.NewId()
	store := newFakeTestBalanceDrainStore()
	allowed := func(networkId server.Id) bool { return networkId == caller }

	result, err := testBalanceDrain(store, allowed, &TestBalanceDrainArgs{DurationSeconds: 300}, testBalanceDrainSession(caller))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result != nil, true)
	connect.AssertEqual(t, store.drainCalls, []server.Id{caller})
	connect.AssertEqual(t, store.durations, []time.Duration{300 * time.Second})

	restored, err := testBalanceRestore(store, testBalanceDrainSession(caller))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, restored.RestoredCount, int64(1))
	connect.AssertEqual(t, store.restoreCalls, []server.Id{caller})
}

func TestTestBalanceDrainRefusesNetworksOutsideTheAllowlist(t *testing.T) {
	store := newFakeTestBalanceDrainStore()
	allowedNetwork := server.NewId()
	allowed := func(networkId server.Id) bool { return networkId == allowedNetwork }

	for _, s := range []*session.ClientSession{testBalanceDrainSession(server.NewId()), nil, {Ctx: context.Background()}} {
		result, err := testBalanceDrain(store, allowed, &TestBalanceDrainArgs{}, s)
		if err == nil || result != nil {
			t.Fatalf("drain accepted a caller outside the allowlist: %v %v", result, err)
		}
	}
	_, err := testBalanceDrain(store, allowed, &TestBalanceDrainArgs{}, testBalanceDrainSession(server.NewId()))
	if !strings.HasPrefix(err.Error(), "403 ") {
		t.Fatalf("refusal must map to 403, got %q", err.Error())
	}
	connect.AssertEqual(t, len(store.drainCalls), 0)

	// empty allowlist: disabled for everyone
	_, err = testBalanceDrain(store, func(server.Id) bool { return false }, nil, testBalanceDrainSession(allowedNetwork))
	connect.AssertEqual(t, err != nil, true)
	connect.AssertEqual(t, len(store.drainCalls), 0)
}

func TestTestBalanceDrainRestoreRoundTrip(t *testing.T) {
	caller := server.NewId()
	other := server.NewId()
	store := newFakeTestBalanceDrainStore()
	store.available[caller] = 5 * 1024 * 1024
	store.available[other] = 7 * 1024 * 1024
	allowed := func(networkId server.Id) bool { return networkId == caller }

	result, err := testBalanceDrain(store, allowed, nil, testBalanceDrainSession(caller))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.DrainedBalanceByteCount, model.ByteCount(5*1024*1024))
	connect.AssertEqual(t, store.balance(caller), model.ByteCount(0))
	connect.AssertEqual(t, store.balance(other), model.ByteCount(7*1024*1024))

	// a second drain does not overwrite the recorded amount
	again, err := testBalanceDrain(store, allowed, nil, testBalanceDrainSession(caller))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, again.DrainedBalanceByteCount, model.ByteCount(5*1024*1024))

	// another caller's restore cannot end this drain
	restored, err := testBalanceRestore(store, testBalanceDrainSession(other))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, restored.RestoredCount, int64(0))
	connect.AssertEqual(t, store.balance(caller), model.ByteCount(0))

	restored, err = testBalanceRestore(store, testBalanceDrainSession(caller))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, restored.RestoredCount, int64(1))
	connect.AssertEqual(t, store.balance(caller), model.ByteCount(5*1024*1024))
}
