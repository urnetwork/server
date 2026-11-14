// The verified claims cache (by_jwt_verified_cache.go), driven by an explicit
// clock and a synthetic vault (by_jwt_keyring_test.go). Each verdict case
// parses the same token with a keyring that remembers verifications and one
// that does not, over the same keys, and compares them: a valid token, a
// tampered one, an expired one under both gates, a rotated key, a dropped key,
// and concurrent parses. The revoked-session and client-state cases need the
// local stores (by_jwt_verified_cache_db_test.go).
package session

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/v2026"
)

// A keyring over the synthetic vault that remembers verifications as the
// process keyring does.
func newByJwtTestCachedKeyring(vault *byJwtTestVault, clock *byJwtTestClock) *byJwtKeyring {
	keyring := newByJwtTestKeyring(vault, clock)
	defaults := defaultByJwtKeyringSettings()
	keyring.settings.verifiedCacheMaxEntryCount = defaults.verifiedCacheMaxEntryCount
	keyring.settings.verifiedCacheMaxEntryAge = defaults.verifiedCacheMaxEntryAge
	keyring.settings.verifiedCacheShardCount = defaults.verifiedCacheShardCount
	keyring.verifiedCache = newByJwtVerifiedCache(keyring.settings)
	return keyring
}

// The verdict of a parse in comparable terms: the error's text, or the claims.
type byJwtTestVerdict struct {
	byJwt *ByJwt
	err   error
}

func (self byJwtTestVerdict) String() string {
	if self.err != nil {
		return "refused: " + self.err.Error()
	}
	return fmt.Sprintf("accepted: %+v", *self.byJwt)
}

func assertByJwtTestSameVerdict(t *testing.T, description string, cached byJwtTestVerdict, uncached byJwtTestVerdict) {
	t.Helper()
	if (cached.err == nil) != (uncached.err == nil) {
		t.Fatalf("%s: cached %s, uncached %s", description, cached, uncached)
	}
	if cached.err != nil {
		if cached.err.Error() != uncached.err.Error() || fmt.Sprintf("%T", cached.err) != fmt.Sprintf("%T", uncached.err) {
			t.Fatalf("%s: cached %s (%T), uncached %s (%T)", description, cached, cached.err, uncached, uncached.err)
		}
		return
	}
	if !reflect.DeepEqual(cached.byJwt, uncached.byJwt) {
		t.Fatalf("%s: cached %s, uncached %s", description, cached, uncached)
	}
}

func parseByJwtTestSigned(keyring *byJwtKeyring, signed string) byJwtTestVerdict {
	byJwt, err := parseByJwtSigned(keyring, signed)
	return byJwtTestVerdict{byJwt: byJwt, err: err}
}

func parseByJwtTestForAudience(keyring *byJwtKeyring, signed string) byJwtTestVerdict {
	byJwt, err := parseByJwtForAudience(context.Background(), keyring, signed, ByJwtAudienceApi)
	return byJwtTestVerdict{byJwt: byJwt, err: err}
}

type byJwtTestCacheCounts struct {
	hitCount   uint64
	missCount  uint64
	storeCount uint64
	evictCount uint64
	entryCount int
}

func byJwtTestCounts(keyring *byJwtKeyring) byJwtTestCacheCounts {
	cache := keyring.verifiedCache
	return byJwtTestCacheCounts{
		hitCount:   cache.hitCount.Load(),
		missCount:  cache.missCount.Load(),
		storeCount: cache.storeCount.Load(),
		evictCount: cache.evictCount.Load(),
		entryCount: cache.entryCount(),
	}
}

func assertByJwtTestCounts(t *testing.T, description string, keyring *byJwtKeyring, want byJwtTestCacheCounts) {
	t.Helper()
	if counts := byJwtTestCounts(keyring); counts != want {
		t.Fatalf("%s: counts = %+v, want %+v", description, counts, want)
	}
}

// A token verified once is answered from the cache on its second parse, with
// the claims a full verification decodes.
func TestByJwtVerifiedCacheSecondParseReusesVerification(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	key := writeByJwtTestKey(t, vault, "1.0.0", clock.time)
	cachedKeyring := newByJwtTestCachedKeyring(vault, clock)
	uncachedKeyring := newByJwtTestKeyring(vault, clock)
	signed := signByJwtWithKey(t, newByJwtTestClaims(), key, true)

	uncached := parseByJwtTestForAudience(uncachedKeyring, signed)
	if uncached.err != nil {
		t.Fatal(uncached.err)
	}
	assertByJwtTestSameVerdict(t, "first parse", parseByJwtTestForAudience(cachedKeyring, signed), uncached)
	assertByJwtTestCounts(t, "first parse", cachedKeyring, byJwtTestCacheCounts{missCount: 1, storeCount: 1, entryCount: 1})
	assertByJwtTestSameVerdict(t, "second parse", parseByJwtTestForAudience(cachedKeyring, signed), uncached)
	assertByJwtTestCounts(t, "second parse", cachedKeyring, byJwtTestCacheCounts{hitCount: 1, missCount: 1, storeCount: 1, entryCount: 1})
}

