package model

// T3 for the sockets store (SOCK-M5-04) on the private test Redis: sidecartest.RunSocketStoreSuite,
// which also holds the Lua pick to PickSocketListeners pick for pick, then what only a real Redis shows:
// one script per Find, field ttls, trimming, pair ttls, and every key in a §2.8 row. The test Redis is
// one node, so §22.4's three-master run waits on T3's cluster (TEST-M1-08); the slot rule is checked on
// every script's keys in socket_store_crossslot_test.go.

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/socketsservice"

	"github.com/urnetwork/server"
)

// A store on clock with the suite's settings.
func testSocketStore(clock sidecar.Clock, settings *socketsservice.SocketStoreSettings) *SocketStore {
	return NewSocketStore(clock, &SocketStoreSettings{Store: settings, CallTimeout: 10 * time.Second})
}

// A random id.
func testSocketStoreId() connect.Id {
	return connect.Id(sidecar.NewId())
}

// A registration with §9.5's defaults on a token path.
func testSocketStoreRegistration() *sidecar.SocketRegistration {
	listenId := testSocketStoreId()
	token := testSocketStoreId()
	return &sidecar.SocketRegistration{
		NameToken:  token[:],
		SealedName: bytes.Repeat([]byte{0x5a}, 152),
		ListenId:   &listenId,
		Protocol:   "tcp",
		Weight:     100,
		MaxStreams: 256,
		Serving:    true,
		Claim:      []byte("synthetic claim"),
	}
}

// Every key in the test's Redis db.
func testSocketStoreKeys(t testing.TB, ctx context.Context) []string {
	t.Helper()
	keys := []string{}
	server.Redis(ctx, func(client server.RedisClient) {
		cursor := uint64(0)
		for {
			page, next, err := client.Scan(ctx, cursor, "*", 1000).Result()
			if err != nil {
				t.Fatal(err)
			}
			keys = append(keys, page...)
			if next == 0 {
				return
			}
			cursor = next
		}
	})
	return keys
}

func TestSocketStorePassesItsSuite(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.Run(t, func(t testing.TB) {
		sidecartest.RunSocketStoreSuite(t, func(clock sidecar.Clock, settings *socketsservice.SocketStoreSettings) socketsservice.SocketStore {
			return testSocketStore(clock, settings)
		})
	})
}

// §22.5: a Find is one script on its group, whatever reports it carries, and no other command.
func TestSocketStorePickIsOneScript(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
		store := testSocketStore(clock, socketsservice.DefaultSocketStoreSettings())
		networkId := testSocketStoreId()
		listenerId := testSocketStoreId()
		registration := testSocketStoreRegistration()
		// two listeners, since the half-group cap lets no report exclude the only one
		other := testSocketStoreRegistration()
		other.NameToken = registration.NameToken
		otherListenId := testSocketStoreId()
		other.ListenId = &otherListenId
		for _, page := range []*socketsservice.SocketStorePage{
			{NetworkId: networkId, ListenerClientId: listenerId, SetVersion: 1, Registrations: []*sidecar.SocketRegistration{registration}},
			{NetworkId: networkId, ListenerClientId: testSocketStoreId(), SetVersion: 1, Registrations: []*sidecar.SocketRegistration{other}},
		} {
			if _, err := store.ApplyPage(ctx, page); err != nil {
				t.Fatal(err)
			}
		}
		scripts := []string{}
		store.scriptCallback = func(name string, keys []string) {
			scripts = append(scripts, name)
		}
		pick, err := store.Pick(ctx, &socketsservice.SocketStorePickRequest{
			NetworkId:       networkId,
			NameKey:         registration.NameKey(),
			DialerClientId:  testSocketStoreId(),
			DialerNetworkId: networkId,
			Reports:         []*socketsservice.SocketStoreReport{{ListenerClientId: listenerId, ListenId: *registration.ListenId}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if pick.Outcome != socketsservice.SocketPickFound || pick.ExclusionCount != 1 {
			t.Fatalf("pick %+v", pick)
		}
		if len(scripts) != 1 || scripts[0] != "pick" {
			t.Fatalf("a Find ran %v", scripts)
		}
	})
}

// §9.5's leases are 45 s hash-field ttls on real Redis, the set's ttl 5 min, and the listing's field
// lives with the newest lease.
func TestSocketStoreLeasesAreFieldTtls(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := socketsservice.DefaultSocketStoreSettings()
		store := testSocketStore(nil, settings)
		networkId := testSocketStoreId()
		listenerId := testSocketStoreId()
		registration := testSocketStoreRegistration()
		if _, err := store.ApplyPage(ctx, &socketsservice.SocketStorePage{
			NetworkId: networkId, ListenerClientId: listenerId, SetVersion: 1,
			Registrations: []*sidecar.SocketRegistration{registration},
		}); err != nil {
			t.Fatal(err)
		}
		groupKeys := socketGroupKeys(networkId, registration.NameKey())
		leaseMillis := settings.Lease.Milliseconds()
		server.Redis(ctx, func(client server.RedisClient) {
			for _, field := range []string{"m:" + socketHexId(listenerId), "r:" + socketHexId(listenerId), "v"} {
				ttls, err := client.HPTTL(ctx, groupKeys[0], field).Result()
				if err != nil {
					t.Fatal(err)
				}
				if len(ttls) != 1 || ttls[0] <= 0 || leaseMillis < ttls[0] {
					t.Errorf("%s's ttl %v, want at most %d ms", field, ttls, leaseMillis)
				}
			}
			if ttl, err := client.PTTL(ctx, groupKeys[1]).Result(); err != nil || ttl <= 0 || settings.Lease < ttl {
				t.Errorf("ix's ttl %v: %v", ttl, err)
			}
			if ttl, err := client.PTTL(ctx, socketClientSetKey(listenerId)).Result(); err != nil || ttl <= settings.Lease || settings.ClientSetTtl < ttl {
				t.Errorf("the set's ttl %v: %v", ttl, err)
			}
			ttls, err := client.HPTTL(ctx, socketNamesKey(networkId), string(registration.NameKey())).Result()
			if err != nil {
				t.Fatal(err)
			}
			if len(ttls) != 1 || ttls[0] <= 0 || leaseMillis < ttls[0] {
				t.Errorf("the listing field's ttl %v", ttls)
			}
		})
	})
}

