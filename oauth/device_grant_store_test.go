package oauth

// Device authorizations on Redis (device_grant_store.go; QD-830, §2.8's api-11), on the test stack's Redis: an
// authorization waits as JSON under {oauthdev_<device code's SHA-256>} with a ttl of its remaining life, and
// {oauthuser_<user code>} points at it with the same ttl; a swap replaces only the value it read and keeps the ttl;
// a take is answered once; a key Redis expired, or an authorization past its life by the store's clock, is gone, and
// the poll answers expired_token for it; and nothing that is not an authorization's is ever answered as one.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A waiting authorization as authorizeDeviceAt makes one, with a fresh device code and the given user code.
func testRedisDeviceAuthorization(t testing.TB, userCode string, now time.Time) *DeviceAuthorization {
	t.Helper()
	deviceCodeBytes := make([]byte, 32)
	if _, err := rand.Read(deviceCodeBytes); err != nil {
		t.Fatal(err)
	}
	return &DeviceAuthorization{
		DeviceCodeSha256:     sha256.Sum256(deviceCodeBytes),
		UserCode:             userCode,
		ClientId:             UrcliClientId,
		Scopes:               []string{UrcliScopeAuthKeys},
		Resource:             testUrcliApiOrigin,
		DpopJkt:              "synthetic-thumbprint",
		AuthorizationDetails: `[{"type":"synthetic"}]`,
		Cmd:                  "synthetic-cmd",
		ExpireTime:           now.Add(DefaultDeviceGrantSettings().CodeDuration),
		Interval:             DefaultDeviceGrantSettings().PollInterval,
	}
}

// Whether two authorizations are the same authorization, versions aside.
func testAssertDeviceAuthorizationEqual(t testing.TB, authorization *DeviceAuthorization, expected *DeviceAuthorization) {
	t.Helper()
	if authorization == nil {
		t.Fatal("no authorization was answered")
	}
	if authorization.DeviceCodeSha256 != expected.DeviceCodeSha256 || authorization.UserCode != expected.UserCode ||
		authorization.ClientId != expected.ClientId || strings.Join(authorization.Scopes, " ") != strings.Join(expected.Scopes, " ") ||
		authorization.Resource != expected.Resource || authorization.DpopJkt != expected.DpopJkt ||
		authorization.AuthorizationDetails != expected.AuthorizationDetails || authorization.Cmd != expected.Cmd ||
		!authorization.ExpireTime.Equal(expected.ExpireTime) || authorization.Interval != expected.Interval ||
		!authorization.LastPollTime.Equal(expected.LastPollTime) || authorization.Approved != expected.Approved ||
		authorization.Denied != expected.Denied || authorization.UserId != expected.UserId ||
		authorization.NetworkId != expected.NetworkId || !authorization.AuthTime.Equal(expected.AuthTime) {
		t.Fatalf("the authorization answered is not the one kept: %+v", authorization)
	}
}

// The value and remaining life of a key, nil for no key.
func testRedisKeyState(t testing.TB, ctx context.Context, key string) (value []byte, ttl time.Duration) {
	t.Helper()
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		value, err = r.Get(ctx, key).Bytes()
		if errors.Is(err, server.RedisNil) {
			value = nil
			return
		}
		server.Raise(err)
		ttl, err = r.PTTL(ctx, key).Result()
		server.Raise(err)
	})
	return
}

