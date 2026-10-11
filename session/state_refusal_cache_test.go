// The remembered refusal is exact, expires at its ttl and stays bounded. Time
// is passed explicitly, so no assertion depends on the wall clock.
package session

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func testingStateRefusalClaims() *ByJwt {
	deviceId, clientId := server.NewId(), server.NewId()
	return &ByJwt{NetworkId: server.NewId(), UserId: server.NewId(), DeviceId: &deviceId, ClientId: &clientId}
}

func testingStateRefusalKey(t *testing.T, byJwt *ByJwt, parentClientId *server.Id) stateRefusalKey {
	t.Helper()
	key, ok := newStateRefusalKey(byJwt, parentClientId)
	if !ok {
		t.Fatal("client credential was not rememberable")
	}
	return key
}

// Another credential of the same client is answered by its own query: the
// state query's ancestor walk can refuse a legacy credential whose root-claimed
// renewal is still valid, and a parent check binds a parent the plain check
// does not.
func TestStateRefusalCacheServesOnlyItsExactCredential(t *testing.T) {
	now := time.Date(2026, 10, 11, 4, 0, 0, 0, time.UTC)
	cache := newStateRefusalCache(16, time.Hour)
	legacy := testingStateRefusalClaims()
	cache.remember(testingStateRefusalKey(t, legacy, nil), now)
	if !cache.refused(testingStateRefusalKey(t, legacy, nil), now) {
		t.Fatal("the refused credential was not remembered")
	}

	rootClaimed := *legacy
	rootClientId := server.NewId()
	rootClaimed.RootClientId = &rootClientId
	sessionClaimed := *legacy
	sessionId := server.NewId()
	sessionClaimed.SessionId = &sessionId
	otherDevice := *legacy
	deviceId := server.NewId()
	otherDevice.DeviceId = &deviceId
	otherUser := *legacy
	otherUser.UserId = server.NewId()
	otherNetwork := *legacy
	otherNetwork.NetworkId = server.NewId()
	parentClientId := server.NewId()
	for _, control := range []struct {
		name           string
		byJwt          *ByJwt
		parentClientId *server.Id
	}{
		{name: "root claim", byJwt: &rootClaimed},
		{name: "session claim", byJwt: &sessionClaimed},
		{name: "device", byJwt: &otherDevice},
		{name: "user", byJwt: &otherUser},
		{name: "network", byJwt: &otherNetwork},
		{name: "parent check", byJwt: legacy, parentClientId: &parentClientId},
	} {
		if cache.refused(testingStateRefusalKey(t, control.byJwt, control.parentClientId), now) {
			t.Errorf("%s: another credential of the client was refused from memory", control.name)
		}
	}

	// an absent optional claim never matches a present zero id
	zeroRoot := *legacy
	zeroRootClientId := server.Id{}
	zeroRoot.RootClientId = &zeroRootClientId
	if cache.refused(testingStateRefusalKey(t, &zeroRoot, nil), now) {
		t.Fatal("a present zero root claim matched an absent one")
	}
}

// Account credentials have no client row, so they are never remembered.
func TestStateRefusalKeyCoversOnlyClientCredentials(t *testing.T) {
	account := &ByJwt{NetworkId: server.NewId(), UserId: server.NewId()}
	if _, ok := newStateRefusalKey(account, nil); ok {
		t.Fatal("an account credential was rememberable")
	}
	if _, ok := newStateRefusalKey(testingStateRefusalClaims(), nil); !ok {
		t.Fatal("a client credential was not rememberable")
	}
}

// A refusal answers until its ttl, never at or after it, and an expired one is
// dropped rather than kept.
func TestStateRefusalCacheExpiresAtTheTtl(t *testing.T) {
	now := time.Date(2026, 10, 11, 4, 0, 0, 0, time.UTC)
	cache := newStateRefusalCache(16, time.Hour)
	key := testingStateRefusalKey(t, testingStateRefusalClaims(), nil)
	cache.remember(key, now)
	if !cache.refused(key, now.Add(time.Hour-time.Nanosecond)) {
		t.Fatal("the refusal expired before its ttl")
	}
	if cache.refused(key, now.Add(time.Hour)) {
		t.Fatal("the refusal answered at its ttl")
	}
	if count := cache.count(); count != 0 {
		t.Fatalf("an expired refusal was kept: %d entries", count)
	}
	// a fresh query verdict starts a fresh ttl
	cache.remember(key, now.Add(time.Hour))
	if !cache.refused(key, now.Add(2*time.Hour-time.Nanosecond)) {
		t.Fatal("a fresh verdict did not restart the ttl")
	}
}

// At most two generations are held. A credential that keeps retrying survives
// the turnover with its original expiry; one that stopped is dropped.
func TestStateRefusalCacheStaysBoundedAndKeepsRetryingCredentials(t *testing.T) {
	now := time.Date(2026, 10, 11, 4, 0, 0, 0, time.UTC)
	cache := newStateRefusalCache(2, time.Hour)
	retrying := testingStateRefusalKey(t, testingStateRefusalClaims(), nil)
	stopped := testingStateRefusalKey(t, testingStateRefusalClaims(), nil)
	cache.remember(retrying, now)
	cache.remember(stopped, now)
	for i := range 6 {
		at := now.Add(time.Duration(i+1) * time.Minute)
		// each new refusal of a full generation turns it over
		cache.remember(testingStateRefusalKey(t, testingStateRefusalClaims(), nil), at)
		if count := cache.count(); 4 < count {
			t.Fatalf("the cache grew past two generations: %d entries", count)
		}
		if !cache.refused(retrying, at) {
			t.Fatalf("turnover %d dropped a credential that kept retrying", i)
		}
	}
	if cache.refused(stopped, now.Add(10*time.Minute)) {
		t.Fatal("a credential that stopped retrying survived two turnovers")
	}
	if cache.refused(retrying, now.Add(time.Hour)) {
		t.Fatal("a carried-over refusal outlived its original expiry")
	}
}

// Forgetting drops every refusal, as the test environment reset requires.
func TestStateRefusalCacheForgetDropsEveryRefusal(t *testing.T) {
	now := time.Date(2026, 10, 11, 4, 0, 0, 0, time.UTC)
	cache := newStateRefusalCache(1, time.Hour)
	first := testingStateRefusalKey(t, testingStateRefusalClaims(), nil)
	second := testingStateRefusalKey(t, testingStateRefusalClaims(), nil)
	cache.remember(first, now)
	cache.remember(second, now)
	cache.forget()
	if cache.refused(first, now) || cache.refused(second, now) || cache.count() != 0 {
		t.Fatal("a refusal survived forget")
	}
}