// A Find trims the leases its clock has ended, with what they left in the group's hash.
func TestSocketStoreTrimsEndedLeases(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
		settings := socketsservice.DefaultSocketStoreSettings()
		store := testSocketStore(clock, settings)
		networkId := testSocketStoreId()
		nameToken := testSocketStoreId()
		listenerIds := []connect.Id{testSocketStoreId(), testSocketStoreId()}
		for index, listenerId := range listenerIds {
			registration := testSocketStoreRegistration()
			registration.NameToken = nameToken[:]
			if _, err := store.ApplyPage(ctx, &socketsservice.SocketStorePage{
				NetworkId: networkId, ListenerClientId: listenerId, SetVersion: 1,
				Registrations: []*sidecar.SocketRegistration{registration},
			}); err != nil {
				t.Fatal(err)
			}
			if index == 0 {
				clock.Advance(settings.Lease / 2)
			}
		}
		// the first lease has ended and the second has not
		clock.Advance(settings.Lease / 2)
		pick, err := store.Pick(ctx, &socketsservice.SocketStorePickRequest{
			NetworkId: networkId, NameKey: nameToken[:], DialerClientId: testSocketStoreId(), DialerNetworkId: networkId,
		})
		if err != nil {
			t.Fatal(err)
		}
		if pick.Outcome != socketsservice.SocketPickFound || pick.Primary.Candidate.ListenerClientId != listenerIds[1] || pick.Backup != nil {
			t.Fatalf("pick %+v", pick)
		}
		groupKeys := socketGroupKeys(networkId, nameToken[:])
		server.Redis(ctx, func(client server.RedisClient) {
			members, err := client.ZRange(ctx, groupKeys[1], 0, -1).Result()
			if err != nil {
				t.Fatal(err)
			}
			if len(members) != 1 || members[0] != socketHexId(listenerIds[1]) {
				t.Errorf("ix holds %v", members)
			}
			exists, err := client.HExists(ctx, groupKeys[0], "m:"+socketHexId(listenerIds[0])).Result()
			if err != nil || exists {
				t.Errorf("the ended lease's record stayed: %v", err)
			}
		})
	})
}

// A pair's field ttl is the one the engine gave, and CreateContract's read finds it until removal.
func TestSocketStorePairTtlIsTheGrants(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		store := testSocketStore(nil, socketsservice.DefaultSocketStoreSettings())
		listenerId := testSocketStoreId()
		dialerId := testSocketStoreId()
		ttl := 10 * time.Second
		if err := store.SetServicePair(ctx, listenerId, dialerId, 1767323045000, ttl); err != nil {
			t.Fatal(err)
		}
		server.Redis(ctx, func(client server.RedisClient) {
			ttls, err := client.HPTTL(ctx, socketPairKey(listenerId), socketHexId(dialerId)).Result()
			if err != nil {
				t.Fatal(err)
			}
			if len(ttls) != 1 || ttls[0] <= 0 || ttl.Milliseconds() < ttls[0] {
				t.Errorf("the pair's ttl %v", ttls)
			}
		})
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		grantExpireMillis, found, err := GetSocketServicePair(readCtx, listenerId, dialerId)
		if err != nil || !found || grantExpireMillis != 1767323045000 {
			t.Fatalf("CreateContract's read: %d %v %v", grantExpireMillis, found, err)
		}
		if _, _, err := GetSocketServicePair(context.Background(), listenerId, dialerId); err == nil {
			t.Fatal("a read with no deadline answered")
		}
		if err := store.RemoveServicePairs(ctx, listenerId); err != nil {
			t.Fatal(err)
		}
		if _, found, err := GetSocketServicePair(readCtx, listenerId, dialerId); err != nil || found {
			t.Fatalf("a removed pair: %v %v", found, err)
		}
	})
}