// The key is the whole token: a token that differs from a remembered one in
// one byte of its signature, its claims or its header is verified in full and
// refused, as without the cache, and nothing is remembered for it.
func TestByJwtVerifiedCacheKeyIsTheWholeToken(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	key := writeByJwtTestKey(t, vault, "1.0.0", clock.time)
	cachedKeyring := newByJwtTestCachedKeyring(vault, clock)
	uncachedKeyring := newByJwtTestKeyring(vault, clock)
	claims := newByJwtTestClaims()
	signed := signByJwtWithKey(t, claims, key, true)
	if verdict := parseByJwtTestForAudience(cachedKeyring, signed); verdict.err != nil {
		t.Fatal(verdict.err)
	}

	parts := strings.Split(signed, ".")
	replaceByte := func(part string, index int) string {
		b := []byte(part)
		if b[index] == 'A' {
			b[index] = 'B'
		} else {
			b[index] = 'A'
		}
		return string(b)
	}
	// claims that name another network, signed by no one
	otherClaims := *claims
	otherClaims.NetworkId = server.NewId()
	forgedPayload := strings.Split(signByJwtWithKey(t, &otherClaims, key, true), ".")[1]
	tampered := []struct {
		description string
		signed      string
	}{
		{description: "signature", signed: parts[0] + "." + parts[1] + "." + replaceByte(parts[2], len(parts[2])/2)},
		{description: "claims", signed: parts[0] + "." + forgedPayload + "." + parts[2]},
		{description: "header", signed: replaceByte(parts[0], 1) + "." + parts[1] + "." + parts[2]},
		{description: "truncated signature", signed: parts[0] + "." + parts[1] + "." + parts[2][:len(parts[2])-2]},
	}
	for _, c := range tampered {
		for range 2 {
			cached := parseByJwtTestForAudience(cachedKeyring, c.signed)
			if cached.err == nil {
				t.Fatalf("%s: a tampered token was accepted", c.description)
			}
			assertByJwtTestSameVerdict(t, c.description, cached, parseByJwtTestForAudience(uncachedKeyring, c.signed))
		}
	}
	counts := byJwtTestCounts(cachedKeyring)
	if counts.hitCount != 0 || counts.storeCount != 1 || counts.entryCount != 1 {
		t.Fatalf("counts = %+v, want no reuse and only the genuine token remembered", counts)
	}
}

// The audience is checked on every request: a token minted for connect alone
// is accepted for connect and refused for the api, from a remembered
// verification as without one, in either order.
func TestByJwtVerifiedCacheChecksTheAudienceOnEveryRequest(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	key := writeByJwtTestKey(t, vault, "1.0.0", clock.time)
	cachedKeyring := newByJwtTestCachedKeyring(vault, clock)
	uncachedKeyring := newByJwtTestKeyring(vault, clock)
	claims := newByJwtTestClaims()
	claims.Audience = gojwt.ClaimStrings{ByJwtAudienceConnect}
	signed := signByJwtWithKey(t, claims, key, true)
	parse := func(keyring *byJwtKeyring, audience string) byJwtTestVerdict {
		byJwt, err := parseByJwtForAudience(context.Background(), keyring, signed, audience)
		return byJwtTestVerdict{byJwt: byJwt, err: err}
	}

	for _, audience := range []string{ByJwtAudienceConnect, ByJwtAudienceApi, ByJwtAudienceConnect, ByJwtAudienceApi} {
		cached := parse(cachedKeyring, audience)
		if (cached.err == nil) != (audience == ByJwtAudienceConnect) {
			t.Fatalf("%s: %s", audience, cached)
		}
		assertByJwtTestSameVerdict(t, audience, cached, parse(uncachedKeyring, audience))
	}
	assertByJwtTestCounts(t, "audiences", cachedKeyring, byJwtTestCacheCounts{hitCount: 3, missCount: 1, storeCount: 1, entryCount: 1})
}

