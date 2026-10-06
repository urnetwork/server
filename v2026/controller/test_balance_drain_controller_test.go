package controller

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
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
	allowed := func(ctx context.Context, networkId server.Id) bool { return networkId == caller }

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
	allowed := func(ctx context.Context, networkId server.Id) bool { return networkId == allowedNetwork }

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
	_, err = testBalanceDrain(store, func(context.Context, server.Id) bool { return false }, nil, testBalanceDrainSession(allowedNetwork))
	connect.AssertEqual(t, err != nil, true)
	connect.AssertEqual(t, len(store.drainCalls), 0)
}

func TestTestBalanceDrainRestoreRoundTrip(t *testing.T) {
	caller := server.NewId()
	other := server.NewId()
	store := newFakeTestBalanceDrainStore()
	store.available[caller] = 5 * 1024 * 1024
	store.available[other] = 7 * 1024 * 1024
	allowed := func(ctx context.Context, networkId server.Id) bool { return networkId == caller }

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

// The bypass-domain gate through the real model gate, with a fake sign-in
// email lookup and a fake store.
func TestTestBalanceDrainBypassDomainGate(t *testing.T) {
	testCaller := server.NewId()
	realCaller := server.NewId()
	listedCaller := server.NewId()
	networkIdEmails := map[server.Id][]string{
		testCaller:   {"acceptance-ib-20261003t120000z-1a2b@acceptance.invalid"},
		realCaller:   {"someone@gmail.com"},
		listedCaller: {"listed@gmail.com"},
	}
	lookups := []server.Id{}
	defer model.Testing_SetTestBalanceDrainSignInEmails(func(networkId server.Id) []string {
		lookups = append(lookups, networkId)
		return networkIdEmails[networkId]
	})()
	defer model.Testing_SetTestBalanceDrainAllowlist(nil)

	drain := func(clientSession *session.ClientSession) (*fakeTestBalanceDrainStore, error) {
		store := newFakeTestBalanceDrainStore()
		_, err := testBalanceDrain(store, model.TestBalanceDrainAllowed, &TestBalanceDrainArgs{DurationSeconds: 300}, clientSession)
		return store, err
	}
	refused := func(name string, clientSession *session.ClientSession) {
		t.Helper()
		store, err := drain(clientSession)
		if err == nil || !strings.HasPrefix(err.Error(), "403 ") {
			t.Fatalf("%s: want a 403 refusal, got %v", name, err)
		}
		connect.AssertEqual(t, len(store.drainCalls), 0)
	}

	model.Testing_SetTestBalanceDrainGate(nil, []string{"acceptance.invalid"})

	// a caller whose live sign-in email is on the test domain is allowed
	store, err := drain(testBalanceDrainSession(testCaller))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, store.drainCalls, []server.Id{testCaller})

	// a real-domain caller is refused
	refused("real domain", testBalanceDrainSession(realCaller))

	// JWT claims are not sign-in records: a test-domain principal or network
	// name on a real-domain network is still refused
	forged := testBalanceDrainSession(realCaller)
	forged.ByJwt.Principal = "ib@acceptance.invalid"
	forged.ByJwt.NetworkName = "ib@acceptance.invalid"
	refused("test-domain JWT claims", forged)

	// the lookup only ever asked about the JWT's own network
	for _, networkId := range lookups {
		if networkId != testCaller && networkId != realCaller {
			t.Fatalf("sign-in lookup for a network other than the caller's: %s", networkId)
		}
	}

	// a malformed bypass list refuses everyone, including test-domain callers
	model.Testing_SetTestBalanceDrainGate(nil, []string{"acceptance.invalid", "*.invalid"})
	refused("malformed list, test domain", testBalanceDrainSession(testCaller))
	refused("malformed list, real domain", testBalanceDrainSession(realCaller))

	// the explicit allowlist still works, beside or without the domain gate
	model.Testing_SetTestBalanceDrainGate([]server.Id{listedCaller}, []string{"acceptance.invalid"})
	store, err = drain(testBalanceDrainSession(listedCaller))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, store.drainCalls, []server.Id{listedCaller})
	model.Testing_SetTestBalanceDrainAllowlist([]server.Id{listedCaller})
	store, err = drain(testBalanceDrainSession(listedCaller))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, store.drainCalls, []server.Id{listedCaller})
	refused("allowlist only, test domain", testBalanceDrainSession(testCaller))

	// neither configured: off
	model.Testing_SetTestBalanceDrainGate(nil, nil)
	refused("off, test domain", testBalanceDrainSession(testCaller))
	refused("off, listed", testBalanceDrainSession(listedCaller))
}

// A request body that names another network is ignored: the drain is always
// the caller's own network, and the gate is evaluated for the caller.
func TestTestBalanceDrainBodyCannotNameAnotherNetwork(t *testing.T) {
	testCaller := server.NewId()
	otherTestNetwork := server.NewId()
	defer model.Testing_SetTestBalanceDrainSignInEmails(func(networkId server.Id) []string {
		if networkId == testCaller || networkId == otherTestNetwork {
			return []string{"ib@acceptance.invalid"}
		}
		return nil
	})()
	model.Testing_SetTestBalanceDrainGate(nil, []string{"acceptance.invalid"})
	defer model.Testing_SetTestBalanceDrainAllowlist(nil)

	var args TestBalanceDrainArgs
	body := `{"duration_seconds": 120, "network_id": "` + otherTestNetwork.String() + `", "client_id": "` + otherTestNetwork.String() + `"}`
	connect.AssertEqual(t, json.Unmarshal([]byte(body), &args), nil)
	store := newFakeTestBalanceDrainStore()
	_, err := testBalanceDrain(store, model.TestBalanceDrainAllowed, &args, testBalanceDrainSession(testCaller))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, store.drainCalls, []server.Id{testCaller})

	// a real-domain caller cannot borrow a test network's eligibility
	realCaller := server.NewId()
	_, err = testBalanceDrain(store, model.TestBalanceDrainAllowed, &args, testBalanceDrainSession(realCaller))
	if err == nil || !strings.HasPrefix(err.Error(), "403 ") {
		t.Fatalf("want a 403 refusal, got %v", err)
	}
	connect.AssertEqual(t, store.drainCalls, []server.Id{testCaller})
}