// The keys are the brief's, and an authorization waits as JSON of api-11's fields under its device code's hash, with
// a ttl of its remaining life, its user code pointing at it with the same ttl; both reads answer it whole, from
// another instance too.
func TestRedisDeviceAuthorizationStoreRoundTrip(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultDeviceGrantSettings()
		store := NewRedisDeviceAuthorizationStore(settings)
		authorization := testRedisDeviceAuthorization(t, "BCDFGHJK", server.NowUtc())
		hash := hex.EncodeToString(authorization.DeviceCodeSha256[:])
		if key := redisDeviceAuthorizationKey(authorization.DeviceCodeSha256); key != "{oauthdev_"+hash+"}" {
			t.Fatalf("the authorization's key is %s", key)
		}
		if key := redisDeviceUserCodeKey("BCDFGHJK"); key != "{oauthuser_BCDFGHJK}" {
			t.Fatalf("the user code's key is %s", key)
		}
		if settings.CodeDuration != 10*time.Minute {
			t.Fatalf("the default life is %s", settings.CodeDuration)
		}
		if err := store.Put(ctx, authorization); err != nil {
			t.Fatal(err)
		}

		value, ttl := testRedisKeyState(t, ctx, redisDeviceAuthorizationKey(authorization.DeviceCodeSha256))
		if value == nil || ttl <= 0 || settings.CodeDuration < ttl {
			t.Fatalf("the authorization's key holds %d bytes with %s to live", len(value), ttl)
		}
		fields := map[string]any{}
		if err := json.Unmarshal(value, &fields); err != nil {
			t.Fatal(err)
		}
		names := []string{"device_code_sha256", "user_code", "client_id", "scopes", "resource", "dpop_jkt", "authorization_details", "cmd", "expire_time", "interval_ms"}
		for _, name := range names {
			if _, ok := fields[name]; !ok {
				t.Errorf("the stored authorization has no %s", name)
			}
		}
		// nothing of a poll or an answer before either
		if len(fields) != len(names) {
			t.Errorf("the stored authorization has %d fields", len(fields))
		}
		if fields["device_code_sha256"] != hash || fields["interval_ms"] != float64(5000) {
			t.Errorf("the stored authorization names %v, interval %v", fields["device_code_sha256"], fields["interval_ms"])
		}
		pointer, pointerTtl := testRedisKeyState(t, ctx, redisDeviceUserCodeKey(authorization.UserCode))
		if string(pointer) != hash || pointerTtl <= 0 || settings.CodeDuration < pointerTtl {
			t.Fatalf("the user code's key holds %q with %s to live", pointer, pointerTtl)
		}

		for _, reader := range []*RedisDeviceAuthorizationStore{store, NewRedisDeviceAuthorizationStore(settings)} {
			read, err := reader.Get(ctx, authorization.DeviceCodeSha256)
			if err != nil {
				t.Fatal(err)
			}
			testAssertDeviceAuthorizationEqual(t, read, authorization)
			read, err = reader.GetByUserCode(ctx, authorization.UserCode)
			if err != nil {
				t.Fatal(err)
			}
			testAssertDeviceAuthorizationEqual(t, read, authorization)
		}
		if read, err := store.Get(ctx, sha256.Sum256([]byte("another"))); read != nil || err != nil {
			t.Fatalf("a device code never kept was answered %+v %v", read, err)
		}
		if read, err := store.GetByUserCode(ctx, "ZZZZZZZZ"); read != nil || err != nil {
			t.Fatalf("a user code never kept was answered %+v %v", read, err)
		}
	})
}