// Only successes are remembered: a token signed with a key the keyring does
// not hold yet is refused, and verifies once the vault gains the key and a
// reread reads it; a malformed token and a token in a refused signing method
// are refused every time.
func TestByJwtVerifiedCacheRemembersOnlySuccesses(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	writeByJwtTestKey(t, vault, "1.0.0", clock.time)
	cachedKeyring := newByJwtTestCachedKeyring(vault, clock)
	cachedKeyring.heldKeys()

	// a version the vault does not hold yet: written elsewhere first
	elsewhere := newByJwtTestVault(t)
	pendingKey := writeByJwtTestKey(t, elsewhere, "2.0.0", clock.time)
	signed := signByJwtWithKey(t, newByJwtTestClaims(), pendingKey, true)
	for range 2 {
		if verdict := parseByJwtTestSigned(cachedKeyring, signed); verdict.err == nil {
			t.Fatal("a token signed with a key the vault does not hold verified")
		}
	}
	writeByJwtKeyFile(t, byJwtRotationKeyFile(vault.root, "2.0.0"), pendingKey)
	clock.time = clock.time.Add(byJwtTestRereadInterval)
	if verdict := parseByJwtTestSigned(cachedKeyring, signed); verdict.err != nil {
		t.Fatalf("the token after the vault gained its key: %s", verdict.err)
	}
	if verdict := parseByJwtTestSigned(cachedKeyring, signed); verdict.err != nil {
		t.Fatal(verdict.err)
	}
	assertByJwtTestCounts(t, "pending key", cachedKeyring, byJwtTestCacheCounts{hitCount: 1, missCount: 3, storeCount: 1, entryCount: 1})

	noneSigned, err := gojwt.NewWithClaims(gojwt.SigningMethodNone, newByJwtTestClaims()).SignedString(gojwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	for _, refused := range []string{"not.a.token", "", noneSigned} {
		for range 2 {
			if verdict := parseByJwtTestSigned(cachedKeyring, refused); verdict.err == nil {
				t.Fatalf("%q verified", refused)
			}
		}
	}
	if counts := byJwtTestCounts(cachedKeyring); counts.storeCount != 1 || counts.hitCount != 1 {
		t.Fatalf("counts = %+v, want no refusal remembered", counts)
	}
}

// An entry answers until the token's expiration, on the keyring's clock, and
// not after; a token already past its expiration or without one is not
// remembered.
func TestByJwtVerifiedCacheEndsAtTheTokenExpiration(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	key := writeByJwtTestKey(t, vault, "1.0.0", clock.time)
	cachedKeyring := newByJwtTestCachedKeyring(vault, clock)
	startTime := clock.time

	claims := newByJwtTestClaims()
	claims.ExpiresAt = gojwt.NewNumericDate(startTime.Add(5 * time.Minute))
	signed := signByJwtWithKey(t, claims, key, true)
	parseByJwtTestSigned(cachedKeyring, signed)
	clock.time = startTime.Add(5*time.Minute - time.Second)
	parseByJwtTestSigned(cachedKeyring, signed)
	assertByJwtTestCounts(t, "before the expiration", cachedKeyring, byJwtTestCacheCounts{hitCount: 1, missCount: 1, storeCount: 1, entryCount: 1})
	clock.time = startTime.Add(5 * time.Minute)
	if verdict := parseByJwtTestSigned(cachedKeyring, signed); verdict.err != nil {
		t.Fatalf("the signature of an expired token: %s", verdict.err)
	}
	// verified in full, and not remembered again
	assertByJwtTestCounts(t, "at the expiration", cachedKeyring, byJwtTestCacheCounts{hitCount: 1, missCount: 2, storeCount: 1, entryCount: 0})

	expired := newByJwtTestClaims()
	expired.ExpiresAt = gojwt.NewNumericDate(clock.time.Add(-time.Minute))
	missing := newByJwtTestClaims()
	missing.ExpiresAt = nil
	for _, unremembered := range []*ByJwt{expired, missing} {
		unrememberedSigned := signByJwtWithKey(t, unremembered, key, true)
		for range 2 {
			if verdict := parseByJwtTestSigned(cachedKeyring, unrememberedSigned); verdict.err != nil {
				t.Fatal(verdict.err)
			}
		}
	}
	assertByJwtTestCounts(t, "expired and missing", cachedKeyring, byJwtTestCacheCounts{hitCount: 1, missCount: 6, storeCount: 1, entryCount: 0})
}

// An expired token gets the uncached verdict under both settings of the
// reject_expired gate, counted as a legacy accept each time it is accepted;
// the expiration is judged on every request, cached or not.
func TestByJwtVerifiedCacheExpiredTokenVerdicts(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	key := writeByJwtTestKey(t, vault, "1.0.0", clock.time)
	cachedKeyring := newByJwtTestCachedKeyring(vault, clock)
	uncachedKeyring := newByJwtTestKeyring(vault, clock)
	// remembered while the keyring's clock is before its expiration; the
	// claims checks judge it on the server's clock, which is past it
	claims := newByJwtTestClaims()
	claims.ExpiresAt = gojwt.NewNumericDate(server.NowUtc().Add(-time.Hour))
	clock.time = claims.ExpiresAt.Time.Add(-time.Hour)
	signed := signByJwtWithKey(t, claims, key, true)

	for _, rejectExpired := range []bool{true, false} {
		func() {
			defer Testing_SetRejectExpired(rejectExpired)()
			defer Testing_SetRejectMissingExpiration(false)()
			for attempt := range 2 {
				description := fmt.Sprintf("reject_expired=%t attempt %d", rejectExpired, attempt)
				before := legacyAcceptCount(AuthLegacyExpired, AuthCredentialNetwork)
				cached := parseByJwtTestForAudience(cachedKeyring, signed)
				cachedLegacy := legacyAcceptCount(AuthLegacyExpired, AuthCredentialNetwork) - before
				uncached := parseByJwtTestForAudience(uncachedKeyring, signed)
				uncachedLegacy := legacyAcceptCount(AuthLegacyExpired, AuthCredentialNetwork) - before - cachedLegacy
				assertByJwtTestSameVerdict(t, description, cached, uncached)
				if (cached.err == nil) == rejectExpired {
					t.Fatalf("%s: %s", description, cached)
				}
				if cachedLegacy != uncachedLegacy || cachedLegacy != map[bool]float64{true: 0, false: 1}[rejectExpired] {
					t.Fatalf("%s: legacy accepts cached %.0f uncached %.0f", description, cachedLegacy, uncachedLegacy)
				}
			}
		}()
	}
	if counts := byJwtTestCounts(cachedKeyring); counts.hitCount != 3 || counts.storeCount != 1 {
		t.Fatalf("counts = %+v, want the expired token's signature reused", counts)
	}
}

// An entry answers for the maximum age after it was stored, and is verified
// in full and remembered again after it.
func TestByJwtVerifiedCacheEndsAtTheMaximumAge(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	key := writeByJwtTestKey(t, vault, "1.0.0", clock.time)
	cachedKeyring := newByJwtTestCachedKeyring(vault, clock)
	maxEntryAge := cachedKeyring.settings.verifiedCacheMaxEntryAge
	startTime := clock.time
	claims := newByJwtTestClaims()
	claims.ExpiresAt = gojwt.NewNumericDate(startTime.Add(24 * time.Hour))
	signed := signByJwtWithKey(t, claims, key, true)

	parseByJwtTestSigned(cachedKeyring, signed)
	clock.time = startTime.Add(maxEntryAge - time.Nanosecond)
	parseByJwtTestSigned(cachedKeyring, signed)
	clock.time = startTime.Add(maxEntryAge)
	parseByJwtTestSigned(cachedKeyring, signed)
	parseByJwtTestSigned(cachedKeyring, signed)
	assertByJwtTestCounts(t, "maximum age", cachedKeyring, byJwtTestCacheCounts{hitCount: 2, missCount: 2, storeCount: 2, entryCount: 1})
}

// A rotation that adds a key version ends no entry: the previous version's
// tokens keep verifying, from the cache, as they do without it, and the new
// version's tokens verify after the reread.
func TestByJwtVerifiedCacheKeepsEntriesAcrossRotation(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	previousKey := writeByJwtTestKey(t, vault, "1.0.0", clock.time)
	cachedKeyring := newByJwtTestCachedKeyring(vault, clock)
	uncachedKeyring := newByJwtTestKeyring(vault, clock)
	previousSigned := signByJwtWithKey(t, newByJwtTestClaims(), previousKey, true)
	assertByJwtTestSameVerdict(t, "previous version", parseByJwtTestSigned(cachedKeyring, previousSigned), parseByJwtTestSigned(uncachedKeyring, previousSigned))

	rotatedKey := writeByJwtTestKey(t, vault, "2.0.0", clock.time)
	rotatedSigned := signByJwtWithKey(t, newByJwtTestClaims(), rotatedKey, true)
	for range 2 {
		assertByJwtTestSameVerdict(t, "rotated version", parseByJwtTestSigned(cachedKeyring, rotatedSigned), parseByJwtTestSigned(uncachedKeyring, rotatedSigned))
		assertByJwtTestSameVerdict(t, "previous version after the rotation", parseByJwtTestSigned(cachedKeyring, previousSigned), parseByJwtTestSigned(uncachedKeyring, previousSigned))
	}
	if keys := cachedKeyring.heldKeys(); len(keys.keys) != 2 || keys.removalGeneration != 0 {
		t.Fatalf("held %d keys at generation %d, want both at generation 0", len(keys.keys), keys.removalGeneration)
	}
	assertByJwtTestCounts(t, "rotation", cachedKeyring, byJwtTestCacheCounts{hitCount: 3, missCount: 2, storeCount: 2, entryCount: 2})
}

// The held keys without the key of kid, as a reread that drops a key would
// install them.
func byJwtTestKeysWithout(keys *byJwtKeys, kid string) *byJwtKeys {
	dropped := &byJwtKeys{
		kidPublicKeys:         map[string]crypto.PublicKey{},
		pathKeys:              map[string]*byJwtKey{},
		pathFailedModifyTimes: map[string]time.Time{},
	}
	for _, key := range keys.keys {
		if key.kid == kid {
			continue
		}
		dropped.keys = append(dropped.keys, key)
		dropped.kidPublicKeys[key.kid] = key.publicKey
		dropped.pathKeys[key.path] = key
		dropped.verificationKeys = append(dropped.verificationKeys, key.publicKey)
	}
	return dropped
}

// A key set that drops a held key ends every remembered verification: a token
// of the dropped key is refused as the keyring without the cache refuses it,
// and a token of a kept key is verified in full again.
func TestByJwtVerifiedCacheEndsEntriesWhenAKeyIsDropped(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	droppedKey := writeByJwtTestKey(t, vault, "1.0.0", clock.time)
	keptKey := writeByJwtTestKey(t, vault, "2.0.0", clock.time)
	cachedKeyring := newByJwtTestCachedKeyring(vault, clock)
	uncachedKeyring := newByJwtTestKeyring(vault, clock)
	droppedSigned := signByJwtWithKey(t, newByJwtTestClaims(), droppedKey, true)
	keptSigned := signByJwtWithKey(t, newByJwtTestClaims(), keptKey, true)
	for _, signed := range []string{droppedSigned, keptSigned} {
		assertByJwtTestSameVerdict(t, "before the drop", parseByJwtTestSigned(cachedKeyring, signed), parseByJwtTestSigned(uncachedKeyring, signed))
	}

	// the vault no longer holds it, so no reread gains it back
	if err := os.RemoveAll(byJwtRotationKeyFile(vault.root, "1.0.0")); err != nil {
		t.Fatal(err)
	}
	for _, keyring := range []*byJwtKeyring{cachedKeyring, uncachedKeyring} {
		keyring.setKeys(byJwtTestKeysWithout(keyring.heldKeys(), byJwtTestKid(t, droppedKey)))
	}
	if generation := cachedKeyring.heldKeys().removalGeneration; generation != 1 {
		t.Fatalf("removal generation = %d, want 1", generation)
	}
	cached := parseByJwtTestSigned(cachedKeyring, droppedSigned)
	if cached.err == nil {
		t.Fatal("a token of the dropped key was accepted")
	}
	assertByJwtTestSameVerdict(t, "dropped key", cached, parseByJwtTestSigned(uncachedKeyring, droppedSigned))
	assertByJwtTestSameVerdict(t, "kept key", parseByJwtTestSigned(cachedKeyring, keptSigned), parseByJwtTestSigned(uncachedKeyring, keptSigned))
	assertByJwtTestCounts(t, "after the drop", cachedKeyring, byJwtTestCacheCounts{missCount: 4, storeCount: 3, entryCount: 1})
}

// Sets every exported field reachable from value, through embedded structs,
// to a value that is not its zero value. salt varies the values.
func fillByJwtTestValue(t *testing.T, value reflect.Value, salt int) {
	t.Helper()
	switch value.Kind() {
	case reflect.Pointer:
		value.Set(reflect.New(value.Type().Elem()))
		fillByJwtTestValue(t, value.Elem(), salt)
	case reflect.Struct:
		if value.Type() == reflect.TypeOf(time.Time{}) {
			value.Set(reflect.ValueOf(time.Unix(int64(1700000000+salt), 0).UTC()))
			return
		}
		for i := range value.NumField() {
			if value.Type().Field(i).IsExported() {
				fillByJwtTestValue(t, value.Field(i), salt)
			}
		}
	case reflect.Slice:
		value.Set(reflect.MakeSlice(value.Type(), 2, 2))
		for i := range 2 {
			fillByJwtTestValue(t, value.Index(i), salt+i)
		}
	case reflect.Array:
		for i := range value.Len() {
			fillByJwtTestValue(t, value.Index(i), salt+i)
		}
	case reflect.String:
		value.SetString(fmt.Sprintf("value-%d", salt))
	case reflect.Bool:
		value.SetBool(salt%2 == 0)
	case reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uint:
		value.SetUint(uint64(salt%200 + 1))
	case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Int:
		value.SetInt(int64(salt%100 + 1))
	default:
		t.Fatalf("no test value for %s", value.Type())
	}
}

// Fails where copy shares a pointer, slice or map with original, anywhere in
// their exported fields. A time's location is an immutable value.
func assertByJwtTestNothingShared(t *testing.T, path string, original reflect.Value, copy reflect.Value) {
	t.Helper()
	switch original.Kind() {
	case reflect.Pointer:
		if original.IsNil() {
			return
		}
		if original.Pointer() == copy.Pointer() {
			t.Errorf("%s is shared", path)
		}
		assertByJwtTestNothingShared(t, path, original.Elem(), copy.Elem())
	case reflect.Slice:
		if original.Len() == 0 {
			return
		}
		if original.Pointer() == copy.Pointer() {
			t.Errorf("%s is shared", path)
		}
		for i := range original.Len() {
			assertByJwtTestNothingShared(t, fmt.Sprintf("%s[%d]", path, i), original.Index(i), copy.Index(i))
		}
	case reflect.Map:
		if !original.IsNil() && original.Pointer() == copy.Pointer() {
			t.Errorf("%s is shared", path)
		}
	case reflect.Struct:
		if original.Type() == reflect.TypeOf(time.Time{}) {
			return
		}
		for i := range original.NumField() {
			field := original.Type().Field(i)
			if field.IsExported() {
				assertByJwtTestNothingShared(t, path+"."+field.Name, original.Field(i), copy.Field(i))
			}
		}
	}
}

// A copy shares nothing with what it copies, for every field of ByJwt,
// including any added later: the reflection walk reaches a new field without
// this test changing.
func TestByJwtCloneSharesNothing(t *testing.T) {
	original := &ByJwt{}
	fillByJwtTestValue(t, reflect.ValueOf(original).Elem(), 1)
	copy := original.clone()
	if !reflect.DeepEqual(original, copy) {
		t.Fatalf("copy %+v differs from %+v", *copy, *original)
	}
	assertByJwtTestNothingShared(t, "ByJwt", reflect.ValueOf(original).Elem(), reflect.ValueOf(copy).Elem())
}

// Every answer is the caller's: changing every field of one answer, pointers
// and slices included, changes neither what the cache holds nor the next
// answer.
func TestByJwtVerifiedCacheAnswersAreCopies(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	key := writeByJwtTestKey(t, vault, "1.0.0", clock.time)
	cachedKeyring := newByJwtTestCachedKeyring(vault, clock)
	claims := &ByJwt{}
	fillByJwtTestValue(t, reflect.ValueOf(claims).Elem(), 1)
	claims.ExpiresAt = gojwt.NewNumericDate(clock.time.Add(time.Hour))
	signed := signByJwtWithKey(t, claims, key, true)

	first := parseByJwtTestSigned(cachedKeyring, signed)
	if first.err != nil {
		t.Fatal(first.err)
	}
	want := first.byJwt.clone()
	fillByJwtTestValue(t, reflect.ValueOf(first.byJwt).Elem(), 50)
	for attempt := range 2 {
		answer := parseByJwtTestSigned(cachedKeyring, signed)
		if answer.err != nil || !reflect.DeepEqual(answer.byJwt, want) {
			t.Fatalf("answer %d = %s, want %+v", attempt, answer, *want)
		}
		// change every pointed-to value of this answer in place
		if answer.byJwt.ClientId != nil {
			*answer.byJwt.ClientId = server.NewId()
		}
		answer.byJwt.Roles[0] = "changed"
		answer.byJwt.Audience[0] = "changed"
		answer.byJwt.ExpiresAt.Time = time.Time{}
		*answer.byJwt.ClientLimitUpgrade = !*answer.byJwt.ClientLimitUpgrade
	}
	assertByJwtTestCounts(t, "copies", cachedKeyring, byJwtTestCacheCounts{hitCount: 2, missCount: 1, storeCount: 1, entryCount: 1})
}

// The cache holds at most its entry bound, dropping the least recently used.
func TestByJwtVerifiedCacheIsBounded(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	key := writeByJwtTestKey(t, vault, "1.0.0", clock.time)
	keyring := newByJwtTestKeyring(vault, clock)
	keyring.settings.verifiedCacheMaxEntryCount = 3
	keyring.settings.verifiedCacheMaxEntryAge = time.Hour
	keyring.settings.verifiedCacheShardCount = 1
	keyring.verifiedCache = newByJwtVerifiedCache(keyring.settings)
	signedTokens := []string{}
	for range 5 {
		signedTokens = append(signedTokens, signByJwtWithKey(t, newByJwtTestClaims(), key, true))
	}

	for _, signed := range signedTokens[:3] {
		parseByJwtTestSigned(keyring, signed)
	}
	// the first is used again, so the second is the least recently used
	parseByJwtTestSigned(keyring, signedTokens[0])
	parseByJwtTestSigned(keyring, signedTokens[3])
	assertByJwtTestCounts(t, "past the bound", keyring, byJwtTestCacheCounts{hitCount: 1, missCount: 4, storeCount: 4, evictCount: 1, entryCount: 3})
	for _, i := range []int{0, 2, 3} {
		if keyring.verifiedClaims(signedTokens[i]) == nil {
			t.Fatalf("token %d was dropped", i)
		}
	}
	if keyring.verifiedClaims(signedTokens[1]) != nil {
		t.Fatal("the least recently used token was kept")
	}

	// a keyring that remembers nothing verifies every time
	keyring.verifiedCache = nil
	for range 2 {
		if verdict := parseByJwtTestSigned(keyring, signedTokens[4]); verdict.err != nil {
			t.Fatal(verdict.err)
		}
	}
	keyring.settings.verifiedCacheMaxEntryCount = 0
	if newByJwtVerifiedCache(keyring.settings) != nil {
		t.Fatal("a cache bounded to no entries was built")
	}
}

// The unverified claim readers of the request gates answer the same for a
// token the keyring remembers and one it does not, and read a remembered
// token's claims without decoding it. Before the keyring reads its keys they
// decode, and read no keys.
func TestByJwtUnverifiedReadersReadVerifiedClaims(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	key := writeByJwtTestKey(t, vault, "1.0.0", clock.time)
	keyring := newByJwtTestCachedKeyring(vault, clock)
	if keyring.verifiedClaims("anything") != nil || vault.readCount != 0 {
		t.Fatal("a reader read the keys before the keyring did")
	}

	guest := newByJwtTestClaims()
	guest.LagGuest = true
	client := newByJwtTestClaims().Client(server.NewId(), server.NewId())
	network := newByJwtTestClaims()
	for _, claims := range []*ByJwt{guest, client, network} {
		signed := signByJwtWithKey(t, claims, key, true)
		decoded := []any{byJwtLagGuestUnverified(keyring, signed), byJwtNamesClientUnverified(keyring, signed)}
		if verdict := parseByJwtTestSigned(keyring, signed); verdict.err != nil {
			t.Fatal(verdict.err)
		}
		if keyring.verifiedClaims(signed) == nil {
			t.Fatal("the verified token is not remembered")
		}
		remembered := []any{byJwtLagGuestUnverified(keyring, signed), byJwtNamesClientUnverified(keyring, signed)}
		if !reflect.DeepEqual(decoded, remembered) {
			t.Fatalf("decoded %v, remembered %v", decoded, remembered)
		}
		if remembered[0] != claims.LagGuest || remembered[1] != (claims.ClientId != nil) {
			t.Fatalf("readers answered %v for %+v", remembered, *claims)
		}
	}

	// claims remembered for bytes that do not decode: only the remembered
	// claims can answer
	planted := newByJwtTestClaims()
	planted.LagGuest = true
	planted.ClientId = &planted.NetworkId
	planted.ExpiresAt = gojwt.NewNumericDate(clock.time.Add(time.Hour))
	keyring.verifiedCache.store("planted", planted, keyring.heldKeys().removalGeneration)
	if !byJwtLagGuestUnverified(keyring, "planted") || !byJwtNamesClientUnverified(keyring, "planted") {
		t.Fatal("the readers decoded instead of reading the remembered claims")
	}
	if byJwtLagGuestUnverified(newByJwtTestKeyring(vault, clock), "planted") {
		t.Fatal("bytes that do not decode were read as a guest's")
	}
}

// Concurrent parses of remembered, new and tampered tokens, and concurrent
// readers, share one keyring: every genuine token is accepted with its own
// claims and every tampered one refused. Run with -race.
func TestByJwtVerifiedCacheConcurrentParses(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	key := writeByJwtTestKey(t, vault, "1.0.0", clock.time)
	keyring := newByJwtTestCachedKeyring(vault, clock)
	keyring.heldKeys()
	type testToken struct {
		signed    string
		networkId server.Id
		tampered  bool
	}
	tokens := []testToken{}
	for range 8 {
		claims := newByJwtTestClaims()
		signed := signByJwtWithKey(t, claims, key, true)
		tokens = append(tokens, testToken{signed: signed, networkId: claims.NetworkId})
		tampered := signed[:len(signed)-4] + "AAAA"
		if tampered == signed {
			tampered = signed[:len(signed)-4] + "BBBB"
		}
		tokens = append(tokens, testToken{signed: tampered, tampered: true})
	}

	const goroutineCount = 16
	const parseCount = 64
	errs := make(chan error, goroutineCount*parseCount)
	var waitGroup sync.WaitGroup
	for i := range goroutineCount {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for j := range parseCount {
				token := tokens[(i*7+j)%len(tokens)]
				verdict := parseByJwtTestSigned(keyring, token.signed)
				byJwtLagGuestUnverified(keyring, token.signed)
				switch {
				case token.tampered && verdict.err == nil:
					errs <- errors.New("a tampered token was accepted")
				case !token.tampered && verdict.err != nil:
					errs <- verdict.err
				case !token.tampered && verdict.byJwt.NetworkId != token.networkId:
					errs <- fmt.Errorf("token for %s answered %s", token.networkId, verdict.byJwt.NetworkId)
				case !token.tampered:
					// the answer is the caller's to change
					verdict.byJwt.NetworkId = server.NewId()
				}
			}
		}()
	}
	waitGroup.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if counts := byJwtTestCounts(keyring); counts.entryCount != 8 || counts.hitCount == 0 {
		t.Fatalf("counts = %+v, want the 8 genuine tokens remembered and reused", counts)
	}
}