// §22.7's schema guard over what the store wrote: every key it leaves is in a family a §2.8 row names.
func TestSocketStoreKeysAreInTheirRows(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		store := testSocketStore(nil, socketsservice.DefaultSocketStoreSettings())
		networkId := testSocketStoreId()
		listenerId := testSocketStoreId()
		blinded := testSocketStoreRegistration()
		edge := testSocketStoreRegistration()
		edge.NameToken = nil
		edge.SealedName = nil
		edge.Name = "tls/443/blog"
		registrations := []*sidecar.SocketRegistration{blinded, edge}
		if bytes.Compare(edge.NameKey(), blinded.NameKey()) < 0 {
			registrations = []*sidecar.SocketRegistration{edge, blinded}
		}
		if _, err := store.ApplyPage(ctx, &socketsservice.SocketStorePage{
			NetworkId: networkId, ListenerClientId: listenerId, SetVersion: 1, Registrations: registrations,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Pick(ctx, &socketsservice.SocketStorePickRequest{
			NetworkId: networkId, NameKey: blinded.NameKey(), DialerClientId: testSocketStoreId(), DialerNetworkId: networkId,
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.SetServicePair(ctx, listenerId, testSocketStoreId(), 0, time.Minute); err != nil {
			t.Fatal(err)
		}
		guard, err := sidecartest.LoadSchemaGuard()
		if err != nil {
			t.Fatal(err)
		}
		keys := testSocketStoreKeys(t, ctx)
		socketKeys := []string{}
		for _, key := range keys {
			for _, prefix := range []string{"{sg_", "{svc_", "{sn_", "{sc_"} {
				if len(prefix) <= len(key) && key[:len(prefix)] == prefix {
					socketKeys = append(socketKeys, key)
				}
			}
		}
		if len(socketKeys) < 6 {
			t.Fatalf("the store left %d keys: %v", len(socketKeys), socketKeys)
		}
		findings, err := guard.CheckStores(ctx, &sidecartest.SchemaGuardStores{RedisKeys: socketKeys})
		if err != nil {
			t.Fatal(err)
		}
		for _, finding := range findings {
			t.Error(finding)
		}
	})
}

// m:'s record carries every field a registration has, whatever bytes its strings hold.
func TestSocketRegistrationRecordRoundTrips(t *testing.T) {
	listenId := testSocketStoreId()
	registration := &sidecar.SocketRegistration{
		Name:       "tls/443/blog",
		ListenId:   &listenId,
		Protocol:   "tcp;|,:",
		Version:    -3,
		Alpn:       []string{"h2", "a;b|c,d"},
		AppMode:    "raw",
		Weight:     1000,
		MaxStreams: 7,
		Exclusive:  true,
		Affinity:   true,
		Serving:    true,
		Draining:   true,
		Inflight:   5,
		Claim:      []byte{0, ';', '|', 1},
	}
	parsed, reportMillis, err := socketParseRegistrationRecord(socketRegistrationRecord(registration), "5;1767323045000")
	if err != nil {
		t.Fatal(err)
	}
	if reportMillis != 1767323045000 || parsed.Name != registration.Name || *parsed.ListenId != listenId ||
		parsed.Protocol != registration.Protocol || parsed.Version != registration.Version ||
		len(parsed.Alpn) != 2 || parsed.Alpn[1] != registration.Alpn[1] || parsed.AppMode != registration.AppMode ||
		parsed.Weight != registration.Weight || parsed.MaxStreams != registration.MaxStreams ||
		!parsed.Exclusive || !parsed.Affinity || !parsed.Serving || !parsed.Draining || parsed.Inflight != 5 ||
		!bytes.Equal(parsed.Claim, registration.Claim) || parsed.NameToken != nil || parsed.SealedName != nil {
		t.Fatalf("parsed %+v", parsed)
	}
	if _, _, err := socketParseRegistrationRecord("too;few", ""); err == nil {
		t.Fatal("a malformed record parsed")
	}
}