// The consent page's answer rewrites the authorization keeping its ttl, and the poll's bookkeeping its last poll and
// interval; a swap from a value another write replaced writes nothing, so neither undoes the other.
func TestRedisDeviceAuthorizationStoreSwapKeepsTheTtl(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		store := NewRedisDeviceAuthorizationStore(DefaultDeviceGrantSettings())
		now := server.NowUtc()
		authorization := testRedisDeviceAuthorization(t, "BCDFGHJK", now)
		if err := store.Put(ctx, authorization); err != nil {
			t.Fatal(err)
		}
		key := redisDeviceAuthorizationKey(authorization.DeviceCodeSha256)
		// a ttl Redis would not reach on its own in the test's time, so a swap that kept it is told from one that reset it
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.PExpire(ctx, key, time.Minute).Err())
		})

		polled, err := store.Get(ctx, authorization.DeviceCodeSha256)
		if err != nil {
			t.Fatal(err)
		}
		// the consent page reads the same authorization before the poll writes
		answered, err := store.GetByUserCode(ctx, authorization.UserCode)
		if err != nil {
			t.Fatal(err)
		}
		poll := *polled
		poll.LastPollTime = now.Add(time.Second)
		poll.Interval += DefaultDeviceGrantSettings().SlowDownStep
		if swapped, err := store.Swap(ctx, polled, &poll); !swapped || err != nil {
			t.Fatalf("the poll's swap: %t %v", swapped, err)
		}
		_, ttl := testRedisKeyState(t, ctx, key)
		if ttl <= 0 || time.Minute < ttl {
			t.Fatalf("the poll's swap left %s to live", ttl)
		}

		approval := *answered
		approval.Approved = true
		approval.UserId = server.NewId()
		approval.NetworkId = server.NewId()
		approval.AuthTime = now
		if swapped, err := store.Swap(ctx, answered, &approval); swapped || err != nil {
			t.Fatalf("an answer from a read the poll replaced: %t %v", swapped, err)
		}
		read, err := store.Get(ctx, authorization.DeviceCodeSha256)
		if err != nil {
			t.Fatal(err)
		}
		testAssertDeviceAuthorizationEqual(t, read, &poll)

		// read again, the answer lands on the poll's bookkeeping
		approval = *read
		approval.Approved = true
		approval.UserId = server.NewId()
		approval.NetworkId = server.NewId()
		approval.AuthTime = now
		if swapped, err := store.Swap(ctx, read, &approval); !swapped || err != nil {
			t.Fatalf("the answer read again: %t %v", swapped, err)
		}
		value, ttl := testRedisKeyState(t, ctx, key)
		if ttl <= 0 || time.Minute < ttl {
			t.Fatalf("the answer's swap left %s to live", ttl)
		}
		fields := map[string]any{}
		if err := json.Unmarshal(value, &fields); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"approved", "user_id", "network_id", "auth_time", "last_poll_time"} {
			if _, ok := fields[name]; !ok {
				t.Errorf("the approved authorization has no %s", name)
			}
		}
		read, err = NewRedisDeviceAuthorizationStore(DefaultDeviceGrantSettings()).GetByUserCode(ctx, authorization.UserCode)
		if err != nil {
			t.Fatal(err)
		}
		testAssertDeviceAuthorizationEqual(t, read, &approval)
		// the poll's stale read writes nothing over the answer
		if swapped, err := store.Swap(ctx, polled, &poll); swapped || err != nil {
			t.Fatalf("a poll from a read the answer replaced: %t %v", swapped, err)
		}
	})
}

// A take is answered the authorization once, removing its key and its user code's pointer; every take after it,
// at any instance, is answered nothing; and of many takes released together exactly one is answered it.
func TestRedisDeviceAuthorizationStoreTakesOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultDeviceGrantSettings()
		store := NewRedisDeviceAuthorizationStore(settings)
		authorization := testRedisDeviceAuthorization(t, "BCDFGHJK", server.NowUtc())
		if err := store.Put(ctx, authorization); err != nil {
			t.Fatal(err)
		}
		taken, err := store.Take(ctx, authorization.DeviceCodeSha256)
		if err != nil {
			t.Fatal(err)
		}
		testAssertDeviceAuthorizationEqual(t, taken, authorization)
		if value, _ := testRedisKeyState(t, ctx, redisDeviceAuthorizationKey(authorization.DeviceCodeSha256)); value != nil {
			t.Fatal("the take left the authorization's key")
		}
		if value, _ := testRedisKeyState(t, ctx, redisDeviceUserCodeKey(authorization.UserCode)); value != nil {
			t.Fatal("the take left the user code's pointer")
		}
		for _, reader := range []*RedisDeviceAuthorizationStore{store, NewRedisDeviceAuthorizationStore(settings)} {
			if taken, err := reader.Take(ctx, authorization.DeviceCodeSha256); taken != nil || err != nil {
				t.Fatalf("a second take was answered %+v %v", taken, err)
			}
			if read, err := reader.GetByUserCode(ctx, authorization.UserCode); read != nil || err != nil {
				t.Fatalf("a taken authorization's user code was answered %+v %v", read, err)
			}
		}

		contested := testRedisDeviceAuthorization(t, "CDFGHJKL", server.NowUtc())
		if err := store.Put(ctx, contested); err != nil {
			t.Fatal(err)
		}
		takeCount := 24
		takens := make([]*DeviceAuthorization, takeCount)
		errs := make([]error, takeCount)
		release := make(chan struct{})
		var ready sync.WaitGroup
		var done sync.WaitGroup
		for i := range takeCount {
			ready.Add(1)
			done.Add(1)
			go func() {
				defer done.Done()
				instance := NewRedisDeviceAuthorizationStore(settings)
				ready.Done()
				<-release
				takens[i], errs[i] = instance.Take(ctx, contested.DeviceCodeSha256)
			}()
		}
		ready.Wait()
		close(release)
		done.Wait()
		answeredCount := 0
		for i := range takeCount {
			if errs[i] != nil {
				t.Fatalf("take %d: %v", i, errs[i])
			}
			if takens[i] != nil {
				answeredCount += 1
				testAssertDeviceAuthorizationEqual(t, takens[i], contested)
			}
		}
		if answeredCount != 1 {
			t.Fatalf("%d of %d concurrent takes were answered the authorization", answeredCount, takeCount)
		}
	})
}