// A token parse as the api makes it (ParseByJwtForAudience: decode, signature,
// claims) when every parse verifies in full, against one whose verification
// is remembered.
func BenchmarkParseByJwtForAudienceVerified(b *testing.B) {
	benchmarkParseByJwtForAudience(b, false)
}

func BenchmarkParseByJwtForAudienceRemembered(b *testing.B) {
	benchmarkParseByJwtForAudience(b, true)
}

func benchmarkParseByJwtForAudience(b *testing.B, remembered bool) {
	vault := &byJwtTestVault{root: b.TempDir()}
	clock := newByJwtTestClock()
	clock.time = time.Now()
	key := writeByJwtRotationKey(b, vault.root, "1.0.0")
	keyring := newByJwtKeyring(&byJwtKeyringSettings{
		keyPaths: func() ([]string, error) {
			return []string{byJwtRotationKeyPath}, nil
		},
		resourcePaths: func(keyPath string) ([]string, error) {
			return server.ResolveResourcePaths(vault.root, byJwtRotationEnv, keyPath)
		},
		now:            clock.now,
		rereadInterval: byJwtTestRereadInterval,
		signingDelay:   byJwtTestSigningDelay,
	})
	if remembered {
		defaults := defaultByJwtKeyringSettings()
		keyring.settings.verifiedCacheMaxEntryCount = defaults.verifiedCacheMaxEntryCount
		keyring.settings.verifiedCacheMaxEntryAge = defaults.verifiedCacheMaxEntryAge
		keyring.settings.verifiedCacheShardCount = defaults.verifiedCacheShardCount
		keyring.verifiedCache = newByJwtVerifiedCache(keyring.settings)
	}
	claims := NewByJwt(server.NewId(), server.NewId(), "test", false, false).Client(server.NewId(), server.NewId())
	signed := signByJwtWithKey(b, claims, key, true)
	ctx := context.Background()
	if _, err := parseByJwtForAudience(ctx, keyring, signed, ByJwtAudienceApi); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := parseByJwtForAudience(ctx, keyring, signed, ByJwtAudienceApi); err != nil {
			b.Fatal(err)
		}
	}
}