// On the store's clock an authorization is answered until its life ends and never from then on; a key whose Redis
// expiry passed is gone; and the poll answers expired_token for either, the user code finding nothing.
func TestRedisDeviceAuthorizationStoreExpires(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		installUrcliTest(t)
		defer SetDeviceAuthorizationStore(nil)
		ctx := t.Context()
		settings := DefaultDeviceGrantSettings()
		store := NewRedisDeviceAuthorizationStore(settings)
		// Redis's own clock is the real one, so the fake starts there
		start := server.NowUtc()
		now := start
		store.Testing_SetNow(func() time.Time { return now })
		authorization := testRedisDeviceAuthorization(t, "BCDFGHJK", start)
		if err := store.Put(ctx, authorization); err != nil {
			t.Fatal(err)
		}
		now = start.Add(settings.CodeDuration - time.Microsecond)
		read, err := store.Get(ctx, authorization.DeviceCodeSha256)
		if err != nil {
			t.Fatal(err)
		}
		testAssertDeviceAuthorizationEqual(t, read, authorization)
		now = start.Add(settings.CodeDuration)
		if read, err := store.Get(ctx, authorization.DeviceCodeSha256); read != nil || err != nil {
			t.Fatalf("an authorization at the end of its life was answered %+v %v", read, err)
		}
		if read, err := store.GetByUserCode(ctx, authorization.UserCode); read != nil || err != nil {
			t.Fatalf("its user code was answered %+v %v", read, err)
		}
		if taken, err := store.Take(ctx, authorization.DeviceCodeSha256); taken != nil || err != nil {
			t.Fatalf("its take was answered %+v %v", taken, err)
		}
		now = start
		if read, err := store.Get(ctx, authorization.DeviceCodeSha256); read != nil || err != nil {
			t.Fatalf("an expired authorization came back %+v %v", read, err)
		}

		// through the grant: a code whose key Redis expired answers expired_token, and its user code nothing
		SetDeviceAuthorizationStore(NewRedisDeviceAuthorizationStore(settings))
		device := startTestDeviceLogin(t, ctx, testUrcliCommand(), server.NowUtc())
		if _, err := device.poll(ctx, server.NowUtc(), device.proof()); !errors.Is(err, ErrAuthorizationPending) {
			t.Fatalf("the first poll: %v", err)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			// an expiry in the past is one Redis applies at once
			server.Raise(r.ExpireAt(ctx, redisDeviceAuthorizationKey(device.deviceCodeSha256()), time.Unix(1, 0)).Err())
			server.Raise(r.ExpireAt(ctx, redisDeviceUserCodeKey(strings.ReplaceAll(device.response.UserCode, "-", "")), time.Unix(1, 0)).Err())
		})
		if _, err := device.poll(ctx, server.NowUtc().Add(time.Minute), device.proof()); !errors.Is(err, ErrExpiredToken) {
			t.Fatalf("a poll after Redis expired the key: %v", err)
		}
		user := newTestUrcliUser(t, ctx)
		if _, err := describeDeviceConsentAt(ctx, device.response.UserCode, user.UserId, user.AuthTime, settings, server.NowUtc()); !errors.Is(err, ErrInvalidGrant) {
			t.Fatalf("the consent page after Redis expired the key: %v", err)
		}
	})
}

// A user code's pointer names one live authorization: a second authorization under a code in use is refused with
// ErrDeviceUserCodeInUse and nothing of it is kept, the first standing; a code that is no stored code reaches no key;
// and a pointer at another code's authorization, or one that does not read, is an error.
func TestRedisDeviceAuthorizationStoreUserCodePointer(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultDeviceGrantSettings()
		store := NewRedisDeviceAuthorizationStore(settings)
		first := testRedisDeviceAuthorization(t, "BCDFGHJK", server.NowUtc())
		if err := store.Put(ctx, first); err != nil {
			t.Fatal(err)
		}
		second := testRedisDeviceAuthorization(t, first.UserCode, server.NowUtc())
		if err := store.Put(ctx, second); !errors.Is(err, ErrDeviceUserCodeInUse) {
			t.Fatalf("a user code in use: %v", err)
		}
		if value, _ := testRedisKeyState(t, ctx, redisDeviceAuthorizationKey(second.DeviceCodeSha256)); value != nil {
			t.Fatal("the refused authorization was kept")
		}
		read, err := store.GetByUserCode(ctx, first.UserCode)
		if err != nil {
			t.Fatal(err)
		}
		testAssertDeviceAuthorizationEqual(t, read, first)

		for _, userCode := range []string{"", "bcdfghjk", "BCDF-GHJK", "BCDFGHJ", "BCDFGHJKL", "AEIOUAEI", "BCDF}GHJ", "{oauthus"} {
			if read, err := store.GetByUserCode(ctx, userCode); read != nil || err != nil {
				t.Errorf("the user code %q was answered %+v %v", userCode, read, err)
			}
			refused := testRedisDeviceAuthorization(t, userCode, server.NowUtc())
			if err := store.Put(ctx, refused); err == nil {
				t.Errorf("an authorization was kept under the user code %q", userCode)
			}
		}

		// a pointer at an authorization of another code
		other := testRedisDeviceAuthorization(t, "CDFGHJKL", server.NowUtc())
		if err := store.Put(ctx, other); err != nil {
			t.Fatal(err)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, redisDeviceUserCodeKey("DFGHJKLM"), hex.EncodeToString(other.DeviceCodeSha256[:]), time.Minute).Err())
			server.Raise(r.Set(ctx, redisDeviceUserCodeKey("FGHJKLMN"), "not hex", time.Minute).Err())
			server.Raise(r.Set(ctx, redisDeviceUserCodeKey("GHJKLMNP"), strings.Repeat("ab", 64), time.Minute).Err())
		})
		for _, userCode := range []string{"DFGHJKLM", "FGHJKLMN", "GHJKLMNP"} {
			if read, err := store.GetByUserCode(ctx, userCode); read != nil || err == nil {
				t.Errorf("the pointer of %s was answered %+v %v", userCode, read, err)
			}
		}
	})
}