// The lag guest gate's claim read for a token the keyring does not remember
// (a decode) and for one it does.
func BenchmarkByJwtLagGuestUnverifiedDecoded(b *testing.B) {
	benchmarkByJwtLagGuestUnverified(b, false)
}

func BenchmarkByJwtLagGuestUnverifiedRemembered(b *testing.B) {
	benchmarkByJwtLagGuestUnverified(b, true)
}

func benchmarkByJwtLagGuestUnverified(b *testing.B, remembered bool) {
	vault := &byJwtTestVault{root: b.TempDir()}
	clock := newByJwtTestClock()
	clock.time = time.Now()
	key := writeByJwtRotationKey(b, vault.root, "1.0.0")
	keyring := newByJwtKeyring(&byJwtKeyringSettings{
		keyPaths: func() ([]string, error) {
			return []string{byJwtRotationKeyPath}, nil
		},
		resourcePaths: func(keyPath string) ([]string, error) {
			return server.ResolveResourcePaths(vault.root, byJwtRotationEnv, keyPath)
		},
		now:                        clock.now,
		rereadInterval:             byJwtTestRereadInterval,
		signingDelay:               byJwtTestSigningDelay,
		verifiedCacheMaxEntryCount: 1024,
		verifiedCacheMaxEntryAge:   time.Hour,
		verifiedCacheShardCount:    16,
	})
	signed := signByJwtWithKey(b, NewByJwt(server.NewId(), server.NewId(), "test", false, false), key, true)
	keyring.heldKeys()
	if remembered {
		if _, err := parseByJwtSigned(keyring, signed); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		byJwtLagGuestUnverified(keyring, signed)
	}
}

// The collector reports the process keyring's counts under the names the
// signals dashboard reads, and passes a pedantic registry.
func TestByJwtVerifiedCacheCollectorReportsTheProcessKeyring(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	if err := registry.Register(newByJwtVerifiedCacheCollector()); err != nil {
		t.Fatal(err)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seriesCounts := map[string]int{}
	for _, family := range families {
		seriesCounts[family.GetName()] = len(family.GetMetric())
	}
	want := map[string]int{
		"urnetwork_auth_jwt_verified_cache_lookups_total":   2,
		"urnetwork_auth_jwt_verified_cache_stores_total":    1,
		"urnetwork_auth_jwt_verified_cache_evictions_total": 1,
		"urnetwork_auth_jwt_verified_cache_entries":         1,
	}
	if !reflect.DeepEqual(seriesCounts, want) {
		t.Fatalf("series = %v, want %v", seriesCounts, want)
	}
}