// What the store refuses: an authorization with no life ahead of it, which Redis would keep forever, or none at all;
// a device code in use; a swap from an authorization the store never answered, or one changing the device code, the
// user code or the life; and a value under an authorization's key that is not that code's authorization, answered as
// an error and gone after a take.
func TestRedisDeviceAuthorizationStoreRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultDeviceGrantSettings()
		store := NewRedisDeviceAuthorizationStore(settings)
		now := server.NowUtc()

		for _, expireTime := range []time.Time{now.Add(-time.Second), {}} {
			authorization := testRedisDeviceAuthorization(t, "BCDFGHJK", now)
			authorization.ExpireTime = expireTime
			if err := store.Put(ctx, authorization); err == nil {
				t.Errorf("an authorization was kept with the life %s", expireTime)
			}
			if value, _ := testRedisKeyState(t, ctx, redisDeviceAuthorizationKey(authorization.DeviceCodeSha256)); value != nil {
				t.Error("a refused authorization left a key")
			}
		}
		if err := store.Put(ctx, nil); err == nil {
			t.Error("no authorization was kept")
		}

		authorization := testRedisDeviceAuthorization(t, "BCDFGHJK", now)
		if err := store.Put(ctx, authorization); err != nil {
			t.Fatal(err)
		}
		again := *authorization
		again.UserCode = "CDFGHJKL"
		if err := store.Put(ctx, &again); err == nil || errors.Is(err, ErrDeviceUserCodeInUse) {
			t.Errorf("a device code in use: %v", err)
		}
		if value, _ := testRedisKeyState(t, ctx, redisDeviceUserCodeKey(again.UserCode)); value != nil {
			t.Error("a refused device code left a user code's pointer")
		}

		unread := *authorization
		if swapped, err := store.Swap(ctx, authorization, &unread); swapped || err == nil {
			t.Errorf("a swap from an authorization never read: %t %v", swapped, err)
		}
		read, err := store.Get(ctx, authorization.DeviceCodeSha256)
		if err != nil {
			t.Fatal(err)
		}
		for _, change := range []func(*DeviceAuthorization){
			func(next *DeviceAuthorization) { next.DeviceCodeSha256 = sha256.Sum256([]byte("another")) },
			func(next *DeviceAuthorization) { next.UserCode = "CDFGHJKL" },
			func(next *DeviceAuthorization) { next.ExpireTime = next.ExpireTime.Add(time.Hour) },
		} {
			next := *read
			change(&next)
			if swapped, err := store.Swap(ctx, read, &next); swapped || err == nil {
				t.Errorf("a swap changing what it keeps: %t %v", swapped, err)
			}
		}
		current, err := store.Get(ctx, authorization.DeviceCodeSha256)
		if err != nil {
			t.Fatal(err)
		}
		testAssertDeviceAuthorizationEqual(t, current, authorization)

		otherJson, err := encodeRedisDeviceAuthorization(testRedisDeviceAuthorization(t, "DFGHJKLM", now))
		if err != nil {
			t.Fatal(err)
		}
		noUserCode := testRedisDeviceAuthorization(t, "lowercase", now)
		for _, value := range [][]byte{[]byte("not json"), otherJson} {
			foreign := testRedisDeviceAuthorization(t, "FGHJKLMN", now)
			key := redisDeviceAuthorizationKey(foreign.DeviceCodeSha256)
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.Set(ctx, key, value, time.Minute).Err())
			})
			if read, err := store.Get(ctx, foreign.DeviceCodeSha256); read != nil || err == nil {
				t.Errorf("a foreign value was answered %+v %v", read, err)
			}
			if taken, err := store.Take(ctx, foreign.DeviceCodeSha256); taken != nil || err == nil {
				t.Errorf("a foreign value was taken %+v %v", taken, err)
			}
			if value, _ := testRedisKeyState(t, ctx, key); value != nil {
				t.Error("a foreign value stayed under the key after its take")
			}
		}
		noUserCodeJson, err := encodeRedisDeviceAuthorization(noUserCode)
		if err != nil {
			t.Fatal(err)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, redisDeviceAuthorizationKey(noUserCode.DeviceCodeSha256), noUserCodeJson, time.Minute).Err())
		})
		if read, err := store.Get(ctx, noUserCode.DeviceCodeSha256); read != nil || err == nil {
			t.Errorf("an authorization with no user code was answered %+v %v", read, err)
		}
	})
}
